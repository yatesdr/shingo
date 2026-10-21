package www

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"shingoedge/engine"
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

// askPendingNotice is what a station shows when its tap went to Core as an ask
// and Core had not answered within the tap's wait (engine.AskWait). Never
// "failed": the request is out and the answer, when it comes, redraws the
// tile. The station holds that action's button while the answer is out.
//
// OWNER-WORDS
const askPendingNotice = "sent — waiting for Core"

// writeAskPending answers an operator action whose ask is still out (err is
// engine.ErrAskPending): 202 with pending true and the notice, which the
// station shows as a notice and not an error (operator-util.js postAction).
// false, writing nothing, for any other error.
func writeAskPending(w http.ResponseWriter, r *http.Request, err error) bool {
	if !errors.Is(err, engine.ErrAskPending) {
		return false
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Trigger", "refreshMaterial")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending", "pending": true, "notice": askPendingNotice})
	return true
}
