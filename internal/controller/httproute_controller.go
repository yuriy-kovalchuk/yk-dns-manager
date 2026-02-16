package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

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

func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var route gatewayv1.HTTPRoute
	if err := r.APIReader.Get(ctx, req.NamespacedName, &route); err != nil {
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
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&route, finalizerName) {
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

	var managedHostnames []string
	if val, ok := route.Annotations[managedHostnamesAnnotation]; ok {
		_ = json.Unmarshal([]byte(val), &managedHostnames)
	}

	currentHostnames := make([]string, 0, len(route.Spec.Hostnames))
	for _, h := range route.Spec.Hostnames {
		currentHostnames = append(currentHostnames, string(h))
	}

	// Delete hostnames that were removed from the spec
	for _, oldHost := range managedHostnames {
		if !Contains(currentHostnames, oldHost) {
			r.Log.Info("hostname removed from HTTPRoute, deleting DNS record", "hostname", oldHost)
			if err := r.DNS.Delete(ctx, oldHost, "A"); err != nil {
				return ctrl.Result{}, fmt.Errorf("deleting removed DNS record for %s: %w", oldHost, err)
			}
		}
	}

	// Update and Create
	for _, hostname := range route.Spec.Hostnames {
		ip, ok := r.DomainMap.LookupIP(string(hostname))
		if !ok {
			r.Log.V(1).Info("no domain mapping found for hostname", "hostname", hostname)
			continue
		}

		r.Log.V(1).Info("resolved hostname to IP", "hostname", hostname, "ip", ip)
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
			continue
		}

		// Non-upsert path: only create if missing
		exists, err := r.DNS.Exists(ctx, string(hostname), "A")
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("checking DNS record for %s: %w", hostname, err)
		}
		if exists {
			r.Log.V(1).Info("DNS record already exists, skipping", "hostname", hostname)
			continue
		}
		if err := r.DNS.Create(ctx, record); err != nil {
			return ctrl.Result{}, fmt.Errorf("creating DNS record for %s: %w", hostname, err)
		}
		r.Log.Info("created DNS record", "hostname", hostname, "ip", ip)
	}

	// Update annotation with the current list of managed hostnames
	if !reflect.DeepEqual(managedHostnames, currentHostnames) {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.APIReader.Get(ctx, req.NamespacedName, &route); err != nil {
				return err
			}
			if route.Annotations == nil {
				route.Annotations = make(map[string]string)
			}
			data, _ := json.Marshal(currentHostnames)
			route.Annotations[managedHostnamesAnnotation] = string(data)
			return r.Update(ctx, &route)
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update managed-hostnames annotation: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

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
