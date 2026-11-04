package components

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScan(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "views/a.templ", `<wa-card><wa-button href="/x">x</wa-button></wa-card>`)
	write(t, dir, "views/helpers.go", "const s = `<wa-tag size=\"xs\">`")
	write(t, dir, "assets/app.js", "el.innerHTML = '<wa-spinner></wa-spinner>'")
	write(t, dir, "views/a_templ.go", `<wa-generated>`)  // generated code: skipped
	write(t, dir, "views/a_test.go", `<wa-in-a-test>`)   // tests: skipped
	write(t, dir, "vendor/x/y.js", `<wa-vendored>`)      // vendored: skipped
	write(t, dir, "assets/components.js", `<wa-itself>`) // the output itself: skipped
	got, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{
		"button",
		"card",
		"spinner",
		"tag",
	}; !slices.Equal(
		got,
		want,
	) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// The committed assets/components.js is what the templates ask for: a tag added to a template
// without running `go run ./internal/web/cmd/components` fails here, not in the browser.
func TestGeneratedFileIsCurrent(t *testing.T) {
	names, err := Scan("..")
	if err != nil {
		t.Fatal(err)
	}
	have, err := os.ReadFile(filepath.Join("..", File))
	if err != nil {
		t.Fatal(err)
	}
	if string(have) != Source(names) {
		t.Fatalf("%s is out of date; run: go run ./internal/web/cmd/components", File)
	}
}
