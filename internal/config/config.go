package config

import (
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// DomainMap maps base domains to their load balancer IPs.
type DomainMap struct {
	entries map[string]string
}

// LoadDomainMap reads a YAML file mapping domains to IPs.
func LoadDomainMap(path string) (*DomainMap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading domain map file: %w", err)
	}

	entries := make(map[string]string)
	if err := yaml.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parsing domain map file: %w", err)
	}

	return &DomainMap{entries: entries}, nil
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
