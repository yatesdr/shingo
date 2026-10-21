package www

// doors.go — the bodies of the doors an Edge calls, wire-free: one body, two
// adapters. Each body takes the asking station and its decoded request and
// returns an asks.Result in the door's own terms (the HTTP status it answers
// with, the sentence of a refusal, the response body, the rows a refusal
// carries). The HTTP handler (handlers_telemetry.go, handlers_containment.go,
// handlers_release_points.go, handlers_inventory_preflight.go) decodes the
// request, calls the body and writes the Result; the ask (protocol/ask.go)
// decodes the same request type from the ask's body, calls the same body, and
// the ask server sends the Result as the answer. The composition root
// registers each kind (cmd/shingocore/routers.go, askDoors).
//
// WHY HERE AND NOT IN A PACKAGE OF THEIR OWN: the bodies are the doors' code
// as it was, over what the doors already reach — the engine's services
// (ServiceAccess, EngineOrchestration) and Core's own pages' event hub, which
// a body broadcasts to after a write so a Core screen redraws whoever asked.
// Both live in www, and www already imports what the bodies need; asks
// imports only protocol, so www importing asks for its Result makes no cycle.
// A new package would have to take the hub from www or lose the broadcast.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"shingo/protocol"

	"shingocore/asks"
	"shingocore/engine"
	"shingocore/service"
)

// Doors holds the door bodies' dependencies. The router's Handlers build one
// per call (Handlers.doors); the composition root builds the one the ask
// server answers through (NewDoors) and hands it to NewRouter, whose event
// hub it is.
type Doors struct {
	engine        ServiceAccess
	orchestration EngineOrchestration
	hub           *EventHub
}

// NewDoors returns the door bodies over eng, with the event hub NewRouter
// starts and serves. Until the router starts it, a broadcast waits in the
// hub's buffer.
func NewDoors(eng *engine.Engine) *Doors {
	return &Doors{engine: eng, orchestration: eng, hub: NewEventHub()}
}

// doors is the bodies over this router's engine and hub.
func (h *Handlers) doors() *Doors {
	return &Doors{engine: h.engine, orchestration: h.orchestration, hub: h.eventHub}
}

// decodeDoor is the one decode both adapters use, so a body that does not
// decode is refused with the same sentence whichever way it came.
func decodeDoor(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }

// badRequest is a refusal of a request the door cannot read or that lacks a
// field it needs: the sentence only, no rows.
func badRequest(msg string) asks.Result {
	return asks.Result{Status: http.StatusBadRequest, Sentence: msg}
}

// refused is a refusal carrying only its sentence, at status.
func refused(status int, msg string) asks.Result {
	return asks.Result{Status: status, Sentence: msg}
}

// accepted is an accept answering body.
func accepted(body any) asks.Result { return asks.Result{Status: http.StatusOK, Body: body} }

// refuseAt is a write door's refusal of a request that named node: the
// sentence with Core's current rows for the node (NodeService.BinsRows; one
// read snapshot). An unknown name answers its unknown row.
func (d *Doors) refuseAt(node string, status int, msg string) asks.Result {
	return asks.Result{Status: status, Sentence: msg, Rows: d.engine.NodeService().BinsRows(node)}
}

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

// askDoor is a door's ask adapter: the same decode over the ask's body (a
// body that does not decode is AnswerBad with the same sentence), the same
// body.
func askDoor[T any](raw json.RawMessage, bad func(error) string, body func(T) asks.Result) asks.Result {
	var req T
	if err := decodeDoor(bytes.NewReader(raw), &req); err != nil {
		return badRequest(bad(err))
	}
	return body(req)
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

// errText is a decode refusal's sentence: the decoder's own.
func errText(err error) string { return err.Error() }

// The ask adapters, one per kind (protocol.AskKinds); station is the asking
// Edge's, from the ask's envelope.

// AskBinLoad answers protocol.AskBinLoad: POST /api/telemetry/bin-load's body.
func (d *Doors) AskBinLoad(_ string, raw json.RawMessage) asks.Result {
	return askDoor(raw, errText, d.binLoad)
}

// AskBinCount answers protocol.AskBinCount: POST /api/telemetry/bin-count's
// body.
func (d *Doors) AskBinCount(_ string, raw json.RawMessage) asks.Result {
	return askDoor(raw, errText, d.binCount)
}

// AskBinClear answers protocol.AskBinClear: POST /api/telemetry/bin-clear's
// body.
func (d *Doors) AskBinClear(_ string, raw json.RawMessage) asks.Result {
	return askDoor(raw, errText, d.binClear)
}

// AskQualityHold answers protocol.AskQualityHold: POST
// /api/telemetry/bin-quality-hold's body.
func (d *Doors) AskQualityHold(_ string, raw json.RawMessage) asks.Result {
	return askDoor(raw, errText, d.binQualityHold)
}

// AskClearHome answers protocol.AskClearHome: POST /api/telemetry/clear-home's
// body, for the asking station.
func (d *Doors) AskClearHome(station string, raw json.RawMessage) asks.Result {
	return askDoor(raw, errText, func(req protocol.ClearHomeRequest) asks.Result {
		return d.clearHome(station, req)
	})
}

// AskPreflight answers protocol.AskPreflight: POST /api/inventory/preflight's
// body, for the asking station.
func (d *Doors) AskPreflight(station string, raw json.RawMessage) asks.Result {
	return askDoor(raw, preflightDecodeErr, func(req protocol.PreflightRequest) asks.Result {
		return d.preflight(context.Background(), station, req)
	})
}

// AskReleasePoints answers protocol.AskReleasePoints: POST /api/release/points'
// body, for the asking station.
func (d *Doors) AskReleasePoints(station string, raw json.RawMessage) asks.Result {
	return askDoor(raw, releasePointsDecodeErr, func(req protocol.ReleasePointsRequest) asks.Result {
		return d.releasePoints(station, req)
	})
}

// AskHeldPartials answers protocol.AskHeldPartials: GET
// /api/telemetry/held-partials' read, the ?payload= it takes as the
// request's payload_code.
func (d *Doors) AskHeldPartials(_ string, raw json.RawMessage) asks.Result {
	return askDoor(raw, errText, func(req protocol.HeldPartialsRequest) asks.Result {
		return d.heldPartials(req.PayloadCode)
	})
}

// AskReseat answers protocol.AskReseat, which has no HTTP door: Core
// re-announces the carrier at the node to the asking station
// (service.BinService.Reseat) and answers whether one stands there. A node
// Core does not know is refused (AnswerNotFound), never answered as empty.
func (d *Doors) AskReseat(station string, raw json.RawMessage) asks.Result {
	return askDoor(raw, errText, func(req protocol.ReseatRequest) asks.Result {
		return d.reseat(station, req)
	})
}

// reseat is AskReseat's body.
func (d *Doors) reseat(station string, req protocol.ReseatRequest) asks.Result {
	if req.Node == "" {
		return badRequest("node is required")
	}
	landed, err := d.engine.BinService().Reseat(station, req.Node)
	switch {
	case errors.Is(err, service.ErrReseatUnknownNode):
		return refused(http.StatusNotFound, fmt.Sprintf("Core has no node %s", req.Node))
	case err != nil:
		return refused(http.StatusInternalServerError, err.Error())
	}
	return accepted(protocol.ReseatAnswer{Node: req.Node, Landed: landed})
}
