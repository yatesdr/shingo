package engine

import "shingo/protocol"

// SetCoreLoaders persists the Core-owned loader config to the durable Edge cache
// (full-state replace), refreshes the loader-store snapshot, and warms the
// threshold-replay gate. Called from the node-list-response handler alongside
// SetCoreNodes, so the cache — the loader resolvers' read source — rides every
// node-list sync.
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
	// The cache is now populated: warm the gate and replay any threshold signals that
	// arrived before this first sync (startup race — hold-and-replay). Idempotent.
	e.warmLoaderCacheAndReplay()
}
