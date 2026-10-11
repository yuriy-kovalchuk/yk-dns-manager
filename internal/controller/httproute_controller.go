// Package controller provides the HTTPRoute reconciler for managing DNS records.
package controller

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"k8s.io/client-go/util/retry"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/config"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

const (
	finalizerName              = "dns.yk/cleanup"
	managedHostnamesAnnotation = "dns.yk/managed-hostnames"
)

// HTTPRouteReconciler reconciles HTTPRoute objects.
type HTTPRouteReconciler struct {
	client.Client
	APIReader client.Reader
	Log       logr.Logger
	DomainMap *config.DomainMap
	DNS       dns.Provider
	Upsert    bool // when true, update existing records; when false, only create missing ones
}

// Reconcile implements controller-runtime's Reconciler interface.
func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var route gatewayv1.HTTPRoute
	if err := r.APIReader.Get(ctx, req.NamespacedName, &route); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The annotation records the hostnames this controller created DNS records
	// for. It is the only source of truth that survives a hostname being edited
	// out of the spec or dropped from the domain map, so every cleanup path
	// reads it.
	owned := r.readOwned(&route)

	currentHostnames := make([]string, 0, len(route.Spec.Hostnames))
	for _, h := range route.Spec.Hostnames {
		currentHostnames = append(currentHostnames, string(h))
	}

	specHostnames := make([]string, 0, len(currentHostnames))
	for _, h := range currentHostnames {
		if _, ok := r.DomainMap.LookupIP(h); ok {
			specHostnames = append(specHostnames, h)
		} else {
			r.Log.V(1).Info("hostname not in domain map, skipping", "hostname", h)
		}
	}

	// Deletion is handled before any early return: a route carrying our
	// finalizer must always become deletable, whatever its spec or the domain
	// map currently look like.
	if !route.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&route, finalizerName) {
			r.Log.Info("deleting DNS records for HTTPRoute", "name", req.NamespacedName)

			// Delete everything the annotation claims we own, plus everything the
			// live spec still maps. The union covers routes written before the
			// annotation existed; without the annotation half, records whose
			// hostname no longer resolves through the map would be orphaned.
			var deleteErr error
			for _, hostname := range union(owned, specHostnames) {
				if err := r.DNS.Delete(ctx, hostname, "A"); err != nil {
					r.Log.Error(err, "failed to delete DNS record", "hostname", hostname)
					deleteErr = fmt.Errorf("at least one DNS record deletion failed")
				} else {
					r.Log.Info("deleted DNS record", "hostname", hostname)
				}
			}

			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := r.APIReader.Get(ctx, req.NamespacedName, &route); err != nil {
					return err
				}
				controllerutil.RemoveFinalizer(&route, finalizerName)
				return r.Update(ctx, &route)
			})
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
			}
			// Best-effort cleanup: log errors but always succeed so the route is removed.
			if deleteErr != nil {
				r.Log.Error(deleteErr, "one or more DNS record deletions failed during cleanup")
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&route, finalizerName) {
		// Don't take ownership of routes we have nothing to manage.
		if len(specHostnames) == 0 {
			return ctrl.Result{}, nil
		}
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.APIReader.Get(ctx, req.NamespacedName, &route); err != nil {
				return err
			}
			controllerutil.AddFinalizer(&route, finalizerName)
			return r.Update(ctx, &route)
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Delete records we own that are no longer wanted. The domain map is the
	// declared desired state, so a hostname that left the map is as unwanted as
	// one that left the spec.
	for _, oldHost := range owned {
		if !Contains(specHostnames, oldHost) {
			r.Log.Info("hostname no longer managed, deleting DNS record", "hostname", oldHost)
			if err := r.DNS.Delete(ctx, oldHost, "A"); err != nil {
				return ctrl.Result{}, fmt.Errorf("deleting removed DNS record for %s: %w", oldHost, err)
			}
		}
	}

	// Update and Create
	for _, hostname := range specHostnames {
		ip, _ := r.DomainMap.LookupIP(hostname)

		r.Log.V(1).Info("resolved hostname to IP", "hostname", hostname, "ip", ip)
		record := dns.Record{
			Hostname: hostname,
			Type:     "A",
			Value:    ip,
			Meta:     map[string]string{"description": "managed by yk-dns-manager"},
		}

		if r.Upsert {
			if err := r.DNS.Upsert(ctx, record); err != nil {
				return ctrl.Result{}, fmt.Errorf("upserting DNS record for %s: %w", hostname, err)
			}
			r.Log.Info("upserted DNS record", "hostname", hostname, "ip", ip)
			continue
		}

		// Non-upsert path: only create if missing.
		exists, err := r.DNS.Exists(ctx, hostname, "A")
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("checking DNS record for %s: %w", hostname, err)
		}
		if exists {
			r.Log.Info("DNS record already exists and upsert mode is disabled — IPs may drift if domain map changes; consider enabling upsert mode to keep records in sync", "hostname", hostname)
			continue
		}
		if err := r.DNS.Create(ctx, record); err != nil {
			return ctrl.Result{}, fmt.Errorf("creating DNS record for %s: %w", hostname, err)
		}
		r.Log.Info("created DNS record", "hostname", hostname, "ip", ip)
	}

	// Update annotation with the list of hostnames we actually manage (those
	// matching the domain map). Both sides are the same shape here, so the
	// comparison cannot loop on a spurious write.
	if !sameStrings(owned, specHostnames) {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.APIReader.Get(ctx, req.NamespacedName, &route); err != nil {
				return err
			}
			if route.Annotations == nil {
				route.Annotations = make(map[string]string)
			}
			data, _ := json.Marshal(specHostnames)
			route.Annotations[managedHostnamesAnnotation] = string(data)
			return r.Update(ctx, &route)
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update managed-hostnames annotation: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

// readOwned returns the hostnames recorded as owned by this controller. A
// corrupt annotation is logged and treated as empty rather than silently
// ignored, because treating it as empty is the only safe reading: the records
// it names may already be gone.
func (r *HTTPRouteReconciler) readOwned(route *gatewayv1.HTTPRoute) []string {
	val, ok := route.Annotations[managedHostnamesAnnotation]
	if !ok || val == "" {
		return nil
	}
	var owned []string
	if err := json.Unmarshal([]byte(val), &owned); err != nil {
		r.Log.Error(err, "failed to parse managed-hostnames annotation, treating route as owning nothing",
			"annotation", val)
		return nil
	}
	return owned
}

// union returns the elements of a then b, deduplicated, preserving order.
func union(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string(nil), a...), b...) {
		if !Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// sameStrings compares two string slices without treating nil and empty as
// different, which reflect.DeepEqual does and which would force a write every
// reconcile for a route that owns nothing.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
