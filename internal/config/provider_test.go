package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_Providers(t *testing.T) {
	content := `secretsNamespace: xk
providers:
  opnsense:
    secret: opnsense-creds
    settings:
      base_url: "https://opnsense.local/api"
      skip_tls_verify: "false"
  pihole:
    provider: pihole
    upsert: true
    secret: pihole-creds
    settings:
      base_url: "https://pihole.local"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfigFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cfg.Providers) != 2 {
		t.Fatalf("expected 2 provider instances, got %d", len(cfg.Providers))
	}
	if cfg.SecretsNamespace != "xk" {
		t.Errorf("expected secretsNamespace 'xk', got %q", cfg.SecretsNamespace)
	}

	os := cfg.Providers["opnsense"]
	if os.Provider != "" {
		t.Errorf("expected Provider type to default to empty (resolved to instance name), got %q", os.Provider)
	}
	if os.Secret != "opnsense-creds" {
		t.Errorf("expected secret 'opnsense-creds', got %q", os.Secret)
	}
	if os.Settings["base_url"] != "https://opnsense.local/api" {
		t.Errorf("expected base_url 'https://opnsense.local/api', got %q", os.Settings["base_url"])
	}
	if os.Settings["skip_tls_verify"] != "false" {
		t.Errorf("expected skip_tls_verify 'false', got %q", os.Settings["skip_tls_verify"])
	}

	ph := cfg.Providers["pihole"]
	if ph.Provider != "pihole" {
		t.Errorf("expected Provider type 'pihole', got %q", ph.Provider)
	}
	if ph.Secret != "pihole-creds" {
		t.Errorf("expected pihole secret 'pihole-creds', got %q", ph.Secret)
	}
	if !ph.Upsert {
		t.Error("expected pihole Upsert to be true")
	}
}

func TestLoadConfig_ProvidersUpsertDefault(t *testing.T) {
	content := `providers:
  opnsense:
    settings:
      base_url: "https://opnsense.local/api"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfigFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Providers["opnsense"].Upsert {
		t.Error("expected Upsert to default to false")
	}
}

func TestLoadConfig_EmptyProviders(t *testing.T) {
	for name, content := range map[string]string{
		"empty file":      "",
		"empty providers": "providers: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfigFromPath(path)
			if err != nil {
				t.Fatalf("empty provider list must be valid, got error: %v", err)
			}
			if len(cfg.Providers) != 0 {
				t.Fatalf("expected 0 instances, got %d", len(cfg.Providers))
			}
		})
	}
}

func TestLoadConfig_OldFormatRejected(t *testing.T) {
	// The old single-provider format must fail loudly (strict decode),
	// not silently become an empty no-op config.
	content := `provider: opnsense
settings:
  base_url: "https://opnsense.local/api"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfigFromPath(path); err == nil {
		t.Fatal("expected error for old-format config, got nil")
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	_, err := LoadConfigFromPath("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
