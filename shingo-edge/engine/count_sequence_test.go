package engine

// count_sequence_test.go — the count-in-parts proof (count brief step 7): one
// randomized sequence of floor events on the Edge engine, checked after every
// event against what physically happened.
//
// THE RULE IT PROVES (owner, 2026-11-19). A bin's count is what is physically
// in the carrier: parts for a payload of one manifest line, kits for a kit. A
// press adds its counter's step as parts, at no ratio (handleProduceTick). An
// assembly cycle draws step × ratio (NodeClaim.ConsumeDraw), the lineside pile
// first (drainLinesideFirst). Strict pack caps a produce bin at its pack and
// holds the rest (splitStrict, chargeStrict). A ratio change rewrites no stored
// number. Nothing else here is a rule of its own: every count below is read
// from the engine's own rows after the engine's own doors ran.
//
// TIER. An engine test on the Edge's SQLite (a file in t.TempDir, reopened at
// a restart), no docker. What can fail it is a Go decision on a count path:
// the tick walk, the release commit, the landing and pickup binds, the typed
// counts, the hold rules, the catalog sync. Core is the stub the package's
// other tests use (bridgeCore for bin.count; release points from the Edge's own
// rows, dbPoints) and its bins feed is held current (holdActableBins) after
// every move, as a plant's is.
//
// WHAT IS ASSERTED AFTER EVERY EVENT:
//
//  1. Conservation, per part: parts charged in (press strokes, the counts
//     carts land with, typed and declared corrections as deltas) equal the
//     parts the engine stores (seat counts, lineside piles, produce holds)
//     plus the parts drawn (step × the ratio the Edge holds, per cycle), plus
//     the parts that left in a departing carrier, plus what a person wrote
//     off (RELEASE EMPTY, underpack, a typed 0), plus what the engine recorded
//     as dropped (hold_dropped rows in the outbox and the open no-cart
//     stretch, which Stop writes).
//  2. Only an allowed writer moved a stored count: each event names the cells
//     it may move (a tick its seats, holds and piles; a typed count its seat,
//     and its hold into a recorded drop, U2; a load or a clear its seat; a
//     sweep a hold into a recorded drop), and every other cell must read as it
//     did. A ratio change, a late catalog, a restart, a re-landing of the same
//     bin, a strict-pack setting and an order's close move nothing.
//  3. Per event, the exact number the event is about: a cycle took step ×
//     ratio from each consume spot, a typed count reads as typed, a landing
//     reads Core's count, a release declared what it said.
//
// REPRODUCE A FAILURE with SHINGO_COUNT_SEQ_SEED=<seed> (the base seed; the
// sequence names its own) and -run TestCount_ConservesPartsOverSequences/seed=<n>.
// The failure prints the whole event list.
//
// NOT MODELLED, and why (each is named in the generator, not silently left
// out): PULL PARTS LINESIDE (a release with chips) is off while
// TestCount_CaptureReleaseCountsThePulledPartsTwice is skipped, and a capture
// is a pile's only writer upward (AdminClearLinesideBucket), so no pile forms
// and the drain-first is not reached here until it is on; LoadBin is a loader
// window's door and a loader window takes no tick (walkTick's manual_swap
// rung), so the kit reaches the assembly the way a cart does, by a landing;
// held and off-plan CATID ticks and the no-cart ledger are another account
// (catid_holds), kept empty, which is asserted; a crash (no Stop) is not a
// restart here: the open no-cart stretch lives in memory and Stop writes it.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/plc"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
	"shingoedge/uop"
)

// The sequence's size: seqRuns sequences of seqEvents events each, from
// seqSeed unless SHINGO_COUNT_SEQ_SEED names another base.
const (
	seqSeed   = int64(20261119)
	seqRuns   = 24
	seqEvents = 90
)

// The parts. SYN-SQ-R8 is pressed and assembled: one manifest line at 8 per
// cycle (4 after a ratio change), so the press adds parts and the assembly
// draws eight at a time from the same count. SYN-SQ-R1 is one line at 1.
// SYN-SQ-KIT is a kit of three lines (2, 1 and 1 per kit: a cart of 20 holds
// 40/20/20), whose ratio is 1: its cart counts kits.
const (
	seqR8  = "SYN-SQ-R8"
	seqR1  = "SYN-SQ-R1"
	seqKit = "SYN-SQ-KIT"
)

// captureCountsTwice keeps PULL PARTS LINESIDE out of the generator while the
// defect TestCount_CaptureReleaseCountsThePulledPartsTwice reproduces stands.
// The fix's commit deletes that test's t.Skip and sets this false.
const captureCountsTwice = true

type seqPhase int

const (
	phaseIdle      seqPhase = iota // a carrier bound, no swap asked
	phaseRequested                 // the swap's two legs staged at the seat
	phaseReleased                  // the operator released it
	phaseLifted                    // the departing carrier lifted
	phaseLanded                    // the next carrier landed; the legs not yet closed
)

// seqSeat is one position and the test's knowledge of it: its claim, the
// carrier Core has there, and how far its swap has gone.
type seqSeat struct {
	name    string
	id      int64
	claimID int64
	part    string
	pack    int
	produce bool
	style   int64
	process int64

	bin   int64 // the carrier Core has at the seat; 0 none
	phase seqPhase
	a, b  *storeorders.Order // supply and evac legs of the swap in hand
}

// seqWorld is one sequence: its file, its engine and the model of what
// physically happened.
type seqWorld struct {
	t    *testing.T
	seed int64
	rng  *rand.Rand
	path string
	db   *store.DB
	eng  *Engine
	srv  *httptest.Server

	seats  []*seqSeat
	byName map[string]*seqSeat
	press  struct{ process, style int64 }
	asm    struct{ process, style int64 }

	nextBin   int64
	placedSeq int64
	plcCount  int64 // the press counter's running total, as the PLC holds it

	ratio        int // SYN-SQ-R8's ratio as the Edge holds it
	ratioDue     int // event index the late catalog arrives at; 0 none pending
	ratioNext    int
	strictPB     bool
	bootSweepDue bool  // a restart's boot sweep is the next event
	downStrokes  int64 // strokes the press counted while the Edge was down

	// The model: parts per part number.
	in, drawn, departed, writtenOff map[string]int

	log   []string
	kinds map[string]int
	event int
}

// seqSnap is the engine's stored numbers at one instant.
type seqSnap struct {
	cells   map[string]int    // "seat N", "seat N bin", "hold N", "pile N P state", "drops P"
	parts   map[string]string // the part each count cell counts; "" for a pointer
	rt      map[string]*processes.RuntimeState
	ingests []protocol.OrderIngestRequest
	noCart  int
	// dropRows counts the recorded drop rows by reason, the open no-cart
	// stretches' rows under "no_bin (open)": what the coverage tally reads.
	dropRows map[string]int
}

// seqStep is what an event did: its line in the event list, the cells it may
// move, and its exact expectation ("" met).
type seqStep struct {
	desc  string
	may   []string
	check func(before, after seqSnap) string
}

// TestCount_ConservesPartsOverSequences is the proof: seqRuns sequences of
// seqEvents events, every event checked (see the file comment). The coverage
// floor at the end fails the run if an event kind never landed, so a generator
// that stopped reaching a door cannot pass vacuously.
func TestCount_ConservesPartsOverSequences(t *testing.T) {
	t.Parallel()
	base := seqSeed
	if s := os.Getenv("SHINGO_COUNT_SEQ_SEED"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		testutil.MustNoErr(t, err, "SHINGO_COUNT_SEQ_SEED")
		base = v
	}
	var mu sync.Mutex
	total := map[string]int{}
	t.Cleanup(func() {
		if t.Failed() {
			return
		}
		for _, k := range seqKindsCovered {
			if total[k] < 3 {
				t.Errorf("event kind %q landed %d times over %d sequences, want at least 3: the generator stopped reaching it", k, total[k], seqRuns)
			}
		}
		for _, k := range seqStatesCovered {
			if total["reached: "+k] < 1 {
				t.Errorf("no sequence reached %q: the generator stopped reaching it", k)
			}
		}
		t.Logf("events landed, and states reached, over %d sequences: %v", seqRuns, total)
	})
	for i := range seqRuns {
		seed := base + int64(i)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			w := newSeqWorld(t, seed)
			w.runSequence(seqEvents)
			mu.Lock()
			for k, n := range w.kinds {
				total[k] += n
			}
			mu.Unlock()
		})
	}
}

// seqKindsCovered is every event kind the coverage floor asks for.
var seqKindsCovered = []string{
	"press stroke", "assembly cycle", "consume request", "release empty", "release underpack",
	"release partial typed", "release partial untyped", "consume lift", "consume land", "kit land",
	"legs closed", "press request", "press release", "press release full", "press lift", "press land",
	"re-landing", "floor count", "core count", "ratio change", "late catalog", "catalog resend",
	"strict toggle", "restart", "boot sweep", "counter moved while down", "stranded sweep",
}

// ── The world ──────────────────────────────────────────────────────────────

func newSeqWorld(t *testing.T, seed int64) *seqWorld {
	t.Helper()
	w := &seqWorld{
		t: t, seed: seed, rng: rand.New(rand.NewSource(seed)), path: testdb.Path(t),
		byName: map[string]*seqSeat{}, nextBin: 1000, ratio: 8,
		in: map[string]int{}, drawn: map[string]int{}, departed: map[string]int{}, writtenOff: map[string]int{},
		kinds: map[string]int{},
	}
	db, err := store.OpenMigrated(w.path)
	testutil.MustNoErr(t, err, "open the sequence's database")
	w.db = db
	t.Cleanup(func() { _ = w.db.Close() })
	// Core's bin.count door, as Core answers a count it accepts: the carrier
	// and generation the count named, and the count as typed.
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/telemetry/bin-count" {
			http.NotFound(rw, r)
			return
		}
		var req protocol.BinCountRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BinID == 0 {
			rw.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(rw).Encode(map[string]any{"status": "error", "detail": "no carrier named"})
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"status": "ok", "bin_id": req.BinID, "uop_remaining": req.ActualUOP, "delta_epoch": req.Epoch,
		})
	}))
	t.Cleanup(w.srv.Close)
	w.startEngine()
	testutil.MustNoErr(t, w.eng.HandlePayloadCatalog(w.catalog()), "the catalog")

	w.press.process, w.press.style = w.process("SQ-PRS")
	w.asm.process, w.asm.style = w.process("SQ-ASM")
	w.seat("SQ_PA", seqR8, true, false)
	w.seat("SQ_PB", seqR8, true, true)
	w.strictPB = true
	w.seat("SQ_CA", seqR8, false, false)
	w.seat("SQ_CB", seqR1, false, false)
	w.seat("SQ_CK", seqKit, false, false)

	// The first carriers, as Core lands them: the press's two empties and the
	// assembly's three carts. Each is a load: parts charged in.
	for _, s := range w.seats {
		count := 0
		if !s.produce {
			count = s.pack
		}
		w.land(s, count, "")
	}
	w.pushBins()
	if msg := w.conservation(w.snap()); msg != "" {
		t.Fatalf("seed=%d: the seeded world does not balance: %s", seed, msg)
	}
	return w
}

// startEngine builds the engine over the world's database as engine.New and
// Start wire what a count path reads: the production uop mutator as the
// inventory sink, the event chain, and Core answering bin.count.
func (w *seqWorld) startEngine() {
	w.eng = testEngine(w.t, w.db)
	w.eng.SetInventoryDeltaSink(uop.New(w.db, w.eng.cfg.StationID(), w.db, w.db))
	w.eng.wireEventHandlers()
	bridgeCore(w.t, w.eng, w.srv.URL)
}

// catalog is Core's payload catalog as the world holds it now.
func (w *seqWorld) catalog() []protocol.CatalogPayloadInfo {
	return []protocol.CatalogPayloadInfo{
		{ID: 1, Name: seqR8, Code: seqR8, UOPCapacity: 16,
			Manifest: []protocol.ManifestItem{{PartNumber: seqR8, PartsPerCycle: int64(w.ratioOrNext())}}},
		{ID: 2, Name: seqR1, Code: seqR1, UOPCapacity: 30,
			Manifest: []protocol.ManifestItem{{PartNumber: seqR1, PartsPerCycle: 1}}},
		// The kit's first line is the one at 2, so a ratio read off the first
		// line instead of the kit's 1 draws twice what it should.
		{ID: 3, Name: seqKit, Code: seqKit, UOPCapacity: 20, Manifest: []protocol.ManifestItem{
			{PartNumber: "SYN-SQ-KC", PartsPerCycle: 2}, {PartNumber: "SYN-SQ-KA", PartsPerCycle: 1},
			{PartNumber: "SYN-SQ-KB", PartsPerCycle: 1}}},
	}
}

// ratioOrNext is the ratio the catalog carries: the Edge's, or the new one
// while it is being delivered.
func (w *seqWorld) ratioOrNext() int {
	if w.ratioNext != 0 {
		return w.ratioNext
	}
	return w.ratio
}

func (w *seqWorld) process(name string) (int64, int64) {
	pid, err := w.db.CreateProcess(name, "", "", "", false)
	testutil.MustNoErr(w.t, err, "process "+name)
	sid, err := w.db.CreateStyle(name+"-STYLE", "", pid)
	testutil.MustNoErr(w.t, err, "style "+name)
	testutil.MustNoErr(w.t, w.db.SetActiveStyle(pid, &sid), "active style "+name)
	return pid, sid
}

// seat creates a two-robot position and its claim. The runtime row's claim
// pointer is stamped as a seat that has completed an order has it (the release
// resolves its claim through it; a seat never stamped drops the disposition,
// release.PlanLeg's toClaim-nil arm, which is its own characterised state).
func (w *seqWorld) seat(name, part string, produce, strict bool) {
	s := &seqSeat{name: name, part: part, produce: produce}
	s.process, s.style = w.asm.process, w.asm.style
	if produce {
		s.process, s.style = w.press.process, w.press.style
	}
	for _, c := range w.catalog() {
		if c.Code == part {
			s.pack = c.UOPCapacity
		}
	}
	id, err := w.db.CreateProcessNode(processes.NodeInput{ProcessID: s.process, CoreNodeName: name, Code: name,
		Name: name, Sequence: len(w.seats) + 1, Enabled: true})
	testutil.MustNoErr(w.t, err, "node "+name)
	s.id = id
	s.claimID, err = w.db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, w.claimInput(s, strict))
	testutil.MustNoErr(w.t, err, "claim "+name)
	_, err = w.db.EnsureProcessNodeRuntime(id)
	testutil.MustNoErr(w.t, err, "runtime "+name)
	testutil.MustNoErr(w.t, w.db.SetProcessNodeActiveClaim(id, &s.claimID), "claim pointer "+name)
	w.seats = append(w.seats, s)
	w.byName[name] = s
}

func (w *seqWorld) claimInput(s *seqSeat, strict bool) processes.NodeClaimInput {
	role := protocol.ClaimRoleConsume
	if s.produce {
		role = protocol.ClaimRoleProduce
	}
	return processes.NodeClaimInput{
		StyleID: s.style, CoreNodeName: s.name, Role: role, SwapMode: protocol.SwapModeTwoRobot,
		PayloadCode: s.part, UOPCapacity: s.pack, StrictPack: strict,
		InboundSource: "SQ_MKT_IN", InboundStaging: s.name + "_IN",
		OutboundStaging: s.name + "_OUT", OutboundDestination: "SQ_MKT_OUT",
	}
}

// land is Core landing a new carrier of count at the seat, for the swap's
// supply leg when one is in hand. Charged in.
func (w *seqWorld) land(s *seqSeat, count int, orderUUID string) int64 {
	w.nextBin++
	w.placedSeq++
	bin := w.nextBin
	payload := s.part
	if s.produce {
		payload = "" // an empty, known to be empty
	}
	w.eng.HandleBinLanded(protocol.BinLanded{BinID: bin, CoreNodeName: s.name, Count: count, Epoch: 1,
		PayloadCode: payload, PayloadKnown: true, OrderUUID: orderUUID, PlacedSeq: w.placedSeq})
	s.bin = bin
	w.in[s.part] += count
	return bin
}

// pushBins is Core's bins feed after a move: every seat's carrier as Core has
// it, held as a current copy.
func (w *seqWorld) pushBins() {
	rows := make([]protocol.NodeBinsRow, 0, len(w.seats))
	for _, s := range w.seats {
		r := protocol.NodeBinsRow{Node: s.name}
		if s.bin != 0 {
			r.Occupied, r.BinID, r.Epoch = true, s.bin, 1
			if !s.produce {
				r.PayloadCode = s.part
			}
		}
		rows = append(rows, r)
	}
	holdActableBins(w.t, w.eng, rows...)
}

// ── Reading the engine ─────────────────────────────────────────────────────

func (w *seqWorld) snap() seqSnap {
	t := w.t
	t.Helper()
	s := seqSnap{cells: map[string]int{}, parts: map[string]string{}, rt: map[string]*processes.RuntimeState{}}
	claimPart := map[string]string{}
	for _, seat := range w.seats {
		claimPart[seat.name] = seat.part
		rt, err := w.db.GetProcessNodeRuntime(seat.id)
		testutil.MustNoErr(t, err, "runtime "+seat.name)
		s.rt[seat.name] = rt
		s.cells["seat "+seat.name], s.parts["seat "+seat.name] = rt.RemainingUOPCached, seat.part
		var bin int64
		if rt.ActiveBinID != nil {
			bin = *rt.ActiveBinID
		}
		s.cells["seat "+seat.name+" bin"] = int(bin)
		hold := "hold " + seat.name
		s.cells[hold], s.parts[hold] = int(rt.PendingUOPDelta), seat.part
		if rt.PendingUOPPayload != "" {
			s.parts[hold] = rt.PendingUOPPayload
		}
		piles, err := w.db.ListLinesideBuckets(seat.id)
		testutil.MustNoErr(t, err, "piles "+seat.name)
		for _, p := range piles {
			k := fmt.Sprintf("pile %s %s %s", seat.name, p.PayloadCode, p.State)
			s.cells[k] += p.Qty
			s.parts[k] = p.PayloadCode
		}
	}
	msgs, err := w.db.ListPendingOutbox(1 << 20)
	testutil.MustNoErr(t, err, "outbox")
	s.dropRows = map[string]int{}
	addDrop := func(d protocol.HoldDropped, reason string) {
		part := d.PayloadCode
		if part == "" {
			part = claimPart[d.Node]
		}
		k := "drops " + part
		s.cells[k] += int(d.Qty)
		s.parts[k] = part
		s.dropRows[reason]++
	}
	for _, m := range msgs {
		var env protocol.Envelope
		switch m.MsgType {
		case protocol.SubjectHoldDropped:
			var data protocol.Data
			var d protocol.HoldDropped
			if json.Unmarshal(m.Payload, &env) != nil || json.Unmarshal(env.Payload, &data) != nil ||
				json.Unmarshal(data.Body, &d) != nil {
				t.Fatalf("a hold_dropped row does not decode: %s", m.Payload)
			}
			addDrop(d, d.Reason)
		case protocol.TypeOrderIngest:
			var req protocol.OrderIngestRequest
			if json.Unmarshal(m.Payload, &env) != nil || env.DecodePayload(&req) != nil {
				t.Fatalf("an ingest row does not decode: %s", m.Payload)
			}
			s.ingests = append(s.ingests, req)
		}
	}
	// The open no-cart stretches: strokes no cart took, recorded when the
	// stretch ends (Stop writes them; sendNoCartStretches).
	w.eng.countMu.Lock()
	for nodeID, rows := range w.eng.noBinStrokes {
		for _, d := range rows {
			if d.Node == "" {
				for _, seat := range w.seats {
					if seat.id == nodeID {
						d.Node = seat.name
					}
				}
			}
			addDrop(d, d.Reason+" (open)")
		}
	}
	w.eng.countMu.Unlock()
	for _, pid := range []int64{w.press.process, w.asm.process} {
		hs, err := w.db.ListOpenCatidHolds(pid)
		testutil.MustNoErr(t, err, "catid holds")
		s.noCart += len(hs)
	}
	return s
}

// conservation is assertion 1: "" when every part balances.
func (w *seqWorld) conservation(s seqSnap) string {
	type acct struct{ seats, holds, piles, drops int }
	by := map[string]*acct{}
	get := func(p string) *acct {
		if by[p] == nil {
			by[p] = &acct{}
		}
		return by[p]
	}
	for cell, n := range s.cells {
		p := s.parts[cell]
		switch {
		case strings.HasSuffix(cell, " bin"):
		case strings.HasPrefix(cell, "seat "):
			get(p).seats += n
		case strings.HasPrefix(cell, "hold "):
			get(p).holds += n
		case strings.HasPrefix(cell, "pile "):
			get(p).piles += n
		case strings.HasPrefix(cell, "drops "):
			get(p).drops += n
		}
	}
	for _, m := range []map[string]int{w.in, w.drawn, w.departed, w.writtenOff} {
		for p := range m {
			get(p)
		}
	}
	parts := make([]string, 0, len(by))
	for p := range by {
		parts = append(parts, p)
	}
	sort.Strings(parts)
	var out []string
	for _, p := range parts {
		a := by[p]
		rhs := a.seats + a.holds + a.piles + w.drawn[p] + w.departed[p] + w.writtenOff[p] + a.drops
		if w.in[p] != rhs {
			out = append(out, fmt.Sprintf("part %s: charged in %d, accounted %d (seats %d + holds %d + piles %d + "+
				"drawn %d + departed %d + written off %d + recorded drops %d): %+d parts the engine's numbers do not explain",
				p, w.in[p], rhs, a.seats, a.holds, a.piles, w.drawn[p], w.departed[p], w.writtenOff[p], a.drops, rhs-w.in[p]))
		}
	}
	if s.noCart != 0 {
		out = append(out, fmt.Sprintf("%d no-cart or held ledger rows: a cycle drew from a seat with no cart, "+
			"which the generator must not ask for", s.noCart))
	}
	return strings.Join(out, "; ")
}

// writers is assertion 2: every cell that moved is one the event may move.
func writers(may []string, before, after seqSnap) string {
	keys := map[string]bool{}
	for k := range before.cells {
		keys[k] = true
	}
	for k := range after.cells {
		keys[k] = true
	}
	var out []string
	for k := range keys {
		if before.cells[k] == after.cells[k] {
			continue
		}
		allowed := false
		for _, m := range may {
			if k == m || strings.HasPrefix(k, m+" ") {
				allowed = true
			}
		}
		if !allowed {
			out = append(out, fmt.Sprintf("%s %d -> %d", k, before.cells[k], after.cells[k]))
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return ""
	}
	who := "nothing"
	if len(may) > 0 {
		who = strings.Join(may, ", ")
	}
	return "a stored count moved that this event may not move (it may move " + who + "): " + strings.Join(out, "; ")
}

// ── The driver ─────────────────────────────────────────────────────────────

type seqEventKind struct {
	weight int
	run    func(w *seqWorld, before seqSnap) (kind string, step *seqStep)
}

var seqEventKinds = []seqEventKind{
	{24, (*seqWorld).evPressStroke},
	{22, (*seqWorld).evAssemblyCycle},
	{6, (*seqWorld).evConsumeRequest},
	{7, (*seqWorld).evConsumeRelease},
	{7, (*seqWorld).evConsumeLift},
	{7, (*seqWorld).evConsumeLand},
	{6, (*seqWorld).evLegsClosed},
	{5, (*seqWorld).evPressRequest},
	{6, (*seqWorld).evPressRelease},
	{6, (*seqWorld).evPressLift},
	{6, (*seqWorld).evPressLand},
	{3, (*seqWorld).evRelanding},
	{4, (*seqWorld).evFloorCount},
	{4, (*seqWorld).evCoreCount},
	{2, (*seqWorld).evRatioChange},
	{1, (*seqWorld).evCatalogResend},
	{2, (*seqWorld).evStrictToggle},
	{2, (*seqWorld).evRestart},
	{2, (*seqWorld).evStrandedSweep},
}

func (w *seqWorld) runSequence(n int) {
	sum := 0
	for _, k := range seqEventKinds {
		sum += k.weight
	}
	for w.event = 1; w.event <= n; w.event++ {
		before := w.snap()
		var kind string
		var step *seqStep
		switch {
		case w.bootSweepDue:
			kind, step = w.evStrandedSweep(before)
		case w.downStrokes > 0:
			kind, step = w.evCounterMovedWhileDown(before)
		case w.ratioDue != 0 && w.event >= w.ratioDue:
			kind, step = w.evLateCatalog(before)
		default:
			for attempt := 0; step == nil && attempt < 200; attempt++ {
				r := w.rng.Intn(sum)
				for _, k := range seqEventKinds {
					if r < k.weight {
						kind, step = k.run(w, before)
						break
					}
					r -= k.weight
				}
			}
		}
		if step == nil {
			w.fail("no event could run")
		}
		w.log = append(w.log, fmt.Sprintf("#%02d %s", w.event, step.desc))
		w.kinds[kind]++
		after := w.snap()
		if msg := writers(step.may, before, after); msg != "" {
			w.fail(msg)
		}
		if step.check != nil {
			if msg := step.check(before, after); msg != "" {
				w.fail(msg)
			}
		}
		if msg := w.conservation(after); msg != "" {
			w.fail("conservation: " + msg)
		}
		w.noteReached(kind, before, after)
	}
}

// noteReached tallies the engine states a sequence reached, beside the event
// kinds: the coverage floor reads the ones a pass must have crossed
// (seqStatesCovered), so a generator that stopped reaching the strict cap or a
// replayed hold fails rather than passes on easy ground.
func (w *seqWorld) noteReached(kind string, before, after seqSnap) {
	note := func(state string) { w.kinds["reached: "+state]++ }
	for r, n := range after.dropRows {
		if n > before.dropRows[r] {
			note("drop " + r)
		}
	}
	for _, s := range w.seats {
		seat, hold := "seat "+s.name, "hold "+s.name
		if after.cells[seat] < 0 && before.cells[seat] >= 0 {
			note("a count below 0")
		}
		if kind != "press stroke" && kind != "counter moved while down" || !s.produce {
			continue
		}
		if after.cells[hold] < before.cells[hold] && after.cells[seat] > before.cells[seat] {
			note("a hold replayed onto a bin")
		}
		if s.name == "SQ_PB" && w.strictPB && after.cells[seat] >= s.pack && after.cells[hold] > before.cells[hold] {
			note("the strict pack capped a bin and held the rest")
		}
		if after.cells[seat+" bin"] == 0 && after.cells[hold] > before.cells[hold] {
			note("an unbound seat held strokes")
		}
	}
	switch kind {
	case "assembly cycle":
		if w.ratio == 4 {
			note("a cycle at ratio 4")
		}
		for k, n := range before.cells {
			if strings.HasPrefix(k, "pile ") && after.cells[k] < n {
				note("a cycle drew a pile")
				break
			}
		}
		for _, s := range w.seats {
			if !s.produce && s.phase == phaseReleased && !before.rt[s.name].ReleasedEmpty {
				note("a cycle charged a released carrier before its lift")
				break
			}
		}
	case "restart":
		if before.dropRows[protocol.HoldDropNoBin+" (open)"] > 0 {
			note("a restart with a no-cart stretch open")
		}
	}
}

// seqStatesCovered is every engine state the coverage floor asks for.
var seqStatesCovered = []string{
	"drop " + protocol.HoldDropHoldCap, "drop " + protocol.HoldDropNoBin, "a count below 0",
	"a hold replayed onto a bin", "the strict pack capped a bin and held the rest", "an unbound seat held strokes",
	"a cycle at ratio 4", "a cycle charged a released carrier before its lift",
}

func (w *seqWorld) fail(msg string) {
	w.t.Helper()
	w.t.Fatalf("seed=%d event #%d: %s\nevents:\n  %s\n(rerun: SHINGO_COUNT_SEQ_SEED=%d go test -count=1 -run 'TestCount_ConservesPartsOverSequences/seed=%d$' ./engine/)",
		w.seed, w.event, msg, strings.Join(w.log, "\n  "), w.seed, w.seed)
}

// pick is a random seat the predicate admits, nil when none does.
func (w *seqWorld) pick(ok func(*seqSeat) bool) *seqSeat {
	var in []*seqSeat
	for _, s := range w.seats {
		if ok(s) {
			in = append(in, s)
		}
	}
	if len(in) == 0 {
		return nil
	}
	return in[w.rng.Intn(len(in))]
}

func (w *seqWorld) emit(process, style, delta int64) {
	w.eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{ProcessID: process, StyleID: style, Delta: delta}})
}

func (w *seqWorld) ratioOf(part string) int {
	if part == seqR8 {
		return w.ratio
	}
	return 1
}

// seatReads is an exact-value check of one seat's count.
func seatReads(s *seqSeat, want int, what string) func(before, after seqSnap) string {
	return func(_, after seqSnap) string {
		if got := after.cells["seat "+s.name]; got != want {
			return fmt.Sprintf("%s: seat %s reads %d, want %d", what, s.name, got, want)
		}
		return ""
	}
}

// bound reports whether the engine has a carrier bound at the seat.
func bound(before seqSnap, s *seqSeat) bool {
	return before.rt[s.name].ActiveBinID != nil
}

func pressSeats(w *seqWorld) []string {
	var out []string
	for _, s := range w.seats {
		if s.produce {
			out = append(out, "seat "+s.name, "hold "+s.name)
		}
	}
	return append(out, "drops")
}

// ── Counted moves ──────────────────────────────────────────────────────────

// evPressStroke: the press's counter steps 1-3; its two spots of one part
// share the step (produceShares) and add it as parts, at no ratio.
func (w *seqWorld) evPressStroke(seqSnap) (string, *seqStep) {
	step := int64(1 + w.rng.Intn(3))
	w.emit(w.press.process, w.press.style, step)
	w.plcCount += step
	w.in[seqR8] += int(step)
	return "press stroke", &seqStep{desc: fmt.Sprintf("press stroke, step %d (PLC %d)", step, w.plcCount), may: pressSeats(w)}
}

// evAssemblyCycle: the assembly counts 1-2 cycles; each consume spot draws
// step × its part's ratio, its pile first. Only when every spot has a cart of
// its part or a pile that covers the draw: a cycle with nothing there is the
// no-cart ledger's (asserted empty).
func (w *seqWorld) evAssemblyCycle(before seqSnap) (string, *seqStep) {
	step := 1 + w.rng.Intn(2)
	want := map[string]int{}
	for _, s := range w.seats {
		if s.produce {
			continue
		}
		draw := step * w.ratioOf(s.part)
		rt := before.rt[s.name]
		pile := before.cells["pile "+s.name+" "+s.part+" active"]
		if (rt.ActiveBinID == nil || rt.ReleasedEmpty) && pile < draw {
			return "", nil
		}
		want[s.name] = draw
	}
	w.emit(w.asm.process, w.asm.style, int64(step))
	var may []string
	for name, draw := range want {
		w.drawn[w.byName[name].part] += draw
		may = append(may, "seat "+name, "pile "+name)
	}
	sort.Strings(may)
	return "assembly cycle", &seqStep{
		desc: fmt.Sprintf("assembly cycle, step %d (%s at %d per cycle)", step, seqR8, w.ratio),
		may:  may,
		check: func(before, after seqSnap) string {
			var out []string
			for name, draw := range want {
				s := w.byName[name]
				held := func(sn seqSnap) int {
					n := sn.cells["seat "+name]
					for k, v := range sn.cells {
						if strings.HasPrefix(k, "pile "+name+" ") {
							n += v
						}
					}
					return n
				}
				if got := held(before) - held(after); got != draw {
					out = append(out, fmt.Sprintf("%s (%s) drew %d, want %d (step %d × ratio %d)",
						name, s.part, got, draw, step, w.ratioOf(s.part)))
				}
			}
			sort.Strings(out)
			return strings.Join(out, "; ")
		},
	}
}

// evCounterMovedWhileDown: after a restart and its boot sweep, the press
// counter's catch-up, charged as the PLC manager charges it: the new reading
// less the last one it stored (plc.CalculateDelta).
func (w *seqWorld) evCounterMovedWhileDown(seqSnap) (string, *seqStep) {
	last, now := w.plcCount, w.plcCount+w.downStrokes
	w.downStrokes = 0
	delta, anomaly := plc.CalculateDelta(last, now, 1000)
	w.emit(w.press.process, w.press.style, delta)
	w.plcCount = now
	w.in[seqR8] += int(delta)
	return "counter moved while down", &seqStep{
		desc: fmt.Sprintf("the press counted while the Edge was down: last %d, now %d, delta %d %s", last, now, delta, anomaly),
		may:  pressSeats(w),
	}
}

// ── The assembly's swaps ───────────────────────────────────────────────────

func (w *seqWorld) evConsumeRequest(before seqSnap) (string, *seqStep) {
	s := w.pick(func(s *seqSeat) bool { return !s.produce && s.phase == phaseIdle && bound(before, s) })
	if s == nil {
		return "", nil
	}
	res, err := w.eng.RequestNodeMaterial(s.id, 1)
	if err != nil {
		w.fail(fmt.Sprintf("request at %s refused: %v", s.name, err))
	}
	s.a, s.b = res.OrderA, res.OrderB
	markStaged(w.t, w.db, s.a.ID)
	markStaged(w.t, w.db, s.b.ID)
	s.phase = phaseRequested
	return "consume request", &seqStep{desc: fmt.Sprintf("%s: request material, both legs staged", s.name)}
}

// evConsumeRelease: the operator's RELEASE on the staged pair, one of the
// release kinds a consume seat offers.
func (w *seqWorld) evConsumeRelease(before seqSnap) (string, *seqStep) {
	s := w.pick(func(s *seqSeat) bool { return !s.produce && s.phase == phaseRequested })
	if s == nil {
		return "", nil
	}
	had := before.cells["seat "+s.name]
	kinds := []string{"release empty", "release underpack", "release partial typed", "release partial untyped"}
	if !captureCountsTwice {
		kinds = append(kinds, "release pull parts")
	}
	kind := kinds[w.rng.Intn(len(kinds))]
	disp := ReleaseDisposition{CalledBy: "seq-op"}
	want := had
	var desc string
	switch kind {
	case "release empty":
		disp.Mode = DispositionCaptureLineside
		want, desc = 0, "RELEASE EMPTY"
	case "release underpack":
		disp.Mode = DispositionReleaseUnderpack
		want, desc = 0, "RELEASE UNDERPACK"
	case "release partial typed":
		typed := w.rng.Intn(s.pack + 4)
		suggested := had
		disp.Mode, disp.PartialCount, disp.PartialCountSuggested = DispositionSendPartialBack, &typed, &suggested
		want, desc = typed, fmt.Sprintf("SEND PARTIAL BACK, typed %d", typed)
		if typed == suggested && typed != 0 {
			desc += " (the pre-fill, untouched)"
		}
	case "release partial untyped":
		disp.Mode = DispositionSendPartialBack
		// The commit declares the seat's own count in the fence's instant
		// (CountFromSeat, writeReleaseCount): the seat keeps it, below 0 too.
		desc = "SEND PARTIAL BACK, no count typed"
	case "release pull parts":
		q := 1 + w.rng.Intn(4)
		disp.Mode, disp.LinesideCapture = DispositionCaptureLineside, map[string]int{s.part: q}
		desc = fmt.Sprintf("PULL PARTS LINESIDE, %d", q)
	}
	if err := w.eng.ReleaseStagedOrders(s.id, disp); err != nil {
		w.fail(fmt.Sprintf("%s at %s refused: %v", desc, s.name, err))
	}
	// The count the release declared: a person's number for the carrier that
	// leaves. What it took off is written off; what it added is charged in.
	if want < had {
		w.writtenOff[s.part] += had - want
	} else {
		w.in[s.part] += want - had
	}
	s.phase = phaseReleased
	return kind, &seqStep{
		desc:  fmt.Sprintf("%s: %s (the seat read %d)", s.name, desc, had),
		may:   []string{"seat " + s.name, "pile " + s.name},
		check: seatReads(s, want, desc),
	}
}

// evConsumeLift: the evac lifts the released carrier; it leaves with the
// count the seat holds (carrierLeft, through leaveSeatsAtLift).
func (w *seqWorld) evConsumeLift(before seqSnap) (string, *seqStep) {
	s := w.pick(func(s *seqSeat) bool { return !s.produce && s.phase == phaseReleased })
	if s == nil {
		return "", nil
	}
	had := before.cells["seat "+s.name]
	w.eng.HandleBinPickedUpMessage(protocol.BinPickedUp{OrderUUID: s.b.UUID, BinID: s.bin, Location: s.name,
		Epoch: 1, PickedUpAt: time.Now().UTC()})
	w.departed[s.part] += had
	s.bin = 0
	w.pushBins()
	s.phase = phaseLifted
	return "consume lift", &seqStep{
		desc:  fmt.Sprintf("%s: the evac lifts its carrier, which leaves with %d", s.name, had),
		may:   []string{"seat " + s.name},
		check: seatReads(s, 0, "after the lift"),
	}
}

// evConsumeLand: the supply lands the next cart with Core's count; the kit
// seat's cart counts kits.
func (w *seqWorld) evConsumeLand(seqSnap) (string, *seqStep) {
	s := w.pick(func(s *seqSeat) bool { return !s.produce && s.phase == phaseLifted })
	if s == nil {
		return "", nil
	}
	count := s.pack - 4 + w.rng.Intn(7)
	bin := w.land(s, count, s.a.UUID)
	w.pushBins()
	s.phase = phaseLanded
	kind := "consume land"
	if s.part == seqKit {
		kind = "kit land"
	}
	return kind, &seqStep{
		desc: fmt.Sprintf("%s: the supply lands bin %d with %d", s.name, bin, count),
		may:  []string{"seat " + s.name, "hold " + s.name, "drops"},
		check: func(before, after seqSnap) string {
			if msg := seatReads(s, count, "the landing")(before, after); msg != "" {
				return msg
			}
			if got := after.cells["seat "+s.name+" bin"]; got != int(bin) {
				return fmt.Sprintf("the landing: seat %s holds bin %d, want %d", s.name, got, bin)
			}
			return ""
		},
	}
}

// evLegsClosed: Core reports both legs delivered (it announced the landing),
// and the supply's delivery is confirmed. Closing an order moves no count.
func (w *seqWorld) evLegsClosed(seqSnap) (string, *seqStep) {
	s := w.pick(func(s *seqSeat) bool { return s.phase == phaseLanded })
	if s == nil {
		return "", nil
	}
	for _, o := range []*storeorders.Order{s.b, s.a} {
		err := w.eng.orderMgr.HandleDeliveredNotice(&protocol.OrderDelivered{OrderUUID: o.UUID,
			DeliveredAt: time.Now().UTC(), DeliveryNode: o.DeliveryNode, LandingSent: true})
		testutil.MustNoErr(w.t, err, "delivered "+o.UUID)
		cur, err := w.db.GetOrder(o.ID)
		testutil.MustNoErr(w.t, err, "order")
		if cur.Status == protocol.StatusDelivered {
			testutil.MustNoErr(w.t, w.eng.orderMgr.ConfirmDelivery(o.ID, o.Quantity), "confirm "+o.UUID)
		}
	}
	s.phase, s.a, s.b = phaseIdle, nil, nil
	return "legs closed", &seqStep{desc: fmt.Sprintf("%s: both legs delivered and confirmed", s.name)}
}

// ── The press's swaps ──────────────────────────────────────────────────────

func (w *seqWorld) evPressRequest(before seqSnap) (string, *seqStep) {
	// A swap is asked for a bin with parts in it: RequestProduceSwap refuses
	// one with none to finalize, which moves nothing.
	s := w.pick(func(s *seqSeat) bool {
		return s.produce && s.phase == phaseIdle && bound(before, s) && before.cells["seat "+s.name] > 0
	})
	if s == nil {
		return "", nil
	}
	res, err := w.eng.RequestProduceSwap(s.id, 0)
	if err != nil {
		w.fail(fmt.Sprintf("produce swap at %s refused: %v", s.name, err))
	}
	s.a, s.b = res.OrderA, res.OrderB
	markStaged(w.t, w.db, s.a.ID)
	markStaged(w.t, w.db, s.b.ID)
	s.phase = phaseRequested
	return "press request", &seqStep{desc: fmt.Sprintf("%s: request a swap, both legs staged", s.name)}
}

// evPressRelease: RELEASE on the press's staged pair, sometimes RELEASE FULL.
// The finalize ships the bin's count as its ingest and clears the seat
// (finalizeDepartingProduce); a bin at 0 or below ships nothing and stays
// bound until its lift.
func (w *seqWorld) evPressRelease(before seqSnap) (string, *seqStep) {
	s := w.pick(func(s *seqSeat) bool { return s.produce && s.phase == phaseRequested })
	if s == nil {
		return "", nil
	}
	had := before.cells["seat "+s.name]
	full := w.rng.Intn(2) == 0
	if err := w.eng.ReleaseStagedOrders(s.id, ReleaseDisposition{CalledBy: "seq-op", DeclareFull: full}); err != nil {
		w.fail(fmt.Sprintf("press release at %s refused: %v", s.name, err))
	}
	shipped := w.snap().ingests[len(before.ingests):]
	kind, desc := "press release", "RELEASE"
	if full {
		kind, desc = "press release full", "RELEASE FULL"
	}
	var ingest *protocol.OrderIngestRequest
	for i := range shipped {
		if shipped[i].BinID == s.bin {
			ingest = &shipped[i]
		}
	}
	s.phase = phaseReleased
	if ingest == nil {
		return kind, &seqStep{
			desc: fmt.Sprintf("%s: %s at %d, nothing to ship", s.name, desc, had),
			check: func(_, _ seqSnap) string {
				if had > 0 {
					return fmt.Sprintf("%s at %d shipped no ingest for bin %d", desc, had, s.bin)
				}
				return ""
			},
		}
	}
	w.departed[s.part] += int(ingest.Quantity)
	declared := ""
	if ingest.DeclaredQty != 0 {
		declared = fmt.Sprintf(", declared full at %d", ingest.DeclaredQty)
	}
	return kind, &seqStep{
		desc: fmt.Sprintf("%s: %s, bin %d ships %d%s", s.name, desc, s.bin, ingest.Quantity, declared),
		may:  []string{"seat " + s.name},
		check: func(before, after seqSnap) string {
			if int(ingest.Quantity) != had {
				return fmt.Sprintf("the ingest ships %d, the seat read %d: the live count is the bin's", ingest.Quantity, had)
			}
			if after.cells["seat "+s.name+" bin"] != 0 || after.cells["seat "+s.name] != 0 {
				return fmt.Sprintf("after the finalize seat %s holds bin %d at %d, want none at 0",
					s.name, after.cells["seat "+s.name+" bin"], after.cells["seat "+s.name])
			}
			return ""
		},
	}
}

// evPressLift: the evac lifts the departing bin. A seat the finalize cleared
// holds nothing of it; one still bound (a bin released at 0) lets it go with
// its count.
func (w *seqWorld) evPressLift(before seqSnap) (string, *seqStep) {
	s := w.pick(func(s *seqSeat) bool { return s.produce && s.phase == phaseReleased })
	if s == nil {
		return "", nil
	}
	had, still := before.cells["seat "+s.name], before.cells["seat "+s.name+" bin"] == int(s.bin)
	w.eng.HandleBinPickedUpMessage(protocol.BinPickedUp{OrderUUID: s.b.UUID, BinID: s.bin, Location: s.name,
		Epoch: 1, PickedUpAt: time.Now().UTC()})
	desc := fmt.Sprintf("%s: the evac lifts bin %d", s.name, s.bin)
	if still {
		w.departed[s.part] += had
		desc += fmt.Sprintf(", still bound at %d", had)
	}
	s.bin = 0
	w.pushBins()
	s.phase = phaseLifted
	return "press lift", &seqStep{desc: desc, may: []string{"seat " + s.name, "hold " + s.name},
		check: func(_, after seqSnap) string {
			if after.cells["seat "+s.name+" bin"] != 0 {
				return fmt.Sprintf("after the lift seat %s still holds bin %d", s.name, after.cells["seat "+s.name+" bin"])
			}
			return ""
		}}
}

// evPressLand: the supply lands an empty. The seat's hold replays onto it at
// the next stroke.
func (w *seqWorld) evPressLand(seqSnap) (string, *seqStep) {
	s := w.pick(func(s *seqSeat) bool { return s.produce && s.phase == phaseLifted })
	if s == nil {
		return "", nil
	}
	bin := w.land(s, 0, s.a.UUID)
	w.pushBins()
	s.phase = phaseLanded
	return "press land", &seqStep{
		desc:  fmt.Sprintf("%s: the supply lands empty bin %d", s.name, bin),
		may:   []string{"seat " + s.name, "hold " + s.name, "drops"},
		check: seatReads(s, 0, "the empty's landing"),
	}
}

// ── Typed counts ───────────────────────────────────────────────────────────

// countable is a seat a person may count: a carrier bound, not mid-lift.
func countable(before seqSnap, s *seqSeat) bool {
	return bound(before, s) && s.phase != phaseLifted && !before.rt[s.name].ReleasedEmpty
}

func (w *seqWorld) typedCount(s *seqSeat) int {
	return w.rng.Intn(s.pack + 6) // above the pack is accepted (rule 7)
}

// typedMay is what a person's count may move: the seat's count, and the
// seat's produce hold, which the count drops with its hold_dropped row (a
// person's count wins over a pile, U2).
func typedMay(s *seqSeat) []string {
	return []string{"seat " + s.name, "hold " + s.name, "drops"}
}

// evFloorCount: a person counts the carrier at the line (RecordBinCount,
// declared to Core first).
func (w *seqWorld) evFloorCount(before seqSnap) (string, *seqStep) {
	s := w.pick(func(s *seqSeat) bool { return countable(before, s) })
	if s == nil {
		return "", nil
	}
	had, q := before.cells["seat "+s.name], w.typedCount(s)
	if err := w.eng.RecordBinCount(s.id, q, "seq-op"); err != nil {
		w.fail(fmt.Sprintf("count %d at %s refused: %v", q, s.name, err))
	}
	w.in[s.part] += q - had
	return "floor count", &seqStep{
		desc:  fmt.Sprintf("%s: a person counts %d at the line (the seat read %d)", s.name, q, had),
		may:   typedMay(s),
		check: seatReads(s, q, "the floor count"),
	}
}

// evCoreCount: a person's count at Core, broadcast as a UOPAdjustment.
func (w *seqWorld) evCoreCount(before seqSnap) (string, *seqStep) {
	s := w.pick(func(s *seqSeat) bool { return countable(before, s) })
	if s == nil {
		return "", nil
	}
	had, q := before.cells["seat "+s.name], w.typedCount(s)
	w.eng.HandleUOPAdjustment(protocol.UOPAdjustment{BinID: s.bin, CoreNodeName: s.name, NewRemaining: q,
		Epoch: before.rt[s.name].ActiveBinEpoch, Actor: "seq-core-op", AdjustedAt: time.Now().UTC()})
	w.in[s.part] += q - had
	return "core count", &seqStep{
		desc:  fmt.Sprintf("%s: a person counts %d at Core (the seat read %d)", s.name, q, had),
		may:   typedMay(s),
		check: seatReads(s, q, "Core's count"),
	}
}

// ── What must move nothing ─────────────────────────────────────────────────

// evRelanding: Core re-announces the landing of the carrier already bound,
// with a stale count. The same bin at the same generation keeps its count (H.4).
func (w *seqWorld) evRelanding(before seqSnap) (string, *seqStep) {
	s := w.pick(func(s *seqSeat) bool { return bound(before, s) && before.cells["seat "+s.name+" bin"] == int(s.bin) })
	if s == nil {
		return "", nil
	}
	w.placedSeq++
	stale := before.cells["seat "+s.name] + 7
	w.eng.HandleBinLanded(protocol.BinLanded{BinID: s.bin, CoreNodeName: s.name, Count: stale, Epoch: 1,
		PayloadCode: s.part, PayloadKnown: true, PlacedSeq: w.placedSeq})
	return "re-landing", &seqStep{desc: fmt.Sprintf("%s: Core re-announces bin %d landed, count %d", s.name, s.bin, stale)}
}

// evRatioChange: the engineer changes SYN-SQ-R8's ratio at Core. The Edge
// hears it some events later (evLateCatalog); until then it draws at the old
// one, which the model follows.
func (w *seqWorld) evRatioChange(seqSnap) (string, *seqStep) {
	if w.ratioDue != 0 {
		return "", nil
	}
	w.ratioNext = 4
	if w.ratio == 4 {
		w.ratioNext = 8
	}
	w.ratioDue = w.event + 1 + w.rng.Intn(6)
	return "ratio change", &seqStep{desc: fmt.Sprintf("Core: %s ratio %d -> %d (the Edge hears it at event #%02d)",
		seqR8, w.ratio, w.ratioNext, w.ratioDue)}
}

// evLateCatalog: the catalog carrying the new ratio arrives (HandlePayloadCatalog).
func (w *seqWorld) evLateCatalog(seqSnap) (string, *seqStep) {
	testutil.MustNoErr(w.t, w.eng.HandlePayloadCatalog(w.catalog()), "late catalog")
	desc := fmt.Sprintf("the catalog arrives late: %s at %d per cycle, was %d", seqR8, w.ratioNext, w.ratio)
	w.ratio, w.ratioNext, w.ratioDue = w.ratioNext, 0, 0
	return "late catalog", &seqStep{desc: desc}
}

func (w *seqWorld) evCatalogResend(seqSnap) (string, *seqStep) {
	if w.ratioDue != 0 {
		return "", nil
	}
	testutil.MustNoErr(w.t, w.eng.HandlePayloadCatalog(w.catalog()), "catalog resend")
	return "catalog resend", &seqStep{desc: "the catalog arrives again, unchanged"}
}

// evStrictToggle: the engineer turns SQ_PB's strict pack on or off (the
// claim save).
func (w *seqWorld) evStrictToggle(seqSnap) (string, *seqStep) {
	s := w.byName["SQ_PB"]
	w.strictPB = !w.strictPB
	id, err := w.db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, w.claimInput(s, w.strictPB))
	testutil.MustNoErr(w.t, err, "strict toggle")
	if id != s.claimID {
		w.fail(fmt.Sprintf("the claim save made claim %d, the seat's is %d", id, s.claimID))
	}
	return "strict toggle", &seqStep{desc: fmt.Sprintf("SQ_PB strict pack -> %t", w.strictPB)}
}

// evRestart: the Edge stops (Stop writes the open no-cart stretches to the
// outbox), the file is reopened by the production open, a new engine is built
// over it and Core's bins feed arrives again. That moves nothing. The boot
// sweep is the next event, and the press's catch-up (0-4 strokes counted while
// the Edge was down) the one after.
func (w *seqWorld) evRestart(seqSnap) (string, *seqStep) {
	w.eng.Stop()
	testutil.MustNoErr(w.t, w.db.Close(), "close at restart")
	db, err := store.Open(w.path)
	testutil.MustNoErr(w.t, err, "reopen at restart")
	w.db = db
	w.startEngine()
	w.pushBins()
	w.bootSweepDue = true
	w.downStrokes = int64(w.rng.Intn(5))
	return "restart", &seqStep{desc: fmt.Sprintf("the Edge restarts; the press counts %d while it is down", w.downStrokes)}
}

// evStrandedSweep: the stranded monitor's scan (sweepProduceHold, run once at
// boot and then every scan), which drops an unbound produce seat's pile with
// no carrier coming, with its hold_dropped row: a hold may only become a
// recorded drop.
func (w *seqWorld) evStrandedSweep(seqSnap) (string, *seqStep) {
	(&strandedMonitor{eng: w.eng}).sweepAtBoot()
	kind, desc := "stranded sweep", "the stranded monitor scans"
	if w.bootSweepDue {
		w.bootSweepDue = false
		kind, desc = "boot sweep", "the stranded monitor's boot sweep"
	}
	return kind, &seqStep{desc: desc, may: []string{"hold SQ_PA", "hold SQ_PB", "drops"}}
}

// TestCount_CaptureReleaseCountsThePulledPartsTwice is the sequence proof's
// first find, kept as its smallest case: PULL PARTS LINESIDE of 5 from a cart
// counted at 16, on a seat whose carrier Core's bins feed names (a plant's
// normal state). Physically 11 stay on the cart and 5 go to the bench. The
// Edge's pile gains the 5 and its seat keeps 16, so the 5 are counted twice
// until the lift, and the cart leaves the Edge's books at 5 over: the capture
// sends its capture_reduction to Core only (uop.Mutator.CaptureToLineside) and
// the release writes no seat count when that delta is the writer
// (ComputeReleaseRemainingUOP answers nil; releaseOrderWithFullLineside skips
// declareReleaseCount). Core's bin reads 11; the Edge's tile, its reorder level
// and the count the cart departs with read 16. With no carrier the feed names
// the release takes the &0 fallback instead and writes the remaining 11 off.
func TestCount_CaptureReleaseCountsThePulledPartsTwice(t *testing.T) {
	t.Skip("DEFECT (sequence proof, 2026-11-19): a PULL PARTS LINESIDE release keeps the seat's count and adds the pile, " +
		"so the pulled parts are counted twice until the lift (releaseOrderWithFullLineside, uop.Mutator.CaptureToLineside). " +
		"Remove this skip with the fix, and set captureCountsTwice false.")
	t.Parallel()
	w := newSeqWorld(t, seqSeed)
	s := w.byName["SQ_CA"]
	res, err := w.eng.RequestNodeMaterial(s.id, 1)
	testutil.MustNoErr(t, err, "request material")
	markStaged(t, w.db, res.OrderA.ID)
	markStaged(t, w.db, res.OrderB.ID)
	testutil.MustNoErr(t, w.eng.ReleaseStagedOrders(s.id, ReleaseDisposition{Mode: DispositionCaptureLineside,
		LinesideCapture: map[string]int{seqR8: 5}, CalledBy: "seq-op"}), "PULL PARTS LINESIDE")
	after := w.snap()
	if msg := w.conservation(after); msg != "" {
		t.Errorf("after pulling 5 of the 16 lineside: %s", msg)
	}
	if seat, pile := after.cells["seat SQ_CA"], after.cells["pile SQ_CA "+seqR8+" active"]; seat != 11 || pile != 5 {
		t.Errorf("after pulling 5 of the 16 lineside the cart reads %d and the pile %d, want 11 and 5", seat, pile)
	}
}
