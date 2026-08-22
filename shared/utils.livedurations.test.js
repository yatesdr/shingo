// Unit tests for installLiveDurations / renderLiveDurations in utils.js.
// Run via the Go wrapper live_durations_test.go. Exit 0 on pass, 1 on failure.
//
// Self-contained: brings a DOM stub with just enough querySelectorAll and
// parent/child structure for the notice-word swap, and a fake clock so the
// tick behaviour is deterministic.

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

// ─── DOM stub ─────────────────────────────────────────────────────────────
function node(attrs) {
    const e = {
        attrs: attrs || {},
        children: [],
        parentNode: null,
        textContent: '',
        hidden: false,
        getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; },
        setAttribute(k, v) { this.attrs[k] = v; },
        append(child) { child.parentNode = this; this.children.push(child); return child; },
        // Matches only the flat selectors this module uses: '[attr]'.
        querySelectorAll(sel) {
            const key = sel.replace('[', '').replace(']', '');
            const out = [];
            const walk = (n) => {
                for (const c of n.children) {
                    if (Object.prototype.hasOwnProperty.call(c.attrs, key)) out.push(c);
                    walk(c);
                }
            };
            walk(this);
            out.forEach = Array.prototype.forEach.bind(out);
            return out;
        },
        querySelector(sel) { return this.querySelectorAll(sel)[0] || null; },
    };
    return e;
}

// ─── module loader with a controllable clock ──────────────────────────────
function loadUtils(nowMs) {
    const src = fs.readFileSync(path.join(__dirname, 'utils.js'), 'utf8');
    const transformed = src
        .replace(/export\s+function\s+/g, 'function ')
        .replace(/export\s+const\s+/g, 'const ');

    const root = node({});
    const timers = [];
    const ctx = vm.createContext({
        document: Object.assign(root, {
            createElement: () => node({}),
            createTextNode: () => node({}),
            getElementById: () => null,
            addEventListener: () => {},
            body: { appendChild: () => {}, addEventListener: () => {} },
            readyState: 'complete',
        }),
        window: { addEventListener: () => {} },
        console,
        setTimeout, clearTimeout,
        setInterval: (fn) => { timers.push(fn); return timers.length; },
        clearInterval: (id) => { if (id) timers[id - 1] = null; },
        Date: Object.assign(function () {}, { now: () => nowMs.value, parse: Date.parse }),
        Math, Promise, Map, Set, JSON, Array, Object, String, Number, isNaN, parseInt,
        EventSource: function () {},
    });
    vm.runInContext(
        transformed +
        '; this.installLiveDurations = installLiveDurations;' +
        '  this.renderLiveDurations = renderLiveDurations;',
        ctx);
    return { ctx, root, timers };
}

const T0 = Date.parse('2026-08-22T14:00:00Z');

// ─── data-since counts up ─────────────────────────────────────────────────
{
    const now = { value: T0 + 14000 };
    const { ctx, root } = loadUtils(now);
    const span = root.append(node({ 'data-since': '2026-08-22T14:00:00Z' }));

    ctx.renderLiveDurations(root);
    // '14 s', with the space: the Go ladder's sub-minute form, which this one
    // now matches. It read '14s' while the two ladders disagreed.
    assert(span.textContent === '14 s', 'data-since renders elapsed, got ' + span.textContent);

    now.value = T0 + 192000; // 3m 12s
    ctx.renderLiveDurations(root);
    assert(span.textContent === '3m 12s', 'data-since re-renders on tick, got ' + span.textContent);
}

// ─── data-until counts down, and the past case ────────────────────────────
{
    const now = { value: T0 };
    const { ctx, root } = loadUtils(now);
    const until = root.append(node({ 'data-until': '2026-08-22T14:41:00Z' }));

    ctx.renderLiveDurations(root);
    // '41m': past ten minutes the Go ladder drops seconds, because they stop
    // carrying anything a reader acts on. It read '41m 0s' before unification.
    assert(until.textContent === '41m', 'data-until renders remaining, got ' + until.textContent);

    now.value = Date.parse('2026-08-22T14:41:01Z');
    ctx.renderLiveDurations(root);
    assert(until.textContent === '—', 'a past deadline renders an em-dash, got ' + until.textContent);

    // data-past overrides the dash — the staged-expiry span keeps "Expired".
    const withPast = root.append(node({ 'data-until': '2026-08-22T14:00:00Z', 'data-past': 'Expired' }));
    ctx.renderLiveDurations(root);
    assert(withPast.textContent === 'Expired', 'data-past wins over the dash, got ' + withPast.textContent);
}

// ─── the interval starts only with nodes, and stops when they go ──────────
{
    const now = { value: T0 };
    const { ctx, root, timers } = loadUtils(now);

    ctx.installLiveDurations(root);
    assert(timers.length === 0, 'no nodes => no interval armed');

    const span = root.append(node({ 'data-since': '2026-08-22T14:00:00Z' }));
    ctx.installLiveDurations(root);
    assert(timers.length === 1, 'a node arms exactly one interval');

    // A second install must not arm a second interval.
    ctx.installLiveDurations(root);
    assert(timers.length === 1, 'install is idempotent');

    // Remove the node; the next tick should clear the interval.
    root.children.length = 0;
    timers[0]();
    assert(timers[0] === null, 'the interval stops once nothing remains to tick');
    void span;
}

// ─── a past data-until alone does not keep the interval alive ─────────────
{
    const now = { value: Date.parse('2026-08-22T15:00:00Z') };
    const { ctx, root } = loadUtils(now);
    root.append(node({ 'data-until': '2026-08-22T14:00:00Z' }));
    assert(ctx.renderLiveDurations(root) === false,
        'an expired countdown is not live work');
}

// ─── the notice-word swap ─────────────────────────────────────────────────
{
    const now = { value: T0 + 14000 };
    const { ctx, root } = loadUtils(now);
    const line = root.append(node({}));
    const word = line.append(node({
        'data-notice-word': '',
        'data-notice-under': 'Replanning',
        'data-notice-over': 'Fault · cannot replan (60011)',
    }));
    word.textContent = 'Replanning';
    line.append(node({ 'data-since': '2026-08-22T14:00:00Z', 'data-notice-after': '60' }));
    const graceClause = line.append(node({ 'data-notice-only-over': '' }));
    graceClause.hidden = true;

    ctx.renderLiveDurations(root);
    assert(word.textContent === 'Replanning', 'under the threshold the word stays Replanning');
    assert(graceClause.hidden === true, 'the grace clause is hidden under the threshold');

    now.value = T0 + 61000;
    ctx.renderLiveDurations(root);
    assert(word.textContent === 'Fault · cannot replan (60011)',
        'crossing the threshold swaps to the fault wording, got ' + word.textContent);
    assert(graceClause.hidden === false, 'the grace clause appears over the threshold');

    // And back, if the clock is ever wound back (a re-render with new data).
    now.value = T0 + 5000;
    ctx.renderLiveDurations(root);
    assert(word.textContent === 'Replanning', 'the swap is a comparison, not a latch');
}

// ─── a node with no threshold is left alone ───────────────────────────────
{
    const now = { value: T0 + 600000 };
    const { ctx, root } = loadUtils(now);
    const line = root.append(node({}));
    const word = line.append(node({ 'data-notice-word': '', 'data-notice-under': 'a', 'data-notice-over': 'b' }));
    word.textContent = 'server-chose-this';
    line.append(node({ 'data-since': '2026-08-22T14:00:00Z' }));

    ctx.renderLiveDurations(root);
    assert(word.textContent === 'server-chose-this',
        'without data-notice-after the client never overrides the server wording');
}

// ─── malformed attributes are skipped, not thrown on ──────────────────────
{
    const now = { value: T0 };
    const { ctx, root } = loadUtils(now);
    const bad = root.append(node({ 'data-since': 'not-a-date' }));
    bad.textContent = 'untouched';
    let threw = false;
    try { ctx.renderLiveDurations(root); } catch (e) { threw = true; }
    assert(!threw, 'an unparseable instant must not throw');
    assert(bad.textContent === 'untouched', 'an unparseable instant is left alone');
}

// ─── THE LADDER, byte for byte against protocol.FormatDuration (Go) ───────
//
// This table is duplicated verbatim in protocol/duration_test.go. The two exist
// to be compared: the fault line is rendered once by the server through the Go
// ladder and then every second by this one, so any disagreement shows on the
// floor as a row that rewrites itself a second after it paints.
//
// If you change a row here, change it there, or the suites disagree and one of
// them is lying.
{
    const { ctx } = loadUtils({ value: T0 });
    const table = [
        [0, '0 s'],
        [999, '0 s'],
        [1000, '1 s'],
        [18000, '18 s'],
        [59999, '59 s'],
        [60000, '1m 00s'],
        [247000, '4m 07s'],
        [599000, '9m 59s'],
        [600000, '10m'],
        [1380000, '23m'],
        [3599000, '59m'],
        [3600000, '1h 00m'],
        [7500000, '2h 05m'],
        [86399000, '23h 59m'],
        [86400000, '1d 00h'],
        [273600000, '3d 04h'],
    ];
    for (const [ms, want] of table) {
        const got = ctx.formatDuration(ms);
        assert(got === want, 'formatDuration(' + ms + ') = ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
    }
    // Negative is floored at zero, as the Go ladder does — "-3 s" on the floor
    // reads as a bug in the page, not as two clocks disagreeing.
    assert(ctx.formatDuration(-3000) === '0 s', 'a negative duration floors at zero');
    // ABSENT is the dash, and it is a different thing from zero. Go never
    // renders it: BuildFaultLine omits the element rather than printing a
    // duration measured from the zero time.
    assert(ctx.formatDuration(null) === '—', 'null is the em dash, not "0 s"');
    assert(ctx.formatDuration(undefined) === '—', 'undefined is the em dash');
    assert(ctx.formatDuration(NaN) === '—', 'NaN is the em dash');
}

// ─── the first live render must not change the server's text ──────────────
//
// THE WHOLE POINT OF THE SHARED LADDER. The server renders the elapsed into the
// span; one second later this module overwrites it. If the two ladders disagree
// the row visibly rewrites itself, which is what a faulted order did on every
// board before the ladders were unified.
{
    const now = { value: Date.parse('2026-08-22T14:04:07Z') };
    const { ctx, root } = loadUtils(now);
    const line = root.append(node({}));
    const clock = line.append(node({ 'data-since': '2026-08-22T14:00:00Z' }));
    // Exactly what protocol.FormatDuration produced server-side for 4m07s.
    const serverText = '4m 07s';
    clock.textContent = serverText;

    ctx.renderLiveDurations(root);
    assert(clock.textContent === serverText,
        'first live render changed the server text from ' + JSON.stringify(serverText) +
        ' to ' + JSON.stringify(clock.textContent) + ' — the ladders have drifted');
}

console.log('live durations: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed === 0 ? 0 : 1);
