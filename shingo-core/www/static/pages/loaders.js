import { apiGet, apiPost, delegateActions, escapeHtml, toast, uiConfirm } from '/static/app.js';

// Core-owned bin loaders, rendered as drag-and-drop containers on the Nodes grid
// — the same mental model as node groups/lanes (nodes-supermarket.js). The modal
// only CREATES a loader (name/role/layout/inbound/outbound); membership is edited
// on the grid: drag a node tile into a dedicated loader to add a position, ⠿-drag
// to reorder (persisted via sort_order), × to remove. shared_window loaders hold a
// payload set instead of nodes (chips). Per-payload UoP thresholds + the lead-time
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

/* ── The loader form ──────────────────────────────────────────────────────
   Written to the form-state convention in docs/ui-style-guide.md: the state
   lives in ONE object, what is on screen is DERIVED from that state, and the
   rules are pure functions of it. What this replaced read values back off the
   DOM in five places and set element.style.display from event handlers — the
   two anti-patterns that section of the guide names by name.
*/

function val(id) { const e = document.getElementById(id); return e ? (e.value || '').trim() : ''; }
function result(msg, isErr) {
  const e = document.getElementById('loader-result');
  if (!e) return;
  e.textContent = msg || '';
  e.style.color = isErr ? 'var(--danger)' : 'var(--success)';
}

// setVal skips a write that would not change anything. Re-rendering the form on
// every change would otherwise reassign a text input's value while it is being
// typed into, which moves the caret to the end.
function setVal(id, v) {
  const e = document.getElementById(id);
  if (e && e.value !== v) e.value = v;
}
function checked(id) { const e = document.getElementById(id); return !!(e && e.checked); }
function setChecked(id, c) { const e = document.getElementById(id); if (e) e.checked = !!c; }
function setText(id, t) { const e = document.getElementById(id); if (e) e.textContent = t; }
function setDisabled(id, d) { const e = document.getElementById(id); if (e) e.disabled = d; }

function setShown(id, show) {
  const e = document.getElementById(id);
  if (e && e.classList) e.classList.toggle('is-hidden', !show);
}

// KIND is what an operator actually picks, and it maps onto two stored fields.
// The form used to ask for the layout AND a "one window at a time" checkbox, so
// the three things a person thinks in terms of were spread across two controls,
// one of which stated a restriction rather than a choice.
function kindToLayout(kind) {
  return kind === 'dedicated' ? 'dedicated_positions' : 'shared_window';
}
function kindFromLoader(l) {
  if (l.layout === 'dedicated_positions') return 'dedicated';
  return l.funnel_windows ? 'single_window' : 'multi_window';
}

// formState is the loader currently in the modal. id 0 means "not saved yet".
let formState = blankForm();

function blankForm() {
  return {
    id: 0,
    name: '',
    role: 'produce',       // produce | consume
    kind: 'multi_window',  // multi_window | single_window | dedicated
    changeoverLoadDirective: false,
    replenishment: 'operator',
    fedByHand: false,
    inbound: '',
    outbound: '',
  };
}

// readForm snapshots the controls into a state object. Nothing else reads the
// DOM for a value.
function readForm() {
  return {
    id: Number(val('loader-edit-id') || 0),
    name: val('loader-name'),
    role: val('loader-role') || 'produce',
    kind: val('loader-kind') || 'multi_window',
    replenishment: val('loader-replenishment') || 'operator',
    fedByHand: checked('loader-fed-by-hand'),
    changeoverLoadDirective: checked('loader-changeover-directive'),
    inbound: val('loader-inbound'),
    outbound: val('loader-outbound'),
  };
}

// normalizeForm folds in the choices that IMPLY another value, so the screen and
// what gets saved cannot disagree. There is exactly one: ticking "fed by hand"
// IS the operator saying there is no source, so the source is cleared and not
// merely hidden. Every OTHER hidden field keeps its value — the save path writes
// all of them, so a gate that blanked one would drop a plant's configuration on
// the next save without saying so.
function normalizeForm(state) {
  if (state.fedByHand) state.inbound = '';
  return state;
}

// formShape decides WHAT IS ON THE SCREEN, from state alone. Nothing that does
// not apply to the chosen loader is rendered at all.
//
// It used to render everything and then grey out the parts that did not apply,
// with a paragraph beside each explaining why it was greyed out. That is where
// the form's nine blocks of prose came from: they were not documentation, they
// were apologies for showing a control that could not be used. A field that is
// absent needs no explanation.
//
// The rules, and each one removes a paragraph that used to be on screen:
//
//   - An UNLOADER has exactly one mode — it drains when the operator clears a
//     window — so there is no supply question to ask.
//   - FED BY HAND means no robot pulls anything, so there is no source to name.
//   - The CARRIER MIX and the per-window capability are properties of a window
//     SET; a dedicated loader is already one part per spot. Both are edited
//     against a saved loader, so they wait for one.
function formShape(state) {
  const dedicated = state.kind === 'dedicated';
  const saved = state.id !== 0;
  return {
    supply: state.role !== 'consume',
    inbound: !state.fedByHand,
    // Outbound is asked of EVERY loader. It used to be hidden on a dedicated
    // loader, on the reading that each spot is its own outbound — but a
    // dedicated loader still has one place its filled carriers go, and hiding
    // the field made "an inbound group and an outbound group" unenterable on
    // exactly the layout that wants it.
    outbound: true,
    mix: !dedicated && saved,
    windows: !dedicated && saved,
  };
}

// validateForm is a pure function of state — no DOM reads — so it can be
// tested. The backend checks the same rules; this one is for immediate feedback.
function validateForm(state) {
  const errors = [];
  if (!state.name) errors.push({ field: 'loader-name', msg: 'Name is required' });
  return { ok: errors.length === 0, errors };
}

// renderForm writes state back to the controls and applies the shape.
function renderForm(state) {
  setVal('loader-edit-id', state.id ? String(state.id) : '');
  setVal('loader-name', state.name);
  setVal('loader-role', state.role);
  setVal('loader-kind', state.kind);
  setChecked('loader-fed-by-hand', state.fedByHand);
  setChecked('loader-changeover-directive', state.changeoverLoadDirective);
  setVal('loader-inbound', state.inbound);
  setVal('loader-outbound', state.outbound);
  setReplenishmentOptions(state);

  const shape = formShape(state);
  setShown('loader-supply-row', shape.supply);
  setShown('loader-inbound', shape.inbound);
  setShown('loader-outbound', shape.outbound);
  setShown('loader-mix-row', shape.mix);
  setShown('loader-windows-row', shape.windows);
  if (shape.mix) renderMixEditor(state.id);
  if (shape.windows) renderWindowCapEditor(state.id);
}

// applyLoaderForm is the single path every control change takes: snapshot, fold
// in the implied values, re-render the whole form. One path, so changing any
// control re-decides the entire form rather than patching the part beside it.
function applyLoaderForm() {
  formState = normalizeForm(readForm());
  renderForm(formState);
}

// renderMixEditor draws the declared carrier mix: how many of each carrier type
// this loader wants on hand. Empty is the normal state and means "take whatever
// is available".
function renderMixEditor(loaderID) {
  const host = document.getElementById('loader-mix-editor');
  if (!host) return;
  const item = loaderItem(loaderID);
  const mix = (item && item.quota) || [];
  const declared = mix.map(function (q) { return q.bin_type_code; });
  const rows = mix.map(function (q) {
    return '<div class="loader-mix-line">'
      + '<span class="loader-chip">' + escapeHtml(q.bin_type_code) + '</span>'
      + '<input type="number" class="form-input loader-mix-want" min="0" value="' + Number(q.want) + '"'
      + ' aria-label="How many ' + escapeHtml(q.bin_type_code) + ' to keep on hand"'
      + ' data-action-change="setLoaderQuota" data-bin-type="' + escapeHtml(q.bin_type_code) + '">'
      + '<button class="btn btn-sm" title="Remove" data-action="removeLoaderQuota" data-bin-type="'
      + escapeHtml(q.bin_type_code) + '">×</button>'
      + '</div>';
  }).join('');
  const rest = binTypeOptions(declared);
  const add = rest
    ? '<div class="loader-mix-add">'
      + '<select id="loader-mix-add-type" class="form-input" aria-label="Carrier type">' + rest + '</select>'
      + '<input type="number" id="loader-mix-add-want" class="form-input loader-mix-want" min="1" value="1" aria-label="How many">'
      + '<button class="btn btn-sm" data-action="addLoaderQuota">Add</button></div>'
    : '';
  host.innerHTML = rows + add;
}

// renderWindowCapEditor draws one row per window: what that window can
// physically take. It sits beside the carrier mix because the two answer
// adjacent questions — the mix is what the LOADER wants on hand, the capability
// is what each SLOT can hold — and an operator setting one usually means the
// other.
//
// Rows come out in the arranged order, the same order that decides which window
// fills first, so both readings of "the first window" agree on one screen.
//
// A window with nothing set takes anything, and that has to stay the meaning of
// empty: every window at every plant is empty today, and the other reading would
// have all of them suddenly accept nothing.
function renderWindowCapEditor(loaderID) {
  const host = document.getElementById('loader-windows-editor');
  if (!host) return;
  const item = loaderItem(loaderID);
  const homes = ((item && item.homes) || []).slice().sort(function (a, b) {
    return (a.sort_order || 0) - (b.sort_order || 0);
  });
  if (!homes.length) {
    host.innerHTML = '<div class="loader-window-cap-empty">No windows yet — '
      + 'drag node tiles into this loader on the grid.</div>';
    return;
  }
  const caps = (item && item.window_bin_types) || {};
  host.innerHTML = homes.map(function (h) {
    const nodeID = Number(h.position_node_id);
    const name = nodesById[nodeID] || ('node ' + nodeID);
    const set = caps[nodeID] || [];
    const chips = set.map(function (code) {
      return '<span class="loader-chip">' + escapeHtml(code)
        + '<span class="loader-chip-x" title="Remove" data-action="removeWindowBinType"'
        + ' data-node-id="' + nodeID + '" data-bin-type="' + escapeHtml(code) + '">×</span></span>';
    }).join('');
    const rest = binTypeOptions(set);
    const add = rest
      ? '<select class="form-input" data-action-change="addWindowBinType" data-node-id="' + nodeID + '"'
        + ' aria-label="Add a carrier type ' + escapeHtml(name) + ' can take">'
        + '<option value="">+ type</option>' + rest + '</select>'
      : '';
    return '<div class="loader-window-cap">'
      + '<span class="loader-window-cap-name">' + escapeHtml(name) + '</span>'
      + (chips || '<span class="loader-window-cap-any">takes anything</span>')
      + add + '</div>';
  }).join('');
}

// binTypeCatalog is the carrier-type list, fetched once — the pickers need
// codes to show and ids to save.
let binTypeCatalog = [];

function loadBinTypeCatalog() {
  return apiGet('/api/bin-types').then(function (d) {
    binTypeCatalog = (d && d.bin_types) || [];
  }).catch(function () { binTypeCatalog = []; });
}

// binTypeOptions lists the carrier catalogue minus what is already set. An "add"
// control should only offer what can actually be added; when that leaves nothing
// the caller drops the control rather than showing an empty one.
function binTypeOptions(exclude) {
  const taken = {};
  (exclude || []).forEach(function (c) { taken[c] = true; });
  return binTypeCatalog.filter(function (t) { return !taken[t.code]; })
    .map(function (t) {
      return '<option value="' + Number(t.id) + '">' + escapeHtml(t.code) + '</option>';
    }).join('');
}

function loaderItem(loaderID) {
  const id = Number(loaderID || 0);
  if (!id) return null;
  return loaderData.find(function (x) { return Number(x.loader.id) === id; }) || null;
}

function quotaFor(el) {
  return {
    loaderID: formState.id,
    code: el.getAttribute('data-bin-type') || '',
  };
}

// addWindowBinType / removeWindowBinType edit ONE window's capability. The API
// replaces the whole set, so both compute the new set from what is on screen and
// send that.
function windowCapSet(nodeID) {
  const item = loaderItem(formState.id);
  const caps = (item && item.window_bin_types) || {};
  return (caps[Number(nodeID)] || []).slice();
}

function saveWindowCap(nodeID, codes) {
  const ids = codes.map(binTypeIDForCode).filter(function (n) { return n > 0; });
  return apiPost('/api/loader/set-window-bin-types', {
    loader_id: formState.id, position_node_id: Number(nodeID), bin_type_ids: ids,
  }).then(function (d) {
    if (d && d.error) { result(d.error, true); return; }
    return refresh().then(function () { renderWindowCapEditor(formState.id); });
  }).catch(function (e) { result('' + e, true); });
}

function addWindowBinType(el) {
  const nodeID = Number(el.getAttribute('data-node-id') || 0);
  const binTypeID = Number(el.value || 0);
  if (!formState.id || !nodeID || !binTypeID) return;
  const t = binTypeCatalog.find(function (x) { return Number(x.id) === binTypeID; });
  if (!t) return;
  saveWindowCap(nodeID, windowCapSet(nodeID).concat([t.code]));
}

function removeWindowBinType(el) {
  const nodeID = Number(el.getAttribute('data-node-id') || 0);
  const code = el.getAttribute('data-bin-type') || '';
  if (!formState.id || !nodeID || !code) return;
  saveWindowCap(nodeID, windowCapSet(nodeID).filter(function (c) { return c !== code; }));
}

function binTypeIDForCode(code) {
  const t = binTypeCatalog.find(function (x) { return x.code === code; });
  return t ? Number(t.id) : 0;
}

function addLoaderQuota() {
  const loaderID = formState.id;
  const binTypeID = Number(val('loader-mix-add-type') || 0);
  const want = Number(val('loader-mix-add-want') || 0);
  if (!loaderID || !binTypeID || want < 1) return;
  apiPost('/api/loader/set-quota', { loader_id: loaderID, bin_type_id: binTypeID, want: want })
    .then(function (d) {
      if (d && d.error) { result(d.error, true); return; }
      refresh().then(function () { renderMixEditor(loaderID); });
    }).catch(function (e) { result('' + e, true); });
}

function setLoaderQuota(el) {
  const q = quotaFor(el);
  const want = Number(el.value || 0);
  const binTypeID = binTypeIDForCode(q.code);
  if (!q.loaderID || !binTypeID || want < 0) return;
  apiPost('/api/loader/set-quota', { loader_id: q.loaderID, bin_type_id: binTypeID, want: want })
    .then(function (d) { if (d && d.error) result(d.error, true); else refresh(); })
    .catch(function (e) { result('' + e, true); });
}

function removeLoaderQuota(el) {
  const q = quotaFor(el);
  const binTypeID = binTypeIDForCode(q.code);
  if (!q.loaderID || !binTypeID) return;
  apiPost('/api/loader/remove-quota', { loader_id: q.loaderID, bin_type_id: binTypeID })
    .then(function (d) {
      if (d && d.error) { result(d.error, true); return; }
      refresh().then(function () { renderMixEditor(q.loaderID); });
    }).catch(function (e) { result('' + e, true); });
}

// setReplenishmentOptions populates the replenishment <select> from state: a
// produce loader picks operator-driven vs auto/UoP-threshold; a consume loader
// (unloader) only drains. Writes loaders.Replenishment (operator | threshold).
//
// The consume list used to carry a disabled "Threshold (coming soon)" option.
// That was the ONLY thing standing between the plant and a loader that neither
// drains nor replenishes, and it was a greyed <option> in a browser — any direct
// POST walked straight past it. The service refuses the combination now
// (service.ErrConsumeThreshold, 400), so the option is gone rather than
// decorative: an unloader has one mode, and the screen says so.
function setReplenishmentOptions(state) {
  const sel = document.getElementById('loader-replenishment');
  if (!sel) return;
  const role = state.role;
  const want = state.replenishment || 'operator';
  let opts, hint;
  if (role === 'consume') {
    opts = [['operator', 'Drain — window-queue empties out as bins fill']];
    hint = 'An unloader drains: bins leave as they fill. It has no threshold mode.';
  } else {
    opts = [['operator', 'Operator-driven — operator stages from the board (no auto-fire)'],
            ['threshold', 'Auto — UoP threshold (Core auto-fires an empty when UoP drops)']];
    hint = 'Auto fires when lineside UoP drops below the per-payload threshold (set on the Inventory page); operator-driven never auto-fires.';
  }
  const valid = opts.some(function (o) { return o[0] === want; });
  const chosen = valid ? want : 'operator';
  sel.innerHTML = opts.map(function (o) {
    return '<option value="' + o[0] + '"' + (o[0] === chosen ? ' selected' : '') + '>'
      + escapeHtml(o[1]) + '</option>';
  }).join('');
  sel.value = chosen;
  const h = document.getElementById('loader-replenishment-hint');
  if (h) h.textContent = hint;
}

// replenishLabel is the short mode tag shown in a loader box header.
function replenishLabel(l) {
  if (l.replenishment === 'threshold') return l.role === 'consume' ? 'threshold' : 'auto-threshold';
  return l.role === 'consume' ? 'drain' : 'operator-driven';
}

// formStateFromLoader is the one place a stored loader becomes form state.
function formStateFromLoader(l) {
  return {
    id: Number(l.id),
    name: l.name || '',
    role: l.role || 'produce',
    kind: kindFromLoader(l),
    changeoverLoadDirective: !!l.changeover_load_directive,
    replenishment: l.replenishment || 'operator',
    // No source IS the fed-by-hand choice; that is what the stored blank means.
    fedByHand: !(l.inbound_source || ''),
    inbound: l.inbound_source || '',
    outbound: l.outbound_dest || '',
  };
}

// showLoaderModal renders the given state and opens the modal. Create and edit
// differ only in the state they hand it and in what stays locked.
function showLoaderModal(state, title, submitLabel, lockRole) {
  formState = state;
  renderForm(formState);
  setDisabled('loader-role', lockRole);
  setDisabled('loader-kind', false);
  setText('loader-modal-title', title);
  setText('loader-submit-btn', submitLabel);
  const m = document.getElementById('loader-modal');
  if (m) m.classList.add('active');
  result('');
  fillDatalists();
}

function openLoaderModal() {
  showLoaderModal(blankForm(), 'Create Loader', 'Create Loader', false);
}

// editLoader opens the modal pre-filled for an existing loader. Role is the
// identity — what the loader IS — so it is locked; change it by delete and
// recreate. Kind stays editable; submitLoader confirms and drops members first
// if changing it would orphan them, because windows and dedicated spots cannot
// carry across.
function editLoader(lid) {
  const item = loaderItem(lid);
  if (!item) return;
  showLoaderModal(formStateFromLoader(item.loader), 'Edit Loader', 'Save', true);
}

function closeLoaderModal() {
  const m = document.getElementById('loader-modal');
  if (m) m.classList.remove('active');
}

// A "clear the inbound source" button used to live here, unreferenced by any
// markup — it was the one-tap way to say "fed directly, no robot pulls". The
// "Fed by hand" checkbox is that choice now, stated as a choice and wired.

// loaderPayload is the wire shape of a state — the one place state becomes a
// request body, so create and edit cannot drift on what they send.
function loaderPayload(state) {
  return {
    name: state.name,
    layout: kindToLayout(state.kind),
    replenishment: state.replenishment,
    funnel_windows: state.kind === 'single_window',
    // Commandeer this station's card during a changeover: instead of offering
    // every payload it serves, the card names the carrier the incoming style
    // needs. Set here because it describes the station, not a style.
    changeover_load_directive: !!state.changeoverLoadDirective,
    inbound_source: state.inbound,
    outbound_dest: state.outbound,
  };
}

// submitLoader handles both create and edit — state.id decides which.
async function submitLoader() {
  const state = normalizeForm(readForm());
  formState = state;
  const v = validateForm(state);
  if (!v.ok) { result(v.errors[0].msg, true); return; }
  const body = loaderPayload(state);

  if (state.id) {
    const eitem = loaderItem(state.id);
    const homes = eitem ? (eitem.homes || []) : [];
    const pls = eitem ? (eitem.payloads || []) : [];
    const doUpdate = function () {
      result('Saving…');
      apiPost('/api/loader/update', Object.assign({ id: state.id }, body)).then(function (d) {
        if (d && d.error) { result(d.error, true); return; }
        result('Saved', false);
        refresh();
        setTimeout(closeLoaderModal, 400);
      }).catch(function (e) { result('' + e, true); });
    };
    // Changing layout on a loader that already has members would orphan them, so confirm
    // and drop them first (the operator opted in).
    if (eitem && body.layout !== eitem.loader.layout && (homes.length + pls.length) > 0) {
      if (!await uiConfirm('Changing layout to "' + body.layout + '" will drop this loader’s ' + homes.length + ' node(s) and ' + pls.length + ' payload(s). Continue?')) {
        formState = formStateFromLoader(eitem.loader);
        renderForm(formState);
        result('Cancelled — unchanged', false);
        return;
      }
      result('Dropping members…');
      Promise.all([].concat(
        homes.map(function (h) { return apiPost('/api/loader/remove-home', { loader_id: state.id, position_node_id: h.position_node_id }); }),
        pls.map(function (p) { return apiPost('/api/loader/remove-payload', { loader_id: state.id, payload_code: p.payload_code }); })
      )).then(doUpdate).catch(function (e) { result('' + e, true); });
      return;
    }
    doUpdate();
    return;
  }

  result('Creating…');
  // 1c: name the loader's outbound group INLINE — if the name isn't an existing
  // node group, create it first, then the loader references it by name. So you set
  // up the loader and its group in one flow instead of pre-making the group.
  const newGroups = [body.outbound_dest].filter(function (n) { return n && !(n in nodesByName); });
  Promise.all(newGroups.map(function (n) { return apiPost('/api/node-group/create', { name: n }); }))
    .then(function () {
      return apiPost('/api/loader/create', Object.assign({ role: state.role }, body));
    })
    .then(async function (d) {
      if (d && d.error) { result(d.error, true); return; }
      result('Created — drag node tiles into it on the grid', false);
      // Stay on the loader that was just created, as an EDIT. The carrier mix and
      // the per-window capability are edited against a saved loader, so before
      // this the form cleared itself and those two sections stayed out of reach
      // until the operator found the loader on the grid and clicked edit.
      await refresh();
      const created = loaderItem(d && d.id);
      if (created) {
        showLoaderModal(formStateFromLoader(created.loader), 'Edit Loader', 'Save', true);
        result('Created — drag node tiles into it on the grid', false);
      }
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

// markLinkedTiles mirrors each loader slot's CANONICAL grid tile state onto the slot,
// for BOTH window/position slots (.loader-member) AND output group-zone slots
// (.loader-group-slot), so a node shows the same live colour (loaded / empty / staged
// / claimed …) everywhere it appears — group or loader. The grid (group) tile itself is
// left untouched: a slot is differentiated by its teal outline, not by ringing the node.
function markLinkedTiles() {
  const STATE = ['tile-has-payload', 'tile-empty-bin', 'tile-staged', 'tile-maintenance', 'tile-claimed', 'tile-disabled', 'tile-synthetic'];
  // Walk every rendered slot tile (both kinds carry .node-tile[data-id]); scope the
  // canonical lookup to #tile-grid so it never matches a slot tile, then copy the grid
  // tile's state classes onto the slot.
  document.querySelectorAll('.loader-member[data-id], .loader-group-slot[data-id]').forEach(function (m) {
    const id = m.dataset.id;
    const grid = document.querySelector('#tile-grid .node-tile[data-id="' + id + '"]');
    STATE.forEach(function (c) { m.classList.remove(c); });
    if (grid) STATE.forEach(function (c) { if (grid.classList.contains(c)) m.classList.add(c); });
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

// groupZoneHtml renders ONE associated node group (the output market) as a labelled
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

// loaderGroupsHtml renders the loader's output market as a drag-in/out zone INSIDE
// the teal box, placed after the positions + payload set + note.
//
// There used to be a second zone here, labelled "Buffer" and then "Staging", for a
// group named on the loader row. It is gone: "Buffer" now means only one thing on
// this screen — the kept-partial SLOTS one zone further up the same box — and where
// a loader's empties come from is the inbound source, with no second answer.
//
// Shown for every layout. A dedicated loader's filled carriers go somewhere too;
// the field used to be hidden on that layout and the zone with it.
function loaderGroupsHtml(l) {
  let html = '';
  if (l.outbound_dest) html += groupZoneHtml('Output', l.outbound_dest);
  return html;
}

// thresholdGapHtml surfaces payloads a THRESHOLD loader serves that carry no UOP
// threshold — the ones nothing will ever order for.
//
// PARTIAL COVERAGE IS THE CASE THAT HIDES. A loader with thresholds on three of
// its five payloads passes every check that asks whether a threshold exists —
// one does — while the other two are ordered by nobody, silently. Saying "3 of 5
// set" is not enough either; the operator needs the count that is WRONG and
// somewhere to go and fix it.
//
// Nothing here for an operator-replenishment loader: a threshold is meaningless
// there, and flagging it would put a permanent complaint on every correctly
// configured loader.
function thresholdGapHtml(item) {
  const l = item.loader;
  if (l.replenishment !== 'threshold') return '';
  const missing = (item.payloads || []).filter(function (p) { return !(p.uop_threshold > 0); });
  const homeMissing = (item.homes || []).filter(function (h) {
    return h.payload_code && !(h.uop_threshold > 0);
  });
  const n = missing.length + homeMissing.length;
  if (n === 0) return '';
  const total = (item.payloads || []).length + (item.homes || []).filter(function (h) { return h.payload_code; }).length;
  // No threshold ANYWHERE is the louder case: nothing orders for this loader at
  // all, rather than for some of its parts.
  const none = n === total;
  const names = missing.map(function (p) { return p.payload_code; })
    .concat(homeMissing.map(function (h) { return h.payload_code; }));
  const label = none
    ? 'no threshold set — nothing will order for this loader'
    : n + ' of ' + total + ' payloads need a threshold';
  return '<a class="loader-threshold-gap' + (none ? ' loader-threshold-gap-none' : '') + '"'
    + ' href="/inventory" title="' + escapeHtml(names.join(', ')) + ' — set a UoP threshold on the Inventory page">'
    + escapeHtml(label) + '</a>';
}

function boxHtml(item) {
  const l = item.loader;
  const dedicated = l.layout === 'dedicated_positions';
  // Flow line. A dedicated loader's OUTBOUND is meaningless (its spots are its
  // own outbound) but its INBOUND is load-bearing: it is where empties are
  // retrieved from, and blank means the replenishment chain silently does
  // nothing. Suppressing the whole line for dedicated hid exactly the value an
  // engineer needs to see at a glance, so dedicated now renders
  // `inbound → (spots)`. "(spots)" rather than a dash on the right, so the
  // destination doesn't read as unset config; a dash on the LEFT is real and
  // is meant to look wrong.
  let meta = escapeHtml(l.role) + ' · ' + escapeHtml(l.layout) + ' · ' + escapeHtml(replenishLabel(l));
  let flow;
  if (dedicated && !l.outbound_dest) {
    flow = (l.inbound_source || '—') + ' → (spots)';
  } else {
    flow = (l.inbound_source || '—') + ' → ' + (l.outbound_dest || '—');
  }
  meta += ' · ' + escapeHtml(flow);
  // Member nodes are shown ONLY for dedicated-home loaders (each position is a
  // meaningful payload-pinned slot). Shared-window loaders + unloaders are defined
  // by the node GROUPS they pull from / feed — showing their individual windows is
  // noise (and confusing to other team members), so they render group zones only.
  const nodes = nodeMembersHtml(item, dedicated);
  const payloadSet = dedicated ? '' : payloadChipsHtml(item);
  const groupsHtml = loaderGroupsHtml(l);
  const hint = isAuth
    ? (dedicated
      ? '<div class="loader-hint">Drag node tiles here · ⠿ reorder · × remove · pick a payload per spot (shows as a badge). UoP threshold lives on the Inventory page.</div>'
      : '<div class="loader-hint">Shared-window loader — drag node tiles in above as its <strong>windows</strong> (where the operator loads); set its shared payloads below. The group zones are the source it pulls from and the supermarket it feeds.</div>')
    : '';
  return '<div class="loader-box" data-loader-id="' + l.id + '" data-layout="' + escapeHtml(l.layout) + '">'
    + '<div class="loader-box-header">'
    + '<span class="loader-box-name">' + escapeHtml(l.name || '(unnamed)') + '</span>'
    + '<span class="loader-box-meta">' + meta + '</span>'
    + thresholdGapHtml(item)
    + (isAuth ? '<button class="loader-box-edit" title="Edit loader">Edit</button>' : '')
    + (isAuth ? '<button class="loader-box-del" title="Delete loader">Delete</button>' : '')
    + '</div>'
    + '<div class="loader-box-body">' + '<div class="loader-members' + (dedicated ? ' loader-members-zoned' : '') + '">' + nodes + '</div>' + payloadSet + hint + groupsHtml + '</div>'
    + '</div>';
}

// nodeMembersHtml renders a loader's node members (bin_loader_homes). Shared_window
// = a flat list of windows (name only). dedicated_positions = members split into two
// zones by home_kind: HOME positions (payload pinned, or awaiting one) and BUFFER
// slots (kept partials, no payload). The zone IS the discriminator now — dropping a
// tile into Buffer marks it home_kind=buffer; an unpinned HOME stays a home (inert
// until a payload is picked), no longer mis-filed as a buffer (the D4 fix).
function nodeMembersHtml(item, dedicated) {
  const homes = item.homes || [];
  if (!dedicated) {
    if (!homes.length) {
      return '<span class="loader-members-empty">no windows yet — drag node tiles in</span>';
    }
    return homes.map(function (h) { return loaderMemberTile(h, false); }).join('');
  }
  const isBuffer = function (h) { return (h.home_kind || 'home') === 'buffer'; };
  const positions = homes.filter(function (h) { return !isBuffer(h); });
  const buffer = homes.filter(isBuffer);
  const posTiles = positions.length
    ? positions.map(function (h) { return loaderMemberTile(h, true); }).join('')
    : '<span class="loader-members-empty">no positions yet — drag a tile in, pick a payload</span>';
  const bufTiles = buffer.length
    ? buffer.map(function (h) { return loaderMemberTile(h, true); }).join('')
    : '<span class="loader-members-empty">no buffer slots — drag a tile into this zone</span>';
  return '<div class="loader-zone-label">Positions</div>'
    + '<div class="loader-zone">' + posTiles + '</div>'
    + '<div class="loader-zone-label">Buffer <span class="loader-zone-sub">kept partials · no payload</span></div>'
    + '<div class="loader-zone loader-zone-buffer">' + bufTiles + '</div>';
}

// loaderMemberTile draws one member slot — reused for windows, home positions and
// buffer slots. A HOME shows its per-spot payload picker; a BUFFER shows a static
// "buffer" badge (it pins no payload); a shared window shows name only. data-kind
// carries home_kind so a drag can tell a within-zone reorder from a cross-zone
// re-kind. The slot reuses the grid node tile (same block/size/state colour, copied
// in markLinkedTiles) with the loader controls on top.
function loaderMemberTile(h, dedicated) {
  const nm = nodesById[h.position_node_id] || ('node#' + h.position_node_id);
  const kind = (h.home_kind || 'home');
  let badge = '';
  if (dedicated) {
    if (kind === 'buffer') {
      badge = '<span class="loader-pc-badge loader-buffer-badge" title="kept-partial buffer slot — pins no payload">buffer</span>';
    } else {
      badge = isAuth ? payloadSelect(h.payload_code)
        : (h.payload_code ? '<span class="loader-pc-badge">' + escapeHtml(h.payload_code) + '</span>' : '');
    }
  }
  return '<div class="node-tile loader-member" data-id="' + h.position_node_id + '" data-kind="' + kind + '"' + (isAuth ? ' draggable="true"' : '') + '>'
    + (isAuth ? '<span class="loader-grip" title="drag the tile to reorder / move">⠿</span>' : '')
    + '<span class="tile-loc">' + escapeHtml(nm) + '</span>'
    + badge
    + (isAuth ? '<span class="loader-member-x" title="remove" draggable="false">×</span>' : '')
    + '</div>';
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

// payloadChipsHtml renders a shared_window loader's allowed payload set. The current
// set shows as chips; editing is a collapsible checklist of the whole catalog (checked =
// in the set) — check/uncheck several at once instead of typing + add one at a time.
function payloadChipsHtml(item) {
  const set = new Set((item.payloads || []).map(function (p) { return p.payload_code; }));
  const chips = Array.from(set).map(function (c) { return '<span class="loader-chip">' + escapeHtml(c) + '</span>'; }).join('');
  if (!isAuth) {
    return '<div class="loader-payload-set"><span class="loader-set-label">Allowed payloads:</span>' + chips + '</div>';
  }
  let html = '<div class="loader-payload-set" data-loader-id="' + item.loader.id + '"><span class="loader-set-label">Allowed payloads (' + set.size + '):</span> ' + chips;
  // Collapsible whole-catalog checklist. Ticking boxes only updates local state — the
  // panel stays OPEN and nothing round-trips until "Save payloads" commits the diff in
  // one batch (set-payload for adds, remove-payload for removes, one refresh after).
  html += '<details class="loader-pc-checklist" style="margin-top:4px">'
    + '<summary style="cursor:pointer;color:var(--primary)">Select payloads ▾</summary>'
    + '<div class="loader-pc-list" style="max-height:180px;overflow-y:auto;border:1px solid var(--border);border-radius:4px;padding:6px;margin-top:4px;display:flex;flex-direction:column;gap:2px">';
  html += payloadCodes.map(function (c) {
    return '<label style="display:flex;align-items:center;gap:6px;cursor:pointer;font-size:0.85rem">'
      + '<input type="checkbox" class="loader-pc-cb" data-pc="' + escapeHtml(c) + '"' + (set.has(c) ? ' checked' : '') + '>'
      + escapeHtml(c) + '</label>';
  }).join('');
  html += '</div>'
    + '<div class="loader-pc-actions">'
    + '<button type="button" class="loader-pc-save">Save payloads</button>'
    + '<span class="loader-pc-status"></span>'
    + '</div>'
    + '</details></div>';
  return html;
}

/* ── Wiring ───────────────────────────────────────────── */

function wireAll(host) {
  host.querySelectorAll('.loader-box').forEach(function (box) {
    const lid = box.dataset.loaderId;
    box.addEventListener('dragover', onBoxDragOver);
    box.addEventListener('dragleave', onBoxDragLeave);
    box.addEventListener('drop', onBoxDrop);
    // 1b: the associated group zone (output) is a drop-target — dropping a
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

    // Allowed-payload checklist: ticking a box only updates the live "unsaved" status
    // (no API call, no re-render — the panel stays open). The Save button commits the
    // diff in one batch. See savePayloads.
    const pcSave = box.querySelector('.loader-pc-save');
    if (pcSave) {
      const updateStatus = function () { refreshPayloadStatus(lid, box); };
      box.querySelectorAll('.loader-pc-cb').forEach(function (cb) {
        cb.addEventListener('change', updateStatus);
      });
      pcSave.addEventListener('click', function () { savePayloads(lid, box, pcSave); });
      updateStatus();
    }
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
  // Which zone did the tile land in? The Buffer zone marks the member
  // home_kind=buffer; anywhere else (Positions zone, shared-window list) is a home.
  const inBuffer = !!(e.target && e.target.closest && e.target.closest('.loader-zone-buffer'));
  const homeKind = inBuffer ? 'buffer' : 'home';

  // Reorder-only when an existing member is dragged within its own zone (kind
  // unchanged). A cross-zone drag (home↔buffer) falls through to set-home to re-kind.
  const draggedTile = tiles.find(function (t) { return parseInt(t.dataset.id, 10) === nodeId; });
  const curKind = draggedTile ? (draggedTile.dataset.kind || 'home') : null;
  if (already && curKind === homeKind) { reorder(); return; }

  // New position (from the grid or another loader), or a cross-zone re-kind. A buffer
  // pins no payload; a home preserves any prior payload/threshold on a move.
  const prev = findHomeAnyLoader(nodeId);
  apiPost('/api/loader/set-home', {
    loader_id: lid, position_node_id: nodeId,
    payload_code: homeKind === 'buffer' ? '' : (prev ? prev.payload_code : ''),
    home_kind: homeKind,
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
    home_kind: home ? (home.home_kind || 'home') : 'home',
    uop_threshold: home ? home.uop_threshold : 0,
  }).then(refresh).catch(function (err) { toast('' + err, 'error'); });
}
function removeMember(lid, nodeId) {
  apiPost('/api/loader/remove-home', { loader_id: Number(lid), position_node_id: Number(nodeId) }).then(refresh).catch(function (err) { toast('' + err, 'error'); });
}
function deleteLoader(lid) {
  apiPost('/api/loader/delete', { id: Number(lid) }).then(refresh).catch(function (err) { toast('' + err, 'error'); });
}
// loaderPayloadDiff returns {checked, toAdd, toRemove} for a loader's checklist:
// the currently-ticked boxes vs the loader's saved payload set.
function loaderPayloadDiff(lid, box) {
  const item = loaderData.find(function (it) { return String(it.loader.id) === String(lid); });
  const current = new Set(((item && item.payloads) || []).map(function (p) { return p.payload_code; }));
  const checked = [];
  box.querySelectorAll('.loader-pc-cb').forEach(function (cb) { if (cb.checked) checked.push(cb.dataset.pc); });
  const checkedSet = new Set(checked);
  const toAdd = checked.filter(function (pc) { return !current.has(pc); });
  const toRemove = Array.from(current).filter(function (pc) { return !checkedSet.has(pc); });
  return { checked: checked, toAdd: toAdd, toRemove: toRemove };
}

// refreshPayloadStatus updates the checklist's live "unsaved" line + Save button state
// as boxes are ticked, without touching the server.
function refreshPayloadStatus(lid, box) {
  const btn = box.querySelector('.loader-pc-save');
  const status = box.querySelector('.loader-pc-status');
  if (!btn) return;
  const d = loaderPayloadDiff(lid, box);
  const dirty = d.toAdd.length + d.toRemove.length > 0;
  btn.disabled = !dirty;
  btn.classList.toggle('is-dirty', dirty);
  if (status) {
    status.textContent = dirty
      ? '● ' + d.checked.length + ' selected · +' + d.toAdd.length + ' / −' + d.toRemove.length + ' unsaved'
      : d.checked.length + ' selected · saved';
  }
}

// savePayloads commits the checklist in ONE batch: it diffs the ticked boxes against
// the loader's saved set and fires the set-payload (adds) / remove-payload (removes)
// calls together, then refreshes once — so the panel stays open while you tick boxes
// instead of collapsing + round-tripping per click.
function savePayloads(lid, box, btn) {
  const d = loaderPayloadDiff(lid, box);
  if (!d.toAdd.length && !d.toRemove.length) { toast('No payload changes to save', 'warning'); return; }
  if (btn) { btn.disabled = true; btn.textContent = 'Saving…'; }
  const ops = d.toAdd.map(function (pc) {
    return apiPost('/api/loader/set-payload', { loader_id: Number(lid), payload_code: pc, uop_threshold: 0 });
  }).concat(d.toRemove.map(function (pc) {
    return apiPost('/api/loader/remove-payload', { loader_id: Number(lid), payload_code: pc });
  }));
  Promise.all(ops).then(function () {
    toast('Saved ' + (d.toAdd.length + d.toRemove.length) + ' payload change(s)', 'success');
    refresh();
  }).catch(function (err) {
    toast('' + err, 'error');
    if (btn) { btn.disabled = false; btn.textContent = 'Save payloads'; }
  });
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

delegateActions(document.body, { openLoaderModal, closeLoaderModal, submitLoader,
  addLoaderQuota, removeLoaderQuota, removeWindowBinType });

// The two controls that commit on CHANGE rather than on click: the carrier-mix
// count and the per-window "+ type" picker. setLoaderQuota was registered as a
// click action, which on a number input fires only when the input itself is
// clicked — typing a count and tabbing away saved nothing.
delegateActions(document.body, { setLoaderQuota, addWindowBinType }, { event: 'change' });

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
  loadBinTypeCatalog();
  // One handler: every control that changes WHAT APPLIES re-decides what is on
  // screen. Kind, role and fed-by-hand each remove or restore whole questions.
  ['loader-kind', 'loader-role', 'loader-fed-by-hand'].forEach(function (id) {
    const el = document.getElementById(id);
    if (el) el.addEventListener('change', applyLoaderForm);
  });
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
