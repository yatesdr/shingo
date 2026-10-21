package messaging

import (
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strings"
	_ "sync" // the plain lock inlines only where sync is imported (protocol/lockcheck)
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/lockcheck"
	"shingocore/internal/lockorder"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// bins_feed.go — the node-bins feed (protocol.FeedBins): the bins at the nodes
// each Edge's operator stations draw.
//
// THE ONE FEED WITH A SCOPE THE EDGE OWNS. Every other feed is the same value
// for every Edge (or one Core works out for itself, like a station's node
// list). This one is the rows for the names the Edge sent on bins.watch, so
// Core keeps, per Edge, in memory: the list as it arrived, its digest (quoted
// back on every heartbeat ack as EdgeHeartbeatAck.BinWatch, as Claims are),
// and what the list was last found to point at — the node ids and bin ids of
// its last snapshot (a watched group's members included) and the outbound
// groups of the consume loaders with a window on it, with their windows
// (bins_feed_groups.go). Nothing is written: after a Core restart the ack
// quotes "" and the Edge publishes again.
//
// An Edge that never published a list is never sent a snapshot: buildFeed
// answers the key unknown for it, so nothing is logged, answered or sent.
//
// AFTER A KNOWN CHANGE, ONE TIMER PER DIRTY EDGE. A bin event at a watched
// node or group window, a station door (LOAD, count, CLEAR) on a watched bin,
// an order to or from a watched node or group, and a level save on a group
// (BinsLevelChanged) mark the Edges watching it dirty; each dirty
// Edge gets one time.AfterFunc, binsPushDelay later, so a burst of changes is
// one rebuild. The push sends only a digest that differs from the last one
// sent to that Edge, so a count that stays above zero sends nothing. A row
// carries its inbound orders, so an order starting or ending at a watched
// place now moves the digest and its push goes out where it used to be
// suppressed: those sends are bounded by order events, coalesced by the
// delay. Every other writer is healed by the next heartbeat's compare (the
// generation bumps that reach no push are named in
// TestBumpEpoch_EverySiteReachesABinsPushOrIsNamed).

// binsPushDelay coalesces the changes one Edge sees in a burst (a delivery
// emits several bin events) into one rebuild and at most one send.
const binsPushDelay = 300 * time.Millisecond

// binWatch is one Edge's watch list and what it was last found to point at.
type binWatch struct {
	digest string
	names  []string
	// nodeIDs and binIDs are those of the last snapshot built for this list.
	nodeIDs map[int64]bool
	binIDs  map[int64]bool
	// nodeNames holds every name an order may carry for a watched place: the
	// names as sent, a watched group's members as their rows name them, the
	// resolved nodes' own names, and the outbound groups.
	nodeNames map[string]bool
	// groups are the outbound groups of the consume loaders with a window on
	// the list, read when the list arrived (bins_feed_groups.go).
	groups []watchGroup
}

// binsFeed holds every Edge's watch list, the pending push per dirty Edge and
// the orders seen touching a watched place. One mutex, held only for map work:
// no read, build or send is made under it.
type binsFeed struct {
	mu    lockcheck.Mutex[lockorder.MessagingBins]
	lists map[string]*binWatch
	// pending is the push timer of each dirty Edge; an Edge is in it from the
	// first change until its push starts.
	pending map[string]*time.Timer
	// orders maps an order seen created to or from a watched place to the
	// Edges watching it, until the order ends. An order created before a Core
	// restart is not in it; its end is healed by the heartbeat.
	orders map[int64][]string
	delay  time.Duration
}

func newBinsFeed() *binsFeed {
	return &binsFeed{
		lists:   map[string]*binWatch{},
		pending: map[string]*time.Timer{},
		orders:  map[int64][]string{},
		delay:   binsPushDelay,
	}
}

// replace takes an Edge's new list. The names are kept sorted and each once,
// so the rows come out in the order the digest sorts them.
func (b *binsFeed) replace(station string, list *protocol.BinWatchList) {
	names := append([]string{}, list.Nodes...)
	sort.Strings(names)
	uniq := names[:0]
	for i, n := range names {
		if i == 0 || n != names[i-1] {
			uniq = append(uniq, n)
		}
	}
	w := &binWatch{digest: list.Digest, names: uniq, nodeNames: map[string]bool{}}
	for _, n := range uniq {
		w.nodeNames[n] = true
	}
	b.mu.Lock()
	b.lists[station] = w
	b.mu.Unlock()
}

// forget drops an Edge's list: an Edge that no longer quotes the bins feed
// (rolled back, or older) is sent nothing more.
func (b *binsFeed) forget(station string) {
	b.mu.Lock()
	delete(b.lists, station)
	b.mu.Unlock()
}

// watchingBin is the Edges whose last snapshot held this bin or whose list
// resolved to one of these nodes, sorted.
func (b *binsFeed) watchingBin(binID int64, nodeIDs []int64) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for station, w := range b.lists {
		hit := binID != 0 && w.binIDs[binID]
		for _, id := range nodeIDs {
			hit = hit || (id != 0 && (w.nodeIDs[id] || w.groupWindow(id)))
		}
		if hit {
			out = append(out, station)
		}
	}
	sort.Strings(out)
	return out
}

// watchingNames is the Edges with any of these names on their list, among the
// nodes it resolved to, or among its outbound groups, sorted.
func (b *binsFeed) watchingNames(names []string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for station, w := range b.lists {
		for _, n := range names {
			if n != "" && w.nodeNames[n] {
				out = append(out, station)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// noteOrder remembers the Edges an order touched when it was created, adding
// to any seen for it before (an order is seen at receipt and at dispatch).
func (b *binsFeed) noteOrder(orderID int64, stations []string) {
	if orderID == 0 || len(stations) == 0 {
		return
	}
	b.mu.Lock()
	seen := map[string]bool{}
	for _, st := range b.orders[orderID] {
		seen[st] = true
	}
	for _, st := range stations {
		if !seen[st] {
			b.orders[orderID] = append(b.orders[orderID], st)
		}
	}
	b.mu.Unlock()
}

// endOrder forgets an order and returns the Edges it touched.
func (b *binsFeed) endOrder(orderID int64) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	stations := b.orders[orderID]
	delete(b.orders, orderID)
	return stations
}

// markDirty arms one push per Edge not already waiting on one. push runs on the
// timer's goroutine.
func (b *binsFeed) markDirty(stations []string, push func(station string)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, st := range stations {
		if b.pending[st] != nil {
			continue
		}
		station := st
		b.pending[station] = time.AfterFunc(b.delay, func() { push(station) })
	}
}

// pushStarted takes an Edge out of the pending set as its push begins, so a
// change from here on arms another.
func (b *binsFeed) pushStarted(station string) {
	b.mu.Lock()
	delete(b.pending, station)
	b.mu.Unlock()
}

// list is the names an Edge watches and their digest; ok is false when Core
// holds no list for it.
func (b *binsFeed) list(station string) (names []string, digest string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w := b.lists[station]
	if w == nil {
		return nil, "", false
	}
	return w.names, w.digest, true
}

// digestFor is the digest of the list Core holds for an Edge, "" for none.
func (b *binsFeed) digestFor(station string) string {
	_, d, _ := b.list(station)
	return d
}

// indexBuild records what a snapshot built for the list with this digest found:
// the nodes its names resolved to (places: the watched names, a watched
// group's members by their row names, the levelled groups) and the bins
// standing there. A build for a list since replaced records nothing.
func (b *binsFeed) indexBuild(station, digest string, places map[string]*nodes.Node, rows []protocol.NodeBinsRow) {
	nodeIDs := make(map[int64]bool, len(places))
	for _, n := range places {
		nodeIDs[n.ID] = true
	}
	binIDs := make(map[int64]bool, len(rows))
	for _, r := range rows {
		if r.BinID != 0 {
			binIDs[r.BinID] = true
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	w := b.lists[station]
	if w == nil || w.digest != digest {
		return
	}
	w.nodeIDs, w.binIDs = nodeIDs, binIDs
	for name, n := range places {
		w.nodeNames[name] = true
		w.nodeNames[n.Name] = true
	}
}

// binWatchFor is the heartbeat ack's BinWatch: the digest of the list Core
// holds for this Edge, "" when none. nil for an Edge that sends no Feeds, which
// would not read it.
//
// An Edge whose heartbeat names Feeds but not FeedBins does not speak this
// feed (a binary from before it, or one rolled back to one): its list is
// dropped, so no snapshot is pushed to an Edge with no handler for it.
func (s *CoreDataService) binWatchFor(p *protocol.EdgeHeartbeat) *string {
	if p.Feeds == nil {
		s.bins.forget(p.StationID)
		return nil
	}
	if _, quoted := p.Feeds[protocol.FeedBins]; !quoted {
		s.bins.forget(p.StationID)
	}
	d := s.bins.digestFor(p.StationID)
	return &d
}

// HandleBinWatch takes an Edge's watch list (bins.watch). It replaces the list
// Core holds, finds the outbound groups of the consume loaders with a watched
// window (indexBinWatchGroups), drops that Edge's memo and sends its snapshot
// whatever was last sent, so a booted Edge has bins one round trip after its
// first ack.
//
// A failed build sends nothing: the ack already quotes the new list's digest,
// and the Edge's next heartbeat, quoting the snapshot digest it holds, retries
// the build through the feed compare.
func (s *CoreDataService) HandleBinWatch(env *protocol.Envelope, list *protocol.BinWatchList) {
	station := ""
	if env != nil { // a message off the wire always has one; some tests pass nil
		station = env.Src.Station
	}
	s.bins.replace(station, list)
	s.indexBinWatchGroups(station, list)
	s.feeds.dropStation(station, protocol.FeedBins)
	now := clock.Now()
	e, ok := s.feedCurrent(station, protocol.FeedBins, now)
	if ok {
		s.resp.sendData(protocol.SubjectBinsSnapshot, station, e.value)
		s.feeds.recordPush(station, protocol.FeedBins, e.digest, now)
	}
}

// buildNodeBins is the FeedBins build for one Edge: known is false when Core
// holds no list for it. The reads are one read snapshot (store.ReadSnapshot),
// so the orders and the bins agree: a torn read can show a carrier inbound and
// already occupied, never neither. Five statements whatever the list's length
// — BEGIN, the names and the levelled groups' names, the orders inbound to
// every place, the bins at every place and levelled group window, COMMIT —
// and a sixth, the members of every watched group in one, when a group is
// watched. Then, outside it, per held partial its line (markHeldLines) and per
// levelled group its levels and its line (groupLines). Pinned by
// TestBuildNodeBins_FiveStatementsWhateverTheListLength,
// TestBuildNodeBins_AWatchedGroupsMembers and TestBuildNodeBins_ALevelledGroupsLine.
func (s *CoreDataService) buildNodeBins(station string) (snap *protocol.NodeBinsSnapshot, known bool, err error) {
	names, listDigest, ok := s.bins.list(station)
	if !ok {
		return nil, false, nil
	}
	groups := s.levelledGroups(station, listDigest)
	var read watchRead
	if err := store.ReadSnapshot(s.db.DB, func(tx *sql.Tx) error {
		var rerr error
		read, rerr = readWatch(tx, names, groups)
		return rerr
	}); err != nil {
		return nil, true, err
	}
	snap = &protocol.NodeBinsSnapshot{ListDigest: listDigest, Rows: read.rows(names)}
	s.markHeldLines(snap.Rows)
	if snap.Groups, err = s.groupLines(station, listDigest, groups, names, read.resolved, read.first()); err != nil {
		return nil, true, err
	}
	if snap.Digest, err = protocol.NodeBinsDigest(*snap); err != nil {
		return nil, true, fmt.Errorf("bins digest: %w", err)
	}
	s.bins.indexBuild(station, listDigest, read.places(names), snap.Rows)
	return snap, true, nil
}

// watchRead is what one build read inside its snapshot.
type watchRead struct {
	// resolved is the watched names and the levelled groups' names that name a
	// node, keyed as asked.
	resolved map[string]*nodes.Node
	// members is each watched group's non-synthetic children, by name.
	members map[int64][]*nodes.Node
	// atNode is the bins at each node read, highest id first.
	atNode map[int64][]*bins.Bin
	// inbound is the orders the dropoff gate counts as on their way to each
	// place, in order id order.
	inbound map[int64][]protocol.NodeBinsInbound
}

// readWatch is the build's reads, orders before bins, through q (the build's
// read snapshot).
func readWatch(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, names []string, groups []watchGroup) (watchRead, error) {
	r := watchRead{atNode: map[int64][]*bins.Bin{}, inbound: map[int64][]protocol.NodeBinsInbound{}}
	var err error
	if r.resolved, err = nodes.GetByDotNames(q, append(append([]string{}, names...), groupNames(groups)...)); err != nil {
		return r, fmt.Errorf("bins: resolve watched names: %w", err)
	}
	if r.members, err = nodes.ListPhysicalChildren(q, watchedGroupIDs(names, r.resolved)); err != nil {
		return r, fmt.Errorf("bins: read watched groups' members: %w", err)
	}
	places := r.placeNodes(names)
	byName := deliveryNames(places)
	inbound, err := orders.ListInFlightByDeliveryNodes(q, sortedNames(byName))
	if err != nil {
		return r, fmt.Errorf("bins: read orders inbound to watched places: %w", err)
	}
	for _, in := range inbound {
		id := byName[in.DeliveryNode]
		r.inbound[id] = append(r.inbound[id], protocol.NodeBinsInbound{
			OrderUUID: in.EdgeUUID, BinType: in.BinTypeCode, Payload: in.PayloadCode, From: in.SourceNode})
	}
	atNodes, err := bins.ListByNodes(q, binsReadIDs(places, groups))
	if err != nil {
		return r, fmt.Errorf("bins: read bins at watched nodes: %w", err)
	}
	for _, b := range atNodes {
		if b.NodeID != nil {
			r.atNode[*b.NodeID] = append(r.atNode[*b.NodeID], b)
		}
	}
	return r, nil
}

// isNodeGroup is what makes a node a group for the bins row: its type, NGRP,
// the node class the dropoff gate and the resolvers treat as a group.
func isNodeGroup(n *nodes.Node) bool { return n.NodeTypeCode == protocol.NodeClassNGRP }

// watchedGroupIDs is the watched names that are groups, each once, sorted.
func watchedGroupIDs(names []string, resolved map[string]*nodes.Node) []int64 {
	seen := map[int64]bool{}
	var ids []int64
	for _, name := range names {
		if n := resolved[name]; n != nil && isNodeGroup(n) && !seen[n.ID] {
			seen[n.ID] = true
			ids = append(ids, n.ID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// placeNodes is the node of every row the build answers: each watched name
// that resolved and each watched group's members.
func (r watchRead) placeNodes(names []string) map[int64]*nodes.Node {
	out := map[int64]*nodes.Node{}
	for _, name := range names {
		if n := r.resolved[name]; n != nil {
			out[n.ID] = n
			for _, m := range r.members[n.ID] {
				out[m.ID] = m
			}
		}
	}
	return out
}

// deliveryNames is every name an order's delivery_node may carry that resolves
// to one of these nodes (as nodes.GetByDotName resolves it): the node's own
// name, when it holds no dot, and "parent.name", when its parent's holds none.
func deliveryNames(places map[int64]*nodes.Node) map[string]int64 {
	out := make(map[string]int64, 2*len(places))
	for id, n := range places {
		if !strings.Contains(n.Name, ".") {
			out[n.Name] = id
		}
		if n.ParentName != "" && !strings.Contains(n.ParentName, ".") {
			out[n.ParentName+"."+n.Name] = id
		}
	}
	return out
}

func sortedNames(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// first is the resident bin at each node read: the highest id there, as the
// HTTP answer takes it.
func (r watchRead) first() map[int64]*bins.Bin {
	first := make(map[int64]*bins.Bin, len(r.atNode))
	for id, at := range r.atNode {
		first[id] = at[0]
	}
	return first
}

// places is the row names the build answered, each with its node: the
// watched names that resolved, the members by their row names, and the
// levelled groups' names.
func (r watchRead) places(names []string) map[string]*nodes.Node {
	out := make(map[string]*nodes.Node, len(r.resolved))
	for name, n := range r.resolved {
		out[name] = n
	}
	for _, name := range names {
		if n := r.resolved[name]; n != nil {
			for _, m := range r.members[n.ID] {
				out[n.Name+"."+m.Name] = m
			}
		}
	}
	return out
}

// rows is one row per watched name in the names' order, each watched group's
// member rows directly after it, no name twice: a member already watched, or
// already answered under another name for the same group, keeps the one row.
func (r watchRead) rows(names []string) []protocol.NodeBinsRow {
	watched := make(map[string]bool, len(names))
	for _, name := range names {
		watched[name] = true
	}
	answered := map[string]bool{}
	rows := make([]protocol.NodeBinsRow, 0, len(names))
	for _, name := range names {
		n := r.resolved[name]
		row := r.row(name, n)
		if n == nil || !isNodeGroup(n) {
			rows = append(rows, row)
			continue
		}
		members := r.members[n.ID]
		for _, m := range members {
			row.Members = append(row.Members, n.Name+"."+m.Name)
		}
		rows = append(rows, row)
		for i, q := range row.Members {
			if !watched[q] && !answered[q] {
				answered[q] = true
				rows = append(rows, r.row(q, members[i]))
			}
		}
	}
	return rows
}

// row is the row for node n under name: unknown for no node, a group row for a
// group (its bins counted, its resident left out), else the resident bin's.
// Members are the caller's: only a watched group is expanded.
func (r watchRead) row(name string, n *nodes.Node) protocol.NodeBinsRow {
	if n == nil {
		return protocol.NodeBinsRow{Node: name, State: protocol.BinsStateUnknown}
	}
	at := r.atNode[n.ID]
	var row protocol.NodeBinsRow
	switch {
	case isNodeGroup(n):
		row = protocol.NodeBinsRow{Node: name, State: protocol.BinsStateGroup}
	case len(at) > 0:
		row = binRow(name, at[0])
	default:
		row = binRow(name, nil)
	}
	row.Bins = len(at)
	row.Inbound = r.inbound[n.ID]
	return row
}

// binRow is the row for bin b standing at a node called name; nil b is an
// empty row.
func binRow(name string, b *bins.Bin) protocol.NodeBinsRow {
	row := protocol.NodeBinsRow{Node: name, State: protocol.BinsStateEmpty}
	if b == nil {
		return row
	}
	row.State = protocol.BinsStateOccupied
	row.Occupied = true
	row.BinID = b.ID
	row.BinLabel = b.Label
	row.BinTypeCode = b.BinTypeCode
	row.Bare = b.BinTypeBare
	row.PayloadCode = b.PayloadCode
	row.Manifest = b.Manifest
	row.ManifestConfirmed = b.ManifestConfirmed
	row.Epoch = b.DeltaEpoch
	row.Stocked = b.UOPRemaining > 0
	row.TotesOnCart = b.TotesOnCart
	row.TotesPerLayer = b.TotesPerLayer
	if b.PayloadCode != "" {
		row.UOPCapacity = b.UOPCapacity
	}
	row.Partial = row.Stocked && b.UOPRemaining < row.UOPCapacity
	if b.HeldSince != nil {
		row.HeldSince = b.HeldSince.UTC().Format(time.RFC3339)
		row.HeldUOP = b.UOPRemaining
	}
	return row
}

// markHeldLines names the line each held partial row waits for: one read per
// HELD partial (HeldPartialInfo), none where no held partial stands. A failed
// read costs the row its line, never the snapshot.
func (s *CoreDataService) markHeldLines(rows []protocol.NodeBinsRow) {
	for i := range rows {
		if rows[i].HeldSince == "" {
			continue
		}
		_, line, held, err := s.db.HeldPartialInfo(rows[i].BinID)
		if err != nil {
			log.Printf("bins: held-partial read for bin %d: %v", rows[i].BinID, err)
			continue
		}
		if held {
			rows[i].HeldFor = line
		}
	}
}

// BinsChanged is called after a known write to a bin: a bin event (binID and
// the nodes it names), or a station door's LOAD, count or CLEAR (binID alone).
// The Edges whose last snapshot held the bin, or whose list resolved to one of
// the nodes, are pushed their snapshot binsPushDelay later.
func (s *CoreDataService) BinsChanged(binID int64, nodeIDs ...int64) {
	s.bins.markDirty(s.bins.watchingBin(binID, nodeIDs), s.pushBins)
}

// BinsOrderStarted is called when an order is created or dispatched, with its
// source and delivery node names. An order to or from a watched node or group
// pushes the Edges watching it, and is remembered until it ends.
func (s *CoreDataService) BinsOrderStarted(orderID int64, nodeNames ...string) {
	stations := s.bins.watchingNames(nodeNames)
	s.bins.noteOrder(orderID, stations)
	s.bins.markDirty(stations, s.pushBins)
}

// BinsOrderEnded is called when an order ends, however it ends. An order
// remembered by BinsOrderStarted pushes the Edges it touched.
func (s *CoreDataService) BinsOrderEnded(orderID int64) {
	s.bins.markDirty(s.bins.endOrder(orderID), s.pushBins)
}

// pushBins rebuilds one Edge's snapshot and sends it when its digest differs
// from the last one sent to that Edge. It records the send as a push: the
// compare's resend guard is left alone, so a lost push is sent again by the
// next heartbeat. A failed build, or an Edge whose list has since gone, sends
// nothing; the heartbeat retries.
func (s *CoreDataService) pushBins(station string) {
	s.bins.pushStarted(station)
	s.feeds.dropStation(station, protocol.FeedBins)
	now := clock.Now()
	e, ok := s.feedCurrent(station, protocol.FeedBins, now)
	if !ok || e.digest == s.feeds.lastSent(station, protocol.FeedBins) {
		return
	}
	s.resp.sendData(protocol.SubjectBinsSnapshot, station, e.value)
	s.feeds.recordPush(station, protocol.FeedBins, e.digest, now)
}
