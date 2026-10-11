// Command fake-opnsense serves an in-memory OPNsense Unbound DNS API so the
// controller can be run end to end without an appliance. It is the same fake the
// integration tests run against, so what you see locally is what the tests pin.
//
// It stores everything in memory and forgets it on exit. Do not point it at a
// real appliance's configuration, and do not put real credentials in it: it
// accepts any credentials, or none.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/testenv"
)

func main() {
	addr := flag.String("addr", ":8053", "listen address")
	seed := flag.String("seed", "", "overrides to prepopulate, comma separated fqdn=ip pairs, e.g. app.example.com=10.0.0.1")
	flag.Parse()

	fake := testenv.NewOPNsense()
	for _, pair := range strings.Split(*seed, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		fqdn, ip, ok := strings.Cut(pair, "=")
		if !ok {
			fmt.Fprintf(os.Stderr, "error: bad --seed entry %q, want fqdn=ip\n", pair)
			os.Exit(1)
		}
		host, domain, ok := strings.Cut(fqdn, ".")
		if !ok || host == "" || domain == "" {
			fmt.Fprintf(os.Stderr, "error: bad --seed fqdn %q, want a fully qualified name\n", fqdn)
			os.Exit(1)
		}
		fake.Seed(testenv.HostOverride{
			Enabled:     "1",
			Hostname:    host,
			Domain:      domain,
			RR:          "A",
			Server:      ip,
			Description: "seeded by fake-opnsense",
		})
	}

	mux := http.NewServeMux()
	mux.Handle("/", fake)
	mux.HandleFunc("/records", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fake.Snapshot())
	})

	server := &http.Server{
		Addr:              *addr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	fmt.Printf("fake OPNsense listening on %s\n", *addr)
	fmt.Printf("  provider settings.base_url: http://localhost%s/api\n", *addr)
	fmt.Printf("  plain HTTP also needs:      allow_insecure_http: \"true\"\n")
	fmt.Printf("  current overrides:          http://localhost%s/records\n", *addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// logRequests prints one line per request, including whether the controller
// presented credentials. A missing Authorization header almost always means the
// provider config is pointing somewhere it should not be. The credential itself
// is never printed.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		_, _, hasAuth := r.BasicAuth()
		next.ServeHTTP(rec, r)
		auth := "none"
		if hasAuth {
			auth = "present"
		}
		fmt.Printf("%s %s -> %d (auth: %s)\n", r.Method, r.URL.Path, rec.status, auth)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
