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
        // Follow-up refresh gives Core time to process receipt + ApplyBinArrival
        // after auto-confirm. With the retrieve_empty staging exemption in Core,
        // bins are available immediately, but this covers any remaining latency.
        setTimeout(() => scheduleRefresh(), 3000);
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
    headerInfo.textContent = view.process.name + ' - ' + style + target;

    headerActions.innerHTML = '';

    // Health badge
    const badge = el('span', {
        className: 'os-health-badge ' + (view.station.health_status === 'online' ? 'online' : 'offline')
    });
    headerActions.appendChild(badge);

    // Changeover / Cutover button
    if (view.active_changeover) {
        headerActions.appendChild(headerBtn('CUTOVER', 'cutover', confirmCutover));
    } else {
        headerActions.appendChild(headerBtn('CHANGEOVER', 'changeover', openChangeoverPicker));
    }

    headerActions.appendChild(headerBtn('REFRESH', 'refresh', loadView));
}

// ─── Changeover Picker ───

function openChangeoverPicker() {
    const styles = view.available_styles || [];
    const currentID = view.current_style ? view.current_style.id : null;
    const others = styles.filter(s => s.id !== currentID);
    if (others.length === 0) {
        showToast('No other styles available', 'error');
        return;
    }

    // Build a modal overlay with style buttons
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
    const pid = view.process.id;
    const ok = await postAction('/api/processes/' + pid + '/changeover/start', {
        to_style_id: toStyleID,
        called_by: view.station.name || 'operator',
        notes: ''
    });
    if (ok) showToast('Changeover to ' + styleName + ' started', 'success');
}

async function confirmCutover() {
    const pid = view.process.id;
    // Simple confirmation via a picker-style modal
    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker' });
    const co = view.active_changeover;
    panel.appendChild(el('div', { className: 'os-co-picker-title',
        textContent: 'Complete cutover to ' + (co.to_style_name || 'target') + '?' }));

    const confirm = el('button', { className: 'os-co-picker-btn', textContent: 'CONFIRM CUTOVER' });
    confirm.addEventListener('click', async () => {
        overlay.remove();
        const ok = await postAction('/api/processes/' + pid + '/changeover/cutover');
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

    // Single manual_swap node: render payload board instead of grid
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

// ─── Payload Board (single manual_swap node) ───

function renderPayloadBoard(entry) {
    const claim = entry.active_claim;
    const runtime = entry.runtime || {};
    const remaining = runtime.remaining_uop != null ? runtime.remaining_uop : 0;
    const binState = entry.bin_state;
    const hasBin = binState && binState.occupied;
    const binLabel = binState && binState.bin_label ? binState.bin_label : 'No bin';
    const binPayload = binState && binState.payload_code ? binState.payload_code : '';
    const roleLabel = claim.role === 'produce' ? 'Loader' : 'Unloader';

    // Node info bar
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

    // Build payload cards
    var allowed = (claim.allowed_payload_codes && claim.allowed_payload_codes.length > 0)
        ? claim.allowed_payload_codes
        : (claim.payload_code ? [claim.payload_code] : []);

    var activeOrders = (entry.orders || []).filter(function(o) {
        return o.status !== 'confirmed' && o.status !== 'cancelled' && o.status !== 'failed';
    });
    var hasDemand = activeOrders.length > 0;

    // Card container
    var cardGrid = el('div', { className: 'os-board-cards' });
    var cols = allowed.length <= 3 ? allowed.length : (allowed.length <= 6 ? 3 : 4);
    cardGrid.style.setProperty('--os-board-cols', cols);

    var queuePos = 1;
    allowed.forEach(function(code) {
        var payloadOrders = activeOrders.filter(function(o) { return o.payload_code === code; });
        var isActive = payloadOrders.length > 0 || (hasDemand && activeOrders.every(function(o) { return !o.payload_code; }));
        var payloadDelivered = payloadOrders.find(function(o) { return o.status === 'delivered'; });
        var payloadInTransit = payloadOrders.find(function(o) { return o.status === 'in_transit' || o.status === 'acknowledged'; });
        var payloadQueued = payloadOrders.find(function(o) { return o.status === 'queued' || o.status === 'pending' || o.status === 'submitted'; });

        var card = el('div', { className: 'os-board-card' });

        // Card state class
        if (payloadDelivered) {
            card.classList.add('os-board-delivered');
        } else if (payloadInTransit) {
            card.classList.add('os-board-transit');
        } else if (isActive) {
            card.classList.add('os-board-queued');
        } else {
            card.classList.add('os-board-nodemand');
        }

        // Payload code
        card.appendChild(el('div', { className: 'os-board-code', textContent: code }));

        // Status tag
        var statusText, statusClass;
        if (payloadDelivered) {
            statusText = 'DELIVERED'; statusClass = 'os-board-tag-delivered';
        } else if (payloadInTransit) {
            statusText = 'IN TRANSIT'; statusClass = 'os-board-tag-transit';
        } else if (payloadQueued) {
            statusText = 'QUEUED'; statusClass = 'os-board-tag-queued';
        } else {
            statusText = 'NO DEMAND'; statusClass = 'os-board-tag-nodemand';
        }
        var tag = el('span', { className: 'os-board-tag ' + statusClass, textContent: statusText });
        card.appendChild(tag);

        // Detail text
        var detailText = '';
        var binIsEmptyForDetail = entry.bin_state && entry.bin_state.occupied && !entry.bin_state.payload_code;
        if (payloadDelivered) {
            detailText = 'Tap to ' + (claim.role === 'produce' ? 'load' : 'unload');
        } else if (binIsEmptyForDetail && (payloadInTransit || payloadQueued)) {
            detailText = 'Empty bin at node — tap to load';
        } else if (payloadInTransit) {
            detailText = 'Robot en route';
        } else if (payloadQueued) {
            detailText = 'Waiting for robot';
        } else {
            detailText = 'No kanban signal';
        }
        card.appendChild(el('div', { className: 'os-board-detail', textContent: detailText }));

        // Queue position badge
        if (isActive) {
            var badge = el('span', { className: 'os-board-pos', textContent: String(queuePos) });
            if (payloadDelivered) badge.classList.add('os-board-pos-delivered');
            else if (payloadInTransit) badge.classList.add('os-board-pos-transit');
            else badge.classList.add('os-board-pos-queued');
            card.appendChild(badge);
            queuePos++;
        }

        // Click handler: delivered cards are always interactive.
        // Queued/in-transit cards are also interactive when an empty bin
        // is already sitting at the node — the operator can load it now
        // without waiting for the robot delivery cycle to complete.
        var binIsEmpty = entry.bin_state && entry.bin_state.occupied && !entry.bin_state.payload_code;
        var canLoad = payloadDelivered || (binIsEmpty && (payloadQueued || payloadInTransit));
        if (canLoad) {
            card.style.cursor = 'pointer';
            card.addEventListener('click', function() {
                openLoadBin(entry.node.id, [code], claim.uop_capacity || 0);
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

function claimedNodes() {
    if (!view || !view.nodes) return [];
    return view.nodes.filter(n => n.active_claim || n.changeover_task);
}

function gridDimensions() {
    var w = window.innerWidth;

    // Fixed grid per screen size — tiles stay in their cell, empty cells stay empty.
    // 7" (~1024x600 or smaller): 2×2
    // 10" (~1280x800): 3×2
    // Large (15"+): 4×2, expand rows if needed
    if (w <= 1024) {
        return { cols: 2, rows: 2 };
    } else if (w <= 1400) {
        return { cols: 3, rows: 2 };
    } else {
        var count = claimedNodes().length || 1;
        return { cols: 4, rows: Math.max(2, Math.ceil(count / 4)) };
    }
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

    if (claim && claim.swap_mode === 'manual_swap') {
        const binState = entry.bin_state;
        const binLabel = binState && binState.bin_label ? binState.bin_label : '';
        const binPayload = binState && binState.payload_code ? binState.payload_code : '';
        const hasQueued = (entry.orders || []).some(o => o.status === 'queued');
        // Manual swap: show what's loaded or awaiting
        let statusText;
        if (hasQueued) {
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
    if (claim.swap_mode === 'manual_swap') {
        const hasQueued = entry.orders && entry.orders.some(o => o.status === 'queued');
        if (hasQueued) return 'os-mid'; // amber for awaiting stock
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
        return '[CO]'; // changeover
    }
    if (isReplenishing(entry)) return '[REP]'; // replenishing
    return null;
}

// ─── Node Context Modal ───

function openModal(nodeID) {
    const entry = findNodeByID(nodeID);
    if (!entry) return;

    // Manual swap: always show the demand queue modal
    // (no longer shortcuts to load form — the queue IS the primary view)
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
        html += '<div class="os-modal-status">[REP] ' + esc(statusText) + '</div>';
    } else {
        html += '<div class="os-modal-status">No active orders</div>';
    }

    // Changeover info
    if (task) {
        html += '<div class="os-modal-co-info">[CO] Changeover: ' + esc(task.situation) + ' - ' + esc(task.state) + '</div>';
    }
    html += '</div>'; // close header

    // Actions — state machine: only show the next step in the cycle.
    // Consume cycle: IDLE → REQUEST MATERIAL → (robot stages) → RELEASE → (robot drops) → CONFIRM
    // Produce cycle: same but FINALIZE instead of REQUEST MATERIAL when node has parts.
    html += '<div class="os-modal-actions">';

    if (claim) {
        if (claim.swap_mode === 'manual_swap') {
            // ─── Demand Queue Cards ───
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

            // Demand queue section
            html += '<div class="os-demand-queue">';
            html += '<div style="font-size:13px;color:#999;margin-bottom:8px;text-transform:uppercase;letter-spacing:1px">';
            html += claim.role === 'produce' ? 'Load Queue' : 'Unload Queue';
            html += '</div>';

            // Order status summary
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

            // Payload cards — each allowed payload as a demand card with per-payload status
            var queuePos = 1;
            allowed.forEach(function(code) {
                // Match orders to this specific payload (fall back to any-demand for legacy orders without payload_code)
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
                // Only delivered cards are interactive — kanban drives demand, operator acts on delivery
                if (payloadDelivered) {
                    html += ' data-action="demand-card:' + esc(code) + '"';
                }
                html += '>';

                // Left side: queue position + payload code
                html += '<div style="display:flex;align-items:center;gap:12px">';
                if (isActive) {
                    html += '<span style="background:' + (payloadDelivered ? '#2a5a2a' : payloadInTransit ? '#5a5a2a' : '#3a5a8a') + ';color:#fff;border-radius:50%;width:28px;height:28px;display:flex;align-items:center;justify-content:center;font-weight:700;font-size:14px">' + queuePos + '</span>';
                    queuePos++;
                } else {
                    html += '<span style="width:28px"></span>';
                }
                html += '<span style="font-size:18px;font-weight:600;color:' + (isActive ? '#fff' : '#666') + '">' + esc(code) + '</span>';
                html += '</div>';

                // Right side: per-payload status indicator
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

            // Action buttons
            if (delivered) {
                html += actionBtn('CONFIRM DELIVERY', 'request', true,
                    '/api/confirm-delivery/' + delivered.id);
            }

            // CLEAR BIN — available when bin is loaded (for unloader ClearBin or mis-load fix)
            if (hasBin && remaining > 0) {
                html += actionBtn('CLEAR BIN', 'empty-tools', true,
                    '/api/process-nodes/' + entry.node.id + '/clear-bin');
            }
        } else {
            // Determine order state for this node
            const orders = entry.orders || [];
            const active = orders.filter(o => o.status !== 'confirmed' && o.status !== 'cancelled' && o.status !== 'failed');
            const staged = active.find(o => o.status === 'staged');
            const delivered = active.find(o => o.status === 'delivered');
            const inFlight = active.find(o => !staged && !delivered);

            if (staged) {
                // Staged — robot waiting, operator must release
                html += actionBtn('RELEASE', 'request', true,
                    '/api/orders/' + staged.id + '/release');
            } else if (delivered) {
                // Delivered — bin dropped, operator confirms delivery
                var confirmLabel = 'CONFIRM';
                var binState = entry.bin_state;
                if (binState && binState.manifest) {
                    try {
                        var mf = JSON.parse(binState.manifest);
                        if (Array.isArray(mf) && mf.length > 0) {
                            var totalQty = mf.reduce(function(sum, item) { return sum + (item.quantity || 0); }, 0);
                            confirmLabel = 'CONFIRM: ' + mf.length + (mf.length === 1 ? ' part' : ' parts') + ', qty ' + totalQty;
                        }
                    } catch(e) { /* manifest not parseable, use default label */ }
                }
                html += actionBtn(confirmLabel, 'request', true,
                    '/api/confirm-delivery/' + delivered.id);
            } else if (inFlight) {
                // Robot working — nothing to do
                html += actionBtn('ROBOT IN TRANSIT', 'close', false, '');
            } else {
                // Idle — primary action depends on role
                if (claim.role === 'produce' && remaining > 0) {
                    html += actionBtn('FINALIZE', 'finalize', true,
                        '/api/process-nodes/' + entry.node.id + '/finalize');
                } else {
                    html += actionBtn('REQUEST MATERIAL', 'request', true,
                        '/api/process-nodes/' + entry.node.id + '/request');
                }
                // RELEASE EMPTY and RELEASE PARTIAL removed from operator HMI.
                // Backend endpoints remain for internal use (changeover, supervisor).
            }
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

    // Demand card tapped — only delivered cards are interactive (kanban drives demand)
    if (action.startsWith('demand-card:')) {
        const code = action.split(':')[1];
        const entry = selectedNodeID ? findNodeByID(selectedNodeID) : null;
        if (!entry) return;
        const claim = entry.active_claim;

        // Delivered card → open load form for this specific payload
        closeModal();
        openLoadBin(entry.node.id, [code], claim ? claim.uop_capacity || 0 : 0);
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

    // POST action — supports url|body_json format for passing payload
    evt.currentTarget.disabled = true;
    let url = action;
    let body = undefined;
    if (action.includes('|')) {
        const parts = action.split('|');
        url = parts[0];
        body = { payload_code: parts[1] };
    }
    const ok = await postAction(url, body);
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


// ─── Bin Load (Manual Swap nodes) ───

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
        btn.style.cssText = 'font-size:14px;padding:10px 20px;margin:0 6px 6px 0;background:var(--os-gray)';
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
    if (ok) showToast('Bin loaded', 'success');
}

document.getElementById('load-bin-cancel').addEventListener('click', closeLoadBin);
document.getElementById('load-bin-submit').addEventListener('click', submitLoadBin);
document.getElementById('load-bin-clear').addEventListener('click', async () => {
    if (!loadBinState) return;
    const nodeID = loadBinState.nodeID;
    closeLoadBin();
    const ok = await postAction('/api/process-nodes/' + nodeID + '/clear-bin');
    if (ok) showToast('Bin cleared', 'success');
});
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

// Re-layout on resize (orientation change, window resize)
window.addEventListener('resize', function() { if (view) renderGrid(); });
