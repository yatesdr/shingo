// The press tranche's scenario matrix, carried end to end across the two
// modules: every swap mode × every case of the swap chain × both roles ×
// flipped and unflipped, each cell that does not exist listed with why.
//
// The tranche's prompt puts the matrix here and not in the sim
// (PROMPT-lead-press-tranche2-swap-chain-2026-10-21.md, "The sim"): this module
// builds the Edge's steps and runs Core, so each cell is FORCED into its state
// (Core's rows, the Edge's orders) rather than waited for, and its outcome is
// read at both ends: the Edge's rows, Core's rows and queue codes, and where
// the bins end up.
//
// Every cell is the plain keep-staged recovery cell (ksrCell): the line holds
// A's bin, A's market holds A's stock, no spare is kept and none is parked on
// the spot. Its claims reach Core's mirror (mirror): W5's hold and fallback
// read the fed shape there (DECISIONS D6.11). The Edge's inventory sink is
// wired, so a landing binds as it does in the plant, and a delivery that does
// not bind fails the cell.
//
// The cases, from the prompt's matrix paragraph and the lead's brief:
//
//	vacant            a vacant dependent position: the prime first, the swap
//	                  held behind it (waiting_for_outside), then the swap runs
//	coming            a prime already in flight: no second prime, the swap
//	                  held behind it
//	wrong             a wrong carrier on the on-deck: cleared, primed, swapped,
//	                  the swap held behind the clear (waiting_for_outside)
//	wrong-at-nothing  consume: the part's carrier counted at nothing (D4.3)
//	marker            a bare marker on the on-deck: wrong, so the same
//	dry               the role's market dry: the swap queues
//	                  (waiting_for_material) and goes when stock lands
//	double            a double tap: the second refused, one set of orders
//	cold              a cold produce press: its primes and no swap
//	changeover        a changeover into a vacant press: each position an Add
//	unchanged         an Unchanged changeover with a vacant on-deck
//
//go:build docker

package scenarios

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"

	corebins "shingocore/store/bins"

	"shingoedge/domain"
	edgeengine "shingoedge/engine"
	edgemessaging "shingoedge/messaging"
	edgeorders "shingoedge/orders"
	"shingoedge/store/processes"
	edgeuop "shingoedge/uop"
)

// ── the table ───────────────────────────────────────────────────────────────

// pmRow is one cell of the matrix: a mode, a role, the flip and a case.
type pmRow struct {
	mode    protocol.SwapMode
	role    protocol.ClaimRole
	flipped bool
	cs      pmCase
}

func (r pmRow) name() string {
	flip := "unflipped"
	if r.flipped {
		flip = "flipped"
	}
	return fmt.Sprintf("%s/%s/%s/%s", r.mode, r.role, flip, r.cs.name)
}

func (r pmRow) press() bool { return r.mode == protocol.SwapModeTwoRobotPressIndex }

// pmCase is one case of the swap chain. na says why the case does not exist
// in a row ("" where it runs); run forces the state and asserts the outcome.
type pmCase struct {
	name string
	na   func(r pmRow) string
	run  func(p *pmCell)
}

// The reasons a cell does not exist. Each is a fact of the code, and the
// structural ones are pinned by TestScenario_PressMatrix_TheCellsThatDoNotExist.
const (
	pmNoDependent = "a dependent position exists only on a press: NodeClaim.DependentPositions is nil " +
		"off two_robot_press_index, so ChainReadiness reads nothing and no prime or clear is made"
	pmNoOnDeckSeq = "sequential has no on-deck: its swap lifts only at the line (its partner is a line of " +
		"its own claim, DependentPositions nil)"
	pmNoOnDeck = "no on-deck off a press: the swap lifts only at the line (DependentPositions nil), so " +
		"a carrier standing elsewhere is no step of it"
	pmNoMarker = "the marker is press-index only: a marker on an on-deck needs an on-deck"
	pmFlipOnly = "the flip (IndexRobotSupplies) is press-index only: it shapes only " +
		"BuildTwoRobotPressIndexSwapSteps, and every other mode's swap is the same either way"
	pmFlipNoIndex = "the flip shapes only the press's index swap; a changeover into a vacant press plans " +
		"each position an Add (a plain delivery), which the flip does not shape, and the outgoing mode " +
		"has no flip"
	pmNoCountGate = "consume has no count gate: the count check is the finalizing produce request's " +
		"(produceRequest, \"no parts to finalize\")"
	pmAtNothingConsume = "the at-nothing carrier is consume's (D4.3: a carrier of the part counted at " +
		"nothing does not hold the part); a produce on-deck's stamped carrier is the wrong row's rule"
)

// naOnDeck is the reason a case about the on-deck does not exist in a mode.
func naOnDeck(r pmRow) string {
	switch {
	case r.mode == protocol.SwapModeSequential:
		return pmNoOnDeckSeq
	case !r.press():
		return pmNoOnDeck
	}
	return ""
}

// naFlip is the reason a flipped row off a press does not exist.
func naFlip(r pmRow) string {
	if r.flipped && !r.press() {
		return pmFlipOnly
	}
	return ""
}

var pmCases = []pmCase{
	{name: "vacant", run: pmVacant, na: func(r pmRow) string {
		if !r.press() {
			return pmNoDependent
		}
		return ""
	}},
	{name: "coming", run: pmComing, na: func(r pmRow) string {
		if !r.press() {
			return pmNoDependent
		}
		return ""
	}},
	{name: "wrong", run: pmWrong, na: naOnDeck},
	{name: "wrong-at-nothing", run: pmWrongAtNothing, na: func(r pmRow) string {
		if s := naOnDeck(r); s != "" {
			return s
		}
		if r.role == protocol.ClaimRoleProduce {
			return pmAtNothingConsume
		}
		return ""
	}},
	{name: "marker", run: pmMarker, na: func(r pmRow) string {
		if !r.press() {
			return pmNoMarker
		}
		return ""
	}},
	{name: "dry", run: pmDry, na: naFlip},
	{name: "double", run: pmDouble, na: naFlip},
	{name: "cold", run: pmCold, na: func(r pmRow) string {
		if s := naFlip(r); s != "" {
			return s
		}
		if r.role == protocol.ClaimRoleConsume {
			return pmNoCountGate
		}
		return ""
	}},
	{name: "changeover", run: pmChangeoverIntoVacant, na: func(r pmRow) string {
		if r.flipped && !r.press() {
			return pmFlipNoIndex
		}
		return ""
	}},
	{name: "unchanged", run: pmUnchanged, na: naOnDeck},
}

// pmRows is every cell of the matrix, the ones that do not exist with them.
func pmRows() []pmRow {
	var rows []pmRow
	for _, mode := range protocol.ConfigurableSwapModes() {
		for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
			for _, flipped := range []bool{false, true} {
				for _, cs := range pmCases {
					rows = append(rows, pmRow{mode: mode, role: role, flipped: flipped, cs: cs})
				}
			}
		}
	}
	return rows
}

// TestScenario_PressMatrix runs every cell of the matrix that exists and
// lists, as a skip with its reason, every one that does not.
//
// THE CELLS RUN IN PARALLEL, as the keep-staged recovery cells do and for the
// same reason: each builds its own Core, Edge and database and spends nearly
// all its time waiting on its bus. They share the default clock, which must be
// the kick clock for every Edge's reconciler (kickClock): one is installed
// here, before any cell starts, and removed after the last one ends.
func TestScenario_PressMatrix(t *testing.T) {
	kicks := &kickClock{}
	clock.SetDefault(kicks)
	t.Cleanup(func() { clock.SetDefault(clock.Real()) })
	for _, r := range pmRows() {
		t.Run(r.name(), func(t *testing.T) {
			if why := r.cs.na(r); why != "" {
				t.Skip("does not exist: " + why)
			}
			t.Parallel()
			started := time.Now()
			defer func() { t.Logf("cell %s ran %s", r.name(), time.Since(started).Round(time.Second)) }()
			r.cs.run(newPmCell(t, r, kicks))
		})
	}
}

// TestScenario_PressMatrix_TheCellsThatDoNotExist pins the structural reasons
// the matrix gives for the cells it skips: off a press there is no dependent
// position (so no on-deck, no wrong carrier or marker on one, no prime), and
// the flip changes no step of any swap but the press's.
func TestScenario_PressMatrix_TheCellsThatDoNotExist(t *testing.T) {
	node := &processes.Node{Name: ksrLine}
	for _, mode := range protocol.ConfigurableSwapModes() {
		for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
			claim := func(flipped bool) *processes.NodeClaim {
				return &processes.NodeClaim{CoreNodeName: ksrLine, Role: role, SwapMode: mode, PayloadCode: ksrPartA,
					InboundSource: ksrMktA, InboundStaging: ksrSpot, OutboundStaging: ksrOutStg,
					OutboundDestination: ksrDest, PairedCoreNode: ksrDeck, IndexRobotSupplies: flipped}
			}
			dep := claim(false).DependentPositions()
			if mode == protocol.SwapModeTwoRobotPressIndex {
				if !slices.Equal(dep, []string{ksrDeck}) {
					t.Errorf("%s/%s: dependent positions %v, want [%s]", mode, role, dep, ksrDeck)
				}
			} else if len(dep) != 0 {
				t.Errorf("%s/%s: dependent positions %v with a paired name, want none off a press", mode, role, dep)
			}
			plain, err := edgeengine.BuildSwapDispatch(node, claim(false))
			mustNil(t, err, string(mode)+" unflipped swap")
			flipped, err := edgeengine.BuildSwapDispatch(node, claim(true))
			mustNil(t, err, string(mode)+" flipped swap")
			same := reflect.DeepEqual(plain, flipped)
			if mode == protocol.SwapModeTwoRobotPressIndex && same {
				t.Errorf("%s/%s: the flip changed no step of the press's swap", mode, role)
			}
			if mode != protocol.SwapModeTwoRobotPressIndex && !same {
				t.Errorf("%s/%s: the flip changed the swap of a mode that is not a press", mode, role)
			}
		}
	}
}

// ── the cell ────────────────────────────────────────────────────────────────

// pmCell is a matrix row's cell: the plain keep-staged recovery cell, with
// what the row reads besides.
type pmCell struct {
	*ksrCell
	r pmRow
	// deckID is the paired position's process node, where a case gives it one
	// (deckNode); 0 otherwise.
	deckID int64

	mu       sync.Mutex
	notBound []string
}

// newPmCell builds a row's cell: the plain cell of the row's mode and role,
// mirrored at Core, no spare on the spot, flipped as the row is, the Edge's
// inventory sink wired, and the line with parts counted (a count a produce
// request will finalize). The dry case's market is stocked with nothing, the
// Unchanged case's style B claims what A claims, and the changeover case's
// style B is a press.
func newPmCell(t *testing.T, r pmRow, kicks *kickClock) *pmCell {
	t.Helper()
	o := ksrOpts{role: r.role, mode: r.mode, plain: true, mirror: true, bareSpot: true, flipped: r.flipped, kicks: kicks}
	switch r.cs.name {
	case "dry":
		o.dryA = true
	case "unchanged":
		o.sameB = true
	case "changeover":
		// Into a press: from a press of another part and carrier, or across
		// modes from any other (the cross-mode read, FanOutPressIndexCrossMode).
		o.modeB = protocol.SwapModeTwoRobotPressIndex
	}
	p := &pmCell{ksrCell: newKsrCell(t, o), r: r}
	// The landing binds through the seat's count writer, which the harness
	// leaves unwired: wired, as in the plant (send_back_test.go).
	p.edge.Engine.SetInventoryDeltaSink(edgeuop.New(p.edge.DB, "edge.test", p.edge.DB, p.edge.DB))
	p.edge.Engine.Events.SubscribeTypes(func(evt edgeengine.Event) {
		if a, ok := evt.Payload.(edgeengine.DeliveredNotBoundEvent); ok {
			p.mu.Lock()
			p.notBound = append(p.notBound, a.CoreNodeName)
			p.mu.Unlock()
		}
	}, edgeengine.EventDeliveredNotBound)
	p.counted(30)
	return p
}

// counted sets the line's count of parts.
func (p *pmCell) counted(n int) {
	p.t.Helper()
	mustNil(p.t, p.edge.DB.SetProcessNodeRuntime(p.nodeID, &p.claimA, n), fmt.Sprintf("the line counts %d", n))
	p.settle()
}

// tap is one tap of the role's request button: the material request on a
// consume line, the produce request on a produce line.
func (p *pmCell) tap() (*edgeengine.NodeOrderResult, error) {
	if p.role == protocol.ClaimRoleProduce {
		return p.edge.Engine.RequestProduceSwap(p.nodeID, 0)
	}
	return p.edge.Engine.RequestNodeMaterial(p.nodeID, 1)
}

// mustTap is a tap the cell expects to be taken.
func (p *pmCell) mustTap(label string) {
	p.t.Helper()
	if _, err := p.tap(); err != nil {
		p.dump(label + " refused")
		p.t.Fatalf("%s refused: %v", label, err)
	}
}

// refusedTap is a tap the cell expects to be refused: it returns an error
// that names why, and no order is made.
func (p *pmCell) refusedTap(label, why string) {
	p.t.Helper()
	before := p.lastID()
	_, err := p.tap()
	p.tick()
	made := p.newRows(before)
	if err == nil || len(made) != 0 {
		p.dump(label)
		p.t.Fatalf("%s: err=%v and %d orders made, want a refusal and none", label, err, len(made))
	}
	if why != "" && !strings.Contains(err.Error(), why) {
		p.t.Errorf("%s: refused with %q, want it to say %q", label, err, why)
	}
	p.t.Logf("%s: refused: %v", label, err)
}

// legsWant is how many legs a swap of the row's mode makes.
func (p *pmCell) legsWant() int {
	if p.mode == protocol.SwapModeSingleRobot || p.mode == protocol.SwapModeSequential {
		return 1
	}
	return 2
}

// ── what a tap made ─────────────────────────────────────────────────────────

// pmMade is one tap's orders, sorted by what each does.
type pmMade struct {
	clears, primes, legs, toLine, other []domain.Order
}

// read sorts every order made after before: a clear is a move off the
// paired position, a prime a delivery to it, a leg a complex order.
func (p *pmCell) read(before int64) pmMade {
	var m pmMade
	for _, o := range p.newRows(before) {
		switch {
		case o.OrderType == protocol.OrderTypeComplex:
			m.legs = append(m.legs, o)
		case o.OrderType == protocol.OrderTypeMove && o.SourceNode == ksrDeck:
			m.clears = append(m.clears, o)
		case o.DeliveryNode == ksrDeck:
			m.primes = append(m.primes, o)
		case o.DeliveryNode == ksrLine:
			m.toLine = append(m.toLine, o)
		default:
			m.other = append(m.other, o)
		}
	}
	return m
}

// chain reads one tap's orders on a press whose on-deck is not ready: clears
// clears, then primes primes, then the swap's legs, in creation order (the
// outbox drains by id, so Core is asked in that order: createClearsThenPrimes),
// nothing to the line, and every leg held at Core with no vendor order for the
// on-deck (waiting_for_outside).
func (p *pmCell) chain(label string, before int64, clears, primes int) pmMade {
	p.t.Helper()
	m := p.read(before)
	if len(m.clears) != clears || len(m.primes) != primes || len(m.legs) != 2 || len(m.toLine) != 0 || len(m.other) != 0 {
		p.dump(label)
		p.t.Fatalf("%s: clears %d, primes %d, legs %d, to the line %d, other %d; want %d, %d, 2, 0, 0",
			label, len(m.clears), len(m.primes), len(m.legs), len(m.toLine), len(m.other), clears, primes)
	}
	for _, c := range m.clears {
		if c.DeliveryNode != ksrDest {
			p.t.Errorf("%s: clear %d goes to %q, want the claim's outbound %s", label, c.ID, c.DeliveryNode, ksrDest)
		}
		for _, pr := range m.primes {
			if pr.ID < c.ID {
				p.t.Errorf("%s: prime %d was created before the clear %d", label, pr.ID, c.ID)
			}
		}
	}
	for _, l := range m.legs {
		for _, o := range append(append([]domain.Order(nil), m.clears...), m.primes...) {
			if l.ID < o.ID {
				p.t.Errorf("%s: swap leg %d was created before the chain's order %d", label, l.ID, o.ID)
			}
		}
	}
	p.held(label, m.legs, protocol.QueueWaitingForOutside)
	return m
}

// held asserts every leg is held at Core with no vendor order and the given
// queue code.
func (p *pmCell) held(label string, legs []domain.Order, code protocol.QueueCode) {
	p.t.Helper()
	for _, l := range legs {
		co := p.coreOf(l)
		if co.VendorOrderID != "" {
			p.dump(label)
			p.t.Fatalf("%s: swap leg %d went to the fleet (%s), want it held", label, l.ID, co.Status)
		}
		if co.QueueCode != string(code) {
			p.dump(label)
			p.t.Errorf("%s: swap leg %d is held as %q (%s: %q) at Core, want %q", label, l.ID, co.QueueCode,
				co.QueueCause, co.QueueReason, code)
		}
	}
}

// finish carries plain orders to their end: the fleet moves them, the
// operator confirms a delivered one, and each ends confirmed at Core.
func (p *pmCell) finish(label string, os ...domain.Order) {
	p.t.Helper()
	p.eventually(label+": done at both ends", func() bool {
		p.fleetStep()
		done := true
		for _, o := range os {
			if p.edgeRow(o.ID).Status == protocol.StatusDelivered {
				mustNil(p.t, p.edge.Engine.OrderManager().ConfirmDelivery(o.ID, 1), "confirm")
			}
			if !protocol.IsTerminal(p.edgeRow(o.ID).Status) || !protocol.IsTerminal(p.coreOf(o).Status) {
				done = false
			}
		}
		return done
	})
	for _, o := range os {
		if s := p.coreOf(o).Status; s != protocol.StatusConfirmed {
			p.t.Errorf("%s: order %d (%s %s->%s) ended %s at Core, want confirmed", label, o.ID, o.OrderType,
				o.SourceNode, o.DeliveryNode, s)
		}
	}
}

// ── what stands where ───────────────────────────────────────────────────────

// put forces a carrier onto a node in Core's rows: a bin of type bt, with
// part counted at uop when part is set (a carrier holding the part), an
// empty when it is not. The Edge's bins table gets it on the next push.
func (p *pmCell) put(node, label string, bt int64, part string, uop int) *corebins.Bin {
	p.t.Helper()
	coreDB := p.core.eng.DB()
	at, err := coreDB.GetNodeByName(node)
	mustNil(p.t, err, "node "+node)
	b := &corebins.Bin{BinTypeID: bt, Label: label, NodeID: &at.ID, Status: "available"}
	mustNil(p.t, coreDB.CreateBin(b), "bin "+label)
	if part != "" {
		mustNil(p.t, coreDB.SetBinManifest(b.ID, `{"items":[]}`, part, uop), "manifest "+label)
		mustNil(p.t, coreDB.ConfirmBinManifest(b.ID, ""), "confirm "+label)
	}
	p.bus.PushBins()
	p.settle()
	return b
}

// right forces the role's index carrier onto the paired position: a full of
// A on a consume press, an empty of A's carrier on a produce press.
func (p *pmCell) right() *corebins.Bin {
	if p.role == protocol.ClaimRoleProduce {
		return p.put(ksrDeck, "KSR-A-DECK", p.typeA.ID, "", 0)
	}
	return p.put(ksrDeck, "KSR-A-DECK", p.typeA.ID, ksrPartA, 40)
}

// stock lands what the line needs in A's market, on a free slot: a full of A
// for consume, an empty of A's carrier for produce.
func (p *pmCell) stock(label string) *corebins.Bin {
	p.t.Helper()
	var slot string
	mustNil(p.t, p.core.eng.DB().QueryRow(`SELECT n.name FROM nodes n WHERE n.parent_id = $1
		AND NOT EXISTS (SELECT 1 FROM bins b WHERE b.node_id = n.id) ORDER BY n.name LIMIT 1`, p.mktA.ID).Scan(&slot),
		"a free slot of A's market")
	if p.role == protocol.ClaimRoleProduce {
		return p.put(slot, label, p.typeA.ID, "", 0)
	}
	return p.put(slot, label, p.typeA.ID, ksrPartA, 40)
}

// at is where a bin stands, by Core's rows.
func (p *pmCell) at(id int64) string {
	p.t.Helper()
	b, err := p.core.eng.DB().GetBin(id)
	mustNil(p.t, err, fmt.Sprintf("bin %d", id))
	return p.nodeName(b.NodeID)
}

// binOf is the bin a Core order carried.
func (p *pmCell) binOf(o domain.Order) int64 {
	p.t.Helper()
	co := p.coreOf(o)
	if co.BinID == nil {
		p.dump("no bin")
		p.t.Fatalf("order %d (%s %s->%s) names no bin at Core", o.ID, o.OrderType, o.SourceNode, o.DeliveryNode)
	}
	return *co.BinID
}

// lineBinOut asserts the line's old bin went to the outbound and one bin
// stands on the line (and, on a press, one on the paired position).
func (p *pmCell) lineBinOut(label string) {
	p.t.Helper()
	bins, err := p.core.eng.DB().ListBins()
	mustNil(p.t, err, "bins")
	on := map[string]int{}
	for _, b := range bins {
		where := p.nodeName(b.NodeID)
		on[where]++
		if b.Label == "KSR-LINE-BIN" && !strings.HasPrefix(where, ksrDest) {
			p.t.Errorf("%s: the line's old bin is at %q, want the outbound %s", label, where, ksrDest)
		}
	}
	wantDeck := 0
	if p.press() {
		wantDeck = 1
	}
	if on[ksrLine] != 1 || on[ksrDeck] != wantDeck {
		p.dump(label)
		p.t.Errorf("%s: bins on the line %d and on the paired position %d, want 1 and %d", label, on[ksrLine],
			on[ksrDeck], wantDeck)
	}
}

func (p *pmCell) press() bool { return p.r.press() }

// settled is the cell left alone after its case: nothing live at either end,
// no bin claimed, and every delivery bound by its landing.
func (p *pmCell) settled(label string) {
	p.t.Helper()
	p.quiet()
	for _, o := range p.live() {
		p.t.Errorf("%s: Edge order %d (%s %s->%s) left %s: %q", label, o.ID, o.OrderType, o.SourceNode,
			o.DeliveryNode, o.Status, o.QueueReason)
	}
	active, err := p.core.eng.DB().ListActiveOrders()
	mustNil(p.t, err, "core active orders")
	for _, o := range active {
		p.t.Errorf("%s: Core order %d (%s %s->%s) left %s (cause %q: %s)", label, o.ID, o.OrderType, o.SourceNode,
			o.DeliveryNode, o.Status, o.QueueCause, o.QueueReason)
	}
	bins, err := p.core.eng.DB().ListBins()
	mustNil(p.t, err, "bins")
	for _, b := range bins {
		if b.ClaimedBy != nil {
			p.t.Errorf("%s: bin %s at %s still claimed by order %d", label, b.Label, p.nodeName(b.NodeID), *b.ClaimedBy)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.notBound) != 0 {
		p.t.Errorf("%s: deliveries not bound at %v", label, p.notBound)
	}
	if p.t.Failed() {
		p.dump(label)
	}
}

// ── the press cases ─────────────────────────────────────────────────────────

// vacantDeckSwap is case 1's tap and run on a press whose paired position is
// vacant: the prime first, the swap's legs held behind it at Core
// (waiting_for_outside), the prime lands, the swap runs and confirms, and the
// primed carrier is indexed onto the line.
func (p *pmCell) vacantDeckSwap(label string) {
	p.t.Helper()
	var m pmMade
	p.swap(label, func() {
		before := p.lastID()
		p.mustTap(label)
		p.tick()
		m = p.chain(label, before, 0, 1)
	})
	p.finish(label+": the prime", m.primes...)
	if at := p.at(p.binOf(m.primes[0])); at != ksrLine {
		p.t.Errorf("%s: the primed carrier is at %q, want indexed onto the line %s", label, at, ksrLine)
	}
	p.lineBinOut(label)
}

func pmVacant(p *pmCell) {
	p.vacantDeckSwap("one tap, the paired position vacant")
	p.settled("the end")
}

// pmComing: a prime already in flight to the paired position, as the Edge
// makes one (createClearsThenPrimes: on the line's process node, a move of a
// full for consume, a retrieve of an empty for produce), with the fleet and
// moving. One tap makes no second prime; the swap is held behind the carrier
// coming (waiting_for_outside: Core's CarrierComingTo), then runs.
func pmComing(p *pmCell) {
	nodeID := p.nodeID
	var first *domain.Order
	var err error
	if p.role == protocol.ClaimRoleProduce {
		first, err = p.edge.Engine.OrderManager().CreateRetrieveOrder(&nodeID, true, 1, ksrDeck, ksrMktA, "",
			"standard", ksrPartA, false, false, edgeorders.Origin{})
	} else {
		first, err = p.edge.Engine.OrderManager().CreateMoveOrder(&nodeID, 1, ksrMktA, ksrDeck, false,
			edgeorders.Origin{})
	}
	mustNil(p.t, err, "the prime already in flight")
	prime := *first
	p.eventually("the prime with the fleet", func() bool { return p.coreOf(prime).VendorOrderID != "" })
	p.drive(p.coreOf(prime), "RUNNING")

	label := "one tap, a carrier coming to the paired position"
	p.swap(label, func() {
		before := p.lastID()
		p.mustTap(label)
		p.tick()
		p.chain(label, before, 0, 0)
	})
	p.finish(label+": the prime already in flight", prime)
	if at := p.at(p.binOf(prime)); at != ksrLine {
		p.t.Errorf("%s: the carrier that was coming is at %q, want indexed onto the line %s", label, at, ksrLine)
	}
	p.lineBinOut(label)
	p.settled("the end")
}

// wrongDeckSwap is a wrong carrier forced onto the paired position and one
// tap: a named-bin clear of it to the claim's outbound, then the prime, then
// the swap held for the on-deck. The clear lifts and the prime has not landed:
// the swap is still held for the carrier coming. The prime lands, the swap
// runs, and the wrong carrier ends at the outbound.
func (p *pmCell) wrongDeckSwap(label string, wrong *corebins.Bin) {
	p.t.Helper()
	var m pmMade
	p.swap(label, func() {
		before := p.lastID()
		p.mustTap(label)
		p.tick()
		m = p.chain(label, before, 1, 1)
		if got := p.coreOf(m.clears[0]); got.BinID == nil || *got.BinID != wrong.ID {
			p.t.Errorf("%s: the clear names bin %v at Core, want the wrong carrier %d", label, got.BinID, wrong.ID)
		}
		p.eventually(label+": the clear with the fleet", func() bool { return p.coreOf(m.clears[0]).VendorOrderID != "" })
		p.drive(p.coreOf(m.clears[0]), "RUNNING", "FINISHED")
		p.tick()
		p.held(label+": the clear landed, the prime not yet", m.legs, protocol.QueueWaitingForOutside)
	})
	p.finish(label+": the clear and the prime", append(m.clears, m.primes...)...)
	if at := p.at(wrong.ID); !strings.HasPrefix(at, ksrDest) {
		p.t.Errorf("%s: the wrong carrier %s is at %q, want the outbound %s", label, wrong.Label, at, ksrDest)
	}
	if at := p.at(p.binOf(m.primes[0])); at != ksrLine {
		p.t.Errorf("%s: the primed carrier is at %q, want indexed onto the line %s", label, at, ksrLine)
	}
	p.lineBinOut(label)
}

// pmWrong: another part's carrier on the paired position (a B bin, full of
// B): wrong for both roles (wrongCarrier: consume, it does not hold the
// claim's part; produce, it holds a part).
func pmWrong(p *pmCell) {
	wrong := p.put(ksrDeck, "KSR-WRONG", p.typeB.ID, ksrPartB, 40)
	p.wrongDeckSwap("one tap, another part's carrier on the paired position", wrong)
	p.settled("the end")
}

// pmWrongAtNothing: consume, A's carrier stamped with A and counted at
// nothing on the paired position. It does not hold the part (D4.3), so it is
// cleared like any wrong carrier.
func pmWrongAtNothing(p *pmCell) {
	wrong := p.put(ksrDeck, "KSR-WRONG", p.typeA.ID, ksrPartA, 0)
	p.wrongDeckSwap("one tap, the part's carrier at nothing on the paired position", wrong)
	p.settled("the end")
}

// pmMarker: a bare marker cart (bin_types.bare_of set: A's carrier's
// marker) on the paired position. Any role reads it wrong (wrongCarrier's
// first arm): cleared, primed, swapped.
func pmMarker(p *pmCell) {
	marker, err := p.core.eng.DB().EnsureBareMarker(p.typeA.ID)
	mustNil(p.t, err, "A's carrier's bare marker")
	wrong := p.put(ksrDeck, "KSR-MARKER", marker, "", 0)
	p.wrongDeckSwap("one tap, a bare marker on the paired position", wrong)
	p.settled("the end")
}

// ── every mode ──────────────────────────────────────────────────────────────

// pmDry: the role's market holds none of what the line needs (dryA), the
// line and a press's paired position ready. One tap makes the swap, and Core
// queues it on waiting_for_material; a second tap is refused while it waits
// and nothing re-arms over Core's scanner passes; stock landing in the market
// lets it go, and it runs to the end.
//
// A sequential swap is its removal, which sources nothing: the removal goes,
// and the backfill it makes on its way is what queues (pmDrySequential).
func pmDry(p *pmCell) {
	if p.mode == protocol.SwapModeSequential {
		pmDrySequential(p)
		return
	}
	if p.press() {
		p.right()
	}
	label := "one tap, the market dry"
	var landed *corebins.Bin
	p.swap(label, func() {
		before := p.lastID()
		p.mustTap(label)
		p.tick()
		m := p.read(before)
		if len(m.legs) != p.legsWant() || len(m.clears)+len(m.primes)+len(m.toLine)+len(m.other) != 0 {
			p.dump(label)
			p.t.Fatalf("%s: %d legs and %d other orders, want %d legs and nothing else", label, len(m.legs),
				len(m.clears)+len(m.primes)+len(m.toLine)+len(m.other), p.legsWant())
		}
		p.held(label, m.legs, protocol.QueueWaitingForMaterial)
		p.refusedTap("a second tap while the swap waits for material", "")
		after := p.lastID()
		for range 3 {
			p.tick()
		}
		if again := p.newRows(after); len(again) != 0 {
			p.dump(label)
			p.t.Fatalf("%s: %d orders made over Core's scanner passes, want none: nothing re-arms", label, len(again))
		}
		p.held(label+": after Core's scanner passes", m.legs, protocol.QueueWaitingForMaterial)
		landed = p.stock("KSR-A-LANDED")
	})
	want := ksrLine
	if p.press() {
		// The landed carrier is the refill: it is set on the paired position,
		// behind the carrier the index took onto the line.
		want = ksrDeck
	}
	if at := p.at(landed.ID); at != want {
		p.t.Errorf("the carrier that landed in the market is at %q, want %s", at, want)
	}
	p.lineBinOut("the swap after the stock landed")
	p.settled("the end")
}

// pmDrySequential: the removal goes to the fleet at once (it sources
// nothing); at the line it makes the backfill, which Core queues on
// waiting_for_material; a second tap is refused; the stock lands and the
// backfill brings it.
func pmDrySequential(p *pmCell) {
	label := "one tap, the market dry"
	removal := p.seqRemoval(label, func(*ksrCell) error {
		_, err := p.tap()
		return err
	})
	if co := p.coreOf(removal); co.QueueCode != "" {
		p.t.Logf("%s: the removal reads %q at Core before the fleet takes it", label, co.QueueCode)
	}
	backfill := p.seqAtTheLine(label, removal)
	p.eventually(label+": the backfill queued for material", func() bool {
		co := p.coreOf(backfill)
		return co.VendorOrderID == "" && co.QueueCode == string(protocol.QueueWaitingForMaterial)
	})
	p.refusedTap("a second tap while the backfill waits for material", "")
	after := p.lastID()
	for range 3 {
		p.tick()
	}
	if again := p.newRows(after); len(again) != 0 {
		p.dump(label)
		p.t.Fatalf("%s: %d orders made over Core's scanner passes, want none", label, len(again))
	}
	p.held(label+": the backfill after Core's scanner passes", []domain.Order{backfill},
		protocol.QueueWaitingForMaterial)
	landed := p.stock("KSR-A-LANDED")
	p.seqLand(label, removal, backfill)
	p.seqConfirm(label, removal, backfill)
	if at := p.at(landed.ID); at != ksrLine {
		p.t.Errorf("the carrier that landed in the market is at %q, want the line %s", at, ksrLine)
	}
	p.lineBinOut("the cycle after the stock landed")
	p.settled("the end")
}

// pmDouble: two taps, the second at once. The second is refused while the
// first swap is live, and the first makes exactly one set of orders, which
// runs to the end. A press's paired position is vacant, so the set is the
// prime and the two legs, and the refusal is also the guard against a second
// prime.
func pmDouble(p *pmCell) {
	label := "a double tap"
	var m pmMade
	p.swap(label, func() {
		before := p.lastID()
		p.mustTap(label + ": the first")
		p.refusedTap(label+": the second", "already in progress")
		switch {
		case p.press():
			m = p.chain(label, before, 0, 1)
		default:
			m = p.read(before)
			if len(m.legs) != p.legsWant() || len(m.clears)+len(m.primes)+len(m.toLine)+len(m.other) != 0 {
				p.dump(label)
				p.t.Fatalf("%s: %d legs and %d other orders, want one set: %d legs", label, len(m.legs),
					len(m.clears)+len(m.primes)+len(m.toLine)+len(m.other), p.legsWant())
			}
		}
	})
	if p.press() {
		p.finish(label+": the prime", m.primes...)
	}
	p.lineBinOut(label)
	p.settled("the end")
}

// pmCold: a cold produce press, its count 0 with the line held. On a press
// with its paired position vacant, one tap makes its prime and no swap; the
// prime lands; a tap while it travels or after it has landed has nothing to
// prime and nothing counted, and is refused. Every other mode has nothing to
// prime, and the tap is refused as before.
func pmCold(p *pmCell) {
	p.counted(0)
	if !p.press() {
		p.refusedTap("one tap, nothing counted", "no parts to finalize")
		p.settled("the end")
		return
	}
	label := "one tap on a cold press"
	before := p.lastID()
	p.mustTap(label)
	p.tick()
	m := p.read(before)
	if len(m.primes) != 1 || len(m.legs)+len(m.clears)+len(m.toLine)+len(m.other) != 0 {
		p.dump(label)
		p.t.Fatalf("%s: primes %d, legs %d, other %d; want its prime and no swap", label, len(m.primes), len(m.legs),
			len(m.clears)+len(m.toLine)+len(m.other))
	}
	p.refusedTap(label+": a second tap while the prime travels", "no parts to finalize")
	p.finish(label+": the prime", m.primes...)
	if at := p.at(p.binOf(m.primes[0])); at != ksrDeck {
		p.t.Errorf("%s: the primed carrier is at %q, want the paired position %s", label, at, ksrDeck)
	}
	bins, err := p.core.eng.DB().ListBins()
	mustNil(p.t, err, "bins")
	for _, b := range bins {
		if b.Label == "KSR-LINE-BIN" && p.nodeName(b.NodeID) != ksrLine {
			p.t.Errorf("%s: the line's bin is at %q, want still on the line", label, p.nodeName(b.NodeID))
		}
	}
	p.refusedTap(label+": a tap after the prime landed", "no parts to finalize")
	p.settled("the end")
}

// ── the changeovers ─────────────────────────────────────────────────────────

// deckNode gives the paired position a process node with a runtime row, as
// the plant gives a changeover's participant one (an unresolved participant
// gets no order: BuildChangeoverPlanReporting), and publishes the claims again
// so Core's mirror names it a position of this station.
func (p *pmCell) deckNode() {
	p.t.Helper()
	var err error
	p.deckID, err = p.edge.DB.CreateProcessNode(processes.NodeInput{ProcessID: p.processID, CoreNodeName: ksrDeck,
		Code: "KSR9", Name: ksrDeck, Sequence: 9, Enabled: true})
	mustNil(p.t, err, "the paired position's process node")
	_, err = p.edge.DB.EnsureProcessNodeRuntime(p.deckID)
	mustNil(p.t, err, "the paired position's runtime")
	mustNil(p.t, edgemessaging.NewPlantClaimsPublisher(p.edge.DB, "edge.test", 0).PublishChanged(p.processID),
		"publish the plant claims")
	p.bus.PushBins()
	p.settle()
}

// seatGone forces a process node's seat to say its carrier has gone: no bin
// bound, the departure recorded (carrierGone), as a lift leaves it.
func (p *pmCell) seatGone(nodeID int64) {
	p.t.Helper()
	mustNil(p.t, p.edge.DB.SetProcessNodeActiveBinID(nodeID, nil), "the seat holds no bin")
	mustNil(p.t, p.edge.DB.SetProcessNodeRuntimeLinesidePayload(nodeID, "", false, string(domain.CarrierDeparted)),
		"the seat's carrier departed")
}

// pmChangeoverIntoVacant: a changeover into a press whose positions are
// vacant: the line's bin taken off and the paired position empty at Core,
// each seat recording its carrier gone. The outgoing style is the row's mode
// (a press, or across modes any other); the incoming is a press of B. START
// plans each vacant position an Add (ApplyNodeState: a vacant Swap is an Add;
// FanOutPressIndexCrossMode plans the to-only paired position a Swap first),
// so its orders are plain deliveries of B, one to each position, and no leg;
// the changeover runs to CUTOVER with a B bin on each.
func pmChangeoverIntoVacant(p *pmCell) {
	p.deckNode()
	p.takeLineBinOff()
	p.bus.PushBins()
	p.settle()
	p.seatGone(p.nodeID)
	p.seatGone(p.deckID)

	label := "START into a vacant press"
	before := p.lastID()
	if _, err := p.edge.Engine.StartProcessChangeover(p.processID, p.styleB, "ksr", ""); err != nil {
		p.dump(label)
		p.t.Fatalf("%s refused: %v", label, err)
	}
	p.tick()
	m := p.read(before)
	if len(m.legs) != 0 || len(m.toLine) != 1 || len(m.primes) != 1 || len(m.clears)+len(m.other) != 0 {
		p.dump(label)
		p.t.Fatalf("%s: legs %d, to the line %d, to the paired position %d, other %d; want an Add to each "+
			"position and no leg", label, len(m.legs), len(m.toLine), len(m.primes), len(m.clears)+len(m.other))
	}
	for _, o := range append(m.toLine, m.primes...) {
		if o.PayloadCode != ksrPartB {
			p.t.Errorf("%s: the delivery %d to %s is for %q, want B's part %s", label, o.ID, o.DeliveryNode,
				o.PayloadCode, ksrPartB)
		}
	}
	p.runChangeover(label, before, ksrLine, ksrDeck)
	p.settled("the end")
}

// pmUnchanged: style B claims the press exactly as A does (sameB), and the
// paired position is vacant. START plans the press Unchanged and makes no
// order at it; a tap while the changeover is armed is refused as every
// request against an armed changeover is (guardStyleTransition); CUTOVER;
// then one tap behaves as case 1, on B's claim.
func pmUnchanged(p *pmCell) {
	label := "START, the press Unchanged"
	before := p.lastID()
	if _, err := p.edge.Engine.StartProcessChangeover(p.processID, p.styleB, "ksr", ""); err != nil {
		p.dump(label)
		p.t.Fatalf("%s refused: %v", label, err)
	}
	p.tick()
	if made := p.newRows(before); len(made) != 0 {
		p.dump(label)
		p.t.Fatalf("%s: %d orders made, want none: the press's claim does not change", label, len(made))
	}
	co, err := p.edge.DB.GetActiveProcessChangeover(p.processID)
	mustNil(p.t, err, "the active changeover")
	tasks, err := p.edge.DB.ListChangeoverNodeTasks(co.ID)
	mustNil(p.t, err, "the changeover's node tasks")
	situation := ""
	for _, task := range tasks {
		if task.ProcessNodeID == p.nodeID {
			situation = task.Situation
		}
	}
	if situation != string(edgeengine.SituationUnchanged) {
		p.t.Errorf("%s: the line's task is %q, want %q", label, situation, edgeengine.SituationUnchanged)
	}

	_, err = p.tap()
	var armed *edgeengine.ChangeoverArmedError
	if !errors.As(err, &armed) {
		p.dump(label)
		p.t.Fatalf("a tap with the changeover armed: err=%v, want the changeover-armed refusal", err)
	}
	if made := p.newRows(before); len(made) != 0 {
		p.t.Fatalf("a tap with the changeover armed made %d orders, want none", len(made))
	}

	p.runChangeover(label, before)
	claimB, err := p.edge.DB.GetStyleNodeClaimByNode(p.styleB, ksrLine)
	mustNil(p.t, err, "B's claim at the line")
	mustNil(p.t, p.edge.DB.SetProcessNodeRuntime(p.nodeID, &claimB.ID, 30), "B's claim, parts counted")
	p.settle()
	p.vacantDeckSwap("one tap after CUTOVER, the paired position vacant")
	p.settled("the end")
}
