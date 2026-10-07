package www

import (
	"regexp"
	"shingo/protocol/testutil"
	"strings"
	"testing"
	"time"
)

// The Core config page is built from the shared settings shape only (U2,
// docs/ui-style-guide/33-settings-pages.md): no inline styles, no native
// <select>, no hex colours, no old .form-/.btn controls, and the save goes to
// the one JSON door.
func TestConfigPage_SettingsShapeOnly(t *testing.T) {
	tmpl, err := templateFS.ReadFile("templates/config.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := staticFS.ReadFile("static/pages/config.js")
	if err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]string{"config.html": string(tmpl), "config.js": string(js)} {
		for _, bad := range []*regexp.Regexp{
			regexp.MustCompile(`style=`),
			regexp.MustCompile(`<select`),
			regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`),
			regexp.MustCompile(`\b(form-group|form-input|btn-primary|btn-sm|card)\b`),
			regexp.MustCompile(`/config/save`),
		} {
			if m := bad.FindString(src); m != "" {
				t.Errorf("%s contains %q", name, m)
			}
		}
	}
	if !strings.Contains(string(tmpl), `class="set-page set-ui"`) {
		t.Error("config.html: the page container is not .set-page.set-ui")
	}
	// Every modal body the page opens carries .set-ui (Core's global input
	// rule would otherwise restyle its boxes).
	if n, m := strings.Count(string(tmpl), `class="modal-overlay"`), strings.Count(string(tmpl), `set-ui" role="dialog"`); n != m {
		t.Errorf("config.html: %d modals, %d with .set-ui on their body", n, m)
	}
}

func TestDurationText(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"0s", "0 s"}, {"10s", "10 s"}, {"45m0s", "45 min"}, {"1h0m0s", "1 h"}, {"1h30m0s", "1 h 30 min"}, {"1m30.5s", "1 min 30 s 500 ms"},
	} {
		d, err := time.ParseDuration(c.in)
		testutil.MustNoErr(t, err, "parse "+c.in)
		if got := durationText(d); got != c.want {
			t.Errorf("durationText(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}
