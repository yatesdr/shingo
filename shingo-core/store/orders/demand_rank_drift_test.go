package orders_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// rankHome is the one file allowed to spell the plant's demand ordering: the
// comparator and its SQL twin live together, and everything else composes them.
const rankHome = "store/orders/demand_rank.go"

// rankSpelling matches the ordering written out by hand in a SQL string —
// `priority DESC` with `created_at` nearby, in either qualified form.
var rankSpelling = regexp.MustCompile(`(?i)priority\s+DESC`)

// TestNoThirdSpellingOfTheDemandRanking is blocking_drift_test's guard applied
// to §9's seam.
//
// The owner's ruling is one sentence: "the demand ranking is one seam ... one
// day we'll expand the demand logic from first come first served to like
// time-to-empty for demand", and the whole value of that is that the change
// lands in ONE place. Round 6 sharpened it: the seam is one comparator with
// exactly TWO callers — the scan's ORDER BY and the steal's under-lock outrank
// check — and "nothing else may spell priority-then-oldest, or time-to-empty
// lands in one site and silently not the other."
//
// A source scan rather than a behaviour test, because the claim is about how the
// ordering is SPELLED. One spelling exists today; that is the dangerous moment
// to write the guard, not the reassuring one — a second copy would agree with
// the first for exactly as long as nobody edited either.
func TestNoThirdSpellingOfTheDemandRanking(t *testing.T) {
	t.Parallel()
	root, err := moduleRootForRank()
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
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if filepath.ToSlash(rel) == rankHome {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(body), "\n") {
			// Prose naming the ordering explains it; that is documentation of the
			// seam, not a second definition of it.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if rankSpelling.MatchString(line) {
				found = append(found, filepath.ToSlash(rel)+":"+strconv.Itoa(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(found) != 0 {
		t.Fatalf("the demand ranking must not be spelled at a query; found %d:\n  %s\n\n"+
			"Compose orders.DemandRankOrderBySQL (in SQL) or orders.DemandRank.Outranks (in Go). "+
			"The ranking has exactly two callers by ruling — the line's ORDER BY and the steal's "+
			"outrank check — because the day it becomes time-to-empty it has to change in one place.",
			len(found), strings.Join(found, "\n  "))
	}
}

// moduleRootForRank walks up until it finds go.mod.
func moduleRootForRank() (string, error) {
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
