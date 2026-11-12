// leg-pickers.characterization.test.js — no list that feeds a claim leg offers
// a lane, and none that feeds a role where Core picks (a source or a
// destination) offers a member of a node group.
//
// The claim and routing saves refuse a lane on every leg, staging included.
// The pickers that feed a leg are the two routing pickers (Settings and the
// Add-process sheet) and the Quality Hold destination picker. Each draws from
// Core's node list, so each must leave a LANE out, and all three ask one
// predicate in desktop-bodies.js.
//
// TWO HALVES. The rule runs under node against a node list with a lane, a
// group, a plain node and a lane's slot. The wiring is read off the page's
// source: the pickers live inside an ES module that needs a DOM, so the test
// counts the sites that declare a leg picker and requires the predicate at
// every one. A third routing picker, or a picker that stops asking, fails.

'use strict';

const fs = require('fs');
const path = require('path');
const assert = require('assert');

// The page code under test. This file lives in www/testdata, outside the
// embedded static tree, so no binary carries or serves it
// (TestStaticTree_HoldsNoTestJS); the code it reads stays in www/static.
const SRC_DIR = path.join(__dirname, '..', '..', '..', '..', 'static', 'js', 'pages');

const B = require(path.join(SRC_DIR, 'desktop-bodies.js'));
const PAGE = fs.readFileSync(path.join(SRC_DIR, 'processes-desktop.js'), 'utf8');

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
    { name: 'SUP-S2', group: 'SUP' },
];
const MO = fs.readFileSync(path.join(SRC_DIR, 'manual-order.js'), 'utf8');
const PROD = fs.readFileSync(path.join(SRC_DIR, 'production.js'), 'utf8');

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

test('legExclusion: a lane name is refused, with a reason', () => {
    const why = B.legExclusion(CORE, 'SUP-L1');
    assert.ok(why, 'a lane must carry a reason');
    assert.match(why, /lane/);
});

test('legExclusion: a group, a slot, a plain node and an unknown name are offered', () => {
    for (const name of ['SUP', 'SUP-L1-S1', 'STG-1', 'NOT-IN-CORE']) {
        assert.strictEqual(B.legExclusion(CORE, name), '', name);
    }
});

test('legExclusion: a member is refused where Core picks, naming its group', () => {
    for (const role of ['source', 'destination']) {
        const why = B.legExclusion(CORE, 'SUP-S2', role);
        assert.ok(why, role + ' must refuse a member');
        assert.match(why, /SUP/, 'the reason names the group');
    }
});

test('legExclusion: a member is a staging spot', () => {
    assert.strictEqual(B.legExclusion(CORE, 'SUP-S2', 'staging'), '');
});

test('legExclusion: no node list yet refuses nothing', () => {
    assert.strictEqual(B.legExclusion(null, 'SUP-L1'), '');
    assert.strictEqual(B.legExclusion(undefined, 'SUP-L1'), '');
});

// The routing rule of each set picker (setInit: the positions and the
// routing set, one list in four buckets). The process sheet declares its rule
// inline in `setInit(PROC_SET, {`; Settings' is the function stSetRule.
function routingRuleSites() {
    const sites = [];
    const sheet = PAGE.indexOf('setInit(PROC_SET, {');
    assert.ok(sheet >= 0, 'the process sheet’s set picker not found');
    sites.push(PAGE.slice(sheet, PAGE.indexOf('});', sheet)));
    const st = PAGE.indexOf('function stSetRule(');
    assert.ok(st >= 0, 'stSetRule not found');
    sites.push(PAGE.slice(st, PAGE.indexOf('\n}\n', st)));
    return sites;
}

test('the two set pickers each ask the leg rule in the bucket’s role', () => {
    const sites = routingRuleSites();
    sites.forEach((s, i) => {
        assert.ok(/B\(\)\.legExclusion\(S\.coreNodes, (?:n|name), b\)/.test(s),
            'set picker #' + (i + 1) + ' does not ask legExclusion in its bucket’s role:\n' + s);
    });
});

test('the Quality Hold destination picker greys lanes and members', () => {
    const start = PAGE.indexOf('async function openContainmentPicker(');
    assert.ok(start >= 0, 'openContainmentPicker not found');
    const body = PAGE.slice(start, PAGE.indexOf('\n}\n', start));
    // The node adapter's rows (nodeRows): Core's nodes in the pickers' order
    // (census #2), the destination rule greying what a leg may not name.
    assert.ok(/rule: n => B\(\)\.legExclusion\(S\.coreNodes, n\.name, 'destination'\)/.test(body) &&
        /nodeRows\(nodes, field\)/.test(body),
        'the containment picker offers Core nodes without the destination rule:\n' + body);
});

// The manual order panel, on its own page and on Production: an order's
// pickup and delivery are where Core picks. Each field's rule is the shared
// node adapter's (shared/node-picker.js): the pickup greys a member, naming
// its group; the delivery greys one except for a complex order, whose Core
// Node is the cell's own position, and a member picked there is unpicked when
// the type leaves complex. What each field then offers and greys is drawn by
// TestEdgePagePickerPins (testdata/edge-pages/*-options.txt).
for (const [name, src] of [['manual-order.js', MO], ['production.js', PROD]]) {
    test(name + ': the pickup greys members', () => {
        assert.ok(/pickup: nodePicker\(document\.getElementById\('mo-pickup'\), nodes,\s*\{ rule: W\.any\(W\.robot, W\.member\) \}/.test(src),
            name + ' offers members as a pickup');
    });
    test(name + ': the delivery takes members for a complex order only', () => {
        assert.ok(/orderType\(\) === 'complex' \? '' : W\.member\(n\)/.test(src), name + ' does not grey a member in the delivery');
        const start = src.indexOf('function updateOrderForm()');
        assert.ok(start >= 0, name + ': updateOrderForm not found');
        const body = src.slice(start, src.indexOf('\n}\n', start));
        assert.ok(/if \(t !== 'complex' && memberOf\[pickers\.delivery\.value\(\)\]\) pickers\.delivery\.set\(''\);/.test(body),
            name + ': updateOrderForm keeps a member delivery outside a complex order');
    });
}

if (failures) {
    console.error('\n' + failures + ' failed, ' + checks + ' passed');
    process.exit(1);
}
console.log(checks + ' leg-picker checks passed');
