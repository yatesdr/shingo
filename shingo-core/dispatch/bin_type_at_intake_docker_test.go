//go:build docker

package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// carrier_at_intake_docker_test.go — THE HOPKINSVILLE INCIDENT, 2026-09-22.
//
// An operator pressed CLEAR at unloader SMN_03. Edge's U2 empty-out side-cycle
// created a move, SMN_03 → the "Supermarket Empty Totes" GROUP. Core resolved
// that group at intake — 1ms BEFORE the order row was written — so the order had
// no id, no bin and no reservation, the resolver's carrier lookup found nothing,
// and the per-node Allowed Bin Types fence read "I do not know" as "do not
// narrow". A 45x48 KD went to SMN_05, which accepts TOTE-2415 and nothing else.
//
// Every gate behaved exactly as written. The signal was sitting on the order the
// whole time: source=SMN_03, holding exactly one carrier, a knockdown.
//
// These drive binTypeAtIntake directly, because that is where the fact is
// recovered; the resolver end of it is covered by
// binresolver/carrier_type_derivation_test.go.

func intakeFixture(t *testing.T, db *store.DB) (kdType, toteType int64, srcNode *nodes.Node) {
	t.Helper()
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bin_types (code) VALUES ('INTAKE-45x48 KD') RETURNING id`).Scan(&kdType), "kd type")
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bin_types (code) VALUES ('INTAKE-TOTE-2415') RETURNING id`).Scan(&toteType), "tote type")
	srcNode = &nodes.Node{Name: "INTAKE-SMN_03", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(srcNode), "create source node")
	return kdType, toteType, srcNode
}

// THE REGRESSION. A move off an unloader names no bin — the scanner claims it
// moments later — so the source node is the only thing that can say what is
// being placed. Before this, intake answered "unknown" and the fence was skipped.
func TestBinTypeAtIntake_ReadsTheSourceNode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	kdType, _, srcNode := intakeFixture(t, db)

	bin := &bins.Bin{Label: "INTAKE-CARRIER-0004", BinTypeID: kdType, NodeID: &srcNode.ID}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin at source")

	lc, _ := newLifecycleForTest(t, db)
	// No BinID and no OriginID — exactly the shape the U2 side-cycle produces.
	order := &orders.Order{SourceNode: srcNode.Name}

	carrier := lc.binTypeAtIntake(order)
	got := carrier.TypeID()
	if got == nil {
		t.Fatalf("binTypeAtIntake could not name the bin type standing at the source node.\n\n"+
			"This is the Hopkinsville miss verbatim: the order has no bin yet because the "+
			"scanner claims it later, so the source node is the only thing that can say what "+
			"is being placed. Unknown here means the Allowed Bin Types fence is skipped and a "+
			"knockdown lands in a tote-only position. bin type=%s", carrier)
	}
	if *got != kdType {
		t.Errorf("bin type = %d, want %d (the knockdown standing at the source)", *got, kdType)
	}
}

// TWO CARRIERS AT THE SOURCE IS NOT AN ANSWER. Narrowing on a guess is worse
// than not narrowing, so this stays unknown — and says so, rather than
// returning a bare nil that reads as "unrestricted".
func TestBinTypeAtIntake_AmbiguousSourceStaysUnknown(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	kdType, toteType, srcNode := intakeFixture(t, db)

	for _, bt := range []int64{kdType, toteType} {
		b := &bins.Bin{Label: fmt.Sprintf("INTAKE-AMBIG-%d", bt), BinTypeID: bt, NodeID: &srcNode.ID}
		testutil.MustNoErr(t, db.CreateBin(b), "create bin at source")
	}

	lc, _ := newLifecycleForTest(t, db)
	order := &orders.Order{SourceNode: srcNode.Name}

	if id := lc.binTypeAtIntake(order).TypeID(); id != nil {
		t.Errorf("a source node holding two bin types produced a definite answer (%d); "+
			"two types is no single answer and picking one fences the resolve on a guess", *id)
	}
}

// THE ORDER'S OWN BIN OUTRANKS THE SOURCE NODE. A bin move names the bin, and
// that is a stronger statement than whatever else happens to stand at the node.
func TestBinTypeAtIntake_NamedBinWins(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	kdType, toteType, srcNode := intakeFixture(t, db)

	resident := &bins.Bin{Label: "INTAKE-RESIDENT", BinTypeID: toteType, NodeID: &srcNode.ID}
	testutil.MustNoErr(t, db.CreateBin(resident), "resident bin")
	named := &bins.Bin{Label: "INTAKE-NAMED", BinTypeID: kdType}
	testutil.MustNoErr(t, db.CreateBin(named), "named bin")

	lc, _ := newLifecycleForTest(t, db)
	order := &orders.Order{SourceNode: srcNode.Name, BinID: &named.ID}

	id := lc.binTypeAtIntake(order).TypeID()
	if id == nil || *id != kdType {
		t.Errorf("carrier = %v, want the order's own bin type %d — a named bin is a stronger "+
			"statement than whatever stands at the source", id, kdType)
	}
}

// NOTHING TO GO ON STAYS UNKNOWN, AND CARRIES ITS REASON. The reason is what
// settleBinType logs against the group, so a door the fence cannot serve shows
// up in the plant's own traffic instead of in somebody's reading of the code.
func TestBinTypeAtIntake_UnknownCarriesItsReason(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	lc, _ := newLifecycleForTest(t, db)

	carrier := lc.binTypeAtIntake(&orders.Order{SourceNode: "INTAKE-NOWHERE"})
	if carrier.TypeID() != nil {
		t.Fatal("an order with nothing to go on produced a bin type")
	}
	if got := carrier.String(); got == "" || got == "UNKNOWN ()" {
		t.Errorf("unknown bin type = %q, want a reason naming the site and the gap", got)
	}
}
