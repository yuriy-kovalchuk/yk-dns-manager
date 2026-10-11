package dns

import (
	"context"
	"errors"
)

// Standard DNS provider errors.
var (
	ErrNotFound     = errors.New("not found")
	ErrAuthFailed   = errors.New("authentication failed")
	ErrConnection   = errors.New("connection failed")
	ErrTimeout      = errors.New("request timeout")
	ErrRetryable    = errors.New("retryable error")
	ErrNonRetryable = errors.New("non-retryable error")
)

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
	HealthCheck(ctx context.Context) error
	// Flush applies changes accumulated by Create/Update/Delete. Providers that
	// need an explicit apply step (OPNsense's reconfigure republishes the whole
	// model config) defer it here so a reconcile costs one apply instead of one
	// per record. Calling it with nothing pending is a no-op.
	Flush(ctx context.Context) error
}
