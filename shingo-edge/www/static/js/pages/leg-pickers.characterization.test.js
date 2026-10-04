// leg-pickers.characterization.test.js — no list that feeds a claim leg offers
// a lane.
//
// The claim and routing saves refuse a lane on every leg, staging included.
// The pickers that feed a leg are the three routing pickers (Settings, the
// Add-process sheet and the Edit-process sheet) and the Quality Hold
// destination picker. Each draws from Core's node list, so each must leave a
// LANE out, and all four ask one predicate in desktop-bodies.js.
//
// TWO HALVES. The rule runs under node against a node list with a lane, a
// group, a plain node and a lane's slot. The wiring is read off the page's
// source: the pickers live inside an ES module that needs a DOM, so the test
// counts the sites that declare a leg picker and requires the predicate at
// every one. A fourth routing picker, or a picker that stops asking, fails.

'use strict';

const fs = require('fs');
const path = require('path');
const assert = require('assert');

const B = require(path.join(__dirname, 'desktop-bodies.js'));
const PAGE = fs.readFileSync(path.join(__dirname, 'processes-desktop.js'), 'utf8');

let failures = 0;
let checks = 0;
function test(name, fn) {
    try {
        fn();
        checks++;
    } catch (e) {
        failures++;
        console.log('  FAIL ' + name + '\n       ' + (e && e.message));
    }
}

const CORE = [
    { name: 'SUP', node_type: 'NGRP' },
    { name: 'SUP-L1', node_type: 'LANE' },
    { name: 'SUP-L1-S1' },
    { name: 'STG-1', node_type: '' },
];

test('isLane: a LANE is a lane', () => {
    assert.strictEqual(B.isLane(CORE[1]), true);
});

test('isLane: a group, a lane slot, a plain node and nothing are not', () => {
    assert.strictEqual(B.isLane(CORE[0]), false);
    assert.strictEqual(B.isLane(CORE[2]), false);
    assert.strictEqual(B.isLane(CORE[3]), false);
    assert.strictEqual(B.isLane(undefined), false);
    assert.strictEqual(B.isLane(null), false);
});

test('laneExclusion: a lane name is refused, with a reason', () => {
    const why = B.laneExclusion(CORE, 'SUP-L1');
    assert.ok(why, 'a lane must carry a reason');
    assert.match(why, /lane/);
});

test('laneExclusion: a group, a slot, a plain node and an unknown name are offered', () => {
    for (const name of ['SUP', 'SUP-L1-S1', 'STG-1', 'NOT-IN-CORE']) {
        assert.strictEqual(B.laneExclusion(CORE, name), '', name);
    }
});

test('laneExclusion: no node list yet refuses nothing', () => {
    assert.strictEqual(B.laneExclusion(null, 'SUP-L1'), '');
    assert.strictEqual(B.laneExclusion(undefined, 'SUP-L1'), '');
});

// The text of each routing picker declaration, to the close of its options
// object. The two sheets declare theirs as `pickerInit(routingPickerKey(…`;
// Settings declares its own in routingPickersReady as `pickerInit(key, …`.
function routingPickerSites() {
    const sites = [];
    const declare = (at) => sites.push(PAGE.slice(at, PAGE.indexOf('});', at)));
    const open = 'pickerInit(routingPickerKey(';
    for (let at = PAGE.indexOf(open); at >= 0; at = PAGE.indexOf(open, at + open.length)) declare(at);
    const ready = PAGE.indexOf('function routingPickersReady(');
    assert.ok(ready >= 0, 'routingPickersReady not found');
    const settings = PAGE.indexOf('pickerInit(key, {', ready);
    assert.ok(settings >= 0 && settings < PAGE.indexOf('\n}\n', ready), 'the Settings routing picker not found');
    declare(settings);
    return sites;
}

test('the three routing pickers each refuse a lane', () => {
    const sites = routingPickerSites();
    assert.strictEqual(sites.length, 3,
        'expected the Settings, Add-process and Edit-process routing pickers; found ' + sites.length);
    sites.forEach((s, i) => {
        assert.ok(/exclude:[\s\S]*B\(\)\.laneExclusion\(S\.coreNodes, n\)/.test(s),
            'routing picker #' + (i + 1) + ' does not ask laneExclusion:\n' + s);
    });
});

test('the Quality Hold destination picker leaves lanes out', () => {
    const start = PAGE.indexOf('async function openContainmentPicker(');
    assert.ok(start >= 0, 'openContainmentPicker not found');
    const body = PAGE.slice(start, PAGE.indexOf('\n}\n', start));
    assert.ok(/\(S\.coreNodes \|\| \[\]\)\.filter\(n => !B\(\)\.isLane\(n\)\)/.test(body),
        'the containment picker offers Core nodes without the lane filter:\n' + body);
});

if (failures) {
    console.error('\n' + failures + ' failed, ' + checks + ' passed');
    process.exit(1);
}
console.log(checks + ' leg-picker checks passed');
