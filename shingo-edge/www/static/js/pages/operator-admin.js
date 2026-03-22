async function createStation() {
    try {
        const parentRaw = document.getElementById('new-station-parent').value;
        await ShingoEdge.api.post('/api/operator-stations', {
            process_id: parseInt(document.getElementById('new-station-process').value, 10),
            parent_station_id: parentRaw ? parseInt(parentRaw, 10) : null,
            code: document.getElementById('new-station-code').value.trim(),
            name: document.getElementById('new-station-name').value.trim(),
            area_label: document.getElementById('new-station-area').value.trim(),
            sequence: parseInt(document.getElementById('new-station-sequence').value || '0', 10),
            controller_node_id: document.getElementById('new-station-controller').value.trim(),
            enabled: true
        });
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

async function createNode() {
    try {
        await ShingoEdge.api.post('/api/op-station-nodes', {
            operator_station_id: parseInt(document.getElementById('new-node-station').value, 10),
            code: document.getElementById('new-node-code').value.trim(),
            name: document.getElementById('new-node-name').value.trim(),
            position_type: 'consume',
            sequence: parseInt(document.getElementById('new-node-sequence').value || '0', 10),
            delivery_node: document.getElementById('new-node-delivery').value.trim(),
            outgoing_node: document.getElementById('new-node-outgoing').value.trim(),
            allows_reorder: true,
            allows_empty_release: true,
            allows_partial_release: true,
            allows_manifest_confirm: true,
            allows_station_change: true,
            enabled: true
        });
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function deleteNode(id) {
    if (!await ShingoEdge.confirm('Delete this operator station node?')) return;
    try {
        await ShingoEdge.api.del('/api/op-station-nodes/' + id);
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function upsertAssignment() {
    try {
        await ShingoEdge.api.post('/api/op-node-assignments', {
            op_node_id: parseInt(document.getElementById('assign-node').value, 10),
            style_id: parseInt(document.getElementById('assign-style').value, 10),
            payload_code: document.getElementById('assign-payload').value.trim(),
            payload_description: document.getElementById('assign-description').value.trim(),
            role: 'consume',
            cycle_mode: document.getElementById('assign-cycle').value,
            uop_capacity: parseInt(document.getElementById('assign-capacity').value || '0', 10),
            reorder_point: parseInt(document.getElementById('assign-reorder').value || '0', 10),
            auto_reorder_enabled: true,
            retrieve_empty: false,
            requires_manifest_confirmation: true,
            allows_partial_return: true,
            changeover_policy: document.getElementById('assign-changeover-policy').value
        });
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function deleteAssignment(id) {
    if (!await ShingoEdge.confirm('Delete this style assignment?')) return;
    try {
        await ShingoEdge.api.del('/api/op-node-assignments/' + id);
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}
