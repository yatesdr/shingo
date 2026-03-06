async function confirmDelivery(orderID, qty) {
    try {
        await ShingoEdge.api.post('/api/confirm-delivery/' + orderID, { final_count: qty });
        ShingoEdge.toast('Delivery confirmed', 'success');
        location.reload();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function submitOrder(orderID) {
    try {
        await ShingoEdge.api.post('/api/orders/' + orderID + '/submit', {});
        ShingoEdge.toast('Order submitted', 'success');
        location.reload();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function releaseOrder(orderID) {
    try {
        await ShingoEdge.api.post('/api/orders/' + orderID + '/release', {});
        ShingoEdge.toast('Order released from staging', 'success');
        location.reload();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function abortOrder(orderID) {
    var ok = await ShingoEdge.confirm('Abort this order? This will cancel it and notify dispatch.');
    if (!ok) return;
    try {
        await ShingoEdge.api.post('/api/orders/' + orderID + '/abort', {});
        ShingoEdge.toast('Order aborted', 'success');
        location.reload();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function stageOrder(orderID, node) {
    try {
        await ShingoEdge.api.post('/api/orders/' + orderID + '/redirect', { delivery_node: node });
        ShingoEdge.toast('Order redirected', 'success');
        location.reload();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

function showRedirectDropdown(orderID) {
    document.getElementById('redirect-order-id').value = orderID;
    ShingoEdge.showModal('redirect-modal');
}

async function submitRedirect() {
    var orderID = document.getElementById('redirect-order-id').value;
    var node = document.getElementById('redirect-node').value;
    try {
        await ShingoEdge.api.post('/api/orders/' + orderID + '/redirect', { delivery_node: node });
        ShingoEdge.toast('Order redirected', 'success');
        ShingoEdge.hideModal('redirect-modal');
        location.reload();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// Staged bin expiry countdown
function updateCountdowns() {
    document.querySelectorAll('[data-countdown]').forEach(function(el) {
        var exp = new Date(el.getAttribute('data-countdown'));
        var now = new Date();
        var diff = exp - now;
        if (diff <= 0) {
            el.textContent = 'Expired';
            return;
        }
        var mins = Math.floor(diff / 60000);
        if (mins >= 60) {
            el.textContent = Math.floor(mins / 60) + 'h ' + (mins % 60) + 'm';
        } else {
            el.textContent = mins + 'm';
        }
    });
}
updateCountdowns();
setInterval(updateCountdowns, 60000);

// SSE with debounce
var _reloadTimer = null;
function debouncedReload() {
    if (_reloadTimer) clearTimeout(_reloadTimer);
    _reloadTimer = setTimeout(function() { location.reload(); }, 500);
}

ShingoEdge.createSSE('/events', {
    onOrderUpdate: function() { debouncedReload(); },
    onCounterAnomaly: function() { location.reload(); }
});
