// tag-suggest.test.js — the tag combo's counter and identity options.
// Run by www/tag_suggest_test.go.
'use strict';

const path = require('path');
const assert = require('assert');
const T = require(path.join(__dirname, 'tag-suggest.js'));

// An argument runs only the checks whose names start with it.
const only = process.argv[2] || '';
let failed = 0;
let ran = 0;
function check(name, fn) {
    if (only && name.indexOf(only) !== 0) return;
    ran++;
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

check('any other kind lists the tags by name, a struct value not shown', () => {
    const o = T.options([{ name: 'S', type: 'STRUCT', value: { a: 1 }, enabled: true }], '');
    assert.deepStrictEqual(o, [{ name: 'S', note: 'STRUCT · published' }]);
    assert.deepStrictEqual(T.options(null, 'counter'), []);
});

check('the derived identity follows the counter index', () => {
    assert.strictEqual(T.derivedIdentity('MES_Cell.Prod_Counter_02'), 'MES_Cell.CATID_02');
    assert.strictEqual(T.derivedIdentity('MES_Cell.Count'), 'MES_Cell.CATID_01');
    assert.strictEqual(T.derivedIdentity('Flat_Tag'), '');
});

// A station cell's tag list, generic: station structs by the family's naming,
// one with an operator-load tag beside it, and the shapes never offered.
const cell = [
    { name: 'MES_Cell', type: 'STRUCT', enabled: false },
    { name: 'Sta010', type: 'STRUCT', enabled: false },
    { name: 'Sta020', type: 'STRUCT', enabled: false },
    { name: 'Sta020_PP_Op_Loaded_Parts_Count', type: 'DINT', enabled: false },
    { name: 'Sta910_T1', type: 'STRUCT', enabled: false },
    { name: 'Sta1234', type: 'STRUCT', enabled: false },
    { name: 'Station_Data', type: 'STRUCT', enabled: false },
    { name: 'Robot_1', type: 'STRUCT', enabled: false },
    { name: 'Load_Count', type: 'DINT', enabled: true, value: 40 },
    { name: 'Sta030_Ware_Count', type: 'DINT', enabled: true, value: 12 },
    { name: 'Sta040_Gun1_Weld_Count', type: 'DINT', enabled: false },
    { name: 'Sta040_Tip_Change_Count', type: 'DINT', enabled: false },
    { name: 'Sta050_Dwell_TMR.ACC', type: 'DINT', enabled: false },
    { name: 'Cycle_Count', type: 'DINT', enabled: false },
    { name: 'Operator_Over_Count', type: 'DINT', enabled: false },
    { name: 'Final_Index', type: 'DINT', enabled: false },
    { name: 'Exit_FIFO_CNTR', type: 'DINT', enabled: false },
    { name: 'MGMT_Parts_Made', type: 'DINT', enabled: false },
    { name: 'Telesis_Line_1', type: 'STRING', enabled: false },
];

check('station: offers each station struct\'s conventional member, operator load first, by name only', () => {
    const o = T.options(cell, 'station');
    const conv = o.filter(x => /\.Prod_Tool_1_CNT\.ACC$/.test(x.name));
    assert.deepStrictEqual(conv.map(x => x.name),
        ['Sta020.Prod_Tool_1_CNT.ACC', 'Sta010.Prod_Tool_1_CNT.ACC', 'Sta910_T1.Prod_Tool_1_CNT.ACC']);
    assert.strictEqual(o[0].name, 'Sta020.Prod_Tool_1_CNT.ACC', 'the operator-load station leads');
    assert.ok(o[0].note.indexOf('operator load') >= 0, o[0].note);
    conv.forEach(x => {
        assert.ok(x.note.indexOf('the family\u2019s convention \u2014 verified when saved') >= 0, x.note);
        assert.ok(x.note.indexOf('reads') < 0, 'no live value for an unbound suggestion: ' + x.note);
    });
    assert.ok(o.some(x => x.name === 'Load_Count'), 'another tag is still offered after them');
});

check('station: never offers a gun, a tip, a timer, a cycle, an index, a FIFO, a management or a marker tag', () => {
    const names = T.options(cell, 'station').map(x => x.name);
    ['Sta030_Ware_Count', 'Sta040_Gun1_Weld_Count', 'Sta040_Tip_Change_Count', 'Sta050_Dwell_TMR.ACC', 'Cycle_Count',
        'Operator_Over_Count', 'Final_Index', 'Exit_FIFO_CNTR', 'MGMT_Parts_Made', 'Telesis_Line_1'].forEach(n =>
        assert.ok(names.indexOf(n) < 0, n + ' offered'));
});

if (!ran) { console.log('FAILED: no check is named ' + only); process.exit(1); }
if (failed) { console.log('FAILED: ' + failed); process.exit(1); }
console.log('tag suggest: all passed');
