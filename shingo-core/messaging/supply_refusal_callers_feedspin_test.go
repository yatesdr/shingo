package messaging

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// TestFeedsPin_ListOpenSupplyRefusalsCallers pins who calls Core's open-refusal
// read. At the base nothing in shingo-core did: the "set Core broadcasts to the
// edges" its comment names was never built, so a lost refusal message was never
// repaired from Core's record.
//
// A source-shape pin, because "who calls it" has no behaviour to drive.
//
// F3 (flipped): exactly one non-test caller, the refusals digest/snapshot
// builder on the heartbeat path (messaging/feeds.go).
func TestFeedsPin_ListOpenSupplyRefusalsCallers(t *testing.T) {
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
	want := []string{"../messaging/feeds.go"} // F3; base: none
	if !reflect.DeepEqual(callers, want) {
		t.Errorf("ListOpenSupplyRefusals callers = %v, want %v (base: none)", callers, want)
	}
}
