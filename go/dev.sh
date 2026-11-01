#!/bin/bash
# Runs flimmer locally for development: no Telegram, sample media,
# web UI on http://127.0.0.1:8090 with the local test password below.
# Not for the Pi (deploy.sh builds and ships the real binary).
set -e
cd "$(dirname "$0")"
DIR=${FLIMMER_DEV_DIR:-/tmp/flimmer-dev}
mkdir -p "$DIR/media" "$DIR/samples"
# Sample files to drag into the web UI (Add page or onto the playlist).
if [ -z "$(ls -A "$DIR/samples")" ]; then
    ffmpeg -v error -f lavfi -i "testsrc=s=720x576:d=1" -frames:v 1 "$DIR/samples/testcard.png"
    ffmpeg -v error -f lavfi -i "color=c=navy:s=720x576" -frames:v 1 "$DIR/samples/poster.jpg"
    ffmpeg -v error -f lavfi -i "testsrc2=s=400x300:d=20:r=25" -f lavfi -i "sine=d=20" \
        -c:v libx264 -pix_fmt yuv420p -c:a aac "$DIR/samples/video-demo.mkv"
fi
echo "Sample media to add: $DIR/samples"
# Wi-Fi settings run against a fake nmcli with a few sample networks in range.
export FAKE_NMCLI_DIR="$DIR/wifi"
mkdir -p "$FAKE_NMCLI_DIR"
[ -e "$FAKE_NMCLI_DIR/scan" ] || printf '%s\n' 'Kino Foyer:78:WPA2' 'Kino Foyer:41:WPA2' 'Café\:Gast:52:' \
    'Büro 5G:33:WPA2 WPA3' 'Uni-Netz:60:WPA2 802.1X' 'Nachbar:18:WPA3' >"$FAKE_NMCLI_DIR/scan"
go tool templ generate ./internal/web/views >/dev/null
# Local test password only; otherwise WEB_PASSWORD in the credentials file
# (or a generated one, logged at the first start).
export FLIMMER_WEB_PASSWORD=${FLIMMER_WEB_PASSWORD:-flimmer-dev-password}
exec go run ./cmd/flimmer -credentials "" -listen 127.0.0.1:8090 -listen-tls 127.0.0.1:8453 \
    -media "$DIR/media" -data "$DIR/data" -mpv-socket "$DIR/mpv.sock" \
    -encoder libx264 -nmcli "bash internal/kiosk/testdata/fake-nmcli" -mpv-args "${MPV_ARGS:---no-config --vo=null --ao=null}" "$@"
