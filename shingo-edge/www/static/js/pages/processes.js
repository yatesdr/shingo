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

// Process + group data injected by the template for sidebar rendering.
var _processes = [];
var _processGroups = [];
try {
    var _pd = document.getElementById('page-data');
    if (_pd) {
        _processes = JSON.parse(_pd.dataset.processes || '[]') || [];
        _processGroups = JSON.parse(_pd.dataset.processGroups || '[]') || [];
    }
} catch(e) {}

// ─── Process editor ─────────────────────────────────────────────────────

function resetProcessForm() {
    document.getElementById('new-process-name').value = '';
    document.getElementById('new-process-description').value = '';
    var el = document.getElementById('new-process-counter-tag');
    if (el) el.value = '';
    var sel = document.getElementById('new-process-counter-plc');
    if (sel) sel.selectedIndex = 0;
    var grp = document.getElementById('new-process-group');
    if (grp) grp.value = '';
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
    const groupVal = document.getElementById('new-process-group') ? document.getElementById('new-process-group').value : '';
    var groupID = null;
    if (groupVal) groupID = parseInt(groupVal, 10);
    try {
        const res = await api.post('/api/processes', {
            name: name,
            description: document.getElementById('new-process-description').value.trim(),
            production_state: 'active_production',
            counter_plc_name: counterPLC,
            counter_tag_name: counterTag,
            counter_enabled: !!(counterPLC && counterTag),
            group_id: groupID
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
    var groupVal = document.getElementById('process-group') ? document.getElementById('process-group').value : '';
    var groupID = null;
    if (groupVal) groupID = parseInt(groupVal, 10);
    try {
        await api.put('/api/processes/' + activeProcessID, {
            name: document.getElementById('process-name').value.trim(),
            description: document.getElementById('process-description').value.trim(),
            production_state: document.getElementById('process-production-state').value,
            counter_plc_name: document.getElementById('counter-plc') ? document.getElementById('counter-plc').value : '',
            counter_tag_name: document.getElementById('counter-tag') ? document.getElementById('counter-tag').value.trim() : '',
            counter_enabled: document.getElementById('counter-enabled') ? document.getElementById('counter-enabled').checked : false,
            changeover_auto_arm: document.getElementById('changeover-auto-arm') ? document.getElementById('changeover-auto-arm').value : 'auto',
            group_id: groupID
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

// ─── Process groups (sidebar organizational taxonomy) ──────────────────

function resetGroupForm() {
    var idEl = document.getElementById('group-id');
    if (idEl) idEl.value = '';
    var nameEl = document.getElementById('group-name');
    if (nameEl) nameEl.value = '';
    var descEl = document.getElementById('group-description');
    if (descEl) descEl.value = '';
}

function openCreateGroupModal() {
    resetGroupForm();
    document.getElementById('group-modal-title').textContent = 'Add Group';
    populateGroupProcessPicker(null);
    showModal('group-modal');
}

function closeGroupModal() {
    hideModal('group-modal');
    resetGroupForm();
}

// populateGroupProcessPicker renders checkboxes for ungrouped processes
// in the group modal. When editing, processes already in this group are
// checked too (so the user can uncheck to remove them).
function populateGroupProcessPicker(currentGroupID) {
    var picker = document.getElementById('group-process-picker');
    if (!picker) return;
    picker.innerHTML = '';

    var candidates = _processes.filter(function(p) {
        return !p.group_id || (currentGroupID && p.group_id === currentGroupID);
    });

    if (candidates.length === 0) {
        picker.innerHTML = '<div class="text-muted-xs" style="padding:0.3rem 0">No ungrouped processes available.</div>';
        return;
    }

    candidates.forEach(function(p) {
        var label = document.createElement('label');
        label.className = 'check-row';
        var cb = document.createElement('input');
        cb.type = 'checkbox';
        cb.value = p.id;
        cb.className = 'group-process-cb';
        if (currentGroupID && p.group_id === currentGroupID) cb.checked = true;
        label.appendChild(cb);
        var span = document.createElement('span');
        span.textContent = p.name;
        label.appendChild(span);
        picker.appendChild(label);
    });
}

async function saveGroup() {
    var id = document.getElementById('group-id').value;
    var name = document.getElementById('group-name').value.trim();
    var description = document.getElementById('group-description').value.trim();
    if (!name) {
        toast('Enter a group name', 'warning');
        return;
    }
    try {
        var groupID;
        if (id) {
            await api.put('/api/process-groups/' + id, { name: name, description: description });
            groupID = parseInt(id, 10);
            toast('Group updated', 'success');
        } else {
            var res = await api.post('/api/process-groups', { name: name, description: description });
            groupID = res.id;
            toast('Group created', 'success');
        }
        // Reconcile membership against the picker. Each candidate row
        // the picker showed (ungrouped + this group's members) is compared
        // to its checkbox: a checked box assigns to this group, an
        // unchecked box that was previously a member ungroups it. Without
        // this diff, unchecking a member to remove it from the group was
        // silently dropped — the row stayed in the group in the DB.
        var picker = document.getElementById('group-process-picker');
        if (picker) {
            var cbs = picker.querySelectorAll('.group-process-cb');
            cbs.forEach(function(cb) {
                var pid = parseInt(cb.value, 10);
                var proc = _processes.find(function(p) { return p.id === pid; });
                if (!proc) return;
                var wasMember = proc.group_id && proc.group_id === groupID;
                var wantsIn = cb.checked;
                if (wantsIn && !wasMember) {
                    api.put('/api/processes/' + pid + '/group', { group_id: groupID }).catch(function(e) {
                        toast('Failed to assign process ' + pid + ': ' + e, 'warning');
                    });
                } else if (!wantsIn && wasMember) {
                    api.put('/api/processes/' + pid + '/group', { group_id: null }).catch(function(e) {
                        toast('Failed to ungroup process ' + pid + ': ' + e, 'warning');
                    });
                }
            });
        }
        location.reload();
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

function editGroup(id) {
    // Use the in-memory _processGroups array instead of a server fetch
    // (there's no GET /api/process-groups/{id} route — only list, and
    // PUT/DELETE on {id}).
    var group = _processGroups.find(function(g) { return g.id === id; });
    if (!group) {
        toast('Group not found', 'error');
        return;
    }
    document.getElementById('group-id').value = group.id;
    document.getElementById('group-name').value = group.name || '';
    document.getElementById('group-description').value = group.description || '';
    document.getElementById('group-modal-title').textContent = 'Edit Group';
    populateGroupProcessPicker(group.id);
    showModal('group-modal');
}

async function deleteGroup(id) {
    try {
        var resp = await api.get('/api/process-groups/' + id + '/member-count');
        var count = resp.count || 0;
        var msg = count > 0
            ? 'Delete this group? ' + count + ' process(es) will be moved back to Ungrouped.'
            : 'Delete this empty group?';
        if (!await confirm(msg)) return;
        await api.del('/api/process-groups/' + id);
        toast('Group deleted', 'success');
        location.reload();
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

// renderSidebar renders the process list grouped by group, with collapsible
// sections. Ungrouped is always last. Collapse state persists in localStorage.
function renderSidebar() {
    var container = document.getElementById('process-sidebar');
    if (!container) return;
    container.innerHTML = '';

    // Build a set of valid group ids so a process whose group_id points
    // at a group that no longer exists (deleted in another tab, imported
    // from a backup, written by direct API) falls through to Ungrouped
    // instead of vanishing — byGroup[orphanID] is never read by the
    // _processGroups loop, so without this guard the process is silently
    // dropped from every section.
    var validGroupIDs = {};
    _processGroups.forEach(function(g) { validGroupIDs[g.id] = true; });

    // Group processes by group_id
    var byGroup = {};
    var ungrouped = [];
    _processes.forEach(function(p) {
        if (p.group_id && validGroupIDs[p.group_id]) {
            if (!byGroup[p.group_id]) byGroup[p.group_id] = [];
            byGroup[p.group_id].push(p);
        } else {
            ungrouped.push(p);
        }
    });

    // Render each group (alphabetical by name)
    _processGroups.forEach(function(g) {
        var members = byGroup[g.id] || [];
        var collapsed = isGroupCollapsed(g.id);

        var header = document.createElement('div');
        header.className = 'proc-group-header' + (collapsed ? ' collapsed' : '');
        header.onclick = function() { toggleGroupCollapse(g.id); renderSidebar(); };
        header.innerHTML =
            '<span class="proc-group-chevron">&#9662;</span>' +
            '<span>' + escapeHtml(g.name) + '</span>' +
            '<span class="proc-group-count">(' + members.length + ')</span>' +
            '<span class="proc-group-actions">' +
                '<button onclick="event.stopPropagation();editGroup(' + g.id + ')" title="Edit group">&#9998;</button>' +
                '<button onclick="event.stopPropagation();deleteGroup(' + g.id + ')" title="Delete group">&times;</button>' +
            '</span>';

        var body = document.createElement('div');
        body.className = 'proc-group-body' + (collapsed ? ' collapsed' : '');
        if (members.length === 0) {
            body.innerHTML = '<div class="proc-group-empty">No processes in this group.</div>';
        } else {
            members.forEach(function(p) {
                body.appendChild(renderProcessLink(p));
            });
        }

        container.appendChild(header);
        container.appendChild(body);
    });

    // Ungrouped section (always last, no edit/delete)
    if (ungrouped.length > 0 || _processGroups.length === 0) {
        var ugCollapsed = isGroupCollapsed('__ungrouped__');
        var ugHeader = document.createElement('div');
        ugHeader.className = 'proc-group-header' + (ugCollapsed ? ' collapsed' : '');
        ugHeader.onclick = function() { toggleGroupCollapse('__ungrouped__'); renderSidebar(); };
        var ugLabel = _processGroups.length > 0 ? 'Ungrouped' : 'All Processes';
        ugHeader.innerHTML =
            '<span class="proc-group-chevron">&#9662;</span>' +
            '<span>' + ugLabel + '</span>' +
            '<span class="proc-group-count">(' + ungrouped.length + ')</span>';

        var ugBody = document.createElement('div');
        ugBody.className = 'proc-group-body' + (ugCollapsed ? ' collapsed' : '');
        if (ungrouped.length === 0) {
            ugBody.innerHTML = '<div class="proc-group-empty">No ungrouped processes.</div>';
        } else {
            ungrouped.forEach(function(p) {
                ugBody.appendChild(renderProcessLink(p));
            });
        }

        container.appendChild(ugHeader);
        container.appendChild(ugBody);
    }
}

function renderProcessLink(p) {
    var a = document.createElement('a');
    a.href = '/processes?process=' + p.id;
    a.className = 'btn' + (p.id === activeProcessID ? ' btn-primary' : '');
    a.style.justifyContent = 'flex-start';
    a.style.textAlign = 'left';
    var html = '<span>' + escapeHtml(p.name) + '</span>';
    if (p.target_style_id) {
        html += '<span class="ml-auto text-muted-xs" title="Changeover target style set">CO</span>';
    }
    a.innerHTML = html;
    return a;
}

var GROUP_COLLAPSE_KEY = 'shingo-edge-process-group-collapse';

function isGroupCollapsed(groupId) {
    try {
        var state = JSON.parse(localStorage.getItem(GROUP_COLLAPSE_KEY) || '{}');
        return !!state[groupId];
    } catch(e) { return false; }
}

function toggleGroupCollapse(groupId) {
    try {
        var state = JSON.parse(localStorage.getItem(GROUP_COLLAPSE_KEY) || '{}');
        state[groupId] = !state[groupId];
        localStorage.setItem(GROUP_COLLAPSE_KEY, JSON.stringify(state));
    } catch(e) {}
}

// Render the sidebar on page load.
renderSidebar();

// editGroup and deleteGroup are called via inline onclick in the sidebar
// HTML (rendered by renderSidebar), so they need to be on window.
window.editGroup = editGroup;
window.deleteGroup = deleteGroup;

// ─── Style editor ───────────────────────────────────────────────────────

function resetStyleForm() {
    document.getElementById('style-id').value = '';
    document.getElementById('style-name').value = '';
    document.getElementById('style-description').value = '';
    document.getElementById('style-expected-catid').value = '';
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
    document.getElementById('style-expected-catid').value = style.expected_catid || '';
    document.getElementById('style-modal-title').textContent = 'Edit Style';
    showModal('style-modal');
}

async function saveStyle() {
    const id = document.getElementById('style-id').value;
    const payload = {
        name: document.getElementById('style-name').value.trim(),
        description: document.getElementById('style-description').value.trim(),
        expected_catid: document.getElementById('style-expected-catid').value.trim(),
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
// not touch (staging, pairing, flags).
//
// index_robot_supplies is absent here too, and it is pointer-typed for exactly
// this reason: it describes the press's hardware, and a grid cell edit about a
// payload must not be able to flip a cell's choreography by omitting a field.
//
// SEND ONLY WHAT THIS EDITOR OWNS. The compare grid edits one field per cell,
// so sequence, reorder_point_source, keep_staged, auto_reorder, the two
// changeover-evacuation fields, the loader-card flag and the key route are all
// omitted here — it has a control for none of them. (The claim MODAL has
// controls for them and sends those; this grid is a different surface with a
// different answer, which is the point of absent-means-untouched.)
//
// auto_reorder used to be echoed back by hand here — read the claim, send its
// own value — which was the same problem patched one field at a time, and only
// after a hard-coded `true` had spent a while re-arming cell auto-reorder on
// every claim an engineer touched. changeover_evac_nodes and
// changeover_evac_destination were echoed for exactly the same reason, and were
// deleted when the store contract was extended to cover them. The echo is never
// the fix: it is correct only for the surfaces someone remembered, and the two
// it covered here still left key_route and key_task
// unprotected. Do not reintroduce one.
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
        inbound_staging: c.inbound_staging || '',
        outbound_staging: c.outbound_staging || '',
        inbound_source: c.inbound_source || '',
        outbound_destination: c.outbound_destination || '',
        auto_request_payload: c.auto_request_payload || '',
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

// ROUND-3 AND ROUND-4 SLOTS.
//
// Both rounds add fields to this editor. Their visibility rules are written
// here NOW, inert behind these two constants, so that those rounds add a
// control and a table value rather than going hunting for every place a field
// has to be registered — which is the trap this table exists to close and the
// one that made IndexRobotSupplies unenterable in the round-1 design review.
//
// The rules are real, not placeholders: flipping a constant is what turns the
// group on, and nothing else here has to change. Until then every entry
// evaluates false, no DOM exists for them, and renderClaimForm skips an id it
// cannot resolve.
const ROUND3_CHANGEOVER = true; // per-position tooling relevance + evac destination (round 3)
const ROUND4_ROUTING = true;     // IndexRobotSupplies + key routes — round 4 shipped both

// hasThirdPosition is read from the form rather than passed, because the
// visibility table's (role, swap) signature is load-bearing: every caller and
// the whole characterization matrix key on those two. A third argument would
// have to be threaded through all of them to answer one row.
function claimFieldVisibility(role, swap) {
    const secondEl = document.getElementById('claims-add-second-paired-node');
    const hasThirdPosition = !!(secondEl && secondEl.value);
    const isManual = swap === 'manual_swap';
    const isPressIndex = swap === 'two_robot_press_index';
    const usesStaging = swap === 'single_robot' || swap === 'two_robot';
    // role is now constrained to consume|produce; the legacy "changeover"
    // role was removed during the UI consistency refactor and is no
    // longer present in either the protocol or the editor's dropdown.
    //
    // The A/B (paired-node) fieldset is only meaningful for the two modes whose
    // engine dispatch reads PairedCoreNode/SecondPairedCoreNode: sequential
    // (A/B cycling) and two_robot_press_index. single_robot and two_robot use
    // staging, not paired nodes, so the fieldset must NOT render for them.
    const showPair = swap === 'sequential' || isPressIndex;
    return {
        'claims-add-payload-group':           !isManual,
        'claims-add-allowed-group':           false,
        'claims-add-capacity-group':          !isManual,
        'claims-add-reorder-group':           !isManual,
        // Board order is an identity fact — every claim has a place in the
        // list, whatever it does.
        'claims-add-sequence-group':          true,
        // Auto-reorder arms the reorder point, so it shows exactly where the
        // reorder point does.
        'claims-add-auto-reorder-row':        !isManual,
        'claims-add-lineside-group':          role === 'consume' && !isManual,
        // Extracted from inside the field group when the numeric row became a
        // 3-column grid; it follows the field it explains.
        'claims-lineside-help':               role === 'consume' && !isManual,
        // Staging fieldset is hidden by manual_swap (no staging concept),
        // then further hidden when the swap mode doesn't use staging at
        // all (sequential / press_index).
        // Round 3's changeover-evac mode stages the incoming style at
        // InboundStaging, so press-index gains this fieldset THEN — and only
        // then. Today's runtime rule is unchanged: plain press-index neither
        // shows nor requires staging.
        'claims-staging-fieldset':            !isManual && (usesStaging || (ROUND3_CHANGEOVER && isPressIndex)),
        // keep_staged parks the incoming bin ON the staging node, so it is
        // meaningless without one.
        'claims-add-keep-staged-row':         !isManual && (usesStaging || (ROUND3_CHANGEOVER && isPressIndex)),
        'claims-add-swap-group':              true,
        'claims-source-fieldset':             !isManual,
        'claims-inbound-source-group':        !isManual,
        // Outbound destination is shown in every swap mode, including
        // two_robot (the old bin still goes somewhere).
        'claims-outbound-destination-group':  !isManual,
        'claims-changeover-fieldset':         !isManual,
        // Round 3 — Changeover fieldset. Per-position tooling relevance applies to
        // any consume or process node, not only presses, so it is not
        // press-index scoped. The evac destination is free-form: a node or a
        // group.
        // Per-position relevance is a press-index question — a single-position node
        // answers it with the Evacuate checkbox above.
        'claims-add-tooling-relevance-row':   ROUND3_CHANGEOVER && isPressIndex,
        // The individual positions follow the LAYOUT, not the mode: a 2-position
        // press has no third position, and offering one is how a selection that
        // can never fire gets made.
        'claims-position-front-row':              ROUND3_CHANGEOVER && isPressIndex,
        'claims-position-paired-row':             ROUND3_CHANGEOVER && isPressIndex,
        'claims-position-second-row':             ROUND3_CHANGEOVER && isPressIndex && hasThirdPosition,
        'claims-add-evac-destination-group':  ROUND3_CHANGEOVER && !isManual,
        // Carry-over only means anything for a marked position, and only a
        // press-index cell has positions to mark.
        'claims-add-carryover-group':         ROUND3_CHANGEOVER && isPressIndex,
        'claims-err-changeover-evac-positions':   false,
        'claims-ab-fieldset':                 showPair,
        'claims-add-second-paired-group':     showPair && isPressIndex,
        'claims-third-position-help':         showPair && isPressIndex,
        'claims-add-reuse-bins-row':          showPair && isPressIndex,
        // Round 4 — Press Index Pairing fieldset. Which robot supplies the
        // press is a per-claim fact (every other press-index geometry fact
        // already lives on NodeClaim), so it belongs beside the positions.
        // Round 4 shipped the flip. Registered by round 2, real now.
        'claims-add-index-robot-supplies-row': isPressIndex,
        // Was an unconditional `false`, which killed the whole group for every
        // claim type. 2635ad10 meant to hide it for manual_swap only — its
        // message says "every other claim type (presses, welds, ...) is
        // untouched" — and the inner claims-auto-request-standard entry below
        // was written that way. The parent was not, so auto_confirm has had no
        // reachable control since: the checkbox renders inside a hidden
        // fieldset, editClaim echoes the stored value into it, and no operator
        // can change it.
        'claims-auto-request-fieldset':       !isManual,
        'claims-auto-request-manual-swap':    false,
        'claims-auto-request-standard':       !isManual,
        // Auto-push is only meaningful for a consume manual_swap
        // (unloader pulling parts from a bin).
        'claims-add-auto-push-row':           isManual && role === 'consume',
        // The load directive is a LOADER's card behaviour, so it follows the
        // manual_swap fieldset. Role-neutral: a loader and an unloader both
        // have a card, and both can be told to name what the changeover needs.
        // Round 4 — Routing, a new fieldset. Key routes are the named paths a
        // leg may take; meaningless for a loader, which does not drive.
        'claims-routing-fieldset':            ROUND4_ROUTING && !isManual,
        'claims-add-key-routes-group':        ROUND4_ROUTING && !isManual,
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
        opt.textContent = p.code + (p.name ? ' — ' + p.name : '') + (p.uop_capacity ? ' (' + p.uop_capacity + ' UoP)' : '');
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
        table.className = 'table table-compact table-fit';
        table.innerHTML = '<colgroup>' +
            '<col style="width:11%">' +   // Core Node
            '<col style="width:9%">' +    // Role
            '<col style="width:9%">' +    // Swap
            '<col style="width:11%">' +   // Wants
            '<col style="width:9%">' +    // Inbound
            '<col style="width:9%">' +    // Outbound
            '<col style="width:9%">' +    // Source
            '<col style="width:9%">' +    // Dest
            '<col style="width:9%">' +    // A/B Pair
            '<col style="width:15%">' +   // Actions
            '</colgroup>' +
            '<thead><tr><th>Core Node</th><th>Role</th><th>Swap</th><th>Wants</th><th>Inbound</th><th>Outbound</th><th>Source</th><th>Dest</th><th>A/B Pair</th><th style="width:1%"></th></tr></thead>';
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
        '<td>' + esc(wants) + (c.uop_capacity ? ' <span style="color:var(--text-muted);font-size:0.8rem">(' + c.uop_capacity + ' UoP)</span>' : '') + '</td>' +
        '<td class="mono">' + esc(c.inbound_staging || '—') + '</td>' +
        '<td class="mono">' + esc(c.outbound_staging || '—') + '</td>' +
        '<td class="mono" style="font-size:0.8rem">' + esc(c.inbound_source || '—') + '</td>' +
        '<td class="mono" style="font-size:0.8rem">' + esc(c.outbound_destination || '—') + '</td>' +
        '<td class="mono" style="font-size:0.8rem">' + esc(c.paired_core_node || '—') + '</td>' +
        '<td style="white-space:normal;font-size:0.75rem">' +
            // manual_swap = a bin loader/unloader. Loaders are Core-owned now and
            // resolve via SynthClaim, so they are no longer authored or edited here
            // (the Swap Mode dropdown drops the option). An existing manual_swap claim
            // is read-only — show a hint instead of Edit — but stays removable so a
            // legacy/stray loader claim can still be cleaned up.
            (c.swap_mode === 'manual_swap'
                ? '<span class="badge" title="Bin loaders are configured on Core (Nodes -> loader setup) and added to an Operator Station; they resolve automatically here. This row is read-only.">Loader</span> '
                : '<button class="btn btn-sm" data-action="edit-claim" data-claim-id="' + c.id + '">Edit</button> ') +
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
        sequence: Math.max(0, parseInt(get('claims-add-sequence').value, 10) || 0),
        autoReorder: get('claims-add-auto-reorder').checked,
        keepStaged: get('claims-add-keep-staged').checked,
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
        indexRobotSupplies: get('claims-add-index-robot-supplies').checked,
        keyRoute: readKeyRoute(),
        keyTask: get('claims-add-key-task').value,
        changeoverEvacNodes: readEvacNodes(),
        changeoverEvacDestination: get('claims-add-evac-destination').value,
        changeoverCarryoverDisposition: get('claims-add-carryover').value,
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
    get('claims-add-sequence').value = String(state.sequence || 0);
    get('claims-add-auto-reorder').checked = !!state.autoReorder;
    get('claims-add-keep-staged').checked = !!state.keepStaged;
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
    get('claims-add-index-robot-supplies').checked = !!state.indexRobotSupplies;
    writeKeyRoute(state.keyRoute || []);
    get('claims-add-key-task').value = state.keyTask || '';
    // ORDER MATTERS: the checkboxes carry NODE names, and the values are filled
    // from the claim's layout. Match the stored marks only after the values
    // exist, or every box is empty-valued and nothing is ever checked.
    renderEvacNodeLabels(state);
    claimLoadedEvacNodes = (state.changeoverEvacNodes || []).slice();
    writeEvacNodes(state.changeoverEvacNodes || []);
    get('claims-add-evac-destination').value = state.changeoverEvacDestination || '';
    // Blank reads as replace everywhere else, so the control shows replace.
    get('claims-add-carryover').value = state.changeoverCarryoverDisposition || 'replace';
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

// ── Field-level validation feedback ─────────────────────────────────
//
// CLAIM_ERROR_SLOTS maps a validation finding's field name to the DOM it
// renders on: the message slot, and the input that gets the error border.
//
// KEYED ON THE WIRE NAME, because two validators feed this and only one of
// them is JavaScript. domain.ValidateNodeClaim (round 1) tags its findings
// with NodeClaimInput's json names, and it is the authority — the browser's
// validateClaimState is a fast local echo of the same rules, so its camelCase
// keys are normalised into these rather than the other way round. A rule that
// exists only on the server still lands on the right field.
const CLAIM_ERROR_SLOTS = {
    style_id:                { slot: 'claims-err-form' },
    core_node_name:          { slot: 'claims-err-core-node-name',          input: 'claims-add-node',
                               notice: 'claims-notice-core-node-name' },
    swap_mode:               { slot: 'claims-err-swap-mode',               input: 'claims-add-swap' },
    payload_code:            { slot: 'claims-err-payload-code',            input: 'claims-add-payload' },
    inbound_staging:         { slot: 'claims-err-inbound-staging',         input: 'claims-add-inbound' },
    outbound_staging:        { slot: 'claims-err-outbound-staging',        input: 'claims-add-outbound' },
    inbound_source:          { slot: 'claims-err-inbound-source',          input: 'claims-add-inbound-source' },
    outbound_destination:    { slot: 'claims-err-outbound-destination',    input: 'claims-add-outbound-destination' },
    paired_core_node:        { slot: 'claims-err-paired-core-node',        input: 'claims-add-paired-node' },
    second_paired_core_node: { slot: 'claims-err-second-paired-core-node', input: 'claims-add-second-paired-node' },
    // key_route has no single input to outline — it is a list, and the row at
    // fault is named in the message. The slot sits under the whole group.
    key_route:               { slot: 'claims-err-key-route' },
    key_task:                { slot: 'claims-err-key-task',                input: 'claims-add-key-task' },
};

// The browser validator's own key spellings, normalised to wire names.
// `staging` is browser-only and has no single field — it is the pair, and the
// inbound slot is where the operator looks first.
const CLAIM_ERROR_KEY_ALIASES = {
    coreNodeName:         'core_node_name',
    payloadCode:          'payload_code',
    swapMode:             'swap_mode',
    staging:              'inbound_staging',
    inboundStaging:       'inbound_staging',
    outboundStaging:      'outbound_staging',
    inboundSource:        'inbound_source',
    outboundDestination:  'outbound_destination',
    pairedCoreNode:       'paired_core_node',
    secondPairedCoreNode: 'second_paired_core_node',
    keyRoute:             'key_route',
    keyTask:              'key_task',
};

function normalizeClaimErrorField(field) {
    if (!field) return '';
    return CLAIM_ERROR_KEY_ALIASES[field] || field;
}

// clearClaimFieldErrors wipes every slot and border. Called before each render
// and on any edit, so a message never outlives the value that caused it.
function clearClaimFieldErrors() {
    Object.keys(CLAIM_ERROR_SLOTS).forEach(function(key) {
        var spec = CLAIM_ERROR_SLOTS[key];
        [spec.slot, spec.notice].forEach(function(id) {
            if (!id) return;
            var el = document.getElementById(id);
            if (!el) return;
            el.textContent = '';
            el.hidden = true;
            el.style.display = 'none';
        });
        if (spec.input) {
            var input = document.getElementById(spec.input);
            if (input && input.classList) input.classList.remove('form-input--error');
        }
    });
    var form = document.getElementById('claims-err-form');
    if (form) { form.textContent = ''; form.hidden = true; form.style.display = 'none'; }
}

// renderClaimFieldErrors puts each finding on its own field.
//
// Findings are {field, message|msg, severity}. Severity "warning" renders in
// the notice slot and does NOT mark the input — a warning did not refuse the
// save, and colouring it like a refusal is how a refusal colour stops meaning
// anything. Everything else is an error.
//
// A finding whose field has no slot still renders, at the form level. Silently
// dropping it would be the toast problem again with extra steps: the operator
// would see a refusal and no reason at all.
function renderClaimFieldErrors(findings) {
    clearClaimFieldErrors();
    var orphans = [];
    (findings || []).forEach(function(f) {
        var key = normalizeClaimErrorField(f.field);
        var text = f.message || f.msg || '';
        var isWarning = f.severity === 'warning';
        var spec = CLAIM_ERROR_SLOTS[key];
        var slotID = spec && (isWarning ? (spec.notice || spec.slot) : spec.slot);
        var el = slotID ? document.getElementById(slotID) : null;
        if (!el) { orphans.push(text); return; }
        el.textContent = el.textContent ? el.textContent + ' ' + text : text;
        el.hidden = false;
        el.style.display = '';
        if (!isWarning && spec.input) {
            var input = document.getElementById(spec.input);
            if (input && input.classList) input.classList.add('form-input--error');
        }
    });
    if (orphans.length > 0) {
        var form = document.getElementById('claims-err-form');
        if (form) {
            form.textContent = orphans.join(' ');
            form.hidden = false;
            form.style.display = '';
        }
    }
}

// CLAIM_INPUT_TO_ERROR_FIELD is CLAIM_ERROR_SLOTS read the other way: which
// finding does editing THIS input answer. Built once from the table so the two
// directions cannot disagree.
const CLAIM_INPUT_TO_ERROR_FIELD = (function() {
    var out = {};
    Object.keys(CLAIM_ERROR_SLOTS).forEach(function(key) {
        var input = CLAIM_ERROR_SLOTS[key].input;
        if (input) out[input] = key;
    });
    return out;
})();

// clearClaimFieldError drops one field's message and border.
//
// A message that outlives the value it was about is worse than no message: the
// operator fixes the field, the red stays, and they stop believing the red.
function clearClaimFieldError(field) {
    var spec = CLAIM_ERROR_SLOTS[field];
    if (!spec) return;
    [spec.slot, spec.notice].forEach(function(id) {
        if (!id) return;
        var el = document.getElementById(id);
        if (!el) return;
        el.textContent = '';
        el.hidden = true;
        el.style.display = 'none';
    });
    if (spec.input) {
        var input = document.getElementById(spec.input);
        if (input && input.classList) input.classList.remove('form-input--error');
    }
}

// ensureClaimErrorDelegation clears a field's error the moment it is edited.
// One delegated listener on the modal rather than a handler per input, and
// idempotent so re-opening the modal does not stack listeners.
function ensureClaimErrorDelegation() {
    var modal = document.getElementById('claim-modal');
    if (!modal || modal.dataset.errDelegated === '1') return;
    modal.dataset.errDelegated = '1';
    var onEdit = function(e) {
        var id = e.target && e.target.id;
        var field = id && CLAIM_INPUT_TO_ERROR_FIELD[id];
        if (field) clearClaimFieldError(field);
    };
    modal.addEventListener('change', onEdit);
    modal.addEventListener('input', onEdit);
}

// ── Node pickers ────────────────────────────────────────────────────
//
// All six geometry pickers were unfiltered dumps of every core node, so "Back
// Press Node" offered supermarkets and groups and "Inbound Source" offered
// presses.
//
// TWO MECHANISMS, AND THE DIFFERENCE MATTERS.
//
//   EXCLUDE only what the runtime CANNOT do. A robot cannot index a bin into a
//   group — the press-index builder emits pickup/dropoff at concrete nodes — so
//   a group in a paired-position picker is not an unlikely choice, it is an
//   impossible one.
//
//   RANK what is merely unlikely, into labelled optgroups. The signals
//   available here (node class, and whether the name is a line position on this
//   process) do not separate a press from a supermarket: both are plain
//   concrete nodes. And the one case where the distinction looks obvious is a
//   trap — a dedicated loader's home position is BOTH a line position AND a
//   legitimate InboundSource (Core's source_finder tier 2, sourceFromDedicated-
//   Loader). Filtering sources by "not a line position" would hide a supported,
//   tested configuration. So sources rank groups first and hide nothing.
//
// The escape hatch exists because a plant's naming and topology can defeat any
// heuristic: "Show every node" reveals the excluded entries in all six pickers.
// Not persisted — it is a per-session look, not a setting.
const NODE_PICKER_KIND = {
    'claims-add-inbound':              'staging',
    'claims-add-outbound':             'staging',
    'claims-add-inbound-source':       'endpoint',
    'claims-add-outbound-destination': 'endpoint',
    'claims-add-evac-destination':     'endpoint',
    'claims-add-paired-node':          'position',
    'claims-add-second-paired-node':   'position',
};

// The blank first option each picker keeps, by element id.
const NODE_PICKER_PLACEHOLDER = {
    'claims-add-inbound':              '-- None --',
    'claims-add-outbound':             '-- None --',
    'claims-add-inbound-source':       '-- None --',
    'claims-add-outbound-destination': '-- None --',
    'claims-add-evac-destination':     '-- Use Outbound Destination --',
    'claims-add-paired-node':          '-- None (no A/B cycling) --',
    'claims-add-second-paired-node':   '-- None (2-position layout) --',
};

var _showAllNodes = false;

function nodeCatalog() {
    var cat = (typeof window !== 'undefined' && window.coreNodeCatalog) || {};
    var out = [];
    Object.keys(cat).forEach(function(name) {
        var info = cat[name] || {};
        out.push({ name: name, type: info.node_type || '' });
    });
    out.sort(function(a, b) { return a.name < b.name ? -1 : (a.name > b.name ? 1 : 0); });
    return out;
}

function isLinePosition(name) {
    var names = (typeof window !== 'undefined' && window.processNodeNames) || [];
    return names.indexOf(name) >= 0;
}

// nodeAllowedForPicker: is this node a POSSIBLE answer for this field.
//
// Every rule here removes something the runtime cannot use, never something it
// merely usually does not. `self` is the claim's own core node and its paired
// positions — a cell cannot stage at, source from or deliver to itself, and a
// press position cannot be paired with itself.
function nodeAllowedForPicker(kind, node, self) {
    if (self && self.indexOf(node.name) >= 0) return false;
    switch (kind) {
        case 'position':
            // The builder emits pickup/dropoff at a concrete node; a group has
            // no coordinates to drive to.
            return node.type !== 'NGRP';
        case 'staging':
            // Staging is a place a robot parks a bin. A group is not a place.
            return node.type !== 'NGRP';
        case 'endpoint':
            // Sources and destinations accept a group OR a concrete node —
            // Core resolves either. Nothing is excluded but self.
            return true;
        default:
            return true;
    }
}

// nodePickerGroupLabel: which optgroup a node sorts into, or '' for a flat
// list. Ranking only; every entry is still selectable.
function nodePickerGroupLabel(kind, node) {
    if (kind === 'position') {
        return isLinePosition(node.name) ? 'This process' : 'Other nodes';
    }
    if (kind === 'endpoint') {
        return node.type === 'NGRP' ? 'Groups' : 'Nodes';
    }
    return '';
}

const NODE_PICKER_GROUP_ORDER = ['This process', 'Groups', 'Other nodes', 'Nodes'];

// buildNodePickers rewrites the six geometry selects from the catalog.
//
// THE CURRENT VALUE IS ALWAYS PRESENT, even when the filter would exclude it.
// A picker that drops the value it is displaying is the round-2 unit-2 bug
// wearing a different hat: the operator opens a claim, the select silently
// falls back to blank, and the next save writes the blank. An out-of-filter
// value is kept and marked so the operator can see WHY it looks odd.
function buildNodePickers(state) {
    var catalog = nodeCatalog();
    Object.keys(NODE_PICKER_KIND).forEach(function(selID) {
        var sel = document.getElementById(selID);
        if (!sel) return;
        var kind = NODE_PICKER_KIND[selID];
        var current = sel.value || '';
        var self = claimSelfNodes(selID, state);

        var buckets = {};
        var order = [];
        var currentIncluded = false;
        catalog.forEach(function(node) {
            if (!_showAllNodes && !nodeAllowedForPicker(kind, node, self)) return;
            var g = _showAllNodes ? '' : nodePickerGroupLabel(kind, node);
            if (!buckets[g]) { buckets[g] = []; order.push(g); }
            buckets[g].push(node);
            if (node.name === current) currentIncluded = true;
        });

        var html = '<option value="">' + escapeHtml(NODE_PICKER_PLACEHOLDER[selID] || '-- None --') + '</option>';
        if (current && !currentIncluded) {
            html += '<option value="' + escapeHtml(current) + '" selected>' +
                escapeHtml(current) + ' (not offered for this field)</option>';
        }
        order.sort(function(a, b) {
            var ia = NODE_PICKER_GROUP_ORDER.indexOf(a);
            var ib = NODE_PICKER_GROUP_ORDER.indexOf(b);
            return (ia < 0 ? 99 : ia) - (ib < 0 ? 99 : ib);
        });
        order.forEach(function(g) {
            if (g) html += '<optgroup label="' + escapeHtml(g) + '">';
            buckets[g].forEach(function(node) {
                var label = node.name + (node.type === 'NGRP' ? ' (group)' : '');
                html += '<option value="' + escapeHtml(node.name) + '">' + escapeHtml(label) + '</option>';
            });
            if (g) html += '</optgroup>';
        });
        sel.innerHTML = html;
        sel.value = current;
    });
}

// claimSelfNodes: the node names this picker must not offer, because the claim
// already uses them somewhere the field cannot repeat.
function claimSelfNodes(selID, state) {
    if (!state) return [];
    var kind = NODE_PICKER_KIND[selID];
    var out = [];
    if (state.coreNodeName) out.push(state.coreNodeName);
    if (kind === 'position') {
        // The three press positions must be distinct — the same rule
        // domain.ValidateNodeClaim enforces server-side.
        if (selID !== 'claims-add-paired-node' && state.pairedCoreNode) out.push(state.pairedCoreNode);
        if (selID !== 'claims-add-second-paired-node' && state.secondPairedCoreNode) out.push(state.secondPairedCoreNode);
    }
    return out;
}

// toggleShowAllNodes is the escape hatch. Deliberately NOT persisted: a plant
// whose naming defeats the heuristic needs a look, not a permanent setting that
// quietly turns the filtering off for everyone who follows.
function resetShowAllNodes() {
    _showAllNodes = false;
    var cb = document.getElementById('claims-show-all-nodes');
    if (cb) cb.checked = false;
}

function toggleShowAllNodes() {
    var cb = document.getElementById('claims-show-all-nodes');
    _showAllNodes = !!(cb && cb.checked);
    renderClaimForm();
}

// renderCollapseHints keeps a collapsed group honest.
//
// Collapsing may hide DETAIL; it must not hide STATE. A card whose summary
// reads "Changeover" tells the operator nothing about whether this claim
// evacuates, so they open every card every time and the collapse has bought
// nothing. The hint carries the current value, and a group holding a
// non-default value opens itself.
function renderCollapseHints(state) {
    var setHint = function(cardID, hintID, text, isDefault) {
        var hint = document.getElementById(hintID);
        if (hint) hint.textContent = text;
        var card = document.getElementById(cardID);
        // Only force OPEN. Forcing closed would slam the card shut under an
        // operator who had just opened it to look.
        if (card && !isDefault) card.open = true;
    };
    var markedNodes = state.changeoverEvacNodes || [];
    var coHint = state.evacuateOnChangeover ? 'evacuate: on' : 'evacuate: off';
    if (markedNodes.length > 0) coHint += ' · cleared for setup: ' + markedNodes.join(', ');
    setHint('claims-changeover-fieldset', 'claims-changeover-hint', coHint,
        !state.evacuateOnChangeover && markedNodes.length === 0);
    setHint('claims-auto-request-fieldset', 'claims-auto-request-hint',
        state.autoRequestPayload ? ('payload: ' + state.autoRequestPayload)
                                 : (state.autoConfirm ? 'auto-confirm: on' : 'disabled'),
        !state.autoRequestPayload && !state.autoConfirm);
}

// readEvacNodes snapshots which of this claim's nodes are marked for clearance.
//
// IT DOES NOT FILTER BY VISIBILITY, and that is deliberate. Filtering here
// looked right and made the drop note impossible: renderClaimForm hides the
// clearance rows the moment the mode changes, so by the time anything asked "what
// will this mode discard" the answer was already gone and the operator was
// told nothing. Same shape as the value-eating bug of round 2 — a view
// decision reaching back into the model.
//
// A position the mode or the layout cannot use is dropped at SAVE by
// claimForbiddenFields, which is where every other such value is dropped, and
// named in the note first.
// ── KEY ROUTE LIST CONTROL ───────────────────────────────────────────────
//
// A repeatable list rather than the single <select> every other node field
// uses, because a key route is ORDERED and arbitrary-length. DOM order IS the
// route order — there is no separate index to keep in step, which is the way
// this kind of control usually rots.
//
// Each row is a node picker rather than a text box: the points must resolve
// against Core's node list or the robot's job dies on issue, and a picker is
// the difference between finding that out here and finding it out at 2am. The
// server checks anyway (ValidateNodeClaim) — a picker is a convenience, not
// the guard.
function keyRouteList() {
    return document.getElementById('claims-key-route-list');
}

function keyRouteOptionsHTML(selected) {
    var html = '<option value="">-- Choose a point --</option>';
    var found = false;
    nodeCatalog().forEach(function(node) {
        var sel = node.name === selected ? ' selected' : '';
        if (sel) found = true;
        html += '<option value="' + escapeHtml(node.name) + '"' + sel + '>' +
            escapeHtml(node.name) + (node.type === 'NGRP' ? ' (group)' : '') + '</option>';
    });
    // A stored point Core no longer offers stays visible and stays SELECTED.
    // Dropping it would silently rewrite a saved route on the next save of an
    // unrelated field — the save-stomp shape round 2 spent a unit on.
    if (selected && !found) {
        html += '<option value="' + escapeHtml(selected) + '" selected>' +
            escapeHtml(selected) + ' (not offered by Core)</option>';
    }
    return html;
}

function keyRouteRowHTML(point) {
    return '<div class="key-route-row">' +
        '<select class="form-input claims-key-route-point">' + keyRouteOptionsHTML(point) + '</select>' +
        '<button type="button" class="btn btn-sm btn-danger" data-action="removeKeyRoutePoint">&times;</button>' +
        '</div>';
}

function writeKeyRoute(points) {
    var list = keyRouteList();
    if (!list) return;
    list.innerHTML = (points || []).map(keyRouteRowHTML).join('');
}

function readKeyRoute() {
    var out = [];
    document.querySelectorAll('.claims-key-route-point').forEach(function(sel) {
        // Blank rows are dropped here rather than sent and refused. An empty
        // row is an operator mid-edit, not a configuration error.
        if (sel.value) out.push(sel.value);
    });
    return out;
}

function addKeyRoutePoint() {
    var list = keyRouteList();
    if (!list) return;
    list.insertAdjacentHTML('beforeend', keyRouteRowHTML(''));
}

// delegateActions calls a pure verb as fn(el, evt) — the clicked element is
// the first argument, not an encoded arg.
function removeKeyRoutePoint(el) {
    var row = el && el.closest && el.closest('.key-route-row');
    if (row) row.remove();
}

// THE CHECKBOXES CARRY NODE NAMES. Each row is a slot in the claim's layout —
// front / back / third — but what it stores is the node that slot currently
// holds, because clearing a node is a node operation and the marks name nodes.
// The slot is presentation; data-slot says which layout field fills the value.
// claimLoadedEvacNodes remembers the marks the claim was LOADED with, because
// the form cannot always represent them.
//
// A mark naming a node this claim no longer holds — the third node was unset,
// or a slot re-pointed — has no checkbox to live in: the row is hidden and its
// value is empty. Reading only the boxes would make that mark vanish from the
// state, and the drop note would have nothing to name. That is the same
// value-eating shape the visibility filter caused in round 3, arriving by a
// different route, so the answer is the same: keep it in the state, name it,
// and let the save drop it deliberately.
var claimLoadedEvacNodes = [];

function readEvacNodes() {
    var out = [];
    var representable = {};
    document.querySelectorAll('.claims-evac-position').forEach(function(cb) {
        if (cb.value) representable[cb.value] = true;
        if (cb.checked && cb.value) out.push(cb.value);
    });
    // Marks no box can hold ride along so the drop note can name them.
    claimLoadedEvacNodes.forEach(function(n) {
        if (!representable[n] && out.indexOf(n) < 0) out.push(n);
    });
    return out;
}

function writeEvacNodes(nodes) {
    var want = {};
    (nodes || []).forEach(function(n) { want[n] = true; });
    document.querySelectorAll('.claims-evac-position').forEach(function(cb) {
        cb.checked = !!(cb.value && want[cb.value]);
    });
}

// renderEvacNodeLabels fills each row's VALUE with the node that slot holds and
// shows it beside the label. "Back position" is the same words on every press on
// the line; "Back position (PLN_002_B)" is the one the operator is standing at —
// and now it is also what gets saved.
//
// A slot with no node gets an empty value, so it can never contribute a mark;
// the row is hidden for that case anyway (claimFieldVisibility).
function renderEvacNodeLabels(state) {
    var pairs = [
        ['front', state.coreNodeName],
        ['paired', state.pairedCoreNode],
        ['second', state.secondPairedCoreNode],
    ];
    pairs.forEach(function(p) {
        var el = document.getElementById('claims-position-' + p[0] + '-node');
        if (el) el.textContent = p[1] ? '(' + p[1] + ')' : '(not set)';
        document.querySelectorAll('.claims-evac-position').forEach(function(cb) {
            if (cb.getAttribute('data-slot') !== p[0]) return;
            var was = cb.checked && cb.value;
            cb.value = p[1] || '';
            // Re-pairing a slot must not silently move a mark to the new node.
            if (was && was !== cb.value) cb.checked = false;
        });
    });
}

// claimForbiddenFields answers: which populated values does THIS mode not use.
//
// Derived from the same (role, swap) facts as claimFieldVisibility, so the
// answer cannot disagree with what the form shows. Only POPULATED fields are
// reported — a mode that does not use a field the operator never set has
// nothing to say about it.
//
// This is the whole set that renderClaimForm used to blank on sight. Moving it
// here changes WHEN a value dies, not WHICH: a straight-through save drops the
// same fields it always did, and a mode toggled away and back no longer loses
// anything, because the question is asked once, at save, about the mode the
// operator actually chose.
function claimForbiddenFields(role, swap, state) {
    var isManual = swap === 'manual_swap';
    var isPressIndex = swap === 'two_robot_press_index';
    var usesStaging = swap === 'single_robot' || swap === 'two_robot';
    var showPair = swap === 'sequential' || isPressIndex;
    var out = [];
    var forbid = function(key, label) {
        var v = state[key];
        if (v === '' || v === false || v === undefined || v === null) return;
        out.push({ key: key, label: label });
    };

    if (!showPair) {
        forbid('pairedCoreNode', 'Paired Node');
    }
    var markedNodes = state.changeoverEvacNodes || [];
    if (isManual && (state.keyRoute || []).length > 0) {
        out.push({ key: 'keyRoute', label: 'Key route', value: [] });
    }
    if (isManual && state.keyTask) {
        out.push({ key: 'keyTask', label: 'Key task', value: '' });
    }
    if (!isPressIndex && state.indexRobotSupplies) {
        out.push({ key: 'indexRobotSupplies', label: 'Index robot fetches the replacement', value: false });
    }
    if (!isPressIndex) {
        // Per-node clearance needs a claim that names several nodes; a
        // single-node claim answers the same question with Evacuate on changeover.
        if (markedNodes.length > 0) {
            out.push({ key: 'changeoverEvacNodes', label: 'Per-node changeover clearance', value: [] });
        }
    } else {
        // A PARTIAL drop: a mark naming a node this claim no longer holds — the
        // third node was unset, or a slot was re-pointed. The rest of the
        // selection is fine and must survive; dropping the whole set would take
        // the good marks with it.
        var held = [state.coreNodeName, state.pairedCoreNode, state.secondPairedCoreNode]
            .filter(function(n) { return !!n; });
        var stale = markedNodes.filter(function(n) { return held.indexOf(n) < 0; });
        if (stale.length > 0) {
            out.push({
                key: 'changeoverEvacNodes',
                label: 'Nodes marked for clearance that this claim no longer holds (' + stale.join(', ') + ')',
                value: markedNodes.filter(function(n) { return held.indexOf(n) >= 0; }),
            });
        }
    }
    if (!(showPair && isPressIndex)) {
        forbid('secondPairedCoreNode', 'Third Press Position');
        forbid('reuseCompatibleBins', 'Reuse compatible bins');
    }
    if (!(isManual && role === 'consume')) {
        forbid('autoPush', 'Auto-push full bins');
    }
    if (!usesStaging && !isManual) {
        forbid('inboundStaging', 'Inbound Staging');
        forbid('outboundStaging', 'Outbound Staging');
    }
    // two_robot uses inbound staging only; robot B takes the old bin straight
    // out, so an outbound staging node is ignored by the builder.
    if (swap === 'two_robot') {
        forbid('outboundStaging', 'Outbound Staging');
    }
    return out;
}

// renderModeDropNote tells the operator, while they are still looking at the
// form, what the selected mode will discard.
//
// BEFORE, NOT AFTER. The old behaviour blanked the field the instant its
// fieldset hid, so the operator's only signal was noticing later that a value
// had gone. Naming them up front makes the drop a decision rather than a
// discovery.
// setHidden sets both halves of hiding an element: the `hidden` attribute for
// meaning, and `.is-hidden` for effect. See that class in shared/components.css
// for why the attribute is not enough by itself.
function setHidden(el, hide) {
    if (!el) return;
    el.hidden = !!hide;
    el.classList.toggle('is-hidden', !!hide);
}

function renderModeDropNote(role, swap, state) {
    var el = document.getElementById('claims-mode-drop-note');
    if (!el) return;
    var dropped = claimForbiddenFields(role, swap, state);
    if (dropped.length === 0) {
        el.textContent = '';
        setHidden(el, true);
        return;
    }
    var names = dropped.map(function(d) { return d.label; }).join(', ');
    el.textContent = (SWAP_MODE_LABELS[swap] || swap) +
        ' does not use: ' + names + ' — cleared when you save.';
    setHidden(el, false);
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

    // Apply the visibility map. The `hidden` ATTRIBUTE carries the meaning —
    // it is what assistive technology and find-in-page read — and `.is-hidden`
    // does the hiding, because the attribute alone cannot: `[hidden]` has
    // class-level specificity, so `.check-row { display: flex }` beats it and
    // a row hidden only by the attribute stays on screen.
    //
    // This replaces a paired inline `style.display` write plus two special
    // cases that re-set `display: flex` on the rows the attribute could not
    // hide. Nothing here needs to know what an element's display value is
    // supposed to be when it comes back, which is what made those cases
    // necessary and what made them easy to forget for a third row.
    for (var id in visibility) {
        var el = document.getElementById(id);
        if (el) {
            el.hidden = !visibility[id];
            el.classList.toggle('is-hidden', !visibility[id]);
        }
    }

    // Disable outbound staging for two_robot (data: ignored anyway).
    var outboundSel = document.getElementById('claims-add-outbound');
    if (outboundSel) {
        if (isTwoRobot) {
            // Disabled, NOT blanked: two_robot ignores outbound staging, and
            // the value is dropped at save. Blanking it here would lose it on
            // a mode toggle the operator was only browsing with.
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
        }
    }

    // NOTHING IS CLEARED HERE, AND THAT IS THE POINT.
    //
    // This function used to blank paired_core_node, second_paired_core_node,
    // reuse_compatible_bins, auto_push and both staging fields whenever their
    // fieldset went out of view. Rendering is not editing: toggling the swap
    // mode to look at another mode's fields, then toggling back, silently
    // destroyed a press-index pairing the operator never touched. The state is
    // the model; visibility is a view of it.
    //
    // Values a mode genuinely cannot use are dropped at SAVE, by
    // claimForbiddenFields below, and the operator is told which — see
    // renderModeDropNote.

    // Manual swap on a fresh open clears staging fields (no concept of
    // staging there). When editing, leave alone so the operator can
    // see prior values before manual_swap was selected.
    var isEditing = !!document.getElementById('claims-edit-id').value;
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

    // Rebuild the geometry pickers for the current claim before reading state
    // back out, so the note below sees the values the pickers actually hold.
    buildNodePickers(readClaimStateFromForm());

    // Validation warning for missing required staging.
    var state = readClaimStateFromForm();
    var warn = document.getElementById('claims-staging-warning');
    if (warn) {
        var missing = (swap === 'single_robot' && (!state.inboundStaging || !state.outboundStaging))
            || (swap === 'two_robot' && !state.inboundStaging);
        warn.style.display = missing ? '' : 'none';
    }
    renderModeDropNote(role, swap, state);
    renderEvacNodeLabels(state);
    renderCollapseHints(state);
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
        // 0 means "no opinion": the store gives a new claim the next free
        // board slot. Not a real position, and not sent as one.
        sequence: 0,
        autoReorder: false,
        keepStaged: false,
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
        changeoverEvacNodes: [],
        changeoverEvacDestination: '',
        indexRobotSupplies: false,
        keyRoute: [],
        keyTask: '',
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
    ensureClaimErrorDelegation();
    clearClaimFieldErrors();
    resetShowAllNodes();
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
        swapMode: claim.swap_mode || '',
        payloadCode: claim.payload_code || '',
        uopCapacity: claim.uop_capacity || 0,
        reorderPoint: claim.reorder_point || 0,
        sequence: claim.sequence || 0,
        autoReorder: !!claim.auto_reorder,
        keepStaged: !!claim.keep_staged,
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
        indexRobotSupplies: !!claim.index_robot_supplies,
        keyRoute: (claim.key_route || []).slice(),
        keyTask: claim.key_task || '',
        changeoverEvacNodes: claim.changeover_evac_nodes || [],
        changeoverEvacDestination: claim.changeover_evac_destination || '',
        changeoverCarryoverDisposition: claim.changeover_carryover_disposition || 'replace',
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
    ensureClaimErrorDelegation();
    clearClaimFieldErrors();
    resetShowAllNodes();
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
        // EVERY error, each on its own field. This used to surface
        // validation.errors[0] as a toast and throw the rest away, which told
        // an operator with three problems about one of them and did not say
        // which input it meant. The toast stays as the summary line — the
        // fields carry the detail.
        renderClaimFieldErrors(validation.errors);
        toast(validation.errors.length === 1
            ? validation.errors[0].msg
            : validation.errors.length + ' fields need attention', 'warning');
        return;
    }
    clearClaimFieldErrors();

    // Drop what this mode cannot use, ONCE, here — not every time a fieldset
    // went out of view. The operator has already been shown the list by
    // renderModeDropNote; the toast below is the confirmation that it happened.
    var dropped = claimForbiddenFields(state.role, state.swapMode, state);
    dropped.forEach(function(d) {
        // An entry may name the value to keep — a PARTIAL drop, where only
        // part of a set is unusable and blanking the whole thing would take
        // good configuration with it.
        if (Object.prototype.hasOwnProperty.call(d, 'value')) {
            state[d.key] = d.value;
        } else if (Array.isArray(state[d.key])) {
            state[d.key] = [];
        } else {
            state[d.key] = (typeof state[d.key] === 'boolean') ? false : '';
        }
    });

    // manual_swap claims carry no edge-side payload: Core owns the loader's
    // payload set (loader board), so payload_code is blank and the operator
    // switches among the aggregate's payloads at load time.
    var primaryPayload = state.swapMode === 'manual_swap' ? '' : state.payloadCode;

    // A FORM THAT OWNS A FIELD SENDS IT. Round 1 made these pointers on
    // NodeClaimInput so a writer could decline to speak; this editor now has
    // controls for three of them, so it speaks. reorder_point_source stays
    // absent — nothing here sets provenance.
    //
    // sequence 0 is the exception, and stays absent: it means "no opinion",
    // and the store reads an absent sequence as "give a new claim the next
    // free board slot". Sending a literal 0 would claim position zero.
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
        inbound_staging: state.inboundStaging,
        outbound_staging: state.outboundStaging,
        inbound_source: state.inboundSource,
        outbound_destination: state.outboundDestination,
        auto_request_payload: state.autoRequestPayload,
        evacuate_on_changeover: state.evacuateOnChangeover,
        reuse_compatible_bins: state.reuseCompatibleBins,
        auto_push: state.autoPush,
        paired_core_node: state.pairedCoreNode,
        second_paired_core_node: state.secondPairedCoreNode,
        index_robot_supplies: state.indexRobotSupplies,
        key_route: state.keyRoute,
        key_task: state.keyTask,
        changeover_evac_nodes: state.changeoverEvacNodes,
        changeover_evac_destination: state.changeoverEvacDestination,
        changeover_carryover_disposition: state.changeoverCarryoverDisposition,
        auto_confirm: state.autoConfirm,
        auto_reorder: state.autoReorder,
        keep_staged: state.keepStaged,
    };
    if (state.sequence > 0) claimBody.sequence = state.sequence;

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

    // postDetailed, not post: a refusal from domain.ValidateNodeClaim carries
    // field_errors, and a successful save can carry warnings (the
    // node-membership notice). api.post throws away both. The server's
    // findings render through the SAME path as the browser's, so the operator
    // sees one thing whether JS or Go caught it.
    var warnings = [];
    for (var i = 0; i < nodeNames.length; i++) {
        claimBody.core_node_name = nodeNames[i];
        var res = await api.postDetailed('/api/style-node-claims', claimBody);
        if (!res.ok) {
            if (res.fieldErrors.length > 0) {
                renderClaimFieldErrors(res.fieldErrors);
                toast(res.fieldErrors.length === 1
                    ? (res.fieldErrors[0].message || res.error)
                    : res.fieldErrors.length + ' fields need attention', 'warning');
            } else {
                toast('Error: ' + res.error, 'error');
            }
            return; // modal stays open on the offending values
        }
        warnings = warnings.concat(res.warnings || []);
    }
    closeClaimModal();
    await loadClaims(_claimsStyleID);
    if (nodeNames.length > 1) toast('Created ' + nodeNames.length + ' claims', 'success');
    if (dropped.length > 0) {
        toast('Cleared (not used by ' + (SWAP_MODE_LABELS[state.swapMode] || state.swapMode) + '): ' +
            dropped.map(function(d) { return d.label; }).join(', '), 'warning');
    }
    // Advisory, and the save already happened. Surfaced after the reload so it
    // is not mistaken for a refusal.
    warnings.forEach(function(w) { toast(w.message || String(w), 'warning'); });
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
        span.textContent = p.code + (p.name ? ' — ' + p.name : '') + (p.uop_capacity ? ' (' + p.uop_capacity + ' UoP)' : '');
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

// createLoaderBoard makes the operator screen for a Core loader that has none on
// this edge, and binds the loader's windows to it.
//
// processID comes from the row when exactly one process already holds the
// loader's windows as process_nodes; it is 0 when none does or several do, and
// then the screen lands on the process being viewed. Either way the confirm
// NAMES the target first — Core sends every loader to every edge, so which edge
// and which process claims one is a decision that has to stay with a person.
async function createLoaderBoard(loaderKey, processID) {
    const target = Number(processID) > 0 ? Number(processID) : Number(activeProcessID);
    if (!target) {
        toast('Open a process first — a screen belongs to one process', 'error');
        return;
    }
    const where = Number(processID) > 0 ? 'the process that already holds its windows' : 'this process';
    if (!await confirm('Create the operator screen for ' + loaderKey + ' on ' + where + ', and bind its windows to it?')) return;
    try {
        await api.post('/api/loader-boards', { loader_key: loaderKey, process_id: target });
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
    addKeyRoutePoint,
    removeKeyRoutePoint,
    keyRouteRowHTML,
    readKeyRoute,
    writeKeyRoute,
    autoFillClaimsCapacity,
    buildAllowedPayloadPicker,
    claimFieldVisibility,
    cloneStyle,
    closeClaimModal,
    closeCloneStyleModal,
    closeGenerateModal,
    closeGroupModal,
    closeProcessModal,
    closeStationModal,
    closeStyleModal,
    createProcess,
    defaultClaimState,
    deleteGroup,
    deleteProcess,
    deleteStation,
    deleteStyle,
    editClaim,
    editGroup,
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
    createLoaderBoard,
    onClaimsStyleChanged,
    onCompareFieldChanged,
    onCompareViewChanged,
    onGenerateBaseChanged,
    openClaimModal,
    openCloneStyleModal,
    openCreateGroupModal,
    openCreateProcessModal,
    openCreateStationModal,
    openCreateStyleModal,
    openGenerateModal,
    readClaimStateFromForm,
    removeClaim,
    removeGenerateRow,
    buildNodePickers,
    claimForbiddenFields,
    clearClaimFieldError,
    clearClaimFieldErrors,
    renderClaimFieldErrors,
    renderClaimForm,
    renderCollapseHints,
    renderClaimRow,
    resetNodePicker,
    resetProcessForm,
    resetStationForm,
    resetStyleForm,
    saveClaim,
    saveGroup,
    saveProcess,
    saveStation,
    saveStyle,
    setActiveStyle,
    showProcessTab,
    syncPayloadCatalog,
    toggleClaimsAddPayload,
    toggleShowAllNodes,
    updateAutoRequestDropdown,
    validateClaimStaging,
    validateClaimState,
    writeClaimStateToForm
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
