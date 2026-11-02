package views

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestNameHue(t *testing.T) {
	if nameHue("Foyer", "") != nameHue("Foyer", "") {
		t.Fatal("same name, different hue")
	}
	seen := map[string]bool{}
	for _, n := range []string{"p1", "p2", "p3", "p4", "Foyer", "Kinosaal", "Bar", "Eingang", "Kasse", "Saal 2"} {
		seen[nameHue(n, "")] = true
	}
	if len(seen) < 4 {
		t.Errorf("ten names share only %d hues", len(seen))
	}
	if nameHue("Foyer", "indigo") != "indigo" {
		t.Error("a chosen color doesn't win over the automatic one")
	}
	render := func(name, set, hue string) string {
		var b bytes.Buffer
		BrandName(name, set, hue).Render(context.Background(), &b)
		return b.String()
	}
	if html := render(
		"Foyer",
		"Foyer",
		"",
	); !strings.Contains(
		html,
		`style="color: var(--fk-hue-`+nameHue("Foyer", "")+`);"`,
	) {
		t.Errorf("automatic color missing:\n%s", html)
	}
	// The popover: the name field (empty while the hostname is shown),
	// "auto" plus nine dots, the chosen one pressed.
	html := render("p4", "", "indigo")
	if !strings.Contains(html, `style="color: var(--fk-hue-indigo);"`) ||
		!strings.Contains(html, `<wa-popover for="brand-name"`) ||
		!strings.Contains(html, `name="name" value=""`) {
		t.Errorf("color, popover or empty name field missing:\n%s", html)
	}
	if n := strings.Count(html, `name="hue"`); n != 10 {
		t.Errorf("%d color dots, want 10", n)
	}
	if !strings.Contains(
		html,
		`value="indigo" class="hue" style="background: var(--fk-hue-indigo);"`,
	) ||
		strings.Count(html, `aria-pressed="true"`) != 1 {
		t.Errorf("chosen dot not marked:\n%s", html)
	}
}
