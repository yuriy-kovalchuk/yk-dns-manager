// Package memory is a DNS provider that keeps records in process memory: for local development without an appliance, and for tests of everything above the provider.
package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

// Origin says who a record belongs to.
type Origin int

const (
	// Managed records were written by the controller.
	Managed Origin = iota
	// Legacy records were written before ownership was marked; they count as managed only when adopted.
	Legacy
	// Foreign records belong to someone else and are never changed.
	Foreign
)

// Record is one record as the backend holds it.
type Record struct {
	FQDN     string
	Type     string
	Value    string
	Origin   Origin
	Owners   []string
	Disabled bool
}

func (r Record) ours(adoptLegacy bool) bool {
	return r.Origin == Managed || (adoptLegacy && r.Origin == Legacy)
}

// Provider holds staged records and the published copy the last Commit made, like a backend that only serves what it has published.
type Provider struct {
	log       logr.Logger
	publisher *dns.Publisher

	mu        sync.Mutex
	stored    []*Record
	published []Record
	failure   error
	reads     int
	writes    int
	publishes int
}

// New creates a memory provider; publish_interval behaves as it does for a real backend.
func New(log logr.Logger, settings map[string]string) (*Provider, error) {
	var interval time.Duration
	if v := settings["publish_interval"]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("memory: publish_interval must be a non-negative duration such as \"5s\", got %q", v)
		}
		interval = d
	}
	log.Info("memory: DNS records live only in this process and are lost on restart")
	return &Provider{log: log, publisher: dns.NewPublisher(interval)}, nil
}

// HealthCheck always succeeds: there is no backend to reach.
func (p *Provider) HealthCheck(context.Context) error { return nil }

// Batch starts one reconcile's unit of work; every batch works on the shared store, so it always sees its own writes.
func (p *Provider) Batch() dns.Batch { return &batch{p: p} }

type batch struct {
	p    *Provider
	used bool
}

// Ensure follows the dns.Batch contract: a live foreign record is a conflict, our duplicates collapse into one, and the owner list only grows.
func (b *batch) Ensure(_ context.Context, record dns.Record, opts dns.EnsureOptions) (dns.Result, error) {
	p := b.p
	p.mu.Lock()
	defer p.mu.Unlock()
	b.use()

	fqdn := normalize(record.Hostname)
	var ours []*Record
	for _, r := range p.matching(fqdn, record.Type) {
		switch {
		case r.ours(opts.AdoptLegacy):
			ours = append(ours, r)
		case !r.Disabled:
			return dns.Conflict, nil
		}
	}
	if len(ours) == 0 {
		p.write("created", &Record{FQDN: fqdn, Type: record.Type, Value: record.Value, Owners: withOwner(nil, record.Owner)})
		return dns.Created, nil
	}

	keep := ours[0]
	if i := slices.IndexFunc(ours, func(r *Record) bool { return !r.Disabled }); i >= 0 {
		keep = ours[i]
	}
	owners := withOwner(keep.Owners, record.Owner)
	if !opts.Overwrite && !keep.Disabled && keep.Value != record.Value {
		if len(owners) != len(keep.Owners) {
			keep.Owners = owners
			p.write("joined", keep)
		}
		return dns.Kept, nil
	}

	for _, r := range ours {
		if r != keep {
			p.remove(r)
		}
	}
	if !keep.Disabled && keep.Origin == Managed && keep.Value == record.Value && len(owners) == len(keep.Owners) {
		if len(ours) > 1 {
			return dns.Updated, nil // only duplicates were removed
		}
		return dns.Unchanged, nil
	}
	keep.Value, keep.Origin, keep.Owners, keep.Disabled = record.Value, Managed, owners, false
	p.write("updated", keep)
	return dns.Updated, nil
}

// Remove deletes our records for hostname that owner may release: those naming it, and those naming no owner at all.
func (b *batch) Remove(_ context.Context, hostname, recordType, owner string, adoptLegacy bool) error {
	p := b.p
	p.mu.Lock()
	defer p.mu.Unlock()
	b.use()

	for _, r := range p.matching(normalize(hostname), recordType) {
		if r.ours(adoptLegacy) && (len(r.Owners) == 0 || slices.Contains(r.Owners, owner)) {
			p.remove(r)
		}
	}
	return nil
}

// Commit publishes the staged records if a write is still unpublished.
func (b *batch) Commit(ctx context.Context) error {
	if !b.used {
		return nil
	}
	return b.p.publisher.Publish(ctx, b.p.publish)
}

// use counts the batch's first call as its one read of the backend; the caller holds mu.
func (b *batch) use() {
	if !b.used {
		b.used = true
		b.p.reads++
	}
}

func (p *Provider) publish(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failure != nil {
		return p.failure
	}
	p.published = p.published[:0]
	for _, r := range p.stored {
		p.published = append(p.published, clone(*r))
	}
	p.publishes++
	p.log.V(1).Info("memory: published", "records", len(p.published))
	return nil
}

// matching returns the stored records for fqdn and type; the caller holds mu.
func (p *Provider) matching(fqdn, recordType string) []*Record {
	var out []*Record
	for _, r := range p.stored {
		if r.FQDN == fqdn && strings.EqualFold(r.Type, recordType) {
			out = append(out, r)
		}
	}
	return out
}

// write stages r, adding it when it is new; the caller holds mu.
func (p *Provider) write(what string, r *Record) {
	if !slices.Contains(p.stored, r) {
		p.stored = append(p.stored, r)
	}
	p.staged(what, *r)
}

// remove stages the deletion of r; the caller holds mu.
func (p *Provider) remove(r *Record) {
	p.stored = slices.DeleteFunc(p.stored, func(s *Record) bool { return s == r })
	p.staged("deleted", *r)
}

func (p *Provider) staged(what string, r Record) {
	p.writes++
	p.publisher.Staged()
	p.log.Info("memory: record "+what, "fqdn", r.FQDN, "value", r.Value, "owners", r.Owners)
}

func withOwner(owners []string, owner string) []string {
	if owner == "" || slices.Contains(owners, owner) {
		return owners
	}
	return append(slices.Clone(owners), owner)
}

func normalize(hostname string) string {
	return strings.ToLower(strings.TrimSuffix(hostname, "."))
}

func clone(r Record) Record {
	r.Owners = slices.Clone(r.Owners)
	return r
}
