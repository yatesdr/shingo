package www

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"shingo/protocol"
)

func (h *Handlers) handleManualMessage(w http.ResponseWriter, r *http.Request) {
	cfg := h.engine.AppConfig()

	orders, _ := h.engine.OrderService().ListActive()
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
			OrderType: string(o.OrderType),
			Status:    string(o.Status),
		})
	}
	ordersJSON, _ := json.Marshal(orderSummaries)
	coreNodesJSON, _ := json.Marshal(coreNodeNames)

	data := map[string]any{
		"Page":              "manual-message",
		"StationID":         cfg.StationID(),
		"Orders":            orders,
		"CoreNodes":         coreNodeNames,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
		"OrdersJSON":        string(ordersJSON),
		"CoreNodesJSON":     string(coreNodesJSON),
	}

	h.renderTemplate(w, r, "manual-message.html", data)
}

// orderEnvelopeSpecs maps a debug-page order message type to its wire type and
// a fresh payload value to decode into. protocol.NewEnvelope takes `any`, so the
// pointer from new() goes straight through.
var orderEnvelopeSpecs = map[string]struct {
	typ     string
	payload func() any
}{
	"order.request":  {protocol.TypeOrderRequest, func() any { return new(protocol.OrderRequest) }},
	"order.cancel":   {protocol.TypeOrderCancel, func() any { return new(protocol.OrderCancel) }},
	"order.receipt":  {protocol.TypeOrderReceipt, func() any { return new(protocol.OrderReceipt) }},
	"order.redirect": {protocol.TypeOrderRedirect, func() any { return new(protocol.OrderRedirect) }},
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
			Version string `json:"version"`
		}
		if e := json.Unmarshal(req.Payload, &p); e != nil {
			writeError(w, http.StatusBadRequest, "invalid payload: "+e.Error())
			return
		}
		hostname, _ := os.Hostname()
		// No Instance: a hand-fired register from the debug page is not this
		// process claiming the station, and stamping the real instance here
		// would let a click move the binding lease.
		env, err = protocol.NewDataEnvelope(protocol.SubjectEdgeRegister, src, dst, &protocol.EdgeRegister{
			StationID: stationID,
			Hostname:  hostname,
			Version:   p.Version,
		})

	case "edge.heartbeat":
		var p struct {
			Uptime int64 `json:"uptime"`
		}
		if e := json.Unmarshal(req.Payload, &p); e != nil {
			writeError(w, http.StatusBadRequest, "invalid payload: "+e.Error())
			return
		}
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
	//
	// All four are the same three steps over a different payload type, so they
	// are a table rather than four copies. The data-channel cases above are not:
	// each builds its envelope from something other than the posted payload
	// (a hostname, a station id), which is why they stay written out.
	default:
		spec, ok := orderEnvelopeSpecs[req.Type]
		if !ok {
			writeError(w, http.StatusBadRequest, "unknown message type: "+req.Type)
			return
		}
		p := spec.payload()
		if e := json.Unmarshal(req.Payload, p); e != nil {
			writeError(w, http.StatusBadRequest, "invalid payload: "+e.Error())
			return
		}
		env, err = protocol.NewEnvelope(spec.typ, src, dst, p)
	}

	if err != nil {
		writeError(w, http.StatusInternalServerError, "build envelope: "+err.Error())
		return
	}

	if err := h.orchestration.SendEnvelope(env); err != nil {
		writeError(w, http.StatusInternalServerError, "send failed: "+err.Error())
		return
	}

	// Return envelope metadata for the UI preview
	writeJSON(w, map[string]any{
		"status":    "ok",
		"msg_id":    env.ID,
		"timestamp": env.Timestamp.Format(time.RFC3339),
	})
}
