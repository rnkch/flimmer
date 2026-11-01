package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rnkch/flimmer/internal/kiosk"
	"github.com/rnkch/flimmer/internal/mpv"
	"github.com/rnkch/flimmer/internal/store"
)

func newServer(t *testing.T) (*Server, *kiosk.Service) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	k := kiosk.New(kiosk.Config{MediaDir: dir, TmpDir: dir, ThumbDir: dir}, st, &mpv.Player{})
	if err := k.SetWebPassword(context.Background(), "test-password-123"); err != nil {
		t.Fatal(err)
	}
	s, err := New(k, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, k
}

func TestBundleFromEmbeddedFiles(t *testing.T) {
	s, _ := newServer(t)
	if len(s.bundle.JS.Contents) < 100_000 || len(s.bundle.CSS.Contents) < 10_000 {
		t.Fatalf("bundle too small: js %d css %d", len(s.bundle.JS.Contents), len(s.bundle.CSS.Contents))
	}
	h := s.Handler()
	for _, p := range []string{"/static/" + s.bundle.JS.Name, "/static/" + s.bundle.CSS.Name, "/static/icons/play.svg"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}

func TestLoginRequiredAndCSRF(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/playlist", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("unauthenticated: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"password": {"wrong-password"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	s.failures = 0 // skip the login back-off in the test

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"password": {"test-password-123"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)
	cookies := rec.Result().Cookies()
	if rec.Code != http.StatusSeeOther || len(cookies) == 0 {
		t.Fatalf("login: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/playlist", nil)
	req.AddCookie(cookies[0])
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `hx-sse:connect="/events"`) {
		t.Fatalf("playlist: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/settings", nil)
	req.AddCookie(cookies[0])
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d", rec.Code)
	}
}

func TestASCIIJSONForHeaders(t *testing.T) {
	got := asciiJSON([]byte("{\"m\":\"ung\u00fcltig \U0001F4FA\"}"))
	want := `{"m":"ung\u00fcltig \ud83d\udcfa"}`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// TestEnglishPagesHaveNoGerman renders every page in English (empty
// database, so no user data) and looks for leftover German text.
func TestEnglishPagesHaveNoGerman(t *testing.T) {
	s, k := newServer(t)
	h := s.Handler()
	token, err := k.Login(context.Background(), "test-password-123")
	if err != nil {
		t.Fatal(err)
	}
	// The default help text is German content for the Telegram group, not UI text.
	k.SetSetting(context.Background(), kiosk.SettingHelpText, "Help")
	german := regexp.MustCompile(`[äöüÄÖÜß]|Einstellungen|Hochladen|Wiedergabe|Speichern|Abmelden`)
	for _, p := range []string{"/playlist", "/add", "/system", "/settings", "/settings/wifi", "/login"} {
		req := httptest.NewRequest("GET", p, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		req.AddCookie(&http.Cookie{Name: "lang", Value: "en"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		// The language switch's own name is data, not UI text.
		body = strings.ReplaceAll(body, "Deutsch", "")
		if m := german.FindString(body); m != "" {
			i := strings.Index(body, m)
			t.Errorf("%s: German text %q near %q", p, m, body[max(0, i-60):min(len(body), i+40)])
		}
	}
}
