// DrillModal — shared cross-section detail modal (plan §15 "Shared drill
// modal"). One component opened from every section's KPI / "View detail →"
// via data-action="openDrill:<metric>". Renders the metric over a longer
// range (4w/12w/52w) with wheel-zoom (chartjs-plugin-zoom), an auto-generated
// narrative line, Esc/backdrop/✕ close, and a 60s idle auto-dismiss. Cancels
// in-flight fetches on re-open/range-change (AbortController) and calls
// chart.destroy() on dismiss to avoid the Chart.js leak vector (§13).

import { el, h } from '/static/app.js';
import { makeChart, registerZoom, chartColors } from '/static/components/charts.js';

// Metric registry: most metrics are mission timeseries fields; duration is a
// two-series P50/P95; fleet/footprint/live degrade gracefully (their 12-week
// histories aren't materialized yet — Q-008/Q-011).
const METRICS = {
    success_rate: { title: 'Success rate', kind: 'mission', field: 'success_rate', type: 'line', unit: '%', max: 100 },
    completed: { title: 'Missions completed', kind: 'mission', field: 'confirmed', type: 'bar' },
    throughput: { title: 'Throughput', kind: 'mission', field: 'total', type: 'bar' },
    cancelled: { title: 'Cancelled missions', kind: 'mission', field: 'cancelled', type: 'bar' },
    cancellation: { title: 'Cancellation rate', kind: 'mission', field: 'cancellation_rate', type: 'line', unit: '%', max: 100 },
    avg_duration: { title: 'Duration (P50 / P95)', kind: 'duration' },
    duration: { title: 'Duration (P50 / P95)', kind: 'duration' },
    in_flight: { title: 'In flight', kind: 'note', note: 'In-flight is a live count — no historical series. Watch the hero tile.' },
    fleet_load: { title: 'Fleet load', kind: 'note', note: 'Peak-concurrency history needs the materialized typical-day aggregate (Q-008). Today’s curve is on the Robot Fleet section.' },
    footprint: { title: 'Plant footprint', kind: 'footprint' },
};

let _active = null;

export function openDrillModal(metric, filterState) {
    closeDrillModal();
    const cfg = METRICS[metric] || { title: metric, kind: 'note', note: 'No detail view for this metric.' };

    const overlay = el('div', { className: 'modal-overlay drill-modal active' });
    const box = el('div', { className: 'modal' });
    box.innerHTML = h`
        <div class="modal-header flex flex-between">
          <h2>${cfg.title}</h2>
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

    if (cfg.kind === 'note') {
        if (state.chart) { try { state.chart.destroy(); } catch (_) {} state.chart = null; }
        chartHolder.innerHTML = '<div class="dash-empty">' + cfg.note + '</div>';
        return;
    }

    // Ensure a canvas exists (note kind may have replaced it).
    if (!chartHolder.querySelector('canvas')) chartHolder.innerHTML = '<canvas></canvas>';

    if (state.controller) { try { state.controller.abort(); } catch (_) {} }
    state.controller = new AbortController();

    const url = buildURL(state);
    fetchJSON(url, state.controller.signal)
        .then((data) => render(state, data))
        .catch((err) => { if (err && err.name === 'AbortError') return; narrative.textContent = 'Failed to load detail.'; });
}

function buildURL(state) {
    const win = rangeWindow(state.range);
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

    if (cfg.kind === 'footprint') {
        const series = (data && data.load_series) || [];
        labels = series.map((b) => fmtDay(b.day));
        datasets = [
            { label: 'Loaded', data: series.map((b) => b.loaded), borderColor: c.success, backgroundColor: c.success, tension: 0.3, pointRadius: 0, fill: false },
            { label: 'Unloaded', data: series.map((b) => b.unloaded), borderColor: c.info, backgroundColor: c.info, tension: 0.3, pointRadius: 0, fill: false },
        ];
        narrativeSeries = series.map((b) => b.loaded);
    } else {
        const points = (data && data.points) || [];
        labels = points.map((p) => fmtDay(p.bucket_start));
        if (cfg.kind === 'duration') {
            datasets = [
                { label: 'P50 (s)', data: points.map((p) => msToS(p.p50_ms)), borderColor: c.info, backgroundColor: c.info, tension: 0.3, pointRadius: 0, fill: false },
                { label: 'P95 (s)', data: points.map((p) => msToS(p.p95_ms)), borderColor: c.warning, backgroundColor: c.warning, tension: 0.3, pointRadius: 0, fill: false },
            ];
            narrativeSeries = points.map((p) => msToS(p.p95_ms));
        } else {
            const vals = points.map((p) => fieldValue(p, cfg.field));
            datasets = [{ label: cfg.title, data: vals, borderColor: c.primary, backgroundColor: c.primary, tension: 0.3, pointRadius: 0, fill: cfg.type === 'bar' }];
            narrativeSeries = vals;
        }
    }

    if (!labels.length) {
        box.querySelector('.drill-modal__chart').innerHTML = '<div class="dash-empty">No data in this range.</div>';
        return;
    }

    const yMax = cfg.max;
    state.chart = makeChart(canvas, {
        type: cfg.type === 'bar' ? 'bar' : 'line',
        data: { labels, datasets },
        options: {
            scales: { y: yMax ? { min: 0, max: yMax } : { min: 0 } },
            plugins: {
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

function fieldValue(p, field) {
    if (field === 'cancellation_rate') return p.total ? Math.round(p.cancelled / p.total * 1000) / 10 : 0;
    if (field === 'success_rate') return Math.round((p.success_rate || 0) * 10) / 10;
    return p[field] || 0;
}

function rangeWindow(range) {
    const today = new Date(); today.setHours(0, 0, 0, 0);
    let days = 84;
    if (range === '4w') days = 28; else if (range === '52w') days = 364;
    const since = new Date(today); since.setDate(since.getDate() - (days - 1));
    return { since: ymd(since), until: ymd(today) };
}

function ymd(d) { return d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0'); }
function fmtDay(iso) { const d = new Date(iso); return isNaN(d.getTime()) ? '' : (d.getMonth() + 1) + '/' + d.getDate(); }
function msToS(ms) { return Math.round((ms || 0) / 100) / 10; }

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
