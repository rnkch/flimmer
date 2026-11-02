package kiosk

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"math"
	"os"
	"strings"
)

// Photos from phones and cameras often carry an ICC profile (Display P3, Adobe RGB). mpv on the
// Pi runs without colour management (gpu-dumb-mode), so it would show the P3 numbers as if they
// were sRGB: dull, off colours. fixStillColor converts such a picture to sRGB while ingesting.
// Only RGB profiles made of three colorants and three tone curves (what P3, Adobe RGB, ProPhoto
// and sRGB are) are understood; anything else stays as it is.

const maxColorPixels = 60_000_000 // a 60 MP picture would need ~250 MB: leave it alone

// fixStillColor rewrites the picture at path as sRGB when its profile says it isn't. It never
// fails the upload: on any problem the file stays as it was.
func (s *Service) fixStillColor(path string) {
	ext := strings.ToLower(strings.TrimPrefix(extOf(path), "."))
	if ext != "jpg" && ext != "png" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var icc []byte
	if ext == "jpg" {
		icc = jpegICC(data)
	} else {
		icc = pngICC(data)
	}
	if icc == nil {
		return
	}
	conv, ok := parseRGBProfile(icc)
	if !ok || conv.isSRGB() {
		return
	}
	out, err := convertToSRGB(data, ext, conv)
	if err != nil {
		slog.Warn("colour conversion skipped", "file", path, "err", err)
		return
	}
	if err := os.WriteFile(path+".tmp", out, 0o644); err != nil {
		return
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		os.Remove(path + ".tmp")
	}
}

func extOf(p string) string {
	if i := strings.LastIndexByte(p, '.'); i >= 0 {
		return p[i:]
	}
	return ""
}

// --- profile extraction -----------------------------------------------------

// jpegSegments calls f for each marker segment before the image data: marker byte, payload.
func jpegSegments(data []byte, f func(marker byte, payload []byte)) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return
	}
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			return
		}
		m := data[i+1]
		if m == 0xDA || m == 0xD9 { // start of scan / end: no more headers
			return
		}
		n := int(binary.BigEndian.Uint16(data[i+2:]))
		if n < 2 || i+2+n > len(data) {
			return
		}
		f(m, data[i+4:i+2+n])
		i += 2 + n
	}
}

// jpegICC joins the profile chunks of a JPEG (APP2 "ICC_PROFILE", numbered); nil when none.
func jpegICC(data []byte) []byte {
	const tag = "ICC_PROFILE\x00"
	chunks := map[int][]byte{}
	total := 0
	jpegSegments(data, func(m byte, p []byte) {
		if m == 0xE2 && len(p) > len(tag)+2 && string(p[:len(tag)]) == tag {
			chunks[int(p[len(tag)])] = p[len(tag)+2:]
			total = int(p[len(tag)+1])
		}
	})
	if total == 0 || len(chunks) != total {
		return nil
	}
	var out []byte
	for i := 1; i <= total; i++ {
		c, ok := chunks[i]
		if !ok {
			return nil
		}
		out = append(out, c...)
	}
	return out
}

// jpegExif is the Exif segment (with its orientation), kept so the picture still turns upright.
func jpegExif(data []byte) []byte {
	var exif []byte
	jpegSegments(data, func(m byte, p []byte) {
		if m == 0xE1 && exif == nil && bytes.HasPrefix(p, []byte("Exif\x00\x00")) {
			exif = p
		}
	})
	return exif
}

// pngICC returns the inflated iCCP profile of a PNG; nil when none.
func pngICC(data []byte) []byte {
	if len(data) < 8 || string(data[1:4]) != "PNG" {
		return nil
	}
	for i := 8; i+8 <= len(data); {
		n := int(binary.BigEndian.Uint32(data[i:]))
		typ := string(data[i+4 : i+8])
		if n < 0 || i+12+n > len(data) {
			return nil
		}
		body := data[i+8 : i+8+n]
		switch typ {
		case "iCCP":
			z := bytes.IndexByte(body, 0) // name, NUL, method byte, zlib stream
			if z < 0 || z+2 > len(body) {
				return nil
			}
			r, err := zlib.NewReader(bytes.NewReader(body[z+2:]))
			if err != nil {
				return nil
			}
			icc, _ := io.ReadAll(io.LimitReader(r, 8<<20))
			return icc
		case "IDAT", "IEND":
			return nil // iCCP must come before the image data
		}
		i += 12 + n
	}
	return nil
}

// --- profile parsing --------------------------------------------------------

type rgbProfile struct {
	m   [3][3]float64            // linear RGB -> XYZ (D50), columns are the colorants
	trc [3]func(float64) float64 // encoded -> linear, per channel
}

func (p *rgbProfile) lut(c int) []float64 {
	t := make([]float64, 65536)
	for i := range t {
		t[i] = clamp01(p.trc[c](float64(i) / 65535))
	}
	return t
}

// sRGB's colorants (adapted to D50, as ICC profiles store them) and tone curve.
var srgbM = [3][3]float64{
	{0.4360747, 0.3850649, 0.1430804},
	{0.2225045, 0.7168786, 0.0606169},
	{0.0139322, 0.0971045, 0.7141733},
}

func srgbDecode(v float64) float64 {
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

func srgbEncode(v float64) float64 {
	if v <= 0.0031308 {
		return v * 12.92
	}
	return 1.055*math.Pow(v, 1/2.4) - 0.055
}

// isSRGB: colorants and curves match sRGB closely enough that converting would change nothing.
func (p *rgbProfile) isSRGB() bool {
	for r := range 3 {
		for c := range 3 {
			if math.Abs(p.m[r][c]-srgbM[r][c]) > 0.004 {
				return false
			}
		}
	}
	for c := range 3 {
		for _, x := range []float64{0.1, 0.25, 0.5, 0.75} {
			if math.Abs(p.trc[c](x)-srgbDecode(x)) > 0.004 {
				return false
			}
		}
	}
	return true
}

func s15(b []byte) float64 { return float64(int32(binary.BigEndian.Uint32(b))) / 65536 }

// parseRGBProfile reads colorants and curves from an ICC profile; ok is false for any other
// kind (CMYK, grey, LUT-based).
func parseRGBProfile(icc []byte) (*rgbProfile, bool) {
	if len(icc) < 132 || string(icc[16:20]) != "RGB " {
		return nil, false
	}
	nTags := int(binary.BigEndian.Uint32(icc[128:]))
	if nTags <= 0 || 132+12*nTags > len(icc) {
		return nil, false
	}
	tags := map[string][]byte{}
	for i := range nTags {
		e := icc[132+12*i:]
		off, size := int(binary.BigEndian.Uint32(e[4:])), int(binary.BigEndian.Uint32(e[8:]))
		if off < 0 || size < 0 || off+size > len(icc) {
			return nil, false
		}
		tags[string(e[:4])] = icc[off : off+size]
	}
	p := &rgbProfile{}
	for c, name := range []string{"rXYZ", "gXYZ", "bXYZ"} {
		t := tags[name]
		if len(t) < 20 || string(t[:4]) != "XYZ " {
			return nil, false
		}
		for r := range 3 {
			p.m[r][c] = s15(t[8+4*r:])
		}
	}
	for c, name := range []string{"rTRC", "gTRC", "bTRC"} {
		f, ok := parseCurve(tags[name])
		if !ok {
			return nil, false
		}
		p.trc[c] = f
	}
	return p, true
}

// parseCurve reads an ICC 'curv' or 'para' tone curve (encoded -> linear).
func parseCurve(t []byte) (func(float64) float64, bool) {
	if len(t) < 12 {
		return nil, false
	}
	switch string(t[:4]) {
	case "curv":
		n := int(binary.BigEndian.Uint32(t[8:]))
		switch {
		case n == 0:
			return func(x float64) float64 { return x }, true
		case n == 1 && len(t) >= 14:
			g := float64(binary.BigEndian.Uint16(t[12:])) / 256
			return func(x float64) float64 { return math.Pow(x, g) }, true
		case n > 1 && len(t) >= 12+2*n:
			tab := make([]float64, n)
			for i := range tab {
				tab[i] = float64(binary.BigEndian.Uint16(t[12+2*i:])) / 65535
			}
			return func(x float64) float64 {
				pos := x * float64(n-1)
				i := int(pos)
				if i >= n-1 {
					return tab[n-1]
				}
				return tab[i] + (tab[i+1]-tab[i])*(pos-float64(i))
			}, true
		}
	case "para":
		typ := int(binary.BigEndian.Uint16(t[8:]))
		counts := []int{1, 3, 4, 5, 7}
		if typ < 0 || typ >= len(counts) || len(t) < 12+4*counts[typ] {
			return nil, false
		}
		var q [7]float64
		for i := range counts[typ] {
			q[i] = s15(t[12+4*i:])
		}
		g, a, b, c, d, e, f := q[0], q[1], q[2], q[3], q[4], q[5], q[6]
		switch typ {
		case 0:
			return func(x float64) float64 { return math.Pow(x, g) }, true
		case 1:
			return func(x float64) float64 {
				if a != 0 && x >= -b/a {
					return math.Pow(a*x+b, g)
				}
				return 0
			}, true
		case 2:
			return func(x float64) float64 {
				if a != 0 && x >= -b/a {
					return math.Pow(a*x+b, g) + c
				}
				return c
			}, true
		case 3:
			return func(x float64) float64 {
				if x >= d {
					return math.Pow(a*x+b, g)
				}
				return c * x
			}, true
		default:
			return func(x float64) float64 {
				if x >= d {
					return math.Pow(a*x+b, g) + e
				}
				return c*x + f
			}, true
		}
	}
	return nil, false
}

// --- conversion -------------------------------------------------------------

func invert3(m [3][3]float64) [3][3]float64 {
	det := m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) -
		m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) +
		m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])
	var o [3][3]float64
	o[0][0] = (m[1][1]*m[2][2] - m[1][2]*m[2][1]) / det
	o[0][1] = (m[0][2]*m[2][1] - m[0][1]*m[2][2]) / det
	o[0][2] = (m[0][1]*m[1][2] - m[0][2]*m[1][1]) / det
	o[1][0] = (m[1][2]*m[2][0] - m[1][0]*m[2][2]) / det
	o[1][1] = (m[0][0]*m[2][2] - m[0][2]*m[2][0]) / det
	o[1][2] = (m[0][2]*m[1][0] - m[0][0]*m[1][2]) / det
	o[2][0] = (m[1][0]*m[2][1] - m[1][1]*m[2][0]) / det
	o[2][1] = (m[0][1]*m[2][0] - m[0][0]*m[2][1]) / det
	o[2][2] = (m[0][0]*m[1][1] - m[0][1]*m[1][0]) / det
	return o
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

// convertToSRGB decodes the picture, maps every pixel from the profile to sRGB and encodes it
// again (JPEG keeps its Exif segment, so the orientation still applies). Colours outside sRGB
// are clipped.
func convertToSRGB(data []byte, ext string, p *rgbProfile) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if cfg.Width*cfg.Height > maxColorPixels {
		return nil, io.ErrShortBuffer
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	conv := mul3(invert3(srgbM), p.m)
	lut := [3][]float64{p.lut(0), p.lut(1), p.lut(2)}
	const encSteps = 4095
	enc := make([]uint16, encSteps+1)
	for i := range enc {
		enc[i] = uint16(math.Round(srgbEncode(float64(i)/encSteps) * 65535))
	}
	b := src.Bounds()
	dst := image.NewNRGBA64(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.NRGBA64Model.Convert(src.At(x, y)).(color.NRGBA64)
			r, g, bl := lut[0][c.R], lut[1][c.G], lut[2][c.B]
			var o [3]uint16
			for k := range 3 {
				v := clamp01(conv[k][0]*r + conv[k][1]*g + conv[k][2]*bl)
				o[k] = enc[int(v*encSteps+0.5)]
			}
			dst.SetNRGBA64(x, y, color.NRGBA64{R: o[0], G: o[1], B: o[2], A: c.A})
		}
	}
	var buf bytes.Buffer
	if ext == "png" {
		err = png.Encode(&buf, to8(dst))
		return buf.Bytes(), err
	}
	if err := jpeg.Encode(&buf, to8(dst), &jpeg.Options{Quality: 93}); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if exif := jpegExif(data); exif != nil && len(exif)+2 <= 0xFFFF {
		seg := append([]byte{0xFF, 0xE1, byte((len(exif) + 2) >> 8), byte(len(exif) + 2)}, exif...)
		out = append(append(out[:2:2], seg...), out[2:]...) // after SOI
	}
	return out, nil
}

func to8(img *image.NRGBA64) *image.NRGBA {
	o := image.NewNRGBA(img.Bounds())
	for i := 0; i+8 <= len(img.Pix); i += 8 {
		for k := range 4 {
			v := uint32(img.Pix[i+2*k])<<8 | uint32(img.Pix[i+2*k+1]) // big-endian sample
			o.Pix[i/2+k] = uint8((v*255 + 32767) / 65535)
		}
	}
	return o
}

func mul3(a, b [3][3]float64) [3][3]float64 {
	var o [3][3]float64
	for i := range 3 {
		for j := range 3 {
			for k := range 3 {
				o[i][j] += a[i][k] * b[k][j]
			}
		}
	}
	return o
}
