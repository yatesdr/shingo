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
//
// AND THE PICTURE BY RELATION (recipe groups, 2026-10-24), from "the picture
// by relation" below: relationLayout's (c, r) tables over the real cells'
// shapes on invented coordinates, the scope gate, and the relation picture's
// cards, fading, chevrons, tags, band and fallback line, with the same frame,
// overlap and text pins at every width plus the cap and the scroll.

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
    // relationScope and sentencesFromView by typeof, so a tree that predates
    // the relation picture loads and each pin below fails on its own line.
    vm.runInContext(src + '\n__out = { renderFlowPicture, pictureRows, ' +
        'relationScope: typeof relationScope === "function" ? relationScope : undefined, ' +
        'sentencesFromView: typeof sentencesFromView === "function" ? sentencesFromView : undefined };', ctx);
    ctx.__out.M = ctx.window.ComposerModel;
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

// A PRESS WHOSE SWAPS STAGE AT ITS BACK POSITIONS — the Hopkinsville shape,
// on the map. Two-robot swaps stage at the back position behind them; two
// one-robot swaps share one back position as their inbound staging and
// another as their outbound staging, while a third (PLN_07) no claim names
// stands alone as a back card; an index
// pair keeps its on-deck partner and names a back position as the inbound
// staging a changeover uses; and one back position runs a claim of its
// own while also being the staging of the front position before it. Every
// staging position is drawn as its module's slot, the shared one in both
// modules; the one with its own claim is its own module as well.
function pressBackStaging() {
    const front = (n, x, claim) => ({ core_node_name: n, kind: 'front', sequence: x + 1, x: x, y: 0, claim: claim });
    const back = (n, x, claim) => Object.assign({ core_node_name: n, kind: 'back', sequence: x + 1, x: x, y: -1.8 },
        claim ? { claim: claim } : {});
    const io = { inbound_source: 'SYN_SM_IN', outbound_destination: 'SYN_SM_OUT' };
    return {
        geometry: true,
        positions: [
            front('PLN_01', 0, Object.assign({ swap_mode: 'two_robot_press_index', payload_code: 'P1', paired_core_node: 'PLN_00',
                inbound_staging: 'PLN_16' }, io)),
            back('PLN_00', 0),
            back('PLN_16', 1),
            front('PLN_03', 2, Object.assign({ swap_mode: 'two_robot', payload_code: 'P3', inbound_staging: 'PLN_02' }, io)),
            back('PLN_02', 2),
            front('PLN_06', 5, Object.assign({ swap_mode: 'two_robot', payload_code: 'P6', inbound_staging: 'PLN_05',
                key_route: ['LM_1', 'LM_2'] }, io)),
            back('PLN_05', 5),
            front('PLN_09', 8, Object.assign({ swap_mode: 'single_robot', payload_code: 'P9',
                inbound_staging: 'PLN_08', outbound_staging: 'PLN_11' }, io)),
            back('PLN_07', 7),
            back('PLN_08', 8),
            front('PLN_12', 11, Object.assign({ swap_mode: 'single_robot', payload_code: 'P12',
                inbound_staging: 'PLN_08', outbound_staging: 'PLN_11' }, io)),
            back('PLN_11', 11),
            front('PLN_14', 13, Object.assign({ swap_mode: 'two_robot', payload_code: 'P14', inbound_staging: 'PLN_15' }, io)),
            back('PLN_15', 13, Object.assign({ swap_mode: 'two_robot', payload_code: 'P15', inbound_staging: 'SLN_15' }, io)),
        ],
        staging: [{ core_node_name: 'SLN_15', partner_of: 'PLN_15', field: 'inbound_staging' }],
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
        const tag = /<rect class="tagbg" x="([\d.]+)"/.exec(body);
        out.push({
            cls: cls, x: x, y: y, w: +box[1], h: +box[2], tagX: tag ? +tag[1] : null,
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

// expectedCards is how many cards the station draws for a cell: one staging
// slot in every swap module for each staging place it parks at (the inbound
// staging, and a one-robot swap's outbound staging — a position of the cell
// included), one in every press index module for its inbound staging, plus
// one card per position, except a position with no claim of its own that
// some module draws as a slot: that one is the slot alone.
function expectedCards(cell) {
    const slotted = new Set();
    let n = 0;
    for (const p of cell.positions) {
        const c = p.claim;
        if (!c || (c.swap_mode !== 'single_robot' && c.swap_mode !== 'two_robot' &&
            c.swap_mode !== 'two_robot_press_index')) continue;
        const fields = c.swap_mode === 'single_robot' ? [c.inbound_staging, c.outbound_staging] : [c.inbound_staging];
        for (const f of fields) {
            if (f) { n++; slotted.add(f); }
        }
    }
    for (const p of cell.positions) {
        if (!(p.claim && p.claim.swap_mode) && slotted.has(p.core_node_name)) continue;
        n++;
    }
    return n;
}

// THE MODULE PICTURE, ASKED FOR BY NAME (relation: false). These four cells
// are the module picture's, and a changeover cell is drawn by relation unless
// it is: the module picture still draws every cell the scope gate leaves alone
// and every fallback, and these pins are what hold it.
function pinFrame(label, cell, frame) {
    const w = frame ? frame.w : 1280;
    const o = Object.assign({ relation: false }, frame ? { frame: frame } : {});
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
        // A SHARED SLOT'S CAPTION shares its row with the "shared" tag, so its
        // budget is the room between the caption's x and the tag's left edge,
        // at the caption's 11 px (about 5.6 units a character) — and the word
        // is drawn whole: a cut caption word is a field nobody can read.
        if (c.tagX !== null) {
            for (const ln of c.lines) {
                check(label + ': shared caption "' + ln + '" is whole and fits before the tag at ' + w,
                    !ln.includes('…') && ln.length * 5.6 + inset <= c.tagX,
                    ln.length + ' chars from ' + inset + ' to the tag at ' + c.tagX);
            }
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
    ['press staging at back positions', pressBackStaging()],
];

console.log('disjointness, frames and text budgets');
for (const [name, cell] of cells) {
    for (const f of widths) {
        pinFrame(name + ' @ ' + f.w + 'x' + f.h, cell, f);
    }
}

// ── the press whose swaps stage at back positions: what each module holds ──
console.log('press staging at back positions: slots, not cards');
{
    const svg = m.renderFlowPicture({ cell: pressBackStaging(), station: { name: 'PIN' } }, { relation: false });
    const parts = svg.split('<g class="module"').slice(1);
    const moduleOf = pos => parts.find(x => x.includes('<g class="node on" data-pos="' + pos + '"')) || '';
    const slotIn = (part, n) => count(part, new RegExp('<g class="stage on[^"]*" data-staging="' + n + '"', 'g'));
    const card = n => count(svg, new RegExp('<g class="node[^"]*" data-pos="' + n + '"', 'g'));
    for (const [pos, st] of [['PLN_03', 'PLN_02'], ['PLN_06', 'PLN_05'], ['PLN_09', 'PLN_08'], ['PLN_09', 'PLN_11'],
        ['PLN_12', 'PLN_08'], ['PLN_12', 'PLN_11'], ['PLN_14', 'PLN_15']]) {
        check('press: ' + pos + "'s module draws " + st + ' as a slot', slotIn(moduleOf(pos), st) === 1);
    }
    // The press index's inbound staging: a slot under the on-deck card, no
    // chevron to it (no running leg reaches it), its tooltip saying what for.
    {
        const idx = moduleOf('PLN_01');
        const deck = /<g class="node on deck[^"]*" data-pos="PLN_00"[^>]*transform="translate\(([-\d.]+),([-\d.]+)\)"/.exec(idx);
        const slot = /<g class="stage on[^"]*" data-staging="PLN_16"[^>]*transform="translate\(([-\d.]+),([-\d.]+)\)">(<title>[^<]*<\/title>)?/.exec(idx);
        check('press: the inbound staging PLN_16 of PLN_01 is a slot under its on-deck card',
            !!deck && !!slot && +slot[2] >= +deck[2] + 56, JSON.stringify({ deck: deck && deck.slice(1), slot: slot && slot.slice(1) }));
        check('press: no move joins PLN_16', !/data-(from|to)="PLN_16"/.test(svg));
        check('press: the tooltip of PLN_16 says it is used at changeover',
            !!slot && slot[3] === '<title>PLN_16 — inbound staging, used at changeover</title>', slot && slot[3]);
    }
    for (const n of ['PLN_02', 'PLN_05', 'PLN_08', 'PLN_11', 'PLN_16']) {
        check('press: ' + n + ', staging with no claim of its own, has no card', card(n) === 0, String(card(n)));
    }
    check('press: PLN_15 runs its own claim, so keeps its own card', card('PLN_15') === 1, String(card('PLN_15')));
    check('press: PLN_07, named by no claim, is a card of its own', card('PLN_07') === 1, String(card('PLN_07')));
    check('press: PLN_08, staged at by two modules, is marked shared in both',
        [moduleOf('PLN_09'), moduleOf('PLN_12')].every(x =>
            /<g class="stage on[^"]*" data-staging="PLN_08"(?:(?!<\/g>).)*>shared</.test(x)));
    check('press: PLN_15, staged at by one module, is not marked shared',
        !/<g class="stage on[^"]*" data-staging="PLN_15"(?:(?!<\/g>).)*>shared</.test(svg));
    check('press: PLN_00 stays on deck inside the module of PLN_01', /class="node on deck[^"]*" data-pos="PLN_00"/.test(moduleOf('PLN_01')));
}

// ── the row words ───────────────────────────────────────────────────────────
//
// The words the schematic gave these cells before the module picture, written
// out: every position not Kind "back" is the front row, the Kind "back" ones
// the back row, and a cell of one kind is one row with no word. Not the grid:
// the old schematic wrapped five to a row and so called the nine-position
// cell's last four "back" — a fact about the wrap, not about the press.
//
// THE MODULE PICTURE'S WORDS (relation: false), the picture these cells were
// pinned on. The relation picture's rows are pinned with its layout below.
console.log('row words');
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
        // ON THE MAP, so the rows are world Y: the same words a staging
        // position had as a card, now that it is drawn as a slot.
        'press staging at back positions': {
            PLN_01: 'front', PLN_00: 'back', PLN_16: 'back', PLN_03: 'front', PLN_02: 'back', PLN_06: 'front', PLN_05: 'back',
            PLN_09: 'front', PLN_07: 'back', PLN_08: 'back', PLN_12: 'front', PLN_11: 'back', PLN_14: 'front', PLN_15: 'back',
        },
    };
    for (const [name, cell] of cells) {
        const got = m.pictureRows(cell, { relation: false });
        check('rows: ' + name + ' keeps its words', JSON.stringify(got) === JSON.stringify(want[name]),
            'got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want[name]));
    }
}

// ── the picture by relation (recipe groups, SYNTH round 2 rows 12-20) ──────
//
// A changeover cell is drawn BY RELATION: each position a card in a grid cell
// of its own, the grid cut from the plan by its widest gaps, nothing to
// scale. relationLayout (composer-model.js) is the rule; these are its
// tables over synthetic cells in the shapes the real ones have, on invented
// round coordinates off an invented origin, and then the picture drawn from
// it: the cards in their cells, the fading, the chevrons on the card doing
// the action, the L/R tag, the lane band, the fallback line, and the frame.

const X0 = 100, Y0 = 200;
const IO = { inbound_source: 'SYN_SM_IN', outbound_destination: 'SYN_SM_OUT' };
const at = (n, seq, x, y, claim, kind) => Object.assign({ core_node_name: n, sequence: seq, kind: kind || 'front',
    x: X0 + x, y: Y0 + y }, claim ? { claim: claim } : {});
const index = (paired, part, second) => Object.assign({ swap_mode: 'two_robot_press_index', payload_code: part || 'SYN-P1',
    paired_core_node: paired }, second ? { second_paired_core_node: second } : {}, IO);
const two = (lane, part) => Object.assign({ swap_mode: 'two_robot', payload_code: part || 'SYN-P2', inbound_staging: lane }, IO);
const single = (lin, lout) => Object.assign({ swap_mode: 'single_robot', payload_code: 'SYN-P3',
    inbound_staging: lin, outbound_staging: lout }, IO);
const seqAB = (other, part) => Object.assign({ swap_mode: 'sequential', payload_code: part, paired_core_node: other }, IO);

// THE PRESS-6 SHAPE. Eleven positions: three columns of press spots, two of
// them 0.35 m apart and the third across the press; three rows 0.4 m apart;
// an on-deck spot at each end standing between the middle and back rows. The
// running claims index the middle row's outer spots from the two decks.
function press6(claims) {
    const c = claims || { PLN_05: index('PLN_01', 'SYN-P1'), PLN_07: index('PLN_08', 'SYN-P2') };
    return {
        geometry: true,
        positions: [
            at('PLN_01', 1, 0, 0.55, c.PLN_01, 'back'),
            at('PLN_02', 2, 0.6, 0, c.PLN_02), at('PLN_03', 3, 0.95, 0, c.PLN_03), at('PLN_04', 4, 3.5, 0, c.PLN_04),
            at('PLN_05', 5, 0.6, 0.4, c.PLN_05), at('PLN_06', 6, 0.95, 0.4, c.PLN_06), at('PLN_07', 7, 3.5, 0.4, c.PLN_07),
            at('PLN_08', 8, 4.5, 0.6, c.PLN_08, 'back'),
            at('PLN_09', 9, 0.6, 0.8, c.PLN_09), at('PLN_10', 10, 0.95, 0.8, c.PLN_10), at('PLN_11', 11, 3.5, 0.8, c.PLN_11),
        ],
    };
}
const PRESS6 = {
    PLN_01: [0, 1], PLN_02: [1, 2], PLN_03: [2, 2], PLN_04: [3, 2], PLN_05: [1, 1], PLN_06: [2, 1],
    PLN_07: [3, 1], PLN_08: [4, 1], PLN_09: [1, 0], PLN_10: [2, 0], PLN_11: [3, 0],
};

// THE HOPKINSVILLE-PRESS SHAPE. Two mirrored halves: a north press spot over a
// back-line spot each side, and an on-deck spot beyond each end just off the
// back line. Two jobs over the same positions: a 2-Robot Index from the decks,
// and a 2-Robot Swap staging at them.
function hkPress(job) {
    const c = job === 'swap'
        ? { PLN_06: two('PLN_05'), PLN_03: two('PLN_02') }
        : { PLN_04: index('PLN_05', 'SYN-P4'), PLN_01: index('PLN_02', 'SYN-P5') };
    return {
        geometry: true,
        positions: [
            at('PLN_05', 1, 0, 0.15, c.PLN_05, 'back'), at('PLN_01', 2, 5.0, 0, c.PLN_01), at('PLN_03', 3, 4.5, 1.3, c.PLN_03),
            at('PLN_02', 4, 7.5, 0.15, c.PLN_02, 'back'), at('PLN_04', 5, 2.5, 0, c.PLN_04), at('PLN_06', 6, 3.0, 1.3, c.PLN_06),
        ],
    };
}
const HKPRESS = { PLN_05: [0, 1], PLN_04: [1, 1], PLN_06: [1, 0], PLN_03: [2, 0], PLN_01: [2, 1], PLN_02: [3, 1] };

// A WELD LINE THAT DRIFTS: three spots 2.5 m apart along a line that wanders
// 0.3 m, two lanes the claims stage at (one shared) and one the routing set
// offers that no claim uses.
function weldDrift(claims) {
    const c = claims || { ALN_01: two('SLN_01'), ALN_02: two('SLN_02'), ALN_03: two('SLN_02') };
    return {
        geometry: true,
        positions: [at('ALN_01', 1, 0, 0, c.ALN_01), at('ALN_02', 2, 2.5, 0.15, c.ALN_02), at('ALN_03', 3, 5.0, 0.3, c.ALN_03)],
        staging: [
            { core_node_name: 'SLN_01', serves: ['ALN_01'] },
            { core_node_name: 'SLN_02', serves: ['ALN_02', 'ALN_03'] },
            { core_node_name: 'SLN_03', serves: [] },
        ],
    };
}

// A NORTH-SOUTH LINE OF FIVE (the Hopkinsville weld cell's shape): 1.5 m
// apart north to south, their x drifting out of order, so a sort by world X
// would scramble them.
function nsLine() {
    return {
        geometry: true,
        positions: [
            at('ALN_01', 1, 0.2, 6, two('SLN_01')), at('ALN_02', 2, 0.4, 4.5, two('SLN_01')),
            at('ALN_03', 3, 0, 3, two('SLN_02')), at('ALN_04', 4, 0.3, 1.5, two('SLN_02')),
            at('ALN_05', 5, 0.1, 0, two('SLN_03')),
        ],
        staging: [
            { core_node_name: 'SLN_01', serves: ['ALN_01', 'ALN_02'] },
            { core_node_name: 'SLN_02', serves: ['ALN_03', 'ALN_04'] },
            { core_node_name: 'SLN_03', serves: ['ALN_05'] },
        ],
    };
}

// A 45-DEGREE DIAGONAL, north-east end first in sequence.
function diagonal() {
    return {
        geometry: true,
        positions: [at('ALN_01', 1, 3, 3, two('SLN_01')), at('ALN_02', 2, 2, 2, two('SLN_01')),
            at('ALN_03', 3, 1, 1, two('SLN_01')), at('ALN_04', 4, 0, 0, two('SLN_01'))],
        staging: [{ core_node_name: 'SLN_01', serves: ['ALN_01', 'ALN_02', 'ALN_03', 'ALN_04'] }],
    };
}

// A ROW OF N spots 2 m apart, each staging at a lane of its own: the cap
// (six fit the HMI's 1240) and the scroll (seven do not, and nothing shrinks).
function rowOf(n) {
    const positions = [], staging = [];
    for (let i = 1; i <= n; i++) {
        const name = 'ALN_0' + i;
        positions.push(at(name, i, 2 * i, 0, two('SLN_0' + i, 'SYN-P' + i)));
        staging.push({ core_node_name: 'SLN_0' + i, serves: [name] });
    }
    return { geometry: true, positions: positions, staging: staging };
}

const L = m.M && typeof m.M.relationLayout === 'function'
    ? (cell, o) => m.M.relationLayout(cell, o)
    : () => { throw new Error('composer-model.js has no relationLayout'); };
function tryL(cell, o) { try { return L(cell, o); } catch (e) { return { error: e.message, cells: {}, band: [] }; } }

// table compares a layout's cells to an expected {node: [c, r]}.
function table(label, got, want, ncols, nrows) {
    const have = {};
    for (const n of Object.keys(got.cells || {})) have[n] = [got.cells[n].c, got.cells[n].r];
    check(label + ': (c, r) as drawn', JSON.stringify(have, Object.keys(want).sort()) === JSON.stringify(want, Object.keys(want).sort()) &&
        Object.keys(have).length === Object.keys(want).length,
        got.error || ('got ' + JSON.stringify(have)));
    check(label + ': ' + ncols + ' x ' + nrows, got.ncols === ncols && got.nrows === nrows, got.ncols + ' x ' + got.nrows);
    check(label + ': no fallback', got.fallback === '', JSON.stringify(got.fallback));
}

// oneCardPerCell is the rule's whole promise: no two positions share a cell,
// every position has one, and no column or row is empty.
function oneCardPerCell(label, cell, got) {
    const seen = {};
    let clash = '';
    for (const p of cell.positions) {
        const g = got.cells && got.cells[p.core_node_name];
        if (!g) { clash = clash || p.core_node_name + ' has no cell'; continue; }
        const k = g.c + ',' + g.r;
        if (seen[k]) clash = clash || p.core_node_name + ' shares ' + k + ' with ' + seen[k];
        seen[k] = p.core_node_name;
    }
    const cols = new Set(Object.values(got.cells || {}).map(g => g.c)), rows = new Set(Object.values(got.cells || {}).map(g => g.r));
    check(label + ': one card per cell, no empty column or row',
        !clash && cols.size === got.ncols && rows.size === got.nrows, got.error || clash || (cols.size + '/' + got.ncols + ' cols, ' + rows.size + '/' + got.nrows + ' rows'));
}

console.log('relation layout: the (c, r) tables');
{
    const p6 = tryL(press6());
    table('press 6 shape', p6, PRESS6, 5, 3);
    oneCardPerCell('press 6 shape', press6(), p6);
    check('press 6 shape: not turned', p6.turned === false, String(p6.turned));
    // THE DECK RULE (the lead's D11): the pairing is the caller's when given
    // and held fixed; a deck whose presses stand in one row takes that row; one
    // whose presses stand in several takes the band rule (the middle row here).
    const low = tryL(press6(), { pairs: { PLN_01: ['PLN_02'] } });
    check('press 6 shape: a deck paired with a front-row spot takes the front row',
        low.cells && low.cells.PLN_01 && low.cells.PLN_01.c === 0 && low.cells.PLN_01.r === 2, JSON.stringify(low.cells && low.cells.PLN_01));
    const wide = tryL(press6(), { pairs: { PLN_01: ['PLN_02', 'PLN_05'], PLN_08: ['PLN_07'] } });
    check('press 6 shape: a deck feeding two rows takes the band rule (middle)',
        wide.cells && wide.cells.PLN_01 && wide.cells.PLN_01.r === 1, JSON.stringify(wide.cells && wide.cells.PLN_01));
    // WITH NO PAIRING AT ALL a spot alone in its column takes the row nearer
    // the middle when it stands 30-70 % of the way between two: the same grid.
    const bare = press6({});
    table('press 6 shape, no claims', tryL(bare), PRESS6, 5, 3);
    // The claims' own pairing when no pairs are passed: a deck the running
    // claim pairs with a back-row spot follows it.
    table('press 6 shape, the claim pairs PLN_01 with PLN_02', tryL(press6({ PLN_02: index('PLN_01') })),
        Object.assign({}, PRESS6, { PLN_01: [0, 2] }), 5, 3);
    // A ROW CHANGE THAT WOULD PUT TWO POSITIONS IN ONE CELL IS NOT MADE: a deck
    // standing behind its own press spot, in its column, stays behind it.
    const behind = { geometry: true, positions: [at('PLN_01', 1, 0, 0, index('PLN_02')), at('PLN_02', 2, 0, -0.5, null, 'back'),
        at('PLN_03', 3, 2, -0.5, index('PLN_04')), at('PLN_04', 4, 2, -1.0, null, 'back')] };
    table('a deck behind its own spot', tryL(behind), { PLN_01: [0, 0], PLN_02: [0, 1], PLN_03: [1, 1], PLN_04: [1, 2] }, 2, 3);

    const hkI = tryL(hkPress('index')), hkS = tryL(hkPress('swap'));
    table('hopkinsville press, 2-Robot Index', hkI, HKPRESS, 4, 2);
    table('hopkinsville press, 2-Robot Swap', hkS, HKPRESS, 4, 2);
    oneCardPerCell('hopkinsville press', hkPress('index'), hkI);
    check('hopkinsville press: PLN_02 in the same cell under both jobs',
        !!(hkI.cells && hkI.cells.PLN_02) && JSON.stringify(hkI.cells.PLN_02) === JSON.stringify(hkS.cells && hkS.cells.PLN_02),
        JSON.stringify([hkI.cells && hkI.cells.PLN_02, hkS.cells && hkS.cells.PLN_02]));

    const wd = tryL(weldDrift());
    table('drifting weld line', wd, { ALN_01: [0, 0], ALN_02: [1, 0], ALN_03: [2, 0] }, 3, 1);
    check('drifting weld line: the band, each lane under the spots that use it, the offered one last',
        JSON.stringify(wd.band) === JSON.stringify([{ node: 'SLN_01', c: 0, offered: false },
            { node: 'SLN_02', c: 1.5, offered: false }, { node: 'SLN_03', c: 2.1, offered: true }]), JSON.stringify(wd.band));
    // Two lanes that would touch are pushed right.
    const crowd = weldDrift({ ALN_01: single('SLN_01', 'SLN_02') });
    crowd.staging = [{ core_node_name: 'SLN_01', serves: ['ALN_01'] }, { core_node_name: 'SLN_02', serves: ['ALN_01'] }];
    check('two lanes under one spot: the second pushed right',
        JSON.stringify(tryL(crowd).band) === JSON.stringify([{ node: 'SLN_01', c: 0, offered: false }, { node: 'SLN_02', c: 0.6, offered: false }]),
        JSON.stringify(tryL(crowd).band));
    // THE BAND READS serves AND NOTHING ELSE: a staging entry whose partner_of
    // names a spot but whose serves is empty is an offer.
    const po = weldDrift();
    po.staging = [{ core_node_name: 'SLN_01', partner_of: 'ALN_01', field: 'inbound_staging' }];
    check('the band reads serves, never partner_of',
        JSON.stringify(tryL(po).band) === JSON.stringify([{ node: 'SLN_01', c: 0, offered: true }]), JSON.stringify(tryL(po).band));

    const ns = tryL(nsLine());
    table('north-south line of five', ns, { ALN_01: [0, 0], ALN_02: [1, 0], ALN_03: [2, 0], ALN_04: [3, 0], ALN_05: [4, 0] }, 5, 1);
    check('north-south line: turned, north on the left', ns.turned === true, String(ns.turned));
    check('north-south line: SLN_01 under ALN_01/02, SLN_02 under 03/04, SLN_03 under 05',
        JSON.stringify(ns.band) === JSON.stringify([{ node: 'SLN_01', c: 0.5, offered: false },
            { node: 'SLN_02', c: 2.5, offered: false }, { node: 'SLN_03', c: 4, offered: false }]), JSON.stringify(ns.band));

    const dg = tryL(diagonal());
    table('45-degree diagonal', dg, { ALN_01: [0, 0], ALN_02: [1, 0], ALN_03: [2, 0], ALN_04: [3, 0] }, 4, 1);
    check('45-degree diagonal: turned', dg.turned === true, String(dg.turned));

    // THE FALLBACK: one position off the map (geometry false, as the server
    // sends it then), or two on one point. No grid; the sentence.
    const off = press6();
    delete off.positions[7].x; delete off.positions[7].y;
    off.geometry = false;
    const fo = tryL(off);
    check('fallback: a position off the map', fo.fallback === 'Shown in order: PLN_08 is not on the plant map.' &&
        Object.keys(fo.cells).length === 0 && fo.ncols === 0, fo.error || JSON.stringify(fo));
    const off2 = press6();
    for (const i of [6, 7]) { delete off2.positions[i].x; delete off2.positions[i].y; }
    off2.geometry = false;
    check('fallback: two off the map', tryL(off2).fallback === 'Shown in order: PLN_07 and PLN_08 are not on the plant map.',
        JSON.stringify(tryL(off2).fallback));
    const none = press6();
    for (const p of none.positions) { delete p.x; delete p.y; }
    none.geometry = false;
    check('fallback: a cell with no position on the map', tryL(none).fallback === 'Shown in order: this cell is not on the plant map.',
        JSON.stringify(tryL(none).fallback));
    const point = press6();
    point.positions[8].x = point.positions[9].x;
    point.positions[8].y = point.positions[9].y;
    check('fallback: two positions on one point', tryL(point).fallback ===
        'Shown in order: PLN_09 and PLN_10 stand on one point of the plant map.', JSON.stringify(tryL(point).fallback));
    for (const [name, cell] of [['drifting weld line', weldDrift()], ['north-south line', nsLine()],
        ['diagonal', diagonal()], ['hopkinsville swap', hkPress('swap')], ['seven in a row', rowOf(7)]]) {
        oneCardPerCell(name, cell, tryL(cell));
    }
}

// ── the scope gate (SYNTH row 17) ───────────────────────────────────────────
console.log('relation picture: the scope gate');
{
    const scope = m.relationScope || (() => { throw new Error('operator-flow.js exports no relationScope'); });
    const tryScope = c => { try { return scope(c); } catch (e) { return e.message; } };
    check('gate: a press-index cell is drawn by relation', tryScope(press6()) === true, String(tryScope(press6())));
    check('gate: a two-robot weld cell is drawn by relation', tryScope(weldDrift()) === true);
    check('gate: a sequential cell is drawn by relation',
        tryScope(weldDrift({ ALN_01: seqAB('ALN_02', 'A'), ALN_02: seqAB('ALN_01', 'B') })) === true);
    check('gate: a cell with no claim keeps the module picture', tryScope(press6({})) === false, String(tryScope(press6({}))));
    check('gate: a legacy manual_swap / simple claim keeps the module picture',
        tryScope(press6({ PLN_02: { swap_mode: 'manual_swap' }, PLN_03: { swap_mode: 'simple' } })) === false);
    const plain = m.renderFlowPicture({ cell: press6({}), station: { name: 'PIN' } }, {});
    check('gate: the no-claim cell draws modules and no fallback line',
        plain.includes('<g class="module"') && !plain.includes('Shown in order'));
    check('gate: the no-claim cell draws the same bytes as relation: false',
        plain === m.renderFlowPicture({ cell: press6({}), station: { name: 'PIN' } }, { relation: false }));
    const asked = m.renderFlowPicture({ cell: press6({}), station: { name: 'PIN' } }, { relation: true });
    check('gate: relation: true draws a no-claim cell by relation', !asked.includes('<g class="module"') &&
        count(asked, /<g class="node[^"]*" data-pos=/g) === 11, String(count(asked, /<g class="node[^"]*" data-pos=/g)));
    const forced = m.renderFlowPicture({ cell: press6(), station: { name: 'PIN' } }, { relation: false });
    check('gate: relation: false draws a changeover cell as modules', forced.includes('<g class="module"'));
}

// ── reading the relation picture back ───────────────────────────────────────
//
// parse is a tag-stack reader over the markup (the renderer writes well-formed
// SVG, so a tokenizer is a parser): every card and band slot with its box in
// picture coordinates and what is inside it — texts, chevron pairs, the tag,
// the bar.
function parse(svg) {
    const cards = [];
    const stack = [];
    const re = /<(\/?)([a-zA-Z]+)([^>]*?)(\/?)>|([^<]+)/g;
    let mm;
    const attr = (s, k) => { const a = new RegExp('\\s' + k + '="([^"]*)"').exec(s); return a ? a[1] : null; };
    const tr = s => { const t = /translate\(([-\d.]+)[ ,]([-\d.]+)\)/.exec(attr(s, 'transform') || ''); return t ? [+t[1], +t[2]] : [0, 0]; };
    while ((mm = re.exec(svg))) {
        if (mm[5] !== undefined) {
            const top = stack[stack.length - 1];
            if (top && top.text) top.text.body += mm[5];
            continue;
        }
        const close = mm[1] === '/', tag = mm[2], rest = mm[3], self = mm[4] === '/';
        if (close) { stack.pop(); continue; }
        const parent = stack[stack.length - 1];
        const card = parent ? parent.card : null;
        // A tspan's words are its text's: the text a reader sees is one string.
        const node = { tag: tag, card: card, cv: parent ? parent.cv : null, gtag: parent ? parent.gtag : null,
            text: parent && parent.text ? parent.text : null };
        const cls = attr(rest, 'class') || '';
        if (tag === 'g' && /^(node|stage)\b/.test(cls) && attr(rest, 'transform')) {
            const [x, y] = tr(rest);
            node.card = { cls: cls, kind: cls.split(' ')[0], name: attr(rest, 'data-pos') || attr(rest, 'data-staging'),
                staging: attr(rest, 'data-staging'), x: x, y: y, w: 0, h: 0, texts: [], cvs: [], chips: [], tag: null, bar: null };
            cards.push(node.card);
        } else if (card && tag === 'g' && /^cv\b/.test(cls)) {
            node.cv = { robot: +attr(rest, 'data-robot'), dir: attr(rest, 'data-dir'), of: attr(rest, 'data-of'), pts: [] };
            card.cvs.push(node.cv);
        } else if (card && tag === 'g' && /^gtag\b/.test(cls)) {
            node.gtag = { cls: cls, box: null, text: '' };
            card.tag = node.gtag;
        } else if (card && tag === 'rect') {
            const b = { x: +(attr(rest, 'x') || 0), y: +(attr(rest, 'y') || 0), w: +attr(rest, 'width'), h: +attr(rest, 'height') };
            if (cls === 'box') { card.w = b.w; card.h = b.h; } else if (node.gtag) node.gtag.box = b;
            else if (/^(partbg|needbg|tgtbg)$/.test(cls)) card.chips.push(b);
        } else if (card && tag === 'path' && node.cv) {
            node.cv.pts.push(tr(rest));
        } else if (card && tag === 'path' && /^press\b/.test(cls)) {
            card.bar = /M([-\d.]+) ([-\d.]+)H([-\d.]+)/.exec(attr(rest, 'd') || '');
        } else if (card && tag === 'text') {
            node.text = { cls: cls, x: +attr(rest, 'x'), y: +attr(rest, 'y'), body: '', anchor: attr(rest, 'text-anchor') };
            if (node.gtag) node.gtag.textNode = node.text; else card.texts.push(node.text);
        }
        if (!self) stack.push(node);
    }
    for (const c of cards) for (const t of c.texts) t.body = t.body.replace(/&amp;/g, '&');
    return cards;
}
// cvBox is a chevron pair's box in its card: each chevron's tip is its
// translate, its arms 6 back along the direction and 5 to either side.
const DV = { r: [1, 0], l: [-1, 0], d: [0, 1], u: [0, -1] };
function cvBox(v) {
    const d = DV[v.dir] || [1, 0];
    let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
    for (const [px, py] of v.pts) {
        for (const [ax, ay] of [[px, py], [px - 6 * d[0], py - 6 * d[1]]]) {
            const ex = d[0] ? 0 : 5, ey = d[1] ? 0 : 5;
            x0 = Math.min(x0, ax - ex); x1 = Math.max(x1, ax + ex); y0 = Math.min(y0, ay - ey); y1 = Math.max(y1, ay + ey);
        }
    }
    return { x: x0, y: y0, w: x1 - x0, h: y1 - y0 };
}
const nodeCard = (cards, n) => cards.find(c => c.kind === 'node' && c.name === n);
const slotOf = (cards, n) => cards.find(c => c.kind === 'stage' && c.staging === n);
const cvs = c => (c ? c.cvs.map(v => v.robot + v.dir).join(' ') : 'no card');

console.log('relation picture: cards in their cells');
{
    const o = {};
    const svg = m.renderFlowPicture({ cell: press6(), station: { name: 'PIN' } }, o);
    const cards = parse(svg);
    const nodes = cards.filter(c => c.kind === 'node');
    check('press 6: every position is a card, no module', nodes.length === 11 && !svg.includes('<g class="module"'),
        nodes.length + ' cards');
    // Columns at 188 + 22 and rows at a fixed pitch: the grid is the layout's,
    // read back off the cards' left edges and their rows' top edges.
    const xOf = c => c, cols = [...new Set(nodes.map(c => c.x))].sort((a, b) => a - b);
    check('press 6: five columns, 210 apart', cols.length === 5 && cols.every((x, i) => !i || x - cols[i - 1] === 210), JSON.stringify(cols));
    for (const [n, cr] of Object.entries(PRESS6)) {
        const c = nodeCard(cards, n);
        check('press 6: ' + n + ' in column ' + cr[0], !!c && c.x === cols[cr[0]], c && String(c.x));
    }
    const top = n => { const c = nodeCard(cards, n); return c ? c.y - (c.h === 76 ? 18 : 0) : NaN; };
    check('press 6: rows top to bottom, the decks in the middle row',
        top('PLN_09') < top('PLN_05') && top('PLN_05') < top('PLN_02') && top('PLN_01') === top('PLN_05') && top('PLN_08') === top('PLN_07'),
        [top('PLN_09'), top('PLN_05'), top('PLN_02'), top('PLN_01'), top('PLN_08')].join(' '));
    check('press 6: a press card is 112 high, an on-deck card 76 (the chevron row)',
        nodeCard(cards, 'PLN_05').h === 112 && nodeCard(cards, 'PLN_01').h === 76 && nodeCard(cards, 'PLN_02').h === 112,
        [nodeCard(cards, 'PLN_05').h, nodeCard(cards, 'PLN_01').h].join(' '));
    check('press 6: reports its height and its width', o.height > Math.max(...nodes.map(c => c.y + c.h)) && o.width === 1280,
        o.height + ' x ' + o.width);
    check('press 6: the card words are the model\'s', nodeCard(cards, 'PLN_05').texts.some(t => t.body === 'Robot 1 supplies PLN_01') &&
        nodeCard(cards, 'PLN_01').texts.some(t => t.body === 'on deck for PLN_05'),
        JSON.stringify(nodeCard(cards, 'PLN_05').texts.map(t => t.body)));
    check('press 6: the part chip on the press card', nodeCard(cards, 'PLN_05').texts.some(t => t.cls === 'partlbl' && t.body === 'SYN-P1'));
    check('press 6: the dock and the legend stay', svg.includes('<g class="dock">') && svg.includes('class="lgt"'));
}

console.log('relation picture: fading');
{
    const cards = parse(m.renderFlowPicture({ cell: press6(), station: { name: 'PIN' } }, {}));
    const faint = n => / faint\b/.test((nodeCard(cards, n) || { cls: 'missing' }).cls);
    // ABSENT active: every node a claim on the picture names is drawn full.
    check('fading: with no active list, the running claims\' nodes are full',
        ['PLN_01', 'PLN_05', 'PLN_07', 'PLN_08'].every(n => !faint(n)), cards.map(c => c.name + ':' + c.cls).join(' | '));
    check('fading: every other card is faint', ['PLN_02', 'PLN_03', 'PLN_04', 'PLN_06', 'PLN_09', 'PLN_10', 'PLN_11'].every(faint));
    check('fading: faint is its own class; off still means no claim',
        /\boff\b/.test(nodeCard(cards, 'PLN_06').cls) && !/\boff\b/.test(nodeCard(cards, 'PLN_05').cls));
    const one = parse(m.renderFlowPicture({ cell: press6(), station: { name: 'PIN' } },
        { active: ['PLN_05', 'PLN_01'], targets: ['PLN_06'], selected: 'PLN_05', selectCard: true }));
    const f1 = n => / faint\b/.test(nodeCard(one, n).cls);
    check('fading: active names what is full', !f1('PLN_05') && !f1('PLN_01') && f1('PLN_07') && f1('PLN_08'));
    check('fading: a target is full, outlined and says "move here"', !f1('PLN_06') && /\btgt\b/.test(nodeCard(one, 'PLN_06').cls) &&
        nodeCard(one, 'PLN_06').texts.some(t => t.body === 'move here'));
    check('fading: the selection is outlined', /\bsel\b/.test(nodeCard(one, 'PLN_05').cls) && !/\bsel\b/.test(nodeCard(one, 'PLN_01').cls));
    const band = parse(m.renderFlowPicture({ cell: weldDrift(), station: { name: 'PIN' } }, { active: ['ALN_01', 'SLN_01'] }));
    check('fading: band lanes fade like cards', !/ faint\b/.test(slotOf(band, 'SLN_01').cls) && / faint\b/.test(slotOf(band, 'SLN_02').cls));
}

console.log('relation picture: chevrons on the card doing the action');
{
    const cards = parse(m.renderFlowPicture({ cell: press6(), station: { name: 'PIN' } }, {}));
    // 2-Robot Index: the deck card Robot 1 up and Robot 2 toward its press
    // spot; the press card one Robot 2 from the deck's side.
    check('2-Robot Index: the west deck carries Robot 1 up and Robot 2 right', cvs(nodeCard(cards, 'PLN_01')) === '1u 2r', cvs(nodeCard(cards, 'PLN_01')));
    check('2-Robot Index: its press spot one Robot 2, from the deck\'s side', cvs(nodeCard(cards, 'PLN_05')) === '2r', cvs(nodeCard(cards, 'PLN_05')));
    check('2-Robot Index: the east deck carries Robot 1 up and Robot 2 left', cvs(nodeCard(cards, 'PLN_08')) === '1u 2l', cvs(nodeCard(cards, 'PLN_08')));
    check('2-Robot Index: its press spot Robot 2 from the east', cvs(nodeCard(cards, 'PLN_07')) === '2l', cvs(nodeCard(cards, 'PLN_07')));
    check('chevrons: an idle card has none', cvs(nodeCard(cards, 'PLN_06')) === '', cvs(nodeCard(cards, 'PLN_06')));
    const none = parse(m.renderFlowPicture({ cell: press6(), station: { name: 'PIN' } }, { chevrons: [] }));
    check('chevrons: [] draws none', none.every(c => c.cvs.length === 0));
    const left = parse(m.renderFlowPicture({ cell: press6(), station: { name: 'PIN' } }, { chevrons: ['PLN_05'] }));
    check('chevrons: only the named claim\'s', cvs(nodeCard(left, 'PLN_01')) === '1u 2r' && cvs(nodeCard(left, 'PLN_08')) === '');
    // A deck feeding the spot above it in its column: vertical. One a column
    // over and a row down is a tie, and a tie is horizontal.
    const up = parse(m.renderFlowPicture({ cell: press6({ PLN_09: index('PLN_05') }), station: { name: 'PIN' } }, {}));
    check('2-Robot Index: a deck below its spot sends Robot 2 up', cvs(nodeCard(up, 'PLN_05')) === '1u 2u' && cvs(nodeCard(up, 'PLN_09')) === '2u',
        cvs(nodeCard(up, 'PLN_05')) + ' / ' + cvs(nodeCard(up, 'PLN_09')));
    const tie = parse(m.renderFlowPicture({ cell: press6({ PLN_09: index('PLN_06') }), station: { name: 'PIN' } }, {}));
    check('2-Robot Index: a deck a column over and a row down sends Robot 2 sideways', cvs(nodeCard(tie, 'PLN_06')) === '1u 2l' &&
        cvs(nodeCard(tie, 'PLN_09')) === '2l', cvs(nodeCard(tie, 'PLN_06')) + ' / ' + cvs(nodeCard(tie, 'PLN_09')));
    // ONE DECK, TWO PRESS SPOTS: a recipe group's on-deck spot feeds each of
    // its group's spots, and the desktop draws the group with a claim on
    // every one of them. Each press card carries its own Robot 2; the deck's
    // Robot 2 toward them is one pair while they stand on one side of it.
    const shared = parse(m.renderFlowPicture({ cell: press6({ PLN_05: index('PLN_01'), PLN_09: index('PLN_01'),
        PLN_07: index('PLN_08', 'SYN-P2') }), station: { name: 'PIN' } }, {}));
    check('2-Robot Index: every press spot one deck feeds carries its Robot 2',
        cvs(nodeCard(shared, 'PLN_05')) === '2r' && cvs(nodeCard(shared, 'PLN_09')) === '2r' && cvs(nodeCard(shared, 'PLN_01')) === '1u 2r',
        [cvs(nodeCard(shared, 'PLN_05')), cvs(nodeCard(shared, 'PLN_09')), cvs(nodeCard(shared, 'PLN_01'))].join(' / '));
    const one9 = parse(m.renderFlowPicture({ cell: press6({ PLN_05: index('PLN_01'), PLN_09: index('PLN_01') }),
        station: { name: 'PIN' } }, { chevrons: ['PLN_09'] }));
    check('2-Robot Index: the second spot a deck feeds draws alone, its deck\'s moves with it',
        cvs(nodeCard(one9, 'PLN_09')) === '2r' && cvs(nodeCard(one9, 'PLN_01')) === '1u 2r' && cvs(nodeCard(one9, 'PLN_05')) === '',
        [cvs(nodeCard(one9, 'PLN_09')), cvs(nodeCard(one9, 'PLN_01')), cvs(nodeCard(one9, 'PLN_05'))].join(' / '));
    // With a third: the third carries Robot 1 up and Robot 2 toward the deck,
    // and is drawn as a card of the deck's height in its own cell.
    const third = parse(m.renderFlowPicture({ cell: press6({ PLN_05: index('PLN_01', 'SYN-P1', 'PLN_09') }), station: { name: 'PIN' } }, {}));
    check('2-Robot Index with a third: the third carries Robot 1 up and Robot 2 toward the deck',
        cvs(nodeCard(third, 'PLN_09')) === '1u 2l' && nodeCard(third, 'PLN_09').h === 76, cvs(nodeCard(third, 'PLN_09')));
    // 2-Robot Swap staging at back positions: the lane card Robot 1 up, the
    // spot Robot 1 from the lane's side.
    const hk = parse(m.renderFlowPicture({ cell: hkPress('swap'), station: { name: 'PIN' } }, {}));
    check('2-Robot Swap: the lane card carries Robot 1 up', cvs(nodeCard(hk, 'PLN_05')) === '1u', cvs(nodeCard(hk, 'PLN_05')));
    check('2-Robot Swap: the spot Robot 1 from the lane\'s side (a tie is horizontal)',
        cvs(nodeCard(hk, 'PLN_06')) === '1r' && cvs(nodeCard(hk, 'PLN_03')) === '1l', cvs(nodeCard(hk, 'PLN_06')) + ' / ' + cvs(nodeCard(hk, 'PLN_03')));
    // ...and with the lane in the band: up from under the grid.
    const wd = parse(m.renderFlowPicture({ cell: weldDrift(), station: { name: 'PIN' } }, {}));
    check('2-Robot Swap: a band lane carries Robot 1 up, its spots Robot 1 up from it',
        cvs(slotOf(wd, 'SLN_02')) === '1u' && cvs(nodeCard(wd, 'ALN_02')) === '1u' && cvs(nodeCard(wd, 'ALN_03')) === '1u',
        cvs(slotOf(wd, 'SLN_02')) + ' / ' + cvs(nodeCard(wd, 'ALN_02')));
    check('a band lane no claim uses carries none', cvs(slotOf(wd, 'SLN_03')) === '');
    // 1-Robot Swap: in-lane Robot 1 up; the spot from the in-lane's side; the
    // out-lane from the spot's side.
    const one = weldDrift({ ALN_02: single('SLN_01', 'SLN_02') });
    one.staging = [{ core_node_name: 'SLN_01', serves: ['ALN_02'] }, { core_node_name: 'SLN_02', serves: ['ALN_02'] }];
    const s1 = parse(m.renderFlowPicture({ cell: one, station: { name: 'PIN' } }, {}));
    check('1-Robot Swap: in-lane up, spot up from it, out-lane down from the spot',
        cvs(slotOf(s1, 'SLN_01')) === '1u' && cvs(nodeCard(s1, 'ALN_02')) === '1u' && cvs(slotOf(s1, 'SLN_02')) === '1d',
        [cvs(slotOf(s1, 'SLN_01')), cvs(nodeCard(s1, 'ALN_02')), cvs(slotOf(s1, 'SLN_02'))].join(' / '));
    // Sequential A/B: the B card Robot 1 up; A no chevron.
    const sq = parse(m.renderFlowPicture({ cell: weldDrift({ ALN_01: seqAB('ALN_02', 'A'), ALN_02: seqAB('ALN_01', 'B') }),
        station: { name: 'PIN' } }, {}));
    check('Sequential A/B: B carries Robot 1 up, A none', cvs(nodeCard(sq, 'ALN_02')) === '1u' && cvs(nodeCard(sq, 'ALN_01')) === '',
        cvs(nodeCard(sq, 'ALN_02')) + ' / ' + cvs(nodeCard(sq, 'ALN_01')));
    // ROBOT COLOUR FROM THE MODEL'S LEGS, never a template: the same cell with
    // the model's index leg made Robot 1's draws the index in Robot 1.
    if (m.sentencesFromView) {
        const view = { cell: press6(), station: { name: 'PIN' } };
        const s = m.sentencesFromView(view);
        s.legs = s.legs.map(l => Object.assign({}, l, { robot: l.kind === 'index' ? 1 : l.robot }));
        const r1 = parse(m.renderFlowPicture(view, { sentences: s }));
        check('chevrons: the robot is the leg\'s', cvs(nodeCard(r1, 'PLN_05')) === '1r' && cvs(nodeCard(r1, 'PLN_01')) === '1u 1r',
            cvs(nodeCard(r1, 'PLN_05')) + ' / ' + cvs(nodeCard(r1, 'PLN_01')));
    } else {
        check('chevrons: the robot is the leg\'s', false, 'operator-flow.js exports no sentencesFromView');
    }
}

console.log('relation picture: the L/R tag');
{
    const cards = parse(m.renderFlowPicture({ cell: press6(), station: { name: 'PIN' } },
        { tags: { PLN_05: { letter: 'L', group: 1 }, PLN_07: { letter: 'R', group: 2 } } }));
    const t5 = nodeCard(cards, 'PLN_05').tag, t7 = nodeCard(cards, 'PLN_07').tag;
    check('tag: L in group 1 on PLN_05, R in group 2 on PLN_07',
        !!t5 && /\bg1\b/.test(t5.cls) && t5.textNode && t5.textNode.body === 'L' && !!t7 && /\bg2\b/.test(t7.cls) && t7.textNode.body === 'R',
        JSON.stringify([t5, t7]));
    check('tag: no tag where none was asked', !nodeCard(cards, 'PLN_01').tag && !nodeCard(cards, 'PLN_06').tag);
}

console.log('relation picture: the fallback line');
{
    const off = press6();
    delete off.positions[7].x; delete off.positions[7].y;
    off.geometry = false;
    const o = {};
    const svg = m.renderFlowPicture({ cell: off, station: { name: 'PIN' } }, o);
    check('fallback: today\'s module picture', svg.includes('<g class="module"'));
    check('fallback: one line in the margin says why', svg.includes('>Shown in order: PLN_08 is not on the plant map.<'),
        (svg.match(/Shown in order[^<]*/) || ['no line'])[0]);
    check('fallback: the modules under it are where relation: false puts them',
        svg.replace(/<text class="fbl"[^>]*>.*?<\/text>/, '') === m.renderFlowPicture({ cell: off, station: { name: 'PIN' } }, { relation: false }));
}

// ── the relation picture's frames ───────────────────────────────────────────
//
// The geometry the module pins hold, held again on the relation picture at
// every width: every card and band slot inside the picture (its own width by
// its own height), no two overlapping, every text inside its card by the
// character budget, every chevron pair inside its card under the card's
// words, the tag on its card's top-left corner inside the room above it.
function pinRelation(label, cell, frame, extra) {
    const o = Object.assign({ frame: frame }, extra || {});
    const svg = m.renderFlowPicture({ cell: cell, station: { name: 'PIN' } }, o);
    const cards = parse(svg);
    const W = o.width, H = o.height;
    check(label + ': drawn by relation', !svg.includes('<g class="module"') && cards.length > 0);
    check(label + ': width ' + W + ' is the frame\'s or wider', typeof W === 'number' && W >= frame.w, String(W));
    let bad = '';
    for (const c of cards) {
        if (!(c.x >= 0 && c.y >= 0 && c.x + c.w <= W + 0.5 && c.y + c.h <= H + 0.5)) bad = bad || c.name + ' outside ' + W + 'x' + H;
        if (c.kind === 'node' && c.w !== CARD_W) bad = bad || c.name + ' is ' + c.w + ' wide';
        const inset = c.kind === 'stage' ? 12 : 14;
        for (const t of c.texts) {
            const charW = t.cls === 'nm' ? 6.6 : 6.1;
            if (t.body.length * charW + (t.anchor === 'middle' ? 2 * 12 : inset) > c.w) bad = bad || c.name + ': "' + t.body + '" overflows';
            if (t.y > c.h || t.y < 0) bad = bad || c.name + ': "' + t.body + '" off its card';
        }
        // The card's words are every text above its chip (the chip's label is
        // the chip's).
        const lastLine = Math.max(0, ...c.texts.filter(t => t.y < (c.chips.length ? c.chips[0].y : c.h)).map(t => t.y));
        const row = [];
        for (const v of c.cvs) {
            const b = cvBox(v);
            row.push(b);
            if (b.x < 0 || b.x + b.w > c.w || b.y < 0 || b.y + b.h > c.h) bad = bad || c.name + ': a chevron pair leaves the card ' + JSON.stringify(b);
            if (b.y < lastLine + 3) bad = bad || c.name + ': a chevron pair sits on the words (' + b.y + ' vs ' + lastLine + ')';
            for (const ch of c.chips) if (b.y + b.h > ch.y) bad = bad || c.name + ': a chevron pair sits on the chip';
        }
        if (row.length) {
            // CENTRED UNDER ITS WORDS, the pairs side by side and never on each other.
            const x0 = Math.min(...row.map(b => b.x)), x1 = Math.max(...row.map(b => b.x + b.w));
            if (Math.abs((x0 + x1) / 2 - c.w / 2) > 1) bad = bad || c.name + ': the chevron row is not centred (' + x0 + '..' + x1 + ')';
            for (let i = 0; i < row.length; i++) for (let j = i + 1; j < row.length; j++) {
                if (overlaps(row[i], row[j])) bad = bad || c.name + ': two chevron pairs overlap';
            }
        }
        if (c.tag && c.tag.box) {
            const b = c.tag.box;
            if (b.x < 0 || b.x + b.w > c.w || b.y < -BAR || b.y + b.h > c.h) bad = bad || c.name + ': the tag leaves its card';
            if (c.bar && b.x + b.w > +c.bar[1] && b.y < +c.bar[2] + 2 && b.y + b.h > +c.bar[2] - 2) bad = bad || c.name + ': the tag sits on the bar';
        }
    }
    check(label + ': every card inside, full width, its words, chevrons and tag inside it', !bad, bad);
    let clash = '';
    for (let i = 0; i < cards.length; i++) {
        for (let j = i + 1; j < cards.length; j++) {
            const a = cards[i], b = cards[j];
            // the room above a card is its bar's and its tag's, so it counts
            const ra = { x: a.x, y: a.y - (a.kind === 'node' ? BAR : 0), w: a.w, h: a.h + (a.kind === 'node' ? BAR : 0) };
            const rb = { x: b.x, y: b.y - (b.kind === 'node' ? BAR : 0), w: b.w, h: b.h + (b.kind === 'node' ? BAR : 0) };
            if (overlaps(ra, rb)) clash = clash || a.name + ' / ' + b.name;
        }
    }
    check(label + ': no two cards overlap', !clash, clash);
    // The dock and the legend stay in the frame the caller shows.
    const dock = /<g class="dock"><line x1="([\d.]+)" y1="([\d.]+)" x2="([\d.]+)"/.exec(svg);
    check(label + ': the dock spans the frame', !!dock && +dock[3] <= frame.w - 20 + 0.5 && +dock[2] >= Math.max(...cards.map(c => c.y + c.h)),
        dock ? dock.slice(1).join(' ') : 'no dock');
    return { o: o, cards: cards };
}

const BAR = 12;
const CARD_W = 188;
console.log('relation picture: frames, overlaps and text budgets');
{
    const relCells = [
        ['press 6 shape', press6()],
        ['press 6 shape, tagged and selected', press6(), { tags: { PLN_05: { letter: 'L', group: 1 }, PLN_07: { letter: 'R', group: 2 } },
            selected: 'PLN_05', selectCard: true, targets: ['PLN_06'], active: ['PLN_05', 'PLN_01'] }],
        ['press 6 shape with a third', press6({ PLN_05: index('PLN_01', 'SYN-P1', 'PLN_09'), PLN_07: index('PLN_08', 'SYN-P2') })],
        ['hopkinsville index', hkPress('index')],
        ['hopkinsville swap', hkPress('swap')],
        ['drifting weld line', weldDrift()],
        ['north-south line', nsLine()],
        ['diagonal', diagonal()],
        ['six in a row', rowOf(6)],
    ];
    for (const [name, cell, extra] of relCells) {
        for (const f of widths) pinRelation(name + ' @ ' + f.w, cell, f, extra);
    }
    // THE CAP AND THE SCROLL (SYNTH row 14): six columns fit the HMI's 1280
    // frame (6 x 188 + 5 x 22 = 1238 of 1240); a seventh makes the picture
    // wider than the frame, and nothing shrinks or wraps.
    const six = pinRelation('cap: six columns', rowOf(6), { w: 1280, h: 560 });
    check('cap: six columns keep the frame\'s 1280', six.o.width === 1280, String(six.o.width));
    const seven = pinRelation('scroll: seven columns', rowOf(7), { w: 1280, h: 560 });
    const sevenNodes = seven.cards.filter(c => c.kind === 'node');
    check('scroll: seven columns make the picture wider, one row of full-size cards',
        seven.o.width === 7 * 188 + 6 * 22 + 40 && sevenNodes.length === 7 && new Set(sevenNodes.map(c => c.y)).size === 1 &&
        sevenNodes.every(c => c.w === 188 && c.h === 112), seven.o.width + ' / ' + sevenNodes.map(c => c.x + ',' + c.y).join(' '));
    const desk = pinRelation('scroll: six columns on the desktop\'s 1084', rowOf(6), { w: 1084, h: 560 });
    check('scroll: the desktop frame scrolls six columns', desk.o.width === 6 * 188 + 5 * 22 + 40, String(desk.o.width));
}

// ── the row words on the relation picture (piece 10) ──────────────────────
console.log('relation picture: the row words');
{
    // A CELL DRAWN BY RELATION HAS NO ROW WORD: which of its rows faces the
    // aisle is not in x and y. Press 6's two lines (PLN_006, 009, 007 and
    // PLN_013, 014, 015; PLN_02-04 and PLN_09-11 here) go without, as does
    // everything between them, and a line.
    const w = m.pictureRows(press6());
    check('rows: press 6 shape drawn by relation has no row word, either line or between',
        Object.keys(w).length === 11 && Object.values(w).every(v => v === ''), JSON.stringify(w));
    const ns = m.pictureRows(nsLine());
    check('rows: a line drawn by relation has no word', Object.values(ns).every(v => v === ''), JSON.stringify(ns));
    // The module picture and the fallback keep their words: world Y where
    // every position has coordinates, Kind where one does not.
    const modRows = m.pictureRows(press6(), { relation: false });
    check('rows: the module picture keeps the world-Y words',
        ['PLN_09', 'PLN_10', 'PLN_11', 'PLN_01', 'PLN_05'].every(n => modRows[n] === 'front') &&
        ['PLN_02', 'PLN_03', 'PLN_04'].every(n => modRows[n] === 'back'), JSON.stringify(modRows));
    const offRows = press6();
    delete offRows.positions[7].x; delete offRows.positions[7].y;
    offRows.geometry = false;
    const fbRows = m.pictureRows(offRows);
    check('rows: the fallback keeps the Kind words',
        fbRows.PLN_01 === 'back' && fbRows.PLN_08 === 'back' &&
        ['PLN_02', 'PLN_05', 'PLN_09', 'PLN_11'].every(n => fbRows[n] === 'front'), JSON.stringify(fbRows));
    // THE IDLE CARDS HAVE NO CAPTION on the relation picture, as the mock draws
    // them: Kind is not the row, and Press 6's back line is Kind "front". The
    // front row, the middle and the back all go without; a named card keeps its
    // words. The module picture and the fallback keep Kind's.
    const words = (cards, n) => (nodeCard(cards, n) || { texts: [] }).texts.filter(t => t.cls === 'ln').map(t => t.body);
    const rel = parse(m.renderFlowPicture({ cell: press6(), station: { name: 'PIN' } }, {}));
    const idle = ['PLN_02', 'PLN_03', 'PLN_04', 'PLN_06', 'PLN_09', 'PLN_10', 'PLN_11'];
    check('captions: no idle card on the relation picture has one, front row, middle or back',
        idle.every(n => words(rel, n).length === 0), idle.map(n => n + ' ' + JSON.stringify(words(rel, n))).join(' '));
    check('captions: a named card keeps its words', words(rel, 'PLN_01').join(' ') === 'on deck for PLN_05',
        JSON.stringify(words(rel, 'PLN_01')));
    const mod = parse(m.renderFlowPicture({ cell: press6(), station: { name: 'PIN' } }, { relation: false }));
    check('captions: the module picture keeps the Kind words', words(mod, 'PLN_09').join(' ') === 'front position',
        JSON.stringify(words(mod, 'PLN_09')));
    const off = press6();
    delete off.positions[7].x; delete off.positions[7].y;
    off.geometry = false;
    const fb = parse(m.renderFlowPicture({ cell: off, station: { name: 'PIN' } }, {}));
    check('captions: the fallback keeps the Kind words', words(fb, 'PLN_09').join(' ') === 'front position',
        JSON.stringify(words(fb, 'PLN_09')));
    // A cell the gate leaves on the module picture keeps the world-Y words:
    // rows clustered within 0.5 m of their first, so the back row stands apart
    // and everything else is the front.
    const plain = m.pictureRows(press6({}));
    check('rows: a no-claim cell keeps the clustered words',
        ['PLN_02', 'PLN_03', 'PLN_04'].every(n => plain[n] === 'back') &&
        ['PLN_01', 'PLN_05', 'PLN_06', 'PLN_07', 'PLN_08', 'PLN_09', 'PLN_10', 'PLN_11'].every(n => plain[n] === 'front'),
        JSON.stringify(plain));
}

if (failures) { console.log(failures + ' FAILED'); process.exit(1); }
console.log('operator-flow geometry: all checks pass');
