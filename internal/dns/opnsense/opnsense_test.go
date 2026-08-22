package opnsense

import (
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

func testSettings() map[string]string {
	return map[string]string{
		"base_url": "https://opnsense.local/api",
	}
}

func testCreds() *dns.Credentials {
	return &dns.Credentials{
		SecretName: "opnsense-creds",
		Data: map[string][]byte{
			"API_KEY":    []byte("key123"),
			"API_SECRET": []byte("secret456"),
		},
	}
}

func TestNew_ValidSettings(t *testing.T) {
	p, err := New(logr.Discard(), testSettings(), testCreds())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.baseURL != "https://opnsense.local/api" {
		t.Errorf("expected baseURL 'https://opnsense.local/api', got %q", p.baseURL)
	}
	if p.apiKey != "key123" || p.apiSecret != "secret456" {
		t.Errorf("expected credentials from secret, got key=%q secret=%q", p.apiKey, p.apiSecret)
	}
}

func TestNew_MissingBaseURL(t *testing.T) {
	if _, err := New(logr.Discard(), map[string]string{}, testCreds()); err == nil {
		t.Fatal("expected error for missing base_url, got nil")
	}
}

func TestNew_NoCredentials(t *testing.T) {
	_, err := New(logr.Discard(), testSettings(), nil)
	if err == nil {
		t.Fatal("expected error when no credentials are provided, got nil")
	}
	if !strings.Contains(err.Error(), "'secret'") {
		t.Errorf("error should point at the 'secret' config field, got: %v", err)
	}
}

func TestNew_MissingCredentialKeys(t *testing.T) {
	creds := &dns.Credentials{
		SecretName: "opnsense-creds",
		Data:       map[string][]byte{"API_KEY": []byte("key123")},
	}
	_, err := New(logr.Discard(), testSettings(), creds)
	if err == nil {
		t.Fatal("expected error for missing API_SECRET key, got nil")
	}
	if !strings.Contains(err.Error(), "API_SECRET") {
		t.Errorf("error should name the missing key, got: %v", err)
	}
}

func TestNew_CredentialKeyWithWhitespace(t *testing.T) {
	// Secret values created from files often carry a trailing newline.
	creds := &dns.Credentials{
		SecretName: "opnsense-creds",
		Data: map[string][]byte{
			"API_KEY":    []byte("key123\n"),
			"API_SECRET": []byte("secret456"),
		},
	}
	p, err := New(logr.Discard(), testSettings(), creds)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.apiKey != "key123" {
		t.Errorf("expected trimmed API key, got %q", p.apiKey)
	}
}

func TestNew_SkipTLSVerify(t *testing.T) {
	settings := testSettings()
	settings["skip_tls_verify"] = "true"

	p, err := New(logr.Discard(), settings, testCreds())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.client == nil {
		t.Fatal("expected non-nil HTTP client")
	}
}
