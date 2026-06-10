// Mission Telemetry (/missions) — the analytical DRILL page (wave 2, Q-035).
// The hero KPI strip, Trends, and Live ops moved to Overview (the snapshot
// page); Missions keeps the working sections: filter bar, cells, parts,
// breakdowns, Failure Pareto, and the mission table + CSV. A global filter
// store (Since/Until + station/robot + state) drives the data sections.

import { apiGet, el, formatDuration, timeAgo, toast } from '/static/app.js';
import { createStore, onSSE, debounce } from '/static/shared/utils.js';
import { CellTile, updateCellTile, pulseCellDot } from '/static/components/CellTile.js';
import { openCellDrill } from '/static/components/CellDrill.js';
import { renderBarList } from '/static/components/BarList.js';
import { makeChart, chartColors, installChartThemeHook } from '/static/components/charts.js';

const filters = createStore({ since: '', until: '', station: '', robot: '', state: '' });

let offset = 0;
const LIMIT = 50;
let lastMissions = []; // for CSV export
// ─── list ───────────────────────────────────────────────────────────────
function refreshList(state) {
    const qs = filterQS(state, { limit: LIMIT, offset });
    apiGet('/api/missions?' + qs).then((data) => {
        const tbody = document.getElementById('mission-list');
        if (!tbody) return;
        lastMissions = (data && data.missions) || [];
        tbody.innerHTML = '';
        for (const m of lastMissions) {
            const tr = el('tr', { className: 'mission-row', dataset: { orderId: m.order_id }, title: 'Click to view mission details for order ' + m.order_id });
            tr.innerHTML =
                '<td>' + m.order_id + '</td>' +
                '<td>' + (m.robot_id || '-') + '</td>' +
                '<td>' + (m.station_id || '-') + '</td>' +
                '<td>' + (m.source_node || '?') + ' &rarr; ' + (m.delivery_node || '?') + '</td>' +
                '<td><span class="badge ' + stateBadgeClass(m.terminal_state) + '">' + stateLabel(m.terminal_state) + '</span></td>' +
                '<td title="' + (m.duration_ms ? m.duration_ms + 'ms' : '') + '">' + formatDuration(m.duration_ms) + '</td>' +
                '<td title="' + formatAbsTime(m.core_completed) + '">' + timeAgo(m.core_completed) + '</td>';
            tbody.appendChild(tr);
        }
        if (!lastMissions.length) tbody.innerHTML = '<tr><td colspan="7" style="text-align:center;color:var(--text-muted)">No missions found</td></tr>';
        renderPagination((data && data.total) || 0, offset, LIMIT);
    });
}

function renderPagination(total, off, limit) {
    const elp = document.getElementById('pagination');
    if (!elp) return;
    if (total <= limit) { elp.innerHTML = ''; return; }
    const page = Math.floor(off / limit) + 1;
    const pages = Math.ceil(total / limit);
    let html = '<span style="color:var(--text-muted)">' + total + ' total</span>';
    if (page > 1) html += ' <button class="btn btn-sm" data-page="' + (off - limit) + '">Prev</button>';
    html += ' <span>Page ' + page + '/' + pages + '</span>';
    if (page < pages) html += ' <button class="btn btn-sm" data-page="' + (off + limit) + '">Next</button>';
    elp.innerHTML = html;
    elp.querySelectorAll('button[data-page]').forEach((b) => b.addEventListener('click', () => { offset = parseInt(b.dataset.page, 10); refreshList(filters.get()); }));
}

function exportCSV() {
    if (!lastMissions.length) { toast('No missions to export', 'info'); return; }
    const cols = ['order_id', 'robot_id', 'station_id', 'source_node', 'delivery_node', 'terminal_state', 'duration_ms', 'core_completed'];
    const lines = [cols.join(',')];
    for (const m of lastMissions) lines.push(cols.map((c) => csvCell(m[c])).join(','));
    const blob = new Blob([lines.join('\n')], { type: 'text/csv' });
    const url = URL.createObjectURL(blob);
    const a = el('a', { href: url, download: 'missions.csv' });
    document.body.appendChild(a); a.click(); a.remove();
    URL.revokeObjectURL(url);
}

// ─── filter wiring ────────────────────────────────────────────────────────
function setState(s) {
    document.querySelectorAll('.state-btn').forEach((b) => b.classList.toggle('is-active', b.dataset.state === s));
    filters.set({ state: s });
}

let paretoChart = null;

function refresh(state) {
    offset = 0;
    refreshBreakdowns(state);
    refreshFailures(state);
    refreshList(state);
    refreshCells();
}

// ─── Section D: cells (production rhythm, §3.D / Phase E) ────────────────────
// Filter-independent live state: load the configured cells, paint each tile's
// current state, then pulse on cell-heartbeat SSE. A burst of ticks schedules
// one debounced state refresh per cell (colors/cycle), not one per fire.
const cellTiles = new Map(); // cell_id -> tile node
let cellList = [];
const cellStateTimers = {};

function refreshCells() {
    apiGet('/api/cells').then((list) => {
        cellList = list || [];
        const grid = document.getElementById('m-cells-grid');
        const note = document.getElementById('m-cells-note');
        if (!grid) return;
        grid.innerHTML = '';
        cellTiles.clear();
        if (!cellList.length) {
            grid.innerHTML = '<div class="dash-empty">No cells configured. <a href="/admin/cells">Define cells</a> to see production rhythm here.</div>';
            if (note) note.textContent = '';
            return;
        }
        if (note) note.textContent = cellList.length + (cellList.length === 1 ? ' cell' : ' cells');
        cellList.forEach((c) => {
            const tile = CellTile(c);
            tile.addEventListener('click', () => openCellDrill(c.cell_id));
            tile.addEventListener('keydown', (e) => {
                if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); openCellDrill(c.cell_id); }
            });
            cellTiles.set(c.cell_id, tile);
            grid.appendChild(tile);
            refreshCellState(c.cell_id);
        });
    }).catch(() => {});
}

function refreshCellState(cellID) {
    apiGet('/api/cells/' + encodeURIComponent(cellID) + '/state')
        .then((state) => { const t = cellTiles.get(cellID); if (t) updateCellTile(t, state); })
        .catch(() => {});
}

function scheduleCellState(cellID) {
    if (cellStateTimers[cellID]) return;
    cellStateTimers[cellID] = setTimeout(() => {
        delete cellStateTimers[cellID];
        refreshCellState(cellID);
    }, 2000);
}

function onCellHeartbeat(data) {
    if (!data) return;
    const pid = Number(data.process_id);
    cellList.forEach((c) => {
        if (c.station !== data.station) return;
        const mine = c.primary_process_id === pid || (c.sub_process_ids || []).indexOf(pid) >= 0;
        if (!mine) return;
        const tile = cellTiles.get(c.cell_id);
        if (tile) { pulseCellDot(tile, pid); scheduleCellState(c.cell_id); }
    });
}

// Sys-health pills (fleet/messaging/database) relocated to the filter bar in
// wave 2 — Live ops itself moved to Overview. Driven by the system-status SSE.
function updateSysPills(data) {
    const el2 = document.getElementById('m-sys-pills');
    if (!el2) return;
    const cur = el2.__sys || (el2.__sys = {});
    if (data && typeof data === 'object') Object.assign(cur, data);
    const dot = (k) => '<span class="health ' + (cur[k] === 'connected' ? 'health-ok' : cur[k] === 'disconnected' ? 'health-fail' : '') + '"></span>' + k;
    el2.innerHTML = dot('fleet') + ' &nbsp; ' + dot('messaging') + ' &nbsp; ' + dot('database');
}

// ─── Section G: failure Pareto (§3.G) ───────────────────────────────────────
function refreshFailures(state) {
    apiGet('/api/missions/failures?' + filterQS(state, {}))
        .then((d) => renderPareto((d && d.reasons) || []))
        .catch(() => {
            // Surface a dead endpoint instead of silently leaving a blank card —
            // a 500 here used to read as "No failures" (worded distinctly from
            // renderPareto's empty state so the two aren't confused).
            const box = document.querySelector('#m-failures .chart-box');
            if (!box) return;
            if (paretoChart) { try { paretoChart.destroy(); } catch (_) {} paretoChart = null; }
            box.innerHTML = '<div class="dash-empty">Failure data unavailable.</div>';
        });
}

function renderPareto(reasons) {
    const box = document.querySelector('#m-failures .chart-box');
    if (!box) return;
    if (paretoChart) { try { paretoChart.destroy(); } catch (_) {} paretoChart = null; }
    if (!reasons.length) { box.innerHTML = '<div class="dash-empty">No failures in this window.</div>'; return; }
    if (!box.querySelector('canvas')) box.innerHTML = '<canvas></canvas>';
    const canvas = box.querySelector('canvas');
    const c = chartColors();
    const labels = reasons.map((r) => r.reason);
    const counts = reasons.map((r) => r.count);
    const total = counts.reduce((a, b) => a + b, 0) || 1;
    let cum = 0;
    const cumPct = counts.map((n) => { cum += n; return Math.round(cum / total * 1000) / 10; });
    // Fault highlighting (Q-026): real robot/hardware faults — the actionable
    // ones surfaced from robot_alarms_json — render in danger red; orchestration
    // noise (the fleet's 60011 "Vendor error", timeouts, manifest) is muted, so
    // a wall of red 60011 no longer hides a battery or motor fault.
    const ROBOT_FAULTS = new Set(['Emergency stop', 'Motor fault', 'Battery', 'Hardware fault', 'Comms', 'Path planning', 'Robot blocked']);
    const barColors = labels.map((l) => (ROBOT_FAULTS.has(l) ? c.danger : c.info));
    paretoChart = makeChart(canvas, {
        type: 'bar',
        data: {
            labels,
            datasets: [
                { type: 'bar', label: 'Count', data: counts, backgroundColor: barColors, yAxisID: 'y', order: 2 },
                { type: 'line', label: 'Cumulative %', data: cumPct, borderColor: c.warning, backgroundColor: c.warning, yAxisID: 'y1', tension: 0.2, pointRadius: 2, order: 1 },
            ],
        },
        options: {
            scales: {
                y: { min: 0, ticks: { precision: 0 } },
                y1: { position: 'right', min: 0, max: 100, grid: { drawOnChartArea: false }, ticks: { callback: (v) => v + '%' } },
            },
            plugins: {
                legend: { display: true, labels: { color: c.text, boxWidth: 12 } },
                tooltip: { callbacks: { afterBody: (items) => { const r = reasons[items[0].dataIndex]; return (r && r.sample_order_ids && r.sample_order_ids.length) ? 'Orders: ' + r.sample_order_ids.join(', ') : ''; } } },
            },
        },
    });
    // Clicking a bar filters the list to failures.
    canvas.onclick = () => setState('FAILED');
}
function refreshAll(state) { refresh(state); }
const onFilterChange = debounce(refresh, 150);

// §3.F breakdowns: top robots and routes. Robot rows are clickable → add the
// robot to the global filter; route isn't a filter facet so route rows are
// informational (Q-012).
function refreshBreakdowns(state) {
    const base = filterQS(state, {});
    apiGet('/api/missions/breakdown?by=robot&' + base).then((d) => {
        renderBarList(document.getElementById('m-bd-robot'), (d && d.rows) || [], {
            label: (r) => r.label, raw: (r) => r.count,
            value: (r) => r.count + ' · ' + formatDuration(r.avg_duration_ms),
            color: 'var(--primary)',
            onClick: (r) => { filters.set({ robot: r.label }); const sel = document.getElementById('m-robot'); if (sel) sel.value = r.label; },
        });
    }).catch(() => {});
    apiGet('/api/missions/breakdown?by=route&' + base).then((d) => {
        renderBarList(document.getElementById('m-bd-route'), (d && d.rows) || [], {
            label: (r) => r.label, raw: (r) => r.count,
            value: (r) => r.count + ' · ' + formatDuration(r.avg_duration_ms),
            color: 'var(--info)',
        });
    }).catch(() => {});
}

function initFilterBar() {
    const since = document.getElementById('m-since');
    const until = document.getElementById('m-until');
    if (since) since.addEventListener('change', () => filters.set({ since: since.value }));
    if (until) until.addEventListener('change', () => filters.set({ until: until.value }));
    const station = document.getElementById('m-station');
    if (station) station.addEventListener('change', () => filters.set({ station: station.value }));
    const robot = document.getElementById('m-robot');
    if (robot) robot.addEventListener('change', () => filters.set({ robot: robot.value }));
    const refresh = document.getElementById('m-refresh');
    if (refresh) refresh.addEventListener('click', () => refreshAll(filters.get()));
    document.querySelectorAll('.state-btn').forEach((btn) => btn.addEventListener('click', () => setState(btn.dataset.state)));
    const csv = document.getElementById('m-csv');
    if (csv) csv.addEventListener('click', exportCSV);
    const tbody = document.getElementById('mission-list');
    if (tbody) tbody.addEventListener('click', (e) => {
        const tr = e.target.closest('tr.mission-row');
        if (tr && tr.dataset.orderId) window.location.href = '/missions/' + tr.dataset.orderId;
    });
}

async function loadFilterOptions() {
    try {
        const stations = await apiGet('/api/stations');
        addOptions('m-station', Array.isArray(stations) ? stations : (stations && stations.stations) || [], (s) => (typeof s === 'string' ? s : (s.id || s.station_id || s.name)));
    } catch (e) { /* non-fatal */ }
    try {
        const robots = await apiGet('/api/robots');
        addOptions('m-robot', Array.isArray(robots) ? robots : (robots && robots.robots) || [], (r) => (typeof r === 'string' ? r : (r.vehicle_id || r.VehicleID || r.id)));
    } catch (e) { /* non-fatal */ }
}

// ─── boot ───────────────────────────────────────────────────────────────
function init() {
    installChartThemeHook(); // for the Failure Pareto chart
    initFilterBar();
    loadFilterOptions();
    updateSysPills(null);
    filters.subscribe(onFilterChange);

    // Drill page: no live KPI/list refresh. Only the connection pill, the
    // relocated sys-health pills, and the live cell-heartbeat stay live.
    onSSE('connected', () => { const p = document.getElementById('m-live'); if (p) { p.classList.add('is-live'); p.innerHTML = '&#9679; live'; } });
    onSSE('system-status', (data) => updateSysPills(data));
    onSSE('cell-heartbeat', onCellHeartbeat);

    refreshAll(filters.get());
}
if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
else init();

// ─── helpers ──────────────────────────────────────────────────────────────
function filterQS(state, extra) {
    const p = new URLSearchParams();
    if (state.since) p.set('since', state.since);
    if (state.until) p.set('until', state.until);
    if (state.station) p.set('station_id', state.station);
    if (state.robot) p.set('robot_id', state.robot);
    if (state.state) p.set('state', state.state);
    for (const k in extra) p.set(k, extra[k]);
    return p.toString();
}

function formatAbsTime(ts) { return ts ? new Date(ts).toLocaleString() : ''; }
function csvCell(v) { if (v === null || v === undefined) return ''; const s = String(v); return /[",\n]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s; }

function stateLabel(state) {
    if (!state) return '-';
    const map = { FINISHED: 'completed', delivered: 'completed', confirmed: 'completed', FAILED: 'failed', failed: 'failed', STOPPED: 'cancelled', cancelled: 'cancelled' };
    return map[state] || state;
}
function stateBadgeClass(state) {
    const label = stateLabel(state);
    const classMap = { completed: 'badge-confirmed', failed: 'badge-failed', cancelled: 'badge-cancelled' };
    return classMap[label] || ('badge-' + label);
}
