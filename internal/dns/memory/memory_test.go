package memory

import (
	"testing"

	"github.com/go-logr/logr"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/dnstest"
)

// TestConformance holds the memory provider to the same contract as a real backend, so tests built on it test the controller against that contract.
func TestConformance(t *testing.T) {
	dnstest.Run(t, func(t *testing.T) (dns.Provider, dnstest.Backend) {
		p, err := New(logr.Discard(), nil)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return p, backend{p}
	})
}

// TestNew_PublishIntervalMustBeADuration: a nonsense or negative interval fails startup.
func TestNew_PublishIntervalMustBeADuration(t *testing.T) {
	for _, v := range []string{"hourly", "-1s"} {
		if _, err := New(logr.Discard(), map[string]string{"publish_interval": v}); err == nil {
			t.Errorf("publish_interval %q should fail startup", v)
		}
	}
}

// backend adapts the provider's own backend access to the conformance suite.
type backend struct{ p *Provider }

// origins maps the suite's origins to ours; both list them in the same order, which toSuite relies on.
var origins = map[dnstest.Origin]Origin{dnstest.Managed: Managed, dnstest.Legacy: Legacy, dnstest.Foreign: Foreign}

func (b backend) Seed(_ *testing.T, r dnstest.Record) {
	b.p.Seed(Record{FQDN: r.FQDN, Value: r.Value, Origin: origins[r.Origin], Disabled: r.Disabled})
}

func (b backend) Stored(_ *testing.T, fqdn string) []dnstest.Record { return toSuite(b.p.Stored(fqdn)) }

func (b backend) Live(_ *testing.T, fqdn string) []dnstest.Record { return toSuite(b.p.Live(fqdn)) }

func toSuite(records []Record) []dnstest.Record {
	var out []dnstest.Record
	for _, r := range records {
		out = append(out, dnstest.Record{FQDN: r.FQDN, Value: r.Value, Origin: dnstest.Origin(r.Origin), Disabled: r.Disabled})
	}
	return out
}
