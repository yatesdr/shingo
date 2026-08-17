package dispatch

import (
	"strings"
	"testing"
)

// mint_ids_pure_test.go — the two id mints, and why one must be unique while the
// other must NOT be.
//
// Every block id this system emits is "<vendorOrderID>-b<n>". So the uniqueness
// of a block id across orders is entirely inherited from the vendor id, and the
// uniqueness of a block id WITHIN an order is entirely mintBlockID's numbering.
// Those are different scopes with opposite requirements, and the pair is easy to
// "tidy" into a bug — which is what these pin.

// TestMintVendorOrderID_CarriesTheOrderID pins the property the consolidation
// makes structural: two orders cannot produce the same vendor id, and therefore
// cannot produce the same block id, whatever their block numbering does.
//
// This was already true before the helper existed — the same format string was
// written out at four call sites. Nothing here is a regression test for a bug
// that happened; it is the property being moved out of convention and into one
// function.
//
// MUTATION (verified): drop the order id from mintVendorOrderID's format string
// (leaving prefix + uuid fragment). The "distinct orders" assertion still passes
// — a uuid fragment is very probably distinct — but the "carries the order id"
// assertion fires immediately. That split is the point: uniqueness-by-luck and
// uniqueness-by-construction look identical from a collision test, so the
// derivation is what has to be asserted.
func TestMintVendorOrderID_CarriesTheOrderID(t *testing.T) {
	t.Parallel()

	got := mintVendorOrderID(4017)
	if !strings.HasPrefix(got, VendorIDPrefix+"4017-") {
		t.Errorf("mintVendorOrderID(4017) = %q, want the prefix %q followed by the order id — the "+
			"order id is what makes two orders' block ids distinct, and a fleet-side id that cannot "+
			"be traced back to an order is also unusable for diagnosis", got, VendorIDPrefix)
	}

	// Distinct orders, distinct ids — the consequence, asserted separately from
	// the derivation above so a mutation can tell them apart.
	a, b := mintVendorOrderID(1), mintVendorOrderID(2)
	if a == b {
		t.Errorf("mintVendorOrderID(1) and (2) both returned %q", a)
	}

	// The same order minted twice differs too. This is NOT the uniqueness
	// argument — it is disambiguation for order-id reuse across a database reset,
	// and it is worth pinning only so the uuid fragment is not "simplified" away
	// on the grounds that the order id alone is unique.
	if x, y := mintVendorOrderID(7), mintVendorOrderID(7); x == y {
		t.Errorf("two mints for order 7 both returned %q; the uuid fragment disambiguates a reused "+
			"order id and must not be dropped", x)
	}
}

// TestMintBlockID_IsDeterministic pins the OPPOSITE property, and it is the one
// that reads as a bug if you meet it cold.
//
// Two paths can append the same tail to one order concurrently — the lane-gate
// valve and the evaluator. They are serialized on a per-lane key and both reload
// before appending, but that guard is Core's own. SEER's rejection of a duplicate
// blockId within an order is the backstop UNDER it, and the backstop only works
// because two racing appends of the same tail compute the SAME id.
//
// Add a uuid, a timestamp or a counter to mintBlockID and both appends are
// accepted: the robot receives the tail twice. A noisy, logged rejection becomes
// a silent double dispatch.
//
// MUTATION (verified): append any entropy to mintBlockID's return. This test's
// determinism assertion fires. Nothing else in the suite does — which is exactly
// why it is pinned here.
func TestMintBlockID_IsDeterministic(t *testing.T) {
	t.Parallel()

	vendor := mintVendorOrderID(42)
	first, second := mintBlockID(vendor, 1), mintBlockID(vendor, 1)
	if first != second {
		t.Fatalf("mintBlockID is not deterministic: %q then %q for the same (vendorOrderID, n).\n"+
			"Two concurrent appends of the same tail must compute the SAME block id — that collision "+
			"is what SEER rejects, and it is the only thing standing between a lost race and a "+
			"silent double dispatch", first, second)
	}

	// Different n, different id — the within-order half of the contract. Without
	// this, "deterministic" would be satisfiable by returning a constant.
	if a, b := mintBlockID(vendor, 1), mintBlockID(vendor, 2); a == b {
		t.Errorf("mintBlockID(v, 1) and (v, 2) both returned %q — blocks within one order must be "+
			"distinguishable", a)
	}

	// And the block id is scoped under its vendor id, which is what carries the
	// cross-order distinction down from mintVendorOrderID.
	if !strings.HasPrefix(first, vendor) {
		t.Errorf("mintBlockID returned %q, which is not scoped under the vendor id %q — cross-order "+
			"distinctness is inherited from the vendor id and nothing else provides it", first, vendor)
	}
}
