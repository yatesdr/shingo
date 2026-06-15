import { apiGet, apiPost, delegateActions, escapeHtml, toast } from '/static/app.js';

// Core-owned bin loaders, rendered as drag-and-drop containers on the Nodes grid
// — the same mental model as node groups/lanes (nodes-supermarket.js). The modal
// only CREATES a loader (name/role/layout/inbound/outbound); membership is edited
// on the grid: drag a node tile into a dedicated loader to add a position, ⠿-drag
// to reorder (persisted via sort_order), × to remove. shared_window loaders hold a
// payload set instead of nodes (chips). Per-payload UOP thresholds + the lead-time
// Calc live on the Inventory page; this surface is structure only.
//
// Coexistence with the supermarket drag code: a loader box's drop handler calls
// stopPropagation so the drop never falls through to #nodes-drop-area's onDropGrid
// (which would reparent/ungroup the node in topology). Member ⠿-grips set ONLY a
// custom drag type (not text/plain), so if a member is dragged out onto the grid,
// supermarket's onDropGrid reads an empty text/plain and no-ops instead of
// reparenting. Membership is an overlay (bin_loader_homes), never a topology move,
// so loader boxes render their own representational tiles and leave the canonical
// grid tile in place.

let nodesByName = {};
let nodesById = {};
let childrenByParent = {}; // parent node id -> [child node ids], to list a group's slots
let payloadCodes = [];
let loaderData = []; // raw /api/loader/list: [{loader, payloads, homes}]
let draggingMemberNode = null;

const pageData = document.getElementById('page-data');
const isAuth = !!pageData && pageData.dataset.authenticated === 'true';

/* ── Create-Loader modal (structure only) ─────────────── */

function val(id) { const e = document.getElementById(id); return e ? (e.value || '').trim() : ''; }
function result(msg, isErr) {
  const e = document.getElementById('loader-result');
  if (!e) return;
  e.textContent = msg || '';
  e.style.color = isErr ? 'var(--danger)' : 'var(--success)';
}

function setVal(id, v) { const e = document.getElementById(id); if (e) e.value = v; }
function setText(id, t) { const e = document.getElementById(id); if (e) e.textContent = t; }
function setDisabled(id, d) { const e = document.getElementById(id); if (e) e.disabled = d; }

// The inbound/outbound/buffer "Material flow" fields are meaningful only for
// shared_window — a dedicated_positions loader's spots are their own in/out, so
// hide that whole section for dedicated (echoes the box header).
function setLayoutFlowVisibility() {
  const sel = document.getElementById('loader-layout');
  const sec = document.getElementById('loader-flow-section');
  if (sec) sec.style.display = (sel && sel.value === 'dedicated_positions') ? 'none' : '';
}

function openLoaderModal() {
  setVal('loader-edit-id', '');
  ['loader-name', 'loader-inbound', 'loader-outbound', 'loader-buffer'].forEach(function (id) { setVal(id, ''); });
  setDisabled('loader-role', false); setDisabled('loader-layout', false);
  setText('loader-modal-title', 'Create Loader');
  setText('loader-submit-btn', 'Create Loader');
  setLayoutFlowVisibility();
  const m = document.getElementById('loader-modal');
  if (m) m.classList.add('active');
  result('');
  fillDatalists();
}

// editLoader opens the modal pre-filled for an existing loader. role + anchor are
// the identity (and layout would orphan members) so they're locked — change those
// by delete + recreate; name/flow endpoints are editable.
function editLoader(lid) {
  const item = loaderData.find(function (it) { return String(it.loader.id) === String(lid); });
  if (!item) return;
  const l = item.loader;
  setVal('loader-edit-id', l.id);
  setVal('loader-name', l.name || '');
  setVal('loader-role', l.role || 'produce');
  setVal('loader-layout', l.layout || 'shared_window');
  setVal('loader-inbound', l.inbound_source || '');
  setVal('loader-outbound', l.outbound_dest || '');
  setVal('loader-buffer', l.buffer_dest || '');
  setDisabled('loader-role', true); setDisabled('loader-layout', true);
  setText('loader-modal-title', 'Edit Loader');
  setText('loader-submit-btn', 'Save');
  setLayoutFlowVisibility();
  const m = document.getElementById('loader-modal');
  if (m) m.classList.add('active');
  result('');
  fillDatalists();
}

function closeLoaderModal() {
  const m = document.getElementById('loader-modal');
  if (m) m.classList.remove('active');
}

// submitLoader handles both create and edit (driven by the hidden loader-edit-id).
function submitLoader() {
  const editId = val('loader-edit-id');
  const name = val('loader-name');
  if (!name) { result('Name is required', true); return; }
  const flow = {
    inbound_source: val('loader-inbound'), outbound_dest: val('loader-outbound'), buffer_dest: val('loader-buffer'),
  };
  if (editId) {
    result('Saving…');
    apiPost('/api/loader/update', Object.assign({ id: Number(editId), name: name, layout: val('loader-layout') }, flow)).then(function (d) {
      if (d && d.error) { result(d.error, true); return; }
      result('Saved', false);
      refresh();
      setTimeout(closeLoaderModal, 400);
    }).catch(function (e) { result('' + e, true); });
    return;
  }
  result('Creating…');
  // 1c: name the loader's node groups INLINE — if the output/buffer name isn't an
  // existing node group, create it first, then the loader references it by name. So you
  // set up the loader and its two groups in one flow instead of pre-making the groups.
  const newGroups = [flow.outbound_dest, flow.buffer_dest].filter(function (n) { return n && !(n in nodesByName); });
  Promise.all(newGroups.map(function (n) { return apiPost('/api/node-group/create', { name: n }); }))
    .then(function () {
      return apiPost('/api/loader/create', Object.assign({ name: name, role: val('loader-role'), layout: val('loader-layout') }, flow));
    })
    .then(function (d) {
      if (d && d.error) { result(d.error, true); return; }
      result('Created — drag node tiles into it on the grid', false);
      ['loader-name', 'loader-inbound', 'loader-outbound', 'loader-buffer'].forEach(function (id) { setVal(id, ''); });
      refresh();
    }).catch(function (e) { result('' + e, true); });
}

function fillDatalists() {
  setDatalist('loader-nodes-dl', Object.keys(nodesByName).map(function (n) {
    return '<option value="' + escapeHtml(n) + '">';
  }).join(''));
  setDatalist('loader-payloads-dl', payloadCodes.map(function (c) {
    return '<option value="' + escapeHtml(c) + '">';
  }).join(''));
}
function setDatalist(id, html) { const el = document.getElementById(id); if (el) el.innerHTML = html; }

/* ── Data load + grid render ──────────────────────────── */

async function refresh() {
  try {
    const results = await Promise.all([apiGet('/api/nodes'), apiGet('/api/payloads'), apiGet('/api/loader/list')]);
    const nd = results[0], pd = results[1], ld = results[2];
    const nodes = (nd && (nd.nodes || nd.data || nd)) || [];
    nodesByName = {}; nodesById = {}; childrenByParent = {};
    (Array.isArray(nodes) ? nodes : []).forEach(function (n) {
      const id = n.id != null ? n.id : n.ID, name = n.name != null ? n.name : n.Name;
      const pid = n.parent_id != null ? n.parent_id : (n.ParentID != null ? n.ParentID : null);
      if (name != null) { nodesByName[name] = id; nodesById[id] = name; }
      if (pid != null) { (childrenByParent[pid] = childrenByParent[pid] || []).push(id); }
    });
    const ps = (pd && (pd.payloads || pd.data || pd)) || [];
    payloadCodes = (Array.isArray(ps) ? ps : []).map(function (p) {
      return p.code || p.Code || p.payload_code || p.PayloadCode || p;
    }).filter(Boolean);
    loaderData = (ld && ld.loaders) || [];
  } catch (e) { /* keep last render */ }
  fillDatalists();
  renderGrid();
}

function renderGrid() {
  const area = document.getElementById('nodes-drop-area');
  if (!area) return; // page has no nodes
  let host = document.getElementById('loader-boxes');
  if (!host) {
    host = document.createElement('div');
    host.id = 'loader-boxes';
    area.insertBefore(host, area.firstChild);
  }
  if (!loaderData.length) {
    host.innerHTML = isAuth
      ? '<div class="loader-empty">No loaders yet. Use <strong>Create Loader</strong>, then drag node tiles into the loader box to assign positions.</div>'
      : '';
    markLinkedTiles();
    return;
  }
  host.innerHTML = loaderData.map(boxHtml).join('');
  wireAll(host);
  markLinkedTiles();
}

// markLinkedTiles mirrors each loader member's CANONICAL grid tile state onto the
// loader-box slot tile, so a slot shows the same live colour (loaded / empty / staged
// / claimed …) as the same node out on the grid — the slots look like the group nodes.
// The grid (group) tile itself is left untouched: a slot is differentiated by its teal
// outline, not by ringing the group node.
function markLinkedTiles() {
  const STATE = ['tile-has-payload', 'tile-empty-bin', 'tile-staged', 'tile-maintenance', 'tile-claimed', 'tile-disabled', 'tile-synthetic'];
  const memberIds = {};
  loaderData.forEach(function (it) {
    (it.homes || []).forEach(function (h) { memberIds[String(h.position_node_id)] = true; });
  });
  Object.keys(memberIds).forEach(function (id) {
    // Scope to the grid so the lookup never matches the slot tile (which now also
    // carries .node-tile[data-id]); copy the canonical tile's state classes onto every
    // slot tile for that node.
    const grid = document.querySelector('#tile-grid .node-tile[data-id="' + id + '"]');
    document.querySelectorAll('.loader-member[data-id="' + id + '"]').forEach(function (m) {
      STATE.forEach(function (c) { m.classList.remove(c); });
      if (grid) STATE.forEach(function (c) { if (grid.classList.contains(c)) m.classList.add(c); });
    });
  });
}

// groupSlots returns the LEAF descendant node ids of a node group (its slots), walking
// NGRP -> LANE -> slot so both lane-nested seeded slots and nodes dropped directly into
// the group show up. Empty group -> [].
function groupSlots(groupName) {
  const gid = nodesByName[groupName];
  if (gid == null) return [];
  const out = [];
  (function walk(id) {
    const kids = childrenByParent[id];
    if (!kids || !kids.length) { if (id !== gid) out.push(id); return; }
    kids.forEach(walk);
  })(gid);
  return out;
}

// groupZoneHtml renders ONE associated node group (output / buffer) as a labelled
// drop-zone inside the loader box: its current slots as draggable tiles (drag a tile OUT
// to the grid to remove it from the group) and the zone itself a drop-target (drag a node
// tile IN to add it). data-group carries the group name for the drop handler.
function groupZoneHtml(label, groupName) {
  const slots = groupSlots(groupName);
  const tiles = slots.length
    ? slots.map(function (id) {
        return '<div class="node-tile loader-group-slot" data-id="' + id + '"' + (isAuth ? ' draggable="true"' : '') + '>'
          + '<span class="tile-loc">' + escapeHtml(nodesById[id] || ('node#' + id)) + '</span></div>';
      }).join('')
    : '<span class="loader-members-empty">' + (isAuth ? 'drag node tiles in' : 'empty') + '</span>';
  return '<div class="loader-box-group-zone" data-group="' + escapeHtml(groupName) + '">'
    + '<div class="loader-group-zone-head"><span class="loader-box-group-label">' + label + '</span>'
    + '<span class="loader-box-group-name">' + escapeHtml(groupName) + '</span></div>'
    + '<div class="loader-group-zone-body">' + tiles + '</div></div>';
}

// loaderGroupsHtml renders the loader's associated node groups — its output supermarket
// (shared_window only; dedicated positions are their own outbound) and its buffer — each
// as a drag-in/out zone INSIDE the teal box, placed after the positions + payload set + note.
function loaderGroupsHtml(l) {
  const dedicated = l.layout === 'dedicated_positions';
  let html = '';
  if (!dedicated && l.outbound_dest) html += groupZoneHtml('Output', l.outbound_dest);
  if (l.buffer_dest) html += groupZoneHtml('Buffer', l.buffer_dest);
  return html;
}

function boxHtml(item) {
  const l = item.loader;
  const dedicated = l.layout === 'dedicated_positions';
  // Dedicated positions are their own inbound+outbound, so the inbound→outbound
  // flow is meaningless (false info) for them — only shared_window shows it.
  let meta = escapeHtml(l.role) + ' · ' + escapeHtml(l.layout);
  if (!dedicated) {
    let flow = (l.inbound_source || '—') + ' → ' + (l.outbound_dest || '—');
    if (l.buffer_dest) flow += ' · buf ' + l.buffer_dest;
    meta += ' · ' + escapeHtml(flow);
  }
  // Member nodes are shown ONLY for dedicated-home loaders (each position is a
  // meaningful payload-pinned slot). Shared-window loaders + unloaders are defined
  // by the node GROUPS they pull from / feed — showing their individual windows is
  // noise (and confusing to other team members), so they render group zones only.
  const nodes = dedicated ? nodeMembersHtml(item, dedicated) : '';
  const payloadSet = dedicated ? '' : payloadChipsHtml(item);
  const groupsHtml = loaderGroupsHtml(l);
  const hint = isAuth
    ? (dedicated
      ? '<div class="loader-hint">Drag node tiles here · ⠿ reorder · × remove · pick a payload per spot (shows as a badge). UOP threshold lives on the Inventory page.</div>'
      : '<div class="loader-hint">Shared-window loader — it references the node groups below (the source it pulls from and the supermarket it feeds). Set its shared payloads below; drag nodes into a group zone to compose it.</div>')
    : '';
  return '<div class="loader-box" data-loader-id="' + l.id + '" data-layout="' + escapeHtml(l.layout) + '">'
    + '<div class="loader-box-header">'
    + '<span class="loader-box-name">' + escapeHtml(l.name || '(unnamed)') + '</span>'
    + '<span class="loader-box-meta">' + meta + '</span>'
    + (isAuth ? '<button class="loader-box-edit" title="Edit loader">Edit</button>' : '')
    + (isAuth ? '<button class="loader-box-del" title="Delete loader">Delete</button>' : '')
    + '</div>'
    + '<div class="loader-box-body">' + (dedicated ? '<div class="loader-members">' + nodes + '</div>' : '') + payloadSet + hint + groupsHtml + '</div>'
    + '</div>';
}

// nodeMembersHtml renders a loader's node members (bin_loader_homes) for BOTH
// layouts: shared_window = windows (name only), dedicated_positions = positions
// each carrying a payload shown as an editable badge.
function nodeMembersHtml(item, dedicated) {
  const homes = item.homes || [];
  if (!homes.length) {
    return '<span class="loader-members-empty">no ' + (dedicated ? 'positions' : 'windows') + ' yet — drag node tiles in</span>';
  }
  return homes.map(function (h) {
    const nm = nodesById[h.position_node_id] || ('node#' + h.position_node_id);
    let badge = '';
    if (dedicated) {
      badge = isAuth ? payloadSelect(h.payload_code)
        : (h.payload_code ? '<span class="loader-pc-badge">' + escapeHtml(h.payload_code) + '</span>' : '');
    }
    // node-tile + loader-member: the slot reuses the grid node tile (same block/size/
    // state colour, copied in markLinkedTiles) with the loader outline + controls on top.
    return '<div class="node-tile loader-member" data-id="' + h.position_node_id + '"' + (isAuth ? ' draggable="true"' : '') + '>'
      + (isAuth ? '<span class="loader-grip" title="drag the tile to reorder / move">⠿</span>' : '')
      + '<span class="tile-loc">' + escapeHtml(nm) + '</span>'
      + badge
      + (isAuth ? '<span class="loader-member-x" title="remove" draggable="false">×</span>' : '')
      + '</div>';
  }).join('');
}

// payloadSelect is an inline per-position payload picker styled as a badge — it
// reads as a teal badge once a payload is chosen (has-payload class).
function payloadSelect(sel) {
  let opts = '<option value="">+ payload</option>';
  payloadCodes.forEach(function (c) {
    opts += '<option value="' + escapeHtml(c) + '"' + (c === sel ? ' selected' : '') + '>' + escapeHtml(c) + '</option>';
  });
  return '<select class="loader-pc-sel' + (sel ? ' has-payload' : '') + '" draggable="false">' + opts + '</select>';
}

// payloadChipsHtml renders a shared_window loader's allowed payload set (the
// shared codes any window may serve), separate from the window nodes above.
function payloadChipsHtml(item) {
  const ps = item.payloads || [];
  let html = '<div class="loader-payload-set"><span class="loader-set-label">Allowed payloads:</span>';
  html += ps.map(function (p) {
    return '<span class="loader-chip">' + escapeHtml(p.payload_code)
      + (isAuth ? '<span class="loader-chip-x" data-pc="' + escapeHtml(p.payload_code) + '" title="remove">×</span>' : '')
      + '</span>';
  }).join('');
  if (isAuth) {
    html += '<span class="loader-chip-add"><input list="loader-payloads-dl" class="loader-add-pc" placeholder="+ payload"><button class="loader-add-pc-btn">add</button></span>';
  }
  html += '</div>';
  return html;
}

/* ── Wiring ───────────────────────────────────────────── */

function wireAll(host) {
  host.querySelectorAll('.loader-box').forEach(function (box) {
    const lid = box.dataset.loaderId;
    box.addEventListener('dragover', onBoxDragOver);
    box.addEventListener('dragleave', onBoxDragLeave);
    box.addEventListener('drop', onBoxDrop);
    // 1b: each associated group zone (output / buffer) is a drop-target — dropping a
    // node tile there reparents it INTO that node group (topology move), distinct from
    // dropping on the box body (a loader-position overlay). Its slot tiles drag OUT.
    box.querySelectorAll('.loader-box-group-zone').forEach(function (g) {
      g.addEventListener('dragover', onGroupDragOver);
      g.addEventListener('dragleave', onGroupDragLeave);
      g.addEventListener('drop', onGroupDrop);
    });
    box.querySelectorAll('.loader-group-slot').forEach(function (s) {
      s.addEventListener('dragstart', onGroupSlotDragStart);
      s.addEventListener('dragend', function () { refresh(); });
    });

    box.querySelectorAll('.loader-member').forEach(function (g) {
      g.addEventListener('dragstart', onMemberDragStart);
      g.addEventListener('dragend', onMemberDragEnd);
    });
    box.querySelectorAll('.loader-pc-sel').forEach(function (s) {
      s.addEventListener('change', function () {
        s.classList.toggle('has-payload', !!s.value);
        setMemberPayload(lid, s.closest('.loader-member').dataset.id, s.value);
      });
    });
    box.querySelectorAll('.loader-member-x').forEach(function (x) {
      x.addEventListener('click', function () {
        removeMember(lid, x.closest('.loader-member').dataset.id);
      });
    });
    const del = box.querySelector('.loader-box-del');
    if (del) del.addEventListener('click', function () { deleteLoader(lid); });
    const edit = box.querySelector('.loader-box-edit');
    if (edit) edit.addEventListener('click', function () { editLoader(lid); });

    box.querySelectorAll('.loader-chip-x').forEach(function (x) {
      x.addEventListener('click', function () { removeChip(lid, x.dataset.pc); });
    });
    const addBtn = box.querySelector('.loader-add-pc-btn');
    if (addBtn) addBtn.addEventListener('click', function () {
      const inp = box.querySelector('.loader-add-pc');
      const pc = inp ? (inp.value || '').trim() : '';
      if (pc) addChip(lid, pc);
    });
  });
}

function onMemberDragStart(e) {
  const tile = e.target.closest('.loader-member');
  draggingMemberNode = tile ? tile.dataset.id : null;
  if (tile) tile.classList.add('dragging');
  e.dataTransfer.effectAllowed = 'move';
  // Custom type ONLY — leaving text/plain unset makes supermarket's onDropGrid
  // no-op if a member is dragged out onto the grid (no accidental reparent).
  e.dataTransfer.setData('application/x-loader-member', draggingMemberNode || '');
}
function onMemberDragEnd(e) {
  const tile = e.target.closest('.loader-member');
  if (tile) tile.classList.remove('dragging');
  draggingMemberNode = null;
}

function onBoxDragOver(e) {
  // Both layouts accept node drops (shared_window = windows, dedicated_positions
  // = positions). preventDefault + stopPropagation so the drop never falls
  // through to #nodes-drop-area's onDropGrid, which would reparent the node to
  // the grid bottom (the "disappear" bug).
  e.preventDefault();
  e.stopPropagation();
  e.dataTransfer.dropEffect = 'move';
  this.classList.add('loader-drop-target');
}
function onBoxDragLeave(e) { this.classList.remove('loader-drop-target'); }

function onBoxDrop(e) {
  e.preventDefault();
  e.stopPropagation(); // keep the drop from reaching onDropGrid (topology reparent)
  this.classList.remove('loader-drop-target');

  const member = e.dataTransfer.getData('application/x-loader-member');
  const nodeId = parseInt(member || e.dataTransfer.getData('text/plain'), 10);
  if (!nodeId) return;
  const lid = parseInt(this.dataset.loaderId, 10);

  const tiles = Array.from(this.querySelectorAll('.loader-member'));
  const already = tiles.some(function (t) { return parseInt(t.dataset.id, 10) === nodeId; });
  const existing = tiles.map(function (t) { return parseInt(t.dataset.id, 10); }).filter(function (id) { return id !== nodeId; });

  // Insert index from the drop X position (mirrors reorderLane).
  let idx = existing.length, k = 0;
  for (let i = 0; i < tiles.length; i++) {
    const id = parseInt(tiles[i].dataset.id, 10);
    if (id === nodeId) continue;
    const r = tiles[i].getBoundingClientRect();
    if (e.clientX < r.left + r.width / 2) { idx = k; break; }
    k++;
  }
  const ordered = existing.slice();
  ordered.splice(idx, 0, nodeId);

  const reorder = function () {
    apiPost('/api/loader/reorder-homes', { loader_id: lid, ordered_ids: ordered }).then(refresh).catch(function (err) { toast('' + err, 'error'); });
  };
  if (already) { reorder(); return; }
  // New position (from the grid or another loader) — preserve any prior payload/threshold on a move.
  const prev = findHomeAnyLoader(nodeId);
  apiPost('/api/loader/set-home', {
    loader_id: lid, position_node_id: nodeId,
    payload_code: prev ? prev.payload_code : '',
    min_stock: prev ? prev.min_stock : 0,
    uop_threshold: prev ? prev.uop_threshold : 0,
  }).then(function (d) {
    if (d && d.error) { toast(d.error, 'error'); return; }
    reorder();
  }).catch(function (err) { toast('' + err, 'error'); });
}

// 1b: group-chip drop-target handlers. stopPropagation keeps the drop from also
// reaching the box (position assign) or the grid (#nodes-drop-area reparent-to-bottom).
function onGroupDragOver(e) {
  e.preventDefault();
  e.stopPropagation();
  e.dataTransfer.dropEffect = 'move';
  this.classList.add('loader-group-drop-target');
}
function onGroupDragLeave() { this.classList.remove('loader-group-drop-target'); }
function onGroupDrop(e) {
  e.preventDefault();
  e.stopPropagation();
  this.classList.remove('loader-group-drop-target');
  const member = e.dataTransfer.getData('application/x-loader-member');
  const nodeId = parseInt(member || e.dataTransfer.getData('text/plain'), 10);
  if (!nodeId) return;
  const groupName = this.dataset.group;
  const parentId = nodesByName[groupName];
  if (parentId == null) { toast('node group ' + groupName + ' not found', 'error'); return; }
  // Reparent the node INTO the group's NGRP — the group owns its slots (topology move,
  // unlike the loader-home overlay). Guarded server-side: a 409 means orders reference
  // the node's current group; surface it rather than force.
  apiPost('/api/node-group/reparent-node', { node_id: nodeId, parent_id: parentId, force: false })
    .then(function (d) { if (d && d.error) { toast(d.error, 'error'); return; } refresh(); })
    .catch(function (err) { toast('' + err, 'error'); });
}

// onGroupSlotDragStart: dragging a slot tile OUT — set text/plain so the grid's onDropGrid
// reparents it back out of the group (or another zone's onGroupDrop re-homes it); the
// dragend handler refreshes so the box reflects the move. stopPropagation keeps the box's
// member-drag from also firing.
function onGroupSlotDragStart(e) {
  e.stopPropagation();
  e.dataTransfer.effectAllowed = 'move';
  e.dataTransfer.setData('text/plain', e.currentTarget.dataset.id || '');
}

/* ── Mutations ────────────────────────────────────────── */

function setMemberPayload(lid, nodeId, pc) {
  const home = findHome(lid, nodeId);
  apiPost('/api/loader/set-home', {
    loader_id: Number(lid), position_node_id: Number(nodeId), payload_code: pc,
    min_stock: home ? home.min_stock : 0, uop_threshold: home ? home.uop_threshold : 0,
  }).then(refresh).catch(function (err) { toast('' + err, 'error'); });
}
function removeMember(lid, nodeId) {
  apiPost('/api/loader/remove-home', { loader_id: Number(lid), position_node_id: Number(nodeId) }).then(refresh).catch(function (err) { toast('' + err, 'error'); });
}
function deleteLoader(lid) {
  apiPost('/api/loader/delete', { id: Number(lid) }).then(refresh).catch(function (err) { toast('' + err, 'error'); });
}
function addChip(lid, pc) {
  apiPost('/api/loader/set-payload', { loader_id: Number(lid), payload_code: pc, uop_threshold: 0 }).then(refresh).catch(function (err) { toast('' + err, 'error'); });
}
function removeChip(lid, pc) {
  apiPost('/api/loader/remove-payload', { loader_id: Number(lid), payload_code: pc }).then(refresh).catch(function (err) { toast('' + err, 'error'); });
}

function findHome(lid, nodeId) {
  const item = loaderData.find(function (it) { return String(it.loader.id) === String(lid); });
  if (!item) return null;
  return (item.homes || []).find(function (h) { return String(h.position_node_id) === String(nodeId); }) || null;
}
function findHomeAnyLoader(nodeId) {
  for (const it of loaderData) {
    const h = (it.homes || []).find(function (hm) { return String(hm.position_node_id) === String(nodeId); });
    if (h) return h;
  }
  return null;
}

/* ── Init ─────────────────────────────────────────────── */

delegateActions(document.body, { openLoaderModal, closeLoaderModal, submitLoader });

// Continuous edge auto-scroll while a node tile is dragged. Native HTML5 drag
// suppresses the mouse WHEEL entirely (no wheel events fire during a drag), so
// the only way to scroll mid-drag is to push the cursor toward the top/bottom
// edge. A 16ms timer (started on dragstart, stopped on dragend/drop) scrolls the
// window smoothly while the cursor sits in the edge band — speed scales with how
// deep into the band it is — which works even when the cursor is held still
// (per-dragover nudges fire too sparsely to scroll). Window is the scroller (no
// inner overflow container on this page), so window.scrollBy is correct.
let _dragY = null;
let _dragScrollTimer = null;
function startDragScroll() {
  if (_dragScrollTimer) return;
  _dragScrollTimer = setInterval(function () {
    if (_dragY == null) return;
    const margin = 110, h = window.innerHeight;
    if (_dragY < margin) window.scrollBy(0, -(6 + Math.ceil((margin - _dragY) / 3)));
    else if (_dragY > h - margin) window.scrollBy(0, 6 + Math.ceil((_dragY - (h - margin)) / 3));
  }, 16);
}
function stopDragScroll() {
  if (_dragScrollTimer) { clearInterval(_dragScrollTimer); _dragScrollTimer = null; }
  _dragY = null;
}

// Run on/after DOMContentLoaded so the supermarket's buildHierarchy (registered
// earlier) has finished placing tiles before markLinkedTiles rings them. A
// deferred module executes at readyState 'interactive', so the listener still
// fires; 'complete' covers a late/dynamic load.
function init() {
  const layoutSel = document.getElementById('loader-layout');
  if (layoutSel) layoutSel.addEventListener('change', setLayoutFlowVisibility);
  document.addEventListener('dragstart', startDragScroll);
  document.addEventListener('dragover', function (e) { _dragY = e.clientY; });
  document.addEventListener('dragend', stopDragScroll);
  document.addEventListener('drop', stopDragScroll);
  refresh();
}
if (document.readyState === 'complete') {
  init();
} else {
  document.addEventListener('DOMContentLoaded', init);
}
