// Package dns provides a provider interface and utilities for DNS record management.
package dns

import (
	"context"
	"strings"
)

// Credentials carries one provider instance's secret data, resolved from
// the Kubernetes Secret named in the instance's `secret` config field.
// The mechanism is key-agnostic: each provider declares the keys it
// expects and validates them in its constructor.
type Credentials struct {
	// SecretName is the name of the Kubernetes Secret the data came from
	// (for error messages). Empty when no secret was configured.
	SecretName string
	// Data is the raw Secret data (key → value bytes).
	Data map[string][]byte
}

// SecretKey returns the value of one key from a Credentials set, trimmed.
// It returns "" when the credentials are nil or the key is absent; it is
// the providers' job to check for absence and fail with a clear error
// that names the missing key.
func (c *Credentials) SecretKey(key string) string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(string(c.Data[key]))
}

// Upsert creates the record if it does not exist, updates it if it does.
// Use it as the Provider.Upsert implementation for backends that have no
// native upsert operation.
func Upsert(ctx context.Context, p Provider, r Record) error {
	exists, err := p.Exists(ctx, r.Hostname, r.Type)
	if err != nil {
		return err
	}
	if exists {
		return p.Update(ctx, r)
	}
	return p.Create(ctx, r)
}

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
