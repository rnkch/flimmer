// Package web serves the browser UI: templ pages, htmx fragments, an SSE
// stream for live updates and large uploads. All actions go through
// core.Service, like the Telegram bot.
package web

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/rnkch/flimmerkiste/internal/access"
	"github.com/rnkch/flimmerkiste/internal/core"
	"github.com/rnkch/flimmerkiste/internal/web/bundle"
	"github.com/rnkch/flimmerkiste/internal/web/i18n"
	"github.com/rnkch/flimmerkiste/internal/web/views"
)

//go:embed assets vendor
var files embed.FS

const sessionCookie = "flimmerkiste_session"

type Server struct {
	k      *core.Service
	certs  *access.Certs // nil without HTTPS
	bundle bundle.Result
	icons  fs.FS
	pwa    fs.FS
	hues   map[string]string // hue name -> hex, for the favicon (favicon.go)

	// boot is new on every start. Every response carries it (X-Boot) and every live
	// connection (/events) opens with it, so a page can tell that the server has
	// restarted (app.js, "boot" and "serverRestarting").
	boot string

	loginMu  sync.Mutex
	failures int
}

// New bundles the browser assets (esbuild, in memory) and prepares routes.
func New(k *core.Service, certs *access.Certs) (*Server, error) {
	start := time.Now()
	res, err := bundle.Build(files)
	if err != nil {
		return nil, err
	}
	slog.Info(
		"web assets bundled",
		"js",
		res.JS.Name,
		"css",
		res.CSS.Name,
		"took",
		time.Since(start).Round(time.Millisecond),
	)
	icons, _ := fs.Sub(files, "vendor/icons")
	pwa, _ := fs.Sub(files, "assets/pwa")
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	return &Server{
		k: k, certs: certs, bundle: res, icons: icons, pwa: pwa, hues: huePalette(files),
		boot: hex.EncodeToString(id),
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("GET /static/{name}", s.static)
	mux.HandleFunc("POST /lang", s.setLang)
	mux.HandleFunc("GET /static/icons/{name}", s.icon)
	// The web app manifest and its icons are fetched by the browser without cookies,
	// so they are public (like /static).
	mux.HandleFunc("GET /manifest.webmanifest", s.manifest)
	mux.HandleFunc("GET /static/pwa/{name}", s.pwaIcon)
	mux.HandleFunc("GET /favicon.svg", s.favicon)
	mux.HandleFunc("GET /favicon.ico", s.faviconICO)

	app := http.NewServeMux()
	app.HandleFunc("POST /logout", s.logout)
	app.HandleFunc(
		"GET /{$}",
		func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/playlist", http.StatusFound) },
	)
	app.HandleFunc("GET /events", s.events)

	app.HandleFunc("GET /playlist", s.playlistPage)
	app.HandleFunc("GET /playlist/list", s.playlistList)
	app.HandleFunc("GET /playlist/summary", s.playlistSummary)
	app.HandleFunc("POST /media/{id}/move", s.mediaMove)
	app.HandleFunc("POST /media/{id}/rename", s.mediaRename)
	app.HandleFunc("POST /media/{id}/duration", s.mediaDuration)
	app.HandleFunc("POST /media/{id}/play", s.mediaPlay)
	app.HandleFunc("POST /media/{id}/enabled", s.mediaEnabled)
	app.HandleFunc("DELETE /media/{id}", s.mediaDelete)
	app.HandleFunc("GET /media/{id}/thumb", s.mediaThumb)
	app.HandleFunc("GET /media/{id}/large", s.mediaLarge)
	app.HandleFunc("GET /media/{id}/preview", s.mediaPreview)
	app.HandleFunc("GET /media/{id}/row", s.mediaRowGet)
	app.HandleFunc("GET /media/{id}/edit/{field}", s.mediaEdit)
	app.HandleFunc("GET /media/{id}/format", s.mediaFormatDialog)
	app.HandleFunc("POST /media/{id}/format", s.mediaFormatSave)
	app.HandleFunc("POST /media/{id}/download", s.mediaDownload)

	app.HandleFunc("GET /add", s.addPage)
	app.HandleFunc(
		"GET /upload",
		func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/add", http.StatusMovedPermanently) },
	)
	app.HandleFunc("POST /upload", s.upload)
	app.HandleFunc("GET /jobs", s.jobs)
	app.HandleFunc("POST /jobs/{id}/cancel", s.jobCancel)
	app.HandleFunc("GET /url/check", s.urlCheck)
	app.HandleFunc("POST /url/add", s.urlAdd)

	app.Handle(
		"GET /player",
		http.RedirectHandler("/playlist", http.StatusMovedPermanently),
	) // merged into the playlist page
	app.HandleFunc("GET /player/card", s.playerCard)
	app.HandleFunc("POST /player/stream", s.playerStream)
	app.HandleFunc("POST /name", s.nameSave)
	app.HandleFunc("GET /lock", s.lockShow)
	app.HandleFunc("POST /lock", s.lockSave)
	app.HandleFunc("POST /player/seek", s.playerSeek)
	app.HandleFunc("POST /player/{action}", s.playerAction)

	app.HandleFunc("GET /system", s.systemPage)
	app.HandleFunc("GET /system/stats", s.systemStats)
	app.HandleFunc("GET /system/log", s.systemLog)
	app.HandleFunc("GET /system/upgrade", s.upgradeShow)
	app.HandleFunc("POST /system/upgrade", s.upgradeInstall)
	app.HandleFunc("GET /system/ytdlp", s.systemYtDlp)
	app.HandleFunc("POST /system/restart", s.systemRestart)
	app.HandleFunc("GET /qr.png", s.qrPNG)
	app.HandleFunc("POST /system/ytdlp-update", s.ytdlpUpdate)

	app.HandleFunc("GET /settings", s.settingsPage)
	app.HandleFunc("POST /settings", s.settingsSave)
	app.HandleFunc("POST /settings/password", s.passwordSave)
	app.HandleFunc("GET /settings/wifi", s.wifiList)
	app.HandleFunc("POST /settings/wifi", s.wifiSave)
	app.HandleFunc("POST /settings/wifi/connect", s.wifiConnect)
	app.HandleFunc("POST /settings/wifi/delete", s.wifiDelete)

	mux.Handle("/", s.requireLogin(app))

	// Rejects state-changing requests from other sites (Sec-Fetch-Site / Origin).
	h := http.NewCrossOriginProtection().Handler(i18n.Middleware(mux))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Boot", s.boot)
		h.ServeHTTP(w, r)
	})
}

func (s *Server) requireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || !s.k.SessionValid(r.Context(), c.Value) {
			if r.Header.Get("HX-Request") != "" {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	render(
		w,
		r,
		views.Login(s.bundle.CSS.Name, s.bundle.JS.Name, "", !s.k.HasWebPassword(r.Context())),
	)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	// Slow down guessing: each failed attempt adds a second, up to 10.
	s.loginMu.Lock()
	delay := time.Duration(min(s.failures, 10)) * time.Second
	s.loginMu.Unlock()
	time.Sleep(delay)

	token, err := s.k.Login(r.Context(), r.PostFormValue("password"))
	if err != nil {
		s.loginMu.Lock()
		s.failures++
		s.loginMu.Unlock()
		s.k.Logf(r.Context(), slog.LevelWarn, "web login failed", "remote", r.RemoteAddr)
		w.WriteHeader(http.StatusUnauthorized)
		views.Login(s.bundle.CSS.Name, s.bundle.JS.Name, "Wrong password.", errors.Is(err, core.ErrNoWebPassword)).
			Render(r.Context(), w)
		return
	}
	s.loginMu.Lock()
	s.failures = 0
	s.loginMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
		MaxAge: int(core.SessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/playlist", http.StatusSeeOther)
}

// setLang stores the UI language chosen in the top bar and reloads the page.
func (s *Server) setLang(w http.ResponseWriter, r *http.Request) {
	tag, ok := i18n.Parse(r.FormValue("lang"))
	if !ok {
		http.Error(w, "unsupported language", http.StatusBadRequest)
		return
	}
	b, _ := tag.Base()
	http.SetCookie(w, &http.Cookie{
		Name: i18n.Cookie, Value: b.String(), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 365 * 24 * 3600,
	})
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.k.Logout(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	switch name {
	case s.bundle.JS.Name:
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Write(s.bundle.JS.Contents)
	case s.bundle.CSS.Name:
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Write(s.bundle.CSS.Contents)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) icon(w http.ResponseWriter, r *http.Request) {
	// app.js asks for ?v=<bundle hash>: that URL never changes its content.
	if r.URL.Query().Get("v") != "" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	}
	http.ServeFileFS(w, r, s.icons, r.PathValue("name"))
}

// Serve listens on every address in addrs until ctx ends.
// tlsAddr (if set) serves HTTPS with the self-signed certificate.
func (s *Server) Serve(ctx context.Context, addrs []string, tlsAddr string) {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No read/write timeout: uploads and the SSE stream are long-lived.
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	var wg sync.WaitGroup
	if tlsAddr != "" && s.certs != nil {
		wg.Add(2)
		go func() { defer wg.Done(); s.serveTLS(ctx, srv, tlsAddr) }()
		go func() { defer wg.Done(); s.refreshCerts(ctx) }()
	}
	for _, a := range addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.serveOn(ctx, srv, a)
		}()
	}
	wg.Wait()
}

func (s *Server) serveTLS(ctx context.Context, srv *http.Server, addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("web listen", "addr", addr, "err", err)
		return
	}
	tlsLn := tls.NewListener(
		ln,
		&tls.Config{GetCertificate: s.certs.GetCertificate, MinVersion: tls.VersionTLS12},
	)
	slog.Info("web listening (https)", "addr", addr, "url", s.k.AccessURL())
	if err := srv.Serve(tlsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("web serve", "addr", addr, "err", err)
	}
}

// refreshCerts renews the certificate when the host gets a new address.
func (s *Server) refreshCerts(ctx context.Context) {
	// Cheap when nothing changed; often enough that a new Wi-Fi address is
	// covered before anyone scans the QR code shown after switching.
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.certs.Refresh(); err != nil {
				slog.Error("https certificate", "err", err)
			}
		}
	}
}

func (s *Server) serveOn(ctx context.Context, srv *http.Server, addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("web listen", "addr", addr, "err", err)
		return
	}
	slog.Info("web listening", "addr", addr)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("web serve", "addr", addr, "err", err)
	}
}
