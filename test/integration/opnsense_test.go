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
func newFake(t *testing.T) (*testenv.OPNsense, string) {
	t.Helper()
	fake := testenv.NewOPNsense()
	fake.RequireCredentials("test-key", "test-secret")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return fake, srv.URL
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

func record(fqdn, ip string) dns.Record {
	return dns.Record{
		Hostname: fqdn,
		Type:     "A",
		Value:    ip,
		Meta:     map[string]string{"description": "managed by yk-dns-manager"},
	}
}

func rowsFor(store map[string]testenv.HostOverride, fqdn string) []testenv.HostOverride {
	var out []testenv.HostOverride
	for _, h := range store {
		if h.FQDN() == fqdn {
			out = append(out, h)
		}
	}
	return out
}

// only returns the single row for fqdn, failing the test if there is not
// exactly one.
func only(t *testing.T, store map[string]testenv.HostOverride, fqdn string) testenv.HostOverride {
	t.Helper()
	rows := rowsFor(store, fqdn)
	if len(rows) != 1 {
		t.Fatalf("%d rows for %s, want 1: %v", len(rows), fqdn, rows)
	}
	return rows[0]
}

func countCalls(fake *testenv.OPNsense, endpoint string) int {
	n := 0
	for _, c := range fake.Calls() {
		if strings.Contains(c, "/"+endpoint) {
			n++
		}
	}
	return n
}

func ensure(t *testing.T, b dns.Batch, rec dns.Record, overwrite bool) dns.Result {
	t.Helper()
	res, err := b.Ensure(context.Background(), rec, overwrite)
	if err != nil {
		t.Fatalf("Ensure %s: %v", rec.Hostname, err)
	}
	return res
}

func commit(t *testing.T, b dns.Batch) {
	t.Helper()
	if err := b.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestEnsureCreatesRecord(t *testing.T) {
	fake, url := newFake(t)
	p := newProvider(t, url)

	b := p.Batch()
	if res := ensure(t, b, record("app.example.com", "10.0.0.1"), false); res != dns.Created {
		t.Fatalf("Ensure = %s, want created", res)
	}
	commit(t, b)

	h := only(t, fake.Published(), "app.example.com")
	if h.Hostname != "app" || h.Domain != "example.com" {
		t.Errorf("split as %q / %q, want app / example.com", h.Hostname, h.Domain)
	}
	if h.Server != "10.0.0.1" || h.RR != "A" || h.Enabled != "1" {
		t.Errorf("row = %+v, want an enabled A record for 10.0.0.1", h)
	}
	if h.Description != "managed by yk-dns-manager" {
		t.Errorf("description = %q", h.Description)
	}
}

func TestEnsureOverwriteUpdatesInPlace(t *testing.T) {
	fake, url := newFake(t)
	p := newProvider(t, url)

	b := p.Batch()
	ensure(t, b, record("app.example.com", "10.0.0.1"), true)
	commit(t, b)

	b = p.Batch()
	if res := ensure(t, b, record("app.example.com", "10.0.0.99"), true); res != dns.Updated {
		t.Fatalf("Ensure = %s, want updated", res)
	}
	commit(t, b)

	if h := only(t, fake.Published(), "app.example.com"); h.Server != "10.0.0.99" {
		t.Errorf("server = %s, want 10.0.0.99", h.Server)
	}
}

func TestEnsureWithoutOverwriteKeepsExistingValue(t *testing.T) {
	fake, url := newFake(t)
	fake.Seed(testenv.HostOverride{Enabled: "1", Hostname: "app", Domain: "example.com", RR: "A", Server: "10.0.0.50"})
	p := newProvider(t, url)

	b := p.Batch()
	if res := ensure(t, b, record("app.example.com", "10.0.0.1"), false); res != dns.Kept {
		t.Fatalf("Ensure = %s, want kept", res)
	}
	commit(t, b)

	if h := only(t, fake.Snapshot(), "app.example.com"); h.Server != "10.0.0.50" {
		t.Errorf("server = %s, want the existing 10.0.0.50", h.Server)
	}
}

// TestEnsureUnchangedWritesNothing is audit B2 at the provider: a record that
// already matches costs one list and no write, and with nothing owed the
// commit does not reconfigure.
func TestEnsureUnchangedWritesNothing(t *testing.T) {
	for _, overwrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("overwrite=%v", overwrite), func(t *testing.T) {
			fake, url := newFake(t)
			p := newProvider(t, url)

			b := p.Batch()
			ensure(t, b, record("app.example.com", "10.0.0.1"), overwrite)
			commit(t, b)
			fake.ResetCalls()

			b = p.Batch()
			if res := ensure(t, b, record("app.example.com", "10.0.0.1"), overwrite); res != dns.Unchanged {
				t.Fatalf("Ensure = %s, want unchanged", res)
			}
			commit(t, b)

			want := []string{"GET /api/unbound/settings/searchHostOverride"}
			if got := fake.Calls(); len(got) != 1 || got[0] != want[0] {
				t.Fatalf("calls = %v, want %v", got, want)
			}
		})
	}
}

// TestEnsureReenablesDisabledRecord is audit B8 at the provider: a disabled
// override answers nothing, so create-only mode must not treat it as an
// existing record to leave alone.
func TestEnsureReenablesDisabledRecord(t *testing.T) {
	fake, url := newFake(t)
	fake.Seed(testenv.HostOverride{Enabled: "0", Hostname: "app", Domain: "example.com", RR: "A", Server: "10.0.0.1",
		Description: "managed by yk-dns-manager"})
	p := newProvider(t, url)

	b := p.Batch()
	if res := ensure(t, b, record("app.example.com", "10.0.0.1"), false); res != dns.Updated {
		t.Fatalf("Ensure = %s, want updated", res)
	}
	commit(t, b)

	if h := only(t, fake.Published(), "app.example.com"); h.Enabled != "1" {
		t.Errorf("enabled = %q, want 1", h.Enabled)
	}
}

func TestEnsureSurfacesValidationFailure(t *testing.T) {
	fake, url := newFake(t)
	p := newProvider(t, url)

	// The appliance answers HTTP 200 with result "failed", so a provider that
	// only checked the status code would report success.
	if _, err := p.Batch().Ensure(context.Background(), record("app.example.com", "not-an-ip"), false); err == nil {
		t.Fatal("expected Ensure to fail for a row the appliance rejects")
	}
	if n := len(fake.Snapshot()); n != 0 {
		t.Errorf("rejected row was stored: %d rows", n)
	}
}

func TestEnsureConvergesDuplicates(t *testing.T) {
	fake, url := newFake(t)
	for _, ip := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		fake.Seed(testenv.HostOverride{Enabled: "1", Hostname: "app", Domain: "example.com", RR: "A", Server: ip})
	}
	p := newProvider(t, url)

	b := p.Batch()
	ensure(t, b, record("app.example.com", "10.0.0.99"), true)
	commit(t, b)

	// A stale duplicate keeps answering after the live one is updated.
	if h := only(t, fake.Published(), "app.example.com"); h.Server != "10.0.0.99" {
		t.Errorf("surviving override has server %q, want 10.0.0.99", h.Server)
	}
}

func TestRemoveDeletesEveryDuplicate(t *testing.T) {
	fake, url := newFake(t)
	for _, ip := range []string{"10.0.0.1", "10.0.0.2"} {
		fake.Seed(testenv.HostOverride{Enabled: "1", Hostname: "app", Domain: "example.com", RR: "A", Server: ip})
	}
	fake.Seed(testenv.HostOverride{Enabled: "1", Hostname: "other", Domain: "example.com", RR: "A", Server: "10.0.0.3"})
	p := newProvider(t, url)

	b := p.Batch()
	if err := b.Remove(context.Background(), "app.example.com", "A"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	commit(t, b)

	published := fake.Published()
	if n := len(rowsFor(published, "app.example.com")); n != 0 {
		t.Errorf("%d app.example.com rows left, want 0", n)
	}
	only(t, published, "other.example.com")
}

func TestRemoveMissingRecordIsNotAnError(t *testing.T) {
	fake, url := newFake(t)
	p := newProvider(t, url)

	b := p.Batch()
	if err := b.Remove(context.Background(), "ghost.example.com", "A"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if n := countCalls(fake, testenv.EndpointDel); n != 0 {
		t.Errorf("issued %d deletes for a record that does not exist", n)
	}
}

// TestRemoveToleratesVanishedRecord: the batch works from the list it read
// first. A row deleted by someone else in the meantime is answered with
// "not found", which is what Remove wanted anyway.
func TestRemoveToleratesVanishedRecord(t *testing.T) {
	fake, url := newFake(t)
	fake.Seed(testenv.HostOverride{Enabled: "1", Hostname: "app", Domain: "example.com", RR: "A", Server: "10.0.0.1"})
	fake.Seed(testenv.HostOverride{Enabled: "1", Hostname: "web", Domain: "example.com", RR: "A", Server: "10.0.0.1"})
	p := newProvider(t, url)
	ctx := context.Background()

	stale := p.Batch()
	if err := stale.Remove(ctx, "web.example.com", "A"); err != nil { // loads the list
		t.Fatalf("Remove web: %v", err)
	}

	other := p.Batch()
	if err := other.Remove(ctx, "app.example.com", "A"); err != nil {
		t.Fatalf("Remove app in another batch: %v", err)
	}

	if err := stale.Remove(ctx, "app.example.com", "A"); err != nil {
		t.Fatalf("Remove of an already deleted row: %v", err)
	}
}

// TestBatchListsOnce is audit D4: however many records a batch touches, it
// reads the appliance's list once.
func TestBatchListsOnce(t *testing.T) {
	fake, url := newFake(t)
	p := newProvider(t, url)

	b := p.Batch()
	ensure(t, b, record("a.example.com", "10.0.0.1"), true)
	ensure(t, b, record("b.example.com", "10.0.0.1"), true)
	ensure(t, b, record("a.example.com", "10.0.0.2"), true)
	if err := b.Remove(context.Background(), "b.example.com", "A"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	commit(t, b)

	if n := countCalls(fake, testenv.EndpointSearch); n != 1 {
		t.Errorf("listed %d times, want 1", n)
	}
	if n := countCalls(fake, testenv.EndpointReconfigure); n != 1 {
		t.Errorf("reconfigured %d times, want 1", n)
	}
	published := fake.Published()
	if h := only(t, published, "a.example.com"); h.Server != "10.0.0.2" {
		t.Errorf("a.example.com = %s, want 10.0.0.2 (the batch must see its own writes)", h.Server)
	}
	if n := len(rowsFor(published, "b.example.com")); n != 0 {
		t.Errorf("b.example.com still has %d rows", n)
	}
}

// TestUnusedBatchCommitsNothing: a reconcile that never touched DNS must not
// reach the appliance, even with a publish owed.
func TestUnusedBatchCommitsNothing(t *testing.T) {
	fake, url := newFake(t)
	p := newProvider(t, url)

	commit(t, p.Batch())

	if n := len(fake.Calls()); n != 0 {
		t.Errorf("an unused batch made %d calls", n)
	}
}

// TestFirstCommitPublishes is audit B10: a new process cannot know whether a
// previous one left staged changes unpublished, so its first used batch
// publishes even when it changed nothing.
func TestFirstCommitPublishes(t *testing.T) {
	fake, url := newFake(t)
	// Staged on the appliance by an earlier process that died before
	// publishing it.
	fake.Seed(testenv.HostOverride{Enabled: "1", Hostname: "app", Domain: "example.com", RR: "A", Server: "10.0.0.1",
		Description: "managed by yk-dns-manager"})
	p := newProvider(t, url)

	b := p.Batch()
	ensure(t, b, record("app.example.com", "10.0.0.1"), false)
	commit(t, b)
	if n := countCalls(fake, testenv.EndpointReconfigure); n != 1 {
		t.Fatalf("first commit reconfigured %d times, want 1", n)
	}

	b = p.Batch()
	ensure(t, b, record("app.example.com", "10.0.0.1"), false)
	commit(t, b)
	if n := countCalls(fake, testenv.EndpointReconfigure); n != 1 {
		t.Errorf("a commit with nothing owed reconfigured; total %d, want 1", n)
	}
}

// TestFailedPublishStaysOwed is audit B10 within a process: after a failed
// reconfigure the next batch finds its records already staged and writes
// nothing, but must still publish.
func TestFailedPublishStaysOwed(t *testing.T) {
	fake, url := newFake(t)
	p := newProvider(t, url)
	ctx := context.Background()

	b := p.Batch()
	ensure(t, b, record("app.example.com", "10.0.0.1"), true)
	fake.SetFailure(testenv.EndpointReconfigure, http.StatusInternalServerError)
	if err := b.Commit(ctx); err == nil {
		t.Fatal("expected Commit to fail while reconfigure fails")
	}
	if n := len(fake.Published()); n != 0 {
		t.Fatalf("published %d rows after a failed commit, want 0", n)
	}

	fake.SetFailure(testenv.EndpointReconfigure, 0)
	b = p.Batch()
	if res := ensure(t, b, record("app.example.com", "10.0.0.1"), true); res != dns.Unchanged {
		t.Fatalf("Ensure = %s, want unchanged", res)
	}
	commit(t, b)

	only(t, fake.Published(), "app.example.com")
}

// TestWriteDuringPublishStaysOwed is audit D3: a write that lands while
// another batch's reconfigure is in flight is not covered by it, so the
// writer's own commit must still publish.
func TestWriteDuringPublishStaysOwed(t *testing.T) {
	fake, url := newFake(t)
	p := newProvider(t, url)

	first := p.Batch()
	ensure(t, first, record("a.example.com", "10.0.0.1"), true)

	second := p.Batch()
	fake.DuringReconfigure(func() {
		fake.DuringReconfigure(nil)
		ensure(t, second, record("b.example.com", "10.0.0.1"), true)
	})
	commit(t, first)

	if n := len(rowsFor(fake.Published(), "b.example.com")); n != 0 {
		t.Fatal("test setup: the racing write was published by the first reconfigure")
	}
	commit(t, second)
	only(t, fake.Published(), "b.example.com")
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
