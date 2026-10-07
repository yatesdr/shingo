package www

import (
	"html/template"
	"io"
	"net/http/httptest"
	"regexp"
	"shingo/protocol/testutil"
	"strings"
	"testing"

	"shingoedge/config"
	"shingoedge/plc"
)

// renderConfigPage renders GET /config through handleConfig with the real
// templates over the shared stub.
func renderConfigPage(t *testing.T, seed func(c *config.Config), boot bool, after ...func(c *config.Config)) string {
	t.Helper()
	h, _ := newTestHandlers(t)
	stub := h.engine.(*stubEngine)
	if seed != nil {
		stub.cfg.Lock()
		seed(stub.cfg)
		stub.cfg.Unlock()
	}
	if boot {
		h.boot = newConfigBoot(stub.cfg)
	}
	for _, f := range after {
		stub.cfg.Lock()
		f(stub.cfg)
		stub.cfg.Unlock()
	}
	stub.plcMgr = plc.NewManager(nil, stub.cfg, nil, nil)
	h.tmpl = template.Must(template.New("").Funcs(templateFuncs()).
		ParseFS(templatesFS, "templates/*.html", "templates/partials/*.html"))
	w := httptest.NewRecorder()
	h.handleConfig(w, httptest.NewRequest("GET", "/config", nil))
	if w.Code != 200 {
		t.Fatalf("GET /config = %d\n%s", w.Code, w.Body.String())
	}
	b, err := io.ReadAll(w.Result().Body)
	testutil.MustNoErr(t, err, "read /config body")
	return string(b)
}

// configPageBody is the page's own markup: from the H1 to the page script,
// without the shared header and footer.
func configPageBody(t *testing.T, html string) string {
	t.Helper()
	i := strings.Index(html, `<div class="page-header">`)
	j := strings.Index(html, `<script type="module" src="/static/js/pages/config.js">`)
	if i < 0 || j < i {
		t.Fatalf("config page markup not found")
	}
	return html[i:j]
}

// TestConfigPage_Shape: the house settings shape (U3): H1 Configuration, the
// header's <title> untouched, the seven sections in order, the neutral .set-
// classes only, no inline style, no native <select>, no hex colour.
func TestConfigPage_Shape(t *testing.T) {
	html := renderConfigPage(t, nil, false)
	if !strings.Contains(html, "<title>Shingo Edge</title>") {
		t.Error("<title> changed; it is the shared header's")
	}
	body := configPageBody(t, html)
	if !strings.Contains(body, "<h1>Configuration</h1>") {
		t.Error("H1 is not Configuration")
	}
	var order []string
	for _, m := range regexp.MustCompile(`<div class="set-sect[^"]*"[^>]*>\s*<h2>([^<]+)</h2>`).FindAllStringSubmatch(body, -1) {
		order = append(order, m[1])
	}
	want := []string{"Station", "Core connection", "PLC link", "Messaging", "Shifts", "Backups", "Account", "Restore from a backup"}
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Errorf("sections = %q, want %q", order, want)
	}
	for _, bad := range []string{` style="`, `<select`, `form-input`, `class="btn`, `class="card"`, `class="card `} {
		if strings.Contains(body, bad) {
			t.Errorf("config page carries %q", bad)
		}
	}
	if m := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`).FindString(body); m != "" {
		t.Errorf("config page carries a hex colour %q", m)
	}
	for _, cls := range regexp.MustCompile(`class="([^"]*)"`).FindAllStringSubmatch(body, -1) {
		for _, c := range strings.Fields(cls[1]) {
			switch {
			case strings.HasPrefix(c, "set-"), c == "on", c == "ok", c == "warn", c == "danger",
				c == "primary", c == "quiet", c == "mid", c == "wide", c == "v", c == "col",
				c == "page-header", c == "modal-overlay", c == "modal", c == "modal-md",
				c == "modal-header", c == "modal-footer", c == "card-body":
			default:
				t.Errorf("class %q is not one of the settings shape's", c)
			}
		}
	}
}

// TestConfigPage_SecretNeverRendered: E8, R3 — the saved secret is not in the
// page; the box says "saved".
func TestConfigPage_SecretNeverRendered(t *testing.T) {
	html := renderConfigPage(t, func(c *config.Config) {
		c.Backup.S3 = config.BackupS3Config{Endpoint: "http://s3.test", Bucket: "b", AccessKey: "AK", SecretKey: "TOPSECRET-1234"}
	}, false)
	if strings.Contains(html, "TOPSECRET-1234") {
		t.Fatal("the saved secret is rendered")
	}
	if !regexp.MustCompile(`data-field="secret_key"[^>]*placeholder="saved"`).MatchString(html) {
		t.Error("secret box does not say saved")
	}
}

// TestConfigPage_LiveWords: the warning when this Edge's own zone is blank,
// and E4's "simulated" PLC title in sim.
func TestConfigPage_LiveWords(t *testing.T) {
	html := renderConfigPage(t, func(c *config.Config) { c.Timezone = ""; c.Sim.Enabled = true }, false)
	if !strings.Contains(html, "no timezone set · showing") {
		t.Error("blank timezone: no warning in the Station title")
	}
	if !regexp.MustCompile(`id="plc-live"><i></i><span>simulated</span>`).MatchString(html) {
		t.Error("sim: PLC link title does not read simulated (E4)")
	}
	html = renderConfigPage(t, func(c *config.Config) { c.Timezone = "America/Chicago" }, false)
	if strings.Contains(html, "no timezone set") {
		t.Error("zone set: warning still shown")
	}
}

// TestConfigPage_RestartNoticeRendered: the notice is computed server-side at
// render from the boot snapshot (the page hands it to settingsPage).
func TestConfigPage_RestartNoticeRendered(t *testing.T) {
	html := renderConfigPage(t, func(c *config.Config) { c.CoreAPI = "http://a.test" }, true)
	if !strings.Contains(html, `data-restart="[]"`) {
		t.Errorf("fresh boot: restart notice not empty")
	}
	html = renderConfigPage(t, func(c *config.Config) { c.CoreAPI = "http://a.test" }, true,
		func(c *config.Config) { c.CoreAPI = "http://b.test" })
	if !strings.Contains(html, `data-restart="[&#34;Core address&#34;]"`) {
		t.Errorf("Core address changed since boot: notice not rendered")
	}
}

// TestConfigPage_LegacyStationIDCarried: a legacy-only Edge's station_id is in
// the page's data (not a field), for the station body to carry; an Edge with
// a UID has none.
func TestConfigPage_LegacyStationIDCarried(t *testing.T) {
	html := renderConfigPage(t, func(c *config.Config) { c.StationUID = ""; c.Messaging.StationID = "plant-a.line-1" }, false)
	if !strings.Contains(html, `data-legacy-station-id="plant-a.line-1"`) {
		t.Error("legacy-only: the legacy station_id is not in the page data")
	}
	if strings.Contains(html, `value="plant-a.line-1"`) {
		t.Error("legacy-only: the legacy id is rendered as an editable field")
	}
	html = renderConfigPage(t, func(c *config.Config) { c.StationUID = "stn-1"; c.Messaging.StationID = "plant-a.line-1" }, false)
	if strings.Contains(html, "data-legacy-station-id") {
		t.Error("UID Edge: legacy id carried though a UID is set")
	}
}
