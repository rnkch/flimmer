package kiosk

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strconv"
)

// minMpvMinor: playlist changes use loadfile's index argument and the
// insert-at flag, which mpv 0.38 introduced. Older versions answer "invalid
// parameter" to every playlist change.
const minMpvMinor = 38

var mpvVersionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// parseMpvVersion reads mpv's "mpv-version" property ("mpv 0.40.0", "mpv v0.37.0-dirty").
func parseMpvVersion(s string) (major, minor int, ok bool) {
	m := mpvVersionRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return major, minor, true
}

// mpvTooOld reports whether the version is a release older than 0.38 (git
// builds without a version number are given the benefit of the doubt).
func mpvTooOld(version string) bool {
	major, minor, ok := parseMpvVersion(version)
	return ok && major == 0 && minor < minMpvMinor
}

// checkMpvVersion warns once mpv is up, so "invalid parameter" in the log has
// an explanation.
func (s *Service) checkMpvVersion(ctx context.Context) {
	data, err := s.player.Command(ctx, "get_property", "mpv-version")
	if err != nil {
		return
	}
	var v string
	if json.Unmarshal(data, &v) != nil {
		return
	}
	slog.Info("mpv", "version", v)
	if mpvTooOld(v) {
		s.Logf(
			ctx,
			slog.LevelError,
			"mpv is too old: playlist changes need mpv 0.38 or newer",
			"found",
			v,
		)
	}
}
