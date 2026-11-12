// node-picker.test.js — the spec of shared/node-picker.js and
// shared/part-picker.js, the node and part adapters over the picker's shell.
// Run by shared/components_test.go (TestSharedComponentSpecs).
//
// The plant is the Core picker pins' (shingo-core/www/core_pickers_pins_test.go
// corePickerFixture), as /api/nodes answers it, plus the Edge's row shapes.
// "cannot happen:" cases name a P0 bug (census U1 §4, REPORT-v2-stop1-P0) at
// the line it lives at today, and hold that the adapter cannot do it.

'use strict';

const { test, run, page, mk, ev, eq } = require('./spec.js');

// Core's /api/nodes, in map order (unsorted).
const CORE = [
    { id: 9, name: 'VIRT-1', is_synthetic: true, zone: 'B', enabled: true },
    { id: 1, name: 'GRP-A', node_type_code: 'NGRP', is_synthetic: true, zone: '', enabled: true },
    { id: 2, name: 'GRP-A-L1', node_type_code: 'LANE', is_synthetic: true, zone: '', enabled: true, parent_id: 1 },
    { id: 3, name: 'GRP-A-L1-S1', zone: 'A', enabled: true, parent_id: 2 },
    { id: 4, name: 'GRP-A-L1-S2', zone: 'A', enabled: true, parent_id: 2 },
    { id: 5, name: 'GRP-OFF', node_type_code: 'NGRP', is_synthetic: true, zone: 'B', enabled: false },
    { id: 6, name: 'GRP-OFF-N1', zone: 'B', enabled: true, parent_id: 5 },
    { id: 7, name: 'LINE-1', display_name: 'Line one', zone: 'B', enabled: true },
    { id: 8, name: '_TRANSIT', is_synthetic: true, zone: '', enabled: true },
    { id: 10, name: '_ROBOT:AMR-01', zone: '', enabled: true },
    { id: 11, name: 'OLD-SLOT', zone: 'B', enabled: false },
    { id: 12, name: 'SPOT-9', zone: '', enabled: true },
    { id: 13, name: 'Spot-10', zone: '', enabled: true },
];

// The Edge's rows: a member names its group; a group may list its members.
const EDGE = [
    { name: 'PRESS-1' },
    { name: 'GRP-A', node_type: 'NGRP', members: ['SLOT-A2'] },
    { name: 'SLOT-A1', group: 'GRP-A' },
    { name: 'SLOT-A2' },
    { name: 'LANE-1', node_type: 'LANE' },
    { name: '_TRANSIT' },
    { name: '_ROBOT:AMR-01' },
];

function adapters() {
    const p = page('layer.js', 'picker.js', 'node-picker.js', 'part-picker.js');
    const [L, , N, P] = p.mods;
    return { p, r: p.r, L, N, P };
}

// lines is rows as the shell draws them: "# section", "value | label · note
// (off: reason)", members indented.
function lines(rows) {
    const out = [];
    let section = null;
    const one = (r, pad) => {
        out.push(pad + r.value + (r.label !== r.value ? ' | ' + r.label : '') + (r.note ? ' · ' + r.note : '') +
            (r.reason ? ' (off: ' + r.reason + ')' : '') + (r.values ? ' => ' + r.values.join(',') : ''));
    };
    for (const r of rows) {
        if (r.section !== undefined && r.section !== section) { section = r.section; out.push('# ' + section); }
        one(r, '');
        for (const m of r.members || []) one(m, '  ');
    }
    return out;
}

function source(N, list) {
    const s = N.nodeSource(() => list);
    return s;
}

async function loaded(N, list) {
    const s = source(N, list);
    await s.load();
    return s;
}

test('nothing happens at import: no listener, no element', async () => {
    const p = page('node-picker.js', 'part-picker.js');
    eq([p.doc.listenerTypes(), p.doc.body.children.length], [[], 0]);
});

test('the tree: containers first in their zone, every node under one indented at any depth', async () => {
    const { N } = adapters();
    const s = await loaded(N, CORE);
    eq(lines(N.nodeRows(s.rows(), {})), [
        '# A',
        'GRP-A · group',
        '  GRP-A-L1 · lane',
        '  GRP-A-L1-S1 | ↳ GRP-A-L1-S1',
        '  GRP-A-L1-S2 | ↳ GRP-A-L1-S2',
        '# B',
        'GRP-OFF · group',
        '  GRP-OFF-N1',
        'LINE-1 · Line one',
        'OLD-SLOT',
        'VIRT-1 · synthetic',
        '# Other',
        'SPOT-9',
        'Spot-10',
        '_ROBOT:AMR-01 · robot',
    ]);
});

test('cannot happen: _TRANSIT is offered (census #5, #11; handlers_test_orders.go:44, orders.js:840)', async () => {
    const { N } = adapters();
    for (const list of [CORE, EDGE]) {
        const s = await loaded(N, list);
        const values = [];
        for (const r of N.nodeRows(s.rows(), { rule: () => '' })) { values.push(r.value); (r.members || []).forEach((m) => values.push(m.value)); }
        eq(values.indexOf('_TRANSIT'), -1);
    }
});

test('cannot happen: a container whose children are lanes files under Other, and a lane\'s slots are not offered (census #6, orders.js:843, :864)', async () => {
    const { N } = adapters();
    const s = await loaded(N, CORE);
    const rows = N.nodeRows(s.rows(), {});
    const a = rows.find((r) => r.value === 'GRP-A');
    eq([a.section, a.members.map((m) => m.value)], ['A', ['GRP-A-L1', 'GRP-A-L1-S1', 'GRP-A-L1-S2']]);
});

test('cannot happen: a disabled group\'s live member is not offered (census #6, orders.js:859)', async () => {
    const { N } = adapters();
    const s = await loaded(N, CORE);
    const rule = N.nodeWhy.disabled;
    const off = N.nodeRows(s.rows(), { rule }).find((r) => r.value === 'GRP-OFF');
    eq([off.reason, off.members[0].value, off.members[0].reason], ['disabled', 'GRP-OFF-N1', undefined]);
});

test('names sort in code-unit order, the order Go\'s sort.Strings gives (the twins, handlers_test_orders.go:95 vs orders.js:862)', async () => {
    const { N } = adapters();
    const s = await loaded(N, CORE);
    const other = N.nodeRows(s.rows(), {}).filter((r) => r.section === 'Other').map((r) => r.value);
    eq(other, ['SPOT-9', 'Spot-10', '_ROBOT:AMR-01']);
});

test('no blanket rule: each field passes its own, and an exclusion is drawn with its reason', async () => {
    const { N } = adapters();
    const s = await loaded(N, CORE);
    const W = N.nodeWhy;
    const strict = lines(N.nodeRows(s.rows(), { rule: W.any(W.robot, W.disabled, W.group, W.synthetic), sections: false }));
    const open = lines(N.nodeRows(s.rows(), { sections: false }));
    eq(strict.filter((l) => l.indexOf('(off:') >= 0), [
        'GRP-A · group (off: a group: pick one of its nodes)',
        '  GRP-A-L1 · lane (off: a group: pick one of its nodes)',
        'GRP-OFF · group (off: disabled)',
        'OLD-SLOT (off: disabled)',
        'VIRT-1 · synthetic (off: synthetic, not a place)',
        '_ROBOT:AMR-01 · robot (off: a robot, not a place)',
    ]);
    eq(open.filter((l) => l.indexOf('(off:') >= 0), []);
});

test('a field may take ids, and a container may stand for the nodes under it', async () => {
    const { N } = adapters();
    const s = await loaded(N, CORE);
    const rows = N.nodeRows(s.rows(), { value: 'id', expand: true, sections: false });
    const a = rows.find((r) => r.label === 'GRP-A');
    eq([a.value, a.values, a.members.map((m) => m.value)], ['1', ['3', '4'], ['2', '3', '4']]);
});

test('the Edge\'s rows: a member under its group, by its group or by the group\'s members; a member rule greys it', async () => {
    const { N } = adapters();
    const s = await loaded(N, EDGE);
    eq(lines(N.nodeRows(s.rows(), { rule: N.nodeWhy.member })), [
        'GRP-A · group',
        '  SLOT-A1 (off: in GRP-A: Core picks inside the group)',
        '  SLOT-A2 (off: in GRP-A: Core picks inside the group)',
        'LANE-1 · lane',
        'PRESS-1',
        '_ROBOT:AMR-01 · robot',
    ]);
});

test('a field\'s universe (only): a node outside it is not drawn, and the ones inside under it move up', async () => {
    const { N } = adapters();
    const s = await loaded(N, CORE);
    const groups = (n) => n.type === 'NGRP';
    eq(lines(N.nodeRows(s.rows(), { only: groups, sections: false })), ['GRP-A · group', 'GRP-OFF · group']);
    const slots = (n) => !n.synthetic && !n.robot;
    eq(lines(N.nodeRows(s.rows(), { only: slots, sections: false })), [
        'GRP-A-L1-S1', 'GRP-A-L1-S2', 'GRP-OFF-N1', 'LINE-1 · Line one', 'OLD-SLOT', 'SPOT-9', 'Spot-10',
    ]);
    const notLanes = (n) => n.type !== 'LANE';
    eq(lines(N.nodeRows(s.rows(), { only: notLanes, sections: false })).slice(0, 3),
        ['GRP-A · group', '  GRP-A-L1-S1', '  GRP-A-L1-S2'], 'a slot under a lane left out reads one level down');
});

// ── the source: one read per page ───────────────────────────────────────────

function mounted(N, r, src, field, opts) {
    const host = mk(r, 'div', { id: 'host-' + Math.random() });
    r.doc.body.appendChild(host);
    const changes = [];
    const k = N.nodePicker(host, src, field || {}, Object.assign({ open: true, onChange: (v) => changes.push(v) }, opts || {}));
    const shown = () => k.el.querySelectorAll('.picker-sect, .picker-row, .picker-none').map((n) =>
        (n.classList.contains('picker-sect') ? '# ' : n.classList.contains('picker-row--member') ? '  ' : '') + n.textContent);
    const row = (v) => k.el.querySelectorAll('.picker-row').find((n) => n.getAttribute('data-value') === v);
    return { host, k, changes, shown, row };
}

test('two pickers on one source make one read, and both draw when it lands', async () => {
    const { N, r } = adapters();
    let reads = 0;
    let land;
    const s = N.nodeSource(() => { reads++; return new Promise((res) => { land = res; }); });
    const a = mounted(N, r, s);
    const b = mounted(N, r, s);
    eq([a.shown(), reads], [['Reading the node list…'], 0], 'not read until asked');
    s.load();
    s.load();
    a.k.input.dispatchEvent(ev('focus'));
    b.k.input.dispatchEvent(ev('focus'));
    eq(reads, 1);
    land([{ name: 'PRESS-1' }, { name: 'DOCK-1' }]);
    await r.advance(0);
    eq([a.shown(), b.shown(), reads], [['DOCK-1', 'PRESS-1'], ['DOCK-1', 'PRESS-1'], 1]);
    await s.load();
    eq(reads, 1, 'a list already read is not read again');
});

test('cannot happen: a failed first read leaves the pickers on "Loading..." with no retry (census #7, orders.js:805)', async () => {
    const { N, r } = adapters();
    let reads = 0;
    const s = N.nodeSource(() => { reads++; return reads === 1 ? Promise.reject(new Error('Server error (502)')) : [{ name: 'PRESS-1' }]; });
    const a = mounted(N, r, s);
    a.k.input.dispatchEvent(ev('focus'));
    await r.advance(0);
    eq([a.shown(), s.rows(), reads], [['Could not read the node list: Server error (502). Open the list again to retry.'], null, 1]);
    a.k.input.dispatchEvent(ev('focus'));
    await r.advance(0);
    eq([a.shown(), reads], [['PRESS-1'], 2]);
});

test('the door\'s answer reads as a list, and a refused one as a failure', async () => {
    const { N } = adapters();
    const ok = N.nodeSource(() => ({ ok: true, status: 200, body: [{ name: 'PRESS-1' }], error: '' }));
    await ok.load();
    const no = N.nodeSource(() => ({ ok: false, status: 0, body: null, error: 'No answer' }));
    await no.load();
    eq([ok.rows().map((n) => n.name), no.rows(), no.error()], [['PRESS-1'], null, 'No answer']);
});

test('cannot happen: a list pushed to the page loses its group and lane tags (census #8, manual-order.js:178, production.js:546)', async () => {
    const { N, r } = adapters();
    const s = N.nodeSource(() => [], EDGE);
    const a = mounted(N, r, s, {}, { selected: 'PRESS-1' });
    s.set([{ name: 'PRESS-1' }, { name: 'GRP-B', node_type: 'NGRP' }, { name: 'LANE-2', node_type: 'LANE' }]);
    eq([a.shown(), a.k.value()], [['GRP-Bgroup', 'LANE-2lane', 'PRESS-1'], 'PRESS-1']);
});

test('a form field: a hidden input of the field\'s name carries the pick, and set() moves it too', async () => {
    const { N, r } = adapters();
    const s = await loaded(N, CORE);
    const a = mounted(N, r, s, { value: 'id', sections: false }, { name: 'node_id' });
    const hidden = a.host.querySelectorAll('input').pop();
    eq([hidden.getAttribute('type'), hidden.getAttribute('name'), hidden.value], ['hidden', 'node_id', '']);
    a.row('7').click();
    eq([hidden.value, a.changes], ['7', ['7']]);
    a.k.set('');
    eq(hidden.value, '');
});

// ── parts ───────────────────────────────────────────────────────────────────

const PARTS = [
    { id: 2, code: 'PART-B', description: 'Bracket' },
    { id: 1, code: 'PART-A', description: 'Arm' },
    { id: 3, code: 'PART-C', name: 'Cover' },
];

test('parts: the catalog in code order, the description as the note, searchable', async () => {
    const { P } = adapters();
    const s = P.partSource(() => PARTS);
    await s.load();
    eq(lines(P.partChoices(s.rows(), { none: '— none —' })), [' | — none —', 'PART-A · Arm', 'PART-B · Bracket', 'PART-C · Cover']);
    eq(P.partChoices(s.rows(), {}).find((x) => x.value === 'PART-C').search, 'Cover');
});

test('parts: a field\'s allowed codes in its order, a code the catalog lacks still offered, a rule greys one', async () => {
    const { P } = adapters();
    const s = P.partSource(() => PARTS);
    await s.load();
    const rule = (p) => (p.code === 'PART-A' ? 'not on this bin type' : '');
    eq(lines(P.partChoices(s.rows(), { allowed: ['PART-C', 'NEW-9', 'PART-A', 'PART-C'], rule })),
        ['PART-C · Cover', 'NEW-9', 'PART-A · Arm (off: not on this bin type)']);
});

test('parts: a picker over no list offers its allowed codes alone; one over a list reads it on first open', async () => {
    const { P, r } = adapters();
    const host = mk(r, 'div', { id: 'ph' });
    r.doc.body.appendChild(host);
    const k = P.partPicker(host, null, { allowed: ['X-1', 'X-2'] }, { open: true });
    eq(k.el.querySelectorAll('.picker-row').map((n) => n.getAttribute('data-value')), ['X-1', 'X-2']);
    let reads = 0;
    const s = P.partSource(() => { reads++; return PARTS; });
    const host2 = mk(r, 'div', { id: 'ph2' });
    r.doc.body.appendChild(host2);
    const k2 = P.partPicker(host2, s, {}, {});
    k2.input.dispatchEvent(ev('focus'));
    await r.advance(0);
    eq([reads, k2.el.querySelectorAll('.picker-row').map((n) => n.getAttribute('data-value'))], [1, ['PART-A', 'PART-B', 'PART-C']]);
});

run();
