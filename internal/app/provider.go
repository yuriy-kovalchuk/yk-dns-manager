package app

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/config"
	dns "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/providers"
)

// healthCheckTimeout bounds the startup connectivity check.
const healthCheckTimeout = 10 * time.Second

// newProvider builds the configured provider and checks it can reach its backend.
func newProvider(ctx context.Context, log logr.Logger, cfg *config.Config) (dns.Provider, error) {
	p, err := providers.New(ctrl.Log, cfg.Provider, cfg.Settings)
	if err != nil {
		return nil, err
	}
	log.Info("initialized DNS provider", "name", cfg.Provider)

	log.Info("checking DNS provider connectivity")
	// Its own short deadline: the HTTP client's 30s timeout is per request, and startup must not hang on an appliance that is down.
	hctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	if err := p.HealthCheck(hctx); err != nil {
		// Non-fatal on purpose: an unreachable appliance would CrashLoop the pod here, while every reconcile reports the same failure and recovers without a restart.
		log.Error(err, "DNS provider is unreachable, continuing anyway", "name", cfg.Provider)
	}
	return p, nil
}
