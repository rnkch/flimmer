package core

import "testing"

func TestMpvTooOld(t *testing.T) {
	for v, want := range map[string]bool{
		"mpv 0.37.0":        true,
		"mpv v0.37.0-dirty": true,
		"mpv 0.38.0":        false,
		"mpv 0.40.0":        false,
		"mpv 1.0.0":         false,
		"mpv-git-abc123":    false, // no number: assume it's fine
		"":                  false,
	} {
		if got := mpvTooOld(v); got != want {
			t.Errorf("mpvTooOld(%q) = %v, want %v", v, got, want)
		}
	}
}
