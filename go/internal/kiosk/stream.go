package kiosk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rnkch/flimmerkiste/internal/mpv"
	"github.com/rnkch/flimmerkiste/internal/store/db"
)

// Streams are playlist entries without a file: mpv plays the URL directly
// (for web pages through its yt-dlp hook, with the entry's format spec).

// StreamSchemes can be played directly by mpv without yt-dlp.
var StreamSchemes = []string{"rtsp", "rtsps", "rtmp", "srt", "udp"}

// IsDirectStream reports whether u is a stream URL that yt-dlp can't probe.
func IsDirectStream(u string) bool {
	p, err := url.Parse(u)
	return err == nil && slices.Contains(StreamSchemes, strings.ToLower(p.Scheme))
}

// DirectStreamInfo describes an RTSP/SRT/... URL for the preview: no
// formats, nothing to download, only "play directly".
func DirectStreamInfo(u string) URLInfo {
	title := u
	if p, err := url.Parse(u); err == nil {
		title = p.Host + p.Path
	}
	return URLInfo{
		URL:   u,
		Title: title,
		Site:  strings.ToUpper(strings.SplitN(u, ":", 2)[0]),
		Live:  true,
	}
}

// StreamSpec turns a chosen format into the spec stored with a stream entry:
// the exact format first, then H.264 at the same height, then anything up to
// that height, so the entry keeps playing when a site drops one format. The
// fallbacks stay within what the Pi can play (30 frames, 720 lines when the height is
// unknown) and end at the worst format, never at the best: a live stream whose formats
// weren't listed used to come out as 1080p60, which is too much. (`<=?` lets a format
// without that information through.)
func StreamSpec(f URLFormat) string {
	const maxFPS, unknownHeight = 30, 720
	if f.Spec == "" || f.Spec == "b" || f.Height == 0 {
		return fmt.Sprintf(
			"bv*[height<=?%[1]d][fps<=?%[2]d][vcodec^=avc1]+ba/b[height<=?%[1]d][fps<=?%[2]d]/w",
			unknownHeight,
			maxFPS,
		)
	}
	h := f.Height
	return fmt.Sprintf(
		"%[1]s/bv*[height<=%[2]d][fps<=?%[3]d][vcodec^=avc1]+ba/b[height<=%[2]d][fps<=?%[3]d]/bv*[height<=%[2]d]+ba/w",
		f.Spec,
		h,
		maxFPS,
	)
}

// AddStream puts a URL into the playlist at pos (0 = end) without downloading.
func (s *Service) AddStream(
	ctx context.Context,
	info URLInfo,
	f URLFormat,
	title string,
	pos int,
) (db.Media, error) {
	if title = strings.TrimSpace(title); title == "" {
		title = info.Title
	}
	spec := ""
	if !IsDirectStream(info.URL) {
		spec = StreamSpec(f)
	}
	m, err := s.addMedia(ctx, db.InsertMediaParams{
		Filename: uniqueStreamName(), Title: title, Kind: "stream", Height: int64(f.Height),
		DurationS: info.Duration, Source: "url", URL: info.URL, Format: spec,
	}, pos)
	if err != nil {
		return m, err
	}
	// Thumbnails now, so the row shows one right away (best effort).
	src := info.Thumbnail
	if src == "" {
		src = info.URL // direct streams and plain files: grab a frame
	}
	s.streamThumbs(ctx, m, src)
	s.Hub.Publish(Event{Type: EventPlaylist})
	s.Logf(ctx, slog.LevelInfo, "added stream", "title", m.Title, "url", m.URL)
	return m, nil
}

func uniqueStreamName() string {
	return fmt.Sprintf("stream-%d", time.Now().UnixNano())
}

// streamThumbs renders the small and the large preview from src (an image
// URL or the stream itself) with ffmpeg.
func (s *Service) streamThumbs(ctx context.Context, m db.Media, src string) {
	if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") &&
		!IsDirectStream(src) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, w := range []int{240, 720} {
		name := fmt.Sprintf("%d.jpg", m.ID)
		if w != 240 {
			name = fmt.Sprintf("%d-%d.jpg", m.ID, w)
		}
		dst := filepath.Join(s.cfg.ThumbDir, name)
		tmp := dst + ".tmp.jpg"
		out, err := exec.CommandContext(ctx, s.cfg.FFmpeg, "-v", "error", "-y", "-nostdin", "-i", src,
			"-frames:v", "1", "-vf", fmt.Sprintf("scale=%d:-2", w), "-q:v", "5", tmp).
			CombinedOutput()
		if err != nil {
			os.Remove(tmp)
			slog.Warn("stream thumbnail", "id", m.ID, "err", lastLine(out))
			return
		}
		os.Rename(tmp, dst)
	}
}

// SetStreamFormat changes the format of a stream entry and reloads it in mpv.
func (s *Service) SetStreamFormat(ctx context.Context, id int64, f URLFormat) error {
	m, err := s.st.GetMedia(ctx, id)
	if err != nil {
		return err
	}
	if err := s.unlockedFor(ctx, m); err != nil {
		return err
	}
	s.plMu.Lock()
	defer s.plMu.Unlock()
	if m.Kind != "stream" {
		return ErrNotFound
	}
	if err := s.st.SetMediaFormat(
		ctx,
		db.SetMediaFormatParams{Format: StreamSpec(f), Height: int64(f.Height), ID: id},
	); err != nil {
		return err
	}
	s.Hub.Publish(Event{Type: EventPlaylist})
	s.reloadEntry(ctx, m)
	return s.sync(ctx)
}

// reloadEntry drops one entry from mpv; the following sync adds it back with
// its current options. Caller holds plMu.
func (s *Service) reloadEntry(ctx context.Context, m db.Media) {
	if s.streaming != "" {
		return
	}
	path := s.MediaPath(m)
	if pl, err := s.player.Playlist(ctx); err == nil {
		if i := slices.IndexFunc(
			pl,
			func(e mpv.PlaylistEntry) bool { return e.Filename == path },
		); i >= 0 {
			s.player.Command(ctx, "playlist-remove", i)
		}
	}
}

// DownloadInstead queues a download of a stream entry; when it is done the
// downloaded copy takes the stream's place in the playlist.
func (s *Service) DownloadInstead(ctx context.Context, id int64) (db.Job, error) {
	m, err := s.st.GetMedia(ctx, id)
	if err != nil {
		return db.Job{}, err
	}
	if m.Kind != "stream" || IsDirectStream(m.URL) || m.DurationS == 0 {
		return db.Job{}, ErrLive
	}
	format := m.Format
	if format == "" {
		format = "b"
	}
	return s.enqueue(ctx, db.InsertJobParams{
		Source: "url", URL: m.URL, Format: format, Title: m.Title, OrigName: m.Title,
		ReplacesMediaID: m.ID,
	})
}

// replaceStream removes the stream entry a finished download replaces.
func (s *Service) replaceStream(ctx context.Context, j db.Job) {
	if j.ReplacesMediaID == 0 {
		return
	}
	err := s.Delete(ctx, j.ReplacesMediaID)
	switch {
	case errors.Is(err, ErrLocked):
		// The stream is on the screen right now; it stays until someone deletes it.
		s.Logf(ctx, slog.LevelWarn, "stream kept: playback is locked", "title", j.Title)
	case err != nil && !errors.Is(err, ErrNotFound):
		slog.Warn("remove replaced stream", "id", j.ReplacesMediaID, "err", err)
	}
}
