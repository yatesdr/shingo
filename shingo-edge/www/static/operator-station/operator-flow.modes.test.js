// operator-flow.modes.test.js — the PER-MODE characterisation of the cell
// picture, run by operator_flow_modes_test.go, which builds one station view
// per swap mode through the real BuildView and hands them over as file paths.
//
// This is the POST-REWRITE record of the module picture: for each of the four
// choreographies, which modules the picture builds, which moves its chevrons
// draw (and in which robot's colour), what each card reads, what the dock
// notes say, whether a route strip or a staging slot is drawn, and which
// robots the legend lists. The words here changed from the pre-change record
// only where the design's owner rulings changed them (see
// PREDICTED-DIFFS-edge-setup-ux-2026-10-05.md §4); every other diff is a
// ruling to cite or a bug to name.

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

// The same harness load() operator-flow.test.js uses: the model in the same
// vm context as the flow module, because every sentence drawn comes from
// composer-model.js.
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
    vm.runInContext(src + '\n__out = { renderFlowPicture, sentencesFromView };', ctx);
    return ctx.__out;
}

const m = load();

// renderOne draws the picture the board draws — read-only, nothing editable,
// no frame (the picture's rest width).
function renderOne(view, opts) { return m.renderFlowPicture(view, opts); }

function count(svg, re) { return (svg.match(re) || []).length; }

// The characterisation, per view. `want` names what each mode draws. Every
// check is on words and presence, not coordinates — the geometry pins live in
// operator-flow.geometry.test.js.
function characterise(label, file, want) {
    console.log(label);
    const view = JSON.parse(fs.readFileSync(file, 'utf8'));
    const svg = renderOne(view);
    // Card lines wrap their robot words in tspans (the words take the robot's
    // colour inside one sentence), so text checks run on the FLATTENED
    // markup — tags stripped the way a reader sees the words.
    const flat = svg.replace(/<tspan[^>]*>/g, '').replace(/<\/tspan>/g, '');

    // ── the moves ───────────────────────────────────────────────────────────
    //
    // One move is one <g class="mv"> holding TWO chevron paths in the trip's
    // robot colour — a pair reads as "this way" from across a cell. The counts
    // are of paths with the move glyph's own "d": the legend draws chevrons
    // too, with a different one, so a legend entry cannot flunk a move count.
    check(label + ': move groups drawn', count(svg, /<g class="mv"[ >]/g) === (want.moves || 0),
        String(count(svg, /<g class="mv"[ >]/g)));
    check(label + ': robot-1 move chevrons', count(svg, /<path class="sc r1" d="M-5/g) === (want.r1Chevrons || 0),
        String(count(svg, /<path class="sc r1" d="M-5/g)));
    check(label + ': robot-2 move chevrons', count(svg, /<path class="sc r2" d="M-5/g) === (want.r2Chevrons || 0),
        String(count(svg, /<path class="sc r2" d="M-5/g)));
    // THE LEG GRAMMAR IS GONE: no leg group, label, tip or dash train may be
    // emitted by the module picture.
    check(label + ': no leg grammar', !/class="leg/.test(svg), '');

    // ── the cards ───────────────────────────────────────────────────────────
    for (const line of want.cardLines || []) {
        check(label + ': a card reads "' + line + '"', flat.includes('>' + line + '<'),
            (flat.match(/class="ln"[^>]*>[^<]*/g) || []).join(' | '));
    }
    for (const line of want.noCardLines || []) {
        check(label + ': no card reads "' + line + '"', !flat.includes('>' + line + '<'));
    }

    // ── the dock ────────────────────────────────────────────────────────────
    // EXACT, on the flattened markup: the robot words in a note are wrapped
    // in their colour, and the note is the whole text element — nothing
    // appended after the last name.
    const notes = (flat.match(/>((Inbound source|Outbound destination)[^<]*)</g) || []).map(t => t.slice(1, -1));
    for (const note of want.dock || []) {
        check(label + ': dock reads "' + note + '"', notes.includes(note), notes.join(' | '));
    }

    // ── the staging slots ───────────────────────────────────────────────────
    check(label + ': staging slots drawn', count(svg, /<g class="stage /g) === (want.stagingSlots || 0),
        String(count(svg, /<g class="stage /g)));
    for (const cap of want.slotCaptions || []) {
        check(label + ': a slot reads "' + cap + '"', svg.includes(cap));
    }
    for (const lane of want.slots || []) {
        check(label + ': a slot is drawn for ' + lane, svg.includes('data-staging="' + lane + '"'));
    }
    for (const lane of want.noSlots || []) {
        check(label + ': no slot is drawn for ' + lane, !svg.includes('data-staging="' + lane + '"'));
    }

    // ── the route strips ────────────────────────────────────────────────────
    check(label + ': route strips drawn', count(svg, /<g class="lmroute /g) === (want.strips || 0),
        String(count(svg, /<g class="lmroute /g)));

    // ── the legend ──────────────────────────────────────────────────────────
    check(label + ': legend entries', count(svg, /<g class="lg" /g) === (want.legend || 0),
        String(count(svg, /<g class="lg" /g)));
    check(label + ': legend lists ' + (want.legendRobots || []).join(' and '),
        (want.legendRobots || []).every(r => svg.includes('>Robot ' + r + '</text>')) &&
        count(svg, /<text class="lgt"[^>]*>Robot /g) === (want.legendRobots || []).length,
        (svg.match(/<text class="lgt"[^>]*>[^<]*/g) || []).join(' | '));
}

const [indexPath, twoRobotPath, singlePath, seqPath, twoLanesPath, mixedPath, indexStagedPath] = process.argv.slice(2);
if (!indexPath || !twoRobotPath || !singlePath || !seqPath || !twoLanesPath || !mixedPath || !indexStagedPath) {
    console.log('usage: node operator-flow.modes.test.js <index-pair.json> <two-robot.json> <single-robot.json> <sequential.json> <two-robot-lanes.json> <mixed.json> <index-staged.json>');
    process.exit(2);
}

// ── two_robot_press_index — the two-row press ───────────────────────────────
// Each produce position draws an INDEX module: the press card, its paired
// back position as the on-deck card inside it, and three moves — the next bin
// in behind the one on deck (Robot 1), the index up into the press (Robot 2),
// the old bin out to the dock (Robot 2). The spare bound positions draw as
// plain cards captioned "back position", as before the rewrite: Kind is a
// fact about the PROCESS, and the synthetic sequential style pairs
// PLN_03/PLN_06. Dock IN names the paired back positions; dock OUT names the
// fronts; the legend lists both robots.
characterise('press index — two rows, two pairs', indexPath, {
    moves: 6, r1Chevrons: 4, r2Chevrons: 8,
    cardLines: ['Robot 1 supplies PLN_02', 'Robot 1 supplies PLN_05', 'Robot 2 indexes',
        'on deck for PLN_01', 'on deck for PLN_04', 'back position'],
    noCardLines: ['Inbound staging', 'Robot 1 moves in', 'Robot 1 clears old', 'front position'],
    dock: ['Inbound source · Robot 1 → PLN_02, PLN_05', 'Outbound destination · Robot 2 ← PLN_01, PLN_04'],
    stagingSlots: 0,
    strips: 0,
    legend: 2, legendRobots: [1, 2],
});

// ── two_robot — stage beside the line, move the new in, pull the old ────────
// Each produce position draws a SWAP module whose staging place IS a position
// of the cell (the Hopkinsville shape). The staging position is drawn as the
// module's inbound slot, like any staging, with Robot 1's move up out of it;
// it has no card of its own, so no card reads "for <press>". The old bin
// leaves for the dock on Robot 2.
characterise('two robot — two staging moves', twoRobotPath, {
    moves: 4, r1Chevrons: 4, r2Chevrons: 4,
    cardLines: ['Robot 1 stages at PLN_02', 'Robot 1 stages at PLN_05', 'Robot 2 pulls old',
        'Inbound staging', 'front position'],
    noCardLines: ['Robot 2 indexes', 'Robot 1 clears old', 'for PLN_03', 'for PLN_06'],
    dock: ['Inbound source · Robot 1 → PLN_02, PLN_05', 'Outbound destination · Robot 2 ← PLN_03, PLN_06'],
    stagingSlots: 2, slots: ['PLN_02', 'PLN_05'],
    strips: 0,
    legend: 2, legendRobots: [1, 2],
});

// ── two_robot at lanes — the slot under the card, Robot 2 out to the dock ────
// The staging places here are lanes, not positions, so the module draws the
// inbound lane as a slot under the card with Robot 1's move up out of it. A
// two-robot swap's old bin goes straight to the dock on Robot 2 — the
// module's out-stub — so the outbound staging the claim names gets no slot
// and no caption: two_robot never parks the old bin, and a slot would draw a
// trip the choreography does not make.
characterise('two robot — staging at lanes, outbound named', twoLanesPath, {
    moves: 2, r1Chevrons: 2, r2Chevrons: 2,
    cardLines: ['Robot 1 stages at SLN_010', 'Robot 2 pulls old', 'Inbound staging'],
    noCardLines: ['Outbound staging', 'Robot 1 clears old'],
    dock: ['Inbound source · Robot 1 → SLN_010', 'Outbound destination · Robot 2 ← PLN_03'],
    stagingSlots: 1, slots: ['SLN_010'], noSlots: ['SLN_04'],
    strips: 0,
    legend: 2, legendRobots: [1, 2],
});

// ── single_robot — the mode with two trips on one robot ─────────────────────
// The card reads the model's two move sentences ("Robot 1 moves in" / "Robot 1
// clears old"). Its staging places are positions of the cell (PLN_02/PLN_05),
// drawn as each module's inbound and outbound slots: both claims stage there,
// so each slot is drawn in both modules and marked shared, and neither
// position has a card of its own. Each module draws the move up out of its
// inbound slot and the park down into its outbound slot, and no out-stub. ONE
// robot: the dock's outbound note and the legend say Robot 1, where the
// pre-change picture hard-coded Robot 2.
characterise('single robot — in and out on one robot', singlePath, {
    moves: 4, r1Chevrons: 8, r2Chevrons: 0,
    cardLines: ['Robot 1 moves in', 'Robot 1 clears old', 'back position'],
    noCardLines: ['One robot, parks and swaps', 'Inbound PLN_02', 'Outbound PLN_05', 'Robot 2', 'front position',
        'for PLN_01', 'for PLN_04'],
    dock: ['Inbound source · Robot 1 → PLN_01, PLN_04', 'Outbound destination · Robot 1 ← PLN_01, PLN_04'],
    stagingSlots: 4, slots: ['PLN_02', 'PLN_05'], slotCaptions: ['>shared<'],
    strips: 0,
    legend: 1, legendRobots: [1],
});

// ── sequential — the A/B pair ───────────────────────────────────────────────
// One TWO-CARD module: one press bar over both, one move from A up into B and
// one out of B. The cards read the A/B flip sentence with each other's name.
// One robot end to end — the dock and legend say Robot 1.
characterise('sequential — the A/B pair', seqPath, {
    moves: 2, r1Chevrons: 4, r2Chevrons: 0,
    cardLines: ['One robot, A/B flip', 'with PLN_06', 'with PLN_03', 'front position', 'back position'],
    noCardLines: ['Robot 1 moves in', 'Robot 2 indexes'],
    dock: ['Inbound source · Robot 1 → PLN_03, PLN_06', 'Outbound destination · Robot 1 ← PLN_03, PLN_06'],
    stagingSlots: 0,
    strips: 0,
    legend: 1, legendRobots: [1],
});

// ── a mixed flow — one single_robot cell beside one two_robot cell ──────────
// Each module's old-bin move, and its card's out mark, is its OWN cell's
// robot: the single cell parks its old bin in its outbound slot with Robot 1,
// the two-robot cell's old bin leaves for the dock on Robot 2. The dock's OUT note names each robot with its
// positions; its mark takes no one robot's colour (two robots use it), and
// the legend lists both.
characterise('mixed — single_robot beside two_robot', mixedPath, {
    moves: 4, r1Chevrons: 6, r2Chevrons: 2,
    cardLines: ['Robot 1 stages at SLN_010', 'Robot 2 pulls old'],
    dock: ['Inbound source · Robot 1 → PLN_01, SLN_010',
        'Outbound destination · Robot 1 ← PLN_01 · Robot 2 ← PLN_03'],
    stagingSlots: 3, slots: ['SLN_011', 'SLN_012', 'SLN_010'],
    strips: 0,
    legend: 2, legendRobots: [1, 2],
});
{
    const svg = renderOne(JSON.parse(fs.readFileSync(mixedPath, 'utf8')));
    const modules = svg.split('<g class="module"').slice(1);
    const moduleOf = pos => modules.find(m => m.includes('<g class="node on" data-pos="' + pos + '"')) || '';
    // The old-bin move is the one that leaves the position's card: out to the
    // dock, or down into the slot a park leg names.
    const outChevrons = (m, pos) => {
        const g = (m.match(new RegExp('<g class="mv" data-from="' + pos + '" data-to="[^"]*">((?:(?!<\/g>).)*)<\/g>')) || [])[1] || '';
        return [...g.matchAll(/<path class="sc (r\d)"/g)].map(x => x[1]);
    };
    const outTo = (m, pos) => (m.match(new RegExp('<g class="mv" data-from="' + pos + '" data-to="([^"]*)"')) || [])[1];
    const outMark = m => (m.match(/<g class="io (r\d)"[^>]*>(?:(?!<\/g>).)*class="iot">out</) || [])[1];
    for (const [pos, r, to] of [['PLN_01', 'r1', 'SLN_012'], ['PLN_03', 'r2', 'dock']]) {
        const m = moduleOf(pos);
        const ch = outChevrons(m, pos);
        check('mixed: ' + pos + ' clears its old bin to ' + to + ' on ' + r,
            outTo(m, pos) === to && ch.length === 2 && ch.every(x => x === r), JSON.stringify({ to: outTo(m, pos), ch: ch }));
        check('mixed: ' + pos + ' card out mark is ' + r, outMark(m) === r, String(outMark(m)));
    }
    const dockOut = (svg.match(/<g class="io[^"]*" transform="translate\(16,26\)">(?:(?!<\/g>).)*<\/g><text class="k" x="36" y="36">OUT</) || [''])[0];
    check('mixed: the dock OUT mark takes neither robot colour', /^<g class="io" /.test(dockOut), dockOut.slice(0, 40));
}

// ── the order pin ───────────────────────────────────────────────────────────
//
// Line-side positions read left to right in world X, on every frame. This is
// the invariant the old 1.772 m spacing check in operator-flow.test.js was
// really about: its DISTANCE-NOT-DIRECTION note says the plant's handedness
// is not the picture's business — but spacing itself dies with the
// to-scale layouts, and what must survive the rewrite is the ORDER the
// operator walks the line in. Where a position has map coordinates, drawing
// order and card order are one question; where it has none (the schematic),
// the sequence column decides.
{
    console.log('order — line-side positions left to right by world X');
    for (const [name, file] of [['press index', indexPath], ['two robot', twoRobotPath],
        ['single robot', singlePath], ['sequential', seqPath]]) {
        const view = JSON.parse(fs.readFileSync(file, 'utf8'));
        const svg = renderOne(view);
        const cards = [...svg.matchAll(/<g class="node[^"]*" data-pos="([^"]+)"[^>]*transform="translate\(([-\d.]+),([-\d.]+)\)"/g)]
            .map(mm => ({ name: mm[1], x: +mm[2] }));
        // The claim's own world X for each drawn card, from the payload.
        const worldX = {};
        for (const p of view.cell.positions) worldX[p.core_node_name] = p.x;
        // FRONT cards (in the running flow) only — back positions sit wherever
        // their module does and say nothing about the line's order. A claimed
        // position is line-side whatever row it stands in, so these keep world
        // order; only a claimless back position outside every module is drawn
        // after the line side (operator-flow.test.js pins that).
        const fronts = cards.filter(c => {
            const p = view.cell.positions.find(q => q.core_node_name === c.name);
            return p && p.kind === 'front' && (p.claim || p.role === 'front');
        });
        check(name + ': front cards keep world order left to right', (() => {
            const placed = fronts.filter(c => isFinite(worldX[c.name]));
            const xs = placed.map(c => worldX[c.name]);
            const sorted = [...xs].sort((a, b) => a - b);
            // ORDER, NOT DISTANCE: the k-th smallest drawn x belongs to the
            // card with the k-th smallest world x. Mirror the world and the
            // drawn row mirrors with it; nothing else may rearrange.
            const byWorld = [...placed].sort((a, b) => worldX[a.name] - worldX[b.name]).map(c => c.x);
            const byDrawn = [...placed].sort((a, b) => a.x - b.x).map(c => c.x);
            return JSON.stringify(byWorld) === JSON.stringify(byDrawn) &&
                sorted.length === xs.length;
        })(), JSON.stringify(fronts.map(c => ({ n: c.name, drawn: c.x, world: worldX[c.name] }))));
    }
}


// ── every leg is drawn once, and every node a claim names is in its module ──
//
// The rule the module picture is built on: a module is one position and
// everything its claim names, and every leg the model gives is drawn once.
// Checked over EVERY fixture, against the model's own legs — the ones the
// picture reads (sentencesFromView) — rather than against a count written
// down per mode, so a leg a template has no place for cannot hide behind a
// count that was wrong when it was written.
//
// A move says which two boxes it joins (data-from, data-to; 'dock' is the
// dock band). The pin does not take that on trust: the pair of chevrons has
// to sit in the gap between those two boxes, inside that module, pointing from
// the first towards the second, in the leg's robot's colour.
//
// The nodes a claim names are the ones its choreography uses: the inbound
// staging, the outbound staging (except a two-robot swap's, which flowspec
// forbids for that mode — Robot 2 takes the old bin straight to the dock), and
// the paired position of an index or A/B claim. Each is a card or a slot in
// the claim's own module. The inbound and outbound source and destination are
// the dock's, not the module's.
const BOX = { card: [188, 92], deck: [188, 56], slot: [116, 44] };

function boxesIn(chunk) {
    const out = [];
    const re = /<g class="(node|stage)([^"]*)" data-(?:pos|staging)="([^"]+)"[^>]*transform="translate\(([-\d.]+),([-\d.]+)\)"/g;
    for (const mm of chunk.matchAll(re)) {
        const kind = mm[1] === 'stage' ? 'slot' : (/\bdeck\b/.test(mm[2]) ? 'deck' : 'card');
        const [w, h] = BOX[kind];
        out.push({ name: mm[3], x: +mm[4], y: +mm[5], w: w, h: h });
    }
    return out;
}

function movesIn(chunk) {
    const out = [];
    const re = /<g class="mv" data-from="([^"]*)" data-to="([^"]*)">((?:(?!<\/g>).)*)<\/g>/g;
    for (const mm of chunk.matchAll(re)) {
        const ch = [...mm[3].matchAll(/<path class="sc (r\d)" d="M-5[^"]*" transform="translate\(([-\d.]+) ([-\d.]+)\) rotate\(([-\d.]+)\)"/g)]
            .map(c => ({ r: c[1], x: +c[2], y: +c[3], th: +c[4] }));
        out.push({ from: mm[1], to: mm[2], chevrons: ch });
    }
    return out;
}

const inside = (b, x, y) => x >= b.x && x <= b.x + b.w && y >= b.y && y <= b.y + b.h;

// between: the chevrons' centre lies in the span of the two boxes, inside
// neither, and the pair points from the first box's centre towards the
// second's.
function between(mv, a, b) {
    if (mv.chevrons.length !== 2) return false;
    const cx = (mv.chevrons[0].x + mv.chevrons[1].x) / 2, cy = (mv.chevrons[0].y + mv.chevrons[1].y) / 2;
    const x0 = Math.min(a.x, b.x), x1 = Math.max(a.x + a.w, b.x + b.w);
    const y0 = Math.min(a.y, b.y), y1 = Math.max(a.y + a.h, b.y + b.h);
    if (!(cx >= x0 && cx <= x1 && cy >= y0 && cy <= y1)) return false;
    if (inside(a, cx, cy) || inside(b, cx, cy)) return false;
    const th = mv.chevrons[0].th * Math.PI / 180;
    const dx = (b.x + b.w / 2) - (a.x + a.w / 2), dy = (b.y + b.h / 2) - (a.y + a.h / 2);
    return Math.cos(th) * dx + Math.sin(th) * dy > 0;
}

function namedNodes(claim) {
    const out = [];
    if (claim.inbound_staging) out.push(claim.inbound_staging);
    if (claim.outbound_staging && claim.swap_mode !== 'two_robot') out.push(claim.outbound_staging);
    if (claim.paired_core_node && (claim.swap_mode === 'two_robot_press_index' || claim.swap_mode === 'sequential')) {
        out.push(claim.paired_core_node);
    }
    return out;
}

{
    console.log('legs — every leg drawn once between its boxes; every named node in its module');
    for (const [name, file] of [['press index', indexPath], ['two robot', twoRobotPath],
        ['two robot lanes', twoLanesPath], ['single robot', singlePath], ['sequential', seqPath],
        ['mixed', mixedPath], ['press index staged', indexStagedPath]]) {
        const view = JSON.parse(fs.readFileSync(file, 'utf8'));
        const svg = renderOne(view);
        const sentences = m.sentencesFromView(view);
        const chunks = svg.split('<g class="module"').slice(1);
        const all = chunks.map(c => ({ boxes: boxesIn(c), moves: movesIn(c) }));
        // A claim's own module is the one that draws its position's card.
        const ownModule = pos => {
            const i = chunks.findIndex(c => c.includes('" data-pos="' + pos + '"') &&
                new RegExp('<g class="node[^"]*" data-pos="' + pos + '"').test(c));
            return i < 0 ? null : all[i];
        };

        check(name + ': the model gives legs to check', sentences && Array.isArray(sentences.legs), '');
        for (const L of (sentences.legs || [])) {
            const tag = name + ': ' + L.kind + ' leg ' + L.from + ' → ' + L.to;
            const hits = [];
            for (const md of all) for (const mv of md.moves) if (mv.from === L.from && mv.to === L.to) hits.push({ md, mv });
            check(tag + ' is drawn exactly once', hits.length === 1, String(hits.length));
            if (hits.length !== 1) continue;
            const { md, mv } = hits[0];
            const want = 'r' + (L.robot === 2 ? 2 : 1);
            check(tag + ' is in Robot ' + want.slice(1) + "'s colour",
                mv.chevrons.length === 2 && mv.chevrons.every(c => c.r === want), JSON.stringify(mv.chevrons.map(c => c.r)));
            const a = md.boxes.find(b => b.name === L.from), b = md.boxes.find(bb => bb.name === L.to);
            check(tag + ' sits between its two boxes, pointing from the first to the second',
                !!a && !!b && between(mv, a, b), JSON.stringify({ from: a, to: b, mv: mv.chevrons }));
        }
        // An old bin that a park leg clears does not ALSO leave for the dock:
        // that would draw the one trip twice, to two places.
        for (const L of (sentences.legs || []).filter(l => l.kind === 'park')) {
            const stubs = all.reduce((n, md) => n + md.moves.filter(mv => mv.from === L.from && mv.to === 'dock').length, 0);
            check(name + ': ' + L.from + ' parks its old bin, so draws no move out to the dock', stubs === 0, String(stubs));
        }

        for (const p of view.cell.positions) {
            if (!p.claim || !p.claim.swap_mode) continue;
            const md = ownModule(p.core_node_name);
            check(name + ': ' + p.core_node_name + ' is drawn in a module', !!md, '');
            if (!md) continue;
            const names = new Set(md.boxes.map(b => b.name));
            for (const n of namedNodes(p.claim)) {
                check(name + ': ' + p.core_node_name + "'s module draws " + n + ' (named by its claim)',
                    names.has(n), [...names].join(', '));
            }
        }
    }
}

if (failures) { console.log(failures + ' FAILED'); process.exit(1); }
console.log('operator-flow per-mode: all checks pass');
