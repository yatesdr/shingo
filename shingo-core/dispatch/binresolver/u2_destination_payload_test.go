package binresolver

import (
	"testing"

	"shingocore/store/reservations"
)

// WHAT D2 STILL FIXES: THE DESTINATION.
//
// An empty-out (U2) that names the part it just cleared carries that part into
// destination resolution: lifecycle_service.resolveSyntheticDestination
// resolves a group destination with order.PayloadCode, and payloadAllowedAt
// gates every child on it. So an empties destination whose slots declare what
// they hold refuses the U2 on the stale part, while the payload-less U2 — the
// carrier is empty, it holds no part — is accepted.
//
// The fixture is flatPayloadStore: one flat slot declaring CLIP. STUD stands
// for the part the unloader just cleared.
func TestU2Destination_StalePartIsGatedByTheDestinationRule(t *testing.T) {
	t.Parallel()
	f, grp := flatPayloadStore("")
	r := &GroupResolver{DB: f}

	if _, err := r.ResolveStore(grp, "STUD", UnknownBinType("test: U2 names no carrier"), reservations.Anyone); err == nil {
		t.Fatal("a U2 tagged with the cleared part STUD was accepted at a slot declared for CLIP; " +
			"at c0c525c0 payloadAllowedAt refuses it")
	}
	if _, err := r.ResolveStore(grp, "", UnknownBinType("test: U2 names no carrier"), reservations.Anyone); err != nil {
		t.Fatalf("the payload-less U2 was refused at the same destination: %v", err)
	}
}
