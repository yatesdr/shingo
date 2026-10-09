// containment.feedspin.test.js — the Quality Containment page's refresh, now
// that the page builds from the Edge's held copy of Core's containment feed.
//
// Before F2: a 5 s poll of /api/containment/state that reloaded the page when
// the text changed, skipping the first poll (baseline), a hidden tab, a failed
// poll, and an open modal — which swallowed the change rather than deferring
// it. Each check below carries that base value beside it and asserts the F2
// value: no poll; a reload on the SSE `containment` event; while a dialog is
// open the reload is held and applied when the dialog closes.
//
// Loaded like changeover.refresh.test.js: the module run through
// vm.runInContext with the import line stripped and its imports supplied as
// globals. setInterval, createSSE, delegateActions and confirm are captured,
// so the test drives the SSE handler and the verbs directly.

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
    const state = {
        modalOpen: false, fetches: 0, reloads: 0, sse: [], actions: null,
        confirmResolve: null, posts: 0, postFails: false,
    };
    const document = {
        hidden: false,
        body: {},
        querySelector(sel) {
            if (!state.modalOpen) return null;
            return sel.split(',').some((s) => s.trim() === '.modal-overlay.active' || s.trim() === '.confirm-overlay') ? {} : null;
        },
        getElementById() { return null; },
        addEventListener() {},
    };
    const context = vm.createContext({
        document,
        window: { location: { reload() { state.reloads++; } }, addEventListener() {} },
        api: {
            post: () => {
                state.posts++;
                return state.postFails ? Promise.reject(new Error('refused')) : Promise.resolve({});
            },
        },
        // A dialog the test opens and closes: modalOpen is true while the
        // promise is pending, as the confirm overlay is in the DOM.
        confirm: () => new Promise((resolve) => {
            state.modalOpen = true;
            state.confirmResolve = (ok) => { state.modalOpen = false; resolve(ok); };
        }),
        toast: () => {},
        delegateActions: (_root, actions) => { state.actions = actions; },
        createSSE: (url, handlers) => { state.sse.push({ url, handlers }); return { close() {} }; },
        fetch: async () => { state.fetches++; return { ok: true, text: async () => '' }; },
        setInterval: (fn, ms) => { intervals.push({ fn, ms }); return intervals.length; },
        clearInterval: () => {},
        setTimeout: () => 0,
        clearTimeout: () => {},
        console, parseInt, JSON, Math, Array, Object, String, Number, Boolean, Promise, Error,
    });
    const src = SRC.replace(/^import \{[^}]+\} from [^\n]+\n+/m, '');
    vm.runInContext(src, context);
    const fire = () => {
        const sub = state.sse[0];
        if (sub && typeof sub.handlers.onContainment === 'function') sub.handlers.onContainment({ digest: 'd2' });
    };
    const tick = () => new Promise((r) => setImmediate(r));
    return { state, intervals, fire, tick };
}

(async () => {
    // --- no poll; one SSE subscription ---------------------------------------
    {
        const t = makeContext();
        // base: 1 interval at 5000 ms (F2: none)
        check('no interval is registered', t.intervals.length === 0, 0, t.intervals.length);
        // base: no SSE subscription (F2: one on /events with onContainment)
        check('one SSE subscription on /events', t.state.sse.length === 1 && t.state.sse[0].url === '/events',
            "1 on '/events'", JSON.stringify(t.state.sse.map((s) => s.url)));
        check('the subscription handles the containment event',
            t.state.sse.length === 1 && typeof t.state.sse[0].handlers.onContainment === 'function', true, false);
        // base: the source polled /api/containment/state (F2: it does not)
        check('the source no longer polls /api/containment/state',
            SRC.indexOf("fetch('/api/containment/state')") < 0, true, false);
        check('nothing is fetched', t.state.fetches === 0, 0, t.state.fetches);
    }

    // --- nothing reloads without an event; each event reloads once ------------
    {
        const t = makeContext();
        // base: the first poll was the baseline and unchanged text never
        // reloaded (F2: no reload without a containment event)
        check('no reload without an event', t.state.reloads === 0, 0, t.state.reloads);
        t.fire();
        // base: changed text reloaded once (F2: the event reloads once)
        check('a containment event reloads once', t.state.reloads === 1, 1, t.state.reloads);
    }

    // --- an open dialog holds the reload; cancel applies it -------------------
    //
    // base: the change seen under the modal was consumed and never reloaded
    // after it closed. F2: held, then applied when the dialog closes.
    {
        const t = makeContext();
        const pending = t.state.actions.unholdHeldBin({ dataset: { binId: '5' } });
        check('the dialog is open', t.state.modalOpen === true, true, t.state.modalOpen);
        t.fire();
        check('no reload while the dialog is open', t.state.reloads === 0, 0, t.state.reloads);
        t.state.confirmResolve(false);
        await pending;
        check('the held reload is applied when the dialog is cancelled', t.state.reloads === 1, 1, t.state.reloads);
        check('a cancelled dialog posts nothing', t.state.posts === 0, 0, t.state.posts);
        t.fire();
        check('the next event reloads', t.state.reloads === 2, 2, t.state.reloads);
    }

    // --- a confirmed verb reloads once; a failed one applies the held reload --
    {
        const t = makeContext();
        const pending = t.state.actions.unholdHeldBin({ dataset: { binId: '5' } });
        t.fire();
        t.state.confirmResolve(true);
        await pending;
        check('a confirmed verb reloads once (its own reload)', t.state.reloads === 1, 1, t.state.reloads);
    }
    {
        const t = makeContext();
        t.state.postFails = true;
        const pending = t.state.actions.verifyContainmentBin({ dataset: { node: 'A', binId: '11' } });
        t.fire();
        t.state.confirmResolve(true);
        await pending;
        await t.tick();
        check('a failed verb applies the held reload', t.state.reloads === 1, 1, t.state.reloads);
    }
    {
        const t = makeContext();
        t.state.postFails = true;
        const pending = t.state.actions.recallContained({ dataset: { payload: 'P' } });
        t.state.confirmResolve(true);
        await pending;
        await t.tick();
        check('a failed verb with nothing held does not reload', t.state.reloads === 0, 0, t.state.reloads);
    }

    if (failures > 0) {
        console.log(`\nFAILED: ${failures} assertion(s); ${passed} passed`);
        process.exit(1);
    }
    console.log(`OK: ${passed} assertions passed`);
})().catch((e) => { console.log('ERROR: ' + (e && e.stack || e)); process.exit(1); });
