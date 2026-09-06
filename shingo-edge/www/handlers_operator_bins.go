// handlers_operator_bins.go — operator-driven bin operations (load,
// request empty/full, clear) plus the read-only Core lookups the bin
// modal needs (node children, payload manifest) and the runtime-order
// clear that handles stranded-bin recovery.

package www

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
	"shingoedge/engine"
)

func (h *Handlers) apiLoadBin(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var req struct {
		PayloadCode string                        `json:"payload_code"`
		UOPCount    *int64                        `json:"uop_count"`
		Manifest    []protocol.IngestManifestItem `json:"manifest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.orchestration.LoadBin(id, req.PayloadCode, req.UOPCount, req.Manifest); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

func (h *Handlers) apiRequestEmptyBin(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var req struct {
		PayloadCode string `json:"payload_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_, err = h.orchestration.RequestEmptyBin(id, req.PayloadCode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeActionOK(w, r, "refreshMaterial")
}

func (h *Handlers) apiRequestFullBin(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var req struct {
		PayloadCode string `json:"payload_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_, err = h.orchestration.RequestFullBin(id, req.PayloadCode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeActionOK(w, r, "refreshMaterial")
}

func (h *Handlers) apiClearLoaderHome(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	if err := h.orchestration.ClearLoaderHome(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

func (h *Handlers) apiGetMarketBins(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	bins, err := h.orchestration.FetchMarketBins(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if bins == nil {
		bins = []engine.MarketBinInfo{}
	}
	writeJSON(w, bins)
}

func (h *Handlers) apiPullFromMarket(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var req struct {
		SourceCoreName string `json:"source_core_node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.orchestration.PullFromMarket(id, req.SourceCoreName); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

func (h *Handlers) apiClearBin(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var body struct {
		BinTypeCode string `json:"bin_type_code"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := h.orchestration.ClearBin(id, body.BinTypeCode); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

// apiRecordCount is the operator declaring the count on the carrier in front of
// them. The number goes to Core, which records it and broadcasts the
// correction; the local cache is written from Core's reply so the two sides
// hold the same number. A refusal from Core surfaces here rather than being
// swallowed — an operator who corrected a count has to know it landed.
func (h *Handlers) apiRecordCount(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var body struct {
		ActualUOP int    `json:"actual_uop"`
		Actor     string `json:"actor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	actor := body.Actor
	if actor == "" {
		actor = "operator"
	}
	if err := h.orchestration.RecordBinCount(id, body.ActualUOP, actor); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

func (h *Handlers) apiPushEmptyOut(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	if err := h.orchestration.PushEmptyOut(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

func (h *Handlers) apiNodeChildren(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		writeJSON(w, []struct{}{})
		return
	}
	children, _ := h.engine.CoreAPI().FetchNodeChildren(name, false)
	if children == nil {
		children = []engine.NodeChildInfo{}
	}
	writeJSON(w, children)
}

func (h *Handlers) apiPayloadManifest(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	if code == "" {
		writeJSON(w, map[string]any{"uop_capacity": 0, "items": []struct{}{}})
		return
	}
	result, _ := h.engine.CoreAPI().FetchPayloadManifest(code)
	if result == nil {
		result = &engine.PayloadManifestResponse{Items: []engine.ManifestItem{}}
	}
	writeJSON(w, result)
}

// apiClearNodeOrders drops both runtime order pointers on a node.
//
// IT IS THE ONE BUTTON ON THIS PAGE THAT DESTROYS STATE WITHOUT ASKING, and
// until this log line it did so invisibly. There is no request logger anywhere
// under www/ — router.go wires Recoverer and Compress and nothing else — so
// nothing recorded that the route had been called, let alone what it discarded.
// A node that lost a live staged leg this way looked exactly like a node that
// never had one, and no journal search could tell the two apart afterwards.
//
// The read is deliberately before the write and its failure is deliberately not
// fatal: not being able to say what is about to be destroyed is a reason to say
// so, not a reason to refuse the operator's clear.
func (h *Handlers) apiClearNodeOrders(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	nodeName := ""
	if node, nerr := h.engine.ProcessService().GetNode(id); nerr == nil && node != nil {
		nodeName = node.CoreNodeName
	}
	if rt, rerr := h.engine.ProcessService().EnsureNodeRuntime(id); rerr == nil && rt != nil {
		log.Printf("clear node orders: node %d (%s) active_order_id=%s staged_order_id=%s — both discarded by operator",
			id, nodeName, orderRef(rt.ActiveOrderID), orderRef(rt.StagedOrderID))
	} else {
		log.Printf("clear node orders: node %d (%s) — both pointers discarded by operator; could not read them first: %v",
			id, nodeName, rerr)
	}
	if err := h.engine.ProcessService().ClearNodeRuntimeOrders(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

// orderRef renders a nullable order pointer for a log line. "none" rather than
// "0" or "<nil>", because the whole value of the line is telling apart a slot
// that held nothing from a slot that held something.
func orderRef(id *int64) string {
	if id == nil {
		return "none"
	}
	return strconv.FormatInt(*id, 10)
}

// apiRefuseSupply records the loader operator's "I cannot fill this call" for
// one card, and apiUndoSupplyRefusal takes it back.
//
// Both take (process node, payload) — the card — never a bare payload. That is
// what makes owner decision 2 structural: the control cannot be aimed at
// something nobody asked for, because the endpoint has no way to name one.
func (h *Handlers) apiRefuseSupply(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var req struct {
		PayloadCode string `json:"payload_code"`
		RefusedBy   string `json:"refused_by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.orchestration.RefuseSupply(id, req.PayloadCode, req.RefusedBy); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

func (h *Handlers) apiUndoSupplyRefusal(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var req struct {
		PayloadCode string `json:"payload_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.orchestration.UndoSupplyRefusal(id, req.PayloadCode); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

// apiAckSupplyRefusal records the cell operator's answer — WAIT or CHANGE OVER.
//
// Both are real answers to a real question and one of them must be given; there
// is no dismiss. CHANGE OVER records here and the HMI then opens the existing
// changeover picker, because the operator still has to say which style — the ack
// is the decision, the picker is the destination.
func (h *Handlers) apiAckSupplyRefusal(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var req struct {
		LoaderNode  string `json:"loader_node"`
		PayloadCode string `json:"payload_code"`
		Choice      string `json:"choice"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.orchestration.AckSupplyRefusal(id, req.LoaderNode, req.PayloadCode, req.Choice); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}
