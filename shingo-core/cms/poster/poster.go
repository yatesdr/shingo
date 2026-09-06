// Package poster drains cms_postings to the middleware.
//
// Drainer-shaped, after messaging/outbox: a doorbell for latency, a ticker as
// the backstop, a mute to protect the retry budget, and a panic boundary per
// posting. It borrows that SHAPE and none of its code — the Kafka outbox drops
// a message after ten attempts, which docs/outbox-ordering.md calls "not a
// delay, a hole". An inventory ledger cannot have a hole, so nothing here
// discards a posting: a row that runs out of attempts becomes `failed` and
// stays for a person.
//
// AMR DISPATCH NEVER WAITS ON THIS. If the middleware is unreachable for a
// shift, bins keep moving and rows accumulate; the backlog is a number on the
// diagnostics page, not a brake on the plant.
//
// WHAT THE RECONCILER RESOLVES, AND WHAT IT DOES NOT.
//
// The reconciler is the only path out of `inflight`, and it works by asking the
// middleware whether it holds a transaction id. That bounds it to rows that
// HAVE an id, and in v1 exactly one after-send failure produces one: a POST
// that returned 2xx and named an id, whose settling write then failed.
// applyResult writes the id before the status precisely so that failure lands
// resolvable instead of not.
//
// Every other route into inflight leaves no id — a 2xx whose body we could not
// parse an id out of, a 5xx, a timeout mid-response, a crash between the send
// and the acknowledgement. Those rows are NOT auto-resolved, and that is the
// design rather than a gap: with no idempotency key on this API, re-sending may
// double-book and marking them posted would invent a success. They age, count
// as UnresolvableInflight on the health surface, and a person checks the
// middleware and settles them by hand.
//
// The line moves when the middleware gains a dedup key. The client already
// sends x-body-sha256; if Juan's side ever honours it, a re-send becomes safe
// and the whole class resolves without anyone being asked to look.
package poster

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"runtime/debug"
	"sync"
	"time"

	"shingocore/cms/client"
	"shingocore/cms/wire"
	"shingocore/store/cms"
)

// Store is the narrow persistence surface the poster needs. Declared
// consumer-side so *store.DB satisfies it for free and a test can hand in a
// fake without a database.
type Store interface {
	CreateCMSPosting(p *cms.Posting) error
	AttachCMSPosting(txnIDs []int64, postingID int64) (int, error)
	NextPendingCMSPostings(limit int) ([]*cms.Posting, error)
	// ListUnpostedCMSTransactions is the sweep's input: rows the subscriber
	// recorded and no posting ever claimed. See SweepOrphansOnce.
	ListUnpostedCMSTransactions(age time.Duration, limit int) ([]*cms.Transaction, error)
	ListCMSTransactionsByPosting(postingID int64) ([]*cms.Transaction, error)
	MarkCMSPostingInflight(id int64, bodySHA string) error
	// MarkCMSPostingTransactionID writes the id without settling the row, so a
	// failure of the settling write leaves something the reconciler can ask
	// about. See applyResult's ClassPosted branch.
	MarkCMSPostingTransactionID(id int64, transactionID string) error
	MarkCMSPostingPosted(id int64, transactionID string, httpStatus int) error
	MarkCMSPostingRejected(id int64, httpStatus int, lastErr string) error
	MarkCMSPostingFailed(id int64, lastErr string) error
	MarkCMSPostingPending(id int64, lastErr string, nextRetry time.Time) error
	// RecordCMSPostingAfterSendError does NOT touch attempts — MarkInflight
	// counts those, once per send — and does not schedule anything: an
	// inflight row's only exit is the reconciler.
	RecordCMSPostingAfterSendError(id int64, lastErr string) error
	ListInflightCMSPostingsOlderThan(d time.Duration) ([]*cms.Posting, error)
	RequeueCMSPosting(id int64) error
}

// Transport is the middleware client. An interface so the poster's tests do not
// need a socket and so a fault can be produced on demand.
type Transport interface {
	Post(ctx context.Context, body []byte, bodySHA string) client.PostResult
	GetByTxID(ctx context.Context, transactionID string) (bool, error)
}

// Config is the poster's half of the cms: block.
type Config struct {
	PollInterval time.Duration
	MaxAttempts  int
	SettleWindow time.Duration
	// MaxRequeues bounds the reconciler's own loop. See applyReconcileMiss.
	MaxRequeues int
	Wire        wire.Config
}

// Poster drains pending postings and reconciles inflight ones.
type Poster struct {
	store     Store
	transport Transport
	cfg       Config
	logf      func(string, ...any)

	wake chan struct{}

	mu sync.Mutex
	// muted stops the drain until a configuration change or a restart. Set on
	// a credential fault ONLY: with a bad key every queued posting fails
	// identically, and letting them all burn their attempts turns a five-minute
	// config fix into a table of `failed` rows somebody has to reason about.
	muted    bool
	mutedWhy string
}

// The shipped defaults, and the ONLY place these numbers live.
//
// config's CMSDefaults and Validate read them from here. They used to be
// spelled once in each — plus a fifth copy inside nextRetry — which is four
// places to retune a value and three to forget, with nothing to say the copies
// had drifted apart.
const (
	DefaultPollInterval = 30 * time.Second
	DefaultMaxAttempts  = 12
	DefaultSettleWindow = 5 * time.Minute
	DefaultMaxRequeues  = 3
)

// New builds a poster. It does not start anything.
func New(store Store, transport Transport, cfg Config, logf func(string, ...any)) *Poster {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.SettleWindow <= 0 {
		cfg.SettleWindow = DefaultSettleWindow
	}
	if cfg.MaxRequeues <= 0 {
		cfg.MaxRequeues = DefaultMaxRequeues
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Poster{
		store: store, transport: transport, cfg: cfg, logf: logf,
		// Buffered by one: a wake that arrives mid-drain must not block the
		// emitter (which is an engine event subscriber, running on the caller's
		// goroutine), and a second wake during one drain is the same request as
		// the first.
		wake: make(chan struct{}, 1),
	}
}

// Enqueue records a batch of transactions as one pending posting.
//
// Called from the engine's EventCMSTransaction subscriber, on the emitting
// goroutine. It writes and rings the doorbell; it does not send, because a bin
// arrival must not wait on an HTTP round trip.
func (p *Poster) Enqueue(txns []*cms.Transaction) error {
	ids := make([]int64, 0, len(txns))
	for _, t := range txns {
		if t != nil && t.ID != 0 {
			ids = append(ids, t.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}

	posting := &cms.Posting{}
	if err := p.store.CreateCMSPosting(posting); err != nil {
		return fmt.Errorf("create cms posting: %w", err)
	}
	attached, err := p.store.AttachCMSPosting(ids, posting.ID)
	if err != nil {
		return fmt.Errorf("attach %d transactions to posting %d: %w", len(ids), posting.ID, err)
	}
	if attached != len(ids) {
		// Some rows were already claimed. The posting still carries what it
		// took, and the ones it did not take belong to someone else — but say
		// so, because the only way this happens is two paths racing over the
		// same transactions, which is worth knowing about.
		p.logf("cms poster: posting %d claimed %d of %d transactions — the rest were already attached",
			posting.ID, attached, len(ids))
	}
	if attached == 0 {
		// Nothing to send. Fail it rather than leave a pending row that will
		// build an empty body forever.
		if err := p.store.MarkCMSPostingFailed(posting.ID, "no transactions attached"); err != nil {
			p.logf("cms poster: fail empty posting %d: %v", posting.ID, err)
		}
		return nil
	}
	p.Ring()
	return nil
}

// Ring wakes the drain without blocking. A wake already waiting is the same
// request, so a full channel is a no-op rather than a stall.
func (p *Poster) Ring() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Run drains until ctx is done. Blocking; start it in a goroutine.
//
// The ticker is a BACKSTOP, not the mechanism. Everything that should be sent
// arrives via Ring; the ticker exists because a doorbell that is missed once
// would otherwise strand a posting until the next arrival, and because retries
// become due on a clock nobody rings.
func (p *Poster) Run(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()

	reconcileEvery := p.cfg.SettleWindow
	if triple := p.cfg.PollInterval * 3; triple > reconcileEvery {
		reconcileEvery = triple
	}
	reconcile := time.NewTicker(reconcileEvery)
	defer reconcile.Stop()

	// A startup sweep before the first tick: a crash left rows behind, and
	// waiting a full settle window to look at them is a delay with no purpose.
	p.SweepOrphansOnce(ctx)
	p.DrainOnce(ctx)
	p.ReconcileOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
			p.DrainOnce(ctx)
		case <-ticker.C:
			// The sweep rides the drain ticker rather than a third one. It is
			// a backstop for a failure that should be rare, and it reads one
			// indexed partial-index query when there is nothing to find.
			p.SweepOrphansOnce(ctx)
			p.DrainOnce(ctx)
		case <-reconcile.C:
			p.ReconcileOnce(ctx)
		}
	}
}

// SweepOrphansOnce re-enqueues transactions that were recorded and never queued.
//
// THE RECONCILIATION HALF OF THE DOORBELL. Enqueue is called from the event
// subscriber, and if it fails — CreateCMSPosting errors, AttachCMSPosting
// errors, the process dies between the two — the transaction rows sit with
// posting_id NULL forever. Nothing retries, because the only thing that would
// have was the notification that already failed, and you cannot wire up an
// absence. AGENTS.md: pair every notification path with a sweep that checks the
// condition directly, and the notification becomes a latency optimisation
// rather than the guarantee.
//
// Until this existed, the only recovery was a person reading the health card's
// unposted count and doing something by hand — and that count is precisely the
// number this makes go down on its own.
func (p *Poster) SweepOrphansOnce(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			p.logf("cms poster: PANIC in orphan sweep: %v\n%s", r, debug.Stack())
		}
	}()
	if p.Muted() {
		return
	}

	// The grace period is one poll interval: long enough that a transaction
	// whose Enqueue is simply still in flight is not swept, short enough that a
	// genuinely orphaned row is picked up on the next tick rather than a shift
	// later.
	orphans, err := p.store.ListUnpostedCMSTransactions(p.cfg.PollInterval, 500)
	if err != nil {
		p.logf("cms poster: read unposted transactions: %v", err)
		return
	}
	if len(orphans) == 0 {
		return
	}
	if ctx.Err() != nil {
		return
	}
	// LOUD, because reaching here means the enqueue path failed and nothing
	// else says so. The sweep repairing it silently would turn a broken
	// subscriber into a permanent 30-second delay nobody ever investigates.
	movements := groupByMovement(orphans)
	p.logf("cms poster: %d cms_transactions across %d movement(s) were recorded and never "+
		"queued for posting — re-enqueueing them. Something went wrong in the event "+
		"subscriber; this sweep is the backstop, not the mechanism.",
		len(orphans), len(movements))
	// ONE POSTING PER MOVEMENT, which is what the doorbell path already does —
	// the subscriber calls Enqueue with one movement's rows. This used to hand
	// the whole 500-row batch to a single Enqueue, so the recovery path, and
	// only the recovery path, built bodies carrying many bins' movements under
	// one ticket. Whether CMS tolerates that is an open question with IT, and a
	// backstop that behaves differently from the mechanism it backs up is worth
	// avoiding whatever the answer turns out to be.
	for _, m := range movements {
		if ctx.Err() != nil {
			return
		}
		if err := p.Enqueue(m); err != nil {
			p.logf("cms poster: re-enqueue %d orphaned transactions: %v", len(m), err)
		}
	}
}

// groupByMovement splits transactions into the movements that produced them,
// preserving the order the rows arrived in.
//
// THE KEY IS (bin, order, created_at) and the last part is what makes it exact.
// One movement's rows are inserted by one Create call inside one transaction,
// and Postgres NOW() is transaction time — so every row of a movement carries
// the identical timestamp, and two movements of the same bin on the same order
// (a there-and-back) carry different ones. Grouping on bin and order alone
// would merge those two into one body.
func groupByMovement(txns []*cms.Transaction) [][]*cms.Transaction {
	type key struct {
		bin, order int64
		at         int64
	}
	var order []key
	byKey := map[key][]*cms.Transaction{}
	for _, t := range txns {
		if t == nil {
			continue
		}
		k := key{at: t.CreatedAt.UnixNano()}
		if t.BinID != nil {
			k.bin = *t.BinID
		}
		if t.OrderID != nil {
			k.order = *t.OrderID
		}
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], t)
	}
	out := make([][]*cms.Transaction, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out
}

// DrainOnce sends every pending posting that is due.
func (p *Poster) DrainOnce(ctx context.Context) {
	if p.Muted() {
		return
	}
	pending, err := p.store.NextPendingCMSPostings(50)
	if err != nil {
		p.logf("cms poster: read pending postings: %v", err)
		return
	}
	for _, posting := range pending {
		if ctx.Err() != nil {
			return
		}
		if p.Muted() {
			return
		}
		p.sendOne(ctx, posting)
	}
}

// sendOne carries one posting through its whole attempt, with a panic boundary.
//
// The boundary is per POSTING rather than per drain: one malformed response
// body must not take the loop down and strand every other row behind it.
func (p *Poster) sendOne(ctx context.Context, posting *cms.Posting) {
	defer func() {
		if r := recover(); r != nil {
			p.logf("cms poster: PANIC sending posting %d: %v\n%s", posting.ID, r, debug.Stack())
		}
	}()

	txns, err := p.store.ListCMSTransactionsByPosting(posting.ID)
	if err != nil {
		p.logf("cms poster: load transactions for posting %d: %v", posting.ID, err)
		return
	}
	rows := wire.Build(txns, p.cfg.Wire)
	if len(rows) == 0 {
		// Nothing sendable. Failing beats leaving a pending row that rebuilds
		// an empty body on every tick forever.
		p.fail(posting, "posting carries no sendable transactions")
		return
	}
	body, err := json.Marshal(rows)
	if err != nil {
		p.fail(posting, fmt.Sprintf("marshal body: %v", err))
		return
	}
	sha := client.BodySHA(body)

	// COMMITTED BEFORE THE SEND. A crash from here on leaves a row that says
	// "we tried, we do not know" — which the reconciler can resolve. A row
	// still saying "pending" while a POST was on the wire would be eligible for
	// re-send with nothing recording that a copy had gone.
	if err := p.store.MarkCMSPostingInflight(posting.ID, sha); err != nil {
		// Another pass took it, or the row moved. Not an error worth shouting
		// about; the row is somebody else's now.
		p.logf("cms poster: posting %d not claimable: %v", posting.ID, err)
		return
	}

	res := p.transport.Post(ctx, body, sha)
	p.applyResult(posting, res)
}

// applyResult turns one attempt into durable state.
//
// Every branch answers the same question — could the middleware already have
// this? — and the answer decides the row's status, not the severity of the
// failure.
func (p *Poster) applyResult(posting *cms.Posting, res client.PostResult) {
	errText := ""
	if res.Err != nil {
		errText = res.Err.Error()
	}
	// attempts was incremented by MarkInflight, so the row's own count is one
	// behind what has now been tried.
	attempts := posting.Attempts + 1

	switch res.Class {
	case client.ClassPosted:
		// THE ID FIRST, THEN THE STATUS, AND THE ORDER IS THE WHOLE POINT.
		// MarkPosted carries both, so a failure of that one statement used to
		// lose the id and leave an inflight row with nothing to ask about —
		// the one inflight state that has no safe automatic resolution.
		// Written separately and in this order, the same failure leaves a row
		// the reconciler can settle by asking the middleware, and it is the
		// only after-send failure in v1 that it can.
		if res.TransactionID != "" {
			if err := p.store.MarkCMSPostingTransactionID(posting.ID, res.TransactionID); err != nil {
				// Not fatal to the attempt. On the path where the next call
				// succeeds this one changed nothing; it matters only when it
				// does not.
				p.logf("cms poster: record transaction id for posting %d: %v", posting.ID, err)
			}
		}
		if err := p.store.MarkCMSPostingPosted(posting.ID, res.TransactionID, res.HTTPStatus); err != nil {
			p.logf("cms poster: mark posting %d posted: %v — the row stays inflight and the "+
				"reconciler resolves it if the transaction id was recorded", posting.ID, err)
			return
		}

	case client.ClassInflight:
		// Accepted, unnamed. Leave it inflight for the reconciler and do NOT
		// schedule a retry: re-sending is the one thing that cannot be done
		// safely from here.
		p.logf("cms poster: posting %d accepted without a transaction id — leaving inflight for the reconciler: %s",
			posting.ID, errText)

	case client.ClassRejected:
		if err := p.store.MarkCMSPostingRejected(posting.ID, res.HTTPStatus, errText); err != nil {
			p.logf("cms poster: mark posting %d rejected: %v", posting.ID, err)
		}
		p.logf("cms poster: posting %d REJECTED (%d): %s", posting.ID, res.HTTPStatus, errText)

	case client.ClassAuthFault:
		// MUTE THE SENDER, REQUEUE THE ROW. Two different things are wrong-
		// looking here and only one of them is: the configuration is broken,
		// the posting is not.
		//
		// A 401/403 is positive evidence the middleware did NOT book this
		// transaction, so the row is safe to send again — and it must be,
		// because the fix is a key in a yaml file and a restart, after which
		// this exact posting should go out. Failing it made the one row that
		// happened to be first in the queue permanently a person's problem
		// while its identical neighbours stayed pending, which is an arbitrary
		// distinction between rows that differ only in ordering.
		//
		// The mute is what protects the rest of the queue; it does not need
		// this row's corpse to do it.
		if err := p.store.MarkCMSPostingPending(posting.ID, errText, nextRetry(attempts, p.cfg.PollInterval)); err != nil {
			p.logf("cms poster: requeue posting %d after a credential fault: %v", posting.ID, err)
		}
		p.mute(fmt.Sprintf("credential fault on posting %d (%d): %s", posting.ID, res.HTTPStatus, errText))

	case client.ClassRetryableBeforeSend:
		if attempts >= p.cfg.MaxAttempts {
			p.fail(posting, fmt.Sprintf("gave up after %d attempts: %s", attempts, errText))
			return
		}
		// Nothing arrived, so the row can safely go back in the queue.
		if err := p.store.MarkCMSPostingPending(posting.ID, errText, nextRetry(attempts, p.cfg.PollInterval)); err != nil {
			p.logf("cms poster: requeue posting %d: %v", posting.ID, err)
		}

	case client.ClassRetryableAfterSend:
		if attempts >= p.cfg.MaxAttempts {
			p.fail(posting, fmt.Sprintf("gave up after %d attempts: %s", attempts, errText))
			return
		}
		// It MAY have landed. The row stays inflight; only the reconciler,
		// having asked the middleware, may return it to the queue. Nothing is
		// scheduled, because there is no retry to schedule — the reconciler's
		// own tick is what comes next.
		if err := p.store.RecordCMSPostingAfterSendError(posting.ID, errText); err != nil {
			p.logf("cms poster: record after-send error for posting %d: %v", posting.ID, err)
		}
	}
}

func (p *Poster) fail(posting *cms.Posting, why string) {
	if err := p.store.MarkCMSPostingFailed(posting.ID, why); err != nil {
		p.logf("cms poster: mark posting %d failed: %v", posting.ID, err)
	}
	p.logf("cms poster: posting %d FAILED: %s", posting.ID, why)
}

// nextRetry is exponential backoff on the poll interval, capped.
//
// Capped rather than unbounded because the cap is what makes MaxAttempts a
// bounded amount of TIME rather than an unbounded one: doubling twelve times
// off a 30s base would put the last attempt a fortnight out, by which point
// nobody is watching for it.
func nextRetry(attempts int, base time.Duration) time.Time {
	if base <= 0 {
		base = DefaultPollInterval
	}
	const maxBackoff = 15 * time.Minute
	shift := attempts - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 20 {
		shift = 20
	}
	d := time.Duration(math.Min(float64(base)*math.Pow(2, float64(shift)), float64(maxBackoff)))
	return time.Now().Add(d)
}

// Muted reports whether the drain is halted.
func (p *Poster) Muted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.muted
}

// MutedReason returns why, for the health surface.
func (p *Poster) MutedReason() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mutedWhy
}

func (p *Poster) mute(why string) {
	p.mu.Lock()
	already := p.muted
	p.muted, p.mutedWhy = true, why
	p.mu.Unlock()
	if !already {
		p.logf("cms poster: MUTED — %s. No further postings will be sent until this is "+
			"fixed and core is RESTARTED. The queued postings are unharmed and go out "+
			"on the next start.", why)
	}
}

// ReconcileOnce resolves inflight postings whose fate is unknown.
//
// THE ONLY PATH FROM INFLIGHT BACK TO THE QUEUE, and it goes through asking the
// middleware. A positive "I do not have it" is the only evidence that
// re-sending is safe; anything else leaves the row where it is.
func (p *Poster) ReconcileOnce(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			p.logf("cms poster: PANIC in reconcile: %v\n%s", r, debug.Stack())
		}
	}()

	aged, err := p.store.ListInflightCMSPostingsOlderThan(p.cfg.SettleWindow)
	if err != nil {
		p.logf("cms poster: read inflight postings: %v", err)
		return
	}
	for _, posting := range aged {
		if ctx.Err() != nil {
			return
		}
		if posting.TransactionID == "" {
			// A crash between the send and the acknowledgement. There is no id
			// to ask about, and NOTHING here may re-send it — an unmatched
			// inflight row is exactly the state that has no safe automatic
			// resolution. It ages, and step 17's health surface makes it loud.
			p.logf("cms poster: posting %d has been inflight since %s with no transaction id — "+
				"it cannot be resolved automatically and needs a person to check the middleware",
				posting.ID, inflightSince(posting))
			continue
		}
		found, err := p.transport.GetByTxID(ctx, posting.TransactionID)
		if err != nil {
			// COULD NOT ASK. Not an answer. Leave the row alone and try again
			// next tick; treating this as not-found would re-send a posting
			// that landed.
			p.logf("cms poster: could not ask about posting %d (%s): %v",
				posting.ID, posting.TransactionID, err)
			continue
		}
		if found {
			if err := p.store.MarkCMSPostingPosted(posting.ID, posting.TransactionID,
				httpStatusReconciledViaGet); err != nil {
				p.logf("cms poster: settle posting %d as posted: %v", posting.ID, err)
			}
			continue
		}
		// The middleware does not have it. This is the one positive signal that
		// makes a re-send safe.
		//
		// BOUNDED, because this is a cycle and Commit 1 made it reachable: a
		// send that fails after the bytes go out leaves the row inflight, the
		// reconciler asks, the middleware says no, the row goes back to
		// pending, and it can fail the same way again. requeue_count was
		// already being incremented for exactly this and nothing read it — a
		// bound with no ceiling is not a bound. attempts does not stop it
		// either: a requeue deliberately preserves the attempt budget, so a
		// loop that never sends successfully could turn forever.
		if posting.RequeueCount >= p.cfg.MaxRequeues {
			p.fail(posting, fmt.Sprintf(
				"the reconciler has returned this posting to the queue %d times and the "+
					"middleware still does not hold it; stopping rather than looping",
				posting.RequeueCount))
			continue
		}
		if err := p.store.RequeueCMSPosting(posting.ID); err != nil {
			p.logf("cms poster: requeue posting %d after a confirmed miss: %v", posting.ID, err)
			continue
		}
		p.logf("cms poster: posting %d was not held by the middleware — returned to the queue "+
			"(requeue %d of %d)", posting.ID, posting.RequeueCount+1, p.cfg.MaxRequeues)
		p.Ring()
	}
}

// httpStatusReconciledViaGet is the http_status stamped on a posting the
// RECONCILER settled, and it is a sentinel rather than an observed response.
//
// Nothing answered 200 to a POST here: the POST's answer was lost, and what
// this records is a GET that said the middleware holds the transaction. The
// column is one field for two different questions, and a bare 200 in the call
// reads as the first. Named, a person reading http_status=200 beside a
// posted_at long after inflight_at has somewhere to go for why.
const httpStatusReconciledViaGet = 200

func inflightSince(posting *cms.Posting) string {
	if posting.InflightAt == nil {
		return "an unknown time"
	}
	return posting.InflightAt.Format(time.RFC3339)
}
