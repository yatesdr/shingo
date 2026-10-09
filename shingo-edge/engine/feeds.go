package engine

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"shingo/protocol"
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
	mu   sync.Mutex
	held map[string]*feedHold
	// coreSpeaksFeeds is true once an ack carried a Feeds map. Until then,
	// and whenever an older Core answers, the Edge keeps its own timers.
	coreSpeaksFeeds bool
	// lastAck is the local (monotonic) time of the last ack; lastServerTS is
	// Core's stamp on it.
	lastAck      time.Time
	lastServerTS time.Time
	// sent and flags are the Edge's side of the resend guard, for the data
	// this Edge owns and sends to Core (its plant claims): the same rule
	// Core applies to its feeds.
	sent  map[string]*feedSendRecord
	flags map[string]string
	// reconcile is what an ack after a gap runs; nil means StartupReconcile.
	// A field so a test can count it.
	reconcile func() error
	// now is this Edge's clock for stamping acks; nil means time.Now. A field
	// so a test can run the Edge's clock ahead of or behind Core's — the rig
	// cannot shift one container's wall clock.
	now func() time.Time
}

// clock is the time an ack is stamped with on this Edge.
func (b *feedBook) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

const (
	// feedResendGuard skips a send of the same digest of the same key this
	// recently; feedFlagAfter sends in a row without Core quoting it back
	// flag the key. Core applies the same two numbers to its feeds.
	feedResendGuard = 2 * time.Minute
	feedFlagAfter   = 3
)

// feedSendRecord is the last send of one Edge-owned key to Core.
type feedSendRecord struct {
	digest string
	at     time.Time
	streak int
}

// sendDue reports whether an Edge-owned key whose digest Core does not hold
// should be sent now, and records the send. The same digest inside the guard
// is in flight and is held back; a third send in a row raises a flag. quoted is
// the digest Core holds for the key: when it is the one sent last, that send
// converged and the streak starts over (Core's rule, messaging/feeds.go compare,
// mirrored): a key whose value moves on every heartbeat is not stuck.
func (e *Engine) sendDue(key, digest, quoted string) bool {
	now := time.Now()
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	if e.feeds.sent == nil {
		e.feeds.sent = map[string]*feedSendRecord{}
	}
	rec := e.feeds.sent[key]
	if rec != nil && quoted == rec.digest {
		rec.streak = 0
		delete(e.feeds.flags, key)
	}
	if rec != nil && rec.digest == digest && now.Sub(rec.at) < feedResendGuard {
		return false
	}
	if rec == nil {
		rec = &feedSendRecord{}
		e.feeds.sent[key] = rec
	}
	rec.digest, rec.at = digest, now
	rec.streak++
	if rec.streak >= feedFlagAfter {
		if e.feeds.flags == nil {
			e.feeds.flags = map[string]string{}
		}
		if _, flagged := e.feeds.flags[key]; !flagged {
			e.logFn("feeds: %s sent to Core %d times in a row without converging", key, rec.streak)
		}
		e.feeds.flags[key] = fmt.Sprintf("%s not converging: sent %d times", key, rec.streak)
	}
	return true
}

// converged records that Core quoted an Edge-owned key back with the digest
// this Edge holds: the streak ends and any flag on it clears.
func (e *Engine) converged(key string) {
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	if rec := e.feeds.sent[key]; rec != nil {
		rec.streak = 0
	}
	delete(e.feeds.flags, key)
}

// FeedFlags lists the Edge-owned keys flagged as not converging, for /status.
func (e *Engine) FeedFlags() []string {
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	out := make([]string, 0, len(e.feeds.flags))
	for _, text := range e.feeds.flags {
		out = append(out, text)
	}
	sort.Strings(out)
	return out
}

// LastCoreAck returns the local time of the last heartbeat ack and Core's
// server stamp on it; zero times before the first ack.
func (e *Engine) LastCoreAck() (local, server time.Time) {
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	return e.feeds.lastAck, e.feeds.lastServerTS
}

// digestPersists reports whether a feed's digest may be written to feed_copy:
// only where the data it names is kept across a restart too. The node list is
// memory-only, so a persisted nodes digest would quote a copy the Edge no
// longer has and Core would never resend it; the scene revision and the
// refusal digest are recomputed from their own stores.
func digestPersists(key string) bool {
	return key == protocol.FeedContainment || key == protocol.FeedCatalog
}

// persistedRow is the feed_copy row for a hold, with the digest and body left
// out where they do not persist.
func persistedRow(key string, h *feedHold) store.FeedCopy {
	row := store.FeedCopy{Feed: key, ReceivedAt: h.receivedAt, ConfirmedAt: h.confirmedAt}
	if digestPersists(key) {
		row.Digest, row.Body = h.digest, h.body
	}
	return row
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
		if digestPersists(r.Feed) {
			h.digest, h.body = r.Digest, r.Body
		}
		h.receivedAt, h.confirmedAt, h.persistedAt = r.ReceivedAt, r.ConfirmedAt, r.ConfirmedAt
	}
}

// FeedDigests is the heartbeat's Feeds map: per Core feed, the digest this
// Edge holds ("" when it holds nothing yet). Never nil, so Core can tell this
// Edge from one that predates feeds.
func (e *Engine) FeedDigests() map[string]string {
	out := map[string]string{}
	out[protocol.FeedContainment] = e.heldDigest(protocol.FeedContainment)
	if d, ok := e.refusalsDigest(); ok {
		out[protocol.FeedRefusals] = d
	}
	out[protocol.FeedNodes] = e.heldDigest(protocol.FeedNodes)
	// The scene revision lives with the geometry; note it so the ack can
	// confirm it.
	rev := e.SceneRevision()
	e.noteHeld(protocol.FeedScene, rev)
	out[protocol.FeedScene] = rev
	out[protocol.FeedCatalog] = e.heldDigest(protocol.FeedCatalog)
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
// The row is written when the digest changed; an identical re-send only moves
// the received time in memory.
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
	e.feeds.mu.Unlock()
	if changed {
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
// for each feed whose digest equals the one held, the confirmation, and the
// plant-claims digests Core holds for this station (feeds_claims.go).
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
		if now.Sub(h.persistedAt) >= feedConfirmPersistEvery {
			h.persistedAt = now
			persist = append(persist, persistedRow(key, h))
		}
	}
	e.feeds.mu.Unlock()
	for _, row := range persist {
		if err := e.db.SaveFeedCopy(row); err != nil {
			e.logFn("feeds: save %s confirmation: %v", row.Feed, err)
		}
	}
	e.answerClaims(ack.Claims)
	if gap {
		e.reconcileAfterGap(reconcile)
	}
}

// reconcileAfterGap runs the order reconcile on the first ack after a silence
// longer than feedAckGap: whatever Core pushed meanwhile may have expired on
// the wire, and the reconcile asks Core for every order this Edge should hold.
func (e *Engine) reconcileAfterGap(reconcile func() error) {
	if reconcile == nil {
		reconcile = e.StartupReconcile
	}
	e.logFn("feeds: first ack after more than %v without one — reconciling orders", feedAckGap)
	if err := reconcile(); err != nil {
		e.logFn("feeds: reconcile after an ack gap: %v", err)
	}
}
