package testenv

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests pin the fake's contract with the real appliance. The provider's
// tests trust the fake, so a drift here would let a provider bug through.

func call(t *testing.T, srv *httptest.Server, method, path, body string, auth bool) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if auth {
		req.SetBasicAuth("key", "secret")
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

const validHost = `{"host":{"enabled":"1","hostname":"app","domain":"example.com","rr":"A","server":"10.0.0.1"}}`

func TestFakeRequiresCredentialsWhenConfigured(t *testing.T) {
	fake := NewOPNsense()
	fake.RequireCredentials("key", "secret")
	srv := httptest.NewServer(fake)
	defer srv.Close()

	if status, _ := call(t, srv, http.MethodGet, searchPath, "", false); status != http.StatusUnauthorized {
		t.Errorf("no credentials: status %d, want 401", status)
	}
	if status, _ := call(t, srv, http.MethodGet, searchPath, "", true); status != http.StatusOK {
		t.Errorf("right credentials: status %d, want 200", status)
	}
}

func TestFakeUnknownUUIDAnswers200WithResult(t *testing.T) {
	srv := httptest.NewServer(NewOPNsense())
	defer srv.Close()

	status, body := call(t, srv, http.MethodPost, setPathPrefix+"nope", validHost, true)
	if status != http.StatusOK || body["result"] != "failed" {
		t.Errorf("set unknown uuid: %d %v, want 200 result=failed", status, body)
	}
	status, body = call(t, srv, http.MethodPost, delPathPrefix+"nope", "{}", true)
	if status != http.StatusOK || body["result"] != "not found" {
		t.Errorf("del unknown uuid: %d %v, want 200 result=not found", status, body)
	}
}

func TestFakeValidatesRows(t *testing.T) {
	srv := httptest.NewServer(NewOPNsense())
	defer srv.Close()

	cases := map[string]string{
		"ipv6 in an A record": `{"host":{"enabled":"1","hostname":"app","domain":"example.com","rr":"A","server":"::1"}}`,
		"not an ip":           `{"host":{"enabled":"1","hostname":"app","domain":"example.com","rr":"A","server":"nope"}}`,
		"missing domain":      `{"host":{"enabled":"1","hostname":"app","domain":"","rr":"A","server":"10.0.0.1"}}`,
		"unknown rr":          `{"host":{"enabled":"1","hostname":"app","domain":"example.com","rr":"TXT","server":"10.0.0.1"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			status, out := call(t, srv, http.MethodPost, addPath, body, true)
			if status != http.StatusOK || out["result"] != "failed" || out["validations"] == nil {
				t.Errorf("got %d %v, want 200 result=failed with validations", status, out)
			}
		})
	}
}

func TestFakeWritesRequirePOST(t *testing.T) {
	fake := NewOPNsense()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	if _, out := call(t, srv, http.MethodGet, addPath, validHost, true); out["result"] != "failed" {
		t.Errorf("GET add: %v, want result=failed", out)
	}
	if n := len(fake.Snapshot()); n != 0 {
		t.Errorf("GET add stored %d rows, want 0", n)
	}
}

func TestFakePublishesOnlyOnReconfigure(t *testing.T) {
	fake := NewOPNsense()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	call(t, srv, http.MethodPost, addPath, validHost, true)
	if len(fake.Snapshot()) != 1 || len(fake.Published()) != 0 {
		t.Fatalf("after add: staged %d published %d, want 1 and 0", len(fake.Snapshot()), len(fake.Published()))
	}

	fake.SetFailure(EndpointReconfigure, http.StatusInternalServerError)
	if status, _ := call(t, srv, http.MethodPost, reconfigurePath, "{}", true); status != http.StatusInternalServerError {
		t.Fatalf("injected reconfigure failure: status %d, want 500", status)
	}
	if len(fake.Published()) != 0 {
		t.Fatal("a failed reconfigure published the staged config")
	}

	fake.SetFailure(EndpointReconfigure, 0)
	if _, out := call(t, srv, http.MethodPost, reconfigurePath, "{}", true); out["status"] != "ok" {
		t.Fatalf("reconfigure: %v, want status=ok", out)
	}
	if len(fake.Published()) != 1 {
		t.Fatalf("after reconfigure: published %d, want 1", len(fake.Published()))
	}
}

func TestFakeSeedIsPublished(t *testing.T) {
	fake := NewOPNsense()
	id := fake.Seed(HostOverride{Enabled: "1", Hostname: "nas", Domain: "example.com", RR: "A", Server: "10.0.0.50"})
	if _, ok := fake.Published()[id]; !ok {
		t.Error("a seeded row should count as already published")
	}
}
