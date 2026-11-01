package kiosk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Upgrades replace the running binary with the latest GitHub release.
//
// A release has one plain binary per platform ("flimmerkiste-linux-arm64") and a
// "checksums.txt" in sha256sum format (see .github/workflows/release.yml). The
// download is only installed when its SHA-256 matches that file and the new
// binary reports the release's version. The old binary stays next to it as
// "<name>.prev": if the new one doesn't stay up (it crashes on start twice),
// it puts the old one back by itself (CheckPendingUpgrade).

// DefaultUpdateRepo is where releases are looked up unless -update-repo says otherwise.
const DefaultUpdateRepo = "rnkch/flimmerkiste"

const (
	defaultUpdateAPI = "https://api.github.com"
	checksumsAsset   = "checksums.txt"
	maxBinarySize    = 200 << 20
	infoCacheFor     = 5 * time.Minute
	upgradeMaxStarts = 2 // starts of a new binary that didn't last, before going back
)

// upgradeConfirmAfter: a new binary that is still up after this is kept (a var for tests).
var upgradeConfirmAfter = time.Minute

var (
	ErrUpgradeOff   = errors.New("updates are switched off")
	ErrUpToDate     = errors.New("already up to date")
	ErrUpgradeBusy  = errors.New("an upgrade is already running")
	ErrNoAsset      = errors.New("the release has no download for this platform")
	ErrChecksum     = errors.New("the download doesn't match its checksum")
	ErrUpgradeCheck = errors.New("the downloaded program doesn't work")
	ErrNoRelease    = errors.New("no release has been published yet")
)

// httpError is a download that GitHub answered with something other than 200.
type httpError struct {
	URL, Status string
	Code        int
}

func (e *httpError) Error() string { return e.URL + ": " + e.Status }

// UpgradeInfo is what is running and what is the latest release.
type UpgradeInfo struct {
	Current    string // Version(): "v0.2.0" for a release build, else a commit
	Release    bool   // Current is a release build
	Latest     string // tag of the latest release, "v0.3.0"
	URL        string // its page on GitHub
	Newer      bool   // Latest is worth installing (newer than Current, or Current isn't a release)
	CanInstall bool   // the release has a download for this platform
}

type ghRelease struct {
	Tag    string `json:"tag_name"`
	URL    string `json:"html_url"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

type upgradeState struct {
	mu   sync.Mutex // held while an upgrade runs
	info struct {
		sync.Mutex
		rel ghRelease
		at  time.Time
	}
}

// platformAsset is the file name of this platform's binary in a release.
func platformAsset() string { return "flimmerkiste-" + runtime.GOOS + "-" + runtime.GOARCH }

// parseVersion reads "v1.2.3" or "1.2.3-rc1" (the suffix is ignored).
func parseVersion(v string) (n [3]int, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, _, _ = strings.Cut(v, "-")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return n, false
	}
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return n, false
		}
		n[i] = x
	}
	return n, true
}

// newerVersion reports whether release latest should replace current: it is
// newer, or current isn't a release build at all (a commit from deploy.sh).
func newerVersion(latest, current string) bool {
	l, ok := parseVersion(latest)
	if !ok {
		return false
	}
	c, ok := parseVersion(current)
	if !ok {
		return true
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func (s *Service) updateAPI() string {
	if s.cfg.UpdateAPI != "" {
		return s.cfg.UpdateAPI
	}
	return defaultUpdateAPI
}

// allowedURL: downloads come over HTTPS (plain HTTP only for a test server).
func (s *Service) allowedURL(u string) bool {
	return strings.HasPrefix(u, "https://") || (s.cfg.UpdateAPI != "" && strings.HasPrefix(u, "http://"))
}

func (s *Service) httpGet(ctx context.Context, url string, accept string) (*http.Response, error) {
	if !s.allowedURL(url) {
		return nil, fmt.Errorf("refusing to download from %q", url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "flimmerkiste/"+Version())
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, &httpError{URL: url, Status: resp.Status, Code: resp.StatusCode}
	}
	return resp, nil
}

// latestRelease asks GitHub for the latest release (never a draft or
// pre-release). The answer is kept for a few minutes unless force is set.
func (s *Service) latestRelease(ctx context.Context, force bool) (ghRelease, error) {
	if s.cfg.UpdateRepo == "" {
		return ghRelease{}, ErrUpgradeOff
	}
	c := &s.upgrade.info
	c.Lock()
	defer c.Unlock()
	if !force && c.rel.Tag != "" && time.Since(c.at) < infoCacheFor {
		return c.rel, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := s.httpGet(ctx, s.updateAPI()+"/repos/"+s.cfg.UpdateRepo+"/releases/latest", "application/vnd.github+json")
	var he *httpError
	if errors.As(err, &he) && he.Code == http.StatusNotFound {
		return ghRelease{}, ErrNoRelease
	}
	if err != nil {
		return ghRelease{}, err
	}
	defer resp.Body.Close()
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil || rel.Tag == "" {
		return ghRelease{}, fmt.Errorf("unexpected answer from GitHub")
	}
	c.rel, c.at = rel, time.Now()
	return rel, nil
}

// CheckUpgrade says what is running and whether a newer release exists.
func (s *Service) CheckUpgrade(ctx context.Context, force bool) (UpgradeInfo, error) {
	rel, err := s.latestRelease(ctx, force)
	if err != nil {
		return UpgradeInfo{}, err
	}
	cur := Version()
	_, isRelease := parseVersion(cur)
	info := UpgradeInfo{Current: cur, Release: isRelease, Latest: rel.Tag, URL: rel.URL, Newer: newerVersion(rel.Tag, cur)}
	for _, a := range rel.Assets {
		if a.Name == platformAsset() {
			info.CanInstall = true
		}
	}
	return info, nil
}

// execPath is the file to replace: the running binary.
func (s *Service) execPath() (string, error) {
	if s.cfg.ExecPath != "" {
		return s.cfg.ExecPath, nil
	}
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// UpgradeResult is what Upgrade did.
type UpgradeResult struct {
	From, To  string
	Restarted bool // false when not run by systemd: the new version starts with the next start
}

// Upgrade installs the latest release and restarts the service. Nothing is
// replaced unless the download is complete, matches its checksum and the new
// binary runs. Refused while the kiosk is locked: it interrupts playback.
func (s *Service) Upgrade(ctx context.Context) (UpgradeResult, error) {
	if err := s.unlocked(ctx); err != nil {
		return UpgradeResult{}, err
	}
	if !s.upgrade.mu.TryLock() {
		return UpgradeResult{}, ErrUpgradeBusy
	}
	defer s.upgrade.mu.Unlock()

	info, err := s.CheckUpgrade(ctx, true)
	if err != nil {
		return UpgradeResult{}, err
	}
	if !info.Newer {
		return UpgradeResult{}, ErrUpToDate
	}
	if !info.CanInstall {
		return UpgradeResult{}, ErrNoAsset
	}
	exe, err := s.execPath()
	if err != nil {
		return UpgradeResult{}, err
	}
	rel, _ := s.latestRelease(ctx, false)
	s.Logf(ctx, slog.LevelInfo, "upgrading", "from", info.Current, "to", info.Latest)

	newPath := exe + ".new"
	defer os.Remove(newPath)
	if err := s.downloadRelease(ctx, rel, newPath); err != nil {
		return UpgradeResult{}, err
	}
	out, err := exec.CommandContext(ctx, newPath, "-version").Output()
	if err != nil || strings.TrimSpace(string(out)) != rel.Tag {
		return UpgradeResult{}, fmt.Errorf("%w: it reports %q, expected %q", ErrUpgradeCheck, strings.TrimSpace(string(out)), rel.Tag)
	}

	// Keep the old binary, mark the upgrade as pending, then swap. Renaming
	// over a running binary is fine on Linux; the old process keeps its inode.
	prev := exe + ".prev"
	if err := s.writePending(pendingUpgrade{From: info.Current, To: rel.Tag}); err != nil {
		return UpgradeResult{}, err
	}
	os.Remove(prev)
	if err := os.Rename(exe, prev); err != nil {
		s.clearPending()
		return UpgradeResult{}, err
	}
	if err := os.Rename(newPath, exe); err != nil {
		os.Rename(prev, exe)
		s.clearPending()
		return UpgradeResult{}, err
	}
	res := UpgradeResult{From: info.Current, To: rel.Tag}
	if os.Getenv("INVOCATION_ID") != "" { // started by systemd
		s.RestartService(ctx)
		res.Restarted = true
	}
	s.Logf(ctx, slog.LevelInfo, "upgraded", "from", res.From, "to", res.To, "restarted", res.Restarted)
	return res, nil
}

// downloadRelease fetches the release's binary for this platform into dst after
// checking it against the release's checksums.txt.
func (s *Service) downloadRelease(ctx context.Context, rel ghRelease, dst string) error {
	var binURL, sumURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case platformAsset():
			binURL = a.URL
		case checksumsAsset:
			sumURL = a.URL
		}
	}
	if binURL == "" {
		return ErrNoAsset
	}
	if sumURL == "" {
		return fmt.Errorf("%w: the release has no %s", ErrChecksum, checksumsAsset)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	resp, err := s.httpGet(ctx, sumURL, "")
	if err != nil {
		return err
	}
	sums, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && strings.TrimPrefix(f[1], "*") == platformAsset() {
			want = strings.ToLower(f[0])
		}
	}
	if want == "" {
		return fmt.Errorf("%w: no entry for %s", ErrChecksum, platformAsset())
	}

	resp, err = s.httpGet(ctx, binURL, "application/octet-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxBinarySize+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > maxBinarySize {
		return fmt.Errorf("the download is larger than %d MB", maxBinarySize>>20)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return ErrChecksum
	}
	return os.Chmod(dst, 0o755)
}

// --- pending upgrade: go back to the old binary if the new one doesn't last ---

type pendingUpgrade struct {
	From, To string
	Starts   int // how often the new binary started without lasting
}

func (s *Service) pendingPath() string { return s.cfg.UpgradeMarker }

func (s *Service) writePending(p pendingUpgrade) error {
	if s.pendingPath() == "" {
		return nil
	}
	b, _ := json.Marshal(p)
	return os.WriteFile(s.pendingPath(), b, 0o644)
}

func (s *Service) clearPending() {
	if s.pendingPath() != "" {
		os.Remove(s.pendingPath())
	}
}

// CheckPendingUpgrade runs first thing at startup. After an upgrade the new
// binary has to stay up for a minute to be kept; one that crashes twice before
// that is replaced by the old one again, and rolledBack tells main to exit so
// systemd starts that.
func (s *Service) CheckPendingUpgrade() (rolledBack bool) {
	if s.pendingPath() == "" {
		return false
	}
	b, err := os.ReadFile(s.pendingPath())
	if err != nil {
		return false
	}
	var p pendingUpgrade
	if json.Unmarshal(b, &p) != nil || p.To != Version() {
		s.clearPending() // some other version runs now: nothing to watch
		return false
	}
	p.Starts++
	if p.Starts > upgradeMaxStarts {
		exe, err := s.execPath()
		if err == nil {
			err = os.Rename(exe+".prev", exe)
		}
		s.clearPending()
		if err != nil {
			slog.Error("going back to the previous version failed", "err", err)
			return false
		}
		s.Logf(context.Background(), slog.LevelError, "upgrade rolled back: the new version didn't stay up", "bad", p.To, "back_to", p.From)
		return true
	}
	if err := s.writePending(p); err != nil {
		slog.Warn("upgrade marker", "err", err)
	}
	time.AfterFunc(upgradeConfirmAfter, func() {
		s.clearPending()
		s.Logf(context.Background(), slog.LevelInfo, "upgrade confirmed", "version", p.To)
	})
	return false
}
