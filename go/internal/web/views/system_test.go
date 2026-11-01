package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

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
		// Warm right now: one plain-language tag.
		{"0x80008", []string{`variant="warning">Läuft gerade etwas langsamer (warm)</wa-tag>`}, []string{"nicht gedrosselt", "Netzteil"}},
		// Weak supply now explains the throttling: only the power tag.
		{"0x50005", []string{`variant="danger">Stromversorgung zu schwach – Netzteil prüfen</wa-tag>`}, []string{"langsamer"}},
		// Weak supply earlier stays visible.
		{"0x10000", []string{`variant="danger" appearance="outlined">Stromversorgung war zu schwach – Netzteil prüfen</wa-tag>`, "nicht gedrosselt"}, nil},
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
