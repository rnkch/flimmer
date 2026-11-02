package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rnkch/flimmerkiste/internal/mpv"
	"github.com/rnkch/flimmerkiste/internal/store"
	"github.com/rnkch/flimmerkiste/internal/store/db"
)

func newTestService(t *testing.T) (*Service, *mpv.Player) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	media := filepath.Join(dir, "media")
	os.MkdirAll(media, 0o755)
	// unix socket paths are limited to ~104 bytes on macOS, so keep it short
	sockDir, _ := os.MkdirTemp("/tmp", "k")
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	p := &mpv.Player{Bin: "mpv", Socket: filepath.Join(sockDir, "s"),
		Args: []string{"--no-config", "--vo=null", "--ao=null", "--loop-playlist=inf"}}
	cfg := Config{MediaDir: media, TmpDir: dir, FFmpeg: "ffmpeg", FFprobe: "ffprobe"}
	return New(cfg, st, p), p
}

func addN(t *testing.T, s *Service, n int) []db.Media {
	t.Helper()
	var out []db.Media
	for i := 0; i < n; i++ {
		name := string(rune('a'+i)) + ".png"
		os.WriteFile(filepath.Join(s.cfg.MediaDir, name), []byte("x"), 0o644)
		m, err := s.addMedia(
			context.Background(),
			db.InsertMediaParams{Filename: name, Title: name, Kind: "image", ImageDurationS: 5},
			0,
		)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func titles(t *testing.T, s *Service) []string {
	list, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i, m := range list {
		if int(m.Position) != i+1 {
			t.Fatalf("positions not contiguous: %v at %d", m.Position, i)
		}
		out = append(out, m.Title)
	}
	return out
}

func TestParseCaption(t *testing.T) {
	cases := map[string]Caption{
		"05 logo 10":      {Position: 5, Title: "logo", Seconds: 10},
		"01 willkommen":   {Position: 1, Title: "willkommen"},
		"neues plakat":    {Title: "neues plakat"},
		"":                {},
		"3 kino abend 15": {Position: 3, Title: "kino abend", Seconds: 15},
		"logo 2025":       {Title: "logo", Seconds: 2025},
	}
	for in, want := range cases {
		if got := ParseCaption(in); got != want {
			t.Errorf("ParseCaption(%q) = %+v, want %+v", in, got, want)
		}
	}
}

func TestVideoFilter(t *testing.T) {
	cases := []struct {
		hdmi bool
		w, h int
		vf   string
		ok   bool
	}{
		// Composite: scale down to fit 720x576 keeping the aspect; even sizes.
		{false, 1920, 1080, "scale=720:404:flags=spline", true},
		{false, 1080, 1920, "scale=324:576:flags=spline", true}, // portrait phone video
		{false, 400, 300, "", false},                            // never scaled up
		{false, 720, 576, "", false},
		{false, 401, 301, "crop=400:300", true}, // odd: one pixel less
		// HDMI: fit 1920x1080.
		{true, 1280, 720, "", false},
		{true, 1270, 720, "", false},
		{true, 3840, 2160, "scale=1920:1080:flags=spline", true},
	}
	for _, c := range cases {
		vf, ok := videoFilter(c.hdmi, c.w, c.h)
		if vf != c.vf || ok != c.ok {
			t.Errorf(
				"videoFilter(%v,%d,%d) = %q,%v want %q,%v",
				c.hdmi,
				c.w,
				c.h,
				vf,
				ok,
				c.vf,
				c.ok,
			)
		}
	}
}

func TestPlaysAsIs(t *testing.T) {
	h264 := func(w, h int, fps float64, br int) probeInfo {
		return probeInfo{
			VideoCodec: "h264",
			PixFmt:     "yuv420p",
			Width:      w,
			Height:     h,
			FPS:        fps,
			BitRate:    br,
		}
	}
	for _, c := range []struct {
		name string
		hdmi bool
		in   probeInfo
		want bool
	}{
		{"720p25 for the CRT", false, h264(1280, 720, 25, 3_000_000), true},
		{"720p60 for the CRT", false, h264(1280, 720, 60, 5_000_000), true},
		{"1080p for the CRT: too big", false, h264(1920, 1080, 25, 5_000_000), false},
		{"1080p30 on HDMI", true, h264(1920, 1080, 29.97, 6_000_000), true},
		{"1080p60 on HDMI: too fast", true, h264(1920, 1080, 60, 6_000_000), false},
		{"bitrate too high", true, h264(1280, 720, 25, 12_000_000), false},
		{"odd size is fine", false, h264(400, 300, 25, 600_000), true},
		{"unknown rate and bitrate", false, h264(640, 480, 0, 0), true},
		{"HEVC", true, probeInfo{VideoCodec: "hevc", PixFmt: "yuv420p", Width: 1280, Height: 720, FPS: 30}, false},
		{"10 bit", true, probeInfo{VideoCodec: "h264", PixFmt: "yuv420p10le", Width: 1280, Height: 720, FPS: 30}, false},
		{"Theora", false, probeInfo{VideoCodec: "theora", PixFmt: "yuv420p", Width: 400, Height: 300, FPS: 25}, false},
	} {
		if got := playsAsIs(c.hdmi, c.in); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if r := parseRate("30000/1001"); r < 29.96 || r > 29.98 {
		t.Errorf("parseRate: %v", r)
	}
	if parseRate("0/0") != 0 || parseRate("") != 0 {
		t.Error("unknown rate not 0")
	}
}

func TestPlaylistOpsWithoutPlayer(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	m := addN(t, s, 4)
	if err := s.Move(ctx, m[3].ID, 1); err != nil {
		t.Fatal(err)
	}
	if got := titles(t, s); !slices.Equal(got, []string{"d.png", "a.png", "b.png", "c.png"}) {
		t.Fatal(got)
	}
	if err := s.MoveBy(ctx, m[3].ID, -1); err != ErrCannotMove {
		t.Fatalf("want ErrCannotMove, got %v", err)
	}
	if err := s.Move(ctx, m[0].ID, 99); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, m[1].ID); err != nil {
		t.Fatal(err)
	}
	if got := titles(t, s); !slices.Equal(got, []string{"d.png", "c.png", "a.png"}) {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.MediaDir, "b.png")); !os.IsNotExist(err) {
		t.Fatal("deleted file still on disk")
	}
	// caption position 2 inserts in the middle
	os.WriteFile(filepath.Join(s.cfg.MediaDir, "e.png"), []byte("x"), 0o644)
	s.addMedia(
		ctx,
		db.InsertMediaParams{Filename: "e.png", Title: "e.png", Kind: "image", ImageDurationS: 5},
		2,
	)
	if got := titles(t, s); !slices.Equal(got, []string{"d.png", "e.png", "c.png", "a.png"}) {
		t.Fatal(got)
	}
}

func TestHelpTextDefault(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	if !strings.Contains(s.Setting(ctx, SettingHelpText), "/slides") {
		t.Fatal("no default help text")
	}
	s.SetSetting(ctx, SettingHelpText, "eigener Text")
	s.ResetSetting(ctx, SettingHelpText)
	if s.Setting(ctx, SettingHelpText) != defaultHelpText {
		t.Fatal("reset did not restore the default")
	}
}

// TestSyncWithMpv runs a real mpv (no video/audio output) and checks that
// playlist changes reach it without restarting the current entry.
func TestSyncWithMpv(t *testing.T) {
	if _, err := exec.LookPath("mpv"); err != nil {
		t.Skip("mpv not installed")
	}
	s, p := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, n := range []string{"a", "b", "c"} {
		exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "color=red:s=64x48", "-frames:v", "1",
			filepath.Join(s.cfg.MediaDir, n+".png")).
			Run()
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

	mpvFiles := func() []string {
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
	var ms []db.Media
	for _, n := range []string{"a", "b", "c"} {
		m, err := s.addMedia(
			ctx,
			db.InsertMediaParams{Filename: n + ".png", Title: n, Kind: "image", ImageDurationS: 60},
			0,
		)
		if err != nil {
			t.Fatal(err)
		}
		ms = append(ms, m)
	}
	if got := mpvFiles(); !slices.Equal(got, []string{"a.png", "b.png", "c.png"}) {
		t.Fatal(got)
	}
	time.Sleep(300 * time.Millisecond)
	first, _ := p.Playlist(ctx)
	if !first[0].Current {
		t.Fatal("a.png should be playing")
	}

	s.Move(ctx, ms[2].ID, 1)
	s.Delete(ctx, ms[1].ID)
	if got := mpvFiles(); !slices.Equal(got, []string{"c.png", "a.png"}) {
		t.Fatal(got)
	}
	pl, _ := p.Playlist(ctx)
	if !pl[1].Current {
		t.Fatal("a.png should still be playing after reorder")
	}

	// a stream replaces the playlist; resuming restores it
	if err := s.Stream(ctx, filepath.Join(s.cfg.MediaDir, "c.png")); err != nil {
		t.Fatal(err)
	}
	if got := mpvFiles(); len(got) != 1 {
		t.Fatal(got)
	}
	if err := s.ResumePlaylist(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mpvFiles(); !slices.Equal(got, []string{"c.png", "a.png"}) {
		t.Fatal(got)
	}
}

// TestIngestVideoAndImage runs the worker on a generated clip and image,
// using libx264 because h264_v4l2m2m only exists on the Pi.
func TestIngestVideoAndImage(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	s, _ := newTestService(t)
	s.cfg.VideoEncoder = "libx264"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	vid := filepath.Join(s.cfg.TmpDir, "clip.mp4")
	img := filepath.Join(s.cfg.TmpDir, "pic.jpg")
	// MPEG-4 Part 2 doesn't play as it is: converted to H.264, size unchanged.
	exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=s=400x300:d=3:r=25", "-c:v", "mpeg4", vid).
		Run()
	exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "color=blue:s=320x240", "-frames:v", "1", img).
		Run()

	// A .mp4 that arrived as a "document" must still be detected as video.
	jv, _ := s.EnqueueFile(ctx, vid, "clip.mp4", ParseCaption("trailer"))
	ji, _ := s.EnqueueFile(ctx, img, "pic.jpg", ParseCaption("1 plakat 12"))
	go s.RunWorker(ctx)

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		a, _ := s.Job(ctx, jv.ID)
		b, _ := s.Job(ctx, ji.ID)
		if (a.State == "done" || a.State == "failed") &&
			(b.State == "done" || b.State == "failed") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, id := range []int64{jv.ID, ji.ID} {
		j, _ := s.Job(ctx, id)
		if j.State != "done" {
			t.Fatalf("job %d: %s %s", id, j.State, j.Error)
		}
	}
	list, _ := s.List(ctx)
	if len(list) != 2 || list[0].Title != "plakat" || list[0].Kind != "image" ||
		list[0].ImageDurationS != 12 ||
		list[1].Kind != "video" ||
		list[1].Width != 400 ||
		list[1].Height != 300 {
		t.Fatalf("%+v", list)
	}
	if _, err := os.Stat(vid); !os.IsNotExist(err) {
		t.Fatal("source file not cleaned up")
	}
}

func TestIngestVideoNoTranscode(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	s, _ := newTestService(t)
	s.cfg.VideoEncoder = "libx264"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// MPEG-4 Part 2 at 400x300 would normally become H.264 at 384x288.
	vid := filepath.Join(s.cfg.TmpDir, "old.avi")
	exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=s=400x300:d=2:r=25", "-c:v", "mpeg4", vid).
		Run()
	orig, _ := os.ReadFile(vid)
	j, _ := s.EnqueueFile(ctx, vid, "old.avi", Caption{NoTranscode: true})
	go s.RunWorker(ctx)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if j, _ = s.Job(ctx, j.ID); j.State == "done" || j.State == "failed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if j.State != "done" {
		t.Fatalf("job: %s %s", j.State, j.Error)
	}
	// The very file, moved: same bytes, same extension, no ffmpeg pass.
	list, _ := s.List(ctx)
	got, _ := os.ReadFile(filepath.Join(s.cfg.MediaDir, list[0].Filename))
	if !bytes.Equal(got, orig) || filepath.Ext(list[0].Filename) != ".avi" ||
		list[0].Width != 400 ||
		list[0].Height != 300 {
		t.Fatalf(
			"not kept as is: %s, %d bytes (was %d), %dx%d",
			list[0].Filename,
			len(got),
			len(orig),
			list[0].Width,
			list[0].Height,
		)
	}
}

// A deploy or restart in the middle of a conversion must not lose the upload:
// the job is queued again on the next start and finishes from the same file.
func TestIngestSurvivesRestart(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	s, _ := newTestService(t)
	s.cfg.VideoEncoder = "libx264"
	vid := filepath.Join(s.cfg.TmpDir, "long.avi")
	// MPEG-4 is always converted; two minutes take a few seconds to encode.
	exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=s=640x480:d=120:r=25", "-c:v", "mpeg4", vid).
		Run()
	j, _ := s.EnqueueFile(context.Background(), vid, "long.avi", Caption{})

	// First run: stop it as soon as the conversion has started.
	ctx1, stop := context.WithCancel(context.Background())
	done1 := make(chan struct{})
	go func() { s.RunWorker(ctx1); close(done1) }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if j, _ = s.Job(context.Background(), j.ID); j.Phase == "transcode" && j.Progress > 0 {
			break
		}
		if j.State == "done" || time.Now().After(deadline) {
			t.Fatalf(
				"conversion not caught while running: %s %s %d%%",
				j.State,
				j.Phase,
				j.Progress,
			)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	<-done1
	if _, err := os.Stat(vid); err != nil {
		t.Fatal("source deleted by the shutdown:", err)
	}

	// Next start: the job runs again and completes.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go s.RunWorker(ctx2)
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if j, _ = s.Job(ctx2, j.ID); j.State == "done" || j.State == "failed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if j.State != "done" {
		t.Fatalf("after restart: %s %s", j.State, j.Error)
	}
	if list, _ := s.List(ctx2); len(list) != 1 || list[0].Kind != "video" {
		t.Fatalf("%+v", list)
	}
	if _, err := os.Stat(vid); !os.IsNotExist(err) {
		t.Fatal("source not cleaned up after the job finished")
	}
}

// A video that plays well on the Pi is moved as it is, like "don't convert".
func TestIngestVideoThatFits(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	s, _ := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	vid := filepath.Join(s.cfg.TmpDir, "fits.mp4")
	exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=s=384x288:d=2:r=25",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-b:v", "500k", vid).Run()
	orig, _ := os.ReadFile(vid)
	j, _ := s.EnqueueFile(ctx, vid, "fits.mp4", Caption{})
	go s.RunWorker(ctx)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if j, _ = s.Job(ctx, j.ID); j.State == "done" || j.State == "failed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	list, _ := s.List(ctx)
	if j.State != "done" || len(list) != 1 {
		t.Fatalf("job: %s %s", j.State, j.Error)
	}
	got, _ := os.ReadFile(filepath.Join(s.cfg.MediaDir, list[0].Filename))
	if !bytes.Equal(got, orig) || filepath.Ext(list[0].Filename) != ".mp4" {
		t.Fatalf("not moved as is: %s", list[0].Filename)
	}
}

// TestChooseFormats uses real yt-dlp -J output (Big Buck Bunny, CC-BY,
// trimmed) to check the automatic choice for the 576-line CRT.
func TestChooseFormats(t *testing.T) {
	b, err := os.ReadFile("testdata/ytdlp-bigbuckbunny.json")
	if err != nil {
		t.Fatal(err)
	}
	var yi ytInfo
	if err := json.Unmarshal(b, &yi); err != nil {
		t.Fatal(err)
	}
	info, err := chooseFormats("https://example.invalid/v", yi)
	if err != nil {
		t.Fatal(err)
	}
	// 720p H.264 (direct download, not HLS) with AAC audio, not the "-drc" variant.
	if info.Auto != "298+140" {
		t.Fatalf("auto = %q", info.Auto)
	}
	for _, f := range info.Formats {
		if strings.Contains(f.Spec, "drc") || strings.HasPrefix(f.Spec, "sb") {
			t.Errorf("unexpected format %+v", f)
		}
		if f.Spec == info.Auto && (f.Height != 720 || f.Slow || f.Size < 150e6) {
			t.Errorf("auto format %+v", f)
		}
	}

	// Without H.264 at 576 lines or more, take the largest H.264 below.
	yi.Formats = slices.DeleteFunc(yi.Formats, func(f ytFormat) bool {
		return strings.HasPrefix(f.VCodec, "avc1") && f.Height >= 576
	})
	if info, _ = chooseFormats("u", yi); info.Auto != "135+140" {
		t.Fatalf("fallback auto = %q", info.Auto)
	}

	if _, err := chooseFormats("u", ytInfo{Type: "playlist"}); err != ErrPlaylist {
		t.Fatalf("playlist: %v", err)
	}
	// Live streams are offered for direct play (and refused for download).
	live := yi
	live.IsLive = true
	if info, err := chooseFormats("u", live); err != nil || !info.Live || info.Auto == "" {
		t.Fatalf("live: %+v %v", info, err)
	}
	if _, err := (&Service{}).EnqueueURL(
		context.Background(),
		"rtsp://cam/live",
		"b",
		"",
		Caption{},
		0,
	); err != ErrLive {
		t.Fatalf("download of rtsp: %v", err)
	}
}

// TestDownloadURL runs the whole URL path with the local yt-dlp against a
// test server: probe, enqueue, download, transcode, playlist.
func TestDownloadURL(t *testing.T) {
	for _, bin := range []string{"yt-dlp", "ffmpeg"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	s, _ := newTestService(t)
	s.cfg.VideoEncoder = "libx264"
	s.cfg.YtDlp = "yt-dlp"
	dir := t.TempDir()
	exec.Command(
		"ffmpeg",
		"-v",
		"error",
		"-y",
		"-f",
		"lavfi",
		"-i",
		"testsrc=s=640x360:d=2:r=25",
		"-f",
		"lavfi",
		"-i",
		"sine=d=2",
		"-pix_fmt",
		"yuv420p",
		"-c:a",
		"aac",
		"-shortest",
		filepath.Join(dir, "clip.mp4"),
	).Run()
	// Serve the file slowly, in chunks, so the download shows progress.
	clip, _ := os.ReadFile(filepath.Join(dir, "clip.mp4"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", strconv.Itoa(len(clip)))
		if r.Method == http.MethodHead {
			return
		}
		for i, n := 0, len(clip)/10+1; i < len(clip); i += n {
			w.Write(clip[i:min(i+n, len(clip))])
			w.(http.Flusher).Flush()
			time.Sleep(150 * time.Millisecond)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	info, err := s.ProbeURL(ctx, srv.URL+"/clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.EnqueueURL(ctx, info.URL, info.Auto, info.Title, ParseCaption("Testfilm"), 0)
	if err != nil {
		t.Fatal(err)
	}
	go s.RunWorker(ctx)
	var seen []int64 // download progress as the web UI would see it
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if j, _ = s.Job(ctx, j.ID); j.State == "done" || j.State == "failed" {
			break
		}
		if j.Phase == "download" && (len(seen) == 0 || seen[len(seen)-1] != j.Progress) {
			seen = append(seen, j.Progress)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if j.State != "done" {
		t.Fatalf("job: %s %s", j.State, j.Error)
	}
	list, _ := s.List(ctx)
	if len(list) != 1 || list[0].Title != "Testfilm" || list[0].Kind != "video" {
		t.Fatalf("%+v", list)
	}
	if len(seen) < 3 || seen[len(seen)-1] < 50 {
		t.Fatalf("download progress not reported: %v", seen)
	}
	t.Logf("download progress seen: %v", seen)
}

func TestParseProgress(t *testing.T) {
	for _, c := range []struct {
		line string
		id   string
		pct  float64
		ok   bool
	}{
		{"FLIMMERPROGRESS mp4   0.0%", "mp4", 0, true}, // as yt-dlp 2026.08 prints it
		{"FLIMMERPROGRESS 298  42.5%", "298", 42.5, true},
		{"FLIMMERPROGRESS mp4 N/A", "", 0, false}, // server sent no size
		{"[download] Destination: x.mp4", "", 0, false},
		{"download:mp4 3.0%", "", 0, false},
	} {
		id, pct, ok := parseProgress(c.line)
		if id != c.id || pct != c.pct || ok != c.ok {
			t.Errorf("parseProgress(%q) = %q, %v, %v", c.line, id, pct, ok)
		}
	}
}

func TestStreamSpec(t *testing.T) {
	if got := StreamSpec(
		URLFormat{Spec: "298+140", Height: 720},
	); got != "298+140/bv*[height<=720][fps<=?30][vcodec^=avc1]+ba/b[height<=720][fps<=?30]/bv*[height<=720]+ba/w" {
		t.Fatal(got)
	}
	// Unknown format: capped, never the plain best (1080p60 live streams were too much).
	if got := StreamSpec(URLFormat{Spec: "b"}); strings.HasSuffix(got, "/b") ||
		!strings.Contains(got, "height<=?720") || !strings.Contains(got, "fps<=?30") {
		t.Fatal(got)
	}
	for f, heavy := range map[URLFormat]bool{
		{Height: 1080, FPS: 60, Codec: "H.264"}:          true,
		{Height: 1080, FPS: 30, Codec: "H.264"}:          false,
		{Height: 720, FPS: 60, Codec: "H.264"}:           false,
		{Height: 2160, FPS: 30, Codec: "H.264"}:          true,
		{Height: 720, FPS: 30, Codec: "VP9", Slow: true}: true,
		{Height: 360, FPS: 30, Codec: "VP9", Slow: true}: false,
	} {
		if f.Heavy() != heavy {
			t.Errorf("%+v: Heavy() = %v", f, f.Heavy())
		}
	}
	if !IsDirectStream("rtsp://cam.local/live") || IsDirectStream("https://x") {
		t.Fatal("IsDirectStream")
	}
}

// serveClip serves a short test video over HTTP.
func serveClip(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=s=640x360:d=4:r=25",
		"-f", "lavfi", "-i", "sine=d=4", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", filepath.Join(dir, "clip.mp4")).
		Run(); err != nil {
		t.Skip("ffmpeg: ", err)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	return srv.URL + "/clip.mp4"
}

// TestStreamEntryWithMpv: a stream entry is played by mpv from its URL, and
// its format spec reaches mpv's ytdl-format option intact.
func TestStreamEntryWithMpv(t *testing.T) {
	for _, bin := range []string{"mpv", "ffmpeg"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	s, p := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clip := serveClip(t)
	connected := make(chan struct{}, 1)
	onConnect := p.OnConnect
	p.OnConnect = func(ctx context.Context) { onConnect(ctx); connected <- struct{}{} }
	go p.Run(ctx)
	<-connected

	m, err := s.AddStream(
		ctx,
		URLInfo{URL: clip, Title: "Clip", Duration: 4},
		URLFormat{Spec: "b"},
		"",
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetStreamFormat(ctx, m.ID, URLFormat{Spec: "18", Height: 360}); err != nil {
		t.Fatal(err)
	}
	pl, _ := p.Playlist(ctx)
	if len(pl) != 1 || pl[0].Filename != clip {
		t.Fatalf("mpv playlist %+v", pl)
	}
	var got string
	for i := 0; i < 40 && got == ""; i++ {
		time.Sleep(100 * time.Millisecond)
		if data, err := p.Command(ctx, "get_property", "ytdl-format"); err == nil {
			json.Unmarshal(data, &got)
		}
	}
	if want := StreamSpec(URLFormat{Spec: "18", Height: 360}); got != want {
		t.Fatalf("ytdl-format = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.ThumbDir, fmt.Sprintf("%d.jpg", m.ID))); err != nil {
		t.Errorf("no thumbnail: %v", err)
	}
	if err := s.Delete(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
}

// TestDownloadInstead: the downloaded copy takes the stream's place.
func TestDownloadInstead(t *testing.T) {
	for _, bin := range []string{"yt-dlp", "ffmpeg"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	s, _ := newTestService(t)
	s.cfg.VideoEncoder = "libx264"
	s.cfg.YtDlp = "yt-dlp"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addN(t, s, 2)
	st, err := s.AddStream(
		ctx,
		URLInfo{URL: serveClip(t), Title: "Clip", Duration: 4},
		URLFormat{Spec: "b"},
		"",
		2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DownloadInstead(ctx, st.ID); err != nil {
		t.Fatal(err)
	}
	go s.RunWorker(ctx)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if list, _ := s.List(ctx); len(list) == 3 && list[1].Kind == "video" {
			if list[1].Title != "Clip" || list[0].Title != "a.png" || list[2].Title != "b.png" {
				t.Fatalf("%+v", list)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	jobs, _ := s.RecentJobs(ctx, 1)
	t.Fatalf("stream not replaced; job %+v", jobs)
}

func TestEntryClockAndImageProgress(t *testing.T) {
	t0 := time.Now()
	c := entryClock{start: t0}
	if got := c.elapsed(t0.Add(3 * time.Second)); got != 3 {
		t.Fatalf("elapsed %v", got)
	}
	c.pausedAt = t0.Add(3 * time.Second) // paused at 3 s …
	if got := c.elapsed(t0.Add(10 * time.Second)); got != 3 {
		t.Fatalf("while paused %v", got)
	}
	c.paused, c.pausedAt = 7*time.Second, time.Time{} // … resumed at 10 s
	if got := c.elapsed(t0.Add(12 * time.Second)); got != 5 {
		t.Fatalf("after pause %v", got)
	}

	s := &Service{}
	img := db.Media{Kind: "image", ImageDurationS: 5}
	if pos, dur := s.Progress(img, PlayerState{Elapsed: 7}); pos != 2 || dur != 5 {
		t.Fatalf("single image repeating: %v/%v", pos, dur)
	}
	vid := db.Media{Kind: "video"}
	if pos, dur := s.Progress(
		vid,
		PlayerState{TimePos: 12, Duration: 40, Elapsed: 99},
	); pos != 12 ||
		dur != 40 {
		t.Fatalf("video uses mpv: %v/%v", pos, dur)
	}
}

// TestImageClockWithMpv: the clock starts when mpv shows an image and stands
// still while paused.
func TestImageClockWithMpv(t *testing.T) {
	if _, err := exec.LookPath("mpv"); err != nil {
		t.Skip("mpv not installed")
	}
	s, p := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "color=red:s=64x48", "-frames:v", "1",
		filepath.Join(s.cfg.MediaDir, "a.png")).
		Run()
	connected := make(chan struct{}, 1)
	onConnect := p.OnConnect
	p.OnConnect = func(ctx context.Context) { onConnect(ctx); connected <- struct{}{} }
	go p.Run(ctx)
	<-connected
	m, err := s.addMedia(
		ctx,
		db.InsertMediaParams{Filename: "a.png", Title: "a", Kind: "image", ImageDurationS: 30},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	pos, dur := s.Progress(m, s.PlayerState())
	if dur != 30 || pos < 1 || pos > 2.5 {
		t.Fatalf("after 1.5 s: %v/%v", pos, dur)
	}
	s.TogglePause(ctx)
	time.Sleep(300 * time.Millisecond)
	paused, _ := s.Progress(m, s.PlayerState())
	time.Sleep(time.Second)
	if still, _ := s.Progress(m, s.PlayerState()); still-paused > 0.05 {
		t.Fatalf("clock ran while paused: %v -> %v", paused, still)
	}
}

func TestScreenshotWithMpv(t *testing.T) {
	if _, err := exec.LookPath("mpv"); err != nil {
		t.Skip("mpv not installed")
	}
	s, p := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "color=red:s=64x48", "-frames:v", "1",
		filepath.Join(s.cfg.MediaDir, "a.png")).
		Run()
	connected := make(chan struct{}, 1)
	onConnect := p.OnConnect
	p.OnConnect = func(ctx context.Context) { onConnect(ctx); connected <- struct{}{} }
	go p.Run(ctx)
	<-connected
	if _, err := s.addMedia(
		ctx,
		db.InsertMediaParams{Filename: "a.png", Title: "a", Kind: "image", ImageDurationS: 30},
		0,
	); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	jpg, err := s.Screenshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jpg) < 100 || jpg[0] != 0xFF || jpg[1] != 0xD8 {
		t.Fatalf("not a JPEG: %d bytes", len(jpg))
	}
	if left, _ := filepath.Glob(filepath.Join(s.cfg.TmpDir, "screen-*")); len(left) > 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestResetWebPassword(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	s.SetWebPassword(ctx, "old-password")
	token, _ := s.Login(ctx, "old-password")
	pw, err := s.ResetWebPassword(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[a-hjkmnp-z2-9]{4}-[a-hjkmnp-z2-9]{4}-[a-hjkmnp-z2-9]{4}$`).
		MatchString(pw) {
		t.Fatalf("password %q", pw)
	}
	if s.SessionValid(ctx, token) {
		t.Fatal("old session still valid")
	}
	if _, err := s.Login(ctx, pw); err != nil {
		t.Fatal("new password does not log in:", err)
	}
	if other, _ := s.ResetWebPassword(ctx); other == pw {
		t.Fatal("same password twice")
	}
}

// The automatic choice never lands on a format the Pi can't play: only 1080p60 and 4K on offer
// means the smallest of them, and with a lighter one around that one wins.
func TestChooseFormatsAvoidsHeavy(t *testing.T) {
	f := func(id string, h int, fps float64) ytFormat {
		return ytFormat{ID: id, VCodec: "avc1.640028", ACodec: "mp4a.40.2", Height: h, FPS: fps}
	}
	info, err := chooseFormats(
		"https://x",
		ytInfo{Formats: []ytFormat{f("a", 1080, 60), f("b", 2160, 30)}},
	)
	if err != nil || info.Auto != "a" {
		t.Fatalf("auto = %q, %v", info.Auto, err)
	}
	info, _ = chooseFormats("https://x", ytInfo{
		Formats: []ytFormat{f("a", 1080, 60), f("b", 1080, 30), f("c", 720, 60)},
	})
	if info.Auto != "c" && info.Auto != "b" || info.Auto == "a" {
		t.Fatalf("auto = %q, want a light format", info.Auto)
	}
}

// Subtitles cycle through the tracks of the playing video and then switch off.
func TestCycleSubtitlesWithMpv(t *testing.T) {
	if _, err := exec.LookPath("mpv"); err != nil {
		t.Skip("mpv not installed")
	}
	s, p := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srt := filepath.Join(s.cfg.TmpDir, "a.srt")
	os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:09,000\nHallo\n"), 0o644)
	if out, err := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=s=160x120:d=10:r=10",
		"-i", srt, "-i", srt, "-map", "0", "-map", "1", "-map", "2", "-c:v", "libx264", "-c:s", "srt",
		"-metadata:s:s:0", "language=deu", "-metadata:s:s:1", "language=eng",
		filepath.Join(s.cfg.MediaDir, "v.mkv")).
		CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	connected := make(chan struct{}, 1)
	onConnect := p.OnConnect
	p.OnConnect = func(ctx context.Context) { onConnect(ctx); connected <- struct{}{} }
	go p.Run(ctx)
	<-connected
	if _, err := s.addMedia(ctx,
		db.InsertMediaParams{Filename: "v.mkv", Title: "v", Kind: "video"}, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if n := s.SubtitleTracks(ctx); n != 2 {
		t.Fatalf("%d subtitle tracks, want 2", n)
	}
	var got []string
	for range 3 {
		name, err := s.CycleSubtitles(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	t.Logf("cycle: %q", got)
	// mpv starts with the first track on, so three steps go: second, off, first.
	if got[0] == got[1] || got[1] != "" && got[2] == got[1] {
		t.Fatalf("cycle: %q", got)
	}
}

// A series page is a playlist for yt-dlp; the first video is added, with a note.
func TestProbeURLPlaylistTakesFirstVideo(t *testing.T) {
	s, _ := newTestService(t)
	fake := filepath.Join(t.TempDir(), "yt-dlp")
	os.WriteFile(fake, []byte(`#!/bin/sh
case "$*" in
*--flat-playlist*) echo '{"_type":"playlist","title":"Series","entries":[{"url":"https://x.test/v/1"},{"url":"https://x.test/v/2"}]}' ;;
*https://x.test/v/1) echo '{"title":"First","webpage_url":"https://x.test/v/1","duration":60,"formats":[{"format_id":"v","vcodec":"avc1.4d","acodec":"mp4a.40.2","height":720,"fps":25}]}' ;;
*) echo '{"_type":"playlist","title":"Series","entries":[{},{}]}' ;;
esac
`), 0o755)
	s.cfg.YtDlp = fake
	info, err := s.ProbeURL(context.Background(), "https://x.test/series")
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "First" || info.URL != "https://x.test/v/1" || info.PlaylistCount != 2 ||
		len(info.Formats) != 1 {
		t.Fatalf("%+v", info)
	}
}
