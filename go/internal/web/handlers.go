package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/a-h/templ"

	"github.com/rnkch/flimmerkiste/internal/access"
	"github.com/rnkch/flimmerkiste/internal/kiosk"
	"github.com/rnkch/flimmerkiste/internal/store/db"
	"github.com/rnkch/flimmerkiste/internal/sysinfo"
	"github.com/rnkch/flimmerkiste/internal/web/i18n"
	"github.com/rnkch/flimmerkiste/internal/web/views"
)

// --- helpers ----------------------------------------------------------------

func (s *Server) page(r *http.Request, title, active string) views.Page {
	return views.Page{
		Title:  title,
		Active: active,
		JS:     s.bundle.JS.Name,
		CSS:    s.bundle.CSS.Name,
		Now:    s.nowPlaying(r.Context()),
		Name: s.k.DisplayName(
			r.Context(),
		),
		NameSet: s.k.Setting(r.Context(), kiosk.SettingDisplayName),
		Hue:     s.k.DisplayHue(r.Context()),
		Locked:  s.k.Locked(r.Context()),
	}
}

func (s *Server) nowPlaying(ctx context.Context) views.NowPlaying {
	ps := s.k.PlayerState()
	n := views.NowPlaying{
		Paused:    ps.Pause,
		Streaming: ps.Streaming,
		TimePos:   ps.TimePos,
		Duration:  ps.Duration,
		Title:     ps.Title,
	}
	if ps.Path == "" {
		n.Idle = true
		return n
	}
	n.Seekable = ps.Streaming == "" && ps.Duration > 0
	if m, ok := s.current(ctx); ok {
		n.Title, n.Position = m.Title, m.Position
		n.TimePos, n.Duration = s.k.Progress(m, ps)
		n.Seekable = n.Seekable && m.Kind != "image"
	}
	return n
}

// current is the playlist entry mpv is showing, if any.
func (s *Server) current(ctx context.Context) (db.Media, bool) {
	path := s.k.PlayerState().Path
	list, _ := s.k.List(ctx)
	for _, m := range list {
		if s.k.MediaPath(m) == path {
			return m, true
		}
	}
	return db.Media{}, false
}

func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		slog.Error("render", "path", r.URL.Path, "err", err)
	}
}

// toast shows a notification in the browser (wa-toast, see assets/app.js);
// extra names further events to trigger, e.g. "jobsChanged".
func toast(w http.ResponseWriter, variant, msg string, extra ...string) {
	events := map[string]any{"showToast": map[string]string{"message": msg, "variant": variant}}
	for _, e := range extra {
		events[e] = true
	}
	b, _ := json.Marshal(events)
	w.Header().Set("HX-Trigger", asciiJSON(b))
}

// asciiJSON escapes non-ASCII as \uXXXX: browsers read header values as
// Latin-1, so "ü" would arrive garbled.
func asciiJSON(b []byte) string {
	var sb strings.Builder
	for _, r := range string(b) {
		switch {
		case r < 0x80:
			sb.WriteRune(r)
		case r > 0xFFFF:
			r1, r2 := utf16.EncodeRune(r)
			fmt.Fprintf(&sb, "\\u%04x\\u%04x", r1, r2)
		default:
			fmt.Fprintf(&sb, "\\u%04x", r)
		}
	}
	return sb.String()
}

// fail reports an error to the browser without swapping anything.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	toast(w, "danger", errText(r.Context(), err))
	w.Header().Set("HX-Reswap", "none")
	w.WriteHeader(http.StatusUnprocessableEntity)
}

// userError is an error whose text is already translated.
type userError string

func (e userError) Error() string { return string(e) }

// errText translates the kiosk's known errors; others pass through as they are.
func errText(ctx context.Context, err error) string {
	var ue userError
	switch {
	case errors.As(err, &ue):
		return string(ue)
	case errors.Is(err, kiosk.ErrPasswordTooShort):
		return i18n.T(ctx, "The password needs at least 8 characters")
	case errors.Is(err, kiosk.ErrEmptyTitle):
		return i18n.T(ctx, "The title must not be empty")
	case errors.Is(err, kiosk.ErrBadDuration):
		return i18n.T(ctx, "The display time must be a positive number")
	case errors.Is(err, kiosk.ErrCannotMove):
		return i18n.T(ctx, "Can't move any further")
	case errors.Is(err, kiosk.ErrNotFound), errors.Is(err, sql.ErrNoRows):
		return i18n.T(ctx, "Not found")
	case errors.Is(err, kiosk.ErrPlaylist):
		return i18n.T(ctx, "Playlists aren't supported, use a single video")
	case errors.Is(err, kiosk.ErrLive):
		return i18n.T(ctx, "Live streams can't be downloaded; play them directly")
	case errors.Is(err, kiosk.ErrNoVideo):
		return i18n.T(ctx, "No video found at this URL")
	case errors.Is(err, kiosk.ErrBadHue):
		return i18n.T(ctx, "Unknown color")
	case errors.Is(err, kiosk.ErrNameTooLong):
		return i18n.T(ctx, "The name can have at most %d characters", kiosk.DisplayNameMax)
	case errors.Is(err, kiosk.ErrLocked):
		return i18n.T(ctx, "Locked: this would interrupt playback. Unlock it at the top.")
	case errors.Is(err, kiosk.ErrDisabled):
		return i18n.T(ctx, "This entry is disabled; enable it first")
	case errors.Is(err, kiosk.ErrUpgradeOff):
		return i18n.T(ctx, "Updates are switched off")
	case errors.Is(err, kiosk.ErrNoRelease):
		return i18n.T(ctx, "No release has been published yet")
	case errors.Is(err, kiosk.ErrUpToDate):
		return i18n.T(ctx, "Already up to date")
	case errors.Is(err, kiosk.ErrUpgradeBusy):
		return i18n.T(ctx, "An update is already running")
	case errors.Is(err, kiosk.ErrNoAsset):
		return i18n.T(ctx, "The release has no download for this device")
	case errors.Is(err, kiosk.ErrChecksum):
		return i18n.T(ctx, "The download doesn't match its checksum; nothing was changed")
	case errors.Is(err, kiosk.ErrUpgradeCheck):
		return i18n.T(ctx, "The downloaded program doesn't work; nothing was changed")
	case errors.Is(err, kiosk.ErrNotSeekable):
		return i18n.T(ctx, "This can't jump to a position")
	case errors.Is(err, kiosk.ErrJobFinished):
		return i18n.T(ctx, "Already finished")
	case errors.Is(err, kiosk.ErrWifiUnavailable):
		return i18n.T(ctx, "Wi-Fi can't be managed on this device")
	case errors.Is(err, kiosk.ErrWifiSSID):
		return i18n.T(ctx, "The network name must be 1 to 32 characters")
	case errors.Is(err, kiosk.ErrWifiPassword):
		return i18n.T(
			ctx,
			"The Wi-Fi password must be 8 to 63 characters (or empty for an open network)",
		)
	case errors.Is(err, kiosk.ErrWifiEnterprise):
		return i18n.T(ctx, "Networks with user name and password (802.1X) are not supported")
	case errors.Is(err, kiosk.ErrWifiFallback):
		return i18n.T(ctx, "The fallback network can't be changed here")
	case errors.Is(err, kiosk.ErrWifiFallbackSSID):
		return i18n.T(ctx, "This is the fallback network, it is already saved")
	case errors.Is(err, kiosk.ErrWifiActive):
		return i18n.T(ctx, "The Pi is connected through this network right now")
	case errors.Is(err, kiosk.ErrWifiNotSaved):
		return i18n.T(ctx, "This network is not saved")
	}
	return err.Error()
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

// --- playlist ---------------------------------------------------------------

func (s *Server) playlistPage(w http.ResponseWriter, r *http.Request) {
	list, err := s.k.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	cur, _ := s.current(r.Context())
	render(
		w,
		r,
		views.PlaylistPage(
			s.page(r, "Playlist", "playlist"),
			list,
			cur.ID,
			s.activeJobs(r.Context()),
		),
	)
}

func (s *Server) activeJobs(ctx context.Context) int {
	jobs, _ := s.k.ActiveJobs(ctx)
	return len(jobs)
}

// The playlist, its summary line and the player card are rendered here for both the GET
// endpoints (asked for after a held-back update, and by tests) and the live connection,
// which pushes them as <hx-partial>s (events).
func (s *Server) playlistListView(ctx context.Context) (templ.Component, error) {
	list, err := s.k.List(ctx)
	if err != nil {
		return nil, err
	}
	cur, _ := s.current(ctx)
	return views.PlaylistList(list, cur.ID), nil
}

func (s *Server) playlistSummaryView(ctx context.Context) (templ.Component, error) {
	list, err := s.k.List(ctx)
	if err != nil {
		return nil, err
	}
	return views.PlaylistSummary(list, s.activeJobs(ctx)), nil
}

func (s *Server) playlistList(w http.ResponseWriter, r *http.Request) {
	c, err := s.playlistListView(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	render(w, r, c)
}

func (s *Server) playlistSummary(w http.ResponseWriter, r *http.Request) {
	c, err := s.playlistSummaryView(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	render(w, r, c)
}

// mediaRow renders one playlist row; edit is "", "title" or "duration".
func (s *Server) mediaRow(w http.ResponseWriter, r *http.Request, id int64, edit string) {
	m, err := s.k.Get(r.Context(), id)
	if err != nil {
		fail(w, r, err)
		return
	}
	if edit == "duration" && m.Kind != "image" {
		edit = ""
	}
	cur, _ := s.current(r.Context())
	render(w, r, views.MediaRow(m, m.ID == cur.ID, edit))
}

func (s *Server) mediaRowGet(w http.ResponseWriter, r *http.Request) {
	s.mediaRow(w, r, pathID(r), "")
}

func (s *Server) mediaEdit(w http.ResponseWriter, r *http.Request) {
	field := r.PathValue("field")
	if field != "title" && field != "duration" {
		http.NotFound(w, r)
		return
	}
	s.mediaRow(w, r, pathID(r), field)
}

func (s *Server) mediaPreview(w http.ResponseWriter, r *http.Request) {
	m, err := s.k.Get(r.Context(), pathID(r))
	if err != nil {
		fail(w, r, err)
		return
	}
	render(w, r, views.PreviewDialog(m))
}

func (s *Server) mediaLarge(w http.ResponseWriter, r *http.Request) {
	m, err := s.k.Get(r.Context(), pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p, err := s.k.Preview(r.Context(), m)
	if err != nil {
		slog.Warn("preview", "id", m.ID, "err", err)
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, p)
}

func (s *Server) mediaMove(w http.ResponseWriter, r *http.Request) {
	pos, err := strconv.Atoi(r.FormValue("position"))
	if err != nil {
		fail(w, r, userError(i18n.T(r.Context(), "Invalid position")))
		return
	}
	if err := s.k.Move(r.Context(), pathID(r), pos); err != nil {
		fail(w, r, err)
		return
	}
	s.playlistList(w, r)
}

func (s *Server) mediaRename(w http.ResponseWriter, r *http.Request) {
	if err := s.k.Rename(r.Context(), pathID(r), r.FormValue("title")); err != nil {
		fail(w, r, err)
		return
	}
	s.mediaRow(w, r, pathID(r), "")
}

func (s *Server) mediaDuration(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.FormValue("seconds"))
	if err == nil {
		err = s.k.SetImageDuration(r.Context(), pathID(r), n)
	}
	if err != nil {
		fail(w, r, kiosk.ErrBadDuration)
		return
	}
	s.mediaRow(w, r, pathID(r), "")
}

func (s *Server) mediaPlay(w http.ResponseWriter, r *http.Request) {
	m, err := s.k.Get(r.Context(), pathID(r))
	if err == nil {
		err = s.k.PlayIndex(r.Context(), int(m.Position))
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mediaEnabled switches an entry on or off ("on" = true/false) and returns its row.
func (s *Server) mediaEnabled(w http.ResponseWriter, r *http.Request) {
	if err := s.k.SetEnabled(r.Context(), pathID(r), r.FormValue("on") == "true"); err != nil {
		fail(w, r, err)
		return
	}
	s.mediaRow(w, r, pathID(r), "")
}

func (s *Server) mediaDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.k.Delete(r.Context(), pathID(r)); err != nil {
		fail(w, r, err)
		return
	}
	s.playlistList(w, r)
}

func (s *Server) mediaThumb(w http.ResponseWriter, r *http.Request) {
	m, err := s.k.Get(r.Context(), pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p, err := s.k.Thumbnail(r.Context(), m)
	if err != nil {
		slog.Warn("thumbnail", "id", m.ID, "err", err)
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, p)
}

// --- uploads ----------------------------------------------------------------

func (s *Server) addPage(w http.ResponseWriter, r *http.Request) {
	jobs, _ := s.k.RecentJobs(r.Context(), jobListLen)
	free, _, _ := sysinfo.DiskFree(s.k.Config().TmpDir)
	render(
		w,
		r,
		views.AddPage(
			s.page(r, "Add", "add"),
			jobs,
			free,
			s.k.SettingInt(r.Context(), kiosk.SettingImageDurationDflt),
		),
	)
}

// How much the Add page's job list and the System page's log show.
const (
	jobListLen = 10
	logListLen = 20
)

func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	jobs, _ := s.k.RecentJobs(r.Context(), jobListLen)
	render(w, r, views.JobList(jobs))
}

func (s *Server) jobCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.k.CancelJob(r.Context(), pathID(r)); err != nil {
		fail(w, r, err)
		return
	}
	toast(w, "success", i18n.T(r.Context(), "Cancelled, the file is deleted"), "jobsChanged")
}

// upload streams multipart files straight to disk (no size limit besides
// free space), then queues them in the order they were sent.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	tmpDir := s.k.Config().TmpDir
	free, _, err := sysinfo.DiskFree(tmpDir)
	if err == nil && r.ContentLength > 0 && uint64(r.ContentLength)+100<<20 > free {
		http.Error(
			w,
			i18n.T(
				r.Context(),
				"Not enough space: %d MB free, upload is %d MB",
				free>>20,
				r.ContentLength>>20,
			),
			http.StatusInsufficientStorage,
		)
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	type saved struct{ path, name string }
	var got []saved
	cleanup := func() {
		for _, f := range got {
			os.Remove(f.path)
		}
	}
	fields := map[string]string{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			cleanup()
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if part.FileName() == "" {
			b, _ := io.ReadAll(io.LimitReader(part, 64<<10))
			fields[part.FormName()] = string(b)
			continue
		}
		p, err := saveTemp(tmpDir, part)
		if err != nil {
			cleanup()
			http.Error(
				w,
				i18n.T(r.Context(), "Saving failed: %s", err.Error()),
				http.StatusInternalServerError,
			)
			return
		}
		got = append(got, saved{p, filepath.Base(part.FileName())})
	}
	if len(got) == 0 {
		http.Error(w, i18n.T(r.Context(), "No file selected"), http.StatusBadRequest)
		return
	}
	pos, _ := strconv.Atoi(fields["position"])
	secs, _ := strconv.Atoi(fields["seconds"])
	title := strings.TrimSpace(fields["title"])
	for i, f := range got {
		c := kiosk.Caption{Seconds: secs, NoTranscode: fields["no_transcode"] != ""}
		if pos > 0 {
			c.Position = pos + i
		}
		if len(got) == 1 {
			c.Title = title
		}
		if _, err := s.k.EnqueueFile(r.Context(), f.path, f.name, c); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	s.k.Logf(r.Context(), slog.LevelInfo, "web upload", "files", len(got))
	w.WriteHeader(http.StatusNoContent)
}

func saveTemp(dir string, part *multipart.Part) (string, error) {
	b := make([]byte, 6)
	rand.Read(b)
	ext := strings.ToLower(filepath.Ext(part.FileName()))
	p := filepath.Join(dir, "web-"+hex.EncodeToString(b)+ext)
	f, err := os.Create(p)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, part); err != nil {
		f.Close()
		os.Remove(p)
		return "", err
	}
	return p, f.Close()
}

// --- player -----------------------------------------------------------------

func (s *Server) playerCard(w http.ResponseWriter, r *http.Request) {
	render(w, r, views.PlayerCard(s.nowPlaying(r.Context())))
}

// webQRShowFor: whoever clicks the button is already on the web UI; the code
// is only for a second phone, so a short glance is enough.
const webQRShowFor = 10 * time.Second

// playerSeek jumps to the second the player bar was clicked at.
// --- kiosk name in the header -------------------------------------------------

func (s *Server) nameShow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	render(
		w,
		r,
		views.BrandName(
			s.k.DisplayName(ctx),
			s.k.Setting(ctx, kiosk.SettingDisplayName),
			s.k.DisplayHue(ctx),
		),
	)
}

// nameSave stores the name and, when a color dot was clicked, its color
// ("auto" = from the name).
func (s *Server) nameSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := s.k.SetDisplayName(ctx, r.FormValue("name")); err != nil {
		fail(w, r, err)
		return
	}
	if r.Form.Has("hue") {
		hue := r.FormValue("hue")
		if hue == "auto" {
			hue = ""
		}
		if err := s.k.SetDisplayHue(ctx, hue); err != nil {
			fail(w, r, err)
			return
		}
	}
	s.nameShow(w, r)
}

func (s *Server) lockShow(w http.ResponseWriter, r *http.Request) {
	render(w, r, views.LockButton(s.k.Locked(r.Context())))
}

func (s *Server) lockSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	on := r.FormValue("locked") == "true"
	if err := s.k.SetLocked(ctx, on); err != nil {
		fail(w, r, err)
		return
	}
	if on {
		toast(w, "warning", i18n.T(ctx, "Locked: nothing can interrupt playback now"))
	} else {
		toast(w, "success", i18n.T(ctx, "Unlocked"))
	}
	s.lockShow(w, r)
}

func (s *Server) playerSeek(w http.ResponseWriter, r *http.Request) {
	pos, err := strconv.ParseFloat(r.FormValue("pos"), 64)
	if err != nil {
		fail(w, r, userError(i18n.T(r.Context(), "Invalid position")))
		return
	}
	if err := s.k.Seek(r.Context(), pos); err != nil {
		fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) playerAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var err error
	switch r.PathValue("action") {
	case "next":
		err = s.k.Next(ctx)
	case "prev":
		err = s.k.Prev(ctx)
	case "pause":
		err = s.k.TogglePause(ctx)
	case "stats":
		err = s.k.ToggleStats(ctx)
	case "resume":
		err = s.k.ResumePlaylist(ctx)
	case "restart":
		if err = s.k.RestartPlayer(ctx); err == nil {
			toast(
				w,
				"success",
				i18n.T(ctx, "The player restarts; playback starts from the first entry."),
			)
		}
	case "qr":
		shown, qerr := s.k.ToggleQR(ctx, webQRShowFor)
		if qerr == nil {
			msg := i18n.T(ctx, "QR code hidden")
			if shown {
				msg = i18n.T(
					ctx,
					"QR code shown on the screen for %d s",
					int(webQRShowFor.Seconds()),
				)
			}
			toast(w, "success", msg)
		}
		err = qerr
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// qrPNG is the access QR code as an image (System page).
func (s *Server) qrPNG(w http.ResponseWriter, r *http.Request) {
	url := s.k.AccessURL()
	if url == "" {
		http.NotFound(w, r)
		return
	}
	png, err := access.PNG(url, 4)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(png)
}

func (s *Server) playerStream(w http.ResponseWriter, r *http.Request) {
	u := strings.TrimSpace(r.FormValue("url"))
	scheme, _, ok := strings.Cut(u, "://")
	switch strings.ToLower(scheme) {
	case "rtsp", "rtsps", "http", "https", "rtmp", "srt", "udp":
	default:
		ok = false
	}
	if !ok {
		fail(w, r, userError(i18n.T(r.Context(), "Invalid URL (rtsp, http(s), rtmp, srt, udp)")))
		return
	}
	if err := s.k.Stream(r.Context(), u); err != nil {
		fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- system -----------------------------------------------------------------

func (s *Server) systemPage(w http.ResponseWriter, r *http.Request) {
	si := sysinfo.Read(r.Context(), s.k.Config().MediaDir)
	events, _ := s.k.Events(r.Context(), logListLen)
	fp := ""
	if s.certs != nil {
		fp = s.certs.Fingerprint()
	}
	render(
		w,
		r,
		views.SystemPage(s.page(r, "System", "system"), si, s.mediaBytes(r.Context()), events,
			s.k.AccessURL(), fp),
	)
}

func (s *Server) systemRestart(w http.ResponseWriter, r *http.Request) {
	if err := s.k.RestartService(r.Context()); err != nil {
		fail(w, r, err)
		return
	}
	toast(
		w,
		"success",
		i18n.T(r.Context(), "Restarting… the page reconnects by itself in a few seconds."),
		"serverRestarting",
	)
}

// upgradeShow is the Flimmerkiste card: what runs and whether a newer release
// exists. ?check=1 asks GitHub again instead of using the answer from the
// last few minutes.
func (s *Server) upgradeShow(w http.ResponseWriter, r *http.Request) {
	info, err := s.k.CheckUpgrade(r.Context(), r.FormValue("check") != "")
	msg := ""
	if err != nil {
		info = kiosk.UpgradeInfo{Current: kiosk.Version()}
		msg = errText(r.Context(), err)
		if !errors.Is(err, kiosk.ErrUpgradeOff) && !errors.Is(err, kiosk.ErrNoRelease) {
			msg = i18n.T(r.Context(), "Couldn't check for updates: %s", msg)
		}
	}
	render(w, r, views.UpgradeCard(info, msg))
}

// upgradeInstall downloads and installs the latest release; the service then
// restarts, so the page reconnects like after "Restart service".
func (s *Server) upgradeInstall(w http.ResponseWriter, r *http.Request) {
	res, err := s.k.Upgrade(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	if res.Restarted {
		toast(
			w,
			"success",
			i18n.T(r.Context(), "Restarting… the page reconnects by itself in a few seconds."),
			"serverRestarting",
		)
	} else {
		toast(
			w,
			"warning",
			i18n.T(r.Context(), "%s is installed. Restart flimmerkiste to run it.", res.To),
		)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) systemYtDlp(w http.ResponseWriter, r *http.Request) {
	render(w, r, views.YtDlpCard(s.k.YtDlpVersion(r.Context())))
}

func (s *Server) systemStats(w http.ResponseWriter, r *http.Request) {
	render(
		w,
		r,
		views.SystemStats(
			sysinfo.Read(r.Context(), s.k.Config().MediaDir),
			s.mediaBytes(r.Context()),
		),
	)
}

func (s *Server) systemLog(w http.ResponseWriter, r *http.Request) {
	events, _ := s.k.Events(r.Context(), logListLen)
	render(w, r, views.EventLog(events))
}

func (s *Server) mediaBytes(ctx context.Context) int64 {
	list, _ := s.k.List(ctx)
	var n int64
	for _, m := range list {
		n += m.SizeBytes
	}
	return n
}

// --- settings ---------------------------------------------------------------

func (s *Server) settings(ctx context.Context) views.Settings {
	return views.Settings{
		HelpText:      s.k.Setting(ctx, kiosk.SettingHelpText),
		Debug:         s.k.Debug(ctx),
		BitrateK:      s.k.SettingInt(ctx, kiosk.SettingVideoBitrateK),
		ImageDuration: s.k.SettingInt(ctx, kiosk.SettingImageDurationDflt),
		HDMI:          s.k.Config().HDMI,
	}
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	render(w, r, views.SettingsPage(s.page(r, "Settings", "settings"), s.settings(r.Context())))
}

// settingsSave stores one section of the settings page (field "section").
func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	set := func(kv map[string]string) bool {
		for k, v := range kv {
			if err := s.k.SetSetting(ctx, k, v); err != nil {
				fail(w, r, err)
				return false
			}
		}
		return true
	}
	switch r.FormValue("section") {
	case "processing":
		bitrate, err1 := strconv.Atoi(r.FormValue("video_bitrate_k"))
		dur, err2 := strconv.Atoi(r.FormValue("image_duration_default"))
		if err1 != nil || err2 != nil || bitrate < 200 || bitrate > 20000 || dur < 1 || dur > 3600 {
			fail(w, r, userError(i18n.T(ctx, "Invalid values: bitrate 200–20000, duration 1–3600")))
			return
		}
		if !set(
			map[string]string{
				kiosk.SettingVideoBitrateK:     strconv.Itoa(bitrate),
				kiosk.SettingImageDurationDflt: strconv.Itoa(dur),
			},
		) {
			return
		}
	case "display":
		if !set(
			map[string]string{kiosk.SettingDebug: strconv.FormatBool(r.FormValue("debug") != "")},
		) {
			return
		}
	case "help":
		// An empty help text means the built-in default.
		help := strings.TrimSpace(r.FormValue("help_text"))
		var err error
		if help == "" {
			err = s.k.ResetSetting(ctx, kiosk.SettingHelpText)
		} else {
			err = s.k.SetSetting(ctx, kiosk.SettingHelpText, help)
		}
		if err != nil {
			fail(w, r, err)
			return
		}
		toast(w, "success", i18n.T(ctx, "Settings saved"))
		render(w, r, views.HelpForm(s.k.Setting(ctx, kiosk.SettingHelpText)))
		return
	default:
		http.Error(w, "unknown section", http.StatusBadRequest)
		return
	}
	toast(w, "success", i18n.T(ctx, "Settings saved"))
}

// --- Wi-Fi -------------------------------------------------------------------

func (s *Server) wifiList(w http.ResponseWriter, r *http.Request) {
	list, err := s.k.WifiNetworks(r.Context(), r.FormValue("scan") != "")
	msg := ""
	if err != nil {
		msg = errText(r.Context(), err)
	}
	render(w, r, views.WifiList(list, msg))
}

func (s *Server) wifiSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ssid := strings.TrimSpace(r.FormValue("ssid"))
	if err := s.k.SaveWifi(ctx, ssid, r.FormValue("password")); err != nil {
		fail(w, r, err)
		return
	}
	toast(w, "success", i18n.T(ctx, "“%s” saved", ssid))
	render(w, r, views.WifiCard())
}

func (s *Server) wifiConnect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := s.k.ConnectWifi(ctx, r.FormValue("name")); err != nil {
		fail(w, r, err)
		return
	}
	toast(
		w,
		"neutral",
		i18n.T(ctx, "Connecting… The QR code with the new address appears on the screen."),
	)
}

func (s *Server) wifiDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := s.k.DeleteWifi(ctx, r.FormValue("name")); err != nil {
		fail(w, r, err)
		return
	}
	s.wifiList(w, r)
}

func (s *Server) passwordSave(w http.ResponseWriter, r *http.Request) {
	if err := s.k.SetWebPassword(r.Context(), r.FormValue("password")); err != nil {
		fail(w, r, err)
		return
	}
	s.k.Logf(r.Context(), slog.LevelInfo, "web password changed")
	w.Header().Set("HX-Redirect", "/login")
}

// --- live updates (SSE) -----------------------------------------------------

// events streams changes to the browser. Player changes and a 1 s clock while
// something plays send the now-playing bar as an <hx-partial>; other changes
// send named events that fragments listen for (hx-trigger="job from:body").
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ctx := r.Context()
	sub, cancel := s.k.Hub.Subscribe()
	defer cancel()

	send := func(event, data string) bool {
		var b bytes.Buffer
		if event != "" {
			fmt.Fprintf(&b, "event: %s\n", event)
		}
		for _, line := range strings.Split(data, "\n") {
			fmt.Fprintf(&b, "data: %s\n", line)
		}
		b.WriteString("\n")
		if _, err := w.Write(b.Bytes()); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	// Partials whose target is not on the current page are skipped by htmx.
	nowPlaying := func() bool {
		n := s.nowPlaying(ctx)
		var b bytes.Buffer
		b.WriteString(`<hx-partial hx-target="#now-playing" hx-swap="outerMorph">`)
		views.NowPlayingBar(n).Render(ctx, &b)
		b.WriteString(`</hx-partial><hx-partial hx-target="#player-progress" hx-swap="outerMorph">`)
		views.PlayerProgress(n).Render(ctx, &b)
		b.WriteString(`</hx-partial><hx-partial hx-target="#row-progress" hx-swap="outerMorph">`)
		views.RowProgress(n).Render(ctx, &b)
		b.WriteString(`</hx-partial>`)
		return send("", b.String())
	}

	// The player card, the playlist and its summary change with the events below. They are
	// pushed in one message, so every open page gets them without asking (and the parts
	// arrive together). A page without these elements ignores them. Everything is sent
	// once when a connection opens: the page may have missed events while it was away.
	live := func(card, list, summary bool) bool {
		var b bytes.Buffer
		partial := func(target string, c templ.Component, err error) {
			if err != nil {
				slog.Warn("live update", "target", target, "err", err)
				return
			}
			fmt.Fprintf(&b, `<hx-partial hx-target="%s" hx-swap="outerMorph">`, target)
			c.Render(ctx, &b)
			b.WriteString(`</hx-partial>`)
		}
		if card {
			partial("#player-card", views.PlayerCard(s.nowPlaying(ctx)), nil)
		}
		if list {
			c, err := s.playlistListView(ctx)
			partial("#playlist", c, err)
		}
		if summary {
			c, err := s.playlistSummaryView(ctx)
			partial("#playlist-summary", c, err)
		}
		return b.Len() == 0 || send("", b.String())
	}

	// Coalesce bursts (e.g. job progress) into at most one event per type per tick.
	pending := map[kiosk.EventType]bool{}
	clock := time.NewTicker(time.Second)
	defer clock.Stop()
	keepAlive := time.NewTicker(20 * time.Second)
	defer keepAlive.Stop()
	// Every connection starts with the boot id: a page that reconnects after a
	// restart sees another one and reloads (app.js, "boot").
	if !send("boot", s.boot) || !live(true, true, true) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-sub:
			pending[e.Type] = true
		case <-keepAlive.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		case <-clock.C:
			ok := true
			var card, list, summary bool
			for t := range pending {
				switch t {
				case kiosk.EventPlayer:
					ok = ok && nowPlaying()
					card, list = true, true // the current entry moved
				case kiosk.EventPlaylist:
					list, summary = true, true
				case kiosk.EventJob:
					summary = true // the number of jobs in progress
				}
				ok = ok && send(string(t), "1")
			}
			clear(pending)
			ok = ok && live(card, list, summary)
			// Every second while something plays (videos and images alike).
			if ps := s.k.PlayerState(); ok && ps.Path != "" && !ps.Pause {
				ok = nowPlaying()
			}
			if !ok {
				return
			}
		}
	}
}

// --- URL downloads (yt-dlp) -------------------------------------------------

func (s *Server) urlCheck(w http.ResponseWriter, r *http.Request) {
	u := strings.TrimSpace(r.FormValue("url"))
	if kiosk.IsDirectStream(u) {
		render(w, r, views.URLPreview(kiosk.DirectStreamInfo(u)))
		return
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		fail(
			w,
			r,
			userError(i18n.T(r.Context(), "Please enter an http(s), rtsp, rtmp, srt or udp link")),
		)
		return
	}
	info, err := s.k.ProbeURL(r.Context(), u)
	if err != nil {
		fail(w, r, err)
		return
	}
	render(w, r, views.URLPreview(info))
}

// parseFormat reads the "spec size height" value of the format select.
func parseFormat(v string) kiosk.URLFormat {
	f := strings.Fields(v)
	var uf kiosk.URLFormat
	if len(f) > 0 {
		uf.Spec = f[0]
	}
	if len(f) > 1 {
		uf.Size, _ = strconv.ParseInt(f[1], 10, 64)
	}
	if len(f) > 2 {
		uf.Height, _ = strconv.Atoi(f[2])
	}
	return uf
}

// urlAdd takes the add form after a link was checked: the shared name,
// position and duration fields, plus the preview's hidden fields.
func (s *Server) urlAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := r.FormValue("checked_url")
	f := parseFormat(r.FormValue("format"))
	pos, _ := strconv.Atoi(r.FormValue("position"))
	secs, _ := strconv.Atoi(r.FormValue("seconds"))
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		title = r.FormValue("page_title")
	}
	if u == "" {
		fail(w, r, userError(i18n.T(ctx, "Please check the link first")))
		return
	}
	if r.FormValue("mode") == "stream" {
		dur, _ := strconv.ParseFloat(r.FormValue("duration"), 64)
		info := kiosk.URLInfo{
			URL:       u,
			Title:     title,
			Thumbnail: r.FormValue("thumbnail"),
			Duration:  dur,
		}
		if _, err := s.k.AddStream(ctx, info, f, title, pos); err != nil {
			fail(w, r, err)
			return
		}
		toast(w, "success", i18n.T(ctx, "Added to the playlist"), "added")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if f.Spec == "" {
		fail(w, r, userError(i18n.T(ctx, "Please check the link first")))
		return
	}
	if _, err := s.k.EnqueueURL(
		ctx,
		u,
		f.Spec,
		title,
		kiosk.Caption{Position: pos, Seconds: secs, NoTranscode: r.FormValue("no_transcode") != ""},
		f.Size,
	); err != nil {
		fail(w, r, err)
		return
	}
	toast(w, "success", i18n.T(ctx, "Added, downloading"), "jobsChanged", "added")
	w.WriteHeader(http.StatusNoContent)
}

// mediaFormatDialog checks a stream's link again and offers its formats.
func (s *Server) mediaFormatDialog(w http.ResponseWriter, r *http.Request) {
	m, err := s.k.Get(r.Context(), pathID(r))
	if err != nil {
		fail(w, r, err)
		return
	}
	info, err := s.k.ProbeURL(r.Context(), m.URL)
	if err != nil {
		fail(w, r, err)
		return
	}
	current, _, _ := strings.Cut(m.Format, "/")
	if _, ok := info.Format(current); !ok {
		current = info.Auto
	}
	render(w, r, views.FormatDialog(m, info, current))
}

func (s *Server) mediaFormatSave(w http.ResponseWriter, r *http.Request) {
	if err := s.k.SetStreamFormat(
		r.Context(),
		pathID(r),
		parseFormat(r.FormValue("format")),
	); err != nil {
		fail(w, r, err)
		return
	}
	toast(w, "success", i18n.T(r.Context(), "Format changed"))
	w.WriteHeader(http.StatusOK) // empty body removes the dialog
}

func (s *Server) mediaDownload(w http.ResponseWriter, r *http.Request) {
	if _, err := s.k.DownloadInstead(r.Context(), pathID(r)); err != nil {
		fail(w, r, err)
		return
	}
	toast(
		w,
		"success",
		i18n.T(r.Context(), "Downloading; the copy replaces the stream when it's done"),
	)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ytdlpUpdate(w http.ResponseWriter, r *http.Request) {
	msg, err := s.k.UpdateYtDlp(r.Context())
	if err != nil {
		toast(w, "danger", msg)
	} else {
		toast(w, "success", msg)
	}
	render(w, r, views.YtDlpCard(s.k.YtDlpVersion(r.Context())))
}
