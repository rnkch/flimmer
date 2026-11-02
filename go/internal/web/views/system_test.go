package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rnkch/flimmerkiste/internal/kiosk"
	"github.com/rnkch/flimmerkiste/internal/sysinfo"
)

// Without a language in the context the views render German (the fallback).
func TestThrottleState(t *testing.T) {
	render := func(raw string) string {
		var b bytes.Buffer
		SystemStats(sysinfo.Info{Throttled: raw, TempC: 55}, 0).Render(context.Background(), &b)
		return b.String()
	}
	for _, c := range []struct {
		raw       string
		want, not []string
	}{
		// A Pi after installs: was warm, fine now. Nothing to worry about, nothing shown.
		{"0x80000", []string{"nicht gedrosselt"}, []string{"wa-tag", "warm"}},
		// Warm right now: one short tag, the plain-language text is its tooltip.
		{"0x80008", []string{`variant="warning">Gedrosselt</wa-tag>`, `>Läuft gerade etwas langsamer (warm)</wa-tooltip>`}, []string{"nicht gedrosselt", "Netzteil"}},
		// Weak supply now explains the throttling: only the power tag.
		{"0x50005", []string{`variant="danger">Netzteil schwach</wa-tag>`, `>Stromversorgung zu schwach – Netzteil prüfen</wa-tooltip>`}, []string{"langsamer"}},
		// Weak supply earlier stays visible.
		{"0x10000", []string{`variant="danger" appearance="outlined">Netzteil schwach</wa-tag>`, `>Stromversorgung war zu schwach – Netzteil prüfen</wa-tooltip>`, "nicht gedrosselt"}, nil},
		{"0x0", []string{"nicht gedrosselt"}, []string{"wa-tag"}},
	} {
		html := render(c.raw)
		cpu := html[strings.Index(html, "temperature-half"):strings.Index(html, `name="memory"`)]
		for _, w := range c.want {
			if !strings.Contains(cpu, w) {
				t.Errorf("%s: missing %q in\n%s", c.raw, w, cpu)
			}
		}
		for _, n := range c.not {
			if strings.Contains(cpu, n) {
				t.Errorf("%s: unexpected %q in\n%s", c.raw, n, cpu)
			}
		}
	}
}

// A link yt-dlp can't analyse still gets a preview: a warning and "play directly" only.
func TestUnprobedPreview(t *testing.T) {
	info := kiosk.UnprobedInfo(
		"https://www.arte.tv/de/videos/1/x/",
		&kiosk.ProbeError{Msg: "ERROR: No video formats found!"},
	)
	var b bytes.Buffer
	URLPreview(info).Render(context.Background(), &b)
	html := b.String()
	for _, w := range []string{"Keine Formatinformationen", "No video formats found", "Direkt abspielen", "sicheren Grenze"} {
		if !strings.Contains(html, w) {
			t.Errorf("missing %q", w)
		}
	}
	if strings.Contains(html, `"mode": "download"`) || strings.Contains(html, `name="format"`) {
		t.Error("offers a download or a format choice without format information")
	}
}
