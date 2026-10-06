// The station's state words, as the operator reads them in the header, on the
// footer badge and on an empty grid. The footer badge, the header line and the
// header's style chip must all say the same thing from the same fields: a
// changeover is the process's target style differing from its active style, the
// running part is current_style. The words are "Running <style>", "Changing
// over" and "No part running". An empty grid says why it is empty in plain
// words: no positions at all, or positions with no part running on them.
//
// operator-render.js isn't DOM-free, so (like operator-render-drain.test.js) we
// strip its ES imports, stub the few imported helpers and the module-level
// element lookups with tiny fake elements, and drive renderHeader, renderFooter
// and renderGrid against a view. Runs under plain Node via the Go wrapper
// operator_render_state_words_test.go. Exit 0 = pass.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
function eq(got, want, label) {
    if (got === want) { passed++; return; }
    failed++;
    console.error('FAIL: ' + label + '\n   got:  ' + JSON.stringify(got) + '\n   want: ' + JSON.stringify(want));
}

function fakeEl(props) {
    const e = {
        textContent: '', innerHTML: '', className: '', children: [],
        style: { removeProperty() {}, setProperty() {} },
        appendChild(c) { this.children.push(c); return c; },
        addEventListener() {}, setAttribute() {},
        querySelector() { return null; },
    };
    e.classList = {
        add(c) { e.className += ' ' + c; }, remove() {}, contains() { return false; },
    };
    Object.assign(e, props || {});
    return e;
}

const els = {};
function elById(id) { if (!els[id]) els[id] = fakeEl({ id: id }); return els[id]; }

let view = null;
const src = fs.readFileSync(path.join(__dirname, 'operator-render.js'), 'utf8')
    .replace(/^import\s.*$/gm, '')
    .replace(/export\s+function\s+/g, 'function ')
    .replace(/export\s+const\s+/g, 'const ')
    .replace(/export\s*\{[^}]*\}\s*;?/g, '');
const ctx = vm.createContext({
    document: {
        getElementById: elById,
        querySelector: () => null,
        body: fakeEl(),
    },
    el: function (tag, props) { return fakeEl(props); },
    getView: function () { return view; },
    claimedNodes: function () {
        return (view && view.nodes || []).filter(n => n.active_claim || n.changeover_task);
    },
    mountFlowPanel() {}, syncFlowPanel() {}, openFlowPanel() {},
    isActive: function () { return false; },
});
vm.runInContext(src + '\nthis.renderHeader = renderHeader; this.renderFooter = renderFooter; this.renderGrid = renderGrid;', ctx);

function findChip(root) {
    for (const c of root.children) {
        if (String(c.className).indexOf('os-header-style ') === 0) return c;
    }
    return null;
}
function chipValue() {
    const chip = findChip(elById('os-header-actions'));
    if (!chip) return null;
    const v = chip.children.find(c => c.className === 'os-header-style-value');
    return v ? v.textContent : null;
}
function render(v) {
    view = v;
    elById('os-header-actions').children = [];
    ctx.renderHeader();
    ctx.renderFooter();
    return {
        header: elById('os-header-info').textContent,
        chip: chipValue(),
        badge: elById('os-footer-badge').textContent,
    };
}

const base = { station: { health_status: 'online' }, nodes: [] };

// ── Running: a current style and no target change ──
let r = render(Object.assign({}, base, {
    process: { name: 'Press 4', active_style_id: 7, target_style_id: 7 },
    current_style: { name: 'PIA15' },
}));
eq(r.badge, 'Running PIA15', 'running: footer badge');
eq(r.header, 'Press 4 - Running PIA15', 'running: header line says the same words as the badge');
eq(r.chip, 'PIA15', 'running: style chip names the style');

// ── Changing over: target differs from active ──
r = render(Object.assign({}, base, {
    process: { name: 'Press 4', active_style_id: 7, target_style_id: 9 },
    current_style: { name: 'PIA15' },
    target_style: { name: 'PIA16' },
    active_changeover: { from_style_name: 'PIA15', to_style_name: 'PIA16' },
}));
eq(r.badge, 'Changing over', 'changing: footer badge');
eq(r.header, 'Press 4 - Changing over PIA15 → PIA16', 'changing: header line leads with the badge words');
eq(r.chip, 'PIA15 → PIA16', 'changing: style chip shows from and to');

// ── No part running: no current style ──
r = render(Object.assign({}, base, {
    process: { name: 'Press 4' },
    current_style: null,
}));
eq(r.badge, 'No part running', 'idle: footer badge');
eq(r.header, 'Press 4 - No part running', 'idle: header line says the same words as the badge');
eq(r.chip, 'No part running', 'idle: style chip says no part running, not a missing style');

// ── Empty grid ──
function gridEmpty(v) {
    view = v;
    const grid = elById('os-grid');
    grid.children = [];
    ctx.renderGrid();
    const e = grid.children.find(c => c.id === 'os-grid-empty');
    return e ? e.textContent : null;
}
eq(gridEmpty(Object.assign({}, base, { process: { name: 'Press 4' }, nodes: [] })),
    'No positions on this screen', 'empty grid with no positions');
eq(gridEmpty(Object.assign({}, base, {
    process: { name: 'Press 4' }, current_style: null,
    nodes: [{ name: 'PLN_01', active_claim: null }],
})), 'No part running — start a changeover to pick one', 'empty grid with positions and no part running');

if (failed) { console.error(passed + ' passed, ' + failed + ' FAILED'); process.exit(1); }
console.log('operator-render state words: ' + passed + ' passed');
