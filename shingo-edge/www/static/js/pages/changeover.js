var processID = parseInt(document.getElementById('page-data').dataset.processId || '0', 10);
var changeoverFlow = Array.isArray(window.changeoverFlow) ? window.changeoverFlow : [];
var changeoverPhase = window.changeoverPhase || '';

async function startProcessChangeover() {
    var toStyleID = parseInt(document.getElementById('co-to-style').value || '0', 10);
    if (!toStyleID) {
        ShingoEdge.toast('Select a target style', 'warning');
        return;
    }
    try {
        await ShingoEdge.api.post('/api/processes/' + processID + '/changeover/start', {
            to_style_id: toStyleID,
            called_by: '',
            notes: ''
        });
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function cancelProcessChangeover() {
    if (!await ShingoEdge.confirm('Cancel the active process changeover?')) return;
    try {
        await ShingoEdge.api.post('/api/processes/' + processID + '/changeover/cancel', {});
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function setChangeoverPhase(phase) {
    try {
        await ShingoEdge.api.post('/api/processes/' + processID + '/changeover/phase', { phase: phase });
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

function nextChangeoverStep() {
    for (var i = 0; i < changeoverFlow.length; i++) {
        if (changeoverFlow[i].kind === changeoverPhase && i + 1 < changeoverFlow.length) {
            return changeoverFlow[i + 1];
        }
    }
    return null;
}

async function advanceToNextPhase() {
    var next = nextChangeoverStep();
    if (!next) {
        ShingoEdge.toast('No later changeover step is configured for this process', 'warning');
        return;
    }
    await setChangeoverPhase(next.kind);
}

async function completeCutover() {
    try {
        await ShingoEdge.api.post('/api/processes/' + processID + '/changeover/cutover', {});
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function switchStation(stationID) {
    try {
        await ShingoEdge.api.post('/api/processes/' + processID + '/changeover/switch-station/' + stationID, {});
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function stageNode(nodeID) {
    try {
        await ShingoEdge.api.post('/api/processes/' + processID + '/changeover/stage-node/' + nodeID, {});
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function emptyNode(nodeID) {
    try {
        await ShingoEdge.api.post('/api/processes/' + processID + '/changeover/empty-node/' + nodeID, {});
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function releaseNode(nodeID) {
    try {
        await ShingoEdge.api.post('/api/processes/' + processID + '/changeover/release-node/' + nodeID, {});
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function switchNode(nodeID) {
    try {
        await ShingoEdge.api.post('/api/processes/' + processID + '/changeover/switch-node/' + nodeID, {});
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

ShingoEdge.createSSE('/events', {
    onOrderUpdate: function() { location.reload(); }
});
