// Package buildinfo exposes build metadata injected via -ldflags.
package buildinfo

// Version and GitCommit are overridden at build time with
// -ldflags "-X .../buildinfo.Version=... -X .../buildinfo.GitCommit=...".
var (
	Version   = "dev"
	GitCommit = "unknown"
)

// String returns "<Version> (<GitCommit>)".
func String() string {
	return Version + " (" + GitCommit + ")"
}
