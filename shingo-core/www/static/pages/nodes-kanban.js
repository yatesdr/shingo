import { delegateActions, escapeHtml } from '/static/shared/dom.js';
import { confirm as uiConfirm } from '/static/shared/layer.js';
import { toast } from '/static/shared/notice.js';
import { apiGet, apiPost } from '/static/app.js';

// Kanban section of the node group settings modal.
//
// A kanban is a node group that declares a level per bin type and a pool per carrier
// family: Core counts its loop, admits every cart of the loop, fetches only what the loop
// has lost. A group with a level is one; there is no switch. This module owns the
// section's whole lifecycle — load, render, stage, save.
//
// NOT "buffer" in any string a person reads. A loader window can have
// home_kind=buffer, which is a different thing with opposite behaviour, and one
// word for two mechanisms is how an operator ends up configuring the wrong one.
//
// STAGED, THEN FLUSHED ON SAVE — unlike the loaders page's mix editor, which
// posts on every keystroke. The shape and vocabulary of the rows are that
// editor's, deliberately, because an operator who has used one should recognise
// the other. The COMMIT is different because the surroundings are: everything
// else in this modal (enabled, allowed bins, algorithms, waiting points) commits
// on Save, and a section that wrote through immediately would make Cancel mean
// two different things on one screen.

var _allStations = JSON.parse(document.getElementById('page-data').dataset.edges || '[]');
var _allBinTypes = JSON.parse(document.getElementById('page-data').dataset.binTypes || '[]');

// One screen's worth of staged state. Rebuilt from the server on every open, so
// nothing survives a modal close — a stale level from the last group you looked
// at is exactly the edit nobody would catch.
var _kanban = null;

function blank(nodeID) {
  return {
    nodeID: nodeID,
    // levels: [{bin_type_id, bin_type_code, want,
    // layer_wants: [{layers, want}]}] as edited. layer_wants are the level's wanted heights, told
    // to the unloader; the one at 0 layers is the type's tops & bottoms row, a cap
    // of its own beside the level (a tote type only).
    levels: [],
    // savedLevels: what the server had, for the remove-diff on save.
    savedLevels: [],
    // pools: {family_id: pool} as edited on the family table, savedPools what the server had.
    // Only a changed pool is posted: an untouched one stands at whatever Core says.
    pools: {},
    savedPools: {},
    // sources: [{id, name}], the groups the keeper may pull from, first to last.
    sources: [],
    // processes: [{process_id, nodes:[{id,name}]}] — the picker's contents.
    processes: [],
    // selected: Set of process_id.
    selected: new Set(),
    // orphans: stored support node ids that belong to no listed process. Kept
    // and re-saved rather than dropped: they usually mean the claim topology
    // moved after somebody saved, and silently deleting a supported position is
    // not a thing a render should do.
    orphans: [],
    // loaded: the server's answer is in. Until then the section shows this
    // blank state, and a save would write its empty sources and supports.
    loaded: false
  };
}

// kanbanLoading says whether the open group's section is still waiting on its
// load. The pop-up's Save writes nothing while it is (nodes-detail.js
// handleNodeSave). A load that failed hides the section, and Save leaves the
// kanban alone.
export function kanbanLoading() {
  return !!_kanban && !_kanban.loaded;
}

// renderKanbanSection loads and draws the section, or hides it.
//
// Hidden for anything that is not a node group. It is also hidden for an
// unauthenticated viewer, because the whole section lives inside the auth-only
// half of the modal and does not exist in the read-only one.
export function renderKanbanSection(isGroupType, nodeID) {
  var box = document.getElementById('nf-kanban');
  if (!box) return;
  _kanban = null;
  if (!isGroupType || !nodeID) {
    box.classList.add('hide');
    setAlgoNote(false);
    return;
  }
  box.classList.remove('hide');
  // mine is this open's state. An answer that finds another in _kanban is for
  // an open no longer on screen, this group's earlier one included, and changes
  // nothing: a late failure used to hide the section of the group opened since.
  var mine = blank(Number(nodeID));
  _kanban = mine;

  Promise.all([
    apiGet('/api/nodes/kanban?id=' + nodeID),
    // The picker's contents are a plant-wide fact, not a per-group one, but it
    // is fetched per open rather than once: a claim set that changed while the
    // page was left open is exactly the case where a cached list offers a
    // process that no longer exists.
    apiGet('/api/nodes/process-options').catch(function (err) {
      // A missing claim mirror must not stop the rest of the section from
      // rendering — the level is editable without it.
      console.warn('kanban: process options', err);
      return { processes: [] };
    })
  ]).then(function (res) {
    var cfg = res[0] || {};
    var opts = (res[1] && res[1].processes) || [];
    if (_kanban !== mine) return; // modal moved on
    _kanban.levels = (cfg.levels || []).map(function (l) {
      return {
        bin_type_id: Number(l.bin_type_id), bin_type_code: l.bin_type_code, want: Number(l.want),
        layer_wants: (cfg.layer_wants || []).filter(function (w) {
          return Number(w.bin_type_id) === Number(l.bin_type_id);
        }).map(function (w) { return { layers: Number(w.layers), want: Number(w.want) }; })
      };
    });
    _kanban.savedLevels = _kanban.levels.map(savedLevel);
    _kanban.sources = (cfg.sources || []).map(function (s) {
      return { id: Number(s.source_node_id), name: s.source_node_name };
    });
    _kanban.processes = opts;

    var storedNodes = new Set((cfg.supports || []).map(function (s) { return Number(s.process_node_id); }));
    var claimed = new Set();
    opts.forEach(function (p) {
      var ids = p.nodes.map(function (n) { return Number(n.id); });
      ids.forEach(function (id) { claimed.add(id); });
      // A process counts as selected when every position it names is stored.
      // Partial overlap reads as not-selected on purpose: ticking the box would
      // claim the operator asked for positions they never saw.
      if (ids.length && ids.every(function (id) { return storedNodes.has(id); })) {
        _kanban.selected.add(p.process_id);
      }
    });
    _kanban.orphans = Array.from(storedNodes).filter(function (id) { return !claimed.has(id); });

    document.getElementById('nf-kanban-strict').checked = !!cfg.strict_sourcing;
    fillStations(cfg.maintenance_station || '');
    fillOverflow(cfg.overflow_destination || '');
    renderLevels();
    renderSources();
    renderSupports();
    _kanban.loaded = true;
  }).catch(function (err) {
    if (_kanban !== mine) return; // modal moved on
    console.error('renderKanbanSection', err);
    // Loud, and the section stays hidden. A section rendered from a failed fetch
    // shows an empty level an operator can save, which turns a read error into a
    // configuration change.
    box.classList.add('hide');
    _kanban = null;
    toast('Could not load the kanban settings for this group.', 'error');
  });
}

// savedLevel is what a save diffs a level line against.
function savedLevel(l) {
  return { bin_type_id: l.bin_type_id, want: l.want };
}

// setAlgoNote annotates the ASRS/algorithm controls instead of hiding them. They
// still decide everything about LOADED carriers here; what a level takes over is
// the empty pull. Saying which half stopped applying is information — a control
// that vanished would just be a question.
function setAlgoNote(on) {
  var note = document.getElementById('nf-kanban-algo-note');
  if (note) note.classList.toggle('hide', !on);
}

// otherGroups is the node groups on the page, this one left out, off the tiles:
// [{id, name}], sorted by name. NGRP is the only group code: Core's v6 migration
// renamed the legacy SMKT / SUP codes to it.
function otherGroups() {
  var seen = {};
  var out = [];
  document.querySelectorAll('.node-tile').forEach(function (t) {
    if (t.dataset.typeCode !== 'NGRP') return;
    var name = t.dataset.name;
    if (!name || seen[name]) return;
    if (_kanban && String(t.dataset.id) === String(_kanban.nodeID)) return; // never itself
    seen[name] = true;
    out.push({ id: Number(t.dataset.id), name: name });
  });
  out.sort(function (a, b) { return a.name.localeCompare(b.name); });
  return out;
}

function fillStations(current) {
  var sel = document.getElementById('nf-kanban-station');
  if (!sel) return;
  var html = '<option value="">— none —</option>';
  _allStations.forEach(function (e) {
    var id = e.station_id || e.station_uid || '';
    if (!id) return;
    html += '<option value="' + escapeHtml(id) + '">' + escapeHtml(e.display_name || id) + '</option>';
  });
  sel.innerHTML = html;
  // A station that is configured but no longer enrolled must still show. Losing
  // it on render would let the next Save silently clear it.
  if (current && !Array.prototype.some.call(sel.options, function (o) { return o.value === current; })) {
    sel.insertAdjacentHTML('beforeend',
      '<option value="' + escapeHtml(current) + '">' + escapeHtml(current) + ' (not enrolled)</option>');
  }
  sel.value = current;
}

function fillOverflow(current) {
  var sel = document.getElementById('nf-kanban-overflow');
  if (!sel) return;
  var html = '<option value="">— none —</option>';
  var seen = {};
  otherGroups().forEach(function (g) {
    seen[g.name] = true;
    html += '<option value="' + escapeHtml(g.name) + '">' + escapeHtml(g.name) + '</option>';
  });
  sel.innerHTML = html;
  if (current && !seen[current]) {
    sel.insertAdjacentHTML('beforeend',
      '<option value="' + escapeHtml(current) + '">' + escapeHtml(current) + ' (not on this page)</option>');
  }
  sel.value = current;
}

// renderLevels draws the declared level, one row per carrier type with its
// wanted heights under it, plus the add row. Same controls and the same vocabulary as the loaders page's carrier mix —
// but the two are not the same promise: a mix is a preference bounded by the
// loader's windows, a level is the number Core holds.
function renderLevels() {
  var host = document.getElementById('nf-kanban-levels');
  if (!host || !_kanban) return;
  setAlgoNote(_kanban.levels.length > 0);
  // A kanban has no overflow: a sized loop always has a slot. A plain group keeps its field.
  var overflowBox = document.getElementById('nf-kanban-overflow-box');
  if (overflowBox) overflowBox.classList.toggle('hide', _kanban.levels.length > 0);
  var declared = {};
  _kanban.levels.forEach(function (l) { declared[l.bin_type_id] = true; });

  var rows = _kanban.levels.map(function (l) {
    return '<div class="loader-mix-line">'
      + '<span class="loader-chip">' + escapeHtml(l.bin_type_code) + '</span>'
      + '<input type="number" class="form-input loader-mix-want" min="0" value="' + Number(l.want) + '"'
      + ' aria-label="How many ' + escapeHtml(l.bin_type_code) + ' to keep on hand"'
      + ' data-action-change="setKanbanWant" data-bin-type-id="' + l.bin_type_id + '">'
      + '<button type="button" class="btn btn-sm" title="Remove"'
      + ' data-action="removeKanbanLevel" data-bin-type-id="' + l.bin_type_id + '">&times;</button>'
      + '</div>'
      + renderLayerWants(l);
  }).join('');

  var rest = _allBinTypes.filter(function (t) { return !declared[Number(t.id)]; });
  var add = '';
  if (rest.length) {
    add = '<div class="loader-mix-add">'
      + '<select id="nf-kanban-add-type" class="form-input" aria-label="Carrier type">'
      + rest.map(function (t) {
        return '<option value="' + Number(t.id) + '">' + escapeHtml(t.code || t.label || '') + '</option>';
      }).join('')
      + '</select>'
      + '<input type="number" id="nf-kanban-add-want" class="form-input loader-mix-want" min="0" value="1" aria-label="How many">'
      + '<button type="button" class="btn btn-sm" data-action="addKanbanLevel">Add</button>'
      + '</div>';
  }
  host.innerHTML = (rows || '<div class="text-muted" style="font-size:0.75rem">No level declared.</div>') + add;
}

// isToteType says whether a carrier type states its totes per layer: only such
// a type can be wanted at a height, tops & bottoms included.
function isToteType(id) {
  var t = _allBinTypes.find(function (x) { return Number(x.id) === Number(id); });
  return !!(t && Number(t.totes_per_layer) > 0);
}

// renderLayerWants draws one level's wanted heights, for a tote type only. The
// fixed "Tops & bottoms" row is the type's row at 0 layers: how many carts at 0
// the group keeps, a cap of its own beside the level (0 keeps none, and the
// group then refuses one). The heights of 1 or more under it tell the unloader
// what to send a cart back as; they are not added up against the level, and
// the level stays the number Core holds.
function renderLayerWants(l) {
  var id = l.bin_type_id;
  var code = escapeHtml(l.bin_type_code);
  if (!isToteType(id)) return '';
  var tb = (l.layer_wants || []).find(function (w) { return w.layers === 0; });
  var tbRow = '<div class="loader-mix-line" style="margin-left:1.5rem">'
    + '<span class="text-sm">Tops &amp; bottoms</span>'
    + '<input type="number" class="form-input loader-mix-want" min="0" value="' + (tb ? Number(tb.want) : 0) + '"'
    + ' aria-label="How many ' + code + ' tops &amp; bottoms to keep"'
    + ' data-action-change="setKanbanTopsAndBottoms" data-bin-type-id="' + id + '">'
    + '</div>';
  var rows = (l.layer_wants || []).filter(function (w) { return w.layers > 0; }).map(function (w) {
    return '<div class="loader-mix-line" style="margin-left:1.5rem">'
      + '<span class="text-sm">' + Number(w.layers) + ' high</span>'
      + '<input type="number" class="form-input loader-mix-want" min="0" value="' + Number(w.want) + '"'
      + ' aria-label="How many ' + code + ' carts ' + Number(w.layers) + ' high"'
      + ' data-action-change="setKanbanLayerWant" data-bin-type-id="' + id + '"'
      + ' data-layers="' + Number(w.layers) + '">'
      + '<button type="button" class="btn btn-sm" title="Remove this height"'
      + ' data-action="removeKanbanLayerWant" data-bin-type-id="' + id + '"'
      + ' data-layers="' + Number(w.layers) + '">&times;</button>'
      + '</div>';
  }).join('');
  return tbRow + rows
    + '<div class="loader-mix-add" style="margin-left:1.5rem">'
    + '<input type="number" class="form-input loader-mix-want" min="1" placeholder="Layers"'
    + ' id="nf-kanban-add-layers-' + id + '" aria-label="' + code + ' height in layers">'
    + '<span class="text-sm">high, want</span>'
    + '<input type="number" class="form-input loader-mix-want" min="0" value="1"'
    + ' id="nf-kanban-add-layer-want-' + id + '" aria-label="How many ' + code + ' carts at that height">'
    + '<button type="button" class="btn btn-sm" data-action="addKanbanLayerWant"'
    + ' data-bin-type-id="' + id + '">Add height</button>'
    + '</div>';
}

function levelByID(id) {
  return _kanban && _kanban.levels.find(function (l) { return l.bin_type_id === id; });
}

export function addKanbanLayerWant(el) {
  var id = Number(el.getAttribute('data-bin-type-id'));
  var l = levelByID(id);
  if (!l) return;
  var layers = Number((document.getElementById('nf-kanban-add-layers-' + id) || {}).value);
  var n = Number((document.getElementById('nf-kanban-add-layer-want-' + id) || {}).value);
  if (!(layers >= 1) || Math.floor(layers) !== layers) {
    toast('A height is a whole number of layers, 1 or more.', 'error');
    return;
  }
  if (!(n >= 0)) n = 0;
  var have = l.layer_wants.find(function (w) { return w.layers === layers; });
  if (have) have.want = n;
  else l.layer_wants.push({ layers: layers, want: n });
  l.layer_wants.sort(function (a, b) { return a.layers - b.layers; });
  renderLevels();
}

export function setKanbanLayerWant(el) {
  var l = levelByID(Number(el.getAttribute('data-bin-type-id')));
  if (!l) return;
  var layers = Number(el.getAttribute('data-layers'));
  var n = Number(el.value);
  if (!(n >= 0)) { n = 0; el.value = '0'; }
  l.layer_wants.forEach(function (w) { if (w.layers === layers) w.want = n; });
}

// setKanbanTopsAndBottoms sets the type's tops & bottoms row; 0 removes it,
// so the card offers tops & bottoms only where a group keeps some.
export function setKanbanTopsAndBottoms(el) {
  var l = levelByID(Number(el.getAttribute('data-bin-type-id')));
  if (!l) return;
  var n = Number(el.value);
  if (!(n >= 0) || Math.floor(n) !== n) { n = 0; el.value = '0'; }
  l.layer_wants = l.layer_wants.filter(function (w) { return w.layers !== 0; });
  if (n > 0) l.layer_wants.unshift({ layers: 0, want: n });
}

export function removeKanbanLayerWant(el) {
  var l = levelByID(Number(el.getAttribute('data-bin-type-id')));
  if (!l) return;
  var layers = Number(el.getAttribute('data-layers'));
  l.layer_wants = l.layer_wants.filter(function (w) { return w.layers !== layers; });
  renderLevels();
}

export function addKanbanLevel() {
  if (!_kanban) return;
  var sel = document.getElementById('nf-kanban-add-type');
  var want = document.getElementById('nf-kanban-add-want');
  var id = Number(sel && sel.value);
  if (!id) return;
  var n = Number(want && want.value);
  if (!(n >= 0)) n = 0;
  var t = _allBinTypes.find(function (x) { return Number(x.id) === id; });
  _kanban.levels.push({ bin_type_id: id, bin_type_code: (t && (t.code || t.label)) || String(id), want: n, layer_wants: [] });
  _kanban.levels.sort(function (a, b) { return a.bin_type_code.localeCompare(b.bin_type_code); });
  renderLevels();
}

export function setKanbanWant(el) {
  if (!_kanban) return;
  var id = Number(el.getAttribute('data-bin-type-id'));
  var n = Number(el.value);
  // Zero is a real declared value — "none of this type, on purpose" — so it is
  // kept rather than treated as a removal. Negative is not a smaller level, it
  // is a number nothing can act on.
  if (!(n >= 0)) { n = 0; el.value = '0'; }
  _kanban.levels.forEach(function (l) { if (l.bin_type_id === id) l.want = n; });
}

export function removeKanbanLevel(el) {
  if (!_kanban) return;
  var id = Number(el.getAttribute('data-bin-type-id'));
  _kanban.levels = _kanban.levels.filter(function (l) { return l.bin_type_id !== id; });
  renderLevels();
}

// moveInList returns list with the entry at index i moved by delta, a copy; the
// same list when that would leave the ends. The sources' up and down buttons.
function moveInList(list, i, delta) {
  var to = i + delta;
  if (i < 0 || i >= list.length || to < 0 || to >= list.length) return list;
  var out = list.slice();
  var moved = out.splice(i, 1)[0];
  out.splice(to, 0, moved);
  return out;
}

// sourceRowsHtml draws the sources in order, each with its own up, down and
// remove, and the add row: one group picked and added at a time, the groups
// already listed left out. Nothing here selects more than one.
function sourceRowsHtml(sources, groups) {
  var rows = sources.map(function (s, i) {
    return '<div class="loader-mix-line">'
      + '<span class="text-sm tnum">' + (i + 1) + '.</span>'
      + '<span class="loader-chip">' + escapeHtml(s.name) + '</span>'
      + '<button type="button" class="btn btn-sm" title="Earlier" data-action="moveKanbanSource" data-index="' + i + '" data-delta="-1"'
      + (i === 0 ? ' disabled' : '') + '>&uarr;</button>'
      + '<button type="button" class="btn btn-sm" title="Later" data-action="moveKanbanSource" data-index="' + i + '" data-delta="1"'
      + (i === sources.length - 1 ? ' disabled' : '') + '>&darr;</button>'
      + '<button type="button" class="btn btn-sm" title="Remove" data-action="removeKanbanSource" data-index="' + i + '">&times;</button>'
      + '</div>';
  }).join('');
  var listed = {};
  sources.forEach(function (s) { listed[s.id] = true; });
  var rest = groups.filter(function (g) { return !listed[g.id]; });
  var add = '';
  if (rest.length) {
    add = '<div class="loader-mix-add">'
      + '<select id="nf-kanban-add-source" class="form-input" aria-label="Source group">'
      + rest.map(function (g) { return '<option value="' + g.id + '">' + escapeHtml(g.name) + '</option>'; }).join('')
      + '</select>'
      + '<button type="button" class="btn btn-sm" data-action="addKanbanSource">Add</button>'
      + '</div>';
  }
  return (rows || '<div class="text-muted" style="font-size:0.75rem">No sources: the keeper searches the plant.</div>') + add;
}

function renderSources() {
  var host = document.getElementById('nf-kanban-sources');
  if (!host || !_kanban) return;
  host.innerHTML = sourceRowsHtml(_kanban.sources, otherGroups());
}

export function addKanbanSource() {
  if (!_kanban) return;
  var sel = document.getElementById('nf-kanban-add-source');
  var id = Number(sel && sel.value);
  if (!id) return;
  var g = otherGroups().find(function (x) { return x.id === id; });
  if (g) _kanban.sources.push(g);
  renderSources();
}

export function removeKanbanSource(el) {
  if (!_kanban) return;
  var i = Number(el.getAttribute('data-index'));
  _kanban.sources = _kanban.sources.filter(function (_, at) { return at !== i; });
  renderSources();
}

export function moveKanbanSource(el) {
  if (!_kanban) return;
  _kanban.sources = moveInList(_kanban.sources, Number(el.getAttribute('data-index')), Number(el.getAttribute('data-delta')));
  renderSources();
}

// renderSupports lists PROCESSES, with the positions each one resolves to shown
// underneath. The operator picks a process because that is the thing they can
// point at on the floor; what gets stored is the positions, because a claim
// lives on the Edge and Core cannot read one when it has to decide anything.
function renderSupports() {
  var host = document.getElementById('nf-kanban-supports');
  if (!host || !_kanban) return;
  if (!_kanban.processes.length) {
    host.innerHTML = '<div class="text-muted" style="font-size:0.75rem">'
      + 'No processes have reported claims yet.</div>';
    return;
  }
  var html = _kanban.processes.map(function (p) {
    var names = p.nodes.map(function (n) { return n.name; }).join(', ');
    return '<label class="text-sm" style="display:flex;align-items:flex-start;gap:8px;margin-bottom:6px;cursor:pointer">'
      + '<input type="checkbox" data-action-change="toggleKanbanSupport"'
      + ' data-process-id="' + escapeHtml(p.process_id) + '"'
      + (_kanban.selected.has(p.process_id) ? ' checked' : '') + '>'
      + '<span><strong>' + escapeHtml(p.process_id) + '</strong>'
      + '<span class="text-muted" style="display:block;font-size:0.72rem">' + escapeHtml(names) + '</span>'
      + '</span></label>';
  }).join('');
  if (_kanban.orphans.length) {
    html += '<div class="text-muted" style="font-size:0.72rem;margin-top:6px">'
      + _kanban.orphans.length + ' saved position'
      + (_kanban.orphans.length === 1 ? ' matches' : 's match')
      + ' no known process. Kept as-is — clear them in the claims.</div>';
  }
  host.innerHTML = html;
}

export function toggleKanbanSupport(el) {
  if (!_kanban) return;
  var pid = el.getAttribute('data-process-id');
  if (el.checked) _kanban.selected.add(pid);
  else _kanban.selected.delete(pid);
}

// rawPost is apiPost without the envelope-unwrapping.
//
// app.js's api() rejects a non-2xx with the server's `error` sentence alone,
// which is the right shape almost everywhere and the wrong one here: the
// holds-bins refusal carries `needs_force` and `drain` alongside the message,
// and unwrapping to the sentence throws both away. Ten lines of fetch rather than widening a helper the
// whole application shares.
function rawPost(url, body) {
  return fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body)
  }).then(function (r) {
    return r.text().then(function (t) {
      var parsed = null;
      try { parsed = JSON.parse(t); } catch (e) { /* not JSON: fall through */ }
      return { ok: r.ok, body: parsed, text: t };
    });
  });
}

// postConfirmable posts, and when the server answers "carriers are standing
// there" it ASKS and re-posts with force.
//
// The confirm is here rather than in front of the request because the browser
// does not know what is standing in the group — only the server does, and only
// at the moment of the save. Asking first would mean either asking every time
// (which trains an operator to click through it) or duplicating the guard on
// this side, where it would be a second definition of the rule.
//
// Only the holds-bins guard is confirmable. A rule refusal carries no
// needs_force and throws, because "this group is already a loader's staging
// group" does not become untrue when somebody clicks again.
async function postConfirmable(url, body) {
  var res = await rawPost(url, body);
  if (res.ok) return res.body;

  var b = res.body || {};
  if (!b.needs_force) throw (b.error || res.text);

  var msg = b.error || 'This change affects carriers standing in the group.';
  if (b.drain && b.drain.length) {
    msg += '\n\nAlso already on their way: ' + b.drain.join('; ');
  }
  if (!await uiConfirm(msg + '\n\nSave anyway?')) {
    // Not an error — the operator answered. Nothing further is posted for this
    // step and the rest of the flush carries on, which is the point of flushing
    // per-thing rather than all at once.
    return null;
  }
  var forced = {};
  Object.keys(body).forEach(function (k) { forced[k] = body[k]; });
  forced.force = true;
  var again = await rawPost(url, forced);
  if (!again.ok) throw ((again.body && again.body.error) || again.text);
  return again.body;
}

// confirmAllowedBinsNarrowing asks before the node form posts a narrower Allowed
// Bins set at a kanban, and returns whether to go ahead.
//
// The form navigates, so the server cannot ask — a 409 there replaces the page
// with an error document. It answers the question here and carries the answer in
// as `force`; the form handler runs the same guard regardless, so a caller that
// skips this is still refused.
//
// Returns true when there is nothing to ask about, which is the overwhelmingly
// common case: any non-group node, any widening, any group holding nothing.
export async function confirmAllowedBinsNarrowing(nodeID, binTypeIDs, form) {
  if (!nodeID) return true;
  var res;
  try {
    res = await apiPost('/api/nodes/kanban/check-types',
      { node_id: Number(nodeID), bin_type_ids: binTypeIDs.map(Number) });
  } catch (err) {
    // A check that could not run must not silently become a pass — but it also
    // must not block an ordinary save on an ordinary node. The server-side
    // guard is the authority either way; this one only buys the dialog.
    console.warn('allowed bins: holds-bins check', err);
    return true;
  }
  if (!res || !res.blocked) return true;
  if (!await uiConfirm(res.blocked + '\n\nSave anyway?')) return false;
  if (form) {
    var inp = document.createElement('input');
    inp.type = 'hidden';
    inp.name = 'force';
    inp.value = 'on';
    form.appendChild(inp);
  }
  return true;
}

// saveKanban flushes the section. Called from the modal's Save, before
// the form posts.
//
// Settings first, then the level, then the supports and sources — a refusal names
// the setting it refused, and an operator reading "this group is already a
// loader's staging group" wants it against the switch they just flipped, not
// against a level row they did not touch. The station comes ahead of the supports
// because the supports need it.
//
// Returns false when a post was refused or failed, with the reason shown: the
// modal's Save stops there, since the form post would reload the page over it.
// An operator who answers no to a "Save anyway?" has refused nothing.
export async function saveKanban() {
  var box = document.getElementById('nf-kanban');
  if (!box || box.classList.contains('hide') || !_kanban) return true;

  var nodeID = _kanban.nodeID;
  var strict = document.getElementById('nf-kanban-strict').checked;
  var station = document.getElementById('nf-kanban-station').value;
  var overflow = document.getElementById('nf-kanban-overflow').value;

  // Warnings collect across the whole flush and are shown once. Four toasts for
  // one Save is four things to dismiss and one thing to read.
  var warnings = [];
  var collect = function (res) {
    if (res && res.warnings) warnings = warnings.concat(res.warnings);
    if (res && res.drain && res.drain.length) {
      warnings.push(res.drain.length + ' order' + (res.drain.length === 1 ? '' : 's')
        + ' already sourcing here will still be served: ' + res.drain.join('; '));
    }
    return res;
  };

  try {
    collect(await postConfirmable('/api/nodes/kanban/settings', {
      group_node_id: nodeID,
      strict_sourcing: strict,
      maintenance_station: station,
      overflow_destination: overflow
    }));

    // Removals come off the DIFF against what the server had. Deleting every row
    // and re-adding would be simpler and would also, for a moment, leave a
    // kanban with no declared level at all.
    var still = {};
    _kanban.levels.forEach(function (l) { still[l.bin_type_id] = true; });
    for (var i = 0; i < _kanban.savedLevels.length; i++) {
      var was = _kanban.savedLevels[i];
      if (!still[was.bin_type_id]) {
        collect(await apiPost('/api/nodes/kanban/level/remove',
          { group_node_id: nodeID, bin_type_id: was.bin_type_id }));
      }
    }
    // Each level carries its wanted heights. They were loaded with the level
    // (the section stays hidden when that load fails), so the list sent is
    // the one on screen and replaces what is stored, an empty list included.
    for (var j = 0; j < _kanban.levels.length; j++) {
      var l = _kanban.levels[j];
      collect(await apiPost('/api/nodes/kanban/level', {
        group_node_id: nodeID, bin_type_id: l.bin_type_id, want: l.want,
        layer_wants: l.layer_wants.map(function (w) { return { layers: w.layers, want: w.want }; })
      }));
    }

    var nodeIDs = _kanban.orphans.slice();
    _kanban.processes.forEach(function (p) {
      if (!_kanban.selected.has(p.process_id)) return;
      p.nodes.forEach(function (n) {
        if (nodeIDs.indexOf(Number(n.id)) < 0) nodeIDs.push(Number(n.id));
      });
    });
    collect(await postConfirmable('/api/nodes/kanban/supports',
      { group_node_id: nodeID, process_node_ids: nodeIDs }));
    collect(await apiPost('/api/nodes/kanban/sources',
      { group_node_id: nodeID, source_node_ids: _kanban.sources.map(function (s) { return s.id; }) }));
    var changed = Object.keys(_kanban.pools).filter(function (id) { return _kanban.pools[id] !== _kanban.savedPools[id]; })
      .map(function (id) { return { family_id: Number(id), pool: _kanban.pools[id] }; });
    if (changed.length) {
      collect(await apiPost('/api/nodes/kanban/pools', { group_node_id: nodeID, pools: changed }));
      _kanban.savedPools = Object.assign({}, _kanban.pools);
    }

    // The staged state now matches the server, so a second Save without
    // reopening does not re-issue removals for rows already gone.
    _kanban.savedLevels = _kanban.levels.map(savedLevel);

    // Deduplicated: the same warning comes back from every endpoint that
    // re-validated the whole configuration, which is all of them.
    var seen = {};
    warnings.filter(function (m) {
      if (seen[m]) return false;
      seen[m] = true;
      return true;
    // Sticky: a warning is a sentence about the plant's configuration, and the
    // default toast is gone in three seconds — long enough to notice something
    // appeared, not long enough to read what it said.
    }).forEach(function (m) { toast(m, 'warning', { sticky: true }); });
  } catch (err) {
    // A refusal (409) arrives as a rejected request whose string IS the
    // server's error sentence — api() unwraps the JSON envelope. Shown
    // as-is rather than wrapped, because the string already names the setting:
    // "GRP-A is already the staging group for loader CT-L" is the whole message.
    toast('' + err, 'error', { sticky: true });
    return false;
  }
  return true;
}

delegateActions(document.body, {
  addKanbanLayerWant,
  addKanbanLevel,
  addKanbanSource,
  moveKanbanSource,
  removeKanbanLayerWant,
  removeKanbanLevel,
  removeKanbanSource,
  setKanbanLayerWant,
  setKanbanPool,
  setKanbanTopsAndBottoms,
  setKanbanWant,
  toggleKanbanSupport
}, { events: ['click', 'change'] });

// ── The keeper's levels (moved from /inventory, owner 2026-10-29) ──────────
//
// The keeper's last tick for one kanban. Per family: pool, minimum, where its carts are, the
// room that admits it, and the floaters. Per type: want − here − on the line − asked − coming −
// at the windows = short, printed term by term rather than collapsed into a status, because a
// group that is short and quiet has opposite explanations and a single pill hides which. The
// want is the type's level and cover, its level and the processes driving it beside it.
//
// HIDDEN when there is nothing to say: no tick for this group yet, or it is
// not a kanban. Read for a viewer with no session too: the gate removes a
// button, not a screen.
//
// "WAITING ON" PRINTS THE ASK'S OWN QUEUE SENTENCE (the dispatcher's words for
// an operator) and keeps the engineer's queue cause in the title, then why nothing
// more was asked (held_by), beside the gap it explains, then why a cart above its want
// is waiting (surplus_cause). No label map: an ask with no sentence prints its cause as
// itself.
var _keeperFor = '';

export function renderKeeperLevels(groupName, nodeID) {
  var box = document.getElementById('keeper-levels');
  var body = document.getElementById('keeper-levels-body');
  var note = document.getElementById('keeper-levels-note');
  var families = document.getElementById('keeper-families-body');
  var familyNote = document.getElementById('keeper-families-note');
  if (!box || !body) return;
  box.classList.add('hide');
  body.innerHTML = '';
  if (note) note.innerHTML = '';
  if (families) families.innerHTML = '';
  if (familyNote) familyNote.innerHTML = '';
  _keeperFor = groupName || '';
  if (!groupName) return;
  apiGet('/api/nodes/kanban/levels').then(function (all) {
    if (_keeperFor !== groupName) return; // the modal moved on
    var rows = (Array.isArray(all) ? all : []).filter(function (r) { return r.group_node === groupName; });
    if (!rows.length) return;
    rows.sort(function (a, b) {
      return (b.gap || 0) - (a.gap || 0) || String(a.bin_type_code).localeCompare(String(b.bin_type_code));
    });
    body.innerHTML = rows.map(keeperRowHtml).join('');
    if (note) note.innerHTML = keeperNoteHtml(rows);
    box.classList.remove('hide');
    if (nodeID) renderKanbanFamilies(groupName, nodeID);
  }).catch(function (err) {
    // A failed read leaves the block hidden: the level editor above still works.
    console.warn('keeper levels', err);
  });
}

// renderKanbanFamilies draws the family table beside the type table: the keeper's last tick
// for where the carts are, the page's read for pool, minimum and room, joined on the family.
function renderKanbanFamilies(groupName, nodeID) {
  var body = document.getElementById('keeper-families-body');
  var note = document.getElementById('keeper-families-note');
  if (!body) return;
  apiGet('/api/nodes/kanban/families?group_node_id=' + nodeID).then(function (res) {
    if (_keeperFor !== groupName) return; // the modal moved on
    var rows = mergeFamilies((res && res.families) || [], (res && res.found) || []);
    if (_kanban && _kanban.nodeID === Number(nodeID)) {
      rows.forEach(function (r) {
        _kanban.pools[r.family_id] = r.pool;
        _kanban.savedPools[r.family_id] = r.pool;
      });
    }
    var editable = !!(_kanban && _kanban.nodeID === Number(nodeID));
    body.innerHTML = rows.map(function (r) { return familyRowHtml(r, editable); }).join('');
    if (note) note.innerHTML = familyNoteHtml(rows);
  }).catch(function (err) {
    // The type table above stands without it.
    console.warn('kanban families', err);
  });
}

// mergeFamilies joins the two reads on the family: rows from the save's arithmetic, and found
// from the tick. A family standing in the loop that the kanban does not level comes from the
// tick alone, with a pool of 0: every cart of it is a stranger.
export function mergeFamilies(rows, found) {
  var out = [];
  var seen = {};
  rows.forEach(function (r) {
    var f = found.find(function (x) { return x.family_id === r.family_id; }) || {};
    seen[r.family_id] = true;
    out.push(Object.assign({}, f, r));
  });
  found.forEach(function (f) {
    if (!seen[f.family_id]) out.push(Object.assign({ room: 0, minimum: 0, pool: 0 }, f));
  });
  return out;
}

export function familyRowHtml(r, editable) {
  var pool = editable
    ? '<input type="number" class="form-input loader-mix-want" min="0" value="' + (r.pool || 0) + '"'
      + ' aria-label="Pool of ' + escapeHtml(r.family || '') + '"'
      + ' data-action-change="setKanbanPool" data-family-id="' + Number(r.family_id) + '">'
    : String(r.pool || 0);
  var found = r.found || 0;
  var foundCell = found === (r.pool || 0)
    ? '<td class="rh-num">' + found + '</td>'
    : '<td class="rh-num kanban-short">' + found + '</td>';
  return '<tr>'
    + '<td>' + escapeHtml(r.family || '') + '</td>'
    + '<td class="rh-num">' + pool + '</td>'
    + '<td class="rh-num">' + (r.minimum || 0) + '</td>'
    + foundCell
    + '<td class="rh-num">' + (r.slot || 0) + '</td>'
    + '<td class="rh-num">' + (r.line || 0) + '</td>'
    + '<td class="rh-num">' + (r.window || 0) + '</td>'
    + '<td class="rh-num">' + (r.flight || 0) + '</td>'
    + '<td class="rh-num">' + (r.room || 0) + '</td>'
    + '<td class="rh-num">' + (r.floaters || 0) + '</td>'
    + '</tr>';
}

// familyNoteHtml is what the numbers cannot say: which cart to send home, what has left the loop,
// what a return will not fit, and what the plant accepted by standing at its minimum.
export function familyNoteHtml(rows) {
  var lines = [];
  rows.forEach(function (r) {
    var name = escapeHtml(r.family || '');
    (r.strangers || []).forEach(function (s) { lines.push(escapeHtml(s) + ': past the pool of ' + name); });
    if (r.lost) lines.push(r.lost + ' of ' + name + ' left the loop');
    if (r.unowned) lines.push(r.unowned + ' of ' + name + ' in flight, unowned');
    if (r.cannot_cover) lines.push(r.cannot_cover + ' of ' + name + ' cannot be covered: ' + (r.pool || 0) + ' carts on ' + (r.room || 0) + ' slots');
    if (r.pool < r.minimum) lines.push(name + ' is under its minimum: the line eats the mix');
    if (!r.pool_declared && r.pool) lines.push(name + ' stands at its minimum: nobody has typed a pool');
    if (r.dips) lines.push(name + ': running level dips by ' + r.dips + ' during unloads');
  });
  return lines.map(function (l) { return '<div>' + l + '</div>'; }).join('');
}

export function keeperRowHtml(r) {
  var gap = r.gap || 0;
  // Short is the only coloured number on the row. A gap of 0 or less is not
  // "good" needing a green tick, it is nothing happening.
  var shortTitle = r.held_by
    ? 'Core is short ' + gap + ' and is held: ' + escapeHtml(r.held_by)
    : 'Core is short ' + gap + ' and has asks out or is placing them';
  var gapCell = gap > 0
    ? '<td class="rh-num kanban-short" title="' + shortTitle + '">' + gap + '</td>'
    : '<td class="rh-num text-muted" title="the level is covered">0</td>';
  var parked = r.oldest_ask_cause
    ? '<span class="tnum" title="the longest-waiting ask against this level has been parked '
      + escapeHtml(r.oldest_ask_age || '') + ' (' + escapeHtml(r.oldest_ask_cause) + ')">'
      + escapeHtml(r.oldest_ask_reason || r.oldest_ask_cause) + ' &middot; ' + escapeHtml(r.oldest_ask_age || '') + '</span>'
    : '';
  var held = r.held_by ? '<span>' + escapeHtml(r.held_by) + '</span>' : '';
  var surplus = r.surplus_cause ? '<span>' + escapeHtml(r.surplus_cause) + '</span>' : '';
  var says = [parked, held, surplus].filter(Boolean).join('<br>');
  var waiting = says
    || '<span class="text-muted" title="nothing parked — every ask this level made is moving">&mdash;</span>';
  var driving = (r.running_processes || []).length
    ? escapeHtml(r.running_processes.join(', '))
    : '<span class="text-muted">&mdash;</span>';
  return '<tr>'
    + '<td>' + escapeHtml(r.bin_type_code || '') + '</td>'
    + '<td class="rh-num">' + (r.want || 0) + '</td>'
    + '<td class="rh-num">' + (r.base || 0) + '</td>'
    + '<td>' + driving + '</td>'
    + '<td class="rh-num">' + (r.resident || 0) + '</td>'
    + '<td class="rh-num">' + (r.held || 0) + '</td>'
    + '<td class="rh-num">' + (r.asked || 0) + '</td>'
    + '<td class="rh-num">' + (r.coming || 0) + '</td>'
    + '<td class="rh-num">' + (r.lit || 0) + '</td>'
    + gapCell
    + '<td>' + waiting + '</td>'
    + '</tr>';
}

// keeperNoteHtml is what is true of the kanban and of no one row: the processes the want cannot
// read a type for. Every row carries the same values.
export function keeperNoteHtml(rows) {
  var r = rows[0];
  if (!r) return '';
  var lines = [];
  if ((r.nothing_processes || []).length) {
    lines.push(escapeHtml(r.nothing_processes.join(', ')) + ' runs a part with no one carrier type, which counts for nothing');
  }
  if ((r.fallback_processes || []).length) {
    lines.push(escapeHtml(r.fallback_processes.join(', ')) + ' is changing over to a style with no carrier type, so it counts on its active style');
  }
  return lines.map(function (l) { return '<div>' + l + '</div>'; }).join('');
}

// setKanbanPool stages a family's pool. Flushed on Save, as the rest of the section is.
export function setKanbanPool(el) {
  if (!_kanban) return;
  var pool = Number(el.value);
  if (!(pool >= 0) || Math.floor(pool) !== pool) {
    el.value = String(_kanban.pools[el.dataset.familyId] || 0);
    return;
  }
  _kanban.pools[el.dataset.familyId] = pool;
}
