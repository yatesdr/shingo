// --- Shared utilities ---

// Debounce: delays execution until `ms` milliseconds after the last call.
// Used to prevent SSE event bursts from saturating the browser main thread.
function debounce(fn, ms) {
  var timer;
  return function() {
    var args = arguments;
    var self = this;
    clearTimeout(timer);
    timer = setTimeout(function() { fn.apply(self, args); }, ms);
  };
}

// HTML escape (replaces per-page esc/escapeHtml)
function escapeHtml(s) {
  if (!s) return '';
  var d = document.createElement('div');
  d.appendChild(document.createTextNode(s));
  return d.innerHTML;
}

// Generic modal show/hide
function showModal(id) {
  document.getElementById(id).classList.add('active');
}
function hideModal(id) {
  document.getElementById(id).classList.remove('active');
}

// Generic JSON request. Throws the server error string (or parsed object's
// `error` field) on non-2xx responses; returns parsed JSON on success.
function api(method, url, body) {
  var opts = { method: method };
  if (body !== undefined && body !== null) {
    opts.headers = { 'Content-Type': 'application/json' };
    opts.body = JSON.stringify(body);
  }
  return fetch(url, opts).then(function(r) {
    if (!r.ok) return r.text().then(function(t) {
      try { throw JSON.parse(t); }
      catch(e) {
        if (typeof e === 'object' && e.error) throw e.error;
        throw t;
      }
    });
    return r.json();
  });
}
function apiPost(url, body) { return api('POST', url, body || {}); }
function apiPut(url, body)  { return api('PUT',  url, body || {}); }
function apiDelete(url)     { return api('DELETE', url); }

// --- Time formatting ---
// timeAgo:        relative ("3m ago"), '-' on falsy.
// formatTime:     local-time string. opts.precision === 'ms' returns
//                 HH:MM:SS.mmm for high-resolution log views.
// formatDuration: human-readable elapsed duration in ms.
function timeAgo(ts) {
  if (!ts) return '-';
  var d = Date.now() - new Date(ts).getTime();
  if (d < 60000) return 'just now';
  if (d < 3600000) return Math.floor(d / 60000) + 'm ago';
  if (d < 86400000) return Math.floor(d / 3600000) + 'h ago';
  return Math.floor(d / 86400000) + 'd ago';
}

function formatTime(ts, opts) {
  if (!ts || ts === '0001-01-01T00:00:00Z') return '-';
  var d = new Date(ts);
  if (isNaN(d.getTime())) return ts;
  if (opts && opts.precision === 'ms') {
    return d.toTimeString().slice(0, 8) + '.' + String(d.getMilliseconds()).padStart(3, '0');
  }
  return d.toLocaleString();
}

function formatDuration(ms) {
  if (!ms || ms <= 0) return '-';
  if (ms < 1000) return ms + 'ms';
  var s = Math.floor(ms / 1000);
  if (s < 60) return s + 's';
  var m = Math.floor(s / 60);
  s = s % 60;
  if (m < 60) return m + 'm ' + s + 's';
  var h = Math.floor(m / 60);
  m = m % 60;
  return h + 'h ' + m + 'm';
}

// Convert UTC timestamps to browser local time
function convertTimestamps() {
  document.querySelectorAll('time[data-utc]').forEach(function(el) {
    var d = new Date(el.getAttribute('data-utc'));
    if (!isNaN(d)) {
      el.textContent = d.toLocaleString();
    }
  });
}
document.addEventListener('DOMContentLoaded', convertTimestamps);

// SSE connection for live updates
(function() {
  let es;

  function connect() {
    es = new EventSource('/events');

    es.addEventListener('order-update', function(e) {
      // Page-specific handlers can override via window.onOrderUpdate
      if (typeof window.onOrderUpdate === 'function') window.onOrderUpdate(e);
    });

    es.addEventListener('inventory-update', function(e) {
      if (typeof window.onInventoryUpdate === 'function') window.onInventoryUpdate(e);
    });

    es.addEventListener('node-update', function(e) {
      if (typeof window.onNodeUpdate === 'function') window.onNodeUpdate(e);
    });

    es.addEventListener('bin-update', function(e) {
      if (typeof window.onBinUpdate === 'function') window.onBinUpdate(e);
    });

    es.addEventListener('mission-event', function(e) {
      if (typeof window.onMissionEvent === 'function') window.onMissionEvent(e);
    });

    es.addEventListener('system-status', function(e) {
      const data = JSON.parse(e.data);
      if (data.fleet !== undefined) {
        const el = document.getElementById('fleet-status');
        if (el) {
          el.className = 'health ' + (data.fleet === 'connected' ? 'health-ok' : 'health-fail');
        }
      }
      if (data.messaging !== undefined) {
        const el = document.getElementById('msg-status');
        if (el) {
          el.className = 'health ' + (data.messaging === 'connected' ? 'health-ok' : 'health-fail');
        }
      }
      if (data.redis !== undefined) {
        const el = document.getElementById('redis-status');
        if (el) {
          el.className = 'health ' + (data.redis === 'connected' ? 'health-ok' : 'health-fail');
        }
      }
    });

    es.addEventListener('robot-update', debounce(function(e) {
      var robots = JSON.parse(e.data);
      var grid = document.getElementById('robot-grid');
      if (!grid) return;

      var seen = {};
      robots.forEach(function(r) {
        seen[r.vehicle_id] = true;
        var tile = grid.querySelector('[data-name="' + r.vehicle_id + '"]');
        if (!tile) {
          // Create new tile
          tile = document.createElement('div');
          tile.className = 'robot-tile robot-' + r.state;
          tile.setAttribute('onclick', 'openRobotModal(this)');
          tile.innerHTML =
            '<div class="robot-name">' + r.vehicle_id +
            (r.charging ? '<span class="robot-charging" title="Charging">&#9889;</span>' : '') +
            '</div>' +
            '<div class="robot-battery" title="Battery: ' + r.battery + '%">' +
            '<div class="robot-battery-fill" style="width:' + r.battery + '%"></div>' +
            '</div>';
          grid.appendChild(tile);
        } else {
          // Update tile class
          tile.className = 'robot-tile robot-' + r.state;
          // Update battery bar
          var fill = tile.querySelector('.robot-battery-fill');
          if (fill) fill.style.width = r.battery + '%';
          var batDiv = tile.querySelector('.robot-battery');
          if (batDiv) batDiv.title = 'Battery: ' + r.battery + '%';
          // Update charging indicator
          var nameDiv = tile.querySelector('.robot-name');
          if (nameDiv) {
            var chgSpan = nameDiv.querySelector('.robot-charging');
            if (r.charging && !chgSpan) {
              chgSpan = document.createElement('span');
              chgSpan.className = 'robot-charging';
              chgSpan.title = 'Charging';
              chgSpan.innerHTML = '&#9889;';
              nameDiv.appendChild(chgSpan);
            } else if (!r.charging && chgSpan) {
              chgSpan.remove();
            }
          }
        }
        // Update data attributes
        tile.dataset.name = r.vehicle_id;
        tile.dataset.state = r.state;
        tile.dataset.ip = r.ip || '';
        tile.dataset.model = r.model || '';
        tile.dataset.map = r.map || '';
        tile.dataset.battery = r.battery;
        tile.dataset.charging = r.charging;
        tile.dataset.station = r.station || '';
        tile.dataset.lastStation = r.last_station || '';
        tile.dataset.available = r.available;
        tile.dataset.connected = r.connected;
        tile.dataset.blocked = r.blocked;
        tile.dataset.emergency = r.emergency;
        tile.dataset.processing = r.processing;
        tile.dataset.error = r.error;
        tile.dataset.x = r.x.toFixed(1);
        tile.dataset.y = r.y.toFixed(1);
        tile.dataset.angle = r.angle.toFixed(1);

        // Update modal if open for this robot
        if (typeof currentRobotVehicle !== 'undefined' && currentRobotVehicle === r.vehicle_id) {
          var modal = document.getElementById('robot-modal');
          if (modal && modal.classList.contains('active')) {
            openRobotModal(tile);
          }
        }
      });

      // Remove stale tiles
      var tiles = grid.querySelectorAll('.robot-tile');
      tiles.forEach(function(tile) {
        if (!seen[tile.dataset.name]) {
          tile.remove();
        }
      });

      // Update robot count
      var countEl = document.getElementById('robot-count');
      if (countEl) {
        countEl.textContent = robots.length + ' robots';
      }

      // Show/hide empty state
      var emptyCard = grid.nextElementSibling;
      if (robots.length === 0 && !grid.children.length) {
        grid.style.display = 'none';
      } else {
        grid.style.display = '';
      }

      // Reapply filter
      if (typeof filterRobots === 'function') {
        filterRobots();
      }
    }, 2000));

    es.addEventListener('cms-transaction', function(e) {
      if (typeof window.cmsAppendRows === 'function') {
        var txns = JSON.parse(e.data);
        window.cmsAppendRows(txns);
      }
    });

    es.addEventListener('debug-log', function(e) {
      if (typeof window.debugAppendRow === 'function') {
        var entry = JSON.parse(e.data);
        window.debugAppendRow(entry);
      }
    });

    es.addEventListener('fire-alarm', function(e) {
      if (typeof window.onFireAlarmUpdate === 'function') {
        var data = JSON.parse(e.data);
        window.onFireAlarmUpdate(data);
      }
    });

    es.onerror = function() {
      es.close();
      setTimeout(connect, 3000);
    };
  }

  // Close SSE connection when navigating away so the browser
  // releases the HTTP/1.1 connection slot immediately.
  window.addEventListener('beforeunload', function() {
    if (es) es.close();
  });

  connect();
})();
