// Lost and late count declarations: the four rows of the round-3 count table
// (SYNTH-round3 §2, a design record outside the repo), as the base (v0.18.0)
// behaves. Core states a count for a carrier at a line seat — a person's count
// (BinService.RecordCount, fenced by countFence, announced by announceCount)
// or a reset (BinManifestService.bumpEpoch's declaration) — and the station
// loses it or takes it late. The info-flow build's lane C6 fences every count
// Core states and lane B2 puts the adjustment and the refresh on the
// must-arrive list; rows (a), (b) and (d) are expected to change then, and
// each test says how. A changed precondition (the declaration's shape, the
// loss) is reported and the test runs on, so its books say what the row now
// does.
//
// Real: Core against Postgres (the count and its fence, the reset doors and
// their declarations, the delta applier with its stale drop and its epoch
// refresh, the outbox rows) and the outbox drainer both sides run
// (protocol/outbox.Drainer) over Core's outbox table; the Edge engine against
// SQLite (the counter-delta handler, the accumulator, HandleUOPAdjustment
// with its fence, HandleBinEpochRefresh), reached through a protocol ingestor
// and subject router wired for the two subjects as cmd/shingoedge/main.go
// wires them (the Edge harness's router does not carry them).
//
// Carried by hand: the broker (declBroker: up, down, or refusing one row while
// the rest go), when the Edge reads what the broker took (in the order it took
// it), and the Edge's flushed deltas to Core's applier
// (twoBooks.shipEdgeDeltas). Core's drainer runs only inside an outage or a
// pass, so nothing is published between the steps a test takes.
//
//go:build docker

package scenarios

import (
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/outbox"
	"shingo/protocol/router"

	corestore "shingocore/store"

	edgeengine "shingoedge/engine"
	edgeuop "shingoedge/uop"
)

// declRig is twoBooks with what a count declaration needs to travel: the
// station registered at Core (no claims mirror here, so Core addresses its
// announcements by the last arm, to every registered Edge), no order holding
// the carrier (an admin Load Payload refuses a held one), the Edge's
// accumulator in hand, the broker Core's drainer publishes to, and the Edge's
// consumer of it.
type declRig struct {
	*twoBooks
	acc      *edgeuop.Mutator
	broker   *declBroker
	consumer *protocol.Ingestor
	mu       sync.Mutex
	read     []string // what the Edge's handlers were given for this bin, in order
}

func newDeclRig(t *testing.T, label string, count int) *declRig {
	t.Helper()
	w := &declRig{twoBooks: newTwoBooks(t, label, count),
		broker: &declBroker{refused: map[string]bool{}, offers: map[string]int{}}}
	w.acc = edgeuop.New(w.edge.DB, handoffStation, w.edge.DB, w.edge.DB)
	w.edge.Engine.SetInventoryDeltaSink(w.acc)
	if _, err := w.core.Exec(`INSERT INTO edge_registry (station_uid, schema_version) VALUES ($1, $2)`,
		handoffStation, protocol.EdgeSchemaBinLanded); err != nil {
		t.Fatalf("register the station: %v", err)
	}
	if err := w.core.ReleaseClaimForBin(w.binID, w.order.ID); err != nil {
		t.Fatalf("release the order's claim: %v", err)
	}
	// The Edge's consumer for the two subjects, as cmd/shingoedge/main.go
	// routes them.
	subjects := router.NewSubject()
	router.RegisterSubject(subjects, protocol.SubjectUOPAdjustment, func(_ *protocol.Envelope, adj *protocol.UOPAdjustment) {
		if adj.BinID == w.binID {
			w.note(protocol.SubjectUOPAdjustment)
		}
		w.edge.Engine.HandleUOPAdjustment(*adj)
	})
	router.RegisterSubject(subjects, protocol.SubjectBinEpochRefresh, func(_ *protocol.Envelope, r *protocol.BinEpochRefresh) {
		if r.BinID == w.binID {
			w.note(protocol.SubjectBinEpochRefresh)
		}
		w.edge.Engine.HandleBinEpochRefresh(*r)
	})
	types := router.New[string]()
	router.Register(types, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) {
		subjects.Dispatch(env, p)
	})
	w.consumer = protocol.NewIngestor(func(hdr *protocol.RawHeader) bool {
		return hdr.Dst.Station == handoffStation || hdr.Dst.Station == protocol.StationBroadcast
	})
	w.consumer.Dispatch = func(env *protocol.Envelope) { types.Dispatch(env, env.Type) }
	return w
}

func (w *declRig) note(subject string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.read = append(w.read, subject)
}

// readByEdge returns what the Edge's handlers were given for this bin since the
// last call.
func (w *declRig) readByEdge() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.read
	w.read = nil
	return out
}

// strokes is n consume strokes on the Edge, flushed into the Edge's outbox as
// one window.
func (w *declRig) strokes(n int) {
	w.t.Helper()
	for i := 0; i < n; i++ {
		w.edge.Engine.Events.Emit(edgeengine.Event{Type: edgeengine.EventCounterDelta,
			Payload: edgeengine.CounterDeltaEvent{ProcessID: w.process, StyleID: w.style, Delta: 1}})
	}
	w.acc.Flush()
}

// books reads the count and the generation each side holds for the carrier.
func (w *declRig) books() (edge int, edgeEpoch int64, core int, coreEpoch int64) {
	w.t.Helper()
	rt := w.edgeRuntime()
	if err := w.core.QueryRow(`SELECT uop_remaining, delta_epoch FROM bins WHERE id=$1`, w.binID).Scan(&core, &coreEpoch); err != nil {
		w.t.Fatalf("read core bin: %v", err)
	}
	return rt.RemainingUOPCached, rt.ActiveBinEpoch, core, coreEpoch
}

// declRow is one of Core's outbox rows for this bin, decoded.
type declRow struct {
	id      int64
	payload []byte
	retries int
	sent    bool
	adj     protocol.UOPAdjustment // for an adjustment row
}

// coreRows lists Core's outbox rows of subject for this bin, oldest first.
func (w *declRig) coreRows(subject string) []declRow {
	w.t.Helper()
	rows, err := w.core.Query(`SELECT id, payload, retries, sent_at IS NOT NULL FROM outbox WHERE msg_type=$1 ORDER BY id`,
		"data."+subject)
	if err != nil {
		w.t.Fatalf("read core outbox: %v", err)
	}
	defer rows.Close()
	var out []declRow
	for rows.Next() {
		var r declRow
		if err := rows.Scan(&r.id, &r.payload, &r.retries, &r.sent); err != nil {
			w.t.Fatalf("scan: %v", err)
		}
		var env protocol.Envelope
		if err := json.Unmarshal(r.payload, &env); err != nil {
			w.t.Fatalf("decode envelope: %v", err)
		}
		var data protocol.Data
		if err := env.DecodePayload(&data); err != nil {
			w.t.Fatalf("decode data: %v", err)
		}
		var bin struct {
			BinID int64 `json:"bin_id"`
		}
		if err := json.Unmarshal(data.Body, &bin); err != nil {
			w.t.Fatalf("decode body: %v", err)
		}
		if bin.BinID != w.binID {
			continue
		}
		if subject == protocol.SubjectUOPAdjustment {
			if err := json.Unmarshal(data.Body, &r.adj); err != nil {
				w.t.Fatalf("decode adjustment: %v", err)
			}
		}
		out = append(out, r)
	}
	return out
}

// declaration is Core's one adjustment row for this bin since the rig was
// built: the count's or the reset's declaration.
func (w *declRig) declaration() declRow {
	w.t.Helper()
	rows := w.coreRows(protocol.SubjectUOPAdjustment)
	if len(rows) != 1 {
		w.t.Fatalf("Core queued %d adjustments for the bin, want 1 (the declaration)", len(rows))
	}
	return rows[0]
}

// declText is a declaration as a failure message names it: count, generation,
// actor and fence.
func declText(adj protocol.UOPAdjustment) string {
	fence := "unfenced"
	if adj.AsOfNet != nil && adj.AsOfSeq != nil {
		fence = "fenced at net " + strconv.FormatInt(*adj.AsOfNet, 10) + " seq " +
			strconv.FormatInt(*adj.AsOfSeq, 10) + " for " + adj.AsOfStation
	}
	return strconv.Itoa(adj.NewRemaining) + " at epoch " + strconv.FormatInt(adj.Epoch, 10) +
		" by " + strconv.Quote(adj.Actor) + ", " + fence
}

func (w *declRig) coreRow(id int64) declRow {
	w.t.Helper()
	var r declRow
	if err := w.core.QueryRow(`SELECT id, payload, retries, sent_at IS NOT NULL FROM outbox WHERE id=$1`, id).
		Scan(&r.id, &r.payload, &r.retries, &r.sent); err != nil {
		w.t.Fatalf("read core outbox row %d: %v", id, err)
	}
	return r
}

// drainUntil runs Core's outbox drainer at interval until done, then stops it.
func (w *declRig) drainUntil(interval time.Duration, what string, done func() bool) {
	w.t.Helper()
	d := outbox.NewDrainer(declOutbox{w.core}, w.broker, "shingo.dispatch", interval, 50)
	d.Start()
	defer d.Stop()
	deadline := time.Now().Add(30 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			w.t.Fatalf("Core's drainer did not reach %s in 30 s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// outage holds the broker down until Core's drainer has dead-lettered row
// (its retries spent) or offered it more than outbox.MaxRetries times and
// still holds it. Reports whether it was dead-lettered.
func (w *declRig) outage(row declRow) (deadLettered bool) {
	w.t.Helper()
	w.broker.set(func(b *declBroker) { b.down = true })
	w.drainUntil(2*time.Millisecond, "the end of the outage", func() bool {
		return w.coreRow(row.id).retries >= outbox.MaxRetries || w.broker.offered(row.payload) > outbox.MaxRetries
	})
	w.broker.set(func(b *declBroker) { b.down = false })
	return w.coreRow(row.id).retries >= outbox.MaxRetries
}

// pass runs one pass of Core's drainer with the broker up (bar the rows it
// refuses) and returns what the broker took in it, oldest first: every row
// pending when it starts is offered once. A second pass slipping in before the
// stop offers a refused row once more, well inside its retries.
func (w *declRig) pass() [][]byte {
	w.t.Helper()
	pending, err := w.core.ListPendingOutbox(1000)
	if err != nil {
		w.t.Fatalf("list core outbox: %v", err)
	}
	before := map[string]int{}
	for _, m := range pending {
		before[string(m.Payload)] = w.broker.offered(m.Payload)
	}
	if len(pending) > 0 {
		w.drainUntil(100*time.Millisecond, "the end of one pass", func() bool {
			for _, m := range pending {
				if w.broker.offered(m.Payload) <= before[string(m.Payload)] {
					return false
				}
			}
			return true
		})
	}
	return w.broker.takeAll()
}

// edgeReads hands what the broker took to the Edge's consumer, in that order.
func (w *declRig) edgeReads(taken [][]byte) {
	for _, raw := range taken {
		w.consumer.HandleRaw(raw)
	}
}

// declBroker is the broker Core's drainer publishes to: down, or up and
// refusing the payloads in refused while the rest go (a publish failing on one
// row while the drainer goes on to the next is the drainer's behaviour, pinned
// by TestPin_P0c_PartialFailureReordersAScope). It keeps what it took, in
// order, for the Edge.
type declBroker struct {
	mu      sync.Mutex
	down    bool
	refused map[string]bool
	offers  map[string]int
	taken   [][]byte
}

func (b *declBroker) Publish(_ string, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.offers[string(payload)]++
	if b.down || b.refused[string(payload)] {
		return errors.New("broker unreachable")
	}
	b.taken = append(b.taken, append([]byte(nil), payload...))
	return nil
}

func (b *declBroker) IsConnected() bool { return true }

func (b *declBroker) set(f func(*declBroker)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f(b)
}

func (b *declBroker) offered(payload []byte) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.offers[string(payload)]
}

func (b *declBroker) takeAll() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.taken
	b.taken = nil
	return out
}

// declOutbox is Core's outbox table as the drainer reads it: the adapter
// shingocore/messaging's NewOutboxDrainer builds, which needs a Kafka client.
type declOutbox struct{ db *corestore.DB }

func (o declOutbox) ListPendingOutbox(limit int) ([]outbox.Message, error) {
	rows, err := o.db.ListPendingOutbox(limit)
	if err != nil {
		return nil, err
	}
	out := make([]outbox.Message, len(rows))
	for i, m := range rows {
		out[i] = outbox.Message{ID: m.ID, Topic: m.Topic, Payload: m.Payload, MsgType: m.MsgType, Retries: m.Retries}
	}
	return out, nil
}

func (o declOutbox) AckOutbox(id int64) error              { return o.db.AckOutbox(id) }
func (o declOutbox) IncrementOutboxRetries(id int64) error { return o.db.IncrementOutboxRetries(id) }
func (o declOutbox) MarkOutboxExhausted(id int64, reason string) error {
	return o.db.MarkOutboxExhausted(id, reason)
}
func (o declOutbox) PurgeOldOutbox(delivered, deadLetter time.Duration) (int, error) {
	n, err := o.db.PurgeOldOutbox(delivered, deadLetter)
	return int(n), err
}

// declResets are the two declarers of a reset, each through a real door that
// leaves the carrier at the seat with a new count and generation: a person's
// Load Payload (the bins page, BinService.LoadPayload) and a press finishing
// the carrier (produce finalize, BinManifestService.RecordProducedBin). The
// seat's role should not matter: HandleUOPAdjustment reads the declaration's
// actor and fence, not the claim's role.
var declResets = []struct {
	name  string
	actor string
	reset func(w *declRig, n int) error
}{
	{"a_persons_load", protocol.AuditActorUI, func(w *declRig, n int) error {
		return w.bins.LoadPayload(w.binID, w.payload, &n, protocol.DeclaredByPerson)
	}},
	{"a_press_finishing_it", protocol.ActorCoreLifecycle, func(w *declRig, n int) error {
		m := `{"items":[{"catid":"` + w.payload + `","qty":` + strconv.Itoa(n) + `}]}`
		return w.manifest.RecordProducedBin(w.binID, m, w.payload, n, "", nil)
	}},
}

// TestScenario_CountDecl_APersonsCountLostEachSideKeepsItsOwn is row (a). The
// station has counted 3 (both books 97) when a person counts 50 at Core. Core
// fences the count at the station's cursor (net -3, seq 1) and queues it; an
// outage longer than the retry budget follows and Core's drainer dead-letters
// it (inventory.uop_adjustment is not on the never-dead-letter list). A count
// ends no generation, so the station's next 5 land at Core, nothing is stale,
// and no refresh is sent: Core reads 45, the Edge 92, and nothing Core sends
// afterwards brings the person's number to the Edge.
//
// Expected to change when the adjustment joins the must-arrive list: the
// count then survives the outage, arrives late and fenced, and is rebased
// (both books 45). Fencing resets alone does not change it: the count is
// already fenced at the one cursor.
func TestScenario_CountDecl_APersonsCountLostEachSideKeepsItsOwn(t *testing.T) {
	t.Parallel()
	w := newDeclRig(t, "CD-A", 100)
	w.strokes(3)
	w.shipEdgeDeltas()
	_, _, _, epoch := w.books()

	b, err := w.core.GetBin(w.binID)
	if err != nil {
		t.Fatalf("read bin: %v", err)
	}
	if _, err := w.bins.RecordCount(b, 50, "cycle-counter"); err != nil {
		t.Fatalf("a person's count: %v", err)
	}
	decl := w.declaration()
	if f := decl.adj; f.AsOfNet == nil || *f.AsOfNet != -3 || f.AsOfSeq == nil || *f.AsOfSeq != 1 ||
		f.AsOfStation != handoffStation || f.Epoch != epoch || f.NewRemaining != 50 {
		t.Errorf("the count's declaration is %s, want 50 at epoch %d fenced at net -3 seq 1 for %s", declText(decl.adj), epoch, handoffStation)
	}
	if !w.outage(decl) {
		t.Errorf("Core's drainer kept the count's declaration through the outage (offered %d times): "+
			"row (a) has changed, as the must-arrive list predicts", w.broker.offered(decl.payload))
	}

	w.strokes(5)
	w.shipEdgeDeltas()
	w.edgeReads(w.pass())

	if got := w.readByEdge(); len(got) != 0 {
		t.Errorf("the Edge was given %v for the bin, want nothing: the count was lost and no generation ended", got)
	}
	if n := len(w.coreRows(protocol.SubjectBinEpochRefresh)); n != 0 {
		t.Errorf("Core queued %d epoch refreshes, want 0: a count ends no generation", n)
	}
	if n := w.staleDrops(); n != 0 {
		t.Errorf("stale_epoch_dropped = %d, want 0", n)
	}
	edge, edgeEpoch, core, coreEpoch := w.books()
	if core != 45 || coreEpoch != epoch || edge != 92 || edgeEpoch != epoch {
		t.Errorf("books: Edge %d at %d, Core %d at %d; want Edge 92 and Core 45, both at %d: "+
			"each side keeps its own number", edge, edgeEpoch, core, coreEpoch, epoch)
	}
}

// TestScenario_CountDecl_AResetsDeclarationLostTheRefreshMovesOnlyTheStamp is
// row (b), for each declarer. The station has counted 2 (98 at E) when Core
// resets the carrier to 60 at E+1; the declaration (unfenced) is
// dead-lettered by an outage. The station counts 4 under E: Core drops them as
// stale and answers with the epoch refresh, which carries no count. The Edge
// adopts E+1 and keeps 94; its next 3 land at Core. Core reads 57, the Edge
// 91: the reset's 60 never reaches the Edge, and the 4 parts counted under
// the ended generation are in the Edge's number and discarded at Core.
//
// Expected to change when the adjustment joins the must-arrive list, with or
// without the reset fenced at net 0, seq 0 of E+1: the declaration survives
// the outage, reaches the Edge ahead of the refresh (the outbox goes in id
// order) while the Edge still holds E, and is written as is at E+1, so both
// books read 57.
// Fencing resets alone changes only the declaration's shape here.
func TestScenario_CountDecl_AResetsDeclarationLostTheRefreshMovesOnlyTheStamp(t *testing.T) {
	t.Parallel()
	for _, tc := range declResets {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newDeclRig(t, "CD-B-"+tc.name, 100)
			w.strokes(2)
			w.shipEdgeDeltas()
			_, _, _, e := w.books()

			if err := tc.reset(w, 60); err != nil {
				t.Fatalf("the reset: %v", err)
			}
			decl := w.declaration()
			if f := decl.adj; f.AsOfNet != nil || f.Epoch != e+1 || f.NewRemaining != 60 || f.Actor != tc.actor {
				t.Errorf("the reset's declaration is %s, want 60 at epoch %d by %q, unfenced", declText(decl.adj), e+1, tc.actor)
			}
			if !w.outage(decl) {
				t.Errorf("Core's drainer kept the reset's declaration through the outage (offered %d times): "+
					"row (b) has changed, as the must-arrive list predicts", w.broker.offered(decl.payload))
			}

			w.strokes(4)
			w.shipEdgeDeltas()
			if n := w.staleDrops(); n == 0 {
				t.Fatal("no stale drop: the 4 parts under the ended generation were applied at Core")
			}
			w.edgeReads(w.pass())
			if got := w.readByEdge(); len(got) != 1 || got[0] != protocol.SubjectBinEpochRefresh {
				t.Errorf("the Edge was given %v for the bin, want only the epoch refresh", got)
			}
			if edge, edgeEpoch, _, _ := w.books(); edge != 94 || edgeEpoch != e+1 {
				t.Errorf("after the refresh the Edge reads %d at %d, want 94 at %d: the refresh moves the stamp only",
					edge, edgeEpoch, e+1)
			}

			w.strokes(3)
			w.shipEdgeDeltas()
			edge, edgeEpoch, core, coreEpoch := w.books()
			if core != 57 || coreEpoch != e+1 || edge != 91 || edgeEpoch != e+1 {
				t.Errorf("books: Edge %d at %d, Core %d at %d; want Edge 91 and Core 57, both at %d: "+
					"the reset's number never reached the Edge", edge, edgeEpoch, core, coreEpoch, e+1)
			}
		})
	}
}

// TestScenario_CountDecl_AFencedCountLateIsRebasedToCores is row (c). The
// station has counted 3 (97) when a person counts 50 at Core, fenced at net
// -3, seq 1. The broker takes it; the Edge reads it late. Before it does, the
// station counts 4 (applied at Core: 46), flushes 2 more still on the wire,
// and records 1 unflushed. The late copy is rebased:
// 50 + (-9 - -3) + (-1) = 43, which is Core's number once the 2 and the 1
// land. Both books read 43.
func TestScenario_CountDecl_AFencedCountLateIsRebasedToCores(t *testing.T) {
	t.Parallel()
	w := newDeclRig(t, "CD-C", 100)
	w.strokes(3)
	w.shipEdgeDeltas()

	b, err := w.core.GetBin(w.binID)
	if err != nil {
		t.Fatalf("read bin: %v", err)
	}
	if _, err := w.bins.RecordCount(b, 50, "cycle-counter"); err != nil {
		t.Fatalf("a person's count: %v", err)
	}
	if f := w.declaration().adj; f.AsOfNet == nil || *f.AsOfNet != -3 || f.AsOfSeq == nil || *f.AsOfSeq != 1 ||
		f.AsOfStation != handoffStation {
		t.Fatalf("the count's declaration is %s, want it fenced at net -3 seq 1 for %s", declText(f), handoffStation)
	}
	late := w.pass()
	if len(late) == 0 {
		t.Fatal("the broker took nothing: the count's declaration was not published")
	}

	w.strokes(4)
	w.shipEdgeDeltas()
	w.strokes(2)
	onTheWire := w.takeEdgeDeltas()
	w.edge.Engine.Events.Emit(edgeengine.Event{Type: edgeengine.EventCounterDelta,
		Payload: edgeengine.CounterDeltaEvent{ProcessID: w.process, StyleID: w.style, Delta: 1}})

	w.edgeReads(late)
	if got := w.readByEdge(); len(got) != 1 || got[0] != protocol.SubjectUOPAdjustment {
		t.Fatalf("the Edge was given %v for the bin, want the count's declaration", got)
	}
	if edge, _, _, _ := w.books(); edge != 43 {
		t.Errorf("the Edge after the late fenced count reads %d, want 43: 50 rebased by the 6 flushed since and the 1 unflushed", edge)
	}
	for _, d := range onTheWire {
		if err := w.inv.ApplyBinUOPDelta(handoffStation, d); err != nil {
			t.Fatalf("the window on the wire: %v", err)
		}
	}
	w.acc.Flush()
	w.shipEdgeDeltas()
	edge, _, core, _ := w.books()
	if core != 43 || edge != 43 {
		t.Errorf("books: Edge %d, Core %d; want both 43", edge, core)
	}
}

// TestScenario_CountDecl_AnUnfencedResetLate is row (d), for each declarer.
// The station has counted 2 (98 at E) when Core resets the carrier to 60 at
// E+1, unfenced. The broker refuses the declaration's publish while it takes
// the rest, so the epoch refresh Core sends for the station's 4 stale parts
// overtakes it: the Edge adopts E+1 at 94 and counts 3 there (Core 57). Then
// the declaration lands, late. A person's (the bins page's Load Payload) is
// written as is: the Edge reads 60, the 3 parts counted since overwritten. A
// press's (lifecycle) names the generation the slot already holds and is
// ignored as already applied: the Edge keeps 91, and Core's 60 never reaches
// it.
//
// Expected to change when every count Core states is fenced: the late copy
// carries net 0, seq 0 of E+1 and is rebased to 57 for both declarers.
func TestScenario_CountDecl_AnUnfencedResetLate(t *testing.T) {
	t.Parallel()
	want := map[string]int{protocol.AuditActorUI: 60, protocol.ActorCoreLifecycle: 91}
	for _, tc := range declResets {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newDeclRig(t, "CD-D-"+tc.name, 100)
			w.strokes(2)
			w.shipEdgeDeltas()
			_, _, _, e := w.books()

			if err := tc.reset(w, 60); err != nil {
				t.Fatalf("the reset: %v", err)
			}
			decl := w.declaration()
			if f := decl.adj; f.AsOfNet != nil || f.Epoch != e+1 || f.NewRemaining != 60 || f.Actor != tc.actor {
				t.Errorf("the reset's declaration is %s, want 60 at epoch %d by %q, unfenced", declText(decl.adj), e+1, tc.actor)
			}
			w.strokes(4)
			w.shipEdgeDeltas()

			w.broker.set(func(b *declBroker) { b.refused[string(decl.payload)] = true })
			w.edgeReads(w.pass())
			if got := w.readByEdge(); len(got) != 1 || got[0] != protocol.SubjectBinEpochRefresh {
				t.Errorf("the Edge was given %v for the bin, want only the epoch refresh", got)
			}
			if r := w.coreRow(decl.id); r.sent || r.retries >= outbox.MaxRetries {
				t.Fatalf("the declaration is sent=%t retries=%d, want pending inside its retries: "+
					"the rig did not hold it back", r.sent, r.retries)
			}
			w.strokes(3)
			w.shipEdgeDeltas()
			if edge, edgeEpoch, core, _ := w.books(); edge != 91 || edgeEpoch != e+1 || core != 57 {
				t.Errorf("before the late declaration: Edge %d at %d, Core %d; want Edge 91 at %d, Core 57",
					edge, edgeEpoch, core, e+1)
			}

			w.broker.set(func(b *declBroker) { delete(b.refused, string(decl.payload)) })
			w.edgeReads(w.pass())
			if got := w.readByEdge(); len(got) != 1 || got[0] != protocol.SubjectUOPAdjustment {
				t.Errorf("the Edge was given %v for the bin, want the late declaration", got)
			}
			edge, edgeEpoch, core, _ := w.books()
			if edge != want[tc.actor] || edgeEpoch != e+1 || core != 57 {
				t.Errorf("after the late declaration by %q: Edge %d at %d, Core %d; want Edge %d at %d, Core 57",
					tc.actor, edge, edgeEpoch, core, want[tc.actor], e+1)
			}
		})
	}
}
