// Package metrics defines every yk_dns_manager_* metric in one place, registered on controller-runtime's registry so they are served next to its own on the manager's metrics endpoint.
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// BuildInfo is always 1; its labels say which version runs against which provider.
	BuildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "yk_dns_manager_build_info",
		Help: "Always 1; labelled with the controller version and the configured DNS provider.",
	}, []string{"version", "provider"})

	// RecordResults counts what Ensure did, plus released records.
	RecordResults = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "yk_dns_manager_records_total",
		Help: "DNS record outcomes: created, updated, unchanged, kept, conflict or released.",
	}, []string{"result"})

	// DNSErrors counts failed provider calls by the step that failed.
	DNSErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "yk_dns_manager_dns_errors_total",
		Help: "Failed DNS operations by stage: ensure, release or publish.",
	}, []string{"stage"})

	// CleanupsAbandoned counts deleted routes released after the grace period with their cleanup still failing.
	CleanupsAbandoned = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "yk_dns_manager_cleanups_abandoned_total",
		Help: "Deleted HTTPRoutes released after --cleanup-grace-period while their DNS cleanup still failed; their records may be left behind.",
	})

	// Publishes counts publish attempts: ok, failed, or deferred to coalesce a burst.
	Publishes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "yk_dns_manager_publishes_total",
		Help: "Publishes of staged DNS changes by result: ok, failed, or deferred by publish_interval.",
	}, []string{"result"})

	// PublishDuration times publishes that ran; for OPNsense that is an Unbound reconfigure, which restarts the resolver.
	PublishDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "yk_dns_manager_publish_duration_seconds",
		Help:    "Duration of publishes that ran, successful or not.",
		Buckets: prometheus.ExponentialBuckets(0.05, 2, 10), // 50ms to ~25s
	})

	// ProviderRequests counts API requests to the DNS backend; code is the HTTP status, or "error" when no response arrived.
	ProviderRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "yk_dns_manager_provider_requests_total",
		Help: "API requests to the DNS backend by provider, endpoint and HTTP status code (\"error\" when no response arrived).",
	}, []string{"provider", "endpoint", "code"})

	// ProviderRequestDuration times API requests to the DNS backend.
	ProviderRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "yk_dns_manager_provider_request_duration_seconds",
		Help:    "Duration of API requests to the DNS backend by provider and endpoint.",
		Buckets: prometheus.DefBuckets,
	}, []string{"provider", "endpoint"})

	// ManagedRecords has one series per hostname a route manages; set it only through SetManagedRecords.
	ManagedRecords = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "yk_dns_manager_managed_record_info",
		Help: "Always 1, one series per hostname an HTTPRoute manages, with the IP its record points at; only the leader reconciles, so other replicas export none.",
	}, []string{"route_namespace", "route", "hostname", "ip"})
)

// managed remembers each route's series, so a route's next set replaces them all.
var managed = struct {
	sync.Mutex
	byRoute map[string][]prometheus.Labels
}{byRoute: map[string][]prometheus.Labels{}}

func init() {
	metrics.Registry.MustRegister(BuildInfo, RecordResults, DNSErrors, CleanupsAbandoned,
		Publishes, PublishDuration, ProviderRequests, ProviderRequestDuration, ManagedRecords)
	// rate() and increase() cannot see a series' first sample, so a counter that only appears on its first increment hides it: start every known one at 0.
	for _, result := range []string{"created", "updated", "unchanged", "kept", "conflict", "released"} {
		RecordResults.WithLabelValues(result)
	}
	for _, stage := range []string{"ensure", "release", "publish"} {
		DNSErrors.WithLabelValues(stage)
	}
	for _, result := range []string{"ok", "failed", "deferred"} {
		Publishes.WithLabelValues(result)
	}
}

// SetManagedRecords replaces a route's series with records (hostname to IP); nil forgets the route.
func SetManagedRecords(namespace, name string, records map[string]string) {
	managed.Lock()
	defer managed.Unlock()
	key := namespace + "/" + name
	for _, l := range managed.byRoute[key] {
		ManagedRecords.Delete(l)
	}
	delete(managed.byRoute, key)
	for hostname, ip := range records {
		l := prometheus.Labels{"route_namespace": namespace, "route": name, "hostname": hostname, "ip": ip}
		ManagedRecords.With(l).Set(1)
		managed.byRoute[key] = append(managed.byRoute[key], l)
	}
}
