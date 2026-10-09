package messaging

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestFeedsPin_ListOpenSupplyRefusalsHasNoCaller pins that Core's open-refusal
// read is called by nothing in shingo-core today: the "set Core broadcasts to
// the edges" its comment names is never built, so a lost refusal message is
// never repaired from Core's record.
//
// A source-shape pin, because "nothing calls it" has no behaviour to drive.
//
// after (F3): exactly one non-test caller, the refusals digest/snapshot
// builder on the heartbeat path (messaging/feeds.go).
func TestFeedsPin_ListOpenSupplyRefusalsHasNoCaller(t *testing.T) {
	call := regexp.MustCompile(`\.ListOpenSupplyRefusals\(`)
	var callers []string
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "node_modules" || strings.HasPrefix(name, ".") && name != ".." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if call.Match(b) {
			callers = append(callers, filepath.ToSlash(path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk shingo-core: %v", err)
	}
	if len(callers) != 0 {
		t.Errorf("ListOpenSupplyRefusals callers = %v, want none at the base (after F3: messaging/feeds.go only)", callers)
	}
}
