package nodes

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"shingocore/store/internal/nodetree"
)

// The parentage guard: a node tree cannot be made into a node graph.
//
// WHY IT IS HERE AND NOT IN THE READERS. Every recursive walk over
// nodes.parent_id — the inheritance lookups, the group-scoped finders, the
// boundary sum — recurses until the chain runs out, so a cycle makes them run
// forever. There are seven of them and none is cycle-safe (see the nodetree
// package doc). Hardening seven readers against bad data is the expensive,
// incomplete answer; the cheap, complete one is upstream, where the bad data
// would be written. A cycle that cannot be created does not have to be survived.
//
// WHAT IS DELIBERATELY NOT HERE: any repair for a cycle that already exists in a
// database. Detection at write time makes new ones impossible; pre-existing
// corruption is an operator/SQL matter, and an auto-repair would have to guess
// which edge in the loop was the mistake. CENSUS-mg1-queries-2026-08-17.sql
// carries a read-only query so a plant can be checked.

// MaxNodeDepth bounds the ancestor walk the guard performs, in HOPS up the
// parent chain — not to be confused with nodes.depth, which is a slot's position
// within its lane and is what lane reachability compares.
//
// It is a TERMINATION BOUND, not a modelled limit on how deep a plant may nest.
// The real hierarchy is three or four levels (group → lane → slot); sixty-four
// is far past anything a floor could have and small enough that the walk is
// bounded work on a database that is already corrupt — which is the case the
// bound exists for, since a guard that hangs on the data it guards against is
// not a guard.
const MaxNodeDepth = 64

// ParentCycleError is the refusal, carrying the chain that proves it.
//
// It names the whole path rather than just the two nodes, because "you cannot
// put A under B" is a statement somebody will disagree with until they can see
// WHY — and the why is a chain that may be four nodes long and is not visible
// on the screen they are looking at.
type ParentCycleError struct {
	NodeID     int64
	NodeName   string
	ParentID   int64
	ParentName string
	// Chain runs from the proposed parent upward to the node being moved: the
	// path that already exists and that the new edge would close into a loop.
	Chain []string
}

func (e *ParentCycleError) Error() string {
	if e.NodeID == e.ParentID {
		return fmt.Sprintf("cannot make %s its own parent", e.NodeName)
	}
	return fmt.Sprintf("cannot move %s under %s: %s is already inside %s (%s)",
		e.NodeName, e.ParentName, e.ParentName, e.NodeName, strings.Join(e.Chain, " → "))
}

// IsParentCycle reports whether err is a parentage-cycle refusal, so an HTTP
// layer can answer 400 rather than 500: the request is well-formed and the
// caller can fix it by choosing a different parent.
func IsParentCycle(err error) bool {
	var pce *ParentCycleError
	return errors.As(err, &pce)
}

// CheckParentage refuses a parent assignment that would create a loop.
//
// Called by every write that sets a non-null parent_id — Update and SetParent,
// which is every live path between them (Reparent goes through SetParent, and
// Create cannot cycle: a node's id does not exist when its parent is chosen, so
// it can be nobody's ancestor).
//
// THE CHECK IS "IS THE PROPOSED PARENT ALREADY BELOW ME", asked by walking UP
// from the parent. Walking down from the node would mean enumerating a whole
// subtree; walking up is one chain, bounded, and it terminates at the root in
// the overwhelmingly common case where there is no cycle at all.
//
// A read failure REFUSES rather than allows. "The rules say no" and "we could
// not find out" are different answers, but only one of them is safe to guess at
// here: letting a write through on a query that failed is how the corruption
// this exists to prevent gets in.
func CheckParentage(db *sql.DB, nodeID, parentID int64) error {
	if nodeID == 0 || parentID == 0 {
		return nil
	}

	// The trivial case, and the one a mistyped form actually produces. Checked
	// first because the ancestor walk below starts AT the parent and would
	// report it at zero hops — true, but a worse sentence than this one.
	if nodeID == parentID {
		name := nodeName(db, nodeID)
		return &ParentCycleError{NodeID: nodeID, NodeName: name, ParentID: parentID, ParentName: name}
	}

	rows, err := db.Query(nodetree.AncestorsWithinDepth(1, 2)+`
		SELECT a.id, n.name, a.hops
		FROM ancestors a JOIN nodes n ON n.id = a.id
		ORDER BY a.hops`, parentID, MaxNodeDepth)
	if err != nil {
		return fmt.Errorf("check parentage of node %d under %d: %w", nodeID, parentID, err)
	}
	defer rows.Close()

	var chain []string
	for rows.Next() {
		var id int64
		var name string
		var hops int
		if err := rows.Scan(&id, &name, &hops); err != nil {
			return fmt.Errorf("check parentage of node %d under %d: %w", nodeID, parentID, err)
		}
		chain = append(chain, name)
		if id == nodeID {
			return &ParentCycleError{
				NodeID:     nodeID,
				NodeName:   name,
				ParentID:   parentID,
				ParentName: chain[0],
				Chain:      chain,
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("check parentage of node %d under %d: %w", nodeID, parentID, err)
	}
	return nil
}

// nodeName is best-effort, for an error message only. A node whose name cannot
// be read still gets refused; it is just named by id.
func nodeName(db *sql.DB, id int64) string {
	var name string
	if err := db.QueryRow(`SELECT name FROM nodes WHERE id=$1`, id).Scan(&name); err != nil || name == "" {
		return fmt.Sprintf("node %d", id)
	}
	return name
}
