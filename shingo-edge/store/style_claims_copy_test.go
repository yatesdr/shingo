package store

import (
	"slices"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// CopyStyleClaims is the "Copy Node Claims" action: one style's claim set,
// verbatim, onto a sibling style â€” the same column list Clone Style uses,
// but into an EXISTING style (a replace, not a scaffold). These pin the
// mechanics: replace semantics, the two payload modes, and the guards the
// store itself owns. The active-style and cross-process RULES live in the
// service layer and are tested there.

// seedClaimRow adds one claim with just the fields the copy test cares about.
// The capacity argument seeds the PAYLOAD CATALOG, not the claim: uop_capacity
// is a dead claim column since the capacity.SQL rework — the value resolves
// from the catalog on every read, keyed on the claim's payload code — so a
// test wanting capacity N puts N in the catalog.
func seedClaimRow(t *testing.T, db *DB, styleID int64, coreNode string, role protocol.ClaimRole, payload string, uop int) {
	t.Helper()
	_, err := processes.UpsertClaim(db.DB, processes.NodeClaimInput{
		StyleID:      styleID,
		CoreNodeName: coreNode,
		Role:         role,
		SwapMode:     protocol.SwapModeSequential,
		PayloadCode:  payload,
	})
	testutil.MustNoErr(t, err, "seed claim "+coreNode)
	if payload != "" && uop > 0 {
		_, err := db.DB.Exec(`INSERT OR IGNORE INTO payload_catalog (name, code, uop_capacity, updated_at)
			VALUES (?, ?, ?, datetime('now'))`, payload, payload, uop)
		testutil.MustNoErr(t, err, "seed catalog "+payload)
	}
}

func claimByNode(t *testing.T, db *DB, styleID int64, coreNode string) *processes.NodeClaim {
	t.Helper()
	claims, err := processes.ListClaims(db.DB, styleID)
	testutil.MustNoErr(t, err, "ListClaims")
	for i := range claims {
		if claims[i].CoreNodeName == coreNode {
			return &claims[i]
		}
	}
	return nil
}

func TestCopyStyleClaims_VerbatimReplace(t *testing.T) {
	db := coverageDB(t)
	processID, _, _, _ := seedProcessWithChildren(t, db, "CopyVerbatim") // process + one style + one claim
	// Reuse the seeded style as the source; add a target with DIFFERENT claims.
	src, err := processes.ListStylesByProcess(db.DB, processID)
	testutil.MustNoErr(t, err, "list styles")
	srcID := src[0].ID
	tgtID, err := db.CreateStyle("CopyTarget", "", processID)
	testutil.MustNoErr(t, err, "create target")
	seedClaimRow(t, db, srcID, "N-PRESS", "produce", "PART-SRC", 40)
	seedClaimRow(t, db, tgtID, "N-PRESS", "consume", "PART-TGT", 99)
	seedClaimRow(t, db, tgtID, "N-ONLY-TGT", "produce", "PART-TGT", 5)

	_, err = db.CopyStyleClaims(srcID, tgtID, true, nil)
	testutil.MustNoErr(t, err, "copy verbatim")

	// The target now mirrors the source exactly: role, payload, capacity.
	got := claimByNode(t, db, tgtID, "N-PRESS")
	if got == nil || got.Role != "produce" || got.PayloadCode != "PART-SRC" || got.UOPCapacity != 40 {
		t.Errorf("target N-PRESS = %+v, want produce/PART-SRC/40 from source", got)
	}
	// Replace semantics: a node the source does not have is GONE.
	if claimByNode(t, db, tgtID, "N-ONLY-TGT") != nil {
		t.Errorf("target-only node survived a replace copy")
	}
	// The source is untouched.
	if srcClaim := claimByNode(t, db, srcID, "N-PRESS"); srcClaim == nil || srcClaim.PayloadCode != "PART-SRC" {
		t.Errorf("source claim changed: %+v", srcClaim)
	}
}

func TestCopyStyleClaims_KeepsTargetPayloads(t *testing.T) {
	db := coverageDB(t)
	processID, _, _, _ := seedProcessWithChildren(t, db, "CopyKeepPayload")
	src, err := processes.ListStylesByProcess(db.DB, processID)
	testutil.MustNoErr(t, err, "list styles")
	srcID := src[0].ID
	tgtID, err := db.CreateStyle("KeepPayloadTarget", "", processID)
	testutil.MustNoErr(t, err, "create target")
	seedClaimRow(t, db, srcID, "N-PRESS", "produce", "PART-SRC", 40)
	seedClaimRow(t, db, srcID, "N-STAGE", "consume", "PART-SRC", 0)
	seedClaimRow(t, db, tgtID, "N-PRESS", "consume", "PART-MINE", 99)

	// includePayloads=false: choreography comes from the source, payloads
	// stay the TARGET's on the node it shares; a source node the target
	// never had arrives with the source's payload (nothing to preserve).
	_, err = db.CopyStyleClaims(srcID, tgtID, false, nil)
	testutil.MustNoErr(t, err, "copy keep-payloads")

	press := claimByNode(t, db, tgtID, "N-PRESS")
	if press == nil || press.Role != "produce" || press.PayloadCode != "PART-MINE" {
		t.Errorf("N-PRESS = %+v, want source role with target's PART-MINE", press)
	}
	stage := claimByNode(t, db, tgtID, "N-STAGE")
	if stage == nil || stage.PayloadCode != "PART-SRC" {
		t.Errorf("N-STAGE = %+v, want source's PART-SRC (no target payload to keep)", stage)
	}
}

func TestCopyStyleClaims_SameStyleAndMissingSource(t *testing.T) {
	db := coverageDB(t)
	processID, _, _, _ := seedProcessWithChildren(t, db, "CopyGuards")
	src, err := processes.ListStylesByProcess(db.DB, processID)
	testutil.MustNoErr(t, err, "list styles")
	srcID := src[0].ID

	if _, err := db.CopyStyleClaims(srcID, srcID, true, nil); err == nil {
		t.Errorf("copy onto self must error")
	}
	if _, err := db.CopyStyleClaims(srcID, 999999, true, nil); err == nil {
		t.Errorf("missing target must error")
	}
}

func TestCopyStyleClaims_OverridesApply(t *testing.T) {
	db := coverageDB(t)
	processID, _, _, _ := seedProcessWithChildren(t, db, "CopyOverrides")
	src, err := processes.ListStylesByProcess(db.DB, processID)
	testutil.MustNoErr(t, err, "list styles")
	srcID := src[0].ID
	tgtID, err := db.CreateStyle("OverridesTarget", "", processID)
	testutil.MustNoErr(t, err, "create target")
	seedClaimRow(t, db, srcID, "N-PRESS", "produce", "PART-SRC", 40)
	seedClaimRow(t, db, srcID, "N-STAGE", "consume", "PART-SRC", 0)

	// Blank = inherit: N-STAGE's row carries no overrides and must arrive
	// exactly as copied. N-PRESS carries the whole field set.
	notes, err := db.CopyStyleClaims(srcID, tgtID, true, []processes.ClaimOverride{
		{
			Node:                "N-PRESS",
			Role:                "consume",
			PayloadCode:         "P-OVR",
			InboundSource:       "SRC-IN",
			OutboundDestination: "SRC-OUT",
			InboundStaging:      "STG-IN",
			OutboundStaging:     "STG-OUT",
		},
	})
	testutil.MustNoErr(t, err, "copy with overrides")
	if len(notes) != 0 {
		t.Errorf("clean overrides produced notes: %v", notes)
	}
	press := claimByNode(t, db, tgtID, "N-PRESS")
	if press == nil {
		t.Fatalf("renamed/overridden claim missing")
	}
	if press.Role != "consume" || press.PayloadCode != "P-OVR" {
		t.Errorf("N-PRESS = %s/%s, want consume/P-OVR", press.Role, press.PayloadCode)
	}
	// The overridden payload is sourceable: walk.go filters on the allowed
	// list, so it must contain the override even though the source's list
	// never had it.
	if !slices.Contains(press.AllowedPayloadCodes, "P-OVR") {
		t.Errorf("allowed list = %v, want it to contain P-OVR", press.AllowedPayloadCodes)
	}
	if press.InboundSource != "SRC-IN" || press.OutboundDestination != "SRC-OUT" ||
		press.InboundStaging != "STG-IN" || press.OutboundStaging != "STG-OUT" {
		t.Errorf("N-PRESS routing = %+v, want overridden values", press)
	}
	stage := claimByNode(t, db, tgtID, "N-STAGE")
	if stage == nil || stage.Role != "consume" || stage.PayloadCode != "PART-SRC" {
		t.Errorf("N-STAGE = %+v, want the copied claim untouched", stage)
	}
}

func TestCopyStyleClaims_RenameCascadeAndCollision(t *testing.T) {
	db := coverageDB(t)
	processID, _, _, _ := seedProcessWithChildren(t, db, "CopyRename")
	src, err := processes.ListStylesByProcess(db.DB, processID)
	testutil.MustNoErr(t, err, "list styles")
	srcID := src[0].ID
	tgtID, err := db.CreateStyle("RenameTarget", "", processID)
	testutil.MustNoErr(t, err, "create target")
	seedClaimRow(t, db, srcID, "N-A", "produce", "PART-A", 10)
	// N-B pairs with N-A the way a press-index cell does: valid upsert.
	_, err = processes.UpsertClaim(db.DB, processes.NodeClaimInput{
		StyleID: srcID, CoreNodeName: "N-B", Role: protocol.ClaimRoleProduce,
		SwapMode:       protocol.SwapModeTwoRobotPressIndex,
		PairedCoreNode: "N-A", OutboundDestination: "LINE-OUT",
	})
	testutil.MustNoErr(t, err, "seed press-index claim")
	seedClaimRow(t, db, srcID, "N-C", "consume", "PART-C", 7)

	notes, err := db.CopyStyleClaims(srcID, tgtID, true, []processes.ClaimOverride{
		{Node: "N-A", CoreNodeName: "N-REN"},
		{Node: "N-C", CoreNodeName: "N-REN"},
	})
	testutil.MustNoErr(t, err, "copy with renames")
	ren := claimByNode(t, db, tgtID, "N-REN")
	if ren == nil {
		t.Fatalf("renamed claim missing")
	}
	if claimByNode(t, db, tgtID, "N-A") != nil {
		t.Errorf("old node name still present after rename")
	}
	// The pairing partner followed the rename.
	b := claimByNode(t, db, tgtID, "N-B")
	if b == nil || b.PairedCoreNode != "N-REN" {
		t.Errorf("N-B = %+v, want PairedCoreNode N-REN after the partner rename", b)
	}
	// The second rename collides with the name the first created: refused,
	// noted, N-C keeps its name.
	if claimByNode(t, db, tgtID, "N-C") == nil {
		t.Errorf("N-C vanished — a refused rename deleted its claim")
	}
	joined := strings.Join(notes, " | ")
	if !strings.Contains(joined, "already has a claim") {
		t.Errorf("notes = %v, want the collision refusal", notes)
	}
	if !strings.Contains(joined, "reference updated") {
		t.Errorf("notes = %v, want the partner-reference cascade", notes)
	}
}

func TestCopyStyleClaims_PressIndexDistinctnessAndRoleWithhold(t *testing.T) {
	db := coverageDB(t)
	processID, _, _, _ := seedProcessWithChildren(t, db, "CopyValidity")
	src, err := processes.ListStylesByProcess(db.DB, processID)
	testutil.MustNoErr(t, err, "list styles")
	srcID := src[0].ID
	tgtID, err := db.CreateStyle("ValidityTarget", "", processID)
	testutil.MustNoErr(t, err, "create target")
	_, err = processes.UpsertClaim(db.DB, processes.NodeClaimInput{
		StyleID: srcID, CoreNodeName: "N-X", Role: protocol.ClaimRoleProduce,
		SwapMode:       protocol.SwapModeTwoRobotPressIndex,
		PairedCoreNode: "N-P1", OutboundDestination: "LINE-OUT",
	})
	testutil.MustNoErr(t, err, "seed press-index claim")
	// Two-robot WITHOUT its required staging, inserted raw: the upsert path
	// would refuse it, but a stored claim predating the validation (or hand
	// edited) can hold one — the override layer must meet the claim as it is.
	_, err = db.DB.Exec(`INSERT INTO style_node_claims (style_id, core_node_name, role, swap_mode)
		VALUES (?, 'N-RAW', 'consume', 'two_robot')`, srcID)
	testutil.MustNoErr(t, err, "seed raw two_robot claim")

	notes, err := db.CopyStyleClaims(srcID, tgtID, true, []processes.ClaimOverride{
		{Node: "N-X", CoreNodeName: "N-P1"},                    // would collapse front onto back
		{Node: "N-RAW", Role: "produce", PayloadCode: "P-RAW"}, // role invalid, payload fine
	})
	testutil.MustNoErr(t, err, "copy with invalid-shape overrides")
	joined := strings.Join(notes, " | ")
	if !strings.Contains(joined, "positions must stay distinct") {
		t.Errorf("notes = %v, want the press-index rename refusal", notes)
	}
	if !strings.Contains(joined, "role change withheld") {
		t.Errorf("notes = %v, want the withheld role note", notes)
	}
	// The refused rename left the pair as it arrived: the claim is still
	// named N-X and still points its back position at N-P1 (a paired node
	// has no claim row of its own to be found by name).
	x := claimByNode(t, db, tgtID, "N-X")
	if x == nil || x.PairedCoreNode != "N-P1" {
		t.Errorf("refused rename must leave the claim as copied: %+v", x)
	}
	// The withheld role did not take, but the sibling payload override did.
	raw := claimByNode(t, db, tgtID, "N-RAW")
	if raw == nil || raw.Role != "consume" {
		t.Errorf("N-RAW = %+v, want copied role consume (override withheld)", raw)
	}
	if raw == nil || raw.PayloadCode != "P-RAW" {
		t.Errorf("N-RAW = %+v, want payload override P-RAW applied", raw)
	}
}

// Pass 1's paired-position gate on a two_robot_press_index claim: front, back
// and third must stay distinct, the offending field is withheld alone with a
// note, and the gate reads the IN-MEMORY row, so a second_paired override is
// checked against a paired override the same row just applied. The gate does
// not apply outside press-index: a sequential claim takes the same values.
func TestCopyStyleClaims_PairedOverrideDistinctness(t *testing.T) {
	db := coverageDB(t)
	processID, _, _, _ := seedProcessWithChildren(t, db, "CopyPairedGate")
	src, err := processes.ListStylesByProcess(db.DB, processID)
	testutil.MustNoErr(t, err, "list styles")
	srcID := src[0].ID
	tgtID, err := db.CreateStyle("PairedGateTarget", "", processID)
	testutil.MustNoErr(t, err, "create target")
	for _, c := range []struct{ node, paired string }{{"N-X", "N-P1"}, {"N-Y", "N-Q1"}, {"N-Z", "N-R1"}} {
		_, err = processes.UpsertClaim(db.DB, processes.NodeClaimInput{
			StyleID: srcID, CoreNodeName: c.node, Role: protocol.ClaimRoleProduce,
			SwapMode:       protocol.SwapModeTwoRobotPressIndex,
			PairedCoreNode: c.paired, OutboundDestination: "LINE-OUT",
		})
		testutil.MustNoErr(t, err, "seed press-index claim "+c.node)
	}
	seedClaimRow(t, db, srcID, "N-SEQ", "consume", "PART-SEQ", 0)

	notes, err := db.CopyStyleClaims(srcID, tgtID, true, []processes.ClaimOverride{
		// Both positions refused: paired names the front itself, and second
		// names the (unchanged) back. The sibling field still applies.
		{Node: "N-X", PairedCoreNode: "N-X", SecondPairedCoreNode: "N-P1", InboundSource: "SRC-IN"},
		// Distinct values apply.
		{Node: "N-Y", PairedCoreNode: "N-Q2", SecondPairedCoreNode: "N-Q3"},
		// Second collides with the paired value this same row just set.
		{Node: "N-Z", PairedCoreNode: "N-R2", SecondPairedCoreNode: "N-R2"},
		// Not press-index: no distinctness gate.
		{Node: "N-SEQ", PairedCoreNode: "N-SEQ", SecondPairedCoreNode: "N-SEQ"},
	})
	testutil.MustNoErr(t, err, "copy with paired overrides")
	joined := strings.Join(notes, " | ")
	for _, want := range []string{
		`node "N-X": paired_core_node override refused`,
		`node "N-X": second_paired_core_node override refused`,
		`node "N-Z": second_paired_core_node override refused`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes = %v, want %q", notes, want)
		}
	}
	if len(notes) != 3 {
		t.Errorf("notes = %v, want exactly the three refusals", notes)
	}

	x := claimByNode(t, db, tgtID, "N-X")
	if x == nil || x.PairedCoreNode != "N-P1" || x.SecondPairedCoreNode != "" || x.InboundSource != "SRC-IN" {
		t.Errorf("N-X = %+v, want paired N-P1, no second, inbound SRC-IN", x)
	}
	y := claimByNode(t, db, tgtID, "N-Y")
	if y == nil || y.PairedCoreNode != "N-Q2" || y.SecondPairedCoreNode != "N-Q3" {
		t.Errorf("N-Y = %+v, want paired N-Q2, second N-Q3", y)
	}
	z := claimByNode(t, db, tgtID, "N-Z")
	if z == nil || z.PairedCoreNode != "N-R2" || z.SecondPairedCoreNode != "" {
		t.Errorf("N-Z = %+v, want paired N-R2 applied and second withheld", z)
	}
	seq := claimByNode(t, db, tgtID, "N-SEQ")
	if seq == nil || seq.PairedCoreNode != "N-SEQ" || seq.SecondPairedCoreNode != "N-SEQ" {
		t.Errorf("N-SEQ = %+v, want both paired overrides applied (no gate outside press-index)", seq)
	}
}

// Pass 1's allowed-list coherence on a payload override appends the new code
// only when the list neither holds it already nor holds the "*" wildcard,
// which already admits every payload.
func TestCopyStyleClaims_PayloadOverrideAllowedList(t *testing.T) {
	db := coverageDB(t)
	processID, _, _, _ := seedProcessWithChildren(t, db, "CopyAllowedList")
	src, err := processes.ListStylesByProcess(db.DB, processID)
	testutil.MustNoErr(t, err, "list styles")
	srcID := src[0].ID
	tgtID, err := db.CreateStyle("AllowedListTarget", "", processID)
	testutil.MustNoErr(t, err, "create target")
	for _, c := range []struct {
		node    string
		allowed []string
	}{{"N-WILD", []string{"*"}}, {"N-HAS", []string{"PART-A", "P-OVR"}}} {
		_, err = processes.UpsertClaim(db.DB, processes.NodeClaimInput{
			StyleID: srcID, CoreNodeName: c.node, Role: protocol.ClaimRoleConsume,
			SwapMode: protocol.SwapModeSequential, PayloadCode: "PART-A",
			AllowedPayloadCodes: c.allowed,
		})
		testutil.MustNoErr(t, err, "seed claim "+c.node)
	}

	notes, err := db.CopyStyleClaims(srcID, tgtID, true, []processes.ClaimOverride{
		{Node: "N-WILD", PayloadCode: "P-OVR"},
		{Node: "N-HAS", PayloadCode: "P-OVR"},
	})
	testutil.MustNoErr(t, err, "copy with payload overrides")
	if len(notes) != 0 {
		t.Errorf("clean overrides produced notes: %v", notes)
	}
	wild := claimByNode(t, db, tgtID, "N-WILD")
	if wild == nil || wild.PayloadCode != "P-OVR" || !slices.Equal(wild.AllowedPayloadCodes, []string{"*"}) {
		t.Errorf("N-WILD = %+v, want payload P-OVR with the allowed list left at [*]", wild)
	}
	has := claimByNode(t, db, tgtID, "N-HAS")
	if has == nil || has.PayloadCode != "P-OVR" || !slices.Equal(has.AllowedPayloadCodes, []string{"PART-A", "P-OVR"}) {
		t.Errorf("N-HAS = %+v, want payload P-OVR with the allowed list unchanged", has)
	}
}
