package dispatch

import (
	"database/sql"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/nodes"
)

// nodeReads is the three node doors a complex attempt's step walks ask: a node
// by its dot-name, a node by id, and the lane a node stands in. *store.DB
// answers them from the database; attemptNodes from one attempt's read of its
// plan.
type nodeReads interface {
	GetNodeByDotName(name string) (*nodes.Node, error)
	GetNode(id int64) (*nodes.Node, error)
	LaneOf(n *nodes.Node) (*nodes.Node, error)
}

// attemptNodes is one complex attempt's read of its plan's nodes, in the
// store.GroupPassReads shape: read what the call chain needs once, carry it,
// forget it. The attempt's walks over its steps (the destination's and the
// hand's slot needs, the final drop's storage question and its capacity gate's,
// the slot book's node at a step (binsAtStep), the sources' find at each
// pickup, admission and the lane-revisit tripwire) each asked the store for a
// step's node by its dot-name and for its parent, again. Here every pickup and
// drop node the plan names is read in one statement (GetNodesByDotNames) at the
// first ask of one, so an attempt that stops before asking reads nothing; a node
// read by id is kept for the attempt; a name the plan did not name is read
// through and kept.
//
// THE SAME ANSWERS. A name the batch did not answer is absent, as
// GetNodeByDotName answers sql.ErrNoRows for it
// (TestGetByDotNames_AgreesWithGetByDotName); a batch that failed leaves the
// names unread and every ask reads the store as it did; an answer is kept, an
// error never. LaneOf is nodes.LaneOf's walk (LaneOfWith) over the kept
// parent. Pinned against the store: TestAttemptNodes_AnswerAsTheStoreDoes.
//
// WHAT IT MAY HOLD. The walks read a node's name, id, type, parent and
// synthetic mark, which nothing inside an attempt should change, and the sources'
// find its enabled mark, which a person sets (the node form,
// www/handlers_nodes.go; scenesync and the loader rename write it back as read)
// outside the scan lock, so a pass that read it a step earlier saw a state as
// current. None reads claimed_by from it; the slot and bin claims stay in SQL
// (ClaimSlotTx and the hand's confirms). Copies are handed out, so a caller that
// edits a node edits nothing the next ask is answered from. One per attempt
// (prepareComplexHandWith), dropped with it: nothing outlives the attempt.
type attemptNodes struct {
	db     *store.DB
	plan   []string // the plan's pickup and drop nodes, read at the first ask of one
	inPlan map[string]bool
	read   bool // the plan's statement has run
	failed bool // and failed: the plan's names are read from the store as asked
	byName map[string]*nodes.Node
	byID   map[int64]*nodes.Node
}

// readAttemptNodes is one attempt's view of the pickup and drop nodes steps
// name; it reads nothing until asked.
func readAttemptNodes(db *store.DB, steps []resolvedStep) *attemptNodes {
	r := &attemptNodes{db: db, inPlan: map[string]bool{}, byName: map[string]*nodes.Node{}, byID: map[int64]*nodes.Node{}}
	for _, s := range steps {
		if (s.Action == protocol.ActionPickup || s.Action == protocol.ActionDropoff) && s.Node != "" && !r.inPlan[s.Node] {
			r.inPlan[s.Node] = true
			r.plan = append(r.plan, s.Node)
		}
	}
	return r
}

// readPlan is the one statement: every name the plan names.
func (r *attemptNodes) readPlan() {
	r.read = true
	found, err := r.db.GetNodesByDotNames(r.plan)
	if err != nil {
		r.failed = true
		return
	}
	for name, n := range found {
		r.byName[name], r.byID[n.ID] = n, n
	}
}

// GetNodeByDotName is the store's door over the attempt's read: a plan name
// the plan's statement did not answer is absent (sql.ErrNoRows); any other
// name, and every name after a failed statement, is read from the store.
func (r *attemptNodes) GetNodeByDotName(name string) (*nodes.Node, error) {
	if r.inPlan[name] && !r.read {
		r.readPlan()
	}
	if n, ok := r.byName[name]; ok {
		return copyNode(n), nil
	}
	if r.inPlan[name] && !r.failed {
		return nil, sql.ErrNoRows
	}
	n, err := r.db.GetNodeByDotName(name)
	if err == nil && n != nil {
		r.byName[name], r.byID[n.ID] = n, n
	}
	return copyNode(n), err
}

// GetNode is the store's door over the attempt's read.
func (r *attemptNodes) GetNode(id int64) (*nodes.Node, error) {
	if n, ok := r.byID[id]; ok {
		return copyNode(n), nil
	}
	n, err := r.db.GetNode(id)
	if err == nil && n != nil {
		r.byID[id] = n
	}
	return copyNode(n), err
}

// LaneOf is nodes.LaneOf over the attempt's read of the parent.
func (r *attemptNodes) LaneOf(n *nodes.Node) (*nodes.Node, error) {
	return nodes.LaneOfWith(n, r.GetNode)
}

// copyNode is a shallow copy of n, nil for nil.
func copyNode(n *nodes.Node) *nodes.Node {
	if n == nil {
		return nil
	}
	c := *n
	return &c
}
