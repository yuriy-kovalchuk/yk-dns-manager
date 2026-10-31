package opnsense

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/metrics"
)

// These tests pin what is specific to OPNsense: the wire format, calls per batch, and how the appliance's answers are read. The provider contract is pinned by the conformance suite in appliance_test.go.

// stub answers the Unbound API with canned replies: search returns rows, every write answers reply[endpoint] or succeeds. It keeps no state, so each test states exactly what the appliance says.
type stub struct {
	rows  []hostRow
	reply map[string]string

	mu    sync.Mutex
	calls []string
	added []hostRow
}

func newStub(t *testing.T, rows ...hostRow) (*stub, *Provider) {
	t.Helper()
	s := &stub{rows: rows, reply: map[string]string{}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	p, err := New(logr.Discard(), httpSettings(srv.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, p
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	endpoint := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/unbound/"), "/")[1]
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, r.Method+" "+endpoint)

	if key, secret, _ := r.BasicAuth(); key != "key123" || secret != "secret456" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if reply, ok := s.reply[endpoint]; ok {
		_, _ = w.Write([]byte(reply))
		return
	}
	switch endpoint {
	case "searchHostOverride":
		_ = json.NewEncoder(w).Encode(map[string]any{"rows": s.rows})
	case "addHostOverride":
		var body struct{ Host hostRow }
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.added = append(s.added, body.Host)
		_, _ = w.Write([]byte(`{"result":"saved","uuid":"new"}`))
	case "setHostOverride":
		_, _ = w.Write([]byte(`{"result":"saved"}`))
	case "delHostOverride":
		_, _ = w.Write([]byte(`{"result":"deleted"}`))
	case "reconfigure":
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}

// count returns how many requests went to endpoint.
func (s *stub) count(endpoint string) int {
	calls, _ := s.requests()
	n := 0
	for _, c := range calls {
		if strings.HasSuffix(c, " "+endpoint) {
			n++
		}
	}
	return n
}

// requests returns the calls so far as "METHOD endpoint", and the rows added.
func (s *stub) requests() ([]string, []hostRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls), slices.Clone(s.added)
}

func (s *stub) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
}

// managedRow is an override the controller wrote, as the appliance lists it.
func managedRow(uuid, hostname, ip string) hostRow {
	return hostRow{UUID: uuid, Enabled: "1", Hostname: hostname, Domain: "example.com", RR: "A", Server: ip, Description: ManagedDescription}
}

func aRecord(fqdn, ip string) dns.Record {
	return dns.Record{Hostname: fqdn, Type: "A", Value: ip}
}

func mustEnsure(t *testing.T, b dns.Batch, rec dns.Record, overwrite bool) dns.Result {
	t.Helper()
	res, err := b.Ensure(context.Background(), rec, dns.EnsureOptions{Overwrite: overwrite})
	if err != nil {
		t.Fatalf("Ensure %s: %v", rec.Hostname, err)
	}
	return res
}

func mustCommit(t *testing.T, b dns.Batch) {
	t.Helper()
	if err := b.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// TestEnsureWritesTheOverrideFields pins the row OPNsense receives: the FQDN split into hostname and domain, and the ownership marker naming the owner as description.
func TestEnsureWritesTheOverrideFields(t *testing.T) {
	s, p := newStub(t)
	rec := aRecord("app.example.com", "10.0.0.1")
	rec.Owner = "httproute/default/app"
	mustEnsure(t, p.Batch(), rec, false)

	want := hostRow{Enabled: "1", Hostname: "app", Domain: "example.com", RR: "A", Server: "10.0.0.1",
		Description: ManagedDescription + " [owners: httproute/default/app]"}
	if _, added := s.requests(); len(added) != 1 || added[0] != want {
		t.Fatalf("added %+v, want %+v", added, want)
	}
}

// TestEnsureUnchangedWritesNothing is audit B2: a record that already matches costs one list, no write, and no reconfigure.
func TestEnsureUnchangedWritesNothing(t *testing.T) {
	for _, overwrite := range []bool{false, true} {
		s, p := newStub(t, managedRow("u1", "app", "10.0.0.1"))
		// the first commit publishes whatever happens: a new process owes one
		b := p.Batch()
		mustEnsure(t, b, aRecord("app.example.com", "10.0.0.1"), overwrite)
		mustCommit(t, b)
		s.reset()

		b = p.Batch()
		if res := mustEnsure(t, b, aRecord("app.example.com", "10.0.0.1"), overwrite); res != dns.Unchanged {
			t.Fatalf("overwrite=%v: Ensure = %s, want unchanged", overwrite, res)
		}
		mustCommit(t, b)
		if calls, _ := s.requests(); !slices.Equal(calls, []string{"GET searchHostOverride"}) {
			t.Errorf("overwrite=%v: calls = %v, want one list", overwrite, calls)
		}
	}
}

// TestEnsureSurfacesValidationFailure: the appliance rejects a row with HTTP 200 and result "failed", so a provider checking only the status code would report success.
func TestEnsureSurfacesValidationFailure(t *testing.T) {
	s, p := newStub(t)
	s.reply["addHostOverride"] = `{"result":"failed","validations":{"host.domain":"A domain is required."}}`

	_, err := p.Batch().Ensure(context.Background(), aRecord("app", "10.0.0.1"), dns.EnsureOptions{})
	if err == nil || !strings.Contains(err.Error(), "host.domain: A domain is required.") {
		t.Fatalf("Ensure = %v, want the appliance's complaint", err)
	}
}

func TestRemoveOfMissingRecordIssuesNoDelete(t *testing.T) {
	s, p := newStub(t)
	if err := p.Batch().Remove(context.Background(), "ghost.example.com", "A", "route/app", true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if n := s.count("delHostOverride"); n != 0 {
		t.Errorf("issued %d deletes for a record that does not exist", n)
	}
}

// TestRemoveToleratesVanishedRecord: the batch works from the list it read first, so a row someone else deleted answers "not found", which is what Remove wanted.
func TestRemoveToleratesVanishedRecord(t *testing.T) {
	s, p := newStub(t, managedRow("u1", "app", "10.0.0.1"))
	s.reply["delHostOverride"] = `{"result":"not found"}`
	if err := p.Batch().Remove(context.Background(), "app.example.com", "A", "route/app", false); err != nil {
		t.Fatalf("Remove of an already deleted row: %v", err)
	}
}

// TestBatchListsOnce is audit D4: however many records a batch touches, it reads the appliance's list once and publishes once.
func TestBatchListsOnce(t *testing.T) {
	s, p := newStub(t, managedRow("u1", "b", "10.0.0.1"))
	b := p.Batch()
	mustEnsure(t, b, aRecord("a.example.com", "10.0.0.1"), true)
	mustEnsure(t, b, aRecord("c.example.com", "10.0.0.1"), true)
	if err := b.Remove(context.Background(), "b.example.com", "A", "route/app", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	mustCommit(t, b)

	if n := s.count("searchHostOverride"); n != 1 {
		t.Errorf("listed %d times, want 1", n)
	}
	if n := s.count("reconfigure"); n != 1 {
		t.Errorf("reconfigured %d times, want 1", n)
	}
}

// TestUnusedBatchCommitsNothing: a reconcile that never touched DNS must not reach the appliance, even with a publish owed.
func TestUnusedBatchCommitsNothing(t *testing.T) {
	s, p := newStub(t)
	mustCommit(t, p.Batch())
	if calls, _ := s.requests(); len(calls) != 0 {
		t.Errorf("an unused batch made calls: %v", calls)
	}
}

func TestHealthCheckRejectsWrongCredentials(t *testing.T) {
	_, p := newStub(t)
	p.apiKey = "other-key"
	err := p.HealthCheck(context.Background())
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("HealthCheck = %v, want an authentication failure", err)
	}
}

// TestRequestsAreCounted: every API request is counted by endpoint and status code, a uuid in the path is not part of the endpoint, and a request that got no answer counts as "error".
func TestRequestsAreCounted(t *testing.T) {
	count := func(endpoint, code string) float64 {
		return testutil.ToFloat64(metrics.ProviderRequests.WithLabelValues("opnsense", endpoint, code))
	}
	search, set, denied, unreachable := count("searchHostOverride", "200"), count("setHostOverride", "200"), count("searchHostOverride", "401"), count("searchHostOverride", "error")

	_, p := newStub(t, managedRow("u1", "app", "10.0.0.1"))
	mustEnsure(t, p.Batch(), aRecord("app.example.com", "10.0.0.2"), true)
	p.apiKey = "wrong"
	_ = p.HealthCheck(context.Background())
	p.baseURL = "http://127.0.0.1:1/api/"
	_ = p.HealthCheck(context.Background())

	for name, d := range map[string]float64{
		"search 200":   count("searchHostOverride", "200") - search,
		"set 200":      count("setHostOverride", "200") - set,
		"search 401":   count("searchHostOverride", "401") - denied,
		"search error": count("searchHostOverride", "error") - unreachable,
	} {
		if d != 1 {
			t.Errorf("%s rose by %v, want 1", name, d)
		}
	}
}
