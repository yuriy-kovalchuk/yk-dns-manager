// Package dnstest is the conformance suite for dns.Provider: it pins the contract the controller relies on, ownership above all.
package dnstest

import (
	"context"
	"testing"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

// Origin says who a record belongs to.
type Origin int

const (
	// Managed records carry the provider's ownership marker.
	Managed Origin = iota
	// Legacy records were written before ownership was marked.
	Legacy
	// Foreign records belong to someone else.
	Foreign
)

// Record is an A record as the suite sees it on the backend.
type Record struct {
	FQDN     string
	Value    string
	Origin   Origin
	Disabled bool
}

// Backend is the provider-specific side of the suite: direct access to the test backend.
type Backend interface {
	// Seed writes a record straight to the backend, already live.
	Seed(t *testing.T, r Record)
	// Stored returns every record for fqdn, published or not.
	Stored(t *testing.T, fqdn string) []Record
	// Live returns the records the backend currently serves for fqdn, disabled ones included.
	Live(t *testing.T, fqdn string) []Record
}

// Factory returns a provider and the empty backend it talks to.
type Factory func(t *testing.T) (dns.Provider, Backend)

const (
	host  = "app.example.com"
	other = "other.example.com"
	ip    = "10.0.0.1"
	newIP = "10.0.0.2"

	// owner and otherOwner are the Ensure/Remove callers in the ownership tests; their form means nothing to the provider.
	owner      = "route/blue"
	otherOwner = "route/green"
)

// Run runs the conformance suite against the provider newBackend builds.
func Run(t *testing.T, newBackend Factory) {
	overwrite := dns.EnsureOptions{Overwrite: true}
	createOnly := dns.EnsureOptions{}

	t.Run("Ensure creates a managed record", func(t *testing.T) {
		p, be := newBackend(t)
		ensure(t, p, A(host, ip), createOnly, dns.Created)
		expectLive(t, be, host, Record{FQDN: host, Value: ip, Origin: Managed})
	})

	t.Run("Ensure on a matching record writes nothing", func(t *testing.T) {
		for _, opts := range []dns.EnsureOptions{createOnly, overwrite} {
			p, be := newBackend(t)
			be.Seed(t, Record{FQDN: host, Value: ip, Origin: Managed})
			ensure(t, p, A(host, ip), opts, dns.Unchanged)
		}
	})

	t.Run("Ensure with overwrite updates a managed record", func(t *testing.T) {
		p, be := newBackend(t)
		be.Seed(t, Record{FQDN: host, Value: ip, Origin: Managed})
		ensure(t, p, A(host, newIP), overwrite, dns.Updated)
		expectLive(t, be, host, Record{FQDN: host, Value: newIP, Origin: Managed})
	})

	t.Run("Ensure without overwrite keeps a managed record's value", func(t *testing.T) {
		p, be := newBackend(t)
		be.Seed(t, Record{FQDN: host, Value: ip, Origin: Managed})
		ensure(t, p, A(host, newIP), createOnly, dns.Kept)
		expectLive(t, be, host, Record{FQDN: host, Value: ip, Origin: Managed})
	})

	t.Run("Ensure re-enables a disabled managed record", func(t *testing.T) {
		p, be := newBackend(t)
		be.Seed(t, Record{FQDN: host, Value: ip, Origin: Managed, Disabled: true})
		ensure(t, p, A(host, ip), createOnly, dns.Updated)
		expectLive(t, be, host, Record{FQDN: host, Value: ip, Origin: Managed})
	})

	t.Run("Ensure never touches a live foreign record", func(t *testing.T) {
		for _, opts := range []dns.EnsureOptions{createOnly, overwrite, {Overwrite: true, AdoptLegacy: true}} {
			p, be := newBackend(t)
			be.Seed(t, Record{FQDN: host, Value: newIP, Origin: Foreign})
			ensure(t, p, A(host, ip), opts, dns.Conflict)
			expectStored(t, be, Record{FQDN: host, Value: newIP, Origin: Foreign})
		}
	})

	t.Run("Ensure ignores a disabled foreign record", func(t *testing.T) {
		p, be := newBackend(t)
		be.Seed(t, Record{FQDN: host, Value: newIP, Origin: Foreign, Disabled: true})
		ensure(t, p, A(host, ip), createOnly, dns.Created)
		expectLive(t, be, host,
			Record{FQDN: host, Value: newIP, Origin: Foreign, Disabled: true},
			Record{FQDN: host, Value: ip, Origin: Managed})
	})

	t.Run("Ensure treats a legacy record as foreign unless adopting", func(t *testing.T) {
		p, be := newBackend(t)
		be.Seed(t, Record{FQDN: host, Value: ip, Origin: Legacy})
		ensure(t, p, A(host, ip), overwrite, dns.Conflict)
		expectStored(t, be, Record{FQDN: host, Value: ip, Origin: Legacy})
	})

	t.Run("Ensure adopts a legacy record when asked", func(t *testing.T) {
		p, be := newBackend(t)
		be.Seed(t, Record{FQDN: host, Value: ip, Origin: Legacy})
		ensure(t, p, A(host, ip), dns.EnsureOptions{AdoptLegacy: true}, dns.Updated)
		expectLive(t, be, host, Record{FQDN: host, Value: ip, Origin: Managed})
	})

	t.Run("Ensure converges duplicate managed records", func(t *testing.T) {
		p, be := newBackend(t)
		be.Seed(t, Record{FQDN: host, Value: ip, Origin: Managed})
		be.Seed(t, Record{FQDN: host, Value: newIP, Origin: Managed})
		ensure(t, p, A(host, newIP), overwrite, dns.Updated)
		expectLive(t, be, host, Record{FQDN: host, Value: newIP, Origin: Managed})
	})

	t.Run("Remove deletes managed records only", func(t *testing.T) {
		p, be := newBackend(t)
		be.Seed(t, Record{FQDN: host, Value: ip, Origin: Managed})
		be.Seed(t, Record{FQDN: host, Value: ip, Origin: Managed})
		be.Seed(t, Record{FQDN: host, Value: newIP, Origin: Foreign, Disabled: true})
		be.Seed(t, Record{FQDN: other, Value: ip, Origin: Managed})
		remove(t, p, owner, false)
		expectLive(t, be, host, Record{FQDN: host, Value: newIP, Origin: Foreign, Disabled: true})
		expectLive(t, be, other, Record{FQDN: other, Value: ip, Origin: Managed})
	})

	t.Run("Remove leaves a foreign record", func(t *testing.T) {
		p, be := newBackend(t)
		be.Seed(t, Record{FQDN: host, Value: ip, Origin: Foreign})
		remove(t, p, owner, true)
		expectStored(t, be, Record{FQDN: host, Value: ip, Origin: Foreign})
	})

	t.Run("Remove deletes a legacy record only when adopting", func(t *testing.T) {
		p, be := newBackend(t)
		be.Seed(t, Record{FQDN: host, Value: ip, Origin: Legacy})
		remove(t, p, owner, false)
		expectStored(t, be, Record{FQDN: host, Value: ip, Origin: Legacy})
		remove(t, p, owner, true)
		expectLive(t, be, host)
	})

	t.Run("Remove of a missing record is not an error", func(t *testing.T) {
		p, be := newBackend(t)
		remove(t, p, owner, true)
		expectLive(t, be, host)
	})

	t.Run("Remove only deletes a record its owner wrote", func(t *testing.T) {
		p, be := newBackend(t)
		ensure(t, p, ownedBy(ip, owner), createOnly, dns.Created)
		remove(t, p, otherOwner, false)
		expectLive(t, be, host, Record{FQDN: host, Value: ip, Origin: Managed})
		remove(t, p, owner, false)
		expectLive(t, be, host)
	})

	t.Run("Ensure joins a record another owner wrote", func(t *testing.T) {
		p, be := newBackend(t)
		ensure(t, p, ownedBy(ip, owner), createOnly, dns.Created)
		// The address already matches, so only the owner list changes, and that is still a write.
		ensure(t, p, ownedBy(ip, otherOwner), createOnly, dns.Updated)
		// Either owner may release the record: whether anything still claims the name is the caller's business.
		remove(t, p, otherOwner, false)
		expectLive(t, be, host)
	})

	t.Run("Ensure joins a record it will not overwrite", func(t *testing.T) {
		p, be := newBackend(t)
		ensure(t, p, ownedBy(ip, owner), createOnly, dns.Created)
		// Create-only keeps the address, but a route claiming the name still joins the owner list so it can release the record later.
		ensure(t, p, ownedBy(newIP, otherOwner), createOnly, dns.Kept)
		expectLive(t, be, host, Record{FQDN: host, Value: ip, Origin: Managed})
		remove(t, p, otherOwner, false)
		expectLive(t, be, host)
	})

	t.Run("Ensure keeps the owners a rewrite would drop", func(t *testing.T) {
		p, be := newBackend(t)
		ensure(t, p, ownedBy(ip, owner), createOnly, dns.Created)
		// The address changes, so the record is rewritten with the first owner still on it.
		ensure(t, p, ownedBy(newIP, otherOwner), overwrite, dns.Updated)
		remove(t, p, owner, false)
		expectLive(t, be, host)
	})

	t.Run("Ensure by an owner already on the record keeps the other owners", func(t *testing.T) {
		p, be := newBackend(t)
		ensure(t, p, ownedBy(ip, owner), createOnly, dns.Created)
		ensure(t, p, ownedBy(ip, otherOwner), createOnly, dns.Updated)
		// The caller is already listed, so nothing is written, and the other owner survives whatever the value looks like.
		ensure(t, p, ownedBy(ip, owner), overwrite, dns.Unchanged)
		remove(t, p, otherOwner, false)
		expectLive(t, be, host)
	})

	t.Run("Remove deletes a record whose owners are unknown", func(t *testing.T) {
		// A record written before owners were marked belongs to no route in particular, so any caller may release it.
		p, be := newBackend(t)
		be.Seed(t, Record{FQDN: host, Value: ip, Origin: Managed})
		remove(t, p, owner, false)
		expectLive(t, be, host)
	})

	t.Run("A batch sees its own writes", func(t *testing.T) {
		p, be := newBackend(t)
		b := p.Batch()
		ensureIn(t, b, A(host, ip), createOnly, dns.Created)
		ensureIn(t, b, A(host, ip), createOnly, dns.Unchanged)
		if err := b.Remove(context.Background(), host, "A", owner, false); err != nil {
			t.Fatalf("Remove(%s): %v", host, err)
		}
		ensureIn(t, b, A(host, newIP), createOnly, dns.Created)
		commit(t, b)
		expectLive(t, be, host, Record{FQDN: host, Value: newIP, Origin: Managed})
	})
}

// A is an A record for fqdn, with no owner recorded.
func A(fqdn, value string) dns.Record {
	return dns.Record{Hostname: fqdn, Type: "A", Value: value}
}

// ownedBy is an A record for the suite's host, requested by owner.
func ownedBy(value, owner string) dns.Record {
	record := A(host, value)
	record.Owner = owner
	return record
}

// ensure runs Ensure in its own committed batch and checks the result.
func ensure(t *testing.T, p dns.Provider, rec dns.Record, opts dns.EnsureOptions, want dns.Result) {
	t.Helper()
	b := p.Batch()
	ensureIn(t, b, rec, opts, want)
	commit(t, b)
}

// ensureIn runs Ensure in b and checks the result.
func ensureIn(t *testing.T, b dns.Batch, rec dns.Record, opts dns.EnsureOptions, want dns.Result) {
	t.Helper()
	got, err := b.Ensure(context.Background(), rec, opts)
	if err != nil {
		t.Fatalf("Ensure(%s, %+v): %v", rec.Hostname, opts, err)
	}
	if got != want {
		t.Fatalf("Ensure(%s, %+v) = %s, want %s", rec.Hostname, opts, got, want)
	}
}

// remove runs Remove for host in its own committed batch.
func remove(t *testing.T, p dns.Provider, owner string, adoptLegacy bool) {
	t.Helper()
	b := p.Batch()
	if err := b.Remove(context.Background(), host, "A", owner, adoptLegacy); err != nil {
		t.Fatalf("Remove(%s): %v", host, err)
	}
	commit(t, b)
}

func commit(t *testing.T, b dns.Batch) {
	t.Helper()
	if err := b.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func expectLive(t *testing.T, be Backend, fqdn string, want ...Record) {
	t.Helper()
	expectSet(t, "live", be.Live(t, fqdn), want)
}

// expectStored checks every record for host, published or not.
func expectStored(t *testing.T, be Backend, want ...Record) {
	t.Helper()
	expectSet(t, "stored", be.Stored(t, host), want)
}

// expectSet compares records ignoring order.
func expectSet(t *testing.T, what string, got, want []Record) {
	t.Helper()
	count := map[Record]int{}
	for _, r := range got {
		count[r]++
	}
	for _, r := range want {
		count[r]--
	}
	for _, n := range count {
		if n != 0 {
			t.Fatalf("%s records = %+v, want %+v", what, got, want)
		}
	}
}
