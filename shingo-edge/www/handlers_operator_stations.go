// handlers_operator_stations.go — operator-station resource: shop-floor
// display page plus CRUD/move for the station rows. Per-node actions
// (release, bins, changeover) live in their own files (handlers_operator_*.go).

package www

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"time"

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
		// The bins first: the containment stamp reads each tile's bin.
		stampStationBins(h.engine, v)
		stampStationCatalog(h.engine, v, time.Now())
		views := []domain.OperatorStationView{*v}
		if !enrichViewContainmentTargets(h.engine, views) {
			v.Containment = &domain.ContainmentKnown{Known: false}
		}
		stampViewCurtainState(h.engine.PLCManager(), views[0].Nodes)
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
	sourcing := styleSourcingViews(h.engine.SourcingStateForProcess(view.Process.Name))
	body, err := json.Marshal(struct {
		*domain.OperatorStationView
		// Station shadows the view's: the station's JSON carries no clock.
		Station         viewStation                  `json:"station"`
		SourcingByStyle map[string]styleSourcingView `json:"sourcing_by_style,omitempty"`
	}{
		OperatorStationView: view,
		Station:             viewStationOf(view.Station),
		SourcingByStyle:     sourcing,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeConditional(w, r, body)
}

// viewStation is the station as its own board's JSON carries it: domain.Station
// without its clocks. last_seen_at and updated_at moved on every throttled
// Touch (15 s) and nothing on the glass draws them, so an idle board's view
// changed four times a minute for nothing; created_at is drawn nowhere either.
// The Processes and Production pages keep domain.Station whole. Pinned by
// TestStationView_CarriesNoStationClock.
type viewStation struct {
	ID               int64  `json:"id"`
	ProcessID        int64  `json:"process_id"`
	Code             string `json:"code"`
	Name             string `json:"name"`
	Note             string `json:"note"`
	AreaLabel        string `json:"area_label"`
	Sequence         int    `json:"sequence"`
	ControllerNodeID string `json:"controller_node_id"`
	Enabled          bool   `json:"enabled"`
	HealthStatus     string `json:"health_status"`
	ProcessName      string `json:"process_name"`
}

func viewStationOf(s domain.Station) viewStation {
	return viewStation{
		ID: s.ID, ProcessID: s.ProcessID, Code: s.Code, Name: s.Name, Note: s.Note, AreaLabel: s.AreaLabel,
		Sequence: s.Sequence, ControllerNodeID: s.ControllerNodeID, Enabled: s.Enabled,
		HealthStatus: s.HealthStatus, ProcessName: s.ProcessName,
	}
}

// writeConditional writes a JSON body with a weak ETag of its bytes, and
// answers 304 with no body when the request quotes that ETag: an unchanged
// view costs the board no bytes, no parse and no render. The build still runs;
// the body is what tells. no-cache makes the browser ask every time. Pinned by
// TestStationView_AnswersAConditionalGet.
func writeConditional(w http.ResponseWriter, r *http.Request, body []byte) {
	sum := fnv.New64a()
	_, _ = sum.Write(body)
	etag := fmt.Sprintf(`W/"%016x"`, sum.Sum64())
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
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

// apiStationCellPicture is the cell picture's own door, and the reason the
// station VIEW no longer carries one.
//
// The picture rode every poll — two queries and a deep copy of the plant's
// NGRP map, at 500 ms a board while events flow, on a Pi with one SQLite
// connection — for a drawing that changes when an engineer edits a cell. The
// view carries a VERSION now (domain.CellPictureVersion) and the page comes
// here when the version it is holding stops matching.
//
// SHOP FLOOR, like the station's composer fetch beside it: a station has no
// login, and this is the picture an operator is looking at.
func (h *Handlers) apiStationCellPicture(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid station id")
		return
	}
	pic, err := h.engine.StationService().CellPictureForStation(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if pic == nil {
		writeError(w, http.StatusNotFound, "no such station")
		return
	}
	writeJSON(w, pic)
}
