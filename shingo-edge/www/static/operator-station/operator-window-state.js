// operator-window-state.js — the single source of truth for a manual_swap
// (loader / unloader) WINDOW's state on the operator board.
//
// One phase derivation + one per-role word table, so the header badge and the
// per-payload cards always tell the SAME story, and the loader/unloader wording
// lives in ONE place instead of as role gates and hand-tuned strings scattered
// through a precedence ladder. This is the refactor of the old cardState ladder in
// operator-render.js (the "subtle word differences" lived there).
//
// Pure + import-free on purpose: it unit-tests under plain Node (the test strips
// `export` and runs it in a vm — see operator-window-state.test.js), and it keeps
// rendering concerns (DOM, transitional coverage badges, idle-card hiding) in
// operator-render.js, which consumes this.
//
// Incident cases preserved + pinned by the test (do not regress):
//   - a consume EMPTY bin is a SWAP, never a produce-style LOAD (plant 2026-06-02);
//   - a produce empty + demand is LOAD-now, not QUEUED (plant 2026-06-01);
//   - an agnostic blank-payload empty lights every allowed payload so the operator
//     picks the carrier, and must NOT stamp every card QUEUED.

'use strict';

// WINDOW_ACTIVE_STATUSES is the set of order statuses still in the live
// lifecycle (= !terminal), for the window-card active filter. This mirrors
// order-status.js isActive exactly; the Go drift test
// (shingo-edge/www/order_status_js_drift_test.go
// TestWindowStateActiveStatusesAgreeWithProtocol) pins it to the protocol's
// non-terminal set so it cannot silently drift.
//
// Exposed as a lex-sorted const so the drift test can read it. The previous
// inlined list omitted sourcing, dispatched, submitted, faulted, and
// reshuffling, so an order in any of those vanished from window cards (fell to
// NO DEMAND) while the operator modal still counted it.
export const WINDOW_ACTIVE_STATUSES = [
    'acknowledged', 'delivered', 'dispatched', 'faulted', 'in_transit',
    'pending', 'queued', 'reshuffling', 'sourcing', 'staged', 'submitted',
];

// isActiveStatus: an order still in the live lifecycle (non-terminal). Mirrors
// order-status.js isActive. Kept as the call site for readability; the const
// above is the drift-pinned source of truth.
export function isActiveStatus(s) {
    return WINDOW_ACTIVE_STATUSES.indexOf(s) !== -1;
}

// ROLE_WORDS centralizes every loader↔unloader wording difference. A loader awaits an
// EMPTY bin to fill; an unloader awaits a FULL bin to pull. This table is the one
// place those words differ — change a label here, both the header and the cards follow.
export const ROLE_WORDS = {
    produce: { awaiting: 'BIN', present: 'LOADED', actVerb: 'load' },
    consume: { awaiting: 'FULL', present: 'FULL', actVerb: 'unload' },
};

function words(role) { return ROLE_WORDS[role] || ROLE_WORDS.produce; }

// nodeFacts derives the role-neutral node-level facts from one station node view
// (the manual_swap entry). The per-card model and the header both build on these, so
// they can never disagree about what's physically at the window.
export function nodeFacts(entry) {
    const claim = entry.active_claim || {};
    const bs = entry.bin_state || {};
    const activeOrders = (entry.orders || []).filter(function (o) { return isActiveStatus(o.status); });
    const binPresent = !!bs.occupied;
    const binEmpty = binPresent && !bs.payload_code;
    const binLoaded = binPresent && !!bs.payload_code;
    return {
        role: claim.role,
        binPresent: binPresent,
        binEmpty: binEmpty,
        binLoaded: binLoaded,
        binPayload: bs.payload_code || '',
        activeOrders: activeOrders,
        hasDemand: activeOrders.length > 0,
        // consume-role swap affordances (a loaded full to pull / an empty to return)
        canClearLoaded: claim.role === 'consume' && binLoaded,
        canSwapEmpty: claim.role === 'consume' && binEmpty,
        // produce: an empty bin parked with no order yet is still loadable
        canLoadEmpty: binEmpty && activeOrders.length === 0,
    };
}

// cardModel computes the rendered state of one (window × payload) card: the status
// tag, its CSS class, the detail line, the click action, and the load-now flag.
// Replaces cardState + the per-card derivation in buildLoaderCard. The precedence is
// preserved exactly (it encodes the incident cases above); only the role-specific
// words are now sourced from ROLE_WORDS rather than inline ternaries.
export function cardModel(entry, code) {
    const f = nodeFacts(entry);
    const role = f.role;
    const w = words(role);

    const payloadOrders = f.activeOrders.filter(function (o) { return o.payload_code === code; });
    // Agnostic blank-payload demand: an empty parked with general (untagged) demand
    // lights every allowed payload so the operator picks the carrier. Must not fire
    // once a bin is loaded or an order carries a specific code (else every tile reads
    // QUEUED) — plant 2026-06-01.
    const hasPayloadDemand = payloadOrders.length > 0 ||
        (f.binEmpty && f.hasDemand && f.activeOrders.every(function (o) { return !o.payload_code; }));
    const payloadActive = hasPayloadDemand || (f.binEmpty && f.hasDemand);
    const payloadDelivered = payloadOrders.some(function (o) { return o.status === 'delivered'; });
    // acknowledged is Core's intake ack (the fleet accepted the order,
    // pre-sourcing) — it is NOT a moving robot. Keep it separate from in_transit
    // so it renders as its own step, not "IN TRANSIT"; in_transit alone is the
    // real transit bucket.
    const payloadInTransit = payloadOrders.some(function (o) { return o.status === 'in_transit'; });
    const payloadAcknowledged = payloadOrders.some(function (o) { return o.status === 'acknowledged'; });
    const loadNow = f.binEmpty && hasPayloadDemand;
    const canClearThisPayload = f.canClearLoaded && f.binPayload === code;

    // Status tag (precedence-ordered; the order encodes the incident fixes — a consume
    // empty must be caught as SWAP before the produce LOAD fallback, plant 2026-06-02).
    let cls, statusText, statusClass;
    if (payloadDelivered) { cls = 'os-board-delivered'; statusText = 'DELIVERED'; statusClass = 'os-board-tag-delivered'; }
    else if (canClearThisPayload) { cls = 'os-board-delivered'; statusText = 'SWAP'; statusClass = 'os-board-tag-delivered'; }
    else if (f.canSwapEmpty) { cls = 'os-board-delivered'; statusText = 'SWAP'; statusClass = 'os-board-tag-delivered'; }
    else if (payloadInTransit) { cls = 'os-board-transit'; statusText = 'IN TRANSIT'; statusClass = 'os-board-tag-transit'; }
    else if (payloadAcknowledged) { cls = 'os-board-queued'; statusText = 'ACKNOWLEDGED'; statusClass = 'os-board-tag-queued'; }
    else if (loadNow) { cls = 'os-board-queued'; statusText = 'LOAD'; statusClass = 'os-board-tag-queued'; }
    else if (hasPayloadDemand) { cls = 'os-board-queued'; statusText = 'QUEUED'; statusClass = 'os-board-tag-queued'; }
    else if (f.canLoadEmpty) { cls = 'os-board-queued'; statusText = 'LOAD'; statusClass = 'os-board-tag-queued'; }
    else { cls = 'os-board-nodemand'; statusText = 'NO DEMAND'; statusClass = 'os-board-tag-nodemand'; }

    // Detail line. The role-specific verbs come from ROLE_WORDS, not inline ternaries.
    let detail;
    if (payloadDelivered) detail = 'Tap to ' + w.actVerb;
    else if (canClearThisPayload) detail = 'Loaded bin parked — tap to swap';
    else if (f.canSwapEmpty) detail = 'Empty bin parked — tap to swap';
    else if (f.binEmpty && (payloadInTransit || hasPayloadDemand)) detail = 'Empty bin at node — tap to load';
    else if (payloadInTransit) detail = 'Robot en route';
    else if (payloadAcknowledged) detail = 'Order accepted — awaiting dispatch';
    else if (hasPayloadDemand) detail = 'Waiting for robot';
    else if (f.canLoadEmpty) detail = 'Empty bin parked — tap to load';
    else detail = 'No kanban signal';

    // Action (what the tap does), role-gated. A LOADER acts on a delivered empty
    // directly — the tap IS the receipt confirmation — so it must not also require
    // bin_state to already reflect the bin (that telemetry lags the order); this
    // mirrors the unloader, which already acts on a delivered order alone.
    const canLoad = role === 'produce' && (payloadDelivered || (f.binEmpty && payloadActive) || f.canLoadEmpty);
    const canUnload = canClearThisPayload || f.canSwapEmpty || (payloadDelivered && role === 'consume');
    const action = canLoad ? 'load' : (canUnload ? 'unload' : '');

    // badgeOrder is the single order the corner badge describes. Picked with the
    // SAME precedence the badge's colour uses (delivered → in transit → first
    // queued) so the badge's colour and its text can never describe different
    // orders when a payload has more than one in flight.
    const badgeOrder = payloadOrders.find(function (o) { return o.status === 'delivered'; }) ||
        payloadOrders.find(function (o) { return o.status === 'in_transit'; }) ||
        payloadOrders[0];

    return {
        cls: cls, statusText: statusText, statusClass: statusClass, detail: detail, action: action, loadNow: loadNow,
        // Queue-position badge facts (rendered by operator-render): only REAL per-payload
        // orders count — the agnostic blank-payload empty must not stamp a number on every card.
        queueCount: payloadOrders.length, delivered: payloadDelivered, inTransit: payloadInTransit,
        // sourceNode is where badgeOrder is pulling FROM — the supermarket or buffer
        // slot the carrier is coming out of. On a home-location board this replaces the
        // queue ordinal: each home is its own one-bin slot, so "3rd in the queue" says
        // nothing an operator can act on, while "which supermarket slot is feeding me"
        // does. Empty when no real per-payload order backs the card.
        sourceNode: (badgeOrder && badgeOrder.source_node) || '',
    };
}

// headerModel computes the window header badge — text + inline color. It reads the
// SAME facts the cards do, so the badge can never contradict a card (the old bug:
// header "AWAITING BIN" above a card saying "tap to load"). Physical bin first; with
// no bin yet, reflect the inbound order, worded by role (a loader awaits an empty
// BIN, an unloader a FULL).
export function headerModel(entry) {
    const f = nodeFacts(entry);
    const w = words(f.role);
    if (f.binPresent) {
        if (f.binLoaded) return { text: w.present, color: 'background:#1a3a1a;color:#6f6' };
        return { text: 'EMPTY', color: 'background:#3a1a1a;color:#f88' };
    }
    if (f.activeOrders.some(function (o) { return o.status === 'delivered'; })) {
        return { text: w.awaiting + ' ARRIVED', color: 'background:#2a3a1a;color:#cf6' };
    }
    // A robot actually en route (in_transit) is "ARRIVING"; an acknowledged
    // order (fleet accepted, not yet moving) is NOT arriving, so it falls
    // through to the "AWAITING" default rather than pretending a bin is on its
    // way.
    if (f.activeOrders.some(function (o) { return o.status === 'in_transit'; })) {
        return { text: w.awaiting + ' ARRIVING', color: 'background:#1a2a3a;color:#6cf' };
    }
    return { text: 'AWAITING ' + w.awaiting, color: 'background:#2a2a1a;color:#ff6' };
}
