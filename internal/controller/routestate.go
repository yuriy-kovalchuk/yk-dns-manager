package controller

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"k8s.io/client-go/util/retry"
)

// RouteState owns every Kubernetes mutation performed for managed
// HTTPRoutes: adding/removing the cleanup finalizer and writing the
// managed-hostnames annotation.
type RouteState struct {
	writer    client.Client
	apiReader client.Reader
	log       logr.Logger
}

// NewRouteState creates a RouteState. apiReader (cache-bypassing) is used
// for reads inside conflict-retry loops to guarantee read-after-write
// consistency.
func NewRouteState(writer client.Client, apiReader client.Reader, log logr.Logger) *RouteState {
	return &RouteState{writer: writer, apiReader: apiReader, log: log}
}

// Get fetches the route bypassing the informer cache.
func (s *RouteState) Get(ctx context.Context, nn types.NamespacedName) (*gatewayv1.HTTPRoute, error) {
	var route gatewayv1.HTTPRoute
	if err := s.apiReader.Get(ctx, nn, &route); err != nil {
		return nil, err
	}
	return &route, nil
}

// EnsureFinalizer adds the cleanup finalizer if it is not present yet.
func (s *RouteState) EnsureFinalizer(ctx context.Context, route *gatewayv1.HTTPRoute) error {
	if controllerutil.ContainsFinalizer(route, finalizerName) {
		return nil
	}
	return s.mutate(ctx, route, func(latest *gatewayv1.HTTPRoute) error {
		controllerutil.AddFinalizer(latest, finalizerName)
		return s.writer.Update(ctx, latest)
	})
}

// RemoveFinalizer removes the cleanup finalizer if present.
func (s *RouteState) RemoveFinalizer(ctx context.Context, route *gatewayv1.HTTPRoute) error {
	if !controllerutil.ContainsFinalizer(route, finalizerName) {
		return nil
	}
	return s.mutate(ctx, route, func(latest *gatewayv1.HTTPRoute) error {
		controllerutil.RemoveFinalizer(latest, finalizerName)
		return s.writer.Update(ctx, latest)
	})
}

// SetManagedHostnames syncs the managed-hostnames annotation with
// hostnames, removing the annotation entirely when the list is empty.
func (s *RouteState) SetManagedHostnames(ctx context.Context, route *gatewayv1.HTTPRoute, hostnames []string) error {
	return s.mutate(ctx, route, func(latest *gatewayv1.HTTPRoute) error {
		current := s.ManagedHostnames(latest)
		if len(current) == 0 && len(hostnames) == 0 {
			return nil
		}
		if len(current) > 0 && reflect.DeepEqual(current, hostnames) {
			return nil
		}
		if len(hostnames) == 0 {
			delete(latest.Annotations, managedHostnamesAnnotation)
			return s.writer.Update(ctx, latest)
		}
		if latest.Annotations == nil {
			latest.Annotations = make(map[string]string)
		}
		data, err := json.Marshal(hostnames)
		if err != nil {
			return err
		}
		latest.Annotations[managedHostnamesAnnotation] = string(data)
		return s.writer.Update(ctx, latest)
	})
}

// ManagedHostnames parses the managed-hostnames annotation. A missing or
// malformed annotation yields an empty list (malformed is logged).
func (s *RouteState) ManagedHostnames(route *gatewayv1.HTTPRoute) []string {
	var hostnames []string
	val, ok := route.Annotations[managedHostnamesAnnotation]
	if !ok {
		return hostnames
	}
	if err := json.Unmarshal([]byte(val), &hostnames); err != nil {
		s.log.Error(err, "corrupt managed-hostnames annotation, treating as empty",
			"name", route.Name, "namespace", route.Namespace)
		hostnames = nil
	}
	return hostnames
}

// mutate re-reads the latest route (bypassing the cache) and applies fn,
// retrying on conflict.
func (s *RouteState) mutate(ctx context.Context, route *gatewayv1.HTTPRoute, fn func(*gatewayv1.HTTPRoute) error) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := s.Get(ctx, client.ObjectKeyFromObject(route))
		if err != nil {
			return err
		}
		return fn(latest)
	})
}
