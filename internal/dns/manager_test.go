package dns

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
)

// mockProvider records operations and can be made to fail per method.
type mockProvider struct {
	mu        sync.Mutex
	exists    map[string]bool
	created   []Record
	upserted  []Record
	deleted   []string
	existsErr error
	createErr error
	upsertErr error
	deleteErr error
	healthErr error
}

func (m *mockProvider) Exists(_ context.Context, hostname, _ string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.existsErr != nil {
		return false, m.existsErr
	}
	return m.exists[hostname], nil
}

func (m *mockProvider) Create(_ context.Context, record Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return m.createErr
	}
	m.created = append(m.created, record)
	return nil
}

func (m *mockProvider) Update(_ context.Context, _ Record) error { return nil }

func (m *mockProvider) Delete(_ context.Context, hostname, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteErr != nil {
		return m.deleteErr
	}
	m.deleted = append(m.deleted, hostname)
	return nil
}

func (m *mockProvider) Upsert(_ context.Context, record Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.upsertErr != nil {
		return m.upsertErr
	}
	m.upserted = append(m.upserted, record)
	return nil
}

func (m *mockProvider) HealthCheck(_ context.Context) error { return m.healthErr }

func (m *mockProvider) snapshot() (created, upserted, deleted []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.created {
		created = append(created, r.Hostname)
	}
	for _, r := range m.upserted {
		upserted = append(upserted, r.Hostname)
	}
	deleted = append(deleted, m.deleted...)
	return
}

func newMockManager(a, b *mockProvider) *Manager {
	m := NewManager(logr.Discard())
	m.Add("a", false, a)
	m.Add("b", true, b)
	return m
}

func TestManager_EnsureRecord_Fanout(t *testing.T) {
	a, b := &mockProvider{}, &mockProvider{}
	m := newMockManager(a, b)

	rec := Record{Hostname: "app.example.com", Type: "A", Value: "10.0.0.1"}
	if err := m.EnsureRecord(context.Background(), rec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Non-upsert instance a: does not exist → Create.
	// Upsert instance b: always Upsert.
	ac, au, ad := a.snapshot()
	bc, bu, bd := b.snapshot()
	if len(ac) != 1 || ac[0] != "app.example.com" {
		t.Errorf("instance a: expected 1 create, got %v", ac)
	}
	if len(au) != 0 {
		t.Errorf("instance a: expected 0 upserts, got %v", au)
	}
	if len(bc) != 0 {
		t.Errorf("instance b: expected 0 creates, got %v", bc)
	}
	if len(bu) != 1 || bu[0] != "app.example.com" {
		t.Errorf("instance b: expected 1 upsert, got %v", bu)
	}
	if len(ad) != 0 || len(bd) != 0 {
		t.Errorf("expected 0 deletes, got a=%v b=%v", ad, bd)
	}
}

func TestManager_EnsureRecord_SkipsExistingNonUpsert(t *testing.T) {
	a := &mockProvider{exists: map[string]bool{"app.example.com": true}}
	b := &mockProvider{}
	m := newMockManager(a, b)

	rec := Record{Hostname: "app.example.com", Type: "A", Value: "10.0.0.1"}
	if err := m.EnsureRecord(context.Background(), rec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ac, au, _ := a.snapshot()
	if len(ac) != 0 || len(au) != 0 {
		t.Errorf("instance a: record exists and upsert disabled — expected no ops, got creates=%v upserts=%v", ac, au)
	}
}

func TestManager_EnsureRecord_JoinsErrors(t *testing.T) {
	boom := errors.New("boom")
	a := &mockProvider{createErr: boom}
	b := &mockProvider{}
	m := newMockManager(a, b)

	err := m.EnsureRecord(context.Background(), Record{Hostname: "app.example.com", Type: "A", Value: "10.0.0.1"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, boom) {
		t.Errorf("expected joined error to wrap boom, got %v", err)
	}
	if !strings.Contains(err.Error(), "a:") {
		t.Errorf("expected error to name failing instance 'a', got %v", err)
	}
	// Instance b must still have been processed despite a's failure.
	if _, bu, _ := b.snapshot(); len(bu) != 1 {
		t.Errorf("instance b: expected 1 upsert despite a failing, got %d", len(bu))
	}
}

func TestManager_DeleteRecord_FanoutAndJoinErrors(t *testing.T) {
	a := &mockProvider{deleteErr: errors.New("delete failed")}
	b := &mockProvider{}
	m := newMockManager(a, b)

	err := m.DeleteRecord(context.Background(), "app.example.com", "A")
	if err == nil || !strings.Contains(err.Error(), "a:") {
		t.Fatalf("expected joined error naming instance a, got %v", err)
	}
	if _, _, bd := b.snapshot(); len(bd) != 1 {
		t.Errorf("instance b: expected delete to still run, got %v", bd)
	}
}

func TestManager_HealthCheck_AllMustPass(t *testing.T) {
	a := &mockProvider{healthErr: errors.New("a down")}
	b := &mockProvider{}
	m := newMockManager(a, b)

	err := m.HealthCheck(context.Background())
	if err == nil || !strings.Contains(err.Error(), "a:") {
		t.Fatalf("expected health check error naming instance a, got %v", err)
	}

	// All healthy → nil.
	healthy := NewManager(logr.Discard())
	healthy.Add("x", false, &mockProvider{})
	if err := healthy.HealthCheck(context.Background()); err != nil {
		t.Errorf("expected nil for healthy manager, got %v", err)
	}
}

func TestManager_Empty_NoOp(t *testing.T) {
	// Zero instances is a valid state (no-op mode): all operations
	// succeed without side effects.
	m := NewManager(logr.Discard())
	if err := m.EnsureRecord(context.Background(), Record{Hostname: "a.example.com", Type: "A", Value: "10.0.0.1"}); err != nil {
		t.Errorf("EnsureRecord on empty manager: %v", err)
	}
	if err := m.DeleteRecord(context.Background(), "a.example.com", "A"); err != nil {
		t.Errorf("DeleteRecord on empty manager: %v", err)
	}
	if err := m.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck on empty manager: %v", err)
	}
}

func TestManager_Len(t *testing.T) {
	m := NewManager(logr.Discard())
	if m.Len() != 0 {
		t.Fatalf("expected empty manager, got %d", m.Len())
	}
	m.Add("a", false, &mockProvider{})
	m.Add("b", true, &mockProvider{})
	if m.Len() != 2 {
		t.Fatalf("expected 2 instances, got %d", m.Len())
	}
}
