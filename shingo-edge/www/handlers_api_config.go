package www

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
	"shingo/protocol/auth"
	"shingoedge/store"
)

// --- Counter Anomalies ---

func (h *Handlers) apiConfirmAnomaly(w http.ResponseWriter, r *http.Request) {
	snapshotID, err := parseID(r, "snapshotID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid snapshot ID")
		return
	}
	if err := h.engine.ConfirmAnomaly(snapshotID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDismissAnomaly(w http.ResponseWriter, r *http.Request) {
	snapshotID, err := parseID(r, "snapshotID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid snapshot ID")
		return
	}
	if err := h.engine.DismissAnomaly(snapshotID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// --- PLC / WarLink ---

func (h *Handlers) apiListPLCs(w http.ResponseWriter, r *http.Request) {
	mgr := h.engine.PLCManager()
	type plcInfo struct {
		Name      string `json:"name"`
		Status    string `json:"status"`
		Connected bool   `json:"connected"`
	}
	names := mgr.PLCNames()
	result := make([]plcInfo, len(names))
	for i, name := range names {
		mp := mgr.GetPLC(name)
		status := "Unknown"
		if mp != nil {
			status = mp.Status
		}
		result[i] = plcInfo{Name: name, Status: status, Connected: mgr.IsConnected(name)}
	}
	writeJSON(w, result)
}

func (h *Handlers) apiWarLinkStatus(w http.ResponseWriter, r *http.Request) {
	mgr := h.engine.PLCManager()
	cfg := h.engine.AppConfig()
	errStr := ""
	if err := mgr.WarLinkError(); err != nil {
		errStr = err.Error()
	}
	mode := cfg.WarLink.Mode
	if mode == "" {
		mode = "sse"
	}
	writeJSON(w, map[string]interface{}{
		"connected": mgr.IsWarLinkConnected(),
		"host":      cfg.WarLink.Host,
		"port":      cfg.WarLink.Port,
		"enabled":   cfg.WarLink.Enabled,
		"mode":      mode,
		"error":     errStr,
	})
}

func (h *Handlers) apiUpdateWarLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		PollRate string `json:"poll_rate"`
		Enabled  bool   `json:"enabled"`
		Mode     string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.Mode != "" && req.Mode != "poll" && req.Mode != "sse" {
		writeError(w, http.StatusBadRequest, "mode must be \"poll\" or \"sse\"")
		return
	}

	cfg := h.engine.AppConfig()
	cfg.Lock()
	if req.Host != "" {
		cfg.WarLink.Host = req.Host
	}
	if req.Port > 0 {
		cfg.WarLink.Port = req.Port
	}
	if req.PollRate != "" {
		d, err := time.ParseDuration(req.PollRate)
		if err != nil {
			cfg.Unlock()
			writeError(w, http.StatusBadRequest, "invalid poll_rate: "+err.Error())
			return
		}
		cfg.WarLink.PollRate = d
	}
	cfg.WarLink.Enabled = req.Enabled
	if req.Mode != "" {
		cfg.WarLink.Mode = req.Mode
	}
	cfg.Unlock()

	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.engine.ApplyWarLinkConfig()

	h.requestBackup("warlink-config")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiPLCTags(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	tags, err := h.engine.PLCManager().DiscoverTags(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, tags)
}

func (h *Handlers) apiPLCAllTags(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	tags, err := h.engine.PLCManager().FetchAllTags(ctx, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, tags)
}

func (h *Handlers) apiReadTag(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PLCName string `json:"plc_name"`
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	val, err := h.engine.PLCManager().ReadTag(req.PLCName, req.TagName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"value": val})
}

// --- Reporting Points Admin ---

func (h *Handlers) apiListReportingPoints(w http.ResponseWriter, r *http.Request) {
	rps, err := h.engine.ListReportingPoints()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, rps)
}

func (h *Handlers) apiCreateReportingPoint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PLCName string `json:"plc_name"`
		TagName string `json:"tag_name"`
		StyleID int64  `json:"style_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := h.engine.CreateReportingPoint(req.PLCName, req.TagName, req.StyleID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.engine.EnsureTagPublished(id, req.PLCName, req.TagName)

	h.requestBackup("reporting-point-created")
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateReportingPoint(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		PLCName string `json:"plc_name"`
		TagName string `json:"tag_name"`
		StyleID int64  `json:"style_id"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	oldRP, _ := h.engine.GetReportingPoint(id)

	if err := h.engine.UpdateReportingPoint(id, req.PLCName, req.TagName, req.StyleID, req.Enabled); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if oldRP != nil {
		h.engine.ManageReportingPointTag(id, oldRP.PLCName, oldRP.TagName, oldRP.WarlinkManaged, req.PLCName, req.TagName)
	}

	h.requestBackup("reporting-point-updated")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteReportingPoint(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}

	rp, _ := h.engine.GetReportingPoint(id)

	if err := h.engine.DeleteReportingPoint(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if rp != nil {
		h.engine.CleanupReportingPointTag(id, rp.PLCName, rp.TagName, rp.WarlinkManaged)
	}

	h.requestBackup("reporting-point-deleted")
	writeJSON(w, map[string]string{"status": "ok"})
}

// --- Styles Admin ---

func (h *Handlers) apiListStyles(w http.ResponseWriter, r *http.Request) {
	styles, err := h.engine.ListStyles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, styles)
}

func (h *Handlers) apiCreateStyle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		ProcessID   int64  `json:"process_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ProcessID == 0 {
		writeError(w, http.StatusBadRequest, "process_id is required")
		return
	}
	id, err := h.engine.CreateStyle(req.Name, req.Description, req.ProcessID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("style-created")
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateStyle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		ProcessID   int64  `json:"process_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ProcessID == 0 {
		writeError(w, http.StatusBadRequest, "process_id is required")
		return
	}
	if err := h.engine.UpdateStyle(id, req.Name, req.Description, req.ProcessID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("style-updated")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteStyle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	if err := h.engine.DeleteStyle(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("style-deleted")
	writeJSON(w, map[string]string{"status": "ok"})
}

// --- Core Nodes ---

func (h *Handlers) apiGetCoreNodes(w http.ResponseWriter, r *http.Request) {
	nodes := h.engine.CoreNodes()
	infos := make([]protocol.NodeInfo, 0, len(nodes))
	for _, n := range nodes {
		infos = append(infos, n)
	}
	writeJSON(w, infos)
}

func (h *Handlers) apiSyncCoreNodes(w http.ResponseWriter, r *http.Request) {
	h.engine.RequestNodeSync()
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiListPayloadCatalog(w http.ResponseWriter, r *http.Request) {
	entries, err := h.engine.ListPayloadCatalog()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, entries)
}

func (h *Handlers) apiSyncPayloadCatalog(w http.ResponseWriter, r *http.Request) {
	h.engine.RequestCatalogSync()
	writeJSON(w, map[string]string{"status": "ok"})
}

// --- Processes Admin ---

func (h *Handlers) apiListProcesses(w http.ResponseWriter, r *http.Request) {
	processes, err := h.engine.ListProcesses()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, processes)
}

func (h *Handlers) apiCreateProcess(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name            string `json:"name"`
		Description     string `json:"description"`
		ProductionState string `json:"production_state"`
		CounterPLCName  string `json:"counter_plc_name"`
		CounterTagName  string `json:"counter_tag_name"`
		CounterEnabled  bool   `json:"counter_enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	id, err := h.engine.CreateProcess(req.Name, req.Description, req.ProductionState, req.CounterPLCName, req.CounterTagName, req.CounterEnabled)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("process-created")
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateProcess(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Name            string `json:"name"`
		Description     string `json:"description"`
		ProductionState string `json:"production_state"`
		CounterPLCName  string `json:"counter_plc_name"`
		CounterTagName  string `json:"counter_tag_name"`
		CounterEnabled  bool   `json:"counter_enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.UpdateProcess(id, req.Name, req.Description, req.ProductionState, req.CounterPLCName, req.CounterTagName, req.CounterEnabled); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Re-sync the reporting point so the counter config edit takes effect
	// immediately — previously, this only ran on SetActiveStyle, which meant
	// that adding/changing counter fields on an already-active process
	// silently did nothing until the style was re-activated. The sync is a
	// no-op when counter config or active style is missing, so it's safe to
	// call unconditionally. Log-and-continue on error: the process update
	// itself succeeded and we don't want to fail the whole request if the
	// secondary sync hits a transient issue.
	if err := h.engine.SyncProcessCounter(id); err != nil {
		log.Printf("sync reporting point after process %d update: %v", id, err)
	}
	h.requestBackup("process-updated")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteProcess(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	if err := h.engine.DeleteProcess(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("process-deleted")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiSetActiveStyle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		StyleID *int64 `json:"style_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.SetActiveStyle(id, req.StyleID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.engine.SyncProcessCounter(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("active-style-updated")
	h.eventHub.Broadcast(SSEEvent{Type: "material-refresh", Data: map[string]string{"action": "active-style-changed"}})
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiListProcessStyles(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	styles, err := h.engine.ListStylesByProcess(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, styles)
}



// --- Style Node Claims ---

func (h *Handlers) apiListStyleNodeClaims(w http.ResponseWriter, r *http.Request) {
	styleID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid style id")
		return
	}
	claims, err := h.engine.ListStyleNodeClaims(styleID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, claims)
}

func (h *Handlers) apiUpsertStyleNodeClaim(w http.ResponseWriter, r *http.Request) {
	var in store.StyleNodeClaimInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.StyleID == 0 {
		writeError(w, http.StatusBadRequest, "style_id is required")
		return
	}
	if in.CoreNodeName == "" {
		writeError(w, http.StatusBadRequest, "core_node_name is required")
		return
	}
	// Consume-role claims only pay off on LANE-parented storage slots —
	// wiring_kanban.go only emits "consume" demand signals when a bin
	// arrives at a child-of-LANE node (see isStorageSlot). Rejecting
	// here keeps operators from silently configuring dead claims.
	// Produce-role claims are unconstrained: the departure check in
	// wiring_kanban is intentionally LANE-gated too, but producers
	// also have lineside trigger points that are legitimately
	// non-storage (the loader emits fulls back into the supermarket),
	// so leave produce permissive and only guardrail consume.
	if in.Role == "consume" {
		if info, ok := h.engine.CoreNodes()[in.CoreNodeName]; ok {
			// Empty ParentNodeType means an older Core that hasn't
			// been upgraded yet — skip the check rather than false-
			// reject on a missing field so rolling upgrades work.
			if info.ParentNodeType != "" && info.ParentNodeType != "LANE" {
				writeError(w, http.StatusBadRequest, fmt.Sprintf(
					"consume claims require a LANE-parented storage slot; %s is parented by %s",
					in.CoreNodeName, info.ParentNodeType))
				return
			}
		}
	}
	id, err := h.engine.UpsertStyleNodeClaim(in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("style-node-claim-updated")
	h.eventHub.Broadcast(SSEEvent{Type: "material-refresh", Data: map[string]string{"action": "node-claim-updated"}})
	// Push the refreshed claim set to Core so demand_registry stays in sync
	// with what the operator just edited. Fire-and-forget — SendClaimSync
	// logs its own failures and the outbox will retry transient send errors.
	go h.engine.SendClaimSync()
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiDeleteStyleNodeClaim(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.engine.DeleteStyleNodeClaim(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("style-node-claim-deleted")
	h.eventHub.Broadcast(SSEEvent{Type: "material-refresh", Data: map[string]string{"action": "node-claim-deleted"}})
	// Claim removed → push the refreshed (shorter) claim set to Core so
	// demand_registry drops the corresponding row. Without this push the
	// registry drifts and Core keeps sending demand signals to a node
	// whose claim is gone.
	go h.engine.SendClaimSync()
	writeJSON(w, map[string]string{"status": "ok"})
}

// --- Core API ---

func (h *Handlers) apiUpdateCoreAPI(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CoreAPI string `json:"core_api"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.CoreAPI = req.CoreAPI
	cfg.Unlock()
	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiTestCoreAPI(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CoreAPI string `json:"core_api"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.CoreAPI == "" {
		writeJSON(w, map[string]interface{}{"connected": false, "error": "no URL"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, "GET", req.CoreAPI+"/api/health", nil)
	if err != nil {
		writeJSON(w, map[string]interface{}{"connected": false, "error": err.Error()})
		return
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		writeJSON(w, map[string]interface{}{"connected": false, "error": err.Error()})
		return
	}
	resp.Body.Close()
	writeJSON(w, map[string]interface{}{"connected": resp.StatusCode < 500})
}

// --- Config Admin ---

func (h *Handlers) apiUpdateMessaging(w http.ResponseWriter, r *http.Request) {
	var req struct {
		KafkaBrokers []string `json:"kafka_brokers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.Messaging.Kafka.Brokers = req.KafkaBrokers
	cfg.Unlock()

	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := h.engine.ReconnectKafka(); err != nil {
		log.Printf("kafka reconnect after config update: %v", err)
	}

	h.requestBackup("messaging-config")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiUpdateStationID(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StationID string `json:"station_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.Messaging.StationID = req.StationID
	cfg.Unlock()

	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("station-id")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiTestKafka(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Broker string `json:"broker"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Broker == "" {
		writeError(w, http.StatusBadRequest, "broker address required")
		return
	}
	conn, err := net.DialTimeout("tcp", req.Broker, 5*time.Second)
	if err != nil {
		writeJSON(w, map[string]interface{}{"connected": false, "error": err.Error()})
		return
	}
	conn.Close()
	writeJSON(w, map[string]interface{}{"connected": true})
}

func (h *Handlers) apiUpdateAutoConfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AutoConfirm bool `json:"auto_confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.Web.AutoConfirm = req.AutoConfirm
	cfg.Unlock()

	if err := cfg.Save(h.engine.ConfigPath()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("auto-confirm")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiChangePassword(w http.ResponseWriter, r *http.Request) {
	username, ok := h.sessions.getUser(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "not logged in")
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	user, err := h.engine.GetAdminUser(username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "user not found")
		return
	}

	if !auth.CheckPassword(user.PasswordHash, req.OldPassword) {
		writeError(w, http.StatusBadRequest, "current password is incorrect")
		return
	}

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}

	if err := h.engine.UpdateAdminPassword(username, hash); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to update password: %v", err))
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}
