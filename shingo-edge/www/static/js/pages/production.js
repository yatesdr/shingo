import { api, createSSE, delegateActions, escapeHtml, hideModal, navigateToProcess, prompt, showModal, toast } from '/static/js/shingoedge.js';

// Production page — operator-facing actions for the per-process node grid.
//
// Extracted from inline <script> blocks in templates/production.html as
// part of the UI consistency refactor. Functions remain attached to
// window (called from inline onclick / data-action handlers in the
// template); refactoring those callers to delegated event handlers is
// out of scope here.

// Material page Release prompt — mirrors orders.js releaseOrder. Asks
// operator to declare bin's remaining parts so the manifest sync at Core
// uses the declared count rather than runtime.RemainingUOPCached, which
// may be stale or zeroed (e.g. by a prior release-click on the slot).
// Empty/0 → bin released as empty (manifest cleared); positive integer
// → manifest preserved with that count.
async function releaseNodeWithPrompt(nodeID) {
    var input = await prompt(
        'How many parts remain in this bin?\n\n' +
        'Enter 0 (or leave blank) to release as EMPTY (manifest cleared).\n' +
        'Enter a positive number to release as PARTIAL (manifest preserved\n' +
        'with that count).',
        { type: 'number', min: 0 }
    );
    if (input === null) return; // operator cancelled
    var trimmed = String(input).trim();
    var partial = trimmed === '' ? 0 : Number(trimmed);
    if (!Number.isInteger(partial) || partial < 0) {
        toast('Invalid count: enter 0, blank, or a positive whole number', 'error');
        return;
    }
    try {
        await api.post('/api/process-nodes/' + nodeID + '/release-empty',
            { partial_count: partial });
        toast(partial > 0
            ? 'Released — partial (' + partial + ' parts preserved)'
            : 'Released — empty (manifest cleared)', 'success');
    } catch(e) {
        toast('Error: ' + e, 'error');
    }
}

function viewBinContents() {
    // Invoked via data-action="viewBinContents". The bin state is
    // JSON in the data-bin-state attribute on the clicked element.
    var binState = {};
    try { binState = JSON.parse(this.dataset.binState || '{}') || {}; }
    catch (e) { binState = {}; }
    document.getElementById('view-bin-title').textContent = 'Bin: ' + (binState.bin_label || 'Unknown');
    var body = document.getElementById('view-bin-body');
    var html = '<div style="display:grid;grid-template-columns:1fr 1fr;gap:0.5rem;margin-bottom:1rem">';
    html += '<div><div style="color:var(--text-muted);font-size:0.8rem">Payload</div><strong>' + escapeHtml(binState.payload_code || 'empty') + '</strong></div>';
    html += '<div><div style="color:var(--text-muted);font-size:0.8rem">UoP Remaining</div><strong>' + (binState.uop_remaining || 0) + '</strong></div>';
    html += '<div><div style="color:var(--text-muted);font-size:0.8rem">Bin Type</div>' + escapeHtml(binState.bin_type_code || '-') + '</div>';
    html += '<div><div style="color:var(--text-muted);font-size:0.8rem">Confirmed</div>' + (binState.manifest_confirmed ? 'Yes' : 'No') + '</div>';
    html += '</div>';
    if (binState.manifest) {
        try {
            var manifest = typeof binState.manifest === 'string' ? JSON.parse(binState.manifest) : binState.manifest;
            var items = manifest.items || [];
            if (items.length > 0) {
                html += '<table class="table" style="font-size:0.85rem"><thead><tr><th>Part</th><th>Qty</th></tr></thead><tbody>';
                items.forEach(function(item) {
                    html += '<tr><td>' + escapeHtml(item.catid || item.part_number || '') + '</td><td>' + (item.qty || item.quantity || 0) + '</td></tr>';
                });
                html += '</tbody></table>';
            }
        } catch(e) {}
    }
    body.innerHTML = html;
    showModal('view-bin-modal');
}

function openRequestEmptyModal() {
    // Invoked via data-action="openRequestEmptyModal" — args come off
    // the clicked element's dataset. `this` is the clicked button.
    var nodeID = parseInt(this.dataset.nodeId, 10);
    var allowedCodes = [];
    try {
        allowedCodes = JSON.parse(this.dataset.allowedPayloads || '[]') || [];
    } catch (e) { allowedCodes = []; }
    document.getElementById('re-node-id').value = nodeID;
    var sel = document.getElementById('re-payload');
    sel.innerHTML = '';
    (allowedCodes || []).forEach(function(code) {
        var opt = document.createElement('option');
        opt.value = code;
        opt.textContent = code;
        sel.appendChild(opt);
    });
    showModal('request-empty-modal');
}

async function submitRequestEmpty() {
    var nodeID = parseInt(document.getElementById('re-node-id').value, 10);
    var payloadCode = document.getElementById('re-payload').value;
    if (!payloadCode) { toast('Select a payload', 'warning'); return; }
    try {
        await api.post('/api/process-nodes/' + nodeID + '/request-empty', {payload_code: payloadCode});
        hideModal('request-empty-modal');
        toast('Empty bin requested', 'success');
    } catch(e) {
        toast('Error: ' + e, 'error');
    }
}

var _loadBinCatalog = null;

async function ensureLoadBinCatalog() {
    if (_loadBinCatalog) return _loadBinCatalog;
    try { _loadBinCatalog = await api.get('/api/payload-catalog'); } catch(_) { _loadBinCatalog = []; }
    if (!Array.isArray(_loadBinCatalog)) _loadBinCatalog = [];
    return _loadBinCatalog;
}

async function openLoadBinModal() {
    // Invoked via data-action="openLoadBinModal" with data-node-id,
    // data-allowed-payloads (JSON array), data-uop-capacity.
    var nodeID = parseInt(this.dataset.nodeId, 10);
    var allowedCodes = [];
    try { allowedCodes = JSON.parse(this.dataset.allowedPayloads || '[]') || []; }
    catch (e) { allowedCodes = []; }
    // defaultCapacity kept as a positional param historically; no
    // callers actually used it for anything beyond pass-through, but
    // pull it off the dataset for parity.
    var defaultCapacity = parseInt(this.dataset.uopCapacity, 10) || 0;
    void defaultCapacity;
    document.getElementById('rb-node-id').value = nodeID;
    document.getElementById('rb-payload-code').value = '';
    var catalog = await ensureLoadBinCatalog();
    var sel = document.getElementById('rb-payload');
    sel.innerHTML = '<option value="">-- Select payload --</option>';
    (allowedCodes || []).forEach(function(code) {
        var entry = catalog.find(function(p) { return p.code === code; });
        var opt = document.createElement('option');
        opt.value = code;
        opt.textContent = code + (entry && entry.name ? ' — ' + entry.name : '');
        sel.appendChild(opt);
    });
    document.getElementById('rb-manifest-rows').innerHTML = '<div style="color:var(--text-muted);font-style:italic;padding:0.5rem 0">Select a payload to see its manifest.</div>';
    showModal('load-bin-modal');
}

async function onLoadPayloadChanged() {
    var code = document.getElementById('rb-payload').value;
    document.getElementById('rb-payload-code').value = code;
    var rows = document.getElementById('rb-manifest-rows');
    if (!code) {
        rows.innerHTML = '<div style="color:var(--text-muted);font-style:italic;padding:0.5rem 0">Select a payload to see its manifest.</div>';
        return;
    }
    rows.innerHTML = '<div style="color:var(--text-muted);padding:0.5rem 0">Loading manifest...</div>';
    try {
        var data = await api.get('/api/payload/' + encodeURIComponent(code) + '/manifest');
        var items = (data && data.items) || [];
        var uopCapacity = (data && data.uop_capacity) || 0;
        rows.innerHTML = '';
        if (items.length === 0) {
            rows.innerHTML = '<div style="color:var(--text-muted);font-style:italic;padding:0.5rem 0">No manifest template for this payload.</div>';
            return;
        }
        var uopRow = document.createElement('div');
        uopRow.style.cssText = 'display:grid;grid-template-columns:1fr 80px;gap:0.5rem;align-items:center;margin-bottom:0.75rem;padding:0.5rem;border:2px solid var(--primary, #4a9);border-radius:4px';
        uopRow.innerHTML = '<div style="font-weight:600">UoP Count</div>' +
            '<input type="number" id="rb-uop-count" class="form-input" value="' + uopCapacity + '" style="text-align:center;font-weight:600">';
        rows.appendChild(uopRow);
        items.forEach(function(item) {
            var row = document.createElement('div');
            row.style.cssText = 'display:grid;grid-template-columns:1fr 80px;gap:0.5rem;align-items:center;margin-bottom:0.5rem;padding:0.5rem;border:1px solid var(--border);border-radius:4px';
            row.innerHTML =
                '<div><div style="font-weight:500">' + escapeHtml(item.part_number) + '</div>' +
                '<div style="color:var(--text-muted);font-size:0.85rem">' + escapeHtml(item.description || '') + '</div></div>' +
                '<input type="number" class="form-input rb-manifest-qty" value="' + (item.quantity || 0) + '" ' +
                    'data-part="' + escapeHtml(item.part_number) + '" data-desc="' + escapeHtml(item.description || '') + '" ' +
                    'style="text-align:center">';
            rows.appendChild(row);
        });
    } catch(e) {
        rows.innerHTML = '<div style="color:var(--danger, red);padding:0.5rem 0">Failed to load manifest.</div>';
    }
}

function closeLoadBinModal() {
    hideModal('load-bin-modal');
}

async function submitLoadBin() {
    var nodeID = parseInt(document.getElementById('rb-node-id').value, 10);
    var payloadCode = document.getElementById('rb-payload-code').value;
    if (!payloadCode) { toast('Select a payload first', 'warning'); return; }
    var manifest = [];
    document.querySelectorAll('.rb-manifest-qty').forEach(function(input) {
        var qty = parseInt(input.value, 10) || 0;
        if (qty > 0) manifest.push({part_number: input.dataset.part, quantity: qty, description: input.dataset.desc || ''});
    });
    if (manifest.length === 0) { toast('Enter at least one quantity', 'warning'); return; }
    try {
        var uopCount = parseInt((document.getElementById('rb-uop-count') || {}).value || '0', 10);
        await api.post('/api/process-nodes/' + nodeID + '/load-bin', {payload_code: payloadCode, uop_count: uopCount, manifest: manifest});
        closeLoadBinModal();
        toast('Bin loaded', 'success');
    } catch(e) {
        toast('Error: ' + e, 'error');
    }
}

// ─── Lineside bucket handlers ───────────────────────────

async function editLinesideBucket() {
    var tr = this.closest('tr');
    var bucketID = tr.getAttribute('data-bucket-id');
    var input = tr.querySelector('.lineside-qty-input');
    var warn = tr.querySelector('.lineside-warn');
    var qty = parseInt(input.value, 10);
    if (isNaN(qty) || qty < 0) {
        warn.textContent = 'Qty must be a non-negative number.';
        warn.style.display = '';
        return;
    }
    warn.style.display = 'none';
    try {
        await api.post('/api/lineside/buckets/' + encodeURIComponent(bucketID) + '/qty', { qty: qty });
        toast('Quantity updated', 'success');
    } catch(e) {
        toast('Error: ' + e, 'error');
    }
}

async function clearLinesideBucket() {
    var tr = this.closest('tr');
    var bucketID = tr.getAttribute('data-bucket-id');
    if (!await confirm('Clear this lineside bucket? The chip will vanish from the operator HMI.')) {
        return;
    }
    try {
        await api.post('/api/lineside/buckets/' + encodeURIComponent(bucketID) + '/clear');
        toast('Lineside bucket cleared', 'success');
    } catch(e) {
        toast('Error: ' + e, 'error');
    }
}

// ─── Shift production chart ─────────────────────────────

var _chartData = {};
try {
    var _pd = document.getElementById('page-data');
    if (_pd) {
        _chartData.shifts = JSON.parse(_pd.dataset.shifts || '[]');
        _chartData.hourlyCounts = JSON.parse(_pd.dataset.hourlyCounts || '{}');
        _chartData.todayDate = _pd.dataset.todayDate || '';
        _chartData.activeProcessID = parseInt(_pd.dataset.activeProcessId || '0', 10);
    }
} catch(e) {}

if (!_chartData.shifts) _chartData.shifts = [];
if (!Array.isArray(_chartData.shifts)) _chartData.shifts = [];
if (!_chartData.hourlyCounts) _chartData.hourlyCounts = {};
if (typeof _chartData.hourlyCounts !== 'object' || Array.isArray(_chartData.hourlyCounts)) _chartData.hourlyCounts = {};

function parseHHMM(s) {
    var parts = s.split(':');
    return parseInt(parts[0]) * 60 + parseInt(parts[1] || '0');
}

function shiftClockHours(shift) {
    var startMin = parseHHMM(shift.start_time);
    var endMin = parseHHMM(shift.end_time);
    var startHour = Math.floor(startMin / 60);
    var endHour = Math.floor((endMin - 1) / 60);
    if (endMin % 60 === 0) endHour = (endMin / 60) - 1;
    if (endMin <= startMin) endHour += 24;
    var hours = [];
    for (var h = startHour; h <= endHour; h++) {
        hours.push(h % 24);
    }
    return hours;
}

var _chartColors = ['#7C7CF0', '#2DD4BF', '#FACC5B'];

function roundUp10(n) {
    return Math.ceil(n / 10) * 10;
}

function renderShiftChart() {
    var canvas = document.getElementById('shift-production-chart');
    var dateEl = document.getElementById('shift-graph-date');
    if (!canvas) return;
    if (dateEl) dateEl.textContent = _chartData.todayDate;

    var shifts = _chartData.shifts || [];
    var counts = _chartData.hourlyCounts || {};
    if (shifts.length === 0) {
        var ctx = canvas.getContext('2d');
        canvas.width = canvas.offsetWidth * (window.devicePixelRatio || 1);
        canvas.height = 260 * (window.devicePixelRatio || 1);
        ctx.scale(window.devicePixelRatio || 1, window.devicePixelRatio || 1);
        ctx.fillStyle = 'var(--text-muted)';
        ctx.font = '14px sans-serif';
        ctx.textAlign = 'center';
        ctx.fillText('No shifts configured', canvas.offsetWidth / 2, 130);
        return;
    }

    var dpr = window.devicePixelRatio || 1;
    var W = canvas.offsetWidth;
    var H = 260;
    canvas.width = W * dpr;
    canvas.height = H * dpr;

    var ctx = canvas.getContext('2d');
    ctx.scale(dpr, dpr);

    var padL = 40, padR = 20, padT = 26, padB = 54;
    var plotW = W - padL - padR;
    var plotH = H - padT - padB;

    var numHours = 8;
    var numVisibleShifts = shifts.length;
    var maxVal = 0;
    var grandTotal = 0;

    for (var si = 0; si < numVisibleShifts; si++) {
        var hours = shiftClockHours(shifts[si]);
        for (var hi = 0; hi < hours.length; hi++) {
            var v = counts[hours[hi]] || 0;
            if (v > maxVal) maxVal = v;
            grandTotal += v;
        }
    }
    if (maxVal === 0) maxVal = 10;
    maxVal = roundUp10(maxVal * 1.15);
    var yStep = 25;
    maxVal = Math.ceil(maxVal / yStep) * yStep;

    var resolved = getComputedStyle(document.documentElement);
    var textColor = resolved.getPropertyValue('--text').trim() || '#e6edf3';
    var mutedColor = resolved.getPropertyValue('--text-muted').trim() || '#8b949e';
    var borderColor = resolved.getPropertyValue('--border').trim() || '#30363d';

    ctx.clearRect(0, 0, W, H);

    ctx.strokeStyle = borderColor;
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(padL, padT);
    ctx.lineTo(padL, padT + plotH);
    ctx.lineTo(W - padR, padT + plotH);
    ctx.stroke();

    ctx.save();
    ctx.translate(12, padT + plotH / 2);
    ctx.rotate(-Math.PI / 2);
    ctx.fillStyle = textColor;
    ctx.font = 'bold 12px sans-serif';
    ctx.textAlign = 'center';
    ctx.fillText('Produced Parts', 0, 0);
    ctx.restore();

    ctx.strokeStyle = borderColor;
    ctx.lineWidth = 0.5;
    ctx.fillStyle = mutedColor;
    ctx.font = '11px sans-serif';
    ctx.textAlign = 'right';
    for (var g = 0; g <= maxVal; g += yStep) {
        var y = padT + plotH - (g / maxVal) * plotH;
        ctx.beginPath();
        ctx.moveTo(padL, y);
        ctx.lineTo(W - padR, y);
        ctx.stroke();
        ctx.fillText(g, padL - 6, y + 4);
    }

    var groupW = plotW / numHours;
    var barPad = 2;
    var barW = numVisibleShifts > 0 ? (groupW - barPad * 2) / numVisibleShifts : groupW - barPad * 2;
    barW = Math.max(4, barW - 2);

    for (var hr = 1; hr <= numHours; hr++) {
        var groupX = padL + (hr - 1) * groupW;

        ctx.fillStyle = mutedColor;
        ctx.font = '10px sans-serif';
        ctx.textAlign = 'center';
        ctx.fillText(String(hr), groupX + groupW / 2, H - 26);

        for (var si3 = 0; si3 < numVisibleShifts; si3++) {
            var shift = shifts[si3];
            var clockHours = shiftClockHours(shift);
            var clockHour = clockHours[hr - 1];
            if (clockHour === undefined) continue;
            var count = counts[clockHour] || 0;
            var barH = (count / maxVal) * plotH;
            var x = groupX + barPad + si3 * (barW + 2);
            var y = padT + plotH - barH;

            var color = _chartColors[si3 % _chartColors.length];
            ctx.fillStyle = color;
            ctx.fillRect(x, y, barW, barH);

            if (count > 0 && barH > 14) {
                ctx.fillStyle = '#000';
                ctx.font = 'bold 9px sans-serif';
                ctx.textAlign = 'center';
                ctx.textBaseline = 'middle';
                ctx.fillText(String(count), x + barW / 2, y + barH / 2);
                ctx.textBaseline = 'alphabetic';
            }
        }
    }

    ctx.fillStyle = textColor;
    ctx.font = 'bold 12px sans-serif';
    ctx.textAlign = 'center';
    ctx.fillText('Shift Hours', padL + plotW / 2, H - 28);

    var legendX = padL;
    for (var si4 = 0; si4 < shifts.length; si4++) {
        var lbl = shifts[si4].name || ('Shift ' + shifts[si4].shift_number);
        var c = _chartColors[si4 % _chartColors.length];
        ctx.fillStyle = c;
        ctx.fillRect(legendX, H - 18, 10, 10);
        ctx.fillStyle = textColor;
        ctx.font = '10px sans-serif';
        ctx.textAlign = 'left';
        var tw = ctx.measureText(lbl).width;
        ctx.fillText(lbl, legendX + 14, H - 9);
        legendX += 14 + tw + 12;
    }

    ctx.fillStyle = textColor;
    ctx.font = 'bold 12px sans-serif';
    ctx.textAlign = 'right';
    ctx.fillText('Total: ' + grandTotal, W - padR, padT - 6);
}

async function onShiftProcessChange() {
    var processID = parseInt(document.getElementById('shift-process-select').value, 10);
    if (!processID) return;
    _chartData.activeProcessID = processID;
    try {
        var today = new Date();
        var todayStr = today.getFullYear() + '-' + String(today.getMonth()+1).padStart(2,'0') + '-' + String(today.getDate()).padStart(2,'0');
        _chartData.todayDate = todayStr;
        _chartData.hourlyCounts = await api.get('/api/hourly-counts?process_id=' + processID + '&date=' + todayStr) || {};
        if (!_chartData.hourlyCounts || typeof _chartData.hourlyCounts !== 'object') _chartData.hourlyCounts = {};
    } catch(e) {
        _chartData.hourlyCounts = {};
    }
    renderShiftChart();
}

renderShiftChart();

createSSE('/events', {
    onCounterUpdate: function(data) {
        if (data.process_id !== _chartData.activeProcessID) return;
        var today = new Date();
        var todayStr = today.getFullYear() + '-' + String(today.getMonth()+1).padStart(2,'0') + '-' + String(today.getDate()).padStart(2,'0');
        if (_chartData.todayDate !== todayStr) return;
        var hour = today.getHours();
        var delta = data.delta || 0;
        if (!_chartData.hourlyCounts[hour]) _chartData.hourlyCounts[hour] = 0;
        _chartData.hourlyCounts[hour] += delta;
        renderShiftChart();
    },
    onCoreNodes: function(data) {
        var nodes = (data.nodes || []).map(function(n) {
            if (typeof n === 'string') return n;
            return n && n.name ? n.name : '';
        }).filter(Boolean);
        ['mo-pickup', 'mo-delivery', 'mo-staging'].forEach(function(selID) {
            var sel = document.getElementById(selID);
            if (!sel) return;
            var cur = sel.value;
            Array.from(sel.options).forEach(function(o) {
                if (o.dataset.source === 'core') o.remove();
            });
            var ref = sel.options[1] || null;
            nodes.sort().forEach(function(n) {
                var opt = document.createElement('option');
                opt.value = n;
                opt.textContent = n;
                opt.dataset.source = 'core';
                sel.insertBefore(opt, ref);
            });
            sel.value = cur;
        });
    }
});

// ─── Manual order ────────────────────────────────────────

(function() {
    var _pd = document.getElementById('page-data');
    if (!_pd) return;
    var edgeNodes = JSON.parse(_pd.dataset.nodes || '[]');
    var coreNodesRaw = JSON.parse(_pd.dataset.coreNodes || '[]');
    function coreNodeName(entry) {
        if (!entry) return '';
        if (typeof entry === 'string') return entry;
        if (typeof entry.name === 'string') return entry.name;
        if (typeof entry.node_id === 'string') return entry.node_id;
        return '';
    }
    var coreType = {};
    coreNodesRaw.forEach(function(e) {
        var nm = coreNodeName(e);
        if (nm && e && typeof e === 'object' && e.node_type) coreType[nm] = e.node_type;
    });
    var coreNodes = coreNodesRaw.map(coreNodeName).filter(Boolean);

    var seen = {};
    var merged = [];
    coreNodes.sort().forEach(function(n) {
        seen[n] = true;
        var edge = edgeNodes.find(function(e){ return e.id === n; });
        merged.push({ id: n, desc: edge ? edge.desc : '', source: 'core', type: coreType[n] || '' });
    });
    edgeNodes.forEach(function(e) {
        if (!seen[e.id]) {
            merged.push({ id: e.id, desc: e.desc, source: 'local', type: '' });
        }
    });

    function nodeOptionLabel(n) {
        var t = (n.type || '').toUpperCase();
        if (t === 'NGRP' || t === 'LANE') {
            return n.id + '  \u00b7 ' + (t === 'LANE' ? 'lane' : 'group');
        }
        var label = (n.id.indexOf('.') !== -1) ? '\u00a0\u00a0\u21b3 ' + n.id : n.id;
        if (n.desc) label += ' \u2014 ' + n.desc;
        if (n.source === 'local') label += ' (local)';
        return label;
    }

    ['mo-pickup', 'mo-delivery', 'mo-staging'].forEach(function(selID) {
        var sel = document.getElementById(selID);
        if (!sel) return;
        merged.forEach(function(n) {
            var opt = document.createElement('option');
            opt.value = n.id;
            opt.dataset.source = n.source;
            opt.textContent = nodeOptionLabel(n);
            sel.appendChild(opt);
        });
    });
})();

function updateOrderForm() {
    var t = document.getElementById('mo-type').value;
    var showNode    = t !== 'move';
    var showPickup  = t === 'move';
    var showDeliv   = t === 'move' || t === 'retrieve' || t === 'complex';
    var showStaging = t === 'retrieve' || t === 'complex';

    document.getElementById('mo-node-group').style.display    = showNode    ? '' : 'none';
    document.getElementById('mo-pickup-group').style.display   = showPickup  ? '' : 'none';
    document.getElementById('mo-delivery-group').style.display = showDeliv   ? '' : 'none';
    document.getElementById('mo-staging-group').style.display  = showStaging ? '' : 'none';

    document.getElementById('mo-delivery-label').textContent =
        (t === 'complex') ? 'Core Node' : 'Delivery Node';

    if (!showNode)    document.getElementById('mo-node').selectedIndex = 0;
    if (!showPickup)  document.getElementById('mo-pickup').selectedIndex = 0;
    if (!showDeliv)   document.getElementById('mo-delivery').selectedIndex = 0;
    if (!showStaging) document.getElementById('mo-staging').selectedIndex = 0;

    autofillNodeDefaults();
}

function autofillNodeDefaults() {
    var sel = document.getElementById('mo-node');
    var opt = sel.options[sel.selectedIndex];
    if (!opt || !opt.value) return;
    var coreNode = opt.dataset.coreNode || '';
    if (!coreNode) return;
    var t = document.getElementById('mo-type').value;
    if (t === 'retrieve') {
        document.getElementById('mo-delivery').value = coreNode;
    }
}

async function createOrder() {
    var t = document.getElementById('mo-type').value;
    var processNodeID = parseInt(document.getElementById('mo-node').value) || 0;
    var qty = parseInt(document.getElementById('mo-qty').value) || 1;

    if (t === 'complex') {
        var stagingNode = document.getElementById('mo-staging').value;
        var productionNode = document.getElementById('mo-delivery').value;
        if (!stagingNode || !productionNode) {
            toast('Staging and production nodes are required', 'error');
            return;
        }
        var body = {
            process_node_id: processNodeID || null,
            quantity: qty,
            steps: [
                {action: 'pickup', node: stagingNode},
                {action: 'dropoff', node: stagingNode},
                {action: 'wait'},
                {action: 'pickup', node: stagingNode},
                {action: 'dropoff', node: productionNode}
            ]
        };
        try {
            await api.post('/api/orders/complex', body);
            toast('Complex order created', 'success');
            resetOrderForm();
        } catch (e) { toast('Error: ' + e, 'error'); }
        return;
    }

    var body = {
        process_node_id: processNodeID || null,
        quantity: qty,
        delivery_node: document.getElementById('mo-delivery').value,
        source_node: document.getElementById('mo-pickup').value,
        staging_node: document.getElementById('mo-staging').value || undefined
    };
    try {
        await api.post('/api/orders/' + t, body);
        toast('Order created', 'success');
        resetOrderForm();
    } catch (e) { toast('Error: ' + e, 'error'); }
}

function resetOrderForm() {
    document.getElementById('mo-node').selectedIndex = 0;
    document.getElementById('mo-qty').value = '1';
    document.getElementById('mo-pickup').selectedIndex = 0;
    document.getElementById('mo-delivery').selectedIndex = 0;
    document.getElementById('mo-staging').selectedIndex = 0;
}

async function syncNodes() {
    var btn = document.getElementById('sync-nodes-btn');
    btn.disabled = true;
    btn.textContent = 'Syncing...';
    try {
        await api.post('/api/core-nodes/sync');
        toast('Node sync requested', 'success');
    } catch (e) { toast('Sync failed: ' + e, 'error'); }
    setTimeout(function() { btn.disabled = false; btn.textContent = 'Sync Nodes'; }, 2000);
}

updateOrderForm();

// ─── delegated event handlers ─────────────────────────
// All page-level data-action verbs route through delegateActions
// on document.body. Multiple event types share the same handler
// map — most handlers are click-only but a few (e.g. updatePreview)
// are referenced via data-action-change / data-action-input too,
// so binding the map across every event type keeps the page wiring
// single-source.
delegateActions(document.body, {
    autofillNodeDefaults,
    clearLinesideBucket,
    closeLoadBinModal,
    createOrder,
    editLinesideBucket,
    ensureLoadBinCatalog,
    navigateToProcess,
    onLoadPayloadChanged,
    onShiftProcessChange,
    openLoadBinModal,
    openRequestEmptyModal,
    releaseNodeWithPrompt,
    resetOrderForm,
    submitLoadBin,
    submitRequestEmpty,
    syncNodes,
    updateOrderForm,
    viewBinContents
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
