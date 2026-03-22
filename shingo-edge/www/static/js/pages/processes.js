const activeProcessID = parseInt(document.getElementById('page-data').dataset.activeProcessId || '0', 10);

function processURL() {
    return '/processes?process=' + activeProcessID;
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
    try {
        const res = await ShingoEdge.api.post('/api/processes', {
            name: name,
            description: document.getElementById('new-process-description').value.trim(),
            production_state: 'active_production',
            cutover_mode: 'manual',
            changeover_flow: collectFlow()
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

function editStyle(style) {
    document.getElementById('style-id').value = style.id;
    document.getElementById('style-name').value = style.name || '';
    document.getElementById('style-description').value = style.description || '';
    document.getElementById('style-catids').value = (style.cat_ids || []).join(', ');
    showProcessTab('styles');
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
    document.getElementById('station-code').value = '';
    document.getElementById('station-name').value = '';
    document.getElementById('station-area').value = '';
    document.getElementById('station-sequence').value = '0';
    document.getElementById('station-parent').value = '';
    document.getElementById('station-controller').value = '';
    document.getElementById('station-enabled').checked = true;
}

function editStation(station) {
    document.getElementById('station-id').value = station.id;
    document.getElementById('station-code').value = station.code || '';
    document.getElementById('station-name').value = station.name || '';
    document.getElementById('station-area').value = station.area_label || '';
    document.getElementById('station-sequence').value = station.sequence || 0;
    document.getElementById('station-parent').value = station.parent_station_id || '';
    document.getElementById('station-controller').value = station.controller_node_id || '';
    document.getElementById('station-enabled').checked = !!station.enabled;
    showProcessTab('stations');
}

async function saveStation() {
    const id = document.getElementById('station-id').value;
    const parentValue = document.getElementById('station-parent').value;
    const payload = {
        process_id: activeProcessID,
        parent_station_id: parentValue ? parseInt(parentValue, 10) : null,
        code: document.getElementById('station-code').value.trim(),
        name: document.getElementById('station-name').value.trim(),
        area_label: document.getElementById('station-area').value.trim(),
        sequence: parseInt(document.getElementById('station-sequence').value || '0', 10),
        controller_node_id: document.getElementById('station-controller').value.trim(),
        enabled: document.getElementById('station-enabled').checked,
        device_mode: 'fixed_hmi'
    };
    if (!payload.name || !payload.code) {
        ShingoEdge.toast('Station code and name are required', 'warning');
        return;
    }
    try {
        if (id) {
            await ShingoEdge.api.put('/api/operator-stations/' + id, payload);
        } else {
            await ShingoEdge.api.post('/api/operator-stations', payload);
        }
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
    document.getElementById('node-code').value = '';
    document.getElementById('node-name').value = '';
    document.getElementById('node-position-type').value = 'consume';
    document.getElementById('node-sequence').value = '0';
    document.getElementById('node-delivery').value = '';
    document.getElementById('node-staging').value = '';
    document.getElementById('node-staging-group').value = '';
    document.getElementById('node-secondary-staging').value = '';
    document.getElementById('node-secondary-group').value = '';
    document.getElementById('node-full-pickup').value = '';
    document.getElementById('node-full-pickup-group').value = '';
    document.getElementById('node-outgoing').value = '';
    document.getElementById('node-outgoing-group').value = '';
    document.getElementById('node-allows-reorder').checked = true;
    document.getElementById('node-allows-empty').checked = true;
    document.getElementById('node-allows-partial').checked = true;
    document.getElementById('node-allows-manifest').checked = true;
    document.getElementById('node-allows-station-change').checked = true;
    document.getElementById('node-enabled').checked = true;
}

function editNode(node) {
    document.getElementById('node-id').value = node.id;
    document.getElementById('node-station-id').value = node.operator_station_id;
    document.getElementById('node-code').value = node.code || '';
    document.getElementById('node-name').value = node.name || '';
    document.getElementById('node-position-type').value = node.position_type || 'consume';
    document.getElementById('node-sequence').value = node.sequence || 0;
    document.getElementById('node-delivery').value = node.delivery_node || '';
    document.getElementById('node-staging').value = node.staging_node || '';
    document.getElementById('node-staging-group').value = node.staging_node_group || '';
    document.getElementById('node-secondary-staging').value = node.secondary_staging_node || '';
    document.getElementById('node-secondary-group').value = node.secondary_node_group || '';
    document.getElementById('node-full-pickup').value = node.full_pickup_node || '';
    document.getElementById('node-full-pickup-group').value = node.full_pickup_node_group || '';
    document.getElementById('node-outgoing').value = node.outgoing_node || '';
    document.getElementById('node-outgoing-group').value = node.outgoing_node_group || '';
    document.getElementById('node-allows-reorder').checked = !!node.allows_reorder;
    document.getElementById('node-allows-empty').checked = !!node.allows_empty_release;
    document.getElementById('node-allows-partial').checked = !!node.allows_partial_release;
    document.getElementById('node-allows-manifest').checked = !!node.allows_manifest_confirm;
    document.getElementById('node-allows-station-change').checked = !!node.allows_station_change;
    document.getElementById('node-enabled').checked = !!node.enabled;
    showProcessTab('stations');
}

async function saveNode() {
    const id = document.getElementById('node-id').value;
    const payload = {
        operator_station_id: parseInt(document.getElementById('node-station-id').value, 10),
        code: document.getElementById('node-code').value.trim(),
        name: document.getElementById('node-name').value.trim(),
        position_type: document.getElementById('node-position-type').value.trim() || 'consume',
        sequence: parseInt(document.getElementById('node-sequence').value || '0', 10),
        delivery_node: document.getElementById('node-delivery').value.trim(),
        staging_node: document.getElementById('node-staging').value.trim(),
        staging_node_group: document.getElementById('node-staging-group').value.trim(),
        secondary_staging_node: document.getElementById('node-secondary-staging').value.trim(),
        secondary_node_group: document.getElementById('node-secondary-group').value.trim(),
        full_pickup_node: document.getElementById('node-full-pickup').value.trim(),
        full_pickup_node_group: document.getElementById('node-full-pickup-group').value.trim(),
        outgoing_node: document.getElementById('node-outgoing').value.trim(),
        outgoing_node_group: document.getElementById('node-outgoing-group').value.trim(),
        allows_reorder: document.getElementById('node-allows-reorder').checked,
        allows_empty_release: document.getElementById('node-allows-empty').checked,
        allows_partial_release: document.getElementById('node-allows-partial').checked,
        allows_manifest_confirm: document.getElementById('node-allows-manifest').checked,
        allows_station_change: document.getElementById('node-allows-station-change').checked,
        enabled: document.getElementById('node-enabled').checked
    };
    if (!payload.code || !payload.name) {
        ShingoEdge.toast('Node code and name are required', 'warning');
        return;
    }
    try {
        if (id) {
            await ShingoEdge.api.put('/api/op-station-nodes/' + id, payload);
        } else {
            await ShingoEdge.api.post('/api/op-station-nodes', payload);
        }
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function deleteNode(id) {
    if (!await ShingoEdge.confirm('Delete this station node?')) return;
    try {
        await ShingoEdge.api.del('/api/op-station-nodes/' + id);
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

function editAssignment(assignment) {
    document.getElementById('assignment-node-id').value = assignment.op_node_id;
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
    showProcessTab('assignments');
}

async function saveAssignment() {
    try {
        await ShingoEdge.api.post('/api/op-node-assignments', {
            op_node_id: parseInt(document.getElementById('assignment-node-id').value, 10),
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
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function deleteAssignment(id) {
    if (!await ShingoEdge.confirm('Delete this assignment?')) return;
    try {
        await ShingoEdge.api.del('/api/op-node-assignments/' + id);
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
