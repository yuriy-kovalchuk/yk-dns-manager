package memory

import (
	"slices"
)

// The methods below act on the backend directly, as an operator editing it by hand would; tests use them to seed and inspect records.

// Seed stores r as already published, like configuration that predates the controller.
func (p *Provider) Seed(r Record) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r.FQDN = normalize(r.FQDN)
	if r.Type == "" {
		r.Type = "A"
	}
	p.stored = append(p.stored, &r)
	p.published = append(p.published, clone(r))
}

// Drop deletes every record for fqdn and republishes, leaving the controller with a backend that no longer matches its routes.
func (p *Provider) Drop(fqdn string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fqdn = normalize(fqdn)
	p.stored = slices.DeleteFunc(p.stored, func(r *Record) bool { return r.FQDN == fqdn })
	p.published = slices.DeleteFunc(p.published, func(r Record) bool { return r.FQDN == fqdn })
}

// Stored returns the records for fqdn, published or not.
func (p *Provider) Stored(fqdn string) []Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Record
	for _, r := range p.stored {
		if r.FQDN == normalize(fqdn) {
			out = append(out, clone(*r))
		}
	}
	return out
}

// Live returns the records for fqdn as of the last publish, which is what the backend serves.
func (p *Provider) Live(fqdn string) []Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Record
	for _, r := range p.published {
		if r.FQDN == normalize(fqdn) {
			out = append(out, clone(r))
		}
	}
	return out
}

// FailPublish makes every publish return err until it is called with nil; writes still succeed and stay staged.
func (p *Provider) FailPublish(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failure = err
}

// Counts is how often batches read, wrote and published since the last ResetCounts.
type Counts struct{ Reads, Writes, Publishes int }

// Counts returns the operation counts.
func (p *Provider) Counts() Counts {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Counts{Reads: p.reads, Writes: p.writes, Publishes: p.publishes}
}

// ResetCounts zeroes the operation counts.
func (p *Provider) ResetCounts() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads, p.writes, p.publishes = 0, 0, 0
}
