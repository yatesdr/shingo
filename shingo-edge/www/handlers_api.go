package www

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"

	"shingoedge/orders"

	"github.com/go-chi/chi/v5"
)

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func parseID(r *http.Request, param string) (int64, error) {
	s := chi.URLParam(r, param)
	return strconv.ParseInt(s, 10, 64)
}

// --- Order Operations ---

func (h *Handlers) apiConfirmDelivery(w http.ResponseWriter, r *http.Request) {
	orderID, err := parseID(r, "orderID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid order ID")
		return
	}
	var req struct {
		FinalCount float64 `json:"final_count"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	if err := h.engine.OrderManager().ConfirmDelivery(orderID, req.FinalCount); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiCreateRetrieveOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayloadID     int64   `json:"payload_id"`
		RetrieveEmpty bool    `json:"retrieve_empty"`
		Quantity      float64 `json:"quantity"`
		DeliveryNode  string  `json:"delivery_node"`
		StagingNode   string  `json:"staging_node"`
		LoadType      string  `json:"load_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var payloadID *int64
	if req.PayloadID > 0 {
		payloadID = &req.PayloadID
		// Auto-fill from payload if not specified
		if p, err := h.engine.DB().GetPayload(req.PayloadID); err == nil {
			if req.DeliveryNode == "" {
				req.DeliveryNode = p.Location
			}
			if req.StagingNode == "" {
				req.StagingNode = p.StagingNode
			}
			req.RetrieveEmpty = p.RetrieveEmpty
		}
	}

	order, err := h.engine.OrderManager().CreateRetrieveOrder(
		payloadID, req.RetrieveEmpty,
		req.Quantity, req.DeliveryNode, req.StagingNode, req.LoadType,
		h.engine.AppConfig().Web.AutoConfirm,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, order)
}

func (h *Handlers) apiCreateStoreOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayloadID  int64   `json:"payload_id"`
		Quantity   float64 `json:"quantity"`
		PickupNode string  `json:"pickup_node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var payloadID *int64
	if req.PayloadID > 0 {
		payloadID = &req.PayloadID
		if p, err := h.engine.DB().GetPayload(req.PayloadID); err == nil {
			if req.PickupNode == "" {
				req.PickupNode = p.Location
			}
		}
	}

	order, err := h.engine.OrderManager().CreateStoreOrder(
		payloadID, req.Quantity, req.PickupNode,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, order)
}

func (h *Handlers) apiCreateMoveOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayloadID    int64   `json:"payload_id"`
		Quantity     float64 `json:"quantity"`
		PickupNode   string  `json:"pickup_node"`
		DeliveryNode string  `json:"delivery_node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var payloadID *int64
	if req.PayloadID > 0 {
		payloadID = &req.PayloadID
		if p, err := h.engine.DB().GetPayload(req.PayloadID); err == nil {
			if req.PickupNode == "" {
				req.PickupNode = p.Location
			}
		}
	}

	order, err := h.engine.OrderManager().CreateMoveOrder(
		payloadID, req.Quantity, req.PickupNode, req.DeliveryNode,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, order)
}

func (h *Handlers) apiSubmitOrder(w http.ResponseWriter, r *http.Request) {
	orderID, err := parseID(r, "orderID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid order ID")
		return
	}
	if err := h.engine.OrderManager().SubmitOrder(orderID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiCancelOrder(w http.ResponseWriter, r *http.Request) {
	orderID, err := parseID(r, "orderID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid order ID")
		return
	}
	if err := h.engine.OrderManager().TransitionOrder(orderID, orders.StatusCancelled, "cancelled by operator"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiSetOrderCount(w http.ResponseWriter, r *http.Request) {
	orderID, err := parseID(r, "orderID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid order ID")
		return
	}
	var req struct {
		FinalCount float64 `json:"final_count"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.DB().UpdateOrderFinalCount(orderID, req.FinalCount, true); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiUpdateReorderPoint(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		ReorderPoint int `json:"reorder_point"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ReorderPoint < 0 {
		writeError(w, http.StatusBadRequest, "reorder_point must be >= 0")
		return
	}
	if err := h.engine.DB().UpdatePayloadReorderPoint(id, req.ReorderPoint); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiToggleAutoReorder(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.DB().UpdatePayloadAutoReorder(id, req.Enabled); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiAbortOrder(w http.ResponseWriter, r *http.Request) {
	orderID, err := parseID(r, "orderID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid order ID")
		return
	}
	if err := h.engine.OrderManager().AbortOrder(orderID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiRedirectOrder(w http.ResponseWriter, r *http.Request) {
	orderID, err := parseID(r, "orderID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid order ID")
		return
	}
	var req struct {
		DeliveryNode string `json:"delivery_node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.DeliveryNode == "" {
		writeError(w, http.StatusBadRequest, "delivery_node is required")
		return
	}
	order, err := h.engine.OrderManager().RedirectOrder(orderID, req.DeliveryNode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, order)
}

// --- Counter Anomalies ---

func (h *Handlers) apiConfirmAnomaly(w http.ResponseWriter, r *http.Request) {
	snapshotID, err := parseID(r, "snapshotID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid snapshot ID")
		return
	}
	if err := h.engine.DB().ConfirmAnomaly(snapshotID); err != nil {
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
	if err := h.engine.DB().DismissAnomaly(snapshotID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// --- Changeover ---

func (h *Handlers) apiChangeoverStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LineID       int64  `json:"line_id"`
		FromJobStyle string `json:"from_job_style"`
		ToJobStyle   string `json:"to_job_style"`
		Operator     string `json:"operator"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.LineID == 0 {
		writeError(w, http.StatusBadRequest, "line_id is required")
		return
	}
	m := h.engine.ChangeoverMachine(req.LineID)
	if m == nil {
		writeError(w, http.StatusNotFound, "production line not found")
		return
	}
	if err := m.Start(req.FromJobStyle, req.ToJobStyle, req.Operator); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiChangeoverAdvance(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LineID   int64  `json:"line_id"`
		Operator string `json:"operator"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.LineID == 0 {
		writeError(w, http.StatusBadRequest, "line_id is required")
		return
	}
	m := h.engine.ChangeoverMachine(req.LineID)
	if m == nil {
		writeError(w, http.StatusNotFound, "production line not found")
		return
	}
	if err := m.Advance(req.Operator); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiChangeoverCancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LineID int64 `json:"line_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.LineID == 0 {
		writeError(w, http.StatusBadRequest, "line_id is required")
		return
	}
	m := h.engine.ChangeoverMachine(req.LineID)
	if m == nil {
		writeError(w, http.StatusNotFound, "production line not found")
		return
	}
	if err := m.Cancel(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
	rps, err := h.engine.DB().ListReportingPoints()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, rps)
}

func (h *Handlers) apiCreateReportingPoint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PLCName    string `json:"plc_name"`
		TagName    string `json:"tag_name"`
		JobStyleID int64  `json:"job_style_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := h.engine.DB().CreateReportingPoint(req.PLCName, req.TagName, req.JobStyleID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Auto-enable tag in WarLink if not already published
	mgr := h.engine.PLCManager()
	if !mgr.IsTagPublished(req.PLCName, req.TagName) {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := mgr.EnableTagPublishing(ctx, req.PLCName, req.TagName); err != nil {
			log.Printf("warlink: auto-enable %s/%s failed (RP %d created): %v", req.PLCName, req.TagName, id, err)
		} else {
			h.engine.DB().SetReportingPointManaged(id, true)
		}
	}

	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateReportingPoint(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		PLCName    string `json:"plc_name"`
		TagName    string `json:"tag_name"`
		JobStyleID int64  `json:"job_style_id"`
		Enabled    bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Read old RP to detect tag changes
	oldRP, _ := h.engine.DB().GetReportingPoint(id)

	if err := h.engine.DB().UpdateReportingPoint(id, req.PLCName, req.TagName, req.JobStyleID, req.Enabled); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Handle WarLink tag management on tag change
	if oldRP != nil && (oldRP.PLCName != req.PLCName || oldRP.TagName != req.TagName) {
		mgr := h.engine.PLCManager()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()

		// Disable old tag if we managed it
		if oldRP.WarlinkManaged {
			if err := mgr.DisableTagPublishing(ctx, oldRP.PLCName, oldRP.TagName); err != nil {
				log.Printf("warlink: auto-disable old %s/%s failed: %v", oldRP.PLCName, oldRP.TagName, err)
			}
		}

		// Enable new tag if not already published
		if !mgr.IsTagPublished(req.PLCName, req.TagName) {
			if err := mgr.EnableTagPublishing(ctx, req.PLCName, req.TagName); err != nil {
				log.Printf("warlink: auto-enable new %s/%s failed: %v", req.PLCName, req.TagName, err)
				h.engine.DB().SetReportingPointManaged(id, false)
			} else {
				h.engine.DB().SetReportingPointManaged(id, true)
			}
		} else {
			h.engine.DB().SetReportingPointManaged(id, false)
		}
	}

	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteReportingPoint(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}

	// Read RP before deleting so we know if we need to disable the tag
	rp, _ := h.engine.DB().GetReportingPoint(id)

	if err := h.engine.DB().DeleteReportingPoint(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Auto-disable tag in WarLink if we were the ones that enabled it
	if rp != nil && rp.WarlinkManaged {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := h.engine.PLCManager().DisableTagPublishing(ctx, rp.PLCName, rp.TagName); err != nil {
			log.Printf("warlink: auto-disable %s/%s failed: %v", rp.PLCName, rp.TagName, err)
		}
	}

	writeJSON(w, map[string]string{"status": "ok"})
}

// --- Job Styles Admin ---

func (h *Handlers) apiListJobStyles(w http.ResponseWriter, r *http.Request) {
	styles, err := h.engine.DB().ListJobStyles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, styles)
}

func (h *Handlers) apiCreateJobStyle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		CatIDs      []string `json:"cat_ids"`
		LineID      int64    `json:"line_id"`
		RPPLCName   string   `json:"rp_plc_name"`
		RPTagName   string   `json:"rp_tag_name"`
		RPEnabled   bool     `json:"rp_enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.LineID == 0 {
		writeError(w, http.StatusBadRequest, "line_id is required")
		return
	}
	id, err := h.engine.DB().CreateJobStyle(req.Name, req.Description, req.CatIDs, req.LineID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Create reporting point if RP fields provided
	if req.RPPLCName != "" && req.RPTagName != "" {
		rpID, rpErr := h.engine.DB().CreateReportingPoint(req.RPPLCName, req.RPTagName, id)
		if rpErr != nil {
			log.Printf("failed to create RP for style %d: %v", id, rpErr)
		} else {
			if !req.RPEnabled {
				h.engine.DB().UpdateReportingPoint(rpID, req.RPPLCName, req.RPTagName, id, false)
			}
			// Auto-enable tag in WarLink
			mgr := h.engine.PLCManager()
			if !mgr.IsTagPublished(req.RPPLCName, req.RPTagName) {
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				if err := mgr.EnableTagPublishing(ctx, req.RPPLCName, req.RPTagName); err != nil {
					log.Printf("warlink: auto-enable %s/%s failed (RP %d): %v", req.RPPLCName, req.RPTagName, rpID, err)
				} else {
					h.engine.DB().SetReportingPointManaged(rpID, true)
				}
			}
		}
	}

	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateJobStyle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		CatIDs      []string `json:"cat_ids"`
		LineID      int64    `json:"line_id"`
		RPPLCName   string   `json:"rp_plc_name"`
		RPTagName   string   `json:"rp_tag_name"`
		RPEnabled   bool     `json:"rp_enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.LineID == 0 {
		writeError(w, http.StatusBadRequest, "line_id is required")
		return
	}
	if err := h.engine.DB().UpdateJobStyle(id, req.Name, req.Description, req.CatIDs, req.LineID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Manage reporting point lifecycle
	existingRP, _ := h.engine.DB().GetReportingPointByStyleID(id)

	if req.RPPLCName != "" && req.RPTagName != "" {
		if existingRP != nil {
			// Update existing RP
			oldPLC, oldTag := existingRP.PLCName, existingRP.TagName
			h.engine.DB().UpdateReportingPoint(existingRP.ID, req.RPPLCName, req.RPTagName, id, req.RPEnabled)

			// Handle WarLink tag management on tag change
			if oldPLC != req.RPPLCName || oldTag != req.RPTagName {
				mgr := h.engine.PLCManager()
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				if existingRP.WarlinkManaged {
					mgr.DisableTagPublishing(ctx, oldPLC, oldTag)
				}
				if !mgr.IsTagPublished(req.RPPLCName, req.RPTagName) {
					if err := mgr.EnableTagPublishing(ctx, req.RPPLCName, req.RPTagName); err != nil {
						h.engine.DB().SetReportingPointManaged(existingRP.ID, false)
					} else {
						h.engine.DB().SetReportingPointManaged(existingRP.ID, true)
					}
				}
			}
		} else {
			// Create new RP
			rpID, rpErr := h.engine.DB().CreateReportingPoint(req.RPPLCName, req.RPTagName, id)
			if rpErr == nil {
				if !req.RPEnabled {
					h.engine.DB().UpdateReportingPoint(rpID, req.RPPLCName, req.RPTagName, id, false)
				}
				mgr := h.engine.PLCManager()
				if !mgr.IsTagPublished(req.RPPLCName, req.RPTagName) {
					ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
					defer cancel()
					if err := mgr.EnableTagPublishing(ctx, req.RPPLCName, req.RPTagName); err != nil {
						h.engine.DB().SetReportingPointManaged(rpID, false)
					} else {
						h.engine.DB().SetReportingPointManaged(rpID, true)
					}
				}
			}
		}
	} else if existingRP != nil {
		// RP fields empty + RP exists → delete RP
		if existingRP.WarlinkManaged {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			h.engine.PLCManager().DisableTagPublishing(ctx, existingRP.PLCName, existingRP.TagName)
		}
		h.engine.DB().DeleteReportingPoint(existingRP.ID)
	}

	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteJobStyle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	if err := h.engine.DB().DeleteJobStyle(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// --- Payloads Admin ---

func (h *Handlers) apiListPayloads(w http.ResponseWriter, r *http.Request) {
	payloads, err := h.engine.DB().ListPayloads()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, payloads)
}

func (h *Handlers) apiListPayloadsByJobStyle(w http.ResponseWriter, r *http.Request) {
	jobStyleID, err := parseID(r, "jobStyleID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job style ID")
		return
	}
	payloads, err := h.engine.DB().ListPayloadsByJobStyle(jobStyleID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, payloads)
}

func (h *Handlers) apiCreatePayload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JobStyleID      int64   `json:"job_style_id"`
		Location        string  `json:"location"`
		StagingNode     string  `json:"staging_node"`
		Description     string  `json:"description"`
		Manifest        string  `json:"manifest"`
		Multiplier      float64 `json:"multiplier"`
		ProductionUnits int     `json:"production_units"`
		Remaining       int     `json:"remaining"`
		ReorderPoint    int     `json:"reorder_point"`
		ReorderQty      int     `json:"reorder_qty"`
		RetrieveEmpty   bool    `json:"retrieve_empty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Manifest == "" {
		req.Manifest = "{}"
	}
	if req.Multiplier <= 0 {
		req.Multiplier = 1
	}
	if req.ReorderQty <= 0 {
		req.ReorderQty = 1
	}
	id, err := h.engine.DB().CreatePayload(req.JobStyleID, req.Location, req.StagingNode, req.Description, req.Manifest, req.Multiplier, req.ProductionUnits, req.Remaining, req.ReorderPoint, req.ReorderQty, req.RetrieveEmpty)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdatePayload(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Location        string  `json:"location"`
		StagingNode     string  `json:"staging_node"`
		Description     string  `json:"description"`
		Manifest        string  `json:"manifest"`
		Multiplier      float64 `json:"multiplier"`
		ProductionUnits int     `json:"production_units"`
		Remaining       int     `json:"remaining"`
		ReorderPoint    int     `json:"reorder_point"`
		ReorderQty      int     `json:"reorder_qty"`
		RetrieveEmpty   bool    `json:"retrieve_empty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.DB().UpdatePayload(id, req.Location, req.StagingNode, req.Description, req.Manifest, req.Multiplier, req.ProductionUnits, req.Remaining, req.ReorderPoint, req.ReorderQty, req.RetrieveEmpty); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeletePayload(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	if err := h.engine.DB().DeletePayload(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiPayloadCount(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		PieceCount float64 `json:"piece_count"`
		Reset      bool    `json:"reset"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	p, err := h.engine.DB().GetPayload(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "payload not found")
		return
	}

	var prodUnits int
	var status string

	if req.Reset {
		// Reset payload to full production units
		if err := h.engine.DB().ResetPayload(id, p.ProductionUnits); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		prodUnits = p.ProductionUnits
		status = "active"
	} else {
		// Convert piece count to production units via multiplier
		prodUnits = int(math.Round(req.PieceCount / p.Multiplier))
		if prodUnits < 0 {
			prodUnits = 0
		}

		status = p.Status
		if prodUnits == 0 {
			status = "empty"
		} else if prodUnits <= p.ReorderPoint {
			status = "replenishing"
		} else {
			status = "active"
		}

		if err := h.engine.DB().UpdatePayloadRemaining(id, prodUnits, status); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	writeJSON(w, map[string]interface{}{
		"status":           "ok",
		"production_units": prodUnits,
		"payload_status":   status,
	})
}

// --- Location Nodes Admin ---

func (h *Handlers) apiListLocationNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.engine.DB().ListLocationNodes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, nodes)
}

func (h *Handlers) apiListLineLocationNodes(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	nodes, err := h.engine.DB().ListLocationNodesByLine(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, nodes)
}

func (h *Handlers) apiCreateLocationNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeID      string `json:"node_id"`
		LineID      int64  `json:"line_id"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.NodeID == "" {
		writeError(w, http.StatusBadRequest, "node_id is required")
		return
	}
	if req.LineID == 0 {
		writeError(w, http.StatusBadRequest, "line_id is required")
		return
	}
	id, err := h.engine.DB().CreateLocationNode(req.NodeID, req.LineID, req.Description)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateLocationNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		NodeID      string `json:"node_id"`
		LineID      int64  `json:"line_id"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.LineID == 0 {
		writeError(w, http.StatusBadRequest, "line_id is required")
		return
	}
	if err := h.engine.DB().UpdateLocationNode(id, req.NodeID, req.LineID, req.Description); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteLocationNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	if err := h.engine.DB().DeleteLocationNode(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// --- Core Nodes ---

func (h *Handlers) apiGetCoreNodes(w http.ResponseWriter, r *http.Request) {
	nodes := h.engine.CoreNodes()
	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}
	writeJSON(w, names)
}

func (h *Handlers) apiSyncCoreNodes(w http.ResponseWriter, r *http.Request) {
	h.engine.RequestNodeSync()
	writeJSON(w, map[string]string{"status": "ok"})
}

// --- Production Lines Admin ---

func (h *Handlers) apiListLines(w http.ResponseWriter, r *http.Request) {
	lines, err := h.engine.DB().ListProductionLines()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, lines)
}

func (h *Handlers) apiCreateLine(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	id, err := h.engine.DB().CreateProductionLine(req.Name, req.Description)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateLine(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.DB().UpdateProductionLine(id, req.Name, req.Description); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteLine(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	if err := h.engine.DB().DeleteProductionLine(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiSetActiveStyle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		JobStyleID *int64 `json:"job_style_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.DB().SetActiveJobStyle(id, req.JobStyleID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiListLineJobStyles(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	styles, err := h.engine.DB().ListJobStylesByLine(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, styles)
}

func (h *Handlers) apiGetStyleReportingPoint(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	rp, err := h.engine.DB().GetReportingPointByStyleID(id)
	if err != nil {
		writeJSON(w, nil)
		return
	}
	writeJSON(w, rp)
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

	user, err := h.engine.DB().GetAdminUser(username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "user not found")
		return
	}

	if !checkPassword(req.OldPassword, user.PasswordHash) {
		writeError(w, http.StatusBadRequest, "current password is incorrect")
		return
	}

	hash, err := hashPassword(req.NewPassword)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}

	if err := h.engine.DB().UpdateAdminPassword(username, hash); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to update password: %v", err))
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}
