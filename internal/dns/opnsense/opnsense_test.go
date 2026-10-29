package opnsense

import (
	"strings"
	"testing"

	"github.com/go-logr/logr"
)

func TestNew_ValidSettings(t *testing.T) {
	p, err := New(logr.Discard(), baseSettings("https://opnsense.local"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Stored with a trailing slash so API paths append directly.
	if p.baseURL != "https://opnsense.local/api/" {
		t.Errorf("expected baseURL 'https://opnsense.local/api/', got %q", p.baseURL)
	}
}

// TestNew_DefaultTTLIgnored: host overrides have no per-record TTL, so default_ttl never worked; it is accepted, but warns.
func TestNew_DefaultTTLIgnored(t *testing.T) {
	for _, v := range []string{"600", "notanumber"} {
		msgs := &[]string{}
		settings := baseSettings("https://opnsense.local")
		settings["default_ttl"] = v
		if _, err := New(logr.New(captureSink{msgs: msgs}), settings); err != nil {
			t.Fatalf("default_ttl %q: unexpected error: %v", v, err)
		}
		if !strings.Contains(strings.Join(*msgs, "\n"), "default_ttl is ignored") {
			t.Errorf("default_ttl %q: expected a warning, got %v", v, *msgs)
		}
	}
}

// TestNew_MissingRequiredSetting: a config missing an API credential fails startup instead of sending anonymous requests.
func TestNew_MissingRequiredSetting(t *testing.T) {
	for _, missing := range []string{"base_url", "api_key", "api_secret"} {
		settings := baseSettings("https://opnsense.local")
		delete(settings, missing)
		if _, err := New(logr.Discard(), settings); err == nil {
			t.Errorf("expected an error when %s is missing", missing)
		}
	}
}

// TestNew_PublishIntervalMustBeADuration: a nonsense or negative interval fails startup.
func TestNew_PublishIntervalMustBeADuration(t *testing.T) {
	for _, v := range []string{"hourly", "-1s"} {
		settings := baseSettings("https://opnsense.local")
		settings["publish_interval"] = v
		if _, err := New(logr.Discard(), settings); err == nil {
			t.Errorf("publish_interval %q should fail startup", v)
		}
	}
}
