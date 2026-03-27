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

// Generic POST/PUT/DELETE with JSON response
function apiPost(url, body) {
  return fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {})
  }).then(function(r) {
    if (!r.ok) return r.text().then(function(t) { try { throw JSON.parse(t); } catch(e) { if (typeof e === 'object' && e.error) throw e.error; throw t; } });
    return r.json();
  });
}

function apiPut(url, body) {
  return fetch(url, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {})
  }).then(function(r) {
    if (!r.ok) return r.text().then(function(t) { try { throw JSON.parse(t); } catch(e) { if (typeof e === 'object' && e.error) throw e.error; throw t; } });
    return r.json();
  });
}

function apiDelete(url) {
  return fetch(url, { method: 'DELETE' }).then(function(r) {
    if (!r.ok) return r.text().then(function(t) { try { throw JSON.parse(t); } catch(e) { if (typeof e === 'object' && e.error) throw e.error; throw t; } });
    return r.json();
  });
}

// Time formatting
function timeAgo(ts) {
  if (!ts) return '-';
  var d = Date.now() - new Date(ts).getTime();
  if (d < 60000) return 'just now';
  if (d < 3600000) return Math.floor(d / 60000) + 'm ago';
  if (d < 86400000) return Math.floor(d / 3600000) + 'h ago';
  return Math.floor(d / 86400000) + 'd ago';
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

// Theme toggle (3-state: light -> dark -> system)
function getStoredTheme() {
  return localStorage.getItem('theme');
}
function getEffectiveTheme() {
  var stored = getStoredTheme();
  if (stored === 'light' || stored === 'dark') return stored;
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}
function applyTheme() {
  var effective = getEffectiveTheme();
  document.documentElement.dataset.theme = effective;
  var btn = document.querySelector('.theme-toggle');
  if (!btn) return;
  var stored = getStoredTheme();
  if (stored === 'dark') {
    btn.textContent = '\u263D'; // moon
  } else if (stored === 'light') {
    btn.textContent = '\u2600'; // sun
  } else {
    btn.textContent = '\u25D0'; // half-circle (system)
  }
}
function toggleTheme() {
  var stored = getStoredTheme();
  if (stored === 'light') {
    localStorage.setItem('theme', 'dark');
  } else if (stored === 'dark') {
    localStorage.removeItem('theme');
  } else {
    localStorage.setItem('theme', 'light');
  }
  applyTheme();
}
document.addEventListener('DOMContentLoaded', function() {
  applyTheme();
  window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', function() {
    if (!getStoredTheme()) applyTheme();
  });
});

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

    es.onerror = function() {
      es.close();
      setTimeout(connect, 3000);
    };
  }

  connect();
})();
