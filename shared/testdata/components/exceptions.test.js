// exceptions.test.js — the spec of shared/exceptions.js, the exception list.
// Run by shared/components_test.go (TestSharedComponentSpecs).
//
// The piece returns markup, and the realm's DOM keeps innerHTML as one raw
// string (world.js), so the cases read the markup itself.

'use strict';

const { test, run, page, eq, ok } = require('./spec.js');

const all = (html, re) => Array.from(html.matchAll(re), (m) => m[1]);

test('the rule alone: no rows, the summary carries the add', () => {
    const { mods: [X] } = page('exceptions.js');
    const h = X.exceptionList({ id: 'counted', label: 'Counted at', summary: 'all 23 at the production counter',
        add: { label: '+ Add a position', act: 'exc-add', exc: 'counted' } });
    ok(h.startsWith('<div class="exc" data-exc="counted">'), 'the list carries its name: ' + h.slice(0, 60));
    eq(all(h, /<div class="exc-name">([^<]*)/g), ['Counted at']);
    eq(all(h, /<div class="exc-sum"><span>([^<]*)<\/span>/g), ['all 23 at the production counter']);
    ok(h.indexOf('<table') < 0, 'no table without rows');
    ok(h.indexOf('</span><button class="pd-btn quiet exc-add" data-act="exc-add" data-exc="counted">+ Add a position</button></div>') > 0,
        'the add sits beside the summary: ' + h);
});

test('one row per place that differs, its controls the caller\'s, then the add', () => {
    const { mods: [X] } = page('exceptions.js');
    const h = X.exceptionList({ id: 'curtain', label: 'Light curtain', sub: 'a full-bin release waits for the curtain',
        summary: '2 of 23 positions',
        rows: [
            { name: 'BPN_006PAL', cells: ['<input class="pd-inp" value="PLC_BIG">'], remove: { act: 'cu-off', node: 518 } },
            { name: 'BPN_009PAL', cells: ['<input class="pd-inp" value="PLC_BIG">'], remove: { act: 'cu-off', node: 521 } },
        ],
        add: { label: '+ Add a position', act: 'cu-add' } });
    eq(all(h, /<small>([^<]*)<\/small>/g), ['a full-bin release waits for the curtain']);
    eq(all(h, /<td class="exc-pos">([^<]*)<\/td>/g), ['BPN_006PAL', 'BPN_009PAL']);
    eq(all(h, /<td>(<input class="pd-inp" value="PLC_BIG">)<\/td>/g).length, 2, 'the caller\'s controls are drawn as given');
    eq(all(h, /data-act="cu-off" data-node="(\d+)"/g), ['518', '521']);
    eq(all(h, /aria-label="([^"]*)"/g), ['Remove BPN_006PAL from Light curtain', 'Remove BPN_009PAL from Light curtain'],
        'each × says what it removes');
    eq(all(h, /title="([^"]*)"/g), ['Remove BPN_006PAL from Light curtain', 'Remove BPN_009PAL from Light curtain'],
        'and says it without a hover to a screen reader and with one to a mouse');
    ok(h.indexOf('<div class="exc-sum"><span>2 of 23 positions</span></div>') > 0, 'with rows, the summary has no add');
    ok(h.indexOf('</table><button class="pd-btn quiet exc-add" data-act="cu-add">+ Add a position</button>') > 0,
        'the add follows the rows: ' + h);
});

test('cannot happen: a place name or a value breaks the markup', () => {
    const { mods: [X] } = page('exceptions.js');
    const h = X.exceptionList({ label: 'Light "curtain"', summary: '<b>none</b>',
        rows: [{ name: 'A<B"C', cells: [], remove: { act: 'x', node: '"1"' } }] });
    eq(all(h, /<td class="exc-pos">([^<]*)<\/td>/g), ['A&lt;B&quot;C']);
    eq(all(h, /<div class="exc-sum"><span>([^<]*)<\/span>/g), ['&lt;b&gt;none&lt;/b&gt;']);
    eq(all(h, /data-node="([^"]*)"/g), ['&quot;1&quot;']);
    ok(h.indexOf('<b>') < 0);
    eq(all(h, /<div class="exc-name">([^<]*)/g), ['Light &quot;curtain&quot;']);
});

test('a row with no remove has no ×; a list with no add has none', () => {
    const { mods: [X] } = page('exceptions.js');
    const h = X.exceptionList({ label: 'Next carrier', summary: '1 of 1 position', rows: [{ name: 'P1', cells: ['Robot 2'] }] });
    ok(h.indexOf('exc-x') < 0);
    ok(h.indexOf('exc-add') < 0);
});

test('countOf: the summary\'s words', () => {
    const { mods: [X] } = page('exceptions.js');
    eq(X.countOf(2, 23, 'position'), '2 of 23 positions');
    eq(X.countOf(23, 23, 'position'), 'all 23 positions');
    eq(X.countOf(1, 1, 'position'), 'all 1 position');
    eq(X.countOf(0, 23, 'position'), '0 of 23 positions');
    eq(X.countOf(0, 0, 'position'), 'no positions');
});

run();
