package www

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"shingocore/store"
)

func (h *Handlers) apiCreateSupermarket(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string           `json:"name"`
		Zone         string           `json:"zone"`
		Lanes        []store.LaneSetup `json:"lanes"`
		ShuffleSlots []string         `json:"shuffle_slots"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	if req.Name == "" || len(req.Lanes) == 0 {
		h.jsonError(w, "name and at least one lane required", http.StatusBadRequest)
		return
	}

	// Expand vendor location patterns
	for i := range req.Lanes {
		req.Lanes[i].VendorLocations = expandLocations(req.Lanes[i].VendorLocations, req.Lanes[i].Depth)
	}

	result, err := h.engine.DB().CreateSupermarket(store.SupermarketSetup{
		Name:         req.Name,
		Zone:         req.Zone,
		Lanes:        req.Lanes,
		ShuffleSlots: req.ShuffleSlots,
	})
	if err != nil {
		h.jsonError(w, "create supermarket: "+err.Error(), http.StatusInternalServerError)
		return
	}

	h.jsonOK(w, map[string]any{"id": result.SupermarketID, "name": result.Name})
}

func (h *Handlers) apiGetSupermarketLayout(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}

	layout, err := h.engine.DB().GetSupermarketLayout(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, layout)
}

func (h *Handlers) apiDeleteSupermarket(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	if err := h.engine.DB().DeleteSupermarket(req.ID); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// expandLocations expands vendor location patterns or uses them as-is.
func expandLocations(locs []string, depth int) []string {
	if len(locs) == 0 {
		return nil
	}
	if len(locs) == 1 && strings.Contains(locs[0], "{") {
		return expandPattern(locs[0], depth)
	}
	return locs
}

// expandPattern expands "LOC-A{1-10}" into ["LOC-A1", "LOC-A2", ..., "LOC-A10"]
func expandPattern(pattern string, maxCount int) []string {
	openIdx := strings.Index(pattern, "{")
	closeIdx := strings.Index(pattern, "}")
	if openIdx < 0 || closeIdx < 0 || closeIdx <= openIdx {
		return []string{pattern}
	}

	prefix := pattern[:openIdx]
	suffix := pattern[closeIdx+1:]
	rangeStr := pattern[openIdx+1 : closeIdx]

	parts := strings.SplitN(rangeStr, "-", 2)
	if len(parts) != 2 {
		return []string{pattern}
	}

	start, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	end, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return []string{pattern}
	}

	var result []string
	for i := start; i <= end && len(result) < maxCount; i++ {
		result = append(result, fmt.Sprintf("%s%d%s", prefix, i, suffix))
	}
	return result
}
