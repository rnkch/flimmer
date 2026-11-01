#!/bin/bash
# ./deploy.sh [user@]host [--force]
# Builds flimmer for the Pi (linux/arm64), installs it with its systemd unit,
# mpv and boot settings, and restarts the service. The user defaults to pi.
# Output: composite (PAL) unless ~/.needs-hdmi exists on the Pi.
# Refuses while playback is locked (web UI or Telegram /lock); --force
# deploys anyway. Run setup.sh on the Pi once before the first deploy.

set -e
TARGET="$1"
FORCE="$2"
if [ -z "$TARGET" ] || { [ -n "$FORCE" ] && [ "$FORCE" != "--force" ]; }; then
    echo "Usage: ./deploy.sh [user@]host [--force]"
    exit 1
fi
case "$TARGET" in *@*) ;; *) TARGET="pi@$TARGET" ;; esac

cd "$(dirname "$0")"
ROOT=$(git rev-parse --show-toplevel)

STATUS=$(git -C "$ROOT" status --porcelain)
if [ -n "$STATUS" ]; then
    echo "❌ Working directory is dirty. Commit changes first:"
    echo "$STATUS"
    exit 1
fi

# A deploy restarts playback: don't while it's locked.
if [ "$FORCE" != "--force" ] && ssh "$TARGET" 'test -e ~/.local/share/flimmer/locked'; then
    echo "🔒 $TARGET is locked (playback must not be interrupted). Unlock it in the web UI or with /lock,"
    echo "   or deploy anyway: ./deploy.sh $TARGET --force"
    exit 1
fi

BUILD=$(mktemp -d)
trap 'rm -rf "$BUILD"' EXIT
echo "🔨 Building flimmer $(git -C "$ROOT" rev-parse --short HEAD) for linux/arm64..."
(cd "$ROOT/go" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$BUILD/flimmer" ./cmd/flimmer)

echo "📤 Uploading to $TARGET..."
ssh "$TARGET" 'rm -rf /tmp/flimmer-deploy && mkdir /tmp/flimmer-deploy'
scp -q "$BUILD/flimmer" flimmer.service journald-flimmer.conf boot/* "$TARGET:/tmp/flimmer-deploy/"
scp -q mpv/composite.conf "$TARGET:/tmp/flimmer-deploy/mpv.conf.composite"
scp -q mpv/hdmi.conf "$TARGET:/tmp/flimmer-deploy/mpv.conf.hdmi"

echo "📦 Installing..."
ssh "$TARGET" 'bash -s' <<'EOF'
set -e
D=/tmp/flimmer-deploy
if [ -f ~/.needs-hdmi ]; then MODE=hdmi; else MODE=composite; fi
echo "   output: $MODE"

# Binary: install under a new name and rename, a running binary can't be overwritten.
mkdir -p ~/.local/bin ~/.config/mpv
install -m 755 $D/flimmer ~/.local/bin/flimmer.new
mv ~/.local/bin/flimmer.new ~/.local/bin/flimmer
cp $D/mpv.conf.$MODE ~/.config/mpv/mpv.conf

# Boot files. cmdline.txt keeps this Pi's own root partition, cloud-init
# instance and Wi-Fi country (a changed instance id would make cloud-init
# set the Pi up again).
KEEP=$(tr ' ' '\n' < /boot/firmware/cmdline.txt | grep -E '^(root|ds|cfg80211\.ieee80211_regdom)=' | tr '\n' ' ')
case "$KEEP" in *root=*) ;; *) echo "❌ no root= in /boot/firmware/cmdline.txt"; exit 1 ;; esac
printf '%s%s\n' "$KEEP" "$(cat $D/cmdline.txt.$MODE)" > $D/cmdline.txt
if ! cmp -s $D/cmdline.txt /boot/firmware/cmdline.txt || ! cmp -s $D/config.txt.$MODE /boot/firmware/config.txt; then
    sudo cp --no-preserve=ownership $D/cmdline.txt /boot/firmware/cmdline.txt
    sudo cp --no-preserve=ownership $D/config.txt.$MODE /boot/firmware/config.txt
    echo "   boot files changed: they take effect after a reboot"
fi

# systemd unit, for this user and home directory.
sed -e "s|^User=.*|User=$(id -un)|" -e "s|/home/pi|$HOME|g" $D/flimmer.service | sudo tee /etc/systemd/system/flimmer.service >/dev/null
sudo systemctl daemon-reload
sudo systemctl enable --quiet flimmer.service
# journald only needs a restart when its settings changed.
sudo mkdir -p /etc/systemd/journald.conf.d
if ! cmp -s $D/journald-flimmer.conf /etc/systemd/journald.conf.d/50-flimmer.conf; then
    sudo cp $D/journald-flimmer.conf /etc/systemd/journald.conf.d/50-flimmer.conf
    sudo systemctl restart systemd-journald && sudo journalctl --flush
    echo "   journal settings updated"
fi

echo "🔄 Restarting..."
sudo systemctl restart flimmer.service
sudo systemctl status flimmer.service --no-pager
rm -rf $D
EOF

echo "✅ Deployed $(git -C "$ROOT" rev-parse --short HEAD) to $TARGET"
