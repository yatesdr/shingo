// station-card.test.js — the Production counter section's live words, its MES
// slots row, and the station counters' list: each counter's row. Which
// positions a counter counts is picked under Position settings › Counted at
// (the exception list), not on the card. Run by www/station_card_test.go.
'use strict';

const path = require('path');
const assert = require('assert');

// The page code under test. This file lives in www/testdata, outside the
// embedded static tree, so no binary carries or serves it
// (TestStaticTree_HoldsNoTestJS); the code it reads stays in www/static.
const SRC_DIR = path.join(__dirname, '..', '..', '..', '..', 'static', 'js', 'pages');

const C = require(path.join(SRC_DIR, 'station-card.js'));

// A name given on the command line runs only the checks whose names hold it;
// a name that matches none fails, so a Go wrapper cannot pass on nothing.
const only = process.argv[2] || '';
let failed = 0;
let ran = 0;
function check(name, fn) {
    if (only && name.indexOf(only) < 0) return;
    ran++;
    try { fn(); console.log('ok   ' + name); } catch (e) { failed++; console.log('FAIL ' + name + '\n     ' + e.message); }
}


check('a two-tag counter shows both tags joined with " + "', () => {
    assert.strictEqual(C.tagsOf({ tag_name: 'Sta020_Fix1_T1.CNT', tag_name_2: 'Sta020_Fix2_T1.CNT' }),
        'Sta020_Fix1_T1.CNT + Sta020_Fix2_T1.CNT');
    assert.strictEqual(C.tagsOf({ tag_name: 'Sta010.CNT', tag_name_2: '' }), 'Sta010.CNT');
});

check('the status: one line, singular and plural, amber only when stopped', () => {
    const read = '2026-11-06T10:00:00Z';
    let s = C.status({ last_poll_at: read, ratio: 1.04, ratio_intervals: 20, strokes_since_click: 1, stopped: false });
    assert.deepStrictEqual(s, { text: '≈1.0 per part · last click 1 stroke ago', warn: false });
    s = C.status({ last_poll_at: read, ratio: 1.04, strokes_since_click: 2, stopped: false });
    assert.strictEqual(s.text, '≈1.0 per part · last click 2 strokes ago');
    s = C.status({ last_poll_at: read, ratio: 1, strokes_since_click: 0, stopped: false });
    assert.strictEqual(s.text, '≈1.0 per part · clicked since the last stroke');
    s = C.status({ last_poll_at: read, ratio: null, strokes_since_click: 40, stopped: true });
    assert.deepStrictEqual(s, { text: 'clicks per part not measured yet · no click for 40 strokes, more than the deepest run-out seen',
        warn: true });
    s = C.status({ ratio: null, strokes_since_click: 3, stopped: false });
    assert.strictEqual(s.text, 'not read yet · clicks per part not measured yet · last click 3 strokes ago');
    s = C.status({ last_poll_at: read, tag_name_2: 'Sta020_Fix2_T1.Prod_Tool_1_CNT.ACC', ratio: 1, strokes_since_click: 1 });
    assert.strictEqual(s.text, 'second tag not read yet · ≈1.0 per part · last click 1 stroke ago');
    assert.strictEqual(s.warn, false, 'a tag not read yet is not amber');
});

// The fixture: a cell counted on its MES struct's first slot (CATID 1315,
// which RH expects), a second slot that rises with it, a third rising alone,
// and two unused; two station counters (one stopped, one not read).
const fmt = () => 'T';
const clock = () => '10:05';
const block = {
    process: { id: 3, counter_plc_name: 'PLC_01', counter_tag_name: 'MES_WELD_6.Prod_Counter_01', counter_enabled: true,
        active_style_id: 21 },
    styles: [{ id: 20, name: 'LH', expected_catid: '1314' }, { id: 21, name: 'RH', expected_catid: '1315, 1316' }],
    slots: [
        { slot: 1, tag: 'MES_WELD_6.Prod_Counter_01', catid: '1315', part: true, count: 5234, read: true,
            rising: true, rose_at: '2026-11-06T10:05:00Z', counted: true, label: 'discovered' },
        { slot: 2, tag: 'MES_WELD_6.Prod_Counter_02', catid: '1315', part: true, count: 5230, read: true,
            rising: true, counted: false, label: 'discovered' },
        { slot: 3, tag: 'MES_WELD_6.Prod_Counter_03', catid: '1316', part: true, count: 14, read: true,
            rising: true, counted: false, label: 'discovered' },
        { slot: 4, tag: 'MES_WELD_6.Prod_Counter_04', catid: '-1', part: false, count: 0, read: true,
            rising: false, counted: false, label: 'discovered' },
        { slot: 5, tag: 'MES_WELD_6.Prod_Counter_05', catid: '0', part: false, count: 0, read: true,
            rising: false, counted: false, label: 'discovered' },
    ],
    items: [{ kind: 'counter_slot_lockstep', severity: 'info', headline: 'Prod_Counter_02 (CATID 1315) rises with Prod_Counter_01',
        data: { tag: 'MES_WELD_6.Prod_Counter_02' } }],
    counters: [
        { id: 7, label: 'op 10', tag_name: 'Sta010.CNT', last_poll_at: '2026-11-06T10:00:00Z', ratio: 1.02,
            ratio_intervals: 20, strokes_since_click: 1, stopped: false },
        { id: 8, label: 'op 20', tag_name: 'Sta020_Fix1.CNT', tag_name_2: 'Sta020_Fix2.CNT', ratio: null,
            ratio_intervals: 0, strokes_since_click: 40, stopped: true },
    ],
    positions: [
        { id: 1, name: 'POS-1', line_counter_id: 7 },
        { id: 2, name: 'POS-2', line_counter_id: 7 },
        { id: 3, name: 'POS-3', line_counter_id: null },
    ],
    fmt: fmt,
    clock: clock,
};
const copy = over => Object.assign(JSON.parse(JSON.stringify(block)), { fmt, clock }, over || {});

// rowOf(html, id): one settings row of the rendered rows, whole.
function rowOf(html, id) {
    const at = html.indexOf('data-group="' + id + '"');
    assert.ok(at >= 0, 'no ' + id + ' row');
    const next = html.indexOf('<div class="pd-sfld', at + 1);
    return html.slice(html.lastIndexOf('<div', at), next < 0 ? html.length : next);
}

// text drops the markup, so a check reads what the page says.
const text = html => html.replace(/<[^>]+>/g, '\u0001').split('\u0001').map(s => s.trim()).filter(Boolean)
    .join(' | ').replace(/&amp;/g, '&');

check('the title: what it counts on, the CATID and the style that expects it, the last stroke', () => {
    assert.strictEqual(C.titleWords(block), 'counting on Prod_Counter_01 · CATID 1315 (RH) · last stroke 10:05');
    assert.ok(C.titleHTML(block).indexOf('pd-cnt ok') > 0, 'the green dot while it counts');
    const b = copy();
    b.slots[0].rising = false;
    b.process.active_style_id = 0;
    b.styles = [];
    assert.strictEqual(C.titleWords(b), 'counting on Prod_Counter_01 · CATID 1315 · last stroke T',
        'a stroke longer ago in full; no style expects the CATID');
    assert.strictEqual(C.titleWords(copy({ slots: [] })), 'counting on Prod_Counter_01', 'no slots read');
    assert.strictEqual(C.titleWords(copy({ process: Object.assign({}, block.process, { counter_enabled: false }) })),
        'off · this process isn’t counted');
    const off = C.titleHTML(copy({ process: { id: 3 } }));
    assert.ok(off.indexOf('not wired') > 0 && off.indexOf('pd-cnt ok') < 0, off);
});

check('the counting rows: MES slots counted first, in-use slots their own lines, unused ones folded', () => {
    const html = C.countingHTML(block);
    const slots = text(rowOf(html, 'slots'));
    assert.strictEqual(slots, 'MES slots | ' +
        'Prod_Counter_01 · counted · CATID 1315 · count 5234 · rising | ' +
        'Prod_Counter_02 · CATID 1315 · count 5230 · rising · rises with Prod_Counter_01: a second counter of the same stroke, counted once | ' +
        'Prod_Counter_03 · CATID 1316 · count 14 · rising | Count Prod_Counter_03 | ' +
        'Prod_Counter_04–05 unused');
    assert.strictEqual(html.split('data-act="ch-repoint"').length, 2, 'one re-point: the rising slot that is not a twin');
    assert.ok(html.indexOf('data-act="ch-repoint" data-pid="3" data-tag="MES_WELD_6.Prod_Counter_03"') > 0, html);
    assert.ok(html.indexOf('pd-tag') < 0, 'no provenance chips');
});

check('the counting rows: unread slots fold, a slot leading with no record says so', () => {
    const b = copy();
    b.slots[1] = Object.assign(b.slots[1], { read: false });
    b.slots[2] = Object.assign(b.slots[2], { rising: true, not_recorded_since: '2026-11-06T09:50:00Z' });
    b.slots[4] = Object.assign(b.slots[4], { read: false });
    const t = text(C.countingHTML(b));
    assert.ok(t.indexOf('Prod_Counter_03 · CATID 1316 · count 14 · rising · strokes since T are not in the record') > 0, t);
    assert.ok(t.indexOf('Prod_Counter_04 unused') > 0, t);
    assert.ok(t.indexOf('Prod_Counter_02, Prod_Counter_05 not read at the last pass') > 0, t);
});

check('the station list: one row per counter, its tags, its positions as words, its status; then + Add', () => {
    const html = C.countingHTML(block);
    const st = rowOf(html, 'stations');
    assert.strictEqual(text(st), 'Station counters | where a part leaves the cart, in place of the production counter | ' +
        'op 10 | Sta010.CNT | Edit | Remove | counts POS-1, POS-2 | ≈1.0 per part · last click 1 stroke ago | ' +
        'op 20 | Sta020_Fix1.CNT + Sta020_Fix2.CNT | Edit | Remove | counts no position yet | ' +
        'not read yet · clicks per part not measured yet · no click for 40 strokes, more than the deepest run-out seen | ' +
        '+ Add station counter');
    assert.strictEqual(st.split('pd-warn').length, 2, 'amber on the stopped counter only');
    assert.ok(st.indexOf('data-checklist') < 0 && st.indexOf('sc-checklist') < 0, 'no checklist: Counted at picks the positions');
    assert.ok(st.lastIndexOf('data-act="sc-new"') > st.lastIndexOf('data-sc-row'), '+ Add after the last row');
});

check('the counting rows: hidden until the counter is wired', () => {
    assert.strictEqual(C.countingHTML(copy({ process: { id: 3 }, counters: [] })), '');
});

if (only && ran === 0) { console.log('FAIL no check matches "' + only + '"'); process.exit(1); }
if (failed) { console.log('FAILED: ' + failed); process.exit(1); }
console.log('station card: all passed');
