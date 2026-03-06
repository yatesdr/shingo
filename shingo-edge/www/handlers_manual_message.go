package www

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"shingo/protocol"
)

func (h *Handlers) handleManualMessage(w http.ResponseWriter, r *http.Request) {
	db := h.engine.DB()
	cfg := h.engine.AppConfig()

	orders, _ := db.ListActiveOrders()
	coreNodes := h.engine.CoreNodes()
	coreNodeNames := make([]string, 0, len(coreNodes))
	for name := range coreNodes {
		coreNodeNames = append(coreNodeNames, name)
	}
	anomalies, rpMap := loadAnomalyData(h)

	// JSON-encode data for page-data attributes
	type orderSummary struct {
		UUID      string `json:"uuid"`
		OrderType string `json:"type"`
		Status    string `json:"status"`
	}
	orderSummaries := make([]orderSummary, 0, len(orders))
	for _, o := range orders {
		orderSummaries = append(orderSummaries, orderSummary{
			UUID:      o.UUID,
			OrderType: o.OrderType,
			Status:    o.Status,
		})
	}
	ordersJSON, _ := json.Marshal(orderSummaries)
	coreNodesJSON, _ := json.Marshal(coreNodeNames)
	lineIDsJSON, _ := json.Marshal([]string{cfg.LineID})

	data := map[string]interface{}{
		"Page":              "manual-message",
		"StationID":         cfg.StationID(),
		"LineIDs":           []string{cfg.LineID},
		"Orders":            orders,
		"CoreNodes":         coreNodeNames,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
		"LineIDsJSON":       string(lineIDsJSON),
		"OrdersJSON":        string(ordersJSON),
		"CoreNodesJSON":     string(coreNodesJSON),
	}

	h.renderTemplate(w, "manual-message.html", data)
}

func (h *Handlers) apiSendManualMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg := h.engine.AppConfig()
	stationID := cfg.StationID()
	src := protocol.Address{Role: protocol.RoleEdge, Station: stationID}
	dst := protocol.Address{Role: protocol.RoleCore}

	var env *protocol.Envelope
	var err error

	switch req.Type {
	// --- Data channel messages ---
	case "edge.register":
		var p struct {
			Version string   `json:"version"`
			LineIDs []string `json:"line_ids"`
		}
		if e := json.Unmarshal(req.Payload, &p); e != nil {
			writeError(w, http.StatusBadRequest, "invalid payload: "+e.Error())
			return
		}
		hostname, _ := os.Hostname()
		env, err = protocol.NewDataEnvelope(protocol.SubjectEdgeRegister, src, dst, &protocol.EdgeRegister{
			StationID: stationID,
			Hostname:  hostname,
			Version:   p.Version,
			LineIDs:   p.LineIDs,
		})

	case "edge.heartbeat":
		var p struct {
			Uptime int64 `json:"uptime"`
		}
		json.Unmarshal(req.Payload, &p)
		env, err = protocol.NewDataEnvelope(protocol.SubjectEdgeHeartbeat, src, dst, &protocol.EdgeHeartbeat{
			StationID: stationID,
			Uptime:    p.Uptime,
		})

	case "production.report":
		var p struct {
			Entries []protocol.ProductionReportEntry `json:"entries"`
		}
		if e := json.Unmarshal(req.Payload, &p); e != nil {
			writeError(w, http.StatusBadRequest, "invalid payload: "+e.Error())
			return
		}
		env, err = protocol.NewDataEnvelope(protocol.SubjectProductionReport, src, dst, &protocol.ProductionReport{
			StationID: stationID,
			Reports:   p.Entries,
		})

	case "node.list_request":
		env, err = protocol.NewDataEnvelope(protocol.SubjectNodeListRequest, src, dst, &protocol.NodeListRequest{})

	// --- Order messages ---
	case "order.request":
		var p protocol.OrderRequest
		if e := json.Unmarshal(req.Payload, &p); e != nil {
			writeError(w, http.StatusBadRequest, "invalid payload: "+e.Error())
			return
		}
		env, err = protocol.NewEnvelope(protocol.TypeOrderRequest, src, dst, &p)

	case "order.cancel":
		var p protocol.OrderCancel
		if e := json.Unmarshal(req.Payload, &p); e != nil {
			writeError(w, http.StatusBadRequest, "invalid payload: "+e.Error())
			return
		}
		env, err = protocol.NewEnvelope(protocol.TypeOrderCancel, src, dst, &p)

	case "order.receipt":
		var p protocol.OrderReceipt
		if e := json.Unmarshal(req.Payload, &p); e != nil {
			writeError(w, http.StatusBadRequest, "invalid payload: "+e.Error())
			return
		}
		env, err = protocol.NewEnvelope(protocol.TypeOrderReceipt, src, dst, &p)

	case "order.redirect":
		var p protocol.OrderRedirect
		if e := json.Unmarshal(req.Payload, &p); e != nil {
			writeError(w, http.StatusBadRequest, "invalid payload: "+e.Error())
			return
		}
		env, err = protocol.NewEnvelope(protocol.TypeOrderRedirect, src, dst, &p)

	case "order.storage_waybill":
		var p protocol.OrderStorageWaybill
		if e := json.Unmarshal(req.Payload, &p); e != nil {
			writeError(w, http.StatusBadRequest, "invalid payload: "+e.Error())
			return
		}
		env, err = protocol.NewEnvelope(protocol.TypeOrderStorageWaybill, src, dst, &p)

	default:
		writeError(w, http.StatusBadRequest, "unknown message type: "+req.Type)
		return
	}

	if err != nil {
		writeError(w, http.StatusInternalServerError, "build envelope: "+err.Error())
		return
	}

	if err := h.engine.SendEnvelope(env); err != nil {
		writeError(w, http.StatusInternalServerError, "send failed: "+err.Error())
		return
	}

	// Return envelope metadata for the UI preview
	writeJSON(w, map[string]interface{}{
		"status":    "ok",
		"msg_id":    env.ID,
		"timestamp": env.Timestamp.Format(time.RFC3339),
	})
}
