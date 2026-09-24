package store

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// AllocateInventoryDeltaSeq returns the next monotonically-increasing
// SequenceID for an inventory-delta scope, and the scope's running net after
// adding netAdd to it.
//
// scopeKind ∈ {"bin", "bucket"}; scopeKey is the same stable string Core's
// dedup table uses (strconv(BinID) for bins; "<core_node_name>|<pair>|<style>|
// <payload>" for buckets); epoch labels the bin's load-lifecycle for bins (0
// for buckets — see ApplyLinesideBucketDelta on Core).
//
// PK is (scope_kind, scope_key, epoch). Per-epoch rows mean a new bin load
// (epoch bump on Core) starts the seq stream at 1 and the net at 0.
//
// THE NET RIDES THE SEQ STATEMENT. net is the signed sum of every delta this
// Edge has flushed for the scope; the flush puts it on the message, and Core
// applies it minus what it has already applied, so a message lost, reordered or
// muted by Core's high-water mark is healed by the next one (SYNTH-round2 §3).
// Adding it here, in the UPSERT that already returns the seq, is what keeps the
// running total free on the Pi: still one statement per flushed scope.
//
// The caller passes only what this row has not already absorbed. A flush whose
// outbox INSERT fails after this statement succeeded has burned the seq and
// added its delta to net; the accumulator remembers that and adds only the new
// part on the retry (see accumulator.flushBins).
//
// Survives Edge restarts — the row is durable. Old-epoch rows linger
// after a load-lifecycle bump (cheap: a handful of bytes each), no
// retention sweep required.
func (db *DB) AllocateInventoryDeltaSeq(scopeKind, scopeKey string, epoch, netAdd int64) (seq, net int64, err error) {
	err = db.QueryRow(`
		INSERT INTO inventory_delta_seq (scope_kind, scope_key, epoch, next_seq, net, updated_at)
		VALUES (?, ?, ?, 1, ?, datetime('now'))
		ON CONFLICT (scope_kind, scope_key, epoch)
		DO UPDATE SET next_seq = next_seq + 1, net = net + excluded.net, updated_at = datetime('now')
		RETURNING next_seq, net`,
		scopeKind, scopeKey, epoch, netAdd,
	).Scan(&seq, &net)
	if err != nil {
		return 0, 0, fmt.Errorf("allocate inventory_delta_seq scope=%s/%s epoch=%d: %w",
			scopeKind, scopeKey, epoch, err)
	}
	return seq, net, nil
}

// rekeyBucketDeltaSeq moves bucket seq rows from the key the Edge used to
// allocate under, "<process_nodes.id>|<pair>|<style>|<payload>", to the key
// Core's dedup row has always had, "<core_node_name>|<pair>|<style>|<payload>"
// (SYNTH-round2 S4). Before this, two process nodes carrying one
// core_node_name numbered two seq streams into one Core high-water row, and
// Core muted the lower one.
//
// COLLISIONS ARE MERGED, DETERMINISTICALLY. Every row that maps to one new key
// (including a row already under the new key) becomes one row with
// MAX(next_seq) — above every seq Core applied from any of them, so the merged
// stream is never muted — and SUM(net). An old-key row's net is always 0 (no
// build that writes the old key writes a net), so SUM(net) is the new-key
// row's net when there is one, which is the value Core anchored to, and 0 when
// there is not, which Core's NULL-anchor rule expects.
//
// Left in place, and inert: a row whose leading field is not an integer (already
// re-keyed), names no process node, or names one with a blank core_node_name.
// No flush allocates under a numeric key any more, so nothing reads them. The
// one ambiguity is a core_node_name that is itself an integer equal to some
// process node's id; no plant names a node that way.
//
// Idempotent, one transaction. Runs at every open; after the first it finds no
// numeric keys that resolve and changes nothing.
func (db *DB) rekeyBucketDeltaSeq() error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("rekey bucket seq: begin: %w", err)
	}
	defer tx.Rollback()

	names, err := coreNodeNamesByID(tx)
	if err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT scope_key, epoch, next_seq, net FROM inventory_delta_seq WHERE scope_kind = 'bucket'`)
	if err != nil {
		return fmt.Errorf("rekey bucket seq: read: %w", err)
	}
	type seqRow struct {
		key            string
		epoch, seq, nt int64
	}
	var all []seqRow
	for rows.Next() {
		var r seqRow
		if err := rows.Scan(&r.key, &r.epoch, &r.seq, &r.nt); err != nil {
			rows.Close()
			return fmt.Errorf("rekey bucket seq: scan: %w", err)
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rekey bucket seq: rows: %w", err)
	}

	type target struct {
		key   string
		epoch int64
	}
	moved := map[target][]seqRow{}
	for _, r := range all {
		lead, rest, ok := strings.Cut(r.key, "|")
		if !ok {
			continue
		}
		id, err := strconv.ParseInt(lead, 10, 64)
		if err != nil {
			continue
		}
		name := names[id]
		if name == "" {
			continue
		}
		t := target{name + "|" + rest, r.epoch}
		moved[t] = append(moved[t], r)
	}
	if len(moved) == 0 {
		return nil
	}
	for t, olds := range moved {
		maxSeq, sumNet := int64(0), int64(0)
		for _, r := range all {
			if r.key == t.key && r.epoch == t.epoch {
				maxSeq, sumNet = r.seq, r.nt
			}
		}
		for _, r := range olds {
			if r.seq > maxSeq {
				maxSeq = r.seq
			}
			sumNet += r.nt
			if _, err := tx.Exec(`DELETE FROM inventory_delta_seq WHERE scope_kind = 'bucket' AND scope_key = ? AND epoch = ?`,
				r.key, r.epoch); err != nil {
				return fmt.Errorf("rekey bucket seq: delete %s: %w", r.key, err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO inventory_delta_seq (scope_kind, scope_key, epoch, next_seq, net, updated_at)
			VALUES ('bucket', ?, ?, ?, ?, datetime('now'))
			ON CONFLICT (scope_kind, scope_key, epoch)
			DO UPDATE SET next_seq = excluded.next_seq, net = excluded.net, updated_at = excluded.updated_at`,
			t.key, t.epoch, maxSeq, sumNet); err != nil {
			return fmt.Errorf("rekey bucket seq: write %s: %w", t.key, err)
		}
	}
	return tx.Commit()
}

// coreNodeNamesByID maps every process node, retired ones included, to its
// core_node_name. A retired node's seq rows still name it.
func coreNodeNamesByID(tx *sql.Tx) (map[int64]string, error) {
	rows, err := tx.Query(`SELECT id, core_node_name FROM process_nodes`)
	if err != nil {
		return nil, fmt.Errorf("rekey bucket seq: read process nodes: %w", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("rekey bucket seq: scan process node: %w", err)
		}
		out[id] = name
	}
	return out, rows.Err()
}
