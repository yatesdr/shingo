import { apiGet, apiPost, delegateActions, escapeHtml, toast, uiConfirm } from '/static/app.js';

// Maintained-group section of the node group settings modal.
//
// A maintained group is a node group whose EMPTY-CARRIER level Core holds: so
// many unclaimed carriers of each declared type, at all times, near the
// equipment that consumes them. This module owns the section's whole lifecycle —
// load, render, stage, save.
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
var _mg = null;

function blank(nodeID) {
  return {
    nodeID: nodeID,
    // levels: [{bin_type_id, bin_type_code, want}] as edited.
    levels: [],
    // savedLevels: what the server had, for the remove-diff on save.
    savedLevels: [],
    // processes: [{process_id, nodes:[{id,name}]}] — the picker's contents.
    processes: [],
    // selected: Set of process_id.
    selected: new Set(),
    // orphans: stored support node ids that belong to no listed process. Kept
    // and re-saved rather than dropped: they usually mean the claim topology
    // moved after somebody saved, and silently deleting a supported position is
    // not a thing a render should do.
    orphans: []
  };
}

// renderMaintainSection loads and draws the section, or hides it.
//
// Hidden for anything that is not a node group. It is also hidden for an
// unauthenticated viewer, because the whole section lives inside the auth-only
// half of the modal and does not exist in the read-only one.
export function renderMaintainSection(isGroupType, nodeID) {
  var box = document.getElementById('nf-maintain');
  if (!box) return;
  _mg = null;
  if (!isGroupType || !nodeID) {
    box.classList.add('hide');
    setAlgoNote(false);
    return;
  }
  box.classList.remove('hide');
  _mg = blank(Number(nodeID));

  Promise.all([
    apiGet('/api/nodes/maintained-group?id=' + nodeID),
    // The picker's contents are a plant-wide fact, not a per-group one, but it
    // is fetched per open rather than once: a claim set that changed while the
    // page was left open is exactly the case where a cached list offers a
    // process that no longer exists.
    apiGet('/api/nodes/process-options').catch(function (err) {
      // A missing claim mirror must not stop the rest of the section from
      // rendering — the level is editable without it.
      console.warn('maintained group: process options', err);
      return { processes: [] };
    })
  ]).then(function (res) {
    var cfg = res[0] || {};
    var opts = (res[1] && res[1].processes) || [];
    if (!_mg || _mg.nodeID !== Number(nodeID)) return; // modal moved on
    _mg.levels = (cfg.levels || []).map(function (l) {
      return { bin_type_id: Number(l.bin_type_id), bin_type_code: l.bin_type_code, want: Number(l.want) };
    });
    _mg.savedLevels = _mg.levels.map(function (l) { return { bin_type_id: l.bin_type_id, want: l.want }; });
    _mg.processes = opts;

    var storedNodes = new Set((cfg.supports || []).map(function (s) { return Number(s.process_node_id); }));
    var claimed = new Set();
    opts.forEach(function (p) {
      var ids = p.nodes.map(function (n) { return Number(n.id); });
      ids.forEach(function (id) { claimed.add(id); });
      // A process counts as selected when every position it names is stored.
      // Partial overlap reads as not-selected on purpose: ticking the box would
      // claim the operator asked for positions they never saw.
      if (ids.length && ids.every(function (id) { return storedNodes.has(id); })) {
        _mg.selected.add(p.process_id);
      }
    });
    _mg.orphans = Array.from(storedNodes).filter(function (id) { return !claimed.has(id); });

    document.getElementById('nf-maintain-enabled').checked = !!cfg.maintain_enabled;
    document.getElementById('nf-maintain-strict').checked = !!cfg.strict_sourcing;
    fillStations(cfg.maintenance_station || '');
    fillOverflow(cfg.overflow_destination || '');
    renderLevels();
    renderSupports();
    setAlgoNote(!!cfg.maintain_enabled);
  }).catch(function (err) {
    console.error('renderMaintainSection', err);
    // Loud, and the section stays hidden. A section rendered from a failed fetch
    // shows an empty level an operator can save, which turns a read error into a
    // configuration change.
    box.classList.add('hide');
    _mg = null;
    toast('Could not load the maintained-group settings for this group.', 'error');
  });
}

// setAlgoNote annotates the ASRS/algorithm controls instead of hiding them. They
// still decide everything about LOADED carriers here; what a maintained level
// takes over is the empty pull. Saying which half stopped applying is
// information — a control that vanished would just be a question.
function setAlgoNote(on) {
  var note = document.getElementById('nf-maintain-algo-note');
  if (note) note.classList.toggle('hide', !on);
}

export function onMaintainToggle() {
  var box = document.getElementById('nf-maintain-enabled');
  setAlgoNote(!!(box && box.checked));
}

function fillStations(current) {
  var sel = document.getElementById('nf-maintain-station');
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
  var sel = document.getElementById('nf-maintain-overflow');
  if (!sel) return;
  var html = '<option value="">— none —</option>';
  // Groups come off the tiles already on the page. NGRP is the current code;
  // SMKT and SUP are legacy codes still on un-migrated databases and are matched
  // the same way everywhere else in this modal.
  var seen = {};
  document.querySelectorAll('.node-tile').forEach(function (t) {
    var code = t.dataset.typeCode;
    if (['NGRP', 'SMKT', 'SUP'].indexOf(code) < 0) return;
    var name = t.dataset.name;
    if (!name || seen[name]) return;
    if (_mg && String(t.dataset.id) === String(_mg.nodeID)) return; // never itself
    seen[name] = true;
    html += '<option value="' + escapeHtml(name) + '">' + escapeHtml(name) + '</option>';
  });
  sel.innerHTML = html;
  if (current && !seen[current]) {
    sel.insertAdjacentHTML('beforeend',
      '<option value="' + escapeHtml(current) + '">' + escapeHtml(current) + ' (not on this page)</option>');
  }
  sel.value = current;
}

// renderLevels draws the declared level, one row per carrier type, plus the add
// row. Same controls and the same vocabulary as the loaders page's carrier mix —
// but this section is a "maintained level", and the two are not the same
// promise: a mix is a preference bounded by the loader's windows, a level is the
// number Core holds.
function renderLevels() {
  var host = document.getElementById('nf-maintain-levels');
  if (!host || !_mg) return;
  var declared = {};
  _mg.levels.forEach(function (l) { declared[l.bin_type_id] = true; });

  var rows = _mg.levels.map(function (l) {
    return '<div class="loader-mix-line">'
      + '<span class="loader-chip">' + escapeHtml(l.bin_type_code) + '</span>'
      + '<input type="number" class="form-input loader-mix-want" min="0" value="' + Number(l.want) + '"'
      + ' aria-label="How many ' + escapeHtml(l.bin_type_code) + ' to keep on hand"'
      + ' data-action-change="setMaintainWant" data-bin-type-id="' + l.bin_type_id + '">'
      + '<button type="button" class="btn btn-sm" title="Remove"'
      + ' data-action="removeMaintainLevel" data-bin-type-id="' + l.bin_type_id + '">&times;</button>'
      + '</div>';
  }).join('');

  var rest = _allBinTypes.filter(function (t) { return !declared[Number(t.id)]; });
  var add = '';
  if (rest.length) {
    add = '<div class="loader-mix-add">'
      + '<select id="nf-maintain-add-type" class="form-input" aria-label="Carrier type">'
      + rest.map(function (t) {
        return '<option value="' + Number(t.id) + '">' + escapeHtml(t.code || t.label || '') + '</option>';
      }).join('')
      + '</select>'
      + '<input type="number" id="nf-maintain-add-want" class="form-input loader-mix-want" min="0" value="1" aria-label="How many">'
      + '<button type="button" class="btn btn-sm" data-action="addMaintainLevel">Add</button>'
      + '</div>';
  }
  host.innerHTML = (rows || '<div class="text-muted" style="font-size:0.75rem">No level declared.</div>') + add;
}

export function addMaintainLevel() {
  if (!_mg) return;
  var sel = document.getElementById('nf-maintain-add-type');
  var want = document.getElementById('nf-maintain-add-want');
  var id = Number(sel && sel.value);
  if (!id) return;
  var n = Number(want && want.value);
  if (!(n >= 0)) n = 0;
  var t = _allBinTypes.find(function (x) { return Number(x.id) === id; });
  _mg.levels.push({ bin_type_id: id, bin_type_code: (t && (t.code || t.label)) || String(id), want: n });
  _mg.levels.sort(function (a, b) { return a.bin_type_code.localeCompare(b.bin_type_code); });
  renderLevels();
}

export function setMaintainWant(el) {
  if (!_mg) return;
  var id = Number(el.getAttribute('data-bin-type-id'));
  var n = Number(el.value);
  // Zero is a real declared value — "none of this type, on purpose" — so it is
  // kept rather than treated as a removal. Negative is not a smaller level, it
  // is a number nothing can act on.
  if (!(n >= 0)) { n = 0; el.value = '0'; }
  _mg.levels.forEach(function (l) { if (l.bin_type_id === id) l.want = n; });
}

export function removeMaintainLevel(el) {
  if (!_mg) return;
  var id = Number(el.getAttribute('data-bin-type-id'));
  _mg.levels = _mg.levels.filter(function (l) { return l.bin_type_id !== id; });
  renderLevels();
}

// renderSupports lists PROCESSES, with the positions each one resolves to shown
// underneath. The operator picks a process because that is the thing they can
// point at on the floor; what gets stored is the positions, because a claim
// lives on the Edge and Core cannot read one when it has to decide anything.
function renderSupports() {
  var host = document.getElementById('nf-maintain-supports');
  if (!host || !_mg) return;
  if (!_mg.processes.length) {
    host.innerHTML = '<div class="text-muted" style="font-size:0.75rem">'
      + 'No processes have reported claims yet.</div>';
    return;
  }
  var html = _mg.processes.map(function (p) {
    var names = p.nodes.map(function (n) { return n.name; }).join(', ');
    return '<label class="text-sm" style="display:flex;align-items:flex-start;gap:8px;margin-bottom:6px;cursor:pointer">'
      + '<input type="checkbox" data-action-change="toggleMaintainSupport"'
      + ' data-process-id="' + escapeHtml(p.process_id) + '"'
      + (_mg.selected.has(p.process_id) ? ' checked' : '') + '>'
      + '<span><strong>' + escapeHtml(p.process_id) + '</strong>'
      + '<span class="text-muted" style="display:block;font-size:0.72rem">' + escapeHtml(names) + '</span>'
      + '</span></label>';
  }).join('');
  if (_mg.orphans.length) {
    html += '<div class="text-muted" style="font-size:0.72rem;margin-top:6px">'
      + _mg.orphans.length + ' saved position'
      + (_mg.orphans.length === 1 ? ' matches' : 's match')
      + ' no known process. Kept as-is — clear them in the claims.</div>';
  }
  host.innerHTML = html;
}

export function toggleMaintainSupport(el) {
  if (!_mg) return;
  var pid = el.getAttribute('data-process-id');
  if (el.checked) _mg.selected.add(pid);
  else _mg.selected.delete(pid);
}

// rawPost is apiPost without the envelope-unwrapping.
//
// app.js's api() throws the server's `error` STRING on a non-2xx, which is the
// right shape almost everywhere and the wrong one here: the holds-bins refusal
// carries `needs_force` and `drain` alongside the message, and unwrapping to the
// string throws both away. Ten lines of fetch rather than widening a helper the
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
// Bins set at a maintained group, and returns whether to go ahead.
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
    res = await apiPost('/api/nodes/maintained-group/check-types',
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

// saveMaintainedGroup flushes the section. Called from the modal's Save, before
// the form posts.
//
// Scalars first, then the level, then the supports — and the order matters once
// the save-time rules land: a refusal names the setting it refused, and an
// operator reading "this group is already a loader's staging group" wants it
// against the switch they just flipped, not against a level row they did not
// touch.
export async function saveMaintainedGroup() {
  var box = document.getElementById('nf-maintain');
  if (!box || box.classList.contains('hide') || !_mg) return;

  var nodeID = _mg.nodeID;
  var enabled = document.getElementById('nf-maintain-enabled').checked;
  var strict = document.getElementById('nf-maintain-strict').checked;
  var station = document.getElementById('nf-maintain-station').value;
  var overflow = document.getElementById('nf-maintain-overflow').value;

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
    collect(await postConfirmable('/api/nodes/maintained-group/settings', {
      group_node_id: nodeID,
      maintain_enabled: enabled,
      strict_sourcing: strict,
      maintenance_station: station,
      overflow_destination: overflow
    }));

    // Removals come off the DIFF against what the server had. Deleting every row
    // and re-adding would be simpler and would also, for a moment, leave a
    // maintained group with no declared level at all.
    var still = {};
    _mg.levels.forEach(function (l) { still[l.bin_type_id] = true; });
    for (var i = 0; i < _mg.savedLevels.length; i++) {
      var was = _mg.savedLevels[i];
      if (!still[was.bin_type_id]) {
        collect(await apiPost('/api/nodes/maintained-group/level/remove',
          { group_node_id: nodeID, bin_type_id: was.bin_type_id }));
      }
    }
    for (var j = 0; j < _mg.levels.length; j++) {
      var l = _mg.levels[j];
      collect(await apiPost('/api/nodes/maintained-group/level',
        { group_node_id: nodeID, bin_type_id: l.bin_type_id, want: l.want }));
    }

    var nodeIDs = _mg.orphans.slice();
    _mg.processes.forEach(function (p) {
      if (!_mg.selected.has(p.process_id)) return;
      p.nodes.forEach(function (n) {
        if (nodeIDs.indexOf(Number(n.id)) < 0) nodeIDs.push(Number(n.id));
      });
    });
    collect(await postConfirmable('/api/nodes/maintained-group/supports',
      { group_node_id: nodeID, process_node_ids: nodeIDs }));

    // The staged state now matches the server, so a second Save without
    // reopening does not re-issue removals for rows already gone.
    _mg.savedLevels = _mg.levels.map(function (x) { return { bin_type_id: x.bin_type_id, want: x.want }; });

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
    // A refusal (409) arrives as a rejected request whose value IS the server's
    // error string — api() unwraps the JSON envelope before throwing. Shown
    // as-is rather than wrapped, because the string already names the setting:
    // "GRP-A is already the staging group for loader CT-L" is the whole message.
    toast('' + err, 'error', { sticky: true });
  }
}

delegateActions(document.body, {
  addMaintainLevel,
  onMaintainToggle,
  removeMaintainLevel,
  setMaintainWant,
  toggleMaintainSupport
}, { events: ['click', 'change'] });
