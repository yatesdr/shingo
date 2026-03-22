var _pd = document.getElementById('page-data').dataset;
var _stationID = _pd.stationId;
var _lineIDs = JSON.parse(_pd.lineIds);
var _orders = JSON.parse(_pd.orders);
var _coreNodes = JSON.parse(_pd.coreNodes).map(function(n) {
    if (typeof n === 'string') return n;
    if (n && typeof n.name === 'string') return n.name;
    if (n && typeof n.node_id === 'string') return n.node_id;
    return '';
}).filter(Boolean);

var _fieldDefs = {
    'edge.register': [
        { id: 'version', label: 'Version', type: 'text', value: 'dev' },
        { id: 'line_ids', label: 'Line IDs (comma-separated)', type: 'text', value: function(){ return _lineIDs.join(','); } }
    ],
    'edge.heartbeat': [
        { id: 'uptime', label: 'Uptime (seconds)', type: 'number', value: '0' }
    ],
    'production.report': [
        { id: 'cat_id', label: 'Cat ID', type: 'text', value: '' },
        { id: 'count', label: 'Count', type: 'number', value: '1' }
    ],
    'node.list_request': [],
    'order.request': [
        { id: 'order_uuid', label: 'Order UUID', type: 'text', value: function(){ return crypto.randomUUID(); } },
        { id: 'order_type', label: 'Order Type', type: 'select', options: ['retrieve','store','move'], value: 'retrieve' },
        { id: 'payload_desc', label: 'Payload Description', type: 'text', value: '' },
        { id: 'quantity', label: 'Quantity', type: 'number', value: '1' },
        { id: 'delivery_node', label: 'Delivery Node', type: 'nodeselect', value: '' },
        { id: 'pickup_node', label: 'Pickup Node', type: 'nodeselect', value: '' },
        { id: 'staging_node', label: 'Staging Node', type: 'nodeselect', value: '' },
        { id: 'load_type', label: 'Load Type', type: 'text', value: '' },
        { id: 'retrieve_empty', label: 'Retrieve Empty', type: 'checkbox', value: false }
    ],
    'order.cancel': [
        { id: 'order_uuid', label: 'Order UUID', type: 'orderselect', value: '' },
        { id: 'reason', label: 'Reason', type: 'text', value: '' }
    ],
    'order.receipt': [
        { id: 'order_uuid', label: 'Order UUID', type: 'orderselect', value: '' },
        { id: 'receipt_type', label: 'Receipt Type', type: 'text', value: 'delivery' },
        { id: 'final_count', label: 'Final Count', type: 'number', value: '0' }
    ],
    'order.redirect': [
        { id: 'order_uuid', label: 'Order UUID', type: 'orderselect', value: '' },
        { id: 'new_delivery_node', label: 'New Delivery Node', type: 'nodeselect', value: '' }
    ],
    'order.storage_waybill': [
        { id: 'order_uuid', label: 'Order UUID', type: 'orderselect', value: '' },
        { id: 'payload_desc', label: 'Payload Description', type: 'text', value: '' },
        { id: 'pickup_node', label: 'Pickup Node', type: 'nodeselect', value: '' },
        { id: 'final_count', label: 'Final Count', type: 'number', value: '0' }
    ]
};

function renderField(f) {
    var val = typeof f.value === 'function' ? f.value() : f.value;
    var html = '<div class="form-group"><label>' + f.label + '</label>';
    if (f.type === 'select') {
        html += '<select id="mm-f-' + f.id + '" class="form-input" onchange="updatePreview()">';
        f.options.forEach(function(o){ html += '<option value="'+o+'"'+(o===val?' selected':'')+'>'+o+'</option>'; });
        html += '</select>';
    } else if (f.type === 'nodeselect') {
        html += '<select id="mm-f-' + f.id + '" class="form-input" onchange="updatePreview()"><option value="">-- Select --</option>';
        _coreNodes.sort().forEach(function(n){ html += '<option value="'+n+'">'+n+'</option>'; });
        html += '</select>';
    } else if (f.type === 'orderselect') {
        html += '<select id="mm-f-' + f.id + '" class="form-input" onchange="updatePreview()"><option value="">-- Select --</option>';
        _orders.forEach(function(o){ html += '<option value="'+o.uuid+'">'+o.uuid.substring(0,8)+'... ('+o.type+', '+o.status+')</option>'; });
        html += '</select>';
    } else if (f.type === 'checkbox') {
        html += '<input type="checkbox" id="mm-f-' + f.id + '" '+(val?'checked':'')+' onchange="updatePreview()">';
    } else {
        html += '<input type="' + f.type + '" id="mm-f-' + f.id + '" class="form-input" value="' + val + '" oninput="updatePreview()">';
    }
    html += '</div>';
    return html;
}

function updateMessageForm() {
    var type = document.getElementById('mm-type').value;
    var fields = _fieldDefs[type] || [];
    var container = document.getElementById('mm-fields');
    container.innerHTML = fields.map(renderField).join('');
    document.getElementById('mm-status').textContent = '';
    updatePreview();
}

function getFieldValue(id) {
    var el = document.getElementById('mm-f-' + id);
    if (!el) return undefined;
    if (el.type === 'checkbox') return el.checked;
    if (el.type === 'number') return parseFloat(el.value) || 0;
    return el.value;
}

function buildPayload() {
    var type = document.getElementById('mm-type').value;
    switch (type) {
        case 'edge.register':
            return { version: getFieldValue('version'), line_ids: (getFieldValue('line_ids')||'').split(',').map(function(s){return s.trim()}).filter(Boolean) };
        case 'edge.heartbeat':
            return { uptime: getFieldValue('uptime') };
        case 'production.report':
            return { entries: [{ cat_id: getFieldValue('cat_id'), count: getFieldValue('count') }] };
        case 'node.list_request':
            return {};
        case 'order.request':
            return {
                order_uuid: getFieldValue('order_uuid'),
                order_type: getFieldValue('order_type'),
                payload_desc: getFieldValue('payload_desc'),
                quantity: getFieldValue('quantity'),
                delivery_node: getFieldValue('delivery_node'),
                pickup_node: getFieldValue('pickup_node'),
                staging_node: getFieldValue('staging_node'),
                load_type: getFieldValue('load_type'),
                retrieve_empty: getFieldValue('retrieve_empty')
            };
        case 'order.cancel':
            return { order_uuid: getFieldValue('order_uuid'), reason: getFieldValue('reason') };
        case 'order.receipt':
            return { order_uuid: getFieldValue('order_uuid'), receipt_type: getFieldValue('receipt_type'), final_count: getFieldValue('final_count') };
        case 'order.redirect':
            return { order_uuid: getFieldValue('order_uuid'), new_delivery_node: getFieldValue('new_delivery_node') };
        case 'order.storage_waybill':
            return { order_uuid: getFieldValue('order_uuid'), payload_desc: getFieldValue('payload_desc'), pickup_node: getFieldValue('pickup_node'), final_count: getFieldValue('final_count') };
        default:
            return {};
    }
}

function updatePreview() {
    var type = document.getElementById('mm-type').value;
    var payload = buildPayload();
    var preview = { type: type, src: { role: 'edge', station: _stationID }, dst: { role: 'core' }, payload: payload };
    document.getElementById('mm-preview').textContent = JSON.stringify(preview, null, 2);
}

async function sendMessage() {
    var type = document.getElementById('mm-type').value;
    var payload = buildPayload();
    var statusEl = document.getElementById('mm-status');
    statusEl.textContent = 'Sending...';
    statusEl.style.color = 'var(--text-muted)';
    try {
        var resp = await ShingoEdge.api.post('/api/manual-message', { type: type, payload: payload });
        statusEl.textContent = 'Sent (id: ' + (resp.msg_id || '').substring(0, 8) + '...)';
        statusEl.style.color = 'var(--color-success, green)';
        ShingoEdge.toast('Message sent', 'success');
    } catch (e) {
        statusEl.textContent = 'Failed: ' + e;
        statusEl.style.color = 'var(--color-danger, red)';
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

// Init
updateMessageForm();

ShingoEdge.createSSE('/events', {
    onCounterAnomaly: function() { location.reload(); }
});
