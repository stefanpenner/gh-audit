// Package version reports a build identifier for the running binary,
// embedded in the audit provenance manifest so a report can be attributed
// to a specific build of gh-audit.
package version

import "runtime/debug"

// Version is overridable at build time via
// -ldflags "-X github.com/stefanpenner/gh-audit/internal/version.Version=v1.2.3".
// Empty by default; Info falls back to the VCS stamp `go build` embeds.
var Version = ""

// Info returns a non-empty build identifier: the ldflags-injected Version
// if set, otherwise the VCS revision `go build` embedded (short SHA, with a
// "-dirty" suffix when the working tree was modified), otherwise "unknown".
// It never returns the empty string, so the manifest always carries an
// attributable value.
func Info() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", ""
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					dirty = "-dirty"
				}
			}
		}
		if rev != "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			return rev + dirty
		}
	}
	return "unknown"
}
