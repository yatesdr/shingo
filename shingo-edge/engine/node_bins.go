package engine

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
)

// node_bins.go — the Edge's half of the node-bins feed (protocol.FeedBins):
// the bins at the nodes this Edge's operator stations draw.
//
// THE ONE FEED WHOSE SCOPE THE EDGE OWNS. Core serves an Edge the rows for the
// names on its watch list (bins.watch), so the Edge publishes the list: the
// names StationService.WatchedBinNodes builds from the plant, with no call to
// Core. Core quotes the digest of the list it holds on every heartbeat ack
// (EdgeHeartbeatAck.BinWatch); the Edge publishes when that differs from its
// own, at once when its own list changes, and on the first ack that carries
// the field while it holds no copy (a restarted Edge asks, rather than waiting
// out Core's resend guard). sendDue is the guard. It never publishes to a Core
// whose last ack lacked the field: that Core has no handler for the list.
//
// THE LIST GOES ON THE DIRECT PATH, NOT THE SQLITE OUTBOX: a lost publish is
// healed by the next ack, which still quotes the old digest, so the publish
// writes nothing on the Edge.
//
// THE COPY IS MEMORY ONLY. It changes with every delivery, so it has no
// feed_copy row (its feedRow in feeds.go has keepRow false, so holdFeed and
// OnCoreAck write none), and after a restart the Edge quotes "" and Core sends
// it again. Screens draw from it by the display rule (CurrentAt); engine paths
// read it for a group's members (groupMembers) and for the carrier a write
// names (boardBinID, the tile's bin); and the decisions on occupancy read it
// under the decision rule (BinsActable, through binsForDecision and binsRowAt),
// each failing closed on a copy that rule refuses. A loader's carriers are
// not decided from it: Core counts and places them (the loader retrieve ask).

// binWatchKey is the resend guard's key for the watch list.
const binWatchKey = "bins.watch"

// nodeBinsCopy is the held snapshot, indexed for the readers and for the
// change event.
type nodeBinsCopy struct {
	snap   protocol.NodeBinsSnapshot
	rows   map[string]protocol.NodeBinsRow
	assign map[string]protocol.NodeBinsAssignment
	wants  map[string][]protocol.NodeBinsWant
}

func indexNodeBins(snap protocol.NodeBinsSnapshot) *nodeBinsCopy {
	c := &nodeBinsCopy{
		snap:   snap,
		rows:   make(map[string]protocol.NodeBinsRow, len(snap.Rows)),
		assign: map[string]protocol.NodeBinsAssignment{},
		wants:  make(map[string][]protocol.NodeBinsWant, len(snap.Groups)),
	}
	for _, r := range snap.Rows {
		c.rows[r.Node] = r
	}
	for _, g := range snap.Groups {
		c.wants[g.Group] = g.Wants
		for _, a := range g.Assignments {
			c.assign[a.Node] = a
		}
	}
	return c
}

// changedSince names the nodes whose row or assignment differs from old, and
// the groups whose wants differ, each sorted. Everything is new against nil.
func (c *nodeBinsCopy) changedSince(old *nodeBinsCopy) (nodes, groups []string) {
	if old == nil {
		old = indexNodeBins(protocol.NodeBinsSnapshot{})
	}
	seen := map[string]bool{}
	for _, m := range []map[string]bool{nameSet(c.rows), nameSet(old.rows), nameSet(c.assign), nameSet(old.assign)} {
		for n := range m {
			if seen[n] {
				continue
			}
			seen[n] = true
			r, hadRow := old.rows[n]
			nr, hasRow := c.rows[n]
			a, hadA := old.assign[n]
			na, hasA := c.assign[n]
			if hadRow != hasRow || hadA != hasA || !reflect.DeepEqual(r, nr) || !reflect.DeepEqual(a, na) {
				nodes = append(nodes, n)
			}
		}
	}
	for g := range nameSet(c.wants) {
		if w, ok := old.wants[g]; !ok || !reflect.DeepEqual(w, c.wants[g]) {
			groups = append(groups, g)
		}
	}
	for g := range nameSet(old.wants) {
		if _, ok := c.wants[g]; !ok {
			groups = append(groups, g)
		}
	}
	sort.Strings(nodes)
	sort.Strings(groups)
	return nodes, groups
}

func nameSet[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// ApplyNodeBinsSnapshot takes a snapshot Core sent on bins.snapshot. The copy
// readers see is replaced, then the digest is held — only after the apply, so
// a snapshot that could not be applied leaves the old digest quoted and Core
// sends again. A snapshot that changed something fires EventNodeBinsUpdated
// naming what; an identical re-send only moves the received time. The list
// digest it answers is Core holding that watch list (heldByCore). Nothing is
// written to SQLite (pinned by TestNodeBins_NoSQLiteWrite).
func (e *Engine) ApplyNodeBinsSnapshot(snap protocol.NodeBinsSnapshot) error {
	if snap.Digest == "" {
		return fmt.Errorf("node-bins snapshot carries no digest")
	}
	next := indexNodeBins(snap)
	nodes, groups := next.changedSince(e.nodeBins.Load())
	e.nodeBins.Store(next)
	e.holdFeed(protocol.FeedBins, snap.Digest, "")
	if snap.ListDigest != "" {
		e.heldByCore(binWatchKey, snap.ListDigest)
	}
	if len(nodes) > 0 || len(groups) > 0 {
		e.Events.Emit(Event{
			Type:      EventNodeBinsUpdated,
			Timestamp: clock.Now(),
			Payload:   NodeBinsUpdatedEvent{Nodes: nodes, Groups: groups},
		})
	}
	return nil
}

// ApplyNodeBinsRows writes the rows Core answered a refused write with into
// the held copy, before the refusal's sentence is shown, so the tile redraws
// Core's row and not the one the write assumed. Each row replaces the row of
// the same name; a name the copy does not hold is not added (the copy answers
// the watch list, and the next snapshot is the list's). The copy keeps its
// digest: it still names the snapshot Core sent, so the next heartbeat
// compare fetches Core's current one. Fires EventNodeBinsUpdated for the
// nodes whose row changed. With no copy held the rows are dropped: there is
// nothing to patch. A snapshot applied meanwhile wins and the rows are dropped
// (it is the newer). Nothing is written to SQLite.
func (e *Engine) ApplyNodeBinsRows(rows []protocol.NodeBinsRow) {
	if len(rows) == 0 {
		return
	}
	old := e.nodeBins.Load()
	if old == nil {
		return
	}
	byName := make(map[string]protocol.NodeBinsRow, len(rows))
	for _, r := range rows {
		byName[r.Node] = r
	}
	snap := old.snap
	snap.Rows = make([]protocol.NodeBinsRow, len(old.snap.Rows))
	for i, r := range old.snap.Rows {
		if nr, ok := byName[r.Node]; ok {
			r = nr
		}
		snap.Rows[i] = r
	}
	next := indexNodeBins(snap)
	nodes, _ := next.changedSince(old)
	if len(nodes) == 0 || !e.nodeBins.CompareAndSwap(old, next) {
		return
	}
	e.Events.Emit(Event{
		Type:      EventNodeBinsUpdated,
		Timestamp: clock.Now(),
		Payload:   NodeBinsUpdatedEvent{Nodes: nodes},
	})
}

// refused applies the rows a refusal from Core carries (a *CoreRefusal) to the
// bins copy, then hands the error back unchanged for the caller to show.
func (e *Engine) refused(err error) error {
	var r *CoreRefusal
	if errors.As(err, &r) {
		e.ApplyNodeBinsRows(r.Rows)
	}
	return err
}

// NodeBinsState is the node-bins copy as this Edge holds it.
type NodeBinsState struct {
	// Rows are the held rows by node name; Groups the held group lines.
	Rows   map[string]protocol.NodeBinsRow
	Groups []protocol.NodeBinsGroup
	// ReceivedAt is when the held copy arrived; ConfirmedAt when a heartbeat
	// ack last quoted its digest back. Zero when never.
	ReceivedAt  time.Time
	ConfirmedAt time.Time
	// Held is false until a snapshot has been applied.
	Held bool
}

// LocalNodeBins is the node-bins copy as this Edge holds it, with its times
// and whether anything is held. Read-only, and nothing is withheld for age: a
// screen shows the time beside the value.
func (e *Engine) LocalNodeBins() NodeBinsState {
	st := NodeBinsState{}
	c := e.nodeBins.Load()
	if c == nil {
		return st
	}
	st.Rows, st.Groups, st.Held = c.rows, c.snap.Groups, true
	st.ReceivedAt, st.ConfirmedAt = e.FeedTimes(protocol.FeedBins)
	return st
}

// answerBinWatch takes an ack's BinWatch: nil is a Core without the feed, and
// nothing is published to it. Otherwise the list is published when the
// quoted digest differs from this Edge's, and on the first ack carrying the
// field while nothing is held, whatever was quoted.
func (e *Engine) answerBinWatch(quoted *string) {
	e.feeds.mu.Lock()
	first := !e.feeds.binWatchCarried
	e.feeds.binWatchCarried = quoted != nil
	if quoted != nil {
		e.feeds.binWatchQuoted = *quoted
	}
	e.feeds.mu.Unlock()
	if quoted == nil {
		return
	}
	e.publishBinWatch(*quoted, first && e.nodeBins.Load() == nil)
}

// binWatchChanged runs when the stations' bin lists were rebuilt to a
// different set of names: while Core serves the feed, the list goes at once.
func (e *Engine) binWatchChanged() {
	e.feeds.mu.Lock()
	speaks, quoted := e.feeds.binWatchCarried, e.feeds.binWatchQuoted
	e.feeds.mu.Unlock()
	if speaks {
		e.publishBinWatch(quoted, false)
	}
}

// binWatchList is this Edge's watch list and its digest; ok false when the
// digest cannot be computed (logged).
func (e *Engine) binWatchList() (nodes []string, digest string, ok bool) {
	if e.binWatchSource != nil {
		nodes = e.binWatchSource()
	}
	if nodes == nil {
		nodes = []string{}
	}
	digest, err := protocol.BinWatchDigest(nodes)
	if err != nil {
		e.logFn("feeds: bins watch digest: %v", err)
		return nil, "", false
	}
	return nodes, digest, true
}

// PublishBinWatchAtBoot sends the watch list in the heartbeater's boot burst,
// beside the register and the node-list and catalog requests, so a restarted
// Edge has its bins one round trip after it starts instead of one heartbeat
// later (Core answers a list with its snapshot at once, HandleBinWatch). It
// goes before any ack, so before this Edge knows whether Core serves the feed:
// an older Core drops the subject as one it does not handle. A send that went
// is recorded under the resend guard, so the first ack's own publish of the
// same list is held back while this one is in flight; a failed send records
// nothing, and the first ack asks as it always has. Pinned by
// TestBinWatch_PublishedAtBoot.
func (e *Engine) PublishBinWatchAtBoot(publish func(*protocol.BinWatchList) error) {
	nodes, digest, ok := e.binWatchList()
	if !ok || publish == nil {
		return
	}
	if err := publish(&protocol.BinWatchList{Digest: digest, Nodes: nodes}); err != nil {
		e.logFn("feeds: publish bins watch at boot (%d nodes): %v — the first ack asks again", len(nodes), err)
		return
	}
	e.sendDue(binWatchKey, digest, "")
}

// publishBinWatch sends this Edge's watch list when Core's quoted digest is
// not its own (or force), through the resend guard.
func (e *Engine) publishBinWatch(quoted string, force bool) {
	nodes, digest, ok := e.binWatchList()
	if !ok {
		return
	}
	if quoted == digest && !force {
		e.converged(binWatchKey)
		return
	}
	if !e.sendDue(binWatchKey, digest, quoted) {
		return
	}
	if e.sendFn == nil {
		return
	}
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectBinsWatch,
		protocol.Address{Role: protocol.RoleEdge, Station: e.cfg.StationID()},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.BinWatchList{Digest: digest, Nodes: nodes},
	)
	if err != nil {
		e.logFn("feeds: build bins watch: %v", err)
		return
	}
	if err := e.sendFn(env); err != nil {
		e.logFn("feeds: publish bins watch (%d nodes): %v — the next ack asks again", len(nodes), err)
	}
}

// BinsActable is Actable for this Edge's bins copy: whether a decision may be
// taken from it now, and why not. Every decision site reads it first, through
// binsForDecision and binsRowAt.
func (e *Engine) BinsActable() (ok bool, why string) {
	_, why = e.actableCopy()
	return why == "", why
}

// actableCopy is the held copy when it may be decided from, else nil and
// Actable's reason: the copy the rule judged, so a reader cannot judge one
// snapshot and read the next.
func (e *Engine) actableCopy() (*nodeBinsCopy, string) {
	c := e.nodeBins.Load()
	_, ownList, listOK := e.binWatchList()
	if !listOK {
		return nil, NotActableOtherList
	}
	e.feeds.mu.Lock()
	var received, confirmed time.Time
	if h := e.feeds.held[protocol.FeedBins]; h != nil {
		received, confirmed = h.receivedAt, h.confirmedAt
	}
	lastAck, now := e.feeds.lastAck, e.feeds.clock()
	e.feeds.mu.Unlock()
	copyList := ""
	if c != nil {
		copyList = c.snap.ListDigest
	}
	if ok, why := Actable(c != nil, copyList, ownList, received, confirmed, lastAck, now); !ok {
		return nil, why
	}
	return c, ""
}

// NotActableUnwatched is binsRowAt's reason for a name the copy holds no row
// for: it is not on this Edge's watch list, so Core was never asked about it.
const NotActableUnwatched = "not on the watch list"

// BinsNotCurrent is a decision about Node refused because the bins copy cannot
// answer for it now: the copy is not actable (Why is Actable's reason), or it
// holds no row for the node (NotActableUnwatched). Its sentence is what an
// operator door shows; an unattended pass logs it with Why and skips.
type BinsNotCurrent struct {
	Node string
	Why  string
}

func (err *BinsNotCurrent) Error() string {
	// OWNER-WORDS
	return fmt.Sprintf("Core's picture of %s is not current — try again", err.Node)
}

// binsWhy is the reason a BinsNotCurrent carries, for a log line; any other
// error's text.
func binsWhy(err error) string {
	var nc *BinsNotCurrent
	if errors.As(err, &nc) {
		return nc.Why
	}
	return err.Error()
}

// binsForDecision is the bins copy's rows for a decision about names, by
// name, when the copy may be decided from (BinsActable); a *BinsNotCurrent
// naming the first name otherwise. A name the copy holds no row for is absent
// from the map, and each caller reads an absent name as its own fail-closed
// answer, never as empty. An unknown name is present, with State unknown and
// Occupied false. Memory only: no statement, no call — but for a miss.
//
// A MISS REBUILDS THE WATCH LIST. A claim edit moves no generation the
// station lists are keyed on, so a place a save just named (a keep-staged
// spot, a market) is not watched until the lists rebuild. On a non-blank name
// with no row the lists are dropped and rebuilt now (the two statements of a
// rebuild), as groupMembers does: when the place is new the list moves, is
// published at once, and the decision after Core's answer finds the row.
func (e *Engine) binsForDecision(names ...string) (map[string]protocol.NodeBinsRow, error) {
	c, why := e.actableCopy()
	if c == nil {
		node := ""
		if len(names) > 0 {
			node = names[0]
		}
		return nil, &BinsNotCurrent{Node: node, Why: why}
	}
	out := make(map[string]protocol.NodeBinsRow, len(names))
	missed := false
	for _, n := range names {
		if r, ok := c.rows[n]; ok {
			out[n] = r
		} else if n != "" {
			missed = true
		}
	}
	if missed && e.stationService != nil {
		e.stationService.InvalidateBinNodes()
		e.stationService.WatchedBinNodes()
	}
	return out, nil
}

// binsRowAt is binsForDecision for one node, with a name the copy holds no
// row for refused as well (NotActableUnwatched).
func (e *Engine) binsRowAt(node string) (protocol.NodeBinsRow, error) {
	rows, err := e.binsForDecision(node)
	if err != nil {
		return protocol.NodeBinsRow{}, err
	}
	r, ok := rows[node]
	if !ok {
		return protocol.NodeBinsRow{}, &BinsNotCurrent{Node: node, Why: NotActableUnwatched}
	}
	return r, nil
}

// groupMembers is a group's members as Core answers them on the bins copy: the
// group row's members (non-synthetic children, qualified group.child, Core's
// own membership; protocol NodeBinsRow). known is false when the copy holds no
// group row for name: nothing held yet, a Core that sends no group rows, or a
// group this Edge does not watch. For the last, the station lists are dropped
// and rebuilt now, so a group a claim edit added is watched from here on and
// the next ask finds it (the lists are keyed on no claim generation).
func (e *Engine) groupMembers(name string) (members []string, known bool) {
	if c := e.nodeBins.Load(); c != nil {
		if r, ok := c.rows[name]; ok && r.State == protocol.BinsStateGroup {
			return r.Members, true
		}
	}
	if e.stationService != nil {
		e.stationService.InvalidateBinNodes()
		e.stationService.WatchedBinNodes()
	}
	return nil, false
}

// boardBinID is the resident bin the bins copy holds for a node: what the
// station's tile draws while Core serves the feed (www stampStationBins), so
// the carrier the operator saw. 0 with no copy held, no row for the node, or a
// row with no bin: the write then names no bin, as it did before it named one.
func (e *Engine) boardBinID(coreNode string) int64 {
	c := e.nodeBins.Load()
	if c == nil {
		return 0
	}
	if r, ok := c.rows[coreNode]; ok && r.Occupied {
		return r.BinID
	}
	return 0
}
