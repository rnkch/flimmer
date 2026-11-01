package kiosk

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Wi-Fi is managed through NetworkManager (Raspberry Pi OS since bookworm).
// Its polkit rule only lets the sudo/netdev groups change system connections
// from an active login session, which a systemd service doesn't have, so
// Config.Nmcli is "sudo -n nmcli" on the Pi.

// fallbackWifiName is the connection name of the always-saved fallback
// network (Config.FallbackWifi), e.g. a phone hotspot staff can start when
// the venue's Wi-Fi is unknown or gone.
const fallbackWifiName = "flimmer-fallback"

var (
	ErrWifiUnavailable  = errors.New("Wi-Fi can't be managed on this device")
	ErrWifiSSID         = errors.New("the network name must be 1 to 32 characters")
	ErrWifiPassword     = errors.New("the Wi-Fi password must be 8 to 63 characters (or empty for an open network)")
	ErrWifiEnterprise   = errors.New("networks with user name and password (802.1X) are not supported")
	ErrWifiFallback     = errors.New("the fallback network can't be changed here")
	ErrWifiActive       = errors.New("the Pi is connected through this network right now")
	ErrWifiNotSaved     = errors.New("this network is not saved")
	ErrWifiFallbackSSID = errors.New("this is the fallback network, it is already saved")
)

// WifiNetwork is a saved network, a network in range, or both.
type WifiNetwork struct {
	Name     string // NetworkManager connection name ("" = not saved)
	SSID     string
	Saved    bool
	Fallback bool
	Active   bool // the Pi is connected through it
	InRange  bool
	Signal   int    // 0-100, only when in range
	Security string // as nmcli reports it, e.g. "WPA2", "WPA2 WPA3", "" (open)
}

// Enterprise reports a network with per-user logins (802.1X), which
// SaveWifi can't set up.
func (n WifiNetwork) Enterprise() bool { return strings.Contains(n.Security, "802.1X") }

func (s *Service) nmcli(ctx context.Context, args ...string) (string, error) {
	if len(s.cfg.Nmcli) == 0 {
		return "", ErrWifiUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.cfg.Nmcli[0], append(slices.Clone(s.cfg.Nmcli[1:]), args...)...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("nmcli: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", ErrWifiUnavailable
		}
		return "", fmt.Errorf("nmcli: %w", err)
	}
	return string(out), nil
}

// splitTerse splits a line of nmcli -t output: fields are separated by ':',
// literal ':' and '\' inside a field are escaped with '\'.
func splitTerse(line string) []string {
	var fields []string
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\\' && i+1 < len(line):
			i++
			b.WriteByte(line[i])
		case c == ':':
			fields = append(fields, b.String())
			b.Reset()
		default:
			b.WriteByte(c)
		}
	}
	return append(fields, b.String())
}

func lines(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == '\n' })
}

// WifiNetworks lists saved networks first (connected one on top), then the
// other networks in range by signal. rescan forces a fresh scan (a few
// seconds); otherwise NetworkManager's recent results are used.
func (s *Service) WifiNetworks(ctx context.Context, rescan bool) ([]WifiNetwork, error) {
	out, err := s.nmcli(ctx, "-t", "-f", "NAME,TYPE,DEVICE", "connection", "show")
	if err != nil {
		return nil, err
	}
	var saved []WifiNetwork
	for _, l := range lines(out) {
		f := splitTerse(l)
		if len(f) < 3 || f[1] != "802-11-wireless" {
			continue
		}
		ssid, err := s.nmcli(ctx, "-g", "802-11-wireless.ssid", "connection", "show", "id", f[0])
		if err != nil {
			return nil, err
		}
		saved = append(saved, WifiNetwork{
			Name: f[0], SSID: splitTerse(strings.TrimRight(ssid, "\n"))[0], Saved: true,
			Fallback: f[0] == fallbackWifiName, Active: f[2] != "",
		})
	}

	scan := "auto"
	if rescan {
		scan = "yes"
	}
	out, err = s.nmcli(ctx, "-t", "-f", "SSID,SIGNAL,SECURITY", "device", "wifi", "list", "--rescan", scan)
	if err != nil {
		return nil, err
	}
	inRange := map[string]WifiNetwork{} // strongest access point per SSID
	for _, l := range lines(out) {
		f := splitTerse(l)
		if len(f) < 3 || f[0] == "" { // hidden networks have no name
			continue
		}
		sig, _ := strconv.Atoi(f[1])
		if n, ok := inRange[f[0]]; !ok || sig > n.Signal {
			inRange[f[0]] = WifiNetwork{SSID: f[0], InRange: true, Signal: sig, Security: f[2]}
		}
	}

	var list []WifiNetwork
	for _, n := range saved {
		if r, ok := inRange[n.SSID]; ok {
			n.InRange, n.Signal, n.Security = true, r.Signal, r.Security
			delete(inRange, n.SSID)
		}
		list = append(list, n)
	}
	slices.SortStableFunc(list, func(a, b WifiNetwork) int {
		return cmp.Or(boolCmp(b.Active, a.Active), boolCmp(a.Fallback, b.Fallback), cmp.Compare(b.Signal, a.Signal))
	})
	var others []WifiNetwork
	for _, n := range inRange {
		others = append(others, n)
	}
	slices.SortFunc(others, func(a, b WifiNetwork) int {
		return cmp.Or(cmp.Compare(b.Signal, a.Signal), strings.Compare(a.SSID, b.SSID))
	})
	return append(list, others...), nil
}

func boolCmp(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// SaveWifi saves a network (or changes the password of a saved one). It does
// not switch over: NetworkManager uses it automatically when the current
// connection is gone, or right away with ConnectWifi.
func (s *Service) SaveWifi(ctx context.Context, ssid, password string) error {
	if ssid == "" || len(ssid) > 32 {
		return ErrWifiSSID
	}
	if password != "" && (len(password) < 8 || len(password) > 63) {
		return ErrWifiPassword
	}
	if fb, _, ok := s.fallbackWifi(); ok && ssid == fb {
		return ErrWifiFallbackSSID
	}
	list, err := s.WifiNetworks(ctx, false)
	if err != nil {
		return err
	}
	var found WifiNetwork
	var existing *WifiNetwork
	for i, n := range list {
		if n.SSID == ssid {
			found = n
			if n.Saved && existing == nil {
				existing = &list[i]
			}
		}
	}
	if found.Enterprise() {
		return ErrWifiEnterprise
	}
	security := found.Security
	sec := []string{"wifi-sec.key-mgmt", keyMgmt(security), "wifi-sec.psk", password}
	if password == "" {
		sec = []string{"remove", "802-11-wireless-security"}
	}
	if existing != nil {
		if existing.Fallback {
			return ErrWifiFallback
		}
		_, err = s.nmcli(ctx, append([]string{"connection", "modify", "id", existing.Name}, sec...)...)
	} else {
		args := []string{"connection", "add", "type", "wifi", "con-name", ssid, "ssid", ssid,
			"connection.autoconnect", "yes"}
		if password != "" {
			args = append(args, sec...)
		}
		_, err = s.nmcli(ctx, args...)
	}
	if err == nil {
		s.Logf(ctx, slog.LevelInfo, "Wi-Fi saved", "ssid", ssid)
		s.Hub.Publish(Event{Type: EventNetwork})
	}
	return err
}

// keyMgmt picks WPA3 (SAE) only for networks that offer nothing else; mixed
// WPA2/WPA3 networks get WPA2-PSK, which every Pi supports.
func keyMgmt(security string) string {
	if strings.Contains(security, "WPA3") && !strings.Contains(security, "WPA2") && !strings.Contains(security, "WPA1") {
		return "sae"
	}
	return "wpa-psk"
}

// DeleteWifi forgets a saved network. The fallback network and the network
// the Pi is connected through can't be deleted.
func (s *Service) DeleteWifi(ctx context.Context, name string) error {
	n, err := s.savedWifi(ctx, name)
	if err != nil {
		return err
	}
	switch {
	case n.Fallback:
		return ErrWifiFallback
	case n.Active:
		return ErrWifiActive
	}
	if _, err := s.nmcli(ctx, "connection", "delete", "id", name); err != nil {
		return err
	}
	s.Logf(ctx, slog.LevelInfo, "Wi-Fi deleted", "ssid", n.SSID)
	s.Hub.Publish(Event{Type: EventNetwork})
	return nil
}

// ConnectWifi switches to a saved network in the background: whoever asked
// may be connected through the old one and lose the page. When the Pi is
// connected, the QR code with its new address is shown on the screen.
func (s *Service) ConnectWifi(ctx context.Context, name string) error {
	if err := s.unlocked(ctx); err != nil {
		return err
	}
	n, err := s.savedWifi(ctx, name)
	if err != nil {
		return err
	}
	if n.Active {
		return nil
	}
	go func() {
		ctx := context.WithoutCancel(ctx)
		if _, err := s.nmcli(ctx, "--wait", "40", "connection", "up", "id", name); err != nil {
			s.Logf(ctx, slog.LevelWarn, "Wi-Fi connect failed", "ssid", n.SSID, "err", err)
		} else {
			s.Logf(ctx, slog.LevelInfo, "Wi-Fi connected", "ssid", n.SSID)
			time.Sleep(2 * time.Second) // DHCP address, HTTPS certificate
			if err := s.ShowQR(ctx); err != nil {
				slog.Warn("show QR after Wi-Fi change", "err", err)
			}
		}
		s.Hub.Publish(Event{Type: EventNetwork})
	}()
	return nil
}

func (s *Service) savedWifi(ctx context.Context, name string) (WifiNetwork, error) {
	list, err := s.WifiNetworks(ctx, false)
	if err != nil {
		return WifiNetwork{}, err
	}
	for _, n := range list {
		if n.Saved && n.Name == name {
			return n, nil
		}
	}
	return WifiNetwork{}, ErrWifiNotSaved
}

// fallbackWifi splits Config.FallbackWifi ("ssid:password") at the first ':'.
func (s *Service) fallbackWifi() (ssid, password string, ok bool) {
	ssid, password, ok = strings.Cut(s.cfg.FallbackWifi, ":")
	return ssid, password, ok && ssid != ""
}

// EnsureFallbackWifi saves the fallback network once, with a lower priority
// than every other network, so a known Wi-Fi always wins when both are in
// range. An existing profile is left alone.
func (s *Service) EnsureFallbackWifi(ctx context.Context) {
	ssid, password, ok := s.fallbackWifi()
	if !ok {
		return
	}
	out, err := s.nmcli(ctx, "-t", "-f", "NAME", "connection", "show")
	if err != nil {
		if !errors.Is(err, ErrWifiUnavailable) {
			s.Logf(ctx, slog.LevelWarn, "fallback Wi-Fi not checked", "err", err)
		}
		return
	}
	for _, l := range lines(out) {
		if splitTerse(l)[0] == fallbackWifiName {
			return
		}
	}
	_, err = s.nmcli(ctx, "connection", "add", "type", "wifi", "con-name", fallbackWifiName, "ssid", ssid,
		"connection.autoconnect", "yes", "connection.autoconnect-priority", "-10",
		"wifi-sec.key-mgmt", "wpa-psk", "wifi-sec.psk", password)
	if err != nil {
		s.Logf(ctx, slog.LevelWarn, "fallback Wi-Fi not saved", "err", err)
		return
	}
	s.Logf(ctx, slog.LevelInfo, "fallback Wi-Fi saved", "ssid", ssid)
}
