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
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
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
	// hostnameIndex indexes routes by the hostnames in their spec, so a
	// record shared by several routes is only removed once none claims it.
	hostnameIndex = "spec.hostnames"
	// cleanupRetryInterval spaces out retries of a failing DNS cleanup.
	cleanupRetryInterval = 10 * time.Second
)

// HTTPRouteReconciler reconciles HTTPRoute objects.
type HTTPRouteReconciler struct {
	client.Client
	APIReader client.Reader
	Log       logr.Logger
	Recorder  events.EventRecorder // emits Events on the HTTPRoute; nil disables them
	DomainMap *config.DomainMap
	DNS       dns.Provider
	// Upsert lets the controller change a managed record that holds a
	// different value; off, it only creates missing ones.
	Upsert bool
	// RequireAccepted limits DNS to routes at least one parent Gateway has
	// accepted. Off, any route with a matching hostname gets DNS.
	RequireAccepted bool
	// CleanupGracePeriod is how long after a route's deletion was requested a
	// failing DNS cleanup is retried before the finalizer is removed anyway.
	CleanupGracePeriod time.Duration
}

// outcomes says how each Ensure result is reported. The note is formatted
// with the hostname and the IP. Unchanged is not reported.
var outcomes = map[dns.Result]struct{ eventType, reason, note string }{
	dns.Created:  {corev1.EventTypeNormal, "DNSRecordCreated", "%[1]s -> %[2]s"},
	dns.Updated:  {corev1.EventTypeNormal, "DNSRecordUpdated", "%[1]s -> %[2]s"},
	dns.Kept:     {corev1.EventTypeWarning, "DNSRecordDrift", "%[1]s holds a different value than %[2]s; upsert mode is off, so it is left alone"},
	dns.Conflict: {corev1.EventTypeWarning, "DNSConflict", "%[1]s already has a DNS record this controller does not manage; it is left alone"},
}

// Reconcile implements controller-runtime's Reconciler interface.
func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	var route gatewayv1.HTTPRoute
	if err := r.Get(ctx, req.NamespacedName, &route); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The annotation records the hostnames this route manages. It is the only
	// record that survives a hostname leaving the spec or the domain map, so
	// every cleanup path reads it.
	owned := r.readOwned(&route)
	wanted := r.wanted(&route)

	// Deletion comes first: a route carrying our finalizer must always become
	// deletable, whatever its spec or the domain map look like.
	if !route.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&route, finalizerName) {
			return ctrl.Result{}, nil
		}
		return r.finalize(ctx, &route, owned, wanted)
	}

	// One batch per reconcile: the provider reads its records once and
	// publishes in a single commit at the end. Committing on the error path
	// too keeps staged work from being stranded; a failed commit is returned
	// so controller-runtime retries it.
	batch := r.DNS.Batch()
	defer func() {
		if cerr := batch.Commit(ctx); cerr != nil {
			dnsErrors.WithLabelValues("publish").Inc()
			err = errors.Join(err, fmt.Errorf("publishing DNS changes: %w", cerr))
		}
		if err != nil {
			r.report(&route, corev1.EventTypeWarning, "DNSSyncFailed", "%v", err)
		}
	}()

	if !controllerutil.ContainsFinalizer(&route, finalizerName) {
		if len(wanted) == 0 {
			return ctrl.Result{}, nil // nothing to manage, so take no ownership
		}
		// Persisted before any record exists, so a deletion can never miss
		// one. The reconcile carries on rather than waiting for this update to
		// come back as an event.
		if err := r.setFinalizer(ctx, &route, true); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Release records we own that the spec or the domain map no longer wants.
	for _, hostname := range owned {
		if !slices.Contains(wanted, hostname) {
			if err := r.release(ctx, batch, &route, hostname, owned); err != nil {
				return ctrl.Result{}, fmt.Errorf("deleting removed DNS record for %s: %w", hostname, err)
			}
		}
	}

	// Create or update. A record the controller does not manage is never
	// touched, and a name that hits one is left out of the annotation, so the
	// annotation can never be used to adopt another route's legacy record.
	managed := make([]string, 0, len(wanted))
	for _, hostname := range wanted {
		ip, _ := r.DomainMap.LookupIP(hostname)
		res, err := batch.Ensure(ctx, dns.Record{Hostname: hostname, Type: "A", Value: ip}, dns.EnsureOptions{
			Overwrite: r.Upsert,
			// A record from before ownership was marked is adopted only for a
			// name this route already recorded owning.
			AdoptLegacy: slices.Contains(owned, hostname),
		})
		if err != nil {
			dnsErrors.WithLabelValues("ensure").Inc()
			return ctrl.Result{}, fmt.Errorf("ensuring DNS record for %s: %w", hostname, err)
		}
		recordResults.WithLabelValues(res.String()).Inc()
		if o, ok := outcomes[res]; ok {
			r.report(&route, o.eventType, o.reason, o.note, hostname, ip)
		}
		if res != dns.Conflict {
			managed = append(managed, hostname)
		}
	}

	// A merge patch of this one annotation cannot conflict with, or clobber,
	// anyone else's change to the route.
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

// wanted returns the route's hostnames the controller should publish: those
// the domain map covers, and none while RequireAccepted is set and no parent
// has accepted the route.
func (r *HTTPRouteReconciler) wanted(route *gatewayv1.HTTPRoute) []string {
	if r.RequireAccepted && !accepted(route) {
		r.Log.V(1).Info("route is not accepted by any parent; not publishing DNS for it", "route", client.ObjectKeyFromObject(route))
		return nil
	}
	var out []string
	for _, h := range route.Spec.Hostnames {
		if _, ok := r.DomainMap.LookupIP(string(h)); ok {
			out = append(out, string(h))
		} else {
			r.Log.V(1).Info("hostname not in domain map, skipping", "hostname", h)
		}
	}
	return out
}

// readOwned returns the hostnames recorded as owned by this controller. A
// corrupt annotation is logged and treated as empty, the only safe reading:
// the records it names may already be gone.
func (r *HTTPRouteReconciler) readOwned(route *gatewayv1.HTTPRoute) []string {
	val := route.Annotations[managedHostnamesAnnotation]
	if val == "" {
		return nil
	}
	var owned []string
	if err := json.Unmarshal([]byte(val), &owned); err != nil {
		r.Log.Error(err, "failed to parse managed-hostnames annotation, treating route as owning nothing", "annotation", val)
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
func (r *HTTPRouteReconciler) finalize(ctx context.Context, route *gatewayv1.HTTPRoute, owned, wanted []string) (ctrl.Result, error) {
	r.Log.Info("deleting DNS records for HTTPRoute", "route", client.ObjectKeyFromObject(route))

	// The union with the spec covers routes written before the annotation
	// existed.
	batch := r.DNS.Batch()
	var cleanupErr error
	for _, hostname := range union(owned, wanted) {
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
			r.report(route, corev1.EventTypeWarning, "DNSCleanupFailed", "retrying until %s: %v", deadline.Format(time.RFC3339), cleanupErr)
			return ctrl.Result{RequeueAfter: min(remaining, cleanupRetryInterval)}, nil
		}
		r.report(route, corev1.EventTypeWarning, "DNSCleanupAbandoned", "releasing the route after the grace period; DNS records may be left behind: %v", cleanupErr)
	}
	return ctrl.Result{}, r.setFinalizer(ctx, route, false)
}

// setFinalizer adds or removes our finalizer. It re-reads the route through
// APIReader on each attempt: a merge patch would replace the whole finalizer
// list and could drop another controller's entry.
func (r *HTTPRouteReconciler) setFinalizer(ctx context.Context, route *gatewayv1.HTTPRoute, present bool) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(route), route); err != nil {
			return err
		}
		if present {
			controllerutil.AddFinalizer(route, finalizerName)
		} else {
			controllerutil.RemoveFinalizer(route, finalizerName)
		}
		return r.Update(ctx, route)
	})
	if err != nil {
		return fmt.Errorf("updating finalizer: %w", err)
	}
	return nil
}

// release removes the managed DNS record for hostname, unless another live
// route still claims the name.
//
// Records the controller does not manage are never removed, whatever the
// annotation says: the provider only deletes rows carrying its ownership
// marker. A record written before ownership was marked is removed only for a
// name the route recorded owning and the domain map covers.
func (r *HTTPRouteReconciler) release(ctx context.Context, batch dns.Batch, route *gatewayv1.HTTPRoute, hostname string, owned []string) error {
	claimant, err := r.otherClaimant(ctx, route, hostname)
	if err != nil {
		return err
	}
	if claimant != "" {
		r.Log.Info("not deleting DNS record still claimed by another route", "hostname", hostname, "route", claimant)
		return nil
	}

	_, mapped := r.DomainMap.LookupIP(hostname)
	if err := batch.Remove(ctx, hostname, "A", mapped && slices.Contains(owned, hostname)); err != nil {
		dnsErrors.WithLabelValues("release").Inc()
		return err
	}
	recordResults.WithLabelValues("released").Inc()
	r.report(route, corev1.EventTypeNormal, "DNSRecordReleased", "%s", hostname)
	return nil
}

// otherClaimant returns another live route that has hostname in its spec, as
// namespace/name, or "" when there is none.
func (r *HTTPRouteReconciler) otherClaimant(ctx context.Context, route *gatewayv1.HTTPRoute, hostname string) (string, error) {
	var routes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routes, client.MatchingFields{hostnameIndex: hostname}); err != nil {
		return "", fmt.Errorf("listing routes that claim %s: %w", hostname, err)
	}
	self := client.ObjectKeyFromObject(route)
	for _, other := range routes.Items {
		if key := client.ObjectKeyFromObject(&other); key != self && other.DeletionTimestamp.IsZero() {
			return key.String(), nil
		}
	}
	return "", nil
}

// report logs an outcome and records it as an Event on the route.
func (r *HTTPRouteReconciler) report(route *gatewayv1.HTTPRoute, eventType, reason, note string, args ...any) {
	r.Log.Info(fmt.Sprintf(note, args...), "route", client.ObjectKeyFromObject(route), "reason", reason)
	if r.Recorder != nil {
		r.Recorder.Eventf(route, nil, eventType, reason, "ReconcileDNS", note, args...)
	}
}

// accepted reports whether at least one parent has accepted the route.
func accepted(route *gatewayv1.HTTPRoute) bool {
	return slices.ContainsFunc(route.Status.Parents, func(p gatewayv1.RouteParentStatus) bool {
		return meta.IsStatusConditionTrue(p.Conditions, string(gatewayv1.RouteConditionAccepted))
	})
}

// union returns the elements of a then b, deduplicated, preserving order.
func union(a, b []string) []string {
	var out []string
	for _, s := range slices.Concat(a, b) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
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
	return oldRoute.Generation != newRoute.Generation || // the spec changed, or deletion was requested
		// our finalizer came or went, for instance removed by hand
		controllerutil.ContainsFinalizer(oldRoute, finalizerName) != controllerutil.ContainsFinalizer(newRoute, finalizerName) ||
		// acceptance lives in status, which does not bump the generation
		(r.RequireAccepted && accepted(oldRoute) != accepted(newRoute))
}
