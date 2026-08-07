// localization-board.js — the floor plan the robots page is built around.
//
// One map, one statistic: p50 across EVERY tick, with a no-estimate counted as
// the zero it is. That statistic is unconditioned, which is the only reason it
// can be banded honestly — a mean over the ticks that succeeded is selected by
// the very thing being measured, so a lane returning 0.98 half the time and
// nothing the rest would render green.
//
// Geometry comes from /api/map/edges, the same endpoint the kiosk map reads,
// and state from /api/robots/localization keyed by lane. Reciprocal edges are
// deduped with laneKey() out of scene-geom.js — the extracted rule, which is
// what that extraction was for — and joined to state with serverLaneKey(),
// which mirrors Segment.Lane(). Both derive from the same two endpoint names,
// so the client never parses the server's lane string back apart.

import { makeProjector, cubicPathD, laneKey } from '/static/components/scene-geom.js';

// BAND_STROKE carries the ordering a SECOND time, in weight.
//
// The vendor's triad is the right vocabulary — it is what the plant already
// reads in RoboShop — but measured under deuteranomaly on dark, green and coral
// collapse to ΔE 6.0: the two ends of the scale become one colour for roughly
// one man in twelve. So hue may not carry the ordering alone. Weight steps do,
// and the map stays legible desaturated to pure greyscale.
export const BAND_STROKE = { good: 1.3, fair: 2.5, poor: 3.6, blind: 4.2, nodata: 1.3 };
export const BAND_TOKEN = {
    good: 'var(--viz-green)',
    fair: 'var(--viz-amber)',
    poor: 'var(--viz-coral)',
    blind: 'var(--viz-coral)',
    // Not a band — the absence of one. Substrate grey, so a lane nobody drove
    // reads as "no answer" rather than as a measured one.
    nodata: 'var(--text-muted)'
};
const BAND_LABEL = {
    good: 'good (≥ 0.80)', fair: 'fair (0.30–0.80)', poor: 'poor (> 0)',
    blind: 'blind (exactly 0)', nodata: 'no data'
};

const SVG_NS = 'http://www.w3.org/2000/svg';
function svg(name, attrs) {
    const n = document.createElementNS(SVG_NS, name);
    for (const k in attrs) if (attrs[k] !== null && attrs[k] !== undefined) n.setAttribute(k, attrs[k]);
    return n;
}

// bandOf is the client's read of a lane's band.
//
// The SERVER already banded it and sends `band`; this only falls back when a
// payload predates the field. Re-deriving it here by default would be a second
// implementation of the thresholds that could disagree with the one that
// counted them for the legend.
function bandOf(lane) { return lane.band || 'nodata'; }

// serverLaneKey mirrors Segment.Lane() on the server: the endpoint pair,
// sorted, joined with a hyphen. Same rule as scene-geom's laneKey with a
// different separator, and it exists so the client never parses the server's
// lane string back into endpoints — see laneRows.
export function serverLaneKey(from, to) {
    if (!from || !to) return '';
    return from < to ? from + '-' + to : to + '-' + from;
}

// histPath renders a distribution as an SVG polyline over a unit box.
//
// THE SHAPE IS THE FINDING, and it is the only mark on this page that can
// separate "consistently mediocre at 0.5" from "excellent half the time, blind
// the rest" — two lanes with the same p50 and opposite remedies. Bin 0 is the
// no-estimate sentinel and is drawn detached from the curve, because it is a
// point and not part of the continuum.
export function histPath(hist, w, h) {
    if (!hist || hist.length < 2) return { line: '', sentinel: 0, max: 0 };
    let max = 0;
    for (let i = 1; i < hist.length; i++) if (hist[i] > max) max = hist[i];
    if (max <= 0) return { line: '', sentinel: hist[0] || 0, max: 0 };
    const bins = hist.length - 1;
    let d = '';
    for (let i = 0; i < bins; i++) {
        const x = (i / (bins - 1)) * w;
        const y = h - (hist[i + 1] / max) * h;
        d += (i === 0 ? 'M' : 'L') + x.toFixed(2) + ' ' + y.toFixed(2);
    }
    return { line: d, sentinel: hist[0] || 0, max: max };
}

export function createBoard(root, opts) {
    const o = opts || {};
    const state = {
        edges: [], board: null,
        window: '7d',
        showChanges: true, showReflectors: true,
        selected: null,      // lane key: area + lane, as serverLaneKey builds it
        focusDiff: null,     // diff id — dims lanes that edit did not touch
        change: null,        // the selected lane's annotation, fetched on select
        robots: [],
        // Viewport. scale/tx/ty are the screen transform; strokes divide by
        // scale so they hold their SCREEN size — a lane that thickened as you
        // zoomed would hide the geometry underneath it, and at 5× map zoomed
        // out it would be an unclickable hair.
        scale: 1, tx: 0, ty: 0, fitScale: 1, fitTx: 0, fitTy: 0,
        rotate90: false, proj: makeProjector(false)
    };

    root.innerHTML =
        '<div class="lb-controls">' +
        '  <div class="lb-seg" id="lb-window"></div>' +
        '  <label class="lb-toggle"><input type="checkbox" id="lb-changes" checked> Changes</label>' +
        '  <label class="lb-toggle"><input type="checkbox" id="lb-refl" checked> Reflectors</label>' +
        '  <span class="lb-spacer"></span>' +
        '  <span class="lb-window-note" id="lb-note"></span>' +
        '</div>' +
        '<div class="lb-main">' +
        '  <section class="lb-rail"><div class="lb-hd">Map changes</div><div id="lb-rail-body"></div></section>' +
        '  <section class="lb-mapwrap">' +
        '    <div class="lb-zoom"><span id="lb-lvl">1.0×</span>' +
        '      <button type="button" data-z="out">−</button>' +
        '      <button type="button" data-z="in">+</button>' +
        '      <button type="button" data-z="fit">Fit</button></div>' +
        '    <svg id="lb-map" role="img" aria-label="Plant floor plan, lanes coloured by localization confidence"></svg>' +
        '    <div class="lb-legend" id="lb-legend"></div>' +
        '  </section>' +
        '  <section class="lb-panel" id="lb-panel"></section>' +
        '</div>';

    const map = root.querySelector('#lb-map');
    const panel = root.querySelector('#lb-panel');
    const railBody = root.querySelector('#lb-rail-body');
    const note = root.querySelector('#lb-note');

    // ── window selector ──────────────────────────────────────────────────
    // Only windows the record can answer. Before the daily histograms landed,
    // 30d could not be served at all against fourteen days of raw — a control
    // whose label promises more than the data holds is the failure this whole
    // design removes.
    const winWrap = root.querySelector('#lb-window');
    ['24h', '7d', '30d'].forEach(function (w) {
        const b = document.createElement('button');
        b.type = 'button';
        b.textContent = w;
        b.dataset.win = w;
        b.className = w === state.window ? 'on' : '';
        b.addEventListener('click', function () {
            if (state.window === w) return;
            state.window = w;
            winWrap.querySelectorAll('button').forEach(function (x) {
                x.className = x.dataset.win === w ? 'on' : '';
            });
            load();
        });
        winWrap.appendChild(b);
    });
    root.querySelector('#lb-changes').addEventListener('change', function (e) {
        state.showChanges = e.target.checked; draw();
    });
    root.querySelector('#lb-refl').addEventListener('change', function (e) {
        state.showReflectors = e.target.checked; draw();
    });
    root.querySelector('.lb-zoom').addEventListener('click', function (e) {
        const z = e.target.dataset && e.target.dataset.z;
        if (!z) return;
        if (z === 'fit') fit();
        else zoomAt(map.clientWidth / 2, map.clientHeight / 2, z === 'in' ? 1.25 : 0.8);
    });

    // ── viewport ─────────────────────────────────────────────────────────
    const MAX_SCALE = 40;
    function clampScale(s) { return Math.max(state.fitScale * 0.5, Math.min(s, state.fitScale * MAX_SCALE)); }
    function applyTransform() {
        const g = map.querySelector('#lb-world');
        if (g) g.setAttribute('transform', 'translate(' + state.tx + ',' + state.ty + ') scale(' + state.scale + ')');
        root.querySelector('#lb-lvl').textContent = (state.scale / state.fitScale).toFixed(1) + '×';
        rescaleStrokes();
    }
    // Stroke widths, point marks and labels hold their SCREEN size.
    function rescaleStrokes() {
        const k = 1 / state.scale;
        map.querySelectorAll('[data-w]').forEach(function (n) {
            n.setAttribute('stroke-width', (parseFloat(n.dataset.w) * k).toFixed(4));
            if (n.dataset.dash) n.setAttribute('stroke-dasharray', n.dataset.dash.split(' ')
                .map(function (v) { return (parseFloat(v) * k).toFixed(3); }).join(' '));
        });
        map.querySelectorAll('[data-r]').forEach(function (n) {
            n.setAttribute('r', (parseFloat(n.dataset.r) * k).toFixed(4));
        });
        map.querySelectorAll('[data-fs]').forEach(function (n) {
            n.setAttribute('font-size', (parseFloat(n.dataset.fs) * k).toFixed(3));
        });
    }
    function zoomAt(px, py, factor) {
        const s2 = clampScale(state.scale * factor);
        const k = s2 / state.scale;
        state.tx = px - k * (px - state.tx);
        state.ty = py - k * (py - state.ty);
        state.scale = s2;
        applyTransform();
    }
    // ── pan / zoom, taken from the kiosk map's shape ─────────────────────
    //
    // NO setPointerCapture, AND THAT IS THE WHOLE FIX. Capturing the pointer
    // on the SVG redirects every subsequent pointer event to the SVG itself,
    // so `pointerup`'s target was ALWAYS #lb-map and never the lane under the
    // cursor -- closest('[data-lane]') found nothing and clicking a lane did
    // nothing at all. dashboard-map.js has never had this bug because it never
    // captures: mousedown on the host, mousemove/mouseup on WINDOW.
    //
    // Listening on window rather than the element is also what makes the drag
    // feel right: the pan keeps tracking when the cursor leaves the map, and
    // releasing outside still ends it, instead of the map sticking to the
    // pointer until you come back.
    map.addEventListener('wheel', function (e) {
        e.preventDefault();
        const r = map.getBoundingClientRect();
        // Gentle per-notch, same 1.12 the kiosk uses -- a full wheel click
        // should nudge, not jump.
        zoomAt(e.clientX - r.left, e.clientY - r.top, e.deltaY > 0 ? 1 / 1.12 : 1.12);
    }, { passive: false });

    let drag = null, suppressClick = false;
    map.addEventListener('mousedown', function (e) {
        if (e.button !== 0) return;
        drag = { x: e.clientX, y: e.clientY, tx: state.tx, ty: state.ty, moved: 0 };
        map.classList.add('lb-dragging');
    });
    window.addEventListener('mousemove', function (e) {
        if (!drag) return;
        const dx = e.clientX - drag.x, dy = e.clientY - drag.y;
        if (Math.abs(dx) + Math.abs(dy) > 2) drag.moved = 1;
        state.tx = drag.tx + dx; state.ty = drag.ty + dy;
        applyTransform();
    });
    window.addEventListener('mouseup', function () {
        if (!drag) return;
        // A drag that ends over a lane must not also select it.
        if (drag.moved) suppressClick = true;
        drag = null;
        map.classList.remove('lb-dragging');
    });

    // Selection rides on `click`, not on pointerup, so the browser resolves the
    // target the ordinary way -- the element actually under the cursor.
    map.addEventListener('click', function (e) {
        if (suppressClick) { suppressClick = false; return; }
        let t = e.target;
        while (t && t !== map && !(t.getAttribute && t.getAttribute('data-lane'))) t = t.parentNode;
        const id = (t && t.getAttribute) ? t.getAttribute('data-lane') : null;
        // Clicking the selected lane again clears it, the way the kiosk map
        // toggles a focused robot.
        select(id === state.selected ? null : id);
    });

    map.addEventListener('dblclick', fit);

    function fit() {
        state.scale = state.fitScale; state.tx = state.fitTx; state.ty = state.fitTy;
        applyTransform();
    }

    // ── data ─────────────────────────────────────────────────────────────
    async function load() {
        const [edges, board] = await Promise.all([
            o.fetchEdges ? o.fetchEdges() : fetch('/api/map/edges').then(function (r) { return r.json(); }),
            o.fetchBoard ? o.fetchBoard(state.window)
                : fetch('/api/robots/localization?window=' + state.window).then(function (r) { return r.json(); })
        ]);
        state.edges = edges || [];
        state.board = board || null;
        computeView();
        draw();
        drawRail();
        drawPanel();
    }

    function computeView() {
        const xs = [], ys = [];
        state.edges.forEach(function (e) {
            xs.push(e.from_x, e.to_x); ys.push(e.from_y, e.to_y);
        });
        if (!xs.length) return;
        const minWx = Math.min.apply(null, xs), maxWx = Math.max.apply(null, xs);
        const minWy = Math.min.apply(null, ys), maxWy = Math.max.apply(null, ys);
        // Orientation from the FULL plant, never a zoomed region — an ROI that
        // happens to be tall must not flip the map under the operator.
        state.rotate90 = (maxWy - minWy) > (maxWx - minWx);
        state.proj = makeProjector(state.rotate90);

        const sx = [], sy = [];
        [[minWx, minWy], [minWx, maxWy], [maxWx, minWy], [maxWx, maxWy]].forEach(function (p) {
            const q = state.proj(p[0], p[1]); sx.push(q[0]); sy.push(q[1]);
        });
        const w = Math.max.apply(null, sx) - Math.min.apply(null, sx) || 1;
        const h = Math.max.apply(null, sy) - Math.min.apply(null, sy) || 1;
        const cw = map.clientWidth || 900, ch = map.clientHeight || 560;
        const pad = 24;
        state.fitScale = Math.min((cw - pad * 2) / w, (ch - pad * 2) / h);
        state.scale = state.fitScale;
        state.fitTx = pad - Math.min.apply(null, sx) * state.scale;
        state.fitTy = pad - Math.min.apply(null, sy) * state.scale;
        state.tx = state.fitTx; state.ty = state.fitTy;
        map.setAttribute('viewBox', '0 0 ' + cw + ' ' + ch);
    }

    // laneRows collapses the directed edges onto physical lanes and joins them
    // to state. 405 directed rows at Springfield are 212 pieces of floor, and
    // drawing both entries double-strokes every aisle.
    function laneRows() {
        const byLane = new Map();
        state.edges.forEach(function (e) {
            // laneKey is the extracted dedup rule: 405 directed rows at
            // Springfield are 212 pieces of floor, and drawing both entries
            // double-strokes every aisle.
            const k = laneKey(e.from_name || '', e.to_name || '');
            if (!byLane.has(k)) byLane.set(k, e);
        });
        // The JOIN key is derived from the edge's OWN endpoint names, never by
        // splitting the server's lane string. Endpoint names may contain a
        // hyphen, so parsing "LM10-LM48" back apart is a guess that works on
        // this plant's naming and breaks on the next one. Both keys come from
        // the same two names, so they cannot disagree.
        const stateByLane = new Map();
        if (state.board) state.board.lanes.forEach(function (l) { stateByLane.set(l.lane, l); });
        const out = [];
        byLane.forEach(function (e, k) {
            out.push({
                key: k, edge: e,
                st: stateByLane.get(serverLaneKey(e.from_name || '', e.to_name || '')) || null
            });
        });
        return out;
    }

    function draw() {
        while (map.firstChild) map.removeChild(map.firstChild);
        const world = svg('g', { id: 'lb-world' });
        map.appendChild(world);
        if (!state.edges.length) return;

        const P = state.proj;
        const focus = state.focusDiff && state.board
            ? new Set((state.board.diffs.find(function (d) { return String(d.id) === String(state.focusDiff); }) || {}).lanes || [])
            : null;

        // zones — thin outline, NO fill, class carries the meaning
        const zg = svg('g', { 'class': 'lb-zones' });
        (state.board ? state.board.areas : []).forEach(function (a) {
            // A zone with readings but no polygon is real and undrawable. It
            // belongs in the panel, which is why the payload keeps it and why
            // skipping it here is not the same as dropping it.
            if (!a.polygon || a.polygon.length < 3) return;
            const pts = a.polygon.map(function (p) { const q = P(p.x, p.y); return q[0] + ',' + q[1]; }).join(' ');
            const solid = a.class === 'ReflectorArea';
            zg.appendChild(svg('polygon', {
                points: pts, fill: 'none', stroke: 'var(--text-muted)',
                'stroke-width': 1, 'data-w': solid ? 1.1 : 0.8,
                'data-dash': solid ? null : '4 3', opacity: solid ? 0.7 : 0.45
            }));
            // Wayfinding, not signal. EVERY zone is labelled, in substrate
            // grey — never keyed to the reflector count, which predicts
            // nothing and would make the map emphasise it.
            let cx = 0, cy = 0;
            a.polygon.forEach(function (p) { const q = P(p.x, p.y); cx += q[0]; cy += q[1]; });
            const t = svg('text', {
                x: cx / a.polygon.length, y: cy / a.polygon.length,
                fill: 'var(--text-muted)', 'font-size': 9, 'data-fs': 9,
                'text-anchor': 'middle', opacity: 0.75
            });
            t.textContent = a.name;
            zg.appendChild(t);
        });
        world.appendChild(zg);

        // lanes
        const lg = svg('g', { 'class': 'lb-lanes' });
        laneRows().forEach(function (row) {
            const e = row.edge;
            const a = P(e.from_x, e.from_y), b = P(e.to_x, e.to_y);
            const curved = e.ctrl1_x !== null && e.ctrl1_x !== undefined &&
                e.ctrl2_x !== null && e.ctrl2_x !== undefined;
            const d = curved
                ? cubicPathD(a, P(e.ctrl1_x, e.ctrl1_y), P(e.ctrl2_x, e.ctrl2_y), b)
                : 'M' + a[0] + ' ' + a[1] + 'L' + b[0] + ' ' + b[1];
            const band = row.st ? bandOf(row.st) : 'nodata';
            const dim = focus && !focus.has(row.st ? row.st.lane : '');

            if (state.showChanges && row.st && row.st.changed && !dim) {
                // Sky halo — DATA. A different category from selection, so a
                // different hue; two marks sharing a hue was unreadable.
                lg.appendChild(svg('path', {
                    d: d, fill: 'none', stroke: 'var(--viz-sky)', opacity: 0.28,
                    'stroke-width': 10, 'data-w': 10, 'stroke-linecap': 'round'
                }));
            }
            const p = svg('path', {
                d: d, fill: 'none',
                stroke: BAND_TOKEN[band], opacity: dim ? 0.12 : 1,
                'stroke-width': BAND_STROKE[band], 'data-w': BAND_STROKE[band],
                'data-dash': band === 'blind' ? '5 4' : null,
                'stroke-linecap': 'round', 'data-lane': row.key,
                'class': 'lb-lane'
            });
            lg.appendChild(p);

            if (state.selected === row.key) {
                // Near-white rim — INTERFACE CHROME, not data.
                lg.appendChild(svg('path', {
                    d: d, fill: 'none', stroke: 'var(--viz-primary)', opacity: 0.9,
                    'stroke-width': BAND_STROKE[band] + 2.4, 'data-w': BAND_STROKE[band] + 2.4,
                    'stroke-linecap': 'round', 'pointer-events': 'none'
                }));
                lg.appendChild(svg('path', {
                    d: d, fill: 'none', stroke: BAND_TOKEN[band],
                    'stroke-width': BAND_STROKE[band], 'data-w': BAND_STROKE[band],
                    'stroke-linecap': 'round', 'pointer-events': 'none'
                }));
            }
        });
        world.appendChild(lg);

        // reflectors — reference, present-but-not-predictive
        if (state.showReflectors && state.board) {
            const rg = svg('g', { 'class': 'lb-refl' });
            state.board.reflectors.forEach(function (r) {
                const q = P(r.x, r.y);
                rg.appendChild(svg('circle', {
                    cx: q[0], cy: q[1], r: 1.6, 'data-r': 1.6,
                    fill: 'var(--viz-teal)', opacity: 0.85
                }));
            });
            world.appendChild(rg);
        }

        // robots — this is the robots page; live positions belong on it
        if (state.robots.length) {
            const rg = svg('g', { 'class': 'lb-robots' });
            state.robots.forEach(function (rb) {
                if (typeof rb.x !== 'number') return;
                const q = P(rb.x, rb.y);
                rg.appendChild(svg('circle', {
                    cx: q[0], cy: q[1], r: 3, 'data-r': 3,
                    fill: 'var(--viz-primary)', stroke: 'var(--elev-surface)',
                    'stroke-width': 1, 'data-w': 1
                }));
            });
            world.appendChild(rg);
        }

        drawLegend();
        applyTransform();
    }

    function drawLegend() {
        const lg = root.querySelector('#lb-legend');
        const counts = (state.board && state.board.plant && state.board.plant.bands) || {};
        // Swatches MIRROR the stroke weights, because a legend chip is the one
        // place colour really is the only channel.
        lg.innerHTML = ['good', 'fair', 'poor', 'blind', 'nodata'].map(function (b) {
            const n = counts[b] || 0;
            return '<span class="lb-key"><i style="background:' + BAND_TOKEN[b] +
                ';height:' + Math.max(2, BAND_STROKE[b]) + 'px"></i>' +
                BAND_LABEL[b] + ' <b>' + n + '</b></span>';
        }).join('') +
            (state.showChanges ? '<span class="lb-key"><i class="lb-halo"></i>changed in window</span>' : '') +
            (state.showReflectors ? '<span class="lb-key"><i style="background:var(--viz-teal);height:6px;width:6px;border-radius:50%"></i>reflector</span>' : '');
    }

    function drawRail() {
        if (!state.board) return;
        const diffs = state.board.diffs || [];
        if (!diffs.length) {
            railBody.innerHTML = '<p class="lb-empty">No map edits recorded yet. ' +
                'The scene sync writes a row only when something actually moved.</p>';
            return;
        }
        railBody.innerHTML = diffs.map(function (d) {
            const on = String(state.focusDiff) === String(d.id);
            // A diff says only what a diff CAN know: no author, no reason.
            const moved = (d.max_delta_m === null || d.max_delta_m === undefined)
                ? 'magnitude n/a'
                : 'max ' + Number(d.max_delta_m).toFixed(2) + ' m';
            return '<button type="button" class="lb-diff' + (on ? ' on' : '') +
                '" data-diff="' + d.id + '">' +
                '<span class="lb-diff-when">' + new Date(d.observed_at).toLocaleString() + '</span>' +
                '<span class="lb-diff-what">' + (d.objects_changed || 0) + ' changed · ' +
                (d.objects_added || 0) + ' added · ' + (d.objects_removed || 0) + ' removed</span>' +
                '<span class="lb-diff-mag">' + moved + '</span>' +
                '<span class="lb-diff-lanes">' + ((d.lanes || []).slice(0, 4).join(', ') || '—') +
                ((d.lanes || []).length > 4 ? ' +' + ((d.lanes || []).length - 4) : '') + '</span>' +
                '</button>';
        }).join('');
        railBody.querySelectorAll('[data-diff]').forEach(function (b) {
            b.addEventListener('click', function () {
                // Rail click is a FILTER; the Changes toggle is a LAYER. Two
                // independent ways to reduce what is drawn, not one.
                state.focusDiff = String(state.focusDiff) === b.dataset.diff ? null : b.dataset.diff;
                drawRail(); draw();
            });
        });
    }

    function select(key) {
        state.selected = key;
        state.change = null;
        draw();
        drawPanel();
        if (!key) return;
        // Fetched per selection rather than carried on the board payload: most
        // lanes have never been edited, so folding it in would compute a
        // before/after for every lane to answer a question about one.
        const row = laneRows().find(function (r) { return r.key === key; });
        if (!row || !row.st) return;
        const q = '?area=' + encodeURIComponent(row.st.area) + '&lane=' + encodeURIComponent(row.st.lane);
        const get = o.fetchChange ? o.fetchChange(row.st.area, row.st.lane)
            : fetch('/api/robots/lane-change' + q).then(function (r) { return r.json(); });
        get.then(function (d) {
            if (state.selected !== key) return; // selection moved on
            state.change = d && d.changed ? d.annotation : { none: true };
            drawPanel();
        }).catch(function () { /* the panel simply omits it */ });
    }

    // annotationBlock renders the four guards, or says plainly why it cannot.
    function annotationBlock() {
        const c = state.change;
        if (!c) return '';
        if (c.none) {
            return '<div class="lb-hist-title">Change</div>' +
                '<p class="lb-note">This lane has never been edited. Nothing to compare — ' +
                'and that is a different statement from "no effect".</p>';
        }
        const pct = function (v) { return (v * 100).toFixed(0) + '%'; };
        const moved = (c.moved_m === null || c.moved_m === undefined)
            ? 'redrawn (no distance — it gained or lost a vertex)'
            : 'moved ' + Number(c.moved_m).toFixed(2) + ' m';
        let rows = '';
        // GUARD 1: the miss rate leads, and the p50 is suppressed when it moved.
        rows += '<tr><td>no estimate</td><td>' + pct(c.no_estimate_before) + ' → ' +
            pct(c.no_estimate_after) + '</td><td>' +
            (c.suppress_p50 ? '<span class="lb-warn-inline">moved</span>' : 'unchanged') + '</td></tr>';
        if (c.suppress_p50) {
            rows += '<tr><td colspan="3" class="lb-suppressed">p50 delta withheld — ' +
                (c.suppressed || 'the miss rate moved') +
                ', so the two averages are over different populations and are not comparable.</td></tr>';
        } else {
            const d = (c.p50_before !== null && c.p50_after !== null)
                ? (c.p50_after - c.p50_before) : null;
            // GUARD 5: ink follows what is ATTRIBUTABLE. A lane that rose with
            // the plant shows neutral, not the success colour.
            let cls = 'lb-neutral';
            if (d !== null && c.plant_delta !== null && c.plant_delta !== undefined) {
                const attributable = d - c.plant_delta;
                if (attributable > 0.02) cls = 'lb-better';
                else if (attributable < -0.02) cls = 'lb-worse';
            }
            rows += '<tr><td>p50</td><td>' + fmtP(c.p50_before) + ' → ' + fmtP(c.p50_after) +
                '</td><td class="' + cls + '">' +
                (d === null ? '—' : (d >= 0 ? '+' : '') + d.toFixed(2)) +
                // GUARD 2: the plant baseline, always, never a toggle.
                (c.plant_delta === null || c.plant_delta === undefined ? ''
                    : ' <span class="lb-plant">plant ' + (c.plant_delta >= 0 ? '+' : '') +
                      Number(c.plant_delta).toFixed(2) + '</span>') +
                '</td></tr>';
        }
        // GUARD 3: both counts AND both window lengths, on the face of it.
        rows += '<tr><td>n</td><td colspan="2">' + c.n_before + ' before (' + c.days_before +
            ' d) · ' + c.n_after + ' after (' + c.days_after + ' d)</td></tr>';

        return '<div class="lb-hist-title">Change</div>' +
            '<div class="lb-change-hd">changed ' + new Date(c.changed_at).toLocaleString() +
            ' · ' + moved + '</div>' +
            // GUARD 4: grey below the minimum, never absent. Absence reads as fine.
            '<table class="lb-change' + (c.below_min_n ? ' lb-thin' : '') + '"><tbody>' +
            rows + '</tbody></table>' +
            (c.below_min_n ? '<p class="lb-warn">Below the minimum n on one side of the ' +
                'edit. Shown greyed rather than hidden — an absent number reads as fine.</p>' : '') +
            '<p class="lb-note">A diff can say what changed and when, never why. ' +
            'No author, no reason — nobody types anything.</p>';
    }

    function drawPanel() {
        if (!state.board) { panel.innerHTML = ''; return; }
        const w = state.board.window;
        note.textContent = w.data_days < w.requested_days
            ? w.data_days + ' of ' + w.requested_days + ' days hold data'
            : w.requested_days + ' days';

        if (!state.selected) {
            const p = state.board.plant;
            panel.innerHTML = '<div class="lb-hd">Plant</div>' +
                '<p class="lb-note">Select a lane for its distribution and history.</p>' +
                statBlock('readings', p.samples) +
                statBlock('p50 (all ticks)', fmtP(p.p50_estimate)) +
                histBlock(p.hist, 'Plant distribution') +
                '<p class="lb-note">Shape, not score. A spike at zero beside a spike near 1.0 is a ' +
                'plant with dead zones; a broad hump in the middle is a plant that is uniformly ' +
                'marginal. They share a p50 and need opposite work.</p>' +
                zoneBlock();
            return;
        }
        const row = laneRows().find(function (r) { return r.key === state.selected; });
        const st = row && row.st;
        if (!st) {
            panel.innerHTML = '<div class="lb-hd">' + (state.selected || '') + '</div>' +
                '<p class="lb-note">No readings in this window. Not the same as zero — ' +
                'nothing drove here, or nothing was recorded.</p>';
            return;
        }
        const noEst = st.samples ? (st.sentinel_samples / st.samples) : 0;
        panel.innerHTML = '<div class="lb-hd">' + st.lane + '</div>' +
            '<div class="lb-band lb-band-' + bandOf(st) + '">' + BAND_LABEL[bandOf(st)] + '</div>' +
            statBlock('p50 estimate (all ticks)', fmtP(st.p50_estimate)) +
            statBlock('no estimate', (noEst * 100).toFixed(1) + '%') +
            statBlock('readings', st.samples + ' (' + st.samples_good + ' produced a number)') +
            statBlock('robots', st.robots) +
            statBlock('days of data', st.days + ' of ' + w.requested_days) +
            (st.versions > 1
                ? '<p class="lb-warn">This window spans ' + st.versions + ' geometry versions — ' +
                  'the lane was edited inside it, so one number over the whole window is ' +
                  'averaging across the change.</p>' : '') +
            (st.below_min_n
                ? '<p class="lb-warn">Below the minimum of ' + state.board.min_samples +
                  ' readings. Greyed rather than banded: too few to say.</p>' : '') +
            (st.hist_incomplete
                ? '<p class="lb-warn">Part of this window has no stored distribution, so the ' +
                  'percentile covers less than the label claims.</p>' : '') +
            histBlock(st.hist, 'Distribution') +
            annotationBlock();
    }

    // ZONES ARE LISTED FROM THEIR NUMBERS, NOT FROM THEIR SHAPE.
    //
    // A zone's statistics come from the roll-up and its outline from the .smap,
    // on different transports with different gates. Rendering the list only for
    // zones we can draw would leave the numbers written and unread on every
    // plant between the collection starting and the first map fetch — which is
    // the exact defect this project keeps finding, so the list is keyed on the
    // zone id and says plainly which ones cannot be drawn yet.
    function zoneBlock() {
        var areas = (state.board && state.board.areas) || [];
        var withStats = areas.filter(function (a) { return a.has_stats; });
        if (!withStats.length) {
            return '<div class="lb-hist-title">Zones</div>' +
                '<p class="lb-note">No zone readings in this window.</p>';
        }
        // Worst first: the no-estimate rate is what the annotation leads with,
        // because a zone the robot cannot localize in returns a clean reading
        // or none at all.
        withStats.sort(function (a, b) { return noestOf(b) - noestOf(a); });
        var rows = withStats.map(function (a) {
            var noest = (noestOf(a) * 100).toFixed(0) + '%';
            var undrawn = (!a.polygon || a.polygon.length < 3)
                ? ' <span class="lb-undrawn" title="This zone has readings but no geometry yet — the map fetch has not run, so it cannot be outlined.">no shape</span>'
                : '';
            var cls = a.class ? a.class.replace('Area', '') : '—';
            return '<tr><td>' + a.name + undrawn + '</td><td>' + cls +
                '</td><td>' + fmtP(a.p50_estimate) + '</td><td>' + noest +
                '</td><td>' + a.samples + '</td></tr>';
        }).join('');
        return '<div class="lb-hist-title">Zones</div>' +
            '<table class="lb-zones-tbl"><thead><tr><th>zone</th><th>class</th>' +
            '<th>p50</th><th>no est.</th><th>n</th></tr></thead><tbody>' +
            rows + '</tbody></table>' +
            '<p class="lb-note">Class is what predicts a miss — every ReflectorArea ' +
            'carrying traffic loses a fifth to three quarters of its readings, and ' +
            'neither LocConfigArea loses any. The reflector count inside a zone ' +
            'predicts nothing and is deliberately not shown.</p>';
    }

    function noestOf(a) { return a.samples ? (a.sentinel_samples / a.samples) : 0; }

    function fmtP(v) { return (v === null || v === undefined) ? '—' : Number(v).toFixed(3); }
    function statBlock(k, v) {
        return '<div class="lb-stat"><span>' + k + '</span><b>' + v + '</b></div>';
    }
    function histBlock(hist, title) {
        const h = histPath(hist, 220, 56);
        if (!h.line) return '<p class="lb-note">No distribution for this window.</p>';
        const total = (hist || []).reduce(function (a, b) { return a + b; }, 0);
        const share = total ? (h.sentinel / total) : 0;
        return '<div class="lb-hist-title">' + title + '</div>' +
            '<svg class="lb-hist" viewBox="0 0 240 70" role="img" aria-label="' + title + '">' +
            '<rect x="0" y="' + (56 - Math.min(56, share * 56 * 3)).toFixed(1) +
            '" width="8" height="' + Math.min(56, share * 56 * 3).toFixed(1) +
            '" fill="var(--viz-coral)" opacity="0.85"><title>no estimate: ' +
            h.sentinel + '</title></rect>' +
            '<path d="' + h.line + '" transform="translate(14,0)" fill="none" ' +
            'stroke="var(--viz-primary)" stroke-width="1.4"/>' +
            '<text x="0" y="68" font-size="7" fill="var(--text-muted)">0 (no est.)</text>' +
            '<text x="220" y="68" font-size="7" fill="var(--text-muted)" text-anchor="end">1.0</text>' +
            '</svg>';
    }

    return {
        load: load,
        setRobots: function (list) { state.robots = list || []; draw(); },
        _state: state,
        _laneRows: laneRows
    };
}

