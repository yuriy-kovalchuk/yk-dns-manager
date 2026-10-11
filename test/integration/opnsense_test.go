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

// These tests pin what is specific to OPNsense: the wire format, how many
// calls a batch makes, and publishing through reconfigure. The provider
// contract itself is covered by the conformance suite (conformance_test.go).

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
	return dns.Record{Hostname: fqdn, Type: "A", Value: ip}
}

// managed is an override row the controller wrote, as seeded on the fake.
func managed(fqdn, ip string) testenv.HostOverride {
	host, domain := splitFQDN(fqdn)
	return testenv.HostOverride{Enabled: "1", Hostname: host, Domain: domain, RR: "A", Server: ip,
		Description: opnsense.ManagedDescription}
}

func splitFQDN(fqdn string) (host, domain string) {
	host, domain, _ = strings.Cut(fqdn, ".")
	return host, domain
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
	res, err := b.Ensure(context.Background(), rec, dns.EnsureOptions{Overwrite: overwrite})
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

// TestEnsureWritesTheOverrideFields pins the row OPNsense receives: the FQDN
// split into hostname and domain, and the ownership marker as description.
func TestEnsureWritesTheOverrideFields(t *testing.T) {
	fake, url := newFake(t)
	p := newProvider(t, url)

	b := p.Batch()
	ensure(t, b, record("app.example.com", "10.0.0.1"), false)
	commit(t, b)

	h := only(t, fake.Published(), "app.example.com")
	if h.Hostname != "app" || h.Domain != "example.com" {
		t.Errorf("split as %q / %q, want app / example.com", h.Hostname, h.Domain)
	}
	if h.Server != "10.0.0.1" || h.RR != "A" || h.Enabled != "1" {
		t.Errorf("row = %+v, want an enabled A record for 10.0.0.1", h)
	}
	if h.Description != opnsense.ManagedDescription {
		t.Errorf("description = %q, want the ownership marker", h.Description)
	}
}

// TestEnsureUnchangedWritesNothing is audit B2: a record that already matches
// costs one list and no write, and with nothing owed the commit does not
// reconfigure.
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

func TestEnsureSurfacesValidationFailure(t *testing.T) {
	fake, url := newFake(t)
	p := newProvider(t, url)

	// The appliance answers HTTP 200 with result "failed", so a provider that
	// only checked the status code would report success.
	if _, err := p.Batch().Ensure(context.Background(), record("app.example.com", "not-an-ip"), dns.EnsureOptions{}); err == nil {
		t.Fatal("expected Ensure to fail for a row the appliance rejects")
	}
	if n := len(fake.Snapshot()); n != 0 {
		t.Errorf("rejected row was stored: %d rows", n)
	}
}

func TestRemoveOfMissingRecordIssuesNoDelete(t *testing.T) {
	fake, url := newFake(t)
	p := newProvider(t, url)

	if err := p.Batch().Remove(context.Background(), "ghost.example.com", "A", true); err != nil {
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
	fake.Seed(managed("app.example.com", "10.0.0.1"))
	fake.Seed(managed("web.example.com", "10.0.0.1"))
	p := newProvider(t, url)
	ctx := context.Background()

	stale := p.Batch()
	if err := stale.Remove(ctx, "web.example.com", "A", false); err != nil { // loads the list
		t.Fatalf("Remove web: %v", err)
	}
	if err := p.Batch().Remove(ctx, "app.example.com", "A", false); err != nil {
		t.Fatalf("Remove app in another batch: %v", err)
	}
	if err := stale.Remove(ctx, "app.example.com", "A", false); err != nil {
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
	if err := b.Remove(context.Background(), "b.example.com", "A", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	commit(t, b)

	if n := countCalls(fake, testenv.EndpointSearch); n != 1 {
		t.Errorf("listed %d times, want 1", n)
	}
	if n := countCalls(fake, testenv.EndpointReconfigure); n != 1 {
		t.Errorf("reconfigured %d times, want 1", n)
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
	fake.Seed(managed("app.example.com", "10.0.0.1"))
	p := newProvider(t, url)

	for range 2 {
		b := p.Batch()
		ensure(t, b, record("app.example.com", "10.0.0.1"), false)
		commit(t, b)
	}
	if n := countCalls(fake, testenv.EndpointReconfigure); n != 1 {
		t.Errorf("reconfigured %d times, want exactly the one publish a new provider owes", n)
	}
}

// TestFailedPublishStaysOwed is audit B10 within a process: after a failed
// reconfigure the next batch finds its records already staged and writes
// nothing, but must still publish.
func TestFailedPublishStaysOwed(t *testing.T) {
	fake, url := newFake(t)
	p := newProvider(t, url)

	b := p.Batch()
	ensure(t, b, record("app.example.com", "10.0.0.1"), true)
	fake.SetFailure(testenv.EndpointReconfigure, http.StatusInternalServerError)
	if err := b.Commit(context.Background()); err == nil {
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
