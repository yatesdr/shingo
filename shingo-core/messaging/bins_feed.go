package messaging

import (
	"fmt"
	"sort"
	_ "sync" // the plain lock inlines only where sync is imported (protocol/lockcheck)
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/lockcheck"
	"shingocore/internal/lockorder"
	"shingocore/store/bins"
	"shingocore/store/nodes"
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
// its last snapshot and the outbound groups of the consume loaders with a
// window on it, with their windows (bins_feed_groups.go). Nothing is written: after a Core restart the ack quotes "" and
// the Edge publishes again.
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
// sent to that Edge, so a count that stays above zero sends nothing. Every
// other writer is healed by the next heartbeat's compare.

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
	// names as sent, the resolved nodes' own names, and the outbound groups.
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
// the nodes the names resolved to and the bins standing there. A build for a
// list since replaced records nothing.
func (b *binsFeed) indexBuild(station, digest string, resolved map[string]*nodes.Node, rows []protocol.NodeBinsRow) {
	nodeIDs := make(map[int64]bool, len(resolved))
	for _, n := range resolved {
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
	for _, n := range resolved {
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
// holds no list for it. Two statements whatever the list's length (the names
// and the levelled groups' names, then the bins at every resolved node and
// every levelled group's windows), the reads GET /api/telemetry/node-bins
// makes; then, per levelled group, its levels and its line (groupLines).
// Pinned by TestBuildNodeBins_TwoStatementsWhateverTheListLength and
// TestBuildNodeBins_ALevelledGroupsLine.
func (s *CoreDataService) buildNodeBins(station string) (snap *protocol.NodeBinsSnapshot, known bool, err error) {
	names, listDigest, ok := s.bins.list(station)
	if !ok {
		return nil, false, nil
	}
	groups := s.levelledGroups(station, listDigest)
	resolved, err := s.db.GetNodesByDotNames(append(append([]string{}, names...), groupNames(groups)...))
	if err != nil {
		return nil, true, fmt.Errorf("bins: resolve watched names: %w", err)
	}
	atNodes, err := s.db.ListBinsByNodes(binsReadIDs(names, resolved, groups))
	if err != nil {
		return nil, true, fmt.Errorf("bins: read bins at watched nodes: %w", err)
	}
	first := firstBins(atNodes)
	snap = &protocol.NodeBinsSnapshot{Rows: nodeBinsRows(names, resolved, first)}
	if snap.Groups, err = s.groupLines(station, listDigest, groups, names, resolved, first); err != nil {
		return nil, true, err
	}
	if snap.Digest, err = protocol.NodeBinsDigest(*snap); err != nil {
		return nil, true, fmt.Errorf("bins digest: %w", err)
	}
	s.bins.indexBuild(station, listDigest, resolved, snap.Rows)
	return snap, true, nil
}

// firstBins is the bin at each node: the first the read returns there (ORDER
// BY id DESC), as the HTTP answer takes it.
func firstBins(atNodes []*bins.Bin) map[int64]*bins.Bin {
	first := make(map[int64]*bins.Bin, len(atNodes))
	for _, b := range atNodes {
		if b.NodeID != nil && first[*b.NodeID] == nil {
			first[*b.NodeID] = b
		}
	}
	return first
}

// nodeBinsRows is one row per name, in the names' order. A name that did not
// resolve, or a node with no bin, is a row with only its name.
func nodeBinsRows(names []string, resolved map[string]*nodes.Node, first map[int64]*bins.Bin) []protocol.NodeBinsRow {
	rows := make([]protocol.NodeBinsRow, 0, len(names))
	for _, name := range names {
		var b *bins.Bin
		if n := resolved[name]; n != nil {
			b = first[n.ID]
		}
		rows = append(rows, binRow(name, b))
	}
	return rows
}

// binRow is the row for bin b standing at a node called name; nil b is a row
// with only the name.
func binRow(name string, b *bins.Bin) protocol.NodeBinsRow {
	row := protocol.NodeBinsRow{Node: name}
	if b == nil {
		return row
	}
	row.Occupied = true
	row.BinID = b.ID
	row.BinLabel = b.Label
	row.BinTypeCode = b.BinTypeCode
	row.Bare = b.BinTypeBare
	row.PayloadCode = b.PayloadCode
	row.Manifest = b.Manifest
	row.ManifestConfirmed = b.ManifestConfirmed
	row.Stocked = b.UOPRemaining > 0
	row.TotesOnCart = b.TotesOnCart
	row.TotesPerLayer = b.TotesPerLayer
	return row
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
