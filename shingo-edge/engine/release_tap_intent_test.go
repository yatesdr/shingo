package engine

// release_tap_intent_test.go — the RELEASE tap on a leg whose status is Core's
// versioned word is an intent, not a status (orders.Manager.
// ReleaseOrderWithDisposition): the leg stays staged with its release sent
// until Core says its robot moves, and every reader of "released" reads that
// sent intent (release.ReleasedFromWait): the act's point, the board's order
// JSON, swap-ready. A Core rejection clears the intent and offers RELEASE
// again. An older Core's leg (version 0) still moves to in_transit at the tap
// (orders/status_mirror_pin_test.go).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/release"
)

// versionedPairAtItsWaits is the second-release pin's two-robot consume swap
// (secondReleaseWorld: 20 on the bin) with both legs at Core's word, staged at
// version 2, and parked at their station wait 0.
func versionedPairAtItsWaits(t *testing.T) *relHarness {
	t.Helper()
	h := secondReleaseWorld(t, protocol.SwapModeTwoRobot, 20)
	for _, leg := range []string{"evac", "supply"} {
		coreWord(h.handler, h.order(leg).UUID, protocol.StatusStaged, 2)
		h.coreStagesAt(leg, 0)
		if o := h.order(leg); o.Status != protocol.StatusStaged || o.CoreVersion != 2 || o.StationWait == nil ||
			*o.StationWait != 0 || o.WaitKind != protocol.WaitKindStation {
			t.Fatalf("fixture: %s reads %s at version %d, wait %v %q; want staged at 2, station wait 0",
				leg, o.Status, o.CoreVersion, o.StationWait, o.WaitKind)
		}
	}
	return h
}

// legTrail is the bus trail of the harness's legs, by name.
func legTrail(h *relHarness) *busTrail {
	names := map[int64]string{}
	for _, l := range h.legs {
		names[l.id] = l.name
	}
	return trailOf(h.eng, names)
}

// pointOf is the act's point for a leg: release.PointOf over its row and the
// waits its plan holds.
func pointOf(t *testing.T, h *relHarness, leg string) release.StaticPoint {
	t.Helper()
	o := h.order(leg)
	in, err := release.DecodeIntent(o.ReleaseIntent)
	testutil.MustNoErr(t, err, "decode "+leg+"'s intent")
	facts, err := release.DecodeFacts(o.ReleaseFacts)
	testutil.MustNoErr(t, err, "decode "+leg+"'s facts")
	return release.PointOf(o.Status, o.StationWait, o.WaitKind, in, facts.Purposes)
}

// board is the station board's tile for the harness's node as the glass reads
// it: swap_ready, and each leg's release_sent from the tile's JSON ("-" when
// the key is absent).
func board(t *testing.T, h *relHarness) (swapReady bool, releaseSent map[string]string) {
	t.Helper()
	view, err := h.eng.stationService.BuildView(context.Background(), h.stationID)
	testutil.MustNoErr(t, err, "BuildView")
	names := map[int64]string{}
	for _, l := range h.legs {
		names[l.id] = l.name
	}
	for i := range view.Nodes {
		if view.Nodes[i].Node.ID != h.nodeID {
			continue
		}
		raw, err := json.Marshal(view.Nodes[i])
		testutil.MustNoErr(t, err, "marshal the tile")
		var tile struct {
			SwapReady bool             `json:"swap_ready"`
			Orders    []map[string]any `json:"orders"`
		}
		testutil.MustNoErr(t, json.Unmarshal(raw, &tile), "decode the tile")
		releaseSent = map[string]string{}
		for _, o := range tile.Orders {
			id, _ := o["id"].(float64)
			v := "-"
			if sent, ok := o["release_sent"]; ok {
				v = strings.TrimSpace(strings.ToLower(jsonText(t, sent)))
			}
			releaseSent[names[int64(id)]] = v
		}
		return tile.SwapReady, releaseSent
	}
	t.Fatalf("the board has no tile for node %d", h.nodeID)
	return false, nil
}

func jsonText(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	testutil.MustNoErr(t, err, "marshal the board's release_sent")
	return string(raw)
}

func expectBoard(t *testing.T, h *relHarness, step string, wantReady bool, want map[string]string) {
	t.Helper()
	ready, sent := board(t, h)
	if ready != wantReady {
		t.Errorf("%s: the tile's swap_ready is %v, want %v", step, ready, wantReady)
	}
	for leg, w := range want {
		if sent[leg] != w {
			t.Errorf("%s: %s's release_sent on the board is %s, want %s", step, leg, sent[leg], w)
		}
	}
}

// TestVersionedRelease_TheTapLeavesTheLegStagedWithItsReleaseSent: the pair's
// RELEASE on two legs Core holds staged at version 2. Each leg gets its one
// order.release for wait 0 and its intent's sent_at, and a history row
// "released from staging" that moves nothing (staged->staged); the board is
// nudged for each, no status event fires, and the legs read staged. The board
// then reads each released (release_sent) and the pair not ready, the act's
// point is past the wait (Driving at 1), and a second press at the last wait
// is refused as released already. Core's in_transit at version 3 is the robot
// moving: the leg moves, and release_sent goes.
func TestVersionedRelease_TheTapLeavesTheLegStagedWithItsReleaseSent(t *testing.T) {
	t.Parallel()
	h := versionedPairAtItsWaits(t)
	expectBoard(t, h, "before the tap", true, map[string]string{"evac": "-", "supply": "-"})
	if p := pointOf(t, h, "evac"); !p.AtWait || p.Driving || p.Ordinal != 0 {
		t.Errorf("before the tap the evac's point is %+v, want at its wait 0", p)
	}
	trail := legTrail(h)
	h.mark()

	err := h.eng.ReleaseStagedOrders(h.nodeID, dispPull(20))
	if got, want := h.outcome(err), "ok | evac=staged supply=staged | rel=evac,supply | ingest=0 capred=-20"; got != want {
		t.Errorf("the tap:\n got  %q\n want %q", got, want)
	}
	trail.expect(t, "the tap", "evac touched", "supply touched")
	expectRows(t, "intents after the tap", pIntents(h), []string{"intent:evac=sent@0", "intent:supply=sent@0"})
	for leg, rels := range h.releaseEnvelopes() {
		if len(rels) != 1 || rels[0].StationWait == nil || *rels[0].StationWait != 0 || rels[0].CalledBy != "operator" {
			t.Errorf("%s's order.release rows %+v, want one for wait 0 by operator", leg, rels)
		}
	}
	for _, leg := range []string{"evac", "supply"} {
		o := h.order(leg)
		in, err := release.DecodeIntent(o.ReleaseIntent)
		testutil.MustNoErr(t, err, "decode "+leg+"'s intent")
		if o.Status != protocol.StatusStaged || o.CoreVersion != 2 || !o.ReleaseSent || in == nil || in.SentAt == "" {
			t.Errorf("after the tap %s reads %s at version %d, release_sent %v, intent %q; want staged at 2, sent, sent_at stamped",
				leg, o.Status, o.CoreVersion, o.ReleaseSent, o.ReleaseIntent)
		}
		expectRows(t, leg+"'s history", orderHistory(t, h.db, h.leg(leg)), []string{
			"pending->submitted: auto-submitted at creation",
			"staged->staged: released from staging",
		})
	}
	expectBoard(t, h, "after the tap", false, map[string]string{"evac": "true", "supply": "true"})
	if p := pointOf(t, h, "evac"); p.AtWait || !p.Driving || p.Ordinal != 1 || p.HasWait || p.NoWaits {
		t.Errorf("after the tap the evac's point is %+v, want Driving to ordinal 1, past its last wait", p)
	}

	// The second press at its last wait: released already, and nothing goes.
	h.mark()
	err = h.eng.ReleaseOrderWithLineside(h.leg("evac"), dispPartial(20))
	if err == nil || !strings.Contains(err.Error(), "was released already") {
		t.Errorf("a second press of the released evac returned %v, want it refused as released already", err)
	}
	if got, want := h.outcome(err), "refuse:released | evac=staged supply=staged | rel=- | ingest=0 capred=0"; got != want {
		t.Errorf("the second press:\n got  %q\n want %q", got, want)
	}
	trail.expect(t, "the second press")

	// Core's in_transit at a newer version: the robot moves.
	coreWord(h.handler, h.order("evac").UUID, protocol.StatusInTransit, 3)
	if o := h.order("evac"); o.Status != protocol.StatusInTransit || o.CoreVersion != 3 || o.ReleaseSent {
		t.Errorf("after Core's in_transit the evac reads %s at version %d, release_sent %v; want in_transit at 3, not sent",
			o.Status, o.CoreVersion, o.ReleaseSent)
	}
	trail.expect(t, "in_transit v3", "evac staged->in_transit")
	expectRows(t, "the evac's history after in_transit", orderHistory(t, h.db, h.leg("evac")), []string{
		"pending->submitted: auto-submitted at creation",
		"staged->staged: released from staging",
		"staged->in_transit: core: in_transit (version 3)",
	})
	expectBoard(t, h, "after the evac moves", false, map[string]string{"evac": "-", "supply": "true"})
	if p := pointOf(t, h, "evac"); p.AtWait || !p.Driving || p.Ordinal != 1 {
		t.Errorf("after the evac moves its point is %+v, want Driving to ordinal 1", p)
	}
}

// TestVersionedRelease_CoresRejectionOffersReleaseAgain: the tap wrote no
// status, so Core's rejection of a versioned leg's release keys on the intent
// it sent. order.error invalid_state clears it and writes Core's sentence for
// the chip, the leg still staged: RELEASE is offered again (the point at its
// wait, release_sent gone, the pair ready). A rejection with no release sent,
// none at all or one still held, changes nothing. manifest_sync_failed clears
// the intent and writes its sentence, and writes no status either.
func TestVersionedRelease_CoresRejectionOffersReleaseAgain(t *testing.T) {
	t.Parallel()
	const invalidState = "Core rejected the release: stub: Core refused (invalid_state). Click release to retry."
	tapped := func(t *testing.T) (*relHarness, *busTrail) {
		t.Helper()
		h := versionedPairAtItsWaits(t)
		testutil.MustNoErr(t, h.eng.ReleaseStagedOrders(h.nodeID, dispPull(20)), "the tap")
		trail := legTrail(h)
		h.mark()
		return h, trail
	}
	// rejectedAtItsWait: the evac is back at its wait with the sentence on its
	// chip, its history the tap's row alone; the supply keeps its release.
	rejectedAtItsWait := func(t *testing.T, h *relHarness, step, sentence string) {
		t.Helper()
		o := h.order("evac")
		if o.Status != protocol.StatusStaged || o.CoreVersion != 2 || o.ReleaseIntent != "" || o.ReleaseHeld != sentence || o.ReleaseSent {
			t.Errorf("%s: the evac reads %s at version %d, intent %q, chip %q, release_sent %v; want staged at 2, no intent, chip %q",
				step, o.Status, o.CoreVersion, o.ReleaseIntent, o.ReleaseHeld, o.ReleaseSent, sentence)
		}
		if p := pointOf(t, h, "evac"); !p.AtWait || p.Driving || p.Ordinal != 0 {
			t.Errorf("%s: the evac's point is %+v, want at its wait 0", step, p)
		}
		expectRows(t, step+": the evac's history", orderHistory(t, h.db, h.leg("evac")), []string{
			"pending->submitted: auto-submitted at creation",
			"staged->staged: released from staging",
		})
		expectRows(t, step+": intents", pIntents(h), []string{"intent:evac=-", "intent:supply=sent@0"})
		expectBoard(t, h, step, true, map[string]string{"evac": "-", "supply": "true"})
		if rel := h.releasedSinceMark(); len(rel) != 0 {
			t.Errorf("%s: released %v, want nothing sent", step, rel)
		}
	}

	t.Run("invalid_state after the tap", func(t *testing.T) {
		t.Parallel()
		h, trail := tapped(t)
		h.coreRefuses("evac", "invalid_state")
		trail.expect(t, "invalid_state", "evac touched")
		rejectedAtItsWait(t, h, "invalid_state", invalidState)

		// Again, with no release out now: a stray fan-out.
		h.coreRefuses("evac", "invalid_state")
		trail.expect(t, "invalid_state again")
		rejectedAtItsWait(t, h, "invalid_state again", invalidState)
	})

	t.Run("manifest_sync_failed after the tap", func(t *testing.T) {
		t.Parallel()
		h, trail := tapped(t)
		h.coreRefuses("evac", "manifest_sync_failed")
		trail.expect(t, "manifest_sync_failed", "evac touched")
		rejectedAtItsWait(t, h, "manifest_sync_failed",
			"Manifest sync failed at Core: stub: Core refused (manifest_sync_failed). Click release to retry.")
	})

	t.Run("invalid_state with no release sent", func(t *testing.T) {
		t.Parallel()
		h := versionedPairAtItsWaits(t)
		trail := legTrail(h)
		h.mark()
		h.coreRefuses("evac", "invalid_state")
		trail.expect(t, "invalid_state, no intent")
		if o := h.order("evac"); o.Status != protocol.StatusStaged || o.CoreVersion != 2 || o.ReleaseIntent != "" || o.ReleaseHeld != "" {
			t.Errorf("the evac reads %s at version %d, intent %q, chip %q; want staged at 2, untouched",
				o.Status, o.CoreVersion, o.ReleaseIntent, o.ReleaseHeld)
		}
		expectRows(t, "the evac's history", orderHistory(t, h.db, h.leg("evac")), []string{
			"pending->submitted: auto-submitted at creation",
		})
	})

	t.Run("invalid_state for a release still held", func(t *testing.T) {
		t.Parallel()
		// The click remembers a supply still driving to its wait (G1): its
		// intent is held, not sent.
		h := newRelHarness(t)
		h.steadyPair(pairSpec{mode: protocol.SwapModeTwoRobot})
		coreWord(h.handler, h.order("evac").UUID, protocol.StatusStaged, 2)
		h.coreStagesAt("evac", 0)
		coreWord(h.handler, h.order("supply").UUID, protocol.StatusDispatched, 2)
		testutil.MustNoErr(t, h.eng.ReleaseStagedOrders(h.nodeID, dispEmpty), "the pair click")
		expectRows(t, "intents after the click", pIntents(h), []string{"intent:evac=sent@0", "intent:supply=held@0"})
		before := h.order("supply")
		history := orderHistory(t, h.db, h.leg("supply"))
		trail := legTrail(h)
		h.mark()

		h.coreRefuses("supply", "invalid_state")
		trail.expect(t, "invalid_state on the held supply")
		after := h.order("supply")
		if after.Status != before.Status || after.CoreVersion != before.CoreVersion ||
			after.ReleaseIntent != before.ReleaseIntent || after.ReleaseHeld != before.ReleaseHeld {
			t.Errorf("the held supply moved: %s at %d, intent %q, chip %q; want %s at %d, intent %q, chip %q",
				after.Status, after.CoreVersion, after.ReleaseIntent, after.ReleaseHeld,
				before.Status, before.CoreVersion, before.ReleaseIntent, before.ReleaseHeld)
		}
		expectRows(t, "the supply's history", orderHistory(t, h.db, h.leg("supply")), history)
	})
}
