// Unit tests for the BOOT WATCHDOG — the classic inline script in
// templates/operator-display.html that reloads an operator board whose module
// graph failed to link.
//
// WHY THIS IS PINNED. Hopkinsville 2026-09-08: a stray `}` in
// operator-release.js meant the operator-station ES module graph never linked,
// so operator.js ran zero lines and every HMI sat on the "Loading..."
// placeholder. createSSE already reloads a board on a build-id change — but it
// lives INSIDE the graph it protects, so the break disabled its own recovery
// and the boards had to be refreshed by hand on the floor.
//
// The four rules this pins, all of which are ways to get it subtly wrong:
//   1. A HEALTHY board must not arm — otherwise every tablet holds a second
//      EventSource forever, doubling SSE clients for no reason.
//   2. A BROKEN board must arm and say so, so the floor sees a diagnosis
//      instead of a mute "Loading...".
//   3. The SAME build id must NOT reload. EventSource reconnects on its own
//      and re-delivers `connected` every time; reloading on that is a reload
//      loop against an unchanged, still-broken build.
//   4. A CHANGED build id must reload exactly once — that is the self-heal,
//      and it is what makes restarting the edge a fleet-wide HMI reload.
//
// Extracts the script from the SHIPPING TEMPLATE rather than a copy, matching
// the extraction convention in the sibling tests: a copy drifts, and a
// watchdog that has drifted from the page is worse than none.
//
// Runs under plain Node (no npm). Exit 0 = pass, 1 = any failure. Run via the
// Go wrapper operator_boot_watchdog_test.go.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
function eq(got, want, label) {
    if (got === want) { passed++; return; }
    failed++;
    console.error('FAIL: ' + label + '\n   got:  ' + JSON.stringify(got) + '\n   want: ' + JSON.stringify(want));
}
function ok(cond, label) { eq(!!cond, true, label); }

// ─── Extract the watchdog from the template ───

const tplPath = path.join(__dirname, '..', '..', 'templates', 'operator-display.html');
const tpl = fs.readFileSync(tplPath, 'utf8');

// The watchdog is the only NON-module <script> block on the page; the two
// module tags carry type="module" and are matched by a different pattern.
const blocks = tpl.match(/<script>\s*([\s\S]*?)\s*<\/script>/g) || [];
eq(blocks.length, 1, 'exactly one classic <script> block in operator-display.html');

const watchdogSrc = blocks[0].replace(/^<script>\s*/, '').replace(/\s*<\/script>$/, '');

// If a Go template action ever lands inside the body, this test is running
// something the browser never sees — and, worse, the shipped script would be
// whatever the action rendered. Fail loudly rather than test a fiction.
ok(watchdogSrc.indexOf('{{') === -1, 'no template action inside the watchdog body');
ok(watchdogSrc.indexOf('__osBooted') !== -1, 'watchdog checks the boot flag');

// ─── Harness ───

// Builds a sandbox, runs the watchdog in it, and hands back the captured timer
// plus the recorders the assertions read.
function boot() {
    const state = {
        timer: null,
        timerDelay: null,
        reloads: 0,
        headerText: null,
        sources: [],
    };

    function FakeEventSource(url) {
        this.url = url;
        this.listeners = {};
        state.sources.push(this);
    }
    FakeEventSource.prototype.addEventListener = function (type, fn) {
        (this.listeners[type] = this.listeners[type] || []).push(fn);
    };
    // Deliver a `connected` frame the way sse.go writes it.
    FakeEventSource.prototype.emitConnected = function (data) {
        (this.listeners['connected'] || []).forEach(function (fn) { fn({ data: data }); });
    };

    const headerEl = {};
    Object.defineProperty(headerEl, 'textContent', {
        get: function () { return state.headerText; },
        set: function (v) { state.headerText = v; },
    });

    const sandbox = {
        window: {},
        document: {
            getElementById: function (id) { return id === 'os-header-info' ? headerEl : null; },
        },
        location: { reload: function () { state.reloads++; } },
        EventSource: FakeEventSource,
        setTimeout: function (fn, ms) { state.timer = fn; state.timerDelay = ms; return 1; },
        JSON: JSON,
    };

    vm.createContext(sandbox);
    vm.runInContext(watchdogSrc, sandbox, { filename: 'operator-display-watchdog.js' });

    state.sandbox = sandbox;
    return state;
}

// ─── 1. The watchdog arms a timer at load, and does nothing before it ───

const armed = boot();
ok(typeof armed.timer === 'function', 'arms a deadline timer at page load');
eq(armed.timerDelay, 15000, 'deadline is 15s (modules link in ms; this only fires on a real link failure)');
eq(armed.sources.length, 0, 'opens no EventSource before the deadline');
eq(armed.reloads, 0, 'no reload before the deadline');

// ─── 2. A HEALTHY board must not arm ───

const healthy = boot();
healthy.sandbox.window.__osBooted = true;
healthy.timer();
eq(healthy.sources.length, 0, 'healthy board opens NO second EventSource');
eq(healthy.reloads, 0, 'healthy board never reloads');
eq(healthy.headerText, null, 'healthy board leaves the header alone');

// ─── 3. A BROKEN board arms, and says so ───

const broken = boot();
broken.timer(); // __osBooted never set — the graph did not link
eq(broken.sources.length, 1, 'broken board opens exactly one EventSource');
eq(broken.sources[0].url, '/events', 'connects to the SSE endpoint');
ok(/failed to load/i.test(broken.headerText || ''), 'replaces the mute "Loading..." with a diagnosis');

// ─── 4. The same build id must NOT reload (no spin loop) ───

const same = boot();
same.timer();
const es1 = same.sources[0];
es1.emitConnected('{"build":"18d3343337d02259"}');
eq(same.reloads, 0, 'first connected frame only records the build');
es1.emitConnected('{"build":"18d3343337d02259"}');
es1.emitConnected('{"build":"18d3343337d02259"}');
eq(same.reloads, 0, 'reconnects reporting the SAME build never reload');

// ─── 5. A CHANGED build id reloads — the self-heal ───

const changed = boot();
changed.timer();
const es2 = changed.sources[0];
es2.emitConnected('{"build":"18d3343337d02259"}');  // the broken build
eq(changed.reloads, 0, 'still waiting');
es2.emitConnected('{"build":"18d3568cd942cdc3"}');  // a fix was deployed
eq(changed.reloads, 1, 'a NEW build id reloads the board exactly once');

// ─── 6. Malformed / missing build data must not reload ───

const junk = boot();
junk.timer();
const es3 = junk.sources[0];
es3.emitConnected('not json at all');
es3.emitConnected('{}');
es3.emitConnected('{"build":""}');
eq(junk.reloads, 0, 'unparseable or empty build payloads are ignored, not acted on');

// ─── Report ───

console.log('operator boot watchdog: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed === 0 ? 0 : 1);
