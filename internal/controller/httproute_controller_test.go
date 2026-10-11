package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/config"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

// mockDNSProvider records DNS operations for test assertions.
type mockDNSProvider struct {
	mu              sync.Mutex
	existingHosts   map[string]bool // hostnames that Exists returns true for
	createdRecords  []dns.Record
	upsertedRecords []dns.Record
	deletedHosts    []string
	flushes         int
	flushErr        error
}

func (m *mockDNSProvider) Exists(_ context.Context, hostname, _ string) (bool, error) {
	if m.existingHosts != nil {
		return m.existingHosts[hostname], nil
	}
	return false, nil
}

func (m *mockDNSProvider) Create(_ context.Context, record dns.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createdRecords = append(m.createdRecords, record)
	return nil
}

func (m *mockDNSProvider) Update(_ context.Context, _ dns.Record) error {
	return nil
}

func (m *mockDNSProvider) Delete(_ context.Context, hostname, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedHosts = append(m.deletedHosts, hostname)
	return nil
}

func (m *mockDNSProvider) Upsert(_ context.Context, record dns.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upsertedRecords = append(m.upsertedRecords, record)
	return nil
}

func (m *mockDNSProvider) HealthCheck(_ context.Context) error {
	return nil
}

func (m *mockDNSProvider) Flush(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushes++
	return m.flushErr
}

func newTestDomainMap(t *testing.T) *config.DomainMap {
	t.Helper()
	return newTestDomainMapWith(t, "my-domain1.com: 10.0.8.100\nmy-domain2.it: 10.0.9.50\n")
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
	}{
		{
			// The annotation is the record of what we created; it must drive
			// cleanup even when the live spec matches nothing.
			name:        "owned hostname recovered from annotation",
			hostnames:   []string{"gone.example.net"},
			annotation:  `["app.my-domain1.com"]`,
			wantDeleted: []string{"app.my-domain1.com"},
		},
		{
			// No spec hostnames at all: the route must still become deletable.
			name:        "no hostnames in spec",
			hostnames:   nil,
			annotation:  `["app.my-domain1.com", "api.my-domain2.it"]`,
			wantDeleted: []string{"app.my-domain1.com", "api.my-domain2.it"},
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
			// not cover is not deleted, but the finalizer still comes off.
			name:        "annotation names a hostname outside the domain map",
			hostnames:   []string{"app.my-domain1.com"},
			annotation:  `["app.my-domain1.com", "vpn.corp.example"]`,
			wantDeleted: []string{"app.my-domain1.com"},
		},
		{
			// A hostname that left the spec is invisible to the spec path, so
			// only the annotation can find it.
			name:        "spec and annotation disagree",
			hostnames:   []string{"app.my-domain1.com", "gone.example.net"},
			annotation:  `["app.my-domain1.com", "api.my-domain2.it"]`,
			wantDeleted: []string{"app.my-domain1.com", "api.my-domain2.it"},
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
		wantOwned   string
		wantNoWrite bool
	}{
		{
			name:        "hostname removed from spec",
			domainMap:   "my-domain1.com: 10.0.8.100\nmy-domain2.it: 10.0.9.50\n",
			hostnames:   []string{"app.my-domain1.com"},
			annotation:  `["app.my-domain1.com","api.my-domain2.it"]`,
			wantDeleted: []string{"api.my-domain2.it"},
			wantOwned:   `["app.my-domain1.com"]`,
		},
		{
			name: "hostname removed from domain map",
			// The spec still names api.my-domain2.it, but the map no longer
			// resolves it. The annotation alone cannot authorize deleting a
			// name outside the map, so the record is left for the operator
			// and the route stops claiming it.
			domainMap:   "my-domain1.com: 10.0.8.100\n",
			hostnames:   []string{"app.my-domain1.com", "api.my-domain2.it"},
			annotation:  `["app.my-domain1.com","api.my-domain2.it"]`,
			wantDeleted: nil,
			wantOwned:   `["app.my-domain1.com"]`,
		},
		{
			// Anyone who can edit the route can write the annotation, so a name
			// the domain map does not cover must never be deleted through it.
			name:        "annotation names a hostname outside the domain map",
			domainMap:   "my-domain1.com: 10.0.8.100\nmy-domain2.it: 10.0.9.50\n",
			hostnames:   []string{"app.my-domain1.com"},
			annotation:  `["app.my-domain1.com","vpn.corp.example"]`,
			wantDeleted: nil,
			wantOwned:   `["app.my-domain1.com"]`,
		},
		{
			// Guard: a reconcile that changes nothing must not write the route.
			// The old comparison mixed an unfiltered list with a filtered one, so
			// an annotation holding a hostname that dropped out of the map forced
			// one extra write — and that write silently rewrote the annotation to
			// hide the divergence while the record itself was never deleted.
			name:        "nothing changed, no annotation write",
			domainMap:   "my-domain1.com: 10.0.8.100\nmy-domain2.it: 10.0.9.50\n",
			hostnames:   []string{"app.my-domain1.com"},
			annotation:  `["app.my-domain1.com"]`,
			wantDeleted: nil,
			wantOwned:   `["app.my-domain1.com"]`,
			wantNoWrite: true,
		},
		{
			name:        "corrupt annotation is rewritten, not fatal",
			domainMap:   "my-domain1.com: 10.0.8.100\nmy-domain2.it: 10.0.9.50\n",
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
