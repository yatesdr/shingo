package protocol

import (
	"encoding/json"
	"time"
)

// ask.go — the ask pair: an Edge-to-Core question that wants an answer, on the
// order topics the rest of the traffic rides (Edge outbox → shingo.orders,
// Core outbox → shingo.dispatch). It replaces the HTTP calls an Edge made to
// Core: a write a person waits on (LOAD, count, CLEAR, hold, a home's clear)
// and a read that is a computed answer at the moment of a decision (preflight,
// release points, held partials, and the reseat a changeover cancel asks for,
// which Core answers by re-announcing the carrier).
//
// ONE SUBJECT EACH WAY. SubjectEdgeAsk carries an EdgeAsk: a kind and its
// request body (protocol/door_bodies.go). SubjectEdgeAnswer
// carries an EdgeAnswer: the same kind, the door's status and sentence, the
// door's response body, and Core's current bins rows for the nodes the act
// touched. The answer is a reply (NewDataReply): its CorID is the ask
// envelope's ID, which is how the Edge matches it to the tap that sent it.
//
// AN ANSWER IS APPLIED BY ITS KIND, waiter or none. The Edge's pending map is
// memory only; a reply that arrives after the Edge restarted still writes its
// rows to the bins table and its fact to its seat, because every answer
// carries what applying it needs.
//
// AN ASK EXPIRES; AN ANSWER DOES NOT. An ask is stamped AskTTL from its
// birth. Core answers an ask that arrives expired with AnswerExpired and does
// nothing ("expired — nothing was done"), so a waiting tile always ends; the
// ingestor's drop is not the answer for this subject. An answer carries a fact
// that has already happened at Core, so it never expires on the wire.
//
// THE KIND LIST IS FENCED (AskKinds, pinned by each side's census test): the
// ask pair must not regrow the HTTP surface one undocumented kind at a time.

const (
	// SubjectEdgeAsk — Edge → Core: an EdgeAsk.
	SubjectEdgeAsk = "edge.ask"
	// SubjectEdgeAnswer — Core → Edge: an EdgeAnswer, a reply to one ask.
	SubjectEdgeAnswer = "edge.answer"
)

// AskTTL is how long an ask stays answerable: long enough to cross a broker
// hiccup, short enough that a tile waiting on it ends while the person who
// tapped is still there.
const AskTTL = 25 * time.Second

// The ask kinds Core answers. Each names the HTTP door it replaced (the routes
// are gone; Core answers the kinds as asks only); its body is that door's
// request type and its answer body that door's response type.
const (
	AskBinLoad       = "bin.load"         // POST /api/telemetry/bin-load: BinLoadRequest → BinLoadResponse
	AskBinCount      = "bin.count"        // POST /api/telemetry/bin-count: BinCountRequest → BinCountResponse
	AskBinClear      = "bin.clear"        // POST /api/telemetry/bin-clear: BinClearRequest → BinClearResponse
	AskQualityHold   = "bin.quality_hold" // POST /api/telemetry/bin-quality-hold: its {bin_id, hold, by} body → {"ok": true}
	AskClearHome     = "clear_home"       // POST /api/telemetry/clear-home: ClearHomeRequest → ClearHomeResponse
	AskPreflight     = "preflight"        // POST /api/inventory/preflight: PreflightRequest → PreflightResponse
	AskReleasePoints = "release_points"   // POST /api/release/points: ReleasePointsRequest → ReleasePointsResponse
	AskHeldPartials  = "held_partials"    // GET /api/telemetry/held-partials: HeldPartialsRequest → []HeldPartial
	// AskReseat has no HTTP door: ReseatRequest → ReseatAnswer. Core
	// re-announces the carrier at the node through the landing verb (a
	// transit.bin_landed to the asking station, on Core's outbox ahead of the
	// answer) and answers whether one stands there.
	AskReseat = "reseat"
	// AskLoaderRetrieve: carriers asked for at a loader or an unloader, empties
	// or fulls (LoaderRetrieveRequest.Direction), by a person or by a release
	// (Origin); Core sizes and places them (dispatch.ReplenishLoader,
	// plan.LoaderWindows) and authors the orders. Born on the ask: no HTTP door
	// ever answered it.
	AskLoaderRetrieve = "loader.retrieve" // LoaderRetrieveRequest → LoaderRetrieveResponse
)

// AskKinds is every kind Core answers, in a fixed order.
func AskKinds() []string {
	return []string{AskBinLoad, AskBinCount, AskBinClear, AskQualityHold, AskClearHome,
		AskPreflight, AskReleasePoints, AskHeldPartials, AskReseat, AskLoaderRetrieve}
}

// EdgeAsk is one question. Body is the kind's request, encoded.
type EdgeAsk struct {
	Kind string          `json:"kind"`
	Body json.RawMessage `json:"body"`
}

// The answer's status, in the door's own terms (the HTTP status the door
// answered with), so a body extracted from a door answers the ask and the
// door alike.
const (
	AnswerOK       = 200
	AnswerRefused  = 409
	AnswerBad      = 400
	AnswerNotFound = 404
	AnswerFailed   = 500
	// AnswerExpired: the ask reached Core after its ExpiresAt, and Core did
	// nothing.
	AnswerExpired = 408
	// AnswerUnknownKind: this Core does not answer the kind (an older Core
	// than the Edge's census); nothing was done.
	AnswerUnknownKind = 501
)

// EdgeAnswer is Core's answer to one ask. Sentence is what a person reads
// when Status is not AnswerOK. Body is the kind's response, encoded, when the
// door gives one. Rows are Core's current bins rows for the nodes the act
// touched, written into the Edge's table before the sentence is shown.
type EdgeAnswer struct {
	Kind     string          `json:"kind"`
	Status   int             `json:"status"`
	Sentence string          `json:"sentence,omitempty"`
	Body     json.RawMessage `json:"body,omitempty"`
	Rows     []NodeBinsRow   `json:"rows,omitempty"`
}

// OK reports whether the act was done.
func (a *EdgeAnswer) OK() bool { return a != nil && a.Status == AnswerOK }

// HeldPartialsRequest is the held-partials read's one parameter, as a body (the
// retired HTTP door took it as ?payload=).
type HeldPartialsRequest struct {
	PayloadCode string `json:"payload_code"`
}

// ReseatRequest is the reseat ask's body: the node, as the asking station
// names it, whose carrier Core re-announces.
type ReseatRequest struct {
	Node string `json:"node"`
}

// ReseatAnswer is the reseat ask's answer body: the node asked about, and
// whether a carrier stands there. Landed true: its landing (BinLanded, with its
// count, epoch and a fresh placement stamp) left Core's outbox before this
// answer. Landed false: none stands there, and the station's seat at Node is
// left by the carrier it held. Node rides the answer so an Edge applies it with
// no memory of the ask (an answer after a restart).
type ReseatAnswer struct {
	Node   string `json:"node"`
	Landed bool   `json:"landed"`
}

// The values of LoaderRetrieveRequest.Direction: what each carrier is.
const (
	LoaderRetrieveEmpty = "empty" // an empty carrier, to a loader's window
	LoaderRetrieveFull  = "full"  // a full of the part, to an unloader's window
)

// The values of LoaderRetrieveRequest.Origin: who asks. A threshold is Core's
// own and never comes over the wire.
const (
	LoaderAskOperator  = "operator"   // a person: REQUEST at the loader, the manual retrieve page
	LoaderAskSideCycle = "side_cycle" // a release offered an unloader a part (the U1)
)

// LoaderRetrieveRequest is an ask for carriers at a loader: the loader's
// identity (LoaderInfo.LoaderKey), what each carrier is (Direction), who asks
// (Origin), the part, the member node the asker stands at ("" for none, which
// a dedicated loader resolves to its first position), how many, and the demand
// episode the asking serves ("" for none) with its class (OriginClass*; "" lets
// Core classify by the episode: attached with one, orphan without).
//
// Where the carriers come from. SourceNode names it; "" takes the loader's
// inbound source, and a loader with none (fed directly) is given nothing.
// AnySource, with SourceNode "", is an asker that names no source and wants
// Core to search the plant for the carrier, as for an order request that
// names none: the manual retrieve page, and a person's REQUEST FULL at an
// unloader with no inbound source.
//
// SkipAutoConfirm keeps Core's stuck-delivered sweep off the orders made, as
// on an order request (OrderRequest.SkipAutoConfirm): a carrier a person
// confirms after the physical act (a release's full-in, a person's full at a
// consume window).
//
// A manual request's staging node, load type and auto-confirm do not ride the
// ask: Core reads none of them on an order, so the asking Edge records them on
// its own rows for the orders Core made.
type LoaderRetrieveRequest struct {
	LoaderKey       string `json:"loader_key"`
	Direction       string `json:"direction"`
	Origin          string `json:"origin"`
	PayloadCode     string `json:"payload_code"`
	MemberNode      string `json:"member_node,omitempty"`
	Count           int    `json:"count"`
	OriginID        string `json:"origin_id,omitempty"`
	OriginClass     string `json:"origin_class,omitempty"`
	SourceNode      string `json:"source_node,omitempty"`
	AnySource       bool   `json:"any_source,omitempty"`
	SkipAutoConfirm bool   `json:"skip_auto_confirm,omitempty"`
}

// LoaderRetrieveResponse is what Core did with the ask: how many it was asked
// for (Want, before what is already coming is subtracted), the orders it
// made (their Edge UUIDs; each reaches the station as a projection), the
// windows it did not order to and why (HeldBy, by node), and why it made none
// at all when it made none (Skipped).
type LoaderRetrieveResponse struct {
	Want    int               `json:"want"`
	Created []string          `json:"created"`
	HeldBy  map[string]string `json:"held_by,omitempty"`
	Skipped string            `json:"skipped,omitempty"`
}
