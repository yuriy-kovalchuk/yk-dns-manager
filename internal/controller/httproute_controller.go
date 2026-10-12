// Package controller makes each HTTPRoute hostname the domain map covers
// resolve to the domain map's IP, and removes the record when the hostname or
// the route goes away.
//
// The provider only changes records it marked as its own. The route's
// managed-hostnames annotation says what to clean up, never what may be
// deleted, since any route editor can write it. A record shared by several
// routes is deleted only when none claims it.
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
func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	route := &gatewayv1.HTTPRoute{}
	if err := r.Get(ctx, req.NamespacedName, route); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// owned: what the route managed so far. A corrupt annotation counts as
	// empty, because guessing could delete the wrong records.
	var owned []string
	if value := route.Annotations[managedHostnamesAnnotation]; value != "" {
		if err := json.Unmarshal([]byte(value), &owned); err != nil {
			r.Log.Error(err, "failed to parse managed-hostnames annotation, treating route as owning nothing", "annotation", value)
			owned = nil
		}
	}

	// wanted: the spec hostnames the domain map covers, or none for a route no
	// Gateway accepted when that is required.
	var wanted []string
	if r.RequireAccepted && !isAccepted(route) {
		r.Log.V(1).Info("route is not accepted by any parent; not publishing DNS for it", "route", req.NamespacedName)
	} else {
		for _, h := range route.Spec.Hostnames {
			if _, ok := r.DomainMap.LookupIP(string(h)); ok {
				wanted = append(wanted, string(h))
			}
		}
	}

	if !route.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, route, owned, wanted)
	}

	// Add the finalizer before any record exists, so a deletion can never
	// skip the cleanup. A route with nothing to manage gets none.
	if !controllerutil.ContainsFinalizer(route, finalizerName) {
		if len(wanted) == 0 {
			return ctrl.Result{}, nil
		}
		if err := r.setFinalizer(ctx, route, true); err != nil {
			r.report(route, corev1.EventTypeWarning, "DNSSyncFailed", err.Error())
			return ctrl.Result{}, err
		}
	}

	// All changes go through one batch and are published once, at the end,
	// even after an error. A failed publish is returned so it is retried.
	batch := r.DNS.Batch()
	defer func() {
		if commitErr := batch.Commit(ctx); commitErr != nil {
			dnsErrors.WithLabelValues("publish").Inc()
			err = errors.Join(err, fmt.Errorf("publishing DNS changes: %w", commitErr))
		}
		if err != nil {
			r.report(route, corev1.EventTypeWarning, "DNSSyncFailed", err.Error())
		}
	}()

	// Remove the records of hostnames the route no longer wants.
	for _, h := range owned {
		if !slices.Contains(wanted, h) {
			if err := r.removeRecord(ctx, batch, route, h, owned); err != nil {
				return ctrl.Result{}, fmt.Errorf("deleting removed DNS record for %s: %w", h, err)
			}
		}
	}

	// Create or update the wanted ones. A legacy record (from before the
	// provider marked ownership) is only taken over for a hostname this route
	// already managed. A hostname held by someone else's record stays out of
	// the annotation, so the route can never claim it later.
	var managed []string
	for _, h := range wanted {
		ip, _ := r.DomainMap.LookupIP(h)
		res, err := batch.Ensure(ctx, dns.Record{Hostname: h, Type: "A", Value: ip},
			dns.EnsureOptions{Overwrite: r.Upsert, AdoptLegacy: slices.Contains(owned, h)})
		if err != nil {
			dnsErrors.WithLabelValues("ensure").Inc()
			return ctrl.Result{}, fmt.Errorf("ensuring DNS record for %s: %w", h, err)
		}
		recordResults.WithLabelValues(res.String()).Inc()
		switch res {
		case dns.Created:
			r.report(route, corev1.EventTypeNormal, "DNSRecordCreated", h+" -> "+ip)
		case dns.Updated:
			r.report(route, corev1.EventTypeNormal, "DNSRecordUpdated", h+" -> "+ip)
		case dns.Kept:
			r.report(route, corev1.EventTypeWarning, "DNSRecordDrift", h+" holds a different value than "+ip+"; upsert mode is off, so it is left alone")
		case dns.Conflict:
			r.report(route, corev1.EventTypeWarning, "DNSConflict", h+" already has a DNS record this controller does not manage; it is left alone")
			continue
		case dns.Unchanged:
		}
		managed = append(managed, h)
	}

	// Save what the route manages now. The patch touches only this annotation.
	if !slices.Equal(owned, managed) {
		list, _ := json.Marshal(managed)
		original := route.DeepCopy()
		if route.Annotations == nil {
			route.Annotations = map[string]string{}
		}
		route.Annotations[managedHostnamesAnnotation] = string(list)
		if err := r.Patch(ctx, route, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update managed-hostnames annotation: %w", err)
		}
	}
	return ctrl.Result{}, nil
}

// reconcileDelete removes a deleted route's records, publishes that, and then
// removes the finalizer. A failing cleanup is retried until CleanupGracePeriod
// after the deletion request; then the route is released anyway, because it
// must always be deletable, and its records may be left behind.
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
	if err := batch.Commit(ctx); err != nil {
		dnsErrors.WithLabelValues("publish").Inc()
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("publishing: %w", err))
	}

	if cleanupErr != nil {
		giveUpAt := route.DeletionTimestamp.Add(r.CleanupGracePeriod)
		if timeLeft := time.Until(giveUpAt); timeLeft > 0 {
			r.report(route, corev1.EventTypeWarning, "DNSCleanupFailed", fmt.Sprintf("retrying until %s: %v", giveUpAt.Format(time.RFC3339), cleanupErr))
			return ctrl.Result{RequeueAfter: min(timeLeft, cleanupRetryInterval)}, nil
		}
		r.report(route, corev1.EventTypeWarning, "DNSCleanupAbandoned", fmt.Sprintf("releasing the route after the grace period; DNS records may be left behind: %v", cleanupErr))
	}
	return ctrl.Result{}, r.setFinalizer(ctx, route, false)
}

// removeRecord deletes the record for hostname unless another live route
// still claims it. The provider only deletes records it marked as its own; a
// legacy record only for a hostname this route managed and the domain map covers.
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
	if err := batch.Remove(ctx, hostname, "A", inDomainMap && slices.Contains(owned, hostname)); err != nil {
		dnsErrors.WithLabelValues("release").Inc()
		return err
	}
	recordResults.WithLabelValues("released").Inc()
	r.report(route, corev1.EventTypeNormal, "DNSRecordReleased", hostname)
	return nil
}

// setFinalizer adds or removes our finalizer. It re-reads the route and
// retries on conflict instead of patching, because a patch would replace the
// whole finalizer list and could drop another controller's entry.
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

// wantsUpdate decides which HTTPRoute updates trigger a reconcile. Status- and
// metadata-only updates, including our own annotation patch, are ignored.
func (r *HTTPRouteReconciler) wantsUpdate(e event.UpdateEvent) bool {
	oldRoute, okOld := e.ObjectOld.(*gatewayv1.HTTPRoute)
	newRoute, okNew := e.ObjectNew.(*gatewayv1.HTTPRoute)
	if !okOld || !okNew {
		return false
	}
	specChanged := oldRoute.Generation != newRoute.Generation // includes a deletion request
	finalizerChanged := controllerutil.ContainsFinalizer(oldRoute, finalizerName) != controllerutil.ContainsFinalizer(newRoute, finalizerName)
	acceptanceChanged := r.RequireAccepted && isAccepted(oldRoute) != isAccepted(newRoute) // status does not bump the generation
	return specChanged || finalizerChanged || acceptanceChanged
}
