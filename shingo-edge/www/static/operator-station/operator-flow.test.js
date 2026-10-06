// operator-flow.test.js — the read-only cell picture, rendered from station
// views the Go side built over the Hopkinsville pull (operator_flow_test.go
// writes them and passes their paths), plus synthetic cells for the shapes
// that plant does not carry. What is asserted is what the operator would see
// wrong: the order the modules stand in, the press rows the captions beside
// the picture name, the dock, and the key-route strips.
//
// What lives elsewhere: the per-mode moves, card lines, dock notes and legend
// in operator-flow.modes.test.js; every card inside its frame, no two
// overlapping and every text inside its card in operator-flow.geometry.test.js.
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
    // its own: the card lines, the dock notes, which legs exist and which
    // robot makes each move all come from composer-model.js, so a harness
    // without it would be rendering a picture no station ever draws. The file
    // hangs its api on window, which is where sentencesFromView looks.
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
    vm.runInContext(src + '\n__out = { renderFlowPicture, pictureRows, CARD_W, CARD_H };', ctx);
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
function clone(o) { return JSON.parse(JSON.stringify(o)); }

// cardsOf reads every position card's box back off the markup — the place the
// browser draws it. Only position cards (`node`), not staging slots: the
// geometry pin owns every rect; these are read for order and for the dock.
function cardsOf(svg) {
    const out = [];
    const re = /<g class="node[^"]*" data-pos="([^"]+)"[^>]*transform="translate\(([-\d.]+),([-\d.]+)\)">(?:<title>[^<]*<\/title>)?<rect class="box" width="([\d.]+)" height="([\d.]+)"/g;
    let mm;
    while ((mm = re.exec(svg))) out.push({ name: mm[1], x: +mm[2], y: +mm[3], w: +mm[4], h: +mm[5] });
    return out;
}

// modulesOf reads the modules back in drawing order: the first position card
// in each is the module's lead (the press, the A of an A/B pair), and the
// module's place is that card's.
function modulesOf(svg) {
    return svg.split('<g class="module"').slice(1).map(part => {
        const cards = cardsOf(part);
        return { lead: cards[0] && cards[0].name, x: cards[0] && cards[0].x, y: cards[0] && cards[0].y, names: cards.map(c => c.name) };
    });
}

// dockOf is the dock's rule and every text row under it, in picture
// coordinates: the dock's halves are translated to the rule's y.
function dockOf(svg) {
    const ln = svg.match(/<g class="dock"><line x1="([\d.]+)" y1="([\d.]+)" x2="([\d.]+)" y2="([\d.]+)"/);
    if (!ln) return null;
    const y = +ln[2];
    const rows = [...svg.slice(svg.indexOf('<g class="dock">')).matchAll(/<text class="[ksv][^"]*"[^>]*y="(\d+)"/g)].map(mm => y + +mm[1]);
    return { x1: +ln[1], y: y, x2: +ln[3], rows: rows, deepest: Math.max(...rows) };
}

// ── style 7 — PART SYN-A-S003, two index pairs ──────────────────────────────
console.log('style 7 — PART SYN-A-S003, two index pairs');
{
    const svg = m.renderFlowPicture(view7);
    check('dock groups', svg.includes('>Supermarket Empty Totes<') && svg.includes('>Supermarket Area<'),
        (svg.match(/<text class="v"[^>]*>[^<]*/g) || []).join(' | '));
    // THE MEMBERS ROW. The note above it names the POSITIONS; this row is
    // where the bins actually come from and go to.
    check('dock members', svg.includes('SMN_05, SMN_06, SMN_07, SMN_08') &&
        (svg.includes('SMN_04, SMN_01, SMN_02, SMN_03') || svg.includes('SMN_01, SMN_02, SMN_03, SMN_04')),
        (svg.match(/SMN_[^<]*/g) || []).join(' | '));
    check('part chips', svg.includes('>SYN-A-P002<') && svg.includes('>SYN-A-P003<'));
    check('no R1/R2 abbreviation anywhere an operator reads', !/>[^<]*\bR[12]\b[^<]*</.test(svg));
    // A <marker> is referenced by url(#id) and resolves over the whole
    // DOCUMENT, and the composer draws a second copy of this picture beside
    // the first — two markers of one name is one marker, whichever rendered
    // last, and one picture's chevrons would take the other's hue. The move
    // glyph is a plain path, placed and rotated.
    check('no move is drawn with a <marker>', !svg.includes('marker'));

    // THE ORDER PIN — it replaces "PLN_01 to PLN_02 is 1.772 m at 120 px/m".
    //
    // The spacing is gone with the to-scale layout; what that check's
    // DISTANCE-NOT-DIRECTION note was protecting survives: the row the
    // operator walks. Modules stand left to right by the world X of their
    // lead position, on every row of the grid, and a mirrored world mirrors
    // the row — so it is the world X deciding, not the sequence or the name.
    //
    // EVERY MODULE HERE IS LINE-SIDE. A back position goes after the line side
    // only when no claim covers it and no other module draws it; this press's
    // index positions stand in the back row but run claims, and their on-deck
    // partners are drawn inside them, so the whole row orders by world X.
    const worldX = {};
    for (const p of view7.cell.positions) worldX[p.core_node_name] = p.x;
    const inWorldOrder = mods => {
        const rows = {};
        for (const md of mods) (rows[md.y] = rows[md.y] || []).push(md);
        return Object.values(rows).every(r => r.every((md, i) => i === 0 ||
            (r[i - 1].x < md.x && worldX[r[i - 1].lead] <= worldX[md.lead])));
    };
    const mods = modulesOf(svg);
    check('order: every position is in a module', new Set(mods.flatMap(md => md.names)).size === view7.cell.positions.length,
        JSON.stringify(mods.map(md => md.names)));
    check('order: modules stand left to right by world X', inWorldOrder(mods),
        JSON.stringify(mods.map(md => ({ lead: md.lead, drawn: md.x, world: worldX[md.lead] }))));
    for (const w of [640, 1084]) {
        const ms = modulesOf(m.renderFlowPicture(view7, { frame: { w: w } }));
        check('order: and still when the grid wraps at ' + w, inWorldOrder(ms),
            JSON.stringify(ms.map(md => ({ lead: md.lead, drawn: [md.x, md.y], world: worldX[md.lead] }))));
    }
    const mirrored = clone(view7);
    for (const p of mirrored.cell.positions) p.x = -p.x;
    const leads = mods.map(md => md.lead).join(' ');
    const mirroredLeads = modulesOf(m.renderFlowPicture(mirrored)).map(md => md.lead).reverse().join(' ');
    check('order: a mirrored world mirrors the row', leads === mirroredLeads, leads + ' vs reversed ' + mirroredLeads);
    // THE ON-DECK POSITION IS INSIDE ITS PRESS'S MODULE. The pair was two
    // cards a leg apart; it is one module now, and layout cannot part it.
    for (const [press, deck] of [['PLN_01', 'PLN_02'], ['PLN_04', 'PLN_05']]) {
        const md = mods.find(x => x.lead === press);
        check('order: ' + deck + ' is drawn in ' + press + '’s module', !!md && md.names.includes(deck),
            JSON.stringify(mods.map(x => x.names)));
    }

    // FRONT AND BACK ARE THE PRESS'S ROWS, from world Y — the line side is
    // the higher Y — and never from the grid, which wraps with the frame.
    const rows = m.pictureRows(view7.cell);
    // Hopkinsville's press is two rows 1.15 m apart in world Y, each a few
    // millimetres ragged; the midpoint splits them.
    const ys = view7.cell.positions.map(p => p.y);
    const mid = (Math.max(...ys) + Math.min(...ys)) / 2;
    check('rows: the higher world Y is front, the lower is back',
        view7.cell.positions.every(p => rows[p.core_node_name] === (p.y > mid ? 'front' : 'back')),
        JSON.stringify(view7.cell.positions.map(p => [p.core_node_name, p.y, rows[p.core_node_name]])));
    check('rows: some position is front and some is back',
        Object.values(rows).includes('front') && Object.values(rows).includes('back'), JSON.stringify(rows));
    // THE WORDS THE TO-SCALE PICTURE GAVE THIS PRESS, written out: the
    // line-side row is PLN_03/PLN_06, and the index positions stand behind it
    // with their on-deck partners.
    const want7 = { PLN_01: 'back', PLN_03: 'front', PLN_02: 'back', PLN_04: 'back', PLN_06: 'front', PLN_05: 'back' };
    for (const n of Object.keys(want7)) {
        check('rows: ' + n + ' is ' + want7[n], rows[n] === want7[n], JSON.stringify(rows));
    }
}

// ── style 11 — PART SYN-A-S007, two staging moves ───────────────────────────
console.log('style 11 — PART SYN-A-S007, two staging moves');
{
    const svg = m.renderFlowPicture(view11);
    // THE SAME SENTENCE AS STYLE 7'S. The dock note used to read `new bins
    // here` where style 7 read `new totes here`, from the payload's bin type —
    // owner ruling R2 (2026-09-12) removed the word.
    check('no bin word survives on the picture', !/tote|Tote/.test(svg.replace(/Supermarket Empty Totes/g, '')),
        (svg.match(/[^<>]*[Tt]ote[^<>]*/g) || []).join(' | '));
    // THE PRESS DID NOT CHANGE SHAPE when the style did: rows are geometry.
    // THE FREE BACK POSITIONS GO AFTER THE LINE SIDE. PLN_02 and PLN_05 are
    // the back-row positions PLN_03's and PLN_06's new bins wait in: each is
    // drawn as its module's inbound slot and has no card of its own. The
    // unclaimed PLN_01 and PLN_04, which no claim names, stand in the back row
    // and go after the line side by world X.
    const leads11 = modulesOf(svg).map(md => md.lead).join(' ');
    check('order: the line side first, the free back positions after it by world X',
        leads11 === 'PLN_03 PLN_06 PLN_01 PLN_04', leads11);
    for (const [pos, st] of [['PLN_03', 'PLN_02'], ['PLN_06', 'PLN_05']]) {
        const part = svg.split('<g class="module"').slice(1).find(x => x.includes('data-pos="' + pos + '"')) || '';
        check('staging at a position: ' + st + ' is ' + pos + "'s inbound slot, and has no card of its own",
            part.includes('data-staging="' + st + '"') && !cardsOf(svg).some(c => c.name === st));
    }
    check('rows: the same press rows as style 7',
        JSON.stringify(m.pictureRows(view11.cell)) === JSON.stringify(m.pictureRows(view7.cell)));
}

// ── schematic — no geometry cached ──────────────────────────────────────────
console.log('schematic — no geometry cached');
{
    const svg = m.renderFlowPicture(viewSchematic);
    const placed = m.renderFlowPicture(view7);
    // THE MODULES DO NOT DEPEND ON THE MAP. The same style draws the same
    // moves and the same cards whether or not the scene is cached.
    check('the same moves as with geometry',
        count(svg, /<path class="sc r2" d="M-5/g) === count(placed, /<path class="sc r2" d="M-5/g) &&
        count(svg, /<path class="sc r1" d="M-5/g) === count(placed, /<path class="sc r1" d="M-5/g),
        count(svg, /<path class="sc r[12]" d="M-5/g) + ' vs ' + count(placed, /<path class="sc r[12]" d="M-5/g));
    // NO COORDINATES, THE SEQUENCE DECIDES.
    const seq = {};
    for (const p of viewSchematic.cell.positions) seq[p.core_node_name] = p.sequence || 0;
    const mods = modulesOf(svg);
    check('order: modules stand left to right by sequence',
        mods.every((md, i) => i === 0 || mods[i - 1].y !== md.y || seq[mods[i - 1].lead] <= seq[md.lead]),
        JSON.stringify(mods.map(md => ({ lead: md.lead, seq: seq[md.lead] }))));
    // NO COORDINATES, THE KIND NAMES THE ROW — the schematic's own two rows:
    // every position not Kind "back" in front, the Kind "back" ones behind.
    // Not the grid, which wraps with the frame. These are the words the
    // schematic gave this press before the module picture.
    const rows = m.pictureRows(viewSchematic.cell);
    const wantSch = { PLN_01: 'front', PLN_03: 'front', PLN_02: 'back', PLN_04: 'front', PLN_06: 'front', PLN_05: 'back' };
    for (const n of Object.keys(wantSch)) {
        check('rows: without coordinates ' + n + ' is ' + wantSch[n], rows[n] === wantSch[n], JSON.stringify(rows));
    }
    check('rows: every position has its word', Object.keys(rows).length === Object.keys(wantSch).length, JSON.stringify(rows));
    // Kind for a position off the map: one missing coordinate is no
    // coordinates, as it always was.
    const partial = clone(view7.cell);
    delete partial.positions[0].x;
    check('rows: one position off the map reads the kind',
        JSON.stringify(m.pictureRows(partial)) === JSON.stringify(m.pictureRows(Object.assign(clone(view7.cell), { geometry: false }))),
        JSON.stringify(m.pictureRows(partial)));
}

console.log('empty cell');
{
    const svg = m.renderFlowPicture({ station: { name: 'X' }, cell: { positions: [], geometry: false } });
    check('never a blank panel', svg.includes('No positions on this station yet'));
}

// ── the dock, under the grid, at every frame ────────────────────────────────
//
// The dock sits under the last grid row, and the height the picture reports
// holds all of it: the rule, the group, the note and the members row. A dock
// row past the reported height is a member line clipped by the frame.
console.log('the dock is under the grid and inside the reported height');
for (const w of [1280, 1084, 640]) {
    const o = { frame: { w: w } };
    const svg = m.renderFlowPicture(view7, o);
    const d = dockOf(svg);
    check(w + ': the dock draws its rows', !!d && d.rows.length === 8, d && String(d.rows.length));
    check(w + ': every dock row inside the reported height', !!d && d.deepest < o.height,
        d && ('deepest ' + d.deepest + ' of ' + o.height));
    check(w + ': every card above the dock rule', !!d && cardsOf(svg).every(c => c.y + c.h <= d.y),
        d && JSON.stringify(cardsOf(svg).filter(c => c.y + c.h > d.y)));
}

// ── the frame — a card is the same size whatever the frame ──────────────────
//
// The picture is laid out in absolute user units and the caller sets the
// viewBox to match, so a card is CARD_W px wide on screen only when the
// viewBox is the element's real width. The desktop draws at its own column
// width rather than the station's 1280 scaled down.
console.log('the frame — the station keeps its own, the desktop asks for the column it has');
{
    const station = m.renderFlowPicture(view7);
    const desk = m.renderFlowPicture(view7, { frame: { w: 1084 } });
    for (const [label, svg] of [['station', station], ['desktop', desk]]) {
        const press = cardsOf(svg).filter(c => c.h === m.CARD_H);
        check(label + ': every position card is ' + m.CARD_W + 'x' + m.CARD_H,
            press.length > 0 && press.every(c => c.w === m.CARD_W), JSON.stringify(press));
    }
    // THE GUTTER IS THE RULE, NOT THE COORDINATE: the dock line is inset by
    // the same gutter as the station's, on both ends, whatever the frame.
    const sd = dockOf(station), dd = dockOf(desk);
    check('desktop dock starts at the same gutter as the station', sd && dd && dd.x1 === sd.x1,
        JSON.stringify({ station: sd, desktop: dd }));
    check('desktop dock ends a gutter short of its frame', sd && dd && (1084 - dd.x2) === (1280 - sd.x2),
        sd && dd && JSON.stringify({ deskRightGutter: 1084 - dd.x2, stationRightGutter: 1280 - sd.x2 }));
}

// ── two routes at once, and more LMs than fit ───────────────────────────────
//
// SYNTHETIC. A two-row cell where BOTH front positions stage on a back
// position and BOTH carry a key route. The staging place is a POSITION, drawn
// as the module's inbound slot, so the strip hangs under that slot — the box
// the trip arrives at — in the trip's robot's colour.
{
    console.log('two routes, each under the card its trip arrives at');
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
    const o = {};
    const svg = m.renderFlowPicture({ cell: two(), station: { name: 'SCREEN' } }, o);
    check('two routes: rows from world Y',
        JSON.stringify(m.pictureRows(two())) === JSON.stringify({ PLN_03: 'front', PLN_06: 'front', PLN_02: 'back', PLN_05: 'back' }),
        JSON.stringify(m.pictureRows(two())));

    check('two routes: two strips', count(svg, /<g class="lmroute /g) === 2,
        String(count(svg, /<g class="lmroute /g)));
    check('two routes: two chevrons', count(svg, /<path class="lmtip"/g) === 2);
    // The move into the front card is Robot 1's in a two-robot swap — the
    // model's leg says so, and the strip is that trip.
    check('two routes: in the trip’s robot colour', count(svg, /<g class="lmroute r1"/g) === 2,
        (svg.match(/<g class="lmroute[^>]*/g) || []).join(' | '));
    const slots = [...svg.matchAll(/<g class="stage[^"]*" data-staging="([^"]+)"[^>]*transform="translate\(([-\d.]+),([-\d.]+)\)">(?:<title>[^<]*<\/title>)?<rect class="box" width="([\d.]+)" height="([\d.]+)"/g)]
        .map(mm => ({ name: mm[1], x: +mm[2], y: +mm[3], w: +mm[4], h: +mm[5] }));
    const lines = [...svg.matchAll(/<path class="lmline" d="M([-\d.]+) ([-\d.]+) ?V([-\d.]+)"/g)]
        .map(mm => ({ x: +mm[1], foot: +mm[2], top: +mm[3] }));
    for (const n of ['PLN_02', 'PLN_05']) {
        const c = slots.find(k => k.name === n);
        check('two routes: one hangs from ' + n + '’s slot’s bottom edge',
            !!c && lines.some(l => l.x > c.x && l.x < c.x + c.w && Math.abs(l.top - 4 - (c.y + c.h)) < 0.5),
            JSON.stringify({ slot: c, lines: lines }));
    }
    check('two routes: each names its own points',
        svg.includes('>1 · LM10<') && svg.includes('>2 · LM11<') &&
        svg.includes('>1 · LM20<') && svg.includes('>2 · LM21<'));
    const d = dockOf(svg);
    check('two routes: every strip stops above the dock rule',
        lines.length === 2 && lines.every(l => l.foot < d.y), JSON.stringify({ lines: lines, dockY: d.y }));
    check('two routes: the reported height holds them', lines.every(l => l.foot < o.height), String(o.height));

    // FOUR MUST FIT. That is the sizing rule the tokens were chosen against;
    // a fifth is reported rather than drawn.
    const four = two();
    four.positions[0].claim.key_route = ['LM10', 'LM11', 'LM12', 'LM13'];
    const svg4 = m.renderFlowPicture({ cell: four, station: { name: 'SCREEN' } }, {});
    check('four LMs fit',
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
}

// ── staging lanes in the module, and the route strip under its slot ─────────
//
// SYNTHETIC. A single_robot press staging at two LANES (not positions), one
// lane in the routing set this flow does not use, and a key route on the
// inbound trip.
{
    console.log('staging slots and the route strip under a slot');
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
    const flat = svg.replace(/<tspan[^>]*>/g, '').replace(/<\/tspan>/g, '');

    // THE SLOTS. Two, both lanes the flow uses; the offered lane is not a
    // slot — on the station it is not drawn at all, on the desktop it is a
    // chip on the unused-staging line.
    check('staging: a slot for each lane in the flow',
        count(svg, /<g class="stage on/g) === 2 && /data-staging="SLN_07"/.test(svg) && /data-staging="SLN_09"/.test(svg),
        svg.match(/<g class="stage [^>]*/g));
    check('staging: each slot says which direction it is',
        flat.includes('>Inbound staging<') && flat.includes('>Outbound staging<'),
        flat.match(/>(In|Out)bound staging[^<]*/g));
    check('staging: a lane is never drawn as a position', !/data-pos="SLN_/.test(svg.replace(/data-tap="[^"]*" data-pos="[^"]*"/g, '')));
    check('staging: the station leaves the offered lane out', !svg.includes('SLN_OFFERED'));
    const desk = m.renderFlowPicture({ cell: cell, station: { name: 'SCREEN' } }, { editable: true, frame: { w: 1084 } });
    check('staging: the desktop draws the offered lane as an unused chip',
        /<g class="stage off chip"[^>]*>(<title>[^<]*<\/title>)?<rect[^>]*\/><text class="nm"[^>]*>SLN_OFFERED</.test(desk),
        desk.match(/<g class="stage off chip"[^]*?<\/g>/g));

    // A NODE A CLAIM NAMES IS USED, WHETHER OR NOT ITS MODULE HAS A SLOT. The
    // index template draws no staging slot, but a press index's inbound
    // staging is read by the staged tooling changeover — so a lane it names
    // is in use, and the desktop must not list it as "not used by this flow".
    // A lane in the routing set no claim names still is.
    const idx = {
        geometry: true,
        positions: [
            { core_node_name: 'PLN_10', sequence: 1, kind: 'front', x: 0, y: 0,
              claim: { swap_mode: 'two_robot_press_index', payload_code: 'P10', paired_core_node: 'PLN_11',
                       inbound_staging: 'SLN_IDX', inbound_source: 'SMN_IN', outbound_destination: 'SMN_OUT' } },
            { core_node_name: 'PLN_11', sequence: 0, kind: 'back', x: 0, y: -1.8 },
        ],
        staging: [{ core_node_name: 'SLN_IDX' }, { core_node_name: 'SLN_FREE' }],
    };
    const idxDesk = m.renderFlowPicture({ cell: idx, station: { name: 'SCREEN' } }, { editable: true, frame: { w: 1084 } });
    const idxChips = (idxDesk.match(/<g class="stage off chip"[^]*?<\/g>/g) || []).join('');
    check('unused line: a press index\'s named inbound staging is not listed as unused',
        !idxChips.includes('SLN_IDX'), idxChips);
    check('unused line: a routing-set lane no claim names is still listed',
        idxChips.includes('>SLN_FREE<'), idxChips);

    // THE TWO MOVES single_robot MAKES, as the card's words (no label on a
    // move): in from the inbound lane, the old bin cleared.
    check('moves: the card says the robot moves in', flat.includes('>Robot 1 moves in<'));
    check('moves: the card says the robot clears the old bin', flat.includes('>Robot 1 clears old<'));

    // ── THE ROUTE STRIP (owner, 2026-09-17) ──
    //
    // "The point of the LMs isn't to represent them to scale, it's to direct
    // flow." So the picture draws no LM GEOGRAPHY: it draws the ORDER the robot
    // is sent through, as a strip HANGING UNDER the slot that trip arrives at,
    // rising into the slot's bottom edge. Order and direction are the content.
    const strip = svg.match(/<g class="lmroute [^]*?<\/g>/);
    check('route: the strip is drawn', !!strip, svg.match(/<g class="lmroute[^>]*/g));
    check('route: it is the trip’s robot colour', /<g class="lmroute r1"/.test(svg));

    const slot = svg.match(/<g class="stage[^"]*" data-staging="SLN_07"[^>]*transform="translate\(([-\d.]+),([-\d.]+)\)">(?:<title>[^<]*<\/title>)?<rect class="box" width="([\d.]+)" height="([\d.]+)"/);
    const sb = slot && { x: +slot[1], y: +slot[2], w: +slot[3], h: +slot[4] };
    const line = svg.match(/<path class="lmline" d="M([-\d.]+) ([-\d.]+) ?V([-\d.]+)"/);
    check('route: the line is vertical, under the arriving slot',
        !!line && !!sb && +line[1] > sb.x && +line[1] < sb.x + sb.w,
        line && JSON.stringify({ lineX: line[1], slot: sb }));
    check('route: it rises into the slot’s bottom edge',
        !!line && !!sb && Math.abs(+line[3] - 4 - (sb.y + sb.h)) < 0.5,
        line && JSON.stringify({ top: line[3], slotBottom: sb && sb.y + sb.h }));
    check('route: it hangs DOWN from the slot', !!line && +line[2] > +line[3],
        line && JSON.stringify({ foot: line[2], top: line[3] }));

    // THE CHEVRON POINTS INTO THE SLOT, which is where the bin is going.
    const tip = svg.match(/<path class="lmtip" d="[^"]*" transform="translate\(([-\d.]+),([-\d.]+)\) rotate\((-?[\d.]+)\)"/);
    check('route: one chevron', count(svg, /<path class="lmtip"/g) === 1);
    check('route: the chevron points INTO the slot', !!tip && +tip[3] === -90, tip && tip[3]);
    check('route: the chevron sits just under the slot’s edge',
        !!tip && !!line && +tip[2] > +line[3] && (+tip[2] - +line[3]) < 24,
        tip && line && JSON.stringify({ tipY: tip[2], top: line[3] }));

    // NUMBERED IN DRIVING ORDER, READ BOTTOM TO TOP. "1 · LM_A" is the first
    // point the robot passes and sits FURTHEST from the slot.
    const lbls = [...svg.matchAll(/<text class="lmlbl" x="([-\d.]+)" y="([-\d.]+)"[^>]*>([^<]+)<\/text>/g)]
        .map(mm => ({ x: +mm[1], y: +mm[2], t: mm[3] }));
    check('route: every chosen LM is on it, NAMED and NUMBERED',
        lbls.length === 3 && lbls.map(l => l.t).join('|') === '1 · LM_A|2 · LM_B|3 · LM_C',
        JSON.stringify(lbls.map(l => l.t)));
    check('route: driving order reads bottom to top',
        lbls.length === 3 && lbls[0].y > lbls[1].y && lbls[1].y > lbls[2].y,
        JSON.stringify(lbls.map(l => l.y)));
    check('route: names sit to the right of the line',
        !!line && lbls.length === 3 && lbls.every(l => l.x > +line[1]),
        line && JSON.stringify({ lineX: line[1], labelX: lbls.map(l => l.x) }));
    check('route: one column, no stagger',
        lbls.length === 3 && lbls.every(l => l.x === lbls[0].x), JSON.stringify(lbls.map(l => l.x)));
    check('route: evenly spaced', (() => {
        const ys = [...svg.matchAll(/<circle class="lmdot" cx="[-\d.]+" cy="([-\d.]+)"/g)].map(mm => +mm[1]);
        if (ys.length !== 3) return false;
        const d1 = ys[0] - ys[1], d2 = ys[1] - ys[2];
        return Math.abs(d1 - d2) < 0.5 && d1 > 1;
    })(), [...svg.matchAll(/<circle class="lmdot" cx="[-\d.]+" cy="([-\d.]+)"/g)].map(mm => mm[1]).join(','));

    // WHOSE TRIP IT IS, in words as well as colour — a colour alone is not a
    // label. The words are the reference's strip, exactly: "Robot 1 via"
    // (REFERENCE-module-picture-2026-10-05.html, the key-route strip).
    check('route: it says whose trip it is', /<text class="lmvia"[^>]*>Robot 1 via<\/text>/.test(svg),
        svg.match(/<text class="lmvia"[^>]*>[^<]*/g));
    check('route: the strip does not touch the dock rule', (() => {
        const d = dockOf(svg);
        return !!d && !!line && +line[2] < d.y;
    })(), JSON.stringify({ foot: line && line[2] }));

    // NO GEOGRAPHY LEFT. The beads, the true-place marks and the coordinate
    // list they were read from are all gone.
    check('route: no unnamed beads', !/<g class="lm[ "]/.test(svg), svg.match(/<g class="lm[^rv][^>]*/g));
    check('route: nothing reads cell.lms', !svg.includes('LM_NOT_ON_ROUTE'));

    // NO KEY ROUTE, NOTHING EXTRA — "shortest way".
    const noRoute = clone(cell);
    noRoute.positions[0].claim.key_route = [];
    const bare = m.renderFlowPicture({ cell: noRoute, station: { name: 'SCREEN' } }, {});
    check('route: none drawn without a chosen route',
        !/<g class="lmroute /.test(bare) && !/class="lmdot"/.test(bare));

    // THE SAME STRIP ON THE SCHEMATIC. Nothing on it was ever to scale.
    const schematic = clone(cell);
    schematic.geometry = false;
    for (const p of schematic.positions) { delete p.x; delete p.y; }
    const sch = m.renderFlowPicture({ cell: schematic, station: { name: 'SCREEN' } }, {});
    check('route: the schematic draws the same strip',
        /<g class="lmroute /.test(sch) && sch.includes('>1 · LM_A<') && sch.includes('>3 · LM_C<'));
    check('route: and still one chevron on it', count(sch, /<path class="lmtip"/g) === 1);
}

// ── the order: line side first, then the back row's own cards ───────────────
//
// SYNTHETIC. A two-row cell whose free back position PLN_09 — no claim covers
// it or stages at it — stands to the LEFT of every line-side position. PLN_02,
// the back position PLN_03's new bin waits in, is drawn as PLN_03's slot and
// so is no module of its own. Line-side
// modules come first, left to right; a back position that is not drawn inside
// another position's module comes after them, whatever its X. A back position
// that IS inside a module (the on-deck card of an index pair, the B of an A/B
// pair) is never pulled out of it, however far left it stands.
{
    console.log('order: line side first, then back positions not inside a module');
    const cell = (coords) => {
        const at = (x, y) => coords ? { x: x, y: y } : {};
        return {
            geometry: coords,
            positions: [
                Object.assign({ core_node_name: 'PLN_03', sequence: 2, kind: 'front',
                  claim: { swap_mode: 'two_robot', payload_code: 'P3', inbound_staging: 'PLN_02',
                           inbound_source: 'SMN_IN', outbound_destination: 'SMN_OUT' } }, at(2, 0)),
                Object.assign({ core_node_name: 'PLN_10', sequence: 3, kind: 'front',
                  claim: { swap_mode: 'two_robot_press_index', payload_code: 'P10', paired_core_node: 'PLN_11',
                           inbound_source: 'SMN_IN', outbound_destination: 'SMN_OUT' } }, at(3, 0)),
                Object.assign({ core_node_name: 'PLN_11', sequence: 0, kind: 'back' }, at(-1, -1.8)),
                Object.assign({ core_node_name: 'PLN_06', sequence: 4, kind: 'front',
                  claim: { swap_mode: 'single_robot', payload_code: 'P6',
                           inbound_source: 'SMN_IN', outbound_destination: 'SMN_OUT' } }, at(4, 0)),
                Object.assign({ core_node_name: 'ALN_A', sequence: 5, kind: 'front',
                  claim: { swap_mode: 'sequential', payload_code: 'PA', paired_core_node: 'ALN_B',
                           inbound_source: 'SMN_IN', outbound_destination: 'SMN_OUT' } }, at(5, 0)),
                Object.assign({ core_node_name: 'ALN_B', sequence: 0, kind: 'back',
                  claim: { swap_mode: 'sequential', payload_code: 'PA', paired_core_node: 'ALN_A',
                           inbound_source: 'SMN_IN', outbound_destination: 'SMN_OUT' } }, at(-2, -1.8)),
                Object.assign({ core_node_name: 'PLN_02', sequence: 1, kind: 'back' }, at(0, -1.8)),
                Object.assign({ core_node_name: 'PLN_09', sequence: 6, kind: 'back' }, at(-3, -1.8)),
            ],
        };
    };
    const want = [['PLN_03'], ['PLN_10', 'PLN_11'], ['PLN_06'], ['ALN_A', 'ALN_B'], ['PLN_09']];
    for (const [label, coords] of [['by world X', true], ['by sequence, back from the kind', false]]) {
        const c = cell(coords);
        check('order ' + label + ': the rows are the ones this pin means',
            m.pictureRows(c).PLN_02 === 'back' && m.pictureRows(c).PLN_09 === 'back' && m.pictureRows(c).PLN_03 === 'front',
            JSON.stringify(m.pictureRows(c)));
        for (const w of [1280, 640]) {
            const got = modulesOf(m.renderFlowPicture({ cell: c, station: { name: 'SCREEN' } }, { frame: { w: w } }))
                .map(md => md.names);
            check('order ' + label + ' at ' + w + ': line side first, the free back position last, no pair split',
                JSON.stringify(got) === JSON.stringify(want), JSON.stringify(got));
        }
    }
}

if (failures) { console.log(failures + ' FAILED'); process.exit(1); }
console.log('operator-flow: all checks pass');
