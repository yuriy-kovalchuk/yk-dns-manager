package app

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/config"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/controller"
	dns "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
}

// newManager builds the controller manager with its probes and the HTTPRoute reconciler registered.
func newManager(opts options, cfg *config.Config, provider dns.Provider) (manager.Manager, error) {
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: ":9090"},
		HealthProbeBindAddress: ":8081",
		// The informer re-delivers every route on this interval and the filter lets those resyncs through, so a record changed on the appliance is republished without a cluster edit.
		Cache: cache.Options{SyncPeriod: &opts.resyncPeriod},
		// Only the Lease holder reconciles: two replicas racing on the appliance create duplicate overrides. The Lease lives in the pod's namespace and is released on shutdown.
		LeaderElection:                opts.leaderElect,
		LeaderElectionID:              "yk-dns-manager.dns.yk",
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to create manager: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("unable to set up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("unable to set up ready check: %w", err)
	}

	reconciler := &controller.HTTPRouteReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Log:       ctrl.Log.WithName("httproute-controller"),
		Recorder:  mgr.GetEventRecorder("yk-dns-manager"),
		DomainMap: cfg.DomainMap,
		DNS:       provider,
		Upsert:    cfg.Upsert,

		RequireAccepted:    opts.requireAccepted,
		CleanupGracePeriod: opts.cleanupGracePeriod,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("unable to set up HTTPRoute controller: %w", err)
	}
	return mgr, nil
}
