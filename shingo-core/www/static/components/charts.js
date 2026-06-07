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

function applyTheme(config, c) {
    config.options = config.options || {};
    const o = config.options;
    if (o.responsive === undefined) o.responsive = true;
    if (o.maintainAspectRatio === undefined) o.maintainAspectRatio = false;
    o.plugins = o.plugins || {};
    if (o.plugins.legend === undefined) o.plugins.legend = { display: false };
    o.plugins.tooltip = Object.assign({
        backgroundColor: c.surface, titleColor: c.text, bodyColor: c.text,
        borderColor: c.grid, borderWidth: 1,
    }, o.plugins.tooltip || {});
    o.scales = o.scales || {};
    for (const axis of ['x', 'y']) {
        o.scales[axis] = o.scales[axis] || {};
        o.scales[axis].grid = Object.assign({ color: c.grid }, o.scales[axis].grid || {});
        o.scales[axis].ticks = Object.assign({ color: c.text }, o.scales[axis].ticks || {});
    }
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
