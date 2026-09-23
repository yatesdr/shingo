import { api, confirm, delegateActions, toast } from '/static/js/shingoedge.js';

// Quality Containment page: Verify Good releases one verified bin from its
// containment node to the claim's outbound; Recall walks a contained
// payload's landed bins into containment. Both are confirm-gated because
// they create robot work.

async function verifyContainmentBin(el) {
    var node = el.dataset.node;
    var binID = parseInt(el.dataset.binId, 10);
    if (!node || !binID) return;
    if (!await confirm('Release bin #' + binID + ' from ' + node + ' to the FG drop zone? Only confirm a bin you have verified good.')) return;
    try {
        await api.post('/api/containment/release', { node_name: node, bin_id: binID, actor: 'containment-screen' });
        toast('Bin #' + binID + ' released — robot dispatched', 'success');
        window.location.reload();
    } catch (e) {
        toast('Error: ' + e, 'error');
    }
}

async function recallContained(el) {
    var payload = el.dataset.payload;
    if (!payload) return;
    if (!await confirm('Walk every bin of ' + payload + ' still sitting at its FG drop nodes into containment?')) return;
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
    }
}

delegateActions(document.body, {
    verifyContainmentBin,
    recallContained,
}, { events: ['click'] });

// ── Auto-refresh ─────────────────────────────────────────────────────────
//
// The page polls its own state endpoint and RELOADS ONLY WHEN THE STATE
// CHANGED — so a cleared containment flag or a bin arriving/departing shows
// up within seconds, while an unchanged page never flickers. Guards:
//
//   - the first poll is the BASELINE only (a kiosk opening would otherwise
//     reload itself once for no reason);
//   - a confirm dialog being open defers the reload to the next tick — a
//     refresh must never reach up and kill a verification the inspector is
//     mid-way through;
//   - a hidden tab skips its turns (background polling buys nothing);
//   - a failed poll says nothing: the next one is five seconds away.
//
// The page carries no inputs, so a reload is lossless — which is why the
// reload-based approach beats a client-side re-render here: one rendering
// path (the server template), zero drift between it and the page.
var _lastState = '';

setInterval(async function() {
    if (document.hidden) return;
    try {
        var res = await fetch('/api/containment/state');
        if (!res.ok) return;
        var text = await res.text();
        if (text === _lastState) return;
        var baseline = _lastState === '';
        _lastState = text;
        if (baseline) return;
        if (document.querySelector('.modal-overlay.active')) return;
        window.location.reload();
    } catch (e) { /* the next poll retries */ }
}, 5000);
