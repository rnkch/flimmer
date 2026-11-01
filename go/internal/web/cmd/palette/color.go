package main

import (
	"fmt"
	"math"
	"strconv"
)

// Color math: sRGB <-> OKLab/OKLCH (Björn Ottosson's OKLab), contrast (WCAG 2.1).

type rgb struct{ r, g, b float64 } // gamma-encoded sRGB, 0..1

type oklch struct{ L, C, H float64 } // L 0..1, H degrees

func parseHex(s string) rgb {
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil || len(s) != 7 {
		panic("bad color " + s)
	}
	return rgb{float64(v>>16&0xff) / 255, float64(v>>8&0xff) / 255, float64(v&0xff) / 255}
}

func (c rgb) hex() string {
	q := func(x float64) int { return int(math.Round(math.Min(math.Max(x, 0), 1) * 255)) }
	return fmt.Sprintf("#%02x%02x%02x", q(c.r), q(c.g), q(c.b))
}

func toLinear(x float64) float64 {
	if x <= 0.04045 {
		return x / 12.92
	}
	return math.Pow((x+0.055)/1.055, 2.4)
}

func toGamma(x float64) float64 {
	if x <= 0.0031308 {
		return 12.92 * x
	}
	return 1.055*math.Pow(x, 1/2.4) - 0.055
}

func (c rgb) lch() oklch {
	r, g, b := toLinear(c.r), toLinear(c.g), toLinear(c.b)
	l := math.Cbrt(0.4122214708*r + 0.5363325363*g + 0.0514459929*b)
	m := math.Cbrt(0.2119034982*r + 0.6806995451*g + 0.1073969566*b)
	s := math.Cbrt(0.0883024619*r + 0.2817188376*g + 0.6299787005*b)
	L := 0.2104542553*l + 0.7936177850*m - 0.0040720468*s
	A := 1.9779984951*l - 2.4285922050*m + 0.4505937099*s
	B := 0.0259040371*l + 0.7827717662*m - 0.8086757660*s
	h := math.Atan2(B, A) * 180 / math.Pi
	if h < 0 {
		h += 360
	}
	return oklch{L, math.Hypot(A, B), h}
}

// rgb converts back; ok is false when the color is outside sRGB.
func (c oklch) rgb() (rgb, bool) {
	A := c.C * math.Cos(c.H*math.Pi/180)
	B := c.C * math.Sin(c.H*math.Pi/180)
	l := math.Pow(c.L+0.3963377774*A+0.2158037573*B, 3)
	m := math.Pow(c.L-0.1055613458*A-0.0638541728*B, 3)
	s := math.Pow(c.L-0.0894841775*A-1.2914855480*B, 3)
	r := 4.0767416621*l - 3.3077115913*m + 0.2309699292*s
	g := -1.2684380046*l + 2.6097574011*m - 0.3413193965*s
	b := -0.0041960863*l - 0.7034186147*m + 1.7076147010*s
	const eps = 1e-6
	ok := r >= -eps && r <= 1+eps && g >= -eps && g <= 1+eps && b >= -eps && b <= 1+eps
	return rgb{toGamma(math.Max(r, 0)), toGamma(math.Max(g, 0)), toGamma(math.Max(b, 0))}, ok
}

// inGamut lowers chroma (keeping L and H) until the color fits sRGB.
func (c oklch) inGamut() rgb {
	if out, ok := c.rgb(); ok {
		return out
	}
	lo, hi := 0.0, c.C
	for i := 0; i < 30; i++ {
		mid := (lo + hi) / 2
		if _, ok := (oklch{c.L, mid, c.H}).rgb(); ok {
			lo = mid
		} else {
			hi = mid
		}
	}
	out, _ := oklch{c.L, lo, c.H}.rgb()
	return out
}

func luminance(c rgb) float64 {
	return 0.2126*toLinear(c.r) + 0.7152*toLinear(c.g) + 0.0722*toLinear(c.b)
}

func contrast(a, b rgb) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// mixHue interpolates angles the short way round.
func mixHue(a, b, t float64) float64 {
	d := math.Mod(b-a+540, 360) - 180
	return math.Mod(a+d*t+360, 360)
}
