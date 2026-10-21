package service

import (
	"log"
	"slices"
	"sort"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// station_bins.go — the bins a station's board draws, and what the board is
// told about them.
//
// ONE LIST PER STATION, BUILT WHEN THE PLANT CHANGES, NEVER PER BUILD. A board
// reads the bins at its tiles' nodes, at its dedicated loaders' buffer spots
// (the buffer strip and a home's CLEAR BIN) and at its consume loaders'
// outbound-group members (the empties). The names come from process_nodes, the
// loaders Core sent and the group membership the node list carried, so the
// list is held and rebuilt only when one of the three has moved: one statement
// on the first build after a change, none on the next. The station view reads
// every name in one node-bins call (www stampStationBins), pinned by
// TestStationView_OneNodeBinsReadPerBuild (www).

// binNodes is every station's bin list as of one plant state.
type binNodes struct {
	key    binNodesKey
	loaded bool
	// byStation is each station's list, sorted, each name once.
	byStation map[int64][]string
	// all is every station's list together, sorted, each name once.
	all []string
	// outbound is the members of each listed consume loader's outbound group,
	// by group name, in the group's order.
	outbound map[string][]string
}

// binNodesKey is the plant state a list was built from: the process_nodes
// generation, the node list's (which replaces the group membership) and the
// loaders'.
type binNodesKey struct{ nodes, plant, loaders uint64 }

// SetLoaderGenerationResolver injects the counter that moves whenever the
// Edge's copy of Core's loaders is replaced, so the board's bin lists know to
// rebuild. Optional; unset reads as 0 (an Edge with no engine, whose loaders do
// not change either).
func (s *StationService) SetLoaderGenerationResolver(r func() uint64) { s.loaderGeneration = r }

func (s *StationService) binNodesKeyNow() binNodesKey {
	k := binNodesKey{nodes: processes.NodeGeneration(), plant: s.plantGen()}
	if s.loaderGeneration != nil {
		k.loaders = s.loaderGeneration()
	}
	return k
}

// BinNodes is the Core node names a station's board reads bins at: its tiles'
// nodes (and the stationless positions of its processes a changeover adopts
// onto it), its dedicated loaders' buffer spots and its consume loaders'
// outbound-group members. Sorted, each once; nil when the station has none or
// the list could not be read.
func (s *StationService) BinNodes(stationID int64) []string {
	return s.heldBinNodes().byStation[stationID]
}

// WatchedBinNodes is BinNodes over every station: the Core node names this
// Edge's operator stations draw, sorted, each once. It is the bins watch list
// this Edge publishes to Core; built from the same held lists, so it costs no
// statement between plant changes and never a call to Core.
func (s *StationService) WatchedBinNodes() []string {
	return s.heldBinNodes().all
}

// SetBinNodesChangedFunc injects what runs when a rebuild of the lists finds
// the watched names (WatchedBinNodes) different from the last build's: the
// engine publishes its watch list at once. Called after the lists are held,
// with no lock held. Optional.
func (s *StationService) SetBinNodesChangedFunc(fn func()) { s.binNodesChanged = fn }

// heldBinNodes is the lists, rebuilt when the plant has moved since they were
// built. A failed read is not held: this build gets no lists and the next
// tries again.
func (s *StationService) heldBinNodes() binNodes {
	s.binsMu.Lock()
	key := s.binNodesKeyNow()
	if s.bins.loaded && s.bins.key == key {
		held := s.bins
		s.binsMu.Unlock()
		return held
	}
	rows, err := s.db.ListProcessNodes()
	if err != nil {
		s.binsMu.Unlock()
		log.Printf("station view: read process nodes for the bin lists: %v", err)
		return binNodes{}
	}
	built := s.buildBinNodes(rows)
	built.key, built.loaded = key, true
	changed := !slices.Equal(built.all, s.bins.all) || !s.bins.loaded
	s.bins = built
	s.binsMu.Unlock()
	if changed && s.binNodesChanged != nil {
		s.binNodesChanged()
	}
	return built
}

// buildBinNodes builds every station's list from the live process nodes.
//
// A STATIONLESS NODE IS LISTED ON EVERY STATION OF ITS PROCESS: a press-index
// extension position has no station of its own, and a changeover adopts it
// onto the board running it (adoptChangeoverParticipants). Listing it ahead of
// the changeover keeps the list a function of the plant, not of the moment.
func (s *StationService) buildBinNodes(rows []processes.Node) binNodes {
	groups := s.GroupMembers()
	stationsOf := map[int64][]int64{}
	for _, n := range rows {
		if n.OperatorStationID != nil && !slices.Contains(stationsOf[n.ProcessID], *n.OperatorStationID) {
			stationsOf[n.ProcessID] = append(stationsOf[n.ProcessID], *n.OperatorStationID)
		}
	}
	out := binNodes{byStation: map[int64][]string{}, outbound: map[string][]string{}}
	sets := map[int64]map[string]bool{}
	for _, n := range rows {
		if n.CoreNodeName == "" {
			continue
		}
		names := append([]string{n.CoreNodeName}, s.loaderBinNodes(n.CoreNodeName, groups, out.outbound)...)
		targets := stationsOf[n.ProcessID]
		if n.OperatorStationID != nil {
			targets = []int64{*n.OperatorStationID}
		}
		for _, sid := range targets {
			if sets[sid] == nil {
				sets[sid] = map[string]bool{}
			}
			for _, name := range names {
				sets[sid][name] = true
			}
		}
	}
	all := map[string]bool{}
	for sid, set := range sets {
		out.byStation[sid] = sortedNames(set)
	}
	for _, set := range sets {
		for name := range set {
			all[name] = true
		}
	}
	out.all = sortedNames(all)
	return out
}

// loaderBinNodes is the names a loader tile at coreNode adds to its board's
// list: a dedicated loader's buffer spots; a consume loader's outbound-group
// members, recorded in outbound. A stage-1 unloader adds none: its group is
// stage 2's windows, which no board reads as empties.
func (s *StationService) loaderBinNodes(coreNode string, groups, outbound map[string][]string) []string {
	if s.loaders == nil {
		return nil
	}
	l, err := s.loaders.LoaderForNode(domain.NodeID(coreNode))
	if err != nil || l == nil {
		return nil
	}
	var names []string
	if l.IsDedicated() {
		for _, pos := range l.Positions() {
			if pos.IsBuffer() {
				names = append(names, string(pos.Node))
			}
		}
	}
	if l.Role() == domain.RoleConsume && !l.LeavesBare() {
		if members := groups[l.OutboundDest()]; len(members) > 0 {
			outbound[l.OutboundDest()] = members
			names = append(names, members...)
		}
	}
	return names
}

func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// BoardBin is one node's bin as the board reads it: Core's node-bins row, from
// the Edge's copy of the feed or over HTTP. The board asks whether a bin holds
// parts (Stocked), never how many.
type BoardBin struct {
	protocol.NodeBinsRow
	// UOPRemaining is the bin's count when read over HTTP; nil from the
	// copy, which carries Stocked and no count.
	UOPRemaining *int
}

// BoardBinsFromHTTP is a node-bins answer as board bins, by node name: Stocked
// is the count above zero, and the count is kept for the tile's bin.
func BoardBinsFromHTTP(infos []protocol.NodeBinInfo) map[string]BoardBin {
	out := make(map[string]BoardBin, len(infos))
	for _, b := range infos {
		uop := b.UOPRemaining
		out[b.NodeName] = BoardBin{NodeBinsRow: protocol.NodeBinsRow{
			Node: b.NodeName, Occupied: b.Occupied, BinID: b.BinID, BinLabel: b.BinLabel,
			BinTypeCode: b.BinTypeCode, Bare: b.Bare, PayloadCode: b.PayloadCode, Manifest: b.Manifest,
			ManifestConfirmed: b.ManifestConfirmed, Stocked: uop > 0,
			TotesOnCart: b.TotesOnCart, TotesPerLayer: b.TotesPerLayer,
			UOPCapacity: b.UOPCapacity, HeldSince: b.HeldSince, HeldFor: b.HeldFor,
		}, UOPRemaining: &uop}
	}
	return out
}

// BoardBinsFromCopy is the Edge's copy of the node-bins feed as board bins.
func BoardBinsFromCopy(rows map[string]protocol.NodeBinsRow) map[string]BoardBin {
	out := make(map[string]BoardBin, len(rows))
	for name, r := range rows {
		out[name] = BoardBin{NodeBinsRow: r}
	}
	return out
}

// StampBins puts what one read of the board's list found onto the view:
//
//   - each tile's bin_state, from its node's row;
//   - once per dedicated loader with buffer spots, the strip (loader.buffer)
//     in the plant's order, on the loader of its first home in that order
//     that is a tile of this board (stripTile);
//   - on a home holding a stocked bin of its part, the spot whose partial its
//     clear would bring home (buffer_partial);
//   - the board's empties, from its consume loader's outbound group.
//
// read is false when the bins are not known (the read failed, or no copy is
// held): no tile gets a bin, the strip's spots are unknown, and nothing else
// is stamped.
func (s *StationService) StampBins(view *domain.OperatorStationView, rows map[string]BoardBin, read bool) {
	held := s.heldBinNodes()
	stripped := map[domain.LoaderID]bool{}
	for i := range view.Nodes {
		tile := &view.Nodes[i]
		name := tile.Node.CoreNodeName
		if row, ok := rows[name]; ok && name != "" {
			tile.BinState = nodeBinState(row)
			if row.UOPRemaining == nil {
				stocked := row.Stocked
				tile.BinStocked = &stocked
			}
		}
		if tile.Loader == nil || s.loaders == nil {
			continue
		}
		l, err := s.loaders.LoaderForNode(domain.NodeID(name))
		if err != nil || l == nil || !l.IsDedicated() {
			continue
		}
		if _, done := stripped[l.ID()]; !done && name == stripTile(l, view.Nodes) {
			stripped[l.ID()] = true
			tile.Loader.Buffer = bufferStrip(l, view.Nodes, rows, read)
		}
		tile.BufferPartial = bufferPartialFor(l, name, rows)
	}
	if read {
		view.Empties = s.boardEmpties(view, held.outbound, rows)
	}
}

// nodeBinState is a board bin as a tile's bin; the count is 0 when the read
// carried none (the tile's BinStocked says then). A bare cart's type is the
// marker Core stamps between the stages of a two-stage unloader, bookkeeping
// and never an operator's word, so only the bare flag reaches the tile (the
// rule enrichViewBinState applies on the Production page;
// TestEnrichViewBinState_BareCartCarriesNoMarkerCode).
func nodeBinState(row BoardBin) *domain.NodeBinState {
	binTypeCode := row.BinTypeCode
	if row.Bare {
		binTypeCode = ""
	}
	var uop int
	if row.UOPRemaining != nil {
		uop = *row.UOPRemaining
	} else if row.HeldSince != "" {
		uop = row.HeldUOP // a held partial's count, which the copy carries
	}
	return &domain.NodeBinState{
		BinID:             row.BinID,
		BinLabel:          row.BinLabel,
		BinTypeCode:       binTypeCode,
		Bare:              row.Bare,
		PayloadCode:       row.PayloadCode,
		UOPRemaining:      uop,
		Manifest:          row.Manifest,
		ManifestConfirmed: row.ManifestConfirmed,
		Occupied:          row.Occupied,
		UOPCapacity:       row.UOPCapacity,
		HeldSince:         row.HeldSince,
		HeldFor:           row.HeldFor,
	}
}

// stripTile is the tile that carries a dedicated loader's buffer strip: its
// first home, in the plant's order, that is a tile of this board. ONCE PER
// LOADER, NOT PER TILE: the strip is the loader's, and on every tile it was
// the same list repeated (about 7 KB raw on a 22-home board). The key keeps
// the wire's name, tile.loader.buffer; the glass reads it from the tile that
// has it. "" when no home of the loader is on the board. Pinned by
// TestStampBins_OneTileOfALoaderCarriesItsStrip.
func stripTile(l *domain.Loader, tiles []domain.StationNodeView) string {
	onBoard := make(map[string]bool, len(tiles))
	for _, t := range tiles {
		onBoard[t.Node.CoreNodeName] = true
	}
	for _, pos := range l.Positions() {
		if !pos.IsBuffer() && onBoard[string(pos.Node)] {
			return string(pos.Node)
		}
	}
	return ""
}

// bufferStrip is a dedicated loader's buffer spots in the plant's order (the
// order Positions keeps), each with what stands there; nil for a loader with
// no buffer spot.
//
// A SPOT WHOSE CLEAR IS IN FLIGHT COUNTS AS AN EMPTY. ClearLoaderHome sends
// the home's emptied carrier to the spot (Order A, a move with no part, on the
// home's orders) and only then brings the partial home, so the spot is about
// to hold an empty: the buffer is not "full with no empties" while that clear
// runs.
func bufferStrip(l *domain.Loader, tiles []domain.StationNodeView, rows map[string]BoardBin, read bool) []domain.BufferSpot {
	clearing := map[string]bool{}
	for _, tile := range tiles {
		if !l.Contains(domain.NodeID(tile.Node.CoreNodeName)) {
			continue
		}
		for _, o := range tile.Orders {
			if o.PayloadCode == "" && o.SourceNode == tile.Node.CoreNodeName && !protocol.IsTerminal(o.Status) {
				clearing[o.DeliveryNode] = true
			}
		}
	}
	var strip []domain.BufferSpot
	for _, pos := range l.Positions() {
		if !pos.IsBuffer() {
			continue
		}
		node := string(pos.Node)
		state := domain.SpotUnknown
		if row, ok := rows[node]; read && ok {
			state = spotState(row)
		}
		if clearing[node] {
			state = domain.SpotEmpty
		}
		strip = append(strip, domain.BufferSpot{Node: node, State: state})
	}
	return strip
}

// spotState is what a read buffer spot holds (a ruling on the brief's C3:
// "Unknown spots (occupied with a count of 0 or below, occupied with no
// payload, or not read) are neither open nor empty"):
//
//   - open: nothing stands there;
//   - empty: occupied, no part, not stocked. Taken literally, "occupied with
//     no payload" is unknown, but that is exactly what a clear leaves (Core
//     empties a carrier to a blank part and a count of 0, emptiedCarrierSET),
//     so read literally no spot could ever be empty; this reading was ruled;
//   - partial: a part, stocked;
//   - unknown: a part that is not stocked, a stocked bin with no part, or a
//     spot not read (bufferStrip).
//
// The wire carries these four states only. A spot a home's clear is in flight
// to is stamped empty (bufferStrip); the glass draws it as coming from the
// home's orders, where the clear's Order A stands. Pinned by
// TestStampBins_BufferStrip.
func spotState(row BoardBin) string {
	switch {
	case !row.Occupied:
		return domain.SpotOpen
	case row.PayloadCode == "" && !row.Stocked:
		return domain.SpotEmpty
	case row.PayloadCode != "" && row.Stocked:
		return domain.SpotPartial
	default:
		return domain.SpotUnknown
	}
}

// bufferPartialFor is the spot whose partial a clear of the home at node would
// bring home: the home holds a stocked bin of its own part, and the first
// buffer spot in the plant's order holds a stocked bin of the same part. The
// test ClearLoaderHome makes, on the same rows, so the board and the act pick
// the same spot. Nil when node is not a home or no spot qualifies.
func bufferPartialFor(l *domain.Loader, node string, rows map[string]BoardBin) *domain.BufferPartial {
	if _, pinned := l.PayloadAt(domain.NodeID(node)); !pinned {
		return nil
	}
	home, ok := rows[node]
	if !ok || !home.Occupied || !home.Stocked || home.PayloadCode == "" {
		return nil
	}
	for _, pos := range l.Positions() {
		spot := string(pos.Node)
		if !pos.IsBuffer() || spot == node {
			continue
		}
		if row, ok := rows[spot]; ok && row.Occupied && row.PayloadCode == home.PayloadCode && row.Stocked {
			return &domain.BufferPartial{Node: spot}
		}
	}
	return nil
}

// boardEmpties is the empties of the board's first consume loader (in tile
// order) whose outbound group the list holds: per bin type a served press
// fills or one standing empty in the group, how many empties of it stand
// there; and the group's open spots. Nil when the board has no such loader.
// An empty is an occupied bin with no part that is not stocked; a bare cart is
// not one.
func (s *StationService) boardEmpties(view *domain.OperatorStationView, outbound map[string][]string, rows map[string]BoardBin) *domain.Empties {
	group := ""
	for _, tile := range view.Nodes {
		if tile.Loader == nil || tile.Loader.Role != string(domain.RoleConsume) || tile.Loader.LeavesBare {
			continue
		}
		if _, ok := outbound[tile.Loader.OutboundDest]; ok {
			group = tile.Loader.OutboundDest
			break
		}
	}
	if group == "" {
		return nil
	}
	have := map[string]int{}
	for _, row := range view.Serves {
		if row.Role == string(protocol.ClaimRoleProduce) && row.Carrier != "" {
			have[row.Carrier] += 0
		}
	}
	open := 0
	for _, member := range outbound[group] {
		row, ok := rows[member]
		switch {
		case !ok:
		case !row.Occupied:
			open++
		case row.PayloadCode == "" && !row.Stocked && !row.Bare && row.BinTypeCode != "":
			have[row.BinTypeCode]++
		}
	}
	out := &domain.Empties{Group: group, Rows: make([]domain.EmptiesRow, 0, len(have))}
	for binType, n := range have {
		out.Rows = append(out.Rows, domain.EmptiesRow{BinType: binType, Have: n, Spots: open})
	}
	sort.Slice(out.Rows, func(i, j int) bool { return out.Rows[i].BinType < out.Rows[j].BinType })
	return out
}
