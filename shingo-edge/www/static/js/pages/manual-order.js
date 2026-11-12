import { api } from '/static/js/shingoedge.js';
import { onSSE } from '/static/shared/bus.js';
import { delegateActions } from '/static/shared/dom.js';
import { nodePicker, nodeSource, nodeWhy } from '/static/shared/node-picker.js';
import { toast } from '/static/shared/notice.js';
import { postAct } from '/static/js/ask-act.js';

// THE PAGE'S ONE NODE LIST (shared/node-picker.js): Core's nodes as the page
// was rendered with them, read again from /api/core-nodes on an SSE
// core-nodes frame (the frame names nodes and types, not groups), and this
// Edge's process nodes Core does not list ("this Edge only"). A process node's
// name is the note on its Core node. The three node fields draw from it, each
// with its own rule.
var _pd = document.getElementById('page-data').dataset;
var edgeNodes = JSON.parse(_pd.nodes);
// memberOf is each member of a node group, by name, to its group.
var memberOf = {};
function withEdgeNodes(core) {
    var seen = {};
    memberOf = {};
    var rows = (core || []).filter(function(n) { return n && n.name; }).map(function(n) {
        seen[n.name] = true;
        if (n.group) memberOf[n.name] = n.group;
        var edge = edgeNodes.find(function(e) { return e.id === n.name; });
        return Object.assign({}, n, { desc: edge ? edge.desc : '' });
    });
    edgeNodes.forEach(function(e) {
        if (e.id && !seen[e.id]) { seen[e.id] = true; rows.push({ name: e.id, desc: e.desc, source: 'local' }); }
    });
    return rows;
}
var nodes = nodeSource(function() { return api.get('/api/core-nodes').then(withEdgeNodes); },
    withEdgeNodes(JSON.parse(_pd.coreNodes)));

// A MEMBER OF A GROUP IS CORE'S TO PICK: an order's pickup and delivery name
// the group (the API refuses a member). The delivery takes one only for a
// complex order, whose Core Node is the cell's own position. No field takes a
// robot.
var W = nodeWhy;
var pickers = {
    pickup: nodePicker(document.getElementById('mo-pickup'), nodes,
        { rule: W.any(W.robot, W.member) }, { placeholder: 'Source node' }),
    delivery: nodePicker(document.getElementById('mo-delivery'), nodes,
        { rule: W.any(W.robot, function(n) { return orderType() === 'complex' ? '' : W.member(n); }) },
        { placeholder: 'Delivery node' }),
    staging: nodePicker(document.getElementById('mo-staging'), nodes,
        { rule: W.robot }, { placeholder: 'Staging node' }),
};

function orderType() { return document.getElementById('mo-type').value; }

// Field visibility per order type:
//   move:     pickup, delivery
//   retrieve: process node (optional), delivery, staging (optional)
//   complex:  process node (optional), production (delivery label), staging
function updateOrderForm() {
    var t = orderType();
    if (t !== 'complex' && memberOf[pickers.delivery.value()]) pickers.delivery.set('');
    pickers.delivery.refresh();
    var showNode    = t !== 'move';
    var showPickup  = t === 'move';
    var showDeliv   = t === 'move' || t === 'retrieve' || t === 'complex';
    var showStaging = t === 'retrieve' || t === 'complex';

    document.getElementById('mo-node-group').style.display    = showNode    ? '' : 'none';
    document.getElementById('mo-pickup-group').style.display   = showPickup  ? '' : 'none';
    document.getElementById('mo-delivery-group').style.display = showDeliv   ? '' : 'none';
    document.getElementById('mo-staging-group').style.display  = showStaging ? '' : 'none';

    document.getElementById('mo-delivery-label').textContent =
        (t === 'complex') ? 'Core Node' : 'Delivery Node';

    // Clear hidden fields
    if (!showNode)    document.getElementById('mo-node').selectedIndex = 0;
    if (!showPickup)  pickers.pickup.set('');
    if (!showDeliv)   pickers.delivery.set('');
    if (!showStaging) pickers.staging.set('');

    autofillNodeDefaults();
}

// When a process node is selected, auto-fill the associated core node
// into the primary target field for the current order type.
function autofillNodeDefaults() {
    var sel = document.getElementById('mo-node');
    var opt = sel.options[sel.selectedIndex];
    if (!opt || !opt.value) return;
    var coreNode = opt.dataset.coreNode || '';
    if (!coreNode) return;
    var t = document.getElementById('mo-type').value;
    if (t === 'retrieve') {
        pickers.delivery.set(coreNode);
    }
}

async function createOrder() {
    var t = document.getElementById('mo-type').value;
    var processNodeID = parseInt(document.getElementById('mo-node').value) || 0;
    var qty = parseInt(document.getElementById('mo-qty').value) || 1;

    if (t === 'complex') {
        var stagingNode = pickers.staging.value();
        var productionNode = pickers.delivery.value();
        if (!stagingNode || !productionNode) {
            toast('Staging and production nodes are required', 'error');
            return;
        }
        var body = {
            process_node_id: processNodeID || null,
            quantity: qty,
            steps: [
                {action: 'pickup', node: stagingNode},
                {action: 'dropoff', node: stagingNode},
                {action: 'wait'},
                {action: 'pickup', node: stagingNode},
                {action: 'dropoff', node: productionNode}
            ]
        };
        try {
            await api.post('/api/orders/complex', body);
            toast('Complex order created', 'success');
            resetForm();
        } catch (e) { toast('Error: ' + e.message, 'error'); }
        return;
    }

    var body = {
        process_node_id: processNodeID || null,
        quantity: qty,
        delivery_node: pickers.delivery.value(),
        source_node: pickers.pickup.value(),
        staging_node: pickers.staging.value() || undefined
    };
    try {
        // At a loader's window the order is Core's to make (the loader ask): a
        // 202 pending answer is sent to Core, not known to be done (ask-act.js).
        var answer = await postAct('/api/orders/' + t, body);
        if (answer && answer.pending) { toast(answer.notice, 'info'); resetForm(); return; }
        toast('Order created', 'success');
        resetForm();
    } catch (e) { toast('Error: ' + e, 'error'); }
}

function resetForm() {
    document.getElementById('mo-node').selectedIndex = 0;
    document.getElementById('mo-qty').value = '1';
    pickers.pickup.set('');
    pickers.delivery.set('');
    pickers.staging.set('');
}

async function syncNodes() {
    var btn = document.getElementById('sync-nodes-btn');
    btn.disabled = true;
    btn.textContent = 'Syncing...';
    try {
        await api.post('/api/core-nodes/sync');
        toast('Node sync requested', 'success');
    } catch (e) { toast('Sync failed: ' + e.message, 'error'); }
    setTimeout(function() { btn.disabled = false; btn.textContent = 'Sync Nodes'; }, 2000);
}

// The page's one stream (shared/bus.js): Core's node list changed, so it is
// read again. A picked node that is no longer on it is unpicked, as a select
// left on no option sends none.
onSSE('core-nodes', function() {
    nodes.load(true).then(function(rows) {
        var names = {};
        (rows || []).forEach(function(n) { names[n.name] = true; });
        Object.keys(pickers).forEach(function(k) {
            if (pickers[k].value() && !names[pickers[k].value()]) pickers[k].set('');
        });
    });
});

// Apply initial visibility for default order type.
updateOrderForm();

// ─── delegated event handlers ─────────────────────────
// All page-level data-action verbs route through delegateActions
// on document.body. Multiple event types share the same handler
// map — most handlers are click-only but a few (e.g. updatePreview)
// are referenced via data-action-change / data-action-input too,
// so binding the map across every event type keeps the page wiring
// single-source.
delegateActions(document.body, {
    autofillNodeDefaults,
    createOrder,
    syncNodes,
    updateOrderForm
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
