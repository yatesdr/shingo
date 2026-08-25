package www

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"shingoedge/domain"
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

// writeActionOK acknowledges an operator action that changed state the client
// will re-read anyway.
//
// ── WHY NOT THE ORDER ─────────────────────────────────────────────────────
//
// These endpoints used to serialise the whole 37-field domain.Order they had
// just created. Nothing read it: the station is event-driven — the response
// triggers a refresh and the client re-renders from the orders list — and
// every caller either awaits the promise and discards it or goes through
// postAction, which reads the body only for an error.
//
// A response body nobody reads is not free. It is a contract: every field in
// it is something a future reader may come to depend on, and something this
// service must keep shaped that way. Shrinking it to an acknowledgement says
// what is actually true — the action happened, go and look — and leaves the
// orders list as the single place order state is read from.
//
// The HX-Trigger is the part that mattered all along, and it is unchanged.
func writeActionOK(w http.ResponseWriter, r *http.Request, trigger string) {
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, trigger)
}

// writeAdvisoryRefusal declines a request that was declined for a GOOD reason —
// the work it asked for is already under way. Same status and same `error` key
// as any other refusal, so nothing that reads this endpoint today changes; the
// additive `notice` flag is what lets a client pick a colour that does not say
// "something is broken".
func writeAdvisoryRefusal(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]any{"error": msg, "notice": true})
}

// writeFieldErrors refuses with per-field detail. `error` carries the first
// message so every existing consumer — which reads exactly that key — keeps
// working unchanged; `field_errors` is additive, and is what lets a client
// render each message ON the field it is about instead of as one toast that
// does not say where to look.
func writeFieldErrors(w http.ResponseWriter, status int, findings []domain.FieldError) {
	msg := "invalid claim"
	for _, f := range findings {
		if f.Severity == domain.SeverityError {
			msg = f.Message
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": msg, "field_errors": findings})
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
