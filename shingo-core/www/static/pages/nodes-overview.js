import { api, apiPost, delegateActions, toast, uiConfirm } from '/static/app.js';
// SIDE-EFFECT IMPORT, and the page's declaration that it needs the detail modal.
// Nothing here calls into that module — what it needs is the module's delegated
// handlers registered, because the node tiles this file filters carry
// data-action="openNodeModal" and nodes-detail.js is what answers it.
//
// It is an IMPORT rather than a second <script> tag because the tag would load
// the module a second time: {{cacheBust}} is a fresh timestamp per call and the
// import below (and nodes-supermarket.js's) carries no query, so the two URLs
// differ and the browser instantiates the module twice. Declaring the need here
// also keeps it off nodes-supermarket.js, which imports the same module for its
// own reasons — if that import ever went away, the tiles must not stop working.
import '/static/pages/nodes-detail.js';

// Page-level helpers for the nodes overview: fleet sync, accordion
// toggle, search/filter. Sibling modules (nodes-detail.js,
// nodes-supermarket.js) read isAuth from #page-data independently
// under ES module scoping.

// ── THE PAGE COMES BACK WHERE YOU LEFT IT ───────────────────────────────────
//
// Saving a node POSTs to /nodes/update, which answers 303 and navigates the
// whole page; the six location.reload() calls across this module and
// nodes-supermarket.js do the same thing by another route. Every one of them
// lands the operator at the TOP, and the node they were working on is a scroll
// down a long tile map. On a floor edit that is the difference between changing
// four nodes and changing one and giving up.
//
// ONE beforeunload HOOK, NOT SEVEN CALL SITES. Every navigation off this page
// goes through it — form.submit(), location.reload(), a plain link — so the
// fix cannot be forgotten by the next thing that reloads. The alternative was
// re-rendering the tiles in place on save, which no code here does: all six
// existing sites reload, and inventing an in-place path for one of them would
// leave the tile map showing pre-save state everywhere else.
//
// KEYED BY PATH AND FENCED BY TIME. Only a return to the SAME page restores,
// and only within 30s, so this is the save-and-come-back round trip and not a
// position remembered from a visit an hour ago. Browsers restore scroll on
// back/forward themselves; a POST-redirect-GET is the case they do not cover.
//
// try/catch because sessionStorage throws outright in some privacy modes, and a
// scroll convenience must never be the thing that stops the page loading.
var SCROLL_KEY = 'nodes:scroll:' + location.pathname;

window.addEventListener('beforeunload', function() {
  try {
    sessionStorage.setItem(SCROLL_KEY, JSON.stringify({ y: window.scrollY, at: Date.now() }));
  } catch (e) { /* storage unavailable: the page simply lands at the top */ }
});

(function restoreScroll() {
  var saved;
  try {
    saved = JSON.parse(sessionStorage.getItem(SCROLL_KEY) || 'null');
    sessionStorage.removeItem(SCROLL_KEY);
  } catch (e) { return; }
  if (!saved || !saved.y || Date.now() - saved.at > 30000) return;
  // After paint, or the document is still short and the scroll goes nowhere.
  window.requestAnimationFrame(function() {
    window.requestAnimationFrame(function() { window.scrollTo(0, saved.y); });
  });
})();

// Sync + the test-data tools used to hide behind Ctrl/Shift-click on one button
// (undiscoverable, and hazardous right next to a production action). They are now
// three explicit actions: Sync from Fleet, and the two test-node tools tucked
// inside the "Dev tools" disclosure in the toolbar.
async function syncFromFleet() {
  if (!await uiConfirm('Sync all nodes and scene data from fleet?')) return;
  var form = document.createElement('form');
  form.method = 'POST';
  form.action = '/nodes/sync-fleet';
  document.body.appendChild(form);
  form.submit();
}
async function generateTestNodes() {
  if (!await uiConfirm('Generate test nodes for debugging?\n\nThis creates ~25 TEST- prefixed nodes.')) return;
  apiPost('/api/nodes/generate-test')
    .then(function(data) {
      if (data.error) toast(data.error, 'error');
      else location.reload();
    })
    .catch(function(err) { toast('Error: ' + err, 'error'); });
}
async function deleteTestNodes() {
  if (!await uiConfirm('Delete all TEST- nodes?')) return;
  apiPost('/api/nodes/delete-test')
    .then(function(data) {
      if (data.error) toast(data.error, 'error');
      else location.reload();
    })
    .catch(function(err) { toast('Error: ' + err, 'error'); });
}

function toggleAccordion(id) {
  document.getElementById(id).classList.toggle('open');
}

function filterNodes() {
  var q = document.getElementById('node-search').value.toLowerCase();
  var z = document.getElementById('node-zone-filter').value;
  var tiles = document.querySelectorAll('.node-tile');
  var shown = 0;
  tiles.forEach(function(tile) {
    if (tile.classList.contains('smkt-absorbed') || tile.classList.contains('smkt-add-tile')) return;
    var matchName = !q || (tile.dataset.name || '').toLowerCase().indexOf(q) >= 0 || (tile.dataset.label && tile.dataset.label.toLowerCase().indexOf(q) >= 0);
    var matchZone = !z || (tile.dataset.zone || '') === z;
    var vis = matchName && matchZone;
    tile.style.display = vis ? '' : 'none';
    if (vis) shown++;
  });
  // Update supermarket group visibility based on visible slots
  document.querySelectorAll('.smkt-group').forEach(function(group) {
    var laneSections = group.querySelectorAll('.smkt-lane, .smkt-shuffle');
    var groupHasVisible = false;
    laneSections.forEach(function(section) {
      var slots = section.querySelectorAll('.node-tile:not(.smkt-add-tile)');
      var sectionVisible = false;
      slots.forEach(function(slot) {
        if (slot.style.display !== 'none') sectionVisible = true;
      });
      section.style.display = sectionVisible ? '' : 'none';
      if (sectionVisible) groupHasVisible = true;
    });
    group.style.display = groupHasVisible ? '' : 'none';
  });
  document.getElementById('node-count').textContent = shown + ' nodes';
}

// Escape-closes the per-page modals; the close* handlers each live in
// their owning module (nodes-detail.js, nodes-supermarket.js) and
// register their own Escape handlers there. This file no longer
// references them across module boundaries.

// ─── delegated event handlers ─────────────────────────
// All page-level data-action verbs route through delegateActions
// on document.body. Multiple event types share the same handler
// map — most handlers are click-only but a few (e.g. updatePreview)
// are referenced via data-action-change / data-action-input too,
// so binding the map across every event type keeps the page wiring
// single-source.
delegateActions(document.body, {
    filterNodes,
    syncFromFleet,
    generateTestNodes,
    deleteTestNodes,
    toggleAccordion
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
