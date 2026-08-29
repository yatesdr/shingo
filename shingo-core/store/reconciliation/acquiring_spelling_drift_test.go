package reconciliation_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// acquiringSpelling matches the acquiring set written out by hand in a SQL
// string — `'queued','sourcing'` in either order, with or without spaces.
var acquiringSpelling = regexp.MustCompile(
	`'queued'\s*,\s*'sourcing'|'sourcing'\s*,\s*'queued'`)

// TestAcquiringSetHasExactlyOneSpelling is blocking_drift_test's guard applied
// to the other set this codebase keeps two definitions of.
//
// store/reconciliation carried
//
//	const acquiringStatusSQL = `'queued','sourcing'`
//
// under a comment that said "not derived from a protocol helper because there is
// no predicate for 'still acquiring'". There is: protocol.IsAcquiring, with
// protocol.AcquiringStatusSQLList() as its SQL projector, and status_test.go
// already pins the two to each other. So the comment was not a decision, it was
// a fact that had stopped being true — and a second definition of a chapter set
// is a second thing to edit on the day the chapter moves. The tier work moves
// this one: the whole meaning migration turns on which orders count as still
// acquiring their material.
//
// A source scan rather than a behaviour test, because the claim is about how the
// set is SPELLED. The two spellings agree today, which is the dangerous case —
// agreement by coincidence reads exactly like agreement by design until somebody
// edits one of them.
func TestAcquiringSetHasExactlyOneSpelling(t *testing.T) {
	t.Parallel()
	root, err := moduleRootDir()
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	var found []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		for i, line := range strings.Split(string(body), "\n") {
			// Comments name the values to explain them; that is documentation of
			// the set, not a second definition of it.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if acquiringSpelling.MatchString(line) {
				found = append(found, filepath.ToSlash(rel)+":"+strconv.Itoa(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(found) != 0 {
		t.Fatalf("the acquiring set must not be spelled at a query; found %d:\n  %s\n\n"+
			"Use protocol.AcquiringStatusSQLList() (or protocol.IsAcquiring in Go). A hand-written "+
			"copy is a second definition of which orders are still acquiring their material, and "+
			"that is the set the whole meaning migration turns on.",
			len(found), strings.Join(found, "\n  "))
	}
}

// moduleRootDir walks up until it finds go.mod, rather than counting ".." hops.
func moduleRootDir() (string, error) {
	dir, err := filepath.Abs(".")
	if err != nil {
		return "", err
	}
	for {
		if _, serr := os.Stat(filepath.Join(dir, "go.mod")); serr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
