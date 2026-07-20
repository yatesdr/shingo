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

// --- harness -------------------------------------------------------------
// Builds a context with a fake clock, a fake SSE bus, and just enough DOM for
// the module's rail/pane wiring, then runs the real sourcing.js in it.
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

    const ctxObj = {
        console,
        document: {
            getElementById: (id) => (id === 'src-root' ? root : null),
            activeElement: null,
        },
        sessionStorage: {
            getItem: (k) => (k in store ? store[k] : null),
            setItem: (k, v) => { store[k] = String(v); },
        },
        window: { location: { reload() { reloads++; } } },
        setTimeout: (fn, ms) => { const id = nextTimerID++; timers.push({ at: now + ms, fn, id }); return id; },
        clearTimeout: () => {},
        onSSE: (name, fn) => { (sseHandlers[name] = sseHandlers[name] || []).push(fn); },
    };
    vm.createContext(ctxObj);

    const src = fs.readFileSync(path.join(__dirname, 'sourcing.js'), 'utf8')
        .replace(/^import[^;]+;\s*/m, '');   // drop the ES import; onSSE is injected
    vm.runInContext(src, ctxObj);

    return {
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

if (failures) {
    console.error('\n' + failures + ' assertion(s) failed');
    process.exit(1);
}
console.log('\nall sourcing reload-trigger tests passed');
