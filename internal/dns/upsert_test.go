package dns

import (
	"context"
	"errors"
	"testing"
)

type upsertFake struct {
	exists    bool
	errExists error
	calls     []string
}

func (f *upsertFake) Exists(_ context.Context, _, _ string) (bool, error) {
	f.calls = append(f.calls, "exists")
	return f.exists, f.errExists
}

func (f *upsertFake) Create(_ context.Context, _ Record) error {
	f.calls = append(f.calls, "create")
	return nil
}

func (f *upsertFake) Update(_ context.Context, _ Record) error {
	f.calls = append(f.calls, "update")
	return nil
}

func (f *upsertFake) Delete(_ context.Context, _, _ string) error {
	f.calls = append(f.calls, "delete")
	return nil
}

func (f *upsertFake) Upsert(_ context.Context, _ Record) error {
	f.calls = append(f.calls, "upsert")
	return nil
}

func (f *upsertFake) HealthCheck(_ context.Context) error { return nil }

func TestUpsert_CreatesWhenAbsent(t *testing.T) {
	f := &upsertFake{exists: false}
	if err := Upsert(context.Background(), f, Record{Hostname: "h", Type: "A", Value: "1.2.3.4"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if len(f.calls) != 2 || f.calls[0] != "exists" || f.calls[1] != "create" {
		t.Errorf("expected [exists create], got %v", f.calls)
	}
}

func TestUpsert_UpdatesWhenPresent(t *testing.T) {
	f := &upsertFake{exists: true}
	if err := Upsert(context.Background(), f, Record{Hostname: "h", Type: "A", Value: "1.2.3.4"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if len(f.calls) != 2 || f.calls[0] != "exists" || f.calls[1] != "update" {
		t.Errorf("expected [exists update], got %v", f.calls)
	}
}

func TestUpsert_ExistsError(t *testing.T) {
	boom := errors.New("boom")
	f := &upsertFake{errExists: boom}
	err := Upsert(context.Background(), f, Record{Hostname: "h", Type: "A", Value: "1.2.3.4"})
	if !errors.Is(err, boom) {
		t.Errorf("expected the Exists error, got %v", err)
	}
	if len(f.calls) != 1 || f.calls[0] != "exists" {
		t.Errorf("expected no mutation calls after an Exists error, got %v", f.calls)
	}
}
