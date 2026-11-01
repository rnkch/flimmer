package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rnkch/flimmer/internal/kiosk"
)

// Without a language in the context the views render German (the fallback).
func TestUpgradeCard(t *testing.T) {
	render := func(info kiosk.UpgradeInfo, msg string) string {
		var b bytes.Buffer
		if err := UpgradeCard(info, msg).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	for _, c := range []struct {
		name      string
		info      kiosk.UpgradeInfo
		msg       string
		want, not []string
	}{
		{"newer release", kiosk.UpgradeInfo{Current: "v0.1.0", Release: true, Latest: "v0.2.0", URL: "https://github.com/o/r/releases/tag/v0.2.0", Newer: true, CanInstall: true},
			"", []string{"v0.2.0 ist verfügbar", `class="disturbs"`, `hx-post="/system/upgrade"`, "v0.2.0 installieren", "hx-confirm="}, []string{"Aktuell", "Entwicklungsversion"}},
		{"up to date", kiosk.UpgradeInfo{Current: "v0.2.0", Release: true, Latest: "v0.2.0"},
			"", []string{"Aktuell", "Erneut prüfen"}, []string{"installieren", `hx-post="/system/upgrade"`}},
		{"commit build gets the release offered", kiosk.UpgradeInfo{Current: "b7edd6f", Latest: "v0.1.0", Newer: true, CanInstall: true},
			"", []string{"Entwicklungsversion", "v0.1.0 installieren"}, nil},
		{"newer, but nothing for this device", kiosk.UpgradeInfo{Current: "v0.1.0", Release: true, Latest: "v0.2.0", Newer: true},
			"", []string{"v0.2.0 ist verfügbar"}, []string{"installieren"}},
		{"check failed", kiosk.UpgradeInfo{Current: "v0.1.0"},
			"Es wurde noch keine Version veröffentlicht", []string{"noch keine Version", "Erneut prüfen"}, []string{"installieren", "Aktuell"}},
		// The link comes from GitHub's answer: scripts must not get through.
		{"hostile link", kiosk.UpgradeInfo{Current: "v0.1.0", Release: true, Latest: "v0.2.0", URL: "javascript:alert(1)", Newer: true},
			"", nil, []string{"javascript:alert"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			html := render(c.info, c.msg)
			for _, w := range c.want {
				if !strings.Contains(html, w) {
					t.Errorf("missing %q in\n%s", w, html)
				}
			}
			for _, n := range c.not {
				if strings.Contains(html, n) {
					t.Errorf("unexpected %q in\n%s", n, html)
				}
			}
		})
	}
}
