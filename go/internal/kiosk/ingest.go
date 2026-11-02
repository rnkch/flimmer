package kiosk

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rnkch/flimmerkiste/internal/store/db"
	"github.com/rnkch/flimmerkiste/internal/sysinfo"
)

// Caption is the upload caption format documented in the help text:
// "<Reihenfolge> <Name> [<Sekunden>]", e.g. "05 logo 10". Every part is optional.
type Caption struct {
	Position    int // 0 = append
	Title       string
	Seconds     int  // 0 = default
	NoTranscode bool // videos: keep codec and size, only remux (web form)
}

func ParseCaption(s string) Caption {
	var c Caption
	f := strings.Fields(s)
	if len(f) > 0 {
		if n, err := strconv.Atoi(f[0]); err == nil && n > 0 {
			c.Position = n
			f = f[1:]
		}
	}
	if len(f) > 1 {
		if n, err := strconv.Atoi(f[len(f)-1]); err == nil && n > 0 {
			c.Seconds = n
			f = f[:len(f)-1]
		}
	}
	c.Title = strings.Join(f, " ")
	return c
}

// EnqueueTelegram queues a Telegram file; the worker downloads it in order.
func (s *Service) EnqueueTelegram(
	ctx context.Context,
	fileID, origName, caption string,
	chatID int64,
) (db.Job, error) {
	c := ParseCaption(caption)
	return s.enqueue(ctx, db.InsertJobParams{
		Source: "telegram", OrigName: origName, Title: c.Title,
		WantPosition: int64(c.Position), ImageDurationS: int64(c.Seconds),
		TgFileID: fileID, TgChatID: chatID,
	})
}

// EnqueueFile queues a file that is already on disk (web upload).
func (s *Service) EnqueueFile(
	ctx context.Context,
	path, origName string,
	c Caption,
) (db.Job, error) {
	return s.enqueue(ctx, db.InsertJobParams{
		Source:   "web",
		SrcPath:  path,
		OrigName: origName,
		Title:    c.Title,
		WantPosition: int64(
			c.Position,
		),
		ImageDurationS: int64(c.Seconds),
		NoTranscode:    boolInt(c.NoTranscode),
	})
}

func (s *Service) enqueue(ctx context.Context, p db.InsertJobParams) (db.Job, error) {
	j, err := s.st.InsertJob(ctx, p)
	if err != nil {
		return j, err
	}
	s.st.TrimJobs(ctx)
	s.Hub.Publish(Event{Type: EventJob, JobID: j.ID})
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return j, nil
}

func (s *Service) Job(ctx context.Context, id int64) (db.Job, error) { return s.st.GetJob(ctx, id) }

var ErrJobFinished = errors.New("the job has already finished")

// CancelJob stops a waiting or running upload, download or conversion and
// deletes its files. A running job is stopped through its context; runJob
// then cleans up.
func (s *Service) CancelJob(ctx context.Context, id int64) error {
	n, err := s.st.CancelJob(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrJobFinished
	}
	s.jobMu.Lock()
	running := s.jobRunning == id && s.jobCancel != nil
	if running {
		s.jobCancel()
	}
	s.jobMu.Unlock()
	if !running {
		s.removeJobFiles(ctx, id)
		s.Logf(ctx, slog.LevelInfo, "upload cancelled", "job", id)
	}
	s.Hub.Publish(Event{Type: EventJob, JobID: id})
	return nil
}

// removeJobFiles deletes what a job left in TmpDir: its source (upload,
// Telegram or URL download) and partial downloads ("url-7.mp4.part").
func (s *Service) removeJobFiles(ctx context.Context, id int64) {
	if j, err := s.st.GetJob(ctx, id); err == nil && j.SrcPath != "" {
		os.Remove(j.SrcPath)
	}
	for _, prefix := range []string{"url-", "tg-"} {
		matches, _ := filepath.Glob(filepath.Join(s.cfg.TmpDir, fmt.Sprintf("%s%d.*", prefix, id)))
		for _, m := range matches {
			os.Remove(m)
		}
	}
}

// tmpPrefixes are the files the service itself puts in TmpDir: uploads,
// downloads, conversion output, screenshots.
var tmpPrefixes = []string{"web-", "url-", "tg-", "out-", "screen-"}

// cleanTmp deletes leftovers in TmpDir at start: files of ours that no
// waiting job needs (half-written output, partial downloads, uploads cut off
// by a power cut). Recent files are kept: an upload may already be arriving.
func (s *Service) cleanTmp(ctx context.Context) {
	keep := map[string]bool{}
	var jobPrefixes []string
	jobs, err := s.st.ListActiveJobs(ctx)
	if err != nil {
		return
	}
	for _, j := range jobs {
		if j.SrcPath != "" {
			keep[filepath.Base(j.SrcPath)] = true
		}
		jobPrefixes = append(jobPrefixes, fmt.Sprintf("url-%d.", j.ID), fmt.Sprintf("tg-%d.", j.ID))
	}
	entries, _ := os.ReadDir(s.cfg.TmpDir)
	var n int
	var bytes int64
	for _, e := range entries {
		name := e.Name()
		info, err := e.Info()
		if err != nil || e.IsDir() || keep[name] || time.Since(info.ModTime()) < time.Minute ||
			!slices.ContainsFunc(
				tmpPrefixes,
				func(p string) bool { return strings.HasPrefix(name, p) },
			) ||
			slices.ContainsFunc(
				jobPrefixes,
				func(p string) bool { return strings.HasPrefix(name, p) },
			) {
			continue
		}
		if os.Remove(filepath.Join(s.cfg.TmpDir, name)) == nil {
			n++
			bytes += info.Size()
		}
	}
	if n > 0 {
		s.Logf(ctx, slog.LevelInfo, "removed leftover temp files", "files", n, "mb", bytes>>20)
	}
}

func (s *Service) ActiveJobs(
	ctx context.Context,
) ([]db.Job, error) {
	return s.st.ListActiveJobs(ctx)
}

func (s *Service) RecentJobs(ctx context.Context, n int) ([]db.Job, error) {
	return s.st.ListRecentJobs(ctx, int64(n))
}

// SetJobProgressMessage records the Telegram message the bot edits with progress.
func (s *Service) SetJobProgressMessage(ctx context.Context, jobID, chatID int64, msgID int) error {
	return s.st.SetJobProgressMessage(
		ctx,
		db.SetJobProgressMessageParams{TgChatID: chatID, TgProgressMsgID: int64(msgID), ID: jobID},
	)
}

// RunWorker processes queued jobs one at a time, oldest first, until ctx ends.
func (s *Service) RunWorker(ctx context.Context) {
	if err := s.st.RequeueRunningJobs(ctx); err != nil {
		slog.Error("requeue jobs", "err", err)
	}
	s.cleanTmp(ctx)
	for ctx.Err() == nil {
		j, err := s.st.NextQueuedJob(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			select {
			case <-ctx.Done():
			case <-s.wake:
			case <-time.After(time.Minute):
			}
			continue
		}
		if err != nil {
			slog.Error("next job", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}
		s.runJob(ctx, j)
	}
}

func (s *Service) runJob(ctx context.Context, j db.Job) {
	if n, err := s.st.SetJobRunning(ctx, j.ID); err != nil || n == 0 {
		return // cancelled while waiting
	}
	// jctx is the job's own context: CancelJob stops this job only, while a
	// cancelled ctx means the service is shutting down.
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.jobMu.Lock()
	s.jobRunning, s.jobCancel = j.ID, cancel
	s.jobMu.Unlock()
	defer func() {
		s.jobMu.Lock()
		s.jobRunning, s.jobCancel = 0, nil
		s.jobMu.Unlock()
	}()
	s.Hub.Publish(Event{Type: EventJob, JobID: j.ID})
	m, err := s.ingest(jctx, j)
	if ctx.Err() != nil {
		return // shutting down; the job is re-queued on next start
	}
	if err != nil && jctx.Err() != nil {
		// Cancelled by the user (CancelJob already marked it).
		s.removeJobFiles(ctx, j.ID)
		s.Logf(ctx, slog.LevelInfo, "upload cancelled", "job", j.ID, "file", j.OrigName)
		s.Hub.Publish(Event{Type: EventJob, JobID: j.ID})
		return
	}
	if err != nil {
		s.st.SetJobFailed(ctx, db.SetJobFailedParams{Error: err.Error(), ID: j.ID})
		s.Logf(ctx, slog.LevelError, "upload failed", "job", j.ID, "file", j.OrigName, "err", err)
	} else {
		s.st.SetJobDone(
			ctx,
			db.SetJobDoneParams{MediaID: sql.NullInt64{Int64: m.ID, Valid: true}, ID: j.ID},
		)
		s.replaceStream(ctx, j)
		s.Logf(ctx, slog.LevelInfo, "added", "title", m.Title, "position", m.Position)
	}
	s.Hub.Publish(Event{Type: EventJob, JobID: j.ID})
}

func (s *Service) progress(ctx context.Context, jobID int64, phase string, pct int) {
	s.st.SetJobProgress(ctx, db.SetJobProgressParams{Phase: phase, Progress: int64(pct), ID: jobID})
	s.Hub.Publish(Event{Type: EventJob, JobID: jobID})
}

func (s *Service) ingest(ctx context.Context, j db.Job) (db.Media, error) {
	src := j.SrcPath
	if j.Source == "telegram" && (src == "" || !exists(src)) {
		if s.Fetch == nil {
			return db.Media{}, errors.New("telegram download not available")
		}
		src = filepath.Join(
			s.cfg.TmpDir,
			fmt.Sprintf("tg-%d%s", j.ID, strings.ToLower(filepath.Ext(j.OrigName))),
		)
		if err := s.Fetch(ctx, j.TgFileID, src); err != nil {
			os.Remove(src)
			return db.Media{}, fmt.Errorf("download: %w", err)
		}
		s.st.SetJobSrcPath(
			ctx,
			db.SetJobSrcPathParams{SrcPath: src, OrigName: j.OrigName, ID: j.ID},
		)
	}
	if j.Source == "url" && (src == "" || !exists(src)) {
		var err error
		if src, err = s.download(ctx, j); err != nil {
			return db.Media{}, err
		}
		// Keep the title as name; the extension decides image vs video handling.
		j.OrigName = j.Title + filepath.Ext(src)
		s.st.SetJobSrcPath(
			ctx,
			db.SetJobSrcPathParams{SrcPath: src, OrigName: j.OrigName, ID: j.ID},
		)
	}
	defer func() {
		// Interrupted by a shutdown (deploy, restart): keep the source, the
		// job is queued again on the next start and picks it up.
		if ctx.Err() == nil {
			os.Remove(src)
		}
	}()

	info, err := probe(ctx, s.cfg.FFprobe, src)
	if err != nil {
		return db.Media{}, err
	}
	ext := strings.ToLower(filepath.Ext(j.OrigName))
	kind := info.kind(ext)
	if kind == "" {
		return db.Media{}, fmt.Errorf("unsupported file %q", j.OrigName)
	}

	bitrateK := s.SettingInt(ctx, SettingVideoBitrateK)
	need := uint64(50 << 20)
	asIs := kind == "video" && (j.NoTranscode != 0 || playsAsIs(s.cfg.HDMI, info))
	switch {
	case asIs:
		// Taken as it is: a rename, no extra space.
	case kind == "video":
		need += uint64(info.Duration * float64(bitrateK*1000+256_000) / 8 * 1.05)
	default:
		need += uint64(fileSize(src)) * 2
	}
	if free, _, err := sysinfo.DiskFree(s.cfg.MediaDir); err == nil && free < need {
		return db.Media{}, fmt.Errorf(
			"not enough disk space: %d MB free, about %d MB needed",
			free>>20,
			need>>20,
		)
	}

	var name string
	switch kind {
	case "image":
		name, err = s.ingestImage(ctx, src, ext)
	case "video":
		name, err = s.ingestVideo(ctx, j.ID, src, ext, info, bitrateK, asIs)
	}
	if err != nil {
		return db.Media{}, err
	}
	dst := filepath.Join(s.cfg.MediaDir, name)

	title := j.Title
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(j.OrigName), filepath.Ext(j.OrigName))
	}
	if title == "" || title == "photo" {
		title = strings.TrimSuffix(name, filepath.Ext(name))
	}
	dur := int(j.ImageDurationS)
	if dur <= 0 {
		dur = s.SettingInt(ctx, SettingImageDurationDflt)
	}
	final, _ := probe(ctx, s.cfg.FFprobe, dst)
	fw, fh := final.Display()
	pos := int(j.WantPosition)
	if j.ReplacesMediaID != 0 { // "download instead": take the stream's current place
		if r, err := s.st.GetMedia(ctx, j.ReplacesMediaID); err == nil {
			pos = int(r.Position)
		}
	}
	return s.addMedia(ctx, db.InsertMediaParams{
		Filename: name, Title: title, Kind: kind, ImageDurationS: int64(dur),
		Width: int64(fw), Height: int64(fh), DurationS: final.Duration,
		SizeBytes: fileSize(dst), Source: j.Source,
	}, pos)
}

// addMedia inserts a ready file at 1-based position pos (0 = end).
func (s *Service) addMedia(ctx context.Context, p db.InsertMediaParams, pos int) (db.Media, error) {
	s.plMu.Lock()
	defer s.plMu.Unlock()
	var m db.Media
	err := s.st.Tx(ctx, func(q *db.Queries) error {
		list, err := q.ListMedia(ctx)
		if err != nil {
			return err
		}
		p.Position = int64(len(list) + 1)
		if m, err = q.InsertMedia(ctx, p); err != nil {
			return err
		}
		if pos > 0 && pos <= len(list) {
			order := ids(list)
			order = append(order[:pos-1], append([]int64{m.ID}, order[pos-1:]...)...)
			m.Position = int64(pos)
			return reorder(ctx, q, order)
		}
		return nil
	})
	if err != nil {
		return m, err
	}
	if err := s.changed(ctx); err != nil {
		slog.Error("sync after add", "err", err)
	}
	return m, nil
}

func (s *Service) ingestImage(ctx context.Context, src, ext string) (string, error) {
	if ext == ".heic" || ext == ".heif" {
		name := uniqueName(s.cfg.MediaDir, "image", ".jpg")
		out, err := exec.CommandContext(ctx, s.cfg.HeifConvert, src, filepath.Join(s.cfg.MediaDir, name)).
			CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("heif-convert: %v: %s", err, lastLine(out))
		}
		s.fixStillColor(filepath.Join(s.cfg.MediaDir, name))
		return name, nil
	}
	switch ext {
	case ".jpeg":
		ext = ".jpg"
	case ".jpg", ".png", ".gif", ".webp", ".bmp":
	default:
		ext = ".jpg"
	}
	name := uniqueName(s.cfg.MediaDir, "image", ext)
	dst := filepath.Join(s.cfg.MediaDir, name)
	if err := moveFile(src, dst); err != nil {
		return name, err
	}
	s.fixStillColor(dst)
	return name, nil
}

// videoFilter scales a video down to fit the output (720×576 composite,
// 1920×1080 HDMI), keeping its aspect ratio and never scaling up (mpv scales
// at playback). Sizes must be even for 4:2:0; an odd one loses a pixel.
// The Pi's hardware encoder needs no 32×16 alignment (tested on Pi OS 13,
// ffmpeg 7.1). ok is false when the picture stays as it is.
func videoFilter(hdmi bool, w, h int) (vf string, ok bool) {
	maxW, maxH := 720, 576
	if hdmi {
		maxW, maxH = 1920, 1080
	}
	switch {
	case w > maxW || h > maxH:
		if w*maxH > h*maxW { // wider than the output
			w, h = maxW, maxW*h/w
		} else {
			w, h = maxH*w/h, maxH
		}
		return fmt.Sprintf("scale=%d:%d:flags=spline", w&^1, h&^1), true
	case w%2 != 0 || h%2 != 0:
		return fmt.Sprintf("crop=%d:%d", w&^1, h&^1), true
	}
	return "", false
}

// keepMaxBitrate: above this a file is converted even if it would play, to
// save space (a 1080p file at 8 Mbit/s is 3.6 GB per hour).
const keepMaxBitrate = 8_000_000

// playsAsIs reports whether the Pi decodes a video in hardware (H.264, 8 bit
// 4:2:0) at a size, frame rate and bitrate worth keeping; then the file is
// taken as it is. Up to 720p for the CRT (only 576 lines are visible), up to
// 1080p on HDMI; 30 fps, or 60 up to 720p. Unknown values don't count against it.
func playsAsIs(hdmi bool, in probeInfo) bool {
	maxW, maxH := 1280, 720
	if hdmi {
		maxW, maxH = 1920, 1080
	}
	w, h := in.Display() // a portrait phone video is 720 wide and 1280 high, not the other way round
	maxFPS := 30.5       // 29.97 and 30
	if h <= 720 {
		maxFPS = 60.5
	}
	// HDR and BT.2020 are converted (the player doesn't colour-manage), so they never play as they are.
	return in.VideoCodec == "h264" && (in.PixFmt == "yuv420p" || in.PixFmt == "yuvj420p") &&
		!in.wideGamut() && w <= maxW && h <= maxH && in.FPS <= maxFPS && in.BitRate <= keepMaxBitrate
}

// ingestVideo takes a video as it is (asIs: it plays well, or the uploader
// said "don't convert"; mpv plays any container and codec ffmpeg reads, and a
// rename on the same disk is instant), or re-encodes it to H.264 at the
// output size.
func (s *Service) ingestVideo(
	ctx context.Context,
	jobID int64,
	src, ext string,
	info probeInfo,
	bitrateK int,
	asIs bool,
) (string, error) {
	if asIs {
		if !plainExt.MatchString(ext) {
			ext = ".mkv" // a name only: mpv detects the format from the content
		}
		name := uniqueName(s.cfg.MediaDir, "video", ext)
		return name, moveFile(src, filepath.Join(s.cfg.MediaDir, name))
	}

	// ffmpeg turns a rotated phone video upright before the filters, so they work on the size
	// as shown, with square pixels.
	dw, dh := info.Display()
	var chain []string
	if vf, ok := videoFilter(s.cfg.HDMI, dw, dh); ok {
		chain = append(chain, vf)
	} else if info.nonSquare() { // fits, but the pixels aren't square: draw them square
		chain = append(chain, fmt.Sprintf("scale=%d:%d:flags=spline", dw&^1, dh&^1))
	}
	if info.nonSquare() {
		chain = append(chain, "setsar=1")
	}
	toSDR := info.wideGamut()
	switch {
	case toSDR && hasFilter(ctx, s.cfg.FFmpeg, "zscale"):
		chain = append(chain, info.sdrChain())
	case toSDR:
		slog.Warn("HDR video, but this ffmpeg has no zscale: colours will be off")
		toSDR = false
	}
	name := uniqueName(s.cfg.MediaDir, "video", ".mkv")
	dst := filepath.Join(s.cfg.MediaDir, name)
	tmp := filepath.Join(s.cfg.TmpDir, "out-"+name)
	defer os.Remove(tmp)

	b := fmt.Sprintf("%dk", bitrateK)
	args := []string{
		"-n",
		"15",
		s.cfg.FFmpeg,
		"-y",
		"-nostdin",
		"-nostats",
		"-progress",
		"pipe:1",
		"-i",
		src,
	}
	if len(chain) > 0 {
		args = append(args, "-vf", strings.Join(chain, ","))
	}
	args = append(args, "-pix_fmt", "yuv420p")
	if toSDR { // say so: the result is BT.709, whatever the source was
		args = append(args, "-colorspace", "bt709", "-color_primaries", "bt709",
			"-color_trc", "bt709", "-color_range", "tv")
	}
	args = append(args, "-vcodec", s.cfg.VideoEncoder,
		"-b:v", b, "-maxrate", b, "-bufsize", b, "-g", "25", "-acodec", "copy", tmp)

	cmd := exec.CommandContext(ctx, "nice", args...)
	killGroupOnCancel(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	last := -1
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		v, ok := strings.CutPrefix(sc.Text(), "out_time_us=")
		if !ok || info.Duration <= 0 {
			continue
		}
		us, _ := strconv.ParseFloat(v, 64)
		pct := min(int(us/1e6/info.Duration*100), 99)
		if pct > last {
			last = pct
			s.progress(ctx, jobID, "transcode", pct)
		}
	}
	if err := cmd.Wait(); err != nil {
		if toSDR &&
			ctx.Err() == nil { // better off-colour than not at all: once without the conversion
			slog.Warn("HDR conversion failed, transcoding without it", "err", stderr.last())
			info.ColorPrimaries, info.ColorTransfer = "", ""
			return s.ingestVideo(ctx, jobID, src, ext, info, bitrateK, false)
		}
		return "", fmt.Errorf("transcoding failed: %v: %s", err, stderr.last())
	}
	return name, moveFile(tmp, dst)
}

// --- ffprobe ----------------------------------------------------------------

type probeInfo struct {
	Format     string
	Duration   float64
	BitRate    int
	HasVideo   bool
	VideoCodec string
	PixFmt     string
	Width      int // as coded; Display() is the size as shown
	Height     int
	FPS        float64 // 0 when unknown

	// What a player needs to know to show it right. Phones store a portrait video as a
	// landscape one with a rotation, and may mark HDR (BT.2020 with HLG or PQ).
	Rotation       int    // degrees the picture is turned for display: 0, 90, 180 or 270
	SARNum, SARDen int    // pixel aspect ratio; 0 when unknown (square)
	ColorPrimaries string // as ffprobe names them: "bt709", "bt2020", "smpte432" (Display P3)...
	ColorTransfer  string // "bt709", "smpte2084" (PQ), "arib-std-b67" (HLG)...
}

// sdrChain converts BT.2020 / HDR video to BT.709 SDR (zscale from zimg, tone-mapped when
// the transfer is PQ or HLG). The input is named explicitly: files often lack some tags,
// and zscale refuses to guess.
func (in probeInfo) sdrChain() string {
	hdr := in.ColorTransfer == "smpte2084" || in.ColorTransfer == "arib-std-b67"
	first := "zscale=t=linear:npl=100"
	if in.ColorPrimaries == "bt2020" {
		first = "zscale=pin=bt2020:min=bt2020nc:rin=tv:t=linear:npl=100"
	}
	if hdr {
		first = strings.Replace(first, "zscale=", "zscale=tin="+in.ColorTransfer+":", 1)
	}
	chain := []string{first, "format=gbrpf32le", "zscale=p=bt709"}
	if hdr {
		chain = append(chain, "tonemap=tonemap=hable:desat=0")
	}
	return strings.Join(append(chain, "zscale=t=bt709:m=bt709:r=tv", "format=yuv420p"), ",")
}

var (
	filterMu    sync.Mutex
	filterCache = map[string]bool{}
)

// hasFilter reports whether this ffmpeg build has the named filter (cached).
func hasFilter(ctx context.Context, ffmpeg, name string) bool {
	filterMu.Lock()
	defer filterMu.Unlock()
	key := ffmpeg + "\x00" + name
	if v, ok := filterCache[key]; ok {
		return v
	}
	out, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-filters").Output()
	v := err == nil && strings.Contains(string(out), " "+name+" ")
	if err == nil {
		filterCache[key] = v
	}
	return v
}

// Display is the size the picture is shown at: turned by Rotation and with square pixels
// (the width follows the pixel aspect ratio, as a player draws it).
func (in probeInfo) Display() (w, h int) {
	w, h = in.Width, in.Height
	if in.nonSquare() {
		w = int(math.Round(float64(w) * float64(in.SARNum) / float64(in.SARDen)))
	}
	if in.Rotation == 90 || in.Rotation == 270 {
		w, h = h, w
	}
	return w, h
}

func (in probeInfo) nonSquare() bool {
	return in.SARNum > 0 && in.SARDen > 0 && in.SARNum != in.SARDen
}

// wideGamut reports HDR (PQ, HLG) or BT.2020 video: the Pi's player shows it as it is,
// without the colour management that would map it to a normal screen, so it looks washed
// out or tinted. It is converted to BT.709 while transcoding (probeInfo.sdrChain).
func (in probeInfo) wideGamut() bool {
	return in.ColorTransfer == "smpte2084" || in.ColorTransfer == "arib-std-b67" ||
		in.ColorPrimaries == "bt2020"
}

func probe(ctx context.Context, ffprobe, path string) (probeInfo, error) {
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path).
		Output()
	if err != nil {
		return probeInfo{}, fmt.Errorf("ffprobe: %w", err)
	}
	var r struct {
		Format struct {
			FormatName string `json:"format_name"`
			Duration   string `json:"duration"`
			BitRate    string `json:"bit_rate"`
		} `json:"format"`
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			PixFmt    string `json:"pix_fmt"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			AvgFPS    string `json:"avg_frame_rate"` // "30000/1001", "0/0" when unknown
			SAR       string `json:"sample_aspect_ratio"`
			Primaries string `json:"color_primaries"`
			Transfer  string `json:"color_transfer"`
			SideData  []struct {
				Type     string  `json:"side_data_type"`
				Rotation float64 `json:"rotation"`
			} `json:"side_data_list"`
			Tags struct {
				Rotate string `json:"rotate"` // older files
			} `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return probeInfo{}, err
	}
	in := probeInfo{Format: r.Format.FormatName}
	in.Duration, _ = strconv.ParseFloat(r.Format.Duration, 64)
	in.BitRate, _ = strconv.Atoi(r.Format.BitRate)
	for _, st := range r.Streams {
		if st.CodecType == "video" && !in.HasVideo {
			in.HasVideo, in.VideoCodec, in.PixFmt, in.Width, in.Height = true, st.CodecName, st.PixFmt, st.Width, st.Height
			in.FPS = parseRate(st.AvgFPS)
			in.ColorPrimaries, in.ColorTransfer = st.Primaries, st.Transfer
			if n, d, ok := strings.Cut(st.SAR, ":"); ok { // "64:45", "N/A" when unknown
				in.SARNum, _ = strconv.Atoi(n)
				in.SARDen, _ = strconv.Atoi(d)
			}
			rot := 0.0
			for _, sd := range st.SideData {
				if sd.Type == "Display Matrix" {
					rot = sd.Rotation
				}
			}
			if rot == 0 && st.Tags.Rotate != "" {
				rot, _ = strconv.ParseFloat(st.Tags.Rotate, 64)
			}
			in.Rotation = ((int(math.Round(rot/90))*90)%360 + 360) % 360
		}
	}
	return in, nil
}

// parseRate reads an ffprobe rate like "30000/1001"; 0 when unknown.
func parseRate(r string) float64 {
	num, den, ok := strings.Cut(r, "/")
	n, err1 := strconv.ParseFloat(num, 64)
	d, err2 := strconv.ParseFloat(den, 64)
	if !ok || err1 != nil || err2 != nil || d == 0 {
		return 0
	}
	return n / d
}

// kind decides image vs video from the probed container, not from how the
// file was sent (a video sent as a Telegram "file" is still a video).
func (in probeInfo) kind(ext string) string {
	if ext == ".heic" || ext == ".heif" {
		return "image"
	}
	if !in.HasVideo {
		return ""
	}
	if in.Format == "gif" || strings.Contains(in.Format, "image2") ||
		strings.HasSuffix(in.Format, "_pipe") {
		return "image"
	}
	return "video"
}

// --- small helpers ----------------------------------------------------------

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// plainExt: file extensions kept for videos taken as they are (".ogv", ".mp4").
var plainExt = regexp.MustCompile(`^\.[a-z0-9]{1,5}$`)

func uniqueName(dir, prefix, ext string) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	for {
		b := make([]byte, 4)
		rand.Read(b)
		for i := range b {
			b[i] = chars[int(b[i])%len(chars)]
		}
		name := prefix + "-" + string(b) + ext
		if !exists(filepath.Join(dir, name)) {
			return name
		}
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// moveFile renames, falling back to copy when src and dst are on different filesystems.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

// tailBuffer keeps the last few KB of a process's stderr for error messages.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tailBuffer) last() string { return lastLine(t.b) }
