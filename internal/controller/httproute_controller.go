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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"k8s.io/client-go/tools/events"
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
	// Recorder emits Kubernetes Events on the HTTPRoute. Nil disables them.
	Recorder  events.EventRecorder
	DomainMap *config.DomainMap
	DNS       dns.Provider
	Upsert    bool // when true, update existing records; when false, only create missing ones
	// RequireAccepted limits DNS to routes that at least one parent Gateway
	// has accepted. Off, any route with a matching hostname gets DNS.
	RequireAccepted bool
	// CleanupGracePeriod is how long after a route's deletion was requested a
	// failing DNS cleanup is retried before the finalizer is removed anyway.
	CleanupGracePeriod time.Duration
}

// cleanupRetryInterval spaces out retries of a failing DNS cleanup.
const cleanupRetryInterval = 10 * time.Second

// Reconcile implements controller-runtime's Reconciler interface.
func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	// Reads come from the informer cache. Writes that could race another
	// writer re-read through APIReader inside their conflict retry.
	var route gatewayv1.HTTPRoute
	if err := r.Get(ctx, req.NamespacedName, &route); err != nil {
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

	if r.RequireAccepted && !accepted(&route) && len(specHostnames) > 0 {
		// An unaccepted route wants no DNS. Records it owns from while it was
		// accepted are released below like any other hostname that left the
		// spec.
		r.Log.V(1).Info("route is not accepted by any parent; not publishing DNS for it", "name", req.NamespacedName)
		specHostnames = nil
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
			dnsErrors.WithLabelValues("publish").Inc()
			r.Log.Error(cerr, "failed to publish staged DNS changes")
			// Surfacing the failure is what makes controller-runtime requeue with
			// backoff; logging it alone would leave the DNS change unpublished
			// with no retry. An existing error already guarantees a requeue.
			if err == nil {
				err = cerr
			}
		}
		if err != nil {
			r.event(&route, corev1.EventTypeWarning, "DNSSyncFailed", "%v", err)
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
		// The finalizer is persisted before any record exists, so a deletion
		// can never miss one. Carry on in the same reconcile rather than
		// waiting for the finalizer update to come back as an event.
	}

	// Release records we own that the spec or the domain map no longer wants.
	for _, oldHost := range owned {
		if !slices.Contains(specHostnames, oldHost) {
			if err := r.release(ctx, batch, &route, oldHost, owned); err != nil {
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
			dnsErrors.WithLabelValues("ensure").Inc()
			return ctrl.Result{}, fmt.Errorf("ensuring DNS record for %s: %w", hostname, err)
		}
		recordResults.WithLabelValues(res.String()).Inc()
		switch res {
		case dns.Created:
			r.Log.Info("created DNS record", "hostname", hostname, "ip", ip)
			r.event(&route, corev1.EventTypeNormal, "DNSRecordCreated", "%s -> %s", hostname, ip)
		case dns.Updated:
			r.Log.Info("updated DNS record", "hostname", hostname, "ip", ip)
			r.event(&route, corev1.EventTypeNormal, "DNSRecordUpdated", "%s -> %s", hostname, ip)
		case dns.Kept:
			r.Log.Info("DNS record already exists with a different value and upsert mode is disabled — IPs may drift if domain map changes; consider enabling upsert mode to keep records in sync", "hostname", hostname, "ip", ip)
			r.event(&route, corev1.EventTypeWarning, "DNSRecordDrift", "%s holds a different value than %s; upsert mode is off, so it is left alone", hostname, ip)
		case dns.Unchanged:
			r.Log.V(1).Info("DNS record already up to date", "hostname", hostname, "ip", ip)
		case dns.Conflict:
			r.Log.Info("DNS record exists but is not managed by this controller; leaving it alone", "hostname", hostname, "ip", ip)
			r.event(&route, corev1.EventTypeWarning, "DNSConflict", "%s already has a DNS record this controller does not manage; it is left alone", hostname)
			continue
		}
		managed = append(managed, hostname)
	}

	// Record the hostnames this route actually manages. A name that hit a
	// conflict is left out, so the annotation can never be used to adopt a
	// legacy record that belongs to another route.
	// A merge patch carries only this one annotation, so it cannot conflict
	// with or clobber anyone else's change to the route.
	if !slices.Equal(owned, managed) {
		data, _ := json.Marshal(managed)
		patch := client.RawPatch(types.MergePatchType,
			fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`, managedHostnamesAnnotation, data))
		if err := r.Patch(ctx, &route, patch); err != nil {
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
		if err := r.release(ctx, batch, route, hostname, owned); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("%s: %w", hostname, err))
		}
	}
	if err := batch.Commit(ctx); err != nil {
		dnsErrors.WithLabelValues("publish").Inc()
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("publishing: %w", err))
	}

	if cleanupErr != nil {
		deadline := route.DeletionTimestamp.Add(r.CleanupGracePeriod)
		if remaining := time.Until(deadline); remaining > 0 {
			r.Log.Error(cleanupErr, "DNS cleanup failed; retrying before releasing the route", "giveUpAt", deadline)
			r.event(route, corev1.EventTypeWarning, "DNSCleanupFailed", "retrying until %s: %v", deadline.Format(time.RFC3339), cleanupErr)
			return ctrl.Result{RequeueAfter: min(remaining, cleanupRetryInterval)}, nil
		}
		r.Log.Error(cleanupErr, "DNS cleanup still failing after the grace period; releasing the route anyway, so DNS records may be left behind")
		r.event(route, corev1.EventTypeWarning, "DNSCleanupAbandoned", "releasing the route after the grace period; DNS records may be left behind: %v", cleanupErr)
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

// accepted reports whether at least one parent has accepted the route.
func accepted(route *gatewayv1.HTTPRoute) bool {
	for _, parent := range route.Status.Parents {
		if meta.IsStatusConditionTrue(parent.Conditions, string(gatewayv1.RouteConditionAccepted)) {
			return true
		}
	}
	return false
}

// release removes the managed DNS record for hostname, unless another live
// route still claims the name.
//
// Records the controller does not manage are never removed, whatever the
// annotation says: the provider only deletes rows carrying its ownership
// marker. A record written before ownership was marked is removed only for a
// name the route recorded owning and the domain map covers.
func (r *HTTPRouteReconciler) release(ctx context.Context, batch dns.Batch, route *gatewayv1.HTTPRoute, hostname string, owned []string) error {
	var routes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routes, client.MatchingFields{hostnameIndex: hostname}); err != nil {
		return fmt.Errorf("listing routes that claim %s: %w", hostname, err)
	}
	for _, other := range routes.Items {
		if (other.Namespace == route.Namespace && other.Name == route.Name) || !other.DeletionTimestamp.IsZero() {
			continue
		}
		r.Log.Info("not deleting DNS record still claimed by another route", "hostname", hostname,
			"route", other.Namespace+"/"+other.Name)
		return nil
	}

	_, mapped := r.DomainMap.LookupIP(hostname)
	if err := batch.Remove(ctx, hostname, "A", mapped && slices.Contains(owned, hostname)); err != nil {
		dnsErrors.WithLabelValues("release").Inc()
		return err
	}
	recordResults.WithLabelValues("released").Inc()
	r.Log.Info("released DNS record", "hostname", hostname)
	r.event(route, corev1.EventTypeNormal, "DNSRecordReleased", "%s", hostname)
	return nil
}

// event records a Kubernetes Event on the route, when a recorder is set.
func (r *HTTPRouteReconciler) event(route *gatewayv1.HTTPRoute, eventtype, reason, note string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(route, nil, eventtype, reason, "ReconcileDNS", note, args...)
	}
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
		For(&gatewayv1.HTTPRoute{}, builder.WithPredicates(predicate.Funcs{UpdateFunc: r.wantsUpdate})).
		Complete(r)
}

// wantsUpdate filters HTTPRoute updates down to the ones that can change
// what the controller publishes. Status-only and metadata-only updates,
// including the controller's own annotation patch, are dropped.
func (r *HTTPRouteReconciler) wantsUpdate(e event.UpdateEvent) bool {
	oldRoute, okOld := e.ObjectOld.(*gatewayv1.HTTPRoute)
	newRoute, okNew := e.ObjectNew.(*gatewayv1.HTTPRoute)
	if !okOld || !okNew {
		return false
	}
	switch {
	case oldRoute.Generation != newRoute.Generation:
		// The spec changed, or deletion was requested.
		return true
	case controllerutil.ContainsFinalizer(oldRoute, finalizerName) != controllerutil.ContainsFinalizer(newRoute, finalizerName):
		// Our finalizer came or went, for instance removed by hand.
		return true
	case r.RequireAccepted && accepted(oldRoute) != accepted(newRoute):
		// Acceptance lives in status, which does not bump the generation.
		return true
	default:
		return false
	}
}
