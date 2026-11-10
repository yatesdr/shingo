package www

import (
	"encoding/json"
	"errors"
	"net/http"

	"shingoedge/engine"
)

// handlers_station_counters.go — the station counters and "counted at" (admin).

// writeStationCounterError answers a refusal 400 with its sentence, and any
// other failure 500.
func writeStationCounterError(w http.ResponseWriter, err error) {
	var r *engine.StationCounterRefused
	if errors.As(err, &r) {
		writeError(w, http.StatusBadRequest, r.Sentence)
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

// stationCounterRequest is a station counter's save: its label, its tag, and
// its optional second tag (the counter counts the two tags' sum). On an
// update a tag_name_2 left out keeps the second tag and "" removes it.
type stationCounterRequest struct {
	Label    string  `json:"label"`
	TagName  string  `json:"tag_name"`
	TagName2 *string `json:"tag_name_2"`
}

// tag2 is the request's second tag for a create: none when left out.
func (r stationCounterRequest) tag2() string {
	if r.TagName2 == nil {
		return ""
	}
	return *r.TagName2
}

func (h *Handlers) apiListStationCounters(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	list, err := h.orchestration.ListStationCounters(processID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, list)
}

func (h *Handlers) apiCreateStationCounter(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var req stationCounterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	lc, err := h.orchestration.CreateStationCounter(processID, req.Label, req.TagName, req.tag2())
	if err != nil {
		writeStationCounterError(w, err)
		return
	}
	h.requestBackup("station-counter-created")
	writeJSON(w, lc)
}

func (h *Handlers) apiUpdateStationCounter(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid station counter id")
		return
	}
	var req stationCounterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.orchestration.UpdateStationCounter(id, req.Label, req.TagName, req.TagName2); err != nil {
		writeStationCounterError(w, err)
		return
	}
	h.requestBackup("station-counter-updated")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteStationCounter(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid station counter id")
		return
	}
	if err := h.orchestration.DeleteStationCounter(id); err != nil {
		writeStationCounterError(w, err)
		return
	}
	h.requestBackup("station-counter-deleted")
	writeJSON(w, map[string]string{"status": "ok"})
}

// apiCountingSlots is the setup page's Counting block's MES slots, as the slot
// watch last read them: {"slots": [...]}.
func (h *Handlers) apiCountingSlots(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	slots, err := h.orchestration.CountingSlots(processID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"slots": slots})
}

// apiStationCountable is the station card's dry run on one position:
// {"countable": true}, or false with the refusal's sentence. It binds nothing.
func (h *Handlers) apiStationCountable(w http.ResponseWriter, r *http.Request) {
	nodeID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	err = h.orchestration.StationCountable(nodeID)
	var refusal *engine.StationCounterRefused
	switch {
	case err == nil:
		writeJSON(w, map[string]any{"countable": true, "refusal": ""})
	case errors.As(err, &refusal):
		writeJSON(w, map[string]any{"countable": false, "refusal": refusal.Sentence})
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// apiSetCountedAt binds a position to a station counter, or (null) back to the
// production counter.
func (h *Handlers) apiSetCountedAt(w http.ResponseWriter, r *http.Request) {
	nodeID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var req struct {
		LineCounterID *int64 `json:"line_counter_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.orchestration.SetCountedAt(nodeID, req.LineCounterID); err != nil {
		writeStationCounterError(w, err)
		return
	}
	h.requestBackup("counted-at")
	writeJSON(w, map[string]string{"status": "ok"})
}
