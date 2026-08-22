// Package opnsense provides a DNS provider implementation for OPNsense Unbound.
package opnsense

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

// reconfigureAttempts and reconfigureDelay bound the retry around
// unbound/service/reconfigure after each mutation.
const (
	reconfigureAttempts = 3
	reconfigureDelay    = 500 * time.Millisecond
)

// Provider implements dns.Provider for OPNsense Unbound DNS.
type Provider struct {
	baseURL   string
	apiKey    string
	apiSecret string
	client    *http.Client
	log       logr.Logger
}

// New creates an OPNsense DNS provider from the given settings map and
// credentials.
//
// Credentials: this provider expects the Kubernetes Secret (named by the
// instance's `secret` config field) to contain the keys API_KEY and
// API_SECRET. Returns (nil, err) if mandatory settings or credential keys
// are missing.
func New(log logr.Logger, settings map[string]string, creds *dns.Credentials) (*Provider, error) {
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
	if creds == nil {
		return nil, fmt.Errorf("opnsense: no credentials provided — set the instance's 'secret' field to a Kubernetes Secret containing keys API_KEY and API_SECRET")
	}
	var missing []string
	if creds.SecretKey("API_KEY") == "" {
		missing = append(missing, "API_KEY")
	}
	if creds.SecretKey("API_SECRET") == "" {
		missing = append(missing, "API_SECRET")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("opnsense: secret %q is missing credential key(s) %s", creds.SecretName, strings.Join(missing, ", "))
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if v := settings["skip_tls_verify"]; v == "true" {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	return &Provider{
		baseURL:   baseURL,
		apiKey:    creds.SecretKey("API_KEY"),
		apiSecret: creds.SecretKey("API_SECRET"),
		client:    &http.Client{Timeout: 30 * time.Second, Transport: transport},
		log:       log,
	}, nil
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

// reconfigure tells OPNsense to apply the persisted DNS changes, retrying
// on transient failures.
func (p *Provider) reconfigure(ctx context.Context) error {
	var lastErr error
	for attempt := 1; attempt <= reconfigureAttempts; attempt++ {
		lastErr = p.doReconfigure(ctx)
		if lastErr == nil {
			return nil
		}
		p.log.Info("reconfigure attempt failed", "attempt", attempt, "error", lastErr.Error())
		if attempt == reconfigureAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(reconfigureDelay):
		}
	}
	return fmt.Errorf("opnsense: reconfigure failed after %d attempts: %w", reconfigureAttempts, lastErr)
}

func (p *Provider) doReconfigure(ctx context.Context) error {
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
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("opnsense: decode reconfigure response: %w", err)
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

// findOverride searches for an existing host override matching hostname and
// record type, returning the row's UUID and enabled state. The UUID is empty
// when no row matches. The API has no per-hostname filter, so the full
// table is fetched and filtered here.
func (p *Provider) findOverride(ctx context.Context, fqdn, recordType string) (string, bool, error) {
	resp, err := p.doRequest(ctx, http.MethodGet, "unbound/settings/searchHostOverride", nil)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("opnsense: searchHostOverride returned status %d", resp.StatusCode)
	}

	var sr searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return "", false, fmt.Errorf("opnsense: decode search response: %w", err)
	}

	host, domain := dns.SplitHostname(fqdn)
	for _, row := range sr.Rows {
		if strings.EqualFold(row.Hostname, host) &&
			strings.EqualFold(row.Domain, domain) &&
			strings.EqualFold(row.RR, recordType) {
			return row.UUID, row.Enabled == "1", nil
		}
	}
	return "", false, nil
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

// Exists checks whether an enabled DNS host override exists for the given
// hostname and record type. A disabled override does not resolve, so it
// counts as absent.
func (p *Provider) Exists(ctx context.Context, hostname, recordType string) (bool, error) {
	p.log.V(1).Info("checking if record exists", "hostname", hostname, "type", recordType)
	uuid, enabled, err := p.findOverride(ctx, hostname, recordType)
	if err != nil {
		return false, err
	}
	return uuid != "" && enabled, nil
}

// Create adds a new DNS host override. If a disabled override for the same
// hostname/type exists (e.g. manually disabled in the UI), it is re-enabled
// in place instead of adding a duplicate.
func (p *Provider) Create(ctx context.Context, record dns.Record) error {
	p.log.V(1).Info("creating record", "hostname", record.Hostname, "type", record.Type, "value", record.Value)

	uuid, enabled, err := p.findOverride(ctx, record.Hostname, record.Type)
	if err != nil {
		return err
	}
	if uuid != "" {
		if enabled {
			return fmt.Errorf("opnsense: override for %s/%s already exists", record.Hostname, record.Type)
		}
		return p.Update(ctx, record)
	}
	return p.addOverride(ctx, record)
}

// callResult executes a mutation call and returns the parsed
// {"result", "uuid"} response.
func (p *Provider) callResult(ctx context.Context, method, path string, body interface{}) (string, string, error) {
	resp, err := p.doRequest(ctx, method, path, body)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("opnsense: %s returned status %d: %s", path, resp.StatusCode, string(respBody))
	}

	var out struct {
		Result string `json:"result"`
		UUID   string `json:"uuid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", fmt.Errorf("opnsense: decode %s response: %w", path, err)
	}
	return out.Result, out.UUID, nil
}

// addOverride performs the raw addHostOverride call and applies the change.
func (p *Provider) addOverride(ctx context.Context, record dns.Record) error {
	result, uuid, err := p.callResult(ctx, http.MethodPost, "unbound/settings/addHostOverride", buildHostBody(record))
	if err != nil {
		return err
	}
	if result != "saved" {
		return fmt.Errorf("opnsense: addHostOverride unexpected result: %s", result)
	}

	p.log.V(1).Info("record created", "uuid", uuid)
	return p.reconfigure(ctx)
}

// Update modifies an existing DNS host override. The full body is always
// sent with enabled="1", so a disabled override is re-enabled by this call.
func (p *Provider) Update(ctx context.Context, record dns.Record) error {
	p.log.V(1).Info("updating record", "hostname", record.Hostname, "type", record.Type, "value", record.Value)

	uuid, _, err := p.findOverride(ctx, record.Hostname, record.Type)
	if err != nil {
		return err
	}
	if uuid == "" {
		return fmt.Errorf("opnsense: no existing override found for %s/%s", record.Hostname, record.Type)
	}

	result, _, err := p.callResult(ctx, http.MethodPost, fmt.Sprintf("unbound/settings/setHostOverride/%s", uuid), buildHostBody(record))
	if err != nil {
		return err
	}
	if result != "saved" {
		return fmt.Errorf("opnsense: setHostOverride unexpected result: %s", result)
	}

	p.log.V(1).Info("record updated", "uuid", uuid)
	return p.reconfigure(ctx)
}

// Delete removes a DNS host override. Deleting a missing or already deleted
// override is not an error (idempotent).
func (p *Provider) Delete(ctx context.Context, hostname, recordType string) error {
	p.log.V(1).Info("deleting record", "hostname", hostname, "type", recordType)

	uuid, _, err := p.findOverride(ctx, hostname, recordType)
	if err != nil {
		return err
	}
	if uuid == "" {
		p.log.V(1).Info("no existing override found for deletion", "hostname", hostname, "type", recordType)
		return nil
	}

	result, _, err := p.callResult(ctx, http.MethodPost, fmt.Sprintf("unbound/settings/delHostOverride/%s", uuid), struct{}{})
	if err != nil {
		return err
	}
	switch result {
	case "deleted":
		p.log.V(1).Info("record deleted", "uuid", uuid)
	case "not found":
		p.log.V(1).Info("record already deleted", "uuid", uuid)
	default:
		return fmt.Errorf("opnsense: delHostOverride unexpected result: %s", result)
	}

	return p.reconfigure(ctx)
}

// Upsert creates or updates a DNS record depending on whether it already
// exists. A disabled override is treated as absent and re-enabled.
func (p *Provider) Upsert(ctx context.Context, record dns.Record) error {
	uuid, _, err := p.findOverride(ctx, record.Hostname, record.Type)
	if err != nil {
		return fmt.Errorf("opnsense: upsert check: %w", err)
	}
	if uuid == "" {
		return p.addOverride(ctx, record)
	}
	return p.Update(ctx, record)
}
