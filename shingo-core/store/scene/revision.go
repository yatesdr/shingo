package scene

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// Revision identifies one state of the scene tables, for the node-list sync's
// "send geometry only when it changed" short-circuit.
//
// A SHA-256 over (id, synced_at) of every point and every edge, in id order.
// synced_at is the change signal because every write path bumps it — Upsert
// sets it to NOW() and ReplaceArea deletes and re-inserts, which mints new ids
// as well — so a row that moved, appeared or vanished changes the digest, and
// a row that did not does not. Nothing that changes the geometry leaves both
// id and synced_at alone.
//
// WHY NOT max(synced_at) + count. It is nearly the same signal and it is
// cheaper to ask the database for, but it is not equivalent: a transaction
// that deletes one row and inserts another keeps the count, and if a
// concurrent commit landed after that transaction began, the new row's NOW()
// is EARLIER than the max already on the table and the max does not move.
// Unlikely, and exactly the kind of unlikely that stops a plant's map from
// updating with nothing to say why. The rows are already in memory for the
// name set, so hashing them costs no extra query; the marginal price of the
// stronger signal is zero.
//
// A FUNCTION OF THE ROWS ONLY. Two Cores answering the same scene — or one
// Core restarted — produce the same revision, so an Edge's cached revision is
// comparable across Core lifetimes. It must never depend on read order, the
// clock, or the process.
func Revision(points []*Point, edges []*Edge) string {
	type row struct {
		id int64
		ns int64
	}
	ps := make([]row, 0, len(points))
	for _, p := range points {
		ps = append(ps, row{p.ID, p.SyncedAt.UnixNano()})
	}
	es := make([]row, 0, len(edges))
	for _, e := range edges {
		es = append(es, row{e.ID, e.SyncedAt.UnixNano()})
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].id < ps[j].id })
	sort.Slice(es, func(i, j int) bool { return es[i].id < es[j].id })
	h := sha256.New()
	for _, r := range ps {
		fmt.Fprintf(h, "p:%d:%d\n", r.id, r.ns)
	}
	for _, r := range es {
		fmt.Fprintf(h, "e:%d:%d\n", r.id, r.ns)
	}
	return hex.EncodeToString(h.Sum(nil))
}
