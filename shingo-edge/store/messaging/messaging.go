// Package messaging holds outbox-message persistence for shingo-edge.
// "Messaging" describes the responsibility — durable inter-process
// communication — rather than the implementation pattern (an outbox
// table). Matches core's `messaging/` sub-package naming.
//
// Phase 5b moved this CRUD out of the flat store/ package; Phase 6.0c
// renamed it from `outbox/` to `messaging/`. The outer store/ keeps
// a type alias (`store.OutboxMessage = messaging.Message`) and the
// `store.MaxOutboxRetries` constant alias so external callers see no
// API change.
package messaging

import (
	"database/sql"
	"fmt"
	"sync/atomic"
	"time"

	"shingo/protocol"
	"shingo/protocol/outbox"
	"shingoedge/store/internal/helpers"
)

// MaxRetries is the number of delivery attempts before a message is
// considered dead-lettered and skipped by the drainer.
// The cap is protocol/outbox's — the drainer that enforces it lives there.
const MaxRetries = outbox.MaxRetries

// Message is one outbox row.
type Message struct {
	ID        int64      `json:"id"`
	Payload   []byte     `json:"payload"`
	MsgType   string     `json:"msg_type"`
	Retries   int        `json:"retries"`
	CreatedAt time.Time  `json:"created_at"`
	SentAt    *time.Time `json:"sent_at"`
}

// Enqueue inserts a new outbound message and returns its row id.
func Enqueue(db *sql.DB, payload []byte, msgType string) (int64, error) {
	res, err := db.Exec(`INSERT INTO outbox (topic, payload, msg_type) VALUES ('orders', ?, ?)`, payload, msgType)
	if err != nil {
		return 0, err
	}
	notifyEnqueued()
	return res.LastInsertId()
}

// coalescableSubjects is the ONE list of message types a newer message may
// delete an older unsent one for. Guarded rather than documented, because the
// cost of getting it wrong is silent permanent data loss.
//
// A type belongs here only if every message of it is a COMPLETE snapshot, so
// that receiving only the newest loses nothing by construction:
//
//   - inventory.lineside_level_report carries every consuming node.
//   - plant.claims PublishAll carries every process, and Core replaces its
//     mirror per process on each message.
//
// Everything else must NOT be here, and the reasons differ:
// bin_uop_delta and lineside_bucket_delta are sequenced INCREMENTS — dropping
// one is a permanently wrong count, which is the whole reason they were given
// NoExpiry. production.tick and demand.origin are discrete events. Every
// order.* is operator intent that exists exactly once.
var coalescableSubjects = map[string]bool{
	protocol.SubjectLinesideLevelReport: true,
	protocol.SubjectPlantClaims:         true,
}

// EnqueueSnapshot replaces every unsent row of msgType with the given payloads,
// in one transaction.
//
// Why this exists: after an outage the edge holds an hour of superseded
// snapshots and publishes all of them in a burst on recovery. Core then
// processes an hour of history to arrive where the newest message alone would
// have put it — and before that, most of the burst is discarded at the ingestor
// for expiry anyway. Only the newest snapshot was ever worth sending.
//
// The DELETE deliberately includes DEAD-LETTERED rows of the type. A superseded
// snapshot that exhausted its retries is doubly worthless, and this is the only
// thing that clears one before the retention window.
//
// The transaction is load-bearing in one direction: a crash between the delete
// and the inserts must not leave the type with zero snapshots. Committing both
// together means the worst case is the previous snapshot surviving, never none.
//
// notifyEnqueued fires ONCE, after commit — the drainer reads every pending row
// per pass, so one doorbell covers the whole batch, and ringing it before commit
// would race the drainer against uncommitted rows.
func EnqueueSnapshot(db *sql.DB, payloads [][]byte, msgType string) error {
	if !coalescableSubjects[msgType] {
		return fmt.Errorf("EnqueueSnapshot: %q is not a coalescable subject — only "+
			"full-snapshot types may supersede their predecessors; sequenced deltas "+
			"and discrete events must use Enqueue", msgType)
	}
	if len(payloads) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("enqueue snapshot %s: begin: %w", msgType, err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM outbox WHERE sent_at IS NULL AND msg_type = ?`, msgType); err != nil {
		return fmt.Errorf("enqueue snapshot %s: supersede: %w", msgType, err)
	}
	for _, payload := range payloads {
		if _, err := tx.Exec(
			`INSERT INTO outbox (topic, payload, msg_type) VALUES ('orders', ?, ?)`,
			payload, msgType,
		); err != nil {
			return fmt.Errorf("enqueue snapshot %s: insert: %w", msgType, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("enqueue snapshot %s: commit: %w", msgType, err)
	}

	notifyEnqueued()
	return nil
}

// enqueueNotifier is the drainer's doorbell, set once at wiring time. It lives
// here rather than on DB because this is the only INSERT into outbox.
var enqueueNotifier atomic.Pointer[func()]

// SetEnqueueNotifier registers fn to run after each successful enqueue. Passing
// nil clears it. Wired in cmd/shingoedge to the drainer's Notify.
func SetEnqueueNotifier(fn func()) {
	if fn == nil {
		enqueueNotifier.Store(nil)
		return
	}
	enqueueNotifier.Store(&fn)
}

func notifyEnqueued() {
	if p := enqueueNotifier.Load(); p != nil {
		(*p)()
	}
}

// ListUnsentByType returns every un-sent outbox message matching one of
// the given msg_type values. Caller decodes the payload — store stays
// schema-agnostic. Used by InventoryDeltaReporter at startup to
// recover the in-memory pending set after a crash where deltas had
// been enqueued but not yet sent.
func ListUnsentByType(db *sql.DB, msgTypes []string) ([]Message, error) {
	if len(msgTypes) == 0 {
		return nil, nil
	}
	placeholders := ""
	args := make([]any, 0, len(msgTypes))
	for i, mt := range msgTypes {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, mt)
	}
	q := `SELECT id, payload, msg_type, retries, created_at FROM outbox WHERE sent_at IS NULL AND msg_type IN (` + placeholders + `) ORDER BY id`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []Message
	for rows.Next() {
		var m Message
		var createdAt string
		if err := rows.Scan(&m.ID, &m.Payload, &m.MsgType, &m.Retries, &createdAt); err != nil {
			return nil, err
		}
		m.CreatedAt = helpers.ScanTime(createdAt)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// ListPending returns the next batch of un-sent messages whose retry
// count is below MaxRetries.
func ListPending(db *sql.DB, limit int) ([]Message, error) {
	rows, err := db.Query(`SELECT id, payload, msg_type, retries, created_at FROM outbox WHERE sent_at IS NULL AND retries < ? ORDER BY id LIMIT ?`, MaxRetries, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []Message
	for rows.Next() {
		var m Message
		var createdAt string
		if err := rows.Scan(&m.ID, &m.Payload, &m.MsgType, &m.Retries, &createdAt); err != nil {
			return nil, err
		}
		m.CreatedAt = helpers.ScanTime(createdAt)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// ListDeadLetter returns un-sent messages that have hit MaxRetries.
func ListDeadLetter(db *sql.DB, limit int) ([]Message, error) {
	rows, err := db.Query(`SELECT id, payload, msg_type, retries, created_at FROM outbox WHERE sent_at IS NULL AND retries >= ? ORDER BY id LIMIT ?`, MaxRetries, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []Message
	for rows.Next() {
		var m Message
		var createdAt string
		if err := rows.Scan(&m.ID, &m.Payload, &m.MsgType, &m.Retries, &createdAt); err != nil {
			return nil, err
		}
		m.CreatedAt = helpers.ScanTime(createdAt)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// Ack marks a message as sent.
func Ack(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE outbox SET sent_at = datetime('now') WHERE id = ?`, id)
	return err
}

// IncrementRetries bumps the retry counter on a message.
func IncrementRetries(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE outbox SET retries = retries + 1 WHERE id = ?`, id)
	return err
}

// MarkExhausted forces a row into the implicit dead-letter state in
// a single UPDATE by setting retries to MaxRetries. The drainer's
// existing skip-when-exhausted behavior applies unchanged; PurgeOld
// cleans up on its normal cadence. reason is accepted to match the
// protocol/outbox Store interface but is not persisted — the schema
// doesn't carry a last_error column. Could be added if forensic
// need surfaces.
func MarkExhausted(db *sql.DB, id int64, reason string) error {
	_, err := db.Exec(`UPDATE outbox SET retries = ? WHERE id = ?`, MaxRetries, id)
	return err
}

// Requeue resets the retry counter so a dead-lettered message will be
// picked up by the drainer again.
func Requeue(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE outbox SET retries = 0 WHERE id = ? AND sent_at IS NULL`, id)
	return err
}

// PurgeOld deletes sent messages older than the given duration, and
// dead-lettered messages (retries >= MaxRetries) older than the given
// duration.
func PurgeOld(db *sql.DB, olderThan time.Duration) (int64, error) {
	// .UTC() is load-bearing: created_at defaults to datetime('now') and
	// sent_at is written as datetime('now'), both of which SQLite produces in
	// UTC, and the comparison is a string compare against that layout. A local
	// cutoff at a US-Central plant reads 5-6 hours older than it is, so rows
	// survive ~29-30h under a 24h retention. Core's twin documents the same
	// trap (shingo-core/store/messaging/messaging.go, PurgeOldOutbox).
	cutoff := time.Now().UTC().Add(-olderThan).Format(helpers.TimeLayout)
	res, err := db.Exec(`DELETE FROM outbox WHERE (sent_at IS NOT NULL AND sent_at < ?) OR (retries >= ? AND created_at < ?)`, cutoff, MaxRetries, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
