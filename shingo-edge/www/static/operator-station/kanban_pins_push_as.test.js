// Characterization pins of the unloader's PUSH AS panel at main 26a82fdf, taken
// before the Kanban build. What is pinned is what the tree does now; the build's
// PINS.md says which unit changes which assertion and what it becomes.
//
// Same loading technique as operator-render-clear-panels.test.js: the board model
// and the window model run as they ship, operator-render.js with its imports
// stripped and el, getView, postAction and document stubbed. Runs under plain Node
// via kanban_pins_push_as_test.go. Exit 0 = pass.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
function eq(got, want, label) {
    const g = JSON.stringify(got), w = JSON.stringify(want);
    if (g === w) { passed++; return; }
    failed++;
    console.error('FAIL: ' + label + '\n   got:  ' + g + '\n   want: ' + w);
}

function el(tag, props) {
    const n = Object.assign({ tag: tag, children: [], listeners: {} }, props || {});
    n.appendChild = function (c) { n.children.push(c); return c; };
    n.addEventListener = function (ev, fn) { n.listeners[ev] = fn; };
    n.remove = function () { n.removed = true; };
    return n;
}
let view = null;
let mounted = [];
const ctx = vm.createContext({
    document: {
        getElementById: () => null,
        querySelectorAll: () => [],
        body: { appendChild: function (n) { mounted.push(n); return n; } },
    },
    el: el,
    getView: function () { return view; },
    postAction: function () { return Promise.resolve(true); },
    isActive: function () { return false; },
});
['operator-window-state.js', 'operator-board-model.js'].forEach(function (f) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, f), 'utf8')
        .replace(/^import\s[^;]*;/gm, '').replace(/^export\s+/gm, ''), ctx);
});
vm.runInContext(fs.readFileSync(path.join(__dirname, 'operator-render.js'), 'utf8')
    .replace(/^import\s.*$/gm, '')
    .replace(/export\s+function\s+/g, 'function ')
    .replace(/export\s+const\s+/g, 'const ')
    .replace(/export\s*\{[^}]*\}\s*;?/g, '') +
    '\nthis.confirmUnloadSwap = confirmUnloadSwap; this.confirmPushEmpty = confirmPushEmpty;' +
    '\nthis.clearChoices = clearChoices; this.clearHighlight = clearHighlight; this.carriersOf = carriersOf;', ctx);

function walk(n, out) {
    out.push(n);
    (n.children || []).forEach(function (c) { walk(c, out); });
    return out;
}
function panel(fn) {
    mounted = [];
    fn();
    const buttons = walk(mounted[0], []).filter(function (n) { return n.tag === 'button'; });
    return {
        buttons: buttons.map(function (b) { return b.textContent; }),
        lit: buttons.filter(function (b) { return String(b.className).indexOf('--lit') !== -1; }).map(function (b) { return b.textContent; }),
    };
}
const plain = v => JSON.parse(JSON.stringify(v));

// ── a lit type with no button lights nothing, and adds no button ──
view = {};
let p = panel(function () { ctx.confirmPushEmpty(1, ['TOTE-S'], false, { bin_type: 'TOTE-L' }); });
eq(p.buttons, ['PUSH EMPTY', 'PUSH AS TOTE-S', 'CANCEL'], 'PUSH AS: the lit type gets no button of its own');
eq(p.lit, [], 'PUSH AS: a lit type with no button lights nothing');
p = panel(function () { ctx.confirmUnloadSwap(1, ['TOTE-S', 'RACK-C'], 0, false, { bin_type: 'TOTE-L' }); });
eq(p.buttons, ['TOTE-S', 'RACK-C', 'CANCEL'], 'CLEAR: the lit type gets no button of its own');
eq(p.lit, [], 'CLEAR: a lit type with no button lights nothing');
p = panel(function () { ctx.confirmPushEmpty(1, ['TOTE-S'], false, { bin_type: 'TOTE-S', layers: 3 }); });
eq(p.buttons, ['PUSH EMPTY', 'PUSH AS TOTE-S', 'CANCEL'], 'a lit height the group does not want gets no button');
eq(p.lit, [], 'a lit height the group does not want lights nothing, not even its plain type');
p = panel(function () { ctx.confirmPushEmpty(1, [], false, { bin_type: 'TOTE-L' }); });
eq(p.buttons, ['PUSH EMPTY', 'CANCEL'], 'no carriers: nothing is added for the lit type');

// ── clearChoices: the cart's floor bounds the carriers, as it bounds Core's walk ──
const floors = { floor: { 'TOTE-S': '45x48', 'KD-S': '45x48', 'TOTE-L': '30x32' } };
const three = ['TOTE-S', 'KD-S', 'TOTE-L'];
const types = c => plain(c).map(x => x.bin_type);
eq(types(ctx.clearChoices(floors, three, 'TOTE-S')), ['TOTE-S', 'KD-S'], 'a 45x48 cart is offered the 45x48 types');
eq(types(ctx.clearChoices(floors, three, 'TOTE-L')), ['TOTE-L'], 'a 30x32 cart is offered the 30x32 type');
eq(types(ctx.clearChoices(floors, three, 'TOTE-S', { bin_type: 'TOTE-L' })), three,
    'the lit choice stays whatever its floor, in the carriers\' order');
eq(types(ctx.clearChoices(floors, ['TOTE-S', 'NO-SIZE'], 'TOTE-S')), ['TOTE-S'],
    'a type with no size is not on a sized cart\'s floor (windowWalk.fits)');
eq(types(ctx.clearChoices(floors, three, 'NO-SIZE')), three, 'a cart whose type has no size: no filter');
eq(types(ctx.clearChoices(floors, three)), three, 'no cart named: no filter');
eq(types(ctx.clearChoices({}, three, 'TOTE-S')), three, 'a Core that sends no floors: no filter');
eq(types(ctx.clearChoices(null, three, 'TOTE-S')), three, 'no view: no filter');
const tall = { floor: floors.floor, empties: { rows: [{ bin_type: 'TOTE-S', layers: 3 }, { bin_type: 'TOTE-L', layers: 2 }] } };
eq(plain(ctx.clearChoices(tall, three, 'TOTE-S')), [{ bin_type: 'TOTE-S' }, { bin_type: 'TOTE-S', layers: 3 }, { bin_type: 'KD-S' }],
    'a kept type keeps its heights; a type off the floor takes its heights with it');

// ── the panels pass the cart's type through ──
view = floors;
p = panel(function () { ctx.confirmPushEmpty(1, three, false, { bin_type: 'KD-S' }, null, 'TOTE-S'); });
eq(p.buttons, ['PUSH EMPTY', 'PUSH AS TOTE-S', 'PUSH AS KD-S', 'CANCEL'], 'PUSH AS: the cart\'s floor only');
eq(p.lit, ['PUSH AS KD-S'], 'PUSH AS: the lit one lit');
p = panel(function () { ctx.confirmPushEmpty(1, three, false, null, null, 'TOTE-L'); });
eq(p.buttons, ['PUSH EMPTY', 'PUSH AS TOTE-L', 'CANCEL'], 'PUSH AS on a 30x32 cart');
p = panel(function () { ctx.confirmUnloadSwap(1, three, 0, false, null, 'TOTE-S'); });
eq(p.buttons, ['TOTE-S', 'KD-S', 'CANCEL'], 'CLEAR: the cart\'s floor only');
p = panel(function () { ctx.confirmUnloadSwap(1, ['TOTE-S', 'TOTE-L'], 0, false, null, 'TOTE-S'); });
eq(p.buttons, ['CONFIRM SWAP', 'CANCEL'], 'CLEAR: one type left on the floor is the one-tap auto-fill');
view = {};

// ── clearHighlight's fall-through with no assignment type ──
const cart = { bin_state: { occupied: true, bin_type_code: 'TOTE-S' } };
eq(plain(ctx.clearHighlight(Object.assign({ assignment: { none_short: true } }, cart), [])), { bin_type: 'TOTE-S' },
    'an assignment naming no type falls through to like for like; no none_short is read');
eq(plain(ctx.clearHighlight(Object.assign({ assignment: { bin_type: 'TOTE-L', group: 'G2' } }, cart), [])), { bin_type: 'TOTE-L' },
    'an assignment lights its type and carries no group');

// ── carriersOf: the tile's list as Core sent it ──
eq(plain(ctx.carriersOf({ loader: { carriers: ['TOTE-S', 'RACK-C'] } })), ['TOTE-S', 'RACK-C'], 'carriersOf reads the tile\'s list');
eq(plain(ctx.carriersOf({})), [], 'carriersOf with no loader is empty');

if (failed > 0) {
    console.error(failed + ' failed, ' + passed + ' passed');
    process.exit(1);
}
console.log(passed + ' passed');
