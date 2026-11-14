// Pins for the robot map AS DRAWN — the comets, the calm rule, click-to-focus,
// zoom and rotate, the minimap, the intent line and the frozen lane — recorded
// at the tree before the map's machinery moves to shared/, so that a move
// which keeps the look keeps these green and a change of look changes them on
// purpose. Run under plain Node via the Go wrapper dashboard_map_render_test.go.
// Exit 0 on pass, 1 on any assertion failure.
//
// THE PAGE RUNS FOR REAL: dashboard-map.js and shared/scene-geom.js, its own
// data loads over a stubbed fetch, its own SSE handler (onRobotUpdate), its
// own render() through its own requestAnimationFrame coalescing, its own click
// and wheel listeners. What is stubbed is the DOM (an element here is a little
// tree that records every attribute set on it), the clock (Date.now is a
// number the pin sets, so "moving" is decided, not timed), and the frame queue
// (a frame runs when the pin says, with the `now` the pin gives).
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
        set innerHTML(v) {
            this.children.forEach(function (c) { c.parentNode = null; });
            this.children = [];
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
// module REASSIGNS (view, focusRobot, turnDeg, the comet state, proj) is handed
// out through an accessor; a reference would go stale on the first render.

function loadSceneGeom(ctx) {
    const raw = fs.readFileSync(path.join(SHARED_DIR, 'scene-geom.js'), 'utf8');
    const src = raw.replace(/^export /mg, '');
    if (src === raw) {
        throw new Error('shared/scene-geom.js no longer declares its exports as ' +
            '"export function"/"export var" at line start, which this harness strips to ' +
            'run it as a script; update loadSceneGeom in dashboard-map.render.test.js');
    }
    vm.runInContext(src, ctx);
}

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
    const data = { orders: [] };

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

    const ctx = {
        console: console,
        Math: Math, Object: Object, Array: Array, JSON: JSON, String: String, Number: Number,
        isFinite: isFinite, parseInt: parseInt, Promise: Promise,
        Date: { now: function () { return clock.t; } },
        document: {
            // 'loading': the module tail then waits for DOMContentLoaded
            // instead of calling init(), and the pin drives the loads itself.
            readyState: 'loading',
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
            createElementNS(ns, name) { return el(name); },
            addEventListener() {},
        },
        window: {
            addEventListener(t, fn) { (winListeners[t] = winListeners[t] || []).push(fn); },
            matchMedia(q) { return { matches: !!opts.reduceMotion && /reduce/.test(q) }; },
        },
        localStorage: {
            getItem(k) { return Object.prototype.hasOwnProperty.call(stored, k) ? stored[k] : null; },
            setItem(k, v) { stored[k] = String(v); },
        },
        setInterval() { return 0; },
        clearInterval() {},
        setTimeout() { return 0; },
        clearTimeout() {},
        requestAnimationFrame(fn) { frameSeq++; frames.push({ id: frameSeq, fn: fn }); return frameSeq; },
        cancelAnimationFrame(id) { frames = frames.filter(function (f) { return f.id !== id; }); },
        fetch(url) {
            let body = [];
            if (url === '/api/map/points') body = POINT_ROWS;
            else if (url === '/api/map/edges') body = EDGE_ROWS;
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
    loadSceneGeom(ctx);

    const raw = fs.readFileSync(path.join(SRC_DIR, 'dashboard-map.js'), 'utf8');
    const stripped = raw.replace(/^import[^;]+;\s*/mg, '');
    const INJECT = '  __export({ render: render, syncCometLayer: syncCometLayer,' +
        ' onRobotUpdate: onRobotUpdate, loadPoints: loadPoints, loadEdges: loadEdges, loadOrders: loadOrders,' +
        ' wireViewControls: wireViewControls,' +
        ' view: function () { return view; }, viewTarget: function () { return viewTarget; },' +
        ' fullPlantView: function () { return fullPlantView; },' +
        ' focusRobot: function () { return focusRobot; }, turnDeg: function () { return turnDeg; },' +
        ' userView: function () { return userView; },' +
        ' lastCometSig: function () { return lastCometSig; }, cometLayer: function () { return cometLayer; },' +
        ' cometState: function () { return cometState; }, minimapEl: function () { return minimapEl; },' +
        ' proj: function (x, y) { return proj(x, y); }, graphScale: function () { return graphScale; },' +
        ' STATUS_COLOR: STATUS_COLOR, IDLE_MOVE_COLOR: IDLE_MOVE_COLOR });\n})();\n';
    const src = stripped.replace(/\}\)\(\);\s*$/, INJECT);
    if (src === stripped) {
        throw new Error('dashboard-map.js no longer ends in the "})();" IIFE close this ' +
            'harness injects before; update the injection in dashboard-map.render.test.js');
    }
    vm.runInContext(src, ctx);
    if (!exported) throw new Error('__export was not reached inside dashboard-map.js');

    const m = exported;
    m.clock = clock;
    m.host = host;
    m.region = region;
    m.ids = ids;
    m.stored = stored;
    m.winListeners = winListeners;
    m.data = data;
    // frame runs the frames queued so far, at `now`. A frame queued while one
    // runs waits for the next call, as a browser's would.
    m.frame = function (now) {
        const due = frames;
        frames = [];
        due.forEach(function (f) { f.fn(now); });
        return due.length;
    };
    m.pendingFrames = function () { return frames.length; };
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
async function oneComet(leg) {
    const m = await boot();
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

    drive(m, [{ id: 'AMR-A', from: [0.5, 0.3], to: [0.7, 0.4] }]);
    paint(m);
    check('the static layer is REBUILT on every render (a new element), not kept',
        m.minimapEl() === mm && mm.children[0] !== net);

    await setOrders(m, [order(9, 'delivered', 'AMR-A', 'SMN_05', 'PLN_02', true)]);
    drive(m, [{ id: 'AMR-A', from: [0.7, 0.4], to: [0.9, 0.5] }]);
    paint(m);
    check('hidden with no active order', mm.style.display === 'none', mm.style.display);
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
    check('a staged order draws its intent line source -> delivery along route C (the one-way stretch backwards)',
        lines.indexOf('0,0 0,0 2,-1 14,-1 16,0 16,0') >= 0, JSON.stringify(lines));
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
