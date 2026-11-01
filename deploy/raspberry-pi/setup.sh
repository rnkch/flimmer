#!/bin/bash
# Run once on a fresh Raspberry Pi OS (64-bit) before the first deploy.sh:
# installs mpv, ffmpeg and the tools flimmerkiste calls.
set -e

echo "=== Installing dependencies ==="
sudo apt-get update
sudo apt-get install -y \
    mpv \
    ffmpeg \
    alsa-utils \
    libheif-examples \
    curl \
    unzip

echo "=== Installing yt-dlp and deno (URL downloads) ==="
# Official standalone yt-dlp: includes YouTube's challenge scripts (yt-dlp-ejs),
# updates itself (yt-dlp -U, also from the web UI). Deno is the JavaScript
# runtime yt-dlp needs for YouTube. Both go to ~/.local/bin, which the
# service has on its PATH (flimmerkiste.service). Checksums are verified.
BIN=$HOME/.local/bin
mkdir -p "$BIN"
TMP=$(mktemp -d)
curl -fsSL -o "$TMP/yt-dlp_linux_aarch64" https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_linux_aarch64
curl -fsSL -o "$TMP/SHA2-256SUMS" https://github.com/yt-dlp/yt-dlp/releases/latest/download/SHA2-256SUMS
(cd "$TMP" && grep ' yt-dlp_linux_aarch64$' SHA2-256SUMS | sha256sum -c -)
install -m 755 "$TMP/yt-dlp_linux_aarch64" "$BIN/yt-dlp"
curl -fsSL -o "$TMP/deno.zip" https://github.com/denoland/deno/releases/latest/download/deno-aarch64-unknown-linux-gnu.zip
curl -fsSL -o "$TMP/deno.zip.sha256sum" https://github.com/denoland/deno/releases/latest/download/deno-aarch64-unknown-linux-gnu.zip.sha256sum
(cd "$TMP" && echo "$(awk '{print $1}' deno.zip.sha256sum)  deno.zip" | sha256sum -c -)
unzip -o -q "$TMP/deno.zip" -d "$BIN"
chmod 755 "$BIN/deno"
rm -rf "$TMP"

echo "=== Verifying installations ==="
mpv --version | head -3
echo "---"
ffmpeg -version | head -1
echo "---"
"$BIN/yt-dlp" --version
"$BIN/deno" --version | head -1
echo "---"
echo "=== Done ==="
