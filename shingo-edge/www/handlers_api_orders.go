package www

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"shingo/protocol"
	"shingoedge/engine"
	ordermgr "shingoedge/orders"
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
		SourceNode    string `json:"source_node"` // optional supermarket group; "" = Core's global FIFO
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
	}

	// One call for one and for many. The batch used to be a separate function
	// that looped CreateRetrieveOrder directly, which is how it ended up as the
	// only creation path with no budget behind it — ask for five empties at a
	// window with room for one and you got five. Both arms now go through the
	// engine, which routes to the reservation seam when a loader owns the
	// destination and leaves everything else exactly as it was.
	made, err := h.orchestration.CreateRetrieveForAPI(engine.APIRetrieveRequest{
		ProcessNodeID: processNodeID,
		RetrieveEmpty: req.RetrieveEmpty,
		Quantity:      req.Quantity,
		DeliveryNode:  req.DeliveryNode,
		SourceNode:    req.SourceNode,
		StagingNode:   req.StagingNode,
		LoadType:      req.LoadType,
		PayloadCode:   req.PayloadCode,
		AutoConfirm:   h.engine.AppConfig().Web.AutoConfirm,
		Count:         count,
	})
	if err != nil {
		if errors.Is(err, engine.ErrLoaderBudgetExhausted) {
			// The plant is in a state that refuses the request, not a bad
			// request. Same answer the operator's own buttons give.
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Nothing created and no error is not a shape the engine should produce —
	// it returns ErrLoaderBudgetExhausted for that — but the handler must not
	// take the caller down if it ever does. An empty slice indexed at [0] is a
	// panic, and a panic here is a 500 with a stack trace where a sentence
	// belongs.
	if len(made) == 0 {
		writeError(w, http.StatusInternalServerError, "order creation returned no orders and no error")
		return
	}

	// A single request keeps its original single-object response; only the batch
	// gets the envelope, and it now reports what was CREATED rather than what
	// was asked for.
	if count == 1 {
		writeJSON(w, made[0])
		return
	}
	results := make([]map[string]any, 0, len(made))
	for _, o := range made {
		results = append(results, map[string]any{"order_id": o.ID, "uuid": o.UUID})
	}
	writeJSON(w, map[string]any{
		"requested": count,
		"created":   len(made),
		"orders":    results,
	})
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

	// NoDemand: an order posted to the HTTP API is a direct command. Somebody
	// wanted it, but it belongs to no cell episode and never will — see the
	// Origin type for why that is the distinction the class records.
	order, err := h.engine.OrderManager().CreateMoveOrder(
		processNodeID, req.Quantity, req.SourceNode, req.DeliveryNode,
		h.engine.AppConfig().Web.AutoConfirm, ordermgr.NoDemand(),
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
		processNodeID, req.Quantity, "", "", req.Steps, ordermgr.NoDemand(),
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, order)
}

// apiCreateIngestOrder is a MANUAL manifest backfill / debug tool — the only
// way to stamp a bin's produced manifest from outside the produce-finalize
// flow (operator scanned a real tote). It is identity-safe: it requires a
// bin_label and Core resolves the bin BY LABEL. It is fire-and-forget: a 200
// means the manifest report was ACCEPTED into the durable outbox, NOT that
// Core recorded it (Core processes asynchronously and does not reply on
// success). Not a product/order endpoint — no order is created.
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
	if err := h.engine.OrderManager().QueueIngestManifest(
		req.PayloadCode, req.BinLabel, 0, req.SourceNode,
		req.Quantity, req.Manifest, producedAt,
	); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Ack: accepted into the outbox, not a durable Core confirmation.
	writeJSON(w, map[string]any{
		"bin_label":    req.BinLabel,
		"payload_code": req.PayloadCode,
		"quantity":     req.Quantity,
		"accepted":     true,
	})
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
	// Tolerate empty body — operator station's postAction sends {}
	// when the CONFIRM button has no extra payload. EOF on empty body
	// is fine: FinalCount defaults to 0.
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
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

	// Body validation lives in parseReleaseRequest (handlers_release.go) so
	// every release endpoint inherits the same post-2026-04-27 guard. See
	// that function's docstring for the contract.
	req, err := parseReleaseRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	disp := buildReleaseDisposition(req)
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
//
// Phase 0b: threads through the override-audit fields (qty_by_part_suggested,
// partial_count, partial_count_suggested) so Core can record any divergence
// between the operator's submission and the system-suggested baseline.
func buildReleaseDisposition(req releaseRequest) engine.ReleaseDisposition {
	switch engine.ReleaseDispositionMode(req.Disposition) {
	case engine.DispositionCaptureLineside:
		return engine.ReleaseDisposition{
			Mode:                     engine.DispositionCaptureLineside,
			LinesideCapture:          req.QtyByPart,
			LinesideCaptureSuggested: req.QtyByPartSuggested,
			CalledBy:                 req.CalledBy,
		}
	case engine.DispositionSendPartialBack:
		return engine.ReleaseDisposition{
			Mode:                  engine.DispositionSendPartialBack,
			PartialCount:          req.PartialCount,
			PartialCountSuggested: req.PartialCountSuggested,
			CalledBy:              req.CalledBy,
		}
	case engine.DispositionReleaseUnderpack:
		// Underpack carries no operator-entered count — the wire
		// shape is &0 (manifest clear). The disposition string is
		// what flags the audit op as released_underpack so
		// forensics can trend missing-inventory separately from
		// system-and-operator-agreed-empty (RELEASE EMPTY).
		return engine.ReleaseDisposition{
			Mode:     engine.DispositionReleaseUnderpack,
			CalledBy: req.CalledBy,
		}
	default:
		// Empty or unknown disposition — preserve legacy "no manifest action"
		// behavior. CalledBy still flows through for the audit trail.
		// Catch typos and client/server enum drift: an explicit non-empty
		// value that isn't recognised is logged so the failure is visible
		// rather than silent. Empty mode (legitimate legacy clients posting
		// bare bodies) is not logged.
		if req.Disposition != "" {
			log.Printf("apiReleaseOrder: unknown disposition %q from client, treating as no manifest action", req.Disposition)
		}
		return engine.ReleaseDisposition{CalledBy: req.CalledBy}
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
