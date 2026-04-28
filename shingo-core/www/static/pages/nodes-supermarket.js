// Supermarket / node-group hierarchy: NGRP and Lane modals, layout
// preview, drag-and-drop reparenting, and the buildHierarchy pass that
// converts the flat tile grid into nested smkt-group sections.
// Requires isAuth from nodes-overview.js and openNodeModal from
// nodes-detail.js.

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
  apiPost('/api/nodegroup/create', { name: name })
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
  apiPost('/api/nodegroup/add-lane', {
    group_id: parseInt(document.getElementById('lane-group-id').value),
    name: name
  })
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
  apiGet('/api/nodegroup/layout?id=' + nodeID)
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
    .catch(function(err) {
      console.error('loadGroupLayout', err);
      list.innerHTML = '<span class="text-muted">Error loading group layout</span>';
    });
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
  apiPost('/api/nodes/reparent', { node_id: nodeID, parent_id: parentID, position: position })
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
  apiPost('/api/nodegroup/reorder-lane', { lane_id: laneID, ordered_ids: orderedIDs })
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
