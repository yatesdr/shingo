// set-pick.test.js — the spec of shared/set-pick.js, the set pick: several
// boxes over one node list. Run by shared/components_test.go
// (TestSharedComponentSpecs). The plant is the Edge's row shapes.

'use strict';

const { test, run, page, mk, eq, ok } = require('./spec.js');

const EDGE = [
    { name: 'ALN_01' }, { name: 'ALN_02' }, { name: 'ALN_03' },
    { name: 'Supermarket Area', node_type: 'NGRP', members: ['SMN_005', 'SMN_006'] },
    { name: 'SMN_005', group: 'Supermarket Area' }, { name: 'SMN_006', group: 'Supermarket Area' },
    { name: 'SLN_01' }, { name: 'SMN_023' },
];

const BOXES = [
    { key: 'positions', label: 'Positions', sub: 'the spots this process works', role: 'positions', need: 'needed' },
    { key: 'source', label: 'Inbound', sub: 'where the bin comes from', role: 'inbound', need: 'needed' },
    { key: 'staging', label: 'Staging', sub: 'where a bin waits', role: 'staging', need: 'optional' },
    { key: 'destination', label: 'Outbound', sub: 'where the bin goes', role: 'outbound', need: 'needed' },
];

async function setPick(opts) {
    const p = page('layer.js', 'picker.js', 'node-picker.js', 'set-pick.js');
    const [, , N, SP] = p.mods;
    const src = N.nodeSource(() => EDGE);
    await src.load();
    const host = mk(p.r, 'div', { id: 'host' });
    p.r.doc.body.appendChild(host);
    const moved = [];
    const s = SP.createSetPick(host, Object.assign({
        boxes: BOXES, source: src,
        field: (key) => ({ members: key === 'positions' || key === 'staging' ? 'fold' : 'none' }),
        onChange: (key) => moved.push(key),
    }, opts || {}));
    const box = (key) => s.el.querySelector('.setpick-box[data-box="' + key + '"]');
    const rows = () => s.el.querySelectorAll('.picker-row').map((n) => (n.classList.contains('picker-row--member') ? '  ' : '') +
        n.querySelector('.picker-label').textContent + n.querySelectorAll('.picker-tag').map((t) => ' [' + t.textContent + ']').join(''));
    const tick = (v) => s.el.querySelectorAll('.picker-row').find((n) => n.getAttribute('data-value') === v).click();
    return { p, s, box, rows, tick, moved };
}

test('nothing happens at import: no listener, no element', async () => {
    const p = page('set-pick.js');
    eq([p.doc.listenerTypes(), p.doc.body.children.length], [[], 0]);
});

test('every box says its name, its count and its picks; a box still needed is marked, an empty optional one says so', async () => {
    const f = await setPick({ selected: { positions: ['ALN_01', 'ALN_02'] } });
    const t = (key) => f.box(key).querySelector('.setpick-t').textContent;
    eq([t('positions'), t('source'), t('staging'), t('destination')],
        ['Positions2', 'Inboundneeded0', 'Stagingoptional0', 'Outboundneeded0']);
    eq(f.box('positions').querySelectorAll('.setpick-chip').map((c) => c.firstChild.textContent), ['ALN_01', 'ALN_02']);
    eq([f.box('source').classList.contains('is-needed'), f.box('staging').classList.contains('is-needed')], [true, false]);
    eq(f.box('positions').getAttribute('data-role'), 'positions');
});

test('a click on a box opens it: the list ticks that box\'s names, and a tick goes into it', async () => {
    const f = await setPick({ selected: { positions: ['ALN_01'] } });
    f.box('destination').click();
    eq([f.s.active(), f.s.el.querySelector('.setpick-head b').textContent], ['destination', 'Picking Outbound']);
    f.tick('SMN_023');
    eq([f.s.value('destination'), f.s.value('positions'), f.moved], [['SMN_023'], ['ALN_01'], ['destination']]);
    eq(f.box('destination').classList.contains('is-needed'), false);
});

test('a row in another box carries that box\'s name as a tag, in its tone', async () => {
    const f = await setPick({ selected: { positions: ['ALN_01'], source: ['Supermarket Area'] } });
    f.box('destination').click();
    ok(f.rows().indexOf('ALN_01 [Positions]') >= 0, 'ALN_01 tagged: ' + f.rows().join(' | '));
    ok(f.rows().indexOf('Supermarket Area [Inbound]') >= 0, 'the group tagged: ' + f.rows().join(' | '));
    const tag = f.s.el.querySelectorAll('.picker-tag').find((t) => t.textContent === 'Inbound');
    eq(tag.getAttribute('data-tone'), 'inbound');
});

test('Inbound names a group as one row; Staging folds its members, pickable once shown', async () => {
    const f = await setPick();
    f.box('source').click();
    eq(f.rows().filter((r) => r.indexOf('SMN_00') >= 0), []);
    f.box('staging').click();
    eq(f.rows().filter((r) => r.indexOf('SMN_00') >= 0), []);
    f.s.el.querySelector('[data-fold="Supermarket Area"]').click();
    f.tick('SMN_006');
    eq(f.s.value('staging'), ['SMN_006']);
});

test('a chip\'s × takes the name out of its box, whichever box is open', async () => {
    const f = await setPick({ selected: { positions: ['ALN_01', 'ALN_02'], source: ['SMN_023'] } });
    f.box('source').querySelector('[data-drop="SMN_023"]').click();
    eq([f.s.value('source'), f.s.active(), f.moved], [[], 'positions', ['source']]);
});

test('dirty is any box moved from where it started; markClean takes the picks as the start', async () => {
    const f = await setPick({ selected: { positions: ['ALN_01'] } });
    eq(f.s.dirty(), false);
    f.tick('ALN_02');
    eq([f.s.dirty(), f.s.el.hasAttribute('data-dirty')], [true, true]);
    f.s.markClean();
    eq([f.s.dirty(), f.s.el.hasAttribute('data-dirty')], [false, false]);
});

test('a closed box cannot be opened and says why', async () => {
    const f = await setPick({ closed: (key) => (key === 'source' ? 'the routing set could not be read' : '') });
    f.box('source').click();
    eq([f.s.active(), f.box('source').querySelector('.setpick-sub').textContent], ['positions', 'the routing set could not be read']);
});

test('a host may move names on a change: set() writes another box', async () => {
    const f = await setPick({
        selected: { source: ['ALN_03'] },
        onChange: (key, s) => { if (key === 'positions') s.set('source', s.value('source').filter((n) => s.value('positions').indexOf(n) < 0)); },
    });
    f.tick('ALN_03');
    eq([f.s.value('positions'), f.s.value('source')], [['ALN_03'], []]);
});

run();
