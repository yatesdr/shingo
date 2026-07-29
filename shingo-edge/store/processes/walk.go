// walk.go — the shared process→style→claim(→node) walk.
//
// Several engine and service read paths need to iterate the same tree —
// processes, their styles, each style's node claims, and (optionally) the
// process_nodes a claim resolves to — applying slightly different filters
// (active-style-only vs all-styles, by role, by swap mode, by core node,
// by payload). Before this helper each call site hand-rolled the walk with
// its own copy of the N+1-avoiding node fetch and its own filter chain
// (findManualSwapNodes, FindLoaderForPayload, FindUnloaderForPayload,
// FindAnyLoaderClaimForPayload, and the since-deleted SendClaimSync and
// loader-threshold binding list). WalkClaims is the single read path they
// share.
//
// It lives in store/processes (a free function over *sql.DB) rather than on
// *engine.Engine deliberately: engine imports service, so a method on the
// engine would be unreachable from service.BuildView (import cycle). The
// walk needs nothing from the engine — only the four process-aggregate
// reads below — so it belongs at the persistence layer where both engine
// and service can call it.

package processes

import (
	"database/sql"
	"fmt"
	"slices"

	"shingo/protocol"
)

// WalkCtx is the per-claim context passed to a WalkClaims visitor.
//
// Node is the zero Node unless WalkOpts.ResolveNode is set and the claim's
// CoreNodeName matches a process_node — in which case visit is called once
// per matching node. Active reports whether Style is the process's active
// style (always evaluated, regardless of WalkOpts.ActiveOnly).
type WalkCtx struct {
	Proc   Process
	Style  Style
	Claim  NodeClaim
	Node   Node
	Active bool
}

// WalkOpts filters the process→style→claim(→node) walk. The zero value
// walks every claim of every style of every process with no node
// resolution. Empty string / false fields mean "no filter on this axis".
type WalkOpts struct {
	ActiveOnly   bool               // only each process's active style
	Role         protocol.ClaimRole // "" = any role
	SwapMode     protocol.SwapMode  // "" = any swap mode
	CoreNodeName string             // "" = any core node
	PayloadCode  string             // "" = any payload; else claim must allow it
	ResolveNode  bool               // populate WalkCtx.Node from process_nodes
}

// WalkClaims iterates style_node_claims across processes and styles,
// applying opts as filters, and calls visit for each matching claim (or,
// when opts.ResolveNode is set, once per matching process_node). visit
// returns stop=true to end the walk early — the contract first-match
// callers rely on.
//
// Iteration order is deterministic and matches the hand-rolled walks this
// replaces: processes in List() order, styles in ListStylesByProcess()
// order, claims in ListClaims() order, nodes in ListNodesByProcess() order.
// FindLoaderForPayload's "first match" routing depends on this ordering, so
// it must not change.
//
// DB errors are wrapped and returned (never logged here) so each caller
// picks its own logging/fail-open-vs-closed discipline. Node resolution
// fetches process_nodes once per process, not once per claim.
func WalkClaims(db *sql.DB, opts WalkOpts, visit func(WalkCtx) (stop bool)) error {
	procs, err := List(db)
	if err != nil {
		return fmt.Errorf("walk claims: list processes: %w", err)
	}
	for _, proc := range procs {
		styles, err := ListStylesByProcess(db, proc.ID)
		if err != nil {
			return fmt.Errorf("walk claims: list styles for process %d: %w", proc.ID, err)
		}

		// process_nodes are fetched lazily, once per process, and only
		// when a matching claim actually needs node resolution.
		var nodes []Node
		var nodesFetched bool

		for _, st := range styles {
			active := proc.ActiveStyleID != nil && st.ID == *proc.ActiveStyleID
			if opts.ActiveOnly && !active {
				continue
			}
			claims, err := ListClaims(db, st.ID)
			if err != nil {
				return fmt.Errorf("walk claims: list claims for style %d: %w", st.ID, err)
			}
			for _, claim := range claims {
				if opts.SwapMode != "" && claim.SwapMode != opts.SwapMode {
					continue
				}
				if opts.Role != "" && claim.Role != opts.Role {
					continue
				}
				if opts.CoreNodeName != "" && claim.CoreNodeName != opts.CoreNodeName {
					continue
				}
				if opts.PayloadCode != "" && !slices.Contains(claim.AllowedPayloads(), opts.PayloadCode) {
					continue
				}
				ctx := WalkCtx{Proc: proc, Style: st, Claim: claim, Active: active}
				if !opts.ResolveNode {
					if visit(ctx) {
						return nil
					}
					continue
				}
				if !nodesFetched {
					nodes, err = ListNodesByProcess(db, proc.ID)
					if err != nil {
						return fmt.Errorf("walk claims: list nodes for process %d: %w", proc.ID, err)
					}
					nodesFetched = true
				}
				// One visit per process_node whose name matches the claim
				// (mirrors findManualSwapNodes, which appended one result per
				// (claim, node) pair). A claim whose CoreNodeName has no
				// matching process_node is skipped — the legacy behaviour.
				for _, node := range nodes {
					if node.CoreNodeName != claim.CoreNodeName {
						continue
					}
					ctx.Node = node
					if visit(ctx) {
						return nil
					}
				}
			}
		}
	}
	return nil
}

// PayloadsForLoader returns the active-style and all-style payload unions for
// the manual_swap claim(s) at coreNodeName with the given role, plus the
// active-style OutboundDestination (active-wins). Its remaining consumer is
// BuildView (service); it also served SendClaimSync until that was deleted
// 2026-07-21. It lives in store/processes rather than on *engine.Engine
// because engine imports service, so a method there would be unreachable from
// BuildView.
//
//   - active: payloads from claims on a process's ACTIVE style only.
//   - all:    payloads from claims on any style (active or not).
//
// Both are sorted for a deterministic wire/HMI shape. A loader with no
// matching claim returns empty slices and "".
func PayloadsForLoader(db *sql.DB, coreNodeName string, role protocol.ClaimRole) (active, all []string, outboundDest string, err error) {
	activeSet := map[string]bool{}
	allSet := map[string]bool{}
	activeKnown := false
	err = WalkClaims(db, WalkOpts{
		Role:         role,
		SwapMode:     protocol.SwapModeManualSwap,
		CoreNodeName: coreNodeName,
	}, func(ctx WalkCtx) bool {
		for _, p := range ctx.Claim.AllowedPayloads() {
			allSet[p] = true
			if ctx.Active {
				activeSet[p] = true
			}
		}
		// Active-style claim wins for OutboundDestination, first one seen.
		if ctx.Active && !activeKnown {
			outboundDest = ctx.Claim.OutboundDestination
			activeKnown = true
		}
		return false
	})
	if err != nil {
		return nil, nil, "", err
	}
	return sortedKeys(activeSet), sortedKeys(allSet), outboundDest, nil
}

// PayloadSet is the active-style and all-style payload union for one loader
// position, both sorted (deterministic HMI shape).
type PayloadSet struct {
	Active []string
	All    []string
}

// PayloadsForManualSwapNodes walks the process/style/claim tree ONCE and returns
// the active/all payload unions for EVERY manual_swap claim, keyed by core node
// name then role. It is the batched form of PayloadsForLoader: BuildView needs
// these sets for each loader tile, and calling PayloadsForLoader per tile made an
// N-home board do N full walks — each walk runs a ListClaims query per style, so
// a station with many homes and many styles cost O(N × styles) queries (44s on the
// Springfield bin loader with 14 homes). This does one walk — O(styles) — and the
// caller looks each node up in the map.
func PayloadsForManualSwapNodes(db *sql.DB) (map[string]map[protocol.ClaimRole]PayloadSet, error) {
	// TWO queries, not a walk. WalkClaims issues one ListStylesByProcess per
	// process and one ListClaims per style — ~53 queries on a 12-process /
	// ~40-style plant — and BuildView calls this once per board build. With five
	// boards polling that was several thousand queries a minute against the one
	// connection store.Open allows, which made this the largest single query
	// consumer in the build once the per-tile work was batched.
	//
	// Filtering and grouping happen in Go rather than in SQL on purpose: it
	// reuses claimSelect and scanNodeClaim verbatim, so there is no risk of a
	// hand-written column list drifting from the scan order, and no question
	// about how a swap_mode constant compares in the DB. Claims are per
	// (style, node), so the row count is small enough that reading them all is
	// cheaper than the queries it replaces.
	//
	// activeByStyle is the only thing the walk provided that a flat claim read
	// does not. The INNER join reproduces the walk's reachable set exactly: a
	// style whose process_id does not resolve was never visited, because the walk
	// started from List(processes).
	//
	// RETIRED styles are excluded, and that is load-bearing rather than tidy.
	// This set decides which payloads the manual-swap board offers the operator.
	// Without the predicate, retiring a style would leave its payloads on a live
	// loader window — soft delete would introduce a board regression that the
	// old hard delete did not have.
	activeByStyle := map[int64]bool{}
	styleRows, err := db.Query(`SELECT s.id,
		CASE WHEN p.active_style_id IS NOT NULL AND p.active_style_id = s.id THEN 1 ELSE 0 END
		FROM styles s JOIN processes p ON p.id = s.process_id
		WHERE s.deleted_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("manual-swap payloads: active styles: %w", err)
	}
	defer styleRows.Close()
	for styleRows.Next() {
		var id int64
		var isActive int
		if err := styleRows.Scan(&id, &isActive); err != nil {
			return nil, fmt.Errorf("manual-swap payloads: scan style: %w", err)
		}
		activeByStyle[id] = isActive != 0
	}
	if err := styleRows.Err(); err != nil {
		return nil, fmt.Errorf("manual-swap payloads: active styles: %w", err)
	}

	claimRows, err := db.Query(`SELECT ` + claimSelect + ` FROM style_node_claims`)
	if err != nil {
		return nil, fmt.Errorf("manual-swap payloads: claims: %w", err)
	}
	defer claimRows.Close()

	type acc struct{ active, all map[string]bool }
	buckets := map[string]map[protocol.ClaimRole]*acc{}
	for claimRows.Next() {
		claim, err := scanNodeClaim(claimRows)
		if err != nil {
			return nil, fmt.Errorf("manual-swap payloads: scan claim: %w", err)
		}
		if claim.SwapMode != protocol.SwapModeManualSwap {
			continue
		}
		// A claim on a style with no resolvable process was unreachable from the
		// walk; skip it rather than silently treating it as inactive.
		isActive, reachable := activeByStyle[claim.StyleID]
		if !reachable {
			continue
		}
		name := claim.CoreNodeName
		role := claim.Role
		if buckets[name] == nil {
			buckets[name] = map[protocol.ClaimRole]*acc{}
		}
		a := buckets[name][role]
		if a == nil {
			a = &acc{active: map[string]bool{}, all: map[string]bool{}}
			buckets[name][role] = a
		}
		for _, p := range claim.AllowedPayloads() {
			a.all[p] = true
			if isActive {
				a.active[p] = true
			}
		}
	}
	if err := claimRows.Err(); err != nil {
		return nil, fmt.Errorf("manual-swap payloads: claims: %w", err)
	}
	out := make(map[string]map[protocol.ClaimRole]PayloadSet, len(buckets))
	for name, roles := range buckets {
		out[name] = make(map[protocol.ClaimRole]PayloadSet, len(roles))
		for role, a := range roles {
			out[name][role] = PayloadSet{Active: sortedKeys(a.active), All: sortedKeys(a.all)}
		}
	}
	return out, nil
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
