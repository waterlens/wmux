// Package version contains build metadata injected by the release build.
package version

// These values are overridden with -ldflags for release builds.
var (
	Version = "dev"
	Commit  = "unknown"
)
