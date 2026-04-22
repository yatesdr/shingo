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
    // Lineside phase 4: the kanbans admin view doesn't know which parts
    // were pulled to lineside (that's captured at the operator station
    // modal). Ask the operator to confirm they intend to release
    // without capturing anything first — if they did pull parts, the
    // operator-station UI is the right place to enter quantities.
    const msg = 'Release this order?\n\n' +
        'If you pulled parts to lineside during the swap, cancel and\n' +
        'use the operator station to record them. Releasing here\n' +
        'dispatches the bots without capturing anything.';
    if (!await ShingoEdge.confirm(msg)) return;
    try {
        await ShingoEdge.api.post('/api/orders/' + orderID + '/release', { qty_by_part: {} });
        ShingoEdge.toast('Order released', 'success');
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
