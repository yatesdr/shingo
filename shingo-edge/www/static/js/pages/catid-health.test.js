// catid-health.test.js — the health strip's list renderer.
// Run by www/catid_health_list_test.go.
'use strict';

const path = require('path');
const assert = require('assert');
const H = require(path.join(__dirname, 'catid-health.js'));

let failed = 0;
function check(name, fn) {
    try {
        fn();
        console.log('ok   ' + name);
    } catch (e) {
        failed++;
        console.log('FAIL ' + name + '\n     ' + e.message);
    }
}

check('no rows is no strip', () => {
    assert.strictEqual(H.listHTML([]), '');
    assert.strictEqual(H.listHTML(null), '');
});

check('a process with its items lists each with its verbs', () => {
    const html = H.listHTML([{ process_id: 3, name: 'CELL-A', worst: 'red', count: 2, headline: 'x' }], {
        3: [
            { kind: 'held', severity: 'red', headline: '5 held on CATID 1007', detail: 'the whole draw',
                data: { hold_id: 11, catid: '1007' } },
            { kind: 'unmapped_catid', severity: 'amber', headline: 'CATID 1003 maps to no style',
                data: { catid: '1003', suggestion: { style_id: 4, style_name: 'STYLE-B' },
                    styles: [{ id: 4, name: 'STYLE-B' }, { id: 5, name: '<C>' }] } },
        ],
    });
    assert.ok(html.indexOf('data-act="ch-charge" data-hold="11"') > 0, 'Charge on the hold');
    assert.ok(html.indexOf('data-act="ch-writeoff" data-hold="11"') > 0, 'Write off on the hold');
    assert.ok(html.indexOf('data-act="ch-pin" data-pid="3" data-style="4" data-name="STYLE-B" data-catid="1003"') > 0, 'the suggestion');
    assert.ok(html.indexOf('Pin to STYLE-B') > 0, 'the suggestion names its style');
    assert.ok(html.indexOf('data-act="ch-pin-pick"') > 0 && html.indexOf('&lt;C&gt;') > 0, 'the picker, escaped');
    assert.ok(html.indexOf('data-open="3"') > 0, 'the name opens the process');
});

check('a slot the PLC counts on carries the click that counts it', () => {
    const html = H.listHTML([{ process_id: 7, name: 'CELL-C', worst: 'red', count: 2, headline: 'x' }], {
        7: [
            { kind: 'counter_slot_idle', severity: 'red', headline: 'The PLC counts on Prod_Counter_02',
                data: { tag: 'MES_Cell.Prod_Counter_02', slot: 2, catid: '1002' } },
            { kind: 'counter_slot_lockstep', severity: 'info', headline: 'a second counter of the same stroke',
                data: { tag: 'MES_Cell.Prod_Counter_03', slot: 3, catid: '1003' } },
        ],
    });
    assert.ok(html.indexOf('data-act="ch-repoint" data-pid="7" data-tag="MES_Cell.Prod_Counter_02"') > 0, html);
    assert.ok(html.indexOf('Count Prod_Counter_02') > 0, 'the button names the slot');
    assert.strictEqual(html.split('data-act="ch-repoint"').length, 2, 'the lockstep line suggests nothing');
});

check('one block per process, toned by its worst item', () => {
    const html = H.listHTML([
        { process_id: 3, name: 'CELL-A', worst: 'red', count: 3, headline: 'Wrong part: the PLC is on 1002' },
        { process_id: 4, name: 'CELL-B', worst: 'info', count: 1, headline: 'The incoming style STYLE-B has no CATIDs' },
    ]);
    assert.ok(html.indexOf('pd-ch-red') > 0 && html.indexOf('data-open="3"') > 0, 'the red block for process 3');
    assert.ok(html.indexOf('and 2 more') > 0, 'the count past the headline');
    assert.ok(html.indexOf('pd-ch-info') > 0 && html.indexOf('more</span>', html.indexOf('CELL-B')) < 0,
        'a single item says nothing more');
});

check('names and headlines are escaped', () => {
    const html = H.listHTML([{ process_id: 1, name: '<b>X', worst: 'amber', count: 1, headline: 'a & "b"' }]);
    assert.ok(html.indexOf('&lt;b&gt;X') > 0 && html.indexOf('a &amp; &quot;b&quot;') > 0, html);
});

if (failed) {
    console.log('FAILED: ' + failed);
    process.exit(1);
}
console.log('catid health: all passed');
