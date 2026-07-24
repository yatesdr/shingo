package www

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// writeJSONWithTrigger writes JSON and adds an HX-Trigger header for htmx callers.
func writeJSONWithTrigger(w http.ResponseWriter, r *http.Request, v any, trigger string) {
	if r.Header.Get("HX-Request") == "true" && trigger != "" {
		w.Header().Set("HX-Trigger", trigger)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// actionExit is an inline recovery action attached to a refusal so the client
// can offer it — and then retry the original request — instead of dead-ending.
// Kind identifies the action for the client; URL is POSTed with an empty body
// to take it; Label is the button text.
type actionExit struct {
	Kind  string `json:"kind"`
	URL   string `json:"url"`
	Label string `json:"label"`
}

// writeErrorWithExit writes an error body that also carries an inline exit
// action, so a refusal the operator can recover from renders a button rather
// than a plain toast.
func writeErrorWithExit(w http.ResponseWriter, status int, msg string, exit actionExit) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": msg, "exit": exit})
}

func parseID(r *http.Request, param string) (int64, error) {
	s := chi.URLParam(r, param)
	return strconv.ParseInt(s, 10, 64)
}
