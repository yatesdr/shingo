package cms

import (
	"database/sql"
	"fmt"
	"time"

	"shingocore/store/internal/helpers"
)

// Posting is one POST to the middleware — one JSON array of transactions, one
// row, one lifecycle.
//
// THE STATUSES ARE NOT A PROGRESS BAR. They record what is known about whether
// the middleware has the data, and the distinction that matters is between
// "certainly not" and "possibly":
//
//	pending   nothing has been sent. Safe to send.
//	inflight  bytes went out and the answer is unknown. NOT safe to send again.
//	posted    the middleware acknowledged it and named a transaction id.
//	rejected  the middleware refused it. Sending it again would be refused again.
//	failed    out of attempts, or a credential fault. Needs a human.
//
// There is no idempotency key on this API, so `inflight` is the whole reason
// this table exists: a row in that state can only be resolved by ASKING the
// middleware whether it has the transaction, never by sending it again.
//
// WHAT THE RECONCILER CAN AND CANNOT RESOLVE, IN V1. Asking requires a
// transaction id, and only one after-send failure has one: a POST that returned
// 2xx and named an id, whose settling write then failed (see MarkTransactionID,
// which exists for that window). Every other way into inflight — a 2xx whose
// body carried no id we could parse, a 5xx, a timeout mid-response, a crash
// between the send and the acknowledgement — leaves nothing to ask about, and
// no automatic move from there is safe: re-sending may double-book and marking
// posted would invent a success. Those rows age, are counted as
// UnresolvableInflight, and wait for a person to check the middleware. That is
// a property of the API rather than an unfinished path — if the middleware ever
// dedupes on the x-body-sha256 the client already sends, the re-send becomes
// safe and the whole class resolves automatically.
type Posting struct {
	ID int64 `json:"id"`
	// BodySHA is the sha256 of the canonical JSON that was sent, written
	// BEFORE the POST so a crash mid-flight leaves a row saying exactly what
	// went out. It is also the dedup hint the client offers the middleware.
	BodySHA       string     `json:"body_sha"`
	Status        string     `json:"status"`
	TransactionID string     `json:"transaction_id"`
	HTTPStatus    *int       `json:"http_status,omitempty"`
	Attempts      int        `json:"attempts"`
	RequeueCount  int        `json:"requeue_count"`
	NextRetryAt   *time.Time `json:"next_retry_at,omitempty"`
	LastError     string     `json:"last_error"`
	CreatedAt     time.Time  `json:"created_at"`
	InflightAt    *time.Time `json:"inflight_at,omitempty"`
	PostedAt      *time.Time `json:"posted_at,omitempty"`
	SettledAt     *time.Time `json:"settled_at,omitempty"`
}

// Posting status values. Spelled once so a typo is a compile error rather than
// a row no query will ever match.
const (
	StatusPending  = "pending"
	StatusInflight = "inflight"
	StatusPosted   = "posted"
	StatusRejected = "rejected"
	StatusFailed   = "failed"
)

const postingCols = `id, body_sha, status, transaction_id, http_status, attempts,
	requeue_count, next_retry_at, last_error, created_at, inflight_at, posted_at, settled_at`

func scanPosting(row interface{ Scan(...any) error }) (*Posting, error) {
	var p Posting
	var httpStatus sql.NullInt64
	var nextRetry, inflightAt, postedAt, settledAt sql.NullTime
	err := row.Scan(&p.ID, &p.BodySHA, &p.Status, &p.TransactionID,
		&httpStatus, &p.Attempts, &p.RequeueCount, &nextRetry, &p.LastError,
		&p.CreatedAt, &inflightAt, &postedAt, &settledAt)
	if err != nil {
		return nil, err
	}
	if httpStatus.Valid {
		v := int(httpStatus.Int64)
		p.HTTPStatus = &v
	}
	for _, f := range []struct {
		src sql.NullTime
		dst **time.Time
	}{
		{nextRetry, &p.NextRetryAt},
		{inflightAt, &p.InflightAt},
		{postedAt, &p.PostedAt},
		{settledAt, &p.SettledAt},
	} {
		if f.src.Valid {
			t := f.src.Time
			*f.dst = &t
		}
	}
	return &p, nil
}

func scanPostings(rows *sql.Rows) ([]*Posting, error) {
	var out []*Posting
	for rows.Next() {
		p, err := scanPosting(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CreatePosting inserts a pending posting and sets p.ID on success.
//
// A new posting is always pending with next_retry_at = now: it has not been
// sent, and the drainer should pick it up on its next pass.
func CreatePosting(db *sql.DB, p *Posting) error {
	id, err := helpers.InsertID(db, `INSERT INTO cms_postings (body_sha, status, next_retry_at)
		VALUES ($1, $2, NOW()) RETURNING id`,
		p.BodySHA, StatusPending)
	if err != nil {
		return fmt.Errorf("create cms posting: %w", err)
	}
	p.ID = id
	p.Status = StatusPending
	return nil
}

// GetPosting returns one posting by id.
//
// No production path reads a posting by id — the drain and the reconciler both
// work from lists — so this exists for the tests that assert on a row after a
// transition. It is kept rather than inlined because the alternative is raw
// SQL over thirteen columns in an external test package, which would be a
// second place the column list is written down.
func GetPosting(db *sql.DB, id int64) (*Posting, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM cms_postings WHERE id=$1`, postingCols), id)
	return scanPosting(row)
}

// NextPending returns postings ready to send, oldest first.
//
// PENDING ONLY. An inflight row is one whose POST may already have landed, and
// this API has no idempotency key — handing one back here is how the same
// inventory transfer gets booked twice. Only the reconciler moves a row out of
// inflight, and only after asking the middleware whether it has the
// transaction. If this filter ever widens, that guarantee is gone.
func NextPending(db *sql.DB, limit int) ([]*Posting, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM cms_postings
		WHERE status = $1 AND (next_retry_at IS NULL OR next_retry_at <= NOW())
		ORDER BY id LIMIT $2`, postingCols), StatusPending, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPostings(rows)
}

// MarkInflight records what is about to be sent, and commits BEFORE the send.
//
// The order is the point. If the row still said "pending" while a POST was on
// the wire, a crash would leave it eligible for re-send with no way to know a
// copy had already gone. Writing first means a crash leaves a row that says
// "we tried, we do not know" — recoverable, because the reconciler can ask.
//
// Guarded on status='pending' so it cannot re-arm a row another pass took.
func MarkInflight(db *sql.DB, id int64, bodySHA string) error {
	res, err := db.Exec(`UPDATE cms_postings
		SET status=$1, body_sha=$2, inflight_at=NOW(), attempts=attempts+1
		WHERE id=$3 AND status=$4`, StatusInflight, bodySHA, id, StatusPending)
	if err != nil {
		return fmt.Errorf("mark cms posting %d inflight: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("cms posting %d is not pending", id)
	}
	return nil
}

// MarkPosted records the middleware's acknowledgement and its transaction id.
func MarkPosted(db *sql.DB, id int64, transactionID string, httpStatus int) error {
	_, err := db.Exec(`UPDATE cms_postings
		SET status=$1, transaction_id=$2, http_status=$3, posted_at=NOW(), settled_at=NOW(),
		    next_retry_at=NULL, last_error=''
		WHERE id=$4`, StatusPosted, transactionID, httpStatus, id)
	if err != nil {
		return fmt.Errorf("mark cms posting %d posted: %w", id, err)
	}
	return nil
}

// MarkTransactionID records the middleware's transaction id WITHOUT settling
// the row.
//
// It exists for one narrow window, and that window is the reconciler's only
// reachable entrance. MarkPosted writes the id and the status in one statement,
// so a failure of it used to lose the id and leave an inflight row with nothing
// to ask about — the single inflight state that has no automatic resolution at
// all. Writing the id first turns that same failure into a row the reconciler
// CAN ask about.
//
// Guarded on status='inflight' so it cannot stamp an id onto a row another pass
// has already settled.
func MarkTransactionID(db *sql.DB, id int64, transactionID string) error {
	res, err := db.Exec(`UPDATE cms_postings SET transaction_id=$1
		WHERE id=$2 AND status=$3`, transactionID, id, StatusInflight)
	if err != nil {
		return fmt.Errorf("record transaction id for cms posting %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("cms posting %d is not inflight", id)
	}
	return nil
}

// MarkRejected records a refusal. Terminal: the middleware did not like the
// body, and it will not like the same body later.
func MarkRejected(db *sql.DB, id int64, httpStatus int, lastErr string) error {
	_, err := db.Exec(`UPDATE cms_postings
		SET status=$1, http_status=$2, last_error=$3, settled_at=NOW(), next_retry_at=NULL
		WHERE id=$4`, StatusRejected, httpStatus, lastErr, id)
	if err != nil {
		return fmt.Errorf("mark cms posting %d rejected: %w", id, err)
	}
	return nil
}

// MarkFailed records a posting out of road — attempts exhausted, or a
// credential fault retrying cannot fix. Needs a human.
func MarkFailed(db *sql.DB, id int64, lastErr string) error {
	_, err := db.Exec(`UPDATE cms_postings
		SET status=$1, last_error=$2, settled_at=NOW(), next_retry_at=NULL
		WHERE id=$3`, StatusFailed, lastErr, id)
	if err != nil {
		return fmt.Errorf("mark cms posting %d failed: %w", id, err)
	}
	return nil
}

// MarkPending returns a posting to the send queue with a retry time.
//
// ONLY FOR FAILURES THAT HAPPENED BEFORE ANY BYTES WENT OUT — a DNS failure, a
// TLS handshake, a refused connection. Those cannot have reached the
// middleware, so re-sending cannot duplicate. A timeout mid-response is NOT
// this: it may have landed, and it stays inflight for the reconciler.
func MarkPending(db *sql.DB, id int64, lastErr string, nextRetry time.Time) error {
	_, err := db.Exec(`UPDATE cms_postings
		SET status=$1, last_error=$2, next_retry_at=$3, inflight_at=NULL
		WHERE id=$4`, StatusPending, lastErr, nextRetry, id)
	if err != nil {
		return fmt.Errorf("mark cms posting %d pending: %w", id, err)
	}
	return nil
}

// RecordAfterSendError records why a send failed, on a row that STAYS
// inflight. It touches nothing else.
//
// It does not schedule anything, and the name says so now: it used to also
// write next_retry_at, which was inert. NextPending reads that column only for
// PENDING rows, the reconciler keys on inflight_at, and RequeuePending
// overwrites it on the way out — so the value was written, never read, and
// implied a retry clock this row is not on. An inflight posting's only exit is
// the reconciler asking the middleware.
//
// attempts is deliberately untouched. MarkInflight is the one place an attempt
// is counted, because it is the one call that happens exactly once per send;
// counting again here would make an after-send failure cost two attempts where
// a before-send failure costs one, and MaxAttempts would mean half as many
// tries on the path that needs them most.
func RecordAfterSendError(db *sql.DB, id int64, lastErr string) error {
	_, err := db.Exec(`UPDATE cms_postings
		SET last_error=$1
		WHERE id=$2`, lastErr, id)
	if err != nil {
		return fmt.Errorf("record after-send error for cms posting %d: %w", id, err)
	}
	return nil
}

// ListInflightOlderThan returns inflight postings that have been inflight
// longer than d — the reconciler's input.
//
// The window is defensive: a POST that succeeded may not be queryable on the
// middleware side for a moment afterwards, and asking too early gets "no such
// transaction" about something that is about to exist.
func ListInflightOlderThan(db *sql.DB, d time.Duration) ([]*Posting, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM cms_postings
		WHERE status=$1 AND inflight_at IS NOT NULL AND inflight_at < NOW() - $2::interval
		ORDER BY id`, postingCols), StatusInflight, fmt.Sprintf("%d milliseconds", d.Milliseconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPostings(rows)
}

// RequeuePending flips an inflight posting back to pending. THE ONLY PATH OUT
// OF INFLIGHT THAT LEADS TO ANOTHER SEND, and it belongs to the reconciler
// alone — it is called after the middleware has said it does NOT have the
// transaction, which is the only evidence that re-sending is safe.
//
// attempts is preserved so the retry budget still bounds the row; requeue_count
// counts separately, so a reconciler/poster loop is bounded on its own terms
// without shortening that budget. The poster reads it against cms.max_requeues
// before calling this — see ReconcileOnce.
func RequeuePending(db *sql.DB, id int64) error {
	res, err := db.Exec(`UPDATE cms_postings
		SET status=$1, requeue_count=requeue_count+1, next_retry_at=NOW(),
		    inflight_at=NULL, transaction_id=''
		WHERE id=$2 AND status=$3`, StatusPending, id, StatusInflight)
	if err != nil {
		return fmt.Errorf("requeue cms posting %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("cms posting %d is not inflight", id)
	}
	return nil
}

// Health is the positive-evidence view of the posting queue.
//
// POSITIVE, NOT INFERRED. "Zero pending" is not health — it is equally the
// signature of a subsystem that stopped being handed anything, which is what a
// broken subscriber, an untagged plant, or a muted poster all look like. The
// fields that say things are WORKING are LastPostedAt and PostedLastHour;
// everything else is context for reading them.
type Health struct {
	Pending  int `json:"pending_count"`
	Inflight int `json:"inflight_count"`
	Posted   int `json:"posted_count"`
	Rejected int `json:"rejected_count"`
	Failed   int `json:"failed_count"`

	// PostedLastHour and LastPostedAt are the evidence that the pipe is open.
	PostedLastHour int        `json:"posted_last_hour"`
	LastPostedAt   *time.Time `json:"last_successful_post_at,omitempty"`

	// The ages are what turn a backlog into a finding. A pending row seconds
	// old is a working queue; one hours old is a stalled one, and the count
	// alone cannot tell them apart.
	OldestPendingAgeSeconds  int64 `json:"oldest_pending_age_seconds"`
	OldestInflightAgeSeconds int64 `json:"oldest_inflight_age_seconds"`

	// UnresolvableInflight are rows that went out and were never acknowledged
	// with an id. Nothing can resolve them automatically — re-sending might
	// double-book and marking them posted would invent a success — so they are
	// counted separately and are always a person's problem.
	UnresolvableInflight int `json:"unresolvable_inflight_count"`

	// Requeued counts postings the reconciler has returned to the queue at
	// least once. CONTEXT, NOT A VERDICT INPUT: a requeue is a normal recovery
	// and a bounded one, and a loop that reaches the bound becomes a `failed`
	// row, which the verdict already ranks. A second loud signal saying the
	// same thing in different words is how a health page becomes noise.
	Requeued int `json:"requeued_count"`

	// RejectedRecent and FailedRecent are the same rows as Rejected and
	// Failed, restricted to those that became terminal inside the health
	// window. THE VERDICT READS THESE; the page shows the lifetime totals.
	//
	// The split exists because a verdict built on the totals can never return
	// to green. One refusal in March would still be saying "attention" in
	// December, and a health surface that cannot go green stops being read —
	// which costs more than the silence it was built to prevent. The totals do
	// not disappear: they stay in the counts row, and the healthy sentence
	// names them so a parked failure is still visible without latching the
	// verdict on it.
	RejectedRecent int `json:"rejected_recent_count"`
	FailedRecent   int `json:"failed_recent_count"`

	// Unposted is cms_transactions rows no posting has claimed. Nonzero and
	// growing means the subscriber is failing to enqueue — transactions are
	// being recorded and will never be sent, which no posting-status count
	// would reveal.
	Unposted int `json:"unposted_transaction_count"`
}

// PostingHealth reads the queue's state in one round trip.
//
// window bounds RejectedRecent and FailedRecent — see those fields. A
// non-positive window is treated as "everything counts", which is what a
// misconfigured value should do here: the verdict gets louder, not quieter.
func PostingHealth(db *sql.DB, window time.Duration) (*Health, error) {
	var h Health
	var lastPosted sql.NullTime
	var oldestPending, oldestInflight sql.NullFloat64

	if window <= 0 {
		// A century, rather than a branch in the SQL. Fails loud.
		window = 100 * 365 * 24 * time.Hour
	}
	// settled_at, not created_at: the question is when the row became
	// terminal, which is when somebody would have had cause to look at it.
	cutoff := fmt.Sprintf("%d milliseconds", window.Milliseconds())

	err := db.QueryRow(`
		SELECT
			count(*) FILTER (WHERE status = 'pending'),
			count(*) FILTER (WHERE status = 'inflight'),
			count(*) FILTER (WHERE status = 'posted'),
			count(*) FILTER (WHERE status = 'rejected'),
			count(*) FILTER (WHERE status = 'failed'),
			count(*) FILTER (WHERE status = 'posted' AND posted_at > NOW() - INTERVAL '1 hour'),
			max(posted_at) FILTER (WHERE status = 'posted'),
			EXTRACT(EPOCH FROM (NOW() - min(created_at) FILTER (WHERE status = 'pending'))),
			EXTRACT(EPOCH FROM (NOW() - min(inflight_at) FILTER (WHERE status = 'inflight'))),
			count(*) FILTER (WHERE status = 'inflight' AND transaction_id = ''),
			count(*) FILTER (WHERE status = 'rejected'
				AND settled_at IS NOT NULL AND settled_at > NOW() - $1::interval),
			count(*) FILTER (WHERE status = 'failed'
				AND settled_at IS NOT NULL AND settled_at > NOW() - $1::interval),
			count(*) FILTER (WHERE requeue_count > 0)
		FROM cms_postings`, cutoff).Scan(
		&h.Pending, &h.Inflight, &h.Posted, &h.Rejected, &h.Failed,
		&h.PostedLastHour, &lastPosted, &oldestPending, &oldestInflight,
		&h.UnresolvableInflight, &h.RejectedRecent, &h.FailedRecent, &h.Requeued)
	if err != nil {
		return nil, fmt.Errorf("cms posting health: %w", err)
	}
	if lastPosted.Valid {
		t := lastPosted.Time
		h.LastPostedAt = &t
	}
	if oldestPending.Valid {
		h.OldestPendingAgeSeconds = int64(oldestPending.Float64)
	}
	if oldestInflight.Valid {
		h.OldestInflightAgeSeconds = int64(oldestInflight.Float64)
	}

	// Separate query: this one counts TRANSACTIONS, and a transaction with no
	// posting is invisible to every count above. It is the tell for a
	// subscriber that is failing to enqueue.
	if err := db.QueryRow(
		`SELECT count(*) FROM cms_transactions WHERE posting_id IS NULL`).Scan(&h.Unposted); err != nil {
		return nil, fmt.Errorf("cms unposted transactions: %w", err)
	}
	return &h, nil
}
