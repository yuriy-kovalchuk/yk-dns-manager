// Package main is the entrypoint for the yk-dns-manager Kubernetes controller.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-logr/logr"
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

// newProviders returns the ordered list of available DNS providers. The first
// one whose mandatory config fields are satisfied wins. Add entries here when
// a new provider is added.
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
		logLevel      string
		domainMapPath string
		domainMap     *config.DomainMap
		dnsProvider   dns.Provider
	)

	ctx := context.Background()
	log := ctrl.Log.WithName("setup")

	flag.StringVar(&logLevel, "zap-log-level", os.Getenv("LOG_LEVEL"), "log level (debug, info, warn, error)")
	flag.StringVar(&domainMapPath, "domain-map-path", os.Getenv("DOMAIN_MAP_PATH"), "path to domain map YAML file")
	flag.Parse()

	opts := zap.Options{Development: logLevel == "debug"}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	log.Info("starting yk-dns-manager", "version", version.Version)

	if domainMapPath == "" {
		return fmt.Errorf("--domain-map-path flag or DOMAIN_MAP_PATH environment variable is required")
	}
	var err error
	domainMap, err = config.LoadDomainMap(domainMapPath)
	if err != nil {
		return fmt.Errorf("unable to load domain map: %w", err)
	}

	providerCfg, err := config.LoadProviderConfig()
	if err != nil {
		return fmt.Errorf("unable to load provider config: %w", err)
	}
	log.Info("loaded provider config", "provider", providerCfg.Provider, "upsert", providerCfg.Upsert)

	var selectedProvider string
	for _, pf := range newProviders() {
		log.Info("trying to initialize provider", "name", pf.name)
		dnsProvider, err = pf.fn(ctrl.Log.WithName("dns-"+pf.name), providerCfg.Settings)
		if err != nil {
			log.Info("provider initialization failed, trying next", "name", pf.name, "error", err)
			continue
		}
		selectedProvider = pf.name
		break
	}
	if dnsProvider == nil {
		return fmt.Errorf("no DNS provider could be initialized from config")
	}
	log.Info("initialized DNS provider", "name", selectedProvider)

	log.Info("checking DNS provider connectivity")
	if err := dnsProvider.HealthCheck(ctx); err != nil {
		return fmt.Errorf("DNS provider health check failed: %w", err)
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
		DomainMap: domainMap,
		DNS:       dnsProvider,
		Upsert:    providerCfg.Upsert,
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
