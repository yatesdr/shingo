var processID = parseInt(document.getElementById('page-data').dataset.processId || '0', 10);

// Actions that need JSON bodies or confirm dialogs remain as thin JS wrappers.
// Node action buttons (Stage, Release, Deliver, Switch, Skip, Retry) are pure htmx
// in node-actions.html. SSE auto-refresh is handled by the htmx SSE extension on
// the changeover-content div.

async function previewProcessChangeover() {
    var toStyleID = parseInt(document.getElementById('co-to-style').value || '0', 10);
    if (!toStyleID) {
        ShingoEdge.toast('Select a target style', 'warning');
        return;
    }
    try {
        var resp = await ShingoEdge.api.post('/api/processes/' + processID + '/changeover/preview', {
            to_style_id: toStyleID
        });
        renderChangeoverPreview(resp);
    } catch (e) {
        ShingoEdge.toast('Preview failed: ' + e, 'error');
    }
}

function renderChangeoverPreview(plan) {
    var body = document.getElementById('changeover-preview-body');
    var panel = document.getElementById('changeover-preview');
    if (!body || !panel) return;
    var actions = (plan && plan.actions) || [];
    if (actions.length === 0) {
        body.innerHTML = '<p style="color:var(--text-muted)">No node changes — target style matches current claims.</p>';
    } else {
        var rows = actions.map(function(a) {
            var orderCell = function(spec) {
                if (!spec) return '<span style="color:var(--text-muted)">&mdash;</span>';
                if (spec.kind === 'complex') {
                    var dest = spec.delivery_node || '(in-place)';
                    return '<span class="mono">complex &rarr; ' + dest + '</span> <span style="color:var(--text-muted);font-size:0.8rem">(' + spec.step_count + ' steps' + (spec.auto_confirm ? ', auto' : '') + ')</span>';
                }
                if (spec.kind === 'retrieve') {
                    return '<span class="mono">retrieve ' + (spec.payload_code || '') + ' &rarr; ' + (spec.delivery_node || '') + '</span>';
                }
                return '';
            };
            var err = a.error ? '<div style="color:red;font-size:0.8rem">' + a.error + '</div>' : '';
            return '<tr>' +
                '<td class="mono">' + a.node_name + err + '</td>' +
                '<td>' + a.situation + '</td>' +
                '<td>' + (a.log_tag || '') + '</td>' +
                '<td>' + orderCell(a.order_a) + '</td>' +
                '<td>' + orderCell(a.order_b) + '</td>' +
                '</tr>';
        }).join('');
        body.innerHTML = '<table class="table"><thead><tr><th>Node</th><th>Situation</th><th>Plan</th><th>Order A</th><th>Order B</th></tr></thead><tbody>' + rows + '</tbody></table>';
    }
    panel.style.display = '';
}

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
