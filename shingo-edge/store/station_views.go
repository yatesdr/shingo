package store

type StationNodeView struct {
	Node           OpStationNode          `json:"node"`
	Runtime        *OpNodeRuntimeState    `json:"runtime,omitempty"`
	Assignment     *OpNodeStyleAssignment `json:"assignment,omitempty"`
	NextStyle      *OpNodeStyleAssignment `json:"next_style,omitempty"`
	ChangeoverTask *ChangeoverNodeTask    `json:"changeover_task,omitempty"`
	Orders         []Order                `json:"orders"`
}

type OperatorStationView struct {
	Station              OperatorStation        `json:"station"`
	Process              Process                `json:"process"`
	ProcessCounter       *ProcessCounterBinding `json:"process_counter,omitempty"`
	CurrentStyle         *Style                 `json:"current_style,omitempty"`
	TargetStyle          *Style                 `json:"target_style,omitempty"`
	AvailableStyles      []Style                `json:"available_styles,omitempty"`
	ActiveChangeover     *ProcessChangeover     `json:"active_changeover,omitempty"`
	StationTask          *ChangeoverStationTask `json:"station_task,omitempty"`
	PendingNodeChanges   int                    `json:"pending_node_changes"`
	CompletedNodeChanges int                    `json:"completed_node_changes"`
	Nodes                []StationNodeView      `json:"nodes"`
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
	view.ProcessCounter, _ = db.GetProcessCounterBinding(process.ID)
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

	nodes, err := db.ListOpStationNodesByStation(stationID)
	if err != nil {
		return nil, err
	}
	nodeTaskMap := map[int64]ChangeoverNodeTask{}
	if view.StationTask != nil {
		nodeTasks, _ := db.ListChangeoverNodeTasks(view.StationTask.ID)
		for _, nodeTask := range nodeTasks {
			nodeTaskMap[nodeTask.OpNodeID] = nodeTask
			if isNodeTaskCompleteForPhase(nodeTask, view.StationTask.CurrentPhase) {
				view.CompletedNodeChanges++
			} else {
				view.PendingNodeChanges++
			}
		}
	}
	for _, node := range nodes {
		nodeView := StationNodeView{Node: node}
		runtime, _ := db.EnsureOpNodeRuntime(node.ID)
		nodeView.Runtime = runtime
		if runtime.ActiveAssignmentID != nil {
			nodeView.Assignment, _ = db.GetOpNodeAssignment(*runtime.ActiveAssignmentID)
		} else if process.ActiveStyleID != nil {
			nodeView.Assignment, _ = db.GetOpNodeAssignmentForStyle(node.ID, *process.ActiveStyleID)
		}
		if process.TargetStyleID != nil {
			nodeView.NextStyle, _ = db.GetOpNodeAssignmentForStyle(node.ID, *process.TargetStyleID)
		}
		if nodeTask, ok := nodeTaskMap[node.ID]; ok {
			taskCopy := nodeTask
			nodeView.ChangeoverTask = &taskCopy
		}
		nodeView.Orders, _ = db.ListActiveOrdersByOpNode(node.ID)
		view.Nodes = append(view.Nodes, nodeView)
	}
	return view, nil
}

func isNodeTaskCompleteForPhase(task ChangeoverNodeTask, phase string) bool {
	switch phase {
	case "runout":
		return task.State == "unchanged" || task.State == "staged" || task.State == "line_cleared" || task.State == "released" || task.State == "switched" || task.State == "verified"
	case "tool_change":
		if task.OldMaterialReleaseRequired {
			return task.State == "line_cleared" || task.State == "released" || task.State == "switched" || task.State == "verified"
		}
		return task.State == "unchanged" || task.State == "staged" || task.State == "released" || task.State == "switched" || task.State == "verified"
	case "release":
		if task.ToAssignmentID == nil {
			return task.State == "unchanged" || task.State == "line_cleared" || task.State == "switched" || task.State == "verified"
		}
		return task.State == "released" || task.State == "switched" || task.State == "verified"
	case "cutover":
		if task.ToAssignmentID == nil {
			return task.State == "unchanged" || task.State == "line_cleared" || task.State == "switched" || task.State == "verified"
		}
		return task.State == "released" || task.State == "switched" || task.State == "verified"
	case "verify":
		return task.State == "switched" || task.State == "verified" || task.State == "unchanged" || task.State == "released"
	default:
		return task.State == "switched" || task.State == "verified" || task.State == "unchanged"
	}
}
