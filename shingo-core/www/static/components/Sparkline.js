// Sparkline — tiny inline-SVG trend line for KPI tile bottom edges and
// per-row mini-trends (plan §3.A, §3.E.1). Pure render function; returns an
// <svg> element. No Chart.js dependency — SVG is cheaper for a 12-24 point
// strip and avoids a canvas-per-tile.
//
//   tile.appendChild(Sparkline(hourlyBuckets, { color: 'var(--success)' }));
//
// values: array of numbers (non-finite entries are dropped). opts: width,
// height, color. Renders nothing (empty svg) for <2 usable points so a cold
// window looks intentional rather than broken.

const SVG_NS = 'http://www.w3.org/2000/svg';

export function Sparkline(values, opts) {
    opts = opts || {};
    const w = opts.width || 120;
    const hgt = opts.height || 18;
    const color = opts.color || 'var(--primary)';
    const svg = document.createElementNS(SVG_NS, 'svg');
    svg.setAttribute('class', 'sparkline');
    svg.setAttribute('width', String(w));
    svg.setAttribute('height', String(hgt));
    svg.setAttribute('viewBox', '0 0 ' + w + ' ' + hgt);
    svg.setAttribute('preserveAspectRatio', 'none');
    svg.setAttribute('aria-hidden', 'true');

    const pts = sparkPoints(values, w, hgt);
    if (pts) {
        const poly = document.createElementNS(SVG_NS, 'polyline');
        poly.setAttribute('points', pts);
        poly.setAttribute('fill', 'none');
        poly.setAttribute('stroke', color);
        poly.setAttribute('stroke-width', '1.5');
        poly.setAttribute('stroke-linejoin', 'round');
        poly.setAttribute('stroke-linecap', 'round');
        // Keep stroke crisp despite the non-uniform viewBox scaling.
        poly.setAttribute('vector-effect', 'non-scaling-stroke');
        svg.appendChild(poly);
    }
    return svg;
}

function sparkPoints(values, w, hgt) {
    const v = (values || []).filter((n) => typeof n === 'number' && isFinite(n));
    if (v.length < 2) return null;
    let min = v[0], max = v[0];
    for (let i = 1; i < v.length; i++) { if (v[i] < min) min = v[i]; if (v[i] > max) max = v[i]; }
    const span = (max - min) || 1;
    const pad = 1.5;
    const n = v.length;
    const out = new Array(n);
    for (let i = 0; i < n; i++) {
        const x = (i / (n - 1)) * w;
        const y = hgt - pad - ((v[i] - min) / span) * (hgt - 2 * pad);
        out[i] = x.toFixed(1) + ',' + y.toFixed(1);
    }
    return out.join(' ');
}
