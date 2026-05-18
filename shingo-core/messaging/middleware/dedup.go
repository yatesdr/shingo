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
// inbox_messages table. Replicates the pre-router InboxDedup decorator's
// shouldProcess contract exactly:
//
//   - nil envelope or empty ID: forward (no inbox row to write).
//   - RecordInboundMessage error: drop and log. The alternative
//     (forward on error) would risk double-execution if the inbox write
//     later succeeds on retry.
//   - already-seen (insert returned not-inserted): drop silently;
//     dbg-log the replay if a logger was provided.
//   - newly recorded: forward.
//
// Compose with router.UseFor scoped to the 8 order-channel envelope
// Types (TypeOrderRequest, TypeOrderCancel, TypeOrderReceipt,
// TypeOrderRedirect, TypeOrderStorageWaybill, TypeComplexOrderRequest,
// TypeOrderRelease, TypeOrderIngest — the Types CoreHandler's dedup
// decorator gated pre-migration). TypeData and the reply-channel Types
// pass through ungated, matching pre-decorator behaviour.
//
// dbg is optional; pass nil to silence the duplicate-ignored log line.
func NewInboxDedup(db *store.DB, dbg func(string, ...any)) router.Middleware {
	return func(env *protocol.Envelope, _ any, next func()) {
		if env == nil || env.ID == "" {
			next()
			return
		}
		inserted, err := db.RecordInboundMessage(env.ID, env.Type, env.Src.Station)
		if err != nil {
			log.Printf("inbox_dedup: record %s: %v", env.ID, err)
			return
		}
		if !inserted {
			if dbg != nil {
				dbg("duplicate inbound ignored: id=%s type=%s from=%s", env.ID, env.Type, env.Src.Station)
			}
			return
		}
		next()
	}
}
