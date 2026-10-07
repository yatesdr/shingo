package www

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"shingo/protocol/auth"
	"shingo/shared"
	"shingoedge/domain"
)

// handleConfig renders the Configuration page (U3): one column of sections,
// one Save. Everything the page needs at load is in the render — the live
// words in each section title, the restart notice, the backup status — so a
// load makes no API request of its own (E6: the backup list is fetched only
// when storage is configured and the list is opened).
func (h *Handlers) handleConfig(w http.ResponseWriter, r *http.Request) {
	cfg := h.engine.AppConfig().Clone()
	mgr := h.engine.PLCManager()

	plcNames := mgr.PLCNames()
	plcStatus := make(map[string]bool)
	plcStatuses := mgr.PLCStatuses()
	connectedPLCs := 0
	for _, name := range plcNames {
		plcStatus[name] = plcStatuses[name] == "Connected"
		if plcStatus[name] {
			connectedPLCs++
		}
	}
	warLinkConnected := mgr.IsWarLinkConnected()

	shiftList, _ := h.engine.ShiftService().List()
	if shiftList == nil {
		shiftList = []domain.Shift{}
	}

	// PLC link title. E4: in sim the apply is a deliberate no-op, so the
	// words say "simulated" rather than a link state the save cannot change.
	plcLive, plcLiveClass := "", ""
	switch {
	case cfg.Sim.Enabled:
		plcLive = "simulated"
	case !cfg.WarLink.Enabled:
		plcLive = "off"
	case warLinkConnected:
		plcLive, plcLiveClass = fmt.Sprintf("connected · %d PLCs", len(plcNames)), "ok"
	default:
		plcLive, plcLiveClass = "not connected", "warn"
	}

	// Messaging title, when the engine can say (the real one can; handler
	// test stubs cannot, and then the title says nothing).
	msgLive, msgLiveClass := "", ""
	if len(cfg.Messaging.Kafka.Brokers) == 0 {
		msgLive, msgLiveClass = "no brokers", "warn"
	} else if se, ok := h.orchestration.(interface{ KafkaConnected() bool }); ok {
		if se.KafkaConnected() {
			msgLive, msgLiveClass = "connected", "ok"
		} else {
			msgLive, msgLiveClass = "not connected", "warn"
		}
	}

	backupStatus := map[string]any{"available": false}
	if h.backup != nil {
		st := h.backup.Status()
		raw, _ := json.Marshal(st)
		_ = json.Unmarshal(raw, &backupStatus)
		backupStatus["available"] = true
	}
	backupJSON, _ := json.Marshal(backupStatus)
	restartJSON, _ := json.Marshal(h.restartPending())

	data := map[string]any{
		"Page":               "config",
		"Config":             cfg,
		"StationID":          cfg.StationID(),
		"DisplayZone":        plantLocation.String(),
		"PLCNames":           plcNames,
		"PLCStatus":          plcStatus,
		"PLCConnected":       connectedPLCs,
		"PLCLive":            plcLive,
		"PLCLiveClass":       plcLiveClass,
		"PollRateText":       durationText(cfg.WarLink.PollRate),
		"MessagingLive":      msgLive,
		"MessagingLiveClass": msgLiveClass,
		"Shifts":             shiftList,
		"BackupIntervalText": durationText(cfg.Backup.ScheduleInterval),
		"SecretSaved":        cfg.Backup.S3.SecretKey != "",
		"BackupStatusJSON":   string(backupJSON),
		"RestartJSON":        string(restartJSON),
	}
	h.renderTemplate(w, r, "config.html", data)
}

// durationText is a stored duration as the settings page shows it: one box
// with its unit in it ("45 min", "10 s", "1 h 30 min"), never "45m0s"
// (§33 rule 5). The Go twin of durationToText in shared/utils.js, used so the
// first paint already reads that way; the page sends it back through
// durationFromText.
func durationText(d time.Duration) string {
	if d == 0 {
		return "0 s"
	}
	if d < 0 {
		return d.String()
	}
	var parts []string
	h := d / time.Hour
	m := (d % time.Hour) / time.Minute
	s := (d % time.Minute) / time.Second
	ms := (d % time.Second) / time.Millisecond
	if h > 0 {
		parts = append(parts, fmt.Sprintf("%d h", h))
	}
	if m > 0 {
		parts = append(parts, fmt.Sprintf("%d min", m))
	}
	if s > 0 {
		parts = append(parts, fmt.Sprintf("%d s", s))
	}
	if ms > 0 {
		parts = append(parts, fmt.Sprintf("%d ms", ms))
	}
	if len(parts) == 0 {
		return d.String()
	}
	return strings.Join(parts, " ")
}

func (h *Handlers) handleProcesses(w http.ResponseWriter, r *http.Request) {
	processList, _ := h.engine.ProcessService().List()
	groupList, _ := h.engine.ProcessService().ListGroups()
	styles, _ := h.engine.StyleService().List()
	stationList, _ := h.engine.StationService().List()
	coreNodes := h.engine.CoreNodes()
	plcNames := h.engine.PLCManager().PLCNames()

	var activeProcess *domain.Process
	if processParam := r.URL.Query().Get("process"); processParam != "" {
		if processID, err := strconv.ParseInt(processParam, 10, 64); err == nil {
			for i := range processList {
				if processList[i].ID == processID {
					activeProcess = &processList[i]
					break
				}
			}
		}
	}
	if activeProcess == nil && len(processList) > 0 {
		activeProcess = &processList[0]
	}

	var activeProcessID int64
	var processStations []domain.Station
	var processNodes []domain.Node
	stationNodeMap := map[int64][]string{}
	if activeProcess != nil {
		activeProcessID = activeProcess.ID
		processStations, _ = h.engine.StationService().ListByProcess(activeProcess.ID)
		processNodes, _ = h.engine.ProcessService().ListNodesByProcess(activeProcess.ID)
	}

	// Derive station→nodes map and claimed-by index from already-fetched processNodes
	stationNameMap := map[int64]string{}
	for _, s := range processStations {
		stationNameMap[s.ID] = s.Name
	}
	claimedByStation := map[string]any{}
	for _, n := range processNodes {
		if n.OperatorStationID == nil {
			continue
		}
		sid := *n.OperatorStationID
		// Stale FK: process_node points at a station that no longer exists
		// (deleted screen left the FK behind). Don't grey out the node in
		// the picker for a phantom owner — clear the dangling pointer and
		// treat the node as unclaimed.
		if _, ok := stationNameMap[sid]; !ok {
			in := domain.NodeInput{
				ProcessID:         n.ProcessID,
				OperatorStationID: nil,
				CoreNodeName:      n.CoreNodeName,
				Code:              n.Code,
				Name:              n.Name,
				Sequence:          n.Sequence,
				Enabled:           n.Enabled,
			}
			if err := h.engine.ProcessService().UpdateNode(n.ID, in); err != nil {
				log.Printf("clear stale operator_station_id on node %d: %v", n.ID, err)
			}
			continue
		}
		stationNodeMap[sid] = append(stationNodeMap[sid], n.CoreNodeName)
		claimedByStation[n.CoreNodeName] = map[string]any{
			"id":   sid,
			"name": stationNameMap[sid],
		}
	}

	// Line positions of the active process, by core node name. The claim
	// editor's pickers rank by this: a paired press position is almost always
	// a node on this process, and a supermarket almost never is. RANK, not
	// filter — a dedicated loader's home position is both a process_node and a
	// legitimate InboundSource (source_finder tier 2), so hiding either side
	// would hide a supported configuration.
	processNodeNames := make([]string, 0, len(processNodes))
	for _, n := range processNodes {
		processNodeNames = append(processNodeNames, n.CoreNodeName)
	}

	// Core loaders this edge draws no screen for. Read failure is logged and
	// left empty rather than failing the page: this is an advisory panel, and a
	// config page that will not load is worse than one missing an advisory.
	loaderBoardGaps, err := h.engine.StationService().LoaderBoardGaps()
	if err != nil {
		log.Printf("loader board gaps: %v", err)
	}

	data := map[string]any{
		"LoaderBoardGaps":  loaderBoardGaps,
		"Page":             "processes",
		"ProcessNodeNames": processNodeNames,
		"Processes":        processList,
		"ProcessGroups":    groupList,
		"Styles":           styles,
		"Stations":         stationList,
		"CoreNodes":        coreNodes,
		"PLCNames":         plcNames,
		"ActiveProcess":    activeProcess,
		"ActiveProcessID":  activeProcessID,
		"ProcessStations":  processStations,
		"ProcessNodes":     processNodes,
		"StationNodeMap":   stationNodeMap,
		"ClaimedByStation": claimedByStation,
	}
	h.renderTemplate(w, r, "processes.html", data)
}

func (h *Handlers) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	next := shared.SafeNextPath(r.URL.Query().Get("next"))
	if username, ok := h.sessions.getUser(r); ok && username != "" {
		dest := next
		if dest == "" {
			dest = "/config"
		}
		http.Redirect(w, r, dest, http.StatusSeeOther)
		return
	}
	h.renderTemplate(w, r, "login.html", map[string]any{
		"Page": "login",
		"Next": next,
	})
}

func (h *Handlers) handleLogin(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	password := r.FormValue("password")
	next := shared.SafeNextPath(r.FormValue("next"))
	if next == "" {
		next = shared.SafeNextPath(r.URL.Query().Get("next"))
	}
	dest := next
	if dest == "" {
		dest = "/config"
	}

	exists, _ := h.engine.AdminService().Exists()
	if !exists {
		hash, err := auth.HashPassword(password)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if _, err := h.engine.AdminService().Create(username, hash); err != nil {
			http.Error(w, "failed to create admin user", http.StatusInternalServerError)
			return
		}
		h.sessions.setUser(w, r, username)
		http.Redirect(w, r, dest, http.StatusSeeOther)
		return
	}

	user, err := h.engine.AdminService().Get(username)
	if err != nil || !auth.CheckPassword(user.PasswordHash, password) {
		h.renderTemplate(w, r, "login.html", map[string]any{
			"Page":  "login",
			"Error": "Invalid username or password",
			"Next":  next,
		})
		return
	}

	h.sessions.setUser(w, r, username)
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (h *Handlers) handleLogout(w http.ResponseWriter, r *http.Request) {
	h.sessions.clear(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
