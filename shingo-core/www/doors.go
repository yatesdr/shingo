package www

// doors.go — the bodies of the doors an Edge asks, wire-free. Each body takes
// the asking station and its decoded request and returns an asks.Result in
// the door's own terms (the status it answers with, the sentence of a
// refusal, the response body, the rows a refusal carries). The ask
// (protocol/ask.go) decodes the request type from the ask's body, calls the
// body, and the ask server sends the Result as the answer. AskDoors is every
// kind and its body; the composition root registers them
// (cmd/shingocore/routers.go, buildAskServer). There is no HTTP door: the
// machine doors that served the same bodies over unauthenticated HTTP are
// gone.
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
	"strconv"
	"strings"

	"shingo/protocol"

	"shingocore/asks"
	"shingocore/dispatch"
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

// AskDoor is one kind Core answers on the ask pair: the body it answers
// through, and whether a repeat of the same ask is answered by re-sending the
// first answer instead of acting again.
type AskDoor struct {
	Kind   string
	Door   asks.Door
	Replay bool
}

// AskDoors is every kind Core answers (protocol.AskKinds, in its order), each
// over its body. Only the home clear replays: it authors orders and refuses a
// second clear of a live one ("already being cleared", engine/clear_home.go),
// so a repeat must get its first answer, not that refusal. Every other kind's
// repeat is harmless (the reads, the reseat's re-announce) or refused by the
// assertion fields its request carries (bin_id, epoch, expect), or bounded by
// the asking episode's outstanding orders (the empties). Pinned by
// TestCensus_AskKinds.
func (d *Doors) AskDoors() []AskDoor {
	return []AskDoor{
		{protocol.AskBinLoad, d.AskBinLoad, false},
		{protocol.AskBinCount, d.AskBinCount, false},
		{protocol.AskBinClear, d.AskBinClear, false},
		{protocol.AskQualityHold, d.AskQualityHold, false},
		{protocol.AskClearHome, d.AskClearHome, true},
		{protocol.AskPreflight, d.AskPreflight, false},
		{protocol.AskReleasePoints, d.AskReleasePoints, false},
		{protocol.AskHeldPartials, d.AskHeldPartials, false},
		{protocol.AskReseat, d.AskReseat, false},
		{protocol.AskLoaderEmpties, d.AskLoaderEmpties, false},
	}
}

// errText is a decode refusal's sentence: the decoder's own.
func errText(err error) string { return err.Error() }

// The ask adapters, one per kind (protocol.AskKinds); station is the asking
// Edge's, from the ask's envelope.

// AskBinLoad answers protocol.AskBinLoad: the LOAD's body (binLoad).
func (d *Doors) AskBinLoad(_ string, raw json.RawMessage) asks.Result {
	return askDoor(raw, errText, d.binLoad)
}

// AskBinCount answers protocol.AskBinCount: the count's body (binCount).
func (d *Doors) AskBinCount(_ string, raw json.RawMessage) asks.Result {
	return askDoor(raw, errText, d.binCount)
}

// AskBinClear answers protocol.AskBinClear: the CLEAR's body (binClear).
func (d *Doors) AskBinClear(_ string, raw json.RawMessage) asks.Result {
	return askDoor(raw, errText, d.binClear)
}

// AskQualityHold answers protocol.AskQualityHold: the hold marker's body
// (binQualityHold).
func (d *Doors) AskQualityHold(_ string, raw json.RawMessage) asks.Result {
	return askDoor(raw, errText, d.binQualityHold)
}

// AskClearHome answers protocol.AskClearHome: the home clear's body
// (clearHome), for the asking station.
func (d *Doors) AskClearHome(station string, raw json.RawMessage) asks.Result {
	return askDoor(raw, errText, func(req protocol.ClearHomeRequest) asks.Result {
		return d.clearHome(station, req)
	})
}

// AskPreflight answers protocol.AskPreflight: the preflight read's body
// (preflight), for the asking station.
func (d *Doors) AskPreflight(station string, raw json.RawMessage) asks.Result {
	return askDoor(raw, preflightDecodeErr, func(req protocol.PreflightRequest) asks.Result {
		return d.preflight(context.Background(), station, req)
	})
}

// AskReleasePoints answers protocol.AskReleasePoints: the release points
// read's body (releasePoints), for the asking station.
func (d *Doors) AskReleasePoints(station string, raw json.RawMessage) asks.Result {
	return askDoor(raw, releasePointsDecodeErr, func(req protocol.ReleasePointsRequest) asks.Result {
		return d.releasePoints(station, req)
	})
}

// AskHeldPartials answers protocol.AskHeldPartials: the held partials read's
// body (heldPartials), for the request's payload_code.
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

// AskLoaderEmpties answers protocol.AskLoaderEmpties: a person at a loader asks
// for empties, and Core sizes and places them through the loader replenishment's
// one body (dispatch.ReplenishLoader, its operator origin: Want as asked, the
// asking episode's own outstanding orders subtracted, the windows chosen by
// plan.LoaderWindows, each order born through AdmitCoreAsk and projected to
// the station). The answer is what was made and, per window not ordered to,
// why. Born on the ask: there is no HTTP door.
func (d *Doors) AskLoaderEmpties(station string, raw json.RawMessage) asks.Result {
	return askDoor(raw, errText, func(req protocol.LoaderEmptiesRequest) asks.Result {
		return d.loaderEmpties(station, req)
	})
}

func (d *Doors) loaderEmpties(station string, req protocol.LoaderEmptiesRequest) asks.Result {
	idText, isKey := strings.CutPrefix(req.LoaderKey, "loader:")
	id, err := strconv.ParseInt(idText, 10, 64)
	if !isKey || err != nil || id <= 0 {
		return badRequest(fmt.Sprintf("%q is not a loader", req.LoaderKey))
	}
	if req.Count <= 0 {
		return badRequest("ask for at least one empty")
	}
	disp := d.engine.Dispatcher()
	cfg, ok, err := disp.LoadReplenishConfig(id)
	if err != nil {
		return refused(http.StatusInternalServerError, err.Error())
	}
	if !ok {
		return refused(http.StatusNotFound, fmt.Sprintf("loader %s is not configured on Core", req.LoaderKey))
	}
	// The asker states the class (a window-free push serves no demand, and
	// says so); an older asker that does not is classified by its episode.
	origin := req.OriginClass
	if origin == "" && req.OriginID != "" {
		origin = string(protocol.OriginClassAttached)
	}
	res, err := disp.ReplenishLoader(dispatch.ReplenishRequest{
		Origin: dispatch.ReplenishByOperator, OrderType: dispatch.OrderTypeRetrieveEmpty,
		StationID: station, LoaderID: id, PayloadCode: req.PayloadCode, MemberNode: req.MemberNode,
		Want: req.Count, OriginID: req.OriginID, OriginClass: origin,
	}, cfg)
	out := protocol.LoaderEmptiesResponse{Want: res.Want, Created: []string{}, Skipped: res.Skipped}
	for _, o := range res.Created {
		out.Created = append(out.Created, o.EdgeUUID)
	}
	if len(res.HeldBy) > 0 {
		out.HeldBy = res.HeldBy
	}
	if err != nil {
		// What was made before the failure stands, and is said.
		return asks.Result{Status: http.StatusInternalServerError, Sentence: err.Error(), Body: out}
	}
	return accepted(out)
}
