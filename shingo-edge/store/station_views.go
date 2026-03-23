package store

type StationNodeView struct {
	Node           ProcessNode              `json:"node"`
	Runtime        *ProcessNodeRuntimeState `json:"runtime,omitempty"`
	ActiveClaim    *StyleNodeClaim          `json:"active_claim,omitempty"`
	ChangeoverTask *ChangeoverNodeTask      `json:"changeover_task,omitempty"`
	Orders         []Order                  `json:"orders"`
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
		if nodeTask, ok := nodeTaskMap[node.ID]; ok {
			taskCopy := nodeTask
			nodeView.ChangeoverTask = &taskCopy
		}
		nodeView.Orders, _ = db.ListActiveOrdersByProcessNode(node.ID)
		view.Nodes = append(view.Nodes, nodeView)
	}
	return view, nil
}

