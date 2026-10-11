package dns

import (
	"context"
)

// Record represents a DNS record to be managed.
type Record struct {
	Hostname string            // FQDN, e.g. "app.example.com"
	Type     string            // "A"
	Value    string            // IP address
	Meta     map[string]string // provider-specific fields (e.g. "description")
}

// Result reports what Ensure did.
type Result int

const (
	// Created means no live record existed and one was added.
	Created Result = iota
	// Updated means an existing record was changed to match.
	Updated
	// Unchanged means the record already matched; nothing was written.
	Unchanged
	// Kept means a live record exists with a different value and was left
	// alone because overwriting was not allowed.
	Kept
)

func (r Result) String() string {
	switch r {
	case Created:
		return "created"
	case Updated:
		return "updated"
	case Unchanged:
		return "unchanged"
	case Kept:
		return "kept"
	default:
		return "unknown"
	}
}

// Provider is the interface that DNS providers must implement.
type Provider interface {
	// HealthCheck verifies the backend is reachable and accepts the
	// configured credentials.
	HealthCheck(ctx context.Context) error
	// Batch starts a unit of work, normally one reconcile. A batch is used by
	// one goroutine; separate batches may run concurrently.
	Batch() Batch
}

// Batch reads the backend's records at most once, on first use, and works
// from that view, so a reconcile with k hostnames costs one read rather than
// k. Its writes may be staged; Commit publishes them.
type Batch interface {
	// Ensure makes record.Hostname resolve to record.Value. With overwrite
	// false, a live record holding a different value is left alone (Kept);
	// a disabled record does not count as live. Nothing is written when the
	// record already matches.
	Ensure(ctx context.Context, record Record, overwrite bool) (Result, error)
	// Remove deletes every record for hostname and recordType. Removing a
	// record that does not exist is not an error.
	Remove(ctx context.Context, hostname, recordType string) error
	// Commit publishes anything staged and not yet published, including
	// writes from earlier batches whose publish failed. It does nothing for a
	// batch that was never used.
	Commit(ctx context.Context) error
}
