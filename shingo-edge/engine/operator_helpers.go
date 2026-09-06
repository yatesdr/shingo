package engine

import (
	"shingoedge/domain"
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
	claim := requestedClaimAtNode(db, node)
	return node, runtime, claim, nil
}

// loadActiveNode (Engine method) wraps the package loadActiveNode with a
// Core-owned-loader fallback: when a node has no per-style edge claim but IS a
// window/position of a Core loader, it returns a SYNTHESIZED manual_swap claim so
// the operator load/clear/request paths treat the node as a loader without a
// per-style style_node_claim. This completes the Core-owned loader refactor — Core
// owns the loader; the edge needs no style_node_claim to operate it. The synthetic
// claim has ID==0; callers that persist active_claim_id MUST guard on ID==0.
func (e *Engine) loadActiveNode(nodeID int64) (*processes.Node, *processes.RuntimeState, *processes.NodeClaim, error) {
	node, runtime, claim, err := loadActiveNode(e.db, nodeID)
	if err != nil || claim != nil || node == nil {
		return node, runtime, claim, err
	}
	if synth := e.synthLoaderClaim(node.CoreNodeName); synth != nil {
		claim = synth
	}
	return node, runtime, claim, nil
}

// synthLoaderClaim returns a synthesized manual_swap NodeClaim for a node that is a
// member of a Core-owned loader but has no per-style edge claim, or nil if the node
// belongs to no loader. Resolved through the SAME LoaderStore the runtime uses, so
// the synthesized view never diverges from dispatch. See domain.Loader.SynthClaim.
func (e *Engine) synthLoaderClaim(coreNodeName string) *processes.NodeClaim {
	if coreNodeName == "" {
		return nil
	}
	l, err := e.loaders().LoaderForNode(domain.NodeID(coreNodeName))
	if err != nil || l == nil {
		return nil
	}
	return l.SynthClaim(domain.NodeID(coreNodeName))
}

// requestedClaimAtNode finds the node claim governing this node right now.
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
//
// POST-FINALIZE DEPENDENCY, pinned by test (TestFindActiveClaim_PostCutover):
// finalizeChangeoverRow nils target_style_id as its first step, which KILLS the
// fallback branch — deliberately load-bearing, not a gap. Post-finalize
// resolution is correct-or-unreachable: active_style_id already points at the
// to-style (the flip precedes finalize), so the first branch answers for every
// node the new style claims, and a node the new style does NOT claim (a
// dropped node's straggler) resolves nil — which its callers treat as "no
// active claim", the honest answer. Review rejected a defensive fallback here
// as code for an unreachable case; if this ever fires wrong, fix the caller's
// expectation, do not widen this resolver.
// IT RETURNS THE REQUESTED CLAIM, AND THE NAME NOW SAYS SO. It was
// findActiveClaim, which reads as "the claim that is active at this node" — i.e.
// the one describing whatever is standing there. It is not that. It resolves
// from the PROCESS's active style, so it answers "what does this node's current
// style say should be here", and it moves the instant a changeover moves the
// process, whether or not anything physical moved with it.
//
// Most of its callers want exactly that: cell geometry, pairing, staging
// nodes, routing — all properties of the style's plan for the node, correctly
// requested-derived. The ones that wanted the carrier's identity were reading
// the wrong thing under a name that hid it. For those, the resident payload on
// the runtime row is the answer.
func requestedClaimAtNode(db *store.DB, node *processes.Node) *processes.NodeClaim {
	process, err := db.GetProcess(node.ProcessID)
	if err != nil {
		return nil
	}
	return requestedClaimForProcess(db, process, node)
}

// requestedClaimForProcess is requestedClaimAtNode for a caller that already holds the
// process row. Split out for the level sweep, which walks every node of every
// process once a period: re-deriving the process per node would turn one read
// into one per node for an answer it was already holding.
//
// ACTIVE FIRST — store.ActiveStyleFirst. It answers "which STYLE'S claim
// governs this node as configured now", which is what its callers want: counts,
// releases, evacuations, cell geometry. This sentence used to say "what is
// standing on this node now", which the resolver cannot know — it reads
// active_style_id and target_style_id and nothing about a carrier. What is
// standing there is the runtime row's lineside payload.
//
// The opposite precedence is a real and legitimate question, asked by
// orders.Manager for an order that does not exist yet; it lives under the same
// name with the other constant, so the two can no longer disagree without a
// reader seeing which was chosen.
func requestedClaimForProcess(db *store.DB, process *processes.Process, node *processes.Node) *processes.NodeClaim {
	return db.ResolveNodeClaim(process, node, store.ActiveStyleFirst)
}

// positionClaimForTask derives the per-position claim for a press position that owns
// changeover work but has no style_node_claims row under its own name.
//
// parentClaimID selects the side, and it is the node task's own record of what
// the position was planned from: ToClaimID for the material arriving,
// FromClaimID for the material physically on the position. Both point at the real
// persisted parent press-index row, because the synthesized claim the planner
// built kept the parent's ID.
//
// Returns nil for anything domain.PositionClaimFromParent does not recognise as a
// position of a press-index parent, so a caller that gets nil reports its own
// refusal rather than acting on an invented claim.
func (e *Engine) positionClaimForTask(parentClaimID *int64, coreNodeName string) *processes.NodeClaim {
	if parentClaimID == nil {
		return nil
	}
	parent, err := e.db.GetStyleNodeClaim(*parentClaimID)
	if err != nil {
		return nil
	}
	return domain.PositionClaimFromParent(parent, coreNodeName)
}

// changeoverToClaim resolves the TARGET-style claim a per-node changeover
// action acts on: the persisted row when the node has one, otherwise the position
// derivation for a fanned-out press position.
func (e *Engine) changeoverToClaim(toStyleID int64, node *processes.Node, task *processes.NodeTask) *processes.NodeClaim {
	if claim, err := e.db.GetStyleNodeClaimByNode(toStyleID, node.CoreNodeName); err == nil && claim != nil {
		return claim
	}
	if task == nil {
		return nil
	}
	return e.positionClaimForTask(task.ToClaimID, node.CoreNodeName)
}

// changeoverFromClaim resolves the OUTGOING claim — what is physically on the
// node — with the same position fallback.
func (e *Engine) changeoverFromClaim(node *processes.Node, task *processes.NodeTask) *processes.NodeClaim {
	if claim := requestedClaimAtNode(e.db, node); claim != nil {
		return claim
	}
	if task == nil {
		return nil
	}
	return e.positionClaimForTask(task.FromClaimID, node.CoreNodeName)
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
