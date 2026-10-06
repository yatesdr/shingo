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
    check(label + ': move groups drawn', count(svg, /<g class="mv">/g) === (want.moves || 0),
        String(count(svg, /<g class="mv">/g)));
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

const [indexPath, twoRobotPath, singlePath, seqPath, twoLanesPath, mixedPath] = process.argv.slice(2);
if (!indexPath || !twoRobotPath || !singlePath || !seqPath || !twoLanesPath || !mixedPath) {
    console.log('usage: node operator-flow.modes.test.js <index-pair.json> <two-robot.json> <single-robot.json> <sequential.json> <two-robot-lanes.json> <mixed.json>');
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
// of the cell (the Hopkinsville shape) — so no slot is drawn (the position is
// already a card) and the only move is the old bin out, Robot 2's. The
// staging positions read "Inbound staging · for <press>" on their own cards.
characterise('two robot — two staging moves', twoRobotPath, {
    moves: 2, r1Chevrons: 0, r2Chevrons: 4,
    cardLines: ['Robot 1 stages at PLN_02', 'Robot 1 stages at PLN_05', 'Robot 2 pulls old',
        'Inbound staging', 'for PLN_03', 'for PLN_06', 'front position'],
    noCardLines: ['Robot 2 indexes', 'Robot 1 clears old'],
    dock: ['Inbound source · Robot 1 → PLN_02, PLN_05', 'Outbound destination · Robot 2 ← PLN_03, PLN_06'],
    stagingSlots: 0,
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
// clears old") — its staging places ARE positions of the cell (PLN_02/PLN_05),
// so no slots and no slot captions; the moves are words on the card. ONE
// robot: the dock's outbound note and the legend say Robot 1, where the
// pre-change picture hard-coded Robot 2 (§4.6 ruling).
characterise('single robot — in and out on one robot', singlePath, {
    moves: 2, r1Chevrons: 4, r2Chevrons: 0,
    cardLines: ['Robot 1 moves in', 'Robot 1 clears old', 'Inbound staging', 'for PLN_01',
        'Outbound staging', 'back position'],
    noCardLines: ['One robot, parks and swaps', 'Inbound PLN_02', 'Outbound PLN_05', 'Robot 2', 'front position'],
    dock: ['Inbound source · Robot 1 → PLN_01, PLN_04', 'Outbound destination · Robot 1 ← PLN_01, PLN_04'],
    stagingSlots: 0,
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
// Each module's move to the dock, and its card's out mark, is its OWN cell's
// robot: the single cell clears with Robot 1, the two-robot cell's old bin
// leaves on Robot 2. The dock's OUT note names each robot with its
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
    // The move to the dock is the module's one horizontal move (rotate 0);
    // every move up out of a slot is vertical.
    const outChevrons = m => [...m.matchAll(/<path class="sc (r\d)" d="M-5[^"]*" transform="[^"]*rotate\(0\.0\)"/g)].map(x => x[1]);
    const outMark = m => (m.match(/<g class="io (r\d)"[^>]*>(?:(?!<\/g>).)*class="iot">out</) || [])[1];
    for (const [pos, r] of [['PLN_01', 'r1'], ['PLN_03', 'r2']]) {
        const m = moduleOf(pos);
        const ch = outChevrons(m);
        check('mixed: ' + pos + ' clears to the dock on ' + r, ch.length === 2 && ch.every(x => x === r), JSON.stringify(ch));
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

if (failures) { console.log(failures + ' FAILED'); process.exit(1); }
console.log('operator-flow per-mode: all checks pass');
