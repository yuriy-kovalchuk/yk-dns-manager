// Package testenv provides in-memory stand-ins for the backends this controller
// talks to. The same fake backs the integration tests and the local development
// server, so what tests exercise is what a developer sees when running locally.
package testenv

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// OPNsense is an in-memory implementation of the subset of the OPNsense Unbound
// REST API this controller uses: searchHostOverride, addHostOverride,
// setHostOverride, delHostOverride and reconfigure.
//
// It is deliberately permissive about authentication and validation. Its job is
// to stand in for the appliance, not to re-implement it; the provider's own
// tests pin what it sends.
type OPNsense struct {
	mu     sync.Mutex
	store  map[string]HostOverride
	nextID int
	calls  []string
}

// NewOPNsense returns an empty fake with no overrides.
func NewOPNsense() *OPNsense {
	return &OPNsense{store: map[string]HostOverride{}}
}

// ServeHTTP routes the API paths the provider calls.
func (f *OPNsense) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	switch {
	case r.URL.Path == "/api/unbound/settings/searchHostOverride":
		f.handleSearch(w, r)
	case r.URL.Path == "/api/unbound/settings/addHostOverride":
		f.handleAdd(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/unbound/settings/setHostOverride/"):
		f.handleSet(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/unbound/settings/delHostOverride/"):
		f.handleDel(w, r)
	case r.URL.Path == "/api/unbound/service/reconfigure":
		f.handleReconfigure(w, r)
	default:
		http.NotFound(w, r)
	}
}

// Snapshot returns a copy of the stored overrides keyed by uuid.
func (f *OPNsense) Snapshot() map[string]HostOverride {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]HostOverride, len(f.store))
	for id, h := range f.store {
		out[id] = h
	}
	return out
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
func (f *OPNsense) Seed(h HostOverride) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("uuid-%d", f.nextID)
	f.store[id] = h
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
	writeJSON(w, map[string]interface{}{"rows": rows, "total": len(rows)})
}

func (f *OPNsense) handleAdd(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Host HostOverride `json:"host"`
	}
	if err := readJSON(r, &payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	id := strings.TrimPrefix(r.URL.Path, "/api/unbound/settings/setHostOverride/")
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
		http.Error(w, `{"result":"not found"}`, http.StatusNotFound)
		return
	}
	f.store[id] = payload.Host
	writeJSON(w, map[string]string{"result": "saved"})
}

func (f *OPNsense) handleDel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/unbound/settings/delHostOverride/")

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.store[id]; !ok {
		http.Error(w, `{"result":"not found"}`, http.StatusNotFound)
		return
	}
	delete(f.store, id)
	writeJSON(w, map[string]string{"result": "deleted"})
}

func (f *OPNsense) handleReconfigure(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
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
