// Overview Section B — Mission trends (plan §15.B). A 2×2 Chart.js grid:
// throughput, success rate, P50/P95 duration, cancellation rate. Driven
// entirely by the global ops filter (Today/7d/30d + station/robot) — wave 2
// removed the section-local range toggle (Q-035), so Overview has one range
// control. Today buckets hourly; 7d/30d bucket by DAY so the x-axis is readable
// (hourly labels repeated 08:00…08:00 across days were unreadable). One
// /api/missions/timeseries fetch powers all four.

import { apiGet } from '/static/app.js';
import { formatDuration } from '/static/shared/utils.js';
import { makeChart, installChartThemeHook, chartColors, withAlpha, progressBarColors, progressSegment } from '/static/components/charts.js';
import { windowFor, bucketLabel, inProgress } from '/static/components/plantclock.js';
import { RUN_TIME_TITLE } from '/static/components/DrillModal.js';

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
            .then((res) => render((res && res.points) || [], win))
            .catch(() => render([], win));
    }

    function destroyCharts() {
        for (const c of charts) { try { c.destroy(); } catch (_) {} }
        charts = [];
    }

    // The series is continuous (the server zero-fills every bucket up to now),
    // so "no rows" means no bucket finished a mission, not an empty array.
    function render(points, win) {
        destroyCharts();
        const grid = document.getElementById(gridId);
        if (!grid) return;
        grid.innerHTML = '';
        const bucket = win.bucket;
        if (!points.some((p) => p.total > 0)) {
            grid.innerHTML = '<div class="dash-empty">No missions finished ' + windowText(win) + '.</div>';
            return;
        }
        const c = chartColors();
        const labels = points.map((p) => bucketLabel(p.bucket_start, bucket));
        // The bucket now falls in has not finished: washed-out bar, dashed line.
        const live = points.map((p) => inProgress(p.bucket_start, bucket));

        charts.push(buildChart(grid, 'throughput', 'Throughput (missions per ' + bucket + ')', {
            type: 'bar',
            data: { labels, datasets: [{ data: points.map((p) => p.total), backgroundColor: progressBarColors(c.vizIndigo, live), borderRadius: 2 }] }, // throughput bars = indigo / series-1 (P19)
            options: { scales: { y: { beginAtZero: true, ticks: { precision: 0 } } } },
        }));

        // Thin buckets (<MIN_RATE_DENOM finished missions) are plotted as null
        // so 100/0 noise doesn't read as a real swing. spanGaps bridges them with
        // a SAME-COLOR DASHED segment: solid where the rate is trustworthy,
        // dashed where it spans a thin bucket or runs into the bucket in progress.
        const rates = points.map((p) => ((p.confirmed + p.failed) >= MIN_RATE_DENOM ? round1(p.success_rate) : null));
        charts.push(buildChart(grid, 'success_rate', 'Success rate (%)', {
            type: 'line',
            data: { labels, datasets: [{
                data: rates,
                borderColor: c.vizGreen, backgroundColor: withAlpha(c.vizGreen, 0.13), fill: true, // success = green + soft fill (P19)
                spanGaps: true,
                segment: progressSegment(live, (ctx) => (ctx.p0.skip || ctx.p1.skip) ? [6, 6] : undefined),
            }] },
            options: { scales: { y: { min: 0, max: 100 } } },
        }, hasValue(rates) ? '' : 'No ' + bucket + ' finished ' + MIN_RATE_DENOM + ' or more missions ' + windowText(win) + ', so no rate is plotted.'));

        // Durations plot in minutes; the tooltip prints the compound duration.
        // A bucket with no finished robot mission has no duration (null), not 0.
        const p50 = points.map((p) => (p.p50_ms > 0 ? toMin(p.p50_ms) : null));
        const p95 = points.map((p) => (p.p95_ms > 0 ? toMin(p.p95_ms) : null));
        charts.push(buildChart(grid, 'duration', 'P50 / P95 run time (min)', {
            type: 'line',
            data: {
                labels,
                datasets: [
                    { label: 'P50', data: p50, borderColor: c.vizSky, backgroundColor: c.vizSky, fill: false, segment: progressSegment(live) }, // P19: P50 sky
                    { label: 'P95', data: p95, borderColor: c.vizViolet, backgroundColor: c.vizViolet, fill: false, segment: progressSegment(live) }, // P19: P95 violet
                ],
            },
            options: {
                scales: { y: { beginAtZero: true } },
                plugins: {
                    legend: { display: true, labels: { color: c.text, boxWidth: 12 } },
                    tooltip: { callbacks: { label: (ctx) => ctx.dataset.label + ' ' + formatDuration(ctx.parsed.y * 60000) } },
                },
            },
        }, (hasValue(p50) || hasValue(p95)) ? '' : 'No robot mission finished ' + windowText(win) + '.', RUN_TIME_TITLE));

        // Scales to its data: rates here sit well under 1%, and a pinned 0–100
        // axis flattened them onto the floor.
        charts.push(buildChart(grid, 'cancellation', 'Cancellation & failure rate (%)', {
            type: 'line',
            data: {
                labels,
                datasets: [
                    { label: 'Cancelled', data: points.map((p) => p.total ? round1(p.cancelled / p.total * 100) : null), borderColor: c.vizAmber, backgroundColor: c.vizAmber, fill: false, segment: progressSegment(live) }, // P19: cancelled amber
                    { label: 'Failed', data: points.map((p) => p.total ? round1((p.failed || 0) / p.total * 100) : null), borderColor: c.vizCoral, backgroundColor: c.vizCoral, fill: false, segment: progressSegment(live) }, // P19: failure = coral (semantic)
                ],
            },
            options: { scales: { y: { beginAtZero: true } }, plugins: { legend: { display: true, labels: { color: c.text, boxWidth: 12 } } } },
        }));

        // Initial draw animates; subsequent data sets shouldn't (§4) — handled
        // by rebuilding fresh charts each refresh, so no per-update jitter.
    }

    // buildChart draws one cell. emptyText, when set, replaces the chart with
    // the guide's empty state: an axis frame with nothing in it reads as "all
    // zero", which is a different claim.
    function buildChart(grid, metric, caption, config, emptyText, title) {
        const cell = document.createElement('div');
        cell.innerHTML = '<div class="chart-caption">' + caption + '</div>';
        if (title) cell.firstChild.title = title;
        grid.appendChild(cell);
        if (emptyText) {
            const empty = document.createElement('div');
            empty.className = 'dash-empty';
            empty.textContent = emptyText;
            cell.appendChild(empty);
            return null;
        }
        const box = document.createElement('div');
        box.className = 'chart-box';
        box.style.height = '200px';
        box.dataset.action = 'openDrill:' + metric; // §15.B click → drill modal
        const canvas = document.createElement('canvas');
        box.appendChild(canvas);
        cell.appendChild(box);
        return makeChart(canvas, config);
    }

    return { mount, refresh };
}

// ─── helpers ──────────────────────────────────────────────────────────────
function windowText(win) {
    return win.days === 1 ? 'today' : 'in the last ' + win.days + ' days';
}
function hasValue(series) { return series.some((v) => v !== null && v !== undefined); }
function round1(v) { return Math.round((v || 0) * 10) / 10; }
function toMin(ms) { return Math.round(ms / 600) / 100; }
