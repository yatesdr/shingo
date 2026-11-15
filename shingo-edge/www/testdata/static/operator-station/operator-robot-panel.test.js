// The robot panel on a cell operator screen (operator-robot-panel.js, the life
// cycle; operator-robot-map.js, the drawing): when it shows and goes, what it
// draws, its crop, its tap, its stream, and that hidden is zero work.
//
// THE PAGE RUNS FOR REAL, as operator-refresh.test.js boots it: the station's
// shipping modules, operator.js last, in one vm with a small fake DOM, a fake
// clock (every timer and animation frame runs when the pin says) and fake
// event streams; the view, the scene and the panel's stream are served here.
// The panel's drawing is loaded through its one import(), which this file
// answers with operator-robot-map.js and the shared scene modules it imports
// (scene-geom, scene-net, scene-path, scene-view, comet-layer), each loaded
// once with its imports bound, as a browser's module map holds them
// (shared/testdata/js/shared-modules.js).
//
// THE SCENE is synthetic, whole metres on a made-up grid: no plant data.
//
// Runs under plain Node (no npm). Exit 0 = pass, 1 = any failure. Run via the
// Go wrapper operator_robot_panel_test.go.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SRC_DIR = path.join(__dirname, '..', '..', '..', 'static', 'operator-station');
const SHARED_DIR = path.join(__dirname, '..', '..', '..', '..', '..', 'shared');
const SM = require(path.join(SHARED_DIR, 'testdata', 'js', 'shared-modules.js'));
const MAP_URL = '/static/operator-station/operator-robot-map.js';
const PANEL_FILES = /operator-robot-map\.js|operator-robot-panel\.js|comet-layer\.js|scene-view\.js/;

let passed = 0, failed = 0, finished = false;
process.on('exit', function (code) {
    if (!finished && code === 0) { console.error('FAIL: the test did not reach its end'); process.exitCode = 1; }
});
function eq(got, want, label) {
    const g = JSON.stringify(got), w = JSON.stringify(want);
    if (g === w) { passed++; return; }
    failed++;
    console.error('FAIL: ' + label + '\n   got:  ' + g + '\n   want: ' + w);
}
function ok(cond, label, detail) {
    if (cond) { passed++; return; }
    failed++;
    console.error('FAIL: ' + label + (detail ? '\n   ' + detail : ''));
}

// ── the scene ────────────────────────────────────────────────────────────────
//
// A 60 x 40 m grid, a vertex every 10 m, every lane stored both ways. The cell
// is five press positions, PLN_01..PLN_05, on a spur 6 m north of N40_20; the
// supermarket SMN_01 sits at the far corner.
function gridScene() {
    const points = {}, edges = [];
    const xs = [0, 10, 20, 30, 40, 50, 60], ys = [0, 10, 20, 30, 40];
    const name = (x, y) => 'N' + x + '_' + y;
    xs.forEach(x => ys.forEach(y => { points[name(x, y)] = { x: x, y: y, class: 'LocationMark' }; }));
    const both = (a, b) => { edges.push({ from: a, to: b }); edges.push({ from: b, to: a }); };
    xs.forEach((x, i) => ys.forEach((y, j) => {
        if (i + 1 < xs.length) both(name(x, y), name(xs[i + 1], y));
        if (j + 1 < ys.length) both(name(x, y), name(x, ys[j + 1]));
    }));
    [1, 2, 3, 4, 5].forEach(k => {
        points['PLN_0' + k] = { x: 34 + 2 * k, y: 26, class: 'GeneralLocation' };
        both('N40_20', 'PLN_0' + k);
    });
    points.SMN_01 = { x: 2, y: 38, class: 'GeneralLocation' };
    both('N0_40', 'SMN_01');
    return { revision: 'r1', points: points, edges: edges };
}
const SCENE = gridScene();
// A robot driving in from the west along the bottom row to PLN_03.
const PATH_WEST = ['N10_0', 'N20_0', 'N30_0', 'N40_0', 'N40_10', 'N40_20', 'PLN_03'];

// ── the station's views ──────────────────────────────────────────────────────

function tile(id, orders) {
    return {
        node: { id: id, process_id: 1, core_node_name: 'PLN_0' + id, name: 'PLN_0' + id },
        runtime: { remaining_uop_cached: 5, active_bin_id: 1 },
        active_claim: { id: 40 + id, core_node_name: 'PLN_0' + id, role: 'consume', swap_mode: 'single_robot',
            payload_code: 'PART-A', uop_capacity: 20 },
        orders: orders || [],
        bin_state: { occupied: true, payload_code: 'PART-A', stocked: true, bin_label: 'BIN-' + id },
    };
}
// cellView is a press screen of five positions; byTile gives each tile its orders.
function cellView(byTile) {
    byTile = byTile || {};
    return {
        station: { id: 7, name: 'Press screen', health_status: 'online' },
        process: { id: 1, name: 'PRESS-1', active_style_id: 11, target_style_id: 11 },
        current_style: { id: 11, name: 'STYLE-1' },
        nodes: [1, 2, 3, 4, 5].map(id => tile(id, byTile[id])),
        bins: { known: true },
    };
}
function boardView(byTile) {
    const v = cellView(byTile);
    v.nodes.forEach(t => {
        delete t.active_claim;
        t.loader = { role: 'produce', layout: 'shared_window', replenishment: 'operator', outbound_dest: '',
            parts: [{ code: 'PART-A' }], leaves_bare: false, pulled_directly: false };
    });
    return v;
}
function order(id, status, robot, delivery, extra) {
    return Object.assign({ id: id, uuid: 'u-' + id, order_type: 'retrieve', status: status, payload_code: 'PART-A',
        source_node: 'SMN_01', delivery_node: delivery || 'PLN_03', eta: null, auto_confirm: false,
        queue_reason: '', queue_code: '', lane_held: false, departed: false,
        created_at: '2026-11-14T11:00:00Z', fault_line: '' }, robot ? { robot_id: robot } : {}, extra || {});
}

// ── one page: its DOM, clock, network and streams ───────────────────────────

function bootPage(opts) {
    opts = opts || {};
    const h = { counts: { writes: 0, panelTimers: 0, rafAsks: 0, sceneGets: 0, imports: 0 }, maps: [] };
    let now = Date.UTC(2026, 10, 14, 12, 0, 0);
    h.now = () => now;

    // the DOM: an element keeps its attributes and children; every write into
    // the robot panel's subtree is counted (h.counts.writes).
    function inPanel(n) { for (; n; n = n.parentNode) if (n.id === 'os-robot-panel') return true; return false; }
    function wrote(n) { if (inPanel(n)) h.counts.writes++; }
    class FakeEl {
        constructor(tag, ns) {
            this.tagName = ns ? String(tag) : String(tag).toUpperCase();
            this.ns = ns || null;
            this.children = [];
            this.parentNode = null;
            this.listeners = {};
            this.dataset = {};
            this.attrs = {};
            this.style = { setProperty(k, v) { this[k] = v; }, removeProperty(k) { delete this[k]; } };
            this.disabled = false;
            this.clientWidth = 0;
            this.clientHeight = 0;
            this._text = '';
            this._hidden = false;
            const self = this;
            this.classList = {
                add(c) { const l = self.className.split(/\s+/).filter(Boolean); if (l.indexOf(c) < 0) l.push(c); self.className = l.join(' '); },
                remove() { const drop = Array.prototype.slice.call(arguments); self.className = self.className.split(/\s+/).filter(x => x && drop.indexOf(x) < 0).join(' '); },
                contains(c) { return self.className.split(/\s+/).indexOf(c) >= 0; },
                toggle(c, on) { if (on === undefined ? !this.contains(c) : on) this.add(c); else this.remove(c); },
            };
        }
        get className() { return this.attrs['class'] || ''; }
        set className(v) { this.attrs['class'] = String(v); wrote(this); }
        get id() { return this.attrs.id || ''; }
        set id(v) { this.attrs.id = String(v); wrote(this); }
        get hidden() { return this._hidden; }
        set hidden(v) { this._hidden = !!v; wrote(this); }
        get textContent() { return this._text + this.children.map(c => c.textContent).join(''); }
        set textContent(v) { this._text = v == null ? '' : String(v); this.children.forEach(c => { c.parentNode = null; }); this.children = []; wrote(this); }
        set innerHTML(v) { this._text = ''; this.children.forEach(c => { c.parentNode = null; }); this.children = []; this._html = String(v); wrote(this); }
        get innerHTML() { return this._html || ''; }
        get firstChild() { return this.children[0] || null; }
        get lastChild() { return this.children[this.children.length - 1] || null; }
        get nextSibling() {
            if (!this.parentNode) return null;
            const sib = this.parentNode.children;
            return sib[sib.indexOf(this) + 1] || null;
        }
        appendChild(c) { return this.insertBefore(c, null); }
        insertBefore(c, ref) {
            if (c.parentNode) c.parentNode.removeChild(c);
            const at = ref ? this.children.indexOf(ref) : -1;
            if (at < 0) this.children.push(c); else this.children.splice(at, 0, c);
            c.parentNode = this;
            wrote(this);
            return c;
        }
        removeChild(c) {
            const at = this.children.indexOf(c);
            if (at >= 0) this.children.splice(at, 1);
            wrote(this);
            c.parentNode = null;
            return c;
        }
        remove() { if (this.parentNode) this.parentNode.removeChild(this); }
        setAttribute(k, v) { this.attrs[k] = String(v); wrote(this); }
        getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; }
        hasAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k); }
        removeAttribute(k) { delete this.attrs[k]; wrote(this); }
        addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); }
        removeEventListener(t, fn) { this.listeners[t] = (this.listeners[t] || []).filter(f => f !== fn); }
        click() { return dispatch(this, 'click'); }
        focus() {}
        getBoundingClientRect() { return { left: 0, top: 0, width: 0, height: 0 }; }
        descendants() { const out = []; (function walk(n) { n.children.forEach(c => { out.push(c); walk(c); }); })(this); return out; }
        querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
        querySelectorAll(sel) {
            if (sel[0] === '.') return this.descendants().filter(n => n.classList.contains(sel.slice(1)));
            if (sel[0] === '#') return this.descendants().filter(n => n.id === sel.slice(1));
            return this.descendants().filter(n => n.tagName.toLowerCase() === sel.toLowerCase());
        }
    }
    h.FakeEl = FakeEl;
    const body = new FakeEl('body');
    body.dataset.stationId = '7';
    const footer = new FakeEl('footer');
    footer.id = 'os-footer';
    body.appendChild(footer);
    const byID = {};
    const docListeners = {}, winListeners = {};
    const document = {
        body: body, documentElement: new FakeEl('html'), readyState: 'complete', hidden: false, visibilityState: 'visible',
        getElementById(id) {
            const live = body.descendants().find(n => n.id === id);
            if (live) return live;
            if ((/^os-/.test(id) && id !== 'os-postcutover-flag' && id !== 'os-alerts' && id !== 'os-robot-panel') ||
                id === 'node-modal' || id === 'node-modal-content' || /^load-bin|^keypad/.test(id)) {
                if (!byID[id]) { byID[id] = new FakeEl('div'); byID[id].id = id; }
                return byID[id];
            }
            return null;
        },
        createElement(tag) { return new FakeEl(tag); },
        createElementNS(ns, tag) { return new FakeEl(tag, ns); },
        createTextNode(t) { const n = new FakeEl('#text'); n._text = String(t); return n; },
        querySelector(sel) { return body.querySelector(sel); },
        querySelectorAll(sel) { return body.querySelectorAll(sel); },
        addEventListener(t, fn) { (docListeners[t] = docListeners[t] || []).push(fn); },
    };
    h.document = document;
    h.body = body;
    // dispatch: the event at target, then up through its ancestors, the
    // document and the window, unless a listener stops it.
    function dispatch(target, type) {
        const ev = { type: type, target: target, stopped: false, defaultPrevented: false,
            stopPropagation() { this.stopped = true; }, preventDefault() { this.defaultPrevented = true; } };
        for (let n = target; n && !ev.stopped; n = n.parentNode) {
            ev.currentTarget = n;
            (n.listeners[type] || []).slice().forEach(fn => fn.call(n, ev));
        }
        if (!ev.stopped) (docListeners[type] || []).slice().forEach(fn => fn(ev));
        if (!ev.stopped) (winListeners[type] || []).slice().forEach(fn => fn(ev));
        return ev;
    }
    h.dispatch = dispatch;
    h.becomeVisible = function () { document.hidden = false; document.visibilityState = 'visible'; (docListeners.visibilitychange || []).forEach(fn => fn({})); };
    h.becomeHidden = function () { document.hidden = true; document.visibilityState = 'hidden'; (docListeners.visibilitychange || []).forEach(fn => fn({})); };

    // the clock: every timer and every animation frame, run when the pin says.
    let timers = [], timerSeq = 0, frames = [];
    function fromPanel() { return PANEL_FILES.test(new Error().stack || ''); }
    function fakeSetTimeout(fn, ms) {
        const t = { id: ++timerSeq, at: now + Math.max(0, ms || 0), fn: fn, panel: fromPanel() };
        if (t.panel) h.counts.panelTimers++;
        timers.push(t);
        return t.id;
    }
    function fakeClearTimeout(id) { timers = timers.filter(t => t.id !== id); }
    function fakeSetInterval(fn, ms) {
        const again = function () { fakeSetTimeout(again, ms); fn(); };
        return fakeSetTimeout(again, ms);
    }
    function fakeRAF(fn) {
        if (fromPanel()) h.counts.rafAsks++;
        frames.push(fn);
        return frames.length;
    }
    function fakeCancelRAF() { frames = []; }
    h.panelTimersPending = () => timers.filter(t => t.panel).length;
    h.framesPending = () => frames.length;
    function flush() {
        let p = Promise.resolve();
        for (let i = 0; i < 8; i++) p = p.then(() => new Promise(r => setImmediate(r)));
        return p;
    }
    h.flush = flush;
    h.advance = async function (ms) {
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
    };
    // vsync runs the frames asked for, with display time t.
    h.vsync = function (t) { const due = frames; frames = []; due.forEach(fn => fn(t)); };
    class FakeDate extends Date {
        constructor() { if (arguments.length === 0) super(now); else super(...arguments); }
        static now() { return now; }
    }

    // the network
    let served = opts.view || cellView();
    h.serve = v => { served = v; };
    h.sceneAnswers = (opts.scene === undefined ? [{ status: 200, body: SCENE }] : opts.scene).slice();
    h.requests = [];
    function response(status, text, headers) {
        return {
            ok: status >= 200 && status < 300, status: status,
            headers: { get(k) { return (headers || {})[String(k).toLowerCase()] || null; } },
            json: async () => JSON.parse(text), text: async () => text,
        };
    }
    function fetchStub(url, o) {
        h.requests.push({ url: url, opts: o || {} });
        if (url === '/api/operator-stations/7/view') return Promise.resolve(response(200, JSON.stringify(served)));
        if (url === '/api/operator-stations/7/scene') {
            h.counts.sceneGets++;
            const a = h.sceneAnswers.length > 1 ? h.sceneAnswers.shift() : h.sceneAnswers[0];
            if (a.reject) return Promise.reject(new Error('network'));
            return Promise.resolve(response(a.status, JSON.stringify(a.body)));
        }
        if (/post-cutover-flag$/.test(url)) return Promise.resolve(response(200, '{"flagged":false}'));
        return Promise.resolve(response(200, '{}'));
    }

    // the streams: the station's bus and the panel's own.
    const streams = [];
    class FakeEventSource {
        constructor(url) { this.url = url; this.listeners = {}; this.closed = false; this.onerror = null; streams.push(this); }
        addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); }
        close() { this.closed = true; }
        emit(t, data) { (this.listeners[t] || []).forEach(fn => fn({ type: t, data: JSON.stringify(data), lastEventId: '' })); }
    }
    h.streams = streams;
    h.panelStreams = () => streams.filter(s => s.url === '/events?topics=robot-pos.7');
    h.openPanelStream = () => h.panelStreams().filter(s => !s.closed)[0] || null;
    h.bus = () => streams.filter(s => s.url !== '/events?topics=robot-pos.7' && !s.closed).slice(-1)[0];
    h.fireBus = (t, d) => { const s = h.bus(); if (s) s.emit(t, d); };
    // frame sends one robot-pos frame on the panel's open stream.
    h.frame = function (data) {
        const s = h.openPanelStream();
        if (!s) throw new Error('no panel stream is open');
        s.emit('robot-pos.7', data);
    };

    // the store
    // Every page is a screen an operator turned ROBOTS on (its store says
    // "on"), unless it brings its own store or is fresh: never pressed.
    const stored = Object.assign({}, opts.stored || (opts.fresh ? {} : { 'shingo.robot-panel.7': 'on' }));
    h.stored = stored;
    const localStorage = opts.store === 'throws'
        ? { getItem() { throw new Error('denied'); }, setItem() { throw new Error('denied'); }, removeItem() { throw new Error('denied'); } }
        : opts.store === 'readonly'
            ? { getItem(k) { return stored[k] == null ? null : stored[k]; }, setItem() { throw new Error('quota'); }, removeItem() { throw new Error('quota'); } }
            : { getItem(k) { return stored[k] == null ? null : stored[k]; }, setItem(k, v) { stored[k] = String(v); }, removeItem(k) { delete stored[k]; } };

    const win = { innerWidth: 1280, innerHeight: 800, location: { hash: '' },
        addEventListener(t, fn) { (winListeners[t] = winListeners[t] || []).push(fn); } };
    const ctx = vm.createContext({
        console: { log: console.log, warn: console.warn, error: function () { (h.errors = h.errors || []).push(Array.prototype.join.call(arguments, ' ')); } },
        JSON, Math, Number, Array, Object, String, Promise, Error, RegExp, Map, Set, WeakMap,
        parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent, AbortController,
        Date: FakeDate,
        setTimeout: fakeSetTimeout, clearTimeout: fakeClearTimeout,
        setInterval: fakeSetInterval, clearInterval: fakeClearTimeout,
        requestAnimationFrame: fakeRAF, cancelAnimationFrame: fakeCancelRAF,
        document: document, window: win, Node: FakeEl, EventSource: FakeEventSource,
        location: win.location, fetch: fetchStub, localStorage: localStorage,
        mountFlowPanel() {}, openFlowPanel() {}, syncFlowPanel() {},
        ensureComposer: async () => ({ openPicker() {} }), openKeypad() {},
        __import(url) {
            h.counts.imports++;
            if (url !== MAP_URL) return Promise.reject(new Error('the test serves no ' + url));
            if (opts.importFails) return Promise.reject(new Error('lost'));
            return Promise.resolve(loadMapModule());
        },
    });
    ctx.globalThis = ctx;

    function loadModule(name, rewriteImport) {
        let src = fs.readFileSync(path.join(SRC_DIR, name), 'utf8');
        src = src.replace(/^import\s[\s\S]*?from\s+['"][^'"]+['"];?[ \t]*$/gm, '');
        if (rewriteImport) {
            if ((src.match(/\bimport\(/g) || []).length !== 1) throw new Error(name + ' has not exactly one import(); update this test');
            src = src.replace(/\bimport\(/g, '__import(');
        }
        const exported = [];
        src = src.replace(/^export\s+(async\s+function|function|const|let)\s+(\w+)/gm, function (_, kw, n) { exported.push(n); return kw + ' ' + n; });
        src = src.replace(/^export\s*\{[^}]*\};?/gm, '');
        const out = vm.runInContext('(function () {\n' + src + '\nreturn {' + exported.map(n => n + ': ' + n).join(', ') + '};\n})()',
            ctx, { filename: path.basename(name) });
        Object.assign(ctx, out);
        return out;
    }
    let mapModule = null;
    function loadMapModule() {
        if (mapModule) return mapModule;
        const load = SM.realm(ctx);
        let src = fs.readFileSync(path.join(SRC_DIR, 'operator-robot-map.js'), 'utf8');
        src = SM.bindImports(src, SM.SCENE, '__shared');
        src = src.replace(/^export\s+function\s+(\w+)/gm, 'function $1');
        const real = vm.runInContext('(function (__shared) {' + src + '\nreturn { createRobotMap: createRobotMap };\n})',
            ctx, { filename: 'operator-robot-map.js' })(load);
        mapModule = { createRobotMap(o) { const m = real.createRobotMap(o); h.maps.push(m); return m; } };
        return mapModule;
    }
    ['../../../../shared/clock.js', '../../../../shared/bus.js'].forEach(n => loadModule(n));
    ctx.serverNow = () => now;
    ctx.formatClock = iso => String(iso);
    ctx.showLostConnectionBanner = () => {};
    ctx.clearLostConnectionBanner = () => {};
    ['../../../../shared/esc.js', 'order-status.js', 'operator-util.js', 'operator-state.js', 'operator-window-state.js',
        'operator-board-model.js', 'operator-board.js', 'operator-supply-refusal.js', 'operator-load-bin.js',
        'operator-release.js', 'operator-modal.js', 'operator-render.js'].forEach(n => loadModule(n));
    loadModule('operator-robot-panel.js', true);
    loadModule('operator.js');
    h.ctx = ctx;

    // ── reading the page ──
    h.panel = () => body.descendants().find(n => n.id === 'os-robot-panel') || null;
    h.visible = () => { const p = h.panel(); return !!p && !p.hidden; };
    h.map = () => h.maps[0] || null;
    h.toggle = () => document.getElementById('os-header-actions').children.find(c => /\brobots\b/.test(c.className)) || null;
    h.svg = () => { const p = h.panel(); return p ? p.querySelectorAll('svg').find(s => s.getAttribute('class') === 'map-svg') : null; };
    h.layer = name => { const s = h.svg(); return s ? s.children.find(g => g.getAttribute('data-layer') === name) : null; };
    h.cometLayer = () => { const p = h.panel(); return p ? p.querySelectorAll('svg').find(s => s.getAttribute('class') === 'map-comet-layer') : null; };
    h.rows = () => { const p = h.panel(); const ul = p && p.querySelector('.os-rp-rows'); return ul ? ul.children.map(li => li.textContent) : []; };
    h.rowEls = () => { const p = h.panel(); const ul = p && p.querySelector('.os-rp-rows'); return ul ? ul.children : []; };
    // setView hands the station a new view the way a write does (the nudge).
    h.setView = async function (v) { served = v; h.fireBus('material-refresh', { action: 'nudge' }); await h.advance(1000); };
    // mark zeroes the work counters.
    h.mark = () => { Object.keys(h.counts).forEach(k => { h.counts[k] = 0; }); };
    h.boot = async function () {
        await flush();
        h.fireBus('connected', { build: 'b1' });
        await h.advance(1000);
    };
    return h;
}

// ── helpers over a page ──────────────────────────────────────────────────────

// available is an {available} frame as the Edge sends it (robot_stream.go
// robotPosAvailable): with interval_ms, Core's robot poll (D17), 2000 unless
// a pin says another.
function available(on, intervalMS) { return { available: on, interval_ms: intervalMS === undefined ? 2000 : intervalMS }; }
// show boots a page onto view v, answers the panel's stream {available: true}
// and returns the page.
async function shownPage(v, opts) {
    const h = bootPage(Object.assign({ view: v }, opts || {}));
    await h.boot();
    h.frame(available(true));
    await h.flush();
    return h;
}
// pos is one robot-pos order entry.
function pos(orderID, robot, x, y, pathNames, heading) {
    const p = { order_id: orderID, order_uuid: 'u-' + orderID, robot_id: robot, x: x, y: y, heading: heading || 0 };
    if (pathNames) p.path = pathNames;
    return p;
}
function frameOf(list) { return { t: '2026-11-14T12:00:00.000Z', interval_ms: 2000, orders: list }; }
// drive sends a frame now and another `ms` later with each robot moved by
// (dx, dy): two positions apart, so the panel sees the robot moving.
async function drive(h, list, dx, ms) {
    h.frame(frameOf(list));
    await h.advance(ms || 2000);
    h.frame(frameOf(list.map(p => Object.assign({}, p, { x: p.x + (dx || 1) }))));
    await h.flush();
}
function attrsOf(n) { return n.attrs; }
function robotGroups(h) { const l = h.layer('robots'); return l ? l.children.filter(g => /^robot:/.test(g.attrs['data-robot'] ? 'robot:' + g.attrs['data-robot'] : '') && !g.children.some(k => k.attrs['class'] === 'map-chip')) : []; }
function chipGroups(h) { const l = h.layer('robots'); return l ? l.children.filter(g => g.children.some(k => k.attrs['class'] === 'map-chip')) : []; }
function chipRects(h) { return chipGroups(h).map(g => g.children.find(k => k.attrs['class'] === 'map-chip')); }
function viewBox(h) { const vb = h.svg().attrs.viewBox.split(' ').map(Number); return { minX: vb[0], minY: vb[1], w: vb[2], h: vb[3] }; }
function proj(x, y) { return [x, -y]; }
// marksOf is the node glyphs on the nodes layer by the scene point each sits
// on: hot, the ones with Core's accent (stroke and glow); quiet, the rest.
function marksOf(h) {
    const out = { hot: [], quiet: [] };
    h.layer('nodes').children.forEach(n => {
        const name = Object.keys(SCENE.points).find(k => {
            const p = proj(SCENE.points[k].x, SCENE.points[k].y);
            return +n.attrs.cx === p[0] && +n.attrs.cy === p[1];
        });
        (n.attrs.stroke === '#7c7cf0' && /drop-shadow/.test(n.attrs.style || '') ? out.hot : out.quiet).push(name || '?');
    });
    out.hot.sort();
    out.quiet.sort();
    return out;
}
// alongAt is the point at fraction f of a polyline's length.
function alongAt(pts, f) {
    const segs = [];
    let total = 0;
    for (let i = 1; i < pts.length; i++) { const l = Math.hypot(pts[i][0] - pts[i - 1][0], pts[i][1] - pts[i - 1][1]); segs.push(l); total += l; }
    let want = f * total;
    for (let i = 0; i < segs.length; i++) {
        if (want <= segs[i]) { const t = want / segs[i]; return [pts[i][0] + (pts[i + 1][0] - pts[i][0]) * t, pts[i][1] + (pts[i + 1][1] - pts[i][1]) * t]; }
        want -= segs[i];
    }
    return pts[pts.length - 1];
}
function within(v, x, y, slack) { slack = slack || 1e-9; return x >= v.minX - slack && x <= v.minX + v.w + slack && y >= v.minY - slack && y <= v.minY + v.h + slack; }
// tap presses the screen at the panel the way a finger does: pointerdown,
// pointerup, then click, each sent to whatever is on top at that moment (the
// panel while it is shown, else the control under it), the click too, as a
// touch's click is hit-tested when it is fired.
function tap(h, under) {
    ['pointerdown', 'pointerup', 'click'].forEach(type => {
        const target = h.visible() ? h.svg() || h.panel() : under;
        h.dispatch(target, type);
    });
}

// ═════════════════════════════════════════════════════════════════════════════
(async function main() {

// ── 1. Lazy: nothing of the panel at boot, and nothing until an order has a robot.
{
    const h = bootPage({ view: cellView({ 3: [order(501, 'queued', '', 'PLN_03'), order(502, 'in_transit', '', 'PLN_03')] }) });
    await h.boot();
    eq([h.counts.imports, h.counts.sceneGets, h.panelStreams().length, !!h.panel()], [0, 0, 0, false],
        'a screen whose orders have no robot loads no drawing, asks for no scene, opens no panel stream');
    ok(!!h.toggle(), 'a cell screen carries the ROBOTS toggle in its top bar');
    eq([h.toggle().textContent, h.toggle().attrs['aria-pressed'], h.toggle().attrs['aria-label']],
        ['ROBOTS', 'true', 'Robots on this screen'], 'the toggle reads ROBOTS, on, on a screen an operator turned it on');
    // A reshuffling parent has no robot: Core tracks its child legs.
    await h.setView(cellView({ 3: [order(503, 'reshuffling', 'AMR-01', 'PLN_03')] }));
    eq([h.counts.imports, h.panelStreams().length], [0, 0], 'a reshuffling parent shows no robot (D13)');
    // A board draws no header and no panel, whatever its orders.
    await h.setView(boardView({ 3: [order(504, 'in_transit', 'AMR-01', 'PLN_03')] }));
    eq([h.toggle(), h.counts.imports, h.panelStreams().length], [null, 0, 0], 'a board carries no toggle and shows no panel');
}

// ── 2. Appears on dispatch, goes on delivery; one scene per page life.
{
    const h = bootPage({ view: cellView({ 3: [order(501, 'queued', '', 'PLN_03')] }) });
    await h.boot();
    await h.setView(cellView({ 3: [order(501, 'dispatched', 'AMR-01', 'PLN_03')] }));
    eq([h.counts.imports, h.counts.sceneGets, h.panelStreams().length], [1, 1, 1],
        'an order dispatched with a robot: the drawing loads once, the scene is asked for once, the panel opens its own stream');
    eq(h.openPanelStream().url, '/events?topics=robot-pos.7', 'the panel stream names only this screen\'s topic, and no station= (D12)');
    const sceneReq = h.requests.find(r => /\/scene$/.test(r.url));
    eq([sceneReq.url, sceneReq.opts.cache, sceneReq.opts.headers], ['/api/operator-stations/7/scene', undefined, undefined],
        'the scene GET leaves the browser\'s cache on, so the door\'s ETag revalidates it on a later page life');
    eq(h.visible(), false, 'nothing is on screen before the stream says {available: true}');
    h.frame(available(true));
    await h.flush();
    eq(h.visible(), true, 'shown once the stream says {available: true}');
    eq(h.panel().attrs['aria-label'], 'Robots on this screen. Tap to hide.', 'the panel says what it is in screen words');
    await h.setView(cellView({ 3: [order(501, 'delivered', 'AMR-01', 'PLN_03')] }));
    eq([h.visible(), h.openPanelStream()], [false, null], 'delivered: no order has a robot, the panel goes and its stream closes');
    await h.setView(cellView({ 2: [order(601, 'in_transit', 'AMR-02', 'PLN_02')] }));
    h.frame(available(true));
    await h.flush();
    eq([h.visible(), h.counts.sceneGets, h.counts.imports], [true, 1, 1],
        'the next order with a robot shows it again on the scene and the drawing already held');
}

// ── 3. One robot: what is drawn.
{
    // The order stored AMR-03, the robot Core last named; the stream names
    // AMR-07, the robot driving it now after a handover (D14).
    const v = cellView({ 3: [order(501, 'in_transit', 'AMR-03', 'PLN_03', { eta: new Date(Date.UTC(2026, 10, 14, 12, 3, 30)).toISOString() })] });
    const h = await shownPage(v);
    await drive(h, [pos(501, 'AMR-07', 12, 0, PATH_WEST, 0)], 1, 2000);
    h.vsync(3000);
    const robots = robotGroups(h), chips = chipGroups(h);
    eq([robots.length, chips.length], [1, 1], 'one robot: one glyph group and one name chip');
    const g = robots[0];
    eq(g.attrs['data-robot'], 'AMR-07', 'the robot drawn is the one the stream names, not the one the order stored (D14)');
    eq(g.children.map(k => k.attrs['class'] || k.tagName), ['map-robot-halo', 'g', 'map-robot-core'],
        'a moving robot: its halo, its heading chevron, its core, as Core draws one');
    const at = proj(13, 0);
    eq(g.children[1].attrs.transform, 'translate(' + at[0] + ',' + at[1] + ') rotate(0)',
        'the chevron sits at the robot\'s last position, turned to its heading on an unturned map');
    eq(g.children[0].attrs.fill, '#34c3e0', 'in transit: the robot takes the status hue Core gives it');
    const chip = chips[0].children;
    const fit = h.map().inspect().fit;
    eq([chip.find(k => k.tagName === 'text').textContent, +chip.find(k => k.tagName === 'text').attrs['font-size'] * fit.s],
        ['AMR-07', 12], 'its chip names the robot at 12 px whatever the zoom');
    // D16 (owner's F5): the robot's destination takes Core's hot-node accent;
    // the cell's other nodes the quiet mark (U10 marked all five).
    eq(marksOf(h), { hot: ['PLN_03'], quiet: ['PLN_01', 'PLN_02', 'PLN_04', 'PLN_05'] },
        'one robot: its destination, PLN_03, takes the accent, and the cell\'s other four nodes do not');
    const quiet = h.layer('nodes').children.find(n => n.attrs.stroke === undefined);
    eq(quiet && [quiet.tagName, quiet.attrs.fill, quiet.attrs['fill-opacity'], quiet.attrs.style], ['circle', '#7e92b3', '0.7', undefined],
        'the quiet cell mark: the node dot in --map-node, no accent stroke, no glow');
    const comets = h.cometLayer().children;
    eq([comets.length, comets[0].tagName, comets.slice(1).every(c => c.tagName === 'circle')], [10, 'polygon', true],
        'one comet: Core\'s head and nine dots (shared/comet-layer.js)');
    eq(comets[0].attrs['class'], 'map-comet-head', 'its path ends at the cell node it brings to: the carry leg, solid');
    // The comet runs along RDS's path from the robot: its first dot is past the
    // robot on the bottom row.
    // At half a lap (display time 3000 ms of a 6000 ms lap) the head is half
    // way along RDS's path drawn from the robot: (13, 0) east to N40_0, north
    // to N40_20, then the spur to PLN_03.
    const head = /translate\(([-\d.]+) ([-\d.]+)\)/.exec(comets[0].attrs.transform);
    const half = alongAt([[13, 0], [20, 0], [30, 0], [40, 0], [40, 10], [40, 20], [40, 26]].map(p => proj(p[0], p[1])), 0.5);
    ok(head && Math.abs(+head[1] - half[0]) < 0.01 && Math.abs(+head[2] - half[1]) < 0.01,
        'the comet head is half way along RDS\'s path from the robot at half a lap', 'head ' + (head && head[0]) + ' want ' + half);
    eq(h.layer('routes').children.length, 0, 'a moving robot draws no static lane');
    const lanes = h.layer('base').children.filter(n => n.attrs['class'] === 'map-aisle');
    ok(lanes.length > 0 && lanes.length < 58 + 6, 'the aisles in the crop, and only those', lanes.length + ' lanes');
    eq(h.rows(), ['AMR-07ETA: ~3 min'], 'its row: its id and its ETA, from the order the board holds');
    eq(h.panel().descendants().filter(n => n.tagName === 'text' || n.tagName === 'SPAN').map(n => n.textContent)
        .some(t => /station/i.test(t)), false, 'no word on the panel says station (UI text says screen)');
    // The order is sent to PLN_04 instead, the same robot and the same nodes
    // drawn: the accent moves with it, and the crop stays.
    const fitsBefore = h.map().inspect().fits;
    await h.setView(cellView({ 3: [order(501, 'in_transit', 'AMR-03', 'PLN_04', { eta: new Date(Date.UTC(2026, 10, 14, 12, 3, 30)).toISOString() })] }));
    eq([marksOf(h).hot, h.map().inspect().fits], [['PLN_04'], fitsBefore],
        'its destination moves to PLN_04: the accent moves with it, and the crop is not worked out again');
}

// ── 4. Empty path and an unroutable hop: no route, no comet (D10).
{
    const h = await shownPage(cellView({ 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03')] }));
    await drive(h, [pos(501, 'AMR-07', 12, 0, null, 0)], 1, 2000);
    h.vsync(3000);
    eq([robotGroups(h).length, h.cometLayer() ? h.cometLayer().children.length : 0, h.layer('routes').children.length],
        [1, 0, 0], 'no path: the robot, no comet, no lane');
    await drive(h, [pos(501, 'AMR-07', 12, 0, ['N10_0', 'N30_0', 'PLN_03'], 0)], 1, 2000);
    h.vsync(6000);
    eq([robotGroups(h).length, h.cometLayer().children.length, h.map().inspect().misses > 0], [1, 0, true],
        'a hop with no stored row: no comet, counted, the robot still drawn');
}

// ── 5. A swap: both robots, both in the crop, both rows.
{
    const v = cellView({ 2: [order(601, 'in_transit', 'AMR-01', 'SMN_01', { order_type: 'store', source_node: 'PLN_02', eta: '2026-11-14T12:04:00Z' })],
        3: [order(602, 'in_transit', 'AMR-02', 'PLN_03', { sibling_order_id: 601 })] });
    const h = await shownPage(v);
    // AMR-02 is still on its way to its source: RDS's path ends at SMN_01.
    await drive(h, [pos(601, 'AMR-01', 40, 22, ['N40_20', 'N30_20'], Math.PI),
        pos(602, 'AMR-02', 4, 0, ['N0_0', 'N0_10', 'N0_20', 'N0_30', 'N0_40', 'SMN_01'], 0)], 1, 2000);
    h.vsync(3000);
    eq(robotGroups(h).map(g => g.attrs['data-robot']), ['AMR-01', 'AMR-02'], 'a swap draws both robots');
    const vb = viewBox(h);
    eq([[41, 22], [5, 0]].map(p => within(vb, proj(p[0], p[1])[0], proj(p[0], p[1])[1])), [true, true],
        'the crop holds both robots of the swap');
    eq(h.cometLayer().children.length, 20, 'two moving robots, two comets');
    eq([h.cometLayer().children[0].attrs['class'], h.cometLayer().children[10].attrs['class']], ['map-comet-head', 'map-comet-head-hollow'],
        'the robot leaving from the cell carries (solid); the one still far from its source fetches (hollow): Core\'s orderLeg');
    eq(h.rows(), ['AMR-01', 'AMR-02'], 'one row each; the robot leaving for the supermarket has no ETA to this screen');
    // D16: both destinations take the accent, the one off the cell too, and it
    // joins the crop (U10's crop held the cell and the robots only).
    eq(marksOf(h), { hot: ['PLN_03', 'SMN_01'], quiet: ['PLN_01', 'PLN_02', 'PLN_04', 'PLN_05'] },
        'a swap: both robots\' destinations take the accent, the supermarket one off the cell included');
    const smn = proj(SCENE.points.SMN_01.x, SCENE.points.SMN_01.y);
    ok(within(vb, smn[0], smn[1]), 'the outbound robot\'s destination, SMN_01, off the cell, is in the crop');
}

// ── 5d. A destination the scene does not place: nothing accented, no error (D16).
{
    const h = await shownPage(cellView({ 3: [order(551, 'in_transit', 'AMR-09', 'NOWHERE_9')] }));
    await drive(h, [pos(551, 'AMR-09', 12, 0, PATH_WEST, 0)], 1, 2000);
    h.vsync(3000);
    eq([robotGroups(h).length, marksOf(h), (h.errors || []).length],
        [1, { hot: [], quiet: ['PLN_01', 'PLN_02', 'PLN_03', 'PLN_04', 'PLN_05'] }, 0],
        'a destination not on the scene: the robot drawn, no node accented, every cell node the quiet mark, nothing logged');
}

// ── 5b. A leg leaving the cell, first seen past its pickup: hollow by Core's
// inference (nearer its source), solid once the station says it departed.
{
    const leaving = extra => cellView({ 2: [order(651, 'in_transit', 'AMR-05', 'SMN_01', Object.assign({ order_type: 'store', source_node: 'PLN_02' }, extra))] });
    const away = ['N20_20', 'N10_20', 'N0_20', 'N0_30', 'N0_40', 'SMN_01'];
    for (const [departed, head, label] of [[false, 'map-comet-head-hollow', 'not departed: Core\'s inference reads it fetching (nearer its source)'],
        [true, 'map-comet-head', 'departed (carrying a bin away from the cell): the carry leg, solid']]) {
        const h = await shownPage(leaving({ departed: departed }));
        await drive(h, [pos(651, 'AMR-05', 20, 20, away, Math.PI)], -1, 2000);
        h.vsync(3000);
        eq(h.cometLayer().children[0].attrs['class'], head, 'a robot leaving the cell, seen first 18 m on: ' + label);
    }
}

// ── 5c. A robot coming to the cell, first seen mid-carry (flat bug: it drew a
// hollow comet). RDS's remaining path ends where the robot is going: at the
// cell node it brings to, it is carrying, from its first comet; ending away
// from the cell, the old rule reads it (here fetching: nearer its source).
{
    const coming = cellView({ 3: [order(661, 'in_transit', 'AMR-06', 'PLN_03')] });
    const cases = [
        [['N10_30', 'N20_30', 'N30_30', 'N40_30', 'N40_20', 'PLN_03'], 'map-comet-head', 'its path ends at PLN_03, the node it brings to: carrying, solid from its first comet'],
        [['N10_30', 'N0_30', 'N0_40', 'SMN_01'], 'map-comet-head-hollow', 'its path ends at its source, away from the cell: fetching, hollow'],
    ];
    for (const [path, head, label] of cases) {
        const h = await shownPage(coming);
        // Seen first at (10, 30): 11 m from its source, 31 m from PLN_03, so
        // Core's inference alone reads it fetching.
        h.frame(frameOf([pos(661, 'AMR-06', 10, 30, path, 0)]));
        await h.advance(2000);
        eq(h.cometLayer() ? h.cometLayer().children.length : 0, 0, 'one position: not seen moving, no comet yet');
        h.frame(frameOf([pos(661, 'AMR-06', 11, 30, path, 0)]));
        await h.flush();
        h.vsync(3000);
        eq(h.cometLayer().children[0].attrs['class'], head, 'a robot first seen 11 m past its source: ' + label);
    }
}

// ── 6. The crop: the cell and the robots, not the path; refit only when the set changes.
{
    const h = await shownPage(cellView({ 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03')] }));
    // The robot's first position comes with a path running across the whole
    // plant: the crop is the cell and the robot, and the path's far end is out.
    h.frame(frameOf([pos(501, 'AMR-07', 40, 10, ['N40_10', 'N30_10', 'N20_10', 'N10_10', 'N0_10'], 3.14)]));
    await h.flush();
    const first = viewBox(h), fits0 = h.map().inspect().fits;
    ok(within(first, 40, -10) && !within(first, 0, -10), 'RDS\'s path is not fitted: its far end lies outside the crop it is drawn in');
    h.frame(frameOf([pos(501, 'AMR-07', 40, 10, ['N40_10', 'N40_20', 'PLN_03'], 1.57)]));
    await h.flush();
    eq([h.map().inspect().fits, viewBox(h)], [fits0, first], 'a new path for the same robot leaves the crop as it was');
    // Twenty positions inside the crop: no refit, the base layer built once.
    const base = h.layer('base').children[0];
    for (let i = 0; i < 20; i++) {
        h.frame(frameOf([pos(501, 'AMR-07', 40, 10 + i * 0.4, ['N40_10', 'N40_20', 'PLN_03'], 1.57)]));
        await h.advance(2000);
    }
    eq([h.map().inspect().fits, h.layer('base').children[0] === base], [fits0, true],
        'twenty positions inside the crop: no refit, the aisles drawn once');
    // A second robot joins: the set changed, one refit, both inside.
    await h.setView(cellView({ 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03')], 4: [order(502, 'dispatched', 'AMR-08', 'PLN_04')] }));
    h.frame(frameOf([pos(501, 'AMR-07', 40, 18, ['N40_20', 'PLN_03'], 1.57), pos(502, 'AMR-08', 60, 40, null, 0)]));
    await h.flush();
    const two = viewBox(h);
    eq([h.map().inspect().fits > fits0, within(two, 60, -40), within(two, 40, -18)], [true, true, true],
        'a robot comes: the crop is worked out again and holds both');
    // A robot driving away leaves the crop: worked out again, a few times over
    // a long drive, never on every position. South, so it leaves through the
    // crop's foot: its chip, above it, is never the first thing cut.
    const fits1 = h.map().inspect().fits;
    for (let i = 1; i <= 40; i++) {
        h.frame(frameOf([pos(501, 'AMR-07', 40, 18, ['N40_20', 'PLN_03'], 1.57), pos(502, 'AMR-08', 60, 40 - i, null, -1.57)]));
        await h.advance(2000);
        const vb = viewBox(h), fit = h.map().inspect().fit;
        const foot = vb.minY + vb.h - fit.margins.bottom / fit.s;
        if (!(-(40 - i) <= foot + 1e-9)) { ok(false, 'a robot driving away stays in the crop', 'out at step ' + i); break; }
    }
    const refits = h.map().inspect().fits - fits1;
    ok(refits >= 1 && refits <= 6, 'a robot driving 40 m away: the crop follows it in a few refits, not forty', refits + ' refits');
}

// ── 7. Eight robots: all in view, every chip whole, legible and apart, eight rows.
// eightRobots draws eight robots at spots and holds the busy-cell rule to them.
async function eightRobots(label, spots) {
    const byTile = { 1: [], 2: [], 3: [], 4: [], 5: [] };
    spots.forEach((s, i) => byTile[1 + (i % 5)].push(order(701 + i, i % 3 === 0 ? 'dispatched' : 'in_transit', 'AMR-0' + (i + 1), 'PLN_0' + (1 + (i % 5)))));
    const h = await shownPage(cellView(byTile));
    await drive(h, spots.map((s, i) => pos(701 + i, 'AMR-0' + (i + 1), s[0], s[1], null, 0)), 0.5, 2000);
    const vb = viewBox(h), fit = h.map().inspect().fit;
    eq(robotGroups(h).length, 8, label + ': eight robots drawn');
    eq(spots.every(s => within(vb, s[0] + 0.5, -s[1])), true, label + ': every robot is inside the crop');
    eq(['PLN_01', 'PLN_02', 'PLN_03', 'PLN_04', 'PLN_05'].every(n => within(vb, SCENE.points[n].x, -SCENE.points[n].y)), true,
        label + ': and so is every node of the cell');
    eq(chipRects(h).every(r => within(vb, +r.attrs.x, +r.attrs.y) && within(vb, +r.attrs.x + +r.attrs.width, +r.attrs.y + +r.attrs.height)),
        true, label + ': every name chip lies whole inside the panel: nothing is cut at an edge');
    const rects = chipRects(h).map(r => ({ x: +r.attrs.x, y: +r.attrs.y, w: +r.attrs.width, h: +r.attrs.height }));
    // scene-view's de-collision keeps two chips 1.25 font apart, and a chip is
    // 1.3 font tall: its own rule lets two borders touch by 0.05 font, never
    // two labels. Held to that rule, not to a stricter one.
    const fontS = 12 / fit.s;
    let overlap = 0;
    rects.forEach((a, i) => rects.slice(i + 1).forEach(b => {
        const dx = Math.min(a.x + a.w, b.x + b.w) - Math.max(a.x, b.x), dy = Math.min(a.y + a.h, b.y + b.h) - Math.max(a.y, b.y);
        if (dx > 1e-6 && dy > fontS * 0.05 + 1e-6) overlap++;
    }));
    eq(overlap, 0, label + ': no two chips overlap past scene-view\'s own allowance (its downward de-collision, which the panel reuses)');
    const text = chipGroups(h).map(g => g.children.find(k => k.tagName === 'text'));
    eq(text.every(t => Math.abs(+t.attrs['font-size'] * fit.s - 12) < 1e-9), true, label + ': every chip label is 12 px on screen');
    eq(h.rowEls().length, 8, label + ': eight rows, two to a line (operator.css .os-rp-rows): the panel grows a line per two robots');
    // A chip scene-view did not move sits two robot radii above its robot.
    const moved = chipGroups(h).filter(g => {
        const i = +g.attrs['data-robot'].slice(-1) - 1, t = g.children.find(k => k.tagName === 'text');
        return Math.abs(+t.attrs.y - (-spots[i][1] - 2 * 9 / fit.s)) > 1e-6;
    }).length;
    console.log('  ' + label + ': crop ' + vb.w.toFixed(1) + ' x ' + vb.h.toFixed(1) + ' m at ' + fit.s.toFixed(2) + ' px/m, ' +
        h.map().inspect().fits + ' crops worked out, ' + moved + ' chips moved down');
    return { h: h, moved: moved };
}
await eightRobots('eight across the plant', [[0, 0], [60, 0], [0, 40], [60, 40], [20, 30], [50, 10], [30, 10], [10, 20]]);
// The busy cell (owner, 2026-11-14): eight robots working one screen's
// orders at once, at and around its five nodes. The chips stack downward
// and the crop widens until none is cut.
const busy = await eightRobots('eight at the cell', [[36, 26], [38, 26], [40, 26], [42, 26], [44, 26], [40, 20], [38, 22], [42, 22]]);
ok(busy.moved > 0, 'the busy cell exercises the chips\' de-collision (a chip moved down)', busy.moved + ' moved');
// A jam: eight robots packed 1.5 m apart, two rows of four, queued at the
// cell's way in. Their chips stack past the crop's foot, and the crop widens
// on that side until the stack fits.
const jam = await eightRobots('eight in a jam', [[38, 19], [39.5, 19], [41, 19], [42.5, 19], [38, 20.5], [39.5, 20.5], [41, 20.5], [42.5, 20.5]]);
ok(jam.moved >= 4 && jam.h.map().inspect().fits > 2, 'the jam stacks the chips and widens the crop for them',
    jam.moved + ' moved, ' + jam.h.map().inspect().fits + ' crops');
// A knot no fleet can make, twelve robots inside a metre: their chips cannot
// stack inside the panel at any zoom, and zooming out only packs them
// tighter. The crop stops at the whole plant (here 60 x 40 m, the 'eight
// across the plant' crop) rather than running away.
{
    const byTile = { 1: [], 2: [], 3: [], 4: [], 5: [] };
    for (let i = 0; i < 12; i++) byTile[1 + (i % 5)].push(order(1101 + i, 'in_transit', 'AMR-' + (40 + i), 'PLN_03'));
    const h = await shownPage(cellView(byTile));
    await drive(h, Array.from({ length: 12 }, (_, i) => pos(1101 + i, 'AMR-' + (40 + i), 40 + (i % 4) * 0.25, 20 + Math.floor(i / 4) * 0.25, null, 0)), 0.1, 2000);
    const vb = viewBox(h);
    ok(vb.w <= 112.7 + 0.1 && Array.from({ length: 12 }, (_, i) => within(vb, 40.1 + (i % 4) * 0.25, -(20 + Math.floor(i / 4) * 0.25))).every(Boolean),
        'a knot of twelve: every robot in the crop, and the crop no wider than the whole plant\'s', 'crop ' + vb.w.toFixed(1) + ' m');
}

// ── 8. Twelve moving robots keep their comets; thirteen calm the map (Core's rule).
{
    const byTile = { 1: [], 2: [], 3: [], 4: [], 5: [] };
    for (let i = 0; i < 13; i++) byTile[1 + (i % 5)].push(order(801 + i, 'in_transit', 'AMR-' + (10 + i), 'PLN_03'));
    const twelve = JSON.parse(JSON.stringify(byTile));
    twelve[3] = twelve[3].filter(o => o.id !== 813);
    const h = await shownPage(cellView(twelve));
    // Spread 4 m apart on two rows, as a busy aisle could hold them.
    const list = n => Array.from({ length: n }, (_, i) => pos(801 + i, 'AMR-' + (10 + i), 2 + 4 * i, i % 2 ? 0 : 10, PATH_WEST, 0));
    await drive(h, list(12), 0.5, 2000);
    h.vsync(3000);
    eq(h.cometLayer().children.length, 120, 'twelve moving robots: twelve comets');
    await h.setView(cellView(byTile));
    await drive(h, list(13), 0.5, 2000);
    h.vsync(4000);
    eq(h.cometLayer().children.length, 0, 'thirteen: the busy-floor calm, no comets, as Core\'s map');
}

// ── 9. Position lost: after 3 x interval_ms with no position.
{
    const h = await shownPage(cellView({ 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03')] }));
    await drive(h, [pos(501, 'AMR-07', 12, 0, PATH_WEST, 0)], 1, 2000);
    h.vsync(3000);
    eq(h.cometLayer().children.length, 10, 'driving: its comet');
    const isDisc = k => k.tagName === 'circle' && /(^| )map-robot( |$)/.test(k.attrs['class'] || '');
    await h.advance(5999);
    eq([h.rows(), robotGroups(h)[0].children.some(k => /is-offline/.test(k.attrs['class'] || ''))], [['AMR-07'], false],
        'just under three intervals with no position: not lost yet');
    await h.advance(2);
    const lost = robotGroups(h)[0].children.find(isDisc);
    eq([h.rows(), lost && lost.attrs['class'], h.rowEls()[0].className], [['AMR-07Position lost'], 'map-robot is-offline', 'os-rp-row is-lost'],
        'three intervals missed: the robot fades where it was and its row says Position lost');
    eq([lost && lost.attrs.cx, lost && lost.attrs.cy], ['13', '0'], 'the faded robot stays where it was last seen');
    h.vsync(9000);
    eq([h.cometLayer().children.length, h.panelTimersPending()], [0, 0], 'a lost robot: no comet, and no timer left to wait on');
    h.frame(frameOf([pos(501, 'AMR-07', 14, 0, PATH_WEST, 0)]));
    await h.flush();
    eq(h.rows(), ['AMR-07'], 'a position again: no longer lost');
}

// ── 9b. An order with a robot that never gets a position (flat gap: it was
// never said). Lost 3 x the interval after the panel opened, or after the
// order was handed over if later. The interval is the last one the stream
// said: its first frame, {available}, carries Core's poll (D17), so before any
// position frame the deadline is the {available} frame's interval; after a
// position frame, that frame's. The row says Position lost and names no robot:
// the stored one may be stale (D14).
{
    const h = bootPage({ view: cellView({ 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03')] }) });
    await h.boot();
    // 1500 ms, not Core's 2 s: the deadline can come only from this frame.
    h.frame(available(true, 1500));
    await h.flush();
    const opened = h.map().inspect().openedAt;
    await h.advance(opened + 4500 - h.now());
    eq(h.rowEls().length, 0, 'no position frame yet, three of the {available} frame\'s intervals since the panel opened: not lost yet');
    await h.advance(2);
    const li = h.rowEls()[0];
    eq([h.rowEls().length, li && li.className, li && li.children[1].textContent, li && li.children[2].textContent],
        [1, 'os-rp-row is-lost', '', 'Position lost'], 'past three of them with no position: a row saying Position lost, naming no robot');
    eq([robotGroups(h).length, h.panel().textContent.indexOf('AMR-07')], [0, -1], 'no robot drawn, and the stored robot named nowhere (D14)');
    eq(h.panelTimersPending(), 0, 'lost: no timer left for it');
    // Its robot is heard from: the row is the robot's.
    h.frame({ t: '2026-11-14T12:00:10.000Z', interval_ms: 1000, orders: [pos(501, 'AMR-07', 12, 0, PATH_WEST, 0)] });
    await h.flush();
    eq(h.rows(), ['AMR-07'], 'a first position: the row names the robot the stream named');
    // A second order handed over long after the panel opened, never heard
    // from: counted from its own handover, on the stream's interval (1000 ms).
    await h.setView(cellView({ 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03')], 4: [order(502, 'dispatched', 'AMR-08', 'PLN_04')] }));
    const listed = h.map().inspect().listedAt[502];
    eq(h.rowEls().length, 1, 'handed over a long while after the panel opened: not lost at once');
    const keep = () => h.frame({ t: '2026-11-14T12:00:20.000Z', interval_ms: 1000, orders: [pos(501, 'AMR-07', 12, 0, PATH_WEST, 0)] });
    while (h.now() < listed + 2999) { keep(); await h.advance(Math.min(900, listed + 2999 - h.now())); }
    keep();
    await h.flush();
    eq(h.rows(), ['AMR-07'], 'just under three of the stream\'s intervals since its handover: not lost yet');
    await h.advance(2);
    keep();
    await h.flush();
    eq(h.rows(), ['AMR-07', 'Position lost'], 'past three intervals: its row says Position lost, after the robots drawn');
}
// A stream that has said no interval gives nothing to count with: the panel
// holds no copy of Core's cadence (D17), so it calls nothing lost and arms no
// timer for it.
{
    const h = bootPage({ view: cellView({ 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03')] }) });
    await h.boot();
    h.frame({ available: true });
    await h.flush();
    await h.advance(60000);
    eq([h.visible(), h.rowEls().length, h.panelTimersPending()], [true, 0, 0],
        'no interval said, a minute with no position: shown, nothing called lost, no timer');
}

// ── 10. A tap hides it, and the tap never reaches the control under it.
{
    const h = await shownPage(cellView({ 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03')] }));
    h.frame(frameOf([pos(501, 'AMR-07', 12, 0, PATH_WEST, 0)]));
    await h.flush();
    const grid = h.document.getElementById('os-grid');
    const under = grid.descendants().find(n => (n.listeners.click || []).length > 0);
    ok(!!under, 'the board has a tile button to tap');
    let hits = 0;
    under.listeners.click = under.listeners.click.map(fn => function (ev) { hits++; return fn.call(this, ev); });
    tap(h, under);
    eq([hits, h.visible(), h.openPanelStream()], [0, false, null], 'pointerdown, pointerup, click on the panel: hidden, and the button under it never ran');
    tap(h, under);
    eq(hits, 1, 'with the panel gone, the same press reaches the button (the harness is not vacuous)');
    await h.setView(cellView({ 3: [order(501, 'staged', 'AMR-07', 'PLN_03')] }));
    eq([h.visible(), h.openPanelStream()], [false, null], 'hidden until the next order gets a robot: the same order moving on keeps it hidden');
    eq(h.toggle().attrs['aria-pressed'], 'true', 'the toggle is unchanged by the tap');
    await h.setView(cellView({ 3: [order(501, 'staged', 'AMR-07', 'PLN_03')], 4: [order(502, 'dispatched', 'AMR-08', 'PLN_04')] }));
    h.frame(available(true));
    await h.flush();
    eq(h.visible(), true, 'the next order with a robot shows it again');
    tap(h, under);
    h.toggle().click();
    await h.flush();
    h.frame(available(true));
    await h.flush();
    eq([h.visible(), h.toggle().attrs['aria-pressed']], [true, 'true'], 'tapped away, the toggle shows it again, still enabled');
}

// ── 10b. The close hint (owner H2): the panel's top edge says a tap hides it,
// and a tap on the hint is a tap on the panel.
{
    const h = await shownPage(cellView({ 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03')] }));
    h.frame(frameOf([pos(501, 'AMR-07', 12, 0, PATH_WEST, 0)]));
    await h.flush();
    const hint = h.panel().querySelector('.os-rp-hint');
    ok(!!hint, 'the shown panel carries the close hint');
    eq([h.panel().children[0] === hint, h.panel().children[1].attrs['class'], hint.hidden, hint.textContent],
        [true, 'os-rp-map', false, 'TAP TO HIDE'],
        'on its top edge, above the map, not hidden: the hint says TAP TO HIDE');
    eq([hint.attrs['aria-hidden'], hint.tagName, Object.keys(hint.listeners).length, h.panel().attrs['aria-label']],
        ['true', 'DIV', 0, 'Robots on this screen. Tap to hide.'],
        'not a control of its own (no button, no listener); the panel\'s label says it to a screen reader');
    eq(h.panel().children.filter(c => !/^os-rp-(map|rows)$/.test(c.attrs['class'])).map(c => c.textContent), ['TAP TO HIDE'],
        'the panel\'s only words beside the map and the rows are the hint\'s three: no paragraph');
    const css = fs.readFileSync(path.join(SRC_DIR, 'operator.css'), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
    const rule = (css.match(/\.os-rp-hint\s*\{([^}]*)\}/) || [])[1] || '';
    ok(rule !== '' && !/display\s*:\s*none|visibility\s*:\s*hidden|opacity\s*:\s*0\b|position\s*:\s*(absolute|fixed)/.test(rule),
        'operator.css draws the hint in the panel\'s flow, shown: not display none, not invisible, not laid over the map', rule);
    const grid = h.document.getElementById('os-grid');
    const under = grid.descendants().find(n => (n.listeners.click || []).length > 0);
    let hits = 0;
    under.listeners.click = under.listeners.click.map(fn => function (ev) { hits++; return fn.call(this, ev); });
    // A finger on the hint: pointerdown, pointerup, then click, each sent to
    // what is on top at that moment, as tap() does for the map.
    ['pointerdown', 'pointerup', 'click'].forEach(type => h.dispatch(h.visible() ? hint : under, type));
    eq([hits, h.visible(), h.openPanelStream(), h.toggle().attrs['aria-pressed']], [0, false, null, 'true'],
        'a tap on the hint hides the panel and closes its stream, the button under it never ran, the toggle unchanged');
    await h.setView(cellView({ 3: [order(501, 'staged', 'AMR-07', 'PLN_03')] }));
    eq(h.visible(), false, 'like any tap: hidden until the next order gets a robot');
    await h.setView(cellView({ 3: [order(501, 'staged', 'AMR-07', 'PLN_03')], 4: [order(502, 'dispatched', 'AMR-08', 'PLN_04')] }));
    h.frame(available(true));
    await h.flush();
    eq([h.visible(), h.panel().querySelector('.os-rp-hint') === hint, hint.textContent], [true, true, 'TAP TO HIDE'],
        'shown again: the same hint, built once with the panel, saying the same');
}

// ── 11. The toggle is remembered per screen, "on" and "off" both; a store
// that fails means off, as a screen nobody pressed.
{
    const v = cellView({ 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03')] });
    const h = await shownPage(v);
    h.toggle().click();
    await h.flush();
    eq([h.visible(), h.openPanelStream(), h.toggle().attrs['aria-pressed'], h.stored['shingo.robot-panel.7']],
        [false, null, 'false', 'off'], 'toggled off: hidden, stream closed, and this screen remembers it');
    const again = bootPage({ view: v, stored: h.stored });
    await again.boot();
    eq([again.toggle().attrs['aria-pressed'], again.counts.imports, again.panelStreams().length], ['false', 0, 0],
        'a reload of this screen comes back off and loads nothing');
    const other = bootPage({ view: v, stored: { 'shingo.robot-panel.7': 'on', 'shingo.robot-panel.8': 'off' } });
    await other.boot();
    eq(other.panelStreams().length, 1, 'another screen\'s setting is not this one\'s');
    again.toggle().click();
    await again.flush();
    eq([again.stored['shingo.robot-panel.7'], again.panelStreams().length], ['on', 1], 'toggled on: the store says on, the panel opens');
    const broken = bootPage({ view: v, store: 'throws' });
    await broken.boot();
    eq([broken.toggle().attrs['aria-pressed'], broken.panelStreams().length], ['false', 0], 'an unreadable store: off, as a screen nobody pressed');
    const readonly = bootPage({ view: v, store: 'readonly' });
    await readonly.boot();
    readonly.toggle().click();
    await readonly.flush();
    eq([readonly.toggle().attrs['aria-pressed'], readonly.openPanelStream()], ['false', null], 'a store that cannot be written: the toggle still holds for the page');
}

// ── 11a. OFF UNTIL AN OPERATOR TURNS IT ON (owner, 2026-11-15). A screen
// nobody has pressed ROBOTS on shows no panel, loads nothing, opens no stream
// and sets no timer, its first dispatch included: the zero cost of hidden. The
// button is drawn off (aria-pressed false: its dimmed outline). A press turns it on and remembers "on"; from then
// the panel shows on dispatch and goes on delivery, as built; a tap hides it
// for the orders that had a robot and leaves the toggle and the store as they
// were; another press turns it off and remembers "off".
{
    const v = cellView({ 3: [order(501, 'dispatched', 'AMR-07', 'PLN_03')] });
    const h = bootPage({ view: cellView({ 3: [order(501, 'queued', '', 'PLN_03')] }), fresh: true });
    await h.boot();
    eq([h.toggle().textContent, h.toggle().attrs['aria-pressed'], h.stored['shingo.robot-panel.7']], ['ROBOTS', 'false', undefined],
        'never pressed: the button is drawn off with no order running, and nothing is stored');
    // Off and on are told apart by the button's colours alone (owner: "the
    // colors do enough there"): the off rule changes the outline and the word.
    const css = fs.readFileSync(path.join(SRC_DIR, 'operator.css'), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
    const offRule = (css.match(/\.os-header-btn\.robots\[aria-pressed="false"\]\s*\{([^}]*)\}/) || [])[1] || '';
    ok(/border-color\s*:/.test(offRule) && /(^|[;\s])color\s*:/.test(offRule), 'operator.css draws off apart from on: its outline and its word', offRule);
    h.mark();
    await h.setView(v);
    await h.advance(60000);
    eq([h.visible(), !!h.panel(), h.counts.imports, h.counts.sceneGets, h.panelStreams().length, h.counts.panelTimers],
        [false, false, 0, 0, 0, 0], 'its first dispatch, and a minute on: no panel, no drawing loaded, no scene, no stream, no timer');
    h.toggle().click();
    await h.flush();
    h.frame(available(true));
    await h.flush();
    eq([h.toggle().textContent, h.toggle().attrs['aria-pressed'], h.stored['shingo.robot-panel.7'], h.visible(), h.panelStreams().length],
        ['ROBOTS', 'true', 'on', true, 1], 'pressed: drawn on, "on" remembered, and the order\'s robot shows at once');
    await h.setView(cellView({ 3: [order(501, 'delivered', 'AMR-07', 'PLN_03')] }));
    eq([h.visible(), h.openPanelStream()], [false, null], 'delivered: it goes, as built');
    await h.setView(cellView({ 3: [order(502, 'dispatched', 'AMR-08', 'PLN_03')] }));
    h.frame(available(true));
    await h.flush();
    eq(h.visible(), true, 'the next dispatch: it shows again, as built');
    tap(h, h.document.getElementById('os-grid'));
    await h.flush();
    eq([h.visible(), h.toggle().attrs['aria-pressed'], h.stored['shingo.robot-panel.7']], [false, 'true', 'on'],
        'a tap hides it for this order; the toggle and the store stay on');
    const back = bootPage({ view: v, stored: h.stored });
    await back.boot();
    eq([back.toggle().attrs['aria-pressed'], back.panelStreams().length], ['true', 1], 'a reload of a screen turned on comes back on');
    // A press while the panel is tap-hidden brings it back (as built); the
    // next press turns ROBOTS off.
    h.toggle().click();
    await h.flush();
    h.frame(available(true));
    await h.flush();
    eq([h.visible(), h.toggle().attrs['aria-pressed'], h.stored['shingo.robot-panel.7']], [true, 'true', 'on'],
        'pressed while tap-hidden: shown again, still on');
    h.toggle().click();
    await h.flush();
    eq([h.toggle().attrs['aria-pressed'], h.stored['shingo.robot-panel.7'], h.visible(), h.openPanelStream()], ['false', 'off', false, null],
        'pressed again: drawn off, "off" remembered, nothing shown');
}

// ── 12. {available: false} hides it, and what tries it again.
{
    const v = cellView({ 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03')] });
    const h = bootPage({ view: v });
    await h.boot();
    h.frame(available(false));
    await h.flush();
    eq([h.visible(), h.openPanelStream(), h.panelStreams().length], [false, null, 1], 'no live robot feed here: hidden, its stream closed');
    await h.setView(cellView({ 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03', { eta: '2026-11-14T12:05:00Z' })] }));
    eq(h.panelStreams().length, 1, 'a view refresh that moves no robot order does not try again');
    await h.setView(cellView({ 3: [order(501, 'staged', 'AMR-07', 'PLN_03')] }));
    eq(h.panelStreams().length, 2, 'a robot order that moves on tries again, once');
    h.frame(available(true));
    await h.flush();
    eq(h.visible(), true, 'and shows when the feed answers');
    // Core's link goes down while shown (U9: {available: false} again).
    h.frame(available(false));
    await h.flush();
    eq([h.visible(), h.openPanelStream()], [false, null], 'the Core link down: hidden, its stream closed');
    h.fireBus('connected', { build: 'b1' });
    await h.flush();
    eq(h.panelStreams().length, 3, 'this screen\'s own stream reconnecting tries it again');
}

// ── 12b. A Core outage that ends with no order change: the panel comes back on
// the first view whose Core copies are current again (the board's "as of"
// gone: view.bins.as_of, view.catalog.as_of), with no timer.
{
    const orders = { 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03')] };
    const withFeeds = (binsAsOf, catalogAsOf) => {
        const v = cellView(orders);
        v.bins = binsAsOf ? { known: true, as_of: binsAsOf } : { known: true };
        if (catalogAsOf) v.catalog = { known: true, as_of: catalogAsOf };
        return v;
    };
    const h = await shownPage(withFeeds(null, null));
    eq(h.visible(), true, 'shown, Core copies current');
    // Core goes quiet: the board's copies pass 150 s and it says "as of".
    await h.setView(withFeeds('2026-11-14T11:57:00Z', '2026-11-14T11:57:00Z'));
    eq(h.visible(), true, 'an "as of" alone does not hide the panel: the stream says when Core\'s link is down');
    h.frame(available(false));
    await h.flush();
    eq([h.visible(), h.panelStreams().length], [false, 1], 'Core\'s link down: hidden');
    h.mark();
    await h.setView(withFeeds('2026-11-14T11:57:00Z', '2026-11-14T11:58:00Z'));
    await h.advance(120000);
    eq([h.panelStreams().length, h.counts.panelTimers], [1, 0], 'still stale, two minutes on: no retry and no timer');
    // Core answers: the bins come back current first; the catalog still old.
    await h.setView(withFeeds(null, '2026-11-14T11:58:00Z'));
    eq(h.panelStreams().length, 1, 'one copy current, one still old: still stale, no retry');
    await h.setView(withFeeds(null, null));
    eq(h.panelStreams().length, 2, 'the first view with every copy current tries the panel again, the orders unchanged');
    h.frame(available(true));
    await h.flush();
    eq(h.visible(), true, 'and it is back');
    // Current to current is no flip: a feed refused for another reason (no
    // capability) is not retried by every fresh view.
    h.frame(available(false));
    await h.flush();
    await h.setView(withFeeds(null, null));
    eq(h.panelStreams().length, 2, 'a fresh view after a fresh view is no flip: no retry');
}

// ── 13. A drawing that does not load is tried again only by the same triggers.
{
    const h = bootPage({ view: cellView({ 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03')] }), importFails: true });
    await h.boot();
    h.mark();
    await h.setView(cellView({ 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03', { eta: '2026-11-14T12:05:00Z' })] }));
    await h.setView(cellView({ 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03', { eta: '2026-11-14T12:06:00Z' })] }));
    eq([h.counts.imports, h.panelStreams().length, !!h.panel()], [0, 0, false], 'a lost load is not retried on every view refresh');
    await h.setView(cellView({ 3: [order(501, 'staged', 'AMR-07', 'PLN_03')] }));
    eq(h.counts.imports, 1, 'a robot order that moves on tries the load again, once');
}

// ── 14. Hidden is zero work, for every cause.
async function zeroWork(label, hideIt) {
    const v = cellView({ 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03')] });
    const h = await shownPage(v);
    await drive(h, [pos(501, 'AMR-07', 12, 0, PATH_WEST, 0)], 1, 2000);
    h.vsync(3000);
    ok(h.panelTimersPending() > 0 || h.framesPending() > 0, label + ': the panel was working before it hid');
    await hideIt(h);
    await h.flush();
    h.mark();
    const pendingTimers = h.panelTimersPending(), pendingFrames = h.framesPending();
    // A minute of the screen's life: view refreshes that move no robot
    // order, display frames, the screen coming back into view.
    for (let i = 0; i < 6; i++) {
        h.fireBus('material-refresh', { action: 'nudge' });
        await h.advance(10000);
        h.vsync(4000 + i * 33);
        h.becomeVisible();
    }
    eq({ open: h.openPanelStream() !== null, rafAsks: h.counts.rafAsks, timersArmed: h.counts.panelTimers, timersPending: pendingTimers,
        framesPending: pendingFrames, writes: h.counts.writes, sceneGets: h.counts.sceneGets, imports: h.counts.imports },
    { open: false, rafAsks: 0, timersArmed: 0, timersPending: 0, framesPending: 0, writes: 0, sceneGets: 0, imports: 0 },
    label + ': hidden is zero work (no stream, no frame, no timer, no write, no request)');
    eq(h.visible(), false, label + ': still hidden');
}
await zeroWork('toggle off', async h => { h.toggle().click(); });
await zeroWork('tap', async h => { tap(h, h.document.getElementById('os-grid')); });
await zeroWork('tap on the hint (owner H2)', async h => {
    const hint = h.panel().querySelector('.os-rp-hint');
    ['pointerdown', 'pointerup', 'click'].forEach(type => h.dispatch(h.visible() ? hint : h.document.getElementById('os-grid'), type));
});
await zeroWork('no robot order', async h => {
    await h.setView(cellView({ 3: [order(501, 'delivered', 'AMR-07', 'PLN_03')] }));
});
await zeroWork('Core link down', async h => { h.frame(available(false)); });
await zeroWork('{available: false} first', async h => {
    h.toggle().click();
    await h.flush();
    h.toggle().click();
    await h.flush();
    h.frame(available(false));
});

// ── 14b. A hidden page is a hide cause (lead ruling on U10, 5): the stream
// closed and the timers stopped while the page is hidden, whatever the board
// does meanwhile; the page shown again shows the panel again.
{
    const h = await shownPage(cellView({ 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03')] }));
    await drive(h, [pos(501, 'AMR-07', 12, 0, PATH_WEST, 0)], 1, 2000);
    h.vsync(3000);
    ok(h.panelTimersPending() > 0 && h.cometLayer().children.length === 10, 'the page shown: the panel is working (a comet, the lost timer)');
    h.becomeHidden();
    await h.flush();
    eq([h.openPanelStream(), h.visible(), h.panelTimersPending()], [null, false, 0], 'the page hidden: the stream closed, the panel hidden, no timer');
    h.mark();
    const pendingFrames = h.framesPending();
    for (let i = 0; i < 6; i++) {
        h.fireBus('material-refresh', { action: 'nudge' });
        await h.advance(10000);
        h.vsync(5000 + i * 33);
    }
    await h.setView(cellView({ 3: [order(501, 'in_transit', 'AMR-07', 'PLN_03')], 4: [order(502, 'dispatched', 'AMR-08', 'PLN_04')] }));
    eq({ open: h.openPanelStream() !== null, rafAsks: h.counts.rafAsks, timersArmed: h.counts.panelTimers, framesPending: pendingFrames,
        writes: h.counts.writes, sceneGets: h.counts.sceneGets, imports: h.counts.imports },
    { open: false, rafAsks: 0, timersArmed: 0, framesPending: 0, writes: 0, sceneGets: 0, imports: 0 },
    'a minute with the page hidden, a new robot order included: zero work');
    h.becomeVisible();
    await h.flush();
    eq(h.panelStreams().filter(s => !s.closed).length, 1, 'the page shown again: the panel opens its stream again');
    h.frame(available(true));
    h.frame(frameOf([pos(501, 'AMR-07', 30, 0, PATH_WEST, 0), pos(502, 'AMR-08', 40, 10, null, 0)]));
    await h.flush();
    eq([h.visible(), h.rows()], [true, ['AMR-07', 'AMR-08']], 'and draws again from the next positions');
}

// ── 15. The source: no route of its own, nothing from Core's map, today's comets.
{
    const src = fs.readFileSync(path.join(SRC_DIR, 'operator-robot-map.js'), 'utf8');
    const imports = [...src.matchAll(/^import\s*\{([^}]*)\}\s*from\s*'([^']+)';/gm)].map(m => m[2] + ':' + m[1].replace(/\s+/g, ''));
    eq(imports, [
        '/static/shared/scene-geom.js:makeProjector,headingDeg,isCoord,dist2',
        '/static/shared/scene-net.js:sceneNet',
        '/static/shared/scene-path.js:pathWorld',
        '/static/shared/scene-view.js:createSceneView',
        '/static/shared/comet-layer.js:createCometLayer',
        '/static/shared/robot-motion.js:COMET_LIMIT,PICKUP_RADIUS,movedFrom,movingAt',
        './operator-util.js:formatETA',
    ], 'the drawing imports the shared scene, path, view, comet and motion modules and the ETA words: no route model (sceneRoute), nothing of dashboard-map.js');
    eq(/canvas|getContext|stroke-dasharray|animateMotion|<animate/.test(src), false, 'no canvas, no dash, no browser-driven motion: the comets are the shared layer\'s dots');
    const boot = fs.readFileSync(path.join(SRC_DIR, 'operator-robot-panel.js'), 'utf8');
    eq((boot.match(/\bimport\(/g) || []).length, 1, 'one import(): the drawing, loaded when the panel first shows');
}

// ── 16. Every word the panel shows.
{
    const words = new Set();
    const h = await shownPage(cellView({ 1: [order(901, 'staged', 'AMR-01', 'PLN_01')],
        2: [order(902, 'faulted', 'AMR-02', 'PLN_02', { fault_line: 'Fault · blocked (52200)' })],
        3: [order(903, 'in_transit', 'AMR-03', 'PLN_03', { eta: '2026-11-14T12:00:30Z' })],
        4: [order(904, 'in_transit', 'AMR-04', 'PLN_04', { eta: '2026-11-14T11:55:00Z' })],
        5: [order(905, 'dispatched', 'AMR-05', 'PLN_05', { eta: '2026-11-14T12:10:00Z' })] }));
    h.frame(frameOf([pos(901, 'AMR-01', 36, 26), pos(902, 'AMR-02', 40, 20), pos(903, 'AMR-03', 40, 10), pos(904, 'AMR-04', 30, 0), pos(905, 'AMR-05', 0, 40)]));
    await h.flush();
    h.rowEls().forEach(li => words.add(li.children[2].textContent));
    eq([...words].sort(), ['Arrived', 'Arriving', 'ETA: ~10 min', 'Fault', 'Running late'],
        'the rows\' words are the tile\'s: Arrived, Arriving, ETA: ~N min, Running late, a fault\'s first word');
    await h.advance(6001);
    eq(h.rowEls().map(li => li.children[2].textContent), ['Position lost', 'Position lost', 'Position lost', 'Position lost', 'Position lost'],
        'and Position lost');
    const faulted = robotGroups(h).find(g => g.attrs['data-robot'] === 'AMR-02');
    ok(!!faulted, 'the faulted robot is drawn');
}
{
    const h = await shownPage(cellView({ 2: [order(902, 'faulted', 'AMR-02', 'PLN_02', { fault_line: 'Replanning' })] }));
    h.frame(frameOf([pos(902, 'AMR-02', 30, 0, ['N30_0', 'N40_0', 'N40_10', 'N40_20', 'PLN_02'], 0)]));
    await h.flush();
    const g = robotGroups(h)[0];
    eq(g.children.map(k => k.attrs['class'] || k.tagName), ['map-robot-fault-flare', 'map-robot'],
        'a faulted robot: the fault flare and its disc, as Core draws one');
    const lane = h.layer('routes').children[0];
    eq([h.layer('routes').children.length, lane && lane.attrs['class'], lane && lane.attrs.stroke], [1, 'map-route-frozen', '#8b949e'],
        'its path is a static lane in neutral grey, no comet');
    eq(h.rows(), ['AMR-02Replanning'], 'its row: Replanning');
}

// ── 17. After a Core-side release (the owner's F4: lane gate, table turn,
// vacate, hard release). Core moves the order on and tells the Edge nothing
// (LEAD-NOTES open item 1), so the Edge order stays staged while its robot
// drives, and staged is vendor-tracked: positions flow and the robot is drawn.
// Driving is the positions' word, not the Edge status's: a robot moving on an
// order that is not faulted gets its route and comet, counts for the calm,
// and its row gives the tile's in-transit words, not Arrived.
{
    const released = extra => cellView({ 3: [order(1701, 'staged', 'AMR-17', 'PLN_03', extra)] });
    const eta = new Date(Date.UTC(2026, 10, 14, 12, 3, 30)).toISOString();
    // Staged, driving (two positions apart), with RDS's path to PLN_03.
    const h = await shownPage(released({ eta: eta }));
    await drive(h, [pos(1701, 'AMR-17', 12, 0, PATH_WEST, 0)], 1, 2000);
    h.vsync(3000);
    eq([robotGroups(h).length, robotGroups(h)[0].children.map(k => k.attrs['class'] || k.tagName)], [1, ['map-robot-halo', 'g', 'map-robot-core']],
        'staged and driving: the robot is drawn, moving');
    const comets = h.cometLayer() ? h.cometLayer().children : [];
    eq([comets.length, comets[0] && comets[0].attrs['class'], h.layer('routes').children.length], [10, 'map-comet-head', 0],
        'staged and driving with RDS\'s path: its comet (carrying to PLN_03, solid), no static lane, as an in-transit robot');
    const head = comets[0] && /translate\(([-\d.]+) ([-\d.]+)\)/.exec(comets[0].attrs.transform);
    const half = alongAt([[13, 0], [20, 0], [30, 0], [40, 0], [40, 10], [40, 20], [40, 26]].map(p => proj(p[0], p[1])), 0.5);
    ok(head && Math.abs(+head[1] - half[0]) < 0.01 && Math.abs(+head[2] - half[1]) < 0.01,
        'the comet runs along RDS\'s path from the robot', 'head ' + (head && head[0]) + ' want ' + half);
    eq(h.rows(), ['AMR-17ETA: ~3 min'], 'staged and driving: its row gives the tile\'s in-transit words from the order\'s ETA, not Arrived');
    // No ETA held: no word.
    const n = await shownPage(released({}));
    await drive(n, [pos(1701, 'AMR-17', 12, 0, PATH_WEST, 0)], 1, 2000);
    eq(n.rows(), ['AMR-17'], 'staged and driving with no ETA: no word, not Arrived');
    // Staged and still: as before the fix.
    const s = await shownPage(released({ eta: eta }));
    s.frame(frameOf([pos(1701, 'AMR-17', 40, 26, ['N40_20', 'PLN_03'], 0)]));
    await s.advance(2000);
    s.frame(frameOf([pos(1701, 'AMR-17', 40, 26, ['N40_20', 'PLN_03'], 0)]));
    await s.flush();
    s.vsync(3000);
    eq([robotGroups(s).length, s.cometLayer() ? s.cometLayer().children.length : 0, s.layer('routes').children.length, s.rows()],
        [1, 0, 0, ['AMR-17Arrived']], 'staged and still, with a path: drawn at rest, no comet, no lane, and its row says Arrived');
    // Staged, moving, but RDS has no route left for it: its row keeps Arrived.
    const e = await shownPage(released({ eta: eta }));
    await drive(e, [pos(1701, 'AMR-17', 39, 25, null, 0)], 0.5, 2000);
    e.vsync(3000);
    eq([e.cometLayer() ? e.cometLayer().children.length : 0, e.rows()], [0, ['AMR-17Arrived']],
        'staged, settling with no route left: no comet, and its row says Arrived');
    // Faulted and moving: the static grey lane, no comet, whatever the positions.
    const f = await shownPage(cellView({ 3: [order(1702, 'faulted', 'AMR-18', 'PLN_03', { fault_line: 'Replanning' })] }));
    await drive(f, [pos(1702, 'AMR-18', 12, 0, PATH_WEST, 0)], 1, 2000);
    f.vsync(3000);
    const lane = f.layer('routes').children[0];
    eq([f.cometLayer() ? f.cometLayer().children.length : 0, f.layer('routes').children.length, lane && lane.attrs['class'], lane && lane.attrs.stroke],
        [0, 1, 'map-route-frozen', '#8b949e'], 'faulted and moving: its path a static grey lane, no comet');
    // The calm: twelve in transit and the released one, all driving: thirteen.
    const byTile = { 1: [], 2: [], 3: [order(1701, 'staged', 'AMR-17', 'PLN_03')], 4: [], 5: [] };
    for (let i = 0; i < 12; i++) byTile[1 + (i % 5)].push(order(1710 + i, 'in_transit', 'AMR-' + (30 + i), 'PLN_03'));
    const c = await shownPage(cellView(byTile));
    await drive(c, [pos(1701, 'AMR-17', 2, 20, PATH_WEST, 0)].concat(Array.from({ length: 12 }, (_, i) =>
        pos(1710 + i, 'AMR-' + (30 + i), 2 + 4 * i, i % 2 ? 0 : 10, PATH_WEST, 0))), 0.5, 2000);
    c.vsync(3000);
    eq(c.cometLayer().children.length, 0, 'twelve in transit and one staged, all driving: the staged one counts, thirteen, the calm');
    // And a twelfth driving robot that is staged keeps the comets: it is counted, not dropped.
    const byTile12 = JSON.parse(JSON.stringify(byTile));
    byTile12[2] = byTile12[2].filter(o => o.id !== 1721);
    const c12 = await shownPage(cellView(byTile12));
    await drive(c12, [pos(1701, 'AMR-17', 2, 20, PATH_WEST, 0)].concat(Array.from({ length: 11 }, (_, i) =>
        pos(1710 + i, 'AMR-' + (30 + i), 2 + 4 * i, i % 2 ? 0 : 10, PATH_WEST, 0))), 0.5, 2000);
    c12.vsync(3000);
    eq(c12.cometLayer().children.length, 120, 'eleven in transit and one staged, all driving: twelve comets, the staged one\'s among them');
    // A faulted robot is not driving, moving or not: twelve in transit and one
    // faulted, all moving, keep their twelve comets.
    const byTileF = JSON.parse(JSON.stringify(byTile));
    byTileF[3][0] = order(1701, 'faulted', 'AMR-17', 'PLN_03', { fault_line: 'Replanning' });
    const cf = await shownPage(cellView(byTileF));
    await drive(cf, [pos(1701, 'AMR-17', 2, 20, PATH_WEST, 0)].concat(Array.from({ length: 12 }, (_, i) =>
        pos(1710 + i, 'AMR-' + (30 + i), 2 + 4 * i, i % 2 ? 0 : 10, PATH_WEST, 0))), 0.5, 2000);
    cf.vsync(3000);
    eq(cf.cometLayer().children.length, 120, 'twelve in transit and one faulted, all moving: the faulted one is not counted, twelve comets');
}

finished = true;
console.log('robot panel: ' + passed + ' passed' + (failed ? ', ' + failed + ' failed' : ''));
if (failed) process.exit(1);
})().catch(function (err) { console.error(err && err.stack || err); process.exit(1); });
