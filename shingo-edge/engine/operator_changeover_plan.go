// operator_changeover_plan.go — read-only changeover planning.
//
// Builds the changeoverPlan struct (consumed by StartProcessChangeover),
// runs the diff post-processor pipeline, and answers preview queries.
// Nothing in this file mutates DB state.

package engine

import (
	"database/sql"
	"fmt"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/engine/changeover"
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
	// participants is the node set the changeover physically touches —
	// superset of nodeTasks, frozen here at plan time.
	participants []domain.ParticipantInput
	// unresolvedParticipants names participants with no process_nodes row.
	// Advisory; see domain.Changeover.UnresolvedParticipants.
	unresolvedParticipants []string
	// tooling is the marked-press decoration, computed from the ORIGINAL claim
	// lists rather than from the diffs. See changeover_tooling.go.
	tooling toolingChangeover
}

// planChangeover assembles all data needed for a changeover without writing anything.
// Returns an error if the changeover request is invalid (wrong style, already active, etc).
//
// Note: validation errors use changeover-specific messages ("process is already running
// style %d", etc). If this is later reused for a dry-run API, the error messages will
// still be appropriate — but callers should be aware they're changeover-flavored.
// logEvacConfigOnWrongSide names the case the ownership decision changes: the
// INCOMING claim asks for evacuation and the outgoing one does not, so this
// transition will not evacuate where the old read would have.
//
// A DIAGNOSTIC, NOT A FALLBACK. Honouring it would be a second ownership rule,
// and the point of the decision is that there is one. Saying it is what turns
// "the config is on the wrong claim" from a silent behaviour change into a
// line an engineer can act on.
func (e *Engine) logEvacConfigOnWrongSide(fromClaims, toClaims []processes.NodeClaim) {
	toByNode := make(map[string]*processes.NodeClaim, len(toClaims))
	for i := range toClaims {
		toByNode[toClaims[i].CoreNodeName] = &toClaims[i]
	}
	for i := range fromClaims {
		from := &fromClaims[i]
		to := toByNode[from.CoreNodeName]
		if domain.EvacConfigOnWrongSide(from, to) {
			e.logFn("changeover: node %s — 'Evacuate on changeover' is set on the INCOMING style's "+
				"claim but not the outgoing one, and the outgoing claim is what decides. This "+
				"transition will NOT evacuate. Move the setting to the outgoing style's claim.",
				from.CoreNodeName)
		}
	}
}

// planChangeover builds everything a changeover needs before anything is
// written: the diffs, the node tasks, the participants and the tooling
// decoration.
//
// materializePositions is the difference between the two callers. A marked press
// position owns no style_node_claims row and therefore no process_nodes row until
// something creates one, and the thing that used to create one was
// ChangeoverService.Create — which runs AFTER this. So the FIRST changeover of
// any marked press planned one position fewer than the second, silently: no
// clearance, no hold, no order (N1-a, sim-proven on a pristine seed 2026-08-24).
//
// Start passes true and the rows are written here, before planning, so the
// first changeover plans exactly like the second. Preview passes false and gets
// the same positions as UNSAVED nodes — the operator must see the work the
// changeover will do, and a preview that writes rows is not a preview.
func (e *Engine) planChangeover(processID, toStyleID int64, materializePositions bool) (*changeoverPlan, error) {
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
	// The tooling changeover parks the incoming style's bins at InboundStaging
	// until tooling-done. Refuse to arm without one, LOUDLY and by name: the
	// alternative is a plan whose supply legs have nowhere to go, discovered as
	// robots idling mid-changeover — or worse, material driving into a cell a
	// human is standing in.
	//
	// Reads the CLAIMS, not the diffs, and so runs before them: the question is
	// about the operator's configuration, which no diff pass can change.
	if err := refuseToolingChangeoverWithoutStaging(fromClaims, toClaims); err != nil {
		return nil, err
	}
	// The tooling decoration, also from the original claims. Applied last, by
	// BuildChangeoverPlan, over the finished plan.
	tooling := planToolingChangeover(fromClaims, toClaims)
	diffs, err := e.applyChangeoverDiffPostProcessors(processID, DiffStyleClaims(fromClaims, toClaims))
	if err != nil {
		return nil, err
	}
	e.logEvacConfigOnWrongSide(fromClaims, toClaims)
	nodes, err := e.db.ListProcessNodesByProcess(processID)
	if err != nil {
		return nil, err
	}
	nodes, err = e.resolveToolingNodes(processID, tooling, nodes, materializePositions)
	if err != nil {
		return nil, err
	}

	stationIDs := make([]int64, len(stations))
	for i := range stations {
		stationIDs[i] = stations[i].ID
	}

	nodeTasks := make([]processes.NodeTaskInput, 0, len(diffs))
	for _, diff := range diffs {
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
		nodeTasks = append(nodeTasks, processes.NodeTaskInput{
			ProcessID:    processID,
			CoreNodeName: diff.CoreNodeName,
			FromClaimID:  fromClaimID,
			ToClaimID:    toClaimID,
			Situation:    string(diff.Situation),
			State:        state,
		})
	}
	nodeTasks = appendToolingClearanceTasks(processID, tooling, nodeTasks)

	participants := buildParticipants(diffs)
	unresolved := assertParticipantsResolve(participants, nodes)

	return &changeoverPlan{
		process:    process,
		style:      style,
		stations:   stations,
		stationIDs: stationIDs,
		diffs:      diffs,
		nodes:      nodes,
		nodeTasks:  nodeTasks,

		participants: participants,

		unresolvedParticipants: unresolved,
		tooling:                tooling,
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
//
// TOOLING IS NOT IN THIS LIST, and that is its declaration: it is not a
// diff post-processor at all. It runs LAST, over the FINISHED PLAN, and
// edits the legs these four produced (see changeover_tooling.go). It was
// a fifth entry here once — FanOutStagedToolingEvacuation — and step 3
// silently disqualified it by rewriting the SwapMode its predicate read,
// so a press that was both marked for tooling and changing bin type got
// no tooling at all. A pass that cannot be reached by another pass's
// output does not need a slot in this ordering.
func (e *Engine) applyChangeoverDiffPostProcessors(processID int64, diffs []ChangeoverNodeDiff) ([]ChangeoverNodeDiff, error) {
	diffs = ApplyReuseCompatibleBinsShortcut(diffs, e.binEmptyAtCoreNode(processID))
	if err := e.refusePressIndexWhenCoreUnavailable(diffs); err != nil {
		return nil, err
	}
	binTypes := e.binTypeSnapshot(diffs)
	diffs = FanOutPressIndexDifferentBinType(diffs, binTypes)
	diffs = FanOutPressIndexCrossMode(diffs, binTypes)
	return diffs, nil
}

// refusePressIndexWhenCoreUnavailable returns an operator-readable error
// when Core is unavailable AND any diff touches a press-index claim.
// Without Core's bin-type catalog, both fan-out passes can't detect
// different-bin-type changeovers and silently fall back to the
// same-bin-type choreography (wrong robots, wrong steps) or to no
// fan-out at all (bins stranded on the press with no order and no error).
//
// Scoped to any non-Unchanged situation on EITHER side, not just
// Swap/Evacuate on the from-side: the cross-mode pass reads bin types for
// extension positions carried on Drop and Add diffs too. HK #27 produced
// ONLY Drop and Add, so the old Swap/Evacuate-scoped gate would have
// waved through exactly the changeover that needed it.
func (e *Engine) refusePressIndexWhenCoreUnavailable(diffs []ChangeoverNodeDiff) error {
	if e.coreClient != nil && e.coreClient.Available() {
		return nil
	}
	for _, d := range diffs {
		if d.Situation == SituationUnchanged {
			continue
		}
		if !isPressIndexClaim(d.FromClaim) && !isPressIndexClaim(d.ToClaim) {
			continue
		}
		return fmt.Errorf("changeover refused: Core unavailable; cannot determine bin types for press-index changeover at %s", d.CoreNodeName)
	}
	return nil
}

// isPressIndexClaim reports whether a claim is a two-robot press-index claim.
// nil-safe: a missing claim (Drop's to-side, Add's from-side) is not one.
func isPressIndexClaim(c *processes.NodeClaim) bool {
	return c != nil && c.SwapMode == protocol.SwapModeTwoRobotPressIndex
}

// PreviewChangeoverPlan returns the order plan that StartProcessChangeover would
// execute, without writing anything. Used by the operator dry-run UI so the
// floor can see exactly which orders will fire on each node before committing.
//
// Validation errors (active changeover already running, wrong style, etc.) are
// returned verbatim — the operator should see the same gating reason a Start
// would surface.
func (e *Engine) PreviewChangeoverPlan(processID, toStyleID int64) (changeover.Plan, error) {
	plan, err := e.planChangeover(processID, toStyleID, false)
	if err != nil {
		return changeover.Plan{}, err
	}
	return BuildChangeoverPlan(plan.diffs, plan.nodes, e.cfg.Web.AutoConfirm, e.activePullSnapshot(plan.nodes), plan.tooling), nil
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

// buildParticipants derives the changeover's participant set from the
// POST-FAN-OUT diffs: every diff node as a task-role participant, plus every
// press-index extension position that no diff already covers as indexed_over.
//
// TASK ROLE IS THE FULL DIFF SLICE, including SituationUnchanged. That is a
// strict widening and it is deliberate: unchanged diffs mint task rows today,
// so scoping participants to "changed only" would silently UNGATE a node the
// current gate blocks. Springfield-safe regardless — a loader that isn't in the
// changeover produces no diff at all, so it never becomes a participant and
// never gets gated (that regression is what forced the gate to be scoped in the
// first place).
//
// INDEXED_OVER is the set this table exists for: a press-index extension position
// is physically traversed by the index motion but mints no order and owns no
// task, so a task-keyed view cannot see it. Same-bin-type press-index
// changeovers never fan out (binTypesDiffer is false), so those positions appear
// ONLY here — and without them, intake gating leaves a position open to
// unrelated dispatch while a bin is about to be placed on it.
//
// Positions already covered by a diff (the different-bin-type case, where fan-out
// gave each position its own task) stay task-role; the UNIQUE constraint would
// reject the duplicate anyway, but skipping it keeps the roles honest rather
// than order-dependent.
func buildParticipants(diffs []ChangeoverNodeDiff) []domain.ParticipantInput {
	taskNodes := make(map[string]bool, len(diffs))
	out := make([]domain.ParticipantInput, 0, len(diffs))
	for _, d := range diffs {
		if d.CoreNodeName == "" || taskNodes[d.CoreNodeName] {
			continue
		}
		taskNodes[d.CoreNodeName] = true
		out = append(out, domain.ParticipantInput{
			CoreNodeName: d.CoreNodeName,
			Role:         domain.ParticipantRoleTask,
		})
	}

	seen := make(map[string]bool, len(taskNodes))
	for _, d := range diffs {
		for _, claim := range []*processes.NodeClaim{d.FromClaim, d.ToClaim} {
			if claim == nil || claim.SwapMode != protocol.SwapModeTwoRobotPressIndex {
				continue
			}
			for _, position := range pressIndexExtensionPositions(claim) {
				if taskNodes[position] || seen[position] {
					continue
				}
				seen[position] = true
				out = append(out, domain.ParticipantInput{
					CoreNodeName:       position,
					Role:               domain.ParticipantRoleIndexedOver,
					OwningTaskCoreNode: d.CoreNodeName,
				})
			}
		}
	}
	return out
}

// assertParticipantsResolve is the plan-time assertion: every participant's
// core_node_name must resolve to a process_nodes row. Returns the names that do
// not, in plan order.
//
// PURE DATA, no I/O — it reads the node list planChangeover already loaded, so
// it costs nothing and cannot fail for its own reasons. Sited in planChangeover
// after the fan-out post-processors, which means PreviewChangeoverPlan surfaces
// it PRE-COMMIT for free (Preview and Start share this function), rather than
// the operator discovering it only after the rows are written.
//
// Advisory by construction: it returns a list, it does not refuse. See
// domain.Changeover.UnresolvedParticipants for why hardening is gated.
//
// TASK-ROLE PARTICIPANTS ARE NOT REPORTED, because their rows are not missing
// for long: ChangeoverService.Create auto-creates a process_nodes row for every
// node task whose name does not resolve, in the same transaction, moments after
// this runs. Reporting them told the engineer to go and add a node the system
// had already added itself — a no-op errand attached to the one message the
// floor reads during a changeover. Every fanned-out press position is a task-role
// participant, so this was the common case, not the rare one.
//
// What is left is the population the advisory is actually about: indexed_over
// positions. Those own no task, Create does not create rows for them (it looks the
// id up and stores NULL when there is none), and without a row they cannot be
// rendered or gated. For those the advice is real.
//
// Scope note: this checks row EXISTENCE only. The companion check — that every
// task-role participant resolves a release station — needs stationForRelease,
// which ships with the affordance widening; without the affordance a named
// station would be advice the operator cannot act on.
func assertParticipantsResolve(participants []domain.ParticipantInput, nodes []processes.Node) []string {
	if len(participants) == 0 {
		return nil
	}
	known := make(map[string]bool, len(nodes))
	for i := range nodes {
		known[nodes[i].CoreNodeName] = true
	}
	var unresolved []string
	for _, p := range participants {
		if p.Role == domain.ParticipantRoleTask {
			continue
		}
		if p.CoreNodeName != "" && !known[p.CoreNodeName] {
			unresolved = append(unresolved, p.CoreNodeName)
		}
	}
	return unresolved
}
