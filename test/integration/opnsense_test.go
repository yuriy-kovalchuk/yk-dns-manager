package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	logrtesting "github.com/go-logr/logr/testing"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/opnsense"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/testenv"
)

// newFake returns a fake appliance that only accepts the credentials
// newProvider sends, so every test also proves the provider authenticates.
func newFake() *testenv.OPNsense {
	fake := testenv.NewOPNsense()
	fake.RequireCredentials("test-key", "test-secret")
	return fake
}

func newProvider(t *testing.T, serverURL string) *opnsense.Provider {
	t.Helper()
	p, err := opnsense.New(logrtesting.NewTestLogger(t), map[string]string{
		"base_url":            serverURL + "/api",
		"api_key":             "test-key",
		"api_secret":          "test-secret",
		"allow_insecure_http": "true",
	})
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}
	return p
}

func TestCreateAndExists(t *testing.T) {
	fake := newFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	p := newProvider(t, srv.URL)
	ctx := context.Background()

	// Should not exist yet.
	exists, err := p.Exists(ctx, "app.example.com", "A")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Fatal("expected record to not exist before Create")
	}

	// Create it.
	err = p.Create(ctx, dns.Record{
		Hostname: "app.example.com",
		Type:     "A",
		Value:    "10.0.0.1",
		Meta:     map[string]string{"description": "test record"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Should exist now.
	exists, err = p.Exists(ctx, "app.example.com", "A")
	if err != nil {
		t.Fatalf("Exists after Create: %v", err)
	}
	if !exists {
		t.Fatal("expected record to exist after Create")
	}

	// Verify the stored data.
	store := fake.Snapshot()
	if len(store) != 1 {
		t.Fatalf("expected 1 entry in store, got %d", len(store))
	}
	for _, h := range store {
		if h.Hostname != "app" {
			t.Errorf("expected hostname 'app', got %q", h.Hostname)
		}
		if h.Domain != "example.com" {
			t.Errorf("expected domain 'example.com', got %q", h.Domain)
		}
		if h.Server != "10.0.0.1" {
			t.Errorf("expected server '10.0.0.1', got %q", h.Server)
		}
		if h.RR != "A" {
			t.Errorf("expected rr 'A', got %q", h.RR)
		}
		if h.Description != "test record" {
			t.Errorf("expected description 'test record', got %q", h.Description)
		}
		if h.Enabled != "1" {
			t.Errorf("expected enabled '1', got %q", h.Enabled)
		}
	}
}

func TestUpdateExistingRecord(t *testing.T) {
	fake := newFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	p := newProvider(t, srv.URL)
	ctx := context.Background()

	// Create initial record.
	err := p.Create(ctx, dns.Record{
		Hostname: "app.example.com",
		Type:     "A",
		Value:    "10.0.0.1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Update to a new IP.
	err = p.Update(ctx, dns.Record{
		Hostname: "app.example.com",
		Type:     "A",
		Value:    "10.0.0.2",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Verify the IP changed in the store.
	store := fake.Snapshot()
	for _, h := range store {
		if h.Server != "10.0.0.2" {
			t.Errorf("expected server '10.0.0.2' after update, got %q", h.Server)
		}
	}
}

func TestUpdateNonExistent(t *testing.T) {
	fake := newFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	p := newProvider(t, srv.URL)
	ctx := context.Background()

	err := p.Update(ctx, dns.Record{
		Hostname: "ghost.example.com",
		Type:     "A",
		Value:    "10.0.0.1",
	})
	if err == nil {
		t.Fatal("expected error when updating non-existent record")
	}
}

func TestDeleteExistingRecord(t *testing.T) {
	fake := newFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	p := newProvider(t, srv.URL)
	ctx := context.Background()

	// Create then delete.
	err := p.Create(ctx, dns.Record{
		Hostname: "app.example.com",
		Type:     "A",
		Value:    "10.0.0.1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	err = p.Delete(ctx, "app.example.com", "A")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Should not exist anymore.
	exists, err := p.Exists(ctx, "app.example.com", "A")
	if err != nil {
		t.Fatalf("Exists after Delete: %v", err)
	}
	if exists {
		t.Fatal("expected record to not exist after Delete")
	}

	store := fake.Snapshot()
	if len(store) != 0 {
		t.Errorf("expected empty store after delete, got %d entries", len(store))
	}
}

func TestDeleteNonExistent(t *testing.T) {
	fake := newFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	p := newProvider(t, srv.URL)
	ctx := context.Background()

	err := p.Delete(ctx, "ghost.example.com", "A")
	if err != nil {
		t.Fatalf("expected no error when deleting non-existent record: %v", err)
	}
}

func TestUpsertCreatesAndUpdates(t *testing.T) {
	fake := newFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	p := newProvider(t, srv.URL)
	ctx := context.Background()

	// First upsert should create.
	err := p.Upsert(ctx, dns.Record{
		Hostname: "app.example.com",
		Type:     "A",
		Value:    "10.0.0.1",
	})
	if err != nil {
		t.Fatalf("Upsert (create): %v", err)
	}

	store := fake.Snapshot()
	if len(store) != 1 {
		t.Fatalf("expected 1 entry after first upsert, got %d", len(store))
	}

	// Second upsert should update.
	err = p.Upsert(ctx, dns.Record{
		Hostname: "app.example.com",
		Type:     "A",
		Value:    "10.0.0.99",
	})
	if err != nil {
		t.Fatalf("Upsert (update): %v", err)
	}

	store = fake.Snapshot()
	if len(store) != 1 {
		t.Fatalf("expected still 1 entry after second upsert, got %d", len(store))
	}
	for _, h := range store {
		if h.Server != "10.0.0.99" {
			t.Errorf("expected server '10.0.0.99' after upsert update, got %q", h.Server)
		}
	}
}

func TestFullLifecycle(t *testing.T) {
	fake := newFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	p := newProvider(t, srv.URL)
	ctx := context.Background()

	// 1. Doesn't exist
	exists, err := p.Exists(ctx, "web.mysite.org", "A")
	if err != nil {
		t.Fatalf("step 1 Exists: %v", err)
	}
	if exists {
		t.Fatal("step 1: expected not to exist")
	}

	// 2. Create
	err = p.Create(ctx, dns.Record{
		Hostname: "web.mysite.org",
		Type:     "A",
		Value:    "192.168.1.10",
		Meta:     map[string]string{"description": "web server"},
	})
	if err != nil {
		t.Fatalf("step 2 Create: %v", err)
	}

	// 3. Exists
	exists, err = p.Exists(ctx, "web.mysite.org", "A")
	if err != nil {
		t.Fatalf("step 3 Exists: %v", err)
	}
	if !exists {
		t.Fatal("step 3: expected to exist after Create")
	}

	// 4. Update IP
	err = p.Update(ctx, dns.Record{
		Hostname: "web.mysite.org",
		Type:     "A",
		Value:    "192.168.1.20",
	})
	if err != nil {
		t.Fatalf("step 4 Update: %v", err)
	}

	// 5. Verify updated value
	store := fake.Snapshot()
	var updatedIP string
	for _, h := range store {
		if h.Hostname == "web" && h.Domain == "mysite.org" {
			updatedIP = h.Server
		}
	}
	if updatedIP != "192.168.1.20" {
		t.Fatalf("step 5: expected IP '192.168.1.20', got %q", updatedIP)
	}

	// 6. Delete
	err = p.Delete(ctx, "web.mysite.org", "A")
	if err != nil {
		t.Fatalf("step 6 Delete: %v", err)
	}

	// 7. Doesn't exist anymore
	exists, err = p.Exists(ctx, "web.mysite.org", "A")
	if err != nil {
		t.Fatalf("step 7 Exists: %v", err)
	}
	if exists {
		t.Fatal("step 7: expected not to exist after Delete")
	}
}

func TestMultipleRecords(t *testing.T) {
	fake := newFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	p := newProvider(t, srv.URL)
	ctx := context.Background()

	records := []dns.Record{
		{Hostname: "app.example.com", Type: "A", Value: "10.0.0.1"},
		{Hostname: "api.example.com", Type: "A", Value: "10.0.0.2"},
		{Hostname: "db.other.net", Type: "A", Value: "10.0.0.3"},
	}

	for _, rec := range records {
		if err := p.Create(ctx, rec); err != nil {
			t.Fatalf("Create %s: %v", rec.Hostname, err)
		}
	}

	store := fake.Snapshot()
	if len(store) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(store))
	}

	// Each should exist independently.
	for _, rec := range records {
		exists, err := p.Exists(ctx, rec.Hostname, rec.Type)
		if err != nil {
			t.Fatalf("Exists %s: %v", rec.Hostname, err)
		}
		if !exists {
			t.Errorf("expected %s to exist", rec.Hostname)
		}
	}

	// Delete one — others should remain.
	if err := p.Delete(ctx, "api.example.com", "A"); err != nil {
		t.Fatalf("Delete api.example.com: %v", err)
	}

	exists, _ := p.Exists(ctx, "api.example.com", "A")
	if exists {
		t.Error("api.example.com should not exist after delete")
	}
	exists, _ = p.Exists(ctx, "app.example.com", "A")
	if !exists {
		t.Error("app.example.com should still exist")
	}
	exists, _ = p.Exists(ctx, "db.other.net", "A")
	if !exists {
		t.Error("db.other.net should still exist")
	}
}

// TestDeleteRemovesAllDuplicates covers the case where OPNsense holds several
// overrides for the same hostname/RR. Deleting only the first left the rest
// answering with stale values and nothing else ever removed them.
func TestDeleteRemovesAllDuplicates(t *testing.T) {
	fake := newFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	p := newProvider(t, srv.URL)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := p.Create(ctx, dns.Record{
			Hostname: "dup.example.com",
			Type:     "A",
			Value:    fmt.Sprintf("10.0.0.%d", i+1),
		}); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	store := fake.Snapshot()
	seeded := len(store)
	if seeded != 2 {
		t.Fatalf("expected 2 duplicate overrides seeded, got %d", seeded)
	}

	if err := p.Delete(ctx, "dup.example.com", "A"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	store = fake.Snapshot()
	if len(store) != 0 {
		t.Fatalf("expected 0 overrides after Delete, got %d: %+v", len(store), store)
	}
}

// TestUpsertConvergesDuplicates checks that writing to a hostname that has
// duplicate overrides leaves exactly one row behind, holding the new value.
func TestUpsertConvergesDuplicates(t *testing.T) {
	fake := newFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	p := newProvider(t, srv.URL)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := p.Create(ctx, dns.Record{
			Hostname: "dup.example.com",
			Type:     "A",
			Value:    fmt.Sprintf("10.0.0.%d", i+1),
		}); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	if err := p.Upsert(ctx, dns.Record{Hostname: "dup.example.com", Type: "A", Value: "10.0.0.99"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	store := fake.Snapshot()
	if len(store) != 1 {
		t.Fatalf("expected 1 override after Upsert, got %d: %+v", len(store), store)
	}
	for _, h := range store {
		if h.Server != "10.0.0.99" {
			t.Errorf("surviving override has server %q, want 10.0.0.99", h.Server)
		}
	}
}

// TestUpsertUpdatePathCallCount pins the API cost of an update-path Upsert.
// It used to be four calls: Exists fetched the full list, Update fetched it
// again, then setHostOverride and a per-record reconfigure. It is now one list
// fetch plus one write, with reconfigure deferred to Flush.
func TestUpsertUpdatePathCallCount(t *testing.T) {
	fake := newFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	p := newProvider(t, srv.URL)
	ctx := context.Background()

	if err := p.Create(ctx, dns.Record{Hostname: "app.example.com", Type: "A", Value: "10.0.0.1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	fake.ResetCalls()

	if err := p.Upsert(ctx, dns.Record{Hostname: "app.example.com", Type: "A", Value: "10.0.0.2"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got := fake.Calls()

	want := []string{
		"GET /api/unbound/settings/searchHostOverride",
		"POST /api/unbound/settings/setHostOverride/uuid-1",
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("update-path Upsert made %d calls %v, want %v", len(got), got, want)
	}

	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	got = fake.Calls()[len(want):]
	if len(got) != 1 || got[0] != "POST /api/unbound/service/reconfigure" {
		t.Fatalf("Flush issued %v, want exactly one reconfigure", got)
	}

	// A second Flush with nothing staged must not reconfigure again.
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	n := len(fake.Calls())
	if n != len(want)+1 {
		t.Fatalf("idle Flush issued extra calls: total %d, want %d", n, len(want)+1)
	}
}

func TestHealthCheckRejectsWrongCredentials(t *testing.T) {
	fake := testenv.NewOPNsense()
	fake.RequireCredentials("other-key", "other-secret")
	srv := httptest.NewServer(fake)
	defer srv.Close()

	err := newProvider(t, srv.URL).HealthCheck(context.Background())
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("HealthCheck = %v, want an authentication failure", err)
	}
}

// TestCreateSurfacesValidationFailure proves a row the appliance rejects comes
// back as an error. The appliance answers HTTP 200 with result "failed", so a
// provider that only checked the status code would report success.
func TestCreateSurfacesValidationFailure(t *testing.T) {
	fake := newFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	err := newProvider(t, srv.URL).Create(context.Background(), dns.Record{
		Hostname: "app.example.com",
		Type:     "A",
		Value:    "not-an-ip",
	})
	if err == nil {
		t.Fatal("expected Create to fail for a row the appliance rejects")
	}
	if n := len(fake.Snapshot()); n != 0 {
		t.Errorf("rejected row was stored: %d rows", n)
	}
}

// TestFlushPublishes pins that writes are staged and only Flush makes Unbound
// answer with them, and that a failed Flush stays pending and is retried.
func TestFlushPublishes(t *testing.T) {
	fake := newFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	p := newProvider(t, srv.URL)
	ctx := context.Background()

	if err := p.Create(ctx, dns.Record{Hostname: "app.example.com", Type: "A", Value: "10.0.0.1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if n := len(fake.Published()); n != 0 {
		t.Fatalf("published %d rows before Flush, want 0", n)
	}

	fake.SetFailure(testenv.EndpointReconfigure, http.StatusInternalServerError)
	if err := p.Flush(ctx); err == nil {
		t.Fatal("expected Flush to fail while reconfigure fails")
	}
	if n := len(fake.Published()); n != 0 {
		t.Fatalf("published %d rows after a failed Flush, want 0", n)
	}

	fake.SetFailure(testenv.EndpointReconfigure, 0)
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("retried Flush: %v", err)
	}
	if n := len(fake.Published()); n != 1 {
		t.Fatalf("published %d rows after a retried Flush, want 1", n)
	}
}
