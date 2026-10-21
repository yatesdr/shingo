// Unit tests for the dashboard primitives in /static/shared/utils.js —
// reconcileList, createStore, and the onSSE event bus (mission-telemetry
// plan §6). Run via the Go wrapper dashboard_test.go. Exit 0 on pass, 1 on
// any assertion failure.
//
// Self-contained: brings its own linked-list DOM stub (so insertBefore /
// firstChild / nextSibling ordering is exercised the way reconcileList uses
// it) and an EventSource stub for the bus.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0;
let failed = 0;
function assert(cond, label) {
    if (cond) { passed++; }
    else { failed++; console.error('FAIL: ' + label); }
}

// ─── DOM stub with real linked-list ordering semantics ────────────────────
function elem(label) {
    const e = {
        label: label || '',
        children: [],
        parentNode: null,
        dataset: {},
    };
    Object.defineProperty(e, 'firstChild', {
        get() { return e.children[0] || null; },
    });
    Object.defineProperty(e, 'nextSibling', {
        get() {
            if (!e.parentNode) return null;
            const i = e.parentNode.children.indexOf(e);
            return (i >= 0 && i + 1 < e.parentNode.children.length)
                ? e.parentNode.children[i + 1] : null;
        },
    });
    e.insertBefore = (node, ref) => {
        if (node.parentNode) {
            const oi = node.parentNode.children.indexOf(node);
            if (oi >= 0) node.parentNode.children.splice(oi, 1);
        }
        node.parentNode = e;
        if (ref === null || ref === undefined) {
            e.children.push(node);
        } else {
            const ri = e.children.indexOf(ref);
            if (ri < 0) e.children.push(node);
            else e.children.splice(ri, 0, node);
        }
        return node;
    };
    e.appendChild = (c) => e.insertBefore(c, null);
    e.removeChild = (c) => {
        const i = e.children.indexOf(c);
        if (i >= 0) { e.children.splice(i, 1); c.parentNode = null; }
        return c;
    };
    return e;
}

function labels(container) {
    return container.children.map((c) => c.label).join(',');
}

// ─── EventSource stub ─────────────────────────────────────────────────────
let lastES = null;
function ESStub(url) {
    this.url = url;
    this.listeners = {};
    this.closed = false;
    this.onerror = null;
    this.addEventListener = (type, fn) => {
        (this.listeners[type] = this.listeners[type] || []).push(fn);
    };
    this.close = () => { this.closed = true; };
    this.fire = (type, dataObj, id) => {
        (this.listeners[type] || []).forEach((fn) => fn({ data: JSON.stringify(dataObj), lastEventId: id || '' }));
    };
    lastES = this;
}

function loadUtils() {
    const src = fs.readFileSync(path.join(__dirname, 'utils.js'), 'utf8');
    const transformed = src
        .replace(/export\s+function\s+/g, 'function ')
        .replace(/export\s+const\s+/g, 'const ');
    const ctx = vm.createContext({
        document: {
            createElement: (tag) => elem(tag),
            createTextNode: (t) => { const n = elem('#text'); n.textContent = t; return n; },
            getElementById: () => null,
            querySelectorAll: () => [],
            addEventListener: () => {},
            body: { appendChild: () => {} },
            readyState: 'complete',
        },
        window: { addEventListener: () => {} },
        console,
        setTimeout, clearTimeout,
        Promise, Map, Set, JSON, Array, Object,
        EventSource: ESStub,
    });
    vm.runInContext(
        transformed +
        '; this.reconcileList = reconcileList;' +
        '  this.createStore = createStore;' +
        '  this.onSSE = onSSE;' +
        '  this.setSSEQuery = typeof setSSEQuery === "function" ? setSSEQuery : function () {};' +
        '  this.setSSEWatchdog = typeof setSSEWatchdog === "function" ? setSSEWatchdog : function () {};' +
        '  this.closeSSEBus = closeSSEBus;',
        ctx);
    return ctx;
}

// ─── createStore ──────────────────────────────────────────────────────────
{
    const ctx = loadUtils();
    const store = ctx.createStore({ range: 'today', station: '' });
    assert(store.get().range === 'today', 'createStore: get returns initial state');

    let seen = null, calls = 0;
    const off = store.subscribe((s) => { seen = s; calls++; });
    store.set({ station: 'STN-A' });
    assert(seen && seen.station === 'STN-A' && seen.range === 'today',
        'createStore: set shallow-merges and notifies');
    assert(calls === 1, 'createStore: one notification per set');

    let other = 0;
    store.subscribe(() => { other++; });
    store.set({ range: '7d' });
    assert(calls === 2 && other === 1, 'createStore: all subscribers notified');

    off();
    store.set({ station: 'STN-B' });
    assert(calls === 2, 'createStore: unsubscribe stops notifications');
    assert(store.get().station === 'STN-B', 'createStore: state still updates after unsubscribe');
}

// ─── reconcileList ────────────────────────────────────────────────────────
{
    const ctx = loadUtils();
    const container = elem('grid');
    const created = [];
    const destroyed = [];
    const opts = () => ({
        key: (it) => it.id,
        create: (it) => { const n = elem('tile-' + it.id); n.dataset.v = String(it.v); created.push(it.id); return n; },
        update: (n, it) => { n.dataset.v = String(it.v); },
        destroy: (n, k) => { destroyed.push(k); },
    });

    ctx.reconcileList(container, [{ id: 'a', v: 1 }, { id: 'b', v: 2 }], opts());
    assert(labels(container) === 'tile-a,tile-b', 'reconcileList: creates in order a,b');
    assert(created.join(',') === 'a,b', 'reconcileList: create called for a,b');

    ctx.reconcileList(container, [{ id: 'b', v: 9 }, { id: 'c', v: 3 }, { id: 'a', v: 1 }], opts());
    assert(labels(container) === 'tile-b,tile-c,tile-a', 'reconcileList: reorders to b,c,a');
    assert(container.children[0].dataset.v === '9', 'reconcileList: b updated in place to 9');
    assert(created.join(',') === 'a,b,c', 'reconcileList: only c newly created');

    ctx.reconcileList(container, [{ id: 'c', v: 3 }, { id: 'a', v: 1 }], opts());
    assert(labels(container) === 'tile-c,tile-a', 'reconcileList: stale b removed');
    assert(destroyed.indexOf('b') >= 0, 'reconcileList: destroy called for removed b');

    ctx.reconcileList(container, [], opts());
    assert(container.children.length === 0, 'reconcileList: empty items clears the list');
}

// ─── reconcileList nodeKey adoption ───────────────────────────────────────
{
    const ctx = loadUtils();
    const c2 = elem('grid2');
    const pre = elem('pre-x'); pre.dataset.k = 'x'; c2.appendChild(pre);
    let createdCount = 0;
    ctx.reconcileList(c2, [{ id: 'x' }], {
        key: (it) => it.id,
        create: () => { createdCount++; return elem('new'); },
        update: (n) => { n.dataset.updated = '1'; },
        nodeKey: (node) => node.dataset.k,
    });
    assert(createdCount === 0, 'reconcileList: nodeKey adopts server-rendered child (no create)');
    assert(c2.children.length === 1 && c2.children[0] === pre, 'reconcileList: adopted node reused');
    assert(pre.dataset.updated === '1', 'reconcileList: adopted node updated in place');
}

// ─── onSSE bus (async — connect is deferred to a microtask) ───────────────
(async () => {
    const ctx = loadUtils();
    const flush = () => new Promise((r) => setTimeout(r, 0));

    const a = [], b = [];
    const offA = ctx.onSSE('robot-update', (d) => a.push(d.n));
    ctx.onSSE('robot-update', (d) => b.push(d.n)); // same type — the clobber bug onSSE fixes
    ctx.onSSE('order-update', () => {});
    await flush();

    assert(lastES !== null, 'onSSE: opens a single EventSource');
    assert(lastES.url.indexOf('topics=') >= 0, 'onSSE: requests a topic filter');
    assert(lastES.url.indexOf('order-update') >= 0 && lastES.url.indexOf('robot-update') >= 0,
        'onSSE: topic filter lists every subscribed type');

    lastES.fire('connected', { build: 'b1' });
    lastES.fire('robot-update', { n: 1 });
    assert(a.length === 1 && b.length === 1,
        'onSSE: two subscribers to the same type both fire (no clobber)');

    offA();
    lastES.fire('robot-update', { n: 2 });
    assert(a.length === 1 && b.length === 2, 'onSSE: unsubscribe removes only that handler');

    // synthetic connected fan-out for snapshot re-fetch on reconnect (§13)
    let connects = 0;
    ctx.onSSE('connected', () => { connects++; });
    await flush();
    lastES.fire('connected', { build: 'b1' });
    assert(connects >= 1, 'onSSE: dispatches synthetic connected for snapshot re-fetch');

    ctx.closeSSEBus();
})().then(async () => {
    // ─── the numbered stream: query, stream frames, the gap, the watchdog ───
    const ctx = loadUtils();
    const flush = () => new Promise((r) => setTimeout(r, 0));
    const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
    ctx.setSSEQuery({ station: 7 });
    ctx.setSSEWatchdog(60);
    let gaps = 0, beats = 0, drops = 0;
    ctx.onSSE('bins', () => {});
    ctx.onSSE('stream-gap', () => { gaps++; });
    ctx.onSSE('heartbeat', () => { beats++; });
    ctx.onSSE('disconnected', () => { drops++; });
    await flush();
    const es = lastES;
    assert(es.url === '/events?topics=bins&station=7',
        'onSSE: the stream frames are no topic, and the query rides the URL (' + es.url + ')');

    es.fire('connected', { build: 'b1', seq: 10 });
    es.fire('bins', { nodes: [] }, '11');
    es.fire('heartbeat', { build: 'b1', seq: 11 });
    assert(gaps === 0 && beats === 1, 'onSSE: a heartbeat at the last id seen is no gap, and reaches subscribers');
    es.fire('heartbeat', { build: 'b1', seq: 13 });
    assert(gaps === 1, 'onSSE: a heartbeat ahead of the last id seen dispatches stream-gap');
    es.fire('heartbeat', { build: 'b1', seq: 13 });
    assert(gaps === 1, 'onSSE: the gap is reported once');
    es.fire('heartbeat', { build: 'b1' });
    assert(gaps === 1, 'onSSE: a heartbeat with no seq (Core) is never a gap');

    await sleep(30);
    es.fire('heartbeat', { build: 'b1', seq: 13 });
    await sleep(40);
    assert(drops === 0 && !es.closed, 'onSSE: a frame inside the watchdog keeps the stream');
    await sleep(60);
    assert(drops === 1 && es.closed, 'onSSE: no frame for the watchdog\'s time: closed and disconnected dispatched');
    ctx.setSSEWatchdog(0); // the reopened stream gets no frames here; keep it
    await sleep(1050);
    assert(lastES !== es && !lastES.closed, 'onSSE: the watchdog\'s drop reconnects after the backoff');
    ctx.closeSSEBus();
}).then(() => {
    console.log('dashboard primitives: ' + passed + ' passed, ' + failed + ' failed');
    process.exit(failed ? 1 : 0);
}).catch((e) => {
    console.error('dashboard test crashed:', e);
    process.exit(1);
});
