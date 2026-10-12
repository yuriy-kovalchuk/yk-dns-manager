package opnsense

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// newTestCA mints a throwaway CA — the shape of a default OPNsense install's
// self-signed certificate.
func newTestCA(t *testing.T) (pemBytes []byte, cert *x509.Certificate, key *ecdsa.PrivateKey) {
	t.Helper()

	notBefore := time.Now().Add(-time.Hour)
	notAfter := time.Now().Add(time.Hour)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "yk-dns-manager test CA"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), cert, key
}

// newSelfSignedAPI stands up an HTTPS endpoint whose certificate chains to that
// CA. It returns the base URL and the CA in PEM form.
func newSelfSignedAPI(t *testing.T) (string, []byte) {
	t.Helper()

	notBefore := time.Now().Add(-time.Hour)
	notAfter := time.Now().Add(time.Hour)

	caPEM, caCert, caKey := newTestCA(t)

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "opnsense.test"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/unbound/settings/searchHostOverride", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rows":[]}`))
	})

	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return srv.URL, caPEM
}

func baseSettings(baseURL string) map[string]string {
	return map[string]string{
		"base_url":   baseURL + "/api",
		"api_key":    "key123",
		"api_secret": "secret456",
	}
}

// TestNew_TLSVerification is the end-to-end proof that ca_file actually fixes
// the problem skip_tls_verify was being used to paper over: with the CA pinned,
// the handshake succeeds and verification stays on.
func TestNew_TLSVerification(t *testing.T) {
	baseURL, caPEM := newSelfSignedAPI(t)

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatalf("write ca_file: %v", err)
	}

	cases := []struct {
		name     string
		extra    map[string]string
		wantOK   bool
		contains string
	}{
		{
			// Without a pinned CA the appliance is untrusted, and this is the
			// failure operators were reaching for skip_tls_verify to silence.
			name:     "default verification rejects an untrusted appliance",
			wantOK:   false,
			contains: "certificate",
		},
		{
			name:   "ca_file pins the appliance CA and verification stays on",
			extra:  map[string]string{"ca_file": caFile},
			wantOK: true,
		},
		{
			name:   "skip_tls_verify accepts the untrusted cert",
			extra:  map[string]string{"skip_tls_verify": "true"},
			wantOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := baseSettings(baseURL)
			for k, v := range tc.extra {
				settings[k] = v
			}

			p, err := New(logr.Discard(), settings)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			err = p.HealthCheck(context.Background())
			if tc.wantOK {
				if err != nil {
					t.Fatalf("HealthCheck: unexpected error %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected the handshake to fail, got success")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.contains)
			}
		})
	}
}

func TestNew_TLSConfigErrors(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "not-a-cert.pem")
	if err := os.WriteFile(notPEM, []byte("this is not a certificate"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	validCA := filepath.Join(dir, "ca.pem")
	caPEM, _, _ := newTestCA(t)
	if err := os.WriteFile(validCA, caPEM, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cases := []struct {
		name     string
		extra    map[string]string
		contains string
	}{
		{
			name:     "ca_file that does not exist fails at startup",
			extra:    map[string]string{"ca_file": filepath.Join(dir, "missing.pem")},
			contains: "reading ca_file",
		},
		{
			// A silently empty cert pool would fall back to the system roots and
			// the operator would believe the appliance CA was pinned.
			name:     "ca_file with no PEM certificate is rejected",
			extra:    map[string]string{"ca_file": notPEM},
			contains: "no valid PEM certificate",
		},
		{
			name:     "ca_file and skip_tls_verify together are rejected",
			extra:    map[string]string{"ca_file": validCA, "skip_tls_verify": "true"},
			contains: "mutually exclusive",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := baseSettings("https://opnsense.local")
			for k, v := range tc.extra {
				settings[k] = v
			}
			_, err := New(logr.Discard(), settings)
			if err == nil {
				t.Fatal("expected New to fail")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.contains)
			}
		})
	}
}

// captureSink collects logr Info messages so the warning can be asserted on.
type captureSink struct {
	msgs *[]string
}

func (c captureSink) Init(logr.RuntimeInfo)          {}
func (c captureSink) Enabled(int) bool               { return true }
func (c captureSink) Error(error, string, ...any)    {}
func (c captureSink) WithName(string) logr.LogSink   { return c }
func (c captureSink) WithValues(...any) logr.LogSink { return c }
func (c captureSink) Info(_ int, msg string, _ ...any) {
	*c.msgs = append(*c.msgs, msg)
}

// TestNew_SkipTLSVerifyWarns asserts the operator is told, on every startup,
// that credentials are being sent to whoever answers on the network path.
func TestNew_SkipTLSVerifyWarns(t *testing.T) {
	msgs := &[]string{}
	log := logr.New(captureSink{msgs: msgs})

	if _, err := New(log, baseSettings("https://opnsense.local")); err != nil {
		t.Fatalf("New without skip_tls_verify: %v", err)
	}
	if len(*msgs) != 0 {
		t.Errorf("a default configuration should not warn, got %v", *msgs)
	}

	settings := baseSettings("https://opnsense.local")
	settings["skip_tls_verify"] = "true"
	if _, err := New(log, settings); err != nil {
		t.Fatalf("New with skip_tls_verify: %v", err)
	}
	joined := strings.Join(*msgs, "\n")
	if !strings.Contains(joined, "skip_tls_verify") {
		t.Errorf("expected a warning naming skip_tls_verify, got %q", joined)
	}
	if !strings.Contains(joined, "ca_file") {
		t.Errorf("expected the warning to point at ca_file as the fix, got %q", joined)
	}
}

// TestNew_Scheme covers plain HTTP. The credentials go out as basic auth on
// every request, so http is refused unless the operator opts in, and the opt-in
// warns on every startup.
func TestNew_Scheme(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		allow   string
		wantErr string
		warns   bool
	}{
		{name: "https", url: "https://opnsense.local"},
		{name: "http refused", url: "http://opnsense.local", wantErr: "cleartext"},
		{name: "http refused with False", url: "http://opnsense.local", allow: "False", wantErr: "cleartext"},
		{name: "http with a value that is not a boolean", url: "http://opnsense.local", allow: "yes", wantErr: "must be true or false"},
		{name: "http allowed with True", url: "http://opnsense.local", allow: "True", warns: true},
		{name: "http allowed", url: "http://opnsense.local", allow: "true", warns: true},
		{name: "other scheme", url: "ftp://opnsense.local", allow: "true", wantErr: "must use https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := &[]string{}
			settings := baseSettings(tc.url)
			if tc.allow != "" {
				settings["allow_insecure_http"] = tc.allow
			}

			_, err := New(logr.New(captureSink{msgs: msgs}), settings)

			if tc.wantErr == "" && err != nil {
				t.Fatalf("New: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("New error = %v, want it to contain %q", err, tc.wantErr)
			}
			warned := strings.Contains(strings.Join(*msgs, "\n"), "allow_insecure_http")
			if warned != tc.warns {
				t.Errorf("warned = %v, want %v (messages %v)", warned, tc.warns, *msgs)
			}
		})
	}
}

// TestNew_SkipTLSVerifyIsABoolean is the deferred part of audit D11: the value
// is parsed as a boolean, so "True" turns verification off and a value that
// is not a boolean fails instead of quietly meaning "off".
func TestNew_SkipTLSVerifyIsABoolean(t *testing.T) {
	cases := []struct {
		value    string
		wantSkip bool
		wantErr  bool
	}{
		{"true", true, false},
		{"True", true, false},
		{"1", true, false},
		{"false", false, false},
		{"", false, false},
		{"yes", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			settings := baseSettings("https://opnsense.local")
			settings["skip_tls_verify"] = tc.value
			p, err := New(logr.Discard(), settings)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "must be true or false") {
					t.Fatalf("New = %v, want a boolean parse error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			tlsConfig := p.client.Transport.(*http.Transport).TLSClientConfig
			skipped := tlsConfig != nil && tlsConfig.InsecureSkipVerify
			if skipped != tc.wantSkip {
				t.Errorf("verification skipped = %v, want %v", skipped, tc.wantSkip)
			}
		})
	}
}
