//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// WHAT A COMPLEX PICKUP AT A CONCRETE NODE TAKES, BY HOW THE STEP IS FLAGGED.
//
// Keep-staged's swap starts with a pickup on the line's inbound staging node,
// where a spare stands: a full of the part on a consume line, an empty its part
// may ride on a produce line. This table is how the allocator judges the bin
// (rule 5, the one bind rule):
//
//   - a pickup that NAMES THE PART and is not Empty takes stock of it: loaded
//     with it, confirmed, counted above 0 (binresolver.StockOnly), of a carrier
//     the part may ride. An unstamped empty is not stock of anything, so a
//     consume spare is a full and nothing else; it used to pass (an unstamped
//     bin is no payload mismatch) and a consume press ran on nothing;
//   - an Empty pickup takes an unstamped empty and refuses a stamped one
//     (binresolver.EmptyBinsOnly). Naming a part on the step, it takes an
//     empty that part may ride (the produce spare, the changeover refill that
//     says which style its carrier is for); naming none, an empty of any type.
//
// A payload that declares no carrier types states no bin-type intent, so every
// type is right for it.
func TestSpotPickup_HowTheStepFlagJudgesTheBin(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	newT := &bins.BinType{Code: "SPK-NEWT"}
	testutil.MustNoErr(t, db.CreateBinType(newT), "new type")
	oldT := &bins.BinType{Code: "SPK-OLDT"}
	testutil.MustNoErr(t, db.CreateBinType(oldT), "old type")
	pNew := &payloads.Payload{Code: "SPK-PNEW"}
	testutil.MustNoErr(t, db.CreatePayload(pNew), "payload new")
	pOld := &payloads.Payload{Code: "SPK-POLD"}
	testutil.MustNoErr(t, db.CreatePayload(pOld), "payload old")
	pUndeclared := &payloads.Payload{Code: "SPK-PUNDECL"}
	testutil.MustNoErr(t, db.CreatePayload(pUndeclared), "payload undeclared")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(pNew.ID, []int64{newT.ID}), "rule new")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(pOld.ID, []int64{oldT.ID}), "rule old")

	type standing struct {
		binType *bins.BinType
		payload string // "" = an unstamped empty
		uop     int    // 0 with a payload = a stamped empty
	}
	n := 0
	spotWith := func(s standing) *nodes.Node {
		n++
		node := &nodes.Node{Name: "SPK-SPOT-" + string(rune('A'+n)), Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(node), "spot")
		b := &bins.Bin{BinTypeID: s.binType.ID, Label: node.Name + "-BIN", NodeID: &node.ID, Status: "available"}
		testutil.MustNoErr(t, db.CreateBin(b), "bin")
		if s.payload != "" {
			testutil.MustNoErr(t, db.SetBinManifest(b.ID, `{"items":[]}`, s.payload, s.uop), "manifest")
			testutil.MustNoErr(t, db.ConfirmBinManifest(b.ID, ""), "confirm")
		}
		return node
	}

	named := func(p string) resolvedStep { return resolvedStep{PayloadCode: p} }
	empty := resolvedStep{Empty: true}
	emptyNamed := func(p string) resolvedStep { return resolvedStep{Empty: true, PayloadCode: p} }

	rows := []struct {
		name  string
		bin   standing
		step  resolvedStep
		order string // the order's payload; the step's wins when set
		takes bool
	}{
		{"named: permitted-type unstamped empty is no stock", standing{newT, "", 0}, named(pNew.Code), pOld.Code, false},
		{"named: full of the payload", standing{newT, pNew.Code, 50}, named(pNew.Code), pOld.Code, true},
		{"named: the payload's carrier counted out", standing{newT, pNew.Code, 0}, named(pNew.Code), pOld.Code, false},
		{"named: non-permitted-type unstamped empty", standing{oldT, "", 0}, named(pNew.Code), pOld.Code, false},
		{"named: empty stamped with another part", standing{newT, pOld.Code, 0}, named(pNew.Code), pOld.Code, false},
		{"named: wrong-payload full", standing{newT, pOld.Code, 50}, named(pNew.Code), pNew.Code, false},
		{"named: payload declares no types, a full of any type", standing{oldT, pUndeclared.Code, 50}, named(pUndeclared.Code), pOld.Code, true},
		{"Empty: any-type unstamped empty", standing{oldT, "", 0}, empty, pNew.Code, true},
		{"Empty + payload: permitted-type unstamped empty", standing{newT, "", 0}, emptyNamed(pNew.Code), pOld.Code, true},
		{"Empty + payload: non-permitted-type unstamped empty", standing{oldT, "", 0}, emptyNamed(pNew.Code), pNew.Code, false},
		{"Empty + payload declaring no types: any-type unstamped empty", standing{oldT, "", 0}, emptyNamed(pUndeclared.Code), pNew.Code, true},
		{"Empty: stamped empty refused", standing{newT, pOld.Code, 0}, empty, pNew.Code, false},
		{"Empty + payload: stamped empty refused", standing{newT, pNew.Code, 0}, emptyNamed(pNew.Code), pNew.Code, false},
	}
	for _, r := range rows {
		spot := spotWith(r.bin)
		step := r.step
		step.Action = protocol.ActionPickup
		step.Node = spot.Name
		got, hadBins, err := d.allocator.findAvailableForNeed(complexPickups([]resolvedStep{step}, "")[0], resolvedStepPayload(step, r.order))
		testutil.MustNoErr(t, err, r.name)
		if !hadBins {
			t.Fatalf("%s: fixture drift — the spot reads empty", r.name)
		}
		if (got != nil) != r.takes {
			t.Errorf("%s: took=%v, want took=%v", r.name, got != nil, r.takes)
		}
	}
}
