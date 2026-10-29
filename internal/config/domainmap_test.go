package config

import (
	"maps"
	"strings"
	"testing"
)

// TestNewDomainMap covers normalization: keys are compared with route hostnames, which are lowercase without a trailing dot, so a key written differently used to never match (audit B12); values are stored in canonical form.
func TestNewDomainMap(t *testing.T) {
	dm, err := NewDomainMap(map[string]string{" App.Example.COM. ": " 10.0.0.1 ", "*.Lab.Example.com": "10.0.0.2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{"app.example.com": "10.0.0.1", "*.lab.example.com": "10.0.0.2"}
	if got := dm.Entries(); !maps.Equal(got, want) {
		t.Errorf("entries = %v, want %v", got, want)
	}
	if ip, ok := dm.LookupIP("x.lab.example.com"); !ok || ip != "10.0.0.2" {
		t.Errorf("x.lab.example.com = %q, %v; want 10.0.0.2", ip, ok)
	}
}

// TestNewDomainMapValidation pins the startup checks: every case below used to load and then produce a broken record or a controller that manages nothing.
func TestNewDomainMapValidation(t *testing.T) {
	cases := []struct {
		name     string
		entries  map[string]string
		contains string
	}{
		{"empty", nil, "no entries"},
		{"value is not an IP", map[string]string{"a.com": "not-an-ip"}, "not a valid IP address"},
		{"value is a hostname", map[string]string{"a.com": "lb.example.com"}, "not a valid IP address"},
		{"value is empty", map[string]string{"a.com": ""}, "not a valid IP address"},
		// the record type is hardcoded to A, so an IPv6 value would land in an A record
		{"IPv6 value", map[string]string{"a.com": "2001:db8::1"}, "not IPv4"},
		{"empty domain key", map[string]string{"": "10.0.0.1"}, "empty domain key"},
		{"key with a space", map[string]string{"my domain.com": "10.0.0.1"}, "not a valid domain name"},
		{"wildcard in the middle", map[string]string{"app.*.example.com": "10.0.0.1"}, "not a valid domain name"},
		{"label starting with a hyphen", map[string]string{"-app.example.com": "10.0.0.1"}, "not a valid domain name"},
		// both normalize to example.com, so one would silently win
		{"duplicate once normalized", map[string]string{"Example.com": "10.0.0.1", "example.com.": "10.0.0.2"}, "duplicates another entry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewDomainMap(tc.entries)
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.contains)
			}
		})
	}
}

// TestLookupIP is audit D2: an exact key matches only that name, where it used to cover every name below it; a wildcard covers any depth and the most specific key wins.
func TestLookupIP(t *testing.T) {
	dm := &DomainMap{entries: map[string]string{
		"exact.com":            "10.0.8.100",
		"*.mydomain.com":       "10.0.0.1",
		"app2.mydomain.com":    "10.0.0.2",
		"mydomain.com":         "10.0.0.3",
		"*.other.mydomain.com": "10.0.0.4",
	}}
	tests := []struct {
		hostname string
		wantIP   string
	}{
		{"exact.com", "10.0.8.100"},
		{"exact.com.", "10.0.8.100"}, // trailing dot (FQDN)
		{"Exact.com", "10.0.8.100"},  // case-insensitive
		{"app.exact.com", ""},        // an exact key does not cover subdomains
		{"deep.nested.exact.com", ""},
		{"app1.mydomain.com", "10.0.0.1"},        // wildcard
		{"deep.nested.mydomain.com", "10.0.0.1"}, // wildcard walks up
		{"app2.mydomain.com", "10.0.0.2"},        // exact wins over wildcard
		{"mydomain.com", "10.0.0.3"},             // a wildcard does not match its bare domain, the exact key does
		{"foo.other.mydomain.com", "10.0.0.4"},   // most specific wildcard
		{"other.mydomain.com", "10.0.0.1"},
		{"unknown.com", ""},
	}
	for _, tt := range tests {
		ip, ok := dm.LookupIP(tt.hostname)
		if ip != tt.wantIP || ok != (tt.wantIP != "") {
			t.Errorf("LookupIP(%q) = %q, %v; want %q", tt.hostname, ip, ok, tt.wantIP)
		}
	}
}

// TestLookupIPWildcardSkipsBareDomain: "*.x" never matches "x" itself.
func TestLookupIPWildcardSkipsBareDomain(t *testing.T) {
	dm := &DomainMap{entries: map[string]string{"*.mydomain.com": "10.0.0.1"}}
	if ip, ok := dm.LookupIP("mydomain.com"); ok {
		t.Errorf("LookupIP(mydomain.com) = %q, want no match", ip)
	}
}
