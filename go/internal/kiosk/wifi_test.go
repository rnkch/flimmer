package kiosk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// newWifiService runs the Wi-Fi code against testdata/fake-nmcli with the
// given networks in range ("SSID:SIGNAL:SECURITY" lines, nmcli -t escaping).
func newWifiService(t *testing.T, scan string) (*Service, string) {
	t.Helper()
	s, _ := newTestService(t)
	dir := t.TempDir()
	t.Setenv("FAKE_NMCLI_DIR", dir)
	os.WriteFile(filepath.Join(dir, "scan"), []byte(scan), 0o644)
	fake, _ := filepath.Abs("testdata/fake-nmcli")
	s.cfg.Nmcli = []string{"bash", fake}
	s.cfg.FallbackWifi = "flimmer:flimmer"
	return s, dir
}

func calls(t *testing.T, dir string) []string {
	b, _ := os.ReadFile(filepath.Join(dir, "calls"))
	return lines(string(b))
}

func TestSplitTerse(t *testing.T) {
	got := splitTerse(`Kino\:Foyer:72:WPA2 WPA3:back\\slash`)
	want := []string{"Kino:Foyer", "72", "WPA2 WPA3", `back\slash`}
	if !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
}

func TestFallbackWifiOnce(t *testing.T) {
	s, dir := newWifiService(t, "")
	ctx := context.Background()
	s.EnsureFallbackWifi(ctx)
	s.EnsureFallbackWifi(ctx) // second start: already saved
	var adds []string
	for _, c := range calls(t, dir) {
		if strings.HasPrefix(c, "connection add") {
			adds = append(adds, c)
		}
	}
	if len(adds) != 1 || !strings.Contains(adds[0], "con-name flimmer-fallback ssid flimmer") ||
		!strings.Contains(adds[0], "connection.autoconnect-priority -10") {
		t.Fatalf("adds: %q", adds)
	}
}

func TestWifiNetworksAndSave(t *testing.T) {
	s, dir := newWifiService(t, "Kino\\:Foyer:72:WPA2\nKino\\:Foyer:30:WPA2\nCafe:40:\nUni:55:WPA2 802.1X\nNeu:20:WPA3\n:80:WPA2\nflimmer:90:WPA2\n")
	ctx := context.Background()
	s.EnsureFallbackWifi(ctx)

	if err := s.SaveWifi(ctx, "Kino:Foyer", "geheim123"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveWifi(ctx, "Neu", "geheim123"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveWifi(ctx, "Cafe", ""); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		ssid, pw string
		err      error
	}{
		{"", "geheim123", ErrWifiSSID},
		{strings.Repeat("x", 33), "geheim123", ErrWifiSSID},
		{"Kurz", "1234567", ErrWifiPassword},
		{"Uni", "geheim123", ErrWifiEnterprise},
		{"flimmer", "geheim123", ErrWifiFallbackSSID},
	} {
		if err := s.SaveWifi(ctx, c.ssid, c.pw); !errors.Is(err, c.err) {
			t.Errorf("SaveWifi(%q): %v, want %v", c.ssid, err, c.err)
		}
	}
	all := strings.Join(calls(t, dir), "\n")
	for _, want := range []string{
		"con-name Kino:Foyer ssid Kino:Foyer connection.autoconnect yes wifi-sec.key-mgmt wpa-psk wifi-sec.psk geheim123",
		"con-name Neu ssid Neu connection.autoconnect yes wifi-sec.key-mgmt sae",
		"con-name Cafe ssid Cafe connection.autoconnect yes\n",
	} {
		if !strings.Contains(all+"\n", want) {
			t.Errorf("missing call %q in\n%s", want, all)
		}
	}

	// Saving a saved network again changes its password instead.
	if err := s.SaveWifi(ctx, "Kino:Foyer", "anders123"); err != nil {
		t.Fatal(err)
	}
	if c := calls(t, dir); !strings.HasPrefix(c[len(c)-1], "connection modify id Kino:Foyer wifi-sec.key-mgmt wpa-psk wifi-sec.psk anders123") {
		t.Fatalf("last call %q", c[len(c)-1])
	}

	list, err := s.WifiNetworks(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range list {
		got = append(got, n.SSID)
	}
	// saved by signal, fallback last among saved, then the rest by signal; no hidden network
	want := []string{"Kino:Foyer", "Cafe", "Neu", "flimmer", "Uni"}
	if !slices.Equal(got, want) {
		t.Fatalf("order %q, want %q", got, want)
	}
	if list[0].Signal != 72 || !list[0].Saved || !list[3].Fallback || list[4].Saved {
		t.Fatalf("%+v", list)
	}
}

func TestDeleteAndConnectWifi(t *testing.T) {
	s, dir := newWifiService(t, "Kino:72:WPA2\nCafe:40:WPA2\n")
	ctx := context.Background()
	s.EnsureFallbackWifi(ctx)
	s.SaveWifi(ctx, "Kino", "geheim123")
	s.SaveWifi(ctx, "Cafe", "geheim123")

	if err := s.DeleteWifi(ctx, fallbackWifiName); !errors.Is(err, ErrWifiFallback) {
		t.Fatalf("delete fallback: %v", err)
	}
	if err := s.DeleteWifi(ctx, "Nope"); !errors.Is(err, ErrWifiNotSaved) {
		t.Fatalf("delete unknown: %v", err)
	}
	if err := s.ConnectWifi(ctx, "Kino"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := os.ReadFile(filepath.Join(dir, "active")); strings.TrimSpace(string(b)) == "Kino" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not connected")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := s.DeleteWifi(ctx, "Kino"); !errors.Is(err, ErrWifiActive) {
		t.Fatalf("delete active: %v", err)
	}
	if err := s.DeleteWifi(ctx, "Cafe"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "con", "Cafe")); !os.IsNotExist(err) {
		t.Fatal("Cafe still saved")
	}
}

func TestWifiUnavailable(t *testing.T) {
	s, _ := newTestService(t)
	s.cfg.Nmcli = []string{"no-such-nmcli-binary"}
	if _, err := s.WifiNetworks(context.Background(), false); !errors.Is(err, ErrWifiUnavailable) {
		t.Fatalf("%v", err)
	}
	s.cfg.Nmcli = nil
	if _, err := s.WifiNetworks(context.Background(), false); !errors.Is(err, ErrWifiUnavailable) {
		t.Fatalf("%v", err)
	}
}
