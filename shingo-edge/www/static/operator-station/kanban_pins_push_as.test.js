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

// ── the lit choice is always offered: a whole kanban's lit type is a source's, and may be
// none of this loader's parts (the panels share clearChoices) ──
view = {};
let p = panel(function () { ctx.confirmPushEmpty(1, ['TOTE-S'], false, { bin_type: 'TOTE-L' }); });
eq(p.buttons, ['PUSH EMPTY', 'PUSH AS TOTE-S', 'PUSH AS TOTE-L', 'CANCEL'], 'PUSH AS: the lit type is appended');
eq(p.lit, ['PUSH AS TOTE-L'], 'PUSH AS: and lit');
p = panel(function () { ctx.confirmUnloadSwap(1, ['TOTE-S', 'RACK-C'], 0, false, { bin_type: 'TOTE-L' }); });
eq(p.buttons, ['TOTE-S', 'RACK-C', 'TOTE-L', 'CANCEL'], 'CLEAR: the lit type is appended');
eq(p.lit, ['TOTE-L'], 'CLEAR: and lit');
p = panel(function () { ctx.confirmPushEmpty(1, ['TOTE-S'], false, { bin_type: 'TOTE-S', layers: 3 }); });
eq(p.buttons, ['PUSH EMPTY', 'PUSH AS TOTE-S', 'PUSH AS TOTE-S · 3 high', 'CANCEL'], 'a lit height is appended with its height');
eq(p.lit, ['PUSH AS TOTE-S · 3 high'], 'and it alone is lit');
p = panel(function () { ctx.confirmPushEmpty(1, [], false, { bin_type: 'TOTE-L' }); });
eq(p.buttons, ['PUSH EMPTY', 'PUSH AS TOTE-L', 'CANCEL'], 'no carriers: the lit type is still offered');
p = panel(function () { ctx.confirmPushEmpty(1, ['TOTE-S'], false, null); });
eq(p.buttons, ['PUSH EMPTY', 'PUSH AS TOTE-S', 'CANCEL'], 'nothing lit: nothing appended, PUSH EMPTY stays');

// ── clearChoices: the cart's family bounds the carriers, as it bounds Core's walk ──
const families = { family: { 'TOTE-S': 'F45', 'KD-S': 'F45', 'TOTE-L': 'F32', 'NO-SIZE': 'NO-SIZE' } };
const three = ['TOTE-S', 'KD-S', 'TOTE-L'];
const types = c => plain(c).map(x => x.bin_type);
eq(types(ctx.clearChoices(families, three, 'TOTE-S')), ['TOTE-S', 'KD-S'], 'a cart of the family is offered its family\'s types');
eq(types(ctx.clearChoices(families, three, 'TOTE-L')), ['TOTE-L'], 'a family of one is offered its one type');
eq(types(ctx.clearChoices(families, three, 'TOTE-S', { bin_type: 'TOTE-L' })), three,
    'the lit choice stays whatever its family, in the carriers\' order');
eq(types(ctx.clearChoices(families, ['TOTE-S', 'NO-SIZE'], 'TOTE-S')), ['TOTE-S'],
    'a type in no family of the cart\'s is not offered (windowWalk.fits)');
eq(types(ctx.clearChoices(families, three, 'NO-SIZE')), [], 'a family of one offers no other type');
eq(types(ctx.clearChoices(families, three)), three, 'no cart named: no filter');
eq(types(ctx.clearChoices({}, three, 'TOTE-S')), three, 'a Core that sends no families: no filter');
eq(types(ctx.clearChoices(null, three, 'TOTE-S')), three, 'no view: no filter');
const tall = { family: families.family, empties: { rows: [{ bin_type: 'TOTE-S', layers: 3 }, { bin_type: 'TOTE-L', layers: 2 }] } };
eq(plain(ctx.clearChoices(tall, three, 'TOTE-S')), [{ bin_type: 'TOTE-S' }, { bin_type: 'TOTE-S', layers: 3 }, { bin_type: 'KD-S' }],
    'a kept type keeps its heights; a type of another family takes its heights with it');

// ── the panels pass the cart's type through ──
view = families;
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

// ── clearHighlight: nothing short lights nothing, before like for like ──
const cart = { bin_state: { occupied: true, bin_type_code: 'TOTE-S' } };
eq(ctx.clearHighlight(Object.assign({ assignment: { none_short: true } }, cart), []), null,
    'no group is short of this cart: nothing lit, not like for like');
eq(plain(ctx.clearHighlight(Object.assign({ assignment: {} }, cart), [])), { bin_type: 'TOTE-S' },
    'an assignment naming nothing still falls through to like for like');
eq(plain(ctx.clearHighlight(Object.assign({ assignment: { bin_type: 'TOTE-L', group: 'G2' } }, cart), [])), { bin_type: 'TOTE-L' },
    'an assignment lights its type; the group is words on the card and on no choice');

// ── carriersOf: the tile's list as Core sent it ──
eq(plain(ctx.carriersOf({ loader: { carriers: ['TOTE-S', 'RACK-C'] } })), ['TOTE-S', 'RACK-C'], 'carriersOf reads the tile\'s list');
eq(plain(ctx.carriersOf({})), [], 'carriersOf with no loader is empty');

// ── the one truth table of a cart's family (shingo-core/kanban/testdata/same_family.json),
// which Core's walk, arm, save and CLEAR door and simcalc read too ──
const familyTable = JSON.parse(fs.readFileSync(path.join(__dirname, '../../../../shingo-core/kanban/testdata/same_family.json'), 'utf8'));
const familyView = { family: {} };
Object.keys(familyTable.types).forEach(function (code) { familyView.family[code] = familyTable.types[code].family; });
familyTable.cases.forEach(function (c) {
    const offered = types(ctx.clearChoices(familyView, [c.cart, c.asked], c.cart)).indexOf(c.asked) !== -1;
    eq(offered, c.same, 'a ' + c.cart + ' is offered ' + c.asked + ' exactly where the table says one family');
});

if (failed > 0) {
    console.error(failed + ' failed, ' + passed + ' passed');
    process.exit(1);
}
console.log(passed + ' passed');
