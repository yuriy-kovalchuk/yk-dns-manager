// Package main is the entrypoint for the yk-dns-manager Kubernetes controller.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"sort"
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
	fn   func(logr.Logger, map[string]string) (dns.Provider, error)
}

// newProviders returns the available DNS providers. The provider config's
// `provider` field picks one by name. Add entries here when a new provider is
// added.
func newProviders() []providerFactory {
	return []providerFactory{
		{"opnsense", func(log logr.Logger, settings map[string]string) (dns.Provider, error) {
			return opnsense.New(log, settings)
		}},
	}
	// Example for adding a new provider:
	//   {"pihole", func(log logr.Logger, settings map[string]string) (dns.Provider, error) {
	//       return pihole.New(log, settings)
	//   }},
}

// selectProvider initializes the provider the config names. It does not fall
// back to another one: a typo in the name, or settings the named provider
// rejects, is a configuration error and is reported as one.
//
// The provider is returned only after the error is checked. A factory such as
// opnsense.New returns a concrete *Provider; when it fails that pointer is nil,
// and converting it to the dns.Provider interface produces an interface that is
// not == nil. Returning it and calling a method later panics with a nil
// dereference instead of reporting the configuration error.
func selectProvider(log logr.Logger, factories []providerFactory, name string, settings map[string]string) (dns.Provider, error) {
	known := make([]string, 0, len(factories))
	for _, pf := range factories {
		if pf.name != name {
			known = append(known, pf.name)
			continue
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
	)

	ctx := context.Background()
	log := ctrl.Log.WithName("setup")

	flag.StringVar(&logLevel, "zap-log-level", os.Getenv("LOG_LEVEL"), "log level: debug, info, warn, error, or a numeric logr verbosity such as -3")
	flag.StringVar(&domainMapPath, "domain-map-path", os.Getenv("DOMAIN_MAP_PATH"), "path to domain map YAML file")
	flag.BoolVar(&requireAccepted, "require-accepted-routes", false, "only publish DNS for HTTPRoutes that at least one parent Gateway has accepted")
	flag.DurationVar(&cleanupGracePeriod, "cleanup-grace-period", 5*time.Minute, "how long a failing DNS cleanup of a deleted HTTPRoute is retried before the route is released anyway")
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

	// Log what the controller will actually manage. A domain map that silently
	// matches nothing is the most common way this controller appears to do
	// nothing, so the count goes at info level and the pairs at debug.
	entries := domainMap.Entries()
	log.Info("loaded domain map", "path", domainMapPath, "entries", len(entries))
	domains := make([]string, 0, len(entries))
	for d := range entries {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	for _, d := range domains {
		log.V(1).Info("domain map entry", "domain", d, "ip", entries[d])
	}

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
	// A short deadline of its own: the HTTP client's 30s timeout applies per
	// request, and startup should not hang on an appliance that is down.
	hctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	err = dnsProvider.HealthCheck(hctx)
	cancel()
	if err != nil {
		// Non-fatal on purpose. The appliance may be down or the credentials may
		// be wrong, and exiting here CrashLoops the pod without ever surfacing the
		// reason through the reconciler. Every reconcile reports the same failure
		// and requeues with backoff, so a provider that comes back recovers on its
		// own without a restart.
		log.Error(err, "DNS provider is unreachable, continuing anyway", "name", providerCfg.Provider)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: ":9090"},
		HealthProbeBindAddress: ":8081",
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

	// Signal handling per Go engineering standard:
	// explicit signal.NotifyContext, not controller-runtime wrapper.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("manager exited with error: %w", err)
	}

	return nil
}

// newLogger builds the process logger. Development stays off: it swaps in a
// console encoder, attaches stack traces and turns DPanic into a real panic, and
// coupling it to "debug" put all three in production clusters. Debug output comes
// from the level, not from development mode.
func newLogger(level zapcore.Level) logr.Logger {
	return zap.New(zap.UseFlagOptions(&zap.Options{Level: level}))
}

// parseLogLevel maps the --zap-log-level value to a zap level. logr verbosity is
// the negative of the zap level, so V(1) needs zapcore.DebugLevel (-1); an
// operator can ask for deeper verbosity with a numeric value such as "-3".
func parseLogLevel(s string) (zapcore.Level, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return zapcore.InfoLevel, nil
	}
	// zapcore.ParseLevel only accepts non-negative numbers, which would leave
	// logr verbosity above V(0) unreachable. Handle the numeric form here so an
	// operator can request V(3) with "-3".
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
