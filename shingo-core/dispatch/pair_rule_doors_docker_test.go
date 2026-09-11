//go:build docker

package dispatch

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
)

// pair_rule_doors_docker_test.go — the pair rule at the edges of a pair's life:
// a partner that has not arrived, and a partner Core refused. Helpers are
// pair_rule_helpers_docker_test.go's.

// ── census 14: a partner row that has not landed yet ───────────────────────

// TestPairRule_AwaitingPartnerParksThenGoesWhenThePartnerLands: the first-created
// leg of a pre-minted pair names a partner whose row Core has not ingested. It
// must not go alone, must hold nothing while it waits, and must go the moment
// the partner lands. The sentence on its row must be true about a partner with
// no row: nothing can be "securing a bin" for an order that does not exist yet.
//
// DEFECT PIN on the sentence. Fails at bcbde0d2: partnerSentence renders
// "Holding this leg until partner order X secures a bin" for this wait
// (ochre-marten F9, verdigris-otter F9). The park and the release are coverage:
// they pass at bcbde0d2, and removing the partnerPending arm from
// DispatchPreparedComplex makes the first-leg assertion fail.
func TestPairRule_AwaitingPartnerParksThenGoesWhenThePartnerLands(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	stage := prNode(t, db, "PR14-STAGE")
	out := prNode(t, db, "PR14-OUT")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "PR14-FRESH")
	prResident(t, db, sd.LineNode, sd.BinType.ID, sd.Payload.Code, "PR14-RESIDENT")

	prSubmitLeg(d, "pr14-supply", "pr14-evac", sd.Payload.Code, sd.LineNode.Name,
		prPick(sd.StorageNode.Name), prDropExcl(stage.Name), prWait(stage.Name), prPick(stage.Name),
		prDrop(sd.LineNode.Name))
	prScanPass(t, d, db, "pr14-supply")

	supply := prReloadUUID(t, db, "pr14-supply")
	if supply.VendorOrderID != "" {
		t.Fatalf("the supply went to the fleet (%s) before its partner existed — a pre-minted pair's first "+
			"leg must wait for the second", supply.VendorOrderID)
	}
	if supply.QueueCode != string(protocol.QueueWaitingForPartner) || supply.QueueCause != string(CauseSwapHold) {
		t.Errorf("supply parked under %q/%q, want %q/%q", supply.QueueCode, supply.QueueCause,
			protocol.QueueWaitingForPartner, CauseSwapHold)
	}
	prAssertHoldsNothing(t, db, supply, "a leg whose partner has not landed")
	if strings.Contains(supply.QueueReason, "secures a bin") {
		t.Errorf("the operator sentence is %q — it says the partner is securing a bin, and the partner has "+
			"no order row at Core yet. The true wait is for the partner's order to arrive", supply.QueueReason)
	}
	if !strings.Contains(supply.QueueReason, shortRef("pr14-evac")) {
		t.Errorf("the sentence %q does not name the partner it is waiting for", supply.QueueReason)
	}

	prSubmitLeg(d, "pr14-evac", "pr14-supply", sd.Payload.Code, sd.LineNode.Name,
		prWait(sd.LineNode.Name), prPick(sd.LineNode.Name), prDrop(out.Name))
	prScanPass(t, d, db, "pr14-evac", "pr14-supply")

	supply, evac := prReloadUUID(t, db, "pr14-supply"), prReloadUUID(t, db, "pr14-evac")
	if supply.VendorOrderID == "" || evac.VendorOrderID == "" {
		t.Fatalf("the partner landed and the pair still did not go: supply %q/%q, evac %q/%q",
			supply.Status, supply.QueueCause, evac.Status, evac.QueueCause)
	}
}

// ── census 12 and 13: a partner Core refused at intake ──────────────────────

// badStepNode is a node no fixture creates, so a step naming it is refused at
// intake as structural (resolution_failed) — the ordinary way a well-formed pair
// ends up with one leg Core never ingested.
const badStepNode = "PR-NO-SUCH-NODE"

// TestPairRule_PartnerRefusedAfterTheFirstLegParkedFailsItLoud is census 12, the
// pre-minted door's order of events: the first leg parks waiting for its
// partner, then Core refuses the partner. Nothing will ever ingest that row, so
// the wait has no releaser; the waiting leg must fail — a fault an operator has
// to see, not congestion — and say why, in the partner's own words.
//
// DEFECT PIN. Fails at bcbde0d2: the survivor stays parked under swap-hold
// indefinitely, and only the 30-minute anomaly board ever notices
// (verdigris-otter F9).
func TestPairRule_PartnerRefusedAfterTheFirstLegParkedFailsItLoud(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	stage := prNode(t, db, "PR12-STAGE")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "PR12-FRESH")
	prResident(t, db, sd.LineNode, sd.BinType.ID, sd.Payload.Code, "PR12-RESIDENT")

	prSubmitLeg(d, "pr12-supply", "pr12-evac", sd.Payload.Code, sd.LineNode.Name,
		prPick(sd.StorageNode.Name), prDropExcl(stage.Name), prWait(stage.Name), prPick(stage.Name),
		prDrop(sd.LineNode.Name))
	prScanPass(t, d, db, "pr12-supply")
	if s := prReloadUUID(t, db, "pr12-supply"); s.QueueCode != string(protocol.QueueWaitingForPartner) {
		t.Fatalf("fixture: the supply is not waiting for its partner (%q/%q)", s.QueueCode, s.QueueCause)
	}

	// The partner is refused: its outbound names a node that does not exist.
	prSubmitLeg(d, "pr12-evac", "pr12-supply", sd.Payload.Code, sd.LineNode.Name,
		prWait(sd.LineNode.Name), prPick(sd.LineNode.Name), prDrop(badStepNode))
	if o, err := db.GetOrderByUUID("pr12-evac"); readFailed(err) {
		t.Fatalf("fixture: read the partner's row: %v", err)
	} else if o != nil {
		t.Fatalf("fixture: Core ingested the partner (%s) — it was meant to be refused at intake", o.Status)
	}
	prScanPass(t, d, db, "pr12-supply")

	supply := prReloadUUID(t, db, "pr12-supply")
	if supply.Status != StatusFailed {
		t.Fatalf("the supply is %q (cause %q) after Core refused its partner at intake. That partner will "+
			"never have a row, so nothing can release this wait: it has to fail, loudly, with the "+
			"partner's reason", supply.Status, supply.QueueCause)
	}
	if !strings.Contains(supply.ErrorDetail, badStepNode) {
		t.Errorf("error detail %q does not carry the partner's rejection reason (it named %s)",
			supply.ErrorDetail, badStepNode)
	}
	prAssertHoldsNothing(t, db, supply, "a leg failed for its refused partner")
}

// TestPairRule_RefusedFirstLegFailsItsPartnerLoud is census 13, the other order:
// Core refuses the FIRST leg, and the second then arrives naming a row that will
// never exist. Once every door pre-mints both uuids this is the shape for all of
// them — the second leg's pointer is present, its partner's row never will be.
//
// DEFECT PIN. Fails at bcbde0d2: the second leg parks under swap-hold forever.
func TestPairRule_RefusedFirstLegFailsItsPartnerLoud(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	stage := prNode(t, db, "PR13-STAGE")
	out := prNode(t, db, "PR13-OUT")
	prResident(t, db, sd.LineNode, sd.BinType.ID, sd.Payload.Code, "PR13-RESIDENT")

	// The first leg is refused: its source names a node that does not exist.
	prSubmitLeg(d, "pr13-supply", "pr13-evac", sd.Payload.Code, sd.LineNode.Name,
		prPick(badStepNode), prDropExcl(stage.Name), prWait(stage.Name), prPick(stage.Name),
		prDrop(sd.LineNode.Name))
	if o, err := db.GetOrderByUUID("pr13-supply"); readFailed(err) {
		t.Fatalf("fixture: read the first leg's row: %v", err)
	} else if o != nil {
		t.Fatalf("fixture: Core ingested the first leg (%s) — it was meant to be refused at intake", o.Status)
	}

	prSubmitLeg(d, "pr13-evac", "pr13-supply", sd.Payload.Code, sd.LineNode.Name,
		prWait(sd.LineNode.Name), prPick(sd.LineNode.Name), prDrop(out.Name))
	prScanPass(t, d, db, "pr13-evac")

	evac := prReloadUUID(t, db, "pr13-evac")
	if evac.VendorOrderID != "" {
		t.Fatalf("the evac went to the fleet (%s) with its supply refused — it lifts the line's bin and "+
			"nothing is coming to replace it", evac.VendorOrderID)
	}
	if evac.Status != StatusFailed {
		t.Fatalf("the evac is %q (cause %q, reason %q) with its partner refused at intake. The partner's row "+
			"will never exist, so a partner-wait here has no releaser", evac.Status, evac.QueueCause, evac.QueueReason)
	}
	if !strings.Contains(evac.ErrorDetail, badStepNode) {
		t.Errorf("error detail %q does not carry the partner's rejection reason (it named %s)",
			evac.ErrorDetail, badStepNode)
	}
}
