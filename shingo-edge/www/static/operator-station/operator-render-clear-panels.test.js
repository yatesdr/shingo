// Unit tests for the two unloader-window panels in operator-render.js:
// confirmUnloadSwap (the CLEAR: "Full pulled, empty filled?") and
// confirmPushEmpty (the empty-carrier panel: PUSH EMPTY and PUSH AS <type>).
// What is pinned is what each panel offers and what each button posts, because
// the post body is the whole contract with the Edge: clear-bin's bin_type_code
// is the type Core stamps on the carrier, and a blank one stamps nothing.
//
// Same loading technique as operator-render-drain.test.js: strip the ES
// imports and stub what the panels touch (el, getView, postAction, document).
// The el stub is a minimal node tree, enough to find buttons by their text and
// click them. Runs under plain Node via operator_render_clear_panels_test.go.
// Exit 0 = pass.

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

// ── stubs ──
function el(tag, props) {
    const n = Object.assign({ tag: tag, children: [], listeners: {} }, props || {});
    n.appendChild = function (c) { n.children.push(c); return c; };
    n.addEventListener = function (ev, fn) { n.listeners[ev] = fn; };
    n.remove = function () { n.removed = true; };
    return n;
}
let view = null;
let posts = [];
let mounted = [];
const src = fs.readFileSync(path.join(__dirname, 'operator-render.js'), 'utf8')
    .replace(/^import\s.*$/gm, '')
    .replace(/export\s+function\s+/g, 'function ')
    .replace(/export\s+const\s+/g, 'const ')
    .replace(/export\s*\{[^}]*\}\s*;?/g, '');
const ctx = vm.createContext({
    document: {
        getElementById: () => null,
        // No overlay is open when a panel opens here (showOverlay replaces one).
        querySelectorAll: () => [],
        body: { appendChild: function (n) { mounted.push(n); return n; } },
    },
    el: el,
    getView: function () { return view; },
    postAction: function (url, body) { posts.push({ url: url, body: body }); return Promise.resolve(true); },
    isActive: function () { return false; },
});
// The board model the panels read their choices and their lit one from
// (operator-board-model.js clearChoices, isLit): pure, so it runs as it ships.
vm.runInContext(fs.readFileSync(path.join(__dirname, 'operator-board-model.js'), 'utf8')
    .replace(/^import\s[^;]*;/gm, '').replace(/^export\s+/gm, ''), ctx);
vm.runInContext(src + '\nthis.confirmUnloadSwap = confirmUnloadSwap; this.confirmPushEmpty = confirmPushEmpty;', ctx);

function walk(n, out) {
    out.push(n);
    (n.children || []).forEach(function (c) { walk(c, out); });
    return out;
}
function open(fn) {
    posts = []; mounted = [];
    fn();
    if (mounted.length !== 1) throw new Error('want one overlay mounted, got ' + mounted.length);
    const nodes = walk(mounted[0], []);
    return {
        buttons: nodes.filter(function (n) { return n.tag === 'button'; }).map(function (b) { return b.textContent; }),
        texts: nodes.filter(function (n) { return n.tag === 'div' && n.textContent; }).map(function (n) { return n.textContent; }),
        click: function (label) {
            const b = nodes.find(function (n) { return n.tag === 'button' && n.textContent === label; });
            if (!b) throw new Error('no button ' + label);
            b.listeners.click();
            return posts.slice();
        },
    };
}

const catalog = [
    { payload_code: 'PART-A', bin_type_code: 'TOTE-S' },
    { payload_code: 'PART-B', bin_type_code: 'TOTE-L' },
    { payload_code: 'PART-C', bin_type_code: 'RACK-C' },
];

// ── confirmUnloadSwap: the CLEAR at an unloader window ──
// The panels take the tile's carrier list (loader.carriers). Which types a
// window's parts give, and the whole catalog when they match none, is the
// server's (service carriersFor, pinned by TestCarriersFor).

let p = open(function () { ctx.confirmUnloadSwap(11, ['TOTE-S', 'TOTE-L'], 0); });
eq(p.texts[0], 'Full pulled, empty filled?', 'clear: title');
eq(p.buttons, ['TOTE-S', 'TOTE-L', 'CANCEL'], 'clear: two types for the node payloads -> a picker');
eq(p.click('TOTE-L'), [{ url: '/api/process-nodes/11/clear-bin', body: { bin_type_code: 'TOTE-L' } }],
    'clear: a picked type posts that code');

p = open(function () { ctx.confirmUnloadSwap(12, ['RACK-C'], 0); });
eq(p.buttons, ['CONFIRM SWAP', 'CANCEL'], 'clear: one type -> auto-fill, one button');
eq(p.click('CONFIRM SWAP'), [{ url: '/api/process-nodes/12/clear-bin', body: { bin_type_code: 'RACK-C' } }],
    'clear: the auto-filled type is posted');

p = open(function () { ctx.confirmUnloadSwap(13, [], 0); });
eq(p.buttons, ['CONFIRM SWAP', 'CANCEL'], 'clear: no catalog -> one button');
eq(p.click('CONFIRM SWAP'), [{ url: '/api/process-nodes/13/clear-bin', body: undefined }],
    'clear: no catalog -> blank code (no body)');

p = open(function () { ctx.confirmUnloadSwap(14, ['RACK-C'], 7); });
eq(p.texts.indexOf('This slot still counts 7. Confirming writes it to zero.') >= 0, true,
    'clear: the discard count is stated');

// ── stage 1: the CLEAR at a window of an unloader that leaves the cart bare ──
// One tap, no picker even when the node's payloads map to several types, and
// a blank code: Core stamps the cart's own marker from the cart's type. The
// panel names no type at all — the marker is Core's bookkeeping.
//
// noTypeOnPanel is the pin the brief names: no "-BARE" text, and none of the
// catalog's bin-type codes, anywhere on the panel.
function noTypeOnPanel(p, label) {
    const codes = catalog.map(function (e) { return e.bin_type_code; });
    p.texts.concat(p.buttons).forEach(function (t) {
        eq(/-BARE/i.test(t), false, label + ': no -BARE text (' + t + ')');
        codes.forEach(function (c) {
            eq(t.indexOf(c) >= 0, false, label + ': no bin-type code ' + c + ' (' + t + ')');
        });
    });
}
p = open(function () { ctx.confirmUnloadSwap(15, ['TOTE-S', 'TOTE-L'], 0, true); });
eq(p.texts[0], 'Full bin on the cart', 'stage 1: title');
eq(p.buttons, ['Full off', 'CANCEL'], 'stage 1: one button, no picker');
eq(p.texts.indexOf('The cart goes on to Stage 2. The next full is on its way.') >= 0, true,
    'stage 1: says where the cart goes');
noTypeOnPanel(p, 'stage 1');
eq(p.click('Full off'), [{ url: '/api/process-nodes/15/clear-bin', body: undefined }],
    'stage 1: blank code (no body) — Core stamps the cart');
p = open(function () { ctx.confirmUnloadSwap(16, ['RACK-C'], 5, true); });
eq(p.buttons, ['Full off', 'CANCEL'], 'stage 1: a single-type node does not auto-fill its type either');
noTypeOnPanel(p, 'stage 1 single-type');
eq(p.click('Full off'), [{ url: '/api/process-nodes/16/clear-bin', body: undefined }],
    'stage 1: still a blank code');
eq(p.texts.indexOf('This slot still counts 5. Confirming writes it to zero.') >= 0, true,
    'stage 1: the discard count is still stated');

// ── confirmPushEmpty: the empty-carrier panel ──
p = open(function () { ctx.confirmPushEmpty(21, ['TOTE-S', 'TOTE-L']); });
eq(p.texts[0], 'Empty bin in slot', 'empty: title');
eq(p.buttons, ['PUSH EMPTY', 'PUSH AS TOTE-S', 'PUSH AS TOTE-L', 'CANCEL'],
    'empty: PUSH EMPTY and PUSH AS <type> for the node payloads are both offered');
eq(p.click('PUSH EMPTY'), [{ url: '/api/process-nodes/21/push-empty', body: undefined }],
    'empty: PUSH EMPTY posts push-empty');
p = open(function () { ctx.confirmPushEmpty(21, ['TOTE-S', 'TOTE-L']); });
eq(p.click('PUSH AS TOTE-S'), [{ url: '/api/process-nodes/21/clear-bin', body: { bin_type_code: 'TOTE-S' } }],
    'empty: PUSH AS posts clear-bin with the type');

// A window whose parts match nothing in the catalog carries the whole catalog
// (carriersFor), and the panel offers every type in it.
p = open(function () { ctx.confirmPushEmpty(22, ['TOTE-S', 'TOTE-L', 'RACK-C']); });
eq(p.buttons, ['PUSH EMPTY', 'PUSH AS TOTE-S', 'PUSH AS TOTE-L', 'PUSH AS RACK-C', 'CANCEL'],
    'empty: the window\'s carriers, each a PUSH AS');

// ── stage 2: a bare cart in the window ──
// One tap, Send on, with a blank code: Core stamps the cart back to the type
// it is the marker of. No PUSH EMPTY (it would send the cart on still bare) and
// no PUSH AS picker — there is no type to pick, and none is shown.
p = open(function () { ctx.confirmPushEmpty(25, ['TOTE-S', 'TOTE-L'], true); });
eq(p.texts[0], 'Cart, no bin', 'stage 2: title');
eq(p.buttons, ['Send on', 'CANCEL'], 'stage 2: bare -> one tap, no PUSH EMPTY, no PUSH AS');
eq(p.texts.indexOf('Put an empty bin on it, then send it on.') >= 0, true,
    'stage 2: says what to do');
noTypeOnPanel(p, 'stage 2');
eq(p.click('Send on'), [{ url: '/api/process-nodes/25/clear-bin', body: undefined }],
    'stage 2: Send on posts clear-bin with a blank code');
p = open(function () { ctx.confirmPushEmpty(27, [], true); });
eq(p.buttons, ['Send on', 'CANCEL'], 'stage 2: no carriers, no picker either');
noTypeOnPanel(p, 'stage 2 zero-payload');
p = open(function () { ctx.confirmPushEmpty(26, [], false); });
eq(p.buttons[0], 'PUSH EMPTY', 'stage 2: a carrier that is not bare keeps PUSH EMPTY');

p = open(function () { ctx.confirmPushEmpty(24, []); });
eq(p.buttons, ['PUSH EMPTY', 'CANCEL'], 'empty: no carriers (no catalog from Core) -> PUSH EMPTY only');


// ── heights: a type the empties group wants heights of ──
// The bin's name (the plain type, no layers) and the bin's name with each
// wanted height, each height posting the CLEAR with its layers; tops & bottoms
// only where the group has its row; the lit one from clearHighlight; a type
// with no wanted heights unchanged.
view = { empties: { group: 'G', rows: [{ bin_type: 'TOTE-S', have: 0, spots: 1 },
    { bin_type: 'TOTE-S', layers: 3, have: 0, want: 2, spots: 1 }, { bin_type: 'RACK-C', have: 0, spots: 1 }] } };
p = open(function () { ctx.confirmUnloadSwap(31, ['TOTE-S', 'RACK-C'], 0, false, { bin_type: 'TOTE-S', layers: 3 }); });
eq(p.buttons, ['TOTE-S', 'TOTE-S · 3 high', 'RACK-C', 'CANCEL'], 'heights: the CLEAR offers the bin\'s name and each wanted height');
const litOf = function (pp) {
    return walk(mounted[0], []).filter(n => n.tag === 'button' && String(n.className).indexOf('--lit') !== -1).map(n => n.textContent);
};
eq(litOf(p), ['TOTE-S · 3 high'], 'heights: the assignment\'s height is lit');
eq(p.click('TOTE-S · 3 high'), [{ url: '/api/process-nodes/31/clear-bin', body: { bin_type_code: 'TOTE-S', layers: 3 } }],
    'heights: a height posts the CLEAR with its type and layers');
p = open(function () { ctx.confirmUnloadSwap(31, ['TOTE-S', 'RACK-C'], 0, false, null); });
eq(p.click('TOTE-S'), [{ url: '/api/process-nodes/31/clear-bin', body: { bin_type_code: 'TOTE-S' } }],
    'heights: the plain type posts no layers');
p = open(function () { ctx.confirmUnloadSwap(31, ['TOTE-S', 'RACK-C'], 0, false, null); });
eq(p.click('RACK-C'), [{ url: '/api/process-nodes/31/clear-bin', body: { bin_type_code: 'RACK-C' } }],
    'heights: a type with none wanted posts no layers');
// One type with heights: its buttons in place of CONFIRM SWAP.
p = open(function () { ctx.confirmUnloadSwap(32, ['TOTE-S'], 0, false, null); });
eq(p.buttons, ['TOTE-S', 'TOTE-S · 3 high', 'CANCEL'], 'heights: one type\'s heights replace CONFIRM SWAP');
// Stage 1 leaves the cart bare: no heights, no layers.
p = open(function () { ctx.confirmUnloadSwap(33, ['TOTE-S'], 0, true, null); });
eq(p.buttons, ['Full off', 'CANCEL'], 'heights: stage 1 offers no height');
eq(p.click('Full off'), [{ url: '/api/process-nodes/33/clear-bin', body: undefined }], 'heights: Full off posts no layers');
// The empty-cart panel: PUSH EMPTY unchanged, PUSH AS by height.
p = open(function () { ctx.confirmPushEmpty(34, ['TOTE-S', 'RACK-C'], false, { bin_type: 'TOTE-S', layers: 3 }); });
eq(p.buttons, ['PUSH EMPTY', 'PUSH AS TOTE-S', 'PUSH AS TOTE-S · 3 high', 'PUSH AS RACK-C', 'CANCEL'],
    'heights: the re-declare offers each height');
eq(p.click('PUSH EMPTY'), [{ url: '/api/process-nodes/34/push-empty', body: undefined }], 'heights: PUSH EMPTY unchanged');
p = open(function () { ctx.confirmPushEmpty(34, ['TOTE-S', 'RACK-C'], false, null); });
eq(p.click('PUSH AS TOTE-S · 3 high'), [{ url: '/api/process-nodes/34/clear-bin', body: { bin_type_code: 'TOTE-S', layers: 3 } }],
    'heights: PUSH AS a height posts its type and layers');
// Stage 2's Send on: a tap per height, posting layers and still no type.
p = open(function () { ctx.confirmPushEmpty(35, ['TOTE-S'], true, null); });
eq(p.buttons, ['Send on', 'Send on · 3 high', 'CANCEL'], 'heights: stage 2 Send on, one tap per height, naming no type');
noTypeOnPanel(p, 'stage 2 with heights');
eq(p.click('Send on · 3 high'), [{ url: '/api/process-nodes/35/clear-bin', body: { layers: 3 } }],
    'heights: Send on at a height posts its layers and no type (the cart\'s type is Core\'s)');
p = open(function () { ctx.confirmPushEmpty(35, ['TOTE-S'], true, null); });
eq(p.click('Send on'), [{ url: '/api/process-nodes/35/clear-bin', body: undefined }],
    'heights: the plain Send on posts no layers');
// A group with a "Tops & bottoms" row: that choice on CLEAR, PUSH AS and Send on,
// declaring 0.
view = { empties: { group: 'G', rows: [{ bin_type: 'TOTE-S', have: 0, spots: 1 },
    { bin_type: 'TOTE-S', layers: 3, have: 0, want: 2, spots: 1 }, { bin_type: 'TOTE-S', layers: 0, have: 0, want: 1, spots: 1 },
    { bin_type: 'RACK-C', have: 0, spots: 1 }] } };
p = open(function () { ctx.confirmUnloadSwap(37, ['TOTE-S', 'RACK-C'], 0, false, { bin_type: 'TOTE-S', layers: 0 }); });
eq(p.buttons, ['TOTE-S', 'TOTE-S · 3 high', 'TOTE-S · tops & bottoms', 'RACK-C', 'CANCEL'],
    'tops & bottoms row: the CLEAR offers it after the heights');
eq(litOf(p), ['TOTE-S · tops & bottoms'], 'tops & bottoms row: an assignment of tops & bottoms is lit');
eq(p.click('TOTE-S · tops & bottoms'), [{ url: '/api/process-nodes/37/clear-bin', body: { bin_type_code: 'TOTE-S', layers: 0 } }],
    'tops & bottoms row: it posts layers 0');
p = open(function () { ctx.confirmPushEmpty(38, ['TOTE-S'], false, null); });
eq(p.buttons, ['PUSH EMPTY', 'PUSH AS TOTE-S', 'PUSH AS TOTE-S · 3 high', 'PUSH AS TOTE-S · tops & bottoms', 'CANCEL'],
    'tops & bottoms row: PUSH AS offers it');
p = open(function () { ctx.confirmPushEmpty(39, ['TOTE-S'], true, null); });
eq(p.buttons, ['Send on', 'Send on · 3 high', 'Send on · tops & bottoms', 'CANCEL'],
    'tops & bottoms row: stage 2 Send on offers it, naming no type');
eq(p.click('Send on · tops & bottoms'), [{ url: '/api/process-nodes/39/clear-bin', body: { layers: 0 } }],
    'tops & bottoms row: Send on · tops & bottoms posts layers 0');
// A Core that sends no group lines: today's panels.
view = { empties: { group: 'G', rows: [{ bin_type: 'TOTE-S', have: 0, spots: 1 }] } };
p = open(function () { ctx.confirmUnloadSwap(36, ['TOTE-S'], 0, false, { bin_type: 'TOTE-S' }); });
eq(p.buttons, ['CONFIRM SWAP', 'CANCEL'], 'no heights wanted: today\'s CONFIRM SWAP');
view = null;
if (failed) { console.error(passed + ' passed, ' + failed + ' FAILED'); process.exit(1); }
console.log('operator-render clear panels: ' + passed + ' passed');
