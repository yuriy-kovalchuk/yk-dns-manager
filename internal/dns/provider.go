// Package dns defines the provider interface the controller manages DNS records through.
package dns

import (
	"context"
)

// Record represents a DNS record to be managed.
type Record struct {
	Hostname string // FQDN, e.g. "app.example.com"
	Type     string // "A"
	Value    string // IP address
}

// Result reports what Ensure did.
type Result int

const (
	// Created means no live record existed and one was added.
	Created Result = iota
	// Updated means an existing managed record was changed to match.
	Updated
	// Unchanged means the record already matched; nothing was written.
	Unchanged
	// Kept means a managed record holds a different value and was left alone
	// because overwriting was not allowed.
	Kept
	// Conflict means a live record the controller does not manage holds the
	// name. Nothing was written.
	Conflict
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
	case Conflict:
		return "conflict"
	default:
		return "unknown"
	}
}

// EnsureOptions controls what Ensure may change.
type EnsureOptions struct {
	// Overwrite lets Ensure change a managed record that holds a different
	// value. Without it such a record is left alone (Kept).
	Overwrite bool
	// AdoptLegacy treats a record written by a version of the controller that
	// did not mark ownership as managed. The caller sets it only when it has
	// its own evidence that the record is the controller's.
	AdoptLegacy bool
}

// Provider is the interface that DNS providers must implement.
//
// A provider must tell the records the controller manages from every other
// record on the backend, and must never change or delete the others. The
// conformance suite in internal/dns/dnstest pins the full contract.
type Provider interface {
	// HealthCheck verifies the backend is reachable and accepts the
	// configured credentials.
	HealthCheck(ctx context.Context) error
	// Batch starts a unit of work, normally one reconcile. A batch is used by
	// one goroutine; separate batches may run concurrently.
	Batch() Batch
}

// Batch works from one view of the backend's records, read on first use,
// and keeps it in step with its own writes. Its writes may be staged; Commit
// publishes them.
type Batch interface {
	// Ensure makes record.Hostname resolve to record.Value as a managed
	// record. A disabled record does not count as live. Nothing is written
	// when the record already matches.
	Ensure(ctx context.Context, record Record, opts EnsureOptions) (Result, error)
	// Remove deletes the managed records for hostname and recordType, and
	// legacy ones when adoptLegacy is set. Other records are left alone.
	// Removing a record that does not exist is not an error.
	Remove(ctx context.Context, hostname, recordType string, adoptLegacy bool) error
	// Commit publishes anything staged and not yet published, including
	// writes from earlier batches whose publish failed. It does nothing for a
	// batch that was never used.
	Commit(ctx context.Context) error
}
