package kiosk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/rnkch/flimmer/internal/mpv"
	"github.com/rnkch/flimmer/internal/store/db"
)

func (s *Service) List(ctx context.Context) ([]db.Media, error) { return s.st.ListMedia(ctx) }

func (s *Service) Get(ctx context.Context, id int64) (db.Media, error) { return s.st.GetMedia(ctx, id) }

// MediaPath is what mpv plays: the file, or for streams the URL.
func (s *Service) MediaPath(m db.Media) string {
	if m.Kind == "stream" {
		return m.URL
	}
	return filepath.Join(s.cfg.MediaDir, m.Filename)
}

// reorder writes positions 1..n for ids in the given order.
func reorder(ctx context.Context, q *db.Queries, ids []int64) error {
	for i, id := range ids {
		if err := q.SetMediaPosition(ctx, db.SetMediaPositionParams{Position: int64(i + 1), ID: id}); err != nil {
			return err
		}
	}
	return nil
}

func ids(list []db.Media) []int64 {
	out := make([]int64, len(list))
	for i, m := range list {
		out[i] = m.ID
	}
	return out
}

// changed runs after every playlist mutation: mpv, subscribers.
// Caller holds plMu.
func (s *Service) changed(ctx context.Context) error {
	s.Hub.Publish(Event{Type: EventPlaylist})
	return s.sync(ctx)
}

// Move puts media id at 1-based position pos (clamped to the list).
func (s *Service) Move(ctx context.Context, id int64, pos int) error {
	s.plMu.Lock()
	defer s.plMu.Unlock()
	err := s.st.Tx(ctx, func(q *db.Queries) error {
		list, err := q.ListMedia(ctx)
		if err != nil {
			return err
		}
		order := ids(list)
		i := slices.Index(order, id)
		if i < 0 {
			return ErrNotFound
		}
		order = slices.Delete(order, i, i+1)
		pos = min(max(pos, 1), len(order)+1)
		order = slices.Insert(order, pos-1, id)
		return reorder(ctx, q, order)
	})
	if err != nil {
		return err
	}
	return s.changed(ctx)
}

// MoveBy shifts media id by delta places (Up = -1, Down = +1).
func (s *Service) MoveBy(ctx context.Context, id int64, delta int) error {
	m, err := s.st.GetMedia(ctx, id)
	if err != nil {
		return err
	}
	n, err := s.st.CountMedia(ctx)
	if err != nil {
		return err
	}
	to := int(m.Position) + delta
	if to < 1 || to > int(n) {
		return ErrCannotMove
	}
	return s.Move(ctx, id, to)
}

var (
	ErrCannotMove  = errors.New("cannot move further")
	ErrNotFound    = errors.New("not found")
	ErrEmptyTitle  = errors.New("the title must not be empty")
	ErrBadDuration = errors.New("the display time must be a positive number")
)

// Delete removes media id from the playlist and its file from disk.
func (s *Service) Delete(ctx context.Context, id int64) error {
	m, err := s.st.GetMedia(ctx, id)
	if err != nil {
		return err
	}
	if err := s.unlockedFor(ctx, m); err != nil {
		return err
	}
	s.plMu.Lock()
	defer s.plMu.Unlock()
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if err := q.DeleteMedia(ctx, id); err != nil {
			return err
		}
		list, err := q.ListMedia(ctx)
		if err != nil {
			return err
		}
		return reorder(ctx, q, ids(list))
	})
	if err != nil {
		return err
	}
	if err := s.changed(ctx); err != nil {
		slog.Error("sync after delete", "err", err)
	}
	// Remove the file after mpv has dropped it from its playlist.
	if m.Kind != "stream" {
		if err := os.Remove(s.MediaPath(m)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	s.dropThumbnail(m.ID)
	s.Logf(ctx, slog.LevelInfo, "deleted", "title", m.Title)
	return nil
}

// Rename only changes the display title; the file on disk keeps its name.
func (s *Service) Rename(ctx context.Context, id int64, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return ErrEmptyTitle
	}
	if err := s.st.SetMediaTitle(ctx, db.SetMediaTitleParams{Title: title, ID: id}); err != nil {
		return err
	}
	s.Hub.Publish(Event{Type: EventPlaylist})
	return nil
}

// SetImageDuration changes how long an image stays on screen.
func (s *Service) SetImageDuration(ctx context.Context, id int64, seconds int) error {
	if seconds <= 0 {
		return ErrBadDuration
	}
	s.plMu.Lock()
	defer s.plMu.Unlock()
	m, err := s.st.GetMedia(ctx, id)
	if err != nil {
		return err
	}
	if err := s.st.SetMediaImageDuration(ctx, db.SetMediaImageDurationParams{ImageDurationS: int64(seconds), ID: id}); err != nil {
		return err
	}
	s.Hub.Publish(Event{Type: EventPlaylist})
	// The duration is a per-entry option in mpv, so the entry is reloaded.
	if m.Kind == "image" {
		s.reloadEntry(ctx, m)
		return s.sync(ctx)
	}
	return nil
}

// Sync makes mpv's playlist match the database.
func (s *Service) Sync(ctx context.Context) error {
	s.plMu.Lock()
	defer s.plMu.Unlock()
	return s.sync(ctx)
}

// sync reconciles mpv's playlist with the database using remove/move/insert,
// so the entry that is currently playing keeps playing. Caller holds plMu.
func (s *Service) sync(ctx context.Context) error {
	if s.streaming != "" {
		return nil // the stream owns mpv until ResumePlaylist
	}
	list, err := s.st.ListMedia(ctx)
	if err != nil {
		return err
	}
	pl, err := s.player.Playlist(ctx)
	if errors.Is(err, mpv.ErrNotConnected) {
		return nil // OnConnect syncs once mpv is up
	}
	if err != nil {
		return err
	}
	want := make([]string, len(list))
	wanted := map[string]bool{}
	for i, m := range list {
		want[i] = s.MediaPath(m)
		wanted[want[i]] = true
	}
	cur := make([]string, len(pl))
	for i, e := range pl {
		cur[i] = e.Filename
	}

	// Drop entries that are gone or duplicated, from the end so indexes stay valid.
	seen := map[string]int{}
	for _, f := range cur {
		seen[f]++
	}
	for i := len(cur) - 1; i >= 0; i-- {
		if !wanted[cur[i]] || seen[cur[i]] > 1 {
			seen[cur[i]]--
			if _, err := s.player.Command(ctx, "playlist-remove", i); err != nil {
				return err
			}
			cur = slices.Delete(cur, i, i+1)
		}
	}

	for i, m := range list {
		if i < len(cur) && cur[i] == want[i] {
			continue
		}
		if j := slices.Index(cur, want[i]); j > i {
			if _, err := s.player.Command(ctx, "playlist-move", j, i); err != nil {
				return err
			}
			f := cur[j]
			cur = slices.Insert(slices.Delete(cur, j, j+1), i, f)
			continue
		}
		flag := "insert-at"
		if len(cur) == 0 {
			flag = "insert-at-play" // start playback when mpv was idle
		}
		opts := ""
		switch {
		case m.Kind == "image":
			opts = fmt.Sprintf("image-display-duration=%d", m.ImageDurationS)
		case m.Kind == "stream" && m.Format != "":
			// mpv hands this to yt-dlp. The spec contains '=' and brackets,
			// so it is length-quoted (%N%value) for mpv's option parser.
			opts = fmt.Sprintf("ytdl-format=%%%d%%%s", len(m.Format), m.Format)
		}
		if _, err := s.player.Command(ctx, "loadfile", want[i], flag, i, opts); err != nil {
			return err
		}
		cur = slices.Insert(cur, i, want[i])
	}
	return nil
}

// --- playback controls ------------------------------------------------------

func (s *Service) Next(ctx context.Context) error {
	if err := s.unlocked(ctx); err != nil {
		return err
	}
	_, err := s.player.Command(ctx, "playlist-next", "force")
	return err
}

func (s *Service) Prev(ctx context.Context) error {
	if err := s.unlocked(ctx); err != nil {
		return err
	}
	_, err := s.player.Command(ctx, "playlist-prev", "force")
	return err
}

func (s *Service) TogglePause(ctx context.Context) error {
	if err := s.unlocked(ctx); err != nil {
		return err
	}
	_, err := s.player.Command(ctx, "cycle", "pause")
	return err
}

var ErrNotSeekable = errors.New("this can't jump to a position")

// Seek jumps to pos seconds in the playing video or seekable stream. Images
// (mpv reports no duration) and live streams are refused.
func (s *Service) Seek(ctx context.Context, pos float64) error {
	if err := s.unlocked(ctx); err != nil {
		return err
	}
	ps := s.PlayerState()
	if ps.Path == "" || ps.Streaming != "" || ps.Duration <= 0 {
		return ErrNotSeekable
	}
	var seekable bool
	if data, err := s.player.Command(ctx, "get_property", "seekable"); err != nil || json.Unmarshal(data, &seekable) != nil || !seekable {
		return ErrNotSeekable
	}
	_, err := s.player.Command(ctx, "seek", min(max(pos, 0), ps.Duration-1), "absolute")
	return err
}

// ToggleStats shows/hides mpv's stats overlay on the screen (key "i").
func (s *Service) ToggleStats(ctx context.Context) error {
	if err := s.unlocked(ctx); err != nil {
		return err
	}
	_, err := s.player.Command(ctx, "keypress", "i")
	return err
}

// PlayIndex jumps to the 1-based playlist position.
func (s *Service) PlayIndex(ctx context.Context, pos int) error {
	if err := s.unlocked(ctx); err != nil {
		return err
	}
	_, err := s.player.Command(ctx, "set_property", "playlist-pos", pos-1)
	return err
}

// Stream replaces the playlist with a stream URL until ResumePlaylist.
func (s *Service) Stream(ctx context.Context, url string) error {
	if err := s.unlocked(ctx); err != nil {
		return err
	}
	s.plMu.Lock()
	defer s.plMu.Unlock()
	if _, err := s.player.Command(ctx, "loadfile", url, "replace"); err != nil {
		return err
	}
	s.streaming = url
	s.Hub.Publish(Event{Type: EventPlayer})
	s.Logf(ctx, slog.LevelInfo, "streaming", "url", url)
	return nil
}

// ResumePlaylist ends a stream and goes back to the playlist.
func (s *Service) ResumePlaylist(ctx context.Context) error {
	if err := s.unlocked(ctx); err != nil {
		return err
	}
	s.plMu.Lock()
	defer s.plMu.Unlock()
	s.streaming = ""
	if _, err := s.player.Command(ctx, "playlist-clear"); err != nil {
		return err
	}
	if _, err := s.player.Command(ctx, "playlist-remove", "current"); err != nil {
		slog.Debug("remove stream entry", "err", err)
	}
	s.Hub.Publish(Event{Type: EventPlayer})
	return s.sync(ctx)
}

// RestartPlayer restarts mpv; it reloads the playlist when it comes back.
func (s *Service) RestartPlayer(ctx context.Context) error {
	if err := s.unlocked(ctx); err != nil {
		return err
	}
	s.player.Restart()
	return nil
}

// RestartService has systemd restart the whole service (Telegram /restart
// all, System page). It waits a moment so the reply still gets out, and uses
// --no-block: the restart stops this process, systemctl included.
func (s *Service) RestartService(ctx context.Context) error {
	if err := s.unlocked(ctx); err != nil {
		return err
	}
	s.Logf(ctx, slog.LevelInfo, "service restart requested")
	go func() {
		time.Sleep(500 * time.Millisecond)
		out, err := exec.Command("sudo", "-n", "systemctl", "--no-block", "restart", "flimmer.service").CombinedOutput()
		if err != nil && !stoppedByRestart(err) {
			s.Logf(context.Background(), slog.LevelError, "service restart failed", "err", err, "out", strings.TrimSpace(string(out)))
		}
	}()
	return nil
}

// stoppedByRestart: systemd stops the service by sending SIGTERM to all its
// processes, so the sudo/systemctl helper may be terminated before it exits.
// That means the restart is already under way, not that it failed.
func stoppedByRestart(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGTERM
}
