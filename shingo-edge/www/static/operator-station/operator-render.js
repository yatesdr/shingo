import { el, esc, fillColor, postAction, showToast } from './operator-util.js';
import { getView, claimedNodes, isReplenishing } from './operator-state.js';

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

    if (view.active_changeover) {
        headerActions.appendChild(headerBtn('CUTOVER', 'cutover', confirmCutover));
    } else {
        headerActions.appendChild(headerBtn('CHANGEOVER', 'changeover', openChangeoverPicker));
    }

    headerActions.appendChild(headerBtn('REFRESH', 'refresh', loadViewRef));
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
        grid.style.removeProperty('--os-cols');
        grid.style.removeProperty('--os-rows');
        renderPayloadBoard(manualSwapNodes[0]);
        return;
    }

    grid.classList.remove('os-board-mode');
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
    const remaining = runtime.remaining_uop != null ? runtime.remaining_uop : 0;
    const binState = entry.bin_state;
    const hasBin = binState && binState.occupied;
    const binLabel = binState && binState.bin_label ? binState.bin_label : 'No bin';
    const binPayload = binState && binState.payload_code ? binState.payload_code : '';
    const roleLabel = claim.role === 'produce' ? 'Loader' : 'Unloader';

    var infoBar = el('div', { className: 'os-board-header' });
    infoBar.innerHTML =
        '<div>' +
            '<div style="font-size:24px;font-weight:700;color:#fff">' + esc(entry.node.name) + ' - ' + roleLabel + '</div>' +
            '<div style="font-size:13px;color:#888;margin-top:4px">Manual Swap | ' +
                (claim.allowed_payload_codes ? claim.allowed_payload_codes.length : 0) + ' payloads configured</div>' +
        '</div>' +
        '<div style="text-align:right">' +
            '<div style="font-size:16px;font-weight:600;color:#fff">Bin: ' + esc(binLabel) + '</div>' +
            (binPayload ? '<div style="font-size:13px;color:#888">' + esc(binPayload) + ' | UOP: ' + remaining + '</div>' : '') +
            '<div style="display:inline-block;font-size:12px;font-weight:700;padding:4px 12px;border-radius:4px;margin-top:4px;' +
                (hasBin ? (remaining > 0 ? 'background:#1a3a1a;color:#6f6' : 'background:#3a1a1a;color:#f88') : 'background:#2a2a1a;color:#ff6') + '">' +
                (hasBin ? (remaining > 0 ? 'LOADED' : 'EMPTY') : 'AWAITING BIN') +
            '</div>' +
        '</div>';
    grid.appendChild(infoBar);

    var allowed = (claim.allowed_payload_codes && claim.allowed_payload_codes.length > 0)
        ? claim.allowed_payload_codes
        : (claim.payload_code ? [claim.payload_code] : []);

    var activeOrders = (entry.orders || []).filter(function(o) {
        return o.status !== 'confirmed' && o.status !== 'cancelled' && o.status !== 'failed';
    });
    var hasDemand = activeOrders.length > 0;

    // Multi-payload starvation fix (investigation-r2.md): when an empty bin
    // is at the node and any demand exists, every allowed payload becomes a
    // loadable option. The operator picks at load time and LoadBin re-binds
    // via the request's payload_code argument. Without this only the
    // tagged payload could be loaded, serializing manual_swap nodes.
    var nodeBinIsEmpty = entry.bin_state && entry.bin_state.occupied && !entry.bin_state.payload_code;
    var loadableHere = nodeBinIsEmpty && hasDemand;

    var cardGrid = el('div', { className: 'os-board-cards' });
    var cols = allowed.length <= 3 ? allowed.length : (allowed.length <= 6 ? 3 : 4);
    cardGrid.style.setProperty('--os-board-cols', cols);

    var queuePos = 1;
    allowed.forEach(function(code) {
        var payloadOrders = activeOrders.filter(function(o) { return o.payload_code === code; });
        var isActive = payloadOrders.length > 0 || loadableHere || (hasDemand && activeOrders.every(function(o) { return !o.payload_code; }));
        var payloadDelivered = payloadOrders.find(function(o) { return o.status === 'delivered'; });
        var payloadInTransit = payloadOrders.find(function(o) { return o.status === 'in_transit' || o.status === 'acknowledged'; });

        var card = el('div', { className: 'os-board-card' });

        if (payloadDelivered) {
            card.classList.add('os-board-delivered');
        } else if (payloadInTransit) {
            card.classList.add('os-board-transit');
        } else if (isActive) {
            card.classList.add('os-board-queued');
        } else {
            card.classList.add('os-board-nodemand');
        }

        card.appendChild(el('div', { className: 'os-board-code', textContent: code }));

        // Any active (non-terminal) order with no delivered/in-transit sibling
        // counts as QUEUED — covers pending, sourcing, queued, submitted,
        // dispatched, staged so cards never silently go inert.
        var statusText, statusClass;
        if (payloadDelivered) {
            statusText = 'DELIVERED'; statusClass = 'os-board-tag-delivered';
        } else if (payloadInTransit) {
            statusText = 'IN TRANSIT'; statusClass = 'os-board-tag-transit';
        } else if (isActive) {
            statusText = 'QUEUED'; statusClass = 'os-board-tag-queued';
        } else {
            statusText = 'NO DEMAND'; statusClass = 'os-board-tag-nodemand';
        }
        card.appendChild(el('span', { className: 'os-board-tag ' + statusClass, textContent: statusText }));

        var detailText = '';
        var binIsEmptyForDetail = entry.bin_state && entry.bin_state.occupied && !entry.bin_state.payload_code;
        if (payloadDelivered) {
            detailText = 'Tap to ' + (claim.role === 'produce' ? 'load' : 'unload');
        } else if (binIsEmptyForDetail && (payloadInTransit || (isActive && !payloadDelivered))) {
            detailText = 'Empty bin at node — tap to load';
        } else if (payloadInTransit) {
            detailText = 'Robot en route';
        } else if (isActive) {
            detailText = 'Waiting for robot';
        } else {
            detailText = 'No kanban signal';
        }
        card.appendChild(el('div', { className: 'os-board-detail', textContent: detailText }));

        if (isActive) {
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
        var canLoad = (payloadDelivered && binIsEmpty) || (binIsEmpty && isActive);
        if (canLoad) {
            card.style.cursor = 'pointer';
            card.addEventListener('click', function() {
                openLoadBinRef(entry.node.id, [code], claim.uop_capacity || 0);
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
    const remaining = runtime.remaining_uop != null ? runtime.remaining_uop : 0;
    const capacity = claim ? claim.uop_capacity : 0;

    const btn = el('div', { className: 'os-node-btn ' + nodeColorClass(entry) });

    if (isReplenishing(entry)) btn.classList.add('os-replenishing');
    if (entry.changeover_task) btn.classList.add('os-changeover');

    btn.appendChild(el('span', { className: 'os-node-name', textContent: entry.node.name }));

    const icon = statusIcon(entry);
    if (icon) btn.appendChild(el('span', { className: 'os-node-icon', textContent: icon }));

    if (claim && claim.swap_mode === 'manual_swap') {
        const binState = entry.bin_state;
        const binLabel = binState && binState.bin_label ? binState.bin_label : '';
        const binPayload = binState && binState.payload_code ? binState.payload_code : '';
        // "Awaiting stock" = any non-terminal order. Just 'queued' lost the
        // indicator the moment the order advanced (sourcing, dispatched,
        // in_transit, delivered-awaiting-confirm).
        const hasActiveOrder = (entry.orders || []).some(o =>
            o.status !== 'confirmed' && o.status !== 'cancelled' && o.status !== 'failed');
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
        btn.appendChild(el('span', { className: 'os-node-payload', textContent: payloadText }));
    }

    btn.addEventListener('click', () => openModalRef(entry.node.id));
    return btn;
}

function nodeColorClass(entry) {
    const claim = entry.active_claim;
    if (!claim) return 'os-unclaimed';
    const remaining = entry.runtime ? entry.runtime.remaining_uop : 0;
    if (claim.swap_mode === 'manual_swap') {
        const hasActiveOrder = entry.orders && entry.orders.some(o =>
            o.status !== 'confirmed' && o.status !== 'cancelled' && o.status !== 'failed');
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
