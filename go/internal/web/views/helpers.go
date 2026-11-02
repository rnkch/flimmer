// Package views holds the templ components of the web UI.
package views

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/rnkch/flimmerkiste/internal/kiosk"
	"github.com/rnkch/flimmerkiste/internal/store/db"
	"github.com/rnkch/flimmerkiste/internal/web/i18n"
)

// t translates an English UI string for the request's language.
func t(ctx context.Context, key string, args ...any) string { return i18n.T(ctx, key, args...) }

// jsStrings is the translation table for assets/*.js, as JSON.
func jsStrings(ctx context.Context) string {
	m := map[string]string{}
	for _, k := range i18n.JS {
		m[k] = i18n.T(ctx, k)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// languages for the switch in the top bar, each named in its own language.
var languages = []struct{ Code, Name string }{
	{"de", "Deutsch"},
	{"en", "English"},
}

// Page is what every full page needs for the layout.
type Page struct {
	Title   string
	Active  string // nav key
	JS      string // bundle file names
	CSS     string
	Now     NowPlaying
	Name    string // display name in the header (setting, or the hostname)
	NameSet string // the stored name, "" while the hostname is shown
	Locked  bool   // playback locked (kiosk.SettingLocked)
	Hue     string // its chosen color, "" = from a hash of the name
	Favicon string // the favicon's address (in the display's color)
}

type NowPlaying struct {
	Idle      bool
	Title     string
	Position  int64 // 1-based playlist position, 0 when unknown
	Paused    bool
	Streaming string
	TimePos   float64
	Duration  float64
	Seekable  bool // a video or a stream with a length; not images or live streams
}

func Dur(sec float64) string {
	s := int(sec)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func id(m db.Media) string { return strconv.FormatInt(m.ID, 10) }

func mediaURL(m db.Media, action string) string {
	return "/media/" + id(m) + action
}

// imageURL is for the cached thumbnail/preview images: SQLite may give a
// deleted entry's id to the next one, so the URL carries the entry's
// creation time and a browser never shows the old picture.
func imageURL(m db.Media, action string) string {
	return mediaURL(m, action) + "?v=" + url.QueryEscape(m.CreatedAt)
}

func itoa[T ~int | ~int64](v T) string { return strconv.FormatInt(int64(v), 10) }

// streamSource is shown for stream entries: host and chosen height.
func streamSource(m db.Media) string {
	src := m.URL
	if u, err := url.Parse(m.URL); err == nil && u.Host != "" {
		src = strings.TrimPrefix(u.Host, "www.")
	}
	if m.Height > 0 {
		src += fmt.Sprintf(" · %dp", m.Height)
	}
	return src
}

// downloadable: a stream that has an end and can be fetched with yt-dlp.
func downloadable(m db.Media) bool {
	return m.Kind == "stream" && m.DurationS > 0 && !kiosk.IsDirectStream(m.URL)
}

// assetVersion is the content hash in the bundle's file name ("app.3f2a9c1d.js"). It
// versions the icon URLs (app.js), so icons can be cached for good and a new release
// still brings new ones.
func assetVersion(js string) string {
	return strings.TrimSuffix(strings.TrimPrefix(js, "app."), ".js")
}

// pageLink makes a link a partial navigation: htmx fetches the page and swaps only
// <main id="page"> (the header with its live connection, the icon cache and the
// components stay), updates the title and the address, and refreshes the tabs
// (#topnav) for the new active page. The href stays, so middle-click, no JavaScript
// and screen readers work as before. The trigger is for <wa-button href>: its anchor
// lives in a shadow root, where htmx can't see it to cancel the native navigation, so
// "prevent" cancels it (after the filter: a click with a modifier key, which asks for a
// new tab or window, is left to the browser). The filter has no spaces: it sits inside
// a trigger specification.
func pageLink(href string) templ.Attributes {
	return templ.Attributes{
		"hx-get":        href,
		"hx-push-url":   "true",
		"hx-target":     "#page",
		"hx-select":     "#page",
		"hx-select-oob": "#topnav:outerMorph",
		"hx-swap":       "outerHTML show:top",
		"hx-trigger":    "click[button===0&&!ctrlKey&&!metaKey&&!shiftKey&&!altKey] prevent",
	}
}

// nav labels are translation keys.
var nav = []struct{ Key, Href, Icon, Label string }{
	{"playlist", "/playlist", "list", "Playlist"},
	{"add", "/add", "plus", "Add"},
	{"system", "/system", "server", "System"},
	{"settings", "/settings", "gear", "Settings"},
}

func jobVariant(state string) string {
	switch state {
	case "done":
		return "success"
	case "failed":
		return "danger"
	case "running":
		return "brand"
	case "cancelled":
		return "warning"
	}
	return "neutral"
}

// jobLabel values are translation keys.
var jobLabel = map[string]string{
	"queued":    "Queued",
	"running":   "Running",
	"done":      "Done",
	"failed":    "Failed",
	"cancelled": "Cancelled",
}

// jobStateLabel names the state, and for running jobs the phase.
func jobStateLabel(j db.Job) string {
	if j.State == "running" {
		switch j.Phase {
		case "download":
			return "Downloading"
		case "transcode":
			return "Transcoding"
		}
	}
	return jobLabel[j.State]
}

// jobName: the file name, or for URL jobs the title (or the URL itself).
func jobName(j db.Job) string {
	switch {
	case j.OrigName != "":
		return j.OrigName
	case j.Title != "":
		return j.Title
	}
	return j.URL
}

// disabledCount is how many entries are switched off.
func disabledCount(list []db.Media) (n int) {
	for _, m := range list {
		if !kiosk.Enabled(m) {
			n++
		}
	}
	return n
}

// loopSeconds is how long one pass through the playlist takes (disabled
// entries aren't played).
func loopSeconds(list []db.Media) float64 {
	var s float64
	for _, m := range list {
		if !kiosk.Enabled(m) {
			continue
		}
		switch m.Kind {
		case "video", "stream": // live streams (no duration) add nothing
			s += m.DurationS
		default:
			s += float64(m.ImageDurationS)
		}
	}
	return s
}

// percent is playback progress with one decimal, so the bar moves on every
// one-second tick even for long entries (1 % of a 5 h film is 3 minutes).
// Formatted like wa-progress-bar formats its own --percentage ("56.9", "56").
func percent(pos, total float64) string {
	if total <= 0 {
		return "0"
	}
	v := math.Round(min(max(pos/total*100, 0), 100)*10) / 10
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// shortTime turns a stored UTC timestamp into local time, e.g. "02.01. 15:04:05".
func shortTime(ctx context.Context, ts string) string {
	tm, err := time.Parse("2006-01-02T15:04:05Z", ts)
	if err != nil {
		return ts
	}
	if i18n.Lang(ctx) == "en" {
		return tm.Local().Format("Jan 2 15:04:05")
	}
	return tm.Local().Format("02.01. 15:04:05")
}

func uptime(ctx context.Context, d time.Duration) string {
	days := int(d.Hours()) / 24
	if days > 0 {
		return t(ctx, "%d d %d h", days, int(d.Hours())%24)
	}
	return t(ctx, "%d h %d min", int(d.Hours()), int(d.Minutes())%60)
}

// vals builds an hx-vals JSON object from key/value pairs.
func vals(kv ...string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// signalClass grades a network's signal for the Wi-Fi icon.
func signalClass(n kiosk.WifiNetwork) string {
	switch {
	case !n.InRange:
		return "signal none"
	case n.Signal >= 60:
		return "signal good"
	case n.Signal >= 35:
		return "signal fair"
	}
	return "signal weak"
}

func signalLabel(ctx context.Context, n kiosk.WifiNetwork) string {
	if !n.InRange {
		return t(ctx, "not in range")
	}
	return t(ctx, "Signal %d %%", n.Signal)
}

// barStyle is the inline style wa-progress-bar sets on itself for value v
// (plus extra declarations). Server-rendered bars must carry it: a morph
// makes the element's attributes match the new HTML, so without it every
// refresh strips the style and the bar stays empty (or full) until the
// value changes.
func barStyle(v string, extra string) templ.SafeCSS {
	return templ.SafeCSS("--percentage: " + v + "%;" + extra)
}

// seekAttrs makes the player progress clickable (Alpine seekBar, seek.js).
func seekAttrs(n NowPlaying) templ.Attributes {
	if !n.Seekable || n.Duration <= 0 {
		return templ.Attributes{}
	}
	return templ.Attributes{
		"x-data":        "seekBar",
		"data-duration": strconv.FormatFloat(n.Duration, 'f', 1, 64),
	}
}

// hostname and maxNameLen: placeholder and limit of the display name editor.
func hostname() string { return kiosk.Hostname() }

const maxNameLen = kiosk.DisplayNameMax

// nameHue is the color of the header name: chosen, or from the name's hash.
func nameHue(name, chosen string) string {
	if chosen != "" {
		return chosen
	}
	return kiosk.HashHue(name)
}

func hueColor(hue string) templ.SafeCSS {
	return templ.SafeCSS("color: var(--kiosk-hue-" + hue + ");")
}

func hueSwatch(hue string) templ.SafeCSS {
	return templ.SafeCSS("background: var(--kiosk-hue-" + hue + ");")
}

// hueLabels are translation keys for the color dots' tooltips.
var hueLabels = map[string]string{
	"red":    "Red",
	"orange": "Orange",
	"yellow": "Yellow",
	"green":  "Green",
	"cyan":   "Cyan",
	"blue":   "Blue",
	"indigo": "Indigo",
	"purple": "Purple",
	"pink":   "Pink",
}

var nameHueList = kiosk.NameHues

func boolStr(b bool) string { return strconv.FormatBool(b) }
