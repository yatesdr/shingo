var isAuth = document.getElementById('page-data').dataset.authenticated === 'true';

function syncOrGenerate(e) {
  if (e.shiftKey) {
    if (!confirm('Delete all TEST- nodes?')) return;
    fetch('/api/nodes/delete-test', {method:'POST'})
      .then(function(r) { return r.json(); })
      .then(function(data) {
        if (data.error) alert(data.error);
        else location.reload();
      })
      .catch(function(err) { alert('Error: ' + err); });
  } else if (e.ctrlKey || e.metaKey) {
    if (!confirm('Generate test nodes for debugging?\n\nThis creates ~25 TEST- prefixed nodes.')) return;
    fetch('/api/nodes/generate-test', {method:'POST'})
      .then(function(r) { return r.json(); })
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

function openNodeModal(el) {
  if (!el || !el.dataset) return;
  var m = document.getElementById('node-modal');
  var inv = document.getElementById('modal-inventory');

  var d = el.dataset;
  document.getElementById('modal-title').textContent = d.name;

  // Parent info
  var typeInfo = document.getElementById('modal-type-info');
  var tiParent = d.parentName || '-';
  document.getElementById('ti-parent').textContent = tiParent;
  typeInfo.style.display = d.parentId ? '' : 'none';

  // Inventory
  inv.style.display = d.synthetic === 'true' ? 'none' : '';
  document.getElementById('inv-count').textContent = d.count;
  if (d.synthetic !== 'true') {
    loadInventory(d.id);
  }

  // Load extended detail (stations, payload types, children)
  var isLeafChild = !!d.parentId && d.synthetic !== 'true';
  var isSyntheticChild = !!d.parentId && d.synthetic === 'true';
  var parentTypeCode = '';
  if (d.parentId) {
    var parentTile = document.querySelector('.node-tile[data-id="' + d.parentId + '"]');
    if (parentTile) parentTypeCode = parentTile.dataset.typeCode || '';
  }
  var isDirectChildOfGroup = isLeafChild && parentTypeCode === 'NGRP';
  var isLaneSlot = isLeafChild && parentTypeCode === 'LANE';
  loadNodeDetail(d.id, d.synthetic === 'true');

  // Hide associations for lane slots (inherit from lane), show for direct children of NGRP
  var assocDiv = document.getElementById('modal-associations');
  if (assocDiv && isLaneSlot) assocDiv.style.display = 'none';

  // Show algorithm dropdowns only for NGRP nodes
  var algoDiv = document.getElementById('ngrp-algorithms');
  if (algoDiv) {
    algoDiv.style.display = d.typeCode === 'NGRP' ? '' : 'none';
    if (d.typeCode === 'NGRP') {
      document.getElementById('nf-retrieve-algo').value = 'FIFO';
      document.getElementById('nf-store-algo').value = 'LKND';
    }
  }

  if (isAuth) {
    var assocSection = document.getElementById('nf-assoc-section');
    var stationsGroup = document.getElementById('cp-stations');
    if (assocSection) assocSection.style.display = isLaneSlot ? 'none' : '';
    if (stationsGroup) stationsGroup.closest('.form-group').style.display = (isSyntheticChild || isLaneSlot) ? 'none' : '';
    var hasParent = !!d.parentId;
    toggleInheritOption('nf-bt-mode', hasParent);
    toggleInheritOption('nf-st-mode', hasParent);
    clearChipPicker('bin-types');
    clearChipPicker('stations');
    document.getElementById('nf-id').value = d.id;
    document.getElementById('nf-node-type-id').value = d.nodeTypeId || '';
    document.getElementById('nf-parent-id').value = d.parentId || '';
    document.getElementById('nf-name').value = d.name;
    document.getElementById('nf-enabled').checked = d.enabled === 'true';
  } else {
    document.getElementById('ro-enabled').textContent = d.enabled === 'true' ? 'Yes' : 'No';
  }

  m.classList.add('active');
}

function loadNodeDetail(nodeID, isSynthetic) {
  var assocDiv = document.getElementById('modal-associations');

  if (assocDiv) assocDiv.style.display = 'none';

  fetch('/api/nodes/detail?id=' + nodeID)
    .then(function(r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); })
    .then(function(data) {
      var btMode = data.bin_type_mode || '';
      var stMode = data.station_mode || '';
      var stations = data.stations || [];
      var effStations = data.effective_stations || [];
      var bts = data.bin_types || [];
      var effBts = data.effective_bin_types || [];

      if (!isAuth) {
        var stLabel = stMode === 'all' ? 'Any' : stMode === 'none' ? 'None (Core only)' : stMode === 'specific' ? (stations.length > 0 ? stations.join(', ') : 'None') : (effStations && effStations.length > 0 ? effStations.join(', ') + ' (inherited)' : 'Any');
        document.getElementById('assoc-stations').textContent = stLabel;
        var btLabel = btMode === 'all' ? 'Any' : btMode === 'specific' ? (bts.length > 0 ? bts.map(function(b) { return b.code; }).join(', ') : 'None') : (effBts && effBts.length > 0 ? effBts.map(function(b) { return b.code; }).join(', ') + ' (inherited)' : 'Any');
        document.getElementById('assoc-bt').textContent = btLabel;
        if (assocDiv) assocDiv.style.display = '';
      }

      if (isAuth) {
        var btSelect = document.getElementById('nf-bt-mode');
        btSelect.value = btMode || (data.node && data.node.parent_id ? 'inherit' : 'all');
        populateChipPicker('bin-types', bts.map(function(b) { return { id: String(b.id), label: b.code }; }));
        onModeChange('bin-types');

        var stSelect = document.getElementById('nf-st-mode');
        stSelect.value = stMode || (data.node && data.node.parent_id ? 'none' : 'all');
        populateChipPicker('stations', stations.map(function(s) { return { id: s, label: s }; }));
        onModeChange('stations');
      }

      var props = data.properties || [];
      props.forEach(function(p) {
        if (p.key === 'retrieve_algorithm') {
          var sel = document.getElementById('nf-retrieve-algo');
          if (sel) sel.value = p.value;
        } else if (p.key === 'store_algorithm') {
          var sel = document.getElementById('nf-store-algo');
          if (sel) sel.value = p.value;
        }
      });
    })
    .catch(function() {});
}

/* --- Chip Picker --- */
var _allBinTypes = JSON.parse(document.getElementById('page-data').dataset.binTypes || '[]');
var _allStations = JSON.parse(document.getElementById('page-data').dataset.edges || '[]');
var _chipSelections = { 'bin-types': [], 'stations': [] };

function getPickerConfig(name) {
  if (name === 'bin-types') return { all: _allBinTypes, inputName: 'bin_type_ids', modeId: 'nf-bt-mode' };
  return { all: _allStations, inputName: 'stations', modeId: 'nf-st-mode' };
}

function onModeChange(name) {
  var cfg = getPickerConfig(name);
  var mode = document.getElementById(cfg.modeId).value;
  var spec = document.getElementById('cp-' + name + '-specific');
  spec.style.display = mode === 'specific' ? '' : 'none';
}

function toggleInheritOption(selectId, hasParent) {
  var sel = document.getElementById(selectId);
  var opt = sel.querySelector('option[value="inherit"]');
  if (opt) opt.disabled = !hasParent;
  if (!hasParent && sel.value === 'inherit') sel.value = 'all';
}

function clearChipPicker(name) {
  _chipSelections[name] = [];
  var chips = document.getElementById('cp-' + name + '-chips');
  if (chips) chips.innerHTML = '';
  var filter = document.querySelector('#cp-' + name + ' .chip-filter');
  if (filter) filter.value = '';
}

function populateChipPicker(name, items) {
  _chipSelections[name] = items.slice();
  renderChips(name);
}

function renderChips(name) {
  var container = document.getElementById('cp-' + name + '-chips');
  container.innerHTML = '';
  _chipSelections[name].forEach(function(item) {
    var chip = document.createElement('span');
    chip.className = 'chip';
    chip.textContent = item.label;
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'chip-remove';
    btn.innerHTML = '&times;';
    btn.onclick = function() { removeChip(name, item.id); };
    chip.appendChild(btn);
    container.appendChild(chip);
  });
  renderChipDropdown(name);
}

function renderChipDropdown(name) {
  var cfg = getPickerConfig(name);
  var dd = document.getElementById('cp-' + name + '-dropdown');
  var filter = document.querySelector('#cp-' + name + ' .chip-filter');
  var q = (filter ? filter.value : '').toLowerCase();
  var selectedIds = _chipSelections[name].map(function(i) { return i.id; });
  var available = cfg.all.filter(function(item) {
    return selectedIds.indexOf(item.id) < 0 && (!q || item.label.toLowerCase().indexOf(q) >= 0);
  });
  if (available.length === 0) {
    dd.innerHTML = '<div class="chip-dropdown-empty">No items</div>';
    return;
  }
  dd.innerHTML = '';
  available.forEach(function(item) {
    var div = document.createElement('div');
    div.className = 'chip-dropdown-item';
    div.textContent = item.label;
    div.onclick = function() { addChip(name, item); };
    dd.appendChild(div);
  });
}

function addChip(name, item) {
  _chipSelections[name].push(item);
  renderChips(name);
}

function removeChip(name, id) {
  _chipSelections[name] = _chipSelections[name].filter(function(i) { return i.id !== id; });
  renderChips(name);
}

function filterChipDropdown(name) { renderChipDropdown(name); }

function showChipDropdown(name) {
  var dd = document.getElementById('cp-' + name + '-dropdown');
  dd.style.display = '';
  renderChipDropdown(name);
}

function hideChipDropdown(name) {
  setTimeout(function() {
    var dd = document.getElementById('cp-' + name + '-dropdown');
    dd.style.display = 'none';
  }, 150);
}

function serializeChipPickers() {
  document.querySelectorAll('input.chip-hidden').forEach(function(el) { el.remove(); });
  var form = document.getElementById('node-form');
  ['bin-types', 'stations'].forEach(function(name) {
    var cfg = getPickerConfig(name);
    var mode = document.getElementById(cfg.modeId).value;
    if (mode !== 'specific') return;
    _chipSelections[name].forEach(function(item) {
      var inp = document.createElement('input');
      inp.type = 'hidden'; inp.name = cfg.inputName; inp.value = item.id;
      inp.className = 'chip-hidden';
      form.appendChild(inp);
    });
  });
}

function closeNodeModal() {
  document.getElementById('node-modal').classList.remove('active');
}

function saveAlgorithmProperties() {
  var algoDiv = document.getElementById('ngrp-algorithms');
  if (!algoDiv || algoDiv.style.display === 'none') return;
  var nodeID = parseInt(document.getElementById('nf-id').value);
  if (!nodeID) return;
  var retrieveAlgo = document.getElementById('nf-retrieve-algo').value;
  var storeAlgo = document.getElementById('nf-store-algo').value;
  fetch('/api/nodes/properties/set', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({node_id: nodeID, key: 'retrieve_algorithm', value: retrieveAlgo})
  });
  fetch('/api/nodes/properties/set', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({node_id: nodeID, key: 'store_algorithm', value: storeAlgo})
  });
}

function deleteNode() {
  var id = document.getElementById('nf-id').value;
  var name = document.getElementById('nf-name').value;
  if (!confirm('Delete node "' + name + '"? This cannot be undone.')) return;
  var form = document.createElement('form');
  form.method = 'POST';
  form.action = '/nodes/delete';
  form.style.display = 'none';
  var inp = document.createElement('input');
  inp.type = 'hidden';
  inp.name = 'id';
  inp.value = id;
  form.appendChild(inp);
  document.body.appendChild(form);
  form.submit();
}

var currentNodeID = 0;
var expandedPayloadID = 0;

function loadInventory(nodeID) {
  currentNodeID = parseInt(nodeID);
  expandedPayloadID = 0;
  var manifestSec = document.getElementById('inv-manifest');
  if (manifestSec) manifestSec.style.display = 'none';
  var list = document.getElementById('inv-list');
  var countEl = document.getElementById('inv-count');
  list.innerHTML = '<span class="text-muted" style="font-size:0.8rem">Loading...</span>';
  fetch('/api/nodes/inventory?id=' + nodeID)
    .then(function(r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); })
    .then(function(items) {
      if (!items || items.length === 0) {
        countEl.textContent = '0';
        list.innerHTML = '<span class="text-muted" style="font-size:0.8rem">Empty</span>';
        return;
      }
      countEl.textContent = items.length;
      var html = '<table style="font-size:0.8rem"><thead><tr><th>Bin</th><th>Type</th><th>Status</th><th>Contents</th><th>UOP</th></tr></thead><tbody>';
      items.forEach(function(b) {
        var binBadges = '<span class="badge badge-' + escapeHtml(b.status) + '">' + escapeHtml(b.status) + '</span>';
        if (b.claimed_by) binBadges += ' <span class="badge badge-claimed">claimed</span>';
        if (b.locked) binBadges += ' <span class="badge badge-locked">locked</span>';
        var contents = b.payload_code
          ? '<strong>' + escapeHtml(b.payload_code) + '</strong>' + (b.manifest_confirmed ? ' \u2714' : '')
          : '<span class="text-muted">empty</span>';
        html += '<tr>' +
          '<td><strong>' + escapeHtml(b.label || 'Bin #' + b.id) + '</strong></td>' +
          '<td>' + escapeHtml(b.bin_type_code || '-') + '</td>' +
          '<td>' + binBadges + '</td>' +
          '<td>' + contents + '</td>' +
          '<td>' + (b.uop_remaining || 0) + '</td>' +
          '</tr>';
      });
      html += '</tbody></table>';
      list.innerHTML = html;
    })
    .catch(function() {
      list.innerHTML = '<span class="text-muted" style="font-size:0.8rem">Error loading</span>';
    });
}

var originalManifest = [];

function expandPayloadManifest(payloadID) {
  expandedPayloadID = payloadID;
  var sec = document.getElementById('inv-manifest');
  document.getElementById('inv-manifest-pid').textContent = payloadID;
  sec.style.display = '';
  var tbody = document.getElementById('inv-manifest-rows');
  tbody.innerHTML = '<tr><td colspan="3" class="text-muted">Loading...</td></tr>';
  fetch('/api/payloads/manifest?id=' + payloadID)
    .then(function(r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); })
    .then(function(items) {
      tbody.innerHTML = '';
      originalManifest = [];
      if (!items) items = [];
      if (isAuth) {
        items.forEach(function(item) {
          originalManifest.push({id: item.id, catid: item.part_number, qty: item.quantity});
          addNodeManifestRow(item.id, item.part_number, item.quantity);
        });
      } else {
        if (items.length === 0) {
          tbody.innerHTML = '<tr><td colspan="3" class="text-muted">No manifest items</td></tr>';
          return;
        }
        items.forEach(function(item) {
          tbody.innerHTML += '<tr><td>' + escapeHtml(item.part_number) + '</td><td>' + item.quantity + '</td><td></td></tr>';
        });
      }
    })
    .catch(function() { tbody.innerHTML = '<tr><td colspan="3" class="text-muted">Error</td></tr>'; });
}

function makeEditable(span) {
  var isQty = span.classList.contains('mr-qty');
  var input = document.createElement('input');
  input.type = isQty ? 'number' : 'text';
  if (isQty) { input.step = '1'; input.min = '0'; }
  input.className = 'mn-input ' + (isQty ? 'mr-qty' : 'mr-catid');
  input.value = span.dataset.value || '';
  if (!isQty) input.placeholder = 'CATID';
  span.replaceWith(input);
  input.focus();
  function commit() {
    var val = isQty ? (parseInt(input.value) || 0) : input.value.trim();
    var s = document.createElement('span');
    s.className = 'mn-val ' + (isQty ? 'mr-qty' : 'mr-catid');
    if (!val && !isQty) s.classList.add('mn-empty');
    s.dataset.value = isQty ? val : (val || '');
    s.textContent = val || (isQty ? '0' : 'CATID');
    s.onclick = function() { makeEditable(s); };
    input.replaceWith(s);
  }
  input.addEventListener('blur', commit);
  input.addEventListener('keydown', function(e) {
    if (e.key === 'Enter') { e.preventDefault(); input.blur(); }
    if (e.key === 'Escape') { input.blur(); }
  });
}

function mnSpan(cls, value, empty) {
  var s = document.createElement('span');
  s.className = 'mn-val ' + cls;
  s.dataset.value = value != null ? value : '';
  if (empty) s.classList.add('mn-empty');
  s.textContent = empty ? (cls === 'mr-qty' ? '0' : 'CATID') : value;
  s.onclick = function() { makeEditable(s); };
  return s;
}

function addNodeManifestRow(itemId, catid, qty) {
  var tbody = document.getElementById('inv-manifest-rows');
  var tr = document.createElement('tr');
  tr.dataset.itemId = itemId || 0;
  var td1 = document.createElement('td');
  var td2 = document.createElement('td');
  var isNew = !catid && (qty == null || qty === '');
  td1.appendChild(mnSpan('mr-catid', catid || '', !catid));
  td2.appendChild(mnSpan('mr-qty', qty != null && qty !== '' ? qty : 0, isNew));
  tr.appendChild(td1);
  tr.appendChild(td2);
  var td3 = document.createElement('td');
  td3.style.textAlign = 'center';
  td3.innerHTML = '<button type="button" class="btn btn-danger btn-sm" onclick="this.closest(\'tr\').remove()" style="padding:0.1rem 0.3rem;font-size:0.65rem">&times;</button>';
  tr.appendChild(td3);
  tbody.appendChild(tr);
  if (isNew) { makeEditable(td1.querySelector('.mr-catid')); }
}

function mnReadVal(el) {
  if (!el) return '';
  return el.tagName === 'INPUT' ? el.value : (el.dataset.value || '');
}

function isManifestDirty() {
  var rows = document.querySelectorAll('#inv-manifest-rows tr');
  var current = [];
  rows.forEach(function(tr) {
    var catidEl = tr.querySelector('.mr-catid');
    if (!catidEl) return;
    current.push({
      id: parseInt(tr.dataset.itemId) || 0,
      catid: mnReadVal(catidEl).trim(),
      qty: parseInt(mnReadVal(tr.querySelector('.mr-qty'))) || 0
    });
  });
  if (current.length !== originalManifest.length) return true;
  for (var i = 0; i < current.length; i++) {
    var c = current[i], o = originalManifest[i];
    if (c.id !== o.id || c.catid !== o.catid || c.qty !== o.qty) return true;
  }
  return false;
}

function collectManifestItems() {
  var rows = document.querySelectorAll('#inv-manifest-rows tr');
  var items = [];
  var valid = true;
  rows.forEach(function(tr) {
    var catidEl = tr.querySelector('.mr-catid');
    if (!catidEl) return;
    var catid = mnReadVal(catidEl).trim();
    var qty = parseInt(mnReadVal(tr.querySelector('.mr-qty'))) || 0;
    if (!catid) { valid = false; return; }
    items.push({id: parseInt(tr.dataset.itemId) || 0, cat_id: catid, quantity: qty});
  });
  return valid ? items : null;
}

function handleNodeSave(e) {
  serializeChipPickers();
  saveAlgorithmProperties();
  if (!expandedPayloadID || !isManifestDirty()) return true;
  e.preventDefault();
  var items = collectManifestItems();
  if (!items) { alert('All rows must have a CATID'); return false; }
  var reason = prompt('Reason for manifest correction:');
  if (!reason) return false;
  fetch('/api/corrections/batch', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({payload_id: expandedPayloadID, node_id: currentNodeID, reason: reason, items: items})
  })
  .then(function(r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); })
  .then(function(data) {
    if (data.error) { alert(data.error); return; }
    document.getElementById('node-form').submit();
  })
  .catch(function(err) { alert('Error saving manifest: ' + err); });
  return false;
}

function closeManifestExpand() {
  document.getElementById('inv-manifest').style.display = 'none';
  expandedPayloadID = 0;
}

document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') { closeNodeModal(); closeOccupancyModal(); closeAddNodeModal(); closeNgrpModal(); closeLaneModal(); }
});

/* --- Occupancy check --- */
function checkOccupancy() {
  document.getElementById('occupancy-modal').classList.add('active');
  document.getElementById('occupancy-modal-content').innerHTML = '<span class="text-muted">Loading...</span>';
  fetch('/api/nodes/occupancy')
    .then(function(r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); })
    .then(function(items) {
      if (!items || items.length === 0) {
        document.getElementById('occupancy-modal-content').innerHTML = '<span class="text-muted">No locations found</span>';
        return;
      }
      var html = '<table style="font-size:0.8rem"><thead><tr><th>Location</th><th>Node</th><th>Fleet Occupied</th><th>In Shingo</th><th>Status</th></tr></thead><tbody>';
      items.forEach(function(item) {
        var cls = '';
        var status = 'OK';
        if (item.discrepancy === 'fleet_only') { cls = ' style="background:#fff3cd"'; status = 'Fleet Only'; }
        else if (item.discrepancy === 'shingo_only') { cls = ' style="background:#f8d7da"'; status = 'Shingo Only'; }
        var occupied = item.fleet_occupied === null ? '-' : (item.fleet_occupied ? 'Yes' : 'No');
        html += '<tr' + cls + '><td>' + escapeHtml(item.location_id) + '</td><td>' + escapeHtml(item.node_name || '-') + '</td><td>' + occupied + '</td><td>' + (item.in_shingo ? 'Yes' : 'No') + '</td><td>' + status + '</td></tr>';
      });
      html += '</tbody></table>';
      document.getElementById('occupancy-modal-content').innerHTML = html;
    })
    .catch(function() {
      document.getElementById('occupancy-modal-content').innerHTML = '<span class="text-muted">Error loading occupancy</span>';
    });
}

function closeOccupancyModal() {
  document.getElementById('occupancy-modal').classList.remove('active');
}

/* --- Node Group modal --- */
function openNgrpModal() {
  document.getElementById('ngrp-name').value = '';
  document.getElementById('ngrp-result').innerHTML = '';
  document.getElementById('ngrp-modal').classList.add('active');
  document.getElementById('ngrp-name').focus();
}
function closeNgrpModal() { document.getElementById('ngrp-modal').classList.remove('active'); }

function createNodeGroup() {
  var name = document.getElementById('ngrp-name').value.trim();
  if (!name) { document.getElementById('ngrp-name').focus(); return; }
  var result = document.getElementById('ngrp-result');
  result.textContent = 'Creating...';
  fetch('/api/nodegroup/create', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({ name: name })
  })
  .then(function(r) { return r.json(); })
  .then(function(data) {
    if (data.error) { result.innerHTML = '<span style="color:var(--danger)">' + escapeHtml(data.error) + '</span>'; return; }
    result.innerHTML = '<span style="color:var(--success)">Created!</span>';
    setTimeout(function() { location.reload(); }, 800);
  })
  .catch(function(e) { result.innerHTML = '<span style="color:var(--danger)">Error: ' + e + '</span>'; });
}

/* --- Lane modal --- */
function openLaneModal(groupId) {
  document.getElementById('lane-group-id').value = groupId;
  document.getElementById('lane-name').value = '';
  document.getElementById('lane-result').innerHTML = '';
  document.getElementById('lane-modal').classList.add('active');
  document.getElementById('lane-name').focus();
}
function closeLaneModal() { document.getElementById('lane-modal').classList.remove('active'); }
function submitAddLane() {
  var name = document.getElementById('lane-name').value.trim();
  if (!name) { document.getElementById('lane-name').focus(); return; }
  var result = document.getElementById('lane-result');
  result.textContent = 'Creating...';
  fetch('/api/nodegroup/add-lane', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({
      group_id: parseInt(document.getElementById('lane-group-id').value),
      name: name
    })
  })
  .then(function(r) { return r.json(); })
  .then(function(data) {
    if (data.error) { result.innerHTML = '<span style="color:var(--danger)">' + escapeHtml(data.error) + '</span>'; return; }
    result.innerHTML = '<span style="color:var(--success)">Created!</span>';
    setTimeout(function() { location.reload(); }, 800);
  })
  .catch(function(e) { result.innerHTML = '<span style="color:var(--danger)">Error: ' + e + '</span>'; });
}

/* --- Add Node modal --- */
var _addNodePending = null;
function addChildNode(parentId, zone) {
  _addNodePending = { parentId: parentId, zone: zone };
  document.getElementById('add-node-title').textContent = 'Add Node';
  document.getElementById('an-name').value = '';
  document.getElementById('add-node-modal').classList.add('active');
  document.getElementById('an-name').focus();
}
function submitAddNode() {
  var p = _addNodePending;
  if (!p) return;
  var name = document.getElementById('an-name').value.trim();
  if (!name) { document.getElementById('an-name').focus(); return; }
  var form = document.createElement('form');
  form.method = 'POST'; form.action = '/nodes/create'; form.style.display = 'none';
  var fields = { name: name, zone: p.zone || '', enabled: 'on' };
  if (p.parentId) fields.parent_id = p.parentId;
  for (var k in fields) {
    var inp = document.createElement('input');
    inp.type = 'hidden'; inp.name = k; inp.value = fields[k];
    form.appendChild(inp);
  }
  document.body.appendChild(form);
  form.submit();
}
function closeAddNodeModal() {
  document.getElementById('add-node-modal').classList.remove('active');
  _addNodePending = null;
}

/* --- Node group grid in modal --- */
function loadGroupLayout(nodeID) {
  var list = document.getElementById('children-list');
  list.innerHTML = '<span class="text-muted">Loading layout...</span>';
  fetch('/api/nodegroup/layout?id=' + nodeID)
    .then(function(r) { return r.json(); })
    .then(function(data) {
      var lanes = data.lanes || [];
      var directNodes = data.direct_nodes || [];
      var stats = data.stats || {};
      var html = '<div class="sm-grid">';
      html += '<div style="font-size:0.75rem;margin-bottom:0.5rem">';
      html += '<span class="sm-cell sm-empty" style="width:14px;height:14px;display:inline-flex;vertical-align:middle"></span> Empty ';
      html += '<span class="sm-cell sm-occupied" style="width:14px;height:14px;display:inline-flex;vertical-align:middle"></span> Occupied ';
      html += '<span class="sm-cell sm-claimed" style="width:14px;height:14px;display:inline-flex;vertical-align:middle"></span> Claimed ';
      html += ' | Slots: ' + stats.total + ' | Occupied: ' + stats.occupied + ' | Claimed: ' + stats.claimed;
      html += '</div>';
      if (directNodes.length > 0) {
        html += '<div class="sm-lane">';
        html += '<span class="sm-lane-label">Direct Nodes</span>';
        directNodes.forEach(function(node) {
          var cls = 'sm-empty';
          var label = escapeHtml(node.name);
          if (node.payload) {
            cls = node.payload.claimed_by ? 'sm-claimed' : 'sm-occupied';
            label = '#' + node.payload.id;
          }
          html += '<span class="sm-cell ' + cls + '" title="' + escapeHtml(node.name) + (node.payload ? ' — ' + escapeHtml(node.payload.payload_code || '') : '') + '">' + label + '</span>';
        });
        html += '</div>';
      }
      lanes.forEach(function(lane) {
        html += '<div class="sm-lane">';
        html += '<span class="sm-lane-label">' + escapeHtml(lane.name) + '</span>';
        (lane.slots || []).forEach(function(slot) {
          var cls = 'sm-empty';
          var label = slot.depth || '';
          if (slot.payload) {
            cls = slot.payload.claimed_by ? 'sm-claimed' : 'sm-occupied';
            label = '#' + slot.payload.id;
          }
          html += '<span class="sm-cell ' + cls + '" title="' + escapeHtml(slot.name) + (slot.payload ? ' — ' + escapeHtml(slot.payload.payload_code || '') : '') + '">' + label + '</span>';
        });
        html += '</div>';
      });
      html += '</div>';
      list.innerHTML = html;
    })
    .catch(function() { list.innerHTML = '<span class="text-muted">Error loading group layout</span>'; });
}

/* --- Drag & Drop --- */
var _dragNodeID = null;

function initDragAndDrop() {
  if (!isAuth) return;
  var grid = document.getElementById('tile-grid');
  if (!grid) return;

  grid.querySelectorAll('.node-tile').forEach(function(tile) {
    if (tile.dataset.synthetic === 'true') return;
    if (tile.classList.contains('smkt-absorbed')) return;
    tile.setAttribute('draggable', 'true');
    tile.addEventListener('dragstart', onDragStart);
    tile.addEventListener('dragend', onDragEnd);
  });

  document.querySelectorAll('.smkt-lane-slots .node-tile').forEach(function(tile) {
    if (tile.dataset.synthetic === 'true') return;
    tile.setAttribute('draggable', 'true');
    tile.addEventListener('dragstart', onDragStart);
    tile.addEventListener('dragend', onDragEnd);
  });

  document.querySelectorAll('.smkt-lane-slots').forEach(function(container) {
    container.addEventListener('dragover', onDragOver);
    container.addEventListener('dragleave', onDragLeave);
    container.addEventListener('drop', onDrop);
  });

  var dropArea = document.getElementById('nodes-drop-area');
  if (dropArea) {
    dropArea.addEventListener('dragover', onDragOverArea);
    dropArea.addEventListener('dragleave', onDragLeave);
    dropArea.addEventListener('drop', onDropGrid);
  }
}

function onDragStart(e) {
  _dragNodeID = this.dataset.id;
  this.classList.add('dragging');
  e.dataTransfer.effectAllowed = 'move';
  e.dataTransfer.setData('text/plain', this.dataset.id);
}

function onDragEnd(e) {
  this.classList.remove('dragging');
  document.querySelectorAll('.drop-target').forEach(function(el) { el.classList.remove('drop-target'); });
}

function onDragOver(e) {
  e.preventDefault();
  e.dataTransfer.dropEffect = 'move';
  this.classList.add('drop-target');
}

function onDragOverArea(e) {
  if (e.target.closest('.smkt-lane-slots')) return;
  e.preventDefault();
  e.dataTransfer.dropEffect = 'move';
  document.getElementById('tile-grid').classList.add('drop-target');
}

function onDragLeave(e) {
  this.classList.remove('drop-target');
}

function onDrop(e) {
  e.preventDefault();
  e.stopPropagation();
  this.classList.remove('drop-target');
  var nodeID = parseInt(e.dataTransfer.getData('text/plain'));
  if (!nodeID) return;

  var laneSection = this.closest('.smkt-lane');
  if (!laneSection) return;
  var isDirectSection = laneSection.classList.contains('ngrp-direct');
  var parentID = parseInt(laneSection.dataset.laneId);
  if (!parentID) return;

  var tiles = this.querySelectorAll('.node-tile[draggable="true"]');
  var existingIDs = [];
  tiles.forEach(function(t) {
    var tid = parseInt(t.dataset.id);
    if (tid !== nodeID) existingIDs.push(tid);
  });

  var insertIdx = existingIDs.length;
  var nonDragIdx = 0;
  for (var i = 0; i < tiles.length; i++) {
    if (parseInt(tiles[i].dataset.id) === nodeID) continue;
    var rect = tiles[i].getBoundingClientRect();
    if (e.clientX < rect.left + rect.width / 2) {
      insertIdx = nonDragIdx;
      break;
    }
    nonDragIdx++;
  }

  var draggedTile = document.querySelector('.node-tile[data-id="' + nodeID + '"]');
  var isAlreadyInLane = draggedTile && draggedTile.dataset.parentId === String(parentID);

  var container = this;
  if (isDirectSection) {
    reparentNode(nodeID, parentID, 0, container, draggedTile, insertIdx);
  } else if (isAlreadyInLane) {
    var ordered = existingIDs.slice();
    ordered.splice(insertIdx, 0, nodeID);
    reorderLane(parentID, ordered, container, draggedTile, insertIdx);
  } else {
    var position = insertIdx + 1;
    reparentNode(nodeID, parentID, position, container, draggedTile, insertIdx);
  }
}

function onDropGrid(e) {
  if (e.target.closest('.smkt-lane-slots')) return;
  e.preventDefault();
  document.getElementById('tile-grid').classList.remove('drop-target');
  this.classList.remove('drop-target');
  var nodeID = parseInt(e.dataTransfer.getData('text/plain'));
  if (!nodeID) return;
  var tile = document.querySelector('.node-tile[data-id="' + nodeID + '"]');
  reparentNode(nodeID, null, 0, null, tile, 0);
}

function reparentNode(nodeID, parentID, position, container, tile, insertIdx) {
  fetch('/api/nodes/reparent', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({ node_id: nodeID, parent_id: parentID, position: position })
  })
  .then(function(r) { return r.json(); })
  .then(function(data) {
    if (data.error) { alert('Reparent failed: ' + data.error); return; }
    if (!tile) return;
    if (parentID && container) {
      var siblings = container.querySelectorAll('.node-tile');
      var refNode = siblings[insertIdx] || null;
      container.insertBefore(tile, refNode);
      tile.dataset.parentId = String(parentID);
      if (!tile.querySelector('.slot-depth')) {
        var badge = document.createElement('span');
        badge.className = 'slot-depth';
        tile.appendChild(badge);
      }
      if (!tile.getAttribute('draggable')) {
        tile.setAttribute('draggable', 'true');
        tile.addEventListener('dragstart', onDragStart);
        tile.addEventListener('dragend', onDragEnd);
      }
      updateLaneDepths(container);
      updateLaneCounts(container);
    } else {
      var oldContainer = tile.closest('.smkt-lane-slots');
      var grid = document.getElementById('tile-grid');
      var badge = tile.querySelector('.slot-depth');
      if (badge) badge.remove();
      tile.dataset.parentId = '';
      tile.dataset.depth = '0';
      grid.appendChild(tile);
      if (oldContainer) {
        updateLaneDepths(oldContainer);
        updateLaneCounts(oldContainer);
      }
    }
  })
  .catch(function(e) { alert('Reparent error: ' + e); });
}

function reorderLane(laneID, orderedIDs, container, draggedTile, insertIdx) {
  fetch('/api/nodegroup/reorder-lane', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({ lane_id: laneID, ordered_ids: orderedIDs })
  })
  .then(function(r) { return r.json(); })
  .then(function(data) {
    if (data.error) { alert('Reorder failed: ' + data.error); return; }
    orderedIDs.forEach(function(id) {
      var t = container.querySelector('.node-tile[data-id="' + id + '"]');
      if (t) container.appendChild(t);
    });
    updateLaneDepths(container);
  })
  .catch(function(e) { alert('Reorder error: ' + e); });
}

function updateLaneDepths(container) {
  var section = container.closest('.smkt-lane');
  var isDirectNodes = section && section.classList.contains('ngrp-direct');
  container.querySelectorAll('.node-tile').forEach(function(tile, idx) {
    var depth = isDirectNodes ? 0 : idx + 1;
    tile.dataset.depth = String(depth);
    var badge = tile.querySelector('.slot-depth');
    if (badge) badge.textContent = isDirectNodes ? '' : depth;
  });
}

function updateLaneCounts(container) {
  var section = container.closest('.smkt-lane');
  if (!section) return;
  var header = section.querySelector('.smkt-lane-header');
  if (!header) return;
  var count = container.querySelectorAll('.node-tile').length;
  var name = header.dataset.laneName || '';
  header.textContent = name + ' (' + count + ')';

  var group = section.closest('.smkt-group');
  if (group) updateGroupSummary(group);
}

function updateGroupSummary(group) {
  var lanes = group.querySelectorAll('.smkt-lane:not(.ngrp-direct)');
  var totalSlots = 0;
  lanes.forEach(function(lane) {
    totalSlots += lane.querySelectorAll('.smkt-lane-slots .node-tile').length;
  });
  var directSection = group.querySelector('.ngrp-direct');
  var directCount = directSection ? directSection.querySelectorAll('.smkt-lane-slots .node-tile').length : 0;
  var summary = lanes.length + ' lane' + (lanes.length !== 1 ? 's' : '')
    + ', ' + totalSlots + ' slot' + (totalSlots !== 1 ? 's' : '');
  if (directCount > 0) {
    summary += ', ' + directCount + ' direct';
  }
  var el = group.querySelector('.smkt-summary');
  if (el) el.textContent = summary;
}

/* --- Build supermarket hierarchy from flat tiles --- */
function buildHierarchy() {
  var grid = document.getElementById('tile-grid');
  if (!grid) return;

  var tilesByID = {};
  grid.querySelectorAll('.node-tile').forEach(function(tile) {
    tilesByID[tile.dataset.id] = tile;
  });

  var grpTiles = [];
  Object.keys(tilesByID).forEach(function(id) {
    var tile = tilesByID[id];
    var tc = tile.dataset.typeCode;
    if (tile.dataset.synthetic === 'true' && (tc === 'NGRP' || tc === 'SMKT' || tc === 'SUP')) {
      grpTiles.push(tile);
    }
  });
  if (grpTiles.length === 0) return;

  grpTiles.forEach(function(grpTile) {
    var grpId = grpTile.dataset.id;
    var grpName = grpTile.dataset.name;

    var lanes = [];
    var directChildren = [];
    var otherChildren = [];
    Object.keys(tilesByID).forEach(function(id) {
      var tile = tilesByID[id];
      if (tile.dataset.parentId !== grpId) return;
      var tc = tile.dataset.typeCode;
      if (tc === 'LANE') lanes.push(tile);
      else if (tile.dataset.synthetic !== 'true') directChildren.push(tile);
      else otherChildren.push(tile);
    });

    function findSlots(parentTile) {
      var pid = parentTile.dataset.id;
      var slots = [];
      Object.keys(tilesByID).forEach(function(id) {
        if (tilesByID[id].dataset.parentId === pid) slots.push(tilesByID[id]);
      });
      slots.sort(function(a, b) {
        return (parseInt(a.dataset.depth) || 0) - (parseInt(b.dataset.depth) || 0);
      });
      return slots;
    }

    var totalSlots = 0;
    var allLaneSlots = [];
    lanes.forEach(function(lane) {
      var slots = findSlots(lane);
      allLaneSlots.push({ lane: lane, slots: slots });
      totalSlots += slots.length;
    });

    var group = document.createElement('div');
    group.className = 'smkt-group';
    group.dataset.smktId = grpId;

    var summary = lanes.length + ' lane' + (lanes.length !== 1 ? 's' : '')
      + ', ' + totalSlots + ' slot' + (totalSlots !== 1 ? 's' : '');
    if (directChildren.length > 0) {
      summary += ', ' + directChildren.length + ' direct';
    }

    var header = document.createElement('div');
    header.className = 'smkt-header';
    header.innerHTML = '<span class="smkt-arrow">&#9660;</span>'
      + '<span class="smkt-name">' + escapeHtml(grpName) + '</span>'
      + '<span class="smkt-summary">' + summary + '</span>';

    header.addEventListener('click', function(e) {
      if (e.target.classList.contains('smkt-name')) return;
      group.classList.toggle('smkt-collapsed');
    });
    header.querySelector('.smkt-name').addEventListener('click', function(e) {
      e.stopPropagation();
      openNodeModal(grpTile);
    });
    group.appendChild(header);

    var body = document.createElement('div');
    body.className = 'smkt-body';

    if (directChildren.length > 0 || isAuth) {
      var directSection = document.createElement('div');
      directSection.className = 'smkt-lane ngrp-direct';
      directSection.dataset.laneId = grpId;

      var directHeader = document.createElement('div');
      directHeader.className = 'smkt-lane-header';
      directHeader.dataset.laneName = 'Direct Nodes';
      directHeader.textContent = 'Direct Nodes (' + directChildren.length + ')';
      directSection.appendChild(directHeader);

      var directContainer = document.createElement('div');
      directContainer.className = 'smkt-lane-slots';
      directContainer.dataset.laneId = grpId;
      directChildren.forEach(function(child) {
        directContainer.appendChild(child);
      });
      directSection.appendChild(directContainer);
      body.appendChild(directSection);
    }

    allLaneSlots.forEach(function(item) {
      var section = document.createElement('div');
      section.className = 'smkt-lane';
      section.dataset.laneId = item.lane.dataset.id;

      var laneName = item.lane.dataset.name;
      var laneHeader = document.createElement('div');
      laneHeader.className = 'smkt-lane-header';
      laneHeader.dataset.laneName = item.lane.dataset.name;
      laneHeader.textContent = laneName + ' (' + item.slots.length + ')';
      laneHeader.addEventListener('click', function() { openNodeModal(item.lane); });
      section.appendChild(laneHeader);

      var slotContainer = document.createElement('div');
      slotContainer.className = 'smkt-lane-slots';
      slotContainer.dataset.laneId = item.lane.dataset.id;
      item.slots.forEach(function(slot) {
        var depth = parseInt(slot.dataset.depth) || 0;
        var badge = document.createElement('span');
        badge.className = 'slot-depth';
        badge.textContent = depth;
        slot.appendChild(badge);
        slotContainer.appendChild(slot);
      });
      section.appendChild(slotContainer);
      body.appendChild(section);
    });

    if (isAuth) {
      var addLaneBtn = document.createElement('div');
      addLaneBtn.className = 'smkt-add-lane';
      addLaneBtn.textContent = '+ Add Lane';
      addLaneBtn.addEventListener('click', function() {
        openLaneModal(grpId);
      });
      body.appendChild(addLaneBtn);
    }

    group.appendChild(body);
    var dropArea = document.getElementById('nodes-drop-area');
    dropArea.insertBefore(group, grid);

    grpTile.classList.add('smkt-absorbed');
    lanes.forEach(function(l) { l.classList.add('smkt-absorbed'); });
    otherChildren.forEach(function(c) { c.classList.add('smkt-absorbed'); });
  });

  var remaining = grid.querySelectorAll('.node-tile:not(.smkt-absorbed)');
  if (remaining.length > 0) {
    var wrapper = document.createElement('div');
    wrapper.className = 'ungrouped-wrapper';
    var label = document.createElement('div');
    label.className = 'ungrouped-label';
    label.textContent = 'Ungrouped Nodes (' + remaining.length + ')';
    grid.parentNode.insertBefore(wrapper, grid);
    wrapper.appendChild(label);
    wrapper.appendChild(grid);
  }
}

// Refresh when bins change (loaded, cleared, moved)
window.onBinUpdate = function() {
  location.reload();
};

document.addEventListener('DOMContentLoaded', function() {
  buildHierarchy();
  initDragAndDrop();
});
