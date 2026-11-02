package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"image"
	"image/color"
	"math"
	"testing"
)

// isBlue tells the frame and the stand from the snow (grey) and the background (dark).
func isBlue(c color.NRGBA) bool { return int(c.B)-int(c.R) > 80 }

func TestIconGeometry(t *testing.T) {
	for _, size := range []int{180, 192, 512} {
		img := render(size, 0)
		s := float64(size)
		thick := thickness * s

		// Bounding box of the frame.
		minX, minY, maxX, maxY := size, size, -1, -1
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				if isBlue(img.NRGBAAt(x, y)) {
					minX, minY = min(minX, x), min(minY, y)
					maxX, maxY = max(maxX, x), max(maxY, y)
				}
			}
		}
		w := float64(maxX - minX + 1)

		// The screen (the noise) is 4:3; the frame around it, with the thicker bottom, is not.
		gMinX, gMinY, gMaxX, gMaxY := size, size, -1, -1
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				if c := img.NRGBAAt(x, y); c.R == c.G && c.G == c.B { // grey: the noise
					gMinX, gMinY = min(gMinX, x), min(gMinY, y)
					gMaxX, gMaxY = max(gMaxX, x), max(gMaxY, y)
				}
			}
		}
		sw, sh := float64(gMaxX-gMinX+1), float64(gMaxY-gMinY+1)
		if ratio := sw / sh; math.Abs(ratio-4.0/3) > 0.04 {
			t.Errorf("%d px: the screen is %.3f:1, want 4:3 (%.0f x %.0f)", size, ratio, sw, sh)
		}
		// A border of t at the top and the sides, 2t at the bottom, over the full width.
		run := func(x, y, dy int) int { // blue pixels from (x, y) in direction dy
			n := 0
			for ; y >= minY && y <= maxY && isBlue(img.NRGBAAt(x, y)); y += dy {
				n++
			}
			return n
		}
		for _, x := range []int{size / 2, minX + int(0.2*w), maxX - int(0.2*w)} {
			if got := run(x, maxY, -1); math.Abs(float64(got)-2*thick) > 2 {
				t.Errorf(
					"%d px: the bottom border at x=%d is %d px, want about %.0f (twice the others)",
					size,
					x,
					got,
					2*thick,
				)
			}
			if got := run(x, minY, +1); math.Abs(float64(got)-thick) > 2 {
				t.Errorf(
					"%d px: the top border at x=%d is %d px, want about %.0f",
					size,
					x,
					got,
					thick,
				)
			}
		}

		// The screen is white noise: grey (no colour) with many different values.
		seen := map[uint8]bool{}
		cx, cy := size/2, size/2-int(0.02*s)
		for y := cy - 15; y < cy+15; y++ {
			for x := cx - 40; x < cx+40; x++ {
				c := img.NRGBAAt(x, y)
				if c.R != c.G || c.G != c.B {
					t.Fatalf("%d px: the screen isn't grey at %d,%d: %v", size, x, y, c)
				}
				seen[c.R] = true
			}
		}
		if len(seen) < 100 {
			t.Errorf(
				"%d px: only %d different grey values on the screen, want white noise",
				size,
				len(seen),
			)
		}
	}
}

// The same icons on every run (a fixed seed), and the corners follow the kind of icon.
func TestIconsAreReproducibleAndCornersFitTheirKind(t *testing.T) {
	a, b := render(192, 0.18), render(192, 0.18)
	if !bytes.Equal(a.Pix, b.Pix) {
		t.Error("two renderings differ: the noise isn't seeded")
	}
	if c := a.NRGBAAt(0, 0); c.A != 0 {
		t.Errorf("a rounded icon has an opaque corner: %v", c)
	}
	if c := render(512, 0).NRGBAAt(0, 0); c != night {
		t.Errorf("a full-bleed icon's corner is %v, want the night background %v", c, night)
	}
	var _ image.Image = a
}

// The SVG is well-formed, has every snow cell, and the frame is in currentColor, so whoever
// embeds it chooses the colour; its viewBox is the frame (thicker at the bottom).
func TestSVG(t *testing.T) {
	doc := svg()
	var parsed struct {
		ViewBox string `xml:"viewBox,attr"`
		Rects   []struct {
			Fill string `xml:"fill,attr"`
		} `xml:"g>rect"`
		Path struct {
			Fill string `xml:"fill,attr"`
		} `xml:"path"`
	}
	if err := xml.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("not well-formed: %v", err)
	}
	if len(parsed.Rects) != svgCols*svgRows {
		t.Errorf("%d snow cells, want %d", len(parsed.Rects), svgCols*svgRows)
	}
	for _, r := range parsed.Rects {
		if len(r.Fill) != 7 || r.Fill[1:3] != r.Fill[3:5] || r.Fill[3:5] != r.Fill[5:7] {
			t.Fatalf("a snow cell isn't grey: %q", r.Fill)
		}
	}
	if parsed.Path.Fill != "currentColor" {
		t.Errorf("the frame is %q, want currentColor", parsed.Path.Fill)
	}
	var w, h float64
	fmt.Sscanf(parsed.ViewBox, "0 0 %g %g", &w, &h)
	if want := (screenW + 2*thickness) / (screenW*3/4 + 3*thickness); math.Abs(w/h-want) > 0.01 {
		t.Errorf("viewBox %q: %.3f:1, want %.3f:1", parsed.ViewBox, w/h, want)
	}
	if svg() != doc {
		t.Error("two runs differ")
	}
}

// The favicon is the icon big on a transparent background.
func TestFavicon(t *testing.T) {
	img := renderFavicon(32)
	if img.Bounds().Dx() != 32 || img.Bounds().Dy() != 32 {
		t.Fatalf("size %v", img.Bounds())
	}
	if c := img.NRGBAAt(0, 0); c.A != 0 {
		t.Errorf("the corner isn't transparent: %v", c)
	}
	var blues, greys int
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			c := img.NRGBAAt(x, y)
			switch {
			case c.A == 0xff && isBlue(c):
				blues++
			case c.A == 0xff && c.R == c.G && c.G == c.B:
				greys++
			}
		}
	}
	if blues < 150 || greys < 150 {
		t.Errorf("%d frame pixels and %d snow pixels: it should fill the canvas", blues, greys)
	}
}
