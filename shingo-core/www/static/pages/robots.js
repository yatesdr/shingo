import { api, delegateActions, el, hideModal, showModal, uiConfirm } from '/static/app.js';

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
