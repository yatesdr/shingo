// localization-board.test.js — the board's pure logic, run under node.
//
// The DOM half (viewport, drawing, rail) is exercised by hand against the real
// page; what is asserted here is the part that would be WRONG SILENTLY: the key
// the client joins state to geometry with, and the shape of the distribution
// the panel draws.

const fs = require('fs');
const path = require('path');
const vm = require('vm');

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
    const file = path.join(__dirname, '..', 'components', 'localization-board.js');
    const raw = fs.readFileSync(file, 'utf8');
    const stripped = raw.replace(/^import[^;]+;\s*/mg, '');
    if (stripped === raw) {
        throw new Error('localization-board.js no longer starts its imports at line start, ' +
            'which this harness strips; update load() in localization-board.test.js');
    }
    const src = stripped.replace(/^export /mg, '');
    const ctx = { console: console, Math: Math, Number: Number, Map: Map, Set: Set, Date: Date };
    vm.createContext(ctx);
    vm.runInContext(src + '\n__out = { histPath: histPath, serverLaneKey: serverLaneKey, ' +
        'deltaVerdict: deltaVerdict, DELTA_SIGNIFICANT: DELTA_SIGNIFICANT, ' +
        'VERDICT_TOKEN: VERDICT_TOKEN, VERDICT_STROKE: VERDICT_STROKE, ' +
        'VERDICT_DASH: VERDICT_DASH, ' +
        'BAND_STROKE: BAND_STROKE, BAND_TOKEN: BAND_TOKEN };', ctx);
    return ctx.__out;
}

const m = load();

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

if (failures) {
    console.error('\n' + failures + ' check(s) failed');
    process.exit(1);
}
console.log('\nall checks passed');
