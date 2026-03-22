const activeProcessID = parseInt(document.getElementById('page-data').dataset.activeProcessId || '0', 10);
const processNodes = Array.isArray(window.processNodes) ? window.processNodes : [];
const processStyles = Array.isArray(window.processStyles) ? window.processStyles : [];
const processAssignments = Array.isArray(window.processAssignments) ? window.processAssignments : [];
const defaultProcessFlow = [
    { key: 'runout', kind: 'runout', label: 'Runout' },
    { key: 'tool_change', kind: 'tool_change', label: 'Tool Change' },
    { key: 'release', kind: 'release', label: 'Release' },
    { key: 'cutover', kind: 'cutover', label: 'Start New Style' },
    { key: 'verify', kind: 'verify', label: 'Verify' }
];

function processURL() {
    return '/processes?process=' + activeProcessID;
}

function resetProcessForm() {
    document.getElementById('new-process-name').value = '';
    document.getElementById('new-process-description').value = '';
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

function assignmentForNode(nodeID) {
    return processAssignments.filter(function(assignment) {
        return assignment.process_node_id === nodeID;
    });
}

function styleNameByID(styleID) {
    const style = processStyles.find(function(item) {
        return item.id === styleID;
    });
    return style ? style.name : ('Style ' + styleID);
}

function nodeLabelByID(nodeID) {
    const node = processNodes.find(function(item) {
        return item.id === nodeID;
    });
    if (!node) return 'Unknown node';
    return node.delegated_station_name ? (node.delegated_station_name + ' / ' + node.name) : node.name;
}

function renderNodeStyleCounts() {
    processNodes.forEach(function(node) {
        const el = document.getElementById('node-style-count-' + node.id);
        if (!el) return;
        const count = assignmentForNode(node.id).length;
        el.textContent = count ? (count + ' style' + (count === 1 ? '' : 's')) : 'None';
    });
}

function renderNodeAssignments() {
    const filter = document.getElementById('station-assignment-node-filter');
    const body = document.getElementById('node-assignments-body');
    const summary = document.getElementById('node-assignment-summary');
    if (!filter || !body || !summary) {
        return;
    }

    const nodeID = parseInt(filter.value || '0', 10);
    if (!nodeID) {
        summary.textContent = 'Select a process node to manage its job-style-specific material behavior.';
        body.innerHTML = '<tr><td colspan="7" class="empty-cell">Select a process node to view its style assignments</td></tr>';
        return;
    }

    const rows = assignmentForNode(nodeID).slice().sort(function(a, b) {
        return styleNameByID(a.style_id).localeCompare(styleNameByID(b.style_id));
    });
    summary.textContent = 'Style assignments for ' + nodeLabelByID(nodeID) + '.';
    if (!rows.length) {
        body.innerHTML = '<tr><td colspan="7" class="empty-cell">No style assignments configured for this process node</td></tr>';
        return;
    }

    body.innerHTML = rows.map(function(assignment) {
        const payload = assignment.payload_description || assignment.payload_code || '-';
        const changeover = assignment.changeover_policy || '-';
        return '<tr>' +
            '<td>' + ShingoEdge.escapeHtml(styleNameByID(assignment.style_id)) + '</td>' +
            '<td>' + ShingoEdge.escapeHtml(payload) + '</td>' +
            '<td>' + assignment.uop_capacity + '</td>' +
            '<td>' + assignment.reorder_point + '</td>' +
            '<td>' + ShingoEdge.escapeHtml(assignment.cycle_mode || 'simple') + '</td>' +
            '<td>' + ShingoEdge.escapeHtml(changeover) + '</td>' +
            '<td style="display:flex;gap:0.4rem;flex-wrap:wrap">' +
            '<button class="btn btn-sm" onclick="editAssignmentByID(' + assignment.id + ')">Edit</button>' +
            '<button class="btn btn-sm btn-danger" onclick="deleteAssignment(' + assignment.id + ')">Delete</button>' +
            '</td>' +
        '</tr>';
    }).join('');
}

function focusNodeAssignments(nodeID) {
    const filter = document.getElementById('station-assignment-node-filter');
    if (filter) {
        filter.value = String(nodeID);
    }
    showProcessTab('stations');
    renderNodeAssignments();
    const builder = document.getElementById('node-style-builder');
    if (builder) {
        builder.scrollIntoView({ behavior: 'smooth', block: 'start' });
    }
}

async function createProcess() {
    const name = document.getElementById('new-process-name').value.trim();
    if (!name) {
        ShingoEdge.toast('Enter a process name', 'warning');
        return;
    }
    try {
        const res = await ShingoEdge.api.post('/api/processes', {
            name: name,
            description: document.getElementById('new-process-description').value.trim(),
            production_state: 'active_production',
            cutover_mode: 'manual',
            changeover_flow: defaultProcessFlow
        });
        window.location = '/processes?process=' + res.id;
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

function collectFlow() {
    const rows = Array.from(document.querySelectorAll('#flow-editor .card-body')).map(function(row) {
        return {
            kind: row.querySelector('.flow-order').dataset.kind,
            enabled: row.querySelector('.flow-enabled').checked,
            label: row.querySelector('.flow-label').value.trim(),
            order: parseInt(row.querySelector('.flow-order').value || '0', 10)
        };
    });
    rows.sort(function(a, b) { return a.order - b.order; });
    return rows.filter(function(step) { return step.enabled || step.kind === 'cutover'; }).map(function(step) {
        return { key: step.kind, kind: step.kind, label: step.label || step.kind };
    });
}

async function saveProcess() {
    try {
        await ShingoEdge.api.put('/api/processes/' + activeProcessID, {
            name: document.getElementById('process-name').value.trim(),
            description: document.getElementById('process-description').value.trim(),
            production_state: document.getElementById('process-production-state').value,
            cutover_mode: document.getElementById('process-cutover-mode').value,
            changeover_flow: collectFlow()
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

async function setProcessActiveStyle(styleID) {
    const value = styleID != null ? styleID : (document.getElementById('process-active-style').value || '');
    try {
        await ShingoEdge.api.put('/api/processes/' + activeProcessID + '/active-style', {
            job_style_id: value ? parseInt(value, 10) : null
        });
        ShingoEdge.toast('Active style updated', 'success');
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function saveProcessCounter() {
    try {
        await ShingoEdge.api.put('/api/processes/' + activeProcessID + '/counter-binding', {
            plc_name: document.getElementById('counter-plc').value,
            tag_name: document.getElementById('counter-tag').value.trim(),
            enabled: document.getElementById('counter-enabled').checked
        });
        ShingoEdge.toast('Process counter binding saved', 'success');
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

function resetStyleForm() {
    document.getElementById('style-id').value = '';
    document.getElementById('style-name').value = '';
    document.getElementById('style-description').value = '';
    document.getElementById('style-catids').value = '';
}

function openCreateStyleModal() {
    resetStyleForm();
    document.getElementById('style-modal-title').textContent = 'Add Job Style';
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
    document.getElementById('style-catids').value = (style.cat_ids || []).join(', ');
    showProcessTab('styles');
    document.getElementById('style-modal-title').textContent = 'Edit Job Style';
    ShingoEdge.showModal('style-modal');
}

async function saveStyle() {
    const id = document.getElementById('style-id').value;
    const payload = {
        name: document.getElementById('style-name').value.trim(),
        description: document.getElementById('style-description').value.trim(),
        cat_ids: document.getElementById('style-catids').value.split(',').map(function(v) { return v.trim(); }).filter(Boolean),
        line_id: activeProcessID,
        rp_plc_name: '',
        rp_tag_name: '',
        rp_enabled: false
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

function resetStationForm() {
    document.getElementById('station-id').value = '';
    document.getElementById('station-name').value = '';
    document.getElementById('station-note').value = '';
    document.getElementById('station-enabled').checked = true;
}

function openCreateStationModal() {
    resetStationForm();
    document.getElementById('station-modal-title').textContent = 'Add Operator Station';
    ShingoEdge.showModal('station-modal');
}

function closeStationModal() {
    ShingoEdge.hideModal('station-modal');
    resetStationForm();
}

function editStation(station) {
    resetStationForm();
    document.getElementById('station-id').value = station.id;
    document.getElementById('station-name').value = station.name || '';
    document.getElementById('station-note').value = station.note || '';
    document.getElementById('station-enabled').checked = !!station.enabled;
    showProcessTab('stations');
    document.getElementById('station-modal-title').textContent = 'Edit Operator Station';
    ShingoEdge.showModal('station-modal');
}

async function saveStation() {
    const id = document.getElementById('station-id').value;
    const payload = {
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
        ShingoEdge.toast('Station name is required', 'warning');
        return;
    }
    try {
        if (id) {
            await ShingoEdge.api.put('/api/operator-stations/' + id, payload);
        } else {
            await ShingoEdge.api.post('/api/operator-stations', payload);
        }
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
    if (!await ShingoEdge.confirm('Delete this operator station?')) return;
    try {
        await ShingoEdge.api.del('/api/operator-stations/' + id);
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

function resetNodeForm() {
    document.getElementById('node-id').value = '';
    document.getElementById('node-core-node').value = '';
    document.getElementById('node-name').value = '';
    document.getElementById('node-position-type').value = 'consume';
    document.getElementById('node-delivery').value = '';
    document.getElementById('node-staging').value = '';
    document.getElementById('node-staging-group').value = '';
    document.getElementById('node-secondary-staging').value = '';
    document.getElementById('node-secondary-group').value = '';
    document.getElementById('node-full-pickup').value = '';
    document.getElementById('node-full-pickup-group').value = '';
    document.getElementById('node-outgoing').value = '';
    document.getElementById('node-outgoing-group').value = '';
    document.getElementById('node-station-id').value = '';
    document.getElementById('node-enabled').checked = true;
    setNodeAdvanced(false);
}

function openCreateNodeModal() {
    resetNodeForm();
    document.getElementById('node-modal-title').textContent = 'Add Process Node';
    ShingoEdge.showModal('node-modal');
}

function closeNodeModal() {
    ShingoEdge.hideModal('node-modal');
    resetNodeForm();
}

function setNodeAdvanced(open) {
    var section = document.getElementById('node-advanced-fields');
    if (!section) return;
    section.style.display = open ? 'block' : 'none';
}

function toggleNodeAdvanced() {
    var section = document.getElementById('node-advanced-fields');
    if (!section) return;
    setNodeAdvanced(section.style.display === 'none');
}

function editNode(node) {
    resetNodeForm();
    document.getElementById('node-id').value = node.id;
    document.getElementById('node-station-id').value = node.delegated_station_id || '';
    document.getElementById('node-core-node').value = node.core_node_name || '';
    document.getElementById('node-name').value = node.name || '';
    document.getElementById('node-position-type').value = node.position_type || 'consume';
    document.getElementById('node-delivery').value = node.delivery_node || '';
    document.getElementById('node-staging').value = node.staging_node || '';
    document.getElementById('node-staging-group').value = node.staging_node_group || '';
    document.getElementById('node-secondary-staging').value = node.secondary_staging_node || '';
    document.getElementById('node-secondary-group').value = node.secondary_node_group || '';
    document.getElementById('node-full-pickup').value = node.full_pickup_node || '';
    document.getElementById('node-full-pickup-group').value = node.full_pickup_node_group || '';
    document.getElementById('node-outgoing').value = node.outgoing_node || '';
    document.getElementById('node-outgoing-group').value = node.outgoing_node_group || '';
    document.getElementById('node-enabled').checked = !!node.enabled;
    setNodeAdvanced(!!(node.staging_node_group || node.secondary_staging_node || node.secondary_node_group || node.full_pickup_node || node.full_pickup_node_group || node.outgoing_node_group || (node.outgoing_node && node.outgoing_node !== node.core_node_name)));
    showProcessTab('stations');
    document.getElementById('node-modal-title').textContent = 'Edit Process Node';
    ShingoEdge.showModal('node-modal');
}

async function saveNode() {
    const id = document.getElementById('node-id').value;
    const delegatedStationValue = document.getElementById('node-station-id').value;
    const payload = {
        process_id: activeProcessID,
        delegated_station_id: delegatedStationValue ? parseInt(delegatedStationValue, 10) : null,
        code: '',
        core_node_name: document.getElementById('node-core-node').value.trim(),
        name: document.getElementById('node-name').value.trim(),
        position_type: document.getElementById('node-position-type').value.trim() || 'consume',
        sequence: 0,
        delivery_node: document.getElementById('node-delivery').value.trim(),
        staging_node: document.getElementById('node-staging').value.trim(),
        staging_node_group: document.getElementById('node-staging-group').value.trim(),
        secondary_staging_node: document.getElementById('node-secondary-staging').value.trim(),
        secondary_node_group: document.getElementById('node-secondary-group').value.trim(),
        full_pickup_node: document.getElementById('node-full-pickup').value.trim(),
        full_pickup_node_group: document.getElementById('node-full-pickup-group').value.trim(),
        outgoing_node: document.getElementById('node-outgoing').value.trim(),
        outgoing_node_group: document.getElementById('node-outgoing-group').value.trim(),
        allows_reorder: false,
        allows_empty_release: false,
        allows_partial_release: false,
        allows_manifest_confirm: false,
        allows_station_change: false,
        enabled: document.getElementById('node-enabled').checked
    };
    if (!payload.core_node_name || !payload.delivery_node) {
        ShingoEdge.toast('Core delegate node and delivery node are required', 'warning');
        return;
    }
    try {
        if (id) {
            await ShingoEdge.api.put('/api/process-nodes/' + id, payload);
        } else {
            await ShingoEdge.api.post('/api/process-nodes', payload);
        }
        closeNodeModal();
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function deleteNode(id) {
    if (!await ShingoEdge.confirm('Delete this process node?')) return;
    try {
        await ShingoEdge.api.del('/api/process-nodes/' + id);
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

function resetAssignmentForm() {
    document.getElementById('assignment-payload-code').value = '';
    document.getElementById('assignment-payload-description').value = '';
    document.getElementById('assignment-capacity').value = '0';
    document.getElementById('assignment-reorder').value = '0';
    document.getElementById('assignment-cycle-mode').value = 'simple';
    document.getElementById('assignment-changeover-policy').value = 'manual_station_change';
    document.getElementById('assignment-changeover-group').value = '';
    document.getElementById('assignment-changeover-sequence').value = '0';
    document.getElementById('assignment-auto-reorder').checked = true;
    document.getElementById('assignment-retrieve-empty').checked = false;
    document.getElementById('assignment-requires-manifest').checked = true;
    document.getElementById('assignment-allows-partial').checked = true;
}

function openCreateAssignmentModal() {
    resetAssignmentForm();
    const filter = document.getElementById('station-assignment-node-filter');
    if (filter && filter.value) {
        document.getElementById('assignment-node-id').value = filter.value;
    }
    document.getElementById('assignment-modal-title').textContent = 'Add Assignment';
    ShingoEdge.showModal('assignment-modal');
}

function closeAssignmentModal() {
    ShingoEdge.hideModal('assignment-modal');
    resetAssignmentForm();
}

function editAssignment(assignment) {
    resetAssignmentForm();
    document.getElementById('assignment-node-id').value = assignment.process_node_id;
    document.getElementById('assignment-style-id').value = assignment.style_id;
    document.getElementById('assignment-payload-code').value = assignment.payload_code || '';
    document.getElementById('assignment-payload-description').value = assignment.payload_description || '';
    document.getElementById('assignment-capacity').value = assignment.uop_capacity || 0;
    document.getElementById('assignment-reorder').value = assignment.reorder_point || 0;
    document.getElementById('assignment-cycle-mode').value = assignment.cycle_mode || 'simple';
    document.getElementById('assignment-changeover-policy').value = assignment.changeover_policy || 'manual_station_change';
    document.getElementById('assignment-changeover-group').value = assignment.changeover_group || '';
    document.getElementById('assignment-changeover-sequence').value = assignment.changeover_sequence || 0;
    document.getElementById('assignment-auto-reorder').checked = !!assignment.auto_reorder_enabled;
    document.getElementById('assignment-retrieve-empty').checked = !!assignment.retrieve_empty;
    document.getElementById('assignment-requires-manifest').checked = !!assignment.requires_manifest_confirmation;
    document.getElementById('assignment-allows-partial').checked = !!assignment.allows_partial_return;
    showProcessTab('stations');
    const filter = document.getElementById('station-assignment-node-filter');
    if (filter) {
        filter.value = String(assignment.process_node_id);
    }
    document.getElementById('assignment-modal-title').textContent = 'Edit Assignment';
    ShingoEdge.showModal('assignment-modal');
}

function editAssignmentByID(id) {
    const assignment = processAssignments.find(function(item) {
        return item.id === id;
    });
    if (!assignment) {
        ShingoEdge.toast('Assignment not found', 'warning');
        return;
    }
    editAssignment(assignment);
}

async function saveAssignment() {
    try {
        await ShingoEdge.api.post('/api/process-node-assignments', {
            process_node_id: parseInt(document.getElementById('assignment-node-id').value, 10),
            style_id: parseInt(document.getElementById('assignment-style-id').value, 10),
            payload_code: document.getElementById('assignment-payload-code').value.trim(),
            payload_description: document.getElementById('assignment-payload-description').value.trim(),
            role: 'consume',
            uop_capacity: parseInt(document.getElementById('assignment-capacity').value || '0', 10),
            reorder_point: parseInt(document.getElementById('assignment-reorder').value || '0', 10),
            auto_reorder_enabled: document.getElementById('assignment-auto-reorder').checked,
            cycle_mode: document.getElementById('assignment-cycle-mode').value,
            retrieve_empty: document.getElementById('assignment-retrieve-empty').checked,
            requires_manifest_confirmation: document.getElementById('assignment-requires-manifest').checked,
            allows_partial_return: document.getElementById('assignment-allows-partial').checked,
            changeover_group: document.getElementById('assignment-changeover-group').value.trim(),
            changeover_sequence: parseInt(document.getElementById('assignment-changeover-sequence').value || '0', 10),
            changeover_policy: document.getElementById('assignment-changeover-policy').value
        });
        closeAssignmentModal();
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function deleteAssignment(id) {
    if (!await ShingoEdge.confirm('Delete this assignment?')) return;
    try {
        await ShingoEdge.api.del('/api/process-node-assignments/' + id);
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

(function initFlowEditor() {
    const flow = Array.isArray(window.processFlow) ? window.processFlow : [];
    if (!flow.length) return;
    const byKind = {};
    flow.forEach(function(step, index) {
        byKind[step.kind] = { order: index + 1, label: step.label || step.kind };
    });
    document.querySelectorAll('.flow-order').forEach(function(input, index) {
        const kind = input.dataset.kind;
        if (byKind[kind]) {
            input.value = byKind[kind].order;
            document.querySelector('.flow-enabled[data-kind="' + kind + '"]').checked = true;
            document.querySelector('.flow-label[data-kind="' + kind + '"]').value = byKind[kind].label;
        } else {
            input.value = index + 1;
            if (kind !== 'cutover') {
                document.querySelector('.flow-enabled[data-kind="' + kind + '"]').checked = false;
            }
        }
    });
})();

(function initStationsBuilder() {
    renderNodeStyleCounts();
    const filter = document.getElementById('station-assignment-node-filter');
    if (!filter) {
        return;
    }
    if (!filter.value && processNodes.length) {
        filter.value = String(processNodes[0].id);
    }
    renderNodeAssignments();
})();
