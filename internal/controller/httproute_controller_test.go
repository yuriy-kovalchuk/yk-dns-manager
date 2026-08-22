package controller

import (
	"context"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/config"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

// newTestReconciler wires the reconciler with a RouteState over the fake
// client and a Manager containing the given mock provider instance (named
// "test").
func newTestReconciler(t *testing.T, fakeClient client.Client, mock *mockDNSProvider, upsert bool, dm *config.DomainMap) *HTTPRouteReconciler {
	t.Helper()
	log := zap.New(zap.UseDevMode(true))
	manager := dns.NewManager(log)
	manager.Add("test", upsert, mock)
	return &HTTPRouteReconciler{
		State:     NewRouteState(fakeClient, fakeClient, log),
		DomainMap: dm,
		DNS:       manager,
		Log:       log,
	}
}

// mockDNSProvider records DNS operations for test assertions.
type mockDNSProvider struct {
	mu              sync.Mutex
	existingHosts   map[string]bool // hostnames that Exists returns true for
	createdRecords  []dns.Record
	upsertedRecords []dns.Record
	deletedHosts    []string
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

func newTestDomainMap(t *testing.T) *config.DomainMap {
	t.Helper()
	return config.NewDomainMap(map[string]string{
		"my-domain1.com": "10.0.8.100",
		"my-domain2.it":  "10.0.9.50",
	})
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
	reconciler := newTestReconciler(t, fakeClient, mock, false, newTestDomainMap(t))

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
	reconciler := newTestReconciler(t, fakeClient, mock, false, newTestDomainMap(t))

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "unknown-route",
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

	// Unmanaged routes are never touched: no records, no finalizer.
	if len(mock.createdRecords) != 0 {
		t.Errorf("expected 0 created records for unknown domain, got %d", len(mock.createdRecords))
	}
	updated := &gatewayv1.HTTPRoute{}
	if err := fakeClient.Get(context.Background(), req.NamespacedName, updated); err != nil {
		t.Fatalf("failed to get route: %v", err)
	}
	if len(updated.Finalizers) != 0 {
		t.Errorf("expected no finalizer on an unmanaged route, got %v", updated.Finalizers)
	}
}

// TestHTTPRouteReconciler_LosesAllMappedHostnames verifies that a managed
// route whose hostnames no longer map has its records deleted and its
// annotation cleared.
func TestHTTPRouteReconciler_LosesAllMappedHostnames(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to install gateway-api scheme: %v", err)
	}

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "loses-mapping-route",
			Namespace:   "default",
			Finalizers:  []string{finalizerName},
			Annotations: map[string]string{managedHostnamesAnnotation: `["app.my-domain1.com"]`},
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
	reconciler := newTestReconciler(t, fakeClient, mock, false, newTestDomainMap(t))

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "loses-mapping-route",
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

	// The orphaned record is deleted.
	if len(mock.deletedHosts) != 1 || mock.deletedHosts[0] != "app.my-domain1.com" {
		t.Fatalf("expected deletion of ['app.my-domain1.com'], got %v", mock.deletedHosts)
	}
	// The annotation is cleared; the finalizer stays (route may be re-mapped).
	updated := &gatewayv1.HTTPRoute{}
	if err := fakeClient.Get(context.Background(), req.NamespacedName, updated); err != nil {
		t.Fatalf("failed to get route: %v", err)
	}
	if _, ok := updated.Annotations[managedHostnamesAnnotation]; ok {
		t.Error("expected managed-hostnames annotation to be removed")
	}
	if len(updated.Finalizers) != 1 {
		t.Errorf("expected the finalizer to remain, got %v", updated.Finalizers)
	}
}

// TestHTTPRouteReconciler_DeletionWithUnmappedHostnames verifies that a
// route whose spec no longer maps but whose annotation still lists a managed
// hostname is released (finalizer removed), never stuck in Terminating.
func TestHTTPRouteReconciler_DeletionWithUnmappedHostnames(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to install gateway-api scheme: %v", err)
	}

	now := metav1.Now()
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "orphan-delete-route",
			Namespace:         "default",
			Finalizers:        []string{finalizerName},
			DeletionTimestamp: &now,
			Annotations:       map[string]string{managedHostnamesAnnotation: `["app.my-domain1.com"]`},
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
	reconciler := newTestReconciler(t, fakeClient, mock, false, newTestDomainMap(t))

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "orphan-delete-route",
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

	// The orphaned record (from the annotation) is deleted.
	if len(mock.deletedHosts) != 1 || mock.deletedHosts[0] != "app.my-domain1.com" {
		t.Fatalf("expected deletion of ['app.my-domain1.com'], got %v", mock.deletedHosts)
	}
	// Finalizer removed → the fake client deletes the object entirely.
	updated := &gatewayv1.HTTPRoute{}
	err = fakeClient.Get(context.Background(), req.NamespacedName, updated)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected route to be gone after finalizer removal, got err=%v", err)
	}
}

// TestHTTPRouteReconciler_DeletionUnion verifies that the deletion path
// covers annotation ∪ spec, not just the spec.
func TestHTTPRouteReconciler_DeletionUnion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to install gateway-api scheme: %v", err)
	}

	now := metav1.Now()
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "union-delete-route",
			Namespace:         "default",
			Finalizers:        []string{finalizerName},
			DeletionTimestamp: &now,
			Annotations:       map[string]string{managedHostnamesAnnotation: `["app.my-domain1.com"]`},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			// Spec lost app.my-domain1.com, gained api.my-domain2.it — both
			// must be deleted on the way out.
			Hostnames: []gatewayv1.Hostname{"api.my-domain2.it"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(route).
		Build()

	mock := &mockDNSProvider{}
	reconciler := newTestReconciler(t, fakeClient, mock, false, newTestDomainMap(t))

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "union-delete-route",
			Namespace: "default",
		},
	}

	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.deletedHosts) != 2 {
		t.Fatalf("expected 2 deleted hosts (annotation ∪ spec), got %v", mock.deletedHosts)
	}
	if mock.deletedHosts[0] != "app.my-domain1.com" || mock.deletedHosts[1] != "api.my-domain2.it" {
		t.Errorf("expected ['app.my-domain1.com', 'api.my-domain2.it'], got %v", mock.deletedHosts)
	}
}

// TestHTTPRouteReconciler_DuplicateSpecHostnames verifies that duplicate
// hostnames in the spec produce a single record.
func TestHTTPRouteReconciler_DuplicateSpecHostnames(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to install gateway-api scheme: %v", err)
	}

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dup-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"app.my-domain1.com", "app.my-domain1.com"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(route).
		Build()

	mock := &mockDNSProvider{}
	reconciler := newTestReconciler(t, fakeClient, mock, false, newTestDomainMap(t))

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "dup-route",
			Namespace: "default",
		},
	}

	// First reconcile adds the finalizer, second processes hostnames.
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.createdRecords) != 1 {
		t.Fatalf("expected 1 created record for duplicated spec hostname, got %d", len(mock.createdRecords))
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
	reconciler := newTestReconciler(t, fakeClient, mock, true, newTestDomainMap(t))

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
	reconciler := newTestReconciler(t, fakeClient, mock, false, newTestDomainMap(t))

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
	reconciler := newTestReconciler(t, fakeClient, mock, false, newTestDomainMap(t))

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
