package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics on top of the reconcile counters controller-runtime already
// exports, served on the manager's metrics endpoint.
var (
	recordResults = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "yk_dns_manager_records_total",
		Help: "DNS record outcomes: created, updated, unchanged, kept, conflict or released.",
	}, []string{"result"})

	dnsErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "yk_dns_manager_dns_errors_total",
		Help: "Failed DNS operations by stage: ensure, release or publish.",
	}, []string{"stage"})
)

func init() {
	metrics.Registry.MustRegister(recordResults, dnsErrors)
}
