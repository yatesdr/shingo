// operator-window-state.js — the single source of truth for a loader or
// unloader WINDOW's state on the operator board.
//
// One phase derivation + one per-role word table, so the header badge and the
// per-payload cards always tell the SAME story, and the loader/unloader wording
// lives in ONE place instead of as role gates and hand-tuned strings scattered
// through a precedence ladder. This is the refactor of the old cardState ladder in
// operator-render.js (the "subtle word differences" lived there).
//
// Pure + import-free on purpose: it unit-tests under plain Node (the test strips
// `export` and runs it in a vm — see operator-window-state.test.js), and it keeps
// rendering concerns (DOM, the operator board's coverage badges, idle-card hiding) in
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

// windowRole is the role a loader window is worked in, the one read of it on
// the glass: 'consume' at a window of a two-stage unloader's stage 2
// (tile.loader.takes_bare; a produce loader since 2026-10-29, worked as an
// unloader all the same), else the loader's role; undefined on a tile with no
// loader. It lives here, at the bottom of the board's imports, so this file
// stays import-free: operator-board-model.js's tileRole and the modal read it
// through tileRole, this file directly.
export function windowRole(entry) {
    const loader = (entry && entry.loader) || null;
    if (!loader) return undefined;
    return loader.takes_bare ? 'consume' : loader.role;
}

// nodeFacts derives the role-neutral node-level facts from one station node view
// (a loader tile). The per-card model and the header both build on these, so
// they can never disagree about what's physically at the window. The role is the
// window's (windowRole, from tile.loader): a loader tile carries no claim.
export function nodeFacts(entry) {
    const role = windowRole(entry);
    const bs = entry.bin_state || {};
    const activeOrders = (entry.orders || []).filter(function (o) { return isActiveStatus(o.status); });
    const binPresent = !!bs.occupied;
    const binEmpty = binPresent && !bs.payload_code;
    const binLoaded = binPresent && !!bs.payload_code;
    return {
        role: role,
        binPresent: binPresent,
        binEmpty: binEmpty,
        binLoaded: binLoaded,
        binPayload: bs.payload_code || '',
        activeOrders: activeOrders,
        hasDemand: activeOrders.length > 0,
        // consume-role swap affordances (a loaded full to pull / an empty to return)
        canClearLoaded: role === 'consume' && binLoaded,
        canSwapEmpty: role === 'consume' && binEmpty,
        // produce: an empty bin parked with no order yet is still loadable
        canLoadEmpty: binEmpty && activeOrders.length === 0,
    };
}

// isOwnEmptyOut reports whether order o is this CONSUME node's own empty-out (U2):
// a move whose source_node is this node. The U2 names no part (the Edge sends it
// blank — createUnloaderEmptyOut), so matching it by payload_code would drop it
// from every card while its carrier leaves; it is attributed by where it leaves
// FROM instead. Nothing broader: a produce home position receives blank L1s, and a
// blank move arriving from another node is not this node's carrier leaving, so
// neither counts (operator-window-state.test.js cases 24–27).
export function isOwnEmptyOut(entry, o) {
    const coreNode = (entry.node && entry.node.core_node_name) || '';
    return windowRole(entry) === 'consume' && coreNode !== '' &&
        o.order_type === 'move' && o.source_node === coreNode;
}

// emptyCarrierHeld reports whether the empty carrier standing on this node is
// promised to an order that has not delivered yet: a live row on the node that
// leaves FROM it (a home's clear, its parent and the move of the emptied
// carrier, are pushed here by Core) at any status short of delivered. Core
// refuses a LOAD onto such a carrier with one sentence (the held-carrier check,
// refuseHeldCarrierTx), so no surface offers LOAD on it; the check stays the
// backstop for a direct post. Only an empty carrier asks: a LOAD of a labelled
// one never reaches Core (the Edge's loadOntoLabelledCarrier). A delivered row
// has given its hold up (Core's arrival), so LOAD returns with it.
export function emptyCarrierHeld(entry) {
    const bs = entry.bin_state || {};
    const coreNode = (entry.node && entry.node.core_node_name) || '';
    if (!bs.occupied || bs.payload_code || coreNode === '') return false;
    const short = function (n) { return String(n || '').replace(/^.*\./, ''); };
    return (entry.orders || []).some(function (o) {
        return isActiveStatus(o.status) && o.status !== 'delivered' && short(o.source_node) === short(coreNode) &&
            !leavesAsDemand(entry, o);
    });
}

// leavesAsDemand reports whether a row leaving a home is the line's demand
// riding on it, not the home's clear: a move that names its part and delivers
// to another node. The clear's rows name no part (the emptied carrier's move)
// or come back to the home (its parent). Demand holds no carrier until Core
// issues it holding the loaded one, so it neither holds the empty nor reads as
// a clear in flight (SPR 2026-10-28: a queued move to the line hid the LOAD of
// the empty its home stood with, and the line waited on a load nobody was shown).
export function leavesAsDemand(entry, o) {
    const coreNode = (entry.node && entry.node.core_node_name) || '';
    const short = function (n) { return String(n || '').replace(/^.*\./, ''); };
    return !!o.payload_code && short(o.source_node) === short(coreNode) && short(o.delivery_node) !== short(coreNode);
}

// CLEAR_RUNNING is a loader home's words while its clear runs and the emptied
// carrier still stands on it: the job, in place of a LOAD the carrier's order
// would have refused. The board's card and the node grid's tile both say it.
export const CLEAR_RUNNING = 'Robot on its way for the empty · partial follows';

// clearRunning reports whether a home's clear is running with its empty still
// on it: the empty is held (emptyCarrierHeld) and a live move delivers TO the
// node (Core pushes the clear's parent, home to home, and the partial's move,
// spot to home). A loader's own moves leave a home and never deliver to it.
export function clearRunning(entry) {
    if (!emptyCarrierHeld(entry)) return false;
    const coreNode = (entry.node && entry.node.core_node_name) || '';
    const short = function (n) { return String(n || '').replace(/^.*\./, ''); };
    return (entry.orders || []).some(function (o) {
        return o.order_type === 'move' && isActiveStatus(o.status) && short(o.delivery_node) === short(coreNode);
    });
}

// cardModel computes the rendered state of one (window × payload) card: the status
// tag, the detail line, the click action, and the load-now flag.
// Replaces cardState + the per-card derivation in buildLoaderCard. The precedence is
// preserved exactly (it encodes the incident cases above); only the role-specific
// words are now sourced from ROLE_WORDS rather than inline ternaries.
export function cardModel(entry, code) {
    const f = nodeFacts(entry);
    const role = f.role;
    const w = words(role);

    const payloadOrders = f.activeOrders.filter(function (o) {
        return o.payload_code === code || isOwnEmptyOut(entry, o);
    });
    // Agnostic blank-payload demand: an empty parked with general (untagged) demand
    // lights every allowed payload so the operator picks the carrier. Must not fire
    // once a bin is loaded or an order carries a specific code (else every tile reads
    // QUEUED) — plant 2026-06-01.
    const hasPayloadDemand = payloadOrders.length > 0 ||
        (f.binEmpty && f.hasDemand && f.activeOrders.every(function (o) { return !o.payload_code; }));
    const payloadActive = hasPayloadDemand || (f.binEmpty && f.hasDemand);
    const payloadDelivered = payloadOrders.some(function (o) { return o.status === 'delivered'; });
    // acknowledged is Core's intake ack (the fleet accepted the order,
    // pre-dispatch) — it is NOT a moving robot. Keep it separate from in_transit
    // so it renders as its own step, not "IN TRANSIT"; in_transit alone is the
    // real transit bucket.
    const payloadInTransit = payloadOrders.some(function (o) { return o.status === 'in_transit'; });
    const payloadAcknowledged = payloadOrders.some(function (o) { return o.status === 'acknowledged'; });
    const loadNow = f.binEmpty && hasPayloadDemand;
    const canClearThisPayload = f.canClearLoaded && f.binPayload === code;

    // waitingOnRobot marks the one card state that is a DOWNTIME signal rather than
    // an operator cue: a call is queued for this payload and nothing is coming for
    // it — nothing delivered, nothing in transit, the fleet has not even acknowledged
    // it, and there is no empty parked to load. LOAD and ACKNOWLEDGED are queued
    // states too but neither is downtime (one is actionable right now, the other has
    // the fleet on it), so the render layer needs this flag to escalate only the real one.
    // Set in the QUEUED branch below and nowhere else.
    let waitingOnRobot = false;

    // Status tag (precedence-ordered; the order encodes the incident fixes — a consume
    // empty must be caught as SWAP before the produce LOAD fallback, plant 2026-06-02).
    let statusText;
    if (payloadDelivered) statusText = 'DELIVERED';
    else if (canClearThisPayload) statusText = 'SWAP';
    else if (f.canSwapEmpty) statusText = 'SWAP';
    else if (payloadInTransit) statusText = 'IN TRANSIT';
    else if (payloadAcknowledged) statusText = 'ACKNOWLEDGED';
    else if (loadNow) statusText = 'LOAD';
    else if (hasPayloadDemand) { statusText = 'QUEUED'; waitingOnRobot = true; }
    else if (f.canLoadEmpty) statusText = 'LOAD';
    else statusText = 'NO DEMAND';

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
    // An empty an unfinished order holds is not loadable: Core refuses the LOAD
    // (emptyCarrierHeld). The card says the job while a clear runs, else that
    // the empty is held, and offers no act; a delivered row ends the hold.
    const held = role === 'produce' && emptyCarrierHeld(entry);
    if (held) {
        const running = clearRunning(entry);
        statusText = running ? 'CLEARING' : 'HELD';
        detail = running ? CLEAR_RUNNING : 'Held for an order — load after it leaves';
        waitingOnRobot = false;
    }
    const canLoad = !held && role === 'produce' && (payloadDelivered || (f.binEmpty && payloadActive) || f.canLoadEmpty);
    const canUnload = canClearThisPayload || f.canSwapEmpty || (payloadDelivered && role === 'consume');
    const action = canLoad ? 'load' : (canUnload ? 'unload' : '');

    // badgeOrder is the single order the corner badge describes. Picked with the
    // SAME precedence the badge's colour uses (delivered → in transit → first
    // queued) so the badge's colour and its text can never describe different
    // orders when a payload has more than one in flight.
    const badgeOrder = payloadOrders.find(function (o) { return o.status === 'delivered'; }) ||
        payloadOrders.find(function (o) { return o.status === 'in_transit'; }) ||
        payloadOrders[0];

    // Queue facts for a waiting card. queueReason is Core's own blocking sentence,
    // mirrored onto the edge order by OrderUpdate (domain.Order.QueueReason) and
    // already rendered verbatim by the operator modal — the BOARD threw it away, so
    // an operator had to tap a card to find out why nothing was coming. Read from
    // whichever order carries one: Core populates it only while status == queued, so
    // a payload with a queued order and a moving one has the reason on the former.
    const reasoned = payloadOrders.find(function (o) { return !!o.queue_reason; });
    // waitingSince is the OLDEST order for the payload, not badgeOrder's timestamp:
    // the question a downtime alert answers is "how long has this cell been waiting",
    // which is the first unanswered call, not the most recent one.
    let waitingSince = '';
    for (let i = 0; i < payloadOrders.length; i++) {
        const created = payloadOrders[i].created_at;
        if (!created) continue;
        if (!waitingSince || Date.parse(created) < Date.parse(waitingSince)) waitingSince = created;
    }

    return {
        statusText: statusText, detail: detail, action: action, loadNow: loadNow && !held,
        // Queue-position badge facts (rendered by operator-render): only REAL per-payload
        // orders count — the agnostic blank-payload empty must not stamp a number on every card.
        queueCount: payloadOrders.length, delivered: payloadDelivered, inTransit: payloadInTransit,
        // sourceNode is where badgeOrder is pulling FROM — the supermarket or buffer
        // slot the carrier is coming out of. On a dedicated-positions board this replaces the
        // queue ordinal: each home is its own one-bin slot, so "3rd in the queue" says
        // nothing an operator can act on, while "which supermarket slot is feeding me"
        // does. Empty when no real per-payload order backs the card.
        sourceNode: (badgeOrder && badgeOrder.source_node) || '',
        // Downtime facts (see waitingOnRobot above). queueReason/waitingSince are
        // populated whenever the orders carry them, not only on a waiting card —
        // the flag decides escalation, these two only supply the words for it.
        waitingOnRobot: waitingOnRobot,
        queueReason: (reasoned && reasoned.queue_reason) || '',
        waitingSince: waitingSince,
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
