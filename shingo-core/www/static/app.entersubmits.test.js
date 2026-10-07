// app.entersubmits.test.js — W5 (ui-cleanup 2026-10-07): Enter in an input
// carrying data-action-keydown="enterSubmits:<verb>" calls <verb> from the
// page's delegateActions map, as the Nodes page's "New Node Group" and
// "Add Lane" inputs do. A page module's functions are never on window, so the
// target must be found in the map, not on window.
//
// Runs under plain node through its Go wrapper (app_entersubmits_wrapper_test.go).
// The real delegateActions (shared/utils.js) and the real enterSubmits (app.js,
// cut out of the file by name) are evaluated against a minimal DOM stub.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0;
let failed = 0;
function check(cond, label) {
    if (cond) { passed++; } else { failed++; console.error('FAIL: ' + label); }
}

// Minimal element: dataset, parent chain, closest('[data-action-x]').
function makeNode(dataset, parent) {
    const n = { dataset: Object.assign({}, dataset || {}), _parent: parent || null, _listeners: {} };
    n.addEventListener = (ev, fn) => { (n._listeners[ev] = n._listeners[ev] || []).push(fn); };
    n.contains = (other) => {
        for (let cur = other; cur; cur = cur._parent) if (cur === n) return true;
        return false;
    };
    n.closest = (sel) => {
        const m = /^\[data-(action(?:-[a-z]+)?)\]$/.exec(sel);
        if (!m) return null;
        const key = m[1].replace(/-([a-z])/g, (_, c) => c.toUpperCase());
        for (let cur = n; cur; cur = cur._parent) if (cur.dataset && cur.dataset[key]) return cur;
        return null;
    };
    return n;
}

function fire(root, type, target, extra) {
    let prevented = false;
    const evt = Object.assign({
        type, target, currentTarget: root,
        preventDefault() { prevented = true; },
        stopPropagation() {},
    }, extra || {});
    (root._listeners[type] || []).forEach((fn) => fn(evt));
    return prevented;
}

const utilsSrc = fs.readFileSync(path.join(__dirname, '..', '..', '..', 'shared', 'utils.js'), 'utf8')
    .replace(/export\s+function\s+/g, 'function ')
    .replace(/export\s+const\s+/g, 'const ');
const appSrc = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
const m = /export function enterSubmits\(targetFnName, el, evt\) \{[\s\S]*?\n\}/.exec(appSrc);
if (!m) { console.error('FAIL: enterSubmits not found in app.js'); process.exit(1); }
const enterSubmitsSrc = m[0].replace(/^export /, '');

const body = makeNode({});
const win = { addEventListener() {} };
const ctx = vm.createContext({
    document: { body, createElement: () => makeNode({}) },
    window: win,
    console, setTimeout, clearTimeout, fetch: () => {},
    EventSource: function () {},
    location: { reload: () => {} },
});
vm.runInContext(utilsSrc + '\n' + enterSubmitsSrc +
    '\n; this.delegateActions = delegateActions; this.enterSubmits = enterSubmits;', ctx);

// The page's wiring, as nodes-supermarket.js does it: one map, every event.
const calls = [];
function createNodeGroup() { calls.push({ fn: 'createNodeGroup', self: this, args: [].slice.call(arguments) }); }
function submitAddLane() { calls.push({ fn: 'submitAddLane', self: this, args: [].slice.call(arguments) }); }
ctx.delegateActions(body, { createNodeGroup, submitAddLane, enterSubmits: ctx.enterSubmits },
    { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });

const ngrp = makeNode({ actionKeydown: 'enterSubmits:createNodeGroup' }, body);
const lane = makeNode({ actionKeydown: 'enterSubmits:submitAddLane' }, body);

check(win.createNodeGroup === undefined && win.submitAddLane === undefined,
    'premise: the targets are not on window');

// Enter in New Node Group → createNodeGroup, once, default prevented.
let prevented = fire(body, 'keydown', ngrp, { key: 'Enter' });
check(calls.length === 1 && calls[0].fn === 'createNodeGroup', 'Enter in ngrp-name calls createNodeGroup once');
check(prevented, 'Enter is preventDefault-ed');
check(calls[0] && calls[0].self === ngrp, 'the target runs with this = the input');

// Enter in Add Lane → submitAddLane.
calls.length = 0;
fire(body, 'keydown', lane, { key: 'Enter' });
check(calls.length === 1 && calls[0].fn === 'submitAddLane', 'Enter in lane-name calls submitAddLane once');

// Any other key does nothing and does not prevent typing.
calls.length = 0;
prevented = fire(body, 'keydown', ngrp, { key: 'a' });
check(calls.length === 0 && !prevented, 'a non-Enter key calls nothing and is not prevented');

// An unknown target is a no-op, not a throw.
calls.length = 0;
const stray = makeNode({ actionKeydown: 'enterSubmits:noSuchVerb' }, body);
let threw = false;
try { fire(body, 'keydown', stray, { key: 'Enter' }); } catch (e) { threw = true; }
check(!threw && calls.length === 0, 'an unmapped target does nothing');

console.log(passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
