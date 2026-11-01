package access

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"net"
	"strings"

	"rsc.io/qr"
)

var tailnet = mustCIDR("100.64.0.0/10")

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// LANAddress is the host's address on the local network: the first private
// IPv4 address that isn't Tailscale's. Empty if there is none.
func LANAddress() string {
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if ok && n.IP.To4() != nil && n.IP.IsPrivate() && !tailnet.Contains(n.IP) {
				return n.IP.String()
			}
		}
	}
	return ""
}

// URL is the address to show for the HTTPS listener on addr (e.g. ":8443").
// override wins when set.
func URL(addr, override string) string {
	if override != "" {
		return strings.TrimRight(override, "/")
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	host := LANAddress()
	if host == "" {
		return ""
	}
	if port == "443" {
		return "https://" + host
	}
	return fmt.Sprintf("https://%s:%s", host, port)
}

// QR encodes url with medium error correction (survives a blurry CRT).
func QR(url string) (*qr.Code, error) {
	return qr.Encode(url, qr.M)
}

// PNG renders url as a QR code PNG with the standard 4-module white border.
func PNG(url string, scale int) ([]byte, error) {
	code, err := QR(url)
	if err != nil {
		return nil, err
	}
	const quiet = 4
	n := (code.Size + 2*quiet) * scale
	img := image.NewGray(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			mx, my := x/scale-quiet, y/scale-quiet
			v := uint8(255)
			if mx >= 0 && my >= 0 && mx < code.Size && my < code.Size && code.Black(mx, my) {
				v = 0
			}
			img.Pix[y*img.Stride+x] = v
		}
	}
	var buf bytes.Buffer
	err = png.Encode(&buf, img)
	return buf.Bytes(), err
}
