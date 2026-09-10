package engine

import (
	"shingo/protocol"
	"shingoedge/store"
)

// SetCoreLoaders persists the Core-owned loader config to the durable Edge cache
// (full-state replace), refreshes the loader-store snapshot, warms the
// threshold-replay gate, and reconciles the second authority away. Called from
// the node-list-response handler alongside SetCoreNodes, so the cache — the
// loader resolvers' read source — rides every node-list sync.
func (e *Engine) SetCoreLoaders(loaders []protocol.LoaderInfo) {
	if err := e.db.ReplaceCoreLoaders(loaders); err != nil {
		e.logFn("core_loaders: cache replace failed (%d loaders) — keeping last-known-good: %v", len(loaders), err)
		return
	}
	if len(loaders) > 0 {
		e.debugFn("core_loaders: cached %d loader(s) from node-list sync", len(loaders))
	}
	// Swap the aggregate LoaderStore's immutable snapshot to the freshly-cached
	// config so resolution reads current loaders without re-querying the cache.
	if s, ok := e.loaderStore.(*aggregateLoaderStore); ok {
		if err := s.Refresh(); err != nil {
			e.logFn("core_loaders: loader-store snapshot refresh failed — keeping last-known-good: %v", err)
		}
	}
	// AFTER the cache and the snapshot, never before. A node whose stored claim
	// moves must be resolvable through the aggregate on the very next read, and
	// the aggregate is only current once Refresh has run.
	e.reconcileLoaderClaims(loaders)
}

// reconcileLoaderClaims moves every stored manual_swap claim into the quarantine
// table, because Core owns loader configuration and a stored claim is a second
// authority for the same six facts SynthClaim serves. See
// store/claim_quarantine.go for the move, the predicate, and why the
// rows are archived rather than deleted.
//
// NEVER ON AN EMPTY LOADER SET. A short or empty loader list is a FAILED
// PROJECTION rather than a plant that retired its loaders: Core's
// BuildLoaderInfos fails the whole node list rather than ship one position short
// (store/loaders_sync.go), precisely so a read failure leaves the Edge on
// last-known-good. That guard should make an empty set unreachable here; this
// behaves as though it is not, because the cost of being wrong is a plant's
// operator-authored configuration and the cost of being careful is one sync.
//
// The refusal is logged only when it actually withheld something. A plant that
// simply has no loaders configured syncs an empty set on every heartbeat, and a
// line saying "declined to move the nothing that is there" on each one is noise
// that would bury the case this guard exists for.
func (e *Engine) reconcileLoaderClaims(loaders []protocol.LoaderInfo) {
	if len(loaders) == 0 {
		stored, err := e.db.CountLoaderClaims()
		if err != nil {
			e.logFn("core_loaders: empty loader set — claim reconcile skipped, and the stored-claim count could not be read: %v", err)
			return
		}
		if stored > 0 {
			e.logFn("core_loaders: empty loader set — %d stored manual_swap claim(s) LEFT IN PLACE. "+
				"An empty set is a failed loader projection, not a retired plant; nothing is quarantined until Core sends a real one.", stored)
		}
		return
	}

	keys := make([]string, 0, len(loaders))
	for _, l := range loaders {
		keys = append(keys, l.LoaderKey)
	}
	moved, err := e.db.QuarantineLoaderClaims(keys)
	if err != nil {
		e.logFn("core_loaders: claim reconcile failed — nothing moved: %v", err)
		return
	}
	// Silent when nothing moved, which is the steady state after the first sync:
	// the population is zero and stays zero, so a repeat sync writes nothing and
	// says nothing.
	for _, m := range moved {
		e.logFn("core_loaders: quarantined style_node_claims id=%d node=%s style=%d payload=%q — %s. "+
			"The row is in %s; restore it with INSERT ... SELECT if this was wrong.",
			m.ID, m.CoreNodeName, m.StyleID, m.PayloadCode,
			store.ClaimQuarantineReason, store.ClaimQuarantineTable)
	}
}
