// Package middleware holds router middlewares that gate or wrap
// inbound dispatch. Phase 3.4 collapses the InboxDedup decorator (172
// lines of MessageHandler method overrides) into a single ~30-line
// router middleware function — the load-bearing payoff of the
// protocol/router cutover.
package middleware

import (
	"log"

	"shingo/protocol"
	"shingo/protocol/router"
	"shingocore/store"
)

// NewInboxDedup returns a router.Middleware that gates dispatch on the
// inbox table. A row means the message was handled to the end: it is
// written after the handler returns (docket B11). A run cut short by a
// crash or a kill leaves no row, so the redelivery Kafka makes of a
// message whose offset never moved is handled again, and each gated
// handler is safe to run again (dispatch/run_twice_docker_test.go). Until
// 2026-11-25 the row was written before the handler and a cut run's
// redelivery was dropped: the message was lost.
//
//   - nil envelope or empty ID: forward (no inbox row to write).
//   - the row cannot be read: drop and log, as a failed record always
//     dropped. Forwarding would run a handled message again on every
//     failed read.
//   - a row: drop silently; dbg-log the replay if a logger was provided.
//   - no row: forward, then write the row. A failed write is logged; the
//     message was handled, and a repeat of it is handled again.
//
// A handler that panics writes no row; the read loop still commits its
// offset (messaging.Client.readLoop), so only a resend of the same
// envelope reaches it again.
//
// ONE READER A TOPIC is this design's precondition, and B18's (the delta
// apply reads its cursor before the bin lock, uop readBinScope). A read
// before the handler and a row after it keep a message to one run only
// while no two goroutines handle one envelope at once. Core reads the orders
// topic on one reader, which runs each message's handler to its end before
// it fetches the next (messaging.Client.readLoop), so a repeat comes after
// the first run wrote its row or was cut; two readers given one envelope
// would both read no row and both run its handler. Pinned by
// TestCoreReadsTheOrdersTopicOnOneReader (engine). A second reader of the
// orders topic should first make this middleware claim the envelope before
// its handler. The one overlap today: a re-configure whose Close gave up on
// a handler at its limit starts the new reader while that handler still
// runs (messaging.Client.Close).
//
// Compose with router.UseFor scoped to the order-channel envelope
// Types (TypeOrderRequest, TypeOrderCancel, TypeOrderReceipt,
// TypeOrderRedirect, TypeComplexOrderRequest, TypeOrderRelease,
// TypeOrderIngest — the Types CoreHandler's dedup decorator gated
// pre-migration). TypeData and the reply-channel Types pass through
// ungated, matching pre-decorator behaviour.
//
// dbg is optional; pass nil to silence the duplicate-ignored log line.
func NewInboxDedup(db *store.DB, dbg func(string, ...any)) router.Middleware {
	return func(env *protocol.Envelope, _ any, next func()) {
		if env == nil || env.ID == "" {
			next()
			return
		}
		seen, err := db.InboundSeen(env.ID)
		if err != nil {
			log.Printf("inbox_dedup: read %s: %v", env.ID, err)
			return
		}
		if seen {
			if dbg != nil {
				dbg("duplicate inbound ignored: id=%s type=%s from=%s", env.ID, env.Type, env.Src.Station)
			}
			return
		}
		next()
		if _, err := db.RecordInboundMessage(env.ID, env.Type); err != nil {
			log.Printf("inbox_dedup: record %s after its handler: %v (a repeat of it is handled again)", env.ID, err)
		}
	}
}
