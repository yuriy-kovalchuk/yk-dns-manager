// Package envtest runs the real controller manager against a real API server and the in-memory DNS provider, exercising the event predicates, the finalizer chain and the API server's semantics that unit tests bypass; it needs the envtest control-plane binaries, which make test-envtest downloads and points at through KUBEBUILDER_ASSETS.
package envtest

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	crenvtest "sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/config"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/controller"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/memory"
)

const (
	finalizerName              = "dns.yk/cleanup"
	managedHostnamesAnnotation = "dns.yk/managed-hostnames"

	// waitTimeout bounds every eventually: a reconcile against the in-memory provider takes milliseconds, so it only matters when something is wrong.
	waitTimeout = 15 * time.Second
	// settleTime is how long consistently watches for something that must not happen, long enough for several reconciles to run.
	settleTime = 3 * time.Second
)

var (
	restConfig *rest.Config
	scheme     = runtime.NewScheme()
	nsCounter  atomic.Int64
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
}

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("skipping envtest suite: KUBEBUILDER_ASSETS is not set (run `make test-envtest`)")
		os.Exit(0)
	}

	// controller-runtime logs through the global logger, which the suite discards; each test gives its reconciler a logger writing into the test output.
	ctrl.SetLogger(zap.New(zap.WriteTo(io.Discard)))

	crdDir, err := gatewayCRDDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "locating Gateway API CRDs: %v\n", err)
		os.Exit(1)
	}

	env := &crenvtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
	}
	restConfig, err = env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "starting envtest: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	if err := env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", err)
	}
	os.Exit(code)
}

// gatewayCRDDir returns the standard-channel CRDs shipped in the Gateway API module go.mod pins, so the API server serves exactly the types the controller compiles against.
func gatewayCRDDir() (string, error) {
	out, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/gateway-api").Output()
	if err != nil {
		return "", err
	}
	return filepath.Join(strings.TrimSpace(string(out)), "config", "crd", "standard"), nil
}

// harnessOptions configures one controller instance.
type harnessOptions struct {
	domainMap map[string]string
	upsert    bool
	// requireAccepted turns on --require-accepted-routes.
	requireAccepted bool
	// cleanupGrace defaults to 30s, long enough for a test waiting on a failing cleanup to recover.
	cleanupGrace time.Duration
	// syncPeriod is the informer resync period; it defaults to the controller's own default of one hour.
	syncPeriod time.Duration
	// publishInterval is the provider's publish_interval setting: the shortest gap between two publishes, zero publishing every batch that owes one.
	publishInterval time.Duration
}

// harness is one running controller, watching its own namespace and writing to its own in-memory DNS backend.
type harness struct {
	t      *testing.T
	ns     string
	dns    *memory.Provider
	client client.Client
}

// start creates a namespace, a DNS backend and a manager running the real reconciler, and tears everything down when the test ends.
func start(t *testing.T, opts harnessOptions) *harness {
	t.Helper()
	ctx := context.Background()

	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	ns := fmt.Sprintf("t%d", nsCounter.Add(1))
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	log := testr.NewWithOptions(t, testr.Options{Verbosity: 1})

	provider, err := memory.New(log.WithName("dns-memory"), map[string]string{"publish_interval": opts.publishInterval.String()})
	if err != nil {
		t.Fatalf("creating provider: %v", err)
	}

	domainMap, err := config.NewDomainMap(opts.domainMap)
	if err != nil {
		t.Fatalf("building domain map: %v", err)
	}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{ns: {}},
			SyncPeriod:        ptr.To(cmp.Or(opts.syncPeriod, time.Hour)),
		},
		// every test starts its own manager in the same process, and controller-runtime rejects a second controller with the same name
		Controller: ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("creating manager: %v", err)
	}

	reconciler := &controller.HTTPRouteReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Log:       log.WithName("httproute-controller"),
		Recorder:  mgr.GetEventRecorder("yk-dns-manager"),
		DomainMap: domainMap,
		DNS:       provider,
		Upsert:    opts.upsert,

		RequireAccepted:    opts.requireAccepted,
		CleanupGracePeriod: cmp.Or(opts.cleanupGrace, 30*time.Second),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatalf("setting up reconciler: %v", err)
	}

	mgrCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(mgrCtx) }()

	// cleanups run last-registered first: routes are deleted while the manager still runs, then the manager stops.
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager exited with error: %v", err)
		}
	})

	h := &harness{t: t, ns: ns, dns: provider, client: c}
	t.Cleanup(h.deleteAllRoutes)
	return h
}

// createUnacceptedRoute creates an HTTPRoute with the given hostnames and no status, as if no Gateway admitted it.
func (h *harness) createUnacceptedRoute(name string, hostnames []string) *gatewayv1.HTTPRoute {
	h.t.Helper()
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.ns},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{Name: "gateway"}},
			},
		},
	}
	for _, hn := range hostnames {
		route.Spec.Hostnames = append(route.Spec.Hostnames, gatewayv1.Hostname(hn))
	}
	if err := h.client.Create(context.Background(), route); err != nil {
		h.t.Fatalf("creating route %s: %v", name, err)
	}
	return route
}

// createRoute also writes the status a Gateway controller would, so these tests keep describing an admitted route (audit S3).
func (h *harness) createRoute(name string, hostnames []string) {
	h.t.Helper()
	route := h.createUnacceptedRoute(name, hostnames)
	route.Status.Parents = []gatewayv1.RouteParentStatus{{
		ParentRef:      gatewayv1.ParentReference{Name: "gateway"},
		ControllerName: "example.com/gateway-controller",
		Conditions: []metav1.Condition{{
			Type:               string(gatewayv1.RouteConditionAccepted),
			Status:             metav1.ConditionTrue,
			Reason:             string(gatewayv1.RouteReasonAccepted),
			ObservedGeneration: route.Generation,
			LastTransitionTime: metav1.Now(),
		}},
	}}
	if err := h.client.Status().Update(context.Background(), route); err != nil {
		h.t.Fatalf("writing status for route %s: %v", name, err)
	}
}

// getRoute returns the route, or nil when it does not exist.
func (h *harness) getRoute(name string) *gatewayv1.HTTPRoute {
	h.t.Helper()
	var route gatewayv1.HTTPRoute
	err := h.client.Get(context.Background(), types.NamespacedName{Namespace: h.ns, Name: name}, &route)
	if client.IgnoreNotFound(err) != nil {
		h.t.Fatalf("getting route %s: %v", name, err)
	}
	if err != nil {
		return nil
	}
	return &route
}

// updateRoute applies mutate to the latest copy of the route, retrying on conflict since the controller writes the same object.
func (h *harness) updateRoute(name string, mutate func(*gatewayv1.HTTPRoute)) {
	h.t.Helper()
	h.eventually("route "+name+" updated", func() bool {
		route := h.getRoute(name)
		if route == nil {
			h.t.Fatalf("route %s does not exist", name)
		}
		mutate(route)
		return h.client.Update(context.Background(), route) == nil
	})
}

func (h *harness) deleteRoute(name string) {
	h.t.Helper()
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.ns}}
	if err := h.client.Delete(context.Background(), route); err != nil {
		h.t.Fatalf("deleting route %s: %v", name, err)
	}
}

func (h *harness) waitRouteGone(name string) {
	h.t.Helper()
	h.eventually("route "+name+" deleted", func() bool { return h.getRoute(name) == nil })
}

func (h *harness) waitFinalizer(name string) {
	h.t.Helper()
	h.eventually("finalizer on "+name, func() bool {
		r := h.getRoute(name)
		return r != nil && slices.Contains(r.Finalizers, finalizerName)
	})
}

func (h *harness) waitOwned(name, annotation string) {
	h.t.Helper()
	h.eventually(fmt.Sprintf("route %s owns %s", name, annotation), func() bool {
		r := h.getRoute(name)
		return r != nil && r.Annotations[managedHostnamesAnnotation] == annotation
	})
}

// deleteAllRoutes deletes every route in the namespace and waits for the controller to release them; one still stuck has its finalizers stripped and the test fails.
func (h *harness) deleteAllRoutes() {
	ctx := context.Background()
	var routes gatewayv1.HTTPRouteList
	if err := h.client.List(ctx, &routes, client.InNamespace(h.ns)); err != nil {
		h.t.Errorf("cleanup: listing routes: %v", err)
		return
	}
	for i := range routes.Items {
		_ = h.client.Delete(ctx, &routes.Items[i])
	}

	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if err := h.client.List(ctx, &routes, client.InNamespace(h.ns)); err == nil && len(routes.Items) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	for i := range routes.Items {
		r := &routes.Items[i]
		r.Finalizers = nil
		_ = h.client.Update(ctx, r)
	}
	h.t.Errorf("cleanup: %d route(s) were not released by the controller", len(routes.Items))
}

// eventually polls cond until it holds or waitTimeout passes.
func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out after %s waiting for: %s", waitTimeout, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// consistently fails the test if cond stops holding at any point during settleTime.
func (h *harness) consistently(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(settleTime)
	for time.Now().Before(deadline) {
		if !cond() {
			h.t.Fatalf("expected to hold for %s, but it stopped: %s", settleTime, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// hasRecord reports whether records is exactly one enabled record pointing at ip.
func hasRecord(records []memory.Record, ip string) bool {
	return len(records) == 1 && !records[0].Disabled && records[0].Value == ip
}
