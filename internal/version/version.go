// Package version holds build identity. The values are injected at build time by the Makefile
// (-ldflags "-X github.com/veritas501/mem-grep/internal/version.Version=…"); git is the only source
// of the version. The defaults below are what an uninjected build (`go run`, `go build` without
// make) reports, so a missing injection is obvious at a glance.
package version

import "fmt"

var (
	// Version is `git describe --tags --always --dirty`: v1.2.3, v1.2.3-4-gabc1234, abc1234,
	// any of them with -dirty; "dev" without git metadata.
	Version = "dev"
	// Commit is the short commit hash.
	Commit = "none"
	// Built is the UTC build time (RFC 3339).
	Built = "unknown"
)

// String is the one-line identity, in the style of `docker version`:
//
//	mg v1.2.3-4-gabc1234-dirty (commit=abc1234, built=2026-07-21T09:00:00Z)
func String(name string) string {
	return fmt.Sprintf("%s %s (commit=%s, built=%s)", name, Version, Commit, Built)
}
