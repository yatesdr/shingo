// The Robots page's Cleaning tab: the clean mission's setup block, a line per
// robot, and the mission history.
//
// The setup is a §33 block on the .set- names, bound with settingsPage: one
// draft and one Save, because a half-typed point must never be applied (the
// stated exception to save-on-change, §33.10). It posts to its own door,
// POST /api/robots/cleaning/setup, whose refusal is the config page's shape,
// so a bad point's message lands under that point's row (points.<i>).
//
// The point boxes offer the scene's points as the person types: /api/map/marks
// is the picker's slim, server-searched view of the scene (the same points as
// /api/map/points without the vendor property blob). Every point is offered,
// not just LocationMarks: the knife could sit anywhere, and the server refuses
// what is not a scene point or is a lane slot.
import { apiResult, collectList, debounce, escapeHtml, formatTime, settingsPage } from '/static/shared/utils.js';

const root = () => document.getElementById('cleaning-setup');
const pointsBox = () => document.getElementById('cleaning-points');

// A setup never saved starts at the column's 25 % floor; frequency is left for
// the person to choose. There are no active hours: the loop runs around the clock.
const NEVER_SAVED = { points: [], frequency: '', min_battery: '25' };

let page = null;

function timeHTML(ts) {
  if (!ts) return '—';
  return '<time data-utc="' + escapeHtml(ts) + '">' + escapeHtml(formatTime(ts)) + '</time>';
}

// ── the points list (§33.9: rows in the value column, Remove on the row,
//    Add after the last; read from the rows on the page, never by index) ──

function pointRow(value) {
  const row = document.createElement('div');
  row.className = 'v';
  row.dataset.row = 'point';
  row.innerHTML = '<span class="set-dim tnum" data-point-no></span>' +
    '<input class="set-inp mid" type="text" list="scene-points" spellcheck="false" autocomplete="off" ' +
    'placeholder="LM513" aria-label="Move point" data-action-input="suggestPoints" value="' + escapeHtml(value || '') + '">' +
    '<button type="button" class="set-btn quiet" data-action="removeCleaningPoint">Remove</button>';
  return row;
}

function numberRows() {
  let n = 0;
  pointsBox().querySelectorAll('[data-row="point"] [data-point-no]').forEach((s) => { s.textContent = 'Move point ' + (++n); });
}

function readPoints() {
  // The server numbers a bad point by its place among the non-blank ones sent.
  let i = 0;
  return collectList(pointsBox(), '[data-row="point"]', (row) => {
    const inp = row.querySelector('input');
    const v = inp.value.trim();
    if (v) inp.dataset.field = 'points.' + (i++);
    else delete inp.dataset.field;
    return v;
  });
}

function writePoints(points) {
  const box = pointsBox();
  box.querySelectorAll('[data-row="point"]').forEach((r) => r.remove());
  const add = box.lastElementChild;
  (points || []).forEach((p) => box.insertBefore(pointRow(p), add));
  numberRows();
}

export function addCleaningPoint() {
  const box = pointsBox();
  const row = pointRow('');
  box.insertBefore(row, box.lastElementChild);
  numberRows();
  refreshShown();
  row.querySelector('input').focus();
}

export function removeCleaningPoint(btn) {
  const row = btn.closest('[data-row="point"]');
  if (row) row.remove();
  numberRows();
  refreshShown();
  if (page) page.update();
}

// suggestPoints fills the shared datalist from the scene as a point is typed.
export const suggestPoints = debounce(function (el) {
  const q = (el && el.value ? el.value : '').trim();
  fetch('/api/map/marks?q=' + encodeURIComponent(q))
    .then((r) => r.json())
    .then((d) => {
      const list = document.getElementById('scene-points');
      if (!list) return;
      list.innerHTML = (d.marks || []).map((m) =>
        '<option value="' + escapeHtml(m.name) + '">' + escapeHtml([m.label, m.class].filter(Boolean).join(' · ')) + '</option>').join('');
    })
    .catch(() => {});
}, 200);

// ── the rest of the block ──

function segValue() {
  const on = root().querySelector('[data-seg="frequency"] button.on');
  return on ? on.dataset.val : '';
}

function setSeg(value) {
  root().querySelectorAll('[data-seg="frequency"] button').forEach((b) => {
    const on = b.dataset.val === value;
    b.classList.toggle('on', on);
    b.setAttribute('aria-pressed', on ? 'true' : 'false');
  });
}

// §33.7: frequency and battery hide while there is no point.
function refreshShown() {
  const none = readPoints().length === 0;
  root().querySelectorAll('[data-needs-points]').forEach((row) => { row.hidden = none; });
}

function read() {
  return {
    points: readPoints(),
    frequency: segValue(),
    min_battery: document.getElementById('cleaning-battery').value.trim(),
  };
}

function write(v) {
  writePoints(v.points);
  setSeg(v.frequency);
  document.getElementById('cleaning-battery').value = v.min_battery;
  refreshShown();
}

function validate(d) {
  const e = {};
  if (d.min_battery === '' || !/^\d+$/.test(d.min_battery) || +d.min_battery > 100) e.min_battery = 'A percent, 0 to 100.';
  return Object.keys(e).length ? e : null;
}

function save(d) {
  return apiResult('POST', '/api/robots/cleaning/setup', {
    points: d.points, frequency: d.frequency, min_battery: parseInt(d.min_battery, 10),
  }).then((r) => {
    const b = (r.body && typeof r.body === 'object') ? r.body : {};
    if (r.ok && b.ok !== false) loadCleaning();
    return { ok: r.ok && b.ok !== false, errors: b.errors, error: b.error };
  });
}

// ── what the server says ──

function drawWords(words) {
  const box = document.getElementById('cleaning-words');
  if (!box) return;
  box.className = 'set-cnt' + (words === 'running' ? ' ok' : (words === 'off' ? '' : ' warn'));
  box.querySelector('span').textContent = words;
}

function drawRobots(robots) {
  const body = document.getElementById('cleaning-robots');
  if (!body) return;
  if (!robots.length) {
    body.innerHTML = '<tr><td colspan="5" class="text-muted">No robots known yet.</td></tr>';
    return;
  }
  // The Cleaning column is the robot's opt-out as a switch, saved the moment
  // it is flipped (§11), through the same door as the pop-up's switch
  // (robots.js robotCleanOptIn). Signed out it is shown and disabled.
  const canEdit = body.dataset.canEdit === '1';
  body.innerHTML = robots.map((r) => {
    const on = !r.opted_out;
    return '<tr><td><strong>' + escapeHtml(r.vehicle_id) + '</strong></td>' +
      '<td>' + timeHTML(r.last_cleaned) + '</td>' +
      '<td class="col-num tnum">' + r.missions_today + '</td>' +
      '<td>' + escapeHtml(r.last_reason || '—') + '</td>' +
      '<td><button type="button" class="set-chk' + (on ? ' on' : '') + '" role="switch" aria-checked="' + on +
      '" aria-label="Clean ' + escapeHtml(r.vehicle_id) + ' on the interval" data-action="robotCleanOptIn" data-vehicle="' +
      escapeHtml(r.vehicle_id) + '"' + (canEdit ? '' : ' disabled') + '></button></td></tr>';
  }).join('');
}

function drawMissions(missions, since) {
  const body = document.getElementById('cleaning-missions');
  const sinceEl = document.getElementById('cleaning-since');
  if (sinceEl) sinceEl.innerHTML = 'since ' + timeHTML(since);
  if (!body) return;
  if (!missions.length) {
    body.innerHTML = '<tr><td colspan="6" class="text-muted">No missions sent.</td></tr>';
    return;
  }
  body.innerHTML = missions.map((m) =>
    '<tr><td>' + timeHTML(m.dispatched_at) + '</td>' +
    '<td>' + escapeHtml(m.vehicle_id) + '</td>' +
    '<td>' + escapeHtml(m.reason) + '</td>' +
    '<td>' + escapeHtml(m.state) + '</td>' +
    '<td>' + timeHTML(m.finished_at) + '</td>' +
    '<td class="mono">' + escapeHtml(m.vendor_order_id) + '</td></tr>').join('');
}

// loadCleaning reads GET /api/robots/cleaning and draws it. The setup's
// fields are written only on the first load: after that the draft is the
// person's, and settingsPage owns it.
export function loadCleaning() {
  return fetch('/api/robots/cleaning')
    .then((r) => r.json())
    .then((d) => {
      if (!page && root()) {
        const s = d.setup;
        write(s.saved ? {
          points: s.points, frequency: s.frequency, min_battery: String(s.min_battery),
        } : NEVER_SAVED);
        page = settingsPage(root(), { sections: { mission: { label: 'clean mission', read, write, validate, save } } });
        root().addEventListener('click', (e) => {
          const seg = e.target.closest('[data-seg="frequency"] button');
          if (seg) setSeg(seg.dataset.val);
        });
        root().addEventListener('input', refreshShown);
        suggestPoints({ value: '' });
      }
      drawWords(d.words);
      drawRobots(d.robots || []);
      drawMissions(d.missions || [], d.since);
      return d;
    });
}
