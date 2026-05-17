// archtest_test.go — structural invariants enforced by grep across
// shingo-edge.
//
// These tests pin the UOP package's chokepoint invariants so future
// edits don't accidentally bypass the verb interface. Cheap CI test
// (no framework, just os.WalkDir + strings.Contains); runs in the
// uop package's test binary alongside accumulator_test.go.
package uop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestArch_NoDirectRecordBinOrRecordBucket asserts that no production
// file outside shingo-edge/uop/ calls inventoryDelta.RecordBin or
// inventoryDelta.RecordBucket directly. Every delta emission must
// route through a named intent verb (Consumed, Produced, Fallthrough,
// CaptureToLineside, AdjustBucket, Backfill — see mutator.go and
// the *.go files in this package).
//
// This invariant pins the value of the Phase 3a refactor: surfacing
// intent at the call site (the verb name documents the plant event)
// instead of having raw RecordBin/RecordBucket sprinkled with
// hand-picked reason strings.
//
// Test files (*_test.go) are exempt — the fakeDeltaSink in
// engine/wiring_counter_delta_test.go intentionally records and
// re-emits to track call shapes. The accumulator's own internal
// usage is also exempt (this file lives in uop/).
func TestArch_NoDirectRecordBinOrRecordBucket(t *testing.T) {
	t.Parallel()
	root := edgeRepoRoot(t)
	var bad []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip the uop package itself — accumulator.go uses
			// recordBin/recordBucket internally (lowercase, private).
			if filepath.Base(path) == "uop" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		for _, pat := range []string{
			"inventoryDelta.RecordBin",
			"inventoryDelta.RecordBucket",
		} {
			if strings.Contains(text, pat) {
				rel, _ := filepath.Rel(root, path)
				bad = append(bad, rel+" contains "+pat)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(bad) > 0 {
		t.Errorf("direct RecordBin/RecordBucket calls found outside uop/:\n  %s\n\n"+
			"Every delta emission must route through a named verb on uop.Mutator.\n"+
			"Add a new verb if needed; don't bypass.", strings.Join(bad, "\n  "))
	}
}

// edgeRepoRoot returns the absolute path to shingo-edge/ by walking
// upward from the test's CWD until it finds a go.mod whose first line
// declares the shingoedge module.
func edgeRepoRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := cwd
	for {
		modPath := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(modPath); err == nil {
			if strings.Contains(string(data), "module shingoedge") {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no shingoedge go.mod found walking up from %s", cwd)
		}
		dir = parent
	}
}
