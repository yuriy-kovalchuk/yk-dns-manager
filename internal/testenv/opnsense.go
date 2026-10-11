// Package testenv provides in-memory stand-ins for the backends this controller
// talks to. The same fake backs the integration tests and the local development
// server, so what tests exercise is what a developer sees when running locally.
package testenv

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
)

// HostOverride mirrors an OPNsense Unbound host override row. Every field is a
// string because that is what the appliance API sends and accepts.
type HostOverride struct {
	Enabled     string `json:"enabled"`
	Hostname    string `json:"hostname"`
	Domain      string `json:"domain"`
	RR          string `json:"rr"`
	Server      string `json:"server"`
	Description string `json:"description"`
	MXPrio      string `json:"mxprio"`
	MX          string `json:"mx"`
}

// FQDN is the hostname the override resolves, matching how the controller reads
// it back from the appliance.
func (h HostOverride) FQDN() string {
	return strings.TrimSuffix(h.Hostname+"."+h.Domain, ".")
}

// Endpoint names accepted by SetFailure.
const (
	EndpointSearch      = "searchHostOverride"
	EndpointAdd         = "addHostOverride"
	EndpointSet         = "setHostOverride"
	EndpointDel         = "delHostOverride"
	EndpointReconfigure = "reconfigure"
)

const (
	searchPath      = "/api/unbound/settings/searchHostOverride"
	addPath         = "/api/unbound/settings/addHostOverride"
	setPathPrefix   = "/api/unbound/settings/setHostOverride/"
	delPathPrefix   = "/api/unbound/settings/delHostOverride/"
	reconfigurePath = "/api/unbound/service/reconfigure"
)

// OPNsense is an in-memory implementation of the subset of the OPNsense Unbound
// REST API this controller uses: searchHostOverride, addHostOverride,
// setHostOverride, delHostOverride and reconfigure.
//
// It follows the appliance where the provider's behaviour depends on it:
//
//   - Edits land in the staged config and only become what Unbound answers
//     with on reconfigure. Snapshot is the staged config, Published is what
//     was last applied.
//   - A uuid that does not exist is answered with HTTP 200 and a result body
//     ("failed" for set, "not found" for del), not with a 404.
//   - add and set validate the row and answer {"result":"failed","validations":…}
//     for one the appliance would reject.
//   - Write endpoints only accept POST.
//   - With RequireCredentials, a request without the right basic auth is
//     answered with 401.
//
// By default it accepts any credentials, or none, so the local development
// server works without configuration.
type OPNsense struct {
	mu        sync.Mutex
	store     map[string]HostOverride
	published map[string]HostOverride
	nextID    int
	calls     []string
	failures  map[string]int

	apiKey, apiSecret string
	duringReconfigure func()
}

// NewOPNsense returns an empty fake with no overrides.
func NewOPNsense() *OPNsense {
	return &OPNsense{
		store:     map[string]HostOverride{},
		published: map[string]HostOverride{},
		failures:  map[string]int{},
	}
}

// RequireCredentials makes the fake reject any request that does not carry
// exactly this key and secret as basic auth.
func (f *OPNsense) RequireCredentials(key, secret string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.apiKey, f.apiSecret = key, secret
}

// SetFailure makes every request to endpoint (one of the Endpoint constants)
// answer with the given HTTP status until it is cleared with a status of 0.
// The request is still recorded by Calls but has no effect on the store.
func (f *OPNsense) SetFailure(endpoint string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if status == 0 {
		delete(f.failures, endpoint)
		return
	}
	f.failures[endpoint] = status
}

// DuringReconfigure registers fn to run inside every reconfigure, after the
// staged config has been taken for publishing and before the response is
// sent. A write fn makes is therefore not published by that reconfigure,
// which is how tests make a write race with a publish in flight.
func (f *OPNsense) DuringReconfigure(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.duringReconfigure = fn
}

// ServeHTTP routes the API paths the provider calls.
func (f *OPNsense) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	wantKey, wantSecret := f.apiKey, f.apiSecret
	f.mu.Unlock()

	if wantKey != "" {
		key, secret, ok := r.BasicAuth()
		if !ok || key != wantKey || secret != wantSecret {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": 401, "message": "Authentication Failed"})
			return
		}
	}

	var endpoint string
	var handler func(http.ResponseWriter, *http.Request)
	switch {
	case r.URL.Path == searchPath:
		endpoint, handler = EndpointSearch, f.handleSearch
	case r.URL.Path == addPath:
		endpoint, handler = EndpointAdd, f.handleAdd
	case strings.HasPrefix(r.URL.Path, setPathPrefix):
		endpoint, handler = EndpointSet, f.handleSet
	case strings.HasPrefix(r.URL.Path, delPathPrefix):
		endpoint, handler = EndpointDel, f.handleDel
	case r.URL.Path == reconfigurePath:
		endpoint, handler = EndpointReconfigure, f.handleReconfigure
	default:
		http.NotFound(w, r)
		return
	}

	f.mu.Lock()
	status := f.failures[endpoint]
	f.mu.Unlock()
	if status != 0 {
		http.Error(w, fmt.Sprintf(`{"message":"injected failure on %s"}`, endpoint), status)
		return
	}

	if endpoint != EndpointSearch && r.Method != http.MethodPost {
		writeJSON(w, map[string]string{"result": "failed"})
		return
	}
	handler(w, r)
}

// Snapshot returns a copy of the staged overrides keyed by uuid: everything
// written so far, whether or not a reconfigure has published it.
func (f *OPNsense) Snapshot() map[string]HostOverride {
	f.mu.Lock()
	defer f.mu.Unlock()
	return copyStore(f.store)
}

// Published returns a copy of the overrides as of the last successful
// reconfigure, which is what Unbound would actually answer with.
func (f *OPNsense) Published() map[string]HostOverride {
	f.mu.Lock()
	defer f.mu.Unlock()
	return copyStore(f.published)
}

// Calls returns the request log in order as "METHOD /path".
func (f *OPNsense) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// ResetCalls clears the request log.
func (f *OPNsense) ResetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// Seed adds an override without going through the API and returns its uuid.
// A seeded row is treated as already published, like configuration that was on
// the appliance before the controller started.
func (f *OPNsense) Seed(h HostOverride) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("uuid-%d", f.nextID)
	f.store[id] = h
	f.published[id] = h
	return id
}

func (f *OPNsense) handleSearch(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	type row struct {
		UUID string `json:"uuid"`
		HostOverride
	}
	rows := []row{}
	for id, h := range f.store {
		rows = append(rows, row{UUID: id, HostOverride: h})
	}
	writeJSON(w, map[string]interface{}{"rows": rows, "rowCount": len(rows), "total": len(rows), "current": 1})
}

func (f *OPNsense) handleAdd(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Host HostOverride `json:"host"`
	}
	if err := readJSON(r, &payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if v := validate(payload.Host); len(v) > 0 {
		writeJSON(w, map[string]interface{}{"result": "failed", "validations": v})
		return
	}

	f.mu.Lock()
	f.nextID++
	id := fmt.Sprintf("uuid-%d", f.nextID)
	f.store[id] = payload.Host
	f.mu.Unlock()

	writeJSON(w, map[string]string{"result": "saved", "uuid": id})
}

func (f *OPNsense) handleSet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, setPathPrefix)
	var payload struct {
		Host HostOverride `json:"host"`
	}
	if err := readJSON(r, &payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.store[id]; !ok {
		writeJSON(w, map[string]string{"result": "failed"})
		return
	}
	if v := validate(payload.Host); len(v) > 0 {
		writeJSON(w, map[string]interface{}{"result": "failed", "validations": v})
		return
	}
	f.store[id] = payload.Host
	writeJSON(w, map[string]string{"result": "saved"})
}

func (f *OPNsense) handleDel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, delPathPrefix)

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.store[id]; !ok {
		writeJSON(w, map[string]string{"result": "not found"})
		return
	}
	delete(f.store, id)
	writeJSON(w, map[string]string{"result": "deleted"})
}

func (f *OPNsense) handleReconfigure(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	f.published = copyStore(f.store)
	hook := f.duringReconfigure
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// validate applies the subset of the appliance's model constraints the
// controller can run into. The keys match the "host.<field>" form OPNsense
// uses in its validations map.
func validate(h HostOverride) map[string]string {
	v := map[string]string{}
	if h.Enabled != "0" && h.Enabled != "1" {
		v["host.enabled"] = "Option not in list."
	}
	if h.Domain == "" {
		v["host.domain"] = "A domain is required."
	}
	switch h.RR {
	case "A", "AAAA":
		addr, err := netip.ParseAddr(h.Server)
		if err != nil || addr.Is4() != (h.RR == "A") {
			v["host.server"] = "A valid IP address must be specified."
		}
	case "MX":
		if h.MX == "" {
			v["host.mx"] = "A mail exchange is required."
		}
	default:
		v["host.rr"] = "Option not in list."
	}
	return v
}

func copyStore(in map[string]HostOverride) map[string]HostOverride {
	out := make(map[string]HostOverride, len(in))
	for id, h := range in {
		out[id] = h
	}
	return out
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v interface{}) error {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
