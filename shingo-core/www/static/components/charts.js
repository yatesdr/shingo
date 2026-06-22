// Shared Chart.js helpers (plan §4, §6, §8 #18). Centralizes theme-aware
// defaults so every dashboard chart reads the CSS-variable palette, and
// installs the live re-theme hook: Chart.js samples CSS variables at
// creation time, so without this a live chart keeps its old axis/grid colors
// after a light/dark toggle. We watch <html data-theme> and patch+redraw
// every tracked instance.
//
// Chart.js + chartjs-plugin-zoom are loaded as UMD <script>s before the page
// module (window.Chart). makeChart() tracks instances and wraps destroy() so
// reconcileList/DrillModal teardown also untracks them (no leak, §13).

const _charts = new Set();

function cssVar(name, fallback) {
    const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    return v || fallback;
}

export function chartColors() {
    return {
        grid: cssVar('--chart-grid', 'rgba(120,120,120,0.25)'),
        text: cssVar('--text-muted', '#6c757d'),
        surface: cssVar('--surface', '#ffffff'),
        primary: cssVar('--primary', '#0d6efd'),
        success: cssVar('--success', '#198754'),
        info: cssVar('--info', '#0dcaf0'),
        warning: cssVar('--warning', '#ffc107'),
        danger: cssVar('--danger', '#dc3545'),
    };
}

// applyTheme sets the shared "calm chart" look so charts don't read as stock
// Chart.js: thin lines, no point markers, x-gridlines off and only a few faint
// y-guides, no axis borders, muted ticks, and a quiet point-style legend. Every
// default is merged UNDER the caller's options (Object.assign({defaults}, caller)),
// so any chart can still override. (P8 restyle — the type-hierarchy/hero pass is
// separate, P8b.)
function applyTheme(config, c) {
    config.options = config.options || {};
    const o = config.options;
    if (o.responsive === undefined) o.responsive = true;
    if (o.maintainAspectRatio === undefined) o.maintainAspectRatio = false;

    // Thin lines, gentle smoothing, no dots — datasets can still override.
    o.elements = o.elements || {};
    o.elements.line = Object.assign({ borderWidth: 1.6, tension: 0.3 }, o.elements.line || {});
    o.elements.point = Object.assign({ radius: 0, hitRadius: 6, hoverRadius: 3 }, o.elements.point || {});
    o.elements.bar = Object.assign({ borderRadius: 3 }, o.elements.bar || {});

    o.plugins = o.plugins || {};
    if (o.plugins.legend === undefined) o.plugins.legend = { display: false };
    // When a chart does show a legend, make it quiet: small circular swatches,
    // muted text, top-right — not boxed Chart.js defaults.
    if (o.plugins.legend && o.plugins.legend.display) {
        if (o.plugins.legend.position === undefined) o.plugins.legend.position = 'top';
        if (o.plugins.legend.align === undefined) o.plugins.legend.align = 'end';
        o.plugins.legend.labels = Object.assign(
            { color: c.text, boxWidth: 8, boxHeight: 8, usePointStyle: true, pointStyle: 'circle', padding: 12, font: { size: 11 } },
            o.plugins.legend.labels || {}
        );
    }
    o.plugins.tooltip = Object.assign({
        backgroundColor: c.surface, titleColor: c.text, bodyColor: c.text,
        borderColor: c.grid, borderWidth: 1, padding: 10, cornerRadius: 6, usePointStyle: true,
    }, o.plugins.tooltip || {});

    o.scales = o.scales || {};
    // X axis: no gridlines, no axis border, muted + decluttered ticks.
    o.scales.x = o.scales.x || {};
    o.scales.x.grid = Object.assign({ display: false, drawBorder: false }, o.scales.x.grid || {});
    o.scales.x.border = Object.assign({ display: false }, o.scales.x.border || {});
    o.scales.x.ticks = Object.assign({ color: c.text, maxRotation: 0, autoSkip: true, maxTicksLimit: 8, font: { size: 11 } }, o.scales.x.ticks || {});
    // Y axis: a few faint horizontal guides only, no axis border, no tick marks.
    o.scales.y = o.scales.y || {};
    o.scales.y.grid = Object.assign({ color: c.grid, drawBorder: false, drawTicks: false }, o.scales.y.grid || {});
    o.scales.y.border = Object.assign({ display: false }, o.scales.y.border || {});
    o.scales.y.ticks = Object.assign({ color: c.text, maxTicksLimit: 5, padding: 8, font: { size: 11 } }, o.scales.y.ticks || {});

    return config;
}

// makeChart creates a themed Chart.js instance and tracks it for re-theming.
// config is a standard Chart.js config ({type, data, options}).
export function makeChart(canvas, config) {
    if (!window.Chart) { console.error('charts: Chart.js not loaded'); return null; }
    applyTheme(config, chartColors());
    // 250ms ease-out on initial draw; sections call update('none') after to
    // avoid jitter on data refresh (§4).
    if (config.options.animation === undefined) {
        config.options.animation = { duration: 250, easing: 'easeOutQuart' };
    }
    const chart = new window.Chart(canvas, config);
    _charts.add(chart);
    const origDestroy = chart.destroy.bind(chart);
    chart.destroy = () => { _charts.delete(chart); origDestroy(); };
    return chart;
}

// registerZoom registers chartjs-plugin-zoom (idempotent). Call before
// creating a chart that sets options.plugins.zoom (drill modal only).
let _zoomRegistered = false;
export function registerZoom() {
    if (_zoomRegistered || !window.Chart) return;
    const zoom = window.ChartZoom || window['chartjs-plugin-zoom'];
    if (zoom) { window.Chart.register(zoom); _zoomRegistered = true; }
}

// reThemeAll patches axis/grid/tooltip colors on every live chart and
// redraws without animation (§8 #18).
export function reThemeAll() {
    const c = chartColors();
    _charts.forEach((chart) => {
        applyTheme(chart.config, c);
        chart.update('none');
    });
}

let _themeHookInstalled = false;
export function installChartThemeHook() {
    if (_themeHookInstalled || typeof MutationObserver === 'undefined') return;
    _themeHookInstalled = true;
    const mo = new MutationObserver((muts) => {
        for (const m of muts) {
            if (m.attributeName === 'data-theme') { reThemeAll(); break; }
        }
    });
    mo.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
}

// Format a bucket_start ISO string for a category x-axis label.
export function bucketLabel(iso, bucket) {
    const d = new Date(iso);
    if (isNaN(d.getTime())) return '';
    if (bucket === 'day') {
        return (d.getMonth() + 1) + '/' + d.getDate();
    }
    return String(d.getHours()).padStart(2, '0') + ':00';
}
