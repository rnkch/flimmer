// Package access is about reaching the web UI from the local network: a
// self-signed HTTPS certificate for the Pi's own addresses, the address to
// show, and a QR code for it.
package access

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// certValidity stays under the 825 days Apple platforms accept for TLS certificates.
const certValidity = 820 * 24 * time.Hour

// Certs keeps a self-signed certificate for the host's names and addresses,
// renewing it when an address is added or it is about to expire.
type Certs struct {
	dir string

	mu   sync.RWMutex
	cert *tls.Certificate
	leaf *x509.Certificate
}

func NewCerts(dir string) (*Certs, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	c := &Certs{dir: dir}
	return c, c.Refresh()
}

// GetCertificate is for tls.Config.
func (c *Certs) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cert, nil
}

// Fingerprint is the SHA-256 of the certificate, to compare with the browser's view.
func (c *Certs) Fingerprint() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.leaf == nil {
		return ""
	}
	sum := sha256.Sum256(c.leaf.Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// Refresh loads the certificate from disk, or makes a new one when it is
// missing, expires within 30 days or doesn't cover the current addresses.
func (c *Certs) Refresh() error {
	names, ips := hostIdentity()
	certPath, keyPath := filepath.Join(c.dir, "cert.pem"), filepath.Join(c.dir, "key.pem")
	if pair, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if leaf, err := x509.ParseCertificate(pair.Certificate[0]); err == nil && covers(leaf, names, ips) &&
			time.Until(leaf.NotAfter) > 30*24*time.Hour {
			c.set(&pair, leaf)
			return nil
		}
	}
	pair, leaf, err := generate(names, ips, certPath, keyPath)
	if err != nil {
		return err
	}
	slog.Info("new HTTPS certificate", "names", names, "ips", ips)
	c.set(pair, leaf)
	return nil
}

func (c *Certs) set(pair *tls.Certificate, leaf *x509.Certificate) {
	c.mu.Lock()
	c.cert, c.leaf = pair, leaf
	c.mu.Unlock()
}

func covers(leaf *x509.Certificate, names []string, ips []net.IP) bool {
	for _, n := range names {
		if !slices.Contains(leaf.DNSNames, n) {
			return false
		}
	}
	for _, ip := range ips {
		if !slices.ContainsFunc(leaf.IPAddresses, ip.Equal) {
			return false
		}
	}
	return true
}

func generate(names []string, ips []net.IP, certPath, keyPath string) (*tls.Certificate, *x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Flimmer", Organization: []string{"Flimmer"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              names,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	return &pair, leaf, err
}

// hostIdentity: the host name (plain and .local for mDNS), localhost, and
// every address of every interface that is up.
func hostIdentity() ([]string, []net.IP) {
	names := []string{"localhost"}
	if h, err := os.Hostname(); err == nil && h != "" {
		h = strings.TrimSuffix(h, ".local")
		names = append(names, h, h+".local")
	}
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
				ips = append(ips, n.IP)
			}
		}
	}
	return names, ips
}
