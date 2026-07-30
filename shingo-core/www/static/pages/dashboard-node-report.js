import { onSSE, setSSEReloadOnBuild } from '/static/shared/utils.js';

(function () {
  var body = document.body;
  var dashboardId = body.getAttribute('data-dashboard-id');
  var CHUNK = 0;

  function calcChunk() {
    var main = document.getElementById('dash-main');
    if (!main) return 10;
    var avail = main.clientHeight;
    var transit = document.getElementById('nr-transit');
    if (transit && transit.style.display !== 'none') {
      avail -= transit.offsetHeight + parseFloat(getComputedStyle(transit).marginBottom);
    }
    var stats = document.getElementById('nr-stats');
    if (stats) {
      avail -= stats.offsetHeight + parseFloat(getComputedStyle(stats).marginBottom);
    }
    avail -= parseFloat(getComputedStyle(main).paddingTop) + parseFloat(getComputedStyle(main).paddingBottom);
    var tmp = document.createElement('table');
    tmp.className = 'nr-col-table';
    tmp.innerHTML = '<tbody><tr><td>&nbsp;</td></tr></tbody>';
    tmp.style.visibility = 'hidden';
    tmp.style.position = 'absolute';
    document.body.appendChild(tmp);
    var rowH = tmp.querySelector('tbody tr').offsetHeight;
    var head = tmp.parentElement;
    var th = document.createElement('table');
    th.className = 'nr-col-table';
    th.innerHTML = '<thead><tr><th>&nbsp;</th></tr></thead>';
    th.style.visibility = 'hidden';
    th.style.position = 'absolute';
    document.body.appendChild(th);
    var headH = th.querySelector('thead').offsetHeight;
    document.body.removeChild(tmp);
    document.body.removeChild(th);
    if (rowH <= 0 || headH <= 0) return 10;
    var count = Math.floor((avail - headH) / rowH);
    return count > 0 ? count : 1;
  }

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

    if (CHUNK <= 0) CHUNK = calcChunk();
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
      var arrow = '\u2192 ' + esc(r.payload_code);
      if (r.dest_node) {
        arrow += ' \u2192 ' + esc(r.dest_node);
      } else {
        arrow += ' in transit';
      }
      parts.push(arrow);
    }
    el.innerHTML = parts.join('  \u2502  ');
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
