// Package bundle builds the browser JS and CSS with esbuild's Go API from a
// file system holding assets/ (our sources) and vendor/ (pinned npm files).
// No node_modules: bare imports are mapped onto vendor/ here.
package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// Entries are bundled to app.js and app.css.
var Entries = []string{"assets/app.js", "assets/app.css"}

// vendorPath maps a bare import to its file under vendor/.
func vendorPath(spec string) (string, bool) {
	switch {
	case spec == "htmx.org":
		return "vendor/htmx/htmx.esm.js", true
	case strings.HasPrefix(spec, "htmx.org/ext/"):
		return "vendor/htmx/ext/" + strings.TrimPrefix(spec, "htmx.org/ext/"), true
	case spec == "alpinejs":
		return "vendor/alpinejs/module.esm.js", true
	case strings.HasPrefix(spec, "@awesome.me/webawesome/"):
		return "vendor/webawesome/" + strings.TrimPrefix(spec, "@awesome.me/webawesome/"), true
	}
	return "", false
}

const ns = "vfs"

func plugin(fsys fs.FS) api.Plugin {
	return api.Plugin{Name: "vfs", Setup: func(b api.PluginBuild) {
		b.OnResolve(
			api.OnResolveOptions{Filter: ".*"},
			func(a api.OnResolveArgs) (api.OnResolveResult, error) {
				p := a.Path
				if strings.HasPrefix(p, "data:") {
					return api.OnResolveResult{External: true}, nil
				}
				if v, ok := vendorPath(p); ok {
					p = v
				} else if strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") ||
					(strings.HasSuffix(a.Importer, ".css") && !strings.HasPrefix(p, "@")) {
					// CSS resolves url('x.css') relative to the stylesheet
					p = path.Join(path.Dir(a.Importer), p)
				} else if a.Kind != api.ResolveEntryPoint {
					return api.OnResolveResult{}, fmt.Errorf(
						"unknown import %q from %s",
						a.Path,
						a.Importer,
					)
				}
				return api.OnResolveResult{Path: path.Clean(p), Namespace: ns}, nil
			},
		)
		b.OnLoad(
			api.OnLoadOptions{Filter: ".*", Namespace: ns},
			func(a api.OnLoadArgs) (api.OnLoadResult, error) {
				src, err := fs.ReadFile(fsys, a.Path)
				if err != nil {
					return api.OnLoadResult{}, err
				}
				s := string(src)
				loader := api.LoaderJS
				if strings.HasSuffix(a.Path, ".css") {
					loader = api.LoaderCSS
				}
				return api.OnLoadResult{
					Contents:   &s,
					Loader:     loader,
					ResolveDir: path.Dir(a.Path),
				}, nil
			},
		)
	}}
}

type File struct {
	Name     string // e.g. app.3f2a9c1d.js
	Contents []byte
}

type Result struct {
	JS, CSS File
	Inputs  []string // every file read from fsys (for the vendor tool)
}

// Build bundles and minifies the entries.
func Build(fsys fs.FS) (Result, error) {
	r := api.Build(api.BuildOptions{
		EntryPoints:       Entries,
		Bundle:            true,
		Write:             false,
		Outdir:            "out",
		Format:            api.FormatIIFE,
		Target:            api.ES2022,
		MinifyWhitespace:  true,
		MinifyIdentifiers: true,
		MinifySyntax:      true,
		LegalComments:     api.LegalCommentsEndOfFile,
		Metafile:          true,
		LogLevel:          api.LogLevelSilent,
		Plugins:           []api.Plugin{plugin(fsys)},
	})
	if len(r.Errors) > 0 {
		msgs := api.FormatMessages(r.Errors, api.FormatMessagesOptions{Kind: api.ErrorMessage})
		return Result{}, fmt.Errorf("esbuild:\n%s", strings.Join(msgs, ""))
	}
	var res Result
	for _, f := range r.OutputFiles {
		sum := sha256.Sum256(f.Contents)
		h := hex.EncodeToString(sum[:4])
		switch path.Ext(f.Path) {
		case ".js":
			res.JS = File{Name: "app." + h + ".js", Contents: f.Contents}
		case ".css":
			res.CSS = File{Name: "app." + h + ".css", Contents: f.Contents}
		}
	}
	var meta struct {
		Inputs map[string]json.RawMessage `json:"inputs"`
	}
	json.Unmarshal([]byte(r.Metafile), &meta)
	for in := range meta.Inputs {
		res.Inputs = append(res.Inputs, strings.TrimPrefix(in, ns+":"))
	}
	return res, nil
}
