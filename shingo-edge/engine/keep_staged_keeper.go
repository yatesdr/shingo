package engine

import (
	"fmt"
	"slices"
	"strings"
	_ "sync" // the plain lock inlines only where sync is imported (protocol/lockcheck)
	"time"

	"shingo/protocol"
	"shingo/protocol/lockcheck"
	"shingoedge/domain"
	"shingoedge/internal/lockorder"
	ordermgr "shingoedge/orders"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// keep_staged_keeper.go — the keeper of a keep-staged spot.
//
// ONE DECISION, ONE OWNER. What a spot needs is decided by decideSpot
// (keep_staged_spot.go), and only the keeper asks it: the level sweep, for every
// keep-staged line, idle ones included, so a spot is topped up without anyone
// pressing anything; and the end of any order of the line, a pickup at the spot,
// or the spot's row changing in the bins copy, at once instead of at the next
// sweep; a pass the cell's held lock turns away runs once the cell is free,
// unless a changeover start or cancel deciding the spot drops it (keepSpot). A
// REQUEST does not: it asks only for the line, taking the spare when one stands
// and the market's carrier when none does (requestClaim), so cancelling what a
// REQUEST made leaves nothing of the spot's behind (a plant's line, 2026-10-08:
// a refill created with each request outlived the request's cancel and took the
// carrier it freed).
//
// It replaced a floor that ran only while a swap waited at Core and assumed that
// swap would lift a spare. A swap planned before the spot was kept never does:
// it stages its own carrier ON the spot. At that line (2026-10-08) the floor
// sent the bin standing there back to a source the swap's own carrier held and
// queued two refills behind both, and re-created all three on each of five
// cancels. The keeper reads the legs' steps instead of assuming them, orders
// nothing while a leg still drops onto the spot, and stops on a cancel.
//
// COST. One indexed read of the line's orders (ListKeptSpotRows) and, unless the
// rows alone settle it (paused, or a leg dropping onto the spot), the spot's row
// from the bins table (memory), per keep-staged line per sweep.

// keepSpot runs the keeper for one keep-staged line; trigger names what asked
// (keeperSweep, keeperOrderEnd, keeperLift, keeperBinsCopy, keeperResume).
//
// TRY, NEVER WAIT, HERE. A held lock is another door deciding this cell now (a
// request, a changeover start or cancel, a save, among others), and its
// decision may not cover the spot: a REQUEST orders nothing for it (above), and
// a holder that read the bins copy before its change decided from the old one.
// The order-end kick runs inside the completion cascade, which events deliver
// synchronously: a changeover start or cancel aborting under the lock reaches
// here on its own goroutine, and the bins copy's wake arrives on the one inbound
// goroutine. So a pass that meets a held lock waits for nothing: it is marked
// for the line (markKeeper), and the keeper's worker runs it once the cell is
// free, from the rows as they stand then (drainKeeperMarks). A changeover start
// or cancel drops, as it lets go, the marks on the spots its own decision covers
// (letGoDecided); for those the next decision point re-asks, at the latest the
// next level sweep (demandReconcileInterval, 60 s), as it did for every pass a
// held lock turned away until 2026-11-25 (docket B6). Pinned by
// TestKeepStagedOneOwner_TheCopyCatchingUpIsADecisionPoint and
// TestKeepStagedOneOwner_AChangeoverDropsOnlyThePassesOnItsSpots.
//
// Every run records the line as keeping the spot (keptSpotLines), so the spot's
// row changing in the bins copy runs it again (keepSpotsOnBinsChange).
func (e *Engine) keepSpot(node *processes.Node, claim *processes.NodeClaim, trigger string) {
	spot := spotNode(claim)
	if node == nil || spot == "" {
		return
	}
	line := keptSpotLine{spot: spot, nodeID: node.ID}
	e.keptSpotLines.Store(line, struct{}{})
	mu := e.primeNodeLock(claim)
	if !mu.TryLock() {
		turnedAway := e.markKeeper(line, claim.CoreNodeName, trigger)
		e.logFn("keep-staged: node %s: spot %s: the cell is being decided (its lock is held) — the pass %s asked for "+
			"is marked for when the cell is free (%d turned away since its last run)",
			node.Name, spot, trigger, turnedAway)
		return
	}
	defer mu.Unlock()
	e.keepSpotLocked(node, claim)
}

// keepSpotLocked is the keeper's pass for one line, run under the cell's lock
// (keepSpot, runMarked): it decides from the rows it reads now.
func (e *Engine) keepSpotLocked(node *processes.Node, claim *processes.NodeClaim) {
	spot := spotNode(claim)
	rows, err := storeorders.ListKeptSpotRows(e.db.DB, node.ID, spot, claim.CoreNodeName)
	if err != nil {
		e.logFn("keep-staged: node %s: read the line's orders: %v — the next pass re-asks", node.Name, err)
		return
	}
	resumed, rerr := processes.KeepStagedResumedAt(e.db.DB, node.ID)
	if rerr != nil {
		e.logFn("keep-staged: node %s: read the resume stamp: %v — the next pass re-asks", node.Name, rerr)
		return
	}
	f := e.spotFactsOf(claim, spotRead{known: true}, rows, resumed)
	if f.paused != "" || f.drops {
		_, note := decideSpot(claim, f)
		e.setKeepStagedNote(claim.CoreNodeName, note)
		return
	}
	// The spot from the bins table (site 3 of the reads), unattended: a table
	// that may not be decided from, or a spot it holds no row for, skips this
	// pass, logged; a spot Core does not know is decided on no more than a
	// failed read was.
	row, err := e.binsRowAt(spot)
	if err != nil {
		e.logFn("keep-staged: node %s: the spot %s: the bins table cannot answer (%s) — the next pass re-asks",
			node.Name, spot, binsWhy(err))
		return
	}
	read := readOfBinsRow(row, e.PayloadBinTypes())
	if !read.known {
		return
	}
	f = e.spotFactsOf(claim, read, rows, resumed)
	plan, note := decideSpot(claim, f)
	e.setKeepStagedNote(claim.CoreNodeName, note)
	e.applySpotPlan(node, claim, plan, liftingOrigin(rows, claim))
}

// What asked for a keeper pass (keepSpot's trigger), as a turned-away pass's
// mark and the worker's log line name it.
const (
	keeperSweep    = "the level sweep"
	keeperOrderEnd = "an order's end"
	keeperLift     = "a lift at the spot"
	keeperBinsCopy = "the bins copy"
	keeperResume   = "RESUME"
)

// keeperQueue is the keeper passes a held cell lock turned away, one mark per
// line, oldest first, and the wake of the worker that runs them
// (startKeeperWorker).
type keeperQueue struct {
	mu      lockcheck.Mutex[lockorder.KeeperMarks]
	marks   []keeperMark
	running bool          // a worker goroutine drains it (startKeeperWorker)
	signal  chan struct{} // the worker's wake, buffered 1
}

// keeperMark is one line's turned-away passes since its last run: the line, the
// cell whose held lock turned them away (the line's core node, the key of
// primeNodeLock), how many, and which triggers asked, in the order they first
// did.
type keeperMark struct {
	line     keptSpotLine
	cell     string
	count    int
	triggers []string
}

// markKeeper records a pass on line that trigger asked for and the held lock of
// cell turned away, merged into the line's one mark, wakes the worker when one
// runs, and returns the line's turned-away count since its last run. It waits
// for nothing: it is called from inside the triggers' own sections.
func (e *Engine) markKeeper(line keptSpotLine, cell, trigger string) int {
	q := &e.keeper
	q.mu.Lock()
	i := slices.IndexFunc(q.marks, func(m keeperMark) bool { return m.line == line })
	if i < 0 {
		q.marks = append(q.marks, keeperMark{line: line, cell: cell})
		i = len(q.marks) - 1
	}
	m := &q.marks[i]
	m.count++
	if !slices.Contains(m.triggers, trigger) {
		m.triggers = append(m.triggers, trigger)
	}
	n, running, signal := m.count, q.running, q.signal
	q.mu.Unlock()
	if running {
		select {
		case signal <- struct{}{}:
		default:
		}
	}
	return n
}

// oldestCell is the cell of the oldest mark; ok is false when none is left.
func (q *keeperQueue) oldestCell() (cell string, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.marks) == 0 {
		return "", false
	}
	return q.marks[0].cell, true
}

// takeWhere removes and returns, oldest first, every mark hit reports true for.
func (q *keeperQueue) takeWhere(hit func(m keeperMark) bool) []keeperMark {
	q.mu.Lock()
	defer q.mu.Unlock()
	var taken, kept []keeperMark
	for _, m := range q.marks {
		if hit(m) {
			taken = append(taken, m)
		} else {
			kept = append(kept, m)
		}
	}
	q.marks = kept
	return taken
}

// drainKeeperMarks runs every marked keeper pass, the oldest mark's cell first.
// It takes the cell's lock with a wait and only then the cell's marks, so a
// mark a changeover start or cancel holding the cell drops as it lets go
// (letGoDecided) is never one it runs; for each it reads the line again and
// runs the same pass as keepSpot (runMarked), deciding from the rows as they
// stand once the cell is free. A pass turned away while this runs is marked
// again and run in turn. It is the worker's whole job and the door a test
// calls; no clock paces it. On a stopping engine it leaves what is left: the
// marks are memory, and the next start's first sweep runs every kept line.
// Pinned by TestKeepStagedOneOwner_TheCopyCatchingUpIsADecisionPoint and
// TestKeepStagedOneOwner_AChangeoverDropsOnlyThePassesOnItsSpots.
func (e *Engine) drainKeeperMarks() {
	for !e.stopping() {
		cell, ok := e.keeper.oldestCell()
		if !ok {
			return
		}
		mu := e.primeNodeLock(&processes.NodeClaim{CoreNodeName: cell})
		mu.Lock()
		for _, m := range e.keeper.takeWhere(func(m keeperMark) bool { return m.cell == cell }) {
			e.runMarked(m)
		}
		mu.Unlock()
	}
}

// runMarked runs one marked pass under its cell's lock (drainKeeperMarks), for
// the claim the line keeps the spot for now (lineKeeping). A line that no
// longer keeps the spot, or keeps it from another cell, is forgotten: its next
// pass is its next decision point's.
func (e *Engine) runMarked(m keeperMark) {
	node, claim := e.lineKeeping(m.line.nodeID, m.line.spot)
	if claim == nil || claim.CoreNodeName != m.cell {
		e.keptSpotLines.Delete(m.line)
		return
	}
	e.logFn("keep-staged: node %s: spot %s: the cell is free — the keeper runs the pass %s asked for (%d turned away)",
		node.Name, m.line.spot, strings.Join(m.triggers, ", "), m.count)
	e.keepSpotLocked(node, claim)
}

// letGoDecided is a changeover start's or cancel's let-go of the cells it held
// (what names it, for the log): first it drops the keeper's marks on the spots
// its own decision covers, each spot with the lines that keep or kept it there
// (spotChange's keeper and leaver), then it releases the cells' locks (unlock).
// The drop comes first, so the worker, which takes a cell's marks only holding
// the cell's lock (drainKeeperMarks), never runs one of them. A mark on any
// other spot of a cell it held stays and is run. Docket B6, R77: run as the
// cancel let go, the passes its own aborts turned away ordered refills the
// moment it returned, ahead of the landing of its flown return
// (TestScenario_KeepStagedCell_CallChangeoverCancel, red so on 2026-11-24); a
// start's would order, as it returns, the refill it leaves to the keeper.
// Pinned by TestKeepStagedOneOwner_AChangeoverDropsOnlyThePassesOnItsSpots.
func (e *Engine) letGoDecided(what string, processID int64, spots []spotChange, unlock func()) {
	covered := map[keeperCellSpot]bool{}
	for _, ch := range spots {
		for _, c := range []*processes.NodeClaim{ch.keeper, ch.leaver} {
			if c != nil {
				covered[keeperCellSpot{cell: c.CoreNodeName, spot: ch.spot}] = true
			}
		}
	}
	dropped := 0
	if len(covered) > 0 {
		for _, m := range e.keeper.takeWhere(func(m keeperMark) bool {
			return covered[keeperCellSpot{cell: m.cell, spot: m.line.spot}]
		}) {
			dropped += m.count
		}
	}
	if dropped > 0 {
		e.logFn("keep-staged: process %d's changeover %s: %d keeper pass(es) turned away on the spots it decided are "+
			"dropped — the keeper's next decision point asks again", processID, what, dropped)
	}
	unlock()
}

// keeperCellSpot is a spot kept from a cell (letGoDecided).
type keeperCellSpot struct{ cell, spot string }

// startKeeperWorker runs the keeper's worker on the engine's group: it drains
// the marks (drainKeeperMarks) each time one is made, and once at its start
// for any made before it. It leaves on the stop channel, and Stop waits for it
// as for every goroutine on the group (TestEngineStop_WaitsForTheLoopsStartLaunched).
func (e *Engine) startKeeperWorker() {
	q := &e.keeper
	q.mu.Lock()
	q.running, q.signal = true, make(chan struct{}, 1)
	q.signal <- struct{}{}
	q.mu.Unlock()
	e.loops.spawn(e.runKeeperWorker)
}

// runKeeperWorker is the keeper's worker: one drain of the marked passes per
// wake, until the engine stops (startKeeperWorker).
func (e *Engine) runKeeperWorker() {
	q := &e.keeper
	defer func() {
		q.mu.Lock()
		q.running = false
		q.mu.Unlock()
	}()
	for {
		select {
		case <-e.stopChan:
			return
		case <-q.signal:
			e.drainKeeperMarks()
		}
	}
}

// spotFactsOf reads the facts decideSpot decides from: the spot read with a
// spare already going taken off it, and from the line's rows what is coming,
// whether a leg still drops onto it, and the pause.
// resumed is the line's board RESUME stamp (nil when never), one of the things
// that re-arms the pause.
func (e *Engine) spotFactsOf(claim *processes.NodeClaim, read spotRead, rows []domain.Order, resumed *time.Time) spotFacts {
	spot := spotNode(claim)
	f := spotFacts{
		read:   read.lessLeaving(spotLeaving(rows, spot, claim.CoreNodeName)),
		coming: spotComing(rows, claim, read.inbound),
		paused: spotPause(rows, claim, resumed),
	}
	f.drops = e.legDropsOnSpot(rows, claim)
	return f
}

// legDropsOnSpot reports whether a live leg of the line, keep-staged refills and
// returns excepted, will still set a carrier down on the spot: a swap built
// without the spare stages there when the spot is its inbound staging. A plain
// move off the spot is a lift, not a drop. An order whose steps cannot be read
// is judged by its delivery node alone.
func (e *Engine) legDropsOnSpot(rows []domain.Order, claim *processes.NodeClaim) bool {
	spot := spotNode(claim)
	for i := range rows {
		o := &rows[i]
		if ordermgr.IsTerminal(o.Status) || isSpotRefill(o, claim) || isSpotReturn(o, claim) {
			continue
		}
		if o.OrderType == protocol.OrderTypeMove && o.SourceNode == spot {
			continue
		}
		steps, err := e.storedStepsOf(o.ID)
		if err != nil || len(steps) == 0 {
			if o.DeliveryNode == spot {
				return true
			}
			continue
		}
		if drops, _ := spotSteps(steps, spot); drops {
			return true
		}
	}
	return false
}

// isSpotReturn reports whether a line's order is a keep-staged return: a plain
// move off the claim's spot to anywhere but the line.
func isSpotReturn(o *domain.Order, c *processes.NodeClaim) bool {
	spot := spotNode(c)
	return spot != "" && o.OrderType == protocol.OrderTypeMove && o.SourceNode == spot &&
		o.DeliveryNode != c.CoreNodeName
}

// spotPause is the keeper's stop, decided from the rows: the sentence when the
// line's newest keep-staged order (a refill or a return) ended cancelled,
// failed or skipped and nothing has re-armed it since, blank otherwise.
//
// ANY CANCEL STOPS IT. Core fails a plain order for a structural reason
// (congestion waits), so re-creating it each sweep would be a failure on a
// timer, and a cancel is someone saying stop; the Edge does not record who
// cancelled, and does not need to. What re-arms it: an order of the line created
// after the end, a save of the claim, or the line's resume stamp (resumed),
// which the board's RESUME, a REQUEST and a changeover START all write. A START
// writes it because its own orders cannot be relied on to: they are created in
// the second its aborts were written, and read as neither before nor after
// (rearmKeptLines). The pause itself is not stored; it survives a restart
// because the rows do.
//
// A RESUME IN THE CANCEL'S OWN SECOND RE-ARMS; NOTHING ELSE DOES. The Edge
// stamps its rows to the second, so a stamp written in the second the refill
// was cancelled reads as neither before nor after it. The resume stamp is only
// ever a person saying go, so the tie goes to it. An order or a claim save in
// that second could equally have come first, so it has to be strictly later.
func spotPause(rows []domain.Order, c *processes.NodeClaim, resumed *time.Time) string {
	var last *domain.Order
	var rearmed time.Time
	for i := range rows {
		o := &rows[i]
		if (isRetrieve(o.OrderType) && o.DeliveryNode == spotNode(c)) || isSpotReturn(o, c) {
			if last == nil || o.ID > last.ID {
				last = o
			}
			continue
		}
		if o.CreatedAt.After(rearmed) {
			rearmed = o.CreatedAt
		}
	}
	if last == nil {
		return ""
	}
	switch last.Status {
	case ordermgr.StatusCancelled, ordermgr.StatusFailed, ordermgr.StatusSkipped:
	default:
		return ""
	}
	if c.UpdatedAt != nil && c.UpdatedAt.After(rearmed) {
		rearmed = *c.UpdatedAt
	}
	if rearmed.After(last.UpdatedAt) || (resumed != nil && !resumed.Before(last.UpdatedAt)) {
		return ""
	}
	what := "refill to"
	if isSpotReturn(last, c) {
		what = "return from"
	}
	return fmt.Sprintf("Keep-staged is paused: the last %s %s was %s. Tap to resume.",
		what, spotNode(c), last.Status)
}

// ResumeKeepStaged is the board's RESUME on a line whose keep-staged keeper a
// cancel paused: it stamps the line's resume and runs the keeper at once, so the
// spot is refilled without a REQUEST and without anything sent to the line. A
// tap that meets the cell being decided marks the pass, and the keeper's worker
// runs it once the cell is free (keepSpot); a changeover start or cancel
// deciding the spot drops it as it lets go, and the stamp stands for the
// keeper's next pass. Until 2026-11-25 such a tap said it worked and did nothing
// (docket B6).
func (e *Engine) ResumeKeepStaged(nodeID int64) error {
	node, err := e.db.GetProcessNode(nodeID)
	if err != nil || node == nil {
		return fmt.Errorf("node %d: not found", nodeID)
	}
	claim := e.claimAtNode(node)
	if spotNode(claim) == "" {
		return fmt.Errorf("node %s does not keep a spare staged", node.Name)
	}
	if err := processes.ResumeKeepStaged(e.db.DB, node.ID); err != nil {
		return err
	}
	e.logFn("keep-staged: node %s: resumed from the board", node.Name)
	e.keepSpot(node, claim, keeperResume)
	return nil
}

// liftingOrigin is the demand a refill serves: the episode of a live leg of the
// line, when there is one, else none.
func liftingOrigin(rows []domain.Order, c *processes.NodeClaim) ordermgr.Origin {
	for i := range rows {
		o := &rows[i]
		if !ordermgr.IsTerminal(o.Status) && o.OriginID != "" && !isSpotRefill(o, c) && !isSpotReturn(o, c) {
			return ordermgr.Attached(o.OriginID)
		}
	}
	return ordermgr.NoDemand()
}

// setKeepStagedNote records the sentence the board shows for a keep-staged
// line, blank to clear it, and nudges the boards when the sentence changed
// (the keeper sets it on every sweep; only a change is news to a board).
func (e *Engine) setKeepStagedNote(line, note string) {
	var prev any
	if note == "" {
		prev, _ = e.keepStagedNotes.LoadAndDelete(line)
	} else {
		prev, _ = e.keepStagedNotes.Swap(line, note)
	}
	if was, _ := prev.(string); was != note {
		e.nudgeBoards()
	}
}

// KeepStagedNote is the board's sentence for a keep-staged line (the station
// view's keep_staged_note), blank when there is nothing to say.
func (e *Engine) KeepStagedNote(line string) string {
	if v, ok := e.keepStagedNotes.Load(line); ok {
		return v.(string)
	}
	return ""
}

// keptFor is the claim a line's keep-staged spot is kept for: from a
// changeover's START to its cutover, the incoming style's claim on the line (nil
// when the incoming style does not claim it, and the start sent its spare
// back); the active claim otherwise. targetStyleID is the changeover's incoming
// style, 0 when none is armed; incoming reads that style's claim on the line.
//
// ONE OWNER THROUGH A CHANGEOVER. The keeper is the only thing that refills a
// spot, and it keeps doing so while a changeover runs, for the claim the line
// is changing to. Its own guards cover the window: a leg still bringing a
// carrier to the spot holds it (legDropsOnSpot), nothing is ordered ahead of a
// lift, and it only tries the cell's lock, which a start or a cancel holds while
// it decides, and a pass that lock turns away is theirs to drop as they let go
// (letGoDecided). The changeover row exists only from START, so a changeover
// that is armed has started.
func keptFor(active *processes.NodeClaim, targetStyleID int64, incoming func(styleID int64) *processes.NodeClaim) *processes.NodeClaim {
	if targetStyleID == 0 {
		return active
	}
	return incoming(targetStyleID)
}

// SpotClaim is keptFor for the line an order completion names.
func (c *orderCompletionCtx) SpotClaim() *processes.NodeClaim {
	return keptFor(c.Claim(), c.toStyleID, func(int64) *processes.NodeClaim { return c.ToClaim() })
}

// kickKeepSpot runs the keeper at once when an order of a keep-staged line ends,
// instead of at the next sweep. A refill a changeover cancel left flying, landing
// on a spot no longer kept for it, goes back first (returnUnwantedLanding).
func (e *Engine) kickKeepSpot(ctx *orderCompletionCtx) {
	o := ctx.order
	if isRetrieve(o.OrderType) && o.DeliveryNode != "" && o.DeliveryNode != ctx.node.CoreNodeName &&
		o.Status == ordermgr.StatusConfirmed {
		e.returnUnwantedLanding(ctx)
	}
	claim := ctx.SpotClaim()
	if claim == nil || spotNode(claim) == "" {
		return
	}
	e.keepSpot(ctx.node, claim, keeperOrderEnd)
}

// keepSpotOnPickup runs the keeper when Core reports a lift at a line's spot, so
// the refill behind a lifted spare is ordered at the lift rather than at the
// swap's end. Core moves the bin off the spot before it reports the pickup, but
// the keeper reads the bins copy, which Core's push updates on its own path: a
// copy still showing the spare decides nothing now, and the copy's change runs
// the keeper again (keepSpotsOnBinsChange).
func (e *Engine) keepSpotOnPickup(order *domain.Order, location string) {
	if order == nil || order.ProcessNodeID == nil || location == "" {
		return
	}
	if node, claim := e.lineKeeping(*order.ProcessNodeID, location); claim != nil {
		e.keepSpot(node, claim, keeperLift)
	}
}

// keptSpotLine is a line the keeper has run for, and the spot it kept
// (keptSpotLines).
type keptSpotLine struct {
	spot   string
	nodeID int64
}

// keepSpotsOnBinsChange runs the keeper again for every line it has run for
// whose spot's row in the bins copy just changed (EventNodeBinsUpdated).
//
// THE COPY CATCHES UP AFTER THE ORDER MESSAGE. The keeper decides from the bins
// copy, and Core's push of a bin's move travels apart from the message that ends
// or lifts the order that moved it, so a kick can read the copy a moment before
// the push lands. Deciding from the bin that has just left, it orders nothing:
// the spare a return took reads as a wrong bin standing, the spare a leg lifted
// as the right one. With nothing to run the keeper when the push landed, the
// spot waited for the next sweep, a minute: in CI run 42888523152 (2026-11-10,
// integration KSR produce/two_robot_press_index/e) the changeover's B fetch
// never came, and holding the scenario's push back until the messages are
// delivered failed 18 of its 56 subtests, every sequential and press moment
// that waits for B after the spare's return. Do not drop this wake while the
// keeper reads the copy and the copy travels apart from the order messages.
// Pinned by TestKeepStagedOneOwner_TheCopyCatchingUpIsADecisionPoint.
//
// Only lines the keeper has run for since start: a spot it has not decided
// since has no decision to correct, and the first sweep runs every kept line. A
// line that no longer keeps the spot (lineKeeping) is forgotten.
func (e *Engine) keepSpotsOnBinsChange(nodes []string) {
	changed := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		changed[n] = true
	}
	e.keptSpotLines.Range(func(k, _ any) bool {
		l := k.(keptSpotLine)
		if !changed[l.spot] {
			return true
		}
		node, claim := e.lineKeeping(l.nodeID, l.spot)
		if claim == nil {
			e.keptSpotLines.Delete(k)
			return true
		}
		e.logFn("keep-staged: node %s: spot %s changed in the bins copy — the keeper decides it again", node.Name, l.spot)
		e.keepSpot(node, claim, keeperBinsCopy)
		return true
	})
}

// lineKeeping is the process node nodeID and the claim it keeps spot for now
// (spotClaimAt); nil when the line cannot be read or keeps no spare there.
func (e *Engine) lineKeeping(nodeID int64, spot string) (*processes.Node, *processes.NodeClaim) {
	node, err := e.db.GetProcessNode(nodeID)
	if err != nil || node == nil {
		return nil, nil
	}
	claim := e.spotClaimAt(node)
	if claim == nil || spotNode(claim) != spot {
		return nil, nil
	}
	return node, claim
}

// spotClaimAt is keptFor for a line read on its own: the process's armed target
// and, when there is one, the target style's claim on the line. A read that
// fails answers nil, and the keeper does nothing until the next pass.
func (e *Engine) spotClaimAt(node *processes.Node) *processes.NodeClaim {
	p, err := e.db.GetProcess(node.ProcessID)
	if err != nil || p == nil {
		return nil
	}
	var target int64
	if p.TargetStyleID != nil {
		target = *p.TargetStyleID
	}
	return keptFor(e.claimAtNode(node), target, func(styleID int64) *processes.NodeClaim {
		c, cerr := e.db.GetStyleNodeClaimByNode(styleID, node.CoreNodeName)
		if cerr != nil {
			return nil
		}
		return c
	})
}

// returnUnwantedLanding sends a refill's bin straight back when it lands on a
// spot nobody keeps it for.
//
// THE LINE'S OWN BIN, NOT A JUDGEMENT ABOUT A STRANGER. It acts only on a refill
// this line ordered, judged by what the Edge ordered and not by a read; a bin
// that reached the spot any other way is left where it stands (decideSpot). The
// keeper orders refills only for the claim the spot is kept for (keptFor), so in
// practice this is a keeper refill for a changeover's incoming claim, ordered
// behind a leg's lift, that outlived the changeover's cancel.
//
// A refill already with the fleet cannot be cancelled, so it lands, possibly
// after the last decision about its spot: a changeover cancelled while the
// incoming style's refill was flying leaves that style's bin on a spot the
// staying style keeps. A decision point reads the spot only when one runs, and
// a swap waiting at Core for its spare does not run one; Edge does know, with
// no read, what it ordered. It judges the landing by the refill's part and
// role, not by the carrier: it has no read of the bin to judge a carrier by.
//
// "Wanted" is judged against the claim the spot is kept for now (keptFor). The
// bin is wanted when that claim keeps this spot for the refill's part and role;
// a refill for the incoming style landing before cutover stays. Otherwise it
// goes back by a plain move to the refill's own source, carrying the refill's
// part, an empty untagged. Only on a keep-staged spot of this line: a plain
// retrieve to any other node is not this code's to undo.
//
// One return per bin: none is made while a live return from the spot exists,
// and the request, floor and changeover reconciles count a live return as the
// spare leaving. That check reads the line's live rows once, only when a landing
// is unwanted; the cascade has already read the order, the node and the claim.
//
// Under the cell's prime lock, taken with a wait: a refill lands from Core's
// confirm on the messaging goroutine, never inside a section that holds a
// cell's lock. Those abort orders and do not confirm them, and a changeover
// cancel's aborts do not reach the changeover's completion, which confirms
// (handleOrphanedTaskOrderCompleted leaves the cancel's tasks alone).
func (e *Engine) returnUnwantedLanding(ctx *orderCompletionCtx) {
	o := ctx.order
	keeper := ctx.SpotClaim()
	if landingWanted(keeper, o) {
		return
	}
	active := ctx.Claim()
	if !keptSpot(keeper, o.DeliveryNode) && !keptSpot(active, o.DeliveryNode) {
		return
	}
	mu := e.primeNodeLock(&processes.NodeClaim{CoreNodeName: ctx.node.CoreNodeName})
	mu.Lock()
	defer mu.Unlock()
	rows, err := e.db.ListActiveOrdersByProcessNode(ctx.node.ID)
	if err != nil {
		e.logFn("keep-staged: node %s: read the line's orders before returning a landed refill: %v", ctx.node.Name, err)
		return
	}
	if spotLeaving(rows, o.DeliveryNode, ctx.node.CoreNodeName) > 0 {
		return // already going back
	}
	carried := o.PayloadCode
	if o.RetrieveEmpty {
		carried = ""
	}
	// The bin the refill delivered, when Core told the station which one.
	var binID int64
	if o.BinID != nil {
		binID = *o.BinID
	}
	e.logFn("keep-staged: node %s: refill %d landed %s on %s, which is not kept for it — sent back to %s",
		ctx.node.Name, o.ID, o.PayloadCode, o.DeliveryNode, o.SourceNode)
	e.returnSpare(ctx.node, o.DeliveryNode, o.SourceNode, carried, binID, ordermgr.NoDemand())
}

// landingWanted reports whether claim keeps the refill's spot for its part and
// role.
func landingWanted(c *processes.NodeClaim, o *domain.Order) bool {
	return keptSpot(c, o.DeliveryNode) && o.PayloadCode == c.PayloadCode && o.RetrieveEmpty == carriesEmpty(c)
}

// keptSpot reports whether claim keeps a spare on spot.
func keptSpot(c *processes.NodeClaim, spot string) bool {
	return spot != "" && spotNode(c) == spot
}
