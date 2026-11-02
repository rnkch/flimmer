package core

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/rnkch/flimmerkiste/internal/store/db"
)

// SettingLocked: while "true", nothing that would interrupt what is on the
// screen is allowed (player controls, streams, restarts, the QR code, Wi-Fi
// switches, deleting or reloading the entry that is playing). Playlist edits
// and uploads still work: they don't touch what is playing.
const SettingLocked = "locked"

var ErrLocked = errors.New("playback is locked")

func (s *Service) Locked(
	ctx context.Context,
) bool {
	return s.Setting(ctx, SettingLocked) == "true"
}

// SetLocked locks or unlocks the display. Config.LockFile mirrors the state for
// deploy.sh, which refuses to restart a locked display.
func (s *Service) SetLocked(ctx context.Context, on bool) error {
	var err error
	if on {
		err = s.SetSetting(ctx, SettingLocked, "true")
	} else {
		err = s.ResetSetting(ctx, SettingLocked)
	}
	if err != nil {
		return err
	}
	s.SyncLockFile(ctx)
	if on {
		s.Logf(ctx, slog.LevelInfo, "locked")
	} else {
		s.Logf(ctx, slog.LevelInfo, "unlocked")
	}
	return nil
}

// SyncLockFile makes Config.LockFile exist exactly while playback is locked.
func (s *Service) SyncLockFile(ctx context.Context) {
	if s.cfg.LockFile == "" {
		return
	}
	var err error
	if s.Locked(ctx) {
		err = os.WriteFile(s.cfg.LockFile, []byte("locked in the web UI or with /lock\n"), 0o644)
	} else if err = os.Remove(s.cfg.LockFile); errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		slog.Warn("lock file", "err", err)
	}
}

// unlocked returns ErrLocked while playback is locked.
func (s *Service) unlocked(ctx context.Context) error {
	if s.Locked(ctx) {
		return ErrLocked
	}
	return nil
}

// unlockedFor returns ErrLocked if playback is locked and m is on the screen.
func (s *Service) unlockedFor(ctx context.Context, m db.Media) error {
	if !s.Locked(ctx) {
		return nil
	}
	if ps := s.PlayerState(); ps.Streaming == "" && ps.Path == s.MediaPath(m) {
		return ErrLocked
	}
	return nil
}
