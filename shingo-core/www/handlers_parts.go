package www

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// Parts section endpoints (plan §3.E). since/until reuse parseMissionFilter;
// ?top= caps the rows (default 10).

func partsTop(r *http.Request) int {
	if t := r.URL.Query().Get("top"); t != "" {
		if n, err := strconv.Atoi(t); err == nil && n > 0 {
			return n
		}
	}
	return 10
}

func (h *Handlers) apiPartsProduced(w http.ResponseWriter, r *http.Request) {
	f := parseMissionFilter(r)
	rows, err := h.engine.PartsService().Produced(f.Since, f.Until, partsTop(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"rows": rows})
}

func (h *Handlers) apiPartsCycleTime(w http.ResponseWriter, r *http.Request) {
	f := parseMissionFilter(r)
	rows, err := h.engine.PartsService().CycleTime(f.Since, f.Until, partsTop(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"rows": rows})
}

func (h *Handlers) apiPartsConsumption(w http.ResponseWriter, r *http.Request) {
	f := parseMissionFilter(r)
	rows, err := h.engine.PartsService().Consumption(f.Since, f.Until, partsTop(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"rows": rows})
}
