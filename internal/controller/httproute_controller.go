// Package controller provides the HTTPRoute reconciler for managing DNS records.
package controller

import (
	"context"
	"fmt"
	"slices"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/config"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

const (
	finalizerName              = "dns.yk/cleanup"
	managedHostnamesAnnotation = "dns.yk/managed-hostnames"
)

// HTTPRouteReconciler reconciles HTTPRoute objects into DNS records.
// It only orchestrates:
//
//   - State owns every mutation of the HTTPRoute (finalizer + annotation).
//   - DNS fans record operations out to every provider instance.
type HTTPRouteReconciler struct {
	State     *RouteState
	DomainMap *config.DomainMap
	DNS       *dns.Manager
	Log       logr.Logger
}

// Reconcile implements controller-runtime's Reconciler interface. Deletion
// is handled first and independently of the domain map, so a route whose
// hostnames no longer map is always released by its finalizer.
func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	route, err := r.State.Get(ctx, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !route.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, route)
	}
	return r.reconcileLive(ctx, route)
}

// reconcileDeletion removes every DNS record the route may own — the
// annotated hostnames plus the currently mapped spec hostnames — and
// releases the finalizer. DNS deletion is best-effort: failures are logged
// but never block finalizer removal, so the route is always deletable.
func (r *HTTPRouteReconciler) reconcileDeletion(ctx context.Context, route *gatewayv1.HTTPRoute) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(route, finalizerName) {
		return ctrl.Result{}, nil
	}

	hostnames := r.State.ManagedHostnames(route)
	for _, h := range r.mappedSpecHostnames(route) {
		if !slices.Contains(hostnames, h) {
			hostnames = append(hostnames, h)
		}
	}

	r.Log.Info("deleting DNS records for HTTPRoute", "name", route.Name, "hostnames", hostnames)
	var failed int
	for _, hostname := range hostnames {
		if err := r.DNS.DeleteRecord(ctx, hostname, "A"); err != nil {
			r.Log.Error(err, "failed to delete DNS record", "hostname", hostname)
			failed++
		}
	}

	if err := r.State.RemoveFinalizer(ctx, route); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}
	r.Log.Info("DNS cleanup complete", "name", route.Name, "deleted", len(hostnames)-failed, "failed", failed)
	return ctrl.Result{}, nil
}

// reconcileLive ensures a record for every mapped spec hostname and deletes
// records for hostnames that were managed but no longer map. The
// managed-hostnames annotation is the source of truth for what we manage.
func (r *HTTPRouteReconciler) reconcileLive(ctx context.Context, route *gatewayv1.HTTPRoute) (ctrl.Result, error) {
	managed := r.State.ManagedHostnames(route)
	specHostnames := r.mappedSpecHostnames(route)

	// Not a route we manage: nothing mapped in the spec and no records we created.
	if len(specHostnames) == 0 && len(managed) == 0 {
		return ctrl.Result{}, nil
	}

	// Take ownership before creating any records, so a failure mid-run
	// still leaves the finalizer in place for cleanup.
	if !controllerutil.ContainsFinalizer(route, finalizerName) {
		if err := r.State.EnsureFinalizer(ctx, route); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		// The finalizer-change event triggers the next reconcile.
		return ctrl.Result{}, nil
	}

	// Delete records for hostnames we managed that left the spec or the
	// domain map.
	for _, oldHost := range managed {
		if !slices.Contains(specHostnames, oldHost) {
			r.Log.Info("hostname no longer managed, deleting DNS record", "hostname", oldHost)
			if err := r.DNS.DeleteRecord(ctx, oldHost, "A"); err != nil {
				return ctrl.Result{}, fmt.Errorf("deleting DNS record for %s: %w", oldHost, err)
			}
		}
	}

	// Ensure records for all current spec hostnames. The per-instance
	// upsert policy is applied inside the Manager.
	for _, hostname := range specHostnames {
		ip, _ := r.DomainMap.LookupIP(hostname)
		r.Log.V(1).Info("resolved hostname to IP", "hostname", hostname, "ip", ip)
		record := dns.Record{
			Hostname: hostname,
			Type:     "A",
			Value:    ip,
			Meta:     map[string]string{"description": "managed by yk-dns-manager"},
		}
		if err := r.DNS.EnsureRecord(ctx, record); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensuring DNS record for %s: %w", hostname, err)
		}
		r.Log.Info("ensured DNS record", "hostname", hostname, "ip", ip)
	}

	// Keep the annotation in sync with what we manage.
	if err := r.State.SetManagedHostnames(ctx, route, specHostnames); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update managed-hostnames annotation: %w", err)
	}
	return ctrl.Result{}, nil
}

// mappedSpecHostnames returns the spec hostnames present in the domain map,
// deduplicated and in spec order.
func (r *HTTPRouteReconciler) mappedSpecHostnames(route *gatewayv1.HTTPRoute) []string {
	seen := make(map[string]struct{}, len(route.Spec.Hostnames))
	hostnames := make([]string, 0, len(route.Spec.Hostnames))
	for _, h := range route.Spec.Hostnames {
		name := string(h)
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if _, ok := r.DomainMap.LookupIP(name); !ok {
			r.Log.V(1).Info("hostname not in domain map, skipping", "hostname", name)
			continue
		}
		hostnames = append(hostnames, name)
	}
	return hostnames
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{}).
		WithEventFilter(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				// Reconcile if the Spec (Generation) has changed.
				if e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() {
					return true
				}
				// Also reconcile if finalizers have changed (e.g. our finalizer was added).
				if len(e.ObjectOld.GetFinalizers()) != len(e.ObjectNew.GetFinalizers()) {
					return true
				}
				// Ignore status-only updates.
				return false
			},
		}).
		Complete(r)
}
