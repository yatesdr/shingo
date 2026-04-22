package store

// NodeBinState holds Core-side bin information fetched via telemetry.
type NodeBinState struct {
	BinLabel          string  `json:"bin_label,omitempty"`
	BinTypeCode       string  `json:"bin_type_code,omitempty"`
	PayloadCode       string  `json:"payload_code,omitempty"`
	UOPRemaining      int     `json:"uop_remaining"`
	Manifest          *string `json:"manifest,omitempty"`
	ManifestConfirmed bool    `json:"manifest_confirmed"`
	Occupied          bool    `json:"occupied"`
}

type StationNodeView struct {
	Node           ProcessNode              `json:"node"`
	Runtime        *ProcessNodeRuntimeState `json:"runtime,omitempty"`
	ActiveClaim    *StyleNodeClaim          `json:"active_claim,omitempty"`
	TargetClaim    *StyleNodeClaim          `json:"target_claim,omitempty"`
	ChangeoverTask *ChangeoverNodeTask      `json:"changeover_task,omitempty"`
	Orders         []Order                  `json:"orders"`
	BinState       *NodeBinState            `json:"bin_state,omitempty"`
	// SwapReady is true when both tracked orders for a two-robot swap are
	// in "staged" status — i.e. both robots are holding at their wait
	// points and a single coordinated release can move both forward.
	// Non-two-robot nodes always report false.
	SwapReady bool `json:"swap_ready"`
	// LinesideActive is the set of buckets currently counting toward
	// remaining UOP on this node (one row per part for the active style).
	// Rendered as the "active lineside bar" beneath the node fill-bar.
	LinesideActive []LinesideBucket `json:"lineside_active,omitempty"`
	// LinesideInactive is the set of stranded buckets — parts that were
	// pulled to lineside under a prior style and haven't been drained or
	// recalled yet. Rendered as stacked chips that open a detail modal.
	LinesideInactive []LinesideBucket `json:"lineside_inactive,omitempty"`
}

type OperatorStationView struct {
	Station          OperatorStation        `json:"station"`
	Process          Process                `json:"process"`
	CurrentStyle     *Style                 `json:"current_style,omitempty"`
	TargetStyle      *Style                 `json:"target_style,omitempty"`
	AvailableStyles  []Style                `json:"available_styles,omitempty"`
	ActiveChangeover *ProcessChangeover     `json:"active_changeover,omitempty"`
	StationTask      *ChangeoverStationTask `json:"station_task,omitempty"`
	Nodes            []StationNodeView      `json:"nodes"`
}

func (db *DB) BuildOperatorStationView(stationID int64) (*OperatorStationView, error) {
	station, err := db.GetOperatorStation(stationID)
	if err != nil {
		return nil, err
	}
	process, err := db.GetProcess(station.ProcessID)
	if err != nil {
		return nil, err
	}

	view := &OperatorStationView{
		Station: *station,
		Process: *process,
	}
	if process.ActiveStyleID != nil {
		if s, err := db.GetStyle(*process.ActiveStyleID); err == nil {
			view.CurrentStyle = s
		}
	}
	if process.TargetStyleID != nil {
		if s, err := db.GetStyle(*process.TargetStyleID); err == nil {
			view.TargetStyle = s
		}
	}
	view.AvailableStyles, _ = db.ListStylesByProcess(process.ID)
	if co, err := db.GetActiveProcessChangeover(process.ID); err == nil {
		view.ActiveChangeover = co
		if stationTask, err := db.GetChangeoverStationTaskByStation(co.ID, stationID); err == nil {
			view.StationTask = stationTask
		}
	}

	nodes, err := db.ListProcessNodesByStation(stationID)
	if err != nil {
		return nil, err
	}
	nodeTaskMap := map[int64]ChangeoverNodeTask{}
	if view.StationTask != nil {
		nodeTasks, _ := db.ListChangeoverNodeTasksByStation(view.ActiveChangeover.ID, stationID)
		for _, nodeTask := range nodeTasks {
			nodeTaskMap[nodeTask.ProcessNodeID] = nodeTask
		}
	}
	for _, node := range nodes {
		nodeView := StationNodeView{Node: node}
		runtime, _ := db.EnsureProcessNodeRuntime(node.ID)
		nodeView.Runtime = runtime
		if process.ActiveStyleID != nil && node.CoreNodeName != "" {
			nodeView.ActiveClaim, _ = db.GetStyleNodeClaimByNode(*process.ActiveStyleID, node.CoreNodeName)
		}
		if process.TargetStyleID != nil && node.CoreNodeName != "" {
			nodeView.TargetClaim, _ = db.GetStyleNodeClaimByNode(*process.TargetStyleID, node.CoreNodeName)
		}
		if nodeTask, ok := nodeTaskMap[node.ID]; ok {
			taskCopy := nodeTask
			nodeView.ChangeoverTask = &taskCopy
		}
		nodeView.Orders, _ = db.ListActiveOrdersByProcessNode(node.ID)
		nodeView.SwapReady = computeSwapReady(db, nodeView.ActiveClaim, runtime)
		// Lineside buckets power the active-bar and stranded-chip UI on
		// the operator station modal. Best-effort — absence of buckets
		// just means the node has nothing pulled to lineside yet.
		nodeView.LinesideActive, _ = db.ListActiveLinesideBuckets(node.ID)
		nodeView.LinesideInactive, _ = db.ListInactiveLinesideBuckets(node.ID)
		view.Nodes = append(view.Nodes, nodeView)
	}
	return view, nil
}

// computeSwapReady returns true only when a two-robot swap has both tracked
// orders (ActiveOrderID and StagedOrderID on the runtime) in "staged" status.
// That's the gate for the single coordinated release: both robots are holding
// at their wait points, so one operator click can move the whole swap forward.
//
// Non-two-robot claims always return false — their single staged order is
// still released via the per-order /api/orders/{id}/release endpoint.
func computeSwapReady(db *DB, claim *StyleNodeClaim, runtime *ProcessNodeRuntimeState) bool {
	if claim == nil || claim.SwapMode != "two_robot" {
		return false
	}
	if runtime == nil || runtime.ActiveOrderID == nil || runtime.StagedOrderID == nil {
		return false
	}
	active, err := db.GetOrder(*runtime.ActiveOrderID)
	if err != nil || active == nil {
		return false
	}
	staged, err := db.GetOrder(*runtime.StagedOrderID)
	if err != nil || staged == nil {
		return false
	}
	return active.Status == "staged" && staged.Status == "staged"
}

