// Package opnsense manages DNS records as OPNsense Unbound host overrides, which only reach Unbound once "reconfigure" publishes them.
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
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

const (
	// ManagedDescription marks the overrides the controller manages; route editors cannot write it.
	ManagedDescription = "yk-dns-manager: managed record, do not edit"
	// ownersPrefix starts the list of owning routes inside the description: what authorizes a later delete.
	ownersPrefix = " [owners:"
	ownersSuffix = "]"
	// LegacyDescription is what versions before ownership marking wrote; it counts as managed only when adopted.
	LegacyDescription = "managed by yk-dns-manager"

	searchPath      = "unbound/settings/searchHostOverride"
	addPath         = "unbound/settings/addHostOverride"
	setPath         = "unbound/settings/setHostOverride/"
	delPath         = "unbound/settings/delHostOverride/"
	reconfigurePath = "unbound/service/reconfigure"

	// Response size caps so a misbehaving appliance cannot exhaust the pod; 8MiB covers tens of thousands of rows.
	maxResponseBytes  = 8 << 20
	maxErrorBodyBytes = 1 << 10 // error bodies are only logged
)

// Provider implements dns.Provider for OPNsense Unbound DNS.
type Provider struct {
	baseURL   string // ends in "/", so API paths append directly
	apiKey    string
	apiSecret string
	client    *http.Client
	log       logr.Logger
	// publisher spaces reconfigures publish_interval apart: each one restarts Unbound, so a burst of changes must not cost one restart each.
	publisher *dns.Publisher
}

// New creates an OPNsense provider from its settings.
func New(log logr.Logger, settings map[string]string) (*Provider, error) {
	for _, key := range []string{"base_url", "api_key", "api_secret"} {
		if settings[key] == "" {
			return nil, fmt.Errorf("opnsense: missing required setting '%s'", key)
		}
	}
	allowHTTP, err := boolSetting(settings, "allow_insecure_http")
	if err != nil {
		return nil, err
	}
	skipVerify, err := boolSetting(settings, "skip_tls_verify")
	if err != nil {
		return nil, err
	}
	if err := checkBaseURL(log, settings["base_url"], allowHTTP); err != nil {
		return nil, err
	}
	if v := settings["default_ttl"]; v != "" {
		log.Info("opnsense: default_ttl is ignored and will be removed: Unbound host overrides have no per-record TTL", "default_ttl", v)
	}
	publishInterval, err := publishIntervalSetting(settings["publish_interval"])
	if err != nil {
		return nil, err
	}
	transport, err := newTransport(log, settings["ca_file"], skipVerify)
	if err != nil {
		return nil, err
	}

	return &Provider{
		baseURL:   strings.TrimRight(settings["base_url"], "/") + "/",
		apiKey:    settings["api_key"],
		apiSecret: settings["api_secret"],
		client:    &http.Client{Timeout: 30 * time.Second, Transport: transport},
		log:       log,
		publisher: dns.NewPublisher(publishInterval),
	}, nil
}

// boolSetting parses a transport-security opt-out: "yes" fails rather than quietly meaning "off", empty means off.
func boolSetting(settings map[string]string, key string) (bool, error) {
	if settings[key] == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(settings[key])
	if err != nil {
		return false, fmt.Errorf("opnsense: %s must be true or false, got %q", key, settings[key])
	}
	return value, nil
}

// checkBaseURL requires https: the key and secret go out as basic auth on every request, so http needs an explicit opt-in.
func checkBaseURL(log logr.Logger, raw string, allowHTTP bool) error {
	baseURL, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("opnsense: invalid base_url %q: %w", raw, err)
	}
	if !strings.HasPrefix(baseURL.Path, "/api") {
		return fmt.Errorf("opnsense: base_url path must start with '/api' (e.g. https://opnsense.example.com/api), got %q", baseURL.Path)
	}
	switch {
	case baseURL.Scheme == "https":
		return nil
	case baseURL.Scheme == "http" && allowHTTP:
		log.Info("opnsense: allow_insecure_http is ENABLED - the API credentials are sent in cleartext. Use it only for a local test appliance.")
		return nil
	case baseURL.Scheme == "http":
		return fmt.Errorf("opnsense: base_url %q uses http, which sends the API credentials in cleartext; use https, or set allow_insecure_http: \"true\" for a local test appliance", baseURL.Redacted())
	default:
		return fmt.Errorf("opnsense: base_url %q must use https", baseURL.Redacted())
	}
}

// publishIntervalSetting is the shortest gap between two reconfigures; empty or 0 publishes on every batch that owes one.
func publishIntervalSetting(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("opnsense: publish_interval must be a duration such as \"5s\", got %q", value)
	}
	if d < 0 {
		return 0, fmt.Errorf("opnsense: publish_interval must not be negative, got %q", value)
	}
	return d, nil
}

// newTransport pins a self-signed appliance's CA with caFile; skipVerify turns verification off and warns on every start.
func newTransport(log logr.Logger, caFile string, skipVerify bool) (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	switch {
	case caFile != "" && skipVerify:
		return nil, fmt.Errorf("opnsense: ca_file and skip_tls_verify are mutually exclusive; pin the CA with ca_file instead of disabling verification")
	case caFile != "":
		pem, err := os.ReadFile(caFile) // #nosec G304 -- path comes from operator-set provider config
		if err != nil {
			return nil, fmt.Errorf("opnsense: reading ca_file %q: %w", caFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("opnsense: ca_file %q contains no valid PEM certificate", caFile)
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
		log.V(1).Info("opnsense: pinning appliance CA", "ca_file", caFile)
	case skipVerify:
		log.Info("opnsense: skip_tls_verify is ENABLED - TLS certificates are not verified, so the API credentials are sent to anyone who answers on the network path. Set ca_file to pin the appliance CA instead.")
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true} // #nosec G402 -- deliberate operator opt-in, warned on every start
	}
	return transport, nil
}

// HealthCheck verifies the API is reachable and accepts the credentials.
func (p *Provider) HealthCheck(ctx context.Context) error {
	return p.call(ctx, http.MethodGet, searchPath, nil, nil)
}

// Batch starts one reconcile's unit of work.
func (p *Provider) Batch() dns.Batch {
	return &batch{p: p}
}

// batch is one reconcile's view of the appliance: the override list, read once and kept in step with its own writes.
type batch struct {
	p      *Provider
	used   bool // any call made; Commit only publishes for a used batch
	loaded bool
	rows   []hostRow
}

// Ensure writes the override for record, clearing our duplicates of that name first; an unmarked override is someone else's and is a conflict. See dns.Batch.
func (b *batch) Ensure(ctx context.Context, record dns.Record, opts dns.EnsureOptions) (dns.Result, error) {
	want := rowFor(record)
	matching, err := b.find(ctx, want)
	if err != nil {
		return 0, err
	}
	var ours []hostRow
	for _, r := range matching {
		switch {
		case r.ours(opts.AdoptLegacy):
			ours = append(ours, r)
		case r.Enabled == "1":
			return dns.Conflict, nil
		}
	}
	if len(ours) == 0 {
		if err := b.save(ctx, want); err != nil {
			return 0, err
		}
		return dns.Created, nil
	}

	// Keep the first enabled override, or the first one if all are disabled.
	keep := ours[0]
	if i := slices.IndexFunc(ours, isEnabled); i >= 0 {
		keep = ours[i]
	}
	// The owner list is bookkeeping, not the record's value: Ensure only ever adds to it, or a re-Ensure would leave a record nobody can delete.
	owners := ownersOf(keep.Description)
	joining := record.Owner != "" && !slices.Contains(owners, record.Owner)
	want.Description = markWith(append(owners, record.Owner)...)
	want.UUID = keep.UUID

	// Create-only leaves a live override with another value alone but still grows its owner list; a disabled one is re-enabled instead.
	if !opts.Overwrite && isEnabled(keep) && keep.Server != want.Server {
		if joining {
			keep.Description = want.Description
			if err := b.save(ctx, keep); err != nil {
				return 0, err
			}
		}
		return dns.Kept, nil
	}

	for _, r := range ours {
		if r.UUID == keep.UUID {
			continue
		}
		if err := b.deleteRow(ctx, r.UUID); err != nil {
			return 0, err
		}
	}
	if len(ours) > 1 {
		b.p.log.Info("removed duplicate host overrides", "hostname", record.Hostname, "removed", len(ours)-1)
	}

	upToDate := isEnabled(keep) && keep.Server == want.Server && keep.Description == want.Description
	switch {
	case upToDate && len(ours) > 1:
		return dns.Updated, nil // only duplicates were removed
	case upToDate:
		return dns.Unchanged, nil
	}
	if err := b.save(ctx, want); err != nil {
		return 0, err
	}
	return dns.Updated, nil
}

// Remove deletes our overrides for hostname, and legacy ones when adopting.
func (b *batch) Remove(ctx context.Context, hostname, recordType, owner string, adoptLegacy bool) error {
	matching, err := b.find(ctx, rowFor(dns.Record{Hostname: hostname, Type: recordType}))
	if err != nil {
		return err
	}
	for _, r := range matching {
		if !r.ours(adoptLegacy) {
			continue
		}
		// The owner list decides who may delete: the annotation naming the hostname is writable by any route editor, and no live route claims it by the time we get here.
		if owners := ownersOf(r.Description); len(owners) > 0 && !slices.Contains(owners, owner) {
			b.p.log.Info("leaving a host override other routes own", "hostname", hostname, "owners", strings.Join(owners, " "))
			continue
		}
		if err := b.deleteRow(ctx, r.UUID); err != nil {
			return err
		}
	}
	return nil
}

// Commit publishes with one reconfigure if any write is still unpublished, including earlier batches' failed publishes.
func (b *batch) Commit(ctx context.Context) error {
	if !b.used {
		return nil
	}
	return b.p.publisher.Publish(ctx, b.p.reconfigure)
}

// reconfigure makes Unbound serve the staged overrides.
func (p *Provider) reconfigure(ctx context.Context) error {
	var res apiResult
	if err := p.call(ctx, http.MethodPost, reconfigurePath, struct{}{}, &res); err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(res.Status), "ok") {
		return fmt.Errorf("opnsense: reconfigure returned status %q", res.Status)
	}
	p.log.V(1).Info("reconfigure completed")
	return nil
}

// find returns the overrides matching want, reading the override list on the batch's first call.
func (b *batch) find(ctx context.Context, want hostRow) ([]hostRow, error) {
	b.used = true
	if !b.loaded {
		var res struct {
			Rows []hostRow `json:"rows"`
		}
		if err := b.p.call(ctx, http.MethodGet, searchPath, nil, &res); err != nil {
			return nil, err
		}
		b.rows, b.loaded = res.Rows, true
	}
	var matching []hostRow
	for _, r := range b.rows {
		if strings.EqualFold(r.Hostname, want.Hostname) && strings.EqualFold(r.Domain, want.Domain) && strings.EqualFold(r.RR, want.RR) {
			matching = append(matching, r)
		}
	}
	return matching, nil
}

// deleteRow deletes one override; "not found" means someone else already did it.
func (b *batch) deleteRow(ctx context.Context, uuid string) error {
	if _, err := b.p.write(ctx, delPath+url.PathEscape(uuid), nil, "deleted", "not found"); err != nil {
		return err
	}
	b.rows = slices.DeleteFunc(b.rows, func(r hostRow) bool { return r.UUID == uuid })
	return nil
}

// save adds row, or rewrites the override its UUID names, and keeps the batch's copy in step.
func (b *batch) save(ctx context.Context, row hostRow) error {
	if row.UUID == "" {
		res, err := b.p.write(ctx, addPath, row, "saved")
		if err != nil {
			return err
		}
		row.UUID = res.UUID
		b.rows = append(b.rows, row)
		return nil
	}
	body := row
	body.UUID = "" // the path names the override
	if _, err := b.p.write(ctx, setPath+url.PathEscape(row.UUID), body, "saved"); err != nil {
		return err
	}
	if i := slices.IndexFunc(b.rows, func(r hostRow) bool { return r.UUID == row.UUID }); i >= 0 {
		b.rows[i] = row
	}
	return nil
}

// hostRow is one host override: a row of the search response, and the body of an add or set.
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

// rowFor is the managed override for record; OPNsense splits "sub.app.example.com" into host "sub" and domain "app.example.com".
func rowFor(record dns.Record) hostRow {
	hostname, domain, _ := strings.Cut(strings.TrimSuffix(record.Hostname, "."), ".")
	return hostRow{
		Enabled: "1", Hostname: hostname, Domain: domain, RR: record.Type,
		Server: record.Value, Description: markWith(record.Owner),
	}
}

// markWith is the description of a record the given owners asked for.
func markWith(owners ...string) string {
	var unique []string
	for _, o := range owners {
		if o != "" && !slices.Contains(unique, o) {
			unique = append(unique, o)
		}
	}
	if len(unique) == 0 {
		return ManagedDescription
	}
	return ManagedDescription + ownersPrefix + " " + strings.Join(unique, " ") + ownersSuffix
}

// isManaged reports whether a description is our mark, with or without owners.
func isManaged(description string) bool {
	return description == ManagedDescription || strings.HasPrefix(description, ManagedDescription+ownersPrefix)
}

// ownersOf returns the owners in a description; empty for pre-ownership records, which stay deletable by any route.
func ownersOf(description string) []string {
	rest, ok := strings.CutPrefix(description, ManagedDescription+ownersPrefix)
	if !ok {
		return nil
	}
	list, _ := strings.CutSuffix(rest, ownersSuffix)
	return strings.Fields(list)
}

func isEnabled(r hostRow) bool { return r.Enabled == "1" }

// ours reports whether the controller may change or delete the override.
func (r hostRow) ours(adoptLegacy bool) bool {
	return isManaged(r.Description) || (adoptLegacy && r.Description == LegacyDescription)
}

// apiResult is the answer to a write: result for add/set/del, uuid for add, status for reconfigure, validations on a rejection.
type apiResult struct {
	Result string `json:"result"`
	UUID   string `json:"uuid"`
	Status string `json:"status"`
	// Validations maps the rejected field to the complaint, e.g. "host.domain": "A domain is required.".
	Validations map[string]string `json:"validations"`
}

// joinValidations renders the appliance's per-field complaints, sorted so the message stays the same across retries.
func joinValidations(validations map[string]string) string {
	fields := make([]string, 0, len(validations))
	for field, complaint := range validations {
		fields = append(fields, field+": "+complaint)
	}
	slices.Sort(fields)
	return strings.Join(fields, "; ")
}

// write POSTs a change to the staged config and counts it as owing a publish; OPNsense rejects with HTTP 200, so the status code proves nothing.
func (p *Provider) write(ctx context.Context, path string, row any, accepted ...string) (apiResult, error) {
	var body any = struct{}{}
	if row != nil {
		body = map[string]any{"host": row}
	}
	var res apiResult
	if err := p.call(ctx, http.MethodPost, path, body, &res); err != nil {
		return res, err
	}
	if !slices.Contains(accepted, res.Result) {
		if len(res.Validations) > 0 {
			return res, fmt.Errorf("opnsense: %s rejected the record (%s)", path, joinValidations(res.Validations))
		}
		return res, fmt.Errorf("opnsense: %s unexpected result: %s", path, res.Result)
	}
	p.publisher.Staged()
	return res, nil
}

// call sends one API request and decodes the JSON answer into out, which may be nil; a non-200 status carries a body snippet.
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
