// Command vendor refreshes internal/web/vendor from pinned npm tarballs.
// It downloads the packages, bundles assets/ once to find which files are
// actually imported, and copies only those (plus licenses and icons).
//
// Run from rpi/go:  go run ./internal/web/cmd/vendor
package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rnkch/flimmerkiste/internal/web/bundle"
)

type pkg struct {
	name, version string
	from          string // directory inside the tarball (below "package/")
	to            string // directory below vendor/
	license       string
}

var pkgs = []pkg{
	{"htmx.org", "4.0.0", "dist", "htmx", "LICENSE"},
	{"alpinejs", "3.17.5", "dist", "alpinejs", ""},
	{"@awesome.me/webawesome", "3.14.0", "dist-cdn", "webawesome", "LICENSE.md"},
	{"@fortawesome/fontawesome-free", "7.3.1", "svgs/solid", "icons", "LICENSE.txt"},
}

// icons used in the templates (Font Awesome Free, solid).
var icons = []string{
	"angles-down",
	"angles-up",
	"arrow-down",
	"arrow-up",
	"backward-step",
	"chart-simple",
	"check",
	"circle-check",
	"circle-exclamation",
	"circle-info",
	"clock",
	"closed-captioning",
	"display",
	"download",
	"ellipsis",
	"eye",
	"eye-slash",
	"file-arrow-up",
	"film",
	"forward-step",
	"gear",
	"grip-vertical",
	"hard-drive",
	"hourglass-half",
	"image",
	"key",
	"language",
	"link",
	"list",
	"lock",
	"lock-open",
	"memory",
	"pause",
	"pen",
	"play",
	"plus",
	"power-off",
	"qrcode",
	"right-from-bracket",
	"rotate",
	"server",
	"sliders",
	"temperature-half",
	"tower-broadcast",
	"trash",
	"triangle-exclamation",
	"upload",
	"wifi",
	"xmark",
}

func main() {
	webDir, _ := filepath.Abs("internal/web")
	if _, err := os.Stat(filepath.Join(webDir, "assets", "app.js")); err != nil {
		log.Fatal("run from rpi/go")
	}
	tmp, err := os.MkdirTemp("", "vendor")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	for _, p := range pkgs {
		if err := fetch(p, filepath.Join(tmp, "vendor", p.to)); err != nil {
			log.Fatalf("%s: %v", p.name, err)
		}
	}
	if err := os.CopyFS(
		filepath.Join(tmp, "assets"),
		os.DirFS(filepath.Join(webDir, "assets")),
	); err != nil {
		log.Fatal(err)
	}

	res, err := bundle.Build(os.DirFS(tmp))
	if err != nil {
		log.Fatal(err)
	}
	keep := map[string]bool{}
	for _, in := range res.Inputs {
		if strings.HasPrefix(in, "vendor/") {
			keep[in] = true
		}
	}
	for _, name := range icons {
		keep["vendor/icons/"+name+".svg"] = true
	}
	for _, p := range pkgs {
		keep["vendor/"+p.to+"/VERSION"] = true
		if p.license != "" {
			keep["vendor/"+p.to+"/"+p.license] = true
		}
	}

	out := filepath.Join(webDir, "vendor")
	if err := os.RemoveAll(out); err != nil {
		log.Fatal(err)
	}
	files := make([]string, 0, len(keep))
	for f := range keep {
		files = append(files, f)
	}
	sort.Strings(files)
	var total int64
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(tmp, f))
		if err != nil {
			log.Fatalf("%s: %v", f, err)
		}
		dst := filepath.Join(webDir, f)
		os.MkdirAll(filepath.Dir(dst), 0o755)
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			log.Fatal(err)
		}
		total += int64(len(b))
	}
	fmt.Printf("vendored %d files, %d KB; bundle js %d KB, css %d KB\n",
		len(files), total/1024, len(res.JS.Contents)/1024, len(res.CSS.Contents)/1024)
}

// fetch extracts package/<from>/ and the license of one npm tarball into dst.
func fetch(p pkg, dst string) error {
	base := p.name
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	url := fmt.Sprintf("https://registry.npmjs.org/%s/-/%s-%s.tgz", p.name, base, p.version)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	prefix := "package/" + p.from + "/"
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		var rel string
		switch {
		case strings.HasPrefix(h.Name, prefix):
			rel = strings.TrimPrefix(h.Name, prefix)
		case p.license != "" && h.Name == "package/"+p.license:
			rel = p.license
		default:
			continue
		}
		if strings.Contains(rel, "..") {
			continue
		}
		target := filepath.Join(dst, rel)
		os.MkdirAll(filepath.Dir(target), 0o755)
		f, err := os.Create(target)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		f.Close()
		if err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dst, "VERSION"), []byte(p.name+"@"+p.version+"\n"), 0o644)
}
