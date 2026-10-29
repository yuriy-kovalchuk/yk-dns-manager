// Package config provides configuration loading for domain maps and DNS provider settings.
package config

import (
	"fmt"
	"maps"
	"net/netip"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// DomainMap maps base domains to their load balancer IPs.
type DomainMap struct {
	entries map[string]string
}

// LoadDomainMap reads a YAML file mapping domains to IPs, validating every entry at startup rather than at reconcile time.
func LoadDomainMap(path string) (*DomainMap, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path comes from an operator-set CLI flag, not request input
	if err != nil {
		return nil, fmt.Errorf("reading domain map file: %w", err)
	}

	raw := make(map[string]string)
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing domain map file: %w", err)
	}

	if len(raw) == 0 {
		return nil, fmt.Errorf("domain map file %s contains no entries: the controller would manage nothing", path)
	}

	entries := make(map[string]string, len(raw))
	for domain, value := range raw {
		key, ip, err := parseEntry(domain, value)
		if err != nil {
			return nil, fmt.Errorf("domain map file %s: %w", path, err)
		}
		if _, dup := entries[key]; dup {
			return nil, fmt.Errorf("domain map file %s: entry %q duplicates another entry once normalized to %q", path, domain, key)
		}
		entries[key] = ip
	}
	return &DomainMap{entries: entries}, nil
}

// parseEntry normalizes a key the way Gateway API writes route hostnames, lowercase without a trailing dot, so it can match one, and requires an IPv4 value.
func parseEntry(domain, value string) (key, ip string, err error) {
	key = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if key == "" {
		return "", "", fmt.Errorf("entry with an empty domain key")
	}
	if !validDomainKey(key) {
		return "", "", fmt.Errorf("entry %q: not a valid domain name or *.domain wildcard", domain)
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil {
		return "", "", fmt.Errorf("entry %q: %q is not a valid IP address", domain, value)
	}
	if !addr.Is4() {
		return "", "", fmt.Errorf("entry %q: %s is not IPv4; only A records are managed", domain, addr)
	}
	return key, addr.String(), nil
}

// dnsLabel is one label of a domain name: letters, digits and hyphens, not starting or ending with a hyphen, at most 63 characters.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// validDomainKey reports whether key is a domain name, optionally starting with "*." for a wildcard.
func validDomainKey(key string) bool {
	name := strings.TrimPrefix(key, "*.")
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if !dnsLabel.MatchString(label) {
			return false
		}
	}
	return true
}

// LookupIP returns the IP for hostname: an exact key matches only itself, a wildcard key covers every name below at any depth, and the most specific match wins.
func (dm *DomainMap) LookupIP(hostname string) (string, bool) {
	hostname = strings.ToLower(strings.TrimSuffix(hostname, "."))
	if ip, ok := dm.entries[hostname]; ok {
		return ip, true
	}
	for parent := hostname; ; {
		dot := strings.Index(parent, ".")
		if dot < 0 {
			return "", false
		}
		parent = parent[dot+1:]
		if ip, ok := dm.entries["*."+parent]; ok {
			return ip, true
		}
	}
}

// Entries returns a copy of the configured domain-to-IP mapping.
func (dm *DomainMap) Entries() map[string]string {
	return maps.Clone(dm.entries)
}
