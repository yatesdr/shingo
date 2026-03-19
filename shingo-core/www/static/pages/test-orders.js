var authenticated = document.getElementById('page-data').dataset.authenticated === 'true';

function switchTab(name) {
  document.querySelectorAll('.to-tab').forEach(function(t) { t.classList.remove('active'); });
  document.querySelectorAll('.to-tab-content').forEach(function(c) { c.classList.remove('active'); });
  document.getElementById('tab-' + name).classList.add('active');
  event.target.classList.add('active');
}

function statusBadge(s) {
  if (!s) return '';
  return '<span class="to-status to-status-' + escapeHtml(s) + '">' + escapeHtml(s) + '</span>';
}

function fmtTime(t) {
  if (!t || t === '0001-01-01T00:00:00Z') return '-';
  try {
    var d = new Date(t);
    return d.toLocaleString();
  } catch(e) { return t; }
}

// --- Kafka order field visibility ---
function updateKafkaFields() {
  var t = document.getElementById('k-order-type').value;
  document.getElementById('k-pickup-wrap').style.display = (t === 'move' || t === 'store') ? '' : 'none';
  document.getElementById('k-delivery-wrap').style.display = (t === 'move' || t === 'retrieve') ? '' : 'none';
  document.getElementById('k-pt-wrap').style.display = (t === 'retrieve' || t === 'move') ? '' : 'none';
}

var scenePoints = []; // cached scene data

function updateCmdFields() {
  var t = document.getElementById('c-cmd-type').value;
  var orderCmds = ['move','jack','unjack','charge'];
  var needsLocation = orderCmds.indexOf(t) >= 0;
  var needsRobot = t !== 'terminate';

  document.getElementById('c-robot-wrap').style.display = needsRobot ? '' : 'none';
  document.getElementById('c-location-wrap').style.display = needsLocation ? '' : 'none';
  document.getElementById('c-config-wrap').style.display = (t === 'jack' || t === 'unjack') ? '' : 'none';
  document.getElementById('c-dispatch-wrap').style.display = (t === 'dispatchable') ? '' : 'none';
  document.getElementById('c-map-wrap').style.display = (t === 'switch_map') ? '' : 'none';
  document.getElementById('c-orderid-wrap').style.display = (t === 'terminate') ? '' : 'none';
  document.getElementById('c-container-wrap').style.display = (t === 'bind_goods' || t === 'unbind_container') ? '' : 'none';
  document.getElementById('c-goods-wrap').style.display = (t === 'bind_goods' || t === 'unbind_goods') ? '' : 'none';

  if (needsLocation) populateLocationDropdown(t);
}

function populateLocationDropdown(cmdType) {
  var sel = document.getElementById('c-location');
  sel.innerHTML = '<option value="">-- select --</option>';
  var filtered = scenePoints.filter(function(sp) {
    if (cmdType === 'charge') return sp.class_name === 'ChargePoint';
    return sp.class_name === 'GeneralLocation';
  });
  for (var i = 0; i < filtered.length; i++) {
    var sp = filtered[i];
    var opt = document.createElement('option');
    opt.value = sp.instance_name;
    opt.textContent = sp.instance_name + (sp.label ? ' (' + sp.label + ')' : '');
    sel.appendChild(opt);
  }
  if (filtered.length === 0) {
    sel.innerHTML = '<option value="">No locations synced</option>';
  }
}

async function loadScenePoints() {
  try {
    var res = await fetch('/api/test-orders/scene-points');
    if (!res.ok) return;
    scenePoints = await res.json();
    if (!scenePoints) scenePoints = [];
    populateLocationDropdown(document.getElementById('c-cmd-type').value);
  } catch(e) { console.error('load scene points:', e); }
}

// --- Render order tables ---
function renderOrdersTable(orders, containerId, isKafka) {
  var container = document.getElementById(containerId);
  if (!orders || orders.length === 0) {
    container.innerHTML = '<p style="color:#888;">No orders found.</p>';
    return;
  }
  var html = '<table class="table"><thead><tr>';
  html += '<th>ID</th><th>UUID</th><th>Type</th><th>Status</th><th>Vendor</th><th>Robot</th><th>From / To</th><th>Created</th>';
  if (authenticated) html += '<th>Actions</th>';
  html += '</tr></thead><tbody>';
  for (var i = 0; i < orders.length; i++) {
    var o = orders[i];
    var isActive = ['pending','sourcing','dispatched','in_transit','delivered'].indexOf(o.status) >= 0;
    var isDelivered = o.status === 'delivered';
    html += '<tr>';
    html += '<td>' + o.id + '</td>';
    html += '<td style="font-size:.8rem;">' + escapeHtml(o.edge_uuid) + '</td>';
    html += '<td>' + escapeHtml(o.order_type) + '</td>';
    html += '<td>' + statusBadge(o.status) + '</td>';
    html += '<td>' + statusBadge(o.vendor_state) + '</td>';
    html += '<td>' + escapeHtml(o.robot_id) + '</td>';
    html += '<td style="font-size:.85rem;">' + escapeHtml(o.pickup_node || '-') + ' &rarr; ' + escapeHtml(o.delivery_node || '-') + '</td>';
    html += '<td style="font-size:.8rem;">' + fmtTime(o.created_at) + '</td>';
    if (authenticated) {
      html += '<td class="to-actions">';
      html += '<button class="to-btn-sm" onclick="viewHistory(' + o.id + ')">History</button>';
      if (isKafka && isActive) {
        html += '<button class="to-btn-sm to-btn-danger" onclick="cancelOrder(\'' + escapeHtml(o.edge_uuid) + '\')">Cancel</button>';
      }
      if (isKafka && isDelivered) {
        html += '<button class="to-btn-sm" onclick="openReceipt(\'' + escapeHtml(o.edge_uuid) + '\')">Receipt</button>';
      }
      if (o.status === 'staged') {
        html += '<button class="to-btn-sm" style="border-color:#ff9800;color:#ff9800;" onclick="releaseComplexOrder(\'' + escapeHtml(o.edge_uuid) + '\')">Release</button>';
      }
      html += '</td>';
    }
    html += '</tr>';
  }
  html += '</tbody></table>';
  container.innerHTML = html;
}

function renderCommandsTable(cmds) {
  var container = document.getElementById('commands-table');
  if (!cmds || cmds.length === 0) {
    container.innerHTML = '<p style="color:#888;">No commands found.</p>';
    return;
  }
  var html = '<table class="table"><thead><tr>';
  html += '<th>ID</th><th>Type</th><th>Robot</th><th>Location</th><th>Vendor Order</th><th>State</th><th>Created</th>';
  if (authenticated) html += '<th>Actions</th>';
  html += '</tr></thead><tbody>';
  for (var i = 0; i < cmds.length; i++) {
    var c = cmds[i];
    html += '<tr>';
    html += '<td>' + c.ID + '</td>';
    html += '<td>' + escapeHtml(c.CommandType) + '</td>';
    html += '<td>' + escapeHtml(c.RobotID) + '</td>';
    html += '<td>' + escapeHtml(c.Location) + '</td>';
    html += '<td style="font-size:.8rem;">' + escapeHtml(c.VendorOrderID) + '</td>';
    html += '<td>' + statusBadge(c.VendorState) + '</td>';
    html += '<td style="font-size:.8rem;">' + fmtTime(c.CreatedAt) + '</td>';
    if (authenticated) {
      html += '<td class="to-actions">';
      if (!c.CompletedAt) {
        html += '<button class="to-btn-sm" onclick="refreshCmdStatus(' + c.ID + ')">Refresh</button>';
      }
      html += '</td>';
    }
    html += '</tr>';
  }
  html += '</tbody></table>';
  container.innerHTML = html;
}

// --- Data loading ---
async function refreshKafkaOrders() {
  try {
    var res = await fetch('/api/test-orders');
    if (!res.ok) { console.error('refresh kafka orders: HTTP', res.status); return; }
    var data = await res.json();
    renderOrdersTable(data, 'kafka-orders-table', true);
  } catch(e) { console.error('refresh kafka orders:', e); }
}

async function refreshDirectOrders() {
  try {
    var res = await fetch('/api/test-orders/direct');
    if (!res.ok) { console.error('refresh direct orders: HTTP', res.status); return; }
    var data = await res.json();
    renderOrdersTable(data, 'direct-orders-table', false);
  } catch(e) { console.error('refresh direct orders:', e); }
}

async function refreshCommands() {
  try {
    var res = await fetch('/api/test-commands');
    if (!res.ok) { console.error('refresh commands: HTTP', res.status); return; }
    var data = await res.json();
    renderCommandsTable(data);
  } catch(e) { console.error('refresh commands:', e); }
}

async function loadRobots() {
  try {
    var res = await fetch('/api/test-orders/robots');
    if (!res.ok) { document.getElementById('c-robot').innerHTML = '<option value="">Unavailable</option>'; return; }
    var robots = await res.json();
    var sel = document.getElementById('c-robot');
    sel.innerHTML = '<option value="">-- select --</option>';
    for (var i = 0; i < robots.length; i++) {
      var r = robots[i];
      var opt = document.createElement('option');
      opt.value = r.VehicleID;
      opt.textContent = r.VehicleID + (r.Connected ? '' : ' (offline)');
      sel.appendChild(opt);
    }
  } catch(e) { console.error('load robots:', e); }
}

// --- Kafka order actions ---
async function submitKafkaOrder() {
  var body = {
    order_type: document.getElementById('k-order-type').value,
    pickup_node: document.getElementById('k-pickup-node').value,
    delivery_node: document.getElementById('k-delivery-node').value,
    payload_code: document.getElementById('k-payload').value,
    quantity: parseInt(document.getElementById('k-quantity').value) || 1,
    priority: parseInt(document.getElementById('k-priority').value) || 0
  };
  if (!body.payload_code) { alert('Payload is required'); return; }
  try {
    var res = await fetch('/api/test-orders/submit', { method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body) });
    var data = await res.json();
    if (!res.ok) { alert(data.error || 'Error'); return; }
    alert('Order submitted: ' + data.order_uuid);
    refreshKafkaOrders();
  } catch(e) { alert('Error: ' + e); }
}

async function cancelOrder(uuid) {
  if (!confirm('Cancel order ' + uuid + '?')) return;
  try {
    var res = await fetch('/api/test-orders/cancel', { method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({order_uuid: uuid, reason: 'cancelled via test page'}) });
    var data = await res.json();
    if (!res.ok) { alert(data.error || 'Error'); return; }
    refreshKafkaOrders();
  } catch(e) { alert('Error: ' + e); }
}

function openReceipt(uuid) {
  document.getElementById('receipt-uuid').value = uuid;
  document.getElementById('receipt-type').value = 'full';
  document.getElementById('receipt-count').value = '1';
  showModal('receipt-modal');
}

async function sendReceipt() {
  var body = {
    order_uuid: document.getElementById('receipt-uuid').value,
    receipt_type: document.getElementById('receipt-type').value,
    final_count: parseInt(document.getElementById('receipt-count').value) || 0
  };
  try {
    var res = await fetch('/api/test-orders/receipt', { method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body) });
    var data = await res.json();
    if (!res.ok) { alert(data.error || 'Error'); return; }
    hideModal('receipt-modal');
    refreshKafkaOrders();
  } catch(e) { alert('Error: ' + e); }
}

// --- Direct order actions ---
async function submitDirectOrder() {
  var body = {
    from_node_id: parseInt(document.getElementById('d-from-node').value),
    to_node_id: parseInt(document.getElementById('d-to-node').value),
    priority: parseInt(document.getElementById('d-priority').value) || 0
  };
  if (!body.from_node_id || !body.to_node_id) { alert('Select both from and to nodes'); return; }
  try {
    var res = await fetch('/api/test-orders/direct', { method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body) });
    var data = await res.json();
    if (!res.ok) { alert(data.error || 'Error'); return; }
    alert('Direct order dispatched: ' + data.vendor_order_id);
    refreshDirectOrders();
  } catch(e) { alert('Error: ' + e); }
}

// --- Robot command actions ---
async function submitCommand() {
  var cmdType = document.getElementById('c-cmd-type').value;
  var body = {
    command_type: cmdType,
    robot_id: document.getElementById('c-robot').value,
    location: document.getElementById('c-location').value,
    config_id: document.getElementById('c-config-id') ? document.getElementById('c-config-id').value : '',
    dispatch_type: document.getElementById('c-dispatch-type').value,
    map_name: document.getElementById('c-map-name').value,
    order_id: document.getElementById('c-order-id').value,
    container_name: document.getElementById('c-container-name').value,
    goods_id: document.getElementById('c-goods-id').value
  };
  if (cmdType !== 'terminate' && !body.robot_id) { alert('Select a robot'); return; }
  try {
    var res = await fetch('/api/test-commands/submit', { method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body) });
    var data = await res.json();
    if (!res.ok) { alert(data.error || 'Error'); return; }
    var msg = data.vendor_order_id ? 'Order created: ' + data.vendor_order_id : 'Command ' + (data.status || 'sent');
    alert(msg);
    refreshCommands();
  } catch(e) { alert('Error: ' + e); }
}

async function refreshCmdStatus(id) {
  try {
    var res = await fetch('/api/test-commands/status?id=' + id);
    var data = await res.json();
    if (!res.ok) { alert(data.error || 'Error'); return; }
    refreshCommands();
  } catch(e) { alert('Error: ' + e); }
}

// --- History modal ---
async function viewHistory(orderId) {
  document.getElementById('hist-order-id').textContent = '#' + orderId;
  document.getElementById('hist-body').innerHTML = '<p style="color:#888;">Loading...</p>';
  showModal('history-modal');
  try {
    var res = await fetch('/api/test-orders/detail?id=' + orderId);
    var data = await res.json();
    if (!res.ok) { document.getElementById('hist-body').innerHTML = '<p style="color:red;">' + escapeHtml(data.error || 'Error') + '</p>'; return; }

    var html = '';
    if (data.order) {
      var o = data.order;
      html += '<div style="margin-bottom:1rem;font-size:.9rem;">';
      html += '<strong>UUID:</strong> ' + escapeHtml(o.edge_uuid) + '<br>';
      html += '<strong>Type:</strong> ' + escapeHtml(o.order_type) + ' &nbsp; <strong>Status:</strong> ' + statusBadge(o.status) + '<br>';
      html += '<strong>Vendor:</strong> ' + escapeHtml(o.vendor_order_id || '-') + ' ' + statusBadge(o.vendor_state) + '<br>';
      html += '<strong>Robot:</strong> ' + escapeHtml(o.robot_id || '-') + '<br>';
      html += '<strong>Route:</strong> ' + escapeHtml(o.pickup_node || '-') + ' &rarr; ' + escapeHtml(o.delivery_node || '-') + '<br>';
      if (o.error_detail) html += '<strong>Error:</strong> <span style="color:#dc3545;">' + escapeHtml(o.error_detail) + '</span><br>';
      html += '</div>';
    }

    if (data.history && data.history.length > 0) {
      html += '<table class="table"><thead><tr><th>Status</th><th>Detail</th><th>Time</th></tr></thead><tbody>';
      for (var i = 0; i < data.history.length; i++) {
        var h = data.history[i];
        html += '<tr><td>' + statusBadge(h.status) + '</td><td style="font-size:.85rem;">' + escapeHtml(h.detail) + '</td><td style="font-size:.8rem;">' + fmtTime(h.created_at) + '</td></tr>';
      }
      html += '</tbody></table>';
    } else {
      html += '<p style="color:#888;">No history entries.</p>';
    }
    document.getElementById('hist-body').innerHTML = html;
  } catch(e) { document.getElementById('hist-body').innerHTML = '<p style="color:red;">Error: ' + escapeHtml(String(e)) + '</p>'; }
}

// --- Toast notifications ---
function showToast(msg, type) {
  var toast = document.createElement('div');
  toast.className = 'to-toast to-toast-' + (type || 'info');
  toast.textContent = msg;
  document.body.appendChild(toast);
  setTimeout(function() { toast.classList.add('to-toast-visible'); }, 10);
  setTimeout(function() {
    toast.classList.remove('to-toast-visible');
    setTimeout(function() { toast.remove(); }, 300);
  }, 6000);
}

// --- SSE ---
var es = new EventSource('/events');
es.addEventListener('order-update', function(e) {
  refreshKafkaOrders();
  refreshDirectOrders();
  try {
    var data = JSON.parse(e.data);
    if (data.type === 'failed') {
      showToast('Order #' + (data.order_id || '?') + ' failed: ' + (data.detail || 'unknown error'), 'error');
    }
  } catch(ex) {}
});

// --- Complex Orders ---

function updateComplexFields() {
  var mode = document.getElementById('cx-cycle-mode').value;
  document.getElementById('cx-staging1-wrap').style.display = (mode === 'two_robot' || mode === 'single_robot') ? '' : 'none';
  document.getElementById('cx-staging2-wrap').style.display = (mode === 'single_robot') ? '' : 'none';
  document.getElementById('cx-pickup-wrap').style.display = (mode === 'two_robot' || mode === 'single_robot') ? '' : 'none';
}

async function submitComplexOrder() {
  var body = {
    cycle_mode: document.getElementById('cx-cycle-mode').value,
    location: document.getElementById('cx-location').value,
    payload_code: document.getElementById('cx-payload').value,
    full_pickup: document.getElementById('cx-full-pickup').value,
    staging_node: document.getElementById('cx-staging1').value,
    staging_node_2: document.getElementById('cx-staging2').value,
    outgoing_dest: document.getElementById('cx-outgoing').value,
    priority: parseInt(document.getElementById('cx-priority').value) || 0
  };
  if (!body.location) { alert('Lineside location is required'); return; }
  try {
    var res = await fetch('/api/test-orders/direct/complex', {
      method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body)
    });
    var data = await res.json();
    if (res.ok) {
      var uuids = (data.orders || []).map(function(o) { return o.role + ': ' + o.order_uuid; }).join(', ');
      showToast('Complex order created: ' + uuids, 'info');
      refreshComplexOrders();
    } else {
      showToast('Error: ' + (data.error || 'unknown'), 'error');
    }
  } catch(e) { showToast('Error: ' + e, 'error'); }
}

async function releaseComplexOrder(orderUUID) {
  try {
    var res = await fetch('/api/test-orders/direct/release', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({ order_uuid: orderUUID })
    });
    var data = await res.json();
    if (res.ok) {
      showToast('Released: ' + orderUUID, 'info');
      refreshComplexOrders();
    } else {
      showToast('Error: ' + (data.error || 'unknown'), 'error');
    }
  } catch(e) { showToast('Error: ' + e, 'error'); }
}

async function refreshComplexOrders() {
  try {
    var res = await fetch('/api/test-orders/direct');
    if (!res.ok) return;
    var orders = await res.json();
    renderOrdersTable(orders, 'complex-orders-table', false);
  } catch(e) {}
}

// --- Init ---
document.addEventListener('DOMContentLoaded', function() {
  updateKafkaFields();
  updateCmdFields();
  updateComplexFields();
  refreshKafkaOrders();
  refreshDirectOrders();
  refreshComplexOrders();
  refreshCommands();
  if (authenticated) { loadRobots(); loadScenePoints(); }
});
