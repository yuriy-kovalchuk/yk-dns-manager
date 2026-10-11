package opnsense

import (
	"crypto/tls"
	"net/http"
	"testing"

	"github.com/go-logr/logr"
)

func TestNew_ValidSettings(t *testing.T) {
	settings := map[string]string{
		"base_url":   "https://opnsense.local/api",
		"api_key":    "key123",
		"api_secret": "secret456",
	}

	p, err := New(logr.Discard(), settings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.baseURL != "https://opnsense.local/api" {
		t.Errorf("expected baseURL 'https://opnsense.local/api', got %q", p.baseURL)
	}
	if p.defaultTTL != 300 {
		t.Errorf("expected default TTL 300, got %d", p.defaultTTL)
	}
}

func TestNew_CustomTTL(t *testing.T) {
	settings := map[string]string{
		"base_url":    "https://opnsense.local/api",
		"api_key":     "key123",
		"api_secret":  "secret456",
		"default_ttl": "600",
	}

	p, err := New(logr.Discard(), settings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.defaultTTL != 600 {
		t.Errorf("expected default TTL 600, got %d", p.defaultTTL)
	}
}

func TestNew_InvalidTTL(t *testing.T) {
	settings := map[string]string{
		"base_url":    "https://opnsense.local/api",
		"api_key":     "key123",
		"api_secret":  "secret456",
		"default_ttl": "notanumber",
	}

	_, err := New(logr.Discard(), settings)
	if err == nil {
		t.Fatal("expected error for invalid default_ttl, got nil")
	}
}

func TestNew_MissingBaseURL(t *testing.T) {
	settings := map[string]string{
		"api_key":    "key123",
		"api_secret": "secret456",
	}

	_, err := New(logr.Discard(), settings)
	if err == nil {
		t.Fatal("expected error for missing base_url, got nil")
	}
}

func TestNew_MissingAPIKey(t *testing.T) {
	settings := map[string]string{
		"base_url":   "https://opnsense.local/api",
		"api_secret": "secret456",
	}

	_, err := New(logr.Discard(), settings)
	if err == nil {
		t.Fatal("expected error for missing api_key, got nil")
	}
}

func TestNew_MissingAPISecret(t *testing.T) {
	settings := map[string]string{
		"base_url": "https://opnsense.local/api",
		"api_key":  "key123",
	}

	_, err := New(logr.Discard(), settings)
	if err == nil {
		t.Fatal("expected error for missing api_secret, got nil")
	}
}

func TestNew_SkipTLSVerify(t *testing.T) {
	settings := map[string]string{
		"base_url":        "https://opnsense.local/api",
		"api_key":         "key123",
		"api_secret":      "secret456",
		"skip_tls_verify": "true",
	}

	p, err := New(logr.Discard(), settings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	transport, ok := p.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", p.client.Transport)
	}
	if transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify to be set on the transport")
	}
	if transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want TLS 1.2 (%d)", transport.TLSClientConfig.MinVersion, tls.VersionTLS12)
	}

	// A default configuration must leave verification untouched, so the system
	// root pool stays in play. Clone() always yields a non-nil TLSClientConfig,
	// so assert on the fields rather than the pointer.
	delete(settings, "skip_tls_verify")
	p, err = New(logr.Discard(), settings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	transport = p.client.Transport.(*http.Transport)
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must be false by default")
	}
	if transport.TLSClientConfig.RootCAs != nil {
		t.Error("RootCAs must be nil by default so the system roots are used")
	}
}
