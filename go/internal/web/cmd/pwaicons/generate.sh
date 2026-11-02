#!/bin/bash
# Renders the home-screen icons (assets/pwa/*.png) from the Font Awesome "tv" glyph the
# header already uses: Tokyo Night blue on the night background. Needs rsvg-convert
# (brew install librsvg). Run from go/internal/web; the PNGs are committed, this is only
# for changing them.
#
#   icon-192.png, icon-512.png     "any" icons, rounded corners
#   icon-maskable-512.png          full bleed, glyph inside the safe zone (a circle of 80 %)
#   apple-touch-icon.png           180 px, full bleed: iOS rounds it itself
set -euo pipefail
cd "$(dirname "$0")/../.."
BG='#1a1b26'   # tokyonight night "bg"
FG='#7aa2f7'   # tokyonight night "blue"
GLYPH_W=576 GLYPH_H=512
PATH_D=$(sed -E 's/.*<path[^>]* d="([^"]+)".*/\1/' vendor/icons/tv.svg)

# icon <size> <corner radius in % of the size> <output>
icon() {
    local size=$1 round=$2 out=$3
    local w; w=$(echo "$size * 0.56" | bc -l)            # glyph width: fits the maskable safe zone
    local k;  k=$(echo "$w / $GLYPH_W" | bc -l)
    local x;  x=$(echo "($size - $w) / 2" | bc -l)
    local y;  y=$(echo "($size - $GLYPH_H * $k) / 2" | bc -l)
    local r;  r=$(echo "$size * $round / 100" | bc -l)
    rsvg-convert -w "$size" -h "$size" -o "assets/pwa/$out" <<SVG
<svg xmlns="http://www.w3.org/2000/svg" width="$size" height="$size" viewBox="0 0 $size $size">
  <rect width="$size" height="$size" rx="$r" fill="$BG"/>
  <g transform="translate($x $y) scale($k)"><path fill="$FG" d="$PATH_D"/></g>
</svg>
SVG
}

icon 192 18 icon-192.png
icon 512 18 icon-512.png
icon 512 0 icon-maskable-512.png
icon 180 0 apple-touch-icon.png
