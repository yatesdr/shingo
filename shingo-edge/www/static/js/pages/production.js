import { api, navigateToProcess, refreshOnEvents } from '/static/js/shingoedge.js';
import { onSSE } from '/static/shared/bus.js';
import { delegateActions, escapeHtml } from '/static/shared/dom.js';
import { confirm, hideModal, prompt, showModal } from '/static/shared/layer.js';
import { nodePicker, nodeSource, nodeWhy } from '/static/shared/node-picker.js';
import { toast } from '/static/shared/notice.js';
import { postAct } from '/static/js/ask-act.js';

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
        toast('Error: ' + e.message, 'error');
    }
}

// binContents is the word for what a bin holds, from its row in the bins
// table: stocked (above zero) and partial (above zero, below its part's
// capacity). A held partial says its declared count.
function binContents(binState) {
    // OWNER-WORDS
    if (binState.held_since) return 'Partial, held (' + (binState.uop_remaining || 0) + ' declared)';
    // OWNER-WORDS
    if (!binState.stocked) return 'Empty';
    // OWNER-WORDS
    if (binState.partial) return 'Partial';
    // OWNER-WORDS
    return binState.uop_capacity > 0 ? 'Full' : 'Has parts';
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
    // The bins table carries whether the bin holds parts and whether fewer
    // than its part's capacity, never a count (binContents).
    // OWNER-WORDS
    html += '<div><div style="color:var(--text-muted);font-size:0.8rem">Contents</div><strong>' + binContents(binState) + '</strong></div>';
    html += '<div><div style="color:var(--text-muted);font-size:0.8rem">Bin Type</div>' + escapeHtml(binState.bare ? 'Cart, no bin' : (binState.bin_type_code || '-')) + '</div>';
    html += '<div><div style="color:var(--text-muted);font-size:0.8rem">Confirmed</div>' + (binState.manifest_confirmed ? 'Yes' : 'No') + '</div>';
    html += '</div>';
    if (binState.manifest) {
        try {
            var manifest = typeof binState.manifest === 'string' ? JSON.parse(binState.manifest) : binState.manifest;
            var items = manifest.items || [];
            if (items.length > 0) {
                // Parts only. The manifest carries no count, and neither does
                // the bins table this page reads (Contents above says whether
                // the bin holds parts). A "Qty" column reading item.qty would
                // print 0 for every line, which is worse than not printing it.
                html += '<table class="table" style="font-size:0.85rem"><thead><tr><th>Part</th></tr></thead><tbody>';
                items.forEach(function(item) {
                    html += '<tr><td>' + escapeHtml(item.catid || item.part_number || '') + '</td></tr>';
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
        var answer = await postAct('/api/process-nodes/' + nodeID + '/request-empty', {payload_code: payloadCode});
        hideModal('request-empty-modal');
        // A 202 pending answer: sent to Core, not known to be done (ask-act.js).
        if (answer && answer.pending) { toast(answer.notice, 'info'); return; }
        toast('Empty bin requested', 'success');
    } catch(e) {
        toast('Error: ' + e, 'error');
    }
}

var _loadBinCatalog = null;

// ensureLoadBinCatalog reads the catalog once per page. A failed read is not
// kept: this open shows the codes alone and the next open reads again.
async function ensureLoadBinCatalog() {
    if (_loadBinCatalog) return _loadBinCatalog;
    var got;
    try { got = await api.get('/api/payload-catalog'); } catch(_) { return []; }
    _loadBinCatalog = Array.isArray(got) ? got : [];
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
        // The input is a COUNT — how many of the part are in the carrier. The
        // template supplies a per-cycle RATIO, so the default is that ratio
        // times a full bin's cycles, which is the number this field defaulted
        // to before the rename. It stays editable: this form is the operator
        // saying what is actually in the bin.
        items.forEach(function(item) {
            var perCycle = item.parts_per_cycle || 0;
            var row = document.createElement('div');
            row.style.cssText = 'display:grid;grid-template-columns:1fr 80px;gap:0.5rem;align-items:center;margin-bottom:0.5rem;padding:0.5rem;border:1px solid var(--border);border-radius:4px';
            row.innerHTML =
                '<div><div style="font-weight:500">' + escapeHtml(item.part_number) + '</div>' +
                '<div style="color:var(--text-muted);font-size:0.85rem">' + escapeHtml(item.description || '') + '</div></div>' +
                '<input type="number" class="form-input rb-manifest-qty" value="' + (perCycle * uopCapacity) + '" ' +
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
        // An undeclared count is OMITTED, not sent as 0. Absence asks Core for
        // the payload's standard pack; a 0 is a bin somebody counted as empty,
        // and this field has no room to mean both. The operator station has its
        // own guard and never submits without a count; this page's field is
        // optional and may not be rendered at all.
        var body = {payload_code: payloadCode, manifest: manifest};
        var uopEl = document.getElementById('rb-uop-count');
        var uopRaw = uopEl ? String(uopEl.value).trim() : '';
        if (uopRaw !== '') {
            var uopCount = parseInt(uopRaw, 10);
            if (isNaN(uopCount)) { toast('UoP count must be a number', 'warning'); return; }
            body.uop_count = uopCount;
        }
        var answer = await postAct('/api/process-nodes/' + nodeID + '/load-bin', body);
        closeLoadBinModal();
        // A 202 pending answer: sent to Core, not known to be done (ask-act.js).
        if (answer && answer.pending) { toast(answer.notice, 'info'); return; }
        toast('Bin loaded', 'success');
    } catch(e) {
        toast('Error: ' + e, 'error');
    }
}

// ─── Lineside bucket handlers ───────────────────────────

async function clearLinesideBucket() {
    var tr = this.closest('tr');
    var bucketID = tr.getAttribute('data-bucket-id');
    if (!await confirm('Clear this lineside bucket? The chip will vanish from the operator HMI.')) {
        return;
    }
    try {
        await api.post('/api/lineside/buckets/' + encodeURIComponent(bucketID) + '/clear');
        // THE ROW LEAVES ON THE CLICK, not on the next reload. This table sits
        // OUTSIDE the page's htmx swap region (#production-content wraps the
        // production-body partial only), so the SSE-driven refreshes never
        // re-render it - before this, a cleared bucket stayed on screen until
        // a full page load, and the toast was promising something the screen
        // denied. When the last bucket goes, the server's empty-state row
        // takes its place.
        var tbody = tr.closest('tbody');
        tr.remove();
        if (tbody && !tbody.querySelector('tr[data-bucket-id]')) {
            tbody.insertAdjacentHTML('beforeend',
                '<tr><td colspan="7" class="empty-cell" style="text-align:center;padding:1rem;">No lineside buckets - no parts currently kitted at any node.</td></tr>');
        }
        toast('Lineside bucket cleared', 'success');
    } catch(e) {
        toast('Error: ' + e.message, 'error');
    }
}

// ─── Shift production chart ─────────────────────────────

var _chartData = {};
try {
    var _pd = document.getElementById('page-data');
    if (_pd) {
        _chartData.shifts = JSON.parse(_pd.dataset.shifts || '[]');
        _chartData.hourlyCounts = JSON.parse(_pd.dataset.prodCounts || '{}');
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

// niceStep picks a "human-friendly" Y-axis grid step (1, 2, 5, 10, 20,
// 50, 100, 200, 500, 1000, ...) for a given target value so the axis
// always shows ~5-8 evenly-spaced labels regardless of magnitude.
function niceStep(target) {
    if (target <= 0) return 1;
    var exp = Math.floor(Math.log10(target));
    var base = Math.pow(10, exp);
    var norm = target / base; // 1.0 – 10.0
    var step;
    if (norm <= 1.5) step = 1;
    else if (norm <= 3) step = 2;
    else if (norm <= 7) step = 5;
    else step = 10;
    return step * base;
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
        // A canvas cannot read a CSS variable: resolve it, as the chart does.
        ctx.fillStyle = getComputedStyle(document.documentElement).getPropertyValue('--text-muted').trim() || '#8b949e';
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

    // One column per hour of the longest shift, not a fixed 8.
    var numHours = 0;
    for (var sh = 0; sh < shifts.length; sh++) {
        numHours = Math.max(numHours, shiftClockHours(shifts[sh]).length);
    }
    if (numHours < 1) numHours = 1;
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
    // Dynamically scale the Y-axis step so we always get ~5-8 gridlines,
    // regardless of whether maxVal is 50 or 50,000. Picks a "nice" step
    // (1, 2, 5, 10, 20, 50, 100, 200, 500, ...) based on magnitude.
    var yStep = niceStep(maxVal / 6);
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
    var sel = document.getElementById('shift-process-select');
    var rawVal = sel.value;
    var isAll = rawVal === 'all';
    var processID = parseInt(rawVal, 10);
    if (!isAll && !processID) return;
    _chartData.activeProcessID = isAll ? 0 : processID;
    try {
        var today = new Date();
        var todayStr = today.getFullYear() + '-' + String(today.getMonth()+1).padStart(2,'0') + '-' + String(today.getDate()).padStart(2,'0');
        _chartData.todayDate = todayStr;
        _chartData.hourlyCounts = await api.get('/api/hourly-counts?process_id=' + encodeURIComponent(rawVal) + '&date=' + todayStr) || {};
        if (!_chartData.hourlyCounts || typeof _chartData.hourlyCounts !== 'object') _chartData.hourlyCounts = {};
    } catch(e) {
        _chartData.hourlyCounts = {};
    }
    renderShiftChart();
}

renderShiftChart();

// The page's one stream (shared/bus.js). #production-content's hx-trigger
// sse: events first (refreshOnEvents), so the content refresh fires before
// the page's own handler for the same frame, as it did; then the shift
// chart's counter and the manual-order node lists. The new-build banner is
// the hub's.
refreshOnEvents('production-content', onSSE);
onSSE('counter-update', function(data) {
    // When viewing "All Processes" (activeProcessID = 0), accept
    // counter updates from any process. Otherwise only match the
    // selected process.
    if (_chartData.activeProcessID && data.process_id !== _chartData.activeProcessID) return;
    var today = new Date();
    var todayStr = today.getFullYear() + '-' + String(today.getMonth()+1).padStart(2,'0') + '-' + String(today.getDate()).padStart(2,'0');
    if (_chartData.todayDate !== todayStr) return;
    var hour = today.getHours();
    var delta = data.delta || 0;
    if (!_chartData.hourlyCounts[hour]) _chartData.hourlyCounts[hour] = 0;
    _chartData.hourlyCounts[hour] += delta;
    renderShiftChart();
});
// Core's node list changed: the manual-order form's one list is read again,
// and a picked node that is no longer on it is unpicked, as a select left on
// no option sends none.
onSSE('core-nodes', function() {
    nodes.load(true).then(function(rows) {
        var names = {};
        (rows || []).forEach(function(n) { names[n.name] = true; });
        Object.keys(pickers).forEach(function(k) {
            if (pickers[k].value() && !names[pickers[k].value()]) pickers[k].set('');
        });
    });
});

// ─── Manual order ────────────────────────────────────────

// THE FORM'S ONE NODE LIST (shared/node-picker.js), as the Manual order page
// keeps it: Core's nodes as the page was rendered with them, read again from
// /api/core-nodes on an SSE core-nodes frame (the frame names nodes and types,
// not groups), and this Edge's process nodes Core does not list ("this Edge
// only"). A process node's name is the note on its Core node.
var _pd = document.getElementById('page-data').dataset;
var edgeNodes = JSON.parse(_pd.nodes || '[]');
// memberOf is each member of a node group, by name, to its group.
var memberOf = {};
function withEdgeNodes(core) {
    var seen = {};
    memberOf = {};
    var rows = (core || []).filter(function(n) { return n && n.name; }).map(function(n) {
        seen[n.name] = true;
        if (n.group) memberOf[n.name] = n.group;
        var edge = edgeNodes.find(function(e) { return e.id === n.name; });
        return Object.assign({}, n, { desc: edge ? edge.desc : '' });
    });
    edgeNodes.forEach(function(e) {
        if (e.id && !seen[e.id]) { seen[e.id] = true; rows.push({ name: e.id, desc: e.desc, source: 'local' }); }
    });
    return rows;
}
var nodes = nodeSource(function() { return api.get('/api/core-nodes').then(withEdgeNodes); },
    withEdgeNodes(JSON.parse(_pd.coreNodes || '[]')));

// A MEMBER OF A GROUP IS CORE'S TO PICK: an order's pickup and delivery name
// the group (the API refuses a member). The delivery takes one only for a
// complex order, whose Core Node is the cell's own position. No field takes a
// robot.
var W = nodeWhy;
var pickers = {
    pickup: nodePicker(document.getElementById('mo-pickup'), nodes,
        { rule: W.any(W.robot, W.member) }, { placeholder: 'Source node' }),
    delivery: nodePicker(document.getElementById('mo-delivery'), nodes,
        { rule: W.any(W.robot, function(n) { return orderType() === 'complex' ? '' : W.member(n); }) },
        { placeholder: 'Delivery node' }),
    staging: nodePicker(document.getElementById('mo-staging'), nodes,
        { rule: W.robot }, { placeholder: 'Staging node' }),
};

function orderType() { return document.getElementById('mo-type').value; }

function updateOrderForm() {
    var t = orderType();
    if (t !== 'complex' && memberOf[pickers.delivery.value()]) pickers.delivery.set('');
    pickers.delivery.refresh();
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
    if (!showPickup)  pickers.pickup.set('');
    if (!showDeliv)   pickers.delivery.set('');
    if (!showStaging) pickers.staging.set('');

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
        pickers.delivery.set(coreNode);
    }
}

async function createOrder() {
    var t = document.getElementById('mo-type').value;
    var processNodeID = parseInt(document.getElementById('mo-node').value) || 0;
    var qty = parseInt(document.getElementById('mo-qty').value) || 1;

    if (t === 'complex') {
        var stagingNode = pickers.staging.value();
        var productionNode = pickers.delivery.value();
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
        } catch (e) { toast('Error: ' + e.message, 'error'); }
        return;
    }

    var body = {
        process_node_id: processNodeID || null,
        quantity: qty,
        delivery_node: pickers.delivery.value(),
        source_node: pickers.pickup.value(),
        staging_node: pickers.staging.value() || undefined
    };
    try {
        // At a loader's window the order is Core's to make (the loader ask): a
        // 202 pending answer is sent to Core, not known to be done (ask-act.js).
        var answer = await postAct('/api/orders/' + t, body);
        if (answer && answer.pending) { toast(answer.notice, 'info'); resetOrderForm(); return; }
        toast('Order created', 'success');
        resetOrderForm();
    } catch (e) { toast('Error: ' + e, 'error'); }
}

function resetOrderForm() {
    document.getElementById('mo-node').selectedIndex = 0;
    document.getElementById('mo-qty').value = '1';
    pickers.pickup.set('');
    pickers.delivery.set('');
    pickers.staging.set('');
}

async function syncNodes() {
    var btn = document.getElementById('sync-nodes-btn');
    btn.disabled = true;
    btn.textContent = 'Syncing...';
    try {
        await api.post('/api/core-nodes/sync');
        toast('Node sync requested', 'success');
    } catch (e) { toast('Sync failed: ' + e.message, 'error'); }
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
    hideModal,
    navigateToProcess,
    onLoadPayloadChanged,
    onShiftProcessChange,
    openLoadBinModal,
    openRequestEmptyModal,
    releaseNodeWithPrompt,
    submitLoadBin,
    submitRequestEmpty,
    syncNodes,
    updateOrderForm,
    viewBinContents
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
