package kiosk

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rnkch/flimmerkiste/internal/access"
)

// The QR code overlay on the screen: mpv draws a BGRA bitmap (overlay-add)
// with the address below it as ASS text (osd-overlay), over whatever plays.

const (
	qrOverlayID  = 1                // overlay-add id
	qrTextID     = 1                // osd-overlay id
	QRShowFor    = 60 * time.Second // default: bot /qr, after a Wi-Fi switch
	quietModules = 4                // white border the QR standard asks for
)

type qrState struct {
	sync.Mutex
	shown bool
	timer *time.Timer
}

// AccessURL is the address the web UI can be reached at on the local network
// (looked up each time: the Pi may get a new address from the router).
func (s *Service) AccessURL() string {
	if s.cfg.TLSAddr == "" && s.cfg.PublicURL == "" {
		return ""
	}
	return access.URL(s.cfg.TLSAddr, s.cfg.PublicURL)
}

// ToggleQR shows the access QR code on the screen for d, or hides it if
// shown. It returns whether it is now shown.
func (s *Service) ToggleQR(ctx context.Context, d time.Duration) (bool, error) {
	if err := s.unlocked(ctx); err != nil {
		return false, err
	}
	s.qr.Lock()
	defer s.qr.Unlock()
	if s.qr.shown {
		s.hideQRLocked(ctx)
		return false, nil
	}
	return true, s.showQRLocked(ctx, d)
}

// ShowQR shows the QR code (again, with the current address) for QRShowFor.
func (s *Service) ShowQR(ctx context.Context) error {
	s.qr.Lock()
	defer s.qr.Unlock()
	if s.qr.shown {
		s.hideQRLocked(ctx)
	}
	return s.showQRLocked(ctx, QRShowFor)
}

func (s *Service) showQRLocked(ctx context.Context, d time.Duration) error {
	if err := s.showQR(ctx); err != nil {
		return err
	}
	s.qr.shown = true
	s.qr.timer = time.AfterFunc(d, func() {
		s.qr.Lock()
		defer s.qr.Unlock()
		s.hideQRLocked(context.Background())
	})
	return nil
}

func (s *Service) hideQRLocked(ctx context.Context) {
	if s.qr.timer != nil {
		s.qr.timer.Stop()
	}
	s.qr.shown = false
	s.player.Command(ctx, "overlay-remove", qrOverlayID)
	s.player.CommandNamed(
		ctx,
		map[string]any{"name": "osd-overlay", "id": qrTextID, "format": "none", "data": ""},
	)
}

func (s *Service) showQR(ctx context.Context) error {
	url := s.AccessURL()
	if url == "" {
		return fmt.Errorf("no address on the local network found")
	}
	code, err := access.QR(url)
	if err != nil {
		return err
	}
	osdW, osdH, par := s.osdGeometry(ctx)

	// Square modules on screen: about 2/3 of the height, whole pixels per
	// module. The width is corrected for non-square pixels the same way mpv
	// corrects the video (with monitorpixelaspect=1.094, osd-par is 0.914 and
	// both are drawn narrower so they come out right on the wider CRT pixels).
	n := code.Size + 2*quietModules
	px := max(int(float64(osdH)*0.66)/n, 2)
	h := n * px
	w := h // bitmap is square; display width below corrects the aspect
	dw := int(math.Round(float64(h) * par))
	x, y := (osdW-dw)/2, (osdH-h)/2-osdH/20

	buf := make([]byte, w*h*4)
	for row := 0; row < h; row++ {
		for col := 0; col < w; col++ {
			mx, my := col/px-quietModules, row/px-quietModules
			v := byte(255)
			if mx >= 0 && my >= 0 && mx < code.Size && my < code.Size && code.Black(mx, my) {
				v = 0
			}
			i := (row*w + col) * 4
			buf[i], buf[i+1], buf[i+2], buf[i+3] = v, v, v, 255 // B G R A, opaque
		}
	}
	file := filepath.Join(s.cfg.TmpDir, "qr.bgra")
	if err := os.WriteFile(file, buf, 0o600); err != nil {
		return err
	}
	if _, err := s.player.Command(
		ctx,
		"overlay-add",
		qrOverlayID,
		x,
		y,
		file,
		0,
		"bgra",
		w,
		h,
		w*4,
		dw,
		h,
	); err != nil {
		return err
	}
	// The address below the code, centered; ASS coordinates in OSD pixels.
	text := fmt.Sprintf(`{\an8\pos(%d,%d)\fs%d\bord3\shad0\1c&HFFFFFF&\3c&H000000&}%s`,
		osdW/2, y+h+osdH/40, max(osdH/18, 16), assEscape(url))
	_, err = s.player.CommandNamed(ctx, map[string]any{
		"name": "osd-overlay", "id": qrTextID, "format": "ass-events",
		"data": text, "res_x": osdW, "res_y": osdH,
	})
	if err != nil {
		slog.Warn("qr text overlay", "err", err)
	}
	return nil
}

// osdGeometry returns the OSD size and pixel aspect, with PAL defaults when
// mpv has no video output (e.g. while idle without a window).
func (s *Service) osdGeometry(ctx context.Context) (w, h int, par float64) {
	w, h, par = 720, 576, 1
	get := func(name string, dst any) {
		if data, err := s.player.Command(ctx, "get_property", name); err == nil {
			json.Unmarshal(data, dst)
		}
	}
	var ow, oh int
	var p float64
	get("osd-width", &ow)
	get("osd-height", &oh)
	get("osd-par", &p)
	if ow > 0 && oh > 0 {
		w, h = ow, oh
	}
	if p > 0.5 && p < 2 {
		par = p
	}
	return w, h, par
}

func assEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "{", `\{`, "}", `\}`).Replace(s)
}
