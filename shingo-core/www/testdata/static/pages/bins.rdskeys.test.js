// Unit tests for bins-rdskeys.js, the RDS keys adapter a family tab's key strip
// draws its Lift and Wait pickers through. Run under plain Node via
// the Go wrapper bins_rdskeys_view_test.go. Exit 0 on pass, 1 on any failure.
//
// What they hold: each field opens on its own view (Lift keys, Wait keys),
// which keeps a key by the operation RDS gives it, never by its name; All
// keeps everything; the default row and the saved row are in every view; a key
// with no operation is under All only.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

// The page code under test stays in www/static; this file is outside the
// embedded tree (TestStaticTree_HoldsNoTestJS).
const SRC = path.join(__dirname, '..', '..', '..', 'static', 'pages', 'bins-rdskeys.js');

let failures = 0;
function check(name, cond, detail) {
    if (cond) {
        console.log('  ok  ' + name);
    } else {
        failures++;
        console.log('  FAIL ' + name + (detail ? ' — ' + detail : ''));
    }
}
function eq(name, got, want) {
    const g = JSON.stringify(got), w = JSON.stringify(want);
    check(name, g === w, 'got ' + g + ', want ' + w);
}

// The module runs in a vm with no ES resolver: its import is dropped (the
// shell is not needed by the pure parts) and its exports made plain.
function load() {
    let src = fs.readFileSync(SRC, 'utf8');
    src = src.replace(/^import .*$/m, '').replace(/^export /mg, '');
    const ctx = {};
    vm.createContext(ctx);
    vm.runInContext(src + '\nthis.rdsKeyRows = rdsKeyRows; this.rdsKeyViews = rdsKeyViews; this.rdsKeySelected = rdsKeySelected;', ctx);
    return ctx;
}
const m = load();

// The scene's keys as the server hands them (bins_family_keys.go keyOptions):
// a sized lift key, a sized wait key, the defaults as listed keys, a key RDS
// gives no operation, and a key given two operations.
const listed = [
    { value: 'JackLoad', label: 'JackLoad', note: 'JackLoad', ops: ['JackLoad'] },
    { value: 'jackload_4548', label: 'jackload_4548', note: 'JackLoad', ops: ['JackLoad'] },
    { value: 'JackUnload', label: 'JackUnload', note: 'JackUnload', ops: ['JackUnload'] },
    { value: 'lift_plain', label: 'lift_plain' },
    { value: 'Wait', label: 'Wait', note: 'Wait', ops: ['Wait'] },
    { value: 'wait_4548', label: 'wait_4548', note: 'Wait, Wait2', ops: ['Wait', 'Wait2'] },
];
const liftField = {
    name: 'lift_bintask', view: 'Lift keys', viewOps: ['JackLoad'],
    options: [
        { value: '', label: 'JackLoad (default)', pinned: true },
        { value: 'jackload_9999', label: 'jackload_9999', note: 'not in RDS', pinned: true, selected: true },
    ].concat(listed),
};
const waitField = {
    name: 'wait_bintask', view: 'Wait keys', viewOps: ['Wait'],
    options: [{ value: '', label: 'Wait (default)', pinned: true, selected: true }].concat(listed),
};

// kept is the values a field's view keeps, in the rows' order.
function kept(field, viewKey) {
    const view = m.rdsKeyViews(field).find((v) => v.key === viewKey);
    return m.rdsKeyRows(field).filter((r) => view.keep(r)).map((r) => r.value);
}

console.log('views');
eq('the lift field opens on Lift keys, then All', m.rdsKeyViews(liftField).map((v) => v.label), ['Lift keys', 'All']);
eq('the wait field opens on Wait keys, then All', m.rdsKeyViews(waitField).map((v) => v.label), ['Wait keys', 'All']);
eq('Lift keys: the default, the saved key not in RDS, and the keys RDS runs as the default lift key',
    kept(liftField, 'kind'), ['', 'jackload_9999', 'JackLoad', 'jackload_4548']);
eq('Wait keys: the default and the keys RDS runs as the default wait key (a second operation does not drop one)',
    kept(waitField, 'kind'), ['', 'Wait', 'wait_4548']);
eq('All keeps every row of the lift field', kept(liftField, 'all'), liftField.options.map((o) => o.value));
eq('All keeps every row of the wait field', kept(waitField, 'all'), waitField.options.map((o) => o.value));
check('a lift key is not under Wait keys', !kept(waitField, 'kind').includes('jackload_4548'));
check('a wait key is not under Lift keys', !kept(liftField, 'kind').includes('wait_4548'));
check('a key with no operation is under All only',
    !kept(liftField, 'kind').includes('lift_plain') && !kept(waitField, 'kind').includes('lift_plain') &&
    kept(liftField, 'all').includes('lift_plain') && kept(waitField, 'all').includes('lift_plain'));
check('the saved key not in RDS is in both views',
    kept(liftField, 'kind').includes('jackload_9999') && kept(liftField, 'all').includes('jackload_9999'));
check('a view keeps by operation, never by name: a key named like a lift key that RDS runs as a wait is a wait key',
    (() => {
        const f = { name: 'lift_bintask', view: 'Lift keys', viewOps: ['JackLoad'],
            options: [{ value: 'jackload_x', label: 'jackload_x', note: 'Wait', ops: ['Wait'] }] };
        return kept(f, 'kind').length === 0;
    })());
eq('a copy without the default key: the view keeps the pinned rows only',
    kept(Object.assign({}, liftField, { viewOps: [] }), 'kind'), ['', 'jackload_9999']);

console.log('rows');
const rows = m.rdsKeyRows(liftField);
eq('the default row: blank value, the key it sends', [rows[0].value, rows[0].label, rows[0].note], ['', 'JackLoad (default)', undefined]);
eq('the saved key the scene lost: noted "not in RDS"', [rows[1].label, rows[1].note], ['jackload_9999', 'not in RDS']);
eq("a scene key: RDS's operation is its note, and the search finds it by it",
    [rows[3].label, rows[3].note, rows[3].search], ['jackload_4548', 'JackLoad', 'JackLoad']);
eq('a key with no operation has no note', rows[5].note, undefined);
eq('the saved value is selected', m.rdsKeySelected(liftField), ['jackload_9999']);
eq('a default saved selects the default row', m.rdsKeySelected(waitField), ['']);

if (failures) {
    console.log(failures + ' failure(s)');
    process.exit(1);
}
console.log('all passed');
