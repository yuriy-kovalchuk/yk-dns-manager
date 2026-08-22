package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	content := `domainMap:
  my-domain1.com: 10.0.8.100
  my-domain2.it: 10.0.9.50
providers:
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

	if len(cfg.DomainMap.Domains()) != 2 {
		t.Fatalf("expected 2 domains, got %d", len(cfg.DomainMap.Domains()))
	}
	if ip, ok := cfg.DomainMap.LookupIP("app.my-domain1.com"); !ok || ip != "10.0.8.100" {
		t.Fatalf("expected LookupIP(app.my-domain1.com)=10.0.8.100, got %q ok=%v", ip, ok)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("expected 1 provider instance, got %d", len(cfg.Providers))
	}
}

func TestLoadConfig_EmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFromPath(path)
	if err != nil {
		t.Fatalf("empty config must be valid, got error: %v", err)
	}
	if len(cfg.DomainMap.Domains()) != 0 || len(cfg.Providers) != 0 {
		t.Fatalf("expected empty config, got %d domains and %d providers", len(cfg.DomainMap.Domains()), len(cfg.Providers))
	}
}

func TestLookupIP(t *testing.T) {
	dm := &DomainMap{entries: map[string]string{
		"my-domain1.com": "10.0.8.100",
		"my-domain2.it":  "10.0.9.50",
	}}

	tests := []struct {
		hostname string
		wantIP   string
		wantOK   bool
	}{
		{"app.my-domain1.com", "10.0.8.100", true},
		{"deep.nested.my-domain1.com", "10.0.8.100", true},
		{"my-domain1.com", "10.0.8.100", true},
		{"service.my-domain2.it", "10.0.9.50", true},
		{"app.my-domain1.com.", "10.0.8.100", true}, // trailing dot (FQDN)
		{"unknown.com", "", false},
		{"notmydomain.com", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.hostname, func(t *testing.T) {
			ip, ok := dm.LookupIP(tt.hostname)
			if ok != tt.wantOK {
				t.Errorf("LookupIP(%q): got ok=%v, want %v", tt.hostname, ok, tt.wantOK)
			}
			if ip != tt.wantIP {
				t.Errorf("LookupIP(%q): got ip=%q, want %q", tt.hostname, ip, tt.wantIP)
			}
		})
	}
}

func TestLookupIPWildcard(t *testing.T) {
	dm := &DomainMap{entries: map[string]string{
		"*.mydomain.com":    "10.0.0.1",
		"app2.mydomain.com": "10.0.0.2",
	}}

	tests := []struct {
		hostname string
		wantIP   string
		wantOK   bool
	}{
		{"app1.mydomain.com", "10.0.0.1", true},        // wildcard match
		{"app2.mydomain.com", "10.0.0.2", true},        // exact match wins over wildcard
		{"app3.mydomain.com", "10.0.0.1", true},        // wildcard match
		{"deep.nested.mydomain.com", "10.0.0.1", true}, // wildcard match (walks up)
		{"mydomain.com", "", false},                    // wildcard does not match bare domain
		{"other.com", "", false},                       // no match
	}

	for _, tt := range tests {
		t.Run(tt.hostname, func(t *testing.T) {
			ip, ok := dm.LookupIP(tt.hostname)
			if ok != tt.wantOK {
				t.Errorf("LookupIP(%q): got ok=%v, want %v", tt.hostname, ok, tt.wantOK)
			}
			if ip != tt.wantIP {
				t.Errorf("LookupIP(%q): got ip=%q, want %q", tt.hostname, ip, tt.wantIP)
			}
		})
	}
}

func TestLookupIPWildcardWithBaseDomain(t *testing.T) {
	dm := &DomainMap{entries: map[string]string{
		"*.mydomain.com":       "10.0.0.1",
		"app2.mydomain.com":    "10.0.0.2",
		"mydomain.com":         "10.0.0.3",
		"*.other.mydomain.com": "10.0.0.4",
	}}

	tests := []struct {
		hostname string
		wantIP   string
		wantOK   bool
	}{
		{"app1.mydomain.com", "10.0.0.1", true},      // wildcard *.mydomain.com
		{"app2.mydomain.com", "10.0.0.2", true},      // exact wins
		{"mydomain.com", "10.0.0.3", true},           // exact base domain
		{"foo.other.mydomain.com", "10.0.0.4", true}, // wildcard *.other.mydomain.com
		{"other.mydomain.com", "10.0.0.1", true},     // wildcard *.mydomain.com
	}

	for _, tt := range tests {
		t.Run(tt.hostname, func(t *testing.T) {
			ip, ok := dm.LookupIP(tt.hostname)
			if ok != tt.wantOK {
				t.Errorf("LookupIP(%q): got ok=%v, want %v", tt.hostname, ok, tt.wantOK)
			}
			if ip != tt.wantIP {
				t.Errorf("LookupIP(%q): got ip=%q, want %q", tt.hostname, ip, tt.wantIP)
			}
		})
	}
}
