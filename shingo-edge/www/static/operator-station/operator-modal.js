import { esc, fillColor, postAction } from './operator-util.js';
import {
    getView, getSelectedNodeID, setSelectedNodeID,
    findNodeByID, isReplenishing,
} from './operator-state.js';
import { openLoadBin } from './operator-load-bin.js';
import { openKeypad } from './operator-keypad.js';
import { openReleasePrompt, openStrandedStub } from './operator-release.js';

const nodeModal = document.getElementById('node-modal');
const nodeModalContent = document.getElementById('node-modal-content');

let loadViewRef = null;
export function setModalLoadView(fn) { loadViewRef = fn; }

export function openModal(nodeID) {
    const entry = findNodeByID(nodeID);
    if (!entry) return;
    setSelectedNodeID(nodeID);
    renderModal(entry);
    nodeModal.hidden = false;
}

export function closeModal() {
    setSelectedNodeID(null);
    nodeModal.hidden = true;
}

export function renderModal(entry) {
    const claim = entry.active_claim;
    const runtime = entry.runtime || {};
    const remaining = runtime.remaining_uop != null ? runtime.remaining_uop : 0;
    const capacity = claim ? claim.uop_capacity : 0;
    const pct = capacity > 0 ? Math.min(remaining / capacity, 1) : 0;
    const task = entry.changeover_task;

    let html = '';

    html += '<div class="os-modal-header">';
    html += '<div class="os-modal-node-name">' + esc(entry.node.name) + '</div>';

    if (claim && claim.swap_mode === 'manual_swap') {
        const binState = entry.bin_state;
        const binLabel = binState && binState.bin_label ? binState.bin_label : 'No bin';
        const binPayload = binState && binState.payload_code ? binState.payload_code : '';
        const roleLabel = claim.role === 'produce' ? 'Loader' : 'Unloader';
        html += '<div class="os-modal-payload">' + roleLabel + ' - Bin: ' + esc(binLabel) + (binPayload ? ' (' + esc(binPayload) + ')' : '') + '</div>';
        html += '<div class="os-modal-fill-row">';
        html += '<div class="os-modal-fill-text" style="font-size:18px;font-weight:600">' + (remaining > 0 ? 'LOADED (' + remaining + ' UOP)' : 'EMPTY') + '</div>';
        html += '</div>';
    } else {
        const binState = entry.bin_state;
        const binLabel = binState && binState.bin_label ? ' - Bin: ' + esc(binState.bin_label) : '';
        html += '<div class="os-modal-payload">' + esc(claim ? claim.payload_code || 'Unassigned' : 'No claim') + binLabel + '</div>';
        html += '<div class="os-modal-fill-row">';
        html += '<div class="os-modal-fill-bar"><div class="os-modal-fill-level" style="width:' + Math.round(pct * 100) + '%;background:' + fillColor(pct, remaining) + '"></div></div>';
        html += '<div class="os-modal-fill-text">' + remaining + ' / ' + capacity + '</div>';
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
        const activeOrders = (entry.orders || []).filter(o => o.status !== 'confirmed' && o.status !== 'cancelled' && o.status !== 'failed');
        const statusText = activeOrders.length > 0
            ? activeOrders.map(o => o.order_type + ': ' + o.status).join(', ')
            : 'Order in progress';
        html += '<div class="os-modal-status">[REP] ' + esc(statusText) + '</div>';
    } else {
        html += '<div class="os-modal-status">No active orders</div>';
    }

    if (task) {
        html += '<div class="os-modal-co-info">[CO] Changeover: ' + esc(task.situation) + ' - ' + esc(task.state) + '</div>';
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

    // Actions — state machine: only show the next step in the cycle.
    // Consume:  IDLE → REQUEST MATERIAL → (stage) → RELEASE → (drop) → CONFIRM
    // Produce:  same but FINALIZE instead of REQUEST when node has parts.
    html += '<div class="os-modal-actions">';

    if (claim) {
        if (claim.swap_mode === 'manual_swap') {
            const binState = entry.bin_state;
            const hasBin = binState && binState.occupied;
            const allowed = (claim.allowed_payload_codes && claim.allowed_payload_codes.length > 0)
                ? claim.allowed_payload_codes
                : (claim.payload_code ? [claim.payload_code] : []);

            const activeOrders = (entry.orders || []).filter(o =>
                o.status !== 'confirmed' && o.status !== 'cancelled' && o.status !== 'failed');
            const hasDemand = activeOrders.length > 0;
            const delivered = activeOrders.find(o => o.status === 'delivered');
            const inTransit = activeOrders.find(o => o.status === 'in_transit' || o.status === 'acknowledged');
            const queued = activeOrders.filter(o => o.status === 'queued' || o.status === 'pending');

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
            }
            if (queued.length > 0) {
                html += '<div style="color:#999;font-size:12px;margin-bottom:10px">' + queued.length + ' order' + (queued.length > 1 ? 's' : '') + ' queued</div>';
            }

            var queuePos = 1;
            allowed.forEach(function(code) {
                var payloadOrders = activeOrders.filter(function(o) { return o.payload_code === code; });
                var isActive = payloadOrders.length > 0 || (hasDemand && activeOrders.every(function(o) { return !o.payload_code; }));
                var payloadDelivered = payloadOrders.find(function(o) { return o.status === 'delivered'; });
                var payloadInTransit = payloadOrders.find(function(o) { return o.status === 'in_transit' || o.status === 'acknowledged'; });
                var payloadQueued = payloadOrders.find(function(o) { return o.status === 'queued' || o.status === 'pending' || o.status === 'submitted'; });

                var cardBg, cardBorder, cardOpacity, cardCursor;
                if (payloadDelivered) {
                    cardBg = '#1a3a1a'; cardBorder = '#2a5a2a'; cardOpacity = '1'; cardCursor = 'pointer';
                } else if (payloadInTransit) {
                    cardBg = '#2a2a1a'; cardBorder = '#5a5a2a'; cardOpacity = '1'; cardCursor = 'default';
                } else if (isActive) {
                    cardBg = '#1a2a4a'; cardBorder = '#3a5a8a'; cardOpacity = '1'; cardCursor = 'default';
                } else {
                    cardBg = '#1a1a1a'; cardBorder = '#333'; cardOpacity = '0.5'; cardCursor = 'default';
                }
                var cardStyle = 'background:' + cardBg + ';border:1px solid ' + cardBorder + ';opacity:' + cardOpacity + ';cursor:' + cardCursor;
                html += '<div class="os-demand-card" style="border-radius:8px;padding:12px 16px;margin-bottom:8px;display:flex;align-items:center;justify-content:space-between;' + cardStyle + '"';
                if (payloadDelivered) {
                    html += ' data-action="demand-card:' + esc(code) + '"';
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

                html += '<div style="font-size:12px;color:' + (payloadDelivered ? '#6f6' : payloadInTransit ? '#ff6' : isActive ? '#8af' : '#555') + '">';
                if (payloadDelivered) {
                    html += 'DELIVERED';
                } else if (payloadInTransit) {
                    html += 'IN TRANSIT';
                } else if (payloadQueued) {
                    html += 'QUEUED';
                } else if (isActive) {
                    html += 'active demand';
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
                html += actionBtn('CONFIRM DELIVERY', 'request', true,
                    '/api/confirm-delivery/' + delivered.id);
            }

            if (hasBin && remaining > 0) {
                html += actionBtn('CLEAR BIN', 'empty-tools', true,
                    '/api/process-nodes/' + entry.node.id + '/clear-bin');
            }
        } else {
            const orders = entry.orders || [];
            const active = orders.filter(o => o.status !== 'confirmed' && o.status !== 'cancelled' && o.status !== 'failed');
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
                // idle FINALIZE/REQUEST (don't apply mid-swap).
                //
                // The active.length>=2 guard is the recovery surface: if one
                // leg is cancelled/failed (active drops to <=1 because the
                // filter on line ~225 strips terminal statuses), there's no
                // swap to coordinate. Fall through to the surviving leg's
                // staged/delivered/inFlight branch so the operator isn't
                // permanently stuck on a disabled WAITING button.
                html += actionBtn('WAITING FOR OTHER ROBOT', 'close', false, '');
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
                html += actionBtn(confirmLabel, 'request', true,
                    '/api/confirm-delivery/' + delivered.id);
            } else if (inFlight) {
                html += actionBtn('ROBOT IN TRANSIT', 'close', false, '');
            } else {
                if (claim.role === 'produce' && remaining > 0) {
                    html += actionBtn('FINALIZE', 'finalize', true,
                        '/api/process-nodes/' + entry.node.id + '/finalize');
                } else {
                    html += actionBtn('REQUEST MATERIAL', 'request', true,
                        '/api/process-nodes/' + entry.node.id + '/request');
                }
                // RELEASE EMPTY and RELEASE PARTIAL removed from operator HMI;
                // backend endpoints stay for changeover/supervisor use.
            }
        }
    }

    if (task) {
        const view = getView();
        const pid = view.process.id;
        const nid = entry.node.id;
        const hasTarget = !!entry.target_claim;

        html += '<div class="os-modal-divider"></div>';

        html += actionBtn('STAGE NEXT MATERIAL', 'stage',
            task.state === 'pending' && hasTarget,
            '/api/processes/' + pid + '/changeover/stage-node/' + nid);

        html += actionBtn('EMPTY FOR TOOL CHANGE', 'empty-tools',
            task.state === 'staging_requested',
            '/api/processes/' + pid + '/changeover/empty-node/' + nid);

        html += actionBtn('RELEASE INTO PRODUCTION', 'release-production',
            task.state === 'empty_requested',
            '/api/processes/' + pid + '/changeover/release-node/' + nid);

        html += actionBtn('SWITCH TO TARGET', 'switch-target',
            task.state === 'release_requested' || task.state === 'released',
            '/api/processes/' + pid + '/changeover/switch-node/' + nid);
    }

    html += '</div>'; // close actions

    html += '<div class="os-modal-actions" style="margin-top:12px">';
    html += '<button type="button" class="os-action-btn close" data-action="close">CLOSE</button>';
    html += '</div>';

    nodeModalContent.innerHTML = html;

    nodeModalContent.querySelectorAll('[data-action]').forEach(btn => {
        btn.addEventListener('click', handleModalAction);
    });
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
