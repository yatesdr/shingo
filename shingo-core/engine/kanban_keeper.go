package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	_ "sync" // the plain lock inlines only where sync is imported (protocol/lockcheck)
	"time"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/lockcheck"
	"shingocore/dispatch"
	"shingocore/internal/lockorder"
	"shingocore/kanban"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// kanban_keeper.go — the kanban level keeper.
//
// A kanban is a node group whose EMPTY-CARRIER level Core holds: so
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
// There is no shadow flag and no on/off: a group with levels, supports and a
// station has a keeper. The per-tick subtraction line is the floor diagnostic, not
// a mode.
//
// ── THE KNOWN EXPOSURE: A WIPED CORE, FOR AT MOST ONE HEARTBEAT ────────────
//
// The keeper's empties come from the shared empty finders, whose cell arm keeps
// a carrier on any of a claim's positions (core, paired, second paired) out of
// the pool. The only Core record that ties a cell position to its Edge is the
// claims mirror the Edges publish; node_stations is an admin allow-list, empty
// on every rig plant, and a node carries no kind. A Core restart that keeps its
// database keeps the mirror and is not exposed. A WIPED Core has no mirror until
// each Edge re-registers and republishes its claims, which it does on every
// registration (shingo-edge/cmd/shingoedge/main.go): at most one heartbeat,
// about 60 s. Inside that window a cell's carrier reads as a free empty, and the
// consequence is one recoverable move. Accepted, with no guard (the orc's
// ruling on the B3 close-out, stop 6): a wiped Core is an incident, not an
// operating mode, and keying the arm on node_stations would guard nothing on the
// rig and the wrong nodes at a plant.

// keeperTickInterval is the cadence. Slow on purpose: this is a level
// keeper, not a reflex. A carrier takes minutes to arrive, the subtraction
// already accounts for what is coming, and a faster tick would buy nothing but
// log volume.
const keeperTickInterval = 30 * time.Second

// KanbanLineState is one (group, type) line of Snapshot() — what the keeper's
// last tick on the Nodes page renders (nodes-kanban.js renderKeeperLevels).
//
// PARKED-NESS IS DERIVED, NEVER STORED. It is read from the live asks' queue
// causes at render time. A stored status is the uopCache lesson: a rollup drifts
// from what it summarises the moment anything else can move underneath it, and
// "this intent is parked" is exactly the kind of fact that goes stale silently.
type KanbanLineState struct {
	GroupNode   string `json:"group_node"`
	BinTypeCode string `json:"bin_type_code"`
	// Want is the effective want (kanban.Level.Want), Base the level declared.
	Want     int `json:"want"`
	Base     int `json:"base"`
	Resident int `json:"resident"`
	Asked    int `json:"asked"`
	Coming   int `json:"coming"`
	// Held is the carts of the type on the line with no order on them, Lit those at the
	// windows the walk lights as it. Gap is the shortfall net of both.
	Held int `json:"held"`
	Lit  int `json:"lit"`
	Gap  int `json:"gap"`
	// Created, OriginID and OpenedAt are for the tick's own tests and logs: the
	// card reads none of them.
	Created  int       `json:"-"`
	OriginID string    `json:"-"`
	OpenedAt time.Time `json:"-"`
	HeldBy   string    `json:"held_by,omitempty"`
	// RunningProcesses are the processes the cover counts for this type: what is
	// driving the want above its base.
	RunningProcesses []string `json:"running_processes,omitempty"`
	// NothingProcesses are the kanban's processes with a part that contributes no
	// type, FallbackProcesses those counting on their active style because their
	// changeover's target resolves to no type.
	NothingProcesses  []string `json:"nothing_processes,omitempty"`
	FallbackProcesses []string `json:"fallback_processes,omitempty"`
	// OldestAskCause is the queue cause of this intent's longest-waiting ask,
	// blank when nothing is parked. This IS the parked-ness signal.
	OldestAskCause string `json:"oldest_ask_cause,omitempty"`
	// OldestAskReason is that ask's queue_reason: the operator sentence the
	// dispatcher wrote from its queue code (FormatQueueSentence). The cause is
	// the engineer's tag; this is what the Nodes page's keeper's levels print.
	OldestAskReason string `json:"oldest_ask_reason,omitempty"`
	// OldestAskAge is rendered on the one duration ladder (protocol.FormatDuration),
	// not Go's "11m3s".
	OldestAskAge string `json:"oldest_ask_age,omitempty"`
	// SurplusCause is why a cart of this type stands above its want while a type of its
	// family is short (engine/kanban_surplus.go). A surplus has no ask, so it has no oldest
	// ask to name.
	SurplusCause string `json:"surplus_cause,omitempty"`
}

// KanbanKeeper holds every kanban's effective want.
type KanbanKeeper struct {
	eng *Engine
	now func() time.Time
	// interval is a field so a test or the sim can drive the cadence.
	interval time.Duration

	// mu guards the snapshot ONLY. There is no episode state to guard.
	mu       lockcheck.Mutex[lockorder.KanbanKeeper]
	snapshot []KanbanLineState
	families []KanbanFamilyState
	ticks    int
}

// NewKanbanKeeper builds the keeper. now is injected so the sim can drive cadence.
func NewKanbanKeeper(eng *Engine, now func() time.Time) *KanbanKeeper {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &KanbanKeeper{eng: eng, now: now, interval: keeperTickInterval}
}

// Run ticks until ctx is cancelled.
func (m *KanbanKeeper) Run(ctx context.Context) {
	go func() {
		t := clock.Default().NewTicker(m.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C():
				m.Tick()
			}
		}
	}()
}

// KanbanFamilyState is one (kanban, family) of the last tick: what the card's family row
// reads. Room is not here: it reads each slot's admits, which the card asks for itself and the
// tick has no reason to.
type KanbanFamilyState struct {
	GroupNode string `json:"group_node"`
	Family    string `json:"family"`
	FamilyID  int64  `json:"family_id"`
	// Pool is what the loop is meant to hold, Minimum what it must to hold the mix and the
	// line. PoolDeclared false says it stands at the minimum because nobody has said.
	Pool         int  `json:"pool"`
	PoolDeclared bool `json:"pool_declared"`
	Minimum      int  `json:"minimum"`
	// Mix is the family's declared levels. Found is where its carts are; a keeper's asks are
	// not in it.
	Mix    int `json:"mix"`
	Slot   int `json:"slot"`
	Line   int `json:"line"`
	Window int `json:"window"`
	Flight int `json:"flight"`
	Found  int `json:"found"`
	Asked  int `json:"asked"`
	// Unowned is carts of Flight on a robot with no live order. Floaters is pool less mix and
	// the line: the carts standing as whatever they came off as.
	Unowned  int `json:"unowned,omitempty"`
	Floaters int `json:"floaters"`
	// Strangers and Lost are found past the pool and short of it, named where a person can act
	// on them.
	Strangers []string `json:"strangers,omitempty"`
	Lost      int      `json:"lost,omitempty"`
}

// FamilySnapshot returns the last tick's per-(kanban, family) state.
func (m *KanbanKeeper) FamilySnapshot() []KanbanFamilyState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]KanbanFamilyState, len(m.families))
	copy(out, m.families)
	return out
}

// Snapshot returns the last tick's per-(group, type) state.
func (m *KanbanKeeper) Snapshot() []KanbanLineState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]KanbanLineState, len(m.snapshot))
	copy(out, m.snapshot)
	return out
}

// Ticks reports how many passes have completed.
func (m *KanbanKeeper) Ticks() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ticks
}

// Tick is one full pass over every kanban.
//
// PURE READ UNTIL IT DECIDES. Every count comes from the database, nothing is
// remembered between passes, and a pass that decides to create nothing has
// written nothing.
//
// THREE STATEMENTS WHEN NOTHING IS OUT, WHATEVER THE PLANT'S SIZE: the picture, the open
// episodes with their asks, one read of the loops. A family short of its pool adds a forecast,
// an asking kanban one read of its slot state, its properties and its children's types, and an
// ask what its birth costs; a type short with a family member above it adds what the arm reads.
// Pin: TestKanbanCost_KeeperTick.
func (m *KanbanKeeper) Tick() {
	if m.eng == nil || m.eng.db == nil {
		return
	}

	picture, err := m.eng.db.ListKanbanPicture(nil)
	if err != nil {
		// Not evidence that no kanban exists. Leaving every level alone is the
		// honest outcome; the next tick asks again.
		m.eng.logFn("keeper: read the kanbans: %v", err)
		return
	}

	// The open set, read fresh. This is the keeper's entire memory.
	open, err := m.eng.db.ListOpenKeeperEpisodes()
	if err != nil {
		m.eng.logFn("keeper: list open episodes: %v", err)
		return
	}
	openByKey := make(map[string]store.KeeperEpisode, len(open))
	for _, o := range open {
		openByKey[o.EpisodeKey] = o
	}

	counts, err := m.eng.db.ReadKanbanCounts(nil)
	if err != nil {
		m.eng.logFn("keeper: count the kanbans: %v", err)
		return
	}

	var states []KanbanLineState
	var families []KanbanFamilyState
	// configuredKeys is every episode key the CONFIG still calls for. Anything
	// open and absent from it is config-withdrawn — see closeWithdrawn.
	configuredKeys := map[string]bool{}
	asked := false

	for from := 0; from < len(picture); {
		to := from
		for to < len(picture) && picture[to].KanbanNodeID == picture[from].KanbanNodeID {
			to++
		}
		got, fams, created := m.tickKanban(picture[from:to], openByKey, counts, configuredKeys)
		states = append(states, got...)
		families = append(families, fams...)
		asked = asked || created
		from = to
	}

	m.closeWithdrawn(open, configuredKeys)

	// An ask born this tick may have parked as it was born: the card reads its
	// cause now, not a tick from now. One read, only on a tick that asked.
	if asked {
		m.refreshParked(states)
	}

	sort.Slice(states, func(i, j int) bool {
		if states[i].GroupNode != states[j].GroupNode {
			return states[i].GroupNode < states[j].GroupNode
		}
		return states[i].BinTypeCode < states[j].BinTypeCode
	})
	m.mu.Lock()
	m.snapshot = states
	m.families = families
	m.ticks++
	m.mu.Unlock()
}

// tickKanban is one kanban's lines and families, and whether it asked. A disabled group has no
// lines. A kanban runs a keeper when it has supports and a station. With no supports it declares
// what it holds and other kanbans' unloaders refill it, so it counts and shows its shortfall,
// and opens and asks nothing. With supports and no station its asks would be invisible on every
// Edge board (the phantom-order family): the save refuses that, a migrated row can hold it, and
// it is skipped out loud. Pins: TestKanban_ANoSupportsKanbanRunsNoKeeper,
// TestKanbanKeeper_RefusesAGroupWithNoStation.
//
// TWO GRAINS. The keeper keeps the loop: it asks outside only for the family's deficit, pool
// less found less asked. A type short inside a whole family is never an outside ask, it is the
// arm's: a keeper that asked per type would raid a supermarket through every changeover and end
// the loop past its pool.
func (m *KanbanKeeper) tickKanban(rows []store.KanbanPictureRow, openByKey map[string]store.KeeperEpisode,
	counts *store.KanbanCounts, configured map[string]bool) ([]KanbanLineState, []KanbanFamilyState, bool) {

	k := rows[0]
	if !k.Live {
		return nil, nil, false
	}
	supported := len(k.Supports) > 0
	keeper := supported && k.StationID != ""
	if supported && !keeper {
		m.eng.logFn("keeper: kanban %s has supports and no station — skipped; "+
			"its asks would be invisible on every Edge board", k.KanbanName)
	}

	lines := make([]keeperLine, len(rows))
	residents := make(map[string]int, len(rows))
	levels := store.LevelsOfPicture(rows)
	walkLines := make([]kanban.Line, len(rows))
	typeFamilies := make(map[string]int64, len(rows))
	for i, r := range rows {
		key := protocol.KanbanEpisodeKey(k.KanbanNodeID, r.BinTypeID)
		episode := openByKey[key]
		c := counts.Level(levels[i], episode.Asked)
		lines[i] = keeperLine{key: key, episode: episode, count: c, state: KanbanLineState{
			GroupNode: k.KanbanName, BinTypeCode: r.BinTypeCode, Want: r.Want(), Base: r.Base,
			Resident: c.Resident, Asked: c.Asked, Coming: c.Coming, Held: c.Held,
			OriginID: episode.OriginID, OpenedAt: episode.OpenedAt,
			RunningProcesses: r.RunningProcesses,
			NothingProcesses: r.NothingProcesses, FallbackProcesses: r.FallbackProcesses,
		}}
		residents[r.BinTypeCode] = c.Resident
		walkLines[i] = kanban.LineOf(r.BinTypeCode, r.Want(), c.Have)
		typeFamilies[r.BinTypeCode] = r.FamilyID
		if keeper {
			configured[key] = true
		}
	}
	lit := counts.Lit(k.KanbanNodeID, walkLines, typeFamilies)
	for i := range lines {
		lines[i].atWindows = lit[rows[i].BinTypeCode]
		lines[i].state.Lit = lines[i].atWindows
		lines[i].state.Gap = lines[i].count.Short() - lines[i].atWindows
	}
	families := familyStates(rows, lines, counts)

	var ask *kanbanAsks
	wants := map[string]int{}
	if keeper {
		ask, wants = m.planAsks(k, rows, lines, families, levels, residents)
	}
	created := false
	for i := range lines {
		l := &lines[i]
		switch {
		case keeper:
			m.tickLine(l, ask, rows[i].FamilyID, wants[rows[i].BinTypeCode])
		case supported:
			l.state.HeldBy = "no station: its asks would show on no board"
		case l.state.Gap >= 1:
			l.state.HeldBy = "no supports, so no keeper: other kanbans' unloaders refill it"
		}
		parked(&l.state, l.episode, m.now())
		created = created || l.state.Created > 0
	}
	if keeper {
		for code, cause := range m.surplusArm(k, rows, lines) {
			for i := range lines {
				if lines[i].state.BinTypeCode == code {
					lines[i].state.SurplusCause = cause
				}
			}
		}
	}
	states := make([]KanbanLineState, len(lines))
	for i := range lines {
		st := lines[i].state
		states[i] = st
		// ONE LINE PER TICK PER GROUP, ALWAYS — the full chain, not "would create N".
		// A reader has to see WHY the number came out as it did, and every term is a
		// separate question with a separate way of being wrong.
		m.eng.dbg("keeper: group=%s type=%s want=%d resident=%d held=%d asked=%d coming=%d lit=%d gap=%d created=%d held_by=%s surplus=%s",
			st.GroupNode, st.BinTypeCode, st.Want, st.Resident, st.Held, st.Asked, st.Coming, st.Lit, st.Gap, st.Created, st.HeldBy, st.SurplusCause)
	}
	return states, families, created
}

// familyStates is the family row of each family the kanban levels, and of any family standing
// in its loop that it does not: those are strangers whatever the count.
func familyStates(rows []store.KanbanPictureRow, lines []keeperLine, counts *store.KanbanCounts) []KanbanFamilyState {
	k := rows[0]
	var order []int64
	byID := map[int64]*KanbanFamilyState{}
	for i, r := range rows {
		f := byID[r.FamilyID]
		if f == nil {
			f = &KanbanFamilyState{GroupNode: k.KanbanName, Family: r.FamilyCode, FamilyID: r.FamilyID,
				Pool: r.Pool, PoolDeclared: r.PoolDeclared, Minimum: r.Minimum}
			byID[r.FamilyID] = f
			order = append(order, r.FamilyID)
		}
		f.Mix += r.Base + r.TopsAndBottomsWant
		f.Asked += lines[i].count.Asked
	}
	carts := counts.Carts(k.KanbanNodeID)
	for _, c := range carts {
		if byID[c.FamilyID] == nil && !c.Asked {
			byID[c.FamilyID] = &KanbanFamilyState{GroupNode: k.KanbanName, Family: c.FamilyCode, FamilyID: c.FamilyID, PoolDeclared: true}
			order = append(order, c.FamilyID)
		}
	}
	out := make([]KanbanFamilyState, 0, len(order))
	for _, id := range order {
		f := byID[id]
		found := counts.Found(k.KanbanNodeID, id)
		f.Slot, f.Line, f.Window, f.Flight, f.Unowned = found.Slot, found.Line, found.Window, found.Flight, found.Unowned
		f.Found = found.Total()
		f.Floaters = max(0, f.Pool-f.Mix-f.Line)
		if f.Found < f.Pool {
			f.Lost = f.Pool - f.Found
		}
		if past := f.Found - f.Pool; past > 0 {
			for _, c := range carts {
				if c.FamilyID == id && !c.Asked && c.Place == store.LoopPlaceSlot && c.Into == "" && len(f.Strangers) < past {
					f.Strangers = append(f.Strangers, c.Label+" stands at "+c.Node)
				}
			}
		}
		out = append(out, *f)
	}
	return out
}

// familyDeficits is each family's deficit, pool less found less asked, where it has one.
func familyDeficits(rows []store.KanbanPictureRow, lines []keeperLine, families []KanbanFamilyState) map[int64]int {
	out := map[int64]int{}
	for _, f := range families {
		if d := f.Pool - f.Found - f.Asked; d > 0 {
			out[f.FamilyID] = d
		}
	}
	return out
}

// askByLargestGap names the type of each ask: the family's largest per-type gap, ties by the
// levels' order, as long as the forecast says one exists outside. The asks never pass the
// family's deficit or the carts there are.
func askByLargestGap(rows []store.KanbanPictureRow, lines []keeperLine, deficit map[int64]int, sourceable map[string]int) map[string]int {
	out := map[string]int{}
	gap := make([]int, len(lines))
	for i := range lines {
		gap[i] = lines[i].state.Gap
	}
	for family, d := range deficit {
		for ; d > 0; d-- {
			best := -1
			for i, r := range rows {
				if r.FamilyID != family || sourceable[r.BinTypeCode]-out[r.BinTypeCode] <= 0 {
					continue
				}
				if best < 0 || gap[i] > gap[best] {
					best = i
				}
			}
			if best < 0 {
				break
			}
			out[rows[best].BinTypeCode]++
			gap[best]--
		}
	}
	return out
}

// keeperLine is one (kanban, type) in a tick.
type keeperLine struct {
	key     string
	episode store.KeeperEpisode
	count   store.LevelState
	state   KanbanLineState
	// atWindows is the carts at the windows the walk lights as this type.
	atWindows int
}

// kanbanAsks is what a family's deficit shares in one tick. Nil: the loop is whole.
type kanbanAsks struct {
	group      *nodes.Node
	station    string
	hasSources bool
	deficit    map[int64]int
	reads      *store.KanbanAskReads
}

// forecast asks, before any ask is minted, the question each ask would put to the
// finder: the fence and the sources its orders carry (no process, the kanban as the
// group it fills), for every type of the kanban, in one statement
// (store.DB.SourceableCounts). A read the finder could not make answers none, and
// the next tick asks again.
func (m *KanbanKeeper) forecast(k store.KanbanPictureRow, codes []string) map[string]int {
	fence := bins.EmptyFence{OriginGroup: k.KanbanName}
	for _, s := range k.Sources {
		fence.Sources = append(fence.Sources, s.ID)
	}
	found, err := m.eng.db.SourceableCounts(codes, fence)
	if err != nil {
		m.eng.logFn("keeper: %s: %v", k.KanbanName, err)
		return nil
	}
	return found
}

// tickLine is the decision for one (kanban, type) with a keeper. want is how many asks the
// family's deficit gives this type.
func (m *KanbanKeeper) tickLine(l *keeperLine, ask *kanbanAsks, family int64, want int) {
	st, c := &l.state, l.count
	lv := c.Level
	deficit := ask != nil && ask.deficit[family] > 0
	switch {
	case deficit && (want > 0 || st.Gap >= 1):
		// Mint the episode if this is the first tick that has been short. The
		// INSERT is the duplicate guard, not a lock: two Cores racing here both
		// try, the loser fails on the partial unique open-key index, and a failed
		// mint stamps nothing so the next tick retries. Threshold is the effective
		// want it opened over, which the episodes page reads as that.
		if l.episode.OriginID == "" {
			origin := store.DemandOrigin{
				OriginID:     uuid.NewString(),
				EpisodeKey:   l.key,
				Kind:         protocol.EpisodeKindKanban,
				StationID:    ask.station,
				CoreNodeName: ask.group.Name,
				OpenedAt:     m.now().UTC(),
				OpenedTotal:  c.Resident,
				Threshold:    lv.Want,
			}
			id, oerr := m.eng.db.OpenCoreEpisode(origin)
			if oerr != nil {
				m.eng.logFn("keeper: open episode %s: %v", l.key, oerr)
				return
			}
			// The row's id: a forecast at the key is promoted in place and keeps
			// its own origin_id (store.OpenCoreEpisode).
			origin.OriginID = id
			l.episode = store.KeeperEpisode{DemandOrigin: origin}
			st.OriginID, st.OpenedAt = origin.OriginID, origin.OpenedAt
			m.eng.logFn("keeper: DEMAND OPENED origin=%s group=%s type=%s want=%d resident=%d",
				origin.OriginID, ask.group.Name, lv.BinTypeCode, lv.Want, c.Resident)
		}
		// NOTHING TO SOURCE, NOTHING ASKED. An ask is pre-resolved to a free
		// position and holds it while it waits for a carrier, so asking past the
		// carts that exist speaks for positions an unloader's empty-out needs. The
		// episode stays open (the family is short, and says so) and HeldBy says why
		// nothing was asked. Pin:
		// TestKanbanKeeper_MintsNoAskForATypeNothingOutsideCanSource.
		if want > 0 {
			st.Created, st.HeldBy = m.createAsks(ask, lv, l.episode.OriginID, want)
		} else {
			st.HeldBy = nothingToSource(lv.BinTypeCode, ask.hasSources)
		}

	case st.Gap >= 1:
		// INSIDE A WHOLE FAMILY: never an outside ask. The arm re-types.
		st.HeldBy = "the family is whole: re-typed at the unloader"

	case c.Short() >= 1:
		// Short only of what the walk has lit at the windows.
		st.HeldBy = fmt.Sprintf("%d at the unloader's windows coming back as %s", st.Lit, lv.BinTypeCode)

	case c.Resident+c.Held >= lv.Want && c.Asked == 0:
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
		if l.episode.OriginID != "" {
			m.closeEpisode(l.episode.DemandOrigin, protocol.CloseReasonRecovered, st.GroupNode, lv.BinTypeCode)
			l.episode = store.KeeperEpisode{}
			st.OriginID, st.OpenedAt = "", time.Time{}
		}
	}
}

// nothingToSource is why a short type asked nothing: no empty of it where this
// kanban's asks may look.
func nothingToSource(code string, hasSources bool) string {
	if hasSources {
		return "no empty " + code + " at any source"
	}
	return "no empty " + code + " outside the group"
}

// parked is the card's parked-ness, derived. The oldest live ask's queue cause is
// the signal — nothing is stored, and an intent with no parked ask simply has no
// cause.
func parked(st *KanbanLineState, ep store.KeeperEpisode, now time.Time) {
	st.OldestAskCause, st.OldestAskReason, st.OldestAskAge = "", "", ""
	if ep.OldestAt != nil {
		st.OldestAskCause = ep.OldestCause
		st.OldestAskReason = ep.OldestReason
		st.OldestAskAge = protocol.FormatDuration(now.UTC().Sub(*ep.OldestAt))
	}
}

// refreshParked reads the open episodes again for the asks this tick made.
func (m *KanbanKeeper) refreshParked(states []KanbanLineState) {
	open, err := m.eng.db.ListOpenKeeperEpisodes()
	if err != nil {
		m.eng.logFn("keeper: list open episodes: %v", err)
		return
	}
	byOrigin := make(map[string]store.KeeperEpisode, len(open))
	for _, o := range open {
		byOrigin[o.OriginID] = o
	}
	for i := range states {
		if ep, ok := byOrigin[states[i].OriginID]; ok {
			parked(&states[i], ep, m.now())
		}
	}
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
// written as a guard.
//
// N ASKS, NOT ONE. Serial filling of a four-deep buffer takes four drive-times,
// which defeats the point of a rate-mismatch buffer.
func (m *KanbanKeeper) createAsks(ask *kanbanAsks, lv store.KanbanLevel, originID string,
	gap int) (created int, heldBy string) {

	g := ask.group
	for i := 0; i < gap; i++ {
		// The slot is the planner's (plan.KanbanSlot), over the tick's one view
		// of the kanban. The one door that has always named its carrier: the level
		// IS a carrier type, so this asks for a free slot OF that type rather than
		// any free slot. Each ask is born and noted before the next is chosen: what
		// is inbound is what moves the next choice on.
		res, err := m.eng.dispatcher.PlanKanbanSlot(ask.reads, g, lv.BinTypeID)
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
			// A level is of EMPTIES: this order type is one of the five fragments a kanban of fulls would widen.
			UUIDPrefix: "core-mnt-",
			OrderType:  dispatch.OrderTypeRetrieveEmpty,
			StationID:  ask.station,
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
			OriginID:    originID,
			OriginClass: protocol.OriginClassAttached,
		})
		if errors.Is(aerr, dispatch.ErrWindowTaken) {
			// The ask holds its slot from its issue, so a slot another order
			// took since the planner read it free is no ask (nothing was
			// written): the loop asks the planner for its next slot.
			m.eng.dbg("keeper: %s/%s: %s was taken since it was chosen", g.Name, lv.BinTypeCode, res.Node.Name)
			ask.reads.NoteSlotTaken(res.Node.ID)
			continue
		}
		if aerr != nil {
			m.eng.logFn("keeper: admit ask for %s/%s at %s: %v",
				g.Name, lv.BinTypeCode, res.Node.Name, aerr)
			return created, "admit refused"
		}
		ask.reads.NoteAsked(res.Node.ID, order.ID)
		m.eng.dispatcher.AcquireCoreAsk(order)
		created++
		m.eng.logFn("keeper: ASK order=%d uuid=%s group=%s type=%s slot=%s origin=%s",
			order.ID, order.EdgeUUID, g.Name, lv.BinTypeCode, res.Node.Name, originID)
	}
	return created, heldBy
}

// closeEpisode ends one intent.
func (m *KanbanKeeper) closeEpisode(episode store.DemandOrigin, reason, group, binType string) {
	closed, err := m.eng.db.CloseDemandOriginByID(episode.OriginID, reason,
		protocol.ClosedByNotification, m.now().UTC())
	if err != nil {
		m.eng.logFn("keeper: close episode %s: %v", episode.OriginID, err)
		return
	}
	if !closed {
		return
	}
	m.eng.logFn("keeper: DEMAND CLOSED origin=%s group=%s type=%s reason=%s",
		episode.OriginID, group, binType, reason)
}

// closeWithdrawn closes every open kanban episode the CONFIG no longer calls
// for — a level line deleted, or a kanban's station or supports cleared.
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
func (m *KanbanKeeper) closeWithdrawn(open []store.KeeperEpisode, configured map[string]bool) {
	// Keyed by episode key, which is lossless: the partial unique open-key index
	// permits exactly one OPEN episode per key, so two rows here cannot collide.
	candidates := make(map[string]store.DemandOrigin, len(open))
	for _, o := range open {
		candidates[o.EpisodeKey] = o.DemandOrigin
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

// KanbanFamilyStates is the family half of the same accessor.
func (e *Engine) KanbanFamilyStates() []KanbanFamilyState {
	if e == nil || e.keeper == nil {
		return nil
	}
	return e.keeper.FamilySnapshot()
}

// KanbanLineStates is the accessor the www layer reads. It exists so www
// never has to reach through KanbanKeeper() into engine internals, and so a Core
// running without a keeper answers "nothing kept" rather than panicking.
func (e *Engine) KanbanLineStates() []KanbanLineState {
	if e == nil || e.keeper == nil {
		return nil
	}
	return e.keeper.Snapshot()
}

// planAsks is what a keeper's tick may ask outside for: each family's deficit, by the types'
// largest gaps against the counted forecast. No deficit asks nothing and reads nothing.
func (m *KanbanKeeper) planAsks(k store.KanbanPictureRow, rows []store.KanbanPictureRow, lines []keeperLine,
	families []KanbanFamilyState, levels []store.KanbanLevel, residents map[string]int) (*kanbanAsks, map[string]int) {
	need := familyDeficits(rows, lines, families)
	if len(need) == 0 {
		return nil, map[string]int{}
	}
	var codes []string
	for i := range rows {
		codes = append(codes, rows[i].BinTypeCode)
	}
	wants := askByLargestGap(rows, lines, need, m.forecast(k, codes))
	ask := &kanbanAsks{
		group:      &nodes.Node{ID: k.KanbanNodeID, Name: k.KanbanName, IsSynthetic: true, Enabled: true},
		station:    k.StationID,
		hasSources: len(k.Sources) > 0,
		deficit:    need,
	}
	if len(wants) > 0 {
		ask.reads = m.eng.db.KanbanAskReads(k.KanbanNodeID, levels, residents)
	}
	return ask, wants
}
