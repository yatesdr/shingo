package engine

import (
	"errors"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/demands"
)

// threshold_episodes.go — the demand grain, Core side.
//
// Core owns two of the episode kinds, threshold and maintain; this file is the
// threshold monitor's. A threshold episode is a continuous period during which
// a place's plant-wide in-loop total for a payload sat below its configured
// threshold. Cell and changeover episodes are authored on Edge and arrive
// through the state-transfer seam.
//
// WHY AN EDGE AND NOT A LEVEL. checkBindings is level-triggered — "total <
// threshold" is true continuously, for as long as it is true — and a level has
// no memory. Mint an origin on the level and you get a fresh "demand" every
// debounce window: an id per ORDER, which is the paperwork-counting failure the
// whole grain argument was against. The open row converts it into an edge:
// its presence answers "already below", and the period between the edges is
// the demand.
//
// THE STATE IS THE TABLE. Whether a place has an open episode, and which one,
// is read from demand_origins at the moment it is needed — one probe of the
// partial unique index on episode_key. The monitor used to keep that answer in
// a map, rehydrated at boot and cleared on close, and every path that closed a
// row without clearing the map (the childless pass, a hand edit, a close whose
// UPDATE failed) left the map stamping a closed origin on new orders. Reading
// the row cannot disagree with the row.
//
// ONE GRAIN: the episode key, (core_node_name, payload). demand_registry can
// carry the same place under two station ids; the episode key cannot, and
// neither can anything keyed here.

// openThresholdEpisode records the falling edge: it mints an episode for a place
// that has none open and returns its origin id, or "" when it could not, after
// logging why.
//
// A PLAIN INSERT, and the partial unique index is the arbiter of a race: two
// evaluations that both read "none open" both try, one wins, and the loser's
// INSERT fails with store.ErrEpisodeAlreadyOpen. That is "already open, re-read"
// — the loser joins the winner's episode. It is deliberately not ON CONFLICT DO
// NOTHING, which would also swallow the failure that is not a race.
//
// CALLED WITH m.mu NOT HELD. A mint is a write to Postgres, and holding the
// monitor's mutex across it would serialise every evaluation behind one INSERT.
func (m *ThresholdMonitor) openThresholdEpisode(key string, b thresholdEntry, total int, usedEdgeReports bool) string {
	if m.eng == nil || m.eng.db == nil {
		return ""
	}
	expected, reason := m.expectedOrdersForThreshold(b, total)
	origin := store.DemandOrigin{
		OriginID:   uuid.NewString(),
		EpisodeKey: key,
		Kind:       protocol.EpisodeKindThreshold,
		// The binding that decided it, station included — the station is data
		// on the episode, not part of the place.
		TriggerRef:            "binding:" + b.stationID + "|" + b.coreNodeName + "|" + b.payloadCode,
		StationID:             b.stationID,
		CoreNodeName:          b.coreNodeName,
		PayloadCode:           b.payloadCode,
		OpenedAt:              m.nowFn().UTC(),
		OpenedTotal:           total,
		Threshold:             b.threshold,
		ExpectedOrders:        expected,
		ExpectedUnknownReason: reason,
	}
	err := m.eng.db.OpenThresholdEpisode(origin, usedEdgeReports)
	if errors.Is(err, store.ErrEpisodeAlreadyOpen) {
		winner, rerr := m.eng.db.OpenOriginForKey(key)
		if rerr == nil && winner != "" {
			m.eng.dbg("threshold_monitor: demand for %s opened concurrently; joining origin=%s", key, winner)
			return winner
		}
		m.eng.logFn("threshold_monitor: open demand episode key=%s: lost the mint race and could not read the winner (%v) — NOT FIRING, an order with no episode cannot be subtracted by the next ask", key, rerr)
		return ""
	}
	if err != nil {
		// A failure to record must never look like a recording: no origin, so
		// no fire, and the next evaluation tries again.
		m.eng.logFn("threshold_monitor: open demand episode key=%s: %v — NOT FIRING, an order with no episode cannot be subtracted by the next ask", key, err)
		return ""
	}
	m.eng.logFn("threshold_monitor: DEMAND OPENED origin=%s station=%s loader=%s payload=%s total=%d threshold=%d expected_orders=%s",
		origin.OriginID, b.stationID, b.coreNodeName, b.payloadCode, total, b.threshold,
		describeExpected(expected, reason))
	return origin.OriginID
}

// closeThresholdEpisode ends the open episode for a place, if there is one.
// One read and, only when a row is open, one write.
func (m *ThresholdMonitor) closeThresholdEpisode(key, reason, closedBy string) {
	if m.eng == nil || m.eng.db == nil {
		return
	}
	originID, err := m.eng.db.OpenOriginForKey(key)
	if err != nil {
		m.eng.logFn("threshold_monitor: read open demand for %s: %v (closing nothing)", key, err)
		return
	}
	if originID == "" {
		return
	}
	m.closeThresholdEpisodeByID(originID, key, reason, closedBy)
}

// closeThresholdEpisodeByID writes one close and reports whether a row moved.
//
// BY ORIGIN, NOT BY KEY. The close names the episode it read, so a caller
// holding a stale candidate — read before a concurrent close-and-reopen —
// cannot close the newer episode for the same place.
func (m *ThresholdMonitor) closeThresholdEpisodeByID(originID, key, reason, closedBy string) bool {
	closed, err := m.eng.db.CloseDemandOriginByID(originID, reason, closedBy, m.nowFn().UTC())
	if err != nil {
		// The row stays open, which is the truth: the next rising edge, or the
		// reconciling sweep, closes it.
		m.eng.logFn("threshold_monitor: close demand episode origin=%s key=%s: %v", originID, key, err)
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
	m.eng.logFn("threshold_monitor: DEMAND CLOSED origin=%s key=%s reason=%s by=%s", originID, key, reason, closedBy)
	return true
}

// closeThresholdEpisodesNotIn closes every candidate episode whose place has no
// live binding, and returns the keys it actually closed.
//
// The set difference itself lives in staleEpisodeKeys, shared with the
// maintainer's config-withdrawn pass. Keys where the UPDATE moved no row are
// left out on purpose: another path had already closed that episode.
func (m *ThresholdMonitor) closeThresholdEpisodesNotIn(candidates map[string]string, live map[string]bool, closedBy string) []string {
	if m.eng == nil || m.eng.db == nil {
		return nil
	}
	var closed []string
	for _, key := range staleEpisodeKeys(candidates, live) {
		if m.closeThresholdEpisodeByID(candidates[key], key, protocol.CloseReasonThresholdRemoved, closedBy) {
			closed = append(closed, key)
		}
	}
	return closed
}

// reconcileThresholdBindings is the sweep's threshold pass: close every open
// threshold episode whose place no longer has a monitored binding. CLOSE-ONLY:
// it never evaluates a level and never creates an order.
//
// THE PRECONDITION IS THE BINDING, NOT THE LEVEL, and that is a deliberate
// narrowing rather than an oversight. The rising edge in checkBindings already
// owns the level close and it owns it well — it runs on every delta, and it
// says `recovered`, which is the true reason. A sweep that also read the total
// would be a second opinion on a question that already has an answer, and the
// two would race: the sweep closing an episode the next delta immediately
// re-opens, filling the surface with false short episodes.
//
// What no notification path owns is a binding that vanished with nothing
// firing. SyncRegistry replaces a station's whole registry and emits a
// RegistryChange only when a threshold VALUE moved; a reconnect's derive
// discards its change list; seeddev is a separate process; and a row deleted
// by hand announces itself to nobody. You cannot wire up an absence; you can
// only notice it afterwards.
//
// IT CAN NO LONGER OSCILLATE, which is why it no longer needs a second half.
// Both of its inputs are the database — the live set from demand_registry, the
// candidates from demand_origins — and so is everything that could re-open what
// it closes: a place with no binding is not evaluated at all. Under the copies
// the monitor kept, a close here re-armed a falling edge the next delta minted
// again (Springfield 2026-08-19: 411 opens, 405 closes), and a later pass had
// to rebuild the monitor's memory as well. There is no memory left to rebuild.
//
// KEYED BY PLACE, the episode key's grain: an episode stays open while ANY
// station's registry row still monitors its place.
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
		// A threshold of 0 is the documented OPT-OUT. ListDemandThresholds
		// already filters `replenish_uop_threshold > 0` in SQL; this is here
		// because "live" is the set this sweep CLOSES against, and a silent
		// widening of that query would turn opted-out bindings into reasons to
		// keep episodes open forever. The predicate belongs next to the
		// decision it drives.
		if e.ReplenishUOPThreshold <= 0 {
			continue
		}
		live[placeKey(e.CoreNodeName, e.PayloadCode)] = true
	}

	open, err := m.eng.db.ListOpenThresholdEpisodes()
	if err != nil {
		m.eng.logFn("demand_reconciler: list open threshold episodes: %v", err)
		return 0
	}
	candidates := make(map[string]string, len(open))
	for _, o := range open {
		candidates[placeKey(o.CoreNodeName, o.PayloadCode)] = o.OriginID
	}
	return len(m.closeThresholdEpisodesNotIn(candidates, live, protocol.ClosedBySweep))
}

// closeThresholdEpisodesForChangedBindings ends the episodes whose binding was
// edited — the denominator moved, or the binding stopped existing.
//
// A threshold change closes the episode and lets the next evaluation open a new
// one. Continuing the old episode across the change would make its cost_ratio a
// division by a number that was never in force for most of its life. Both rows
// then record the transition honestly.
//
// A NEW THRESHOLD OF ZERO IS NOT A NEW DENOMINATOR. SyncRegistry reports a
// binding that vanished as a change to zero — that is how a retired loader
// arrives here — and zero is also the documented opt-out. Either way the place
// is no longer watched, which is `threshold_removed`: the same word
// reconcileThresholdBindings uses when it finds the same state by sweeping.
//
// A newly ADDED binding has no open episode, so this is a no-op for it.
func (m *ThresholdMonitor) closeThresholdEpisodesForChangedBindings(changes []demands.RegistryChange) {
	for _, c := range changes {
		if c.OldThreshold == c.NewThreshold {
			continue
		}
		reason := protocol.CloseReasonThresholdChanged
		if c.NewThreshold <= 0 {
			reason = protocol.CloseReasonThresholdRemoved
		}
		m.closeThresholdEpisode(placeKey(c.CoreNodeName, c.PayloadCode), reason, protocol.ClosedByNotification)
	}
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
