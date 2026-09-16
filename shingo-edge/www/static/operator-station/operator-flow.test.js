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
    vm.runInContext(src + '\n__out = { renderFlowPicture, layoutPositions, legsFor, sentencesFromView, sentencesFromModel, CARD_W, CARD_H };', ctx);
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

console.log('style 7 — PART 40421-RVJ56.37, two index pairs');
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
    check('part chips', svg.includes('>55544-DWC33.21<') && svg.includes('>61477-ATD38.66<'));
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

console.log('style 11 — PART 68644-WSL97.20, two staging moves');
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
    check('staging cards say whose', svg.includes('>staging for PLN_03<') && svg.includes('>staging for PLN_06<'));
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

if (failures) { console.log(failures + ' FAILED'); process.exit(1); }
console.log('operator-flow: all checks pass');
