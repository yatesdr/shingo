package dispatch

import (
	"database/sql"
	"errors"
	"testing"

	"shingocore/store/nodes"
)

// fakeCapacityDB lets us drive every branch of CheckDropoffCapacity
// without spinning up the dispatcher harness. Each call returns
// pre-loaded values; nil/zero values represent "not found" or "empty."
type fakeCapacityDB struct {
	node        *nodes.Node
	getNodeErr  error
	binCount    int
	binCountErr error
	inFlight    int
	inFlightErr error

	// NGRP support: walk children + per-child counts.
	children       []*nodes.Node
	childrenErr    error
	binsByChild    map[int64]int
	inFlightByName map[string]int
}

func (f *fakeCapacityDB) GetNodeByDotName(string) (*nodes.Node, error) {
	return f.node, f.getNodeErr
}
func (f *fakeCapacityDB) CountBinsByNode(nodeID int64) (int, error) {
	if c, ok := f.binsByChild[nodeID]; ok {
		return c, nil
	}
	return f.binCount, f.binCountErr
}
func (f *fakeCapacityDB) CountInFlightOrdersByDeliveryNodeExcluding(name string, _ int64) (int, error) {
	if c, ok := f.inFlightByName[name]; ok {
		return c, nil
	}
	return f.inFlight, f.inFlightErr
}
func (f *fakeCapacityDB) ListChildNodes(int64) ([]*nodes.Node, error) {
	return f.children, f.childrenErr
}

// TestCheckDropoffCapacity is the table-driven gate test for Phase 4 of
// bin-transit-state. Locks down every behavior of the predicate so a
// future regression doesn't quietly stop blocking (or start blocking
// on a path that should pass through). Per the regression-test rigor
// pillar: positive AND negative cases for the predicate.
func TestCheckDropoffCapacity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		deliveryNode string
		db           *fakeCapacityDB
		wantBlocked  bool
		wantCause    string // expected structured cause; "" means no cause expected
	}{
		{
			name:         "empty deliveryNode is never blocked",
			deliveryNode: "",
			db:           &fakeCapacityDB{},
			wantBlocked:  false,
		},
		{
			// A node that is not in the table is not a capacity problem. Let
			// dispatch produce the real error rather than queueing forever on a
			// reason that can never clear.
			name:         "node not found is not blocked (forward progress)",
			deliveryNode: "TYPO-NODE",
			db:           &fakeCapacityDB{getNodeErr: sql.ErrNoRows},
			wantBlocked:  false,
		},
		{
			// Same read, different fact. This used to be folded in with the row
			// above, so a database blip on the node lookup passed the gate while
			// the identical blip on either read below failed it closed.
			name:         "node lookup read error fails closed (blocked)",
			deliveryNode: "CONC-NODE",
			db:           &fakeCapacityDB{getNodeErr: errors.New("db blip")},
			wantBlocked:  true,
			wantCause:    "capacity-check-failed",
		},
		{
			name:         "nil node with no error is not blocked",
			deliveryNode: "GONE-NODE",
			db:           &fakeCapacityDB{},
			wantBlocked:  false,
		},
		{
			name:         "R06-1: bin-count read error fails closed (blocked)",
			deliveryNode: "CONC-NODE",
			db:           &fakeCapacityDB{node: &nodes.Node{ID: 7, Name: "CONC-NODE"}, binCountErr: errors.New("db blip")},
			wantBlocked:  true,
			wantCause:    "capacity-check-failed",
		},
		{
			name:         "R06-1: in-flight read error fails closed (blocked)",
			deliveryNode: "CONC-NODE",
			db:           &fakeCapacityDB{node: &nodes.Node{ID: 7, Name: "CONC-NODE"}, inFlightErr: errors.New("db blip")},
			wantBlocked:  true,
			wantCause:    "capacity-check-failed",
		},
		// ── the three NGRP arms ──────────────────────────────────────────
		// All three used to fail open, and the two per-child ones failed open in
		// the worst direction: `err == nil && count > 0` means a read error skips
		// the continue and the child is counted FREE — "there is room", on no
		// evidence. The outer gate's reads have failed closed since they were
		// written; these are the same reads one level down.
		{
			name:         "NGRP child list unreadable fails closed (blocked)",
			deliveryNode: "SMG_01",
			db: &fakeCapacityDB{
				node:        &nodes.Node{ID: 5, Name: "SMG_01", IsSynthetic: true, NodeTypeCode: "NGRP"},
				childrenErr: errors.New("db blip"),
			},
			wantBlocked: true,
			wantCause:   "capacity-check-failed",
		},
		{
			// Not "ngrp-full": nothing was learned about these slots, and telling
			// an operator a group is full sends them to go clear one that may be
			// empty.
			name:         "NGRP per-child bin read error fails closed, and says so",
			deliveryNode: "SMG_01",
			db: &fakeCapacityDB{
				node: &nodes.Node{ID: 5, Name: "SMG_01", IsSynthetic: true, NodeTypeCode: "NGRP"},
				children: []*nodes.Node{
					{ID: 51, Name: "SMG_01_S1", Enabled: true},
					{ID: 52, Name: "SMG_01_S2", Enabled: true},
				},
				binCountErr: errors.New("db blip"),
			},
			wantBlocked: true,
			wantCause:   "capacity-check-failed",
		},
		{
			name:         "NGRP per-child in-flight read error fails closed, and says so",
			deliveryNode: "SMG_01",
			db: &fakeCapacityDB{
				node: &nodes.Node{ID: 5, Name: "SMG_01", IsSynthetic: true, NodeTypeCode: "NGRP"},
				children: []*nodes.Node{
					{ID: 51, Name: "SMG_01_S1", Enabled: true},
					{ID: 52, Name: "SMG_01_S2", Enabled: true},
				},
				binsByChild: map[int64]int{51: 0, 52: 0}, // both empty, so the bin read succeeds
				inFlightErr: errors.New("db blip"),
			},
			wantBlocked: true,
			wantCause:   "capacity-check-failed",
		},
		{
			// The other half of the rule: one readable free child still passes,
			// even though its sibling could not be read. A free slot that was
			// actually SEEN is evidence, and one is enough.
			name:         "NGRP with one readable free child passes despite an unreadable sibling",
			deliveryNode: "SMG_01",
			db: &fakeCapacityDB{
				node: &nodes.Node{ID: 5, Name: "SMG_01", IsSynthetic: true, NodeTypeCode: "NGRP"},
				children: []*nodes.Node{
					{ID: 51, Name: "SMG_01_S1", Enabled: true},
					{ID: 52, Name: "SMG_01_S2", Enabled: true},
				},
				binsByChild:    map[int64]int{52: 0}, // S2 readable and empty
				inFlightByName: map[string]int{"SMG_01_S2": 0},
				binCountErr:    errors.New("db blip"), // S1 unreadable
			},
			wantBlocked: false,
		},
		{
			name:         "synthetic LANE defers to lane planner (not gated here)",
			deliveryNode: "LANE_A",
			db:           &fakeCapacityDB{node: &nodes.Node{ID: 5, Name: "LANE_A", IsSynthetic: true, NodeTypeCode: "LANE"}, binCount: 99},
			wantBlocked:  false,
		},
		{
			name:         "NGRP with all children FREE: not blocked (resolver picks at dispatch)",
			deliveryNode: "SMG_01",
			db: &fakeCapacityDB{
				node: &nodes.Node{ID: 5, Name: "SMG_01", IsSynthetic: true, NodeTypeCode: "NGRP"},
				children: []*nodes.Node{
					{ID: 51, Name: "SMG_01_S1", Enabled: true},
					{ID: 52, Name: "SMG_01_S2", Enabled: true},
				},
				// All children empty, no in-flight inbound.
			},
			wantBlocked: false,
		},
		{
			name:         "NGRP with at least ONE free child: not blocked",
			deliveryNode: "SMG_01",
			db: &fakeCapacityDB{
				node: &nodes.Node{ID: 5, Name: "SMG_01", IsSynthetic: true, NodeTypeCode: "NGRP"},
				children: []*nodes.Node{
					{ID: 51, Name: "SMG_01_S1", Enabled: true},
					{ID: 52, Name: "SMG_01_S2", Enabled: true},
					{ID: 53, Name: "SMG_01_S3", Enabled: true},
				},
				binsByChild: map[int64]int{51: 1, 52: 1}, // S3 still free
			},
			wantBlocked: false,
		},
		{
			name:         "NGRP fully saturated by physical bins: blocked",
			deliveryNode: "SMG_01",
			db: &fakeCapacityDB{
				node: &nodes.Node{ID: 5, Name: "SMG_01", IsSynthetic: true, NodeTypeCode: "NGRP"},
				children: []*nodes.Node{
					{ID: 51, Name: "SMG_01_S1", Enabled: true},
					{ID: 52, Name: "SMG_01_S2", Enabled: true},
				},
				binsByChild: map[int64]int{51: 1, 52: 1},
			},
			wantBlocked: true,
			wantCause:   "ngrp-full",
		},
		{
			name:         "NGRP fully saturated by in-flight orders inbound: blocked",
			deliveryNode: "SMG_01",
			db: &fakeCapacityDB{
				node: &nodes.Node{ID: 5, Name: "SMG_01", IsSynthetic: true, NodeTypeCode: "NGRP"},
				children: []*nodes.Node{
					{ID: 51, Name: "SMG_01_S1", Enabled: true},
					{ID: 52, Name: "SMG_01_S2", Enabled: true},
				},
				inFlightByName: map[string]int{"SMG_01_S1": 1, "SMG_01_S2": 1},
			},
			wantBlocked: true,
			wantCause:   "ngrp-full",
		},
		{
			name:         "NGRP mix of physical + in-flight saturating all: blocked",
			deliveryNode: "SMG_01",
			db: &fakeCapacityDB{
				node: &nodes.Node{ID: 5, Name: "SMG_01", IsSynthetic: true, NodeTypeCode: "NGRP"},
				children: []*nodes.Node{
					{ID: 51, Name: "SMG_01_S1", Enabled: true},
					{ID: 52, Name: "SMG_01_S2", Enabled: true},
				},
				binsByChild:    map[int64]int{51: 1},
				inFlightByName: map[string]int{"SMG_01_S2": 1},
			},
			wantBlocked: true,
			wantCause:   "ngrp-full",
		},
		{
			name:         "NGRP with disabled + synthetic children skipped from count",
			deliveryNode: "SMG_01",
			db: &fakeCapacityDB{
				node: &nodes.Node{ID: 5, Name: "SMG_01", IsSynthetic: true, NodeTypeCode: "NGRP"},
				children: []*nodes.Node{
					{ID: 51, Name: "SMG_01_S1", Enabled: true},                        // counts
					{ID: 52, Name: "SMG_01_S2", Enabled: false},                       // skipped (disabled)
					{ID: 53, Name: "SMG_01_NESTED", Enabled: true, IsSynthetic: true}, // skipped (synthetic)
				},
				binsByChild: map[int64]int{51: 1},
			},
			wantBlocked: true,
			wantCause:   "ngrp-full", // only S1 counted; it's full
		},
		{
			name:         "NGRP with no usable children passes through (resolver surfaces error)",
			deliveryNode: "SMG_01",
			db: &fakeCapacityDB{
				node:     &nodes.Node{ID: 5, Name: "SMG_01", IsSynthetic: true, NodeTypeCode: "NGRP"},
				children: []*nodes.Node{}, // empty group
			},
			wantBlocked: false,
		},
		{
			name:         "concrete node, empty: not blocked (the green path)",
			deliveryNode: "LINE_01",
			db:           &fakeCapacityDB{node: &nodes.Node{ID: 7, Name: "LINE_01"}},
			wantBlocked:  false,
		},
		{
			name:         "concrete node, occupied by 1 bin: blocked with bin-count reason",
			deliveryNode: "LINE_01",
			db:           &fakeCapacityDB{node: &nodes.Node{ID: 7, Name: "LINE_01"}, binCount: 1},
			wantBlocked:  true,
			wantCause:    "dropoff-occupied",
		},
		{
			name:         "concrete node, occupied by multiple bins (mis-tracked state)",
			deliveryNode: "LINE_01",
			db:           &fakeCapacityDB{node: &nodes.Node{ID: 7, Name: "LINE_01"}, binCount: 3},
			wantBlocked:  true,
			wantCause:    "dropoff-occupied",
		},
		{
			name:         "concrete node empty but in-flight inbound: blocked with in-flight reason",
			deliveryNode: "LINE_01",
			db:           &fakeCapacityDB{node: &nodes.Node{ID: 7, Name: "LINE_01"}, inFlight: 1},
			wantBlocked:  true,
			wantCause:    "dropoff-inflight",
		},
		{
			name:         "bin AND in-flight both nonzero: bin-count branch wins (deterministic)",
			deliveryNode: "LINE_01",
			db:           &fakeCapacityDB{node: &nodes.Node{ID: 7, Name: "LINE_01"}, binCount: 1, inFlight: 1},
			wantBlocked:  true,
			wantCause:    "dropoff-occupied",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocked, block := CheckDropoffCapacity(tc.db, tc.deliveryNode, 0)
			if blocked != tc.wantBlocked {
				t.Errorf("CheckDropoffCapacity blocked = %v, want %v (cause=%q)", blocked, tc.wantBlocked, block.Cause)
			}
			if tc.wantCause != "" && string(block.Cause) != tc.wantCause {
				t.Errorf("cause = %q, want %q", block.Cause, tc.wantCause)
			}
			// A blocked result always carries a cause and pins the destination; a
			// not-blocked result carries neither.
			if blocked && block.Cause == "" {
				t.Errorf("blocked but cause is empty")
			}
			if blocked && block.Params.Destination == "" {
				t.Errorf("blocked but destination param is empty")
			}
			if !blocked && (block.Cause != "" || block.Params.Destination != "") {
				t.Errorf("not blocked but got cause/destination %+v", block)
			}
		})
	}
}
