package opnsense

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

// TestResponseBodyLimit covers the unbounded reads the provider used to do.
// Every response body was read in full while the pod carries a 256Mi memory
// limit, so an appliance (or anything answering for it) could OOM the controller
// by streaming a large body.
func TestResponseBodyLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Past the cap, so the read stops at the limit rather than buffering the
		// whole thing.
		_, _ = io.WriteString(w, strings.Repeat("a", maxResponseBytes+4096))
	}))
	defer srv.Close()

	p, err := New(logr.Discard(), httpSettings(srv.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = p.Exists(context.Background(), "app.example.com", "A")
	if err == nil {
		t.Fatal("expected an oversized body to be rejected")
	}
	if !strings.Contains(err.Error(), "exceeded the") {
		t.Errorf("error %q does not name the byte limit", err.Error())
	}
}

// TestErrorBodyTruncated checks that a failing status does not embed an
// arbitrarily large body in the error, which then reaches the logs.
func TestErrorBodyTruncated(t *testing.T) {
	const padding = "PADPAD"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat(padding, 4096))
	}))
	defer srv.Close()

	p, err := New(logr.Discard(), httpSettings(srv.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = p.Create(context.Background(), dns.Record{Hostname: "app.example.com", Type: "A", Value: "10.0.0.1"})
	if err == nil {
		t.Fatal("expected a 500 to be reported")
	}
	if !strings.Contains(err.Error(), "addHostOverride returned status 500") {
		t.Errorf("error %q does not name the failing endpoint", err.Error())
	}
	if len(err.Error()) > 2*maxErrorBodyBytes {
		t.Errorf("error carries %d bytes, want it bounded by %d", len(err.Error()), maxErrorBodyBytes)
	}
}

// httpSettings is baseSettings for a plain-HTTP test server, which the
// provider only accepts with allow_insecure_http.
func httpSettings(baseURL string) map[string]string {
	s := baseSettings(baseURL)
	s["allow_insecure_http"] = "true"
	return s
}
