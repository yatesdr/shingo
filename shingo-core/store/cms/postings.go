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
type Posting struct {
	ID            int64      `json:"id"`
	BatchKey      string     `json:"batch_key"`
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

const postingCols = `id, batch_key, body_sha, status, transaction_id, http_status, attempts,
	requeue_count, next_retry_at, last_error, created_at, inflight_at, posted_at, settled_at`

func scanPosting(row interface{ Scan(...any) error }) (*Posting, error) {
	var p Posting
	var httpStatus sql.NullInt64
	var nextRetry, inflightAt, postedAt, settledAt sql.NullTime
	err := row.Scan(&p.ID, &p.BatchKey, &p.BodySHA, &p.Status, &p.TransactionID,
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
	id, err := helpers.InsertID(db, `INSERT INTO cms_postings (batch_key, body_sha, status, next_retry_at)
		VALUES ($1, $2, $3, NOW()) RETURNING id`,
		p.BatchKey, p.BodySHA, StatusPending)
	if err != nil {
		return fmt.Errorf("create cms posting: %w", err)
	}
	p.ID = id
	p.Status = StatusPending
	return nil
}

// GetPosting returns one posting by id.
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
func MarkInflight(db *sql.DB, id int64, batchKey, bodySHA string) error {
	res, err := db.Exec(`UPDATE cms_postings
		SET status=$1, batch_key=$2, body_sha=$3, inflight_at=NOW(), attempts=attempts+1
		WHERE id=$4 AND status=$5`, StatusInflight, batchKey, bodySHA, id, StatusPending)
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

// ScheduleRetry records why a try failed and when to look again, WITHOUT
// touching attempts.
//
// MarkInflight is the one place an attempt is counted, because it is the one
// call that happens exactly once per send. Counting again here would make an
// after-send failure cost two attempts where a before-send failure costs one,
// and MaxAttempts would mean half as many tries on the path that needs them
// most.
func ScheduleRetry(db *sql.DB, id int64, lastErr string, nextRetry time.Time) error {
	_, err := db.Exec(`UPDATE cms_postings
		SET last_error=$1, next_retry_at=$2
		WHERE id=$3`, lastErr, nextRetry, id)
	if err != nil {
		return fmt.Errorf("schedule retry for cms posting %d: %w", id, err)
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
// counts separately, so a reconciler/poster loop is boundable on its own terms
// without shortening that budget.
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

	// Unposted is cms_transactions rows no posting has claimed. Nonzero and
	// growing means the subscriber is failing to enqueue — transactions are
	// being recorded and will never be sent, which no posting-status count
	// would reveal.
	Unposted int `json:"unposted_transaction_count"`
}

// PostingHealth reads the queue's state in one round trip.
func PostingHealth(db *sql.DB) (*Health, error) {
	var h Health
	var lastPosted sql.NullTime
	var oldestPending, oldestInflight sql.NullFloat64

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
			count(*) FILTER (WHERE status = 'inflight' AND transaction_id = '')
		FROM cms_postings`).Scan(
		&h.Pending, &h.Inflight, &h.Posted, &h.Rejected, &h.Failed,
		&h.PostedLastHour, &lastPosted, &oldestPending, &oldestInflight,
		&h.UnresolvableInflight)
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
