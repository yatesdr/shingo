import { api, debounce, delegateActions, el, hideModal, openFromQuery, showModal, toast, uiConfirm } from '/static/app.js';
import { installLiveDurations, reconcileList, onSSE, switchTab as showTab } from '/static/shared/utils.js';
import { createRobotTile, updateRobotTile } from '/static/components/RobotTile.js';
import { createBoard } from '/static/components/localization-board.js';
import { loadEMaintReport, loadRobotHours } from '/static/components/robot-fleet-tab.js';
import { addCleaningPoint, loadCleaning, removeCleaningPoint, suggestPoints } from '/static/components/robot-cleaning-tab.js';

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

// openRobotModal is the click; fillRobotModal alone is the SSE refresh of an
// open pop-up.
function openRobotModal(el) {
  fillRobotModal(el);
}

function fillRobotModal(el) {
  var d = el.dataset;
  currentRobotVehicle = d.name;
  document.getElementById('rm-title').textContent = d.name;

  var stateEl = document.getElementById('rm-state');
  stateEl.textContent = d.state;
  stateEl.className = 'badge badge-robot-' + d.state;

  // The order the tile names ("on #664"), as a link to it. The tile's
  // data-order-id is written by the server render and by RobotTile on every
  // SSE frame, so the pop-up and the tile cannot name different orders.
  var orderEl = document.getElementById('rm-order');
  orderEl.textContent = '';
  if (d.orderId) {
    var a = document.createElement('a');
    a.href = '/orders?open=' + encodeURIComponent(d.orderId);
    a.textContent = '#' + d.orderId;
    orderEl.appendChild(a);
  } else {
    orderEl.textContent = 'none';
  }

  document.getElementById('rm-ip').textContent = d.ip || '-';
  document.getElementById('rm-model').textContent = d.model || '-';
  document.getElementById('rm-map').textContent = d.map || '-';
  document.getElementById('rm-battery').textContent = d.battery + '%';
  document.getElementById('rm-charging').textContent = d.charging === 'true' ? 'Yes' : 'No';
  document.getElementById('rm-station').textContent = d.station || '-';
  document.getElementById('rm-last-station').textContent = d.lastStation || '-';
  // Dispatchable, and when it is not, the fleet's own reasons. "No" alone left
  // the floor guessing which of seven things was wrong.
  document.getElementById('rm-available').textContent = d.available === 'true'
    ? 'Yes' : 'No' + (d.notDispatchable ? ' — ' + d.notDispatchable : '');
  document.getElementById('rm-connected').textContent = d.connected === 'true' ? 'Yes' : 'No';
  document.getElementById('rm-blocked').textContent = d.blocked === 'true' ? 'Yes' : 'No';
  document.getElementById('rm-emergency').textContent = d.emergency === 'true' ? 'Yes' : 'No';
  document.getElementById('rm-processing').textContent = d.processing === 'true' ? 'Yes' : 'No';
  document.getElementById('rm-error').textContent = d.error === 'true' ? 'Yes' : 'No';
  document.getElementById('rm-position').textContent = d.x + ', ' + d.y + ' (' + d.angle + '\u00B0)';

  showForceComplete(d.orderId || '');
  showModal('robot-modal');
}

// showForceComplete offers Force complete only when the robot is not on a
// Shingo order. Core reads a forced finish as a real one and would record the
// bin delivered; that order is ended from the order (Terminate) instead.
// The server refuses it too, for a pop-up opened before the order started.
function showForceComplete(orderID) {
  var btn = document.getElementById('rm-btn-force');
  var note = document.getElementById('rm-force-note');
  if (btn) btn.hidden = !!orderID;
  if (!note) return;
  note.hidden = !orderID;
  note.textContent = '';
  if (!orderID) return;
  note.appendChild(document.createTextNode('On order #' + orderID + '. To end it, open the order and Terminate it: '));
  var a = document.createElement('a');
  a.href = '/orders?open=' + encodeURIComponent(orderID);
  a.textContent = 'open #' + orderID;
  note.appendChild(a);
}

function closeRobotModal() {
  hideModal('robot-modal');
}

// robotControlPost sends one robot control and says how it went in a toast
// (§11), for every control in the pop-up. Resolves to whether it went through.
function robotControlPost(url, body, okText) {
  return fetch(url, {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body)})
    .then(function(r) { return r.json().then(function(d) { return {ok:r.ok, data:d}; }); })
    .then(function(r) {
      if (r.ok) toast(okText || 'Done', 'success');
      else toast((r.data && r.data.error) || 'Refused', 'error');
      return r.ok;
    })
    .catch(function() {
      toast('Network error', 'error');
      return false;
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
  robotControlPost('/api/robots/availability', {vehicle_id: currentRobotVehicle, available: avail},
    currentRobotVehicle + (avail ? ' is dispatchable' : ' is out of dispatch'));
}

// robotControl takes (true) or releases (false) the fleet manager's control
// lock. A robot someone else holds control of stays undispatchable however
// often it is made dispatchable.
function robotControl(take) {
  var t = take === true || take === 'true';
  robotControlPost('/api/robots/control', {vehicle_id: currentRobotVehicle, take: t},
    t ? 'Took control of ' + currentRobotVehicle : 'Released control of ' + currentRobotVehicle);
}

// currentOrderID is the Shingo order the open robot is on, '' when none.
function currentOrderID() {
  var tile = document.querySelector('.robot-tile[data-name="' + CSS.escape(currentRobotVehicle) + '"]');
  return (tile && tile.dataset.orderId) || '';
}

// Retry re-runs only the failed step. If the robot dropped its load before it,
// the retry finishes on an empty deck and Shingo records a delivery that did
// not happen, so on a Shingo order it asks first.
async function robotRetryFailed() {
  var order = currentOrderID();
  if (order && !await uiConfirm(currentRobotVehicle + ' is on order #' + order +
      '. Retry re-runs only the failed step. If the robot dropped its load, Shingo will record a delivery that did not happen. Retry?')) return;
  robotControlPost('/api/robots/retry', {vehicle_id: currentRobotVehicle}, 'Retry sent to ' + currentRobotVehicle);
}

// Force complete ends the robot's task at the fleet: for its own parking and
// charging orders. On a Shingo order the button is hidden (showForceComplete)
// and the server refuses.
async function robotForceComplete() {
  if (!await uiConfirm('Force complete current task for ' + currentRobotVehicle + '?')) return;
  robotControlPost('/api/robots/force-complete', {vehicle_id: currentRobotVehicle}, 'Force complete sent to ' + currentRobotVehicle);
}

// ── Air-knife cleaning, per robot (the Cleaning tab's table) ──────────────
//
// Each row has the robot's schedule switch and Clean. Clean sends the robot
// through the knife now, with no confirmation (§11: nothing is deleted or
// lost). It is disabled with the server's reason as its title, the same
// sentence the POST refuses with (engine.CleanNowRefusal), so the button never
// offers what the server refuses. The switch is the robot's opt-out, saved the
// moment it is flipped; it decides what the interval sends, not what Clean may.
function robotClean(btn) {
  var vehicle = btn.dataset.vehicle;
  btn.disabled = true;
  robotControlPost('/api/robots/cleaning/clean-now', {vehicle_id: vehicle}, vehicle + ' sent to the air knife')
    .then(refreshCleaningTab);
}

function robotCleanOptIn(sw) {
  var vehicle = sw.dataset.vehicle;
  var on = !sw.classList.contains('on');
  sw.classList.toggle('on', on);
  sw.setAttribute('aria-checked', on ? 'true' : 'false');
  robotControlPost('/api/robots/cleaning/opt-out', {vehicle_id: vehicle, out: !on},
    vehicle + (on ? ' is on the cleaning schedule' : ' is off the cleaning schedule'))
    .then(refreshCleaningTab);
}

// ── Tabs ──────────────────────────────────────────────────────────────────
//
// The shared switchTab (§22) shows the panel; this loads a panel the first
// time it is shown, and again on every later visit since both are snapshots.
var cleaningShown = false;

function switchTab(key, el) {
  showTab(key, el);
  if (key === 'fleet') { loadEMaintReport(); loadRobotHours(); }
  if (key === 'cleaning') { cleaningShown = true; loadCleaning(); }
}

function refreshCleaningTab() {
  if (cleaningShown) loadCleaning();
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
    addCleaningPoint,
    closeRobotModal,
    filterRobots,
    loadEMaintReport,
    openRobotModal,
    removeCleaningPoint,
    robotClean,
    robotCleanOptIn,
    robotControl,
    robotForceComplete,
    robotRetryFailed,
    robotSetAvailability,
    suggestPoints,
    switchTab
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });

// ─── live robot-grid rebuild ──────────────────────────
// Subscribed on the shared onSSE bus (shared/utils.js) for 'robot-update'
// (Q-002 — replaces app.js's retired window.onRobotUpdate dispatch). The
// rebuild lives in this module so it runs in the
// scope where openRobotModal / filterRobots / currentRobotVehicle exist —
// SSE-created tiles use data-action="openRobotModal" (matching the server
// template) so delegateActions handles their clicks. Debounced to 2s, same
// as the handler that previously lived inline in app.js.
// Migrated onto the shared reconcileList primitive + RobotTile component
// (plan §3.C / §6). reconcileList adopts the server-rendered tiles by
// data-name (nodeKey) so the first SSE frame updates them in place instead of
// rebuilding the grid. createRobotTile/updateRobotTile preserve the exact
// markup + dataset the openRobotModal handler depends on.
onSSE('robot-update', debounce(function(robots) {
  // `robots` is the parsed SSE payload (the shared bus does JSON.parse).
  var grid = document.getElementById('robot-grid');
  if (!grid) return;

  reconcileList(grid, robots, {
    key: function(r) { return r.vehicle_id; },
    create: function(r) { return createRobotTile(r); },
    update: function(node, r) {
      updateRobotTile(node, r);
      // Keep an open detail modal in sync with live updates.
      if (currentRobotVehicle === r.vehicle_id) {
        var modal = document.getElementById('robot-modal');
        if (modal && modal.classList.contains('active')) fillRobotModal(node);
      }
    },
    nodeKey: function(node) { return node.dataset.name; },
  });

  var countEl = document.getElementById('robot-count');
  if (countEl) countEl.textContent = robots.length + ' robots';
  grid.style.display = robots.length === 0 ? 'none' : '';

  // The fault clock on any tile the reconcile just touched.
  installLiveDurations(grid);
  filterRobots();
}, 2000));

// Server-rendered tiles carry the same spans on first paint.
installLiveDurations(document.getElementById('robot-grid') || document);


// ── The localization board ────────────────────────────────────────────────
//
// Booted after the tiles so a slow map fetch never delays the fleet list, which
// is what someone opening this page in a hurry came for. The board does not
// draw robots (R18): its robot control is the vehicle filter only.
var board = null;

function bootLocalizationBoard() {
  var root = document.getElementById('localization-board');
  if (!root) return;
  board = createBoard(root, {});
  board.load().catch(function (err) {
    root.innerHTML = '<p class="lb-empty">Could not load the localization board: ' +
      (err && err.message ? err.message : err) + '</p>';
  });
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', bootLocalizationBoard);
} else {
  bootLocalizationBoard();
}

// /robots?open=AMR-07 opens that robot's pop-up — the order pop-up links the
// robot it names here (app.js openFromQuery).
openFromQuery(function(name) {
  var tile = document.querySelector('.robot-tile[data-name="' + CSS.escape(name) + '"]');
  if (tile) openRobotModal(tile);
});
