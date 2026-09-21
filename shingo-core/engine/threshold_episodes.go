package engine

import (
	"sort"
	"strings"

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
// stamp on its signals, and the station and payload it belongs to.
//
// Both are carried rather than parsed back out of the bindingKey. The key is
// station|node|payload, so a prefix match would find the station and a suffix
// match the payload — today. Either would break the first time the field it
// matches contains the separator, silently, by failing to close an episode
// nobody is watching.
//
// THE STATION IS HERE FOR Resync. It has to answer "which
// payloads does this station still hold in memory?", and an episode can outlive
// its entry in thresholdsByPayload — rehydrateThresholdEpisodes fills
// openOrigins at boot, before the startup sweep builds the binding cache, and
// closeThresholdEpisodeRef clears an episode without touching the cache.
// Reading both maps is what makes the answer complete, and reading openOrigins
// needs a station on the ref.
type openEpisodeRef struct {
	originID    string
	stationID   string
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
	m.openOrigins[key] = openEpisodeRef{originID: origin.OriginID, stationID: b.stationID, payloadCode: b.payloadCode}
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
// rebuildPayloadBindings rebuilds a payload's bindings from demand_registry and,
// before the grain existed, simply dropped whatever was there — so a binding
// deleted underneath an open demand stranded that demand permanently, with
// nothing anywhere saying it had ended. This is its only caller.
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
//
// IT RETURNS THE KEYS IT ACTUALLY CLOSED, not a count, because closing the
// episode is only half of what an absent binding needs doing about it — the
// sweep has to go on and rebuild the monitor's memory for the payloads those
// keys belong to, and it cannot do that from a number. Keys where the UPDATE
// moved no row are left out on purpose: another path had already closed that
// episode, so this pass did not discover anything and has nothing to announce.
// Order is staleEpisodeKeys' order, which is sorted.
func (m *ThresholdMonitor) closeThresholdEpisodesNotIn(candidates map[string]openEpisodeRef, live map[string]bool, closedBy string) []string {
	if m.eng == nil || m.eng.db == nil {
		return nil
	}
	var closed []string
	for _, key := range staleEpisodeKeys(candidates, live) {
		if m.closeThresholdEpisodeRef(key, candidates[key], protocol.CloseReasonThresholdRemoved, closedBy) {
			closed = append(closed, key)
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
// returns unchanged inside one transaction emits nothing at all. Two of its
// three call sites discard the change list outright; one of those tells the
// monitor by another route (Resync, on Edge register), and the other is seeddev,
// a separate process with no running monitor to tell. A row deleted by hand in
// Postgres announces itself to nobody at all. You cannot wire up an absence; you
// can only notice it afterwards.
//
// THE STALE-EDGE REAPER USED TO BE THE WORST OF THOSE SITES AND NO LONGER
// EXISTS. core_handler.go used to delete every binding a silent station had, and
// what this pass then noticed was not an absence but a fabrication: nobody
// withdrew that config, so every close it wrote was false, and the close cleared
// belowThresholdSince and re-armed the mint. That is not a sweep catching an
// absence, it is a sweep supplying the other half of an oscillator — Springfield,
// 1293 rows for one station over two days. The floor is only as good as the
// facts under it, which is why the wipe went rather than this pass.
//
// IT READS THE DATABASE, NOT openOrigins. The monitor's map is a cache of what
// the monitor thinks is open, and the failure this sweep exists to catch
// includes the cases where that belief is the thing that went wrong.
//
// AND IT FINISHES THE RECONCILIATION IT STARTS — see
// dropAbsentBindingsFromMemory. Closing the episode and leaving the binding in
// thresholdsByPayload is one half of a reconciliation: the close clears
// belowThresholdSince, so the next delta mints the same demand again and the
// next pass closes it again. Springfield 2026-08-19 is that shape with no
// reaper anywhere in it — 411 opens, 411 distinct origins, 405 closes, every
// binding still matching the registry on station, node, payload and threshold —
// and after six eliminations the writer that emptied demand_registry is still
// unnamed. An entry point nobody can name cannot be fixed at its door, so it is
// fixed here: whatever put the binding in memory, the floor takes it out again
// on the same pass that closes its episode.
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
	// HOW MANY ROWS THE REGISTRY HOLDS PER PAYLOAD, counted off the SAME read
	// the live set is built from, so the number reported cannot disagree with
	// the comparison it explains. It costs one map and no query: the pass
	// already has every monitored row in hand and already walks them. It counts
	// MONITORED rows only, because it is incremented under the same opt-out
	// filter below that the live set is — see dropAbsentBindingsFromMemory for
	// what that means to a reader of the line.
	registryRows := make(map[string]int, len(entries))
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
		registryRows[e.PayloadCode]++
	}

	open, err := m.eng.db.ListOpenThresholdEpisodes()
	if err != nil {
		m.eng.logFn("demand_reconciler: list open threshold episodes: %v", err)
		return 0
	}
	candidates := make(map[string]openEpisodeRef, len(open))
	// The display form of each binding, built here rather than parsed back out
	// of the key later. bindingKey is station|node|payload, so splitting it is
	// correct right up until one of the three fields contains the separator, and
	// the failure would be a log line naming the wrong loader in the middle of an
	// incident. openEpisodeRef carries the station for the same reason and
	// deliberately does not carry the node; this is where the node is still in
	// hand.
	labels := make(map[string]string, len(open))
	for _, o := range open {
		key := bindingKey(o.StationID, o.CoreNodeName, o.PayloadCode)
		candidates[key] = openEpisodeRef{
			originID: o.OriginID, stationID: o.StationID, payloadCode: o.PayloadCode,
		}
		labels[key] = o.StationID + "/" + o.CoreNodeName
	}
	closed := m.closeThresholdEpisodesNotIn(candidates, live, protocol.ClosedBySweep)
	m.dropAbsentBindingsFromMemory(closed, candidates, labels, registryRows)
	return len(closed)
}

// dropAbsentBindingsFromMemory is the other half of the sweep's reconciliation:
// having closed the episodes of bindings the database does not have, it takes
// those bindings OUT OF THE MONITOR'S MEMORY, by rebuilding the affected
// payloads from the database.
//
// WHY THE CLOSE ALONE IS NOT A FIX. reconcileThresholdBindings treats
// demand_registry as the truth about which bindings exist — that is the whole
// premise — and then leaves thresholdsByPayload holding a binding that truth
// says is gone. closeThresholdEpisodeRef clears belowThresholdSince on its way
// out, which re-arms the falling edge, so the next delta for that payload mints
// the same demand again and the next pass closes it again. One withdrawn config
// renders as a stream of instantaneous demands, which is the exact failure the
// episode grain was built to end.
//
// IT IS THE REBUILD THE NOTIFICATION DOORS ALREADY DO, not a second one:
// rebuildPayloadBindings, shared with engagePayloads. Two implementations of
// "make memory agree with demand_registry for this payload" would answer
// differently the first time one of them learned something the other did not,
// and the scoping lesson in staleEpisodeKeys is exactly the kind of thing that
// gets learned once. That function also carries the part that is easy to get
// wrong: LookupDemandThresholdsByPayload is PLANT-WIDE, so a payload bound at
// two stations that loses one of them keeps the other's binding, and the
// comparison is by key rather than by "does this payload have bindings left".
//
// THE REBUILD HALF ONLY, AND THAT IS THE WHOLE OF WHAT THIS PASS MAY DO. It
// used to call engagePayloads, which rebuilds AND THEN EVALUATES — reads the
// authoritative in-loop total and creates replenishment orders for every
// binding below threshold. The rebuild is plant-wide, so the bindings that
// evaluation reached were mostly healthy ones at other loaders that this pass
// had never been asked about; and when the read fails it deliberately falls
// through to a total of 0, which is below every threshold there is. A
// sixty-second timer that answers one Postgres blip by ordering material for
// every payload it happened to touch is not a floor under anything. A
// reconciling sweep keeps the record straight: it never creates an order and it
// never evaluates a level. Surviving bindings are evaluated by the next delta,
// as they always were — which is the same rule reconcileThresholdBindings
// already states about the close, for the same reason. The half that stays
// behind is evaluateRebuiltBindings.
//
// AFTER THE CLOSES, NEVER BEFORE, and the order is load-bearing twice. The
// sweep's closes are attributed `by=sweep`, which is what makes the notification
// paths' share measurable; the rebuild's own comparison says `by=notification`
// and would take that attribution away if it got there first. And by the time it
// runs, closeThresholdEpisodeRef has already dropped each closed key from
// openOrigins, so the candidate set the rebuild builds cannot contain a row
// this pass just closed — no episode is closed twice. The one thing it CAN find
// is a different origin under the same key, minted by a delta that raced between
// this pass's ListOpenThresholdEpisodes and its close; closing that one is
// correct, because its binding is absent too.
//
// ONLY ON AN ACTUAL CLOSE. A pass that found nothing stale does no lookup and
// prints nothing, so a plant whose notification paths all work pays nothing for
// the floor — and the line below stays a finding rather than a heartbeat.
//
// WHAT THE LINE SAYS, AND WHY THE REASONING IS HERE RATHER THAN IN IT. The line
// is four fields and one clause; this comment is the explanation, and that split
// is deliberate on both counts. Springfield's Core journal is 3.5 GB and an
// unbounded scan of it takes about forty minutes, and this line fires once per
// affected payload per sweep for as long as the mismatch lasts — the 2026-08-19
// burst ran over nineteen hours. A paragraph per pass is a meaningful
// contribution to the file the next diagnosis has to grep. It also has to be
// skimmable beside its neighbours, which are all of the DEMAND OPENED /
// NEGATIVE COUNT shape: key=value fields a reader's eye can run down.
//
// registry_rows IS THE FACT THE 2026-08-19 JOURNAL DID NOT HAVE. The burst
// showed 411 opens, 411 distinct origins and 405 closes for bindings whose
// station, node, payload and threshold matched demand_registry exactly, and
// nothing recorded whether the ROWS were there at the moment the comparison
// decided they were not. So the count is carried: 0 means this payload's
// bindings are gone plant-wide, and a non-zero count means it is still bound at
// another loader and only the listed bindings went. Those are different plants
// and they want different investigations.
//
// It counts MONITORED rows — replenish_uop_threshold > 0 — because it is
// incremented under the same opt-out filter the live set is built under. A row
// whose threshold is 0 is the documented opt-out, not watched by anything, so
// counting it would make a payload nobody monitors read as still bound. The
// consequence for a reader is that a bare `SELECT count(*) FROM demand_registry`
// can legitimately return a larger number.
//
// absent_bindings IS A LIST, NOT A SCALAR station FIELD, because the several-
// bindings case is the one that happened: the onset of the 2026-08-19 burst was
// a whole-station absence that closed two long-open episodes on two different
// nodes in the same instant. A field whose meaning depends on how many there are
// is the ambiguity that made that journal unreadable in the first place. Sorted,
// for the same reason staleEpisodeKeys sorts — two journals of one incident have
// to diff.
func (m *ThresholdMonitor) dropAbsentBindingsFromMemory(closed []string, candidates map[string]openEpisodeRef, labels map[string]string, registryRows map[string]int) {
	if len(closed) == 0 || m.eng == nil || m.eng.db == nil {
		return
	}
	absent := make(map[string][]string, len(closed))
	for _, key := range closed {
		payload := candidates[key].payloadCode
		absent[payload] = append(absent[payload], labels[key])
	}
	// Sorted, for the same reason staleEpisodeKeys sorts: a pass that announces
	// several payloads at once must announce them in the same order next time,
	// or two journals of the same incident will not diff.
	payloads := make([]string, 0, len(absent))
	for payload := range absent {
		payloads = append(payloads, payload)
	}
	sort.Strings(payloads)
	// ANNOUNCE AND REBUILD IN THE SAME LOOP, so the line is immediately followed
	// by the rebuild it describes. It also puts the rebuilds in the sorted order
	// the announcement is in, which a map-keyed set could not promise — the same
	// two-journals-must-diff argument as above, applied to whatever the rebuild
	// itself logs.
	for _, payload := range payloads {
		m.eng.logFn("demand_reconciler: STALE BINDING IN MEMORY payload=%s absent_bindings=%s registry_rows=%d closed=%d — monitor held bindings with no demand_registry row; episodes closed and the payload's bindings rebuilt from the database",
			payload, strings.Join(absent[payload], ","), registryRows[payload], len(absent[payload]))
		m.rebuildPayloadBindings(payload)
	}
}

// closeThresholdEpisodesForChangedBindings ends the episodes whose binding was
// edited — the denominator moved, or the binding stopped existing.
//
// A threshold change closes the episode and lets the next evaluation open a new
// one. Continuing the old episode across the change would make its cost_ratio a
// division by a number that was never in force for most of its life — the
// episode would be measured against a threshold nobody was using. Both rows
// then record the transition honestly, and OnThresholdChanges already carries
// OldThreshold/NewThreshold for whoever wants to read it back.
//
// A NEW THRESHOLD OF ZERO IS NOT A NEW DENOMINATOR, and the reason has to be
// picked accordingly. SyncRegistry reports a binding that vanished as a change
// to zero — that is how a retired loader arrives here — and zero is also the
// documented opt-out, under which Core stops watching the pair entirely. Either
// way the place is no longer watched, which is `threshold_removed`: the need did
// not recover and nothing is measuring it against a new number. It is the same
// word reconcileThresholdBindings uses when it finds the same state by sweeping,
// and one vocabulary for one fact about the plant is what lets closed_by
// measure which mechanism is doing the work.
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
		m.closeThresholdEpisode(
			bindingKey(c.StationID, c.CoreNodeName, c.PayloadCode),
			reason, protocol.ClosedByNotification)
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
		m.openOrigins[key] = openEpisodeRef{originID: o.OriginID, stationID: o.StationID, payloadCode: o.PayloadCode}
		m.belowThresholdSince[key] = o.OpenedAt
	}
	n := len(open)
	m.mu.Unlock()
	if n > 0 {
		m.eng.logFn("threshold_monitor: rehydrated %d open demand episode(s) across restart", n)
	}
}
