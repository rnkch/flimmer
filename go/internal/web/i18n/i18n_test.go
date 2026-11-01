package i18n

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/text/language"
)

func TestTranslateAndPlural(t *testing.T) {
	de := With(context.Background(), language.German)
	en := With(context.Background(), language.English)
	cases := []struct{ got, want string }{
		{T(de, "Settings"), "Einstellungen"},
		{T(en, "Settings"), "Settings"},
		{T(de, "Delete “%s”?", "Plakat"), "„Plakat“ löschen?"},
		{T(de, "%d entries", 1), "1 Eintrag"},
		{T(de, "%d entries", 5), "5 Einträge"},
		{T(en, "%d entries", 1), "1 entry"},
		{T(en, "%d entries", 1012), "1,012 entries"},
		{T(de, "%d entries", 1012), "1.012 Einträge"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q want %q", c.got, c.want)
		}
	}
}

func TestEveryKeyIsTranslated(t *testing.T) {
	for en, de := range german {
		if de == "" {
			t.Errorf("empty German text for %q", en)
		}
	}
	for _, k := range JS {
		if _, ok := german[k]; !ok {
			t.Errorf("JS string %q missing from catalog", k)
		}
	}
}

func TestFromRequest(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if got := FromRequest(r); got != language.English {
		t.Errorf("Accept-Language en: %v", got)
	}
	r.Header.Set("Accept-Language", "fr-FR")
	if got := FromRequest(r); got != language.German {
		t.Errorf("unsupported falls back to German: %v", got)
	}
	r.AddCookie(&http.Cookie{Name: Cookie, Value: "en"})
	if got := FromRequest(r); got != language.English {
		t.Errorf("cookie wins: %v", got)
	}
}
