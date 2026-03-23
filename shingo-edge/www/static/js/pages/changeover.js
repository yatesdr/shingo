var processID = parseInt(document.getElementById('page-data').dataset.processId || '0', 10);

// Actions that need JSON bodies or confirm dialogs remain as thin JS wrappers.
// Node action buttons (Stage, Release, Deliver, Switch, Skip, Retry) are pure htmx
// in node-actions.html. SSE auto-refresh is handled by the htmx SSE extension on
// the changeover-content div.

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
        htmx.trigger(document.body, 'refreshChangeover');
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function cancelProcessChangeover() {
    if (!await ShingoEdge.confirm('Cancel the active process changeover?')) return;
    try {
        await ShingoEdge.api.post('/api/processes/' + processID + '/changeover/cancel', {});
        htmx.trigger(document.body, 'refreshChangeover');
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function completeCutover() {
    try {
        await ShingoEdge.api.post('/api/processes/' + processID + '/changeover/cutover', {});
        htmx.trigger(document.body, 'refreshChangeover');
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function switchStation(stationID) {
    try {
        await ShingoEdge.api.post('/api/processes/' + processID + '/changeover/switch-station/' + stationID, {});
        htmx.trigger(document.body, 'refreshChangeover');
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}
