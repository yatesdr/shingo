// Unit tests for live-schedule.js and the Overview hero's live wiring (LC9,
// LC11). Run under plain Node via the Go wrapper live_schedule_test.go. Exit 0
// on pass, 1 on any assertion failure.
//
// What they hold:
//   - a burst of events is one run, after it settles;
//   - a hidden tab runs nothing, and one catch-up runs when it is shown;
//   - the event throttle fires at most once per window and never on its own;
//   - Overview: robot-update makes no request, counts the banner's robots from
//     the frame, and reads the alerts at most once per 30 s; order-update reads
//     the active count and the alerts once per burst.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let failures = 0;
function check(name, cond, detail) {
    if (cond) {
        console.log('  ok  ' + name);
    } else {
        failures++;
        console.log('  FAIL ' + name + (detail ? ' — ' + detail : ''));
    }
}

// A fake clock: timers fire only when the test advances time.
function fakeClock() {
    let now = 0;
    let timers = [];
    let nextID = 1;
    return {
        now: () => now,
        setTimeout(fn, ms) { const id = nextID++; timers.push({ id, at: now + ms, fn }); return id; },
        clearTimeout(id) { timers = timers.filter((t) => t.id !== id); },
        advance(ms) {
            const end = now + ms;
            for (;;) {
                timers.sort((a, b) => a.at - b.at);
                const t = timers[0];
                if (!t || t.at > end) break;
                timers.shift();
                now = t.at;
                t.fn();
            }
            now = end;
        },
        pending: () => timers.length,
    };
}

function fakeDoc() {
    const listeners = {};
    return {
        visibilityState: 'visible',
        addEventListener(type, fn) { (listeners[type] = listeners[type] || []).push(fn); },
        setHidden(hidden) {
            this.visibilityState = hidden ? 'hidden' : 'visible';
            (listeners.visibilitychange || []).forEach((fn) => fn());
        },
    };
}

// Loads live-schedule.js as plain script: its exports become context globals.
function loadSchedule(ctx) {
    const src = fs.readFileSync(path.join(__dirname, 'live-schedule.js'), 'utf8')
        .replace(/^export /gm, '');
    vm.runInContext(src, ctx);
}

// ─── live-schedule.js ──────────────────────────────────────────────────────
{
    const ctx = vm.createContext({ Object, Array, Date, Infinity });
    loadSchedule(ctx);
    const { createLiveSchedule, createEventThrottle, countRobotAlerts } = ctx;

    // Debounce: four kicks in a burst → one run, 1500 ms after the last.
    {
        const clk = fakeClock();
        const doc = fakeDoc();
        let runs = 0;
        const s = createLiveSchedule(() => { runs++; }, { debounceMs: 1500, doc, setTimeout: clk.setTimeout, clearTimeout: clk.clearTimeout });
        s.kick(); clk.advance(500); s.kick(); clk.advance(500); s.kick(); s.kick();
        clk.advance(1499);
        check('burst: nothing before it settles', runs === 0, 'runs=' + runs);
        clk.advance(1);
        check('burst of 4 → 1 run', runs === 1, 'runs=' + runs);
        clk.advance(60000);
        check('no run without an event (no timer of its own)', runs === 1 && clk.pending() === 0, 'runs=' + runs);
    }

    // Hidden tab: kicks run nothing; one catch-up on show; none if nothing missed.
    {
        const clk = fakeClock();
        const doc = fakeDoc();
        let runs = 0, catchUps = 0;
        const s = createLiveSchedule(() => { runs++; }, { debounceMs: 1500, doc, catchUp: () => { catchUps++; }, setTimeout: clk.setTimeout, clearTimeout: clk.clearTimeout });
        doc.setHidden(true);
        for (let i = 0; i < 50; i++) { s.kick(); clk.advance(2000); }
        check('hidden: 50 events → 0 runs', runs === 0, 'runs=' + runs);
        doc.setHidden(false);
        check('shown: exactly one catch-up', catchUps === 1 && runs === 0, 'catchUps=' + catchUps + ' runs=' + runs);
        doc.setHidden(true); doc.setHidden(false);
        check('shown again with nothing missed: no catch-up', catchUps === 1, 'catchUps=' + catchUps);
    }

    // A debounce pending when the tab hides does not run; it becomes the catch-up.
    {
        const clk = fakeClock();
        const doc = fakeDoc();
        let runs = 0;
        const s = createLiveSchedule(() => { runs++; }, { debounceMs: 1500, doc, setTimeout: clk.setTimeout, clearTimeout: clk.clearTimeout });
        s.kick(); clk.advance(500);
        doc.setHidden(true);
        clk.advance(5000);
        check('pending run in a tab that hid: not run', runs === 0, 'runs=' + runs);
        doc.setHidden(false);
        check('… caught up once on show', runs === 1, 'runs=' + runs);
    }

    // Throttle: at most once per window, driven only by asks; touch counts.
    {
        let t = 0;
        const th = createEventThrottle(30000, () => t);
        const fired = [];
        for (t = 0; t <= 90000; t += 2000) if (th.ready()) fired.push(t);
        check('throttle: one per 30 s over 90 s of 2 s asks', JSON.stringify(fired) === JSON.stringify([0, 30000, 60000, 90000]), JSON.stringify(fired));
        t = 100000; th.touch();
        t = 129999;
        check('throttle: touch() counts as a read', th.ready() === false);
        t = 130000;
        check('throttle: ready again 30 s after the touch', th.ready() === true);
    }

    // Robot counts from the frame; keyless slots skipped (the cache skips them).
    {
        const c = countRobotAlerts([
            { vehicle_id: 'A', blocked: true },
            { vehicle_id: 'B', emergency: true, error: true },
            { vehicle_id: 'C' },
            { vehicle_id: '', blocked: true, error: true },
        ]);
        check('countRobotAlerts', c.robots_blocked === 1 && c.robots_emergency === 1 && c.robots_error === 1, JSON.stringify(c));
        const z = countRobotAlerts(null);
        check('countRobotAlerts(null) is zeros', z.robots_blocked === 0 && z.robots_emergency === 0 && z.robots_error === 0);
    }
}

// ─── Overview hero wiring ───────────────────────────────────────────────────
function htmlTag(strings, ...values) {
    let out = strings[0];
    for (let i = 0; i < values.length; i++) {
        const v = values[i];
        if (v === null || v === undefined || v === false) { /* skip */ }
        else if (typeof v === 'object' && v.__html === true) out += v.value;
        else out += String(v).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
        out += strings[i + 1];
    }
    return out;
}

function loadHero() {
    const clk = fakeClock();
    const doc = fakeDoc();
    const els = {};
    function el(id) {
        return els[id] || (els[id] = {
            id, innerHTML: '', className: '', children: [],
            classList: { add() {} },
            appendChild(c) { this.children.push(c); },
            querySelector() { return { textContent: '' }; },
        });
    }
    doc.getElementById = (id) => el(id);
    doc.createElement = () => el('anon-' + Math.random());

    const requests = [];
    let alertsAnswer = { total: 1, robots_blocked: 0, robots_emergency: 0, robots_error: 0, stuck_missions: 1, stuck_items: [{ order_id: 7 }] };
    const sse = {};
    const ctx = vm.createContext({
        Object, Array, Date, Infinity, URLSearchParams, Promise, JSON, Math, String, Number,
        document: doc,
        setTimeout: clk.setTimeout, clearTimeout: clk.clearTimeout,
        apiGet(url) {
            requests.push(url);
            if (url.startsWith('/api/missions/alerts')) return Promise.resolve(JSON.parse(JSON.stringify(alertsAnswer)));
            if (url.startsWith('/api/missions/active')) return Promise.resolve({ count: 3 });
            return Promise.resolve({ confirmed: 0, failed: 0, total: 0, success_rate: 0 });
        },
        h: htmlTag,
        onSSE(type, fn) { (sse[type] = sse[type] || []).push(fn); },
        debounce(fn, ms) { let id = null; return (...a) => { if (id !== null) clk.clearTimeout(id); id = clk.setTimeout(() => { id = null; fn(...a); }, ms); }; },
        formatDuration: (ms) => ms + 'ms',
        windowFor: () => ({ since: 'a', until: 'b', prevSince: 'c', prevUntil: 'd' }),
        KpiTile: () => el('tile-' + Math.random()),
        updateKpiTile() {},
        RUN_TIME_TITLE: '',
    });
    loadSchedule(ctx);
    // The live-schedule functions default to the real document/clock; bind
    // the fakes by wrapping them before the hero module captures them.
    const real = ctx.createLiveSchedule;
    ctx.createLiveSchedule = (run, opts) => real(run, Object.assign({}, opts, { doc, setTimeout: clk.setTimeout, clearTimeout: clk.clearTimeout }));
    const realTh = ctx.createEventThrottle;
    ctx.createEventThrottle = (ms) => realTh(ms, clk.now);
    const src = fs.readFileSync(path.join(__dirname, 'overview', 'hero.js'), 'utf8')
        .replace(/^import[^;]+;\s*/gm, '')
        .replace(/^export /gm, '');
    vm.runInContext(src + "\n;globalThis.__hero = createHeroSection({ get: () => ({ range: 'day', station: '', robot: '' }) });", ctx);
    const hero = ctx.__hero;
    hero.mount();
    return {
        clk, doc, requests, hero,
        emit(type, data) { (sse[type] || []).forEach((fn) => fn(data)); },
        banner: () => el('ops-alerts').innerHTML,
        setAlerts(a) { alertsAnswer = a; },
        count(prefix) { return requests.filter((u) => u.startsWith(prefix)).length; },
    };
}

const flush = () => new Promise((r) => setImmediate(r));

(async () => {
    // Open: the page's refresh reads the alerts once.
    {
        const t = loadHero();
        t.hero.refresh({ range: 'day', station: '', robot: '' });
        await flush();
        const base = t.count('/api/missions/alerts');
        check('open: one alerts read', base === 1, 'alerts=' + base);
        check('open: banner shows the stuck order', /1 active order stuck/.test(t.banner()), t.banner());

        // 120 s of robot ticks every 2 s, no order event: alerts at most once per 30 s, no active reads.
        const before = t.requests.length;
        for (let i = 0; i < 60; i++) {
            t.clk.advance(2000);
            t.emit('robot-update', [{ vehicle_id: 'R1', blocked: i >= 30 }, { vehicle_id: 'R2' }]);
            await flush();
        }
        const robotReqs = t.requests.slice(before);
        const alertsReads = robotReqs.filter((u) => u.startsWith('/api/missions/alerts')).length;
        check('robot-update: 60 ticks → 4 alerts reads (one per 30 s)', alertsReads === 4, 'alerts=' + alertsReads + ' ' + JSON.stringify(robotReqs));
        check('robot-update: no other request', robotReqs.length === alertsReads, JSON.stringify(robotReqs));
        // The last tick said R1 is blocked; the alerts answer (fixed) says 0
        // robots — the tick after the last read must win on the banner.
        t.emit('robot-update', [{ vehicle_id: 'R1', blocked: true }]);
        await flush();
        check('robot-update: banner counts robots from the frame', /1 robot blocked/.test(t.banner()) && /2 alerts/.test(t.banner()), t.banner());
    }

    // order-update: a burst is one active read + one alerts read.
    {
        const t = loadHero();
        t.hero.refresh({ range: 'day', station: '', robot: '' });
        await flush();
        const before = t.requests.length;
        for (let i = 0; i < 5; i++) { t.emit('order-update', { type: 'status_changed' }); t.clk.advance(200); }
        t.clk.advance(1500);
        await flush();
        const reqs = t.requests.slice(before);
        check('order-update burst → 1 active + 1 alerts', reqs.length === 2
            && reqs.filter((u) => u.startsWith('/api/missions/active')).length === 1
            && reqs.filter((u) => u.startsWith('/api/missions/alerts')).length === 1, JSON.stringify(reqs));
    }

    // Hidden tab: nothing; one catch-up on show.
    {
        const t = loadHero();
        t.hero.refresh({ range: 'day', station: '', robot: '' });
        await flush();
        t.doc.setHidden(true);
        const before = t.requests.length;
        for (let i = 0; i < 60; i++) {
            t.clk.advance(2000);
            t.emit('robot-update', [{ vehicle_id: 'R1' }]);
            if (i % 5 === 0) t.emit('order-update', {});
        }
        await flush();
        check('hidden: 120 s of events → 0 requests', t.requests.length === before, JSON.stringify(t.requests.slice(before)));
        t.doc.setHidden(false);
        await flush();
        const reqs = t.requests.slice(before);
        check('shown: one catch-up (active + alerts)', reqs.length === 2
            && reqs.some((u) => u.startsWith('/api/missions/active'))
            && reqs.some((u) => u.startsWith('/api/missions/alerts')), JSON.stringify(reqs));
    }

    if (failures) { console.log(failures + ' failure(s)'); process.exit(1); }
    console.log('all passed');
})().catch((e) => { console.log('FAIL harness: ' + (e && e.stack || e)); process.exit(1); });
