// shingoedge.refresh.test.js — the Edge hub's refreshOnEvents: an htmx
// partial's "sse:<type>" triggers fired from the page's one bus stream, for
// the three partials that carry them (Production, Changeover, Orders).
//
// refreshOnEvents and sseTriggerTypes are cut out of shingoedge.js and run in
// a vm context with a stub document, a recording onSSE (the page passes
// shared/bus.js's) and a recording htmx. Each partial's hx-trigger is read off
// its template, so a trigger added there is subscribed here without a list to
// keep in step: each sse: name must become one subscription, in the
// template's order, and each frame of that type one htmx.trigger(element,
// 'sse:<type>', data) — the DOM event the hx-trigger listens for. htmx-sse.js
// used to do this from a second stream it opened on the element.

const fs = require('fs');
const path = require('path');
const vm = require('vm');

// The code under test. This file lives in www/testdata, outside the embedded
// static tree, so no binary carries or serves it (TestStaticTree_HoldsNoTestJS).
const WWW = path.join(__dirname, '..', '..', '..');
const HUB = fs.readFileSync(path.join(WWW, 'static', 'js', 'shingoedge.js'), 'utf8');

let failures = 0;
let passed = 0;

function check(what, cond, expected, actual) {
    if (cond) { passed++; return; }
    failures++;
    console.log(`FAIL: ${what}\n  expected: ${expected}\n  actual:   ${actual}`);
}

// cut returns one top-level function of the hub, from its line to the brace
// that closes it, `export` dropped.
function cut(name) {
    const m = new RegExp('^(?:export )?function ' + name + '\\(', 'm').exec(HUB);
    if (!m) throw new Error('shingoedge.js has no function ' + name + '; the cut has drifted');
    let i = HUB.indexOf('{', m.index);
    for (let depth = 0; i < HUB.length; i++) {
        if (HUB[i] === '{') depth++;
        else if (HUB[i] === '}' && --depth === 0) break;
    }
    return HUB.slice(m.index, i + 1).replace(/^export /, '');
}

// triggerOf reads an element's hx-trigger off its template.
function triggerOf(template, id) {
    const html = fs.readFileSync(path.join(WWW, 'templates', template), 'utf8');
    const m = new RegExp('<div[^>]*\\bid="' + id + '"[^>]*>', 's').exec(html);
    if (!m) throw new Error(template + ': no #' + id);
    const t = /hx-trigger="([^"]*)"/.exec(m[0]);
    if (!t) throw new Error(template + ': #' + id + ' has no hx-trigger');
    return t[1];
}

// page is one tab: the elements by id, the subscriptions made, the triggers
// fired.
function page(elements) {
    const p = { subs: [], fired: [], elements: Object.assign({}, elements) };
    const ctx = vm.createContext({
        document: { getElementById: (id) => p.elements[id] || null },
        window: { htmx: { trigger: (el, name, data) => p.fired.push([el.id, name, data]) } },
        Array, String, console,
    });
    vm.runInContext(cut('sseTriggerTypes') + '\n' + cut('refreshOnEvents'), ctx);
    p.refreshOnEvents = ctx.refreshOnEvents;
    p.window = ctx.window;
    p.onSSE = (type, fn) => { p.subs.push([type, fn]); return () => {}; };
    p.frame = (type, data) => p.subs.filter((s) => s[0] === type).forEach((s) => s[1](data, { type }));
    return p;
}
function element(id, trigger) {
    return { id, getAttribute: (k) => (k === 'hx-trigger' ? trigger : null) };
}

const PARTIALS = [
    ['production.html', 'production-content', ['order-update', 'order-failed', 'counter-update']],
    ['changeover.html', 'changeover-content', ['order-update', 'order-failed', 'changeover-update']],
    ['orders.html', 'orders-content', ['order-update', 'order-completed', 'order-failed']],
];

for (const [template, id, want] of PARTIALS) {
    const trig = triggerOf(template, id);
    const p = page({ [id]: element(id, trig) });
    const n = p.refreshOnEvents(id, p.onSSE);
    const types = p.subs.map((s) => s[0]);
    check(`${template}: one subscription per sse: trigger, in order`, types.join(',') === want.join(','),
        want.join(','), types.join(','));
    check(`${template}: returns the count`, n === want.length, want.length, n);
    for (const type of want) {
        p.fired.length = 0;
        p.frame(type, { id: 7 });
        check(`${template}: a ${type} frame fires sse:${type} on #${id} once`,
            p.fired.length === 1 && p.fired[0][0] === id && p.fired[0][1] === 'sse:' + type && p.fired[0][2].id === 7,
            `[${id} sse:${type}]`, JSON.stringify(p.fired));
    }
}

// --- the element is looked up on each frame ---------------------------------
{
    const p = page({ 'orders-content': element('orders-content', 'refreshOrders from:body, sse:order-update throttle:3s') });
    p.refreshOnEvents('orders-content', p.onSSE);
    delete p.elements['orders-content'];
    p.frame('order-update', {});
    check('a frame after the element left the page fires nothing', p.fired.length === 0, 0, p.fired.length);
    p.elements['orders-content'] = element('orders-content', '');
    p.frame('order-update', {});
    check('a frame once a new element of that id is in fires on it', p.fired.length === 1, 1, p.fired.length);
    p.window.htmx = undefined;
    let threw = null;
    try { p.frame('order-update', {}); } catch (e) { threw = e; }
    check('no htmx on the page: a frame does nothing and does not throw', threw === null && p.fired.length === 1, 'no throw', String(threw));
}

// --- nothing to subscribe ---------------------------------------------------
{
    const p = page({});
    check('no such element: 0, nothing subscribed', p.refreshOnEvents('changeover-content', p.onSSE) === 0 && p.subs.length === 0,
        '0 and none', p.subs.length);
    const q = page({ x: element('x', 'refreshX from:body, sse:') });
    check('an element with no sse:<type> trigger: 0, nothing subscribed', q.refreshOnEvents('x', q.onSSE) === 0 && q.subs.length === 0,
        '0 and none', q.subs.length);
}

console.log(`${passed} passed, ${failures} failed`);
if (failures > 0) process.exit(1);
