package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProviderConfig(t *testing.T) {
	content := `provider: opnsense
settings:
  base_url: "https://opnsense.local/api"
  api_key: "testkey"
  api_secret: "testsecret"
  default_ttl: "300"
`
	path := filepath.Join(t.TempDir(), "dns-provider.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadProviderConfigFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Provider != "opnsense" {
		t.Errorf("expected provider 'opnsense', got %q", cfg.Provider)
	}
	if cfg.Settings["base_url"] != "https://opnsense.local/api" {
		t.Errorf("expected base_url 'https://opnsense.local/api', got %q", cfg.Settings["base_url"])
	}
	if cfg.Settings["api_key"] != "testkey" {
		t.Errorf("expected api_key 'testkey', got %q", cfg.Settings["api_key"])
	}
	if cfg.Settings["default_ttl"] != "300" {
		t.Errorf("expected default_ttl '300', got %q", cfg.Settings["default_ttl"])
	}
}

func TestLoadProviderConfig_UpsertTrue(t *testing.T) {
	content := `provider: opnsense
upsert: true
settings:
  base_url: "https://opnsense.local/api"
  api_key: "testkey"
  api_secret: "testsecret"
`
	path := filepath.Join(t.TempDir(), "dns-provider.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadProviderConfigFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !cfg.Upsert {
		t.Error("expected Upsert to be true")
	}
}

func TestLoadProviderConfig_UpsertDefault(t *testing.T) {
	content := `provider: opnsense
settings:
  base_url: "https://opnsense.local/api"
  api_key: "testkey"
  api_secret: "testsecret"
`
	path := filepath.Join(t.TempDir(), "dns-provider.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadProviderConfigFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Upsert {
		t.Error("expected Upsert to default to false")
	}
}

func TestLoadProviderConfig_MissingProvider(t *testing.T) {
	content := `settings:
  base_url: "https://opnsense.local/api"
`
	path := filepath.Join(t.TempDir(), "dns-provider.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadProviderConfigFromPath(path)
	if err == nil {
		t.Fatal("expected error for missing provider field, got nil")
	}
}

func TestLoadProviderConfig_EnvVarExpansion(t *testing.T) {
	t.Setenv("TEST_API_KEY", "key-from-env")
	t.Setenv("TEST_API_SECRET", "secret-from-env")

	content := `provider: opnsense
settings:
  base_url: "https://opnsense.local/api"
  api_key: "${TEST_API_KEY}"
  api_secret: "${TEST_API_SECRET}"
`
	path := filepath.Join(t.TempDir(), "dns-provider.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadProviderConfigFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Settings["api_key"] != "key-from-env" {
		t.Errorf("expected api_key 'key-from-env', got %q", cfg.Settings["api_key"])
	}
	if cfg.Settings["api_secret"] != "secret-from-env" {
		t.Errorf("expected api_secret 'secret-from-env', got %q", cfg.Settings["api_secret"])
	}
	// Non-env values should remain unchanged.
	if cfg.Settings["base_url"] != "https://opnsense.local/api" {
		t.Errorf("expected base_url unchanged, got %q", cfg.Settings["base_url"])
	}
}

func TestLoadProviderConfig_EnvVarUnset(t *testing.T) {
	content := `provider: opnsense
settings:
  base_url: "https://opnsense.local/api"
  api_key: "${UNSET_VAR_THAT_DOES_NOT_EXIST}"
  api_secret: "literal-value"
`
	path := filepath.Join(t.TempDir(), "dns-provider.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadProviderConfigFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Unset env var expands to empty string.
	if cfg.Settings["api_key"] != "" {
		t.Errorf("expected api_key '' for unset env var, got %q", cfg.Settings["api_key"])
	}
	// Literal values without ${} stay as-is.
	if cfg.Settings["api_secret"] != "literal-value" {
		t.Errorf("expected api_secret 'literal-value', got %q", cfg.Settings["api_secret"])
	}
}

func TestLoadProviderConfig_MissingFile(t *testing.T) {
	_, err := LoadProviderConfigFromPath("/nonexistent/path/dns-provider.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
