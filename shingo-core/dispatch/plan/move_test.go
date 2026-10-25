package plan

import (
	"testing"

	"shingocore/domain"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// move_test.go — the bin move's plan: each rule that refuses a person's move,
// the take, and the auto pick's PickableAt.

// moveReader is the reads a concrete-destination move makes. Every other Reader
// method is the nil embedded interface, so a move reaching one panics the test.
type moveReader struct {
	Reader
	atNode   map[int64][]*bins.Bin
	dest     *nodes.Node
	occupied int
	fence    []*bins.BinType
}

func (r *moveReader) ListBinsByNode(id int64) ([]*bins.Bin, error) { return r.atNode[id], nil }
func (r *moveReader) GetNodeByDotName(string) (*nodes.Node, error) { return r.dest, nil }
func (r *moveReader) CountBinsByNode(int64) (int, error)           { return r.occupied, nil }
func (r *moveReader) CountInFlightOrdersByDeliveryNodeExcluding(string, int64) (int, error) {
	return 0, nil
}
func (r *moveReader) GetEffectiveBinTypes(int64) ([]*bins.BinType, error) { return r.fence, nil }
func (r *moveReader) BinTypeBareOf(int64) (*int64, error)                 { return nil, nil }

var (
	moveSource = &nodes.Node{ID: 1, Name: "SRC", Enabled: true}
	moveDest   = &nodes.Node{ID: 2, Name: "DST", Enabled: true}
)

func at(id, node int64) *bins.Bin {
	n := node
	return &bins.Bin{ID: id, Label: "B", NodeID: &n, BinTypeID: 7, Status: domain.BinStatusAvailable}
}

func movePlanner(r *moveReader) *Planner {
	if r.dest == nil {
		r.dest = moveDest
	}
	return NewPlanner(r, nil, nil, nil)
}

func wantRefused(t *testing.T, p Plan, rule RefusalRule) *Refusal {
	t.Helper()
	if p.Refused == nil || p.Refused.Rule != rule {
		t.Fatalf("plan refused = %+v, want rule %d", p.Refused, rule)
	}
	return p.Refused
}

func TestValidateMove_RefusesARobotsBinNamingTheHolder(t *testing.T) {
	t.Parallel()
	b := at(10, 1)
	holder := int64(44)
	b.ClaimedBy = &holder
	r := wantRefused(t, movePlanner(&moveReader{}).ValidateMove(Given{Bin: b, Source: moveSource, Dest: moveDest}), RefusedClaimed)
	if r.Holder != 44 {
		t.Fatalf("holder = %d, want 44: the refusal names the order holding the bin", r.Holder)
	}
}

func TestValidateMove_APendingReservationIsNoRefusal(t *testing.T) {
	t.Parallel()
	b := at(10, 1)
	b.HasPendingReservation = true
	p := movePlanner(&moveReader{}).ValidateMove(Given{Bin: b, Source: moveSource, Dest: moveDest})
	if p.Refused != nil || p.Dest != moveDest || p.Bin != b || p.Node != moveSource {
		t.Fatalf("plan = %+v, want the bin moved to DST: a soft hold is the issue's to take, by who asked (Q1)", p)
	}
}

func TestValidateMove_AHeldStatusIsThePersonsCall(t *testing.T) {
	t.Parallel()
	b := at(10, 1)
	b.Status = domain.BinStatusFlagged
	if p := movePlanner(&moveReader{}).ValidateMove(Given{Bin: b, Source: moveSource, Dest: moveDest}); p.Refused != nil {
		t.Fatalf("a person's move of a held bin refused: %+v", p.Refused)
	}
}

func TestValidateMove_EachRuleRefuses(t *testing.T) {
	t.Parallel()
	dead := &nodes.Node{ID: 2, Name: "DST", Enabled: false}
	locked := at(10, 1)
	locked.Locked = true
	for _, tc := range []struct {
		name string
		bin  *bins.Bin
		dest *nodes.Node
		r    *moveReader
		want RefusalRule
	}{
		{"not at the named source", at(10, 9), moveDest, &moveReader{}, RefusedNotAtSource},
		{"locked for handling", locked, moveDest, &moveReader{}, RefusedLocked},
		{"a dead destination (v4 §2.5)", at(10, 1), dead, &moveReader{dest: dead}, RefusedDestDisabled},
		{"a full destination", at(10, 1), moveDest, &moveReader{occupied: 1}, RefusedDestFull},
		{"a destination fenced to another carrier", at(10, 1), moveDest,
			&moveReader{fence: []*bins.BinType{{ID: 8, Code: "TOTE"}}}, RefusedCarrier},
	} {
		p := movePlanner(tc.r).ValidateMove(Given{Bin: tc.bin, Source: moveSource, Dest: tc.dest})
		if p.Refused == nil || p.Refused.Rule != tc.want {
			t.Errorf("%s: refused = %+v, want rule %d", tc.name, p.Refused, tc.want)
		}
	}
}

// TestChooseMove_PickableAtSkipsWhatAPersonCouldNotBeHandedBlind: claimed,
// pending-reserved, locked, a pickup-blocking status and a carrier the
// destination refuses are all skipped, and the first bin left is the move.
func TestChooseMove_PickableAtSkipsWhatAPersonCouldNotBeHandedBlind(t *testing.T) {
	t.Parallel()
	holder := int64(5)
	claimed, reserved, locked, held, wrongType, ok := at(1, 1), at(2, 1), at(3, 1), at(4, 1), at(5, 1), at(6, 1)
	claimed.ClaimedBy = &holder
	reserved.HasPendingReservation = true
	locked.Locked = true
	held.Status = domain.BinStatusFlagged
	wrongType.BinTypeID = 9
	r := &moveReader{
		atNode: map[int64][]*bins.Bin{1: {claimed, reserved, locked, held, wrongType, ok}},
		fence:  []*bins.BinType{{ID: 7, Code: "CART"}},
	}
	p := movePlanner(r).ChooseMove(moveSource, moveDest)
	if p.Refused != nil || p.Bin != ok {
		t.Fatalf("plan = %+v (bin %+v), want bin 6", p, p.Bin)
	}
	r.atNode[1] = []*bins.Bin{claimed, reserved, locked, held, wrongType}
	wantRefused(t, movePlanner(r).ChooseMove(moveSource, moveDest), RefusedNoFreeBin)
}
