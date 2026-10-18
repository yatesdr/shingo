// composer-modal.test.js — the pick-a-part modal's decisions (pick-a-part U4).
//
// WHAT IS PINNED: which screen each tap leads to — the part list, "two off?"
// only when an enabled recipe has two produce slots, a live style used
// silently, one fitting recipe used without being shown, several fitting
// recipes asked about by name, none → see an engineer, the Positions screen
// only with the engineer's switch on — and the part rows' words: Core's
// verdict pill (owner, 2026-09-10), the style a part runs as, a CATID.
//
// WHAT IS REAL: composer-render.js and composer-model.js. WHAT IS STUBBED: the
// DOM (screens are asserted as the markup the module wrote), the picture
// renderer, and fetch (the composer read answers from FLOW; a preview answers
// nothing, so Go stays on "Checking…").
//
// Run by www/composer_modal_test.go.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let failures = 0;
let checks = 0;
const tests = [];
function test(name, fn) { tests.push([name, fn]); }

const modelSrc = fs.readFileSync(path.join(__dirname, 'composer-model.js'), 'utf8');
const modelFactory = vm.runInThisContext(
    '(function(module, exports, console){' + modelSrc + '\nreturn module.exports;})',
    { filename: 'composer-model.js' });
const modelMod = { exports: {} };
const ComposerModel = modelFactory(modelMod, modelMod.exports, console);

const escSrc = fs.readFileSync(
    path.join(__dirname, '..', '..', '..', '..', 'shared', 'esc.js'), 'utf8');
const esc = vm.runInThisContext(
    '(function(){' +
    escSrc.replace(/^import .*$/gm, '').replace(/export\s+function\s+esc\(/, 'function esc(') +
    '\nreturn esc;})',
    { filename: 'esc.js' })();

// A DOM element just real enough: innerHTML is what the module wrote, and the
// root keeps its click handler so a test can tap.
function makeEl(id) {
    const el = {
        id: id || '', hidden: true, innerHTML: '', dataset: {}, style: {}, handlers: {},
        classList: { add() {}, remove() {}, contains: () => false, toggle() {} },
        addEventListener(type, fn) { el.handlers[type] = fn; }, appendChild(c) { return c; }, focus() {},
        querySelector: () => null, querySelectorAll: () => [], insertBefore() {},
    };
    return el;
}
const els = {};
for (const id of ['os-comp-pick', 'os-comp-q']) els[id] = makeEl(id);
const doc = {
    readyState: 'complete',
    body: { dataset: {}, appendChild(n) { if (n.id) els[n.id] = n; return n; } },
    getElementById: id => els[id] || null,
    createElement: () => makeEl(''),
    addEventListener() {},
    querySelector: () => null,
    querySelectorAll: () => [],
};
const win = {
    location: { search: '', hash: '', pathname: '/operator/station/5' },
    addEventListener() {},
    ComposerModel: ComposerModel,
};

let FLOW = {};
const CALLS = [];
Object.assign(global, {
    window: win, document: doc, esc,
    renderFlowPicture: () => '<g class="picture"></g>',
    setTimeout: () => 0, clearTimeout: () => {}, requestAnimationFrame: fn => fn(),
    setInterval: () => 0, clearInterval: () => {},
    AbortController: class { constructor() { this.signal = {}; } abort() {} },
    fetch: (url, init) => {
        CALLS.push({ url: String(url), body: init && init.body ? JSON.parse(init.body) : null });
        return String(url).indexOf('/composer') >= 0
            ? Promise.resolve({ ok: true, status: 200, json: async () => FLOW })
            : new Promise(() => {});
    },
});

const src = fs.readFileSync(path.join(__dirname, 'composer-render.js'), 'utf8')
    .replace(/^import[\s\S]*?from\s+'[^']+';\s*$/gm, '')
    .replace(/^export\s*\{[^}]*\};?\s*$/gm, '')
    .replace(/^export\s+function\s+/gm, 'function ');
vm.runInThisContext('(function(){\n' + src + '\n})();', { filename: 'composer-render.js' });
const UI = win.ComposerUI;

// ── fixtures ────────────────────────────────────────────────────────────────
function style(id, name, parts) {
    return { id: id, name: name, claim_count: parts.length, parts: parts.map(p => ({ payload_code: p })) };
}
function recipe(id, name, roles) {
    return { id: String(id), name: name, where: 'PLN_01', cells: {},
        slots: roles.map((r, i) => ({ nodes: ['PLN_0' + (i + 1)], role: r })) };
}
function viewWith(gate) {
    return {
        process: { id: 1, active_style_id: 0, flow_composer_enabled: gate },
        station: { id: 5, name: 'Screen 5' },
        composer: {
            styles: [
                style(1, 'A-RUN', ['PART-A']),
                style(2, 'B-RUN', ['PART-B']),
                style(3, 'PAIR-AB', ['PART-A', 'PART-B']),
                style(4, 'WELD-RUN', ['IN-1', 'OUT-1']),
                style(5, 'R-RUN', ['PART-R']),
            ],
            recent_targets: [2],
        },
        sourcing_by_style: {
            'A-RUN': { code: 'green' },
            'B-RUN': { code: 'red', note: 'missing PART-B' },
        },
    };
}
const ROWS = ['PART-A', 'PART-B', 'PART-C', 'PART-D', 'PART-R', 'IN-1', 'OUT-1']
    .map(c => ({ code: c, name: c, catid: c === 'PART-A' ? 'CAT-A' : '' }));

function root() { return els['os-composer']; }
function html() { return root() ? root().innerHTML : ''; }
function tap(act, data) {
    const btn = { dataset: Object.assign({ act: act }, data || {}) };
    root().handlers.click({
        target: { closest: sel => (sel === '[data-act]' ? btn : null) },
        stopPropagation() {},
    });
}
function settle() { return new Promise(r => setImmediate(r)); }
function expectIn(what, needle) {
    if (html().indexOf(needle) < 0) throw new Error(what + ': the modal does not show "' + needle + '"\n      ' + html().slice(0, 400));
}
async function open(flow, gate) {
    UI.close();
    FLOW = flow;
    UI.openPicker(viewWith(!!gate));
    await settle(); await settle();
}

// ── the part list ───────────────────────────────────────────────────────────
test('the part list is the parts, with Core\'s verdict, the style each runs as, and a CATID', async () => {
    await open({ part_rows: ROWS, presets: [] });
    const box = els['os-comp-pick'].innerHTML;
    for (const want of ['PART-A', 'runs as A-RUN', 'CATID CAT-A', '<span class="vd ok">Parts available</span>',
        'No bin at Core', 'missing PART-B', '<span class="vd build">No flow yet</span>']) {
        if (box.indexOf(want) < 0) throw new Error('part list lacks "' + want + '"\n      ' + box.slice(0, 600));
    }
});
test('a style that runs several parts is a chip under each of its parts', async () => {
    await open({ part_rows: ROWS, presets: [] });
    const box = els['os-comp-pick'].innerHTML;
    if (box.indexOf('data-act="style" data-style="3"') < 0) throw new Error('no chip for PAIR-AB: ' + box.slice(0, 600));
});
test('RECENT is the parts of the last styles changed over to', async () => {
    await open({ part_rows: ROWS, presets: [] });
    const box = els['os-comp-pick'].innerHTML;
    const recent = box.indexOf('Recent'), all = box.indexOf('All parts'), b = box.indexOf('data-part="PART-B"');
    if (recent < 0 || !(recent < b && b < all)) throw new Error('PART-B is not under Recent: ' + box.slice(0, 600));
});

// ── two off? ────────────────────────────────────────────────────────────────
test('"two off?" is asked only when an enabled recipe has two PRODUCE slots', async () => {
    await open({ part_rows: ROWS, presets: [recipe(1, 'Standard', ['produce']), recipe(2, 'Weld', ['consume', 'produce'])] });
    tap('part', { part: 'PART-C' });
    if (html().indexOf('alone, or two off') >= 0) throw new Error('two off asked with no two-produce recipe');
    await open({ part_rows: ROWS, presets: [recipe(1, 'Standard', ['produce']), recipe(3, 'Pair', ['produce', 'produce'])] });
    tap('part', { part: 'PART-C' });
    expectIn('two-produce recipe', 'alone, or two off');
});

// ── resolve ─────────────────────────────────────────────────────────────────
test('a part that runs in a style goes straight to Go, silently', async () => {
    await open({ part_rows: ROWS, presets: [recipe(1, 'Standard', ['produce']), recipe(2, 'Retool', ['produce'])] });
    tap('part', { part: 'PART-A' });
    expectIn('running part', 'Checking what this changeover does');
});
test('a new part with one fitting recipe goes to Go without being asked', async () => {
    await open({ part_rows: ROWS, presets: [recipe(1, 'Standard', ['produce'])] });
    tap('part', { part: 'PART-C' });
    expectIn('one recipe', 'Checking what this changeover does');
});
test('a new part with two fitting recipes is asked by the engineer\'s names', async () => {
    await open({ part_rows: ROWS, presets: [recipe(1, 'Standard', ['produce']), recipe(2, 'Tooling change', ['produce'])] });
    tap('part', { part: 'PART-C' });
    expectIn('two recipes', 'Which one?');
    expectIn('two recipes', 'Standard');
    expectIn('two recipes', 'Tooling change');
    tap('recipe', { preset: '2' });
    expectIn('picked one', 'Checking what this changeover does');
});
test('no recipe takes the part: an engineer sets it up', async () => {
    await open({ part_rows: ROWS, presets: [recipe(2, 'Weld', ['consume', 'produce'])] });
    tap('part', { part: 'PART-C' });
    expectIn('no recipe', 'An engineer sets this up on the desktop.');
});
test('two off: the second part, then a two-produce recipe mints', async () => {
    await open({ part_rows: ROWS, presets: [recipe(3, 'Pair', ['produce', 'produce'])] });
    tap('part', { part: 'PART-C' });
    tap('second');
    const box = els['os-comp-pick'].innerHTML;
    if (box.indexOf('data-part="PART-C"') >= 0) throw new Error('the second list offers the first part again');
    tap('part', { part: 'PART-D' });
    expectIn('pair', 'Checking what this changeover does');
});
test('two off onto a pair that already runs resolves to it', async () => {
    await open({ part_rows: ROWS, presets: [recipe(3, 'Pair', ['produce', 'produce'])] });
    tap('part', { part: 'PART-A' });
    tap('second');
    tap('part', { part: 'PART-B' });
    expectIn('running pair', 'Checking what this changeover does');
});
test('a weld style picked whole goes to Go', async () => {
    await open({ part_rows: ROWS, presets: [] });
    tap('style', { style: '4' });
    expectIn('weld style', 'Checking what this changeover does');
});

test('Go previews the mint body and asks Core about the parts', async () => {
    await open({ part_rows: ROWS, presets: [recipe(7, 'Standard', ['produce'])] });
    CALLS.length = 0;
    tap('part', { part: 'PART-C' });
    const pv = CALLS.find(c => c.url.indexOf('/flow/preview') >= 0);
    if (!pv) throw new Error('Go made no preview: ' + JSON.stringify(CALLS));
    if (pv.url !== '/api/processes/1/flow/preview?preflight=1') throw new Error('preview url ' + pv.url);
    if (JSON.stringify(pv.body) !== '{"preset_id":7,"parts":[{"code":"PART-C"}],"station_id":5}') {
        throw new Error('preview body ' + JSON.stringify(pv.body));
    }
});

// ── positions (R-K) ─────────────────────────────────────────────────────────
test('the Positions screen appears only with the engineer\'s switch on', async () => {
    await open({ part_rows: ROWS, presets: [recipe(1, 'Standard', ['produce'])] }, true);
    tap('part', { part: 'PART-C' });
    expectIn('switch on', 'Positions · Standard');
    tap('next');
    expectIn('after positions', 'Checking what this changeover does');
});

(async () => {
    for (const [name, fn] of tests) {
        try {
            await fn();
            checks++;
        } catch (e) {
            failures++;
            console.error('FAIL  ' + name + '\n      ' + (e && e.message));
        }
    }
    if (failures > 0) {
        console.error('FAILED: ' + failures + ' of ' + (failures + checks));
        process.exit(1);
    }
    console.log('OK: ' + checks + ' checks passed');
})();
