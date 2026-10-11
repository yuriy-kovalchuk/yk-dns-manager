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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

// Provider implements dns.Provider for OPNsense Unbound DNS.
type Provider struct {
	baseURL    string
	apiKey     string
	apiSecret  string
	defaultTTL int
	client     *http.Client
	log        logr.Logger

	// mu guards pending, which records that overrides were staged but not yet
	// published by reconfigure. Concurrent flushes are harmless: reconfigure
	// publishes whatever the appliance currently holds.
	mu      sync.Mutex
	pending bool
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

	defaultTTL := 300
	if v := settings["default_ttl"]; v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("opnsense: invalid default_ttl %q: %w", v, err)
		}
		defaultTTL = parsed
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if err := applyTLSConfig(transport, settings, log); err != nil {
		return nil, err
	}

	return &Provider{
		baseURL:    baseURL,
		apiKey:     apiKey,
		apiSecret:  apiSecret,
		defaultTTL: defaultTTL,
		client:     &http.Client{Timeout: 30 * time.Second, Transport: transport},
		log:        log,
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
	// Context cancellation is not an error — return nil immediately.
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

// reconfigure tells OPNsense to apply DNS changes.
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
	p.log.V(1).Info("reconfigure completed", "status", result.Status)
	return nil
}

// searchResponse is the shape returned by searchHostOverride.
type searchResponse struct {
	Rows []hostRow `json:"rows"`
}

// hostRow represents a single host override row from the search response.
type hostRow struct {
	UUID     string `json:"uuid"`
	Enabled  string `json:"enabled"`
	Hostname string `json:"hostname"`
	Domain   string `json:"domain"`
	RR       string `json:"rr"`
	Server   string `json:"server"`
}

// findOverrides returns the UUID of every host override matching fqdn/recordType.
// OPNsense accepts several overrides for the same name, so more than one is
// possible and every caller has to account for it.
func (p *Provider) findOverrides(ctx context.Context, fqdn, recordType string) ([]string, error) {
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

	host, domain := dns.SplitHostname(fqdn)
	var uuids []string
	for _, row := range sr.Rows {
		if strings.EqualFold(row.Hostname, host) &&
			strings.EqualFold(row.Domain, domain) &&
			strings.EqualFold(row.RR, recordType) {
			uuids = append(uuids, row.UUID)
		}
	}
	return uuids, nil
}

// convergeDuplicates deletes every override after the first. A stale duplicate
// keeps answering after the live one is updated, so the extra rows have to go.
func (p *Provider) convergeDuplicates(ctx context.Context, hostname string, uuids []string) error {
	if len(uuids) <= 1 {
		return nil
	}
	for _, uuid := range uuids[1:] {
		if err := p.deleteOverride(ctx, uuid); err != nil {
			return err
		}
	}
	p.log.Info("removed duplicate host overrides", "hostname", hostname, "removed", len(uuids)-1)
	return nil
}

// buildHostBody creates the JSON body for add/set host override calls.
func buildHostBody(record dns.Record) map[string]interface{} {
	host, domain := dns.SplitHostname(record.Hostname)
	description := ""
	if record.Meta != nil {
		description = record.Meta["description"]
	}
	return map[string]interface{}{
		"host": map[string]string{
			"enabled":     "1",
			"hostname":    host,
			"domain":      domain,
			"rr":          record.Type,
			"server":      record.Value,
			"description": description,
			"mxprio":      "",
			"mx":          "",
		},
	}
}

// Exists checks whether a DNS host override exists for the given hostname and record type.
func (p *Provider) Exists(ctx context.Context, hostname, recordType string) (bool, error) {
	p.log.V(1).Info("checking if record exists", "hostname", hostname, "type", recordType)
	uuids, err := p.findOverrides(ctx, hostname, recordType)
	if err != nil {
		return false, err
	}
	return len(uuids) > 0, nil
}

// Create adds a new DNS host override. The change is staged; call Flush to
// publish it.
func (p *Provider) Create(ctx context.Context, record dns.Record) error {
	p.log.V(1).Info("creating record", "hostname", record.Hostname, "type", record.Type, "value", record.Value)
	if err := p.createOverride(ctx, record); err != nil {
		return err
	}
	p.setPending()
	return nil
}

func (p *Provider) createOverride(ctx context.Context, record dns.Record) error {
	body := buildHostBody(record)
	resp, err := p.doRequest(ctx, http.MethodPost, "unbound/settings/addHostOverride", body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opnsense: addHostOverride returned status %d: %s", resp.StatusCode, readErrorBody(resp))
	}

	var result struct {
		Result string `json:"result"`
		UUID   string `json:"uuid"`
	}
	if err := decodeJSON(resp, &result, "addHostOverride"); err != nil {
		return err
	}
	if result.Result != "saved" {
		return fmt.Errorf("opnsense: addHostOverride unexpected result: %s", result.Result)
	}

	p.log.V(1).Info("record created", "uuid", result.UUID)
	return nil
}

// Update modifies an existing DNS host override. The change is staged; call
// Flush to publish it.
func (p *Provider) Update(ctx context.Context, record dns.Record) error {
	p.log.V(1).Info("updating record", "hostname", record.Hostname, "type", record.Type, "value", record.Value)

	uuids, err := p.findOverrides(ctx, record.Hostname, record.Type)
	if err != nil {
		return err
	}
	if len(uuids) == 0 {
		return fmt.Errorf("opnsense: no existing override found for %s/%s", record.Hostname, record.Type)
	}
	if err := p.convergeDuplicates(ctx, record.Hostname, uuids); err != nil {
		return err
	}
	if err := p.updateOverride(ctx, uuids[0], record); err != nil {
		return err
	}
	p.setPending()
	return nil
}

func (p *Provider) updateOverride(ctx context.Context, uuid string, record dns.Record) error {
	body := buildHostBody(record)
	resp, err := p.doRequest(ctx, http.MethodPost, fmt.Sprintf("unbound/settings/setHostOverride/%s", uuid), body)
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

	p.log.V(1).Info("record updated", "uuid", uuid)
	return nil
}

// Delete removes a DNS host override.
// Delete removes every DNS host override matching hostname/recordType. The
// changes are staged; call Flush to publish them.
func (p *Provider) Delete(ctx context.Context, hostname, recordType string) error {
	p.log.V(1).Info("deleting record", "hostname", hostname, "type", recordType)

	uuids, err := p.findOverrides(ctx, hostname, recordType)
	if err != nil {
		return err
	}
	if len(uuids) == 0 {
		p.log.V(1).Info("opnsense: no existing override found for deletion", "hostname", hostname, "type", recordType)
		return nil
	}

	// Deleting only the first match left the remaining overrides answering with
	// stale values, and nothing else would ever remove them.
	for _, uuid := range uuids {
		if err := p.deleteOverride(ctx, uuid); err != nil {
			return err
		}
	}
	p.log.V(1).Info("overrides deleted", "hostname", hostname, "count", len(uuids))
	p.setPending()
	return nil
}

func (p *Provider) deleteOverride(ctx context.Context, uuid string) error {
	resp, err := p.doRequest(ctx, http.MethodPost, fmt.Sprintf("unbound/settings/delHostOverride/%s", uuid), struct{}{})
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
		p.log.V(1).Info("record deleted", "uuid", uuid)
	case "not found":
		p.log.V(1).Info("record already deleted", "uuid", uuid)
	default:
		return fmt.Errorf("opnsense: delHostOverride unexpected result: %s", result.Result)
	}
	return nil
}

// Upsert creates or updates a DNS record, resolving the existing override once.
// The change is staged; call Flush to publish it.
func (p *Provider) Upsert(ctx context.Context, record dns.Record) error {
	uuids, err := p.findOverrides(ctx, record.Hostname, record.Type)
	if err != nil {
		return fmt.Errorf("opnsense: upsert lookup: %w", err)
	}

	if len(uuids) == 0 {
		if err := p.createOverride(ctx, record); err != nil {
			return err
		}
		p.setPending()
		return nil
	}

	if err := p.convergeDuplicates(ctx, record.Hostname, uuids); err != nil {
		return err
	}
	if err := p.updateOverride(ctx, uuids[0], record); err != nil {
		return err
	}
	p.setPending()
	return nil
}

// Flush publishes staged overrides with a single reconfigure. OPNsense keeps
// edits in its model config and only republishes them on reconfigure, so one
// call per reconcile is enough; doing it per record multiplied a global
// operation by the number of hostnames on the route.
func (p *Provider) Flush(ctx context.Context) error {
	p.mu.Lock()
	pending := p.pending
	p.mu.Unlock()
	if !pending {
		return nil
	}
	if err := p.reconfigure(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	p.pending = false
	p.mu.Unlock()
	return nil
}

func (p *Provider) setPending() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = true
}
