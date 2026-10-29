package opnsense

import (
	"crypto/tls"
	"net/http"
	"strings"
	"testing"

	"github.com/go-logr/logr"
)

// validSettings is the smallest configuration New accepts. It is built per
// call so a test can change or drop a key.
func validSettings() map[string]string {
	return map[string]string{
		"base_url":   "https://opnsense.local/api",
		"api_key":    "key123",
		"api_secret": "secret456",
	}
}

func TestNew_ValidSettings(t *testing.T) {
	p, err := New(logr.Discard(), validSettings())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Stored with a trailing slash so API paths append directly.
	if p.baseURL != "https://opnsense.local/api/" {
		t.Errorf("expected baseURL 'https://opnsense.local/api/', got %q", p.baseURL)
	}
}

// TestNew_DefaultTTLIgnored: Unbound host overrides have no per-record TTL,
// so default_ttl never had an effect. It is still accepted, so existing configs
// keep working, but it warns.
func TestNew_DefaultTTLIgnored(t *testing.T) {
	for _, v := range []string{"600", "notanumber"} {
		msgs := &[]string{}
		settings := validSettings()
		settings["default_ttl"] = v
		if _, err := New(logr.New(captureSink{msgs: msgs}), settings); err != nil {
			t.Fatalf("default_ttl %q: unexpected error: %v", v, err)
		}
		if !strings.Contains(strings.Join(*msgs, "\n"), "default_ttl is ignored") {
			t.Errorf("default_ttl %q: expected a warning, got %v", v, *msgs)
		}
	}
}

// TestNew_MissingRequiredSetting: every credential the API call needs is
// required, so a config that omits one fails startup instead of sending
// anonymous requests to the appliance.
func TestNew_MissingRequiredSetting(t *testing.T) {
	for _, missing := range []string{"base_url", "api_key", "api_secret"} {
		settings := validSettings()
		delete(settings, missing)
		if _, err := New(logr.Discard(), settings); err == nil {
			t.Errorf("expected an error when %s is missing", missing)
		}
	}
}

func TestNew_SkipTLSVerify(t *testing.T) {
	settings := validSettings()
	settings["skip_tls_verify"] = "true"

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
