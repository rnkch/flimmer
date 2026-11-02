package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVersionCompare(t *testing.T) {
	for _, c := range []struct {
		latest, current string
		want            bool
	}{
		{"v0.2.0", "v0.1.0", true},
		{"v0.10.0", "v0.9.5", true}, // numeric, not alphabetical
		{"v1.0.0", "v0.9.9", true},
		{"v0.1.0", "v0.1.0", false},
		{"v0.1.0", "v0.2.0", false},
		{"v0.1.0", "b7edd6f", true}, // a commit build is never "current"
		{"v0.1.0", "dev", true},
		{"nightly", "v0.1.0", false},
	} {
		if got := newerVersion(c.latest, c.current); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}

// fakeRelease is a GitHub release: the API answer plus the files it points to.
type fakeRelease struct {
	tag      string
	binary   string // content of this platform's binary
	checksum string // "" = the real one
	noAsset  bool
}

// upgradeEnv wires a service to a fake GitHub and a fake installed binary.
func upgradeEnv(t *testing.T, rel *fakeRelease) (s *Service, exe string) {
	t.Helper()
	t.Setenv("INVOCATION_ID", "") // not run by systemd: no restart
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases/latest":
			type asset struct {
				Name string `json:"name"`
				URL  string `json:"browser_download_url"`
			}
			assets := []asset{{checksumsAsset, "http://" + r.Host + "/dl/checksums"}}
			if !rel.noAsset {
				assets = append(assets, asset{platformAsset(), "http://" + r.Host + "/dl/binary"})
			}
			json.NewEncoder(w).
				Encode(map[string]any{"tag_name": rel.tag, "html_url": "https://example.test/r", "assets": assets})
		case "/dl/binary":
			fmt.Fprint(w, rel.binary)
		case "/dl/checksums":
			sum := rel.checksum
			if sum == "" {
				h := sha256.Sum256([]byte(rel.binary))
				sum = hex.EncodeToString(h[:])
			}
			fmt.Fprintf(w, "%s  %s\n%s  flimmerkiste-other-os\n", sum, platformAsset(), sum)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	s, _ = newTestService(t)
	dir := t.TempDir()
	exe = filepath.Join(dir, "flimmerkiste")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho v0.2.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.cfg.UpdateRepo, s.cfg.UpdateAPI, s.cfg.ExecPath = "o/r", srv.URL, exe
	s.cfg.UpgradeMarker = filepath.Join(dir, "upgrade-pending")
	old := releaseVersion
	releaseVersion = "v0.2.0"
	t.Cleanup(func() { releaseVersion = old })
	return s, exe
}

// script is a stand-in binary that reports version v when called with -version.
func script(v string) string { return "#!/bin/sh\necho " + v + "\n" }

func content(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpgradeInstalls(t *testing.T) {
	rel := &fakeRelease{tag: "v0.3.0", binary: script("v0.3.0")}
	s, exe := upgradeEnv(t, rel)
	ctx := context.Background()

	info, err := s.CheckUpgrade(ctx, true)
	if err != nil || !info.Newer || !info.CanInstall || info.Latest != "v0.3.0" ||
		info.Current != "v0.2.0" ||
		!info.Release {
		t.Fatalf("check: %+v, %v", info, err)
	}
	res, err := s.Upgrade(ctx)
	if err != nil || res.From != "v0.2.0" || res.To != "v0.3.0" || res.Restarted {
		t.Fatalf("upgrade: %+v, %v", res, err)
	}
	if content(t, exe) != script("v0.3.0") || content(t, exe+".prev") != script("v0.2.0") {
		t.Fatalf("binaries: new %q, prev %q", content(t, exe), content(t, exe+".prev"))
	}
	if fi, _ := os.Stat(exe); fi.Mode()&0o111 == 0 {
		t.Fatal("new binary isn't executable")
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Fatal("temporary download left behind")
	}
	if _, err := os.Stat(s.cfg.UpgradeMarker); err != nil {
		t.Fatalf("no pending-upgrade marker: %v", err)
	}
}

func TestUpgradeRefusals(t *testing.T) {
	ctx := context.Background()
	oldScript := script("v0.2.0")
	for name, c := range map[string]struct {
		rel  fakeRelease
		want error
	}{
		"wrong checksum":     {fakeRelease{tag: "v0.3.0", binary: script("v0.3.0"), checksum: "00ff"}, ErrChecksum},
		"reports wrong tag":  {fakeRelease{tag: "v0.3.0", binary: script("v9.9.9")}, ErrUpgradeCheck},
		"not a program":      {fakeRelease{tag: "v0.3.0", binary: "garbage"}, ErrUpgradeCheck},
		"no platform asset":  {fakeRelease{tag: "v0.3.0", binary: script("v0.3.0"), noAsset: true}, ErrNoAsset},
		"already up to date": {fakeRelease{tag: "v0.2.0", binary: script("v0.2.0")}, ErrUpToDate},
		"older release":      {fakeRelease{tag: "v0.1.0", binary: script("v0.1.0")}, ErrUpToDate},
	} {
		t.Run(name, func(t *testing.T) {
			rel := c.rel
			s, exe := upgradeEnv(t, &rel)
			if _, err := s.Upgrade(ctx); !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if content(t, exe) != oldScript {
				t.Fatal("the installed binary was changed")
			}
			for _, f := range []string{exe + ".new", exe + ".prev", s.cfg.UpgradeMarker} {
				if _, err := os.Stat(f); !os.IsNotExist(err) {
					t.Errorf("%s left behind", filepath.Base(f))
				}
			}
		})
	}

	t.Run("locked", func(t *testing.T) {
		s, exe := upgradeEnv(t, &fakeRelease{tag: "v0.3.0", binary: script("v0.3.0")})
		s.SetLocked(ctx, true)
		if _, err := s.Upgrade(ctx); !errors.Is(err, ErrLocked) {
			t.Fatalf("got %v", err)
		}
		if content(t, exe) != oldScript {
			t.Fatal("changed while locked")
		}
	})
	t.Run("updates off", func(t *testing.T) {
		s, _ := upgradeEnv(t, &fakeRelease{tag: "v0.3.0", binary: script("v0.3.0")})
		s.cfg.UpdateRepo = ""
		if _, err := s.CheckUpgrade(ctx, true); !errors.Is(err, ErrUpgradeOff) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("plain http is refused outside tests", func(t *testing.T) {
		s, _ := newTestService(t)
		if s.allowedURL("http://example.test/x") || !s.allowedURL("https://example.test/x") {
			t.Fatal("URL policy")
		}
	})
}

// A new binary that crashes twice before it has been up for a minute is
// replaced by the old one again; one that lasts is kept.
func TestUpgradeRollsBack(t *testing.T) {
	ctx := context.Background()
	s, exe := upgradeEnv(t, &fakeRelease{tag: "v0.3.0", binary: script("v0.3.0")})
	if _, err := s.Upgrade(ctx); err != nil {
		t.Fatal(err)
	}
	releaseVersion = "v0.3.0" // the new binary is what runs now
	upgradeConfirmAfter = time.Hour
	t.Cleanup(func() { upgradeConfirmAfter = time.Minute })

	for i := 1; i <= upgradeMaxStarts; i++ {
		if s.CheckPendingUpgrade() {
			t.Fatalf("rolled back on start %d", i)
		}
	}
	if !s.CheckPendingUpgrade() {
		t.Fatal("no roll back on the third start")
	}
	if content(t, exe) != script("v0.2.0") {
		t.Fatalf("old binary not back: %q", content(t, exe))
	}
	if _, err := os.Stat(s.cfg.UpgradeMarker); !os.IsNotExist(err) {
		t.Fatal("marker left after rolling back")
	}
}

func TestUpgradeConfirmed(t *testing.T) {
	ctx := context.Background()
	s, exe := upgradeEnv(t, &fakeRelease{tag: "v0.3.0", binary: script("v0.3.0")})
	if _, err := s.Upgrade(ctx); err != nil {
		t.Fatal(err)
	}
	releaseVersion = "v0.3.0"
	upgradeConfirmAfter = 50 * time.Millisecond
	t.Cleanup(func() { upgradeConfirmAfter = time.Minute })

	if s.CheckPendingUpgrade() {
		t.Fatal("rolled back on the first start")
	}
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(s.cfg.UpgradeMarker); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("upgrade not confirmed")
		}
	}
	if content(t, exe) != script("v0.3.0") {
		t.Fatal("the new binary was replaced")
	}
}

// A marker for another version (deploy.sh put something else there) is dropped.
func TestPendingMarkerForOtherVersion(t *testing.T) {
	s, _ := upgradeEnv(t, &fakeRelease{tag: "v0.3.0", binary: script("v0.3.0")})
	s.writePending(pendingUpgrade{From: "v0.1.0", To: "v0.3.0"})
	if s.CheckPendingUpgrade() { // running v0.2.0
		t.Fatal("rolled back")
	}
	if _, err := os.Stat(s.cfg.UpgradeMarker); !os.IsNotExist(err) {
		t.Fatal("stale marker kept")
	}
}
