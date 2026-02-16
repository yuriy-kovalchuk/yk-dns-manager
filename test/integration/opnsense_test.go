package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	logrtesting "github.com/go-logr/logr/testing"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/opnsense"
)

// fakeOPNsense is a minimal in-memory OPNsense Unbound API for testing.
type fakeOPNsense struct {
	mu     sync.Mutex
	store  map[string]hostOverride
	nextID int
	calls  []string // tracks endpoint calls in order
}

type hostOverride struct {
	Enabled     string `json:"enabled"`
	Hostname    string `json:"hostname"`
	Domain      string `json:"domain"`
	RR          string `json:"rr"`
	Server      string `json:"server"`
	Description string `json:"description"`
	MXPrio      string `json:"mxprio"`
	MX          string `json:"mx"`
}

func newFakeOPNsense() *fakeOPNsense {
	return &fakeOPNsense{store: map[string]hostOverride{}}
}

func (f *fakeOPNsense) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	switch {
	case r.URL.Path == "/api/unbound/settings/searchHostOverride":
		f.handleSearch(w, r)
	case r.URL.Path == "/api/unbound/settings/addHostOverride":
		f.handleAdd(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/unbound/settings/setHostOverride/"):
		f.handleSet(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/unbound/settings/delHostOverride/"):
		f.handleDel(w, r)
	case r.URL.Path == "/api/unbound/service/reconfigure":
		f.handleReconfigure(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeOPNsense) handleSearch(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	type row struct {
		UUID string `json:"uuid"`
		hostOverride
	}
	rows := []row{}
	for id, h := range f.store {
		rows = append(rows, row{UUID: id, hostOverride: h})
	}
	writeJSON(w, map[string]interface{}{"rows": rows, "total": len(rows)})
}

func (f *fakeOPNsense) handleAdd(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Host hostOverride `json:"host"`
	}
	if err := readJSON(r, &payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.nextID++
	id := fmt.Sprintf("uuid-%d", f.nextID)
	f.store[id] = payload.Host
	f.mu.Unlock()

	writeJSON(w, map[string]string{"result": "saved", "uuid": id})
}

func (f *fakeOPNsense) handleSet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/unbound/settings/setHostOverride/")
	var payload struct {
		Host hostOverride `json:"host"`
	}
	if err := readJSON(r, &payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.store[id]; !ok {
		http.Error(w, `{"result":"not found"}`, http.StatusNotFound)
		return
	}
	f.store[id] = payload.Host
	writeJSON(w, map[string]string{"result": "saved"})
}

func (f *fakeOPNsense) handleDel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/unbound/settings/delHostOverride/")

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.store[id]; !ok {
		http.Error(w, `{"result":"not found"}`, http.StatusNotFound)
		return
	}
	delete(f.store, id)
	writeJSON(w, map[string]string{"result": "deleted"})
}

func (f *fakeOPNsense) handleReconfigure(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v interface{}) error {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func newProvider(t *testing.T, serverURL string) *opnsense.Provider {
	t.Helper()
	p, err := opnsense.New(logrtesting.NewTestLogger(t), map[string]string{
		"base_url":   serverURL + "/api",
		"api_key":    "test-key",
		"api_secret": "test-secret",
	})
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}
	return p
}

func TestCreateAndExists(t *testing.T) {
	fake := newFakeOPNsense()
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
	fake.mu.Lock()
	if len(fake.store) != 1 {
		t.Fatalf("expected 1 entry in store, got %d", len(fake.store))
	}
	for _, h := range fake.store {
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
	fake.mu.Unlock()
}

func TestUpdateExistingRecord(t *testing.T) {
	fake := newFakeOPNsense()
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
	fake.mu.Lock()
	for _, h := range fake.store {
		if h.Server != "10.0.0.2" {
			t.Errorf("expected server '10.0.0.2' after update, got %q", h.Server)
		}
	}
	fake.mu.Unlock()
}

func TestUpdateNonExistent(t *testing.T) {
	fake := newFakeOPNsense()
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
	fake := newFakeOPNsense()
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

	fake.mu.Lock()
	if len(fake.store) != 0 {
		t.Errorf("expected empty store after delete, got %d entries", len(fake.store))
	}
	fake.mu.Unlock()
}

func TestDeleteNonExistent(t *testing.T) {
	fake := newFakeOPNsense()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	p := newProvider(t, srv.URL)
	ctx := context.Background()

	err := p.Delete(ctx, "ghost.example.com", "A")
	if err == nil {
		t.Fatal("expected error when deleting non-existent record")
	}
}

func TestUpsertCreatesAndUpdates(t *testing.T) {
	fake := newFakeOPNsense()
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

	fake.mu.Lock()
	if len(fake.store) != 1 {
		t.Fatalf("expected 1 entry after first upsert, got %d", len(fake.store))
	}
	fake.mu.Unlock()

	// Second upsert should update.
	err = p.Upsert(ctx, dns.Record{
		Hostname: "app.example.com",
		Type:     "A",
		Value:    "10.0.0.99",
	})
	if err != nil {
		t.Fatalf("Upsert (update): %v", err)
	}

	fake.mu.Lock()
	if len(fake.store) != 1 {
		t.Fatalf("expected still 1 entry after second upsert, got %d", len(fake.store))
	}
	for _, h := range fake.store {
		if h.Server != "10.0.0.99" {
			t.Errorf("expected server '10.0.0.99' after upsert update, got %q", h.Server)
		}
	}
	fake.mu.Unlock()
}

func TestFullLifecycle(t *testing.T) {
	fake := newFakeOPNsense()
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
	fake.mu.Lock()
	var updatedIP string
	for _, h := range fake.store {
		if h.Hostname == "web" && h.Domain == "mysite.org" {
			updatedIP = h.Server
		}
	}
	fake.mu.Unlock()
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
	fake := newFakeOPNsense()
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

	fake.mu.Lock()
	if len(fake.store) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(fake.store))
	}
	fake.mu.Unlock()

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
