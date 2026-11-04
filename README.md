# Flimmerkiste

Turn any screen into a looping display, managed from your phone.

Flimmerkiste turns a screen (a TV in a shop window, a monitor in a lobby, an old
CRT) into a display that loops your images, videos and streams. It is a single
Go binary that drives [mpv](https://mpv.io) and keeps its state in SQLite.
You manage it from a phone: scan the QR code on the screen, log in, upload
files or paste links. Videos are converted so the hardware can play them
smoothly. Reorder, rename or add entries while it plays: what is on screen
keeps running. A lock keeps anyone from interrupting playback during an
event. An optional Telegram bot gives remote control.

It grew up on a Raspberry Pi 3B+ driving a PAL CRT over composite, which is
still the best-tested setup ([deploy/raspberry-pi](deploy/raspberry-pi)).

## Features

- Playlist of images (with display time), videos and streams; edited live
  without restarting playback. An entry can be disabled instead of deleted: it
  keeps its place, title and file but is skipped until it is enabled again
- Web UI (German, English): upload files of any size, add links (yt-dlp),
  drag to reorder, player controls, system status, Wi-Fi settings
  (NetworkManager), playback lock
- QR code with the web address on the screen; self-signed HTTPS on the local
  network
- Uploads are checked with ffprobe and re-encoded to H.264 only when the
  device can't play them as they are
- Phone media is made to look right: rotated and non-square-pixel videos are
  scaled by the size as shown, HDR (HLG/PQ, BT.2020) videos are converted to
  BT.709 (needs ffmpeg's `zscale`), and photos with an ICC profile (Display P3,
  Adobe RGB) are converted to sRGB, since the player does no colour management
- Optional Telegram bot (German): uploads, playlist, player controls,
  status, screenshots

## Requirements

Linux with mpv **0.38 or newer** (with DRM/KMS or a desktop session; Debian 13
and Raspberry Pi OS 13 have 0.40; older versions refuse playlist changes and
flimmerkiste says so in its log), ffmpeg/ffprobe,
`heif-convert` for HEIC photos, yt-dlp for links, NetworkManager (`nmcli`)
for the Wi-Fi settings. Building needs Go (see `go/go.mod`).

## Run it

```bash
cd go
go build ./cmd/flimmerkiste
./flimmerkiste -encoder libx264
```

On first start without a password, flimmerkiste generates one and logs it
(`generated the web password`). The web UI listens on
`http://127.0.0.1:8080` and `https://<LAN address>:8443`. State lives in
`~/.local/share/flimmerkiste` (`-data`), media in its `media/` folder (`-media`).
`./flimmerkiste -h` lists all options. `-encoder` defaults to the Raspberry Pi's
hardware encoder (`h264_v4l2m2m`); use `libx264` elsewhere.

Optional `~/.config/flimmerkiste/credentials` (`-credentials`):

```
BOT_TOKEN=<Telegram bot token>
GROUP_ID=<Telegram group the bot answers in>
WEB_PASSWORD=<first web password, change it later in the web UI>
```

A fallback Wi-Fi network `flimmerkiste` / `flimmerkiste` (`-fallback-wifi`) is saved
with the lowest priority: start a phone hotspot with that name and password
to reach a device whose Wi-Fi is gone.

## Raspberry Pi

`deploy/raspberry-pi/setup.sh` prepares a fresh Raspberry Pi OS;
`deploy/raspberry-pi/deploy.sh [user@]host` builds, installs the systemd
service and the boot files (composite PAL by default, HDMI when
`~/.needs-hdmi` exists on the Pi) and restarts flimmerkiste.

## Home screen (PWA)

Every page links a web app manifest (`/manifest.webmanifest`, public) and home-screen icons,
so a browser *can* offer to put the remote on a home screen as an app of its own. The app is
named after the display, so several displays stay apart on one phone. There is deliberately no
service worker: the page is a remote for a screen on the same network, nothing in it works
offline, and cached files would get in the way of the self-upgrade.

Browsers only install a page as an app from a trusted HTTPS origin. flimmerkiste's own
address (`https://<LAN address>:8443`) uses a self-signed certificate, so expect a plain
shortcut instead, or nothing; this hasn't been tried on real phones yet.

The icon is a 4:3 screen full of TV snow in a frame with a thicker bottom border, drawn by
`go run ./internal/web/cmd/pwaicons` (in `go/`). The home-screen PNGs are blue on the night
background. The header, the login page and the favicon use the SVG version, whose frame takes
the colour of the display's name (chosen in the name editor, else from a hash of the name); the
favicon is served in that colour at `/favicon.svg`, with a PNG at `/favicon.ico` as fallback.
A change of the name or colour reaches every open page over the live connection: the header and
the tab icon follow without a reload, and a page that was in the background catches up when it
comes back.

## Releases and upgrades

A release is a Git tag. Pushing one builds the binaries and publishes them
(`.github/workflows/release.yml`):

```bash
git tag v0.2.0 && git push origin v0.2.0
```

Each release has `flimmerkiste-linux-arm64`, `flimmerkiste-linux-amd64` and
`checksums.txt`. A tag with a suffix (`v0.2.0-rc1`) is a pre-release, which
upgrades ignore.

A running flimmerkiste installs the latest release itself: the **Flimmerkiste** card on
the System page, or `/upgrade` in Telegram (`/upgrade jetzt` installs).

- It only asks GitHub (`api.github.com`, so GitHub sees the device's address)
  when someone opens the System page or runs `/upgrade`, never by itself.
- The download is installed only if its SHA-256 matches `checksums.txt` and
  the new binary reports the release's version. The checksums come from the
  same release, so they guard against a broken download, not against someone
  who can publish on the GitHub account.
- The old binary stays next to it as `flimmerkiste.prev`. A new version that
  crashes twice before it has been up for a minute is replaced by the old one
  again. The database isn't rolled back: say so in the release notes when a
  release changes it incompatibly.
- It restarts the systemd service (the screen goes dark for a few seconds),
  and is refused while playback is locked.
- A build from source (`deploy.sh`, `go run`) reports a commit instead of a
  version; any release counts as newer than it.
- `-update-repo owner/name` follows another repository's releases (a fork),
  `-update-repo ""` switches upgrades off.

## Development

```bash
cd go
./dev.sh          # http://127.0.0.1:8090, no Telegram, password flimmerkiste-dev-password
go test ./...     # needs mpv and ffmpeg
```

Code is formatted with [golines](https://github.com/golangci/golines) (100
columns, on top of gofmt); CI checks it, and generated files are skipped:

```bash
go install github.com/golangci/golines@v0.15.0
cd go && golines -w --ignore-generated -m 100 $(git ls-files '*.go')
git config core.hooksPath .githooks   # once: the pre-commit hook checks the staged files
```

Templates are [templ](https://templ.guide) (`go tool templ generate
./internal/web/views`), queries [sqlc](https://sqlc.dev) (`go tool sqlc
generate`). The browser code (htmx, Alpine.js, Web Awesome) is vendored and
bundled with esbuild at startup. Web Awesome components are registered for every `<wa-…>`
tag in the templates, helpers and scripts (`go run ./internal/web/cmd/components`, also run by
`go run ./internal/web/cmd/vendor`); a test fails when `assets/components.js` is out of date.
Components that only scripts create are registered by hand in `assets/components-extra.js`.

## License

GNU General Public License v3.0, see [LICENSE](LICENSE).

Bundled third-party code: [htmx](https://htmx.org) (`vendor/htmx/LICENSE`),
[Alpine.js](https://alpinejs.dev) (MIT),
[Web Awesome](https://webawesome.com) (`vendor/webawesome/LICENSE.md`),
[Font Awesome Free](https://fontawesome.com) icons (`vendor/icons/LICENSE.txt`),
colors from [tokyonight.nvim](https://github.com/folke/tokyonight.nvim)
(Apache-2.0).
