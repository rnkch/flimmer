// Package kiosk is the core of the service. The Telegram bot and the web UI
// both call into it; it is the only place that changes playlist state.
package kiosk

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rnkch/flimmerkiste/internal/mpv"
	"github.com/rnkch/flimmerkiste/internal/store"
	"github.com/rnkch/flimmerkiste/internal/store/db"
)

type Config struct {
	MediaDir     string
	TmpDir       string // uploads and downloads land here before ingest
	ThumbDir     string // generated previews for the web UI
	HDMI         bool   // true when ~/.needs-hdmi (-hdmi-flag) exists
	FFmpeg       string
	FFprobe      string
	HeifConvert  string
	YtDlp        string   // yt-dlp binary for URL downloads
	TLSAddr      string   // HTTPS listener of the web UI, e.g. ":8443" ("" = off)
	Nmcli        []string // NetworkManager CLI, e.g. {"sudo", "-n", "nmcli"} (nil = no Wi-Fi management)
	FallbackWifi string   // "ssid:password" saved once as the lowest-priority network ("" = none)
	PublicURL    string   // address to show instead of https://<LAN IP>:<port>
	VideoEncoder string   // h264_v4l2m2m on the Pi
	LockFile     string   // exists while playback is locked (checked by deploy.sh)

	UpdateRepo    string // GitHub "owner/name" whose releases Upgrade installs ("" = updates off)
	UpdateAPI     string // GitHub API address, only set by tests
	ExecPath      string // the binary Upgrade replaces, only set by tests (default: this one)
	UpgradeMarker string // file that watches a new version after an upgrade
}

// Fetcher downloads a Telegram file to dst. Set by the bot.
type Fetcher func(ctx context.Context, fileID, dst string) error

type Service struct {
	cfg    Config
	st     *store.Store
	player *mpv.Player
	Hub    *Hub

	Fetch Fetcher

	plMu      sync.Mutex // serialises playlist changes and mpv sync
	streaming string     // URL while a stream replaces the playlist

	stateMu sync.Mutex
	state   PlayerState
	clock   entryClock

	thumbMu sync.Mutex

	qr      qrState
	upgrade upgradeState

	wake chan struct{}

	jobMu      sync.Mutex
	jobRunning int64              // id of the job the worker is processing
	jobCancel  context.CancelFunc // stops it (CancelJob)
}

func New(cfg Config, st *store.Store, player *mpv.Player) *Service {
	s := &Service{cfg: cfg, st: st, player: player, Hub: NewHub(), wake: make(chan struct{}, 1)}
	player.OnConnect = func(ctx context.Context) {
		s.plMu.Lock()
		s.streaming = ""
		s.plMu.Unlock()
		s.checkMpvVersion(ctx)
		if err := s.Sync(ctx); err != nil {
			slog.Error("initial playlist sync", "err", err)
		}
	}
	player.OnProperty = s.onProperty
	return s
}

func (s *Service) Config() Config { return s.cfg }

// --- player state -----------------------------------------------------------

type PlayerState struct {
	Path          string  `json:"path"`
	Elapsed       float64 `json:"elapsed"` // seconds the current entry has been on screen, pauses excluded
	Title         string  `json:"title"`
	Pause         bool    `json:"pause"`
	PlaylistPos   int     `json:"playlist_pos"`
	PlaylistCount int     `json:"playlist_count"`
	TimePos       float64 `json:"time_pos"`
	Duration      float64 `json:"duration"`
	Streaming     string  `json:"streaming"`
}

func (s *Service) onProperty(name string, data json.RawMessage) {
	s.stateMu.Lock()
	st := &s.state
	now := time.Now()
	switch name {
	case "path":
		st.Path = ""
		json.Unmarshal(data, &st.Path)
		// mpv reports no time for images, so the service keeps the clock.
		s.clock = entryClock{start: now}
		if st.Pause {
			s.clock.pausedAt = now
		}
	case "media-title":
		st.Title = ""
		json.Unmarshal(data, &st.Title)
	case "pause":
		json.Unmarshal(data, &st.Pause)
		if st.Pause && s.clock.pausedAt.IsZero() {
			s.clock.pausedAt = now
		} else if !st.Pause && !s.clock.pausedAt.IsZero() {
			s.clock.paused += now.Sub(s.clock.pausedAt)
			s.clock.pausedAt = time.Time{}
		}
	case "playlist-pos":
		json.Unmarshal(data, &st.PlaylistPos)
	case "playlist-count":
		json.Unmarshal(data, &st.PlaylistCount)
	case "time-pos":
		st.TimePos = 0
		json.Unmarshal(data, &st.TimePos)
	case "duration":
		st.Duration = 0
		json.Unmarshal(data, &st.Duration)
	}
	s.stateMu.Unlock()
	// time-pos changes many times per second; the web UI polls for it instead.
	if name != "time-pos" {
		s.Hub.Publish(Event{Type: EventPlayer})
	}
}

// entryClock measures how long the current entry has been shown.
type entryClock struct {
	start    time.Time
	paused   time.Duration // finished pauses
	pausedAt time.Time     // start of the current pause, zero while playing
}

func (c entryClock) elapsed(now time.Time) float64 {
	if c.start.IsZero() {
		return 0
	}
	end := now
	if !c.pausedAt.IsZero() {
		end = c.pausedAt
	}
	return max(end.Sub(c.start)-c.paused, 0).Seconds()
}

// Progress is the position and length of entry m if it is the one playing:
// mpv's numbers for videos and streams, the service's clock for images
// (wrapping around when a single image repeats). dur is 0 when unknown.
func (s *Service) Progress(m db.Media, ps PlayerState) (pos, dur float64) {
	if m.Kind != "image" {
		return ps.TimePos, ps.Duration
	}
	dur = float64(m.ImageDurationS)
	if dur <= 0 {
		return 0, 0
	}
	return math.Mod(ps.Elapsed, dur), dur
}

func (s *Service) PlayerState() PlayerState {
	s.stateMu.Lock()
	st := s.state
	st.Elapsed = s.clock.elapsed(time.Now())
	s.stateMu.Unlock()
	s.plMu.Lock()
	st.Streaming = s.streaming
	s.plMu.Unlock()
	return st
}

// --- settings ---------------------------------------------------------------

const (
	SettingHelpText          = "help_text"
	SettingDebug             = "debug"
	SettingVideoBitrateK     = "video_bitrate_k"
	SettingImageDurationDflt = "image_duration_default"
	SettingWebPasswordHash   = "web_password_hash"
	SettingDisplayName       = "display_name" // header name in the web UI; "" = hostname
	SettingDisplayHue        = "display_hue"  // its color (one of NameHues); "" = from the name
)

// defaultHelpText is shown until someone saves their own help text.
//
//go:embed help.txt
var defaultHelpText string

var settingDefaults = map[string]string{
	SettingHelpText:          defaultHelpText,
	SettingDebug:             "false",
	SettingVideoBitrateK:     "2000",
	SettingImageDurationDflt: "5",
}

func (s *Service) Setting(ctx context.Context, key string) string {
	v, err := s.st.GetSetting(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return settingDefaults[key]
	}
	if err != nil {
		slog.Error("read setting", "key", key, "err", err)
		return settingDefaults[key]
	}
	return v
}

func (s *Service) SettingInt(ctx context.Context, key string) int {
	n, err := strconv.Atoi(s.Setting(ctx, key))
	if err != nil {
		n, _ = strconv.Atoi(settingDefaults[key])
	}
	return n
}

func (s *Service) SetSetting(ctx context.Context, key, value string) error {
	err := s.st.PutSetting(ctx, db.PutSettingParams{Key: key, Value: value})
	if err == nil {
		s.Hub.Publish(Event{Type: EventSettings})
	}
	return err
}

// ResetSetting drops a stored value, so the default applies again.
func (s *Service) ResetSetting(ctx context.Context, key string) error {
	err := s.st.DeleteSetting(ctx, key)
	if err == nil {
		s.Hub.Publish(Event{Type: EventSettings})
	}
	return err
}

// DisplayNameMax is how long the name in the web UI's header may be.
const DisplayNameMax = 20

var ErrNameTooLong = fmt.Errorf("the name can have at most %d characters", DisplayNameMax)

// DisplayName is the display's name in the web UI header: set there, or the
// hostname (without domain). Only a label: the system hostname stays as it is.
func (s *Service) DisplayName(ctx context.Context) string {
	if n := s.Setting(ctx, SettingDisplayName); n != "" {
		return n
	}
	return Hostname()
}

// Hostname is the short hostname ("lobby" for "lobby.local").
func Hostname() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(h, ".")
	return h
}

// SetDisplayName stores the header name; empty brings back the hostname.
func (s *Service) SetDisplayName(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return s.ResetSetting(ctx, SettingDisplayName)
	case utf8.RuneCountInString(name) > DisplayNameMax:
		return ErrNameTooLong
	}
	return s.SetSetting(ctx, SettingDisplayName, name)
}

// NameHues are the palette's hue families the header name can take; the
// theme has a readable shade of each per color scheme (--kiosk-hue-*).
var NameHues = []string{
	"red",
	"orange",
	"yellow",
	"green",
	"cyan",
	"blue",
	"indigo",
	"purple",
	"pink",
}

var ErrBadHue = errors.New("unknown color")

// HashHue picks a hue from a hash of the name: the automatic color, the same
// for the same name, usually different for different displays.
func HashHue(name string) string {
	h := fnv.New32a()
	h.Write([]byte(name))
	return NameHues[h.Sum32()%uint32(len(NameHues))]
}

// DisplayHue is the chosen color of the header name, "" for automatic.
func (s *Service) DisplayHue(
	ctx context.Context,
) string {
	return s.Setting(ctx, SettingDisplayHue)
}

// SetDisplayHue stores the color; "" goes back to the automatic one.
func (s *Service) SetDisplayHue(ctx context.Context, hue string) error {
	if hue == "" {
		return s.ResetSetting(ctx, SettingDisplayHue)
	}
	if !slices.Contains(NameHues, hue) {
		return ErrBadHue
	}
	return s.SetSetting(ctx, SettingDisplayHue, hue)
}

func (s *Service) Debug(ctx context.Context) bool { return s.Setting(ctx, SettingDebug) == "true" }

// --- activity log -----------------------------------------------------------

// Logf writes to the service log and to the events table shown in the web UI.
func (s *Service) Logf(ctx context.Context, level slog.Level, msg string, args ...any) {
	slog.Log(ctx, level, msg, args...)
	text := msg
	for i := 0; i+1 < len(args); i += 2 {
		text += " " + toString(args[i]) + "=" + toString(args[i+1])
	}
	s.st.InsertEvent(ctx, db.InsertEventParams{Level: level.String(), Message: text})
	s.st.TrimEvents(ctx)
	s.Hub.Publish(Event{Type: EventLog})
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case error:
		return t.Error()
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func (s *Service) Events(ctx context.Context, n int) ([]db.Event, error) {
	return s.st.ListEvents(ctx, int64(n))
}
