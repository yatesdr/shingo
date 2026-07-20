package www

import (
	"net/http"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
)

// changeoverNodeView enriches a ChangeoverNodeTask with claim details for display.
type changeoverNodeView struct {
	domain.NodeTask
	ProcessID      int64              `json:"process_id"`
	Situation      string             `json:"situation"`
	FromPayload    string             `json:"from_payload"`
	ToPayload      string             `json:"to_payload"`
	FromRole       protocol.ClaimRole `json:"from_role"`
	ToRole         protocol.ClaimRole `json:"to_role"`
	LastOrderError string             `json:"last_order_error,omitempty"`
}

// styleSourcingView is the changeover picker's per-style sourceability
// annotation, keyed by style name. Status is the gated verdict; Blocked marks a
// red style shown-but-unselectable; Note is the short operator hint (the missing
// payloads for red, the running-low payloads for yellow).
type styleSourcingView struct {
	Status  string
	Blocked bool
	Note    string
}

func styleSourcingViewFrom(s protocol.SourcingState) styleSourcingView {
	v := styleSourcingView{Status: s.Status}
	switch s.Status {
	case "not_configured":
		// No claims means no verdict, so the picker cannot offer it. Blocked for
		// the same reason as red — unselectable — but the note says "not set up"
		// rather than naming payloads, because none are configured to name.
		v.Blocked = true
		v.Status = "not set up"
		v.Note = "no sourceability claims"
	case "red":
		v.Blocked = true
		if len(s.Missing) > 0 {
			v.Note = "missing " + strings.Join(s.Missing, ", ")
		}
	case "yellow":
		payloads := make([]string, 0, len(s.AtRisk))
		for _, r := range s.AtRisk {
			payloads = append(payloads, r.PayloadCode)
		}
		if len(payloads) > 0 {
			v.Note = "low: " + strings.Join(payloads, ", ")
		}
	case "green":
		// Selectable, no note — the only verdict that is affirmatively fine.
	default:
		// Fail CLOSED. Status is an open string on the wire, so an Edge running
		// against a newer Core can receive a verdict it does not know — exactly
		// what happened when not_configured was added. An unrecognised verdict
		// is not permission to change over, so it blocks rather than falling
		// through to selectable as it used to.
		v.Blocked = true
		v.Note = "unrecognised sourcing verdict — update Edge"
	}
	return v
}

// changeoverViewData holds the common data loaded for changeover views.
type changeoverViewData struct {
	Styles           []domain.Style
	CurrentStyleName string
	ActiveChangeover *domain.Changeover
	StationTasks     []domain.StationTask
	NodeTaskMap      map[int64][]changeoverNodeView
	CentralNodeTasks []changeoverNodeView
	AllNodesComplete bool
	// SourcingByStyle annotates each selectable style with its sourceability
	// verdict (keyed by style name). Read from Edge's local cache — no Core
	// round-trip. Empty when the feed has no verdict for a style (no annotation).
	SourcingByStyle map[string]styleSourcingView
}

func (h *Handlers) buildChangeoverViewData(activeProcess *domain.Process) changeoverViewData {
	var d changeoverViewData
	d.NodeTaskMap = map[int64][]changeoverNodeView{}

	if activeProcess == nil {
		d.AllNodesComplete = true
		return d
	}

	d.Styles, _ = h.engine.StyleService().ListByProcess(activeProcess.ID)
	// Annotate each style with its sourceability verdict from Edge's local cache
	// (the picker shows green/yellow selectable, red shown-but-blocked).
	d.SourcingByStyle = map[string]styleSourcingView{}
	for _, s := range h.engine.SourcingStateForProcess(activeProcess.Name) {
		d.SourcingByStyle[s.StyleID] = styleSourcingViewFrom(s)
	}
	if activeProcess.ActiveStyleID != nil {
		if s, err := h.engine.StyleService().Get(*activeProcess.ActiveStyleID); err == nil {
			d.CurrentStyleName = s.Name
		}
	}
	d.ActiveChangeover, _ = h.engine.ChangeoverService().GetActive(activeProcess.ID)
	if d.ActiveChangeover != nil {
		d.StationTasks, _ = h.engine.ChangeoverService().ListStationTasks(d.ActiveChangeover.ID)
		allNodeTasks, _ := h.engine.ChangeoverService().ListNodeTasks(d.ActiveChangeover.ID)

		// Build map of processNodeID -> operatorStationID from process nodes
		processNodes, _ := h.engine.ProcessService().ListNodesByProcess(activeProcess.ID)
		nodeStationMap := make(map[int64]*int64, len(processNodes))
		for i := range processNodes {
			nodeStationMap[processNodes[i].ID] = processNodes[i].OperatorStationID
		}

		for _, task := range allNodeTasks {
			view := changeoverNodeView{
				NodeTask:  task,
				ProcessID: activeProcess.ID,
				Situation: task.Situation,
			}
			if task.FromClaimID != nil {
				if claim, err := h.engine.StyleService().GetClaim(*task.FromClaimID); err == nil {
					view.FromPayload = claim.PayloadCode
					view.FromRole = claim.Role
				}
			}
			if task.ToClaimID != nil {
				if claim, err := h.engine.StyleService().GetClaim(*task.ToClaimID); err == nil {
					view.ToPayload = claim.PayloadCode
					view.ToRole = claim.Role
				}
			}
			// Check linked orders for failures
			for _, orderID := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
				if orderID != nil {
					if o, err := h.engine.OrderService().Get(*orderID); err == nil && o.Status == protocol.StatusFailed {
						view.LastOrderError = "Order " + o.UUID[:8] + " failed"
					}
				}
			}
			stationID := nodeStationMap[task.ProcessNodeID]
			if stationID != nil {
				d.NodeTaskMap[*stationID] = append(d.NodeTaskMap[*stationID], view)
			} else {
				d.CentralNodeTasks = append(d.CentralNodeTasks, view)
			}
		}
	}

	d.AllNodesComplete = true
	for _, tasks := range d.NodeTaskMap {
		for _, t := range tasks {
			if !domain.IsNodeTaskStateTerminal(t.State, t.Situation) {
				d.AllNodesComplete = false
			}
		}
	}
	for _, t := range d.CentralNodeTasks {
		if !domain.IsNodeTaskStateTerminal(t.State, t.Situation) {
			d.AllNodesComplete = false
		}
	}

	return d
}

func (h *Handlers) handleChangeover(w http.ResponseWriter, r *http.Request) {
	processList, _ := h.engine.ProcessService().List()
	activeProcess := resolveProcessFromQuery(r, processList)

	d := h.buildChangeoverViewData(activeProcess)

	var activeProcessID int64
	if activeProcess != nil {
		activeProcessID = activeProcess.ID
	}

	var changeoverHistory []domain.Changeover
	if activeProcess != nil {
		changeoverHistory, _ = h.engine.ChangeoverService().List(activeProcess.ID)
		// Filter out the active changeover from history
		filtered := changeoverHistory[:0]
		for _, c := range changeoverHistory {
			if d.ActiveChangeover == nil || c.ID != d.ActiveChangeover.ID {
				filtered = append(filtered, c)
			}
		}
		changeoverHistory = filtered
	}

	anomalies, rpMap := loadAnomalyData(h)
	data := map[string]any{
		"Page":              "changeover",
		"Processes":         processList,
		"ActiveProcess":     activeProcess,
		"ActiveProcessID":   activeProcessID,
		"Styles":            d.Styles,
		"CurrentStyle":      d.CurrentStyleName,
		"ActiveChangeover":  d.ActiveChangeover,
		"StationTasks":      d.StationTasks,
		"NodeTaskMap":       d.NodeTaskMap,
		"CentralNodeTasks":  d.CentralNodeTasks,
		"AllNodesComplete":  d.AllNodesComplete,
		"SourcingByStyle":   d.SourcingByStyle,
		"ChangeoverHistory": changeoverHistory,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
	}
	h.renderTemplate(w, r, "changeover.html", data)
}

func (h *Handlers) handleChangeoverPartial(w http.ResponseWriter, r *http.Request) {
	processList, _ := h.engine.ProcessService().List()
	activeProcess := resolveProcessFromQuery(r, processList)

	d := h.buildChangeoverViewData(activeProcess)

	var activeProcessID int64
	if activeProcess != nil {
		activeProcessID = activeProcess.ID
	}

	data := map[string]any{
		"ActiveProcess":    activeProcess,
		"ActiveProcessID":  activeProcessID,
		"Styles":           d.Styles,
		"CurrentStyle":     d.CurrentStyleName,
		"ActiveChangeover": d.ActiveChangeover,
		"StationTasks":     d.StationTasks,
		"NodeTaskMap":      d.NodeTaskMap,
		"CentralNodeTasks": d.CentralNodeTasks,
		"AllNodesComplete": d.AllNodesComplete,
		"SourcingByStyle":  d.SourcingByStyle,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "changeover-body", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
