// handlers_operator_bins.go — operator-driven bin operations (load,
// request empty/full, clear) plus the read-only Core lookups the bin
// modal needs (node children, payload manifest) and the runtime-order
// clear that handles stranded-bin recovery.

package www

import (
	"encoding/json"
	"net/http"

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
		UOPCount    int64                         `json:"uop_count"`
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
	order, err := h.orchestration.RequestEmptyBin(id, req.PayloadCode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, order, "refreshMaterial")
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
	order, err := h.orchestration.RequestFullBin(id, req.PayloadCode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, order, "refreshMaterial")
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
	children, _ := h.engine.CoreAPI().FetchNodeChildren(name)
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

func (h *Handlers) apiClearNodeOrders(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	if err := h.engine.ProcessService().UpdateNodeRuntimeOrders(id, nil, nil); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}
