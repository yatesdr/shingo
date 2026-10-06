package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestProcessesDesktopPopoverActs runs the popover-acts pin under node.
//
// The Processes page keeps the set of acts that must stop the click before it
// reaches the document in POPOVER_ACTS (processes-desktop.js): onClick is bound
// to #pd-root, boot() puts a closer on the DOCUMENT, and an act that opens
// #pd-pop without stopping the event has its menu hidden again one listener
// later — the "clickable, nothing happens" report the style row's ⋯ lived with
// for three rounds. The pin walks a real bubbling click through the page's own
// listeners and asserts the menu is still open after the bubble ends.
func TestProcessesDesktopPopoverActs(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the popover-acts pin")
	}
	script := filepath.Join("static", "js", "pages", "processes-desktop.popover-acts.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("popover-acts pin failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("popover acts: %s", out)
}
