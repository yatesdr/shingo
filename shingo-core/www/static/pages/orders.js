import { api, apiGet, apiPost, debounce, delegateActions, escapeHtml, formatTime, h, hideModal, showModal, toggleVisibility, uiConfirm } from '/static/app.js';
import { onSSE } from '/static/shared/utils.js';

// Controls live inside the manifest, which can be on screen twice at once
// (detail page with a child-step modal open over it), so the status line is
// found relative to the button that was pressed rather than by a page-wide
// id. `el` is the clicked element — delegateActions appends it after the
// colon args.
// Auth comes from the wrapper's data-authenticated, set by the Go template.
// The manifest is drawn by JS on two surfaces, neither of which is a
// template, so {{if .Authenticated}} can't gate the controls.
function isAuthenticated() {
  var root = document.querySelector('[data-sse="orders"]');
  return !!root && root.dataset.authenticated === 'true';
}

function controlMsg(el) {
  var scope = el && el.closest ? el.closest('.manifest') : null;
  return scope ? scope.querySelector('.ctl-msg') : null;
}

function orderControlPost(url, body, el) {
  var msg = controlMsg(el);
  if (msg) msg.textContent = 'Sending...';
  apiPost(url, body)
    .then(function() {
      if (msg) msg.textContent = 'OK';
      // Re-render in place instead of location.reload(): a reload from
      // inside the modal would close it, and on the detail page it would
      // throw away scroll for no gain.
      refreshVisibleManifest();
    })
    .catch(function(e) {
      console.error('orderControl', url, e);
      if (msg) msg.textContent = (typeof e === 'string' && e) ? e : 'Network error';
    });
}

// Re-draw the open order after a control action.
function refreshVisibleManifest() {
  if (_orderModalID != null) openOrderModal(_orderModalID);
}

async function terminateOrder(id, el) {
  // id arrives as a string from data-action="terminateOrder:<id>" colon-arg
  // dispatch; the Go handler decodes order_id as int64 and rejects string
  // JSON values with "invalid request".
  var oid = parseInt(id, 10);
  if (!await uiConfirm('Terminate order #' + oid + '? This cannot be undone.')) return;
  orderControlPost('/api/orders/terminate', {order_id: oid}, el);
}

// cancelOrderFromRow is the operator-facing cancel action surfaced on
// the orders list table. Backed by the same /api/orders/terminate
// endpoint as the detail-page button — the verb difference is operator
// vocabulary, not a separate code path. The row's click handler
// (openOrderModal) is suppressed implicitly because delegateActions
// dispatches to the nearest [data-action] ancestor and stops there.
async function cancelOrderFromRow(id, el) {
  var oid = parseInt(id, 10);
  if (!await uiConfirm('Cancel order #' + oid + '? This will abort any in-flight robot work and release claimed bins.')) return;
  // From a list row there is no manifest to re-render, so fall back to the
  // reload the list has always done.
  apiPost('/api/orders/terminate', {order_id: oid})
    .then(function() { location.reload(); })
    .catch(function(e) { console.error('cancelOrderFromRow', oid, e); });
}

// Force-confirm a delivered order whose bin can't be recovered (moved by
// something else, or arrival side effects never propagated). Same effect
// as waiting 5 min for the auto-confirm loop. Goes through the recovery
// repair endpoint, which routes to ForceConfirmDelivered server-side.
async function forceConfirmDelivered(id, el) {
  var oid = parseInt(id, 10);
  if (!await uiConfirm('Force-confirm order #' + oid + ' (skip operator confirm)? Use when the bin has been moved elsewhere and the order is stuck in delivered.')) return;
  orderControlPost('/api/recovery/repair', {action: 'force_confirm_delivered', order_id: oid, bin_id: 0}, el);
}

// Hard-release a dwelling order past its wait, whoever owns that wait (W3).
//
// This is the ESCAPE HATCH, not the ordinary release. The station's board is
// where a station-owned wait gets released; Core's fence advances a lane-owned
// one when the lane is safe. This bypasses both, for the case those mechanisms
// are themselves wedged — which this stream met three times in one campaign.
//
// The confirm says what is actually at risk, because a hard release of a
// station wait can drive a robot into a cell somebody is still working in. The
// server records the actor and the physical lane verdict it overrode.
async function hardReleaseOrder(id, el) {
  var oid = parseInt(id, 10);
  if (!await uiConfirm(
      'HARD RELEASE order #' + oid + '?\n\n' +
      'This advances the robot past its wait WITHOUT asking whose turn it is — ' +
      'skipping both the station\'s release and Core\'s lane fence.\n\n' +
      'If the cell or lane is still occupied, the robot will drive into it. ' +
      'Use this only when the normal releaser is stuck. The action is recorded ' +
      'against your username.')) return;
  orderControlPost('/api/orders/hard-release', {order_id: oid}, el);
}

function setOrderPriority(id, el) {
  var oid = parseInt(id, 10);
  var scope = el && el.closest ? el.closest('.manifest') : document;
  var input = scope.querySelector('.ctl-priority');
  var p = input ? parseInt(input.value, 10) : NaN;
  if (isNaN(p)) return;
  orderControlPost('/api/orders/priority', {order_id: oid, priority: p}, el);
}

// --- Order detail modal ---
var _orderModalID = null;

function openOrderModal(id, el, evt) {
  // The ID cell is a real <a href> to the detail page so the permalink can
  // be copied and ctrl/cmd/middle-clicked into a new tab. A plain click
  // opens the modal instead, so every click on a row lands in the same
  // place. Modified clicks fall through to the browser.
  if (evt && evt.type === 'click' && !evt.metaKey && !evt.ctrlKey && !evt.shiftKey && !evt.altKey && evt.button === 0) {
    evt.preventDefault();
  }
  _orderModalID = id;
  var title = document.getElementById('order-modal-title');
  var loading = document.getElementById('order-modal-loading');
  var content = document.getElementById('order-modal-content');
  var errEl = document.getElementById('order-modal-error');
  title.textContent = 'Order #' + id;
  // classList, not style.display. The markup marks these hidden with
  // class="hide" (.hide { display:none }), and clearing an inline display
  // cannot beat a class — so the old style.display='' left the body
  // invisible and the modal opened as an empty box. Third instance of this
  // exact bug in this file; see manualOrderTransportTypeChanged.
  loading.classList.remove('hide');
  content.classList.add('hide');
  errEl.classList.add('hide');
  showModal('order-modal-overlay');
  // Reflect the open order in the URL so it is linkable and survives a
  // refresh; /orders/detail?id=N redirects here.
  try { history.replaceState(null, '', '/orders?open=' + id); } catch (e) { /* non-fatal */ }

  apiGet('/api/orders/enriched?id=' + id)
    .then(function(data) {
      renderOrderModal(data);
      loading.classList.add('hide');
      errEl.classList.add('hide');
      content.classList.remove('hide');
    })
    .catch(function(e) {
      console.error('openOrderModal', id, e);
      loading.classList.add('hide');
      errEl.textContent = (typeof e === 'string' && e) ? e : 'Failed to load order';
      errEl.classList.remove('hide');
    });
}

function closeOrderModal() {
  _orderModalID = null;
  hideModal('order-modal-overlay');
  // Drop ?open= so a refresh doesn't reopen what you just closed, keeping
  // whatever status filter you were browsing.
  try {
    var u = new URL(location.href);
    u.searchParams.delete('open');
    history.replaceState(null, '', u.pathname + (u.search || ''));
  } catch (e) { /* non-fatal */ }
}

document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape' && _orderModalID) closeOrderModal();
});

// The label is NOT bold. It used to be, which inverted the hierarchy on
// every field in the manifest: the caption "VENDOR ORDER" carried more
// weight than the value beside it, so the eye had nothing to land on and
// the page read as a wall of shouting captions. Labels are small, muted
// and light; the value is the emphasis. (.manifest-field in style.css.)
function field(label, val, cls) {
  return '<div class="manifest-field' + (cls ? ' ' + cls : '') + '"><label>' + label + '</label><span>' + val + '</span></div>';
}
function fieldH(label, val, cls) { return field(label, escapeHtml(val || '-'), cls); }

// elapsedLabel answers "how long did this take / has this been going" —
// the question the timestamps made you compute by hand.
// durationText renders a span of seconds. One spelling, because the timeline's
// unaccounted-gap marker has to read in the same units as the elapsed label
// beside it — two formatters would drift into two vocabularies.
function durationText(secs) {
  return secs < 60 ? secs + 's'
    : secs < 3600 ? Math.floor(secs / 60) + 'm ' + (secs % 60) + 's'
    : Math.floor(secs / 3600) + 'h ' + Math.floor((secs % 3600) / 60) + 'm';
}

function elapsedLabel(o) {
  if (!o.created_at) return '';
  var start = new Date(o.created_at).getTime();
  var end = o.completed_at ? new Date(o.completed_at).getTime() : Date.now();
  var secs = Math.round((end - start) / 1000);
  if (!isFinite(secs) || secs < 0) return '';
  var txt = durationText(secs);
  return o.completed_at ? 'took ' + txt : txt + ' elapsed';
}

// buildManifest renders the order manifest and returns the HTML. It is the
// ONE order view: the separate /orders/detail page was retired once it and
// this modal rendered the same thing, so there is no second surface to
// drift. /orders?open=N deep-links straight to it.
function buildManifest(data, opts) {
  opts = opts || {};
  var o = data.order;
  var out = '<div class="manifest">';

  // ── HERO ──
  // The route is what an order IS, so it leads — one readable
  // "SMN_004 → SMN_001" line rather than two labelled cells in a grid, with
  // the status and the elapsed time beside it. Everything else is a
  // footnote to that. This replaced a header of bold-label / plain-value
  // pairs, which inverted the emphasis: the eye landed on the word
  // "Originating Station" instead of on the station.
  out += '<div class="manifest-head">';
  out += '<div class="manifest-hero">';
  out += '<span class="manifest-route">' + escapeHtml(o.source_node || '—') +
    ' <span class="manifest-arrow">&rarr;</span> ' + escapeHtml(o.delivery_node || '—') + '</span>';
  out += '<span class="badge badge-' + o.status + '">' + escapeHtml(o.status) + '</span>';
  var elapsed = elapsedLabel(o);
  if (elapsed) out += '<span class="manifest-elapsed tnum">' + elapsed + '</span>';
  out += '</div>';

  // Anything wrong or blocking goes directly under the hero, full width —
  // queue_reason is the whole story on a queued order (why it is stuck) and
  // the manifest never showed it, so the detail page would have hidden it
  // on exactly the orders someone opens that page to understand.
  if (o.error_detail) out += '<div class="manifest-error">' + escapeHtml(o.error_detail) + '</div>';
  if (o.queue_reason) out += '<div class="manifest-reason">' + escapeHtml(o.queue_reason) + '</div>';

  // Identity strip: small, muted, one wrapping line. Zones ride along with
  // the nodes they belong to instead of taking their own cells.
  var ident = [];
  ident.push(escapeHtml(o.order_type));
  if (o.payload_desc) ident.push(escapeHtml(o.payload_desc));
  if (data.source_node && data.source_node.zone) ident.push('from ' + escapeHtml(data.source_node.zone));
  if (data.delivery_node && data.delivery_node.zone) ident.push('to ' + escapeHtml(data.delivery_node.zone));
  ident.push(escapeHtml(o.station_id));
  ident.push('qty ' + o.quantity);
  ident.push('priority ' + o.priority);
  if (o.parent_order_id) {
    ident.push('step ' + o.sequence + ' of <a href="#" data-action="openOrderModal:' + o.parent_order_id +
      '" data-prevent-default="1">#' + o.parent_order_id + '</a>');
  }
  out += '<div class="manifest-ident">' + ident.join('<span class="manifest-dot">&middot;</span>') + '</div>';
  out += '<div class="manifest-uuid">' + escapeHtml(o.edge_uuid) + '</div>';
  out += '</div>';

  // ── CARGO + TRANSPORT ──
  // One flowing field strip rather than fixed 2- and 3-column grid rows.
  // The grid reserved a cell per column whether or not it had content, so a
  // section with one field left half the width empty — most of the dead
  // space on this page came from there. Fields now wrap naturally and only
  // occupy what they need.
  var facts = [];
  if (data.bin) {
    facts.push(field('Bin', escapeHtml(data.bin.label) + ' <span class="manifest-sub">(' + escapeHtml(data.bin.bin_type_code) + ')</span>'));
    facts.push(field('Bin Status', '<span class="badge">' + escapeHtml(data.bin.status) + '</span>'));
  }
  if (data.payload) {
    facts.push(field('Payload', '#' + data.payload.id + ' <span class="manifest-sub">' + escapeHtml(data.payload.payload_code) + '</span>'));
    facts.push(field('UoP Remaining', data.payload.uop_remaining + ''));
    facts.push(field('Manifest', data.payload.manifest_confirmed ? '<span class="badge badge-available">confirmed</span>' : '<span class="badge badge-empty">unconfirmed</span>'));
  }
  if (o.vendor_order_id) {
    facts.push(field('Vendor Order', '<span class="manifest-mono">' + escapeHtml(o.vendor_order_id) + '</span>'));
    facts.push(fieldH('Vendor State', o.vendor_state));
  }
  if (o.robot_id) facts.push(fieldH('Robot', o.robot_id));
  if (facts.length) out += '<div class="manifest-facts">' + facts.join('') + '</div>';

  if (data.bin || data.payload) {

    // Manifest items (click to expand)
    if (data.manifest_items && data.manifest_items.length > 0) {
      var mid = 'om-manifest-' + o.id;
      out += '<div class="manifest-expand">';
      out += '<a href="#" data-action="toggleVisibility:' + mid + '" data-prevent-default="1" >';
      out += 'Manifest (' + data.manifest_items.length + ' item' + (data.manifest_items.length > 1 ? 's' : '') + ')</a>';
      // display:none, not .hide — the shared toggleVisibility helper flips
      // style.display, and a class it cannot beat is exactly the bug that
      // kept the manual-order Bin/Quantity groups permanently hidden.
      out += h`<table class="table-compact manifest-items" id="${mid}" style="display:none">
        <thead><tr><th>Part Number</th><th>Qty</th><th>Lot</th><th>Notes</th></tr></thead><tbody>${
          data.manifest_items.map(function(item) {
            return h`<tr><td>${item.part_number}</td><td>${item.quantity}</td><td>${item.lot_code || ''}</td><td>${item.notes || ''}</td></tr>`;
          })
        }</tbody></table></div>`;
    }
  }


  // ── ROBOT STATUS ──
  if (data.robot) {
    var rb = data.robot;
    var st = rb.Connected ? (rb.Emergency || rb.Blocked ? 'error' : (rb.Busy ? 'busy' : (rb.Available ? 'ready' : 'paused'))) : 'offline';
    out += '<div class="manifest-section">Robot Status</div>';
    out += '<div class="manifest-facts">';
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
    out += '<div class="manifest-facts">';
    out += '<div>' + field('State', '<span class="badge badge-' + escapeHtml(data.vendor_detail.State) + '">' + escapeHtml(data.vendor_detail.State) + '</span>' + (data.vendor_detail.IsTerminal ? ' (terminal)' : '')) + '</div>';
    if (vd.fromLoc) out += '<div>' + fieldH('From Location', vd.fromLoc) + '</div>';
    if (vd.toLoc) out += '<div>' + fieldH('To Location', vd.toLoc) + '</div>';
    out += '</div>';

    var hasSubOrders = vd.containerName || vd.goodsId || vd.loadOrderId || vd.unloadOrderId;
    if (hasSubOrders) {
      out += '<div class="manifest-facts">';
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
        return h`<tr class="row-click" data-action="openOrderModal:${c.id}">
          <td>${c.sequence}</td><td>${c.id}</td><td>${c.order_type}</td>
          <td><span class="badge badge-${c.status}">${c.status}</span></td>
          <td>${c.source_node}</td><td>${c.delivery_node}</td><td>${c.robot_id}</td>
        </tr>`;
      })
    }</tbody></table>`;
  }

  // ── TIMELINE ──
  //
  // ── IT STARTS AT THE ORDER, NOT AT THE FIRST ROW ──────────────────────────
  //
  // order_history records status CHANGES, and a row is written in the same
  // transaction as every change — so nothing is ever lost. But an order's
  // CREATION writes no row, and a gate that parks a blocked order in its ENTRY
  // status changes nothing, so it writes none either. The result was a panel
  // that began at whatever happened first and silently dropped everything
  // before it.
  //
  // Measured at Springfield 2026-08-11: 34 of 110 complex orders in two days had
  // a gap between created_at and their first history row; the average was 28
  // MINUTES and the worst 7h42m. Every other order type was a clean zero — which
  // is why the panel looks trustworthy right up until the order where it isn't,
  // and the orders it truncates are the interesting ones: the ones that WAITED.
  //
  // Both facts are already in the database, so this is a read-side fix and it
  // works on every order already stored. What is NOT stored is what the order was
  // DOING in that window — queue_cause is a current-value column that gets
  // overwritten — so the gap is marked as unaccounted rather than guessed at. A
  // panel that admits what it does not know beats one that implies nothing
  // happened.
  if (data.history && data.history.length > 0) {
    out += '<div class="manifest-section">History</div>';
    var lead = '';
    if (o && o.created_at) {
      lead = h`<li>
          <span class="tl-time">${{__html:true, value: formatTime(o.created_at)}}</span>
          <span class="badge badge-xs">created</span>
          <span class="tl-detail">order created</span>
        </li>`;
      var gapSecs = Math.round(
        (new Date(data.history[0].created_at).getTime() - new Date(o.created_at).getTime()) / 1000);
      // 60s, matching the threshold the Springfield measurement used. Below that
      // is transaction timing, not a wait worth a line.
      if (isFinite(gapSecs) && gapSecs > 60) {
        lead += h`<li class="tl-unaccounted">
            <span class="tl-time">—</span>
            <span class="badge badge-xs">unaccounted</span>
            <span class="tl-detail">${durationText(gapSecs) + ' before the first recorded change — the order existed and nothing was written for it'}</span>
          </li>`;
      }
    }
    out += h`<ul class="timeline-list">${{__html:true, value: lead}}${
      data.history.map(function(ev) {
        return h`<li>
          <span class="tl-time">${{__html:true, value: formatTime(ev.created_at)}}</span>
          <span class="badge badge-xs badge-${ev.status}">${ev.status}</span>
          ${ev.detail ? {__html:true, value: h`<span class="tl-detail">${ev.detail}</span>`} : ''}
        </li>`;
      })
    }</ul>`;
  }

  // ── CONTROLS ──
  // Rendered here rather than only on the detail page, so the modal and the
  // permalink are identical in what they can DO, not just what they show.
  // Before this, "peek at the order" and "act on the order" were split
  // across two surfaces and the modal was read-only.
  if (isAuthenticated()) {
    out += '<div class="manifest-section">Controls</div>';
    out += '<div class="manifest-controls">';
    out += '<div class="ctl-msg text-muted text-sm"></div>';
    out += '<div class="flex gap-1 items-center flex-wrap">';
    // can_cancel comes from the server (www.canCancelStatus), the same gate
    // engine.TerminateOrder applies — never re-derived from a status list here.
    if (data.can_cancel) {
      out += '<button class="btn btn-danger btn-sm" data-action="terminateOrder:' + o.id + '">Terminate Order</button>';
    }
    // Hard release (W3), beside Terminate because it is the same class of verb:
    // an engineer overriding the machinery. can_hard_release comes from the
    // server — TRUE only for a wait CORE owns — for the same reason can_cancel
    // does: never re-derive a gate in JS that the handler also applies. A
    // STATION-owned wait is released from the station's board, by the person who
    // can see the cell.
    if (data.can_hard_release) {
      out += '<button class="btn btn-warning btn-sm" data-action="hardReleaseOrder:' + o.id + '">Hard Release</button>';
    }
    if (o.status === 'delivered') {
      out += '<button class="btn btn-warning btn-sm" data-action="forceConfirmDelivered:' + o.id + '">Force Confirm</button>';
    }
    out += '<label class="order-priority-label">Priority:</label>';
    out += '<input type="number" class="form-input order-priority-input ctl-priority" value="' + (o.priority || 0) + '">';
    out += '<button class="btn btn-sm" data-action="setOrderPriority:' + o.id + '">Set Priority</button>';
    out += '</div></div>';
  }

  // Footer
  var links = [];
  if (o.vendor_order_id) {
    links.push('<a href="/missions/' + o.id + '" title="View mission telemetry, timeline, and robot tracking for this order">Mission Telemetry</a>');
  }
  if (links.length) out += '<div class="manifest-footer">' + links.join(' &middot; ') + '</div>';

  out += '</div>'; // end manifest
  return out;
}

function renderOrderModal(data) {
  document.getElementById('order-modal-content').innerHTML = buildManifest(data);
}

// Deep link: /orders?open=N opens that order's modal on load. There is no
// separate detail page any more — one order view, reachable by link.
// /orders/detail?id=N redirects here so old links and bookmarks still land
// on the order.
(function openFromQuery() {
  var id = new URLSearchParams(location.search).get('open');
  if (id) openOrderModal(id);
})();

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
// Kept from the two fetches below so the staged tab can answer "what is at the
// node you just picked?" without a round trip per keystroke. Advisory only —
// the server reads the bin itself at submit (payloadAtSource in
// handlers_orders.go), so a stale cache shows a stale sentence, never a wrong
// order.
var _moNodes = [];
var _moBins = [];

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
  document.querySelectorAll('#manual-order-modal .tab-btn').forEach(function(t) { t.classList.remove('active'); });
  document.querySelectorAll('.manual-order-tab-content').forEach(function(c) { c.classList.remove('active'); });
  document.getElementById('manual-order-tab-' + name).classList.add('active');
  btn.classList.add('active');
  document.getElementById('mo-status').textContent = '';
  updateManualOrderQuantityVisibility();
}

// buildNodeOptionsHTML renders the manual-order node <option>s with the topology
// made visible: synthetic containers (NGRP "group" / LANE "lane") are badged and
// their child slots nested beneath them, so a container no longer reads like a
// targetable slot — operators were dead-ending by picking a synthetic group like
// "Supermarket Area". Display-only: /api/nodes already carries node_type_code /
// is_synthetic / parent_id, and every node stays selectable (a group is a valid
// source for a group-retrieve). Mirrors the edge manual-order picker.
function buildNodeOptionsHTML(nodes) {
  var byId = {};
  nodes.forEach(function(n) { byId[n.id] = n; });
  function typeLabel(n) {
    var c = (n.node_type_code || '').toUpperCase();
    if (c === 'NGRP') return 'group';
    if (c === 'LANE') return 'lane';
    return n.is_synthetic ? 'group' : '';
  }
  // A container with no zone of its own sits with its first child's zone.
  function zoneOf(n) {
    if (n.zone) return n.zone;
    if (n.is_synthetic) {
      var kid = nodes.find(function(c) { return c.parent_id === n.id && c.zone; });
      if (kid) return kid.zone;
    }
    return 'Other';
  }
  function opt(n, indent) {
    var t = typeLabel(n);
    var label = (indent ? '  ↳ ' : '') + n.name + (t ? '  · ' + t : '');
    return '<option value="' + escapeHtml(n.name) + '">' + escapeHtml(label) + '</option>';
  }
  var byZone = {};
  nodes.forEach(function(n) {
    if (!n.enabled) return;
    var z = zoneOf(n);
    (byZone[z] = byZone[z] || []).push(n);
  });
  var byName = function(a, b) { return a.name.localeCompare(b.name); };
  var html = '<option value="">— select —</option>';
  Object.keys(byZone).sort().forEach(function(zone) {
    var roots = [], kids = {};
    byZone[zone].forEach(function(n) {
      if (n.parent_id && byId[n.parent_id]) (kids[n.parent_id] = kids[n.parent_id] || []).push(n);
      else roots.push(n);
    });
    roots.sort(byName);
    html += '<optgroup label="' + escapeHtml(zone) + '">';
    roots.forEach(function(root) {
      html += opt(root, false);
      (kids[root.id] || []).sort(byName).forEach(function(k) { html += opt(k, true); });
    });
    html += '</optgroup>';
  });
  return html;
}

function loadManualOrderDropdowns() {
  apiGet('/api/nodes')
    .then(function(nodes) {
      _moNodes = nodes || [];
      var html = buildNodeOptionsHTML(nodes);
      // Transport tab
      document.getElementById('mo-source').innerHTML = html;
      document.getElementById('mo-delivery').innerHTML = html;
      // Staged tab
      document.getElementById('mo-staged-source').innerHTML = html;
      document.getElementById('mo-staged-staging').innerHTML = html;
      document.getElementById('mo-staged-delivery').innerHTML = html;
      // Move-robot tab
      document.getElementById('mo-moverobot-dest').innerHTML = html;
      stagedSourceChanged();
    })
    .catch(function(e) { console.error('loadManualOrderDropdowns nodes', e); });

  // The transport tab's retrieve / retrieve_empty payload is the REQUEST — "bring
  // me a bin of X" — so it is a genuine choice and keeps the full catalog. The
  // staged tab's is not, and no longer has a list to fish through.
  apiGet('/api/payloads/templates')
    .then(function(bps) {
      var html = '<option value="">— none —</option>';
      for (var i = 0; i < bps.length; i++) {
        html += '<option value="' + escapeHtml(bps[i].code) + '">' + escapeHtml(bps[i].code) + ' — ' + escapeHtml(bps[i].description) + '</option>';
      }
      document.getElementById('mo-payload').innerHTML = html;
    })
    .catch(function(e) { console.error('loadManualOrderDropdowns payloads', e); });

  loadManualOrderBinDropdown();
}

// stagedSourceChanged answers "what will this order actually be carrying?" from
// the source the operator just named, so the form does not ask them.
//
// Two shapes, because they are two different questions:
//   - a concrete node holds a bin, and the bin knows what it is. Read it out;
//     post nothing. The server reads the same bin at submit and is the one that
//     decides, so a stale cache here cannot produce a wrong order.
//   - a group holds several, and the payload is how you say WHICH one. That is
//     a real choice, so it keeps a picker — listing only what is parked in that
//     group, not the whole catalog.
function stagedSourceChanged() {
  var name = document.getElementById('mo-staged-source').value;
  var derived = document.getElementById('mo-staged-payload-derived');
  var groupWrap = document.getElementById('mo-staged-payload-group');
  var readout = document.getElementById('mo-staged-payload-readout');
  var sel = document.getElementById('mo-staged-payload');

  function showReadout(text) {
    derived.classList.remove('hide');
    groupWrap.classList.add('hide');
    readout.textContent = text;
  }

  if (!name) { showReadout('Select a source node.'); return; }

  var node = _moNodes.find(function(n) { return n.name === name; });
  if (!node) { showReadout('Read from the bin at the source when the order is submitted.'); return; }

  if (node.is_synthetic) {
    // Every node under this container, however deep (group → lane → slot).
    var under = {}, added = true;
    under[node.id] = true;
    while (added) {
      added = false;
      _moNodes.forEach(function(n) {
        if (!under[n.id] && n.parent_id && under[n.parent_id]) { under[n.id] = true; added = true; }
      });
    }
    var codes = [];
    _moBins.forEach(function(b) {
      var at = _moNodes.find(function(n) { return n.name === b.node_name; });
      if (at && under[at.id] && b.payload_code && codes.indexOf(b.payload_code) === -1) codes.push(b.payload_code);
    });
    if (!codes.length) { showReadout('Nothing is parked in ' + name + ' right now.'); return; }
    derived.classList.add('hide');
    groupWrap.classList.remove('hide');
    var html = '<option value="">— any —</option>';
    codes.sort().forEach(function(c) { html += '<option value="' + escapeHtml(c) + '">' + escapeHtml(c) + '</option>'; });
    sel.innerHTML = html;
    return;
  }

  var here = _moBins.filter(function(b) { return b.node_name === name; });
  if (!here.length) { showReadout('No bin at ' + name + '.'); return; }
  if (here.length > 1) { showReadout(here.length + ' bins at ' + name + ' — which one is picked is decided at dispatch.'); return; }
  showReadout(here[0].payload_code
    ? here[0].label + ' — ' + here[0].payload_code
    : here[0].label + ' — empty');
}

function loadManualOrderBinDropdown() {
  apiGet('/api/bins/available')
    .then(function(bins) {
      _moBins = bins || [];
      stagedSourceChanged();
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

// Field visibility for the Transport tab, derived from the order type.
//
// Two bugs lived here. The element was looked up as 'mo-pickup-group' —
// that is the Edge template's id; Core's is 'mo-source-group', so every
// call threw on the null deref and none of the later toggles ran. And the
// toggles wrote element.style.display while the template marks the
// initially-hidden groups with class="hide" (.hide { display:none });
// clearing an inline display can't beat a class, so the Bin and Quantity
// groups could never appear. Toggling the class fixes both, and matches
// the style guide's "visibility is derived from state" rule.
function manualOrderTransportTypeChanged() {
  var t = document.getElementById('mo-transport-type').value;
  var batch = (t === 'retrieve' || t === 'retrieve_empty');
  var specific = (t === 'retrieve_specific');
  var vis = {
    'mo-source-group': !specific && !batch,  // Move only
    'mo-delivery-group': true,
    'mo-payload-group': !specific && t !== 'move',
    'mo-bin-group': specific,
    'mo-quantity-group': batch,
  };
  Object.keys(vis).forEach(function(id) {
    var el = document.getElementById(id);
    if (el) el.classList.toggle('hide', !vis[id]);
  });
}

function updateManualOrderQuantityVisibility() {
  if (_moActiveTab !== 'transport') {
    document.getElementById('mo-quantity-group').classList.add('hide');
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
      body.delivery_node = document.getElementById('mo-delivery').value;
      if (t !== 'move') body.payload_code = document.getElementById('mo-payload').value;

      if (t === 'move' && !body.source_node) {
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
    // Only sent when the source is a group, where it names WHICH bin to take.
    // For a concrete source the server reads the bin itself, so the page
    // deliberately sends nothing rather than a second opinion.
    var groupPick = document.getElementById('mo-staged-payload-group');
    if (groupPick && !groupPick.classList.contains('hide')) {
      body.payload_code = document.getElementById('mo-staged-payload').value;
    }
    if (!body.source_node) { status.textContent = 'Source node is required'; status.style.color = 'var(--danger)'; return; }
    if (!body.staging_node) { status.textContent = 'Staging node is required'; status.style.color = 'var(--danger)'; return; }
    if (!body.delivery_node) { status.textContent = 'Delivery node is required'; status.style.color = 'var(--danger)'; return; }
  } else if (tab === 'move_robot') {
    // Not an order. This tab talks to the fleet directly, so it posts
    // somewhere else, gets a different answer back, and has no order number to
    // show or table row to reload for.
    var dest = document.getElementById('mo-moverobot-dest').value;
    if (!dest) { status.textContent = 'Destination node is required'; status.style.color = 'var(--danger)'; return; }
    status.textContent = 'Submitting...';
    status.style.color = 'var(--text-muted)';
    btn.disabled = true;
    apiPost('/api/robots/move', { delivery_node: dest, priority: body.priority })
      .then(function(data) {
        status.textContent = 'Robot sent to ' + data.destination;
        status.style.color = 'var(--success)';
        btn.disabled = false;
      })
      .catch(function(e) {
        console.error('moveRobot', e);
        status.textContent = (typeof e === 'string' && e) ? e : 'Network error';
        status.style.color = 'var(--danger)';
        btn.disabled = false;
      });
    return;
  }

  status.textContent = 'Submitting...';
  status.style.color = 'var(--text-muted)';
  btn.disabled = true;

  apiPost('/api/orders/spot', body)
    .then(function(data) {
      // Getting here means an order exists and is on its way. Anything else
      // arrives as a rejection and lands in .catch below, so there is no
      // failure to check for here any more -- this used to read
      // data.status === 'failed' out of a 200 and print "created (failed)".
      var msg;
      if (data.count && data.count > 1) {
        msg = data.count + ' orders created (first: #' + data.order_id + ')';
      } else {
        msg = 'Order #' + data.order_id + ' created (' + data.status + ')';
      }
      status.textContent = msg;
      status.style.color = 'var(--success)';
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
    hardReleaseOrder,
    loadManualOrderBinDropdown,
    loadManualOrderDropdowns,
    manualOrderTransportTypeChanged,
    openManualOrderModal,
    openOrderModal,
    orderControlPost,
    renderOrderModal,
    cancelOrderFromRow,
    setOrderPriority,
    stagedSourceChanged,
    submitManualOrder,
    switchManualOrderTab,
    terminateOrder,
    updateManualOrderQuantityVisibility
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
