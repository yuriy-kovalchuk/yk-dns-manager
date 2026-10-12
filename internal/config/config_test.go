package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDomainMap(t *testing.T) {
	content := "my-domain1.com: 10.0.8.100\nmy-domain2.it: 10.0.9.50\n"
	path := filepath.Join(t.TempDir(), "domain-map.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	dm, err := LoadDomainMap(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(dm.Entries()) != 2 {
		t.Fatalf("expected 2 domains, got %d", len(dm.Entries()))
	}
}

// TestLoadDomainMapValidation pins the startup checks. Every case below used to
// load successfully and then either produce a broken DNS record or leave a
// controller running that manages nothing.
func TestLoadDomainMapValidation(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		contains string
	}{
		{
			name:     "empty file",
			content:  "",
			contains: "no entries",
		},
		{
			name:     "null document",
			content:  "null\n",
			contains: "no entries",
		},
		{
			name:     "not a mapping",
			content:  "- a\n- b\n",
			contains: "parsing domain map file",
		},
		{
			// yaml.v3 rejects this already; asserted so a dependency change
			// cannot quietly start letting the last entry win.
			name:     "duplicate keys",
			content:  "\"a.com\": \"10.0.0.1\"\n\"a.com\": \"10.0.0.2\"\n",
			contains: "already defined",
		},
		{
			name:     "value is not an IP",
			content:  "a.com: not-an-ip\n",
			contains: "not a valid IP address",
		},
		{
			name:     "value is a hostname",
			content:  "a.com: lb.example.com\n",
			contains: "not a valid IP address",
		},
		{
			name:     "value is empty",
			content:  "a.com: \"\"\n",
			contains: "not a valid IP address",
		},
		{
			// The record type is hardcoded to A, so an IPv6 value would be
			// written into an A record.
			name:     "IPv6 value",
			content:  "a.com: 2001:db8::1\n",
			contains: "not IPv4",
		},
		{
			name:     "empty domain key",
			content:  "\"\": \"10.0.0.1\"\n",
			contains: "empty domain key",
		},
		{
			name:     "key with a space",
			content:  "\"my domain.com\": 10.0.0.1\n",
			contains: "not a valid domain name",
		},
		{
			name:     "wildcard in the middle",
			content:  "\"app.*.example.com\": 10.0.0.1\n",
			contains: "not a valid domain name",
		},
		{
			name:     "label starting with a hyphen",
			content:  "\"-app.example.com\": 10.0.0.1\n",
			contains: "not a valid domain name",
		},
		{
			// Both normalize to example.com, so one would silently win.
			name:     "duplicate once normalized",
			content:  "\"Example.com\": 10.0.0.1\n\"example.com.\": 10.0.0.2\n",
			contains: "duplicates another entry",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "domain-map.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := LoadDomainMap(path)
			if err == nil {
				t.Fatalf("expected %s to be rejected", tc.name)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.contains)
			}
		})
	}
}

func TestLoadDomainMapNormalizesValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "domain-map.yaml")
	if err := os.WriteFile(path, []byte("a.com: \" 10.0.8.100 \"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dm, err := LoadDomainMap(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := dm.Entries()["a.com"]; got != "10.0.8.100" {
		t.Errorf("value = %q, want the canonical 10.0.8.100", got)
	}
}

// TestLookupIP is audit D2: an exact key matches only that name. It used to
// cover every name below it as well.
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
		{"my-domain1.com", "10.0.8.100", true},
		{"my-domain1.com.", "10.0.8.100", true}, // trailing dot (FQDN)
		{"My-Domain1.com", "10.0.8.100", true},  // case-insensitive
		{"my-domain2.it", "10.0.9.50", true},
		{"app.my-domain1.com", "", false}, // an exact key does not cover subdomains
		{"deep.nested.my-domain1.com", "", false},
		{"unknown.com", "", false},
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

// TestLoadDomainMapNormalizesKeys is audit B12: keys are compared with route
// hostnames, which are lowercase without a trailing dot, so a key written
// differently used to never match.
func TestLoadDomainMapNormalizesKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "domain-map.yaml")
	content := "\" App.Example.COM. \": 10.0.0.1\n\"*.Lab.Example.com\": 10.0.0.2\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	dm, err := LoadDomainMap(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip, ok := dm.LookupIP("app.example.com"); !ok || ip != "10.0.0.1" {
		t.Errorf("app.example.com = %q, %v; want 10.0.0.1", ip, ok)
	}
	if ip, ok := dm.LookupIP("x.lab.example.com"); !ok || ip != "10.0.0.2" {
		t.Errorf("x.lab.example.com = %q, %v; want 10.0.0.2", ip, ok)
	}
}
