package opnsense

import (
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
	if p.client == nil {
		t.Fatal("expected non-nil HTTP client")
	}
}
