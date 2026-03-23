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
    try {
        await ShingoEdge.api.post('/api/orders/' + orderID + '/release', {});
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
