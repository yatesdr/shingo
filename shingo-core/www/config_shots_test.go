//go:build shots

package www

import (
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/shared"
	"shingocore/config"
	"shingocore/messaging"
)

// config_shots_test.go — the Core Configuration page, both themes, at
// 1360x900 and 800 wide, plus the computed styles of its controls, for U2's
// evidence. The page is the real template, the real static files and the real
// handlers, on a stub engine (configPinEngine) with a synthetic config, so it
// needs no database. Run:
//
//	cd shingo-core && CONFIG_SHOTS_OUT=<dir> go test -tags shots -count=1 \
//	    -run '^TestConfigShots$' -v ./www/
//
// The theme is stored the way the toggle stores it (localStorage['theme'])
// and the browser's scheme is emulated to match, as the Edge's
// TestSettingsShots does.
func TestConfigShots(t *testing.T) {
	out := os.Getenv("CONFIG_SHOTS_OUT")
	if out == "" {
		t.Skip("CONFIG_SHOTS_OUT not set")
	}
	chrome := coreChromeBinary()
	if chrome == "" {
		t.Fatal("no Chrome binary: set COMPOSER_SHOTS_CHROME")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}

	srv := configShotsServer(t)
	for _, s := range []struct {
		file   string
		theme  string
		scheme int // Blink preferredColorScheme: 0 dark, 1 light
		w, h   int
	}{
		{"core-config-light-1360.png", "light", 1, 1360, 900},
		{"core-config-dark-1360.png", "dark", 0, 1360, 900},
		{"core-config-light-800.png", "light", 1, 800, 900},
		{"core-config-dark-800.png", "dark", 0, 800, 900},
		{"core-config-light-1360-full.png", "light", 1, 1360, 2200},
		{"core-config-dark-1360-full.png", "dark", 0, 1360, 2200},
		{"core-config-light-800-full.png", "light", 1, 800, 2600},
	} {
		target := filepath.Join(out, s.file)
		_ = os.Remove(target)
		u := srv.URL + "/__shots/theme?t=" + s.theme + "&to=" + url.QueryEscape("/config")
		cmd := exec.Command(chrome,
			"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check", "--user-data-dir="+t.TempDir(),
			"--blink-settings=preferredColorScheme="+strconv.Itoa(s.scheme), "--hide-scrollbars",
			fmt.Sprintf("--window-size=%d,%d", s.w, s.h), "--virtual-time-budget=8000",
			"--screenshot="+target, u)
		if raw, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("chrome %s: %v\n%s", s.file, err, raw)
		}
		if info, err := os.Stat(target); err != nil || info.Size() < 10_000 {
			t.Fatalf("%s missing or blank: %v", target, err)
		}
		t.Logf("wrote %s", target)
	}

	// The DOM once more: the theme took, the page drew, no inline style.
	dom := chromeDOM(t, chrome, srv.URL+"/__shots/theme?t=dark&to="+url.QueryEscape("/config"), 1360, 0)
	if !strings.Contains(dom, `data-theme="dark"`) || !strings.Contains(dom, `set-page set-ui`) {
		t.Errorf("config page did not render in the dark theme")
	}

	// Computed styles, one row per control, at 1360 wide.
	probe := srv.URL + "/__shots/theme?t=light&to=" + url.QueryEscape("/__shots/probe?to=/config")
	cs := chromeDOM(t, chrome, probe, 1500, 1)
	m := regexp.MustCompile(`(?s)<pre id="out">(.*?)</pre>`).FindStringSubmatch(cs)
	if m == nil {
		t.Fatalf("probe wrote nothing")
	}
	if err := os.WriteFile(filepath.Join(out, "computed-style-core.json"), []byte(htmlUnescape(m[1])), 0o644); err != nil {
		t.Fatal(err)
	}
}

func chromeDOM(t *testing.T, chrome, u string, w, scheme int) string {
	t.Helper()
	cmd := exec.Command(chrome,
		"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check", "--user-data-dir="+t.TempDir(),
		"--blink-settings=preferredColorScheme="+strconv.Itoa(scheme),
		fmt.Sprintf("--window-size=%d,900", w), "--virtual-time-budget=8000", "--dump-dom", u)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("chrome --dump-dom: %v", err)
	}
	return string(raw)
}

func htmlUnescape(s string) string {
	return strings.NewReplacer("&quot;", `"`, "&amp;", "&", "&lt;", "<", "&gt;", ">", "&#39;", "'").Replace(s)
}

// configShotsEngine is the pin stub plus the two accessors the page's live
// words read: no messaging client (no words) and the stub's HealthService.
type configShotsEngine struct{ *configPinEngine }

func (e configShotsEngine) MsgClient() *messaging.Client { return nil }

func configShotsServer(t *testing.T) *httptest.Server {
	cfg := config.Defaults()
	cfg.Timezone = ""
	cfg.RDS.BaseURL = "http://fleet.test:8088"
	cfg.RDS.PollInterval = 5 * time.Second
	cfg.RDS.Timeout = 10 * time.Second
	cfg.RDS.FaultGrace = 45 * time.Minute
	cfg.Messaging.Kafka.Brokers = []string{"kafka-a.test:9092", "kafka-b.test:9092"}
	cfg.Messaging.Kafka.GroupID = "shingocore"
	cfg.Messaging.OrdersTopic = "shingo.orders"
	cfg.Messaging.DispatchTopic = "shingo.dispatch"
	n := &cfg.Notifications
	n.Enabled = true
	n.SMTPHost = "smtp.test"
	n.SMTPPort = 587
	n.SMTPTLS = true
	n.SMTPUser = "alerts"
	n.SMTPPassword = "synthetic"
	n.FromAddress = "shingo@test.invalid"
	n.Recipients = []string{"lead@test.invalid", "maint@test.invalid"}
	n.ThrottleMinutes = 15
	cfg.FireAlarm.Enabled = true
	pg := &cfg.Database.Postgres
	pg.Host, pg.Port, pg.Database, pg.User, pg.Password, pg.SSLMode = "postgres.test", 5432, "shingocore", "shingocore", "synthetic", "disable"
	pg.MaxOpenConns, pg.MaxIdleConns, pg.ConnMaxLifetime = 25, 10, 5*time.Minute

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := &configPinEngine{cfg: cfg, path: path}
	eng := configShotsEngine{stub}
	h := &Handlers{
		engine: eng, orchestration: eng,
		sessions: newSessionStore("shots-secret"),
		tmpls:    map[string]*template.Template{},
		boot:     takeBootSnapshot(cfg),
	}
	base := template.Must(template.New("").Funcs(templateFuncs(nil)).ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html"))
	h.tmpls["config.html"] = template.Must(template.Must(base.Clone()).ParseFS(templateFS, "templates/config.html"))

	r := chi.NewRouter()
	r.Handle("/static/shared/*", http.StripPrefix("/static/shared/", http.FileServer(http.FS(shared.Files))))
	staticSub, _ := fs.Sub(staticFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))
	r.Get("/events", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r.Get("/config", h.handleConfig)
	r.Put("/api/config", h.apiConfigSave)
	r.Get("/__shots/theme", func(w http.ResponseWriter, r *http.Request) {
		theme, to := r.URL.Query().Get("t"), r.URL.Query().Get("to")
		// Log in on the way: the page's nav reads the session.
		sess, _ := h.sessions.Get(r, sessionName)
		sess.Values["authenticated"] = true
		sess.Values["username"] = "shots"
		_ = sess.Save(r, w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><script>localStorage.setItem('theme', %s); location.replace(%s);</script>`,
			strconv.Quote(theme), strconv.Quote(to))
	})
	r.Get("/__shots/probe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, computedStyleProbe, strconv.Quote(r.URL.Query().Get("to")), coreProbeSelectors)
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// coreProbeSelectors names one element per control on the Core config page.
const coreProbeSelectors = `[
 ["section title", ".set-sect h2"],
 ["live words", ".set-cnt"],
 ["row label", ".set-fld label"],
 ["sub-label", ".set-fld small"],
 ["input, host (.mid)", "#cfg-pg-host"],
 ["input, port (base)", "[data-field=pg_port]"],
 ["input, URL (.wide)", "#cfg-fleet-url"],
 ["input, duration", "#cfg-fleet-poll"],
 ["switch", "[data-switch=fa_enabled]"],
 ["segment button", "[data-seg] button"],
 ["button", "[data-action=testDatabase]"],
 ["button, quiet", ".set-savebar [data-set=discard]"],
 ["button, primary", ".set-savebar [data-set=save]"],
 ["note", ".set-note"],
 ["save bar words", ".set-savebar .prov"]
]`

// computedStyleProbe loads a page in a same-origin frame and writes the
// computed style of each selector into <pre id="out">.
const computedStyleProbe = `<!doctype html><meta charset="utf-8">
<iframe id="f" width="1360" height="900"></iframe><pre id="out"></pre>
<script>
const f = document.getElementById('f');
f.onload = () => setTimeout(() => {
  const d = f.contentDocument, out = {};
  for (const [name, sel] of %[2]s) {
    const el = d.querySelector(sel);
    if (!el) { out[name] = null; continue; }
    const cs = f.contentWindow.getComputedStyle(el);
    out[name] = { selector: sel, fontSize: cs.fontSize, fontWeight: cs.fontWeight, height: cs.height,
      padding: cs.padding, border: cs.borderTopWidth + ' ' + cs.borderTopStyle, radius: cs.borderTopLeftRadius,
      width: cs.width, minWidth: cs.minWidth, fontFamily: cs.fontFamily };
  }
  document.getElementById('out').textContent = JSON.stringify(out, null, 1);
}, 800);
f.src = %[1]s;
</script>`

func coreChromeBinary() string {
	if p := os.Getenv("COMPOSER_SHOTS_CHROME"); p != "" {
		return p
	}
	candidates := []string{`C:\Program Files\Google\Chrome\Application\chrome.exe`, `C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`}
	if runtime.GOOS != "windows" {
		candidates = []string{"google-chrome", "chromium", "chromium-browser"}
	}
	for _, c := range candidates {
		if strings.Contains(c, string(os.PathSeparator)) {
			if _, err := os.Stat(c); err == nil {
				return c
			}
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}
