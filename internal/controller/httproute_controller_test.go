package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/config"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/metrics"
)

// mockDNSProvider records DNS operations for assertions: Ensure is an upsert or a create depending on overwrite, conflictHosts hold records the controller does not manage, and Remove only deletes rows whose owners include the caller.
type mockDNSProvider struct {
	mu              sync.Mutex
	existingHosts   map[string]bool
	conflictHosts   map[string]bool
	createdRecords  []dns.Record
	upsertedRecords []dns.Record
	ensureOpts      map[string]dns.EnsureOptions
	deletedHosts    []string
	adoptedRemovals []string // hostnames removed with adoptLegacy set
	flushes         int
	flushErr        error
	deferPublish    time.Duration // >0: Commit holds the publish back this long
	removeErr       error
	known           map[string]string   // hostname → value of records this mock created
	owners          map[string][]string // hostname → owners recorded on the record
}

func (m *mockDNSProvider) HealthCheck(_ context.Context) error {
	return nil
}

func (m *mockDNSProvider) Batch() dns.Batch {
	return mockBatch{m}
}

type mockBatch struct{ m *mockDNSProvider }

func (b mockBatch) Ensure(_ context.Context, record dns.Record, opts dns.EnsureOptions) (dns.Result, error) {
	b.m.mu.Lock()
	defer b.m.mu.Unlock()
	if b.m.ensureOpts == nil {
		b.m.ensureOpts = map[string]dns.EnsureOptions{}
	}
	b.m.ensureOpts[record.Hostname] = opts
	if b.m.conflictHosts[record.Hostname] {
		return dns.Conflict, nil
	}
	if b.m.known == nil {
		b.m.known = map[string]string{}
	}
	value, ok := b.m.known[record.Hostname]
	if ok && value == record.Value && !b.m.needsOwner(record.Hostname, record.Owner) {
		return dns.Unchanged, nil
	}
	if ok && value == record.Value {
		// The address already matches; only the owner list needs joining.
		b.m.recordOwner(record.Hostname, record.Owner)
		return dns.Updated, nil
	}
	if opts.Overwrite {
		b.m.known[record.Hostname] = record.Value
		b.m.recordOwner(record.Hostname, record.Owner)
		b.m.upsertedRecords = append(b.m.upsertedRecords, record)
		return dns.Updated, nil
	}
	if ok || b.m.existingHosts[record.Hostname] {
		return dns.Kept, nil
	}
	b.m.known[record.Hostname] = record.Value
	b.m.recordOwner(record.Hostname, record.Owner)
	b.m.createdRecords = append(b.m.createdRecords, record)
	return dns.Created, nil
}

// needsOwner reports whether the mock's record for hostname is missing owner.
func (m *mockDNSProvider) needsOwner(hostname, owner string) bool {
	return owner != "" && !slices.Contains(m.owners[hostname], owner)
}

func (m *mockDNSProvider) recordOwner(hostname, owner string) {
	if owner == "" {
		return
	}
	if m.owners == nil {
		m.owners = map[string][]string{}
	}
	if !slices.Contains(m.owners[hostname], owner) {
		m.owners[hostname] = append(m.owners[hostname], owner)
	}
}

func (b mockBatch) Remove(_ context.Context, hostname, _, owner string, adoptLegacy bool) error {
	b.m.mu.Lock()
	defer b.m.mu.Unlock()
	if b.m.removeErr != nil {
		return b.m.removeErr
	}
	// A record other owners wrote belongs to them, whoever asks.
	if owners := b.m.owners[hostname]; len(owners) > 0 && !slices.Contains(owners, owner) {
		return nil
	}
	b.m.deletedHosts = append(b.m.deletedHosts, hostname)
	delete(b.m.known, hostname)
	delete(b.m.owners, hostname)
	if adoptLegacy {
		b.m.adoptedRemovals = append(b.m.adoptedRemovals, hostname)
	}
	return nil
}

func (b mockBatch) Commit(_ context.Context) error {
	b.m.mu.Lock()
	defer b.m.mu.Unlock()
	if b.m.deferPublish > 0 {
		return &dns.PublishDeferred{After: b.m.deferPublish}
	}
	b.m.flushes++
	return b.m.flushErr
}

// testDomainMap is the domain map most tests run against.
var testDomainMap = map[string]string{"*.my-domain1.com": "10.0.8.100", "*.my-domain2.it": "10.0.9.50"}

func newTestDomainMap(t *testing.T, entries map[string]string) *config.DomainMap {
	t.Helper()
	dm, err := config.NewDomainMap(entries)
	if err != nil {
		t.Fatal(err)
	}
	return dm
}

// newTestScheme returns the scheme every fixture here needs.
func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to install gateway-api scheme: %v", err)
	}
	return scheme
}

// newFakeRouteClient builds the fake client these fixtures share: the Gateway API scheme plus the hostname index the reconciler lists through.
func newFakeRouteClient(t *testing.T, routes ...*gatewayv1.HTTPRoute) client.WithWatch {
	t.Helper()
	builder := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithIndex(&gatewayv1.HTTPRoute{}, hostnameIndex, indexHostnames)
	for _, r := range routes {
		builder = builder.WithObjects(r).WithStatusSubresource(r)
	}
	return builder.Build()
}

// newTestReconciler wires the collaborators every test shares; the knobs a test is about (Upsert, RequireAccepted, CleanupGracePeriod, Recorder) go at the call site.
func newTestReconciler(t *testing.T, mock *mockDNSProvider, c client.WithWatch) *HTTPRouteReconciler {
	t.Helper()
	return &HTTPRouteReconciler{
		Client:    c,
		APIReader: c,
		Log:       zap.New(zap.UseDevMode(true)),
		DomainMap: newTestDomainMap(t, testDomainMap),
		DNS:       mock,
	}
}

// routeRequest is the reconcile request for default/<name>.
func routeRequest(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}}
}

// reconcile runs one reconcile of default/<name> and fails the test on an error.
func reconcile(t *testing.T, r *HTTPRouteReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), routeRequest(name))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

// reconcileOnce runs one upserting reconcile of default/<name> against a fake client holding routes, and returns the reconciler for inspection.
func reconcileOnce(t *testing.T, mock *mockDNSProvider, name string, routes ...*gatewayv1.HTTPRoute) *HTTPRouteReconciler {
	t.Helper()
	r := newTestReconciler(t, mock, newFakeRouteClient(t, routes...))
	r.Upsert = true
	reconcile(t, r, name)
	return r
}

// getRoute returns default/<name>, failing the test when it is gone.
func getRoute(t *testing.T, c client.Reader, name string) gatewayv1.HTTPRoute {
	t.Helper()
	var route gatewayv1.HTTPRoute
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &route); err != nil {
		t.Fatalf("getting route %s: %v", name, err)
	}
	return route
}

// managedRoute is default/<name> carrying our finalizer, plus the managed-hostnames annotation when it is not empty.
func managedRoute(name, annotation string, hostnames ...string) *gatewayv1.HTTPRoute {
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "default", Finalizers: []string{finalizerName},
	}}
	if annotation != "" {
		route.Annotations = map[string]string{managedHostnamesAnnotation: annotation}
	}
	for _, h := range hostnames {
		route.Spec.Hostnames = append(route.Spec.Hostnames, gatewayv1.Hostname(h))
	}
	return route
}

// TestHTTPRouteReconciler_Ensure covers what a new route's reconciles write: one record per covered hostname, created or upserted, never a second time.
func TestHTTPRouteReconciler_Ensure(t *testing.T) {
	want := []dns.Record{{Hostname: "app.my-domain1.com", Type: "A", Value: "10.0.8.100", Owner: "httproute/default/route"}}
	cases := []struct {
		name         string
		hostname     string
		upsert       bool
		existing     bool
		wantCreated  []dns.Record
		wantUpserted []dns.Record
	}{
		{name: "created", hostname: "app.my-domain1.com", wantCreated: want},
		{name: "upserted", hostname: "app.my-domain1.com", upsert: true, wantUpserted: want},
		{name: "existing record skipped without upsert", hostname: "app.my-domain1.com", existing: true},
		{name: "hostname outside the domain map", hostname: "app.unknown.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route := managedRoute("route", "", tc.hostname)
			route.Finalizers = nil
			mock := &mockDNSProvider{existingHosts: map[string]bool{"app.my-domain1.com": tc.existing}}
			r := newTestReconciler(t, mock, newFakeRouteClient(t, route))
			r.Upsert = tc.upsert

			for range 2 {
				if res := reconcile(t, r, "route"); res.RequeueAfter != 0 {
					t.Errorf("RequeueAfter = %s, want none", res.RequeueAfter)
				}
			}
			if !slices.Equal(mock.createdRecords, tc.wantCreated) {
				t.Errorf("created %+v, want %+v", mock.createdRecords, tc.wantCreated)
			}
			if !slices.Equal(mock.upsertedRecords, tc.wantUpserted) {
				t.Errorf("upserted %+v, want %+v", mock.upsertedRecords, tc.wantUpserted)
			}
		})
	}
}

// TestHTTPRouteReconciler_Deletion releases what a deleted route manages; it is also the C1 regression: the "no hostname matches" early return used to sit above the deletion branch, so such a route could never leave Terminating.
func TestHTTPRouteReconciler_Deletion(t *testing.T) {
	cases := []struct {
		name        string
		hostnames   []string
		annotation  string
		wantDeleted []string
		wantAdopted []string // removals allowed to take legacy records
	}{
		{
			// the spec covers routes from before the annotation existed, and without it nothing proves a legacy record is ours
			name:        "spec hostnames without annotation",
			hostnames:   []string{"app.my-domain1.com", "api.my-domain2.it"},
			wantDeleted: []string{"app.my-domain1.com", "api.my-domain2.it"},
		},
		{
			// the annotation records what we created, so it must drive cleanup even when the live spec matches nothing
			name:        "owned hostname recovered from annotation",
			hostnames:   []string{"gone.example.net"},
			annotation:  `["app.my-domain1.com"]`,
			wantDeleted: []string{"app.my-domain1.com"},
			wantAdopted: []string{"app.my-domain1.com"},
		},
		{
			name:        "no hostnames in spec",
			annotation:  `["app.my-domain1.com", "api.my-domain2.it"]`,
			wantDeleted: []string{"app.my-domain1.com", "api.my-domain2.it"},
			wantAdopted: []string{"app.my-domain1.com", "api.my-domain2.it"},
		},
		{
			// finalizer present but nothing ever recorded as owned: nothing to delete, but the finalizer must still come off
			name:      "finalizer with no annotation",
			hostnames: []string{"gone.example.net"},
		},
		{
			// a hostname the domain map does not cover is released like any other, but without adopting a legacy record: only rows carrying the provider's marker can go, and route editors cannot write it
			name:        "annotation names a hostname outside the domain map",
			hostnames:   []string{"app.my-domain1.com"},
			annotation:  `["app.my-domain1.com", "vpn.corp.example"]`,
			wantDeleted: []string{"app.my-domain1.com", "vpn.corp.example"},
			wantAdopted: []string{"app.my-domain1.com"},
		},
		{
			// a hostname that left the spec is invisible to the spec path, so only the annotation can find it
			name:        "spec and annotation disagree",
			hostnames:   []string{"app.my-domain1.com", "gone.example.net"},
			annotation:  `["app.my-domain1.com", "api.my-domain2.it"]`,
			wantDeleted: []string{"app.my-domain1.com", "api.my-domain2.it"},
			wantAdopted: []string{"app.my-domain1.com", "api.my-domain2.it"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := metav1.Now()
			route := managedRoute("route", tc.annotation, tc.hostnames...)
			route.DeletionTimestamp = &now
			c := newFakeRouteClient(t, route)
			mock := &mockDNSProvider{}

			if res := reconcile(t, newTestReconciler(t, mock, c), "route"); res.RequeueAfter != 0 {
				t.Errorf("RequeueAfter = %s, want none", res.RequeueAfter)
			}
			if err := c.Get(context.Background(), routeRequest("route").NamespacedName, &gatewayv1.HTTPRoute{}); !apierrors.IsNotFound(err) {
				t.Fatalf("route still exists (err=%v): it is stuck in Terminating", err)
			}
			if !slices.Equal(mock.deletedHosts, tc.wantDeleted) {
				t.Errorf("deleted %v, want %v", mock.deletedHosts, tc.wantDeleted)
			}
			if !slices.Equal(mock.adoptedRemovals, tc.wantAdopted) {
				t.Errorf("legacy-adopting removals = %v, want %v", mock.adoptedRemovals, tc.wantAdopted)
			}
		})
	}
}

// TestHTTPRouteReconciler_FlushFailureRequeues proves a failed apply is returned rather than swallowed: logging alone leaves the override staged, unpublished, with nothing to retry it.
func TestHTTPRouteReconciler_FlushFailureRequeues(t *testing.T) {
	route := managedRoute("route", "", "app.my-domain1.com")
	mock := &mockDNSProvider{flushErr: errors.New("reconfigure failed")}
	r := newTestReconciler(t, mock, newFakeRouteClient(t, route))

	_, err := r.Reconcile(context.Background(), routeRequest("route"))
	if err == nil || !strings.Contains(err.Error(), "reconfigure failed") {
		t.Fatalf("error = %v, want the flush failure returned so the reconcile requeues", err)
	}
}

// TestHTTPRouteReconciler_CleanupOfUnownedHostnames covers the live path: owned records go when a hostname stops being managed, whether it left the spec or the domain map.
func TestHTTPRouteReconciler_CleanupOfUnownedHostnames(t *testing.T) {
	cases := []struct {
		name        string
		domainMap   map[string]string // testDomainMap when nil
		hostnames   []string
		annotation  string
		wantDeleted []string
		wantAdopted []string // removals allowed to take legacy records
		wantOwned   string
		wantNoWrite bool
	}{
		{
			name:        "hostname removed from spec",
			hostnames:   []string{"app.my-domain1.com"},
			annotation:  `["app.my-domain1.com","api.my-domain2.it"]`,
			wantDeleted: []string{"api.my-domain2.it"},
			wantAdopted: []string{"api.my-domain2.it"},
			wantOwned:   `["app.my-domain1.com"]`,
		},
		{
			// the spec still names api.my-domain2.it but the map no longer resolves it, so the record is released; a legacy record is not adopted outside the map
			name:        "hostname removed from domain map",
			domainMap:   map[string]string{"*.my-domain1.com": "10.0.8.100"},
			hostnames:   []string{"app.my-domain1.com", "api.my-domain2.it"},
			annotation:  `["app.my-domain1.com","api.my-domain2.it"]`,
			wantDeleted: []string{"api.my-domain2.it"},
			wantOwned:   `["app.my-domain1.com"]`,
		},
		{
			// a route editor can write the annotation, so the name is released; but only managed rows can go, and legacy rows are never adopted for a name outside the map
			name:        "annotation names a hostname outside the domain map",
			hostnames:   []string{"app.my-domain1.com"},
			annotation:  `["app.my-domain1.com","vpn.corp.example"]`,
			wantDeleted: []string{"vpn.corp.example"},
			wantOwned:   `["app.my-domain1.com"]`,
		},
		{
			// guard: a reconcile that changes nothing must not write the route; the old filtered-vs-unfiltered comparison forced a write that hid the divergence while the record stayed
			name:        "nothing changed, no annotation write",
			hostnames:   []string{"app.my-domain1.com"},
			annotation:  `["app.my-domain1.com"]`,
			wantOwned:   `["app.my-domain1.com"]`,
			wantNoWrite: true,
		},
		{
			name:       "corrupt annotation is rewritten, not fatal",
			hostnames:  []string{"app.my-domain1.com"},
			annotation: `{not json`,
			wantOwned:  `["app.my-domain1.com"]`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route := managedRoute("route", tc.annotation, tc.hostnames...)
			c := newFakeRouteClient(t, route)
			mock := &mockDNSProvider{}
			r := newTestReconciler(t, mock, c)
			r.Upsert = true
			if tc.domainMap != nil {
				r.DomainMap = newTestDomainMap(t, tc.domainMap)
			}

			before := getRoute(t, c, "route").ResourceVersion
			reconcile(t, r, "route")

			if !slices.Equal(mock.deletedHosts, tc.wantDeleted) {
				t.Errorf("deleted %v, want %v", mock.deletedHosts, tc.wantDeleted)
			}
			if !slices.Equal(mock.adoptedRemovals, tc.wantAdopted) {
				t.Errorf("legacy-adopting removals = %v, want %v", mock.adoptedRemovals, tc.wantAdopted)
			}
			updated := getRoute(t, c, "route")
			if got := updated.Annotations[managedHostnamesAnnotation]; got != tc.wantOwned {
				t.Errorf("annotation = %q, want %q", got, tc.wantOwned)
			}
			if tc.wantNoWrite && updated.ResourceVersion != before {
				t.Errorf("reconcile wrote the route (%s -> %s) despite nothing having changed", before, updated.ResourceVersion)
			}
		})
	}
}

// TestHTTPRouteReconciler_ConflictIsNotOwned: a name held by a record the controller does not manage stays out of the annotation, so the route can never adopt or delete it.
func TestHTTPRouteReconciler_ConflictIsNotOwned(t *testing.T) {
	mock := &mockDNSProvider{conflictHosts: map[string]bool{"app.my-domain1.com": true}}
	r := reconcileOnce(t, mock, "route", managedRoute("route", "", "app.my-domain1.com", "web.my-domain1.com"))

	updated := getRoute(t, r, "route")
	if got := updated.Annotations[managedHostnamesAnnotation]; got != `["web.my-domain1.com"]` {
		t.Errorf("annotation = %s, want only the hostname without a conflict", got)
	}
}

// TestHTTPRouteReconciler_WildcardHostnameIsIgnored: managing "*.my-domain1.com" would put a wildcard record on the appliance, so it is skipped while the route's exact names still get theirs.
func TestHTTPRouteReconciler_WildcardHostnameIsIgnored(t *testing.T) {
	mock := &mockDNSProvider{}
	r := reconcileOnce(t, mock, "route", managedRoute("route", "", "*.my-domain1.com", "app.my-domain1.com"))

	if len(mock.ensureOpts) != 1 {
		t.Fatalf("ensured %v, want only app.my-domain1.com", mock.ensureOpts)
	}
	if _, ok := mock.ensureOpts["*.my-domain1.com"]; ok {
		t.Error("the wildcard hostname was ensured")
	}
	if _, ok := mock.ensureOpts["app.my-domain1.com"]; !ok {
		t.Error("app.my-domain1.com was not ensured")
	}

	updated := getRoute(t, r, "route")
	if got := updated.Annotations[managedHostnamesAnnotation]; got != `["app.my-domain1.com"]` {
		t.Errorf("annotation = %s, want only the exact hostname", got)
	}
}

// TestHTTPRouteReconciler_AdoptsLegacyOnlyForOwnedNames: a record written before ownership was marked is adopted only for a name the route already recorded owning.
func TestHTTPRouteReconciler_AdoptsLegacyOnlyForOwnedNames(t *testing.T) {
	mock := &mockDNSProvider{}
	reconcileOnce(t, mock, "route", managedRoute("route", `["app.my-domain1.com"]`, "app.my-domain1.com", "web.my-domain1.com"))

	if !mock.ensureOpts["app.my-domain1.com"].AdoptLegacy {
		t.Error("app.my-domain1.com is recorded as owned, so its legacy record should be adoptable")
	}
	if mock.ensureOpts["web.my-domain1.com"].AdoptLegacy {
		t.Error("web.my-domain1.com is not recorded as owned, so a legacy record must not be adopted")
	}
}

// TestHTTPRouteReconciler_SharedHostnameIsKept is audit B1: two routes own one name, and deleting either must leave the record the other still needs.
func TestHTTPRouteReconciler_SharedHostnameIsKept(t *testing.T) {
	now := metav1.Now()
	deleting := managedRoute("blue", `["app.my-domain1.com"]`, "app.my-domain1.com")
	deleting.DeletionTimestamp = &now
	other := managedRoute("green", `["app.my-domain1.com"]`, "app.my-domain1.com")

	mock := &mockDNSProvider{}
	reconcileOnce(t, mock, "blue", deleting, other)

	if len(mock.deletedHosts) != 0 {
		t.Errorf("deleted %v while green still claims it", mock.deletedHosts)
	}
}

// TestHTTPRouteReconciler_CleanupRetriesWithinGracePeriod is audit B9 and D10: a failing cleanup holds the finalizer until the grace period runs out, then the route is released anyway.
func TestHTTPRouteReconciler_CleanupRetriesWithinGracePeriod(t *testing.T) {
	cases := []struct {
		name          string
		mock          *mockDNSProvider
		deletedAgo    time.Duration
		wantFinalizer bool
		wantAbandoned float64
	}{
		{"delete fails within grace", &mockDNSProvider{removeErr: errors.New("appliance down")}, time.Minute, true, 0},
		{"publish fails within grace", &mockDNSProvider{flushErr: errors.New("reconfigure failed")}, time.Minute, true, 0},
		{"delete fails after grace", &mockDNSProvider{removeErr: errors.New("appliance down")}, time.Hour, false, 1},
		{"cleanup succeeds", &mockDNSProvider{}, time.Minute, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deleted := metav1.NewTime(time.Now().Add(-tc.deletedAgo))
			route := managedRoute("route", `["app.my-domain1.com"]`, "app.my-domain1.com")
			route.DeletionTimestamp = &deleted

			c := newFakeRouteClient(t, route)
			r := newTestReconciler(t, tc.mock, c)
			r.CleanupGracePeriod = 5 * time.Minute

			abandoned := testutil.ToFloat64(metrics.CleanupsAbandoned)
			metrics.SetManagedRecords("default", "route", map[string]string{"app.my-domain1.com": "10.0.8.100"})
			res := reconcile(t, r, "route")
			// DeleteLabelValues reports whether the series was still there
			if exported := metrics.ManagedRecords.DeleteLabelValues("default", "route", "app.my-domain1.com", "10.0.8.100"); exported != tc.wantFinalizer {
				t.Errorf("managed record still exported = %v, want %v: a released route drops its series", exported, tc.wantFinalizer)
			}
			if d := testutil.ToFloat64(metrics.CleanupsAbandoned) - abandoned; d != tc.wantAbandoned {
				t.Errorf("abandoned cleanups rose by %v, want %v", d, tc.wantAbandoned)
			}
			var got gatewayv1.HTTPRoute
			held := c.Get(context.Background(), routeRequest("route").NamespacedName, &got) == nil && slices.Contains(got.Finalizers, finalizerName)
			if held != tc.wantFinalizer {
				t.Fatalf("finalizer held = %v, want %v", held, tc.wantFinalizer)
			}
			if held && (res.RequeueAfter <= 0 || res.RequeueAfter > cleanupRetryInterval) {
				t.Errorf("RequeueAfter = %s, want a retry within %s", res.RequeueAfter, cleanupRetryInterval)
			}
		})
	}
}

// TestHTTPRouteReconciler_PublishHeldBackIsRequeued is audit M4: holding a publish back to coalesce changes is not a failure; the route comes back when it is due and still records what it manages.
func TestHTTPRouteReconciler_PublishHeldBackIsRequeued(t *testing.T) {
	route := managedRoute("route", "", "app.my-domain1.com")
	c := newFakeRouteClient(t, route)
	mock := &mockDNSProvider{deferPublish: 30 * time.Second}
	r := newTestReconciler(t, mock, c)
	r.Upsert = true

	if res := reconcile(t, r, "route"); res.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %s, want the route back when the publish is due", res.RequeueAfter)
	}

	got := getRoute(t, c, "route")
	if got.Annotations[managedHostnamesAnnotation] != `["app.my-domain1.com"]` {
		t.Errorf("annotation = %q, the route must still record what it manages", got.Annotations[managedHostnamesAnnotation])
	}
}

// TestHTTPRouteReconciler_DeletionWaitsForAHeldBackPublish: a deletion is not published until the provider lets it out, so the finalizer stays until it actually goes.
func TestHTTPRouteReconciler_DeletionWaitsForAHeldBackPublish(t *testing.T) {
	deleted := metav1.NewTime(time.Now())
	route := managedRoute("route", `["app.my-domain1.com"]`, "app.my-domain1.com")
	route.DeletionTimestamp = &deleted

	c := newFakeRouteClient(t, route)
	mock := &mockDNSProvider{deferPublish: 30 * time.Second}
	r := newTestReconciler(t, mock, c)
	r.CleanupGracePeriod = 5 * time.Minute

	if res := reconcile(t, r, "route"); res.RequeueAfter <= 0 || res.RequeueAfter > 30*time.Second {
		t.Errorf("RequeueAfter = %s, want the route back when the publish is due", res.RequeueAfter)
	}
	if !slices.Contains(getRoute(t, c, "route").Finalizers, finalizerName) {
		t.Fatal("the route was released while its deletion is still unpublished")
	}

	// Once the hold is over, the deletion goes out and the route is released.
	mock.deferPublish = 0
	reconcile(t, r, "route")
	if err := c.Get(context.Background(), routeRequest("route").NamespacedName, &gatewayv1.HTTPRoute{}); !apierrors.IsNotFound(err) {
		t.Errorf("route still exists (err=%v), want it released once the deletion is published", err)
	}
}

// TestHTTPRouteReconciler_RequireAccepted is audit S3: off (the default) a route gets DNS whatever its status; on, only once a parent accepted it.
func TestHTTPRouteReconciler_RequireAccepted(t *testing.T) {
	acceptedStatus := []gatewayv1.RouteParentStatus{{
		ParentRef:      gatewayv1.ParentReference{Name: "gateway"},
		ControllerName: "example.com/gateway-controller",
		Conditions: []metav1.Condition{{
			Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue, Reason: "Accepted",
		}},
	}}
	cases := []struct {
		name            string
		requireAccepted bool
		accepted        bool
		wantEnsured     bool
	}{
		{"off, not accepted", false, false, true},
		{"on, not accepted", true, false, false},
		{"on, accepted", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route := managedRoute("route", "", "app.my-domain1.com")
			if tc.accepted {
				route.Status.Parents = acceptedStatus
			}
			mock := &mockDNSProvider{}
			r := newTestReconciler(t, mock, newFakeRouteClient(t, route))
			r.Upsert = true
			r.RequireAccepted = tc.requireAccepted
			reconcile(t, r, "route")
			if ensured := len(mock.upsertedRecords) > 0; ensured != tc.wantEnsured {
				t.Errorf("ensured a record = %v, want %v", ensured, tc.wantEnsured)
			}
		})
	}
}

// TestHTTPRouteReconciler_OneReconcilePublishes is audit D7: the reconcile that adds the finalizer also creates the records, without waiting for a second event.
func TestHTTPRouteReconciler_OneReconcilePublishes(t *testing.T) {
	route := managedRoute("route", "", "app.my-domain1.com")
	route.Finalizers = nil

	mock := &mockDNSProvider{}
	r := reconcileOnce(t, mock, "route", route)

	if len(mock.upsertedRecords) != 1 {
		t.Fatalf("ensured %d records in the first reconcile, want 1", len(mock.upsertedRecords))
	}
	got := getRoute(t, r, "route")
	if !slices.Contains(got.Finalizers, finalizerName) {
		t.Error("finalizer not added")
	}
	if got.Annotations[managedHostnamesAnnotation] != `["app.my-domain1.com"]` {
		t.Errorf("annotation = %q", got.Annotations[managedHostnamesAnnotation])
	}
}

// TestHTTPRouteReconciler_CannotReleaseAnotherRoutesRecord is audit H1: the ownership marker named no route, so writing a hostname into the annotation let one route delete another route's record.
func TestHTTPRouteReconciler_CannotReleaseAnotherRoutesRecord(t *testing.T) {
	// the backend holds a record route blue wrote; blue is gone from the cluster, and green's editor named it in green's annotation
	mock := &mockDNSProvider{
		known:  map[string]string{"app.my-domain1.com": "10.0.8.100"},
		owners: map[string][]string{"app.my-domain1.com": {"httproute/default/blue"}},
	}
	route := managedRoute("green", `["app.my-domain1.com"]`, "web.my-domain1.com")
	c := newFakeRouteClient(t, route)
	r := newTestReconciler(t, mock, c)
	r.Upsert = true
	reconcile(t, r, "green")

	if len(mock.deletedHosts) != 0 {
		t.Errorf("green released %v, a record route blue wrote", mock.deletedHosts)
	}
	if _, ok := mock.known["app.my-domain1.com"]; !ok {
		t.Error("the record blue wrote is gone")
	}

	got := getRoute(t, c, "green")
	if got.Annotations[managedHostnamesAnnotation] != `["web.my-domain1.com"]` {
		t.Errorf("annotation = %q, want only what green manages", got.Annotations[managedHostnamesAnnotation])
	}
}

// TestHTTPRouteReconciler_WantsUpdate is audit D8: which updates trigger a reconcile.
func TestHTTPRouteReconciler_WantsUpdate(t *testing.T) {
	base := managedRoute("route", "", "app.my-domain1.com")
	base.Generation = 1
	base.ResourceVersion = "1"
	with := func(mutate func(*gatewayv1.HTTPRoute)) *gatewayv1.HTTPRoute {
		r := base.DeepCopy()
		r.ResourceVersion = "2" // every write to the API server bumps it
		mutate(r)
		return r
	}
	acceptedParent := []gatewayv1.RouteParentStatus{{Conditions: []metav1.Condition{{
		Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue,
	}}}}

	cases := []struct {
		name            string
		updated         *gatewayv1.HTTPRoute
		requireAccepted bool
		want            bool
	}{
		{"spec change", with(func(r *gatewayv1.HTTPRoute) { r.Generation = 2 }), false, true},
		{"our finalizer removed", with(func(r *gatewayv1.HTTPRoute) { r.Finalizers = nil }), false, true},
		{"another finalizer swapped in", with(func(r *gatewayv1.HTTPRoute) {
			r.Finalizers = []string{finalizerName, "other.io/finalizer"}
		}), false, false},
		{"annotation only", with(func(r *gatewayv1.HTTPRoute) {
			r.Annotations = map[string]string{managedHostnamesAnnotation: `["x"]`}
		}), false, false},
		{"acceptance flips, not required", with(func(r *gatewayv1.HTTPRoute) { r.Status.Parents = acceptedParent }), false, false},
		{"acceptance flips, required", with(func(r *gatewayv1.HTTPRoute) { r.Status.Parents = acceptedParent }), true, true},
		// the informer re-delivers the object unchanged on its resync period; only a reconcile can notice the appliance no longer matches
		{"informer resync", base.DeepCopy(), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &HTTPRouteReconciler{RequireAccepted: tc.requireAccepted}
			if got := r.wantsUpdate(event.UpdateEvent{ObjectOld: base, ObjectNew: tc.updated}); got != tc.want {
				t.Errorf("wantsUpdate = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHTTPRouteReconciler_EventsAndMetrics is audit D6: outcomes show up as Events on the route and in the Prometheus counters.
func TestHTTPRouteReconciler_EventsAndMetrics(t *testing.T) {
	createdBefore := testutil.ToFloat64(metrics.RecordResults.WithLabelValues("created"))
	conflictBefore := testutil.ToFloat64(metrics.RecordResults.WithLabelValues("conflict"))

	// its own name, so no other test's count for the same route is replaced
	route := managedRoute("metrics", "", "app.my-domain1.com", "web.my-domain1.com")
	c := newFakeRouteClient(t, route)
	recorder := events.NewFakeRecorder(10)
	mock := &mockDNSProvider{conflictHosts: map[string]bool{"web.my-domain1.com": true}}
	r := newTestReconciler(t, mock, c)
	r.Recorder = recorder
	reconcile(t, r, "metrics")
	// the conflicting name is not managed, so only app gets a series
	if v := testutil.ToFloat64(metrics.ManagedRecords.WithLabelValues("default", "metrics", "app.my-domain1.com", "10.0.8.100")); v != 1 {
		t.Errorf("managed record series for app = %v, want 1", v)
	}

	close(recorder.Events)
	var got []string
	for e := range recorder.Events {
		got = append(got, e)
	}
	want := []string{
		"Normal DNSRecordCreated app.my-domain1.com -> 10.0.8.100",
		"Warning DNSConflict web.my-domain1.com already has a DNS record this controller does not manage; it is left alone",
	}
	if !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}

	if d := testutil.ToFloat64(metrics.RecordResults.WithLabelValues("created")) - createdBefore; d != 1 {
		t.Errorf("created counter rose by %v, want 1", d)
	}
	if d := testutil.ToFloat64(metrics.RecordResults.WithLabelValues("conflict")) - conflictBefore; d != 1 {
		t.Errorf("conflict counter rose by %v, want 1", d)
	}
}
