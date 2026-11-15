package www

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// TestStation_NoHoverOnlyWords: the station is a touch panel, and a touch
// panel has no hover. A title attribute, a title property or an SVG <title>
// on the station's glass is a sentence nobody at the line can read, so the
// station's scripts write none: a reason goes where a tap shows it (the
// tile's panel, a confirm) or it goes.
//
// Counted per file, comments dropped (readJS). The one file allowed is the
// flow picture: renderFlowPicture is shared with Processes
// (processes-desktop.js imports it), and its <title>s carry a cut node's whole
// name and a staging slot's role on the desktop, where a mouse can read them.
// EXPIRY: when the station's flow panel stops drawing renderFlowPicture's
// SVG, or the picture stops writing <title>.
func TestStation_NoHoverOnlyWords(t *testing.T) {
	t.Parallel()
	sink := regexp.MustCompile(`title="|\.title\s*=[^=]|<title>`)
	entries, err := os.ReadDir(filepath.Join("static", "operator-station"))
	if err != nil {
		t.Fatalf("read the station's scripts: %v", err)
	}
	got := map[string]int{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".js" {
			continue
		}
		for i, line := range strings.Split(readJS(t, e.Name()), "\n") {
			if sink.MatchString(line) {
				got[e.Name()]++
				t.Logf("%s:%d %s", e.Name(), i+1, strings.TrimSpace(line))
			}
		}
	}
	// Until 2026-11-14 the tiles' chips, a disabled button's reason, the
	// release panel's two buttons and the composer's verdicts carried theirs
	// too (14 in all, the flow picture's 4 among them).
	want := map[string]int{
		"operator-flow.js": 4,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hover-only words on the station, per file: %v, want %v (the lines are logged above)", got, want)
	}
}
