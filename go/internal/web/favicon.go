package web

import (
	"context"
	"io/fs"
	"net/http"
	"regexp"
	"strings"

	"github.com/rnkch/flimmerkiste/internal/kiosk"
)

// defaultFaviconColor is Tokyo Night's blue: the colour of the PNG icons.
const defaultFaviconColor = "#7aa2f7"

var palette60 = regexp.MustCompile(`--wa-color-([a-z]+)-60:\s*(#[0-9a-fA-F]{6})`)

// huePalette reads the colour of every hue's tint 60 from the Tokyo Night palette
// (assets/tokyonight.css): a tab icon can't use the page's CSS variables, it needs the value.
// Tint 60 is the middle one, readable on a light and on a dark tab bar.
func huePalette(fsys fs.FS) map[string]string {
	css, _ := fs.ReadFile(fsys, "assets/tokyonight.css")
	out := map[string]string{}
	for _, m := range palette60.FindAllStringSubmatch(string(css), -1) {
		out[m[1]] = strings.ToLower(m[2])
	}
	return out
}

// kioskColor is the colour of this kiosk's name in the header (the chosen one, else from a hash
// of the name), as a hex value.
func (s *Server) kioskColor(ctx context.Context) string {
	hue := s.k.DisplayHue(ctx)
	if hue == "" {
		hue = kiosk.HashHue(s.k.DisplayName(ctx))
	}
	if c, ok := s.hues[hue]; ok {
		return c
	}
	return defaultFaviconColor
}

// faviconHref is the favicon's address for a page. The colour is in it so that a browser,
// which keeps favicons for long, fetches a new one after the colour was changed.
func (s *Server) faviconHref(ctx context.Context) string {
	return "/favicon.svg?c=" + strings.TrimPrefix(s.kioskColor(ctx), "#")
}

// favicon is the app's icon with the frame in the kiosk's colour (public: browsers fetch it
// without cookies).
func (s *Server) favicon(w http.ResponseWriter, r *http.Request) {
	svg, err := fs.ReadFile(s.pwa, "icon.svg")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-cache") // the colour can be changed
	w.Write([]byte(strings.ReplaceAll(string(svg), "currentColor", s.kioskColor(r.Context()))))
}

// faviconICO answers the /favicon.ico every browser asks for by habit (and clients that don't
// read the page's links): the PNG, which they all take at this address.
func (s *Server) faviconICO(w http.ResponseWriter, r *http.Request) {
	png, err := fs.ReadFile(s.pwa, "favicon-32.png")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(png)
}
