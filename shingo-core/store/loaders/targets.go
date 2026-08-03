package loaders

import (
	"sort"

	"shingo/shared/windoworder"
)

// targets.go — which nodes an inbound carrier for a loader may be delivered to,
// and how many may be inbound at once.
//
// This is the Core port of the Edge's domain.Loader.ReservationTarget
// (shingo-edge/domain/loader.go). It exists so Core can originate a loader
// replenishment without asking the Edge where to put it.
//
// PINNED TO THE EDGE, NOT TO WHAT LOOKS RIGHT. The two implementations are held
// together by shared golden vectors, and where the Edge does something
// surprising this reproduces the surprise rather than quietly improving on it.
// Two subsystems disagreeing about which window a carrier goes to is worse than
// both being consistently odd, and the divergence would only surface as a
// carrier arriving somewhere nobody expected. Each such case is called out
// below; a fix to any of them is a change to BOTH sides plus the vectors.

// Target is one delivery node for an inbound carrier.
type Target struct {
	// NodeName is the physical node an empty is delivered to. Names, not ids:
	// the Edge keys on names, and this answer has to be comparable to the Edge's.
	NodeName string
	// PayloadCode is the payload pinned at this node (dedicated positions only).
	// Empty for a shared window, whose payload set belongs to the loader.
	PayloadCode string
}

// DeliveryTargetsInput is everything DeliveryTargets needs, already read. It
// takes assembled config rather than a database handle so the same function
// serves the live path and the golden-vector tests without a Postgres in the
// loop — the vectors are the permanent gate and must be runnable anywhere.
type DeliveryTargetsInput struct {
	// Layout, FunnelWindows: the loader's own shape.
	Layout        string
	FunnelWindows bool
	// Homes as ListHomes returns them (sort_order, then position node id).
	Homes []Home
	// NodeNames resolves each home's PositionNodeID to its node name. A home
	// whose node is missing from this map is skipped, matching BuildLoaderInfos,
	// which drops a position whose node has vanished rather than failing the
	// whole sync.
	NodeNames map[int64]string
	// Payloads is the shared allowed set (shared_window only).
	Payloads []Payload
	// Member is the specific member node a triggering signal named, or "" for
	// none. Honoured for dedicated layouts only — see below.
	Member string
	// PayloadCode is the payload being replenished. "" means payload-agnostic,
	// which is a legal transitional state and matches any target.
	PayloadCode string
}

// DeliveryTargets returns the nodes an inbound carrier for this loader may be
// delivered to, and the BUDGET — how many carriers may be inbound across that
// whole set at once.
//
// Returns (nil, 0) when the loader has no target for the payload. That is a
// normal answer meaning "not this loader", not a failure.
//
// The three shapes:
//
//   - shared_window, spreading: every window, budget = window count. One carrier
//     per window.
//   - shared_window, funnelling: the first window only, budget 1.
//   - dedicated_positions: one independent one-bin position, budget 1. Positions
//     never share a budget, so the budget is 1 no matter how many there are.
func DeliveryTargets(in DeliveryTargetsInput) (targets []Target, budget int) {
	if in.Layout == LayoutSharedWindow {
		// A blank payload is the payload-agnostic case and matches; a named one
		// must be in the shared set.
		if in.PayloadCode != "" && !servesPayload(in.Payloads, in.PayloadCode) {
			return nil, 0
		}
		windows := sharedWindows(in)
		if len(windows) == 0 {
			// A shared loader with no windows configured yet (created in the admin
			// screen, members not dragged in) has nowhere to deliver. Fails closed,
			// matching the Edge constructor, which refuses to build such a loader
			// at all.
			return nil, 0
		}
		if in.FunnelWindows {
			return windows[:1], 1
		}
		return windows, len(windows)
	}

	// Dedicated: honour the member the signal named, so a payload loaded at two
	// positions goes to the position that actually reported low rather than
	// whichever one sorts first.
	if in.Member != "" {
		for _, h := range in.Homes {
			name, ok := in.NodeNames[h.PositionNodeID]
			if !ok || name != in.Member {
				continue
			}
			if in.PayloadCode == "" || h.PayloadCode == in.PayloadCode {
				return []Target{{NodeName: name, PayloadCode: h.PayloadCode}}, 1
			}
		}
	}
	// No member named, or it named nothing that serves this payload: first match.
	for _, h := range in.Homes {
		name, ok := in.NodeNames[h.PositionNodeID]
		if !ok {
			continue
		}
		if h.PayloadCode == in.PayloadCode {
			return []Target{{NodeName: name, PayloadCode: h.PayloadCode}}, 1
		}
	}
	return nil, 0
}

// sharedWindows lists a shared loader's windows in the order the Edge sees them.
//
// THE OPERATOR'S ARRANGEMENT DECIDES, then a number-aware name sort. The rule
// is shared/windoworder, imported by both sides, because the order IS the
// delivery decision: the funnel case delivers to "the first window" and
// spreading fills free windows in order, so if Core and the Edge order these
// differently a carrier goes somewhere nobody expected.
//
// This used to sort by name alone. Core stored the operator's window order,
// sent it down, and the Edge threw it away on arrival — no ordinal on the wire,
// no ordinal column in the cache, and a cache read that re-sorted by name. The
// arrangement was accepted, persisted, transmitted and ignored. Both halves of
// that are now carried, and the name sort is the fallback rather than the rule.
//
// The fallback is number-aware for a reason worth keeping: plain text order
// matches intent only while window names stay uniform. A plant names its
// windows W1, W2, W3 and everything looks right until the loader reaches ten,
// where plain text puts W10 before W2 and the funnel target moves without
// anybody touching it.
//
// STILL ODD, deliberately, and reproduced from the Edge because parity beats
// tidiness: BUFFER SLOTS COUNT AS WINDOWS. home_kind exists to separate a
// payload-pinned home from a kept-partial buffer, but nothing filters buffers
// out of a shared loader's windows on either side: BuildLoaderInfos emits every
// home as a position and the Edge turns every position into a window. So a
// buffer slot on a shared loader is a delivery target and a unit of budget. The
// filter, if one is wanted, belongs in BuildLoaderInfos where both sides would
// inherit it.
func sharedWindows(in DeliveryTargetsInput) []Target {
	type ordered struct {
		Target
		windoworder.Window
	}
	tmp := make([]ordered, 0, len(in.Homes))
	for _, h := range in.Homes {
		name, ok := in.NodeNames[h.PositionNodeID]
		if !ok {
			continue
		}
		// No PayloadCode: a shared window pins nothing; the loader's payload set
		// governs. Carrying the home's payload here would let a stray value on a
		// window row look like a pinned position.
		tmp = append(tmp, ordered{
			Target: Target{NodeName: name},
			Window: windoworder.Window{Ordinal: h.SortOrder, Name: name},
		})
	}
	sort.SliceStable(tmp, func(i, j int) bool { return windoworder.Less(tmp[i].Window, tmp[j].Window) })
	out := make([]Target, len(tmp))
	for i, t := range tmp {
		out[i] = t.Target
	}
	return out
}

func servesPayload(set []Payload, code string) bool {
	for _, p := range set {
		if p.PayloadCode == code {
			return true
		}
	}
	return false
}
