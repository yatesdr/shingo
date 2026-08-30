package binresolver

import (
	"strings"
	"testing"

	"shingocore/store/nodes"
	"shingocore/store/payloads"
	"shingocore/store/reservations"
)

// empties_only_store_test.go — THE ADMISSION RULE, AND THE ONLY THING THAT
// CERTIFIES IT.
//
// A group that declares a maintain level is an empties bank: every level counts
// EMPTY carriers of a bin type, and the keeper tops the group up with empty
// carriers. A store carrying a payload does not belong in one — it is a carrier
// the keeper's own count cannot see, standing in a slot the keeper will then try
// to refill.
//
// THIS IS A FAIL-SAFE, AND ITS RUNTIME POPULATION MAY BE EMPTY. The path that
// actually put labelled carriers into a press empties bank was an ordering race
// at the operator's CLEAR (shingo-edge engine/operator_bin_ops.go), and that
// race is closed in the same change as this refusal. So a sim run that never
// fires this arm has proved nothing about it — a totally broken arm produces the
// same clean board. These tests ARE the certification; do not substitute a run
// for them.

func emptiesOnlyStore() (*fakeStore, *nodes.Node) {
	f := newFakeStore()
	grp := &nodes.Node{ID: 900, Name: "SYN_PRESS_EMPTIES", Enabled: true, IsSynthetic: true}
	slot := &nodes.Node{ID: 901, Name: "PEB_001", Enabled: true, ParentID: &grp.ID}
	f.nodes[grp.ID] = grp
	f.nodes[slot.ID] = slot
	f.children[grp.ID] = []*nodes.Node{slot}
	f.maintainLevels = map[int64][]nodes.MaintainLevel{
		900: {{GroupNodeID: 900, BinTypeID: 7, BinTypeCode: "PEB-45x58", Want: 4}},
	}
	// Deliberately BELOW level and with a free slot, so nothing else in
	// ResolveStore can be the reason a store is refused.
	f.emptyCounts = map[groupKey]int{{900, "PEB-45x58"}: 0}
	return f, grp
}

// A LABELLED STORE AIMED AT AN EMPTIES BANK IS REFUSED — with a free slot and
// the group below its level, so the refusal can only be the payload.
func TestEmptiesOnly_LabelledStoreRefused(t *testing.T) {
	t.Parallel()
	f, grp := emptiesOnlyStore()
	r := &GroupResolver{DB: f}

	if _, err := r.ResolveStore(grp, "PANEL-A", nil, reservations.Anyone); err == nil {
		t.Fatal("a PANEL-A store landed in SYN_PRESS_EMPTIES. A maintained group's level " +
			"counts EMPTY carriers, so a payload-bearing bin parked there is invisible to " +
			"the keeper, which then fetches another carrier to stand beside it")
	} else if !strings.Contains(err.Error(), "empties-only node group SYN_PRESS_EMPTIES") {
		// The exact phrase is load-bearing: classifyResolutionError matches it to
		// return ResolutionCapacity, which is what makes admission try the
		// group's overflow destination instead of terminal-failing the order.
		// It must also END with the group name — groupFromResolutionError takes
		// everything after the last "node group ".
		t.Errorf("err = %q, want the empties-only phrasing ending in the group name", err)
	}
}

// THE KEEPER CANNOT BE REFUSED BY ITS OWN RULE. Its stores pass an empty payload
// code, which is the whole point of the bank.
func TestEmptiesOnly_EmptyCarrierStillLands(t *testing.T) {
	t.Parallel()
	f, grp := emptiesOnlyStore()
	r := &GroupResolver{DB: f}

	res, err := r.ResolveStore(grp, "", nil, reservations.Anyone)
	if err != nil {
		t.Fatalf("an EMPTY carrier was refused by the empties-only rule: %v. The refusal "+
			"would close the bank against the keeper that fills it", err)
	}
	if res.Node.Name != "PEB_001" {
		t.Errorf("resolved to %s, want PEB_001", res.Node.Name)
	}
}

// A GROUP WITH NO DECLARED LEVEL IS NOT AN EMPTIES BANK, and takes payloads
// exactly as it did before. This is every group in every plant today.
func TestEmptiesOnly_UndeclaredGroupUnaffected(t *testing.T) {
	t.Parallel()
	f, grp := emptiesOnlyStore()
	f.maintainLevels = nil
	r := &GroupResolver{DB: f}

	if _, err := r.ResolveStore(grp, "PANEL-A", nil, reservations.Anyone); err != nil {
		t.Fatalf("a payload store was refused by a group with NO declared level: %v. The "+
			"rule keys on the level, not on the group's name or its contents", err)
	}
}

// ── The flat-slot payload declaration, which both flat branches ignored ──────

func flatPayloadStore(algo string) (*fakeStore, *nodes.Node) {
	f := newFakeStore()
	grp := &nodes.Node{ID: 910, Name: "FLAT-GRP", Enabled: true, IsSynthetic: true}
	slot := &nodes.Node{ID: 911, Name: "FLAT-SLOT", Enabled: true, ParentID: &grp.ID}
	f.nodes[grp.ID] = grp
	f.nodes[slot.ID] = slot
	f.children[grp.ID] = []*nodes.Node{slot}
	// The slot declares it holds CLIP and nothing else.
	f.effPayloads[slot.ID] = []*payloads.Payload{{Code: "CLIP"}}
	if algo != "" {
		f.setProp(grp.ID, "store_algorithm", algo)
	}
	return f, grp
}

// A FLAT SLOT'S PAYLOAD DECLARATION IS HONOURED — LKND. Both lane branches read
// node_payloads for stores and neither flat branch did, so a group of flat slots
// that declared what each slot holds had those declarations silently ignored.
func TestFlatPayloadDeclaration_LKNDRefusesMismatch(t *testing.T) {
	t.Parallel()
	f, grp := flatPayloadStore("")
	r := &GroupResolver{DB: f}

	if _, err := r.ResolveStore(grp, "STUD", nil, reservations.Anyone); err == nil {
		t.Fatal("a STUD store landed in a flat slot declared for CLIP. An identically " +
			"configured LANE would have refused it; the flat branch never grew the clause")
	}
	if _, err := r.ResolveStore(grp, "CLIP", nil, reservations.Anyone); err != nil {
		t.Fatalf("the DECLARED payload was refused at its own slot: %v", err)
	}
}

// Same rule, DPTH. "Packs back-to-front regardless of payload" is about ordering
// preference, not about ignoring a slot that declared what it holds.
func TestFlatPayloadDeclaration_DPTHRefusesMismatch(t *testing.T) {
	t.Parallel()
	f, grp := flatPayloadStore(StoreDPTH)
	r := &GroupResolver{DB: f}

	if _, err := r.ResolveStore(grp, "STUD", nil, reservations.Anyone); err == nil {
		t.Fatal("DPTH stored STUD into a flat slot declared for CLIP")
	}
	if _, err := r.ResolveStore(grp, "CLIP", nil, reservations.Anyone); err != nil {
		t.Fatalf("DPTH refused the DECLARED payload at its own slot: %v", err)
	}
}

// AN UNDECLARED SLOT ACCEPTS EVERYTHING. node_payloads is a restriction, not a
// whitelist that defaults closed — reversing that would close every default slot
// in the system.
func TestFlatPayloadDeclaration_UndeclaredSlotAcceptsAnything(t *testing.T) {
	t.Parallel()
	f, grp := flatPayloadStore("")
	f.effPayloads = nil
	r := &GroupResolver{DB: f}

	if _, err := r.ResolveStore(grp, "STUD", nil, reservations.Anyone); err != nil {
		t.Fatalf("an undeclared flat slot refused a store: %v", err)
	}
}
