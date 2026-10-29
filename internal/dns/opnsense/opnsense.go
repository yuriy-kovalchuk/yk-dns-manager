// Package opnsense manages DNS records as OPNsense Unbound host overrides.
//
// OPNsense keeps edits to host overrides in a staged config; they only reach
// Unbound when "reconfigure" publishes them. Every override the controller
// writes carries ManagedDescription followed by the owners that asked for it,
// and only such overrides are ever changed or deleted.
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
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

const (
	// ManagedDescription marks the overrides the controller manages. Route
	// editors cannot write it, so it decides what may be changed.
	ManagedDescription = "yk-dns-manager: managed record, do not edit"
	// ownersPrefix starts the list of owners inside the description: the routes
	// that asked for the record, which is what authorizes a later delete.
	ownersPrefix = " [owners:"
	ownersSuffix = "]"
	// LegacyDescription is what versions before ownership marking wrote. Such
	// an override is only treated as managed when the caller adopts it.
	LegacyDescription = "managed by yk-dns-manager"

	searchPath      = "unbound/settings/searchHostOverride"
	addPath         = "unbound/settings/addHostOverride"
	setPath         = "unbound/settings/setHostOverride/"
	delPath         = "unbound/settings/delHostOverride/"
	reconfigurePath = "unbound/service/reconfigure"

	// Response size limits, so a misbehaving appliance cannot exhaust the
	// pod's memory. 8MiB covers tens of thousands of override rows.
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

	// staged counts writes made by any batch; published is the value staged
	// had when the last successful reconfigure started. A publish is owed
	// while published < staged. A new provider starts owing one, since a
	// previous process may have died with changes staged.
	mu        sync.Mutex
	staged    uint64
	published uint64
	// publishInterval is the shortest gap between two reconfigures: publishing
	// restarts Unbound, so a burst of route changes must not cost one restart
	// per change. Zero publishes every batch that owes one. lastPublish, under
	// the same mutex, is when a reconfigure last succeeded.
	publishInterval time.Duration
	lastPublish     time.Time
}

// New creates an OPNsense provider from its settings.
func New(log logr.Logger, settings map[string]string) (*Provider, error) {
	for _, key := range []string{"base_url", "api_key", "api_secret"} {
		if settings[key] == "" {
			return nil, fmt.Errorf("opnsense: missing required setting '%s'", key)
		}
	}

	// The two opt-outs of transport security are real booleans: "True" or
	// "1" work, and a value such as "yes" fails rather than quietly meaning
	// "off". Empty means off.
	flags := map[string]bool{}
	for _, key := range []string{"allow_insecure_http", "skip_tls_verify"} {
		if settings[key] == "" {
			continue
		}
		value, err := strconv.ParseBool(settings[key])
		if err != nil {
			return nil, fmt.Errorf("opnsense: %s must be true or false, got %q", key, settings[key])
		}
		flags[key] = value
	}

	// base_url must point at the API, and use https: the API key and secret
	// go out as basic auth on every request. Plain http is allowed only with
	// an explicit opt-in, meant for the local test appliance.
	baseURL, err := url.Parse(settings["base_url"])
	if err != nil {
		return nil, fmt.Errorf("opnsense: invalid base_url %q: %w", settings["base_url"], err)
	}
	if !strings.HasPrefix(baseURL.Path, "/api") {
		return nil, fmt.Errorf("opnsense: base_url path must start with '/api' (e.g. https://opnsense.example.com/api), got %q", baseURL.Path)
	}
	switch {
	case baseURL.Scheme == "https":
	case baseURL.Scheme == "http" && flags["allow_insecure_http"]:
		log.Info("opnsense: allow_insecure_http is ENABLED - the API credentials are sent in cleartext. Use it only for a local test appliance.")
	case baseURL.Scheme == "http":
		return nil, fmt.Errorf("opnsense: base_url %q uses http, which sends the API credentials in cleartext; use https, or set allow_insecure_http: \"true\" for a local test appliance", baseURL.Redacted())
	default:
		return nil, fmt.Errorf("opnsense: base_url %q must use https", baseURL.Redacted())
	}

	if v := settings["default_ttl"]; v != "" {
		log.Info("opnsense: default_ttl is ignored and will be removed: Unbound host overrides have no per-record TTL", "default_ttl", v)
	}

	// reconfigure restarts Unbound, which takes seconds and costs resolution in
	// the middle of a burst of changes. publish_interval is the shortest gap
	// between two of them; empty or 0 publishes on every batch that owes one.
	publishInterval := time.Duration(0)
	if v := settings["publish_interval"]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("opnsense: publish_interval must be a duration such as \"5s\", got %q", v)
		}
		if d < 0 {
			return nil, fmt.Errorf("opnsense: publish_interval must not be negative, got %q", v)
		}
		publishInterval = d
	}

	transport, err := newTransport(log, settings, flags["skip_tls_verify"])
	if err != nil {
		return nil, err
	}

	return &Provider{
		baseURL:   strings.TrimRight(settings["base_url"], "/") + "/",
		apiKey:    settings["api_key"],
		apiSecret: settings["api_secret"],
		client:    &http.Client{Timeout: 30 * time.Second, Transport: transport},
		log:       log,
		staged:    1,

		publishInterval: publishInterval,
	}, nil
}

// newTransport is the HTTP transport for the appliance API. ca_file pins the
// CA of a self-signed appliance, the safe option; skip_tls_verify turns
// verification off and hands the credentials to anyone on the network path, so
// it warns on every start.
func newTransport(log logr.Logger, settings map[string]string, skipVerify bool) (*http.Transport, error) {
	caFile := settings["ca_file"]
	if caFile != "" && skipVerify {
		return nil, fmt.Errorf("opnsense: ca_file and skip_tls_verify are mutually exclusive; pin the CA with ca_file instead of disabling verification")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" || skipVerify {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile) // #nosec G304 -- path comes from operator-set provider config
		if err != nil {
			return nil, fmt.Errorf("opnsense: reading ca_file %q: %w", caFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("opnsense: ca_file %q contains no valid PEM certificate", caFile)
		}
		transport.TLSClientConfig.RootCAs = pool
		log.V(1).Info("opnsense: pinning appliance CA", "ca_file", caFile)
	}
	if skipVerify {
		log.Info("opnsense: skip_tls_verify is ENABLED - TLS certificates are not verified, so the API credentials are sent to anyone who answers on the network path. Set ca_file to pin the appliance CA instead.")
		transport.TLSClientConfig.InsecureSkipVerify = true // #nosec G402 -- deliberate operator opt-in, warned on every start
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

// batch is one reconcile's view of the appliance: the override list, read
// once on first use and kept in step with the batch's own writes.
type batch struct {
	p      *Provider
	used   bool // any call made; Commit only publishes for a used batch
	loaded bool
	rows   []hostRow
}

// Ensure makes record.Hostname resolve to record.Value. See dns.Batch.
//
// An enabled override without our mark is someone else's record: Ensure
// reports a conflict and touches nothing. OPNsense accepts several
// overrides for one name, and a stale duplicate keeps answering, so whenever
// Ensure writes it first deletes our duplicates.
func (b *batch) Ensure(ctx context.Context, record dns.Record, opts dns.EnsureOptions) (dns.Result, error) {
	want := rowFor(record)
	matching, err := b.find(ctx, want)
	if err != nil {
		return 0, err
	}

	var ours []hostRow
	for _, r := range matching {
		if r.ours(opts.AdoptLegacy) {
			ours = append(ours, r)
		} else if r.Enabled == "1" {
			return dns.Conflict, nil
		}
	}

	if len(ours) == 0 {
		res, err := b.p.write(ctx, addPath, want, "saved")
		if err != nil {
			return 0, err
		}
		want.UUID = res.UUID
		b.rows = append(b.rows, want)
		return dns.Created, nil
	}

	// Keep the first enabled override, or the first one if all are disabled.
	keep := ours[0]
	for _, r := range ours {
		if r.Enabled == "1" {
			keep = r
			break
		}
	}

	// The owner list is the controller's own bookkeeping, not part of the
	// record's value: a route claiming a record another route wrote joins it,
	// and every write keeps the owners already there. Joining only when the row
	// otherwise matches, or not at all in create-only mode, would drop the other
	// route's owner and leave the record nobody can delete.
	needJoin := false
	if owners := ownersOf(keep.Description); record.Owner != "" && !slices.Contains(owners, record.Owner) {
		want.Description = markWith(append(owners, record.Owner)...)
		needJoin = true
	}

	// Create-only mode leaves a live override with another value alone, except
	// that its owner list still grows. A disabled one answers nothing, so it is
	// re-enabled instead.
	if !opts.Overwrite && keep.Enabled == "1" && keep.Server != want.Server {
		if !needJoin {
			return dns.Kept, nil
		}
		joined := keep
		joined.Description = want.Description
		if _, err := b.p.write(ctx, setPath+url.PathEscape(keep.UUID), joined, "saved"); err != nil {
			return dns.Kept, err
		}
		b.replaceRow(joined)
		return dns.Kept, nil
	}

	for _, r := range ours {
		if r.UUID != keep.UUID {
			if err := b.deleteRow(ctx, r.UUID); err != nil {
				return 0, err
			}
		}
	}
	if len(ours) > 1 {
		b.p.log.Info("removed duplicate host overrides", "hostname", record.Hostname, "removed", len(ours)-1)
	}

	upToDate := keep.Enabled == "1" && keep.Server == want.Server && keep.Description == want.Description
	switch {
	case upToDate && len(ours) > 1:
		return dns.Updated, nil // only duplicates were removed
	case upToDate:
		return dns.Unchanged, nil
	}
	if _, err := b.p.write(ctx, setPath+url.PathEscape(keep.UUID), want, "saved"); err != nil {
		return 0, err
	}
	want.UUID = keep.UUID
	b.replaceRow(want)
	return dns.Updated, nil
}

// Remove deletes our overrides for hostname, and legacy ones when adopting.
// Anyone else's are left alone.
func (b *batch) Remove(ctx context.Context, hostname, recordType, owner string, adoptLegacy bool) error {
	matching, err := b.find(ctx, rowFor(dns.Record{Hostname: hostname, Type: recordType}))
	if err != nil {
		return err
	}
	for _, r := range matching {
		if !r.ours(adoptLegacy) {
			continue
		}
		// The owner list says who may delete the record. The caller has no claim
		// on one written by other routes alone: the annotation that named the
		// hostname is writable by anyone who can edit the route. The controller
		// only gets here when no live route claims the hostname any more, so the
		// other owners have stopped claiming it and the record goes with the
		// caller's claim.
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

// Commit publishes with one reconfigure, if any write is still unpublished,
// including writes of earlier batches whose publish failed.
func (b *batch) Commit(ctx context.Context) error {
	p := b.p
	if !b.used {
		return nil
	}
	p.mu.Lock()
	target := p.staged
	owed := p.published < target
	wait := p.publishInterval - time.Since(p.lastPublish)
	p.mu.Unlock()
	if !owed {
		return nil
	}
	if wait > 0 {
		// The writes stay owed, so the next Commit after the wait publishes
		// everything a burst of reconciles staged, instead of each of them
		// restarting Unbound.
		return &dns.PublishDeferred{After: wait}
	}

	var res apiResult
	if err := p.call(ctx, http.MethodPost, reconfigurePath, struct{}{}, &res); err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(res.Status), "ok") {
		return fmt.Errorf("opnsense: reconfigure returned status %q", res.Status)
	}
	p.log.V(1).Info("reconfigure completed")

	// A write made while this reconfigure ran is above target, so it stays
	// owed.
	p.mu.Lock()
	p.published = max(p.published, target)
	p.lastPublish = time.Now()
	p.mu.Unlock()
	return nil
}

// find returns the overrides with want's hostname, domain and record type,
// reading the override list on the batch's first call.
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

// deleteRow deletes one override. "not found" means someone else deleted it
// already, which is the goal.
func (b *batch) deleteRow(ctx context.Context, uuid string) error {
	if _, err := b.p.write(ctx, delPath+url.PathEscape(uuid), nil, "deleted", "not found"); err != nil {
		return err
	}
	b.rows = slices.DeleteFunc(b.rows, func(r hostRow) bool { return r.UUID == uuid })
	return nil
}

// replaceRow swaps the batch's stored copy of an override it just rewrote,
// so later calls in the same batch see what it wrote.
func (b *batch) replaceRow(row hostRow) {
	for i := range b.rows {
		if b.rows[i].UUID == row.UUID {
			b.rows[i] = row
		}
	}
}

// hostRow is one host override: a row of the search response, and the body
// of an add or set.
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

// rowFor is the managed override for record. OPNsense splits the name:
// "sub.app.example.com" is hostname "sub" in domain "app.example.com".
func rowFor(record dns.Record) hostRow {
	hostname, domain, _ := strings.Cut(strings.TrimSuffix(record.Hostname, "."), ".")
	return hostRow{
		Enabled: "1", Hostname: hostname, Domain: domain, RR: record.Type,
		Server: record.Value, Description: markWith(record.Owner),
	}
}

// markWith is the description of a record the given owners asked for.
func markWith(owners ...string) string {
	seen := make(map[string]bool, len(owners))
	unique := make([]string, 0, len(owners))
	for _, o := range owners {
		if o == "" || seen[o] {
			continue
		}
		seen[o] = true
		unique = append(unique, o)
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

// ownersOf returns the owners in a description. It is empty for a record
// written before owners were marked, which leaves it deletable by any route
// until an Ensure records who asked for it.
func ownersOf(description string) []string {
	rest, ok := strings.CutPrefix(description, ManagedDescription+ownersPrefix)
	if !ok {
		return nil
	}
	list, _ := strings.CutSuffix(rest, ownersSuffix)
	return strings.Fields(list)
}

// ours reports whether the controller may change or delete the override.
func (r hostRow) ours(adoptLegacy bool) bool {
	return isManaged(r.Description) || (adoptLegacy && r.Description == LegacyDescription)
}

// apiResult is the answer to a write: "result" for add, set and del, plus
// "uuid" for add, "status" for reconfigure, and "validations" when the
// appliance rejected the row.
type apiResult struct {
	Result string `json:"result"`
	UUID   string `json:"uuid"`
	Status string `json:"status"`
	// Validations maps the rejected field to the appliance's complaint, e.g.
	// "host.domain": "A domain is required.". Without it a rejection says only
	// "failed", which is useless for finding out why a record was refused.
	Validations map[string]string `json:"validations"`
}

// joinValidations renders the appliance's per-field complaints, sorted so the
// message stays the same across retries.
func joinValidations(validations map[string]string) string {
	fields := make([]string, 0, len(validations))
	for field, complaint := range validations {
		fields = append(fields, field+": "+complaint)
	}
	slices.Sort(fields)
	return strings.Join(fields, "; ")
}

// write POSTs a change to the staged config and counts it as owing a
// publish. OPNsense rejects a write with HTTP 200 and a result other than
// the accepted ones, so the status code alone proves nothing.
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
	p.mu.Lock()
	p.staged++
	p.mu.Unlock()
	return res, nil
}

// call sends one API request and decodes the JSON answer into out, which may
// be nil. A status other than 200 is an error carrying a short snippet of the
// body.
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
