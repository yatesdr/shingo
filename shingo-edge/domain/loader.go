package domain

import (
	"fmt"
	"maps"
	"slices"
	"sort"

	"shingo/protocol"
)

// loader.go — the Edge runtime's first-class bin-loader aggregate (B1, the
// foundation of the multi-window refactor; see
// bin-loader-multiwindow-reviews-2026-06-12/FINAL-ADJUDICATION.md, checkpoint C0).
//
// A value type with constructor-enforced invariants and typed identifiers. It
// began as an additive shape with no runtime consumer; that is long past. The
// reservation seam counts against the cluster it describes, the resolver returns
// *Loader through a LoaderStore, and the older (node, claim) shim it replaced is
// gone. Anything describing this type as inert is out of date.
//
// Why it exists: today "what loader is this node part of, and what is its
// budget" is re-derived in five places (the resolver, the in-flight counter, the
// station view, the HMI renderer, the demand emitter) from a core_node_name plus
// scattered boolean predicates — and two of those derivations already disagree
// for a shared loader that has window homes. A single typed aggregate that owns
// its windows, its payload set, and its slot count makes multi-window fall out
// instead of being patched into a node-keyed world.

// ── Typed domain identifiers ────────────────────────────────────────
//
// Newtypes over string so the compiler rejects the A1 bug class — an empty count
// keyed by the wrong node string (process_node vs core_node; see
// [[shingo_manual_swap_core_node_scoping]]). Adopted on NEW surfaces only (this
// type, the reservation seam, the delivery-set query). Legacy string call sites
// convert at the boundary; this is deliberately NOT a repo-wide rename.

type (
	// LoaderID is the stable identity of a bin-loader aggregate. Today it is the
	// loader's anchor core_node_name; callers treat it opaquely so a future
	// switch to a UUID is invisible to them. Distinct from NodeID so a physical
	// node can never be passed where a loader identity is expected.
	LoaderID string

	// NodeID is a physical node's core_node_name — a shared loader's window, a
	// dedicated loader's position, or an anchor. The order delivery_node lives
	// in this space.
	NodeID string

	// PayloadCode identifies a payload (part) type. The empty value means
	// "unassigned" — a positive PositionKind, never the empty string, marks a
	// window (the convention C0 replaces).
	PayloadCode string
)

func (id LoaderID) String() string   { return string(id) }
func (n NodeID) String() string      { return string(n) }
func (p PayloadCode) String() string { return string(p) }

// ── Enums ───────────────────────────────────────────────────────────

// LoaderLayout is the structural shape of a loader. It is the single authoritative
// layout discriminator in code — never inferred from "does it have positions" or
// "is the payload empty" (the live bug this replaced in loaderMemberNodes). Values
// mirror the Core aggregate's loaders.Layout* constants and the wire.
type LoaderLayout string

const (
	LayoutSharedWindow       LoaderLayout = protocol.LoaderLayoutSharedWindow
	LayoutDedicatedPositions LoaderLayout = protocol.LoaderLayoutDedicatedPositions
)

// LoaderRole is produce (a bin loader: operator fills empties) or consume (an
// unloader: operator empties fulls). Mirrors protocol.ClaimRole's loader values.
type LoaderRole string

const (
	RoleProduce LoaderRole = LoaderRole(protocol.ClaimRoleProduce)
	RoleConsume LoaderRole = LoaderRole(protocol.ClaimRoleConsume)
)

func (r LoaderRole) valid() bool { return r == RoleProduce || r == RoleConsume }

// LoaderReplenishment is operator (the operator stages/clears at the board) or
// threshold (UOP kanban autoreorder — Core's threshold monitor fires the empties).
// A consume loader (unloader) is always operator: its single mode is the
// window-queue drain. Mirrors the Core aggregate's loaders.Replenishment* constants.
type LoaderReplenishment string

const (
	ReplenishmentOperator  LoaderReplenishment = protocol.LoaderReplenishmentOperator
	ReplenishmentThreshold LoaderReplenishment = protocol.LoaderReplenishmentThreshold
)

// PositionKind is the EXPLICIT marker that replaces the empty-payload-means-window
// convention. A window belongs to a shared_window loader's shared budget and
// carries no per-position payload; a dedicated position carries exactly one
// payload (which may be empty == not yet assigned by the operator). Empty payload
// alone is ambiguous between "window" and "unassigned dedicated position"; the
// kind disambiguates. On the wire and the Edge cache the kind is materialized
// (protocol.LoaderPosition.Kind, core_loader_positions.kind); in code the
// authoritative discriminator stays LoaderLayout, and kind is derived from it at
// the single Core projection point (BuildLoaderInfos).
type PositionKind string

const (
	PositionKindWindow    PositionKind = "window"
	PositionKindDedicated PositionKind = "dedicated"
)

// ── Member types ────────────────────────────────────────────────────

// Window is one load point of a shared_window loader. Every window presents the
// same shared demand; an empty may be delivered to ANY free window, and all
// windows draw on the loader's single budget. Currently one physical bin per
// window (the manualSwapWindowSlots=1 assumption B3 retires at the LOADER level,
// not the window level).
type Window struct {
	Node NodeID
}

// Position is one dedicated home of a dedicated_positions loader: one node bound
// to one payload with its own replenishment policy. Positions do NOT share a
// budget — each is an independent one-bin slot for a distinct payload.
type Position struct {
	Node    NodeID
	Payload PayloadCode // "" when the operator hasn't assigned a payload yet
	// HomeKind is Core's own classification of this position: HomeKindHome or
	// HomeKindBuffer. EMPTY MEANS UNKNOWN — a Core predating the field on the
	// wire — and callers must fall back to the blank-payload inference rather
	// than reading blank as "home"; an older Core sends blank for buffers too.
	//
	// It exists because Payload == "" answers two questions at once: a BUFFER
	// slot pins no payload, and so does a HOME the operator has dragged in but
	// not yet assigned. Core keeps them apart (bin_loader_homes.home_kind, read
	// by InSourcePool to leave an unassigned home out of the source pool); the
	// Edge could not, and classified an unpinned home as a buffer.
	HomeKind     string
	UOPThreshold int
}

// Position home kinds, mirroring Core's bin_loader_homes.home_kind. Blank is a
// third state — "this Core did not say" — and IsBuffer treats it as such.
const (
	HomeKindHome   = protocol.LoaderHomeKindHome
	HomeKindBuffer = protocol.LoaderHomeKindBuffer
)

// IsBuffer reports whether this position is a kept-partial BUFFER slot.
//
// Prefers Core's answer and falls back to the old inference only when Core did
// not give one. That fallback is the pre-field behaviour verbatim, so an Edge
// talking to an older Core behaves exactly as it did — and an Edge talking to a
// current Core stops calling an unassigned home a buffer.
func (p Position) IsBuffer() bool {
	switch p.HomeKind {
	case HomeKindBuffer:
		return true
	case HomeKindHome:
		return false
	default:
		return p.Payload == ""
	}
}

// ── The aggregate ───────────────────────────────────────────────────

// Loader is the Edge runtime's first-class bin-loader aggregate. Fields are
// unexported and every instance is built through a constructor, so an invalid
// loader is unconstructible rather than validated downstream:
//
//   - a shared layout with per-position payloads is unrepresentable — the
//     shared constructor takes a payload SET, not positions, so the type
//     signature alone forbids it (stronger than a runtime check);
//   - zero windows / zero positions / zero payloads, empty node ids, and empty
//     payloads in the shared set are rejected by the constructors;
//   - SlotCount is DERIVED, never passed, so a slot count below the member count
//     cannot be expressed.
//
// The reservation seam and the resolver hang off this type. Immutable
// after construction (no setters); slice accessors return copies so callers
// cannot mutate the aggregate's state.
type Loader struct {
	id            LoaderID
	name          string
	role          LoaderRole
	layout        LoaderLayout
	replenishment LoaderReplenishment
	windows       []Window      // shared_window only
	positions     []Position    // dedicated_positions only
	payloadSet    []PayloadCode // shared_window only — the shared allowed set
	slotCount     int           // total physical slots; the shared-window empty-in budget

	// Optional runtime config carried so the empty-in / completion paths need
	// neither the legacy claim nor a second lookup. Set via LoaderOption.
	inboundSource           string              // the empty market L1s source from
	outboundDest            string              // the market filled (L2) / emptied (U2) bins go to on completion
	uopThreshold            map[PayloadCode]int // shared_window per-payload UOP-threshold (C-push opt-in, display-read only on Edge); dedicated carries it on Position
	funnelWindows           bool                // shared_window only: take one window at a time instead of spreading (see FunnelWindows)
	changeoverLoadDirective bool                // a changeover commandeers this station's card (see ChangeoverLoadDirective)
}

// LoaderOption sets optional runtime config on a constructed Loader. Variadic, so
// existing call sites that don't carry this config compile unchanged.
type LoaderOption func(*Loader)

// WithInboundSource sets the empty market a loader's L1 retrieve_empty orders
// source from (claim.InboundSource / CoreLoader.InboundSource).
func WithInboundSource(src string) LoaderOption {
	return func(l *Loader) { l.inboundSource = src }
}

// WithOutboundDest sets the market a loader's FILLED bin (L2) / an unloader's
// EMPTIED bin (U2) is sent to on completion (claim.OutboundDestination /
// CoreLoader.OutboundDest). The completion handlers read this off the aggregate
// instead of the legacy claim once the legacy path is retired (step 5).
func WithOutboundDest(dst string) LoaderOption {
	return func(l *Loader) { l.outboundDest = dst }
}

// WithUOPThreshold sets the shared_window per-payload UOP threshold — carried
// down from the Core aggregate for display/config parity. Nothing on the Edge
// acts on it any more (the threshold receiver it fed is deleted; Core orders
// directly). Mirrors WithMinStock; dedicated loaders carry the threshold on
// each Position.
func WithUOPThreshold(m map[PayloadCode]int) LoaderOption {
	return func(l *Loader) {
		l.uopThreshold = make(map[PayloadCode]int, len(m))
		maps.Copy(l.uopThreshold, m)
	}
}

// WithFunnelWindows restricts a shared_window loader to ONE window at a time:
// empties funnel to its first window on a budget of 1 rather than spreading one
// bin per window (CoreLoader.FunnelWindows, owned by Core's bin_loaders row).
//
// Not passing the option leaves it false, which is SPREAD — the behaviour every
// loader has. The whole field is stated as the restriction for exactly that
// reason: a loader built without knowing about this setting behaves the way
// loaders behaved before the setting existed.
func WithFunnelWindows(funnel bool) LoaderOption {
	return func(l *Loader) { l.funnelWindows = funnel }
}

// NewSharedWindowLoader builds a shared_window loader: N windows presenting one
// shared payload set, drawing on one budget (= the window count). Returns an
// error rather than a half-built aggregate on any invariant violation.
func NewSharedWindowLoader(id LoaderID, name string, role LoaderRole, repl LoaderReplenishment, windows []Window, payloadSet []PayloadCode, opts ...LoaderOption) (*Loader, error) {
	if id == "" {
		return nil, fmt.Errorf("loader: empty id")
	}
	if !role.valid() {
		return nil, fmt.Errorf("loader %s: invalid role %q", id, role)
	}
	if len(windows) == 0 {
		return nil, fmt.Errorf("loader %s: shared_window needs at least one window", id)
	}
	if len(payloadSet) == 0 {
		return nil, fmt.Errorf("loader %s: shared_window needs at least one payload", id)
	}
	for i, w := range windows {
		if w.Node == "" {
			return nil, fmt.Errorf("loader %s: window %d has empty node", id, i)
		}
	}
	for i, p := range payloadSet {
		if p == "" {
			return nil, fmt.Errorf("loader %s: empty payload at index %d in shared set", id, i)
		}
	}
	l := &Loader{
		id:            id,
		name:          name,
		role:          role,
		layout:        LayoutSharedWindow,
		replenishment: repl,
		windows:       append([]Window(nil), windows...),
		payloadSet:    append([]PayloadCode(nil), payloadSet...),
		slotCount:     len(windows),
	}
	for _, o := range opts {
		o(l)
	}
	return l, nil
}

// NewDedicatedPositionsLoader builds a dedicated_positions (home-location) loader:
// N independent one-bin positions, each its own node and payload. A position with
// an empty payload is legal (the operator has not assigned one yet) — what is
// rejected is an empty node id or zero positions.
func NewDedicatedPositionsLoader(id LoaderID, name string, role LoaderRole, repl LoaderReplenishment, positions []Position, opts ...LoaderOption) (*Loader, error) {
	if id == "" {
		return nil, fmt.Errorf("loader: empty id")
	}
	if !role.valid() {
		return nil, fmt.Errorf("loader %s: invalid role %q", id, role)
	}
	if len(positions) == 0 {
		return nil, fmt.Errorf("loader %s: dedicated_positions needs at least one position", id)
	}
	for i, p := range positions {
		if p.Node == "" {
			return nil, fmt.Errorf("loader %s: position %d has empty node", id, i)
		}
	}
	l := &Loader{
		id:            id,
		name:          name,
		role:          role,
		layout:        LayoutDedicatedPositions,
		replenishment: repl,
		positions:     append([]Position(nil), positions...),
		slotCount:     len(positions),
	}
	for _, o := range opts {
		o(l)
	}
	return l, nil
}

// ── Accessors (the fields are unexported; these are read-only) ───────

func (l *Loader) ID() LoaderID                       { return l.id }
func (l *Loader) Name() string                       { return l.name }
func (l *Loader) Role() LoaderRole                   { return l.role }
func (l *Loader) Layout() LoaderLayout               { return l.layout }
func (l *Loader) Replenishment() LoaderReplenishment { return l.replenishment }

// InboundSource is the empty market this loader's L1 retrieve_empty orders source
// from (empty when not configured — Core then falls back to a global FIFO scan).
func (l *Loader) InboundSource() string { return l.inboundSource }

// OutboundDest is the market this loader's filled bin (L2) / unloader's emptied bin
// (U2) is sent to on completion. Empty when not configured — the completion handler
// then logs and skips rather than firing a malformed move.
func (l *Loader) OutboundDest() string { return l.outboundDest }

// UOPThresholdFor returns the per-payload UOP threshold (the C-push opt-in): the
// shared per-payload value for shared_window, or the matching position's
// UOPThreshold for dedicated. Zero means "no UOP-threshold policy" (not opted into
// C-push). Only display reads this on the Edge (station board starvation tint);
// ordering is Core's.
func (l *Loader) UOPThresholdFor(p PayloadCode) int {
	if v, ok := l.uopThreshold[p]; ok {
		return v
	}
	for _, pos := range l.positions {
		if pos.Payload == p {
			return pos.UOPThreshold
		}
	}
	return 0
}

// hasConfiguredThreshold reports whether any payload/position carries a UOP
// threshold > 0 — i.e. the loader is actually set up for threshold (C-push)
// replenishment. A threshold-mode loader with none configured is misconfigured.
func (l *Loader) hasConfiguredThreshold() bool {
	for _, v := range l.uopThreshold {
		if v > 0 {
			return true
		}
	}
	for _, pos := range l.positions {
		if pos.UOPThreshold > 0 {
			return true
		}
	}
	return false
}

// PayloadsMissingThreshold lists the payloads this loader serves that carry NO
// UOP threshold, when the loader is switched to threshold replenishment. Empty
// for an operator loader (a threshold is meaningless there) and empty when every
// payload has one.
//
// PARTIAL COVERAGE IS THE COMMON CASE AND IT LOOKS FINE. A loader serving five
// parts with thresholds on three of them passes every check that asks "is a
// threshold configured" — one is — while the other two are ordered by nobody.
// Nothing fires for them and nothing says so. That is the same silence as the
// mode fallback this replaced, one level down: not a loader in the wrong mode,
// but two parts inside a right one that no path covers.
//
// Sorted so the answer is stable for a screen and for a log line.
func (l *Loader) PayloadsMissingThreshold() []PayloadCode {
	if l.replenishment != ReplenishmentThreshold {
		return nil
	}
	var out []PayloadCode
	for _, p := range l.payloadSet {
		if l.uopThreshold[p] <= 0 {
			out = append(out, p)
		}
	}
	for _, pos := range l.positions {
		if pos.Payload != "" && pos.UOPThreshold <= 0 {
			out = append(out, pos.Payload)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// UsesOperatorStaging reports whether this loader is supplied by operator-driven
// opportunistic staging — the window-free push — rather than by the automatic
// threshold path. A consume loader is always operator, so this is true for it.
//
// THE SWITCH MEANS WHAT IT SAYS. This used to be true ALSO for a threshold
// loader with no threshold configured: such a loader would be signalled by
// nobody, so it quietly fell back to operator staging instead. The intent was
// to keep it from starving, and the effect was worse — the loader ran in a mode
// no screen reported. Every surface said "threshold" while the carriers arriving
// at it came from the operator push, and the only trace was one warning line at
// startup.
//
// So: threshold on means the threshold path runs, for whatever thresholds are
// configured. Configure none and nothing fires — which is a plainly visible
// "you have not set this up yet", not a different mode wearing this one's name.
// Threshold off means the push. Two modes, each doing what it is called.
func (l *Loader) UsesOperatorStaging() bool {
	return l.replenishment == ReplenishmentOperator
}

// MisconfiguredThreshold reports a loader switched to threshold replenishment
// with no threshold configured — so the threshold path has nothing to fire on
// and the loader is fed by nothing at all.
//
// This used to describe a fallback ("it is falling back to operator staging").
// There is no fallback now, which makes this warning the ONLY thing standing
// between that configuration and a loader nobody feeds. It must stay loud and
// it must be reachable — see SweepPushLoaders, where it is deliberately checked
// outside the operator-staging gate, because a misconfigured loader is skipped
// by that gate and would otherwise be warned about by nobody.
func (l *Loader) MisconfiguredThreshold() bool {
	return l.replenishment == ReplenishmentThreshold && !l.hasConfiguredThreshold()
}

// SlotCount is the loader's total physical bin slots — for a shared_window loader
// this is the shared empty-in budget (one demand of N → exactly N empties across
// all windows, never 2N). For a dedicated loader it is the position count; each
// position is independently one-bin, so the per-reservation budget there is 1
// (the seam scopes a dedicated reservation to a single position node).
func (l *Loader) SlotCount() int { return l.slotCount }

func (l *Loader) IsShared() bool    { return l.layout == LayoutSharedWindow }
func (l *Loader) IsDedicated() bool { return l.layout == LayoutDedicatedPositions }

// FunnelWindows reports whether this loader takes its empties ONE window at a
// time (budget 1, all to the first window) rather than spreading one bin per
// window. Core owns the setting; see WithFunnelWindows for why it reads as the
// restriction. Meaningless for a dedicated loader, whose positions never shared
// a budget.
func (l *Loader) FunnelWindows() bool { return l.funnelWindows }

// WithChangeoverLoadDirective opts this station into having its card
// commandeered during a changeover.
func WithChangeoverLoadDirective(on bool) LoaderOption {
	return func(l *Loader) { l.changeoverLoadDirective = on }
}

// ChangeoverLoadDirective reports whether a changeover replaces this station's
// board with an instruction naming the carrier the incoming style needs.
//
// Core owns the setting — it is a fact about the station and how it is run, so
// it lives beside the rest of the station's setup in bin_loaders rather than on
// each style's claim, where it used to be duplicated per style.
func (l *Loader) ChangeoverLoadDirective() bool { return l.changeoverLoadDirective }

// IsOperatorDriven reports whether the loader's replenishment is operator-driven
// (replenishment = operator) — the operator stages/clears at the board rather than
// the automatic threshold path supplying it.
func (l *Loader) IsOperatorDriven() bool { return l.replenishment == ReplenishmentOperator }

// Windows returns a copy of the shared_window load points (nil for dedicated).
func (l *Loader) Windows() []Window { return append([]Window(nil), l.windows...) }

// Positions returns a copy of the dedicated positions (nil for shared_window).
func (l *Loader) Positions() []Position { return append([]Position(nil), l.positions...) }

// PayloadSet returns a copy of the shared allowed payload set (nil for dedicated;
// a dedicated loader's payloads live per-position).
func (l *Loader) PayloadSet() []PayloadCode { return append([]PayloadCode(nil), l.payloadSet...) }

// LoadablePayloadCodes returns the flattened set of payload codes an operator may
// load or request at this loader: the shared allowed set (shared_window) plus any
// dedicated-position payloads. This is the Core-owned source of truth that the
// load/request gates and the operator board both read, so they never disagree
// about what is loadable. SynthClaim seeds its AllowedPayloadCodes from the same set.
func (l *Loader) LoadablePayloadCodes() []string {
	codes := make([]string, 0, len(l.payloadSet)+len(l.positions))
	for _, p := range l.payloadSet {
		codes = append(codes, string(p))
	}
	for _, pos := range l.positions {
		if pos.Payload != "" {
			codes = append(codes, string(pos.Payload))
		}
	}
	return codes
}

// PayloadAt returns the payload pinned to the dedicated position at coreNode.
// Only meaningful for a dedicated_positions loader: each position is one node
// bound to one payload. A shared_window window, an unpinned home, or a buffer
// slot carries no payload — ("", false). A node that isn't a position of this
// loader also returns ("", false).
func (l *Loader) PayloadAt(coreNode NodeID) (PayloadCode, bool) {
	for _, pos := range l.positions {
		if pos.Node == coreNode {
			return pos.Payload, pos.Payload != ""
		}
	}
	return "", false
}

// LoadablePayloadCodesAt scopes LoadablePayloadCodes to a SPECIFIC member node.
// For a dedicated_positions loader each position is a one-payload home, so the
// loadable set AT that node is just its own pinned payload (an unpinned home or a
// buffer slot has none → nil). For a shared_window loader every window offers the
// whole shared set, so it returns LoadablePayloadCodes() unchanged. This is the
// per-node truth the operator board and the load/request gate must read for a
// dedicated loader: a home must advertise (and the gate accept) only its own part,
// never the other positions' parts. SynthClaim seeds AllowedPayloadCodes from it.
func (l *Loader) LoadablePayloadCodesAt(coreNode NodeID) []string {
	if l.IsDedicated() {
		if p, ok := l.PayloadAt(coreNode); ok {
			return []string{string(p)}
		}
		return nil
	}
	return l.LoadablePayloadCodes()
}

// SynthClaim builds an in-memory, NON-PERSISTED manual_swap NodeClaim that
// represents this loader at coreNode. It is the Core-owned-loader path's stand-in
// for a per-style style_node_claim: a node that is a window/position of a Core
// loader but has no edge claim still must read as a manual_swap loader node for
// the operator board + load/clear runtime to engage. ID stays 0 to mark it
// synthetic — it is never written back, and callers MUST guard `ID == 0` before
// using the id as a foreign key (e.g. runtime active_claim_id). AllowedPayloadCodes
// is scoped to THIS node (LoadablePayloadCodesAt): a dedicated home carries only
// its own pinned payload, a shared window the whole set — so loadablePayloads
// resolves the same per-node truth off the claim that the gate does.
//
// TWO SYNTHESIZED CLAIMS EXIST IN THIS PACKAGE AND THEIR ID RULES ARE OPPOSITE.
//
//	Loader.SynthClaim (here)        ID == 0.       There is no persisted row
//	                                               anywhere. Guard ID == 0 before
//	                                               using it as a foreign key.
//	SynthesizePositionClaim    ID == parent's. A press position's claim IS a
//	                                               view of the parent press-index
//	                                               row, and node tasks store that
//	                                               id so FromClaimID/ToClaimID
//	                                               resolve back to it.
//
// Neither is ever written back. The difference is whether the thing being
// stood in for exists: a Core-owned loader window has no claim at all, while a
// press position has one — its parent's. Reading a position's ID as "synthetic, so
// zero" would break the position resolver; writing a loader's ID anywhere would
// dangle. See PositionClaimFromParent in process.go for the position side.
func (l *Loader) SynthClaim(coreNode NodeID) *NodeClaim {
	return &NodeClaim{
		CoreNodeName:        string(coreNode),
		Role:                protocol.ClaimRole(l.role),
		SwapMode:            protocol.SwapModeManualSwap,
		AllowedPayloadCodes: l.LoadablePayloadCodesAt(coreNode),
		InboundSource:       l.inboundSource,
		OutboundDestination: l.outboundDest,
		AutoConfirm:         true, // mandatory for bin_loader claims (auto-confirm delivery)
	}
}

// Contains reports whether node is one of this loader's member nodes (a window or a
// position) — used to resolve a loader from any of the physical nodes that belong to
// it. The loader IDENTITY (l.id) is NOT compared: after the step-4 cutover it is the
// loader_key token, not a node name, so a loader is never a node.
// Every loader has >=1 materialised member, so the identity is never needed here.
func (l *Loader) Contains(node NodeID) bool {
	for _, w := range l.windows {
		if w.Node == node {
			return true
		}
	}
	for _, p := range l.positions {
		if p.Node == node {
			return true
		}
	}
	return false
}

// ServesPayload reports whether this loader can stage the payload: any member of
// the shared set for shared_window, or any position's payload for dedicated.
func (l *Loader) ServesPayload(p PayloadCode) bool {
	if l.IsShared() {
		return slices.Contains(l.payloadSet, p)
	}
	for _, pos := range l.positions {
		if pos.Payload == p {
			return true
		}
	}
	return false
}

// ReservationTarget returns the delivery-node set and the empty-in BUDGET for a
// demand of `payload` at this loader — the inputs the reservation seam counts
// across and caps to. The per-layout semantics live here so the seam stays
// layout-agnostic:
//
//   - shared_window, multiWindow=false: funnel to the loader's anchor (its ID)
//     with a budget of 1 — behaviour-identical to the pre-multi-window resolver
//     (FINAL-ADJUDICATION flip-trigger #3: don't fragment the budget before
//     delivery spreads / the board renders the windows).
//   - shared_window, multiWindow=true: spread across the loader's windows —
//     budget = SlotCount, delivered round-robin to free windows. A loader with a
//     single window (no homes configured) still resolves to one node / budget 1,
//     so flipping the flag only changes loaders actually configured multi-window.
//   - dedicated_positions: the payload maps to ONE independent one-bin position;
//     budget 1, delivered there. When member names a position serving the payload,
//     deliver to THAT position (the same-payload-two-positions fix) instead of
//     first-match; member "" (operator request) falls back to
//     first-match, preserving prior behaviour. Positions never share a budget.
//
// member is the specific loader member node the triggering signal names
// (LoopBelowThresholdSignal.MemberNodeName). It is honoured only for dedicated
// layouts — shared windows share one budget, so the seam's free-window assignment
// (perNode==0) picks the slot and member is ignored. "" means "no member named."
//
// Returns (nil, 0) when the loader does not serve the payload (no target).
func (l *Loader) ReservationTarget(member NodeID, payload PayloadCode, multiWindow bool) (nodes []NodeID, budget int) {
	if l.IsShared() {
		// A blank payload is the payload-agnostic transitional stage — still a
		// valid target; a named payload must be in the shared set.
		if payload != "" && !l.ServesPayload(payload) {
			return nil, 0
		}
		if multiWindow {
			out := make([]NodeID, len(l.windows))
			for i, w := range l.windows {
				out[i] = w.Node
			}
			return out, l.slotCount
		}
		// multiWindow off: funnel to the loader's first WINDOW (a real node), budget 1 —
		// never the identity l.id, which is the loader_key token, not a node.
		// Members are materialised so a window always exists; guard anyway.
		if len(l.windows) == 0 {
			return nil, 0
		}
		return []NodeID{l.windows[0].Node}, 1
	}
	// Dedicated: honour the named member when it is a position serving the payload —
	// route the empty to the position the signal already identified, not first-match.
	if member != "" {
		for _, pos := range l.positions {
			if pos.Node == member && (payload == "" || pos.Payload == payload) {
				return []NodeID{pos.Node}, 1
			}
		}
	}
	// No member named, or it didn't match a serving position: first-match fallback.
	for _, pos := range l.positions {
		if pos.Payload == payload {
			return []NodeID{pos.Node}, 1
		}
	}
	return nil, 0
}

// DeliveryNodes is the set of nodes an empty for this loader may be delivered to
// — every window for shared_window, every position for dedicated. The
// reservation seam counts in-flight empties across this set (for shared, the
// whole set shares one budget; for dedicated, each position is queried alone).
func (l *Loader) DeliveryNodes() []NodeID {
	if l.IsShared() {
		out := make([]NodeID, len(l.windows))
		for i, w := range l.windows {
			out[i] = w.Node
		}
		return out
	}
	out := make([]NodeID, len(l.positions))
	for i, p := range l.positions {
		out[i] = p.Node
	}
	return out
}
