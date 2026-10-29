package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimal is the smallest config Load accepts, for tests to append to.
const minimal = "provider: opnsense\ndomains:\n  \"*.example.com\": 10.0.0.1\n"

func load(t *testing.T, content string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestLoad(t *testing.T) {
	cfg, err := load(t, minimal+`settings:
  base_url: "https://opnsense.local/api"
  api_key: "testkey"
  default_ttl: "300"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Provider != "opnsense" {
		t.Errorf("provider = %q, want opnsense", cfg.Provider)
	}
	if cfg.Upsert {
		t.Error("expected Upsert to default to false")
	}
	if ip, ok := cfg.DomainMap.LookupIP("app.example.com"); !ok || ip != "10.0.0.1" {
		t.Errorf("app.example.com = %q, %v; want 10.0.0.1", ip, ok)
	}
	want := map[string]string{"base_url": "https://opnsense.local/api", "api_key": "testkey", "default_ttl": "300"}
	for k, v := range want {
		if cfg.Settings[k] != v {
			t.Errorf("settings.%s = %q, want %q", k, cfg.Settings[k], v)
		}
	}
}

func TestLoad_UpsertTrue(t *testing.T) {
	cfg, err := load(t, minimal+"upsert: true\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Upsert {
		t.Error("expected Upsert to be true")
	}
}

func TestLoad_Rejected(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		contains string
	}{
		{name: "empty file", content: "", contains: "missing required field 'provider'"},
		{name: "missing provider", content: "domains:\n  \"*.example.com\": 10.0.0.1\n", contains: "missing required field 'provider'"},
		{name: "missing domains", content: "provider: opnsense\n", contains: "no entries"},
		{name: "invalid domain entry", content: "provider: opnsense\ndomains:\n  a.com: not-an-ip\n", contains: "not a valid IP address"},
		// yaml.v3 rejects this already; asserted so a dependency change cannot start letting the last entry win
		{name: "duplicate domain keys", content: "provider: opnsense\ndomains:\n  a.com: 10.0.0.1\n  a.com: 10.0.0.2\n", contains: "already defined"},
		{name: "domains not a mapping", content: "provider: opnsense\ndomains:\n  - a\n", contains: "parsing config file"},
		// an unset variable used to expand to "" and hide a missing Secret key
		{name: "unset variable", content: minimal + "settings:\n  api_key: \"${UNSET_VAR_THAT_DOES_NOT_EXIST}\"\n", contains: "settings.api_key: environment variable UNSET_VAR_THAT_DOES_NOT_EXIST"},
		// audit D11: a misspelled top-level field used to be ignored, leaving its default in place
		{name: "unknown field", content: minimal + "upsrt: true\n", contains: "upsrt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := load(t, tc.content); err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.contains)
			}
		})
	}
}

func TestLoad_EnvVarExpansion(t *testing.T) {
	t.Setenv("TEST_API_KEY", "key-from-env")
	t.Setenv("word", "SHOULD-NOT-APPEAR")

	cfg, err := load(t, minimal+`settings:
  base_url: "https://opnsense.local/api"
  api_key: "${TEST_API_KEY}"
  api_secret: "pa$word and $5"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{
		"api_key":  "key-from-env",
		"base_url": "https://opnsense.local/api",
		// only ${NAME} is expanded, so a "$" in a literal value survives where os.ExpandEnv turned "pa$word" into "pa"
		"api_secret": "pa$word and $5",
	}
	for k, v := range want {
		if cfg.Settings[k] != v {
			t.Errorf("settings.%s = %q, want %q", k, cfg.Settings[k], v)
		}
	}
}

func TestLoad_MissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/path/config.yaml"); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
