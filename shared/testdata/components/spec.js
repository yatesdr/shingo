// spec.js — the small runner the component specs share (layer.test.js,
// notice.test.js, http.test.js, picker.test.js). A page is a realm from the
// helper pins' world.js: its DOM, fake clock, fetch and ES-module loader, so
// the components load unmodified, as a page loads them.
//
// A case names what it holds. A case that starts "cannot happen:" names a bug
// the P0 pins found in today's copies, at the file:line the P0 report gave it
// (REPORT-v2-stop1-P0), and holds that the component cannot do it.

'use strict';

const { makeRealm, Event } = require('../helper-pins-harness/world.js');

const cases = [];
function test(name, fn) { cases.push({ name, fn }); }

// page is a fresh tab with the named shared modules loaded.
function page(...names) {
    const r = makeRealm({ surface: 'edge' });
    const mods = names.map((n) => r.load('/static/shared/' + n));
    return { r, doc: r.doc, mods };
}

function mk(r, tag, attrs, kids) {
    const e = r.doc.createElement(tag);
    Object.keys(attrs || {}).forEach((k) => e.setAttribute(k, attrs[k]));
    (kids || []).forEach((k) => e.appendChild(typeof k === 'string' ? r.doc.createTextNode(k) : k));
    return e;
}
function ev(type, extra) { return new Event(type, Object.assign({ bubbles: true, cancelable: true }, extra || {})); }
function key(target, k) { const e = ev('keydown', { key: k }); target.dispatchEvent(e); return e; }
// press is a pointer pressed on down and released on up: the click lands on
// up, as the browser retargets a drag's click.
function press(down, up) {
    down.dispatchEvent(ev('mousedown'));
    (up || down).dispatchEvent(ev('click'));
}
function watch(p) {
    const w = { state: 'pending', value: undefined };
    p.then((v) => { w.state = 'resolved'; w.value = v; }, (e) => { w.state = 'rejected'; w.value = e; });
    return w;
}

class Fail extends Error {}
function eq(got, want, what) {
    const g = JSON.stringify(got), w = JSON.stringify(want);
    if (g !== w) throw new Fail((what ? what + ': ' : '') + 'got ' + g + ', want ' + w);
}
function ok(v, what) { if (!v) throw new Fail(what || 'expected true'); }

async function run() {
    let failed = 0;
    for (const c of cases) {
        try {
            await c.fn();
            console.log('ok   ' + c.name);
        } catch (e) {
            failed++;
            console.log('FAIL ' + c.name + '\n     ' + (e instanceof Fail ? e.message : (e && e.stack) || e));
        }
    }
    console.log((cases.length - failed) + '/' + cases.length + ' passed');
    process.exitCode = failed ? 1 : 0;
}

module.exports = { test, run, page, mk, ev, key, press, watch, eq, ok };
