// Rendering test for the card a loader draws when a served cell changes over
// to a part it has none of.
//
// The deliverable is a sentence an operator reads off a board while standing
// at a loader, so it is asserted by RENDERING it. Round 1's changeover banner
// ("1 participant node HAVE no process node configured") and round 2's modal
// geometry both survived review and died the moment they were rendered.
//
// The banner is gone: the board draws a Load card, and its colour follows the
// cell's state, never the kind of demand. A cell changing over to a part it has
// none of is red, the same card as a cell that ran out, and no card says
// "changeover".
//
// Runs under plain Node (no npm): the model (operator-board-model.js), the
// window model it reads and the board's renderer (operator-board.js), as they
// ship, in one vm with a minimal element. Exit 0 = pass, 1 = any failure. Run
// via the Go wrapper operator_changeover_load_card_test.go.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
function ok(cond, what, expected, actual) {
    if (cond) { passed++; return; }
    failed++;
    console.error('FAIL: ' + what + '\n   expected: ' + expected + '\n   actual:   ' + actual);
}

// A minimal element: enough to build a card and read its text back.
function fakeEl(tag) {
    const e = {
        tagName: tag, children: [], className: '', _text: '', listeners: {},
        style: { setProperty() {} },
        classList: { add(c) { e.className += ' ' + c; }, remove() {} },
        appendChild(c) { e.children.push(c); return c; },
        addEventListener(k, f) { e.listeners[k] = f; },
        set textContent(v) { e._text = String(v); },
        get textContent() { return e._text + e.children.map(c => c.textContent).join(''); },
        set innerHTML(v) { e._html = v; },
    };
    return e;
}
const ctx = vm.createContext({
    el: function (tag, props) { return Object.assign(fakeEl(tag), props || {}); },
});
['operator-window-state.js', 'operator-board-model.js', 'operator-board.js'].forEach(function (f) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, f), 'utf8')
        .replace(/^import\s[^;]*;/gm, '').replace(/^export\s+/gm, ''), ctx);
});

function tile(id, name, part) {
    return {
        node: { id: id, name: name, core_node_name: name }, orders: [],
        loader: { role: 'produce', layout: 'dedicated_positions', replenishment: 'threshold', outbound_dest: '',
            parts: [{ code: part }], directive: false, leaves_bare: false, pulled_directly: false },
        bin_state: { occupied: true, payload_code: '', stocked: false, bin_type_code: 'TOTE-L' },
    };
}
function row(cell, part, have, o) {
    return Object.assign({ cell: cell, part: part, role: 'consume', carrier: 'TOTE-L', have_uop: have, capacity_uop: 100,
        incoming: false, node: 'PLN_A2', process_id: 2, process_node_id: 2, reorder_uop: 0, waiting: false }, o || {});
}
function card(view) {
    const m = ctx.boardModel(view);
    const c = m.zones.find(z => z.key === 'load').cards[0];
    return { card: c, text: c ? ctx.buildCard(c, { act() {} }).textContent : '' };
}

// ── a cell changing over to a part it has none of ──────────────────────────
{
    const r = card({ station: {}, nodes: [tile(1, 'SMN_A21', 'PART-NEW')],
        serves: [row('PRESS-3', 'PART-OLD', 40), row('PRESS-3', 'PART-NEW', 0, { incoming: true, waiting: true })] });
    ok(r.card && r.card.tone === 'red', 'the card is red', 'red', r.card && r.card.tone);
    ok(/NOW/.test(r.text), 'the card carries NOW', 'NOW', r.text);
    ok(/PRESS-3 out/.test(r.text), 'the reason names the cell', 'PRESS-3 out', r.text);
    ok(/PART-NEW/.test(r.text), 'the part is the headline', 'PART-NEW', r.text);
    ok(/SMN_A21/.test(r.text) && /TOTE-L/.test(r.text), 'the spot and what it holds', 'SMN_A21 holds an empty TOTE-L', r.text);
    ok(!/changeover/i.test(r.text), 'no card says changeover', 'no "changeover"', r.text);
}

// ── the same cell out of a running part: the same red card ─────────────────
{
    const r = card({ station: {}, nodes: [tile(1, 'SMN_A21', 'PART-RUN')], serves: [row('PRESS-3', 'PART-RUN', 0)] });
    ok(r.card && r.card.tone === 'red' && /PRESS-3 out/.test(r.text), 'a running part out reads the same', 'red, PRESS-3 out', r.text);
}

// ── changing over with some left: waiting, amber ───────────────────────────
{
    const r = card({ station: {}, nodes: [tile(1, 'SMN_A21', 'PART-NEW')],
        serves: [row('PRESS-3', 'PART-NEW', 30, { incoming: true, waiting: true })] });
    ok(r.card && r.card.tone === 'amber' && /SOON/.test(r.text) && /PRESS-3 waiting/.test(r.text),
        'changing over with some left reads waiting', 'amber, SOON, PRESS-3 waiting', r.text);
}

// ── an incoming part nobody is waiting on: plain ───────────────────────────
{
    const r = card({ station: {}, nodes: [tile(1, 'SMN_A21', 'PART-NEW')],
        serves: [row('PRESS-3', 'PART-NEW', 0, { incoming: true })] });
    ok(r.card && r.card.tone === 'plain' && !/NOW|SOON/.test(r.text),
        'an incoming row not waiting colours nothing', 'plain', r.text);
}

// ── text goes in as text ────────────────────────────────────────────────────
{
    const r = card({ station: {}, nodes: [tile(1, 'SMN_A', 'PART-<img>&')], serves: [row('PRESS-3', 'PART-<img>&', 0)] });
    ok(/PART-<img>&/.test(r.text), 'a part is set as text, once, never escaped twice', 'PART-<img>&', r.text);
}

if (failed > 0) {
    console.error('\n' + failed + ' failure(s), ' + passed + ' passed');
    process.exit(1);
}
console.log('OK: ' + passed + ' assertions passed');
