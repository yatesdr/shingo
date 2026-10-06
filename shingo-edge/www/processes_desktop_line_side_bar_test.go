package www

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// processes_desktop_line_side_bar_test.go — the line-side bar over a module's
// cards is drawn in the page's ink, so it shows in both of the desktop's
// themes.
//
// The bar read --station, which shared/tokens.css holds at the same near-white
// in both themes because the station is dark by construction. The desktop
// Processes page draws the same picture and follows the engineer's theme, so
// in the light theme the bar was near-white on a white card and vanished. The
// rule reads --os-station first: the desktop maps it to --text-strong (themed),
// the station maps it to --station itself, so the station is unchanged.
func TestLineSideBarIsThemedOnTheDesktop(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("static", "operator-station", "flow-picture.css"))
	if err != nil {
		t.Fatalf("read flow-picture.css: %v", err)
	}
	press := cssRule(t, string(b), ".os-flow-picture .press")
	if !hasDecl(press, "stroke", `var\(--os-station,\s*var\(--station\)\)`) {
		t.Errorf(".os-flow-picture .press: want stroke: var(--os-station, var(--station)); got %q", press)
	}

	if !regexp.MustCompile(`(?m)^\s*--os-station:\s*var\(--text-strong\);`).MatchString(cssRule(t, desktopCSS(t), ":root")) {
		t.Errorf("processes-desktop.css :root must map --os-station to --text-strong, the themed ink")
	}

	op, err := os.ReadFile(filepath.Join("static", "operator-station", "operator.css"))
	if err != nil {
		t.Fatalf("read operator.css: %v", err)
	}
	if !regexp.MustCompile(`(?m)^\s*--os-station:\s*var\(--station\);`).MatchString(string(op)) {
		t.Errorf("operator.css must map --os-station to --station, so the station's bar keeps its colour")
	}
}
