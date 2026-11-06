// tag-suggest.test.js — the tag combo's counter and identity options.
// Run by www/tag_suggest_test.go.
'use strict';

const path = require('path');
const assert = require('assert');
const T = require(path.join(__dirname, 'tag-suggest.js'));

let failed = 0;
function check(name, fn) {
    try { fn(); console.log('ok   ' + name); } catch (e) { failed++; console.log('FAIL ' + name + '\n     ' + e.message); }
}

const tags = [
    { name: 'MES_Cell', type: 'STRUCT', enabled: false },
    { name: 'MES_Cell.Prod_Counter_02', type: 'DINT', configured: true, enabled: true, value: 1234 },
    { name: 'Other_Tag', type: 'BOOL', enabled: true, value: true },
];

check('a counter field offers listed members first, then the struct convention, then the rest', () => {
    const o = T.options(tags, 'counter');
    assert.deepStrictEqual(o.map(x => x.name), ['MES_Cell.Prod_Counter_02', 'MES_Cell.Prod_Counter_01', 'MES_Cell', 'Other_Tag']);
    assert.ok(o[0].note.indexOf('configured') >= 0 && o[0].note.indexOf('reads 1234') >= 0 && o[0].note.indexOf('published') >= 0, o[0].note);
    assert.ok(o[1].note.indexOf('member of MES_Cell') >= 0, o[1].note);
});

check('an identity field offers the CATID member', () => {
    const o = T.options(tags, 'identity');
    assert.strictEqual(o[0].name, 'MES_Cell.CATID_01');
});

check('any other kind lists the tags as they come, a struct value not shown', () => {
    const o = T.options([{ name: 'S', type: 'STRUCT', value: { a: 1 }, enabled: true }], '');
    assert.deepStrictEqual(o, [{ name: 'S', note: 'STRUCT · published' }]);
    assert.deepStrictEqual(T.options(null, 'counter'), []);
});

check('the derived identity follows the counter index', () => {
    assert.strictEqual(T.derivedIdentity('MES_Cell.Prod_Counter_02'), 'MES_Cell.CATID_02');
    assert.strictEqual(T.derivedIdentity('MES_Cell.Count'), 'MES_Cell.CATID_01');
    assert.strictEqual(T.derivedIdentity('Flat_Tag'), '');
});

if (failed) { console.log('FAILED: ' + failed); process.exit(1); }
console.log('tag suggest: all passed');
