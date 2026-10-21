package protocol

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWaitOwnershipIsDeclaredOnce: the wait kinds, the ownership predicate and
// the vacate stamp's type are declared in this package and nowhere else. Core
// and the Edge read the same field off the same steps, and a second spelling
// on either side is how the two come to disagree about who may advance a
// wait — the station offering a RELEASE Core refuses, or Core holding a leg
// the station believes it released. Aliases are fine (plan.Vacate =
// VacateStamp, WaitKindLane = protocol.WaitKindLane); a literal value, a
// second predicate or a second struct is not.
func TestWaitOwnershipIsDeclaredOnce(t *testing.T) {
	t.Parallel()
	forbidden := []struct {
		re   *regexp.Regexp
		what string
	}{
		{regexp.MustCompile(`(?i)waitkind\w*\s*(=|==|!=)\s*"[^"]`),
			"declares or compares a wait kind by its literal value; use protocol.WaitKind*"},
		{regexp.MustCompile(`[kK]ind\w*\s*(==|!=)\s*"(station|lane|vacate)"`),
			"compares a kind to a wait kind's literal value; use protocol.WaitKind*"},
		{regexp.MustCompile(`(?i)func\s+(\([^)]*\)\s*)?\w*waitkind\w*\s*\(\s*\w+\s+string\s*\)\s*bool`),
			"defines a predicate over a wait kind; use protocol.IsCoreWaitKind / IsStationWaitKind"},
		{regexp.MustCompile(`WaitKindLane.*\|\|.*WaitKindVacate|WaitKindVacate.*\|\|.*WaitKindLane`),
			"re-spells the ownership predicate; use protocol.IsCoreWaitKind"},
		{regexp.MustCompile(`type\s+\w*[vV]acate\w*\s+struct\b`),
			"declares a vacate stamp struct; alias protocol.VacateStamp"},
	}
	checked := 0
	for _, module := range []string{"../shingo-core", "../shingo-edge"} {
		err := filepath.WalkDir(module, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "node_modules" || name == "testdata" || strings.HasPrefix(name, ".") {
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
			checked++
			for i, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "//") {
					continue
				}
				for _, f := range forbidden {
					if f.re.MatchString(line) {
						t.Errorf("%s:%d %s — the kinds, the predicate and the stamp live in "+
							"protocol/waitkind.go:\n\t%s", filepath.ToSlash(path), i+1, f.what, strings.TrimSpace(line))
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v (if the module moved, repoint this test)", module, err)
		}
	}
	if checked < 100 {
		t.Fatalf("read only %d Go files across shingo-core and shingo-edge — the walk missed the modules "+
			"and the test is vacuous", checked)
	}
}
