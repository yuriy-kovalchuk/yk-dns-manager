// Package opnsense provides a DNS provider implementation for OPNsense Unbound.
package opnsense

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

const (
	searchPath      = "unbound/settings/searchHostOverride"
	addPath         = "unbound/settings/addHostOverride"
	setPath         = "unbound/settings/setHostOverride/"
	delPath         = "unbound/settings/delHostOverride/"
	reconfigurePath = "unbound/service/reconfigure"
)

// Bounds on how much of an OPNsense response body is read, so a hostile or
// misbehaving appliance cannot exhaust the pod's memory. 8MiB covers tens of
// thousands of override rows at roughly 200 bytes each.
const (
	maxResponseBytes = 8 << 20
	// Error bodies are only ever logged, so a short snippet is enough.
	maxErrorBodyBytes = 1 << 10
)

// Provider implements dns.Provider for OPNsense Unbound DNS.
type Provider struct {
	baseURL   string
	apiKey    string
	apiSecret string
	client    *http.Client
	log       logr.Logger

	// OPNsense keeps override edits in its staged config until reconfigure
	// publishes them. staged counts writes made through any batch; published
	// is the value staged had when the last successful reconfigure started.
	// A commit owes a publish while published < staged. Counting, rather than
	// a flag, keeps a write made during another batch's reconfigure from being
	// marked published by it, and keeps a failed publish owed until a later
	// commit succeeds.
	//
	// A new provider starts one write behind: a previous process may have
	// staged changes it never published, and nothing short of a reconfigure
	// can tell.
	mu        sync.Mutex
	staged    uint64
	published uint64
}

// New creates an OPNsense DNS provider from the given settings map.
func New(log logr.Logger, settings map[string]string) (*Provider, error) {
	for _, key := range []string{"base_url", "api_key", "api_secret"} {
		if settings[key] == "" {
			return nil, fmt.Errorf("opnsense: missing required setting '%s'", key)
		}
	}
	baseURL := settings["base_url"]
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("opnsense: invalid base_url %q: %w", baseURL, err)
	}
	if !strings.HasPrefix(parsedURL.Path, "/api") {
		return nil, fmt.Errorf("opnsense: base_url path must start with '/api' (e.g. https://opnsense.example.com/api), got %q", parsedURL.Path)
	}
	if err := checkScheme(parsedURL, settings, log); err != nil {
		return nil, err
	}
	if v := settings["default_ttl"]; v != "" {
		log.Info("opnsense: default_ttl is ignored and will be removed: Unbound host overrides have no per-record TTL", "default_ttl", v)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if err := applyTLSConfig(transport, settings, log); err != nil {
		return nil, err
	}

	return &Provider{
		baseURL:   strings.TrimRight(baseURL, "/") + "/",
		apiKey:    settings["api_key"],
		apiSecret: settings["api_secret"],
		client:    &http.Client{Timeout: 30 * time.Second, Transport: transport},
		log:       log,
		staged:    1,
	}, nil
}

// checkScheme refuses plain HTTP unless the operator opts in. The API key and
// secret go out as basic auth on every request, so over http they cross the
// network in cleartext. The opt-in exists for the in-memory fake used in local
// development.
func checkScheme(u *url.URL, settings map[string]string, log logr.Logger) error {
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if settings["allow_insecure_http"] != "true" {
			return fmt.Errorf("opnsense: base_url %q uses http, which sends the API credentials in cleartext; use https, or set allow_insecure_http: \"true\" for a local test appliance", u.Redacted())
		}
		log.Info("opnsense: allow_insecure_http is ENABLED - the API credentials are sent in cleartext. Use it only for a local test appliance.")
		return nil
	default:
		return fmt.Errorf("opnsense: base_url %q must use https", u.Redacted())
	}
}

// applyTLSConfig wires the two TLS knobs an operator controls. ca_file pins a
// private CA, the safe answer to a self-signed appliance. skip_tls_verify
// switches verification off and is loudly discouraged: it hands the basic-auth
// credentials to anyone on the network path.
func applyTLSConfig(transport *http.Transport, settings map[string]string, log logr.Logger) error {
	caFile := settings["ca_file"]
	skipVerify := settings["skip_tls_verify"] == "true"

	if caFile != "" && skipVerify {
		return fmt.Errorf("opnsense: ca_file and skip_tls_verify are mutually exclusive; pin the CA with ca_file instead of disabling verification")
	}
	if caFile == "" && !skipVerify {
		return nil
	}

	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		// #nosec G304 -- path comes from operator-set provider config, not request input
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return fmt.Errorf("opnsense: reading ca_file %q: %w", caFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("opnsense: ca_file %q contains no valid PEM certificate", caFile)
		}
		cfg.RootCAs = pool
		log.V(1).Info("opnsense: pinning appliance CA", "ca_file", caFile)
	}
	if skipVerify {
		log.Info("opnsense: skip_tls_verify is ENABLED - TLS certificates are not verified, so the API credentials are sent to anyone who answers on the network path. Set ca_file to pin the appliance CA instead.")
		cfg.InsecureSkipVerify = true // #nosec G402 -- deliberate operator opt-in, warned on every startup
	}
	transport.TLSClientConfig = cfg
	return nil
}

// call sends one API request and decodes the JSON answer into out, which may
// be nil. A non-200 status is an error carrying a bounded snippet of the body.
func (p *Provider) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("opnsense: marshal %s request: %w", path, err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("opnsense: build %s request: %w", path, err)
	}
	req.SetBasicAuth(p.apiKey, p.apiSecret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("opnsense: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("opnsense: %s: authentication failed (HTTP %d)", path, resp.StatusCode)
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return fmt.Errorf("opnsense: %s returned status %d: %s", path, resp.StatusCode, snippet)
	}
	if out == nil {
		return nil
	}

	// A body that hits the limit is reported as such instead of surfacing a
	// confusing truncated-JSON error.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("opnsense: read %s response: %w", path, err)
	}
	if len(data) == maxResponseBytes {
		return fmt.Errorf("opnsense: %s response exceeded the %d byte limit", path, maxResponseBytes)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("opnsense: decode %s response: %w", path, err)
	}
	return nil
}

// apiResult is the answer to every write: add, set and del report "result",
// add also the new "uuid", and reconfigure reports "status".
type apiResult struct {
	Result string `json:"result"`
	UUID   string `json:"uuid"`
	Status string `json:"status"`
}

// write POSTs body to path and requires one of the accepted results. OPNsense
// answers a rejected write with HTTP 200, so the status code alone proves
// nothing.
func (p *Provider) write(ctx context.Context, path string, body any, accepted ...string) (apiResult, error) {
	var res apiResult
	if err := p.call(ctx, http.MethodPost, path, body, &res); err != nil {
		return res, err
	}
	if !slices.Contains(accepted, res.Result) {
		return res, fmt.Errorf("opnsense: %s unexpected result: %s", path, res.Result)
	}
	return res, nil
}

// HealthCheck verifies the OPNsense API is reachable and credentials are valid.
func (p *Provider) HealthCheck(ctx context.Context) error {
	return p.call(ctx, http.MethodGet, searchPath, nil, nil)
}

// publish reconfigures if any staged write is not yet published.
func (p *Provider) publish(ctx context.Context) error {
	p.mu.Lock()
	target := p.staged
	owed := p.published < target
	p.mu.Unlock()
	if !owed {
		return nil
	}

	var res apiResult
	if err := p.call(ctx, http.MethodPost, reconfigurePath, struct{}{}, &res); err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(res.Status), "ok") {
		return fmt.Errorf("opnsense: reconfigure returned status %q", res.Status)
	}
	p.log.V(1).Info("reconfigure completed")

	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = max(p.published, target)
	return nil
}

func (p *Provider) markStaged() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.staged++
}

// hostRow is one host override: a row of the search response, and with an
// empty UUID the body of an add or set.
type hostRow struct {
	UUID        string `json:"uuid,omitempty"`
	Enabled     string `json:"enabled"`
	Hostname    string `json:"hostname"`
	Domain      string `json:"domain"`
	RR          string `json:"rr"`
	Server      string `json:"server"`
	Description string `json:"description"`
	MXPrio      string `json:"mxprio"`
	MX          string `json:"mx"`
}

// rowFor is the override record asks for.
func rowFor(record dns.Record) hostRow {
	host, domain := splitHostname(record.Hostname)
	return hostRow{
		Enabled: "1", Hostname: host, Domain: domain, RR: record.Type,
		Server: record.Value, Description: record.Meta["description"],
	}
}

// splitHostname splits an FQDN into OPNsense's hostname and domain fields:
// "sub.app.example.com" becomes ("sub", "app.example.com").
func splitHostname(fqdn string) (hostname, domain string) {
	hostname, domain, _ = strings.Cut(strings.TrimSuffix(fqdn, "."), ".")
	return hostname, domain
}

func (r hostRow) enabled() bool { return r.Enabled == "1" }

// is reports whether the row is the override want names.
func (r hostRow) is(want hostRow) bool {
	return strings.EqualFold(r.Hostname, want.Hostname) &&
		strings.EqualFold(r.Domain, want.Domain) &&
		strings.EqualFold(r.RR, want.RR)
}

// matches reports whether the row already says what want says.
func (r hostRow) matches(want hostRow) bool {
	return r.enabled() && r.Server == want.Server && r.Description == want.Description
}

// Batch starts a unit of work against the appliance.
func (p *Provider) Batch() dns.Batch {
	return &batch{p: p}
}

// batch is one reconcile's view of the appliance. It lists the overrides once,
// on first use, and keeps that list in step with its own writes, so later
// calls in the same batch see them.
type batch struct {
	p      *Provider
	used   bool
	loaded bool
	rows   []hostRow
}

func (b *batch) load(ctx context.Context) error {
	b.used = true
	if b.loaded {
		return nil
	}
	var res struct {
		Rows []hostRow `json:"rows"`
	}
	if err := b.p.call(ctx, http.MethodGet, searchPath, nil, &res); err != nil {
		return err
	}
	b.rows, b.loaded = res.Rows, true
	return nil
}

func (b *batch) find(want hostRow) []hostRow {
	var out []hostRow
	for _, r := range b.rows {
		if r.is(want) {
			out = append(out, r)
		}
	}
	return out
}

func (b *batch) remove(ctx context.Context, uuid string) error {
	// "not found" means someone else already deleted it, which is the goal.
	if _, err := b.p.write(ctx, delPath+url.PathEscape(uuid), struct{}{}, "deleted", "not found"); err != nil {
		return err
	}
	b.p.markStaged()
	b.rows = slices.DeleteFunc(b.rows, func(r hostRow) bool { return r.UUID == uuid })
	return nil
}

// Ensure implements dns.Batch.
//
// OPNsense accepts several overrides for one name, and a stale duplicate keeps
// answering after the live one is updated. Whenever Ensure writes, it first
// removes every override for the name except the one it keeps.
func (b *batch) Ensure(ctx context.Context, record dns.Record, overwrite bool) (dns.Result, error) {
	if err := b.load(ctx); err != nil {
		return 0, err
	}
	want := rowFor(record)

	existing := b.find(want)
	if len(existing) == 0 {
		res, err := b.p.write(ctx, addPath, map[string]hostRow{"host": want}, "saved")
		if err != nil {
			return 0, err
		}
		b.p.markStaged()
		want.UUID = res.UUID
		b.rows = append(b.rows, want)
		return dns.Created, nil
	}

	keep := existing[0]
	if i := slices.IndexFunc(existing, hostRow.enabled); i >= 0 {
		keep = existing[i]
	}

	// A disabled override answers nothing, so it does not count as a live
	// record that create-only mode must leave alone.
	if !overwrite && keep.enabled() {
		if keep.matches(want) {
			return dns.Unchanged, nil
		}
		return dns.Kept, nil
	}

	for _, r := range existing {
		if r.UUID != keep.UUID {
			if err := b.remove(ctx, r.UUID); err != nil {
				return 0, err
			}
		}
	}
	if removed := len(existing) - 1; removed > 0 {
		b.p.log.Info("removed duplicate host overrides", "hostname", record.Hostname, "removed", removed)
	}

	if keep.matches(want) {
		if len(existing) > 1 {
			return dns.Updated, nil
		}
		return dns.Unchanged, nil
	}
	if _, err := b.p.write(ctx, setPath+url.PathEscape(keep.UUID), map[string]hostRow{"host": want}, "saved"); err != nil {
		return 0, err
	}
	b.p.markStaged()
	want.UUID = keep.UUID
	b.rows[slices.IndexFunc(b.rows, func(r hostRow) bool { return r.UUID == keep.UUID })] = want
	return dns.Updated, nil
}

// Remove implements dns.Batch.
func (b *batch) Remove(ctx context.Context, hostname, recordType string) error {
	if err := b.load(ctx); err != nil {
		return err
	}
	for _, r := range b.find(rowFor(dns.Record{Hostname: hostname, Type: recordType})) {
		if err := b.remove(ctx, r.UUID); err != nil {
			return err
		}
	}
	return nil
}

// Commit implements dns.Batch.
func (b *batch) Commit(ctx context.Context) error {
	if !b.used {
		return nil
	}
	return b.p.publish(ctx)
}
