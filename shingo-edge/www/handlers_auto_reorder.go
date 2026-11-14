package www

import (
	"encoding/json"
	"net/http"

	"shingoedge/domain"
)

// handlers_auto_reorder.go — a line's auto-reorder switch (engine
// FlipAutoReorder): Processes' read and its two writes, and the station
// board's flip where an engineer has allowed it.

// apiGetAutoReorderSwitch is Processes' read of the switch: the setting, the
// positions a person has switched off, and the last flips.
func (h *Handlers) apiGetAutoReorderSwitch(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	sw, err := h.engine.ProcessService().AutoReorderSwitch(processID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, sw)
}

// apiFlipAutoReorder is Processes' flip, by the session's user.
func (h *Handlers) apiFlipAutoReorder(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	by := "admin"
	if u, ok := h.sessions.getUser(r); ok && u != "" {
		by = u
	}
	if err := h.orchestration.FlipAutoReorder(processID, req.On, by, domain.FlipFromProcesses); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// apiSetAutoReorderSwitchHMI shows or hides the switch on the station board.
func (h *Handlers) apiSetAutoReorderSwitchHMI(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var req struct {
		Shown bool `json:"shown"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.ProcessService().SetAutoReorderSwitchHMI(processID, req.Shown); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// apiStationFlipAutoReorder is the station board's flip of its line's switch,
// by the station's name. The engine refuses it where Processes has not shown
// the switch on the station.
func (h *Handlers) apiStationFlipAutoReorder(w http.ResponseWriter, r *http.Request) {
	stationID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid station id")
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	station, err := h.engine.StationService().Get(stationID)
	if err != nil || station == nil {
		writeError(w, http.StatusNotFound, "station not found")
		return
	}
	if err := h.orchestration.FlipAutoReorder(station.ProcessID, req.On, station.Name, domain.FlipFromStation); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}
