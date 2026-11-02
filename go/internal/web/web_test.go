package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
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
	if got := cache(
		"/static/icons/film.svg?v=abc123",
	); got != "public, max-age=31536000, immutable" {
		t.Errorf("versioned icon: %q", got)
	}
	if got := cache("/static/icons/film.svg"); got != "public, max-age=86400" {
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

type sseMessage struct{ event, data string }

// openEvents connects to /events like a page does and returns its messages.
func openEvents(t *testing.T, s *Server, k *kiosk.Service) <-chan sseMessage {
	t.Helper()
	token, err := k.Login(context.Background(), "test-password-123")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); srv.Close() })
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	out := make(chan sseMessage, 64)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		var m sseMessage
		for sc.Scan() {
			switch line := sc.Text(); {
			case line == "":
				if m.event != "" || m.data != "" {
					out <- m
				}
				m = sseMessage{}
			case strings.HasPrefix(line, "event: "):
				m.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				m.data += strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return out
}

// partials reads messages for up to d and returns which push targets were seen in
// them (the <hx-partial> targets of unnamed messages) and which named events.
func partials(msgs <-chan sseMessage, d time.Duration) (targets, events map[string]bool) {
	targets, events = map[string]bool{}, map[string]bool{}
	re := regexp.MustCompile(`hx-target="(#[a-z-]+)"`)
	deadline := time.After(d)
	for {
		select {
		case m, ok := <-msgs:
			if !ok {
				return
			}
			if m.event != "" {
				events[m.event] = true
			}
			for _, x := range re.FindAllStringSubmatch(m.data, -1) {
				targets[x[1]] = true
			}
		case <-deadline:
			return
		}
	}
}

// The player card, the playlist and its summary are pushed over the live connection:
// everything when it opens (events missed while away), then only what an event changed.
func TestLiveConnectionPushesWhatChanged(t *testing.T) {
	s, k := newServer(t)
	msgs := openEvents(t, s, k)
	card, list, summary := "#player-card", "#playlist", "#playlist-summary"
	brand, favicon := "#brand", "#favicon"

	first, _ := partials(msgs, 1200*time.Millisecond)
	for _, want := range []string{card, list, summary, brand, favicon} {
		if !first[want] {
			t.Errorf("a new connection isn't sent %s (got %v)", want, first)
		}
	}
	for _, c := range []struct {
		event     kiosk.EventType
		want, not []string
	}{
		{kiosk.EventPlayer, []string{card, list}, []string{summary, brand}}, // the current entry moved
		{kiosk.EventPlaylist, []string{list, summary}, []string{card, brand}},
		{kiosk.EventJob, []string{summary}, []string{card, list, brand}}, // jobs in progress
		// the name or its colour: the header and the tab icon, in every open page
		{kiosk.EventSettings, []string{brand, favicon}, []string{card, list, summary}},
	} {
		k.Hub.Publish(kiosk.Event{Type: c.event})
		got, events := partials(msgs, 2*time.Second)
		for _, w := range c.want {
			if !got[w] {
				t.Errorf("%s: %s not pushed (got %v)", c.event, w, got)
			}
		}
		for _, n := range c.not {
			if got[n] {
				t.Errorf("%s: %s pushed although nothing changed there", c.event, n)
			}
		}
		if !events[string(c.event)] { // other parts of the page still listen for the named event
			t.Errorf("%s: the named event is gone (got %v)", c.event, events)
		}
	}
}

// What is pushed isn't fetched again: no refetch triggers on these three elements, and
// the GET endpoints (a held-back update is asked for afterwards) render the same elements.
func TestPlaylistAndPlayerCardAreNotPolled(t *testing.T) {
	s, k := newServer(t)
	token, err := k.Login(context.Background(), "test-password-123")
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) string {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	page := get("/playlist")
	for _, id := range []string{`id="player-card"`, `id="playlist"`, `id="playlist-summary"`} {
		if !strings.Contains(page, id) {
			t.Errorf("the page lacks %s", id)
		}
	}
	for _, url := range []string{"/player/card", "/playlist/list", "/playlist/summary"} {
		if strings.Contains(page, `hx-get="`+url+`"`) {
			t.Errorf("the page still fetches %s itself", url)
		}
	}
	for path, id := range map[string]string{
		"/player/card": `id="player-card"`, "/playlist/list": `id="playlist"`,
		"/playlist/summary": `id="playlist-summary"`,
	} {
		if !strings.Contains(get(path), id) {
			t.Errorf("%s doesn't render %s", path, id)
		}
	}
}

// The home-screen manifest and its icons are public (a browser fetches them without
// cookies), carry the kiosk's own name, and every icon is a PNG of the size it claims.
func TestManifestAndHomeScreenIcons(t *testing.T) {
	s, k := newServer(t)
	if err := k.SetDisplayName(context.Background(), "Foyer Kino 2"); err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder { // no cookie: not logged in
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}

	rec := get("/manifest.webmanifest")
	if rec.Code != 200 ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "application/manifest+json") {
		t.Fatalf("manifest: %d %q (it must be public)", rec.Code, rec.Header().Get("Content-Type"))
	}
	var m struct {
		Name            string `json:"name"`
		ShortName       string `json:"short_name"`
		StartURL        string `json:"start_url"`
		Scope           string `json:"scope"`
		Display         string `json:"display"`
		ThemeColor      string `json:"theme_color"`
		BackgroundColor string `json:"background_color"`
		Icons           []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "Flimmerkiste · Foyer Kino 2" || m.ShortName != "Foyer Kino 2" ||
		m.StartURL != "/playlist" || m.Scope != "/" || m.Display != "standalone" ||
		m.ThemeColor != "#1a1b26" || m.BackgroundColor != "#1a1b26" {
		t.Errorf("manifest: %+v", m)
	}
	var have192, have512, haveMaskable bool
	for _, ic := range m.Icons {
		img := get(ic.Src)
		if img.Code != 200 || img.Header().Get("Content-Type") != "image/png" {
			t.Errorf("icon %s: %d %q", ic.Src, img.Code, img.Header().Get("Content-Type"))
			continue
		}
		cfg, err := png.DecodeConfig(img.Body)
		if err != nil {
			t.Errorf("icon %s isn't a PNG: %v", ic.Src, err)
			continue
		}
		if want := fmt.Sprintf(
			"%dx%d",
			cfg.Width,
			cfg.Height,
		); ic.Sizes != want ||
			cfg.Width != cfg.Height {
			t.Errorf("icon %s claims %s but is %s", ic.Src, ic.Sizes, want)
		}
		have192 = have192 || cfg.Width == 192
		have512 = have512 || cfg.Width == 512 && ic.Purpose != "maskable"
		haveMaskable = haveMaskable || ic.Purpose == "maskable"
	}
	if !have192 || !have512 || !haveMaskable {
		t.Errorf(
			"icons: 192 %v, 512 %v, maskable %v (Chrome needs 192 and 512)",
			have192,
			have512,
			haveMaskable,
		)
	}
	if rec := get("/static/pwa/nope.png"); rec.Code != 404 {
		t.Errorf("an unknown icon: %d", rec.Code)
	}
	if rec := get("/static/pwa/apple-touch-icon.png"); rec.Code != 200 {
		t.Errorf("apple-touch-icon: %d", rec.Code)
	}

	// Every page, the login page included, points at them.
	body := get("/login").Body.String()
	for _, want := range []string{
		`rel="manifest" href="/manifest.webmanifest"`, `rel="apple-touch-icon"`,
		`name="theme-color" content="#1a1b26" media="(prefers-color-scheme: dark)"`,
		`name="theme-color" content="#e1e2e7" media="(prefers-color-scheme: light)"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page head lacks %s", want)
		}
	}
}

// The favicon is the app's icon with the frame in the kiosk's colour, public like the other
// icons; the header and the login page use the same icon (currentColor, so it takes the
// colour of the name).
func TestFaviconAndHeaderIconFollowTheKioskColor(t *testing.T) {
	s, k := newServer(t)
	ctx := context.Background()
	for _, hue := range kiosk.NameHues { // every hue the name can have has a colour to use
		if c := s.hues[hue]; len(c) != 7 || c[0] != '#' {
			t.Errorf("no colour for hue %q: %q", hue, c)
		}
	}
	get := func(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	for hue, want := range map[string]string{"red": s.hues["red"], "green": s.hues["green"]} {
		k.SetDisplayHue(ctx, hue)
		rec := get("/favicon.svg", nil) // no cookie
		body := rec.Body.String()
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/svg+xml" ||
			!strings.Contains(body, `fill="`+want+`"`) || strings.Contains(body, "currentColor") {
			t.Errorf("%s: %d %q, want the frame in %s:\n%.200s", hue, rec.Code,
				rec.Header().Get("Content-Type"), want, body)
		}
	}
	if s.hues["red"] == s.hues["green"] {
		t.Error("two hues share a colour")
	}
	// Without a chosen colour it is the one the name hashes to, as in the header.
	k.SetDisplayHue(ctx, "")
	k.SetDisplayName(ctx, "Foyer")
	if want := s.hues[kiosk.HashHue("Foyer")]; !strings.Contains(
		get("/favicon.svg", nil).Body.String(),
		want,
	) {
		t.Errorf("automatic colour: the favicon lacks %s", want)
	}

	// The PNG fallback, also at the address browsers ask for by habit.
	rec := get("/favicon.ico", nil)
	cfg, err := png.DecodeConfig(rec.Body)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" || err != nil ||
		cfg.Width != 32 {
		t.Errorf("/favicon.ico: %d %q %v %+v", rec.Code, rec.Header().Get("Content-Type"), err, cfg)
	}
	// The header's icon, uncoloured (currentColor) and cached for good under a version.
	rec = get("/static/pwa/icon.svg?v=abc", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "currentColor") ||
		rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("icon.svg: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}

	// The pages: the favicon carries the colour (so it is fetched again after a change), the
	// header and the login page use the app's icon, and the old glyph is gone.
	k.SetDisplayHue(ctx, "red")
	token, _ := k.Login(ctx, "test-password-123")
	page := get("/playlist", &http.Cookie{Name: sessionCookie, Value: token}).Body.String()
	for _, want := range []string{
		`rel="icon" href="/favicon.svg?c=` + strings.TrimPrefix(s.hues["red"], "#") + `"`,
		`href="/static/pwa/favicon-32.png"`, `<wa-icon library="flimmerkiste" name="icon"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/playlist lacks %s", want)
		}
	}
	login := get("/login", nil).Body.String()
	if !strings.Contains(login, `rel="icon" href="/favicon.svg"`) ||
		!strings.Contains(login, `<wa-icon library="flimmerkiste" name="icon"`) {
		t.Errorf("the login page lacks the favicon or the icon")
	}
	if strings.Contains(page, `name="tv"`) || strings.Contains(login, `name="tv"`) {
		t.Error("the old tv glyph is still in use")
	}
}

// Changing the colour (here by the API, as another browser would) reaches every open page
// over the live connection: the header's name and icon, and the tab icon, in the new colour.
func TestColourChangeIsPushedToOpenPages(t *testing.T) {
	s, k := newServer(t)
	msgs := openEvents(t, s, k)
	partials(msgs, 1200*time.Millisecond) // what a new connection gets first

	if err := k.SetDisplayName(context.Background(), "Foyer"); err != nil {
		t.Fatal(err)
	}
	if err := k.SetDisplayHue(context.Background(), "red"); err != nil {
		t.Fatal(err)
	}
	var brand, favicon string
	deadline := time.After(3 * time.Second)
	for brand == "" || favicon == "" || !strings.Contains(brand, "kiosk-hue-red") {
		select {
		case m, ok := <-msgs:
			if !ok {
				t.Fatal("the connection ended")
			}
			for _, part := range strings.Split(m.data, "</hx-partial>") {
				switch {
				case strings.Contains(part, `hx-target="#brand"`):
					brand = part
				case strings.Contains(part, `hx-target="#favicon"`):
					favicon = part
				}
			}
		case <-deadline:
			t.Fatalf("not pushed in time:\nbrand: %.200s\nfavicon: %.200s", brand, favicon)
		}
	}
	if !strings.Contains(brand, "Foyer") ||
		!strings.Contains(brand, `color: var(--kiosk-hue-red);`) {
		t.Errorf("the pushed header lacks the new name or colour:\n%s", brand)
	}
	if want := `c=` + strings.TrimPrefix(s.hues["red"], "#"); !strings.Contains(favicon, want) {
		t.Errorf("the pushed tab icon lacks %s:\n%s", want, favicon)
	}
}

// Every page ends with a link to the project, on the page background.
func TestFooterLinksToGitHub(t *testing.T) {
	s, _ := newServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	if body := rec.Body.String(); !strings.Contains(body, `<footer class="site-footer`) ||
		!strings.Contains(body, `href="https://github.com/rnkch/flimmerkiste"`) {
		t.Fatal("no footer with the GitHub link on the login page")
	}
}
