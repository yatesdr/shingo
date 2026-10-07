//go:build shots

package www

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"shingo/protocol/debuglog"
	"shingo/shared/scenefixtures"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
)

// settings_shots_test.go — the Processes Settings tab (D5) at 1440x900, in
// both themes, with the browser's prefers-color-scheme set independently of
// the theme. The ui-cleanup brief's U0 takes these before the settings rules
// move to shared/components.css (U1), and U1 re-takes them with the same
// command for a pixel compare. S1 (color-scheme on the theme) is judged on
// the mismatched pair.
//
//	processes-settings-light.png         theme light, scheme light
//	processes-settings-dark.png          theme dark,  scheme dark
//	processes-settings-light-osdark.png  theme light, scheme dark
//	processes-settings-dark-oslight.png  theme dark,  scheme light
//
// THE THEME IS SET, THE SCHEME IS EMULATED. header.html takes the theme from
// localStorage['theme'] and falls back to the scheme, so the two can only
// differ if the theme is stored. /__shots/theme stores it on the page's own
// origin and replaces itself with the real page — the same state the theme
// toggle leaves behind. The scheme is Blink's preferredColorScheme (0 dark,
// 1 light), as composer_shots_test.go's desktop shots use.
//
// SCROLLBARS ARE SHOWN (no --hide-scrollbars, unlike TestComposerShots): the
// settings sheet scrolls inside the page, and its scrollbar is the one native
// control on this tab that follows color-scheme — the only place the
// mismatched pair can differ.
//
// Only these four; TestComposerShots is the full set. Run:
//
//	cd shingo-edge && SETTINGS_SHOTS_OUT=<dir> go test -tags shots -count=1 \
//	    -run '^TestSettingsShots$' -v ./www/
func TestSettingsShots(t *testing.T) {
	out := os.Getenv("SETTINGS_SHOTS_OUT")
	if out == "" {
		t.Skip("SETTINGS_SHOTS_OUT not set")
	}
	chrome := chromeBinary()
	if chrome == "" {
		t.Fatal("no Chrome binary: set COMPOSER_SHOTS_CHROME")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", out, err)
	}

	fx := scenefixtures.A()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")

	cfg := config.Defaults()
	cfg.CoreAPI = "http://core.invalid"
	cfg.StationUID = "shots.press400"
	cfg.Namespace = "shots"
	cfg.LineID = "press400"
	cfg.Messaging.StationID = cfg.StationUID
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: t.Logf})
	eng.Start()
	t.Cleanup(eng.Stop)

	dbg, err := debuglog.New(64, nil)
	if err != nil {
		t.Fatalf("debuglog: %v", err)
	}
	_, router, stop := NewRouter(eng, dbg, nil)
	t.Cleanup(stop)

	mux := http.NewServeMux()
	// An open EventSource keeps virtual time from ever running out.
	mux.HandleFunc("/events", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/__shots/theme", func(w http.ResponseWriter, r *http.Request) {
		theme := r.URL.Query().Get("t")
		to := r.URL.Query().Get("to")
		if (theme != "light" && theme != "dark") || len(to) == 0 || to[0] != '/' {
			http.Error(w, "t=light|dark and a local to= are required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><script>
localStorage.setItem('theme', %s); location.replace(%s);
</script>`, strconv.Quote(theme), strconv.Quote(to))
	})
	mux.Handle("/", router)

	// The admin session, as TestComposerShots carries it (headless Chrome
	// cannot log in).
	var adminCookie *http.Cookie
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if adminCookie != nil && r.URL.Path != "/login" {
			r.AddCookie(adminCookie)
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	if resp, err := noFollow.PostForm(srv.URL+"/login", url.Values{
		"username": {"shots"}, "password": {"shots"},
	}); err == nil {
		for _, c := range resp.Cookies() {
			adminCookie = c
		}
		resp.Body.Close()
	}
	if adminCookie == nil {
		t.Fatal("no session cookie from /login")
	}

	d5 := fmt.Sprintf("/processes?process=%d#tab=settings", seeded.ProcessID)
	for _, s := range []struct {
		file   string
		theme  string
		scheme int // Blink preferredColorScheme: 0 dark, 1 light
	}{
		{"processes-settings-light.png", "light", 1},
		{"processes-settings-dark.png", "dark", 0},
		{"processes-settings-light-osdark.png", "light", 0},
		{"processes-settings-dark-oslight.png", "dark", 1},
	} {
		target := filepath.Join(out, s.file)
		_ = os.Remove(target)
		u := srv.URL + "/__shots/theme?t=" + s.theme + "&to=" + url.QueryEscape(d5)
		cmd := exec.Command(chrome,
			"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check", "--user-data-dir="+t.TempDir(),
			"--blink-settings=preferredColorScheme="+strconv.Itoa(s.scheme),
			"--window-size=1440,900", "--virtual-time-budget=15000",
			"--screenshot="+target, u)
		cmd.Dir = out
		if raw, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("chrome %s: %v\n%s", s.file, err, raw)
		}
		info, err := os.Stat(target)
		if err != nil {
			t.Fatalf("%s was not written: %v", target, err)
		}
		if info.Size() < 10_000 {
			t.Fatalf("%s is %d bytes — a blank page, not a screen", target, info.Size())
		}
		t.Logf("wrote %s (%d bytes)", target, info.Size())

		// The DOM, to prove the theme took and the tab drew.
		dom := exec.Command(chrome,
			"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
			"--user-data-dir="+t.TempDir(),
			"--blink-settings=preferredColorScheme="+strconv.Itoa(s.scheme),
			"--window-size=1440,900", "--virtual-time-budget=15000", "--dump-dom", u)
		raw, err := dom.Output()
		if err != nil {
			t.Fatalf("chrome --dump-dom %s: %v", s.file, err)
		}
		if want := `data-theme="` + s.theme + `"`; !strings.Contains(string(raw), want) {
			t.Errorf("%s: page rendered without %s", s.file, want)
		}
		if !strings.Contains(string(raw), `pd-settings`) {
			t.Errorf("%s: page rendered without the settings sheet (.pd-settings)", s.file)
		}
	}
}
