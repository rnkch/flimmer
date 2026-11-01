package kiosk

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rnkch/flimmer/internal/store/db"
)

func TestTrimJobsAndEvents(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	// Two old jobs still waiting, then 130 finished ones.
	var waiting []int64
	for i := 0; i < 2; i++ {
		j, _ := s.st.InsertJob(ctx, db.InsertJobParams{Source: "web", OrigName: "waiting"})
		waiting = append(waiting, j.ID)
	}
	for i := 0; i < 130; i++ {
		j, _ := s.st.InsertJob(ctx, db.InsertJobParams{Source: "web", OrigName: "old"})
		if i%2 == 0 {
			s.st.SetJobDone(ctx, db.SetJobDoneParams{MediaID: sql.NullInt64{}, ID: j.ID})
		} else {
			s.st.SetJobFailed(ctx, db.SetJobFailedParams{Error: "x", ID: j.ID})
		}
	}
	s.st.TrimJobs(ctx)
	all, _ := s.st.ListRecentJobs(ctx, 1000)
	if len(all) != 102 {
		t.Fatalf("%d jobs left, want 100 finished + 2 waiting", len(all))
	}
	for _, id := range waiting {
		if j, err := s.Job(ctx, id); err != nil || j.State != "queued" {
			t.Fatalf("waiting job %d removed: %v", id, err)
		}
	}

	for i := 0; i < 250; i++ {
		s.Logf(ctx, 0, "entry")
	}
	if ev, _ := s.Events(ctx, 1000); len(ev) != 200 {
		t.Fatalf("%d log entries, want the newest 200", len(ev))
	}
}

func TestSeekWithMpv(t *testing.T) {
	for _, bin := range []string{"mpv", "ffmpeg"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	s, p := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=s=320x240:d=10:r=25",
		"-pix_fmt", "yuv420p", filepath.Join(s.cfg.MediaDir, "v.mp4")).Run()
	exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "color=red:s=64x48", "-frames:v", "1",
		filepath.Join(s.cfg.MediaDir, "a.png")).Run()
	connected := make(chan struct{}, 1)
	onConnect := p.OnConnect
	p.OnConnect = func(ctx context.Context) { onConnect(ctx); connected <- struct{}{} }
	go p.Run(ctx)
	<-connected
	s.addMedia(ctx, db.InsertMediaParams{Filename: "v.mp4", Title: "v", Kind: "video"}, 0)
	img, _ := s.addMedia(ctx, db.InsertMediaParams{Filename: "a.png", Title: "a", Kind: "image", ImageDurationS: 30}, 0)

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if ok() {
				return
			}
		}
		t.Fatal("timed out waiting for " + what)
	}
	waitFor("video", func() bool { return s.PlayerState().Duration > 9 })
	if err := s.Seek(ctx, 6); err != nil {
		t.Fatal(err)
	}
	waitFor("position 6 s", func() bool {
		var pos float64
		data, _ := p.Command(ctx, "get_property", "time-pos")
		json.Unmarshal(data, &pos)
		return pos >= 5.9 && pos < 8
	})

	s.PlayIndex(ctx, int(img.Position))
	waitFor("image", func() bool { return filepath.Base(s.PlayerState().Path) == "a.png" })
	time.Sleep(200 * time.Millisecond) // duration property settles to 0 for the image
	if err := s.Seek(ctx, 3); !errors.Is(err, ErrNotSeekable) {
		t.Fatalf("seek in an image: %v", err)
	}
}

func TestDisplayName(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	if got := s.DisplayName(ctx); got != Hostname() || got == "" || strings.Contains(got, ".") {
		t.Fatalf("default %q, want the short hostname %q", got, Hostname())
	}
	if err := s.SetDisplayName(ctx, "  Foyer  "); err != nil || s.DisplayName(ctx) != "Foyer" {
		t.Fatalf("set: %v, name %q", err, s.DisplayName(ctx))
	}
	if err := s.SetDisplayName(ctx, "Kinosäle Großer Saal"); err != nil { // 20 characters, 22 bytes
		t.Fatalf("20 characters with umlauts: %v", err)
	}
	if err := s.SetDisplayName(ctx, "Kinosäle Großer Saal!"); !errors.Is(err, ErrNameTooLong) || s.DisplayName(ctx) != "Kinosäle Großer Saal" {
		t.Fatalf("21 characters: %v, name %q", err, s.DisplayName(ctx))
	}
	if err := s.SetDisplayName(ctx, " "); err != nil || s.DisplayName(ctx) != Hostname() {
		t.Fatalf("empty: %v, name %q", err, s.DisplayName(ctx))
	}
}

func TestDisplayHue(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	if s.DisplayHue(ctx) != "" {
		t.Fatal("default is not automatic")
	}
	if err := s.SetDisplayHue(ctx, "indigo"); err != nil || s.DisplayHue(ctx) != "indigo" {
		t.Fatalf("set: %v, %q", err, s.DisplayHue(ctx))
	}
	if err := s.SetDisplayHue(ctx, "magenta; background: red"); !errors.Is(err, ErrBadHue) || s.DisplayHue(ctx) != "indigo" {
		t.Fatalf("unknown color accepted: %v", err)
	}
	if err := s.SetDisplayHue(ctx, ""); err != nil || s.DisplayHue(ctx) != "" {
		t.Fatalf("back to automatic: %v, %q", err, s.DisplayHue(ctx))
	}
	if HashHue("Foyer") != HashHue("Foyer") || !slices.Contains(NameHues, HashHue("Foyer")) {
		t.Fatal("hash hue not stable or not a palette hue")
	}
}
