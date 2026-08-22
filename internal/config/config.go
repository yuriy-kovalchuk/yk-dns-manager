// Package config provides configuration loading for yk-dns-manager.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Config is the full application configuration, loaded from a single YAML
// file: the domain map plus all DNS provider instances.
type Config struct {
	// DomainMap maps hostnames/domains to load balancer IPs.
	DomainMap *DomainMap
	// Providers are the configured DNS provider instances. An empty map is
	// valid: the controller runs in no-op mode.
	Providers map[string]ProviderInstance
	// SecretsNamespace is the namespace provider credential Secrets are
	// read from. When empty (in-cluster default), the app's own namespace
	// is used. Set it when running locally against a dev cluster.
	SecretsNamespace string
}

// fileConfig is the on-disk shape of the configuration file.
type fileConfig struct {
	DomainMap        map[string]string           `yaml:"domainMap"`
	Providers        map[string]ProviderInstance `yaml:"providers"`
	SecretsNamespace string                      `yaml:"secretsNamespace"`
}

// LoadConfigFromPath reads the full application configuration from the
// given file path.
func LoadConfigFromPath(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var fc fileConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // reject unknown keys
	if err := dec.Decode(&fc); err != nil && !errors.Is(err, io.EOF) {
		// io.EOF means the file was empty — a valid no-op config.
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	return &Config{
		DomainMap:        NewDomainMap(fc.DomainMap),
		Providers:        fc.Providers,
		SecretsNamespace: fc.SecretsNamespace,
	}, nil
}

// DomainMap maps base domains to their load balancer IPs.
type DomainMap struct {
	entries map[string]string
}

// NewDomainMap creates a DomainMap from raw domain-to-IP entries.
func NewDomainMap(entries map[string]string) *DomainMap {
	if entries == nil {
		entries = map[string]string{}
	}
	return &DomainMap{entries: entries}
}

// LookupIP finds the IP for a hostname by matching against domain entries.
// It walks up the domain labels checking for exact matches and wildcard entries.
// Exact matches take priority over wildcards. For example, given:
//
//	"*.mydomain.com":    "10.0.0.1"
//	"app2.mydomain.com": "10.0.0.2"
//
// "app1.mydomain.com" returns "10.0.0.1" (wildcard match)
// "app2.mydomain.com" returns "10.0.0.2" (exact match wins)
func (dm *DomainMap) LookupIP(hostname string) (string, bool) {
	hostname = strings.TrimSuffix(hostname, ".")
	// Walk up the domain labels until we find a match
	for h := hostname; h != ""; {
		// Check exact match first
		if ip, ok := dm.entries[h]; ok {
			return ip, true
		}
		idx := strings.Index(h, ".")
		if idx < 0 {
			break
		}
		// Check wildcard match at this level
		if ip, ok := dm.entries["*."+h[idx+1:]]; ok {
			return ip, true
		}
		h = h[idx+1:]
	}
	return "", false
}

// Domains returns all configured base domains.
func (dm *DomainMap) Domains() []string {
	domains := make([]string, 0, len(dm.entries))
	for d := range dm.entries {
		domains = append(domains, d)
	}
	return domains
}

// ProviderInstance configures a single DNS backend instance.
type ProviderInstance struct {
	// Provider is the provider type (e.g. "opnsense"). When empty, the
	// instance name (the map key) is used as the type.
	Provider string `yaml:"provider"`
	// Upsert controls whether this instance updates existing records on
	// every reconcile (true) or only creates records that don't exist yet
	// (false).
	Upsert bool `yaml:"upsert"`
	// Secret is the name of the Kubernetes Secret holding this instance's
	// credentials. The app reads it via the API at startup; each provider
	// declares and validates the keys it expects. Empty for providers that
	// need no credentials.
	Secret string `yaml:"secret"`
	// Settings are provider-specific non-secret connection settings.
	Settings map[string]string `yaml:"settings"`
}
