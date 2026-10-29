package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemp writes content to a fresh file and returns its path.
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadProviderConfig(t *testing.T, content string) (*ProviderConfig, error) {
	t.Helper()
	return LoadProviderConfigFromPath(writeTemp(t, content))
}

func TestLoadProviderConfig(t *testing.T) {
	cfg, err := loadProviderConfig(t, `provider: opnsense
settings:
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
	want := map[string]string{"base_url": "https://opnsense.local/api", "api_key": "testkey", "default_ttl": "300"}
	for k, v := range want {
		if cfg.Settings[k] != v {
			t.Errorf("settings.%s = %q, want %q", k, cfg.Settings[k], v)
		}
	}
}

func TestLoadProviderConfig_UpsertTrue(t *testing.T) {
	cfg, err := loadProviderConfig(t, "provider: opnsense\nupsert: true\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Upsert {
		t.Error("expected Upsert to be true")
	}
}

func TestLoadProviderConfig_Rejected(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		contains string
	}{
		{name: "missing provider", content: "settings:\n  base_url: \"https://opnsense.local/api\"\n", contains: "provider"},
		// an unset variable used to expand to "" and hide a missing Secret key
		{name: "unset variable", content: "provider: opnsense\nsettings:\n  api_key: \"${UNSET_VAR_THAT_DOES_NOT_EXIST}\"\n", contains: "settings.api_key: environment variable UNSET_VAR_THAT_DOES_NOT_EXIST"},
		// audit D11: a misspelled top-level field used to be ignored, leaving its default in place
		{name: "unknown field", content: "provider: opnsense\nupsrt: true\n", contains: "upsrt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadProviderConfig(t, tc.content); err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.contains)
			}
		})
	}
}

func TestLoadProviderConfig_EnvVarExpansion(t *testing.T) {
	t.Setenv("TEST_API_KEY", "key-from-env")
	t.Setenv("word", "SHOULD-NOT-APPEAR")

	cfg, err := loadProviderConfig(t, `provider: opnsense
settings:
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

func TestLoadProviderConfig_MissingFile(t *testing.T) {
	if _, err := LoadProviderConfigFromPath("/nonexistent/path/dns-provider.yaml"); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
