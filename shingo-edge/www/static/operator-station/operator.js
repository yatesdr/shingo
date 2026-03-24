// Operator Station Display — Touch-centric 10" screen
// Replaces canvas-based hmi.js with DOM rendering, SSE live updates, modal interaction.

const stationID = parseInt(document.body.dataset.stationId, 10);
let view = null;          // current OperatorStationView
let selectedNodeID = null; // node ID for open modal (track by ID, not index)
let eventSource = null;
let refreshTimer = null;
let lastViewJSON = '';

// ─── DOM refs ───
const grid = document.getElementById('os-grid');
const headerInfo = document.getElementById('os-header-info');
const headerActions = document.getElementById('os-header-actions');
const footerStatus = document.getElementById('os-footer-status');
const footerBadge = document.getElementById('os-footer-badge');
const nodeModal = document.getElementById('node-modal');
const nodeModalContent = document.getElementById('node-modal-content');
const keypadModal = document.getElementById('keypad-modal');
const keypadDisplay = document.getElementById('keypad-display');
const toastContainer = document.getElementById('os-toast');

// ─── Data Fetch ───

async function loadView() {
    try {
        const res = await fetch('/api/operator-stations/' + stationID + '/view');
        if (!res.ok) { showToast('Connection error: ' + res.status, 'error'); return; }
        const text = await res.text();
        if (text === lastViewJSON) return;
        lastViewJSON = text;
        view = JSON.parse(text);
        renderAll();
    } catch (e) {
        showToast('Network error', 'error');
    }
}

// ─── SSE ───

function connectSSE() {
    if (eventSource) { eventSource.close(); }
    eventSource = new EventSource('/events');
    const events = ['order-update', 'order-completed', 'order-failed',
                    'counter-update', 'changeover-update', 'material-refresh'];
    for (const name of events) {
        eventSource.addEventListener(name, () => scheduleRefresh());
    }
    eventSource.onerror = () => {
        eventSource.close();
        eventSource = null;
        setTimeout(connectSSE, 3000);
    };
}

function scheduleRefresh() {
    if (refreshTimer) return;
    refreshTimer = setTimeout(async () => {
        refreshTimer = null;
        await loadView();
    }, 500);
}

// ─── Rendering ───

function renderAll() {
    if (!view) return;
    renderHeader();
    renderGrid();
    renderFooter();
    if (selectedNodeID !== null) {
        const entry = findNodeByID(selectedNodeID);
        if (entry) {
            renderModal(entry);
        } else {
            closeModal();
        }
    }
}

function findNodeByID(id) {
    if (!view || !view.nodes) return null;
    return view.nodes.find(n => n.node.id === id) || null;
}

// ─── Header ───

function renderHeader() {
    const style = view.current_style ? view.current_style.name : 'No Style';
    const target = view.target_style ? (' \u2192 ' + view.target_style.name) : '';
    headerInfo.textContent = view.process.name + ' \u2014 ' + style + target;

    headerActions.innerHTML = '';

    // Health badge
    const badge = el('span', {
        className: 'os-health-badge ' + (view.station.health_status === 'online' ? 'online' : 'offline')
    });
    headerActions.appendChild(badge);

    headerActions.appendChild(headerBtn('REFRESH', 'refresh', loadView));
}

function headerBtn(label, cls, onClick) {
    const btn = el('button', { className: 'os-header-btn ' + cls, textContent: label });
    btn.addEventListener('click', onClick);
    return btn;
}

// ─── Grid ───

function renderGrid() {
    const nodes = claimedNodes();
    grid.innerHTML = '';

    if (nodes.length === 0) {
        grid.style.removeProperty('--os-cols');
        grid.style.removeProperty('--os-rows');
        const empty = el('div', { id: 'os-grid-empty', textContent: 'No claimed nodes' });
        grid.appendChild(empty);
        return;
    }

    const { cols, rows } = gridDimensions();
    grid.style.setProperty('--os-cols', cols);
    grid.style.setProperty('--os-rows', rows);

    for (const entry of nodes) {
        grid.appendChild(createNodeButton(entry));
    }
}

function claimedNodes() {
    if (!view || !view.nodes) return [];
    return view.nodes.filter(n => n.active_claim || n.changeover_task);
}

function gridDimensions() {
    return { cols: 3, rows: 4 };
}

function createNodeButton(entry) {
    const claim = entry.active_claim;
    const runtime = entry.runtime || {};
    const remaining = runtime.remaining_uop != null ? runtime.remaining_uop : 0;
    const capacity = claim ? claim.uop_capacity : 0;

    const btn = el('div', { className: 'os-node-btn ' + nodeColorClass(entry) });

    if (isReplenishing(entry)) btn.classList.add('os-replenishing');
    if (entry.changeover_task) btn.classList.add('os-changeover');

    // Node name
    btn.appendChild(el('span', { className: 'os-node-name', textContent: entry.node.name }));

    // Status icon
    const icon = statusIcon(entry);
    if (icon) btn.appendChild(el('span', { className: 'os-node-icon', textContent: icon }));

    if (claim && claim.role === 'bin_loader') {
        const binState = entry.bin_state;
        const binLabel = binState && binState.bin_label ? binState.bin_label : '';
        const binPayload = binState && binState.payload_code ? binState.payload_code : '';
        // Bin loader: show what's loaded
        btn.appendChild(el('span', {
            className: 'os-node-remaining',
            textContent: binPayload ? binPayload : (remaining > 0 ? 'LOADED' : (binState && binState.occupied ? 'EMPTY' : 'NO BIN'))
        }));
        btn.appendChild(el('span', {
            className: 'os-node-payload',
            textContent: binLabel || 'Bin Loader'
        }));
    } else {
        // Remaining count
        btn.appendChild(el('span', {
            className: 'os-node-remaining',
            textContent: claim ? String(remaining) : '--'
        }));

        // Capacity
        if (claim && capacity > 0) {
            btn.appendChild(el('span', {
                className: 'os-node-capacity',
                textContent: '/ ' + capacity
            }));
        }

        // Payload
        const payloadText = claim ? (claim.payload_code || 'Unassigned') : '';
        btn.appendChild(el('span', { className: 'os-node-payload', textContent: payloadText }));
    }

    btn.addEventListener('click', () => openModal(entry.node.id));
    return btn;
}

function nodeColorClass(entry) {
    const claim = entry.active_claim;
    if (!claim) return 'os-unclaimed';
    const remaining = entry.runtime ? entry.runtime.remaining_uop : 0;
    if (claim.role === 'bin_loader') {
        return remaining > 0 ? 'os-full' : 'os-empty';
    }
    const capacity = claim.uop_capacity || 1;
    if (remaining <= 0) return 'os-empty';
    const pct = remaining / capacity;
    if (pct < 0.33) return 'os-low';
    if (pct < 0.66) return 'os-mid';
    return 'os-full';
}

function isReplenishing(entry) {
    const rt = entry.runtime;
    return rt && (rt.active_order_id || rt.staged_order_id);
}

function statusIcon(entry) {
    if (entry.changeover_task && entry.changeover_task.state !== 'switched' && entry.changeover_task.state !== 'verified') {
        return '\u{1F527}'; // wrench
    }
    if (isReplenishing(entry)) return '\u{1F504}'; // counterclockwise arrows
    return null;
}

// ─── Node Context Modal ───

function openModal(nodeID) {
    const entry = findNodeByID(nodeID);
    if (!entry) return;

    // Bin loader: skip the node modal, go straight to load form
    if (entry.active_claim && entry.active_claim.role === 'bin_loader') {
        const claim = entry.active_claim;
        const allowed = (claim.allowed_payload_codes && claim.allowed_payload_codes.length > 0)
            ? claim.allowed_payload_codes
            : (claim.payload_code ? [claim.payload_code] : []);
        openLoadBin(entry.node.id, allowed, claim.uop_capacity || 0);
        return;
    }
    selectedNodeID = nodeID;
    renderModal(entry);
    nodeModal.hidden = false;
}

function closeModal() {
    selectedNodeID = null;
    nodeModal.hidden = true;
}

function renderModal(entry) {
    const claim = entry.active_claim;
    const runtime = entry.runtime || {};
    const remaining = runtime.remaining_uop != null ? runtime.remaining_uop : 0;
    const capacity = claim ? claim.uop_capacity : 0;
    const pct = capacity > 0 ? Math.min(remaining / capacity, 1) : 0;
    const task = entry.changeover_task;

    let html = '';

    // Header
    html += '<div class="os-modal-header">';
    html += '<div class="os-modal-node-name">' + esc(entry.node.name) + '</div>';

    if (claim && claim.role === 'bin_loader') {
        const binState = entry.bin_state;
        const binLabel = binState && binState.bin_label ? binState.bin_label : 'No bin';
        const binPayload = binState && binState.payload_code ? binState.payload_code : '';
        html += '<div class="os-modal-payload">Bin: ' + esc(binLabel) + (binPayload ? ' \u2014 ' + esc(binPayload) : '') + '</div>';
        html += '<div class="os-modal-fill-row">';
        html += '<div class="os-modal-fill-text" style="font-size:18px;font-weight:600">' + (remaining > 0 ? 'LOADED (' + remaining + ' UOP)' : 'EMPTY') + '</div>';
        html += '</div>';
    } else {
        const binState = entry.bin_state;
        const binLabel = binState && binState.bin_label ? ' \u2014 Bin: ' + esc(binState.bin_label) : '';
        html += '<div class="os-modal-payload">' + esc(claim ? claim.payload_code || 'Unassigned' : 'No claim') + binLabel + '</div>';
        // Fill bar
        html += '<div class="os-modal-fill-row">';
        html += '<div class="os-modal-fill-bar"><div class="os-modal-fill-level" style="width:' + Math.round(pct * 100) + '%;background:' + fillColor(pct, remaining) + '"></div></div>';
        html += '<div class="os-modal-fill-text">' + remaining + ' / ' + capacity + '</div>';
        html += '</div>';
    }

    // Order status
    if (isReplenishing(entry)) {
        const activeOrders = (entry.orders || []).filter(o => o.status !== 'confirmed' && o.status !== 'cancelled' && o.status !== 'failed');
        const statusText = activeOrders.length > 0
            ? activeOrders.map(o => o.order_type + ': ' + o.status).join(', ')
            : 'Order in progress';
        html += '<div class="os-modal-status">\u{1F504} ' + esc(statusText) + '</div>';
    } else {
        html += '<div class="os-modal-status">No active orders</div>';
    }

    // Changeover info
    if (task) {
        html += '<div class="os-modal-co-info">\u{1F527} Changeover: ' + esc(task.situation) + ' \u2014 ' + esc(task.state) + '</div>';
    }
    html += '</div>'; // close header

    // Actions
    html += '<div class="os-modal-actions">';

    if (claim) {
        if (claim.role === 'bin_loader') {
            // Store load data on the module scope — can't embed JSON in data-action (escaping breaks it)
            _pendingLoadData = {
                nodeID: entry.node.id,
                allowed: (claim.allowed_payload_codes && claim.allowed_payload_codes.length > 0)
                    ? claim.allowed_payload_codes
                    : (claim.payload_code ? [claim.payload_code] : []),
                capacity: claim.uop_capacity || 0
            };
            html += actionBtn('LOAD BIN', 'load-bin', true, 'load-bin');
        } else {
            // Normal production actions
            html += actionBtn('REQUEST MATERIAL', 'request', true,
                '/api/process-nodes/' + entry.node.id + '/request');
            html += actionBtn('RELEASE EMPTY', 'release-empty', true,
                '/api/process-nodes/' + entry.node.id + '/release-empty');
            html += actionBtn('RELEASE PARTIAL', 'release-partial', true,
                'keypad:' + entry.node.id + ':' + remaining);

            if (claim.role === 'produce') {
                html += actionBtn('FINALIZE', 'finalize', true,
                    '/api/process-nodes/' + entry.node.id + '/finalize');
            }
        }

        // Manifest confirmation
        const orders = entry.orders || [];
        const hasUnconfirmedDelivery = orders.some(o => o.status === 'delivered' && o.order_type === 'retrieve');
        if (hasUnconfirmedDelivery) {
            html += actionBtn('CONFIRM MANIFEST', 'confirm-manifest', true,
                '/api/process-nodes/' + entry.node.id + '/manifest/confirm');
        }
    }

    // Changeover actions (all shown, grayed when invalid)
    if (task) {
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

    // Close button
    html += '<div class="os-modal-actions" style="margin-top:12px">';
    html += '<button type="button" class="os-action-btn close" data-action="close">CLOSE</button>';
    html += '</div>';

    nodeModalContent.innerHTML = html;

    // Wire up action clicks
    nodeModalContent.querySelectorAll('[data-action]').forEach(btn => {
        btn.addEventListener('click', handleModalAction);
    });
}

function actionBtn(label, cls, enabled, action) {
    return '<button type="button" class="os-action-btn ' + cls + '"' +
        (!enabled ? ' disabled' : '') +
        ' data-action="' + esc(action) + '">' + esc(label) + '</button>';
}

async function handleModalAction(evt) {
    const action = evt.currentTarget.dataset.action;
    if (!action) return;

    if (action === 'close') { closeModal(); return; }

    if (action === 'load-bin' && _pendingLoadData) {
        const data = _pendingLoadData;
        _pendingLoadData = null;
        closeModal();
        openLoadBin(data.nodeID, data.allowed, data.capacity);
        return;
    }

    if (action.startsWith('keypad:')) {
        const parts = action.split(':');
        const nodeID = parseInt(parts[1], 10);
        const remaining = parseInt(parts[2], 10) || 0;
        closeModal();
        openKeypad(nodeID, remaining);
        return;
    }

    // POST action
    evt.currentTarget.disabled = true;
    const ok = await postAction(action);
    if (ok) closeModal();
}

// ─── API Helper ───

async function postAction(url, body) {
    try {
        const res = await fetch(url, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body || {})
        });
        if (!res.ok) {
            const text = await res.text();
            let msg;
            try { msg = JSON.parse(text).error || text; } catch { msg = text; }
            showToast(msg, 'error');
            return false;
        }
        await loadView();
        return true;
    } catch (e) {
        showToast('Network error', 'error');
        return false;
    }
}

// ─── Keypad ───

let keypadState = null;

function openKeypad(nodeID, remaining) {
    const initial = remaining > 0 ? String(remaining) : '0';
    keypadState = { nodeID, value: initial };
    keypadDisplay.textContent = initial;
    keypadModal.hidden = false;
}

function closeKeypad() {
    keypadState = null;
    keypadModal.hidden = true;
}

// Keypad grid clicks
document.querySelector('.os-keypad-grid').addEventListener('click', evt => {
    const key = evt.target.dataset.key;
    if (!key || !keypadState) return;
    if (key === 'back') {
        keypadState.value = keypadState.value.length > 1 ? keypadState.value.slice(0, -1) : '0';
    } else {
        keypadState.value = keypadState.value === '0' ? key : keypadState.value + key;
    }
    keypadDisplay.textContent = keypadState.value;
});

document.getElementById('keypad-cancel').addEventListener('click', closeKeypad);
document.getElementById('keypad-clear').addEventListener('click', () => {
    if (!keypadState) return;
    keypadState.value = '0';
    keypadDisplay.textContent = '0';
});
document.getElementById('keypad-ok').addEventListener('click', async () => {
    if (!keypadState) return;
    const qty = parseInt(keypadState.value || '0', 10);
    const nodeID = keypadState.nodeID;
    closeKeypad();
    const ok = await postAction('/api/process-nodes/' + nodeID + '/release-partial', { qty });
    if (ok) closeModal();
});


// ─── Bin Load (Bin Loader nodes) ───

let _pendingLoadData = null;
let loadBinState = null;

function openLoadBin(nodeID, allowedCodes, defaultCapacity) {
    loadBinState = { nodeID, payloadCode: '' };
    // Build payload picker buttons
    const payloadEl = document.getElementById('load-bin-payload');
    payloadEl.innerHTML = '';
    const rows = document.getElementById('load-bin-rows');
    rows.innerHTML = '<div style="color:#999;text-align:center;padding:12px">Select a payload above</div>';
    (allowedCodes || []).forEach(code => {
        const btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'os-action-btn';
        btn.style.cssText = 'font-size:14px;padding:10px 20px;margin:0 6px 6px 0';
        btn.textContent = code;
        btn.dataset.code = code;
        btn.addEventListener('click', () => selectLoadPayload(code));
        payloadEl.appendChild(btn);
    });
    document.getElementById('load-bin-modal').hidden = false;
}

async function selectLoadPayload(code) {
    if (!loadBinState) return;
    loadBinState.payloadCode = code;
    // Highlight selected
    document.querySelectorAll('#load-bin-payload button').forEach(btn => {
        btn.className = 'os-action-btn' + (btn.dataset.code === code ? ' request' : '');
    });
    // Fetch manifest template from Core
    const rows = document.getElementById('load-bin-rows');
    rows.innerHTML = '<div style="color:#999;text-align:center;padding:12px">Loading manifest...</div>';
    try {
        const res = await fetch('/api/payload/' + encodeURIComponent(code) + '/manifest');
        const data = res.ok ? await res.json() : { uop_capacity: 0, items: [] };
        const items = data.items || [];
        const uopCapacity = data.uop_capacity || 0;
        rows.innerHTML = '';
        if (items.length === 0) {
            rows.innerHTML = '<div style="color:#f66;padding:8px">No manifest template for this payload</div>';
            return;
        }
        // UOP count field (from payload template, not sum of parts)
        const uopRow = document.createElement('div');
        uopRow.style.cssText = 'display:grid;grid-template-columns:1fr 100px;gap:8px;align-items:center;margin-bottom:12px;padding:10px;background:#1a2a1a;border-radius:4px;border:1px solid #2a4a2a';
        uopRow.innerHTML =
            '<div style="font-size:16px;font-weight:600;color:#fff">UOP Count</div>' +
            '<input type="number" id="os-load-uop" value="' + uopCapacity + '" ' +
                'style="width:100%;font-size:18px;padding:8px;border:1px solid #444;border-radius:4px;background:#222;color:#fff;text-align:center;font-weight:600">';
        rows.appendChild(uopRow);

        items.forEach(item => {
            const row = document.createElement('div');
            row.style.cssText = 'display:grid;grid-template-columns:1fr 80px;gap:8px;align-items:center;margin-bottom:8px;padding:8px;background:#1a1a1a;border-radius:4px';
            row.innerHTML =
                '<div>' +
                    '<div style="font-size:15px;color:#fff">' + esc(item.part_number) + '</div>' +
                    '<div style="font-size:12px;color:#999">' + esc(item.description || '') + '</div>' +
                '</div>' +
                '<input type="number" class="os-manifest-qty" value="' + (item.quantity || 0) + '" ' +
                    'data-part="' + esc(item.part_number) + '" data-desc="' + esc(item.description || '') + '" ' +
                    'style="width:100%;font-size:18px;padding:8px;border:1px solid #444;border-radius:4px;background:#222;color:#fff;text-align:center">';
            rows.appendChild(row);
        });
    } catch (e) {
        rows.innerHTML = '<div style="color:#f66;padding:8px">Failed to load manifest</div>';
    }
}

function closeLoadBin() {
    loadBinState = null;
    document.getElementById('load-bin-modal').hidden = true;
}

async function submitLoadBin() {
    if (!loadBinState || !loadBinState.payloadCode) {
        showToast('Select a payload first', 'error');
        return;
    }
    const manifest = [];
    document.querySelectorAll('.os-manifest-qty').forEach(input => {
        const qty = parseInt(input.value, 10) || 0;
        if (qty > 0) {
            manifest.push({
                part_number: input.dataset.part,
                quantity: qty,
                description: input.dataset.desc || ''
            });
        }
    });
    if (manifest.length === 0) {
        showToast('Enter at least one quantity', 'error');
        return;
    }
    const uopEl = document.getElementById('os-load-uop');
    const uopCount = uopEl ? parseInt(uopEl.value, 10) || 0 : 0;
    const body = { payload_code: loadBinState.payloadCode, uop_count: uopCount, manifest };
    const nodeID = loadBinState.nodeID;
    closeLoadBin();
    const ok = await postAction('/api/process-nodes/' + nodeID + '/load-bin', body);
    if (ok) {
        showToast('Bin loaded', 'success');
        // Delay re-fetch to give Core time to process the bin.load via Kafka
        setTimeout(loadView, 1500);
    }
}

document.getElementById('load-bin-cancel').addEventListener('click', closeLoadBin);
document.getElementById('load-bin-submit').addEventListener('click', submitLoadBin);
document.getElementById('load-bin-modal').addEventListener('click', evt => {
    if (evt.target === document.getElementById('load-bin-modal')) closeLoadBin();
});

// ─── Footer ───

function renderFooter() {
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

// ─── Toast ───

function showToast(msg, type) {
    const toast = el('div', {
        className: 'os-toast-msg' + (type ? ' ' + type : ''),
        textContent: msg
    });
    toastContainer.appendChild(toast);
    setTimeout(() => toast.remove(), 3200);
}

// ─── Backdrop click to close modals ───

nodeModal.addEventListener('click', evt => {
    if (evt.target === nodeModal) closeModal();
});
keypadModal.addEventListener('click', evt => {
    if (evt.target === keypadModal) closeKeypad();
});
// ─── Utilities ───

function el(tag, props) {
    const e = document.createElement(tag);
    if (props) Object.assign(e, props);
    return e;
}

function esc(s) {
    if (!s) return '';
    const d = document.createElement('div');
    d.textContent = s;
    return d.innerHTML;
}

function fillColor(pct, remaining) {
    if (remaining <= 0) return 'var(--os-red)';
    if (pct < 0.33) return 'var(--os-red)';
    if (pct < 0.66) return 'var(--os-amber)';
    return 'var(--os-green-bright)';
}

// ─── Init ───

connectSSE();
loadView();
