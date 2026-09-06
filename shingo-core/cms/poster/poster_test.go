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

	// orphans are transactions no posting has claimed — the sweep's input.
	// AttachCMSPosting removes what it claims, mirroring the SQL's
	// posting_id IS NULL guard.
	orphans []*cms.Transaction

	failCreate       error
	failAttach       error
	failNextPend     error
	failListUnposted error
	// failMarkPosted reproduces the one after-send failure the reconciler can
	// resolve: the POST succeeded and named an id, and the write that would
	// have settled the row did not land.
	failMarkPosted error
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
			Storeroom: "SM01", BinLabel: "B", SourceType: cms.SourceTypeMovement,
		})
	}
	f.txns[postingID] = rows
	// A claimed row is no longer unposted — the SQL's posting_id IS NULL guard,
	// in the fake, so a sweep test cannot pass by sweeping the same rows twice.
	claimed := make(map[int64]bool, len(txnIDs))
	for _, id := range txnIDs {
		claimed[id] = true
	}
	kept := f.orphans[:0]
	for _, t := range f.orphans {
		if !claimed[t.ID] {
			kept = append(kept, t)
		}
	}
	f.orphans = kept
	return len(rows), nil
}

// ListUnpostedCMSTransactions ignores age: every orphan a test plants is one it
// wants swept. The real query's grace period is about racing a live subscriber,
// which no test here has.
func (f *fakeStore) ListUnpostedCMSTransactions(age time.Duration, limit int) ([]*cms.Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failListUnposted != nil {
		return nil, f.failListUnposted
	}
	out := make([]*cms.Transaction, len(f.orphans))
	copy(out, f.orphans)
	return out, nil
}

// orphan plants a transaction the subscriber recorded and no posting claimed.
func (f *fakeStore) orphan(ids ...int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		f.orphans = append(f.orphans, &cms.Transaction{
			ID: id, CatID: fmt.Sprintf("PART-%d", id), Delta: int64(id),
			Storeroom: "SM01", BinLabel: "B", SourceType: cms.SourceTypeMovement,
		})
	}
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

func (f *fakeStore) MarkCMSPostingInflight(id int64, bodySHA string) error {
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
	p.Status, p.BodySHA, p.InflightAt = cms.StatusInflight, bodySHA, &now
	p.Attempts++
	return nil
}

// MarkCMSPostingTransactionID mirrors the SQL's inflight guard: it writes the
// id and touches nothing else.
func (f *fakeStore) MarkCMSPostingTransactionID(id int64, transactionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.postings[id]
	if p == nil {
		return fmt.Errorf("no posting %d", id)
	}
	if p.Status != cms.StatusInflight {
		return fmt.Errorf("cms posting %d is not inflight", id)
	}
	p.TransactionID = transactionID
	return nil
}

func (f *fakeStore) MarkCMSPostingPosted(id int64, transactionID string, httpStatus int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failMarkPosted != nil {
		return f.failMarkPosted
	}
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

func (f *fakeStore) RecordCMSPostingAfterSendError(id int64, lastErr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.postings[id]
	// Attempts deliberately untouched: MarkInflight counts them. next_retry_at
	// is untouched too — an inflight row is not on a retry clock, its only exit
	// is the reconciler.
	p.LastError = lastErr
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
	// AND POSTING 1, THE ONE THAT FAULTED. This test asserted only on the rows
	// that were never tried, so it was blind to what happened to the row that
	// was — which is where the bug was. A 401 is positive evidence the
	// middleware did not book it, and the fix is a key in a yaml file, so the
	// row is safe to send again and must be waiting to be.
	if got := store.get(1); got.Status != cms.StatusPending {
		t.Errorf("the faulting posting is %q, want pending — a credential fault is a "+
			"statement about the configuration, not about this row, and failing it makes "+
			"the row that happened to be first in the queue a person's problem while its "+
			"identical neighbours stay queued", got.Status)
	}
	if !strings.Contains(p.MutedReason(), "credential") {
		t.Errorf("muted reason = %q, want it to name the credential fault", p.MutedReason())
	}

	// Further drains do nothing. A mute is cleared by restarting core — there
	// is no unmute verb, deliberately: the fault it fires on is a credential,
	// and a credential is fixed in the yaml that is read at boot.
	p.DrainOnce(context.Background())
	if tr.postCount() != 1 {
		t.Errorf("posts = %d after a second drain, want 1 — muted means muted", tr.postCount())
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

// TestSendOne_MarkPostedFailurePersistsTxID guards the reconciler's only
// reachable entrance.
//
// The POST succeeded and named an id; the write that would have settled the row
// failed. Before the id was written first, that lost it, leaving an inflight
// row with nothing to ask about — the one inflight state nothing can resolve.
// The row must end up inflight AND carrying the id.
func TestSendOne_MarkPostedFailurePersistsTxID(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.failMarkPosted = errors.New("connection reset by peer")
	tr := &fakeTransport{results: []client.PostResult{
		{Class: client.ClassPosted, TransactionID: "MW-4242", HTTPStatus: 200},
	}}
	p := testPoster(t, store, tr)
	enqueue(t, p, 1)

	p.DrainOnce(context.Background())

	got := store.get(1)
	if got.Status != cms.StatusInflight {
		t.Errorf("status = %q, want inflight — the settling write failed", got.Status)
	}
	if got.TransactionID != "MW-4242" {
		t.Fatalf("transaction_id = %q, want %q — without it the reconciler has nothing to "+
			"ask the middleware about and the row can never be resolved",
			got.TransactionID, "MW-4242")
	}
}

// TestReconcile_ResolvesInflightWithTxIDFromDBBlip drives that same production
// path and then reconciles what it left.
//
// The reconcile tests below used to reach into the fake and assign a
// TransactionID by hand, under a comment saying "as a 2xx-then-timeout would
// have" — a thing no production path did, because MarkPosted carried the id and
// the status together and a lost acknowledgement lost both. They now drive this
// same path, so the fixture is the production one rather than a description of
// it.
func TestReconcile_ResolvesInflightWithTxIDFromDBBlip(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.failMarkPosted = errors.New("connection reset by peer")
	tr := &fakeTransport{
		results:  []client.PostResult{{Class: client.ClassPosted, TransactionID: "MW-4243", HTTPStatus: 200}},
		getFound: true,
	}
	p := testPoster(t, store, tr)
	enqueue(t, p, 1)
	p.DrainOnce(context.Background())

	// The blip is over: the settling write would work now.
	store.mu.Lock()
	store.failMarkPosted = nil
	store.mu.Unlock()
	store.ageInflight(1, 2*time.Minute)

	p.ReconcileOnce(context.Background())

	got := store.get(1)
	if got.Status != cms.StatusPosted {
		t.Errorf("status = %q, want posted — the middleware confirmed it holds MW-4243", got.Status)
	}
	if len(tr.getCalls) != 1 || tr.getCalls[0] != "MW-4243" {
		t.Errorf("reconciler asked about %v, want exactly [MW-4243]", tr.getCalls)
	}
	if tr.postCount() != 1 {
		t.Errorf("posts = %d, want 1 — resolving must never re-send", tr.postCount())
	}
}

// TestDrain_AuthFaultRequeuesTheFaultingPosting.
//
// A 401 is not a statement about the posting. It is positive evidence the
// middleware did NOT book it — so the row is safe to send again, and it must
// be, because the fix is a key in a yaml file and a restart after which this
// exact posting should go out.
//
// Failing it made the row that happened to be first in the queue permanently a
// person's problem while its identical neighbours stayed pending: an arbitrary
// distinction between rows differing only in ordering. The mute is what
// protects the rest of the queue, and it does not need a corpse to do it.
func TestDrain_AuthFaultRequeuesTheFaultingPosting(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tr := &fakeTransport{results: []client.PostResult{
		{Class: client.ClassAuthFault, HTTPStatus: 401, Err: errors.New("denied")},
	}}
	p := testPoster(t, store, tr)
	enqueue(t, p, 1)

	p.DrainOnce(context.Background())

	got := store.get(1)
	if got.Status == cms.StatusFailed {
		t.Errorf("the faulting posting was failed. A config fault that a yaml edit repairs "+
			"must not turn a queued transfer into a row somebody has to reason about; "+
			"last_error was %q", got.LastError)
	}
	if got.Status != cms.StatusPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 — the requeue must not refill the retry budget "+
			"either, or a wrong key could loop forever once someone unmutes", got.Attempts)
	}
	if !p.Muted() {
		t.Error("the poster is not muted — requeueing without muting would burn every " +
			"posting's budget against the same bad credential")
	}
}

// ── reconcile ───────────────────────────────────────────────────────────
// ── reconcile ───────────────────────────────────────────────────────────

func TestReconcile_ConfirmedHitSettlesThePosting(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// The row reaches inflight-with-an-id the way production does: the POST
	// returned 2xx and named one, and the write that would have settled the
	// row failed. This used to be set by hand under a comment claiming a
	// 2xx-then-timeout produced it, which nothing ever did.
	store.failMarkPosted = errors.New("connection reset by peer")
	tr := &fakeTransport{
		results:  []client.PostResult{{Class: client.ClassPosted, TransactionID: "MW-77", HTTPStatus: 200}},
		getFound: true,
	}
	p := testPoster(t, store, tr)
	enqueue(t, p, 1)
	p.DrainOnce(context.Background())
	// The blip is over by the time the reconciler runs.
	store.mu.Lock()
	store.failMarkPosted = nil
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
	store.failMarkPosted = errors.New("connection reset by peer")
	tr := &fakeTransport{
		results:  []client.PostResult{{Class: client.ClassPosted, TransactionID: "MW-78", HTTPStatus: 200}},
		getFound: false,
	}
	p := testPoster(t, store, tr)
	enqueue(t, p, 1)
	p.DrainOnce(context.Background())
	store.mu.Lock()
	store.failMarkPosted = nil
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
	store.failMarkPosted = errors.New("connection reset by peer")
	tr := &fakeTransport{
		results: []client.PostResult{{Class: client.ClassPosted, TransactionID: "MW-79", HTTPStatus: 200}},
		getErr:  errors.New("middleware unreachable"),
	}
	p := testPoster(t, store, tr)
	enqueue(t, p, 1)
	p.DrainOnce(context.Background())
	store.mu.Lock()
	store.failMarkPosted = nil
	store.mu.Unlock()
	store.ageInflight(1, 2*time.Minute)

	p.ReconcileOnce(context.Background())

	if got := store.get(1); got.Status != cms.StatusInflight {
		t.Errorf("status = %q, want inflight — a failed query is not an answer", got.Status)
	}
	if tr.postCount() != 1 {
		t.Errorf("posts = %d, want 1 — nothing may be re-sent on a non-answer", tr.postCount())
	}
	// AND IT MUST NOT HAVE BEEN REQUEUED. This is the one reconciler branch
	// where a regression re-sends a posting the middleware is holding: treating
	// "the query failed" as "the middleware does not have it" is a single
	// mis-read of a three-valued answer, and it double-books a transfer on an
	// API with no idempotency key.
	if got := store.get(1); got.RequeueCount != 0 {
		t.Errorf("requeue_count = %d, want 0 — a transport error was read as a confirmed "+
			"miss, which is how a landed transfer gets sent twice", got.RequeueCount)
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
	store.failMarkPosted = errors.New("connection reset by peer")
	tr := &fakeTransport{
		results:  []client.PostResult{{Class: client.ClassPosted, TransactionID: "MW-80", HTTPStatus: 200}},
		getFound: false,
	}
	p := testPoster(t, store, tr)
	enqueue(t, p, 1)
	p.DrainOnce(context.Background())
	store.mu.Lock()
	store.failMarkPosted = nil
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

// TestReconcile_RequeueLoopIsBounded.
//
// The cycle is real and this branch made it reachable: a send that fails after
// the bytes go out leaves the row inflight, the reconciler asks, the middleware
// says it does not hold it, the row goes back to pending, and it can fail the
// same way again. requeue_count was already being incremented for exactly this
// and nothing read it — a bound with no ceiling is not a bound.
//
// attempts does not stop it either: a requeue deliberately PRESERVES the
// attempt budget, so a row that is requeued before it ever exhausts its
// attempts could turn indefinitely.
func TestReconcile_RequeueLoopIsBounded(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// The one path that reaches a requeue: the POST returns 2xx and names an
	// id, the settling write fails, and the middleware then says it does not
	// hold that id after all. An after-send failure would NOT do — it leaves
	// no id, so the reconciler never asks and never requeues.
	store.failMarkPosted = errors.New("connection reset by peer")
	tr := &fakeTransport{
		results:  []client.PostResult{{Class: client.ClassPosted, TransactionID: "MW-LOOP", HTTPStatus: 200}},
		getFound: false, // ...and then denies holding it, every time
	}
	p := New(store, tr, Config{
		PollInterval: time.Millisecond,
		MaxAttempts:  100, // deliberately generous: the requeue bound is what must stop this
		SettleWindow: time.Minute,
		MaxRequeues:  2,
		Wire:         wire.Config{ReasonCode: "T", IncreaseType: "I", DecreaseType: "D", UnitOfMeasure: "EA", UserID: "U"},
	}, func(string, ...any) {})
	enqueue(t, p, 1)

	// Turn the crank well past the bound.
	for i := 0; i < 10; i++ {
		p.DrainOnce(context.Background())
		store.ageInflight(1, 2*time.Minute)
		p.ReconcileOnce(context.Background())
	}

	got := store.get(1)
	if got.Status != cms.StatusFailed {
		t.Errorf("status = %q after ten reconcile passes, want failed — the loop has no other "+
			"ceiling and would turn for as long as the middleware keeps saying no", got.Status)
	}
	if got.RequeueCount > 2 {
		t.Errorf("requeue_count = %d, want at most the configured 2", got.RequeueCount)
	}
	if !strings.Contains(got.LastError, "returned this posting to the queue") {
		t.Errorf("last_error = %q, want it to say the reconciler stopped rather than that a "+
			"send failed — those send someone to different places", got.LastError)
	}
	// And the bound is what stopped it, not the attempt budget.
	if got.Attempts >= 100 {
		t.Errorf("attempts = %d — MaxAttempts was set to 100 precisely so it could not be "+
			"what ended this; if it was, the requeue bound is doing nothing", got.Attempts)
	}
}

// TestReconcile_RequeuesUpToTheBound is the selectivity half: a bound that
// refuses the FIRST requeue would turn one bad round trip into a dead row and
// defeat the recovery the reconciler exists for.
func TestReconcile_RequeuesUpToTheBound(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.failMarkPosted = errors.New("connection reset by peer")
	tr := &fakeTransport{
		results:  []client.PostResult{{Class: client.ClassPosted, TransactionID: "MW-ONCE", HTTPStatus: 200}},
		getFound: false,
	}
	p := New(store, tr, Config{
		PollInterval: time.Millisecond, MaxAttempts: 100, SettleWindow: time.Minute,
		MaxRequeues: 2,
		Wire:        wire.Config{ReasonCode: "T", IncreaseType: "I", DecreaseType: "D", UnitOfMeasure: "EA", UserID: "U"},
	}, func(string, ...any) {})
	enqueue(t, p, 1)

	p.DrainOnce(context.Background())
	store.ageInflight(1, 2*time.Minute)
	p.ReconcileOnce(context.Background())

	got := store.get(1)
	if got.Status != cms.StatusPending {
		t.Errorf("status = %q after ONE confirmed miss, want pending — that is the recovery "+
			"the reconciler exists to perform", got.Status)
	}
	if got.RequeueCount != 1 {
		t.Errorf("requeue_count = %d, want 1", got.RequeueCount)
	}
}

// ── backoff ─────────────────────────────────────────────────────────────
// ── backoff ─────────────────────────────────────────────────────────────

// TestSweep_ReenqueuesOrphanedTransactions is the reconciliation half of the
// doorbell.
//
// Enqueue runs on the event subscriber's goroutine. If it fails — the posting
// insert errors, the attach errors, the process dies between them — the
// transaction rows sit with posting_id NULL and nothing retries, because the
// only thing that would have was the notification that already failed. You
// cannot wire up an absence, so the sweep asks the question directly.
func TestSweep_ReenqueuesOrphanedTransactions(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	p := testPoster(t, store, &fakeTransport{})

	// Two rows the subscriber recorded and never queued.
	store.orphan(41, 42)

	p.SweepOrphansOnce(context.Background())

	got := store.get(1)
	if got == nil {
		t.Fatal("the sweep created no posting — the transactions are still unqueued, and " +
			"the only thing that would have queued them has already failed")
	}
	if got.Status != cms.StatusPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
	rows, _ := store.ListCMSTransactionsByPosting(got.ID)
	if len(rows) != 2 {
		t.Errorf("posting carries %d transactions, want 2", len(rows))
	}
}

// TestSweep_DoesNotReclaimWhatItAlreadySwept: the sweep must converge. A second
// pass over rows a first pass claimed would create a posting per tick, forever,
// for work that is already queued.
func TestSweep_DoesNotReclaimWhatItAlreadySwept(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	p := testPoster(t, store, &fakeTransport{})
	store.orphan(41)

	p.SweepOrphansOnce(context.Background())
	p.SweepOrphansOnce(context.Background())
	p.SweepOrphansOnce(context.Background())

	store.mu.Lock()
	n := len(store.postings)
	store.mu.Unlock()
	if n != 1 {
		t.Errorf("%d postings after three sweeps, want 1 — a claimed transaction is no "+
			"longer unposted, and a sweep that cannot see that manufactures work", n)
	}
}

// TestSweep_StaysQuietWhenThereIsNothingToFind. The sweep rides the drain
// ticker, so on a healthy plant it must do nothing at all — a backstop that
// creates a posting when there is no orphan is worse than no backstop.
func TestSweep_StaysQuietWhenThereIsNothingToFind(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	p := testPoster(t, store, &fakeTransport{})

	p.SweepOrphansOnce(context.Background())

	store.mu.Lock()
	n := len(store.postings)
	store.mu.Unlock()
	if n != 0 {
		t.Errorf("the sweep created %d postings with no orphans to find", n)
	}
}

// TestSweep_IsSilencedByTheMute. A credential fault means every send fails
// identically; sweeping then would keep manufacturing postings to fail against
// a credential nobody has fixed yet, which is the exact thing the mute exists
// to stop.
func TestSweep_IsSilencedByTheMute(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tr := &fakeTransport{results: []client.PostResult{
		{Class: client.ClassAuthFault, HTTPStatus: 401, Err: errors.New("denied")},
	}}
	p := testPoster(t, store, tr)
	enqueue(t, p, 1)
	p.DrainOnce(context.Background())
	if !p.Muted() {
		t.Fatal("setup: the poster did not mute")
	}

	store.orphan(41)
	p.SweepOrphansOnce(context.Background())

	store.mu.Lock()
	n := len(store.postings)
	store.mu.Unlock()
	if n != 1 {
		t.Errorf("%d postings, want 1 — the sweep ran while muted", n)
	}
}

// TestNextRetry_GrowsAndIsCapped// TestNextRetry_GrowsAndIsCapped. The cap is what makes MaxAttempts a bounded
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
