const activeProcessID = parseInt(document.getElementById('page-data').dataset.activeProcessId || '0', 10);
const claimedByStation = window.claimedByStation || {};

function processURL() {
    return '/processes?process=' + activeProcessID;
}

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
    ShingoEdge.showModal('process-modal');
}

function closeProcessModal() {
    ShingoEdge.hideModal('process-modal');
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
        ShingoEdge.toast('Enter a process name', 'warning');
        return;
    }
    const counterPLC = document.getElementById('new-process-counter-plc').value;
    const counterTag = document.getElementById('new-process-counter-tag').value.trim();
    try {
        const res = await ShingoEdge.api.post('/api/processes', {
            name: name,
            description: document.getElementById('new-process-description').value.trim(),
            production_state: 'active_production',
            counter_plc_name: counterPLC,
            counter_tag_name: counterTag,
            counter_enabled: !!(counterPLC && counterTag)
        });
        // Auto-create a Default style and set it active
        try {
            const style = await ShingoEdge.api.post('/api/styles', {
                name: 'Default',
                description: 'Default style',
                process_id: res.id
            });
            await ShingoEdge.api.put('/api/processes/' + res.id + '/active-style', {
                style_id: style.id
            });
        } catch (_) { /* non-fatal */ }
        window.location = '/processes?process=' + res.id;
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function saveProcess() {
    try {
        await ShingoEdge.api.put('/api/processes/' + activeProcessID, {
            name: document.getElementById('process-name').value.trim(),
            description: document.getElementById('process-description').value.trim(),
            production_state: document.getElementById('process-production-state').value,
            counter_plc_name: document.getElementById('counter-plc') ? document.getElementById('counter-plc').value : '',
            counter_tag_name: document.getElementById('counter-tag') ? document.getElementById('counter-tag').value.trim() : '',
            counter_enabled: document.getElementById('counter-enabled') ? document.getElementById('counter-enabled').checked : false
        });
        ShingoEdge.toast('Process saved', 'success');
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function deleteProcess(id) {
    if (!await ShingoEdge.confirm('Delete this process and all of its station configuration?')) return;
    try {
        await ShingoEdge.api.del('/api/processes/' + id);
        window.location = '/processes';
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}


function resetStyleForm() {
    document.getElementById('style-id').value = '';
    document.getElementById('style-name').value = '';
    document.getElementById('style-description').value = '';
}

function openCreateStyleModal() {
    resetStyleForm();
    document.getElementById('style-modal-title').textContent = 'Add Style';
    ShingoEdge.showModal('style-modal');
}

function closeStyleModal() {
    ShingoEdge.hideModal('style-modal');
    resetStyleForm();
}

function editStyle(style) {
    resetStyleForm();
    document.getElementById('style-id').value = style.id;
    document.getElementById('style-name').value = style.name || '';
    document.getElementById('style-description').value = style.description || '';
    document.getElementById('style-modal-title').textContent = 'Edit Style';
    ShingoEdge.showModal('style-modal');
}

// --- Node Claims tab ---

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
        _payloadCatalog = await ShingoEdge.api.get('/api/payload-catalog');
        if (!Array.isArray(_payloadCatalog)) _payloadCatalog = [];
    } catch (_) { _payloadCatalog = []; }
    var sel = document.getElementById('claims-add-payload');
    if (!sel) return;
    sel.innerHTML = '<option value="">-- Select --</option><option value="__empty__">Empty (clear node)</option>';
    _payloadCatalog.forEach(function(p) {
        var opt = document.createElement('option');
        opt.value = p.code;
        opt.textContent = p.code + (p.name ? ' \u2014 ' + p.name : '') + (p.uop_capacity ? ' (' + p.uop_capacity + ' UOP)' : '');
        opt.dataset.capacity = p.uop_capacity || 0;
        sel.appendChild(opt);
    });
}

async function loadClaims(styleID) {
    var list = document.getElementById('claims-list');
    list.innerHTML = '';
    if (!styleID) return;
    try {
        var claims = await ShingoEdge.api.get('/api/styles/' + styleID + '/node-claims');
        _currentClaims = Array.isArray(claims) ? claims : [];
        if (!Array.isArray(claims) || claims.length === 0) {
            list.innerHTML = '<div style="color:var(--text-muted);font-style:italic;padding:0.5rem 0">No node claims for this style. Use the form below to add claims.</div>';
            return;
        }
        var table = document.createElement('table');
        table.className = 'table';
        table.innerHTML = '<thead><tr><th>Core Node</th><th>Role</th><th>Swap</th><th>Wants</th><th>Inbound</th><th>Outbound</th><th style="width:1%"></th></tr></thead>';
        var tbody = document.createElement('tbody');
        claims.forEach(function(c) {
            var tr = document.createElement('tr');
            tr.id = 'claim-row-' + c.id;
            var wants;
            if (c.role === 'changeover') {
                wants = 'Evacuate &amp; restore';
            } else if (c.payload_code === '__empty__') {
                wants = 'Empty (clear node)';
            } else if (c.payload_code) {
                wants = c.payload_code + (c.role === 'produce' ? ' (empty bin)' : '');
            } else {
                wants = 'Unset';
            }
            var swapLabel = {'simple': 'Simple', 'single_robot': '1-Robot', 'two_robot': '2-Robot'}[c.swap_mode] || c.swap_mode || 'Simple';
            var flags = [];
            if (c.keep_staged) flags.push('staged');
            if (c.evacuate_on_changeover) flags.push('evac');
            if (c.auto_reorder) flags.push('auto');
            var flagStr = flags.length ? ' <span style="color:var(--text-muted);font-size:0.75rem">' + flags.join(', ') + '</span>' : '';
            // Store claim data for edit
            var claimJSON = ShingoEdge.escapeHtml(JSON.stringify(c));
            tr.innerHTML =
                '<td class="mono">' + ShingoEdge.escapeHtml(c.core_node_name) + '</td>' +
                '<td><span class="status-badge">' + ({consume:'Consume',produce:'Produce',changeover:'Changeover'}[c.role] || c.role) + '</span>' + flagStr + '</td>' +
                '<td>' + swapLabel + '</td>' +
                '<td>' + ShingoEdge.escapeHtml(wants) + (c.uop_capacity ? ' <span style="color:var(--text-muted);font-size:0.8rem">(' + c.uop_capacity + ' UOP)</span>' : '') + '</td>' +
                '<td class="mono">' + ShingoEdge.escapeHtml(c.inbound_staging || '\u2014') + '</td>' +
                '<td class="mono">' + ShingoEdge.escapeHtml(c.outbound_staging || '\u2014') + '</td>' +
                '<td style="white-space:nowrap">' +
                    '<button class="btn btn-sm" onclick=\'editClaim(' + JSON.stringify(c).replace(/'/g, "&#39;") + ')\'>Edit</button> ' +
                    '<button class="btn btn-sm btn-danger" onclick="removeClaim(' + c.id + ')">Remove</button>' +
                '</td>';
            tbody.appendChild(tr);
        });
        table.appendChild(tbody);
        list.appendChild(table);
    } catch (_) {}
}

function openClaimModal() {
    if (!_claimsStyleID) { ShingoEdge.toast('Select a style first', 'warning'); return; }
    document.getElementById('claims-edit-id').value = '';
    // Mark already-claimed nodes as disabled with strikethrough
    var sel = document.getElementById('claims-add-node');
    var claimedNodes = _currentClaims.map(function(c) { return c.core_node_name; });
    Array.from(sel.options).forEach(function(opt) {
        if (!opt.value) return;
        var claimed = claimedNodes.indexOf(opt.value) >= 0;
        opt.disabled = claimed;
        opt.style.textDecoration = claimed ? 'line-through' : '';
        opt.style.color = claimed ? 'var(--text-muted)' : '';
    });
    sel.value = '';
    sel.disabled = false;
    document.getElementById('claims-add-role').value = 'consume';
    document.getElementById('claims-add-swap').value = 'simple';
    document.getElementById('claims-add-payload').selectedIndex = 0;
    document.getElementById('claims-add-capacity').value = '0';
    document.getElementById('claims-add-reorder').value = '0';
    document.getElementById('claims-add-inbound').value = '';
    document.getElementById('claims-add-outbound').value = '';
    document.getElementById('claims-add-keep-staged').checked = false;
    document.getElementById('claims-add-evacuate').checked = false;
    document.getElementById('claim-modal-title').textContent = 'Add Node Claim';
    toggleClaimsAddPayload();
    ShingoEdge.showModal('claim-modal');
}

function editClaim(claim) {
    if (!_claimsStyleID) return;
    document.getElementById('claims-edit-id').value = claim.id;
    var sel = document.getElementById('claims-add-node');
    Array.from(sel.options).forEach(function(opt) {
        opt.disabled = false;
        opt.style.textDecoration = '';
        opt.style.color = '';
    });
    sel.value = claim.core_node_name;
    sel.disabled = true; // can't change node on edit
    document.getElementById('claims-add-role').value = claim.role || 'consume';
    document.getElementById('claims-add-swap').value = claim.swap_mode || 'simple';
    document.getElementById('claims-add-payload').value = claim.payload_code || '';
    document.getElementById('claims-add-capacity').value = claim.uop_capacity || 0;
    document.getElementById('claims-add-reorder').value = claim.reorder_point || 0;
    document.getElementById('claims-add-inbound').value = claim.inbound_staging || '';
    document.getElementById('claims-add-outbound').value = claim.outbound_staging || '';
    document.getElementById('claims-add-keep-staged').checked = !!claim.keep_staged;
    document.getElementById('claims-add-evacuate').checked = !!claim.evacuate_on_changeover;
    document.getElementById('claim-modal-title').textContent = 'Edit Node Claim';
    toggleClaimsAddPayload();
    ShingoEdge.showModal('claim-modal');
}

function closeClaimModal() {
    ShingoEdge.hideModal('claim-modal');
    document.getElementById('claims-add-node').disabled = false;
}

function validateClaimStaging() {
    var swap = document.getElementById('claims-add-swap').value;
    var inbound = document.getElementById('claims-add-inbound').value;
    var outbound = document.getElementById('claims-add-outbound').value;
    var warn = document.getElementById('claims-staging-warning');
    var needsStaging = swap === 'single_robot' || swap === 'two_robot';
    if (warn) warn.style.display = (needsStaging && (!inbound || !outbound)) ? '' : 'none';
    return !needsStaging || (!!inbound && !!outbound);
}

async function saveClaim() {
    var node = document.getElementById('claims-add-node').value;
    if (!node) { ShingoEdge.toast('Select a core node', 'warning'); return; }
    var role = document.getElementById('claims-add-role').value;
    var payloadCode = document.getElementById('claims-add-payload').value;
    var capacity = parseInt(document.getElementById('claims-add-capacity').value, 10) || 0;
    var reorder = parseInt(document.getElementById('claims-add-reorder').value, 10) || 0;

    if ((role === 'consume' || role === 'produce') && !payloadCode) {
        ShingoEdge.toast('Select a payload', 'warning');
        return;
    }
    if (!validateClaimStaging()) {
        ShingoEdge.toast('Swap modes require both inbound and outbound staging', 'warning');
        return;
    }

    try {
        await ShingoEdge.api.post('/api/style-node-claims', {
            style_id: _claimsStyleID,
            core_node_name: node,
            role: role,
            swap_mode: document.getElementById('claims-add-swap').value,
            payload_code: role === 'changeover' ? '' : payloadCode,
            uop_capacity: capacity,
            reorder_point: reorder,
            auto_reorder: true,
            inbound_staging: document.getElementById('claims-add-inbound').value,
            outbound_staging: document.getElementById('claims-add-outbound').value,
            keep_staged: document.getElementById('claims-add-keep-staged').checked,
            evacuate_on_changeover: document.getElementById('claims-add-evacuate').checked
        });
        closeClaimModal();
        await loadClaims(_claimsStyleID);
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function removeClaim(id) {
    try {
        await ShingoEdge.api.del('/api/style-node-claims/' + id);
        await loadClaims(_claimsStyleID);
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

function toggleClaimsAddPayload() {
    var role = document.getElementById('claims-add-role').value;
    var isChangeover = role === 'changeover';
    // Changeover-only nodes don't need payload or UOP config
    document.getElementById('claims-add-payload-group').style.display = isChangeover ? 'none' : '';
    document.getElementById('claims-add-reorder-group').style.display = isChangeover ? 'none' : '';
    if (isChangeover) {
        document.getElementById('claims-add-payload').value = '';
        document.getElementById('claims-add-capacity').value = '0';
        document.getElementById('claims-add-reorder').value = '0';
    }
}

async function syncPayloadCatalog() {
    try {
        await ShingoEdge.api.post('/api/payload-catalog/sync');
        _payloadCatalog = [];
        await loadPayloadCatalog();
        ShingoEdge.toast('Payload catalog synced', 'success');
    } catch (e) {
        ShingoEdge.toast('Sync failed: ' + e, 'error');
    }
}

function autoFillClaimsCapacity() {
    var sel = document.getElementById('claims-add-payload');
    var opt = sel.options[sel.selectedIndex];
    if (opt && opt.dataset.capacity) {
        document.getElementById('claims-add-capacity').value = opt.dataset.capacity;
    }
}

async function saveStyle() {
    const id = document.getElementById('style-id').value;
    const payload = {
        name: document.getElementById('style-name').value.trim(),
        description: document.getElementById('style-description').value.trim(),
        process_id: activeProcessID
    };
    if (!payload.name) {
        ShingoEdge.toast('Enter a style name', 'warning');
        return;
    }
    try {
        if (id) {
            await ShingoEdge.api.put('/api/styles/' + id, payload);
        } else {
            await ShingoEdge.api.post('/api/styles', payload);
        }
        closeStyleModal();
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function deleteStyle(id) {
    if (!await ShingoEdge.confirm('Delete this style?')) return;
    try {
        await ShingoEdge.api.del('/api/styles/' + id);
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

// --- Operator Screens (Stations) ---

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
    document.getElementById('station-modal-title').textContent = 'Add Operator Screen';
    ShingoEdge.showModal('station-modal');
}

function closeStationModal() {
    ShingoEdge.hideModal('station-modal');
    resetStationForm();
}

async function editStation(station) {
    resetStationForm();
    document.getElementById('station-id').value = station.id;
    document.getElementById('station-name').value = station.name || '';
    document.getElementById('station-note').value = station.note || '';
    document.getElementById('station-enabled').checked = !!station.enabled;
    // Load claimed nodes for this station
    try {
        var nodes = await ShingoEdge.api.get('/api/operator-stations/' + station.id + '/claimed-nodes');
        resetNodePicker(Array.isArray(nodes) ? nodes : []);
    } catch (_) {
        resetNodePicker([]);
    }
    showProcessTab('stations');
    document.getElementById('station-modal-title').textContent = 'Edit Operator Screen';
    ShingoEdge.showModal('station-modal');
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
        ShingoEdge.toast('Screen name is required', 'warning');
        return;
    }
    try {
        var stationID;
        if (id) {
            await ShingoEdge.api.put('/api/operator-stations/' + id, payload);
            stationID = id;
        } else {
            var res = await ShingoEdge.api.post('/api/operator-stations', payload);
            stationID = res.id;
        }
        // Save claimed nodes
        await ShingoEdge.api.put('/api/operator-stations/' + stationID + '/claimed-nodes', {
            nodes: getPickedNodes()
        });
        closeStationModal();
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function moveStation(id, direction) {
    try {
        await ShingoEdge.api.post('/api/operator-stations/' + id + '/move', { direction: direction });
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function deleteStation(id) {
    if (!await ShingoEdge.confirm('Delete this operator screen and its node assignments?')) return;
    try {
        await ShingoEdge.api.del('/api/operator-stations/' + id);
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

// Wire up tag-select pickers for PLC counter tag fields
(function initTagSelects() {
    ShingoEdge.tagSelect('counter-tag', 'counter-plc');
    ShingoEdge.tagSelect('new-process-counter-tag', 'new-process-counter-plc');
})();

// Initialize Node Claims tab (load catalog + first style's claims)
if (activeProcessID) initClaimsTab();
