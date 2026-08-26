//go:build docker

package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// folder_predicate_census_test.go — "does this order own legs?" is spelled once.
//
// ── WHY A FENCE AT ALL ────────────────────────────────────────────────────
//
// The predicate is `EXISTS (SELECT 1 FROM orders leg WHERE leg.parent_order_id
// = <o>.id)`. It is small, it is obvious, and that is exactly what makes it get
// re-typed inline: it looks too trivial to be worth sharing right up until two
// copies disagree.
//
// This codebase has already paid that bill once, in the same shape. Lane
// reachability was spelled in SQL and again as a Go loop in
// dispatch/reshuffle.go; they disagreed about NULL depth, so a depth-less
// occupied child made every slot in its lane reachable to one side and blocked
// to the other. The disagreement was silent for months. helpers.ShallowerInSameLane
// is the answer that came out of it, and OwnsNoCargoSQL sits beside it for the
// same reason.
//
// ── WHAT THE SECOND SPELLING COSTS HERE ───────────────────────────────────
//
// The competing spelling is not another SQL clause — it is `order.BinID == nil`
// in Go, at seven sites. That predicate is TRUE OF A COORDINATOR AND TRUE OF A
// DEFECT: a folder's bin_id is NULL permanently and correctly, and a single-bin
// order whose planMove never persisted one is NULL because something broke.
// Measured at the pin on the lane-stress rig 2026-08-13, the bin-state strip
// reported twelve anomalies — every one a compound parent whose legs had
// delivered correctly, zero true positives — and read "Core degraded" all run.
//
// Those seven are SHADOWED, not yet cut (service/folder_shadow.go). This census
// guards the SQL half so the shared spelling cannot quietly grow a second copy
// while that window runs.

// folderPredicateNeedle is the giveaway substring of a hand-rolled copy. It is
// deliberately the JOIN CONDITION rather than the whole clause: a re-speller
// will change the alias, the whitespace and the EXISTS/NOT EXISTS polarity long
// before they change which column they join on.
const folderPredicateNeedle = "FROM orders leg WHERE leg.parent_order_id"

// folderPredicateOwner is the one file allowed to contain it.
const folderPredicateOwner = "internal/helpers/lane_reachability.go"

// TestFolderPredicate_HasExactlyOneSpelling is the fence.
//
// MUTATION (verified): paste the clause into any other store file and this
// names the file and line. Rename the helper's own copy and the owner assertion
// fires, so the census cannot pass by finding nothing anywhere.
func TestFolderPredicate_HasExactlyOneSpelling(t *testing.T) {
	t.Parallel()
	root := "."

	var offenders []string
	ownerHits := 0
	filesScanned := 0

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		filesScanned++
		rel := filepath.ToSlash(path)
		for i, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, folderPredicateNeedle) {
				continue
			}
			if strings.HasSuffix(rel, folderPredicateOwner) {
				ownerHits++
				continue
			}
			offenders = append(offenders, rel+":"+itoa(i+1))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk store tree: %v", err)
	}

	// A census that scanned nothing must not read as a census that found
	// nothing. Same rule the instruments in this batch are built on.
	if filesScanned == 0 {
		t.Fatal("the census scanned ZERO files, so its silence means nothing")
	}

	// And it must find the spelling where it is supposed to live, or it is
	// asserting the absence of a string that simply moved.
	if ownerHits == 0 {
		t.Fatalf("the predicate was not found in its owner (%s) at all. Either it moved — in which "+
			"case update folderPredicateOwner in the same commit — or the needle no longer matches "+
			"it, which would make every other assertion in this test vacuous.", folderPredicateOwner)
	}

	if len(offenders) > 0 {
		t.Errorf("the coordinator predicate is spelled at %d site(s) outside %s: %s\n\n"+
			"Whether an order owns legs is the fact that decides whose bin it is, and it must have "+
			"ONE spelling: helpers.OwnsNoCargoSQL for SQL callers, db.OrderOwnsNoCargo for Go ones. "+
			"Two copies of a predicate is how the SQL and the Go loop came to disagree about lane "+
			"reachability, and that disagreement was silent for months.",
			len(offenders), folderPredicateOwner, strings.Join(offenders, ", "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
