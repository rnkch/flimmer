package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLock(t *testing.T) {
	s, _ := newTestService(t)
	s.cfg.LockFile = filepath.Join(t.TempDir(), "locked")
	ctx := context.Background()
	ms := addN(t, s, 3)
	// mpv isn't running: pretend the first entry is on the screen.
	s.stateMu.Lock()
	s.state.Path = s.MediaPath(ms[0])
	s.stateMu.Unlock()

	if err := s.SetLocked(ctx, true); err != nil || !s.Locked(ctx) {
		t.Fatalf("lock: %v", err)
	}
	if _, err := os.Stat(s.cfg.LockFile); err != nil {
		t.Fatalf("no lock file for deploy.sh: %v", err)
	}
	for name, err := range map[string]error{
		"next":            s.Next(ctx),
		"prev":            s.Prev(ctx),
		"pause":           s.TogglePause(ctx),
		"seek":            s.Seek(ctx, 1),
		"stats":           s.ToggleStats(ctx),
		"play entry":      s.PlayIndex(ctx, 2),
		"stream":          s.Stream(ctx, "rtsp://example.invalid/x"),
		"resume":          s.ResumePlaylist(ctx),
		"restart player":  s.RestartPlayer(ctx),
		"restart service": s.RestartService(ctx),
		"wifi":            s.ConnectWifi(ctx, "Kino"),
		"delete playing":  s.Delete(ctx, ms[0].ID),
	} {
		if !errors.Is(err, ErrLocked) {
			t.Errorf("%s while locked: %v", name, err)
		}
	}
	if _, err := s.ToggleQR(ctx, time.Second); !errors.Is(err, ErrLocked) {
		t.Errorf("QR while locked: %v", err)
	}
	// Editing the playlist doesn't touch what's playing.
	if err := s.Rename(ctx, ms[0].ID, "Plakat"); err != nil {
		t.Errorf("rename while locked: %v", err)
	}
	if err := s.Move(ctx, ms[2].ID, 1); err != nil {
		t.Errorf("move while locked: %v", err)
	}
	if err := s.Delete(ctx, ms[1].ID); err != nil {
		t.Errorf("delete another entry while locked: %v", err)
	}

	if err := s.SetLocked(ctx, false); err != nil || s.Locked(ctx) {
		t.Fatalf("unlock: %v", err)
	}
	if _, err := os.Stat(s.cfg.LockFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock file left after unlocking: %v", err)
	}
	if err := s.Delete(ctx, ms[0].ID); err != nil {
		t.Errorf("delete after unlocking: %v", err)
	}
}
