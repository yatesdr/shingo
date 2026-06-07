// dashboard.js — chromeless floor-display renderer for the dashboard platform.
//
// v1 renders the 'task-board' kind: a live table of active AMR orders scoped to
// this dashboard's station set. The scoping is server-side — the page fetches
// /api/board/orders?dashboard=<id>, and Core filters by the dashboard's stored
// stations. SSE is used only as a CHANGE-PING: any order-update triggers a
// debounced refetch of the scoped list. That keeps each board pulling just its
// own area's slice and the renderer dead simple (no per-event row diffing), at
// the cost of a small full-list refetch per change — cheap at board scale.
//
// Standalone module: it does NOT load the admin app.js, so it owns its own SSE
// connection, reconnect, and build-id auto-reload (a kiosk has no operator to
// disrupt, so picking up a new Core build by reloading is the right call).
//
// Adding a new dashboard kind: branch on `kind` in init() and render into
// #dash-main; register the kind's renderer template in handlers_dashboards.go.

import { onSSE, setSSEReloadOnBuild } from '/static/shared/utils.js';

(function () {
  var body = document.body;
  var dashboardId = body.getAttribute('data-dashboard-id');
  var kind = body.getAttribute('data-dashboard-kind') || 'task-board';

  // ── Header chrome: clock + connection dot ──────────────────────────
  function tickClock() {
    var el = document.getElementById('dash-clock');
    if (el) el.textContent = new Date().toLocaleTimeString();
  }
  setInterval(tickClock, 1000);
  tickClock();

  function setConnected(ok) {
    var el = document.getElementById('dash-conn');
    if (el) el.className = 'dash-conn ' + (ok ? 'dash-conn-ok' : 'dash-conn-down');
  }

  // ── Formatting helpers ─────────────────────────────────────────────
  function esc(s) {
    var d = document.createElement('span');
    d.textContent = (s === null || s === undefined) ? '' : s;
    return d.innerHTML;
  }

  function formatETA(str) {
    if (!str) return '-';
    var d = new Date(str);
    if (isNaN(d.getTime())) return '-';
    var diff = d - Date.now();
    if (diff <= 0) return 'arriving';
    var mins = Math.floor(diff / 60000);
    if (mins < 1) return '<1m';
    if (mins < 60) return mins + 'm';
    var hrs = Math.floor(mins / 60);
    return hrs + 'h' + (mins % 60) + 'm';
  }

  var STATUS_LABELS = {
    pending: 'Pending', queued: 'Queued', acknowledged: 'ACK', staged: 'Staged',
    dispatched: 'Dispatched', in_transit: 'In Transit', blocked: 'Blocked',
    delivered: 'Delivered', completed: 'Done'
  };
  function statusLabel(s) { return STATUS_LABELS[s] || s || '-'; }
  function statusClass(s) { return 'st-' + (s || 'unknown'); }

  // ── task-board renderer ────────────────────────────────────────────
  var seen = {}; // order_id -> true, to flash only genuinely new rows

  function rowHTML(o) {
    return '<td class="r-robot">' + esc(o.robot_id || '-') + '</td>' +
      '<td>' + esc(o.source_node || '-') + '</td>' +
      '<td class="r-payload">' + esc(o.payload_code || '-') + '</td>' +
      '<td>' + esc(o.current_station || '-') + '</td>' +
      '<td>' + esc(o.delivery_node || '-') + '</td>' +
      '<td class="r-status ' + statusClass(o.status) + '">' + esc(statusLabel(o.status)) + '</td>' +
      '<td class="r-eta">' + esc(formatETA(o.eta)) + '</td>';
  }

  function render(list) {
    var tbody = document.getElementById('board-body');
    if (!tbody) return;
    var nextSeen = {};
    tbody.innerHTML = '';
    for (var i = 0; i < list.length; i++) {
      var o = list[i];
      nextSeen[o.order_id] = true;
      var tr = document.createElement('tr');
      tr.className = statusClass(o.status) + (seen[o.order_id] ? '' : ' row-new');
      tr.innerHTML = rowHTML(o);
      tbody.appendChild(tr);
    }
    seen = nextSeen;
    var empty = document.getElementById('board-empty');
    if (empty) empty.style.display = list.length ? 'none' : 'block';
  }

  function load() {
    fetch('/api/board/orders?dashboard=' + encodeURIComponent(dashboardId)).then(function (r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    }).then(function (data) {
      render(data || []);
    }).catch(function (e) {
      console.error('dashboard: load failed:', e);
    });
  }

  var reloadTimer = null;
  function scheduleReload() {
    clearTimeout(reloadTimer);
    reloadTimer = setTimeout(load, 250);
  }

  // ── SSE via the shared onSSE bus (Q-020): ONE EventSource per tab. The bus
  // owns connection, reconnect/backoff, and build-id detection;
  // setSSEReloadOnBuild(true) makes a build change reload the kiosk (no
  // operator to dismiss a refresh banner — adopt the new Core build). The
  // bus's synthetic 'connected' re-fires on every (re)connect (refetch the
  // scoped list) and 'disconnected' drives the offline dot.
  function init() {
    if (kind !== 'task-board') {
      console.warn('dashboard: unsupported kind:', kind);
      return;
    }
    setSSEReloadOnBuild(true);
    onSSE('connected', function () { setConnected(true); load(); });
    onSSE('disconnected', function () { setConnected(false); });
    onSSE('order-update', scheduleReload);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
