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
        renderUnresolvedParticipants(co && co.unresolved_participants);
        htmx.trigger(document.body, 'refreshChangeover');
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

async function cancelProcessChangeover() {
    if (!await confirm('Cancel the active process changeover?')) return;
    try {
        await api.post('/api/processes/' + processID + '/changeover/cancel', {});
        renderUnresolvedParticipants(null);
        htmx.trigger(document.body, 'refreshChangeover');
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

async function completeCutover() {
    try {
        await api.post('/api/processes/' + processID + '/changeover/cutover', {});
        renderUnresolvedParticipants(null);
        htmx.trigger(document.body, 'refreshChangeover');
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

// releaseChangeoverMaterial is the operator saying the setup is finished: ONE
// click that releases every leg of this changeover that is holding.
//
// A tool change is human work at the asset. While it runs, the marked seats'
// bins are already gone and the incoming material is parked at inbound staging
// with robots holding it — deliberately, so nothing drives into a cell someone
// is standing in. This is the button that ends that hold.
//
// It reports counts because the honest answer is sometimes partial: a leg that
// has not reached its wait yet cannot be released, and the operator needs to
// know there is one to come back for rather than assuming the cell is fed.
async function releaseChangeoverMaterial() {
    try {
        var res = await api.post('/api/processes/' + processID + '/changeover/release', {
            called_by: 'operator_station'
        });
        var released = (res && res.released) || 0;
        var pending = (res && res.pending) || 0;
        if (released === 0 && pending === 0) {
            toast('Nothing is waiting to be released', 'info');
        } else if (pending > 0) {
            toast('Released ' + released + '; ' + pending + ' not ready yet — click again when they stage', 'warning');
        } else {
            toast('Released ' + released + ' — material moving in', 'success');
        }
        htmx.trigger(document.body, 'refreshChangeover');
    } catch (e) {
        toast('Release failed: ' + e, 'error');
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

// renderUnresolvedParticipants shows the start response's advisory: participant
// nodes whose core_node_name resolves to no process_nodes row. These are
// press-index extension seats that own NO task — physically traversed by the
// index motion, carrying no order of their own, and invisible to every consumer
// without a row.
//
// SEATS THAT OWN A TASK ARE NOT IN THIS LIST and the wording must not imply
// they are. Their rows are auto-created at changeover start, and since the
// per-node actions resolve a seat's claim through its task's parent claim, a
// fanned-out seat is fully driveable with no configuration at all. The advisory
// used to name those too, which sent the engineer to add a node the system had
// just added itself.
//
// A BANNER, NOT A TOAST, and never blocking. The changeover has already
// started; this is a config gap the engineer fixes on the process-nodes page,
// which is not something to read in the three seconds a toast lasts. It is also
// the only place this list is ever available — it is transient and not
// persisted, so nothing re-renders it from state.
//
// Pass a falsy or empty list to clear it, which is what cancel and cutover do:
// the advisory belongs to one changeover and must not outlive it.
function renderUnresolvedParticipants(nodes) {
    var el = document.getElementById('changeover-advisory');
    if (!el) return;
    if (!nodes || !nodes.length) {
        el.hidden = true;
        el.innerHTML = '';
        return;
    }
    var names = nodes.map(function(n) { return '<span class="mono">' + escapeHtml(String(n)) + '</span>'; }).join(', ');
    var one = nodes.length === 1;
    el.innerHTML =
        '<strong>Changeover started.</strong> ' + nodes.length +
        (one ? ' participant node has' : ' participant nodes have') +
        ' no process node configured: ' + names +
        '. ' + (one ? 'This seat is' : 'These seats are') +
        ' indexed over by the press rather than handled directly, so ' +
        (one ? 'it owns' : 'they own') + ' no changeover task and no order. Without a row ' +
        (one ? 'it cannot' : 'they cannot') + ' be rendered on the board or protected from ' +
        'unrelated robot traffic — add ' + (one ? 'it' : 'them') + ' on the ' +
        '<a href="/processes">process nodes</a> page. The changeover is running regardless.';
    el.hidden = false;
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
    releaseChangeoverMaterial,
    renderChangeoverPreview,
    startProcessChangeover,
    switchStation
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
