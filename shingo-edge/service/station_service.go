package service

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"sync"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/lineside"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// claimsByCoreNode indexes a style's node claims by core node name — the
// batched form of GetStyleNodeClaimByNode for a whole board. Exact rather than
// approximate: style_node_claims declares UNIQUE(style_id, core_node_name), so
// there is at most one claim per (style, node) and the map cannot lose one.
func claimsByCoreNode(db *sql.DB, styleID int64) (map[string]processes.NodeClaim, error) {
	claims, err := processes.ListClaims(db, styleID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]processes.NodeClaim, len(claims))
	for _, c := range claims {
		out[c.CoreNodeName] = c
	}
	return out, nil
}

// LoaderResolver resolves the Core-owned loader aggregate a node belongs to, for
// the operator view. It is consumer-defined HERE (service sits below engine, so it
// cannot import the engine's LoaderStore); the engine injects its flag-selected
// LoaderStore via SetLoaderResolver. Routing the view through the SAME resolver the
// runtime uses is deliberate — it keeps "what loader is this node part of" a single
// source of truth (the B1 goal) instead of re-deriving it from the cache in the view.
//
// Contract: a clean miss returns (nil, nil) — the node is not a known aggregate
// loader, and BuildView keeps its legacy claim-derived fields. A non-nil error is a
// real failure; the view degrades to legacy rather than inventing a grouping.
type LoaderResolver interface {
	LoaderAt(coreNode domain.NodeID, role domain.LoaderRole) (*domain.Loader, error)
	// LoaderForNode resolves the loader (any role) a node belongs to, so the view
	// can treat a Core-owned loader's window/position as a manual_swap loader node
	// even when it has no per-style edge claim. (nil, nil) on a clean miss.
	LoaderForNode(coreNode domain.NodeID) (*domain.Loader, error)
}

// StationService owns operator-station CRUD and the two cross-aggregate
// orchestrations that span stations + processes + orders + lineside:
// SetNodes (sync process_nodes for a station) and BuildView (the
// operator HMI projection). Phase 6.1 introduced the cross-aggregate
// methods; Phase 6.2′ folded in the per-station CRUD that previously
// sat as named methods on *engine.Engine.
type StationService struct {
	db *store.DB
	// loaders resolves a node's parent loader aggregate for the operator view.
	// Optional: nil leaves the multi-window view fields empty (legacy behaviour),
	// so the lighter test constructors that don't wire it compile and pass
	// unchanged. The engine injects the live resolver via SetLoaderResolver.
	loaders LoaderResolver
	// stranded resolves a node's active parked-ticks alarm sentence (P2-C8) by
	// core node name. Optional: nil leaves StrandedAlarm empty. The engine injects
	// the live resolver (its strandedAlarms map) via SetStrandedResolver.
	stranded func(coreNodeName string) string

	// touched throttles the liveness write — see Touch.
	touchMu sync.Mutex
	touched map[int64]touchState
}

type touchState struct {
	status string
	at     time.Time
}

// touchThrottle is how stale last_seen_at is allowed to get while a board keeps
// polling. Well under any human's sense of "is that screen alive", and the
// column has no behavioural consumer — nothing reaps or alarms on it, it is
// surfaced for display only.
const touchThrottle = 15 * time.Second

// NewStationService constructs a StationService wrapping the shared
// *store.DB. The loader resolver is wired separately via SetLoaderResolver so
// existing call sites (and tests) that don't need multi-window view fields stay
// unchanged.
func NewStationService(db *store.DB) *StationService {
	return &StationService{db: db}
}

// SetLoaderResolver injects the loader-aggregate resolver used to populate the
// multi-window view fields (WindowGroupAnchor / WindowNodes). The engine calls
// this once at startup with its flag-selected LoaderStore.
func (s *StationService) SetLoaderResolver(r LoaderResolver) { s.loaders = r }

// SetStrandedResolver injects the parked-ticks alarm resolver (P2-C8) — the
// engine's StrandedAlarmDetail — so BuildView can render the tile chip. Optional;
// unset leaves StrandedAlarm empty for the lighter test constructors.
func (s *StationService) SetStrandedResolver(r func(coreNodeName string) string) { s.stranded = r }

// ── Cross-aggregate orchestrations ──────────────────────────────────

// SetNodes syncs process_nodes for a station to match the given core
// node names. Cross-aggregate: stations + processes + orders. Nodes
// with active orders are disabled rather than deleted to preserve
// referential integrity for downstream telemetry.
//
// Phase 6.1 introduced this method as a thin delegate; Phase 6.4a
// moved the body in from the (now-deleted) outer
// store/station_nodes.go::SetStationNodes.
func (s *StationService) SetNodes(stationID int64, nodeNames []string) error {
	station, err := s.db.GetOperatorStation(stationID)
	if err != nil {
		return err
	}

	// Two different sets, deliberately.
	//
	// stationNodes — what THIS station currently owns. Only these may be detached
	// by the removal pass below; widening that pass to the whole process would rip
	// nodes out from under sibling stations.
	stationNodes, err := s.db.ListProcessNodesByStation(stationID)
	if err != nil {
		return err
	}

	// byCoreName — every node under the PROCESS. The reuse-or-create decision has
	// to be process-global: a Core node already modelled in this process (orphaned,
	// or currently owned by a sibling station) must be ADOPTED, never re-created.
	// Making this decision from the station-local set is what let one Core node
	// spawn three process_nodes rows at HK (PLN_01 → ids 1, 13, 17): each rebind
	// that didn't already own the node minted a fresh row, and GenerateUniqueCode
	// suffixed the code (pln-01, pln-01-2, pln-01-3) to satisfy the only constraint
	// there was, UNIQUE(process_id, code). Every copy then carried its own runtime
	// row and drew its own copy of every PLC tick, because findActiveClaim resolves
	// a claim by core_node_name rather than by node id — so all three matched.
	processNodes, err := s.db.ListProcessNodesByProcess(station.ProcessID)
	if err != nil {
		return err
	}
	byCoreName := map[string]processes.Node{}
	for _, n := range processNodes {
		byCoreName[n.CoreNodeName] = n
	}

	// Normalize input: trim and deduplicate, preserving order.
	clean := make([]string, 0, len(nodeNames))
	desired := map[string]bool{}
	for _, name := range nodeNames {
		name = strings.TrimSpace(name)
		if name != "" && !desired[name] {
			desired[name] = true
			clean = append(clean, name)
		}
	}

	for i, name := range clean {
		if n, exists := byCoreName[name]; exists {
			// Adopt in place. UNIQUE(process_id, core_node_name) means a Core node
			// has exactly ONE row per process, so finding it on another station is a
			// MOVE, not a conflict — and it has to be a move, or we are back to
			// minting duplicates. Re-point it, re-sequence it, re-enable it.
			if n.OperatorStationID == nil {
				log.Printf("station: adopting orphaned process_node %d (%s) onto station %d", n.ID, name, stationID)
			} else if *n.OperatorStationID != stationID {
				log.Printf("station: moving process_node %d (%s) from station %d to station %d", n.ID, name, *n.OperatorStationID, stationID)
			}
			if _, err := s.db.Exec(`UPDATE process_nodes SET operator_station_id=?, sequence=?, enabled=1, updated_at=datetime('now')
				WHERE id=?`, stationID, i+1, n.ID); err != nil {
				return err
			}
			if _, err := s.db.EnsureProcessNodeRuntime(n.ID); err != nil {
				return err
			}
			continue
		}
		id, err := s.db.CreateProcessNode(processes.NodeInput{
			ProcessID:         station.ProcessID,
			OperatorStationID: &stationID,
			CoreNodeName:      name,
			Name:              name,
			Sequence:          i + 1,
			Enabled:           true,
		})
		if err != nil {
			return err
		}
		if _, err := s.db.EnsureProcessNodeRuntime(id); err != nil {
			return err
		}
	}

	for _, n := range stationNodes {
		if desired[n.CoreNodeName] {
			continue
		}
		active, err := s.db.ListActiveOrdersByProcessNode(n.ID)
		if err != nil {
			return err
		}
		if len(active) > 0 {
			if _, err := s.db.Exec(`UPDATE process_nodes SET enabled=0, updated_at=datetime('now') WHERE id=?`, n.ID); err != nil {
				return err
			}
			continue
		}
		if err := s.db.DeleteProcessNode(n.ID); err != nil {
			return err
		}
	}

	return nil
}

// BuildView returns the operator station view used by the operator
// HMI. Cross-aggregate: stations + processes + claims + lineside.
//
// Phase 6.1 introduced this method as a thin delegate; Phase 6.4a
// moved the body in from the (now-deleted) outer
// store/station_views.go::BuildOperatorStationView. The two helpers
// (ComputeSwapReady, LookupLastReleaseError) stay in store/ so the
// existing test file station_views_test.go can continue exercising
// them directly without import-cycle gymnastics.
// BuildView assembles the operator-station view.
//
// ctx is honoured at the tile-loop boundary so a build whose requester has gone
// away stops instead of running to completion. That matters more here than the
// usual "be a good citizen": Edge serialises every DB operation on one
// connection (store.Open sets SetMaxOpenConns(1)), and a tile costs ~8 queries,
// so an abandoned 22-tile build holds the connection against work someone is
// still waiting on. Before this, a browser abort freed only the browser — the
// handler took no context, so each timed-out poll ADDED an orphan build and
// removed none, which is how Springfield's bin-loader board wound from 3.1s to
// 116s over a day of uptime.
//
// Cancellation is checked per TILE, not per query: it needs no ctx plumbing
// through the 300+ non-context store calls and still bounds the wasted work to
// a single tile.
func (s *StationService) BuildView(ctx context.Context, stationID int64) (*store.OperatorStationView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	station, err := s.db.GetOperatorStation(stationID)
	if err != nil {
		return nil, err
	}
	process, err := s.db.GetProcess(station.ProcessID)
	if err != nil {
		return nil, err
	}

	view := &store.OperatorStationView{
		Station: *station,
		Process: *process,
	}
	if process.ActiveStyleID != nil {
		if st, err := s.db.GetStyle(*process.ActiveStyleID); err == nil {
			view.CurrentStyle = st
		}
	}
	if process.TargetStyleID != nil {
		if st, err := s.db.GetStyle(*process.TargetStyleID); err == nil {
			view.TargetStyle = st
		}
	}
	view.AvailableStyles, _ = s.db.ListStylesByProcess(process.ID)
	if co, err := s.db.GetActiveProcessChangeover(process.ID); err == nil {
		view.ActiveChangeover = co
		if stationTask, err := s.db.GetChangeoverStationTaskByStation(co.ID, stationID); err == nil {
			view.StationTask = stationTask
		}
	}

	nodes, err := s.db.ListProcessNodesByStation(stationID)
	if err != nil {
		return nil, err
	}
	nodeTaskMap := map[int64]processes.NodeTask{}
	childOf := map[int64]string{}

	// THREE narrowings used to hide a changeover node from its own station, and
	// all three are relaxed here. A press-index extension seat is auto-created
	// with no operator_station_id (changeover_service.go inserts only
	// process_id/core_node_name/code/name), so it fell through every one:
	//
	//  1. `if view.StationTask != nil` — a station with no
	//     changeover_station_tasks row got an EMPTY task map, so none of its
	//     nodes showed a task even when tasks existed. The real precondition is
	//     the CHANGEOVER existing, not this station having a row; gate on that.
	//  2. ListChangeoverNodeTasksByStation filters `n.operator_station_id=?`,
	//     which drops every task whose node has no station. Read the tasks
	//     unfiltered and key them by process_node_id — safe, because the map is
	//     only ever consulted for nodes in THIS station's list, so another
	//     station's task can never match.
	//  3. ListProcessNodesByStation likewise never returns a stationless node.
	//     Resolve each participant's station (own → owning task's node) and
	//     append the ones that belong here as CHILD tiles of the node they
	//     extend.
	if view.ActiveChangeover != nil {
		allTasks, _ := s.db.ListChangeoverNodeTasks(view.ActiveChangeover.ID)
		taskByID := make(map[int64]processes.NodeTask, len(allTasks))
		for _, nodeTask := range allTasks {
			nodeTaskMap[nodeTask.ProcessNodeID] = nodeTask
			taskByID[nodeTask.ID] = nodeTask
		}

		known := make(map[int64]bool, len(nodes))
		for i := range nodes {
			known[nodes[i].ID] = true
		}
		// Does THIS board own any of the changeover's work? Computed from the
		// station's own nodes, before any adoption, so it answers "is this
		// changeover being run from here" rather than "did we adopt something".
		// It is the anchor for the orphan fallback below.
		stationRunsChangeover := false
		for i := range nodes {
			if _, ok := nodeTaskMap[nodes[i].ID]; ok {
				stationRunsChangeover = true
				break
			}
		}
		parts, perr := s.db.ListParticipantsWithStation(view.ActiveChangeover.ID)
		if perr != nil {
			log.Printf("station view: resolve participant stations for changeover %d: %v", view.ActiveChangeover.ID, perr)
		}
		for _, p := range parts {
			if p.ProcessNodeID == nil || known[*p.ProcessNodeID] {
				continue
			}
			// Two ways a participant belongs on this board:
			//
			//  - OWNER: it has no station of its own but the task that owns it
			//    does, and that station is us. The press-index case — PLN_02 is
			//    an `indexed_over` seat of PLN_01's task, so it rides along.
			//
			//  - ORPHAN: it resolves to NO station at all. That happens when a
			//    changeover FANS OUT and gives the seat its OWN task: station
			//    resolution walks own -> owning-task's-node, and for a
			//    self-owning task both are the same stationless row, so it
			//    lands nil and the seat renders NOWHERE. Hopkinsville
			//    2026-07-28: a tote->bin changeover dropped all four press
			//    positions independently, PLN_02/PLN_05 vanished from the
			//    board, and the two robots parked at them could not be
			//    released — there was no tile to press. Adopt onto the board
			//    already running this changeover.
			//
			// Adoption stays inside the ActiveChangeover guard, so these seats
			// appear only while they have work and disappear afterwards. That
			// matters: a paired on-deck position must NOT be a permanent tile —
			// LoadBin refuses to stamp a part there precisely because doing so
			// hung a press-index swap once already.
			byOwner := p.StationID != nil && *p.StationID == stationID && p.StationSource == "owner"
			orphan := p.StationID == nil && stationRunsChangeover
			if !byOwner && !orphan {
				continue
			}
			child, gerr := s.db.GetProcessNode(*p.ProcessNodeID)
			if gerr != nil || child == nil {
				continue
			}
			// Render as a child of the node whose task owns it — but never of
			// itself. A fanned-out seat owns its own task, so naming it its own
			// parent would be meaningless; it stands as its own tile instead.
			if p.OwningTaskID != nil {
				if owner, ok := taskByID[*p.OwningTaskID]; ok && owner.ProcessNodeID != child.ID {
					childOf[child.ID] = owner.NodeName
				}
			}
			nodes = append(nodes, *child)
			known[child.ID] = true
		}
	}
	// Loader payload sets, computed ONCE for the whole board. PayloadsForLoader
	// walks every process/style/claim, so calling it per manual_swap tile made an
	// N-home board do N walks (14 homes → ~44s on the Springfield bin loader). One
	// walk, keyed by (core node, role); each tile just looks itself up below.
	loaderPayloads, err := processes.PayloadsForManualSwapNodes(s.db.DB)
	if err != nil {
		loaderPayloads = nil // fail-open: tiles fall back to the claim-derived set
	}
	// Per-tile lookups hoisted to one query each for the whole board. Same
	// motivation as loaderPayloads above: every read serialises on one
	// connection, so a query inside the tile loop is a query multiplied by the
	// tile count (22 on the Springfield bin loader).
	//
	// Claims are safe to index by core node name because style_node_claims
	// declares UNIQUE(style_id, core_node_name) — at most one claim per
	// (style, node), so a map lookup is exactly what GetStyleNodeClaimByNode
	// returns. Both maps fail open to nil: a nil map lookup yields the zero
	// value, and the tile then reads as "no claim", which is the same thing
	// GetStyleNodeClaimByNode's sql.ErrNoRows produced.
	var activeClaims, targetClaims map[string]processes.NodeClaim
	if process.ActiveStyleID != nil {
		activeClaims, _ = claimsByCoreNode(s.db.DB, *process.ActiveStyleID)
	}
	if process.TargetStyleID != nil {
		targetClaims, _ = claimsByCoreNode(s.db.DB, *process.TargetStyleID)
	}
	nodeIDs := make([]int64, 0, len(nodes))
	nodeKeys := make([]orders.NodeKey, 0, len(nodes))
	for _, node := range nodes {
		nodeIDs = append(nodeIDs, node.ID)
		nodeKeys = append(nodeKeys, orders.NodeKey{ProcessNodeID: node.ID, CoreNodeName: node.CoreNodeName})
	}
	// Runtime rows: one SELECT for the board. This is the READ half of
	// EnsureProcessNodeRuntime — the tile loop still calls Ensure for any node
	// missing from this map, so the INSERT happens for exactly the same set of
	// nodes as before. On a live plant every node already has its row, so the
	// per-tile query disappears and the write path is untouched.
	runtimes, err := processes.RuntimesForNodes(s.db.DB, nodeIDs)
	if err != nil {
		runtimes = nil // fall back to per-node Ensure below
	}
	boardOrders, err := orders.ListActiveByNodeKeys(s.db.DB, nodeKeys)
	if err != nil {
		boardOrders = nil
	}
	// Standing supply refusals, ONE READ FOR THE WHOLE BOARD, indexed
	// loader_node → payload → refusal. The table holds only what is open — one
	// row per card actually refused right now — so the whole-table read is
	// cheaper than a query per card, and this is the board's poll path, which is
	// the one place that difference is felt. Best-effort, like every other
	// enrichment here: a failed read renders a board without refusal state
	// rather than no board.
	refusals := map[string]map[string]domain.SupplyRefusal{}
	// byPayload is the CUSTOMER's index of the same rows. A cell does not know
	// which window supplies it — that resolution is the thing the broadcast
	// design deliberately avoids needing — so it looks up by part alone and the
	// sentence names the window that said it.
	byPayload := map[string]domain.CellSupplyRefusal{}
	if open, rerr := s.db.ListOpenSupplyRefusals(); rerr == nil {
		for _, r := range open {
			if refusals[r.LoaderNode] == nil {
				refusals[r.LoaderNode] = map[string]domain.SupplyRefusal{}
			}
			refusals[r.LoaderNode][r.PayloadCode] = domain.SupplyRefusal{
				RefusedAt: r.RefusedAt,
				RefusedBy: r.RefusedBy,
				Answered:  r.Answered(),
				AckChoice: r.AckChoice,
			}
			// An UNANSWERED refusal wins over an answered one for the same part:
			// it is the one that still needs a person, and it is what the modal
			// fires on.
			if prev, seen := byPayload[r.PayloadCode]; seen && !prev.Answered {
				continue
			}
			byPayload[r.PayloadCode] = domain.CellSupplyRefusal{
				LoaderNode: r.LoaderNode, PayloadCode: r.PayloadCode,
				RefusedAt: r.RefusedAt, RefusedBy: r.RefusedBy,
				Answered: r.Answered(), AckChoice: r.AckChoice,
			}
		}
	}
	activeBuckets, err := lineside.ListActiveForNodes(s.db.DB, nodeIDs)
	if err != nil {
		activeBuckets = nil // best-effort, as the per-node call was
	}
	inactiveBuckets, err := lineside.ListInactiveForNodes(s.db.DB, nodeIDs)
	if err != nil {
		inactiveBuckets = nil
	}
	for _, node := range nodes {
		// See the BuildView doc comment: one abandoned tile-loop iteration is the
		// granularity at which we give the single DB connection back.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		nodeView := store.StationNodeView{Node: node}
		runtime := runtimes[node.ID]
		if runtime == nil {
			// No row yet (a freshly created node), or the batch read failed.
			// Ensure materialises it, exactly as the per-tile call always did.
			runtime, _ = s.db.EnsureProcessNodeRuntime(node.ID)
		}
		nodeView.Runtime = runtime
		if process.ActiveStyleID != nil && node.CoreNodeName != "" {
			if c, ok := activeClaims[node.CoreNodeName]; ok {
				claim := c
				nodeView.ActiveClaim = &claim
			}
		}
		if process.TargetStyleID != nil && node.CoreNodeName != "" {
			if c, ok := targetClaims[node.CoreNodeName]; ok {
				claim := c
				nodeView.TargetClaim = &claim
			}
		}
		// Core-owned-loader fallback: a window/position of a Core loader with no
		// per-style edge claim still reads as a manual_swap loader node, so the
		// operator board renders (and the runtime treats it as a loader). Synthesize
		// the claim from the aggregate via the SAME resolver the runtime uses, so the
		// view and the engine never disagree. A node that isn't an aggregate loader
		// resolves to nil (clean miss) and keeps its plain-node view.
		if nodeView.ActiveClaim == nil && s.loaders != nil && node.CoreNodeName != "" {
			if l, lerr := s.loaders.LoaderForNode(domain.NodeID(node.CoreNodeName)); lerr == nil && l != nil {
				nodeView.ActiveClaim = l.SynthClaim(domain.NodeID(node.CoreNodeName))
			}
		}
		if nodeTask, ok := nodeTaskMap[node.ID]; ok {
			taskCopy := nodeTask
			nodeView.ChangeoverTask = &taskCopy
		}
		// Child tile: rendered here only because the node it extends lives on
		// this station. Marked so the board can suppress the release button —
		// it owns no task and no order, so there is nothing to release.
		nodeView.ChildOfNode = childOf[node.ID]
		// Include orders sourcing FROM this node's CoreNode in addition to
		// orders tracked at this process_node. A manual_swap supermarket
		// loader (SMN_001 etc.) doesn't directly own orders — the line
		// operator's REQUEST creates orders tracked at the line node. But
		// the loader operator still needs to see "demand for my bin" so
		// they keep it loaded. Plant test 2026-04-27: line-initiated swap
		// orders went silent on the loader UI after the kanban-spam guard
		// stopped firing process-node-tracked orders here.
		nodeView.Orders = boardOrders[node.ID]
		nodeView.SwapReady = store.ComputeSwapReady(s.db, nodeView.ActiveClaim, runtime, nodeView.ChangeoverTask)
		// Lineside buckets power the active-bar and stranded-chip UI on
		// the operator station modal. Best-effort — absence of buckets
		// just means the node has nothing pulled to lineside yet.
		nodeView.LinesideActive = activeBuckets[node.ID]
		nodeView.LinesideInactive = inactiveBuckets[node.ID]
		// Surface any pending release-time error that's been rolled back to
		// Staged for the operator to retry.
		nodeView.LastReleaseError = store.LookupLastReleaseError(s.db, runtime)
		// Surface any active parked-ticks alarm (P2-C7/C8): consume ticks piling
		// up on this node while no bin is bound. Rendered as an amber chip.
		if s.stranded != nil {
			nodeView.StrandedAlarm = s.stranded(node.CoreNodeName)
		}
		// THE CUSTOMER'S HALF. A call and the part: this node has an outstanding
		// order for something a loader operator has said they cannot supply.
		// Attached wherever that is true, which on a loader window is its own
		// refusal and on a cell is somebody else's — the same test either way,
		// because "a cell that asked" is what owner decision 3 means and an
		// outstanding order is what asking looks like.
		if len(byPayload) > 0 {
			for i := range nodeView.Orders {
				o := nodeView.Orders[i]
				if o.PayloadCode == "" || protocol.IsTerminal(o.Status) {
					continue
				}
				if r, ok := byPayload[o.PayloadCode]; ok {
					rc := r
					nodeView.SupplyRefusedForMe = &rc
					break
				}
			}
		}
		// Multi-process loader-board unions: for a manual_swap node, resolve
		// the active-style and all-style payload sets across EVERY active
		// process sharing this CoreNodeName (PayloadsForLoader walks all
		// processes), so a loader shared by two cells surfaces both cells'
		// payloads, not just this station's. Plus the transitional flag the
		// board reads to default into preload mode.
		if nodeView.ActiveClaim != nil && nodeView.ActiveClaim.SwapMode == protocol.SwapModeManualSwap {
			if rp, ok := loaderPayloads[node.CoreNodeName][nodeView.ActiveClaim.Role]; ok {
				nodeView.ActiveStylePayloads = rp.Active
				nodeView.AllStylePayloads = rp.All
			}
			// Standing refusals for this window's cards, from the one snapshot
			// taken above. Role-agnostic on purpose: an unloader states the same
			// kind of thing about empties that a loader states about parts, and
			// the mechanism carries over unchanged — only the WORDING differs,
			// and that lives on the render side.
			if forThisWindow := refusals[node.CoreNodeName]; len(forThisWindow) > 0 {
				nodeView.SupplyRefusals = forThisWindow
			}
			// Operator-driven (board defaults to preload) + dedicated-position layout +
			// window-group membership all come from the Core aggregate — the SAME resolver
			// the runtime uses — so the board and the engine never disagree. A node absent
			// from the aggregate resolves to nil (exactly as for the runtime), leaving the
			// operator/layout fields false.
			if s.loaders != nil {
				if loader, err := s.loaders.LoaderAt(domain.NodeID(node.CoreNodeName), domain.LoaderRole(nodeView.ActiveClaim.Role)); err == nil && loader != nil {
					nodeView.OperatorDriven = loader.IsOperatorDriven()
					nodeView.HomeLocationLoader = loader.IsDedicated()
					// Core owns the loader's payload set — the board shows it (the edge claim
					// is just the node now). Overrides the claim-derived set above; falls back
					// to it only when the loader carries no Core payloads (legacy / not migrated).
					// Scoped to THIS node (the same per-node set the load/request gate uses):
					// a dedicated home shows only its own pinned payload, not the loader's
					// other positions' parts, so the board and the engine never disagree.
					if codes := loader.LoadablePayloadCodesAt(domain.NodeID(node.CoreNodeName)); len(codes) > 0 {
						nodeView.ActiveStylePayloads = codes
						nodeView.AllStylePayloads = codes
					}
					if loader.IsShared() {
						if wins := loader.Windows(); len(wins) > 1 {
							nodeView.WindowGroupAnchor = string(loader.ID())
							names := make([]string, len(wins))
							for i, w := range wins {
								names[i] = string(w.Node)
							}
							nodeView.WindowNodes = names
						}
					}
				}
			}
		}
		view.Nodes = append(view.Nodes, nodeView)
	}

	// Lineside UOP per active payload, attached to manual_swap loader nodes so
	// the transitional board can show real numbers on ACTIVE cards instead of a
	// meaningless "no demand" (the loader is operator-driven). Computed once for
	// the process's active style; all local Edge data.
	// Gate on the board actually having a tile that consumes the result.
	// activePayloadLineside is a PLANT-WIDE scan — every active consume claim on
	// the edge, not just this station's — so a board with no manual_swap produce
	// tile was paying the full cost and then discarding every value. The
	// predicate is exactly the one the loop below filters on.
	wantsLineside := false
	for i := range view.Nodes {
		nv := &view.Nodes[i]
		if nv.ActiveClaim != nil &&
			nv.ActiveClaim.SwapMode == protocol.SwapModeManualSwap &&
			nv.ActiveClaim.Role == protocol.ClaimRoleProduce {
			wantsLineside = true
			break
		}
	}
	if lineside := s.activePayloadLineside(wantsLineside); len(lineside) > 0 {
		for i := range view.Nodes {
			nv := &view.Nodes[i]
			if nv.ActiveClaim == nil ||
				nv.ActiveClaim.SwapMode != protocol.SwapModeManualSwap ||
				nv.ActiveClaim.Role != protocol.ClaimRoleProduce {
				continue
			}
			m := map[string]int{}
			starved := map[string]bool{}
			for _, p := range nv.ActiveStylePayloads {
				if v, ok := lineside[p]; ok {
					m[p] = v
					if nv.ActiveClaim != nil && linesideStarved(nv.ActiveClaim.UOPCapacity, v) {
						starved[p] = true
					}
				}
			}
			if len(m) > 0 {
				nv.ActivePayloadLineside = m
			}
			if len(starved) > 0 {
				nv.StarvedPayloads = starved
			}
		}
	}

	return view, nil
}

// linesideStarved reports whether a manual_swap loader's lineside stock for a
// payload has dropped into the operator-preload danger zone. v1 is a simple
// floor: below a quarter of a full bin (capacityUOP). This is the single
// danger-tier function — time-to-empty escalation slots in here later
// (SHINGO_TODO "Starvation alert time-to-empty escalation") without touching
// the view assembly or the render path. Returns false when capacity is
// unknown (no floor to compare against).
func linesideStarved(capacityUOP, linesideUOP int) bool {
	if capacityUOP <= 0 {
		return false
	}
	return linesideUOP < capacityUOP/4
}

// activePayloadLineside sums the current lineside UOP per payload across EVERY
// active-style CONSUME node on the Edge — not just this station's process — so a
// loader's payloads pick up the lineside at whatever cell consumes them, even
// when that cell is a different process. Per node it counts the bin at the
// consuming node (RemainingUOPCached) plus parts already pulled to the line
// (active lineside buckets). "All active, summed": every active consume claim
// for a payload contributes, so a loader feeding multiple cells sees the
// combined lineside. A consume claim with several allowed payloads attributes
// its node's total to each (rare; the common case is one payload per node). All
// reads are local Edge state — no Core round-trip.
// wanted=false short-circuits: the caller has no tile that would read the result,
// and this is a plant-wide scan.
func (s *StationService) activePayloadLineside(wanted bool) map[string]int {
	out := map[string]int{}
	if !wanted {
		return out
	}
	// Walk first, WITHOUT touching the DB per visit, then satisfy every visit
	// from two batched reads. The per-visit GetProcessNodeRuntime +
	// ListActiveLinesideBuckets were 2 queries per active consume claim
	// plant-wide, on the one connection store.Open allows.
	//
	// Visits are collected with their MULTIPLICITY preserved, not deduplicated by
	// node: the original recomputed the node total on every visit and added it to
	// each of that claim's payloads, so a node reached by two claims contributed
	// twice. Deduplicating here would silently change the sums.
	type visit struct {
		nodeID   int64
		payloads []string
	}
	var visits []visit
	nodeIDs := make([]int64, 0, 8)
	_ = processes.WalkClaims(s.db.DB, processes.WalkOpts{
		ActiveOnly:  true,
		Role:        protocol.ClaimRoleConsume,
		ResolveNode: true,
	}, func(ctx processes.WalkCtx) bool {
		if ctx.Node.ID == 0 {
			return false
		}
		payloads := ctx.Claim.AllowedPayloads()
		if len(payloads) == 0 {
			return false
		}
		visits = append(visits, visit{nodeID: ctx.Node.ID, payloads: payloads})
		nodeIDs = append(nodeIDs, ctx.Node.ID)
		return false // collect all
	})
	if len(visits) == 0 {
		return out
	}

	// Best-effort, matching the per-node calls this replaces: a read error there
	// contributed 0 for that node. A failed batch contributes 0 for every node,
	// which is a wider blast radius, so it is logged rather than silent.
	runtimes, err := processes.RuntimesForNodes(s.db.DB, nodeIDs)
	if err != nil {
		log.Printf("station view: lineside runtime batch failed, lineside totals will read low: %v", err)
	}
	buckets, err := lineside.ListActiveForNodes(s.db.DB, nodeIDs)
	if err != nil {
		log.Printf("station view: lineside bucket batch failed, lineside totals will read low: %v", err)
	}

	totals := make(map[int64]int, len(nodeIDs))
	for _, id := range nodeIDs {
		if _, done := totals[id]; done {
			continue
		}
		total := 0
		if rt := runtimes[id]; rt != nil {
			total += rt.RemainingUOPCached
		}
		for _, b := range buckets[id] {
			total += b.Qty
		}
		totals[id] = total
	}
	for _, v := range visits {
		for _, p := range v.payloads {
			out[p] += totals[v.nodeID]
		}
	}
	return out
}

// ── Per-station CRUD ────────────────────────────────────────────────

// List returns every operator_stations row.
func (s *StationService) List() ([]stations.Station, error) {
	return s.db.ListOperatorStations()
}

// ListByProcess returns operator_stations rows for one process.
func (s *StationService) ListByProcess(processID int64) ([]stations.Station, error) {
	return s.db.ListOperatorStationsByProcess(processID)
}

// Get returns one operator_station by id.
func (s *StationService) Get(id int64) (*stations.Station, error) {
	return s.db.GetOperatorStation(id)
}

// Create inserts a station, generating code and sequence when not
// supplied.
func (s *StationService) Create(in stations.Input) (int64, error) {
	return s.db.CreateOperatorStation(in)
}

// Update modifies an operator_station.
func (s *StationService) Update(id int64, in stations.Input) error {
	return s.db.UpdateOperatorStation(id, in)
}

// Delete removes an operator_station.
func (s *StationService) Delete(id int64) error {
	return s.db.DeleteOperatorStation(id)
}

// Touch updates last_seen_at and health_status.
//
// Throttled, because it is called on EVERY operator-station view request —
// including every request that merely joined an already-running build — and it is
// a WRITE on the single connection store.Open allows, so it queues ahead of the
// reads the boards are waiting on. Five boards polling produced a write per poll
// for a column nothing behaves on.
//
// A status CHANGE always writes through immediately; only the repeated
// same-status refresh is collapsed. Worst case last_seen_at trails reality by
// touchThrottle while a board is polling steadily.
func (s *StationService) Touch(id int64, healthStatus string) error {
	s.touchMu.Lock()
	if s.touched == nil {
		s.touched = make(map[int64]touchState)
	}
	prev, seen := s.touched[id]
	if seen && prev.status == healthStatus && time.Since(prev.at) < touchThrottle {
		s.touchMu.Unlock()
		return nil
	}
	// Record before the write so a slow or failing write cannot turn into a
	// tight retry loop on the hot path.
	s.touched[id] = touchState{status: healthStatus, at: time.Now()}
	s.touchMu.Unlock()
	return s.db.TouchOperatorStation(id, healthStatus)
}

// Move swaps the sequence of a station with its neighbour in the
// given direction ("up" or "down").
func (s *StationService) Move(id int64, direction string) error {
	return s.db.MoveOperatorStation(id, direction)
}

// GetNodeNames returns the core_node_name list for a station.
func (s *StationService) GetNodeNames(stationID int64) ([]string, error) {
	return s.db.GetStationNodeNames(stationID)
}
