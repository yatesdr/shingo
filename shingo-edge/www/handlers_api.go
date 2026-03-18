package www

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"shingoedge/store"
)

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func parseID(r *http.Request, param string) (int64, error) {
	s := chi.URLParam(r, param)
	return strconv.ParseInt(s, 10, 64)
}

// resolvePayload loads a payload by ID and returns a pointer for order linking.
// Returns (nil, nil) if id is zero. Returns (ptr, nil) if the payload can't be found.
func (h *Handlers) resolvePayload(id int64) (payloadPtr *int64, p *store.Payload) {
	if id <= 0 {
		return nil, nil
	}
	payloadPtr = &id
	payload, err := h.engine.DB().GetPayload(id)
	if err != nil {
		return payloadPtr, nil
	}
	return payloadPtr, payload
}
