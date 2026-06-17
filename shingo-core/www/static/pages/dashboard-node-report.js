import { onSSE, setSSEReloadOnBuild } from '/static/shared/utils.js';

(function () {
  var body = document.body;
  var dashboardId = body.getAttribute('data-dashboard-id');

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

  function esc(s) {
    var d = document.createElement('span');
    d.textContent = (s === null || s === undefined) ? '' : s;
    return d.innerHTML;
  }

  function renderHeader(data) {
    var table = document.getElementById('nr-table');
    var thead = table ? table.querySelector('thead tr') : null;
    if (!thead) return;
    var layout = data.layout || 'dedicated_positions';
    if (layout === 'shared_window') {
      thead.innerHTML =
        '<th>Payload</th><th>Status</th><th>Node</th><th>UOP</th>';
    } else {
      thead.innerHTML =
        '<th>Node</th><th>Node Group</th><th>Status</th><th>Payload</th><th>UOP</th>';
    }
  }

  function render(layout, rows) {
    var tbody = document.getElementById('nr-body');
    var empty = document.getElementById('nr-empty');
    var table = document.getElementById('nr-table');
    var stats = document.getElementById('nr-stats');
    if (!tbody) return;

    tbody.innerHTML = '';

    if (!rows || rows.length === 0) {
      if (table) table.style.display = 'none';
      if (empty) empty.style.display = 'block';
      if (stats) stats.innerHTML = '';
      return;
    }
    if (table) table.style.display = '';

    var filled = 0;
    var isShared = layout === 'shared_window';
    for (var i = 0; i < rows.length; i++) {
      var r = rows[i];
      var tr = document.createElement('tr');
      if (r.occupied) {
        tr.className = 'nr-row-filled';
        filled++;
      } else {
        tr.className = 'nr-row-empty';
      }

      var statusHTML = r.occupied
        ? '<span class="nr-dot nr-dot-filled"></span> FILLED'
        : '<span class="nr-dot nr-dot-empty nr-dot-pulse"></span> EMPTY';

      if (isShared) {
        var payloadHTML = esc(r.payload_code);
        var nodeHTML = r.node_name
          ? esc(r.node_name) + (r.group_name ? ' <span class="nr-uop">(' + esc(r.group_name) + ')</span>' : '')
          : '<span class="nr-empty-payload">&mdash;</span>';
        var uopText = r.uop_remaining ? r.uop_remaining + ' UOP' : '\u2014';
        tr.innerHTML =
          '<td class="nr-payload">' + payloadHTML + '</td>' +
          '<td class="nr-status">' + statusHTML + '</td>' +
          '<td class="nr-node">' + nodeHTML + '</td>' +
          '<td class="nr-uop">' + esc(uopText) + '</td>';
      } else {
        var payloadHTML = r.payload_code
          ? esc(r.payload_code)
          : '<span class="nr-empty-payload">&mdash;</span>';
        var uopText = r.uop_remaining ? r.uop_remaining + ' UOP' : '\u2014';
        tr.innerHTML =
          '<td class="nr-node">' + esc(r.node_name) + '</td>' +
          '<td class="nr-group">' + esc(r.group_name || '') + '</td>' +
          '<td class="nr-status">' + statusHTML + '</td>' +
          '<td class="nr-payload">' + payloadHTML + '</td>' +
          '<td class="nr-uop">' + esc(uopText) + '</td>';
      }
      tbody.appendChild(tr);
    }

    if (stats) {
      stats.innerHTML =
        '<span class="nr-stat-label">' + filled + ' / ' + rows.length + ' filled</span>';
    }
  }

  function load() {
    fetch('/api/dashboards/' + encodeURIComponent(dashboardId) + '/node-report?t=' + Date.now())
      .then(function (r) {
        if (!r.ok) throw new Error('HTTP ' + r.status);
        return r.json();
      })
      .then(function (data) {
        var titleEl = document.getElementById('nr-title');
        var subEl = document.getElementById('nr-subtitle');
        if (data.loader_name) {
          if (titleEl) titleEl.textContent = data.loader_name;
          var count = data.homes_count || data.payloads_count || 0;
          var label = data.layout === 'shared_window' ? 'payload' : 'position';
          if (subEl) subEl.textContent = data.layout + ' \u00b7 ' + count + ' ' + label + (count !== 1 ? 's' : '');
        }
        renderHeader(data);
        render(data.layout, data.rows || []);
      })
      .catch(function (e) {
        console.error('node-report: load failed:', e);
      });
  }

  var reloadTimer = null;
  function scheduleReload() {
    clearTimeout(reloadTimer);
    reloadTimer = setTimeout(load, 250);
  }

  function init() {
    setSSEReloadOnBuild(true);
    load();
    onSSE('connected', function () { setConnected(true); load(); });
    onSSE('disconnected', function () { setConnected(false); });
    onSSE('bin-update', scheduleReload);
    onSSE('node-update', scheduleReload);
    onSSE('inventory-update', scheduleReload);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
