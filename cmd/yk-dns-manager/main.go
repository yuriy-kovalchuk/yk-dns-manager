// Package main is the entrypoint for the yk-dns-manager Kubernetes controller.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/config"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/controller"
	dns "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
	opnsense "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/opnsense"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/version"
)

// providerFactory is a single provider candidate for initialization.
type providerFactory struct {
	name string
	// settings lists every key the provider understands, so a typo such as "sip_tls_verify" fails startup; nil skips the check.
	settings []string
	fn       func(logr.Logger, map[string]string) (dns.Provider, error)
}

// newProviders lists the available providers, picked by name from the config; add entries here for a new backend.
func newProviders() []providerFactory {
	return []providerFactory{
		{
			name:     "opnsense",
			settings: []string{"base_url", "api_key", "api_secret", "ca_file", "skip_tls_verify", "allow_insecure_http", "default_ttl", "publish_interval"},
			fn: func(log logr.Logger, settings map[string]string) (dns.Provider, error) {
				return opnsense.New(log, settings)
			},
		},
	}
}

// selectProvider initializes exactly the named provider: a bad name or rejected settings is a config error, and the provider is returned only after the error check because a nil concrete pointer becomes a non-nil interface.
func selectProvider(log logr.Logger, factories []providerFactory, name string, settings map[string]string) (dns.Provider, error) {
	known := make([]string, 0, len(factories))
	for _, pf := range factories {
		if pf.name != name {
			known = append(known, pf.name)
			continue
		}
		if pf.settings != nil {
			var unknown []string
			for key := range settings {
				if !slices.Contains(pf.settings, key) {
					unknown = append(unknown, key)
				}
			}
			if len(unknown) > 0 {
				slices.Sort(unknown)
				return nil, fmt.Errorf("DNS provider %q: unknown settings %s; known settings: %s",
					name, strings.Join(unknown, ", "), strings.Join(pf.settings, ", "))
			}
		}
		p, err := pf.fn(log.WithName("dns-"+pf.name), settings)
		if err != nil {
			return nil, fmt.Errorf("initializing DNS provider %q: %w", name, err)
		}
		return p, nil
	}
	return nil, fmt.Errorf("unknown DNS provider %q; available: %s", name, strings.Join(known, ", "))
}

// healthCheckTimeout bounds the startup connectivity check.
const healthCheckTimeout = 10 * time.Second

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		logLevel           string
		domainMapPath      string
		domainMap          *config.DomainMap
		cleanupGracePeriod time.Duration
		requireAccepted    bool
		leaderElect        bool
		resyncPeriod       time.Duration
	)

	ctx := context.Background()
	log := ctrl.Log.WithName("setup")

	flag.StringVar(&logLevel, "zap-log-level", os.Getenv("LOG_LEVEL"), "log level: debug, info, warn, error, or a numeric logr verbosity such as -3")
	flag.StringVar(&domainMapPath, "domain-map-path", os.Getenv("DOMAIN_MAP_PATH"), "path to domain map YAML file")
	flag.BoolVar(&leaderElect, "leader-elect", true, "elect a leader so only one replica manages DNS at a time; turn off only when running a single instance outside a cluster")
	flag.BoolVar(&requireAccepted, "require-accepted-routes", false, "only publish DNS for HTTPRoutes that at least one parent Gateway has accepted")
	flag.DurationVar(&cleanupGracePeriod, "cleanup-grace-period", 5*time.Minute, "how long a failing DNS cleanup of a deleted HTTPRoute is retried before the route is released anyway")
	flag.DurationVar(&resyncPeriod, "resync-period", time.Hour, "how often every HTTPRoute is reconciled again even when nothing changed in the cluster, which is what republishes a record someone changed or deleted on the DNS backend; 0 turns it off")
	flag.Parse()

	level, err := parseLogLevel(logLevel)
	if err != nil {
		return err
	}
	// The level is what turns debug output on.
	ctrl.SetLogger(newLogger(level))

	log.Info("starting yk-dns-manager", "version", version.Version, "logLevel", level.String())

	if domainMapPath == "" {
		return fmt.Errorf("--domain-map-path flag or DOMAIN_MAP_PATH environment variable is required")
	}
	domainMap, err = config.LoadDomainMap(domainMapPath)
	if err != nil {
		return fmt.Errorf("unable to load domain map: %w", err)
	}

	logDomainMap(log, domainMapPath, domainMap)

	providerCfg, err := config.LoadProviderConfig()
	if err != nil {
		return fmt.Errorf("unable to load provider config: %w", err)
	}
	log.Info("loaded provider config", "provider", providerCfg.Provider, "upsert", providerCfg.Upsert)

	dnsProvider, err := selectProvider(ctrl.Log, newProviders(), providerCfg.Provider, providerCfg.Settings)
	if err != nil {
		return err
	}
	log.Info("initialized DNS provider", "name", providerCfg.Provider)

	log.Info("checking DNS provider connectivity")
	// Its own short deadline: the HTTP client's 30s timeout is per request, and startup must not hang on an appliance that is down.
	hctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	err = dnsProvider.HealthCheck(hctx)
	cancel()
	if err != nil {
		// Non-fatal on purpose: an unreachable appliance would CrashLoop the pod here, while every reconcile reports the same failure and recovers without a restart.
		log.Error(err, "DNS provider is unreachable, continuing anyway", "name", providerCfg.Provider)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: ":9090"},
		HealthProbeBindAddress: ":8081",
		// The informer re-delivers every route on this interval and the filter lets those resyncs through, so a record changed on the appliance is republished without a cluster edit.
		Cache: cache.Options{SyncPeriod: &resyncPeriod},
		// Only the Lease holder reconciles: two replicas racing on the appliance create duplicate overrides. The Lease lives in the pod's namespace and is released on shutdown.
		LeaderElection:                leaderElect,
		LeaderElectionID:              "yk-dns-manager.dns.yk",
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return fmt.Errorf("unable to create manager: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up ready check: %w", err)
	}

	reconciler := &controller.HTTPRouteReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Log:       ctrl.Log.WithName("httproute-controller"),
		Recorder:  mgr.GetEventRecorder("yk-dns-manager"),
		DomainMap: domainMap,
		DNS:       dnsProvider,
		Upsert:    providerCfg.Upsert,

		RequireAccepted:    requireAccepted,
		CleanupGracePeriod: cleanupGracePeriod,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to set up HTTPRoute controller: %w", err)
	}

	// Signal handling per the Go standard: explicit signal.NotifyContext, not the controller-runtime wrapper.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("manager exited with error: %w", err)
	}

	return nil
}

// logDomainMap says what the controller will actually manage: a map that silently matches nothing is the usual way it appears to do nothing, so the count is info and the pairs debug.
func logDomainMap(log logr.Logger, path string, domainMap *config.DomainMap) {
	entries := domainMap.Entries()
	log.Info("loaded domain map", "path", path, "entries", len(entries))
	domains := make([]string, 0, len(entries))
	for d := range entries {
		domains = append(domains, d)
	}
	slices.Sort(domains)
	for _, d := range domains {
		log.V(1).Info("domain map entry", "domain", d, "ip", entries[d])
		// exact keys used to cover every name below them too, so an upgrade with such a key releases the subdomains' records
		if !strings.HasPrefix(d, "*.") && entries["*."+d] == "" {
			log.Info("domain map key matches only this exact name; use \"*."+d+"\" to also cover the names below it", "domain", d)
		}
	}
}

// newLogger keeps zap development mode off: it swaps in a console encoder, attaches stack traces and turns DPanic into a panic, so debug comes from the level instead.
func newLogger(level zapcore.Level) logr.Logger {
	return zap.New(zap.UseFlagOptions(&zap.Options{Level: level}))
}

// parseLogLevel maps --zap-log-level to a zap level; logr verbosity is the negative of it, so V(1) needs zapcore.DebugLevel (-1).
func parseLogLevel(s string) (zapcore.Level, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return zapcore.InfoLevel, nil
	}
	// zapcore.ParseLevel only accepts non-negative numbers, so the numeric form is handled here to keep logr verbosity above V(0) reachable
	if n, err := strconv.Atoi(s); err == nil {
		if n < math.MinInt8 || n > math.MaxInt8 {
			return zapcore.InfoLevel, fmt.Errorf("invalid --zap-log-level %q: out of range", s)
		}
		return zapcore.Level(n), nil
	}
	level, err := zapcore.ParseLevel(s)
	if err != nil {
		return zapcore.InfoLevel, fmt.Errorf("invalid --zap-log-level %q: %w", s, err)
	}
	return level, nil
}
