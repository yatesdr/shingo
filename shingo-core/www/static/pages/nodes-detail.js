// Node detail modal: form fields, chip pickers (bin types & stations),
// inventory list with editable manifest, occupancy comparison modal.
// Requires isAuth from nodes-overview.js.

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

  apiGet('/api/nodes/detail?id=' + nodeID)
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
    .catch(function(err) { console.error('loadNodeDetail', err); });
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
  apiPost('/api/nodes/properties/set', {node_id: nodeID, key: 'retrieve_algorithm', value: retrieveAlgo})
    .catch(function(err) { console.error('saveAlgorithmProperties retrieve', err); });
  apiPost('/api/nodes/properties/set', {node_id: nodeID, key: 'store_algorithm', value: storeAlgo})
    .catch(function(err) { console.error('saveAlgorithmProperties store', err); });
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
  apiGet('/api/nodes/inventory?id=' + nodeID)
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
    .catch(function(err) {
      console.error('loadInventory', err);
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
  apiGet('/api/payloads/manifest?id=' + payloadID)
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
    .catch(function(err) {
      console.error('expandPayloadManifest', err);
      tbody.innerHTML = '<tr><td colspan="3" class="text-muted">Error</td></tr>';
    });
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
  apiPost('/api/corrections/batch', {payload_id: expandedPayloadID, node_id: currentNodeID, reason: reason, items: items})
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

/* --- Occupancy check --- */
function checkOccupancy() {
  document.getElementById('occupancy-modal').classList.add('active');
  document.getElementById('occupancy-modal-content').innerHTML = '<span class="text-muted">Loading...</span>';
  apiGet('/api/nodes/occupancy')
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
    .catch(function(err) {
      console.error('checkOccupancy', err);
      document.getElementById('occupancy-modal-content').innerHTML = '<span class="text-muted">Error loading occupancy</span>';
    });
}

function closeOccupancyModal() {
  document.getElementById('occupancy-modal').classList.remove('active');
}
