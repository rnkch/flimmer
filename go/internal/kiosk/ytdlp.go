package kiosk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rnkch/flimmerkiste/internal/store/db"
	"github.com/rnkch/flimmerkiste/internal/sysinfo"
)

var (
	ErrPlaylist = errors.New("playlists are not supported, use a single video")
	ErrLive     = errors.New("live streams can't be downloaded, play them directly")
	ErrNoVideo  = errors.New("no video found at this URL")
)

// URLInfo is what yt-dlp reports about a URL, reduced to what the UI needs.
type URLInfo struct {
	URL       string
	Title     string
	Thumbnail string
	Site      string
	Duration  float64
	Live      bool        // can only be played directly, not downloaded
	Formats   []URLFormat // video formats, lowest resolution first
	Auto      string      // Spec of the format chosen for the CRT
}

// Format returns the format with the given spec (the automatic one if spec is empty).
func (info URLInfo) Format(spec string) (URLFormat, bool) {
	if spec == "" {
		spec = info.Auto
	}
	for _, f := range info.Formats {
		if f.Spec == spec {
			return f, true
		}
	}
	return URLFormat{}, false
}

// URLFormat is one downloadable video format (paired with the best audio
// when the video has none of its own).
type URLFormat struct {
	Spec   string // yt-dlp -f value, e.g. "298+140"
	Height int
	FPS    int
	Codec  string // H.264, VP9, AV1, ...
	Size   int64  // estimated bytes, video + audio; 0 = unknown
	Slow   bool   // the Pi has to decode this codec in software
}

func (f URLFormat) Auto(info URLInfo) bool { return f.Spec == info.Auto }

type ytFormat struct {
	ID             string  `json:"format_id"`
	Ext            string  `json:"ext"`
	VCodec         string  `json:"vcodec"`
	ACodec         string  `json:"acodec"`
	Height         int     `json:"height"`
	FPS            float64 `json:"fps"`
	TBR            float64 `json:"tbr"`
	ABR            float64 `json:"abr"`
	Filesize       int64   `json:"filesize"`
	FilesizeApprox int64   `json:"filesize_approx"`
	Protocol       string  `json:"protocol"`
}

type ytInfo struct {
	Type       string     `json:"_type"`
	Title      string     `json:"title"`
	Thumbnail  string     `json:"thumbnail"`
	Extractor  string     `json:"extractor_key"`
	Duration   float64    `json:"duration"`
	IsLive     bool       `json:"is_live"`
	WebpageURL string     `json:"webpage_url"`
	Formats    []ytFormat `json:"formats"`
}

// ProbeURL asks yt-dlp what can be downloaded from url (nothing is downloaded).
func (s *Service) ProbeURL(ctx context.Context, url string) (URLInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.cfg.YtDlp, "-J", "--no-playlist", "--no-warnings", "--", url).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return URLInfo{}, fmt.Errorf("yt-dlp: %s", lastLine(ee.Stderr))
		}
		return URLInfo{}, fmt.Errorf("yt-dlp: %w", err)
	}
	var yi ytInfo
	if err := json.Unmarshal(out, &yi); err != nil {
		return URLInfo{}, err
	}
	return chooseFormats(url, yi)
}

// chooseFormats builds the format list and picks the automatic choice:
// H.264 (the Pi decodes it in hardware) at the smallest resolution of at
// least 576 lines, so the PAL picture is scaled down, never up.
func chooseFormats(url string, yi ytInfo) (URLInfo, error) {
	if yi.Type == "playlist" || yi.Type == "multi_video" {
		return URLInfo{}, ErrPlaylist
	}
	info := URLInfo{URL: url, Title: yi.Title, Thumbnail: yi.Thumbnail, Site: yi.Extractor, Duration: yi.Duration, Live: yi.IsLive}
	if yi.WebpageURL != "" {
		info.URL = yi.WebpageURL
	}

	size := func(f ytFormat) int64 {
		switch {
		case f.Filesize > 0:
			return f.Filesize
		case f.FilesizeApprox > 0:
			return f.FilesizeApprox
		case f.TBR > 0 && yi.Duration > 0:
			return int64(f.TBR * 125 * yi.Duration) // kbit/s -> bytes
		}
		return 0
	}

	// Best audio-only track: AAC first (copied as is), never YouTube's "-drc" variants.
	var audio *ytFormat
	audioScore := func(f ytFormat) float64 {
		s := f.ABR + f.TBR
		if strings.HasPrefix(f.ACodec, "mp4a") {
			s += 10000
		}
		return s
	}
	for i, f := range yi.Formats {
		if f.VCodec != "none" || f.ACodec == "" || f.ACodec == "none" || strings.Contains(f.ID, "drc") {
			continue
		}
		if audio == nil || audioScore(f) > audioScore(*audio) {
			audio = &yi.Formats[i]
		}
	}

	// One entry per resolution/codec/frame rate; direct downloads beat HLS.
	type key struct {
		h, fps int
		codec  string
	}
	best := map[key]ytFormat{}
	for _, f := range yi.Formats {
		if f.VCodec == "none" || f.Height == 0 || f.Protocol == "mhtml" {
			continue
		}
		k := key{f.Height, int(math.Round(f.FPS)), codecName(f.VCodec)}
		if old, ok := best[k]; !ok || (strings.HasPrefix(old.Protocol, "m3u8") && !strings.HasPrefix(f.Protocol, "m3u8")) {
			best[k] = f
		}
	}
	for k, f := range best {
		uf := URLFormat{Spec: f.ID, Height: k.h, FPS: k.fps, Codec: k.codec, Size: size(f), Slow: k.codec != "H.264"}
		if f.ACodec == "none" || f.ACodec == "" {
			if audio == nil {
				continue // video without any audio track: skip
			}
			uf.Spec += "+" + audio.ID
			if a := size(*audio); a > 0 && uf.Size > 0 {
				uf.Size += a
			}
		}
		info.Formats = append(info.Formats, uf)
	}
	if len(info.Formats) == 0 {
		// Formats without height (e.g. a plain file): let yt-dlp pick.
		if len(yi.Formats) == 0 {
			return URLInfo{}, ErrNoVideo
		}
		info.Formats = []URLFormat{{Spec: "b", Codec: "?"}}
		info.Auto = "b"
		return info, nil
	}
	sort.Slice(info.Formats, func(i, j int) bool {
		a, b := info.Formats[i], info.Formats[j]
		if a.Height != b.Height {
			return a.Height < b.Height
		}
		if a.Slow != b.Slow {
			return !a.Slow
		}
		return a.FPS < b.FPS
	})

	// Automatic choice: smallest fast (H.264) format with at least 576 lines,
	// lower frame rate first; else the largest fast one; else the smallest
	// slow one with at least 576 lines; else the largest of all.
	pick := func(fast bool) string {
		var largest string
		for _, f := range info.Formats { // sorted: height asc, then fps asc
			if f.Slow == fast {
				continue
			}
			if f.Height >= 576 {
				return f.Spec
			}
			largest = f.Spec
		}
		return largest
	}
	if info.Auto = pick(true); info.Auto == "" {
		info.Auto = pick(false)
	}
	return info, nil
}

func codecName(vcodec string) string {
	switch {
	case strings.HasPrefix(vcodec, "avc1"), vcodec == "h264":
		return "H.264"
	case strings.HasPrefix(vcodec, "vp09"), strings.HasPrefix(vcodec, "vp9"):
		return "VP9"
	case strings.HasPrefix(vcodec, "av01"):
		return "AV1"
	case strings.HasPrefix(vcodec, "hev1"), strings.HasPrefix(vcodec, "hvc1"):
		return "HEVC"
	}
	return vcodec
}

// EnqueueURL queues a download; the worker fetches it with yt-dlp and then
// transcodes it like any upload. size is the expected download size (0 = unknown).
func (s *Service) EnqueueURL(ctx context.Context, url, format, title string, c Caption, size int64) (db.Job, error) {
	if IsDirectStream(url) {
		return db.Job{}, ErrLive
	}
	if free, _, err := sysinfo.DiskFree(s.cfg.TmpDir); err == nil && size > 0 && uint64(size)+200<<20 > free {
		return db.Job{}, fmt.Errorf("not enough disk space: %d MB free, download is about %d MB", free>>20, size>>20)
	}
	if c.Title != "" {
		title = c.Title
	}
	return s.enqueue(ctx, db.InsertJobParams{
		Source: "url", URL: url, Format: format, Title: title, OrigName: title,
		WantPosition: int64(c.Position), ImageDurationS: int64(c.Seconds), NoTranscode: boolInt(c.NoTranscode),
	})
}

// download fetches a URL job with yt-dlp into TmpDir and returns the file.
const progressMarker = "KIOSKPROGRESS"

// parseProgress reads a yt-dlp progress line printed by our template,
// "KIOSKPROGRESS <format id> <percent>". The percent is padded ("  4.2%") and
// "N/A" when the server sends no size; then ok is false.
func parseProgress(line string) (id string, pct float64, ok bool) {
	f := strings.Fields(line)
	if len(f) != 3 || f[0] != progressMarker {
		return "", 0, false
	}
	pct, err := strconv.ParseFloat(strings.TrimSuffix(f[2], "%"), 64)
	if err != nil {
		return "", 0, false
	}
	return f[1], pct, true
}

func (s *Service) download(ctx context.Context, j db.Job) (string, error) {
	parts := strings.Count(j.Format, "+") + 1
	cmd := exec.CommandContext(ctx, s.cfg.YtDlp,
		"--no-playlist", "--no-warnings", "--no-mtime", "--newline", "--progress",
		"-f", j.Format, "--merge-output-format", "mkv",
		"-o", filepath.Join(s.cfg.TmpDir, fmt.Sprintf("url-%d.%%(ext)s", j.ID)),
		// "download:" selects the template for download progress; it is not
		// printed. The marker is what we look for: "KIOSKPROGRESS mp4   4.2%".
		"--progress-template", "download:"+progressMarker+" %(info.format_id)s %(progress._percent_str)s",
		"--print", "after_move:FILE:%(filepath)s",
		"--", j.URL)
	killGroupOnCancel(cmd) // the standalone yt-dlp runs as two processes, plus ffmpeg for merging
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("yt-dlp: %w", err)
	}
	var file, lastID string
	done, last := -1, -1
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if f, ok := strings.CutPrefix(line, "FILE:"); ok {
			file = f
			continue
		}
		id, pct, ok := parseProgress(line)
		if !ok {
			continue
		}
		if id != lastID { // next part (video, then audio)
			lastID = id
			done++
		}
		overall := min(int((float64(min(done, parts-1))*100+pct)/float64(parts)), 99)
		if overall > last {
			last = overall
			s.progress(ctx, j.ID, "download", overall)
		}
	}
	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("download failed: %s", stderr.last())
	}
	if file == "" || !exists(file) {
		return "", errors.New("download failed: yt-dlp produced no file")
	}
	return file, nil
}

// --- yt-dlp maintenance -----------------------------------------------------

var ytVersion struct {
	sync.Mutex
	v string
}

// YtDlpVersion returns the installed yt-dlp version ("" if it isn't installed).
func (s *Service) YtDlpVersion(ctx context.Context) string {
	ytVersion.Lock()
	defer ytVersion.Unlock()
	if ytVersion.v == "" {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, s.cfg.YtDlp, "--version").Output(); err == nil {
			ytVersion.v = strings.TrimSpace(string(out))
		}
	}
	return ytVersion.v
}

// UpdateYtDlp runs yt-dlp's self-update (works for the standalone binary).
func (s *Service) UpdateYtDlp(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.cfg.YtDlp, "-U").CombinedOutput()
	ytVersion.Lock()
	ytVersion.v = ""
	ytVersion.Unlock()
	msg := lastLine(out)
	s.Logf(ctx, slog.LevelInfo, "yt-dlp update", "result", msg)
	if err != nil {
		return msg, fmt.Errorf("yt-dlp update: %s", msg)
	}
	return msg, nil
}
