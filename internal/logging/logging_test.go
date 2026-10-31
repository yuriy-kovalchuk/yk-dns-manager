package logging

import (
	"os"
	"strings"
	"testing"

	"go.uber.org/zap/zapcore"
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
		got, err := ParseLevel(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseLevel(%q): expected an error, got level %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseLevel(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestDebugLevelEnablesVerbosity pins what the flag promises: debug used to arrive only as a side effect of zap Development mode, which also changed the encoder and made DPanic fatal.
func TestDebugLevelEnablesVerbosity(t *testing.T) {
	level, err := ParseLevel("debug")
	if err != nil {
		t.Fatalf("ParseLevel: %v", err)
	}

	debugLogger := New(level)
	if !debugLogger.V(1).Enabled() {
		t.Error("V(1) is not enabled at debug level")
	}

	infoLogger := New(zapcore.InfoLevel)
	if infoLogger.V(1).Enabled() {
		t.Error("V(1) is enabled at info level")
	}
	if !infoLogger.Enabled() {
		t.Error("plain Info should be enabled at info level")
	}
}

// TestDebugLevelKeepsProductionEncoder is the other half of that fix: asking for debug must not switch on zap development mode inside a cluster.
func TestDebugLevelKeepsProductionEncoder(t *testing.T) {
	level, err := ParseLevel("debug")
	if err != nil {
		t.Fatalf("ParseLevel: %v", err)
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

	New(level).V(1).Info("probe")

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
