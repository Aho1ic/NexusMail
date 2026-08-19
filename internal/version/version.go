// Package version contains the application version embedded at build time.
package version

// Value is replaced by the release workflow through -ldflags.
var Value = "dev"
