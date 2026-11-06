package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// runTagSuggestJS runs the tag combo's option test under node: the checks
// whose names start with only, or every check.
func runTagSuggestJS(t *testing.T, only string) {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping the tag-suggest test")
	}
	out, err := exec.Command(nodePath, filepath.Join("static", "js", "pages", "tag-suggest.test.js"), only).CombinedOutput()
	if err != nil {
		t.Fatalf("tag-suggest test failed:\n%s\nerror: %v", out, err)
	}
}

// TestTagSuggestJS runs every option check.
func TestTagSuggestJS(t *testing.T) { runTagSuggestJS(t, "") }

// TestTagSuggest_StationKindOffersTheConventionalMember: the station combo
// offers each station struct's conventional count member, by name only (no
// live read), operator-load stations first.
func TestTagSuggest_StationKindOffersTheConventionalMember(t *testing.T) {
	runTagSuggestJS(t, "station: offers")
}

// TestTagSuggest_NeverOffersAGunTimerOrIndex: the station combo never offers a
// tag shaped like a gun, tip, timer, cycle, index, FIFO, management or marker
// count.
func TestTagSuggest_NeverOffersAGunTimerOrIndex(t *testing.T) {
	runTagSuggestJS(t, "station: never")
}
