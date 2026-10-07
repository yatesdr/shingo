// overview.maxwait.test.js — LC9 follow-up (ui-cleanup 2026-10-07).
// The Overview's order-update schedule (createLiveSchedule in live-schedule.js,
// wired in overview/hero.js) has a 10 s max wait, like the Dashboard's W4
// debounceMaxWait. A plain trailing debounce never fires while order-update
// events arrive closer than 1.5 s, so the active count and the alerts banner
// were never read under a steady stream.
//
// Runs under plain node through its Go wrapper
// (overview_maxwait_wrapper_test.go). The real hero.js and live-schedule.js run
// in a vm on a fake clock; every read is counted by URL.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0;
let failed = 0;
function check(cond, label, got) {
    if (cond) { passed++; } else { failed++; console.error('FAIL: ' + label + (got !== undefined ? ' (got ' + got + ')' : '')); }
}

function makeClock() {
    let now = 0, seq = 0;
    const timers = new Map();
    return {
        setTimeout(fn, ms) { const id = ++seq; timers.set(id, { at: now + ms, fn, id }); return id; },
        clearTimeout(id) { timers.delete(id); },
        advanceTo(t) {
            for (;;) {
                let next = null;
                for (const tm of timers.values()) if (tm.at <= t && (!next || tm.at < next.at || (tm.at === next.at && tm.id < next.id))) next = tm;
                if (!next) break;
                timers.delete(next.id);
                now = next.at;
                next.fn();
            }
            now = t;
        },
        now() { return now; },
    };
}

function loadHero() {
    const clock = makeClock();
    const els = {};
    function el(id) {
        return els[id] || (els[id] = {
            id, innerHTML: '', className: '', children: [],
            classList: { add() {} },
            appendChild(c) { this.children.push(c); },
            querySelector() { return { textContent: '' }; },
        });
    }
    const doc = {
        visibilityState: 'visible',
        addEventListener() {},
        getElementById: (id) => el(id),
        createElement: () => el('anon-' + Math.random()),
    };
    const reads = []; // [time, url]
    const sse = {};
    const ctx = vm.createContext({
        Object, Array, Date, Infinity, URLSearchParams, Promise, JSON, Math, String, Number,
        document: doc,
        setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout,
        apiGet(url) {
            reads.push([clock.now(), url]);
            if (url.startsWith('/api/missions/active')) return Promise.resolve({ count: 1 });
            return Promise.resolve({});
        },
        h: () => '',
        onSSE(type, fn) { (sse[type] = sse[type] || []).push(fn); },
        debounce(fn) { return fn; },
        formatDuration: (ms) => ms + 'ms',
        windowFor: () => ({}),
        KpiTile: () => el('tile-' + Math.random()),
        updateKpiTile() {},
        RUN_TIME_TITLE: '',
    });
    const sched = fs.readFileSync(path.join(__dirname, 'live-schedule.js'), 'utf8').replace(/^export /gm, '');
    vm.runInContext(sched, ctx);
    const real = ctx.createLiveSchedule;
    ctx.createLiveSchedule = (run, opts) => real(run, Object.assign({}, opts, { doc, setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout }));
    const realTh = ctx.createEventThrottle;
    ctx.createEventThrottle = (ms) => realTh(ms, clock.now);
    const src = fs.readFileSync(path.join(__dirname, 'overview', 'hero.js'), 'utf8')
        .replace(/^import[^;]+;\s*/gm, '')
        .replace(/^export /gm, '');
    vm.runInContext(src + "\n;globalThis.__hero = createHeroSection({ get: () => ({ range: 'day', station: '', robot: '' }) });", ctx);
    ctx.__hero.mount();
    return {
        clock,
        emit(type, data) { (sse[type] || []).forEach((fn) => fn(data)); },
        active: () => reads.filter((r) => r[1].startsWith('/api/missions/active')).map((r) => r[0]),
        alerts: () => reads.filter((r) => r[1].startsWith('/api/missions/alerts')).map((r) => r[0]),
    };
}

// A steady stream: one order-update per second for 30 s.
(function steadyStream() {
    const t = loadHero();
    for (let ms = 0; ms < 30000; ms += 1000) { t.clock.advanceTo(ms); t.emit('order-update', {}); }
    t.clock.advanceTo(60000);
    const a = t.active();
    check(a.length === 3, 'order-update every 1 s for 30 s: 3 active reads', a.join(','));
    check(t.alerts().join(',') === a.join(','), 'each active read brings its alerts read', t.alerts().join(','));
    let gap = a.length ? a[0] : Infinity;
    for (let i = 1; i < a.length; i++) gap = Math.max(gap, a[i] - a[i - 1]);
    check(gap <= 10000, 'no gap over 10 s while events keep coming', a.join(','));
})();

// A single event: one read, 1.5 s later.
(function single() {
    const t = loadHero();
    t.emit('order-update', {});
    t.clock.advanceTo(60000);
    check(t.active().join(',') === '1500', 'one order-update: 1 read at +1.5 s', t.active().join(','));
})();

// Sparse events (5 s apart): each reads 1.5 s later, as before.
(function sparse() {
    const t = loadHero();
    for (let ms = 0; ms < 20000; ms += 5000) { t.clock.advanceTo(ms); t.emit('order-update', {}); }
    t.clock.advanceTo(60000);
    check(t.active().join(',') === '1500,6500,11500,16500', 'sparse order-updates: trailing debounce unchanged', t.active().join(','));
})();

// The max wait is the Overview's alone: createLiveSchedule's default has none
// (Inventory passes only debounceMs).
(function defaultHasNoCeiling() {
    const clock = makeClock();
    const ctx = vm.createContext({ Object, Array, Date, Infinity });
    vm.runInContext(fs.readFileSync(path.join(__dirname, 'live-schedule.js'), 'utf8').replace(/^export /gm, ''), ctx);
    const fires = [];
    const s = ctx.createLiveSchedule(() => fires.push(clock.now()), { debounceMs: 1500, doc: null, setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout });
    for (let ms = 0; ms < 30000; ms += 1000) { clock.advanceTo(ms); s.kick(); }
    clock.advanceTo(60000);
    check(fires.join(',') === '30500', 'no maxWaitMs: steady stream reads once, after it ends', fires.join(','));
})();

console.log(passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
