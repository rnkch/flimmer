package access

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"image/png"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCertsCoverHostAndAreReused(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	fp := c.Fingerprint()
	if len(fp) != 95 || strings.Count(fp, ":") != 31 {
		t.Fatalf("fingerprint %q", fp)
	}
	names, ips := hostIdentity()
	if !covers(c.leaf, names, ips) {
		t.Fatalf("certificate doesn't cover %v %v", names, ips)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "key.pem")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", fi.Mode().Perm())
	}

	// Same host: the stored certificate is reused.
	c2, err := NewCerts(dir)
	if err != nil || c2.Fingerprint() != fp {
		t.Fatalf("not reused: %v", err)
	}

	// A certificate that lacks a current address is replaced.
	pemBytes, _ := os.ReadFile(filepath.Join(dir, "cert.pem"))
	block, _ := pem.Decode(pemBytes)
	leaf, _ := x509.ParseCertificate(block.Bytes)
	if covers(leaf, names, append(ips, net.ParseIP("10.99.99.99"))) {
		t.Fatal("covers() accepts an unknown address")
	}
	generate(
		[]string{"localhost"},
		nil,
		filepath.Join(dir, "cert.pem"),
		filepath.Join(dir, "key.pem"),
	)
	c3, err := NewCerts(dir)
	if err != nil || !covers(c3.leaf, names, ips) {
		t.Fatalf("not renewed for the host's addresses: %v", err)
	}
}

func TestURLAndPNG(t *testing.T) {
	if got := URL(":8443", "https://kiosk.example/"); got != "https://kiosk.example" {
		t.Fatalf("override: %q", got)
	}
	if lan := LANAddress(); lan != "" {
		if got := URL(":8443", ""); got != "https://"+lan+":8443" {
			t.Fatalf("lan url: %q", got)
		}
		if got := URL(":443", ""); got != "https://"+lan {
			t.Fatalf("port 443: %q", got)
		}
	}
	// A typical LAN address fits a small code: version 2 (25 modules) at level M.
	code, err := QR("https://192.168.1.50:8443")
	if err != nil {
		t.Fatal(err)
	}
	if code.Size > 29 {
		t.Fatalf("QR has %d modules", code.Size)
	}
	b, err := PNG("https://192.168.1.50:8443", 4)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil || img.Bounds().Dx() != (code.Size+8)*4 {
		t.Fatalf("png %v %v", img.Bounds(), err)
	}
}
