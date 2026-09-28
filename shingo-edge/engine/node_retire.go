package engine

import (
	"database/sql"
	"errors"
	"fmt"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// node_retire.go — the node-level half of the delete pair. A process delete
// ends the asking; a node retire ends the place. The demand episodes a node's
// disappearance strands are closed here, through the same ordinary close
// writer the process delete uses, so a demand never waits on a cell that
// cannot be satisfied again.

// RetireNode retires a process_node and closes the demand episodes whose
// origins point at it.
//
// The episodes worth closing are only those at THIS node: a process's other
// nodes can still satisfy the same need, so the list is filtered on the
// node's core name rather than closed wholesale. The close is
// `claim_removed`/`notification` for the same reason the process delete's is:
// the need did not recover, it stopped being asked.
//
// Idempotent: retiring a node that is already gone (or never existed) is a
// double-click and succeeds without doing anything.
func (e *Engine) RetireNode(nodeID int64) error {
	node, err := processes.GetNode(e.db.DB, nodeID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("retire node %d: read: %w", nodeID, err)
	}

	if node.ProcessName == "" {
		// Same disposition as the process delete's empty name: an episode key
		// built on an empty process names no place. Close nothing, retire
		// anyway — refusing would leave a node nobody can remove.
		e.logFn("demand_episode: node %d has an empty process name — retiring it without closing any "+
			"episode. A list keyed on an empty process would match other processes' unresolved "+
			"episodes, and closing those would end demands that are still being asked.", nodeID)
	} else if err := e.closeNodeEpisodes(node.ProcessName, node.CoreNodeName); err != nil {
		return err
	}

	// The piles the retire takes, read while they still exist.
	piles, err := e.db.ListLinesidePileKeysForNode(nodeID)
	if err != nil {
		return fmt.Errorf("retire node %d: list lineside piles: %w", nodeID, err)
	}
	e.countMu.Lock()
	defer e.countMu.Unlock()
	if err := e.processService.DeleteNode(nodeID); err != nil {
		return err
	}
	if len(piles) > 0 && e.inventoryDelta != nil {
		e.inventoryDelta.PilesChanged(piles...)
	}
	return nil
}

// closeNodeEpisodes closes every open episode whose origin is this node. It
// is closeProcessEpisodes with one difference: the filter. The list is read
// for the whole process (the origin rows name the process, not the node), and
// the node's own core name selects which episodes this retire actually ends —
// the others belong to places that are still alive.
//
// It stops at the first failure and the retire is refused with it, for the
// same reason as the process delete: the node stays, the operator retries,
// and no close is lost.
func (e *Engine) closeNodeEpisodes(processName, coreNodeName string) error {
	open, err := e.db.ListOpenDemandOriginsForProcess(processName)
	if err != nil {
		return fmt.Errorf("retire node %s: list open episodes: %w", coreNodeName, err)
	}
	for i := range open {
		ep := &open[i]
		if ep.CoreNodeName != coreNodeName {
			continue
		}
		e.logFn("demand_episode: node %s is being retired — closing origin=%s key=%s kind=%s",
			coreNodeName, ep.OriginID, ep.EpisodeKey, ep.Kind)
		if err := e.closeEpisode(ep.EpisodeKey, protocol.CloseReasonClaimRemoved, protocol.ClosedByNotification); err != nil {
			return fmt.Errorf("retire node %s: %w", coreNodeName, err)
		}
	}
	return nil
}
