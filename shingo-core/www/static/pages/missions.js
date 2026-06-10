// Mission Telemetry (/missions) — slice 1: hero strip + global filter bar +
// preserved list (plan §2, §3.A, §3.H). The analytical sections B–G land in
// slices 2–4 below the hero; this slice establishes the global filter store
// driving both the hero and the list, and removes the old table-internal
// filter bar. Reuses the slice-0 KpiTile component + stats-v2/active/alerts
// endpoints.

import { apiGet, el, formatDuration, timeAgo, toast } from '/static/app.js';
import { createStore, onSSE, debounce } from '/static/shared/utils.js';
import { KpiTile, updateKpiTile } from '/static/components/KpiTile.js';
import { CellTile, updateCellTile, pulseCellDot } from '/static/components/CellTile.js';
import { openCellDrill } from '/static/components/CellDrill.js';
import { renderBarList } from '/static/components/BarList.js';
import { createTrendsSection } from '/static/pages/overview/trends.js';
import { makeChart, chartColors, installChartThemeHook } from '/static/components/charts.js';

const filters = createStore({ since: '', until: '', station: '', robot: '', state: '' });

let offset = 0;
const LIMIT = 50;
let lastMissions = []; // for CSV export
const tiles = {};

// ─── hero ───────────────────────────────────────────────────────────────
function buildHero() {
    const grid = document.getElementById('m-kpi-grid');
    if (!grid) return;
    grid.innerHTML = '';
    const specs = [
        { id: 'success', label: 'Success rate' },
        { id: 'volume', label: 'Volume' },
        { id: 'avg', label: 'Avg duration' },
        { id: 'inflight', label: 'In flight' },
        // Alerts is clickable → filters the list to failures (§3.A drill).
        { id: 'alerts', label: 'Alerts', drill: 'alerts' },
    ];
    for (const s of specs) { const t = KpiTile(s); tiles[s.id] = t; grid.appendChild(t); }
    // Alerts tile → filter list to failed missions.
    tiles.alerts.addEventListener('click', () => {
        setState('FAILED');
        document.querySelector('.missions-dash section.card')?.scrollIntoView({ behavior: 'smooth' });
    });
}

function refreshHero(state) {
    const cur = filterQS(state, {});
    const prevWin = prevWindow(state);
    Promise.all([
        apiGet('/api/missions/stats/v2?' + cur).catch(() => null),
        prevWin ? apiGet('/api/missions/stats/v2?' + prevWin).catch(() => null) : Promise.resolve(null),
    ]).then(([s, prev]) => {
        if (!s) return;
        const denom = (s.confirmed || 0) + (s.failed || 0);
        updateKpiTile(tiles.success, {
            label: 'Success rate',
            value: denom > 0 ? s.success_rate.toFixed(1) + '%' : '—',
            sub: denom > 0 ? s.confirmed + ' of ' + denom : 'no completed missions',
            delta: (prev && (prev.confirmed + prev.failed) > 0) ? ptDelta(s.success_rate - prev.success_rate) : null,
        });
        // Volume headlines total throughput; confirmed is the sub-stat (it
        // belongs to the success-rate tile). Delta tracks the headline (total).
        updateKpiTile(tiles.volume, { label: 'Volume', value: s.total, sub: s.confirmed + ' confirmed', delta: prev ? countDelta(s.total - prev.total) : null });
        updateKpiTile(tiles.avg, {
            // Headline execution time (assignment→terminal, what the robot spent);
            // lead time (created→terminal) is the sub-stat (Q-031).
            label: 'Avg duration',
            value: (s.total > 0 && s.avg_execution_ms > 0) ? formatDuration(s.avg_execution_ms) : '—',
            sub: s.avg_duration_ms > 0 ? 'Lead ' + formatDuration(s.avg_duration_ms) : '',
        });
    });
    refreshActive();
    refreshAlerts();
}

function refreshActive() {
    apiGet('/api/missions/active')
        .then((d) => updateKpiTile(tiles.inflight, { label: 'In flight', value: (d && typeof d.count === 'number') ? d.count : '—', sub: 'live' }))
        .catch(() => {});
}

function refreshAlerts() {
    apiGet('/api/missions/alerts').then((a) => {
        const total = a ? a.total : 0;
        updateKpiTile(tiles.alerts, { label: 'Alerts', drill: 'alerts', value: total || 0, sub: total ? 'click to filter' : 'all clear', tone: total ? 'bad' : undefined });
        const holder = document.getElementById('m-alerts');
        if (!holder) return;
        if (!total) { holder.innerHTML = ''; return; }
        const parts = [];
        if (a.robots_blocked) parts.push(a.robots_blocked + ' blocked');
        if (a.robots_emergency) parts.push(a.robots_emergency + ' emergency');
        if (a.robots_error) parts.push(a.robots_error + ' in error');
        if (a.stuck_missions) parts.push(a.stuck_missions + ' stuck');
        holder.innerHTML = '<div class="alerts-banner" role="status"><span class="alerts-banner__count">⚠ ' + total + ' alert' + (total > 1 ? 's' : '') + '</span><span>' + parts.join(' · ') + '</span></div>';
    }).catch(() => {});
}

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

let trendsSection = null;
let paretoChart = null;

function refresh(state) {
    offset = 0;
    refreshHero(state);
    if (trendsSection) trendsSection.refresh(state);
    refreshParts(state);
    refreshBreakdowns(state);
    refreshFailures(state);
    refreshList(state);
    // Live ops is filter-independent ("right now"); refreshed here for the
    // first paint and then driven by SSE.
    refreshLiveOps();
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

// ─── Section C: live ops (always live, §3.C) ────────────────────────────────
function refreshLiveOps() {
    apiGet('/api/board/orders').then((data) => {
        const orders = Array.isArray(data) ? data : (data && (data.orders || data.board)) || [];
        const holder = document.getElementById('m-inflight');
        if (!holder) return;
        if (!orders.length) { holder.innerHTML = '<div class="dash-empty">No missions in flight.</div>'; return; }
        holder.innerHTML = '';
        for (const o of orders.slice(0, 20)) {
            const id = o.order_id || o.id || o.ID;
            const robot = o.robot_id || o.robot || o.RobotID || '—';
            const src = o.source_node || o.source || o.SourceNode || '?';
            const dst = o.delivery_node || o.delivery || o.DeliveryNode || '?';
            const status = o.status || o.Status || '';
            const row = el('div', { className: 'bar-row', style: { gridTemplateColumns: 'auto auto 1fr auto' } });
            row.appendChild(el('span', { className: 'fleet-row__name' }, '#' + id));
            row.appendChild(el('span', { className: 'badge' }, String(status)));
            row.appendChild(el('span', {}, robot + '  ' + src + ' → ' + dst));
            row.appendChild(el('span', { className: 'bar-row__value' }, o.created_at ? timeAgo(o.created_at) : ''));
            holder.appendChild(row);
        }
    }).catch(() => { const h2 = document.getElementById('m-inflight'); if (h2) h2.innerHTML = '<div class="dash-empty">Live ops unavailable.</div>'; });
}

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

// §3.E parts: produced / cycle time / consumption. Part isn't a global
// filter facet (the missions query has no part filter), so rows are
// informational for now (Q-014).
function refreshParts(state) {
    const base = filterQS(state, { top: '10' });
    apiGet('/api/parts/produced?' + base).then((d) => renderBarList(document.getElementById('m-parts-produced'), (d && d.rows) || [], {
        label: (r) => r.part_number, raw: (r) => r.qty, value: (r) => r.qty + ' · ' + r.missions, color: 'var(--info)',
    })).catch(() => {});
    apiGet('/api/parts/cycle-time?' + base).then((d) => renderBarList(document.getElementById('m-parts-cycle'), (d && d.rows) || [], {
        label: (r) => r.part_number, raw: (r) => r.avg_duration_ms, value: (r) => formatDuration(r.avg_duration_ms), color: 'var(--text-muted)',
    })).catch(() => {});
    apiGet('/api/parts/consumption?' + base).then((d) => renderBarList(document.getElementById('m-parts-consume'), (d && d.rows) || [], {
        label: (r) => r.part_number, raw: (r) => r.uop, value: (r) => r.uop + ' UoP', color: 'var(--info)',
    })).catch(() => {});
}

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
    buildHero();
    installChartThemeHook();
    trendsSection = createTrendsSection(filters, { toggleId: 'm-trend-toggle', gridId: 'm-trend-grid' });
    trendsSection.mount();
    initFilterBar();
    loadFilterOptions();
    updateSysPills(null);
    filters.subscribe(onFilterChange);

    onSSE('connected', () => { const p = document.getElementById('m-live'); if (p) { p.classList.add('is-live'); p.innerHTML = '&#9679; live'; } });
    const live = debounce(() => { refreshActive(); refreshAlerts(); refreshLiveOps(); }, 1500);
    onSSE('order-update', live);
    onSSE('robot-update', live);
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

// prevWindow returns the previous equal-length window's stats query, or null
// when an explicit date range isn't set (no delta without bounds).
function prevWindow(state) {
    if (!state.since || !state.until) return null;
    const s = new Date(state.since), u = new Date(state.until);
    if (isNaN(s.getTime()) || isNaN(u.getTime())) return null;
    const days = Math.round((u - s) / 86400000) + 1;
    const prevU = new Date(s); prevU.setDate(prevU.getDate() - 1);
    const prevS = new Date(prevU); prevS.setDate(prevS.getDate() - (days - 1));
    const p = new URLSearchParams();
    p.set('since', ymd(prevS)); p.set('until', ymd(prevU));
    if (state.station) p.set('station_id', state.station);
    if (state.robot) p.set('robot_id', state.robot);
    return p.toString();
}

function ptDelta(diff) { if (!diff) return { dir: 'flat', text: '0pt' }; const up = diff > 0; return { dir: up ? 'up' : 'down', text: Math.abs(diff).toFixed(1) + 'pt', good: up }; }
function countDelta(diff) { if (!diff) return { dir: 'flat', text: '0' }; const up = diff > 0; return { dir: up ? 'up' : 'down', text: '' + Math.abs(diff), good: up }; }
function ymd(d) { return d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0'); }
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
