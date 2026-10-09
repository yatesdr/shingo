import { api, confirm, createSSE, delegateActions, toast } from '/static/js/shingoedge.js';

// Quality Containment page: Verify Good releases one verified bin from its
// containment node to the claim's outbound; Recall walks a contained
// payload's landed bins into containment. Both are confirm-gated because
// they create robot work.

async function verifyContainmentBin(el) {
    var node = el.dataset.node;
    var binID = parseInt(el.dataset.binId, 10);
    if (!node || !binID) return;
    if (!await confirmHolding('Release bin #' + binID + ' from ' + node + ' to the FG drop zone? Only confirm a bin you have verified good.')) return;
    try {
        await api.post('/api/containment/release', { node_name: node, bin_id: binID, actor: 'containment-screen' });
        toast('Bin #' + binID + ' released — robot dispatched', 'success');
        window.location.reload();
    } catch (e) {
        toast('Error: ' + e, 'error');
        applyHeldReload();
    }
}

async function recallContained(el) {
    var payload = el.dataset.payload;
    if (!payload) return;
    if (!await confirmHolding('Walk every bin of ' + payload + ' still sitting at its FG drop nodes into containment?')) return;
    try {
        var resp = await api.post('/api/containment/recall', { payload_code: payload, actor: 'containment-screen' });
        var created = (resp && resp.created) || 0;
        var refusals = (resp && resp.refusals) || [];
        if (refusals.length > 0) {
            toast('Recalled ' + created + ' bin(s); refused: ' + refusals[0], 'warning');
        } else {
            toast('Recalled ' + created + ' bin(s)', created > 0 ? 'success' : 'warning');
        }
        window.location.reload();
    } catch (e) {
        toast('Error: ' + e, 'error');
        applyHeldReload();
    }
}

async function unholdHeldBin(el) {
    var binID = parseInt(el.dataset.binId, 10);
    if (!binID) return;
    if (!await confirmHolding('Clear the hold on bin #' + binID + '? It returns to the ordinary flow — use only when the hold is stale or was a mistake.')) return;
    try {
        await api.post('/api/containment/unhold', { bin_id: binID });
        toast('Hold cleared on bin #' + binID, 'success');
        window.location.reload();
    } catch (e) {
        toast('Error: ' + e, 'error');
        applyHeldReload();
    }
}

delegateActions(document.body, {
    verifyContainmentBin,
    recallContained,
    unholdHeldBin,
}, { events: ['click'] });

// ── Refresh on Core's containment changes ────────────────────────────────
//
// The page renders the Edge's held copy of Core's containment feed. The Edge
// sends the SSE `containment` event when that copy changes — Core pushes it on
// every known containment write, and the heartbeat heals a missed push — and
// the page reloads on it. No poll: an unchanged copy sends nothing.
//
// A reload must never reach up and kill a verification the inspector is
// mid-way through, so while a dialog is open the reload is HELD, and applied
// when the dialog closes without acting (a confirmed verb reloads the page
// itself on success, and applies the held reload on failure).
//
// The page carries no inputs, so a reload is lossless — which is why the
// reload-based approach beats a client-side re-render here: one rendering
// path (the server template), zero drift between it and the page.
var _reloadHeld = false;

function dialogOpen() {
    return !!document.querySelector('.modal-overlay.active, .confirm-overlay');
}

function reloadOnContainment() {
    if (dialogOpen()) {
        _reloadHeld = true;
        return;
    }
    _reloadHeld = false;
    window.location.reload();
}

function applyHeldReload() {
    if (!_reloadHeld) return;
    _reloadHeld = false;
    window.location.reload();
}

// confirmHolding is confirm() for the page's verbs: a cancelled dialog applies
// any reload held while it was open.
async function confirmHolding(message) {
    var ok = await confirm(message);
    if (!ok) applyHeldReload();
    return ok;
}

createSSE('/events', { onContainment: reloadOnContainment });
