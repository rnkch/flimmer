package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func waitJob(
	t *testing.T,
	s *Service,
	id int64,
	until func(state, phase string, progress int64) bool,
) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := s.Job(context.Background(), id)
		if until(j.State, j.Phase, j.Progress) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, _ := s.Job(context.Background(), id)
	t.Fatalf("job %d stuck: %s %s %d%% %s", id, j.State, j.Phase, j.Progress, j.Error)
}

func TestCancelQueuedJob(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	src := filepath.Join(s.cfg.TmpDir, "web-queued.jpg")
	os.WriteFile(src, []byte("x"), 0o644)
	j, _ := s.EnqueueFile(ctx, src, "a.jpg", Caption{})
	if err := s.CancelJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if j, _ = s.Job(ctx, j.ID); j.State != "cancelled" {
		t.Fatalf("state %s", j.State)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("file not deleted")
	}
	if err := s.CancelJob(ctx, j.ID); !errors.Is(err, ErrJobFinished) {
		t.Fatalf("second cancel: %v", err)
	}
}

// Cancelling a running conversion stops ffmpeg, deletes source and output,
// and the worker goes on with the next job.
func TestCancelRunningJob(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	s, _ := newTestService(t)
	s.cfg.VideoEncoder = "libx264"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	vid := filepath.Join(s.cfg.TmpDir, "web-long.avi")
	exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=s=640x480:d=120:r=25", "-c:v", "mpeg4", vid).
		Run()
	img := filepath.Join(s.cfg.TmpDir, "web-next.jpg")
	exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "color=blue:s=64x48", "-frames:v", "1", img).
		Run()
	long, _ := s.EnqueueFile(ctx, vid, "long.avi", Caption{})
	next, _ := s.EnqueueFile(ctx, img, "next.jpg", Caption{})
	go s.RunWorker(ctx)

	waitJob(
		t,
		s,
		long.ID,
		func(state, phase string, p int64) bool { return phase == "transcode" && p > 0 },
	)
	if err := s.CancelJob(ctx, long.ID); err != nil {
		t.Fatal(err)
	}
	waitJob(t, s, next.ID, func(state, _ string, _ int64) bool { return state == "done" })
	if j, _ := s.Job(ctx, long.ID); j.State != "cancelled" {
		t.Fatalf("cancelled job ended as %s %s", j.State, j.Error)
	}
	if _, err := os.Stat(vid); !os.IsNotExist(err) {
		t.Fatal("source not deleted")
	}
	if out, _ := filepath.Glob(filepath.Join(s.cfg.TmpDir, "out-*")); len(out) > 0 {
		t.Fatalf("conversion output left: %v", out)
	}
	if list, _ := s.List(ctx); len(list) != 1 || list[0].Kind != "image" {
		t.Fatalf("playlist: %+v", list)
	}
}

func TestCleanTmp(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	old := time.Now().Add(-time.Hour)
	file := func(name string, age time.Time) string {
		p := filepath.Join(s.cfg.TmpDir, name)
		os.WriteFile(p, []byte("x"), 0o644)
		os.Chtimes(p, age, age)
		return p
	}
	active := file("web-active.mp4", old)
	j, _ := s.EnqueueFile(ctx, active, "active.mp4", Caption{})
	keep := []string{
		active,
		file(fmt.Sprintf("url-%d.mp4.part", j.ID), old), // partial download of a waiting job
		file("web-arriving.mp4", time.Now()),            // upload in progress
		file("notes.txt", old),                          // not ours
	}
	gone := []string{
		file("web-orphan.mp4", old),
		file("out-video-abcd.mkv", old),
		file("url-999.mp4.part", old),
		file("screen-1.png", old),
	}
	s.cleanTmp(ctx)
	for _, p := range keep {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("deleted %s", filepath.Base(p))
		}
	}
	for _, p := range gone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("kept %s", filepath.Base(p))
		}
	}
}

// Children die with the command (the standalone yt-dlp runs as two processes).
func TestKillGroupOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	marker := "31.4159"
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep "+marker+" & wait")
	killGroupOnCancel(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	cmd.Wait()
	time.Sleep(200 * time.Millisecond)
	if out, _ := exec.Command("pgrep", "-f", "sleep "+marker).Output(); len(out) > 0 {
		exec.Command("pkill", "-f", "sleep "+marker).Run()
		t.Fatalf("child still running: %s", out)
	}
}

// A helper killed by SIGTERM during the restart is not a failed restart.
func TestStoppedByRestart(t *testing.T) {
	err := exec.Command("sh", "-c", "kill -TERM $$").Run()
	if !stoppedByRestart(err) {
		t.Fatalf("SIGTERM not recognised: %v", err)
	}
	if stoppedByRestart(exec.Command("sh", "-c", "exit 1").Run()) {
		t.Fatal("a real failure (exit 1) counted as restart")
	}
	if stoppedByRestart(exec.Command("no-such-binary-for-test").Run()) {
		t.Fatal("a missing binary counted as restart")
	}
}
