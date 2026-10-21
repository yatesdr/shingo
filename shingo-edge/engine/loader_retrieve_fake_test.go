package engine

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
)

// coreRetrieve stands in for Core's loader.retrieve door (shingo-core
// www.Doors.AskLoaderRetrieve) on an Edge-only harness. It is a model, not the
// sizing: Core's own pins own that (dispatch loader_replenish, the consume
// vectors, plan LoaderWindows). It answers each ask as Core does in shape, for
// empties and fulls — one order per free target window, up to what the ask
// still wants, the orders projected to this Edge before the answer — so the
// Edge's flows after a carrier is ordered run as they do against a Core.
type coreRetrieve struct {
	asks atomic.Int64 // loader.retrieve asks answered
	mu   sync.Mutex
	last protocol.LoaderRetrieveRequest
	// answer, when set, is the answer given instead of the model's.
	answer func(protocol.LoaderRetrieveRequest) protocol.LoaderRetrieveResponse
	// hold, while true, leaves every ask unanswered (a Core that does not
	// answer); the asks stay on the outbox and are answered once it is false.
	hold atomic.Bool
}

func (c *coreRetrieve) lastAsk() protocol.LoaderRetrieveRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// answerLoaderAsks makes eng's Core answer loader.retrieve for the test's life.
func answerLoaderAsks(t *testing.T, eng *Engine) *coreRetrieve {
	t.Helper()
	c := &coreRetrieve{}
	eng.asks.mu.Lock()
	if eng.asks.kinds == nil {
		eng.asks.kinds = map[string]bool{}
	}
	eng.asks.kinds[protocol.AskLoaderRetrieve] = true
	eng.asks.mu.Unlock()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			if eng.PendingAsks() > 0 && !c.hold.Load() { // only while an ask is out
				c.serve(t, eng)
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })
	return c
}

// awaitAsks waits until eng has no ask out: a trigger that asks without
// waiting (a release's full-in) has had Core's answer applied.
func awaitAsks(t *testing.T, eng *Engine) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for eng.PendingAsks() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d ask(s) still out after 3s", eng.PendingAsks())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// serve answers every loader.retrieve ask on the outbox and takes it off.
func (c *coreRetrieve) serve(t *testing.T, eng *Engine) {
	msgs, err := eng.db.ListPendingOutbox(100)
	if err != nil {
		return // the test's own db is closing
	}
	for _, m := range msgs {
		if m.MsgType != protocol.SubjectEdgeAsk {
			continue
		}
		var env protocol.Envelope
		var data protocol.Data
		var ask protocol.EdgeAsk
		if json.Unmarshal(m.Payload, &env) != nil || json.Unmarshal(env.Payload, &data) != nil ||
			json.Unmarshal(data.Body, &ask) != nil || ask.Kind != protocol.AskLoaderRetrieve {
			continue
		}
		var req protocol.LoaderRetrieveRequest
		if err := json.Unmarshal(ask.Body, &req); err != nil {
			t.Errorf("decode loader.retrieve ask: %v", err)
			continue
		}
		c.mu.Lock()
		c.last = req
		answer := c.answer
		c.mu.Unlock()
		var res protocol.LoaderRetrieveResponse
		if answer != nil {
			res = answer(req)
		} else {
			res = c.model(t, eng, req)
		}
		body, err := json.Marshal(res)
		if err != nil {
			t.Errorf("encode loader.retrieve answer: %v", err)
			continue
		}
		if err := eng.db.AckOutbox(m.ID); err != nil {
			t.Errorf("take the ask off the outbox: %v", err)
		}
		c.asks.Add(1)
		eng.HandleEdgeAnswer(answerTo(&env), &protocol.EdgeAnswer{Kind: protocol.AskLoaderRetrieve, Status: protocol.AnswerOK, Body: body})
	}
}

// fakeTargets is the model's delivery targets for an ask at dl: every window of
// a shared loader (its first alone when it funnels), and on a dedicated loader
// the member position serving the part, else the first that does. Nil when
// the loader does not serve the part.
func fakeTargets(dl *domain.Loader, member, payload string) []string {
	if dl.IsShared() {
		if payload != "" && !dl.ServesPayload(domain.PayloadCode(payload)) {
			return nil
		}
		names := nodeIDStrings(dl.DeliveryNodes())
		if dl.FunnelWindows() && len(names) > 0 {
			return names[:1]
		}
		return names
	}
	for _, pos := range dl.Positions() {
		if member != "" && string(pos.Node) == member && (payload == "" || string(pos.Payload) == payload) {
			return []string{string(pos.Node)}
		}
	}
	for _, pos := range dl.Positions() {
		if string(pos.Payload) == payload {
			return []string{string(pos.Node)}
		}
	}
	return nil
}

// model is Core's answer in shape: the loader's target windows for the ask; a
// window with a carrier of the ask's direction already delivering there held
// as "window-order-open", one the bins table shows occupied held as
// "occupied"; the rest ordered to until the ask's count is met, each order
// projected here first. A person's ask subtracts what is already coming for
// the part at the targets; a release's full (the side cycle) subtracts the
// part's live fulls across every window and stops for a full of the part
// standing on a target window. A loader with no inbound source is given
// nothing unless the ask names a source or any source.
func (c *coreRetrieve) model(t *testing.T, eng *Engine, req protocol.LoaderRetrieveRequest) protocol.LoaderRetrieveResponse {
	if req.Origin == protocol.LoaderAskSideCycle {
		// A release's full-in: each part in order, each seeing the orders the
		// parts before it projected here, one outcome each.
		res := protocol.LoaderRetrieveResponse{Want: req.Count, Created: []string{}}
		for _, p := range req.Payloads {
			one := req
			one.Payloads, one.PayloadCode = nil, p
			r := c.modelPart(t, eng, one)
			res.Parts = append(res.Parts, protocol.LoaderRetrievePart{PayloadCode: p, Created: r.Created, HeldBy: r.HeldBy, Skipped: r.Skipped})
			res.Created = append(res.Created, r.Created...)
		}
		return res
	}
	return c.modelPart(t, eng, req)
}

// modelPart is the model's answer for one part.
func (c *coreRetrieve) modelPart(t *testing.T, eng *Engine, req protocol.LoaderRetrieveRequest) protocol.LoaderRetrieveResponse {
	var dl *domain.Loader
	for _, role := range []domain.LoaderRole{domain.RoleProduce, domain.RoleConsume} {
		ls, err := eng.loaders().Loaders(role)
		if err != nil {
			t.Errorf("list loaders: %v", err)
			return protocol.LoaderRetrieveResponse{}
		}
		for _, l := range ls {
			if string(l.ID()) == req.LoaderKey {
				dl = l
			}
		}
	}
	none := protocol.LoaderRetrieveResponse{Created: []string{}}
	if dl == nil {
		none.Skipped = "no such loader"
		return none
	}
	source := req.SourceNode
	if source == "" && !req.AnySource {
		source = dl.InboundSource()
		if source == "" {
			none.Skipped = "loader has no inbound source: it is fed directly, so Shingo pulls no carriers for it"
			return none
		}
	}
	empty := req.Direction == protocol.LoaderRetrieveEmpty
	names := fakeTargets(dl, req.MemberNode, req.PayloadCode)
	if len(names) == 0 {
		none.Skipped = fmt.Sprintf("loader has no delivery target for payload %q", req.PayloadCode)
		return none
	}
	scope := names
	if req.Origin == protocol.LoaderAskSideCycle {
		scope = nodeIDStrings(dl.DeliveryNodes())
	}
	active, err := eng.db.ListActiveOrdersByDeliveryNodeSet(scope)
	if err != nil {
		t.Errorf("list in flight: %v", err)
		return protocol.LoaderRetrieveResponse{}
	}
	res := protocol.LoaderRetrieveResponse{Created: []string{}, HeldBy: map[string]string{}}
	resident := map[string]bool{}
	standing := map[string]string{}
	if rows, err := eng.binsForDecision(names...); err == nil {
		for n, r := range rows {
			resident[n] = r.State == protocol.BinsStateOccupied
			if resident[n] {
				standing[n] = r.PayloadCode
			}
		}
	}
	want := req.Count
	open := map[string]bool{}
	for _, o := range active {
		if o.OrderType != orders.TypeRetrieve || o.RetrieveEmpty != empty {
			continue
		}
		open[o.DeliveryNode] = true
		if o.PayloadCode == req.PayloadCode {
			want--
		}
	}
	res.Want = req.Count
	if req.Origin == protocol.LoaderAskSideCycle {
		for _, n := range names {
			if standing[n] == req.PayloadCode {
				none.Want, none.Skipped = req.Count, fmt.Sprintf("a full of %s already stands at %s; nothing more until it is cleared", req.PayloadCode, n)
				return none
			}
		}
		if want <= 0 {
			none.Want, none.Skipped = req.Count, "this part already has a carrier outstanding"
			return none
		}
	}
	prefix := "core-l1-"
	if !empty {
		prefix = "core-u1-"
	}
	for _, n := range names {
		if open[n] {
			res.HeldBy[n] = "window-order-open"
			continue
		}
		if resident[n] {
			res.HeldBy[n] = "occupied"
			continue
		}
		if len(res.Created) >= want {
			continue
		}
		uuid := fmt.Sprintf("%s%s-%s-%d", prefix, n, req.PayloadCode, c.asks.Load()*100+int64(len(res.Created)))
		if _, err := eng.ApplyOrderProjection(protocol.OrderProjection{
			OrderUUID: uuid, OrderType: protocol.OrderTypeRetrieve, Status: "pending",
			StationID: eng.cfg.StationID(), Quantity: 1, SourceNode: source, DeliveryNode: n,
			PayloadCode: req.PayloadCode, RetrieveEmpty: empty, OriginID: req.OriginID, OriginClass: req.OriginClass,
		}); err != nil {
			t.Errorf("project %s: %v", uuid, err)
			continue
		}
		res.Created = append(res.Created, uuid)
	}
	if len(res.HeldBy) == 0 {
		res.HeldBy = nil
	}
	return res
}

// holdSweep holds, as a table eng may decide from, Core's rows for every
// place in eng's store: free, or occupied by the payload resident names. The
// fake Core (coreRetrieve.model) reads its windows' occupancy from it.
func holdSweep(t *testing.T, eng *Engine, resident map[string]string) {
	t.Helper()
	holdPlaces(t, eng, func(n string) (NodeBinInfo, bool) {
		p, ok := resident[n]
		return NodeBinInfo{NodeName: n, Occupied: ok, PayloadCode: p}, true
	})
}

// inFlightEmpties counts non-terminal retrieve_empty orders across a delivery set
// — the quantity the never-2N invariant bounds.
func inFlightEmpties(t *testing.T, db *store.DB, nodes []string) int {
	t.Helper()
	return inFlightOf(t, db, nodes, true)
}

// inFlightFulls counts non-terminal full retrieves (U1s) across a delivery set.
func inFlightFulls(t *testing.T, db *store.DB, nodes []string) int {
	t.Helper()
	return inFlightOf(t, db, nodes, false)
}

func inFlightOf(t *testing.T, db *store.DB, nodes []string, empty bool) int {
	t.Helper()
	list, err := db.ListActiveOrdersByDeliveryNodeSet(nodes)
	if err != nil {
		t.Fatalf("ListActiveOrdersByDeliveryNodeSet(%v): %v", nodes, err)
	}
	n := 0
	for _, o := range list {
		if o.RetrieveEmpty == empty {
			n++
		}
	}
	return n
}
