import { api, delegateActions, escapeHtml, hideModal, navigateToProcess, prompt, showModal, toast } from '/static/js/shingoedge.js';

// Material page — operator-facing actions for the per-process node grid.
//
// Extracted from inline <script> blocks in templates/material.html as
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
    html += '<div><div style="color:var(--text-muted);font-size:0.8rem">UOP Remaining</div><strong>' + (binState.uop_remaining || 0) + '</strong></div>';
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
        uopRow.innerHTML = '<div style="font-weight:600">UOP Count</div>' +
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

// ─── delegated event handlers ─────────────────────────
// All page-level data-action verbs route through delegateActions
// on document.body. Multiple event types share the same handler
// map — most handlers are click-only but a few (e.g. updatePreview)
// are referenced via data-action-change / data-action-input too,
// so binding the map across every event type keeps the page wiring
// single-source.
delegateActions(document.body, {
    closeLoadBinModal,
    ensureLoadBinCatalog,
    navigateToProcess,
    onLoadPayloadChanged,
    openLoadBinModal,
    openRequestEmptyModal,
    releaseNodeWithPrompt,
    submitLoadBin,
    submitRequestEmpty,
    viewBinContents
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
