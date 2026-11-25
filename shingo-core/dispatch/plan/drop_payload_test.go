package plan

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"shingo/protocol"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// TestDropPayload: a drop asks its group with its own payload; with none, it
// asks with what it carries — nothing for an empty, the order's for a full (C2b
// Q4). A full set down mid-route must stay fenced by the payload it carries.
func TestDropPayload(t *testing.T) {
	t.Parallel()
	// steps: 0 pickup, 1 drop, 2 pickup, 3 drop
	pickup := func(j int) bool { return j == 0 || j == 2 }
	for _, tc := range []struct {
		name        string
		i           int
		stepPayload string
		emptyAt     map[int]bool
		want        string
	}{
		{"an empty's drop asks with nothing", 1, "", map[int]bool{0: true}, ""},
		{"a full's drop asks with the order's", 1, "", nil, "ORDER"},
		{"the step's own payload wins", 1, "STEP", map[int]bool{0: true}, "STEP"},
		{"the NEAREST pickup decides", 3, "", map[int]bool{0: true}, "ORDER"},
		{"no pickup ahead: the order's", 0, "", nil, "ORDER"},
	} {
		got := DropPayload(tc.i, tc.stepPayload, "ORDER", pickup, func(j int) bool { return tc.emptyAt[j] })
		if got != tc.want {
			t.Errorf("%s: DropPayload = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// walkReader is a store's node doors over one table of nodes, for the replay
// walk's read (walkNodes): GetNodeByDotName answers a name or sql.ErrNoRows, as
// the store's does; GetNodesByDotNames answers every name it finds, the store's
// shape (TestGetByDotNames_AgreesWithGetByDotName pins the real pair), or fails
// when failBatch is set. Every other Reader method is the nil embedded interface,
// so a walk reaching one panics the test.
type walkReader struct {
	Reader
	byName    map[string]*nodes.Node
	binsAt    map[int64][]*bins.Bin
	failBatch bool
}

func (r *walkReader) GetNodeByDotName(name string) (*nodes.Node, error) {
	n, ok := r.byName[name]
	if !ok {
		return nil, sql.ErrNoRows
	}
	c := *n
	return &c, nil
}

func (r *walkReader) GetNodesByDotNames(names []string) (map[string]*nodes.Node, error) {
	if r.failBatch {
		return nil, errors.New("connection refused")
	}
	out := map[string]*nodes.Node{}
	for _, name := range names {
		if n, err := r.GetNodeByDotName(name); err == nil {
			out[name] = n
		}
	}
	return out, nil
}

func (r *walkReader) ListBinsByNode(id int64) ([]*bins.Bin, error) { return r.binsAt[id], nil }

// TestWalkNodes_AnswerAsTheStoreDoes pins the replay walk's one read of its
// steps' nodes (walkNodes) to the store's door: for every name a step names
// (a pickup, a group drop, a dotted child, an unknown name, one named twice), a
// lane wait's point, and a name no step names, the walk's GetNodeByDotName
// answers what the store's does (the same node, or sql.ErrNoRows), asked once and
// asked again after the caller edits the node it was handed; with the
// statement failed, every name is the store's answer. ListBinsByNode is the
// store's. The found column states which names resolve, so a fixture that stops
// building what it says fails here rather than agreeing vacuously.
func TestWalkNodes_AnswerAsTheStoreDoes(t *testing.T) {
	t.Parallel()
	parent := int64(3)
	depth := 2
	r := &walkReader{byName: map[string]*nodes.Node{
		"WN-A":    {ID: 1, Name: "WN-A", Enabled: true},
		"WN-GRP":  {ID: 2, Name: "WN-GRP", IsSynthetic: true, NodeTypeCode: protocol.NodeClassNGRP},
		"WN-L.S1": {ID: 4, Name: "WN-S1", ParentID: &parent, Depth: &depth, ParentName: "WN-L"},
		"WN-PT":   {ID: 5, Name: "WN-PT"}, // a Core node a lane wait's point happens to share
		"WN-OFF":  {ID: 6, Name: "WN-OFF"},
	}, binsAt: map[int64][]*bins.Bin{1: {{ID: 9, BinTypeID: 7}}}}
	steps := []Step{
		{Action: protocol.ActionPickup, Node: "WN-A"},
		{Action: protocol.ActionDropoff, Node: "WN-GRP"},
		{Action: protocol.ActionWait, Node: "WN-PT", WaitKind: WaitKindLane},
		{Action: protocol.ActionPickup, Node: "WN-L.S1"},
		{Action: protocol.ActionDropoff, Node: "WN-NOPE"},
		{Action: protocol.ActionDropoff},
		{Action: protocol.ActionPickup, Node: "WN-A"},
	}
	cases := []struct {
		name  string
		found bool
	}{
		{"WN-A", true}, {"WN-GRP", true}, {"WN-L.S1", true}, {"WN-NOPE", false},
		{"WN-PT", true},    // the lane wait's point: no step asks it of the walk, read through
		{"WN-OFF", true},   // named by no step: read through
		{"WN-ELSE", false}, // named by no step and unknown
	}
	for _, failBatch := range []bool{false, true} {
		r.failBatch = failBatch
		w := readWalkNodes(r, steps)
		for _, c := range cases {
			for ask := 1; ask <= 2; ask++ {
				want, wErr := r.GetNodeByDotName(c.name)
				got, gErr := w.GetNodeByDotName(c.name)
				if (wErr == nil) != (gErr == nil) || (wErr != nil && !errors.Is(gErr, sql.ErrNoRows)) ||
					!reflect.DeepEqual(want, got) {
					t.Errorf("failed statement %v, %q (ask %d): the store answers %+v, %v; the walk %+v, %v",
						failBatch, c.name, ask, want, wErr, got, gErr)
					continue
				}
				if (got != nil) != c.found {
					t.Errorf("%q found %+v, want found=%v", c.name, got, c.found)
				}
				if got != nil {
					got.Name, got.IsSynthetic = "EDITED", !got.IsSynthetic // the caller's copy, not the walk's
				}
			}
		}
		if got, err := w.ListBinsByNode(1); err != nil || !reflect.DeepEqual(got, r.binsAt[1]) {
			t.Errorf("failed statement %v: ListBinsByNode(1) = %+v, %v; the store's %+v", failBatch, got, err, r.binsAt[1])
		}
	}
}
