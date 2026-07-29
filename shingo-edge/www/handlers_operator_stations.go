// handlers_operator_stations.go — operator-station resource: shop-floor
// display page plus CRUD/move for the station rows. Per-node actions
// (release, bins, changeover) live in their own files (handlers_operator_*.go).

package www

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"shingo/protocol"
	"shingoedge/domain"
)

func (h *Handlers) handleOperatorStationDisplay(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		http.Error(w, "invalid station id", http.StatusBadRequest)
		return
	}
	station, err := h.engine.StationService().Get(id)
	if err != nil {
		http.Error(w, "station not found", http.StatusNotFound)
		return
	}
	_ = h.engine.StationService().Touch(id, "online")
	data := map[string]any{
		"Page":    "operator-display",
		"Station": station,
	}
	h.renderTemplate(w, r, "operator-display.html", data)
}

func (h *Handlers) apiGetOperatorStationView(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid station id")
		return
	}
	// Coalesced per station: concurrent requests for the same board share one
	// build instead of each starting their own on the single DB connection. The
	// Core enrichment runs inside the shared build too, so joiners get a
	// complete view rather than a half-populated one.
	view, err := h.stationViews.do(r.Context(), id, func(ctx context.Context) (*domain.OperatorStationView, error) {
		v, err := h.engine.StationService().BuildView(ctx, id)
		if err != nil {
			return nil, err
		}
		views := []domain.OperatorStationView{*v}
		enrichViewBinState(h.engine.CoreAPI(), views)
		h.orchestration.EnrichHomeBufferPartials(views[0].Nodes)
		v.Nodes = views[0].Nodes
		return v, nil
	})
	if err != nil {
		// The requester gave up (browser abort, or its own timeout). There is
		// nobody to answer and it is not a 404 — writing one would only mislabel
		// a healthy station as missing in the logs.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		writeError(w, http.StatusNotFound, "station not found")
		return
	}
	_ = h.engine.StationService().Touch(id, "online")
	// Per-style sourceability for the operator's changeover picker, from the SAME
	// mapping the admin changeover page uses (styleSourcingViewFrom) so the two
	// screens can't disagree about whether a style is sourceable. Read from Edge's
	// local sourcing_state cache — no Core round-trip on the board's poll path,
	// which is load-bearing here (see the coalescing comment above).
	//
	// Note the picker deliberately does NOT honour Blocked: on the admin page a red
	// style is unselectable, but the operator is allowed to change over onto one and
	// let the orders queue. The field ships anyway rather than being filtered out —
	// it is the admin policy, and hiding it here would make the two views look like
	// they disagree about the verdict when they only differ on what to do about it.
	sourcing := map[string]styleSourcingView{}
	for _, s := range h.engine.SourcingStateForProcess(view.Process.Name) {
		sourcing[s.StyleID] = styleSourcingViewFrom(s)
	}
	writeJSON(w, struct {
		*domain.OperatorStationView
		PayloadBinTypes []protocol.PayloadBinTypeInfo `json:"payload_bin_types,omitempty"`
		SourcingByStyle map[string]styleSourcingView  `json:"sourcing_by_style,omitempty"`
	}{
		OperatorStationView: view,
		PayloadBinTypes:     h.engine.PayloadBinTypes(),
		SourcingByStyle:     sourcing,
	})
}

func (h *Handlers) apiGetActiveOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := h.engine.OrderService().ListActive()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, orders)
}

func (h *Handlers) apiListOperatorStations(w http.ResponseWriter, r *http.Request) {
	stations, err := h.engine.StationService().List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, stations)
}

func (h *Handlers) apiCreateOperatorStation(w http.ResponseWriter, r *http.Request) {
	var in domain.StationInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := h.engine.StationService().Create(in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateOperatorStation(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var in domain.StationInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.StationService().Update(id, in); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteOperatorStation(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.engine.StationService().Delete(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiMoveOperatorStation(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct {
		Direction string `json:"direction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Direction != "up" && req.Direction != "down" {
		writeError(w, http.StatusBadRequest, "direction must be up or down")
		return
	}
	if err := h.engine.StationService().Move(id, req.Direction); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}
