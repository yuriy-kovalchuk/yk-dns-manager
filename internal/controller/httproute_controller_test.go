package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/config"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

// mockDNSProvider records DNS operations for test assertions. Ensure with
// overwrite on is recorded as an upsert, with overwrite off as a create, which
// is skipped for hostnames in existingHosts. Hostnames in conflictHosts hold a
// record the controller does not manage.
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
	removeErr       error
	known           map[string]string // hostname → value of records this mock created
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
	if ok && value == record.Value {
		return dns.Unchanged, nil
	}
	if opts.Overwrite {
		b.m.known[record.Hostname] = record.Value
		b.m.upsertedRecords = append(b.m.upsertedRecords, record)
		return dns.Updated, nil
	}
	if ok || b.m.existingHosts[record.Hostname] {
		return dns.Kept, nil
	}
	b.m.known[record.Hostname] = record.Value
	b.m.createdRecords = append(b.m.createdRecords, record)
	return dns.Created, nil
}

func (b mockBatch) Remove(_ context.Context, hostname, _ string, adoptLegacy bool) error {
	b.m.mu.Lock()
	defer b.m.mu.Unlock()
	if b.m.removeErr != nil {
		return b.m.removeErr
	}
	b.m.deletedHosts = append(b.m.deletedHosts, hostname)
	delete(b.m.known, hostname)
	if adoptLegacy {
		b.m.adoptedRemovals = append(b.m.adoptedRemovals, hostname)
	}
	return nil
}

func (b mockBatch) Commit(_ context.Context) error {
	b.m.mu.Lock()
	defer b.m.mu.Unlock()
	b.m.flushes++
	return b.m.flushErr
}

func newTestDomainMap(t *testing.T) *config.DomainMap {
	t.Helper()
	return newTestDomainMapWith(t, "\"*.my-domain1.com\": 10.0.8.100\n\"*.my-domain2.it\": 10.0.9.50\n")
}

func newTestDomainMapWith(t *testing.T, content string) *config.DomainMap {
	t.Helper()
	path := filepath.Join(t.TempDir(), "domain-map.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	dm, err := config.LoadDomainMap(path)
	if err != nil {
		t.Fatal(err)
	}
	return dm
}

func TestHTTPRouteReconciler_Reconcile(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to install gateway-api scheme: %v", err)
	}

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"app.my-domain1.com"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&gatewayv1.HTTPRoute{}, hostnameIndex, indexHostnames).
		WithObjects(route).
		Build()

	mock := &mockDNSProvider{}
	reconciler := &HTTPRouteReconciler{
		Client:    fakeClient,
		APIReader: fakeClient,
		Log:       zap.New(zap.UseDevMode(true)),
		DomainMap: newTestDomainMap(t),
		DNS:       mock,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-route",
			Namespace: "default",
		},
	}

	// First reconcile adds the finalizer
	result, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Error("expected no requeue")
	}

	// Second reconcile processes the hostnames
	result, err = reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error on second reconcile: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Error("expected no requeue")
	}

	if len(mock.createdRecords) != 1 {
		t.Fatalf("expected 1 created record, got %d", len(mock.createdRecords))
	}
	rec := mock.createdRecords[0]
	if rec.Hostname != "app.my-domain1.com" {
		t.Errorf("expected hostname 'app.my-domain1.com', got %q", rec.Hostname)
	}
	if rec.Value != "10.0.8.100" {
		t.Errorf("expected value '10.0.8.100', got %q", rec.Value)
	}
	if rec.Type != "A" {
		t.Errorf("expected type 'A', got %q", rec.Type)
	}
}

func TestHTTPRouteReconciler_ReconcileUnknownDomain(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to install gateway-api scheme: %v", err)
	}

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unknown-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"app.unknown.com"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&gatewayv1.HTTPRoute{}, hostnameIndex, indexHostnames).
		WithObjects(route).
		Build()

	mock := &mockDNSProvider{}
	reconciler := &HTTPRouteReconciler{
		Client:    fakeClient,
		APIReader: fakeClient,
		Log:       zap.New(zap.UseDevMode(true)),
		DomainMap: newTestDomainMap(t),
		DNS:       mock,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "unknown-route",
			Namespace: "default",
		},
	}

	// First reconcile adds the finalizer
	result, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Second reconcile processes hostnames — no match expected
	result, err = reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Error("expected no requeue")
	}

	if len(mock.createdRecords) != 0 {
		t.Errorf("expected 0 created records for unknown domain, got %d", len(mock.createdRecords))
	}
}

func TestHTTPRouteReconciler_UpsertEnabled(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to install gateway-api scheme: %v", err)
	}

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "upsert-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"app.my-domain1.com"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&gatewayv1.HTTPRoute{}, hostnameIndex, indexHostnames).
		WithObjects(route).
		Build()

	mock := &mockDNSProvider{}
	reconciler := &HTTPRouteReconciler{
		Client:    fakeClient,
		APIReader: fakeClient,
		Log:       zap.New(zap.UseDevMode(true)),
		DomainMap: newTestDomainMap(t),
		DNS:       mock,
		Upsert:    true,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "upsert-route",
			Namespace: "default",
		},
	}

	// First reconcile adds the finalizer
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Second reconcile calls Upsert
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.upsertedRecords) != 1 {
		t.Fatalf("expected 1 upserted record, got %d", len(mock.upsertedRecords))
	}
	if len(mock.createdRecords) != 0 {
		t.Errorf("expected 0 created records when upsert is enabled, got %d", len(mock.createdRecords))
	}
}

func TestHTTPRouteReconciler_CreateSkipsExisting(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to install gateway-api scheme: %v", err)
	}

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "skip-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"app.my-domain1.com"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&gatewayv1.HTTPRoute{}, hostnameIndex, indexHostnames).
		WithObjects(route).
		Build()

	mock := &mockDNSProvider{
		existingHosts: map[string]bool{"app.my-domain1.com": true},
	}
	reconciler := &HTTPRouteReconciler{
		Client:    fakeClient,
		APIReader: fakeClient,
		Log:       zap.New(zap.UseDevMode(true)),
		DomainMap: newTestDomainMap(t),
		DNS:       mock,
		Upsert:    false,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "skip-route",
			Namespace: "default",
		},
	}

	// First reconcile adds the finalizer
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Second reconcile skips existing record
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.createdRecords) != 0 {
		t.Errorf("expected 0 created records for existing host, got %d", len(mock.createdRecords))
	}
}

func TestHTTPRouteReconciler_Deletion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to install gateway-api scheme: %v", err)
	}

	now := metav1.Now()
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "delete-route",
			Namespace:         "default",
			Finalizers:        []string{finalizerName},
			DeletionTimestamp: &now,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"app.my-domain1.com", "api.my-domain2.it"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&gatewayv1.HTTPRoute{}, hostnameIndex, indexHostnames).
		WithObjects(route).
		Build()

	mock := &mockDNSProvider{}
	reconciler := &HTTPRouteReconciler{
		Client:    fakeClient,
		APIReader: fakeClient,
		Log:       zap.New(zap.UseDevMode(true)),
		DomainMap: newTestDomainMap(t),
		DNS:       mock,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "delete-route",
			Namespace: "default",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Error("expected no requeue")
	}

	if len(mock.deletedHosts) != 2 {
		t.Fatalf("expected 2 deleted hosts, got %d", len(mock.deletedHosts))
	}
	if mock.deletedHosts[0] != "app.my-domain1.com" {
		t.Errorf("expected first deleted host 'app.my-domain1.com', got %q", mock.deletedHosts[0])
	}
	if mock.deletedHosts[1] != "api.my-domain2.it" {
		t.Errorf("expected second deleted host 'api.my-domain2.it', got %q", mock.deletedHosts[1])
	}
}

// TestHTTPRouteReconciler_DeletionWithoutMatchingHostname is the C1 regression
// test. The early return for "no hostname matches the domain map" used to sit
// above the deletion branch, so a route carrying our finalizer whose hostnames
// no longer match the map could never leave Terminating.
func TestHTTPRouteReconciler_DeletionWithoutMatchingHostname(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to install gateway-api scheme: %v", err)
	}

	cases := []struct {
		name        string
		hostnames   []string
		annotation  string
		wantDeleted []string
		wantAdopted []string // removals allowed to take legacy records
	}{
		{
			// The annotation is the record of what we created; it must drive
			// cleanup even when the live spec matches nothing.
			name:        "owned hostname recovered from annotation",
			hostnames:   []string{"gone.example.net"},
			annotation:  `["app.my-domain1.com"]`,
			wantDeleted: []string{"app.my-domain1.com"},
			wantAdopted: []string{"app.my-domain1.com"},
		},
		{
			// No spec hostnames at all: the route must still become deletable.
			name:        "no hostnames in spec",
			hostnames:   nil,
			annotation:  `["app.my-domain1.com", "api.my-domain2.it"]`,
			wantDeleted: []string{"app.my-domain1.com", "api.my-domain2.it"},
			wantAdopted: []string{"app.my-domain1.com", "api.my-domain2.it"},
		},
		{
			// Finalizer present but nothing ever recorded as owned: nothing to
			// delete, but the finalizer must still come off.
			name:        "finalizer with no annotation",
			hostnames:   []string{"gone.example.net"},
			annotation:  "",
			wantDeleted: nil,
		},
		{
			// A hostname written into the annotation that the domain map does
			// not cover is released like any other, but without adopting a
			// legacy record: the provider only deletes rows carrying its
			// ownership marker, which a route editor cannot write.
			name:        "annotation names a hostname outside the domain map",
			hostnames:   []string{"app.my-domain1.com"},
			annotation:  `["app.my-domain1.com", "vpn.corp.example"]`,
			wantDeleted: []string{"app.my-domain1.com", "vpn.corp.example"},
			wantAdopted: []string{"app.my-domain1.com"},
		},
		{
			// A hostname that left the spec is invisible to the spec path, so
			// only the annotation can find it.
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
			route := &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "orphan-route",
					Namespace:         "default",
					Finalizers:        []string{finalizerName},
					DeletionTimestamp: &now,
				},
				Spec: gatewayv1.HTTPRouteSpec{},
			}
			for _, h := range tc.hostnames {
				route.Spec.Hostnames = append(route.Spec.Hostnames, gatewayv1.Hostname(h))
			}
			if tc.annotation != "" {
				route.Annotations = map[string]string{managedHostnamesAnnotation: tc.annotation}
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithIndex(&gatewayv1.HTTPRoute{}, hostnameIndex, indexHostnames).
				WithObjects(route).
				Build()

			mock := &mockDNSProvider{}
			reconciler := &HTTPRouteReconciler{
				Client:    fakeClient,
				APIReader: fakeClient,
				Log:       zap.New(zap.UseDevMode(true)),
				DomainMap: newTestDomainMap(t),
				DNS:       mock,
			}

			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "orphan-route", Namespace: "default"}}
			if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var updated gatewayv1.HTTPRoute
			key := types.NamespacedName{Name: "orphan-route", Namespace: "default"}
			if err := fakeClient.Get(context.Background(), key, &updated); err == nil {
				for _, f := range updated.Finalizers {
					if f == finalizerName {
						t.Fatalf("finalizer %q still present: route is stuck in Terminating", finalizerName)
					}
				}
			}

			if len(mock.deletedHosts) != len(tc.wantDeleted) {
				t.Fatalf("expected %d deleted hosts %v, got %d %v", len(tc.wantDeleted), tc.wantDeleted, len(mock.deletedHosts), mock.deletedHosts)
			}
			for i, want := range tc.wantDeleted {
				if mock.deletedHosts[i] != want {
					t.Errorf("deleted[%d] = %q, want %q", i, mock.deletedHosts[i], want)
				}
			}
			if !slices.Equal(mock.adoptedRemovals, tc.wantAdopted) {
				t.Errorf("legacy-adopting removals = %v, want %v", mock.adoptedRemovals, tc.wantAdopted)
			}
		})
	}
}

// TestHTTPRouteReconciler_FlushFailureRequeues proves a failed apply is returned
// rather than swallowed. Logging it alone would leave the override staged in
// OPNsense's config, never published, with nothing to retry it.
func TestHTTPRouteReconciler_FlushFailureRequeues(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to install gateway-api scheme: %v", err)
	}

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "flush-route",
			Namespace:  "default",
			Finalizers: []string{finalizerName},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"app.my-domain1.com"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&gatewayv1.HTTPRoute{}, hostnameIndex, indexHostnames).
		WithObjects(route).
		Build()

	mock := &mockDNSProvider{flushErr: errors.New("reconfigure failed")}
	reconciler := &HTTPRouteReconciler{
		Client:    fakeClient,
		APIReader: fakeClient,
		Log:       zap.New(zap.UseDevMode(true)),
		DomainMap: newTestDomainMap(t),
		DNS:       mock,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "flush-route", Namespace: "default"},
	}
	_, err := reconciler.Reconcile(context.Background(), req)
	if err == nil {
		t.Fatal("expected the flush failure to be returned so the reconcile requeues")
	}
	if !strings.Contains(err.Error(), "reconfigure failed") {
		t.Errorf("error = %v, want it to carry the flush failure", err)
	}
}

// TestHTTPRouteReconciler_CleanupOfUnownedHostnames covers the live (non-deleting)
// path: records the controller owns must be removed when the hostname stops being
// managed, whether it left the spec or the domain map.
func TestHTTPRouteReconciler_CleanupOfUnownedHostnames(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to install gateway-api scheme: %v", err)
	}

	cases := []struct {
		name        string
		domainMap   string
		hostnames   []string
		annotation  string
		wantDeleted []string
		wantAdopted []string // removals allowed to take legacy records
		wantOwned   string
		wantNoWrite bool
	}{
		{
			name:        "hostname removed from spec",
			domainMap:   "\"*.my-domain1.com\": 10.0.8.100\n\"*.my-domain2.it\": 10.0.9.50\n",
			hostnames:   []string{"app.my-domain1.com"},
			annotation:  `["app.my-domain1.com","api.my-domain2.it"]`,
			wantDeleted: []string{"api.my-domain2.it"},
			wantAdopted: []string{"api.my-domain2.it"},
			wantOwned:   `["app.my-domain1.com"]`,
		},
		{
			name: "hostname removed from domain map",
			// The spec still names api.my-domain2.it, but the map no longer
			// resolves it, so the managed record is released. A legacy record
			// is not adopted for a name outside the map.
			domainMap:   "\"*.my-domain1.com\": 10.0.8.100\n",
			hostnames:   []string{"app.my-domain1.com", "api.my-domain2.it"},
			annotation:  `["app.my-domain1.com","api.my-domain2.it"]`,
			wantDeleted: []string{"api.my-domain2.it"},
			wantOwned:   `["app.my-domain1.com"]`,
		},
		{
			// Anyone who can edit the route can write the annotation. The name
			// is released, but only managed rows can go, and a legacy record is
			// never adopted for a name the domain map does not cover.
			name:        "annotation names a hostname outside the domain map",
			domainMap:   "\"*.my-domain1.com\": 10.0.8.100\n\"*.my-domain2.it\": 10.0.9.50\n",
			hostnames:   []string{"app.my-domain1.com"},
			annotation:  `["app.my-domain1.com","vpn.corp.example"]`,
			wantDeleted: []string{"vpn.corp.example"},
			wantOwned:   `["app.my-domain1.com"]`,
		},
		{
			// Guard: a reconcile that changes nothing must not write the route.
			// The old comparison mixed an unfiltered list with a filtered one, so
			// an annotation holding a hostname that dropped out of the map forced
			// one extra write — and that write silently rewrote the annotation to
			// hide the divergence while the record itself was never deleted.
			name:        "nothing changed, no annotation write",
			domainMap:   "\"*.my-domain1.com\": 10.0.8.100\n\"*.my-domain2.it\": 10.0.9.50\n",
			hostnames:   []string{"app.my-domain1.com"},
			annotation:  `["app.my-domain1.com"]`,
			wantDeleted: nil,
			wantOwned:   `["app.my-domain1.com"]`,
			wantNoWrite: true,
		},
		{
			name:        "corrupt annotation is rewritten, not fatal",
			domainMap:   "\"*.my-domain1.com\": 10.0.8.100\n\"*.my-domain2.it\": 10.0.9.50\n",
			hostnames:   []string{"app.my-domain1.com"},
			annotation:  `{not json`,
			wantDeleted: nil,
			wantOwned:   `["app.my-domain1.com"]`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route := &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "live-route",
					Namespace:  "default",
					Finalizers: []string{finalizerName},
					Annotations: map[string]string{
						managedHostnamesAnnotation: tc.annotation,
					},
				},
				Spec: gatewayv1.HTTPRouteSpec{},
			}
			for _, h := range tc.hostnames {
				route.Spec.Hostnames = append(route.Spec.Hostnames, gatewayv1.Hostname(h))
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithIndex(&gatewayv1.HTTPRoute{}, hostnameIndex, indexHostnames).
				WithObjects(route).
				Build()

			mock := &mockDNSProvider{}
			reconciler := &HTTPRouteReconciler{
				Client:    fakeClient,
				APIReader: fakeClient,
				Log:       zap.New(zap.UseDevMode(true)),
				DomainMap: newTestDomainMapWith(t, tc.domainMap),
				DNS:       mock,
				Upsert:    true,
			}

			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "live-route", Namespace: "default"}}
			before := route.ResourceVersion
			if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(mock.deletedHosts) != len(tc.wantDeleted) {
				t.Fatalf("expected %d deleted hosts %v, got %d %v", len(tc.wantDeleted), tc.wantDeleted, len(mock.deletedHosts), mock.deletedHosts)
			}
			for i, want := range tc.wantDeleted {
				if mock.deletedHosts[i] != want {
					t.Errorf("deleted[%d] = %q, want %q", i, mock.deletedHosts[i], want)
				}
			}
			if !slices.Equal(mock.adoptedRemovals, tc.wantAdopted) {
				t.Errorf("legacy-adopting removals = %v, want %v", mock.adoptedRemovals, tc.wantAdopted)
			}

			var updated gatewayv1.HTTPRoute
			key := types.NamespacedName{Name: "live-route", Namespace: "default"}
			if err := fakeClient.Get(context.Background(), key, &updated); err != nil {
				t.Fatalf("route should still exist: %v", err)
			}
			if got := updated.Annotations[managedHostnamesAnnotation]; got != tc.wantOwned {
				t.Errorf("annotation = %q, want %q", got, tc.wantOwned)
			}
			if tc.wantNoWrite && updated.ResourceVersion != before {
				t.Errorf("reconcile wrote the route (%s -> %s) despite nothing having changed", before, updated.ResourceVersion)
			}
		})
	}
}

// reconcileOnce runs one reconcile of default/<name> against a fake client
// holding routes, and returns the reconciler, whose client can be inspected.
func reconcileOnce(t *testing.T, mock *mockDNSProvider, name string, routes ...*gatewayv1.HTTPRoute) *HTTPRouteReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to install gateway-api scheme: %v", err)
	}
	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&gatewayv1.HTTPRoute{}, hostnameIndex, indexHostnames)
	for _, r := range routes {
		builder = builder.WithObjects(r)
	}
	c := builder.Build()
	reconciler := &HTTPRouteReconciler{
		Client:    c,
		APIReader: c,
		Log:       zap.New(zap.UseDevMode(true)),
		DomainMap: newTestDomainMap(t),
		DNS:       mock,
		Upsert:    true,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}}
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return reconciler
}

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

// TestHTTPRouteReconciler_ConflictIsNotOwned: a name held by a record the
// controller does not manage stays out of the annotation, so the route can
// never later adopt or delete that record through it.
func TestHTTPRouteReconciler_ConflictIsNotOwned(t *testing.T) {
	mock := &mockDNSProvider{conflictHosts: map[string]bool{"app.my-domain1.com": true}}
	r := reconcileOnce(t, mock, "route", managedRoute("route", "", "app.my-domain1.com", "web.my-domain1.com"))

	var updated gatewayv1.HTTPRoute
	if err := r.Get(context.Background(), types.NamespacedName{Name: "route", Namespace: "default"}, &updated); err != nil {
		t.Fatal(err)
	}
	if got := updated.Annotations[managedHostnamesAnnotation]; got != `["web.my-domain1.com"]` {
		t.Errorf("annotation = %s, want only the hostname without a conflict", got)
	}
}

// TestHTTPRouteReconciler_WildcardHostnameIsIgnored: a route hostname of
// "*.my-domain1.com" matches the domain map's wildcard key, and managing it
// would put a wildcard record on the appliance, so it is skipped and the exact
// names of the same route still get their records.
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

	var updated gatewayv1.HTTPRoute
	if err := r.Get(context.Background(), types.NamespacedName{Name: "route", Namespace: "default"}, &updated); err != nil {
		t.Fatal(err)
	}
	if got := updated.Annotations[managedHostnamesAnnotation]; got != `["app.my-domain1.com"]` {
		t.Errorf("annotation = %s, want only the exact hostname", got)
	}
}

// TestHTTPRouteReconciler_AdoptsLegacyOnlyForOwnedNames: a record written
// before ownership was marked is adopted only for a name the route already
// recorded owning.
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

// TestHTTPRouteReconciler_SharedHostnameIsKept is audit B1: two routes serving
// one name each record it as owned, and deleting either must leave the record
// the other still needs.
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

// TestHTTPRouteReconciler_CleanupRetriesWithinGracePeriod is audit B9 and D10:
// a failing cleanup holds the finalizer and retries until the grace period
// after the deletion request runs out, then releases the route anyway.
func TestHTTPRouteReconciler_CleanupRetriesWithinGracePeriod(t *testing.T) {
	cases := []struct {
		name          string
		mock          *mockDNSProvider
		deletedAgo    time.Duration
		wantFinalizer bool
	}{
		{"delete fails within grace", &mockDNSProvider{removeErr: errors.New("appliance down")}, time.Minute, true},
		{"publish fails within grace", &mockDNSProvider{flushErr: errors.New("reconfigure failed")}, time.Minute, true},
		{"delete fails after grace", &mockDNSProvider{removeErr: errors.New("appliance down")}, time.Hour, false},
		{"cleanup succeeds", &mockDNSProvider{}, time.Minute, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deleted := metav1.NewTime(time.Now().Add(-tc.deletedAgo))
			route := managedRoute("route", `["app.my-domain1.com"]`, "app.my-domain1.com")
			route.DeletionTimestamp = &deleted

			scheme := runtime.NewScheme()
			if err := gatewayv1.Install(scheme); err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithIndex(&gatewayv1.HTTPRoute{}, hostnameIndex, indexHostnames).
				WithObjects(route).Build()
			r := &HTTPRouteReconciler{
				Client: c, APIReader: c, Log: zap.New(zap.UseDevMode(true)),
				DomainMap: newTestDomainMap(t), DNS: tc.mock, CleanupGracePeriod: 5 * time.Minute,
			}

			key := types.NamespacedName{Name: "route", Namespace: "default"}
			res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			var got gatewayv1.HTTPRoute
			held := c.Get(context.Background(), key, &got) == nil && slices.Contains(got.Finalizers, finalizerName)
			if held != tc.wantFinalizer {
				t.Fatalf("finalizer held = %v, want %v", held, tc.wantFinalizer)
			}
			if held && (res.RequeueAfter <= 0 || res.RequeueAfter > cleanupRetryInterval) {
				t.Errorf("RequeueAfter = %s, want a retry within %s", res.RequeueAfter, cleanupRetryInterval)
			}
		})
	}
}

// TestHTTPRouteReconciler_RequireAccepted is audit S3. Off (the default), a
// route gets DNS whatever its status; on, only once a parent accepted it.
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
			scheme := runtime.NewScheme()
			if err := gatewayv1.Install(scheme); err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithIndex(&gatewayv1.HTTPRoute{}, hostnameIndex, indexHostnames).
				WithObjects(route).WithStatusSubresource(route).Build()
			mock := &mockDNSProvider{}
			r := &HTTPRouteReconciler{
				Client: c, APIReader: c, Log: zap.New(zap.UseDevMode(true)),
				DomainMap: newTestDomainMap(t), DNS: mock, Upsert: true, RequireAccepted: tc.requireAccepted,
			}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "route", Namespace: "default"}}
			if _, err := r.Reconcile(context.Background(), req); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if ensured := len(mock.upsertedRecords) > 0; ensured != tc.wantEnsured {
				t.Errorf("ensured a record = %v, want %v", ensured, tc.wantEnsured)
			}
		})
	}
}

// TestHTTPRouteReconciler_OneReconcilePublishes is audit D7: the reconcile
// that adds the finalizer also creates the records, instead of relying on the
// finalizer update coming back as a second event.
func TestHTTPRouteReconciler_OneReconcilePublishes(t *testing.T) {
	route := managedRoute("route", "", "app.my-domain1.com")
	route.Finalizers = nil

	mock := &mockDNSProvider{}
	r := reconcileOnce(t, mock, "route", route)

	if len(mock.upsertedRecords) != 1 {
		t.Fatalf("ensured %d records in the first reconcile, want 1", len(mock.upsertedRecords))
	}
	var got gatewayv1.HTTPRoute
	if err := r.Get(context.Background(), types.NamespacedName{Name: "route", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Finalizers, finalizerName) {
		t.Error("finalizer not added")
	}
	if got.Annotations[managedHostnamesAnnotation] != `["app.my-domain1.com"]` {
		t.Errorf("annotation = %q", got.Annotations[managedHostnamesAnnotation])
	}
}

// TestHTTPRouteReconciler_WantsUpdate is audit D8: which updates trigger a
// reconcile.
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
		// The informer re-delivers the object unchanged on its resync period.
		// Only a reconcile can notice that the appliance no longer matches.
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

// TestHTTPRouteReconciler_EventsAndMetrics is audit D6: outcomes show up as
// Events on the route and in the Prometheus counters.
func TestHTTPRouteReconciler_EventsAndMetrics(t *testing.T) {
	createdBefore := testutil.ToFloat64(recordResults.WithLabelValues("created"))
	conflictBefore := testutil.ToFloat64(recordResults.WithLabelValues("conflict"))

	route := managedRoute("route", "", "app.my-domain1.com", "web.my-domain1.com")
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&gatewayv1.HTTPRoute{}, hostnameIndex, indexHostnames).
		WithObjects(route).Build()
	recorder := events.NewFakeRecorder(10)
	r := &HTTPRouteReconciler{
		Client: c, APIReader: c, Log: zap.New(zap.UseDevMode(true)), Recorder: recorder,
		DomainMap: newTestDomainMap(t), DNS: &mockDNSProvider{conflictHosts: map[string]bool{"web.my-domain1.com": true}},
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "route", Namespace: "default"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
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

	if d := testutil.ToFloat64(recordResults.WithLabelValues("created")) - createdBefore; d != 1 {
		t.Errorf("created counter rose by %v, want 1", d)
	}
	if d := testutil.ToFloat64(recordResults.WithLabelValues("conflict")) - conflictBefore; d != 1 {
		t.Errorf("conflict counter rose by %v, want 1", d)
	}
}
