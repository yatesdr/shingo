package www

import (
	"encoding/json"
	"net/http"

	"shingo/protocol"
)

// --- Order Creation ---

func (h *Handlers) apiCreateRetrieveOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayloadID     int64  `json:"payload_id"`
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

	payloadID, p := h.resolvePayload(req.PayloadID)
	if p != nil {
		if req.DeliveryNode == "" {
			req.DeliveryNode = p.Location
		}
		if req.StagingNode == "" {
			req.StagingNode = p.StagingNode
		}
		if req.PayloadCode == "" {
			req.PayloadCode = p.PayloadCode
		}
		req.RetrieveEmpty = p.RetrieveEmpty
	}

	// Batch mode: create multiple empty-bin orders (max 5)
	count := req.Count
	if count < 1 {
		count = 1
	}
	if count > 1 {
		if count > 5 {
			writeError(w, http.StatusBadRequest, "count exceeds maximum of 5")
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
		payloadID, req.RetrieveEmpty,
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
		PayloadID  int64  `json:"payload_id"`
		Quantity   int64  `json:"quantity"`
		PickupNode string `json:"pickup_node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	payloadID, p := h.resolvePayload(req.PayloadID)
	if p != nil && req.PickupNode == "" {
		req.PickupNode = p.Location
	}

	order, err := h.engine.OrderManager().CreateStoreOrder(
		payloadID, req.Quantity, req.PickupNode,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Auto-set count and submit so the order actually gets sent to core.
	if err := h.engine.DB().UpdateOrderFinalCount(order.ID, req.Quantity, true); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.engine.OrderManager().SubmitOrder(order.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, order)
}

func (h *Handlers) apiCreateMoveOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayloadID    int64  `json:"payload_id"`
		Quantity     int64  `json:"quantity"`
		PickupNode   string `json:"pickup_node"`
		DeliveryNode string `json:"delivery_node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	payloadID, p := h.resolvePayload(req.PayloadID)
	if p != nil && req.PickupNode == "" {
		req.PickupNode = p.Location
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

func (h *Handlers) apiCreateComplexOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayloadID int64                      `json:"payload_id"`
		Quantity  int64                      `json:"quantity"`
		Steps     []protocol.ComplexOrderStep `json:"steps"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Steps) == 0 {
		writeError(w, http.StatusBadRequest, "steps are required")
		return
	}

	var payloadID *int64
	if req.PayloadID > 0 {
		payloadID = &req.PayloadID
	}

	order, err := h.engine.OrderManager().CreateComplexOrder(
		payloadID, req.Quantity, req.Steps,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, order)
}

func (h *Handlers) apiCreateIngestOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayloadID   int64                        `json:"payload_id"`
		PayloadCode string                       `json:"payload_code"`
		BinLabel    string                       `json:"bin_label"`
		PickupNode  string                       `json:"pickup_node"`
		Quantity    int64                        `json:"quantity"`
		Manifest    []protocol.IngestManifestItem `json:"manifest"`
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

	payloadID, p := h.resolvePayload(req.PayloadID)
	if p != nil {
		if req.PickupNode == "" {
			req.PickupNode = p.Location
		}
		if req.PayloadCode == "" {
			req.PayloadCode = p.PayloadCode
		}
	}

	order, err := h.engine.OrderManager().CreateIngestOrder(
		payloadID, req.PayloadCode, req.BinLabel, req.PickupNode,
		req.Quantity, req.Manifest,
		h.engine.AppConfig().Web.AutoConfirm,
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
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiReleaseOrder(w http.ResponseWriter, r *http.Request) {
	orderID, err := parseID(r, "orderID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid order ID")
		return
	}

	if err := h.engine.OrderManager().ReleaseOrder(orderID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
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
	if err := h.engine.OrderManager().AbortOrder(orderID); err != nil {
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
		FinalCount int64 `json:"final_count"`
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

// --- Smart Request (operator button) ---

// apiSmartRequest creates the correct order type based on payload config.
// Delegates to engine.RequestOrders which handles all hot-swap modes
// (single_robot, two_robot) and simple retrieve.
func (h *Handlers) apiSmartRequest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayloadID int64 `json:"payload_id"`
		Quantity  int64 `json:"quantity"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.PayloadID == 0 {
		writeError(w, http.StatusBadRequest, "payload_id is required")
		return
	}
	if req.Quantity < 1 {
		req.Quantity = 1
	}

	result, err := h.engine.RequestOrders(req.PayloadID, req.Quantity)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, result)
}
