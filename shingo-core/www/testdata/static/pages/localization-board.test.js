// localization-board.test.js — the board's pure logic, run under node.
//
// What is asserted here is the part that would be WRONG SILENTLY: the key the
// client joins state to geometry with, the shape of the distribution the panel
// draws, and (at the end, through createBoard) the lanes as drawn on a
// synthetic scene. The rest of the DOM half (viewport gestures, rail, panel) is
// exercised by hand against the real page.

const fs = require('fs');
const path = require('path');
const vm = require('vm');

// The page code under test. This file lives in www/testdata, outside the
// embedded static tree, so no binary carries or serves it
// (TestStaticTree_HoldsNoTestJS); the code it reads stays in www/static.
const SRC_DIR = path.join(__dirname, '..', '..', '..', 'static', 'pages');

let failures = 0;
function check(name, cond, detail) {
    if (cond) { console.log('  ok   ' + name); return; }
    failures++;
    console.log('  FAIL ' + name + (detail ? '\n       ' + detail : ''));
}

// The module imports from an absolute /static/ path the browser resolves and
// node cannot, so the import is stripped and the module evaluated as a script —
// the same technique dashboard-map.curve.test.js uses on scene-geom.js.
//
// /g, not one shot: a second import would otherwise survive the strip and leave
// a script vm cannot parse, which reads as a syntax error rather than as a
// stale harness.
function load() {
    const file = path.join(SRC_DIR, '..', 'components', 'localization-board.js');
    const raw = fs.readFileSync(file, 'utf8');
    const stripped = raw.replace(/^import[^;]+;\s*/mg, '');
    if (stripped === raw) {
        throw new Error('localization-board.js no longer starts its imports at line start, ' +
            'which this harness strips; update load() in localization-board.test.js');
    }
    const src = stripped.replace(/^export /mg, '');
    // The seeds do their calendar arithmetic with plantclock's addDays. It is
    // evaluated in its own context and handed in, so the board is tested
    // against the real helper rather than a copy of it.
    const pc = loadPlantclock();
    // The DOM half is reached only through createBoard (the lane pins below);
    // document builds its SVG, window takes its drag listeners, and fetch is
    // the robot filter's /api/robots read, answered with no robots.
    const ctx = { console: console, Math: Math, Number: Number, Map: Map, Set: Set, Date: Date,
        addDays: pc.addDays, plantDate: pc.plantDate, serverNow: function () { return Date.now(); },
        document: { createElementNS: function (ns, name) { return svgEl(name); } },
        window: { addEventListener: function () {} },
        fetch: function () { return Promise.resolve({ json: function () { return Promise.resolve([]); } }); } };
    vm.createContext(ctx);
    // The stripped imports, each the real file, its imports and exports
    // stripped to declarations in this context: scene-geom (makeProjector,
    // rotate90For, laneKey, laneSpec), scene-net (sceneNet) over it, and the
    // robot map's rows-to-scene adapter (components/scene-rows.js sceneOfRows),
    // which reads scene-geom's isCoord.
    const strip = function (f) { return fs.readFileSync(f, 'utf8').replace(/^import[^;]+;\s*/mg, '').replace(/^export /mg, ''); };
    const shared = path.join(SRC_DIR, '..', '..', '..', '..', 'shared');
    vm.runInContext(strip(path.join(shared, 'scene-geom.js')), ctx);
    vm.runInContext(strip(path.join(shared, 'scene-net.js')), ctx);
    vm.runInContext(strip(path.join(SRC_DIR, '..', 'components', 'scene-rows.js')), ctx);
    vm.runInContext(src + '\n__out = { createBoard: createBoard, histPath: histPath, serverLaneKey: serverLaneKey, ' +
        'deltaVerdict: deltaVerdict, DELTA_SIGNIFICANT: DELTA_SIGNIFICANT, ' +
        'annotationVerdict: annotationVerdict, ' +
        'seedMainRange: seedMainRange, seedCompareDays: seedCompareDays, ' +
        'rangeProblem: rangeProblem, RANGE_MAX_DAYS: RANGE_MAX_DAYS, ' +
        'VERDICT_TOKEN: VERDICT_TOKEN, VERDICT_STROKE: VERDICT_STROKE, ' +
        'VERDICT_DASH: VERDICT_DASH, ' +
        'BAND_STROKE: BAND_STROKE, BAND_TOKEN: BAND_TOKEN };', ctx);
    return ctx.__out;
}

function loadPlantclock() {
    const file = path.join(SRC_DIR, '..', 'components', 'plantclock.js');
    const src = fs.readFileSync(file, 'utf8').replace(/^import[^;]+;\s*/mg, '').replace(/^export /mg, '');
    const ctx = { Intl: Intl, Date: Date, Map: Map, JSON: JSON, String: String, Number: Number, isNaN: isNaN,
        window: { PLANT_TZ: 'America/Chicago' }, serverNow: function () { return Date.now(); } };
    vm.createContext(ctx);
    vm.runInContext(src + '\n__out = { addDays: addDays, plantDate: plantDate };', ctx);
    return ctx.__out;
}

const m = load();
const pc = loadPlantclock();

// --- the join key --------------------------------------------------------
//
// This mirrors Segment.Lane() on the server. If the two ever disagree the board
// joins nothing, every lane renders "no data", and the page looks like a plant
// with no readings rather than like a bug.
console.log('serverLaneKey');
(function () {
    check('sorts the pair, so both stored directions give one key',
        m.serverLaneKey('LM48', 'LM10') === 'LM10-LM48' &&
        m.serverLaneKey('LM10', 'LM48') === 'LM10-LM48',
        m.serverLaneKey('LM48', 'LM10') + ' vs ' + m.serverLaneKey('LM10', 'LM48'));

    // An endpoint name containing a hyphen is exactly why the client must not
    // split the server's lane string back apart. Building the key forward from
    // the two names is safe; parsing "AP-1-AP-2" is a guess.
    check('survives a hyphen inside an endpoint name',
        m.serverLaneKey('AP-1', 'AP-2') === 'AP-1-AP-2',
        m.serverLaneKey('AP-1', 'AP-2'));

    // An unnameable edge has no lane key — the same rule the server enforces,
    // and returning something plausible here would re-introduce the defect the
    // quarantine exists to surface.
    check('an unnamed endpoint yields no key',
        m.serverLaneKey('', 'LM10') === '' && m.serverLaneKey('LM10', '') === '',
        JSON.stringify([m.serverLaneKey('', 'LM10'), m.serverLaneKey('LM10', '')]));
})();

// --- the distribution ----------------------------------------------------
console.log('histPath');
(function () {
    // Bin 0 is the no-estimate sentinel and must NOT be part of the curve. It
    // is a point, not a range, and folding it in would draw a plant with dead
    // zones as one that is merely poor at the low end.
    const hist = new Array(51).fill(0);
    hist[0] = 40;   // sentinel
    hist[45] = 10;  // a real cluster near 0.9

    const h = m.histPath(hist, 200, 50);
    check('the sentinel is reported separately from the curve',
        h.sentinel === 40, 'sentinel=' + h.sentinel);
    check('the curve scales to the largest VALUE bin, not to the sentinel',
        h.max === 10, 'max=' + h.max +
        ' — scaling to the sentinel would flatten every real reading to nothing');
    check('the curve is drawn', h.line.indexOf('M') === 0, JSON.stringify(h.line.slice(0, 40)));

    // A histogram holding only sentinels has no curve to draw, and must say so
    // rather than emitting a flat line at zero that reads as "measured, bad".
    const blind = new Array(51).fill(0);
    blind[0] = 12;
    const hb = m.histPath(blind, 200, 50);
    check('an all-sentinel histogram draws no curve',
        hb.line === '' && hb.sentinel === 12,
        JSON.stringify(hb));

    check('a missing histogram is absent, not empty-at-zero',
        m.histPath(null, 200, 50).line === '' && m.histPath([], 200, 50).line === '',
        'a padded or invented curve answers from a distribution nobody stored');

    // THE SPAN CONTRACT: histPath draws over exactly [0, w]. histBlock
    // translates the curve +14 to clear the sentinel bar, so the "1.0" axis
    // label belongs at 14 + w — the curve's actual right edge. The original
    // pinned the label at 220 while the curve ran to 234, and the top bin
    // rendered to the right of "1.0", which read as values above 1. Confidence
    // is bounded at 1.0 by construction; only the axis can be wrong.
    const full = new Array(51).fill(0);
    full[50] = 5;              // everything in the top bin
    const hf = m.histPath(full, 220, 56);
    const firstX = parseFloat(hf.line.slice(1));
    const lastX = parseFloat(hf.line.slice(hf.line.lastIndexOf('L') + 1));
    check('the curve starts at x=0 and ends at exactly x=w',
        firstX === 0 && lastX === 220,
        'first=' + firstX + ' last=' + lastX +
        ' — the 1.0 label sits at 14 + w and drifts with this span');
})();

// --- the redundant channel -----------------------------------------------
console.log('bands');
(function () {
    // Hue may not carry the ordering alone: measured under deuteranomaly on
    // dark, green and coral collapse to dE 6.0. Weight is the second channel,
    // and it has to be MONOTONIC across the ordered bands or it carries
    // nothing.
    const order = ['good', 'fair', 'watch', 'poor', 'blind'];
    let mono = true, prev = -Infinity, seen = [];
    order.forEach(function (b) {
        const w = m.BAND_STROKE[b];
        seen.push(b + '=' + w);
        if (!(w > prev)) mono = false;
        prev = w;
    });
    check('stroke weight rises monotonically across the ordered bands',
        mono, seen.join(' '));

    check('every band has a token and a weight',
        order.concat(['nodata']).every(function (b) {
            return m.BAND_TOKEN[b] && m.BAND_STROKE[b];
        }), JSON.stringify(m.BAND_TOKEN));

    // no-data is not a band. If it ever shares a hue with a measured band, a
    // lane nobody drove renders as a measurement.
    check('no-data does not share a hue with any measured band',
        order.every(function (b) { return m.BAND_TOKEN[b] !== m.BAND_TOKEN.nodata; }),
        m.BAND_TOKEN.nodata);
})();

// --- the window seeds and the picker's guards ------------------------------
console.log('range seeds and guards');
(function () {
    // The seeds take the PLANT's date as a bare YYYY-MM-DD, pinned here with a
    // FIXED date so the assertion cannot drift with the clock. The main range
    // ends on today (the brief: the opening range includes the current plant
    // day; today's rows arrive with the nightly roll-up and the window note
    // says so). It used to end yesterday.
    const TODAY = '2026-08-19';

    const main = m.seedMainRange(TODAY);
    check('the main seed is the last seven days, ending on the plant’s today',
        main.from === '2026-08-13' && main.to === '2026-08-19',
        JSON.stringify(main));

    // Compare stays one closed day against another: B yesterday, A a week
    // before it. Today has no rolled-up rows, so B = today would compare
    // against nothing.
    const cmp = m.seedCompareDays(TODAY);
    check('the compare seed is yesterday vs the same weekday a week before',
        cmp.b === '2026-08-18' && cmp.a === '2026-08-11',
        JSON.stringify(cmp) + ' — seven days apart, not six, or it compares ' +
        'adjacent weekdays');

    // An evening at a plant west of UTC: 22:00 CDT on Aug 19 is 03:00 UTC on
    // Aug 20. The seed used to take the browser's instant and do UTC date
    // math, so here its range already ended on the plant's today while at noon
    // it ended yesterday. Today comes from plantDate in the plant zone, so the
    // evening and noon seeds are the same range.
    const eveningDay = pc.plantDate('2026-08-20T03:00:00Z');
    const noonDay = pc.plantDate('2026-08-19T17:00:00Z');
    check('the plant date of a late evening is still the plant’s today',
        eveningDay === '2026-08-19' && noonDay === '2026-08-19',
        eveningDay + ' / ' + noonDay);
    check('an evening seed and a noon seed are the same range',
        JSON.stringify(m.seedMainRange(eveningDay)) === JSON.stringify(m.seedMainRange(noonDay)) &&
        m.seedMainRange(eveningDay).to === '2026-08-19',
        JSON.stringify(m.seedMainRange(eveningDay)));
    check('a seed across a month boundary is calendar arithmetic',
        m.seedMainRange('2026-03-03').from === '2026-02-25' &&
        m.seedCompareDays('2026-03-01').b === '2026-02-28',
        JSON.stringify([m.seedMainRange('2026-03-03'), m.seedCompareDays('2026-03-01')]));

    // The endpoint's guards, mirrored client-side. Every rule the server
    // 400s on should be refused before the fetch, or the picker's error
    // surface is a network toast instead of the input.
    check('a good range passes',
        m.rangeProblem('2026-08-01', '2026-08-13', TODAY) === null);
    check('a one-day range is askable (from === to)',
        m.rangeProblem('2026-08-13', '2026-08-13', TODAY) === null,
        'compare mode asks for exactly this shape');
    check('a half-picked range is refused',
        !!m.rangeProblem('', '2026-08-13', TODAY) &&
        !!m.rangeProblem('2026-08-01', '', TODAY));
    check('an inverted range is refused',
        m.rangeProblem('2026-08-13', '2026-08-01', TODAY) !== null);
    check('an end of today is allowed, mirroring the handler',
        m.rangeProblem('2026-08-01', TODAY, TODAY) === null,
        'the handler serves it and lets data_days say the day is empty; ' +
        'the client refusing it would be stricter than the server');
    check('a strictly future end is refused',
        m.rangeProblem('2026-08-01', '2026-08-20', TODAY) !== null);
    // The inclusive span cap: Aug 1 2025 → Aug 1 2026 is 366 days and fits;
    // one day more does not. Both sides pinned, the same boundary the
    // server's boardMaxSpanDays 400s on.
    check('the span cap is inclusive at both edges',
        m.rangeProblem('2025-08-01', '2026-08-01', TODAY) === null &&
        m.rangeProblem('2025-07-31', '2026-08-01', TODAY) !== null,
        '366 fits, 367 does not — must move with boardMaxSpanDays');
    check('RANGE_MAX_DAYS still mirrors the server cap',
        m.RANGE_MAX_DAYS === 366);
})();

// --- the compare verdict --------------------------------------------------
console.log('deltaVerdict');
(function () {
    const MIN = 20;
    const lane = function (p50, n, sentinel) {
        return { p50_estimate: p50, samples: n, sentinel_samples: sentinel || 0 };
    };

    // A lane that rose exactly with the plant has NOT improved. This is the
    // guard that stops "better" from meaning "the whole plant had a good
    // week" — the attribution failure the annotation's plant baseline exists
    // to prevent, carried into the map.
    check('a lane that follows the plant reads neutral',
        m.deltaVerdict(lane(0.60, 100), lane(0.70, 100), 0.75, 0.85, MIN) === 'neutral',
        'lane +0.10, plant +0.10 — attributable is zero');

    check('a lane that beats the plant reads better',
        m.deltaVerdict(lane(0.60, 100), lane(0.70, 100), 0.75, 0.76, MIN) === 'better',
        'lane +0.10, plant +0.01 — attributable +0.09');

    check('a lane that falls behind a rising plant reads worse',
        m.deltaVerdict(lane(0.60, 100), lane(0.62, 100), 0.75, 0.85, MIN) === 'worse',
        'lane +0.02, plant +0.10 — attributable -0.08');

    // The threshold itself, tested at ±0.001 around it. The boundary is the
    // annotation's own, and both sides of it are pinned so retuning one
    // caller cannot silently hollow out the other.
    check('the significance threshold is applied to the attributable delta',
        m.deltaVerdict(lane(0.60, 100), lane(0.60 + m.DELTA_SIGNIFICANT + 0.001, 100), 0.5, 0.5, MIN) === 'better' &&
        m.deltaVerdict(lane(0.60, 100), lane(0.60 + m.DELTA_SIGNIFICANT - 0.001, 100), 0.5, 0.5, MIN) === 'neutral',
        'just over and just under DELTA_SIGNIFICANT, plant flat');

    // The miss-rate guard, mirrored from the server: routing into a bad
    // reflector zone makes the conditioned average go UP while things get
    // WORSE, so a miss rate that moved > 10 points suppresses the verdict
    // entirely rather than reporting a delta over different populations.
    check('a miss rate that moved materially suppresses the verdict',
        m.deltaVerdict(lane(0.60, 100, 0), lane(0.90, 100, 20), 0.5, 0.5, MIN) === 'suppressed',
        'miss rate 0% → 20%');

    check('a miss rate that moved two points does not suppress',
        m.deltaVerdict(lane(0.60, 100, 0), lane(0.70, 100, 2), 0.5, 0.5, MIN) === 'better',
        'miss rate 0% → 2%');

    // Guards that grey rather than hide — absence reads as fine.
    check('below the minimum n on either side greys',
        m.deltaVerdict(lane(0.60, MIN - 1), lane(0.90, 100), 0.5, 0.5, MIN) === 'thin' &&
        m.deltaVerdict(lane(0.60, 100), lane(0.90, MIN - 1), 0.5, 0.5, MIN) === 'thin',
        'either side below min_samples');

    check('no estimate on either side is nodata',
        m.deltaVerdict(lane(null, 100), lane(0.90, 100), 0.5, 0.5, MIN) === 'nodata' &&
        m.deltaVerdict(lane(0.60, 100), lane(null, 100), 0.5, 0.5, MIN) === 'nodata',
        'a missing side is a missing answer, not a zero');

    check('a lane present on one board only is nodata',
        m.deltaVerdict(null, lane(0.90, 100), 0.5, 0.5, MIN) === 'nodata',
        'nobody drove it in one window');

    // A missing PLANT baseline on ONE side: the adjustment that side would
    // contribute is zero, so the OTHER side's plant level enters the
    // attributable delta raw. A plant at 0.5 against a null reads as the plant
    // "rising" 0.5, and a +0.10 lane reads worse. That is the honest failure
    // of a half-missing baseline — it does not invent symmetry, and the
    // verdict says the data cannot support the comparison rather than
    // crediting the lane. Pinned as-is so a future "friendlier" null
    // handling cannot silently start inventing baselines.
    check('a half-missing plant baseline biases, does not invent',
        m.deltaVerdict(lane(0.60, 100), lane(0.70, 100), null, 0.5, MIN) === 'worse',
        'plant null vs 0.5: the 0.5 enters the attributable delta unpaired');

    // Both plant baselines missing: no adjustment, and the raw lane delta
    // stands alone.
    check('no plant baselines at all leaves the raw delta',
        m.deltaVerdict(lane(0.60, 100), lane(0.70, 100), null, null, MIN) === 'better',
        'null on both sides — no adjustment, no invention');
})();

// The change annotation's verdict — same arithmetic as deltaVerdict, but over
// a /lane-change payload, and it must return its pieces (num, lane, plant)
// because the banner prints all three. Pinned here so banner and table cell
// can never disagree, and so the suppressed/no-p50 cases stay verdicts with
// banner weight rather than reverting to grey footnotes.
console.log('annotationVerdict');
(function () {
    const ann = function (before, after, plant) {
        return m.annotationVerdict({
            p50_before: before, p50_after: after, plant_delta: plant,
            suppress_p50: false
        });
    };

    check('a lane that beats the plant is better, with the attributable number',
        (function () {
            const v = ann(0.60, 0.70, 0.01);
            return v.cls === 'lb-better' && v.word === 'better' &&
                Math.abs(v.num - 0.09) < 1e-9 &&
                Math.abs(v.lane - 0.10) < 1e-9 && Math.abs(v.plant - 0.01) < 1e-9;
        })(), 'lane +0.10, plant +0.01 → better +0.09');

    check('a lane that moves with the plant is neutral',
        ann(0.60, 0.70, 0.10).cls === 'lb-neutral',
        'lane +0.10, plant +0.10 → attributable 0');

    check('a lane that falls behind the plant is worse',
        ann(0.60, 0.62, 0.10).cls === 'lb-worse',
        'lane +0.02, plant +0.10 → attributable −0.08');

    check('the ±0.02 threshold applies to the attributable delta',
        ann(0.60, 0.60 + m.DELTA_SIGNIFICANT + 0.001, 0).cls === 'lb-better' &&
        ann(0.60, 0.60 + m.DELTA_SIGNIFICANT - 0.001, 0).cls === 'lb-neutral',
        'just over and just under, plant flat');

    check('a suppressed payload is a withheld verdict, not a missing one',
        (function () {
            const v = m.annotationVerdict({ suppress_p50: true, suppressed: 'miss rate moved' });
            return v.cls === 'lb-suppressed' && v.word === 'not comparable' &&
                v.num === null && v.plant === null;
        })(), 'suppress_p50 must still render a banner');

    check('a null p50 on one side is stated, not guessed',
        (function () {
            const v = ann(null, 0.70, 0.0);
            return v.cls === 'lb-neutral' && v.word === 'no p50 one side' &&
                v.num === null;
        })(), 'no before-average → no verdict number');

    check('a null plant delta withholds the classification, keeps the raw delta',
        (function () {
            const v = ann(0.60, 0.70, null);
            return v.cls === 'lb-neutral' && v.num === null &&
                Math.abs(v.lane - 0.10) < 1e-9 && v.plant === null;
        })(),
        'without a baseline nothing is attributable; the lane delta is data, not a verdict');

    check('a null payload is not a verdict',
        m.annotationVerdict(null) === null,
        'no annotation → no banner');
})();

// The greyscale channel: hue does not carry direction, so worse and
// suppressed must be distinguishable from better and neutral by something
// other than colour.
console.log('verdict marks');
(function () {
    check('worse is dashed and better is not',
        !!m.VERDICT_DASH.worse && !m.VERDICT_DASH.better,
        'the dash is the direction channel that survives desaturation');
    check('suppressed is dashed too',
        !!m.VERDICT_DASH.suppressed,
        'not-comparable must not read as a confident solid mark');
    check('every verdict has a token and a weight',
        ['better', 'worse', 'neutral', 'suppressed', 'thin', 'nodata'].every(function (v) {
            return m.VERDICT_TOKEN[v] && m.VERDICT_STROKE[v];
        }), JSON.stringify(m.VERDICT_TOKEN));
})();

// --- the lanes, as drawn ---------------------------------------------------
//
// The board's floor plan on the synthetic route scene (route-scene.js, the
// scene the robot map's pins draw). Pinned by U1 as the board drew it, then
// moved onto the robot map's lanes and lane shape with the same look (lead
// decision D15, 2026-11-14; every check that changed says so). Driven through
// createBoard, the page's own entry, with its edge and board fetches handed in
// (opts.fetchEdges and opts.fetchBoard) and no board loaded, so every lane
// reads "no data".

// A recording SVG element: tag, attributes (data-* mirrored into dataset, as
// a browser does), children, and the two lookups the board makes of its map.
function svgEl(name) {
    const n = {
        tag: name, attrs: {}, dataset: {}, children: [], parentNode: null, textContent: '',
        classList: { add: function () {}, remove: function () {} },
        setAttribute: function (k, v) {
            this.attrs[k] = String(v);
            if (k.indexOf('data-') === 0) {
                this.dataset[k.slice(5).replace(/-([a-z])/g, function (_, c) { return c.toUpperCase(); })] = String(v);
            }
        },
        getAttribute: function (k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; },
        appendChild: function (c) { c.parentNode = this; this.children.push(c); return c; },
        removeChild: function (c) { this.children.splice(this.children.indexOf(c), 1); c.parentNode = null; return c; },
        get firstChild() { return this.children[0] || null; },
        addEventListener: function () {},
        getBoundingClientRect: function () { return { left: 0, top: 0, width: 900, height: 560 }; },
        descendants: function () {
            const out = [];
            (function walk(x) { x.children.forEach(function (c) { out.push(c); walk(c); }); })(this);
            return out;
        },
        querySelector: function (sel) {
            const id = /^#(.+)$/.exec(sel);
            return id ? this.descendants().find(function (c) { return c.attrs.id === id[1]; }) || null : null;
        },
        querySelectorAll: function (sel) {
            const a = /^\[([\w-]+)\]$/.exec(sel);
            return a ? this.descendants().filter(function (c) { return a[1] in c.attrs; }) : [];
        },
    };
    return n;
}

// boardOn mounts a board over `rows` and loads it.
async function boardOn(rows) {
    const map = svgEl('svg');
    const els = { '#lb-map': map };
    const root = {
        set innerHTML(v) { /* the board's markup; its elements are the stand-ins below */ },
        querySelector: function (sel) {
            if (!els[sel]) {
                els[sel] = {
                    value: '', hidden: false, disabled: false, textContent: '', innerHTML: '', dataset: {},
                    options: [{ value: '' }], addEventListener: function () {},
                };
            }
            return els[sel];
        },
    };
    const board = m.createBoard(root, {
        fetchEdges: function () { return Promise.resolve(rows); },
        fetchBoard: function () { return Promise.resolve(null); },
    });
    await board.load();
    return { board: board, map: map };
}

async function lanePins() {
    console.log('lanes as drawn (synthetic route scene)');
    const RS = require('./route-scene.js');
    const b = await boardOn(RS.edgeRows());
    const rows = b.board._laneRows();
    // DECLARED CHANGE (lead decision D15, 2026-11-14): the lanes are the robot
    // map's, sceneNet(scene).lanes, in Core's aisle order (by vertex, in the
    // order the rows first name them). It was the first ROW per lane in row
    // order: LM911_SMN_05, LM911_LM912, LM912_PLN_02, LM932_PLN_02,
    // LM931_LM932, LM931_SMN_05, LM921_SMN_05, LM921_PLN_02, LM941_LM942.
    check('15 stored rows are 9 lanes, the one-way ones included, keyed by scene-geom\'s laneKey, in the robot map\'s aisle order (D15)',
        JSON.stringify(rows.map(function (r) { return r.key; })) === JSON.stringify([
            'LM911_SMN_05', 'LM931_SMN_05', 'LM921_SMN_05', 'LM911_LM912', 'LM912_PLN_02', 'LM932_PLN_02',
            'LM921_PLN_02', 'LM931_LM932', 'LM941_LM942']),
        JSON.stringify(rows.map(function (r) { return r.key; })));
    const twin = rows.find(function (r) { return r.key === 'LM921_PLN_02'; });
    check('a lane takes the handles of the FIRST row stored between its ends (PLN_02>LM921, listed before its twin)',
        twin && twin.lane.from === 'PLN_02' && twin.lane.to === 'LM921' && JSON.stringify(twin.lane.h) === '[15,-4,9,-4]',
        twin && JSON.stringify(twin.lane));
    check('no board loaded: no lane has state', rows.every(function (r) { return r.st === null; }));
    check('orientation comes from the edges\' endpoints: this scene is wider than tall, unrotated',
        b.board._state.rotate90 === false);

    const world = b.map.children[0];
    const zones = world.children[0], lanes = world.children[1];
    check('the map draws one world group: an empty zones group, then the lanes (no board: no reflectors)',
        b.map.children.length === 1 && world.attrs.id === 'lb-world' && world.children.length === 2 &&
        zones.attrs['class'] === 'lb-zones' && zones.children.length === 0 && lanes.attrs['class'] === 'lb-lanes',
        world.children.map(function (c) { return c.attrs['class']; }).join(','));
    // DECLARED CHANGE (D15): a lane is drawn in the robot map's lane shape
    // (scene-geom.js laneSpec): a cubic <path> through all four handles, a
    // <line> for a straight lane. A straight lane was a <path d="M..L..">,
    // which paints the same.
    const shapeOf = function (p) {
        return p.tag === 'line' ? 'line ' + [p.attrs.x1, p.attrs.y1, p.attrs.x2, p.attrs.y2].join(',') : 'path ' + p.attrs.d;
    };
    check('two elements per lane, the click target first, then the mark', lanes.children.length === 18 &&
        lanes.children.every(function (p, i) { return p.attrs['class'] === (i % 2 ? 'lb-lane' : 'lb-lane-hit'); }),
        lanes.children.map(function (p) { return p.attrs['class']; }).join(','));
    const marks = lanes.children.filter(function (p, i) { return i % 2 === 1; });
    // NOT CHANGED: each lane runs as its first stored row does, as the board
    // always drew it. The shared lanes run from their earlier vertex; on this
    // scene that turns SMN_05-LM931 round (stored one way only, LM931>SMN_05),
    // and a dashed mark drawn the other way lays its dashes elsewhere, which
    // the before/after shots showed (laneRows). So it is drawn 2,-1 -> 0,0 still.
    const want = ['line 0,0,0,-2', 'line 2,-1,0,0', 'path M0 0C1 4 7 4 8 0', 'line 0,-2,16,-2', 'line 16,-2,16,0',
        'line 16,0,14,-1', 'path M16 0C15 4 9 4 8 0', 'line 14,-1,2,-1', 'line 30,-10,34,-10'];
    check('each lane is drawn in the robot map\'s lane shape: a straight one as a <line>, a curved one as the projected cubic (D15)',
        JSON.stringify(marks.map(shapeOf)) === JSON.stringify(want), JSON.stringify(marks.map(shapeOf)));
    const scale = b.board._state.scale;
    check('a mark: unfilled where it is a path, round-capped, no-data grey, 1.3 px held at screen size, keyed by its lane',
        marks.every(function (p, i) {
            return (p.tag === 'line' || p.attrs.fill === 'none') && p.attrs['stroke-linecap'] === 'round' &&
                p.attrs.stroke === 'var(--text-muted)' && p.attrs.opacity === '1' && p.attrs['data-w'] === '1.3' &&
                p.attrs['stroke-width'] === (1.3 / scale).toFixed(4) && p.attrs['data-lane'] === rows[i].key;
        }), marks.map(function (p) { return JSON.stringify(p.attrs); })[0]);
    check('a click target: the same shape, transparent, 14 px, hit on its stroke',
        lanes.children.filter(function (p, i) { return i % 2 === 0; }).every(function (p, i) {
            return shapeOf(p) === shapeOf(marks[i]) && p.attrs.stroke === 'transparent' && p.attrs['data-w'] === '14' &&
                p.attrs['pointer-events'] === 'stroke' && p.attrs['data-lane'] === rows[i].key;
        }));

    // A curved lane turned round: its first stored row runs LM921>SMN_05, from
    // the later vertex (SMN_05 is named first), so the shared lane runs
    // SMN_05>LM921 with its handles turned; the board draws it as the row, the
    // same cubic it always drew (M8 0C7 4 1 4 0 0).
    const rs = RS.edgeRows();
    const cb = await boardOn([rs[0], rs[10]]);
    const cm = cb.map.children[0].children[1].children;
    check('a curved lane whose first stored row runs from its later vertex is drawn as that row, handles in its order',
        shapeOf(cm[3]) === 'path M8 0C7 4 1 4 0 0', cm.map(shapeOf).join(' | '));

    // DECLARED CHANGE (D15): handles are all four or none, as the robot map
    // reads them (scene-rows.js sceneOfRows, isCoord). The board's own curve
    // test read ctrl1_x and ctrl2_x only, so a row carrying those two and no y
    // drew a cubic with NaN handles (M0 0C1 NaN 7 NaN 8 0). /api/map/edges
    // never sends one (Core writes the four handles together); such a row is
    // now a straight lane, as on the robot map.
    const half = Object.assign({}, RS.edgeRows()[9]);
    delete half.ctrl1_y; delete half.ctrl2_y;
    const hb = await boardOn([half]);
    const hm = hb.map.children[0].children[1].children[1];
    check('a row with ctrl1_x and ctrl2_x but no y is a straight lane, not a cubic with NaN handles (D15)',
        shapeOf(hm) === 'line 0,0,8,0', shapeOf(hm));

    const tall = RS.edgeRows().map(function (r) {
        return Object.assign({}, r, { from_x: r.from_y, from_y: r.from_x, to_x: r.to_y, to_y: r.to_x });
    });
    const tb = await boardOn(tall);
    check('the same edges transposed (taller than wide) draw rotated', tb.board._state.rotate90 === true);
}

(async function () {
    try {
        await lanePins();
    } catch (e) {
        failures++;
        console.log('  FAIL the lane pins threw\n       ' + (e && e.stack || e));
    }
    if (failures) {
        console.error('\n' + failures + ' check(s) failed');
        process.exit(1);
    }
    console.log('\nall checks passed');
})();
