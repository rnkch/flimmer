package web

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rnkch/flimmerkiste/internal/kiosk"
	"github.com/rnkch/flimmerkiste/internal/mpv"
	"github.com/rnkch/flimmerkiste/internal/store"
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
		t.Fatalf(
			"bundle too small: js %d css %d",
			len(s.bundle.JS.Contents),
			len(s.bundle.CSS.Contents),
		)
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
	req := httptest.NewRequest(
		"POST",
		"/login",
		strings.NewReader(url.Values{"password": {"wrong-password"}}.Encode()),
	)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	s.failures = 0 // skip the login back-off in the test

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(
		"POST",
		"/login",
		strings.NewReader(url.Values{"password": {"test-password-123"}}.Encode()),
	)
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

// Every response carries the server's boot id, so a page can tell that the
// server restarted (app.js, "serverRestarting"): same within one start,
// different between starts.
func TestBootID(t *testing.T) {
	get := func(s *Server, path string) string {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Header().Get("X-Boot")
	}
	a, _ := newServer(t)
	b, _ := newServer(t)
	id := get(a, "/login")
	if len(id) != 16 {
		t.Fatalf("boot id %q", id)
	}
	if get(a, "/login") != id || get(a, "/playlist") != id { // /playlist redirects to the login
		t.Fatal("the boot id changed within one start, or is missing on a redirect")
	}
	if get(b, "/login") == id {
		t.Fatal("two starts share a boot id")
	}
}

// Switching an entry off over HTTP: the row comes back dimmed with "Enable" in
// its menu (and no "Show now"), the summary counts it, and it can be switched on again.
func TestDisableEntryOverHTTP(t *testing.T) {
	s, k := newServer(t)
	ctx := context.Background()
	m, err := k.AddStream(
		ctx, kiosk.URLInfo{URL: "rtsp://example.invalid/cam", Title: "Cam", Live: true},
		kiosk.URLFormat{}, "", 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	token, err := k.Login(ctx, "test-password-123")
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path string, form url.Values) string {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		req.AddCookie(&http.Cookie{Name: "lang", Value: "en"})
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	row := fmt.Sprintf("/media/%d/enabled", m.ID)

	off := do("POST", row, url.Values{"on": {"false"}})
	for _, want := range []string{`class="off"`, "Disabled: skipped in playback", "Enable</wa-dropdown-item>", `"on": "true"`} {
		if !strings.Contains(off, want) {
			t.Errorf("disabled row lacks %q:\n%s", want, off)
		}
	}
	if strings.Contains(off, "Show now") || strings.Contains(off, "Disable</wa-dropdown-item>") {
		t.Errorf("a disabled row offers Show now or Disable:\n%s", off)
	}
	if got, _ := k.Get(ctx, m.ID); kiosk.Enabled(got) {
		t.Fatal("still enabled in the database")
	}
	if sum := do("GET", "/playlist/summary", nil); !strings.Contains(sum, "1 disabled") {
		t.Errorf("summary doesn't count the disabled entry: %s", sum)
	}

	on := do("POST", row, url.Values{"on": {"true"}})
	if strings.Contains(on, "off-note") || !strings.Contains(on, "Show now") ||
		!strings.Contains(on, "Disable</wa-dropdown-item>") {
		t.Errorf("enabled row:\n%s", on)
	}
	if sum := do("GET", "/playlist/summary", nil); strings.Contains(sum, "disabled") {
		t.Errorf("summary still mentions disabled entries: %s", sum)
	}
}

// A live connection (/events) opens with the boot id, so a page that
// reconnects after a restart can tell (app.js, "boot").
func TestEventsOpenWithBootID(t *testing.T) {
	first := func(s *Server, k *kiosk.Service) (event, data string) {
		t.Helper()
		token, err := k.Login(context.Background(), "test-password-123")
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(s.Handler())
		defer srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() && sc.Text() != "" { // the first message ends at the blank line
			switch line := sc.Text(); {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		return event, data
	}
	a, ka := newServer(t)
	b, kb := newServer(t)
	ev, id := first(a, ka)
	if ev != "boot" || id != a.boot || id == "" {
		t.Fatalf("first message: event %q data %q, boot id %q", ev, id, a.boot)
	}
	if ev2, id2 := first(a, ka); ev2 != "boot" || id2 != id {
		t.Fatalf("a second connection to the same start: %q %q, want %q", ev2, id2, id)
	}
	if _, idB := first(b, kb); idB == id {
		t.Fatal("two starts share a boot id")
	}
}

// The htmx extensions a page uses must be named in its htmx-config: htmx silently
// ignores an extension that isn't (app.js registers "keep-overlays").
func TestPagesEnableTheirHtmxExtensions(t *testing.T) {
	s, _ := newServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	m := regexp.MustCompile(`name="htmx-config" content='([^']*)'`).
		FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("no htmx-config meta:\n%s", rec.Body.String())
	}
	for _, ext := range []string{"sse", "alpine-compat", "keep-overlays"} {
		if !strings.Contains(m[1], ext) {
			t.Errorf("htmx-config %q doesn't enable %q", m[1], ext)
		}
	}
}

// Tabs and the other links to a page are partial navigations: the href stays (new tab,
// no JavaScript), htmx takes over the plain click and swaps only <main id="page">, and
// every page has exactly one #page and one #topnav for htmx to select.
func TestNavigationIsPartial(t *testing.T) {
	s, k := newServer(t)
	token, err := k.Login(context.Background(), "test-password-123")
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string, htmx bool) string {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		if htmx { // the navigation request: the full page comes back, htmx selects #page
			req.Header.Set("HX-Request", "true")
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	for _, path := range []string{"/playlist", "/add", "/system", "/settings"} {
		for _, htmx := range []bool{false, true} {
			body := get(path, htmx)
			if n := strings.Count(body, `id="page"`); n != 1 {
				t.Errorf("%s (htmx %v): %d elements with id=page, want 1", path, htmx, n)
			}
			if n := strings.Count(body, `id="topnav"`); n != 1 {
				t.Errorf("%s (htmx %v): %d elements with id=topnav, want 1", path, htmx, n)
			}
			if !strings.Contains(body, "<title>") {
				t.Errorf("%s (htmx %v): no <title> for htmx to take over", path, htmx)
			}
		}
	}
	body := get("/playlist", false)
	for _, href := range []string{"/playlist", "/add", "/system", "/settings"} {
		if !strings.Contains(body, `hx-get="`+href+`"`) {
			t.Errorf("no partial navigation for %s", href)
		}
		if !strings.Contains(body, `href="`+href+`"`) {
			t.Errorf("the link to %s lost its href", href)
		}
	}
	for _, attr := range []string{
		`hx-target="#page"`, `hx-select="#page"`, `hx-select-oob="#topnav:outerMorph"`,
		`hx-push-url="true"`, `hx-swap="outerHTML show:top"`,
		// a modifier-key click is left to the browser; the plain one is cancelled
		`hx-trigger="click[button===0&amp;&amp;!ctrlKey&amp;&amp;!metaKey&amp;&amp;!shiftKey&amp;&amp;!altKey] prevent"`,
	} {
		if !strings.Contains(body, attr) {
			t.Errorf("page lacks %s", attr)
		}
	}
}

// Icons are cached for good under a versioned URL (the bundle's content hash, from the
// asset-version meta); without a version they keep the short cache.
func TestIconCachingAndAssetVersion(t *testing.T) {
	s, _ := newServer(t)
	cache := func(url string) string {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: %d", url, rec.Code)
		}
		return rec.Header().Get("Cache-Control")
	}
	if got := cache("/static/icons/tv.svg?v=abc123"); got != "public, max-age=31536000, immutable" {
		t.Errorf("versioned icon: %q", got)
	}
	if got := cache("/static/icons/tv.svg"); got != "public, max-age=86400" {
		t.Errorf("unversioned icon: %q", got)
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	body := rec.Body.String()
	m := regexp.MustCompile(`name="asset-version" content="([0-9A-Za-z]+)"`).
		FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no asset-version meta:\n%s", body)
	}
	if !strings.Contains(body, `/static/app.`+m[1]+`.js`) {
		t.Errorf("asset-version %q is not the bundle's hash (%s)", m[1], s.bundle.JS.Name)
	}
	// Hidden until its components are defined, so a page doesn't show unstyled.
	if !strings.Contains(body, `class="login wa-cloak"`) {
		t.Errorf("the login page isn't cloaked")
	}
}
