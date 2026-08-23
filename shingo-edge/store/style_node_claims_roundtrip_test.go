package store

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// claimEditorBody is the request body the claims editor actually sends: every
// field the modal owns a control for, and nothing else. It is the shape of
// processes.js's claimBody / claimToBody, expressed in Go so the round-trip is
// testable without a browser.
//
// The four columns it does NOT carry — sequence, reorder_point_source,
// keep_staged, auto_reorder — are the point. The editor has no control for any
// of them, so it must not be able to change them.
func claimEditorBody(c *processes.NodeClaim) processes.NodeClaimInput {
	return processes.NodeClaimInput{
		StyleID:               c.StyleID,
		CoreNodeName:          c.CoreNodeName,
		Role:                  c.Role,
		SwapMode:              c.SwapMode,
		PayloadCode:           c.PayloadCode,
		AllowedPayloadCodes:   c.AllowedPayloadCodes,
		UOPCapacity:           c.UOPCapacity,
		ReorderPoint:          c.ReorderPoint,
		LinesideSoftThreshold: c.LinesideSoftThreshold,
		InboundStaging:        c.InboundStaging,
		OutboundStaging:       c.OutboundStaging,
		InboundSource:         c.InboundSource,
		OutboundDestination:   c.OutboundDestination,
		AutoRequestPayload:    c.AutoRequestPayload,
		EvacuateOnChangeover:  c.EvacuateOnChangeover,
		ReuseCompatibleBins:   c.ReuseCompatibleBins,
		AutoPush:              c.AutoPush,
		PairedCoreNode:        c.PairedCoreNode,
		SecondPairedCoreNode:  c.SecondPairedCoreNode,
		AutoConfirm:           c.AutoConfirm,
	}
}

// TestUpsertStyleNodeClaim_EditorSaveIsANoOp is the characterization test for
// the save-stomp.
//
// fetch -> claimEditorBody -> save -> fetch must be the identity on EVERY
// persisted column. Before the pointer contract it was not: updateClaim wrote
// all 22 columns unconditionally, so the four the editor does not send were
// reset on every save of an unrelated field — board order to 0, the reorder
// point's provenance to "legacy", both flags to false. An engineer changing an
// inbound source silently reordered the board.
//
// Distinctive non-default values throughout, so a column that gets reset says
// so instead of coincidentally matching its zero value.
func TestUpsertStyleNodeClaim_EditorSaveIsANoOp(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	processID, err := db.CreateProcess("RT-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	styleID, err := db.CreateStyle("RT-STYLE", "", processID)
	testutil.MustNoErr(t, err, "create style")

	seed := processes.NodeClaimInput{
		StyleID:               styleID,
		CoreNodeName:          "RT-NODE",
		Role:                  protocol.ClaimRoleProduce,
		SwapMode:              protocol.SwapModeTwoRobotPressIndex,
		PayloadCode:           "PART-RT",
		AllowedPayloadCodes:   []string{"PART-RT", "PART-RT2"},
		UOPCapacity:           720,
		ReorderPoint:          37,
		LinesideSoftThreshold: 12,
		InboundStaging:        "RT-IN-STAGE",
		OutboundStaging:       "RT-OUT-STAGE",
		InboundSource:         "RT-EMPTIES",
		OutboundDestination:   "RT-MARKET",
		AutoRequestPayload:    "PART-RT",
		EvacuateOnChangeover:  true,
		ReuseCompatibleBins:   true,
		AutoPush:              true,
		PairedCoreNode:        "RT-NODE-B",
		SecondPairedCoreNode:  "RT-NODE-C",
		AutoConfirm:           true,
		// The four the editor never sends, seeded to values that are NOT the
		// zero value, so a stomp is visible.
		Sequence:           domain.Ptr(7),
		ReorderPointSource: domain.Ptr("calculated"),
		AutoReorder:        domain.Ptr(true),
		KeepStaged:         domain.Ptr(true),
	}
	claimID, err := db.UpsertStyleNodeClaim(seed)
	testutil.MustNoErr(t, err, "seed claim")

	before, err := db.GetStyleNodeClaim(claimID)
	testutil.MustNoErr(t, err, "fetch before")

	// The editor round-trip: fetch, map to the body it sends, save.
	if _, err := db.UpsertStyleNodeClaim(claimEditorBody(before)); err != nil {
		t.Fatalf("editor save: %v", err)
	}

	after, err := db.GetStyleNodeClaim(claimID)
	testutil.MustNoErr(t, err, "fetch after")

	// Column by column, named, so a failure says WHICH one moved.
	for _, c := range []struct {
		col         string
		before, got any
	}{
		{"role", before.Role, after.Role},
		{"swap_mode", before.SwapMode, after.SwapMode},
		{"payload_code", before.PayloadCode, after.PayloadCode},
		{"uop_capacity", before.UOPCapacity, after.UOPCapacity},
		{"reorder_point", before.ReorderPoint, after.ReorderPoint},
		{"reorder_point_source", before.ReorderPointSource, after.ReorderPointSource},
		{"auto_reorder", before.AutoReorder, after.AutoReorder},
		{"inbound_staging", before.InboundStaging, after.InboundStaging},
		{"outbound_staging", before.OutboundStaging, after.OutboundStaging},
		{"inbound_source", before.InboundSource, after.InboundSource},
		{"outbound_destination", before.OutboundDestination, after.OutboundDestination},
		{"auto_request_payload", before.AutoRequestPayload, after.AutoRequestPayload},
		{"keep_staged", before.KeepStaged, after.KeepStaged},
		{"evacuate_on_changeover", before.EvacuateOnChangeover, after.EvacuateOnChangeover},
		{"paired_core_node", before.PairedCoreNode, after.PairedCoreNode},
		{"second_paired_core_node", before.SecondPairedCoreNode, after.SecondPairedCoreNode},
		{"auto_confirm", before.AutoConfirm, after.AutoConfirm},
		{"sequence", before.Sequence, after.Sequence},
		{"lineside_soft_threshold", before.LinesideSoftThreshold, after.LinesideSoftThreshold},
		{"reuse_compatible_bins", before.ReuseCompatibleBins, after.ReuseCompatibleBins},
		{"auto_push", before.AutoPush, after.AutoPush},
	} {
		if c.before != c.got {
			t.Errorf("%s changed across an editor save: %v -> %v (the editor owns no control for it)",
				c.col, c.before, c.got)
		}
	}
	if len(after.AllowedPayloadCodes) != len(before.AllowedPayloadCodes) {
		t.Errorf("allowed_payload_codes changed: %v -> %v", before.AllowedPayloadCodes, after.AllowedPayloadCodes)
	}

	// And the seeded values really were distinctive — a test that seeded zeros
	// would pass no matter what updateClaim did.
	if before.Sequence != 7 || before.ReorderPointSource != "calculated" || !before.AutoReorder || !before.KeepStaged {
		t.Fatalf("seed did not take, so this test proves nothing: seq=%d source=%q autoReorder=%v keepStaged=%v",
			before.Sequence, before.ReorderPointSource, before.AutoReorder, before.KeepStaged)
	}
}

// A writer that DOES speak about the four still changes them. "Absent means
// untouched" must not become "unchangeable" — the replenishment admin page and
// the board reorder both depend on being able to write them.
func TestUpsertStyleNodeClaim_ExplicitOptionalFieldsStillWrite(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	processID, err := db.CreateProcess("RT2-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	styleID, err := db.CreateStyle("RT2-STYLE", "", processID)
	testutil.MustNoErr(t, err, "create style")

	base := processes.NodeClaimInput{
		StyleID:            styleID,
		CoreNodeName:       "RT2-NODE",
		Role:               protocol.ClaimRoleConsume,
		SwapMode:           protocol.SwapModeSingleRobot,
		PayloadCode:        "PART-RT2",
		InboundStaging:     "RT2-IN",
		OutboundStaging:    "RT2-OUT",
		Sequence:           domain.Ptr(3),
		ReorderPointSource: domain.Ptr("calculated"),
		AutoReorder:        domain.Ptr(true),
		KeepStaged:         domain.Ptr(true),
	}
	claimID, err := db.UpsertStyleNodeClaim(base)
	testutil.MustNoErr(t, err, "seed claim")

	upd := base
	upd.Sequence = domain.Ptr(9)
	upd.ReorderPointSource = domain.Ptr("manual")
	upd.AutoReorder = domain.Ptr(false)
	upd.KeepStaged = domain.Ptr(false)
	if _, err := db.UpsertStyleNodeClaim(upd); err != nil {
		t.Fatalf("explicit update: %v", err)
	}

	after, err := db.GetStyleNodeClaim(claimID)
	testutil.MustNoErr(t, err, "fetch after")
	if after.Sequence != 9 {
		t.Errorf("sequence = %d, want 9", after.Sequence)
	}
	if after.ReorderPointSource != "manual" {
		t.Errorf("reorder_point_source = %q, want manual", after.ReorderPointSource)
	}
	if after.AutoReorder {
		t.Error("auto_reorder = true, want false — an explicit false must write")
	}
	if after.KeepStaged {
		t.Error("keep_staged = true, want false — an explicit false must write")
	}
}

// A new claim still gets its defaults: absent means untouched only on UPDATE,
// because only UPDATE has a prior value to leave alone.
func TestUpsertStyleNodeClaim_InsertDefaultsForAbsentOptionals(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	processID, err := db.CreateProcess("RT3-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	styleID, err := db.CreateStyle("RT3-STYLE", "", processID)
	testutil.MustNoErr(t, err, "create style")

	mk := func(node string) int64 {
		id, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
			StyleID:         styleID,
			CoreNodeName:    node,
			Role:            protocol.ClaimRoleConsume,
			SwapMode:        protocol.SwapModeSingleRobot,
			PayloadCode:     "PART-RT3",
			InboundStaging:  "RT3-IN",
			OutboundStaging: "RT3-OUT",
		})
		testutil.MustNoErr(t, err, "create claim "+node)
		return id
	}
	firstID, secondID := mk("RT3-A"), mk("RT3-B")

	first, err := db.GetStyleNodeClaim(firstID)
	testutil.MustNoErr(t, err, "fetch first")
	second, err := db.GetStyleNodeClaim(secondID)
	testutil.MustNoErr(t, err, "fetch second")

	if first.Sequence != 1 || second.Sequence != 2 {
		t.Errorf("sequences = %d, %d; want 1, 2 — an absent sequence takes the next free board slot",
			first.Sequence, second.Sequence)
	}
	if first.ReorderPointSource != "legacy" {
		t.Errorf("reorder_point_source = %q, want legacy", first.ReorderPointSource)
	}
	if first.AutoReorder || first.KeepStaged {
		t.Errorf("new claim flags = autoReorder:%v keepStaged:%v, want both false",
			first.AutoReorder, first.KeepStaged)
	}
}
