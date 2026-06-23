// Overview Section D — Plant footprint (plan §15.D). The "how much has shingo
// taken over" exec narrative: cells/bins under management with growth
// sparklines, plus a combined daily loaded/unloaded velocity chart. Data:
// /api/footprint (plant-wide; ignores the station/robot filters).

import { apiGet, h } from '/static/app.js';
import { Sparkline } from '/static/components/Sparkline.js';
import { makeChart, installChartThemeHook, chartColors } from '/static/components/charts.js';

export function createFootprintSection(store) {
    let chart = null;
    let binsChart = null;

    function mount() {
        installChartThemeHook();
        const body = document.getElementById('ops-footprint-body');
        if (!body) return;
        // P8b: Bins managed is the hero (full/empty as its subtitle); Lines and
        // Processes demote to the supporting row.
        body.innerHTML = h`
            <div class="ov-hero">
              <div class="ov-hero__value" id="fp-bins">—</div>
              <div class="ov-hero__label">Bins managed</div>
              <div class="ov-hero__sub" id="fp-bins-split"></div>
            </div>
            <div class="ov-support">
              <div class="ov-support__item">
                <div class="ov-support__value" id="fp-cells">—</div>
                <div class="ov-support__label">Lines</div>
                <div id="fp-cells-spark" class="kpi-spark"></div>
              </div>
              <div class="ov-support__item">
                <div class="ov-support__value" id="fp-processes">—</div>
                <div class="ov-support__label">Processes managed</div>
              </div>
            </div>
            <div class="chart-caption" style="margin-top:1rem">Bins managed — full vs empty (last 30d)</div>
            <div class="chart-box" style="height:180px"><canvas id="fp-bins-canvas"></canvas></div>
            <div class="chart-caption" style="margin-top:1rem">Bins loaded / unloaded per day (last 30d)</div>
            <div class="chart-box" style="height:180px"><canvas id="fp-canvas"></canvas></div>`;
    }

    // Footprint is plant-wide; the same data regardless of filter, so we fetch
    // once on mount and ignore filter-driven refreshes after the first.
    let loaded = false;
    function refresh() {
        if (loaded) return;
        apiGet('/api/footprint').then((fp) => { loaded = true; render(fp); }).catch(() => {});
    }

    function render(fp) {
        if (!fp) return;
        setText('fp-cells', fp.cells_managed); // edge/line registrations (Q-034: not physical cells yet)
        setText('fp-processes', fp.processes_managed);
        setText('fp-bins', fp.bins_managed); // total stays the headline
        setText('fp-bins-split', binSplitText(fp));
        spark('fp-cells-spark', fp.cells_spark, 'var(--success)');
        renderBinsChart(fp.bins_series || []);
        renderChart(fp.load_series || []);
    }

    // renderBinsChart draws the reconstructed full-vs-empty occupancy as a
    // stacked area (full + empty = total bins managed) over the last 30 days.
    function renderBinsChart(series) {
        const canvas = document.getElementById('fp-bins-canvas');
        if (!canvas) return;
        if (binsChart) { try { binsChart.destroy(); } catch (_) {} binsChart = null; }
        // Only draw once there's a day with bins; otherwise the reconstruction
        // log doesn't reach back far enough yet.
        if (!series.length || !series.some((b) => b.total > 0)) {
            const box = canvas.parentElement;
            if (box) box.innerHTML = '<div class="dash-empty">No bin history in range yet (fills in as the uop log grows).</div>';
            return;
        }
        const c = chartColors();
        const labels = series.map((b) => fmtDay(b.day));
        binsChart = makeChart(canvas, {
            type: 'line',
            data: {
                labels,
                datasets: [
                    { label: 'Full', data: series.map((b) => b.full), borderColor: c.info, backgroundColor: withAlpha(c.info, 0.45), fill: true, stack: 'bins', tension: 0.2, pointRadius: 0 },
                    { label: 'Empty', data: series.map((b) => b.empty), borderColor: c.text, backgroundColor: withAlpha(c.text, 0.18), fill: true, stack: 'bins', tension: 0.2, pointRadius: 0 },
                ],
            },
            options: { scales: { y: { min: 0, stacked: true, ticks: { precision: 0 } } }, plugins: { legend: { display: true, labels: { color: c.text, boxWidth: 12 } } } },
        });
    }

    function renderChart(series) {
        const canvas = document.getElementById('fp-canvas');
        if (!canvas) return;
        if (chart) { try { chart.destroy(); } catch (_) {} chart = null; }
        if (!series.length) {
            const box = canvas.parentElement;
            if (box) box.innerHTML = '<div class="dash-empty">No load/unload activity in the last 30 days.</div>';
            return;
        }
        const c = chartColors();
        const labels = series.map((b) => fmtDay(b.day));
        chart = makeChart(canvas, {
            type: 'line',
            data: {
                labels,
                datasets: [
                    { label: 'Loaded', data: series.map((b) => b.loaded), borderColor: c.success, backgroundColor: c.success, tension: 0.3, pointRadius: 0, fill: false },
                    { label: 'Unloaded', data: series.map((b) => b.unloaded), borderColor: c.info, backgroundColor: c.info, tension: 0.3, pointRadius: 0, fill: false },
                ],
            },
            options: { scales: { y: { min: 0, ticks: { precision: 0 } } }, plugins: { legend: { display: true, labels: { color: c.text, boxWidth: 12 } } } },
        });
    }

    return { mount, refresh };
}

function setText(id, v) { const e = document.getElementById(id); if (e) e.textContent = (v === null || v === undefined) ? '—' : v; }
function spark(id, data, color) {
    const el = document.getElementById(id);
    if (!el) return;
    el.textContent = '';
    if (Array.isArray(data) && data.length >= 2) el.appendChild(Sparkline(data, { color, width: 160, height: 18 }));
}
// binSplitText renders the current full/empty split (Q-032) under the Bins-
// managed headline. full = uop_remaining > 0; full + empty = total.
function binSplitText(fp) {
    return (fp.bins_full || 0) + ' full · ' + (fp.bins_empty || 0) + ' empty';
}
// withAlpha turns a CSS color into a translucent fill for the stacked area.
// Handles hex; falls back to color-mix for var()/named colors.
function withAlpha(color, a) {
    if (color && color[0] === '#') {
        let hex = color.slice(1);
        if (hex.length === 3) hex = hex.split('').map((x) => x + x).join('');
        const n = parseInt(hex, 16);
        return 'rgba(' + ((n >> 16) & 255) + ',' + ((n >> 8) & 255) + ',' + (n & 255) + ',' + a + ')';
    }
    return 'color-mix(in srgb, ' + color + ' ' + Math.round(a * 100) + '%, transparent)';
}
function fmtDay(iso) { const d = new Date(iso); return isNaN(d.getTime()) ? '' : (d.getMonth() + 1) + '/' + d.getDate(); }
