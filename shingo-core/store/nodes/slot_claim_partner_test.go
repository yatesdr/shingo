//go:build docker

package nodes_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// slot_claim_partner_test.go — the seatbelt's partner arm (P8, P11).
//
// A drop the vacated-slot rule admitted on the PARTNER's lift finds the
// partner's bin on the node. ClaimSlotTx credits that bin only while, inside the
// claim's own statement, the partner still holds it the way a confirmed claim
// leaves it, is not terminal, and is this order's linked sibling. Each test
// breaks exactly one of those and expects a refusal; the first shows all three
// holding and the claim going through.

type partnerFixture struct {
	db      *store.DB
	slot    int64
	bin     int64
	order   int64
	partner int64
}

func newPartnerFixture(t *testing.T, name string) partnerFixture {
	t.Helper()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	slot := mkSlotNode(t, db.DB, name+"-SLOT")
	p := &orders.Order{EdgeUUID: name + "-p", StationID: "edge.1", OrderType: "complex", Status: "sourcing",
		Quantity: 1, SiblingOrderUUID: name + "-l"}
	if err := orders.Create(db.DB, p); err != nil {
		t.Fatalf("create partner: %v", err)
	}
	l := &orders.Order{EdgeUUID: name + "-l", StationID: "edge.1", OrderType: "complex", Status: "sourcing",
		Quantity: 1, SiblingOrderUUID: name + "-p"}
	if err := orders.Create(db.DB, l); err != nil {
		t.Fatalf("create order: %v", err)
	}
	b := testdb.CreateBinAtNode(t, db, sd.Payload.Code, slot, name+"-BIN")
	testdb.ClaimBinForTest(t, db, b.ID, p.ID) // reserve → claim → confirm: the hold confirmComplexPlan leaves
	if err := reservations.AcquireSlot(db.DB, l.ID, slot, "test"); err != nil {
		t.Fatalf("reserve slot: %v", err)
	}
	return partnerFixture{db: db, slot: slot, bin: b.ID, order: l.ID, partner: p.ID}
}

func (f partnerFixture) claim() error {
	return f.db.ConfirmSlotClaimWithPartner(f.slot, f.order, nil, f.partner, []int64{f.bin})
}

func (f partnerFixture) assertUnclaimed(t *testing.T, why string) {
	t.Helper()
	n, err := nodes.Get(f.db.DB, f.slot)
	if err != nil {
		t.Fatalf("reload slot: %v", err)
	}
	if n.ClaimedBy != nil {
		t.Errorf("%s: slot claimed by %d after a refused claim", why, *n.ClaimedBy)
	}
}

// P8 — the arm credits a live sibling's held bin, and nothing else changes.
func TestClaimSlotPartner_CreditsALiveSiblingsHeldBin(t *testing.T) {
	t.Parallel()
	f := newPartnerFixture(t, "CSP-OK")
	if err := f.claim(); err != nil {
		t.Fatalf("the partner holds the only bin, is live and is this order's sibling — the claim must "+
			"go through: %v", err)
	}
	// And the plain claim is exactly as strict as before: the same slot, the same
	// bin, no partner named, is refused.
	g := newPartnerFixture(t, "CSP-PLAIN")
	if err := g.db.ConfirmSlotClaim(g.slot, g.order, nil); err == nil {
		t.Fatal("a plain claim onto a node holding another order's bin went through — the partner arm " +
			"must be vacuous when no partner is named")
	}
}

// P8 — the partner gave its bin back between the gate and the claim.
func TestClaimSlotPartner_RefusesWhenThePartnerReleasedItsBin(t *testing.T) {
	t.Parallel()
	f := newPartnerFixture(t, "CSP-REL")
	if err := f.db.ReleaseClaimForBin(f.bin, f.partner); err != nil {
		t.Fatalf("release the partner's claim: %v", err)
	}
	if err := f.claim(); err == nil {
		t.Fatal("the partner no longer holds the bin, so nothing is coming for it — the claim must be refused")
	}
	f.assertUnclaimed(t, "released partner")
}

// P8 / P11 — a terminal partner whose claim and reservation lingered.
func TestClaimSlotPartner_RefusesATerminalPartnersLeakedClaim(t *testing.T) {
	t.Parallel()
	f := newPartnerFixture(t, "CSP-TERM")
	if _, err := f.db.DB.Exec(`UPDATE orders SET status='cancelled' WHERE id=$1`, f.partner); err != nil {
		t.Fatalf("terminalize partner: %v", err)
	}
	if err := f.claim(); err == nil {
		t.Fatal("a cancelled partner's claim leaked, and the arm credited it — a terminal order lifts nothing")
	}
	f.assertUnclaimed(t, "terminal partner")
}

// P8 — the named partner is not this order's sibling.
func TestClaimSlotPartner_RefusesASiblingLinkMismatch(t *testing.T) {
	t.Parallel()
	f := newPartnerFixture(t, "CSP-LINK")
	if _, err := f.db.DB.Exec(`UPDATE orders SET sibling_order_uuid='someone-else' WHERE id=$1`, f.order); err != nil {
		t.Fatalf("break the link: %v", err)
	}
	if err := f.claim(); err == nil {
		t.Fatal("the arm credited a bin held by an order that is not this order's partner")
	}
	f.assertUnclaimed(t, "link mismatch")
}
