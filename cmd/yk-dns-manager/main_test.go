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
		// Numeric levels map straight through, which is how an operator reaches
		// logr V(3): zapcore.Level(-3).
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

// TestDebugLevelEnablesVerbosity pins the user-visible behaviour the flag
// promises. The old code never set zap.Options.Level, so debug output arrived
// only as a side effect of Development mode, which also switched the encoder to
// console and turned DPanic into a real panic.
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

// TestDebugLevelKeepsProductionEncoder asserts the other half of the fix: asking
// for debug must not switch on zap development mode, whose console encoder and
// DPanic-panics behaviour are wrong for a pod in a cluster.
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

// stubProvider satisfies dns.Provider without implementing anything. Only the
// interface conversion matters here, and embedding the interface keeps that
// from turning the test into a mock.
type stubProvider struct {
	dns.Provider
}

func TestSelectProviderSkipsFailedFactories(t *testing.T) {
	factories := []providerFactory{
		{"broken", func(logr.Logger, map[string]string) (dns.Provider, error) {
			return nil, errors.New("bad settings")
		}},
		{"good", func(logr.Logger, map[string]string) (dns.Provider, error) {
			return stubProvider{}, nil
		}},
	}

	p, name, err := selectProvider(logr.Discard(), factories, nil)
	if err != nil {
		t.Fatalf("selectProvider: %v", err)
	}
	if name != "good" {
		t.Errorf("selected %q, want %q", name, "good")
	}
	if p == nil {
		t.Error("provider is nil despite a working factory")
	}
}

func TestSelectProviderFailsWhenNoneInitialize(t *testing.T) {
	factories := []providerFactory{
		{"broken", func(logr.Logger, map[string]string) (dns.Provider, error) {
			return nil, errors.New("bad settings")
		}},
	}

	p, name, err := selectProvider(logr.Discard(), factories, nil)
	if err == nil {
		t.Fatal("expected an error when every factory fails")
	}
	if name != "" {
		t.Errorf("selected %q, want no provider", name)
	}
	if p != nil {
		t.Error("provider must be nil so callers can guard on it")
	}
}

// A factory that returns a nil concrete pointer mirrors opnsense.New's shape.
// Converted to dns.Provider that value is not == nil, so anything that stores it
// before checking the error hands the caller a provider that panics on first use
// instead of reporting the configuration error.
func TestSelectProviderDoesNotKeepTypedNilProvider(t *testing.T) {
	factories := []providerFactory{
		{"typednil", func(logr.Logger, map[string]string) (dns.Provider, error) {
			var p *stubProvider
			return p, errors.New("missing base_url")
		}},
	}

	p, _, err := selectProvider(logr.Discard(), factories, nil)
	if err == nil {
		t.Fatal("expected the factory error to be returned")
	}
	if p != nil {
		t.Error("a typed-nil provider was returned; callers guard with == nil")
	}
}
