package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

func TestEndpointOf(t *testing.T) {
	cases := map[string]string{
		"/api/unbound/settings/searchHostOverride":                                   "searchHostOverride",
		"/api/unbound/settings/setHostOverride/3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5b": "setHostOverride",
		"/api/hosts/42":                     "hosts",
		"/api/records/0123456789abcdef0123": "records",
		"/":                                 "/",
	}
	for path, want := range cases {
		if got := endpointOf(path); got != want {
			t.Errorf("endpointOf(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestInstrumentTransportCountsRequests: every request is counted by endpoint and status code and timed, and one that got no response counts as "error".
func TestInstrumentTransportCountsRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/denied" {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	client := &http.Client{Transport: InstrumentTransport("test")(http.DefaultTransport)}
	count := func(endpoint, code string) float64 {
		return testutil.ToFloat64(ProviderRequests.WithLabelValues("test", endpoint, code))
	}
	ok, denied, unreachable := count("items", "200"), count("denied", "401"), count("items", "error")
	timed := timedRequests(t, "items")

	for _, path := range []string{"/api/items/12", "/api/denied"} {
		if err := get(client, srv.URL+path); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
	}
	srv.Close()
	if err := get(client, srv.URL+"/api/items"); err == nil {
		t.Fatal("expected a request to a closed server to fail")
	}

	for name, d := range map[string]float64{
		"items 200":   count("items", "200") - ok,
		"denied 401":  count("denied", "401") - denied,
		"items error": count("items", "error") - unreachable,
	} {
		if d != 1 {
			t.Errorf("%s rose by %v, want 1", name, d)
		}
	}
	if d := timedRequests(t, "items") - timed; d != 2 {
		t.Errorf("timed %d items requests, want 2, the failed one included", d)
	}
}

func get(client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func timedRequests(t *testing.T, endpoint string) uint64 {
	t.Helper()
	var m dto.Metric
	if err := ProviderRequestDuration.WithLabelValues("test", endpoint).(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("reading the histogram: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}
