package engine

import "shingoedge/store"

// loadActiveNode loads a process node, its runtime state, and active claim.
// Pure function - takes db parameter instead of Engine receiver.
func loadActiveNode(db *store.DB, nodeID int64) (*store.ProcessNode, *store.ProcessNodeRuntimeState, *store.StyleNodeClaim, error) {
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

// findActiveClaim finds the active style node claim for a process node.
// Pure function - takes db parameter instead of Engine receiver.
func findActiveClaim(db *store.DB, node *store.ProcessNode) *store.StyleNodeClaim {
	process, err := db.GetProcess(node.ProcessID)
	if err != nil || process.ActiveStyleID == nil {
		return nil
	}
	claim, err := db.GetStyleNodeClaimByNode(*process.ActiveStyleID, node.CoreNodeName)
	if err != nil {
		return nil
	}
	return claim
}

// loadChangeoverNodeTask loads the changeover station task and node task for a given changeover and node.
// Pure function - takes db parameter instead of Engine receiver.
func loadChangeoverNodeTask(db *store.DB, changeoverID int64, node *store.ProcessNode) (*store.ChangeoverStationTask, *store.ChangeoverNodeTask, error) {
	var changeoverTask *store.ChangeoverStationTask
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
