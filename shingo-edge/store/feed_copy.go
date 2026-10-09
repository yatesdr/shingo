package store

import (
	"database/sql"
	"fmt"
	"time"
)

// feed_copy.go — what this Edge holds of each Core feed, across a restart.
//
// One row per Core feed key (protocol.FeedContainment, ...). The digest is kept
// only where the data is kept: containment carries its body here, the catalog's
// rows live in payload_catalog. The node list is memory-only and the scene
// revision lives with the geometry, so their rows keep only the two times;
// refusals are digested from their own table. The times are what the screens
// say "as of" with after a restart while Core is unreachable.

// FeedCopy is one feed_copy row. A zero time is stored as NULL.
type FeedCopy struct {
	Feed        string
	Digest      string
	Body        string
	ReceivedAt  time.Time
	ConfirmedAt time.Time
}

// ListFeedCopies reads every held feed row, for the engine to load at boot.
func (db *DB) ListFeedCopies() ([]FeedCopy, error) {
	rows, err := db.Query(`SELECT feed, digest, COALESCE(body, ''), COALESCE(received_at, ''), COALESCE(confirmed_at, '')
		FROM feed_copy`)
	if err != nil {
		return nil, fmt.Errorf("list feed copies: %w", err)
	}
	defer rows.Close()
	var out []FeedCopy
	for rows.Next() {
		var fc FeedCopy
		var received, confirmed string
		if err := rows.Scan(&fc.Feed, &fc.Digest, &fc.Body, &received, &confirmed); err != nil {
			return nil, fmt.Errorf("scan feed copy: %w", err)
		}
		fc.ReceivedAt = parseFeedTime(received)
		fc.ConfirmedAt = parseFeedTime(confirmed)
		out = append(out, fc)
	}
	return out, rows.Err()
}

// SaveFeedCopy writes one feed row whole. The engine calls it only when the
// digest changed or a confirmation is due to be persisted, never per heartbeat.
func (db *DB) SaveFeedCopy(fc FeedCopy) error {
	_, err := db.Exec(`
		INSERT INTO feed_copy (feed, digest, body, received_at, confirmed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(feed) DO UPDATE SET
			digest = excluded.digest, body = excluded.body,
			received_at = excluded.received_at, confirmed_at = excluded.confirmed_at`,
		fc.Feed, fc.Digest, nullIfEmpty(fc.Body), feedTime(fc.ReceivedAt), feedTime(fc.ConfirmedAt))
	if err != nil {
		return fmt.Errorf("save feed copy %s: %w", fc.Feed, err)
	}
	return nil
}

func feedTime(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: t.UTC().Format(time.RFC3339Nano), Valid: true}
}

func parseFeedTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func nullIfEmpty(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
