// Overview Section B — Mission trends (plan §15.B). A 2×2 Chart.js grid:
// throughput, success rate, P50/P95 duration, cancellation rate. The
// 24h/7d/30d toggle is LOCAL to this section (§3.B) — execs want the trend
// story regardless of the global snapshot window; station/robot still scope
// from the global store. One /api/missions/timeseries fetch powers all four.

import { apiGet } from '/static/app.js';
import { makeChart, installChartThemeHook, bucketLabel, chartColors } from '/static/components/charts.js';

export function createTrendsSection(store, opts) {
    opts = opts || {};
    const toggleId = opts.toggleId || 'ops-trend-toggle';
    const gridId = opts.gridId || 'ops-trend-grid';
    let localRange = '7d';
    let charts = [];
    let lastState = null;

    function mount() {
        installChartThemeHook();
        const toggle = document.getElementById(toggleId);
        if (toggle) {
            toggle.innerHTML = '';
            for (const r of ['24h', '7d', '30d']) {
                const b = document.createElement('button');
                b.textContent = r;
                b.dataset.range = r;
                if (r === localRange) b.classList.add('is-active');
                b.addEventListener('click', () => {
                    if (localRange === r) return;
                    localRange = r;
                    toggle.querySelectorAll('button').forEach((x) => x.classList.toggle('is-active', x.dataset.range === r));
                    if (lastState) refresh(lastState);
                });
                toggle.appendChild(b);
            }
        }
    }

    function refresh(state) {
        lastState = state;
        const win = windowFor(localRange);
        const p = new URLSearchParams({ bucket: win.bucket, since: win.since, until: win.until });
        if (state.station) p.set('station_id', state.station);
        if (state.robot) p.set('robot_id', state.robot);
        apiGet('/api/missions/timeseries?' + p.toString())
            .then((res) => render((res && res.points) || [], win.bucket))
            .catch(() => render([], win.bucket));
    }

    function destroyCharts() {
        for (const c of charts) { try { c.destroy(); } catch (_) {} }
        charts = [];
    }

    function render(points, bucket) {
        destroyCharts();
        const grid = document.getElementById(gridId);
        if (!grid) return;
        grid.innerHTML = '';
        if (!points.length) {
            grid.innerHTML = '<div class="dash-empty">No missions in this window.</div>';
            return;
        }
        const c = chartColors();
        const labels = points.map((p) => bucketLabel(p.bucket_start, bucket));

        charts.push(buildChart(grid, 'throughput', 'Throughput (missions per ' + bucket + ')', {
            type: 'bar',
            data: { labels, datasets: [{ data: points.map((p) => p.total), backgroundColor: c.primary, borderRadius: 2 }] },
        }));

        charts.push(buildChart(grid, 'success_rate', 'Success rate (%)', {
            type: 'line',
            data: { labels, datasets: [{ data: points.map((p) => round1(p.success_rate)), borderColor: c.success, backgroundColor: c.success, tension: 0.3, pointRadius: 0, fill: false }] },
            options: { scales: { y: { min: 0, max: 100 } } },
        }));

        charts.push(buildChart(grid, 'duration', 'P50 / P95 duration (s)', {
            type: 'line',
            data: {
                labels,
                datasets: [
                    { label: 'P50', data: points.map((p) => msToS(p.p50_ms)), borderColor: c.info, backgroundColor: c.info, tension: 0.3, pointRadius: 0, fill: false },
                    { label: 'P95', data: points.map((p) => msToS(p.p95_ms)), borderColor: c.warning, backgroundColor: c.warning, tension: 0.3, pointRadius: 0, fill: false },
                ],
            },
            options: { plugins: { legend: { display: true, labels: { color: c.text, boxWidth: 12 } } } },
        }));

        charts.push(buildChart(grid, 'cancellation', 'Cancellation rate (%)', {
            type: 'line',
            data: { labels, datasets: [{ data: points.map((p) => p.total ? round1(p.cancelled / p.total * 100) : 0), borderColor: c.text, backgroundColor: c.text, tension: 0.3, pointRadius: 0, fill: false }] },
            options: { scales: { y: { min: 0, max: 100 } } },
        }));

        // Initial draw animates; subsequent data sets shouldn't (§4) — handled
        // by rebuilding fresh charts each refresh, so no per-update jitter.
    }

    function buildChart(grid, metric, caption, config) {
        const cell = document.createElement('div');
        cell.innerHTML = '<div class="chart-caption">' + caption + '</div>';
        const box = document.createElement('div');
        box.className = 'chart-box';
        box.style.height = '200px';
        box.dataset.action = 'openDrill:' + metric; // §15.B click → drill modal
        const canvas = document.createElement('canvas');
        box.appendChild(canvas);
        cell.appendChild(box);
        grid.appendChild(cell);
        return makeChart(canvas, config);
    }

    return { mount, refresh };
}

// ─── helpers ──────────────────────────────────────────────────────────────
function ymd(d) {
    return d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0');
}

// windowFor maps the local toggle to a date window + bucket granularity.
// Date-bound filters can't express a true rolling 24h (§8 #17) — 24h covers
// yesterday+today at hourly granularity.
function windowFor(range) {
    const today = new Date(); today.setHours(0, 0, 0, 0);
    if (range === '30d') {
        const since = new Date(today); since.setDate(since.getDate() - 29);
        return { since: ymd(since), until: ymd(today), bucket: 'day' };
    }
    if (range === '7d') {
        const since = new Date(today); since.setDate(since.getDate() - 6);
        return { since: ymd(since), until: ymd(today), bucket: 'hour' };
    }
    const since = new Date(today); since.setDate(since.getDate() - 1); // 24h
    return { since: ymd(since), until: ymd(today), bucket: 'hour' };
}

function round1(v) { return Math.round((v || 0) * 10) / 10; }
function msToS(ms) { return Math.round((ms || 0) / 100) / 10; }
