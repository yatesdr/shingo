package engine

import (
	"fmt"
	"sort"
	_ "sync" // the plain lock inlines only where sync is imported (protocol/lockcheck)
	"time"

	"shingo/protocol"
	"shingo/protocol/lockcheck"
	"shingo/protocol/resend"
	"shingoedge/internal/lockorder"
	"shingoedge/store"
)

// feeds.go — the Edge's half of the feed digests (protocol/feeds.go has the rule).
//
// No goroutine and no timer. The heartbeat asks FeedDigests what this Edge holds;
// the ack handler calls OnCoreAck with Core's answer. A feed's copy is replaced
// only by the data Core sends, and its digest is recorded only after that data
// applied cleanly, so a failed apply leaves the old digest and Core sends again.
//
// NOTHING IS BLANKED FOR BEING OLD. Readers get the last value Core sent and the
// time Core last confirmed it; a screen shows that time when it is old. There is
// no second, direct read of Core behind it.

const (
	// FeedAsOfAfter is how old a confirmation may be before a screen says
	// "as of" beside the value: two missed heartbeats and a half.
	FeedAsOfAfter = 150 * time.Second
	// feedConfirmPersistEvery bounds how often a confirmation alone is
	// written to feed_copy. The time only matters after a restart with Core
	// unreachable, where an "as of" a few minutes early is honest and a write
	// per row per heartbeat would be 1,440 a day each.
	feedConfirmPersistEvery = 5 * time.Minute
	// feedAckGap is the silence after which the next ack reconciles orders.
	// Core's order projections live 5 minutes on the wire and Core calls a
	// station stale only after 15, so a projection pushed during a 5-15
	// minute outage expired with nothing to repair it. Four minutes sits
	// inside the data TTL and well under the stale threshold.
	feedAckGap = 4 * time.Minute
)

// feedHold is what this Edge holds of one Core feed.
type feedHold struct {
	digest      string
	body        string
	receivedAt  time.Time
	confirmedAt time.Time
	// persistedAt is when this row was last written to feed_copy.
	persistedAt time.Time
}

// feedBook is every held feed plus what the last ack said about Core.
type feedBook struct {
	mu   lockcheck.Mutex[lockorder.Feeds]
	held map[string]*feedHold
	// coreSpeaksFeeds is true once an ack carried a Feeds map. Until then,
	// and whenever an older Core answers, the Edge keeps its own timers.
	coreSpeaksFeeds bool
	// owned is the cache of every Edge-owned family (owned.go): what this Edge
	// holds, what the last heartbeat quoted and what Core held at the last ack.
	// ownedSeq orders its builds; ownedFloorAt is when every family was last
	// built together (ownedFloor).
	owned        map[string]*ownedHeld
	ownedSeq     uint64
	ownedFloorAt time.Time
	// lastAck is the local (monotonic) time of the last ack; lastServerTS is
	// Core's stamp on it.
	lastAck      time.Time
	lastServerTS time.Time
	// sent and flags are the Edge's side of the resend guard, for the data
	// this Edge owns and sends to Core (owned.go: its plant claims, its bins
	// watch list, its cell catalog): the same rule Core applies to its feeds
	// (protocol/resend).
	sent  map[string]*resend.Record
	flags map[string]string
	// reconcile is what an ack after a gap runs; nil means StartupReconcile.
	// A field so a test can count it.
	reconcile func() error
	// now is this Edge's feed clock (clock): it stamps acks, ages the held
	// copies and times the resend guard; nil means time.Now. A field
	// so a test can run the Edge's clock ahead of or behind Core's — the rig
	// cannot shift one container's wall clock.
	now func() time.Time
	// stale is, per aged feed (feedRow.aged), whether the copy was past
	// FeedAsOfAfter when last looked at (crossingsLocked).
	stale map[string]bool
	// ackObserver is told after every ack (SetCoreAckObserver); nil for none.
	ackObserver func()
}

// feedAged is who is told when a held copy's age crosses FeedAsOfAfter:
// nobody (no screen shows its age), the boards (the station view's "Bins as
// of", "Catalog as of": www station_bins.go, station_catalog.go), or the
// containment page (www handlers_containment.go, which reloads on
// EventContainmentUpdated).
type feedAged int

const (
	notAged feedAged = iota
	agedOnBoard
	agedOnContainment
)

// feedRow is everything the Edge says about one Core feed, in one place. A
// feed's facts were six lists, and two had drifted: the Core-link page left
// bins out, and its as-of rule was not the screens'. TestFeedRows_CoverAllFeeds
// fails when a key of protocol.AllFeeds has no row.
type feedRow struct {
	key string
	// quote is the digest the heartbeat names for the feed; ok false leaves
	// the key out of the heartbeat (FeedDigests).
	quote func(e *Engine, key string) (digest string, ok bool)
	// persistDigest: the digest and body are written to feed_copy, only where
	// the data they name survives a restart too. The node list is memory-only,
	// so a persisted nodes digest would quote a copy the Edge no longer has and
	// Core would never resend it; the scene revision and the refusal digest are
	// recomputed from their own stores.
	persistDigest bool
	// keepRow: the feed has a feed_copy row at all (its times). The node-bins
	// copy has none: it changes with every delivery, a row would be a SQLite
	// write per bin movement, and after a restart the Edge quotes "" and Core
	// sends it again. Pinned by TestNodeBins_NoSQLiteWrite.
	keepRow bool
	// aged is who a crossing of FeedAsOfAfter is announced to.
	aged feedAged
}

// feedRows has one row per Core feed. The row order is the Core-link order:
// the order /status and Diagnostics list the feeds in (FeedKeys), and the
// order FeedDigests quotes them in.
var feedRows = [...]feedRow{
	{key: protocol.FeedContainment, quote: quoteHeld, persistDigest: true, keepRow: true, aged: agedOnContainment},
	{key: protocol.FeedRefusals, quote: quoteRefusals, keepRow: true},
	{key: protocol.FeedNodes, quote: quoteHeld, keepRow: true},
	{key: protocol.FeedScene, quote: quoteScene, keepRow: true},
	{key: protocol.FeedCatalog, quote: quoteHeld, persistDigest: true, keepRow: true, aged: agedOnBoard},
	// Always quoted, "" while nothing is held: Core drops an Edge's watch
	// list when its heartbeat does not name the key (a build without the feed).
	{key: protocol.FeedBins, quote: quoteHeld, aged: agedOnBoard},
	// Quoted from the cache itself (quoteSourcing), so its digest needs no
	// persisting and a delta that never arrived shows as a different digest.
	{key: protocol.FeedSourcing, quote: quoteSourcing, keepRow: true},
}

// feedRowOf is the row for a feed key; the zero row (nothing persisted, no
// feed_copy row, not aged) for a key that has none.
func feedRowOf(key string) feedRow {
	for _, f := range feedRows {
		if f.key == key {
			return f
		}
	}
	return feedRow{}
}

// FeedKeys is every Core feed, in the order the Core-link page lists them.
func FeedKeys() []string {
	out := make([]string, len(feedRows))
	for i, f := range feedRows {
		out[i] = f.key
	}
	return out
}

// quoteHeld quotes the digest that arrived with the copy, "" when none.
func quoteHeld(e *Engine, key string) (string, bool) { return e.heldDigest(key), true }

// quoteRefusals quotes the digest of this Edge's own open refusal table
// (refusalsDigest), left out when it could not be read or the last ack came
// from a Core that does not answer feeds.
func quoteRefusals(e *Engine, _ string) (string, bool) { return e.refusalsDigest() }

// quoteSourcing quotes the digest of this Edge's sourcing cache (sourcingDigest).
func quoteSourcing(e *Engine, _ string) (string, bool) { return e.sourcingDigest() }

// quoteScene quotes the scene revision, which lives with the geometry, and
// notes it so the ack can confirm it.
func quoteScene(e *Engine, key string) (string, bool) {
	rev := e.SceneRevision()
	e.noteHeld(key, rev)
	return rev, true
}

// CurrentAt is the display rule every screen applies to a held copy: it is
// current as of the later of its arrival and Core's last confirmation (a copy
// that has just arrived is as fresh as a confirmed one), and old when that is
// longer ago than FeedAsOfAfter. at is zero when neither time is set, and then
// old is true; each caller says what "nothing held" means on its screen.
func CurrentAt(received, confirmed, now time.Time) (at time.Time, old bool) {
	at = confirmed
	if received.After(at) {
		at = received
	}
	return at, now.Sub(at) > FeedAsOfAfter
}

// crossingsLocked compares each aged feed's age now with what it was when last
// looked at, and reports a crossing of FeedAsOfAfter either way: for a feed a
// station shows (board) and for containment, whose page reloads on its own
// event. The age is CurrentAt's; nothing held is never stale (the screens say
// "not known" for that, from other facts). Memory only, three compares:
// called on every heartbeat tick (FeedDigests), confirmation (OnCoreAck) and
// arrival (holdFeed), because the board does not poll and the crossing writes
// nothing anyone would announce.
func (b *feedBook) crossingsLocked(now time.Time) (board, containment bool) {
	if b.stale == nil {
		b.stale = map[string]bool{}
	}
	for _, f := range feedRows {
		if f.aged == notAged {
			continue
		}
		stale := false
		if h := b.held[f.key]; h != nil && !(h.receivedAt.IsZero() && h.confirmedAt.IsZero()) {
			_, stale = CurrentAt(h.receivedAt, h.confirmedAt, now)
		}
		if b.stale[f.key] == stale {
			continue
		}
		b.stale[f.key] = stale
		if f.aged == agedOnContainment {
			containment = true
		} else {
			board = true
		}
	}
	return board, containment
}

// announceCrossings says what crossingsLocked found, outside the lock: one
// board nudge for the station feeds, and the containment event (whose page
// reloads on it; every station takes it too).
func (e *Engine) announceCrossings(board, containment bool) {
	if board {
		e.nudgeBoards()
	}
	if containment {
		e.Events.Emit(Event{Type: EventContainmentUpdated, Timestamp: time.Now(),
			Payload: ContainmentUpdatedEvent{Digest: e.heldDigest(protocol.FeedContainment)}})
	}
}

// ageFeeds is the heartbeat tick's look at the aged feeds.
func (e *Engine) ageFeeds() {
	e.feeds.mu.Lock()
	board, containment := e.feeds.crossingsLocked(e.feeds.clock())
	e.feeds.mu.Unlock()
	e.announceCrossings(board, containment)
}

// clock is this Edge's feed clock: the time an ack is stamped with, the held
// copies are aged against, and the resend guard reads.
func (b *feedBook) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// sendDue reports whether an Edge-owned key whose digest Core does not hold
// should be sent now, and records the send, under the resend guard Core applies
// to its feeds (protocol/resend) and on the book's clock, as Core's compare
// reads its own. The same digest inside the guard is in flight and is held
// back, unless Core held it and now quotes otherwise (a restarted Core): then
// it goes at once. A third send in a row raises a flag. quoted is the digest
// Core holds for the key: when it is any copy sent since the last one Core
// proved (resend.Record.Quoted, Core's rule) the streak starts over and the
// flag clears, because Core's answer is as old as its ack and a key whose
// value moves on every heartbeat is not stuck. A Core frozen at one digest is
// believed once. Pinned by TestSendDue_AQuoteOfAnOlderSendConverges and
// TestSendDue_AFrozenQuoteStillFlags.
func (e *Engine) sendDue(key, digest, quoted string) bool {
	now := e.feeds.clock()
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	rec := e.feeds.sentLocked(key)
	if rec.Quoted(quoted) {
		delete(e.feeds.flags, key)
	}
	if !rec.Send(digest, quoted, now) {
		return false
	}
	if rec.NotConverging() {
		if e.feeds.flags == nil {
			e.feeds.flags = map[string]string{}
		}
		if _, flagged := e.feeds.flags[key]; !flagged {
			e.logFn("feeds: %s sent to Core %d times in a row without converging", key, rec.Streak())
		}
		e.feeds.flags[key] = fmt.Sprintf("%s not converging: sent %d times", key, rec.Streak())
	}
	return true
}

// recordPush records a send of an Edge-owned key made outside sendDue (the
// claims publisher's publish on a save), as Core records its own: a quote of
// it converges, and the resend guard and the streak are left alone.
func (e *Engine) recordPush(key, digest string) {
	now := e.feeds.clock()
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	e.feeds.sentLocked(key).Pushed(digest, now)
}

// converged records that Core quoted an Edge-owned key back with the digest
// this Edge holds: the streak ends and any flag on it clears.
func (e *Engine) converged(key string) {
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	if rec := e.feeds.sent[key]; rec != nil {
		rec.Converged()
	}
	delete(e.feeds.flags, key)
}

// heldByCore records that Core holds digest for an Edge-owned key, learned
// from something other than an ack's quote: the bins snapshot names the watch
// list it answers. It is a quote like an ack's: any copy sent since the last
// one proved ends the streak, and when it is the last send, a Core that
// restarts before its next ack is healed on its first one (resend.Record: a
// held send quoted otherwise was lost). Pinned by
// TestHeldByCore_AnOlderSendEndsTheStreak and
// TestSendDue_ARestartedCoreIsHealedAtOnce.
func (e *Engine) heldByCore(key, digest string) {
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	if rec := e.feeds.sent[key]; rec != nil && rec.Quoted(digest) {
		delete(e.feeds.flags, key)
	}
}

// FeedFlags is the Edge's flag list, for /status and Diagnostics: the
// Edge-owned keys flagged as not converging, then the start's schema drift
// (store.DriftAtBoot), which stands until a start finds none.
func (e *Engine) FeedFlags() []string {
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	out := make([]string, 0, len(e.feeds.flags))
	for _, text := range e.feeds.flags {
		out = append(out, text)
	}
	sort.Strings(out)
	return append(out, e.db.DriftAtBoot()...)
}

// LastCoreAck returns the local time of the last heartbeat ack and Core's
// server stamp on it; zero times before the first ack.
func (e *Engine) LastCoreAck() (local, server time.Time) {
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	return e.feeds.lastAck, e.feeds.lastServerTS
}

// SetCoreAckObserver installs fn, called after every heartbeat ack is taken
// (OnCoreAck): after LastCoreAck has moved, outside the book's lock, on the
// ack's goroutine, so it must not block. It is how a reader of the Core link
// learns the ack arrived without polling LastCoreAck; the www robot stream
// says {available} from it. nil removes it. Pinned by
// TestOnCoreAck_TellsTheObserverAfterTheAckMoved.
func (e *Engine) SetCoreAckObserver(fn func()) {
	e.feeds.mu.Lock()
	e.feeds.ackObserver = fn
	e.feeds.mu.Unlock()
}

// persistedRow is the feed_copy row for a hold, with the digest and body left
// out where they do not persist.
func persistedRow(key string, h *feedHold) store.FeedCopy {
	row := store.FeedCopy{Feed: key, ReceivedAt: h.receivedAt, ConfirmedAt: h.confirmedAt}
	if feedRowOf(key).persistDigest {
		row.Digest, row.Body = h.digest, h.body
	}
	return row
}

// sentLocked is the send record of one Edge-owned key, made when there is none.
func (b *feedBook) sentLocked(key string) *resend.Record {
	if b.sent == nil {
		b.sent = map[string]*resend.Record{}
	}
	rec := b.sent[key]
	if rec == nil {
		rec = &resend.Record{}
		b.sent[key] = rec
	}
	return rec
}

func (b *feedBook) holdLocked(key string) *feedHold {
	if b.held == nil {
		b.held = map[string]*feedHold{}
	}
	h := b.held[key]
	if h == nil {
		h = &feedHold{}
		b.held[key] = h
	}
	return h
}

// loadFeedCopies reads feed_copy into the book at boot, so the digests and
// times survive a restart. A failed read starts empty: every feed then reads
// as never received and Core sends it on the first heartbeat.
func (e *Engine) loadFeedCopies() {
	rows, err := e.db.ListFeedCopies()
	if err != nil {
		e.logFn("feeds: load held copies: %v — starting with none", err)
		return
	}
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	for _, r := range rows {
		h := e.feeds.holdLocked(r.Feed)
		if feedRowOf(r.Feed).persistDigest {
			h.digest, h.body = r.Digest, r.Body
		}
		h.receivedAt, h.confirmedAt, h.persistedAt = r.ReceivedAt, r.ConfirmedAt, r.ConfirmedAt
	}
}

// FeedDigests is the heartbeat's Feeds map: per Core feed, the digest this
// Edge holds ("" when it holds nothing yet). Never nil, so Core can tell this
// Edge from one that predates feeds.
func (e *Engine) FeedDigests() map[string]string {
	// The heartbeat's tick is the Edge's clock for the screens' "as of".
	e.ageFeeds()
	out := map[string]string{}
	for _, f := range feedRows {
		if d, ok := f.quote(e, f.key); ok {
			out[f.key] = d
		}
	}
	return out
}

// noteHeld records the digest a heartbeat is about to quote for a feed whose
// digest is computed rather than received (the scene revision, the refusal
// table), so the ack that answers it can confirm it.
func (e *Engine) noteHeld(key, digest string) {
	e.feeds.mu.Lock()
	e.feeds.holdLocked(key).digest = digest
	e.feeds.mu.Unlock()
}

// heldDigest is the digest this Edge holds for a feed, "" when none.
func (e *Engine) heldDigest(key string) string {
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	if h := e.feeds.held[key]; h != nil {
		return h.digest
	}
	return ""
}

// holdFeed records a feed Core sent, AFTER its apply returned nil. body is the
// encoded copy for a feed whose data lives only here (containment), else "".
// The row is written when the digest changed, for every feed that keeps one
// (feedRow.keepRow); an identical re-send only moves the received time in memory.
func (e *Engine) holdFeed(key, digest, body string) {
	now := time.Now()
	e.feeds.mu.Lock()
	h := e.feeds.holdLocked(key)
	changed := h.digest != digest || h.receivedAt.IsZero()
	h.digest, h.body, h.receivedAt = digest, body, now
	var row store.FeedCopy
	if changed {
		h.persistedAt = now
		row = persistedRow(key, h)
	}
	board, containment := e.feeds.crossingsLocked(e.feeds.clock())
	e.feeds.mu.Unlock()
	e.announceCrossings(board, containment)
	if changed && feedRowOf(key).keepRow {
		if err := e.db.SaveFeedCopy(row); err != nil {
			e.logFn("feeds: save %s: %v", key, err)
		}
	}
}

// FeedTimes returns when a feed's current copy arrived and when Core last
// confirmed it; zero times when it was never received or never confirmed.
func (e *Engine) FeedTimes(key string) (receivedAt, confirmedAt time.Time) {
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	if h := e.feeds.held[key]; h != nil {
		return h.receivedAt, h.confirmedAt
	}
	return time.Time{}, time.Time{}
}

// CoreSpeaksFeeds reports whether the last ack came from a Core that answers
// feed digests. False before the first ack, so an Edge that has not yet heard
// from Core keeps its own re-asks running.
func (e *Engine) CoreSpeaksFeeds() bool {
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	return e.feeds.coreSpeaksFeeds
}

// OnCoreAck takes Core's answer to a heartbeat: whether Core speaks feeds,
// for each feed whose digest equals the one held, the confirmation, Core's
// answer for each family this Edge owns (answerOwned: its plant claims, its
// bins watch list, its cell catalog), the ask kinds, the ephemeral topic Core
// names and the station's name; then it tells the ack observer
// (SetCoreAckObserver).
func (e *Engine) OnCoreAck(ack *protocol.EdgeHeartbeatAck) {
	if ack == nil {
		return
	}
	now := e.feeds.clock()
	var persist []store.FeedCopy
	e.feeds.mu.Lock()
	// Monotonic: a wall-clock step on the Pi must not fake or hide a gap. No
	// previous ack since boot is not a gap; boot runs its own reconcile.
	gap := !e.feeds.lastAck.IsZero() && now.Sub(e.feeds.lastAck) > feedAckGap
	reconcile := e.feeds.reconcile
	e.feeds.coreSpeaksFeeds = ack.Feeds != nil
	e.feeds.lastAck, e.feeds.lastServerTS = now, ack.ServerTS
	for key, d := range ack.Feeds {
		h := e.feeds.held[key]
		if h == nil || h.digest != d {
			continue
		}
		h.confirmedAt = now
		if feedRowOf(key).keepRow && now.Sub(h.persistedAt) >= feedConfirmPersistEvery {
			h.persistedAt = now
			persist = append(persist, persistedRow(key, h))
		}
	}
	board, containment := e.feeds.crossingsLocked(now)
	observer := e.feeds.ackObserver
	e.feeds.mu.Unlock()
	e.announceCrossings(board, containment)
	for _, row := range persist {
		if err := e.db.SaveFeedCopy(row); err != nil {
			e.logFn("feeds: save %s confirmation: %v", row.Feed, err)
		}
	}
	e.answerOwned(ack)
	e.setAskKinds(ack.Asks)
	// Core's verified ephemeral topic, or none (an older Core, or one that
	// could not verify it), which closes the live robot feed (robot_watch.go).
	e.OfferEphemeralTopic(ack.EphemeralTopic)
	// The station's name for people, held in memory (takeStationName).
	e.takeStationName(ack.DisplayName)
	if observer != nil {
		observer()
	}
	if gap {
		e.reconcileAfterGap(reconcile)
	}
}

// reconcileAfterGap runs the order reconcile on the first ack after a silence
// longer than feedAckGap: whatever Core pushed meanwhile may have expired on
// the wire, and the reconcile asks Core for every order this Edge should hold.
// StartupReconcile is that reconcile and nothing more: it asks for no node
// list or catalog, which a Core that answers feeds sends when the heartbeat
// quotes a digest it does not hold, and the heartbeater re-asks of an older one.
func (e *Engine) reconcileAfterGap(reconcile func() error) {
	if reconcile == nil {
		reconcile = e.StartupReconcile
	}
	e.logFn("feeds: first ack after more than %v without one — reconciling orders", feedAckGap)
	if err := reconcile(); err != nil {
		e.logFn("feeds: reconcile after an ack gap: %v", err)
	}
}

// ActableAckWithin is how recent the last heartbeat ack must be for a held copy
// to be decided from: one heartbeat (messaging.Heartbeater's 60 s) plus slack.
const ActableAckWithin = 75 * time.Second

// Why a held copy is not actable (Actable's reason). Each is a state the
// copy's reader can name; none of them is a guess at what Core holds.
const (
	NotActableNothingHeld  = "nothing held"           // no snapshot since this Edge started
	NotActableOtherList    = "built for another list" // the snapshot answers a watch list this Edge no longer has
	NotActableNoAck        = "no ack from Core"       // no heartbeat ack since this Edge started
	NotActableAckOld       = "Core has not answered"  // the last ack is older than ActableAckWithin
	NotActableNotConfirmed = "not confirmed by Core"  // the last ack quoted a different digest
)

// Actable is the decision rule, beside CurrentAt's display rule: a held copy
// may be decided from only when all of these hold —
//   - a snapshot is held;
//   - it answers the list this Edge watches now (its list digest is ours);
//   - the last heartbeat ack confirmed this copy's digest, or arrived before
//     the copy did (the copy is newer than anything Core said about it);
//   - that ack is younger than ActableAckWithin.
//
// One positive test, so a Core restart (the ack stops confirming), an Edge
// restart (nothing held) and Core unreachable (the ack ages out) all read not
// actable, and no absence reads as an answer. confirmed is the copy's last
// confirmation and lastAck the last ack's time, both on the feed book's clock:
// the copy was confirmed by the last ack exactly when the two are equal.
func Actable(held bool, copyList, ownList string, received, confirmed, lastAck, now time.Time) (ok bool, why string) {
	switch {
	case !held:
		return false, NotActableNothingHeld
	case copyList != ownList:
		return false, NotActableOtherList
	case lastAck.IsZero():
		return false, NotActableNoAck
	case now.Sub(lastAck) > ActableAckWithin:
		return false, NotActableAckOld
	case !confirmed.Equal(lastAck) && !received.After(lastAck):
		return false, NotActableNotConfirmed
	}
	return true, ""
}
