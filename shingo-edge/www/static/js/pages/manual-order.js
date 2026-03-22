// Merge core-synced nodes with edge-local nodes for the dropdowns.
// Core nodes take precedence; edge-local nodes fill in any extras.
(function() {
    var _pd = document.getElementById('page-data').dataset;
    var edgeNodes = JSON.parse(_pd.nodes);
    var coreNodesRaw = JSON.parse(_pd.coreNodes);
    function coreNodeName(entry) {
        if (!entry) return '';
        if (typeof entry === 'string') return entry;
        if (typeof entry.name === 'string') return entry.name;
        if (typeof entry.node_id === 'string') return entry.node_id;
        return '';
    }
    var coreNodes = coreNodesRaw.map(coreNodeName).filter(Boolean);

    // Build merged set: core nodes first, then any edge-only nodes
    var seen = {};
    var merged = [];
    coreNodes.sort().forEach(function(n) {
        seen[n] = true;
        // Find matching edge node for description
        var edge = edgeNodes.find(function(e){ return e.id === n; });
        merged.push({ id: n, desc: edge ? edge.desc : '', source: 'core' });
    });
    edgeNodes.forEach(function(e) {
        if (!seen[e.id]) {
            merged.push({ id: e.id, desc: e.desc, source: 'local' });
        }
    });

    // Populate all node selects
    ['mo-pickup', 'mo-delivery', 'mo-staging'].forEach(function(selID) {
        var sel = document.getElementById(selID);
        merged.forEach(function(n) {
            var opt = document.createElement('option');
            opt.value = n.id;
            opt.dataset.source = n.source;
            var label = n.id;
            if (n.desc) label += ' \u2014 ' + n.desc;
            if (n.source === 'local') label += ' (local)';
            opt.textContent = label;
            sel.appendChild(opt);
        });
    });
})();

function updateOrderForm() {
    var t = document.getElementById('mo-type').value;
    document.getElementById('mo-delivery-group').style.display = (t === 'retrieve' || t === 'move' || t === 'complex') ? '' : 'none';
    document.getElementById('mo-pickup-group').style.display = (t === 'store' || t === 'move') ? '' : 'none';
    document.getElementById('mo-staging-group').style.display = (t === 'complex') ? '' : 'none';
    // Update labels for complex mode
    document.getElementById('mo-delivery-label').textContent = (t === 'complex') ? 'Production Node' : 'Delivery Node';
    autofillNodeDefaults();
}

function autofillNodeDefaults() {
    var sel = document.getElementById('mo-node');
    var opt = sel.options[sel.selectedIndex];
    if (!opt || !opt.value) return;
    var t = document.getElementById('mo-type').value;
    if (t === 'retrieve') {
        document.getElementById('mo-delivery').value = opt.dataset.delivery || '';
    } else if (t === 'move') {
        document.getElementById('mo-delivery').value = '';
        document.getElementById('mo-pickup').value = opt.dataset.delivery || '';
    } else if (t === 'store') {
        document.getElementById('mo-pickup').value = opt.dataset.delivery || '';
    } else if (t === 'complex') {
        document.getElementById('mo-delivery').value = opt.dataset.delivery || '';
        document.getElementById('mo-staging').value = opt.dataset.staging || '';
    }
}

async function createOrder() {
    var t = document.getElementById('mo-type').value;
    var processNodeID = parseInt(document.getElementById('mo-node').value) || 0;

    if (t === 'complex') {
        var stagingNode = document.getElementById('mo-staging').value;
        var productionNode = document.getElementById('mo-delivery').value;
        if (!stagingNode || !productionNode) {
            ShingoEdge.toast('Staging and production nodes are required', 'error');
            return;
        }
        var body = {
            process_node_id: processNodeID || null,
            quantity: parseInt(document.getElementById('mo-qty').value) || 0,
            steps: [
                {action: 'pickup', node_group: stagingNode},
                {action: 'dropoff', node: stagingNode},
                {action: 'wait'},
                {action: 'pickup', node: stagingNode},
                {action: 'dropoff', node: productionNode}
            ]
        };
        try {
            await ShingoEdge.api.post('/api/orders/complex', body);
            ShingoEdge.toast('Complex order created', 'success');
            document.getElementById('mo-node').selectedIndex = 0;
            document.getElementById('mo-qty').value = '1';
            document.getElementById('mo-delivery').selectedIndex = 0;
            document.getElementById('mo-staging').selectedIndex = 0;
        } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
        return;
    }

    var body = {
        process_node_id: processNodeID || null,
        quantity: parseInt(document.getElementById('mo-qty').value) || 0,
        delivery_node: document.getElementById('mo-delivery').value,
        pickup_node: document.getElementById('mo-pickup').value
    };
    try {
        await ShingoEdge.api.post('/api/orders/' + t, body);
        ShingoEdge.toast('Order created', 'success');
        // Reset form
        document.getElementById('mo-node').selectedIndex = 0;
        document.getElementById('mo-qty').value = '1';
        document.getElementById('mo-delivery').selectedIndex = 0;
        document.getElementById('mo-pickup').selectedIndex = 0;
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function syncNodes() {
    var btn = document.getElementById('sync-nodes-btn');
    btn.disabled = true;
    btn.textContent = 'Syncing...';
    try {
        await ShingoEdge.api.post('/api/core-nodes/sync');
        ShingoEdge.toast('Node sync requested', 'success');
    } catch (e) { ShingoEdge.toast('Sync failed: ' + e, 'error'); }
    setTimeout(function() { btn.disabled = false; btn.textContent = 'Sync Nodes'; }, 2000);
}

ShingoEdge.createSSE('/events', {
    onCounterAnomaly: function() { location.reload(); },
    onCoreNodes: function(data) {
        var nodes = (data.nodes || []).map(function(n) {
            if (typeof n === 'string') return n;
            return n && n.name ? n.name : '';
        }).filter(Boolean);
        ['mo-pickup', 'mo-delivery', 'mo-staging'].forEach(function(selID) {
            var sel = document.getElementById(selID);
            var cur = sel.value;
            // Remove core-sourced options, keep local-only
            Array.from(sel.options).forEach(function(o) {
                if (o.dataset.source === 'core') o.remove();
            });
            // Re-add core nodes at the top (after blank first option if any)
            var ref = sel.options[1] || null;
            nodes.sort().forEach(function(n) {
                var opt = document.createElement('option');
                opt.value = n;
                opt.textContent = n;
                opt.dataset.source = 'core';
                sel.insertBefore(opt, ref);
            });
            sel.value = cur;
        });
    }
});
