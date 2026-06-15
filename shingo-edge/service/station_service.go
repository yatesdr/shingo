package service

import (
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

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
}

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

	existing, err := s.db.ListProcessNodesByStation(stationID)
	if err != nil {
		return err
	}

	existingMap := map[string]processes.Node{}
	for _, n := range existing {
		existingMap[n.CoreNodeName] = n
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
		if _, exists := existingMap[name]; exists {
			if _, err := s.db.Exec(`UPDATE process_nodes SET sequence=?, enabled=1, updated_at=datetime('now')
				WHERE operator_station_id=? AND core_node_name=?`, i+1, stationID, name); err != nil {
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

	for _, n := range existing {
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
func (s *StationService) BuildView(stationID int64) (*store.OperatorStationView, error) {
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
	if view.StationTask != nil {
		nodeTasks, _ := s.db.ListChangeoverNodeTasksByStation(view.ActiveChangeover.ID, stationID)
		for _, nodeTask := range nodeTasks {
			nodeTaskMap[nodeTask.ProcessNodeID] = nodeTask
		}
	}
	for _, node := range nodes {
		nodeView := store.StationNodeView{Node: node}
		runtime, _ := s.db.EnsureProcessNodeRuntime(node.ID)
		nodeView.Runtime = runtime
		if process.ActiveStyleID != nil && node.CoreNodeName != "" {
			nodeView.ActiveClaim, _ = s.db.GetStyleNodeClaimByNode(*process.ActiveStyleID, node.CoreNodeName)
		}
		if process.TargetStyleID != nil && node.CoreNodeName != "" {
			nodeView.TargetClaim, _ = s.db.GetStyleNodeClaimByNode(*process.TargetStyleID, node.CoreNodeName)
		}
		if nodeTask, ok := nodeTaskMap[node.ID]; ok {
			taskCopy := nodeTask
			nodeView.ChangeoverTask = &taskCopy
		}
		// Include orders sourcing FROM this node's CoreNode in addition to
		// orders tracked at this process_node. A manual_swap supermarket
		// loader (SMN_001 etc.) doesn't directly own orders — the line
		// operator's REQUEST creates orders tracked at the line node. But
		// the loader operator still needs to see "demand for my bin" so
		// they keep it loaded. Plant test 2026-04-27: line-initiated swap
		// orders went silent on the loader UI after the kanban-spam guard
		// stopped firing process-node-tracked orders here.
		nodeView.Orders, _ = s.db.ListActiveOrdersByProcessNodeOrSource(node.ID, node.CoreNodeName)
		nodeView.SwapReady = store.ComputeSwapReady(s.db, nodeView.ActiveClaim, runtime, nodeView.ChangeoverTask)
		// Lineside buckets power the active-bar and stranded-chip UI on
		// the operator station modal. Best-effort — absence of buckets
		// just means the node has nothing pulled to lineside yet.
		nodeView.LinesideActive, _ = s.db.ListActiveLinesideBuckets(node.ID)
		nodeView.LinesideInactive, _ = s.db.ListInactiveLinesideBuckets(node.ID)
		// Surface any pending release-time error that's been rolled back to
		// Staged for the operator to retry.
		nodeView.LastReleaseError = store.LookupLastReleaseError(s.db, runtime)
		// Multi-process loader-board unions: for a manual_swap node, resolve
		// the active-style and all-style payload sets across EVERY active
		// process sharing this CoreNodeName (PayloadsForLoader walks all
		// processes), so a loader shared by two cells surfaces both cells'
		// payloads, not just this station's. Plus the transitional flag the
		// board reads to default into preload mode.
		if nodeView.ActiveClaim != nil && nodeView.ActiveClaim.SwapMode == protocol.SwapModeManualSwap {
			if act, all, _, err := processes.PayloadsForLoader(s.db.DB, node.CoreNodeName, nodeView.ActiveClaim.Role); err == nil {
				nodeView.ActiveStylePayloads = act
				nodeView.AllStylePayloads = all
			}
			if transitional, err := s.db.IsTransitionalLoader(node.CoreNodeName); err == nil {
				nodeView.TransitionalLoader = transitional
			}
			if homeLoc, err := s.db.IsHomeLocationLoader(node.CoreNodeName); err == nil {
				nodeView.HomeLocationLoader = homeLoc
			}
			// Window-group membership from the Core aggregate (the view-path cutover,
			// C4b). The per-node legacy fields above structurally cannot express that
			// this node is one window of a shared multi-window loader; the resolver —
			// the same one the runtime resolves empties through — can. Additive: only a
			// SHARED loader with more than one window populates these; single-window,
			// dedicated, and legacy loaders leave them empty, so existing views are
			// byte-identical.
			if s.loaders != nil {
				if loader, err := s.loaders.LoaderAt(domain.NodeID(node.CoreNodeName), domain.LoaderRole(nodeView.ActiveClaim.Role)); err == nil && loader != nil && loader.IsShared() {
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
		view.Nodes = append(view.Nodes, nodeView)
	}

	// Lineside UOP per active payload, attached to manual_swap loader nodes so
	// the transitional board can show real numbers on ACTIVE cards instead of a
	// meaningless "no demand" (the loader is operator-driven). Computed once for
	// the process's active style; all local Edge data.
	if lineside := s.activePayloadLineside(); len(lineside) > 0 {
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
func (s *StationService) activePayloadLineside() map[string]int {
	out := map[string]int{}
	_ = processes.WalkClaims(s.db.DB, processes.WalkOpts{
		ActiveOnly:  true,
		Role:        protocol.ClaimRoleConsume,
		ResolveNode: true,
	}, func(ctx processes.WalkCtx) bool {
		if ctx.Node.ID == 0 {
			return false
		}
		total := 0
		if rt, err := s.db.GetProcessNodeRuntime(ctx.Node.ID); err == nil && rt != nil {
			total += rt.RemainingUOPCached
		}
		if buckets, err := s.db.ListActiveLinesideBuckets(ctx.Node.ID); err == nil {
			for _, b := range buckets {
				total += b.Qty
			}
		}
		for _, p := range ctx.Claim.AllowedPayloads() {
			out[p] += total
		}
		return false // collect all
	})
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
func (s *StationService) Touch(id int64, healthStatus string) error {
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
