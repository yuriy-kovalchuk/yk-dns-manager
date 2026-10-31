// Package app wires the controller together: flags, logging, config, the DNS provider and the manager.
package app

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/logging"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/version"
)

// Run starts the controller and blocks until ctx is cancelled or the manager fails.
func Run(ctx context.Context) error {
	opts := parseFlags()

	level, err := logging.ParseLevel(opts.logLevel)
	if err != nil {
		return err
	}
	ctrl.SetLogger(logging.New(level))
	log := ctrl.Log.WithName("setup")
	log.Info("starting yk-dns-manager", "version", version.Version, "logLevel", level.String())

	cfg, err := loadConfig(log, opts.configPath)
	if err != nil {
		return err
	}

	provider, err := newProvider(ctx, log, cfg)
	if err != nil {
		return err
	}

	mgr, err := newManager(opts, cfg, provider)
	if err != nil {
		return err
	}

	log.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("manager exited with error: %w", err)
	}
	return nil
}
