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
//
// The tab's styles live in robots.html's page-local <style> block (the
// Phase 6 convention) — NOT injected here: a JS-built stylesheet is the
// one shape css_collision_drift_test.go cannot see, and the state colours
// are --dc-* tokens in shared/tokens.css so dark theme comes free.
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
  // The colon arg arrives as the STRING "false" (delegateActions dispatch),
  // and !expand would read it as true — Collapse all expanded everything.
  const on = expand === true || expand === 'true';
  if (!on) { expandedSections.clear(); expandedSubs.clear(); render(); return; }
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
    for (let i = 0; i < matrix.robots.length; i++) {
      const r = matrix.robots[i];
      line('Pulling ' + r.vehicle + '… (' + (i + 1) + ' of ' + matrix.robots.length + ')');
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
  const g = group();
  const robots = matrix.robots || [];
  const canEdit = el.dataset.canEdit === '1';

  // The toolbar is built ONCE and never rebuilt: rebuilding it on every
  // render dropped the filter box's focus mid-word, and replacing the
  // .dc-scroll box reset its scroll to 0/0 on every expand. Only the
  // table inside the persistent box is replaced now — the input keeps its
  // focus and caret, and the scroller keeps its position.
  if (!document.getElementById('dc-scroll')) {
    // Toolbar order (Eric, 2026-11-20): buttons, legend, pull status, Pull
    // fleet — the legend and the status live IN the bar, between the
    // Collapse-all and Pull-fleet buttons, not above the table.
    el.innerHTML = '<div class="dc-toolbar flex flex-between mb-1 flex-wrap gap-1">'
      + '<div class="flex gap-1 flex-wrap">'
      + '<input class="set-inp" id="dc-filter" placeholder="Filter params…" value="' + escapeHtml(filterText) + '" data-action-input="filterDeviceConfig" data-action="filterDeviceConfig">'
      + '<button type="button" class="btn btn-sm" data-action="toggleAllDeviceConfig:true">Expand all</button>'
      + '<button type="button" class="btn btn-sm" data-action="toggleAllDeviceConfig:false">Collapse all</button>'
      + '</div>'
      + '<div class="dc-legend">'
      + '<div><span class="dc-cell dc-red">red</span> = differs from fleet</div>'
      + '<div><span class="dc-cell dc-yellow">yellow</span> = no value from pull</div>'
      + '<div><span class="dc-cell dc-blue">light blue</span> = device-specific (calibration) value</div>'
      + '<div><span class="dc-cell">••••••••</span> = credential — compared, never stored or shown</div>'
      + '<div>click a section to expand / collapse</div>'
      + '</div>'
      + '<div class="text-muted text-sm" id="dc-pull-status"></div>'
      + (canEdit && matrix.can_pull
          ? '<button type="button" class="btn btn-sm btn-primary" data-action="pullFleetDeviceConfig">Pull fleet</button>'
          : '')
      + '</div>'
      + '<div class="dc-scroll" id="dc-scroll"></div>';
  }
  const head = '<table class="dc-table"><thead><tr>'
      + '<th class="dc-lead0">Section</th><th class="dc-lead1">Subsection</th><th class="dc-lead2">Parameter</th>'
      + robots.map(r => '<th class="dc-robot">' + escapeHtml(r.vehicle)
          + '<div class="dc-date">' + (r.has_pull ? mmddyy(r.pulled_at) : 'never') + '</div></th>').join('')
      + '</tr></thead><tbody>';

  let h = '';
  for (const s of g.sections) {
    const secOpen = expandedSections.has(s.name);
    const f = filterText ? s.rows.some(matchesFilter) : false;
    // section row (always visible)
    h += '<tr class="dc-sec' + (filterText && !f ? ' dc-hidden' : '') + '">'
      + '<td class="dc-lead0"><button type="button" class="dc-toggle" data-action="toggleDeviceConfigSection:' + escapeHtml(s.name) + '">' + (secOpen ? '▾' : '▸') + ' ' + escapeHtml(s.name) + '</button></td>'
      + '<td class="dc-lead1"></td><td class="dc-lead2">' + s.subs.length + ' subsections · ' + s.rows.length + ' rows</td>'
      + robots.map(r => { const u = rollup(s.rows, r.vehicle); return rollupCell(u); }).join('')
      + '</tr>';
    if (!secOpen && !f) continue;
    for (const sub of s.subs) {
      const key = s.name + '|' + sub.name;
      const subOpen = expandedSubs.has(key) || (filterText && sub.rows.some(matchesFilter));
      h += '<tr class="dc-sub">'
        + '<td class="dc-lead0"></td>'
        + '<td class="dc-lead1"><button type="button" class="dc-toggle" data-action="toggleDeviceConfigSection:' + escapeHtml(s.name + '|' + sub.name) + '">' + (subOpen ? '▾' : '▸') + ' ' + escapeHtml(sub.name) + '</button></td>'
        + '<td class="dc-lead2">' + sub.rows.length + ' rows</td>'
        + robots.map(r => { const u = rollup(sub.rows, r.vehicle); return rollupCell(u); }).join('')
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
  document.getElementById('dc-scroll').innerHTML = head + h + '</tbody></table>';
}

function matchesFilter(row) {
  return (row.section + ' ' + row.subsection + ' ' + row.path).toLowerCase().indexOf(filterText) >= 0;
}

function rollupCell(u) {
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
  // Mask only VALUED credential cells: yellow stays yellow (no pull) and an
  // absent credential stays a dash — masking those would hide the state the
  // colours exist to show (round-2, argent-condor).
  if (row.credential && (c.state === 'ok' || c.state === 'differs' || c.state === 'calibration')) {
    text = '••••••••';
  }
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
