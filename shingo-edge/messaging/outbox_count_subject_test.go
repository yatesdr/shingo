package messaging

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/outbox"
	"shingo/protocol/testutil"
	"shingoedge/store"
)

// outbox_count_subject_test.go — the Edge's real outbox, driven by the real
// drainer, against a broker that refuses every publish.

// refusingPublisher fails every publish and counts the attempts.
type refusingPublisher struct {
	mu       sync.Mutex
	attempts int
}

func (p *refusingPublisher) Publish(string, []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts++
	return errors.New("broker unreachable")
}

func (p *refusingPublisher) IsConnected() bool { return true }

func (p *refusingPublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts
}

// listCountingStore is the production adapter with its ListPendingOutbox calls
// counted, so the statements a failed publish costs can be told apart from the
// one list every pass issues whether or not anything fails.
type listCountingStore struct {
	*edgeOutboxStore
	lists atomic.Int64
}

func (s *listCountingStore) ListPendingOutbox(limit int) ([]outbox.Message, error) {
	s.lists.Add(1)
	return s.edgeOutboxStore.ListPendingOutbox(limit)
}

// failedCountPublishes runs the real drainer over one count-subject row against
// a broker that refuses everything, for at least passes drain passes. It
// returns the publish attempts, and the statements beyond the per-pass list,
// i.e. what the failures themselves cost. The stop condition reads no database,
// so the test's own polling is not in the count.
func failedCountPublishes(t *testing.T, subject string, passes int64) (db *store.DB, attempts int, failureStatements int64) {
	t.Helper()
	db, counter, err := store.OpenCounting(filepath.Join(t.TempDir(), "outbox.db"))
	if err != nil {
		t.Fatalf("open counting db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.EnqueueOutbox([]byte(`{"count":1}`), subject); err != nil {
		t.Fatalf("enqueue %s: %v", subject, err)
	}

	pub := &refusingPublisher{}
	st := &listCountingStore{edgeOutboxStore: &edgeOutboxStore{db: db}}
	counter.Reset()
	d := outbox.NewDrainer(st, pub, "orders", time.Millisecond, DrainBatchSize)
	d.Start()
	testutil.Eventually(t, 5*time.Second, func() bool { return st.lists.Load() >= passes })
	d.Stop()
	return db, pub.count(), counter.Count() - st.lists.Load()
}

// TestCountRowRetriesPastTheBudget is S1 on the Edge's real outbox. A
// bin_uop_delta or lineside_bucket_delta row the broker refuses is offered on
// every pass, past MaxRetries, and stays pending rather than dead. A refusal
// costs no statement beyond the pass's own list: one fewer than before.
//
// Inverted pin: at base (TestPin_P0a_CountRowDeadLettersAfterTenFailures) the
// row died after exactly 10 refusals, each costing an IncrementOutboxRetries.
func TestCountRowRetriesPastTheBudget(t *testing.T) {
	t.Parallel()
	for _, subject := range []string{protocol.SubjectBinUOPDelta, protocol.SubjectLinesideBucketDelta} {
		db, attempts, failureStatements := failedCountPublishes(t, subject, 15)
		if attempts < 15 {
			t.Errorf("%s: %d publish attempts over at least 15 passes, want one per pass — "+
				"the row stopped being offered", subject, attempts)
		}
		if failureStatements != 0 {
			t.Errorf("%s: failures cost %d statements over %d attempts, want 0 — a refused count "+
				"publish writes nothing", subject, failureStatements, attempts)
		}
		pending, err := db.CountPendingOutbox()
		testutil.MustNoErr(t, err, "count pending")
		dead, err := db.CountDeadLetterOutbox()
		testutil.MustNoErr(t, err, "count dead letters")
		if pending != 1 || dead != 0 {
			t.Errorf("%s: pending=%d dead=%d, want 1 and 0 — a count row is never dead-lettered", subject, pending, dead)
		}
	}
}

// TestOrdinarySubjectStillDeadLetters is the control arm, green before and
// after S1: a subject other than the two counts keeps its retry budget, so a
// row the broker refuses 10 times is dead-lettered as it always was.
func TestOrdinarySubjectStillDeadLetters(t *testing.T) {
	t.Parallel()
	_, attempts, failureStatements := failedCountPublishes(t, protocol.SubjectProductionReport, 15)
	if attempts != 10 {
		t.Errorf("%d publish attempts, want exactly 10 (outbox.MaxRetries)", attempts)
	}
	if failureStatements != int64(attempts) {
		t.Errorf("failures cost %d statements over %d attempts, want one IncrementOutboxRetries each",
			failureStatements, attempts)
	}
}
