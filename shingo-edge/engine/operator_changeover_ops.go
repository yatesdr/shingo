package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/engine/changeover"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// changeoverPlan holds all pre-computed data needed to start a changeover.
// Built by planChangeover (read-only), consumed by StartProcessChangeover (mutations).
type changeoverPlan struct {
	process    *processes.Process
	style      *processes.Style
	stations   []stations.Station
	stationIDs []int64
	diffs      []ChangeoverNodeDiff
	nodes      []processes.Node
	nodeTasks  []processes.NodeTaskInput
}

// planChangeover assembles all data needed for a changeover without writing anything.
// Returns an error if the changeover request is invalid (wrong style, already active, etc).
//
// Note: validation errors use changeover-specific messages ("process is already running
// style %d", etc). If this is later reused for a dry-run API, the error messages will
// still be appropriate — but callers should be aware they're changeover-flavored.
func (e *Engine) planChangeover(processID, toStyleID int64) (*changeoverPlan, error) {
	process, err := e.db.GetProcess(processID)
	if err != nil {
		return nil, err
	}
	if process.ActiveStyleID != nil && *process.ActiveStyleID == toStyleID {
		return nil, fmt.Errorf("process is already running style %d", toStyleID)
	}
	if _, err := e.db.GetActiveProcessChangeover(processID); err == nil {
		return nil, fmt.Errorf("process already has an active changeover")
	} else if err != sql.ErrNoRows {
		return nil, err
	}
	style, err := e.db.GetStyle(toStyleID)
	if err != nil {
		return nil, err
	}
	if style.ProcessID != processID {
		return nil, fmt.Errorf("target style %d does not belong to process %d", toStyleID, processID)
	}

	// Pre-fetch all data before opening transaction (SQLite deadlock prevention)
	stations, err := e.db.ListOperatorStationsByProcess(processID)
	if err != nil {
		return nil, err
	}
	var fromClaims, toClaims []processes.NodeClaim
	if process.ActiveStyleID != nil {
		fromClaims, err = e.db.ListStyleNodeClaims(*process.ActiveStyleID)
		if err != nil {
			return nil, fmt.Errorf("list from-style claims: %w", err)
		}
	}
	toClaims, err = e.db.ListStyleNodeClaims(toStyleID)
	if err != nil {
		return nil, fmt.Errorf("list to-style claims: %w", err)
	}
	diffs, err := e.applyChangeoverDiffPostProcessors(processID, DiffStyleClaims(fromClaims, toClaims))
	if err != nil {
		return nil, err
	}
	nodes, err := e.db.ListProcessNodesByProcess(processID)
	if err != nil {
		return nil, err
	}

	stationIDs := make([]int64, len(stations))
	for i := range stations {
		stationIDs[i] = stations[i].ID
	}

	nodeTasks := make([]processes.NodeTaskInput, len(diffs))
	for i, diff := range diffs {
		state := "unchanged"
		switch diff.Situation {
		case SituationSwap, SituationEvacuate, SituationDrop, SituationAdd:
			state = "swap_required"
		}
		var fromClaimID, toClaimID *int64
		if diff.FromClaim != nil {
			id := diff.FromClaim.ID
			fromClaimID = &id
		}
		if diff.ToClaim != nil {
			id := diff.ToClaim.ID
			toClaimID = &id
		}
		nodeTasks[i] = processes.NodeTaskInput{
			ProcessID:    processID,
			CoreNodeName: diff.CoreNodeName,
			FromClaimID:  fromClaimID,
			ToClaimID:    toClaimID,
			Situation:    string(diff.Situation),
			State:        state,
		}
	}

	return &changeoverPlan{
		process:    process,
		style:      style,
		stations:   stations,
		stationIDs: stationIDs,
		diffs:      diffs,
		nodes:      nodes,
		nodeTasks:  nodeTasks,
	}, nil
}

// applyChangeoverDiffPostProcessors is the changeover diff pipeline.
// It composes every post-processor that mutates the raw diff list
// before node-task creation, plus the Core-availability check that
// gates the press-index fan-out.
//
// Order matters and is enforced here:
//
//  1. Reuse-compatible-bins shortcut. Rewrites Swap/Evacuate to
//     Unchanged when a press-index claim opts into reuse_compatible_bins,
//     payloads match, and the physical bin is empty. Runs first so
//     downstream post-processors see the reduced diff list.
//
//  2. Press-index Core-availability gate. Refuses the changeover when
//     Core is unavailable and any remaining diff is a press-index
//     swap/evacuate — without Core's bin-type catalog the per-position
//     fan-out silently degrades to "same bin type" and would route a
//     real different-bin-type changeover through the wrong choreography.
//
//  3. Same-mode different-bin-type fan-out. Expands a press-index
//     parent diff into per-position diffs when the from- and to-claim
//     payloads have different bin type codes.
//
//  4. Cross-mode / extension-position fan-out. Picks up press-index
//     extension positions (PairedCoreNode / SecondPairedCoreNode)
//     that the same-mode pass left alone — cross-mode changeovers
//     (one side press-index, the other not) and same-mode same-bin-
//     type position-count deltas. Acts only on positions not already
//     covered, so step 3 always wins for positions it touched.
//
// Add new diff post-processors to this function so the ordering
// invariants stay in one place. Any new processor needs to declare
// where it sits in the pipeline relative to the existing four.
func (e *Engine) applyChangeoverDiffPostProcessors(processID int64, diffs []ChangeoverNodeDiff) ([]ChangeoverNodeDiff, error) {
	diffs = ApplyReuseCompatibleBinsShortcut(diffs, e.binEmptyAtCoreNode(processID))
	if err := e.refusePressIndexWhenCoreUnavailable(diffs); err != nil {
		return nil, err
	}
	binTypes := e.binTypeSnapshot(diffs)
	diffs = FanOutPressIndexDifferentBinType(diffs, binTypes)
	diffs = FanOutPressIndexCrossMode(diffs)
	return diffs, nil
}

// refusePressIndexWhenCoreUnavailable returns an operator-readable error
// when Core is unavailable AND any diff is a press-index Swap/Evacuate.
// Without Core's bin-type catalog, the per-position fan-out can't
// detect different-bin-type changeovers and silently falls back to the
// same-bin-type choreography (wrong robots, wrong steps).
func (e *Engine) refusePressIndexWhenCoreUnavailable(diffs []ChangeoverNodeDiff) error {
	if e.coreClient != nil && e.coreClient.Available() {
		return nil
	}
	for _, d := range diffs {
		if (d.Situation != SituationSwap && d.Situation != SituationEvacuate) ||
			d.FromClaim == nil || d.FromClaim.SwapMode != protocol.SwapModeTwoRobotPressIndex {
			continue
		}
		return fmt.Errorf("changeover refused: Core unavailable; cannot determine bin types for press-index changeover at %s", d.CoreNodeName)
	}
	return nil
}

// PreviewChangeoverPlan returns the order plan that StartProcessChangeover would
// execute, without writing anything. Used by the operator dry-run UI so the
// floor can see exactly which orders will fire on each node before committing.
//
// Validation errors (active changeover already running, wrong style, etc.) are
// returned verbatim — the operator should see the same gating reason a Start
// would surface.
func (e *Engine) PreviewChangeoverPlan(processID, toStyleID int64) (changeover.Plan, error) {
	plan, err := e.planChangeover(processID, toStyleID)
	if err != nil {
		return changeover.Plan{}, err
	}
	return BuildChangeoverPlan(plan.diffs, plan.nodes, e.cfg.Web.AutoConfirm, e.activePullSnapshot(plan.nodes)), nil
}

// binTypeSnapshot pre-resolves canonical bin type codes for every
// payload referenced by from- or to-claims in the diff. Used by the
// press-index different-bin-type fan-out to detect bin-type changes
// between styles. The lookup goes through the Core PayloadManifest
// endpoint, so a single call per unique payload covers the plan.
// Failures leave the payload out of the map; the comparator treats
// missing entries as "unknown" and falls back to same-bin-type
// behaviour. The Core-availability gate refuses press-index
// changeover starts when this lookup can't resolve, so an empty map
// here for press-index claims is impossible at this point.
func (e *Engine) binTypeSnapshot(diffs []ChangeoverNodeDiff) map[string]string {
	out := make(map[string]string)
	if e.coreClient == nil || !e.coreClient.Available() {
		return out
	}
	collect := func(claim *processes.NodeClaim) {
		if claim == nil || claim.PayloadCode == "" {
			return
		}
		if _, seen := out[claim.PayloadCode]; seen {
			return
		}
		// Mark as "looked up but unknown" so we don't retry on the
		// fallthrough path; an empty value means "no signal".
		out[claim.PayloadCode] = ""
		resp, err := e.coreClient.FetchPayloadManifest(claim.PayloadCode)
		if err != nil || resp == nil {
			return
		}
		if resp.BinTypeCode != "" {
			out[claim.PayloadCode] = resp.BinTypeCode
		}
	}
	for i := range diffs {
		collect(diffs[i].FromClaim)
		collect(diffs[i].ToClaim)
	}
	return out
}

// activePullSnapshot returns a CoreNodeName → bool map from the runtime
// rows of every process node passed in. Used by the planner so
// sequential changeover can decide which physical side is "active"
// (line currently pulling from it) vs "inactive". Nodes whose runtime
// row is missing or unreadable default to false; the planner's
// resolveSequentialActivePull tie-break handles the both-false case.
func (e *Engine) activePullSnapshot(nodes []processes.Node) map[string]bool {
	out := make(map[string]bool, len(nodes))
	for i := range nodes {
		rt, err := e.db.GetProcessNodeRuntime(nodes[i].ID)
		if err != nil || rt == nil {
			continue
		}
		out[nodes[i].CoreNodeName] = rt.ActivePull
	}
	return out
}

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

// findNodeByCoreName finds a process node by its CoreNodeName.
func findNodeByCoreName(nodes []processes.Node, coreName string) *processes.Node {
	for i := range nodes {
		if nodes[i].CoreNodeName == coreName {
			return &nodes[i]
		}
	}
	return nil
}

// ReleaseChangeoverWaitResult reports the outcome of a release-wait click so
// the frontend can show the operator how much actually happened. Released is
// the count of legs whose OrderRelease envelopes were queued this call;
// Pending is the count of legs that exist but weren't in staged status yet
// (still sourcing / in_transit / etc.) and so were silently skipped — those
// are the legs the operator may need to come back for on a second click.
// Already-terminal legs (released earlier, cancelled, failed) are not
// counted in either field.
type ReleaseChangeoverWaitResult struct {
	Released int `json:"released"`
	Pending  int `json:"pending"`
}

// ReleaseChangeoverWait releases all evacuation orders that are currently staged
// (waiting at a wait step). Called once per operator gate:
//   - First call releases the "ready" wait on all nodes
//   - For evacuate nodes, orders stage again at the second wait, and the second
//     call releases "tooling done"
//
// Per-slot disposition: each task carries up to two staged orders — the evac
// leg (OldMaterialReleaseOrderID) and the supply leg (NextMaterialOrderID).
// They get DIFFERENT dispositions:
//
//   - Evac leg: auto-detected per task from the line's runtime cache. If the
//     line still has parts (RemainingUOPCached > 0), the evac is sent as
//     send_partial_back with that exact count — Core syncs the bin's
//     manifest to the partial value at release time, and the bin arrives at
//     the supermarket flagged as partial with the right qty. If the line is
//     empty (RemainingUOPCached == 0), the evac is release_empty — manifest
//     cleared, preserving the 2026-04 ALN_001 fix intent (bin can't land at
//     OutboundDestination tagged with stale payload). The operator never
//     types a number; the system already knows it.
//
//     The caller's disposition (passed in `disp`) acts as an override: if
//     they supplied Mode=send_partial_back with a PartialCount, that count
//     wins over the runtime auto-detect. Useful for future flows where an
//     operator manually overrides the cached value, but the default path
//     (no modal, just a click) bypasses operator entry entirely.
//
//   - Supply leg: receives Mode="" (zero-value) regardless of anything else.
//     buildProtocolDisposition translates this to nil on the wire, and
//     Core's SyncOrClearForReleased hits the no-op branch — the supply
//     bin's manifest is left alone. The supply bin is mid-transit from the
//     supermarket carrying its real uop_remaining; applying any evac-leg
//     disposition would zero a manifest that should ride through to
//     delivery. (Confirmed regression on plant order 682 / 2026-05-06.)
//
// TODO: expand to per-bin disposition flow when a plant scenario needs
// it (e.g., operator override of the runtime count, or different
// dispositions per evac bin). Engine is already neutral; this is a
// frontend + handler-shape change.
//
// disp.CalledBy is plumbed through for audit on both legs.
//
// F' Phase 2 — evac-first sequencing for paired tasks.
//
// When a task has both an evac leg (OldMaterialReleaseOrderID) and a
// supply leg (NextMaterialOrderID), only the evac fires at click time.
// The supply leg auto-releases mid-evac, when the evac robot finishes
// picking up the bin and starts moving away from the slot. This is
// NOT when the evac order is fully complete (drop at outbound is later);
// it's when the pickup block within the evac order transitions to
// FINISHED, which is precisely the moment the slot is physically clear
// for the supply robot. Core's RDS poller emits the per-block FINISHED
// transition and publishes BinPickedUp; handler_bin_picked_up.go's
// HandleBinPickedUp looks up the paired supply order via
// GetChangeoverNodeTaskByEvacOrderID (NOT SiblingOrderID — that's used
// by operator-station two-robot paths and is intentionally untouched
// here) and calls releaseUnlessTerminal on it. This eliminates the
// crash-race window where the supply robot could arrive at the slot
// before the evac robot has cleared it.
//
// Pre-Phase-2 behaviour: both legs fired together, gated on
// Status==Staged. If the operator clicked the changeover-wide release
// before R1 was at its wait point, the staged-only switch made the
// click a no-op — but flipping to "release any non-terminal" without
// the evac-first defer would race R2 to the slot. Phase 2 collapses
// the staged-only switch (Friday-incident fix) AND adds the defer
// (the safer architecture the collapse demands).
//
// Result.Pending: includes both deferred-supply legs (non-terminal,
// will fire on evac pickup) and any standalone-leg orders we skipped
// because they weren't in a releasable state at click time.
func (e *Engine) ReleaseChangeoverWait(processID int64, disp ReleaseDisposition) (ReleaseChangeoverWaitResult, error) {
	var result ReleaseChangeoverWaitResult

	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return result, err
	}
	tasks, err := e.db.ListChangeoverNodeTasks(changeover.ID)
	if err != nil {
		return result, err
	}

	// Supply leg always rides through with no manifest action regardless of
	// what the operator chose. Empty Mode → buildProtocolDisposition returns
	// nil → Core no-op. CalledBy still flows for audit.
	supplyDisp := ReleaseDisposition{CalledBy: disp.CalledBy}

	// Collect per-task failures rather than swallowing them. Pre-fix
	// behaviour was log-and-continue + return nil, which silently recreated
	// the original ALN_001 incident on partial failure: one node's manifest
	// stays stale, the operator gets a 200 OK, and the bin loader can't
	// move that bin. Returning errors.Join ensures the handler surfaces
	// the failed node names instead of lying about success.
	var failures []error
	for _, task := range tasks {
		if task.Situation == "unchanged" {
			continue
		}
		// Auto-detect evac disposition from the line's runtime cache for
		// THIS task's node. Operator override (caller-supplied
		// SendPartialBack with a count) wins if present.
		evacDisp := evacDispositionForTask(e, task, disp)

		hasEvac := task.OldMaterialReleaseOrderID != nil
		hasSupply := task.NextMaterialOrderID != nil
		pairedEvacSupply := hasEvac && hasSupply

		type slot struct {
			id   *int64
			disp ReleaseDisposition
			kind string // for log/error context only
		}
		var slots []slot
		if hasEvac {
			slots = append(slots, slot{id: task.OldMaterialReleaseOrderID, disp: evacDisp, kind: "evac"})
		}
		// Supply leg fires at click time ONLY when there's no paired evac
		// (e.g., add-situation tasks). When paired with evac, we defer to
		// HandleBinPickedUp which fires the sibling release on evac pickup
		// confirm — see Phase 2 docstring above.
		if hasSupply && !pairedEvacSupply {
			slots = append(slots, slot{id: task.NextMaterialOrderID, disp: supplyDisp, kind: "supply"})
		}

		for _, s := range slots {
			if s.id == nil {
				continue
			}
			order, err := e.db.GetOrder(*s.id)
			if err != nil {
				log.Printf("release changeover wait node %s (%s): get order: %v", task.NodeName, s.kind, err)
				failures = append(failures, fmt.Errorf("node %s (%s): get order: %w", task.NodeName, s.kind, err))
				continue
			}
			if orders.IsTerminal(order.Status) {
				// Already released earlier, cancelled, or failed. No
				// operator action required.
				continue
			}
			if err := e.ReleaseOrderWithLineside(order.ID, s.disp); err != nil {
				log.Printf("release changeover wait node %s (%s): %v", task.NodeName, s.kind, err)
				failures = append(failures, fmt.Errorf("node %s (%s): %w", task.NodeName, s.kind, err))
				continue
			}
			result.Released++
		}

		// Count deferred supply legs (paired-with-evac) so the operator
		// HMI can show "released N, M deferred for pickup-confirm." Skip
		// counting if the supply is already terminal.
		if pairedEvacSupply {
			supply, err := e.db.GetOrder(*task.NextMaterialOrderID)
			if err == nil && !orders.IsTerminal(supply.Status) {
				result.Pending++
			}
		}
	}
	return result, errors.Join(failures...)
}

// evacDispositionForTask picks the right evac-leg disposition. Operator
// override wins; otherwise auto-detect from the node's runtime cache.
//
//   - Caller passed Mode=send_partial_back with PartialCount > 0 → use it.
//   - Caller passed any other non-empty Mode → use it as-is (escape hatch
//     for future flows).
//   - Caller passed Mode="" → look up node runtime. If RemainingUOPCached >
//     0, send_partial_back with that count; else release_empty
//     (capture_lineside + empty captures → wire-form release_empty;
//     preserves the ALN_001 fix).
//
// On runtime lookup failure: fall back to release_empty rather than
// failing the whole release. The whole point of the manifest clear is to
// prevent stale payload at OutboundDestination — better to clear than to
// silently no-op when we can't read the current count.
func evacDispositionForTask(e *Engine, task processes.NodeTask, override ReleaseDisposition) ReleaseDisposition {
	if override.Mode != "" {
		return override
	}

	runtime, err := e.db.GetProcessNodeRuntime(task.ProcessNodeID)
	if err != nil {
		log.Printf("release changeover wait node %s: runtime lookup failed (%v); defaulting evac to release_empty", task.NodeName, err)
		return ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: override.CalledBy}
	}

	if runtime != nil && runtime.RemainingUOPCached > 0 {
		count := runtime.RemainingUOPCached
		return ReleaseDisposition{
			Mode:         DispositionSendPartialBack,
			PartialCount: &count,
			CalledBy:     override.CalledBy,
		}
	}
	return ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: override.CalledBy}
}

// isPendingOrderStatus reports whether the order is alive but not yet at
// staged — i.e., it's expected to reach staged on its own and the operator
// would benefit from being told to wait. Conservative: anything that isn't
// staged AND isn't past-staged counts as pending. (Note: a "released" order
// transitions to StatusInTransit per orders.Manager — there's no separate
// StatusReleased to filter out.)
func isPendingOrderStatus(s protocol.Status) bool {
	switch s {
	case orders.StatusInTransit, orders.StatusDelivered, orders.StatusConfirmed,
		orders.StatusCancelled, orders.StatusFailed:
		return false
	case orders.StatusStaged:
		return false
	default:
		return true
	}
}

// SequentialChangeoverCutover is the per-node operator action that gates
// the active-side swap during a sequential SWAP changeover.
//
// Sequential SWAP ships a single complex order with a mid-sequence wait
// at the active position. The robot has finished swapping the inactive
// side and is parked at the active position. The operator clicks
// "cutover" to:
//
//  1. Flip ActivePull to the previously-inactive (now freshly-stocked)
//     side. The line starts pulling from the new bin immediately.
//  2. Release the wait inside the running complex order so the robot
//     proceeds to evac the now-inactive side and deliver the new bin.
//
// Order matters: flip BEFORE release. If the wait released first, the
// robot could begin pickup at a position the line is still pulling
// from. Atomic from the operator's POV (one HTTP call, server-side
// sequence is internal).
//
// nodeID is the changeover task's primary process node (CoreNodeName).
// The cutover handler re-reads ActivePull at the moment of the click to
// find which physical side is inactive — the planner-time resolution is
// not persisted, but ActivePull doesn't change between plan and cutover
// (the changeover itself doesn't flip; only this handler does).
func (e *Engine) SequentialChangeoverCutover(processID, nodeID int64, calledBy string) error {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return fmt.Errorf("sequential cutover: no active changeover for process %d: %w", processID, err)
	}
	task, err := e.db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		return fmt.Errorf("sequential cutover: get node task: %w", err)
	}
	if task.Situation != "swap" {
		return fmt.Errorf("sequential cutover: node task situation is %q, not swap", task.Situation)
	}
	if task.FromClaimID == nil {
		return fmt.Errorf("sequential cutover: node task has no from-claim id")
	}
	fromClaim, err := e.db.GetStyleNodeClaim(*task.FromClaimID)
	if err != nil || fromClaim == nil {
		return fmt.Errorf("sequential cutover: get from-claim: %w", err)
	}
	if fromClaim.SwapMode != protocol.SwapModeSequential {
		return fmt.Errorf("sequential cutover: from-claim swap_mode is %q, not sequential", fromClaim.SwapMode)
	}
	if fromClaim.PairedCoreNode == "" {
		return fmt.Errorf("sequential cutover: from-claim has no paired_core_node")
	}
	if task.NextMaterialOrderID == nil {
		return fmt.Errorf("sequential cutover: node task has no tracked complex order")
	}

	// Resolve inactive/active using the same logic the planner ran. The
	// inactive-node CoreNodeName names the physical node we're flipping
	// pull TO (it's been freshly stocked by the pre-cutover steps).
	processNode, err := e.db.GetProcessNode(task.ProcessNodeID)
	if err != nil {
		return fmt.Errorf("sequential cutover: get process node: %w", err)
	}
	nodes, err := e.db.ListProcessNodesByProcess(processNode.ProcessID)
	if err != nil {
		return fmt.Errorf("sequential cutover: list process nodes: %w", err)
	}
	activePull := e.activePullSnapshot(nodes)
	inactive, _ := resolveSequentialActivePull(fromClaim, activePull)
	if inactive == "" {
		return fmt.Errorf("sequential cutover: could not resolve inactive node from active-pull snapshot")
	}
	var inactivePhysical *processes.Node
	for i := range nodes {
		if nodes[i].CoreNodeName == inactive {
			inactivePhysical = &nodes[i]
			break
		}
	}
	if inactivePhysical == nil {
		return fmt.Errorf("sequential cutover: inactive node %q not found in process %d", inactive, processNode.ProcessID)
	}

	// 1. Flip first (so when the robot wakes, the line is already pulling
	// from the freshly-stocked side and the robot can safely evac the
	// now-stale active side).
	if err := e.FlipABNode(inactivePhysical.ID); err != nil {
		return fmt.Errorf("sequential cutover: flip active-pull to %s: %w", inactive, err)
	}

	// 2. Release the wait. The complex order's mid-sequence wait is at
	// the active position; releasing it lets the robot proceed.
	disp := ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: calledBy}
	if err := e.ReleaseOrderWithLineside(*task.NextMaterialOrderID, disp); err != nil {
		return fmt.Errorf("sequential cutover: release wait on order %d: %w", *task.NextMaterialOrderID, err)
	}
	log.Printf("sequential changeover: cutover at node %s (process=%d task=%d) — flipped pull to %s, released order %d",
		task.NodeName, processID, task.ID, inactive, *task.NextMaterialOrderID)
	return nil
}

// canCompleteChangeover reports whether a changeover row may transition to
// "completed". Both checks are required:
//
//  1. Every changeover_node_tasks row must be in a terminal state (per
//     domain.IsNodeTaskStateTerminal).
//  2. Every order referenced by a node task (NextMaterialOrderID,
//     OldMaterialReleaseOrderID) must be in a terminal status (per
//     protocol.IsTerminal).
//
// Pinning both checks keeps the gate honest if either state machine
// drifts independently of the other. Returns (false, reasons, nil) when
// blocked, with one human-readable line per blocker — the HMI handler
// surfaces these so operators see "task at node ALN_002 in
// staging_requested; order 703 in in_transit" rather than a generic 500.
func (e *Engine) canCompleteChangeover(changeoverID int64) (bool, []string, error) {
	tasks, err := e.db.ListChangeoverNodeTasks(changeoverID)
	if err != nil {
		return false, nil, err
	}
	var reasons []string
	for _, task := range tasks {
		if !domain.IsNodeTaskStateTerminal(task.State, task.Situation) {
			reasons = append(reasons, fmt.Sprintf("task at node %s in %s", task.NodeName, task.State))
		}
	}
	seen := map[int64]struct{}{}
	for _, task := range tasks {
		for _, orderID := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
			if orderID == nil {
				continue
			}
			if _, dup := seen[*orderID]; dup {
				continue
			}
			seen[*orderID] = struct{}{}
			order, err := e.db.GetOrder(*orderID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				return false, nil, err
			}
			if !protocol.IsTerminal(order.Status) {
				reasons = append(reasons, fmt.Sprintf("order %d in %s", order.ID, order.Status))
			}
		}
	}
	if len(reasons) > 0 {
		return false, reasons, nil
	}
	return true, nil, nil
}

// CompleteProcessProductionCutover runs the operator-driven cutover:
// gate → flip active style → finalize. Trigger source is recorded as
// "operator-hmi" on the changeover row.
func (e *Engine) CompleteProcessProductionCutover(processID int64) error {
	return e.completeCutover(processID, "operator-hmi")
}

// CompleteProcessProductionCutoverFromPLC is the entry point used by
// the PLC-driven cutover monitor. Identical to the operator-driven
// path except the changeover row records "plc-auto" as the trigger
// source for audit/postmortem.
func (e *Engine) CompleteProcessProductionCutoverFromPLC(processID int64) error {
	return e.completeCutover(processID, "plc-auto")
}

func (e *Engine) completeCutover(processID int64, triggeredBy string) error {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return err
	}
	// Gate must run before any of the five mutations below. The function
	// flips active_style_id (line below) before writing the completed row;
	// inserting the gate after the flip would leave the system on the
	// to-style with an still-in-progress changeover row if the gate
	// blocked. findActiveClaim resolves from process.ActiveStyleID, so
	// that order is unrecoverable without operator intervention.
	if ok, reasons, err := e.canCompleteChangeover(changeover.ID); err != nil || !ok {
		if err != nil {
			return err
		}
		return fmt.Errorf("cannot cutover: %s", strings.Join(reasons, "; "))
	}
	toStyleID := changeover.ToStyleID
	if err := e.db.SetActiveStyle(processID, &toStyleID); err != nil {
		return err
	}
	return e.finalizeChangeoverRow(processID, changeover.ID, triggeredBy)
}

// finalizeChangeoverRow runs the post-gate, post-flip steps shared by
// CompleteProcessProductionCutover and tryCompleteProcessChangeover:
// clear target style, mark production active, sync the counter
// reporting-point's style_id, and write the completed row.
//
// Step order is load-bearing: restoreChangeoverState reads
// (active_style, changeover.state) jointly during crash recovery and the
// invariant it relies on is "active_style flipped ⇒ changeover writeable
// to completed." Reordering would break that recovery contract.
//
// SyncProcessCounter is included here so the auto-completion path
// (tryCompleteProcessChangeover) keeps the reporting point's style_id
// in sync — without this, a PLC- or event-driven cutover via the auto
// path would land with the reporting point still pointing at the
// from-style.
func (e *Engine) finalizeChangeoverRow(processID, changeoverID int64, triggeredBy string) error {
	if err := e.db.SetTargetStyle(processID, nil); err != nil {
		return err
	}
	if err := e.db.SetProcessProductionState(processID, "active_production"); err != nil {
		return err
	}
	if err := e.SyncProcessCounter(processID); err != nil {
		return err
	}
	return e.db.UpdateProcessChangeoverStateWithTrigger(changeoverID, "completed", triggeredBy)
}

func (e *Engine) CancelProcessChangeover(processID int64) error {
	return e.cancelProcessChangeoverInternal(processID, nil)
}

// CancelProcessChangeoverRedirect cancels the active changeover and immediately
// starts a new one to a different target style. If nextStyleID is nil, behaves
// identically to CancelProcessChangeover (plain revert).
func (e *Engine) CancelProcessChangeoverRedirect(processID int64, nextStyleID *int64) error {
	return e.cancelProcessChangeoverInternal(processID, nextStyleID)
}

func (e *Engine) cancelProcessChangeoverInternal(processID int64, nextStyleID *int64) error {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return err
	}

	// Abort all in-flight orders linked to this changeover's node tasks.
	// Core will handle safe resolution (convert loaded robots to store orders).
	nodeTasks, _ := e.db.ListChangeoverNodeTasks(changeover.ID)
	for _, task := range nodeTasks {
		for _, orderID := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
			if orderID == nil {
				continue
			}
			order, err := e.db.GetOrder(*orderID)
			if err != nil {
				continue
			}
			if orders.IsTerminal(order.Status) {
				continue
			}
			if err := e.orderMgr.AbortOrder(order.ID); err != nil {
				log.Printf("changeover cancel: abort order %s: %v", order.UUID, err)
			}
		}
		// Mark node task as cancelled
		if err := e.db.UpdateChangeoverNodeTaskState(task.ID, "cancelled"); err != nil {
			log.Printf("changeover: update node task %d state to cancelled: %v", task.ID, err)
		}
	}

	// Clear runtime order references for affected nodes
	for _, task := range nodeTasks {
		runtime, err := e.db.GetProcessNodeRuntime(task.ProcessNodeID)
		if err != nil || runtime == nil {
			continue
		}
		if err := e.db.UpdateProcessNodeRuntimeOrders(task.ProcessNodeID, nil, nil); err != nil {
			log.Printf("changeover: update runtime orders for node %d: %v", task.ProcessNodeID, err)
		}
	}

	if err := e.db.UpdateProcessChangeoverState(changeover.ID, "cancelled"); err != nil {
		return err
	}
	if err := e.db.SetTargetStyle(processID, nil); err != nil {
		return err
	}
	if err := e.db.SetProcessProductionState(processID, "active_production"); err != nil {
		return err
	}

	// Redirect — start new changeover immediately to a different target style
	if nextStyleID != nil && *nextStyleID != 0 {
		_, err := e.StartProcessChangeover(processID, *nextStyleID,
			"changeover-redirect", "redirected from cancelled changeover")
		return err
	}

	return nil
}

func (e *Engine) tryCompleteProcessChangeover(processID int64) error {
	process, err := e.db.GetProcess(processID)
	if err != nil {
		return err
	}
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return nil
	}
	if process.ActiveStyleID == nil || *process.ActiveStyleID != changeover.ToStyleID {
		return nil
	}
	// Gate before the station-task force-switch. Today's auto-completion
	// path checked node-task terminality only; the broader gate also
	// requires linked orders to be terminal so a late-arriving order
	// completion doesn't leave a node task stranded after the row is
	// closed.
	ok, _, err := e.canCompleteChangeover(changeover.ID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	tasks, err := e.db.ListChangeoverStationTasks(changeover.ID)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if err := e.db.UpdateChangeoverStationTaskState(task.ID, "switched"); err != nil {
			log.Printf("changeover: update station task state: %v", err)
		}
	}
	return e.finalizeChangeoverRow(processID, changeover.ID, "auto-task-terminal")
}

func isNodeTaskTerminal(task *processes.NodeTask) bool {
	return domain.IsNodeTaskStateTerminal(task.State, task.Situation)
}

func ensureNodeTaskCanRequestOrder(orderID *int64, action string, db *store.DB) error {
	if orderID == nil {
		return nil
	}
	order, err := db.GetOrder(*orderID)
	if err != nil {
		return fmt.Errorf("%s already requested and order lookup failed: %w", action, err)
	}
	if !orders.IsTerminal(order.Status) {
		return fmt.Errorf("%s already requested with active order %s", action, order.UUID)
	}
	return nil
}
