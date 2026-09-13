package processes

import (
	"strings"
	"testing"

	"shingoedge/domain"
)

// splitColumns reads a SQL column list into its names.
func splitColumns(list string) []string {
	var out []string
	for _, col := range strings.Split(list, ",") {
		if col = strings.TrimSpace(col); col != "" {
			out = append(out, col)
		}
	}
	return out
}

// cloneOnlyExclusions are columns the FINGERPRINT reads that the CLONE list
// deliberately leaves out. Each one is a stored column of a live claim — so a
// preview has to see it change — that a clone must nonetheless not carry to a
// brand-new style.
//
//	keep_staged  the option is withheld: UpsertClaim, ValidateNodeClaim and the
//	             changeover planner all refuse a claim that asks for it
//	             (domain.KeepStagedWithheld), and a clone takes the column's
//	             default rather than spreading a legacy flag. Stored rows keep
//	             theirs, which is exactly why the fingerprint still reads it —
//	             a save that clears one is a change to the flow.
var cloneOnlyExclusions = map[string]string{
	"keep_staged": "withheld at every write door; a clone takes the default",
}

// fingerprintExclusions are the mirror: columns a CLONE carries that the
// FINGERPRINT deliberately does not read. A preview is a plan, and the
// fingerprint's question is "would this plan still be the same plan" — so a
// column no planner and no runtime reads has no business invalidating one.
//
//	key_task  the branch removed its one runtime forward. Nothing reads the
//	          column, so editing it changes no behaviour — and while it was
//	          hashed, editing it invalidated every open preview on the press.
//	          The column stays (and a clone still carries it) because the data
//	          is still there and Core's mirror still has the field.
var fingerprintExclusions = map[string]string{
	"key_task": "no runtime reader on this tree; hashing it invalidated previews for a column that drives nothing",
}

// TestFlowFingerprintColumnsAreCloneClaimColumnsPlusID holds the two lists
// together: domain.FlowFingerprint serialises a claim on the columns
// cloneClaimColumns names, plus id, plus the deliberate clone exclusions above.
// A column added to the INSERT and to the clone list without reaching the
// fingerprint would be one a preview could not see change; one added to the
// fingerprint and named nowhere here would be a column the fingerprint reads
// that nothing accounts for.
//
// IT WAS A PLAIN EQUALITY, and that was a proxy rather than the property. The
// two lists answer different questions — "what is a claim" and "what does a
// copy carry" — and they were the same list only until the first column earned
// a reason to be in one and not the other. The exclusions are named, with the
// reason, so adding a second one is a decision somebody writes down.
func TestFlowFingerprintColumnsAreCloneClaimColumnsPlusID(t *testing.T) {
	t.Parallel()
	clone := splitColumns(cloneClaimColumns)
	got := domain.FlowFingerprintColumns()

	inFingerprint := map[string]bool{}
	for _, c := range got {
		inFingerprint[c] = true
	}
	// (a) EVERY CLONE COLUMN IS IN THE FINGERPRINT. One that is not is a
	// column a preview cannot see change.
	for _, c := range clone {
		if inFingerprint[c] {
			continue
		}
		if _, named := fingerprintExclusions[c]; !named {
			t.Errorf("cloneClaimColumns names %q and the fingerprint does not read it — "+
				"a save that changed it would not be seen as stale. Add it to the fingerprint, "+
				"or say in fingerprintExclusions why no preview needs to see it move", c)
		}
	}
	// An exclusion that has stopped being one is a stale note.
	for c, why := range fingerprintExclusions {
		if inFingerprint[c] {
			t.Errorf("%q is named a fingerprint exclusion (%s) and the fingerprint reads it; drop the note", c, why)
		}
	}
	// (b) EVERY FINGERPRINT COLUMN IS ACCOUNTED FOR: id, a clone column, or a
	// named exclusion.
	inClone := map[string]bool{}
	for _, c := range clone {
		inClone[c] = true
	}
	for _, c := range got {
		if c == "id" || inClone[c] {
			continue
		}
		if _, named := cloneOnlyExclusions[c]; !named {
			t.Errorf("the fingerprint reads %q, which is neither a clone column nor a named "+
				"exclusion — add it to cloneClaimColumns or say here why a clone must not carry it", c)
		}
	}
	// And an exclusion that has stopped being one is a stale note.
	for c := range cloneOnlyExclusions {
		if inClone[c] {
			t.Errorf("%q is named a clone-only exclusion and is in cloneClaimColumns; drop the note", c)
		}
		if !inFingerprint[c] {
			t.Errorf("%q is named a clone-only exclusion and the fingerprint does not read it either; "+
				"it is simply gone, so drop the note", c)
		}
	}
	// (c) THE SHARED COLUMNS KEEP THEIR ORDER. fingerprintClaim writes values
	// in this order and the hash is that string, so a reorder changes every
	// fingerprint on the plant at once.
	var shared, cloneShared []string
	for _, c := range got {
		if inClone[c] {
			shared = append(shared, c)
		}
	}
	for _, c := range clone {
		if _, skipped := fingerprintExclusions[c]; !skipped {
			cloneShared = append(cloneShared, c)
		}
	}
	if len(shared) != len(cloneShared) {
		t.Fatalf("shared columns = %d, clone columns (excluding named exclusions) = %d", len(shared), len(cloneShared))
	}
	for i := range shared {
		if shared[i] != cloneShared[i] {
			t.Errorf("shared column %d is %q in the fingerprint and %q in the clone list — "+
				"the order is the hash", i, shared[i], cloneShared[i])
		}
	}
	if len(got) == 0 || got[0] != "id" {
		t.Errorf("the fingerprint's first column is %v, want id", got[:1])
	}
}
