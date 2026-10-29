// Package testenv provides in-memory stand-ins for the backends this controller talks to, shared by the integration tests and the local development server.
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

// HostOverride mirrors an OPNsense Unbound host override row; every field is a string because that is what the API sends and accepts.
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

// FQDN is the hostname the override resolves, matching how the controller reads it back from the appliance.
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

// OPNsense is an in-memory stand-in for the Unbound API subset this controller uses, faithful to the appliance where the provider depends on it: edits publish only on reconfigure, unknown uuids answer 200 with a result body, rows are validated, writes are POST-only, and basic auth is rejected when RequireCredentials is set.
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

// RequireCredentials makes the fake reject any request without exactly this key and secret as basic auth.
func (f *OPNsense) RequireCredentials(key, secret string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.apiKey, f.apiSecret = key, secret
}

// SetFailure makes every request to endpoint answer with the given status until cleared with 0; the call is still recorded but does not touch the store.
func (f *OPNsense) SetFailure(endpoint string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if status == 0 {
		delete(f.failures, endpoint)
		return
	}
	f.failures[endpoint] = status
}

// DuringReconfigure runs fn inside every reconfigure after staging and before the response, so a write made by fn is not published by that reconfigure.
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
			_ = json.NewEncoder(w).Encode(map[string]any{"status": 401, "message": "Authentication Failed"})
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

// Snapshot returns a copy of the staged overrides keyed by uuid, published or not.
func (f *OPNsense) Snapshot() map[string]HostOverride {
	f.mu.Lock()
	defer f.mu.Unlock()
	return copyStore(f.store)
}

// Published returns a copy of the overrides as of the last successful reconfigure, which is what Unbound would actually answer with.
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

// Seed adds an override outside the API and returns its uuid, treated as already published like config that predates the controller.
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
	writeJSON(w, map[string]any{"rows": rows, "rowCount": len(rows), "total": len(rows), "current": 1})
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
		writeJSON(w, map[string]any{"result": "failed", "validations": v})
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
		writeJSON(w, map[string]any{"result": "failed", "validations": v})
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

// validate applies the subset of the appliance's model constraints the controller can hit, keyed in OPNsense's "host.<field>" form.
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
