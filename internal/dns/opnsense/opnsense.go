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
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
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
// Returns (nil, err) if mandatory settings are missing.
func New(log logr.Logger, settings map[string]string) (*Provider, error) {
	baseURL := settings["base_url"]
	if baseURL == "" {
		return nil, fmt.Errorf("opnsense: missing required setting 'base_url'")
	}
	// Validate that base_url includes the /api path segment.
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("opnsense: invalid base_url %q: %w", baseURL, err)
	}
	if !strings.HasPrefix(parsedURL.Path, "/api") {
		return nil, fmt.Errorf(
			"opnsense: base_url path must start with '/api' (e.g. https://opnsense.example.com/api), got %q",
			parsedURL.Path,
		)
	}
	if err := checkScheme(parsedURL, settings, log); err != nil {
		return nil, err
	}
	apiKey := settings["api_key"]
	if apiKey == "" {
		return nil, fmt.Errorf("opnsense: missing required setting 'api_key'")
	}
	apiSecret := settings["api_secret"]
	if apiSecret == "" {
		return nil, fmt.Errorf("opnsense: missing required setting 'api_secret'")
	}

	if v := settings["default_ttl"]; v != "" {
		log.Info("opnsense: default_ttl is ignored and will be removed: Unbound host overrides have no per-record TTL", "default_ttl", v)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if err := applyTLSConfig(transport, settings, log); err != nil {
		return nil, err
	}

	return &Provider{
		baseURL:   baseURL,
		apiKey:    apiKey,
		apiSecret: apiSecret,
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

// applyTLSConfig wires the two TLS knobs an operator controls.
//
// ca_file pins a private CA, which is the safe answer to a self-signed
// appliance. skip_tls_verify switches verification off entirely and is loudly
// discouraged: it removes the only defence against anyone on the network path
// reading or forging the basic-auth credentials sent with every request.
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

// doRequest builds and executes an HTTP request against the OPNsense API.
func (p *Provider) doRequest(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	// Fail fast on a cancelled context instead of building a request.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("opnsense: marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	url := strings.TrimRight(p.baseURL, "/") + "/" + strings.TrimLeft(path, "/")
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("opnsense: build request: %w", err)
	}

	req.SetBasicAuth(p.apiKey, p.apiSecret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opnsense: %s %s: %w", method, path, err)
	}
	return resp, nil
}

// Bounds on how much of an OPNsense response body is read. Every read here was
// unbounded while the pod carries a 256Mi memory limit, so a hostile or
// misbehaving appliance could exhaust it. 8MiB is generous for the override
// list — a row is roughly 200 bytes of JSON, so this covers tens of thousands
// of entries — while still failing fast on a runaway response.
const (
	maxResponseBytes = 8 << 20
	// Error bodies are only ever logged, so a short snippet is enough.
	maxErrorBodyBytes = 1 << 10
)

// decodeJSON reads at most maxResponseBytes of the response body into target.
// A body that hits the limit is reported as such instead of surfacing a
// confusing truncated-JSON error.
func decodeJSON(resp *http.Response, target any, what string) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("opnsense: read %s response: %w", what, err)
	}
	if len(data) == maxResponseBytes {
		return fmt.Errorf("opnsense: %s response exceeded the %d byte limit", what, maxResponseBytes)
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("opnsense: decode %s response: %w", what, err)
	}
	return nil
}

func readErrorBody(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	return string(body)
}

// HealthCheck verifies the OPNsense API is reachable and credentials are valid.
func (p *Provider) HealthCheck(ctx context.Context) error {
	resp, err := p.doRequest(ctx, http.MethodGet, "unbound/settings/searchHostOverride", nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("opnsense: authentication failed (HTTP %d)", resp.StatusCode)
	default:
		return fmt.Errorf("opnsense: health check failed (HTTP %d)", resp.StatusCode)
	}
}

// reconfigure tells OPNsense to publish its staged config to Unbound.
func (p *Provider) reconfigure(ctx context.Context) error {
	resp, err := p.doRequest(ctx, http.MethodPost, "unbound/service/reconfigure", struct{}{})
	if err != nil {
		return fmt.Errorf("opnsense: reconfigure: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opnsense: reconfigure returned status %d", resp.StatusCode)
	}

	var result struct {
		Status string `json:"status"`
	}
	if err := decodeJSON(resp, &result, "reconfigure"); err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(result.Status), "ok") {
		return fmt.Errorf("opnsense: reconfigure returned status %q", result.Status)
	}
	p.log.V(1).Info("reconfigure completed")
	return nil
}

func (p *Provider) markStaged() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.staged++
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

	if err := p.reconfigure(ctx); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if target > p.published {
		p.published = target
	}
	return nil
}

// searchResponse is the shape returned by searchHostOverride.
type searchResponse struct {
	Rows []hostRow `json:"rows"`
}

// hostRow represents a single host override row from the search response.
type hostRow struct {
	UUID        string `json:"uuid"`
	Enabled     string `json:"enabled"`
	Hostname    string `json:"hostname"`
	Domain      string `json:"domain"`
	RR          string `json:"rr"`
	Server      string `json:"server"`
	Description string `json:"description"`
}

func (r hostRow) enabled() bool { return r.Enabled == "1" }

// is reports whether the row is the override for fqdn and recordType.
func (r hostRow) is(fqdn, recordType string) bool {
	host, domain := dns.SplitHostname(fqdn)
	return strings.EqualFold(r.Hostname, host) &&
		strings.EqualFold(r.Domain, domain) &&
		strings.EqualFold(r.RR, recordType)
}

// matches reports whether the row already says what record wants.
func (r hostRow) matches(record dns.Record) bool {
	return r.enabled() && r.Server == record.Value && r.Description == description(record)
}

func description(record dns.Record) string {
	if record.Meta == nil {
		return ""
	}
	return record.Meta["description"]
}

func rowFor(uuid string, record dns.Record) hostRow {
	host, domain := dns.SplitHostname(record.Hostname)
	return hostRow{
		UUID: uuid, Enabled: "1", Hostname: host, Domain: domain,
		RR: record.Type, Server: record.Value, Description: description(record),
	}
}

// listOverrides fetches every host override on the appliance.
func (p *Provider) listOverrides(ctx context.Context) ([]hostRow, error) {
	resp, err := p.doRequest(ctx, http.MethodGet, "unbound/settings/searchHostOverride", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("opnsense: searchHostOverride returned status %d", resp.StatusCode)
	}

	var sr searchResponse
	if err := decodeJSON(resp, &sr, "search"); err != nil {
		return nil, err
	}
	return sr.Rows, nil
}

// buildHostBody creates the JSON body for add/set host override calls.
func buildHostBody(record dns.Record) map[string]interface{} {
	host, domain := dns.SplitHostname(record.Hostname)
	return map[string]interface{}{
		"host": map[string]string{
			"enabled":     "1",
			"hostname":    host,
			"domain":      domain,
			"rr":          record.Type,
			"server":      record.Value,
			"description": description(record),
			"mxprio":      "",
			"mx":          "",
		},
	}
}

func (p *Provider) addOverride(ctx context.Context, record dns.Record) (string, error) {
	resp, err := p.doRequest(ctx, http.MethodPost, "unbound/settings/addHostOverride", buildHostBody(record))
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("opnsense: addHostOverride returned status %d: %s", resp.StatusCode, readErrorBody(resp))
	}

	var result struct {
		Result string `json:"result"`
		UUID   string `json:"uuid"`
	}
	if err := decodeJSON(resp, &result, "addHostOverride"); err != nil {
		return "", err
	}
	if result.Result != "saved" {
		return "", fmt.Errorf("opnsense: addHostOverride unexpected result: %s", result.Result)
	}

	p.log.V(1).Info("override added", "hostname", record.Hostname, "uuid", result.UUID)
	return result.UUID, nil
}

func (p *Provider) setOverride(ctx context.Context, uuid string, record dns.Record) error {
	resp, err := p.doRequest(ctx, http.MethodPost, "unbound/settings/setHostOverride/"+url.PathEscape(uuid), buildHostBody(record))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opnsense: setHostOverride returned status %d: %s", resp.StatusCode, readErrorBody(resp))
	}

	var result struct {
		Result string `json:"result"`
	}
	if err := decodeJSON(resp, &result, "setHostOverride"); err != nil {
		return err
	}
	if result.Result != "saved" {
		return fmt.Errorf("opnsense: setHostOverride unexpected result: %s", result.Result)
	}

	p.log.V(1).Info("override updated", "hostname", record.Hostname, "uuid", uuid)
	return nil
}

func (p *Provider) deleteOverride(ctx context.Context, uuid string) error {
	resp, err := p.doRequest(ctx, http.MethodPost, "unbound/settings/delHostOverride/"+url.PathEscape(uuid), struct{}{})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opnsense: delHostOverride returned status %d: %s", resp.StatusCode, readErrorBody(resp))
	}

	var result struct {
		Result string `json:"result"`
	}
	if err := decodeJSON(resp, &result, "delHostOverride"); err != nil {
		return err
	}
	switch result.Result {
	case "deleted":
		p.log.V(1).Info("override deleted", "uuid", uuid)
	case "not found":
		p.log.V(1).Info("override already deleted", "uuid", uuid)
	default:
		return fmt.Errorf("opnsense: delHostOverride unexpected result: %s", result.Result)
	}
	return nil
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
	rows, err := b.p.listOverrides(ctx)
	if err != nil {
		return err
	}
	b.rows, b.loaded = rows, true
	return nil
}

func (b *batch) find(fqdn, recordType string) []hostRow {
	var out []hostRow
	for _, r := range b.rows {
		if r.is(fqdn, recordType) {
			out = append(out, r)
		}
	}
	return out
}

func (b *batch) remove(ctx context.Context, uuid string) error {
	if err := b.p.deleteOverride(ctx, uuid); err != nil {
		return err
	}
	b.p.markStaged()
	for i, r := range b.rows {
		if r.UUID == uuid {
			b.rows = append(b.rows[:i], b.rows[i+1:]...)
			break
		}
	}
	return nil
}

func (b *batch) replace(row hostRow) {
	for i, r := range b.rows {
		if r.UUID == row.UUID {
			b.rows[i] = row
			return
		}
	}
	b.rows = append(b.rows, row)
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

	existing := b.find(record.Hostname, record.Type)
	if len(existing) == 0 {
		uuid, err := b.p.addOverride(ctx, record)
		if err != nil {
			return 0, err
		}
		b.p.markStaged()
		b.replace(rowFor(uuid, record))
		return dns.Created, nil
	}

	keep := existing[0]
	for _, r := range existing {
		if r.enabled() {
			keep = r
			break
		}
	}

	// A disabled override answers nothing, so it does not count as a live
	// record that create-only mode must leave alone.
	if !overwrite && keep.enabled() {
		if keep.matches(record) {
			return dns.Unchanged, nil
		}
		return dns.Kept, nil
	}

	removed := 0
	for _, r := range existing {
		if r.UUID == keep.UUID {
			continue
		}
		if err := b.remove(ctx, r.UUID); err != nil {
			return 0, err
		}
		removed++
	}
	if removed > 0 {
		b.p.log.Info("removed duplicate host overrides", "hostname", record.Hostname, "removed", removed)
	}

	if keep.matches(record) {
		if removed > 0 {
			return dns.Updated, nil
		}
		return dns.Unchanged, nil
	}
	if err := b.p.setOverride(ctx, keep.UUID, record); err != nil {
		return 0, err
	}
	b.p.markStaged()
	b.replace(rowFor(keep.UUID, record))
	return dns.Updated, nil
}

// Remove implements dns.Batch.
func (b *batch) Remove(ctx context.Context, hostname, recordType string) error {
	if err := b.load(ctx); err != nil {
		return err
	}
	for _, r := range b.find(hostname, recordType) {
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
