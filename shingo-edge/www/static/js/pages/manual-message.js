import { api } from '/static/js/shingoedge.js';
import { delegateActions, escapeHtml } from '/static/shared/dom.js';
import { nodePicker, nodeSource } from '/static/shared/node-picker.js';
import { toast } from '/static/shared/notice.js';

var _pd = document.getElementById('page-data').dataset;
var _stationID = _pd.stationId;
var _orders = JSON.parse(_pd.orders);
// The page's one node list (shared/node-picker.js), Core's names as the page
// was rendered with them. Every node field draws from it, and takes any node:
// this is a debug console that sends what it is told.
var _coreNodes = JSON.parse(_pd.coreNodes);
var _nodes = nodeSource(function() { return _coreNodes; }, _coreNodes);
// The node fields of the form now drawn, by field id.
var _nodePickers = {};

var _fieldDefs = {
    'edge.register': [
        { id: 'version', label: 'Version', type: 'text', value: 'dev' }
    ],
    'edge.heartbeat': [],
    'node.list_request': [],
    'order.request': [
        { id: 'order_uuid', label: 'Order UUID', type: 'text', value: function(){ return crypto.randomUUID(); } },
        { id: 'order_type', label: 'Order Type', type: 'select', options: ['retrieve','move'], value: 'retrieve' },
        { id: 'payload_desc', label: 'Payload Description', type: 'text', value: '' },
        { id: 'quantity', label: 'Quantity', type: 'number', value: '1' },
        { id: 'delivery_node', label: 'Delivery Node', type: 'nodeselect', value: '' },
        { id: 'source_node', label: 'Source Node', type: 'nodeselect', value: '' },
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
    ]
};

// renderField writes one field's markup. Every name and value from the plant
// (node names, the order uuid/type/status, a field's value) is escaped.
function renderField(f) {
    var val = typeof f.value === 'function' ? f.value() : f.value;
    var esc = escapeHtml;
    var html = '<div class="form-group"><label>' + esc(f.label) + '</label>';
    if (f.type === 'select') {
        html += '<select id="mm-f-' + f.id + '" class="form-input" data-action-change="updatePreview">';
        f.options.forEach(function(o){ html += '<option value="'+esc(o)+'"'+(o===val?' selected':'')+'>'+esc(o)+'</option>'; });
        html += '</select>';
    } else if (f.type === 'nodeselect') {
        html += '<div id="mm-f-' + f.id + '" data-node-field="' + f.id + '"></div>';
    } else if (f.type === 'orderselect') {
        html += '<select id="mm-f-' + f.id + '" class="form-input" data-action-change="updatePreview"><option value="">-- Select --</option>';
        _orders.forEach(function(o){ html += '<option value="'+esc(o.uuid)+'">'+esc(o.uuid.substring(0,8))+'... ('+esc(o.type)+', '+esc(o.status)+')</option>'; });
        html += '</select>';
    } else if (f.type === 'checkbox') {
        html += '<input type="checkbox" id="mm-f-' + f.id + '" '+(val?'checked':'')+' data-action-change="updatePreview">';
    } else {
        html += '<input type="' + f.type + '" id="mm-f-' + f.id + '" class="form-input" value="' + esc(val) + '" data-action-input="updatePreview">';
    }
    html += '</div>';
    return html;
}

function updateMessageForm() {
    var type = document.getElementById('mm-type').value;
    var fields = _fieldDefs[type] || [];
    var container = document.getElementById('mm-fields');
    container.innerHTML = fields.map(renderField).join('');
    _nodePickers = {};
    fields.forEach(function(f) {
        if (f.type !== 'nodeselect') return;
        _nodePickers[f.id] = nodePicker(document.getElementById('mm-f-' + f.id), _nodes, {},
            { placeholder: f.label, onChange: updatePreview });
    });
    document.getElementById('mm-status').textContent = '';
    updatePreview();
}

function getFieldValue(id) {
    if (_nodePickers[id]) return _nodePickers[id].value();
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
            return { version: getFieldValue('version') };
        case 'edge.heartbeat':
        case 'node.list_request':
            return {};
        case 'order.request':
            return {
                order_uuid: getFieldValue('order_uuid'),
                order_type: getFieldValue('order_type'),
                payload_desc: getFieldValue('payload_desc'),
                quantity: getFieldValue('quantity'),
                delivery_node: getFieldValue('delivery_node'),
                source_node: getFieldValue('source_node'),
                retrieve_empty: getFieldValue('retrieve_empty')
            };
        case 'order.cancel':
            return { order_uuid: getFieldValue('order_uuid'), reason: getFieldValue('reason') };
        case 'order.receipt':
            return { order_uuid: getFieldValue('order_uuid'), receipt_type: getFieldValue('receipt_type'), final_count: getFieldValue('final_count') };
        case 'order.redirect':
            return { order_uuid: getFieldValue('order_uuid'), new_delivery_node: getFieldValue('new_delivery_node') };
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
        var resp = await api.post('/api/manual-message', { type: type, payload: payload });
        statusEl.textContent = 'Sent (id: ' + (resp.msg_id || '').substring(0, 8) + '...)';
        statusEl.style.color = 'var(--color-success, green)';
        toast('Message sent', 'success');
    } catch (e) {
        statusEl.textContent = 'Failed: ' + e.message;
        statusEl.style.color = 'var(--color-danger, red)';
        toast('Error: ' + e.message, 'error');
    }
}

// Init
updateMessageForm();

// ─── delegated event handlers ─────────────────────────
// All page-level data-action verbs route through delegateActions
// on document.body. Multiple event types share the same handler
// map — most handlers are click-only but a few (e.g. updatePreview)
// are referenced via data-action-change / data-action-input too,
// so binding the map across every event type keeps the page wiring
// single-source.
delegateActions(document.body, {
    sendMessage,
    updateMessageForm,
    updatePreview
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
