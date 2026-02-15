package controller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/config"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

const finalizerName = "dns.yk/cleanup"

// HTTPRouteReconciler reconciles HTTPRoute objects.
type HTTPRouteReconciler struct {
	client.Client
	Log       logr.Logger
	DomainMap *config.DomainMap
	DNS       dns.Provider
	Upsert    bool // when true, update existing records; when false, only create missing ones
}

func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var route gatewayv1.HTTPRoute
	if err := r.Get(ctx, req.NamespacedName, &route); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !route.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&route, finalizerName) {
			r.Log.Info("deleting DNS records for HTTPRoute", "name", req.NamespacedName)
			for _, hostname := range route.Spec.Hostnames {
				if err := r.DNS.Delete(ctx, string(hostname), "A"); err != nil {
					return ctrl.Result{}, fmt.Errorf("deleting DNS record for %s: %w", hostname, err)
				}
				r.Log.Info("deleted DNS record", "hostname", hostname)
			}
			controllerutil.RemoveFinalizer(&route, finalizerName)
			if err := r.Update(ctx, &route); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is present — return early so the update triggers
	// a fresh reconcile with the finalizer already in place.
	if !controllerutil.ContainsFinalizer(&route, finalizerName) {
		controllerutil.AddFinalizer(&route, finalizerName)
		if err := r.Update(ctx, &route); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Resolve IPs for each hostname
	for _, hostname := range route.Spec.Hostnames {
		ip, ok := r.DomainMap.LookupIP(string(hostname))
		if !ok {
			r.Log.Info("no domain mapping found for hostname", "hostname", hostname)
			continue
		}
		r.Log.Info("resolved hostname to IP", "hostname", hostname, "ip", ip)
		record := dns.Record{
			Hostname: string(hostname),
			Type:     "A",
			Value:    ip,
			Meta:     map[string]string{"description": "managed by yk-dns-manager"},
		}
		if r.Upsert {
			if err := r.DNS.Upsert(ctx, record); err != nil {
				return ctrl.Result{}, fmt.Errorf("upserting DNS record for %s: %w", hostname, err)
			}
			r.Log.Info("upserted DNS record", "hostname", hostname, "ip", ip)
		} else {
			exists, err := r.DNS.Exists(ctx, string(hostname), "A")
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("checking DNS record for %s: %w", hostname, err)
			}
			if exists {
				r.Log.Info("DNS record already exists, skipping", "hostname", hostname)
				continue
			}
			if err := r.DNS.Create(ctx, record); err != nil {
				return ctrl.Result{}, fmt.Errorf("creating DNS record for %s: %w", hostname, err)
			}
			r.Log.Info("created DNS record", "hostname", hostname, "ip", ip)
		}
	}

	return ctrl.Result{}, nil
}

func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{}).
		Complete(r)
}
