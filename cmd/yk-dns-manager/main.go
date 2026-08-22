// Package main is the entrypoint for the yk-dns-manager controller:
// flag parsing, config loading, dependency assembly (internal/app), and
// controller manager startup.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/app"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/config"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/controller"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/version"
)

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
		logLevel   string
		configPath string
	)

	ctx := context.Background()
	log := ctrl.Log.WithName("setup")

	flag.StringVar(&logLevel, "zap-log-level", os.Getenv("LOG_LEVEL"), "log level (debug, info, warn, error)")
	flag.StringVar(&configPath, "config-path", os.Getenv("CONFIG_PATH"), "path to the config YAML file (domain map + providers)")
	flag.Parse()

	opts := zap.Options{Development: logLevel == "debug"}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	log.Info("starting yk-dns-manager", "version", version.Version)

	if configPath == "" {
		return fmt.Errorf("--config-path flag or CONFIG_PATH environment variable is required")
	}
	cfg, err := config.LoadConfigFromPath(configPath)
	if err != nil {
		return fmt.Errorf("unable to load config: %w", err)
	}
	log.Info("loaded config", "domains", len(cfg.DomainMap.Domains()), "providerInstances", len(cfg.Providers))

	restConfig := ctrl.GetConfigOrDie()
	k8sClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("unable to create Kubernetes client: %w", err)
	}

	// Provider instances + credential Secrets + startup health checks.
	dnsManager, err := app.Build(ctx, log, cfg, k8sClient)
	if err != nil {
		return fmt.Errorf("initializing DNS providers: %w", err)
	}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
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
		State:     controller.NewRouteState(mgr.GetClient(), mgr.GetAPIReader(), ctrl.Log.WithName("httproute-controller").WithValues("component", "route-state")),
		DomainMap: cfg.DomainMap,
		DNS:       dnsManager,
		Log:       ctrl.Log.WithName("httproute-controller"),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to set up HTTPRoute controller: %w", err)
	}

	// Graceful shutdown on SIGTERM/SIGINT.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("manager exited with error: %w", err)
	}
	return nil
}
