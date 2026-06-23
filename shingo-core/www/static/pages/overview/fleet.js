// Overview Section C — Robot fleet, three-layer utilization framing
// (plan §15.C). Top KPI strip (fleet size / online / missions / utilization),
// the Fleet Load chart (hourly concurrency vs the fleet ceiling), and
// per-robot rows with mission-derived utilization bars. Data:
// /api/robots/fleet. The typical-day overlay is deferred (Q-008).

import { apiGet, h } from '/static/app.js';
import { reconcileList } from '/static/shared/utils.js';
import { makeChart, installChartThemeHook, chartColors } from '/static/components/charts.js';
import { createFleetRow, updateFleetRow } from '/static/components/RobotTile.js';

export function createFleetSection(store) {
    let chart = null;

    function mount() {
        installChartThemeHook();
        const body = document.getElementById('ops-fleet-body');
        if (!body) return;
        body.innerHTML = h`
            <div class="grid grid-4 fleet-kpis" id="fl-kpis">
              <div class="card" style="margin:0"><div class="kpi-label">In fleet</div><div class="stat-value" id="fl-size">—</div></div>
              <div class="card" style="margin:0"><div class="kpi-label">Online</div><div class="stat-value" id="fl-online">—</div></div>
              <div class="card" style="margin:0"><div class="kpi-label">Missions (window)</div><div class="stat-value" id="fl-missions">—</div></div>
              <div class="card" style="margin:0"><div class="kpi-label">Fleet utilization</div><div class="stat-value" id="fl-util">—</div></div>
            </div>
            <div class="fleet-load-box">
              <div class="flex flex-between" style="align-items:flex-start">
                <div>
                  <div class="fleet-headline" id="fl-head">&mdash;</div>
                  <div class="fleet-headline--peak" id="fl-peak"></div>
                </div>
                <span class="ceiling-pill" id="fl-ceiling" style="display:none">ceiling reached</span>
              </div>
              <div class="chart-caption" id="fl-day" style="margin-top:0.35rem"></div>
              <div class="chart-box" style="height:200px;margin-top:0.5rem"><canvas id="fl-canvas"></canvas></div>
              <div class="fleet-summary" id="fl-summary"></div>
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
        setText('fl-size', f.size);
        setText('fl-online', f.online + ' / ' + f.size);
        setText('fl-missions', f.missions);
        setText('fl-util', (f.util_pct || 0).toFixed(0) + '%');

        const head = document.getElementById('fl-head');
        if (head) head.textContent = (f.avg_load || 0).toFixed(1) + ' / ' + f.size + ' robots used on average · ' + (f.util_pct || 0).toFixed(0) + '% fleet utilization';
        const peak = document.getElementById('fl-peak');
        if (peak) peak.textContent = f.peak_concurrency ? ('Peak: ' + f.peak_concurrency + ' robots @ ' + (f.peak_hour || '—')) : 'No peak in window';
        const ceil = document.getElementById('fl-ceiling');
        if (ceil) ceil.style.display = f.ceiling_reached ? '' : 'none';

        renderSummary(f);
        renderChart(data.load_series || [], data.typical_series || [], f.size, data.load_granularity || 'hour');
        renderRows(data.robots || []);
    }

    function renderSummary(f) {
        const el = document.getElementById('fl-summary');
        if (!el) return;
        const cells = [
            ['Avg load', (f.avg_load || 0).toFixed(1)],
            ['vs typical', f.typical_load != null ? (f.avg_load - f.typical_load).toFixed(1) : '—'],
            ['At ceiling', f.ceiling_reached ? 'yes' : 'no'],
            ['Headroom', (f.headroom != null ? f.headroom.toFixed(1) : '—')],
        ];
        el.innerHTML = cells.map((c) => '<div><div class="v">' + c[1] + '</div><div class="k">' + c[0] + '</div></div>').join('');
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
            if (box) box.innerHTML = '<div class="dash-empty">No fleet activity in this window.</div>';
            return;
        }
        const c = chartColors();
        const ceiling = Math.max(size, 1);
        const dayLabel = document.getElementById('fl-day');
        const daily = granularity === 'day';

        let labels, datasets;
        if (daily) {
            labels = load.map((d) => fmtDay(d.day));
            if (dayLabel) dayLabel.textContent = 'Fleet load — daily peak / avg robots used across range';
            // Fill the AVERAGE (typical usage) and draw peak as a thin envelope
            // line above it — filling under peak overstated usage (it made a big
            // mountain when the floor mostly uses ~1 robot). Avg first so its
            // fill sits behind the peak line.
            datasets = [{
                label: 'Avg robots used', data: load.map((d) => Math.round((d.avg || 0) * 10) / 10),
                borderColor: c.success, backgroundColor: withAlpha(c.success, 0.18),
                fill: true, tension: 0.3, pointRadius: 0,
            }, {
                label: 'Peak robots used', data: load.map((d) => d.peak),
                borderColor: c.info, borderWidth: 1.4, pointRadius: 0, fill: false, tension: 0.3,
            }, {
                label: 'Fleet ceiling', data: labels.map(() => ceiling),
                borderColor: c.warning, borderDash: [6, 4], borderWidth: 1, pointRadius: 0, fill: false,
            }];
        } else {
            labels = load.map((h2) => fmtHour(h2.hour));
            if (dayLabel) {
                const d0 = load[0] && load[0].hour ? new Date(load[0].hour) : null;
                const day = d0 && !isNaN(d0.getTime()) ? ymd(d0) : '';
                dayLabel.textContent = 'Fleet load — ' + (day || 'latest day');
            }
            datasets = [{
                label: 'Robots used', data: load.map((h2) => h2.concurrency),
                borderColor: c.info, backgroundColor: withAlpha(c.info, 0.18),
                fill: true, tension: 0.3, pointRadius: 0,
            }, {
                label: 'Fleet ceiling', data: labels.map(() => ceiling),
                borderColor: c.warning, borderDash: [6, 4], borderWidth: 1, pointRadius: 0, fill: false,
            }];
            // Typical-day overlay (deferred — empty until materialized, Q-008).
            if (typical && typical.length === load.length) {
                datasets.push({
                    label: 'Typical', data: typical.map((t) => t.concurrency != null ? t.concurrency : t),
                    borderColor: c.text, borderDash: [3, 3], borderWidth: 1, pointRadius: 0, fill: false,
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

// ─── helpers ──────────────────────────────────────────────────────────────
function setText(id, v) { const e = document.getElementById(id); if (e) e.textContent = v; }
function fmtHour(iso) { const d = new Date(iso); return isNaN(d.getTime()) ? '' : String(d.getHours()).padStart(2, '0') + ':00'; }
function fmtDay(iso) { const d = new Date(iso); return isNaN(d.getTime()) ? '' : (d.getMonth() + 1) + '/' + d.getDate(); }

function ymd(d) { return d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0'); }
function windowFor(range) {
    const today = new Date(); today.setHours(0, 0, 0, 0);
    let days = 1;
    if (range === '7d') days = 7; else if (range === '30d') days = 30;
    const since = new Date(today); since.setDate(since.getDate() - (days - 1));
    return { since: ymd(since), until: ymd(today) };
}

// withAlpha turns a CSS color into a translucent fill. Handles hex; falls
// back to color-mix for var()/named colors.
function withAlpha(color, a) {
    if (color && color[0] === '#') {
        let hex = color.slice(1);
        if (hex.length === 3) hex = hex.split('').map((x) => x + x).join('');
        const n = parseInt(hex, 16);
        return 'rgba(' + ((n >> 16) & 255) + ',' + ((n >> 8) & 255) + ',' + (n & 255) + ',' + a + ')';
    }
    return 'color-mix(in srgb, ' + color + ' ' + Math.round(a * 100) + '%, transparent)';
}
