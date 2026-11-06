// station-card.test.js — a station counter's card: its positions' boxes and
// the bindings its save sends. Run by www/station_card_test.go.
'use strict';

const path = require('path');
const assert = require('assert');
const C = require(path.join(__dirname, 'station-card.js'));

let failed = 0;
function check(name, fn) {
    try { fn(); console.log('ok   ' + name); } catch (e) { failed++; console.log('FAIL ' + name + '\n     ' + e.message); }
}

const positions = [
    { id: 1, name: 'POS-1', line_counter_id: null },
    { id: 2, name: 'POS-2', line_counter_id: null },
    { id: 3, name: 'POS-3', line_counter_id: 7 },
    { id: 4, name: 'POS-4', line_counter_id: 8 },
];
const verdicts = {
    1: { countable: true, refusal: '' },
    2: { countable: false, refusal: 'POS-2 is a loader under RUN; a loader is counted by its loads' },
    3: { countable: true, refusal: '' },
    4: { countable: true, refusal: '' },
};
const byID = rows => Object.fromEntries(rows.map(r => [r.id, r]));

check('the only station counter pre-ticks its countable unbound positions, and no refused one', () => {
    const r = byID(C.rows(7, 1, positions.slice(0, 3), verdicts));
    assert.strictEqual(r[1].checked, true, 'POS-1: countable, unbound');
    assert.strictEqual(r[2].checked, false, 'POS-2: refused');
    assert.strictEqual(r[2].disabled, true);
    assert.strictEqual(r[2].refusal, verdicts[2].refusal, 'the refusal sentence greys the box');
    assert.strictEqual(r[3].checked, true, 'POS-3: bound here');
});

check('with a second station counter nothing unbound is pre-ticked', () => {
    const r = byID(C.rows(7, 2, positions, verdicts));
    assert.strictEqual(r[1].checked, false, 'POS-1: unbound, but this is not the only station');
    assert.strictEqual(r[3].checked, true, 'POS-3: bound here');
    assert.strictEqual(r[4].checked, false, 'POS-4: bound to the other counter');
    assert.strictEqual(r[4].at, 8);
});

check('a position not yet checked is greyed, never pre-ticked', () => {
    const r = byID(C.rows(7, 1, [{ id: 9, name: 'POS-9', line_counter_id: null }], {}));
    assert.strictEqual(r[9].checked, false);
    assert.strictEqual(r[9].disabled, true);
});

check('the save sends each changed box, and only those', () => {
    const ticked = { 1: true, 2: true, 3: false, 4: true };
    const out = C.changes(7, positions, verdicts, ticked);
    assert.deepStrictEqual(out, [
        { id: 1, line_counter_id: 7 },
        { id: 3, line_counter_id: null },
        { id: 4, line_counter_id: 7 },
    ]);
    assert.deepStrictEqual(C.changes(7, positions, verdicts, { 3: true }), [], 'an unchanged box sends nothing');
});

if (failed) { console.log('FAILED: ' + failed); process.exit(1); }
console.log('station card: all passed');
