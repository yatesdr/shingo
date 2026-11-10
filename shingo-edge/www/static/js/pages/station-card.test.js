// station-card.test.js — a station counter's card: its positions' boxes and
// the bindings its save sends. Run by www/station_card_test.go.
'use strict';

const path = require('path');
const assert = require('assert');
const C = require(path.join(__dirname, 'station-card.js'));

// A name given on the command line runs only the checks whose names hold it.
const only = process.argv[2] || '';
let failed = 0;
function check(name, fn) {
    if (only && name.indexOf(only) < 0) return;
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

check('a two-tag counter shows both tags joined with " + "', () => {
    assert.strictEqual(C.tagsOf({ tag_name: 'Sta020_Fix1_T1.CNT', tag_name_2: 'Sta020_Fix2_T1.CNT' }),
        'Sta020_Fix1_T1.CNT + Sta020_Fix2_T1.CNT');
    assert.strictEqual(C.tagsOf({ tag_name: 'Sta010.CNT', tag_name_2: '' }), 'Sta010.CNT');
});

const fmt = () => 'T';

check('the measures: the clicks per part, measured; none yet says so', () => {
    let m = C.measures({ ratio: 1.04, ratio_intervals: 20, strokes_since_click: 2, stopped: false });
    assert.strictEqual(m.ratio, '≈1.0 clicks per part over 20 strokes');
    assert.strictEqual(m.stopped, 'last click 2 production strokes ago');
    m = C.measures({ ratio: null, ratio_intervals: 0, strokes_since_click: 40, stopped: true });
    assert.strictEqual(m.ratio, 'clicks per part: not measured yet');
    assert.strictEqual(m.stopped, 'stopped: no click for 40 production strokes, more than the deepest run-out seen');
});

// The Counting block, rendered from a fixture: a cell counted on its MES
// struct's first slot, and two station counters (one read, one not; one
// stopped).
const block = {
    process: { id: 3, counter_plc_name: 'PLC_01', counter_tag_name: 'MES_WELD_6.Prod_Counter_01' },
    slots: [
            { slot: 1, tag: 'MES_WELD_6.Prod_Counter_01', catid: '1001', part: true, count: 5234, read: true,
                rising: true, rose_at: '2026-11-06T10:00:00Z', counted: true, label: 'discovered' },
            { slot: 2, tag: 'MES_WELD_6.Prod_Counter_02', catid: '-1', part: false, count: 0, read: true,
                rising: false, counted: false, label: 'discovered' },
            { slot: 3, tag: 'MES_WELD_6.Prod_Counter_03', catid: '', part: false, count: 0, read: false,
                rising: false, counted: false, label: 'discovered' },
    ],
    counters: [
        { id: 7, label: 'load station', tag_name: 'STATION_01', last_poll_at: '2026-11-06T10:00:00Z', ratio: 1.02,
            ratio_intervals: 20, strokes_since_click: 2, stopped: false },
        { id: 8, label: '', tag_name: 'STATION_02', ratio: null, ratio_intervals: 0, strokes_since_click: 40, stopped: true },
    ],
    positions: [
        { id: 1, name: 'POS-1', line_counter_id: 7 },
        { id: 2, name: 'POS-2', line_counter_id: null },
    ],
    verdicts: { 1: { countable: true, refusal: '' }, 2: { countable: true, refusal: '' } },
    fmt: fmt,
};

// groupsOf splits the block's HTML into its groups, in order, each with its
// labelled lines: { group, lines: [{ src, text }] }.
function groupsOf(html) {
    return html.split('data-group="').slice(1).map(part => {
        const lines = [];
        const re = /data-src="([^"]+)"><span class="pd-tag">([^<]*)<\/span> <span>([^<]*)<\/span>/g;
        let m;
        while ((m = re.exec(part)) !== null) {
            assert.strictEqual(m[1], m[2], 'a line shows the label it carries');
            lines.push({ src: m[1], text: m[3] });
        }
        return { group: part.slice(0, part.indexOf('"')), lines: lines };
    });
}

// labelOf is the label of the one line whose text starts with prefix.
function labelOf(lines, prefix) {
    const hit = lines.filter(l => l.text.indexOf(prefix) === 0);
    assert.strictEqual(hit.length, 1, 'one line starting "' + prefix + '" in ' + JSON.stringify(lines));
    return hit[0].src;
}

check('the counting block: two groups, the MES slots discovered and the slot counted on confirmed', () => {
    const g = groupsOf(C.countingHTML(block));
    assert.deepStrictEqual(g.map(x => x.group), ['slots', 'stations']);
    const s = g[0].lines;
    assert.strictEqual(labelOf(s, 'counts on Prod_Counter_01'), 'confirmed');
    assert.strictEqual(labelOf(s, 'Prod_Counter_01 · CATID 1001 · count 5234 · rising'), 'discovered');
    assert.strictEqual(labelOf(s, 'Prod_Counter_02 · CATID -1, no part · count 0 · not rising'), 'discovered');
    assert.strictEqual(labelOf(s, 'Prod_Counter_03 · not read at the last pass'), 'discovered');
    assert.strictEqual(s.length, 4);
});

check('the counting block: a slot the PLC counts on, with no reading from before it rose, says the record lacks its strokes', () => {
    const b = JSON.parse(JSON.stringify(block));
    b.fmt = fmt;
    b.slots[1] = { slot: 2, tag: 'MES_WELD_6.Prod_Counter_02', catid: '1001', part: true, count: 14, read: true,
        rising: true, rose_at: '2026-11-06T10:01:00Z', counted: false, label: 'discovered',
        not_recorded_since: '2026-11-06T09:50:00Z' };
    const s = groupsOf(C.countingHTML(b))[0].lines;
    assert.strictEqual(labelOf(s, 'Prod_Counter_02 · CATID 1001 · count 14 · rising · strokes since T are not in the record'),
        'discovered');
});

check('the counting block: a station tag discovered until read, then confirmed; positions confirmed; the rest measured', () => {
    const s = groupsOf(C.countingHTML(block))[1].lines;
    assert.strictEqual(labelOf(s, 'tag STATION_01: saved and read'), 'confirmed');
    assert.strictEqual(labelOf(s, 'tag STATION_02: saved, not read yet'), 'discovered');
    assert.strictEqual(labelOf(s, 'counts POS-1'), 'confirmed');
    assert.strictEqual(labelOf(s, 'counts no position yet'), 'confirmed');
    assert.strictEqual(labelOf(s, '≈1.0 clicks per part over 20 strokes'), 'measured');
    assert.strictEqual(labelOf(s, 'last click 2 production strokes ago'), 'measured');
    assert.strictEqual(labelOf(s, 'stopped: no click for 40 production strokes'), 'measured');
    assert.strictEqual(s.length, 8, 'two cards of four lines');
});

if (failed) { console.log('FAILED: ' + failed); process.exit(1); }
console.log('station card: all passed');
