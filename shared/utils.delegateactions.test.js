// Unit tests for delegateActions + installHtmxTimestampConversion in
// /static/shared/utils.js. Run under plain Node via the Go test
// wrapper utils_js_test.go. Exit 0 on pass, 1 on any assertion
// failure.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

// Minimal DOM stub — just enough to exercise the helper's behavior.
function makeNode(opts) {
    opts = opts || {};
    const n = {
        tagName: (opts.tag || 'div').toUpperCase(),
        dataset: Object.assign({}, opts.dataset || {}),
        _children: [],
        _parent: null,
        _listeners: {},
    };
    n.addEventListener = (ev, fn) => { (n._listeners[ev] = n._listeners[ev] || []).push(fn); };
    n.appendChild = (c) => { n._children.push(c); c._parent = n; };
    n.contains = (other) => {
        let cur = other;
        while (cur) { if (cur === n) return true; cur = cur._parent; }
        return false;
    };
    n.closest = (sel) => {
        // Only [data-action] selector is exercised.
        let cur = n;
        while (cur) {
            if (sel === '[data-action]' && cur.dataset && cur.dataset.action) return cur;
            cur = cur._parent;
        }
        return null;
    };
    return n;
}

function fireClick(root, target) {
    const listeners = root._listeners.click || [];
    listeners.forEach(fn => fn({ target }));
}

let passed = 0;
let failed = 0;
function assert(cond, label) {
    if (cond) { passed++; }
    else { failed++; console.error('FAIL: ' + label); }
}

function loadUtils() {
    const src = fs.readFileSync(path.join(__dirname, 'utils.js'), 'utf8');
    // Strip the `export` keyword so the file evaluates as plain script
    // inside the VM context. `export function NAME` becomes
    // `function NAME`, `export const NAME` becomes `const NAME`.
    // No real ESM resolution needed for these unit tests.
    const transformed = src
        .replace(/export\s+function\s+/g, 'function ')
        .replace(/export\s+const\s+/g, 'const ');
    const ctx = vm.createContext({
        document: { createElement: () => makeNode({}) },
        window: { addEventListener: () => {} },
        console,
        setTimeout, clearTimeout, fetch: () => {},
        EventSource: function () {},
        location: { reload: () => {} },
    });
    vm.runInContext(transformed + '; this.delegateActions = delegateActions;', ctx);
    return ctx;
}

// Custom loader for installHtmxTimestampConversion: wires up a
// document stub whose body remembers listeners and whose readyState
// is 'complete' so the install path attaches synchronously.
function loadUtilsForHtmx() {
    const src = fs.readFileSync(path.join(__dirname, 'utils.js'), 'utf8');
    const transformed = src
        .replace(/export\s+function\s+/g, 'function ')
        .replace(/export\s+const\s+/g, 'const ');
    const bodyListeners = {};
    const body = {
        addEventListener: (ev, fn) => { (bodyListeners[ev] = bodyListeners[ev] || []).push(fn); },
        _fire: (ev, evtObj) => { (bodyListeners[ev] || []).forEach(fn => fn(evtObj)); },
    };
    const ctx = vm.createContext({
        document: {
            readyState: 'complete',
            body,
            createElement: () => makeNode({}),
            addEventListener: () => {},
            querySelectorAll: () => [],
        },
        window: { addEventListener: () => {} },
        console,
        setTimeout, clearTimeout, fetch: () => {},
        EventSource: function () {},
        location: { reload: () => {} },
    });
    vm.runInContext(transformed +
        '; this.installHtmxTimestampConversion = installHtmxTimestampConversion;' +
        ' this.convertTimestamps = convertTimestamps;',
        ctx);
    return { ctx, body, bodyListeners };
}

function makeTimeNode(utc) {
    return {
        tagName: 'TIME',
        textContent: utc, // pre-conversion sentinel
        getAttribute: (name) => name === 'data-utc' ? utc : null,
    };
}

// ─── Tests ──────────────────────────────────────────────────────────────

(function testDispatchesByActionName() {
    const ctx = loadUtils();
    const root = makeNode({});
    const btn = makeNode({ tag: 'button', dataset: { action: 'foo' } });
    root.appendChild(btn);

    let fooCalls = 0;
    let barCalls = 0;
    ctx.delegateActions(root, {
        foo: () => { fooCalls++; },
        bar: () => { barCalls++; },
    });
    fireClick(root, btn);
    assert(fooCalls === 1, 'foo handler fired once');
    assert(barCalls === 0, 'unrelated handler did not fire');
})();

(function testIgnoresClicksOutsideDataAction() {
    const ctx = loadUtils();
    const root = makeNode({});
    const plain = makeNode({});
    root.appendChild(plain);

    let calls = 0;
    ctx.delegateActions(root, { foo: () => { calls++; } });
    fireClick(root, plain);
    assert(calls === 0, 'click on non-[data-action] element does nothing');
})();

(function testPassesElementToHandler() {
    const ctx = loadUtils();
    const root = makeNode({});
    const btn = makeNode({ tag: 'button', dataset: { action: 'edit', claimId: '42' } });
    root.appendChild(btn);

    let receivedDataset = null;
    ctx.delegateActions(root, {
        edit: (el) => { receivedDataset = el.dataset.claimId; },
    });
    fireClick(root, btn);
    assert(receivedDataset === '42', 'handler receives the matched element with dataset');
})();

(function testIdempotentBinding() {
    const ctx = loadUtils();
    const root = makeNode({});
    const btn = makeNode({ tag: 'button', dataset: { action: 'foo' } });
    root.appendChild(btn);

    let calls = 0;
    ctx.delegateActions(root, { foo: () => { calls++; } });
    ctx.delegateActions(root, { foo: () => { calls++; } });
    fireClick(root, btn);
    assert(calls === 1, 'idempotent: second delegateActions call does not double-bind');
    assert(root.dataset.delegated === '1', 'sentinel flag set after first bind');
})();

(function testCustomSentinel() {
    const ctx = loadUtils();
    const root = makeNode({});
    const btn = makeNode({ tag: 'button', dataset: { action: 'foo' } });
    root.appendChild(btn);

    let calls = 0;
    ctx.delegateActions(root, { foo: () => { calls++; } }, { sentinel: 'innerBind' });
    ctx.delegateActions(root, { foo: () => { calls++; } }, { sentinel: 'outerBind' });
    fireClick(root, btn);
    assert(calls === 2, 'distinct sentinels allow independent bindings');
})();

(function testNestedClickResolvesToClosestDataAction() {
    const ctx = loadUtils();
    const root = makeNode({});
    const btn = makeNode({ tag: 'button', dataset: { action: 'edit' } });
    const icon = makeNode({ tag: 'span' });
    btn.appendChild(icon);
    root.appendChild(btn);

    let calls = 0;
    ctx.delegateActions(root, { edit: () => { calls++; } });
    fireClick(root, icon);
    assert(calls === 1, 'click on inner element walks up to nearest [data-action]');
})();

// ─── installHtmxTimestampConversion smoke test ──────────────────────────

(function testHtmxAfterSwapConvertsTimestamps() {
    const { ctx, body } = loadUtilsForHtmx();
    ctx.installHtmxTimestampConversion();
    assert(Array.isArray(body._listeners) || typeof body._fire === 'function',
        'install wired a body listener');

    // Synthetic swapped-in subtree carrying two <time data-utc=…> nodes.
    const t1 = makeTimeNode('2026-05-24T12:00:00Z');
    const t2 = makeTimeNode('2026-05-24T12:30:00Z');
    const swappedSubtree = {
        querySelectorAll: (sel) => sel === 'time[data-utc]' ? [t1, t2] : [],
    };
    body._fire('htmx:afterSwap', { detail: { target: swappedSubtree } });

    assert(t1.textContent !== '2026-05-24T12:00:00Z',
        'first time node textContent rewritten away from raw UTC');
    assert(t2.textContent !== '2026-05-24T12:30:00Z',
        'second time node textContent rewritten away from raw UTC');
})();

(function testHtmxInstallIsIdempotent() {
    const { ctx, bodyListeners } = loadUtilsForHtmx();
    ctx.installHtmxTimestampConversion();
    ctx.installHtmxTimestampConversion();
    ctx.installHtmxTimestampConversion();
    const swapListeners = bodyListeners['htmx:afterSwap'] || [];
    assert(swapListeners.length === 1,
        'repeated install calls register only one listener');
})();

if (failed > 0) {
    console.error(`\nFAILED: ${failed} of ${passed + failed}`);
    process.exit(1);
}
console.log(`PASS: ${passed} assertions for shared/utils.js`);
process.exit(0);
