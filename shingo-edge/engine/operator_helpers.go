package engine

import (
	"shingoedge/store"
	"shingoedge/store/processes"
)

// loadActiveNode loads a process node, its runtime state, and active claim.
// Pure function - takes db parameter instead of Engine receiver.
func loadActiveNode(db *store.DB, nodeID int64) (*processes.Node, *processes.RuntimeState, *processes.NodeClaim, error) {
	node, err := db.GetProcessNode(nodeID)
	if err != nil {
		return nil, nil, nil, err
	}
	runtime, err := db.EnsureProcessNodeRuntime(nodeID)
	if err != nil {
		return nil, nil, nil, err
	}
	claim := findActiveClaim(db, node)
	return node, runtime, claim, nil
}

// findActiveClaim finds the node claim governing this node right now.
// Pure function — takes db parameter instead of Engine receiver.
//
// Normal case (no changeover, or existing node during changeover):
// returns the claim under process.ActiveStyleID. Until cutover fires,
// the active style is still the from-style, and existing nodes keep
// their from-style claim — that's the correct "right now" answer.
//
// Add-node fallback: an "add-node" changeover (e.g., 1 node → 2 node)
// creates a brand-new process_node for the to-style. Before cutover,
// that new node has NO claim on the active (from) style — it didn't
// exist there. Without the fallback, every handler that asks "what
// claim describes this node?" gets nil and short-circuits. Plant
// 2026-05-12: bin arrived at the freshly-added node with the right
// UOP in Core, but handleNodeOrderDelivered (wiring_delivered.go:63)
// got claim==nil, returned early, and SetProcessNodeRuntimeForDeliveredBin
// never ran. HMI rendered remaining_uop_cached=0 (the default) instead
// of the bin's actual count.
//
// The fallback only fires when active-style lookup returns nil AND
// a target-style is configured (i.e. a changeover is in progress).
// After cutover, active_style_id == the prior target, and the first
// branch succeeds — behavior reverts to identical pre-fallback.
func findActiveClaim(db *store.DB, node *processes.Node) *processes.NodeClaim {
	process, err := db.GetProcess(node.ProcessID)
	if err != nil {
		return nil
	}
	if process.ActiveStyleID != nil {
		if claim, cerr := db.GetStyleNodeClaimByNode(*process.ActiveStyleID, node.CoreNodeName); cerr == nil && claim != nil {
			return claim
		}
	}
	if process.TargetStyleID != nil {
		if claim, cerr := db.GetStyleNodeClaimByNode(*process.TargetStyleID, node.CoreNodeName); cerr == nil && claim != nil {
			return claim
		}
	}
	return nil
}

// loadChangeoverNodeTask loads the changeover station task and node task for a given changeover and node.
// Pure function - takes db parameter instead of Engine receiver.
func loadChangeoverNodeTask(db *store.DB, changeoverID int64, node *processes.Node) (*processes.StationTask, *processes.NodeTask, error) {
	var changeoverTask *processes.StationTask
	if node.OperatorStationID != nil {
		task, err := db.GetChangeoverStationTaskByStation(changeoverID, *node.OperatorStationID)
		if err != nil {
			return nil, nil, err
		}
		changeoverTask = task
	}
	nodeTask, err := db.GetChangeoverNodeTaskByNode(changeoverID, node.ID)
	if err != nil {
		return nil, nil, err
	}
	return changeoverTask, nodeTask, nil
}
