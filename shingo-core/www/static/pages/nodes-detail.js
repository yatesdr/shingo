import { api, apiGet, apiPost, delegateActions, el, escapeHtml, removeClosestRow, toast, uiConfirm, uiPrompt } from '/static/app.js';
import { confirmAllowedBinsNarrowing, renderMaintainSection, saveMaintainedGroup } from '/static/pages/nodes-maintain.js';

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
      // "Enable ASRS" defaults ON (controls shown); loadNodeDetail flips it
      // off below if the group has asrs_enabled=off persisted.
      var asrsBox = document.getElementById('nf-asrs-enabled');
      if (asrsBox) asrsBox.checked = true;
      var asrsControls = document.getElementById('nf-asrs-controls');
      if (asrsControls) asrsControls.classList.remove('hide');
    }
  }

  // Maintained group — the same group-type test the algorithms use, and only
  // for an operator who can edit: the section lives in the auth-only half of
  // the modal and has no read-only twin.
  renderMaintainSection(isAuth && isGroupType, d.id);

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

      // The waiting-points section is built from the group's LANE children.
      renderLaneGate(
        data.node && ['NGRP', 'SMKT', 'SUP'].indexOf(data.node.node_type_code) >= 0,
        data.node ? data.node.id : 0);

      var props = data.properties || [];
      props.forEach(function(p) {
        if (p.key === 'retrieve_algorithm') {
          var sel = document.getElementById('nf-retrieve-algo');
          if (sel) sel.value = p.value;
        } else if (p.key === 'store_algorithm') {
          var sel = document.getElementById('nf-store-algo');
          if (sel) sel.value = p.value;
        } else if (p.key === 'asrs_enabled') {
          var abox = document.getElementById('nf-asrs-enabled');
          if (abox) abox.checked = (p.value !== 'off');
          var actrls = document.getElementById('nf-asrs-controls');
          if (actrls) actrls.classList.toggle('hide', p.value === 'off');
        } else if (p.key === 'resolve_around') {
          var rabox = document.getElementById('nf-resolve-around');
          if (rabox) rabox.checked = (p.value === 'on');
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
  var filter = document.querySelector('#cp-' + name + ' .tag-filter');
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
    chip.className = 'tag';
    chip.textContent = item.label;
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'tag-remove';
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
  var filter = document.querySelector('#cp-' + name + ' .tag-filter');
  var q = (filter ? filter.value : '').toLowerCase();
  var selectedIds = _chipSelections[name].map(function(i) { return i.id; });
  var available = cfg.all.filter(function(item) {
    return selectedIds.indexOf(item.id) < 0 && (!q || item.label.toLowerCase().indexOf(q) >= 0);
  });
  if (available.length === 0) {
    dd.innerHTML = '<div class="tag-dropdown-empty">No items</div>';
    return;
  }
  dd.innerHTML = '';
  available.forEach(function(item) {
    var div = document.createElement('div');
    div.className = 'tag-dropdown-item';
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
  document.querySelectorAll('input.tag-hidden').forEach(function(el) { el.remove(); });
  var form = document.getElementById('node-form');
  ['bin-types', 'stations'].forEach(function(name) {
    var cfg = getPickerConfig(name);
    var mode = document.getElementById(cfg.modeId).value;
    if (mode !== 'specific') return;
    _chipSelections[name].forEach(function(item) {
      var inp = document.createElement('input');
      inp.type = 'hidden'; inp.name = cfg.inputName; inp.value = item.id;
      inp.className = 'tag-hidden';
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
  // Resolve-around: per-group lane-preference arm (default off).
  var raBox = document.getElementById('nf-resolve-around');
  apiPost('/api/nodes/properties/set', {node_id: nodeID, key: 'resolve_around', value: (raBox && raBox.checked) ? 'on' : 'off'})
    .catch(function(err) { console.error('saveAlgorithmProperties resolve_around', err); });
  var retrieveAlgo = document.getElementById('nf-retrieve-algo').value;
  var storeAlgo = document.getElementById('nf-store-algo').value;
  apiPost('/api/nodes/properties/set', {node_id: nodeID, key: 'retrieve_algorithm', value: retrieveAlgo})
    .catch(function(err) { console.error('saveAlgorithmProperties retrieve', err); });
  apiPost('/api/nodes/properties/set', {node_id: nodeID, key: 'store_algorithm', value: storeAlgo})
    .catch(function(err) { console.error('saveAlgorithmProperties store', err); });
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

// handleNodeSave runs everything the modal saves OUTSIDE the form post, then
// posts the form.
//
// preventDefault IS THE FIRST STATEMENT, AND HAS TO BE. delegateActions ignores
// what a handler returns and only calls preventDefault itself for
// data-prevent-default, so a preventDefault reached after the first `await`
// arrives once the browser has already begun navigating — the awaited work is
// abandoned mid-flight and, worse, any question it wanted to ask is asked of a
// page that is on its way out. That was already true of the waiting-point save
// (whose confirm could not be seen) and it is load-bearing now: the
// maintained-group save can be REFUSED, and a refusal nobody sees is a refusal
// that did not happen.
//
// form.submit() at the end does not fire a submit event, so this cannot re-enter.
async function handleNodeSave(el, evt) {
  if (evt) evt.preventDefault();
  var form = document.getElementById('node-form');

  serializeChipPickers();
  saveAlgorithmProperties();

  // Clear any force from a PREVIOUS save. The modal's form is one long-lived
  // element, so a hidden input added when the operator confirmed a narrowing
  // would otherwise still be there the next time they pressed Save — on a
  // different node, with the guard silently overridden and nobody asked.
  form.querySelectorAll('input[name="force"]').forEach(function(i) { i.remove(); });

  // Asked before anything is written, because it is a question about what the
  // FORM is carrying — the narrower Allowed Bins set the post is about to apply.
  var btMode = document.getElementById('nf-bt-mode');
  if (btMode && btMode.value === 'specific') {
    var ok = await confirmAllowedBinsNarrowing(
      document.getElementById('nf-id').value,
      _chipSelections['bin-types'].map(function(i) { return i.id; }),
      form);
    if (!ok) return;
  }

  // This one can ask the human a question (a mark the map does not know).
  await saveLaneGatePoints();
  await saveMaintainedGroup();

  if (expandedPayloadID && isManifestDirty()) {
    var items = collectManifestItems();
    if (!items) { toast('All rows must have a CATID', 'info'); return; }
    var reason = await uiPrompt('Reason for manifest correction:');
    if (!reason) return;
    try {
      var data = await apiPost('/api/corrections/batch',
        {payload_id: expandedPayloadID, node_id: currentNodeID, reason: reason, items: items});
      if (data && data.error) { toast(data.error, 'error'); return; }
    } catch (err) {
      toast('Error saving manifest: ' + err, 'error');
      return;
    }
  }
  form.submit();
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
    clearGatePoint,
    clearGroupWaitPoints,
    onGatePointSearch,
    pickGatePoint,
    saveAlgorithmProperties,
    serializeChipPickers,
    showChipDropdown,
    toggleInheritOption
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });

document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') { closeNodeModal(); }
});

// ── Waiting points ─────────────────────────────────────────────────────────
//
// ON THE GROUP, and now the PROPERTY is the group's too. One field lists the
// spots a robot with work anywhere in this block may stand at; the per-lane rows
// below it are the legacy override, kept working while plants migrate and
// scheduled for deletion.
//
// The place a human goes to configure a supermarket was always the group — you
// want the aisles side by side, so which are gated is one glance rather than five
// modals. What changed is that the answer is now one field instead of five,
// because a robot at a waiting spot owns nothing and never needed a spot of its
// lane's own.
//
// There is no enable switch. A block with spots stages robots at them; a block
// without any holds its orders before dispatch. A switch and a spot could
// disagree, and only one of them can be right.

var _gateGroupID = 0;          // the group whose section is open, 0 when none
var _gateMarks = [];           // last search result, for round-trip validation
var _gateMarksLoaded = false;  // false until a search has answered at least once
var _gateSearchTimer = null;
var _gateRowSeq = 0;

// renderLaneGate builds the section for a GROUP from its child lanes. Hidden for
// anything that is not a group, and for a group with no lanes — there is nothing
// to say about waiting points on a node with no aisles.
function renderLaneGate(isGroup, groupID) {
  var box = document.getElementById('lane-gate');
  var list = document.getElementById('lane-gate-list');
  if (!box || !list) return;

  list.innerHTML = '';
  _gateMarks = [];
  _gateMarksLoaded = false;
  _gateGroupID = 0;
  if (!isGroup) { box.classList.add('hide'); return; }

  // ONE call for the whole section — the group's spots and its lanes' overrides
  // together. Two calls would let the overrides render before the thing they
  // override.
  apiGet('/api/nodes/lane-gate-points?group_id=' + groupID)
    .then(function(data) {
      var lanes = (data && data.lanes) || [];
      box.classList.toggle('hide', lanes.length === 0);
      if (lanes.length === 0) return;
      _gateGroupID = groupID;
      list.appendChild(groupWaitRow(groupID, (data && data.group_wait_points) || ''));
      lanes.forEach(function(lane) { list.appendChild(gateRow(lane)); });
    })
    .catch(function(err) {
      console.warn('lane gate: list lanes', err);
      box.classList.add('hide');
    });
}

// groupWaitRow is the PRIMARY control: the block's waiting spots, comma
// separated. It carries data-group-id rather than data-lane-id so the save walk
// can tell the two rows apart and post each against the right node.
//
// A plain text field rather than the lane rows' mark picker. The picker searches
// one name at a time and this is a list; a multi-select widget for a value an
// engineer types once at commissioning is machinery nobody asked for. The save
// still round-trips against the known marks, one entry at a time.
function groupWaitRow(groupID, value) {
  var row = document.createElement('div');
  row.className = 'form-group';
  row.style.marginBottom = '14px';
  row.dataset.groupId = groupID;
  row.dataset.saved = value || '';
  row.innerHTML =
    '<label class="text-sm" style="font-weight:600">Waiting spots for this block</label>' +
    '<input type="text" class="text-sm group-wait-input" style="width:100%"' +
    ' value="' + escapeHtml(value || '') + '"' +
    ' placeholder="Comma-separated map points, e.g. AISLE-A-WAIT, AISLE-B-WAIT" autocomplete="off">' +
    '<div class="flex flex-between" style="margin-top:4px;align-items:center">' +
      '<span class="text-muted group-wait-state" style="font-size:0.75rem"></span>' +
      '<button type="button" class="btn btn-sm" data-action="clearGroupWaitPoints">Clear</button>' +
    '</div>';
  renderGroupWaitState(row, value || '');
  return row;
}

// renderGroupWaitState says what the value MEANS. The count matters on its own:
// a robot that arrives when every spot is taken queues on the floor, and that is
// the fleet's business rather than Core's — but a person choosing the number
// should be told that is the trade they are making.
function renderGroupWaitState(row, value) {
  var s = row.querySelector('.group-wait-state');
  if (!s) return;
  var pts = String(value || '').split(',').map(function(p) { return p.trim(); })
              .filter(function(p) { return p.length > 0; });
  s.textContent = pts.length
    ? 'Gated — ' + pts.length + ' spot' + (pts.length === 1 ? '' : 's') +
      ' for every lane in this block. Orders ship unsealed and their slot is chosen on arrival.'
    : 'Not gated — every lane here decides its slot before dispatch and holds the order if the answer is no.';
}

// clearGroupWaitPoints stands down the WHOLE block, so the confirmation counts
// the whole block. A per-lane number here would quote an interruption far
// smaller than the one about to happen.
async function clearGroupWaitPoints(el) {
  var row = el.closest('[data-group-id]');
  if (!row) return;
  var input = row.querySelector('.group-wait-input');
  if (!input.value.trim()) { renderGroupWaitState(row, ''); return; }

  var waiting = 0;
  try {
    var data = await apiGet('/api/nodes/lane-waiting?group_id=' + row.dataset.groupId);
    waiting = (data && data.waiting) || 0;
  } catch (e) {
    console.warn('group wait: waiting count', e);
  }
  if (waiting > 0) {
    var ok = await uiConfirm(waiting + ' robot' + (waiting === 1 ? ' is' : 's are') +
      ' waiting in this block. ' + (waiting === 1 ? 'It' : 'They') +
      ' will complete under the old rules; new orders for EVERY lane here will wait before ' +
      'dispatch instead. Clear the spots?');
    if (!ok) return;
  }
  input.value = '';
  renderGroupWaitState(row, '');
}

function gateRow(lane) {
  var id = 'gp-' + (++_gateRowSeq);
  var row = document.createElement('div');
  row.className = 'form-group';
  row.style.marginBottom = '10px';
  row.dataset.laneId = lane.lane_id;
  row.dataset.laneName = lane.name;
  row.dataset.saved = lane.point || '';
  row.innerHTML =
    '<label class="text-sm" style="font-weight:600">' + escapeHtml(lane.name) +
      ' <span class="text-muted" style="font-weight:400">— legacy override</span></label>' +
    '<div class="tag-field" style="position:relative">' +
      '<input type="text" class="text-sm gate-point-input" id="' + id + '" style="width:100%"' +
      ' value="' + escapeHtml(lane.point || '') + '"' +
      ' placeholder="Search location marks, or type a name…" autocomplete="off"' +
      ' data-action-input="onGatePointSearch" data-action-focus="onGatePointSearch">' +
      '<div class="tag-dropdown hide gate-point-dropdown"></div>' +
    '</div>' +
    '<div class="flex flex-between" style="margin-top:4px;align-items:center">' +
      '<span class="text-muted gate-point-state" style="font-size:0.75rem"></span>' +
      '<button type="button" class="btn btn-sm" data-action="clearGatePoint">Clear</button>' +
    '</div>';
  renderGateState(row, lane.point || '');
  return row;
}

// renderGateState says what the value MEANS, not what it is. The field already
// shows the name; what a human needs is which of the two behaviours they have
// just chosen for that aisle.
function renderGateState(row, value) {
  var s = row.querySelector('.gate-point-state');
  if (!s) return;
  s.textContent = value
    ? 'Gated — robots wait at ' + value + '.'
    : 'Not gated — orders wait before a robot is sent.';
}

function gateRowOf(el) { return el && el.closest ? el.closest('[data-lane-id]') : null; }

export function onGatePointSearch(el) {
  var row = gateRowOf(el);
  if (!row) return;
  var input = row.querySelector('.gate-point-input');
  renderGateState(row, input.value.trim());
  clearTimeout(_gateSearchTimer);
  // Debounced: a plant scene carries a lot of marks and every keystroke would
  // otherwise be a query.
  _gateSearchTimer = setTimeout(function() { fetchGateMarks(row, input.value.trim()); }, 150);
}

function fetchGateMarks(row, q) {
  apiGet('/api/map/marks?q=' + encodeURIComponent(q))
    .then(function(data) {
      _gateMarks = (data && data.marks) || [];
      _gateMarksLoaded = true;
      renderGateDropdown(row, data && data.truncated, (data && data.matched) || 0);
    })
    .catch(function(err) {
      // A scene that cannot be read must not block configuration: the field still
      // accepts a typed name, which is the sim and emergency path.
      console.warn('lane gate: mark search', err);
      _gateMarks = [];
      _gateMarksLoaded = false;
      renderGateDropdown(row, false, 0);
    });
}

function renderGateDropdown(row, truncated, matched) {
  var dd = row.querySelector('.gate-point-dropdown');
  if (!dd) return;
  if (!_gateMarks.length) {
    dd.innerHTML = '<div class="text-muted" style="padding:6px 8px;font-size:0.75rem">' +
      (_gateMarksLoaded
        ? 'No matching marks in the current map — type a name to use it anyway.'
        : 'Map not available — type a mark name.') +
      '</div>';
    dd.classList.remove('hide');
    return;
  }
  var html = _gateMarks.map(function(m) {
    var sub = [m.label, m.class, m.area].filter(Boolean).join(' · ');
    return '<div class="tag-option" data-action="pickGatePoint" data-mark="' + escapeHtml(m.name) + '">' +
      '<strong>' + escapeHtml(m.name) + '</strong>' +
      (sub ? '<span class="text-muted" style="margin-left:8px;font-size:0.75rem">' + escapeHtml(sub) + '</span>' : '') +
      '</div>';
  }).join('');
  if (truncated) {
    html += '<div class="text-muted" style="padding:6px 8px;font-size:0.75rem">' +
      matched + ' marks match — keep typing to narrow.</div>';
  }
  dd.innerHTML = html;
  dd.classList.remove('hide');
}

export function pickGatePoint(el) {
  var row = gateRowOf(el);
  if (!row || !el.dataset.mark) return;
  row.querySelector('.gate-point-input').value = el.dataset.mark;
  row.querySelector('.gate-point-dropdown').classList.add('hide');
  renderGateState(row, el.dataset.mark);
}

// clearGatePoint takes a lane out of staging, and asks first when robots are
// standing at the point it is about to remove.
export async function clearGatePoint(el) {
  var row = gateRowOf(el);
  if (!row) return;
  var input = row.querySelector('.gate-point-input');
  if (!input.value.trim()) { renderGateState(row, ''); return; }

  var waiting = 0;
  try {
    var data = await apiGet('/api/nodes/lane-waiting?lane_id=' + row.dataset.laneId);
    waiting = (data && data.waiting) || 0;
  } catch (e) {
    console.warn('lane gate: waiting count', e);
  }
  if (waiting > 0) {
    var ok = await uiConfirm(waiting + ' robot' + (waiting === 1 ? ' is' : 's are') +
      ' waiting at ' + row.dataset.laneName + '. ' + (waiting === 1 ? 'It' : 'They') +
      ' will complete under the old rules; new orders will wait before dispatch instead. Clear it?');
    if (!ok) return;
  }
  input.value = '';
  renderGateState(row, '');
}

// saveLaneGatePoints writes every row whose value CHANGED, and is the last place a
// typo is cheap to catch. The value goes to the fleet verbatim as a block
// location: a point that is not on the map kills the waybill at issue time, on the
// floor, with a robot already committed. A confirm here costs a click.
async function saveLaneGatePoints() {
  var box = document.getElementById('lane-gate');
  if (!box || box.classList.contains('hide')) return;

  // THE GROUP ROW FIRST, and against the GROUP's node id. The per-lane rows post
  // against laneId; posting the block's spots against a lane id would write the
  // list onto one lane as a legacy override — which reads as saved, gates that
  // one aisle, and leaves the rest of the block ungated with nothing to say so.
  var groupRow = box.querySelector('[data-group-id]');
  if (groupRow) {
    var groupInput = groupRow.querySelector('.group-wait-input');
    var groupValue = groupInput.value.trim();
    if (groupValue !== (groupRow.dataset.saved || '')) {
      var unknown = [];
      if (_gateMarksLoaded && _gateMarks.length) {
        groupValue.split(',').forEach(function(p) {
          var v = p.trim();
          if (v && !_gateMarks.some(function(m) { return m.name === v; })) unknown.push(v);
        });
      }
      var proceed = true;
      if (unknown.length) {
        proceed = await uiConfirm(unknown.join(', ') + (unknown.length === 1 ? ' is' : ' are') +
          ' not a location mark on the current map. A point the fleet cannot find kills the ' +
          'waybill when the robot is already committed. Save anyway?');
      }
      if (proceed) {
        try {
          await apiPost('/api/nodes/properties/set',
            {node_id: parseInt(groupRow.dataset.groupId, 10), key: 'group_wait_points', value: groupValue});
          groupRow.dataset.saved = groupValue;
        } catch (err) {
          toast('Could not save the waiting spots for this block: ' + err, 'error');
        }
      }
    }
  }

  var rows = box.querySelectorAll('[data-lane-id]');
  for (var i = 0; i < rows.length; i++) {
    var row = rows[i];
    var value = row.querySelector('.gate-point-input').value.trim();
    if (value === (row.dataset.saved || '')) continue; // untouched: no write, no audit row

    if (value && _gateMarksLoaded && _gateMarks.length) {
      var known = _gateMarks.some(function(m) { return m.name === value; });
      if (!known) {
        var ok = await uiConfirm('"' + value + '" is not in the current map. Robots sent to a point ' +
          'the fleet does not know will fail at dispatch. Save it for ' + row.dataset.laneName + ' anyway?');
        if (!ok) continue;
      }
    }
    try {
      await apiPost('/api/nodes/properties/set',
        {node_id: parseInt(row.dataset.laneId, 10), key: 'lane_gate_point', value: value});
      row.dataset.saved = value;
    } catch (err) {
      toast('Could not save the waiting point for ' + row.dataset.laneName + ': ' + err, 'error');
    }
  }
}
