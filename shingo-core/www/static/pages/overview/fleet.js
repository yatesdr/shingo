// Overview Section C — Robot fleet, three-layer utilization framing
// (plan §15.C). Top KPI strip (fleet size / online / missions / utilization),
// the Fleet Load chart (hourly concurrency vs the fleet ceiling), and
// per-robot rows with mission-derived utilization bars. Data:
// /api/robots/fleet. The typical-day overlay is deferred (Q-008).

import { apiGet, h, setText } from '/static/app.js';
import { reconcileList } from '/static/shared/utils.js';
import { makeChart, installChartThemeHook, chartColors, withAlpha, progressSegment } from '/static/components/charts.js';
import { windowFor, bucketLabel, plantDate, inProgress } from '/static/components/plantclock.js';
import { createFleetRow, updateFleetRow } from '/static/components/RobotTile.js';

export function createFleetSection(store) {
    let chart = null;

    function mount() {
        installChartThemeHook();
        const body = document.getElementById('ops-fleet-body');
        if (!body) return;
        // P8b: dual hero (utilization + peak), supporting stats demoted, chart kept.
        // The rows carry three bare numbers, so a header row in the rows' own grid
        // names them.
        body.innerHTML = h`
            <div class="ov-hero-row">
              <div class="ov-hero">
                <div class="ov-hero__value" id="fl-util">—</div>
                <div class="ov-hero__label">Fleet utilization</div>
              </div>
              <div class="ov-hero">
                <div class="ov-hero__value" id="fl-peak">—</div>
                <div class="ov-hero__label">Peak concurrency</div>
                <div class="ov-hero__sub" id="fl-peak-sub"></div>
              </div>
            </div>
            <div class="ov-support">
              <div class="ov-support__item"><div class="ov-support__value" id="fl-size">—</div><div class="ov-support__label">In fleet</div></div>
              <div class="ov-support__item"><div class="ov-support__value" id="fl-online">—</div><div class="ov-support__label">Online</div></div>
              <div class="ov-support__item"><div class="ov-support__value" id="fl-missions">—</div><div class="ov-support__label">Missions (window)</div></div>
              <div class="ov-support__item"><div class="ov-support__value" id="fl-avgload">—</div><div class="ov-support__label">Avg load</div></div>
            </div>
            <div class="fleet-load-box">
              <div class="chart-caption" id="fl-day"></div>
              <div class="chart-box" style="height:200px;margin-top:0.35rem"><canvas id="fl-canvas"></canvas></div>
            </div>
            <div class="fleet-row text-muted-sm">
              <span>Robot</span><span>State</span>
              <span title="Share of the window this robot spent on orders">Busy</span>
              <span>Missions</span><span>Battery</span>
            </div>
            <div class="bar-list" id="fl-rows"></div>`;
    }

    function refresh(state) {
        const p = new URLSearchParams();
        const win = windowFor(state.range);
        p.set('since', win.since); p.set('until', win.until);
        if (state.station) p.set('station_id', state.station);
        if (state.robot) p.set('robot_id', state.robot);
        apiGet('/api/robots/fleet?' + p.toString())
            .then(render)
            .catch(() => { const b = document.getElementById('ops-fleet-body'); if (b) b.innerHTML = '<div class="dash-empty">Fleet data unavailable.</div>'; });
    }

    function render(data) {
        if (!data || !data.fleet) return;
        const f = data.fleet;
        // Dual hero.
        setText('fl-util', (f.util_pct || 0).toFixed(0) + '%');
        setText('fl-peak', (f.peak_concurrency != null ? f.peak_concurrency : '—') + ' / ' + f.size);
        // "at ceiling" is the one amber (semantic limit) — P18; otherwise a muted peak-hour note.
        var peakSub = document.getElementById('fl-peak-sub');
        if (peakSub) {
            if (f.ceiling_reached) { peakSub.textContent = 'at ceiling'; peakSub.classList.add('is-ceiling'); }
            else { peakSub.textContent = f.peak_concurrency ? ('peak @ ' + (f.peak_hour || '—')) : 'no peak in window'; peakSub.classList.remove('is-ceiling'); }
        }
        // Supporting row (demoted).
        setText('fl-size', f.size);
        setText('fl-online', f.online + ' / ' + f.size);
        setText('fl-missions', f.missions);
        setText('fl-avgload', (f.avg_load || 0).toFixed(1));

        renderChart(data.load_series || [], data.typical_series || [], f.size, data.load_granularity || 'hour');
        renderRows(data.robots || []);
    }

    // renderChart draws the Fleet Load curve. granularity 'hour' (Today) is the
    // intraday concurrency curve for the viewed day; 'day' (7d/30d) is a per-day
    // peak/avg rollup across the range, so the chart honors the range selector.
    function renderChart(load, typical, size, granularity) {
        const canvas = document.getElementById('fl-canvas');
        if (!canvas) return;
        if (chart) { try { chart.destroy(); } catch (_) {} chart = null; }
        if (!load.length) {
            const dayLabel = document.getElementById('fl-day');
            if (dayLabel) dayLabel.textContent = '';
            const box = canvas.parentElement;
            if (box) { box.style.height = 'auto'; box.innerHTML = '<div class="dash-empty">No fleet activity in this window.</div>'; }
            return;
        }
        const c = chartColors();
        const ceiling = Math.max(size, 1);
        const dayLabel = document.getElementById('fl-day');
        const daily = granularity === 'day';

        let labels, datasets;
        if (daily) {
            labels = load.map((d) => bucketLabel(d.day, 'day'));
            const live = load.map((d) => inProgress(d.day, 'day'));
            if (dayLabel) dayLabel.textContent = 'Fleet load — daily peak / avg robots used across range';
            // Fill the AVERAGE (typical usage) and draw peak as a thin envelope
            // line above it — filling under peak overstated usage (it made a big
            // mountain when the floor mostly uses ~1 robot). Avg first so its
            // fill sits behind the peak line.
            datasets = [{
                label: 'Avg robots used', data: load.map((d) => Math.round((d.avg || 0) * 10) / 10),
                borderColor: c.vizTeal, backgroundColor: withAlpha(c.vizTeal, 0.13), // P19: avg = teal + soft fill
                fill: true, segment: progressSegment(live),
            }, {
                label: 'Peak robots used', data: load.map((d) => d.peak),
                borderColor: c.vizIndigo, borderWidth: 1.4, fill: false, segment: progressSegment(live), // P19: peak = indigo line
            }, {
                label: 'Fleet ceiling', data: labels.map(() => ceiling),
                borderColor: c.vizAmber, borderDash: [6, 4], borderWidth: 1, pointRadius: 0, fill: false,
            }];
        } else {
            const live = load.map((h2) => inProgress(h2.hour, 'hour'));
            labels = load.map((h2) => bucketLabel(h2.hour, 'hour'));
            if (dayLabel) {
                const day = load[0] ? plantDate(load[0].hour) : '';
                dayLabel.textContent = 'Fleet load — ' + (day || 'latest day');
            }
            datasets = [{
                label: 'Robots used', data: load.map((h2) => h2.concurrency),
                borderColor: c.vizTeal, backgroundColor: withAlpha(c.vizTeal, 0.13), // P19: teal line + soft fill
                fill: true, segment: progressSegment(live),
            }, {
                label: 'Fleet ceiling', data: labels.map(() => ceiling),
                borderColor: c.vizAmber, borderDash: [6, 4], borderWidth: 1, pointRadius: 0, fill: false,
            }];
            // Typical-day overlay (deferred — empty until materialized, Q-008).
            if (typical && typical.length === load.length) {
                datasets.push({
                    label: 'Typical', data: typical.map((t) => t.concurrency != null ? t.concurrency : t),
                    borderColor: c.vizSecondary, borderDash: [3, 3], borderWidth: 1, pointRadius: 0, fill: false, // P18: context = gray
                });
            }
        }
        chart = makeChart(canvas, {
            type: 'line',
            data: { labels, datasets },
            options: {
                scales: { y: { min: 0, suggestedMax: ceiling, ticks: { precision: 0 } } },
                plugins: { legend: { display: true, labels: { color: c.text, boxWidth: 12 } } },
            },
        });
    }

    function renderRows(robots) {
        const container = document.getElementById('fl-rows');
        if (!container) return;
        reconcileList(container, robots, {
            key: (r) => r.vehicle_id,
            create: (r) => createFleetRow(r),
            update: (node, r) => updateFleetRow(node, r),
        });
    }

    return { mount, refresh };
}
