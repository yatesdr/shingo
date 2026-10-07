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
// A tool change is human work at the asset. While it runs, the marked positions'
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
        // deferred counts legs the release HELD: each has an intent and goes by
        // itself when what it waits on happens (its robot parks, the old bin is
        // lifted, the curtain clears, Core answers), so it needs no click. The
        // first hold's own sentence says what it is waiting on. pending counts
        // legs nothing remembered, which do need the click again.
        var released = (res && res.released) || 0;
        var pending = (res && res.pending) || 0;
        var deferred = (res && res.deferred) || 0;
        var why = (res && res.held && res.held[0]) || 'they go by themselves';
        var follow = deferred > 0 ? '; ' + deferred + ' held, ' + why : '';
        if (released === 0 && pending === 0 && deferred === 0) {
            toast('Nothing is waiting to be released', 'info');
        } else if (pending > 0) {
            toast('Released ' + released + follow + '; ' + pending + ' not ready yet — click again when they stage', 'warning');
        } else {
            toast('Released ' + released + follow + ' — material moving in', 'success');
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
// press-index extension positions that own NO task — physically traversed by the
// index motion, carrying no order of their own, and invisible to every consumer
// without a row.
//
// POSITIONS THAT OWN A TASK ARE NOT IN THIS LIST and the wording must not imply
// they are. Their rows are auto-created at changeover start, and since the
// per-node actions resolve a position's claim through its task's parent claim, a
// fanned-out position is fully driveable with no configuration at all. The advisory
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
        '. ' + (one ? 'This position is' : 'These positions are') +
        ' indexed over by the press rather than handled directly, so ' +
        (one ? 'it owns' : 'they own') + ' no changeover task and no order. Without a row ' +
        (one ? 'it cannot' : 'they cannot') + ' be rendered on the board or protected from ' +
        'unrelated robot traffic — add ' + (one ? 'it' : 'them') + ' on the ' +
        '<a href="/processes">process nodes</a> page. The changeover is running regardless.';
    el.hidden = false;
}

// ─── live refresh: one reload per action (LC4) ────────
// #changeover-content reloads on refreshChangeover (an action's own answer, or
// the explicit trigger above) and on the SSE events the action causes
// (changeover-update, order-update, order-failed). They arrive together, and
// each used to start its own reload: up to three per click. Every trigger now
// joins one pending reload, issued once the burst has settled. Order traffic
// keeps the 2 s spacing its throttle gave it; anything else is not held back
// by it.
var SETTLE_MS = 300;
var ORDER_GAP_MS = 2000;
var reloadTimer = null;
var reloadDue = 0;
var lastReload = 0;
document.body.addEventListener('htmx:confirm', function(evt) {
    if (!evt.detail.elt || evt.detail.elt.id !== 'changeover-content') return;
    evt.preventDefault();
    var now = Date.now();
    var trig = evt.detail.triggeringEvent;
    var due = now + SETTLE_MS;
    if (trig && trig.type === 'sse:order-update') due = Math.max(due, lastReload + ORDER_GAP_MS);
    if (reloadTimer) {
        if (due >= reloadDue) return; // joins the pending reload
        clearTimeout(reloadTimer);
    }
    reloadDue = due;
    reloadTimer = setTimeout(function() {
        reloadTimer = null;
        lastReload = Date.now();
        evt.detail.issueRequest(true);
    }, due - now);
});

// ─── live refresh keeps the chosen target style ───────
// The partial is swapped wholesale, and #co-to-style and the preview live in
// it. Carry the choice and its open preview across the swap, as long as the
// new partial still offers that style (no changeover started meanwhile).
//
// Restored on afterSwap AND afterSettle: htmx 2's settle step puts the
// server's attributes back about 20 ms after the swap, which re-hides the
// panel (style="display:none") after an afterSwap-only restore. The choice is
// held here until a settle has applied it, so a refresh that starts in
// between saves nothing from the half-restored page and loses nothing.
var keptChoice = null;      // {style, preview}: to apply after the swap in flight
var restorePending = false; // keptChoice not yet applied by an afterSettle
function isChangeoverSwap(evt) {
    return evt.detail && evt.detail.target && evt.detail.target.id === 'changeover-content';
}
function readChoice() {
    var sel = document.getElementById('co-to-style');
    var panel = document.getElementById('changeover-preview');
    var body = document.getElementById('changeover-preview-body');
    return sel && sel.value ? {
        style: sel.value,
        preview: panel && body && panel.style.display !== 'none' ? body.innerHTML : null
    } : null;
}
function applyChoice(kept) {
    var sel = document.getElementById('co-to-style');
    if (!kept || !sel) return false;
    var offered = Array.prototype.some.call(sel.options, function(o) {
        return o.value === kept.style && !o.disabled;
    });
    if (!offered) return false;
    sel.value = kept.style;
    var panel = document.getElementById('changeover-preview');
    var body = document.getElementById('changeover-preview-body');
    if (kept.preview !== null && panel && body) {
        body.innerHTML = kept.preview;
        panel.style.display = '';
    }
    return true;
}
document.body.addEventListener('htmx:beforeSwap', function(evt) {
    // A failed reload is not swapped (no afterSwap/afterSettle follows), so
    // it must not arm a restore.
    if (!isChangeoverSwap(evt) || evt.detail.shouldSwap === false) return;
    if (!restorePending) keptChoice = readChoice();
    restorePending = true;
});
document.body.addEventListener('htmx:afterSwap', function(evt) {
    if (!isChangeoverSwap(evt) || !restorePending) return;
    if (!applyChoice(keptChoice)) keptChoice = null;
});
document.body.addEventListener('htmx:afterSettle', function(evt) {
    if (!isChangeoverSwap(evt) || !restorePending) return;
    applyChoice(keptChoice);
    keptChoice = null;
    restorePending = false;
});

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
    startProcessChangeover,
    switchStation
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
