package engine

// The aggregate (Core-owned) loader read path.
//
// The loader resolvers read Core's bin_loaders config from the synced
// core_loaders cache. Each cache loader is projected into the
// manualSwapNode{node, claim} shape the runtime consumes — the node IDENTITY
// comes from the present Edge process_node (GetProcessNodeByCoreNodeName), the
// CONFIG is synthesized from the cache.
//
// Mappings: role stays produce|consume; layout shared_window|dedicated_positions;
// replenishment auto|operator. A produce loader with replenishment=operator is
// "transitional" (auto-L1 suppressed); a consume loader with replenishment=auto
// is AutoPush. Per-payload min_stock → the synthetic claim's ReorderPoint, so
// refillLoaderForPayload reads the right value per payload even on a multi-payload
// shared_window loader.

import (
	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// cacheManualSwapNode resolves the Edge process_node for nodeName and pairs it
// with a NodeClaim synthesized from the cached loader for one payload. Returns
// nil if the node has no Edge process_node (topology not yet synced).
func (e *Engine) cacheManualSwapNode(nodeName string, l store.CoreLoader, payloadCode string, allowed []string, reorderPoint int) *manualSwapNode {
	node, err := e.db.GetProcessNodeByCoreNodeName(nodeName)
	if err != nil || node == nil {
		e.logFn("loader-aggregate:no process_node for %s (err=%v) — loader unresolved", nodeName, err)
		return nil
	}
	m := manualSwapNode{
		node: *node,
		claim: processes.NodeClaim{
			CoreNodeName:        node.CoreNodeName,
			Role:                protocol.ClaimRole(l.Role),
			SwapMode:            protocol.SwapModeManualSwap,
			PayloadCode:         payloadCode,
			AllowedPayloadCodes: allowed,
			ReorderPoint:        reorderPoint,
			InboundSource:       l.InboundSource,
			OutboundDestination: l.OutboundDest,
			AutoPush:            l.Role == "consume" && l.Replenishment == "auto",
			AutoConfirm:         true,
		},
	}
	return &m
}

// loaderAllowedAndMin returns a shared_window loader's full allowed payload set
// plus the min_stock of payloadCode (and whether it's in the set).
func loaderAllowedAndMin(l store.CoreLoader, payloadCode string) (allowed []string, minStock int, ok bool) {
	for _, p := range l.Payloads {
		allowed = append(allowed, p.PayloadCode)
		if p.PayloadCode == payloadCode {
			minStock, ok = p.MinStock, true
		}
	}
	return allowed, minStock, ok
}

// resolveCoreLoaderForPayload finds the cached loader of the given role serving
// payloadCode and projects it. dedicated_positions matches a position (one payload
// per position → its node); shared_window matches the allowed set (→ its first window
// node). coreNodeName, when non-empty, scopes to a loader containing that member node.
func (e *Engine) resolveCoreLoaderForPayload(coreNodeName, payloadCode, role string) *manualSwapNode {
	loaders, err := e.db.ListCoreLoaders()
	if err != nil {
		e.logFn("loader-aggregate:list cache: %v", err)
		return nil
	}
	for _, l := range loaders {
		if l.Role != role {
			continue
		}
		for _, pos := range l.Positions {
			if pos.PayloadCode != payloadCode {
				continue
			}
			if coreNodeName != "" && pos.PositionNode != coreNodeName {
				continue
			}
			return e.cacheManualSwapNode(pos.PositionNode, l, payloadCode, []string{payloadCode}, pos.MinStock)
		}
		if coreNodeName != "" && !loaderContainsNode(l, coreNodeName) {
			continue
		}
		if allowed, minStock, ok := loaderAllowedAndMin(l, payloadCode); ok {
			node := sharedLoaderMemberNode(l)
			if node == "" {
				continue
			}
			return e.cacheManualSwapNode(node, l, payloadCode, allowed, minStock)
		}
	}
	return nil
}

// manualSwapNodesFromCore projects every cached loader (optionally scoped to
// coreNodeName) into one manualSwapNode per loader — the aggregate twin of
// findManualSwapNodes. The push/board paths that consume this iterate
// claim.AllowedPayloads() and check role/AutoPush/transitional; none read a
// per-payload min_stock here (the sweep resolves per-payload separately), so the
// loader-level claim suffices.
func (e *Engine) manualSwapNodesFromCore(coreNodeName string) []manualSwapNode {
	loaders, err := e.db.ListCoreLoaders()
	if err != nil {
		e.logFn("loader-aggregate:list cache: %v", err)
		return nil
	}
	var out []manualSwapNode
	for _, l := range loaders {
		nodes := loaderMemberNodes(l)
		for _, n := range nodes {
			if coreNodeName != "" && n.nodeName != coreNodeName {
				continue
			}
			if m := e.cacheManualSwapNode(n.nodeName, l, n.payload, n.allowed, n.minStock); m != nil {
				out = append(out, *m)
			}
		}
	}
	return out
}

// memberNode is one resolvable (node, payload set) of a cached loader.
type memberNode struct {
	nodeName string
	payload  string
	allowed  []string
	minStock int
}

// loaderMemberNodes lists a cached loader's resolvable member nodes, branching on
// the loader's LAYOUT — the single authoritative discriminator — NOT on
// "does it have positions" (the live bug C1 fixes):
//
//   - dedicated_positions → one independent member per position, each carrying
//     its own payload (an independent one-bin slot).
//   - shared_window → one member at the loader's FIRST WINDOW presenting the SHARED
//     payload set. The push/board enumeration acts on a single member; the reservation
//     seam spreads actual delivery across every window. (Pre-6b this used the loader's
//     borrowed anchor node; that node is gone, so the first real window stands in.)
//
// The old heuristic took the dedicated branch whenever len(Positions) > 0, so a
// shared loader that had window homes (possible since the grid editor shipped)
// emitted one member per window with allowed:[""] — a blank-payload "member" the
// push path would try to stage against. Branching on Layout removes that.
func loaderMemberNodes(l store.CoreLoader) []memberNode {
	switch l.Layout {
	case string(domain.LayoutDedicatedPositions):
		return dedicatedMembers(l)
	case string(domain.LayoutSharedWindow):
		return sharedMember(l)
	default:
		// Pre-layout / unknown Core: fall back to the prior heuristic so a loader
		// synced without a layout still resolves rather than vanishing.
		if len(l.Positions) > 0 {
			return dedicatedMembers(l)
		}
		return sharedMember(l)
	}
}

// dedicatedMembers projects each dedicated position into its own member node.
func dedicatedMembers(l store.CoreLoader) []memberNode {
	out := make([]memberNode, 0, len(l.Positions))
	for _, pos := range l.Positions {
		out = append(out, memberNode{nodeName: pos.PositionNode, payload: pos.PayloadCode, allowed: []string{pos.PayloadCode}, minStock: pos.MinStock})
	}
	return out
}

// sharedAllowedPayloads is a shared_window loader's full allowed payload set.
func sharedAllowedPayloads(l store.CoreLoader) []string {
	allowed := make([]string, 0, len(l.Payloads))
	for _, p := range l.Payloads {
		allowed = append(allowed, p.PayloadCode)
	}
	return allowed
}

// sharedMember projects a shared_window loader as ONE member at its first window node
// presenting the shared payload set — the push/board enumeration acts on a single
// member while the reservation seam spreads delivery across every window. Empty when
// the loader has no window yet (admin-created, not configured), so it resolves to
// nothing rather than to a borrowed node.
func sharedMember(l store.CoreLoader) []memberNode {
	node := sharedLoaderMemberNode(l)
	if node == "" {
		return nil
	}
	return []memberNode{{nodeName: node, payload: "", allowed: sharedAllowedPayloads(l), minStock: 0}}
}

// loaderContainsNode reports whether node is one of the cached loader's member nodes
// (a window or a dedicated position). Replaces the old core_node_name identity match
// now that a loader has no node of its own.
func loaderContainsNode(l store.CoreLoader, node string) bool {
	for _, p := range l.Positions {
		if p.PositionNode == node {
			return true
		}
	}
	return false
}

// sharedLoaderMemberNode returns a shared_window loader's first window node — a real
// node the cache-direct resolvers anchor a single-member projection on (the seam
// spreads actual delivery across every window). "" when the loader has no window yet.
func sharedLoaderMemberNode(l store.CoreLoader) string {
	if len(l.Positions) > 0 {
		return l.Positions[0].PositionNode
	}
	return ""
}

// loaderMinStockFromCore returns the per-payload min_stock for a cached loader at
// coreNodeName (the position node for dedicated, or a window for shared_window), and
// whether it was found.
func (e *Engine) loaderMinStockFromCore(coreNodeName, payloadCode string) (int, bool) {
	loaders, err := e.db.ListCoreLoaders()
	if err != nil {
		return 0, false
	}
	for _, l := range loaders {
		for _, pos := range l.Positions {
			if pos.PositionNode == coreNodeName && pos.PayloadCode == payloadCode {
				return pos.MinStock, true
			}
		}
		if loaderContainsNode(l, coreNodeName) {
			for _, p := range l.Payloads {
				if p.PayloadCode == payloadCode {
					return p.MinStock, true
				}
			}
		}
	}
	return 0, false
}

// isTransitionalFromCore reports whether the cached loader containing coreNodeName is
// operator-driven (replenishment=operator) — the aggregate twin of the
// transitional_loaders flag.
func (e *Engine) isTransitionalFromCore(coreNodeName string) bool {
	loaders, err := e.db.ListCoreLoaders()
	if err != nil {
		return false
	}
	for _, l := range loaders {
		if l.Replenishment == "operator" && loaderContainsNode(l, coreNodeName) {
			return true
		}
	}
	return false
}

// hasThresholdFromCore reports whether the cached (loader-node, payload) carries
// a UOP threshold > 0 — the aggregate twin of hasOptInLoaderThreshold.
func (e *Engine) hasThresholdFromCore(coreNodeName, payloadCode string) bool {
	loaders, err := e.db.ListCoreLoaders()
	if err != nil {
		return false
	}
	for _, l := range loaders {
		for _, pos := range l.Positions {
			if pos.PositionNode == coreNodeName && pos.PayloadCode == payloadCode {
				return pos.UOPThreshold > 0
			}
		}
		if loaderContainsNode(l, coreNodeName) {
			for _, p := range l.Payloads {
				if p.PayloadCode == payloadCode {
					return p.UOPThreshold > 0
				}
			}
		}
	}
	return false
}
