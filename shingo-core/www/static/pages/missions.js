// Mission Telemetry (/missions) — the analytical DRILL page (wave 2, Q-035).
// The hero KPI strip, Trends, and Live ops moved to Overview (the snapshot
// page); Missions keeps the working sections: filter bar, cells, parts,
// breakdowns, Failure Pareto, and the mission table + CSV. A global filter
// store (Since/Until + station/robot + state) drives the data sections.

import { apiGet, el, formatDuration, timeAgo, toast } from '/static/app.js';
import { createStore, onSSE, debounce } from '/static/shared/utils.js';
import { CellTile, updateCellTile, pulseCellDot } from '/static/components/CellTile.js';
import { openCellDrill } from '/static/components/CellDrill.js';
// BarList is no longer imported here: U3 replaced both breakdown panels with
// tables, and this was its last consumer in the repo. The owner has since made
// the call and components/BarList.js is DELETED — a primitive with no consumer
// is not a primitive, it is a file that has to be kept correct for nobody.
// Its .bar-row* CSS is a separate question and is annotated where it lives
// (shared/components.css); .bar-list itself is still live on /overview.
import { makeChart, chartColors, installChartThemeHook } from '/static/components/charts.js';

const filters = createStore({ since: '', until: '', station: '', robot: '', state: '' });

let offset = 0;
const LIMIT = 50;
let lastMissions = []; // for CSV export

// Station uid → operator label, refreshed with each list response.
//
// The rows keep the opaque station_id — it is what the filter sends, what the
// CSV export writes and what the drill-down queries by. Only the rendered text
// becomes the label, so a rename shows up here on the next refresh without
// anything stored having changed. A station with no registry row (core-spot,
// core-direct, core-test, '*') is absent from the map and renders as itself.
let stationNames = {};
function stationLabel(id) {
    if (!id) return '-';
    return stationNames[id] || id;
}
// ─── list ───────────────────────────────────────────────────────────────
function refreshList(state) {
    const qs = filterQS(state, { limit: LIMIT, offset });
    apiGet('/api/missions?' + qs).then((data) => {
        const tbody = document.getElementById('mission-list');
        if (!tbody) return;
        lastMissions = (data && data.missions) || [];
        stationNames = (data && data.station_names) || {};
        tbody.innerHTML = '';
        for (const m of lastMissions) {
            const tr = el('tr', { className: 'mission-row', dataset: { orderId: m.order_id }, title: 'Click to view mission details for order ' + m.order_id });
            tr.innerHTML =
                '<td>' + m.order_id + '</td>' +
                '<td>' + (m.robot_id || '-') + '</td>' +
                '<td>' + stationLabel(m.station_id) + '</td>' +
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

// ─── Dwell (per-state, p50/p95) ─────────────────────────────────────────────
//
// GET /api/missions/dwell answers the question every mission stat dodged:
// where did the time GO. One created→terminal number cannot tell a mission
// that spent eight of its nine minutes queued behind material from one that
// spent them driving.
//
// Reads order_history, not mission_telemetry — 76.6% of terminal orders have
// no mission_telemetry row at all, so dwell computed from missions would
// silently describe a quarter of the work.
//
// The count is shown beside every percentile because at this volume it has to
// be: a p95 over four samples is noise wearing a statistic's clothes, and a
// reader needs to see that without going and checking.
const DWELL_LABELS = {
    time_to_dispatch: 'To dispatch',
    transit: 'Transit',
    staged_release: 'Staged → resume',
    staged_delivery: 'Staged → delivered',
    operator_fill: 'Operator fill',
};

// A percentile computed from very few samples is reported, but marked, rather
// than hidden — hiding it would leave a gap a reader fills with a guess.
const DWELL_THIN_SAMPLE = 5;

function fmtSeconds(s) {
    if (s === null || s === undefined) return '-';
    if (s <= 0) return '0s';
    if (s < 60) return (s < 10 ? s.toFixed(1) : Math.round(s)) + 's';
    const m = Math.floor(s / 60);
    const rem = Math.round(s % 60);
    if (m < 60) return m + 'm ' + rem + 's';
    return Math.floor(m / 60) + 'h ' + (m % 60) + 'm';
}

function refreshDwell(state) {
    const host = document.getElementById('m-dwell-row');
    if (!host) return;
    apiGet('/api/missions/dwell?' + filterQS(state, {})).then((data) => {
        const rows = (data && data.rows) || [];
        if (!rows.length) {
            host.innerHTML = '<span class="text-muted-sm">No transitions in this window</span>';
            return;
        }
        host.innerHTML = rows.map((r) => {
            const label = DWELL_LABELS[r.key] || r.key;
            // No samples is not zero seconds. Say so rather than printing 0s,
            // which reads as "instant".
            if (!r.count) {
                return '<div class="dwell-cell dwell-empty" title="' + r.from + ' → ' + r.to + '">'
                    + '<span class="dwell-label">' + label + '</span>'
                    + '<span class="dwell-val">no data</span>'
                    + '<span class="dwell-count">0 samples</span>'
                    + '</div>';
            }
            const thin = r.count < DWELL_THIN_SAMPLE ? ' dwell-thin' : '';
            return '<div class="dwell-cell' + thin + '" title="' + r.from + ' → ' + r.to
                + (thin ? ' — only ' + r.count + ' samples, read with care' : '') + '">'
                + '<span class="dwell-label">' + label + '</span>'
                + '<span class="dwell-val">' + fmtSeconds(r.p50_seconds)
                + ' <span class="dwell-sep">/</span> ' + fmtSeconds(r.p95_seconds) + '</span>'
                + '<span class="dwell-count">' + r.count + ' sample' + (r.count === 1 ? '' : 's') + '</span>'
                + '</div>';
        }).join('');
    }).catch(() => {
        host.innerHTML = '<span class="text-muted-sm">Dwell unavailable</span>';
    });
}

let paretoChart = null;

function refresh(state) {
    offset = 0;
    refreshDwell(state);
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

// §3.F breakdowns, U3 — TABLES, and an INDEXED per-robot figure.
//
// The bar list this replaces showed each robot's mean mission duration. Five
// reviewers wanted that retired outright and were right about why: RDS assigns
// the routes, so a per-robot mean measures which routes that robot happened to
// draw. A robot parked on supermarket hauls reads slow and is not — so the panel
// reliably concluded "AMR-04 is slow" about the one variable it could not see.
//
// The figure is now duration ÷ that route's median duration, aggregated as a
// MEDIAN OF RATIOS per robot. 1.00x means the robot runs its routes in the time
// those routes normally take. It is a sentence about the robot rather than about
// the route mix.
//
// A TABLE and not a bar list, because a bar encodes ONE number as length and
// this panel now has three (volume, duration, index) — the form could not hold
// the content. Under ten rows a sorted table also reads faster and carries the
// exact figures.
//
// THE SMALL-n RULES, both of them, and they are different rules:
//   - no route cleared min_route_samples  → the Index COLUMN IS DROPPED, and a
//     note says why. An empty column reads as a claim about the robots.
//   - a robot's index is over fewer than min_robot_samples missions → the figure
//     is greyed AND its sample count is printed, per 5.4/5.9. Greying alone is
//     colour-only signalling; the number is what makes it actionable.
//   - a robot ran on no qualifying route at all → route_index is null from the
//     server and the cell is an em dash with a title. Not 0.00x, which would
//     read as infinitely fast.
//
// LABELS. The duration column says "Avg mission" and not "cycle time". These
// figures average mission_telemetry.duration_ms, which is a TRANSPORT duration —
// the same distinction that renamed /api/parts/cycle-time to
// /api/parts/mission-duration. A cycle time is a cell's, not a robot's.
function fmtIndex(x) {
    // One decimal, per the number doctrine's row for a ratio. Two would assert a
    // precision a median of a few dozen heavy-tailed ratios cannot support.
    return x.toFixed(1) + '×';
}

function breakdownTable(container, rows, opts) {
    if (!container) return;
    if (!rows || !rows.length) {
        container.innerHTML = '<div class="dash-empty">No missions in this window.</div>';
        return;
    }
    const showIndex = !!opts.showIndex;
    const minRobot = opts.minRobotSamples || 0;

    const head = '<thead><tr>'
        + '<th>' + opts.labelHead + '</th>'
        + '<th class="col-num">Missions</th>'
        + '<th class="col-num" title="Average of mission_telemetry.duration_ms — a TRANSPORT duration, not a cell cycle time">Avg mission</th>'
        + (showIndex ? '<th class="col-num" title="Median over this robot’s missions of (duration ÷ that route’s median duration). 1.0× means the robot runs its routes in the time those routes normally take.">Route index</th>' : '')
        + '</tr></thead>';

    const body = rows.map((r) => {
        const label = String(opts.label(r));
        let idxCell = '';
        if (showIndex) {
            if (r.route_index === null || r.route_index === undefined) {
                idxCell = '<td class="col-num u3-nodata" title="This robot ran no missions on a route with enough samples to be a denominator, so it has no index. Not the same as a fast one.">&mdash;</td>';
            } else {
                const thin = r.index_samples < minRobot;
                idxCell = '<td class="col-num' + (thin ? ' u3-thin' : '') + '"'
                    + (thin ? ' title="Indexed over ' + r.index_samples + ' missions, below the ' + minRobot + ' this surface needs before the figure is stable."' : '')
                    + '>' + fmtIndex(r.route_index)
                    + (thin ? ' <span class="u3-n">n=' + r.index_samples + '</span>' : '')
                    + '</td>';
            }
        }
        return '<tr' + (opts.onClick ? ' class="u3-row" data-label="' + escapeAttr(label) + '"' : '') + '>'
            + '<td title="' + escapeAttr(label) + '">' + escapeText(label) + '</td>'
            + '<td class="col-num tnum">' + r.count + '</td>'
            + '<td class="col-num tnum">' + escapeText(formatDuration(r.avg_duration_ms)) + '</td>'
            + idxCell
            + '</tr>';
    }).join('');

    container.innerHTML = (opts.note ? '<p class="u3-note">' + escapeText(opts.note) + '</p>' : '')
        + '<table class="u3-tbl">' + head + '<tbody>' + body + '</tbody></table>';

    if (opts.onClick) {
        container.querySelectorAll('.u3-row').forEach((tr) => {
            tr.addEventListener('click', () => opts.onClick(tr.dataset.label));
        });
    }
}

function escapeText(s) {
    return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}
function escapeAttr(s) {
    return escapeText(s).replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

// §3.F breakdowns: top robots and routes. Robot rows are clickable → add the
// robot to the global filter; route isn't a filter facet so route rows are
// informational (Q-012).
function refreshBreakdowns(state) {
    const base = filterQS(state, {});
    apiGet('/api/missions/breakdown?by=robot&' + base).then((d) => {
        const rows = (d && d.rows) || [];
        // index_available is the server's answer to "did ANY route qualify".
        // Defaulting it to true here would drop the distinction the endpoint was
        // extended to carry; defaulting to false would hide a live column on an
        // older server. Read it, and treat a missing field as "no index" — an
        // absent answer is not a positive finding.
        const showIndex = !!(d && d.index_available);
        const minRoute = (d && d.min_route_samples) || 0;
        breakdownTable(document.getElementById('m-bd-robot'), rows, {
            labelHead: 'Robot',
            label: (r) => r.label,
            showIndex,
            minRobotSamples: (d && d.min_robot_samples) || 0,
            note: showIndex
                ? ''
                : 'Route index not shown: no route in this window has the '
                  + minRoute + ' missions needed for its median to be a denominator. '
                  + 'Widen the window rather than reading the durations as a robot comparison — '
                  + 'the routes differ more than the robots do.',
            onClick: (label) => {
                filters.set({ robot: label });
                const sel = document.getElementById('m-robot');
                if (sel) sel.value = label;
            },
        });
    }).catch(() => {});
    apiGet('/api/missions/breakdown?by=route&' + base).then((d) => {
        breakdownTable(document.getElementById('m-bd-route'), (d && d.rows) || [], {
            labelHead: 'Route',
            label: (r) => r.label,
            showIndex: false,
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
