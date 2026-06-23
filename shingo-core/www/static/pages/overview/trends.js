// Overview Section B — Mission trends (plan §15.B). A 2×2 Chart.js grid:
// throughput, success rate, P50/P95 duration, cancellation rate. Driven
// entirely by the global ops filter (Today/7d/30d + station/robot) — wave 2
// removed the section-local range toggle (Q-035), so Overview has one range
// control. Today buckets hourly; 7d/30d bucket by DAY so the x-axis is readable
// (hourly labels repeated 08:00…08:00 across days were unreadable). One
// /api/missions/timeseries fetch powers all four.

import { apiGet } from '/static/app.js';
import { makeChart, installChartThemeHook, bucketLabel, chartColors } from '/static/components/charts.js';

// Minimum completed+failed missions for a bucket's success rate to be plotted.
// Below this, the rate is pure 100/0 noise (a 1-mission bucket is always 0% or
// 100%), so we render a gap instead (B4; Q-005 small-denominator fix).
const MIN_RATE_DENOM = 3;

export function createTrendsSection(store, opts) {
    opts = opts || {};
    const gridId = opts.gridId || 'ops-trend-grid';
    let charts = [];

    function mount() {
        installChartThemeHook();
        // No local toggle anymore — the global ops filter drives the range.
    }

    function refresh(state) {
        const win = windowFor(state.range || 'today');
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
            data: { labels, datasets: [{ data: points.map((p) => p.total), backgroundColor: c.vizAccent, borderRadius: 2 }] }, // throughput = the ONE indigo anchor (P18)
        }));

        charts.push(buildChart(grid, 'success_rate', 'Success rate (%)', {
            type: 'line',
            // Thin buckets (<MIN_RATE_DENOM finished missions) are plotted as null
            // so 100/0 noise doesn't read as a real swing. Rather than leave a
            // blank gap, spanGaps bridges them with a SAME-COLOR DASHED segment:
            // solid where the rate is trustworthy, dashed where it's spanning a
            // dropped/thin bucket — via the segment.borderDash callback, which
            // dashes any segment touching a skipped point.
            data: { labels, datasets: [{
                data: points.map((p) => ((p.confirmed + p.failed) >= MIN_RATE_DENOM ? round1(p.success_rate) : null)),
                borderColor: c.vizPrimary, backgroundColor: c.vizPrimary, tension: 0.3, pointRadius: 0, fill: false, // success = white, not green (P18)
                spanGaps: true,
                segment: { borderDash: (ctx) => (ctx.p0.skip || ctx.p1.skip) ? [6, 6] : undefined },
            }] },
            options: { scales: { y: { min: 0, max: 100 } } },
        }));

        charts.push(buildChart(grid, 'duration', 'P50 / P95 duration (s)', {
            type: 'line',
            data: {
                labels,
                datasets: [
                    { label: 'P50', data: points.map((p) => msToS(p.p50_ms)), borderColor: c.vizPrimary, backgroundColor: c.vizPrimary, tension: 0.3, pointRadius: 0, fill: false }, // P18: P50 white
                    { label: 'P95', data: points.map((p) => msToS(p.p95_ms)), borderColor: c.vizSecondary, backgroundColor: c.vizSecondary, tension: 0.3, pointRadius: 0, fill: false }, // P18: P95 gray
                ],
            },
            options: { plugins: { legend: { display: true, labels: { color: c.text, boxWidth: 12 } } } },
        }));

        charts.push(buildChart(grid, 'cancellation', 'Cancellation & failure rate (%)', {
            type: 'line',
            data: {
                labels,
                datasets: [
                    { label: 'Cancelled', data: points.map((p) => p.total ? round1(p.cancelled / p.total * 100) : 0), borderColor: c.vizSecondary, backgroundColor: c.vizSecondary, tension: 0.3, pointRadius: 0, fill: false }, // P18: cancelled gray
                    { label: 'Failed', data: points.map((p) => p.total ? round1((p.failed || 0) / p.total * 100) : 0), borderColor: c.danger, backgroundColor: c.danger, tension: 0.3, pointRadius: 0, fill: false }, // P18: failure = red (semantic)
                ],
            },
            options: { scales: { y: { min: 0, max: 100 } }, plugins: { legend: { display: true, labels: { color: c.text, boxWidth: 12 } } } },
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

// windowFor maps the global ops range (today/7d/30d) to a date window + bucket
// granularity. 7d/30d bucket by DAY (readable axis); Today buckets hourly.
function windowFor(range) {
    const today = new Date(); today.setHours(0, 0, 0, 0);
    if (range === '30d') {
        const since = new Date(today); since.setDate(since.getDate() - 29);
        return { since: ymd(since), until: ymd(today), bucket: 'day' };
    }
    if (range === '7d') {
        const since = new Date(today); since.setDate(since.getDate() - 6);
        return { since: ymd(since), until: ymd(today), bucket: 'day' };
    }
    // 'today' — hourly across the current day.
    return { since: ymd(today), until: ymd(today), bucket: 'hour' };
}

function round1(v) { return Math.round((v || 0) * 10) / 10; }
function msToS(ms) { return Math.round((ms || 0) / 100) / 10; }
