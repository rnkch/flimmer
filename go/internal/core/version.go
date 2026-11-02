package core

import "runtime/debug"

// releaseVersion is set when a release is built (-ldflags "-X
// github.com/rnkch/flimmerkiste/internal/core.releaseVersion=v1.2.3").
var releaseVersion string

// Version is the release the binary was built as ("v1.2.3"), else the git
// commit it was built from, e.g. "e9a221e" or "e9a221e+dirty"; "dev" when the
// build has no VCS information (go run).
func Version() string {
	if releaseVersion != "" {
		return releaseVersion
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev, dirty string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	if rev == "" {
		return "dev"
	}
	return rev[:min(7, len(rev))] + dirty
}
