// picker.test.js — the spec of shared/picker.js, the picker's shell. Run by
// shared/components_test.go (TestSharedComponentSpecs). The rows are
// synthetic: the shell knows no node or part, and the adapters that do are S3.

'use strict';

const { test, run, page, mk, ev, key, press, eq } = require('./spec.js');

const ROWS = [
    { value: 'PLN_001', label: 'PLN_001', section: 'line' },
    { value: 'PLN_002', label: 'PLN_002', section: 'line', reason: 'used by position 3' },
    { value: 'PLN_003', label: 'PLN_003', section: 'line', note: 'staging' },
    {
        value: 'GRP_A', label: 'GRP_A', section: 'group', values: ['SMN_001', 'SMN_002'],
        members: [{ value: 'SMN_001', label: 'SMN_001' }, { value: 'SMN_002', label: 'SMN_002', search: 'supermarket' }],
    },
];

function picker(opts, rows) {
    const p = page('layer.js', 'picker.js');
    const [L, P] = p.mods;
    const host = mk(p.r, 'div', { id: 'host' });
    p.r.doc.body.appendChild(host);
    const changes = [];
    const k = P.createPicker(host, Object.assign({ rows: rows === undefined ? ROWS : rows, onChange: (v) => changes.push(v) }, opts || {}));
    const shown = () => k.el.querySelectorAll('.picker-sect, .picker-row, .picker-add, .picker-none').map((n) =>
        (n.classList.contains('picker-sect') ? '# ' : n.classList.contains('picker-row--member') ? '  ' : '')
        + n.textContent + (n.classList.contains('is-on') ? ' [on]' : '') + (n.classList.contains('is-off') ? ' [off]' : '')
        + (n.classList.contains('is-active') ? ' <' : ''));
    const row = (v) => k.el.querySelectorAll('.picker-row').find((n) => n.getAttribute('data-value') === v);
    const typeQ = (q) => { k.input.value = q; k.input.dispatchEvent(ev('input')); };
    return { p, r: p.r, L, P, host, k, changes, shown, row, typeQ };
}

test('nothing happens at import: no listener, no element', async () => {
    const p = page('picker.js');
    eq([p.doc.listenerTypes(), p.doc.body.children.length], [[], 0]);
});

test('rows are drawn under their section headings, members indented under their group', async () => {
    const f = picker({ open: true });
    eq(f.shown(), ['# line', 'PLN_001', 'PLN_002used by position 3 [off]', 'PLN_003staging', '# group', 'GRP_A', '  SMN_001', '  SMN_002']);
});

test('an exclusion is shown with its reason and cannot be picked by click or key', async () => {
    const f = picker({ open: true, multi: true });
    f.row('PLN_002').click();
    eq([f.k.value(), f.row('PLN_002').getAttribute('aria-disabled'), f.row('PLN_002').querySelector('.picker-why').textContent],
        [[], 'true', 'used by position 3']);
    key(f.k.input, 'ArrowDown');
    key(f.k.input, 'ArrowDown');
    eq(f.k.el.querySelector('.is-active').getAttribute('data-value'), 'PLN_003', 'the keyboard steps over it');
});

test('the search filters by label, value and search text; a member that matches brings its group', async () => {
    const f = picker({ open: true });
    f.typeQ('pln_00');
    eq(f.shown(), ['# line', 'PLN_001 <', 'PLN_002used by position 3 [off]', 'PLN_003staging']);
    f.typeQ('supermarket');
    eq(f.shown(), ['# group', 'GRP_A <', '  SMN_002']);
    f.typeQ('grp');
    eq(f.shown(), ['# group', 'GRP_A <', '  SMN_001', '  SMN_002']);
    f.typeQ('zzz');
    eq(f.shown(), ['Nothing matches']);
});

test('a keystroke never redraws the search box', async () => {
    const f = picker({ open: true });
    const box = f.k.input;
    f.typeQ('PLN');
    f.typeQ('PLN_00');
    eq([f.k.el.querySelector('.picker-q') === box, box.value], [true, 'PLN_00']);
});

test('the keyboard: arrows move the active row, Enter picks it, the input names it for a reader', async () => {
    const f = picker({ open: true });
    key(f.k.input, 'ArrowDown');
    eq(f.k.input.getAttribute('aria-activedescendant'), f.row('PLN_001').id, 'first');
    key(f.k.input, 'ArrowDown');
    key(f.k.input, 'ArrowUp');
    key(f.k.input, 'ArrowUp');
    const enter = ev('keydown', { key: 'Enter' });
    f.k.input.dispatchEvent(enter);
    eq([f.k.value(), f.changes, enter.defaultPrevented], ['PLN_001', ['PLN_001'], true]);
});

test('a single picker: a pick replaces the value and closes its list', async () => {
    const f = picker({ selected: 'PLN_001' });
    f.k.input.dispatchEvent(ev('focus'));
    eq([f.L.layerDepth(), f.k.el.querySelector('.picker-list').hidden], [1, false], 'open on focus');
    f.row('PLN_003').click();
    eq([f.k.value(), f.L.layerDepth(), f.k.el.querySelector('.picker-list').hidden, f.changes], ['PLN_003', 0, true, ['PLN_003']]);
});

test('a single pick ends the search: the box is empty under the chip, a multi-select keeps its query', async () => {
    const f = picker({});
    f.k.input.dispatchEvent(ev('focus'));
    f.typeQ('pln_00');
    f.row('PLN_003').click();
    eq([f.k.value(), f.k.input.value], ['PLN_003', ''], 'single');
    const m = picker({ multi: true, open: true });
    m.typeQ('pln_00');
    m.row('PLN_003').click();
    eq([m.k.value(), m.k.input.value], [['PLN_003'], 'pln_00'], 'multi');
});

test('a multi-select: a pick toggles, the chips are the selection and their × takes one out', async () => {
    const f = picker({ multi: true, open: true });
    f.row('PLN_001').click();
    f.row('PLN_003').click();
    f.row('PLN_001').click();
    f.row('PLN_001').click();
    eq(f.k.value(), ['PLN_003', 'PLN_001']);
    eq(f.k.el.querySelectorAll('.picker-chip').map((c) => c.firstChild.textContent), ['PLN_003', 'PLN_001']);
    f.k.el.querySelector('[data-drop="PLN_003"]').click();
    eq([f.k.value(), f.changes.length], [['PLN_001'], 5]);
});

test('a row that stands for others (a group) picks them all, and is on when all are', async () => {
    const f = picker({ multi: true, open: true });
    f.row('GRP_A').click();
    eq([f.k.value(), f.row('GRP_A').classList.contains('is-on'), f.row('SMN_001').classList.contains('is-on')], [['SMN_001', 'SMN_002'], true, true]);
    f.row('SMN_002').click();
    eq(f.row('GRP_A').classList.contains('is-on'), false, 'one member out');
    f.row('GRP_A').click();
    f.row('GRP_A').click();
    eq(f.k.value(), [], 'all on, then off');
});

test('R4: in multi mode a held node is drawn greyed with its reason, even when it is in the selection', async () => {
    const f = picker({ multi: true, open: true, selected: ['PLN_002'] });
    f.row('PLN_002').click();
    eq([f.k.value(), f.shown()[2]], [['PLN_002'], 'PLN_002used by position 3 [on] [off]']);
});

// ── Add and use ─────────────────────────────────────────────────────────────

test('"Add and use" shows when the search names no row, runs once per press, and picks what it made', async () => {
    let runs = 0;
    let finish;
    const f = picker({ open: true, add: { run: (q) => { runs++; return new Promise((res) => { finish = () => res({ value: q, label: q + ' (new)' }); }); } } });
    f.typeQ('PLN_001');
    eq(f.k.el.querySelector('.picker-add'), null, 'a row has that name');
    f.typeQ('PLN_009');
    eq(f.shown(), ['Add “PLN_009” and use it <'], 'offered');
    f.k.el.querySelector('.picker-add').click();
    key(f.k.input, 'Enter');
    eq([runs, f.shown()], [1, ['Adding…']], 'busy, one run');
    finish();
    await f.r.advance(0);
    eq([f.k.value(), f.k.input.value, f.changes], ['PLN_009', '', ['PLN_009']]);
    eq(f.shown().indexOf('PLN_009 (new) [on]') >= 0, true, 'drawn before the adapter has it');
});

test('"Add and use" that made nothing changes nothing and can be pressed again', async () => {
    let runs = 0;
    const f = picker({ open: true, add: { label: (q) => 'New node ' + q, run: async () => { runs++; return null; } } });
    f.typeQ('X');
    f.k.el.querySelector('.picker-add').click();
    await f.r.advance(0);
    f.k.el.querySelector('.picker-add').click();
    await f.r.advance(0);
    eq([runs, f.k.value(), f.shown()], [2, '', ['New node X']]);
});

// ── the list as a layer popover ─────────────────────────────────────────────

test('a dropdown list closes on Escape before the sheet under it', async () => {
    const p = page('layer.js', 'picker.js');
    const [L, P] = p.mods;
    const host = mk(p.r, 'div', {});
    const s = L.openSheet({ title: 'Add process', body: host });
    const k = P.createPicker(host, { rows: ROWS });
    k.input.dispatchEvent(ev('focus'));
    key(k.input, 'Escape');
    eq([k.el.querySelector('.picker-list').hidden, s.open], [true, true], 'first Escape: the list');
    key(k.input, 'Escape');
    eq(s.open, false, 'second: the sheet');
});

test('a press outside a dropdown closes it; one on the picker does not', async () => {
    const f = picker({});
    const elsewhere = mk(f.r, 'button', {}, ['x']);
    f.r.doc.body.appendChild(elsewhere);
    f.k.open();
    press(f.k.input);
    eq(f.L.layerDepth(), 1, 'a press on its own box');
    press(elsewhere);
    eq(f.L.layerDepth(), 0, 'a press outside');
});

test('one list open at a time: opening a second picker\'s list closes the first', async () => {
    const f = picker({});
    const host2 = mk(f.r, 'div', {});
    f.r.doc.body.appendChild(host2);
    const k2 = f.P.createPicker(host2, { rows: ROWS });
    f.k.open();
    k2.open();
    eq([f.k.el.querySelector('.picker-list').hidden, k2.el.querySelector('.picker-list').hidden, f.L.layerDepth()], [true, false, 1]);
});

test('an always-open list (a checklist) is not a layer and stays up', async () => {
    const f = picker({ open: true, multi: true });
    f.k.input.dispatchEvent(ev('focus'));
    f.row('PLN_001').click();
    key(f.k.input, 'Escape');
    eq([f.L.layerDepth(), f.k.el.querySelector('.picker-list').hidden], [0, false]);
});

// ── work, filters, loading ──────────────────────────────────────────────────

test('a moved selection is work the sheet asks before discarding; the search box alone is not', async () => {
    const p = page('layer.js', 'picker.js');
    const [L, P] = p.mods;
    const host = mk(p.r, 'div', {});
    L.openSheet({ title: 'Positions', body: host });
    const k = P.createPicker(host, { rows: ROWS, multi: true, open: true });
    k.input.value = 'PLN';
    k.input.dispatchEvent(ev('input'));
    eq(k.el.hasAttribute('data-dirty'), false, 'typing in the search box');
    k.el.querySelectorAll('.picker-row')[0].click();
    eq(k.el.hasAttribute('data-dirty'), true, 'a pick');
    key(p.r.doc.body, 'Escape');
    eq(p.r.doc.querySelectorAll('.layer-scrim').length, 2, 'the sheet asked');
    k.markClean();
    eq(k.el.hasAttribute('data-dirty'), false, 'markClean');
});

test('filters put their query in the search box; a second press clears it', async () => {
    const f = picker({ open: true, filters: [{ label: 'PLN', query: 'PLN_' }, { label: 'SMN', query: 'SMN_' }] });
    f.k.el.querySelectorAll('.picker-filters button')[1].click();
    eq([f.k.input.value, f.shown(), f.k.el.querySelectorAll('.picker-filters .is-on').length], ['SMN_', ['# group', 'GRP_A', '  SMN_001', '  SMN_002'], 1]);
    f.k.el.querySelectorAll('.picker-filters button')[1].click();
    eq(f.k.input.value, '');
});

test('rows not read yet say so; refresh draws them when they land', async () => {
    let rows = null;
    const f = picker({ open: true, loading: 'Reading the node list…' }, () => rows);
    eq(f.shown(), ['Reading the node list…']);
    rows = ROWS.slice(0, 1);
    f.k.refresh();
    eq(f.shown(), ['# line', 'PLN_001']);
});

test('set() moves the selection without calling onChange; {clean} makes it the starting point', async () => {
    const f = picker({ multi: true, open: true });
    f.k.set(['PLN_001']);
    eq([f.k.value(), f.changes, f.k.el.hasAttribute('data-dirty')], [['PLN_001'], [], true]);
    f.k.set(['PLN_003'], { clean: true });
    eq(f.k.el.hasAttribute('data-dirty'), false);
});

// ── write-through (the wave C ruling) ───────────────────────────────────────

// A write the test answers by hand: onPick's promise, and what it was asked.
function writeThrough(extra) {
    const asked = [];
    let answer = null;
    const f = picker(Object.assign({
        multi: true, open: true, selected: ['PLN_001'],
        onPick: (next, row) => { asked.push({ next, row: row.value }); return new Promise((res) => { answer = res; }); },
    }, extra || {}));
    const chips = () => f.k.el.querySelectorAll('.picker-chip').map((c) => c.firstChild.textContent);
    return Object.assign(f, { asked, chips, answer: (v) => answer(v) });
}

test('write-through: the picked row shows pending and the chips do not move until the write answers', async () => {
    const f = writeThrough();
    f.row('PLN_003').click();
    eq([f.asked, f.chips(), f.row('PLN_003').classList.contains('is-pending'), f.row('PLN_003').getAttribute('aria-busy')],
        [[{ next: ['PLN_001', 'PLN_003'], row: 'PLN_003' }], ['PLN_001'], true, 'true']);
    f.row('PLN_001').click();
    eq(f.asked.length, 1, 'a second pick while one is out is not taken');
    f.answer(['PLN_001', 'PLN_003', 'SMN_001']);
    await f.r.advance(0);
    eq([f.chips(), f.k.value(), f.row('PLN_003').classList.contains('is-pending'), f.k.el.hasAttribute('data-dirty'), f.changes],
        [['PLN_001', 'PLN_003', 'SMN_001'], ['PLN_001', 'PLN_003', 'SMN_001'], false, false, [['PLN_001', 'PLN_003', 'SMN_001']]],
        'the server\'s selection, drawn clean');
});

test('write-through: a refusal leaves the chips as they were and shows the reason; a chip\'s × writes through too', async () => {
    const f = writeThrough();
    f.row('PLN_003').click();
    f.answer({ error: 'PLN_003 is used by PART SYN-A' });
    await f.r.advance(0);
    const why = f.k.el.querySelector('.picker-refused');
    eq([f.chips(), f.k.value(), why.hidden, why.textContent, f.changes], [['PLN_001'], ['PLN_001'], false, 'PLN_003 is used by PART SYN-A', []]);
    f.k.el.querySelector('[data-drop="PLN_001"]').click();
    eq([f.asked[1], f.chips(), why.hidden], [{ next: [], row: 'PLN_001' }, ['PLN_001'], true], 'the × asks; the old reason goes');
    f.answer({ values: [] });
    await f.r.advance(0);
    eq(f.chips(), []);
});

test('write-through: an answer that is not a promise is taken at once; a throw is a refusal', async () => {
    const f = picker({ multi: true, open: true, onPick: (next) => next.filter((v) => v !== 'SMN_002') });
    f.row('GRP_A').click();
    eq(f.k.value(), ['SMN_001'], 'what onPick returned, not what was picked');
    const g = picker({ open: true, onPick: () => { throw new Error('no answer'); } });
    g.row('PLN_001').click();
    eq([g.k.value(), g.k.el.querySelector('.picker-refused').textContent], ['', 'no answer']);
});

run();
