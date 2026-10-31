package providers

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	dns "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

// stubProvider satisfies dns.Provider without implementing anything; name tells which factory built it.
type stubProvider struct {
	dns.Provider
	name string
}

// sampleSettings gives a valid value for every provider settings key the factory list declares, so a key without an entry fails the test.
var sampleSettings = map[string]string{
	"base_url":            "https://opnsense.local/api",
	"api_key":             "key",
	"api_secret":          "secret",
	"ca_file":             "", // empty means unset; a real one would need a real PEM
	"skip_tls_verify":     "true",
	"allow_insecure_http": "true",
	"default_ttl":         "600",
	"publish_interval":    "5s",
}

// TestProvidersAcceptTheirOwnSettings covers the real newProviders() list, which the fake-factory tests never touch: every key a factory declares must still build. A declared key the provider stopped reading is invisible to it, which is why the list stays where the config check can see it.
func TestProvidersAcceptTheirOwnSettings(t *testing.T) {
	for _, f := range newProviders() {
		settings := map[string]string{}
		for _, key := range f.settings {
			value, ok := sampleSettings[key]
			if !ok {
				t.Errorf("provider %q declares %q, which this test has no value for", f.name, key)
				continue
			}
			settings[key] = value
		}
		if _, err := f.fn(logr.Discard(), settings); err != nil {
			t.Errorf("provider %q refused its own declared settings: %v", f.name, err)
		}
	}
}

// TestSelectProvider covers picking by name (a typo used to start whichever provider came first), returning the named provider's own error, never a typed-nil provider, and rejecting unknown settings keys (audit D11).
func TestSelectProvider(t *testing.T) {
	calls := map[string]int{}
	factory := func(name string, p dns.Provider, err error, settings ...string) providerFactory {
		return providerFactory{name: name, settings: settings, fn: func(logr.Logger, map[string]string) (dns.Provider, error) {
			calls[name]++
			return p, err
		}}
	}
	var typedNil *stubProvider
	factories := []providerFactory{
		factory("first", stubProvider{name: "first"}, nil),
		factory("second", stubProvider{name: "second"}, nil),
		factory("broken", nil, errors.New("missing required setting 'api_key'")),
		// opnsense.New returns a nil concrete pointer with its error, which is not == nil once it is a dns.Provider
		factory("typednil", typedNil, errors.New("missing base_url")),
		factory("strict", stubProvider{name: "strict"}, nil, "base_url", "skip_tls_verify"),
	}
	cases := []struct {
		name     string
		settings map[string]string
		wantErr  string
	}{
		{name: "second"},
		{name: "opnsens", wantErr: `unknown DNS provider "opnsens"; available: first, second`},
		{name: "broken", wantErr: "api_key"},
		{name: "typednil", wantErr: "missing base_url"},
		{name: "strict", settings: map[string]string{"base_url": "x", "sip_tls_verify": "true"}, wantErr: "unknown settings sip_tls_verify"},
		{name: "strict", settings: map[string]string{"base_url": "x"}},
	}
	for _, tc := range cases {
		clear(calls)
		p, err := selectProvider(logr.Discard(), factories, tc.name, tc.settings)
		for called := range calls {
			if called != tc.name {
				t.Errorf("%s: initialized %q too", tc.name, called)
			}
		}
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: error = %v, want it to mention %q", tc.name, err, tc.wantErr)
			}
			if p != nil {
				t.Errorf("%s: provider must be nil on an error so callers can guard on it", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got := p.(stubProvider).name; got != tc.name {
			t.Errorf("selected %q, want %q", got, tc.name)
		}
	}
}
