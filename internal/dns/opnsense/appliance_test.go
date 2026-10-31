package opnsense

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/dnstest"
)

// TestConformanceAgainstAppliance runs the provider conformance suite against a real OPNsense, which no fake can stand in for. It needs OPNSENSE_TEST_URL (https://<appliance>/api), OPNSENSE_TEST_KEY and OPNSENSE_TEST_SECRET, plus OPNSENSE_TEST_CA_FILE for a self-signed appliance. It deletes every override for dnstest.Names, so point it at a test appliance.
func TestConformanceAgainstAppliance(t *testing.T) {
	baseURL := os.Getenv("OPNSENSE_TEST_URL")
	if baseURL == "" {
		t.Skip("OPNSENSE_TEST_URL is not set")
	}
	settings := map[string]string{
		"base_url":   baseURL,
		"api_key":    os.Getenv("OPNSENSE_TEST_KEY"),
		"api_secret": os.Getenv("OPNSENSE_TEST_SECRET"),
		"ca_file":    os.Getenv("OPNSENSE_TEST_CA_FILE"),
	}
	dnstest.Run(t, func(t *testing.T) (dns.Provider, dnstest.Backend) {
		p, err := New(logr.Discard(), settings)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		be := appliance{p}
		be.clear(t)
		t.Cleanup(func() { be.clear(t) })
		return p, be
	})
}

// appliance is the suite's direct access to the appliance, through the same API the provider uses; the override description carries the ownership marker.
type appliance struct{ p *Provider }

var descriptions = map[dnstest.Origin]string{
	dnstest.Managed: ManagedDescription,
	dnstest.Legacy:  LegacyDescription,
	dnstest.Foreign: "configured by hand",
}

func (a appliance) Seed(t *testing.T, r dnstest.Record) {
	t.Helper()
	row := rowFor(dns.Record{Hostname: r.FQDN, Type: "A", Value: r.Value})
	row.Description = descriptions[r.Origin]
	if r.Disabled {
		row.Enabled = "0"
	}
	ctx := context.Background()
	if _, err := a.p.write(ctx, addPath, row, "saved"); err != nil {
		t.Fatalf("seeding %s: %v", r.FQDN, err)
	}
	if err := a.p.reconfigure(ctx); err != nil {
		t.Fatalf("publishing the seed: %v", err)
	}
}

func (a appliance) Stored(t *testing.T, fqdn string) []dnstest.Record {
	t.Helper()
	var out []dnstest.Record
	for _, r := range a.rows(t, fqdn) {
		origin := dnstest.Foreign
		switch {
		case isManaged(r.Description):
			origin = dnstest.Managed
		case r.Description == LegacyDescription:
			origin = dnstest.Legacy
		}
		out = append(out, dnstest.Record{FQDN: fqdn, Value: r.Server, Origin: origin, Disabled: r.Enabled != "1"})
	}
	return out
}

// Live is Stored: the API lists the staged configuration only, and every check of live records follows a successful commit.
func (a appliance) Live(t *testing.T, fqdn string) []dnstest.Record { return a.Stored(t, fqdn) }

func (a appliance) rows(t *testing.T, fqdn string) []hostRow {
	t.Helper()
	var res struct {
		Rows []hostRow `json:"rows"`
	}
	if err := a.p.call(context.Background(), http.MethodGet, searchPath, nil, &res); err != nil {
		t.Fatalf("listing overrides: %v", err)
	}
	return slices.DeleteFunc(res.Rows, func(r hostRow) bool {
		return !strings.EqualFold(r.Hostname+"."+r.Domain, fqdn)
	})
}

// clear deletes every override for the suite's names and publishes, so each test starts from an empty backend.
func (a appliance) clear(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, fqdn := range dnstest.Names {
		for _, r := range a.rows(t, fqdn) {
			if _, err := a.p.write(ctx, delPath+url.PathEscape(r.UUID), nil, "deleted", "not found"); err != nil {
				t.Fatalf("clearing %s: %v", fqdn, err)
			}
		}
	}
	if err := a.p.reconfigure(ctx); err != nil {
		t.Fatalf("publishing the cleanup: %v", err)
	}
}
