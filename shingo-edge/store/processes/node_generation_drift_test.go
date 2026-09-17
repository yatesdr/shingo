package processes_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// node_generation_drift_test.go — every writer of process_nodes publishes that
// it wrote.
//
// WHY A TREE SCAN AND NOT A UNIT TEST. The station poll no longer reads
// process_nodes; it carries a VERSION derived from what it already holds, and
// processes.NodeGeneration is the one input to that version which cannot be
// derived — it has to be published by the writers. A writer that forgets is
// invisible: nothing fails, no log appears, and a board goes on drawing a cell
// that has a position it no longer has (or missing one it just got) until
// something else moves the version. That is the exact failure mode a unit test
// of the writers we KNOW about cannot cover, because the defect is a writer
// nobody thought of.
//
// FILE-LEVEL, AND THAT IS ITS LIMIT. It asserts that a file writing
// process_nodes also mentions the counter — not that this particular statement
// bumps it. A second writer added to a file that already bumps slips past. The
// value is in the case that actually happens: a new writer in a new file, by
// somebody who has not read this. When you are that person and this test sent
// you here, the rule is one line after the write lands:
// processes.BumpNodeGeneration().
func TestProcessNodeWritersPublishTheirGeneration(t *testing.T) {
	root := moduleRoot(t)
	write := regexp.MustCompile(`(?i)(INSERT INTO|UPDATE|DELETE FROM)\s+process_nodes\b`)

	// MIGRATIONS ARE EXEMPT, and the reason is the clock rather than a
	// judgement about their code: store/migrations*.go runs inside store.Open,
	// before the router is built and before any board can poll. There is no
	// version held by anybody to invalidate.
	exempt := func(rel string) bool {
		rel = filepath.ToSlash(rel)
		return strings.HasPrefix(rel, "store/migrations") ||
			strings.HasPrefix(rel, "store/schema/")
	}

	var missing []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == "node_modules" || name == "testdata" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if exempt(rel) {
			return nil
		}
		blob, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		src := string(blob)
		if !write.MatchString(src) {
			return nil
		}
		if strings.Contains(src, "NodeGeneration") {
			return nil
		}
		missing = append(missing, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(missing) > 0 {
		t.Errorf("these files write process_nodes and never publish the generation:\n  %s\n\n"+
			"A station poll carries a cell-picture VERSION rather than the picture, and\n"+
			"processes.NodeGeneration is the half of that version nothing can derive. A write\n"+
			"that does not bump it leaves every board drawing the cell as it was, with nothing\n"+
			"saying so. Add processes.BumpNodeGeneration() after the write lands (after the\n"+
			"COMMIT, if it is in a transaction).",
			strings.Join(missing, "\n  "))
	}
}

// moduleRoot walks up from this package to the directory holding go.mod, so
// the scan covers the whole edge module rather than this one package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("no go.mod above %s; this scan has to see the whole module", dir)
	return ""
}
