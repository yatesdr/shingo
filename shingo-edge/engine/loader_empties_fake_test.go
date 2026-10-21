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
)

// coreEmpties stands in for Core's loader.empties door (shingo-core
// www.Doors.AskLoaderEmpties) on an Edge-only harness. It is a model, not the
// sizing: Core's own pins own that (dispatch loader_replenish, plan
// LoaderWindows). It answers each ask as Core does in shape — one order per
// free target window, up to what the ask still wants, the orders projected to
// this Edge before the answer — so the Edge's flows after the empty is ordered
// run as they do against a Core.
type coreEmpties struct {
	asks atomic.Int64 // loader.empties asks answered
	mu   sync.Mutex
	last protocol.LoaderEmptiesRequest
	// answer, when set, is the answer given instead of the model's.
	answer func(protocol.LoaderEmptiesRequest) protocol.LoaderEmptiesResponse
}

func (c *coreEmpties) lastAsk() protocol.LoaderEmptiesRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// answerEmpties makes eng's Core answer loader.empties for the test's life.
func answerEmpties(t *testing.T, eng *Engine) *coreEmpties {
	t.Helper()
	c := &coreEmpties{}
	eng.asks.mu.Lock()
	if eng.asks.kinds == nil {
		eng.asks.kinds = map[string]bool{}
	}
	eng.asks.kinds[protocol.AskLoaderEmpties] = true
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
			c.serve(t, eng)
		}
	}()
	t.Cleanup(func() { close(stop); <-done })
	return c
}

// serve answers every loader.empties ask on the outbox and takes it off.
func (c *coreEmpties) serve(t *testing.T, eng *Engine) {
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
			json.Unmarshal(data.Body, &ask) != nil || ask.Kind != protocol.AskLoaderEmpties {
			continue
		}
		var req protocol.LoaderEmptiesRequest
		if err := json.Unmarshal(ask.Body, &req); err != nil {
			t.Errorf("decode loader.empties ask: %v", err)
			continue
		}
		c.mu.Lock()
		c.last = req
		answer := c.answer
		c.mu.Unlock()
		var res protocol.LoaderEmptiesResponse
		if answer != nil {
			res = answer(req)
		} else {
			res = c.model(t, eng, req)
		}
		body, err := json.Marshal(res)
		if err != nil {
			t.Errorf("encode loader.empties answer: %v", err)
			continue
		}
		if err := eng.db.AckOutbox(m.ID); err != nil {
			t.Errorf("take the ask off the outbox: %v", err)
		}
		c.asks.Add(1)
		eng.HandleEdgeAnswer(answerTo(&env), &protocol.EdgeAnswer{Kind: protocol.AskLoaderEmpties, Status: protocol.AnswerOK, Body: body})
	}
}

// model is Core's answer in shape: the loader's target windows for the ask, a
// window with an empty already delivering there held as "window-order-open",
// one the bins table shows occupied held as "occupied",
// the rest ordered to until the ask's count (less what is already coming for
// the part) is met, each order projected here first.
func (c *coreEmpties) model(t *testing.T, eng *Engine, req protocol.LoaderEmptiesRequest) protocol.LoaderEmptiesResponse {
	var dl *domain.Loader
	for _, role := range []domain.LoaderRole{domain.RoleProduce, domain.RoleConsume} {
		ls, err := eng.loaders().Loaders(role)
		if err != nil {
			t.Errorf("list loaders: %v", err)
			return protocol.LoaderEmptiesResponse{}
		}
		for _, l := range ls {
			if string(l.ID()) == req.LoaderKey {
				dl = l
			}
		}
	}
	if dl == nil {
		return protocol.LoaderEmptiesResponse{Skipped: "no such loader", Created: []string{}}
	}
	nodes, _ := dl.ReservationTarget(domain.NodeID(req.MemberNode), domain.PayloadCode(req.PayloadCode), eng.multiWindowFor(dl))
	names := nodeIDStrings(nodes)
	active, err := eng.db.ListActiveOrdersByDeliveryNodeSet(names)
	if err != nil {
		t.Errorf("list in flight: %v", err)
		return protocol.LoaderEmptiesResponse{}
	}
	res := protocol.LoaderEmptiesResponse{Created: []string{}, HeldBy: map[string]string{}}
	open := map[string]bool{}
	resident := map[string]bool{}
	if rows, err := eng.binsForDecision(names...); err == nil {
		for n, r := range rows {
			resident[n] = r.State == protocol.BinsStateOccupied
		}
	}
	want := req.Count
	for _, o := range active {
		if o.RetrieveEmpty {
			open[o.DeliveryNode] = true
			if o.PayloadCode == req.PayloadCode {
				want--
			}
		}
	}
	res.Want = want
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
		uuid := fmt.Sprintf("core-l1-%s-%d", n, c.asks.Load()*100+int64(len(res.Created)))
		if _, err := eng.ApplyOrderProjection(protocol.OrderProjection{
			OrderUUID: uuid, OrderType: protocol.OrderTypeRetrieve, Status: "pending",
			StationID: eng.cfg.StationID(), Quantity: 1, SourceNode: dl.InboundSource(), DeliveryNode: n,
			PayloadCode: req.PayloadCode, RetrieveEmpty: true, OriginID: req.OriginID, OriginClass: req.OriginClass,
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
