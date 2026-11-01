// Command flimmer turns a screen into a looping display: mpv, the ingest
// queue, the Telegram bot and the web UI in one process.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/rnkch/flimmer/internal/access"
	"github.com/rnkch/flimmer/internal/bot"
	"github.com/rnkch/flimmer/internal/kiosk"
	"github.com/rnkch/flimmer/internal/mpv"
	"github.com/rnkch/flimmer/internal/store"
	"github.com/rnkch/flimmer/internal/web"
)

func main() {
	home, _ := os.UserHomeDir()
	var (
		dataDir   = flag.String("data", home+"/.local/share/flimmer", "database, temp files, thumbnails and the HTTPS certificate")
		mediaDir  = flag.String("media", "", "media directory (default <data>/media)")
		creds     = flag.String("credentials", home+"/.config/flimmer/credentials", "BOT_TOKEN / GROUP_ID / WEB_PASSWORD file (optional; without a bot token no Telegram)")
		listen    = flag.String("listen", "127.0.0.1:8080,tailscale:8080", "web UI addresses, comma separated; tailscale:PORT = all Tailscale IPs; empty = off")
		listenTLS = flag.String("listen-tls", ":8443", "HTTPS address for the local network (self-signed certificate); empty = off")
		publicURL = flag.String("public-url", "", "address shown in the QR code; default https://<LAN IP>:<port>")
		socket    = flag.String("mpv-socket", "/tmp/flimmer-mpv.sock", "mpv IPC socket")
		mpvArgs   = flag.String("mpv-args", "", "extra mpv arguments, space separated")
		encoder   = flag.String("encoder", "h264_v4l2m2m", "ffmpeg H.264 encoder")
		hdmiFlag  = flag.String("hdmi-flag", home+"/.needs-hdmi", "file whose existence selects HDMI output")
		nmcli     = flag.String("nmcli", "sudo -n nmcli", "NetworkManager CLI for the Wi-Fi settings, space separated; empty = off")
		fallback  = flag.String("fallback-wifi", "flimmer:flimmer", "ssid:password saved once as the lowest-priority Wi-Fi (e.g. a phone hotspot); empty = none")
		debugLog  = flag.Bool("v", false, "debug logging")
	)
	flag.Parse()
	if *mediaDir == "" {
		*mediaDir = filepath.Join(*dataDir, "media")
	}

	// Logs go to stderr; under systemd that is the journal, which stamps and
	// rotates them (journalctl -u flimmer). The journal has its own
	// timestamps, so slog's are dropped there.
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if *debugLog {
		opts.Level = slog.LevelDebug
	}
	if os.Getenv("JOURNAL_STREAM") != "" {
		opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				return slog.Attr{}
			}
			return a
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, opts)))

	if err := run(*mediaDir, *dataDir, *creds, *listen, *listenTLS, *publicURL, *socket, *mpvArgs, *encoder, *hdmiFlag, *nmcli, *fallback); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(mediaDir, dataDir, credsPath, listen, listenTLS, publicURL, socket, mpvArgs, encoder, hdmiFlag, nmcli, fallbackWifi string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tmpDir := filepath.Join(dataDir, "tmp")
	thumbDir := filepath.Join(dataDir, "thumbs")
	for _, d := range []string{mediaDir, dataDir, tmpDir, thumbDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	st, err := store.Open(ctx, filepath.Join(dataDir, "flimmer.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	_, hdmiErr := os.Stat(hdmiFlag)
	cfg := kiosk.Config{
		MediaDir:     mediaDir,
		TmpDir:       tmpDir,
		ThumbDir:     thumbDir,
		HDMI:         hdmiErr == nil,
		FFmpeg:       "ffmpeg",
		FFprobe:      "ffprobe",
		HeifConvert:  "heif-convert",
		YtDlp:        "yt-dlp",
		TLSAddr:      listenTLS,
		PublicURL:    publicURL,
		VideoEncoder: encoder,
		LockFile:     filepath.Join(dataDir, "locked"),
		Nmcli:        strings.Fields(nmcli),
		FallbackWifi: fallbackWifi,
	}
	player := &mpv.Player{
		Bin:    "mpv",
		Socket: socket,
		Args:   append([]string{"--loop-playlist=inf", "--image-display-duration=5"}, strings.Fields(mpvArgs)...),
	}
	k := kiosk.New(cfg, st, player)
	k.SyncLockFile(ctx)
	slog.Info("flimmer starting", "media", mediaDir, "hdmi", cfg.HDMI)

	creds := map[string]string{}
	if credsPath != "" {
		if creds, err = readCredentials(credsPath); errors.Is(err, os.ErrNotExist) {
			creds = map[string]string{} // no file: web UI only
		} else if err != nil {
			return err
		}
	}
	// The first web password comes from the credentials file (or, for local
	// runs, FLIMMER_WEB_PASSWORD); later changes are made in the web UI.
	// Without one, a random password is generated and logged once.
	pw := creds["WEB_PASSWORD"]
	if pw == "" {
		pw = os.Getenv("FLIMMER_WEB_PASSWORD")
	}
	if !k.HasWebPassword(ctx) {
		if pw != "" {
			if err := k.SetWebPassword(ctx, pw); err != nil {
				return fmt.Errorf("WEB_PASSWORD: %w", err)
			}
		} else if pw, err = k.ResetWebPassword(ctx); err != nil {
			return fmt.Errorf("web password: %w", err)
		} else {
			slog.Warn("generated the web password; change it in Settings", "password", pw)
		}
	}

	done := make(chan struct{}, 4)
	go func() { player.Run(ctx); done <- struct{}{} }()
	go func() { k.RunWorker(ctx); done <- struct{}{} }()
	go k.EnsureFallbackWifi(ctx)
	go k.YtDlpVersion(ctx) // takes ~5 s on a Pi 3B+; cached for the System page
	n := 2

	if creds["BOT_TOKEN"] != "" || creds["GROUP_ID"] != "" {
		group, err := strconv.ParseInt(creds["GROUP_ID"], 10, 64)
		if creds["BOT_TOKEN"] == "" || err != nil {
			return fmt.Errorf("%s: BOT_TOKEN and GROUP_ID required", credsPath)
		}
		b, err := bot.New(creds["BOT_TOKEN"], group, k)
		if err != nil {
			return fmt.Errorf("telegram: %w", err)
		}
		go func() { b.Run(ctx); done <- struct{}{} }()
		n++
	}

	if listen != "" || listenTLS != "" {
		var certs *access.Certs
		if listenTLS != "" {
			if certs, err = access.NewCerts(filepath.Join(dataDir, "tls")); err != nil {
				return fmt.Errorf("https certificate: %w", err)
			}
		}
		ws, err := web.New(k, certs)
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}
		var addrs []string
		if listen != "" {
			addrs = strings.Split(listen, ",")
		}
		go func() { ws.Serve(ctx, addrs, listenTLS); done <- struct{}{} }()
		n++
	}

	<-ctx.Done()
	slog.Info("shutting down")
	for ; n > 0; n-- {
		<-done
	}
	return nil
}

// readCredentials reads the KEY=VALUE file shared with the old config.py.
func readCredentials(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	vals := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			vals[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return vals, sc.Err()
}
