import { apiGet, apiPost, delegateActions, h, toast, uiConfirm } from '/static/app.js';

// Core-owned bin loaders ("stations"), drawn as boxes on the Nodes page.
//
// THE BOX IS THE FORM. The create card asks three things — a name, what the
// station does, and (for an unloader) whether the full bin comes off at one
// station or two — and nothing else. Every place the station uses is then
// filled in on its box: each slot is labelled in plant words, noun first
// ("Windows", "Fulls come from", "Empties go to"), and a required slot that is
// still empty is red and says what it needs. Everything that has a sensible
// default sits behind one Settings link on the box and saves as it is ticked.
//
// Two ways to fill a slot, one API call each:
//   - tap the slot to arm it, then tap a node (or a group header) on the grid.
//     While a slot is armed the grid dims every tile that cannot go there and
//     prints why on the tile; a Find box filters by name; Esc disarms.
//   - drag a node tile (or a group header, `application/x-node-group`) onto
//     the slot. Group drags are accepted only by the slots that name a place;
//     a window is always one node.
//
// Coexistence with the supermarket drag code: every drop handler here calls
// stopPropagation so the drop never falls through to #nodes-drop-area's
// onDropGrid (which would reparent the node in topology). Member ⠿-grips and
// group headers set ONLY custom drag types (not text/plain), so dragging one
// onto the grid is a no-op there. Membership is an overlay
// (bin_loader_homes), never a topology move, so boxes render their own tiles
// and leave the canonical grid tile in place.

let nodesByName = {};
let nodesById = {};
let nodeInfo = {}; // node id -> {id, name, parentName, synthetic, typeCode}
let payloadCodes = [];
let loaderData = []; // raw /api/loader/list: [{loader, payloads, homes, quota, window_bin_types}]
let draggingMemberNode = null;

// View state for the boxes. None of it is configuration: it says which slot
// is armed, which station has its settings open, and which pairs have the
// "carts wait in a group" box ticked before a group has been named.
let armed = null;        // {loaderID, slot} | null
let settingsOpen = 0;    // loader id (stage 1 for a pair) whose settings show
let waitOpen = {};       // stage-1 id -> true while the wait slot is being filled
let findText = '';

const pageData = document.getElementById('page-data');
const isAuth = !!pageData && pageData.dataset.authenticated === 'true';

function val(id) { const e = document.getElementById(id); return e ? (e.value || '').trim() : ''; }
function setVal(id, v) {
  // Skips a write that changes nothing, so a re-render never moves the caret
  // of an input that is being typed into.
  const e = document.getElementById(id);
  if (e && e.value !== v) e.value = v;
}
function setText(id, t) { const e = document.getElementById(id); if (e) e.textContent = t; }
function setShown(id, show) {
  const e = document.getElementById(id);
  if (e && e.classList) e.classList.toggle('is-hidden', !show);
}
function setPressed(id, on) {
  const e = document.getElementById(id);
  if (!e) return;
  if (e.classList) e.classList.toggle('is-selected', !!on);
  if (e.setAttribute) e.setAttribute('aria-pressed', on ? 'true' : 'false');
}

/* ── Form state ───────────────────────────────────────────────────────────
   Written to the form-state convention in docs/ui-style-guide.md: the state
   lives in ONE object, what is on screen is DERIVED from it by formShape, and
   the rules are pure functions of it. The same state shape serves the create
   card (id 0) and a saved station's Settings (id set), so formShape is the one
   place that decides what shows on either.
*/

let formState = blankForm();

function blankForm() {
  return {
    id: 0,
    name: '',
    role: '',     // '' until picked | produce | consume
    stages: '',   // '' until picked | single | two   (unloaders only)
  };
}

// STAGE_SUFFIX is how Core names the two loaders of a pair; the box shows the
// station under its own name, without it.
const STAGE_SUFFIX = [' · stage 1', ' · stage 2'];
function baseName(name) {
  const n = name || '';
  for (let i = 0; i < STAGE_SUFFIX.length; i++) {
    if (n.endsWith(STAGE_SUFFIX[i])) return n.slice(0, n.length - STAGE_SUFFIX[i].length);
  }
  return n;
}

// readForm snapshots the create card. Role and stages are button choices and
// already live in state; the name is the one text input.
function readForm() {
  return Object.assign({}, formState, { name: val('loader-name') });
}

// normalizeForm folds in the choices that IMPLY another value, so the screen
// and what gets saved cannot disagree:
//   - FED DIRECTLY means there is no source, so the source is cleared, not
//     merely hidden.
//   - Two stations is an unloader's answer; a loader clears nothing.
function normalizeForm(state) {
  if (state.fedByHand) state.inbound = '';
  if (state.role !== 'consume') state.stages = state.id ? 'single' : '';
  return state;
}

// formShape decides WHAT IS ON SCREEN, from state alone. A row that does not
// apply is absent rather than disabled: an absent control needs no paragraph
// explaining why it cannot be used.
function formShape(state) {
  const saved = state.id !== 0;
  const unloader = state.role === 'consume';
  const pair = state.stages === 'two';
  const dedicated = !!state.dedicated;
  return {
    // Create card. How the full bin comes off is asked only of an unloader,
    // and only when it is created: a saved station is one or two stations.
    stages: !saved && unloader,
    // Settings rows. Partials and re-pulling the next full are questions about
    // what an UNLOADER is fed; the service refuses both on a loader.
    partials: saved && unloader,
    autoPush: saved && unloader,
    fedByHand: saved,
    // An unloader drains when a window is cleared; only a loader has a supply
    // mode to choose, and only a loader has a changeover card to commandeer.
    supply: saved && !unloader,
    changeover: saved && !unloader,
    // A pair is two shared-window unloaders; there is no layout to pick.
    dedicated: saved && !pair,
    // Filling one window at a time is a budget shared across windows, so it
    // means nothing where every position is its own one-bin slot.
    funnel: saved && !dedicated,
    // The carrier mix and the per-window capability are properties of a
    // window SET; a dedicated station is already one part per spot.
    mix: saved && !dedicated,
    windows: saved && !dedicated,
    remove: saved,
  };
}

// validateForm is a pure function of state — no DOM reads — so it can be
// tested. The backend checks the same rules; this one is for immediate
// feedback, under the field it is about.
function validateForm(state) {
  const errors = [];
  if (!state.name) errors.push({ field: 'loader-name', msg: 'Name is required' });
  if (!state.role) errors.push({ field: 'loader-role', msg: 'Pick what it does' });
  if (state.role === 'consume' && !state.stages) {
    errors.push({ field: 'loader-stages', msg: 'Pick how the full bin comes off' });
  }
  return { ok: errors.length === 0, errors };
}

const ERROR_SLOTS = {
  'loader-name': 'loader-name-error',
  'loader-role': 'loader-role-error',
  'loader-stages': 'loader-stages-error',
  'loader-form': 'loader-form-error',
};

function showErrors(errors) {
  Object.keys(ERROR_SLOTS).forEach(function (f) { setText(ERROR_SLOTS[f], ''); });
  const name = document.getElementById('loader-name');
  if (name && name.classList) name.classList.remove('form-input--error');
  (errors || []).forEach(function (er) {
    setText(ERROR_SLOTS[er.field] || ERROR_SLOTS['loader-form'], er.msg);
    if (er.field === 'loader-name' && name && name.classList) name.classList.add('form-input--error');
  });
}

function renderForm(state) {
  setVal('loader-name', state.name);
  setPressed('loader-role-produce', state.role === 'produce');
  setPressed('loader-role-consume', state.role === 'consume');
  setPressed('loader-stages-single', state.stages === 'single');
  setPressed('loader-stages-two', state.stages === 'two');
  setShown('loader-stages-row', formShape(state).stages);
}

function pickStationRole(role) {
  formState = normalizeForm(Object.assign(readForm(), { role: role }));
  renderForm(formState);
  setText(ERROR_SLOTS['loader-role'], '');
}

function pickStationStages(stages) {
  formState = normalizeForm(Object.assign(readForm(), { stages: stages }));
  renderForm(formState);
  setText(ERROR_SLOTS['loader-stages'], '');
}

// createRequest is the create card's wire shape — the one place a new station
// becomes a request, so the three kinds cannot drift on what they send.
// "Pull the next full automatically" defaults ON for a new unloader; every
// other setting starts at the column default and lives in Settings.
function createRequest(state) {
  if (state.role === 'consume' && state.stages === 'two') {
    return {
      url: '/api/loader/create-two-stage',
      body: { name: state.name, accept_partials: false, auto_push: true },
    };
  }
  const body = { name: state.name, role: state.role, layout: 'shared_window', replenishment: 'operator' };
  if (state.role === 'consume') body.auto_push = true;
  return { url: '/api/loader/create', body: body };
}

function openLoaderModal() {
  formState = blankForm();
  renderForm(formState);
  showErrors([]);
  const m = document.getElementById('loader-modal');
  if (m) m.classList.add('active');
  const n = document.getElementById('loader-name');
  if (n && n.focus) n.focus();
}

// Closing discards the card (style guide: clear-on-close), whichever way it
// was closed: ×, Cancel or Esc. The backdrop does not close it.
function closeLoaderModal() {
  const m = document.getElementById('loader-modal');
  if (m) m.classList.remove('active');
  formState = blankForm();
  renderForm(formState);
  showErrors([]);
}

function modalOpen() {
  const m = document.getElementById('loader-modal');
  return !!(m && m.classList && m.classList.contains('active'));
}

async function submitLoader() {
  const state = normalizeForm(readForm());
  formState = state;
  const v = validateForm(state);
  showErrors(v.errors);
  if (!v.ok) return;
  const req = createRequest(state);
  const btn = document.getElementById('loader-submit-btn');
  if (btn) btn.disabled = true;
  try {
    const d = await apiPost(req.url, req.body);
    if (d && d.error) { showErrors([{ field: 'loader-form', msg: d.error }]); return; }
    closeLoaderModal();
    await refresh();
    // The new box is the next thing to fill in: arm its windows so the next
    // tap on the grid adds one.
    if (d && d.id) armSlotFor(Number(d.id), 'windows');
  } catch (e) {
    showErrors([{ field: 'loader-form', msg: '' + e }]);
  } finally {
    if (btn) btn.disabled = false;
  }
}

/* ── Saved station → state → update body ──────────────────────────────── */

// fedDirectlyOf reads "fed directly from process" from its own field. A blank
// source is not the answer: a new station has a blank source too, and that one
// needs a place.
function fedDirectlyOf(l) {
  return !!l.fed_directly;
}

// formStateFromLoader is the one place a stored loader becomes state.
function formStateFromLoader(l) {
  return {
    id: Number(l.id),
    // The stored name, suffix and all: an update writes it back as it is.
    name: l.name || '',
    role: l.role || 'produce',
    stages: l.second_stage_loader_id ? 'two' : 'single',
    stage2ID: Number(l.second_stage_loader_id || 0),
    dedicated: l.layout === 'dedicated_positions',
    funnel: !!l.funnel_windows,
    changeoverLoadDirective: !!l.changeover_load_directive,
    acceptPartials: !!l.accept_partials,
    autoPush: !!l.auto_push,
    replenishment: l.replenishment || 'operator',
    fedByHand: fedDirectlyOf(l),
    inbound: l.inbound_source || '',
    outbound: l.outbound_dest || '',
  };
}

// loaderPayload is the update body of a state. /api/loader/update is a
// full-row write, so every stored field is sent back — a field this screen
// does not show is carried through unchanged rather than flattened.
function loaderPayload(state) {
  const unloader = state.role === 'consume';
  return {
    id: state.id,
    name: state.name,
    layout: state.dedicated ? 'dedicated_positions' : 'shared_window',
    replenishment: state.replenishment,
    funnel_windows: !!state.funnel,
    changeover_load_directive: !!state.changeoverLoadDirective,
    // Unloaders only; the server refuses true on a loader.
    accept_partials: unloader && !!state.acceptPartials,
    auto_push: unloader && !!state.autoPush,
    fed_directly: !!state.fedByHand,
    inbound_source: state.inbound,
    outbound_dest: state.outbound,
  };
}

// settingUpdate is one ticked Settings row as an update body: the stored row
// with that one field changed.
function settingUpdate(loader, field, value) {
  const state = formStateFromLoader(loader);
  state[field] = value;
  return loaderPayload(normalizeForm(state));
}

// placeUpdate is one place slot filled (or cleared, with ''). Naming a source
// is the answer to "fed directly?" as well, so it clears that.
function placeUpdate(loader, slot, name) {
  const state = formStateFromLoader(loader);
  if (slot === 'inbound') { state.inbound = name; state.fedByHand = false; }
  if (slot === 'outbound') state.outbound = name;
  return loaderPayload(state);
}

// waitGroupUpdates names (or, with '', clears) the group a pair's carts wait
// in: stage 2 pulls from it and stage 1 sends to it. STAGE 2 FIRST: stage 2
// having a source is what puts the pair in pull mode, and in direct mode Core
// derives stage 1's destination from stage 2's windows itself — so stage 1's
// write only means what it says once stage 2's has landed.
function waitGroupUpdates(stage1, stage2, group) {
  return [placeUpdate(stage2, 'inbound', group), placeUpdate(stage1, 'outbound', group)];
}

/* ── The box: slots in plant words ────────────────────────────────────── */

function loaderItem(loaderID) {
  const id = Number(loaderID || 0);
  if (!id) return null;
  return loaderData.find(function (x) { return Number(x.loader.id) === id; }) || null;
}

// stationContext finds the station a loader belongs to: itself, or the pair
// it is stage 1 or stage 2 of.
function stationContext(loaderID) {
  const item = loaderItem(loaderID);
  if (!item) return null;
  const l = item.loader;
  if (l.second_stage_loader_id) {
    return { s1: item, s2: loaderItem(l.second_stage_loader_id), stage: 1 };
  }
  const first = loaderData.find(function (x) {
    return Number(x.loader.second_stage_loader_id || 0) === Number(l.id);
  });
  if (first) return { s1: first, s2: item, stage: 2 };
  return { s1: item, s2: null, stage: 0 };
}

function stationName(ctx) { return baseName(ctx.s1.loader.name) || '(unnamed)'; }

function waiting(s1, s2) {
  return !!((s2 && s2.loader.inbound_source) || waitOpen[s1.loader.id]);
}

function windowsSlot(item) {
  return {
    key: 'windows', label: 'Windows', loaderID: Number(item.loader.id),
    required: true, empty: !(item.homes || []).length,
    need: 'Needs a window', hint: 'tap, then tap a node',
  };
}

function placeSlot(item, key, label, value, required) {
  return {
    key: key, label: label, loaderID: Number(item.loader.id),
    value: value || '', required: required, empty: !value,
    need: 'Needs a place', hint: 'tap, then tap a group or node',
  };
}

function partsSlot(item, label) {
  const payloads = item.payloads || [];
  return {
    key: 'parts', label: label, loaderID: Number(item.loader.id),
    required: true, empty: payloads.length === 0,
    need: 'Needs a part', hint: '',
  };
}

// stationSlots is the box as data: one group per stage (one group for a
// single station), each a list of slots. Pure, so what the box asks for can
// be tested without drawing it.
function stationSlots(item, s2) {
  const l = item.loader;
  const unloader = l.role === 'consume';
  const shared = l.layout !== 'dedicated_positions';
  if (!s2) {
    const fed = fedDirectlyOf(l);
    const inbound = placeSlot(item, 'inbound', unloader ? 'Fulls come from' : 'Empties come from', l.inbound_source, !fed);
    inbound.fed = fed && !l.inbound_source;
    // A loader with a spot per part fills each bin where it stands: its fulls
    // stay on its own spots, so a blank "Fulls go to" is a running station, not
    // a missing place.
    const staysOnSpots = !unloader && !shared;
    const outbound = placeSlot(item, 'outbound', unloader ? 'Empties go to' : 'Fulls go to', l.outbound_dest, !staysOnSpots);
    outbound.stays = staysOnSpots && !l.outbound_dest;
    const slots = [windowsSlot(item), inbound, outbound];
    if (shared) slots.push(partsSlot(item, unloader ? 'Parts it drains' : 'Parts it fills'));
    return [{ stage: 0, item: item, slots: slots }];
  }
  const fed1 = fedDirectlyOf(l);
  const in1 = placeSlot(item, 'inbound', 'Fulls come from', l.inbound_source, !fed1);
  in1.fed = fed1 && !l.inbound_source;
  const wait = waiting(item, s2);
  const one = [windowsSlot(item), in1, partsSlot(item, 'Parts it drains')];
  // Direct mode: Core names stage 1's destination from stage 2's windows. It
  // is shown, not asked.
  if (!wait) {
    one.push({ key: 'onto', label: 'Carts go on to', readonly: true, value: l.outbound_dest || '',
      loaderID: Number(l.id), required: false, empty: false });
  }
  const two = [
    windowsSlot(s2),
    placeSlot(s2, 'outbound', 'Carts with an empty bin go to', s2.loader.outbound_dest, true),
    { key: 'waitCheck', checked: wait, loaderID: Number(l.id), required: false, empty: false },
  ];
  if (wait) {
    const ws = placeSlot(s2, 'wait', 'Carts wait at', s2.loader.inbound_source, true);
    two.push(ws);
  }
  return [{ stage: 1, item: item, slots: one }, { stage: 2, item: s2, slots: two }];
}

function countNeeds(groups) {
  let n = 0;
  groups.forEach(function (g) {
    g.slots.forEach(function (s) { if (s.required && s.empty) n++; });
  });
  return n;
}

function statusText(n) {
  if (!n) return '';
  return 'Not running yet — ' + n + (n === 1 ? ' slot needs a place' : ' slots need a place');
}

function kindText(l, pair) {
  if (l.role !== 'consume') return 'Fills bins';
  return 'Empties bins · ' + (pair ? 'two stations' : 'one station');
}

const STAGE_TITLE = {
  1: 'Stage 1 · takes the full bin off',
  2: 'Stage 2 · puts an empty bin on',
};

// HTML is built with h`` (style guide, HTML construction): interpolations are
// escaped, arrays are joined as they are. A nested h`` result that is not in an
// array would be escaped again, so it goes through raw() — the helper's own
// opt-out — and every function here returns markup already built that way.
function raw(html) { return { __html: true, value: html || '' }; }

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
  const homeMissing = (item.homes || []).filter(function (hm) {
    return hm.payload_code && !(hm.uop_threshold > 0);
  });
  const n = missing.length + homeMissing.length;
  if (n === 0) return '';
  const total = (item.payloads || []).length + (item.homes || []).filter(function (hm) { return hm.payload_code; }).length;
  // No threshold ANYWHERE is the louder case: nothing orders for this loader at
  // all, rather than for some of its parts.
  const none = n === total;
  const names = missing.map(function (p) { return p.payload_code; })
    .concat(homeMissing.map(function (hm) { return hm.payload_code; }));
  const label = none
    ? 'no threshold set — nothing will order for this loader'
    : n + ' of ' + total + ' payloads need a threshold';
  return h`<a class="loader-threshold-gap${none ? ' loader-threshold-gap-none' : ''}" href="/inventory" title="${names.join(', ') + ' — set a UoP threshold on the Inventory page'}">${label}</a>`;
}

// configGapHtml surfaces the refusals the EDGE MAKES that are not an empty
// slot — on the screen where the loader is configured, which is the only
// screen that can say so.
//
// The Edge checks every loader in projectCoreLoader → the C0 constructors, and
// a loader that fails is SKIPPED: logged once, left out of the snapshot, no
// operator board. It cannot warn about a loader it has discarded, so this
// screen is the only witness. Springfield 2026-08-26 lost 70 minutes to a
// shared_window unloader with windows and no payload.
//
// The missing-member refusals (no windows, no positions, no payloads) are the
// red slots on the box now, and the box header counts them. What is left here
// is the malformed-member half, which has no slot to turn red.
//
// The conditions mirror shingo-edge/domain/loader.go NewSharedWindowLoader and
// NewDedicatedPositionsLoader. Keep them in step.
function configGapHtml(item) {
  const l = item.loader;
  const homes = item.homes || [];
  const payloads = item.payloads || [];
  const missing = [];
  if (l.layout !== 'dedicated_positions' && payloads.some(function (p) { return !p.payload_code; })) {
    missing.push('a blank payload');
  }
  if (homes.some(function (hm) { return !hm.position_node_id; })) missing.push('a position with no node');
  if (missing.length === 0) return '';
  return h`<span class="loader-config-gap" title="The Edge refuses a loader in this shape and renders no operator board for it.">${'incomplete — ' + missing.join(', ')}</span>`;
}

// gridHtml renders every station, drawing a two-stage unloader as ONE station
// with a Stage 1 and a Stage 2 column.
// stationsHeadHtml is the Stations section heading, in the same shape as the
// page's "Node groups" and "Ungrouped nodes" heads (nodes-supermarket.js
// sectionHead). A two-stage pair is one station, so a stage 2 is not counted.
function stationsHeadHtml(items) {
  const n = items.filter(function (it) {
    return !items.some(function (o) { return Number(o.loader.second_stage_loader_id) === Number(it.loader.id); });
  }).length;
  return h`<div class="section-head node-section-head"><h2>Stations <span class="node-section-count">${String(n)}</span></h2></div>`;
}

function gridHtml(items) {
  const byID = {};
  const secondStages = {};
  items.forEach(function (it) {
    byID[it.loader.id] = it;
    if (it.loader.second_stage_loader_id) secondStages[it.loader.second_stage_loader_id] = true;
  });
  return items.map(function (it) {
    if (secondStages[it.loader.id]) return '';
    const s2 = it.loader.second_stage_loader_id ? (byID[it.loader.second_stage_loader_id] || null) : null;
    return stationHtml(it, s2);
  }).join('');
}

function stationHtml(item, s2) {
  const l = item.loader;
  const pair = !!s2;
  const groups = stationSlots(item, s2);
  const status = statusText(countNeeds(groups));
  const open = isAuth && Number(settingsOpen) === Number(l.id);
  const head = h`<div class="loader-station-head">`
    + h`<span class="loader-box-name">${baseName(l.name) || '(unnamed)'}</span>`
    + h`<span class="loader-station-kind">${kindText(l, pair)}</span>`
    + (status ? h`<span class="badge badge-warn loader-station-status">${status}</span>` : '')
    + configGapHtml(item) + (s2 ? configGapHtml(s2) : '')
    + thresholdGapHtml(item)
    + (isAuth
      ? h`<button type="button" class="loader-settings-link" data-action="toggleStationSettings" data-loader-id="${l.id}">${open ? 'Close settings' : 'Settings'}</button>`
      : '')
    + h`</div>`;
  const body = open
    ? settingsHtml(item, s2)
    : h`<div class="loader-station-body${pair ? ' loader-pair-grid' : ''}">${groups.map(stageBoxHtml)}</div>`;
  return h`<div class="loader-station${pair ? ' loader-pair' : ''}" data-loader-id="${l.id}">${raw(head)}${raw(body)}</div>`;
}

function stageBoxHtml(group) {
  const l = group.item.loader;
  const title = group.stage ? h`<div class="loader-stage-title">${STAGE_TITLE[group.stage]}</div>` : '';
  return h`<div class="loader-box" data-loader-id="${l.id}" data-layout="${l.layout}" data-stage="${group.stage}">${raw(title)}${group.slots.map(function (s) { return slotHtml(s, group); })}</div>`;
}

function isArmed(slot) {
  return !!armed && Number(armed.loaderID) === Number(slot.loaderID) && armed.slot === slot.key;
}

function slotHtml(slot, group) {
  if (slot.key === 'waitCheck') {
    const id = 'loader-wait-' + slot.loaderID;
    return h`<label class="form-check loader-wait-check" for="${id}"><input type="checkbox" id="${id}" data-action-change="toggleWaitGroup" data-loader-id="${slot.loaderID}"${raw(slot.checked ? ' checked' : '')}${raw(isAuth ? '' : ' disabled')}> Carts wait in a group between the stations</label>`;
  }
  const needed = slot.required && slot.empty;
  const cls = 'loader-slot loader-slot-' + slot.key + (needed ? ' is-needed' : '') + (isArmed(slot) ? ' is-armed' : '');
  const label = h`<div class="loader-slot-label">${slot.label}</div>`;
  let body;
  if (slot.key === 'windows') {
    body = windowsBodyHtml(group.item, slot);
  } else if (slot.key === 'parts') {
    body = (needed ? h`<div class="loader-slot-need">${slot.need}</div>` : '') + payloadChipsHtml(group.item);
  } else if (slot.readonly) {
    body = h`<div class="loader-place is-readonly">${slot.value || '—'}${raw(placeKindHtml(slot.value))}</div>`;
  } else {
    return h`<div class="${cls} loader-slot-place" data-slot="${slot.key}" data-loader-id="${slot.loaderID}">${raw(label)}${raw(placeBodyHtml(slot))}</div>`;
  }
  return h`<div class="${cls}" data-slot="${slot.key}" data-loader-id="${slot.loaderID}">${raw(label)}${raw(body)}</div>`;
}

// placeKindHtml says what a named place IS: a group, or a name that no longer
// resolves to any node (a destination is stored as a name, so a renamed or
// deleted node leaves one behind).
function placeKindHtml(name) {
  if (!name) return '';
  const id = nodesByName[name];
  if (id == null) return Object.keys(nodesByName).length ? h` <span class="loader-place-kind is-warn">· not found</span>` : '';
  const n = nodeInfo[id];
  return n && n.synthetic ? h` <span class="loader-place-kind">· group</span>` : '';
}

function placeBodyHtml(slot) {
  let inner;
  if (!slot.empty) {
    inner = h`<span class="loader-place-name">${slot.value}</span>${raw(placeKindHtml(slot.value))}`;
  } else if (slot.fed) {
    inner = h`<span class="loader-place-name">Fed directly from process</span>`;
  } else if (slot.stays) {
    inner = h`<span class="loader-place-name">Stays on its own spots</span>`;
  } else {
    inner = h`<span>${slot.need}</span>` + (isAuth ? h`<span class="loader-place-hint">${slot.hint}</span>` : '');
  }
  const state = slot.empty ? (slot.required ? ' is-needed' : ' is-quiet') : '';
  if (!isAuth) return h`<div class="loader-place${state}">${raw(inner)}</div>`;
  const shown = isArmed(slot) ? h`<span>Tap a group or node below</span>` : inner;
  const clear = slot.empty ? ''
    : h`<button type="button" class="loader-place-x" title="Remove" aria-label="${'Remove ' + slot.label}" data-action="clearSlot" data-loader-id="${slot.loaderID}" data-slot="${slot.key}">×</button>`;
  return h`<div class="loader-place-row"><button type="button" class="loader-place${state}" data-action="armSlot" data-loader-id="${slot.loaderID}" data-slot="${slot.key}">${raw(shown)}</button>${raw(clear)}</div>`;
}

function windowsBodyHtml(item, slot) {
  const dedicated = item.loader.layout === 'dedicated_positions';
  const on = isArmed(slot);
  const add = isAuth
    ? h`<button type="button" class="loader-slot-add${on ? ' is-armed' : ''}" data-action="armSlot" data-loader-id="${slot.loaderID}" data-slot="windows">${on ? '+ add — tap a node below' : '+ add'}</button>`
    : '';
  const need = slot.empty ? h`<span class="loader-slot-need">${slot.need}</span>` : '';
  return h`<div class="loader-members${dedicated ? ' loader-members-zoned' : ''}">${raw(nodeMembersHtml(item, dedicated, need + add))}</div>`;
}

// nodeMembersHtml renders a loader's node members (bin_loader_homes). Shared
// window = a flat list of windows. dedicated_positions = members split into two
// zones by home_kind: HOME positions (payload pinned, or awaiting one) and
// BUFFER slots (kept partials, no payload). The zone IS the discriminator —
// dropping a tile into Buffer marks it home_kind=buffer; an unpinned HOME stays
// a home (inert until a payload is picked).
function nodeMembersHtml(item, dedicated, tail) {
  const homes = item.homes || [];
  if (!dedicated) {
    return homes.map(function (hm) { return loaderMemberTile(hm, false); }).join('') + (tail || '');
  }
  const isBuffer = function (hm) { return (hm.home_kind || 'home') === 'buffer'; };
  const positions = homes.filter(function (hm) { return !isBuffer(hm); });
  const buffer = homes.filter(isBuffer);
  const bufTiles = buffer.length
    ? buffer.map(function (hm) { return loaderMemberTile(hm, true); })
    : raw(h`<span class="loader-members-empty">no buffer slots — drag a tile into this zone</span>`);
  return h`<div class="loader-zone-label">Positions</div>`
    + h`<div class="loader-zone">${positions.map(function (hm) { return loaderMemberTile(hm, true); })}${raw(tail)}</div>`
    + h`<div class="loader-zone-label">Buffer <span class="loader-zone-sub">kept partials · no payload</span></div>`
    + h`<div class="loader-zone loader-zone-buffer">${bufTiles}</div>`;
}

// loaderMemberTile draws one member slot — reused for windows, home positions and
// buffer slots. A HOME shows its per-spot payload picker; a BUFFER shows a static
// "buffer" badge; a shared window shows name only. data-kind carries home_kind so
// a drag can tell a within-zone reorder from a cross-zone re-kind.
function loaderMemberTile(home, dedicated) {
  const nm = nodesById[home.position_node_id] || ('node#' + home.position_node_id);
  const kind = (home.home_kind || 'home');
  let badge = '';
  if (dedicated) {
    if (kind === 'buffer') {
      badge = h`<span class="loader-pc-badge loader-buffer-badge" title="kept-partial buffer slot — pins no payload">buffer</span>`;
    } else if (isAuth) {
      badge = payloadSelect(home.payload_code);
    } else if (home.payload_code) {
      badge = h`<span class="loader-pc-badge">${home.payload_code}</span>`;
    }
  }
  const grip = isAuth ? h`<span class="loader-grip" title="drag the tile to reorder / move">⠿</span>` : '';
  const x = isAuth ? h`<span class="loader-member-x" title="remove" draggable="false">×</span>` : '';
  return h`<div class="node-tile loader-member" data-id="${home.position_node_id}" data-kind="${kind}"${raw(isAuth ? ' draggable="true"' : '')}>${raw(grip)}<span class="tile-loc" title="${nm}">${nm}</span>${raw(badge)}${raw(x)}</div>`;
}

function payloadSelect(sel) {
  const opts = payloadCodes.map(function (c) {
    return h`<option value="${c}"${raw(c === sel ? ' selected' : '')}>${c}</option>`;
  });
  return h`<select class="loader-pc-sel${sel ? ' has-payload' : ''}" draggable="false"><option value="">+ payload</option>${opts}</select>`;
}

// payloadChipsHtml renders a shared-window loader's part set as chips, with a
// collapsible whole-catalog checklist to edit it. Ticking boxes only updates
// local state — nothing round-trips until "Save parts" commits the diff.
function payloadChipsHtml(item) {
  const set = new Set((item.payloads || []).map(function (p) { return p.payload_code; }));
  const chips = Array.from(set).map(function (c) { return h`<span class="loader-chip">${c}</span>`; });
  if (!isAuth) return h`<div class="loader-payload-set">${chips}</div>`;
  const boxes = payloadCodes.map(function (c) {
    return h`<label class="loader-pc-item"><input type="checkbox" class="loader-pc-cb" data-pc="${c}"${raw(set.has(c) ? ' checked' : '')}>${c}</label>`;
  });
  return h`<div class="loader-payload-set" data-loader-id="${item.loader.id}">${chips}`
    + h`<details class="loader-pc-checklist"><summary class="loader-pc-summary">+ part</summary>`
    + h`<div class="loader-pc-list">${boxes}</div>`
    + h`<div class="loader-pc-actions"><button type="button" class="loader-pc-save">Save parts</button><span class="loader-pc-status"></span></div>`
    + h`</details></div>`;
}

/* ── Settings (behind the box's one link; saves as ticked) ────────────── */

function settingCheck(loaderID, field, on, text) {
  const id = 'loader-set-' + loaderID + '-' + field;
  return h`<label class="form-check loader-setting" for="${id}"><input type="checkbox" id="${id}" data-action-change="saveStationSetting" data-loader-id="${loaderID}" data-field="${field}"${raw(on ? ' checked' : '')}> ${text}</label>`;
}

const SUPPLY_OPTIONS = [
  ['operator', 'When the operator asks'],
  ['threshold', 'Automatically when parts run low'],
];

function supplyHtml(state) {
  const id = 'loader-set-' + state.id + '-replenishment';
  const opts = SUPPLY_OPTIONS.map(function (o) {
    return h`<option value="${o[0]}"${raw(o[0] === state.replenishment ? ' selected' : '')}>${o[1]}</option>`;
  });
  return h`<div class="form-group loader-setting"><label for="${id}">Order empties</label><select id="${id}" class="form-input" data-action-change="saveStationSetting" data-loader-id="${state.id}" data-field="replenishment">${opts}</select></div>`;
}

function settingsHtml(item, s2) {
  const state = formStateFromLoader(item.loader);
  const shape = formShape(state);
  const lid = state.id;
  const pair = !!s2;
  let first = '';
  if (shape.supply) first += supplyHtml(state);
  if (shape.partials) first += settingCheck(lid, 'acceptPartials', state.acceptPartials, 'Accept partly used bins, not only full ones');
  if (shape.autoPush) first += settingCheck(lid, 'autoPush', state.autoPush, 'Pull the next full automatically when a window frees');
  if (shape.fedByHand) first += settingCheck(lid, 'fedByHand', state.fedByHand, 'Fed directly from process');
  if (shape.changeover) first += settingCheck(lid, 'changeoverLoadDirective', state.changeoverLoadDirective, 'During a changeover, tell this station which carrier to load');
  // "Cart" is the operator's word for a two-stage unloader's carrier (style
  // guide glossary); every other station says carrier.
  const noun = pair ? 'cart' : 'carrier';
  let win = '';
  if (shape.dedicated) win += settingCheck(lid, 'dedicated', state.dedicated, 'One spot per part (each window takes one part only)');
  if (shape.funnel) win += settingCheck(lid, 'funnel', state.funnel, 'Fill one window at a time');
  if (shape.mix) {
    win += h`<div class="loader-setting loader-setting-block"><div class="loader-setting-label">Keep on hand <span class="loader-setting-aside">${'empty = any ' + noun}</span></div><div id="loader-mix-editor">${raw(mixEditorHtml(lid, noun))}</div></div>`;
  }
  if (shape.windows) {
    const caps = windowCapHtml(item, pair ? 'Stage 1' : '') + (pair ? windowCapHtml(s2, 'Stage 2') : '');
    win += h`<div class="loader-setting loader-setting-block"><div class="loader-setting-label">What each window can take <span class="loader-setting-aside">${'nothing set = any ' + noun}</span></div><div id="loader-windows-editor">${raw(caps)}</div></div>`;
  }
  const stageHead = pair ? h`<div class="loader-stage-title">Stage 1</div>` : '';
  const winSection = win ? h`<div class="loader-settings-section"><div class="loader-stage-title">Windows</div>${raw(win)}</div>` : '';
  const del = shape.remove
    ? h`<button type="button" class="btn btn-sm btn-danger" data-action="deleteStation" data-loader-id="${lid}">Delete station</button>`
    : '';
  return h`<div class="loader-settings"><div class="loader-settings-section">${raw(stageHead)}${raw(first)}</div>${raw(winSection)}`
    + h`<div class="loader-settings-foot">${raw(del)}<span class="loader-settings-note">Changes save as you tick them.</span></div></div>`;
}

// mixEditorHtml draws the declared carrier mix: how many of each carrier type
// this station wants on hand. Empty is the normal state and means "take
// whatever is available". No bare types: a mix line of one could never be
// fetched, because no empty finder hands a bare carrier out.
function mixEditorHtml(loaderID, noun) {
  noun = noun || 'carrier';
  const lid = Number(loaderID);
  const item = loaderItem(loaderID);
  const mix = (item && item.quota) || [];
  const declared = mix.map(function (q) { return q.bin_type_code; });
  const rows = mix.map(function (q) {
    return h`<div class="loader-mix-line"><span class="loader-chip">${q.bin_type_code}</span>`
      + h`<input type="number" class="form-input loader-mix-want" min="0" value="${Number(q.want)}" aria-label="${'How many ' + q.bin_type_code + ' to keep on hand'}" data-action-change="setLoaderQuota" data-loader-id="${lid}" data-bin-type="${q.bin_type_code}">`
      + h`<button class="btn btn-sm" title="Remove" data-action="removeLoaderQuota" data-loader-id="${lid}" data-bin-type="${q.bin_type_code}">×</button></div>`;
  }).join('');
  const rest = binTypeOptions(declared, false);
  const add = rest
    ? h`<div class="loader-mix-add"><select id="loader-mix-add-type" class="form-input" aria-label="${noun + ' type'}">${raw(rest)}</select>`
      + h`<input type="number" id="loader-mix-add-want" class="form-input loader-mix-want" min="1" value="1" aria-label="How many">`
      + h`<button class="btn btn-sm" data-action="addLoaderQuota" data-loader-id="${lid}">${'+ ' + noun + ' type'}</button></div>`
    : '';
  return rows + add;
}

// windowCapHtml draws one row per window: what that window can physically
// take. Rows come out in the arranged order, the same order that decides which
// window fills first. A window with nothing set takes anything, and that has
// to stay the meaning of empty: every window at every plant is empty today.
//
// Never offers a bare type. A marker is admitted wherever its carrier is, so
// naming the carrier is enough, and a bare type is not something a person
// picks.
function windowCapHtml(item, heading) {
  if (!item) return '';
  const loaderID = Number(item.loader.id);
  const homes = (item.homes || []).slice().sort(function (a, b) {
    return (a.sort_order || 0) - (b.sort_order || 0);
  });
  const head = heading ? h`<div class="loader-window-cap-head">${heading}</div>` : '';
  if (!homes.length) return head + h`<div class="loader-window-cap-empty">No windows yet.</div>`;
  const caps = item.window_bin_types || {};
  return head + homes.map(function (hm) {
    const nodeID = Number(hm.position_node_id);
    const name = nodesById[nodeID] || ('node ' + nodeID);
    const set = caps[nodeID] || [];
    const chips = set.map(function (code) {
      return h`<span class="loader-chip">${code}<span class="loader-chip-x" title="Remove" data-action="removeWindowBinType" data-loader-id="${loaderID}" data-node-id="${nodeID}" data-bin-type="${code}">×</span></span>`;
    });
    const rest = binTypeOptions(set, false);
    const add = rest
      ? h`<select class="form-input" data-action-change="addWindowBinType" data-loader-id="${loaderID}" data-node-id="${nodeID}" aria-label="${'Add a type ' + name + ' can take'}"><option value="">+ type</option>${raw(rest)}</select>`
      : '';
    const any = chips.length ? '' : h`<span class="loader-window-cap-any">takes anything</span>`;
    return h`<div class="loader-window-cap"><span class="loader-window-cap-name">${name}</span>${chips}${raw(any)}${raw(add)}</div>`;
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

// binTypeOptions lists the carrier catalogue minus what is already set. An
// "add" control should only offer what can actually be added; when that leaves
// nothing the caller drops the control. withBare says whether bare types are
// offered at all; no caller on this page passes true.
function binTypeOptions(exclude, withBare) {
  const taken = {};
  (exclude || []).forEach(function (c) { taken[c] = true; });
  return binTypeCatalog.filter(function (t) { return !taken[t.code] && (withBare || !t.bare); })
    .map(function (t) {
      return h`<option value="${Number(t.id)}">${t.code}</option>`;
    }).join('');
}

function binTypeIDForCode(code) {
  const t = binTypeCatalog.find(function (x) { return x.code === code; });
  return t ? Number(t.id) : 0;
}

function elLoaderID(el) { return Number((el && el.getAttribute && el.getAttribute('data-loader-id')) || 0); }

function windowCapSet(loaderID, nodeID) {
  const item = loaderItem(loaderID);
  const caps = (item && item.window_bin_types) || {};
  return (caps[Number(nodeID)] || []).slice();
}

function saveWindowCap(loaderID, nodeID, codes) {
  const ids = codes.map(binTypeIDForCode).filter(function (n) { return n > 0; });
  return apiPost('/api/loader/set-window-bin-types', {
    loader_id: loaderID, position_node_id: Number(nodeID), bin_type_ids: ids,
  }).then(refresh).catch(function (e) { toast('' + e, 'error'); });
}

function addWindowBinType(el) {
  const loaderID = elLoaderID(el);
  const nodeID = Number(el.getAttribute('data-node-id') || 0);
  const binTypeID = Number(el.value || 0);
  if (!loaderID || !nodeID || !binTypeID) return;
  const t = binTypeCatalog.find(function (x) { return Number(x.id) === binTypeID; });
  if (!t) return;
  saveWindowCap(loaderID, nodeID, windowCapSet(loaderID, nodeID).concat([t.code]));
}

function removeWindowBinType(el) {
  const loaderID = elLoaderID(el);
  const nodeID = Number(el.getAttribute('data-node-id') || 0);
  const code = el.getAttribute('data-bin-type') || '';
  if (!loaderID || !nodeID || !code) return;
  saveWindowCap(loaderID, nodeID, windowCapSet(loaderID, nodeID).filter(function (c) { return c !== code; }));
}

function addLoaderQuota(el) {
  const loaderID = elLoaderID(el);
  const binTypeID = Number(val('loader-mix-add-type') || 0);
  const want = Number(val('loader-mix-add-want') || 0);
  if (!loaderID || !binTypeID || want < 1) return;
  apiPost('/api/loader/set-quota', { loader_id: loaderID, bin_type_id: binTypeID, want: want })
    .then(refresh).catch(function (e) { toast('' + e, 'error'); });
}

function setLoaderQuota(el) {
  const loaderID = elLoaderID(el);
  const want = Number(el.value || 0);
  const binTypeID = binTypeIDForCode(el.getAttribute('data-bin-type') || '');
  if (!loaderID || !binTypeID || want < 0) return;
  apiPost('/api/loader/set-quota', { loader_id: loaderID, bin_type_id: binTypeID, want: want })
    .then(refresh).catch(function (e) { toast('' + e, 'error'); });
}

function removeLoaderQuota(el) {
  const loaderID = elLoaderID(el);
  const binTypeID = binTypeIDForCode(el.getAttribute('data-bin-type') || '');
  if (!loaderID || !binTypeID) return;
  apiPost('/api/loader/remove-quota', { loader_id: loaderID, bin_type_id: binTypeID })
    .then(refresh).catch(function (e) { toast('' + e, 'error'); });
}

function toggleStationSettings(el) {
  const id = elLoaderID(el);
  settingsOpen = Number(settingsOpen) === id ? 0 : id;
  armed = null;
  renderGrid();
}

// saveStationSetting saves ONE ticked row: the stored loader with that field
// changed. Nothing else on the row moves, which is what keeps a setting this
// screen does not show — or one the person did not touch — as it was.
async function saveStationSetting(el) {
  const lid = elLoaderID(el);
  const field = el.getAttribute('data-field') || '';
  const item = loaderItem(lid);
  if (!item || !field) return;
  const value = el.type === 'checkbox' ? !!el.checked : el.value;
  const body = settingUpdate(item.loader, field, value);
  // Changing the layout would orphan the members: windows and dedicated
  // positions cannot carry across. Say so, and drop them first on a yes.
  if (field === 'dedicated') {
    const homes = item.homes || [];
    const pls = item.payloads || [];
    if (homes.length + pls.length > 0) {
      if (!await uiConfirm('This drops the station’s ' + homes.length + ' window(s) and ' + pls.length + ' part(s). Continue?')) {
        renderGrid();
        return;
      }
      try {
        await Promise.all([].concat(
          homes.map(function (hm) { return apiPost('/api/loader/remove-home', { loader_id: lid, position_node_id: hm.position_node_id }); }),
          pls.map(function (p) { return apiPost('/api/loader/remove-payload', { loader_id: lid, payload_code: p.payload_code }); })
        ));
      } catch (e) { toast('' + e, 'error'); return; }
    }
  }
  try {
    await apiPost('/api/loader/update', body);
  } catch (e) {
    toast('' + e, 'error');
  }
  await refresh();
}

async function deleteStation(el) {
  const lid = elLoaderID(el);
  const ctx = stationContext(lid);
  if (!ctx) return;
  const msg = 'Delete ' + stationName(ctx) + '?' + (ctx.s2 ? ' Both stages go with it.' : '');
  if (!await uiConfirm(msg)) return;
  try {
    await apiPost('/api/loader/delete', { id: Number(ctx.s1.loader.id) });
  } catch (e) { toast('' + e, 'error'); return; }
  settingsOpen = 0;
  armed = null;
  await refresh();
}

/* ── Tap-to-assign ────────────────────────────────────────────────────── */

// windowOwners maps each node that is some station's window to that station.
function windowOwners() {
  const out = {};
  loaderData.forEach(function (it) {
    const ctx = stationContext(it.loader.id);
    const station = ctx ? stationName(ctx) : baseName(it.loader.name);
    (it.homes || []).forEach(function (hm) {
      out[Number(hm.position_node_id)] = { loaderID: Number(it.loader.id), station: station };
    });
  });
  return out;
}

// armContext is what assignReason needs to know about the armed slot.
function armContext(a) {
  const ctx = stationContext(a.loaderID);
  if (!ctx) return null;
  const item = loaderItem(a.loaderID);
  const l = item.loader;
  let current = '';
  if (a.slot === 'inbound' || a.slot === 'wait') current = l.inbound_source || '';
  if (a.slot === 'outbound') current = l.outbound_dest || '';
  return {
    stage: ctx.stage,
    station: stationName(ctx),
    // Direct mode: stage 2's windows are grouped by Core, so a node already
    // in another group cannot join them.
    direct: ctx.stage === 2 && !(ctx.s2.loader.inbound_source || ''),
    ownGroup: ctx.s1.loader.outbound_dest || '',
    current: current,
    owners: windowOwners(),
  };
}

const GROUP_NOT_A_WINDOW = 'a group can’t be a window';

// assignReason says whether the armed slot can take a target, and the few
// words the tile shows either way. Pure: {ok, current?, note}.
//
// target: {id, name, group (a group header), synthetic, typeCode, parentName, hasBin}
function assignReason(a, target, ctx) {
  if (a.slot === 'windows') {
    if (target.group || target.synthetic) return { ok: false, note: GROUP_NOT_A_WINDOW };
    const owner = ctx.owners[Number(target.id)];
    if (owner && owner.loaderID === Number(a.loaderID)) {
      return { ok: false, current: true, note: ctx.stage ? 'already Stage ' + ctx.stage : 'already a window' };
    }
    if (owner) return { ok: false, note: 'window of ' + owner.station };
    if (ctx.stage === 2 && ctx.direct && target.parentName && target.parentName !== ctx.ownGroup) {
      return { ok: false, note: 'in ' + target.parentName };
    }
    if (ctx.stage === 2 && target.hasBin) return { ok: false, note: 'has a bin on it' };
    return { ok: true, note: 'tap to add' };
  }
  if (target.typeCode === 'LANE') return { ok: false, note: 'a lane' };
  if (!target.group) {
    const owner = ctx.owners[Number(target.id)];
    if (owner && owner.station === ctx.station) return { ok: false, note: 'window of ' + owner.station };
  }
  if (ctx.current && ctx.current === target.name) return { ok: false, current: true, note: 'already here' };
  return { ok: true, note: 'tap to use' };
}

function armText(a) {
  const ctx = stationContext(a.loaderID);
  if (!ctx) return '';
  if (a.slot === 'windows') {
    const what = ctx.stage ? 'a Stage ' + ctx.stage + ' window' : 'a window';
    return 'Adding ' + what + ' — tap a node. Greyed nodes can’t go here.';
  }
  const slot = [].concat.apply([], stationSlots(ctx.s1, ctx.s2).map(function (g) { return g.slots; }))
    .find(function (s) { return s.key === a.slot && Number(s.loaderID) === Number(a.loaderID); });
  return (slot ? slot.label : 'Place') + ' — tap a group or node. Greyed ones can’t go here.';
}

function assignBarHtml() {
  if (!armed) return '';
  return h`<div class="loader-assign-line"><span class="loader-assign-text">${armText(armed)}</span>`
    + h`<label for="loader-assign-find" class="loader-assign-find-label">Find</label>`
    + h`<input type="search" id="loader-assign-find" class="form-input" value="${findText}" data-action-input="filterAssign" autocomplete="off">`
    + h`<button type="button" class="btn btn-sm" data-action="disarmSlot">Done</button></div>`
    + h`<div class="loader-assign-hint">Dragging a tile onto a slot still works. Esc stops adding.</div>`;
}

function armSlotFor(loaderID, slot) {
  armed = { loaderID: Number(loaderID), slot: slot };
  settingsOpen = 0;
  renderGrid();
}

function armSlot(el) {
  const lid = elLoaderID(el);
  const slot = el.getAttribute('data-slot') || '';
  if (!lid || !slot) return;
  if (armed && armed.loaderID === lid && armed.slot === slot) { disarmSlot(); return; }
  armSlotFor(lid, slot);
}

function disarmSlot() {
  if (!armed) return;
  armed = null;
  findText = '';
  renderGrid();
}

function filterAssign(el) {
  findText = (el.value || '').trim();
  decorateGrid();
}

function tileTarget(tile) {
  const id = Number(tile.dataset.id || 0);
  const n = nodeInfo[id] || {};
  return {
    id: id,
    name: n.name || tile.dataset.name || '',
    synthetic: n.synthetic != null ? n.synthetic : tile.dataset.synthetic === 'true',
    typeCode: n.typeCode || tile.dataset.typeCode || '',
    parentName: n.parentName != null ? n.parentName : (tile.dataset.parentName || ''),
    hasBin: tile.dataset.hasPayload === 'true' || tile.dataset.hasEmptyBin === 'true',
  };
}

function groupTarget(groupEl) {
  const id = Number(groupEl.dataset.smktId || 0);
  const n = nodeInfo[id] || {};
  const nameEl = groupEl.querySelector('.smkt-name');
  return { id: id, name: n.name || (nameEl ? nameEl.textContent : ''), group: true, synthetic: true, typeCode: n.typeCode || 'NGRP' };
}

const ASSIGN_CLASSES = ['assign-lit', 'assign-dim', 'assign-current', 'assign-filtered'];

function markTarget(el, r, name) {
  ASSIGN_CLASSES.forEach(function (c) { el.classList.remove(c); });
  const old = el.querySelector(':scope > .assign-note');
  if (old) old.remove();
  if (!r) return;
  el.classList.add(r.ok ? 'assign-lit' : (r.current ? 'assign-current' : 'assign-dim'));
  if (findText && name.toLowerCase().indexOf(findText.toLowerCase()) < 0) el.classList.add('assign-filtered');
  const note = document.createElement('span');
  note.className = 'assign-note';
  note.textContent = r.note;
  el.appendChild(note);
}

// decorateGrid dims or lights every canonical tile (and every group header)
// for the armed slot, or clears the marks when nothing is armed.
function decorateGrid() {
  const area = document.getElementById('nodes-drop-area');
  if (!area) return;
  const ctx = armed ? armContext(armed) : null;
  if (armed && !ctx) armed = null;
  area.classList.toggle('assign-armed', !!armed);
  document.querySelectorAll('#nodes-drop-area .node-tile:not(.loader-member)').forEach(function (tile) {
    if (!armed) { markTarget(tile, null, ''); return; }
    const t = tileTarget(tile);
    markTarget(tile, assignReason(armed, t, ctx), t.name);
  });
  document.querySelectorAll('#nodes-drop-area .smkt-group').forEach(function (g) {
    const hdr = g.querySelector('.smkt-header');
    if (!hdr) return;
    if (!armed) { markTarget(hdr, null, ''); return; }
    const t = groupTarget(g);
    markTarget(hdr, assignReason(armed, t, ctx), t.name);
  });
}

// onArmedClick takes a tap on the grid while a slot is armed. Registered in
// the capture phase so it runs before the tile's own open-the-node action and
// the group header's collapse toggle, and stops both.
function onArmedClick(e) {
  if (!armed) return;
  const t = e.target;
  if (!t || !t.closest) return;
  if (t.closest('#loader-boxes') || t.closest('#loader-assign-bar')) return;
  const tile = t.closest('#nodes-drop-area .node-tile:not(.loader-member)');
  const hdr = tile ? null : t.closest('#nodes-drop-area .smkt-header');
  if (!tile && !hdr) return;
  e.preventDefault();
  e.stopPropagation();
  const el = tile || hdr;
  if (!el.classList.contains('assign-lit')) return;
  const target = tile ? tileTarget(tile) : groupTarget(hdr.closest('.smkt-group'));
  assignToArmed(target);
}

function assignToArmed(target) {
  const a = armed;
  if (!a) return;
  if (a.slot === 'windows') { appendWindow(a.loaderID, target.id); return; } // stays armed: windows come in sets
  armed = null;
  findText = '';
  assignPlace(a.loaderID, a.slot, target.name);
}

// appendWindow adds a window at the end of the station's arranged order — the
// same two calls a drop onto the box makes.
function appendWindow(lid, nodeId) {
  const item = loaderItem(lid);
  const existing = ((item && item.homes) || []).slice()
    .sort(function (a, b) { return (a.sort_order || 0) - (b.sort_order || 0); })
    .map(function (hm) { return Number(hm.position_node_id); })
    .filter(function (id) { return id !== Number(nodeId); });
  apiPost('/api/loader/set-home', {
    loader_id: Number(lid), position_node_id: Number(nodeId), payload_code: '', home_kind: 'home', uop_threshold: 0,
  }).then(function () {
    return apiPost('/api/loader/reorder-homes', { loader_id: Number(lid), ordered_ids: existing.concat([Number(nodeId)]) });
  }).then(refresh).catch(function (err) { toast('' + err, 'error'); refresh(); });
}

// assignPlace fills (or with '' clears) a place slot. The writes go one after
// another, in the order given: the pair's wait group depends on it.
async function assignPlace(lid, slot, name) {
  const ctx = stationContext(lid);
  const item = loaderItem(lid);
  if (!ctx || !item) return;
  const bodies = slot === 'wait'
    ? waitGroupUpdates(ctx.s1.loader, ctx.s2.loader, name)
    : [placeUpdate(item.loader, slot, name)];
  try {
    for (const b of bodies) await apiPost('/api/loader/update', b);
  } catch (e) {
    toast('' + e, 'error');
  }
  await refresh();
}

function clearSlot(el) {
  const lid = elLoaderID(el);
  const slot = el.getAttribute('data-slot') || '';
  if (!lid || !slot) return;
  if (armed && armed.loaderID === lid && armed.slot === slot) armed = null;
  assignPlace(lid, slot, '');
}

// toggleWaitGroup is the pair's "carts wait in a group between the stations".
// Ticking it only opens the Carts wait at slot (armed, so the next tap names
// the group); nothing is saved until a group is named. Unticking a pair that
// has a group puts it back in direct mode.
function toggleWaitGroup(el) {
  const s1id = elLoaderID(el);
  const ctx = stationContext(s1id);
  if (!ctx || !ctx.s2) return;
  const s2id = Number(ctx.s2.loader.id);
  if (el.checked) {
    waitOpen[s1id] = true;
    armSlotFor(s2id, 'wait');
    return;
  }
  delete waitOpen[s1id];
  if (armed && armed.loaderID === s2id && armed.slot === 'wait') armed = null;
  if (ctx.s2.loader.inbound_source) { assignPlace(s2id, 'wait', ''); return; }
  renderGrid();
}

/* ── Data load + grid render ──────────────────────────── */

async function refresh() {
  try {
    const results = await Promise.all([apiGet('/api/nodes'), apiGet('/api/payloads'), apiGet('/api/loader/list')]);
    const nd = results[0], pd = results[1], ld = results[2];
    const nodes = (nd && (nd.nodes || nd.data || nd)) || [];
    nodesByName = {}; nodesById = {}; nodeInfo = {};
    (Array.isArray(nodes) ? nodes : []).forEach(function (n) {
      const id = n.id != null ? n.id : n.ID, name = n.name != null ? n.name : n.Name;
      if (name == null) return;
      nodesByName[name] = id; nodesById[id] = name;
      nodeInfo[id] = {
        id: id, name: name, synthetic: !!n.is_synthetic,
        typeCode: n.node_type_code || '', parentName: n.parent_name || '',
      };
    });
    const ps = (pd && (pd.payloads || pd.data || pd)) || [];
    payloadCodes = (Array.isArray(ps) ? ps : []).map(function (p) {
      return p.code || p.Code || p.payload_code || p.PayloadCode || p;
    }).filter(Boolean);
    loaderData = (ld && ld.loaders) || [];
  } catch (e) { /* keep last render */ }
  if (armed && !loaderItem(armed.loaderID)) armed = null;
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
  let bar = document.getElementById('loader-assign-bar');
  if (!bar) {
    bar = document.createElement('div');
    bar.id = 'loader-assign-bar';
    area.insertBefore(bar, host.nextSibling);
  }
  host.innerHTML = loaderData.length
    ? stationsHeadHtml(loaderData) + gridHtml(loaderData)
    : (isAuth ? h`<div class="loader-empty">No stations yet. Use <strong>+ Station</strong> to add one.</div>` : '');
  bar.innerHTML = assignBarHtml();
  bar.classList.toggle('is-hidden', !armed);
  wireAll(host);
  markLinkedTiles();
  decorateGrid();
}

// markLinkedTiles mirrors each window's CANONICAL grid tile state onto its
// tile in the box, so a node shows the same live colour (loaded / empty /
// staged / claimed …) everywhere it appears. The canonical tile is not always
// in #tile-grid — buildHierarchy moves a group's children into the group card —
// so the lookup excludes the box tiles instead of scoping by container.
function markLinkedTiles() {
  const STATE = ['tile-has-payload', 'tile-empty-bin', 'tile-staged', 'tile-maintenance', 'tile-claimed', 'tile-disabled', 'tile-synthetic'];
  document.querySelectorAll('.loader-member[data-id]').forEach(function (m) {
    const id = m.dataset.id;
    const grid = document.querySelector('.node-tile[data-id="' + id + '"]:not(.loader-member)');
    STATE.forEach(function (c) { m.classList.remove(c); });
    if (grid) STATE.forEach(function (c) { if (grid.classList.contains(c)) m.classList.add(c); });
  });
}

/* ── Wiring ───────────────────────────────────────────── */

function wireAll(host) {
  host.querySelectorAll('.loader-box').forEach(function (box) {
    const lid = box.dataset.loaderId;
    box.addEventListener('dragover', onBoxDragOver);
    box.addEventListener('dragleave', onBoxDragLeave);
    box.addEventListener('drop', onBoxDrop);
    box.querySelectorAll('.loader-slot-place').forEach(function (s) {
      s.addEventListener('dragover', onPlaceDragOver);
      s.addEventListener('dragleave', onPlaceDragLeave);
      s.addEventListener('drop', onPlaceDrop);
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

function isGroupDrag(e) {
  const types = (e.dataTransfer && e.dataTransfer.types) || [];
  return Array.prototype.indexOf.call(types, 'application/x-node-group') >= 0;
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

// A window is one node. A group dragged onto a stage box is still let drop,
// so the refusal can say why instead of the drop silently not happening.
function onBoxDragOver(e) {
  e.stopPropagation();
  e.preventDefault();
  e.dataTransfer.dropEffect = 'move';
  this.classList.add('loader-drop-target');
}
function onBoxDragLeave() { this.classList.remove('loader-drop-target'); }

function onBoxDrop(e) {
  e.preventDefault();
  e.stopPropagation(); // keep the drop from reaching onDropGrid (topology reparent)
  this.classList.remove('loader-drop-target');
  if (isGroupDrag(e)) { toast(GROUP_NOT_A_WINDOW, 'warning'); return; }

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

function onPlaceDragOver(e) {
  e.preventDefault();
  e.stopPropagation();
  e.dataTransfer.dropEffect = isGroupDrag(e) ? 'link' : 'move';
  this.classList.add('loader-drop-target');
}
function onPlaceDragLeave() { this.classList.remove('loader-drop-target'); }

// onPlaceDrop names a place by drag: a group header or a node tile. The same
// rule the grid shows when the slot is armed decides whether it can go here.
function onPlaceDrop(e) {
  e.preventDefault();
  e.stopPropagation();
  this.classList.remove('loader-drop-target');
  if (e.dataTransfer.getData('application/x-loader-member')) return;
  const lid = parseInt(this.dataset.loaderId, 10);
  const slot = this.dataset.slot;
  const groupName = e.dataTransfer.getData('application/x-node-group');
  let target;
  if (groupName) {
    const gid = nodesByName[groupName];
    target = { id: gid, name: groupName, group: true, synthetic: true, typeCode: (nodeInfo[gid] || {}).typeCode || 'NGRP' };
  } else {
    const nodeId = parseInt(e.dataTransfer.getData('text/plain'), 10);
    if (!nodeId || !nodesById[nodeId]) return;
    const n = nodeInfo[nodeId] || {};
    target = { id: nodeId, name: nodesById[nodeId], synthetic: !!n.synthetic, typeCode: n.typeCode || '', parentName: n.parentName || '' };
  }
  const a = { loaderID: lid, slot: slot };
  const ctx = armContext(a);
  if (!ctx) return;
  const r = assignReason(a, target, ctx);
  if (!r.ok) { if (!r.current) toast(target.name + ': ' + r.note, 'warning'); return; }
  if (armed && armed.loaderID === lid && armed.slot === slot) { armed = null; findText = ''; }
  assignPlace(lid, slot, target.name);
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

// loaderPayloadDiff returns {checked, toAdd, toRemove} for a box's part
// checklist: the ticked boxes vs the loader's saved set.
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
      ? d.checked.length + ' selected · +' + d.toAdd.length + ' / −' + d.toRemove.length + ' unsaved'
      : d.checked.length + ' selected · saved';
  }
}

function savePayloads(lid, box, btn) {
  const d = loaderPayloadDiff(lid, box);
  if (!d.toAdd.length && !d.toRemove.length) return;
  if (btn) { btn.disabled = true; btn.textContent = 'Saving…'; }
  const ops = d.toAdd.map(function (pc) {
    return apiPost('/api/loader/set-payload', { loader_id: Number(lid), payload_code: pc, uop_threshold: 0 });
  }).concat(d.toRemove.map(function (pc) {
    return apiPost('/api/loader/remove-payload', { loader_id: Number(lid), payload_code: pc });
  }));
  Promise.all(ops).then(refresh).catch(function (err) {
    toast('' + err, 'error');
    if (btn) { btn.disabled = false; btn.textContent = 'Save parts'; }
  });
}

function findHome(lid, nodeId) {
  const item = loaderData.find(function (it) { return String(it.loader.id) === String(lid); });
  if (!item) return null;
  return (item.homes || []).find(function (hm) { return String(hm.position_node_id) === String(nodeId); }) || null;
}
function findHomeAnyLoader(nodeId) {
  for (const it of loaderData) {
    const h = (it.homes || []).find(function (hm) { return String(hm.position_node_id) === String(nodeId); });
    if (h) return h;
  }
  return null;
}

/* ── Init ─────────────────────────────────────────────── */

delegateActions(document.body, {
  openLoaderModal, closeLoaderModal, submitLoader, pickStationRole, pickStationStages,
  armSlot, disarmSlot, clearSlot, toggleStationSettings, deleteStation,
  addLoaderQuota, removeLoaderQuota, removeWindowBinType,
});

// The controls that commit on CHANGE rather than on click: every Settings
// row, the wait-group checkbox, the carrier-mix count and the per-window
// "+ type" picker.
delegateActions(document.body, { saveStationSetting, toggleWaitGroup, setLoaderQuota, addWindowBinType }, { event: 'change' });
delegateActions(document.body, { filterAssign }, { event: 'input' });

document.addEventListener('click', onArmedClick, true);

// Esc closes the create card when it is open, and otherwise stops adding.
document.addEventListener('keydown', function (e) {
  if (e.key !== 'Escape') return;
  if (modalOpen()) { closeLoaderModal(); return; }
  if (armed) disarmSlot();
});

// Continuous edge auto-scroll while a tile is dragged. Native HTML5 drag
// suppresses the mouse WHEEL entirely, so the only way to scroll mid-drag is to
// push the cursor toward the top/bottom edge. A 16ms timer (started on
// dragstart, stopped on dragend/drop) scrolls the window while the cursor sits
// in the edge band — speed scales with how deep into the band it is.
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
// earlier) has finished placing tiles before markLinkedTiles and decorateGrid
// read them. A deferred module executes at readyState 'interactive', so the
// listener still fires; 'complete' covers a late/dynamic load.
function init() {
  loadBinTypeCatalog();
  const name = document.getElementById('loader-name');
  if (name) name.addEventListener('keydown', function (e) { if (e.key === 'Enter') submitLoader(); });
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
