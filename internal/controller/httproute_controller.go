// Package controller makes each HTTPRoute hostname the domain map covers resolve to that map's IP, and cleans the record up when the hostname or the route goes away.
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
	managedHostnamesAnnotation = "dns.yk/managed-hostnames" // JSON list of hostnames the route manages
	hostnameIndex              = "spec.hostnames"           // finds every route that claims a hostname
	cleanupRetryInterval       = 10 * time.Second           // retry interval for a failing cleanup
)

// HTTPRouteReconciler reconciles HTTPRoute objects.
type HTTPRouteReconciler struct {
	client.Client
	APIReader          client.Reader // bypasses the cache; used for finalizer updates
	Log                logr.Logger
	Recorder           events.EventRecorder // nil disables Events
	DomainMap          *config.DomainMap
	DNS                dns.Provider
	Upsert             bool          // also fix managed records that hold a different value
	RequireAccepted    bool          // only routes a parent Gateway accepted get DNS
	CleanupGracePeriod time.Duration // how long a failing cleanup is retried before the route is released
}

// Reconcile brings the DNS records of one HTTPRoute in line with its spec.
func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	route := &gatewayv1.HTTPRoute{}
	if err := r.Get(ctx, req.NamespacedName, route); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	owned := r.ownedHostnames(route)
	wanted := r.wantedHostnames(route)

	if !route.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, route, owned, wanted)
	}
	// A route with nothing to manage gets no finalizer.
	if len(wanted) == 0 && !controllerutil.ContainsFinalizer(route, finalizerName) {
		return ctrl.Result{}, nil
	}

	// All changes go through one batch, published once at the end even after an error.
	batch := r.DNS.Batch()
	syncErr := r.sync(ctx, batch, route, owned, wanted)
	requeue, commitErr := commit(ctx, batch)
	if err := errors.Join(syncErr, commitErr); err != nil {
		r.report(route, corev1.EventTypeWarning, "DNSSyncFailed", err.Error())
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// sync adds the finalizer before any record exists, releases the hostnames the route dropped, ensures the ones it wants and records which it now manages.
func (r *HTTPRouteReconciler) sync(ctx context.Context, batch dns.Batch, route *gatewayv1.HTTPRoute, owned, wanted []string) error {
	if !controllerutil.ContainsFinalizer(route, finalizerName) {
		if err := r.setFinalizer(ctx, route, true); err != nil {
			return err
		}
	}
	for _, h := range owned {
		if slices.Contains(wanted, h) {
			continue
		}
		if err := r.removeRecord(ctx, batch, route, h, owned); err != nil {
			return fmt.Errorf("deleting removed DNS record for %s: %w", h, err)
		}
	}
	var managed []string
	for _, h := range wanted {
		isManaged, err := r.ensureRecord(ctx, batch, route, h, slices.Contains(owned, h))
		if err != nil {
			return err
		}
		if isManaged {
			managed = append(managed, h)
		}
	}
	if slices.Equal(owned, managed) {
		return nil
	}
	return r.saveManagedHostnames(ctx, route, managed)
}

// ensureRecord points hostname at its domain-map IP and reports whether the route now manages it; a legacy record is adopted only for a hostname the route already managed, and a name held by someone else's record is not managed.
func (r *HTTPRouteReconciler) ensureRecord(ctx context.Context, batch dns.Batch, route *gatewayv1.HTTPRoute, hostname string, adoptLegacy bool) (bool, error) {
	ip, _ := r.DomainMap.LookupIP(hostname)
	res, err := batch.Ensure(ctx, dns.Record{Hostname: hostname, Type: "A", Value: ip, Owner: routeOwner(route)},
		dns.EnsureOptions{Overwrite: r.Upsert, AdoptLegacy: adoptLegacy})
	if err != nil {
		dnsErrors.WithLabelValues("ensure").Inc()
		return false, fmt.Errorf("ensuring DNS record for %s: %w", hostname, err)
	}
	recordResults.WithLabelValues(res.String()).Inc()
	switch res {
	case dns.Created:
		r.report(route, corev1.EventTypeNormal, "DNSRecordCreated", hostname+" -> "+ip)
	case dns.Updated:
		r.report(route, corev1.EventTypeNormal, "DNSRecordUpdated", hostname+" -> "+ip)
	case dns.Kept:
		r.report(route, corev1.EventTypeWarning, "DNSRecordDrift", hostname+" holds a different value than "+ip+"; upsert mode is off, so it is left alone")
	case dns.Conflict:
		r.report(route, corev1.EventTypeWarning, "DNSConflict", hostname+" already has a DNS record this controller does not manage; it is left alone")
		return false, nil
	case dns.Unchanged:
	}
	return true, nil
}

// commit publishes the batch; a publish the provider holds back to coalesce a burst is not an error but a time to come back.
func commit(ctx context.Context, batch dns.Batch) (time.Duration, error) {
	err := batch.Commit(ctx)
	var deferred *dns.PublishDeferred
	if errors.As(err, &deferred) {
		return deferred.After, nil
	}
	if err != nil {
		dnsErrors.WithLabelValues("publish").Inc()
		return 0, fmt.Errorf("publishing DNS changes: %w", err)
	}
	return 0, nil
}

// ownedHostnames are the hostnames the route recorded managing; a corrupt annotation counts as empty because guessing could delete the wrong records.
func (r *HTTPRouteReconciler) ownedHostnames(route *gatewayv1.HTTPRoute) []string {
	value := route.Annotations[managedHostnamesAnnotation]
	if value == "" {
		return nil
	}
	var owned []string
	if err := json.Unmarshal([]byte(value), &owned); err != nil {
		r.Log.Error(err, "failed to parse managed-hostnames annotation, treating route as owning nothing", "annotation", value)
		return nil
	}
	return owned
}

// wantedHostnames are the spec hostnames the domain map covers, or none for a route no Gateway accepted when acceptance is required.
func (r *HTTPRouteReconciler) wantedHostnames(route *gatewayv1.HTTPRoute) []string {
	if r.RequireAccepted && !isAccepted(route) {
		r.Log.V(1).Info("route is not accepted by any parent; not publishing DNS for it", "route", client.ObjectKeyFromObject(route))
		return nil
	}
	var wanted []string
	for _, h := range route.Spec.Hostnames {
		if _, ok := r.DomainMap.LookupIP(string(h)); !ok {
			continue
		}
		// A wildcard route hostname would become a wildcard record answering for names other routes serve, so only exact names are managed.
		if strings.HasPrefix(string(h), "*.") {
			r.report(route, corev1.EventTypeWarning, "DNSWildcardHostnameIgnored", string(h)+" is a wildcard hostname; only exact names get DNS records")
			continue
		}
		wanted = append(wanted, string(h))
	}
	return wanted
}

// saveManagedHostnames records what the route manages now; the patch touches only this annotation.
func (r *HTTPRouteReconciler) saveManagedHostnames(ctx context.Context, route *gatewayv1.HTTPRoute, managed []string) error {
	list, _ := json.Marshal(managed)
	original := route.DeepCopy()
	if route.Annotations == nil {
		route.Annotations = map[string]string{}
	}
	route.Annotations[managedHostnamesAnnotation] = string(list)
	if err := r.Patch(ctx, route, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("failed to update managed-hostnames annotation: %w", err)
	}
	return nil
}

// reconcileDelete removes a deleted route's records, publishes that, then drops the finalizer; a failing cleanup retries until the grace period runs out and the route is released anyway, because it must always be deletable.
func (r *HTTPRouteReconciler) reconcileDelete(ctx context.Context, route *gatewayv1.HTTPRoute, owned, wanted []string) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(route, finalizerName) {
		return ctrl.Result{}, nil
	}
	r.Log.Info("deleting DNS records for HTTPRoute", "route", client.ObjectKeyFromObject(route))

	// The wanted hostnames cover routes from before the annotation existed.
	toRemove := slices.Clone(owned)
	for _, h := range wanted {
		if !slices.Contains(toRemove, h) {
			toRemove = append(toRemove, h)
		}
	}

	batch := r.DNS.Batch()
	var cleanupErr error
	for _, h := range toRemove {
		if err := r.removeRecord(ctx, batch, route, h, owned); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("%s: %w", h, err))
		}
	}
	requeue, err := commit(ctx, batch)
	cleanupErr = errors.Join(cleanupErr, err)

	giveUpAt := route.DeletionTimestamp.Add(r.CleanupGracePeriod)
	timeLeft := time.Until(giveUpAt)
	switch {
	case cleanupErr == nil && requeue == 0:
		// published
	case timeLeft <= 0:
		if requeue > 0 {
			cleanupErr = errors.Join(cleanupErr, errors.New("the deletion was never published before the grace period ran out"))
		}
		r.report(route, corev1.EventTypeWarning, "DNSCleanupAbandoned", fmt.Sprintf("releasing the route after the grace period; DNS records may be left behind: %v", cleanupErr))
	case cleanupErr == nil:
		// The provider is only holding the publish back, so the finalizer stays until the deletion actually goes out.
		return ctrl.Result{RequeueAfter: min(requeue, timeLeft)}, nil
	default:
		r.report(route, corev1.EventTypeWarning, "DNSCleanupFailed", fmt.Sprintf("retrying until %s: %v", giveUpAt.Format(time.RFC3339), cleanupErr))
		return ctrl.Result{RequeueAfter: min(timeLeft, cleanupRetryInterval)}, nil
	}
	return ctrl.Result{}, r.setFinalizer(ctx, route, false)
}

// removeRecord deletes the record for hostname unless another live route still claims it; the provider deletes only rows it marked, and only those this route owns.
func (r *HTTPRouteReconciler) removeRecord(ctx context.Context, batch dns.Batch, route *gatewayv1.HTTPRoute, hostname string, owned []string) error {
	var claimants gatewayv1.HTTPRouteList
	if err := r.List(ctx, &claimants, client.MatchingFields{hostnameIndex: hostname}); err != nil {
		return fmt.Errorf("listing routes that claim %s: %w", hostname, err)
	}
	for _, other := range claimants.Items {
		isSelf := other.Namespace == route.Namespace && other.Name == route.Name
		if !isSelf && other.DeletionTimestamp.IsZero() {
			r.Log.Info("not deleting DNS record still claimed by another route", "hostname", hostname, "route", other.Namespace+"/"+other.Name)
			return nil
		}
	}

	_, inDomainMap := r.DomainMap.LookupIP(hostname)
	if err := batch.Remove(ctx, hostname, "A", routeOwner(route), inDomainMap && slices.Contains(owned, hostname)); err != nil {
		dnsErrors.WithLabelValues("release").Inc()
		return err
	}
	recordResults.WithLabelValues("released").Inc()
	r.report(route, corev1.EventTypeNormal, "DNSRecordReleased", hostname)
	return nil
}

// setFinalizer adds or removes our finalizer, re-reading and retrying on conflict because a patch would replace the whole finalizer list and could drop another controller's entry.
func (r *HTTPRouteReconciler) setFinalizer(ctx context.Context, route *gatewayv1.HTTPRoute, add bool) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(route), route); err != nil {
			return err
		}
		if add {
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

// report logs what happened and records it as an Event on the route.
func (r *HTTPRouteReconciler) report(route *gatewayv1.HTTPRoute, eventType, reason, message string) {
	r.Log.Info(message, "route", client.ObjectKeyFromObject(route), "reason", reason)
	if r.Recorder != nil {
		r.Recorder.Eventf(route, nil, eventType, reason, "ReconcileDNS", "%s", message)
	}
}

// isAccepted reports whether at least one parent Gateway accepted the route.
func isAccepted(route *gatewayv1.HTTPRoute) bool {
	for _, parent := range route.Status.Parents {
		if meta.IsStatusConditionTrue(parent.Conditions, string(gatewayv1.RouteConditionAccepted)) {
			return true
		}
	}
	return false
}

// routeOwner is the identity the provider stores with every record a route writes, which is what lets it refuse a record this route never wrote.
func routeOwner(route *gatewayv1.HTTPRoute) string {
	return "httproute/" + route.Namespace + "/" + route.Name
}

// indexHostnames returns a route's hostnames for hostnameIndex.
func indexHostnames(obj client.Object) []string {
	var hostnames []string
	if route, ok := obj.(*gatewayv1.HTTPRoute); ok {
		for _, h := range route.Spec.Hostnames {
			hostnames = append(hostnames, strings.TrimSuffix(string(h), "."))
		}
	}
	return hostnames
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

// wantsUpdate ignores status- and metadata-only updates including our own annotation patch, and passes informer resyncs on purpose: they are the only event that notices a record changed on the backend while the spec stayed put.
func (r *HTTPRouteReconciler) wantsUpdate(e event.UpdateEvent) bool {
	oldRoute, okOld := e.ObjectOld.(*gatewayv1.HTTPRoute)
	newRoute, okNew := e.ObjectNew.(*gatewayv1.HTTPRoute)
	if !okOld || !okNew {
		return false
	}
	resynced := oldRoute.ResourceVersion == newRoute.ResourceVersion
	specChanged := oldRoute.Generation != newRoute.Generation // includes a deletion request
	finalizerChanged := controllerutil.ContainsFinalizer(oldRoute, finalizerName) != controllerutil.ContainsFinalizer(newRoute, finalizerName)
	acceptanceChanged := r.RequireAccepted && isAccepted(oldRoute) != isAccepted(newRoute) // status does not bump the generation
	return resynced || specChanged || finalizerChanged || acceptanceChanged
}
