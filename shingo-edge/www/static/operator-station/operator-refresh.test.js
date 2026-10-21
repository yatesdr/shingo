// The station's refresh: which events rebuild this station's view, how often,
// and what a build costs the screen.
//
// Loads the SHIPPING modules whole, operator.js last, into one vm with a small
// fake DOM, a fake clock (every timer the station arms runs on it) and a fake
// event stream, as operator-safe-taps.test.js loads the modules. The view
// fetch is answered from `served`, held for `buildMs` of the fake clock when a
// test needs a build in flight.
//
// Runs under plain Node (no npm). Exit 0 = pass, 1 = any failure. Run via the
// Go wrapper operator_refresh_test.go.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
let finished = false;
process.on('exit', function (code) {
    if (!finished && code === 0) { console.error('FAIL: the test did not reach its end'); process.exitCode = 1; }
});
function eq(got, want, label) {
    const g = JSON.stringify(got), w = JSON.stringify(want);
    if (g === w) { passed++; return; }
    failed++;
    console.error('FAIL: ' + label + '\n   got:  ' + g + '\n   want: ' + w);
}

// ── a small fake DOM ─────────────────────────────────────────────────────────

class FakeStyle {
    constructor() { this.props = {}; this.cssText = ''; }
    setProperty(k, v) { this.props[k] = v; }
    removeProperty(k) { delete this.props[k]; }
}

class FakeEl {
    constructor(tag) {
        this.tagName = String(tag).toUpperCase();
        this.children = [];
        this.parentNode = null;
        this.listeners = {};
        this.dataset = {};
        this.attrs = {};
        this.className = '';
        this.disabled = false;
        this._text = '';
        this._html = null;
        this._style = new FakeStyle();
        const self = this;
        this.classList = {
            add(c) { const l = self.className.split(/\s+/).filter(Boolean); if (l.indexOf(c) < 0) l.push(c); self.className = l.join(' '); },
            remove() {
                const drop = Array.prototype.slice.call(arguments);
                self.className = self.className.split(/\s+/).filter(x => x && drop.indexOf(x) < 0).join(' ');
            },
            contains(c) { return self.className.split(/\s+/).indexOf(c) >= 0; },
            toggle(c, on) { if (on === undefined ? !this.contains(c) : on) this.add(c); else this.remove(c); },
        };
    }
    get style() { return this._style; }
    set style(v) { this._style.cssText = String(v); }
    get textContent() {
        if (this.children.length === 0) return this._text;
        return this._text + this.children.map(c => c.textContent).join('');
    }
    set textContent(v) { this._text = v == null ? '' : String(v); this._html = null; this.children = []; }
    get innerHTML() { return this._html != null ? this._html : this._text; }
    set innerHTML(v) { this._html = String(v); this._text = ''; this.children = []; }
    get firstChild() { return this.children[0] || null; }
    appendChild(c) {
        if (c.parentNode) c.remove();
        c.parentNode = this; this.children.push(c); return c;
    }
    insertBefore(c, ref) {
        if (c.parentNode) c.remove();
        c.parentNode = this;
        const i = this.children.indexOf(ref);
        if (i < 0) this.children.push(c); else this.children.splice(i, 0, c);
        return c;
    }
    remove() {
        if (this.parentNode) {
            const i = this.parentNode.children.indexOf(this);
            if (i >= 0) this.parentNode.children.splice(i, 1);
        }
        this.parentNode = null;
        this.removed = true;
    }
    setAttribute(k, v) { this.attrs[k] = v; }
    getAttribute(k) { return this.attrs[k]; }
    addEventListener(ev, fn) { (this.listeners[ev] = this.listeners[ev] || []).push(fn); }
    click(target) {
        const evt = { target: target || this, currentTarget: this, stopPropagation() {}, preventDefault() {} };
        return Promise.all((this.listeners.click || []).map(fn => fn.call(this, evt)));
    }
    descendants() {
        const out = [];
        (function walk(n) { n.children.forEach(c => { out.push(c); walk(c); }); })(this);
        return out;
    }
    querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
    querySelectorAll(sel) {
        if (sel[0] === '.') return this.descendants().filter(n => n.classList.contains(sel.slice(1)));
        if (sel[0] === '#') return this.descendants().filter(n => n.id === sel.slice(1));
        return [];
    }
}

const byID = {};
const body = new FakeEl('body');
body.dataset.stationId = '7';
const document = {
    body: body,
    readyState: 'complete',
    getElementById(id) {
        const live = body.descendants().find(n => n.id === id);
        if (live) return live;
        if (/^os-/.test(id) && id !== 'os-postcutover-flag' && id !== 'os-alerts') {
            if (!byID[id]) { byID[id] = new FakeEl('div'); byID[id].id = id; }
            return byID[id];
        }
        if (id === 'node-modal' || id === 'node-modal-content' || /^load-bin|^keypad/.test(id)) {
            if (!byID[id]) { byID[id] = new FakeEl('div'); byID[id].id = id; }
            return byID[id];
        }
        return null;
    },
    createElement(tag) { return new FakeEl(tag); },
    createTextNode(t) { const n = new FakeEl('#text'); n._text = String(t); return n; },
    querySelector(sel) { return body.querySelector(sel); },
    querySelectorAll(sel) { return body.querySelectorAll(sel); },
    visibilityState: 'visible',
    listeners: {},
    addEventListener(ev, fn) { (this.listeners[ev] = this.listeners[ev] || []).push(fn); },
};
// becomeVisible: the screen comes back into view, as the browser says it.
function becomeVisible() {
    document.visibilityState = 'visible';
    (document.listeners.visibilitychange || []).forEach(fn => fn({}));
}

// ── the fake clock ───────────────────────────────────────────────────────────

let now = Date.UTC(2026, 9, 17, 12, 0, 0);
let timers = [];
let timerSeq = 0;
function fakeSetTimeout(fn, ms) {
    const t = { id: ++timerSeq, at: now + Math.max(0, ms || 0), fn: fn };
    timers.push(t);
    return t.id;
}
function fakeClearTimeout(id) { timers = timers.filter(t => t.id !== id); }
function fakeSetInterval(fn, ms) {
    const again = function () { tagInterval(fakeSetTimeout(again, ms)); fn(); };
    return tagInterval(fakeSetTimeout(again, ms));
}
function tagInterval(id) {
    timers.forEach(t => { if (t.id === id) t.interval = true; });
    return id;
}
// nextTick is when the next interval timer (the station's minute tick) fires.
function nextTick() {
    const t = timers.filter(x => x.interval).sort((a, b) => a.at - b.at)[0];
    return t ? t.at : null;
}
function flush() {
    let p = Promise.resolve();
    for (let i = 0; i < 6; i++) p = p.then(() => new Promise(r => setImmediate(r)));
    return p;
}
// advance runs every timer due in the next ms, in order, letting the promises
// each one starts settle before the next.
async function advance(ms) {
    const end = now + ms;
    await flush();
    for (;;) {
        timers.sort((a, b) => a.at - b.at || a.id - b.id);
        const t = timers[0];
        if (!t || t.at > end) break;
        timers.shift();
        now = t.at;
        t.fn();
        await flush();
    }
    now = end;
    await flush();
}
class FakeDate extends Date {
    constructor() {
        if (arguments.length === 0) super(now);
        else super(...arguments);
    }
    static now() { return now; }
}

// ── the network ──────────────────────────────────────────────────────────────

const VIEW_URL = '/api/operator-stations/7/view';
let served = null;        // the view the station's GET answers with
let buildMs = 0;          // how long a view GET takes, on the fake clock
let viewGets = [];        // the fake-clock time each view GET started
let requests = [];
// conditional: the server sends an ETag and answers a matching If-None-Match
// with 304, as /view does.
let conditional = true;
// cutoverFlag: what GET .../post-cutover-flag answers (null: not flagged);
// flagFails and postFails: the next answers fail.
let cutoverFlag = null;
let flagFails = 0;
let viewFails = 0;
let postFails = 0;
let notModified = 0;
function etagOf(text) {
    let h = 0;
    for (let i = 0; i < text.length; i++) h = (h * 31 + text.charCodeAt(i)) >>> 0;
    return 'W/"' + h.toString(16) + '"';
}
function response(status, text, headers) {
    const res = {
        ok: status >= 200 && status < 300, status: status,
        headers: { get(k) { return (headers || {})[String(k).toLowerCase()] || null; } },
        json: async () => JSON.parse(text),
        text: async () => text,
        clone() { return res; },
    };
    return res;
}
function fetchStub(url, opts) {
    opts = opts || {};
    const method = opts.method || 'GET';
    requests.push({ method: method, url: url, at: now, headers: opts.headers || {},
        body: opts.body === undefined ? undefined : JSON.parse(opts.body) });
    if (url === VIEW_URL) {
        viewGets.push(now);
        if (viewFails > 0) { viewFails--; return Promise.resolve(response(502, 'bad gateway')); }
        const text = JSON.stringify(served);
        const etag = etagOf(text);
        const quoted = opts.headers && opts.headers['If-None-Match'];
        const res = conditional && quoted === etag ? response(304, '', { etag: etag })
            : response(200, text, conditional ? { etag: etag } : {});
        if (res.status === 304) notModified++;
        if (buildMs <= 0) return Promise.resolve(res);
        return new Promise(resolve => fakeSetTimeout(() => resolve(res), buildMs));
    }
    if (/post-cutover-flag$/.test(url)) {
        if (flagFails > 0) { flagFails--; return Promise.resolve(response(500, 'oops')); }
        return Promise.resolve(response(200, JSON.stringify(cutoverFlag ? { flagged: true, flag: cutoverFlag } : { flagged: false })));
    }
    if (method !== 'GET' && postFails > 0) { postFails--; return Promise.resolve(response(409, '{"error":"refused"}')); }
    return Promise.resolve(response(200, '{}'));
}
function posts() { return requests.filter(r => r.method !== 'GET').map(r => r.url); }

// ── the event stream ─────────────────────────────────────────────────────────
//
// A fake EventSource: every stream the station opens is kept; fire() delivers
// an event on the newest open one, as the Edge's /events would.

const streams = [];
class FakeEventSource {
    constructor(url) {
        this.url = url;
        this.listeners = {};
        this.closed = false;
        this.onerror = null;
        streams.push(this);
    }
    addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); }
    close() { this.closed = true; }
    emit(type, data, id) {
        (this.listeners[type] || []).forEach(fn => fn({ type: type, data: JSON.stringify(data), lastEventId: id || '' }));
    }
    fail() { if (this.onerror) this.onerror(); }
}
function openStream() { return streams.filter(s => !s.closed).slice(-1)[0] || null; }
function fire(type, data) {
    const s = openStream();
    if (s) s.emit(type, data);
}
let reloads = 0;

// ── load the shipping modules ────────────────────────────────────────────────

const toasts = [];
let composerOpens = 0;
const ctx = vm.createContext({
    console, JSON, Math, Number, Array, Object, String, Promise, Error, RegExp, Map, Set,
    parseInt, isNaN, encodeURIComponent, decodeURIComponent, AbortController,
    Date: FakeDate,
    setTimeout: fakeSetTimeout,
    clearTimeout: fakeClearTimeout,
    setInterval: fakeSetInterval,
    document: document,
    window: {
        innerWidth: 1280, innerHeight: 800, addEventListener() {},
    },
    EventSource: FakeEventSource,
    location: { reload() { reloads++; } },
    fetch: fetchStub,
    serverNow: () => now,
    formatClock: (iso) => String(iso),
    mountFlowPanel() {}, openFlowPanel() {}, syncFlowPanel() {},
    ensureComposer: async () => ({ openPicker() { composerOpens++; } }),
    openKeypad() {},
});
ctx.globalThis = ctx;

function loadModule(name) {
    let src = fs.readFileSync(path.join(__dirname, name), 'utf8');
    src = src.replace(/^import\s[\s\S]*?from\s+['"][^'"]+['"];?[ \t]*$/gm, '');
    const exported = [];
    src = src.replace(/^export\s+(async\s+function|function|const|let)\s+(\w+)/gm, function (_, kw, n) {
        exported.push(n);
        return kw + ' ' + n;
    });
    src = src.replace(/^export\s*\{[^}]*\};?/gm, '');
    const fns = [];
    src.replace(/^(?:async\s+)?function\s+(\w+)/gm, function (_, n) { fns.push(n); return _; });
    const wrapped = '(function () {\n' + src + '\nreturn { exported: {' +
        exported.map(n => n + ': ' + n).join(', ') + '}, fns: {' +
        fns.map(n => n + ': ' + n).join(', ') + '} };\n})()';
    const out = vm.runInContext(wrapped, ctx, { filename: name });
    Object.assign(ctx, out.exported);
    return out.fns;
}

// ── the station's view ───────────────────────────────────────────────────────

// A press station: process 1, two tiles (process nodes 1 and 2), an order 501
// on tile 1, and a served row of another process (process 2, process node 30).
function pressView() {
    return {
        station: { id: 7, name: 'Press station', health_status: 'online' },
        process: { id: 1, name: 'PRESS-1', active_style_id: 11, target_style_id: 11 },
        current_style: { id: 11, name: 'STYLE-1' },
        nodes: [1, 2].map(id => ({
            node: { id: id, process_id: 1, core_node_name: 'PLN_' + id, name: 'PLN_' + id },
            runtime: { remaining_uop_cached: 5, active_bin_id: 1 },
            active_claim: { id: 40 + id, core_node_name: 'PLN_' + id, role: 'consume', swap_mode: 'single_robot',
                payload_code: 'PART-A', uop_capacity: 20 },
            orders: id === 1 ? [{ id: 501, uuid: 'u-501', order_type: 'retrieve', status: 'in_transit',
                payload_code: 'PART-A', delivery_node: 'PLN_1', created_at: '2026-10-17T11:00:00Z' }] : [],
            bin_state: { occupied: true, payload_code: 'PART-A', stocked: true, bin_label: 'BIN-' + id },
        })),
        serves: [{ cell: 'PRESS-2', part: 'PART-A', role: 'consume', carrier: 'TOTE-1', have_uop: 4, capacity_uop: 10,
            incoming: false, node: 'PLN_30', process_id: 2, process_node_id: 30, reorder_uop: 0, waiting: false }],
        bins: { known: true },
    };
}

served = pressView();
// The shared bus the station takes its events from, first, so a station
// module's own export of a name wins as its import does in the page; the
// fake clock's server time stays the station's clock.
loadModule('../../../../shared/utils.js');
ctx.serverNow = () => now;
ctx.formatClock = (iso) => String(iso);
['../../../../shared/esc.js', 'order-status.js', 'operator-util.js', 'operator-state.js', 'operator-window-state.js',
    'operator-board-model.js', 'operator-board.js', 'operator-supply-refusal.js', 'operator-load-bin.js',
    'operator-release.js', 'operator-modal.js', 'operator-render.js',
].forEach(loadModule);
const showToastShipped = ctx.showToast;
const OP = loadModule('operator.js');
// Toasts are counted where operator.js makes them (its module scope bound the
// shipping showToast; the toast container records what reached it).
const toastBox = document.getElementById('os-toast');
function toastTexts() { return toastBox.children.map(c => c.textContent); }
void showToastShipped; void toasts;

// gets counts the view GETs over the next ms of the fake clock.
async function getsOver(ms) {
    const before = viewGets.length;
    await advance(ms);
    return viewGets.length - before;
}
// keepAlive is the server's heartbeat with no seq: it feeds the station's
// stream watchdog and says nothing about lost frames.
function keepAlive() { fire('heartbeat', {}); }
// idle advances ms of a quiet board whose stream is healthy: a heartbeat every
// 30 s, as the Edge sends them.
async function idle(ms) {
    const end = now + ms;
    while (now + 30000 <= end) { await advance(30000); keepAlive(); }
    await advance(end - now);
}
// settle hands the station the view in `served` (the nudge every station takes
// after a write), lets every timer run out, and clears the counters.
async function settle() {
    keepAlive();
    fire('material-refresh', { action: 'nudge' });
    await advance(5000);
    viewGets = []; requests = []; toastBox.children = [];
}

// ═════════════════════════════════════════════════════════════════════════════
(async function main() {

await settle();
eq(streams.length, 1, 'the station opens one event stream');
eq(['order-update', 'bins', 'supply-refusal', 'sourcing', 'containment', 'material-refresh', 'core-nodes']
    .every(t => (openStream().listeners[t] || []).length === 1), true,
    'every type the station takes is on that one stream, Core\'s node list and the nudge included');
{
    const q = openStream().url.split('?')[1] || '';
    const params = {};
    q.split('&').forEach(kv => { const p = kv.split('='); params[p[0]] = decodeURIComponent(p[1] || ''); });
    eq(params.station, '7', 'the stream names its station (the Edge marks the screen online on its connect and heartbeat)');
    eq(params.topics.split(',').indexOf('core-nodes') >= 0 && params.topics.split(',').indexOf('heartbeat') < 0, true,
        'the stream asks for the types it takes, and none of the stream\'s own frames');
    // The first connect fetches too (a bug fixed: only a reconnect did, so what
    // changed between the page's first fetch and the stream's connect was
    // announced to nobody).
    viewGets = [];
    openStream().emit('connected', { build: 'build-A', seq: 0 });
    eq(await getsOver(600), 1, 'the stream\'s first connect fetches the view');
    await settle();
}

// ── which events rebuild this station ────────────────────────────────────────
//
// Each case: an event, then whether this station fetches its view within the
// debounce (one build) or not at all. The station draws process 1, process
// nodes 1 and 2 (PLN_1, PLN_2), order 501 on PLN_1, and a served row of process
// 2 at process node 30.
{
    const cases = [
        ['order-update', { order_id: 9, process_node_id: 1 }, 1, 'an order at one of its tiles'],
        ['order-update', { order_id: 9, process_node_id: 30 }, 1, 'an order at a served row\'s process node'],
        ['order-update', { order_id: 501, process_node_id: 1 }, 1, 'an order on one of its tiles'],
        ['order-update', { order_id: 9, process_node_id: 77 }, 0, 'an order at another station\'s node'],
        ['order-update', { order_id: 9 }, 1, 'an order naming no process node (every station)'],
        ['counter-update', { process_id: 1, delta: 1 }, 1, 'a count of its process'],
        ['counter-update', { process_id: 2, delta: 1 }, 1, 'a count of a served cell\'s process'],
        ['counter-update', { process_id: 9, delta: 1 }, 0, 'a count of another process'],
        ['counter-update', { process_node_id: 2, core_node_name: 'PLN_2' }, 1, 'an adjustment at one of its tiles'],
        ['counter-update', { process_node_id: 88, core_node_name: 'X' }, 0, 'an adjustment at another node'],
        ['changeover-update', { action: 'release', process_id: 1 }, 1, 'its own process\'s changeover'],
        ['changeover-update', { action: 'release', process_id: 2 }, 1, 'a served cell\'s changeover'],
        ['changeover-update', { action: 'release', process_id: 9 }, 0, 'another process\'s changeover'],
        ['changeover-verify-mismatch', { process_id: 1 }, 1, 'a part-id flag on its process'],
        ['changeover-verify-mismatch', { process_id: 9 }, 0, 'a part-id flag on another process'],
        ['order-failed', { order_id: 501, reason: 'x' }, 1, 'a failed order on one of its tiles'],
        ['order-failed', { order_id: 9, reason: 'x' }, 0, 'another station\'s failed order'],
        ['order-faulted', { order_id: 501, reason: 'x' }, 1, 'a faulted order on one of its tiles'],
        ['order-faulted', { order_id: 9, reason: 'x' }, 0, 'another station\'s faulted order'],
        ['uop-stranded-alarm', { core_node_name: 'PLN_2', detail: 'counts wait' }, 1, 'a stranded count at one of its tiles'],
        ['uop-stranded-alarm', { core_node_name: 'PLN_9', detail: 'counts wait' }, 0, 'a stranded count elsewhere'],
        ['bins', { nodes: ['PLN_1'], groups: [] }, 1, 'bins at one of its tiles'],
        ['bins', { nodes: ['OTHER'], groups: [] }, 0, 'bins elsewhere'],
        ['material-refresh', { action: 'nudge' }, 1, 'the nudge after a write (every station)'],
        ['core-nodes', { nodes: [] }, 1, 'Core\'s node list applied (every station)'],
        ['containment', { digest: 'd2' }, 1, 'the containment copy changed (every station)'],
        ['supply-refusal', { loader_node: 'PLN_1', payload_code: 'PART-X' }, 1, 'a refusal at one of its nodes'],
        ['supply-refusal', { loader_node: 'SMN_X', payload_code: 'PART-A' }, 1, 'a refusal of a part an order on its tiles is for'],
        ['supply-refusal', { loader_node: 'SMN_X', payload_code: 'PART-X' }, 0, 'a refusal elsewhere'],
        ['supply-refusal', { loader_node: '', payload_code: '' }, 1, 'the refusal snapshot from Core (every station)'],
        ['sourcing', { processes: ['PRESS-1'] }, 1, 'a sourcing report for its process'],
        ['sourcing', { processes: ['PRESS-2'] }, 1, 'a sourcing report for a served cell'],
        ['sourcing', { processes: ['PRESS-9'] }, 0, 'a sourcing report for another process'],
        ['sourcing', { processes: [] }, 1, 'a sourcing snapshot naming nothing (every station)'],
    ];
    for (const c of cases) {
        await settle();
        fire(c[0], c[1]);
        eq(await getsOver(600), c[2], c[0] + ', ' + c[3] + ': view fetches');
    }
    // The toasts follow the same rule.
    await settle();
    fire('order-failed', { order_id: 9, reason: 'no robot' });
    await advance(600);
    eq(toastTexts(), [], 'order-failed for another station\'s order: no toast here');
    await settle();
    fire('order-failed', { order_id: 501, order_type: 'retrieve', reason: 'no robot' });
    await advance(600);
    eq(toastTexts().length, 1, 'order-failed for its own order: the toast');
    await settle();
    fire('uop-stranded-alarm', { core_node_name: 'PLN_9', detail: 'counts wait at PLN_9', needs_action: true });
    await advance(600);
    eq(toastTexts(), [], 'a stranded count at another station\'s node: no toast here');
}
{
    // A loader board: a bins event naming a buffer spot it draws, or its
    // empties group; and on a board that draws empties, any bins event (the
    // group's members are not on the glass).
    const loader = pressView();
    loader.nodes[0].loader = { role: 'produce', layout: 'dedicated_positions', replenishment: 'operator',
        outbound_dest: '', parts: [{ code: 'PART-A' }], leaves_bare: false, pulled_directly: false,
        buffer: [{ node: 'LDR.SMN_F1', state: 'partial' }] };
    served = loader;
    await settle();
    fire('bins', { nodes: ['LDR.SMN_F1'], groups: [] });
    eq(await getsOver(600), 1, 'bins at one of its buffer spots: view fetches');
    const unloader = pressView();
    unloader.empties = { group: 'EMPTY-GRP', rows: [] };
    served = unloader;
    await settle();
    fire('bins', { nodes: [], groups: ['EMPTY-GRP'] });
    eq(await getsOver(600), 1, 'bins naming its empties group: view fetches');
    await settle();
    fire('bins', { nodes: ['EMPTY-GRP.SMN_E4'], groups: [] });
    eq(await getsOver(600), 1, 'bins at a node a board with empties may draw: view fetches');
    served = pressView();
    await settle();
}

// ── the debounce is a cap ────────────────────────────────────────────────────
{
    // A build that takes 400 ms, and an event this station draws every 100 ms
    // for three seconds: event-driven builds start at least 500 ms apart (a
    // bug fixed: an event landing mid-build re-armed at 0 ms, back to back).
    await settle();
    buildMs = 400;
    for (let i = 0; i < 30; i++) {
        fire('order-update', { order_id: 9, process_node_id: 1 });
        await advance(100);
    }
    await advance(1000);
    const gaps = [];
    for (let i = 1; i < viewGets.length; i++) gaps.push(viewGets[i] - viewGets[i - 1]);
    eq(gaps.length >= 4, true, 'steady events still rebuild the board (' + viewGets.length + ' builds)');
    eq(gaps.filter(g => g < 500), [], 'event-driven builds start at least 500 ms apart');

    // A tap's own refresh is not delayed: postAction's loadView starts a build
    // at once on a quiet board.
    buildMs = 0;
    await settle();
    await ctx.postAction('/api/process-nodes/1/clear-bin', {}, OP.loadView);
    eq(viewGets.length, 1, 'a tap\'s refresh starts its build at once');
    eq(viewGets[0] - requests[0].at, 0, 'with no wait after the POST answers');
    await settle();
}

// ── an idle board makes no request ───────────────────────────────────────────
{
    // No timer fetches the view: an hour of a quiet board with a healthy
    // stream (a heartbeat every 30 s) is no request at all.
    const grid = document.getElementById('os-grid');
    served = pressView();
    await settle();
    await idle(3600 * 1000);
    eq([viewGets.length, requests.length], [0, 0], 'an idle hour: no view fetch and no request of any kind');
    eq(streams.filter(s => !s.closed).length, 1, 'and the stream stayed up through it (the watchdog fed by heartbeats)');
    const drawn = grid.children[0];

    // A nudge after a write fetches once, quoting the ETag of the view held;
    // the unchanged view answers 304 and the board draws nothing.
    notModified = 0;
    fire('material-refresh', { action: 'nudge' });
    eq(await getsOver(600), 1, 'a nudge fetches the view once');
    const quoted = requests.filter(r => r.url === VIEW_URL).map(r => !!(r.headers && r.headers['If-None-Match']));
    eq([quoted.every(Boolean), notModified], [true, 1], 'quoting its ETag, answered 304');
    eq(grid.children[0] === drawn, true, 'a 304 draws nothing: the tiles on the glass are the same nodes');

    // The time-relative words move on the minute tick, which fetches nothing.
    const eta = pressView();
    eta.nodes[1].bin_state = { occupied: false };
    eta.nodes[1].orders = [{ id: 502, uuid: 'u-502', order_type: 'retrieve', status: 'in_transit', payload_code: 'PART-A',
        delivery_node: 'PLN_2', eta: new Date(now + 5 * 60000 + 20000).toISOString(), created_at: '2026-10-17T11:00:00Z' }];
    served = eta;
    fire('order-update', { order_id: 502, process_node_id: 2 });
    await advance(600);
    const pill = () => grid.descendants().filter(n => n.classList.contains('os-node-eta-pill')).map(n => n.textContent);
    eq(pill(), ['ETA: ~5 min'], 'the ETA pill as the view arrives');
    const before = viewGets.length;
    // Past the second minute tick from here: the pill says what it says at
    // that tick.
    const tick = nextTick() + 60000;
    await idle(tick - now + 10);
    const left = Math.round((Date.parse(eta.nodes[1].orders[0].eta) - tick) / 60000);
    eq(pill(), ['ETA: ~' + left + ' min'], 'two minute ticks on, the tick has redrawn it from the held view');
    eq(viewGets.length - before, 0, 'and fetched nothing in those two minutes');
    served = pressView();
    await settle();
}

// ── a lost frame, a silent stream, a hidden screen ───────────────────────────
{
    served = pressView();
    await settle();
    // The stream's numbers: frames up to 20 seen; a heartbeat at 20 is no gap.
    openStream().emit('order-update', { order_id: 9, process_node_id: 77 }, '20');
    openStream().emit('heartbeat', { seq: 20 });
    eq(await getsOver(600), 0, 'a heartbeat at the last frame seen fetches nothing');
    // A heartbeat ahead of the last frame seen: one frame never arrived.
    openStream().emit('heartbeat', { seq: 22 });
    eq(await getsOver(600), 1, 'a heartbeat ahead of the last frame seen fetches the view');
    openStream().emit('heartbeat', { seq: 22 });
    eq(await getsOver(600), 0, 'once');

    // No frame for 75 s: the stream is dead though no error fired. The sign
    // says so, the bus reopens it, and the connect fetches.
    await settle();
    const header = document.getElementById('os-header-actions');
    const dot = () => {
        const d = header.descendants().find(n => n.classList.contains('os-health-badge'));
        return d ? d.className.replace('os-health-badge', '').trim() : null;
    };
    const silent = openStream();
    keepAlive();
    await advance(74000);
    eq([silent.closed, dot()], [false, 'online'], 'a stream silent for 74 s is still taken as live');
    await advance(1000);
    eq([silent.closed, dot()], [true, 'offline'], 'silent for 75 s: closed, and the dot offline');
    await advance(1000);
    const reopened = openStream();
    eq(!!reopened && reopened !== silent, true, 'the bus reopens it');
    reopened.emit('connected', { build: 'build-A', seq: 30 });
    eq(await getsOver(600), 1, 'and the connect fetches the view');
    eq(dot(), 'online', 'the dot back online');

    // A screen that comes back into view fetches.
    await settle();
    becomeVisible();
    eq(await getsOver(600), 1, 'becoming visible fetches the view');
    await settle();
}

// ── the event stream: reconnect, the connected reset, the reload ─────────────
{
    await settle();
    const first = openStream();
    eq(!!first && /^\/events/.test(first.url), true, 'the stream is /events');
    first.emit('connected', { build: 'build-A' });
    eq(await getsOver(600), 1, 'every connect fetches the view, the first included');
    // A dropped stream reconnects after 1 s, then 2 s, doubling to 10 s.
    const opened = () => streams.length;
    let n = opened();
    first.fail();
    eq(first.closed, true, 'a failed stream is closed');
    await advance(999);
    eq(opened(), n, 'no reconnect before 1 s');
    await advance(1);
    eq(opened(), n + 1, 'reconnected at 1 s');
    n = opened();
    openStream().fail();
    await advance(1999);
    eq(opened(), n, 'the second try waits longer');
    await advance(1);
    eq(opened(), n + 1, 'reconnected at 2 s');
    // The connected reset: once connected, the next drop is back to 1 s.
    openStream().emit('connected', { build: 'build-A' });
    n = opened();
    openStream().fail();
    await advance(1000);
    eq(opened(), n + 1, 'after a connected event the next reconnect is at 1 s again');
    eq(reloads, 0, 'the same build: no reload');
    // A new build: the kiosk reloads.
    openStream().emit('connected', { build: 'build-B' });
    eq(reloads, 1, 'a connected event naming a new build reloads the page');
    reloads = 0;
    // A heartbeat naming a new build reloads too (a proxy can hold the stream
    // open through a restart).
    openStream().emit('heartbeat', { build: 'build-C' });
    eq(reloads, 1, 'a heartbeat naming a new build reloads the page');
    reloads = 0;
    // A reconnect rebuilds the view: what was announced while the stream was
    // down is drawn now.
    await settle();
    openStream().fail();
    await advance(1000);
    openStream().emit('connected', { build: 'build-B' });
    eq(await getsOver(600), 1, 'a reconnect rebuilds the view');
    await settle();
}

// ── part-change alerts and the post-cutover banner ───────────────────────────
{
    const alerts = () => body.descendants().filter(n => n.classList.contains('os-alert') && !n.removed);
    const alertTexts = () => alerts().map(a => a.descendants().filter(n => n.classList.contains('os-alert-text'))
        .map(n => n.textContent).join(' '));
    const alertButton = (label) => {
        const b = alerts().map(a => a.descendants().find(n => n.tagName === 'BUTTON' && n.textContent === label)).find(Boolean);
        if (!b) throw new Error('no alert button ' + label + ' in ' + JSON.stringify(alertTexts()));
        return b;
    };
    const tapAlert = async (label) => { await alertButton(label).click(); await advance(10); };
    const boardView = () => {
        const v = pressView();
        v.nodes.forEach(t => {
            delete t.active_claim;
            t.loader = { role: 'produce', layout: 'shared_window', replenishment: 'operator', outbound_dest: '',
                parts: [{ code: 'PART-A' }], leaves_bare: false, pulled_directly: false };
        });
        return v;
    };
    const mismatch = { process_id: 1, process_name: 'PRESS-1', plc_name: 'PLC-1', style_id: 11, style_name: 'STYLE-1',
        live_catid: 'CAT-9', expected_catid: 'CAT-1' };

    served = pressView();
    await settle();
    fire('catid-mismatch', Object.assign({}, mismatch, { process_id: 9, process_name: 'PRESS-9' }));
    await advance(600);
    eq(alerts().length, 0, 'a part-id mismatch on another process: no alert here');
    fire('catid-mismatch', mismatch);
    await advance(600);
    eq(alertTexts(), ['Wrong part on PRESS-1: the process reports CAT-9; STYLE-1 runs CAT-1.'],
        'a part-id mismatch on its process: the process and both part ids');
    await advance(10000);
    eq(alerts().length, 1, 'the alert stays until dismissed');
    await tapAlert('Dismiss');
    eq(alerts().length, 0, 'Dismiss takes it off this screen');

    fire('catid-change-prompt', { process_id: 1, process_name: 'PRESS-1', plc_name: 'PLC-1', new_catid: 'CAT-2',
        has_target: true, target_style_id: 12, target_style_name: 'STYLE-2' });
    await advance(600);
    eq(alertTexts(), ['The process\'s part changed.'], 'the change prompt says the part changed');
    composerOpens = 0;
    await tapAlert('Start changeover');
    eq([composerOpens, alerts().length], [1, 0], 'Start changeover opens today\'s picker, with no target, and the prompt goes');
    fire('catid-change-prompt', { process_id: 1, process_name: 'PRESS-1', new_catid: 'CAT-2' });
    await advance(600);
    await tapAlert('Dismiss');
    eq(alerts().length, 0, 'the prompt\'s Dismiss');

    fire('catid-auto-cutover', { process_id: 1, process_name: 'PRESS-1', changeover_id: 5, new_catid: 'CAT-2' });
    await advance(600);
    eq(alertTexts(), ['Cutover was pressed automatically.'], 'the auto-cutover notice');
    await tapAlert('Dismiss');

    // A board of loader tiles draws none of the three.
    served = boardView();
    await settle();
    fire('catid-mismatch', mismatch);
    fire('catid-change-prompt', { process_id: 1, new_catid: 'CAT-2' });
    fire('catid-auto-cutover', { process_id: 1, new_catid: 'CAT-2' });
    await advance(600);
    eq(alerts().length, 0, 'a board of loader tiles draws no part-change alert');

    // The post-cutover banner, on a running board: shown when its event
    // arrives, though nothing on the view moved (a bug fixed: the check ran
    // only when the changeover state moved).
    served = pressView();
    await settle();
    cutoverFlag = { message: 'PRESS-1 reports CAT-9 after the cutover to STYLE-1', has_mapped: false };
    fire('changeover-verify-mismatch', { process_id: 1, process_name: 'PRESS-1', changeover_id: 5, style_id: 11 });
    await advance(600);
    eq(alertTexts(), ['PRESS-1 reports CAT-9 after the cutover to STYLE-1'], 'the post-cutover banner shows on the event');
    // changeover-verify-cleared clears it on every board.
    fire('changeover-verify-cleared', { process_id: 1 });
    await advance(10);
    eq(alerts().length, 0, 'changeover-verify-cleared takes the banner down');

    // A failed flag fetch is tried again on the next view fetch.
    flagFails = 1;
    fire('changeover-verify-mismatch', { process_id: 1 });
    await advance(600);
    eq(alerts().length, 0, 'the flag fetch failed: nothing shown yet');
    fire('material-refresh', { action: 'nudge' });
    await advance(600);
    eq(alertTexts(), ['PRESS-1 reports CAT-9 after the cutover to STYLE-1'],
        'the next view fetch tries the flag again (a bug fixed: a failed fetch was never retried)');
    // Its buttons come back after a failed POST.
    postFails = 1;
    await tapAlert('Confirm — process is correct');
    eq(alertButton('Confirm — process is correct').disabled, false, 'a refused confirm leaves its button live');
    cutoverFlag = null;
    await tapAlert('Confirm — process is correct');
    eq(alerts().length, 0, 'a confirm that lands takes the banner down');

    // No post-cutover banner on a board of loader tiles.
    served = boardView();
    cutoverFlag = { message: 'flagged', has_mapped: false };
    await settle();
    fire('changeover-verify-mismatch', { process_id: 1 });
    await advance(4000);
    eq(alerts().length, 0, 'a board of loader tiles draws no post-cutover banner');
    cutoverFlag = null;
    served = pressView();
    await settle();
}

// ── the stale sign ───────────────────────────────────────────────────────────
{
    const header = document.getElementById('os-header-actions');
    const sign = () => {
        const s = header.descendants().find(n => n.classList.contains('os-stale-sign'));
        const dot = header.descendants().find(n => n.classList.contains('os-health-badge'));
        return [s ? s.textContent : null, dot ? dot.className.replace('os-health-badge', '').trim() : null];
    };
    // refetch: something the station draws changed (the nudge after a write).
    const refetch = async () => { fire('material-refresh', { action: 'nudge' }); await advance(600); };
    served = pressView();
    await settle();
    eq(sign(), ['Live', 'online'], 'a station whose server answers: Live, the dot online');

    // A failed fetch: the sign says the server is not answering, with the time
    // of the data shown, and no toast (a bug fixed: a toast each poll, and a
    // green dot by construction).
    viewFails = 1;
    await refetch();
    const down = sign();
    eq([/^Not answering · data from \d{4}-/.test(down[0]), down[1]], [true, 'offline'],
        'a failed fetch: "Not answering · data from <time>", the dot offline (' + down[0] + ')');
    eq(toastTexts(), [], 'no toast for a failed fetch');
    await advance(20000);
    eq(viewGets.length, 1, 'no timer retries it');
    keepAlive();
    await advance(600);
    eq([viewGets.length, sign()], [2, ['Live', 'online']], 'the next heartbeat retries it, and it answers: Live again');
    keepAlive();
    await advance(600);
    eq(viewGets.length, 2, 'a heartbeat after a fetch that answered fetches nothing');

    // The stream drops: not answering until it is back.
    openStream().fail();
    await advance(10);
    eq(sign()[1], 'offline', 'the event stream dropped: the dot offline');
    await advance(1000);
    openStream().emit('connected', { build: 'build-B' });
    await advance(600);
    eq(sign(), ['Live', 'online'], 'the stream back: Live');

    // What the view says it does not know.
    const unknown = pressView();
    unknown.bins = { known: false };
    unknown.containment = { known: false };
    served = unknown;
    await refetch();
    eq(sign(), ['Bins not known yet · Containment not known yet', 'stale'],
        'bins and containment not known: said, the dot stale');
    const old = pressView();
    old.bins = { known: true, as_of: '2026-10-17T11:58:00Z' };
    served = old;
    await refetch();
    eq(sign(), ['Bins as of 2026-10-17T11:58:00Z', 'stale'], 'bins as of a time');

    // The payload catalog (each part's manifest lines, read by the load
    // modal): as of a time, or not known.
    const oldCatalog = pressView();
    oldCatalog.catalog = { known: true, as_of: '2026-10-17T11:57:00Z' };
    served = oldCatalog;
    await refetch();
    eq(sign(), ['Catalog as of 2026-10-17T11:57:00Z', 'stale'], 'the catalog as of a time');
    const noCatalog = pressView();
    noCatalog.catalog = { known: false };
    served = noCatalog;
    await refetch();
    eq(sign(), ['Catalog not known yet', 'stale'], 'the catalog not known');
    served = pressView();
    await refetch();
    eq(sign(), ['Live', 'online'], 'the catalog current again: Live');

    // On a board of loader tiles the sign is in its top line; while the bins
    // are not known its bar's request is not offered.
    const bar = pressView();
    bar.nodes.forEach(t => {
        delete t.active_claim;
        delete t.bin_state;
        t.orders = [];
        t.loader = { role: 'produce', layout: 'shared_window', replenishment: 'operator', outbound_dest: '',
            parts: [{ code: 'PART-A' }], leaves_bare: false, pulled_directly: false };
    });
    bar.bins = { known: false };
    served = bar;
    await refetch();
    const grid = document.getElementById('os-grid');
    const top = grid.descendants().find(n => n.classList.contains('os-pc-sign'));
    eq(top ? top.textContent : null, 'Bins not known yet', 'the board says it in its top line');
    const req = grid.descendants().find(n => n.tagName === 'BUTTON' && /^Request empty/.test(n.textContent));
    eq(req ? [req.textContent, req.disabled] : null, ['Request empty — could not look', true],
        'bins not known: the board bar offers no request (a bug fixed: it offered one for windows it could not see)');
    // The node modal offers no request card either.
    ctx.openModal(1);
    const modalHTML = document.getElementById('node-modal-content').innerHTML;
    eq(/request-empty/.test(modalHTML), false, 'bins not known: the node modal offers no request card');
    ctx.closeModal();
    bar.bins = { known: true };
    served = bar;
    await refetch();
    ctx.openModal(1);
    eq(/request-empty/.test(document.getElementById('node-modal-content').innerHTML), true,
        'bins known and no bin: the request card as before');
    ctx.closeModal();
    served = pressView();
    await settle();
}

// ── a sticky alert is not evicted by later toasts ────────────────────────────
{
    toastBox.children = [];
    ctx.showToast('order failed', 'error', { sticky: true });
    for (let i = 0; i < 5; i++) ctx.showToast('note ' + i, 'info');
    eq(toastTexts().filter(t => t.indexOf('order failed') === 0).length, 1,
        'a sticky toast survives five later toasts (a bug fixed: the container dropped the oldest, sticky or not)');
    eq(toastTexts().length <= 4, true, 'the plain toasts still give way to each other');
    toastBox.children = [];
}

finished = true;
console.log('operator refresh: ' + passed + ' passed' + (failed ? ', ' + failed + ' FAILED' : ''));
process.exitCode = failed ? 1 : 0;
})().catch(function (err) { console.error(err); process.exitCode = 1; });
