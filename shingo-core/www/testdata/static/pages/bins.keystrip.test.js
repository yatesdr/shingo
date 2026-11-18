// Unit tests for bins-keystrip.js, a family tab's key strip on the Bins page:
// its three states (at rest, a field open, a key picked and not saved) and
// Cancel. Run under plain Node via the Go wrapper bins_keystrip_view_test.go.
// Exit 0 on pass, 1 on any failure.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

// The page code under test stays in www/static; this file is outside the
// embedded tree (TestStaticTree_HoldsNoTestJS).
const SRC = path.join(__dirname, '..', '..', '..', 'static', 'pages', 'bins-keystrip.js');

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
// pure parts need no picker) and its exports made plain.
function load() {
    let src = fs.readFileSync(SRC, 'utf8');
    src = src.replace(/^import .*$/m, '').replace(/^export /mg, '');
    const ctx = {};
    vm.createContext(ctx);
    vm.runInContext(src + '\nthis.m = { keyStripState, openKey, pickKey, closeKey, cancelKeys, keyStripView };', ctx);
    return ctx.m;
}
const m = load();

// Two fields as the server hands them (bins_family_keys.go keyField): a lift
// saved to a listed key, a wait saved to a key the scene copy no longer lists.
const fields = {
    lift: { name: 'lift_bintask', options: [
        { value: '', label: 'JackLoad (default)', pinned: true },
        { value: 'jackload_3230', label: 'jackload_3230', note: 'JackLoad', ops: ['JackLoad'] },
        { value: 'jackload_4548', label: 'jackload_4548', note: 'JackLoad', ops: ['JackLoad'], pinned: true, selected: true },
    ] },
    wait: { name: 'wait_bintask', options: [
        { value: '', label: 'Wait (default)', pinned: true },
        { value: 'wait_3230_v1', label: 'wait_3230_v1', note: 'not in RDS', pinned: true, selected: true, missing: true },
        { value: 'wait_4548', label: 'wait_4548', note: 'Wait', ops: ['Wait'] },
    ] },
};
const strip = (v) => ({
    lift: [v.keys.lift.label, v.keys.lift.note, v.keys.lift.open],
    wait: [v.keys.wait.label, v.keys.wait.note, v.keys.wait.open],
    dirty: v.dirty,
});

console.log('at rest');
const rest = m.keyStripState(fields);
eq('each key as saved, no field open, nothing to save', strip(m.keyStripView(rest)),
    { lift: ['jackload_4548', '', false], wait: ['wait_3230_v1', 'not in RDS', false], dirty: false });
eq('the hidden inputs carry what is saved', [m.keyStripView(rest).keys.lift.value, m.keyStripView(rest).keys.wait.value],
    ['jackload_4548', 'wait_3230_v1']);
const blank = m.keyStripState({ lift: { options: [{ value: '', label: 'JackLoad (default)', pinned: true, selected: true }] } });
eq('a blank key reads as the default', m.keyStripView(blank).keys.lift.label, 'JackLoad (default)');

console.log('open');
const liftOpen = m.openKey(rest, 'lift');
eq('Change on Lift opens Lift alone', strip(m.keyStripView(liftOpen)),
    { lift: ['jackload_4548', '', true], wait: ['wait_3230_v1', 'not in RDS', false], dirty: false });
const waitOpen = m.openKey(m.closeKey(liftOpen), 'wait');
eq('one field open at a time: Change on Wait closes Lift', [m.keyStripView(waitOpen).keys.lift.open, m.keyStripView(waitOpen).keys.wait.open], [false, true]);
eq('Escape or a press away closes it with nothing changed', strip(m.keyStripView(m.closeKey(liftOpen))), strip(m.keyStripView(rest)));
eq('an unknown field opens nothing', m.openKey(rest, 'park').open, '');

console.log('picked, not saved');
const picked = m.pickKey(liftOpen, 'lift', 'jackload_3230');
eq('a pick closes the picker and marks the key not saved; Save keys and Cancel show', strip(m.keyStripView(picked)),
    { lift: ['jackload_3230', 'not saved', false], wait: ['wait_3230_v1', 'not in RDS', false], dirty: true });
eq('the hidden input carries the picked key', m.keyStripView(picked).keys.lift.value, 'jackload_3230');
const toDefault = m.pickKey(m.openKey(picked, 'wait'), 'wait', '');
eq('the default picked is not saved either', strip(m.keyStripView(toDefault)).wait, ['Wait (default)', 'not saved', false]);
const back = m.pickKey(m.openKey(picked, 'lift'), 'lift', 'jackload_4548');
eq('picking the saved key again leaves nothing to save', strip(m.keyStripView(back)),
    { lift: ['jackload_4548', '', false], wait: ['wait_3230_v1', 'not in RDS', false], dirty: false });
eq('closing the other field keeps the pick', m.keyStripView(m.closeKey(m.openKey(picked, 'wait'))).keys.lift.note, 'not saved');

console.log('Cancel');
const cancelled = m.cancelKeys(toDefault);
eq('Cancel puts both fields back, the "not in RDS" mark with it', strip(m.keyStripView(cancelled)), strip(m.keyStripView(rest)));
eq('Cancel puts the hidden inputs back', [m.keyStripView(cancelled).keys.lift.value, m.keyStripView(cancelled).keys.wait.value],
    ['jackload_4548', 'wait_3230_v1']);
eq('Cancel with a field open closes it', m.cancelKeys(m.openKey(picked, 'wait')).open, '');

if (failures) {
    console.log(failures + ' failure(s)');
    process.exit(1);
}
console.log('all passed');
