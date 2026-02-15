package opnsense

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-logr/logr"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

func init() {
	dns.Register("opnsense", func(log logr.Logger, settings map[string]string) (dns.Provider, error) {
		return New(log, settings)
	})
}

// Provider implements dns.Provider for OPNsense Unbound DNS.
type Provider struct {
	baseURL    string
	apiKey     string
	apiSecret  string
	defaultTTL int
	client     *http.Client
	log        logr.Logger
}

// New creates an OPNsense DNS provider from the given settings map.
// Required settings: base_url, api_key, api_secret.
// Optional settings: default_ttl (default 300), skip_tls_verify (default false).
func New(log logr.Logger, settings map[string]string) (*Provider, error) {
	baseURL := settings["base_url"]
	if baseURL == "" {
		return nil, fmt.Errorf("opnsense: missing required setting 'base_url'")
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
	if v := settings["skip_tls_verify"]; v == "true" {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	return &Provider{
		baseURL:    baseURL,
		apiKey:     apiKey,
		apiSecret:  apiSecret,
		defaultTTL: defaultTTL,
		client:     &http.Client{Transport: transport},
		log:        log,
	}, nil
}

// doRequest builds and executes an HTTP request against the OPNsense API.
func (p *Provider) doRequest(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
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

// reconfigure tells OPNsense to apply DNS changes.
func (p *Provider) reconfigure(ctx context.Context) error {
	resp, err := p.doRequest(ctx, http.MethodPost, "unbound/service/reconfigure", struct{}{})
	if err != nil {
		return fmt.Errorf("opnsense: reconfigure: %w", err)
	}
	defer resp.Body.Close()

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

// findOverride searches for an existing host override matching hostname and record type.
// Returns the UUID if found, or empty string if not.
func (p *Provider) findOverride(ctx context.Context, fqdn, recordType string) (string, error) {
	resp, err := p.doRequest(ctx, http.MethodGet, "unbound/settings/searchHostOverride", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("opnsense: searchHostOverride returned status %d", resp.StatusCode)
	}

	var sr searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return "", fmt.Errorf("opnsense: decode search response: %w", err)
	}

	host, domain := dns.SplitHostname(fqdn)
	for _, row := range sr.Rows {
		if strings.EqualFold(row.Hostname, host) &&
			strings.EqualFold(row.Domain, domain) &&
			strings.EqualFold(row.RR, recordType) {
			return row.UUID, nil
		}
	}
	return "", nil
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
	p.log.Info("checking if record exists", "hostname", hostname, "type", recordType)
	uuid, err := p.findOverride(ctx, hostname, recordType)
	if err != nil {
		return false, err
	}
	return uuid != "", nil
}

// Create adds a new DNS host override.
func (p *Provider) Create(ctx context.Context, record dns.Record) error {
	p.log.Info("creating record", "hostname", record.Hostname, "type", record.Type, "value", record.Value)

	body := buildHostBody(record)
	resp, err := p.doRequest(ctx, http.MethodPost, "unbound/settings/addHostOverride", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("opnsense: addHostOverride returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Result string `json:"result"`
		UUID   string `json:"uuid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("opnsense: decode addHostOverride response: %w", err)
	}
	if result.Result != "saved" {
		return fmt.Errorf("opnsense: addHostOverride unexpected result: %s", result.Result)
	}

	p.log.Info("record created", "uuid", result.UUID)
	return p.reconfigure(ctx)
}

// Update modifies an existing DNS host override.
func (p *Provider) Update(ctx context.Context, record dns.Record) error {
	p.log.Info("updating record", "hostname", record.Hostname, "type", record.Type, "value", record.Value)

	uuid, err := p.findOverride(ctx, record.Hostname, record.Type)
	if err != nil {
		return err
	}
	if uuid == "" {
		return fmt.Errorf("opnsense: no existing override found for %s/%s", record.Hostname, record.Type)
	}

	body := buildHostBody(record)
	resp, err := p.doRequest(ctx, http.MethodPost, fmt.Sprintf("unbound/settings/setHostOverride/%s", uuid), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("opnsense: setHostOverride returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Result string `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("opnsense: decode setHostOverride response: %w", err)
	}
	if result.Result != "saved" {
		return fmt.Errorf("opnsense: setHostOverride unexpected result: %s", result.Result)
	}

	p.log.Info("record updated", "uuid", uuid)
	return p.reconfigure(ctx)
}

// Delete removes a DNS host override.
func (p *Provider) Delete(ctx context.Context, hostname, recordType string) error {
	p.log.Info("deleting record", "hostname", hostname, "type", recordType)

	uuid, err := p.findOverride(ctx, hostname, recordType)
	if err != nil {
		return err
	}
	if uuid == "" {
		return fmt.Errorf("opnsense: no existing override found for %s/%s", hostname, recordType)
	}

	resp, err := p.doRequest(ctx, http.MethodPost, fmt.Sprintf("unbound/settings/delHostOverride/%s", uuid), struct{}{})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("opnsense: delHostOverride returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Result string `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("opnsense: decode delHostOverride response: %w", err)
	}
	if result.Result != "deleted" {
		return fmt.Errorf("opnsense: delHostOverride unexpected result: %s", result.Result)
	}

	p.log.Info("record deleted", "uuid", uuid)
	return p.reconfigure(ctx)
}

// Upsert creates or updates a DNS record depending on whether it already exists.
func (p *Provider) Upsert(ctx context.Context, record dns.Record) error {
	exists, err := p.Exists(ctx, record.Hostname, record.Type)
	if err != nil {
		return fmt.Errorf("opnsense: upsert check: %w", err)
	}
	if exists {
		return p.Update(ctx, record)
	}
	return p.Create(ctx, record)
}
