package engine

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/dispatch/binresolver"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// maintainer.go — the maintained-group level keeper.
//
// A maintained group is a node group whose EMPTY-CARRIER level Core holds: so
// many unclaimed carriers of each declared type, at all times, near the
// equipment that consumes them. This is the thing that holds it.
//
// ── A PLAIN PERIODIC TICK, AND EMPHATICALLY NOT THE THRESHOLD MONITOR ───────
//
// The threshold monitor is event-driven with a debounce, a warm-up, and an
// in-memory below-since map, because a LEVEL has no memory and minting on one
// would produce a fresh "demand" every window. None of that machinery is
// imported here, and the reason is not taste:
//
//   - Its event path is structurally blind to empties (evaluatePayload
//     short-circuits on a blank payload code), so it could not see this demand
//     even if it were asked to.
//   - CORRECT SUBTRACTION MAKES RE-RUNNING HARMLESS. This tick subtracts what it
//     has already asked for from what it wants, so running it twice produces the
//     same answer as running it once. A debounce exists to damp a miscounting
//     loop; count properly and there is nothing to damp.
//
// ── ZERO IN-MEMORY EPISODE STATE ───────────────────────────────────────────
//
// No openOrigins map, no belowThresholdSince, no rehydrate-on-boot. The open set
// is read from the database every tick, which makes the entire
// restart-duplication failure class UNREACHABLE rather than handled: a restart
// is indistinguishable from an ordinary tick, because the tick never believed
// anything it had not just read. Round 2 chose this over map-and-rehydrate, and
// it is why the restart test is cheap.
//
// ── LIVE-CAPABLE, GATED ONLY BY CONFIG ─────────────────────────────────────
//
// There is no shadow flag. A group is maintained when its own maintain_enabled
// property says so, which no production group has set until an owner sets it.
// The per-tick subtraction line is the floor diagnostic, not a mode.

// maintainerTickInterval is the cadence. Slow on purpose: this is a level
// keeper, not a reflex. A carrier takes minutes to arrive, the subtraction
// already accounts for what is coming, and a faster tick would buy nothing but
// log volume.
const maintainerTickInterval = 30 * time.Second

// MaintainerGroupState is one (group, type) line of Snapshot() — what the
// replenishment-health page renders.
//
// PARKED-NESS IS DERIVED, NEVER STORED. It is read from the live asks' queue
// causes at render time. A stored status is the uopCache lesson: a rollup drifts
// from what it summarises the moment anything else can move underneath it, and
// "this intent is parked" is exactly the kind of fact that goes stale silently.
type MaintainerGroupState struct {
	GroupNode   string    `json:"group_node"`
	BinTypeCode string    `json:"bin_type_code"`
	Want        int       `json:"want"`
	Resident    int       `json:"resident"`
	Asked       int       `json:"asked"`
	Coming      int       `json:"coming"`
	Gap         int       `json:"gap"`
	Created     int       `json:"created"`
	HeldBy      string    `json:"held_by,omitempty"`
	OriginID    string    `json:"origin_id,omitempty"`
	OpenedAt    time.Time `json:"opened_at,omitempty"`
	// OldestAskCause is the queue cause of this intent's longest-waiting ask,
	// blank when nothing is parked. This IS the parked-ness signal.
	OldestAskCause string `json:"oldest_ask_cause,omitempty"`
	OldestAskAge   string `json:"oldest_ask_age,omitempty"`
}

// Maintainer holds every maintained group's declared level.
type Maintainer struct {
	eng *Engine
	now func() time.Time
	// interval is a field so a test or the sim can drive the cadence.
	interval time.Duration

	// mu guards the snapshot ONLY. There is no episode state to guard.
	mu       sync.Mutex
	snapshot []MaintainerGroupState
	ticks    int
}

// NewMaintainer builds the keeper. now is injected so the sim can drive cadence.
func NewMaintainer(eng *Engine, now func() time.Time) *Maintainer {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Maintainer{eng: eng, now: now, interval: maintainerTickInterval}
}

// Run ticks until ctx is cancelled.
func (m *Maintainer) Run(ctx context.Context) {
	go func() {
		t := time.NewTicker(m.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.Tick()
			}
		}
	}()
}

// Snapshot returns the last tick's per-(group, type) state.
func (m *Maintainer) Snapshot() []MaintainerGroupState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MaintainerGroupState, len(m.snapshot))
	copy(out, m.snapshot)
	return out
}

// Ticks reports how many passes have completed.
func (m *Maintainer) Ticks() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ticks
}

// Tick is one full pass over every maintained group.
//
// PURE READ UNTIL IT DECIDES. Every count comes from the database, nothing is
// remembered between passes, and a pass that decides to create nothing has
// written nothing.
func (m *Maintainer) Tick() {
	if m.eng == nil || m.eng.db == nil {
		return
	}

	allNodes, err := m.eng.db.ListNodes()
	if err != nil {
		// Not evidence that no group is maintained. Leaving every level alone is
		// the honest outcome; the next tick asks again.
		m.eng.logFn("maintainer: list nodes: %v", err)
		return
	}

	// The open set, read fresh. This is the keeper's entire memory.
	open, err := m.eng.db.ListOpenEpisodesOfKind(protocol.EpisodeKindMaintain)
	if err != nil {
		m.eng.logFn("maintainer: list open episodes: %v", err)
		return
	}
	openByKey := make(map[string]store.DemandOrigin, len(open))
	for _, o := range open {
		openByKey[o.EpisodeKey] = o
	}

	var states []MaintainerGroupState
	// configuredKeys is every episode key the CONFIG still calls for. Anything
	// open and absent from it is config-withdrawn — see closeWithdrawn.
	configuredKeys := map[string]bool{}

	for _, g := range allNodes {
		if !g.IsSynthetic || !g.Enabled {
			continue
		}
		if m.eng.db.GetNodeProperty(g.ID, nodes.PropMaintainEnabled) != "on" {
			continue
		}
		station := m.eng.db.GetNodeProperty(g.ID, nodes.PropMaintenanceStation)
		if station == "" {
			// Config save refuses this (MG1-4), so reaching it means the property
			// was written by another path. Refuse to run rather than mint an order
			// projectOrder would silently drop — an order invisible on every Edge
			// board is the phantom-order family.
			m.eng.logFn("maintainer: group %s is maintained with no maintenance_station — skipped; "+
				"its asks would be invisible on every Edge board", g.Name)
			continue
		}
		levels, lerr := m.eng.db.ListMaintainLevels(g.ID)
		if lerr != nil {
			m.eng.logFn("maintainer: list levels for %s: %v", g.Name, lerr)
			continue
		}
		for _, lv := range levels {
			key := protocol.MaintainEpisodeKey(g.Name, lv.BinTypeCode)
			configuredKeys[key] = true
			states = append(states, m.tickOne(g, lv, station, key, openByKey[key]))
		}
	}

	m.closeWithdrawn(open, configuredKeys)

	sort.Slice(states, func(i, j int) bool {
		if states[i].GroupNode != states[j].GroupNode {
			return states[i].GroupNode < states[j].GroupNode
		}
		return states[i].BinTypeCode < states[j].BinTypeCode
	})
	m.mu.Lock()
	m.snapshot = states
	m.ticks++
	m.mu.Unlock()
}

// tickOne is the subtraction and the decision for one (group, type).
func (m *Maintainer) tickOne(g *nodes.Node, lv store.MaintainLevel, station, key string,
	episode store.DemandOrigin) MaintainerGroupState {

	st := MaintainerGroupState{
		GroupNode: g.Name, BinTypeCode: lv.BinTypeCode, Want: lv.Want,
		OriginID: episode.OriginID, OpenedAt: episode.OpenedAt,
	}

	// ── THE THREE POPULATIONS ───────────────────────────────────────────────
	// Kept distinct because they answer different questions and one of them is
	// not the keeper's doing. Collapsing any two is how a level over- or
	// under-fills with nothing reporting an error.
	resident, err := m.eng.db.CountEmptyBinsOfTypeInGroup(lv.BinTypeCode, g.ID)
	if err != nil {
		m.eng.logFn("maintainer: count residents %s/%s: %v", g.Name, lv.BinTypeCode, err)
		return st
	}
	asked := 0
	if episode.OriginID != "" {
		asked, err = m.eng.db.CountLiveRootsByOrigin(episode.OriginID)
		if err != nil {
			m.eng.logFn("maintainer: count asks %s/%s: %v", g.Name, lv.BinTypeCode, err)
			return st
		}
	}
	coming, err := m.eng.db.CountTypedInboundToGroup(g.ID, g.Name, lv.BinTypeCode)
	if err != nil {
		m.eng.logFn("maintainer: count inbound %s/%s: %v", g.Name, lv.BinTypeCode, err)
		return st
	}

	gap := lv.Want - resident - asked - coming
	st.Resident, st.Asked, st.Coming, st.Gap = resident, asked, coming, gap

	switch {
	case gap >= 1:
		// Mint the episode if this is the first tick that has been short. The
		// INSERT is the duplicate guard, not a lock: two Cores racing here both
		// try, the loser fails on the partial unique open-key index, and a failed
		// mint stamps nothing so the next tick retries.
		if episode.OriginID == "" {
			origin := store.DemandOrigin{
				OriginID:     uuid.NewString(),
				EpisodeKey:   key,
				Kind:         protocol.EpisodeKindMaintain,
				StationID:    station,
				CoreNodeName: g.Name,
				OpenedAt:     m.now().UTC(),
				OpenedTotal:  resident,
				Threshold:    lv.Want,
			}
			if oerr := m.eng.db.OpenCoreEpisode(origin, false); oerr != nil {
				m.eng.logFn("maintainer: open episode %s: %v", key, oerr)
				return st
			}
			episode = origin
			st.OriginID, st.OpenedAt = origin.OriginID, origin.OpenedAt
			m.eng.logFn("maintainer: DEMAND OPENED origin=%s group=%s type=%s want=%d resident=%d",
				origin.OriginID, g.Name, lv.BinTypeCode, lv.Want, resident)
		}
		st.Created, st.HeldBy = m.createAsks(g, lv, station, episode, gap)

	case resident >= lv.Want && asked == 0:
		// ── THE SETTLE EDGE, NOT THE RISING EDGE ──────────────────────────
		//
		// Closing the moment the level touches want re-opens the duplicate
		// window: asks are still in flight, the episode closes, the level dips, a
		// NEW origin mints, and CountLiveRootsByOrigin(new) is zero — so the
		// keeper re-asks for carriers already on their way. That is the
		// 241-duplicates shape arriving through the close.
		//
		// Requiring BOTH conditions makes "never a live ask on a closed episode"
		// structural. The cost is accepted and stated: a permanently unsourceable
		// ask holds its episode open forever, which is the honest record and the
		// row the operator surface renders.
		if episode.OriginID != "" {
			m.closeEpisode(episode, protocol.CloseReasonRecovered, g.Name, lv.BinTypeCode)
			st.OriginID, st.OpenedAt = "", time.Time{}
		}
	}

	// Parked-ness, derived. The oldest live ask's queue cause is the signal —
	// nothing is stored, and an intent with no parked ask simply has no cause.
	if episode.OriginID != "" {
		if cause, age, ok := m.oldestAskCause(episode.OriginID); ok {
			st.OldestAskCause = cause
			st.OldestAskAge = age.Round(time.Second).String()
		}
	}

	// ONE LINE PER TICK PER GROUP, ALWAYS — the full chain, not "would create N".
	// A reader has to see WHY the number came out as it did, and every term is a
	// separate question with a separate way of being wrong.
	m.eng.dbg("maintainer: group=%s type=%s want=%d resident=%d asked=%d coming=%d gap=%d created=%d held_by=%s",
		g.Name, lv.BinTypeCode, lv.Want, resident, asked, coming, gap, st.Created, st.HeldBy)

	return st
}

// createAsks runs the pre-resolve loop: one ask per free typed slot, bounded by
// the resolver rather than by a clamp.
//
// PRE-RESOLVE, AND THE THREE THINGS IT BUYS (round 2's ruling over
// derive-at-admit): resolveSyntheticDestination becomes a no-op for these orders
// because the destination is already concrete; the never-re-resolved window is
// unreachable by construction, since planTransport's re-resolution gate never
// sees a group-named delivery from here; and the ask count is bounded by PHYSICAL
// FREE SLOTS through ResolveStore's own per-child count+inflight>=1 check — the
// proven ReplenishLoader shape. A clamp above that loop would be unreachable code
// dressed as a guard.
//
// N ASKS, NOT ONE. Serial filling of a four-deep buffer takes four drive-times,
// which defeats the point of a rate-mismatch buffer.
func (m *Maintainer) createAsks(g *nodes.Node, lv store.MaintainLevel, station string,
	episode store.DemandOrigin, gap int) (created int, heldBy string) {

	resolver := m.eng.maintainerResolver()
	if resolver == nil {
		return 0, "no resolver"
	}
	binTypeID := lv.BinTypeID
	for i := 0; i < gap; i++ {
		res, err := resolver.ResolveStore(g, "", &binTypeID, reservations.Anyone)
		if err != nil || res == nil || res.Node == nil {
			// QUEUE-ON-FULL IS A NORMAL OUTCOME, NEVER AN ERROR. The group has no
			// free typed slot this tick; the level stays short, the episode stays
			// open, and the next tick asks again.
			if err != nil {
				return created, err.Error()
			}
			return created, "no free slot"
		}
		order, aerr := m.eng.dispatcher.AdmitCoreAsk(dispatch.CoreAskSpec{
			UUIDPrefix: "core-mnt-",
			StationID:  station,
			// No source node: the ask is plant-scoped and the finder's tiers
			// choose where the carrier comes from. Naming the group here would
			// make the keeper source from the group it is trying to fill.
			SourceNode:   "",
			DeliveryNode: res.Node.Name,
			// THE BOARD CARD, and it names the GROUP rather than only the type.
			// DeliveryNode beside it is the concrete position the pre-resolve
			// chose (PRESS-BUFFER-A-P02), which is where the robot goes but not
			// what the operator is looking at the board to learn. Group names
			// happen to prefix position names in the demo plant; nothing
			// enforces it, so the card says it rather than relying on it.
			PayloadDesc: "empty " + lv.BinTypeCode + " → " + g.Name,
			OriginID:    episode.OriginID,
			OriginClass: protocol.OriginClassAttached,
		})
		if aerr != nil {
			m.eng.logFn("maintainer: admit ask for %s/%s at %s: %v",
				g.Name, lv.BinTypeCode, res.Node.Name, aerr)
			return created, "admit refused"
		}
		m.eng.dispatcher.QueueCoreAsk(order, station)
		created++
		m.eng.logFn("maintainer: ASK order=%d uuid=%s group=%s type=%s slot=%s origin=%s",
			order.ID, order.EdgeUUID, g.Name, lv.BinTypeCode, res.Node.Name, episode.OriginID)
	}
	return created, heldBy
}

// closeEpisode ends one intent.
func (m *Maintainer) closeEpisode(episode store.DemandOrigin, reason, group, binType string) {
	closed, err := m.eng.db.CloseDemandOriginByID(episode.OriginID, reason,
		protocol.ClosedByNotification, m.now().UTC())
	if err != nil {
		m.eng.logFn("maintainer: close episode %s: %v", episode.OriginID, err)
		return
	}
	if !closed {
		return
	}
	m.eng.logFn("maintainer: DEMAND CLOSED origin=%s group=%s type=%s reason=%s",
		episode.OriginID, group, binType, reason)
}

// closeWithdrawn closes every open maintain episode the CONFIG no longer calls
// for — a level line deleted, or a group disabled.
//
// THE PRECONDITION IS THE CONFIG, NOT THE LEVEL, the same distinction
// reconcileThresholdBindings draws. The settle edge already owns the level close
// and owns it well; a second opinion on that question would race it, closing
// episodes the next tick re-opens. What no other path owns is a declaration that
// vanished.
//
// A READ FAILURE IS NOT AN EMPTY CONFIG SET. Tick returns early on either list
// error before reaching here, so this is only ever called with a set that was
// actually read — otherwise a transient database blip would close every open
// intent in the plant as though somebody had deleted the configuration.
// THE SET DIFFERENCE IS SHARED with the threshold paths — staleEpisodeKeys —
// rather than written a second time here. The lesson it carries (compare live
// keys, not "is the set empty") was learned once on the threshold side, and a
// separate copy would be free to un-learn it.
func (m *Maintainer) closeWithdrawn(open []store.DemandOrigin, configured map[string]bool) {
	// Keyed by episode key, which is lossless: the partial unique open-key index
	// permits exactly one OPEN episode per key, so two rows here cannot collide.
	candidates := make(map[string]store.DemandOrigin, len(open))
	for _, o := range open {
		candidates[o.EpisodeKey] = o
	}
	for _, key := range staleEpisodeKeys(candidates, configured) {
		o := candidates[key]
		// threshold_removed, not recovered: the need did not get its carriers, it
		// stopped being watched. Reporting a satisfied demand every time somebody
		// deletes a level is the conflation claim_removed exists to prevent on
		// the cell side.
		m.closeEpisode(o, protocol.CloseReasonThresholdRemoved, o.CoreNodeName, "")
	}
}

// oldestAskCause reads the parked-ness signal off the live asks.
func (m *Maintainer) oldestAskCause(originID string) (string, time.Duration, bool) {
	live, _, err := m.eng.db.ListOrdersByOrigin(originID, 200)
	if err != nil || len(live) == 0 {
		return "", 0, false
	}
	var oldest *orders.Order
	for _, o := range live {
		if protocol.IsTerminal(protocol.Status(o.Status)) || o.QueueCause == "" {
			continue
		}
		if oldest == nil || o.CreatedAt.Before(oldest.CreatedAt) {
			oldest = o
		}
	}
	if oldest == nil {
		return "", 0, false
	}
	return oldest.QueueCause, m.now().UTC().Sub(oldest.CreatedAt), true
}

// maintainerResolver builds the slot chooser.
//
// THE SAME RESOLVER INTAKE USES, deliberately — a slot the keeper picks is one
// the ordinary path would have picked, and the per-child count+inflight>=1 check
// that bounds the pre-resolve loop is that resolver's own, not a second opinion
// written here.
func (e *Engine) maintainerResolver() *binresolver.GroupResolver {
	if e.db == nil {
		return nil
	}
	return &binresolver.GroupResolver{DB: e.db, DebugLog: e.dbg}
}

// MaintainedGroupStates is the accessor the www layer reads. It exists so www
// never has to reach through Maintainer() into engine internals, and so a Core
// running without a keeper answers "nothing maintained" rather than panicking.
func (e *Engine) MaintainedGroupStates() []MaintainerGroupState {
	if e == nil || e.maintainer == nil {
		return nil
	}
	return e.maintainer.Snapshot()
}
