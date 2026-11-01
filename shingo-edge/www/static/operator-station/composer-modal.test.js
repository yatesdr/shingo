// composer-modal.test.js — the station's changeover modal's decisions.
//
// WHAT IS PINNED: which screen each tap leads to — HMI setup off lists the
// styles and nothing else; on, the styles lead the part list, a
// part asks how the press runs it only when the recipes offer more than one
// way, parts that already run in a style ask Use or Set up new, setting
// up always passes the spots screen, the second part's list leads with the
// matching half — and the bodies Go previews, and what Go says. On a recipe
// with groups: what each recipe screen asks the picture to draw full, tag and
// hold still, the on-deck spot following its spot, and the part list asking
// by the groups' names.
//
// WHAT IS REAL: composer-render.js and composer-model.js. WHAT IS STUBBED: the
// DOM (screens are asserted as the markup the module wrote), the picture
// renderer (it records the options each picture was asked for, and reports
// PICW as the picture's width when a test sets it), and fetch (the composer
// read answers from FLOW; a preview answers PREVIEW when a test sets it and
// nothing otherwise, so Go stays on "Checking…").
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
// The station loads flowspec-data.js before this module (composer-boot.js), so
// the field words the screens print are flowspec's (F3).
{
    const sandbox = { window: {} };
    vm.runInNewContext(fs.readFileSync(path.join(__dirname, 'flowspec-data.js'), 'utf8'), sandbox,
        { filename: 'flowspec-data.js' });
    win.FLOWSPEC = sandbox.window.FLOWSPEC;
}

let FLOW = {};
let PREVIEW = null;
let MINT = null;       // flow/mint's answer, when a test sets it
let START = null;      // changeover/start's answer, when a test sets it
let PIC = {};          // the options the last picture was drawn with
const PICS = [];       // every picture's options, in order
let PICW = 0;          // the width the stub reports, when set
const CALLS = [];
Object.assign(global, {
    window: win, document: doc, esc,
    renderFlowPicture: (v, o) => {
        PIC = o || {};
        PICS.push(PIC);
        if (PICW) PIC.width = PICW;
        return '<g class="picture"></g>';
    },
    setTimeout: () => 0, clearTimeout: () => {}, requestAnimationFrame: fn => fn(),
    setInterval: () => 0, clearInterval: () => {},
    AbortController: class { constructor() { this.signal = {}; } abort() {} },
    fetch: (url, init) => {
        CALLS.push({ url: String(url), body: init && init.body ? JSON.parse(init.body) : null });
        if (String(url).indexOf('/composer') >= 0) return Promise.resolve({ ok: true, status: 200, json: async () => FLOW });
        if (PREVIEW && String(url).indexOf('/flow/preview') >= 0) {
            return Promise.resolve({ ok: true, status: 200, json: async () => PREVIEW });
        }
        if (MINT && String(url).indexOf('/flow/mint') >= 0) {
            return Promise.resolve({ ok: true, status: 200, json: async () => MINT });
        }
        if (START && String(url).indexOf('/changeover/start') >= 0) {
            return Promise.resolve({ ok: START.ok !== false, status: START.status || 200, json: async () => START.body || {} });
        }
        return new Promise(() => {});
    },
});

const src = fs.readFileSync(path.join(__dirname, 'composer-render.js'), 'utf8')
    .replace(/^import[\s\S]*?from\s+'[^']+';\s*$/gm, '')
    .replace(/^export\s*\{[^}]*\};?\s*$/gm, '')
    .replace(/^export\s+function\s+/gm, 'function ');
vm.runInThisContext('(function(){\n' + src + '\n})();', { filename: 'composer-render.js' });
const UI = win.ComposerUI;

// ── fixtures ────────────────────────────────────────────────────────────────
function style(id, name, parts, spots, nodes) {
    return { id: id, name: name, claim_count: parts.length, spots: spots || parts.length,
        claim_nodes: nodes || [], parts: parts.map(p => ({ payload_code: p })) };
}
function recipe(id, name, roles) {
    return { id: String(id), name: name, where: 'PLN_01', cells: {},
        slots: roles.map((r, i) => ({ nodes: ['PLN_0' + (i + 1)], role: r })) };
}
function viewWith(setup) {
    return {
        process: { id: 1, name: 'Press 6', active_style_id: 1, flow_composer_enabled: setup },
        station: { id: 5, name: 'Screen 5' },
        composer: {
            styles: [
                style(1, 'A-RUN', ['PART-A'], 1, ['PLN_01']),
                style(2, 'B-RUN', ['PART-B']),
                style(3, 'PAIR-AB', ['PART-A', 'PART-B']),
                style(6, 'A-SPLIT', ['PART-A'], 2),
            ],
            recent_targets: [2],
        },
        sourcing_by_style: {
            'B-RUN': { code: 'red', note: 'missing PART-B' },
            'PAIR-AB': { code: 'unchecked' },
        },
    };
}
const ROWS = ['PART-A', 'PART-B', 'PART-C', 'PART-D', 'PART-E', 'IN-1', 'OUT-1']
    .map(c => ({ code: c, name: c, catid: c === 'PART-A' ? 'CAT-A' : '' }));
const ONE = recipe(1, 'One spot', ['produce']);
const TWO = recipe(3, 'Two spot', ['produce', 'produce']);

function root() { return els['os-composer']; }
function html() { return root() ? root().innerHTML : ''; }
function box() { return els['os-comp-pick'].innerHTML; }
function tap(act, data) {
    const btn = { dataset: Object.assign({ act: act }, data || {}) };
    root().handlers.click({
        target: { closest: sel => (sel === '[data-act]' ? btn : null) },
        stopPropagation() {},
    });
}
function settle() { return new Promise(r => setImmediate(r)); }
function expectIn(what, needle, where) {
    const h = where === undefined ? html() : where;
    if (h.indexOf(needle) < 0) throw new Error(what + ': does not show "' + needle + '"\n      ' + h.slice(0, 500));
}
function expectOut(what, needle, where) {
    const h = where === undefined ? html() : where;
    if (h.indexOf(needle) >= 0) throw new Error(what + ': shows "' + needle + '"\n      ' + h.slice(0, 500));
}
async function open(flow, setup, view) {
    UI.close();
    FLOW = flow;
    PREVIEW = null;
    MINT = null;
    START = null;
    PICW = 0;
    UI.openPicker(view || viewWith(setup !== false));
    await settle(); await settle();
}
// urls is every request since the last CALLS reset, by path and query.
function urls() { return CALLS.map(c => c.url); }
// sortedKeys is a map as JSON with its keys in order: a body's map, read for what it says.
function sortedKeys(o) { return JSON.stringify(Object.keys(o || {}).sort().reduce((m, k) => { m[k] = o[k]; return m; }, {})); }
function bodyOf(part) {
    const c = CALLS.filter(x => x.url.indexOf(part) >= 0).pop();
    return c ? JSON.stringify(c.body) : '';
}
// typeQ types into the picker's search box.
function typeQ(text) {
    els['os-comp-q'].value = text;
    els['os-comp-q'].handlers.input();
}
function lastPreview() {
    const pv = CALLS.filter(c => c.url.indexOf('/flow/preview') >= 0).pop();
    if (!pv) throw new Error('Go made no preview: ' + JSON.stringify(CALLS));
    return pv.body;
}

// ── HMI setup off: the styles and nothing else ────────────────────────
test('setup off lists the styles, the running one first and marked, and no parts', async () => {
    await open({ part_rows: ROWS, presets: [ONE] }, false);
    const b = box();
    expectIn('saved list', '>Styles<', b);
    expectIn('saved list', 'data-act="saved" data-style="1"', b);
    expectIn('running', '<span class="vd ok">Running</span>', b);
    expectOut('saved list', 'data-act="part"', b);
    if (b.indexOf('data-style="1"') > b.indexOf('data-style="2"')) throw new Error('the running style is not first');
});
test('setup off: a style tapped asks "Change over to …?" and reads nothing', async () => {
    await open({ part_rows: ROWS, presets: [ONE] }, false);
    CALLS.length = 0;
    tap('saved', { style: '2' });
    expectIn('confirm', 'Change over to B-RUN?');
    expectOut('confirm', 'Checking what this changeover does');
    if (CALLS.length) throw new Error('the tap sent ' + urls().join());
});

// ── CHANGEOVER to Start, a style tapped, by the switch ──────────────────────
// Every request the station sends from the CHANGEOVER tap to a started
// changeover, and every screen between, for a style tapped under Styles.
const B_PREVIEW = {
    order_count: 2,
    actions: [
        { core_node_name: 'PLN_06', evac_order: { from: 'PLN_06', to: 'ULN_002' } },
        { core_node_name: 'PLN_09', supply_order: { from: 'SMN_BUF_100', to: 'PLN_09', payload_code: 'PART-B' } },
    ],
    mint: { style_id: 2, style_name: 'B-RUN', cells: [{ core_node_name: 'PLN_09', role: 'produce',
        swap_mode: 'two_robot_press_index', payload_code: 'PART-B' }] },
};
async function styleToStart(setup) {
    UI.close();
    CALLS.length = 0;
    FLOW = { part_rows: ROWS, presets: [ONE] };
    PREVIEW = B_PREVIEW;
    MINT = { style_id: 2, style_name: 'B-RUN', fingerprint: 'fp-2' };
    START = { body: { id: 9 } };
    UI.openPicker(viewWith(setup));
    await settle(); await settle();
    const opened = urls();
    tap('saved', { style: '2' });
    await settle(); await settle();
    const card = html();
    const tapped = urls().slice(opened.length);
    tap('start');
    await settle(); await settle(); await settle();
    return { opened: opened, card: card, tapped: tapped, started: urls().slice(opened.length + tapped.length), after: html() };
}
test('setup on: a style tapped is the card, then mint and start on its fingerprint', async () => {
    const r = await styleToStart(true);
    if (r.opened.join() !== '/api/operator-stations/5/composer') throw new Error('on open: ' + r.opened.join());
    if (r.tapped.join() !== '/api/processes/1/flow/preview?preflight=1') throw new Error('on the tap: ' + r.tapped.join());
    for (const want of ['Change over to B-RUN?', 'Takes the bins off PLN_06 to ULN_002.', 'class="picture"',
        'os-comp-trips', 'data-act="back"', 'Start changeover']) {
        expectIn('the card', want, r.card);
    }
    if (r.started.join() !== '/api/processes/1/flow/mint,/api/processes/1/changeover/start') {
        throw new Error('on Start: ' + r.started.join());
    }
    if (bodyOf('/flow/mint') !== '{"style_id":2,"station_id":5}') throw new Error('mint ' + bodyOf('/flow/mint'));
    if (bodyOf('/changeover/start') !== '{"to_style_id":2,"called_by":"Screen 5","flow_fingerprint":"fp-2"}') {
        throw new Error('start ' + bodyOf('/changeover/start'));
    }
    expectIn('started', 'Changeover started', r.after);
});
test('setup off: a style tapped is the confirm, and Start is the start alone', async () => {
    const r = await styleToStart(false);
    if (r.opened.length) throw new Error('on open: ' + r.opened.join());
    if (r.tapped.length) throw new Error('on the tap: ' + r.tapped.join());
    for (const want of ['Change over to B-RUN?', 'data-act="back">Back<', 'data-act="start">Start<']) {
        expectIn('the confirm', want, r.card);
    }
    for (const not of ['class="picture"', 'os-comp-trips', 'Start changeover', 'os-comp-sentence']) {
        expectOut('the confirm', not, r.card);
    }
    if (r.started.join() !== '/api/processes/1/changeover/start') throw new Error('on Start: ' + r.started.join());
    if (bodyOf('/changeover/start') !== '{"to_style_id":2,"called_by":"Screen 5"}') {
        throw new Error('start ' + bodyOf('/changeover/start'));
    }
    expectIn('started', 'Changeover started', r.after);
    expectIn('started', 'B-RUN.', r.after);
});
test('setup off: Back from the confirm is the style list', async () => {
    await open({ part_rows: ROWS, presets: [ONE] }, false);
    tap('saved', { style: '2' });
    tap('back');
    expectIn('back', 'data-act="saved" data-style="2"', box());
});
test('setup off: the running style tapped says so and offers no Start', async () => {
    await open({ part_rows: ROWS, presets: [ONE] }, false);
    CALLS.length = 0;
    tap('saved', { style: '1' });
    await settle(); await settle();
    expectIn('running', 'A-RUN is already running — there is nothing to change over.');
    expectOut('running', 'data-act="start"');
    if (CALLS.length) throw new Error('the tap sent ' + urls().join());
});
test('setup off: a refused start says why where Start was', async () => {
    await open({ part_rows: ROWS, presets: [ONE] }, false);
    PREVIEW = B_PREVIEW;
    MINT = { style_id: 2, style_name: 'B-RUN', fingerprint: 'fp-2' };
    START = { ok: false, status: 400, body: { error: 'PLN_09 has an order in flight' } };
    const say = { textContent: '', className: '' };
    tap('saved', { style: '2' });
    await settle(); await settle();
    root().querySelector = sel => (sel === '.os-comp-say' ? { querySelector: () => say, appendChild() {} } : null);
    tap('start');
    await settle(); await settle(); await settle();
    root().querySelector = () => null;
    if (say.textContent !== 'PLN_09 has an order in flight') throw new Error('refusal: "' + say.textContent + '"');
});
test('setup off has no Set up a part; setup on has it', async () => {
    await open({ part_rows: ROWS, presets: [ONE] }, false);
    expectOut('setup off', 'Set up a part', box());
    expectOut('setup off', 'data-act="part"', box());
    await open({ part_rows: ROWS, presets: [ONE] }, true);
    expectIn('setup on', 'Set up a part', box());
});
test('the search box lets the touch keyboard up: no inputmode none, not focused on open', async () => {
    let focused = 0;
    els['os-comp-q'].focus = () => { focused++; };
    await open({ part_rows: ROWS, presets: [ONE, TWO] }, true);
    expectIn('picker', 'placeholder="type a part number">');
    expectOut('picker', 'inputmode');
    expectOut('picker', 'readonly');
    tap('part', { part: 'PART-C' });
    tap('two');
    expectOut('second part', 'inputmode');
    els['os-comp-q'].focus = () => {};
    if (focused) throw new Error('focused ' + focused + ' times on open; the tap is what raises the keyboard');
});
test('the search box: "type a part number", no CATID shown, a search by CATID still finds it', async () => {
    const v = viewWith(true);
    v.composer.styles[1].catid = 'CAT-B';
    await open({ part_rows: ROWS, presets: [ONE, TWO] }, true, v);
    expectIn('picker', 'placeholder="type a part number"');
    expectOut('picker', 'scan');
    expectOut('picker rows', 'CATID', box());
    typeQ('cat-a');
    expectIn('search by CATID', 'data-part="PART-A"', box());
    expectOut('search by CATID', 'data-part="PART-C"', box());
    typeQ('cat-b');
    expectIn('a style by CATID', 'data-style="2"', box());
    typeQ('');
    tap('part', { part: 'PART-C' });
    tap('two');
    expectIn('second part', 'placeholder="type a part number"');
    expectOut('second part rows', 'CATID', box());
    const off = viewWith(false);
    off.composer.styles[1].catid = 'CAT-B';
    await open({ part_rows: ROWS, presets: [ONE] }, false, off);
    expectIn('setup off', 'placeholder="type a part number"');
    expectOut('setup off rows', 'CATID', box());
});

// ── HMI setup on ─────────────────────────────────────────────────────────────
test('setup on: the styles lead, then the parts with Core\'s word', async () => {
    await open({ part_rows: ROWS, presets: [ONE] });
    const b = box();
    if (!(b.indexOf('>Styles<') < b.indexOf('Set up a part'))) throw new Error('order: ' + b.slice(0, 400));
    for (const want of ['data-part="PART-C"', 'in A-RUN', 'No bin at Core', 'Stock not checked',
        '<span class="vd build">Not set up yet</span>']) {
        expectIn('part list', want, b);
    }
});
test('a part asks how the press runs it only when the recipes offer more than one way', async () => {
    await open({ part_rows: ROWS, presets: [ONE] });
    tap('part', { part: 'PART-C' });
    expectOut('one way only', 'how does the press run it');
    await open({ part_rows: ROWS, presets: [ONE, TWO] });
    tap('part', { part: 'PART-C' });
    for (const w of ['One Part, One Spot', 'Two Parts, Two Spots', 'One Part, Two Spots']) expectIn('three ways', w);
    await open({ part_rows: ROWS, presets: [TWO] });
    tap('part', { part: 'PART-C' });
    expectOut('two-spot recipe only', 'One Part, One Spot');
    expectIn('two-spot recipe only', 'One Part, Two Spots');
});
test('setting up always passes the spots screen, then Go previews the mint', async () => {
    await open({ part_rows: ROWS, presets: [ONE] });
    CALLS.length = 0;
    tap('part', { part: 'PART-C' });
    expectIn('spots', 'Spots · One spot');
    tap('next');
    expectIn('go', 'Checking what this changeover does');
    if (JSON.stringify(lastPreview()) !== '{"preset_id":1,"parts":[{"code":"PART-C"}],"station_id":5}') {
        throw new Error('body ' + JSON.stringify(lastPreview()));
    }
});
test('parts that already run in a style ask Use style or Set up new', async () => {
    await open({ part_rows: ROWS, presets: [ONE, TWO] });
    tap('part', { part: 'PART-A' });
    tap('one');
    expectIn('reuse', 'Use saved style');
    expectIn('reuse', 'data-style="1"');
    expectOut('reuse', 'data-style="6"');   // the two-spot A is a different style
    CALLS.length = 0;
    tap('saved', { style: '1' });
    if (lastPreview().style_id !== 1) throw new Error('use did not preview the style');
    await open({ part_rows: ROWS, presets: [ONE, TWO] });
    tap('part', { part: 'PART-A' });
    tap('one');
    tap('setupnew');
    expectIn('set up new', 'Spots · One spot');
    CALLS.length = 0;
    tap('next');
    if (lastPreview().new !== true) throw new Error('set up new did not say so: ' + JSON.stringify(lastPreview()));
});
test('two fitting recipes are asked by the engineer\'s names', async () => {
    await open({ part_rows: ROWS, presets: [ONE, recipe(2, 'Tooling change', ['produce'])] });
    tap('part', { part: 'PART-C' });
    expectIn('two recipes', 'Which recipe?');
    expectIn('two recipes', 'Tooling change');
    tap('recipe', { preset: '2' });
    expectIn('picked one', 'Spots · Tooling change');
});
test('no recipe runs it: an engineer adds one on the desktop', async () => {
    await open({ part_rows: ROWS, presets: [recipe(2, 'Weld', ['consume', 'produce'])] });
    tap('part', { part: 'PART-C' });
    expectIn('no recipe', 'An engineer adds the recipe on the desktop: Processes › Press 6 › Recipes.');
});
test('Two Parts, Two Spots: the second list leads with the matching half', async () => {
    const rows = ROWS.concat(['SYN44-PARTA.06', 'SYN45-PARTA.06', 'SYN43-PARTA.06', 'SYN71-PARTB.06']
        .map(c => ({ code: c, name: c })));
    await open({ part_rows: rows, presets: [ONE, TWO] });
    tap('part', { part: 'SYN44-PARTA.06' });
    tap('two');
    const b = box();
    const near = b.indexOf('Matching half'), all = b.indexOf('All parts');
    if (near < 0 || !(near < b.indexOf('SYN45-PARTA.06') && b.indexOf('SYN43-PARTA.06') < all)) {
        throw new Error('both neighbours do not lead: ' + b.slice(0, 600));
    }
    if (b.indexOf('SYN71-PARTB.06') < all) throw new Error('an unrelated part is in the matching half');
    expectOut('second list', 'data-part="SYN44-PARTA.06"', b);
    CALLS.length = 0;
    tap('part', { part: 'SYN45-PARTA.06' });
    tap('next');
    if (JSON.stringify(lastPreview().parts) !== '[{"code":"SYN44-PARTA.06"},{"code":"SYN45-PARTA.06"}]') {
        throw new Error('pair body ' + JSON.stringify(lastPreview()));
    }
});
test('One Part, Two Spots mints the one part with split', async () => {
    await open({ part_rows: ROWS, presets: [ONE, TWO] });
    tap('part', { part: 'PART-C' });
    tap('split');
    expectIn('split spots', 'PART-C · One Part, Two Spots');
    CALLS.length = 0;
    tap('next');
    const b = lastPreview();
    if (b.split !== true || b.preset_id !== 3 || b.parts.length !== 1) throw new Error('split body ' + JSON.stringify(b));
});

// ── spots: Press 6, on the server's own card ─────────────────────────────────
//
// The owner's rule (2026-10-22): the floor may pick pairings, but only the
// ones the engineer set up in recipes, and an on-deck spot never feeds the
// other side. The card is the station's composer read of the Press 6 fixture
// (www/handlers_flow_spots_test.go), written by the Go wrapper; a tap looks
// its targets up there and restates nothing.
const PRESS6 = process.argv[2] ? JSON.parse(fs.readFileSync(process.argv[2], 'utf8')) : null;
function tapPos(node) {
    root().handlers.click({
        target: { closest: sel => (sel === '[data-pos]' ? { dataset: { pos: node } } : null) },
        stopPropagation() {},
    });
}
function tapLane(node) {
    root().handlers.click({
        target: { closest: sel => (sel === '[data-staging]' ? { dataset: { staging: node } } : null) },
        stopPropagation() {},
    });
}
function chips() { return (html().match(/data-act="unmove" data-node="[^"]*">[^<]*/g) || []).map(c => c.split('>')[1]); }
function lit() { return (PIC.targets || []).slice().sort(); }
function hint() {
    const m = /class="os-comp-hint">([\s\S]*?)<\/div>/.exec(html());
    return m ? m[1].replace(/<[^>]+>/g, '') : '';
}
function press6(edit) {
    const flow = JSON.parse(JSON.stringify(PRESS6));
    if (edit) edit(flow);
    return flow;
}
function card(flow, name) { return flow.presets.find(p => p.name === name); }
async function twoOff(flow) {
    await open(flow);
    tap('part', { part: 'SYN-P6-X' });
    tap('two');
    tap('part', { part: 'SYN-P6-Y' });
    expectIn('two off', 'Spots · Two spot press index');
}
async function oneOff(flow) {
    await open(flow);
    tap('part', { part: 'SYN-P6-Z' });
    tap('one');
    expectIn('one off', 'Spots · One spot press index');
}
function spotsTest(name, fn) {
    test(name, async () => {
        if (!PRESS6) throw new Error('no Press 6 read: run through www/composer_modal_test.go');
        await fn();
    });
}
spotsTest('spots open where the cell last ran the recipe, as the server shipped it, and no chip until a part moves', async () => {
    await twoOff(press6());
    if (chips().length) throw new Error('chips before a move: ' + JSON.stringify(chips()));
    CALLS.length = 0;
    tap('next');
    if (JSON.stringify(lastPreview().overrides) !== '{"PLN_006":"PLN_010","PLN_007":"PLN_012"}') {
        throw new Error('pre-fill: ' + JSON.stringify(lastPreview()));
    }
});
spotsTest('a press spot lights only the spots its recipes pair with its deck, and outlines itself', async () => {
    await twoOff(press6());
    tapPos('PLN_010');
    if (PIC.selected !== 'PLN_010' || PIC.selectCard !== true) throw new Error('selection: ' + JSON.stringify(PIC));
    if (JSON.stringify(lit()) !== '["PLN_006","PLN_009"]') throw new Error('lit: ' + JSON.stringify(lit()));
    expectIn('hint', 'PLN_010 · where this part runs', hint());
});
spotsTest('an on-deck card with nowhere to go selects nothing, and PLN_013 moves nothing', async () => {
    await twoOff(press6());
    tapPos('PLN_005');
    expectIn('hint', 'This is where the parts go', hint());
    if (PIC.selected || lit().length) throw new Error('deck: ' + JSON.stringify(PIC));
    tapPos('PLN_013');
    if (chips().length || PIC.selected) throw new Error('moved: ' + JSON.stringify(chips()) + ' ' + PIC.selected);
});
spotsTest('a press then its own deck keeps the press; nothing swaps', async () => {
    await twoOff(press6());
    tapPos('PLN_010');
    tapPos('PLN_005');
    if (PIC.selected !== 'PLN_010' || chips().length) throw new Error('swapped: ' + PIC.selected + ' ' + JSON.stringify(chips()));
});
spotsTest('a spot then another spot re-selects it; the decks never cross (was: a swap)', async () => {
    await twoOff(press6());
    tapPos('PLN_010');
    tapPos('PLN_012');
    if (PIC.selected !== 'PLN_012') throw new Error('selected ' + PIC.selected);
    if (chips().length) throw new Error('swapped: ' + JSON.stringify(chips()));
});
spotsTest('a press spot moves to a lit spot and back, its ✕ is the undo, and Go sends what the screen shows', async () => {
    await twoOff(press6());
    tapPos('PLN_010'); tapPos('PLN_009');
    if (JSON.stringify(chips()) !== '["PLN_010 → PLN_009 ✕"]') throw new Error('move: ' + JSON.stringify(chips()));
    tapPos('PLN_009'); tapPos('PLN_010');
    if (chips().length) throw new Error('moved back: ' + JSON.stringify(chips()));
    tapPos('PLN_010'); tapPos('PLN_006');
    if (JSON.stringify(chips()) !== '["PLN_010 → PLN_006 ✕"]') throw new Error('to the recipe spot: ' + JSON.stringify(chips()));
    tap('unmove', { node: 'PLN_006' });
    if (chips().length) throw new Error('undo: ' + JSON.stringify(chips()));
    CALLS.length = 0;
    tap('next');
    if (sortedKeys(lastPreview().overrides) !== '{"PLN_006":"PLN_010","PLN_007":"PLN_012"}') {
        throw new Error('body ' + JSON.stringify(lastPreview()));
    }
});
spotsTest('a lane moves only to a lane, and its chip names its job', async () => {
    await oneOff(press6());
    tapLane('STG_TOOL_1');
    if (JSON.stringify(lit()) !== '["STG_TOOL_2"]') throw new Error('lit: ' + JSON.stringify(lit()));
    tapPos('PLN_011');
    if (chips().length) throw new Error('a lane went to a position: ' + JSON.stringify(chips()));
    tapLane('STG_TOOL_2');
    if (JSON.stringify(chips()) !== '["Inbound staging for PLN_009: STG_TOOL_1 → STG_TOOL_2 ✕"]') {
        throw new Error('chip: ' + JSON.stringify(chips()));
    }
});
spotsTest('an on-deck spot an engineer set up moves, and its chip names whose it is', async () => {
    // As if a recipe paired PLN_006 with PLN_013 as well: the card's targets
    // are all the screen reads.
    await twoOff(press6(f => card(f, 'Two spot press index').spots.find(s => s.node === 'PLN_005').targets.push('PLN_013')));
    tapPos('PLN_005');
    if (JSON.stringify(lit()) !== '["PLN_013"]') throw new Error('lit: ' + JSON.stringify(lit()));
    tapPos('PLN_013');
    if (chips().indexOf('on deck for PLN_010: PLN_005 → PLN_013 ✕') < 0) throw new Error('chip: ' + JSON.stringify(chips()));
});
spotsTest('a recipe naming a node the cell lost is shown with its sentence and not offered', async () => {
    const said = 'Two spot press index sends cell 2 to ULN_003, which is no longer a position here — ' +
        'an engineer re-saves the recipe on the desktop';
    const flow = press6(f => {
        const c = card(f, 'Two spot press index');
        f.presets.push(Object.assign({}, c, { id: '99', name: 'Two spot copy' }));
        c.stale = [said];
    });
    await open(flow);
    tap('part', { part: 'SYN-P6-X' });
    tap('two');
    tap('part', { part: 'SYN-P6-Y' });
    expectIn('stale shown', said);
    expectOut('stale offered', 'data-act="recipe" data-preset="' + card(flow, 'Two spot press index').id + '"');
    expectIn('the live one offered', 'data-act="recipe" data-preset="99"');
});

// What the recipe screens ask the picture for, read back from the stub: the
// nodes drawn full (everything else fades), the L/R tags, and the deck
// pairing the layout holds still while the operator moves things.
function sorted(xs) { return JSON.stringify((xs || []).slice().sort()); }
function tagsOf(o) {
    const t = (o || PIC).tags || {};
    return Object.keys(t).sort().map(n => n + ':' + t[n].letter + t[n].group).join(' ');
}
function pairsOf(o) {
    const p = (o || PIC).pairs || {};
    return Object.keys(p).sort().map(n => n + '<' + p[n].slice().sort().join(',')).join(' ');
}
spotsTest('an ungrouped recipe is drawn by relation, the recipe full but a deck with nowhere to go, nothing tagged', async () => {
    await twoOff(press6());
    if (PIC.relation !== true) throw new Error('relation: ' + PIC.relation);
    if (sorted(PIC.active) !== '["PLN_010","PLN_012"]') throw new Error('active: ' + sorted(PIC.active));
    if (tagsOf()) throw new Error('tags on an ungrouped recipe: ' + tagsOf());
    if (pairsOf() !== 'PLN_005<PLN_010 PLN_008<PLN_012') throw new Error('pairs: ' + pairsOf());
    expectIn('hint', 'This is where the parts go. Tap one to change where it is · as it is? Next', hint());
    tapPos('PLN_010');
    if (sorted(PIC.active) !== '["PLN_006","PLN_009","PLN_010","PLN_012"]') {
        throw new Error('active with PLN_010 selected: ' + sorted(PIC.active));
    }
    if (pairsOf() !== 'PLN_005<PLN_010 PLN_008<PLN_012') throw new Error('the pairing moved on a tap: ' + pairsOf());
});

// ── spots on recipe groups: the server's grouped card ────────────────────────
//
// The second read is the Go lane's grouped fixture (newPressGrouped): "Two
// spot press index" re-saved with Left Hand on PLN_006 or PLN_010 (PLN_005 on
// deck) or PLN_013 (PLN_014 on deck — synthetic, so the card has a carry) and
// Right Hand on PLN_007 or PLN_012 (PLN_008 on deck); "One spot press index"
// stays ungrouped on the same process. The card ships at and carry; the
// screen looks them up.
const GROUPED = process.argv[3] ? JSON.parse(fs.readFileSync(process.argv[3], 'utf8')) : null;
function grouped(edit) {
    const flow = JSON.parse(JSON.stringify(GROUPED));
    if (edit) edit(flow);
    return flow;
}
function groupedTest(name, fn) {
    test(name, async () => {
        if (!GROUPED) throw new Error('no grouped Press 6 read: run through www/composer_modal_test.go');
        await fn();
    });
}
const CARRIED_CHIPS = '["PLN_010 → PLN_013 (on deck PLN_014) ✕"]';
groupedTest('grouped: nothing selected, the press spots in play are full, each hand tagged, the pairing held', async () => {
    await twoOff(grouped());
    if (chips().length) throw new Error('chips before a move: ' + JSON.stringify(chips()));
    if (PIC.relation !== true) throw new Error('relation: ' + PIC.relation);
    if (sorted(PIC.active) !== '["PLN_010","PLN_012"]') throw new Error('active: ' + sorted(PIC.active));
    if (tagsOf() !== 'PLN_010:L1 PLN_012:R2') throw new Error('tags: ' + tagsOf());
    if (pairsOf() !== 'PLN_005<PLN_006,PLN_010,PLN_013 PLN_008<PLN_007,PLN_012') throw new Error('pairs: ' + pairsOf());
    // No on-deck spot here can move, so the hint names the press spots only.
    expectIn('hint', 'Where this cell last ran the recipe. Tap a press spot to change it · as it is? Next', hint());
    expectIn('hint, bold', 'Tap a <b>press spot</b> to change it');
});
groupedTest('grouped: PLN_010 lights only Left Hand\'s other spots, tagged, and the hint names the group', async () => {
    await twoOff(grouped());
    tapPos('PLN_010');
    if (PIC.selected !== 'PLN_010' || PIC.selectCard !== true) throw new Error('selection: ' + PIC.selected);
    if (JSON.stringify(lit()) !== '["PLN_006","PLN_013"]') throw new Error('lit: ' + JSON.stringify(lit()));
    if (sorted(PIC.active) !== '["PLN_006","PLN_010","PLN_012","PLN_013"]') {
        throw new Error('active: ' + sorted(PIC.active));
    }
    if (tagsOf() !== 'PLN_006:L1 PLN_010:L1 PLN_012:R2 PLN_013:L1') throw new Error('tags: ' + tagsOf());
    expectIn('hint', 'PLN_010 · Left Hand · where this part runs. Tap a spot marked move here', hint());
    if (pairsOf() !== 'PLN_005<PLN_006,PLN_010,PLN_013 PLN_008<PLN_007,PLN_012') throw new Error('pairs: ' + pairsOf());
});
groupedTest('grouped: PLN_005, with nowhere to go at PLN_010, is not selected', async () => {
    await twoOff(grouped());
    tapPos('PLN_005');
    if (PIC.selected || lit().length) throw new Error('deck: ' + PIC.selected + ' lit ' + JSON.stringify(lit()));
    expectIn('hint', 'Where this cell last ran the recipe. Tap a press spot to change it', hint());
});
groupedTest('grouped: a spot moved to PLN_013 carries its deck, and one chip says so', async () => {
    await twoOff(grouped());
    tapPos('PLN_010'); tapPos('PLN_013');
    if (JSON.stringify(chips().sort()) !== CARRIED_CHIPS) throw new Error('carry: ' + JSON.stringify(chips()));
    if (sorted(PIC.active) !== '["PLN_012","PLN_013"]') throw new Error('active: ' + sorted(PIC.active));
    if (tagsOf() !== 'PLN_012:R2 PLN_013:L1') throw new Error('tags: ' + tagsOf());
    if (pairsOf() !== 'PLN_005<PLN_006,PLN_010,PLN_013 PLN_008<PLN_007,PLN_012') throw new Error('the pairing moved: ' + pairsOf());
    tapPos('PLN_014');
    if (PIC.selected || lit().length) throw new Error('the carried deck, with nowhere to go, was selected: ' + PIC.selected);
});
groupedTest('grouped: the carry chip\'s ✕ undoes the spot and its deck together', async () => {
    await twoOff(grouped());
    tapPos('PLN_010'); tapPos('PLN_013');
    tap('unmove', { node: 'PLN_006' });
    if (chips().length) throw new Error('undo: ' + JSON.stringify(chips()));
    CALLS.length = 0;
    tap('next');
    if (sortedKeys(lastPreview().overrides) !== '{"PLN_006":"PLN_010","PLN_007":"PLN_012"}') {
        throw new Error('body ' + JSON.stringify(lastPreview()));
    }
});
groupedTest('grouped: back from PLN_013 to PLN_010 the deck goes home', async () => {
    await twoOff(grouped());
    tapPos('PLN_010'); tapPos('PLN_013');
    tapPos('PLN_013'); tapPos('PLN_010');
    if (chips().length) throw new Error('back: ' + JSON.stringify(chips()));
});
groupedTest('grouped: the deck lights the list of the stand its spot stands on now', async () => {
    // As if Left Hand listed PLN_015 on deck for PLN_013 as well: the card's
    // at is all the screen reads.
    await twoOff(grouped(f => card(f, 'Two spot press index').spots.find(s => s.node === 'PLN_005').at.PLN_013.push('PLN_015')));
    tapPos('PLN_010'); tapPos('PLN_013'); tapPos('PLN_014');
    if (JSON.stringify(lit()) !== '["PLN_015"]') throw new Error('lit at PLN_013: ' + JSON.stringify(lit()));
    tapPos('PLN_015');
    if (JSON.stringify(chips()) !== '["PLN_010 → PLN_013 (on deck PLN_015) ✕"]') {
        throw new Error('chips: ' + JSON.stringify(chips()));
    }
});
groupedTest('grouped: Go previews the carried move with the spot\'s, and draws the flow by relation', async () => {
    await twoOff(grouped());
    tapPos('PLN_010'); tapPos('PLN_013');
    CALLS.length = 0;
    PREVIEW = { order_count: 0, actions: [], mint: { minted: true, style_name: 'SYN-P6-X + SYN-P6-Y', cells: [
        { core_node_name: 'PLN_013', role: 'produce', swap_mode: 'two_robot_press_index', paired_core_node: 'PLN_014',
            payload_code: 'SYN-P6-X' },
        { core_node_name: 'PLN_012', role: 'produce', swap_mode: 'two_robot_press_index', paired_core_node: 'PLN_008',
            payload_code: 'SYN-P6-Y' }] } };
    tap('next');
    const o = lastPreview().overrides || {};
    if (JSON.stringify(Object.keys(o).sort().map(k => k + '→' + o[k])) !== '["PLN_005→PLN_014","PLN_006→PLN_013","PLN_007→PLN_012"]') {
        throw new Error('body ' + JSON.stringify(lastPreview()));
    }
    await settle(); await settle();
    expectIn('go', 'class="picture"');
    if (PIC.relation !== true) throw new Error('Go relation: ' + PIC.relation);
    if (sorted(PIC.active) !== '["PLN_008","PLN_012","PLN_013","PLN_014"]') throw new Error('Go active: ' + sorted(PIC.active));
    if (tagsOf() !== 'PLN_012:R2 PLN_013:L1') throw new Error('Go tags: ' + tagsOf());
    if (pairsOf() !== 'PLN_005<PLN_006,PLN_010,PLN_013 PLN_008<PLN_007,PLN_012') throw new Error('Go pairs: ' + pairsOf());
});
groupedTest('grouped: the part list asks for the Right Hand part, and the parts are named by hand', async () => {
    await open(grouped());
    tap('part', { part: 'SYN-P6-X' });
    tap('two');
    expectIn('second sheet', '<h2>The Right Hand part?</h2>');
    tap('part', { part: 'SYN-P6-Y' });
    expectIn('spots', '<span class="sub">SYN-P6-X (Left Hand) + SYN-P6-Y (Right Hand)</span>');
    // A saved style places its parts by its own claims: the reuse screen
    // names no hand, and Set up new does.
    await open(grouped());
    tap('part', { part: 'PART-A' });
    tap('two');
    tap('part', { part: 'PART-B' });
    expectIn('reuse', '<h2>PART-A + PART-B</h2>');
    tap('setupnew');
    expectIn('set up new', '<span class="sub">PART-A (Left Hand) + PART-B (Right Hand)</span>');
    await open(press6());
    tap('part', { part: 'SYN-P6-X' });
    tap('two');
    expectIn('second sheet, no groups', '<h2>Second part</h2>');
    tap('part', { part: 'SYN-P6-Y' });
    expectIn('spots, no groups', '<span class="sub">SYN-P6-X + SYN-P6-Y</span>');
});
groupedTest('grouped: Which recipe draws each card by relation, its hands tagged, as wide as the picture', async () => {
    const flow = grouped(f => f.presets.push(Object.assign({}, card(f, 'Two spot press index'), { id: '99', name: 'Two spot copy' })));
    await open(flow);
    PICW = 1500;
    PICS.length = 0;
    tap('part', { part: 'SYN-P6-X' });
    tap('two');
    tap('part', { part: 'SYN-P6-Y' });
    expectIn('which', 'Which recipe? · SYN-P6-X (Left Hand) + SYN-P6-Y (Right Hand)');
    if (PICS.length !== 2) throw new Error(PICS.length + ' pictures, want 2');
    for (const o of PICS) {
        if (o.relation !== true) throw new Error('Which relation: ' + o.relation);
        if (sorted(o.active) !== '["PLN_005","PLN_006","PLN_007","PLN_008"]') throw new Error('Which active: ' + sorted(o.active));
        if (tagsOf(o) !== 'PLN_006:L1 PLN_007:R2') throw new Error('Which tags: ' + tagsOf(o));
        if (pairsOf(o) !== 'PLN_005<PLN_006,PLN_010,PLN_013 PLN_008<PLN_007,PLN_012') throw new Error('Which pairs: ' + pairsOf(o));
    }
    expectIn('which svg', '<svg class="os-flow-picture" viewBox="0 0 1500 ');
});
groupedTest('grouped: the Spots svg is drawn at the picture\'s own size, then fitted to its box', async () => {
    await open(grouped());
    PICW = 1500;
    tap('part', { part: 'SYN-P6-X' });
    tap('two');
    tap('part', { part: 'SYN-P6-Y' });
    expectIn('spots svg', 'viewBox="0 0 1500 560" width="1500" height="560"');
});

// A box the stub can measure: a fresh one each time the screen is redrawn, as
// a browser's would be, and the Spots svg in it.
function measuredBox(w, h) {
    const boxes = [];
    const svg = { attrs: {}, setAttribute(k, v) { this.attrs[k] = v; } };
    root().querySelector = sel => {
        if (sel !== '.os-comp-scroll') return null;
        const last = boxes[boxes.length - 1];
        if (last && last.html === root().innerHTML) return last;
        boxes.push({ html: root().innerHTML, clientWidth: w, clientHeight: h, scrollLeft: 0, scrollTop: 0 });
        return boxes[boxes.length - 1];
    };
    Object.defineProperty(svg, 'parentNode', { get: () => root().querySelector('.os-comp-scroll') });
    els['os-comp-svg'] = svg;
    return { svg: svg, box: () => root().querySelector('.os-comp-scroll'),
        done() { delete els['os-comp-svg']; root().querySelector = () => null; } };
}
groupedTest('grouped: the Spots picture fits its box both ways, never scaled up', async () => {
    await open(grouped());
    tap('part', { part: 'SYN-P6-X' });
    tap('two');
    PICW = 1698;
    const m = measuredBox(1280, 596);
    try {
        tap('part', { part: 'SYN-P6-Y' });
        // 1698 x 560: the width binds, 1280 / 1698.
        if (m.svg.attrs.width !== '1280' || m.svg.attrs.height !== '422') throw new Error('fit: ' + JSON.stringify(m.svg.attrs));
        PICW = 900;
        tapPos('PLN_010');
        if (m.svg.attrs.width !== '900' || m.svg.attrs.height !== '560') throw new Error('scaled up: ' + JSON.stringify(m.svg.attrs));
    } finally { m.done(); PICW = 0; }
});
groupedTest('grouped: past the touch floor the picture scrolls, and a tap keeps the scroll', async () => {
    await open(grouped());
    tap('part', { part: 'SYN-P6-X' });
    tap('two');
    PICW = 4000;
    const m = measuredBox(1280, 596);
    try {
        tap('part', { part: 'SYN-P6-Y' });
        if (m.svg.attrs.width !== '2000') throw new Error('the floor is a half: ' + JSON.stringify(m.svg.attrs));
        m.box().scrollLeft = 640; m.box().scrollTop = 0;
        tapPos('PLN_010');
        const after = m.box();
        if (after.scrollLeft !== 640) throw new Error('the scroll jumped: ' + after.scrollLeft);
    } finally { m.done(); PICW = 0; }
});
// ── the Spots step as the floor drives it: requests, a part moved twice ─────
groupedTest('grouped: the Spots taps send nothing; Next is the one request', async () => {
    await twoOff(grouped());
    CALLS.length = 0;
    tapPos('PLN_010'); tapPos('PLN_013');
    tap('unmove', { node: 'PLN_006' });
    tapPos('PLN_005');
    tapPos('PLN_012'); tapPos('PLN_007');
    if (CALLS.length) throw new Error('a Spots tap sent ' + urls().join());
    tap('next');
    if (urls().join() !== '/api/processes/1/flow/preview?preflight=1') throw new Error('Next sent ' + urls().join());
});
groupedTest('grouped: a part moved twice keeps one chip, and its ✕ goes back to where it opened', async () => {
    await twoOff(grouped());
    tapPos('PLN_010'); tapPos('PLN_013');
    tapPos('PLN_013'); tapPos('PLN_006');
    if (JSON.stringify(chips()) !== '["PLN_010 → PLN_006 ✕"]') throw new Error('twice: ' + JSON.stringify(chips()));
    tapPos('PLN_006'); tapPos('PLN_013');
    tap('unmove', { node: 'PLN_006' });
    if (chips().length) throw new Error('undo: ' + JSON.stringify(chips()));
    CALLS.length = 0;
    tap('next');
    if (sortedKeys(lastPreview().overrides) !== '{"PLN_006":"PLN_010","PLN_007":"PLN_012"}') {
        throw new Error('body ' + JSON.stringify(lastPreview()));
    }
});
groupedTest('grouped: the hash entry reaches PLN_010 selected', async () => {
    const flow = grouped();
    await open(flow);
    win.location.hash = '#compose=positions;recipe=' + card(flow, 'Two spot press index').id +
        ';parts=SYN-P6-X,SYN-P6-Y;moves=PLN_006:PLN_010,PLN_007:PLN_012;sel=PLN_006';
    try {
        if (!UI.openFromHash()) throw new Error('the hash opened nothing');
    } finally {
        win.location.hash = '';
    }
    if (JSON.stringify(lit()) !== '["PLN_006","PLN_013"]') throw new Error('lit: ' + JSON.stringify(lit()));
    if (tagsOf() !== 'PLN_006:L1 PLN_010:L1 PLN_012:R2 PLN_013:L1') throw new Error('tags: ' + tagsOf());
    expectIn('hint', 'PLN_010 · Left Hand · where this part runs', hint());
});
groupedTest('grouped process: the ungrouped One spot still lends and names no group', async () => {
    await oneOff(grouped());
    tapPos('PLN_009');
    if (JSON.stringify(lit()) !== '["PLN_006"]') throw new Error('lit: ' + JSON.stringify(lit()));
    expectIn('hint', 'PLN_009 · where this part runs', hint());
    if (tagsOf()) throw new Error('tags: ' + tagsOf());
});

// ── Go says what happens and draws the press ─────────────────────────────────
test('Go says what comes off, what comes on, and Core\'s word, and draws the press', async () => {
    await open({ part_rows: ROWS, presets: [ONE] }, true);   // the card is setup on only
    PREVIEW = {
        order_count: 3,
        actions: [
            { core_node_name: 'PLN_06', evac_order: { from: 'PLN_06', to: 'ULN_002' } },
            { core_node_name: 'PLN_07', evac_order: { from: 'PLN_07', to: 'ULN_003' } },
            { core_node_name: 'PLN_09', supply_order: { from: 'SMN_BUF_100', to: 'PLN_09', payload_code: 'PART-B' } },
        ],
        preflight: { state: 'missing', missing: ['PART-B', 'PART-A'], absent: ['PART-B'] },
        mint: { style_id: 2, style_name: 'B-RUN', cells: [{ core_node_name: 'PLN_09', role: 'produce',
            swap_mode: 'two_robot_press_index', payload_code: 'PART-B' }] },
    };
    tap('saved', { style: '2' });
    await settle(); await settle();
    for (const want of ['Change over to B-RUN?',
        'Takes the bins off PLN_06 and PLN_07 to ULN_002 and ULN_003.',
        'Brings PART-B to PLN_09 from SMN_BUF_100.',
        'Core has no bin for PART-B: that delivery waits until it has one.',
        'Core has none free for PART-A right now: that delivery waits until one is free.',
        'class="picture"', 'os-comp-trips', 'Start changeover']) {
        expectIn('go', want);
    }
    expectOut('go', '<details');
    expectOut('go', 'trips and wait');
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
