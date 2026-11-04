// catid-health.test.js — the health strip's list renderer.
// Run by www/catid_health_js_test.go.
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

check('one block per process, toned by its worst item', () => {
    const html = H.listHTML([
        { process_id: 3, name: 'CELL-A', worst: 'red', count: 3, headline: 'Wrong part: the PLC is on 1002' },
        { process_id: 4, name: 'CELL-B', worst: 'info', count: 1, headline: '1 automatic decision(s) in 24 h' },
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
