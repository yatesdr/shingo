package engine

import (
	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/demands"
)

// threshold_episodes.go — the demand grain, Core side.
//
// Core owns exactly ONE of the three episode kinds. A threshold episode is a
// continuous period during which a loader's plant-wide in-loop total for a
// payload sat below its configured threshold. Cell and changeover episodes are
// authored on Edge and arrive through the state-transfer seam.
//
// WHY AN EDGE AND NOT A LEVEL. checkBindings is level-triggered — "total <
// threshold" is true continuously, for as long as it is true — and a level has
// no memory. Mint an origin on the level and you get a fresh "demand" every
// debounce window: an id per ORDER, which is the paperwork-counting failure the
// whole grain argument was against. below_threshold_since converts it into an
// edge, and the period between the edges is the demand.
//
// STATE LIVES IN MEMORY, WRITE-THROUGH ON TRANSITION. Both maps sit beside
// thresholdsByPayload under the same mutex. Transitions are rare — twice per
// episode — while evaluations run on every incoming delta, so the write cost is
// nothing and the read cost must be nothing.

// openEpisodeRef is what the monitor remembers about an open episode: the id to
// stamp on its signals, and the payload it belongs to.
//
// The payload is carried rather than parsed back out of the bindingKey. The key
// is station|node|payload and the payload is its last field, so a suffix match
// would work today — and would break the first time a payload code contains the
// separator, silently, by failing to close an episode nobody is watching.
type openEpisodeRef struct {
	originID    string
	payloadCode string
}

// openThresholdEpisode records the falling edge, minting an episode if this is
// the first crossing. Idempotent while the level stays breached.
//
// CALLED WITH m.mu NOT HELD. It takes the lock itself around the map reads and
// writes and deliberately does the database work outside it: a mint is a write
// to Postgres, and holding the monitor's mutex across it would serialise every
// evaluation in the system behind one INSERT.
func (m *ThresholdMonitor) openThresholdEpisode(key string, b thresholdEntry, total int, usedEdgeReports bool) {
	// The pure unit harness (newTestMonitor) leaves eng nil, same reason
	// fireHook exists: it tests the debounce and fire gates without standing up
	// an engine. No engine means no database, and an episode that cannot be
	// persisted must not be stamped as if it had been.
	if m.eng == nil || m.eng.db == nil {
		return
	}
	m.mu.Lock()
	_, alreadyBelow := m.belowThresholdSince[key]
	m.mu.Unlock()
	if alreadyBelow {
		// Still below, same episode. Re-stamping would make every demand look
		// as if it had just started, which is exactly how 2026-07-21 hid: a
		// two-hour demand rendered as a stream of instantaneous ones.
		return
	}

	expected, reason := m.expectedOrdersForThreshold(b, total)
	origin := store.DemandOrigin{
		OriginID:   uuid.NewString(),
		EpisodeKey: protocol.ThresholdEpisodeKey(b.coreNodeName, b.payloadCode),
		Kind:       protocol.EpisodeKindThreshold,
		// The binding is the identity here, and bindingKey IS the episode key's
		// payload — recorded so a reader does not have to re-derive it.
		TriggerRef:            "binding:" + key,
		StationID:             b.stationID,
		CoreNodeName:          b.coreNodeName,
		PayloadCode:           b.payloadCode,
		OpenedAt:              m.now().UTC(),
		OpenedTotal:           total,
		Threshold:             b.threshold,
		ExpectedOrders:        expected,
		ExpectedUnknownReason: reason,
	}
	if err := m.eng.db.OpenThresholdEpisode(origin, usedEdgeReports); err != nil {
		// The mint failed, so do NOT stamp the edge. Leaving the level unstamped
		// means the next evaluation tries again; stamping it would mark the
		// demand as recorded when nothing recorded it, and no later crossing
		// would ever retry. A failure to observe must not look like an
		// observation.
		m.eng.logFn("threshold_monitor: open demand episode key=%s: %v", key, err)
		return
	}

	m.mu.Lock()
	m.belowThresholdSince[key] = origin.OpenedAt
	m.openOrigins[key] = openEpisodeRef{originID: origin.OriginID, payloadCode: b.payloadCode}
	m.mu.Unlock()

	m.eng.logFn("threshold_monitor: DEMAND OPENED origin=%s station=%s loader=%s payload=%s total=%d threshold=%d expected_orders=%s",
		origin.OriginID, b.stationID, b.coreNodeName, b.payloadCode, total, b.threshold,
		describeExpected(expected, reason))
}

// closeThresholdEpisode ends the episode for a binding, if the monitor is
// holding one.
//
// A no-op when nothing is held, which is the common case: the rising edge is
// evaluated on every delta for a payload that is comfortably stocked, so this
// is called constantly and must be free when there is nothing to do.
func (m *ThresholdMonitor) closeThresholdEpisode(key, reason, closedBy string) {
	if m.eng == nil || m.eng.db == nil {
		return
	}
	m.mu.Lock()
	ref, open := m.openOrigins[key]
	m.mu.Unlock()
	if !open {
		return
	}
	m.closeThresholdEpisodeRef(key, ref, reason, closedBy)
}

// closeThresholdEpisodeRef writes one close and drops the monitor's hold on it.
// Reports whether a row actually moved.
//
// SPLIT OUT SO THE SWEEP CAN CLOSE AN EPISODE THE MONITOR IS NOT HOLDING. The
// notification path always starts from openOrigins, but the sweep starts from
// the database, and the two sets are not the same — a close whose UPDATE failed
// leaves the maps cleared and the row open, which is precisely the state that
// needs a floor under it. Requiring a map entry would have made the sweep blind
// to the one case the notification path had already given up on.
//
// The hold is dropped only when it is still THIS episode. Comparing origin ids
// rather than just deleting the key means a sweep pass carrying a stale
// candidate — read before a concurrent close-and-reopen — cannot delete the
// hold on the newer episode, which would suppress its rising edge and strand a
// live demand.
func (m *ThresholdMonitor) closeThresholdEpisodeRef(key string, ref openEpisodeRef, reason, closedBy string) bool {
	m.mu.Lock()
	if held, ok := m.openOrigins[key]; ok && held.originID == ref.originID {
		delete(m.openOrigins, key)
		delete(m.belowThresholdSince, key)
	}
	m.mu.Unlock()

	closed, err := m.eng.db.CloseDemandOriginByID(ref.originID, reason, closedBy, m.now().UTC())
	if err != nil {
		// The maps are already cleared, so this episode will not be closed by a
		// notification path again — the reconciling sweep is what catches it,
		// which is precisely the job it exists for, and why the sweep reads the
		// database rather than these maps.
		m.eng.logFn("threshold_monitor: close demand episode origin=%s key=%s: %v", ref.originID, key, err)
		return false
	}
	if !closed {
		// Already closed by another path. Ordinary — several sites evaluate the
		// edges and the sweep runs underneath all of them — and specifically NOT
		// counted as the sweep's work, or closed_by's whole purpose (measuring
		// how much of the closing the notification paths have stopped doing)
		// would be inflated by races it did not win.
		return false
	}
	m.eng.logFn("threshold_monitor: DEMAND CLOSED origin=%s key=%s reason=%s by=%s", ref.originID, key, reason, closedBy)
	return true
}

// closeThresholdEpisodesForPayloadNotIn closes every open episode for a payload
// whose binding is no longer in the rebuilt set.
//
// THIS IS THE NOTIFICATION PATH for threshold_removed, not the only one — the
// reconciling sweep fires it too, and so does the maintainer's config-withdrawn
// pass. (This comment did once say "the only site", and it stopped being true
// when the sweep landed. The reason is a shared vocabulary, not a private one:
// anything that notices a declaration vanish says threshold_removed.)
//
// engagePayloads rebuilds a payload's bindings from demand_registry and, before
// the grain existed, simply dropped whatever was there — so a binding deleted
// underneath an open demand stranded that demand permanently, with nothing
// anywhere saying it had ended.
//
// SCOPED BY LIVE-KEY SET, NOT BY "the payload has no bindings left" — see
// staleEpisodeKeys, which now owns that comparison for every caller.
//
// The need did not RECOVER here — it stopped being watched. Closing these as
// `recovered` would report a satisfied demand every time somebody deleted a
// binding, which is the same conflation claim_removed exists to prevent on the
// cell side.
func (m *ThresholdMonitor) closeThresholdEpisodesForPayloadNotIn(payload string, live map[string]bool) {
	if m.eng == nil || m.eng.db == nil {
		return
	}
	m.mu.Lock()
	candidates := make(map[string]openEpisodeRef)
	for key, ref := range m.openOrigins {
		if ref.payloadCode == payload {
			candidates[key] = ref
		}
	}
	m.mu.Unlock()

	m.closeThresholdEpisodesNotIn(candidates, live, protocol.ClosedByNotification)
}

// closeThresholdEpisodesNotIn is the threshold side of THE key comparison, and
// there is exactly one of it.
//
// Both the notification path and the reconciling sweep ask the same question —
// "which of these open episodes has no binding left?" — and they must not be
// able to answer it differently. What the callers legitimately differ on is
// only WHERE THE CANDIDATES COME FROM. The notification path knows which
// payload it just rebuilt and reads the monitor's own hold; the sweep knows
// nothing and reads the database. That difference is a parameter, not a second
// function.
//
// The set difference itself lives in staleEpisodeKeys, shared with the
// maintainer's config-withdrawn pass — see there for the scoping lesson it
// carries.
func (m *ThresholdMonitor) closeThresholdEpisodesNotIn(candidates map[string]openEpisodeRef, live map[string]bool, closedBy string) int {
	if m.eng == nil || m.eng.db == nil {
		return 0
	}
	closed := 0
	for _, key := range staleEpisodeKeys(candidates, live) {
		if m.closeThresholdEpisodeRef(key, candidates[key], protocol.CloseReasonThresholdRemoved, closedBy) {
			closed++
		}
	}
	return closed
}

// reconcileThresholdBindings is the sweep's threshold pass: close every open
// threshold episode whose BINDING no longer exists.
//
// THE PRECONDITION IS THE BINDING, NOT THE LEVEL, and that is a deliberate
// narrowing rather than an oversight. The rising edge in checkBindings already
// owns the level close and it owns it well — it runs on every delta, it applies
// the hysteresis margin, and it says `recovered`, which is the true reason. A
// sweep that also read the total would be a second opinion on a question that
// already has an answer, and the two would race: the sweep closing an episode
// the next delta immediately re-opens, filling the surface with false
// short episodes. A reconciler that closes live episodes is worse than none.
//
// What no notification path owns is a binding that vanished with nothing
// firing, and there are live sites where exactly that happens. SyncRegistry
// DELETEs and re-INSERTs a station's whole registry, and only emits a
// RegistryChange when a threshold VALUE moved — so a binding that vanishes and
// returns unchanged inside one transaction emits nothing at all. Worse, three
// call sites discard the change list entirely, and one of them is the
// stale-edge reaper (core_handler.go, `SyncDemandRegistry(sid, nil)`), which
// deletes every binding a station has. You cannot wire up an absence; you can
// only notice it afterwards.
//
// IT READS THE DATABASE, NOT openOrigins. The monitor's map is a cache of what
// the monitor thinks is open, and the failure this sweep exists to catch
// includes the cases where that belief is the thing that went wrong.
func (m *ThresholdMonitor) reconcileThresholdBindings() int {
	if m.eng == nil || m.eng.db == nil {
		return 0
	}
	entries, err := m.eng.db.ListDemandThresholds()
	if err != nil {
		// A READ FAILURE IS NOT AN EMPTY BINDING SET. Treating it as one would
		// close every open threshold episode in the plant on a transient
		// Postgres blip — the sweep's worst possible failure mode, and one that
		// would look exactly like a very effective sweep.
		m.eng.logFn("demand_reconciler: list demand thresholds: %v", err)
		return 0
	}
	live := make(map[string]bool, len(entries))
	for _, e := range entries {
		// A threshold of 0 is the documented OPT-OUT — Core never signals for
		// such a pair, bin-count is Edge's — so for episode purposes the place
		// is no longer watched and `threshold_removed` is the honest reason
		// when the value drops to 0 with nothing firing.
		//
		// ListDemandThresholds already filters `replenish_uop_threshold > 0` in
		// SQL, so this is belt-and-braces rather than the load-bearing filter.
		// It is here because "live" is the set this sweep CLOSES against, and a
		// silent widening of that query — someone reusing it for a listing page
		// and dropping the WHERE — would turn opted-out bindings into reasons
		// to keep episodes open forever, with no test on that query able to say
		// so. The predicate belongs next to the decision it drives.
		if e.ReplenishUOPThreshold <= 0 {
			continue
		}
		live[bindingKey(e.StationID, e.CoreNodeName, e.PayloadCode)] = true
	}

	open, err := m.eng.db.ListOpenThresholdEpisodes()
	if err != nil {
		m.eng.logFn("demand_reconciler: list open threshold episodes: %v", err)
		return 0
	}
	candidates := make(map[string]openEpisodeRef, len(open))
	for _, o := range open {
		candidates[bindingKey(o.StationID, o.CoreNodeName, o.PayloadCode)] = openEpisodeRef{
			originID: o.OriginID, payloadCode: o.PayloadCode,
		}
	}
	return m.closeThresholdEpisodesNotIn(candidates, live, protocol.ClosedBySweep)
}

// closeThresholdEpisodesForChangedBindings ends the episodes whose DENOMINATOR
// moved.
//
// A threshold change closes the episode and lets the next evaluation open a new
// one. Continuing the old episode across the change would make its cost_ratio a
// division by a number that was never in force for most of its life — the
// episode would be measured against a threshold nobody was using. Both rows
// then record the transition honestly, and OnThresholdChanges already carries
// OldThreshold/NewThreshold for whoever wants to read it back.
//
// A newly ADDED binding has no open episode, so this is a no-op for it.
func (m *ThresholdMonitor) closeThresholdEpisodesForChangedBindings(changes []demands.RegistryChange) {
	for _, c := range changes {
		if c.OldThreshold == c.NewThreshold {
			continue
		}
		m.closeThresholdEpisode(
			bindingKey(c.StationID, c.CoreNodeName, c.PayloadCode),
			protocol.CloseReasonThresholdChanged, protocol.ClosedByNotification)
	}
}

// currentThresholdOrigin returns the open episode id for a binding, if any —
// what a fired signal gets stamped with so the orders Edge mints in response
// are children of the demand rather than 484 unrelated rows.
func (m *ThresholdMonitor) currentThresholdOrigin(key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.openOrigins[key].originID
}

// expectedOrdersForThreshold is the system's own stated intent at the falling
// edge: ceil((threshold - opened_total) / capacity).
//
// OPENED_TOTAL IS CLAMPED AT 0. Without the clamp a -443 reading produces
// ceil(455/18) = 26 computed entirely from garbage, and the ratio that comes
// out the other end is a division by a number that was never true.
//
// NULL, NOT 0 AND NOT 1, WHEN CAPACITY IS UNKNOWABLE. A payload with no
// uop_capacity gives a denominator nobody can compute, and both 0 and 1 render
// as a real ratio somebody would draw a conclusion from. A demand whose
// denominator is unknowable is a different state from one whose denominator is
// 1, and the surface shows a dash for it.
func (m *ThresholdMonitor) expectedOrdersForThreshold(b thresholdEntry, total int) (*int, string) {
	p, err := m.eng.db.GetPayloadByCode(b.payloadCode)
	if err != nil {
		return nil, "capacity lookup failed"
	}
	if p == nil {
		return nil, "payload not in catalog"
	}
	if p.UOPCapacity <= 0 {
		return nil, "payload has no uop_capacity"
	}
	opened := total
	if opened < 0 {
		opened = 0
	}
	gap := b.threshold - opened
	if gap <= 0 {
		// Below threshold but the gap rounds to nothing — one bin closes it.
		one := 1
		return &one, ""
	}
	n := (gap + p.UOPCapacity - 1) / p.UOPCapacity
	return &n, ""
}

// describeExpected renders expected_orders for a log line without lying about
// a NULL. "unknown (reason)" is not the same statement as "0".
func describeExpected(expected *int, reason string) string {
	if expected == nil {
		if reason == "" {
			reason = "unknown"
		}
		return "unknown (" + reason + ")"
	}
	return itoa(*expected)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// rehydrateThresholdEpisodes rebuilds the open-episode maps from the database.
//
// WITHOUT THIS, EVERY CORE RESTART DOUBLES EVERY OPEN DEMAND. startupSweep
// rebuilds thresholdsByPayload from scratch and then evaluates every binding;
// with empty maps each binding still below threshold reads as a first crossing,
// mints a second episode for a place that already has one, and the original
// stays open forever. The partial unique index turns that into a write error
// rather than a silent duplicate — but an error on every restart, for every
// hungry loader, is not the outcome either.
//
// It is not an edge case. The file's own comment notes that restarting Core is
// the remedy an operator reaches for BECAUSE the counts look wrong — i.e.
// precisely when demands are open. This is the highest-frequency path here.
func (m *ThresholdMonitor) rehydrateThresholdEpisodes() {
	if m.eng == nil || m.eng.db == nil {
		return
	}
	open, err := m.eng.db.ListOpenThresholdEpisodes()
	if err != nil {
		// Leave the maps empty and let the mint's unique-index failure be the
		// backstop. Guessing "nothing is open" would be worse: it is the state
		// that mints duplicates.
		m.eng.logFn("threshold_monitor: rehydrate open demand episodes: %v", err)
		return
	}
	m.mu.Lock()
	for _, o := range open {
		key := bindingKey(o.StationID, o.CoreNodeName, o.PayloadCode)
		m.openOrigins[key] = openEpisodeRef{originID: o.OriginID, payloadCode: o.PayloadCode}
		m.belowThresholdSince[key] = o.OpenedAt
	}
	n := len(open)
	m.mu.Unlock()
	if n > 0 {
		m.eng.logFn("threshold_monitor: rehydrated %d open demand episode(s) across restart", n)
	}
}
