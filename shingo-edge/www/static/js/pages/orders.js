import { api, navigateToProcessOrOrders, refreshOnEvents } from '/static/js/shingoedge.js';
import { onSSE } from '/static/shared/bus.js';
import { delegateActions } from '/static/shared/dom.js';
import { confirm, prompt } from '/static/shared/layer.js';
import { toast } from '/static/shared/notice.js';
import { typedNote } from '/static/shared/count-words.js';
import { uopRatio } from '/static/shared/count.js';

// Order actions that need JSON bodies or confirm dialogs.
// HX-Trigger-based refresh is htmx's; #orders-content's sse: triggers are
// fired from the page's one stream (shared/bus.js).
refreshOnEvents('orders-content', onSSE);

async function confirmDelivery(orderID, qty) {
    // qty arrives as a string from data-action="confirmDelivery:<id>:<qty>"
    // colon-arg dispatch; the Go handler decodes final_count as int64 and
    // rejects JSON string values with "cannot unmarshal string into ...
    // .final_count of type int64". Coerce before JSON.stringify.
    var n = parseInt(qty, 10) || 0;
    try {
        await api.post('/api/confirm-delivery/' + orderID, { final_count: n });
        toast('Delivery confirmed', 'success');
        htmx.trigger(document.body, 'refreshOrders');
    } catch (e) { toast('Error: ' + e.message, 'error'); }
}

async function submitOrder(orderID) {
    try {
        await api.post('/api/orders/' + orderID + '/submit', {});
        toast('Order submitted', 'success');
        htmx.trigger(document.body, 'refreshOrders');
    } catch (e) { toast('Error: ' + e.message, 'error'); }
}

async function releaseOrder(orderID, el) {
    // Prompt the operator for the remaining-parts count on the bin being
    // released. The number drives the disposition:
    //
    //   - empty input or 0   → DispositionCaptureLineside (manifest cleared)
    //   - positive integer N → DispositionSendPartialBack with PartialCount=N
    //                          (manifest synced to N, bin returns partial)
    //
    // Plant 2026-05-11 (SNF2 ALN_001): pre-fix this button hardcoded
    // capture_lineside with empty captures regardless of bin state, which
    // wiped manifests on partial bins when operators used this as an HMI
    // workaround. A bin with 3600 parts arrived at supermarket empty.
    // Prompting forces the operator to declare the count rather than
    // silently assuming empty.
    //
    // Lineside-pull contract (unchanged from pre-fix): the orders admin
    // view doesn't know which parts were pulled to lineside (that's
    // captured at the operator station modal). If the operator pulled
    // parts, they should cancel here and use the operator-station UI to
    // record per-part quantities — this prompt only handles the bin's
    // remaining total, not per-part captures.
    const input = await prompt(
        'How many parts remain in this bin?\n\n' +
        'Enter 0 (or leave blank) to release as EMPTY (manifest cleared).\n' +
        'Enter a positive number to release as PARTIAL (manifest preserved\n' +
        'with that count).\n\n' +
        'If you pulled parts to lineside during the swap, cancel and use\n' +
        'the operator station to record per-part captures.',
        { type: 'number', min: 0 }
    );
    if (input === null) return; // operator cancelled
    const trimmed = String(input).trim();
    const partial = trimmed === '' ? 0 : Number(trimmed);
    if (!Number.isInteger(partial) || partial < 0) {
        toast('Invalid count: enter 0, blank, or a positive whole number', 'warning');
        return;
    }
    const body = partial > 0
        ? { disposition: 'send_partial_back', partial_count: partial, called_by: 'admin-ui' }
        : { disposition: 'capture_lineside', qty_by_part: {}, called_by: 'admin-ui' };
    try {
        await postRelease(body);
        // The UoP the parts make above a ratio of 1, and the above-pack note.
        const t = partial > 0 ? await templateOf(el && el.dataset ? el.dataset.payloadCode : '') : null;
        const note = t ? typedNote(partial, t.ratio, t.pack) : null;
        toast((partial > 0
            ? 'Order released — partial (' + partial + ' parts preserved)'
            : 'Order released — empty (manifest cleared)') + (note ? ' ' + note.text : ''), note && note.tone ? 'warning' : 'success');
        htmx.trigger(document.body, 'refreshOrders');
    } catch (e) { toast('Error: ' + e.message, 'error'); }

    async function postRelease(b) {
        await api.post('/api/orders/' + orderID + '/release', b);
    }
}

// templateOf is the payload's pack and UOP ratio from the Edge's catalog copy,
// null when the code is blank or the read fails: the toast's notes are said
// when they are known and never stand in the release's way.
async function templateOf(code) {
    if (!code) return null;
    try {
        const data = await api.get('/api/payload/' + encodeURIComponent(code) + '/manifest');
        return { pack: (data && data.uop_capacity) || 0, ratio: uopRatio(data && data.items) };
    } catch (e) {
        return null;
    }
}

// The operator's word is Cancel (the owner, 2026-11-25): the button, this
// dialog and its toast. The dialog's own buttons name the choice, since a plain
// Cancel beside "Cancel this order?" would read as the order's cancel.
//
// The cancel asks Core (www apiCancelOrder): cancelled when Core has done it
// within the wait; Core's sentence, as the error, when it cannot; or pending
// with "Cancel sent; Core has not answered yet", and the row then says
// "Cancel sent" until Core's word ends it.
async function cancelOrder(orderID) {
    if (!await confirm('Cancel this order?', { ok: 'Cancel order', cancel: 'Keep order' })) return;
    try {
        const res = await api.post('/api/orders/' + orderID + '/cancel', {});
        if (res && res.pending) toast(res.notice, 'info');
        else toast('Order cancelled', 'success');
    } catch (e) { toast('Error: ' + e.message, 'error'); }
    // The rows redraw on every answer: Core's refusal changes the row too (its
    // status Core's, its sentence under it).
    htmx.trigger(document.body, 'refreshOrders');
}

// Staged-expiry and fault clocks tick from shared installLiveDurations
// (installed in shingoedge.js). This page's own minute-resolution countdown
// loop was a second implementation of the same thing; the staged span now
// carries data-until + data-past="Expired".

// ─── Client-side table filter (mirrors Core's orders.js) ───
function applyOrderSearch() {
    var input = document.getElementById('filter-search');
    var countEl = document.getElementById('filter-count');
    var table = document.getElementById('orders-table');
    if (!input || !table) return;
    var rows = table.querySelectorAll('tbody tr');
    var q = input.value.toLowerCase().trim();
    var visible = 0;
    for (var i = 0; i < rows.length; i++) {
        var text = rows[i].textContent.toLowerCase();
        var show = !q || text.indexOf(q) !== -1;
        rows[i].style.display = show ? '' : 'none';
        if (show) visible++;
    }
    if (countEl) countEl.textContent = q ? visible + ' of ' + rows.length : '';
}
applyOrderSearch();
// Re-apply on input, and re-run after htmx swaps the table partial.
var _searchInput = document.getElementById('filter-search');
if (_searchInput) {
    _searchInput.addEventListener('input', applyOrderSearch);
    document.body.addEventListener('htmx:afterSwap', applyOrderSearch);
}

// ─── delegated event handlers ─────────────────────────
// All page-level data-action verbs route through delegateActions
// on document.body. Multiple event types share the same handler
// map — most handlers are click-only but a few (e.g. updatePreview)
// are referenced via data-action-change / data-action-input too,
// so binding the map across every event type keeps the page wiring
// single-source.
// (updateCountdowns used to sit here; the staged-expiry clock moved to
// installLiveDurations in shingoedge.js and the leftover reference
// threw a ReferenceError at module load that killed every button
// handler on this page.)
delegateActions(document.body, {
    cancelOrder,
    confirmDelivery,
    navigateToProcessOrOrders,
    releaseOrder,
    submitOrder
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
