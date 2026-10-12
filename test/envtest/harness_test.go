// Package envtest runs the real controller manager against a real API server
// (controller-runtime envtest) and the in-memory OPNsense fake, through the real
// OPNsense provider. Unlike the unit tests, which call Reconcile directly on a
// fake client, this exercises the event predicates, the finalizer → reconcile
// chain and the API server's generation and finalizer semantics.
//
// It needs the envtest control-plane binaries. `make test-envtest` downloads
// them and sets KUBEBUILDER_ASSETS; without that variable the suite is skipped.
package envtest

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/opnsense"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/testenv"
)

const (
	finalizerName              = "dns.yk/cleanup"
	managedHostnamesAnnotation = "dns.yk/managed-hostnames"

	// waitTimeout bounds every eventually. A reconcile against the in-process
	// fake takes milliseconds, so this only matters when something is wrong.
	waitTimeout = 15 * time.Second
	// settleTime is how long consistently watches for something that must not
	// happen. It is long enough for several reconciles to run.
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

	// controller-runtime logs through the global logger. The suite discards it;
	// each test gives its reconciler a logger that writes into the test output.
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

// gatewayCRDDir returns the standard-channel CRDs shipped in the Gateway API
// module go.mod pins, so the API server serves exactly the types the controller
// is compiled against.
func gatewayCRDDir() (string, error) {
	out, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/gateway-api").Output()
	if err != nil {
		return "", err
	}
	return filepath.Join(strings.TrimSpace(string(out)), "config", "crd", "standard"), nil
}

// harnessOptions configures one controller instance.
type harnessOptions struct {
	domainMap string
	upsert    bool
	// requireAccepted turns on --require-accepted-routes.
	requireAccepted bool
	// cleanupGrace defaults to 30s, long enough for any test that waits for
	// a failing cleanup to recover.
	cleanupGrace time.Duration
	// syncPeriod is the informer resync period; it defaults to the controller's
	// own default of one hour.
	syncPeriod time.Duration
	// publishInterval is the provider's publish_interval setting: the shortest
	// gap between two publishes. Zero means the provider publishes every batch
	// that owes one.
	publishInterval time.Duration
}

// harness is one running controller, watching its own namespace and talking to
// its own fake appliance.
type harness struct {
	t      *testing.T
	ns     string
	fake   *testenv.OPNsense
	client client.Client
	srvURL string
}

// start creates a namespace, a fake appliance and a manager running the real
// reconciler and provider. Everything is torn down when the test ends: routes
// are deleted while the controller still runs, so finalizers are released the
// normal way.
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

	fake := testenv.NewOPNsense()
	fake.RequireCredentials("envtest-key", "envtest-secret")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	log := testr.NewWithOptions(t, testr.Options{Verbosity: 1})

	settings := map[string]string{
		"base_url":            srv.URL + "/api",
		"api_key":             "envtest-key",
		"api_secret":          "envtest-secret",
		"allow_insecure_http": "true",
	}
	if opts.publishInterval > 0 {
		settings["publish_interval"] = opts.publishInterval.String()
	}
	provider, err := opnsense.New(log.WithName("dns-opnsense"), settings)
	if err != nil {
		t.Fatalf("creating provider: %v", err)
	}

	mapPath := filepath.Join(t.TempDir(), "domain-map.yaml")
	if err := os.WriteFile(mapPath, []byte(opts.domainMap), 0o600); err != nil {
		t.Fatal(err)
	}
	domainMap, err := config.LoadDomainMap(mapPath)
	if err != nil {
		t.Fatalf("loading domain map: %v", err)
	}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{ns: {}},
			SyncPeriod:        ptr.To(cmp.Or(opts.syncPeriod, time.Hour)),
		},
		// Every test starts its own manager in the same process, and
		// controller-runtime rejects a second controller with the same name.
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

	// Cleanups run last-registered first: routes are deleted while the
	// manager still runs, then the manager stops.
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager exited with error: %v", err)
		}
	})

	h := &harness{t: t, ns: ns, fake: fake, client: c, srvURL: srv.URL}
	t.Cleanup(h.deleteAllRoutes)
	return h
}

// routeOption adjusts a route before it is created.
type routeOption func(*routeSpec)

type routeSpec struct {
	unaccepted bool
}

// createRoute creates an HTTPRoute with the given hostnames. Unless told
// otherwise it also writes the status a Gateway controller would, marking the
// route Accepted by its parent, so these tests keep describing an admitted
// route once the controller starts checking for one (audit S3).
func (h *harness) createRoute(name string, hostnames []string, opts ...routeOption) {
	h.t.Helper()
	var spec routeSpec
	for _, o := range opts {
		o(&spec)
	}

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

	if spec.unaccepted {
		return
	}
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

// updateRoute applies mutate to the latest copy of the route, retrying on
// conflict since the controller writes the same object.
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
		return r != nil && hasFinalizer(r)
	})
}

func (h *harness) waitOwned(name, annotation string) {
	h.t.Helper()
	h.eventually(fmt.Sprintf("route %s owns %s", name, annotation), func() bool {
		r := h.getRoute(name)
		return r != nil && r.Annotations[managedHostnamesAnnotation] == annotation
	})
}

// deleteAllRoutes deletes every route in the namespace and waits for the
// controller to release them. A route still stuck after the wait has its
// finalizers stripped so the next test starts clean, and the test fails.
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

// consistently fails the test if cond stops holding at any point during
// settleTime.
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

// dropOnAppliance deletes an override through the appliance's own API and
// republishes Unbound, the way an operator editing the appliance by hand would.
// The controller is left with a backend that no longer matches the route.
func (h *harness) dropOnAppliance(fqdn string) {
	h.t.Helper()
	settings := h.srvURL + "/api/unbound/settings/"
	found := false
	for uuid, row := range h.fake.Snapshot() {
		if row.FQDN() != fqdn {
			continue
		}
		found = true
		h.postToAppliance(settings + testenv.EndpointDel + "/" + uuid)
	}
	if !found {
		h.t.Fatalf("no override for %s to delete", fqdn)
	}
	h.postToAppliance(h.srvURL + "/api/unbound/service/" + testenv.EndpointReconfigure)
}

func (h *harness) postToAppliance(url string) {
	h.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		h.t.Fatal(err)
	}
	req.SetBasicAuth("envtest-key", "envtest-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("posting to %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("posting to %s: status %d", url, resp.StatusCode)
	}
}

// countCalls counts appliance requests whose path contains endpoint.
func (h *harness) countCalls(endpoint string) int {
	n := 0
	for _, c := range h.fake.Calls() {
		if strings.Contains(c, "/"+endpoint) {
			n++
		}
	}
	return n
}

// rows returns the overrides in store whose name is fqdn.
func rows(store map[string]testenv.HostOverride, fqdn string) []testenv.HostOverride {
	var out []testenv.HostOverride
	for _, h := range store {
		if h.FQDN() == fqdn {
			out = append(out, h)
		}
	}
	return out
}

// hasRow reports whether store holds exactly one override for fqdn, enabled and
// pointing at ip.
func hasRow(store map[string]testenv.HostOverride, fqdn, ip string) bool {
	r := rows(store, fqdn)
	return len(r) == 1 && r[0].Enabled == "1" && r[0].Server == ip
}

func hasFinalizer(r *gatewayv1.HTTPRoute) bool {
	for _, f := range r.Finalizers {
		if f == finalizerName {
			return true
		}
	}
	return false
}
