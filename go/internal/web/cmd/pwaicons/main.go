// Command pwaicons draws the app's icon (internal/web/assets/pwa/): a screen in 4:3 filled
// with white noise (TV snow), in a frame whose bottom border is twice as thick as the others.
// The screen is the 4:3 part; the frame around it is not.
//
//	icon.svg          the frame in currentColor: the header and login page colour it like the
//	                  kiosk's name, and the server fills in the colour for the favicon
//	favicon-32.png    transparent, the frame in Tokyo Night's blue (the fallback favicon)
//	icon-*.png, apple-touch-icon.png
//	                  home-screen icons: blue on the night background
//
// Everything is drawn here, so it is the same on every run: the noise comes from a fixed seed.
//
// The PNGs are committed; run this only to change them, from go/:
//
//	go run ./internal/web/cmd/pwaicons
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
)

var (
	night = color.NRGBA{0x1a, 0x1b, 0x26, 0xff} // tokyonight night "bg"
	blue  = color.NRGBA{0x7a, 0xa2, 0xf7, 0xff} // tokyonight night "blue"
)

// The file names the manifest and the page head use (handlers.go, layout.templ).
var icons = []struct {
	file  string
	size  int
	round float64 // corner radius as a fraction of the size; 0 = full bleed
}{
	{"icon-192.png", 192, 0.18},       // purpose "any": rounded, transparent corners
	{"icon-512.png", 512, 0.18},       // purpose "any"
	{"icon-maskable-512.png", 512, 0}, // purpose "maskable": the system cuts the shape
	{"apple-touch-icon.png", 180, 0},  // iOS rounds it itself
}

func main() {
	dir := filepath.Join("internal", "web", "assets", "pwa")
	write := func(file string, data []byte) {
		if err := os.WriteFile(filepath.Join(dir, file), data, 0o644); err != nil {
			log.Fatal(err)
		}
	}
	encode := func(img image.Image) []byte {
		var b strings.Builder
		if err := png.Encode(&b, img); err != nil {
			log.Fatal(err)
		}
		return []byte(b.String())
	}
	for _, ic := range icons {
		write(ic.file, encode(render(ic.size, ic.round)))
	}
	write("favicon-32.png", encode(renderFavicon(32)))
	write("icon.svg", []byte(svg()))
}

// Geometry, as fractions of the icon size. A maskable icon has to keep its content inside a
// circle of 80 % of the size; this fits (half the diagonal of the whole drawing is about
// 0.37 of the size).
const (
	screenW   = 0.46 // width of the screen, the noise; its height is 3/4 of it (4:3)
	thickness = 0.05 // the frame's border at the top and the sides; at the bottom it is twice that
	cornerR   = 0.14 // outer corner radius, as a share of the frame's height
	supersamp = 4    // anti-aliasing: samples per pixel and axis
)

// render draws a home-screen icon: size x size pixels on the night background, with rounded
// transparent corners when round > 0.
func render(size int, round float64) *image.NRGBA {
	return draw(size, round, screenW+2*thickness, true)
}

// renderFavicon draws the icon big on a transparent background (a favicon is tiny).
func renderFavicon(size int) *image.NRGBA { return draw(size, 0, 0.94, false) }

// draw draws the frame with the given outer width (a fraction of the size), centred.
func draw(size int, round, width float64, background bool) *image.NRGBA {
	s := float64(size)
	k := width / (screenW + 2*thickness) // scales the whole drawing
	t := thickness * s * k
	iw := screenW * s * k // the screen, 4:3
	ih := iw * 3 / 4
	w, h := iw+2*t, ih+3*t // the frame around it: t at the top and the sides, 2t at the bottom
	x0, y0 := (s-w)/2, (s-h)/2
	ix, iy := x0+t, y0+t
	outerR := cornerR * h
	innerR := math.Max(outerR-t, 0.02*h)

	// White noise in cells of whole pixels (the screen is about 58 cells wide, at least 2 px
	// each), the same size at every position.
	cell := max(2, int(math.Round(iw/58)))
	cols, rows := int(iw)/cell+2, int(ih)/cell+2
	rng := rand.New(rand.NewSource(7))
	snow := make([]uint8, cols*rows)
	for i := range snow {
		snow[i] = uint8(rng.Intn(256))
	}

	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			// The noise value of this pixel's cell (not per sample: cells stay crisp).
			cx, cy := (px-int(ix))/cell, (py-int(iy))/cell
			grey := uint8(0)
			if cx >= 0 && cy >= 0 && cx < cols && cy < rows {
				grey = snow[cy*cols+cx]
			}
			var r, g, b, a float64 // premultiplied sums
			for sy2 := 0; sy2 < supersamp; sy2++ {
				for sx2 := 0; sx2 < supersamp; sx2++ {
					x := float64(px) + (float64(sx2)+0.5)/supersamp
					y := float64(py) + (float64(sy2)+0.5)/supersamp
					if round > 0 && !inRoundRect(x, y, 0, 0, s, s, round*s) {
						continue // outside the rounded icon: transparent
					}
					var c color.NRGBA
					switch {
					case inRoundRect(x, y, x0, y0, w, h, outerR) && !inRoundRect(x, y, ix, iy, iw, ih, innerR):
						c = blue // the frame
					case inRoundRect(x, y, ix, iy, iw, ih, innerR):
						c = color.NRGBA{grey, grey, grey, 0xff} // the snow
					case background:
						c = night
					default:
						continue // transparent
					}
					r, g, b, a = r+float64(c.R), g+float64(c.G), b+float64(c.B), a+255
				}
			}
			if a == 0 {
				continue
			}
			n := float64(supersamp * supersamp)
			img.SetNRGBA(px, py, color.NRGBA{
				R: uint8(math.Round(r / (a / 255))),
				G: uint8(math.Round(g / (a / 255))),
				B: uint8(math.Round(b / (a / 255))),
				A: uint8(math.Round(a / n)),
			})
		}
	}
	return img
}

// The SVG is drawn in a box of the frame's size: 1000 units equal the icon size at scale 1.
const (
	svgCols = 12 // snow cells across the screen (9 down): coarse enough to read as snow at 20 px
	svgRows = 9
)

// svg draws the icon as SVG: the snow as grey cells, then the frame on top in currentColor, so
// whoever embeds it chooses the colour. The viewBox is the frame itself.
func svg() string {
	t := thickness * 1000
	iw := screenW * 1000
	ih := iw * 3 / 4
	w, h := iw+2*t, ih+3*t
	outerR := cornerR * h
	innerR := math.Max(outerR-t, 0.02*h)

	var b strings.Builder
	fmt.Fprintf(&b, "<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 %g %g\">\n", w, h)
	b.WriteString("<g shape-rendering=\"crispEdges\">\n")
	rng := rand.New(rand.NewSource(7))
	cw, ch := iw/svgCols, ih/svgRows
	for r := 0; r < svgRows; r++ {
		for c := 0; c < svgCols; c++ {
			g := rng.Intn(256)
			// A little over the edges: the frame covers it, and no seam shows between cells.
			fmt.Fprintf(
				&b,
				"<rect x=\"%.2f\" y=\"%.2f\" width=\"%.2f\" height=\"%.2f\" fill=\"#%02x%02x%02x\"/>\n",
				t+float64(c)*cw,
				t+float64(r)*ch,
				cw+0.4,
				ch+0.4,
				g,
				g,
				g,
			)
		}
	}
	b.WriteString("</g>\n")
	fmt.Fprintf(&b, "<path fill=\"currentColor\" fill-rule=\"evenodd\" d=\"%s %s\"/>\n",
		roundRectPath(0, 0, w, h, outerR), roundRectPath(t, t, iw, ih, innerR))
	b.WriteString("</svg>\n")
	return b.String()
}

// roundRectPath is a closed path around the rectangle x, y, w, h with corner radius r.
func roundRectPath(x, y, w, h, r float64) string {
	return fmt.Sprintf(
		"M%.2f %.2fH%.2fA%.2f %.2f 0 0 1 %.2f %.2fV%.2fA%.2f %.2f 0 0 1 %.2f %.2fH%.2f"+
			"A%.2f %.2f 0 0 1 %.2f %.2fV%.2fA%.2f %.2f 0 0 1 %.2f %.2fZ",
		x+r,
		y,
		x+w-r,
		r,
		r,
		x+w,
		y+r,
		y+h-r,
		r,
		r,
		x+w-r,
		y+h,
		x+r,
		r,
		r,
		x,
		y+h-r,
		y+r,
		r,
		r,
		x+r,
		y,
	)
}

// inRoundRect reports whether (px, py) lies in the rectangle x, y, w, h with corner radius r.
func inRoundRect(px, py, x, y, w, h, r float64) bool {
	if px < x || py < y || px > x+w || py > y+h {
		return false
	}
	r = math.Min(r, math.Min(w, h)/2)
	dx := math.Max(math.Max(x+r-px, px-(x+w-r)), 0)
	dy := math.Max(math.Max(y+r-py, py-(y+h-r)), 0)
	return dx*dx+dy*dy <= r*r
}
