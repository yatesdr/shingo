// Pins for the robot map AS DRAWN — the comets, the calm rule, click-to-focus,
// zoom and rotate, the minimap, the intent line and the frozen lane — recorded
// at the tree before the map's machinery moves to shared/, so that a move
// which keeps the look keeps these green and a change of look changes them on
// purpose. Run under plain Node via the Go wrapper dashboard_map_render_test.go.
// Exit 0 on pass, 1 on any assertion failure.
//
// THE PAGE RUNS FOR REAL: dashboard-map.js and the shared scene modules it
// imports (shared/scene-geom.js, scene-net.js, scene-view.js, comet-layer.js,
// each loaded once as a browser's module map loads it: shared-modules.js), its
// own data loads over a stubbed fetch, its own SSE handler (onRobotUpdate), its
// own render() through its own requestAnimationFrame coalescing, its own click
// and wheel listeners. What is stubbed is the DOM (an element here is a little
// tree that records every attribute set on it), the clock (Date.now is a
// number the pin sets, so "moving" is decided, not timed), the frame queue
// (a frame runs when the pin says, with the `now` the pin gives), the page's
// visibility (document.hidden, set by the pin, with the visibilitychange it
// fires) and, for a pin that asks, an IntersectionObserver the pin drives.
//
// THE SCENE is shared/testdata/js/route-scene.json, the one the route vectors
// in dashboard-map.curve.test.js and the Edge routers' pins read. No plant
// data: a synthetic grid in whole metres.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

// The page code under test. This file lives in www/testdata, outside the
// embedded static tree, so no binary carries or serves it
// (TestStaticTree_HoldsNoTestJS); the code it reads stays in www/static.
const SRC_DIR = path.join(__dirname, '..', '..', '..', 'static', 'pages');
const SHARED_DIR = path.join(SRC_DIR, '..', '..', '..', '..', 'shared');
const SM = require(path.join(SHARED_DIR, 'testdata', 'js', 'shared-modules.js'));

let failures = 0;
const pending = [];
function check(name, cond, detail) {
    if (cond) {
        console.log('  ok  ' + name);
    } else {
        failures++;
        console.log('  FAIL ' + name + (detail ? ' — ' + detail : ''));
    }
}

// --- the scene -------------------------------------------------------------

// As GET /api/map/points and GET /api/map/edges serve them (route-scene.js).
const RS = require('./route-scene.js');
const POINT_ROWS = RS.pointRows();
const EDGE_ROWS = RS.edgeRows();

// --- a small recording DOM -------------------------------------------------
//
// An element is a tree node that keeps its tag and every attribute set on it,
// so an assertion talks about what was drawn. Attribute values are stored as
// the strings a browser would hold.

function el(tag) {
    const classes = new Set();
    return {
        tag: tag, attrs: {}, children: [], style: {}, parentNode: null, listeners: {}, textContent: '',
        classList: { add(c) { classes.add(c); }, remove(c) { classes.delete(c); }, contains(c) { return classes.has(c); } },
        setAttribute(k, v) { this.attrs[k] = String(v); },
        getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; },
        removeAttribute(k) { delete this.attrs[k]; },
        appendChild(c) { return this.insertBefore(c, null); },
        insertBefore(c, ref) {
            if (c.parentNode) c.parentNode.removeChild(c);
            const at = ref ? this.children.indexOf(ref) : -1;
            if (at < 0) this.children.push(c); else this.children.splice(at, 0, c);
            c.parentNode = this;
            return c;
        },
        removeChild(c) {
            const at = this.children.indexOf(c);
            if (at >= 0) this.children.splice(at, 1);
            c.parentNode = null;
            return c;
        },
        get firstChild() { return this.children[0] || null; },
        get nextSibling() {
            if (!this.parentNode) return null;
            const sib = this.parentNode.children;
            return sib[sib.indexOf(this) + 1] || null;
        },
        // innerHTML is not parsed: the markup is kept as written, and every
        // write is counted, so a pin can say what a legend shows and how often
        // the page rewrote it.
        html: '', writes: 0, parsed: {},
        set innerHTML(v) {
            this.children.forEach(function (c) { c.parentNode = null; });
            this.children = [];
            this.html = String(v);
            this.writes++;
            this.parsed = {};
        },
        // querySelector answers for markup written with innerHTML: one stand-in
        // per class selector the markup holds, new on every write, as a browser
        // parses new elements each time.
        querySelector(sel) {
            const name = sel.replace(/^\./, '');
            if (this.html.indexOf('class="' + name + '"') < 0) return null;
            return this.parsed[sel] || (this.parsed[sel] = el('button'));
        },
        addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); },
    };
}

function walk(n, fn) { fn(n); n.children.forEach(function (c) { walk(c, fn); }); }
function findAll(root, pred) { const out = []; walk(root, function (n) { if (pred(n)) out.push(n); }); return out; }
function cls(n) { return n.getAttribute('class') || ''; }

// drawList spells an element as its tag and its attributes, sorted by name:
// the look, independent of the order the page happened to set them in.
function drawList(n) {
    return n.tag + ' ' + Object.keys(n.attrs).sort().map(function (k) { return k + '=' + n.attrs[k]; }).join(' ');
}

// --- the harness -------------------------------------------------------------
//
// Same injection technique as dashboard-map.curve.test.js: the module is an
// IIFE, so one __export call goes in just before it closes. Everything the
// module REASSIGNS (view, focusRobot, turnDeg, proj) is handed out through an
// accessor; a reference would go stale on the first render. The comet layer's
// own state is read through its accessors (sig, layer, state).

function load(opts) {
    opts = opts || {};
    // A real epoch, not a small number: a robot never seen moving has
    // lastMoveAt 0, and isMoving compares Date.now() against it, so a clock
    // near zero would read every robot as moving.
    const clock = { t: Date.UTC(2026, 10, 14, 12, 0, 0) };
    let frameSeq = 0;
    let frames = [];
    const stored = Object.assign({}, opts.stored || {});
    const winListeners = {};
    const docListeners = {};
    const data = { orders: [] };
    // opts.io: give the page an IntersectionObserver, recorded so a pin can
    // say when the map scrolls off screen. Without it the page has none.
    const observers = [];
    function IntersectionObserver(cb) {
        const io = { cb: cb, targets: [], observe(t) { this.targets.push(t); } };
        observers.push(io);
        return io;
    }

    const region = el('div');
    region.setAttribute('class', 'map-region');
    const main = el('div');
    region.appendChild(main);
    const host = el('div');
    host.setAttribute('id', 'map-svg-wrap');
    host.clientWidth = 1600;
    host.clientHeight = 900;
    host.getBoundingClientRect = function () { return { left: 0, top: 0, width: 1600, height: 900 }; };
    main.appendChild(host);
    const recenter = el('button');
    const rotate = el('button');
    const ids = { 'map-svg-wrap': host, 'map-recenter': recenter, 'map-rotate': rotate };
    // The header legend, the map key and the rail, as the template holds them.
    ['map-legend', 'map-class-legend', 'rail-motion-list', 'rail-activity-list'].forEach(function (id) {
        ids[id] = el(id === 'map-legend' ? 'span' : id === 'map-class-legend' ? 'div' : 'ul');
    });
    // Every element the page creates, in order, so a pin can count what a
    // render built.
    const made = [];
    // Timers run when the pin says (runTimers), not on a clock.
    let timerSeq = 0;
    let timers = [];

    const ctx = {
        console: console,
        Math: Math, Object: Object, Array: Array, JSON: JSON, String: String, Number: Number,
        isFinite: isFinite, parseInt: parseInt, Promise: Promise,
        Date: { now: function () { return clock.t; } },
        document: {
            // 'loading': the module tail then waits for DOMContentLoaded
            // instead of calling init(), and the pin drives the loads itself.
            readyState: 'loading',
            hidden: false,
            body: {
                dataset: { dashboardId: '7' },
                getAttribute(k) { return k === 'data-dashboard-id' ? '7' : null; },
            },
            documentElement: {},
            getElementById(id) { return ids[id] || null; },
            querySelector(sel) {
                if (sel === '.map-region') return region;
                if (sel === '#map-svg-wrap .map-svg') return host.children[0] || null;
                return null;
            },
            querySelectorAll() { return []; },
            createElementNS(ns, name) { const e = el(name); made.push(e); return e; },
            addEventListener(t, fn) { (docListeners[t] = docListeners[t] || []).push(fn); },
        },
        window: {
            addEventListener(t, fn) { (winListeners[t] = winListeners[t] || []).push(fn); },
            matchMedia(q) { return { matches: !!opts.reduceMotion && /reduce/.test(q) }; },
            IntersectionObserver: opts.io ? IntersectionObserver : undefined,
        },
        localStorage: {
            getItem(k) { return Object.prototype.hasOwnProperty.call(stored, k) ? stored[k] : null; },
            setItem(k, v) { stored[k] = String(v); },
        },
        setInterval() { return 0; },
        clearInterval() {},
        setTimeout(fn) { timerSeq++; timers.push({ id: timerSeq, fn: fn }); return timerSeq; },
        clearTimeout(id) { timers = timers.filter(function (t) { return t.id !== id; }); },
        requestAnimationFrame(fn) { frameSeq++; frames.push({ id: frameSeq, fn: fn }); return frameSeq; },
        cancelAnimationFrame(id) { frames = frames.filter(function (f) { return f.id !== id; }); },
        fetch(url) {
            let body = [];
            // opts.extraPoints: more synthetic points, served after the route scene's.
            if (url === '/api/map/points') body = POINT_ROWS.concat(opts.extraPoints || []);
            else if (url === '/api/map/edges') body = data.edges || EDGE_ROWS; // data.edges: a pin's other scene
            else if (url.indexOf('/api/board/orders?dashboard=') === 0) body = data.orders;
            return Promise.resolve({ ok: true, json() { return Promise.resolve(JSON.parse(JSON.stringify(body))); } });
        },
        // Injected in place of the stripped ES imports.
        onSSE() {},
        setSSEReloadOnBuild() {},
        escapeHtml(s) { return String(s); },
        formatClock() { return ''; },
        serverNow() { return 0; },
    };
    let exported = null;
    ctx.__export = function (o) { exported = o; };
    vm.createContext(ctx);

    const raw = fs.readFileSync(path.join(SRC_DIR, 'dashboard-map.js'), 'utf8');
    const INJECT = '  __export({ render: render,' +
        ' syncCometLayer: function (routes, robotR) { comets.sync(routes, robotR, view); },' +
        ' onRobotUpdate: onRobotUpdate, loadPoints: loadPoints, loadEdges: loadEdges, loadOrders: loadOrders,' +
        ' wireViewControls: wireViewControls, wireCometPause: function () { comets.wirePause(); },' +
        ' view: function () { return view; }, viewTarget: function () { return viewTarget; },' +
        ' fullPlantView: function () { return fullPlantView; },' +
        ' focusRobot: function () { return focusRobot; }, turnDeg: function () { return turnDeg; },' +
        ' userView: function () { return userView; },' +
        ' lastCometSig: function () { return comets.sig(); }, cometLayer: function () { return comets.layer(); },' +
        ' cometState: function () { return comets.state(); }, minimapEl: function () { return minimapEl; },' +
        ' proj: function (x, y) { return proj(x, y); }, graphScale: function () { return graphScale; },' +
        ' sceneView: function () { return sceneView; },' +
        ' STATUS_COLOR: STATUS_COLOR, IDLE_MOVE_COLOR: IDLE_MOVE_COLOR });\n})();\n';
    const src = raw.replace(/\}\)\(\);\s*$/, INJECT);
    if (src === raw) {
        throw new Error('dashboard-map.js no longer ends in the "})();" IIFE close this ' +
            'harness injects before; update the injection in dashboard-map.render.test.js');
    }
    // The page's imports of the shared scene modules are bound to them, loaded
    // once into this realm; its other imports (dom.js, clock.js, bus.js) are
    // the stand-ins above.
    SM.runPage(ctx, src, SM.realm(ctx), null, 'dashboard-map.js');
    if (!exported) throw new Error('__export was not reached inside dashboard-map.js');

    const m = exported;
    m.clock = clock;
    m.host = host;
    m.region = region;
    m.ids = ids;
    m.stored = stored;
    m.winListeners = winListeners;
    m.data = data;
    m.doc = ctx.document;
    // fireDoc hands the page's document listeners an event, as the browser would.
    m.fireDoc = function (type) { (docListeners[type] || []).forEach(function (fn) { fn({ type: type }); }); };
    m.observers = observers;
    // frame runs the frames queued so far, at `now`. A frame queued while one
    // runs waits for the next call, as a browser's would.
    m.frame = function (now) {
        const due = frames;
        frames = [];
        due.forEach(function (f) { f.fn(now); });
        return due.length;
    };
    m.pendingFrames = function () { return frames.length; };
    // runTimers runs the timers set so far (the wheel's settle, for one).
    m.runTimers = function () {
        const due = timers;
        timers = [];
        due.forEach(function (t) { t.fn(); });
        return due.length;
    };
    // made: every element the page has created, in order. madeSince(n) is
    // the ones after the first n.
    m.made = made;
    m.madeSince = function (n) { return made.slice(n); };
    // scene is the drawn map: the <svg> render() put in the host.
    m.scene = function () { return host.children[0] || null; };
    return m;
}

// boot loads the scene through the page's own loads, then the orders.
async function boot(opts) {
    const m = load(opts);
    await m.loadPoints();
    await m.loadEdges();
    return m;
}

async function setOrders(m, orders) {
    m.data.orders = orders;
    await m.loadOrders();
}

// drive feeds each robot twice, `from` then `to`, a second apart, through the
// page's SSE handler: a displacement past the jitter threshold is motion, and
// a robot fed the same place twice is standing still.
function drive(m, robots) {
    m.onRobotUpdate(robots.map(function (r) { return frameOf(r.id, r.from); }));
    m.clock.t += 1000;
    m.onRobotUpdate(robots.map(function (r) { return frameOf(r.id, r.to || r.from, r.angle); }));
}
function frameOf(id, at, angle) {
    return { vehicle_id: id, state: 'busy', x: at[0], y: at[1], angle: angle || 0, charging: false, connected: true };
}

// paint runs the scheduled render, the way the page's coalescing runs it.
function paint(m) { m.frame(m.clock.t); }

function order(id, status, robot, src, dst, carrying) {
    return {
        order_id: id, robot_id: robot, source_node: src, delivery_node: dst, status: status,
        carrying: !!carrying, payload_code: '', station_id: '', created_at: '', current_station: '',
    };
}

// comets reads the comet layer as the page built it: one entry per comet, its
// ten dots in layer order.
function comets(m) {
    const layer = m.cometLayer();
    const kids = layer ? layer.children : [];
    const out = [];
    for (let i = 0; i < kids.length; i += 10) out.push(kids.slice(i, i + 10));
    return out;
}

// membership is the comet SET signature without its frame and scale parts.
function membership(m) { return m.lastCometSig().split('|').slice(2).join('|'); }

// robotGroup finds the [data-robot] glyph group render() drew for a robot.
function robotGroup(m, id) {
    return findAll(m.scene(), function (n) { return n.getAttribute('data-robot') === id && n.tag === 'g'; })[0] || null;
}

// click hands the host's delegated listener a click on `target`.
function click(m, target) {
    (m.host.listeners.click || []).forEach(function (fn) { fn({ type: 'click', target: target }); });
}

function near(a, b) { return Math.abs(a - b) < 1e-9; }
function sameView(a, b) {
    return !!a && !!b && near(a.minX, b.minX) && near(a.minY, b.minY) && near(a.w, b.w) && near(a.h, b.h);
}
function viewNums(v) { return v ? [v.minX, v.minY, v.w, v.h].map(function (x) { return +x.toFixed(6); }) : null; }

function test(fn) { pending.push(fn); }

// ═══ 2. the comet set ═══════════════════════════════════════════════════════

test(async function cometSetSignature() {
    console.log('comet set');
    const m = await boot();
    await setOrders(m, [
        order(9, 'in_transit', 'AMR-A', 'SMN_05', 'PLN_02', true),
        order(10, 'dispatched', 'AMR-B', 'LM941', 'LM942', false),
        order(11, 'in_transit', 'AMR-C', 'LM932', 'PLN_02', true),
        order(12, 'staged', 'AMR-D', 'LM911', 'LM912', false),
    ]);
    drive(m, [
        { id: 'AMR-A', from: [0.3, 0.2], to: [0.5, 0.3] },
        // 4.5 m short of its source, nearer the source than the delivery: fetch.
        { id: 'AMR-B', from: [25, 10], to: [25.5, 10] },
        { id: 'AMR-C', from: [14, 1] },                       // standing still
        { id: 'AMR-E', from: [8, 0], to: [8.5, 0] },          // moving, no order
    ]);
    paint(m);
    const c = comets(m);
    check('two comets: the two moving robots on working orders', c.length === 2, 'got ' + c.length);
    check('ten dots each: an arrow head and nine circles',
        c.every(function (d) { return d.length === 10 && d[0].tag === 'polygon' && d.slice(1).every(function (x) { return x.tag === 'circle'; }); }),
        JSON.stringify(c.map(function (d) { return d.map(function (x) { return x.tag; }); })));
    // Sorted by the signature STRING, so order 10 sorts before order 9.
    check('the set is AMR-B\'s fetch then AMR-A\'s carry, ordered by signature string',
        membership(m) === '10>fetch>dispatched:#4f9bff;9>carry>in_transit:#34c3e0', membership(m));
    check('a fetch comet is hollow, a carry comet solid',
        cls(c[0][0]) === 'map-comet-head-hollow' && c[0][1].attrs.fill === 'none' &&
        cls(c[1][0]) === 'map-comet-head' && c[1][1].attrs.fill === '#34c3e0',
        drawList(c[0][0]) + ' | ' + drawList(c[1][0]));
    const route = m.cometState()[1].pts.map(function (p) { return p.join(','); }).join(' ');
    check('AMR-A\'s comet runs SMN_05 -> LM931 -> LM932 -> PLN_02, the one-way stretch backwards',
        route === '0.5,-0.3 0,0 2,-1 14,-1 16,0 16,0', route);

    // Motion alone refreshes geometry; the dots are the same elements.
    const before = m.cometLayer().children.slice();
    const sig = m.lastCometSig();
    drive(m, [
        { id: 'AMR-A', from: [0.5, 0.3], to: [0.7, 0.4] },
        { id: 'AMR-B', from: [25.5, 10], to: [26, 10] },
    ]);
    paint(m);
    const after = m.cometLayer().children;
    check('a move keeps the signature and every dot element',
        m.lastCometSig() === sig && after.length === before.length &&
        after.every(function (x, i) { return x === before[i]; }), m.lastCometSig() + ' vs ' + sig);
    check('...and the comet\'s geometry follows the robot',
        m.cometState()[1].pts[0].join(',') === '0.7,-0.4', JSON.stringify(m.cometState()[1].pts[0]));

    // AMR-B reaches its source: the leg latches to carry and the set rebuilds.
    drive(m, [
        { id: 'AMR-A', from: [0.7, 0.4], to: [0.9, 0.5] },
        { id: 'AMR-B', from: [26, 10], to: [27, 10] },
    ]);
    paint(m);
    const flipped = comets(m);
    check('reaching the source flips AMR-B to carry: a new set, solid dots',
        membership(m) === '10>carry>dispatched:#4f9bff;9>carry>in_transit:#34c3e0' &&
        cls(flipped[0][0]) === 'map-comet-head' && flipped[0][0] !== c[0][0],
        membership(m));

    // A status change is a new set too.
    await setOrders(m, [
        order(9, 'reshuffling', 'AMR-A', 'SMN_05', 'PLN_02', true),
        order(10, 'dispatched', 'AMR-B', 'LM941', 'LM942', false),
    ]);
    const held = m.cometLayer().children.slice();
    drive(m, [
        { id: 'AMR-A', from: [0.9, 0.5], to: [1.1, 0.6] },
        { id: 'AMR-B', from: [27, 10], to: [27.2, 10] },
    ]);
    paint(m);
    check('a status change rebuilds the set in the new status colour',
        membership(m) === '10>carry>dispatched:#4f9bff;9>carry>reshuffling:#df6fb4' &&
        m.cometLayer().children[0] !== held[0], membership(m));
});

// ═══ 3. the 12-robot calm rule ══════════════════════════════════════════════

function fleet(n) {
    const out = [];
    for (let i = 1; i <= n; i++) {
        const id = 'AMR-' + String(i).padStart(2, '0');
        out.push({ id: id, from: [i, 2], to: [i + 0.2, 2] });
    }
    return out;
}
function workOrders(robots, base) {
    return robots.map(function (r, i) { return order(base + i, 'in_transit', r.id, 'SMN_05', 'PLN_02', true); });
}

test(async function twelveCometsThenNone() {
    console.log('calm rule');
    const twelve = await boot();
    const r12 = fleet(12);
    await setOrders(twelve, workOrders(r12, 101));
    drive(twelve, r12);
    paint(twelve);
    check('12 moving robots on working orders: 12 comets', comets(twelve).length === 12, 'got ' + comets(twelve).length);

    const thirteen = await boot();
    const r13 = fleet(13);
    await setOrders(thirteen, workOrders(r13, 101).concat([order(120, 'staged', 'AMR-07', 'LM911', 'LM912', false)]));
    drive(thirteen, r13);
    paint(thirteen);
    check('13: no comets at all', comets(thirteen).length === 0, 'got ' + comets(thirteen).length);
    check('13: and a staged order draws no intent line',
        findAll(thirteen.scene(), function (n) { return cls(n) === 'map-route-intent'; }).length === 0);

    // Focus overrides the calm: the focused robot's comet, and only it.
    click(thirteen, robotGroup(thirteen, 'AMR-07').children[0]);
    paint(thirteen);
    check('13 with AMR-07 focused: one comet', comets(thirteen).length === 1 &&
        membership(thirteen) === '107>carry>in_transit:#34c3e0', membership(thirteen));
    check('...and the focused robot\'s staged order draws its intent line',
        findAll(thirteen.scene(), function (n) { return cls(n) === 'map-route-intent'; }).length === 1);
});

test(async function countedPerOrderNotPerRobot() {
    const m = await boot();
    const r7 = fleet(7);
    // Two working orders on each of seven robots: 14 moving orders.
    await setOrders(m, workOrders(r7, 201).concat(workOrders(r7, 301)));
    drive(m, r7);
    paint(m);
    check('7 robots carrying 14 moving orders is past the limit: no comets', comets(m).length === 0,
        'got ' + comets(m).length);
});

// ═══ 4. the comet's look, frame by frame ════════════════════════════════════

// One comet on a polyline drawn straight into the layer, robotR 1: the look
// as a draw list, every element's attributes, at a fixed time.
async function oneComet(leg, opts) {
    const m = await boot(opts);
    m.syncCometLayer([{ pts: [[0, 0], [10, 0], [10, 5]], color: '#34c3e0', leg: leg, sig: 'k>' + leg }], 1);
    return m;
}
function layerList(m) { return m.cometLayer().children.map(drawList); }

const HEAD_PTS = 'points=0.85,0 -0.5,0.5 -0.175,0 -0.5,-0.5';
const RADII = ['0.372', '0.344', '0.316', '0.288', '0.260', '0.232', '0.204', '0.176', '0.148'];

test(async function theCarryCometAtHalfLap() {
    console.log('comet look');
    const m = await oneComet('carry');
    check('built at opacity 0, before the first frame',
        m.cometLayer().children.every(function (x) { return x.attrs.opacity === '0'; }));
    m.frame(3000);
    const want = ['polygon class=map-comet-head fill=#8fdeee opacity=1.000 ' + HEAD_PTS +
        ' transform=translate(7.50 0.00) rotate(0.0)'];
    ['7.26', '7.02', '6.78', '6.54', '6.30', '6.06', '5.82', '5.58', '5.34'].forEach(function (x, i) {
        want.push('circle cx=' + x + ' cy=0.00 fill=#34c3e0 opacity=' + (0.9 - i * 0.1).toFixed(3) + ' r=' + RADII[i]);
    });
    const got = layerList(m);
    check('carry at half a lap: a lightened solid head, nine solid dots tapering and fading behind it',
        JSON.stringify(got) === JSON.stringify(want), '\n' + got.join('\n'));
});

test(async function theFetchCometAtHalfLap() {
    const m = await oneComet('fetch');
    m.frame(3000);
    const want = ['polygon class=map-comet-head-hollow fill=none opacity=1.000 ' + HEAD_PTS +
        ' stroke=#8fdeee stroke-width=0.140 transform=translate(7.50 0.00) rotate(0.0)'];
    ['7.26', '7.02', '6.78', '6.54', '6.30', '6.06', '5.82', '5.58', '5.34'].forEach(function (x, i) {
        want.push('circle cx=' + x + ' cy=0.00 fill=none opacity=' + (0.9 - i * 0.1).toFixed(3) + ' r=' + RADII[i] +
            ' stroke=#34c3e0 stroke-width=0.140');
    });
    const got = layerList(m);
    check('fetch at half a lap: the same comet, hollow',
        JSON.stringify(got) === JSON.stringify(want), '\n' + got.join('\n'));
});

test(async function theHeadTurnsTheCorner() {
    const m = await oneComet('carry');
    m.frame(4500);
    const got = m.cometLayer().children.map(function (x) {
        return x.tag === 'polygon' ? x.attrs.transform : x.attrs.cx + ',' + x.attrs.cy;
    });
    const want = ['translate(10.00 1.25) rotate(90.0)',
        '10.00,1.01', '10.00,0.77', '10.00,0.53', '10.00,0.29', '10.00,0.05',
        '9.81,0.00', '9.57,0.00', '9.33,0.00', '9.09,0.00'];
    check('three quarters round: the head is past the corner and heads down the second leg',
        JSON.stringify(got) === JSON.stringify(want), JSON.stringify(got));
});

test(async function theEndsFade() {
    const m = await oneComet('carry');
    m.frame(300);
    const op = m.cometLayer().children.map(function (x) { return x.attrs.opacity; });
    const want = ['0.833', '0.510', '0.240', '0.023', '0.084', '0.150', '0.184', '0.186', '0.156', '0.094'];
    check('just after the wrap: the head fades in from the robot, the tail fades out at the far end',
        JSON.stringify(op) === JSON.stringify(want), JSON.stringify(op));
    m.frame(5700);
    check('near the end of the lap the head fades out', m.cometLayer().children[0].attrs.opacity === '0.500',
        m.cometLayer().children[0].attrs.opacity);
    check('the loop re-arms itself every frame while a comet exists', m.pendingFrames() === 1,
        'pending ' + m.pendingFrames());
});

// ═══ 4b. the comet loop's cost ══════════════════════════════════════════════
//
// The loop runs on every display frame but DRAWS at most 30 a second, and not
// at all while nobody can see it. A drawn frame sets the head's transform
// exactly once, so counting those sets counts the frames that did work.

function countHeadDraws(m) {
    const head = m.cometLayer().children[0];
    const set = head.setAttribute;
    let n = 0;
    head.setAttribute = function (k, v) { if (k === 'transform') n++; return set.call(this, k, v); };
    return function () { return n; };
}

// One second of display frames at hz, timestamps t0 + i * 1000 / hz.
function runDisplay(m, hz, t0) {
    for (let i = 0; i < hz; i++) m.frame(t0 + i * 1000 / hz);
}

// What a comet built at the same time draws on its very first frame at `now`:
// the look at that instant, with no history.
async function drawnFreshAt(now) {
    const ref = await oneComet('carry');
    ref.frame(now);
    return layerList(ref);
}

test(async function thirtyDrawnFramesASecond() {
    console.log('comet loop cost');
    for (const hz of [60, 120]) {
        const m = await oneComet('carry');
        const draws = countHeadDraws(m);
        runDisplay(m, hz, 0);
        check('a ' + hz + ' Hz display: 30 of its ' + hz + ' frames draw', draws() === 30, draws() + ' drawn');
        check('...and the loop still re-arms on every display frame', m.pendingFrames() === 1,
            'pending ' + m.pendingFrames());
    }
    const m = await oneComet('carry');
    const draws = countHeadDraws(m);
    runDisplay(m, 144, 0);
    check('a 144 Hz display: never more than 30 a second', draws() <= 30 && draws() >= 28, draws() + ' drawn');
});

test(async function aSkippedFrameIsSkippedNotSlowed() {
    const m = await oneComet('carry');
    m.frame(2990);
    const at2990 = layerList(m);
    m.frame(3000);
    check('a frame 10 ms after a drawn one draws nothing', JSON.stringify(layerList(m)) === JSON.stringify(at2990));
    m.frame(3030);
    const fresh = await drawnFreshAt(3030);
    check('the next drawn frame puts every dot where the clock says, as a first frame at that time would',
        JSON.stringify(layerList(m)) === JSON.stringify(fresh), '\n' + layerList(m).join('\n'));

    const run = await oneComet('carry');
    for (let i = 0; i <= 180; i++) run.frame(i * 1000 / 60);
    check('three seconds at 60 Hz, half of them skipped, ends on the half-lap look',
        JSON.stringify(layerList(run)) === JSON.stringify(await drawnFreshAt(3000)), '\n' + layerList(run).join('\n'));
});

const SAME_SET = [{ pts: [[0, 0], [10, 0], [10, 5]], color: '#34c3e0', leg: 'carry', sig: 'k>carry' }];

test(async function noCometWorkWhileHidden() {
    const half = await drawnFreshAt(3000);
    const m = await oneComet('carry');
    m.wireCometPause();
    m.frame(1000);
    const shown = layerList(m);
    m.doc.hidden = true;
    m.fireDoc('visibilitychange');
    check('hidden: the loop stops, no frame queued', m.pendingFrames() === 0, 'pending ' + m.pendingFrames());
    m.syncCometLayer(SAME_SET, 1);
    check('...a render while hidden arms nothing and moves no dot',
        m.pendingFrames() === 0 && JSON.stringify(layerList(m)) === JSON.stringify(shown), 'pending ' + m.pendingFrames());
    m.doc.hidden = false;
    m.fireDoc('visibilitychange');
    check('shown again: one frame queued', m.pendingFrames() === 1, 'pending ' + m.pendingFrames());
    m.frame(3000);
    check('...and it draws where the clock says, not where the comet stopped',
        JSON.stringify(layerList(m)) === JSON.stringify(half), '\n' + layerList(m).join('\n'));
    m.doc.hidden = true;
    m.frame(3100);
    check('a frame that lands after the tab hides, before the event, draws nothing and does not re-arm',
        m.pendingFrames() === 0 && JSON.stringify(layerList(m)) === JSON.stringify(half), 'pending ' + m.pendingFrames());
});

test(async function noCometWorkWhileOffScreen() {
    const half = await drawnFreshAt(3000);
    const o = await oneComet('carry', { io: true });
    o.wireCometPause();
    const io = o.observers[0];
    check('the page watches the map host for leaving the screen',
        o.observers.length === 1 && !!io && io.targets[0] === o.host);
    if (!io) return;
    o.frame(1000);
    io.cb([{ isIntersecting: false }]);
    check('off screen: the loop stops', o.pendingFrames() === 0, 'pending ' + o.pendingFrames());
    o.syncCometLayer(SAME_SET, 1);
    check('...and a render arms nothing', o.pendingFrames() === 0, 'pending ' + o.pendingFrames());
    io.cb([{ isIntersecting: true }]);
    check('back on screen: one frame queued', o.pendingFrames() === 1, 'pending ' + o.pendingFrames());
    o.frame(3000);
    check('...drawing where the clock says', JSON.stringify(layerList(o)) === JSON.stringify(half));
});

test(async function noCometWorkWithNoComets() {
    const e = await oneComet('carry');
    e.syncCometLayer([], 1);
    check('no comets: the queued frame is cancelled, not run to find nothing',
        e.pendingFrames() === 0 && e.cometLayer().children.length === 0, 'pending ' + e.pendingFrames());
});

// ═══ 5. click-to-focus ══════════════════════════════════════════════════════

async function twoWorking(opts) {
    const m = await boot(opts);
    await setOrders(m, [
        order(9, 'in_transit', 'AMR-A', 'SMN_05', 'PLN_02', true),
        order(10, 'dispatched', 'AMR-B', 'LM941', 'LM942', false),
        order(13, 'blocked', 'AMR-F', 'SMN_05', 'PLN_02', true),
        order(14, 'staged', 'AMR-G', 'LM911', 'LM912', false),
    ]);
    drive(m, [
        { id: 'AMR-A', from: [0.3, 0.2], to: [0.5, 0.3] },
        { id: 'AMR-B', from: [25, 10], to: [25.5, 10] },
        { id: 'AMR-F', from: [0, 0] },
    ]);
    paint(m);
    return m;
}
function frozen(m) { return findAll(m.scene(), function (n) { return cls(n) === 'map-route-frozen'; }); }
function intents(m) { return findAll(m.scene(), function (n) { return cls(n) === 'map-route-intent'; }); }

test(async function clickToFocus() {
    console.log('click to focus');
    const m = await twoWorking();
    check('one delegated click listener on the map host', (m.host.listeners.click || []).length === 1);
    const view0 = Object.assign({}, m.view());
    check('unfocused: both comets, the frozen lane at full weight, the staged intent line',
        comets(m).length === 2 && frozen(m).length === 1 && frozen(m)[0].attrs['stroke-opacity'] === '0.4' &&
        intents(m).length === 1);

    // A click on the chevron, two levels inside AMR-A's group.
    const chevron = findAll(robotGroup(m, 'AMR-A'), function (n) { return n.tag === 'polygon'; })[0];
    click(m, chevron);
    paint(m);
    check('a click inside a robot\'s glyph focuses it', m.focusRobot() === 'AMR-A', m.focusRobot());
    const ring = findAll(robotGroup(m, 'AMR-A'), function (n) { return cls(n) === 'map-robot-focus'; });
    const halo = findAll(robotGroup(m, 'AMR-A'), function (n) { return cls(n) === 'map-robot-halo'; });
    check('the focused robot wears a ring 1.8 robot radii wide (the halo is 1.35)',
        ring.length === 1 && halo.length === 1 &&
        Math.abs(Number(ring[0].attrs.r) / Number(halo[0].attrs.r) - 1.8 / 1.35) < 1e-9,
        ring.length + ' ring(s)');
    check('focus leaves the camera where it was', sameView(m.view(), view0),
        JSON.stringify(viewNums(m.view())) + ' vs ' + JSON.stringify(viewNums(view0)));
    check('the other robot\'s comet is REMOVED, not dimmed: one comet, the focused robot\'s',
        comets(m).length === 1 && membership(m) === '9>carry>in_transit:#34c3e0', membership(m));
    check('the other robot\'s frozen lane is dimmed', frozen(m).length === 1 &&
        frozen(m)[0].attrs['stroke-opacity'] === '0.22', frozen(m).map(drawList).join(' | '));
    check('another robot\'s staged order draws no intent line at all, dimmed or not', intents(m).length === 0);

    // The name chip is a [data-robot] group of its own: a click on its text.
    const chip = findAll(m.scene(), function (n) {
        return n.getAttribute('data-robot') === 'AMR-A' && n.children.some(function (k) { return k.tag === 'text'; });
    })[0];
    click(m, chip.children.filter(function (k) { return k.tag === 'text'; })[0]);
    paint(m);
    check('a click on the focused robot\'s name chip clears the focus', m.focusRobot() === null, m.focusRobot());
    check('...and both comets come back', comets(m).length === 2);

    click(m, robotGroup(m, 'AMR-B'));
    paint(m);
    click(m, m.scene());
    paint(m);
    check('a click on empty floor clears the focus', m.focusRobot() === null, m.focusRobot());
});

test(async function aDragDoesNotFocus() {
    const m = await twoWorking();
    m.wireViewControls();
    m.host.listeners.mousedown[0]({ button: 0, clientX: 100, clientY: 100 });
    m.winListeners.mousemove[0]({ clientX: 110, clientY: 100 });
    m.winListeners.mouseup[0]({});
    click(m, robotGroup(m, 'AMR-A'));
    check('the click that ends a drag is swallowed', m.focusRobot() === null, m.focusRobot());
    click(m, robotGroup(m, 'AMR-A'));
    check('the next click focuses', m.focusRobot() === 'AMR-A', m.focusRobot());
});

// ═══ 6. zoom and rotate ═════════════════════════════════════════════════════

// The scene's points span x 0..34, y -1..10: wider than tall, so it opens
// unturned. The container is 1600 x 900.
test(async function theIdleViewPerTurn() {
    console.log('zoom and rotate');
    const m = await boot();
    m.wireViewControls();
    drive(m, [{ id: 'AMR-A', from: [3, 2], to: [3.5, 2] }]);
    paint(m);
    check('a wider-than-tall plant opens unturned: proj is [x, -y]',
        m.turnDeg() === 0 && m.proj(3, 7).join(',') === '3,-7', m.turnDeg() + ' ' + m.proj(3, 7));
    check('idle: the whole plant padded 5% of its long side', JSON.stringify(viewNums(m.fullPlantView())) ===
        JSON.stringify([-1.7, -11.7, 37.4, 14.4]), JSON.stringify(viewNums(m.fullPlantView())));
    check('...and widened to the container\'s 16:9, centred',
        JSON.stringify(viewNums(m.view())) === JSON.stringify([-1.7, -15.01875, 37.4, 21.0375]),
        JSON.stringify(viewNums(m.view())));
    check('the drawn map carries that view as its viewBox',
        m.scene().attrs.viewBox.split(' ').map(Number).every(function (x, i) {
            return near(x, [m.view().minX, m.view().minY, m.view().w, m.view().h][i]);
        }), m.scene().attrs.viewBox);
    // The chevron's own group, inside the robot's [data-robot] group.
    const chev = function () {
        return robotGroup(m, 'AMR-A').children.filter(function (n) { return n.tag === 'g'; })[0].attrs.transform;
    };
    check('a moving robot heading 0 rad draws its chevron at rotate(0)', /rotate\(0\)$/.test(chev()), chev());

    const turns = [
        [90, '7,3', [-28.744444, -1.7, 66.488889, 37.4], 'rotate(90)'],
        [180, '-3,7', [-35.7, -6.01875, 37.4, 21.0375], 'rotate(180)'],
        [270, '-7,-3', [-37.744444, -35.7, 66.488889, 37.4], 'rotate(270)'],
        [0, '3,-7', [-1.7, -15.01875, 37.4, 21.0375], 'rotate(0)'],
    ];
    turns.forEach(function (t) {
        m.ids['map-rotate'].listeners.click[0]({});
        check('Rotate turns a quarter clockwise to ' + t[0] + ': proj(3, 7) = [' + t[1] + ']',
            m.turnDeg() === t[0] && m.proj(3, 7).join(',') === t[1], m.turnDeg() + ' ' + m.proj(3, 7));
        check('...remembered on this screen', m.stored['shingo.map-turn.7'] === String(t[0]),
            JSON.stringify(m.stored));
        check('...the idle view re-frames to the turned plant',
            JSON.stringify(viewNums(m.view())) === JSON.stringify(t[2]), JSON.stringify(viewNums(m.view())));
        check('...and the chevron turns with it: ' + t[3], chev().slice(-t[3].length) === t[3], chev());
    });
});

test(async function aRememberedTurnIsUsed() {
    const m = await boot({ stored: { 'shingo.map-turn.7': '180' } });
    drive(m, [{ id: 'AMR-A', from: [3, 2], to: [3.5, 2] }]);
    paint(m);
    check('a turn saved on this screen wins over the plant\'s shape', m.turnDeg() === 180, String(m.turnDeg()));
});

test(async function wheelZoomAndRecenter() {
    const m = await boot();
    m.wireViewControls();
    drive(m, [{ id: 'AMR-A', from: [3, 2], to: [3.5, 2] }]);
    paint(m);
    const v0 = Object.assign({}, m.view());
    const under = function (v) { return [v.minX + (400 / 1600) * v.w, v.minY + (300 / 900) * v.h]; };
    m.host.listeners.wheel[0]({ deltaY: -100, clientX: 400, clientY: 300, preventDefault() {} });
    const v1 = m.view();
    check('a wheel notch in zooms by 1/1.12', near(v1.w, v0.w / 1.12) && near(v1.h, v0.h / 1.12),
        JSON.stringify(viewNums(v1)));
    check('...keeping the floor under the cursor where it was',
        near(under(v1)[0], under(v0)[0]) && near(under(v1)[1], under(v0)[1]),
        JSON.stringify([under(v0), under(v1)]));
    check('...and latching the manual view', m.userView() === true);
    const vz = Object.assign({}, m.view());
    await setOrders(m, [order(9, 'in_transit', 'AMR-A', 'SMN_05', 'PLN_02', true)]);
    drive(m, [{ id: 'AMR-A', from: [3.5, 2], to: [4, 2] }]);
    paint(m);
    check('while latched, a new order does not re-frame', sameView(m.view(), vz),
        JSON.stringify(viewNums(m.view())) + ' vs ' + JSON.stringify(viewNums(vz)));

    m.ids['map-recenter'].listeners.click[0]({});
    let n = 0;
    while (m.pendingFrames() && n < 500) { m.frame(m.clock.t); n++; }
    check('Recenter drops the latch and eases (over several frames) to the auto-framed view',
        m.userView() === false && n > 2 && sameView(m.view(), m.viewTarget()) && !sameView(m.view(), vz),
        n + ' frames, ' + JSON.stringify(viewNums(m.view())));
});

test(async function zoomIsBounded() {
    const m = await boot();
    m.wireViewControls();
    drive(m, [{ id: 'AMR-A', from: [3, 2], to: [3.5, 2] }]);
    paint(m);
    const plant = Math.max(m.fullPlantView().w, m.fullPlantView().h);
    for (let i = 0; i < 60; i++) m.host.listeners.wheel[0]({ deltaY: -100, clientX: 800, clientY: 450, preventDefault() {} });
    check('zooming in stops at 8% of the plant', near(Math.max(m.view().w, m.view().h), plant * 0.08),
        String(Math.max(m.view().w, m.view().h) / plant));
    for (let i = 0; i < 60; i++) m.host.listeners.wheel[0]({ deltaY: 100, clientX: 800, clientY: 450, preventDefault() {} });
    check('zooming out stops at 125% of the plant', near(Math.max(m.view().w, m.view().h), plant * 1.25),
        String(Math.max(m.view().w, m.view().h) / plant));
});

// ═══ 7. the minimap ═════════════════════════════════════════════════════════

test(async function theMinimap() {
    console.log('minimap');
    const m = await boot();
    await setOrders(m, [order(9, 'in_transit', 'AMR-A', 'SMN_05', 'PLN_02', true)]);
    drive(m, [
        { id: 'AMR-A', from: [0.3, 0.2], to: [0.5, 0.3] },
        { id: 'AMR-C', from: [14, 1] },
    ]);
    paint(m);
    const mm = m.minimapEl();
    check('the minimap is in the map region, shown while an order is active',
        !!mm && mm.parentNode === m.region && cls(mm) === 'map-minimap' && mm.style.display === '');
    check('its viewBox is the whole plant',
        mm.attrs.viewBox.split(' ').map(Number).every(function (x, i) { return near(x, [-1.7, -11.7, 37.4, 14.4][i]); }),
        mm.attrs.viewBox);
    const net = mm.children[0];
    const lines = net.children.filter(function (k) { return k.tag === 'line'; });
    const dots = net.children.filter(function (k) { return k.tag === 'circle'; });
    check('static layer at 0.3 opacity: one line per physical lane (9), one dot per scene point (10), nothing else',
        net.attrs.opacity === '0.3' && lines.length === 9 && dots.length === 10 && net.children.length === 19,
        net.children.map(function (k) { return k.tag; }).join(','));
    const ends = lines.map(function (l) { return [l.attrs.x1, l.attrs.y1, l.attrs.x2, l.attrs.y2].join(' '); });
    check('the curved lanes are drawn as CHORDS: SMN_05-LM921 and PLN_02-LM921 are straight lines',
        ends.indexOf('0 0 8 0') >= 0 && ends.indexOf('16 0 8 0') >= 0 &&
        net.children.every(function (k) { return k.tag !== 'path'; }), JSON.stringify(ends));
    const bots = mm.children[1].children;
    check('one dot per robot at its place, in its map colour',
        bots.length === 2 && bots[0].attrs.cx === '0.5' && bots[0].attrs.cy === '-0.3' &&
        bots[0].attrs.fill === m.STATUS_COLOR.in_transit && bots[0].attrs.r === String(m.graphScale() * 0.22),
        bots.map(drawList).join(' | '));
    check('a viewport rectangle on top', mm.children[2].tag === 'rect' && mm.children.length === 3);

    // Built ONCE (U2, 2026-11-14; was rebuilt on every render, against its own
    // comment). A build attaches a new network layer to the minimap; count them.
    let builds = 0;
    const attach = mm.insertBefore;
    mm.insertBefore = function (c, ref) {
        if (c.tag === 'g' && c.attrs.opacity === '0.3') builds++;
        return attach.call(this, c, ref);
    };
    drive(m, [{ id: 'AMR-A', from: [0.5, 0.3], to: [0.7, 0.4] }]);
    paint(m);
    for (let i = 0; i < 4; i++) { m.onRobotUpdate([frameOf('AMR-A', [0.7, 0.4])]); paint(m); }
    check('the static layer is built ONCE: five more renders build no network and keep every element',
        m.minimapEl() === mm && builds === 0 && mm.children[0] === net && net.children.length === 19 &&
        lines.every(function (l, i) { return net.children[i] === l; }), builds + ' builds');
    const moved = mm.children[1].children[0];
    check('...while the robot dots follow the robots on the dynamic path',
        mm.children[1].children.length === 2 && moved.attrs.cx === '0.7' && moved.attrs.cy === '-0.4',
        drawList(moved));

    await setOrders(m, [order(9, 'delivered', 'AMR-A', 'SMN_05', 'PLN_02', true)]);
    drive(m, [{ id: 'AMR-A', from: [0.7, 0.4], to: [0.9, 0.5] }]);
    paint(m);
    check('hidden with no active order', mm.style.display === 'none', mm.style.display);

    // ...and rebuilt when what it draws changes: the scene, or the turn.
    await m.loadEdges();
    m.onRobotUpdate([frameOf('AMR-A', [0.9, 0.5])]);
    paint(m);
    check('a scene reload rebuilds the network once', builds === 1 && mm.children[0] !== net && mm.children.length === 3,
        builds + ' builds');
    m.wireViewControls();
    m.ids['map-rotate'].listeners.click[0]({});
    const turned = mm.children[0].children.filter(function (k) { return k.tag === 'line'; })
        .map(function (l) { return [l.attrs.x1, l.attrs.y1, l.attrs.x2, l.attrs.y2].join(' '); });
    check('a quarter turn rebuilds it once, turned: SMN_05-LM921 now runs 0 0 0 8',
        builds === 2 && turned.indexOf('0 0 0 8') >= 0 && mm.children.length === 3, builds + ' builds ' + JSON.stringify(turned));
});

// ═══ 8. the intent line and the frozen lane ═════════════════════════════════

test(async function intentAndFrozenLanes() {
    console.log('intent line and frozen lane');
    const m = await boot();
    await setOrders(m, [
        order(12, 'staged', 'AMR-D', 'SMN_05', 'PLN_02', false),
        order(15, 'queued', '', 'LM941', 'SMN_05', false),
        order(13, 'blocked', 'AMR-F', 'SMN_05', 'PLN_02', true),
    ]);
    drive(m, [{ id: 'AMR-F', from: [0, 0] }]);
    paint(m);
    const lines = intents(m).map(function (n) { return n.attrs.points; });
    // The route model (lead decision D9): directed, weighed by the distance
    // driven. Route C is stored one way, toward SMN_05, so SMN_05 -> PLN_02
    // is A. Before the route model this line ran C backwards
    // (0,0 0,0 2,-1 14,-1 16,0 16,0).
    check('a staged order draws its intent line source -> delivery along route A, the route model\'s way (D9)',
        lines.indexOf('0,0 0,0 0,-2 16,-2 16,0 16,0') >= 0, JSON.stringify(lines));
    check('an intent with no network route is a straight hint', lines.indexOf('30,-10 0,0') >= 0, JSON.stringify(lines));
    const it = intents(m)[0];
    check('intent: unfilled, 0.34 opacity', it.attrs.fill === 'none' && it.attrs['stroke-opacity'] === '0.34',
        drawList(it));
    const f = frozen(m);
    check('a blocked order draws one frozen lane, neutral grey, along route C',
        f.length === 1 && f[0].attrs.stroke === m.IDLE_MOVE_COLOR && f[0].attrs.fill === 'none' &&
        f[0].attrs['stroke-opacity'] === '0.4' && f[0].attrs.points === '0,0 0,0 2,-1 14,-1 16,0 16,0',
        f.map(drawList).join(' | '));
});

test(async function reducedMotionDrawsLanesNotComets() {
    const m = await boot({ reduceMotion: true });
    await setOrders(m, [order(9, 'in_transit', 'AMR-A', 'SMN_05', 'PLN_02', true)]);
    drive(m, [{ id: 'AMR-A', from: [0.3, 0.2], to: [0.5, 0.3] }]);
    paint(m);
    const f = frozen(m);
    check('reduced motion: no comet, a frozen lane in the status colour instead',
        comets(m).length === 0 && f.length === 1 && f[0].attrs.stroke === m.STATUS_COLOR.in_transit &&
        f[0].attrs.points === '0.5,-0.3 0,0 2,-1 14,-1 16,0 16,0', f.map(drawList).join(' | '));
});

// ═══ 9. the whole scene, as drawn ═══════════════════════════════════════════
//
// Every element of the drawn map in paint order, and the header legend, the
// map key and the rail, for a floor that uses every glyph the page draws: a
// snapped and an unsnapped action point, occupied, empty and hot bays, two
// palette classes, moving robots on each leg, a blocked one, an errored one,
// charging, paused, offline and docked robots, a stack of six staged robots
// whose name chips de-collide down to a leader line, and an idle robot driving
// away from a delivered order. Three states: as fed, AMR-A focused, and then
// turned a quarter. Recorded at c0739799, the tree before the scene was
// retained (dashboard-map.scene.golden.txt). Regenerate it only in a commit
// that changes the look on purpose, and say so there:
//
//     node dashboard-map.render.test.js --update-scene-golden

const SCENE_GOLDEN = path.join(__dirname, 'dashboard-map.scene.golden.txt');
const UPDATE_SCENE_GOLDEN = process.argv.indexOf('--update-scene-golden') >= 0;

// sceneList spells the drawn map in paint order: each element depth-first as
// its draw list (and its text), indented by depth. A layer group
// (g[data-layer]) only orders paint and is not spelled; its children sit at
// its own depth, so a scene grouped into layers spells the same as a flat one.
function sceneList(root) {
    const out = [];
    (function visit(n, depth) {
        const layer = n.tag === 'g' && n.getAttribute('data-layer') !== null;
        if (!layer) out.push('  '.repeat(depth) + drawList(n) + (n.textContent ? ' "' + n.textContent + '"' : ''));
        n.children.forEach(function (c) { visit(c, layer ? depth : depth + 1); });
    })(root, 0);
    return out;
}

function pointRow(id, name, klass, x, y) {
    return { id: id, area_name: 'AREA', instance_name: name, class_name: klass, point_name: name, label: '', pos_x: x, pos_y: y };
}
// One of each node glyph, inside the route scene's extent so the plant frame
// is the route scene's. graphScale there is 4 m, so an action point snaps
// within 2 m of a network vertex, and a robot within 2 m of a bay stands on
// it (the first such bay in point order). Every bay below is more than 2 m
// from every robot but the one named.
const GLYPH_POINTS = [
    pointRow(101, 'AP_01', 'ActionPoint', 8.3, 0.2), // 0.36 m off LM921: drawn on it
    pointRow(102, 'AP_02', 'ActionPoint', 21, 5),    // 5.8 m from any vertex: drawn where it is
    pointRow(103, 'CP_01', 'ChargePoint', 22, 6),    // AMR-C stands on it: drawn
    pointRow(104, 'CP_02', 'ChargePoint', 26, 6),    // empty: not drawn
    pointRow(105, 'PK_01', 'ParkPoint', 24, 3),      // AMR-P stands on it: drawn
    pointRow(106, 'PK_02', 'ParkPoint', 30, 7),      // empty but order 17's destination: drawn, accented
    pointRow(107, 'PK_03', 'ParkPoint', 32, 7),      // empty: not drawn
    pointRow(108, 'SH_01', 'Shelf', 12, 6),          // its own class colour; order 17's source: accented
    pointRow(109, 'RK_01', 'Rack', 10, 6),           // another class colour
];

function robotFrame(id, at, extra) {
    return Object.assign({ vehicle_id: id, state: 'ready', x: at[0], y: at[1], angle: 0, charging: false }, extra || {});
}

// glyphFloor boots the page on the route scene plus GLYPH_POINTS and feeds
// it the floor above, two robot frames a second apart, then paints.
async function glyphFloor() {
    const m = await boot({ extraPoints: GLYPH_POINTS });
    m.wireViewControls();
    const staged = ['S1', 'S2', 'S3', 'S4', 'S5', 'S6'].map(function (s, i) {
        return order(21 + i, 'staged', 'AMR-' + s, 'LM911', 'LM912', false);
    });
    await setOrders(m, [
        order(9, 'in_transit', 'AMR-A', 'SMN_05', 'PLN_02', true),
        order(10, 'dispatched', 'AMR-B', 'LM941', 'LM942', false),
        order(13, 'blocked', 'AMR-F', 'SMN_05', 'PLN_02', true),
        order(17, 'queued', '', 'SH_01', 'PK_02', false),
        order(30, 'delivered', 'AMR-I', 'RK_01', 'AP_02', false),
    ].concat(staged));
    function feed(t) {
        const list = [
            robotFrame('AMR-A', t ? [0.5, 0.3] : [0.3, 0.2], { state: 'busy', angle: 0.3 }),
            robotFrame('AMR-B', t ? [25.5, 10] : [25, 10], { state: 'busy' }),
            robotFrame('AMR-F', [0, 0]),
            robotFrame('AMR-E', [12, 8], { state: 'error' }),
            robotFrame('AMR-C', [22.05, 6.02]),
            robotFrame('AMR-K', [28, 2], { charging: true }),
            robotFrame('AMR-P', [24.03, 3.01], { state: 'paused' }),
            robotFrame('AMR-O', [30, 4], { state: 'offline' }),
            robotFrame('AMR-I', t ? [8.5, 6] : [8, 6], { angle: 1.2 }),
        ];
        ['S1', 'S2', 'S3', 'S4', 'S5', 'S6'].forEach(function (s) { list.push(robotFrame('AMR-' + s, [20, 2])); });
        m.onRobotUpdate(list);
    }
    feed(0);
    m.clock.t += 1000;
    feed(1);
    paint(m);
    return m;
}

function sceneRecord(m, title) {
    return ['== ' + title].concat(sceneList(m.scene()),
        ['-- header legend', m.ids['map-legend'].html,
            '-- map key', m.ids['map-class-legend'].html,
            '-- rail: active', m.ids['rail-motion-list'].html,
            '-- rail: activity', m.ids['rail-activity-list'].html]);
}

async function glyphFloorRecord() {
    const m = await glyphFloor();
    const out = sceneRecord(m, 'as fed');
    click(m, robotGroup(m, 'AMR-A'));
    paint(m);
    out.push.apply(out, sceneRecord(m, 'AMR-A focused'));
    m.ids['map-rotate'].listeners.click[0]({});
    out.push.apply(out, sceneRecord(m, 'turned a quarter, AMR-A still focused'));
    return { m: m, text: out.join('\n') + '\n' };
}

test(async function theWholeSceneAsDrawn() {
    console.log('the whole scene');
    const rec = await glyphFloorRecord();
    if (UPDATE_SCENE_GOLDEN) {
        fs.writeFileSync(SCENE_GOLDEN, rec.text);
        console.log('  wrote ' + SCENE_GOLDEN);
        return;
    }
    const want = fs.readFileSync(SCENE_GOLDEN, 'utf8').split('\n');
    const got = rec.text.split('\n');
    let at = 0;
    while (at < want.length && at < got.length && want[at] === got[at]) at++;
    check('the scene, legend, map key and rail are drawn as recorded in ' + path.basename(SCENE_GOLDEN),
        at === want.length && at === got.length,
        'first difference at line ' + (at + 1) + '\n  golden: ' + want[at] + '\n  page:   ' + got[at]);
});

// ═══ 10. the retained scene: what a frame costs ═════════════════════════════
//
// The map keeps one <svg> and four layers (base, routes, nodes, robots). Base
// and nodes are built only when what they show changes; routes and robots are
// patched in place. Each pin counts a real DOM operation on the real page —
// elements a layer is handed, elements created — and then checks the drawing:
// the scene as kept must spell exactly what a fresh build draws at that
// moment (sameAsFresh), and the change itself must be on screen.

function layerOf(m, name) {
    return m.scene().children.filter(function (c) { return c.getAttribute('data-layer') === name; })[0] || null;
}
function inside(n, root) {
    for (let p = n; p; p = p.parentNode) if (p === root) return true;
    return false;
}
// createdIn: the elements made since `mark` that are now inside `root`.
function createdIn(m, mark, root) {
    return m.madeSince(mark).filter(function (e) { return inside(e, root); });
}
// handed counts the elements a layer is given (a build hands it every element
// it draws once), from now on.
function handed(layer) {
    let n = 0;
    const ins = layer.insertBefore;
    layer.insertBefore = function (c, ref) { n++; return ins.call(this, c, ref); };
    return function () { return n; };
}
// sameAsFresh: the scene as the page kept it, against what a fresh build of
// the whole scene draws now. Leaves the fresh build on screen.
function sameAsFresh(m) {
    const kept = sceneList(m.scene());
    m.sceneView().clear();
    m.render();
    const fresh = sceneList(m.scene());
    let at = 0;
    while (at < kept.length && kept[at] === fresh[at]) at++;
    return { ok: at === kept.length && at === fresh.length, why: 'line ' + (at + 1) + '\n  kept:  ' + kept[at] + '\n  fresh: ' + fresh[at] };
}
// tick moves the three moving robots of glyphFloor a little, a second on.
function tick(m, k) {
    m.clock.t += 1000;
    m.onRobotUpdate([
        robotFrame('AMR-A', [0.5 + 0.1 * k, 0.3], { state: 'busy', angle: 0.3 }),
        robotFrame('AMR-B', [25.5 + 0.1 * k, 10], { state: 'busy' }),
        robotFrame('AMR-I', [8.5 + 0.1 * k, 6], { angle: 1.2 }),
    ]);
    paint(m);
}
function heading(m, id) { return robotGroup(m, id).children.filter(function (n) { return n.tag === 'g'; })[0] || null; }
function halo(m, id) { return robotGroup(m, id).children.filter(function (n) { return cls(n) === 'map-robot-halo'; })[0] || null; }

test(async function aFrameWithNothingStaticBuildsNothing() {
    console.log('the retained scene');
    const m = await glyphFloor();
    const base = layerOf(m, 'base'), nodes = layerOf(m, 'nodes');
    check('one <svg> in four layers, in paint order: base, routes, nodes, robots',
        m.scene().children.map(function (c) { return c.getAttribute('data-layer'); }).join(',') === 'base,routes,nodes,robots');
    const baseHanded = handed(base), nodesHanded = handed(nodes);
    const baseKids = base.children.slice(), nodeKids = nodes.children.slice();
    const group = robotGroup(m, 'AMR-A'), glow = halo(m, 'AMR-A'), chev = heading(m, 'AMR-A');
    const mark = m.made.length;
    for (let k = 1; k <= 5; k++) tick(m, k);
    check('five robot ticks with nothing static changed hand the base and the nodes nothing, and keep every element',
        baseHanded() === 0 && nodesHanded() === 0 &&
        base.children.length === baseKids.length && base.children.every(function (c, i) { return c === baseKids[i]; }) &&
        nodes.children.length === nodeKids.length && nodes.children.every(function (c, i) { return c === nodeKids[i]; }),
        baseHanded() + ' to the base, ' + nodesHanded() + ' to the nodes');
    check('...and create no element anywhere in the scene: robots, chips and routes are patched in place',
        createdIn(m, mark, m.scene()).length === 0, createdIn(m, mark, m.scene()).map(drawList).join(' | '));
    const s = m.proj(1.0, 0.3);
    check('AMR-A\'s group, halo and chevron are the elements they were, moved to where it is now',
        robotGroup(m, 'AMR-A') === group && halo(m, 'AMR-A') === glow && heading(m, 'AMR-A') === chev &&
        chev.attrs.transform.indexOf('translate(' + s[0] + ',' + s[1] + ')') === 0 && glow.attrs.cx === String(s[0]),
        chev.attrs.transform);
    check('...so the halo\'s CSS pulse runs on: it is not a new element each render', glow.parentNode === group);
    const same = sameAsFresh(m);
    check('the scene as kept is the scene a fresh build draws', same.ok, same.why);
});

test(async function oneNodesBuildPerHotSetChange() {
    const m = await glyphFloor();
    const base = layerOf(m, 'base'), nodes = layerOf(m, 'nodes');
    const baseKids = base.children.slice();
    const baseHanded = handed(base), nodesHanded = handed(nodes);
    await setOrders(m, m.data.orders.concat([order(40, 'queued', '', 'SH_01', 'PK_03', false)]));
    m.render(); // as the page's order reload does once the orders are in (scheduleOrderReload)
    const at = m.proj(32, 7);
    const pk3 = nodes.children.filter(function (c) { return c.attrs.cx === String(at[0]) && c.attrs.cy === String(at[1]); });
    check('an order naming an empty bay builds the nodes layer once, and the base not at all',
        nodesHanded() === nodes.children.length && baseHanded() === 0 &&
        base.children.every(function (c, i) { return c === baseKids[i]; }),
        nodesHanded() + ' handed for ' + nodes.children.length + ' node elements, ' + baseHanded() + ' to the base');
    check('...and the bay it names, PK_03, is now drawn, accented',
        pk3.length === 1 && pk3[0].attrs.stroke === '#7c7cf0', pk3.map(drawList).join(' | '));
    for (let k = 1; k <= 3; k++) tick(m, k);
    check('...then three robot ticks build nothing more', nodesHanded() === nodes.children.length && baseHanded() === 0,
        nodesHanded() + ' handed');
    const same = sameAsFresh(m);
    check('the scene as kept is the scene a fresh build draws', same.ok, same.why);
});

test(async function oneNodesBuildPerBayChange() {
    const m = await glyphFloor();
    const nodes = layerOf(m, 'nodes');
    const nodesHanded = handed(nodes), baseHanded = handed(layerOf(m, 'base'));
    const cp1 = m.proj(22, 6);
    const ringAt = function () {
        return nodes.children.filter(function (c) { return cls(c) === 'map-node-charge' && c.attrs.cx === String(cp1[0]); }).length;
    };
    check('CP_01 is drawn while AMR-C stands on it', ringAt() === 1);
    // AMR-C drives off its charger to 4 m from any bay.
    m.onRobotUpdate([robotFrame('AMR-C', [18, 6])]);
    paint(m);
    check('a robot leaving its bay builds the nodes once and the base not at all, and the empty bay is gone',
        nodesHanded() === nodes.children.length && baseHanded() === 0 && ringAt() === 0,
        nodesHanded() + ' handed for ' + nodes.children.length + ', ' + baseHanded() + ' to the base');
    const same = sameAsFresh(m);
    check('the scene as kept is the scene a fresh build draws', same.ok, same.why);
});

test(async function oneBaseBuildPerSceneLoad() {
    const m = await glyphFloor();
    const base = layerOf(m, 'base');
    const baseHanded = handed(base);
    const v0 = Object.assign({}, m.view());
    // The same plant with route C's two stored rows gone; the median lane,
    // and so every marker size, is unchanged.
    m.data.edges = EDGE_ROWS.filter(function (e) {
        return e.instance_name !== 'PLN_02-LM932' && e.instance_name !== 'LM932-LM931';
    });
    await m.loadEdges();
    m.render();
    const aisles = base.children.filter(function (c) { return cls(c) === 'map-aisle'; });
    check('a scene load that keeps the view builds the base once, with the lanes it now has (7 of 9)',
        sameView(m.view(), v0) && baseHanded() === base.children.length && aisles.length === 7,
        baseHanded() + '/' + base.children.length + ' handed, ' + aisles.length + ' aisles, view ' + JSON.stringify(viewNums(m.view())));
    const same = sameAsFresh(m);
    check('the scene as kept is the scene a fresh build draws', same.ok, same.why);
});

test(async function oneBuildPerSettledZoom() {
    const m = await glyphFloor();
    const base = layerOf(m, 'base'), nodes = layerOf(m, 'nodes');
    const baseHanded = handed(base), nodesHanded = handed(nodes);
    const group = robotGroup(m, 'AMR-A'), r0 = halo(m, 'AMR-A').attrs.r;
    const aisle0 = base.children.filter(function (c) { return cls(c) === 'map-aisle'; })[0].attrs['stroke-width'];
    const notch = { deltaY: -100, clientX: 800, clientY: 450, preventDefault() {} };
    const mark = m.made.length;
    m.host.listeners.wheel[0](notch);
    m.host.listeners.wheel[0](notch);
    check('two wheel notches move the viewBox and draw nothing yet', baseHanded() === 0 && nodesHanded() === 0 &&
        createdIn(m, mark, m.scene()).length === 0, baseHanded() + ' handed');
    check('...and leave one settle pending, the first cancelled by the second', m.runTimers() === 1);
    paint(m);
    const aisle1 = base.children.filter(function (c) { return cls(c) === 'map-aisle'; })[0].attrs['stroke-width'];
    check('the settle builds the base and the nodes once, at the zoomed marker size',
        baseHanded() === base.children.length && nodesHanded() === nodes.children.length && aisle1 !== aisle0,
        baseHanded() + '/' + base.children.length + ' base, ' + nodesHanded() + '/' + nodes.children.length + ' nodes, aisle ' + aisle0 + ' -> ' + aisle1);
    check('...and resizes the robots in place: AMR-A\'s group is the same, its halo the new size',
        robotGroup(m, 'AMR-A') === group && halo(m, 'AMR-A').attrs.r !== r0, r0 + ' -> ' + halo(m, 'AMR-A').attrs.r);
    const built = baseHanded();
    tick(m, 1);
    check('a robot tick after the settle builds nothing', baseHanded() === built, baseHanded() + ' handed');
    const same = sameAsFresh(m);
    check('the scene as kept is the scene a fresh build draws', same.ok, same.why);
});

test(async function oneBuildPerRotate() {
    const m = await glyphFloor();
    const base = layerOf(m, 'base'), nodes = layerOf(m, 'nodes');
    const baseHanded = handed(base), nodesHanded = handed(nodes);
    const group = robotGroup(m, 'AMR-A'), chev = heading(m, 'AMR-A');
    m.ids['map-rotate'].listeners.click[0]({});
    check('a quarter turn builds the base and the nodes once',
        baseHanded() === base.children.length && nodesHanded() === nodes.children.length,
        baseHanded() + '/' + base.children.length + ' base, ' + nodesHanded() + '/' + nodes.children.length + ' nodes');
    const turned = 'translate(0.3,0.5) rotate(' + (-(0.3 * 180 / Math.PI) + 90) + ')';
    check('...and turns the robots in place: AMR-A\'s group and chevron are the same, heading turned with the map',
        robotGroup(m, 'AMR-A') === group && heading(m, 'AMR-A') === chev && chev.attrs.transform === turned,
        chev.attrs.transform + ' vs ' + turned);
    const same = sameAsFresh(m);
    check('the scene as kept is the scene a fresh build draws', same.ok, same.why);
});

test(async function robotsChangeShapeInTheirOwnGroup() {
    const m = await glyphFloor();
    const iGroup = robotGroup(m, 'AMR-I'), aGroup = robotGroup(m, 'AMR-A');
    // AMR-I stands still for longer than the motion linger: it comes to rest.
    m.clock.t += 6000;
    m.onRobotUpdate([robotFrame('AMR-I', [8.5, 6], { angle: 1.2 })]);
    paint(m);
    check('a robot coming to rest keeps its group and trades halo, chevron and core for one disc',
        robotGroup(m, 'AMR-I') === iGroup && iGroup.children.length === 1 && cls(iGroup.children[0]) === 'map-robot',
        iGroup.children.map(drawList).join(' | '));
    // AMR-P is taken off pause: its disc keeps its element and loses the
    // paused outline, an attribute removed, not left behind.
    const pDisc = robotGroup(m, 'AMR-P').children[0];
    m.onRobotUpdate([robotFrame('AMR-P', [24.03, 3.01])]);
    paint(m);
    check('a paused robot made ready keeps its disc, which drops the paused class and its style',
        robotGroup(m, 'AMR-P').children[0] === pDisc && cls(pDisc) === 'map-robot' && pDisc.getAttribute('style') === null,
        drawList(pDisc));
    // A robot new to the floor joins the robots, before every name chip.
    m.onRobotUpdate([robotFrame('AMR-N', [16, 8], { state: 'error' })]);
    paint(m);
    const kids = layerOf(m, 'robots').children;
    const firstChip = kids.findIndex(function (k) { return k.children.some(function (c) { return c.tag === 'text'; }); });
    const nAt = kids.indexOf(robotGroup(m, 'AMR-N'));
    check('a new robot\'s group lands after the last robot group and before the first chip',
        nAt === firstChip - 1 && kids[kids.length - 1].children.some(function (c) { return c.textContent === 'AMR-N'; }),
        'group at ' + nAt + ', first chip at ' + firstChip);
    // A robot whose position goes missing leaves the map.
    m.onRobotUpdate([{ vehicle_id: 'AMR-O', state: 'offline' }]);
    paint(m);
    check('a robot with no position is taken off the map, chip and all',
        findAll(m.scene(), function (n) { return n.getAttribute('data-robot') === 'AMR-O'; }).length === 0);
    // Focus: the ring joins AMR-A's own group; the staged intent lines go.
    const lane = frozen(m)[0];
    click(m, aGroup);
    paint(m);
    check('focusing AMR-A puts its ring in its own group, first, and removes the other robots\' intent lines',
        robotGroup(m, 'AMR-A') === aGroup && cls(aGroup.children[0]) === 'map-robot-focus' && intents(m).length === 0,
        aGroup.children.map(cls).join(','));
    check('...and dims AMR-F\'s frozen lane on the same element', frozen(m)[0] === lane &&
        lane.attrs['stroke-opacity'] === '0.22', drawList(lane));
    const same = sameAsFresh(m);
    check('the scene as kept is the scene a fresh build draws', same.ok, same.why);
});

test(async function aLaneFollowsItsRobotInPlace() {
    const m = await boot({ reduceMotion: true });
    await setOrders(m, [order(9, 'in_transit', 'AMR-A', 'SMN_05', 'PLN_02', true)]);
    drive(m, [{ id: 'AMR-A', from: [0.3, 0.2], to: [0.5, 0.3] }]);
    paint(m);
    const lane = frozen(m)[0];
    drive(m, [{ id: 'AMR-A', from: [0.5, 0.3], to: [0.7, 0.4] }]);
    paint(m);
    check('under reduced motion the frozen lane is the same element, its points following the robot',
        frozen(m).length === 1 && frozen(m)[0] === lane && lane.attrs.points === '0.7,-0.4 0,0 2,-1 14,-1 16,0 16,0',
        drawList(lane));
    const same = sameAsFresh(m);
    check('the scene as kept is the scene a fresh build draws', same.ok, same.why);
});

// ═══ 11. the legends and the rail write only on change ══════════════════════
//
// The header legend, the map key and the rail's two lists are innerHTML. A
// render rewrites one only when its markup differs from what the page last
// wrote there; each check counts the writes, then reads what is shown.

const PANELS = ['map-legend', 'map-class-legend', 'rail-motion-list', 'rail-activity-list'];
function writes(m) { return PANELS.map(function (id) { return m.ids[id].writes; }); }
// moveInPlace is a robot tick on a stopped clock: three robots move, and
// nothing a legend or the rail says about them changes.
function moveInPlace(m, k) {
    m.onRobotUpdate([
        robotFrame('AMR-A', [0.5 + 0.1 * k, 0.3], { state: 'busy', angle: 0.3 }),
        robotFrame('AMR-B', [25.5 + 0.1 * k, 10], { state: 'busy' }),
        robotFrame('AMR-I', [8.5 + 0.1 * k, 6], { angle: 1.2 }),
    ]);
    paint(m);
}

test(async function legendsAndRailWriteOnlyOnChange() {
    console.log('legends and rail');
    const m = await glyphFloor();
    const w0 = writes(m);
    const shown = PANELS.map(function (id) { return m.ids[id].html; });
    for (let k = 1; k <= 5; k++) moveInPlace(m, k);
    check('five robot ticks that change no count, key or row write none of the legend, the map key and the rail',
        JSON.stringify(writes(m)) === JSON.stringify(w0), JSON.stringify(w0) + ' -> ' + JSON.stringify(writes(m)));
    check('...which still show what they showed', PANELS.every(function (id, i) { return m.ids[id].html === shown[i]; }));

    // AMR-K errors on its charger: Charging 2 -> 1, Error 1 -> 2.
    m.onRobotUpdate([robotFrame('AMR-K', [28, 2], { charging: true, state: 'error' })]);
    paint(m);
    const legend = m.ids['map-legend'];
    check('a robot changing bucket rewrites the header legend once, with the new counts, and nothing else',
        JSON.stringify(writes(m)) === JSON.stringify([w0[0] + 1, w0[1], w0[2], w0[3]]) &&
        legend.html.indexOf('Charging<span class="map-legend-count">1</span>') >= 0 &&
        legend.html.indexOf('Error<span class="map-legend-count">2</span>') >= 0, JSON.stringify(writes(m)) + ' ' + legend.html);

    const key = m.ids['map-class-legend'];
    const kw = key.writes;
    key.querySelector('.map-key-toggle').listeners.click[0]({});
    check('opening the map key rewrites it once, open',
        key.writes === kw + 1 && key.html.indexOf('aria-expanded="true"') >= 0 && key.html.indexOf(' hidden') < 0, key.html);
    moveInPlace(m, 6);
    moveInPlace(m, 7);
    check('...renders after that leave it alone', key.writes === kw + 1, String(key.writes - kw));
    key.querySelector('.map-key-toggle').listeners.click[0]({});
    check('...and the button it wrote is the one listened to: it closes the key again',
        key.writes === kw + 2 && key.html.indexOf('aria-expanded="false"') >= 0, key.html);

    // AMR-F's order is released: blocked -> in_transit.
    const rail = m.ids['rail-motion-list'], feed = m.ids['rail-activity-list'];
    const rw = rail.writes, fw = feed.writes, lw = legend.writes;
    await setOrders(m, m.data.orders.map(function (o) {
        return o.order_id === 13 ? Object.assign({}, o, { status: 'in_transit' }) : o;
    }));
    m.render();
    check('an order changing status rewrites the active list and the feed once each',
        rail.writes === rw + 1 && feed.writes === fw + 1 && rail.html.indexOf('BLOCKED') < 0 &&
        feed.html.indexOf('AMR-F responding') >= 0, rail.writes - rw + ' / ' + (feed.writes - fw));
    const aged = feed.html;
    m.clock.t += 10000;
    moveInPlace(m, 8);
    check('ten seconds on, the feed rewrites once as its lines fade, and the legend does not',
        feed.writes === fw + 2 && feed.html !== aged && legend.writes === lw + 1, feed.writes - fw + ' feed, ' + (legend.writes - lw) + ' legend');
});

// --- run -------------------------------------------------------------------

(async function run() {
    for (const fn of pending) {
        try {
            await fn();
        } catch (e) {
            failures++;
            console.log('  FAIL ' + (fn.name || 'a pin') + ' threw — ' + (e && e.stack || e));
        }
    }
    console.log(failures === 0 ? 'PASS' : failures + ' FAILURE(S)');
    process.exit(failures === 0 ? 0 : 1);
})();
