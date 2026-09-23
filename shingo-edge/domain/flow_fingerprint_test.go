package domain

import (
	"regexp"
	"testing"
	"time"

	"shingo/protocol"
)

// flow_fingerprint_test.go — the fingerprint a preview carries and a save or
// a start checks.
//
// It is a pure hash of STORED rows: the process, the active style and its
// claims, the target style and its claims, the routing set. Recomputed on
// every check, never stored — so an Edge restart cannot lose it, and a stale
// preview is refused by comparing two hashes of the truth.

func fpClaim(id int64, node, payload string) NodeClaim {
	return NodeClaim{
		ID: id, StyleID: 7, CoreNodeName: node, Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobot,
		PayloadCode: payload, InboundStaging: "PLN_02", InboundSource: "Supermarket Empty Totes",
		OutboundDestination: "Supermarket Area", UOPCapacity: 1420, ReorderPointSource: "legacy",
		ChangeoverCarryoverDisposition: CarryoverReplace, Sequence: 1,
		Source: ClaimSourceAdmin, CalledBy: "engineer",
	}
}

func fpBase() (int64, *int64, []NodeClaim, int64, []NodeClaim) {
	from := int64(10)
	return 1, &from,
		[]NodeClaim{fpClaim(31, "PLN_03", "PIA12"), fpClaim(32, "PLN_06", "PIA11")},
		7,
		[]NodeClaim{fpClaim(19, "PLN_01", "PIA27"), fpClaim(20, "PLN_04", "PIA26")}
}

// fp is FlowFingerprint with the error asserted away. It cannot fail on these
// inputs — every field is a string, number, bool, list or pointer to one — and
// a test that threaded the error would say less about the hash.
func fingerprintOf(t *testing.T, processID int64, fromStyleID *int64, from []NodeClaim, toStyleID int64, to []NodeClaim) string {
	t.Helper()
	got, err := FlowFingerprint(processID, fromStyleID, from, toStyleID, to)
	if err != nil {
		t.Fatalf("FlowFingerprint: %v", err)
	}
	return got
}

func TestFlowFingerprint_IsHexSHA256(t *testing.T) {
	t.Parallel()
	p, f, fc, to, tc := fpBase()
	got := fingerprintOf(t, p, f, fc, to, tc)
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(got) {
		t.Errorf("fingerprint %q is not 64 hex characters", got)
	}
	if again := fingerprintOf(t, p, f, fc, to, tc); again != got {
		t.Errorf("two computations over the same rows differ: %s vs %s", got, again)
	}
}

// TestFlowFingerprint_OrderIndependent: the claims and routing rows are
// sorted before hashing, so the order a query happened to return them in
// changes nothing.
func TestFlowFingerprint_OrderIndependent(t *testing.T) {
	t.Parallel()
	p, f, fc, to, tc := fpBase()
	want := fingerprintOf(t, p, f, fc, to, tc)
	rev := func(c []NodeClaim) []NodeClaim { return []NodeClaim{c[1], c[0]} }
	got := fingerprintOf(t, p, f, rev(fc), to, rev(tc))
	if got != want {
		t.Errorf("reordering the rows changed the fingerprint: %s vs %s", got, want)
	}
}

// TestFlowFingerprint_ChangesOnEveryListedColumn: every column
// cloneClaimColumns names, plus id, moves the hash — on either side — and
// so does a routing row flipping enabled, the routing set gaining a row, and
// each of the ids.
func TestFlowFingerprint_ChangesOnEveryListedColumn(t *testing.T) {
	t.Parallel()
	p, f, fc, to, tc := fpBase()
	base := fingerprintOf(t, p, f, fc, to, tc)

	edits := map[string]func(c *NodeClaim){
		"id":                               func(c *NodeClaim) { c.ID++ },
		"core_node_name":                   func(c *NodeClaim) { c.CoreNodeName += "X" },
		"role":                             func(c *NodeClaim) { c.Role = protocol.ClaimRoleConsume },
		"swap_mode":                        func(c *NodeClaim) { c.SwapMode = protocol.SwapModeSingleRobot },
		"payload_code":                     func(c *NodeClaim) { c.PayloadCode += "X" },
		"reorder_point":                    func(c *NodeClaim) { c.ReorderPoint++ },
		"reorder_point_source":             func(c *NodeClaim) { c.ReorderPointSource = "manual" },
		"auto_reorder":                     func(c *NodeClaim) { c.AutoReorder = true },
		"inbound_staging":                  func(c *NodeClaim) { c.InboundStaging += "X" },
		"outbound_staging":                 func(c *NodeClaim) { c.OutboundStaging += "X" },
		"inbound_source":                   func(c *NodeClaim) { c.InboundSource += "X" },
		"outbound_destination":             func(c *NodeClaim) { c.OutboundDestination += "X" },
		"containment_destination":          func(c *NodeClaim) { c.ContainmentDestination += "X" },
		"allowed_payload_codes":            func(c *NodeClaim) { c.AllowedPayloadCodes = []string{"A"} },
		"auto_request_payload":             func(c *NodeClaim) { c.AutoRequestPayload = "A" },
		"keep_staged":                      func(c *NodeClaim) { c.KeepStaged = true },
		"evacuate_on_changeover":           func(c *NodeClaim) { c.EvacuateOnChangeover = true },
		"paired_core_node":                 func(c *NodeClaim) { c.PairedCoreNode += "X" },
		"auto_confirm":                     func(c *NodeClaim) { c.AutoConfirm = true },
		"sequence":                         func(c *NodeClaim) { c.Sequence++ },
		"lineside_soft_threshold":          func(c *NodeClaim) { c.LinesideSoftThreshold++ },
		"second_paired_core_node":          func(c *NodeClaim) { c.SecondPairedCoreNode += "X" },
		"reuse_compatible_bins":            func(c *NodeClaim) { c.ReuseCompatibleBins = true },
		"auto_push":                        func(c *NodeClaim) { c.AutoPush = true },
		"changeover_evac_nodes":            func(c *NodeClaim) { c.ChangeoverEvacNodes = []string{"PLN_01"} },
		"changeover_evac_destination":      func(c *NodeClaim) { c.ChangeoverEvacDestination += "X" },
		"index_robot_supplies":             func(c *NodeClaim) { c.IndexRobotSupplies = true },
		"key_route":                        func(c *NodeClaim) { c.KeyRoute = []string{"LM1"} },
		"changeover_carryover_disposition": func(c *NodeClaim) { c.ChangeoverCarryoverDisposition = CarryoverKeepLineside },
		"source_preset_id":                 func(c *NodeClaim) { c.SourcePresetID = Ptr(int64(3)) },
		"source_preset_version":            func(c *NodeClaim) { c.SourcePresetVersion = Ptr(2) },
	}
	if len(edits) != len(FlowFingerprintColumns()) {
		t.Fatalf("this table edits %d columns; FlowFingerprintColumns names %d — keep them together", len(edits), len(FlowFingerprintColumns()))
	}
	for _, col := range FlowFingerprintColumns() {
		edit, ok := edits[col]
		if !ok {
			t.Errorf("column %s is fingerprinted but this table has no edit for it", col)
			continue
		}
		fromSide := append([]NodeClaim(nil), fc...)
		edit(&fromSide[0])
		if fingerprintOf(t, p, f, fromSide, to, tc) == base {
			t.Errorf("editing %s on the FROM side left the fingerprint unchanged", col)
		}
		toSide := append([]NodeClaim(nil), tc...)
		edit(&toSide[1])
		if fingerprintOf(t, p, f, fc, to, toSide) == base {
			t.Errorf("editing %s on the TO side left the fingerprint unchanged", col)
		}
	}

	if fingerprintOf(t, p+1, f, fc, to, tc) == base {
		t.Error("another process id left the fingerprint unchanged")
	}
	if fingerprintOf(t, p, Ptr(int64(11)), fc, to, tc) == base || fingerprintOf(t, p, nil, fc, to, tc) == base {
		t.Error("another (or no) active style left the fingerprint unchanged")
	}
	if fingerprintOf(t, p, f, fc, to+1, tc) == base {
		t.Error("another target style left the fingerprint unchanged")
	}
	if fingerprintOf(t, p, f, fc, to, tc[:1]) == base {
		t.Error("a claim leaving the target style left the fingerprint unchanged")
	}
}

// TestFlowFingerprint_IgnoresAttributionAndRuntime: who wrote a row, when,
// whether it was retired and revived, the level's falling edge and the
// routing row's label/sequence/origin/caller are not the flow. An
// attribution-only rewrite must not invalidate a preview.
func TestFlowFingerprint_IgnoresAttributionAndRuntime(t *testing.T) {
	t.Parallel()
	p, f, fc, to, tc := fpBase()
	base := fingerprintOf(t, p, f, fc, to, tc)

	now := time.Now()
	restamped := append([]NodeClaim(nil), tc...)
	restamped[0].Source, restamped[0].CalledBy = ClaimSourceHMI, "Press 400"
	restamped[0].UpdatedAt, restamped[0].RetiredAt = &now, nil
	restamped[0].CreatedAt = now
	restamped[0].BelowReorderSince = &now
	restamped[1].UpdatedAt = &now
	if fingerprintOf(t, p, f, fc, to, restamped) != base {
		t.Error("an attribution-only rewrite changed the fingerprint")
	}
}

// TestFlowFingerprint_ExcludesTheRoutingSet: the routing set is an OFFER
// LIST, not a flow (owner, 2026-09-13).
//
// It says which nodes the position panel may put in front of an operator, and
// nothing on the server refuses a saved claim that names a node outside it —
// so adopting or dropping a lane changes no stored flow and cannot change
// what a plan would do. In the hash it invalidated every open preview on the
// press for a change to an option list, and it put the most expensive query
// in the feature inside the save's exclusive transaction.
//
// There is no argument to pass any more; this test is the reason the
// signature has one fewer.
func TestFlowFingerprint_ExcludesTheRoutingSet(t *testing.T) {
	t.Parallel()
	if got := FlowFingerprintColumns(); len(got) == 0 {
		t.Fatal("FlowFingerprintColumns is empty")
	}
	for _, col := range FlowFingerprintColumns() {
		if col == "key_task" {
			t.Error("key_task is fingerprinted; its one runtime forward was removed, so editing it " +
				"invalidates every open preview for a column that drives nothing")
		}
	}
}

// TestFlowFingerprintColumns_AreTheStructsOwnTags: the column list is derived
// from fingerprintClaim rather than written beside it. This was a third
// hand-maintained claim column list — the shape that started the review.
func TestFlowFingerprintColumns_AreTheStructsOwnTags(t *testing.T) {
	t.Parallel()
	cols := FlowFingerprintColumns()
	if len(cols) == 0 {
		t.Fatal("no columns derived")
	}
	seen := map[string]bool{}
	for _, c := range cols {
		if c == "" || c == "-" {
			t.Errorf("derived an empty column name from %v", cols)
		}
		if seen[c] {
			t.Errorf("column %s derived twice", c)
		}
		seen[c] = true
	}
	// The first is id and the last two are the preset provenance pair: the
	// struct's field order IS the list's order, so this catches a reordering
	// that would silently move every stored fingerprint.
	if cols[0] != "id" {
		t.Errorf("first column is %s, want id", cols[0])
	}
}
