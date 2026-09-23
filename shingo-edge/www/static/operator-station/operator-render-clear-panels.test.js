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
        body: { appendChild: function (n) { mounted.push(n); return n; } },
    },
    el: el,
    getView: function () { return view; },
    postAction: function (url, body) { posts.push({ url: url, body: body }); return Promise.resolve(true); },
    isActive: function () { return false; },
});
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
view = { payload_bin_types: catalog };

let p = open(function () { ctx.confirmUnloadSwap(11, ['PART-A', 'PART-B'], 0); });
eq(p.texts[0], 'Full pulled, empty filled?', 'clear: title');
eq(p.buttons, ['TOTE-S', 'TOTE-L', 'CANCEL'], 'clear: two types for the node payloads -> a picker');
eq(p.click('TOTE-L'), [{ url: '/api/process-nodes/11/clear-bin', body: { bin_type_code: 'TOTE-L' } }],
    'clear: a picked type posts that code');

p = open(function () { ctx.confirmUnloadSwap(12, ['PART-C'], 0); });
eq(p.buttons, ['CONFIRM SWAP', 'CANCEL'], 'clear: one type -> auto-fill, one button');
eq(p.click('CONFIRM SWAP'), [{ url: '/api/process-nodes/12/clear-bin', body: { bin_type_code: 'RACK-C' } }],
    'clear: the auto-filled type is posted');

view = { payload_bin_types: [] };
p = open(function () { ctx.confirmUnloadSwap(13, ['PART-A'], 0); });
eq(p.buttons, ['CONFIRM SWAP', 'CANCEL'], 'clear: no catalog -> one button');
eq(p.click('CONFIRM SWAP'), [{ url: '/api/process-nodes/13/clear-bin', body: undefined }],
    'clear: no catalog -> blank code (no body)');

view = { payload_bin_types: catalog };
p = open(function () { ctx.confirmUnloadSwap(14, ['PART-C'], 7); });
eq(p.texts.indexOf('This slot still counts 7. Confirming writes it to zero.') >= 0, true,
    'clear: the discard count is stated');

// ── stage 1: the CLEAR at an unloader with a bare type ──
// One tap, no picker even when the node's payloads map to several types, and
// a blank code: the Edge (ClearBin) fills the loader's bare type.
p = open(function () { ctx.confirmUnloadSwap(15, ['PART-A', 'PART-B'], 0, 'HALF-TOTE'); });
eq(p.texts[0], 'Full pulled — carrier leaves bare', 'stage 1: title');
eq(p.buttons, ['CONFIRM SWAP', 'CANCEL'], 'stage 1: one button, no picker');
eq(p.click('CONFIRM SWAP'), [{ url: '/api/process-nodes/15/clear-bin', body: undefined }],
    'stage 1: blank code (no body) — the Edge fills the bare type');
p = open(function () { ctx.confirmUnloadSwap(16, ['PART-C'], 5, 'HALF-TOTE'); });
eq(p.buttons, ['CONFIRM SWAP', 'CANCEL'], 'stage 1: a single-type node does not auto-fill its type either');
eq(p.click('CONFIRM SWAP'), [{ url: '/api/process-nodes/16/clear-bin', body: undefined }],
    'stage 1: still a blank code');
eq(p.texts.indexOf('This slot still counts 5. Confirming writes it to zero.') >= 0, true,
    'stage 1: the discard count is still stated');

// ── confirmPushEmpty: the empty-carrier panel ──
p = open(function () { ctx.confirmPushEmpty(21, ['PART-A', 'PART-B']); });
eq(p.texts[0], 'Empty bin in slot', 'empty: title');
eq(p.buttons, ['PUSH EMPTY', 'PUSH AS TOTE-S', 'PUSH AS TOTE-L', 'CANCEL'],
    'empty: PUSH EMPTY and PUSH AS <type> for the node payloads are both offered');
eq(p.click('PUSH EMPTY'), [{ url: '/api/process-nodes/21/push-empty', body: undefined }],
    'empty: PUSH EMPTY posts push-empty');
p = open(function () { ctx.confirmPushEmpty(21, ['PART-A', 'PART-B']); });
eq(p.click('PUSH AS TOTE-S'), [{ url: '/api/process-nodes/21/clear-bin', body: { bin_type_code: 'TOTE-S' } }],
    'empty: PUSH AS posts clear-bin with the type');

// A node with no payloads (a zero-payload loader's SynthClaim carries []) gets
// the whole catalog: every distinct bin_type_code in payload_bin_types.
p = open(function () { ctx.confirmPushEmpty(22, []); });
eq(p.buttons, ['PUSH EMPTY', 'PUSH AS TOTE-S', 'PUSH AS TOTE-L', 'PUSH AS RACK-C', 'CANCEL'],
    'empty: no payloads -> the full catalog');
p = open(function () { ctx.confirmPushEmpty(23, ['PART-UNMAPPED']); });
eq(p.buttons, ['PUSH EMPTY', 'PUSH AS TOTE-S', 'PUSH AS TOTE-L', 'PUSH AS RACK-C', 'CANCEL'],
    'empty: payloads that match nothing -> the full catalog');

// ── stage 2: a bare carrier in the window ──
// PUSH EMPTY is hidden, so PUSH AS <type> is the only way out. A zero-payload
// stage-2 loader offers the whole catalog, which never holds a bare type
// (Core refuses one in payload_bin_types).
p = open(function () { ctx.confirmPushEmpty(25, [], true); });
eq(p.buttons, ['PUSH AS TOTE-S', 'PUSH AS TOTE-L', 'PUSH AS RACK-C', 'CANCEL'],
    'stage 2: bare -> no PUSH EMPTY, PUSH AS for the full catalog');
eq(p.texts.indexOf('This carrier is bare. Push it out as its real type:') >= 0, true,
    'stage 2: says the carrier is bare');
eq(p.click('PUSH AS TOTE-L'), [{ url: '/api/process-nodes/25/clear-bin', body: { bin_type_code: 'TOTE-L' } }],
    'stage 2: PUSH AS re-stamps the carrier through clear-bin');
p = open(function () { ctx.confirmPushEmpty(26, [], false); });
eq(p.buttons[0], 'PUSH EMPTY', 'stage 2: a carrier that is not bare keeps PUSH EMPTY');

view = { payload_bin_types: [] };
p = open(function () { ctx.confirmPushEmpty(24, ['PART-A']); });
eq(p.buttons, ['PUSH EMPTY', 'CANCEL'], 'empty: no catalog -> PUSH EMPTY only');

if (failed) { console.error(passed + ' passed, ' + failed + ' FAILED'); process.exit(1); }
console.log('operator-render clear panels: ' + passed + ' passed');
