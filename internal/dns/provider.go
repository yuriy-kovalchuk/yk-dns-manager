// Package dns defines the provider interface the controller manages DNS records through.
package dns

import (
	"context"
	"fmt"
	"time"
)

// Record represents a DNS record to be managed.
type Record struct {
	Hostname string // FQDN, e.g. "app.example.com"
	Type     string // "A"
	Value    string // IP address
	// Owner names who asked for the record ("httproute/<namespace>/<name>"); the provider stores it so Remove can tell whose record it is.
	Owner string
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
	// Kept means a managed record holds another value and overwriting was not allowed.
	Kept
	// Conflict means an unmanaged live record holds the name; nothing was written.
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
	// Overwrite lets Ensure change a managed record that holds another value; without it such a record is Kept.
	Overwrite bool
	// AdoptLegacy treats pre-ownership records as managed; the caller sets it only on its own evidence.
	AdoptLegacy bool
}

// Provider is a DNS backend: it must tell managed records from every other record and never touch the others.
type Provider interface {
	// HealthCheck verifies the backend is reachable with the configured credentials.
	HealthCheck(ctx context.Context) error
	// Batch starts a unit of work, normally one reconcile: one goroutine per batch, batches may run concurrently.
	Batch() Batch
}

// Batch works from one view of the backend's records, read on first use and kept in step with its own writes.
type Batch interface {
	// Ensure makes record.Hostname resolve to record.Value, recording record.Owner: the owner is what authorizes a later Remove.
	Ensure(ctx context.Context, record Record, opts EnsureOptions) (Result, error)
	// Remove deletes the records for hostname and recordType that owner owns, plus legacy ones when adoptLegacy is set; other owners' records stay.
	Remove(ctx context.Context, hostname, recordType, owner string, adoptLegacy bool) error
	// Commit publishes anything staged, including earlier batches whose publish failed; *PublishDeferred means a publish is held back and still owed.
	Commit(ctx context.Context) error
}

// PublishDeferred reports a publish held back on purpose to coalesce changes: they stay owed, and Commit must run again after After.
type PublishDeferred struct {
	After time.Duration
}

func (d *PublishDeferred) Error() string {
	return fmt.Sprintf("publish held back, retry in %s", d.After.Round(time.Millisecond))
}
