package controller

import (
	"context"
	"os"
	"path/filepath"
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
}

func (m *mockDNSProvider) Exists(_ context.Context, hostname, recordType string) (bool, error) {
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

func (m *mockDNSProvider) Update(_ context.Context, record dns.Record) error {
	return nil
}

func (m *mockDNSProvider) Delete(_ context.Context, hostname, recordType string) error {
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

func newTestDomainMap(t *testing.T) *config.DomainMap {
	t.Helper()
	content := "my-domain1.com: 10.0.8.100\nmy-domain2.it: 10.0.9.50\n"
	path := filepath.Join(t.TempDir(), "domain-map.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
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
	if result.Requeue {
		t.Error("expected no requeue")
	}

	// Second reconcile processes the hostnames
	result, err = reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error on second reconcile: %v", err)
	}
	if result.Requeue {
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
	if result.Requeue {
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
	if result.Requeue {
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
