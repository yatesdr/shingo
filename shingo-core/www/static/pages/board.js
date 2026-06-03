import { api, el, timeAgo } from '/static/app.js';

(function() {
  var orders = {};
  var filterStation = '';
  var filterStatus = '';
  var es = null;
  var reconnectTimer = null;
  var reconnectDelay = 3000;
  var MAX_RECONNECT_DELAY = 30000;
  var knownStations = {};
  var knownStatuses = {};

  function formatETA(str) {
    if (!str || str === '') return '-';
    var d = new Date(str);
    if (isNaN(d.getTime())) return '-';
    var diff = d - Date.now();
    if (diff <= 0) return 'arriving';
    var mins = Math.floor(diff / 60000);
    if (mins < 1) return '<1m';
    if (mins < 60) return mins + 'm';
    var hrs = Math.floor(mins / 60);
    var rem = mins % 60;
    return hrs + 'h' + rem + 'm';
  }

  function statusLabel(s) {
    if (!s) return '-';
    var map = {
      'pending': 'Pending',
      'queued': 'Queued',
      'acknowledged': 'ACK',
      'staged': 'Staged',
      'dispatched': 'Dispatched',
      'in_transit': 'In Transit',
      'blocked': 'Blocked',
      'delivered': 'Delivered',
      'completed': 'Done',
      'failed': 'Failed',
      'cancelled': 'Cancelled',
      'skipped': 'Skipped'
    };
    return map[s] || s;
  }

  function statusClass(s) {
    return 'status-' + (s || 'unknown');
  }

  function buildRow(o) {
    var tr = document.createElement('tr');
    tr.id = 'board-row-' + o.order_id;
    tr.className = statusClass(o.status);
    tr.innerHTML =
      '<td class="board-robot">' + esc(o.robot_id || '-') + '</td>' +
      '<td class="board-cell">' + esc(o.source_node || '-') + '</td>' +
      '<td class="board-payload">' + esc(o.payload_code || '-') + '</td>' +
      '<td class="board-current">' + esc(o.current_station || '-') + '</td>' +
      '<td class="board-cell">' + esc(o.delivery_node || '-') + '</td>' +
      '<td class="board-status ' + statusClass(o.status) + '">' + statusLabel(o.status) + '</td>' +
      '<td class="board-eta">' + formatETA(o.eta) + '</td>';
    return tr;
  }

  function esc(s) {
    var d = document.createElement('span');
    d.textContent = s;
    return d.innerHTML;
  }

  function updateRow(o) {
    var tbody = document.getElementById('board-body');
    if (!tbody) return;
    var existing = document.getElementById('board-row-' + o.order_id);
    if (existing) {
      var newRow = buildRow(o);
      tbody.replaceChild(newRow, existing);
    } else {
      tbody.appendChild(buildRow(o));
      flashRow(o.order_id, 'status-flash-green');
    }
  }

  function removeRow(orderID) {
    var tbody = document.getElementById('board-body');
    if (!tbody) return;
    var row = document.getElementById('board-row-' + orderID);
    if (!row) return;
    flashRow(orderID, 'status-flash-red', function() {
      if (row.parentNode) row.parentNode.removeChild(row);
    });
  }

  function flashRow(orderID, cls, cb) {
    var row = document.getElementById('board-row-' + orderID);
    if (!row) { if (cb) cb(); return; }
    row.classList.add(cls);
    row.addEventListener('animationend', function() {
      row.classList.remove(cls);
      if (cb) cb();
    }, { once: true });
  }

  function showEmpty(show) {
    var empty = document.getElementById('board-empty');
    if (!empty) return;
    empty.style.display = show ? 'block' : 'none';
  }

  function applyFilters() {
    var tbody = document.getElementById('board-body');
    if (!tbody) return;
    var rows = tbody.querySelectorAll('tr[id]');
    var anyVisible = false;
    for (var i = 0; i < rows.length; i++) {
      var row = rows[i];
      var id = row.id.replace('board-row-', '');
      var o = orders[id];
      if (!o) { row.style.display = 'none'; continue; }
      var matchStation = !filterStation || o.station_id === filterStation;
      var matchStatus = !filterStatus || o.status === filterStatus;
      var visible = matchStation && matchStatus;
      row.style.display = visible ? '' : 'none';
      if (visible) anyVisible = true;
    }
    showEmpty(!anyVisible);
  }

  function restoreFilters(data) {
    var selStation = document.getElementById('filter-station');
    var selStatus = document.getElementById('filter-status');
    if (!selStation || !selStatus) return;

    for (var i = 0; i < data.length; i++) {
      var st = data[i].station_id;
      if (st && !knownStations[st]) {
        knownStations[st] = true;
        var opt = document.createElement('option');
        opt.value = st;
        opt.textContent = st;
        selStation.appendChild(opt);
      }
    }

    for (var j = 0; j < data.length; j++) {
      var stat = data[j].status;
      if (stat && !knownStatuses[stat]) {
        knownStatuses[stat] = true;
        var optS = document.createElement('option');
        optS.value = stat;
        optS.textContent = statusLabel(stat);
        selStatus.appendChild(optS);
      }
    }
  }

  function loadOrders() {
    fetch('/api/board/orders').then(function(r) {
      if (!r.ok) throw new Error(r.status);
      return r.json();
    }).then(function(data) {
      var tbody = document.getElementById('board-body');
      if (!tbody) return;
      tbody.innerHTML = '';
      orders = {};
      for (var i = 0; i < data.length; i++) {
        var o = data[i];
        orders[o.order_id] = o;
        tbody.appendChild(buildRow(o));
      }
      restoreFilters(data);
      applyFilters();
      showEmpty(data.length === 0);
    }).catch(function(err) {
      console.error('board: load orders failed:', err);
    });
  }

  function fetchAndUpdate(orderID) {
    fetch('/api/board/orders?id=' + orderID).then(function(r) {
      if (!r.ok) throw new Error(r.status);
      return r.json();
    }).then(function(o) {
      if (!o) {
        removeRow(orderID);
        delete orders[orderID];
      } else {
        orders[o.order_id] = o;
        updateRow(o);
      }
      applyFilters();
    }).catch(function(err) {
      console.error('board: fetch order ' + orderID + ' failed:', err);
    });
  }

  var terminalStatuses = ['confirmed', 'failed', 'cancelled', 'skipped'];

  function onOrderUpdate(evt) {
    var data;
    try {
      data = JSON.parse(evt.data);
    } catch (e) {
      console.error('board: parse error:', e);
      return;
    }

    var type = data.type;
    var orderID = data.order_id;

    if (!orderID) return;

    if (type === 'eta_update') {
      var existing = orders[orderID];
      if (existing) {
        existing.eta = data.eta;
        updateRow(existing);
      }
      return;
    }

    if (type === 'status_changed') {
      if (terminalStatuses.indexOf(data.new_status) !== -1) {
        removeRow(orderID);
        delete orders[orderID];
        applyFilters();
        return;
      }
      fetchAndUpdate(orderID);
      return;
    }

    if (type === 'dispatched' || type === 'queued') {
      fetchAndUpdate(orderID);
      return;
    }

    if (type === 'completed' || type === 'failed' || type === 'cancelled' || type === 'skipped') {
      removeRow(orderID);
      delete orders[orderID];
      applyFilters();
      return;
    }
  }

  function setConnected(connected) {
    var indicator = document.getElementById('board-connection');
    if (!indicator) return;
    indicator.className = 'board-connection ' + (connected ? 'board-connected' : 'board-disconnected');
  }

  function connectSSE() {
    if (es) {
      es.close();
      es = null;
    }
    if (reconnectTimer) {
      clearTimeout(reconnectTimer);
      reconnectTimer = null;
    }

    es = new EventSource('/events');
    setConnected(true);

    es.addEventListener('connected', function() {
      setConnected(true);
      reconnectDelay = 3000;
      loadOrders();
    });

    es.addEventListener('order-update', onOrderUpdate);

    es.addEventListener('heartbeat', function(evt) {
      setConnected(true);
    });

    es.onerror = function() {
      setConnected(false);
      if (es) {
        es.close();
        es = null;
      }
      reconnectTimer = setTimeout(connectSSE, reconnectDelay);
      reconnectDelay = Math.min(reconnectDelay * 2, MAX_RECONNECT_DELAY);
    };
  }

  function init() {
    connectSSE();

    var selStation = document.getElementById('filter-station');
    if (selStation) {
      selStation.addEventListener('change', function() {
        filterStation = selStation.value;
        applyFilters();
      });
    }

    var selStatus = document.getElementById('filter-status');
    if (selStatus) {
      selStatus.addEventListener('change', function() {
        filterStatus = selStatus.value;
        applyFilters();
      });
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
