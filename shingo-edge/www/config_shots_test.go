//go:build shots

package www

import (
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"shingo/protocol/debuglog"
	"shingo/shared/scenefixtures"
	"shingoedge/backup"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
)

// config_shots_test.go — the Edge Configuration page (U3) in both themes, at
// 1360×900 and 800 wide, through the real router over a test config, plus the
// computed-style table that compares each control on this page with the same
// control on the Processes Settings tab (U1's proof: one definition, three
// pages).
//
//	config-<theme>-1360.png        1360×900, the first screen
//	config-<theme>-1360-full.png   1360×2600, the whole page
//	config-<theme>-800.png         800×900
//	config-<theme>-800-full.png    800×3400
//	computed-style.md              the table
//
// The scheme is emulated to match the theme (S1 is U1's). Run:
//
//	cd shingo-edge && CONFIG_SHOTS_OUT=<dir> go test -tags shots -count=1 \
//	    -run '^TestConfigShots$' -v ./www/
//
// CONFIG_SHOTS_PLCS=<n> points WarLink at a fake that reports n PLCs, so the
// "PLCs WarLink can see" chip row is full (it wraps at 800 wide).
func TestConfigShots(t *testing.T) {
	out := os.Getenv("CONFIG_SHOTS_OUT")
	if out == "" {
		t.Skip("CONFIG_SHOTS_OUT not set")
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
	for n, s := range [][3]string{{"Day", "06:00", "14:00"}, {"Swing", "14:00", "22:00"}} {
		if err := db.UpsertShift(n+1, s[0], s[1], s[2]); err != nil {
			t.Fatalf("seed shift: %v", err)
		}
	}

	// A synthetic, fully set config: every section has something to show.
	cfgPath := filepath.Join(t.TempDir(), "shingoedge.yaml")
	cfg := config.Defaults()
	cfg.CoreAPI = "http://core.invalid:8080"
	cfg.StationUID = "stn-5f0c2a9e41d7b3c8"
	cfg.Namespace = "shots"
	cfg.LineID = "press400"
	cfg.Timezone = "America/Chicago"
	cfg.Messaging.StationID = cfg.StationUID
	cfg.Messaging.Kafka.Brokers = []string{"kafka-1.invalid:9092", "kafka-2.invalid:9092"}
	cfg.WarLink = config.WarLinkConfig{Host: "warlink.invalid", Port: 8080, PollRate: 2 * time.Second, Enabled: true, Mode: "poll"}
	cfg.Backup.Enabled = true
	cfg.Backup.ScheduleInterval = time.Hour
	cfg.Backup.S3 = config.BackupS3Config{Endpoint: "https://s3.example.invalid", Bucket: "edge-backups",
		Region: "us-east-1", AccessKey: "SHOTSKEY", SecretKey: "shots-secret", UsePathStyle: true}
	plcs := 0
	if v := os.Getenv("CONFIG_SHOTS_PLCS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("CONFIG_SHOTS_PLCS=%q: want a count", v)
		}
		plcs = n
		cfg.WarLink.Host, cfg.WarLink.Port = fakeWarlink(t, n)
	}
	eng := engine.New(engine.Config{AppConfig: cfg, ConfigPath: cfgPath, DB: db, LogFunc: t.Logf})
	eng.Start()
	t.Cleanup(eng.Stop)
	for deadline := time.Now().Add(15 * time.Second); len(eng.PLCManager().PLCNames()) < plcs; {
		if time.Now().After(deadline) {
			t.Fatalf("the fake WarLink's %d PLCs never reached the manager", plcs)
		}
		time.Sleep(100 * time.Millisecond)
	}

	dbg, err := debuglog.New(64, nil)
	if err != nil {
		t.Fatalf("debuglog: %v", err)
	}
	svc := backup.NewService(db, cfg, cfgPath, "shots", t.Logf)
	_, router, stop := NewRouter(eng, dbg, svc)
	t.Cleanup(stop)

	mux := http.NewServeMux()
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
	// /__shots/styles loads the two pages in same-origin frames and writes
	// each control's computed style into its own DOM for --dump-dom.
	d5 := fmt.Sprintf("/processes?process=%d#tab=settings", seeded.ProcessID)
	mux.HandleFunc("/__shots/styles", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, stylesProbe, strconv.Quote(d5))
	})
	mux.Handle("/", router)

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

	for _, s := range []struct {
		file   string
		theme  string
		scheme int
		w, h   int
	}{
		{"config-light-1360.png", "light", 1, 1360, 900},
		{"config-dark-1360.png", "dark", 0, 1360, 900},
		{"config-light-1360-full.png", "light", 1, 1360, 2600},
		{"config-dark-1360-full.png", "dark", 0, 1360, 2600},
		{"config-light-800.png", "light", 1, 800, 900},
		{"config-dark-800.png", "dark", 0, 800, 900},
		{"config-light-800-full.png", "light", 1, 800, 3400},
		{"config-dark-800-full.png", "dark", 0, 800, 3400},
	} {
		target := filepath.Join(out, s.file)
		_ = os.Remove(target)
		u := srv.URL + "/__shots/theme?t=" + s.theme + "&to=" + url.QueryEscape("/config")
		cmd := exec.Command(chrome,
			"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check", "--user-data-dir="+t.TempDir(),
			"--hide-scrollbars",
			"--blink-settings=preferredColorScheme="+strconv.Itoa(s.scheme),
			fmt.Sprintf("--window-size=%d,%d", s.w, s.h), "--virtual-time-budget=15000",
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
	}

	// The DOM after the page script ran: the theme took, the page drew, and
	// the restart notice and save bar exist.
	for _, theme := range []string{"light", "dark"} {
		u := srv.URL + "/__shots/theme?t=" + theme + "&to=" + url.QueryEscape("/config")
		raw, err := exec.Command(chrome, "--headless=new", "--disable-gpu", "--no-first-run",
			"--no-default-browser-check", "--user-data-dir="+t.TempDir(), "--window-size=1360,900",
			"--virtual-time-budget=15000", "--dump-dom", u).Output()
		if err != nil {
			t.Fatalf("dump-dom: %v", err)
		}
		dom := string(raw)
		for _, want := range []string{`data-theme="` + theme + `"`, `<h1>Configuration</h1>`, `class="set-savebar"`,
			`No unsaved changes`, `automatic · `} {
			if !strings.Contains(dom, want) {
				t.Errorf("%s: page DOM lacks %q", theme, want)
			}
		}
	}

	// The computed-style table, light theme.
	raw, err := exec.Command(chrome, "--headless=new", "--disable-gpu", "--no-first-run",
		"--no-default-browser-check", "--user-data-dir="+t.TempDir(), "--window-size=1440,900",
		"--blink-settings=preferredColorScheme=1",
		"--virtual-time-budget=20000", "--dump-dom", srv.URL+"/__shots/styles").Output()
	if err != nil {
		t.Fatalf("styles dump: %v", err)
	}
	m := regexp.MustCompile(`(?s)<pre id="out">(.*?)</pre>`).FindStringSubmatch(string(raw))
	if m == nil || !strings.Contains(m[1], "|") {
		t.Fatalf("no computed-style table in the probe's DOM:\n%.2000s", raw)
	}
	table := html.UnescapeString(m[1])
	md := "# Computed style: Edge Configuration vs Processes Settings (U3)\n\n" +
		"Chrome headless, light theme, 1440×900, `TestConfigShots` (shots tag). Each row is one control, read with\n" +
		"getComputedStyle on the Edge Configuration page (`/config`) and on the Processes page's Settings tab\n" +
		"(`" + d5 + "`). Same = every property listed is equal.\n\n" + table
	if err := os.WriteFile(filepath.Join(out, "computed-style.md"), []byte(md), 0o644); err != nil {
		t.Fatalf("write table: %v", err)
	}
	if strings.Contains(table, "| DIFFERS") {
		t.Errorf("a control differs between the two pages:\n%s", table)
	}
	t.Logf("computed-style.md:\n%s", table)
}

// stylesProbe: two same-origin frames, one per page; once both have drawn
// their controls, a markdown table of computed styles goes into <pre id=out>.
const stylesProbe = `<!doctype html><meta charset="utf-8">
<iframe id="a" src="/config" width="1360" height="900"></iframe>
<iframe id="b" src=%s width="1440" height="900"></iframe>
<pre id="out"></pre>
<script>
const props = ['font-size', 'font-weight', 'height', 'padding', 'border', 'border-radius', 'width', 'min-width'];
// kind 'control': every property is judged except width (a box is sized by
// its size attribute or its words, a button by its label: §33 rule 4).
// kind 'text': the type and spacing are judged; height and width follow the
// words, and a border of width 0 is judged on its width only.
const pairs = [
  ['text box (base, 96 floor)', 'control', '.set-inp[data-field="port"]', '.pd-inp:not(.wide)'],
  ['text box .wide (420)', 'control', '.set-inp.wide', '.pd-inp.wide'],
  ['text box .mid (240, added)', 'control', '.set-inp.mid', null],
  ['button', 'control', '.set-btn:not(.primary):not(.quiet):not(.danger)', '.pd-btn:not(.primary):not(.quiet):not(.danger)'],
  ['primary button (Save)', 'control', '.set-savebar .set-btn.primary', '.pd-savebar .pd-btn.primary'],
  ['quiet button', 'control', '.set-btn.quiet', '.pd-btn.quiet'],
  ['switch', 'control', '.set-chk', '.pd-chk'],
  ['segment button', 'control', '.set-seg button', '.pd-seg button'],
  ['chip', 'control', '.set-chip', '.pd-chip'],
  ['row label', 'text', '.set-fld > label', '.pd-sfld > label'],
  ['sub-label', 'text', '.set-fld small', '.pd-sfld small'],
  ['section title', 'text', '.set-sect h2', '.pd-sect h2'],
  ['live words', 'text', '.set-cnt', '.pd-cnt'],
  ['save bar', 'text', '.set-savebar', '.pd-savebar'],
  ['save bar words', 'text', '.set-savebar .prov', '.pd-savebar .prov'],
  ['field row', 'text', '.set-fld', '.pd-sfld'],
];
function pick(doc, sel) {
  if (!sel) return null;
  const all = Array.from(doc.querySelectorAll(sel));
  return all.find(n => n.offsetParent !== null) || all[0] || null;
}
function read(win, n, p) { return n ? win.getComputedStyle(n).getPropertyValue(p) : '(none on this page)'; }
function ready() {
  const a = document.getElementById('a').contentWindow, b = document.getElementById('b').contentWindow;
  try {
    return a.document.querySelector('.set-savebar') && b.document.querySelector('.pd-savebar') ? [a, b] : null;
  } catch (e) { return null; }
}
function judged(kind, p, va, vb) {
  if (p === 'width' || p === 'min-width') return kind === 'control' && p === 'min-width';
  if (kind === 'text' && p === 'height') return false;
  return true;
}
function verdict(kind, p, va, vb, na, nb, a, b) {
  if (!na || !nb) return 'only one page has it';
  if (va === vb) return 'same';
  if (p === 'border' && kind === 'text' &&
      a.getComputedStyle(na).borderTopWidth === '0px' && b.getComputedStyle(nb).borderTopWidth === '0px') {
    return 'same (no border; ink differs)';
  }
  return judged(kind, p, va, vb) ? 'DIFFERS' : 'follows its words';
}
function run() {
  const w = ready();
  if (!w) { setTimeout(run, 200); return; }
  const [a, b] = w;
  const NL = String.fromCharCode(10);
  let md = '| control | property | Edge config | Processes Settings | |' + NL + '|---|---|---|---|---|' + NL;
  for (const [name, kind, sa, sb] of pairs) {
    const na = pick(a.document, sa), nb = pick(b.document, sb);
    for (const p of props) {
      const va = read(a, na, p), vb = read(b, nb, p);
      md += '| ' + name + ' | ' + p + ' | ' + va + ' | ' + vb + ' | ' + verdict(kind, p, va, vb, na, nb, a, b) + ' |' + NL;
    }
  }
  document.getElementById('out').textContent = md;
}
setTimeout(run, 500);
</script>`

// fakeWarlink serves WarLink's PLC list (GET /api/) with n connected PLCs and
// an empty tag map for each, and returns its host and port.
func fakeWarlink(t *testing.T, n int) (string, int) {
	t.Helper()
	var list strings.Builder
	list.WriteString("[")
	for i := 1; i <= n; i++ {
		if i > 1 {
			list.WriteString(",")
		}
		fmt.Fprintf(&list, `{"name":"press_line_plc_%02d","status":"Connected"}`, i)
	}
	list.WriteString("]")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/" {
			fmt.Fprint(w, list.String())
			return
		}
		fmt.Fprint(w, "{}")
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("fake WarLink url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("fake WarLink port: %v", err)
	}
	return u.Hostname(), port
}
