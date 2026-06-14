// Package version provides build-time version information injected via ldflags.
package version

import "runtime"

// Version holds the application version (set at build time via ldflags).
// Commit holds the git commit hash (set at build time via ldflags).
// BuildDate holds the ISO 8601 build timestamp (set at build time via ldflags).
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// GoVersion returns the Go runtime version used to build this binary.
func GoVersion() string { return runtime.Version() }
