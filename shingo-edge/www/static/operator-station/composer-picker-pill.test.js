// composer-picker-pill.test.js — the picker's pill says what Core said.
//
// Each style row in the S2 picker ends in a pill drawn from the view's
// sourcing_by_style, Core's verdict keyed by style name. A red verdict means
// Core found no available bin for at least one of the style's parts, so the
// pill says that ("No bin at Core") rather than a vaguer "No parts", and the
// note Core already sent ("missing PANEL-A") rides along on the row. Every
// verdict the payload can carry is pinned here: green, yellow, red,
// not_configured, no verdict at all, and a style with no flow.
//
// The row's flow summary names the positions the way the rest of the screen
// does — full names, no abbreviation — so a scan for a position matches the
// name printed on the floor.
//
// WHAT IS REAL: composer-render.js and composer-model.js, and openPicker
// drawing the rows. WHAT IS STUBBED: the DOM, as in
// composer-strip-unplaced-chip.test.js; the rows are asserted as the string
// the module wrote.
//
// Run by www/composer_picker_pill_test.go.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let failures = 0;
let checks = 0;
function test(name, fn) {
    try {
        fn();
        checks++;
    } catch (e) {
        failures++;
        console.error('FAIL  ' + name + '\n      ' + (e && e.message));
    }
}

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

function makeEl(id) {
    return {
        id: id || '', hidden: true, innerHTML: '', dataset: {}, style: {},
        classList: { add() {}, remove() {}, contains: () => false, toggle() {} },
        addEventListener() {}, appendChild(c) { return c; }, focus() {},
        querySelector: () => null, querySelectorAll: () => [],
    };
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
Object.assign(global, {
    window: win, document: doc, esc,
    renderFlowPicture: () => '', pictureRows: () => ({}), sentencesFromModel: () => [],
    setTimeout: () => 0, clearTimeout: () => {}, requestAnimationFrame: fn => fn(),
    fetch: () => Promise.resolve({ ok: false, json: async () => null }),
});

const src = fs.readFileSync(path.join(__dirname, 'composer-render.js'), 'utf8')
    .replace(/^import[\s\S]*?from\s+'[^']+';\s*$/gm, '')
    .replace(/^export\s*\{[^}]*\};?\s*$/gm, '')
    .replace(/^export\s+function\s+/gm, 'function ');
vm.runInThisContext('(function(){\n' + src + '\n})();', { filename: 'composer-render.js' });

function style(id, name, nodes) {
    return { id: id, name: name, claim_count: nodes.length, claim_modes: ['swap'], claim_nodes: nodes };
}
const VIEW = {
    process: { id: 1, active_style_id: 0, flow_composer_enabled: true },
    station: { id: 5 },
    composer: {
        styles: [
            style(1, 'Green Part', ['PLN_01']),
            style(2, 'Yellow Part', ['PLN_01']),
            style(3, 'Red Part', ['PLN_01', 'PLN_04']),
            style(4, 'Red Bare', ['PLN_01']),
            style(5, 'Unchecked Part', ['PLN_01']),
            style(6, 'Silent Part', ['PLN_01']),
            { id: 7, name: 'Blank Part', claim_count: 0 },
        ],
    },
    // The four shapes styleSourcingViewFrom (handlers_changeover.go) writes.
    sourcing_by_style: {
        'Green Part': { status: 'green', code: 'green', blocked: false, note: '' },
        'Yellow Part': { status: 'yellow', code: 'yellow', blocked: false, note: 'low: PANEL-B' },
        'Red Part': { status: 'red', code: 'red', blocked: true, note: 'missing PANEL-A' },
        'Red Bare': { status: 'red', code: 'red', blocked: true, note: '' },
        'Unchecked Part': { status: 'not set up', code: 'not_configured', blocked: true,
            note: 'no sourceability claims' },
    },
};

win.ComposerUI.openPicker(VIEW);
const html = els['os-comp-pick'].innerHTML;

// One row's markup, found by the style name in its .id span.
function rowFor(name) {
    const rows = html.split('<button class="os-comp-row"').slice(1);
    const r = rows.find(x => x.indexOf('<span class="id">' + esc(name) + '</span>') >= 0);
    if (!r) throw new Error('no picker row for ' + name + '\n      picker: ' + html);
    return r;
}
function pill(name) {
    const m = /<span class="vd ([a-z]+)"([^>]*)>([^<]*)<\/span>/.exec(rowFor(name));
    if (!m) throw new Error('no pill on the row for ' + name);
    return { cls: m[1], attrs: m[2], text: m[3] };
}
function meta(name) {
    const m = /<span class="meta">([\s\S]*?)<\/span>/.exec(rowFor(name));
    return m ? m[1] : '';
}
function expectPill(name, cls, text) {
    const p = pill(name);
    if (p.cls !== cls || p.text !== text) {
        throw new Error(name + ': pill is ' + p.cls + ' "' + p.text + '", want ' + cls + ' "' + text + '"');
    }
    return p;
}
function expectNote(name, p, note) {
    if (meta(name).indexOf(' · ' + note) < 0) {
        throw new Error(name + ': the row does not carry the note "' + note + '": ' + meta(name));
    }
    if (p.attrs.indexOf('title="' + note + '"') < 0) {
        throw new Error(name + ': the pill has no title carrying the note: ' + p.attrs);
    }
}

test('green says parts are available', () => {
    expectPill('Green Part', 'ok', 'Parts available');
});
test('yellow says running low and names what is low', () => {
    expectNote('Yellow Part', expectPill('Yellow Part', 'unv', 'Running low'), 'low: PANEL-B');
});
test('red says Core has no bin and names what is missing', () => {
    expectNote('Red Part', expectPill('Red Part', 'no', 'No bin at Core'), 'missing PANEL-A');
});
test('red with no note still says Core has no bin, and adds nothing to the row', () => {
    const p = expectPill('Red Bare', 'no', 'No bin at Core');
    if (/ · $/.test(meta('Red Bare')) || p.attrs.indexOf('title') >= 0) {
        throw new Error('an empty note left a mark: ' + meta('Red Bare') + ' / ' + p.attrs);
    }
});
test('not_configured says Core does not check the style', () => {
    expectNote('Unchecked Part', expectPill('Unchecked Part', 'unv', 'Not checked at Core'),
        'no sourceability claims');
});
test('no verdict at all stays unverified', () => {
    expectPill('Silent Part', 'unv', 'Unverified');
});
test('a style with no flow says so', () => {
    expectPill('Blank Part', 'build', 'No flow yet');
});
test('the flow summary names positions in full', () => {
    const m = meta('Red Part');
    if (m.indexOf('PLN_01/PLN_04') < 0) throw new Error('positions are not named in full: ' + m);
});
test('no row says "No parts"', () => {
    if (html.indexOf('No parts') >= 0) throw new Error('"No parts" is still on the picker');
});

if (failures > 0) {
    console.error('FAILED: ' + failures + ' of ' + (failures + checks));
    process.exit(1);
}
console.log('OK: ' + checks + ' checks passed');
