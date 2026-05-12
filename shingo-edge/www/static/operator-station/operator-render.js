import { el, esc, fillColor, postAction, showToast } from './operator-util.js';
import { getView, claimedNodes, isReplenishing } from './operator-state.js';
import { isActive } from './order-status.js';

const grid = document.getElementById('os-grid');
const headerInfo = document.getElementById('os-header-info');
const headerActions = document.getElementById('os-header-actions');
const footerStatus = document.getElementById('os-footer-status');
const footerBadge = document.getElementById('os-footer-badge');

let openModalRef = null;
let openLoadBinRef = null;
let loadViewRef = null;

export function setRenderRefs(refs) {
    openModalRef = refs.openModal;
    openLoadBinRef = refs.openLoadBin;
    loadViewRef = refs.loadView;
}

// ─── Header ───

export function renderHeader() {
    const view = getView();
    const style = view.current_style ? view.current_style.name : 'No Style';
    const target = view.target_style ? (' \u2192 ' + view.target_style.name) : '';
    headerInfo.textContent = view.process.name + ' - ' + style + target;

    headerActions.innerHTML = '';

    const badge = el('span', {
        className: 'os-health-badge ' + (view.station.health_status === 'online' ? 'online' : 'offline')
    });
    headerActions.appendChild(badge);

    // Active style chip — sits next to the changeover button so the operator
    // can see which style is running. During changeover shows "current → target".
    const styleName = view.current_style ? view.current_style.name : 'No Style';
    const targetName = view.target_style ? view.target_style.name : null;
    const styleChip = el('div', { className: 'os-header-style' + (targetName ? ' changing' : '') });
    styleChip.appendChild(el('span', { className: 'os-header-style-label', textContent: 'STYLE' }));
    const styleValue = el('span', { className: 'os-header-style-value' });
    styleValue.textContent = targetName ? styleName + ' \u2192 ' + targetName : styleName;
    styleChip.appendChild(styleValue);
    headerActions.appendChild(styleChip);

    // Bin-loader board mode (single manual_swap claim) is operated by a
    // forklift driver — strip the changeover/cutover buttons. Those flows
    // belong on a regular operator station, not this view.
    //
    // HMI Tier 2 (post-F'-Phase-2): the changeover-wide RELEASE header
    // button was removed. Each node tile is the release surface during
    // changeover — operator clicks the tile, gets the same production
    // release modal, picks disposition (or accepts the auto-detected
    // pre-fill), submits. Phase 2's deferred-supply chain auto-releases
    // the supply leg when the evac robot picks up.
    //
    // CUTOVER button: shown only when the process does NOT have PLC-
    // driven cutover enabled (Theme C's auto_cutover_enabled flag). On
    // auto-cutover processes, the PLC's falling edge on Changeover_Active
    // fires CompleteProcessProductionCutoverFromPLC automatically — a
    // manual button creates ambiguity about who's "really" in charge of
    // cutover, and a stuck PLC bit is something to investigate, not to
    // mask with a manual override. Theme B's gate is the safety net for
    // both paths.
    if (!isBoardMode()) {
        if (view.active_changeover) {
            if (!view.process.auto_cutover_enabled) {
                headerActions.appendChild(headerBtn('CUTOVER', 'cutover', confirmCutover));
            }
            // CANCEL during active changeover: aborts every in-flight evac+
            // supply order on the process's node tasks, marks the changeover
            // row cancelled, and resets the process back to active_production
            // on the from-style. Core handles safe resolution of orders
            // already mid-route (queued → disappears, loaded robot → store-
            // order rerouted to a safe drop). Operator wraps this in a
            // confirmation modal because the action is destructive.
            headerActions.appendChild(headerBtn('CANCEL', 'cancel-changeover', confirmCancelChangeover));
        } else {
            headerActions.appendChild(headerBtn('CHANGEOVER', 'changeover', openChangeoverPicker));
        }
    }

    headerActions.appendChild(headerBtn('REFRESH', 'refresh', loadViewRef));
}

function isBoardMode() {
    const nodes = claimedNodes();
    return nodes.length === 1
        && nodes[0].active_claim
        && nodes[0].active_claim.swap_mode === 'manual_swap';
}

function openChangeoverPicker() {
    const view = getView();
    const styles = view.available_styles || [];
    const currentID = view.current_style ? view.current_style.id : null;
    const others = styles.filter(s => s.id !== currentID);
    if (others.length === 0) {
        showToast('No other styles available', 'error');
        return;
    }

    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker' });
    panel.appendChild(el('div', { className: 'os-co-picker-title', textContent: 'Change over to:' }));

    for (const s of others) {
        const btn = el('button', { className: 'os-co-picker-btn', textContent: s.name });
        btn.addEventListener('click', () => {
            overlay.remove();
            startChangeover(s.id, s.name);
        });
        panel.appendChild(btn);
    }

    const cancel = el('button', { className: 'os-co-picker-btn cancel', textContent: 'CANCEL' });
    cancel.addEventListener('click', () => overlay.remove());
    panel.appendChild(cancel);

    overlay.appendChild(panel);
    overlay.addEventListener('click', evt => { if (evt.target === overlay) overlay.remove(); });
    document.body.appendChild(overlay);
}

async function startChangeover(toStyleID, styleName) {
    const view = getView();
    const pid = view.process.id;
    const ok = await postAction('/api/processes/' + pid + '/changeover/start', {
        to_style_id: toStyleID,
        called_by: (view.station.name && view.station.name.trim()) || 'operator',
        notes: ''
    }, loadViewRef);
    if (ok) showToast('Changeover to ' + styleName + ' started', 'success');
}


async function confirmCancelChangeover() {
    const view = getView();
    const pid = view.process.id;
    const co = view.active_changeover;
    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker' });
    panel.appendChild(el('div', { className: 'os-co-picker-title',
        textContent: 'Cancel changeover to ' + (co.to_style_name || 'target') + '?' }));
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
        textContent: 'In-flight robots will be recalled. Loaded bins are routed to safe storage.' }));

    const confirm = el('button', { className: 'os-co-picker-btn danger', textContent: 'CANCEL CHANGEOVER' });
    confirm.addEventListener('click', async () => {
        overlay.remove();
        const ok = await postAction('/api/processes/' + pid + '/changeover/cancel', {}, loadViewRef);
        if (ok) showToast('Changeover cancelled', 'success');
    });
    panel.appendChild(confirm);

    const dismiss = el('button', { className: 'os-co-picker-btn cancel', textContent: 'KEEP CHANGEOVER' });
    dismiss.addEventListener('click', () => overlay.remove());
    panel.appendChild(dismiss);

    overlay.appendChild(panel);
    overlay.addEventListener('click', evt => { if (evt.target === overlay) overlay.remove(); });
    document.body.appendChild(overlay);
}

async function confirmCutover() {
    const view = getView();
    const pid = view.process.id;
    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker' });
    const co = view.active_changeover;
    panel.appendChild(el('div', { className: 'os-co-picker-title',
        textContent: 'Complete cutover to ' + (co.to_style_name || 'target') + '?' }));

    const confirm = el('button', { className: 'os-co-picker-btn', textContent: 'CONFIRM CUTOVER' });
    confirm.addEventListener('click', async () => {
        overlay.remove();
        const ok = await postAction('/api/processes/' + pid + '/changeover/cutover', undefined, loadViewRef);
        if (ok) showToast('Cutover complete', 'success');
    });
    panel.appendChild(confirm);

    const cancel = el('button', { className: 'os-co-picker-btn cancel', textContent: 'CANCEL' });
    cancel.addEventListener('click', () => overlay.remove());
    panel.appendChild(cancel);

    overlay.appendChild(panel);
    overlay.addEventListener('click', evt => { if (evt.target === overlay) overlay.remove(); });
    document.body.appendChild(overlay);
}

function headerBtn(label, cls, onClick) {
    const btn = el('button', { className: 'os-header-btn ' + cls, textContent: label });
    btn.addEventListener('click', onClick);
    return btn;
}

// ─── Grid ───

export function renderGrid() {
    const nodes = claimedNodes();
    grid.innerHTML = '';

    if (nodes.length === 0) {
        grid.classList.remove('os-board-mode');
        document.body.classList.remove('os-board-mode-active');
        grid.style.removeProperty('--os-cols');
        grid.style.removeProperty('--os-rows');
        const empty = el('div', { id: 'os-grid-empty', textContent: 'No claimed nodes' });
        grid.appendChild(empty);
        return;
    }

    // Single manual_swap node: render payload board instead of grid.
    const manualSwapNodes = nodes.filter(function(n) {
        return n.active_claim && n.active_claim.swap_mode === 'manual_swap';
    });
    if (manualSwapNodes.length === 1 && nodes.length === 1) {
        grid.classList.add('os-board-mode');
        document.body.classList.add('os-board-mode-active');
        grid.style.removeProperty('--os-cols');
        grid.style.removeProperty('--os-rows');
        renderPayloadBoard(manualSwapNodes[0]);
        return;
    }

    grid.classList.remove('os-board-mode');
    document.body.classList.remove('os-board-mode-active');
    const { cols, rows } = gridDimensions();
    grid.style.setProperty('--os-cols', cols);
    grid.style.setProperty('--os-rows', rows);

    for (const entry of nodes) {
        grid.appendChild(createNodeButton(entry));
    }
}

function renderPayloadBoard(entry) {
    const claim = entry.active_claim;
    const runtime = entry.runtime || {};
    const remaining = runtime.remaining_uop_cached != null ? runtime.remaining_uop_cached : 0;
    const binState = entry.bin_state;
    const hasBin = binState && binState.occupied;
    const binLabel = binState && binState.bin_label ? binState.bin_label : 'No bin';
    const binPayload = binState && binState.payload_code ? binState.payload_code : '';
    const roleLabel = claim.role === 'produce' ? 'Loader' : 'Unloader';

    var infoBar = el('div', { className: 'os-board-header' });
    infoBar.innerHTML =
        '<div>' +
            '<div style="font-size:42px;font-weight:700;color:#fff">' + esc(entry.node.name) + ' - ' + roleLabel + '</div>' +
            '<div style="font-size:20px;color:#aab;margin-top:6px">Manual Swap | ' +
                (claim.allowed_payload_codes ? claim.allowed_payload_codes.length : 0) + ' payloads configured</div>' +
        '</div>' +
        '<div style="text-align:right">' +
            '<div style="font-size:28px;font-weight:600;color:#fff">Bin: ' + esc(binLabel) + '</div>' +
            (binPayload ? '<div style="font-size:20px;color:#aab;margin-top:4px">' + esc(binPayload) + ' | UOP: ' + remaining + '</div>' : '') +
            '<div style="display:inline-block;font-size:22px;font-weight:700;padding:8px 20px;border-radius:6px;margin-top:8px;' +
                (hasBin ? (binPayload ? 'background:#1a3a1a;color:#6f6' : 'background:#3a1a1a;color:#f88') : 'background:#2a2a1a;color:#ff6') + '">' +
                (hasBin ? (binPayload ? 'LOADED' : 'EMPTY') : 'AWAITING BIN') +
            '</div>' +
        '</div>';
    grid.appendChild(infoBar);

    var allowed = (claim.allowed_payload_codes && claim.allowed_payload_codes.length > 0)
        ? claim.allowed_payload_codes
        : (claim.payload_code ? [claim.payload_code] : []);

    var activeOrders = (entry.orders || []).filter(function(o) {
        return isActive(o.status);
    });
    var hasDemand = activeOrders.length > 0;

    // Multi-payload starvation fix (investigation-r2.md): when an empty bin
    // is at the node and any demand exists, every allowed payload becomes a
    // loadable option. The operator picks at load time and LoadBin re-binds
    // via the request's payload_code argument. Without this only the
    // tagged payload could be loaded, serializing manual_swap nodes.
    var nodeBinIsEmpty = entry.bin_state && entry.bin_state.occupied && !entry.bin_state.payload_code;
    var loadableHere = nodeBinIsEmpty && hasDemand;

    // Empty-bin escape hatch: an empty bin is parked at the node but no
    // demand signal has arrived. Allow the operator to load any allowed
    // payload — server already permits this (LoadBin only checks bin
    // presence + empty payload, not demand).
    var canLoadEmpty = nodeBinIsEmpty && !hasDemand;

    // BANDAID — pull this manual-request path when proper demand signals
    // land for manual_swap loaders / unloaders. Without it the board is a
    // wall of greyed cards with nothing actionable when no kanban has
    // fired yet. Mirrors operator-modal.js (idleNoDemand).
    var canRequestHere = !hasBin && !hasDemand;

    var cardGrid = el('div', { className: 'os-board-cards' });
    var cols = allowed.length <= 3 ? allowed.length : (allowed.length <= 6 ? 3 : 4);
    cardGrid.style.setProperty('--os-board-cols', cols);

    var queuePos = 1;
    allowed.forEach(function(code) {
        var payloadOrders = activeOrders.filter(function(o) { return o.payload_code === code; });
        // The "every order lacks payload_code" fallback is the demand-signaling
        // bandaid for the empty-bin-parked phase: when an empty is at the node
        // and there is general demand but no per-payload binding yet, every
        // allowed payload should light up so the operator can pick. Once a bin
        // is loaded (payload_code set) or after an L2 has been created with a
        // specific payload_code, the fallback must NOT fire — otherwise every
        // tile renders QUEUED while only one payload is actually in flight.
        var hasPayloadDemand = payloadOrders.length > 0 || (nodeBinIsEmpty && hasDemand && activeOrders.every(function(o) { return !o.payload_code; }));
        var isActive = hasPayloadDemand || loadableHere;
        var payloadDelivered = payloadOrders.find(function(o) { return o.status === 'delivered'; });
        var payloadInTransit = payloadOrders.find(function(o) { return o.status === 'in_transit' || o.status === 'acknowledged'; });

        var card = el('div', { className: 'os-board-card' });

        // "Load now" — empty bin physically sitting at the loader AND there's
        // demand for this specific payload. This is the action moment for the
        // operator: an empty has arrived, the system wants this payload,
        // operator picks the bin up and stuffs it. Green glow + pulse layered
        // over the base state class so the operator's eye is drawn here.
        var loadNow = nodeBinIsEmpty && hasPayloadDemand;

        if (payloadDelivered) {
            card.classList.add('os-board-delivered');
        } else if (payloadInTransit) {
            card.classList.add('os-board-transit');
        } else if (hasPayloadDemand) {
            card.classList.add('os-board-queued');
        } else if (canLoadEmpty) {
            card.classList.add('os-board-queued');
        } else if (canRequestHere) {
            card.classList.add('os-board-requestable');
        } else {
            card.classList.add('os-board-nodemand');
        }
        if (loadNow) card.classList.add('os-board-load-now');

        card.appendChild(el('div', { className: 'os-board-code', textContent: code }));

        // Demand count — number of outstanding L1 orders for this payload
        // that are still WAITING in the queue (not yet being moved by a
        // robot). Orders past in_transit/acknowledged/delivered have left
        // the queue and are tracked by the status badge below; counting
        // them here inflates "BINS QUEUED" with orders that are already
        // en route or arrived (plant 2026-05-11 confusion: SMN_001 card
        // showed bins as queued that were actually staged at the line).
        //
        // Filter explicitly by status so the count matches operator
        // expectation ("how many more are waiting to start moving").
        var queuedOrders = payloadOrders.filter(function(o) {
            return o.status !== 'delivered'
                && o.status !== 'acknowledged'
                && o.status !== 'in_transit';
        });
        if (queuedOrders.length > 0) {
            var demandBox = el('div', { className: 'os-board-demand' });
            demandBox.appendChild(el('div', { className: 'os-board-demand-num',
                textContent: String(queuedOrders.length) }));
            demandBox.appendChild(el('div', { className: 'os-board-demand-label',
                textContent: queuedOrders.length === 1 ? 'BIN QUEUED' : 'BINS QUEUED' }));
            card.appendChild(demandBox);
        }

        var statusText, statusClass;
        if (payloadDelivered) {
            statusText = 'DELIVERED'; statusClass = 'os-board-tag-delivered';
        } else if (payloadInTransit) {
            statusText = 'IN TRANSIT'; statusClass = 'os-board-tag-transit';
        } else if (hasPayloadDemand) {
            statusText = 'QUEUED'; statusClass = 'os-board-tag-queued';
        } else if (canLoadEmpty) {
            statusText = 'LOAD'; statusClass = 'os-board-tag-queued';
        } else if (canRequestHere) {
            statusText = 'REQUEST'; statusClass = 'os-board-tag-request';
        } else {
            statusText = 'NO DEMAND'; statusClass = 'os-board-tag-nodemand';
        }
        card.appendChild(el('span', { className: 'os-board-tag ' + statusClass, textContent: statusText }));

        var detailText = '';
        var binIsEmptyForDetail = entry.bin_state && entry.bin_state.occupied && !entry.bin_state.payload_code;
        if (payloadDelivered) {
            detailText = 'Tap to ' + (claim.role === 'produce' ? 'load' : 'unload');
        } else if (binIsEmptyForDetail && (payloadInTransit || (hasPayloadDemand && !payloadDelivered))) {
            detailText = 'Empty bin at node — tap to load';
        } else if (payloadInTransit) {
            detailText = 'Robot en route';
        } else if (hasPayloadDemand) {
            detailText = 'Waiting for robot';
        } else if (canLoadEmpty) {
            detailText = 'Empty bin parked \u2014 tap to load';
        } else if (canRequestHere) {
            detailText = claim.role === 'produce' ? 'Tap to request empty bin' : 'Tap to request full bin';
        } else {
            detailText = 'No kanban signal';
        }
        card.appendChild(el('div', { className: 'os-board-detail', textContent: detailText }));

        if (hasPayloadDemand) {
            var badge = el('span', { className: 'os-board-pos', textContent: String(queuePos) });
            if (payloadDelivered) badge.classList.add('os-board-pos-delivered');
            else if (payloadInTransit) badge.classList.add('os-board-pos-transit');
            else badge.classList.add('os-board-pos-queued');
            card.appendChild(badge);
            queuePos++;
        }

        // Delivered cards interactive only while the bin is still empty —
        // once payload_code is set the bin is loaded and any further tap
        // would re-open Load Bin against an already-loaded carrier (server
        // refuses but the modal would still appear, confusing the operator).
        // Queued/in-transit cards interactive when an empty bin sits at the
        // node so operator can load without waiting for the delivery cycle.
        var hasBinState = !!entry.bin_state;
        var binOccupied = hasBinState && entry.bin_state.occupied;
        var binNoPayload = hasBinState && !entry.bin_state.payload_code;
        var binIsEmpty = binOccupied && binNoPayload;
        var canLoad = (payloadDelivered && binIsEmpty) || (binIsEmpty && isActive) || canLoadEmpty;
        if (canLoad) {
            card.style.cursor = 'pointer';
            card.addEventListener('click', function() {
                openLoadBinRef(entry.node.id, [code], claim.uop_capacity || 0);
            });
        } else if (canRequestHere) {
            card.style.cursor = 'pointer';
            card.addEventListener('click', function() {
                var url = claim.role === 'produce'
                    ? '/api/process-nodes/' + entry.node.id + '/request-empty'
                    : '/api/process-nodes/' + entry.node.id + '/request-full';
                postAction(url, { payload_code: code }, loadViewRef);
            });
        }

        cardGrid.appendChild(card);
    });

    if (allowed.length === 0) {
        cardGrid.appendChild(el('div', {
            style: 'color:#666;font-style:italic;padding:24px;text-align:center;grid-column:1/-1',
            textContent: 'No payloads configured'
        }));
    }

    grid.appendChild(cardGrid);
}

function gridDimensions() {
    var w = window.innerWidth;
    // Fixed grid per screen size — 7": 2×2, 10": 3×2, 15"+: 4×N.
    if (w <= 1024) return { cols: 2, rows: 2 };
    if (w <= 1400) return { cols: 3, rows: 2 };
    var count = claimedNodes().length || 1;
    return { cols: 4, rows: Math.max(2, Math.ceil(count / 4)) };
}

function createNodeButton(entry) {
    const claim = entry.active_claim;
    const runtime = entry.runtime || {};
    const remaining = runtime.remaining_uop_cached != null ? runtime.remaining_uop_cached : 0;
    const capacity = claim ? claim.uop_capacity : 0;

    // Tile background by priority: release-ready (blue) > changeover
    // (orange) > fill state (full/mid/low/empty). Higher-priority states
    // replace the underlying state color so the operator gets a single
    // clear cue per tile rather than a stack of overlays. Replenishing
    // stays as an inset ring (separate "robot in motion" signal that
    // doesn't compete with the click-to-act vs CO-context cues).
    const releaseReady = isReleaseReady(entry);
    const inChangeover = !!entry.changeover_task;
    let stateClass;
    if (releaseReady) {
        stateClass = 'os-release-ready';
    } else if (inChangeover) {
        stateClass = 'os-changeover';
    } else {
        stateClass = nodeColorClass(entry);
    }
    const btn = el('div', { className: 'os-node-btn ' + stateClass });

    if (isReplenishing(entry)) btn.classList.add('os-replenishing');

    btn.appendChild(el('span', { className: 'os-node-name', textContent: entry.node.name }));

    // Banner label for the priority states. The full-tile background
    // already signals "something is up"; the label says what.
    if (releaseReady) {
        btn.appendChild(el('span', { className: 'os-node-banner', textContent: 'RELEASE READY' }));
    } else if (inChangeover) {
        btn.appendChild(el('span', { className: 'os-node-banner', textContent: 'CHANGEOVER' }));
    }

    // [REP] corner badge stays for replenishing (different signal — bin
    // move in flight). [CO] badge is suppressed because the full-tile
    // CHANGEOVER banner makes it redundant.
    const icon = statusIcon(entry);
    if (icon && icon === '[REP]') {
        btn.appendChild(el('span', { className: 'os-node-icon', textContent: icon }));
    }

    if (claim && claim.swap_mode === 'manual_swap') {
        const binState = entry.bin_state;
        const binLabel = binState && binState.bin_label ? binState.bin_label : '';
        const binPayload = binState && binState.payload_code ? binState.payload_code : '';
        // "Awaiting stock" = any non-terminal order. Just 'queued' lost the
        // indicator the moment the order advanced (sourcing, dispatched,
        // in_transit, delivered-awaiting-confirm).
        const hasActiveOrder = (entry.orders || []).some(o => isActive(o.status));
        let statusText;
        if (hasActiveOrder) {
            statusText = 'AWAITING STOCK';
        } else if (binPayload) {
            statusText = binPayload;
        } else if (remaining > 0) {
            statusText = 'LOADED';
        } else if (binState && binState.occupied) {
            statusText = 'EMPTY';
        } else {
            statusText = 'NO BIN';
        }
        btn.appendChild(el('span', { className: 'os-node-remaining', textContent: statusText }));
        if (binLabel) {
            const labelEl = el('span', { className: 'os-node-payload', textContent: binLabel });
            labelEl.style.cssText = 'font-size:14px;font-weight:600;color:#fff';
            btn.appendChild(labelEl);
        } else {
            btn.appendChild(el('span', { className: 'os-node-payload', textContent: 'Manual Swap' }));
        }
    } else {
        btn.appendChild(el('span', {
            className: 'os-node-remaining',
            textContent: claim ? String(remaining) : '--'
        }));
        if (claim && capacity > 0) {
            btn.appendChild(el('span', {
                className: 'os-node-capacity',
                textContent: '/ ' + capacity
            }));
        }
        const payloadText = claim ? (claim.payload_code || 'Unassigned') : '';
        appendOrderStatusChips(btn, entry);
        btn.appendChild(el('span', { className: 'os-node-payload', textContent: payloadText }));
    }

    btn.addEventListener('click', () => openModalRef(entry.node.id));
    return btn;
}

// Renders one chip per active order on the node. Stacked when a two-robot
// swap has both Order A and Order B in flight.
function appendOrderStatusChips(btn, entry) {
    const active = (entry.orders || []).filter(o => isActive(o.status));
    if (active.length === 0) return;
    const row = el('div', { className: 'os-node-status' });
    active.forEach(o => {
        row.appendChild(el('span', {
            className: 'os-node-status-chip',
            textContent: String(o.status || '').replace(/_/g, ' ')
        }));
    });
    btn.appendChild(row);
}

// isReleaseReady drives the os-release-ready blue glow. Same screen
// handles both production and changeover; the gate behind the operator's
// click differs per context, so this function picks the right gate.
//
// Changeover context (entry.changeover_task present):
//   - Phase 2 model. Click fires evac via ReleaseOrderWithLineside; the
//     supply leg auto-fires on evac pickup-confirm via HandleBinPickedUp's
//     deferred-supply branch.
//   - Paired evac+supply: glow when evac is at `staged` (robot at slot
//     wait point) AND supply is at `in_transit` or `staged` (dispatched,
//     past Manager.ReleaseOrder's pre-dispatch guard — supply has a
//     VendorOrderID so the auto-release will fire cleanly when evac
//     picks up). If supply is still `acknowledged` or earlier, clicking
//     would fire evac but the supply auto-release would silently no-op
//     against the pre-dispatch supply order; the glow waits past that.
//   - Standalone evac (no paired supply, e.g. drop-situation tasks):
//     glow when evac is at `staged`. No supply chain to coordinate.
//
// Production context (no changeover_task):
//   - Two-robot swap mid-cycle. Click fires ReleaseStagedOrders which
//     releases both legs at once — needs both robots at wait points.
//     entry.swap_ready (computed in store/station_views.go ComputeSwapReady)
//     already encodes that condition. Single source of truth: defer to
//     it for the production gate.
//
// Returns false otherwise (pre-dispatch, terminal, single-robot consume
// where Release is always available without a "ready" moment, etc.).
function isReleaseReady(entry) {
    const task = entry.changeover_task;
    if (task) {
        const orders = entry.orders || [];
        const byID = (id) => id == null ? null : orders.find(o => o.id === id) || null;
        const evac = byID(task.old_material_release_order_id);
        const supply = byID(task.next_material_order_id);
        if (!evac) return false;
        if (evac.status !== 'staged') return false;
        if (supply) {
            return supply.status === 'in_transit' || supply.status === 'staged';
        }
        return true;
    }
    // Production context — defer to the existing swap_ready flag.
    return !!entry.swap_ready;
}

function nodeColorClass(entry) {
    const claim = entry.active_claim;
    if (!claim) return 'os-unclaimed';
    const remaining = entry.runtime ? entry.runtime.remaining_uop_cached : 0;
    if (claim.swap_mode === 'manual_swap') {
        const hasActiveOrder = entry.orders && entry.orders.some(o => isActive(o.status));
        if (hasActiveOrder) return 'os-mid';
        return remaining > 0 ? 'os-full' : 'os-empty';
    }
    const capacity = claim.uop_capacity || 1;
    if (remaining <= 0) return 'os-empty';
    const pct = remaining / capacity;
    if (pct < 0.33) return 'os-low';
    if (pct < 0.66) return 'os-mid';
    return 'os-full';
}

function statusIcon(entry) {
    if (entry.changeover_task && entry.changeover_task.state !== 'switched' && entry.changeover_task.state !== 'verified') {
        return '[CO]';
    }
    if (isReplenishing(entry)) return '[REP]';
    return null;
}

// ─── Footer ───

export function renderFooter() {
    const view = getView();
    const co = view.active_changeover;
    const state = view.process.production_state || '';

    if (co) {
        const nodes = claimedNodes();
        const coNodes = nodes.filter(n => n.changeover_task);
        const done = coNodes.filter(n => n.changeover_task.state === 'switched' || n.changeover_task.state === 'verified').length;
        footerStatus.textContent = co.from_style_name + ' \u2192 ' + co.to_style_name +
            ' [' + done + '/' + coNodes.length + ' nodes]';
    } else {
        footerStatus.textContent = 'Operator Station Ready';
    }

    footerBadge.textContent = state.replace(/_/g, ' ');
    footerBadge.className = 'os-footer-badge';
    if (state === 'active_production') footerBadge.classList.add('producing');
    if (state === 'changeover_active') footerBadge.classList.add('changeover');
}

// Expose fillColor so the modal module can render the fill bar without
// re-importing it from operator-util.
export { fillColor };
