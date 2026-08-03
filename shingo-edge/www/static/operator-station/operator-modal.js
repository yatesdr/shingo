import { esc, fillColor, postAction, formatETA } from './operator-util.js';
import {
    confirmRefuseSupply, confirmUndoSupplyRefusal, REFUSE_LABEL, UNDO_LABEL,
} from './operator-supply-refusal.js';
import {
    getView, getSelectedNodeID, setSelectedNodeID,
    findNodeByID, isReplenishing,
} from './operator-state.js';
import { openLoadBin } from './operator-load-bin.js';
import { openKeypad } from './operator-keypad.js';
import { openReleasePrompt, openStrandedStub } from './operator-release.js';
import { isActive } from './order-status.js';

const nodeModal = document.getElementById('node-modal');
const nodeModalContent = document.getElementById('node-modal-content');

let loadViewRef = null;
export function setModalLoadView(fn) { loadViewRef = fn; }

// ── Don't rebuild the modal out from under a finger ──────────────────────────
//
// renderModal ends in `nodeModalContent.innerHTML = html`, which DESTROYS every
// button and builds new ones. If that lands between an operator's pointerdown
// and the click, the element the press started on no longer exists, so the
// browser never generates a click for it — the tap is silently swallowed and
// nothing reaches the server.
//
// The board refreshes on every changed view, and on a live plant the view
// changes constantly: Springfield 2026-07-31 measured ~30 view-changing events
// per minute sustained for a full hour (a consume node's PLC ticks alone
// guarantee it), i.e. a full modal teardown roughly every 2 seconds the whole
// time an operator was trying to press RELEASE.
//
// So: hold repaints for the duration of a press and apply the last one after.
// The narrow window is the fix — the alternative (skip repaints whenever the
// modal is open) leaves the operator looking at stale buttons for as long as
// they leave it open, which is a worse failure. The real end state is a keyed
// render that never replaces unchanged elements at all; see SHINGO_TODO "C3".
//
// Precedent: operator.js already skips the repaint while the release prompt is
// up, for the same underlying reason. This covers the modal itself.
// pressStartedAt is the RELEASE VALVE. pointerup/pointercancel are not
// guaranteed to arrive — a backgrounded tab, a gesture captured by the browser
// or a kiosk shell, a pointer that leaves the window — and without a valve a
// missed one would leave pressInFlight stuck true and freeze the modal forever
// on stale buttons. That is exactly the failure this mechanism exists to avoid,
// so it must not be able to cause it. A press outlives its own window: anything
// older than PRESS_MAX_MS is treated as never having ended.
const PRESS_MAX_MS = 2000;
let pressInFlight = false;
let pressStartedAt = 0;
let pendingEntry = null;

function pressIsStale() {
    return pressInFlight && (Date.now() - pressStartedAt) > PRESS_MAX_MS;
}

nodeModalContent.addEventListener('pointerdown', () => {
    pressInFlight = true;
    pressStartedAt = Date.now();
});

// Second valve: if the page is hidden or loses focus mid-press, the matching
// pointerup will never be seen here.
for (const evt of ['visibilitychange', 'blur']) {
    window.addEventListener(evt, () => { pressInFlight = false; });
}

// pointerup on window (not the modal) so a press that drags off the button
// still clears. The deferred repaint runs in a later task via setTimeout: click
// is dispatched synchronously after pointerup, so repainting inline here would
// destroy the button before its own click fired — reintroducing the bug at the
// last possible moment.
for (const evt of ['pointerup', 'pointercancel']) {
    window.addEventListener(evt, () => {
        if (!pressInFlight) return;
        pressInFlight = false;
        setTimeout(() => {
            if (!pendingEntry) return;
            const entry = pendingEntry;
            pendingEntry = null;
            renderModal(entry);
        }, 0);
    });
}

export function openModal(nodeID) {
    const entry = findNodeByID(nodeID);
    if (!entry) return;
    setSelectedNodeID(nodeID);
    renderModal(entry);
    nodeModal.classList.add('active');
}

export function closeModal() {
    setSelectedNodeID(null);
    nodeModal.classList.remove('active');
    // Drop any repaint that was waiting on a press: the modal it targeted is
    // gone, and applying it after a reopen would paint the previous node.
    pendingEntry = null;
}

export function renderModal(entry) {
    // A press is in flight — defer, so the button under the finger survives
    // long enough to fire its click. See the pressInFlight comment above.
    // A stale press (no pointerup ever arrived) releases the hold rather than
    // freezing the modal.
    if (pressIsStale()) {
        pressInFlight = false;
    }
    if (pressInFlight) {
        pendingEntry = entry;
        return;
    }
    const claim = entry.active_claim;
    const runtime = entry.runtime || {};
    const remaining = runtime.remaining_uop_cached != null ? runtime.remaining_uop_cached : 0;
    const capacity = claim ? claim.uop_capacity : 0;
    const pct = capacity > 0 ? Math.min(remaining / capacity, 1) : 0;
    const task = entry.changeover_task;

    let html = '';

    html += '<div class="modal-header">';
    html += '<div class="modal-node-name">' + esc(entry.node.name) + '</div>';

    if (claim && claim.swap_mode === 'manual_swap') {
        const binState = entry.bin_state;
        const binLabel = binState && binState.bin_label ? binState.bin_label : 'No bin';
        const binPayload = binState && binState.payload_code ? binState.payload_code : '';
        const roleLabel = claim.role === 'produce' ? 'Loader' : 'Unloader';
        html += '<div class="modal-payload">' + roleLabel + ' - Bin: ' + esc(binLabel) + (binPayload ? ' (' + esc(binPayload) + ')' : '') + '</div>';
        html += '<div class="modal-fill-row">';
        html += '<div class="modal-fill-text" style="font-size:18px;font-weight:600">' + (remaining > 0 ? 'LOADED (' + remaining + ' UoP)' : 'EMPTY') + '</div>';
        html += '</div>';
    } else {
        const binState = entry.bin_state;
        const binLabel = binState && binState.bin_label ? ' - Bin: ' + esc(binState.bin_label) : '';
        html += '<div class="modal-payload">' + esc(claim ? claim.payload_code || 'Unassigned' : 'No claim') + binLabel + '</div>';
        html += '<div class="modal-fill-row">';
        html += '<div class="modal-fill-bar"><div class="modal-fill-level" style="width:' + Math.round(pct * 100) + '%;background:' + fillColor(pct, remaining) + '"></div></div>';
        html += '<div class="modal-fill-text">' + remaining + ' / ' + capacity + '</div>';
        html += '</div>';

        // Active lineside chips — operator-pulled parts on the current style.
        const activeBuckets = entry.lineside_active || [];
        if (activeBuckets.length > 0) {
            html += '<div class="os-lineside-active-row">';
            html += '<div class="os-lineside-label">Lineside</div>';
            html += '<div class="os-lineside-chips">';
            activeBuckets.forEach(function(b) {
                html += '<span class="os-lineside-chip active">' +
                    esc(b.part_number) + ': <strong>' + (b.qty || 0) + '</strong></span>';
            });
            html += '</div></div>';
        }

        // Stranded chips — inactive buckets from prior styles.
        const strandedBuckets = entry.lineside_inactive || [];
        if (strandedBuckets.length > 0) {
            html += '<div class="os-lineside-stranded-row">';
            html += '<div class="os-lineside-label stranded">Stranded</div>';
            html += '<div class="os-lineside-chips">';
            strandedBuckets.forEach(function(b) {
                html += '<button type="button" class="os-lineside-chip stranded" ' +
                    'data-action="stranded-chip:' + b.id + '">' +
                    esc(b.part_number) + ': ' + (b.qty || 0) + '</button>';
            });
            html += '</div></div>';
        }
    }

    if (isReplenishing(entry)) {
        const activeOrders = (entry.orders || []).filter(o => isActive(o.status));
        const statusText = activeOrders.length > 0
            ? activeOrders.map(o => o.order_type + ': ' + o.status).join(', ')
            : 'Order in progress';
        html += '<div class="modal-status">[REP] ' + esc(statusText) + '</div>';
    } else {
        html += '<div class="modal-status">No active orders</div>';
    }

    if (task) {
        html += '<div class="modal-co-info">[CO] Changeover: ' + esc(task.situation) + ' - ' + esc(task.state) + '</div>';
    }
    html += '</div>'; // close header

    // Release-error chip: surfaces a Core-side recoverable failure (currently
    // only manifest_sync_failed) that has rolled the order back to Staged for
    // retry. The retry IS the existing release button below — no separate
    // action needed since the order is back in StatusStaged. Chip clears
    // automatically once the next release succeeds.
    if (entry.last_release_error) {
        html += '<div class="os-release-error-chip" style="' +
            'margin:8px 0;padding:10px 14px;border-radius:6px;' +
            'background:#3a1f1a;color:#ffb3a8;border:1px solid #6a3028;' +
            'font-size:13px;line-height:1.4">' +
            '<strong>Release error:</strong> ' + esc(entry.last_release_error) +
            '</div>';
    }

    // UOP-accumulating chip (P2-C8): counts are piling up on this node while no
    // bin is bound. Under the configured wait that is a swap in progress and the
    // chip is informational; past it the sentence adds the fix ("... Record
    // Count on the bin tab.") and the chip goes amber. Rendered verbatim from
    // the server, so the sentence is the single source of the wording — the
    // presence of the call to action is what the styling keys on. Clears
    // automatically once a bin binds (Record Count / the next delivery).
    //
    // CAVEAT WORTH KNOWING: this notice needs the count to keep RISING. A
    // stranded carrier on a stopped line goes flat and the chip disappears on
    // its own. "Chip gone" therefore does not mean "resolved" — which is part of
    // why the wording no longer implies anything is still being watched.
    if (entry.stranded_alarm) {
        const asking = entry.stranded_alarm.indexOf('Record Count') !== -1;
        html += '<div class="os-stranded-chip" style="' +
            'margin:8px 0;padding:10px 14px;border-radius:6px;' +
            (asking
                ? 'background:#3a2f14;color:#ffd98a;border:1px solid #6a5322;'
                : 'background:#1f2a36;color:#dbe7f5;border:1px solid #35485e;') +
            'font-size:13px;line-height:1.4">' +
            esc(entry.stranded_alarm) +
            '</div>';
    }

    // Skip-note chip: surfaces when a linked changeover complex order
    // reached terminal "skipped" — Core's no_source_bin path (the source
    // node was emptied externally, e.g. operator pulled the bin to quality
    // hold before the evac dispatched). The changeover state machine has
    // already advanced past this leg; the chip exists so the operator
    // knows the auto-skip happened and can recover manually if the
    // physical world doesn't actually match. Cleared by the next
    // state-advancing operator action.
    const skipNote = entry.changeover_task && entry.changeover_task.skip_note;
    if (skipNote) {
        html += '<div class="os-skip-note-chip" style="' +
            'margin:8px 0;padding:10px 14px;border-radius:6px;' +
            'background:#2a2410;color:#f5d97a;border:1px solid #5a4a1a;' +
            'font-size:13px;line-height:1.4">' +
            '<strong>Auto-skipped:</strong> ' + esc(skipNote) +
            ' — recover manually if needed.' +
            '</div>';
    }

    // Actions — state machine: only show the next step in the cycle.
    // Consume:  IDLE → REQUEST MATERIAL → (stage) → RELEASE → (drop) → CONFIRM
    // Produce:  same but REQUEST SWAP (the /finalize route) when node has parts;
    //           the manifest count locks at RELEASE, not here (Fix D).
    html += '<div class="modal-actions">';

    // CHILD TILE — this node is shown here only because the node it extends
    // lives on this station (a press-index seat with no station of its own).
    // It owns no changeover task and no orders, so there is NOTHING to release
    // from here. Offering a release button would either no-op or, worse, act on
    // the parent's work from a tile that does not represent it. Render the
    // relationship and stop; the seat is visible (which is the point — it used
    // to be invisible and got fork-trucked) but not actionable.
    if (entry.child_of_node) {
        html += '<div style="padding:12px 16px;border-radius:8px;background:#1a1a1a;border:1px solid #444;color:#aab;font-size:14px;line-height:1.5">' +
            'Indexed-over position of <strong>' + esc(entry.child_of_node) + '</strong>.' +
            '<div style="color:#888;font-size:12px;margin-top:4px">' +
            'The changeover moves a bin through this seat. There is nothing to release here — ' +
            'work it from ' + esc(entry.child_of_node) + '.</div></div>';
    } else if (claim) {
        if (claim.swap_mode === 'manual_swap') {
            const binState = entry.bin_state;
            const hasBin = binState && binState.occupied;
            const allowed = (claim.allowed_payload_codes && claim.allowed_payload_codes.length > 0)
                ? claim.allowed_payload_codes
                : (claim.payload_code ? [claim.payload_code] : []);

            const activeOrders = (entry.orders || []).filter(o => isActive(o.status));
            const hasDemand = activeOrders.length > 0;
            const delivered = activeOrders.find(o => o.status === 'delivered');
            // Keep acknowledged separate from in_transit: a robot actually en
            // route is "IN TRANSIT"; an acknowledged order (fleet accepted,
            // pre-sourcing) is NOT in transit and gets its own summary line.
            const inTransit = activeOrders.find(o => o.status === 'in_transit');
            const acknowledged = activeOrders.find(o => o.status === 'acknowledged');
            const queued = activeOrders.filter(o => o.status === 'queued' || o.status === 'pending');
            // Loader can fill a parked empty bin even without a delivered L1
            // order. Post-2026-05-12, L1 retrieve_empty IS created when demand
            // fires but Core queues it (dropoff-capacity gate) until the
            // parked bin clears — so the demand-card sits in 'queued', not
            // 'delivered', while the bin is right there ready to fill.
            // LoadBin's no-L1 fallback (operator_bin_ops.go:94) creates the
            // L2 move-out directly, so any allowed payload is a valid pick.
            // Treat each card as if it were delivered for the purpose of
            // click-handling.
            const parkedEmptyAtLoader = claim.role === 'produce' &&
                hasBin && binState && !binState.payload_code;

            // Symmetric unloader case: full bin parked at unloader, U1 was
            // skipped (unloaderHasUsableFullPresent). No operator click action
            // is needed — the bin drains via PLC counters as parts are pulled
            // downstream — but the matching payload card should signal "this
            // is here, ready" instead of leaving the operator to think the
            // demand is unmet.
            const parkedFullPayload = (claim.role === 'consume' && hasBin && binState && binState.payload_code)
                ? binState.payload_code : null;

            html += '<div class="os-demand-queue">';
            html += '<div style="font-size:13px;color:#999;margin-bottom:8px;text-transform:uppercase;letter-spacing:1px">';
            html += claim.role === 'produce' ? 'Load Queue' : 'Unload Queue';
            html += '</div>';

            if (delivered) {
                html += '<div style="background:#1a3a1a;border:1px solid #2a5a2a;border-radius:6px;padding:10px;margin-bottom:10px;display:flex;align-items:center;gap:8px">';
                html += '<span style="font-size:14px;font-weight:700;color:#6f6">[READY]</span>';
                html += '<span style="color:#6f6;font-weight:600">Bin delivered - ready for ' + (claim.role === 'produce' ? 'loading' : 'unloading') + '</span>';
                html += '</div>';
            } else if (inTransit) {
                html += '<div style="background:#2a2a1a;border:1px solid #5a5a2a;border-radius:6px;padding:10px;margin-bottom:10px;display:flex;align-items:center;gap:8px">';
                html += '<span style="font-size:14px;font-weight:700;color:#ff6">[IN TRANSIT]</span>';
                html += '<span style="color:#ff6;font-weight:600">Robot in transit</span>';
                html += '</div>';
            } else if (acknowledged) {
                html += '<div style="background:#1a2a4a;border:1px solid #3a5a8a;border-radius:6px;padding:10px;margin-bottom:10px;display:flex;align-items:center;gap:8px">';
                html += '<span style="font-size:14px;font-weight:700;color:#8af">[ACKNOWLEDGED]</span>';
                html += '<span style="color:#8af;font-weight:600">Order accepted - awaiting dispatch</span>';
                html += '</div>';
            }
            if (queued.length > 0) {
                html += '<div style="color:#999;font-size:12px;margin-bottom:10px">' + queued.length + ' order' + (queued.length > 1 ? 's' : '') + ' queued</div>';
            }

            var queuePos = 1;
            allowed.forEach(function(code) {
                var payloadOrders = activeOrders.filter(function(o) { return o.payload_code === code; });
                // Mirror operator-render.js: the no-payload-code fallback is
                // for the empty-bin-parked demand-signaling phase only. After
                // load (bin has payload_code) or once any active order has a
                // payload_code, fall back to the strict per-payload match.
                var nodeBinIsEmpty = !!(binState && binState.occupied && !binState.payload_code);
                var isActive = payloadOrders.length > 0 || (nodeBinIsEmpty && hasDemand && activeOrders.every(function(o) { return !o.payload_code; }));
                var payloadDelivered = payloadOrders.find(function(o) { return o.status === 'delivered'; });
                // Keep acknowledged separate from in_transit: the card renders a
                // real transit order as IN TRANSIT and an acknowledged order as
                // its own ACKNOWLEDGED step, not a moving robot.
                var payloadInTransit = payloadOrders.find(function(o) { return o.status === 'in_transit'; });
                var payloadAcknowledged = payloadOrders.find(function(o) { return o.status === 'acknowledged'; });
                var payloadQueued = payloadOrders.find(function(o) { return o.status === 'queued' || o.status === 'pending' || o.status === 'submitted'; });

                var parkedFullThisCode = parkedFullPayload === code;
                // BANDAID — pull this manual-request path when proper demand
                // signals land for manual_swap loaders / unloaders.
                //
                // Idle-state clickability: when there's no bin and no order
                // for this payload, let the operator tap the card to issue a
                // Request Empty (produce loader) or Request Full (consume
                // unloader). Otherwise the manual_swap HMI has no way to
                // create demand from the station — operators get a wall of
                // "no demand" cards with nothing actionable.
                //
                // Long term, manual_swap demand should flow in automatically
                // (auto_request_payload, kanban / lineside demand signals,
                // upstream consume-side reorder), so the operator never has
                // to manually request a bin from this screen. Once that's
                // wired up, the canRequest branch — and the matching
                // /request-empty / /request-full URLs below — becomes dead
                // code and should be deleted instead of being left as a
                // permanent escape hatch.
                var idleNoDemand = !hasBin && !payloadOrders.length && !hasDemand;
                var canRequest = idleNoDemand;
                var clickable = !!payloadDelivered || parkedEmptyAtLoader || canRequest;
                var requestUrl = '';
                if (canRequest) {
                    requestUrl = claim.role === 'produce'
                        ? '/api/process-nodes/' + entry.node.id + '/request-empty|' + code
                        : '/api/process-nodes/' + entry.node.id + '/request-full|' + code;
                }
                var cardBg, cardBorder, cardOpacity, cardCursor;
                if (payloadDelivered) {
                    cardBg = '#1a3a1a'; cardBorder = '#2a5a2a'; cardOpacity = '1'; cardCursor = 'pointer';
                } else if (parkedEmptyAtLoader) {
                    cardBg = '#1a3a1a'; cardBorder = '#2a5a2a'; cardOpacity = '1'; cardCursor = 'pointer';
                } else if (parkedFullThisCode) {
                    cardBg = '#1a3a1a'; cardBorder = '#2a5a2a'; cardOpacity = '1'; cardCursor = 'default';
                } else if (payloadInTransit) {
                    cardBg = '#2a2a1a'; cardBorder = '#5a5a2a'; cardOpacity = '1'; cardCursor = 'default';
                } else if (payloadAcknowledged) {
                    cardBg = '#1a2a4a'; cardBorder = '#3a5a8a'; cardOpacity = '1'; cardCursor = 'default';
                } else if (isActive) {
                    cardBg = '#1a2a4a'; cardBorder = '#3a5a8a'; cardOpacity = '1'; cardCursor = 'default';
                } else if (canRequest) {
                    cardBg = '#1a1a1a'; cardBorder = '#555'; cardOpacity = '1'; cardCursor = 'pointer';
                } else {
                    cardBg = '#1a1a1a'; cardBorder = '#333'; cardOpacity = '0.5'; cardCursor = 'default';
                }
                var cardStyle = 'background:' + cardBg + ';border:1px solid ' + cardBorder + ';opacity:' + cardOpacity + ';cursor:' + cardCursor;
                html += '<div class="os-demand-card" style="border-radius:8px;padding:12px 16px;margin-bottom:8px;display:flex;align-items:center;justify-content:space-between;' + cardStyle + '"';
                if (clickable) {
                    if (canRequest && !payloadDelivered && !parkedEmptyAtLoader) {
                        html += ' data-action="' + esc(requestUrl) + '"';
                    } else {
                        html += ' data-action="demand-card:' + esc(code) + '"';
                    }
                }
                html += '>';

                html += '<div style="display:flex;align-items:center;gap:12px">';
                if (isActive) {
                    html += '<span style="background:' + (payloadDelivered ? '#2a5a2a' : payloadInTransit ? '#5a5a2a' : '#3a5a8a') + ';color:#fff;border-radius:50%;width:28px;height:28px;display:flex;align-items:center;justify-content:center;font-weight:700;font-size:14px">' + queuePos + '</span>';
                    queuePos++;
                } else {
                    html += '<span style="width:28px"></span>';
                }
                html += '<span style="font-size:18px;font-weight:600;color:' + (isActive ? '#fff' : '#666') + '">' + esc(code) + '</span>';
                html += '</div>';

                var labelColor = payloadDelivered || parkedEmptyAtLoader || parkedFullThisCode ? '#6f6'
                    : payloadInTransit ? '#ff6'
                    : isActive ? '#8af'
                    : canRequest ? '#aaa'
                    : '#555';
                html += '<div style="font-size:12px;color:' + labelColor + '">';
                if (payloadDelivered) {
                    html += 'DELIVERED';
                } else if (parkedEmptyAtLoader) {
                    html += 'TAP TO LOAD';
                } else if (parkedFullThisCode) {
                    html += 'BIN READY';
                } else if (payloadInTransit) {
                    html += 'IN TRANSIT';
                } else if (payloadAcknowledged) {
                    html += 'ACKNOWLEDGED';
                } else if (payloadQueued) {
                    html += 'QUEUED';
                } else if (isActive) {
                    html += 'active demand';
                } else if (canRequest) {
                    html += claim.role === 'produce' ? 'TAP TO REQUEST EMPTY' : 'TAP TO REQUEST FULL';
                } else {
                    html += 'no demand';
                }
                html += '</div>';
                html += '</div>';
            });

            if (allowed.length === 0) {
                html += '<div style="color:#666;font-style:italic;padding:12px">No payloads configured</div>';
            }

            html += '</div>'; // close demand queue

            if (delivered) {
                // Guard delivered.id before building the URL: a half-built
                // complex order (Order B never created) can leave the order
                // record with a missing/zero ID. Posting that to chi yields
                // a default 404 from an unmatched route, which surfaces as
                // a useless "404 page not found" toast. Render the button
                // disabled so the operator refreshes instead of hammering.
                if (Number.isInteger(delivered.id) && delivered.id > 0) {
                    html += actionBtn('CONFIRM DELIVERY', 'request', true,
                        '/api/confirm-delivery/' + delivered.id);
                } else {
                    html += actionBtn('CONFIRM (refresh)', 'close', false, '');
                }
            }

            if (hasBin && remaining > 0) {
                html += actionBtn('CLEAR BIN', 'empty-tools', true,
                    '/api/process-nodes/' + entry.node.id + '/clear-bin');
            }
        } else {
            const orders = entry.orders || [];
            const active = orders.filter(o => isActive(o.status));
            const staged = active.find(o => o.status === 'staged');
            const delivered = active.find(o => o.status === 'delivered');
            const inFlight = active.find(o => !staged && !delivered);

            if (entry.swap_ready) {
                // Two-robot swap: lineside robot has reached its wait point.
                // One click releases both legs unconditionally regardless of
                // Order A's state. swap_ready is the single gate (see
                // store/station_views.go ComputeSwapReady).
                html += actionBtn('RELEASE', 'request', true,
                    'release-prompt:/api/process-nodes/' + entry.node.id + '/release-staged');
            } else if (claim && claim.swap_mode === 'two_robot' && active.length >= 2) {
                // Two-robot swap in progress with BOTH legs still alive but
                // swap_ready is false — Robot B hasn't reached its wait point.
                // Show explicit waiting state instead of the per-order RELEASE
                // branch (would release one leg, bypass disposition prompt) or
                // idle REQUEST SWAP/REQUEST (don't apply mid-swap).
                //
                // The active.length>=2 guard is the recovery surface: if one
                // leg is cancelled/failed (active drops to <=1 because the
                // filter on line ~225 strips terminal statuses), there's no
                // swap to coordinate. Fall through to the surviving leg's
                // staged/delivered/inFlight branch so the operator isn't
                // permanently stuck on a disabled WAITING button.
                //
                // The label carries the REASON (waitingLabel): this arm sits
                // above the inFlight arm, so without it the operator gets the
                // one label in this chain that explains nothing. The blocker is
                // the leg that is not parked — the robot they are waiting on.
                const blocker = active.find(o => o.status !== 'staged') || null;
                html += actionBtn(waitingLabel(blocker), 'close', false, '');
            } else if (staged) {
                // Sequential / single-robot — single staged, single release.
                html += actionBtn('RELEASE', 'request', true,
                    'release-prompt:/api/orders/' + staged.id + '/release');
            } else if (delivered) {
                var confirmLabel = 'CONFIRM';
                var binState = entry.bin_state;
                if (binState && binState.manifest) {
                    try {
                        var mf = JSON.parse(binState.manifest);
                        if (Array.isArray(mf) && mf.length > 0) {
                            var totalQty = mf.reduce(function(sum, item) { return sum + (item.quantity || 0); }, 0);
                            confirmLabel = 'CONFIRM: ' + mf.length + (mf.length === 1 ? ' part' : ' parts') + ', qty ' + totalQty;
                        }
                    } catch (err) {
                        console.error('renderModal manifest parse', err);
                    }
                }
                if (Number.isInteger(delivered.id) && delivered.id > 0) {
                    html += actionBtn(confirmLabel, 'request', true,
                        '/api/confirm-delivery/' + delivered.id);
                } else {
                    // Same guard as the manual_swap branch above: a
                    // half-built complex order can carry a delivered status
                    // with a missing/zero ID. Render disabled so the operator
                    // refreshes rather than hits the chi 404 path.
                    html += actionBtn('CONFIRM (refresh)', 'close', false, '');
                }
            } else if (inFlight) {
                // Disabled button when any non-staged/non-delivered active
                // order exists for this node. Catches the duplicate-order
                // case (operator presses swap, sees nothing happen because
                // it's queued, presses again) without any backend dedup —
                // the HMI just won't let them re-submit while one is in
                // flight. Backend safety-net dedup is a separate Core
                // concern; this is the cheap visible-to-the-operator fix.
                //
                // Status-aware label so a queued order doesn't pretend a
                // robot is moving when capacity gating is actually what's
                // holding it. queue_reason is pushed by Core via OrderUpdate
                // and stored on the edge order row; show it when available.
                if (inFlight.status === 'queued') {
                    var queueLabel = inFlight.queue_reason
                        ? 'IN QUEUE: ' + inFlight.queue_reason
                        : 'IN QUEUE';
                    html += actionBtn(queueLabel, 'close', false, '');
                } else if (inFlight.status === 'acknowledged') {
                    // acknowledged is Core's intake ack, pre-sourcing — not a
                    // moving robot. Show its own label instead of pretending a
                    // robot is in transit.
                    html += actionBtn('ACKNOWLEDGED', 'close', false, '');
                } else if (inFlight.status === 'sourcing') {
                    // Core is acquiring reservations/confirmations — same
                    // pre-fleet family as queued; surface the queue_reason when
                    // Core sent one.
                    var sourceLabel = inFlight.queue_reason
                        ? 'SOURCING: ' + inFlight.queue_reason
                        : 'SOURCING';
                    html += actionBtn(sourceLabel, 'close', false, '');
                } else {
                    html += actionBtn('ROBOT IN TRANSIT', 'close', false, '');
                }
            } else {
                if (claim.role === 'produce' && remaining > 0) {
                    html += actionBtn('REQUEST SWAP', 'finalize', true,
                        '/api/process-nodes/' + entry.node.id + '/finalize');
                } else if (claim.role === 'produce') {
                    // remaining=0: operator brings an empty bin to the press.
                    // /request-empty issues a retrieve order for an empty
                    // compatible with one of the allowed payloads.
                    var allowed = claim.allowed_payload_codes || (claim.payload_code ? [claim.payload_code] : []);
                    if (allowed.length > 0) {
                        html += actionBtn('REQUEST EMPTY', 'request', true,
                            '/api/process-nodes/' + entry.node.id + '/request-empty|' + allowed[0]);
                    }
                } else {
                    html += actionBtn('REQUEST MATERIAL', 'request', true,
                        '/api/process-nodes/' + entry.node.id + '/request');
                }
                // RELEASE EMPTY and RELEASE PARTIAL removed from operator HMI;
                // backend endpoints stay for changeover/supervisor use.
            }
        }
    }

    // 4-step CO ladder (STAGE → EMPTY → RELEASE → SWITCH) only renders for
    // tool-change-style flows where the operator must manually advance each
    // phase. For a regular swap with no tool change, the top-region RELEASE
    // (swap_ready path) does the whole thing in one click; showing the ladder
    // implies a workflow the operator shouldn't be doing and makes EMPTY FOR
    // TOOL CHANGE clickable when no tool change is actually required.
    //
    // Marker logic:
    //   - situation='evacuate': by construction the to-claim has
    //     EvacuateOnChangeover=true (that's what makes a swap into an
    //     evacuate per changeover.go:91). Always show.
    //   - situation='drop' + from-claim has EvacuateOnChangeover=true: the
    //     operator marked this node for tool-change-style evacuation
    //     (piece-1 fix keeps the task at empty_requested until pickup).
    //   - Otherwise: hide. The simple top-region RELEASE covers the case.
    const needsToolChangeFlow = task && (
        task.situation === 'evacuate' ||
        (task.situation === 'drop' && claim && claim.evacuate_on_changeover)
    );
    if (task && needsToolChangeFlow) {
        const view = getView();
        const pid = view.process.id;
        const nid = entry.node.id;
        const hasTarget = !!entry.target_claim;

        html += '<div class="modal-divider"></div>';

        html += actionBtn('STAGE NEXT MATERIAL', 'stage',
            task.state === 'pending' && hasTarget,
            '/api/processes/' + pid + '/changeover/stage-node/' + nid);

        html += actionBtn('EMPTY FOR TOOL CHANGE', 'empty-tools',
            task.state === 'staging_requested',
            '/api/processes/' + pid + '/changeover/evacuate-node/' + nid);

        html += actionBtn('RELEASE INTO PRODUCTION', 'release-production',
            task.state === 'empty_requested',
            '/api/processes/' + pid + '/changeover/deliver-material/' + nid);

        html += actionBtn('SWITCH TO TARGET', 'switch-target',
            task.state === 'release_requested' || task.state === 'released',
            '/api/processes/' + pid + '/changeover/switch-node/' + nid);
    }

    html += '</div>'; // close actions

    html += '<div class="modal-actions" style="margin-top:12px">';
    html += '<button type="button" class="os-action-btn close" data-action="close">CLOSE</button>';
    html += '</div>';

    // THE SUPPLY REFUSAL, BELOW EVERYTHING ELSE AND BEHIND A RULE.
    //
    // Every other control on this modal acts on the material in front of the
    // operator. This one tells somebody at another node that their parts are not
    // coming, which is a different kind of act, so it does not share a row with
    // them. Last and separated, not hidden: reaching it must never require
    // knowing it is there.
    //
    // Only on loader windows, matching what the server will accept — a node that
    // is not a manual-swap window renders no loader card, so a refusal against it
    // could not have come from a person looking at one.
    if (claim && claim.swap_mode === 'manual_swap') {
        const refusable = (claim.allowed_payload_codes && claim.allowed_payload_codes.length > 0)
            ? claim.allowed_payload_codes
            : (claim.payload_code ? [claim.payload_code] : []);
        if (refusable.length > 0) {
            const standing = entry.supply_refusals || {};
            html += '<div class="modal-divider"></div>';
            refusable.forEach(function(code) {
                const refused = !!standing[code];
                // The part is named on the button whenever the window carries more
                // than one, because on a shared window "NO PARTS AVAILABLE" alone
                // does not say which part and the operator would be guessing.
                const suffix = refusable.length > 1 ? ' — ' + esc(code) : '';
                html += '<button type="button" class="os-board-refusal-btn"' +
                    (refused ? ' data-refused="1"' : '') +
                    ' data-action="supply-' + (refused ? 'undo' : 'refuse') + ':' + esc(code) + '">' +
                    (refused ? UNDO_LABEL : REFUSE_LABEL) + suffix +
                    '</button>';
            });
        }
    }

    nodeModalContent.innerHTML = html;

    nodeModalContent.querySelectorAll('[data-action]').forEach(btn => {
        btn.addEventListener('click', handleModalAction);
    });
}

// waitingLabel explains WHY a two-robot swap is not releasable yet, reading it
// off the leg that is not yet parked.
//
// THE BRANCH THIS SERVES USED TO SAY NOTHING. The two-robot "waiting" arm sits
// ABOVE the inFlight arm in renderModal's if/else chain, and inFlight is the
// only place that renders queue_reason / ACKNOWLEDGED / SOURCING / ROBOT IN
// TRANSIT. So for a two-robot swap — the one case where the operator cannot act
// and most needs to know why — the chain terminated on a bare "WAITING FOR
// OTHER ROBOT" and every explanatory label below was unreachable.
//
// Springfield ALN_003, 2026-07-31: 32 minutes of bare "WAITING FOR OTHER ROBOT"
// while the supply leg fought three navigation faults the Edge had already
// recorded, then the abandon sweep took both legs. Measured again the same day
// on the next pair: 38 minutes reading "acknowledged" on the board while Core
// held it queued on `waiting_for_material` with no bin in the pool.
//
// queue_reason is preferred over the status word because it is a whole sentence
// from Core ("Waiting for material: 74577-6SA0A.06") and because it survives
// the status-write path independently — SetOrderQueueReason bypasses the
// transition validator, so the reason lands on the Edge row even in the window
// where the status push itself was refused.
function waitingLabel(blocker) {
    const base = 'WAITING FOR OTHER ROBOT';
    if (!blocker) return base;
    if (blocker.queue_reason) return base + ' — ' + blocker.queue_reason;
    switch (blocker.status) {
        case 'faulted':
            return base + ' — faulted, recovering';
        case 'queued':
            return base + ' — queued';
        case 'sourcing':
            return base + ' — sourcing';
        case 'acknowledged':
            return base + ' — acknowledged, not yet dispatched';
        case 'in_transit': {
            const eta = formatETA(blocker.eta);
            return eta.empty ? base + ' — in transit' : base + ' — ' + eta.text;
        }
        default:
            return base;
    }
}

function actionBtn(label, cls, enabled, action) {
    return '<button type="button" class="os-action-btn ' + cls + '"' +
        (!enabled ? ' disabled' : '') +
        ' data-action="' + esc(action) + '">' + esc(label) + '</button>';
}

// Verb dispatch table. Adding a new prefix-action means one entry here.
// Each handler receives (arg, evt, action) where arg is everything after
// the first colon; action is the full data-action string.
const ACTION_HANDLERS = {
    'close': () => closeModal(),

    'demand-card': (code) => {
        const sid = getSelectedNodeID();
        const entry = sid !== null ? findNodeByID(sid) : null;
        if (!entry) return;
        const claim = entry.active_claim;
        closeModal();
        openLoadBin(entry.node.id, [code], claim ? claim.uop_capacity || 0 : 0);
    },

    'keypad': (arg) => {
        const parts = arg.split(':');
        const nodeID = parseInt(parts[0], 10);
        const remaining = parseInt(parts[1], 10) || 0;
        closeModal();
        openKeypad(nodeID, remaining);
    },

    'release-prompt': (url) => {
        const sid = getSelectedNodeID();
        const entry = sid !== null ? findNodeByID(sid) : null;
        openReleasePrompt(url, entry);
    },

    // Both refusal verbs close the modal and reload the board on success: the
    // card behind it is about to change state, and leaving a stale modal over a
    // card that now says something different is how an operator ends up acting
    // twice.
    'supply-refuse': (code) => {
        const sid = getSelectedNodeID();
        const entry = sid !== null ? findNodeByID(sid) : null;
        if (!entry) return;
        confirmRefuseSupply(entry.node.id, code, () => { closeModal(); if (loadViewRef) loadViewRef(); });
    },

    'supply-undo': (code) => {
        const sid = getSelectedNodeID();
        const entry = sid !== null ? findNodeByID(sid) : null;
        if (!entry) return;
        confirmUndoSupplyRefusal(entry.node.id, code, () => { closeModal(); if (loadViewRef) loadViewRef(); });
    },

    'stranded-chip': (arg) => {
        const bucketID = parseInt(arg, 10);
        const sid = getSelectedNodeID();
        const entry = sid !== null ? findNodeByID(sid) : null;
        if (!entry) return;
        const bucket = (entry.lineside_inactive || []).find(b => b.id === bucketID);
        if (bucket) openStrandedStub(bucket, handleModalAction);
    },
};

export async function handleModalAction(evt) {
    const action = evt.currentTarget.dataset.action;
    if (!action) return;

    // Split on first colon — arg may itself contain colons (URLs do).
    const colon = action.indexOf(':');
    const verb = colon === -1 ? action : action.slice(0, colon);
    const arg = colon === -1 ? '' : action.slice(colon + 1);

    const handler = ACTION_HANDLERS[verb];
    if (handler) {
        await handler(arg, evt, action);
        return;
    }

    // Default branch: POST to action URL. url|body_json format encodes a
    // payload in the data-action string.
    evt.currentTarget.disabled = true;
    let url = action;
    let body = undefined;
    if (action.includes('|')) {
        const parts = action.split('|');
        url = parts[0];
        body = { payload_code: parts[1] };
    }
    const ok = await postAction(url, body, loadViewRef);
    if (ok) closeModal();
}

nodeModal.addEventListener('click', evt => {
    if (evt.target === nodeModal) closeModal();
});
