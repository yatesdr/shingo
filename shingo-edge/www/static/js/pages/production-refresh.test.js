// production-refresh.test.js — the Production page's one /events stream drives
// #production-content's refreshes (C2).
//
// production-refresh.js is loaded through vm with its `export` keywords
// stripped, so its functions are globals of the context. Each refresh event
// must fire refresh('sse:<type>', data) — the DOM event name the hx-trigger
// listens for — and then the page's own handler for that type; the page's
// other handlers pass through untouched. createSSE's own mapping (handler
// name -> event type, shingoedge.js) is applied here too, so a handler name
// that would subscribe to the wrong event type fails.

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

const SRC = fs.readFileSync(path.join(__dirname, 'production-refresh.js'), 'utf8');
const context = vm.createContext({ Object, Array, String, console });
// `export const` becomes `var`: a top-level const is not a property of the
// context's global, a var (and a function declaration) is.
vm.runInContext(SRC.replace(/^export const /gm, 'var ').replace(/^export /gm, ''), context);
const { productionStreamHandlers, PRODUCTION_REFRESH_EVENTS } = context;

// createSSE's key -> event type rule, copied from shingoedge.js.
function eventTypeOf(key) {
    return key.substring(2).replace(/([A-Z])/g, '-$1').toLowerCase().substring(1);
}

const calls = [];
const refresh = (trigger, data) => calls.push(['refresh', trigger, data]);
const own = {
    onCounterUpdate: (data) => calls.push(['chart', data]),
    onCoreNodes: (data) => calls.push(['nodes', data]),
};
const handlers = productionStreamHandlers(refresh, own);

// --- the stream subscribes to every refresh event, plus the page's own ------
const types = Object.keys(handlers).map(eventTypeOf).sort();
const want = ['core-nodes', 'counter-update', 'order-failed', 'order-update'];
check('subscribed event types', types.join(',') === want.join(','), want.join(','), types.join(','));
check('the refresh list', PRODUCTION_REFRESH_EVENTS.join(',') === 'order-update,order-failed,counter-update',
    'order-update,order-failed,counter-update', PRODUCTION_REFRESH_EVENTS.join(','));

// --- each refresh event fires its sse: trigger -------------------------------
for (const type of ['order-update', 'order-failed']) {
    calls.length = 0;
    const key = Object.keys(handlers).find((k) => eventTypeOf(k) === type);
    handlers[key]({ id: 7 });
    check(`${type} fires sse:${type} once`,
        calls.length === 1 && calls[0][0] === 'refresh' && calls[0][1] === 'sse:' + type && calls[0][2].id === 7,
        `[refresh sse:${type}]`, JSON.stringify(calls));
}

// --- counter-update refreshes first, then the shift chart --------------------
calls.length = 0;
handlers.onCounterUpdate({ process_id: 3, delta: 2 });
check('counter-update fires sse:counter-update then the chart',
    calls.length === 2 && calls[0][1] === 'sse:counter-update' && calls[1][0] === 'chart',
    '[refresh sse:counter-update, chart]', JSON.stringify(calls));

// --- core-nodes passes through, no refresh ------------------------------------
calls.length = 0;
handlers.onCoreNodes({ nodes: ['LN-1'] });
check('core-nodes reaches the page handler only', calls.length === 1 && calls[0][0] === 'nodes',
    '[nodes]', JSON.stringify(calls));

// --- with no page handlers the refreshes still fire --------------------------
calls.length = 0;
productionStreamHandlers(refresh, undefined).onOrderFailed({});
check('no page handlers: order-failed still refreshes', calls.length === 1 && calls[0][1] === 'sse:order-failed',
    '[refresh sse:order-failed]', JSON.stringify(calls));

console.log(`${passed} passed, ${failures} failed`);
if (failures > 0) process.exit(1);
