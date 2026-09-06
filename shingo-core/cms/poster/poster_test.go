package poster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"shingocore/cms/client"
	"shingocore/cms/wire"
	"shingocore/store/cms"
)

// ── fakes ───────────────────────────────────────────────────────────────

// fakeStore is an in-memory cms_postings / cms_transactions pair. It enforces
// the same guards the SQL does, because those guards ARE the design: a fake
// that let an inflight row be re-armed would make the poster's tests agree with
// a poster that double-books.
type fakeStore struct {
	mu       sync.Mutex
	postings map[int64]*cms.Posting
	txns     map[int64][]*cms.Transaction // by posting id
	nextID   int64

	failCreate   error
	failAttach   error
	failNextPend error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		postings: map[int64]*cms.Posting{},
		txns:     map[int64][]*cms.Transaction{},
	}
}

func (f *fakeStore) CreateCMSPosting(p *cms.Posting) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreate != nil {
		return f.failCreate
	}
	f.nextID++
	p.ID = f.nextID
	p.Status = cms.StatusPending
	now := time.Now()
	p.NextRetryAt = &now
	cp := *p
	f.postings[p.ID] = &cp
	return nil
}

func (f *fakeStore) AttachCMSPosting(txnIDs []int64, postingID int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAttach != nil {
		return 0, f.failAttach
	}
	rows := make([]*cms.Transaction, 0, len(txnIDs))
	for _, id := range txnIDs {
		rows = append(rows, &cms.Transaction{
			ID: id, CatID: fmt.Sprintf("PART-%d", id), Delta: int64(id),
			Storeroom: "SM01", BinLabel: "B", SourceType: "movement",
		})
	}
	f.txns[postingID] = rows
	return len(rows), nil
}

func (f *fakeStore) NextPendingCMSPostings(limit int) ([]*cms.Posting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNextPend != nil {
		return nil, f.failNextPend
	}
	var out []*cms.Posting
	for _, p := range f.postings {
		// PENDING ONLY, and past its retry time — the same filter the SQL
		// applies. A fake that returned inflight rows would let a broken
		// poster pass.
		if p.Status != cms.StatusPending {
			continue
		}
		if p.NextRetryAt != nil && p.NextRetryAt.After(time.Now()) {
			continue
		}
		cp := *p
		out = append(out, &cp)
	}
	// ORDER BY id, as the SQL does. Ranging a map is randomised, and a fake
	// that hands rows back in a different order each run makes any test about
	// WHICH posting was tried first flake rather than fail.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeStore) ListCMSTransactionsByPosting(postingID int64) ([]*cms.Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.txns[postingID], nil
}

func (f *fakeStore) MarkCMSPostingInflight(id int64, batchKey, bodySHA string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.postings[id]
	if !ok {
		return fmt.Errorf("no posting %d", id)
	}
	if p.Status != cms.StatusPending {
		return fmt.Errorf("cms posting %d is not pending", id)
	}
	now := time.Now()
	p.Status, p.BatchKey, p.BodySHA, p.InflightAt = cms.StatusInflight, batchKey, bodySHA, &now
	p.Attempts++
	return nil
}

func (f *fakeStore) MarkCMSPostingPosted(id int64, transactionID string, httpStatus int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.postings[id]
	if p == nil {
		return fmt.Errorf("no posting %d", id)
	}
	p.Status, p.TransactionID, p.HTTPStatus = cms.StatusPosted, transactionID, &httpStatus
	p.NextRetryAt = nil
	return nil
}

func (f *fakeStore) MarkCMSPostingRejected(id int64, httpStatus int, lastErr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.postings[id]
	p.Status, p.HTTPStatus, p.LastError, p.NextRetryAt = cms.StatusRejected, &httpStatus, lastErr, nil
	return nil
}

func (f *fakeStore) MarkCMSPostingFailed(id int64, lastErr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.postings[id]
	p.Status, p.LastError, p.NextRetryAt = cms.StatusFailed, lastErr, nil
	return nil
}

func (f *fakeStore) MarkCMSPostingPending(id int64, lastErr string, nextRetry time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.postings[id]
	p.Status, p.LastError, p.NextRetryAt, p.InflightAt = cms.StatusPending, lastErr, &nextRetry, nil
	return nil
}

func (f *fakeStore) ScheduleCMSPostingRetry(id int64, lastErr string, nextRetry time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.postings[id]
	// Attempts deliberately untouched: MarkInflight counts them.
	p.LastError, p.NextRetryAt = lastErr, &nextRetry
	return nil
}

func (f *fakeStore) ListInflightCMSPostingsOlderThan(d time.Duration) ([]*cms.Posting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*cms.Posting
	cutoff := time.Now().Add(-d)
	for _, p := range f.postings {
		if p.Status == cms.StatusInflight && p.InflightAt != nil && p.InflightAt.Before(cutoff) {
			cp := *p
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeStore) RequeueCMSPosting(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.postings[id]
	if p == nil || p.Status != cms.StatusInflight {
		return fmt.Errorf("cms posting %d is not inflight", id)
	}
	now := time.Now()
	p.Status, p.RequeueCount, p.NextRetryAt, p.InflightAt, p.TransactionID =
		cms.StatusPending, p.RequeueCount+1, &now, nil, ""
	return nil
}

func (f *fakeStore) get(id int64) *cms.Posting {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.postings[id]
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// ageInflight backdates a row so the reconciler will look at it.
func (f *fakeStore) ageInflight(id int64, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := time.Now().Add(-d)
	f.postings[id].InflightAt = &t
}

// fakeTransport answers with a scripted sequence, recording what it was sent.
type fakeTransport struct {
	mu       sync.Mutex
	results  []client.PostResult
	bodies   [][]byte
	shas     []string
	getFound bool
	getErr   error
	getCalls []string
	panicOn  int
}

func (f *fakeTransport) Post(ctx context.Context, body []byte, sha string) client.PostResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.bodies)
	f.bodies = append(f.bodies, append([]byte(nil), body...))
	f.shas = append(f.shas, sha)
	if f.panicOn == n+1 {
		panic("scripted transport panic")
	}
	if n < len(f.results) {
		return f.results[n]
	}
	if len(f.results) == 0 {
		return client.PostResult{Class: client.ClassPosted, TransactionID: "MW-1", HTTPStatus: 200}
	}
	return f.results[len(f.results)-1]
}

func (f *fakeTransport) GetByTxID(ctx context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls = append(f.getCalls, id)
	return f.getFound, f.getErr
}

func (f *fakeTransport) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func testPoster(t *testing.T, store Store, tr Transport) *Poster {
	t.Helper()
	return New(store, tr, Config{
		PollInterval: time.Second,
		MaxAttempts:  3,
		SettleWindow: time.Minute,
		Wire: wire.Config{
			ReasonCode: "TEST-AMR", IncreaseType: "I", DecreaseType: "D",
			UnitOfMeasure: "EA", UserID: "SHINGO",
		},
	}, func(string, ...any) {})
}

func enqueue(t *testing.T, p *Poster, ids ...int64) {
	t.Helper()
	rows := make([]*cms.Transaction, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, &cms.Transaction{ID: id, SourceType: "movement"})
	}
	if err := p.Enqueue(rows); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
}

// ── the class → status map ──────────────────────────────────────────────

func TestDrain_ClassToStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		result     client.PostResult
		wantStatus string
		why        string
	}{
		{
			name:       "posted",
			result:     client.PostResult{Class: client.ClassPosted, TransactionID: "MW-9", HTTPStatus: 200},
			wantStatus: cms.StatusPosted,
			why:        "acknowledged and named",
		},
		{
			name:       "accepted without an id stays inflight",
			result:     client.PostResult{Class: client.ClassInflight, HTTPStatus: 200, Err: errors.New("no id")},
			wantStatus: cms.StatusInflight,
			why:        "it landed and we cannot name it — only the reconciler may move it",
		},
		{
			name:       "rejected is terminal",
			result:     client.PostResult{Class: client.ClassRejected, HTTPStatus: 400, Err: errors.New("bad body")},
			wantStatus: cms.StatusRejected,
			why:        "the same body would be refused again",
		},
		{
			name:       "5xx stays inflight",
			result:     client.PostResult{Class: client.ClassRetryableAfterSend, HTTPStatus: 503, Err: errors.New("unavailable")},
			wantStatus: cms.StatusInflight,
			why:        "the server has the request; it may have booked it before failing",
		},
		{
			name:       "dns failure returns to the queue",
			result:     client.PostResult{Class: client.ClassRetryableBeforeSend, Err: errors.New("no such host")},
			wantStatus: cms.StatusPending,
			why:        "nothing arrived, so re-sending cannot duplicate",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newFakeStore()
			tr := &fakeTransport{results: []client.PostResult{tc.result}}
			p := testPoster(t, store, tr)
			enqueue(t, p, 1)
			p.DrainOnce(context.Background())

			got := store.get(1)
			if got.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q — %s", got.Status, tc.wantStatus, tc.why)
			}
		})
	}
}

// TestDrain_AfterSendFailureIsNeverReturnedToTheQueue is the duplicate guard,
// stated as the property rather than as a status check. A 5xx row must not
// become sendable again on its own at any point: the poster only reads pending
// rows, so leaving it inflight is what prevents the second POST.
func TestDrain_AfterSendFailureIsNeverReturnedToTheQueue(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tr := &fakeTransport{results: []client.PostResult{
		{Class: client.ClassRetryableAfterSend, HTTPStatus: 500, Err: errors.New("boom")},
	}}
	p := testPoster(t, store, tr)
	enqueue(t, p, 1)

	// Drain repeatedly. A row that ever flipped back to pending would be picked
	// up and sent a second time.
	for i := 0; i < 5; i++ {
		p.DrainOnce(context.Background())
	}
	if n := tr.postCount(); n != 1 {
		t.Errorf("the middleware was sent %d copies, want 1 — a request that may have "+
			"landed must not be re-sent without asking", n)
	}
	if got := store.get(1); got.Status != cms.StatusInflight {
		t.Errorf("status = %q, want inflight", got.Status)
	}
}

// TestDrain_BeforeSendFailureIsRetriedThenGivesUp: the other side of the same
// rule. Nothing arrived, so re-sending is safe — but not forever.
func TestDrain_BeforeSendFailureIsRetriedThenGivesUp(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tr := &fakeTransport{results: []client.PostResult{
		{Class: client.ClassRetryableBeforeSend, Err: errors.New("no such host")},
	}}
	p := testPoster(t, store, tr) // MaxAttempts 3
	enqueue(t, p, 1)

	for i := 0; i < 6; i++ {
		// The backoff schedules the retry into the future; clear it so the
		// test exercises the attempt COUNT rather than the clock.
		store.mu.Lock()
		if pp := store.postings[1]; pp != nil && pp.Status == cms.StatusPending {
			now := time.Now().Add(-time.Second)
			pp.NextRetryAt = &now
		}
		store.mu.Unlock()
		p.DrainOnce(context.Background())
	}

	got := store.get(1)
	if got.Status != cms.StatusFailed {
		t.Errorf("status = %q, want failed after exhausting attempts", got.Status)
	}
	if tr.postCount() != 3 {
		t.Errorf("posts = %d, want 3 (MaxAttempts) — the budget must bound the retries", tr.postCount())
	}
	// Failed, not dropped. The Kafka outbox discards after ten attempts and
	// docs/outbox-ordering.md calls that a hole; an inventory ledger cannot
	// have one, so the row stays for a person.
	if !strings.Contains(got.LastError, "gave up") {
		t.Errorf("last_error = %q, want it to say the poster gave up and why", got.LastError)
	}
}

// TestDrain_AuthFaultMutesRatherThanBurningEveryBudget: with a bad credential
// every queued posting fails identically. Letting them all exhaust their
// attempts turns a five-minute config fix into a table of dead rows.
func TestDrain_AuthFaultMutesRatherThanBurningEveryBudget(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tr := &fakeTransport{results: []client.PostResult{
		{Class: client.ClassAuthFault, HTTPStatus: 401, Err: errors.New("denied")},
	}}
	p := testPoster(t, store, tr)
	enqueue(t, p, 1)
	enqueue(t, p, 2)
	enqueue(t, p, 3)

	p.DrainOnce(context.Background())

	if !p.Muted() {
		t.Fatal("a credential fault did not mute the poster")
	}
	if tr.postCount() != 1 {
		t.Errorf("posts = %d, want 1 — the other queued postings must not be tried "+
			"against a credential nobody has fixed", tr.postCount())
	}
	if store.get(2).Status != cms.StatusPending || store.get(3).Status != cms.StatusPending {
		t.Error("the untried postings did not stay pending — they must survive the mute")
	}
	if !strings.Contains(p.MutedReason(), "credential") {
		t.Errorf("muted reason = %q, want it to name the credential fault", p.MutedReason())
	}

	// Further drains do nothing until somebody clears it.
	p.DrainOnce(context.Background())
	if tr.postCount() != 1 {
		t.Errorf("posts = %d after a second drain, want 1 — muted means muted", tr.postCount())
	}
	p.Unmute()
	p.DrainOnce(context.Background())
	if tr.postCount() == 1 {
		t.Error("unmuting did not resume the drain")
	}
}

// TestDrain_PanicInOneSendDoesNotStrandTheRest. The boundary is per POSTING:
// one malformed response must not take the loop down and hold every other row
// behind it.
func TestDrain_PanicInOneSendDoesNotStrandTheRest(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tr := &fakeTransport{panicOn: 1} // the first send panics; the rest succeed
	p := testPoster(t, store, tr)
	enqueue(t, p, 1)
	enqueue(t, p, 2)
	enqueue(t, p, 3)

	p.DrainOnce(context.Background()) // must not panic out

	posted := 0
	for id := int64(1); id <= 3; id++ {
		if store.get(id).Status == cms.StatusPosted {
			posted++
		}
	}
	if posted != 2 {
		t.Errorf("posted %d of the 3 postings, want 2 — one panicked and the other two "+
			"must still have been sent", posted)
	}
}

// TestDrain_MarksInflightBeforeSending is the crash-recovery contract. If the
// row still said "pending" while a POST was on the wire, a crash would leave it
// eligible for re-send with nothing recording that a copy had gone.
func TestDrain_MarksInflightBeforeSending(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	var statusAtSend string
	tr := &fakeTransport{}
	p := testPoster(t, store, &recordingTransport{
		inner: tr,
		before: func() {
			statusAtSend = store.get(1).Status
		},
	})
	enqueue(t, p, 1)
	p.DrainOnce(context.Background())

	if statusAtSend != cms.StatusInflight {
		t.Errorf("status while the POST was in flight = %q, want inflight — the write must "+
			"commit BEFORE the send, or a crash mid-flight looks like a row that was never sent",
			statusAtSend)
	}
}

type recordingTransport struct {
	inner  Transport
	before func()
}

func (r *recordingTransport) Post(ctx context.Context, body []byte, sha string) client.PostResult {
	if r.before != nil {
		r.before()
	}
	return r.inner.Post(ctx, body, sha)
}

func (r *recordingTransport) GetByTxID(ctx context.Context, id string) (bool, error) {
	return r.inner.GetByTxID(ctx, id)
}

// TestDrain_SendsTheTranslatedBodyAndItsHash: the body on the wire is what
// wire.Build produced, and the sha accompanies it. A hash of something other
// than the body would be worse than no hash — it would make the dedup hint lie.
func TestDrain_SendsTheTranslatedBodyAndItsHash(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tr := &fakeTransport{}
	p := testPoster(t, store, tr)
	enqueue(t, p, 5)
	p.DrainOnce(context.Background())

	if tr.postCount() != 1 {
		t.Fatalf("posts = %d, want 1", tr.postCount())
	}
	var rows []map[string]any
	if err := json.Unmarshal(tr.bodies[0], &rows); err != nil {
		t.Fatalf("the body is not a JSON array: %v (%s)", err, tr.bodies[0])
	}
	if len(rows) != 1 {
		t.Fatalf("body carries %d rows, want 1: %s", len(rows), tr.bodies[0])
	}
	if rows[0]["ReasonCode"] != "TEST-AMR" || rows[0]["UserId"] != "SHINGO" {
		t.Errorf("body does not carry the configured vocabulary: %s", tr.bodies[0])
	}
	if tr.shas[0] != client.BodySHA(tr.bodies[0]) {
		t.Errorf("the sha sent (%s) is not the hash of the body sent", tr.shas[0])
	}
}

// ── enqueue ─────────────────────────────────────────────────────────────

func TestEnqueue_CreatesOnePendingPostingAndRings(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	p := testPoster(t, store, &fakeTransport{})
	enqueue(t, p, 1, 2, 3)

	got := store.get(1)
	if got == nil || got.Status != cms.StatusPending {
		t.Fatalf("posting = %+v, want one pending row", got)
	}
	if got.BatchKey == "" {
		t.Error("batch_key is empty — it is what identifies this POST in a log after a crash")
	}
	rows, _ := store.ListCMSTransactionsByPosting(1)
	if len(rows) != 3 {
		t.Errorf("posting carries %d transactions, want 3", len(rows))
	}
	// The doorbell must be armed, or the row waits for a ticker it did not
	// need to.
	select {
	case <-p.wake:
	default:
		t.Error("Enqueue did not ring the doorbell")
	}
}

func TestEnqueue_IgnoresRowsWithNoID(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	p := testPoster(t, store, &fakeTransport{})
	// Unsaved rows have no id and cannot be attached to anything.
	if err := p.Enqueue([]*cms.Transaction{{SourceType: "movement"}, nil}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if store.get(1) != nil {
		t.Error("a posting was created for transactions that have no ids")
	}
}

// ── reconcile ───────────────────────────────────────────────────────────

func TestReconcile_ConfirmedHitSettlesThePosting(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tr := &fakeTransport{
		results:  []client.PostResult{{Class: client.ClassRetryableAfterSend, HTTPStatus: 500, Err: errors.New("boom")}},
		getFound: true,
	}
	p := testPoster(t, store, tr)
	enqueue(t, p, 1)
	p.DrainOnce(context.Background())
	// Give it a transaction id, as a 2xx-then-timeout would have.
	store.mu.Lock()
	store.postings[1].TransactionID = "MW-77"
	store.mu.Unlock()
	store.ageInflight(1, 2*time.Minute)

	p.ReconcileOnce(context.Background())

	if got := store.get(1); got.Status != cms.StatusPosted {
		t.Errorf("status = %q, want posted — the middleware confirmed it holds the transaction", got.Status)
	}
}

// TestReconcile_ConfirmedMissIsTheOnlyThingThatRequeues. A positive "I do not
// have it" is the only evidence that re-sending is safe.
func TestReconcile_ConfirmedMissIsTheOnlyThingThatRequeues(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tr := &fakeTransport{
		results:  []client.PostResult{{Class: client.ClassRetryableAfterSend, HTTPStatus: 500, Err: errors.New("boom")}},
		getFound: false,
	}
	p := testPoster(t, store, tr)
	enqueue(t, p, 1)
	p.DrainOnce(context.Background())
	store.mu.Lock()
	store.postings[1].TransactionID = "MW-78"
	store.mu.Unlock()
	store.ageInflight(1, 2*time.Minute)

	p.ReconcileOnce(context.Background())

	got := store.get(1)
	if got.Status != cms.StatusPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if got.RequeueCount != 1 {
		t.Errorf("requeue_count = %d, want 1", got.RequeueCount)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 preserved — a requeue must not refill the retry budget", got.Attempts)
	}
}

// TestReconcile_CouldNotAskLeavesTheRowAlone. "The query failed" is not "the
// middleware does not have it", and collapsing the two re-sends a transaction
// that landed.
func TestReconcile_CouldNotAskLeavesTheRowAlone(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tr := &fakeTransport{
		results: []client.PostResult{{Class: client.ClassRetryableAfterSend, HTTPStatus: 500, Err: errors.New("boom")}},
		getErr:  errors.New("middleware unreachable"),
	}
	p := testPoster(t, store, tr)
	enqueue(t, p, 1)
	p.DrainOnce(context.Background())
	store.mu.Lock()
	store.postings[1].TransactionID = "MW-79"
	store.mu.Unlock()
	store.ageInflight(1, 2*time.Minute)

	p.ReconcileOnce(context.Background())

	if got := store.get(1); got.Status != cms.StatusInflight {
		t.Errorf("status = %q, want inflight — a failed query is not an answer", got.Status)
	}
	if tr.postCount() != 1 {
		t.Errorf("posts = %d, want 1 — nothing may be re-sent on a non-answer", tr.postCount())
	}
}

// TestReconcile_NoTransactionIDIsNeverAutoResolved: a crash between the send
// and the acknowledgement leaves a row with nothing to ask about. There is no
// safe automatic resolution — re-sending might double-book, and marking it
// posted would invent a success. It ages, loudly.
func TestReconcile_NoTransactionIDIsNeverAutoResolved(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tr := &fakeTransport{
		results:  []client.PostResult{{Class: client.ClassInflight, HTTPStatus: 200, Err: errors.New("no id in body")}},
		getFound: false,
	}
	p := testPoster(t, store, tr)
	enqueue(t, p, 1)
	p.DrainOnce(context.Background())
	store.ageInflight(1, 2*time.Minute)

	p.ReconcileOnce(context.Background())

	if got := store.get(1); got.Status != cms.StatusInflight {
		t.Errorf("status = %q, want inflight — an unidentifiable posting has no safe automatic move", got.Status)
	}
	if len(tr.getCalls) != 0 {
		t.Errorf("the reconciler queried the middleware %d times with no id to query for", len(tr.getCalls))
	}
	if tr.postCount() != 1 {
		t.Errorf("posts = %d, want 1 — an unidentifiable posting must NEVER be re-sent", tr.postCount())
	}
}

// TestReconcile_RespectsTheSettleWindow: a POST that just went out has not had
// time to be indexed on the middleware side, and asking too early gets a
// not-found that would requeue something that landed.
func TestReconcile_RespectsTheSettleWindow(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tr := &fakeTransport{
		results:  []client.PostResult{{Class: client.ClassRetryableAfterSend, HTTPStatus: 500, Err: errors.New("boom")}},
		getFound: false,
	}
	p := testPoster(t, store, tr)
	enqueue(t, p, 1)
	p.DrainOnce(context.Background())
	store.mu.Lock()
	store.postings[1].TransactionID = "MW-80"
	store.mu.Unlock()
	// NOT aged: it went inflight a moment ago.

	p.ReconcileOnce(context.Background())

	if got := store.get(1); got.Status != cms.StatusInflight {
		t.Errorf("status = %q, want inflight — a fresh posting is inside the settle window", got.Status)
	}
	if len(tr.getCalls) != 0 {
		t.Errorf("the reconciler asked about a posting %d ms old", 0)
	}
}

// ── backoff ─────────────────────────────────────────────────────────────

// TestNextRetry_GrowsAndIsCapped. The cap is what makes MaxAttempts a bounded
// amount of TIME: doubling twelve times off a 30s base would put the last
// attempt a fortnight out, by which point nobody is watching for it.
func TestNextRetry_GrowsAndIsCapped(t *testing.T) {
	t.Parallel()
	base := 30 * time.Second
	var prev time.Duration
	for attempts := 1; attempts <= 12; attempts++ {
		d := time.Until(nextRetry(attempts, base))
		if d < prev-time.Second {
			t.Errorf("attempt %d waits %s, less than the previous %s — backoff must not shrink",
				attempts, d, prev)
		}
		if d > 16*time.Minute {
			t.Errorf("attempt %d waits %s, past the 15m cap", attempts, d)
		}
		prev = d
	}
	if first := time.Until(nextRetry(1, base)); first > 31*time.Second {
		t.Errorf("the first retry waits %s, want about the poll interval", first)
	}
}

func TestNextRetry_ZeroBaseDoesNotBusyLoop(t *testing.T) {
	t.Parallel()
	// A zero interval would schedule every retry for "now" and spin the drain.
	if d := time.Until(nextRetry(1, 0)); d < time.Second {
		t.Errorf("a zero base gives a %s retry delay — that is a busy loop", d)
	}
}
