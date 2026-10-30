// Inventory: two tables from one read (/api/inventory/loop, service
// InventoryService.Loop). Supermarket positions: a row per (loader position,
// part) the threshold monitor watches. Lines: a row per claim of a running
// style at a line (an Edge process), consume and produce. Each table opens on
// the rows that need a look (the row's look, decided on Core), worst first, the
// healthy rest folded behind one control at its foot. Cells hold values; each
// column's meaning is in its header and its ⓘ. The count ledger is on
// /inventory/count-history.
//
// The UoP threshold is edited in the row and saved on Enter through the loader
// config's own doors (set-home / set-payload), which re-derive demand and
// write the one audit row the hover reads. The threshold calculator is
// uncalibrated: it is reached only from the UoP threshold column's ⓘ, and a
// value it suggests saves only on Confirm there.

import { apiGet, apiPost, escapeHtml, delegateActions, toast, debounce } from '/static/app.js';
import { formatClock, formatTime, onSSE, serverNow } from '/static/shared/utils.js';
import { createLiveSchedule } from '/static/pages/live-schedule.js';

// ── state ──────────────────────────────────────────────────────────────
let view = null;          // the last /api/inventory/loop answer
let loadError = null;     // why the last read failed, else null
let searchTerm = '';
let flashKey = '';        // the row saved last, flashed once on the next render
let pendingRender = false; // a read landed while a row was being edited
let thrPopOpen = false;   // the UoP threshold column's ⓘ is open
const authenticated = (document.querySelector('.inv-page') || { dataset: {} }).dataset.authenticated === 'true';

// The fold, per table: open shows the healthy rows too. Remembered for the
// tab's session only. try/catch because sessionStorage throws outright in
// some privacy modes, and a remembered fold must never stop the page loading.
const open = { positions: readOpen('positions'), lines: readOpen('lines') };
function readOpen(table) {
  try { return sessionStorage.getItem('inventory:open:' + table) === '1'; } catch (e) { return false; }
}

// Column help: one line per column, on its ⓘ — never in a cell.
const HELP = {
  inloop: 'Every counted unit of the part in the loop: its bins wherever they stand, plus active lineside piles. The bar is that against the UoP threshold, the tick: red past it, amber near it, green clear. Hover a value for bins away from this spot.',
  empty: 'In loop at the line\'s 14-day median running rate. Red at or under the UoP threshold (a one-bin home at a pack or more: when it is empty); — when no running line uses the part, ? when it has no measured rate.',
  threshold: 'Core orders a refill when In loop drops below this; 0 = not monitored. Enter saves, Esc undoes. Hover a value for who set it.',
  ontheway: 'Bins in transit or queued to this spot.',
  inbin: 'Parts in the bin at the line / its pack.',
  left: 'Consume: until the bin at the line is empty. Produce: until it is full. At the line\'s rate over the last 30 minutes; ? when it has not run. Red past the cell reorder point.',
  rp: 'The cell calls its swap at this count: consume at or under it, produce at or over it (0 = when full). Set on the station\'s Edge.',
  lineway: 'Bins on their way to the line: fulls to a consume spot, empties to a produce spot.',
};

// ── data loading ─────────────────────────────────────────────────────────
let lastLoadAt = 0;

async function loadAll(quiet) {
  lastLoadAt = Date.now();
  try {
    view = await apiGet('/api/inventory/loop');
    loadError = null;
  } catch (e) {
    loadError = e;
    if (!quiet) toast('Failed to load inventory: ' + (e.message || e), 'error', { sticky: true });
  }
  if (editing()) { pendingRender = true; return; }
  renderAll();
}

// editing is true while a threshold box holds a value not yet saved: a live
// read must not wipe what someone is typing.
function editing() {
  return Array.from(document.querySelectorAll('.loop-thr-input'))
    .some((el) => el.value !== el.dataset.orig);
}

// ── pure helpers (lifted by inventory.loop.test.js) ──────────────────────

function num(n) { return (n == null || isNaN(n)) ? '—' : Number(n).toLocaleString(); }

// dur is a duration as the floor reads it: "47 min", or "2.3 h" from 90
// minutes up; "—" when there is none.
function dur(seconds) {
  if (seconds == null || isNaN(seconds)) return '—';
  const m = seconds / 60;
  if (m >= 90) return (Math.round(m / 6) / 10).toLocaleString() + ' h';
  return Math.round(m).toLocaleString() + ' min';
}

// timeToEmpty is a position's cover while a running line uses the part; null
// when none does.
function timeToEmpty(r) {
  return r.group ? r.cover_seconds : null;
}

// timeLeft is what a row is sorted by: a position's time to empty, a line's
// time until its bin is empty or full; null when unknown.
function timeLeft(r) {
  return r.kind === 'line' ? r.cover_seconds : timeToEmpty(r);
}

// worstFirst sorts the least time left first and the unknown last; ties by
// line, part, spot.
function worstFirst(a, b) {
  const ta = timeLeft(a), tb = timeLeft(b);
  return (ta == null ? Infinity : ta) - (tb == null ? Infinity : tb)
    || String(a.group || '').localeCompare(String(b.group || ''))
    || String(a.payload_code).localeCompare(String(b.payload_code)) || String(a.node).localeCompare(String(b.node));
}

// foldedBodyHtml is a table's body: the rows that need a look (r.look, Core's
// rule), worst first, then one row-width control for the healthy rest, "Show N
// more". Open, every row shows and the control folds them again. When nothing
// needs a look the body is one line, "All N <noun> healthy", with "Show all
// N": the only place Show all appears, and never an empty body. A search shows
// every match, unfolded.
function foldedBodyHtml(rows, table, cols, nouns, rowHtml) {
  const sorted = rows.slice().sort(worstFirst);
  const all = open[table] || !!searchTerm;
  const look = sorted.filter((r) => r.look);
  const shown = all ? sorted : look;
  const ctl = (label) => '<button type="button" class="btn btn-sm" data-action="toggleFold" data-table="' + table + '">'
    + label + '</button>';
  const foot = (html) => '<tr class="loop-fold"><td colspan="' + cols + '">' + html + '</td></tr>';
  if (!shown.length) {
    const n = sorted.length;
    return foot((n === 1 ? '1 ' + nouns[0] : 'All ' + n + ' ' + nouns[1]) + ' healthy ' + ctl('Show all ' + n));
  }
  let html = shown.map(rowHtml).join('');
  if (searchTerm) return html;
  if (shown.length < sorted.length) html += foot(ctl('Show ' + (sorted.length - shown.length) + ' more'));
  else if (open[table] && look.length < sorted.length) html += foot(ctl('Show fewer'));
  return html;
}

// offHomeTitle names the part's bins standing away from the position, for the
// In loop hover.
function offHomeTitle(list) {
  if (!list || !list.length) return '';
  return list.map((o) => o.bins + (o.bins === 1 ? ' bin' : ' bins') + (o.node ? ' at ' + o.node : ' on a robot')).join('\n');
}

// thrTitle is the threshold's hover: who changed the value, when and from
// what, and the threshold as time at the 14-day median running rate.
function thrTitle(r) {
  const parts = [];
  const p = r.provenance;
  if (p) {
    parts.push(p.source + ' ' + num(p.value) + (p.old != null ? ' (was ' + num(p.old) + ')' : '')
      + (p.by ? ' by ' + p.by : '') + ' on ' + formatTime(p.at) + (p.current ? '' : '; the value has changed since'));
  } else {
    parts.push('set before changes were recorded');
  }
  if (r.cycle_seconds != null) parts.push('≈ ' + dur(r.threshold * r.cycle_seconds) + ' at the 14-day median running rate');
  return parts.join('\n');
}

// doubtHtml is the line above the tables: each bin whose count makes its
// part's sum untrustworthy, linking to its recount door. '' when there are none.
function doubtHtml(doubt) {
  if (!doubt || !doubt.length) return '';
  const items = doubt.map((d) => {
    const what = d.reason === 'below_zero' ? 'reads ' + num(d.uop) : 'not confirmed';
    return '<a href="/bins?open=' + encodeURIComponent(d.bin_id) + '">' + escapeHtml(d.label || ('bin ' + d.bin_id))
      + (d.node ? ' at ' + escapeHtml(d.node) : '') + '</a> ' + escapeHtml(what);
  });
  return '<div class="loop-doubt" role="note">'
    + '<span class="badge badge-warn">' + doubt.length + ' doubtful</span> '
    + items.join(' · ') + '</div>';
}

// hl wraps the current search term in a highlight mark, escaping each piece.
// The match runs on the RAW text: run on the escaped string, a search for
// "amp" or "quot" put the mark inside an entity and broke it.
function hl(text) {
  const raw = text == null ? '' : String(text);
  if (!searchTerm) return escapeHtml(raw);
  const t = searchTerm.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  return raw.split(new RegExp('(' + t + ')', 'ig'))
    .map((part, i) => i % 2 ? '<mark class="inv-hit">' + escapeHtml(part) + '</mark>' : escapeHtml(part))
    .join('');
}

function help(key) {
  return ' <span class="loop-help" tabindex="0" title="' + escapeHtml(HELP[key]) + '">ⓘ</span>';
}

// ── supermarket positions ─────────────────────────────────────────────────

function posHeadHtml() {
  return '<tr>'
    + '<th>Part</th>'
    + '<th>Spot</th>'
    + '<th class="loop-r">In loop (UoP)' + help('inloop') + '</th>'
    + '<th class="loop-r">Time to empty' + help('empty') + '</th>'
    + '<th class="loop-r loop-thr-h">' + thrHeadHtml() + '</th>'
    + '<th class="loop-r">On the way' + help('ontheway') + '</th>'
    + '</tr>';
}

// thrHeadHtml is the UoP threshold header. Its ⓘ opens on click: the column's
// one line and the only way to the calculator, labelled uncalibrated.
function thrHeadHtml() {
  return 'UoP threshold <button type="button" class="loop-help loop-help-btn" data-action="toggleThrPop" aria-expanded="'
    + thrPopOpen + '" title="' + escapeHtml(HELP.threshold) + '">ⓘ</button>'
    + '<div class="loop-pop" id="thr-pop"' + (thrPopOpen ? '' : ' hidden') + '>' + escapeHtml(HELP.threshold)
    + ' <button type="button" class="loop-link" data-action="openCalcPicker">calculator (uncalibrated)</button></div>';
}

// thresholdCell is the input (a plain number when logged out), value only,
// with the one chip a threshold can carry: over what a multi-bin home holds.
function thresholdCell(r) {
  const key = escapeHtml(r.key);
  const title = thrTitle(r);
  const chip = r.state === 'above_home_capacity' ? '<span class="badge badge-warn loop-chip">ABOVE HOME CAPACITY</span> ' : '';
  const value = authenticated
    ? '<input type="number" min="0" step="1" class="loop-thr-input tnum" value="' + r.threshold + '" data-orig="'
      + r.threshold + '" data-key="' + key + '" title="' + escapeHtml(title) + '" aria-label="UoP threshold for '
      + escapeHtml(r.payload_code) + ' at ' + escapeHtml(r.node) + '" data-action-keydown="thrKey" data-action-input="thrInput">'
    : '<span class="tnum" title="' + escapeHtml(title) + '">' + num(r.threshold) + '</span>';
  return '<div class="loop-thr">' + chip + value + '</div>';
}

// meterHtml is the threshold line beside In loop: the in-loop fill against an
// amber tick at the UoP threshold, on a scale of 1.6 thresholds (or a tenth
// past the sum when that is longer). The fill is coral when the row is past its
// threshold (Core's beyond), amber within a fifth above the threshold, green
// clear of it; hatched when the sum is below zero and cannot be read.
function meterHtml(r) {
  if (r.in_loop == null) return '';
  if (r.in_loop < 0) return '<span class="thr-meter thr-meter--err" aria-hidden="true"></span>';
  const thr = r.threshold || 0;
  const scale = Math.max(thr * 1.6, r.in_loop * 1.1, 1);
  const fill = Math.min(100, (r.in_loop / scale) * 100);
  const st = r.beyond ? ' thr-meter--below' : r.in_loop < thr * 1.2 ? ' thr-meter--near' : '';
  let html = '<span class="thr-meter' + st + '" aria-hidden="true"><span class="thr-meter__fill" style="width:'
    + fill.toFixed(1) + '%"></span>';
  if (thr > 0) html += '<span class="thr-meter__tick" style="left:' + Math.min(100, (thr / scale) * 100).toFixed(1) + '%"></span>';
  return html + '</span>';
}

// coming is an On the way cell: the count, a dash when none.
function coming(n) {
  return n > 0 ? num(n) : '—';
}

function posRowHtml(r) {
  const tte = timeToEmpty(r);
  const off = offHomeTitle(r.off_home);
  const cls = 'loop-row' + (r.key === flashKey ? ' loop-flash' : '');
  return '<tr class="' + cls + '" data-key="' + escapeHtml(r.key) + '">'
    + '<td><code>' + hl(r.payload_code) + '</code></td>'
    + '<td>' + hl(r.node) + '</td>'
    + '<td class="loop-r tnum' + (r.in_loop < 0 ? ' loop-short' : '') + '"><span class="loop-inloop">' + meterHtml(r)
      + '<span class="loop-inloop__n">'
      + (off ? '<span class="loop-tip" title="' + escapeHtml(off) + '">' + num(r.in_loop) + '</span>' : num(r.in_loop))
      + '</span></span></td>'
    + '<td class="loop-r tnum' + (r.beyond && tte != null ? ' loop-short' : '') + '">'
      + (!r.group ? '—' : tte == null ? '?' : dur(tte)) + '</td>'
    + '<td class="loop-r">' + thresholdCell(r) + '</td>'
    + '<td class="loop-r tnum">' + coming(r.coming) + '</td>'
    + '</tr>';
}

// posBodyHtml is the positions table body in its default view (foldedBodyHtml).
function posBodyHtml(rows) {
  if (!rows.length) return '<tr><td colspan="6" class="dash-empty">' + (searchTerm ? 'No position matches.' : 'No part is monitored.') + '</td></tr>';
  return foldedBodyHtml(rows, 'positions', 6, ['position', 'positions'], posRowHtml);
}

// ── lines ────────────────────────────────────────────────────────────────

function lineHeadHtml() {
  return '<tr>'
    + '<th>Line</th>'
    + '<th>Part</th>'
    + '<th>Role</th>'
    + '<th class="loop-r">In bin / pack' + help('inbin') + '</th>'
    + '<th class="loop-r">Time left' + help('left') + '</th>'
    + '<th class="loop-r">Cell reorder point' + help('rp') + '</th>'
    + '<th class="loop-r">On the way' + help('lineway') + '</th>'
    + '</tr>';
}

// lineRowHtml is one claim at a line. A line with two spots for one part names
// the spot beside the line, so its rows are told apart.
function lineRowHtml(r, twoSpots) {
  const inBin = r.in_bin == null || !r.pack ? '—' : num(r.in_bin) + ' / ' + num(r.pack);
  const left = r.cover_seconds != null ? dur(r.cover_seconds) : r.in_bin == null ? '—' : '?';
  const where = (r.station ? r.station + '\'s' : 'its') + ' Edge';
  return '<tr class="loop-row" data-key="' + escapeHtml(r.key) + '">'
    + '<td>' + hl(r.group || r.node) + (twoSpots ? ' <span class="text-muted">' + hl(r.node) + '</span>' : '') + '</td>'
    + '<td><code>' + hl(r.payload_code) + '</code></td>'
    + '<td class="loop-role">' + escapeHtml(r.role) + '</td>'
    + '<td class="loop-r tnum"' + (r.in_bin == null ? ' title="no bin at the line"' : '') + '>' + inBin + '</td>'
    + '<td class="loop-r tnum' + (r.beyond && r.cover_seconds != null ? ' loop-short' : '') + '">' + left + '</td>'
    + '<td class="loop-r tnum" title="' + escapeHtml('Set on ' + where) + '">' + num(r.reorder_point) + '</td>'
    + '<td class="loop-r tnum">' + coming(r.coming) + '</td>'
    + '</tr>';
}

// lineBodyHtml is the lines table body in its default view (foldedBodyHtml).
function lineBodyHtml(rows) {
  const spots = {};
  const at = (r) => r.group + '|' + r.payload_code + '|' + r.role;
  rows.forEach((r) => { spots[at(r)] = (spots[at(r)] || 0) + 1; });
  return foldedBodyHtml(rows, 'lines', 7, ['line', 'lines'], (r) => lineRowHtml(r, spots[at(r)] > 1));
}

function matches(r) {
  if (!searchTerm) return true;
  const t = searchTerm.toLowerCase();
  return [r.payload_code, r.description, r.node, r.group]
    .some((v) => String(v || '').toLowerCase().includes(t));
}

// ── rendering ─────────────────────────────────────────────────────────────
function renderAll() {
  pendingRender = false;
  const posBody = document.getElementById('pos-body');
  const lineBody = document.getElementById('line-body');
  const lines = document.getElementById('lines');
  const doubt = document.getElementById('loop-doubt');
  if (!posBody) return;
  if (!view) {
    posBody.innerHTML = '<tr><td colspan="6" class="dash-empty">Could not load — '
      + escapeHtml(String(loadError ? (loadError.message || loadError) : 'no answer'))
      + ' <button class="btn btn-sm" data-action="refresh">Retry</button></td></tr>';
    return;
  }
  const rows = (view.rows || []).filter(matches);
  document.getElementById('pos-head').innerHTML = posHeadHtml();
  posBody.innerHTML = posBodyHtml(rows.filter((r) => r.kind !== 'line'));
  const lineRows = rows.filter((r) => r.kind === 'line');
  if (lines) lines.hidden = !lineRows.length;
  document.getElementById('line-head').innerHTML = lineHeadHtml();
  if (lineBody) lineBody.innerHTML = lineBodyHtml(lineRows);
  if (doubt) doubt.innerHTML = doubtHtml(view.doubt);
  const asof = document.getElementById('inv-asof');
  if (asof) asof.textContent = 'as of ' + formatClock(serverNow());
  flashKey = '';
}

function rowByKey(key) { return ((view && view.rows) || []).find((r) => r.key === key); }

// ── threshold edit ───────────────────────────────────────────────────────
function thrInput(el) {
  const tr = el.closest('tr');
  if (tr) tr.classList.toggle('loop-row--dirty', el.value !== el.dataset.orig);
}

function thrKey(el, ev) {
  const e = ev || window.event;
  if (!e) return;
  if (e.key === 'Enter') {
    e.preventDefault();
    saveThreshold(el.dataset.key, el.value, 'manual');
  } else if (e.key === 'Escape') {
    el.value = el.dataset.orig;
    thrInput(el);
    el.blur();
  }
}

// saveThreshold writes one row's UoP threshold through the loader config's
// door for its layout: a dedicated home by its position, a shared window
// loader by its part. source is "manual" from the row, and from the calculator
// "calculated" when its suggestion was confirmed as it stood.
async function saveThreshold(key, value, source) {
  const r = rowByKey(key);
  if (!r || r.kind !== 'loader') return;
  const thr = Number(value);
  if (!Number.isInteger(thr) || thr < 0) { toast('A UoP threshold is a whole number, 0 or more', 'warning'); return; }
  try {
    if (r.layout === 'dedicated_positions') {
      await apiPost('/api/loader/set-home', { loader_id: r.loader_id, position_node_id: r.position_node_id,
        payload_code: r.payload_code, uop_threshold: thr, source });
    } else {
      await apiPost('/api/loader/set-payload', { loader_id: r.loader_id, payload_code: r.payload_code,
        uop_threshold: thr, source });
    }
    flashKey = key;
    document.querySelectorAll('.loop-thr-input[data-key="' + CSS.escape(key) + '"]').forEach((el) => { el.dataset.orig = String(thr); el.value = String(thr); });
    await loadAll(true);
  } catch (e) {
    toast('Save failed: ' + (e.message || e), 'error', { sticky: true });
  }
}

// toggleThrPop opens or closes the UoP threshold column's ⓘ.
function toggleThrPop() {
  thrPopOpen = !thrPopOpen;
  const pop = document.getElementById('thr-pop');
  if (pop) pop.hidden = !thrPopOpen;
}

// calcRows are the positions the calculator can work on, least time left first.
function calcRows() {
  return ((view && view.rows) || []).filter((r) => r.kind === 'loader' && r.inputs).sort(worstFirst);
}

// openCalcPicker opens the calculator on the worst position; its picker
// moves it to another. A value it suggests saves only on Confirm (the near
// miss of 2026-10-29: a stop's tail made suggestions of 1, one click from the
// live threshold).
function openCalcPicker() {
  const rows = calcRows();
  if (!rows.length) { toast('No position has measured refill times yet', 'info'); return; }
  openCalc(rows[0].key);
}

function pickCalcRow(el) { openCalc(el.value); }

// ── the calculator's inputs ──────────────────────────────────────────────
let calcKey = '';
let calcSuggested = null; // the value the modal's calculator last suggested

function secs(v) { return v == null ? '—' : num(Math.round(v)) + ' s'; }

// openCalc opens the calculator's inputs for a row, the suggestion filled in
// and, with a session, one Confirm to save it.
function openCalc(key) {
  const r = rowByKey(key);
  if (!r || !r.inputs) return;
  calcKey = key;
  calcSuggested = r.suggested;
  const i = r.inputs;
  const title = document.getElementById('loop-calc-title');
  if (title) title.textContent = 'Calculator (uncalibrated)';
  const pick = '<label class="loop-calc-pick">Position <select id="calc-row" data-action-input="pickCalcRow">'
    + calcRows().map((o) => '<option value="' + escapeHtml(o.key) + '"' + (o.key === key ? ' selected' : '') + '>'
      + escapeHtml(o.payload_code + ' at ' + o.node) + '</option>').join('') + '</select></label>';
  const days = (view && view.lead_days) || 14;
  const cyc = r.cycle_seconds != null ? (Math.round(r.cycle_seconds * 10) / 10) : '';
  const field = (label, val) => '<tr><th>' + escapeHtml(label) + '</th><td class="loop-r tnum">' + val + '</td></tr>';
  const body = document.getElementById('loop-calc-body');
  body.innerHTML = pick + '<table class="loop-calc-table"><tbody>'
    + '<tr><th>Cycle (s)</th><td class="loop-r"><input type="number" min="0" step="0.1" id="calc-cycle" class="tnum" value="' + cyc + '"></td></tr>'
    + field('L1 queue', secs(i.l1_queue_seconds))
    + field('L1 transit', secs(i.l1_transit_seconds))
    + field('L2 load (median)', secs(i.l2_load_seconds))
    + field('L2 transit', secs(0))
    + field('Market → cell (p95)', secs(i.market_to_cell_seconds))
    + '<tr><th>Safety factor</th><td class="loop-r"><input type="number" min="0" step="0.1" id="calc-safety" class="tnum" value="' + ((view && view.safety) || 1.5) + '"></td></tr>'
    + field('Completed L1 cycles, this position', num(r.completed_l1))
    + field('Completed L1 cycles, the part', num(i.samples_l1))
    + field('Completed retrieves', num(i.samples_retrieve))
    + '</tbody></table>'
    + '<div class="loop-sub">Observed over ' + days + ' days. The cycle is the 14-day median running rate\'s. '
    + 'L2 transit is 0: no store orders exist.</div>'
    + '<div id="calc-out" class="loop-calc-out">' + (r.suggested == null
      ? 'No suggestion: no completed refill at this position in ' + days + ' days.'
      : calcOutHtml(r.suggested, r.confidence)) + '</div>'
    + (authenticated ? '<div class="loop-calc-actions">'
      + '<button type="button" class="btn btn-sm" data-action="runCalc">Recalculate</button> '
      + '<button type="button" class="btn btn-sm btn-primary" data-action="confirmCalc">Confirm</button></div>' : '');
  document.getElementById('loop-calc').classList.add('active');
}

// calcOutHtml is the modal's suggestion: the value to save (an input with a
// session, prefilled) and the confidence.
function calcOutHtml(value, confidence) {
  if (value == null) return '';
  const v = authenticated
    ? '<input type="number" min="0" step="1" id="calc-value" class="tnum" value="' + value + '" aria-label="UoP threshold to save">'
    : '<b class="tnum">' + num(value) + '</b>';
  return 'Suggested UoP threshold ' + v + ' · confidence ' + escapeHtml(confidence || '');
}

async function runCalc() {
  const r = rowByKey(calcKey);
  if (!r) return;
  const cycle = Number((document.getElementById('calc-cycle') || {}).value);
  if (!(cycle > 0)) { toast('Enter the cycle in seconds', 'warning'); return; }
  const out = document.getElementById('calc-out');
  try {
    const d = await apiPost('/api/loader/calculate', {
      core_node_name: r.node, payload_code: r.payload_code, cycle_seconds: cycle, days: (view && view.lead_days) || 14,
      safety_factor: Number((document.getElementById('calc-safety') || {}).value) || 0,
    });
    const l1 = d && d.outputs ? d.outputs.L1Threshold : null;
    calcSuggested = l1;
    if (out) out.innerHTML = calcOutHtml(l1, d && d.confidence);
  } catch (e) {
    toast('Calculate failed: ' + (e.message || e), 'error');
  }
}

// confirmCalc saves the modal's value: "calculated" when it is the
// calculator's suggestion as it stood, "manual" when it was typed over.
function confirmCalc() {
  const el = document.getElementById('calc-value');
  if (!el) return;
  const value = Number(el.value);
  closeCalc();
  saveThreshold(calcKey, value, value === calcSuggested ? 'calculated' : 'manual');
}

function closeCalc() { const m = document.getElementById('loop-calc'); if (m) m.classList.remove('active'); }

// ── interactions ───────────────────────────────────────────────────────────
const onSearch = debounce((el) => { searchTerm = (el.value || '').trim(); renderAll(); }, 150);
function refresh() { loadAll(); }
// toggleFold opens or folds one table (the control's data-table) and
// remembers it for the session.
function toggleFold(el) {
  const table = el && el.dataset ? el.dataset.table : '';
  if (!(table in open)) return;
  open[table] = !open[table];
  try { sessionStorage.setItem('inventory:open:' + table, open[table] ? '1' : '0'); } catch (e) { /* not remembered: the fold still works */ }
  renderAll();
}
function exportInventory() { window.location = '/api/inventory/export'; }

delegateActions(document.body, {
  onSearch, refresh, toggleFold, exportInventory, thrKey, thrInput, toggleThrPop, openCalcPicker, pickCalcRow,
  runCalc, confirmCalc, closeCalc,
}, { events: ['click', 'input', 'keydown'] });

// '/' focuses the search box from anywhere (unless already typing in a field).
document.addEventListener('keydown', (e) => {
  if (e.key === '/' && !/^(INPUT|TEXTAREA|SELECT)$/.test((e.target.tagName || ''))) {
    e.preventDefault();
    const s = document.getElementById('inv-search');
    if (s) s.focus();
  } else if (e.key === 'Escape') {
    closeCalc();
    if (thrPopOpen) toggleThrPop();
  }
});
// A read that landed mid-edit renders once the box is left.
document.addEventListener('focusout', () => { setTimeout(() => { if (pendingRender && !editing()) renderAll(); }, 0); });

// Live: order events re-read the loop, a burst being one read after it
// settles; a hidden tab reads nothing and catches up once when shown; a
// minute's interval keeps the stamp honest without traffic. The read carries
// the calculator's 14-day aggregate, so it is paced by events and the minute,
// not polled harder.
const FALLBACK_MS = 60000;
const live = document.getElementById('inv-live');
function markLive() { if (live) { live.textContent = 'live'; live.classList.add('is-live'); } }
const reload = createLiveSchedule(() => loadAll(true), { debounceMs: 5000, maxWaitMs: 30000 });
onSSE('order-update', () => { markLive(); reload.kick(); });
let connectedOnce = false;
onSSE('connected', () => {
  markLive();
  if (connectedOnce) reload.kick();
  connectedOnce = true;
});
setInterval(() => {
  if (reload.hidden()) { reload.markStale(); return; }
  if (Date.now() - lastLoadAt >= FALLBACK_MS - 1000) loadAll(true);
}, FALLBACK_MS);

loadAll();
