import { api, confirm, delegateActions, escapeHtml, navigateToProcess, toast } from '/static/js/shingoedge.js';

var processID = parseInt(document.getElementById('page-data').dataset.processId || '0', 10);

// Actions that need JSON bodies or confirm dialogs remain as thin JS wrappers.
// Node action buttons (Stage, Release, Deliver, Switch, Skip, Retry) are pure htmx
// in node-actions.html. SSE auto-refresh is handled by the htmx SSE extension on
// the changeover-content div.

async function previewProcessChangeover() {
    var toStyleID = parseInt(document.getElementById('co-to-style').value || '0', 10);
    if (!toStyleID) {
        toast('Select a target style', 'warning');
        return;
    }
    try {
        var resp = await api.post('/api/processes/' + processID + '/changeover/preview', {
            to_style_id: toStyleID
        });
        renderChangeoverPreview(resp);
    } catch (e) {
        toast('Preview failed: ' + e, 'error');
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
        var esc = escapeHtml;
        var rows = actions.map(function(a) {
            var orderCell = function(spec) {
                if (!spec) return '<span style="color:var(--text-muted)">&mdash;</span>';
                if (spec.kind === 'complex') {
                    var dest = spec.delivery_node || '(in-place)';
                    var stepCount = Number(spec.step_count) || 0;
                    return '<span class="mono">complex &rarr; ' + esc(dest) + '</span> <span style="color:var(--text-muted);font-size:0.8rem">(' + stepCount + ' steps' + (spec.auto_confirm ? ', auto' : '') + ')</span>';
                }
                if (spec.kind === 'retrieve') {
                    return '<span class="mono">retrieve ' + esc(spec.payload_code || '') + ' &rarr; ' + esc(spec.delivery_node || '') + '</span>';
                }
                return '';
            };
            var err = a.error ? '<div style="color:red;font-size:0.8rem">' + esc(a.error) + '</div>' : '';
            return '<tr>' +
                '<td class="mono">' + esc(a.node_name || '') + err + '</td>' +
                '<td>' + esc(a.situation || '') + '</td>' +
                '<td>' + esc(a.log_tag || '') + '</td>' +
                '<td>' + orderCell(a.supply_order) + '</td>' +
                '<td>' + orderCell(a.evac_order) + '</td>' +
                '</tr>';
        }).join('');
        body.innerHTML = '<table class="table"><thead><tr><th>Node</th><th>Situation</th><th>Plan</th><th>Supply</th><th>Evac</th></tr></thead><tbody>' + rows + '</tbody></table>';
    }
    panel.style.display = '';
}

async function startProcessChangeover() {
    var toStyleID = parseInt(document.getElementById('co-to-style').value || '0', 10);
    if (!toStyleID) {
        toast('Select a target style', 'warning');
        return;
    }
    try {
        var co = await api.post('/api/processes/' + processID + '/changeover/start', {
            to_style_id: toStyleID,
            called_by: '',
            notes: ''
        });
        if (co && co.awaiting_stock && co.awaiting_stock.length) {
            toast('Changeover started — awaiting stock for: ' + co.awaiting_stock.join(', ') +
                '. These supply orders will dispatch automatically once the bins are loaded and manifest-confirmed.', 'warning');
        }
        htmx.trigger(document.body, 'refreshChangeover');
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

async function cancelProcessChangeover() {
    if (!await confirm('Cancel the active process changeover?')) return;
    try {
        await api.post('/api/processes/' + processID + '/changeover/cancel', {});
        htmx.trigger(document.body, 'refreshChangeover');
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

async function completeCutover() {
    try {
        await api.post('/api/processes/' + processID + '/changeover/cutover', {});
        htmx.trigger(document.body, 'refreshChangeover');
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

async function switchStation(stationID) {
    try {
        await api.post('/api/processes/' + processID + '/changeover/switch-station/' + stationID, {});
        htmx.trigger(document.body, 'refreshChangeover');
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

// closeChangeoverPreview — fired by the "Close" button on the
// changeover preview panel. Was an inline document.getElementById(...)
// expression; named here so the auto-dispatcher can wire it.
function closeChangeoverPreview() {
    var panel = document.getElementById('changeover-preview');
    if (panel) panel.style.display = 'none';
}

// ─── delegated event handlers ─────────────────────────
// All page-level data-action verbs route through delegateActions
// on document.body. Multiple event types share the same handler
// map — most handlers are click-only but a few (e.g. updatePreview)
// are referenced via data-action-change / data-action-input too,
// so binding the map across every event type keeps the page wiring
// single-source.
delegateActions(document.body, {
    cancelProcessChangeover,
    closeChangeoverPreview,
    completeCutover,
    navigateToProcess,
    previewProcessChangeover,
    renderChangeoverPreview,
    startProcessChangeover,
    switchStation
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
