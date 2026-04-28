// Page-level helpers for the nodes overview: auth flag, fleet sync,
// accordion toggle, search/filter, page init. Loaded first so isAuth is
// in scope for nodes-detail.js and nodes-supermarket.js.

var isAuth = document.getElementById('page-data').dataset.authenticated === 'true';

function syncOrGenerate(e) {
  if (e.shiftKey) {
    if (!confirm('Delete all TEST- nodes?')) return;
    apiPost('/api/nodes/delete-test')
      .then(function(data) {
        if (data.error) alert(data.error);
        else location.reload();
      })
      .catch(function(err) { alert('Error: ' + err); });
  } else if (e.ctrlKey || e.metaKey) {
    if (!confirm('Generate test nodes for debugging?\n\nThis creates ~25 TEST- prefixed nodes.')) return;
    apiPost('/api/nodes/generate-test')
      .then(function(data) {
        if (data.error) alert(data.error);
        else location.reload();
      })
      .catch(function(err) { alert('Error: ' + err); });
  } else {
    if (!confirm('Sync all nodes and scene data from fleet?')) return;
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

document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') { closeNodeModal(); closeOccupancyModal(); closeAddNodeModal(); closeNgrpModal(); closeLaneModal(); }
});

// Refresh when bins change (loaded, cleared, moved)
window.onBinUpdate = function() {
  location.reload();
};

document.addEventListener('DOMContentLoaded', function() {
  buildHierarchy();
  initDragAndDrop();
});
