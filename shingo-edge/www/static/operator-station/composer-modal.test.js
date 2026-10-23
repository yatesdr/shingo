// composer-modal.test.js — the station's changeover modal's decisions.
//
// WHAT IS PINNED: which screen each tap leads to — HMI setup off lists the
// styles and nothing else; on, the styles lead the part list, a
// part asks how the press runs it only when the recipes offer more than one
// way, parts that already run in a style ask Use or Set up new, setting
// up always passes the spots screen, the second part's list leads with the
// matching half — and the bodies Go previews, and what Go says.
//
// WHAT IS REAL: composer-render.js and composer-model.js. WHAT IS STUBBED: the
// DOM (screens are asserted as the markup the module wrote), the picture
// renderer, and fetch (the composer read answers from FLOW; a preview answers
// PREVIEW when a test sets it and nothing otherwise, so Go stays on
// "Checking…").
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
let PIC = {};          // the options the last picture was drawn with
const CALLS = [];
Object.assign(global, {
    window: win, document: doc, esc,
    renderFlowPicture: (v, o) => { PIC = o || {}; return '<g class="picture"></g>'; },
    setTimeout: () => 0, clearTimeout: () => {}, requestAnimationFrame: fn => fn(),
    setInterval: () => 0, clearInterval: () => {},
    AbortController: class { constructor() { this.signal = {}; } abort() {} },
    fetch: (url, init) => {
        CALLS.push({ url: String(url), body: init && init.body ? JSON.parse(init.body) : null });
        if (String(url).indexOf('/composer') >= 0) return Promise.resolve({ ok: true, status: 200, json: async () => FLOW });
        if (PREVIEW && String(url).indexOf('/flow/preview') >= 0) {
            return Promise.resolve({ ok: true, status: 200, json: async () => PREVIEW });
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
async function open(flow, setup) {
    UI.close();
    FLOW = flow;
    PREVIEW = null;
    UI.openPicker(viewWith(setup !== false));
    await settle(); await settle();
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
test('setup off: a style tapped previews that style, nothing minted', async () => {
    await open({ part_rows: ROWS, presets: [ONE] }, false);
    CALLS.length = 0;
    tap('saved', { style: '2' });
    expectIn('saved go', 'Checking what this changeover does');
    if (JSON.stringify(lastPreview()) !== '{"style_id":2,"station_id":5}') throw new Error('body ' + JSON.stringify(lastPreview()));
});

// ── HMI setup on ─────────────────────────────────────────────────────────────
test('setup on: the styles lead, then the parts with Core\'s word and a CATID', async () => {
    await open({ part_rows: ROWS, presets: [ONE] });
    const b = box();
    if (!(b.indexOf('>Styles<') < b.indexOf('Set up a part'))) throw new Error('order: ' + b.slice(0, 400));
    for (const want of ['data-part="PART-C"', 'CATID CAT-A', 'in A-RUN', 'No bin at Core', 'Stock not checked',
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
spotsTest('spots open where the cell last ran the recipe, as the server shipped it', async () => {
    await twoOff(press6());
    if (JSON.stringify(chips().sort()) !== '["PLN_006 → PLN_010 ✕","PLN_007 → PLN_012 ✕"]') {
        throw new Error('pre-fill: ' + JSON.stringify(chips()));
    }
});
spotsTest('a press spot lights only the spots its recipes pair with its deck, and outlines itself', async () => {
    await twoOff(press6());
    tapPos('PLN_010');
    if (PIC.selected !== 'PLN_010' || PIC.selectCard !== true) throw new Error('selection: ' + JSON.stringify(PIC));
    if (JSON.stringify(lit()) !== '["PLN_006","PLN_009"]') throw new Error('lit: ' + JSON.stringify(lit()));
    expectIn('hint', 'PLN_010 · where this part runs', hint());
});
spotsTest('the on-deck card selects its own job, and PLN_013 moves nothing', async () => {
    await twoOff(press6());
    tapPos('PLN_005');
    expectIn('hint', 'PLN_005 · on deck for PLN_010', hint());
    if (PIC.selected !== 'PLN_005' || lit().length) throw new Error('deck: ' + JSON.stringify(PIC));
    tapPos('PLN_013');
    if (chips().length !== 2 || chips().some(c => c.indexOf('PLN_013') >= 0)) throw new Error('moved: ' + JSON.stringify(chips()));
    if (PIC.selected !== 'PLN_005') throw new Error('the selection went: ' + PIC.selected);
});
spotsTest('a press then its own deck re-selects the deck; nothing swaps', async () => {
    await twoOff(press6());
    tapPos('PLN_010');
    tapPos('PLN_005');
    if (PIC.selected !== 'PLN_005' || chips().length !== 2) throw new Error('swapped: ' + JSON.stringify(chips()));
});
spotsTest('a spot then another spot re-selects it; the decks never cross (was: a swap)', async () => {
    await twoOff(press6());
    tapPos('PLN_010');
    tapPos('PLN_012');
    if (PIC.selected !== 'PLN_012') throw new Error('selected ' + PIC.selected);
    if (JSON.stringify(chips().sort()) !== '["PLN_006 → PLN_010 ✕","PLN_007 → PLN_012 ✕"]') {
        throw new Error('swapped: ' + JSON.stringify(chips()));
    }
});
spotsTest('a press spot moves to a lit spot and back, and Go sends what the chips say', async () => {
    await twoOff(press6());
    tapPos('PLN_010'); tapPos('PLN_009');
    if (JSON.stringify(chips().sort()) !== '["PLN_006 → PLN_009 ✕","PLN_007 → PLN_012 ✕"]') {
        throw new Error('move: ' + JSON.stringify(chips()));
    }
    tapPos('PLN_009'); tapPos('PLN_006');
    if (JSON.stringify(chips()) !== '["PLN_007 → PLN_012 ✕"]') throw new Error('put back: ' + JSON.stringify(chips()));
    CALLS.length = 0;
    tap('next');
    if (JSON.stringify(lastPreview().overrides) !== '{"PLN_007":"PLN_012"}') throw new Error('body ' + JSON.stringify(lastPreview()));
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
// ── Go says what happens and draws the press ─────────────────────────────────
test('Go says what comes off, what comes on, and Core\'s word, and draws the press', async () => {
    await open({ part_rows: ROWS, presets: [ONE] }, false);
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
