// --- Shifts ---
(function loadShifts() {
    var shifts = JSON.parse(document.getElementById('page-data').dataset.shifts);
    for (var i = 0; i < shifts.length; i++) {
        var s = shifts[i];
        var nameEl = document.querySelector('.shift-name[data-shift="' + s.shift_number + '"]');
        var startEl = document.querySelector('.shift-start[data-shift="' + s.shift_number + '"]');
        var endEl = document.querySelector('.shift-end[data-shift="' + s.shift_number + '"]');
        if (nameEl) nameEl.value = s.name;
        if (startEl) startEl.value = s.start_time;
        if (endEl) endEl.value = s.end_time;
    }
})();

async function saveShifts() {
    var shifts = [];
    for (var n = 1; n <= 3; n++) {
        var name = document.querySelector('.shift-name[data-shift="' + n + '"]').value.trim();
        var start = document.querySelector('.shift-start[data-shift="' + n + '"]').value;
        var end = document.querySelector('.shift-end[data-shift="' + n + '"]').value;
        shifts.push({ shift_number: n, name: name, start_time: start, end_time: end });
    }
    try {
        await ShingoEdge.api.put('/api/shifts', shifts);
        ShingoEdge.toast('Shifts saved', 'success');
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// --- Section collapse ---
function toggleSection(id) {
    var el = document.getElementById(id);
    el.classList.toggle('collapsed');
    saveSectionState();
}

function saveSectionState() {
    var sections = document.querySelectorAll('.setup-section[id]');
    var state = {};
    for (var i = 0; i < sections.length; i++) {
        state[sections[i].id] = sections[i].classList.contains('collapsed');
    }
    localStorage.setItem('setup-sections', JSON.stringify(state));
}

(function restoreSectionState() {
    var saved = localStorage.getItem('setup-sections');
    if (!saved) return;
    try {
        var state = JSON.parse(saved);
        for (var id in state) {
            if (state[id]) {
                var el = document.getElementById(id);
                if (el) el.classList.add('collapsed');
            }
        }
    } catch (e) {}
})();

// --- Station Configuration (unified save) ---
async function saveStationConfig() {
    try {
        await Promise.all([
            ShingoEdge.api.put('/api/config/station-id', {
                station_id: document.getElementById('station-id-input').value.trim()
            }),
            ShingoEdge.api.put('/api/config/warlink', (function() {
                var d = ShingoEdge.getFormData('warlink-form');
                d.port = parseInt(d.port) || 8080;
                return d;
            })()),
            ShingoEdge.api.put('/api/config/messaging', { kafka_brokers: collectBrokers() }),
            ShingoEdge.api.put('/api/config/auto-confirm', {
                auto_confirm: document.getElementById('auto-confirm').checked
            })
        ]);
        ShingoEdge.toast('Configuration saved', 'success');
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// --- WarLink mode toggle ---
function onWarlinkModeChange(mode) {
    var pollInput = document.querySelector('#warlink-form [name="poll_rate"]');
    if (pollInput) {
        pollInput.disabled = (mode !== 'poll');
        pollInput.style.opacity = (mode !== 'poll') ? '0.5' : '';
    }
}
// Apply initial state
(function() {
    var modeSelect = document.querySelector('#warlink-form [name="mode"]');
    if (modeSelect) onWarlinkModeChange(modeSelect.value);
})();

async function refreshPLCChips() {
    try {
        var plcs = await ShingoEdge.api.get('/api/plcs');
        var wrapper = document.getElementById('plc-chips-wrapper');
        if (!wrapper) return;
        if (!plcs || plcs.length === 0) {
            wrapper.innerHTML = '';
            return;
        }
        var html = '<label style="display:block;margin-bottom:0.25rem;font-weight:500;color:var(--text-muted)">Available PLCs</label>' +
            '<div id="plc-chips" style="display:flex;gap:0.5rem;flex-wrap:wrap">';
        for (var i = 0; i < plcs.length; i++) {
            var p = plcs[i];
            html += '<span class="plc-chip ' + (p.connected ? 'plc-chip-connected' : 'plc-chip-disconnected') + '" id="plc-status-' + p.name + '"><span class="plc-health-dot ' + (p.connected ? 'plc-health-online' : 'plc-health-unknown') + '" id="plc-health-' + p.name + '"></span>' + p.name + '</span>';
        }
        html += '</div>';
        wrapper.innerHTML = html;
    } catch (e) { /* ignore */ }
}

// --- Tag Picker ---
var _tagCache = {};

function loadTagsForPLC(plcName, cb) {
    if (_tagCache[plcName]) { cb(_tagCache[plcName]); return; }
    ShingoEdge.api.get('/api/plcs/all-tags/' + encodeURIComponent(plcName)).then(function(tags) {
        _tagCache[plcName] = tags || [];
        cb(_tagCache[plcName]);
    }).catch(function() { cb([]); });
}

function onPLCSelectChange(sel) {
    var form = sel.closest('.card-body');
    var tagInput = form.querySelector('[name="tag_name"]');
    if (tagInput) tagInput.value = '';
    var dropdown = form.querySelector('.tag-picker-dropdown');
    if (dropdown) dropdown.style.display = 'none';
    if (sel.value) {
        loadTagsForPLC(sel.value, function() {});
    }
}

function openTagPicker(input) {
    var form = input.closest('.card-body');
    var plcSel = form.querySelector('[name="plc_name"]');
    if (!plcSel || !plcSel.value) return;
    loadTagsForPLC(plcSel.value, function(tags) {
        renderTagDropdown(input, tags, input.value);
    });
}

function filterTagPicker(input) {
    var form = input.closest('.card-body');
    var plcSel = form.querySelector('[name="plc_name"]');
    if (!plcSel || !plcSel.value) return;
    var tags = _tagCache[plcSel.value] || [];
    renderTagDropdown(input, tags, input.value);
}

function renderTagDropdown(input, tags, filter) {
    var dropdown = input.parentElement.querySelector('.tag-picker-dropdown');
    var lc = (filter || '').toLowerCase();
    var filtered = tags.filter(function(t) { return !lc || t.name.toLowerCase().indexOf(lc) !== -1; });
    if (filtered.length === 0) {
        dropdown.innerHTML = '<div class="tag-picker-empty">No matching tags</div>';
    } else {
        var html = '';
        var limit = Math.min(filtered.length, 100);
        for (var i = 0; i < limit; i++) {
            var t = filtered[i];
            html += '<div class="tag-picker-item" onmousedown="selectTagPickerItem(this, \'' + ShingoEdge.escapeHtml(t.name).replace(/'/g, "\\'") + '\')">';
            html += '<span class="tag-picker-name">' + ShingoEdge.escapeHtml(t.name) + '</span>';
            if (t.enabled === false) {
                html += '<span class="tag-picker-unpublished">(not published)</span>';
            }
            html += '<span class="tag-picker-type">' + ShingoEdge.escapeHtml(t.type) + '</span>';
            html += '</div>';
        }
        if (filtered.length > limit) {
            html += '<div class="tag-picker-empty">' + (filtered.length - limit) + ' more...</div>';
        }
        dropdown.innerHTML = html;
    }
    dropdown.style.display = '';
}

function selectTagPickerItem(el, tagName) {
    var picker = el.closest('.tag-picker');
    var input = picker.querySelector('.tag-picker-input');
    input.value = tagName;
    picker.querySelector('.tag-picker-dropdown').style.display = 'none';
}

// Close tag picker on outside click
document.addEventListener('click', function(e) {
    if (!e.target.closest('.tag-picker')) {
        var dropdowns = document.querySelectorAll('.tag-picker-dropdown');
        for (var i = 0; i < dropdowns.length; i++) dropdowns[i].style.display = 'none';
    }
});

// --- Cat-ID Chips ---
var _catIDData = { 'js-add': [], 'js-edit': [] };

function renderCatIDChips(prefix) {
    var container = document.getElementById(prefix + '-catids');
    var ids = _catIDData[prefix] || [];
    var html = '';
    for (var i = 0; i < ids.length; i++) {
        html += '<span class="cat-id-chip">' + ShingoEdge.escapeHtml(ids[i]) +
            '<button type="button" class="cat-id-remove" onclick="removeCatIDChip(\'' + prefix + '\',' + i + ')">&times;</button></span>';
    }
    container.innerHTML = html;
}

function addCatIDChip(prefix) {
    var input = document.getElementById(prefix + '-catid-input');
    var val = input.value.trim();
    if (!val) return;
    if (!_catIDData[prefix]) _catIDData[prefix] = [];
    _catIDData[prefix].push(val);
    input.value = '';
    renderCatIDChips(prefix);
}

function removeCatIDChip(prefix, index) {
    _catIDData[prefix].splice(index, 1);
    renderCatIDChips(prefix);
}

function getCatIDs(prefix) {
    return _catIDData[prefix] || [];
}

function setCatIDs(prefix, ids) {
    _catIDData[prefix] = ids || [];
    renderCatIDChips(prefix);
}

// --- Production Lines ---
async function addLine() {
    var d = ShingoEdge.getFormData('line-add-form');
    try {
        await ShingoEdge.api.post('/api/lines', d);
        ShingoEdge.toast('Process added', 'success');
        ShingoEdge.hideModal('line-add');
        location.reload();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

function openEditLine(id, name, desc) {
    ShingoEdge.populateForm('line-edit-form', { id: id, name: name, description: desc });
    ShingoEdge.showModal('line-edit');
}

async function saveLine() {
    var d = ShingoEdge.getFormData('line-edit-form');
    var id = d.id; delete d.id;
    try {
        await ShingoEdge.api.put('/api/lines/' + id, d);
        ShingoEdge.toast('Process updated', 'success');
        ShingoEdge.hideModal('line-edit');
        location.reload();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function deleteLine(id) {
    var ok = await ShingoEdge.confirm('Delete this process and all its styles?');
    if (!ok) return;
    try {
        await ShingoEdge.api.del('/api/lines/' + id);
        ShingoEdge.toast('Deleted', 'success');
        location.reload();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function setActiveStyle(lineID, jsID) {
    try {
        await ShingoEdge.api.put('/api/lines/' + lineID + '/active-style', {
            job_style_id: jsID ? parseInt(jsID) : null
        });
        ShingoEdge.toast('Active style updated', 'success');
        loadStyleChips(lineID);
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// --- Style Chips ---
async function loadStyleChips(lineID) {
    var container = document.getElementById('style-chips-' + lineID);
    if (!container) return;
    try {
        var styles = await ShingoEdge.api.get('/api/lines/' + lineID + '/job-styles');
        var lines = await ShingoEdge.api.get('/api/lines');
        var activeStyleID = null;
        for (var i = 0; i < lines.length; i++) {
            if (lines[i].id === lineID) { activeStyleID = lines[i].active_job_style_id; break; }
        }
        if (!styles || styles.length === 0) {
            container.innerHTML = '<span style="font-size:0.8rem;color:var(--text-muted);padding:0.25rem 0">No styles</span>';
            return;
        }
        var html = '';
        for (var i = 0; i < styles.length; i++) {
            var s = styles[i];
            var cls = s.id === activeStyleID ? 'style-chip style-chip-active' : 'style-chip style-chip-inactive';
            html += '<span class="' + cls + '" onclick="openEditJS(' + s.id + ',' + lineID + ',' + (s.id === activeStyleID ? 'true' : 'false') + ')" title="' + ShingoEdge.escapeHtml(s.description || '') + '">' + ShingoEdge.escapeHtml(s.name) + '</span>';
        }
        container.innerHTML = html;
    } catch (e) {
        container.innerHTML = '<span style="font-size:0.8rem;color:var(--text-muted)">Error loading styles</span>';
    }
}

function refreshAllStyleChips() {
    var cards = document.querySelectorAll('[data-line-id]');
    for (var i = 0; i < cards.length; i++) {
        loadStyleChips(parseInt(cards[i].getAttribute('data-line-id')));
    }
}

// Load all chips on page load
(function() { refreshAllStyleChips(); })();

// --- Job Styles ---
var _currentJSLineID = 0;
var _currentJSIsActive = false;

function openAddJS(lineID) {
    _currentJSLineID = lineID;
    var form = document.getElementById('js-add-form');
    form.querySelector('[name="line_id"]').value = lineID;
    form.querySelector('[name="name"]').value = '';
    form.querySelector('[name="description"]').value = '';
    form.querySelector('[name="plc_name"]').value = '';
    form.querySelector('[name="tag_name"]').value = '';
    form.querySelector('[name="rp_enabled"]').checked = true;
    setCatIDs('js-add', []);
    ShingoEdge.showModal('js-add');
}

async function addJobStyle() {
    var d = ShingoEdge.getFormData('js-add-form');
    d.line_id = parseInt(d.line_id);
    d.cat_ids = getCatIDs('js-add');
    d.rp_plc_name = d.plc_name || '';
    d.rp_tag_name = d.tag_name || '';
    d.rp_enabled = !!d.rp_enabled;
    delete d.plc_name;
    delete d.tag_name;
    try {
        await ShingoEdge.api.post('/api/job-styles', d);
        ShingoEdge.toast('Style added', 'success');
        ShingoEdge.hideModal('js-add');
        loadStyleChips(d.line_id);
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function openEditJS(id, lineID, isActive) {
    _currentJSLineID = lineID;
    _currentJSIsActive = isActive;

    // Fetch style data
    var styles = await ShingoEdge.api.get('/api/lines/' + lineID + '/job-styles');
    var style = null;
    for (var i = 0; i < (styles || []).length; i++) {
        if (styles[i].id === id) { style = styles[i]; break; }
    }
    if (!style) { ShingoEdge.toast('Style not found', 'error'); return; }

    var form = document.getElementById('js-edit-form');
    form.querySelector('[name="id"]').value = id;
    form.querySelector('[name="line_id"]').value = lineID;
    form.querySelector('[name="name"]').value = style.name;
    form.querySelector('[name="description"]').value = style.description;
    setCatIDs('js-edit', style.cat_ids || []);

    // Fetch reporting point for this style
    var rp = null;
    try {
        rp = await ShingoEdge.api.get('/api/job-styles/' + id + '/reporting-point');
    } catch (e) {}

    form.querySelector('[name="plc_name"]').value = rp ? rp.plc_name : '';
    form.querySelector('[name="tag_name"]').value = rp ? rp.tag_name : '';
    form.querySelector('[name="rp_enabled"]').checked = rp ? rp.enabled : true;

    // Show/hide "Set as Active" button
    var setActiveBtn = document.getElementById('js-edit-set-active');
    setActiveBtn.style.display = isActive ? 'none' : '';

    ShingoEdge.showModal('js-edit');
}

async function saveJobStyle() {
    var d = ShingoEdge.getFormData('js-edit-form');
    var id = d.id; delete d.id;
    d.line_id = parseInt(d.line_id);
    d.cat_ids = getCatIDs('js-edit');
    d.rp_plc_name = d.plc_name || '';
    d.rp_tag_name = d.tag_name || '';
    d.rp_enabled = !!d.rp_enabled;
    delete d.plc_name;
    delete d.tag_name;
    try {
        await ShingoEdge.api.put('/api/job-styles/' + id, d);
        ShingoEdge.toast('Style updated', 'success');
        ShingoEdge.hideModal('js-edit');
        loadStyleChips(d.line_id);
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function setActiveStyleFromModal() {
    var form = document.getElementById('js-edit-form');
    var styleID = parseInt(form.querySelector('[name="id"]').value);
    var lineID = parseInt(form.querySelector('[name="line_id"]').value);
    await setActiveStyle(lineID, styleID);
    ShingoEdge.hideModal('js-edit');
}

async function deleteJobStyle(id, lineID) {
    var ok = await ShingoEdge.confirm('Delete this style?');
    if (!ok) return;
    try {
        await ShingoEdge.api.del('/api/job-styles/' + id);
        ShingoEdge.toast('Deleted', 'success');
        loadStyleChips(lineID);
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// --- Payloads ---
async function onPayloadLineChange() {
    var lineID = document.getElementById('payload-line').value;
    var jsSel = document.getElementById('payload-job-style');
    jsSel.innerHTML = '<option value="">-- Select Style --</option>';
    if (!lineID) { loadPayloads(); return; }
    try {
        var styles = await ShingoEdge.api.get('/api/lines/' + lineID + '/job-styles');
        for (var i = 0; i < (styles || []).length; i++) {
            var opt = document.createElement('option');
            opt.value = styles[i].id;
            opt.textContent = styles[i].name;
            jsSel.appendChild(opt);
        }
    } catch (e) {}
    loadPayloads();
}

async function loadPayloads() {
    var jsID = document.getElementById('payload-job-style').value;
    var body = document.getElementById('payload-body');
    var addBar = document.getElementById('payload-add-bar');
    if (!jsID) {
        body.innerHTML = '<tr><td colspan="9" class="empty-cell">Select a process and style to view payloads</td></tr>';
        addBar.style.display = 'none';
        return;
    }
    addBar.style.display = '';
    try {
        var payloads = await ShingoEdge.api.get('/api/payloads/job-style/' + jsID);
        if (!payloads || payloads.length === 0) {
            body.innerHTML = '<tr><td colspan="9" class="empty-cell">No payloads for this style</td></tr>';
            return;
        }
        var html = '';
        for (var i = 0; i < payloads.length; i++) {
            var p = payloads[i];
            var modeLabel = p.cycle_mode === 'two_robot' ? 'Two Robot' : p.cycle_mode === 'single_robot' ? 'Single Robot' : 'Sequential';
            html += '<tr id="payload-' + p.id + '">' +
                '<td class="mono">' + ShingoEdge.escapeHtml(p.location) + '</td>' +
                '<td class="mono">' + ShingoEdge.escapeHtml(p.payload_code) + '</td>' +
                '<td>' + ShingoEdge.escapeHtml(p.description) + '</td>' +
                '<td>' + p.role + '</td>' +
                '<td>' + p.remaining + ' / ' + p.reorder_point + '</td>' +
                '<td>' + p.reorder_point + '</td>' +
                '<td><span class="status-badge status-' + (p.cycle_mode === 'sequential' ? 'stored' : 'active') + '">' + modeLabel + '</span></td>' +
                '<td><span class="status-badge status-' + p.status + '">' + p.status + '</span></td>' +
                '<td class="actions">' +
                    '<button class="btn-icon" onclick=\'openEditPayload(' + JSON.stringify(p).replace(/'/g, "\\'") + ')\' title="Edit">&#9998;</button>' +
                    '<button class="btn-icon" onclick="resetPayload(' + p.id + ')" title="Reset to full">&#8634;</button>' +
                    '<button class="btn-icon btn-icon-danger" onclick="deletePayload(' + p.id + ')" title="Delete">&#10005;</button>' +
                '</td></tr>';
        }
        body.innerHTML = html;
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// ── Cycle mode form helpers ──────────────────────────────────────────

function onCycleModeChange(formId) {
    var form = document.getElementById(formId);
    var mode = form.querySelector('[name="cycle_mode"]').value;
    var fields = form.querySelector('.hotswap-fields');
    var staging2 = form.querySelectorAll('.staging2-row');

    // Node selection fields apply to all cycle modes
    fields.style.display = '';

    staging2.forEach(function(el) {
        el.style.display = (mode === 'single_robot') ? '' : 'none';
        if (mode !== 'single_robot') {
            el.querySelectorAll('.node-value-group select').forEach(function(sel) {
                sel.innerHTML = '<option value="">-- Select --</option>';
            });
        }
    });
}

function onNodeTypeChange(sel) {
    var row = sel.closest('.form-row');
    var valueGroup = row.querySelector('.node-value-group');
    if (sel.value) {
        valueGroup.style.display = '';
        var valueSel = valueGroup.querySelector('select');
        if (valueSel) populateNodeSelect(valueSel, sel.value);
    } else {
        valueGroup.style.display = 'none';
        var valueSel = valueGroup.querySelector('select');
        if (valueSel) { valueSel.innerHTML = '<option value="">-- Select --</option>'; }
    }
}

// Populate a node value dropdown with nodes from Core, filtered by type.
// nodeType: "node" = physical nodes (non-NGRP), "group" = NGRP synthetic groups
function populateNodeSelect(sel, nodeType) {
    var currentVal = sel.value;
    loadCoreNodeListFull(function(nodes) {
        sel.innerHTML = '<option value="">-- Select --</option>';
        for (var i = 0; i < nodes.length; i++) {
            var n = nodes[i];
            var isGroup = (n.node_type === 'NGRP');
            if (nodeType === 'group' && !isGroup) continue;
            if (nodeType === 'node' && isGroup) continue;
            var opt = document.createElement('option');
            opt.value = n.name;
            opt.textContent = n.name;
            if (n.name === currentVal) opt.selected = true;
            sel.appendChild(opt);
        }
    });
}

// Cache full node objects (name + node_type) from Core
var _coreNodeListFull = null;

function loadCoreNodeListFull(cb) {
    if (_coreNodeListFull !== null) { cb(_coreNodeListFull); return; }
    ShingoEdge.api.get('/api/core-nodes').then(function(nodes) {
        _coreNodeListFull = (nodes || []).sort(function(a, b) {
            return a.name.localeCompare(b.name);
        });
        cb(_coreNodeListFull);
    }).catch(function() { cb([]); });
}

// Cache location nodes from Edge DB (node_id, description)
var _locationNodeList = null;

function loadLocationNodeList(cb) {
    if (_locationNodeList !== null) { cb(_locationNodeList); return; }
    ShingoEdge.api.get('/api/location-nodes').then(function(nodes) {
        _locationNodeList = (nodes || []).sort(function(a, b) {
            return a.node_id.localeCompare(b.node_id);
        });
        cb(_locationNodeList);
    }).catch(function() { cb([]); });
}

function populateLocationSelect(sel, currentVal, cb) {
    loadLocationNodeList(function(nodes) {
        sel.innerHTML = '<option value="">-- Select --</option>';
        for (var i = 0; i < nodes.length; i++) {
            var n = nodes[i];
            var opt = document.createElement('option');
            opt.value = n.node_id;
            opt.textContent = n.node_id + (n.description ? ' \u2013 ' + n.description : '');
            if (n.node_id === currentVal) opt.selected = true;
            sel.appendChild(opt);
        }
        if (cb) cb();
    });
}

function readCycleModeFields(formId) {
    var form = document.getElementById(formId);
    var result = { cycle_mode: 'sequential', staging_node: '', staging_node_group: '', staging_node_2: '', staging_node_2_group: '', full_pickup_node: '', full_pickup_node_group: '', outgoing_node: '', outgoing_node_group: '' };

    var mode = form.querySelector('[name="cycle_mode"]');
    if (mode) result.cycle_mode = mode.value || 'sequential';

    // Full Pickup
    var fpType = form.querySelector('[name="full_pickup_type"]');
    var fpVal = form.querySelector('[name="full_pickup_value"]');
    if (fpType && fpVal && fpType.value === 'node') result.full_pickup_node = fpVal.value;
    else if (fpType && fpVal && fpType.value === 'group') result.full_pickup_node_group = fpVal.value;

    // Staging 1
    var stType = form.querySelector('[name="staging_type"]');
    var stVal = form.querySelector('[name="staging_value"]');
    if (stType && stVal && stType.value === 'node') result.staging_node = stVal.value;
    else if (stType && stVal && stType.value === 'group') result.staging_node_group = stVal.value;

    // Staging 2
    var st2Type = form.querySelector('[name="staging2_type"]');
    var st2Val = form.querySelector('[name="staging2_value"]');
    if (st2Type && st2Val && st2Type.value === 'node') result.staging_node_2 = st2Val.value;
    else if (st2Type && st2Val && st2Type.value === 'group') result.staging_node_2_group = st2Val.value;

    // Empty Drop
    var edType = form.querySelector('[name="outgoing_type"]');
    var edVal = form.querySelector('[name="outgoing_value"]');
    if (edType && edVal && edType.value === 'node') result.outgoing_node = edVal.value;
    else if (edType && edVal && edType.value === 'group') result.outgoing_node_group = edVal.value;

    return result;
}

function populateCycleModeFields(formId, p) {
    var form = document.getElementById(formId);

    var modeSelect = form.querySelector('[name="cycle_mode"]');
    if (modeSelect) modeSelect.value = p.cycle_mode || 'sequential';

    function setNodeField(typeName, valueName, nodeVal, groupVal) {
        var typeEl = form.querySelector('[name="' + typeName + '"]');
        var valEl = form.querySelector('[name="' + valueName + '"]');
        if (!typeEl || !valEl) return;
        var targetVal = nodeVal || groupVal || '';
        if (nodeVal) { typeEl.value = 'node'; }
        else if (groupVal) { typeEl.value = 'group'; }
        else { typeEl.value = ''; }
        // Trigger node type change to show/populate the dropdown
        var row = typeEl.closest('.form-row');
        var valueGroup = row.querySelector('.node-value-group');
        if (typeEl.value) {
            valueGroup.style.display = '';
            populateNodeSelect(valEl, typeEl.value);
            // Set value after populate completes (async)
            loadCoreNodeListFull(function() { valEl.value = targetVal; });
        } else {
            valueGroup.style.display = 'none';
            valEl.innerHTML = '<option value="">-- Select --</option>';
        }
    }

    setNodeField('full_pickup_type', 'full_pickup_value', p.full_pickup_node, p.full_pickup_node_group);
    setNodeField('staging_type', 'staging_value', p.staging_node, p.staging_node_group);
    setNodeField('staging2_type', 'staging2_value', p.staging_node_2, p.staging_node_2_group);
    setNodeField('outgoing_type', 'outgoing_value', p.outgoing_node, p.outgoing_node_group);

    onCycleModeChange(formId);
}

// ── Payload CRUD ─────────────────────────────────────────────────────

async function addPayload() {
    var jsID = document.getElementById('payload-job-style').value;
    if (!jsID) { ShingoEdge.toast('Select a style first', 'warning'); return; }
    var d = ShingoEdge.getFormData('payload-add-form');
    d.job_style_id = parseInt(jsID);
    d.reorder_point = parseInt(d.reorder_point) || 0;

    // Merge cycle mode fields
    var cm = readCycleModeFields('payload-add-form');
    Object.assign(d, cm);

    // Clean up UI-only fields
    delete d.full_pickup_type; delete d.full_pickup_value;
    delete d.staging_type; delete d.staging_value;
    delete d.staging2_type; delete d.staging2_value;
    delete d.outgoing_type; delete d.outgoing_value;

    try {
        await ShingoEdge.api.post('/api/payloads', d);
        ShingoEdge.toast('Payload added', 'success');
        ShingoEdge.hideModal('payload-add');
        loadPayloads();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

function openAddPayload() {
    var sel = document.querySelector('#payload-add-form [name="location"]');
    populateLocationSelect(sel, '', function() {
        ShingoEdge.showModal('payload-add');
    });
}

function openEditPayload(p) {
    var sel = document.querySelector('#payload-edit-form [name="location"]');
    populateLocationSelect(sel, p.location, function() {
        ShingoEdge.populateForm('payload-edit-form', {
            id: p.id, location: p.location,
            description: p.description, payload_code: p.payload_code || '',
            role: p.role, auto_reorder: p.auto_reorder,
            reorder_point: p.reorder_point
        });
        populateCycleModeFields('payload-edit-form', p);
        ShingoEdge.showModal('payload-edit');
    });
}

async function savePayload() {
    var d = ShingoEdge.getFormData('payload-edit-form');
    var id = d.id; delete d.id;
    d.reorder_point = parseInt(d.reorder_point) || 0;

    // Merge cycle mode fields
    var cm = readCycleModeFields('payload-edit-form');
    Object.assign(d, cm);

    // Clean up UI-only fields
    delete d.full_pickup_type; delete d.full_pickup_value;
    delete d.staging_type; delete d.staging_value;
    delete d.staging2_type; delete d.staging2_value;
    delete d.outgoing_type; delete d.outgoing_value;

    try {
        await ShingoEdge.api.put('/api/payloads/' + id, d);
        ShingoEdge.toast('Payload updated', 'success');
        ShingoEdge.hideModal('payload-edit');
        loadPayloads();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function deletePayload(id) {
    var ok = await ShingoEdge.confirm('Delete this payload?');
    if (!ok) return;
    try {
        await ShingoEdge.api.del('/api/payloads/' + id);
        ShingoEdge.toast('Deleted', 'success');
        loadPayloads();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function resetPayload(id) {
    var ok = await ShingoEdge.confirm('Reset this payload to full capacity?');
    if (!ok) return;
    try {
        await ShingoEdge.api.put('/api/payloads/' + id + '/count', { piece_count: 0, reset: true });
        ShingoEdge.toast('Payload reset', 'success');
        loadPayloads();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// --- Core Nodes ---
var _coreNodeSet = {};

async function fetchCoreNodes() {
    try {
        var nodes = await ShingoEdge.api.get('/api/core-nodes');
        _coreNodeSet = {};
        for (var i = 0; i < (nodes || []).length; i++) {
            _coreNodeSet[nodes[i].name] = true;
        }
    } catch (e) { /* ignore */ }
}

async function syncCoreNodes() {
    try {
        await ShingoEdge.api.post('/api/core-nodes/sync', {});
        ShingoEdge.toast('Syncing nodes...', 'success');
        // Invalidate both cached node lists so pickers re-fetch
        _coreNodeList = null;
        _coreNodeListFull = null;
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// --- Location Nodes (nested under processes) ---
async function loadLineNodes(lineID) {
    var container = document.getElementById('node-list-' + lineID);
    if (!container) return;
    try {
        var nodes = await ShingoEdge.api.get('/api/lines/' + lineID + '/location-nodes');
        if (!nodes || nodes.length === 0) {
            container.innerHTML = '';
            return;
        }
        var html = '<div style="border-top:1px solid var(--border);padding-top:0.4rem;margin-top:0.25rem">' +
            '<label style="display:block;margin-bottom:0.25rem;font-weight:500;font-size:0.8rem;color:var(--text-muted)">Location Nodes</label>' +
            '<div style="display:flex;gap:0.4rem;flex-wrap:wrap">';
        for (var i = 0; i < nodes.length; i++) {
            var n = nodes[i];
            var confirmed = _coreNodeSet[n.node_id];
            var chipClass = confirmed ? 'style-chip style-chip-active' : 'style-chip style-chip-inactive';
            var tip = n.description ? ShingoEdge.escapeHtml(n.description) : '';
            if (!confirmed) tip = (tip ? tip + ' — ' : '') + 'Unconfirmed (not in core)';
            html += '<span class="' + chipClass + '" onclick="openEditNode(' + n.id + ',' + n.line_id + ')" title="' + tip + '" style="cursor:pointer">' + ShingoEdge.escapeHtml(n.node_id) + '</span>';
        }
        html += '</div></div>';
        container.innerHTML = html;
    } catch (e) {
        container.innerHTML = '';
    }
}

async function refreshAllLineNodes() {
    await fetchCoreNodes();
    var cards = document.querySelectorAll('[data-line-id]');
    for (var i = 0; i < cards.length; i++) {
        loadLineNodes(parseInt(cards[i].getAttribute('data-line-id')));
    }
}

// Load all nodes on page load
(function() { refreshAllLineNodes(); })();

var _currentNodeLineID = 0;

function openAddNode(lineID) {
    _currentNodeLineID = lineID;
    var form = document.getElementById('node-add-form');
    form.querySelector('[name="line_id"]').value = lineID;
    form.querySelector('[name="node_id"]').value = '';
    form.querySelector('[name="description"]').value = '';
    ShingoEdge.showModal('node-add');
}

async function addNode() {
    var d = ShingoEdge.getFormData('node-add-form');
    d.line_id = parseInt(d.line_id);
    try {
        await ShingoEdge.api.post('/api/location-nodes', d);
        ShingoEdge.toast('Node added', 'success');
        ShingoEdge.hideModal('node-add');
        loadLineNodes(d.line_id);
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function openEditNode(id, lineID) {
    _currentNodeLineID = lineID;
    // Fetch current node data
    var nodes = await ShingoEdge.api.get('/api/lines/' + lineID + '/location-nodes');
    var node = null;
    for (var i = 0; i < (nodes || []).length; i++) {
        if (nodes[i].id === id) { node = nodes[i]; break; }
    }
    if (!node) { ShingoEdge.toast('Node not found', 'error'); return; }
    ShingoEdge.populateForm('node-edit-form', { id: id, line_id: lineID, node_id: node.node_id, description: node.description });
    ShingoEdge.showModal('node-edit');
}

async function saveNode() {
    var d = ShingoEdge.getFormData('node-edit-form');
    var id = d.id; delete d.id;
    d.line_id = parseInt(d.line_id);
    try {
        await ShingoEdge.api.put('/api/location-nodes/' + id, d);
        ShingoEdge.toast('Node updated', 'success');
        ShingoEdge.hideModal('node-edit');
        loadLineNodes(d.line_id);
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function deleteNode(id, lineID) {
    var ok = await ShingoEdge.confirm('Delete this location node?');
    if (!ok) return;
    try {
        await ShingoEdge.api.del('/api/location-nodes/' + id);
        ShingoEdge.toast('Deleted', 'success');
        loadLineNodes(lineID || _currentNodeLineID);
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function deleteNodeFromModal() {
    var form = document.getElementById('node-edit-form');
    var id = parseInt(form.querySelector('[name="id"]').value);
    var lineID = parseInt(form.querySelector('[name="line_id"]').value);
    var ok = await ShingoEdge.confirm('Delete this location node?');
    if (!ok) return;
    try {
        await ShingoEdge.api.del('/api/location-nodes/' + id);
        ShingoEdge.toast('Deleted', 'success');
        ShingoEdge.hideModal('node-edit');
        loadLineNodes(lineID);
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// --- Node Picker (core nodes dropdown) ---
var _coreNodeList = null;

function loadCoreNodeList(cb) {
    if (_coreNodeList !== null) { cb(_coreNodeList); return; }
    ShingoEdge.api.get('/api/core-nodes').then(function(names) {
        _coreNodeList = (names || []).sort();
        cb(_coreNodeList);
    }).catch(function() { cb([]); });
}

function openNodePicker(input) {
    loadCoreNodeList(function(nodes) {
        renderNodeDropdown(input, nodes, input.value);
    });
}

function filterNodePicker(input) {
    var nodes = _coreNodeList || [];
    renderNodeDropdown(input, nodes, input.value);
}

function renderNodeDropdown(input, nodes, filter) {
    var dropdown = input.parentElement.querySelector('.tag-picker-dropdown');
    if (!dropdown) return;
    var lc = (filter || '').toLowerCase();
    var filtered = nodes.filter(function(n) { return !lc || n.toLowerCase().indexOf(lc) !== -1; });
    if (filtered.length === 0) {
        dropdown.innerHTML = '<div class="tag-picker-empty">No matching nodes</div>';
    } else {
        var html = '';
        var limit = Math.min(filtered.length, 100);
        for (var i = 0; i < limit; i++) {
            html += '<div class="tag-picker-item" onmousedown="selectNodePickerItem(this, \'' + ShingoEdge.escapeHtml(filtered[i]).replace(/'/g, "\\'") + '\')">';
            html += '<span class="tag-picker-name">' + ShingoEdge.escapeHtml(filtered[i]) + '</span>';
            html += '</div>';
        }
        if (filtered.length > limit) {
            html += '<div class="tag-picker-empty">' + (filtered.length - limit) + ' more...</div>';
        }
        dropdown.innerHTML = html;
    }
    dropdown.style.display = '';
}

function selectNodePickerItem(el, nodeName) {
    var picker = el.closest('.tag-picker');
    var input = picker.querySelector('.tag-picker-input');
    input.value = nodeName;
    picker.querySelector('.tag-picker-dropdown').style.display = 'none';
}

// --- Messaging (Kafka) ---
function addBrokerRow() {
    var container = document.getElementById('broker-rows');
    var row = document.createElement('div');
    row.className = 'broker-row';
    row.innerHTML = '<input type="text" class="form-input broker-host" style="flex:1" placeholder="localhost">' +
        '<input type="number" class="form-input broker-port" style="width:80px" placeholder="9092">' +
        '<button class="btn btn-sm" onclick="testBroker(this)">Test</button>' +
        '<span class="broker-status"></span>' +
        '<button class="btn-icon btn-icon-danger" onclick="removeBrokerRow(this)" title="Remove">&#10005;</button>';
    container.appendChild(row);
}

function removeBrokerRow(btn) {
    var row = btn.closest('.broker-row');
    var container = document.getElementById('broker-rows');
    if (container.querySelectorAll('.broker-row').length > 1) {
        row.remove();
    } else {
        row.querySelector('.broker-host').value = '';
    }
}

function collectBrokers() {
    var rows = document.querySelectorAll('.broker-row');
    var brokers = [];
    for (var i = 0; i < rows.length; i++) {
        var host = rows[i].querySelector('.broker-host').value.trim();
        var port = rows[i].querySelector('.broker-port').value.trim();
        if (host) brokers.push(host + ':' + (port || '9092'));
    }
    return brokers;
}

function brokerAddr(row) {
    var host = row.querySelector('.broker-host').value.trim();
    var port = row.querySelector('.broker-port').value.trim();
    return host ? host + ':' + (port || '9092') : '';
}

async function testBroker(btn) {
    var row = btn.closest('.broker-row');
    var host = brokerAddr(row);
    var status = row.querySelector('.broker-status');
    if (!host) { status.textContent = ''; return; }
    status.textContent = 'Testing...';
    status.className = 'broker-status';
    try {
        var res = await ShingoEdge.api.post('/api/config/kafka/test', { broker: host });
        if (res.connected) {
            status.textContent = 'Connected';
            status.className = 'broker-status broker-status-ok';
        } else {
            status.textContent = res.error || 'Failed';
            status.className = 'broker-status broker-status-err';
        }
    } catch (e) {
        status.textContent = 'Error';
        status.className = 'broker-status broker-status-err';
    }
}

// --- Password ---
async function changePassword() {
    try {
        await ShingoEdge.api.post('/api/config/password', {
            old_password: document.getElementById('pw-old').value,
            new_password: document.getElementById('pw-new').value
        });
        ShingoEdge.toast('Password changed', 'success');
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// --- Persistent Toast ---
function showPLCAlert(plcName, error) {
    // Dedup by PLC name
    var existing = document.querySelector('[data-plc-alert="' + plcName + '"]');
    if (existing) return;
    var container = document.querySelector('.toast-container');
    if (!container) {
        container = document.createElement('div');
        container.className = 'toast-container';
        document.body.appendChild(container);
    }
    var toast = document.createElement('div');
    toast.className = 'toast toast-persistent';
    toast.setAttribute('data-plc-alert', plcName);
    toast.innerHTML = '<span class="toast-msg">PLC ' + ShingoEdge.escapeHtml(plcName) + ' offline' + (error ? ': ' + ShingoEdge.escapeHtml(error) : '') + '</span>' +
        '<button class="toast-close" onclick="this.parentElement.remove()">&times;</button>';
    container.appendChild(toast);
}

function dismissPLCAlert(plcName) {
    var el = document.querySelector('[data-plc-alert="' + plcName + '"]');
    if (el) el.remove();
}

// --- SSE ---
ShingoEdge.createSSE('/events', {
    onPlcHealthAlert: function(data) {
        showPLCAlert(data.plc_name, data.error);
        var dot = document.getElementById('plc-health-' + data.plc_name);
        if (dot) { dot.className = 'plc-health-dot plc-health-offline'; }
    },
    onPlcHealthRecover: function(data) {
        dismissPLCAlert(data.plc_name);
        var dot = document.getElementById('plc-health-' + data.plc_name);
        if (dot) { dot.className = 'plc-health-dot plc-health-online'; }
    },
    onPlcStatus: function(data) {
        var el = document.getElementById('plc-status-' + data.plcName);
        if (el) {
            // Preserve the health dot
            var dot = el.querySelector('.plc-health-dot');
            el.className = 'plc-chip ' + (data.connected ? 'plc-chip-connected' : 'plc-chip-disconnected');
            if (dot && !el.contains(dot)) el.insertBefore(dot, el.firstChild);
        }
        if (data.connected) {
            dismissPLCAlert(data.plcName);
            var hdot = document.getElementById('plc-health-' + data.plcName);
            if (hdot) { hdot.className = 'plc-health-dot plc-health-online'; }
        }
    },
    onCoreNodes: function(data) {
        _coreNodeSet = {};
        var nodes = data.nodes || [];
        for (var i = 0; i < nodes.length; i++) {
            _coreNodeSet[nodes[i].name] = true;
        }
        _coreNodeList = nodes.map(function(n) { return n.name; }).sort();
        _coreNodeListFull = null; // Invalidate full list so cycle mode dropdowns re-fetch with types
        // Refresh node chips without re-fetching core nodes
        var cards = document.querySelectorAll('[data-line-id]');
        for (var i = 0; i < cards.length; i++) {
            loadLineNodes(parseInt(cards[i].getAttribute('data-line-id')));
        }
        ShingoEdge.toast('Node list updated (' + nodes.length + ' nodes)', 'success');
    },
    onWarlinkStatus: function(data) {
        var badge = document.getElementById('warlink-status');
        if (badge) {
            badge.textContent = data.connected ? 'Connected' : 'Disconnected';
            badge.className = 'status-badge ' + (data.connected ? 'status-connected' : 'status-disconnected');
        }
        if (data.connected) {
            refreshPLCChips();
        } else {
            // Mark all chips disconnected
            var chips = document.querySelectorAll('.plc-chip');
            for (var i = 0; i < chips.length; i++) {
                chips[i].className = 'plc-chip plc-chip-disconnected';
            }
        }
    }
});
