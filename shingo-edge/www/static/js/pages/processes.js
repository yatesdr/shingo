import { api, confirm, delegateActions, escapeHtml, hideModal, showModal, tagSelect, toast } from '/static/js/shingoedge.js';

// Processes admin page — process / style / node-claim / operator-station
// editors driven by inline onclick handlers in processes.html. Functions
// referenced from those handlers stay window-attached.
//
// This file is the worked example of the form-state convention (see
// docs/ui-style-guide.md "Forms"). The claim editor is the non-trivial
// section: it pins behavior via shingo-edge/www/static/js/pages/processes.characterization.test.js
// (337 assertions covering every role × swap_mode cell + saveClaim
// payload shape). Any silent change to which fields show/require/POST
// fails CI before deploy.
//
// Conventions used:
//   - claimState holds form values in one object — no scattered
//     getElementById calls.
//   - render(state) drives DOM from state. Conditional visibility
//     comes from CLAIM_FIELD_VISIBILITY, a (role, swap) → visibility-map
//     lookup. The 31 imperative style.display toggles from the prior
//     version collapse to one function plus one table.
//   - readClaimStateFromForm() snapshots DOM back into state.
//   - validateClaimState(state) is pure — same input, same output. The
//     panel called this out as the single highest-value behavior to
//     pin since it's where claim-editor regressions hide.
//   - saveClaim() runs the read → validate → POST pipeline.

const activeProcessID = parseInt(document.getElementById('page-data').dataset.activeProcessId || '0', 10);
const claimedByStation = window.claimedByStation || {};

// ─── Process editor ─────────────────────────────────────────────────────

function resetProcessForm() {
    document.getElementById('new-process-name').value = '';
    document.getElementById('new-process-description').value = '';
    var el = document.getElementById('new-process-counter-tag');
    if (el) el.value = '';
    var sel = document.getElementById('new-process-counter-plc');
    if (sel) sel.selectedIndex = 0;
}

function openCreateProcessModal() {
    resetProcessForm();
    document.getElementById('process-modal-title').textContent = 'Add Process';
    showModal('process-modal');
}

function closeProcessModal() {
    hideModal('process-modal');
    resetProcessForm();
}

function showProcessTab(tab) {
    document.querySelectorAll('.process-tab').forEach(function(button) {
        button.classList.toggle('btn-primary', button.dataset.tab === tab);
        button.classList.toggle('active', button.dataset.tab === tab);
    });
    document.querySelectorAll('.process-tab-panel').forEach(function(panel) {
        panel.style.display = panel.id === 'process-tab-' + tab ? 'block' : 'none';
    });
}

async function createProcess() {
    const name = document.getElementById('new-process-name').value.trim();
    if (!name) {
        toast('Enter a process name', 'warning');
        return;
    }
    const counterPLC = document.getElementById('new-process-counter-plc').value;
    const counterTag = document.getElementById('new-process-counter-tag').value.trim();
    try {
        const res = await api.post('/api/processes', {
            name: name,
            description: document.getElementById('new-process-description').value.trim(),
            production_state: 'active_production',
            counter_plc_name: counterPLC,
            counter_tag_name: counterTag,
            counter_enabled: !!(counterPLC && counterTag)
        });
        // Auto-create a Default style and set it active
        try {
            const style = await api.post('/api/styles', {
                name: 'Default',
                description: 'Default style',
                process_id: res.id
            });
            await api.put('/api/processes/' + res.id + '/active-style', {
                style_id: style.id
            });
        } catch (e) {
            toast('Process created but default style setup failed: ' + e, 'warning');
        }
        window.location = '/processes?process=' + res.id;
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

async function saveProcess() {
    try {
        await api.put('/api/processes/' + activeProcessID, {
            name: document.getElementById('process-name').value.trim(),
            description: document.getElementById('process-description').value.trim(),
            production_state: document.getElementById('process-production-state').value,
            counter_plc_name: document.getElementById('counter-plc') ? document.getElementById('counter-plc').value : '',
            counter_tag_name: document.getElementById('counter-tag') ? document.getElementById('counter-tag').value.trim() : '',
            counter_enabled: document.getElementById('counter-enabled') ? document.getElementById('counter-enabled').checked : false,
            auto_cutover_enabled: document.getElementById('auto-cutover-enabled') ? document.getElementById('auto-cutover-enabled').checked : false
        });
        toast('Process saved', 'success');
        location.reload();
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

async function deleteProcess(id) {
    if (!await confirm('Delete this process and all of its station configuration?')) return;
    try {
        await api.del('/api/processes/' + id);
        window.location = '/processes';
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

// ─── Style editor ───────────────────────────────────────────────────────

function resetStyleForm() {
    document.getElementById('style-id').value = '';
    document.getElementById('style-name').value = '';
    document.getElementById('style-description').value = '';
}

function openCreateStyleModal() {
    resetStyleForm();
    document.getElementById('style-modal-title').textContent = 'Add Style';
    showModal('style-modal');
}

function closeStyleModal() {
    hideModal('style-modal');
    resetStyleForm();
}

function editStyle() {
    // Invoked via data-action="editStyle" with data-style-json="{{json .}}"
    // on the clicked button. Parse the style JSON off the element.
    var style = {};
    try { style = JSON.parse(this.dataset.styleJson || '{}') || {}; }
    catch (e) { style = {}; }
    resetStyleForm();
    document.getElementById('style-id').value = style.id;
    document.getElementById('style-name').value = style.name || '';
    document.getElementById('style-description').value = style.description || '';
    document.getElementById('style-modal-title').textContent = 'Edit Style';
    showModal('style-modal');
}

async function saveStyle() {
    const id = document.getElementById('style-id').value;
    const payload = {
        name: document.getElementById('style-name').value.trim(),
        description: document.getElementById('style-description').value.trim(),
        process_id: activeProcessID
    };
    if (!payload.name) {
        toast('Enter a style name', 'warning');
        return;
    }
    try {
        if (id) {
            await api.put('/api/styles/' + id, payload);
        } else {
            await api.post('/api/styles', payload);
        }
        closeStyleModal();
        location.reload();
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

async function deleteStyle(id) {
    if (!await confirm('Delete this style?')) return;
    try {
        await api.del('/api/styles/' + id);
        location.reload();
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

// Discoverability fix for Field-notes Note 6: a new process with
// styles defined but no active style has no operator-facing path to
// pick one. apiSetActiveStyle already exists; this wires the per-row
// "Set Active" button on the Styles table to it.
async function setActiveStyle(id) {
    const styleID = parseInt(id, 10);
    try {
        await api.put('/api/processes/' + activeProcessID + '/active-style', { style_id: styleID });
        location.reload();
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

// ─── Clone style ────────────────────────────────────────────────────────
// Scaffold one new style from an existing one (claims copied verbatim);
// payloads get set afterward in the Node Claims compare grid.

var _cloneSrcStyleID = 0;

function openCloneStyleModal() {
    // Invoked via data-action with data-style-json="{{json .}}" on the clicked row.
    var style = {};
    try { style = JSON.parse(this.dataset.styleJson || '{}') || {}; }
    catch (e) { style = {}; }
    _cloneSrcStyleID = style.id || 0;
    document.getElementById('clone-style-src-name').textContent = style.name || '';
    document.getElementById('clone-style-name').value = (style.name || '') + ' (copy)';
    document.getElementById('clone-style-description').value = style.description || '';
    showModal('clone-style-modal');
}

function closeCloneStyleModal() {
    hideModal('clone-style-modal');
    _cloneSrcStyleID = 0;
}

async function cloneStyle() {
    var name = document.getElementById('clone-style-name').value.trim();
    if (!name) { toast('Enter a name for the cloned style', 'warning'); return; }
    if (!_cloneSrcStyleID) { toast('No source style selected', 'error'); return; }
    try {
        await api.post('/api/styles/' + _cloneSrcStyleID + '/clone', {
            name: name,
            description: document.getElementById('clone-style-description').value.trim()
        });
        closeCloneStyleModal();
        toast('Style cloned — set payloads in Node Claims → Compare all', 'success');
        location.reload();
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

// ─── Generate variants ──────────────────────────────────────────────────
// Stamp out a family of styles from one base: the base's produce claims
// become grid columns, each variant row is a name + a payload per column.
// Capacity and allowed-codes derive from the chosen payload, so a variant
// is one value per produce node. POSTs the whole batch atomically.

var _generateColumns = [];   // [{coreNodeName, swapMode, payloadCode}]
var _generateRowSeq = 0;

async function openGenerateModal() {
    await loadPayloadCatalog();
    await buildGenerateColumns();
    _generateRowSeq = 0;
    renderGenerateGrid();
    addGenerateRow();
    showModal('generate-modal');
}

function closeGenerateModal() {
    hideModal('generate-modal');
}

async function onGenerateBaseChanged() {
    await buildGenerateColumns();
    renderGenerateGrid();   // rebuilds the table+tbody, clearing prior rows
    addGenerateRow();
}

// buildGenerateColumns fetches the selected base style's claims and keeps the
// produce ones as columns (falling back to every claim if the base has no
// produce claims). Each column carries the base payload as the cell default,
// so an untouched cell inherits the base.
async function buildGenerateColumns() {
    var baseID = parseInt(document.getElementById('generate-base').value, 10) || 0;
    _generateColumns = [];
    if (!baseID) return;
    try {
        var claims = await api.get('/api/styles/' + baseID + '/node-claims');
        if (!Array.isArray(claims)) claims = [];
        var produce = claims.filter(function(c) { return c.role === 'produce'; });
        var cols = produce.length ? produce : claims;
        _generateColumns = cols.map(function(c) {
            return { coreNodeName: c.core_node_name, swapMode: c.swap_mode, payloadCode: c.payload_code || '' };
        });
    } catch (e) {
        toast('Error loading base claims: ' + e, 'error');
    }
}

function renderGenerateGrid() {
    var wrap = document.getElementById('generate-grid-wrap');
    if (_generateColumns.length === 0) {
        wrap.innerHTML = '<div style="color:var(--text-muted);font-style:italic;padding:0.5rem 0">Base style has no node claims to set payloads on. Add claims to the base first.</div>';
        return;
    }
    var head = '<th>New style name</th>';
    _generateColumns.forEach(function(col) {
        head += '<th class="mono" style="font-size:0.8rem">' + escapeHtml(col.coreNodeName) + '</th>';
    });
    head += '<th style="width:1%"></th>';
    wrap.innerHTML =
        '<table class="table" style="margin:0"><thead><tr>' + head + '</tr></thead>' +
        '<tbody id="generate-grid-tbody"></tbody></table>';
}

function payloadOptionsHTML(selected) {
    var html = '<option value="">-- payload --</option>';
    _payloadCatalog.forEach(function(p) {
        var sel = p.code === selected ? ' selected' : '';
        html += '<option value="' + escapeHtml(p.code) + '"' + sel + '>' +
            escapeHtml(p.code + (p.uop_capacity ? ' (' + p.uop_capacity + ')' : '')) + '</option>';
    });
    return html;
}

function addGenerateRow() {
    var tbody = document.getElementById('generate-grid-tbody');
    if (!tbody) return;
    var rid = 'gen-row-' + (_generateRowSeq++);
    var tr = document.createElement('tr');
    tr.id = rid;
    var cells = '<td><input type="text" class="form-input gen-name" placeholder="e.g. 2001-DOOR" style="min-width:10rem"></td>';
    _generateColumns.forEach(function(col) {
        cells += '<td><select class="form-input gen-payload" data-node="' + escapeHtml(col.coreNodeName) +
            '" data-swap="' + escapeHtml(col.swapMode) + '">' + payloadOptionsHTML(col.payloadCode) + '</select></td>';
    });
    cells += '<td><button class="btn btn-sm btn-danger" type="button" data-action="removeGenerateRow:' + rid + '">&times;</button></td>';
    tr.innerHTML = cells;
    tbody.appendChild(tr);
}

function removeGenerateRow(rid) {
    var tr = document.getElementById(rid);
    if (tr) tr.remove();
}

function capacityForPayload(code) {
    var hit = _payloadCatalog.find(function(p) { return p.code === code; });
    return hit ? (hit.uop_capacity || 0) : 0;
}

async function generateStyles() {
    var baseID = parseInt(document.getElementById('generate-base').value, 10) || 0;
    if (!baseID) { toast('Pick a base style', 'warning'); return; }
    var rows = Array.prototype.slice.call(document.querySelectorAll('#generate-grid-tbody tr'));
    var variants = [];
    rows.forEach(function(row) {
        var name = row.querySelector('.gen-name').value.trim();
        if (!name) return;   // skip blank rows
        var overrides = [];
        row.querySelectorAll('.gen-payload').forEach(function(sel) {
            var code = sel.value;
            if (!code) return;   // leave this node's claim at the base payload
            var isManual = sel.dataset.swap === 'manual_swap';
            overrides.push({
                core_node_name: sel.dataset.node,
                // manual_swap stores '' in payload_code and drives off the
                // allowed set; every other mode binds payload_code directly.
                payload_code: isManual ? '' : code,
                uop_capacity: capacityForPayload(code),
                allowed_payload_codes: [code]
            });
        });
        variants.push({ name: name, description: '', overrides: overrides });
    });
    if (variants.length === 0) { toast('Enter at least one variant name', 'warning'); return; }
    try {
        var res = await api.post('/api/styles/' + baseID + '/generate', { variants: variants });
        closeGenerateModal();
        toast('Generated ' + (res && res.ids ? res.ids.length : variants.length) + ' styles', 'success');
        location.reload();
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

// ─── Node Claims: compare-all matrix ────────────────────────────────────
// "Compare all" pivots the per-style claim list into a matrix — rows = core
// nodes, columns = styles — so the payload (or capacity / reorder / lineside)
// can be set across the whole family in one place. Each cell edit writes that
// single claim through the same upsert the per-style editor uses; structural
// fields (staging, pairing, flags) stay in the one-style editor, reachable by
// clicking a style heading. Edits existing claims only — a missing cell is "—".

var _compareMode = false;
var _compareField = 'payload';
var _compareStyles = [];     // [{id, name, active}]
var _compareClaims = {};     // styleID -> { coreNode -> claim }

function compareStylesFromSelector() {
    var sel = document.getElementById('claims-style-selector');
    var out = [];
    if (!sel) return out;
    Array.prototype.forEach.call(sel.options, function(o) {
        var id = parseInt(o.value, 10);
        if (!id) return;
        out.push({ id: id, name: o.textContent.replace(/\s*\(active\)\s*$/, ''), active: /\(active\)/.test(o.textContent) });
    });
    return out;
}

function onCompareViewChanged() {
    var checked = document.querySelector('input[name="claims-view"]:checked');
    _compareMode = !!checked && checked.value === 'all';
    applyCompareMode();
}

function onCompareFieldChanged() {
    var sel = document.getElementById('compare-field');
    _compareField = sel ? sel.value : 'payload';
    if (_compareMode) renderCompareMatrix();
}

function applyCompareMode() {
    var one = !_compareMode;
    var ids = {
        'claims-list': !one,            // hidden when comparing
        'claims-compare-list': one,
        'compare-field-wrap': one,
        'compare-help': one,
    };
    Object.keys(ids).forEach(function(id) {
        var el = document.getElementById(id);
        if (el) el.hidden = ids[id];
    });
    var singleWrap = document.getElementById('claims-single-style-wrap');
    if (singleWrap) singleWrap.style.display = one ? '' : 'none';
    var addBtn = document.getElementById('claims-add-claim-btn');
    if (addBtn) addBtn.style.display = one ? '' : 'none';
    if (_compareMode) renderCompareMatrix();
}

async function renderCompareMatrix() {
    var wrap = document.getElementById('claims-compare-list');
    if (!wrap) return;
    wrap.innerHTML = '<div style="color:var(--text-muted);padding:0.5rem 0">Loading…</div>';
    await loadPayloadCatalog();
    _compareStyles = compareStylesFromSelector();
    if (_compareStyles.length === 0) { wrap.innerHTML = '<div class="empty-cell">No styles to compare.</div>'; return; }
    _compareClaims = {};
    await Promise.all(_compareStyles.map(function(s) {
        return api.get('/api/styles/' + s.id + '/node-claims').then(function(claims) {
            var byNode = {};
            (Array.isArray(claims) ? claims : []).forEach(function(c) { byNode[c.core_node_name] = c; });
            _compareClaims[s.id] = byNode;
        }).catch(function() { _compareClaims[s.id] = {}; });
    }));

    // Node row order: union across styles, ordered by sequence on first sight.
    var nodeOrder = [], seen = {};
    _compareStyles.forEach(function(s) {
        var byNode = _compareClaims[s.id] || {};
        Object.keys(byNode).sort(function(a, b) { return (byNode[a].sequence || 0) - (byNode[b].sequence || 0); }).forEach(function(n) {
            if (!seen[n]) { seen[n] = true; nodeOrder.push(n); }
        });
    });
    if (nodeOrder.length === 0) { wrap.innerHTML = '<div class="empty-cell">No node claims on these styles yet.</div>'; return; }

    var thead = '<th>Node</th>';
    _compareStyles.forEach(function(s) {
        thead += '<th><button class="btn btn-sm compare-style-jump" data-style="' + s.id + '" title="Open this style\'s full claim editor">' +
            escapeHtml(s.name) + (s.active ? ' ●' : '') + '</button></th>';
    });
    var body = '';
    nodeOrder.forEach(function(node) {
        var row = '<td class="mono" style="font-size:0.8rem;white-space:nowrap">' + escapeHtml(node) + '</td>';
        _compareStyles.forEach(function(s) {
            row += '<td>' + compareCellHTML(s.id, node, (_compareClaims[s.id] || {})[node]) + '</td>';
        });
        body += '<tr>' + row + '</tr>';
    });
    wrap.innerHTML = '<div style="overflow-x:auto"><table class="table" style="margin:0"><thead><tr>' + thead + '</tr></thead><tbody>' + body + '</tbody></table></div>';
    ensureCompareDelegation(wrap);
}

function compareCellHTML(styleID, node, c) {
    if (!c) return '<span style="color:var(--text-muted)">—</span>';
    var attrs = 'data-style="' + styleID + '" data-node="' + escapeHtml(node) + '"';
    if (_compareField === 'payload') {
        var primary = c.swap_mode === 'manual_swap'
            ? ((c.allowed_payload_codes && c.allowed_payload_codes[0]) || '')
            : (c.payload_code || '');
        return '<select class="form-input compare-cell" ' + attrs + ' data-kind="payload" style="min-width:9rem">' + payloadOptionsHTML(primary) + '</select>';
    }
    var val = c[_compareField] || 0;
    return '<input type="number" class="form-input compare-cell" ' + attrs + ' data-kind="num" value="' + val + '" min="0" style="max-width:6rem">';
}

function ensureCompareDelegation(wrap) {
    if (!wrap || wrap.dataset.delegated === '1') return;
    wrap.dataset.delegated = '1';
    wrap.addEventListener('change', function(e) {
        var cell = e.target.closest && e.target.closest('.compare-cell');
        if (cell && wrap.contains(cell)) saveCompareCell(cell);
    });
    wrap.addEventListener('click', function(e) {
        var jump = e.target.closest && e.target.closest('.compare-style-jump');
        if (jump && wrap.contains(jump)) jumpToStyleEditor(parseInt(jump.dataset.style, 10));
    });
}

// claimToBody maps a fetched claim to the upsert POST body, mirroring
// saveClaim's claimBody so a compare-grid edit preserves every field it does
// not touch (staging, pairing, flags). transitional_loader is omitted so the
// *bool "absent = leave untouched" contract holds.
function claimToBody(c) {
    return {
        style_id: c.style_id,
        core_node_name: c.core_node_name,
        role: c.role,
        swap_mode: c.swap_mode,
        payload_code: c.payload_code || '',
        allowed_payload_codes: c.allowed_payload_codes || [],
        uop_capacity: c.uop_capacity || 0,
        reorder_point: c.reorder_point || 0,
        lineside_soft_threshold: c.lineside_soft_threshold || 0,
        auto_reorder: true,
        inbound_staging: c.inbound_staging || '',
        outbound_staging: c.outbound_staging || '',
        inbound_source: c.inbound_source || '',
        outbound_destination: c.outbound_destination || '',
        auto_request_payload: c.auto_request_payload || '',
        keep_staged: false,
        evacuate_on_changeover: !!c.evacuate_on_changeover,
        reuse_compatible_bins: !!c.reuse_compatible_bins,
        auto_push: !!c.auto_push,
        paired_core_node: c.paired_core_node || '',
        second_paired_core_node: c.second_paired_core_node || '',
        auto_confirm: !!c.auto_confirm
    };
}

async function saveCompareCell(el) {
    var styleID = parseInt(el.dataset.style, 10);
    var node = el.dataset.node;
    var claim = (_compareClaims[styleID] || {})[node];
    if (!claim) { toast('No claim for that cell', 'error'); return; }
    var body = claimToBody(claim);
    if (el.dataset.kind === 'payload') {
        var code = el.value;
        if (body.swap_mode === 'manual_swap') {
            body.payload_code = '';
            body.allowed_payload_codes = code ? [code] : [];
        } else {
            body.payload_code = code;
            body.allowed_payload_codes = code ? [code] : [];
            body.uop_capacity = capacityForPayload(code);
        }
    } else {
        body[_compareField] = parseInt(el.value, 10) || 0;
    }
    try {
        await api.post('/api/style-node-claims', body);
        // Keep the local cache in step so a follow-up edit on the same cell
        // builds on the saved value rather than the stale fetch.
        claim.payload_code = body.payload_code;
        claim.allowed_payload_codes = body.allowed_payload_codes;
        claim.uop_capacity = body.uop_capacity;
        claim.reorder_point = body.reorder_point;
        claim.lineside_soft_threshold = body.lineside_soft_threshold;
        flashSaved(el);
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

function flashSaved(el) {
    var prev = el.style.backgroundColor;
    el.style.backgroundColor = 'var(--ok-bg, #d6f5d6)';
    setTimeout(function() { el.style.backgroundColor = prev; }, 500);
}

function jumpToStyleEditor(styleID) {
    if (!styleID) return;
    var sel = document.getElementById('claims-style-selector');
    if (sel) sel.value = String(styleID);
    onClaimsStyleChanged();             // sets _claimsStyleID + loads that style's claims
    var oneRadio = document.querySelector('input[name="claims-view"][value="one"]');
    if (oneRadio) oneRadio.checked = true;
    _compareMode = false;
    applyCompareMode();
}

// ─── Claim editor — state-driven ───────────────────────────────────────
//
// CLAIM_FIELD_VISIBILITY: the (role, swap_mode) lookup table that
// replaces the prior 31-toggle imperative editor. Given the current
// role and swap mode, returns a map of fieldset/group element ID →
// boolean for whether that field should be visible.
//
// The map is the source of truth for what shows when. Editing the
// editor (e.g., wiring a new field to a swap mode) is a one-line
// table change here, not a hunt through showModal/openClaimModal/
// editClaim/toggleClaimsAddPayload/validateClaimStaging looking for
// every place to add a `style.display = ''`.

function claimFieldVisibility(role, swap) {
    const isManual = swap === 'manual_swap';
    const isPressIndex = swap === 'two_robot_press_index';
    const usesStaging = swap === 'single_robot' || swap === 'two_robot';
    // role is now constrained to consume|produce; the legacy "changeover"
    // role was removed during the UI consistency refactor and is no
    // longer present in either the protocol or the editor's dropdown.
    const showPair = !isManual;
    return {
        'claims-add-payload-group':           !isManual,
        'claims-add-allowed-group':           false,
        'claims-add-capacity-group':          !isManual,
        'claims-add-reorder-group':           !isManual,
        'claims-add-lineside-group':          role === 'consume' && !isManual,
        // Staging fieldset is hidden by manual_swap (no staging concept),
        // then further hidden when the swap mode doesn't use staging at
        // all (sequential / press_index).
        'claims-staging-fieldset':            !isManual && usesStaging,
        'claims-add-swap-group':              true,
        'claims-source-fieldset':             !isManual,
        'claims-inbound-source-group':        !isManual,
        // Outbound destination is shown in every swap mode, including
        // two_robot (the old bin still goes somewhere).
        'claims-outbound-destination-group':  !isManual,
        'claims-changeover-fieldset':         !isManual,
        'claims-ab-fieldset':                 showPair,
        'claims-add-second-paired-group':     showPair && isPressIndex,
        'claims-add-reuse-bins-row':          showPair && isPressIndex,
        'claims-auto-request-fieldset':       false,
        'claims-auto-request-manual-swap':    false,
        'claims-auto-request-standard':       !isManual,
        // Auto-push is only meaningful for a consume manual_swap
        // (unloader pulling parts from a bin).
        'claims-add-auto-push-row':           isManual && role === 'consume',
    };
}

// SWAP_MODE_LABELS: presentation map for the existing claims table.
const SWAP_MODE_LABELS = {
    simple: 'Simple',
    sequential: 'Sequential',
    single_robot: '1-Robot',
    two_robot: '2-Robot',
    two_robot_press_index: '2-Robot Press Index',
    manual_swap: 'Manual Swap',
};

const ROLE_LABELS = {
    consume: 'Consume',
    produce: 'Produce',
};

// claim editor state — populated by openClaimModal / editClaim,
// snapshotted by readClaimStateFromForm before save.
var _payloadCatalog = [];
var _claimsStyleID = 0;
var _currentClaims = [];

async function initClaimsTab() {
    await loadPayloadCatalog();
    var sel = document.getElementById('claims-style-selector');
    if (sel && sel.value) {
        _claimsStyleID = parseInt(sel.value, 10);
        await loadClaims(_claimsStyleID);
    }
}

function onClaimsStyleChanged() {
    var sel = document.getElementById('claims-style-selector');
    _claimsStyleID = parseInt(sel.value, 10) || 0;
    loadClaims(_claimsStyleID);
}

async function loadPayloadCatalog() {
    if (_payloadCatalog.length > 0) return;
    try {
        _payloadCatalog = await api.get('/api/payload-catalog');
        if (!Array.isArray(_payloadCatalog)) _payloadCatalog = [];
    } catch (_) { _payloadCatalog = []; }
    var sel = document.getElementById('claims-add-payload');
    if (!sel) return;
    sel.innerHTML = '<option value="">-- Select --</option><option value="__empty__">Empty (clear node)</option>';
    _payloadCatalog.forEach(function(p) {
        var opt = document.createElement('option');
        opt.value = p.code;
        opt.textContent = p.code + (p.name ? ' — ' + p.name : '') + (p.uop_capacity ? ' (' + p.uop_capacity + ' UOP)' : '');
        opt.dataset.capacity = p.uop_capacity || 0;
        sel.appendChild(opt);
    });
}

async function loadClaims(styleID) {
    var list = document.getElementById('claims-list');
    list.innerHTML = '';
    if (!styleID) return;
    try {
        var claims = await api.get('/api/styles/' + styleID + '/node-claims');
        _currentClaims = Array.isArray(claims) ? claims : [];
        if (!Array.isArray(claims) || claims.length === 0) {
            list.innerHTML = '<div style="color:var(--text-muted);font-style:italic;padding:0.5rem 0">No node claims for this style. Use the form below to add claims.</div>';
            return;
        }
        var table = document.createElement('table');
        table.className = 'table';
        table.innerHTML = '<thead><tr><th>Core Node</th><th>Role</th><th>Swap</th><th>Wants</th><th>Inbound</th><th>Outbound</th><th>Source</th><th>Dest</th><th>A/B Pair</th><th style="width:1%"></th></tr></thead>';
        var tbody = document.createElement('tbody');
        claims.forEach(function(c) {
            tbody.appendChild(renderClaimRow(c));
        });
        table.appendChild(tbody);
        list.appendChild(table);
        ensureClaimsListDelegation(list);
    } catch (e) {
        toast('Error loading claims: ' + e, 'error');
    }
}

// renderClaimRow builds the <tr> for a single existing claim. Pure
// (claim) → DOM, no global state read; easy to unit-test if/when a
// browserless harness lands for the row rendering.
function renderClaimRow(c) {
    var tr = document.createElement('tr');
    tr.id = 'claim-row-' + c.id;
    var wants;
    if (c.payload_code === '__empty__') {
        wants = 'Empty (clear node)';
    } else if (c.payload_code) {
        wants = c.payload_code + (c.role === 'produce' ? ' (empty bin)' : '');
    } else {
        wants = 'Unset';
    }
    var swapLabel = SWAP_MODE_LABELS[c.swap_mode] || c.swap_mode || '';
    var flags = [];
    if (c.keep_staged) flags.push('staged');
    if (c.evacuate_on_changeover) flags.push('evac');
    if (c.auto_reorder) flags.push('auto');
    var flagStr = flags.length ? ' <span style="color:var(--text-muted);font-size:0.75rem">' + flags.join(', ') + '</span>' : '';
    var esc = escapeHtml;
    tr.innerHTML =
        '<td class="mono">' + esc(c.core_node_name) + '</td>' +
        '<td><span class="badge">' + esc(ROLE_LABELS[c.role] || c.role) + '</span>' + flagStr + '</td>' +
        '<td>' + esc(swapLabel) + '</td>' +
        '<td>' + esc(wants) + (c.uop_capacity ? ' <span style="color:var(--text-muted);font-size:0.8rem">(' + c.uop_capacity + ' UOP)</span>' : '') + '</td>' +
        '<td class="mono">' + esc(c.inbound_staging || '—') + '</td>' +
        '<td class="mono">' + esc(c.outbound_staging || '—') + '</td>' +
        '<td class="mono" style="font-size:0.8rem">' + esc(c.inbound_source || '—') + '</td>' +
        '<td class="mono" style="font-size:0.8rem">' + esc(c.outbound_destination || '—') + '</td>' +
        '<td class="mono" style="font-size:0.8rem">' + esc(c.paired_core_node || '—') + '</td>' +
        '<td style="white-space:nowrap">' +
            '<button class="btn btn-sm" data-action="edit-claim" data-claim-id="' + c.id + '">Edit</button> ' +
            '<button class="btn btn-sm btn-danger" data-action="remove-claim" data-claim-id="' + c.id + '">Remove</button>' +
        '</td>';
    return tr;
}

// Single delegated click listener on the claims-list container. The list
// is wiped/refilled by loadClaims, but the container persists, so we
// attach once (idempotent via a sentinel dataset flag).
function ensureClaimsListDelegation(list) {
    if (!list || list.dataset.delegated === '1') return;
    list.dataset.delegated = '1';
    list.addEventListener('click', function(e) {
        var btn = e.target.closest && e.target.closest('[data-action]');
        if (!btn || !list.contains(btn)) return;
        var id = parseInt(btn.dataset.claimId, 10);
        if (btn.dataset.action === 'edit-claim') {
            var claim = _currentClaims.find(function(c) { return c.id === id; });
            if (claim) editClaim(claim);
        } else if (btn.dataset.action === 'remove-claim') {
            removeClaim(id);
        }
    });
}

// ── Claim form: read/write/render/validate ──────────────────────────────

// readClaimStateFromForm: snapshot the current DOM inputs into a state
// object. Pure DOM → JS; no side effects. Used by saveClaim and by the
// onchange handlers wired in processes.html that re-render the form
// whenever role or swap mode flips.
function readClaimStateFromForm() {
    var get = function(id) { return document.getElementById(id); };
    var allowedCodes = [];
    document.querySelectorAll('.allowed-payload-cb:checked').forEach(function(cb) {
        allowedCodes.push(cb.value);
    });
    return {
        id: get('claims-edit-id').value,
        styleId: _claimsStyleID,
        coreNodeName: get('claims-add-node').value,
        role: get('claims-add-role').value,
        swapMode: get('claims-add-swap').value,
        payloadCode: get('claims-add-payload').value,
        allowedPayloadCodes: allowedCodes,
        uopCapacity: parseInt(get('claims-add-capacity').value, 10) || 0,
        reorderPoint: parseInt(get('claims-add-reorder').value, 10) || 0,
        linesideSoftThreshold: Math.max(0, parseInt(get('claims-add-lineside-soft').value, 10) || 0),
        inboundStaging: get('claims-add-inbound').value,
        outboundStaging: get('claims-add-outbound').value,
        inboundSource: get('claims-add-inbound-source').value,
        outboundDestination: get('claims-add-outbound-destination').value,
        autoRequestPayload: get('claims-add-auto-request').value,
        evacuateOnChangeover: get('claims-add-evacuate').checked,
        reuseCompatibleBins: get('claims-add-reuse-bins').checked,
        autoPush: get('claims-add-auto-push').checked,
        pairedCoreNode: get('claims-add-paired-node').value,
        secondPairedCoreNode: get('claims-add-second-paired-node').value,
        autoConfirm: get('claims-add-auto-confirm').checked,
    };
}

// writeClaimStateToForm: opposite direction — push a state object out
// to the form inputs. Used by editClaim (existing claim → form) and
// openClaimModal (default state → form).
function writeClaimStateToForm(state) {
    var get = function(id) { return document.getElementById(id); };
    get('claims-edit-id').value = state.id || '';
    get('claims-add-node').value = state.coreNodeName || '';
    get('claims-add-role').value = state.role || 'consume';
    get('claims-add-swap').value = state.swapMode || 'single_robot';
    get('claims-add-payload').value = state.payloadCode || '';
    get('claims-add-capacity').value = String(state.uopCapacity || 0);
    get('claims-add-reorder').value = String(state.reorderPoint || 0);
    get('claims-add-lineside-soft').value = String(state.linesideSoftThreshold || 0);
    get('claims-add-inbound').value = state.inboundStaging || '';
    get('claims-add-outbound').value = state.outboundStaging || '';
    get('claims-add-inbound-source').value = state.inboundSource || '';
    get('claims-add-outbound-destination').value = state.outboundDestination || '';
    get('claims-add-auto-request').value = state.autoRequestPayload || '';
    get('claims-add-evacuate').checked = !!state.evacuateOnChangeover;
    get('claims-add-reuse-bins').checked = !!state.reuseCompatibleBins;
    get('claims-add-auto-push').checked = !!state.autoPush;
    get('claims-add-paired-node').value = state.pairedCoreNode || '';
    get('claims-add-second-paired-node').value = state.secondPairedCoreNode || '';
    get('claims-add-auto-confirm').checked = !!state.autoConfirm;
}

// validateClaimState: pure (state) → {ok, errors}. Side-effect free so
// it can be unit-tested without a DOM. saveClaim translates errors to
// toasts; validate doesn't know about UI.
function validateClaimState(state) {
    var errors = [];
    if (!state.coreNodeName) {
        errors.push({ field: 'coreNodeName', msg: 'Select a core node' });
    }
    // manual_swap loaders carry no edge-side allowed list: Core owns the loader's
    // payload set (set on the loader board), so the per-style edge picker was
    // retired and there is nothing to require here. Other roles still need a
    // primary payload.
    if (state.swapMode !== 'manual_swap' && (state.role === 'consume' || state.role === 'produce') && !state.payloadCode) {
        errors.push({ field: 'payloadCode', msg: 'Select a payload' });
    }
    // single_robot needs both inbound+outbound staging, two_robot just inbound.
    if (state.swapMode === 'single_robot' && (!state.inboundStaging || !state.outboundStaging)) {
        errors.push({ field: 'staging', msg: 'Swap modes require both inbound and outbound staging' });
    } else if (state.swapMode === 'two_robot' && !state.inboundStaging) {
        errors.push({ field: 'staging', msg: 'Two-robot swap requires inbound staging' });
    }
    if (state.swapMode === 'two_robot_press_index') {
        if (!state.pairedCoreNode) {
            errors.push({ field: 'pairedCoreNode', msg: '2-Robot Press Index requires a Back Press Node' });
        }
        if (!state.outboundDestination) {
            errors.push({ field: 'outboundDestination', msg: '2-Robot Press Index requires an Outbound Destination' });
        }
        if (state.secondPairedCoreNode) {
            if (state.secondPairedCoreNode === state.pairedCoreNode) {
                errors.push({ field: 'secondPairedCoreNode', msg: 'Third press position must differ from the Back Press Node' });
            }
            if (state.secondPairedCoreNode === state.coreNodeName) {
                errors.push({ field: 'secondPairedCoreNode', msg: 'Third press position must differ from the front (Core Node)' });
            }
        }
    }
    return { ok: errors.length === 0, errors: errors };
}

// renderClaimForm: drives the editor DOM from current role/swap mode.
// Replaces the prior toggleClaimsAddPayload + validateClaimStaging
// pair. The lookup at claimFieldVisibility is the single source of
// truth for what shows when.
function renderClaimForm() {
    var role = document.getElementById('claims-add-role').value;
    var swap = document.getElementById('claims-add-swap').value;
    var isManual = swap === 'manual_swap';
    var isPressIndex = swap === 'two_robot_press_index';
    var isTwoRobot = swap === 'two_robot';
    var visibility = claimFieldVisibility(role, swap);

    // Apply visibility map. Both `hidden` and inline `display` are toggled:
    // several template elements use the HTML `hidden` attribute as their
    // initial state, and clearing inline `display` alone leaves the UA
    // `[hidden]{display:none}` rule in force.
    for (var id in visibility) {
        var el = document.getElementById(id);
        if (el) {
            el.hidden = !visibility[id];
            el.style.display = visibility[id] ? '' : 'none';
        }
    }
    // The reuse-bins row uses display:flex when visible (not block).
    var reuseRow = document.getElementById('claims-add-reuse-bins-row');
    if (reuseRow && visibility['claims-add-reuse-bins-row']) {
        reuseRow.style.display = 'flex';
    }
    // auto-push uses flex too.
    var autoPushRow = document.getElementById('claims-add-auto-push-row');
    if (autoPushRow && visibility['claims-add-auto-push-row']) {
        autoPushRow.style.display = 'flex';
    }

    // Disable outbound staging for two_robot (data: ignored anyway).
    var outboundSel = document.getElementById('claims-add-outbound');
    if (outboundSel) {
        if (isTwoRobot) {
            outboundSel.value = '';
            outboundSel.disabled = true;
            outboundSel.style.opacity = '0.5';
        } else {
            outboundSel.disabled = false;
            outboundSel.style.opacity = '';
        }
    }

    // Press-index dual-purpose A/B fieldset labels.
    if (visibility['claims-ab-fieldset']) {
        var legend = document.getElementById('claims-ab-legend');
        var help = document.getElementById('claims-ab-help');
        var label = document.getElementById('claims-ab-label');
        var pairSel = document.getElementById('claims-add-paired-node');
        if (isPressIndex) {
            legend.textContent = 'Press Index Pairing';
            help.textContent = 'Second press position. Bins index forward from this node into the active node when the active node releases. Required for 2-Robot Press Index Swap.';
            label.innerHTML = 'Back Press Node <span style="color:var(--danger,#c33)">*</span>';
            if (pairSel.options.length > 0 && pairSel.options[0].value === '') {
                pairSel.options[0].textContent = '-- Select back press node --';
            }
        } else {
            legend.textContent = 'A/B Node Cycling';
            help.textContent = 'Pair this node with another node for alternating operation. The operator flips which node is active via the station HMI.';
            label.textContent = 'Paired Node';
            if (pairSel.options.length > 0 && pairSel.options[0].value === '') {
                pairSel.options[0].textContent = '-- None (no A/B cycling) --';
            }
            // Reset state that doesn't apply outside press index.
            document.getElementById('claims-add-second-paired-node').value = '';
            document.getElementById('claims-add-reuse-bins').checked = false;
        }
    } else {
        // AB fieldset hidden entirely → clear paired-node state.
        document.getElementById('claims-add-paired-node').value = '';
        document.getElementById('claims-add-second-paired-node').value = '';
        document.getElementById('claims-add-reuse-bins').checked = false;
    }

    // Auto-push only applies to a consume manual_swap (unloader).
    if (!(isManual && role === 'consume')) {
        document.getElementById('claims-add-auto-push').checked = false;
    }

    // Manual swap on a fresh open clears staging fields (no concept of
    // staging there). When editing, leave alone so the operator can
    // see prior values before manual_swap was selected.
    var isEditing = !!document.getElementById('claims-edit-id').value;
    if (isManual && !isEditing) {
        document.getElementById('claims-add-reorder').value = '0';
        document.getElementById('claims-add-payload').value = '';
        document.getElementById('claims-add-inbound').value = '';
        document.getElementById('claims-add-outbound').value = '';
        document.getElementById('claims-add-inbound-source').value = '';
        document.getElementById('claims-add-evacuate').checked = false;
        document.getElementById('claims-add-paired-node').value = '';
        buildAllowedPayloadPicker([]);
    }
    if (isManual && isEditing) {
        var picker = document.getElementById('claims-allowed-picker');
        var hasCheckboxes = picker && picker.querySelector('.allowed-payload-cb');
        if (!hasCheckboxes) {
            var legacyPayload = document.getElementById('claims-add-payload').value;
            var seed = legacyPayload ? [legacyPayload] : [];
            buildAllowedPayloadPicker(seed);
            updateAutoRequestDropdown();
        }
    }

    // Clear staging values when not used so they aren't saved.
    if (!(swap === 'single_robot' || swap === 'two_robot') && !isManual) {
        document.getElementById('claims-add-inbound').value = '';
        document.getElementById('claims-add-outbound').value = '';
    }

    // Validation warning for missing required staging.
    var warn = document.getElementById('claims-staging-warning');
    if (warn) {
        var state = readClaimStateFromForm();
        var missing = (swap === 'single_robot' && (!state.inboundStaging || !state.outboundStaging))
            || (swap === 'two_robot' && !state.inboundStaging);
        warn.style.display = missing ? '' : 'none';
    }
}

// Backwards-compat shims for inline onchange handlers in processes.html.
// (`onchange="toggleClaimsAddPayload(); validateClaimStaging()"`)
function toggleClaimsAddPayload() { renderClaimForm(); }
function validateClaimStaging()   { renderClaimForm(); return true; }

function defaultClaimState() {
    return {
        id: '',
        coreNodeName: '',
        role: 'consume',
        swapMode: 'single_robot',
        payloadCode: '',
        allowedPayloadCodes: [],
        uopCapacity: 0,
        reorderPoint: 0,
        linesideSoftThreshold: 0,
        inboundStaging: '',
        outboundStaging: '',
        inboundSource: '',
        outboundDestination: '',
        autoRequestPayload: '',
        evacuateOnChangeover: false,
        reuseCompatibleBins: false,
        autoPush: false,
        pairedCoreNode: '',
        secondPairedCoreNode: '',
        autoConfirm: false,
    };
}

function openClaimModal() {
    if (!_claimsStyleID) { toast('Select a style first', 'warning'); return; }
    // Mark already-claimed nodes as disabled with strikethrough.
    var sel = document.getElementById('claims-add-node');
    var claimedNodes = _currentClaims.map(function(c) { return c.core_node_name; });
    Array.from(sel.options).forEach(function(opt) {
        if (!opt.value) return;
        var claimed = claimedNodes.indexOf(opt.value) >= 0;
        opt.disabled = claimed;
        opt.style.textDecoration = claimed ? 'line-through' : '';
        opt.style.color = claimed ? 'var(--text-muted)' : '';
    });
    sel.disabled = false;
    writeClaimStateToForm(defaultClaimState());
    document.getElementById('claim-modal-title').textContent = 'Add Node Claim';
    renderClaimForm();
    showModal('claim-modal');
}

function editClaim(claim) {
    if (!_claimsStyleID) return;
    var sel = document.getElementById('claims-add-node');
    Array.from(sel.options).forEach(function(opt) {
        opt.disabled = false;
        opt.style.textDecoration = '';
        opt.style.color = '';
    });
    sel.disabled = false;
    writeClaimStateToForm({
        id: claim.id,
        coreNodeName: claim.core_node_name,
        role: claim.role || 'consume',
        swapMode: claim.swap_mode || 'simple',
        payloadCode: claim.payload_code || '',
        uopCapacity: claim.uop_capacity || 0,
        reorderPoint: claim.reorder_point || 0,
        linesideSoftThreshold: claim.lineside_soft_threshold || 0,
        inboundStaging: claim.inbound_staging || '',
        outboundStaging: claim.outbound_staging || '',
        inboundSource: claim.inbound_source || '',
        outboundDestination: claim.outbound_destination || '',
        autoRequestPayload: claim.auto_request_payload || '',
        evacuateOnChangeover: !!claim.evacuate_on_changeover,
        reuseCompatibleBins: !!claim.reuse_compatible_bins,
        autoPush: !!claim.auto_push,
        pairedCoreNode: claim.paired_core_node || '',
        secondPairedCoreNode: claim.second_paired_core_node || '',
        autoConfirm: !!claim.auto_confirm,
    });
    document.getElementById('claim-modal-title').textContent = 'Edit Node Claim';
    if (claim.swap_mode === 'manual_swap') {
        // Legacy claims migrated from bin_loader have payload_code set but
        // allowed_payload_codes empty. Seed the picker from payload_code
        // so Save doesn't immediately reject with "Select at least one
        // allowed payload".
        var allowed = claim.allowed_payload_codes || [];
        if (allowed.length === 0 && claim.payload_code) {
            allowed = [claim.payload_code];
        }
        buildAllowedPayloadPicker(allowed);
        updateAutoRequestDropdown();
        document.getElementById('claims-add-auto-request').value = claim.auto_request_payload || '';
    }
    renderClaimForm();
    showModal('claim-modal');
}

function closeClaimModal() {
    hideModal('claim-modal');
    document.getElementById('claims-add-node').disabled = false;
}

async function saveClaim() {
    var state = readClaimStateFromForm();
    var validation = validateClaimState(state);
    if (!validation.ok) {
        // Surface the first error; field-level error rendering is a
        // follow-up. Today's UX matches the prior single-toast behavior.
        toast(validation.errors[0].msg, 'warning');
        return;
    }

    // manual_swap claims carry no edge-side payload: Core owns the loader's
    // payload set (loader board), so payload_code is blank and the operator
    // switches among the aggregate's payloads at load time.
    var primaryPayload = state.swapMode === 'manual_swap' ? '' : state.payloadCode;

    var claimBody = {
        style_id: state.styleId,
        core_node_name: state.coreNodeName,
        role: state.role,
        swap_mode: state.swapMode,
        payload_code: primaryPayload,
        allowed_payload_codes: state.allowedPayloadCodes,
        uop_capacity: state.uopCapacity,
        reorder_point: state.reorderPoint,
        lineside_soft_threshold: state.linesideSoftThreshold,
        auto_reorder: true,
        inbound_staging: state.inboundStaging,
        outbound_staging: state.outboundStaging,
        inbound_source: state.inboundSource,
        outbound_destination: state.outboundDestination,
        auto_request_payload: state.autoRequestPayload,
        // KeepStaged column persists as a backend safety net for the
        // future supermarket rewire; the editor never sets it true.
        keep_staged: false,
        evacuate_on_changeover: state.evacuateOnChangeover,
        reuse_compatible_bins: state.reuseCompatibleBins,
        auto_push: state.autoPush,
        paired_core_node: state.pairedCoreNode,
        second_paired_core_node: state.secondPairedCoreNode,
        auto_confirm: state.autoConfirm,
    };

    // Loader replenishment + dedicated-position layout are configured on the Core
    // loader setup screen (Nodes -> Create/Edit Loader), not via this claim, so the
    // operator_driven / home_location flags are no longer sent (the *bool
    // "absent = leave untouched" contract leaves any legacy edge-table value alone).

    // NGRP expansion: if the picked node is a group AND we're creating
    // (not editing), fan out to the physical children with one POST
    // each. Confirmation required so a stray group-pick doesn't silently
    // create N claims.
    var sel = document.getElementById('claims-add-node');
    var selectedOpt = sel.options[sel.selectedIndex];
    var nodeType = selectedOpt ? selectedOpt.dataset.type : '';
    var nodeNames = [state.coreNodeName];
    if (nodeType === 'NGRP' && !state.id) {
        try {
            var children = await api.get('/api/node/' + encodeURIComponent(state.coreNodeName) + '/children');
            if (Array.isArray(children) && children.length > 0) {
                var childNames = children.map(function(c) { return c.name; });
                if (!await confirm('Create ' + state.role + ' claims for ' + childNames.length + ' nodes under ' + state.coreNodeName + '?\n\n' + childNames.join(', '))) {
                    return;
                }
                nodeNames = childNames;
            } else {
                toast('No physical children found under ' + state.coreNodeName, 'warning');
                return;
            }
        } catch (e) {
            toast('Error fetching children: ' + e, 'error');
            return;
        }
    }

    try {
        for (var i = 0; i < nodeNames.length; i++) {
            claimBody.core_node_name = nodeNames[i];
            await api.post('/api/style-node-claims', claimBody);
        }
        closeClaimModal();
        await loadClaims(_claimsStyleID);
        if (nodeNames.length > 1) toast('Created ' + nodeNames.length + ' claims', 'success');
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

async function removeClaim(id) {
    try {
        await api.del('/api/style-node-claims/' + id);
        await loadClaims(_claimsStyleID);
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

async function syncPayloadCatalog() {
    try {
        await api.post('/api/payload-catalog/sync');
        _payloadCatalog = [];
        await loadPayloadCatalog();
        toast('Payload catalog synced', 'success');
    } catch (e) {
        toast('Sync failed: ' + e, 'error');
    }
}

function buildAllowedPayloadPicker(selected) {
    var picker = document.getElementById('claims-allowed-picker');
    picker.innerHTML = '';
    var checkedSet = new Set(selected || []);
    _payloadCatalog.forEach(function(p) {
        var label = document.createElement('label');
        label.style.cssText = 'display:flex;align-items:center;gap:0.5rem;cursor:pointer';
        var cb = document.createElement('input');
        cb.type = 'checkbox';
        cb.className = 'allowed-payload-cb';
        cb.value = p.code;
        cb.checked = checkedSet.has(p.code);
        cb.addEventListener('change', updateAutoRequestDropdown);
        label.appendChild(cb);
        var span = document.createElement('span');
        span.textContent = p.code + (p.name ? ' — ' + p.name : '') + (p.uop_capacity ? ' (' + p.uop_capacity + ' UOP)' : '');
        label.appendChild(span);
        picker.appendChild(label);
    });
    if (_payloadCatalog.length === 0) {
        picker.innerHTML = '<div style="color:var(--text-muted);font-style:italic">No payloads in catalog. Sync from Core first.</div>';
    }
}

function getSelectedAllowedPayloads() {
    var codes = [];
    document.querySelectorAll('.allowed-payload-cb:checked').forEach(function(cb) {
        codes.push(cb.value);
    });
    return codes;
}

function updateAutoRequestDropdown() {
    var sel = document.getElementById('claims-add-auto-request');
    var current = sel.value;
    sel.innerHTML = '<option value="">-- Disabled --</option>';
    var selected = getSelectedAllowedPayloads();
    selected.forEach(function(code) {
        var opt = document.createElement('option');
        opt.value = code;
        opt.textContent = code;
        if (code === current) opt.selected = true;
        sel.appendChild(opt);
    });
}

function autoFillClaimsCapacity() {
    var sel = document.getElementById('claims-add-payload');
    var opt = sel.options[sel.selectedIndex];
    if (opt && opt.dataset.capacity) {
        document.getElementById('claims-add-capacity').value = opt.dataset.capacity;
    }
}

// ─── Operator Stations (Screens) ───────────────────────────────────────

function resetStationForm() {
    document.getElementById('station-id').value = '';
    document.getElementById('station-name').value = '';
    document.getElementById('station-note').value = '';
    document.getElementById('station-enabled').checked = true;
    resetNodePicker([]);
}

function resetNodePicker(checkedNodes) {
    var checked = new Set(checkedNodes || []);
    var editingID = document.getElementById('station-id').value;
    document.querySelectorAll('.station-node-cb').forEach(function(cb) {
        var name = cb.value;
        cb.checked = checked.has(name);
        var claim = claimedByStation[name];
        var ownerSpan = cb.closest('label').querySelector('.station-node-owner');
        if (claim && String(claim.id) !== editingID) {
            cb.disabled = true;
            cb.closest('label').style.opacity = '0.5';
            ownerSpan.textContent = '(' + claim.name + ')';
        } else {
            cb.disabled = false;
            cb.closest('label').style.opacity = '';
            ownerSpan.textContent = '';
        }
    });
}

function getPickedNodes() {
    var nodes = [];
    document.querySelectorAll('.station-node-cb:checked').forEach(function(cb) {
        nodes.push(cb.value);
    });
    return nodes;
}

function openCreateStationModal() {
    resetStationForm();
    document.getElementById('station-modal-title').textContent = 'Add Operator Station';
    showModal('station-modal');
}

function closeStationModal() {
    hideModal('station-modal');
    resetStationForm();
}

async function editStation() {
    // Invoked via data-action="editStation" with data-station="{{json .}}".
    var station = {};
    try { station = JSON.parse(this.dataset.station || '{}') || {}; }
    catch (e) { station = {}; }
    resetStationForm();
    document.getElementById('station-id').value = station.id;
    document.getElementById('station-name').value = station.name || '';
    document.getElementById('station-note').value = station.note || '';
    document.getElementById('station-enabled').checked = !!station.enabled;
    // Load claimed nodes for this station
    try {
        var nodes = await api.get('/api/operator-stations/' + station.id + '/claimed-nodes');
        resetNodePicker(Array.isArray(nodes) ? nodes : []);
    } catch (e) {
        resetNodePicker([]);
        toast('Could not load claimed nodes: ' + e, 'error');
    }
    showProcessTab('stations');
    document.getElementById('station-modal-title').textContent = 'Edit Operator Station';
    showModal('station-modal');
}

async function saveStation() {
    var id = document.getElementById('station-id').value;
    var payload = {
        process_id: activeProcessID,
        name: document.getElementById('station-name').value.trim(),
        note: document.getElementById('station-note').value.trim(),
        code: '',
        area_label: '',
        sequence: 0,
        controller_node_id: '',
        enabled: document.getElementById('station-enabled').checked,
        device_mode: 'fixed_hmi'
    };
    if (!payload.name) {
        toast('Station name is required', 'warning');
        return;
    }
    try {
        var stationID;
        if (id) {
            await api.put('/api/operator-stations/' + id, payload);
            stationID = id;
        } else {
            var res = await api.post('/api/operator-stations', payload);
            stationID = res.id;
        }
        // Save claimed nodes
        await api.put('/api/operator-stations/' + stationID + '/claimed-nodes', {
            nodes: getPickedNodes()
        });
        closeStationModal();
        location.reload();
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

async function moveStation(id, direction) {
    try {
        await api.post('/api/operator-stations/' + id + '/move', { direction: direction });
        location.reload();
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

async function deleteStation(id) {
    if (!await confirm('Delete this operator station and its node assignments?')) return;
    try {
        await api.del('/api/operator-stations/' + id);
        location.reload();
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

// Wire up tag-select pickers for PLC counter tag fields
(function initTagSelects() {
    tagSelect('counter-tag', 'counter-plc');
    tagSelect('new-process-counter-tag', 'new-process-counter-plc');
})();

// Initialize Node Claims tab (load catalog + first style's claims)
if (activeProcessID) initClaimsTab();

// ─── delegated event handlers ─────────────────────────
// All page-level data-action verbs route through delegateActions
// on document.body. Multiple event types share the same handler
// map — most handlers are click-only but a few (e.g. updatePreview)
// are referenced via data-action-change / data-action-input too,
// so binding the map across every event type keeps the page wiring
// single-source.
delegateActions(document.body, {
    addGenerateRow,
    autoFillClaimsCapacity,
    buildAllowedPayloadPicker,
    claimFieldVisibility,
    cloneStyle,
    closeClaimModal,
    closeCloneStyleModal,
    closeGenerateModal,
    closeProcessModal,
    closeStationModal,
    closeStyleModal,
    createProcess,
    defaultClaimState,
    deleteProcess,
    deleteStation,
    deleteStyle,
    editClaim,
    editStation,
    editStyle,
    ensureClaimsListDelegation,
    generateStyles,
    getPickedNodes,
    getSelectedAllowedPayloads,
    initClaimsTab,
    loadClaims,
    loadPayloadCatalog,
    moveStation,
    onClaimsStyleChanged,
    onCompareFieldChanged,
    onCompareViewChanged,
    onGenerateBaseChanged,
    openClaimModal,
    openCloneStyleModal,
    openCreateProcessModal,
    openCreateStationModal,
    openCreateStyleModal,
    openGenerateModal,
    readClaimStateFromForm,
    removeClaim,
    removeGenerateRow,
    renderClaimForm,
    renderClaimRow,
    resetNodePicker,
    resetProcessForm,
    resetStationForm,
    resetStyleForm,
    saveClaim,
    saveProcess,
    saveStation,
    saveStyle,
    setActiveStyle,
    showProcessTab,
    syncPayloadCatalog,
    toggleClaimsAddPayload,
    updateAutoRequestDropdown,
    validateClaimStaging,
    validateClaimState,
    writeClaimStateToForm
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
