package service

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// THE TAKE: what Core's loader send makes this Edge do to its own screens.
//
// Core may name the Edge that works a loader (bin_loaders.edge_station_uid).
// Which Edge it names decides what this one takes:
//
//   - THIS EDGE (LoaderCaseHere): the Edge takes the loader whole. When Core's
//     pick becomes this Edge, every window goes onto the loader's screen here,
//     and with no screen here the Edge makes the process and screen through
//     CreateLoaderBoard, the door "Make a screen" uses. Windows Core adds later
//     go on; windows it removes come off.
//   - ANOTHER EDGE (LoaderCaseElsewhere): this Edge takes and offers nothing for
//     the loader. A screen of it here from before loses the loader's windows;
//     the process and screen stay, and the page says where Core moved it.
//   - NONE, the blank every loader has until a person picks (and all a Core
//     predating the field sends): inference. A window Core adds to a loader
//     this Edge already held goes onto the one screen holding the loader's
//     other windows; one it removes comes off. No such screen, or more than
//     one, or a loader new to this Edge, and nothing is added.
//
// ONLY A LOADER SCREEN IS EVER WRITTEN: a screen every live position of which
// is a loader window (held by a loader now, or marked v56). A screen with a
// press position is a person's (SPR's Unloader windows sit on two press
// screens, its Finished Goods Unloader screen beside two plain positions), and
// the Edge never writes it on its own. Nor does the take add a window that is
// already a live position anywhere here: that one is its process's (round 8's
// rule for "Core added"). It deletes no process and no screen; it rolls back
// only a process it made itself in the same pass.
//
// REMOVALS ARE READ FROM STATE, ADDITIONS FROM THE DIFFERENCE. Whenever the
// take runs, every position on a loader screen that the Edge marked a window
// and no loader holds now, or that a loader Core names another Edge for holds,
// comes off. Additions are only what the send brought: the windows a loader
// gained, or all of them when Core's pick became this Edge. So a window a
// person takes off a screen while Core still holds it stays off.
//
// AN ORDER RUNNING AT A REMOVED WINDOW keeps it, disabled, through SetNodes'
// own kept path; the page says so. The take remembers that something was kept
// and runs its removals again on the next loader send it is given, and the
// engine's start runs them once. No timer.
//
// STATEMENTS. A send that changes no loader's windows or pick, with nothing
// kept, issues none. One that does reads the plant once (readTakePlant), marks
// what is newly held from that read, and writes the difference through
// SetNodes and CreateLoaderBoard, whose own reads and refusals stay theirs.

// LoaderCase is which Edge Core names to work a loader, read by this Edge.
type LoaderCase int

const (
	// LoaderCaseNone: Core names no Edge (blank), or predates the field.
	LoaderCaseNone LoaderCase = iota
	// LoaderCaseHere: Core names this Edge.
	LoaderCaseHere
	// LoaderCaseElsewhere: Core names another Edge.
	LoaderCaseElsewhere
)

// LoaderFacts is what the take and the page read of one Core loader.
type LoaderFacts struct {
	Key, Name string
	// Windows is the loader's board windows, in Core's order
	// (domain.IsBoardWindow): never a buffer spot.
	Windows []string
	Case    LoaderCase
	// EdgeUID and EdgeName are the Edge Core names: blank for LoaderCaseNone.
	EdgeUID, EdgeName string
}

// loaderCaseOf reads Core's pick against this Edge's station uid. A blank
// self (a bench with no identity) is never "this Edge".
func loaderCaseOf(uid, self string) LoaderCase {
	switch {
	case uid == "":
		return LoaderCaseNone
	case self != "" && uid == self:
		return LoaderCaseHere
	default:
		return LoaderCaseElsewhere
	}
}

// FactsOfInfos is the take's reading of a loader set as Core sends it.
func FactsOfInfos(infos []protocol.LoaderInfo, self string) []LoaderFacts {
	out := make([]LoaderFacts, 0, len(infos))
	for _, l := range infos {
		f := LoaderFacts{Key: l.LoaderKey, Name: l.Name, Case: loaderCaseOf(l.EdgeStationUID, self),
			EdgeUID: l.EdgeStationUID, EdgeName: l.EdgeDisplayName}
		for _, p := range l.Positions {
			if p.CoreNodeName != "" && domain.IsBoardWindow(domain.LoaderLayout(l.Layout), p.HomeKind) {
				f.Windows = append(f.Windows, p.CoreNodeName)
			}
		}
		out = append(out, f)
	}
	return out
}

// FactsOfCoreLoaders is the same reading of the loader cache.
func FactsOfCoreLoaders(ls []store.CoreLoader, self string) []LoaderFacts {
	out := make([]LoaderFacts, 0, len(ls))
	for _, l := range ls {
		out = append(out, LoaderFacts{Key: l.LoaderKey, Name: l.Name, Windows: l.BoardWindows(),
			Case: loaderCaseOf(l.EdgeStationUID, self), EdgeUID: l.EdgeStationUID, EdgeName: l.EdgeDisplayName})
	}
	return out
}

// SetLoaderFacts holds the loader set as this Edge now has it: the engine sets
// it from the cache at its start and from each send it applies.
func (s *StationService) SetLoaderFacts(facts []LoaderFacts) {
	held := append([]LoaderFacts(nil), facts...)
	s.loaderFacts.Store(&held)
}

// LoaderFactsHeld is the loader set SetLoaderFacts last held; ok is false
// before the first.
func (s *StationService) LoaderFactsHeld() (facts []LoaderFacts, ok bool) {
	p := s.loaderFacts.Load()
	if p == nil {
		return nil, false
	}
	return *p, true
}

// loaderFact is one held loader by key.
func (s *StationService) loaderFact(key string) (LoaderFacts, bool) {
	facts, _ := s.LoaderFactsHeld()
	for _, f := range facts {
		if f.Key == key {
			return f, true
		}
	}
	return LoaderFacts{}, false
}

// LoaderTakeNote is why the take left a loader Core names this Edge for
// untaken, as its last run found it; "" when nothing stands in the way.
func (s *StationService) LoaderTakeNote(key string) string {
	if p := s.takeNotes.Load(); p != nil {
		return (*p)[key]
	}
	return ""
}

// loaderDiff is what one send changes, per loader, against the set held.
type loaderDiff struct {
	changed bool
	// added is each loader's board windows the send brought, in Core's order.
	added map[string][]string
	// whole: Core's pick became this Edge (or the loader is new and names
	// it), so the Edge takes every window.
	whole map[string]bool
	// had is each loader the held set had: inference adds only to a loader
	// this Edge already held, since a new loader's nodes on a screen were not
	// that loader's windows when a person put them there.
	had map[string]bool
}

// diffLoaders compares the board facts only: windows (as a set, so Core
// re-ordering them is no change) and the Edge picked. A loader's name or its
// payloads moving is not the take's business.
func diffLoaders(held, arriving []LoaderFacts) loaderDiff {
	d := loaderDiff{added: map[string][]string{}, whole: map[string]bool{}, had: map[string]bool{}}
	before := make(map[string]LoaderFacts, len(held))
	for _, f := range held {
		before[f.Key] = f
	}
	seen := make(map[string]bool, len(arriving))
	for _, f := range arriving {
		seen[f.Key] = true
		old, had := before[f.Key]
		d.had[f.Key] = had
		if !had || old.EdgeUID != f.EdgeUID {
			d.changed = true
		}
		if f.Case == LoaderCaseHere && (!had || old.Case != LoaderCaseHere) {
			d.whole[f.Key] = true
		}
		for _, w := range f.Windows {
			if !slices.Contains(old.Windows, w) {
				d.added[f.Key] = append(d.added[f.Key], w)
				d.changed = true
			}
		}
		for _, w := range old.Windows {
			if !slices.Contains(f.Windows, w) {
				d.changed = true
			}
		}
	}
	for key := range before {
		if !seen[key] {
			d.changed = true
		}
	}
	return d
}

// takePosition is one live position as the take's read gives it.
type takePosition struct {
	id      int64
	name    string
	marked  bool
	enabled bool
}

// takeScreen is one live screen, its positions in order.
type takeScreen struct {
	id, processID int64
	link          string
	positions     []takePosition
}

// takePlant is the take's one read: every screen with its positions, and every
// position on no screen, with where each node is a live position.
type takePlant struct {
	screens []*takeScreen
	byID    map[int64]*takeScreen
	// live is each node name's live positions: the processes holding it.
	live map[string][]int64
	// processNames is each process's name, for a note.
	processNames map[int64]string
	// positionsOf is each process's live positions, on a screen or not.
	positionsOf map[int64][]string
	// unmarked is the ids of live positions not yet marked windows.
	unmarked map[int64]string
}

// readTakePlant is the one read. A screen with no position comes back once
// with a zero position id.
func (s *StationService) readTakePlant() (*takePlant, error) {
	rows, err := s.db.Query(`
		SELECT s.id, s.process_id, s.core_loader_key, p.name, COALESCE(n.id, 0), COALESCE(n.core_node_name, ''),
			COALESCE(n.loader_window, 0), COALESCE(n.enabled, 0), COALESCE(n.sequence, 0)
		FROM operator_stations s
		JOIN processes p ON p.id = s.process_id
		LEFT JOIN process_nodes n ON n.operator_station_id = s.id AND n.deleted_at IS NULL
		UNION ALL
		SELECT 0, n.process_id, '', p.name, n.id, n.core_node_name, n.loader_window, n.enabled, n.sequence
		FROM process_nodes n
		JOIN processes p ON p.id = n.process_id
		WHERE n.operator_station_id IS NULL AND n.deleted_at IS NULL
		ORDER BY 1, 9, 5`)
	if err != nil {
		return nil, fmt.Errorf("read the plant for the loader take: %w", err)
	}
	defer rows.Close()
	pl := &takePlant{byID: map[int64]*takeScreen{}, live: map[string][]int64{}, processNames: map[int64]string{},
		positionsOf: map[int64][]string{}, unmarked: map[int64]string{}}
	for rows.Next() {
		var sid, pid, nid int64
		var link, pname, name string
		var marked, enabled, seq int
		if err := rows.Scan(&sid, &pid, &link, &pname, &nid, &name, &marked, &enabled, &seq); err != nil {
			return nil, fmt.Errorf("read the plant for the loader take: %w", err)
		}
		pl.processNames[pid] = pname
		if sid != 0 && pl.byID[sid] == nil {
			sc := &takeScreen{id: sid, processID: pid, link: link}
			pl.byID[sid] = sc
			pl.screens = append(pl.screens, sc)
		}
		if nid == 0 {
			continue
		}
		pos := takePosition{id: nid, name: name, marked: marked == 1, enabled: enabled == 1}
		if sid != 0 {
			pl.byID[sid].positions = append(pl.byID[sid].positions, pos)
		}
		pl.live[name] = append(pl.live[name], pid)
		pl.positionsOf[pid] = append(pl.positionsOf[pid], name)
		if !pos.marked {
			pl.unmarked[nid] = name
		}
	}
	return pl, rows.Err()
}

// isLoaderScreen is whether every live position on sc is a loader window.
func (s *StationService) isLoaderScreen(sc *takeScreen) bool {
	for _, p := range sc.positions {
		if !p.marked && !s.LoaderHolds(p.name) {
			return false
		}
	}
	return true
}

// holdsAny is whether sc has a live position among names.
func (sc *takeScreen) holdsAny(names []string) bool {
	for _, p := range sc.positions {
		if slices.Contains(names, p.name) {
			return true
		}
	}
	return false
}

// screenFor is the loader's screen here: the one loader screen holding any of
// its windows; among several, the one that names it; with none holding a
// window, one that names it (useLink, the case-1 return). Zero is none;
// ambiguous says it was several with none named.
func (s *StationService) screenFor(f LoaderFacts, pl *takePlant, useLink bool) (sid int64, ambiguous bool) {
	var holding, linked []*takeScreen
	for _, sc := range pl.screens {
		if !s.isLoaderScreen(sc) {
			continue
		}
		if sc.holdsAny(f.Windows) {
			holding = append(holding, sc)
		}
		if sc.link == f.Key {
			linked = append(linked, sc)
		}
	}
	switch {
	case len(holding) == 1:
		return holding[0].id, false
	case len(holding) > 1:
		for _, sc := range holding {
			if sc.link == f.Key {
				return sc.id, false
			}
		}
		return 0, true
	case useLink && len(linked) == 1:
		return linked[0].id, false
	}
	return 0, false
}

// screenPlan is one screen's write: what comes off, what goes on, and the
// loader the screen is taken for.
type screenPlan struct {
	remove map[string]bool
	// retry is the disabled positions coming off: kept for an order before.
	retry []int64
	add   []string
	link  string
}

// TakeResult is what one run of the take did, for the engine's log.
type TakeResult struct {
	Ran     bool
	Marked  int
	Changes []string // one line per screen written or made
	Kept    []string // windows left on a screen for a running order
}

// TakeLoaders runs the take: held is the loader set before the send, arriving
// the set now (both FactsOf...). start runs it without a send: removals and
// marks only, since nothing arrived. It runs at all only when the send
// changed a loader's windows or pick, something was kept before, or start.
func (s *StationService) TakeLoaders(held, arriving []LoaderFacts, start bool) (TakeResult, error) {
	var res TakeResult
	diff := diffLoaders(held, arriving)
	if !start && !diff.changed && !s.takePending.Load() {
		return res, nil
	}
	res.Ran = true
	pl, err := s.readTakePlant()
	if err != nil {
		return res, err
	}
	// The v56 marks, from the same read: every live position a loader holds
	// now is a window from here on (MarkLoaderWindows' rule).
	var marks []int64
	for id, name := range pl.unmarked {
		if s.LoaderHolds(name) {
			marks = append(marks, id)
		}
	}
	slices.Sort(marks)
	if err := s.db.MarkProcessNodesLoaderWindow(marks); err != nil {
		return res, fmt.Errorf("mark loader windows: %w", err)
	}
	res.Marked = len(marks)
	for _, sc := range pl.screens {
		for i, p := range sc.positions {
			if !p.marked && slices.Contains(marks, p.id) {
				sc.positions[i].marked = true
			}
		}
	}

	facts := map[string]LoaderFacts{}
	for _, f := range arriving {
		facts[f.Key] = f
	}
	plans := map[int64]*screenPlan{}
	planFor := func(sid int64) *screenPlan {
		if plans[sid] == nil {
			plans[sid] = &screenPlan{remove: map[string]bool{}}
		}
		return plans[sid]
	}
	s.planRemovals(pl, facts, planFor)
	notes := s.planAdditions(pl, arriving, diff, start, planFor, &res)

	pending := false
	for _, sc := range pl.screens {
		p := plans[sc.id]
		if p == nil {
			continue
		}
		kept, wrote, err := s.writeScreenPlan(sc, p)
		if err != nil {
			return res, fmt.Errorf("screen %d: %w", sc.id, err)
		}
		if len(kept) > 0 {
			pending = true
			res.Kept = append(res.Kept, kept...)
		}
		if wrote != "" {
			res.Changes = append(res.Changes, wrote)
		}
	}
	s.takePending.Store(pending)
	s.takeNotes.Store(&notes)
	return res, nil
}

// planRemovals plans, on every loader screen, the positions coming off: a
// window Core removed (marked, held by no loader now) or one of a loader Core
// names another Edge for. A disabled one is planned too, so its retire is
// tried again once its order is gone.
func (s *StationService) planRemovals(pl *takePlant, facts map[string]LoaderFacts, planFor func(int64) *screenPlan) {
	for _, sc := range pl.screens {
		if !s.isLoaderScreen(sc) {
			continue
		}
		for _, p := range sc.positions {
			off, moved := false, ""
			if s.loaders != nil {
				l, _ := s.loaders.LoaderForNode(domain.NodeID(p.name))
				switch {
				case l == nil || !l.Contains(domain.NodeID(p.name)):
					off = p.marked
				case facts[string(l.ID())].Case == LoaderCaseElsewhere:
					off, moved = true, string(l.ID())
				}
			}
			if !off {
				continue
			}
			plan := planFor(sc.id)
			plan.remove[p.name] = true
			// The screen stays the moved loader's, so its page can say where
			// Core moved it once every window is off.
			if moved != "" && plan.link == "" {
				plan.link = moved
			}
			if !p.enabled {
				plan.retry = append(plan.retry, p.id)
			}
		}
	}
}

// planAdditions plans each loader's windows going on, by its case, and makes
// a screen where Core's pick became this Edge and none is here. It returns
// why a loader Core names this Edge for was left untaken.
func (s *StationService) planAdditions(pl *takePlant, arriving []LoaderFacts, diff loaderDiff, start bool,
	planFor func(int64) *screenPlan, res *TakeResult) map[string]string {
	notes := map[string]string{}
	if start {
		return notes
	}
	for _, f := range arriving {
		if f.Case == LoaderCaseElsewhere || (f.Case == LoaderCaseNone && !diff.had[f.Key]) {
			continue
		}
		want := diff.added[f.Key]
		if diff.whole[f.Key] {
			want = f.Windows
		}
		var on []string
		for _, w := range want {
			if len(pl.live[w]) == 0 {
				on = append(on, w)
			}
		}
		sid, ambiguous := s.screenFor(f, pl, f.Case == LoaderCaseHere)
		switch {
		case sid != 0:
			if len(on) > 0 {
				plan := planFor(sid)
				plan.add = append(plan.add, on...)
				plan.link = f.Key
			}
		case ambiguous:
			if f.Case == LoaderCaseHere {
				notes[f.Key] = "more than one screen here holds its windows and none was made for it"
			}
		case f.Case == LoaderCaseHere && diff.whole[f.Key]:
			line, why := s.makeLoaderScreen(f, pl)
			if line != "" {
				res.Changes = append(res.Changes, line)
			}
			if why != "" {
				notes[f.Key] = why
			}
		}
	}
	return notes
}

// makeLoaderScreen makes the loader's process and screen: on the one process
// already holding its windows when that process holds nothing else (the
// "modelled, nobody made the screen" case LoaderBoardGaps names), else on a
// new process named after the loader. It makes nothing where a window is a
// position of a person's process, or the name is taken, and says why.
func (s *StationService) makeLoaderScreen(f LoaderFacts, pl *takePlant) (line, why string) {
	if len(f.Windows) == 0 {
		return "", ""
	}
	holders := map[int64]bool{}
	for _, w := range f.Windows {
		for _, pid := range pl.live[w] {
			holders[pid] = true
		}
	}
	name := strings.TrimSpace(f.Name)
	if name == "" {
		name = f.Key
	}
	var pid int64
	made := false
	switch len(holders) {
	case 0:
		id, err := s.db.CreateProcess(name, "", "", "", false)
		if err != nil {
			if errors.Is(err, processes.ErrNameTaken) {
				return "", fmt.Sprintf("a process named %q is already here", name)
			}
			return "", "the process could not be made: " + err.Error()
		}
		pid, made = id, true
	case 1:
		for id := range holders {
			pid = id
		}
		for _, n := range pl.positionsOf[pid] {
			if !slices.Contains(f.Windows, n) {
				w := firstHeld(f.Windows, pl, pid)
				return "", fmt.Sprintf("%s is a position of %s, which holds more than this loader", w, pl.processNames[pid])
			}
		}
	default:
		w := firstHeld(f.Windows, pl, 0)
		return "", fmt.Sprintf("its windows are positions of more than one process here (%s among them)", w)
	}
	sid, err := s.CreateLoaderBoard(f.Key, pid)
	if err != nil {
		if made {
			if delErr := s.db.DeleteProcess(pid); delErr != nil {
				return "", fmt.Sprintf("the screen could not be made (%v), and the process made for it could not be removed (%v)", err, delErr)
			}
		}
		return "", "the screen could not be made: " + err.Error()
	}
	if made {
		return fmt.Sprintf("made process %q and screen %d for %s", name, sid, f.Key), ""
	}
	return fmt.Sprintf("made screen %d on process %q for %s", sid, pl.processNames[pid], f.Key), ""
}

// firstHeld is the first of windows that is a live position (of pid, or of any
// process for 0).
func firstHeld(windows []string, pl *takePlant, pid int64) string {
	for _, w := range windows {
		for _, holder := range pl.live[w] {
			if pid == 0 || holder == pid {
				return w
			}
		}
	}
	return ""
}

// writeScreenPlan writes one screen's plan through SetNodes: its enabled
// positions less what comes off, then what goes on. A plan that only retries
// disabled positions whose orders still run writes nothing. It returns the
// positions SetNodes kept for an order, and a line saying what it wrote.
func (s *StationService) writeScreenPlan(sc *takeScreen, p *screenPlan) (kept []string, line string, err error) {
	var enabledOff []string
	for _, pos := range sc.positions {
		if pos.enabled && p.remove[pos.name] {
			enabledOff = append(enabledOff, pos.name)
		}
	}
	if len(p.add) == 0 && len(enabledOff) == 0 {
		if len(p.retry) == 0 {
			return nil, "", nil
		}
		running, err := s.anyOrderRunning(p.retry)
		if err != nil {
			return nil, "", err
		}
		if running {
			for _, pos := range sc.positions {
				if !pos.enabled && p.remove[pos.name] {
					kept = append(kept, pos.name)
				}
			}
			return kept, "", nil
		}
	}
	var names []string
	for _, pos := range sc.positions {
		if pos.enabled && !p.remove[pos.name] {
			names = append(names, pos.name)
		}
	}
	names = append(names, p.add...)
	kept, err = s.SetNodes(sc.id, names)
	if err != nil {
		return nil, "", err
	}
	if p.link != "" && sc.link == "" {
		if err := s.db.SetOperatorStationLoader(sc.id, p.link); err != nil {
			return kept, "", err
		}
	}
	var parts []string
	if len(p.add) > 0 {
		parts = append(parts, "put on "+strings.Join(p.add, ", "))
	}
	var off []string
	for name := range p.remove {
		if !slices.Contains(kept, name) {
			off = append(off, name)
		}
	}
	slices.Sort(off)
	if len(off) > 0 {
		parts = append(parts, "took off "+strings.Join(off, ", "))
	}
	if len(kept) > 0 {
		parts = append(parts, "kept "+strings.Join(kept, ", ")+" for a running order")
	}
	return kept, fmt.Sprintf("screen %d: %s", sc.id, strings.Join(parts, "; ")), nil
}

// anyOrderRunning is whether an order is active at any of the positions.
func (s *StationService) anyOrderRunning(ids []int64) (bool, error) {
	for _, id := range ids {
		active, err := s.db.ListActiveOrdersByProcessNode(id)
		if err != nil {
			return false, err
		}
		if len(active) > 0 {
			return true, nil
		}
	}
	return false, nil
}
