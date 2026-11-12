// catid-health.test.js — the health items' renderers: the list's count, a
// process's items.
// Run by www/catid_health_list_test.go.
'use strict';

const path = require('path');
const assert = require('assert');

// The page code under test. This file lives in www/testdata, outside the
// embedded static tree, so no binary carries or serves it
// (TestStaticTree_HoldsNoTestJS); the code it reads stays in www/static.
const SRC_DIR = path.join(__dirname, '..', '..', '..', '..', 'static', 'js', 'pages');

const H = require(path.join(SRC_DIR, 'catid-health.js'));

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

check('a process with no red or amber item has no count', () => {
    assert.strictEqual(H.countHTML(null), '');
    assert.strictEqual(H.countHTML({ process_id: 4, worst: 'info', count: 2, red: 0, amber: 0 }), '', 'info adds nothing');
    assert.strictEqual(H.needing({ process_id: 4, worst: 'info', count: 2, red: 0, amber: 0 }), 0);
});

check('the count is red plus amber, toned by the worst, and opens Settings', () => {
    const red = H.countHTML({ process_id: 3, worst: 'red', count: 7, red: 2, amber: 3 });
    assert.ok(red.indexOf('chip chip-err') > 0 && red.indexOf('>5</span>') > 0, red);
    assert.ok(red.indexOf('data-act="open-settings" data-process="3"') > 0, 'it opens the process on Settings');
    const amber = H.countHTML({ process_id: 5, worst: 'amber', count: 1, red: 0, amber: 1 });
    assert.ok(amber.indexOf('chip chip-warn') > 0 && amber.indexOf('>1</span>') > 0, amber);
});

check('an item carries its verbs', () => {
    const held = H.itemHTML(3, { kind: 'held', severity: 'info', headline: '5 held on CATID 1007', data: { hold_id: 11 } });
    assert.ok(held.indexOf('pd-ch-info') > 0, held);
    assert.ok(held.indexOf('data-act="ch-charge" data-hold="11"') > 0, 'Charge on the hold');
    assert.ok(held.indexOf('data-act="ch-writeoff" data-hold="11"') > 0, 'Write off on the hold');
    const unmapped = H.itemHTML(3, { kind: 'unmapped_catid', severity: 'amber', headline: 'CATID 1003 maps to no style',
        data: { catid: '1003', suggestion: { style_id: 4, style_name: 'STYLE-B' },
            styles: [{ id: 4, name: 'STYLE-B' }, { id: 5, name: '<C>' }] } });
    assert.ok(unmapped.indexOf('data-act="ch-pin" data-pid="3" data-style="4" data-name="STYLE-B" data-catid="1003"') > 0, 'the suggestion');
    assert.ok(unmapped.indexOf('Pin to STYLE-B') > 0, 'the suggestion names its style');
    assert.ok(unmapped.indexOf('data-act="ch-pin-pick"') > 0 && unmapped.indexOf('&lt;C&gt;') > 0, 'the picker, escaped');
});

check('a slot the PLC counts on carries the click that counts it; its lockstep line suggests nothing', () => {
    const idle = H.itemHTML(7, { kind: 'counter_slot_idle', severity: 'red', headline: 'The PLC counts on Prod_Counter_02',
        data: { tag: 'MES_Cell.Prod_Counter_02', slot: 2, catid: '1002' } });
    assert.ok(idle.indexOf('data-act="ch-repoint" data-pid="7" data-tag="MES_Cell.Prod_Counter_02"') > 0, idle);
    assert.ok(idle.indexOf('Count Prod_Counter_02') > 0, 'the button names the slot');
    const lock = H.itemHTML(7, { kind: 'counter_slot_lockstep', severity: 'info', headline: 'a second counter of the same stroke',
        data: { tag: 'MES_Cell.Prod_Counter_03', slot: 3, catid: '1003' } });
    assert.ok(lock.indexOf('data-act=') < 0, lock);
});

check('a wrong part with its changeover carries the one tap that starts it', () => {
    const html = H.itemHTML(9, { kind: 'wrong_part', severity: 'red',
        headline: 'The PLC is on 1002 (ALT); RUN is active. Start a changeover to ALT — 6 strokes held at P4 and P5.',
        detail: 'since 2026-11-07 10:00, 6 parts counted as ALT', data: { catid: '1002' },
        action: { kind: 'start_changeover', to_style_id: 20, to_style_name: '<ALT>' } });
    assert.ok(html.indexOf('data-act="ch-start" data-pid="9" data-style="20" data-name="&lt;ALT&gt;"') > 0, html);
    assert.ok(html.indexOf('Start changeover to &lt;ALT&gt;') > 0, 'the button names the style, escaped');
    const plain = H.itemHTML(9, { kind: 'wrong_part', severity: 'amber', headline: 'Wrong part: the PLC is on 1007', data: { catid: '1007' } });
    assert.ok(plain.indexOf('data-act=') < 0, 'an item with no action offers nothing');
});

check('the detail is behind the headline, never its words (a PLC read error among them)', () => {
    const html = H.itemHTML(1, { kind: 'counter_unreadable', severity: 'red', headline: 'Production counter T unreadable',
        detail: 'read T: <timeout> & "x"' });
    assert.ok(html.indexOf('title="read T: &lt;timeout&gt; &amp; &quot;x&quot;">Production counter T unreadable</span>') > 0, html);
    assert.strictEqual(html.split('timeout').length, 2, 'said once, in the title');
});

check('headlines are escaped', () => {
    const html = H.itemHTML(1, { kind: 'shared_catid', severity: 'amber', headline: 'a & "b" <c>' });
    assert.ok(html.indexOf('a &amp; &quot;b&quot; &lt;c&gt;') > 0, html);
});

if (failed) {
    console.log('FAILED: ' + failed);
    process.exit(1);
}
console.log('catid health: all passed');
