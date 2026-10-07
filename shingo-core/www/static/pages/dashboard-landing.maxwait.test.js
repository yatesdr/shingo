// dashboard-landing.maxwait.test.js — W4 follow-up (ui-cleanup 2026-10-07).
// The Dashboard's Active Orders card refreshes on order-update through
// debounceMaxWait(refreshActiveOrders, 1500, 10000). A plain trailing debounce
// never fired under a steady stream (events < 1.5 s apart); the max wait makes
// it fire at least every 10 s. With sparse events it is the trailing debounce.
//
// Runs under plain node through its Go wrapper
// (dashboard_landing_maxwait_wrapper_test.go). debounceMaxWait is cut out of
// the page script by name and run on a fake clock.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0;
let failed = 0;
function check(cond, label, got) {
    if (cond) { passed++; } else { failed++; console.error('FAIL: ' + label + (got !== undefined ? ' (got ' + got + ')' : '')); }
}

const src = fs.readFileSync(path.join(__dirname, 'dashboard-landing.js'), 'utf8');
const m = /function debounceMaxWait\(fn, waitMs, maxMs\) \{[\s\S]*?\n\}/.exec(src);
if (!m) { console.error('FAIL: debounceMaxWait not found in dashboard-landing.js'); process.exit(1); }
if (src.indexOf("onSSE('order-update', debounceMaxWait(refreshActiveOrders, 1500, 10000))") < 0) {
    console.error('FAIL: order-update is not wired through debounceMaxWait(refreshActiveOrders, 1500, 10000)');
    process.exit(1);
}

// Fake clock: setTimeout/clearTimeout over a sorted timer list.
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

function harness() {
    const clock = makeClock();
    const ctx = vm.createContext({ setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout });
    vm.runInContext(m[0] + '\n; this.debounceMaxWait = debounceMaxWait;', ctx);
    const fires = [];
    const d = ctx.debounceMaxWait(() => fires.push(clock.now()), 1500, 10000);
    return { clock, fires, d };
}

// A steady stream: one event per second for 30 s.
(function steadyStream() {
    const h = harness();
    for (let t = 0; t < 30000; t += 1000) { h.clock.advanceTo(t); h.d(); }
    h.clock.advanceTo(60000);
    check(h.fires.length === 3, 'event every 1 s for 30 s: 3 refreshes', h.fires.length);
    let gap = 0;
    for (let i = 1; i < h.fires.length; i++) gap = Math.max(gap, h.fires[i] - h.fires[i - 1]);
    check(h.fires[0] <= 10000 && gap <= 10000, 'no gap over 10 s while events keep coming', h.fires.join(','));
})();

// A single event: one refresh, 1.5 s later.
(function single() {
    const h = harness();
    h.d();
    h.clock.advanceTo(60000);
    check(h.fires.length === 1 && h.fires[0] === 1500, 'one event: 1 refresh at +1.5 s', h.fires.join(','));
})();

// Sparse events (5 s apart): each one refreshes 1.5 s later, as before.
(function sparse() {
    const h = harness();
    for (let t = 0; t < 20000; t += 5000) { h.clock.advanceTo(t); h.d(); }
    h.clock.advanceTo(60000);
    check(h.fires.join(',') === '1500,6500,11500,16500', 'sparse events: trailing debounce unchanged', h.fires.join(','));
})();

// A short burst (5 events 200 ms apart): one refresh after the last.
(function burst() {
    const h = harness();
    for (let t = 0; t <= 800; t += 200) { h.clock.advanceTo(t); h.d(); }
    h.clock.advanceTo(60000);
    check(h.fires.join(',') === '2300', 'short burst: 1 refresh, 1.5 s after the last event', h.fires.join(','));
})();

console.log(passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
