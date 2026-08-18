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

if (failures) {
    console.error('\n' + failures + ' check(s) failed');
    process.exit(1);
}
console.log('\nall checks passed');
