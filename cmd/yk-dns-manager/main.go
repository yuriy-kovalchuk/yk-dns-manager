package main

import (
	"flag"
	"fmt"
	"os"

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
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
	_ "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/providers"
)

var (
	scheme  = runtime.NewScheme()
	Version = "dev"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
}

func main() {
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	log := ctrl.Log.WithName("setup")

	log.Info("starting yk-dns-manager", "version", Version)

	domainMapPath := os.Getenv("DOMAIN_MAP_PATH")
	if domainMapPath == "" {
		domainMapPath = "configs/domain-map.yaml"
	}
	domainMap, err := config.LoadDomainMap(domainMapPath)
	if err != nil {
		return fmt.Errorf("unable to load domain map: %w", err)
	}
	log.Info("loaded domain map", "path", domainMapPath)

	providerCfg, err := config.LoadProviderConfig()
	if err != nil {
		return fmt.Errorf("unable to load provider config: %w", err)
	}
	log.Info("loaded provider config", "provider", providerCfg.Provider)

	dnsProvider, err := dns.NewProvider(providerCfg.Provider, ctrl.Log.WithName("dns-"+providerCfg.Provider), providerCfg.Settings)
	if err != nil {
		return fmt.Errorf("unable to create DNS provider: %w", err)
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

	log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("manager exited with error: %w", err)
	}

	return nil
}
