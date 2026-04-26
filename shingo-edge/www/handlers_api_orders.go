package www

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"shingo/protocol"
	"shingoedge/engine"
)

// MaxBatchRetrieveCount is the maximum number of orders in a batch retrieve request.
const MaxBatchRetrieveCount = 5

// --- Order Creation ---

func (h *Handlers) apiCreateRetrieveOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProcessNodeID int64  `json:"process_node_id"`
		PayloadCode   string `json:"payload_code"`
		RetrieveEmpty bool   `json:"retrieve_empty"`
		Quantity      int64  `json:"quantity"`
		DeliveryNode  string `json:"delivery_node"`
		StagingNode   string `json:"staging_node"`
		LoadType      string `json:"load_type"`
		Count         int    `json:"count"` // >1 creates a batch of empty bin orders
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var processNodeID *int64
	if req.ProcessNodeID > 0 {
		processNodeID = &req.ProcessNodeID
	}
	if processNodeID != nil && req.DeliveryNode == "" {
		if node, err := h.engine.ProcessService().GetNode(*processNodeID); err == nil {
			req.DeliveryNode = node.CoreNodeName
		}
	}

	// Batch mode: create multiple empty-bin orders (max 5)
	count := req.Count
	if count < 1 {
		count = 1
	}
	if count > 1 {
		if count > MaxBatchRetrieveCount {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("count exceeds maximum of %d", MaxBatchRetrieveCount))
			return
		}
		if req.PayloadCode == "" || req.DeliveryNode == "" {
			writeError(w, http.StatusBadRequest, "payload_code and delivery_node required for batch")
			return
		}
		h.createRetrieveBatch(w, req.PayloadCode, req.DeliveryNode, count)
		return
	}

	order, err := h.engine.OrderManager().CreateRetrieveOrder(
		processNodeID, req.RetrieveEmpty,
		req.Quantity, req.DeliveryNode, req.StagingNode, req.LoadType, req.PayloadCode,
		h.engine.AppConfig().Web.AutoConfirm,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, order)
}

func (h *Handlers) createRetrieveBatch(w http.ResponseWriter, payloadCode, deliveryNode string, count int) {
	type result struct {
		OrderID int64  `json:"order_id,omitempty"`
		UUID    string `json:"uuid,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	var results []result
	created := 0
	for i := 0; i < count; i++ {
		order, err := h.engine.OrderManager().CreateRetrieveOrder(
			nil, true, 1, deliveryNode, "", "standard", payloadCode,
			h.engine.AppConfig().Web.AutoConfirm,
		)
		if err != nil {
			results = append(results, result{Error: err.Error()})
			continue
		}
		results = append(results, result{OrderID: order.ID, UUID: order.UUID})
		created++
	}
	writeJSON(w, map[string]interface{}{
		"requested": count,
		"created":   created,
		"orders":    results,
	})
}

func (h *Handlers) apiCreateStoreOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProcessNodeID int64  `json:"process_node_id"`
		Quantity      int64  `json:"quantity"`
		SourceNode    string `json:"source_node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var processNodeID *int64
	if req.ProcessNodeID > 0 {
		processNodeID = &req.ProcessNodeID
		if node, err := h.engine.ProcessService().GetNode(*processNodeID); err == nil && req.SourceNode == "" {
			req.SourceNode = node.CoreNodeName
		}
	}

	order, err := h.engine.OrderManager().CreateStoreOrder(
		processNodeID, req.Quantity, req.SourceNode,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Auto-set count and submit so the order actually gets sent to core.
	if err := h.engine.OrderManager().SubmitStoreOrder(order.ID, req.Quantity); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSONWithTrigger(w, r, order, "refreshMaterial")
}

func (h *Handlers) apiCreateMoveOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProcessNodeID int64  `json:"process_node_id"`
		Quantity      int64  `json:"quantity"`
		SourceNode    string `json:"source_node"`
		DeliveryNode  string `json:"delivery_node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var processNodeID *int64
	if req.ProcessNodeID > 0 {
		processNodeID = &req.ProcessNodeID
		if node, err := h.engine.ProcessService().GetNode(*processNodeID); err == nil && req.SourceNode == "" {
			req.SourceNode = node.CoreNodeName
		}
	}

	order, err := h.engine.OrderManager().CreateMoveOrder(
		processNodeID, req.Quantity, req.SourceNode, req.DeliveryNode,
		h.engine.AppConfig().Web.AutoConfirm,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, order)
}

func (h *Handlers) apiCreateComplexOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProcessNodeID int64                       `json:"process_node_id"`
		Quantity      int64                       `json:"quantity"`
		Steps         []protocol.ComplexOrderStep `json:"steps"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Steps) == 0 {
		writeError(w, http.StatusBadRequest, "steps are required")
		return
	}

	var processNodeID *int64
	if req.ProcessNodeID > 0 {
		processNodeID = &req.ProcessNodeID
	}

	order, err := h.engine.OrderManager().CreateComplexOrder(
		processNodeID, req.Quantity, "", req.Steps,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, order)
}

func (h *Handlers) apiCreateIngestOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProcessNodeID int64                         `json:"process_node_id"`
		PayloadCode   string                        `json:"payload_code"`
		BinLabel      string                        `json:"bin_label"`
		SourceNode    string                        `json:"source_node"`
		Quantity      int64                         `json:"quantity"`
		Manifest      []protocol.IngestManifestItem `json:"manifest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.PayloadCode == "" {
		writeError(w, http.StatusBadRequest, "payload_code is required")
		return
	}
	if req.BinLabel == "" {
		writeError(w, http.StatusBadRequest, "bin_label is required")
		return
	}

	var processNodeID *int64
	if req.ProcessNodeID > 0 {
		processNodeID = &req.ProcessNodeID
		if node, err := h.engine.ProcessService().GetNode(*processNodeID); err == nil && req.SourceNode == "" {
			req.SourceNode = node.CoreNodeName
		}
	}

	producedAt := time.Now().UTC().Format(time.RFC3339)
	order, err := h.engine.OrderManager().CreateIngestOrder(
		processNodeID, req.PayloadCode, req.BinLabel, req.SourceNode,
		req.Quantity, req.Manifest,
		h.engine.AppConfig().Web.AutoConfirm,
		producedAt,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, order)
}

// --- Order Actions ---

func (h *Handlers) apiConfirmDelivery(w http.ResponseWriter, r *http.Request) {
	orderID, err := parseID(r, "orderID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid order ID")
		return
	}
	var req struct {
		FinalCount int64 `json:"final_count"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := h.engine.OrderManager().ConfirmDelivery(orderID, req.FinalCount); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshOrders")
}

// apiReleaseOrder is the operator's "release" click for a staged order.
//
// Phase 3 (lineside): release is unified through ReleaseOrderWithLineside
// so the engine can (1) capture parts the operator pulled to lineside
// during the swap, (2) reset the node counter, and (3) advance the
// changeover task state atomically before the bots head back.
//
// Phase 8 (release-time manifest): the body now also carries a disposition
// so the operator can choose between "bin is empty" (capture_lineside) and
// "send the partial bin back to supermarket" (send_partial_back). The
// disposition late-binds the bin's manifest at Core (see OrderRelease and
// BinManifestService.SyncOrClearForReleased).
//
// Backward compat: an absent or empty disposition (Mode == "") leaves the
// bin's manifest untouched at Core — matching pre-Phase-8 behavior. The
// "NOTHING PULLED" button explicitly sends disposition="capture_lineside",
// which is the path that newly clears the bin manifest.
func (h *Handlers) apiReleaseOrder(w http.ResponseWriter, r *http.Request) {
	orderID, err := parseID(r, "orderID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid order ID")
		return
	}

	// Body is optional — empty body means "release, no lineside capture,
	// no manifest action" (legacy behavior).
	var req struct {
		Disposition string         `json:"disposition"`
		QtyByPart   map[string]int `json:"qty_by_part"`
		CalledBy    string         `json:"called_by"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	disp := buildReleaseDisposition(req.Disposition, req.QtyByPart, req.CalledBy)
	if err := h.orchestration.ReleaseOrderWithLineside(orderID, disp); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshOrders")
}

// buildReleaseDisposition translates the JSON body into an engine.ReleaseDisposition.
// An unknown or empty disposition string maps to the zero-value disposition
// (Mode == "") — Core leaves the bin's manifest alone, preserving pre-Phase-8
// behavior for older clients posting bare bodies.
func buildReleaseDisposition(mode string, qtyByPart map[string]int, calledBy string) engine.ReleaseDisposition {
	switch engine.ReleaseDispositionMode(mode) {
	case engine.DispositionCaptureLineside:
		return engine.ReleaseDisposition{
			Mode:            engine.DispositionCaptureLineside,
			LinesideCapture: qtyByPart,
			CalledBy:        calledBy,
		}
	case engine.DispositionSendPartialBack:
		return engine.ReleaseDisposition{
			Mode:     engine.DispositionSendPartialBack,
			CalledBy: calledBy,
		}
	default:
		// Empty or unknown disposition — preserve legacy "no manifest action"
		// behavior. CalledBy still flows through for the audit trail.
		// Catch typos and client/server enum drift: an explicit non-empty
		// value that isn't recognised is logged so the failure is visible
		// rather than silent. Empty mode (legitimate legacy clients posting
		// bare bodies) is not logged.
		if mode != "" {
			log.Printf("apiReleaseOrder: unknown disposition %q from client, treating as no manifest action", mode)
		}
		return engine.ReleaseDisposition{CalledBy: calledBy}
	}
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
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshOrders")
}

func (h *Handlers) apiCancelOrder(w http.ResponseWriter, r *http.Request) {
	orderID, err := parseID(r, "orderID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid order ID")
		return
	}
	if err := h.engine.OrderManager().AbortOrder(orderID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshOrders")
}

func (h *Handlers) apiSetOrderCount(w http.ResponseWriter, r *http.Request) {
	orderID, err := parseID(r, "orderID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid order ID")
		return
	}
	var req struct {
		FinalCount int64 `json:"final_count"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.OrderService().UpdateFinalCount(orderID, req.FinalCount, true); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
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
