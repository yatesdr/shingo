// containment.feedspin.test.js — the Quality Containment page's refresh, as it
// is before the page builds from the Edge's held copy.
//
// Today: a 5 s poll of /api/containment/state that reloads the page when the
// text changed, skipping the first poll (baseline), a hidden tab, a failed
// poll, and an open modal. Each check carries its predicted value after F2:
// no poll; a reload on the SSE `containment` event; while a modal is open the
// reload is held and applied on close.
//
// Loaded like changeover.refresh.test.js: the module run through
// vm.runInContext with the import line stripped and its imports supplied as
// globals. setInterval is captured, so each poll is one awaited call.

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let failures = 0;
let passed = 0;

function check(what, cond, expected, actual) {
    if (cond) { passed++; return; }
    failures++;
    console.log(`FAIL: ${what}\n  expected: ${expected}\n  actual:   ${actual}`);
}

const SRC = fs.readFileSync(path.join(__dirname, 'containment.js'), 'utf8');

function makeContext() {
    const intervals = [];
    const state = { body: 'A', ok: true, throws: false, modalOpen: false, hidden: false, fetches: 0, reloads: 0, sse: [] };
    const document = {
        get hidden() { return state.hidden; },
        body: {},
        querySelector(sel) { return state.modalOpen && sel === '.modal-overlay.active' ? {} : null; },
        getElementById() { return null; },
        addEventListener() {},
    };
    const context = vm.createContext({
        document,
        window: { location: { reload() { state.reloads++; } }, addEventListener() {} },
        api: { post: () => Promise.resolve({}) },
        confirm: () => Promise.resolve(true),
        toast: () => {},
        delegateActions: () => {},
        // Present so a post-F2 page that subscribes through createSSE runs
        // here; today the page does not import it.
        createSSE: (url, handlers) => { state.sse.push({ url, handlers }); return { close() {} }; },
        fetch: async (url) => {
            state.fetches++;
            if (state.throws) throw new Error('network');
            return { ok: state.ok, text: async () => state.body };
        },
        setInterval: (fn, ms) => { intervals.push({ fn, ms }); return intervals.length; },
        clearInterval: () => {},
        setTimeout: () => 0,
        clearTimeout: () => {},
        console, parseInt, JSON, Math, Array, Object, String, Number, Boolean, Promise, Error,
    });
    const src = SRC.replace(/^import \{[^}]+\} from [^\n]+\n+/m, '');
    vm.runInContext(src, context);
    const poll = async () => {
        if (intervals.length === 0) return;
        await intervals[0].fn();
    };
    return { state, intervals, poll };
}

(async () => {
    // --- the poll exists, every 5 s ------------------------------------------
    {
        const t = makeContext();
        // after: 0 intervals (F2: no poll)
        check('one interval is registered', t.intervals.length === 1, 1, t.intervals.length);
        // after: no interval (F2)
        check('at 5000 ms', t.intervals.length === 1 && t.intervals[0].ms === 5000, 5000, t.intervals[0] && t.intervals[0].ms);
        // after: one createSSE('/events', {onContainment}) subscription (F2)
        check('no SSE subscription', t.state.sse.length === 0, 0, t.state.sse.length);
        // after: the source names the containment SSE event (F2)
        check('the source polls /api/containment/state',
            SRC.indexOf("fetch('/api/containment/state')") >= 0, true, false);
    }

    // --- first poll is the baseline, unchanged text never reloads -------------
    {
        const t = makeContext();
        await t.poll();
        check('the first poll does not reload', t.state.reloads === 0, 0, t.state.reloads);
        await t.poll();
        await t.poll();
        // after: same in spirit — no reload without a containment event (F2)
        check('unchanged text does not reload', t.state.reloads === 0, 0, t.state.reloads);
        check('each tick fetches', t.state.fetches === 3, 3, t.state.fetches);
    }

    // --- changed text reloads -------------------------------------------------
    {
        const t = makeContext();
        await t.poll();
        t.state.body = 'B';
        await t.poll();
        // after: a reload on the SSE `containment` event instead of a poll (F2)
        check('changed text reloads once', t.state.reloads === 1, 1, t.state.reloads);
    }

    // --- an open modal: the change is consumed, not deferred -----------------
    //
    // The comment in containment.js says the open modal "defers the reload to
    // the next tick". The code stores the new text before the modal check, so
    // the next tick sees no change and the reload is lost until the text changes
    // again.
    {
        const t = makeContext();
        await t.poll();
        t.state.body = 'B';
        t.state.modalOpen = true;
        await t.poll();
        check('no reload while a modal is open', t.state.reloads === 0, 0, t.state.reloads);
        t.state.modalOpen = false;
        await t.poll();
        // after: 1 — the held reload is applied when the modal closes (F2)
        check('the change seen under the modal is not reloaded after it closes',
            t.state.reloads === 0, 0, t.state.reloads);
        t.state.body = 'C';
        await t.poll();
        check('the next change reloads', t.state.reloads === 1, 1, t.state.reloads);
    }

    // --- hidden tab, failed poll, thrown fetch --------------------------------
    {
        const t = makeContext();
        t.state.hidden = true;
        await t.poll();
        // after: no poll at all (F2)
        check('a hidden tab does not fetch', t.state.fetches === 0, 0, t.state.fetches);
        t.state.hidden = false;
        await t.poll();                       // baseline 'A'
        t.state.body = 'B';
        t.state.ok = false;
        await t.poll();
        check('a non-ok poll does not reload', t.state.reloads === 0, 0, t.state.reloads);
        t.state.ok = true;
        t.state.throws = true;
        await t.poll();
        check('a thrown fetch does not reload', t.state.reloads === 0, 0, t.state.reloads);
        t.state.throws = false;
        await t.poll();
        check('the change still reloads once Core answers', t.state.reloads === 1, 1, t.state.reloads);
    }

    if (failures > 0) {
        console.log(`\nFAILED: ${failures} assertion(s); ${passed} passed`);
        process.exit(1);
    }
    console.log(`OK: ${passed} assertions passed`);
})().catch((e) => { console.log('ERROR: ' + (e && e.stack || e)); process.exit(1); });
