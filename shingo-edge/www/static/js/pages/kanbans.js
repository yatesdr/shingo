// Order actions that need JSON bodies or confirm dialogs.
// SSE auto-refresh and HX-Trigger-based refresh are handled by htmx.

async function confirmDelivery(orderID, qty) {
    try {
        await ShingoEdge.api.post('/api/confirm-delivery/' + orderID, { final_count: qty });
        ShingoEdge.toast('Delivery confirmed', 'success');
        htmx.trigger(document.body, 'refreshOrders');
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function submitOrder(orderID) {
    try {
        await ShingoEdge.api.post('/api/orders/' + orderID + '/submit', {});
        ShingoEdge.toast('Order submitted', 'success');
        htmx.trigger(document.body, 'refreshOrders');
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function releaseOrder(orderID) {
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
    // Lineside-pull contract (unchanged from pre-fix): the kanbans admin
    // view doesn't know which parts were pulled to lineside (that's
    // captured at the operator station modal). If the operator pulled
    // parts, they should cancel here and use the operator-station UI to
    // record per-part quantities — this prompt only handles the bin's
    // remaining total, not per-part captures.
    const input = window.prompt(
        'How many parts remain in this bin?\n\n' +
        'Enter 0 (or leave blank) to release as EMPTY (manifest cleared).\n' +
        'Enter a positive number to release as PARTIAL (manifest preserved\n' +
        'with that count).\n\n' +
        'If you pulled parts to lineside during the swap, cancel and use\n' +
        'the operator station to record per-part captures.',
        ''
    );
    if (input === null) return; // operator cancelled
    const trimmed = String(input).trim();
    const partial = trimmed === '' ? 0 : Number(trimmed);
    if (!Number.isInteger(partial) || partial < 0) {
        ShingoEdge.toast('Invalid count: enter 0, blank, or a positive whole number', 'error');
        return;
    }
    const body = partial > 0
        ? { disposition: 'send_partial_back', partial_count: partial, called_by: 'admin-ui' }
        : { disposition: 'capture_lineside', qty_by_part: {}, called_by: 'admin-ui' };
    try {
        await ShingoEdge.api.post('/api/orders/' + orderID + '/release', body);
        ShingoEdge.toast(partial > 0
            ? 'Order released — partial (' + partial + ' parts preserved)'
            : 'Order released — empty (manifest cleared)', 'success');
        htmx.trigger(document.body, 'refreshOrders');
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function abortOrder(orderID) {
    if (!await ShingoEdge.confirm('Abort this order?')) return;
    try {
        await ShingoEdge.api.post('/api/orders/' + orderID + '/abort', {});
        ShingoEdge.toast('Order aborted', 'success');
        htmx.trigger(document.body, 'refreshOrders');
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// Staged bin expiry countdown
function updateCountdowns() {
    document.querySelectorAll('[data-countdown]').forEach(function(el) {
        var exp = new Date(el.getAttribute('data-countdown'));
        var diff = exp - new Date();
        if (diff <= 0) { el.textContent = 'Expired'; return; }
        var mins = Math.floor(diff / 60000);
        el.textContent = mins >= 60 ? Math.floor(mins / 60) + 'h ' + (mins % 60) + 'm' : mins + 'm';
    });
}
updateCountdowns();
setInterval(updateCountdowns, 60000);
