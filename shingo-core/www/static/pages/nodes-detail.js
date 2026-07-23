import { api, apiGet, apiPost, delegateActions, el, escapeHtml, removeClosestRow, toast, uiConfirm, uiPrompt } from '/static/app.js';

// Node detail modal: form fields, chip pickers (bin types & stations),
// inventory list with editable manifest, occupancy comparison modal.
//
// Each per-page module reads isAuth from #page-data directly — the
// previous "Requires isAuth from nodes-overview.js" comment described
// pre-module behavior that no longer works under ES module scoping.

var isAuth = document.getElementById('page-data').dataset.authenticated === 'true';

export function openNodeModal(el) {
  if (!el || !el.dataset) return;
  var m = document.getElementById('node-modal');
  var inv = document.getElementById('modal-inventory');

  var d = el.dataset;
  document.getElementById('modal-title').textContent = d.name;

  // Parent info
  var typeInfo = document.getElementById('modal-type-info');
  var tiParent = d.parentName || '-';
  document.getElementById('ti-parent').textContent = tiParent;
  typeInfo.classList.toggle('hide', !d.parentId);

  // Inventory
  inv.classList.toggle('hide', d.synthetic === 'true');
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
  if (assocDiv && isLaneSlot) assocDiv.classList.add('hide');

  // Show algorithm dropdowns only for NGRP nodes. Toggle the `hide`
  // class — `style="display:none"` was swapped for `class="hide"` in
  // the UI-consistency refactor, but the inline style.display toggle
  // here wasn't updated, so .hide kept the wrapper invisible for every
  // node type.
  // Group type codes: NGRP is current; SMKT / SUP are legacy codes still on
  // un-migrated DBs (matched the same way in nodes-supermarket.js). The
  // algorithm / ASRS controls apply to any of them — keying only on the exact
  // 'NGRP' string left the whole section hidden on legacy-coded groups.
  var isGroupType = d.typeCode === 'NGRP' || d.typeCode === 'SMKT' || d.typeCode === 'SUP';
  var algoDiv = document.getElementById('ngrp-algorithms');
  if (algoDiv) {
    algoDiv.classList.toggle('hide', !isGroupType);
    if (isGroupType) {
      document.getElementById('nf-retrieve-algo').value = 'FIFO';
      document.getElementById('nf-store-algo').value = 'LKND';
      // Reset reshuffle controls to defaults; loadNodeDetail
      // overrides from persisted properties below.
      _reshuffleTargets = [];
      renderReshuffleTargetChips();
      var restoreSel = document.getElementById('nf-reshuffle-restore');
      if (restoreSel) restoreSel.value = 'off';
      // "Enable ASRS" defaults ON (controls shown); loadNodeDetail flips it
      // off below if the group has asrs_enabled=off persisted.
      var asrsBox = document.getElementById('nf-asrs-enabled');
      if (asrsBox) asrsBox.checked = true;
      var asrsControls = document.getElementById('nf-asrs-controls');
      if (asrsControls) asrsControls.classList.remove('hide');
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

  if (assocDiv) assocDiv.classList.add('hide');

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
        if (assocDiv) assocDiv.classList.remove('hide');
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
        } else if (p.key === 'reshuffle_target_nodes') {
          try {
            var arr = JSON.parse(p.value);
            if (Array.isArray(arr)) {
              _reshuffleTargets = arr.slice();
            }
          } catch (e) { console.warn('reshuffle_target_nodes parse', e); }
        } else if (p.key === 'reshuffle_restore_blockers') {
          var rsel = document.getElementById('nf-reshuffle-restore');
          if (rsel) rsel.value = (p.value === 'on') ? 'on' : 'off';
        } else if (p.key === 'asrs_enabled') {
          var abox = document.getElementById('nf-asrs-enabled');
          if (abox) abox.checked = (p.value !== 'off');
          var actrls = document.getElementById('nf-asrs-controls');
          if (actrls) actrls.classList.toggle('hide', p.value === 'off');
        }
      });

      // Populate the +Add dropdown with direct children of this NGRP
      // (excluding lanes and synthetic children, plus any already
      // selected). Children come from data.children.
      var children = (data.children || []).filter(function(c) {
        return !c.is_synthetic && (c.node_type_code !== 'LANE');
      });
      _reshuffleTargetCandidates = children.map(function(c) { return c.name; });
      renderReshuffleTargetChips();
    })
    .catch(function(err) { console.error('loadNodeDetail', err); });
}

// ── Reshuffle-target chip picker (NGRP only) ────────────────────────────
var _reshuffleTargets = [];          // selected names, in order
var _reshuffleTargetCandidates = []; // all direct-child names

function renderReshuffleTargetChips() {
  var container = document.getElementById('nf-reshuffle-targets-chips');
  if (!container) return;
  container.innerHTML = '';
  _reshuffleTargets.forEach(function(name) {
    var chip = document.createElement('span');
    chip.className = 'chip';
    chip.textContent = name;
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'chip-remove';
    btn.innerHTML = '&times;';
    btn.onclick = function() {
      _reshuffleTargets = _reshuffleTargets.filter(function(n) { return n !== name; });
      renderReshuffleTargetChips();
    };
    chip.appendChild(btn);
    container.appendChild(chip);
  });
  // Re-populate the add-dropdown with names not already selected.
  var dd = document.getElementById('nf-reshuffle-targets-add');
  if (!dd) return;
  dd.innerHTML = '<option value="">+ Add target node…</option>';
  _reshuffleTargetCandidates.forEach(function(name) {
    if (_reshuffleTargets.indexOf(name) !== -1) return;
    var opt = document.createElement('option');
    opt.value = name;
    opt.textContent = name;
    dd.appendChild(opt);
  });
}

function addReshuffleTarget() {
  var dd = document.getElementById('nf-reshuffle-targets-add');
  if (!dd || !dd.value) return;
  if (_reshuffleTargets.indexOf(dd.value) === -1) {
    _reshuffleTargets.push(dd.value);
    renderReshuffleTargetChips();
  }
  dd.value = '';
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
  spec.classList.toggle('hide', mode !== 'specific');
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
  dd.classList.remove('hide');
  renderChipDropdown(name);
}

function hideChipDropdown(name) {
  setTimeout(function() {
    var dd = document.getElementById('cp-' + name + '-dropdown');
    dd.classList.add('hide');
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

// onAsrsToggle shows/hides the algorithm decision controls when the operator
// flips the "Enable ASRS" checkbox. The actual persistence happens on save.
function onAsrsToggle() {
  var box = document.getElementById('nf-asrs-enabled');
  var ctrls = document.getElementById('nf-asrs-controls');
  if (box && ctrls) ctrls.classList.toggle('hide', !box.checked);
}

function saveAlgorithmProperties() {
  var algoDiv = document.getElementById('ngrp-algorithms');
  if (!algoDiv || algoDiv.classList.contains('hide')) return;
  var nodeID = parseInt(document.getElementById('nf-id').value);
  if (!nodeID) return;
  // Enable-ASRS flag: 'off' makes the resolver use default algorithms.
  var asrsBox = document.getElementById('nf-asrs-enabled');
  apiPost('/api/nodes/properties/set', {node_id: nodeID, key: 'asrs_enabled', value: (asrsBox && asrsBox.checked) ? 'on' : 'off'})
    .catch(function(err) { console.error('saveAlgorithmProperties asrs_enabled', err); });
  var retrieveAlgo = document.getElementById('nf-retrieve-algo').value;
  var storeAlgo = document.getElementById('nf-store-algo').value;
  apiPost('/api/nodes/properties/set', {node_id: nodeID, key: 'retrieve_algorithm', value: retrieveAlgo})
    .catch(function(err) { console.error('saveAlgorithmProperties retrieve', err); });
  apiPost('/api/nodes/properties/set', {node_id: nodeID, key: 'store_algorithm', value: storeAlgo})
    .catch(function(err) { console.error('saveAlgorithmProperties store', err); });
  // Complex-order buried-reshuffle properties.
  var targets = JSON.stringify(_reshuffleTargets || []);
  apiPost('/api/nodes/properties/set', {node_id: nodeID, key: 'reshuffle_target_nodes', value: targets})
    .catch(function(err) { console.error('saveAlgorithmProperties reshuffle_target_nodes', err); });
  var restoreVal = (document.getElementById('nf-reshuffle-restore') || {}).value || 'off';
  apiPost('/api/nodes/properties/set', {node_id: nodeID, key: 'reshuffle_restore_blockers', value: restoreVal})
    .catch(function(err) { console.error('saveAlgorithmProperties reshuffle_restore_blockers', err); });
}

async function deleteNode() {
  var id = document.getElementById('nf-id').value;
  var name = document.getElementById('nf-name').value;
  if (!await uiConfirm('Delete node "' + name + '"? This cannot be undone.')) return;
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
  if (manifestSec) manifestSec.classList.add('hide');
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
      var html = '<table style="font-size:0.8rem"><thead><tr><th>Bin</th><th>Type</th><th>Status</th><th>Contents</th><th>UoP</th></tr></thead><tbody>';
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
  sec.classList.remove('hide');
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
  td3.innerHTML = '<button type="button" class="btn btn-danger btn-sm" data-action="removeClosestRow" style="padding:0.1rem 0.3rem;font-size:0.65rem">&times;</button>';
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

async function handleNodeSave(el, evt) {
  serializeChipPickers();
  saveAlgorithmProperties();
  if (!expandedPayloadID || !isManifestDirty()) return true;
  if (evt) evt.preventDefault();
  var items = collectManifestItems();
  if (!items) { toast('All rows must have a CATID', 'info'); return false; }
  var reason = await uiPrompt('Reason for manifest correction:');
  if (!reason) return false;
  apiPost('/api/corrections/batch', {payload_id: expandedPayloadID, node_id: currentNodeID, reason: reason, items: items})
    .then(function(data) {
      if (data.error) { toast(data.error, 'error'); return; }
      document.getElementById('node-form').submit();
    })
    .catch(function(err) { toast('Error saving manifest: ' + err, 'error'); });
  return false;
}

function closeManifestExpand() {
  document.getElementById('inv-manifest').classList.add('hide');
  expandedPayloadID = 0;
}

// The "Check Occupancy" block was removed here: it depended on RDS/SEER bin
// tracking (GET /binDetails) that was never provisioned in production, so it
// errored on live plants (and the UI swallowed the real error). Its toolbar
// button had already been hidden. If RDS bin tracking ever ships, restore it
// from git history.

// ─── delegated event handlers ─────────────────────────
// All page-level data-action verbs route through delegateActions
// on document.body. Multiple event types share the same handler
// map — most handlers are click-only but a few (e.g. updatePreview)
// are referenced via data-action-change / data-action-input too,
// so binding the map across every event type keeps the page wiring
// single-source.
delegateActions(document.body, {
    addChip,
    addNodeManifestRow,
    addReshuffleTarget,
    clearChipPicker,
    closeManifestExpand,
    closeNodeModal,
    collectManifestItems,
    deleteNode,
    expandPayloadManifest,
    filterChipDropdown,
    getPickerConfig,
    handleNodeSave,
    hideChipDropdown,
    isManifestDirty,
    loadInventory,
    loadNodeDetail,
    makeEditable,
    mnReadVal,
    mnSpan,
    onAsrsToggle,
    onModeChange,
    openNodeModal,
    populateChipPicker,
    removeChip,
    renderChipDropdown,
    renderChips,
    saveAlgorithmProperties,
    serializeChipPickers,
    showChipDropdown,
    toggleInheritOption
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });

document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') { closeNodeModal(); }
});
