package service

import (
	"log"
	"math"
	"strconv"
	"time"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// station_composer.go — the Flow Composer's data, built once per request in
// three shapes.
//
// WHAT THE POLL CARRIES AND WHAT IT DOES NOT (S8). This block used to ride
// every station view, which meant the whole composer — every style's cells,
// the routing set, the preset cards and the travel graph — was built,
// marshalled and gzipped on every poll of every board. Boards poll at 500 ms
// while events flow, and the station reads the composer half of it ONCE, when
// an operator taps CHANGEOVER. The rest was discarded: 40 styles x 6 cells
// built and thrown away about 1,700 times an hour per station.
//
// So the view now carries only what the PICKER draws — the style rows and
// their summaries — and everything the composer itself needs is one fetch,
// made when the composer opens and held for the session. The two shapes come
// out of one builder, because the day they came out of two would be the day
// an engineer and an operator disagreed about the same part.
//
// ONE READ OF THE CLAIMS. Every scope below is fed by a single
// ListLiveClaimsByProcess: the style summaries, the stored cells, the preset
// member counts, the flow provenance and the desktop's Advanced blocks all
// read the same rows. Before this there were four walks over the same product
// (the builder, presetMemberCounts, addAdvanced, and the presets service's
// styleFlows), each issuing its own query per style.

// composerScope says which of the three shapes to build.
//
// A WIDENING SEQUENCE WITH TWO NAMED FORKS, and it used to claim it had none:
// "each carries everything the one before it does — so a field can only ever
// move outward, never fork." That was already untrue when it was written —
// Scene rides the station read and Map the desktop's, never both, because they
// are the same network and a payload carrying both carries it twice — and the
// rule it was really keeping is the one below.
//
// THE RULE: what a surface already has decides what it is sent. Nothing is
// carried twice on one payload.
//
// The two forks, and the pin for each:
//
//   - Scene (station) vs Map (desktop): the same network, one collapsed.
//     TestComposerRead_DesktopByteBudget holds that the station read has no Map.
//   - ComposerStyle.Nodes and .Parts ride the PICKER scope only. They are the
//     core node of every claim and the distinct payloads with their positions
//     — which is what Claims says, on the same object, and the composer scopes
//     carry Claims. composer-model's styleFacts derives them for any surface
//     that has cells and no summary.
//     TestComposerStyleFields_TheDerivableOnesRideOnlyThePickerScope is the pin.
//
// What styleFor() then merges is still safe: the picker block is the only place
// those two exist, so a merge cannot find two answers to one field.
type composerScope int

const (
	// composerPicker is the block the station VIEW carries: the picker's rows
	// and the set-up card's header. No cells, no routing set, no presets, no
	// travel graph.
	composerPicker composerScope = iota
	// composerStation is the station's own composer fetch: the picker block
	// plus the stored cells, the routing set, the preset cards and the scene.
	// No Map, no Advanced — the HMI has no Advanced modal, and the map is a
	// hundred kilobytes of coordinates for a screen the station never opens.
	composerStation
	// composerDesktop is the admin read: everything, plus the press as a
	// picture, the Advanced blocks and the plant map.
	composerDesktop
)

func (s composerScope) carriesFlows() bool { return s >= composerStation }

// recentChangeoverWindow bounds the history read that answers both "when did
// this style last run" and "what were the last four targets".
//
// A BOUND, NOT A PAGE, and the trade is stated rather than hidden: a style
// this press has not run in its last 200 changeovers shows no "ran 08-29" on
// its picker row instead of showing a very old one. At a press's real rate
// that is a style untouched for months, and a blank there is the more honest
// answer anyway. What it buys is a read that stops growing: the unbounded
// version was 0.28 ms at 600 rows and 12.4 ms at 12,000, twice per poll.
const recentChangeoverWindow = 200

// ComposerForProcess is the DESKTOP's read: everything the station's composer
// fetch carries, plus the whole press as a picture, the Advanced blocks and
// the plant map.
//
// The same builder, deliberately. "The desktop and the HMI are one thing
// managed in one place" (U9 ruling R1) is only true if there is one thing
// behind both, and a second builder here would be the two-copies problem
// wearing a different hat.
func (s *StationService) ComposerForProcess(processID int64) (*domain.ComposerData, error) {
	process, err := s.db.GetProcess(processID)
	if err != nil {
		return nil, err
	}
	if process == nil {
		return nil, nil
	}
	styles, err := s.db.ListStylesByProcess(processID)
	if err != nil {
		return nil, err
	}
	claims := s.liveClaimsByStyle(processID)
	// ONE ListProcessNodesByProcess FOR THIS READ, and it used to be two: the
	// picture read the node list, and pressOrder read it again for the preset
	// cards' position order. Read once here and handed to both (P2).
	nodes, err := s.db.ListProcessNodesByProcess(processID)
	if err != nil {
		log.Printf("composer: list process nodes for %d: %v", processID, err)
	}
	out := s.buildComposerData(processID, styles, claims, composerDesktop, func() []processes.Node { return nodes })
	// THE ROUTING SET THE BLOCK JUST BUILT, not a second read of it: the
	// picture's staging offers are the enabled staging rows, which is the same
	// list out.Routing carries.
	out.Cell = s.processCellPicture(process, claims, nodes, routingFromBlock(out))
	out.Map = s.composerMap()
	return out, nil
}

// ComposerForStation is the STATION's read, made once when the composer opens
// and held for the session (S8). Station-shaped: the scene, no Map, no
// Advanced blocks.
//
// IT CARRIES ITS OWN PICTURE NOW, station-scoped (SYNTH §3 B2). The HMI's
// composer used to draw the POLLED view's picture, which is the board's: the
// staging the RUNNING flow parks at, and nothing the cell merely could park
// at. So the composer — the one screen whose whole job is choosing where a bin
// goes — was the surface with no staging options drawn on it, which is the
// opposite of the complaint this work started from.
//
// The composer's picture is the positions plus the process's ENABLED
// staging-role routing nodes (R4). +0 queries: the node list is the one this
// read already takes for the preset order, and the back positions are a fold
// over the claims in hand.
func (s *StationService) ComposerForStation(stationID int64) (*domain.ComposerData, error) {
	station, err := s.db.GetOperatorStation(stationID)
	if err != nil {
		return nil, err
	}
	if station == nil {
		return nil, nil
	}
	processID := station.ProcessID
	process, err := s.db.GetProcess(processID)
	if err != nil {
		return nil, err
	}
	if process == nil {
		return nil, nil
	}
	styles, err := s.db.ListStylesByProcess(processID)
	if err != nil {
		return nil, err
	}
	byStyle := s.liveClaimsByStyle(processID)
	// LAZY, not read: the node list is wanted only for the preset cards' position
	// order and the picture, and a cell with no presets must not pay a query for
	// a value nothing reads. memoNodes makes the two share one read when both
	// want it. See composerPresets.
	nodes := memoNodes(func() []processes.Node { return s.processNodes(processID) })
	out := s.buildComposerData(processID, styles, byStyle, composerStation, nodes)
	active := map[string]domain.NodeClaim{}
	live := make([]processes.NodeClaim, 0, 32)
	for styleID, rows := range byStyle {
		live = append(live, rows...)
		if process.ActiveStyleID != nil && styleID == *process.ActiveStyleID {
			for _, c := range rows {
				active[c.CoreNodeName] = c
			}
		}
	}
	out.Cell = s.cellPicture(stationID, process, active, live, nodes(), cellPictureOptions{
		stagingOffers: stagingOffers(routingFromBlock(out)),
	})
	return out, nil
}

// memoNodes makes a node-list reader that reads at most once, however many
// callers want it. The preset order and the picture both do on this read; only
// the preset order does when the cell has presets and no picture is asked for.
func memoNodes(read func() []processes.Node) func() []processes.Node {
	var (
		once  bool
		cache []processes.Node
	)
	return func() []processes.Node {
		if !once {
			once, cache = true, read()
		}
		return cache
	}
}

// processNodes is the node list, fail-open with a line.
func (s *StationService) processNodes(processID int64) []processes.Node {
	nodes, err := s.db.ListProcessNodesByProcess(processID)
	if err != nil {
		log.Printf("composer: list process nodes for %d: %v", processID, err)
	}
	return nodes
}

// liveClaimsByStyle is THE claims read for a composer build: one query for
// the whole process, grouped by style in the order ListClaims would have
// given each one.
func (s *StationService) liveClaimsByStyle(processID int64) map[int64][]processes.NodeClaim {
	rows, err := processes.ListLiveClaimsByProcess(s.db.DB, processID)
	if err != nil {
		log.Printf("composer: list live claims for process %d: %v", processID, err)
		return map[int64][]processes.NodeClaim{}
	}
	out := make(map[int64][]processes.NodeClaim, 16)
	for _, c := range rows {
		out[c.StyleID] = append(out[c.StyleID], c)
	}
	return out
}

// buildComposerData assembles one of the three shapes from rows already read.
// It issues no per-style query.
func (s *StationService) buildComposerData(processID int64, styles []processes.Style,
	claims map[int64][]processes.NodeClaim, scope composerScope,
	nodes func() []processes.Node) *domain.ComposerData {
	out := &domain.ComposerData{}

	lastRun, recent := s.changeoverHistory(processID)
	out.RecentTargets = recent

	for _, st := range styles {
		rows := claims[st.ID]
		cs := domain.ComposerStyle{
			ID: st.ID, Name: st.Name, CATID: st.ExpectedCATID,
			LastRun:    lastRun[st.ID],
			ClaimCount: len(rows),
		}
		seenMode := map[string]bool{}
		seenPart := map[string]bool{}
		for _, c := range rows {
			m := string(c.SwapMode)
			if m != "" && !seenMode[m] {
				seenMode[m] = true
				cs.Modes = append(cs.Modes, m)
			}
			// NODES AND PARTS ARE THE PICKER'S, and only the picker's. On a
			// scope that carries Claims they are the same strings twice on one
			// object — 2,290 and 13,018 bytes of the desktop read, measured —
			// and composer-model's styleFacts derives them from the cells for
			// any surface that has them. See composerScope's forks.
			//
			// MODES STAYS ON EVERY SCOPE. It is a deduplicated handful per
			// style (one or two words), it is what the picker row's glyph and
			// the set-up card's meta line read, and unlike the other two it is
			// not a per-claim list — so the copy costs almost nothing and
			// deriving it would buy almost nothing.
			if scope != composerPicker {
				continue
			}
			cs.Nodes = append(cs.Nodes, c.CoreNodeName)
			if c.PayloadCode != "" && !seenPart[c.PayloadCode] {
				seenPart[c.PayloadCode] = true
				cs.Parts = append(cs.Parts, domain.ComposerPart{
					PayloadCode: c.PayloadCode, Node: c.CoreNodeName,
				})
			}
		}
		if scope.carriesFlows() {
			// The cells, the provenance and the Advanced blocks are the
			// COMPOSER's, not the picker's: the set-up card that reads the
			// provenance line opens after the composer fetch, and a board
			// polling at 500 ms should not be collapsing 40 styles' claims
			// into cells it will not draw.
			for _, c := range rows {
				cs.Claims = append(cs.Claims, domain.Collapse(c))
			}
			cs.SavedFrom, cs.SavedOn = flowProvenance(rows)
		}
		if scope == composerDesktop && len(rows) > 0 {
			adv := make(map[string]domain.FlowAdvanced, len(rows))
			for i := range rows {
				adv[rows[i].CoreNodeName] = *domain.AdvancedOf(&rows[i])
			}
			cs.Advanced = adv
		}
		out.Styles = append(out.Styles, cs)
	}

	if !scope.carriesFlows() {
		return out
	}

	// THE PART SET, BELOW THE EARLY RETURN. One union read on a fetch that
	// happens when the composer opens — never on the poll, which is the line
	// this return is.
	out.Palette = s.processPalette(processID)

	for _, r := range s.routingNodes(processID) {
		if !r.Enabled {
			// A retired lane is not an option the operator should know not to
			// pick — but it is COUNTED on the way past, because a role whose
			// every row is switched off draws a heading over nothing and the
			// screens have to be able to say which of the two empties it is.
			if out.RoutingOff == nil {
				out.RoutingOff = map[string][]string{}
			}
			out.RoutingOff[r.Role] = append(out.RoutingOff[r.Role], r.CoreNodeName)
			continue
		}
		// Sequence travels; Label does not. composer-model.js's
		// defaultRouting picks a cell's default source and destination by
		// LOWEST SEQUENCE, so dropping it would silently re-pick every
		// default on the HMI. Label is dropped because its only writer
		// (desktop-bodies.js routingAdd) sets it to the node name and every
		// reader falls back to the node name, so it never carried anything
		// the name did not.
		out.Routing = append(out.Routing, domain.ComposerRoutingNode{
			CoreNodeName: r.CoreNodeName, Role: r.Role, Sequence: r.Sequence,
		})
	}

	out.Presets = s.composerPresets(processID, claims, nodes)
	// SCENE OR MAP, NEVER BOTH — they are the same network. The desktop gets
	// Map (whose edges carry Len) in ComposerForProcess; the station gets the
	// collapsed form here.
	if scope == composerStation {
		out.Scene = s.composerScene()
	}
	return out
}

// changeoverHistory answers both history questions from ONE bounded read:
// the day each style last ran, and the last four distinct targets.
//
// NO Go RE-SORT. The rows arrive newest-first from SQL
// (idx_changeovers_process_started), so the first time a style is seen is its
// most recent changeover for both answers. The previous version read the
// whole table twice and then sorted one of the copies again in Go.
func (s *StationService) changeoverHistory(processID int64) (map[int64]string, []int64) {
	lastRun := map[int64]string{}
	var recent []int64
	cos, err := s.db.ListRecentProcessChangeovers(processID, recentChangeoverWindow)
	if err != nil {
		return lastRun, nil
	}
	seenTarget := map[int64]bool{}
	for _, co := range cos {
		if co.ToStyleID == 0 {
			continue
		}
		// RECENT is the last four DISTINCT targets. Distinct because a press
		// that ran the same two parts all week would otherwise fill the group
		// with one name.
		if !seenTarget[co.ToStyleID] {
			seenTarget[co.ToStyleID] = true
			if len(recent) < 4 {
				recent = append(recent, co.ToStyleID)
			}
		}
		when := co.StartedAt
		if co.CompletedAt != nil {
			when = *co.CompletedAt
		}
		if when.IsZero() {
			continue
		}
		// Formatted here rather than in the JS so "ran 08-29" is one
		// implementation and not a date library on a touch screen.
		if _, seen := lastRun[co.ToStyleID]; !seen {
			lastRun[co.ToStyleID] = when.Format("01-02")
		}
	}
	return lastRun, recent
}

// composerMap is the geometry cache as D3 draws it, with each edge's length
// alongside its shape. Nil when the cache is empty — the screen then says the
// map has not arrived, which is true and is better than a plant drawn from
// the node list with the positions guessed.
//
// Built from the same per-cache memo composerScene reads, so the lengths on
// the two shapes are one computation and cannot disagree.
func (s *StationService) composerMap() *domain.ComposerMap {
	g, memo := s.sceneDerived()
	if memo == nil || len(g.Points) == 0 {
		return nil
	}
	m := &domain.ComposerMap{
		// The revision is READ: D3's caption says "plant map from Core ·
		// revision <rev>" (processes-desktop.js:1493), which is how an
		// engineer tells a stale cache from a current one.
		Revision: g.Revision,
		Points:   make(map[string]domain.ComposerMapPoint, len(g.Points)),
		Edges:    make([]domain.ComposerMapEdge, 0, len(g.Edges)),
	}
	for name, p := range g.Points {
		m.Points[name] = domain.ComposerMapPoint{Class: p.ClassName, X: p.X, Y: p.Y}
	}
	// EVERY ENDPOINT IS A POINT. Core sends points and edges as two lists and
	// NewSceneGeometry validates them apart, so an edge may name a point the
	// point list does not carry. The page then drew the lane (the edge used to
	// carry its own copy of the coordinates) and did not draw the point, and
	// Tp(name) returned null for it — a lane arriving from nowhere.
	//
	// Placing it from the edge's own coordinates fixes that and is what lets
	// the copies go: with this loop, Points[e.From] always resolves.
	place := func(name string, x, y float64) {
		if _, ok := m.Points[name]; !ok {
			m.Points[name] = domain.ComposerMapPoint{X: x, Y: y}
		}
	}
	for i, e := range g.Edges {
		place(e.From, e.FromX, e.FromY)
		place(e.To, e.ToX, e.ToY)
		m.Edges = append(m.Edges, domain.ComposerMapEdge{
			From: e.From, To: e.To,
			Len:     memo.Edges[i].Len,
			Handles: e.Handles,
		})
	}
	return m
}

// processCellPicture draws the cell for the whole process — stationID 0, the
// scope cellPositionNames documents. Every input is one the caller already
// has: the claims it read for the style blocks, and the node list it read for
// the preset order.
func (s *StationService) processCellPicture(process *processes.Process,
	byStyle map[int64][]processes.NodeClaim, nodes []processes.Node,
	routing []domain.RoutingNode) *domain.CellPicture {

	active := map[string]domain.NodeClaim{}
	live := make([]processes.NodeClaim, 0, 32)
	for styleID, rows := range byStyle {
		live = append(live, rows...)
		if process.ActiveStyleID != nil && styleID == *process.ActiveStyleID {
			for _, c := range rows {
				active[c.CoreNodeName] = c
			}
		}
	}
	// stationID 0 — every station of this process, the desktop's scope.
	return s.cellPicture(0, process, active, live, nodes, cellPictureOptions{
		stagingOffers: stagingOffers(routing),
	})
}

// stagingOffers is the enabled staging-role members of a process's routing set
// — what this cell MAY park at, drawn on the composer's picture whether or not
// the running flow uses them (R4).
//
// ENABLED ONLY, like every other offer the composer makes: a retired lane is
// not an option the operator should have to know not to pick.
func stagingOffers(routing []domain.RoutingNode) []string {
	var out []string
	for _, r := range routing {
		if r.Enabled && r.Role == domain.RoutingRoleStaging {
			out = append(out, r.CoreNodeName)
		}
	}
	return out
}

// flowProvenance is where a style's flow came from and when, for the set-up
// card's dim line (owner ruling R1).
//
// The claims ARE the flow, so this reads them: the newest updated_at, and the
// surface every one of them agrees on. A style whose claims disagree — half
// written from the desktop and half from the station — gets no surface at all
// rather than whichever one happened to sort first. The card then says when
// without saying where, which is true; naming one of the two would be the
// caption inventing the fact the source field exists to record.
//
// The date is formatted here, like LastRun, so "09-02" has one implementation
// and not a date library on a touch screen.
func flowProvenance(claims []processes.NodeClaim) (from, on string) {
	var newest time.Time
	src := ""
	for i, c := range claims {
		// UpdatedAt is a pointer: a row the store has never rewritten has none.
		if c.UpdatedAt != nil && c.UpdatedAt.After(newest) {
			newest = *c.UpdatedAt
		}
		switch {
		case i == 0:
			src = c.Source
		case c.Source != src:
			src = "" // they disagree; say nothing
		}
	}
	switch src {
	case domain.ClaimSourceAdmin:
		from = "the desktop"
	case domain.ClaimSourceHMI:
		// The station that saved it, by the name on the floor.
		for _, c := range claims {
			if c.CalledBy != "" {
				from = c.CalledBy
				break
			}
		}
	}
	if !newest.IsZero() {
		on = newest.Format("01-02")
	}
	return from, on
}

// routingNodes is the process's routing set, fail-open WITH A LINE.
//
// It returned nil silently, which is the one shape that cannot be told apart
// from a real answer: a process whose routing set could not be READ and one
// that genuinely has no rows both reach the screens as "no options", and the
// screens then say "add them in Settings › Routing" about a set that may be
// full. Every sibling reader on this path logs; this one did not.
func (s *StationService) routingNodes(processID int64) []domain.RoutingNode {
	rows, err := s.db.ListRoutingNodes(processID)
	if err != nil {
		log.Printf("composer: list routing nodes for %d: %v", processID, err)
		return nil
	}
	return rows
}

// processPalette is the part set every part picker offers. Fail-open with a
// line, like its siblings: a composer that opens with no parts to choose from
// is bad, and one that does not open at all is worse.
func (s *StationService) processPalette(processID int64) []string {
	codes, err := s.db.ProcessPalette(processID)
	if err != nil {
		log.Printf("composer: part set for process %d: %v", processID, err)
		return nil
	}
	return codes
}

// cellOrder is the process's position names in the order the cell has them,
// which is the order every surface prints a shape's positions in. See
// domain.PresetShapeNodes.
//
// NAMED FOR THE CELL, not for a press (owner, 2026-09-17): a 4x2 is a weld
// cell, and the order of its positions is not a fact about presses.
func cellOrder(nodes []processes.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.CoreNodeName)
	}
	return out
}

// composerPresets turns stored presets into strip cards. The card's glyph needs
// ONE mode; a preset whose cells disagree leaves it blank and draws the empty
// glyph rather than electing one of them to stand for the rest.
//
// IT PARSES THROUGH domain.ParsePresetShape, AND THAT IS THE FIX. This
// unmarshalled p.FlowJSON into a bare []FlowCell, while CreateFlowPreset
// validates and stores `{"cells":[…]}` — an object. The two never matched, so
// the `continue` below fired on every stored preset and the strip drew no
// cards at all. U8 left this code unexercised (R9), and until U10 stored a
// preset and asked the composer block for its card, nothing was red.
// TestComposerPresets_AStoredPresetRendersACard is that test.
//
// THE NODE LIST ARRIVES AS A FUNCTION, and that is P2's second half. It used
// to be READ by the caller and handed in, so a cell with ZERO presets paid a
// node-list query for a value the early return below then discarded — one line
// from where exactly this was fixed for the member counts, and invisible to the
// budget pin because the pin read it before it started counting. Called after
// the return, so the query happens when there is a card to order.
func (s *StationService) composerPresets(processID int64, claims map[int64][]processes.NodeClaim,
	nodes func() []processes.Node) []domain.ComposerPreset {

	rows, err := s.db.ListFlowPresets(processID, false)
	if err != nil || len(rows) == 0 {
		// NO PRESETS, NO MEMBER COUNT AND NO NODE LIST. Every process starts
		// here and most stay here: counting members for an empty card list was
		// 41 of the poll's 86 queries, building a map nothing then indexed.
		return nil
	}
	pressOrder := cellOrder(nodes())
	members := presetMemberCounts(claims)
	var out []domain.ComposerPreset
	for _, p := range rows {
		cells, err := domain.ParsePresetShape(p.FlowJSON)
		if err != nil {
			// A preset that does not parse is not a card to offer. It also
			// cannot have come from this edge's own writer, so it is worth a
			// line rather than a silent skip.
			log.Printf("composer: preset %d (%s v%d) does not parse, no card offered: %v", p.ID, p.Name, p.Version, err)
			continue
		}
		card := domain.ComposerPreset{
			// The ID is the preset's id, not its name: a tap has to resolve
			// to one version, and two versions of a name share the name.
			ID: strconv.FormatInt(p.ID, 10), Name: p.Name,
			Cells: map[string]domain.FlowCell{},
		}
		for _, c := range cells {
			card.Cells[c.CoreNodeName] = c
		}
		card.Mode = oneMode(cells)
		card.Where = domain.PresetShapeNodeWord(cells, pressOrder)
		card.Use = presetUse(members[p.ID])
		out = append(out, card)
	}
	return out
}

// presetMemberCounts is how many of the process's parts run each preset: the
// claims that point at it, counted by style.
//
// FROM THE CLAIMS ALREADY READ. It used to re-list every style's claims on
// its own — a second walk over the same rows the builder had just collapsed.
//
// WHY THE CARD COUNTS PARTS AND NOT POSITIONS. It said "2 positions", which the
// card already says on its own line — so the count repeated the line above it
// and told the operator nothing they could act on. `used by 8 parts` is the
// thing that makes a card worth tapping: it says the press already runs this
// shape for eight other parts, which is the only evidence the floor has that a
// shape is the right one.
//
// Provenance, read off the claim rows, the same source the desktop's Used by
// column reads. NOT the shape compare: a member that has drifted is still a
// part that came from this preset, and the floor is not shown drift (§1b).
func presetMemberCounts(claims map[int64][]processes.NodeClaim) map[int64]int {
	out := map[int64]int{}
	for _, rows := range claims {
		for _, c := range rows {
			if c.SourcePresetID != nil {
				out[*c.SourcePresetID]++
				break // one part, counted once, however many positions it has
			}
		}
	}
	return out
}

// presetUse is the strip card's third line: how many parts run this shape.
//
// NO BIN WORD (owner ruling R2, 2026-09-12). It read `used by 7 parts · totes`,
// with the word appended in the render layer because it is a fact about the
// STYLE and the card is per-process — so the same card read `· totes` beside
// one part and `· bins` beside another. The press's parts do not all travel in
// one thing, and a card is not where an operator learns what a bin is.
//
// A shape nobody runs yet says so rather than saying "0 parts": a card the
// operator has never seen used is still a card they may tap, and a zero reads
// as a broken count.
func presetUse(members int) string {
	switch members {
	case 0:
		return "not used yet"
	case 1:
		return "used by 1 part"
	}
	return "used by " + strconv.Itoa(members) + " parts"
}

// composerScene reduces the geometry cache to the adjacency a waypoint search
// walks. Nil when there is no scene: the "Robot drives via" row then offers
// "shortest way" alone, because a waypoint list invented without a map is a
// route the robot cannot drive.
//
// COMPUTED ONCE PER CACHE, NOT PER REQUEST. The lengths are a pure function of
// the geometry, and the geometry is replaced only by a complete node-list
// response — so this is 840 square roots on a cache swap instead of 840 on
// every read. Keyed by the cache's identity rather than its revision: two
// caches can share a revision string, and the pointer cannot be wrong.
func (s *StationService) composerScene() *domain.ComposerScene {
	_, memo := s.sceneDerived()
	return memo
}

// sceneDerived returns the geometry cache and the adjacency derived from it,
// computing the derivation once per cache rather than once per request.
//
// Keyed by the cache's IDENTITY rather than its revision: two caches can
// carry the same revision string, and a pointer cannot be wrong. The cache is
// replaced only by a complete node-list response (NewSceneGeometry is
// all-or-nothing), so this recomputes on a sync and never on a poll.
func (s *StationService) sceneDerived() (*domain.SceneGeometry, *domain.ComposerScene) {
	if s.sceneGeometry == nil {
		return nil, nil
	}
	g := s.sceneGeometry()
	if g == nil || len(g.Edges) == 0 {
		return g, nil
	}
	s.sceneMu.Lock()
	defer s.sceneMu.Unlock()
	if s.sceneMemoFor != g {
		sc := &domain.ComposerScene{Edges: make([]domain.ComposerEdge, 0, len(g.Edges))}
		for _, e := range g.Edges {
			sc.Edges = append(sc.Edges, domain.ComposerEdge{
				From: e.From, To: e.To, Len: math.Hypot(e.ToX-e.FromX, e.ToY-e.FromY),
			})
		}
		s.sceneMemoFor, s.sceneMemo = g, sc
	}
	return g, s.sceneMemo
}

// routingFromBlock reads the routing set back off the block that was just
// built, so the picture's staging offers and the position panel's staging
// options are one list read once.
//
// COMPOSER ROWS, NOT STORE ROWS: what reaches this point has already been
// filtered to the enabled members (buildComposerData drops the rest into
// RoutingOff), so `Enabled` is true by construction and is set here to say so
// rather than left false for stagingOffers to mis-read.
func routingFromBlock(out *domain.ComposerData) []domain.RoutingNode {
	rows := make([]domain.RoutingNode, 0, len(out.Routing))
	for _, r := range out.Routing {
		rows = append(rows, domain.RoutingNode{
			CoreNodeName: r.CoreNodeName, Role: r.Role, Sequence: r.Sequence, Enabled: true,
		})
	}
	return rows
}
