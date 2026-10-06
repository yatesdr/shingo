// operator-flow.geometry.test.js — the DISJOINTNESS pin for the cell picture,
// run from a Go test (operator_flow_geometry_test.go) with no database: the
// cells here are synthetic, so the pin can ask for shapes no seeded plant
// carries.
//
// WHAT IT PINS, on every frame the picture is drawn at (640 to 1600 in steps
// of 40, plus the station's 1280x560): every drawn card inside the frame (its
// width by the height the picture reports), no two cards overlapping, and
// every text fitting the card that drew it by a character budget computed
// from that card's own width. This replaces the
// whole-plant test's cardsOf, which hard-coded six cards and one frame.
//
// RED UNTIL THE MODULE PICTURE LANDS, AND COMMITTED WITH IT — the way a
// fault's test is. At the pre-change tree the staging band grows its cards to
// fit their words and lays them in one row whatever the frame (six grown
// cards are 1242 units against a 1240 span at 1280, and off the frame at
// 640), long names are drawn in full, and a nine-position row does not wrap.
// The design sheet's numbers table (fixed 116-wide staging slots, module
// wrap at frame width, names cut in the middle with the full name in the
// tooltip) is the spec this pin holds the rewrite to.

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
    vm.runInContext(src + '\n__out = { renderFlowPicture, pictureRows };', ctx);
    return ctx.__out;
}

const m = load();
function count(svg, re) { return (svg.match(re) || []).length; }

// ── the cells ───────────────────────────────────────────────────────────────
//
// WELD-2 WITHOUT GEOMETRY. The lane-stress sim plant's weld cell — consume
// PANEL-B and BRKT, produce ASSY, single_robot with both staging lanes, none
// of it on the map — which is the schematic every width must hold.
function weld2() {
    return {
        geometry: false,
        positions: [
            { core_node_name: 'ALN_003', sequence: 1, kind: 'front',
              claim: { swap_mode: 'single_robot', payload_code: 'PANEL-B',
                       inbound_staging: 'SLN_003', outbound_staging: 'SLN_004',
                       inbound_source: 'SYN_STAMP', outbound_destination: 'SYN_STAMP' } },
            { core_node_name: 'ALN_004', sequence: 2, kind: 'front',
              claim: { swap_mode: 'single_robot', payload_code: 'BRKT',
                       inbound_staging: 'SLN_005', outbound_staging: 'SLN_006',
                       inbound_source: 'SYN_COMP', outbound_destination: 'SYN_COMP' } },
            { core_node_name: 'ALN_005', sequence: 3, kind: 'front',
              claim: { swap_mode: 'single_robot', payload_code: 'ASSY',
                       inbound_staging: 'SLN_007', outbound_staging: 'SLN_008',
                       inbound_source: 'SYN_COMP', outbound_destination: 'SYN_COMP' } },
        ],
        staging: [
            { core_node_name: 'SLN_003', partner_of: 'ALN_003', field: 'inbound_staging' },
            { core_node_name: 'SLN_004', partner_of: 'ALN_003', field: 'outbound_staging' },
            { core_node_name: 'SLN_005', partner_of: 'ALN_004', field: 'inbound_staging' },
            { core_node_name: 'SLN_006', partner_of: 'ALN_004', field: 'outbound_staging' },
            { core_node_name: 'SLN_007', partner_of: 'ALN_005', field: 'inbound_staging' },
            { core_node_name: 'SLN_008', partner_of: 'ALN_005', field: 'outbound_staging' },
        ],
    };
}

// THE REFERENCE'S HARD-CASES CELL (section 5 of the design sheet): an index
// pair, a two_robot position sharing SLN_010 with a single_robot one, a
// 4-waypoint route, an unused position, and two offered-but-unused lanes.
// One position carries a 30-character name so the middle-cut rule is
// exercised at every width.
function hardCases() {
    return {
        geometry: false,
        positions: [
            { core_node_name: 'PLN_003', sequence: 1, kind: 'front',
              claim: { swap_mode: 'two_robot_press_index', payload_code: 'PANEL-A',
                       paired_core_node: 'PLN_001',
                       inbound_source: 'SYN_SM_IN', outbound_destination: 'SYN_SM_OUT' } },
            { core_node_name: 'PLN_001', sequence: 2, kind: 'back' },
            { core_node_name: 'PLN_002', sequence: 3, kind: 'front',
              claim: { swap_mode: 'two_robot', payload_code: 'BRKT',
                       inbound_staging: 'SLN_010',
                       inbound_source: 'SYN_SM_IN', outbound_destination: 'SYN_SM_OUT',
                       key_route: ['LM_21', 'LM_22', 'LM_23', 'LM_24'] } },
            { core_node_name: 'PLN_006', sequence: 4, kind: 'front',
              claim: { swap_mode: 'single_robot', payload_code: 'STUD',
                       inbound_staging: 'SLN_010', outbound_staging: 'SLN_012',
                       inbound_source: 'SYN_SM_IN', outbound_destination: 'SYN_SM_OUT' } },
            { core_node_name: 'PLN_004', sequence: 5, kind: 'front' },
            { core_node_name: 'SMN_BUF_LINE4_WELD_CELL_NORTH_02', sequence: 6, kind: 'front',
              claim: { swap_mode: 'single_robot', payload_code: 'BRKT',
                       inbound_staging: 'SMN_BUF_LINE4_STAGE_NORTH_IN_01',
                       outbound_staging: 'SMN_BUF_LINE4_STAGE_NORTH_OUT_1',
                       inbound_source: 'SYN_SM_IN', outbound_destination: 'SYN_SM_OUT' } },
        ],
        staging: [
            { core_node_name: 'SLN_010', partner_of: 'PLN_002', field: 'inbound_staging' },
            { core_node_name: 'SLN_012', partner_of: 'PLN_006', field: 'outbound_staging' },
            { core_node_name: 'SLN_011' },
            { core_node_name: 'SLN_013' },
        ],
    };
}

// NINE POSITIONS in one row: the width at which a row must wrap, with route
// strips and staging riding along. This is the cell the wrap rule was written
// for and no seeded plant reaches (plant A's widest row is six).
function ninePositions() {
    const positions = [];
    const staging = [];
    for (let i = 1; i <= 9; i++) {
        const n = 'PLN_0' + i;
        positions.push({
            core_node_name: n, sequence: i, kind: 'front',
            claim: i % 2 === 1
                ? { swap_mode: 'two_robot', payload_code: 'PART-' + i, inbound_staging: 'SLN_' + i,
                    inbound_source: 'SYN_SM_IN', outbound_destination: 'SYN_SM_OUT',
                    key_route: ['LM_A', 'LM_B'] }
                : { swap_mode: 'two_robot_press_index', payload_code: 'PART-' + i,
                    paired_core_node: i > 1 ? 'PLN_0' + (i - 1) : '',
                    inbound_source: 'SYN_SM_IN', outbound_destination: 'SYN_SM_OUT' },
        });
        staging.push({ core_node_name: 'SLN_' + i, partner_of: n, field: 'inbound_staging' });
    }
    return { geometry: false, positions: positions, staging: staging };
}

// ── the pin ─────────────────────────────────────────────────────────────────
//
// Reads every drawn card off the markup — position cards AND staging cards,
// which cardsOf never covered — with the texts inside each, and asks three
// things of the whole drawing.

// cardsOf returns each card's box and its texts (name, lines, markup
// stripped). The group open tag's attributes come in either order (data-pos /
// data-staging before or after the transform), so the transform is matched on
// its own and the texts read to the group's close.
function cardsOf(svg) {
    const out = [];
    const re = /<g class="(node|stage)[^"]*"[^>]*?transform="translate\(([-\d.]+),([-\d.]+)\)">([\s\S]*?)<\/g>/g;
    let mm;
    while ((mm = re.exec(svg))) {
        const cls = mm[1], x = +mm[2], y = +mm[3], body = mm[4];
        const box = /<rect class="box" width="([\d.]+)" height="([\d.]+)"/.exec(body);
        if (!box) continue;
        const nm = /<text class="nm"[^>]*>([\s\S]*?)<\/text>/.exec(body);
        const lns = [...body.matchAll(/<text class="ln"[^>]*>([\s\S]*?)<\/text>/g)].map(x2 => x2[1]);
        out.push({
            cls: cls, x: x, y: y, w: +box[1], h: +box[2],
            name: nm ? nm[1].replace(/<[^>]+>/g, '') : '',
            lines: lns.map(l => l.replace(/<[^>]+>/g, '')),
        });
    }
    return out;
}

function overlaps(a, b) { return a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h; }

// THE CHARACTER BUDGET, FROM THE CARD'S OWN WIDTH. Card text is fixed type —
// a name at 13 px is about 6.6 units a character, a line at 12 px about 6.1
// (the estimate stagingTextW already uses) — so "the text fits its card" is
// a character count against the card's inner width. Whatever fixed width the
// picture gives a card, its words must be cut to live inside it: that is the
// middle-cut rule, and this is its measurable form.
function fits(text, card, inset, charW) {
    return text.length * charW + inset <= card.w;
}

// expectedCards is how many cards the station draws for a cell: one per
// position, plus one staging slot in every swap module for each staging
// place its claim names that is not itself a position of the cell.
function expectedCards(cell) {
    const positionNames = new Set(cell.positions.map(p => p.core_node_name));
    let n = cell.positions.length;
    for (const p of cell.positions) {
        const c = p.claim;
        if (!c || (c.swap_mode !== 'single_robot' && c.swap_mode !== 'two_robot')) continue;
        for (const f of [c.inbound_staging, c.outbound_staging]) {
            if (f && !positionNames.has(f)) n++;
        }
    }
    return n;
}

function pinFrame(label, cell, frame) {
    const w = frame ? frame.w : 1280;
    const o = frame ? { frame: frame } : {};
    const svg = m.renderFlowPicture({ cell: cell, station: { name: 'PIN' } }, o);
    const h = o.height;
    const cards = cardsOf(svg);
    // Shared staging is drawn in every module that names it, and staging the
    // flow does not use is drawn only on the desktop, so the count follows the
    // claims rather than the cell's list of staging places.
    check(label + ': every position and staging card is drawn',
        cards.length === expectedCards(cell),
        'drew ' + cards.length + ', wanted ' + expectedCards(cell));
    // The picture reports its own height and the station panel scrolls when it
    // is taller than 560, never scales, so a frame is width by reported height.
    const bottom = Math.max(0, ...cards.map(c => c.y + c.h));
    check(label + ': reported height ' + h + ' holds every card',
        typeof h === 'number' && isFinite(h) && h >= bottom, 'height ' + h + ', lowest card bottom ' + bottom);
    for (const c of cards) {
        check(label + ': ' + (c.name || c.cls) + ' inside ' + w + 'x' + h,
            c.x >= 0 && c.y >= 0 && c.x + c.w <= w + 0.5 && c.y + c.h <= h + 0.5,
            JSON.stringify(c) + ' of ' + w + 'x' + h);
    }
    for (let i = 0; i < cards.length; i++) {
        for (let j = i + 1; j < cards.length; j++) {
            check(label + ': ' + (cards[i].name || '?') + ' / ' + (cards[j].name || '?') + ' disjoint at ' + w,
                !overlaps(cards[i], cards[j]),
                JSON.stringify(cards[i]) + ' vs ' + JSON.stringify(cards[j]));
        }
    }
    // THE TEXT BUDGET. Position-card insets are 14 (the card's own text x);
    // staging-card insets 12. A cut name must also carry its full name as a
    // tooltip — the cut may hide a distinction the operator needs ('…IN_01'
    // and '…OUT_1' still read apart BECAUSE the cut keeps both ends).
    for (const c of cards) {
        const inset = c.cls === 'stage' ? 12 : 14;
        check(label + ': name "' + c.name + '" fits its ' + c.w + '-wide card at ' + w,
            fits(c.name, c, inset, c.cls === 'stage' ? 6.6 : 6.6), c.name.length + ' chars in ' + c.w);
        if (c.name.includes('…')) {
            check(label + ': cut name "' + c.name + '" carries the full name as a tooltip',
                new RegExp('<title>[^<]*</title><text class="nm"[^>]*>[^<]*' + c.name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).test(svg) ||
                svg.includes('<title>' + cell.positions.map(p => p.core_node_name).find(n => n.length > c.name.length) + '</title>'),
                'no <title> found for the cut name');
        }
        for (const ln of c.lines) {
            check(label + ': line "' + ln + '" fits its ' + c.w + '-wide card at ' + w,
                fits(ln, c, inset, 6.1), ln.length + ' chars in ' + c.w);
        }
    }
    return cards;
}

const widths = [];
for (let w = 640; w <= 1600; w += 40) widths.push({ w: w, h: 560 });
widths.push({ w: 1280, h: 560 }); // the station frame, named so a failure reads

const cells = [
    ['weld-2 schematic', weld2()],
    ['hard cases', hardCases()],
    ['nine positions', ninePositions()],
];

console.log('disjointness, frames and text budgets');
for (const [name, cell] of cells) {
    for (const f of widths) {
        pinFrame(name + ' @ ' + f.w + 'x' + f.h, cell, f);
    }
}

// ── the row words on cells with no coordinates ──────────────────────────────
//
// The words the schematic gave these cells before the module picture, written
// out: every position not Kind "back" is the front row, the Kind "back" ones
// the back row, and a cell of one kind is one row with no word. Not the grid:
// the old schematic wrapped five to a row and so called the nine-position
// cell's last four "back" — a fact about the wrap, not about the press.
console.log('row words without coordinates');
{
    const want = {
        'weld-2 schematic': { ALN_003: '', ALN_004: '', ALN_005: '' },
        'hard cases': {
            PLN_003: 'front', PLN_001: 'back', PLN_002: 'front', PLN_006: 'front', PLN_004: 'front',
            SMN_BUF_LINE4_WELD_CELL_NORTH_02: 'front',
        },
        'nine positions': {
            PLN_01: '', PLN_02: '', PLN_03: '', PLN_04: '', PLN_05: '', PLN_06: '', PLN_07: '', PLN_08: '', PLN_09: '',
        },
    };
    for (const [name, cell] of cells) {
        const got = m.pictureRows(cell);
        check('rows: ' + name + ' keeps its words', JSON.stringify(got) === JSON.stringify(want[name]),
            'got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want[name]));
    }
}

if (failures) { console.log(failures + ' FAILED'); process.exit(1); }
console.log('operator-flow geometry: all checks pass');
