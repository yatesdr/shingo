import { api, debounce, delegateActions, el, hideModal, showModal, uiConfirm } from '/static/app.js';

var currentRobotVehicle = '';

function filterRobots() {
  var q = document.getElementById('robot-search').value.toLowerCase();
  var s = document.getElementById('robot-state-filter').value;
  var tiles = document.querySelectorAll('.robot-tile');
  var shown = 0;
  tiles.forEach(function(tile) {
    var matchName = !q || tile.dataset.name.toLowerCase().indexOf(q) >= 0;
    var matchState = !s || tile.dataset.state === s;
    var vis = matchName && matchState;
    tile.style.display = vis ? '' : 'none';
    if (vis) shown++;
  });
  document.getElementById('robot-count').textContent = shown + ' robots';
}

function openRobotModal(el) {
  var d = el.dataset;
  currentRobotVehicle = d.name;
  document.getElementById('rm-title').textContent = d.name;

  var stateEl = document.getElementById('rm-state');
  stateEl.textContent = d.state;
  stateEl.className = 'badge badge-robot-' + d.state;

  document.getElementById('rm-ip').textContent = d.ip || '-';
  document.getElementById('rm-model').textContent = d.model || '-';
  document.getElementById('rm-map').textContent = d.map || '-';
  document.getElementById('rm-battery').textContent = d.battery + '%';
  document.getElementById('rm-charging').textContent = d.charging === 'true' ? 'Yes' : 'No';
  document.getElementById('rm-station').textContent = d.station || '-';
  document.getElementById('rm-last-station').textContent = d.lastStation || '-';
  document.getElementById('rm-available').textContent = d.available === 'true' ? 'Yes' : 'No';
  document.getElementById('rm-connected').textContent = d.connected === 'true' ? 'Yes' : 'No';
  document.getElementById('rm-blocked').textContent = d.blocked === 'true' ? 'Yes' : 'No';
  document.getElementById('rm-emergency').textContent = d.emergency === 'true' ? 'Yes' : 'No';
  document.getElementById('rm-processing').textContent = d.processing === 'true' ? 'Yes' : 'No';
  document.getElementById('rm-error').textContent = d.error === 'true' ? 'Yes' : 'No';
  document.getElementById('rm-position').textContent = d.x + ', ' + d.y + ' (' + d.angle + '\u00B0)';

  showModal('robot-modal');
}

function closeRobotModal() {
  hideModal('robot-modal');
}

function robotControlPost(url, body) {
  var msg = document.getElementById('rm-status-msg');
  if (msg) msg.textContent = 'Sending...';
  fetch(url, {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body)})
    .then(function(r) { return r.json().then(function(d) { return {ok:r.ok, data:d}; }); })
    .then(function(r) {
      if (msg) msg.textContent = r.ok ? 'OK' : (r.data.error || 'Error');
    })
    .catch(function(e) {
      if (msg) msg.textContent = 'Network error';
    });
}

function robotSetAvailability(available) {
  // available arrives as the literal string "true"/"false" from
  // data-action="robotSetAvailability:<bool>" colon-arg dispatch.
  // Both strings are truthy in JS, and the Go handler decodes
  // available as bool which rejects JSON string values with
  // "cannot unmarshal string into ... .available of type bool".
  // Coerce to a real boolean before posting.
  var avail = available === true || available === 'true';
  robotControlPost('/api/robots/availability', {vehicle_id: currentRobotVehicle, available: avail});
}

function robotRetryFailed() {
  robotControlPost('/api/robots/retry', {vehicle_id: currentRobotVehicle});
}

async function robotForceComplete() {
  if (!await uiConfirm('Force complete current task for ' + currentRobotVehicle + '?')) return;
  robotControlPost('/api/robots/force-complete', {vehicle_id: currentRobotVehicle});
}

document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') closeRobotModal();
});

// ─── delegated event handlers ─────────────────────────
// All page-level data-action verbs route through delegateActions
// on document.body. Multiple event types share the same handler
// map — most handlers are click-only but a few (e.g. updatePreview)
// are referenced via data-action-change / data-action-input too,
// so binding the map across every event type keeps the page wiring
// single-source.
delegateActions(document.body, {
    closeRobotModal,
    filterRobots,
    openRobotModal,
    robotControlPost,
    robotForceComplete,
    robotRetryFailed,
    robotSetAvailability
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });

// ─── live robot-grid rebuild ──────────────────────────
// app.js wires the SSE 'robot-update' event and forwards it here via
// window.onRobotUpdate. The rebuild lives in this module so it runs in the
// scope where openRobotModal / filterRobots / currentRobotVehicle exist —
// SSE-created tiles use data-action="openRobotModal" (matching the server
// template) so delegateActions handles their clicks. Debounced to 2s, same
// as the handler that previously lived inline in app.js.
window.onRobotUpdate = debounce(function(e) {
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
      tile.setAttribute('data-action', 'openRobotModal');
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
    if (currentRobotVehicle === r.vehicle_id) {
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
  if (robots.length === 0 && !grid.children.length) {
    grid.style.display = 'none';
  } else {
    grid.style.display = '';
  }

  // Reapply filter
  filterRobots();
}, 2000);
