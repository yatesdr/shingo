import { api, apiGet, apiPost, debounce, delegateActions, escapeHtml, formatTime, h, hideModal, showModal, toggleVisibility, uiConfirm } from '/static/app.js';
import { onSSE } from '/static/shared/utils.js';

function orderControlPost(url, body) {
  var msg = document.getElementById('order-status-msg');
  if (msg) msg.textContent = 'Sending...';
  apiPost(url, body)
    .then(function() {
      if (msg) msg.textContent = 'OK - reloading...';
      setTimeout(function() { location.reload(); }, 800);
    })
    .catch(function(e) {
      console.error('orderControl', url, e);
      if (msg) msg.textContent = (typeof e === 'string' && e) ? e : 'Network error';
    });
}

async function terminateOrder(id) {
  // id arrives as a string from data-action="terminateOrder:<id>" colon-arg
  // dispatch; the Go handler decodes order_id as int64 and rejects string
  // JSON values with "invalid request".
  var oid = parseInt(id, 10);
  if (!await uiConfirm('Terminate order #' + oid + '? This cannot be undone.')) return;
  orderControlPost('/api/orders/terminate', {order_id: oid});
}

// cancelOrderFromRow is the operator-facing cancel action surfaced on
// the orders list table. Backed by the same /api/orders/terminate
// endpoint as the detail-page button — the verb difference is operator
// vocabulary, not a separate code path. The row's click handler
// (openOrderModal) is suppressed implicitly because delegateActions
// dispatches to the nearest [data-action] ancestor and stops there.
async function cancelOrderFromRow(id) {
  var oid = parseInt(id, 10);
  if (!await uiConfirm('Cancel order #' + oid + '? This will abort any in-flight robot work and release claimed bins.')) return;
  orderControlPost('/api/orders/terminate', {order_id: oid});
}

// Force-confirm a delivered order whose bin can't be recovered (moved by
// something else, or arrival side effects never propagated). Same effect
// as waiting 5 min for the auto-confirm loop. Goes through the recovery
// repair endpoint, which routes to ForceConfirmDelivered server-side.
async function forceConfirmDelivered(id) {
  var oid = parseInt(id, 10);
  if (!await uiConfirm('Force-confirm order #' + oid + ' (skip operator confirm)? Use when the bin has been moved elsewhere and the order is stuck in delivered.')) return;
  orderControlPost('/api/recovery/repair', {action: 'force_confirm_delivered', order_id: oid, bin_id: 0});
}

function setOrderPriority(id) {
  var oid = parseInt(id, 10);
  var p = parseInt(document.getElementById('order-priority').value, 10);
  if (isNaN(p)) return;
  orderControlPost('/api/orders/priority', {order_id: oid, priority: p});
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

  apiGet('/api/orders/enriched?id=' + id)
    .then(function(data) {
      loading.style.display = 'none';
      content.style.display = '';
      renderOrderModal(data);
    })
    .catch(function(e) {
      console.error('openOrderModal', id, e);
      loading.style.display = 'none';
      errEl.style.display = '';
      errEl.textContent = (typeof e === 'string' && e) ? e : 'Failed to load order';
    });
}

function closeOrderModal() {
  _orderModalID = null;
  hideModal('order-modal-overlay');
}

document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape' && _orderModalID) closeOrderModal();
});

function field(label, val, cls) {
  return '<div class="manifest-field' + (cls ? ' ' + cls : '') + '"><label><strong>' + label + '</strong></label><span>' + val + '</span></div>';
}
function fieldH(label, val, cls) { return field(label, escapeHtml(val || '-'), cls); }

function renderOrderModal(data) {
  var o = data.order;
  var out = '<div class="manifest">';

  // ── HEADER ──
  // Title is set on the modal <h2> already; build the status line + identity here
  out += '<div class="manifest-head">';
  // Status line: badge + error together
  out += '<div style="margin-bottom:0.25rem">';
  out += '<span class="badge badge-' + o.status + '">' + escapeHtml(o.status) + '</span>';
  if (o.error_detail) out += ' <span style="color:var(--danger);font-size:0.82rem">' + escapeHtml(o.error_detail) + '</span>';
  out += '</div>';
  // UUID + type
  out += '<div class="manifest-uuid"><strong>UUID:</strong> ' + escapeHtml(o.edge_uuid) + ' (' + escapeHtml(o.order_type) + ')</div>';
  // Station + priority
  out += '<div class="manifest-meta"><span><strong>Originating Station:</strong> ' + escapeHtml(o.station_id) + ' (Priority: ' + o.priority + ')</span></div>';
  if (o.payload_desc) {
    out += '<div class="manifest-meta"><span><strong>Description:</strong> ' + escapeHtml(o.payload_desc) + '</span></div>';
  }
  // Timestamps
  out += '<div class="manifest-meta">';
  out += '<span><strong>Created:</strong> ' + formatTime(o.created_at) + '</span>';
  out += '<span><strong>Modified:</strong> ' + formatTime(o.updated_at) + '</span>';
  if (o.completed_at) out += '<span><strong>Completed:</strong> ' + formatTime(o.completed_at) + '</span>';
  if (o.parent_order_id) out += '<span><strong>Parent:</strong> <a href="#" data-action="openOrderModal:' + o.parent_order_id + '" data-prevent-default="1" >#' + o.parent_order_id + '</a> (step ' + o.sequence + ')</span>';
  out += '</div></div>';

  // ── ROUTING ──
  out += '<div class="manifest-row">';
  out += '<div>';
  out += field('Source', escapeHtml(o.source_node || '-') + (data.source_node && data.source_node.zone ? ' <span style="color:var(--text-muted)">(' + escapeHtml(data.source_node.zone) + ')</span>' : ''));
  out += '</div><div>';
  out += field('Delivery', escapeHtml(o.delivery_node || '-') + (data.delivery_node && data.delivery_node.zone ? ' <span style="color:var(--text-muted)">(' + escapeHtml(data.delivery_node.zone) + ')</span>' : ''));
  out += '</div></div>';

  // ── CARGO: bin + payload ──
  if (data.bin || data.payload) {
    out += '<div class="manifest-row">';
    if (data.bin) {
      out += '<div>';
      out += field('Bin', escapeHtml(data.bin.label) + ' <span style="color:var(--text-muted)">(' + escapeHtml(data.bin.bin_type_code) + ')</span>');
      out += field('Bin Status', '<span class="badge">' + escapeHtml(data.bin.status) + '</span>');
      out += '</div>';
    }
    if (data.payload) {
      out += '<div>';
      out += field('Payload', '#' + data.payload.id + ' <span style="color:var(--text-muted)">' + escapeHtml(data.payload.payload_code) + '</span>');
      out += field('UOP Remaining', data.payload.uop_remaining + '');
      out += field('Manifest', data.payload.manifest_confirmed ? '<span class="badge badge-available">confirmed</span>' : '<span class="badge badge-empty">unconfirmed</span>');
      out += '</div>';
    }
    out += '</div>';

    // Manifest items (click to expand)
    if (data.manifest_items && data.manifest_items.length > 0) {
      var mid = 'om-manifest-' + o.id;
      out += '<div style="border-bottom:1px solid var(--border);padding:0.375rem 0">';
      out += '<a href="#" style="font-size:0.8rem" data-action="toggleVisibility:' + mid + '" data-prevent-default="1" >';
      out += 'Manifest (' + data.manifest_items.length + ' item' + (data.manifest_items.length > 1 ? 's' : '') + ')</a>';
      out += h`<table class="table-compact" id="${mid}" style="display:none;font-size:0.78rem;margin-top:0.25rem">
        <thead><tr><th>Part Number</th><th>Qty</th><th>Lot</th><th>Notes</th></tr></thead><tbody>${
          data.manifest_items.map(function(item) {
            return h`<tr><td>${item.part_number}</td><td>${item.quantity}</td><td>${item.lot_code || ''}</td><td>${item.notes || ''}</td></tr>`;
          })
        }</tbody></table></div>`;
    }
  }

  // ── TRANSPORT: vendor + robot ──
  if (o.vendor_order_id || o.robot_id) {
    out += '<div class="manifest-section">Transport</div>';
    out += '<div class="manifest-row cols-3">';
    if (o.vendor_order_id) out += '<div>' + field('Vendor Order', '<span style="font-family:monospace;font-size:0.75rem">' + escapeHtml(o.vendor_order_id) + '</span>') + fieldH('Vendor State', o.vendor_state) + '</div>';
    if (o.robot_id) out += '<div>' + fieldH('Robot ID', o.robot_id) + '</div>';
    out += '<div>' + field('Quantity', o.quantity + '') + '</div>';
    out += '</div>';
  }

  // ── ROBOT STATUS ──
  if (data.robot) {
    var rb = data.robot;
    var st = rb.Connected ? (rb.Emergency || rb.Blocked ? 'error' : (rb.Busy ? 'busy' : (rb.Available ? 'ready' : 'paused'))) : 'offline';
    out += '<div class="manifest-section">Robot Status</div>';
    out += '<div class="manifest-row cols-3">';
    out += '<div>' + field('Vehicle', escapeHtml(rb.VehicleID) + ' <span class="badge badge-' + st + '">' + st + '</span>') + '</div>';
    out += '<div>' + field('Battery', Math.round(rb.BatteryLevel) + '%' + (rb.Charging ? ' (charging)' : '')) + '</div>';
    out += '<div>' + field('Station', escapeHtml(rb.CurrentStation || rb.LastStation || '-')) + '</div>';
    out += '</div>';
    if (rb.Emergency) out += '<div class="manifest-alert manifest-alert-danger">EMERGENCY STOP ACTIVE</div>';
    if (rb.Blocked) out += '<div class="manifest-alert manifest-alert-warn">Robot is blocked</div>';
  }

  // ── RDS LIVE DETAIL ──
  if (data.vendor_detail && data.vendor_detail.Raw) {
    var vd = data.vendor_detail.Raw;
    out += '<div class="manifest-section">Fleet Detail (RDS Live)</div>';
    out += '<div class="manifest-row cols-3">';
    out += '<div>' + field('State', '<span class="badge badge-' + escapeHtml(data.vendor_detail.State) + '">' + escapeHtml(data.vendor_detail.State) + '</span>' + (data.vendor_detail.IsTerminal ? ' (terminal)' : '')) + '</div>';
    if (vd.fromLoc) out += '<div>' + fieldH('From Location', vd.fromLoc) + '</div>';
    if (vd.toLoc) out += '<div>' + fieldH('To Location', vd.toLoc) + '</div>';
    out += '</div>';

    var hasSubOrders = vd.containerName || vd.goodsId || vd.loadOrderId || vd.unloadOrderId;
    if (hasSubOrders) {
      out += '<div class="manifest-row cols-3">';
      if (vd.containerName) out += '<div>' + fieldH('Container', vd.containerName) + '</div>';
      if (vd.goodsId) out += '<div>' + fieldH('Goods', vd.goodsId) + '</div>';
      if (vd.loadOrderId) out += '<div>' + field('Load Sub-Order', escapeHtml(vd.loadOrderId) + ' <span class="badge">' + escapeHtml(vd.loadState || '') + '</span>') + '</div>';
      if (vd.unloadOrderId) out += '<div>' + field('Unload Sub-Order', escapeHtml(vd.unloadOrderId) + ' <span class="badge">' + escapeHtml(vd.unloadState || '') + '</span>') + '</div>';
      out += '</div>';
    }

    if (vd.blocks && vd.blocks.length > 0) {
      out += h`<table class="table-compact"><thead><tr><th>Block</th><th>Location</th><th>State</th><th>Operation</th><th>Container</th></tr></thead><tbody>${
        vd.blocks.map(function(b) {
          return h`<tr><td>${b.blockId}</td><td>${b.location}</td><td><span class="badge">${b.state}</span></td><td>${b.operation}</td><td>${b.containerName}</td></tr>`;
        })
      }</tbody></table>`;
    }

    if (vd.errors && vd.errors.length) out += '<div class="manifest-alert manifest-alert-danger"><strong>Errors:</strong> ' + vd.errors.map(escapeHtml).join(', ') + '</div>';
    if (vd.warnings && vd.warnings.length) out += '<div class="manifest-alert manifest-alert-warn"><strong>Warnings:</strong> ' + vd.warnings.map(escapeHtml).join(', ') + '</div>';
  }

  // ── CHILD ORDERS / STEPS ──
  if (data.children && data.children.length > 0) {
    out += '<div class="manifest-section">Order Steps</div>';
    out += h`<table class="table-compact"><thead><tr><th>#</th><th>ID</th><th>Type</th><th>Status</th><th>Source</th><th>Delivery</th><th>Robot</th></tr></thead><tbody>${
      data.children.map(function(c) {
        return h`<tr style="cursor:pointer" data-action="openOrderModal:${c.id}">
          <td>${c.sequence}</td><td>${c.id}</td><td>${c.order_type}</td>
          <td><span class="badge badge-${c.status}">${c.status}</span></td>
          <td>${c.source_node}</td><td>${c.delivery_node}</td><td>${c.robot_id}</td>
        </tr>`;
      })
    }</tbody></table>`;
  }

  // ── TIMELINE ──
  if (data.history && data.history.length > 0) {
    out += '<div class="manifest-section">History</div>';
    out += h`<ul class="timeline-list">${
      data.history.map(function(ev) {
        return h`<li>
          <span class="tl-time">${{__html:true, value: formatTime(ev.created_at)}}</span>
          <span class="badge badge-${ev.status}" style="font-size:0.7rem">${ev.status}</span>
          ${ev.detail ? {__html:true, value: h`<span class="tl-detail">${ev.detail}</span>`} : ''}
        </li>`;
      })
    }</ul>`;
  }

  // Footer
  out += '<div style="text-align:right;font-size:0.75rem;margin-top:0.625rem;padding-top:0.375rem;border-top:1px solid var(--border)">';
  if (o.vendor_order_id) out += '<a href="/missions/' + o.id + '" title="View mission telemetry, timeline, and robot tracking for this order">Mission Telemetry</a> &middot; ';
  out += '<a href="/orders/detail?id=' + o.id + '">Open full detail page &rarr;</a></div>';

  out += '</div>'; // end manifest
  document.getElementById('order-modal-content').innerHTML = out;
}

// SSE auto-refresh for open modal — subscribed on the shared onSSE bus.
// The handler receives the already-parsed payload (the bus does JSON.parse,
// reconnect, and build-id detection); replaces the retired app.js IIFE's
// window.onOrderUpdate dispatch (Q-002).
onSSE('order-update', debounce(function(data) {
  if (_orderModalID != null) {
    // A modal is open. Refresh it when this event is for that order.
    // order_id arrives as a number in the SSE JSON, but _orderModalID comes
    // from the data-action colon-arg as a string — normalize both sides.
    // Do NOT hard-reload while a modal is open: location.reload() would
    // discard the modal (and any filter/scroll state) and defeat the
    // targeted refresh below.
    if (data && Number(data.order_id) === Number(_orderModalID)) {
      openOrderModal(_orderModalID);
    }
    return;
  }
  // No modal open: refresh the order list to reflect status changes.
  location.reload();
}, 2000));

// --- Manual order modal ---
var _moNodesLoaded = false;
var _moActiveTab = 'transport';

function openManualOrderModal() {
  showModal('manual-order-modal');
  document.getElementById('mo-status').textContent = '';
  document.getElementById('manual-order-submit-btn').disabled = false;
  if (!_moNodesLoaded) {
    _moNodesLoaded = true;
    loadManualOrderDropdowns();
  }
  manualOrderTransportTypeChanged();
}

function closeManualOrderModal() {
  hideModal('manual-order-modal');
}

function switchManualOrderTab(name, btn) {
  _moActiveTab = name;
  document.querySelectorAll('.manual-order-tab').forEach(function(t) { t.classList.remove('active'); });
  document.querySelectorAll('.manual-order-tab-content').forEach(function(c) { c.classList.remove('active'); });
  document.getElementById('manual-order-tab-' + name).classList.add('active');
  btn.classList.add('active');
  document.getElementById('mo-status').textContent = '';
  updateManualOrderQuantityVisibility();
}

function loadManualOrderDropdowns() {
  apiGet('/api/nodes')
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
      document.getElementById('mo-source').innerHTML = html;
      document.getElementById('mo-delivery').innerHTML = html;
      // Staged tab
      document.getElementById('mo-staged-source').innerHTML = html;
      document.getElementById('mo-staged-staging').innerHTML = html;
      document.getElementById('mo-staged-delivery').innerHTML = html;
      // Swap tab
      document.getElementById('mo-swap-node').innerHTML = html;
      // Send-to tab
      document.getElementById('mo-sendto-dest').innerHTML = html;
    })
    .catch(function(e) { console.error('loadManualOrderDropdowns nodes', e); });

  apiGet('/api/payloads/templates')
    .then(function(bps) {
      var html = '<option value="">— none —</option>';
      for (var i = 0; i < bps.length; i++) {
        html += '<option value="' + escapeHtml(bps[i].code) + '">' + escapeHtml(bps[i].code) + ' — ' + escapeHtml(bps[i].description) + '</option>';
      }
      document.getElementById('mo-payload').innerHTML = html;
      document.getElementById('mo-staged-payload').innerHTML = html;
      document.getElementById('mo-swap-payload').innerHTML = html;
    })
    .catch(function(e) { console.error('loadManualOrderDropdowns payloads', e); });

  loadManualOrderBinDropdown();
}

function loadManualOrderBinDropdown() {
  apiGet('/api/bins/available')
    .then(function(bins) {
      if (!bins || !bins.length) {
        document.getElementById('mo-bin').innerHTML = '<option value="">No available bins</option>';
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
      document.getElementById('mo-bin').innerHTML = html;
    })
    .catch(function(e) { console.error('loadManualOrderBinDropdown', e); });
}

function manualOrderTransportTypeChanged() {
  var t = document.getElementById('mo-transport-type').value;
  var pickup = document.getElementById('mo-pickup-group');
  var delivery = document.getElementById('mo-delivery-group');
  var payload = document.getElementById('mo-payload-group');
  var binGroup = document.getElementById('mo-bin-group');
  var qtyGroup = document.getElementById('mo-quantity-group');

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

function updateManualOrderQuantityVisibility() {
  var tab = _moActiveTab;
  var qtyGroup = document.getElementById('mo-quantity-group');
  if (tab !== 'transport') {
    qtyGroup.style.display = 'none';
    return;
  }
  manualOrderTransportTypeChanged();
}

function submitManualOrder() {
  var status = document.getElementById('mo-status');
  var btn = document.getElementById('manual-order-submit-btn');
  var tab = _moActiveTab;
  var body = {
    priority: parseInt(document.getElementById('mo-priority').value, 10) || 0,
    description: document.getElementById('mo-description').value
  };

  if (tab === 'transport') {
    var t = document.getElementById('mo-transport-type').value;
    body.order_type = t;

    if (t === 'retrieve_specific') {
      body.bin_label = document.getElementById('mo-bin').value;
      body.delivery_node = document.getElementById('mo-delivery').value;
      if (!body.bin_label) { status.textContent = 'Bin is required'; status.style.color = 'var(--danger)'; return; }
      if (!body.delivery_node) { status.textContent = 'Delivery node is required'; status.style.color = 'var(--danger)'; return; }
    } else {
      if (t !== 'retrieve' && t !== 'retrieve_empty') body.source_node = document.getElementById('mo-source').value;
      if (t !== 'store') body.delivery_node = document.getElementById('mo-delivery').value;
      if (t !== 'move') body.payload_code = document.getElementById('mo-payload').value;

      if ((t === 'move' || t === 'store') && !body.source_node) {
        status.textContent = 'Source node is required'; status.style.color = 'var(--danger)'; return;
      }
      if ((t === 'move' || t === 'retrieve' || t === 'retrieve_empty') && !body.delivery_node) {
        status.textContent = 'Delivery node is required'; status.style.color = 'var(--danger)'; return;
      }

      // Quantity for batch retrieve
      if (t === 'retrieve' || t === 'retrieve_empty') {
        var qty = parseInt(document.getElementById('mo-quantity').value, 10) || 1;
        if (qty > 1) body.quantity = qty;
      }
    }
  } else if (tab === 'staged') {
    body.order_type = 'staged';
    body.source_node = document.getElementById('mo-staged-source').value;
    body.staging_node = document.getElementById('mo-staged-staging').value;
    body.delivery_node = document.getElementById('mo-staged-delivery').value;
    body.payload_code = document.getElementById('mo-staged-payload').value;
    if (!body.source_node) { status.textContent = 'Source node is required'; status.style.color = 'var(--danger)'; return; }
    if (!body.staging_node) { status.textContent = 'Staging node is required'; status.style.color = 'var(--danger)'; return; }
    if (!body.delivery_node) { status.textContent = 'Delivery node is required'; status.style.color = 'var(--danger)'; return; }
  } else if (tab === 'swap') {
    body.order_type = 'swap';
    body.delivery_node = document.getElementById('mo-swap-node').value;
    body.payload_code = document.getElementById('mo-swap-payload').value;
    if (!body.delivery_node) { status.textContent = 'Target node is required'; status.style.color = 'var(--danger)'; return; }
    if (!body.payload_code) { status.textContent = 'Payload is required'; status.style.color = 'var(--danger)'; return; }
  } else if (tab === 'send_to') {
    body.order_type = 'send_to';
    body.delivery_node = document.getElementById('mo-sendto-dest').value;
    if (!body.delivery_node) { status.textContent = 'Destination node is required'; status.style.color = 'var(--danger)'; return; }
  }

  status.textContent = 'Submitting...';
  status.style.color = 'var(--text-muted)';
  btn.disabled = true;

  apiPost('/api/orders/spot', body)
    .then(function(data) {
      var msg;
      if (data.count && data.count > 1) {
        msg = data.count + ' orders created (first: #' + data.order_id + ')';
      } else if (data.store_order_id) {
        msg = 'Store #' + data.store_order_id + ' (' + data.store_status + ') + Retrieve #' + data.retrieve_order_id + ' (' + data.retrieve_status + ')';
      } else {
        msg = 'Order #' + data.order_id + ' created (' + data.status + ')';
        if (data.error_detail) msg += ' — ' + data.error_detail;
      }
      status.textContent = msg;
      var failed = data.status === 'failed' || data.store_status === 'failed' || data.retrieve_status === 'failed';
      status.style.color = failed ? 'var(--danger)' : 'var(--success)';
      setTimeout(function() { location.reload(); }, 1200);
    })
    .catch(function(e) {
      console.error('submitManualOrder', e);
      status.textContent = (typeof e === 'string' && e) ? e : 'Network error';
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

// ─── delegated event handlers ─────────────────────────
// All page-level data-action verbs route through delegateActions
// on document.body. Multiple event types share the same handler
// map — most handlers are click-only but a few (e.g. updatePreview)
// are referenced via data-action-change / data-action-input too,
// so binding the map across every event type keeps the page wiring
// single-source.
delegateActions(document.body, {
    closeManualOrderModal,
    closeOrderModal,
    field,
    fieldH,
    forceConfirmDelivered,
    loadManualOrderBinDropdown,
    loadManualOrderDropdowns,
    manualOrderTransportTypeChanged,
    openManualOrderModal,
    openOrderModal,
    orderControlPost,
    renderOrderModal,
    cancelOrderFromRow,
    setOrderPriority,
    submitManualOrder,
    switchManualOrderTab,
    terminateOrder,
    updateManualOrderQuantityVisibility
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
