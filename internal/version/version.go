// Package version provides build-time version information injected via ldflags.
package version

// Version, Commit and BuildDate are set at build time via ldflags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)
