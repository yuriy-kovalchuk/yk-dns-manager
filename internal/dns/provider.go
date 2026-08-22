package dns

import (
	"context"
)

// Record represents a DNS record to be managed.
type Record struct {
	Hostname string            // FQDN, e.g. "app.example.com"
	Type     string            // "A", "AAAA", "CNAME"
	Value    string            // IP address or target
	Meta     map[string]string // provider-specific fields (e.g. "description")
}

// Provider is the interface that DNS providers must implement.
//
// Credential contract: a provider that needs credentials receives them via
// the *Credentials argument to its constructor — the raw data of the
// Kubernetes Secret named by the instance's `secret` config field. Each
// provider declares the keys it expects (e.g. "API_KEY"/"API_SECRET" or
// "USERNAME"/"PASSWORD") and fails construction with an error naming any
// missing key. Providers that need no credentials receive nil.
type Provider interface {
	Exists(ctx context.Context, hostname, recordType string) (bool, error)
	Create(ctx context.Context, record Record) error
	Update(ctx context.Context, record Record) error
	Delete(ctx context.Context, hostname, recordType string) error
	Upsert(ctx context.Context, record Record) error
	HealthCheck(ctx context.Context) error
}
