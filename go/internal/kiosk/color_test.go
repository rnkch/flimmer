package kiosk

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// ingestFile runs a file through the upload queue and returns the stored file's path.
func ingestFile(t *testing.T, s *Service, path, name string) string {
	t.Helper()
	s.cfg.VideoEncoder = "libx264" // h264_v4l2m2m exists only on the Pi
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	j, err := s.EnqueueFile(ctx, path, name, Caption{})
	if err != nil {
		t.Fatal(err)
	}
	go s.RunWorker(ctx)
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		if j, _ = s.Job(ctx, j.ID); j.State == "done" || j.State == "failed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	list, _ := s.List(ctx)
	if j.State != "done" || len(list) != 1 {
		t.Fatalf("job: %s %s", j.State, j.Error)
	}
	return filepath.Join(s.cfg.MediaDir, list[0].Filename)
}

func ffmpegOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
}

// A phone video is stored landscape with a rotation; it must be scaled by the size as shown, and
// not come out stretched.
func TestIngestRotatedVideo(t *testing.T) {
	ffmpegOrSkip(t)
	s, _ := newTestService(t)
	flat := filepath.Join(s.cfg.TmpDir, "flat.mp4")
	vid := filepath.Join(s.cfg.TmpDir, "rot.mp4")
	if out, err := exec.Command(
		"ffmpeg",
		"-v",
		"error",
		"-y",
		"-f",
		"lavfi",
		"-i",
		"testsrc=s=1280x720:d=1:r=25",
		"-c:v",
		"libx264",
		"-pix_fmt",
		"yuv420p",
		flat,
	).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if out, err := exec.Command("ffmpeg", "-v", "error", "-y", "-display_rotation", "90", "-i", flat,
		"-c", "copy", vid).
		CombinedOutput(); err != nil {
		t.Skipf("this ffmpeg can't set a rotation: %v: %s", err, out)
	}
	in, _ := probe(context.Background(), "ffprobe", vid)
	if w, h := in.Display(); w != 720 || h != 1280 {
		t.Fatalf("probe shows %dx%d (rotation %d), want 720x1280", w, h, in.Rotation)
	}
	got, _ := probe(context.Background(), "ffprobe", ingestFile(t, s, vid, "rot.mp4"))
	// 720x1280 upright, fitted into 720x576: 324x576
	if w, h := got.Display(); w != 324 || h != 576 {
		t.Fatalf("stored %dx%d (coded %dx%d, rotation %d), want 324x576 upright",
			w, h, got.Width, got.Height, got.Rotation)
	}
}

func TestIngestNonSquarePixels(t *testing.T) {
	ffmpegOrSkip(t)
	s, _ := newTestService(t)
	vid := filepath.Join(s.cfg.TmpDir, "anamorphic.mp4")
	// 1440x1080 pixels with SAR 4:3 are shown as 1920x1080: too big, so it is transcoded
	if out, err := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i",
		"testsrc=s=1440x1080:d=1:r=25,setsar=4/3", "-c:v", "libx264", "-pix_fmt", "yuv420p", vid).
		CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got, _ := probe(context.Background(), "ffprobe", ingestFile(t, s, vid, "ana.mp4"))
	// shown at 1920x1080, fitted into 720x576: 720x404 with square pixels
	if w, h := got.Display(); w != 720 || h != 404 || got.nonSquare() {
		t.Fatalf("stored %dx%d (coded %dx%d, SAR %d:%d), want 720x404 square",
			w, h, got.Width, got.Height, got.SARNum, got.SARDen)
	}
}

func TestPlaysAsIsUsesDisplaySize(t *testing.T) {
	// 1280x720 stored, turned upright: 720x1280 as shown, too tall for the CRT
	in := probeInfo{
		VideoCodec: "h264",
		PixFmt:     "yuv420p",
		Width:      1280,
		Height:     720,
		FPS:        25,
		Rotation:   90,
	}
	if playsAsIs(false, in) {
		t.Fatal("a portrait 720x1280 video plays as it is on composite")
	}
	hdr := probeInfo{VideoCodec: "h264", PixFmt: "yuv420p", Width: 640, Height: 360, FPS: 25,
		ColorPrimaries: "bt2020", ColorTransfer: "arib-std-b67"}
	if playsAsIs(false, hdr) {
		t.Fatal("HLG video plays as it is")
	}
}

// HDR phone video is shown tinted and flat by a player without colour management: it is
// converted to BT.709.
func TestIngestHDRVideo(t *testing.T) {
	ffmpegOrSkip(t)
	s, _ := newTestService(t)
	if !hasFilter(context.Background(), "ffmpeg", "zscale") {
		t.Skip("this ffmpeg has no zscale")
	}
	vid := filepath.Join(s.cfg.TmpDir, "hlg.mp4")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=s=640x360:d=1:r=25",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-color_primaries", "bt2020", "-color_trc", "arib-std-b67",
		"-colorspace", "bt2020nc",
		"-x264-params", "colorprim=bt2020:transfer=arib-std-b67:colormatrix=bt2020nc", vid).
		CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	in, _ := probe(context.Background(), "ffprobe", vid)
	if !in.wideGamut() {
		t.Fatalf("test file isn't tagged HDR: %+v", in)
	}
	got, _ := probe(context.Background(), "ffprobe", ingestFile(t, s, vid, "hlg.mp4"))
	if got.wideGamut() || got.ColorPrimaries != "bt709" || got.ColorTransfer != "bt709" {
		t.Fatalf("not converted: primaries %q, transfer %q", got.ColorPrimaries, got.ColorTransfer)
	}
}

// --- photos -----------------------------------------------------------------

func xyzTag(x, y, z float64) []byte {
	b := append([]byte("XYZ \x00\x00\x00\x00"), make([]byte, 12)...)
	for i, v := range []float64{x, y, z} {
		binary.BigEndian.PutUint32(b[8+4*i:], uint32(int32(v*65536)))
	}
	return b
}

// sRGB's curve as an ICC parametric curve (type 3).
func srgbPara() []byte {
	b := append([]byte("para\x00\x00\x00\x00\x00\x03\x00\x00"), make([]byte, 20)...)
	for i, v := range []float64{2.4, 1 / 1.055, 0.055 / 1.055, 1 / 12.92, 0.04045} {
		binary.BigEndian.PutUint32(b[12+4*i:], uint32(int32(v*65536)))
	}
	return b
}

// fakeProfile builds a matrix/TRC RGB profile with the given colorants (columns r, g, b).
func fakeProfile(m [3][3]float64) []byte {
	tags := []struct {
		sig  string
		data []byte
	}{
		{"rXYZ", xyzTag(m[0][0], m[1][0], m[2][0])},
		{"gXYZ", xyzTag(m[0][1], m[1][1], m[2][1])},
		{"bXYZ", xyzTag(m[0][2], m[1][2], m[2][2])},
		{"rTRC", srgbPara()}, {"gTRC", srgbPara()}, {"bTRC", srgbPara()},
	}
	hdr := make([]byte, 132)
	copy(hdr[16:], "RGB XYZ ")
	binary.BigEndian.PutUint32(hdr[128:], uint32(len(tags)))
	off := 132 + 12*len(tags)
	var table, body []byte
	for _, tg := range tags {
		e := make([]byte, 12)
		copy(e, tg.sig)
		binary.BigEndian.PutUint32(e[4:], uint32(off+len(body)))
		binary.BigEndian.PutUint32(e[8:], uint32(len(tg.data)))
		table, body = append(table, e...), append(body, tg.data...)
	}
	return append(append(hdr, table...), body...)
}

// Display P3's colorants, adapted to D50.
var p3M = [3][3]float64{
	{0.5151, 0.2920, 0.1571},
	{0.2412, 0.6922, 0.0666},
	{-0.0011, 0.0419, 0.7841},
}

// jpegWithICC is a flat-colour JPEG with the profile in APP2 and a made-up Exif segment.
func jpegWithICC(t *testing.T, c color.NRGBA, icc []byte) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, 255
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	app := func(m byte, payload []byte) []byte {
		n := len(payload) + 2
		return append([]byte{0xFF, m, byte(n >> 8), byte(n)}, payload...)
	}
	out := append([]byte{}, b.Bytes()[:2]...)
	out = append(out, app(0xE1, []byte("Exif\x00\x00orientation-marker"))...)
	out = append(out, app(0xE2, append([]byte("ICC_PROFILE\x00\x01\x01"), icc...))...)
	return append(out, b.Bytes()[2:]...)
}

func pixelOf(t *testing.T, data []byte) color.NRGBA {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return color.NRGBAModel.Convert(img.At(16, 16)).(color.NRGBA)
}

func TestStillColorP3ToSRGB(t *testing.T) {
	s, _ := newTestService(t)
	path := filepath.Join(s.cfg.MediaDir, "p3.jpg")
	os.WriteFile(path, jpegWithICC(t, color.NRGBA{200, 100, 100, 255}, fakeProfile(p3M)), 0o644)
	s.fixStillColor(path)
	got, _ := os.ReadFile(path)
	px := pixelOf(t, got)
	// The same numbers mean a more saturated colour in P3 than in sRGB: red goes up, green and
	// blue go down.
	if px.R <= 205 || px.G >= 98 || px.B >= 100 {
		t.Fatalf("P3 (200,100,100) became sRGB %v", px)
	}
	if !bytes.Contains(got, []byte("orientation-marker")) {
		t.Fatal("the Exif segment (orientation) was dropped")
	}
	if jpegICC(got) != nil {
		t.Fatal("the result still claims the old profile")
	}

	// Grey stays grey (same white point).
	os.WriteFile(path, jpegWithICC(t, color.NRGBA{128, 128, 128, 255}, fakeProfile(p3M)), 0o644)
	s.fixStillColor(path)
	got, _ = os.ReadFile(path)
	if px := pixelOf(
		t,
		got,
	); abs(int(px.R)-128) > 2 || abs(int(px.G)-128) > 2 ||
		abs(int(px.B)-128) > 2 {
		t.Fatalf("grey became %v", px)
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func TestStillColorLeavesSRGBAndUntagged(t *testing.T) {
	s, _ := newTestService(t)
	path := filepath.Join(s.cfg.MediaDir, "x.jpg")
	for name, data := range map[string][]byte{
		"sRGB profile":   jpegWithICC(t, color.NRGBA{200, 100, 100, 255}, fakeProfile(srgbM)),
		"no profile":     jpegWithICC(t, color.NRGBA{200, 100, 100, 255}, nil),
		"broken profile": jpegWithICC(t, color.NRGBA{200, 100, 100, 255}, []byte("garbage")),
	} {
		os.WriteFile(path, data, 0o644)
		s.fixStillColor(path)
		if got, _ := os.ReadFile(path); !bytes.Equal(got, data) {
			t.Errorf("%s: file was rewritten", name)
		}
	}
}

// The whole way: a P3 photo uploaded through the queue comes out as sRGB.
func TestIngestP3Photo(t *testing.T) {
	ffmpegOrSkip(t)
	s, _ := newTestService(t)
	src := filepath.Join(s.cfg.TmpDir, "photo.jpg")
	os.WriteFile(src, jpegWithICC(t, color.NRGBA{200, 100, 100, 255}, fakeProfile(p3M)), 0o644)
	got, _ := os.ReadFile(ingestFile(t, s, src, "photo.jpg"))
	if px := pixelOf(t, got); px.R <= 205 {
		t.Fatalf("not converted: %v", px)
	}
}
