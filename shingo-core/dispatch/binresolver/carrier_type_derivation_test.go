package binresolver

import (
	"errors"
	"testing"

	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// carrier_type_derivation_test.go — THE GATE THAT WAS NEVER REACHED.
//
// binTypeAllowed has been correct since it was written and did nothing for just
// as long: the carrier type arrived as a parameter, five of the seven store call
// sites passed nil, and the one that did pass it (the level keeper) was added
// long after. Every test in this package predating these therefore proves the
// PREDICATE and none of them prove the WIRE.
//
// These do. Each drives ResolveStore with binTypeID=nil — exactly what every
// ordinary move, retrieve and complex step passes — and asserts the fence bites
// anyway, because the resolver now derives the type from the asking order.
//
// THE FIXTURE IS HOPKINSVILLE'S. Two declared types, a supermarket group whose
// children are split between them, which is the configuration the fence was
// asked for and the one that has never been enforceable.

const (
	btTote = int64(2) // TOTE-2415
	btKD   = int64(1) // 45x48 KD
)

// fencedGroup builds a group with two children: one that accepts only totes,
// one that accepts only knockdowns. Both are free.
func fencedGroup(t *testing.T) (*fakeStore, *nodes.Node, *nodes.Node, *nodes.Node) {
	t.Helper()
	f := newFakeStore()
	grp := &nodes.Node{ID: 300, Name: "Supermarket Empty Totes", Enabled: true, IsSynthetic: true}
	toteSlot := &nodes.Node{ID: 301, Name: "SMN_05", Enabled: true, ParentID: &grp.ID}
	kdSlot := &nodes.Node{ID: 302, Name: "SMN_06", Enabled: true, ParentID: &grp.ID}
	f.nodes[grp.ID] = grp
	f.nodes[toteSlot.ID] = toteSlot
	f.nodes[kdSlot.ID] = kdSlot
	f.children[grp.ID] = []*nodes.Node{toteSlot, kdSlot}
	f.effBinTypes = map[int64][]*bins.BinType{
		toteSlot.ID: {{ID: btTote, Code: "TOTE-2415"}},
		kdSlot.ID:   {{ID: btKD, Code: "45x48 KD"}},
	}
	return f, grp, toteSlot, kdSlot
}

// A KNOCKDOWN LANDS IN THE KNOCKDOWN SLOT, with nobody passing a type.
//
// This is the whole feature in one assertion. Before the derivation the resolver
// saw binTypeID=nil, skipped the check entirely, and picked whichever slot ranked
// best — which for two equally empty children is the first one, the tote slot.
func TestResolveStore_DerivesCarrierType_FencesKnockdown(t *testing.T) {
	t.Parallel()
	f, grp, _, kdSlot := fencedGroup(t)
	f.carrierTypes = map[int64]int64{42: btKD}

	r := &GroupResolver{DB: f}
	got, err := r.ResolveStore(grp, "", nil, reservations.AskerFor(42, 42))
	if err != nil {
		t.Fatalf("ResolveStore: %v", err)
	}
	if got.Node.ID != kdSlot.ID {
		t.Errorf("a knockdown resolved to %s, want %s.\n\n"+
			"The per-node Allowed Bin Types fence was not consulted. Nobody passed a "+
			"carrier type — which is what every ordinary order does — so the resolver "+
			"has to derive it from the asking order or the fence stays decorative.",
			got.Node.Name, kdSlot.Name)
	}
}

// And the other way, so the first case is not passing on slot ordering.
func TestResolveStore_DerivesCarrierType_FencesTote(t *testing.T) {
	t.Parallel()
	f, grp, toteSlot, _ := fencedGroup(t)
	f.carrierTypes = map[int64]int64{42: btTote}

	r := &GroupResolver{DB: f}
	got, err := r.ResolveStore(grp, "", nil, reservations.AskerFor(42, 42))
	if err != nil {
		t.Fatalf("ResolveStore: %v", err)
	}
	if got.Node.ID != toteSlot.ID {
		t.Errorf("a tote resolved to %s, want %s", got.Node.Name, toteSlot.Name)
	}
}

// A GROUP WHOSE EVERY SLOT REFUSES THIS CARRIER IS "FULL", in the queue-on-full
// sense — the phrasing classifyResolutionError reads as CAPACITY, so the order
// parks and retries instead of failing the operator's action.
func TestResolveStore_AllSlotsFenced_ParksAsCapacity(t *testing.T) {
	t.Parallel()
	f, grp, _, _ := fencedGroup(t)
	const btWire = int64(3) // 48x54, declared nowhere in this group
	f.carrierTypes = map[int64]int64{42: btWire}

	r := &GroupResolver{DB: f}
	_, err := r.ResolveStore(grp, "", nil, reservations.AskerFor(42, 42))
	if err == nil {
		t.Fatal("a carrier no slot in the group accepts was placed anyway")
	}
	if got := err.Error(); got != "no available slot in node group Supermarket Empty Totes" {
		t.Errorf("err = %q, want the capacity phrasing.\n\n"+
			"A different sentence classifies as structural and FAILS the push rather "+
			"than parking it, which turns a fence into a cancelled order.", got)
	}
}

// THE OVERRIDE STILL WINS. A level keeper's ask names its type before any carrier
// exists and its order may not be readable by id yet, so an explicit parameter
// must not be second-guessed by the derivation.
func TestResolveStore_ExplicitTypeOverridesDerivation(t *testing.T) {
	t.Parallel()
	f, grp, toteSlot, _ := fencedGroup(t)
	f.carrierTypes = map[int64]int64{42: btKD} // the order says knockdown

	bt := btTote // the caller says tote, and the caller wins
	r := &GroupResolver{DB: f}
	got, err := r.ResolveStore(grp, "", &bt, reservations.AskerFor(42, 42))
	if err != nil {
		t.Fatalf("ResolveStore: %v", err)
	}
	if got.Node.ID != toteSlot.ID {
		t.Errorf("resolved to %s, want %s — an explicit carrier type must outrank "+
			"the derivation, or the level keeper loses the one answer it has",
			got.Node.Name, toteSlot.Name)
	}
}

// A DERIVATION READ FAILURE NARROWS NOTHING, which is the opposite disposition
// from binTypeAllowed's refusal and deliberately so: this read answers "what IS
// the carrier", and failing to learn that must leave the resolve exactly as it
// was before the gate existed rather than refusing to place anything.
func TestResolveStore_DerivationReadFailureResolvesUntyped(t *testing.T) {
	t.Parallel()
	f, grp, _, _ := fencedGroup(t)
	f.carrierTypeErr = errors.New("connection reset by peer")

	r := &GroupResolver{DB: f}
	got, err := r.ResolveStore(grp, "", nil, reservations.AskerFor(42, 42))
	if err != nil || got == nil || got.Node == nil {
		t.Fatalf("got %v, err %v — an unreadable carrier type must resolve UNTYPED, "+
			"not refuse. Refusing converts a database blip into a parked robot", got, err)
	}
}

// reservations.Anyone carries OrderID 0 and must stay untyped, so the call sites
// with no order in hand keep the behaviour they have always had.
func TestResolveStore_AnyoneResolvesUntyped(t *testing.T) {
	t.Parallel()
	f, grp, toteSlot, _ := fencedGroup(t)
	f.carrierTypes = map[int64]int64{42: btKD}

	r := &GroupResolver{DB: f}
	got, err := r.ResolveStore(grp, "", nil, reservations.Anyone)
	if err != nil {
		t.Fatalf("ResolveStore: %v", err)
	}
	if got.Node.ID != toteSlot.ID {
		t.Errorf("an order-less resolve picked %s; with no order there is no carrier "+
			"to derive, so the fence must not narrow and the ordinary ranking stands",
			got.Node.Name)
	}
}
