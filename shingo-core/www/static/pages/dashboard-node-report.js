import { onSSE, setSSEReloadOnBuild } from '/static/shared/utils.js';

(function () {
  var body = document.body;
  var dashboardId = body.getAttribute('data-dashboard-id');
  var demoParam = new URLSearchParams(window.location.search).get('demo');
  var demoQ = demoParam ? '&demo=' + encodeURIComponent(demoParam) : '';
  var MAX_ROWS = 8;

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
    var activeClass = r.is_active_style ? ' nr-row-active' : '';

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

    var numCols = Math.ceil(rows.length / MAX_ROWS);
    if (numCols < 1) numCols = 1;
    var fontSize = Math.max(0.65, Math.min(1.15, 1.15 - (numCols - 1) * 0.12));
    var filled = 0;
    var thead = headerHTML(layout);
    var html = '';
    for (var start = 0; start < rows.length; start += MAX_ROWS) {
      var chunk = rows.slice(start, start + MAX_ROWS);
      var borderStyle = start > 0 ? 'border-left:3px solid rgba(255,255,255,0.15);' : '';
      html += '<table class="nr-col-table" style="font-size:' + fontSize + 'rem;' + borderStyle + '">' +
        '<thead><tr>' + thead + '</tr></thead><tbody>';
      for (var i = 0; i < chunk.length; i++) {
        var r = chunk[i];
        if (r.occupied) filled++;
        html += '<tr class="' + (r.occupied ? 'nr-row-filled' : 'nr-row-empty') + (r.is_active_style ? ' nr-row-active' : '') + '">' +
          rowHTML(r, layout) + '</tr>';
      }
        var colSpan = layout === 'shared_window' ? 4 : 5;
        for (var pad = chunk.length; pad < MAX_ROWS; pad++) {
          html += '<tr class="nr-row-empty"><td colspan="' + colSpan + '">&nbsp;</td></tr>';
        }
      html += '</tbody></table>';
    }
    container.innerHTML = html;

    var tbodies = container.querySelectorAll('.nr-col-table tbody');
    if (tbodies.length > 1) {
      var first = tbodies[0];
      first.style.overflowY = 'auto';
      for (var t = 1; t < tbodies.length; t++) {
        (function (slave) {
          first.addEventListener('scroll', function () {
            slave.scrollTop = first.scrollTop;
          });
        })(tbodies[t]);
      }
    }

    if (stats) {
      stats.innerHTML =
        '<span class="nr-stat-label">' + filled + ' / ' + rows.length + ' filled</span>';
    }
  }

  function renderTransit(rows) {
    var el = document.getElementById('nr-transit');
    if (!el) return;
    if (!rows || rows.length === 0) {
      el.innerHTML = '';
      el.style.display = 'none';
      return;
    }
    el.style.display = 'block';
    var parts = [];
    for (var i = 0; i < rows.length; i++) {
      var r = rows[i];
      if (r.is_empty) {
        var src = r.source_node ? esc(r.source_node) + ' \u2192 ' : '';
        parts.push('\u25c6 EMPTY ' + src + 'returning');
      } else if (r.is_partial) {
        var src2 = r.source_node ? esc(r.source_node) + ' \u2192 ' : '';
        parts.push('\u25c6 PARTIAL (' + r.uop_remaining + ' UoP) ' + src2 + 'returning');
      } else {
        var arrow = '\u2192 ' + esc(r.payload_code);
        if (r.dest_node) {
          arrow += ' \u2192 ' + esc(r.dest_node);
        } else {
          arrow += ' in transit';
        }
        parts.push(arrow);
      }
    }
    el.innerHTML = parts.join('  \u2502  ');
  }

  function load() {
    fetch('/api/dashboards/' + encodeURIComponent(dashboardId) + '/node-report?t=' + Date.now() + demoQ)
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
        renderTransit(data.transit || []);
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
