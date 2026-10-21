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
// the windows and what is already coming (the loader retrieve: ReplenishLoader
// under its loader's lock). Pinned by TestCensus_AskKinds.
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
		{protocol.AskLoaderRetrieve, d.AskLoaderRetrieve, false},
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
// (preflight).
func (d *Doors) AskPreflight(_ string, raw json.RawMessage) asks.Result {
	return askDoor(raw, preflightDecodeErr, func(req protocol.PreflightRequest) asks.Result {
		return d.preflight(context.Background(), req)
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

// AskLoaderRetrieve answers protocol.AskLoaderRetrieve: carriers asked for at a
// loader, empties or fulls, by a person (the operator origin) or by a release
// (the side-cycle origin), and Core sizes and places them through the loader
// replenishment's one body (dispatch.ReplenishLoader: Want as asked, what is
// already coming subtracted by the origin's rule, the windows chosen by
// plan.LoaderWindows, each order born through AdmitCoreAsk and projected to
// the station). The answer is what was made and, per window not ordered to,
// why. Born on the ask: there is no HTTP door.
func (d *Doors) AskLoaderRetrieve(station string, raw json.RawMessage) asks.Result {
	return askDoor(raw, errText, func(req protocol.LoaderRetrieveRequest) asks.Result {
		return d.loaderRetrieve(station, req)
	})
}

// loaderRetrieveKinds maps the ask's stated direction and origin onto
// ReplenishLoader's. A threshold is Core's own and is not asked for.
var (
	loaderRetrieveDirections = map[string]protocol.OrderType{
		protocol.LoaderRetrieveEmpty: dispatch.OrderTypeRetrieveEmpty,
		protocol.LoaderRetrieveFull:  dispatch.OrderTypeRetrieve,
	}
	loaderRetrieveOrigins = map[string]dispatch.ReplenishOrigin{
		protocol.LoaderAskOperator:  dispatch.ReplenishByOperator,
		protocol.LoaderAskSideCycle: dispatch.ReplenishBySideCycle,
	}
)

func (d *Doors) loaderRetrieve(station string, req protocol.LoaderRetrieveRequest) asks.Result {
	idText, isKey := strings.CutPrefix(req.LoaderKey, "loader:")
	id, err := strconv.ParseInt(idText, 10, 64)
	if !isKey || err != nil || id <= 0 {
		return badRequest(fmt.Sprintf("%q is not a loader", req.LoaderKey))
	}
	pull, ok := loaderRetrieveDirections[req.Direction]
	if !ok {
		return badRequest(fmt.Sprintf("direction %q is neither %q nor %q", req.Direction, protocol.LoaderRetrieveEmpty, protocol.LoaderRetrieveFull))
	}
	asker, ok := loaderRetrieveOrigins[req.Origin]
	if !ok {
		return badRequest(fmt.Sprintf("origin %q is neither %q nor %q", req.Origin, protocol.LoaderAskOperator, protocol.LoaderAskSideCycle))
	}
	if req.Count <= 0 {
		return badRequest("ask for at least one carrier")
	}
	// One shape per origin: a release's full-in names its parts in payloads, a
	// person names one in payload_code.
	if (asker == dispatch.ReplenishBySideCycle) != (len(req.Payloads) > 0) {
		return badRequest("a release's full-in names its parts in payloads; a person's ask names one in payload_code")
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
	// says so); an asker that does not is classified by its episode.
	origin := req.OriginClass
	if origin == "" && req.OriginID != "" {
		origin = string(protocol.OriginClassAttached)
	}
	res, err := disp.ReplenishLoader(dispatch.ReplenishRequest{
		Origin: asker, OrderType: pull, SourceNode: req.SourceNode, AnySource: req.AnySource,
		SkipAutoConfirm: req.SkipAutoConfirm,
		StationID:       station, LoaderID: id, PayloadCode: req.PayloadCode, Payloads: req.Payloads, MemberNode: req.MemberNode,
		Want: req.Count, OriginID: req.OriginID, OriginClass: origin,
	}, cfg)
	out := protocol.LoaderRetrieveResponse{Want: res.Want, Created: []string{}, Skipped: res.Skipped}
	for _, o := range res.Created {
		out.Created = append(out.Created, o.EdgeUUID)
	}
	if len(res.HeldBy) > 0 {
		out.HeldBy = res.HeldBy
	}
	for _, p := range res.Parts {
		part := protocol.LoaderRetrievePart{PayloadCode: p.PayloadCode, Created: []string{}, Skipped: p.Skipped}
		for _, o := range p.Created {
			part.Created = append(part.Created, o.EdgeUUID)
		}
		if len(p.HeldBy) > 0 {
			part.HeldBy = p.HeldBy
		}
		out.Parts = append(out.Parts, part)
	}
	if err != nil {
		// What was made before the failure stands, and is said.
		return asks.Result{Status: http.StatusInternalServerError, Sentence: err.Error(), Body: out}
	}
	return accepted(out)
}
