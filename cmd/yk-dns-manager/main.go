// Package main is the entrypoint for the yk-dns-manager Kubernetes controller.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/app"
)

func main() {
	// Signal handling per the Go standard: explicit signal.NotifyContext, not the controller-runtime wrapper.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
