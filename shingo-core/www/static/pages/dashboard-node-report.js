import { onSSE, setSSEReloadOnBuild } from '/static/shared/utils.js';

(function () {
  var body = document.body;
  var dashboardId = body.getAttribute('data-dashboard-id');
  var CHUNK = 10;

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

  function headerHTML(layout) {
    if (layout === 'shared_window') {
      return '<th>Payload</th><th>Status</th><th>Node</th><th>UoP</th>';
    }
    return '<th>Node</th><th>Node Group</th><th>Status</th><th>Payload</th><th>UoP</th>';
  }

  function rowHTML(r, layout) {
    var isShared = layout === 'shared_window';
    var statusHTML = r.occupied
      ? '<span class="nr-dot nr-dot-filled"></span> FILLED'
      : '<span class="nr-dot nr-dot-empty nr-dot-pulse"></span> EMPTY';
    var uopText = r.uop_remaining ? r.uop_remaining + ' UoP' : '\u2014';

    if (isShared) {
      var payloadHTML = esc(r.payload_code);
      var nodeHTML = r.node_name
        ? esc(r.node_name) + (r.group_name ? ' <span class="nr-group">(' + esc(r.group_name) + ')</span>' : '')
        : '<span class="nr-empty-payload">&mdash;</span>';
      return '<td class="nr-payload">' + payloadHTML + '</td>' +
        '<td class="nr-status">' + statusHTML + '</td>' +
        '<td class="nr-node">' + nodeHTML + '</td>' +
        '<td class="nr-uop">' + esc(uopText) + '</td>';
    }
    var payloadHTML = r.payload_code
      ? esc(r.payload_code)
      : '<span class="nr-empty-payload">&mdash;</span>';
    return '<td class="nr-node">' + esc(r.node_name) + '</td>' +
      '<td class="nr-group">' + esc(r.group_name || '') + '</td>' +
      '<td class="nr-status">' + statusHTML + '</td>' +
      '<td class="nr-payload">' + payloadHTML + '</td>' +
      '<td class="nr-uop">' + esc(uopText) + '</td>';
  }

  function render(layout, rows) {
    var container = document.getElementById('nr-columns');
    var empty = document.getElementById('nr-empty');
    var stats = document.getElementById('nr-stats');
    if (!container) return;

    if (!rows || rows.length === 0) {
      container.innerHTML = '';
      if (empty) empty.style.display = 'block';
      if (stats) stats.innerHTML = '';
      return;
    }
    if (empty) empty.style.display = 'none';

    var filled = 0;
    var thead = headerHTML(layout);
    var html = '';
    for (var start = 0; start < rows.length; start += CHUNK) {
      var chunk = rows.slice(start, start + CHUNK);
      html += '<table class="nr-col-table"' +
        (start > 0 ? ' style="border-left:3px solid rgba(255,255,255,0.15)"' : '') + '>' +
        '<thead><tr>' + thead + '</tr></thead><tbody>';
      for (var i = 0; i < chunk.length; i++) {
        var r = chunk[i];
        if (r.occupied) filled++;
        html += '<tr class="' + (r.occupied ? 'nr-row-filled' : 'nr-row-empty') + '">' +
          rowHTML(r, layout) + '</tr>';
      }
      html += '</tbody></table>';
    }
    container.innerHTML = html;

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
