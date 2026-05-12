// operator_changeover_start.go — kick off a changeover.
//
// StartProcessChangeover runs the preflight gate, calls planChangeover
// (see operator_changeover_plan.go), creates the changeover row via
// changeoverService, aborts in-flight orders on affected nodes, and
// emits all robot orders with embedded waits.

package engine

import (
	"context"
	"fmt"

	"shingoedge/store/processes"
)

// Error handling policy: log and continue. Do not add early returns without understanding the caller contract. See 2567plandiscussion.md.
func (e *Engine) StartProcessChangeover(processID, toStyleID int64, calledBy, notes string) (*processes.Changeover, error) {
	// Pre-flight inventory gate: refuse to start if Core reports any
	// required payload has zero available bins in the supermarket — the
	// changeover would deadlock at the first retrieve. Run BEFORE
	// planning so planning-side side effects (DB writes, robot aborts)
	// don't fire on a doomed start. preflightChecker is wired in tests
	// that don't care about the gate; nil-skip there.
	if e.preflightChecker != nil && e.coreClient != nil && e.coreClient.Available() {
		missing, perr := e.preflightChecker.PreflightInventoryCheck(context.Background(), toStyleID)
		if perr != nil {
			return nil, fmt.Errorf("changeover preflight: %w", perr)
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("changeover refused: missing bins for payloads %v", missing)
		}
	}
	plan, err := e.planChangeover(processID, toStyleID)
	if err != nil {
		return nil, err
	}

	if _, err := e.changeoverService.Create(processID, plan.process.ActiveStyleID, toStyleID,
		calledBy, notes, plan.stationIDs, plan.nodeTasks, plan.nodes); err != nil {
		return nil, err
	}

	// Abort pre-existing orders on affected nodes (not unchanged ones).
	for _, diff := range plan.diffs {
		if diff.Situation == SituationUnchanged {
			continue
		}
		node := findNodeByCoreName(plan.nodes, diff.CoreNodeName)
		if node != nil {
			e.AbortNodeOrders(node.ID)
		}
	}

	// Retrieve the changeover we just created so we can link node tasks.
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return nil, err
	}

	// Create ALL robot orders up front with embedded wait steps.
	// Operator controls flow by releasing waits, not by triggering individual orders.
	orderPlan := BuildChangeoverPlan(plan.diffs, plan.nodes, e.cfg.Web.AutoConfirm, e.activePullSnapshot(plan.nodes))
	e.applyChangeoverPlan(changeover, orderPlan)

	return e.db.GetActiveProcessChangeover(processID)
}

// binEmptyAtCoreNode returns a closure that reports whether the physical
// bin at a CoreNodeName is empty (RemainingUOPCached == 0) for nodes in
// the given process. The reuse-compatible-bins shortcut uses this to
// skip press-index swaps when the next style produces the same payload
// and reuse_compatible_bins is opted in. Errors collapse to "not empty"
// — defensive, never auto-skip a swap on the basis of a runtime read
// failure.
func (e *Engine) binEmptyAtCoreNode(processID int64) func(coreNodeName string) bool {
	nodes, err := e.db.ListProcessNodesByProcess(processID)
	if err != nil {
		return func(string) bool { return false }
	}
	idByName := make(map[string]int64, len(nodes))
	for _, n := range nodes {
		idByName[n.CoreNodeName] = n.ID
	}
	return func(name string) bool {
		id, ok := idByName[name]
		if !ok {
			return false
		}
		rt, err := e.db.GetProcessNodeRuntime(id)
		if err != nil || rt == nil {
			return false
		}
		return rt.RemainingUOPCached == 0
	}
}
