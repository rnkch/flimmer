package kiosk

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/rnkch/flimmerkiste/internal/store/db"
)

// Thumbnail returns the path of a small JPEG preview, creating it on first use.
func (s *Service) Thumbnail(ctx context.Context, m db.Media) (string, error) {
	return s.frame(ctx, m, 240)
}

// Preview returns a large view: the file itself for images, a 720 px wide
// frame for videos.
func (s *Service) Preview(ctx context.Context, m db.Media) (string, error) {
	if m.Kind == "image" {
		return s.MediaPath(m), nil
	}
	return s.frame(ctx, m, 720)
}

// frame renders a JPEG of the given width (1 s into videos), cached on disk.
func (s *Service) frame(ctx context.Context, m db.Media, width int) (string, error) {
	name := fmt.Sprintf("%d.jpg", m.ID)
	if width != 240 {
		name = fmt.Sprintf("%d-%d.jpg", m.ID, width)
	}
	dst := filepath.Join(s.cfg.ThumbDir, name)
	if exists(dst) {
		return dst, nil
	}
	s.thumbMu.Lock()
	defer s.thumbMu.Unlock()
	if exists(dst) {
		return dst, nil
	}
	args := []string{"-v", "error", "-y", "-nostdin"}
	if m.Kind == "video" && m.DurationS > 2 {
		args = append(args, "-ss", "1")
	}
	tmp := dst + ".tmp.jpg"
	args = append(
		args,
		"-i",
		s.MediaPath(m),
		"-frames:v",
		"1",
		"-vf",
		fmt.Sprintf("scale=%d:-2", width),
		"-q:v",
		"5",
		tmp,
	)
	if out, err := exec.CommandContext(ctx, "nice", append([]string{"-n", "10", s.cfg.FFmpeg}, args...)...).
		CombinedOutput(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("thumbnail: %v: %s", err, lastLine(out))
	}
	return dst, os.Rename(tmp, dst)
}

func (s *Service) dropThumbnail(id int64) {
	os.Remove(filepath.Join(s.cfg.ThumbDir, fmt.Sprintf("%d.jpg", id)))
	os.Remove(filepath.Join(s.cfg.ThumbDir, fmt.Sprintf("%d-720.jpg", id)))
}
