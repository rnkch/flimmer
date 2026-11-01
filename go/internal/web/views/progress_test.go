package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rnkch/flimmerkiste/internal/store/db"
)

// Server-rendered progress bars must carry the --percentage style that
// wa-progress-bar sets on itself, or a morph strips it (see barStyle).
func TestPercent(t *testing.T) {
	for _, c := range []struct {
		pos, total float64
		want       string
	}{{141, 247.7, "56.9"}, {0, 10, "0"}, {5, 10, "50"}, {12, 10, "100"}, {-1, 10, "0"}, {3, 0, "0"}, {1, 18000, "0"}, {9, 18000, "0.1"}} {
		if got := percent(c.pos, c.total); got != c.want {
			t.Errorf("percent(%v, %v) = %q, want %q", c.pos, c.total, got, c.want)
		}
	}
}

func TestProgressBarsCarryPercentage(t *testing.T) {
	n := NowPlaying{TimePos: 141, Duration: 247.7}
	render := func(f func(*bytes.Buffer) error) string {
		var b bytes.Buffer
		if err := f(&b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	ctx := context.Background()
	for name, html := range map[string]string{
		"player": render(func(b *bytes.Buffer) error { return PlayerProgress(n).Render(ctx, b) }),
		"row":    render(func(b *bytes.Buffer) error { return RowProgress(n).Render(ctx, b) }),
		"jobs": render(func(b *bytes.Buffer) error {
			return JobList([]db.Job{{ID: 1, State: "running", Phase: "transcode", Progress: 42, OrigName: "a.mp4"}}).Render(ctx, b)
		}),
	} {
		want := map[string]string{"player": `value="56.9" style="--percentage: 56.9%;"`, "row": `value="56.9" style="--percentage: 56.9%;"`, "jobs": `value="42" style="--percentage: 42%;"`}[name]
		if !strings.Contains(html, want) {
			t.Errorf("%s: no %q in\n%s", name, want, html)
		}
	}
}

func TestPlayerProgressSeekable(t *testing.T) {
	render := func(n NowPlaying) string {
		var b bytes.Buffer
		PlayerProgress(n).Render(context.Background(), &b)
		return b.String()
	}
	html := render(NowPlaying{TimePos: 30, Duration: 600, Seekable: true})
	for _, want := range []string{`x-data="seekBar"`, `data-duration="600.0"`, `hx-post="/player/seek"`, `id="seek-pos"`} {
		if !strings.Contains(html, want) {
			t.Errorf("seekable bar misses %q:\n%s", want, html)
		}
	}
	if html := render(
		NowPlaying{TimePos: 3, Duration: 10},
	); strings.Contains(html, "seekBar") ||
		strings.Contains(html, "/player/seek") {
		t.Errorf("image bar is clickable:\n%s", html)
	}
}
