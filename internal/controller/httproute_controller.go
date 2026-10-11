// Package controller provides the HTTPRoute reconciler for managing DNS records.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
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
	// hostnameIndex indexes routes by the hostnames in their spec, so a
	// record shared by several routes is only removed once none claims it.
	hostnameIndex = "spec.hostnames"
)

// HTTPRouteReconciler reconciles HTTPRoute objects.
type HTTPRouteReconciler struct {
	client.Client
	APIReader client.Reader
	Log       logr.Logger
	DomainMap *config.DomainMap
	DNS       dns.Provider
	Upsert    bool // when true, update existing records; when false, only create missing ones
	// CleanupGracePeriod is how long after a route's deletion was requested a
	// failing DNS cleanup is retried before the finalizer is removed anyway.
	CleanupGracePeriod time.Duration
}

// cleanupRetryInterval spaces out retries of a failing DNS cleanup.
const cleanupRetryInterval = 10 * time.Second

// Reconcile implements controller-runtime's Reconciler interface.
func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
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
		if !controllerutil.ContainsFinalizer(&route, finalizerName) {
			return ctrl.Result{}, nil
		}
		return r.finalize(ctx, &route, owned, specHostnames)
	}

	// One batch per reconcile: the provider reads its records once, and the
	// changes are published in a single commit at the end, so a route with N
	// hostnames costs one read and at most one publish. Committing on the
	// error path as well keeps staged work from being stranded.
	batch := r.DNS.Batch()
	defer func() {
		if cerr := batch.Commit(ctx); cerr != nil {
			r.Log.Error(cerr, "failed to publish staged DNS changes")
			// Surfacing the failure is what makes controller-runtime requeue with
			// backoff; logging it alone would leave the DNS change unpublished
			// with no retry. An existing error already guarantees a requeue.
			if err == nil {
				err = cerr
			}
		}
	}()

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

	// Release records we own that the spec or the domain map no longer wants.
	for _, oldHost := range owned {
		if !slices.Contains(specHostnames, oldHost) {
			if err := r.release(ctx, batch, req.NamespacedName, oldHost, owned); err != nil {
				return ctrl.Result{}, fmt.Errorf("deleting removed DNS record for %s: %w", oldHost, err)
			}
		}
	}

	// Create or update. In create-only mode (Upsert false) a managed record
	// that already exists is left alone, whatever it holds. A record the
	// controller does not manage is never touched.
	managed := make([]string, 0, len(specHostnames))
	for _, hostname := range specHostnames {
		ip, _ := r.DomainMap.LookupIP(hostname)

		r.Log.V(1).Info("resolved hostname to IP", "hostname", hostname, "ip", ip)
		record := dns.Record{
			Hostname: hostname,
			Type:     "A",
			Value:    ip,
		}

		// A record written before ownership was marked is adopted only for a
		// name this route already recorded owning; the domain map covers every
		// name here.
		res, err := batch.Ensure(ctx, record, dns.EnsureOptions{
			Overwrite:   r.Upsert,
			AdoptLegacy: slices.Contains(owned, hostname),
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensuring DNS record for %s: %w", hostname, err)
		}
		switch res {
		case dns.Created, dns.Updated:
			r.Log.Info(res.String()+" DNS record", "hostname", hostname, "ip", ip)
		case dns.Kept:
			r.Log.Info("DNS record already exists with a different value and upsert mode is disabled — IPs may drift if domain map changes; consider enabling upsert mode to keep records in sync", "hostname", hostname, "ip", ip)
		case dns.Unchanged:
			r.Log.V(1).Info("DNS record already up to date", "hostname", hostname, "ip", ip)
		case dns.Conflict:
			r.Log.Info("DNS record exists but is not managed by this controller; leaving it alone", "hostname", hostname, "ip", ip)
			continue
		}
		managed = append(managed, hostname)
	}

	// Record the hostnames this route actually manages. A name that hit a
	// conflict is left out, so the annotation can never be used to adopt a
	// legacy record that belongs to another route.
	if !slices.Equal(owned, managed) {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.APIReader.Get(ctx, req.NamespacedName, &route); err != nil {
				return err
			}
			if route.Annotations == nil {
				route.Annotations = make(map[string]string)
			}
			data, _ := json.Marshal(managed)
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

// finalize releases a deleted route's DNS records and publishes the change
// before removing the finalizer, so the records are gone from DNS by the time
// the route is. A failing cleanup is retried until CleanupGracePeriod after
// the deletion was requested; after that the finalizer is removed anyway,
// because a route must always be deletable, and the records may be left
// behind.
func (r *HTTPRouteReconciler) finalize(ctx context.Context, route *gatewayv1.HTTPRoute, owned, specHostnames []string) (ctrl.Result, error) {
	key := client.ObjectKeyFromObject(route)
	r.Log.Info("deleting DNS records for HTTPRoute", "name", key)

	// Release everything the annotation claims we own, plus everything the
	// live spec still maps. The union covers routes written before the
	// annotation existed.
	batch := r.DNS.Batch()
	var cleanupErr error
	for _, hostname := range union(owned, specHostnames) {
		if err := r.release(ctx, batch, key, hostname, owned); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("%s: %w", hostname, err))
		}
	}
	if err := batch.Commit(ctx); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("publishing: %w", err))
	}

	if cleanupErr != nil {
		deadline := route.DeletionTimestamp.Add(r.CleanupGracePeriod)
		if remaining := time.Until(deadline); remaining > 0 {
			r.Log.Error(cleanupErr, "DNS cleanup failed; retrying before releasing the route", "giveUpAt", deadline)
			return ctrl.Result{RequeueAfter: min(remaining, cleanupRetryInterval)}, nil
		}
		r.Log.Error(cleanupErr, "DNS cleanup still failing after the grace period; releasing the route anyway, so DNS records may be left behind")
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.APIReader.Get(ctx, key, route); err != nil {
			return err
		}
		controllerutil.RemoveFinalizer(route, finalizerName)
		return r.Update(ctx, route)
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// release removes the managed DNS record for hostname, unless another live
// route still claims the name.
//
// Records the controller does not manage are never removed, whatever the
// annotation says: the provider only deletes rows carrying its ownership
// marker. A record written before ownership was marked is removed only for a
// name the route recorded owning and the domain map covers.
func (r *HTTPRouteReconciler) release(ctx context.Context, batch dns.Batch, self types.NamespacedName, hostname string, owned []string) error {
	var routes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routes, client.MatchingFields{hostnameIndex: hostname}); err != nil {
		return fmt.Errorf("listing routes that claim %s: %w", hostname, err)
	}
	for _, other := range routes.Items {
		if (other.Namespace == self.Namespace && other.Name == self.Name) || !other.DeletionTimestamp.IsZero() {
			continue
		}
		r.Log.Info("not deleting DNS record still claimed by another route", "hostname", hostname,
			"route", other.Namespace+"/"+other.Name)
		return nil
	}

	_, mapped := r.DomainMap.LookupIP(hostname)
	if err := batch.Remove(ctx, hostname, "A", mapped && slices.Contains(owned, hostname)); err != nil {
		return err
	}
	r.Log.Info("released DNS record", "hostname", hostname)
	return nil
}

// indexHostnames is the hostnameIndex extractor.
func indexHostnames(obj client.Object) []string {
	route, ok := obj.(*gatewayv1.HTTPRoute)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(route.Spec.Hostnames))
	for _, h := range route.Spec.Hostnames {
		out = append(out, strings.TrimSuffix(string(h), "."))
	}
	return out
}

// union returns the elements of a then b, deduplicated, preserving order.
func union(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string(nil), a...), b...) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &gatewayv1.HTTPRoute{}, hostnameIndex, indexHostnames); err != nil {
		return fmt.Errorf("indexing routes by hostname: %w", err)
	}
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
