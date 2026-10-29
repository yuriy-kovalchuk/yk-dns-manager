package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"

	dns "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

func TestParseLogLevel(t *testing.T) {
	cases := []struct {
		in      string
		want    zapcore.Level
		wantErr bool
	}{
		{in: "", want: zapcore.InfoLevel},
		{in: "debug", want: zapcore.DebugLevel},
		{in: "info", want: zapcore.InfoLevel},
		{in: "warn", want: zapcore.WarnLevel},
		{in: "error", want: zapcore.ErrorLevel},
		{in: "DEBUG", want: zapcore.DebugLevel},
		{in: "  info  ", want: zapcore.InfoLevel},
		// numeric levels map straight through, which is how an operator reaches logr V(3): zapcore.Level(-3)
		{in: "-3", want: zapcore.Level(-3)},
		{in: "-200", wantErr: true},
		{in: "chatty", wantErr: true},
	}

	for _, tc := range cases {
		got, err := parseLogLevel(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseLogLevel(%q): expected an error, got level %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseLogLevel(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestDebugLevelEnablesVerbosity pins what the flag promises: debug used to arrive only as a side effect of zap Development mode, which also changed the encoder and made DPanic fatal.
func TestDebugLevelEnablesVerbosity(t *testing.T) {
	level, err := parseLogLevel("debug")
	if err != nil {
		t.Fatalf("parseLogLevel: %v", err)
	}

	debugLogger := newLogger(level)
	if !debugLogger.V(1).Enabled() {
		t.Error("V(1) is not enabled at debug level")
	}

	infoLogger := newLogger(zapcore.InfoLevel)
	if infoLogger.V(1).Enabled() {
		t.Error("V(1) is enabled at info level")
	}
	if !infoLogger.Enabled() {
		t.Error("plain Info should be enabled at info level")
	}
}

// TestDebugLevelKeepsProductionEncoder is the other half of that fix: asking for debug must not switch on zap development mode inside a cluster.
func TestDebugLevelKeepsProductionEncoder(t *testing.T) {
	level, err := parseLogLevel("debug")
	if err != nil {
		t.Fatalf("parseLogLevel: %v", err)
	}

	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatalf("temp log: %v", err)
	}
	orig := os.Stderr
	os.Stderr = f
	defer func() {
		os.Stderr = orig
		_ = f.Close()
	}()

	newLogger(level).V(1).Info("probe")

	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, "{") {
		t.Errorf("debug emitted a development-mode line, want JSON: %q", line)
	}
	if !strings.Contains(line, `"probe"`) {
		t.Errorf("V(1) message missing from output: %q", line)
	}
}

// stubProvider satisfies dns.Provider without implementing anything; only the interface conversion matters here.
type stubProvider struct {
	dns.Provider
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

func TestSelectProviderPicksByName(t *testing.T) {
	var called []string
	factory := func(name string) providerFactory {
		return providerFactory{name: name, fn: func(logr.Logger, map[string]string) (dns.Provider, error) {
			called = append(called, name)
			return stubProvider{}, nil
		}}
	}

	p, err := selectProvider(logr.Discard(), []providerFactory{factory("first"), factory("second")}, "second", nil)
	if err != nil {
		t.Fatalf("selectProvider: %v", err)
	}
	if p == nil {
		t.Fatal("provider is nil despite a working factory")
	}
	if len(called) != 1 || called[0] != "second" {
		t.Errorf("initialized %v, want only the named provider", called)
	}
}

// TestSelectProviderRejectsUnknownName covers a typo that used to start whichever provider came first and accepted the settings.
func TestSelectProviderRejectsUnknownName(t *testing.T) {
	factories := []providerFactory{
		{name: "opnsense", fn: func(logr.Logger, map[string]string) (dns.Provider, error) {
			return stubProvider{}, nil
		}},
	}

	p, err := selectProvider(logr.Discard(), factories, "opnsens", nil)
	if err == nil {
		t.Fatal("expected an error for an unknown provider name")
	}
	if !strings.Contains(err.Error(), `"opnsens"`) || !strings.Contains(err.Error(), "opnsense") {
		t.Errorf("error = %v, want it to name the bad value and the available providers", err)
	}
	if p != nil {
		t.Error("provider must be nil so callers can guard on it")
	}
}

// TestSelectProviderReturnsInitError makes the named provider's own error the startup error, where it used to be logged at info behind a generic "no DNS provider".
func TestSelectProviderReturnsInitError(t *testing.T) {
	factories := []providerFactory{
		{name: "opnsense", fn: func(logr.Logger, map[string]string) (dns.Provider, error) {
			return nil, errors.New("missing required setting 'api_key'")
		}},
	}

	_, err := selectProvider(logr.Discard(), factories, "opnsense", nil)
	if err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("error = %v, want the provider's own error", err)
	}
}

// TestSelectProviderDoesNotKeepTypedNilProvider mirrors opnsense.New returning a nil concrete pointer: converted to dns.Provider that is not == nil, so storing it before the error check yields a provider that panics on first use.
func TestSelectProviderDoesNotKeepTypedNilProvider(t *testing.T) {
	factories := []providerFactory{
		{name: "typednil", fn: func(logr.Logger, map[string]string) (dns.Provider, error) {
			var p *stubProvider
			return p, errors.New("missing base_url")
		}},
	}

	p, err := selectProvider(logr.Discard(), factories, "typednil", nil)
	if err == nil {
		t.Fatal("expected the factory error to be returned")
	}
	if p != nil {
		t.Error("a typed-nil provider was returned; callers guard with == nil")
	}
}

// TestSelectProviderRejectsUnknownSettings is audit D11: a misspelled settings key used to be ignored, so the setting silently had no effect.
func TestSelectProviderRejectsUnknownSettings(t *testing.T) {
	factories := []providerFactory{{
		name:     "opnsense",
		settings: []string{"base_url", "skip_tls_verify"},
		fn: func(logr.Logger, map[string]string) (dns.Provider, error) {
			return stubProvider{}, nil
		},
	}}

	_, err := selectProvider(logr.Discard(), factories, "opnsense", map[string]string{"base_url": "x", "sip_tls_verify": "true"})
	if err == nil || !strings.Contains(err.Error(), "sip_tls_verify") {
		t.Fatalf("error = %v, want it to name the unknown key", err)
	}

	if _, err := selectProvider(logr.Discard(), factories, "opnsense", map[string]string{"base_url": "x"}); err != nil {
		t.Fatalf("known settings rejected: %v", err)
	}
}
