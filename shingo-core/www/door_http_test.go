package www

// door_http_test.go — the machine doors' HTTP adapters, kept for the doors'
// body pins only.
//
// Core answers an Edge's acts as asks only (protocol.AskKinds, Doors.AskDoors);
// the routes that served the same bodies over unauthenticated HTTP are gone
// (TestCensus_AskKinds fences them out of router.go). These are those
// adapters as they were, served to an httptest recorder, so the pins written
// against a body's status, sentence and bytes read as before. The ask adapters
// answer through the same bodies (door_ask_agreement_test.go).

import (
	"encoding/json"
	"net/http"

	"shingo/protocol"
	"shingocore/asks"
)

// serveDoor is a door's HTTP adapter: decode the request (a body that does not
// decode is a 400 with bad's sentence), call the body, write its Result.
func serveDoor[T any](w http.ResponseWriter, r *http.Request, bad func(error) string, body func(T) asks.Result) {
	var req T
	if err := decodeDoor(r.Body, &req); err != nil {
		writeResult(w, badRequest(bad(err)))
		return
	}
	writeResult(w, body(req))
}

// writeResult writes a door's Result as the doors always have: JSON, the
// status (an accept's implicit), and the body — a refusal with no body of its
// own is written as doorRefusal, the sentence and its rows, which with no
// rows is the bytes jsonError writes.
func writeResult(w http.ResponseWriter, res asks.Result) {
	body := res.Body
	if body == nil && res.Status != http.StatusOK {
		body = doorRefusal{Error: res.Sentence, Rows: res.Rows}
	}
	w.Header().Set("Content-Type", "application/json")
	if res.Status != http.StatusOK {
		w.WriteHeader(res.Status)
	}
	_ = json.NewEncoder(w).Encode(body)
}

// apiTelemetryHeldPartials lists the held partials of one part, oldest held
// first: a finish-later claim's REQUEST shows them for the operator to pick.
// GET /api/telemetry/held-partials?payload=CODE — the adapter over
// Doors.heldPartials, which the held_partials ask answers through too.
func (h *Handlers) apiTelemetryHeldPartials(w http.ResponseWriter, r *http.Request) {
	writeResult(w, h.doors().heldPartials(r.URL.Query().Get("payload")))
}

// apiBinLoad sets the manifest on the bin at a node. Direct HTTP replacement
// for bin loading — synchronous, returns updated bin state.
// POST /api/telemetry/bin-load — the adapter over Doors.binLoad, which the
// bin.load ask answers through too.
func (h *Handlers) apiBinLoad(w http.ResponseWriter, r *http.Request) {
	serveDoor(w, r, errText, h.doors().binLoad)
}

// apiBinCount records a count an operator made at the LINE, against the bin at
// a node. POST /api/telemetry/bin-count
//
// This is the missing half of a channel that ran one way. A count only ever
// travels with a DECLARATION — somebody or some lifecycle event deciding a
// number — and Core had two doors for declaring one downward (clear/load, and
// the cycle count on the bins page) while the Edge had none for declaring one
// upward except as a rider on an order release. An operator at the line who
// noticed a carrier's count was wrong outside a release had nowhere to say so,
// and the number that IS authoritative — the one at the line, next to the
// carrier — could not reach Core's ledger.
//
// It is the same act as the cycle count on the bins page and it goes through
// the same service, so it lands the same audit rows, clears the same
// go-count-this flag, and enqueues the same UOPAdjustment to every station,
// inside the count's transaction. (Until the record-count fence it broadcast
// nothing, and this comment said it did.) The only difference is who is
// standing where when they say it.
//
// The reply carries the fence (as_of_net, as_of_seq, as_of_station) along with
// the count and epoch, so the station that took the count rebases on the same
// point in its stream as the broadcast will, whichever of the two it applies
// first. Writing the reply's number over its count would lose the ticks it
// consumed while the request was out.
//
// Not an epoch bump; see store/bins.RecordCount for why.
//
// The adapter over Doors.binCount, which the bin.count ask answers through too.
func (h *Handlers) apiBinCount(w http.ResponseWriter, r *http.Request) {
	serveDoor(w, r, errText, h.doors().binCount)
}

// apiBinClear clears the manifest on the bin at a node, resetting it to empty.
// POST /api/telemetry/bin-clear
//
// Optional bin_type_code: when present the carrier's bin_type_id is updated
// atomically with the manifest clear (dunnage floating — operator declared
// which dunnage type they just loaded). An unknown code returns 400; the code
// is not checked against the types the node admits. Absent → bin_type_id is
// unchanged (existing behaviour).
//
// Optional layers: the height the person declared, in layers of totes. The
// cart's count of totes is written as layers × its own type's totes per layer
// after the clear and any stamp, in the clear's transaction, with a ledger
// row; a type that says no totes per layer gets no write. A negative height is
// refused with 400 before anything is written. Absent → nothing is declared
// and the clear is the statements it always was.
//
// TWO CARTS IGNORE THE CODE, and are stamped inside the clear's transaction
// instead (bins.ResolveBareStampTx):
//   - at a window of a stage 1 (a loader naming a second stage) the cart takes
//     its own type's bare marker. A stage 1 never re-types a cart to a real
//     type, and several cart types run through one stage 1 with nothing
//     configured. A stage 1 with nowhere to send the cart is refused with 409
//     BEFORE the clear: after it, the Edge's empty-out has no destination and a
//     bare cart would sit on the window.
//   - a bare cart anywhere else (stage 2's SEND ON) takes its carrier back. An
//     older Edge still posts PUSH AS <type> here; the cart gets its own type
//     regardless, because a bare cart is a known cart.
//
// cleared_bin_type_code in the answer is the cart's type as an operator knows
// it — the carrier, never a marker.
//
// The adapter over Doors.binClear, which the bin.clear ask answers through too.
func (h *Handlers) apiBinClear(w http.ResponseWriter, r *http.Request) {
	serveDoor(w, r, errText, h.doors().binClear)
}

// apiClearHome clears a dedicated loader's home: Core declares the carrier on
// it empty and brings a buffer partial of the home's part to it, choosing how
// (engine.ClearLoaderHome).
// POST /api/telemetry/clear-home
//
// 200 carries the arm, the clear's parent uuid, and the carrier's id and new
// delta epoch — the arm "none", with the sentence in detail, when the zero is
// recorded and no job could take the carrier. The only 409 is a second clear
// of a home whose clear is live; a request naming no clearable home is 400.
//
// The adapter over Doors.clearHome, for the station the body names; the
// clear_home ask answers through the same body for its envelope's station,
// and its request carries none.
func (h *Handlers) apiClearHome(w http.ResponseWriter, r *http.Request) {
	type stationed struct {
		protocol.ClearHomeRequest
		Station string `json:"station"`
	}
	serveDoor(w, r, errText, func(req stationed) asks.Result {
		return h.doors().clearHome(req.Station, req.ClearHomeRequest)
	})
}

// apiBinQualityHold is the machine path (telemetry group) the Edge's station
// action calls: set or clear one bin's hold marker. Keyed by bin_id — the
// Edge reads the bin off the node and carries Core's authoritative id, the
// same id its containment move order binds. The adapter over
// Doors.binQualityHold, which the bin.quality_hold ask answers through too.
func (h *Handlers) apiBinQualityHold(w http.ResponseWriter, r *http.Request) {
	var req qualityHoldRequest
	if err := decodeDoor(r.Body, &req); err != nil {
		writeResult(w, badRequest(errText(err)))
		return
	}
	res := h.doors().binQualityHold(req)
	if res.Status == http.StatusOK {
		// The accept is a bare encode with no Content-Type of its own, as it
		// always was (TestBinQualityHoldDoor_AcceptHasNoContentType).
		_ = json.NewEncoder(w).Encode(res.Body)
		return
	}
	writeResult(w, res)
}

// apiReleasePoints answers what releasing each named order would let its
// robot do next: the nodes its pending segment enters and the lifts it waits
// on (dispatch.ReleasePoints).
// POST /api/release/points — the adapter over Doors.releasePoints, for the
// station_id the body names; the release_points ask answers through the same
// body for its envelope's station, and its request carries none.
func (h *Handlers) apiReleasePoints(w http.ResponseWriter, r *http.Request) {
	type stationed struct {
		protocol.ReleasePointsRequest
		StationID string `json:"station_id"`
	}
	serveDoor(w, r, releasePointsDecodeErr, func(req stationed) asks.Result {
		return h.doors().releasePoints(req.StationID, req.ReleasePointsRequest)
	})
}

// apiInventoryPreflight wraps InventoryService.PreflightAvailability for the
// HTTP boundary: the adapter over Doors.preflight, which the preflight ask
// answers through too.
//
// Request:  protocol.PreflightRequest
// Response: protocol.PreflightResponse (missing, absent)
func (h *Handlers) apiInventoryPreflight(w http.ResponseWriter, r *http.Request) {
	serveDoor(w, r, preflightDecodeErr, func(req protocol.PreflightRequest) asks.Result {
		return h.doors().preflight(r.Context(), req)
	})
}
