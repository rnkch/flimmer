package core

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rnkch/flimmerkiste/internal/store/db"
)

// A disabled entry stays in the database (title, file, position) but mpv
// doesn't get it: it isn't played, and "show now" maps to mpv's shorter list.
func TestDisabledEntriesWithMpv(t *testing.T) {
	if _, err := exec.LookPath("mpv"); err != nil {
		t.Skip("mpv not installed")
	}
	s, p := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, n := range []string{"a", "b", "c", "d"} {
		exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "color=red:s=64x48",
			"-frames:v", "1", filepath.Join(s.cfg.MediaDir, n+".png")).Run()
	}
	connected := make(chan struct{}, 1)
	onConnect := p.OnConnect
	p.OnConnect = func(ctx context.Context) { onConnect(ctx); connected <- struct{}{} }
	go p.Run(ctx)
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("mpv did not start")
	}

	files := func() []string {
		pl, err := p.Playlist(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range pl {
			out = append(out, filepath.Base(e.Filename))
		}
		return out
	}
	playing := func() string {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
			pl, _ := p.Playlist(ctx)
			for _, e := range pl {
				if e.Current {
					return filepath.Base(e.Filename)
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		return ""
	}
	want := func(what string, expect ...string) {
		t.Helper()
		if got := files(); !slices.Equal(got, expect) {
			t.Fatalf("%s: mpv has %v, want %v", what, got, expect)
		}
	}
	ms := map[string]db.Media{}
	for _, n := range []string{"a", "b", "c", "d"} {
		m, err := s.addMedia(ctx, db.InsertMediaParams{
			Filename: n + ".png", Title: n, Kind: "image", ImageDurationS: 60,
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		ms[n] = m
	}
	want("start", "a.png", "b.png", "c.png", "d.png")

	// Off: gone from mpv, still in the database, in place.
	if err := s.SetEnabled(ctx, ms["b"].ID, false); err != nil {
		t.Fatal(err)
	}
	want("b disabled", "a.png", "c.png", "d.png")
	list, _ := s.List(ctx)
	if len(list) != 4 || list[1].Title != "b" || Enabled(list[1]) || !Enabled(list[0]) {
		t.Fatalf("database after disabling b: %+v", list)
	}
	if err := s.SetEnabled(ctx, ms["b"].ID, false); err != nil { // nothing to do
		t.Fatal(err)
	}

	// "Show now": position 4 is mpv's third entry; a disabled one can't be shown.
	if err := s.PlayIndex(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if got := playing(); got != "d.png" {
		t.Fatalf("show position 4: %q is playing", got)
	}
	if err := s.PlayIndex(ctx, 2); !errors.Is(err, ErrDisabled) {
		t.Fatalf("show a disabled entry: %v", err)
	}
	if err := s.PlayIndex(ctx, 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("show a position that doesn't exist: %v", err)
	}

	// Reordering with a disabled entry in between keeps the order of the rest.
	if err := s.Move(ctx, ms["d"].ID, 1); err != nil {
		t.Fatal(err)
	}
	want("d moved to the top", "d.png", "a.png", "c.png")

	// Locked: other entries can be switched, the one on the screen can't be turned off.
	s.SetLocked(ctx, true)
	if err := s.SetEnabled(ctx, ms["c"].ID, false); err != nil {
		t.Fatalf("disable another entry while locked: %v", err)
	}
	want("c disabled while locked", "d.png", "a.png")
	if err := s.SetEnabled(ctx, ms["d"].ID, false); !errors.Is(err, ErrLocked) {
		t.Fatalf("disable the playing entry while locked: %v", err)
	}
	want("d still there", "d.png", "a.png")
	if err := s.SetEnabled(ctx, ms["b"].ID, true); err != nil {
		t.Fatalf("enable while locked: %v", err)
	}
	want("b enabled while locked", "d.png", "a.png", "b.png")
	s.SetLocked(ctx, false)

	// Turning off the playing entry moves on to the next one.
	if err := s.SetEnabled(ctx, ms["d"].ID, false); err != nil {
		t.Fatal(err)
	}
	want("d disabled", "a.png", "b.png")
	if got := playing(); got != "a.png" {
		t.Fatalf("after turning off the playing entry: %q", got)
	}

	// Everything on again: database order, nothing lost.
	for _, n := range []string{"c", "d"} {
		if err := s.SetEnabled(ctx, ms[n].ID, true); err != nil {
			t.Fatal(err)
		}
	}
	want("all enabled", "d.png", "a.png", "b.png", "c.png")
}

// Nothing enabled: mpv gets an empty playlist and idles; the entries are all still there.
func TestAllDisabled(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	ms := addN(t, s, 2)
	for _, m := range ms {
		if err := s.SetEnabled(ctx, m.ID, false); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := s.List(ctx)
	if len(list) != 2 || Enabled(list[0]) || Enabled(list[1]) {
		t.Fatalf("%+v", list)
	}
	if err := s.PlayIndex(ctx, 1); !errors.Is(err, ErrDisabled) {
		t.Fatalf("got %v", err)
	}
}
