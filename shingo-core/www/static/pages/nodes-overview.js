import { api, apiPost, delegateActions, toast, uiConfirm } from '/static/app.js';
import { autoReloadOnSSE } from '/static/shared/utils.js';

// Page-level helpers for the nodes overview: fleet sync, accordion
// toggle, search/filter. Sibling modules (nodes-detail.js,
// nodes-supermarket.js) read isAuth from #page-data independently
// under ES module scoping.

// Node tiles (and supermarket lanes) render their occupancy server-side,
// so reflect live moves without a manual refresh. node-update covers
// node config/enable changes; bin-update covers occupancy. Longer
// debounce because bin moves are frequent plant-wide — a burst collapses
// into one reload. Skipped while a node modal is open or the operator is
// typing in the search/filter (see autoReloadOnSSE).
autoReloadOnSSE(['node-update', 'bin-update'], { debounceMs: 2000 });

async function syncOrGenerate(e) {
  if (e.shiftKey) {
    if (!await uiConfirm('Delete all TEST- nodes?')) return;
    apiPost('/api/nodes/delete-test')
      .then(function(data) {
        if (data.error) toast(data.error, 'error');
        else location.reload();
      })
      .catch(function(err) { toast('Error: ' + err, 'error'); });
  } else if (e.ctrlKey || e.metaKey) {
    if (!await uiConfirm('Generate test nodes for debugging?\n\nThis creates ~25 TEST- prefixed nodes.')) return;
    apiPost('/api/nodes/generate-test')
      .then(function(data) {
        if (data.error) toast(data.error, 'error');
        else location.reload();
      })
      .catch(function(err) { toast('Error: ' + err, 'error'); });
  } else {
    if (!await uiConfirm('Sync all nodes and scene data from fleet?')) return;
    var form = document.createElement('form');
    form.method = 'POST';
    form.action = '/nodes/sync-fleet';
    document.body.appendChild(form);
    form.submit();
  }
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
    syncOrGenerate,
    toggleAccordion
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
