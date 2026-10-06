// DrillModal — shared cross-section detail modal (plan §15 "Shared drill
// modal"). One component opened from every section's KPI / "View detail →"
// via data-action="openDrill:<metric>". Renders the metric over a longer
// range (4w/12w/52w) with wheel-zoom (chartjs-plugin-zoom), an auto-generated
// narrative line, Esc/backdrop/✕ close, and a 60s idle auto-dismiss. Cancels
// in-flight fetches on re-open/range-change (AbortController) and calls
// chart.destroy() on dismiss to avoid the Chart.js leak vector (§13).

import { el, h } from '/static/app.js';
import { formatDuration } from '/static/shared/utils.js';
import { makeChart, registerZoom, chartColors, progressBarColors, progressSegment } from '/static/components/charts.js';
import { windowFor, bucketLabel, inProgress } from '/static/components/plantclock.js';

// Metric registry: most metrics are mission timeseries fields; duration is a
// two-series P50/P95 in minutes. A metric with no history (the live active
// count, fleet load before Q-008) has no entry and no drill affordance: a
// modal that opens on a placeholder sentence is a button that does nothing.
// Rates other than success scale to their data (a sub-1% rate on a 0-100 axis
// is a line on the floor); counts take integer ticks.
// RUN_TIME_TITLE says what "run time" is wherever it is shown: the Overview's
// average tile, the P50/P95 trend and this drill. It measures the work, so
// fault time is out; it is not the Missions list's Duration (created to the
// fleet's terminal report), which is why it is not called duration.
export const RUN_TIME_TITLE = 'Robot assigned to load down; fault time excluded';

const METRICS = {
    success_rate: { title: 'Success rate', kind: 'mission', field: 'success_rate', type: 'line', unit: '%', max: 100 },
    completed: { title: 'Missions completed', kind: 'mission', field: 'confirmed', type: 'bar', count: true },
    throughput: { title: 'Throughput', kind: 'mission', field: 'total', type: 'bar', count: true },
    cancelled: { title: 'Cancelled missions', kind: 'mission', field: 'cancelled', type: 'bar', count: true },
    cancellation: { title: 'Cancellation rate', kind: 'mission', field: 'cancellation_rate', type: 'line', unit: '%' },
    avg_duration: { title: 'Run time (P50 / P95)', hint: RUN_TIME_TITLE, kind: 'duration' },
    duration: { title: 'Run time (P50 / P95)', hint: RUN_TIME_TITLE, kind: 'duration' },
    footprint: { title: 'Plant footprint', kind: 'footprint' },
};

let _active = null;

export function openDrillModal(metric, filterState) {
    closeDrillModal();
    const cfg = METRICS[metric];
    if (!cfg) return;

    const overlay = el('div', { className: 'modal-overlay drill-modal active' });
    const box = el('div', { className: 'modal' });
    box.innerHTML = h`
        <div class="modal-header flex flex-between">
          <h2 title="${cfg.hint || ''}">${cfg.title}</h2>
          <button class="modal-close" title="Close">&times;</button>
        </div>
        <div class="range-toggle drill-range">
          <button data-range="4w">4w</button>
          <button data-range="12w" class="is-active">12w</button>
          <button data-range="52w">52w</button>
        </div>
        <div class="drill-modal__chart"><canvas></canvas></div>
        <div class="drill-modal__narrative"></div>`;
    overlay.appendChild(box);
    document.body.appendChild(overlay);

    const state = {
        overlay, box, cfg, metric,
        filter: filterState || {},
        range: '12w',
        chart: null,
        controller: null,
        idleTimer: null,
        onEsc: null,
    };
    _active = state;

    const close = () => closeDrillModal();
    box.querySelector('.modal-close').addEventListener('click', close);
    overlay.addEventListener('click', (e) => { if (e.target === overlay) close(); });
    state.onEsc = (e) => { if (e.key === 'Escape') close(); };
    document.addEventListener('keydown', state.onEsc);

    box.querySelectorAll('.drill-range button').forEach((btn) => {
        btn.addEventListener('click', () => {
            if (state.range === btn.dataset.range) return;
            state.range = btn.dataset.range;
            box.querySelectorAll('.drill-range button').forEach((b) => b.classList.toggle('is-active', b === btn));
            resetIdle(state);
            load(state);
        });
    });

    overlay.addEventListener('mousemove', () => resetIdle(state));
    resetIdle(state);
    load(state);
}

function resetIdle(state) {
    clearTimeout(state.idleTimer);
    state.idleTimer = setTimeout(() => closeDrillModal(), 60000);
}

function load(state) {
    const { cfg } = state;
    const chartHolder = state.box.querySelector('.drill-modal__chart');
    const narrative = state.box.querySelector('.drill-modal__narrative');
    narrative.textContent = '';

    // Ensure a canvas exists (an empty state may have replaced it).
    if (!chartHolder.querySelector('canvas')) chartHolder.innerHTML = '<canvas></canvas>';

    if (state.controller) { try { state.controller.abort(); } catch (_) {} }
    state.controller = new AbortController();

    const url = buildURL(state);
    fetchJSON(url, state.controller.signal)
        .then((data) => render(state, data))
        .catch((err) => { if (err && err.name === 'AbortError') return; narrative.textContent = 'Failed to load detail.'; });
}

function buildURL(state) {
    const win = windowFor(state.range);
    const p = new URLSearchParams({ bucket: 'day', since: win.since, until: win.until });
    if (state.filter.station) p.set('station_id', state.filter.station);
    if (state.filter.robot) p.set('robot_id', state.filter.robot);
    if (state.cfg.kind === 'footprint') return '/api/footprint';
    return '/api/missions/timeseries?' + p.toString();
}

function render(state, data) {
    const { cfg, box } = state;
    const canvas = box.querySelector('.drill-modal__chart canvas');
    if (!canvas) return;
    if (state.chart) { try { state.chart.destroy(); } catch (_) {} state.chart = null; }
    const c = chartColors();
    registerZoom();

    let labels = [];
    let datasets = [];
    let narrativeSeries = [];
    let live = [];
    let hasRows = false;

    // Same palette as the page charts (guide: The palette): loaded teal /
    // unloaded indigo as on the footprint card, P50 sky / P95 violet as on the
    // trends card, a single metric in indigo (series-1).
    if (cfg.kind === 'footprint') {
        const series = (data && data.load_series) || [];
        labels = series.map((b) => bucketLabel(b.day, 'day'));
        live = series.map((b) => inProgress(b.day, 'day'));
        datasets = [
            { label: 'Loaded', data: series.map((b) => b.loaded), borderColor: c.vizTeal, backgroundColor: c.vizTeal, fill: false, segment: progressSegment(live) },
            { label: 'Unloaded', data: series.map((b) => b.unloaded), borderColor: c.vizIndigo, backgroundColor: c.vizIndigo, fill: false, segment: progressSegment(live) },
        ];
        narrativeSeries = series.map((b) => b.loaded);
        hasRows = series.some((b) => b.loaded > 0 || b.unloaded > 0);
    } else {
        const points = (data && data.points) || [];
        labels = points.map((p) => bucketLabel(p.bucket_start, 'day'));
        live = points.map((p) => inProgress(p.bucket_start, 'day'));
        hasRows = points.some((p) => p.total > 0);
        if (cfg.kind === 'duration') {
            const p95 = points.map((p) => (p.p95_ms > 0 ? toMin(p.p95_ms) : null));
            datasets = [
                { label: 'P50 (min)', data: points.map((p) => (p.p50_ms > 0 ? toMin(p.p50_ms) : null)), borderColor: c.vizSky, backgroundColor: c.vizSky, fill: false, segment: progressSegment(live) },
                { label: 'P95 (min)', data: p95, borderColor: c.vizViolet, backgroundColor: c.vizViolet, fill: false, segment: progressSegment(live) },
            ];
            narrativeSeries = p95;
            hasRows = p95.some((v) => v !== null);
        } else {
            const vals = points.map((p) => fieldValue(p, cfg.field));
            datasets = [cfg.type === 'bar'
                ? { label: cfg.title, data: vals, backgroundColor: progressBarColors(c.vizIndigo, live) }
                : { label: cfg.title, data: vals, borderColor: c.vizIndigo, backgroundColor: c.vizIndigo, fill: false, segment: progressSegment(live) }];
            narrativeSeries = vals;
        }
    }
    // The in-progress day is drawn, but kept out of the trend sentence: a
    // partly-elapsed day is not a fall.
    narrativeSeries = narrativeSeries.filter((_, i) => !live[i]);

    if (!hasRows) {
        box.querySelector('.drill-modal__chart').innerHTML = '<div class="dash-empty">No data in this range.</div>';
        return;
    }

    const y = cfg.max ? { min: 0, max: cfg.max } : { beginAtZero: true };
    if (cfg.count || cfg.kind === 'footprint') y.ticks = { precision: 0 };
    const tooltip = cfg.kind === 'duration'
        ? { callbacks: { label: (ctx) => ctx.dataset.label.replace(' (min)', '') + ' ' + formatDuration(ctx.parsed.y * 60000) } }
        : {};
    state.chart = makeChart(canvas, {
        type: cfg.type === 'bar' ? 'bar' : 'line',
        data: { labels, datasets },
        options: {
            scales: { y },
            plugins: {
                tooltip,
                legend: { display: datasets.length > 1, labels: { color: c.text, boxWidth: 12 } },
                zoom: {
                    zoom: { wheel: { enabled: true }, pinch: { enabled: true }, mode: 'x' },
                    pan: { enabled: true, mode: 'x' },
                },
            },
        },
    });

    box.querySelector('.drill-modal__narrative').textContent = narrate(cfg.title, narrativeSeries, state.range);
}

export function closeDrillModal() {
    if (!_active) return;
    const a = _active;
    _active = null;
    clearTimeout(a.idleTimer);
    if (a.onEsc) document.removeEventListener('keydown', a.onEsc);
    if (a.controller) { try { a.controller.abort(); } catch (_) {} }
    if (a.chart) { try { a.chart.destroy(); } catch (_) {} }
    if (a.overlay && a.overlay.parentNode) a.overlay.parentNode.removeChild(a.overlay);
}

// ─── helpers ──────────────────────────────────────────────────────────────
function fetchJSON(url, signal) {
    return fetch(url, { signal }).then((r) => { if (!r.ok) throw new Error('http ' + r.status); return r.json(); });
}

// fieldValue reads one metric off a bucket. A rate over an empty bucket has no
// value (null, a gap), not 0%: the series is zero-filled, and a day nothing
// finished on is not a day everything failed.
function fieldValue(p, field) {
    if (field === 'cancellation_rate') return p.total ? Math.round(p.cancelled / p.total * 1000) / 10 : null;
    if (field === 'success_rate') return (p.confirmed + p.failed) > 0 ? Math.round(p.success_rate * 10) / 10 : null;
    return p[field] || 0;
}

function toMin(ms) { return Math.round(ms / 600) / 100; }

// narrate computes a one-sentence trend summary from first→last (§15 auto
// narrative).
function narrate(title, values, range) {
    const v = (values || []).filter((x) => typeof x === 'number' && isFinite(x));
    if (v.length < 2) return '';
    const first = v.find((x) => x > 0);
    const last = v[v.length - 1];
    const label = range === '4w' ? '4 weeks' : range === '52w' ? '52 weeks' : '12 weeks';
    if (!first || !last) return title + ' over the last ' + label + '.';
    const ratio = last / first;
    if (ratio >= 1.15) return title + ' grew ' + ratio.toFixed(1) + '× over the last ' + label + '.';
    if (ratio <= 0.87) return title + ' fell ' + Math.round((1 - ratio) * 100) + '% over the last ' + label + '.';
    return title + ' held roughly steady over the last ' + label + '.';
}
