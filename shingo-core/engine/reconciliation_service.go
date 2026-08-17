package engine

import (
	"fmt"
	"strings"
	"time"

	"shingo/protocol"
	"shingo/shared/clock"
	"shingocore/dispatch"
	"shingocore/service"
	"shingocore/store/messaging"
	"shingocore/store/orders"
	"shingocore/store/reconciliation"
	"shingocore/store/recovery"
)

// ReconciliationService runs the periodic reconciliation loop plus
// auto-confirm-stuck-delivered logic. db is declared as the
// ReconciliationStore interface (see reconciliation_store.go); *store.DB
// satisfies it structurally so engine wiring is unchanged.
//
// confirmDelivered is a late-bound callback the engine wires up after
// the dispatcher is constructed (engine.New → engine.Start ordering).
// AutoConfirmStuckDeliveredOrders calls it once per stuck order; the
// production binding routes through dispatch.LifecycleService.ConfirmReceipt
// so the (Delivered → Confirmed) actionMap entry fires fireCompleted →
// EmitOrderCompleted. The old direct-DB path bypassed that emit, which
// left Edge stranded at delivered.
type ReconciliationService struct {
	db               ReconciliationStore
	logFn            LogFunc
	confirmDelivered func(order *orders.Order) error
	// abandonOrder cancels a stuck order (and cascades to its two-robot
	// sibling). Late-bound to dispatch.LifecycleService.CancelOrder in
	// engine.New, same wiring rationale as confirmDelivered above.
	abandonOrder func(order *orders.Order, reason string) error
	// advanceCompound re-drives a compound (reshuffle) parent whose children
	// are all terminal. Late-bound to dispatch.Dispatcher.AdvanceCompoundOrder
	// in engine.New. The liveness backstop for reshuffle parents stranded in
	// `reshuffling` when a child→parent terminal event was missed (crash) or
	// never fired (the cancelled-child vector has no child→parent event arm).
	advanceCompound func(parentID int64) error
	// burialTally reads the burial shadow instrument's since-boot counts
	// (service/burial_shadow.go). Late-bound like the callbacks above, and for
	// the same wiring reason: BinService is constructed after this service.
	//
	// It rides THIS loop rather than getting a timer of its own because the
	// numbers are a soak reading, not an alert — one periodic line beside the
	// other tallies is exactly the cadence a week of data wants, and a second
	// loop would be a second thing to reason about for a measurement that is
	// meant to be deleted once it has answered.
	burialTally func() service.BurialTally
	// lastFolderShadow is the previous folder-recognition reading, so the sweep
	// only speaks when the number has MOVED. See logFolderShadow.
	lastFolderShadow string
}

func newReconciliationService(db ReconciliationStore, logFn LogFunc) *ReconciliationService {
	return &ReconciliationService{db: db, logFn: logFn}
}

func (s *ReconciliationService) Summary() (*reconciliation.Summary, error) {
	return s.db.GetReconciliationSummary()
}

func (s *ReconciliationService) ListAnomalies() ([]*reconciliation.Anomaly, error) {
	return s.db.ListReconciliationAnomalies()
}

func (s *ReconciliationService) ListRecoveryActions(limit int) ([]*recovery.Action, error) {
	return s.db.ListRecoveryActions(limit)
}

func (s *ReconciliationService) RequeueOutbox(id int64) error {
	return s.db.RequeueOutbox(id)
}

func (s *ReconciliationService) ListDeadLetterOutbox(limit int) ([]*messaging.OutboxMessage, error) {
	return s.db.ListDeadLetterOutbox(limit)
}

func (s *ReconciliationService) Loop(stopCh <-chan struct{}, interval, autoConfirmTimeout, abandonTimeout, abandonOperatorGatedTimeout time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			summary, err := s.Summary()
			if err != nil {
				s.logFn("engine: reconciliation summary error: %v", err)
				continue
			}
			if summary.Status != "ok" {
				// `expired_staged_bins`, NOT `staged`. This field counts BINS whose
				// staging expired (the staged_bin_expired anomaly); it has nothing to do
				// with orders in status `staged`, which sit two fields to its left under
				// `stuck`. The short label cost a live diagnosis on 2026-08-10: a pure
				// staging deadlock — eight orders in status `staged`, robots parked at
				// marks — was read off this line as `stuck=8 staged=0`, and the 0 was
				// taken to mean no order was staged. It meant no BIN had expired.
				s.logFn("engine: reconciliation status=%s anomalies=%d stuck=%d expired_staged_bins=%d stale_edges=%d outbox=%d dead_letters=%d",
					summary.Status,
					summary.TotalAnomalies,
					summary.StuckOrders,
					summary.ExpiredStagedBins,
					summary.StaleEdges,
					summary.OutboxPending,
					summary.DeadLetters,
				)
			}
			if autoConfirmTimeout > 0 {
				if n, err := s.AutoConfirmStuckDeliveredOrders(autoConfirmTimeout); err != nil {
					s.logFn("engine: auto-confirm delivered error: %v", err)
				} else if n > 0 {
					s.logFn("engine: auto-confirmed %d stuck delivered orders", n)
				}
			}
			if abandonTimeout > 0 {
				if n, err := s.AbandonStuckOrders(abandonTimeout, abandonOperatorGatedTimeout); err != nil {
					s.logFn("engine: abandon stuck orders error: %v", err)
				} else if n > 0 {
					s.logFn("engine: abandoned %d stuck orders", n)
				}
			}
			if n, err := s.AdvanceStuckReshuffleParents(); err != nil {
				s.logFn("engine: advance stuck reshuffle parents error: %v", err)
			} else if n > 0 {
				s.logFn("engine: re-drove %d stuck reshuffle parents", n)
			}
			if n, err := s.db.ReapOrphanedReservations(); err != nil {
				s.logFn("engine: reap orphaned reservations error: %v", err)
			} else if n > 0 {
				s.logFn("engine: reaped %d orphaned reservations from terminal/gone orders", n)
			}
			s.logBurialShadow()
			s.logFolderShadow()
			s.logArrivalRefusals()
			s.logDestNodeDrift()
		}
	}
}

// logBurialShadow reports the burial instrument, and it reports two different
// kinds of thing on one line.
//
// The SOFT count is data: placements that buried a plan, which the design says
// happen and the held-bin path turns into digs. It is silent at zero — a plant
// where nothing ever buries a plan should not spend a line every sweep saying so,
// and no burial-shadow lines in a week of journal is itself an answer.
//
// The BYPASS count is a should-be-zero, and it sits here beside the other
// should-be-zeros for that reason. The burial guard refuses a placement in front
// of a hard claim at the store-slot selector, so a non-zero bypass means a
// placement path reached a lane without consulting it. It is logged even when the
// soft count is zero, and it says so loudly.
func (s *ReconciliationService) logBurialShadow() {
	if s.burialTally == nil {
		return // not wired (tests, or a build without BinService)
	}
	t := s.burialTally()
	if t.Bypass > 0 {
		head, tail, _ := strings.Cut(service.BurialBypassMarker, " ")
		s.logFn("burial-shadow BYPASS=%d (expected 0) — placements buried a hard claim that ALREADY "+
			"EXISTED when the placing order was committed, so the store-slot selector was never "+
			"asked. Find the placement path and route it through nodes.FindStoreSlotInLaneExcluding. "+
			"THIS COUNT is the number, not a grep of it; for the per-event lines search the journal "+
			"for %q followed by %q — split here so this line stays out of its own results.",
			t.Bypass, head, tail)
	}
	// Non-zero and NOT a defect, so it is reported apart and without an
	// expected-zero sentence. Until the PLAN §R.4 split, these were counted as
	// bypasses and carried the "find the placement path" instruction, which was
	// false for most of them: nothing to find, the claim did not exist yet. A
	// should-be-zero that is never zero for reasons nobody can act on stops being
	// read (law 9 from the other direction), and every BYPASS=3..5 reading since
	// §R.4 was this population wearing the wrong sentence.
	if t.Churn > 0 {
		s.logFn("burial-shadow CHURN=%d — approved-then-invalidated: the buried claim arrived AFTER "+
			"the placing order was committed and driving, so no check at any Core moment could have "+
			"seen it. Accepted and healed — the cascade dissolves and re-plans. This is the measured "+
			"price of law 6 (~2.5 min of re-work per occurrence), not a defect, and non-zero is "+
			"expected on a busy plant.", t.Churn)
	}
	if t.Soft == 0 && t.DigUncovered == 0 {
		return
	}
	s.logFn("burial-shadow tally (since boot): soft-hold burials %d (longest held at burial %s), "+
		"dig-uncovered %d",
		t.Soft, t.SoftLongestHeld.Round(time.Second), t.DigUncovered)
}

// logFolderShadow reports the folder-recognition window (§R.96 stage 0d), which
// is what ARMS it: the tally is an in-process map, and until this existed
// FolderShadowReport, FolderShadowTally and FolderShadowSampled had ZERO
// readers anywhere in the tree. An instrument nobody can read is not running,
// it is compiling — and the stage-1 cutover was going to be justified by a
// number that could not be obtained.
//
// IT SPEAKS WHEN THE READING MOVES, not every sweep. Emitting the same three
// numbers every reconciliation for a whole window is how a line stops being
// read (law 9); emitting only on change means the LAST such line in the journal
// is always the current reading, which is exactly what the cutover needs to
// quote.
//
// AND IT SPEAKS AT ZERO SAMPLES, once, deliberately. Three of the seven sites
// have unverified reachability, so "no site sampled" is a finding and not
// silence — it distinguishes an instrument that is running and has seen nothing
// from one that is not running at all, which is the distinction a check that
// cannot tell whether it had input never makes.
func (s *ReconciliationService) logFolderShadow() {
	line := service.FolderShadowLine()
	if line == s.lastFolderShadow {
		return
	}
	s.lastFolderShadow = line

	total := 0
	for _, n := range service.FolderShadowTally() {
		total += n
	}
	if total == 0 {
		s.logFn("folder-shadow window (coordinator/ordinary/sampled per site): %s — no false positive "+
			"yet. Zero here does NOT clear a site: read the sampled count beside it, because a site "+
			"nothing reached and a site that never fired wrongly produce the same zero.", line)
		return
	}
	head, tail, _ := strings.Cut(service.FolderShadowMarker, " ")
	s.logFn("folder-shadow FALSE POSITIVES=%d (coordinator/ordinary/sampled per site): %s — these "+
		"firings landed on orders that own legs, whose NULL bin_id is permanent and correct. THIS "+
		"COUNT is the number the cutover quotes, not a grep of it; for the per-event lines search the "+
		"journal for %q followed by %q. The ordinary column is what the cutover must KEEP.",
		total, line, head, tail)
}

// logArrivalRefusals reports the arrival claim guard, and it belongs beside
// logBurialShadow because it is the same kind of thing: a should-be-zero.
//
// A refusal means an order reached its destination carrying a bin the ledger says
// it does not own — two orders pointed at one bin. Both known causes are fixed
// (the swap-rebind clobber that mis-aimed a leg, and the ghost eviction that wiped
// a live claim), so a non-zero count here is either a third cause or a regression
// of one of those.
//
// Silent at zero, like the soft-burial count: a plant where this never fires
// should not spend a line every sweep saying so, and no arrival-refusal lines in a
// week of journal is itself the answer. THIS IS THE NUMBER the deferred
// fail-vs-park disposition waits on (see arrival_guard.go) — if it stays zero, a
// refusal is an impossible state and failing loud is cheap and right; if it
// climbs, parking has to earn a releaser and a floor.
func (s *ReconciliationService) logArrivalRefusals() {
	tally := ArrivalRefusalTally()
	total := 0
	for _, n := range tally {
		total += n
	}
	if total == 0 {
		return
	}
	head, tail, _ := strings.Cut(ArrivalRefusalMarker, " ")
	s.logFn("arrival-guard REFUSALS=%d (expected 0) — orders arrived carrying a bin the ledger says "+
		"they do not own; by site %v. THIS COUNT is the number, not a grep of it; for the per-event "+
		"lines search the journal for %q followed by %q — split here so this line stays out of its "+
		"own results.",
		total, tally, head, tail)
}

// logDestNodeDrift reports the bin-state drift tripwire — the third should-be-zero
// on this sweep, and the first one about a bin-state FACT rather than a guard.
//
// A drift means one bin's destination is recorded in two places that disagree: the
// order's plan, and the order_bins row the settle is about to place from. Silent
// at zero for the same reason as its neighbours — a plant where this never fires
// should not spend a line every sweep saying so.
//
// THE READ INSTRUCTION DOES NOT CONTAIN ITS OWN SEARCH PATTERN. Splitting it is
// not fussiness: a tally line that quotes its grep string is matched by that
// string, so the count read back is tally-lines-plus-events and a should-be-zero
// reads non-zero forever (PLAN §R.9, and see the two lines above this one).
func (s *ReconciliationService) logDestNodeDrift() {
	tally := DestNodeDriftTally()
	total := 0
	for _, n := range tally {
		total += n
	}
	if total == 0 {
		return
	}
	driftHead, driftTail, _ := strings.Cut(DestNodeDriftMarker, " ")
	s.logFn("bin-state DRIFTS=%d (expected 0) — a bin's recorded destination disagreed with its own "+
		"plan at settle time, so the robot and the ledger are about to disagree about where that bin "+
		"is; by site %v. THIS COUNT is the number, not a grep of it. For the per-order detail (order, "+
		"bin, both nodes) search the journal for the words %q and %q adjacent — written apart here so "+
		"this line stays out of its own results.",
		total, tally, driftHead, driftTail)
}

// AutoConfirmStuckDeliveredOrders confirms delivered orders that have been
// waiting longer than the configured timeout. Returns count of auto-confirmed orders.
func (s *ReconciliationService) AutoConfirmStuckDeliveredOrders(timeout time.Duration) (int, error) {
	if timeout <= 0 {
		return 0, nil
	}

	// Compare against the injectable clock, NOT the DB's wall NOW(): order
	// updated_at is stamped with clock.Now() (sim-time in sim — orders/orders.go),
	// so a wall-NOW() comparison never fires once the sim clock outruns wall time
	// (10× → immediately), silently stranding every delivery at 'delivered'. In
	// production clock.Now() == time.Now(), so behaviour is unchanged there.
	cutoff := clock.Now().UTC().Add(-timeout)
	rows, err := s.db.Query(`
		SELECT id
		FROM orders
		WHERE status = 'delivered'
		  AND completed_at IS NULL
		  AND updated_at < $1
		  AND NOT skip_auto_confirm
		ORDER BY updated_at ASC
		LIMIT 100`, cutoff)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var orderIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		orderIDs = append(orderIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if s.confirmDelivered == nil {
		// Unwired callback — never the production path (engine.New sets it),
		// but bare unit fixtures may construct the service without one.
		// Log + no-op rather than panic so the periodic Loop survives.
		s.logFn("engine: auto-confirm skipped (%d candidate orders): confirmDelivered callback not wired", len(orderIDs))
		return 0, nil
	}

	confirmed := 0
	for _, id := range orderIDs {
		order, err := s.db.GetOrder(id)
		if err != nil {
			s.logFn("engine: auto-confirm reload order %d: %v (skipping this pass; periodic loop retries)", id, err)
			continue
		}
		if order.Status != "delivered" {
			continue // no longer delivered — nothing to confirm
		}
		if err := s.confirmDelivered(order); err != nil {
			s.logFn("engine: auto-confirm order %d: %v", order.ID, err)
			continue
		}
		s.logFn("engine: auto-confirmed stuck delivered order %d (uuid=%s)", order.ID, order.EdgeUUID)
		s.db.RecordRecoveryAction("auto_confirm_delivered", "order", order.ID,
			fmt.Sprintf("auto-confirmed delivered order after %s timeout", timeout), "system")
		confirmed++
	}

	return confirmed, nil
}

// AdvanceStuckReshuffleParents is the liveness backstop for compound (reshuffle)
// parents left in `reshuffling` after ALL their children reached a terminal status —
// a state that should be transient (a child→parent terminal event re-drives the
// parent) but strands FOREVER when that event is missed: a Core crash between the
// last child's terminal transition and AdvanceCompoundOrder, or the cancelled-child
// vector, which has no child→parent event arm at all. For each such parent it
// re-drives AdvanceCompoundOrder, which resumes (coordinated) / completes (plain) /
// fails (a failed-or-cancelled child) the parent per the children's terminal states.
// Idempotent: a parent advanced out of `reshuffling` is not re-selected next pass.
//
// OPEN PARENTS ARE NOT STUCK, they are mid-dig, so the predicate excludes them.
// "All children terminal" stops meaning "finished" under the fold, where it is
// the ordinary state between two moves of one reshuffle — and this sweep runs on
// the PERIODIC ticker, not only at boot, so it would find each of those gaps
// within a pass and complete a half-dug lane.
//
// This is the SECOND of the two guards, and not the load-bearing one.
// AdvanceCompoundOrder refuses an open parent itself, and the poller and event
// paths reach that refusal without ever coming through here — so correctness
// does not rest on this line. What rests on it is the forensic record: without
// it the sweep re-drives every open parent every pass and writes a
// RecordRecoveryAction below claiming it rescued a stranded reshuffle. A
// recovery log that fires when nothing was recovered destroys the same thing an
// alarm that cannot fire destroys — the next reader's ability to believe it —
// and it is arguably worse, because somebody will eventually count these.
func (s *ReconciliationService) AdvanceStuckReshuffleParents() (int, error) {
	rows, err := s.db.Query(`
		SELECT p.id
		FROM orders p
		WHERE p.status = 'reshuffling'
		  AND NOT p.open_for_children
		  AND EXISTS (SELECT 1 FROM orders c WHERE c.parent_order_id = p.id)
		  AND NOT EXISTS (
			SELECT 1 FROM orders c
			WHERE c.parent_order_id = p.id
			  AND c.status NOT IN ('confirmed', 'failed', 'cancelled', 'skipped')
		  )
		ORDER BY p.id
		LIMIT 100`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var parentIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		parentIDs = append(parentIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if s.advanceCompound == nil {
		// Unwired callback — never the production path (engine.New sets it), but bare
		// unit fixtures may omit it. Log + no-op rather than panic.
		if len(parentIDs) > 0 {
			s.logFn("engine: reshuffle-liveness skipped (%d stuck parents): advanceCompound callback not wired", len(parentIDs))
		}
		return 0, nil
	}

	// A THIRD REASON A PARENT IN THIS SET WAS NOT STUCK used to be filtered here:
	// a service dig held its lane past its last blocker until the bin it uncovered
	// was collected, which put it in `reshuffling`, sealed, with every child
	// terminal — every clause of the SELECT above and none of the meaning. That
	// hold is gone. A finished dig hands its corridor to the order collecting the
	// bin and terminates on the ordinary path, so a dig appearing here is stuck in
	// the plain sense this sweep was written for, and re-driving it is right.
	advanced := 0
	for _, id := range parentIDs {
		if err := s.advanceCompound(id); err != nil {
			s.logFn("engine: re-drive stuck reshuffle parent %d: %v (skipping this pass; loop retries)", id, err)
			continue
		}
		s.logFn("engine: re-drove stuck reshuffle parent %d (all children terminal)", id)
		s.db.RecordRecoveryAction("advance_stuck_reshuffle", "order", id,
			"re-drove compound parent stranded in reshuffling with all children terminal", "system")
		advanced++
	}
	return advanced, nil
}

// AbandonStuckOrders cancels RUNTIME-stuck orders that have sat without progress past the
// timeout: a robot parked at a staging node (staged), or a leg handed to the fleet that
// never started moving (dispatched). The latter is the long-weekend drain — orders
// dispatched Friday whose robots dwelled all weekend, drained, and faulted on transport
// when finally moved (2026-06-05/07) sit at `dispatched`/vendor CREATED.
//
// Scope = protocol.IsStuckSweepCandidate ({dispatched, staged}). in_transit is excluded (an
// actively moving robot is not stuck). PRE-DISPATCH WAITING (queued/sourcing) is excluded
// per the operator-driven-demand rule: demand is operator-driven and never
// evaporates, so a waiting order holds INDEFINITELY and is never abandoned on a
// timer — a wait of days is legitimate, and give-up is an operator decision. This
// is the narrowing of the old
// {queued,staged,sourcing,dispatched} set: a swap removal leg whose supply never arrives now
// WAITS (the sibling gate holds it in queued/sourcing) rather than being auto-cancelled at
// ~1h; the operator cancels if it is truly abandoned.
//
// Cancelling reuses the standard teardown (fleet cancel, bin unclaim, auto-return, Edge
// notify) and cascades to the swap sibling. Returns the count abandoned.
// operatorGatedTimeout is the separate, longer bound applied to operator-gated
// staging (dispatch.IsOperatorGatedStaging) — a coordinated swap leg parked at
// its wait point waiting for a human to press RELEASE. 0 means such a leg is
// never auto-cancelled. See config.StagingConfig.AbandonStuckOperatorGated.
func (s *ReconciliationService) AbandonStuckOrders(timeout, operatorGatedTimeout time.Duration) (int, error) {
	if timeout <= 0 {
		return 0, nil
	}

	// Sim-clock cutoff, same rationale as AutoConfirmStuckDeliveredOrders — order
	// updated_at is clock.Now()-stamped, so a wall-NOW() comparison never fires in sim.
	now := clock.Now().UTC()
	// Candidates are selected on the SHORTER of the two thresholds so both
	// classes surface here; each order is then held to its OWN bound in the loop
	// below. Selecting on `timeout` alone would silently floor a shorter
	// operator-gated bound at the base one.
	selectTimeout := timeout
	if operatorGatedTimeout > 0 && operatorGatedTimeout < selectTimeout {
		selectTimeout = operatorGatedTimeout
	}
	cutoff := now.Add(-selectTimeout)
	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT id
		FROM orders
		WHERE status IN (%s)
		  AND updated_at < $1
		ORDER BY updated_at ASC
		LIMIT 100`, protocol.StuckSweepStatusSQLList()), cutoff)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var orderIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		orderIDs = append(orderIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if s.abandonOrder == nil {
		s.logFn("engine: abandon-stuck skipped (%d candidate orders): abandonOrder callback not wired", len(orderIDs))
		return 0, nil
	}

	abandoned := 0
	for _, id := range orderIDs {
		order, err := s.db.GetOrder(id)
		if err != nil {
			s.logFn("engine: abandon-stuck reload order %d: %v (skipping this pass; periodic loop retries)", id, err)
			continue
		}
		// A sibling cancel from an earlier iteration this pass may already
		// have moved this one out of the stuck-sweep set (terminal, or a
		// re-queue back to a pre-dispatch waiting state) — skip if it is no
		// longer a runtime-stuck candidate.
		if !protocol.IsStuckSweepCandidate(order.Status) {
			continue
		}
		// A lane-gate-staged order is a robot physically parked at a lane wait
		// point, holding a bin and an unsealed waybill, waiting for Core to
		// append its tail. Its updated_at never moves while it dwells, so the
		// cutoff fires reliably — and abandoning it cancels a committed robot
		// mid-order and strands the bin it is carrying. That is not "stuck"; it
		// is Core owing it a decision.
		//
		// This is distinct from the operator-gated swap leg handled below:
		// IsGateStaged is a wait CORE owes a decision on, whereas
		// IsOperatorGatedStaging is one a HUMAN owes, and the second gets a
		// longer bound rather than an exemption.
		//
		// ⛔ THEY ARE NO LONGER MUTUALLY EXCLUSIVE, and the reasoning that used
		// to sit here — "by construction, IsGateStaged returns false for any
		// Coordinated order" — is now false. IsGateStaged asks which WAIT the
		// order is parked at, not what class of order it is, and a coordinated
		// plan can hold an operator wait and a lane wait at once. A coordinated
		// order parked at a lane wait satisfies BOTH predicates.
		//
		// THE ORDERING IS WHAT DISAMBIGUATES, and it is already right: this
		// check runs first, so whichever wait the order is actually parked at
		// decides which party owes it. Parked at the lane wait → Core owes it →
		// exempt. Parked at the operator wait → IsGateStaged is false, it falls
		// through, and the longer human-scale bound applies. The wait kind names
		// the party, which is the whole point of putting the kind on the step.
		//
		// So this is a re-derivation, not a repair: the code did the right thing
		// for a reason that has changed underneath it. Moving these two checks
		// past each other would now be a behaviour change.
		//
		// Skipping here rather than in the SQL keeps the exemption next to the
		// re-check above, where a reader looking at what the sweep cancels will
		// see it. It is the sweep's own stated principle (give-up is an operator
		// decision, demand never evaporates) applied to a case it predates.
		//
		// TODO(increment 7): a dwelling order must not be merely exempt — it needs
		// its own watchdog: a staged-too-long queue code and an operator surface,
		// so the wait is visible and owned rather than silent. Exemption alone
		// trades a destructive failure for an invisible one.
		if dispatch.IsGateStaged(order) {
			continue
		}
		// Which clock applies to THIS order. Operator-gated staging — a
		// coordinated swap leg parked at its wait point — is waiting on a
		// HUMAN, not on the system, so it answers to its own longer bound. It
		// stays BOUNDED rather than exempt, so a genuinely forgotten swap
		// still cannot park two robots forever. Springfield ALN_003
		// 2026-07-31: the base 1h cancelled a staged evac and cascaded its
		// supply, destroying both legs of a live changeover.
		// See dispatch.IsOperatorGatedStaging.
		effective := timeout
		reason := fmt.Sprintf("abandoned: stuck in %s past %s", order.Status, timeout)
		if dispatch.IsOperatorGatedStaging(order) {
			if operatorGatedTimeout <= 0 {
				s.logFn("engine: abandon-stuck holding operator-gated order %d (uuid=%s, staged %s) — auto-cancel disabled for operator-gated staging",
					order.ID, order.EdgeUUID, now.Sub(order.UpdatedAt).Round(time.Second))
				continue
			}
			effective = operatorGatedTimeout
			reason = fmt.Sprintf("abandoned: operator-gated staging past %s", operatorGatedTimeout)
		}
		// Selected on the shorter cutoff but not yet past its own bound. Logged
		// rather than silent: a held order is a robot parked holding a bin, and
		// an exemption nobody can see trades a destructive failure for an
		// invisible one.
		if order.UpdatedAt.After(now.Add(-effective)) {
			s.logFn("engine: abandon-stuck holding order %d (uuid=%s status=%s) — waiting %s of %s",
				order.ID, order.EdgeUUID, order.Status, now.Sub(order.UpdatedAt).Round(time.Second), effective)
			continue
		}
		// A SWEPT DIG LEG ENDS ITS CHAPTER, AND HAS TO SAY SO IN THE ONE WORD
		// THE DISPOSITION READS. Gate 1's wording is "a failed OR SWEPT dig
		// child dissolves the chapter", and the chapter-end test reads the
		// cancel's marker rather than its status — deliberately, because an
		// OPERATOR cancel must not read as a chapter end or the parent comes
		// back to the acquiring set carrying work a human stopped. A sweep is
		// not an operator, so it says which it is.
		//
		// The diagnostic sentence is not lost, it moves: RecordRecoveryAction
		// below still files the full "stuck in %s past %s" text, which is where
		// a sweep's reasoning belongs. The leg's error_detail is read by
		// machinery; the recovery ledger is read by people.
		legReason := reason
		if order.ParentOrderID != nil {
			legReason = dispatch.ReshuffleLegFailedDetail
		}
		if err := s.abandonOrder(order, legReason); err != nil {
			s.logFn("engine: abandon stuck order %d: %v", order.ID, err)
			continue
		}
		s.logFn("engine: abandoned stuck order %d (uuid=%s status=%s)", order.ID, order.EdgeUUID, order.Status)
		s.db.RecordRecoveryAction("abandon_stuck_order", "order", order.ID, reason, "system")
		abandoned++
	}

	return abandoned, nil
}
