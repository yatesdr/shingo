// Overview Section D — Plant footprint (plan §15.D). The "how much has shingo
// taken over" exec narrative: cells/bins under management with growth
// sparklines, plus a combined daily loaded/unloaded velocity chart. Data:
// /api/footprint (plant-wide; ignores the station/robot filters).

import { apiGet, h } from '/static/app.js';
import { Sparkline } from '/static/components/Sparkline.js';
import { makeChart, installChartThemeHook, chartColors } from '/static/components/charts.js';

export function createFootprintSection(store) {
    let chart = null;

    function mount() {
        installChartThemeHook();
        const body = document.getElementById('ops-footprint-body');
        if (!body) return;
        body.innerHTML = h`
            <div class="footprint-nums">
              <div class="footprint-num card" style="margin:0">
                <div class="v" id="fp-cells">—</div><div class="k">Cells managed</div>
                <div id="fp-cells-spark" class="kpi-spark"></div>
              </div>
              <div class="footprint-num card" style="margin:0">
                <div class="v" id="fp-bins">—</div><div class="k">Bins managed</div>
                <div id="fp-bins-spark" class="kpi-spark"></div>
              </div>
            </div>
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
        setText('fp-cells', fp.cells_managed);
        setText('fp-bins', fp.bins_managed);
        spark('fp-cells-spark', fp.cells_spark, 'var(--success)');
        spark('fp-bins-spark', fp.bins_spark, 'var(--info)');
        renderChart(fp.load_series || []);
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
function fmtDay(iso) { const d = new Date(iso); return isNaN(d.getTime()) ? '' : (d.getMonth() + 1) + '/' + d.getDate(); }
