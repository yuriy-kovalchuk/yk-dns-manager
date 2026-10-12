// Package config provides configuration loading for domain maps and DNS provider settings.
package config

import (
	"fmt"
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

// LoadDomainMap reads a YAML file mapping domains to IPs.
//
// Every entry is validated here rather than at reconcile time. The record type
// is hardcoded to A, so a value that is not an IPv4 address would produce a
// broken DNS record, and an empty map would leave a controller running that
// manages nothing.
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
		if strings.TrimSpace(domain) == "" {
			return nil, fmt.Errorf("domain map file %s: entry with an empty domain key", path)
		}
		// Keys are compared with route hostnames, which Gateway API keeps
		// lowercase and without a trailing dot, so a key written any other way
		// would silently never match.
		key := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
		if !validDomainKey(key) {
			return nil, fmt.Errorf("domain map entry %q: not a valid domain name or *.domain wildcard", domain)
		}
		if _, dup := entries[key]; dup {
			return nil, fmt.Errorf("domain map entry %q: duplicates another entry once normalized to %q", domain, key)
		}
		addr, err := netip.ParseAddr(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("domain map entry %q: %q is not a valid IP address", domain, value)
		}
		if !addr.Is4() {
			return nil, fmt.Errorf("domain map entry %q: %s is not IPv4; only A records are managed", domain, addr)
		}
		entries[key] = addr.String()
	}

	return &DomainMap{entries: entries}, nil
}

// dnsLabel is one label of a domain name: letters, digits and hyphens, not
// starting or ending with a hyphen, at most 63 characters.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// validDomainKey reports whether key is a domain name, optionally starting
// with "*." for a wildcard.
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

// LookupIP returns the IP for hostname.
//
// An exact key matches only that name. A wildcard key "*.example.com" covers
// every name below example.com, at any depth, like a DNS wildcard, but not
// example.com itself. The exact key wins, then the most specific wildcard:
//
//	"example.com":       10.0.0.1   example.com
//	"*.example.com":     10.0.0.2   app.example.com, a.b.example.com
//	"*.lab.example.com": 10.0.0.3   x.lab.example.com
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
	out := make(map[string]string, len(dm.entries))
	for d, ip := range dm.entries {
		out[d] = ip
	}
	return out
}
