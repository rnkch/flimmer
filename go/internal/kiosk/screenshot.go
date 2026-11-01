package kiosk

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Screenshot returns what the screen shows as a JPEG. "window" captures the
// output including the OSD (QR code, stats); without a real video output
// (--vo=null in tests and dev.sh) mpv only has the decoded frame, so it falls
// back to "video".
func (s *Service) Screenshot(ctx context.Context) ([]byte, error) {
	png := filepath.Join(s.cfg.TmpDir, fmt.Sprintf("screen-%d.png", time.Now().UnixNano()))
	defer os.Remove(png)
	mode := "window"
	_, err := s.player.Command(ctx, "screenshot-to-file", png, mode)
	if err != nil {
		mode = "video"
		_, err = s.player.Command(ctx, "screenshot-to-file", png, mode)
	}
	if err != nil {
		return nil, fmt.Errorf("screenshot: %w", err)
	}
	// The CRT's pixels are not square (osd-par, see showQR): stretch the
	// window capture so the picture looks like it does on the screen.
	vf := "null"
	if mode == "window" {
		if w, _, par := s.osdGeometry(ctx); math.Abs(par-1) > 0.01 {
			vf = fmt.Sprintf("scale=%d:ih", int(math.Round(float64(w)/par)))
		}
	}
	jpg := png[:len(png)-len(".png")] + ".jpg"
	defer os.Remove(jpg)
	out, err := exec.CommandContext(ctx, s.cfg.FFmpeg, "-v", "error", "-y", "-i", png, "-vf", vf, "-q:v", "3", jpg).
		CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("screenshot: %v: %s", err, out)
	}
	return os.ReadFile(jpg)
}
