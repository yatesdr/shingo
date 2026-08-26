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
// The band ramp is green→amber→orange→coral (good to poor). Measured under
// deuteranomaly on dark, green and coral collapse to ΔE 6.0: the two ends of
// the scale become one colour for roughly one man in twelve. So hue may not
// carry the ordering alone. Weight steps do, and the map stays legible
// desaturated to pure greyscale.
export const BAND_STROKE = { good: 1.3, fair: 2.5, watch: 3.0, poor: 3.6, blind: 4.2, nodata: 1.3 };

// LANE_HIT_STROKE is the invisible click target laid under every lane, in the
// same screen-pixel units BAND_STROKE uses.
//
// A stroked path is only clickable across its stroke, so the visible weight WAS
// the hit target — and a `good` lane is 1.3px of it. Selecting one meant landing
// a click inside a hairline, on a map you can also pan by dragging, which in
// practice meant missing and re-aiming. The weights above cannot simply grow:
// they carry the band ordering a second time for the deuteranomaly case, so
// making them thick enough to hit would flatten the very signal they exist to
// encode.
//
// So the target is separated from the mark. 14 rather than the ~24 a touch
// guideline would ask for: lanes in a dense aisle sit close together, and a
// target wide enough to be comfortable in isolation starts swallowing its
// neighbour. This is the widest value that still resolves adjacent lanes at fit
// zoom, and zooming in widens the gap without widening the target — which is
// what the constant screen-size rescale already buys.
export const LANE_HIT_STROKE = 14;
export const BAND_TOKEN = {
    good: 'var(--viz-green)',
    fair: 'var(--viz-amber)',
    watch: 'var(--viz-orange)',
    poor: 'var(--viz-coral)',
    blind: 'var(--viz-coral)',
    // Not a band — the absence of one. Substrate grey, so a lane nobody drove
    // reads as "no answer" rather than as a measured one.
    nodata: 'var(--text-muted)'
};
const BAND_LABEL = {
    good: 'good (≥ 0.80)', fair: 'fair (0.50–0.80)', watch: 'watch (0.30–0.50)',
    poor: 'poor (< 0.30)', blind: 'blind (exactly 0)', nodata: 'no data'
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

// ── Window control seeds and guards ─────────────────────────────────────────
//
// Pure date math, at module scope so the node harness can pin it: these are
// the values that would be WRONG SILENTLY — an off-by-one that includes a
// day the roll-up has not closed yet, or a span cap that stopped mirroring
// the server's.

function iso(d) { return d.toISOString().slice(0, 10); }

// seedMainRange is the board's opening range: the last seven COMPLETE days,
// ending yesterday. The roll-up writes a day's rows the night after, so a
// range ending today silently overstates itself by one empty day.
export function seedMainRange(today) {
    const to = new Date(today); to.setUTCDate(to.getUTCDate() - 1);
    const from = new Date(to); from.setUTCDate(from.getUTCDate() - 6);
    return { from: iso(from), to: iso(to) };
}

// seedCompareDays is what compare mode opens with: B yesterday, A seven days
// before it — the same weekday, the day-grain version of the old "equal
// stretch immediately before it" seed.
export function seedCompareDays(today) {
    const b = new Date(today); b.setUTCDate(b.getUTCDate() - 1);
    const a = new Date(b); a.setUTCDate(a.getUTCDate() - 7);
    return { a: iso(a), b: iso(b) };
}

// RANGE_MAX_DAYS mirrors boardMaxSpanDays (handlers_robots.go). The endpoint
// 400s past it; the picker refuses before the fetch. Both want changing
// together.
export const RANGE_MAX_DAYS = 366;

// rangeProblem is the picker's client-side mirror of the endpoint's guards:
// null means the range is askable. ISO dates compare lexicographically.
// to == today is allowed, mirroring the handler: a legitimate inclusive end
// with no rolled-up rows yet, which the window note reports via data_days.
// Only a STRICTLY future date is refused.
export function rangeProblem(from, to, today) {
    if (!from || !to) return 'pick both dates';
    if (from > to) return 'the end is before the start';
    if (to > today) return 'the end date is in the future';
    const days = (Date.parse(to) - Date.parse(from)) / 86400000 + 1;
    if (days > RANGE_MAX_DAYS) {
        return days + ' days is past the ' + RANGE_MAX_DAYS + '-day limit';
    }
    return null;
}

// jsonOK surfaces the server's own 400 text instead of letting the error
// body parse as an empty-looking board. rangeProblem keeps the picker off
// the guards; this covers everything else (a hand-edited URL, a 500).
function jsonOK(r) {
    return r.json().then(function (d) {
        if (!r.ok) throw new Error(d && d.error ? d.error : 'HTTP ' + r.status);
        return d;
    });
}

// ── The compare verdict ─────────────────────────────────────────────────────
//
// Compare mode answers the engineer's actual question — "I changed things
// between these dates; what got better?" — by asking it over two explicit
// windows instead of one trailing one. The verdict is computed CLIENT-SIDE
// from two board payloads: every guard it needs is already on the wire, and a
// second server surface would re-derive the annotation's arithmetic in a
// place the annotation itself cannot see.

// DELTA_SIGNIFICANT is the smallest attributable movement worth calling a
// change — the annotation's own ±0.02 (see guard 5 in the panel), named here
// so the two callers cannot drift apart.
export const DELTA_SIGNIFICANT = 0.02;
// MISS_RATE_MOVED_MATERIALLY mirrors the server's noEstimateMovedMaterially
// (localization_board.go): above ten points the two sides' conditioned
// figures measure different populations and the p50 delta is an artifact of
// what survived. Both constants want changing together.
export const MISS_RATE_MOVED_MATERIALLY = 0.10;

// deltaVerdict grades one lane between two windows.
//
// a and b are the lane's BoardLane rows from the two boards; plantA/plantB
// are the two boards' plant p50 estimates (numbers or null). Returns one of:
//
//   better | worse | neutral — the attributable delta cleared ±0.02, didn't,
//                          or sat on the fence
//   suppressed          — the no-estimate rate moved > 10 pts between the
//                          windows; the averages are not comparable
//   thin                — either side is below min_samples; too few to say
//   nodata              — either side has no estimate at all (nobody drove
//                          it, or no histogram survived)
//
// The delta is PLANT-ADJUSTED before the threshold is applied: a lane that
// rose with the whole plant has not improved, and crediting it is the
// attribution failure the annotation's guard 2 exists to prevent.
export function deltaVerdict(a, b, plantA, plantB, minSamples) {
    if (!a || !b) return 'nodata';
    if (a.p50_estimate == null || b.p50_estimate == null) return 'nodata';
    if ((a.samples || 0) < minSamples || (b.samples || 0) < minSamples) return 'thin';
    const missA = a.samples ? a.sentinel_samples / a.samples : 0;
    const missB = b.samples ? b.sentinel_samples / b.samples : 0;
    if (Math.abs(missB - missA) > MISS_RATE_MOVED_MATERIALLY) return 'suppressed';
    const pa = plantA == null ? 0 : plantA;
    const pb = plantB == null ? 0 : plantB;
    const attributable = (b.p50_estimate - a.p50_estimate) - (pb - pa);
    if (attributable > DELTA_SIGNIFICANT) return 'better';
    if (attributable < -DELTA_SIGNIFICANT) return 'worse';
    return 'neutral';
}

// annotationVerdict grades an annotation payload (the /lane-change answer)
// for the change block's banner. It is the annotation's own arithmetic —
// attributable = lane delta − plant delta, ±0.02, withheld when the miss
// rate moved — lifted out of the table cell it used to hide in. Returned as
// data, not HTML, so the threshold and the plant-null and suppressed cases
// are assertable without a DOM.
export function annotationVerdict(c) {
    if (!c) return null;
    if (c.suppress_p50) {
        return { cls: 'lb-suppressed', word: 'not comparable',
            num: null, lane: null, plant: null };
    }
    if (c.p50_before == null || c.p50_after == null) {
        return { cls: 'lb-neutral', word: 'no p50 one side',
            num: null, lane: null, plant: null };
    }
    const d = c.p50_after - c.p50_before;
    const hasPlant = c.plant_delta !== null && c.plant_delta !== undefined;
    const attributable = hasPlant ? d - c.plant_delta : null;
    let cls = 'lb-neutral', word = 'no change', num = null;
    if (attributable !== null) {
        if (attributable > DELTA_SIGNIFICANT) { cls = 'lb-better'; word = 'better'; num = attributable; }
        else if (attributable < -DELTA_SIGNIFICANT) { cls = 'lb-worse'; word = 'worse'; num = attributable; }
    }
    return { cls: cls, word: word, num: num, lane: d,
        plant: hasPlant ? c.plant_delta : null };
}

// VERDICT_STROKE / VERDICT_TOKEN / VERDICT_DASH are the compare-mode marks.
//
// HUE ALONE DOES NOT CARRY DIRECTION. Green and coral collapse under
// deuteranomaly at dE 6.0 — the measured reason the band ramp carries weight
// as its second channel — so direction gets its own non-hue channel: dashed
// means worse, solid means better. The map stays legible desaturated to
// greyscale, and the dash is also the reason worse is not merely "red, but
// thicker" — a reader comparing two boards side by side sees stroke pattern,
// not weight differences.
export const VERDICT_STROKE = {
    better: 3.4, worse: 3.4, neutral: 1.6, suppressed: 2.6,
    thin: 1.3, nodata: 1.3
};
export const VERDICT_TOKEN = {
    better: 'var(--viz-green)', worse: 'var(--viz-coral)',
    neutral: 'var(--text-muted)', suppressed: 'var(--viz-amber)',
    thin: 'var(--text-muted)', nodata: 'var(--text-muted)'
};
export const VERDICT_DASH = {
    worse: '7 5', suppressed: '2 3'
};
const VERDICT_LABEL = {
    better: 'better', worse: 'worse', neutral: 'no change',
    suppressed: 'not comparable', thin: 'too few readings',
    nodata: 'no data one side'
};

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
        from: '', to: '',    // the main view's explicit day range; ISO dates
        compare: false,      // compare mode: one day against one day
        boardA: null,        // the "before" (day A) board in compare mode
        dayA: '', dayB: '',  // compare days A (earlier) and B (later); ISO dates
        showChanges: true, showReflectors: true,
        selected: null,      // lane key: area + lane, as serverLaneKey builds it
        focusDiff: null,     // diff id — dims lanes that edit did not touch
        change: null,        // the selected lane's annotation, fetched on select
        robots: [],
        robot: '',            // vehicle_id filter; '' is the fleet view
        // Viewport. scale/tx/ty are the screen transform; strokes divide by
        // scale so they hold their SCREEN size — a lane that thickened as you
        // zoomed would hide the geometry underneath it, and at 5× map zoomed
        // out it would be an unclickable hair.
        scale: 1, tx: 0, ty: 0, fitScale: 1, fitTx: 0, fitTy: 0,
        rotate90: false, proj: makeProjector(false)
    };

    root.innerHTML =
        '<div class="lb-controls">' +
        '  <label class="lb-dt" id="lb-range-wrap">' +
        '    <input type="date" id="lb-from" title="First complete day">' +
        '    <span aria-hidden="true">→</span>' +
        '    <input type="date" id="lb-to" title="Last complete day">' +
        '  </label>' +
        '  <label class="lb-toggle"><input type="checkbox" id="lb-compare"> Compare</label>' +
        '  <span id="lb-cmp" class="lb-cmp" hidden>' +
        '    <label class="lb-dt">A <input type="date" id="lb-day-a" title="The earlier day"></label>' +
        '    <label class="lb-dt">vs B <input type="date" id="lb-day-b" title="The later day"></label>' +
        '  </span>' +
        '  <label class="lb-toggle lb-robot-pick"><select id="lb-robot">' +
        '    <option value="">Fleet</option>' +
        '  </select></label>' +
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

    // ── the range picker ─────────────────────────────────────────────────
    //
    // An explicit from→to REPLACED the 7d/30d presets. A preset is a trailing
    // label, not a question: "30d" cannot ask about the week before the map
    // edit, and its only virtue — being one click — is what the two date
    // fields lose nothing of, since they open on the last seven complete
    // days. The roll-up closes complete days, so the picker's ends are DAYS
    // and both open on yesterday as the last closed one; a "today" end is a
    // day that has no rows yet and reads as a plant-wide dropout.
    const fromIn = root.querySelector('#lb-from');
    const toIn = root.querySelector('#lb-to');
    {
        const seed = seedMainRange(new Date());
        state.from = seed.from; state.to = seed.to;
        fromIn.value = seed.from; toIn.value = seed.to;
    }

    function loadMainIfAskable() {
        const problem = rangeProblem(state.from, state.to, iso(new Date()));
        note.textContent = problem || '';
        if (!problem) load();
    }
    fromIn.addEventListener('change', function () {
        state.from = fromIn.value; loadMainIfAskable();
    });
    toIn.addEventListener('change', function () {
        state.to = toIn.value; loadMainIfAskable();
    });

    // ── compare mode ─────────────────────────────────────────────────────
    //
    // ONE DAY AGAINST ONE DAY, A (earlier) vs B (later), and the map
    // recolours by VERDICT rather than by band. The old mode let two
    // arbitrary RANGES fight — a 3-day A against a 12-day B is not a
    // comparison, and nothing about the verdict arithmetic survives unequal
    // windows intact. A day is the smallest window the roll-up can answer
    // honestly (both sides close, no mixing of geometry versions inside a
    // side), which makes the two sides cleanly comparable populations. It
    // opens with B yesterday, A seven days before it — the same weekday, the
    // day-grain version of the old equal-stretch seed.
    //
    // Both days ride the SAME board endpoint via its from/to params (from =
    // to = the day); no second server surface exists to drift out of sync
    // with the annotation's guards.
    const cmpWrap = root.querySelector('#lb-cmp');
    const cmpToggle = root.querySelector('#lb-compare');
    const dayAIn = root.querySelector('#lb-day-a');
    const dayBIn = root.querySelector('#lb-day-b');

    function seedCompareDaysInputs() {
        const seed = seedCompareDays(new Date());
        state.dayA = seed.a; state.dayB = seed.b;
        dayAIn.value = seed.a; dayBIn.value = seed.b;
    }

    function dayProblem() {
        if (!state.dayA || !state.dayB) return 'pick both days';
        if (state.dayA === state.dayB) return 'the two days are the same';
        if (state.dayA > state.dayB) return 'day A is after day B';
        if (state.dayB > iso(new Date())) return 'day B is in the future';
        return null;
    }
    // dayB == today is allowed on the same terms the range picker allows it.
    function loadCompareIfAskable() {
        const problem = dayProblem();
        note.textContent = problem || '';
        if (!problem) load();
    }
    dayAIn.addEventListener('change', function () {
        state.dayA = dayAIn.value; loadCompareIfAskable();
    });
    dayBIn.addEventListener('change', function () {
        state.dayB = dayBIn.value; loadCompareIfAskable();
    });

    cmpToggle.addEventListener('change', function () {
        state.compare = cmpToggle.checked;
        cmpWrap.hidden = !state.compare;
        // The main picker is inert in compare mode — its change handler would
        // otherwise fire a compare-shaped load, and a control that does
        // something other than what it shows is the failure this page keeps
        // refusing.
        fromIn.disabled = state.compare;
        toIn.disabled = state.compare;
        if (state.compare) seedCompareDaysInputs();
        load();
    });

    // ── robot selector ───────────────────────────────────────────────────
    //
    // Picking an AMR switches the whole board into that robot's world: every
    // lane recolours to its performance, and the "plant" baseline becomes the
    // robot's aggregate. Fleet is the default and the way back. The list comes
    // from /api/robots rather than the SSE position feed, because a
    // parked or disconnected AMR — exactly the one a reader wants to inspect —
    // may carry no current position and still have a confidence record.
    const robotSel = root.querySelector('#lb-robot');
    robotSel.addEventListener('change', function () {
        state.robot = robotSel.value;
        load();
    });
    populateRobotFilter(robotSel, state);

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

    // boardWindows (the 7d/30d presets) is gone: the picker asks every window
    // as an explicit from→to, which is the only spelling of the question the
    // endpoint answers without a label losing contact with its days.

    // ── data ─────────────────────────────────────────────────────────────
    async function load() {
        // The robot param rides on the board URL only; the edges never change
        // with the filter, so they are not refetched. An empty robot is fleet.
        const robotParam = state.robot ? '&robot=' + encodeURIComponent(state.robot) : '';
        const boardURL = function (from, to) {
            return '/api/robots/localization?from=' + from + '&to=' + to + robotParam;
        };
        let boardP;
        if (state.compare) {
            // One day each side: from = to = the day.
            boardP = o.fetchBoard
                ? Promise.all([o.fetchBoard(state.dayA, state.robot), o.fetchBoard(state.dayB, state.robot)])
                : Promise.all([boardURL(state.dayA, state.dayA), boardURL(state.dayB, state.dayB)]
                    .map(function (u) { return fetch(u).then(jsonOK); }));
        } else {
            boardP = o.fetchBoard
                ? o.fetchBoard(state.from + '/' + state.to, state.robot)
                : fetch(boardURL(state.from, state.to)).then(jsonOK);
        }
        const [edges, boards] = await Promise.all([
            o.fetchEdges ? o.fetchEdges() : fetch('/api/map/edges').then(function (r) { return r.json(); }),
            boardP
        ]);
        state.edges = edges || [];
        if (state.compare && Array.isArray(boards)) {
            state.boardA = boards[0] || null;
            state.board = boards[1] || null;
        } else {
            state.boardA = null;
            state.board = Array.isArray(boards) ? null : (boards || null);
        }
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

    // verdictByLane builds the compare-mode colour key: one verdict per
    // physical lane, from that lane's rows on both boards. The plant values
    // are each board's own p50, so the adjustment is over the SAME two
    // windows the lane is compared across.
    function verdictByLane() {
        if (!state.compare || !state.boardA || !state.board) return null;
        const byLane = new Map();
        state.boardA.lanes.forEach(function (l) { byLane.set(l.lane, { a: l }); });
        state.board.lanes.forEach(function (l) {
            const e = byLane.get(l.lane);
            if (e) e.b = l; else byLane.set(l.lane, { b: l });
        });
        const plantA = state.boardA.plant ? state.boardA.plant.p50_estimate : null;
        const plantB = state.board.plant ? state.board.plant.p50_estimate : null;
        const minN = state.board.min_samples || 20;
        const out = new Map();
        byLane.forEach(function (pair, lane) {
            out.set(lane, deltaVerdict(pair.a || null, pair.b || null, plantA, plantB, minN));
        });
        return out;
    }

    function draw() {
        while (map.firstChild) map.removeChild(map.firstChild);
        const world = svg('g', { id: 'lb-world' });
        map.appendChild(world);
        if (!state.edges.length) return;

        const P = state.proj;
        const verdicts = verdictByLane();
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
            // COMPARE MODE replaces the band with the verdict: the question
            // the reader brought is "what changed between A and B", and a
            // band answers "how good is B". The two colourings never share a
            // legend — drawLegend swaps wholesale with the mode.
            const verdict = verdicts ? (verdicts.get(row.st ? row.st.lane : '') || 'nodata') : null;
            const mark = verdict
                ? {
                    token: VERDICT_TOKEN[verdict], stroke: VERDICT_STROKE[verdict],
                    dash: VERDICT_DASH[verdict] || null
                }
                : {
                    token: BAND_TOKEN[band], stroke: BAND_STROKE[band],
                    dash: band === 'blind' ? '5 4' : null
                };
            const dim = focus && !focus.has(row.st ? row.st.lane : '');

            if (state.showChanges && row.st && row.st.changed && !dim) {
                // Sky halo — DATA. A different category from selection, so a
                // different hue; two marks sharing a hue was unreadable.
                lg.appendChild(svg('path', {
                    d: d, fill: 'none', stroke: 'var(--viz-sky)', opacity: 0.28,
                    'stroke-width': 10, 'data-w': 10, 'stroke-linecap': 'round'
                }));
            }
            // The click target, under the mark it belongs to. Transparent and
            // wide; carries the same data-lane, so the existing walk-up in the
            // click handler resolves it without knowing it exists.
            //
            // pointer-events:stroke is explicit rather than relying on the
            // default: `visiblePainted` does hit-test a transparent stroke, but
            // that reads like an accident to anyone changing the fill later, and
            // the whole element only exists to be hit.
            //
            // Appended BEFORE the visible path so the mark still paints on top
            // and stays the thing that receives a click where it is actually
            // drawn; the target only picks up the misses either side of it.
            lg.appendChild(svg('path', {
                d: d, fill: 'none', stroke: 'transparent',
                'stroke-width': LANE_HIT_STROKE, 'data-w': LANE_HIT_STROKE,
                'stroke-linecap': 'round', 'data-lane': row.key,
                'pointer-events': 'stroke', 'class': 'lb-lane-hit'
            }));
            const p = svg('path', {
                d: d, fill: 'none',
                stroke: mark.token, opacity: dim ? 0.12 : 1,
                'stroke-width': mark.stroke, 'data-w': mark.stroke,
                'data-dash': mark.dash,
                'stroke-linecap': 'round', 'data-lane': row.key,
                'class': 'lb-lane'
            });
            lg.appendChild(p);

            if (state.selected === row.key) {
                // Near-white rim — INTERFACE CHROME, not data.
                lg.appendChild(svg('path', {
                    d: d, fill: 'none', stroke: 'var(--viz-primary)', opacity: 0.9,
                    'stroke-width': mark.stroke + 2.4, 'data-w': mark.stroke + 2.4,
                    'stroke-linecap': 'round', 'pointer-events': 'none'
                }));
                lg.appendChild(svg('path', {
                    d: d, fill: 'none', stroke: mark.token,
                    'stroke-width': mark.stroke, 'data-w': mark.stroke,
                    'data-dash': mark.dash,
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
        lg.innerHTML = ['good', 'fair', 'watch', 'poor', 'blind', 'nodata'].map(function (b) {
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
    // The verdict leads as a banner — the number that answers "did the edit
    // help" is the block's whole point, and it used to sit 11px and grey in a
    // table cell third from the top.
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
        const v = annotationVerdict(c);
        // The banner. Suppressed and no-p50 are also verdicts, not footnotes —
        // "withheld" carries the same visual weight as "worse", because an
        // engineer skimming banners who skips the table must still see that
        // nothing was concluded.
        const banner = '<div class="lb-verdict-banner ' + v.cls +
            (c.below_min_n ? ' lb-banner-thin' : '') + '">' + v.word +
            (v.num === null ? '' : ' ' + (v.num >= 0 ? '+' : '') + v.num.toFixed(3)) +
            (v.plant === null ? '' : '<span class="lb-plant-inline">lane ' +
                (v.lane >= 0 ? '+' : '') + v.lane.toFixed(3) + ' · plant ' +
                (v.plant >= 0 ? '+' : '') + v.plant.toFixed(3) + '</span>') +
            (c.below_min_n ? '<span class="lb-plant-inline">few readings one side — ' +
                'the number stands but is not strong</span>' : '') +
            '</div>';
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
            // the plant shows neutral, not the success colour. The class comes
            // from annotationVerdict so banner and cell can never disagree.
            rows += '<tr><td>p50</td><td>' + fmtP(c.p50_before) + ' → ' + fmtP(c.p50_after) +
                '</td><td class="' + v.cls + '">' +
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

        return '<div class="lb-hist-title">Change</div>' + banner +
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
        if (state.compare && state.boardA) {
            note.textContent = 'A ' + state.dayA + '  vs  B ' + state.dayB;
            drawComparePanel();
            return;
        }
        note.textContent = w.data_days < w.requested_days
            ? w.data_days + ' of ' + w.requested_days + ' days hold data'
            : w.requested_days + ' days';

        if (!state.selected) {
            const p = state.board.plant;
            // Under a robot filter the "plant" baseline IS that robot's
            // aggregate — the lanes it drove, merged — so the header names it
            // rather than saying "Plant". Fleet view keeps the plant label.
            const scope = state.robot ? state.robot : 'Plant';
            panel.innerHTML = '<div class="lb-hd">' + scope + '</div>' +
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
    // drawComparePanel is the compare mode's panel: the plant-level A→B
    // summary when nothing is selected, both windows side by side for the
    // selected lane. The verdict leads in both — it is the answer to the
    // question compare mode exists for.
    function drawComparePanel() {
        const plantA = state.boardA.plant || {};
        const plantB = state.board.plant || {};
        const verdicts = verdictByLane() || new Map();
        const tally = {};
        verdicts.forEach(function (v) { tally[v] = (tally[v] || 0) + 1; });

        if (!state.selected) {
            const scope = state.robot ? state.robot : 'Plant';
            const dP = (plantA.p50_estimate != null && plantB.p50_estimate != null)
                ? plantB.p50_estimate - plantA.p50_estimate : null;
            panel.innerHTML = '<div class="lb-hd">' + scope + ' · compare</div>' +
                '<div class="lb-stat"><span>p50 A → B</span><b>' + fmtP(plantA.p50_estimate) +
                ' → ' + fmtP(plantB.p50_estimate) +
                (dP === null ? '' : ' (' + (dP >= 0 ? '+' : '') + dP.toFixed(3) + ')') +
                '</b></div>' +
                '<div class="lb-stat"><span>readings A → B</span><b>' +
                (plantA.samples || 0) + ' → ' + (plantB.samples || 0) + '</b></div>' +
                verdictCountsBlock(tally) +
                '<p class="lb-note">The plant delta is the baseline every lane verdict is ' +
                'measured against — a lane that moved with the plant reads neutral, not ' +
                'better. Dashed marks are not comparable or worse; solid green is the only ' +
                'improvement.</p>';
            return;
        }

        const row = laneRows().find(function (r) { return r.key === state.selected; });
        if (!row || !row.st) {
            panel.innerHTML = '<div class="lb-hd">' + (state.selected || '') + '</div>' +
                '<p class="lb-note">No readings in window B. Not the same as zero — ' +
                'nothing drove here, or nothing was recorded.</p>';
            return;
        }
        const laneA = (state.boardA.lanes || []).find(function (l) { return l.lane === row.st.lane; });
        const v = verdicts.get(row.st.lane) || 'nodata';
        // The attributable number, shown where the verdict is not suppressed —
        // the same arithmetic deltaVerdict applied.
        let attrLine = '';
        if (v === 'better' || v === 'worse' || v === 'neutral') {
            const pa = plantA.p50_estimate == null ? 0 : plantA.p50_estimate;
            const pb = plantB.p50_estimate == null ? 0 : plantB.p50_estimate;
            const attr = (row.st.p50_estimate - (laneA ? laneA.p50_estimate : 0)) - (pb - pa);
            attrLine = '<div class="lb-stat"><span>attributable Δ</span><b>' +
                (attr >= 0 ? '+' : '') + attr.toFixed(3) + '</b></div>';
        }
        panel.innerHTML = '<div class="lb-hd">' + row.st.lane + '</div>' +
            '<div class="lb-verdict lb-verdict-' + v + '">' + VERDICT_LABEL[v] + '</div>' +
            attrLine +
            '<div class="lb-stat"><span>p50 A → B</span><b>' +
            fmtP(laneA ? laneA.p50_estimate : null) + ' → ' + fmtP(row.st.p50_estimate) +
            '</b></div>' +
            '<div class="lb-stat"><span>no estimate A → B</span><b>' +
            pctOf(laneA) + ' → ' + pctOf(row.st) + '</b></div>' +
            '<div class="lb-stat"><span>readings A → B</span><b>' +
            (laneA ? laneA.samples : 0) + ' → ' + row.st.samples + '</b></div>' +
            (v === 'suppressed'
                ? '<p class="lb-warn">The no-estimate rate moved too far between the ' +
                  'windows for the two averages to describe the same population — the ' +
                  'verdict is withheld rather than guessed.</p>' : '') +
            (v === 'thin'
                ? '<p class="lb-warn">Too few readings on one side of the comparison ' +
                  'to say anything. Greyed rather than hidden — an absent number reads ' +
                  'as fine.</p>' : '');
    }

    function verdictCountsBlock(tally) {
        return ['better', 'worse', 'neutral', 'suppressed', 'thin', 'nodata']
            .map(function (v) {
                return '<div class="lb-stat"><span class="lb-verdict-dot lb-verdict-' + v +
                    '">' + VERDICT_LABEL[v] + '</span><b>' + (tally[v] || 0) + '</b></div>';
            }).join('');
    }

    function pctOf(l) {
        return l && l.samples ? ((l.sentinel_samples / l.samples) * 100).toFixed(1) + '%' : '—';
    }

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
        const w = 220, h0 = 56;
        const h = histPath(hist, w, h0);
        if (!h.line) return '<p class="lb-note">No distribution for this window.</p>';
        const total = (hist || []).reduce(function (a, b) { return a + b; }, 0);
        const share = total ? (h.sentinel / total) : 0;
        // The curve is translated +14 to clear the sentinel bar, so its
        // screen span is [14, 14+w]. The 1.0 tick sits at the CURVE's right
        // edge, not the viewBox's — the original drew it at 220 while the
        // curve ran to 234, and the top bin rendered to the right of the
        // "1.0" label, reading as values above 1.
        const x0 = 14, x1 = 14 + w;
        return '<div class="lb-hist-title">' + title + '</div>' +
            '<svg class="lb-hist" viewBox="0 0 240 70" role="img" aria-label="' + title + '">' +
            '<rect x="0" y="' + (h0 - Math.min(h0, share * h0 * 3)).toFixed(1) +
            '" width="8" height="' + Math.min(h0, share * h0 * 3).toFixed(1) +
            '" fill="var(--viz-coral)" opacity="0.85"><title>no estimate: ' +
            h.sentinel + '</title></rect>' +
            '<path d="' + h.line + '" transform="translate(' + x0 + ',0)" fill="none" ' +
            'stroke="var(--viz-primary)" stroke-width="1.4"/>' +
            '<text x="' + x0 + '" y="68" font-size="7" fill="var(--text-muted)">0 (no est.)</text>' +
            '<text x="' + x1 + '" y="68" font-size="7" fill="var(--text-muted)" text-anchor="end">1.0</text>' +
            '</svg>';
    }

    return {
        load: load,
        setRobots: function (list) { state.robots = list || []; draw(); },
        setRobot: function (id) {
            state.robot = id || '';
            if (robotSel.value !== state.robot) robotSel.value = state.robot;
            load();
        },
        _state: state,
        _laneRows: laneRows
    };
}

// populateRobotFilter fills the AMR dropdown once, preserving any current
// selection. Sorted by vehicle id so the list is stable across the 2s SSE
// refreshes that could otherwise retrigger it.
//
// /api/robots returns fleet.RobotStatus with NO json tags, so Go marshals the
// field names verbatim — VehicleID in PascalCase, not vehicle_id. The SSE
// position feed is a different code path that snake-cases; do not assume the
// two agree.
function populateRobotFilter(sel, state) {
    fetch('/api/robots').then(function (r) { return r.json(); }).then(function (robots) {
        const ids = (robots || []).map(function (r) { return r.VehicleID || r.vehicle_id; })
            .filter(Boolean).sort();
        const prev = sel.value;
        // Rebuild only when the set changed, so a refresh that finds the same
        // fleet does not blow away the open dropdown mid-selection.
        const same = sel.options.length === ids.length + 1 &&
            ids.every(function (id, i) { return sel.options[i + 1].value === id; });
        if (same) return;
        sel.innerHTML = '<option value="">Fleet</option>' +
            ids.map(function (id) { return '<option value="' + id + '">' + id + '</option>'; }).join('');
        if (prev && ids.indexOf(prev) >= 0) sel.value = prev;
    }).catch(function () { /* the dropdown stays at Fleet; the board still loads */ });
}

