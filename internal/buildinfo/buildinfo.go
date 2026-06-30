// Package buildinfo holds version metadata stamped at build time via
// -ldflags, so the About tab, /api/version, and release tags never drift.
// Unstamped builds (go run, plain go build) fall back to "dev".
package buildinfo

var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)
