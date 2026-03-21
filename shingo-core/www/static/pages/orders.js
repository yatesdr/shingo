function orderControlPost(url, body) {
  var msg = document.getElementById('order-status-msg');
  if (msg) msg.textContent = 'Sending...';
  fetch(url, {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body)})
    .then(function(r) { return r.json().then(function(d) { return {ok:r.ok, data:d}; }); })
    .then(function(r) {
      if (msg) msg.textContent = r.ok ? 'OK - reloading...' : (r.data.error || 'Error');
      if (r.ok) setTimeout(function() { location.reload(); }, 800);
    })
    .catch(function(e) {
      if (msg) msg.textContent = 'Network error';
    });
}

function terminateOrder(id) {
  if (!confirm('Terminate order #' + id + '? This cannot be undone.')) return;
  orderControlPost('/api/orders/terminate', {order_id: id});
}

function setOrderPriority(id) {
  var p = parseInt(document.getElementById('order-priority').value, 10);
  if (isNaN(p)) return;
  orderControlPost('/api/orders/priority', {order_id: id, priority: p});
}

// --- Order detail modal ---
var _orderModalID = null;

function openOrderModal(id) {
  _orderModalID = id;
  var title = document.getElementById('order-modal-title');
  var loading = document.getElementById('order-modal-loading');
  var content = document.getElementById('order-modal-content');
  var errEl = document.getElementById('order-modal-error');
  title.textContent = 'Order #' + id;
  loading.style.display = '';
  content.style.display = 'none';
  errEl.style.display = 'none';
  showModal('order-modal-overlay');

  fetch('/api/orders/enriched?id=' + id)
    .then(function(r) { return r.json().then(function(d) { return {ok:r.ok, data:d}; }); })
    .then(function(r) {
      if (!r.ok) {
        loading.style.display = 'none';
        errEl.style.display = '';
        errEl.textContent = r.data.error || 'Failed to load order';
        return;
      }
      loading.style.display = 'none';
      content.style.display = '';
      renderOrderModal(r.data);
    })
    .catch(function() {
      loading.style.display = 'none';
      errEl.style.display = '';
      errEl.textContent = 'Network error';
    });
}

function closeOrderModal() {
  _orderModalID = null;
  hideModal('order-modal-overlay');
}

document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape' && _orderModalID) closeOrderModal();
});

function fmtTime(s) {
  if (!s) return '-';
  var d = new Date(s);
  return d.toLocaleString();
}

function field(label, val, cls) {
  return '<div class="manifest-field' + (cls ? ' ' + cls : '') + '"><label><strong>' + label + '</strong></label><span>' + val + '</span></div>';
}
function fieldH(label, val, cls) { return field(label, escapeHtml(val || '-'), cls); }

function renderOrderModal(data) {
  var o = data.order;
  var h = '<div class="manifest">';

  // ── HEADER ──
  // Title is set on the modal <h2> already; build the status line + identity here
  h += '<div class="manifest-head">';
  // Status line: badge + error together
  h += '<div style="margin-bottom:0.25rem">';
  h += '<span class="badge badge-' + o.status + '">' + escapeHtml(o.status) + '</span>';
  if (o.error_detail) h += ' <span style="color:var(--danger);font-size:0.82rem">' + escapeHtml(o.error_detail) + '</span>';
  h += '</div>';
  // UUID + type
  h += '<div class="manifest-uuid"><strong>UUID:</strong> ' + escapeHtml(o.edge_uuid) + ' (' + escapeHtml(o.order_type) + ')</div>';
  // Station + priority
  h += '<div class="manifest-meta"><span><strong>Originating Station:</strong> ' + escapeHtml(o.station_id) + ' (Priority: ' + o.priority + ')</span></div>';
  if (o.payload_desc) {
    h += '<div class="manifest-meta"><span><strong>Description:</strong> ' + escapeHtml(o.payload_desc) + '</span></div>';
  }
  // Timestamps
  h += '<div class="manifest-meta">';
  h += '<span><strong>Created:</strong> ' + fmtTime(o.created_at) + '</span>';
  h += '<span><strong>Modified:</strong> ' + fmtTime(o.updated_at) + '</span>';
  if (o.completed_at) h += '<span><strong>Completed:</strong> ' + fmtTime(o.completed_at) + '</span>';
  if (o.parent_order_id) h += '<span><strong>Parent:</strong> <a href="#" onclick="event.preventDefault();openOrderModal(' + o.parent_order_id + ')">#' + o.parent_order_id + '</a> (step ' + o.sequence + ')</span>';
  h += '</div></div>';

  // ── ROUTING ──
  h += '<div class="manifest-row">';
  h += '<div>';
  h += field('Pickup', escapeHtml(o.pickup_node || '-') + (data.pickup_node && data.pickup_node.zone ? ' <span style="color:var(--text-muted)">(' + escapeHtml(data.pickup_node.zone) + ')</span>' : ''));
  h += '</div><div>';
  h += field('Delivery', escapeHtml(o.delivery_node || '-') + (data.delivery_node && data.delivery_node.zone ? ' <span style="color:var(--text-muted)">(' + escapeHtml(data.delivery_node.zone) + ')</span>' : ''));
  h += '</div></div>';

  // ── CARGO: bin + payload ──
  if (data.bin || data.payload) {
    h += '<div class="manifest-row">';
    if (data.bin) {
      h += '<div>';
      h += field('Bin', escapeHtml(data.bin.label) + ' <span style="color:var(--text-muted)">(' + escapeHtml(data.bin.bin_type_code) + ')</span>');
      h += field('Bin Status', '<span class="badge">' + escapeHtml(data.bin.status) + '</span>');
      h += '</div>';
    }
    if (data.payload) {
      h += '<div>';
      h += field('Payload', '#' + data.payload.id + ' <span style="color:var(--text-muted)">' + escapeHtml(data.payload.payload_code) + '</span>');
      h += field('UOP Remaining', data.payload.uop_remaining + '');
      h += field('Manifest', data.payload.manifest_confirmed ? '<span class="badge badge-available">confirmed</span>' : '<span class="badge badge-empty">unconfirmed</span>');
      h += '</div>';
    }
    h += '</div>';

    // Manifest items (click to expand)
    if (data.manifest_items && data.manifest_items.length > 0) {
      var mid = 'om-manifest-' + o.id;
      h += '<div style="border-bottom:1px solid var(--border);padding:0.375rem 0">';
      h += '<a href="#" style="font-size:0.8rem" onclick="event.preventDefault();var el=document.getElementById(\'' + mid + '\');el.style.display=el.style.display===\'none\'?\'\':\'none\'">';
      h += 'Manifest (' + data.manifest_items.length + ' item' + (data.manifest_items.length > 1 ? 's' : '') + ')</a>';
      h += '<table class="table-compact" id="' + mid + '" style="display:none;font-size:0.78rem;margin-top:0.25rem">';
      h += '<thead><tr><th>Part Number</th><th>Qty</th><th>Lot</th><th>Notes</th></tr></thead><tbody>';
      for (var mi = 0; mi < data.manifest_items.length; mi++) {
        var item = data.manifest_items[mi];
        h += '<tr><td>' + escapeHtml(item.part_number) + '</td>';
        h += '<td>' + item.quantity + '</td>';
        h += '<td>' + escapeHtml(item.lot_code || '') + '</td>';
        h += '<td>' + escapeHtml(item.notes || '') + '</td></tr>';
      }
      h += '</tbody></table></div>';
    }
  }

  // ── TRANSPORT: vendor + robot ──
  if (o.vendor_order_id || o.robot_id) {
    h += '<div class="manifest-section">Transport</div>';
    h += '<div class="manifest-row cols-3">';
    if (o.vendor_order_id) h += '<div>' + field('Vendor Order', '<span style="font-family:monospace;font-size:0.75rem">' + escapeHtml(o.vendor_order_id) + '</span>') + fieldH('Vendor State', o.vendor_state) + '</div>';
    if (o.robot_id) h += '<div>' + fieldH('Robot ID', o.robot_id) + '</div>';
    h += '<div>' + field('Quantity', o.quantity + '') + '</div>';
    h += '</div>';
  }

  // ── ROBOT STATUS ──
  if (data.robot) {
    var rb = data.robot;
    var st = rb.Connected ? (rb.Emergency || rb.Blocked ? 'error' : (rb.Busy ? 'busy' : (rb.Available ? 'ready' : 'paused'))) : 'offline';
    h += '<div class="manifest-section">Robot Status</div>';
    h += '<div class="manifest-row cols-3">';
    h += '<div>' + field('Vehicle', escapeHtml(rb.VehicleID) + ' <span class="badge badge-' + st + '">' + st + '</span>') + '</div>';
    h += '<div>' + field('Battery', Math.round(rb.BatteryLevel) + '%' + (rb.Charging ? ' (charging)' : '')) + '</div>';
    h += '<div>' + field('Station', escapeHtml(rb.CurrentStation || rb.LastStation || '-')) + '</div>';
    h += '</div>';
    if (rb.Emergency) h += '<div class="manifest-alert manifest-alert-danger">EMERGENCY STOP ACTIVE</div>';
    if (rb.Blocked) h += '<div class="manifest-alert manifest-alert-warn">Robot is blocked</div>';
  }

  // ── RDS LIVE DETAIL ──
  if (data.vendor_detail && data.vendor_detail.Raw) {
    var vd = data.vendor_detail.Raw;
    h += '<div class="manifest-section">Fleet Detail (RDS Live)</div>';
    h += '<div class="manifest-row cols-3">';
    h += '<div>' + field('State', '<span class="badge badge-' + escapeHtml(data.vendor_detail.State) + '">' + escapeHtml(data.vendor_detail.State) + '</span>' + (data.vendor_detail.IsTerminal ? ' (terminal)' : '')) + '</div>';
    if (vd.fromLoc) h += '<div>' + fieldH('From Location', vd.fromLoc) + '</div>';
    if (vd.toLoc) h += '<div>' + fieldH('To Location', vd.toLoc) + '</div>';
    h += '</div>';

    var hasSubOrders = vd.containerName || vd.goodsId || vd.loadOrderId || vd.unloadOrderId;
    if (hasSubOrders) {
      h += '<div class="manifest-row cols-3">';
      if (vd.containerName) h += '<div>' + fieldH('Container', vd.containerName) + '</div>';
      if (vd.goodsId) h += '<div>' + fieldH('Goods', vd.goodsId) + '</div>';
      if (vd.loadOrderId) h += '<div>' + field('Load Sub-Order', escapeHtml(vd.loadOrderId) + ' <span class="badge">' + escapeHtml(vd.loadState || '') + '</span>') + '</div>';
      if (vd.unloadOrderId) h += '<div>' + field('Unload Sub-Order', escapeHtml(vd.unloadOrderId) + ' <span class="badge">' + escapeHtml(vd.unloadState || '') + '</span>') + '</div>';
      h += '</div>';
    }

    if (vd.blocks && vd.blocks.length > 0) {
      h += '<table class="table-compact"><thead><tr><th>Block</th><th>Location</th><th>State</th><th>Operation</th><th>Container</th></tr></thead><tbody>';
      for (var i = 0; i < vd.blocks.length; i++) {
        var b = vd.blocks[i];
        h += '<tr><td>' + escapeHtml(b.blockId) + '</td><td>' + escapeHtml(b.location) + '</td><td><span class="badge">' + escapeHtml(b.state) + '</span></td><td>' + escapeHtml(b.operation) + '</td><td>' + escapeHtml(b.containerName) + '</td></tr>';
      }
      h += '</tbody></table>';
    }

    if (vd.errors && vd.errors.length) h += '<div class="manifest-alert manifest-alert-danger"><strong>Errors:</strong> ' + vd.errors.map(escapeHtml).join(', ') + '</div>';
    if (vd.warnings && vd.warnings.length) h += '<div class="manifest-alert manifest-alert-warn"><strong>Warnings:</strong> ' + vd.warnings.map(escapeHtml).join(', ') + '</div>';
  }

  // ── CHILD ORDERS / STEPS ──
  if (data.children && data.children.length > 0) {
    h += '<div class="manifest-section">Order Steps</div>';
    h += '<table class="table-compact"><thead><tr><th>#</th><th>ID</th><th>Type</th><th>Status</th><th>Pickup</th><th>Delivery</th><th>Robot</th></tr></thead><tbody>';
    for (var i = 0; i < data.children.length; i++) {
      var c = data.children[i];
      h += '<tr style="cursor:pointer" onclick="openOrderModal(' + c.id + ')">';
      h += '<td>' + c.sequence + '</td><td>' + c.id + '</td><td>' + escapeHtml(c.order_type) + '</td>';
      h += '<td><span class="badge badge-' + c.status + '">' + escapeHtml(c.status) + '</span></td>';
      h += '<td>' + escapeHtml(c.pickup_node) + '</td><td>' + escapeHtml(c.delivery_node) + '</td><td>' + escapeHtml(c.robot_id) + '</td></tr>';
    }
    h += '</tbody></table>';
  }

  // ── TIMELINE ──
  if (data.history && data.history.length > 0) {
    h += '<div class="manifest-section">History</div>';
    h += '<ul class="timeline-list">';
    for (var i = 0; i < data.history.length; i++) {
      var ev = data.history[i];
      h += '<li>';
      h += '<span class="tl-time">' + fmtTime(ev.created_at) + '</span>';
      h += '<span class="badge badge-' + ev.status + '" style="font-size:0.7rem">' + escapeHtml(ev.status) + '</span>';
      if (ev.detail) h += '<span class="tl-detail">' + escapeHtml(ev.detail) + '</span>';
      h += '</li>';
    }
    h += '</ul>';
  }

  // Footer
  h += '<div style="text-align:right;font-size:0.75rem;margin-top:0.625rem;padding-top:0.375rem;border-top:1px solid var(--border)">';
  if (o.vendor_order_id) h += '<a href="/missions/' + o.id + '" title="View mission telemetry, timeline, and robot tracking for this order">Mission Telemetry</a> &middot; ';
  h += '<a href="/orders/detail?id=' + o.id + '">Open full detail page &rarr;</a></div>';

  h += '</div>'; // end manifest
  document.getElementById('order-modal-content').innerHTML = h;
}

// SSE auto-refresh for open modal
window.onOrderUpdate = function(e) {
  try {
    var data = JSON.parse(e.data);
    if (_orderModalID && data && data.order_id === _orderModalID) {
      openOrderModal(_orderModalID);
    }
  } catch(err) {}
};

// --- Spot order modal ---
var _spotNodesLoaded = false;
var _spotActiveTab = 'transport';

function openSpotOrderModal() {
  showModal('spot-order-modal');
  document.getElementById('spot-status').textContent = '';
  document.getElementById('spot-submit-btn').disabled = false;
  if (!_spotNodesLoaded) {
    _spotNodesLoaded = true;
    loadSpotDropdowns();
  }
  spotTransportTypeChanged();
}

function closeSpotOrderModal() {
  hideModal('spot-order-modal');
}

function switchSpotTab(name, btn) {
  _spotActiveTab = name;
  document.querySelectorAll('.spot-tab').forEach(function(t) { t.classList.remove('active'); });
  document.querySelectorAll('.spot-tab-content').forEach(function(c) { c.classList.remove('active'); });
  document.getElementById('spot-tab-' + name).classList.add('active');
  btn.classList.add('active');
  document.getElementById('spot-status').textContent = '';
  updateSpotQuantityVisibility();
}

function loadSpotDropdowns() {
  fetch('/api/nodes')
    .then(function(r) { return r.json(); })
    .then(function(nodes) {
      var byZone = {};
      for (var i = 0; i < nodes.length; i++) {
        var n = nodes[i];
        if (!n.enabled) continue;
        var z = n.zone || 'Other';
        if (!byZone[z]) byZone[z] = [];
        byZone[z].push(n);
      }
      var zones = Object.keys(byZone).sort();
      var html = '<option value="">— select —</option>';
      for (var zi = 0; zi < zones.length; zi++) {
        var zone = zones[zi];
        html += '<optgroup label="' + escapeHtml(zone) + '">';
        var zNodes = byZone[zone];
        zNodes.sort(function(a, b) { return a.name.localeCompare(b.name); });
        for (var ni = 0; ni < zNodes.length; ni++) {
          html += '<option value="' + escapeHtml(zNodes[ni].name) + '">' + escapeHtml(zNodes[ni].name) + '</option>';
        }
        html += '</optgroup>';
      }
      // Transport tab
      document.getElementById('spot-pickup').innerHTML = html;
      document.getElementById('spot-delivery').innerHTML = html;
      // Staged tab
      document.getElementById('spot-staged-pickup').innerHTML = html;
      document.getElementById('spot-staged-staging').innerHTML = html;
      document.getElementById('spot-staged-delivery').innerHTML = html;
      // Swap tab
      document.getElementById('spot-swap-node').innerHTML = html;
      // Send-to tab
      document.getElementById('spot-sendto-dest').innerHTML = html;
    });

  fetch('/api/payloads/templates')
    .then(function(r) { return r.json(); })
    .then(function(bps) {
      var html = '<option value="">— none —</option>';
      for (var i = 0; i < bps.length; i++) {
        html += '<option value="' + escapeHtml(bps[i].code) + '">' + escapeHtml(bps[i].code) + ' — ' + escapeHtml(bps[i].description) + '</option>';
      }
      document.getElementById('spot-payload').innerHTML = html;
      document.getElementById('spot-staged-payload').innerHTML = html;
      document.getElementById('spot-swap-payload').innerHTML = html;
    });

  loadSpotBinDropdown();
}

function loadSpotBinDropdown() {
  fetch('/api/bins/available')
    .then(function(r) { return r.json(); })
    .then(function(bins) {
      if (!bins || !bins.length) {
        document.getElementById('spot-bin').innerHTML = '<option value="">No available bins</option>';
        return;
      }
      var byZone = {};
      for (var i = 0; i < bins.length; i++) {
        var b = bins[i];
        var z = b.zone || 'Other';
        if (!byZone[z]) byZone[z] = [];
        byZone[z].push(b);
      }
      var zones = Object.keys(byZone).sort();
      var html = '<option value="">— select bin —</option>';
      for (var zi = 0; zi < zones.length; zi++) {
        var zone = zones[zi];
        html += '<optgroup label="' + escapeHtml(zone) + '">';
        var zBins = byZone[zone];
        zBins.sort(function(a, b) { return a.label.localeCompare(b.label); });
        for (var bi = 0; bi < zBins.length; bi++) {
          var b = zBins[bi];
          var text = b.label + ' @ ' + b.node_name;
          if (b.payload_code) text += ' (' + b.payload_code + ')';
          html += '<option value="' + escapeHtml(b.label) + '">' + escapeHtml(text) + '</option>';
        }
        html += '</optgroup>';
      }
      document.getElementById('spot-bin').innerHTML = html;
    });
}

function spotTransportTypeChanged() {
  var t = document.getElementById('spot-transport-type').value;
  var pickup = document.getElementById('spot-pickup-group');
  var delivery = document.getElementById('spot-delivery-group');
  var payload = document.getElementById('spot-payload-group');
  var binGroup = document.getElementById('spot-bin-group');
  var qtyGroup = document.getElementById('spot-quantity-group');

  if (t === 'retrieve_specific') {
    // Retrieve specific: bin selector + delivery only
    pickup.style.display = 'none';
    delivery.style.display = '';
    payload.style.display = 'none';
    binGroup.style.display = '';
  } else {
    binGroup.style.display = 'none';
    // Move: pickup + delivery
    // Retrieve: delivery + payload
    // Retrieve Empty: delivery + payload
    // Store: pickup + payload
    pickup.style.display = (t === 'retrieve' || t === 'retrieve_empty') ? 'none' : '';
    delivery.style.display = (t === 'store') ? 'none' : '';
    payload.style.display = (t === 'move') ? 'none' : '';
  }

  // Quantity only for retrieve and retrieve_empty
  qtyGroup.style.display = (t === 'retrieve' || t === 'retrieve_empty') ? '' : 'none';
}

function updateSpotQuantityVisibility() {
  var tab = _spotActiveTab;
  var qtyGroup = document.getElementById('spot-quantity-group');
  if (tab !== 'transport') {
    qtyGroup.style.display = 'none';
    return;
  }
  spotTransportTypeChanged();
}

function submitSpotOrder() {
  var status = document.getElementById('spot-status');
  var btn = document.getElementById('spot-submit-btn');
  var tab = _spotActiveTab;
  var body = {
    priority: parseInt(document.getElementById('spot-priority').value, 10) || 0,
    description: document.getElementById('spot-description').value
  };

  if (tab === 'transport') {
    var t = document.getElementById('spot-transport-type').value;
    body.order_type = t;

    if (t === 'retrieve_specific') {
      body.bin_label = document.getElementById('spot-bin').value;
      body.delivery_node = document.getElementById('spot-delivery').value;
      if (!body.bin_label) { status.textContent = 'Bin is required'; status.style.color = 'var(--danger)'; return; }
      if (!body.delivery_node) { status.textContent = 'Delivery node is required'; status.style.color = 'var(--danger)'; return; }
    } else {
      if (t !== 'retrieve' && t !== 'retrieve_empty') body.pickup_node = document.getElementById('spot-pickup').value;
      if (t !== 'store') body.delivery_node = document.getElementById('spot-delivery').value;
      if (t !== 'move') body.payload_code = document.getElementById('spot-payload').value;

      if ((t === 'move' || t === 'store') && !body.pickup_node) {
        status.textContent = 'Pickup node is required'; status.style.color = 'var(--danger)'; return;
      }
      if ((t === 'move' || t === 'retrieve' || t === 'retrieve_empty') && !body.delivery_node) {
        status.textContent = 'Delivery node is required'; status.style.color = 'var(--danger)'; return;
      }

      // Quantity for batch retrieve
      if (t === 'retrieve' || t === 'retrieve_empty') {
        var qty = parseInt(document.getElementById('spot-quantity').value, 10) || 1;
        if (qty > 1) body.quantity = qty;
      }
    }
  } else if (tab === 'staged') {
    body.order_type = 'staged';
    body.pickup_node = document.getElementById('spot-staged-pickup').value;
    body.staging_node = document.getElementById('spot-staged-staging').value;
    body.delivery_node = document.getElementById('spot-staged-delivery').value;
    body.payload_code = document.getElementById('spot-staged-payload').value;
    if (!body.pickup_node) { status.textContent = 'Pickup node is required'; status.style.color = 'var(--danger)'; return; }
    if (!body.staging_node) { status.textContent = 'Staging node is required'; status.style.color = 'var(--danger)'; return; }
    if (!body.delivery_node) { status.textContent = 'Delivery node is required'; status.style.color = 'var(--danger)'; return; }
  } else if (tab === 'swap') {
    body.order_type = 'swap';
    body.delivery_node = document.getElementById('spot-swap-node').value;
    body.payload_code = document.getElementById('spot-swap-payload').value;
    if (!body.delivery_node) { status.textContent = 'Target node is required'; status.style.color = 'var(--danger)'; return; }
    if (!body.payload_code) { status.textContent = 'Payload is required'; status.style.color = 'var(--danger)'; return; }
  } else if (tab === 'send_to') {
    body.order_type = 'send_to';
    body.delivery_node = document.getElementById('spot-sendto-dest').value;
    if (!body.delivery_node) { status.textContent = 'Destination node is required'; status.style.color = 'var(--danger)'; return; }
  }

  status.textContent = 'Submitting...';
  status.style.color = 'var(--text-muted)';
  btn.disabled = true;

  fetch('/api/orders/spot', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body)})
    .then(function(r) { return r.json().then(function(d) { return {ok:r.ok, data:d}; }); })
    .then(function(r) {
      if (!r.ok) {
        status.textContent = r.data.error || 'Error';
        status.style.color = 'var(--danger)';
        btn.disabled = false;
        return;
      }
      var msg;
      if (r.data.count && r.data.count > 1) {
        msg = r.data.count + ' orders created (first: #' + r.data.order_id + ')';
      } else if (r.data.store_order_id) {
        msg = 'Store #' + r.data.store_order_id + ' (' + r.data.store_status + ') + Retrieve #' + r.data.retrieve_order_id + ' (' + r.data.retrieve_status + ')';
      } else {
        msg = 'Order #' + r.data.order_id + ' created (' + r.data.status + ')';
        if (r.data.error_detail) msg += ' — ' + r.data.error_detail;
      }
      status.textContent = msg;
      var failed = r.data.status === 'failed' || r.data.store_status === 'failed' || r.data.retrieve_status === 'failed';
      status.style.color = failed ? 'var(--danger)' : 'var(--success)';
      setTimeout(function() { location.reload(); }, 1200);
    })
    .catch(function() {
      status.textContent = 'Network error';
      status.style.color = 'var(--danger)';
      btn.disabled = false;
    });
}

// Client-side table filter
(function() {
  var input = document.getElementById('filter-search');
  var countEl = document.getElementById('filter-count');
  var table = document.getElementById('orders-table');
  if (!input || !table) return;

  var rows = table.querySelectorAll('tbody tr');

  input.addEventListener('input', function() {
    var q = this.value.toLowerCase().trim();
    var visible = 0;
    for (var i = 0; i < rows.length; i++) {
      var text = rows[i].textContent.toLowerCase();
      var show = !q || text.indexOf(q) !== -1;
      rows[i].style.display = show ? '' : 'none';
      if (show) visible++;
    }
    countEl.textContent = q ? visible + ' of ' + rows.length : '';
  });
})();
