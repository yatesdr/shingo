// Unit tests for the /sourcing page's live-update triggers. Run under plain
// Node via the Go wrapper sourcing_reload_js_test.go. Exit 0 on pass, 1 on any
// assertion failure.
//
// The bug these exist to prevent: the page shipped with
// onSSE('connected', reload), which is an infinite loop — load, SSE connects,
// 'connected' fires, reload, connects again. It pulsed forever on an idle
// plant (field-observed at Springfield). The headline case here is
// "60 simulated seconds of an idle page produce zero reloads".

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

// htmlTag mirrors shared/utils.js's `h`: auto-escaping tagged template with an
// {__html:true,value} opt-out. Reimplemented rather than imported because the
// harness runs the module in a vm with no ES resolver — the same reason onSSE is
// injected. Kept behaviourally identical on the two things the panel relies on:
// escaping, and the opt-out.
function htmlTag(strings, ...values) {
    let out = strings[0];
    for (let i = 0; i < values.length; i++) {
        const v = values[i];
        if (Array.isArray(v)) out += v.join('');
        else if (v === null || v === undefined || v === false) { /* skip */ }
        else if (typeof v === 'object' && v.__html === true) out += v.value;
        else {
            out += String(v).replace(/&/g, '&amp;').replace(/</g, '&lt;')
                .replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
        }
        out += strings[i + 1];
    }
    return out;
}

// fixedNowDate pins Date.now() so a "held for" figure computed against the wall
// clock is a fixed number in a test. new Date(x) still behaves normally.
function fixedNowDate(iso) {
    const fixed = new Date(iso).getTime();
    const D = function (...args) { return args.length ? new (Date.bind.apply(Date, [null].concat(args)))() : new Date(fixed); };
    D.now = () => fixed;
    D.parse = Date.parse;
    D.UTC = Date.UTC;
    D.prototype = Date.prototype;
    return D;
}

// --- harness -------------------------------------------------------------
// Builds a context with a fake clock, a fake SSE bus, and just enough DOM for
// the module's rail/pane wiring and its verdict-history panel, then runs the
// real sourcing.js in it.
function load(opts) {
    opts = opts || {};
    const processes = opts.processes || ['SNF2', 'P42 NF1'];

    let now = 0;
    const timers = [];   // {at, fn, id}
    let nextTimerID = 1;
    let reloads = 0;
    const sseHandlers = {};
    const store = Object.assign({}, opts.session || {});

    function makeEl(dataset) {
        const el = {
            dataset: dataset || {},
            hidden: false,
            classList: { _s: new Set(), add(c) { this._s.add(c); }, contains(c) { return this._s.has(c); } },
            _attrs: {},
            _listeners: {},
            setAttribute(k, v) { this._attrs[k] = v; },
            getAttribute(k) { return this._attrs[k]; },
            addEventListener(ev, fn) { (this._listeners[ev] = this._listeners[ev] || []).push(fn); },
            focus() { ctxObj.document.activeElement = this; },
        };
        return el;
    }

    const tabs = processes.map(p => makeEl({ process: p }));
    const panes = processes.map(p => makeEl({ process: p }));

    const root = makeEl({});
    root.querySelectorAll = (sel) => (sel === '.src-rrow' ? tabs : panes);

    // Verdict-history hosts, one per process, same shape the template emits.
    // They carry innerHTML because that is what the panel writes and what the
    // assertions read back.
    const histHosts = processes.map((p) => {
        const el = makeEl({ process: p });
        el.innerHTML = '';
        return el;
    });

    // The panel's fetch. `events` null means "the request failed", which is a
    // different case from "the window holds no rows" and is asserted as such.
    const apiCalls = [];
    const feed = opts.feed;   // {since, events} | undefined (=> no rows)

    const docListeners = {};
    const ctxObj = {
        console,
        document: {
            getElementById: (id) => (id === 'src-root' ? root : null),
            querySelectorAll: (sel) => {
                if (sel === '.src-history') return histHosts;
                if (sel === '.src-history time[data-utc]') return [];
                return [];
            },
            addEventListener: (ev, fn) => { (docListeners[ev] = docListeners[ev] || []).push(fn); },
            hidden: !!opts.hidden,
            activeElement: null,
        },
        sessionStorage: {
            getItem: (k) => (k in store ? store[k] : null),
            setItem: (k, v) => { store[k] = String(v); },
        },
        window: { location: { reload() { reloads++; } } },
        setTimeout: (fn, ms) => { const id = nextTimerID++; timers.push({ at: now + ms, fn, id }); return id; },
        clearTimeout: () => {},
        Date: opts.nowISO ? fixedNowDate(opts.nowISO) : Date,
        onSSE: (name, fn) => { (sseHandlers[name] = sseHandlers[name] || []).push(fn); },
        // Injected in place of the ES import, same as onSSE.
        api: {
            get: (url) => {
                apiCalls.push(url);
                if (opts.apiFails) return Promise.reject('boom');
                return Promise.resolve(feed || { since: '2026-07-20T00:00:00Z', events: [] });
            },
        },
        h: htmlTag,
    };
    vm.createContext(ctxObj);

    const src = fs.readFileSync(path.join(__dirname, 'sourcing.js'), 'utf8')
        .replace(/^import[^;]+;\s*/m, '');   // drop the ES import; onSSE/api/h injected
    vm.runInContext(src, ctxObj);

    return {
        apiCalls,
        // The panel's render is async (it awaits the fetch); one microtask turn
        // is enough because the stubbed promise is already resolved.
        settle: () => Promise.resolve().then(() => Promise.resolve()),
        history: (process) => (histHosts.find((e) => e.dataset.process === process) || {}).innerHTML,
        fire(name, payload) { (sseHandlers[name] || []).forEach(fn => fn(payload)); },
        // advance the fake clock, running any timer that comes due
        tick(ms) {
            const end = now + ms;
            for (;;) {
                const due = timers.filter(t => t.at <= end).sort((a, b) => a.at - b.at);
                if (!due.length) break;
                const t = due[0];
                timers.splice(timers.indexOf(t), 1);
                now = t.at;
                t.fn();
            }
            now = end;
        },
        // Flip tab visibility and fire visibilitychange, as the browser does.
        setHidden(hidden) {
            ctxObj.document.hidden = hidden;
            (docListeners.visibilitychange || []).forEach(fn => fn());
        },
        // Simulate a click on an unlock-panel blocked-style link.
        clickGoto(process) {
            const link = {
                dataset: { gotoProcess: process },
                closest(sel) { return sel === '[data-goto-process]' ? link : null; },
            };
            (docListeners.click || []).forEach(fn => fn({ target: link, preventDefault() {} }));
        },
        reloads: () => reloads,
        session: () => store,
        tabs,
        panes,
        handlers: sseHandlers,
    };
}

// --- tests ---------------------------------------------------------------

console.log('sourcing: idle page does not reload');
{
    const h = load();
    // A real page connects once on load. That must not schedule anything.
    h.fire('connected');
    h.tick(60000);
    check('60s idle → zero reloads', h.reloads() === 0, 'got ' + h.reloads());
}

console.log('sourcing: first connect is not a reload trigger');
{
    const h = load();
    h.fire('connected');
    h.fire('connected');   // a duplicate first-connect must still not reload
    h.tick(30000);
    check('repeated first connects → zero reloads', h.reloads() === 0, 'got ' + h.reloads());
}

console.log('sourcing: reconnect after a real drop DOES reload');
{
    const h = load();
    h.fire('connected');       // initial
    h.fire('disconnected');    // the drop
    h.fire('connected');       // re-connect — events may have been missed
    h.tick(5000);
    check('drop then reconnect → one reload', h.reloads() === 1, 'got ' + h.reloads());
}

console.log('sourcing: a disconnect before any connect cannot arm a reload');
{
    const h = load();
    h.fire('disconnected');
    h.fire('connected');
    h.tick(30000);
    check('disconnect-first → zero reloads', h.reloads() === 0, 'got ' + h.reloads());
}

console.log('sourcing: verdict change reloads promptly');
{
    const h = load();
    h.fire('connected');
    h.fire('sourcing-update', { changed: 1 });
    h.tick(1000);
    check('not yet at 1s', h.reloads() === 0, 'got ' + h.reloads());
    h.tick(2000);
    check('reloaded by 3s', h.reloads() === 1, 'got ' + h.reloads());
}

console.log('sourcing: bin churn is coalesced hard, not strobed');
{
    const h = load();
    h.fire('connected');
    for (let i = 0; i < 200; i++) h.fire('bin-update');
    h.tick(5000);
    check('200 bin updates → no reload at 5s', h.reloads() === 0, 'got ' + h.reloads());
    h.tick(30000);
    check('one reload after the drift window', h.reloads() === 1, 'got ' + h.reloads());
}

console.log('sourcing: a hidden tab defers its reload until visible');
{
    const h = load({ hidden: true });
    h.fire('connected');
    h.fire('sourcing-update', { changed: 1 });
    h.tick(3000);
    check('hidden tab does not reload', h.reloads() === 0, 'got ' + h.reloads());
    h.setHidden(false);
    check('reloads once on return to foreground', h.reloads() === 1, 'got ' + h.reloads());
}

console.log('sourcing: a visible tab reloads as before');
{
    const h = load({ hidden: false });
    h.fire('connected');
    h.fire('sourcing-update', { changed: 1 });
    h.tick(3000);
    check('visible tab reloads promptly', h.reloads() === 1, 'got ' + h.reloads());
}

console.log('sourcing: unlock-panel link selects the blocked process');
{
    const h = load({ processes: ['SNF2', 'P42 NF1'] });
    h.clickGoto('P42 NF1');
    const visible = h.panes.filter(p => !p.hidden).map(p => p.dataset.process);
    check('goto link selects the target pane', visible.length === 1 && visible[0] === 'P42 NF1',
        'got ' + JSON.stringify(visible));
}

console.log('sourcing: selected process survives a reload');
{
    // Simulates the post-reload page: the rail re-renders server-side with the
    // first process selected, and the module restores the operator's choice
    // from sessionStorage.
    const h = load({ session: { 'sourcing:selected-process': 'P42 NF1' } });
    const selected = h.tabs.filter(t => t.getAttribute('aria-selected') === 'true')
        .map(t => t.dataset.process);
    check('restored selection', selected.length === 1 && selected[0] === 'P42 NF1',
        'got ' + JSON.stringify(selected));
    const visible = h.panes.filter(p => !p.hidden).map(p => p.dataset.process);
    check('matching pane visible', visible.length === 1 && visible[0] === 'P42 NF1',
        'got ' + JSON.stringify(visible));
}

console.log('sourcing: a stale saved process falls back to the first');
{
    const h = load({ session: { 'sourcing:selected-process': 'DECOMMISSIONED' } });
    const visible = h.panes.filter(p => !p.hidden).map(p => p.dataset.process);
    check('falls back to first pane', visible.length === 1 && visible[0] === 'SNF2',
        'got ' + JSON.stringify(visible));
}

// --- verdict-history panel (7.2 / S4) ------------------------------------
//
// The rules under test are the number doctrine's, not the panel's: a measured
// zero, an absence and a not-applicable must render differently from each other,
// a count travels with its window, and a failed fetch is not an empty result.
// Each of these has a plausible wrong implementation that looks fine on screen.

const FEED = {
    since: '2026-07-20T00:00:00Z',
    events: [
        // newest first, as the endpoint returns them
        { process_key: 'SNF2', style_id: 'A', status: 'green', missing_payload: '', reason: '', observed_at: '2026-07-26T09:41:00Z' },
        { process_key: 'SNF2', style_id: 'A', status: 'red', missing_payload: '76683-6SA0B.06', reason: 'missing 76683-6SA0B.06, 74577-6SA0A.06', observed_at: '2026-07-26T09:14:00Z' },
        { process_key: 'SNF2', style_id: 'A', status: 'green', missing_payload: '', reason: '', observed_at: '2026-07-26T09:13:30Z' },
    ],
};

(async () => {

console.log('sourcing/history: the endpoint is fetched, with an explicit limit');
{
    const t = load({ feed: FEED });
    await t.settle();
    const call = t.apiCalls[0] || '';
    check('fetches /api/sourceability/events', call.indexOf('/api/sourceability/events') === 0, 'got ' + call);
    check('sends an explicit limit', /[?&]limit=\d+/.test(call), 'got ' + call);
}

console.log('sourcing/history: the newest row reads as current, older rows carry a duration');
{
    const t = load({ feed: FEED, nowISO: '2026-07-26T10:41:00Z' });
    await t.settle();
    const html = t.history('SNF2');
    check('newest row says current', html.indexOf('current') >= 0, html);
    // 09:14 -> 09:41 is 27 minutes: the outage length, which is the number the
    // panel exists to show.
    check('the blocked spell reads 27m 00s', html.indexOf('27m 00s') >= 0, html);
    // The newest row has been in force an hour at the pinned clock.
    check('current state carries its age', html.indexOf('1h 00m') >= 0, html);
    // Compound, never decimal hours. "0.45h" is the failure this guards.
    check('no decimal hours anywhere', !/\d\.\d+\s*h/.test(html), html);
}

console.log('sourcing/history: a measured zero is a number, not a dash');
{
    const t = load({
        nowISO: '2026-07-26T09:20:00Z',
        feed: {
            since: '2026-07-20T00:00:00Z',
            events: [
                { process_key: 'SNF2', style_id: 'A', status: 'green', missing_payload: '', reason: '', observed_at: '2026-07-26T09:14:00Z' },
                // same second: a real 0 s, which shared/utils.js formatDuration
                // would render as '-' — i.e. as an absence.
                { process_key: 'SNF2', style_id: 'A', status: 'red', missing_payload: 'X', reason: 'missing X', observed_at: '2026-07-26T09:14:00Z' },
            ],
        },
    });
    await t.settle();
    const html = t.history('SNF2');
    check('a zero-length spell prints 0 s', html.indexOf('0 s') >= 0, html);
}

console.log('sourcing/history: not-applicable is not the same cell as no-data');
{
    const t = load({ feed: FEED, nowISO: '2026-07-26T10:41:00Z' });
    await t.settle();
    const html = t.history('SNF2');
    // A recovery is missing nothing BY CONSTRUCTION — n/a, not an em dash.
    check('a recovery row reads n/a for Missing', html.indexOf('n/a') >= 0, html);
    check('and does not use the no-data dash for it',
        html.indexOf('&mdash;') < 0, html);
    // The column holds only the FIRST missing payload; the full list is in
    // reason, and the panel must say so rather than truncate silently.
    check('a multi-payload reason is flagged', html.indexOf('and more') >= 0, html);
    check('the full list is in the title', html.indexOf('74577-6SA0A.06') >= 0, html);
}

console.log('sourcing/history: the count travels with its window');
{
    const t = load({ feed: FEED });
    await t.settle();
    const html = t.history('SNF2');
    check('prints the count', html.indexOf('3 changes') >= 0, html);
    check('prints the window beside it', /since\s/.test(html), html);
}

console.log('sourcing/history: an empty window says so, and says over what');
{
    const t = load({ feed: { since: '2026-07-20T00:00:00Z', events: [] } });
    await t.settle();
    const html = t.history('SNF2');
    check('no table frame is drawn', html.indexOf('<table') < 0, html);
    check('the empty state names the window', /since\s/.test(html), html);
    check('and explains that quiet is normal', html.indexOf('a quiet process') >= 0, html);
}

console.log('sourcing/history: a failed fetch is not an empty history');
{
    const t = load({ apiFails: true });
    await t.settle();
    const html = t.history('SNF2');
    check('says unavailable', html.indexOf('unavailable') >= 0, html);
    check('does NOT claim there were no changes', html.indexOf('No verdict change') < 0, html);
}

console.log('sourcing/history: an unknown status renders as itself');
{
    const t = load({
        feed: {
            since: '2026-07-20T00:00:00Z',
            events: [{ process_key: 'SNF2', style_id: 'A', status: 'amber_ish', missing_payload: '', reason: '', observed_at: '2026-07-26T09:14:00Z' }],
        },
    });
    await t.settle();
    const html = t.history('SNF2');
    // The vocabulary has grown before. A default arm rendering blank turns the
    // next addition into a silent data-loss bug in the UI.
    check('unknown status is printed verbatim', html.indexOf('amber_ish') >= 0, html);
}

console.log('sourcing/history: a process with no rows does not inherit another process\'s');
{
    const t = load({ processes: ['SNF2', 'P42 NF1'], feed: FEED });
    await t.settle();
    check('the other pane is empty-stated', (t.history('P42 NF1') || '').indexOf('No verdict change') >= 0,
        t.history('P42 NF1'));
}

if (failures) {
    console.error('\n' + failures + ' assertion(s) failed');
    process.exit(1);
}
console.log('\nall sourcing reload-trigger + verdict-history tests passed');

})();
