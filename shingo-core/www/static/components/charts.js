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
        // Gridlines come from the substrate ramp's hairline step (U8), not from
        // a token of their own. --chart-grid used to be
        // color-mix(--text-muted 25%) — structure derived from the TEXT ramp,
        // because there was no steel token to derive it from. There is now.
        grid: cssVar('--sub-1', '#2b3543'),
        text: cssVar('--text-muted', '#6c757d'),
        surface: cssVar('--surface', '#ffffff'),
        primary: cssVar('--primary', '#7c7cf0'),
        accent: cssVar('--accent', '#7c7cf0'),
        // Curated data-viz palette (P19 — supersedes P18 mono). One vibrant set:
        // categorical indigo→violet→sky→teal→amber→coral + green=good/coral=bad/
        // amber=warn. vizPrimary (white/near-black) is hero numbers + chart text.
        vizPrimary: cssVar('--viz-primary', '#eaf0f6'),
        vizSecondary: cssVar('--viz-secondary', '#8b949e'),
        vizAccent: cssVar('--viz-accent', '#7c7cf0'),
        vizIndigo: cssVar('--viz-indigo', '#7c7cf0'),
        vizViolet: cssVar('--viz-violet', '#b07cf5'),
        vizSky: cssVar('--viz-sky', '#38bdf8'),
        vizTeal: cssVar('--viz-teal', '#2dd4bf'),
        vizAmber: cssVar('--viz-amber', '#facc5b'),
        vizCoral: cssVar('--viz-coral', '#fb7185'),
        vizGreen: cssVar('--viz-green', '#34d399'),
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

    // Thin straight lines, no dots — except a point with no neighbour, which
    // would otherwise draw nothing at all (a one-bucket window opened blank).
    // tension stays 0: a curve between two hourly points asserts values that
    // were never measured (guide §Data visualization, "Lines are straight").
    o.elements = o.elements || {};
    o.elements.line = Object.assign({ borderWidth: 1.6, tension: 0 }, o.elements.line || {});
    o.elements.point = Object.assign({ radius: isolatedPointRadius, hitRadius: 6, hoverRadius: 3 }, o.elements.point || {});
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

// isolatedPointRadius draws a marker only where a value has no drawn
// neighbour on either side — a single-bucket series, or one value between gaps.
// Everywhere else the line carries the data and dots would be clutter.
function isolatedPointRadius(ctx) {
    const data = ctx.dataset && ctx.dataset.data;
    if (!data || ctx.type !== 'data') return 0;
    const i = ctx.dataIndex;
    const here = data[i];
    if (here === null || here === undefined) return 0;
    const prev = i > 0 ? data[i - 1] : null;
    const next = i < data.length - 1 ? data[i + 1] : null;
    const lone = (prev === null || prev === undefined) && (next === null || next === undefined);
    return lone ? 3 : 0;
}

// progressBarColors gives each bar its colour, with the in-progress bucket
// (inProgress from plantclock.js) washed out so a partly-elapsed hour does
// not read as a drop. flags[i] is true for the in-progress bucket.
export function progressBarColors(color, flags) {
    return flags.map((f) => (f ? withAlpha(color, 0.35) : color));
}

// progressSegment dashes the line segment that runs into the in-progress
// bucket, for the same reason. Combine with any other segment rule by
// passing it as `other`.
export function progressSegment(flags, other) {
    return {
        borderDash: (ctx) => {
            if (flags[ctx.p1DataIndex]) return [4, 4];
            return other ? other(ctx) : undefined;
        },
    };
}

// withAlpha turns a resolved colour into a translucent fill (P19 soft fills).
// Handles hex; falls back to color-mix for var()/named colours.
export function withAlpha(color, a) {
    if (color && color[0] === '#') {
        let hex = color.slice(1);
        if (hex.length === 3) hex = hex.split('').map((x) => x + x).join('');
        const n = parseInt(hex, 16);
        return 'rgba(' + ((n >> 16) & 255) + ',' + ((n >> 8) & 255) + ',' + (n & 255) + ',' + a + ')';
    }
    return 'color-mix(in srgb, ' + color + ' ' + Math.round(a * 100) + '%, transparent)';
}
