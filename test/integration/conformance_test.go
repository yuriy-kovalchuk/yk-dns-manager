package integration

import (
	"testing"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/dnstest"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/opnsense"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/testenv"
)

// TestConformance runs the provider conformance suite against OPNsense.
func TestConformance(t *testing.T) {
	dnstest.Run(t, func(t *testing.T) (dns.Provider, dnstest.Backend) {
		fake, url := newFake(t)
		return newProvider(t, url), opnsenseBackend{fake}
	})
}

// opnsenseBackend adapts the fake appliance to the conformance suite. The
// override description carries the ownership marker.
type opnsenseBackend struct{ fake *testenv.OPNsense }

var descriptions = map[dnstest.Origin]string{
	dnstest.Managed: opnsense.ManagedDescription,
	dnstest.Legacy:  opnsense.LegacyDescription,
	dnstest.Foreign: "configured by hand",
}

func (b opnsenseBackend) Seed(_ *testing.T, r dnstest.Record) {
	enabled := "1"
	if r.Disabled {
		enabled = "0"
	}
	host, domain := splitFQDN(r.FQDN)
	b.fake.Seed(testenv.HostOverride{
		Enabled: enabled, Hostname: host, Domain: domain, RR: "A",
		Server: r.Value, Description: descriptions[r.Origin],
	})
}

func (b opnsenseBackend) Stored(_ *testing.T, fqdn string) []dnstest.Record {
	return toRecords(rowsFor(b.fake.Snapshot(), fqdn))
}

func (b opnsenseBackend) Live(_ *testing.T, fqdn string) []dnstest.Record {
	return toRecords(rowsFor(b.fake.Published(), fqdn))
}

func toRecords(rows []testenv.HostOverride) []dnstest.Record {
	var out []dnstest.Record
	for _, h := range rows {
		origin := dnstest.Foreign
		switch h.Description {
		case opnsense.ManagedDescription:
			origin = dnstest.Managed
		case opnsense.LegacyDescription:
			origin = dnstest.Legacy
		}
		out = append(out, dnstest.Record{FQDN: h.FQDN(), Value: h.Server, Origin: origin, Disabled: h.Enabled != "1"})
	}
	return out
}
