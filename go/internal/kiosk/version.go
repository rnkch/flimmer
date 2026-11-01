package kiosk

import "runtime/debug"

// Version is the git commit the binary was built from, e.g. "e9a221e" or
// "e9a221e+dirty"; "dev" when the build has no VCS information (go run).
func Version() string {
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
