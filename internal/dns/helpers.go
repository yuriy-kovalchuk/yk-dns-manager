package dns

import (
	"strings"
)

// SplitHostname splits an FQDN into subdomain and domain parts.
// e.g. "app.example.com" → ("app", "example.com")
// e.g. "sub.app.example.com" → ("sub.app", "example.com")
func SplitHostname(fqdn string) (hostname, domain string) {
	fqdn = strings.TrimSuffix(fqdn, ".")
	parts := strings.SplitN(fqdn, ".", 2)
	if len(parts) < 2 {
		return fqdn, ""
	}
	return parts[0], parts[1]
}
