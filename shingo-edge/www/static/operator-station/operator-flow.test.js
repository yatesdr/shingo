// operator-flow.test.js — the read-only cell picture, rendered from station
// views the Go side built over the Hopkinsville pull (operator_flow_test.go
// writes them and passes their paths). What is asserted is what the operator
// would see wrong: the legs of each choreography, their labels, the dock
// notes and the bin word, the caption, and — as a COORDINATE check over
// every pair — that no two cards collide.
//
// operator-flow.js imports shared/scene-geom.js and the station's util/state
// modules. vm runs a script: scene-geom's exports are stripped to plain
// declarations and evaluated first, then the flow module with its imports
// stripped and the two local helpers it uses stubbed. Exit 0 = pass.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let failures = 0;
function check(name, cond, detail) {
    if (cond) { console.log('  ok   ' + name); return; }
    failures++;
    console.log('  FAIL ' + name + (detail ? '\n       ' + detail : ''));
}

function load() {
    const ctx = {
        console: console, Math: Math, Set: Set, Number: Number, isFinite: isFinite, JSON: JSON, Object: Object, Array: Array,
        document: { getElementById() { return null; }, createElementNS() { return { setAttribute() {} }; }, createTextNode() { return {}; }, body: { appendChild() {} } },
        window: { location: { hash: '' } },
        el(tag, props) { return Object.assign({ appendChild() {}, addEventListener() {}, setAttribute() {} }, props || {}); },
        esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); },
        getView() { return null; },
    };
    ctx.module = undefined;
    vm.createContext(ctx);
    // THE MODEL, IN THE SAME CONTEXT. operator-flow.js computes no sentence of
    // its own any more: the card lines, the dock notes and which legs exist
    // all come from composer-model.js, so a harness without it would be
    // rendering a picture no station ever draws. The file hangs its api on
    // window, which is where sentencesFromView looks.
    const modelSrc = fs.readFileSync(path.join(__dirname, 'composer-model.js'), 'utf8');
    vm.runInContext(modelSrc, ctx);
    if (!ctx.window.ComposerModel) throw new Error('composer-model.js did not attach to window; update load()');
    const geomFile = path.join(__dirname, '..', '..', '..', '..', 'shared', 'scene-geom.js');
    const geomRaw = fs.readFileSync(geomFile, 'utf8');
    const geomSrc = geomRaw.replace(/^export /mg, '');
    if (geomSrc === geomRaw) throw new Error('shared/scene-geom.js no longer declares its exports at line start; update load()');
    vm.runInContext(geomSrc, ctx);
    const raw = fs.readFileSync(path.join(__dirname, 'operator-flow.js'), 'utf8');
    const src = raw.replace(/^import[^;]+;\s*/mg, '').replace(/^export /mg, '');
    if (src === raw) throw new Error('operator-flow.js no longer starts its imports/exports at line start; update load()');
    vm.runInContext(src + '\n__out = { renderFlowPicture, layoutPositions, layoutStaging, legsFor, sentencesFromView, sentencesFromModel, CARD_W, CARD_H };', ctx);
    return ctx.__out;
}

const m = load();
const [style7Path, style11Path, schematicPath] = process.argv.slice(2);
if (!style7Path || !style11Path || !schematicPath) {
    console.log('usage: node operator-flow.test.js <style7.json> <style11.json> <schematic.json>');
    process.exit(2);
}
const view7 = JSON.parse(fs.readFileSync(style7Path, 'utf8'));
const view11 = JSON.parse(fs.readFileSync(style11Path, 'utf8'));
const viewSchematic = JSON.parse(fs.readFileSync(schematicPath, 'utf8'));

function count(svg, re) { return (svg.match(re) || []).length; }

// cardsOf reads every card's box back off the markup — the same numbers
// the browser lays out — so the overlap check is on what is drawn.
function cardsOf(svg) {
    const out = [];
    const re = /<g class="node[^"]*" data-pos="([^"]+)" transform="translate\(([-\d.]+),([-\d.]+)\)"><rect class="box" width="([\d.]+)" height="([\d.]+)"/g;
    let mm;
    while ((mm = re.exec(svg))) out.push({ name: mm[1], x: +mm[2], y: +mm[3], w: +mm[4], h: +mm[5] });
    return out;
}
function overlaps(a, b) { return a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h; }
function noCollisions(label, svg) {
    const cards = cardsOf(svg);
    check(label + ': six cards drawn', cards.length === 6, String(cards.length));
    for (let i = 0; i < cards.length; i++) {
        for (let j = i + 1; j < cards.length; j++) {
            check(label + ': ' + cards[i].name + ' and ' + cards[j].name + ' do not collide', !overlaps(cards[i], cards[j]),
                JSON.stringify(cards[i]) + ' vs ' + JSON.stringify(cards[j]));
        }
    }
    for (const c of cards) {
        check(label + ': ' + c.name + ' inside the picture', c.x >= 0 && c.y >= 0 && c.x + c.w <= 1280 && c.y + c.h <= 470, JSON.stringify(c));
    }
    return cards;
}

console.log('style 7 — PART SYN-A-S003, two index pairs');
{
    const svg = m.renderFlowPicture(view7);
    check('two Robot 2 index legs', count(svg, /class="leg thin r2"/g) === 2, String(count(svg, /class="leg thin r2"/g)));
    check('no Robot 1 leg', count(svg, /class="leg thin r1"/g) === 0);
    check('"Robot 2 indexes" under each pair', count(svg, /class="leg-lbl r2"[^>]*>Robot 2 indexes</g) === 2, String(count(svg, /class="leg-lbl r2"[^>]*>Robot 2 indexes</g)));
    check('dock IN note names the on-deck positions', svg.includes('Inbound source · Robot 1 → PLN_02, PLN_05'), svg.match(/Robot 1 brings[^<]*/) && svg.match(/Robot 1 brings[^<]*/)[0]);
    check('dock OUT note names the front positions', svg.includes('Outbound destination · Robot 2 ← PLN_01, PLN_04'), svg.match(/Robot 2 takes[^<]*/) && svg.match(/Robot 2 takes[^<]*/)[0]);
    check('dock groups', svg.includes('>Supermarket Empty Totes<') && svg.includes('>Supermarket Area<'));
    check('dock members', svg.includes('SMN_05, SMN_06, SMN_07, SMN_08') && svg.includes('SMN_04, SMN_01, SMN_02, SMN_03') || svg.includes('SMN_01, SMN_02, SMN_03, SMN_04'),
        (svg.match(/SMN_[^<]*/g) || []).join(' | '));
    check('caption says true spacing', svg.includes('SCREEN A4 · positions at true spacing'),
        (svg.match(/>[A-Z0-9 ]+ · positions[^<]*/) || [''])[0]);
    check('row labels once', count(svg, />FRONT · LINE SIDE</g) === 1 && count(svg, />BACK</g) === 1);
    check('PLN_01 card line', svg.includes('>Robot 1 supplies PLN_02<') && count(svg, /class="ln"[^>]*>Robot 2 indexes</g) === 2);
    check('PLN_02 on deck', svg.includes('>on deck for PLN_01<') && svg.includes('>on deck for PLN_04<'));
    check('unused PLN_03/PLN_06 are front positions', count(svg, />front position</g) === 2 && count(svg, />back position</g) === 0);
    check('part chips', svg.includes('>SYN-A-P002<') && svg.includes('>SYN-A-P003<'));
    check('no R1/R2 abbreviation anywhere an operator reads', !/>[^<]*\bR[12]\b[^<]*</.test(svg));
    // THIS PIN READ "no arrowheads" AND THE PICTURE HAS THEM NOW (owner,
    // 2026-09-16: the legs "show flows with the purple or teal arrows, it might
    // help if they were like live, showing the arrows going in or out"). The
    // half of it worth keeping is the `marker` half, so it is spelled out
    // rather than deleted: a <marker> is referenced by url(#id) and resolves
    // over the whole DOCUMENT, and the composer draws a second copy of this
    // picture beside the first — two markers of one name is one marker,
    // whichever rendered last, and one picture's arrowheads would take the
    // other's hue. legsFor places and rotates a plain path instead.
    check('one direction chevron per leg, and not one of them a marker',
        count(svg, /class="legtip r[12]"/g) === 2 && !svg.includes('marker'),
        count(svg, /class="legtip r[12]"/g) + ' chevrons');
    // And the travelling dashes are a SECOND path over each leg rather than a
    // dash pattern on the leg, which is what keeps the leg's own width, hue and
    // z-order out of the animation's hands — and what lets reduced motion
    // switch the whole thing off by hiding one element.
    check('the flow dashes are an overlay, not the leg itself',
        count(svg, /class="legflow r[12]"/g) === 2, String(count(svg, /class="legflow r[12]"/g)));
    const cards = noCollisions('style 7', svg);
    const p1 = cards.find(c => c.name === 'PLN_01'), p2 = cards.find(c => c.name === 'PLN_02'), p5 = cards.find(c => c.name === 'PLN_05');
    // True relative spacing at the reference's 120 px/m: PLN_01 to PLN_02 is
    // 1.772 m = 212.6 px centre to centre, and PLN_05 is at one end of the row.
    //
    // DISTANCE, NOT DIRECTION. These were signed — `p2.x - p1.x` positive and
    // "PLN_05 is leftmost" — which is the plant's HANDEDNESS, not a layout
    // rule: the fixture's map is the plant's under a mirror, so both flipped
    // and neither had anything to say about the picture. What the picture owes
    // is the SPACING and that the outermost card is outermost.
    check('PLN_01 to PLN_02 is 1.772 m at 120 px/m', p1 && p2 && Math.abs(Math.abs(p2.x - p1.x) - 1.772 * 120) < 0.5, p1 && p2 && String(p2.x - p1.x));
    check('PLN_05 is at one end of the row', p5 && (cards.every(c => c.x >= p5.x) || cards.every(c => c.x <= p5.x)));
}

console.log('style 11 — PART SYN-A-S007, two staging moves');
{
    const svg = m.renderFlowPicture(view11);
    check('two Robot 1 staging legs', count(svg, /class="leg thin r1"/g) === 2, String(count(svg, /class="leg thin r1"/g)));
    check('no Robot 2 leg', count(svg, /class="leg thin r2"/g) === 0);
    check('"Robot 1 moves in" on each', count(svg, />Robot 1 moves in</g) === 2);
    // THE SAME SENTENCE AS STYLE 7'S, and that is the pin. It used to read
    // `new bins here` where style 7 read `new totes here`, from the payload's
    // bin type — owner ruling R2 (2026-09-12) removed the word, so the two
    // styles' dock notes now differ only in the positions they name.
    check('the dock note names the field and the positions it drives',
        svg.includes('Inbound source · Robot 1 → PLN_02, PLN_05') && svg.includes('Outbound destination · Robot 2 ← PLN_03, PLN_06'),
        (svg.match(/(Inbound source|Outbound destination)[^<]*/g) || []).join(' | '));
    check('no bin word survives on the picture', !/tote|Tote/.test(svg.replace(/Supermarket Empty Totes/g, '')),
        (svg.match(/[^<>]*[Tt]ote[^<>]*/g) || []).join(' | '));
    // WHICH STAGING, NOT JUST WHOSE (owner, 2026-09-16). Both directions read
    // "staging for PLN_03", so the card could not tell an engineer whether that
    // slot takes the new bin in or the old one out — the difference between the
    // two trips drawn on the picture around it. The word is the claim's own
    // label (F3, via fieldWord), so the card and the control that set it are
    // one name for one field.
    check('staging cards say which staging, and whose',
        svg.includes('>Inbound staging<') && svg.includes('>for PLN_03<') &&
        svg.includes('>for PLN_06<'),
        (svg.match(/>(Inbound|Outbound) staging<|>for PLN_[0-9]+</g) || []).join(' | '));
    check('PLN_03 card line', svg.includes('>Robot 1 stages at PLN_02<') && svg.includes('>Robot 2 pulls old<'));
    check('unused PLN_01/PLN_04 are still front positions (the press did not change shape)', count(svg, />front position</g) === 2 && count(svg, />back position</g) === 0,
        (svg.match(/>(front|back) position</g) || []).join(' '));
    check('row labels: line side on top, back below', count(svg, />FRONT · LINE SIDE</g) === 1 && count(svg, />BACK</g) === 1);
    noCollisions('style 11', svg);
}

console.log('schematic — no geometry cached');
{
    const svg = m.renderFlowPicture(viewSchematic);
    check('caption says not to scale', svg.includes('positions not to scale'), svg.match(/PRESS 400[^<]*/) && svg.match(/PRESS 400[^<]*/)[0]);
    check('still draws the legs', count(svg, /class="leg thin r2"/g) === 2);
    noCollisions('schematic', svg);
}

console.log('empty cell');
{
    const svg = m.renderFlowPicture({ station: { name: 'X' }, cell: { positions: [], geometry: false } });
    check('never a blank panel', svg.includes('No positions on this station yet'));
}

// V1 (2026-09-12). At the desktop's RESTING frame the dock strip ran off the
// bottom of the picture and cut its member line — `SMN_05, SMN_06, SMN_07,
// SMN_08` — through the middle. The dock sat at a PROPORTION of the station
// frame's height (470/560), and a proportion does not know that the four text
// rows below the rule are a fixed 64 px whatever the frame does.
//
// So the dock is laid out from the frame's FOOT when the proportion would put
// it too low, and this measures both ends of that: the station's y is exactly
// where it was, and at 304 every row of the dock is inside the picture.
console.log('the dock is laid out from the foot, and never clipped');
{
    // Every <text> the dock draws, as an absolute y: the group is translated
    // to DOCK_Y and its rows sit at 30, 36, 48 and 64 inside it.
    const dockRows = svg => {
        const y = Number((svg.match(/<line x1="\d+" y1="(\d+)"/) || [])[1]);
        const out = [];
        const g = svg.slice(svg.indexOf('<g class="dock">'));
        for (const m of g.matchAll(/<text class="[ks]"[^>]*y="(\d+)"/g)) out.push(y + Number(m[1]));
        return { ruleY: y, rows: out, deepest: Math.max(...out) };
    };

    const station = dockRows(m.renderFlowPicture(view7));
    check('the station dock is exactly where it was: rule at 470',
        station.ruleY === 470, 'rule at ' + station.ruleY);
    check('and every row of it is inside 560', station.deepest < 560,
        'deepest row at ' + station.deepest + ' of 560');

    // The desktop's resting frame after P3 gave the positions box its floor.
    const short = dockRows(m.renderFlowPicture(view7, { frame: { w: 1074, h: 304 } }));
    check('at a 304 frame the dock moves up rather than off the bottom',
        short.ruleY < 304 && short.deepest < 304,
        'rule at ' + short.ruleY + ', deepest row at ' + short.deepest + ' of 304');
    check('the member line is whole, not clipped by a few pixels',
        304 - short.deepest >= 8, '304 - ' + short.deepest + ' = ' + (304 - short.deepest) + ' px of room');
    check('the rows are still the dock, not a squeezed copy of it',
        short.rows.length === station.rows.length,
        short.rows.length + ' rows at 304, ' + station.rows.length + ' at 560');

    // AND THE CARDS GIVE WAY TO IT. Both placements centre their rows on
    // CENTER_Y, a proportion of the station frame's height, while a card is a
    // fixed 92 tall — so at 304 the second row reached past where the dock now
    // is. liftAboveDock slides the block up by exactly the overlap.
    //
    // (This view is the SCHEMATIC at 304 and was before this change too: to
    // scale stops below 360 for the Hopkinsville spread, measured. Nothing
    // here moved that line.)
    const shortSvg = m.renderFlowPicture(view7, { frame: { w: 1074, h: 304 } });
    for (const c of cardsOf(shortSvg)) {
        check('short-frame card ' + c.name + ' is above the dock rule', c.y + c.h <= short.ruleY,
            JSON.stringify(c) + ' vs rule ' + short.ruleY);
    }
    noCollisions('304 frame', shortSvg);
}

console.log('the frame — the station keeps its own, the desktop asks for the column it has');
{
    // WHY A FRAME AT ALL. The picture is laid out in absolute user units and
    // the caller sets the viewBox to match, so a card is CARD_W px wide on
    // screen only when the viewBox is the element's real width. The desktop's
    // main column is 1084 px at the spec's 1440; drawn into the station's
    // 1280x560 viewBox inside a 330 px frame, preserveAspectRatio fitted the
    // whole drawing at 0.59 and every card came out 111 px wide with 9 px
    // titles. The fix is not a scale factor, it is telling the renderer how
    // big the picture actually is.
    const station = m.renderFlowPicture(view7);
    check('the station frame is untouched: dock line spans 150..1130 at y 470',
        station.includes('x1="150" y1="470" x2="1130" y2="470"'),
        (station.match(/<line x1="[^"]*" y1="[^"]*" x2="[^"]*" y2="[^"]*"/) || [''])[0]);
    for (const c of cardsOf(station)) {
        check('station card ' + c.name + ' is ' + m.CARD_W + 'x' + m.CARD_H, c.w === m.CARD_W && c.h === m.CARD_H, JSON.stringify(c));
    }

    const desk = m.renderFlowPicture(view7, { frame: { w: 1084, h: 430 } });
    const cards = cardsOf(desk);
    check('desktop frame still draws six cards', cards.length === 6, String(cards.length));
    for (const c of cards) {
        // THE POINT OF THE WHOLE CHANGE: a card is the same size in the
        // desktop's frame as in the station's, because both are 1:1 with the
        // element they are drawn into.
        check('desktop card ' + c.name + ' is full size', c.w === m.CARD_W && c.h === m.CARD_H, JSON.stringify(c));
        check('desktop card ' + c.name + ' inside the 1084x430 frame',
            c.x >= 0 && c.y >= 0 && c.x + c.w <= 1084 && c.y + c.h <= 430, JSON.stringify(c));
    }
    for (let i = 0; i < cards.length; i++) {
        for (let j = i + 1; j < cards.length; j++) {
            check('desktop ' + cards[i].name + ' and ' + cards[j].name + ' do not collide', !overlaps(cards[i], cards[j]),
                JSON.stringify(cards[i]) + ' vs ' + JSON.stringify(cards[j]));
        }
    }
    // THE GUTTER IS THE RULE, NOT THE COORDINATE. These two used to assert
    // x2="934" and y1="3\d\d" — the serialised numbers for a 1084x430 frame,
    // which change whenever the desktop column does and say nothing about what
    // is supposed to hold. What holds is that the dock line is inset by the
    // same gutter as the station's, on both ends, whatever the frame is, and
    // that it sits inside it.
    const dockOf = svg => {
        const m2 = svg.match(/<line x1="([\d.]+)" y1="([\d.]+)" x2="([\d.]+)" y2="([\d.]+)"/);
        return m2 && { x1: +m2[1], y1: +m2[2], x2: +m2[3], y2: +m2[4] };
    };
    const sd = dockOf(station), dd = dockOf(desk);
    check('desktop dock starts at the same gutter as the station', sd && dd && dd.x1 === sd.x1,
        JSON.stringify({ station: sd, desktop: dd }));
    check('desktop dock ends a gutter short of its frame',
        sd && dd && (1084 - dd.x2) === (1280 - sd.x2),
        sd && dd && JSON.stringify({ deskRightGutter: 1084 - dd.x2, stationRightGutter: 1280 - sd.x2 }));
    check('desktop dock sits inside the 430 frame', dd && dd.y1 > 0 && dd.y1 < 430, dd && String(dd.y1));
    check('desktop still says true spacing', desk.includes('positions at true spacing'));
    check('desktop still labels both rows', count(desk, />FRONT · LINE SIDE</g) === 1 && count(desk, />BACK</g) === 1);
}

// ── two routes at once, the short frames, and more LMs than fit ─────────────
//
// SYNTHETIC. A two-row cell where BOTH front positions stage on a back card and
// BOTH carry a key route — the case the first cut could not draw at all,
// because it ran every strip from the dock's IN glyph and so stacked them in
// one corner of the picture.
{
    const two = () => ({
        geometry: true,
        positions: [
            { core_node_name: 'PLN_03', sequence: 1, kind: 'front', x: 0, y: 0,
              claim: { swap_mode: 'two_robot', payload_code: 'SYN-A-P010', inbound_staging: 'PLN_02',
                       inbound_source: 'SMN_IN', outbound_destination: 'SMN_OUT', key_route: ['LM10', 'LM11'] } },
            { core_node_name: 'PLN_06', sequence: 2, kind: 'front', x: 3, y: 0,
              claim: { swap_mode: 'two_robot', payload_code: 'SYN-A-P011', inbound_staging: 'PLN_05',
                       inbound_source: 'SMN_IN', outbound_destination: 'SMN_OUT', key_route: ['LM20', 'LM21'] } },
            { core_node_name: 'PLN_02', sequence: 3, kind: 'back', x: 0, y: -1.8 },
            { core_node_name: 'PLN_05', sequence: 4, kind: 'back', x: 3, y: -1.8 },
        ],
        staging: [],
    });
    const svg = m.renderFlowPicture({ cell: two(), station: { name: 'SCREEN' } }, {});

    check('two routes: two strips', count(svg, /<g class="lmroute /g) === 2,
        String(count(svg, /<g class="lmroute /g)));
    check('two routes: two chevrons', count(svg, /<path class="lmtip"/g) === 2);

    // EACH UNDER ITS OWN CARD, and that is the whole reason the drawing moved:
    // the two lines sit on the two arriving cards' centres, which are far apart,
    // so neither strip can be read as the other's.
    const boxes = m.layoutPositions(two(), undefined).boxes;
    const lineXs = [...svg.matchAll(/<path class="lmline" d="M([-\d.]+) /g)].map(mm => +mm[1]).sort((a, b) => a - b);
    const want = [boxes.PLN_02, boxes.PLN_05].map(b => b.x + b.w / 2).sort((a, b) => a - b);
    check('two routes: one under each arriving card',
        lineXs.length === 2 && lineXs.every((x, i) => Math.abs(x - want[i]) < 0.5),
        JSON.stringify({ lineXs: lineXs, cardCentres: want }));
    check('two routes: they do not share a corner', lineXs.length === 2 && Math.abs(lineXs[1] - lineXs[0]) > 100,
        JSON.stringify(lineXs));
    check('two routes: each names its own points',
        svg.includes('>1 · LM10<') && svg.includes('>2 · LM11<') &&
        svg.includes('>1 · LM20<') && svg.includes('>2 · LM21<'));

    // ROOM IS MADE FOR IT. liftAboveDock reserves the strip's height under the
    // lowest row the same way it reserves the caption's, so the strip never
    // crosses the dock's rule and the cards never leave the frame.
    const dockY = +svg.match(/<line x1="[\d.]+" y1="([\d.]+)"/)[1];
    const feet = [...svg.matchAll(/<path class="lmline" d="M[-\d.]+ ([-\d.]+) V/g)].map(mm => +mm[1]);
    check('two routes: every strip stops above the dock rule',
        feet.length === 2 && feet.every(y => y < dockY), JSON.stringify({ feet: feet, dockY: dockY }));
    // THE CAPTION IS NOT IN THE STRIP'S BAND. It is the other left-anchored
    // line under the last row, and the first shot of this drawing had the two
    // printed over each other.
    check('two routes: the station caption clears every strip', (() => {
        const cap = svg.match(/<text class="mlbl" x="[\d.]+" y="([\d.]+)"[^>]*>SCREEN/);
        if (!cap) return false;
        return feet.every(y => y < +cap[1] - 8);
    })(), JSON.stringify({ feet: feet, caption: (svg.match(/<text class="mlbl" x="[\d.]+" y="([\d.]+)"[^>]*>SCREEN/) || [])[1] }));
    check('two routes: no card is pushed out of the frame',
        Object.keys(boxes).every(n => boxes[n].y >= 20), JSON.stringify(Object.keys(boxes).map(n => boxes[n].y)));

    // FOUR MUST FIT AT THE STATION FRAME. That is the sizing rule the tokens
    // were chosen against; a fifth is reported rather than drawn.
    const four = two();
    four.positions[0].claim.key_route = ['LM10', 'LM11', 'LM12', 'LM13'];
    const svg4 = m.renderFlowPicture({ cell: four, station: { name: 'SCREEN' } }, {});
    check('four LMs fit at the station frame',
        ['1 · LM10', '2 · LM11', '3 · LM12', '4 · LM13'].every(t => svg4.includes('>' + t + '<')) &&
        !/lmmore/.test(svg4),
        svg4.match(/<text class="lmlbl"[^>]*>[^<]*/g));

    // MORE THAN FITS: the first three, then "+N" in the last slot. Never
    // smaller type, and never a silently shortened route.
    const six = two();
    six.positions[0].claim.key_route = ['LM10', 'LM11', 'LM12', 'LM13', 'LM14', 'LM15'];
    const svg6 = m.renderFlowPicture({ cell: six, station: { name: 'SCREEN' } }, {});
    check('overflow: the first three are named',
        ['1 · LM10', '2 · LM11', '3 · LM12'].every(t => svg6.includes('>' + t + '<')));
    check('overflow: the rest are counted, not dropped', /<text class="lmmore"[^>]*>\+3<\/text>/.test(svg6),
        svg6.match(/<text class="lmmore"[^>]*>[^<]*/g));
    check('overflow: nothing past the count is drawn',
        !svg6.includes('LM13') && !svg6.includes('LM14') && !svg6.includes('LM15'));

    // THE DESKTOP'S FRAMES. The composer draws the same picture in a shorter
    // box, and liftAboveDock is what keeps the strip inside it.
    // 430 IS THE PICTURE BOX'S CSS BASIS (.pd-pic is `flex: 0 1 430px`), which
    // it gets when the page is tall enough to give it. The frame it actually
    // measures at 1440x900 is shorter and is pinned below.
    const desk = m.renderFlowPicture({ cell: two(), station: { name: 'SCREEN' } }, { frame: { w: 1084, h: 430 } });
    const deskDock = +desk.match(/<line x1="[\d.]+" y1="([\d.]+)"/)[1];
    const deskFeet = [...desk.matchAll(/<path class="lmline" d="M[-\d.]+ ([-\d.]+) V/g)].map(mm => +mm[1]);
    check('desktop 430: both strips are drawn', count(desk, /<g class="lmroute /g) === 2,
        String(count(desk, /<g class="lmroute /g)));
    check('desktop 430: both stay above the dock rule',
        deskFeet.length === 2 && deskFeet.every(y => y < deskDock), JSON.stringify({ deskFeet: deskFeet, deskDock: deskDock }));
    // ONE ROW IS ALL 430 HOLDS, and the count rides it. The composer's frame
    // puts its cards against the picture's top inset, so liftAboveDock has
    // nothing left to give and the band under the last row takes one waypoint.
    // Naming the first and counting the rest is the "+N" rule at its limit; a
    // strip that silently showed one point of three would be the lie.
    check('desktop 430: the first point is named and the rest counted',
        /<text class="lmlbl"[^>]*>1 · LM10 \+1<\/text>/.test(desk),
        desk.match(/<text class="lm(lbl|more)"[^>]*>[^<]*/g));

    // THE DESKTOP'S OWN FRAME DRAWS NO STRIP, and 1074x304 is not a guess: it
    // is what the Processes page measures at 1440x900, logged by the shot
    // harness ("picture 1074x304"). At that height the dock band and two rows
    // of cards take the picture between them — the cards are already against
    // the top inset, so liftAboveDock has nothing to give and 14 units are left
    // under the lowest row where one waypoint row needs 72.
    //
    // NOTHING, rather than a chevron with no waypoint under it or a line across
    // the dock's rule. The desktop is also the surface that does not need it:
    // its positions table carries a KEY ROUTE column, which the board has no
    // room for and which is why the strip exists.
    const desktopReal = m.renderFlowPicture({ cell: two(), station: { name: 'SCREEN' } }, { frame: { w: 1074, h: 304 } });
    check('desktop 1074x304 (the measured frame): no strip rather than a broken one',
        !/<g class="lmroute /.test(desktopReal) && !/class="lmdot"/.test(desktopReal) && !/class="lmtip"/.test(desktopReal),
        desktopReal.match(/<g class="lmroute[^>]*/g));
    // AND IT IS THE PICTURE IT ALWAYS WAS. Reserving room for a strip that then
    // cannot be drawn would have moved the cards for nothing; liftAboveDock
    // takes the caption's lift instead where the strip will not fit.
    check('desktop 1074x304: the cards are where they were before the strip existed', (() => {
        const withRoute = m.layoutPositions(two(), { w: 1074, h: 304 }).boxes;
        const plain = two();
        for (const p of plain.positions) { if (p.claim) p.claim.key_route = []; }
        const without = m.layoutPositions(plain, { w: 1074, h: 304 }).boxes;
        return Object.keys(withRoute).every(n => withRoute[n].y === without[n].y);
    })());
}

// ── the staging band, the legs it grew, and the LM marks ─────────────────────
//
// SYNTHETIC, not one of the three plant views. The three views above are the
// Hopkinsville pull, where every staging slot IS a position of the cell
// (PLN_02/PLN_05 park on their own back slots) and every LM sits on the aisle
// rather than on a 1.8 m leg between two adjacent cards — so that plant shows
// neither feature, correctly. This is the cell that does.
{
    const cell = {
        geometry: true,
        positions: [
            { core_node_name: 'PLN_01', sequence: 1, kind: 'front', x: 0, y: 0,
              claim: { swap_mode: 'single_robot', payload_code: 'PART-A',
                       inbound_staging: 'SLN_07', outbound_staging: 'SLN_09',
                       inbound_source: 'SMN_IN', outbound_destination: 'SMN_OUT',
                       key_route: ['LM_A', 'LM_B', 'LM_C'] } },
            { core_node_name: 'PLN_04', sequence: 2, kind: 'front', x: 2, y: 0 },
        ],
        staging: [
            { core_node_name: 'SLN_07', partner_of: 'PLN_01', partner_kind: 'staging',
              field: 'inbound_staging', x: 0.5, y: -1 },
            { core_node_name: 'SLN_09', partner_of: 'PLN_01', partner_kind: 'staging',
              field: 'outbound_staging', x: 1.5, y: -1 },
            { core_node_name: 'SLN_OFFERED' },
        ],
        // A COORDINATE LIST THE PICTURE MUST NOT READ. CellPicture.LMs is gone
        // from the server; this stays in the fixture so the renderer is proved
        // to ignore it rather than merely not to be given it.
        lms: [{ name: 'LM_NOT_ON_ROUTE', x: 1, y: 0.2 }],
    };
    const svg = m.renderFlowPicture({ cell: cell, station: { name: 'SCREEN' } }, {});

    // THE BAND. Three cards, and none of them is a position.
    check('staging: three cards in the band', count(svg, /<g class="stage /g) === 3, svg.match(/<g class="stage [^>]*/g));
    check('staging: an offered lane is drawn off', /class="stage off" data-staging="SLN_OFFERED"/.test(svg));
    check('staging: a lane in the flow is drawn on', /class="stage on" data-staging="SLN_07"/.test(svg));
    check('staging: the card says which direction it is',
        svg.includes('Inbound staging · for PLN_01') && svg.includes('Outbound staging · for PLN_01'),
        svg.match(/>(In|Out)bound staging[^<]*/g));
    check('staging: a lane is never drawn as a position',
        !/data-pos="SLN_/.test(svg), 'a staging lane reached the positions list');

    // THE LEGS single_robot GREW. In from the inbound lane, out to the outbound one.
    check('legs: single_robot draws two', count(svg, /<g class="legg">/g) === 2, String(count(svg, /<g class="legg">/g)));
    check('legs: one says the robot moves in', svg.includes('>Robot 1 moves in<'));
    check('legs: one says the robot clears the old bin', svg.includes('>Robot 1 clears old<'));

    // ── THE ROUTE STRIP (owner, 2026-09-17; redrawn from the side-by-side) ──
    //
    // "The point of the LMs isn't to represent them to scale, it's to direct
    // flow." So the picture draws no LM GEOGRAPHY: it draws the ORDER the robot
    // is sent through, as a strip HANGING UNDER the card that trip arrives at,
    // rising into the card's bottom edge. Order and direction are the content.
    //
    // HERE THE ARRIVING CARD IS A BAND CARD. PLN_01 stages at SLN_07, which is
    // a staging lane and not a position, so this block is also the pin that a
    // strip under a band card is drawn the same way and given room to be.
    const strip = svg.match(/<g class="lmroute [^]*?<\/g>/);
    check('route: the strip is drawn', !!strip, svg.match(/<g class="lmroute[^>]*/g));
    check('route: it is the trip’s robot colour', /<g class="lmroute r1"/.test(svg));

    // IT HANGS UNDER THE ARRIVING CARD: a vertical line on that card's centre,
    // its top ON the card's bottom edge. Not a horizontal run somewhere else in
    // the picture, which is what this replaced.
    const stgBox = m.layoutStaging(cell, undefined).boxes.SLN_07;
    const line = svg.match(/<path class="lmline" d="M([-\d.]+) ([-\d.]+) V([-\d.]+)"/);
    check('route: the line is vertical, on the arriving card’s centre',
        !!line && Math.abs(+line[1] - (stgBox.x + stgBox.w / 2)) < 0.5,
        line && JSON.stringify({ lineX: line[1], cardCx: stgBox.x + stgBox.w / 2 }));
    check('route: its top is the arriving card’s bottom edge',
        !!line && Math.abs(+line[3] - (stgBox.y + stgBox.h)) < 0.5,
        line && JSON.stringify({ top: line[3], cardBottom: stgBox.y + stgBox.h }));
    check('route: it hangs DOWN from the card', !!line && +line[2] > +line[3],
        line && JSON.stringify({ foot: line[2], top: line[3] }));

    // THE CHEVRON POINTS INTO THE CARD, which is where the bin is going.
    const tip = svg.match(/<path class="lmtip" d="[^"]*" transform="translate\(([-\d.]+),([-\d.]+)\) rotate\((-?[\d.]+)\)"/);
    check('route: one chevron', count(svg, /<path class="lmtip"/g) === 1);
    check('route: the chevron points INTO the card', !!tip && +tip[3] === -90, tip && tip[3]);
    check('route: the chevron sits just under the card’s edge',
        !!tip && !!line && +tip[2] > +line[3] && (+tip[2] - +line[3]) < 24,
        tip && line && JSON.stringify({ tipY: tip[2], cardBottom: line[3] }));

    // NUMBERED IN DRIVING ORDER, READ BOTTOM TO TOP. "1 · LM_A" is the first
    // point the robot passes and sits FURTHEST from the card; the last one is
    // nearest it, because that is the order the trip happens in.
    const lbls = [...svg.matchAll(/<text class="lmlbl" x="([-\d.]+)" y="([-\d.]+)"[^>]*>([^<]+)<\/text>/g)]
        .map(mm => ({ x: +mm[1], y: +mm[2], t: mm[3] }));
    check('route: every chosen LM is on it, NAMED and NUMBERED',
        lbls.length === 3 && lbls.map(l => l.t).join('|') === '1 · LM_A|2 · LM_B|3 · LM_C',
        JSON.stringify(lbls.map(l => l.t)));
    check('route: driving order reads bottom to top',
        lbls.length === 3 && lbls[0].y > lbls[1].y && lbls[1].y > lbls[2].y,
        JSON.stringify(lbls.map(l => l.y)));

    // ONE BASELINE EACH, TO THE RIGHT OF THE LINE. No stagger: the alternating
    // above/below baselines the first cut needed are gone with the horizontal
    // run that forced them.
    check('route: names sit to the right of the line',
        !!line && lbls.length === 3 && lbls.every(l => l.x > +line[1]),
        line && JSON.stringify({ lineX: line[1], labelX: lbls.map(l => l.x) }));
    check('route: one column, no stagger',
        lbls.length === 3 && lbls.every(l => l.x === lbls[0].x),
        JSON.stringify(lbls.map(l => l.x)));

    // EVENLY SPACED, which is the whole of "not to scale": the dots sit at
    // equal intervals whatever the map says about the distances between them.
    check('route: evenly spaced', (() => {
        const ys = [...svg.matchAll(/<circle class="lmdot" cx="[-\d.]+" cy="([-\d.]+)"/g)].map(mm => +mm[1]);
        if (ys.length !== 3) return false;
        const d1 = ys[0] - ys[1], d2 = ys[1] - ys[2];
        return Math.abs(d1 - d2) < 0.5 && d1 > 1;
    })(), [...svg.matchAll(/<circle class="lmdot" cx="[-\d.]+" cy="([-\d.]+)"/g)].map(mm => mm[1]).join(','));

    // WHOSE TRIP IT IS, in the picture's muted token. The strip's colour says
    // it too, and a colour alone is not a label.
    check('route: it says whose trip it is', /<text class="lmvia"[^>]*>Robot 1 comes in via<\/text>/.test(svg),
        svg.match(/<text class="lmvia"[^>]*>[^<]*/g));

    // IT DOES NOT REACH THE DOCK. The strip is read-back about one card, not a
    // second leg drawn from the IN glyph — which is what the first cut was, and
    // what put two positions' routes in the same corner of the picture.
    check('route: the strip does not touch the dock rule', (() => {
        const dock = svg.match(/<line x1="[\d.]+" y1="([\d.]+)"/);
        return !!dock && !!line && +line[2] < +dock[1];
    })(), JSON.stringify({ foot: line && line[2] }));

    // NO GEOGRAPHY LEFT. The beads, the true-place marks and the coordinate
    // list they were read from are all gone.
    check('route: no unnamed beads', !/<g class="lm[ "]/.test(svg), svg.match(/<g class="lm[^rv][^>]*/g));
    check('route: nothing reads cell.lms', !svg.includes('LM_NOT_ON_ROUTE'));

    // NO KEY ROUTE, NOTHING EXTRA — "shortest way", which is what the panel
    // already calls it.
    const noRoute = JSON.parse(JSON.stringify(cell));
    noRoute.positions[0].claim.key_route = [];
    const bare = m.renderFlowPicture({ cell: noRoute, station: { name: 'SCREEN' } }, {});
    check('route: none drawn without a chosen route',
        !/<g class="lmroute /.test(bare) && !/class="lmdot"/.test(bare));

    // THE SAME STRIP ON THE SCHEMATIC. Nothing on it was ever to scale, so
    // there is nothing for the schematic to fall back from.
    const schematic = JSON.parse(JSON.stringify(cell));
    schematic.geometry = false;
    for (const p of schematic.positions) { delete p.x; delete p.y; }
    const sch = m.renderFlowPicture({ cell: schematic, station: { name: 'SCREEN' } }, {});
    check('route: the schematic draws the same strip',
        /<g class="lmroute /.test(sch) && sch.includes('>1 · LM_A<') && sch.includes('>3 · LM_C<'));
    check('route: and still one chevron on it', count(sch, /<path class="lmtip"/g) === 1);
}

if (failures) { console.log(failures + ' FAILED'); process.exit(1); }
console.log('operator-flow: all checks pass');
