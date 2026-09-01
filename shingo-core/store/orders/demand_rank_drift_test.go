package orders_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"shingocore/store/orders"
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
// The demand ranking is one seam (§9): first-come-first-served today,
// time-to-empty later, and the whole value of that is that the change lands in
// ONE place. Round 6 sharpened it to a shape — one comparator with exactly TWO
// callers, the scan's ORDER BY and the steal's under-lock outrank check.
// Anything else that spells priority-then-oldest is a site time-to-empty would
// silently not reach.
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

// TestDemandRankOrderBySQL_IsTotal pins the tiebreak, which the source scan
// above cannot see: it counts SPELLINGS, and a spelling that stops at
// (priority, created_at) is still exactly one.
//
// Without a total order the scan hands back tied rows in whatever sequence
// Postgres chose that call, so the line's order is not a fact about the plant —
// and TestDemandRank_TheScanOrderIsTheComparator, which walks the result
// pairwise against the comparator, has no answer for a pair neither side
// outranks. The tiebreak is the row id because ids ascend with creation, so it
// agrees with the ageing guarantee instead of cutting across it.
func TestDemandRankOrderBySQL_IsTotal(t *testing.T) {
	t.Parallel()
	got := orders.DemandRankOrderBySQL()
	for _, want := range []string{"priority DESC", "created_at ASC", "id ASC"} {
		if !strings.Contains(got, want) {
			t.Errorf("the line's ORDER BY is %q, and it is missing %q. "+
				"Priority, then age, then the row id — the last one is what makes the ranking TOTAL, "+
				"so two demands the plant cannot otherwise separate still come back in one order "+
				"every time.", got, want)
		}
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
