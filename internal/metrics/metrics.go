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

	// ManagedHostnames is the sum of every route's count; set it only through SetManagedHostnames.
	ManagedHostnames = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "yk_dns_manager_managed_hostnames",
		Help: "Hostnames the controller currently manages DNS records for; only the leader reconciles, so other replicas report 0.",
	})
)

// managed holds each route's count, since the gauge is their sum and a route's count replaces its previous one.
var managed = struct {
	sync.Mutex
	byRoute map[string]int
}{byRoute: map[string]int{}}

func init() {
	metrics.Registry.MustRegister(BuildInfo, RecordResults, DNSErrors, CleanupsAbandoned,
		Publishes, PublishDuration, ProviderRequests, ProviderRequestDuration, ManagedHostnames)
}

// SetManagedHostnames records how many hostnames route manages; 0 forgets the route.
func SetManagedHostnames(route string, n int) {
	managed.Lock()
	defer managed.Unlock()
	if n == 0 {
		delete(managed.byRoute, route)
	} else {
		managed.byRoute[route] = n
	}
	total := 0
	for _, c := range managed.byRoute {
		total += c
	}
	ManagedHostnames.Set(float64(total))
}
