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
// read (store.DB.ListOpenSupplyRefusals): exactly one non-test caller,
// buildRefusalsFeed in messaging/feeds.go, the FeedRefusals build whose digest
// the heartbeat ack quotes and whose snapshot goes to an Edge that differs.
//
// Until "fix(core,edge): a lost supply refusal message heals from Core's open
// set" nothing in shingo-core called it, so a lost refusal message was never
// repaired from Core's record.
//
// A source-shape pin, because "who calls it" has no behaviour to drive. A
// second caller turns it red: add it to want once it is meant to read the
// same open set.
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
	want := []string{"../messaging/feeds.go"} // buildRefusalsFeed
	if !reflect.DeepEqual(callers, want) {
		t.Errorf("ListOpenSupplyRefusals callers = %v, want %v", callers, want)
	}
}
