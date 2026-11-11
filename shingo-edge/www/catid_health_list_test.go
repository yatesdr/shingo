package www

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestCatidHealthListJS runs the health strip's renderer test under node.
func TestCatidHealthListJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the catid-health renderer test")
	}
	script := filepath.Join("testdata", "static", "js", "pages", "catid-health.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("catid-health renderer test failed:\n%s\nerror: %v", out, err)
	}
}

// TestCatidHealthStrip_ToneIsTokens: the strip's rules (.pd-ch*) name no
// literal colour. They carried light-theme hex backgrounds, so on the dark
// page an info item was a white box with grey text; each tone is now its
// status token's rule and wash (docs/ui-style-guide/04-design-tokens.md
// rule 1).
func TestCatidHealthStrip_ToneIsTokens(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join("static", "css", "processes-desktop.css"))
	if err != nil {
		t.Fatal(err)
	}
	literal := regexp.MustCompile(`#[0-9A-Fa-f]{3,8}\b|\brgba?\(|\bhsla?\(`)
	rules := 0
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), ".pd-ch") {
			continue
		}
		rules++
		if m := literal.FindString(line); m != "" {
			t.Errorf("a strip rule names a literal colour (%s): %s", m, strings.TrimSpace(line))
		}
	}
	for _, tone := range []string{".pd-ch-red", ".pd-ch-amber", ".pd-ch-info"} {
		if !strings.Contains(string(b), tone+" {") {
			t.Errorf("no %s rule", tone)
		}
	}
	if rules == 0 {
		t.Fatal("no .pd-ch rule found: the strip's rules moved, and this test reads nothing")
	}
}
