package dns

import "context"

// Record represents a DNS record to be managed.
type Record struct {
	Hostname string            // FQDN, e.g. "app.example.com"
	Type     string            // "A", "AAAA", "CNAME"
	Value    string            // IP address or target
	TTL      int               // 0 = provider default
	Meta     map[string]string // provider-specific fields (e.g. "description")
}

// Provider is the interface that DNS providers must implement.
type Provider interface {
	Exists(ctx context.Context, hostname, recordType string) (bool, error)
	Create(ctx context.Context, record Record) error
	Update(ctx context.Context, record Record) error
	Delete(ctx context.Context, hostname, recordType string) error
	Upsert(ctx context.Context, record Record) error
}
