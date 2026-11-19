// The Robots page's Device Config tab: every robot's device configuration
// side by side — the fleet matrix over stored #1500 pulls.
//
// ALL COMPUTATION IS SERVER-SIDE. GET /api/robots/device-config returns
// rows with per-robot cell STATES (ok/differs/calibration/missing/absent/
// absent_differs) plus the plurality and counts; this file renders states,
// never recomputes them — the plurality/absence/tie rules are the hardest
// logic in the feature and belong where the Go suite can pin them.
//
// The scan/dig model: all sections collapsed on open; a collapsed cell is
// "-" plain when the fleet agrees under it, a red cell box when some
// subfield differs for that robot (the tooltip names the subfields);
// clicking a section expands subsections (their own collapsed rows),
// clicking a subsection expands leaves. The filter box auto-expands
// matches. Credential rows render •••••••• — compared, never stored or
// shown (the fingerprint is computed server-side and never ships).
//
// Pull: the browser loops robots 200 ms apart, one POST each (the
// server also enforces the gap — it is a server property, not client
// goodwill), then reloads the matrix once: the round-1 cut, no streaming.
import { escapeHtml } from '/static/shared/dom.js';
import { send } from '/static/shared/http.js';
import { plantDate } from '/static/components/plantclock.js';

const root = () => document.getElementById('deviceconfig-root');

let matrix = null;
const expandedSections = new Set();
const expandedSubs = new Set(); // "section|subsection"
let filterText = '';
let pulling = false;

// ── data actions (bound in pages/robots.js) ──────────────────────────────

export async function loadDeviceConfig() {
  const out = await send('GET', '/api/robots/device-config', null, { notice: false });
  if (!out.ok) { renderError(out.error); return; }
  matrix = out.body;
  render();
}

export function toggleDeviceConfigSection(arg) {
  const [section, sub] = String(arg || '').split('|');
  if (!section) return;
  if (sub === undefined) {
    expandedSections.has(section) ? expandedSections.delete(section) : expandedSections.add(section);
    if (!expandedSections.has(section)) {
      for (const k of [...expandedSubs]) if (k.startsWith(section + '|')) expandedSubs.delete(k);
    }
  } else {
    const key = section + '|' + sub;
    expandedSubs.has(key) ? expandedSubs.delete(key) : expandedSubs.add(key);
  }
  render();
}

export function filterDeviceConfig(el) {
  filterText = (el.value || '').toLowerCase();
  render();
}

export function toggleAllDeviceConfig(expand) {
  if (!expand) { expandedSections.clear(); expandedSubs.clear(); render(); return; }
  group().sections.forEach(s => {
    expandedSections.add(s.name);
    s.subs.forEach(x => expandedSubs.add(s.name + '|' + x.name));
  });
  render();
}

export async function pullFleetDeviceConfig() {
  if (!matrix || pulling) return;
  pulling = true;
  const line = (t) => { const el = document.getElementById('dc-pull-status'); if (el) el.textContent = t; };
  try {
    for (const r of matrix.robots) {
      line('Pulling ' + r.vehicle + '…');
      const out = await send('POST', '/api/robots/device-config/pull', { vehicle: r.vehicle },
        { timeout: 60000, notice: false });
      line(out.ok ? (r.vehicle + ': ' + (out.body && out.body.params || '?') + ' params')
                  : (r.vehicle + ': ' + out.error));
      await new Promise(res => setTimeout(res, 200)); // the brief's pacing; the server enforces it too
    }
    line('Reloading matrix…');
    await loadDeviceConfig();
    line('Pull complete');
  } finally { pulling = false; }
}

// ── grouping and rollups ──────────────────────────────────────────────────

function group() {
  const secs = new Map(); // name -> {name, subs: Map(name -> {name, rows: []}), rows: []}
  (matrix.rows || []).forEach(r => {
    if (!secs.has(r.section)) secs.set(r.section, { name: r.section, subs: new Map(), rows: [] });
    const s = secs.get(r.section);
    if (!s.subs.has(r.subsection)) s.subs.set(r.subsection, { name: r.subsection, rows: [] });
    const sub = s.subs.get(r.subsection);
    sub.rows.push(r); s.rows.push(r);
  });
  return { sections: [...secs.values()].map(s => ({ ...s, subs: [...s.subs.values()] })) };
}

// rollup(rows, vehicle): 'red' | 'blue' | 'plain' — red beats blue beats
// plain (the UI spec's collapsed order), and the differing subfields ride
// along for the tooltip.
function rollup(rows, vehicle) {
  let red = [], blue = false;
  rows.forEach(r => {
    const c = (r.cells || {})[vehicle];
    if (!c) return;
    if (c.state === 'differs' || c.state === 'absent_differs') red.push(r.path);
    else if (c.state === 'calibration') blue = true;
  });
  return red.length ? { k: 'red', diff: red } : blue ? { k: 'blue', diff: [] } : { k: 'plain', diff: [] };
}

// ── rendering ─────────────────────────────────────────────────────────────

function render() {
  const el = root();
  if (!el || !matrix) return;
  injectStyles();
  const g = group();
  const robots = matrix.robots || [];
  const canEdit = el.dataset.canEdit === '1';

  let h = '<div class="flex flex-between mb-1 flex-wrap gap-1">'
    + '<div class="flex gap-1 flex-wrap">'
    + '<input class="set-inp" id="dc-filter" placeholder="Filter params…" value="' + escapeHtml(filterText) + '" data-action-input="filterDeviceConfig" data-action="filterDeviceConfig">'
    + '<button type="button" class="btn btn-sm" data-action="toggleAllDeviceConfig:true">Expand all</button>'
    + '<button type="button" class="btn btn-sm" data-action="toggleAllDeviceConfig:false">Collapse all</button>'
    + '</div>'
    + (canEdit ? '<button type="button" class="btn btn-sm btn-primary" data-action="pullFleetDeviceConfig">Pull fleet</button>' : '')
    + '</div>'
    + '<div class="text-muted text-sm mb-1" id="dc-pull-status"></div>'
    + '<div class="dc-legend text-muted text-sm mb-1">red = differs from fleet · yellow = no value from pull · light blue = device-specific (calibration) value · •••••••• = credential — compared, never stored or shown · click a section to expand/collapse</div>'
    + '<div class="dc-scroll"><table class="dc-table"><thead><tr>'
    + '<th class="dc-lead0">Section</th><th class="dc-lead1">Subsection</th><th class="dc-lead2">Parameter</th>'
    + robots.map(r => '<th class="dc-robot">' + escapeHtml(r.vehicle)
        + '<div class="dc-date">' + (r.has_pull ? mmddyy(r.pulled_at) : 'never') + '</div></th>').join('')
    + '</tr></thead><tbody>';

  for (const s of g.sections) {
    const secOpen = expandedSections.has(s.name);
    const f = filterText ? s.rows.some(matchesFilter) : false;
    // section row (always visible)
    h += '<tr class="dc-sec' + (filterText && !f ? ' dc-hidden' : '') + '">'
      + '<td class="dc-lead0"><button type="button" class="dc-toggle" data-action="toggleDeviceConfigSection:' + escapeHtml(s.name) + '">' + (secOpen ? '▾' : '▸') + ' ' + escapeHtml(s.name) + '</button></td>'
      + '<td class="dc-lead1"></td><td class="dc-lead2">' + s.subs.length + ' subsections · ' + s.rows.length + ' rows</td>'
      + robots.map(r => { const u = rollup(s.rows, r.vehicle); return rollupCell(u, s.name); }).join('')
      + '</tr>';
    if (!secOpen && !f) continue;
    for (const sub of s.subs) {
      const key = s.name + '|' + sub.name;
      const subOpen = expandedSubs.has(key) || (filterText && sub.rows.some(matchesFilter));
      h += '<tr class="dc-sub">'
        + '<td class="dc-lead0"></td>'
        + '<td class="dc-lead1"><button type="button" class="dc-toggle" data-action="toggleDeviceConfigSection:' + escapeHtml(s.name + '|' + sub.name) + '">' + (subOpen ? '▾' : '▸') + ' ' + escapeHtml(sub.name) + '</button></td>'
        + '<td class="dc-lead2">' + sub.rows.length + ' rows</td>'
        + robots.map(r => { const u = rollup(sub.rows, r.vehicle); return rollupCell(u, key); }).join('')
        + '</tr>';
      if (!subOpen) continue;
      for (const row of sub.rows) {
        if (filterText && !matchesFilter(row)) continue;
        h += '<tr class="dc-leaf">'
          + '<td class="dc-lead0"></td><td class="dc-lead1"></td>'
          + '<td class="dc-lead2">' + escapeHtml(row.path) + '</td>'
          + robots.map(r => leafCell(row, r.vehicle)).join('')
          + '</tr>';
      }
    }
  }
  h += '</tbody></table></div>';
  el.innerHTML = h;
}

function matchesFilter(row) {
  return (row.section + ' ' + row.subsection + ' ' + row.path).toLowerCase().indexOf(filterText) >= 0;
}

function rollupCell(u, tooltipKey) {
  const cls = u.k === 'red' ? 'dc-red' : u.k === 'blue' ? 'dc-blue' : '';
  const tip = u.k === 'red' ? ' title="differs under here: ' + escapeHtml(u.diff.slice(0, 10).join(', ')) + (u.diff.length > 10 ? '…' : '') + '"' : '';
  return '<td class="dc-cell ' + cls + '"' + tip + '>–</td>';
}

function leafCell(row, vehicle) {
  const c = (row.cells || {})[vehicle] || { state: 'missing' };
  let cls = '', text = '–', tip = [];
  switch (c.state) {
    case 'ok': text = c.value; break;
    case 'differs': cls = 'dc-red'; text = c.value; break;
    case 'calibration': cls = 'dc-blue'; text = c.value; break;
    case 'missing': cls = 'dc-yellow'; text = ''; break;
    case 'absent': text = '–'; break;
    case 'absent_differs': cls = 'dc-red'; text = '–'; break;
    default: text = c.value || '';
  }
  if (row.credential) { text = '••••••••'; }
  tip.push(row.credential
    ? 'credential — compared, never stored or shown; ' + (c.state === 'differs' ? 'differs from fleet' : c.state === 'ok' ? 'agrees with fleet' : c.state)
    : (c.value === '' && text === '–' ? 'absent' : c.value));
  if (row.type) tip.push(row.type + (row.unit ? ' ' + row.unit : '') + (row.range ? ' (' + row.range + ')' : ''));
  if (row.desc) tip.push(row.desc);
  tip.push(row.tie ? 'fleet is split — no fleet value' : fleetLine(row));
  return '<td class="dc-cell ' + cls + '" title="' + escapeHtml(tip.join(' · ')) + '">' + escapeHtml(text) + '</td>';
}

function fleetLine(row) {
  const parts = Object.entries(row.counts || {})
    .sort((a, b) => b[1] - a[1])
    .map(([v, n]) => v + ' ×' + n);
  return 'fleet: ' + (row.plurality || '(none)') + ' [' + parts.join(', ') + ']';
}

// mm/dd/yy, date only, PLANT-LOCAL (the brief's rule). plantDate gives the
// plant's calendar date as a bare string; the rest is string arithmetic, so
// no browser-calendar read ever happens (clock drift test's rule).
function mmddyy(ts) {
  const [y, m, d] = plantDate(ts).split('-');
  if (!y) return '';
  return Number(m) + '/' + Number(d) + '/' + y.slice(2);
}

function renderError(msg) {
  const el = root();
  if (el) el.innerHTML = '<div class="text-muted">Device Config unavailable: ' + escapeHtml(msg || 'no answer') + '</div>';
}

// One-time styles: sticky lead columns, the state colours, the toggle.
// Colour notes: red/yellow/blue cell boxes per the UI spec; a dash in a red
// box is an absent-compares-red, a value in a blue box a calibration diff.
function injectStyles() {
  if (document.getElementById('dc-styles')) return;
  const s = document.createElement('style');
  s.id = 'dc-styles';
  s.textContent = `
.dc-scroll { overflow-x: auto; max-height: 70vh; overflow-y: auto; border: 1px solid var(--line, #ccc); }
.dc-table { border-collapse: collapse; font-size: 0.8rem; white-space: nowrap; }
.dc-table th, .dc-table td { padding: 2px 8px; border-bottom: 1px solid var(--line, #eee); text-align: left; }
.dc-table thead th { position: sticky; top: 0; background: var(--panel, #fff); z-index: var(--z-sticky); }
.dc-lead0 { position: sticky; left: 0; background: inherit; z-index: var(--z-raised); min-width: 9rem; }
.dc-lead1 { position: sticky; left: 9rem; background: inherit; z-index: var(--z-raised); min-width: 8rem; }
.dc-lead2 { position: sticky; left: 17rem; background: inherit; z-index: var(--z-raised); min-width: 12rem; }
.dc-robot { min-width: 5.5rem; }
.dc-date { font-weight: normal; font-size: 0.7rem; color: var(--muted, #888); }
.dc-toggle { background: none; border: none; cursor: pointer; padding: 0; font: inherit; text-align: left; }
.dc-sec .dc-lead0, .dc-sub .dc-lead1 { font-weight: 600; }
.dc-cell.dc-red { background: #c4453c; color: #fff; }
.dc-cell.dc-yellow { background: #e6c229; }
.dc-cell.dc-blue { background: #8fbcd4; }
.dc-hidden { display: none; }
`;
  document.head.appendChild(s);
}
