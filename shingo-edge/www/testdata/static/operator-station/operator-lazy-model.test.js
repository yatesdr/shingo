// operator-lazy-model.test.js — the station loads composer-model.js when a
// screen needs it, not at boot: what a cold board requests, what the style
// chip's flow panel and CHANGEOVER wait on, and what the glass says when the
// load is lost.
//
// Loads the SHIPPING modules whole, operator.js last, into one vm with a small
// fake DOM, a fake clock and a fake event stream, as operator-refresh.test.js
// does, but with the real operator-flow.js and composer-boot.js where that
// file stubs them. The shared files are the three the station imports
// (clock.js, bus.js, esc.js). window starts with no ComposerModel, as a page
// whose template carries no composer-model.js tag. Every tag composer-boot.js
// appends to <head> is kept and answered by the test (load or error); a model
// tag that loads runs the real composer-model.js in the same context.
//
// The two `import()`s are the lines rewritten (vm has no module loader):
// operator-flow.js's import of composer-boot.js answers with the module
// already loaded here, as the browser's module map does, and composer-boot.js's
// import of composer-render.js records the URL and sets window.ComposerUI.
//
// Runs under plain Node (no npm). Exit 0 = pass. Run by
// operator_lazy_model_test.go.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

// The page code under test. This file lives in www/testdata, outside the
// embedded static tree, so no binary carries or serves it
// (TestStaticTree_HoldsNoTestJS); the code it reads stays in www/static.
const SRC_DIR = path.join(__dirname, '..', '..', '..', 'static', 'operator-station');
const SHARED_DIR = path.join(SRC_DIR, '..', '..', '..', '..', 'shared');

// One station per run, so the wrapper runs this once per mode: 'cold' (boot,
// then the style chip and CHANGEOVER), 'flow-hash' (a cold load at #flow) and
// 'lost' (the model's request fails).
const MODE = process.argv[2] || 'cold';
if (['cold', 'flow-hash', 'lost'].indexOf(MODE) < 0) { console.error('unknown mode ' + MODE); process.exit(2); }

let failures = 0;
let finished = false;
process.on('exit', function (code) {
    if (!finished && code === 0) { console.error('FAIL: the test did not reach its end'); process.exitCode = 1; }
});
function check(name, cond, detail) {
    if (cond) { console.log('  ok   ' + name); return; }
    failures++;
    console.log('  FAIL ' + name + (detail ? '\n       ' + detail : ''));
}

// ── a small fake DOM ─────────────────────────────────────────────────────────

class FakeStyle {
    constructor() { this.props = {}; this.cssText = ''; }
    setProperty(k, v) { this.props[k] = v; }
    removeProperty(k) { delete this.props[k]; }
}

class FakeEl {
    constructor(tag) {
        this.tagName = String(tag).toUpperCase();
        this.children = [];
        this.parentNode = null;
        this.listeners = {};
        this.dataset = {};
        this.attrs = {};
        this.className = '';
        this.hidden = false;
        this._text = '';
        this._html = null;
        this._style = new FakeStyle();
        const self = this;
        this.classList = {
            add(c) { const l = self.className.split(/\s+/).filter(Boolean); if (l.indexOf(c) < 0) l.push(c); self.className = l.join(' '); },
            remove(c) { self.className = self.className.split(/\s+/).filter(x => x && x !== c).join(' '); },
            contains(c) { return self.className.split(/\s+/).indexOf(c) >= 0; },
            toggle(c, on) { if (on === undefined ? !this.contains(c) : on) this.add(c); else this.remove(c); },
        };
    }
    get style() { return this._style; }
    set style(v) { this._style.cssText = String(v); }
    get textContent() {
        if (this.children.length === 0) return this._text;
        return this._text + this.children.map(c => c.textContent).join('');
    }
    set textContent(v) { this._text = v == null ? '' : String(v); this._html = null; this.children = []; }
    get innerHTML() { return this._html != null ? this._html : this._text; }
    set innerHTML(v) { this._html = String(v); this._text = ''; this.children = []; }
    get firstChild() { return this.children[0] || null; }
    appendChild(c) { if (c.parentNode) c.remove(); c.parentNode = this; this.children.push(c); return c; }
    insertBefore(c, ref) {
        if (c.parentNode) c.remove();
        c.parentNode = this;
        const i = this.children.indexOf(ref);
        if (i < 0) this.children.push(c); else this.children.splice(i, 0, c);
        return c;
    }
    remove() {
        if (this.parentNode) {
            const i = this.parentNode.children.indexOf(this);
            if (i >= 0) this.parentNode.children.splice(i, 1);
        }
        this.parentNode = null;
    }
    setAttribute(k, v) { this.attrs[k] = String(v); }
    getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
    removeAttribute(k) { delete this.attrs[k]; }
    addEventListener(ev, fn) { (this.listeners[ev] = this.listeners[ev] || []).push(fn); }
    removeEventListener(ev, fn) { this.listeners[ev] = (this.listeners[ev] || []).filter(f => f !== fn); }
    // A listener that throws is recorded, as the browser would report it,
    // and the tap goes on: "nothing threw" is the check that reads it.
    click() {
        const evt = { target: this, currentTarget: this, stopPropagation() {}, preventDefault() {} };
        return Promise.all((this.listeners.click || []).map(fn => {
            try { return fn.call(this, evt); } catch (err) { errors.push('threw: ' + (err && err.stack || err)); return null; }
        }));
    }
    descendants() {
        const out = [];
        (function walk(n) { n.children.forEach(c => { out.push(c); walk(c); }); })(this);
        return out;
    }
    querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
    querySelectorAll(sel) {
        if (sel[0] === '.') return this.descendants().filter(n => n.classList.contains(sel.slice(1)));
        if (sel[0] === '#') return this.descendants().filter(n => n.id === sel.slice(1));
        return [];
    }
}

const byID = {};
const body = new FakeEl('body');
body.dataset.stationId = '7';
const head = new FakeEl('head');
// appended lists every asset composer-boot.js put in <head>, in order.
const appended = [];
const headAppend = head.appendChild.bind(head);
head.appendChild = c => { appended.push(c.dataset.composerAsset); return headAppend(c); };
const document = {
    body: body,
    head: head,
    readyState: 'complete',
    visibilityState: 'visible',
    listeners: {},
    addEventListener(ev, fn) { (this.listeners[ev] = this.listeners[ev] || []).push(fn); },
    getElementById(id) {
        const live = body.descendants().find(n => n.id === id);
        if (live) return live;
        // The flow panel's ids are the panel's own (mountFlowPanel builds
        // them); every other station id is a template element, made on demand.
        if (/^os-flow/.test(id)) return null;
        if (/^os-/.test(id) && id !== 'os-postcutover-flag' && id !== 'os-alerts') {
            if (!byID[id]) { byID[id] = new FakeEl('div'); byID[id].id = id; }
            return byID[id];
        }
        if (id === 'node-modal' || id === 'node-modal-content' || /^load-bin|^keypad/.test(id)) {
            if (!byID[id]) { byID[id] = new FakeEl('div'); byID[id].id = id; }
            return byID[id];
        }
        return null;
    },
    createElement(tag) { return new FakeEl(tag); },
    createElementNS(ns, tag) { return new FakeEl(tag); },
    createTextNode(t) { const n = new FakeEl('#text'); n._text = String(t); return n; },
    querySelector(sel) {
        const m = /^(link|script)\[data-composer-asset="([^"]+)"\]$/.exec(sel);
        if (m) return head.children.find(c => c.tagName === m[1].toUpperCase() && c.dataset.composerAsset === m[2]) || null;
        return body.querySelector(sel);
    },
    querySelectorAll(sel) { return body.querySelectorAll(sel); },
};

// ── the fake clock ───────────────────────────────────────────────────────────

let now = Date.UTC(2026, 9, 17, 12, 0, 0);
let timers = [];
let timerSeq = 0;
function fakeSetTimeout(fn, ms) {
    const t = { id: ++timerSeq, at: now + Math.max(0, ms || 0), fn: fn };
    timers.push(t);
    return t.id;
}
function fakeClearTimeout(id) { timers = timers.filter(t => t.id !== id); }
function fakeSetInterval(fn, ms) {
    const again = function () { fakeSetTimeout(again, ms); fn(); };
    return fakeSetTimeout(again, ms);
}
function flush() {
    let p = Promise.resolve();
    for (let i = 0; i < 6; i++) p = p.then(() => new Promise(r => setImmediate(r)));
    return p;
}
async function advance(ms) {
    const end = now + ms;
    await flush();
    for (;;) {
        timers.sort((a, b) => a.at - b.at || a.id - b.id);
        const t = timers[0];
        if (!t || t.at > end) break;
        timers.shift();
        now = t.at;
        t.fn();
        await flush();
    }
    now = end;
    await flush();
}
class FakeDate extends Date {
    constructor() {
        if (arguments.length === 0) super(now);
        else super(...arguments);
    }
    static now() { return now; }
}

// ── the network ──────────────────────────────────────────────────────────────

// A press station with one changeover cell: two single-robot press spots.
function pressView(rev) {
    return {
        station: { id: 7, name: 'Press station', health_status: 'online' },
        process: { id: 1, name: 'PRESS-1', active_style_id: 11, target_style_id: 11 },
        current_style: { id: 11, name: 'STYLE-1' },
        cell_version: 'v1',
        rev: rev || 0,
        nodes: [1, 2].map(id => ({
            node: { id: id, process_id: 1, core_node_name: 'PLN_' + id, name: 'PLN_' + id },
            runtime: { remaining_uop_cached: 5, active_bin_id: 1 },
            active_claim: { id: 40 + id, core_node_name: 'PLN_' + id, role: 'consume', swap_mode: 'single_robot',
                payload_code: 'PART-A', uop_capacity: 20 },
            orders: [],
            bin_state: { occupied: true, payload_code: 'PART-A', stocked: true, bin_label: 'BIN-' + id },
        })),
        bins: { known: true },
    };
}
function cellPicture() {
    return {
        version: 'v1', geometry: false, groups: {}, staging: [],
        positions: [1, 2].map(id => ({
            core_node_name: 'PLN_' + id, sequence: id, kind: 'front',
            claim: { swap_mode: 'single_robot', payload_code: 'PART-A', inbound_staging: 'SLN_' + id,
                outbound_staging: 'SLN_9' + id, inbound_source: 'SM', outbound_destination: 'SM' },
        })),
    };
}
let served = pressView();
const requests = [];
function response(status, text, headers) {
    const res = {
        ok: status >= 200 && status < 300, status: status,
        headers: { get(k) { return (headers || {})[String(k).toLowerCase()] || null; } },
        json: async () => JSON.parse(text),
        text: async () => text,
        clone() { return res; },
    };
    return res;
}
function fetchStub(url, opts) {
    requests.push({ method: (opts && opts.method) || 'GET', url: url });
    if (url === '/api/operator-stations/7/view') return Promise.resolve(response(200, JSON.stringify(served)));
    if (url === '/api/operator-stations/7/cell') return Promise.resolve(response(200, JSON.stringify(cellPicture())));
    if (/post-cutover-flag$/.test(url)) return Promise.resolve(response(200, '{"flagged":false}'));
    return Promise.resolve(response(200, '{}'));
}

// ── the event stream ─────────────────────────────────────────────────────────

const streams = [];
class FakeEventSource {
    constructor(url) { this.url = url; this.listeners = {}; this.closed = false; this.onerror = null; streams.push(this); }
    addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); }
    close() { this.closed = true; }
    emit(type, data) {
        (this.listeners[type] || []).forEach(fn => fn({ type: type, data: JSON.stringify(data), lastEventId: '' }));
    }
}
function fire(type, data) {
    const s = streams.filter(x => !x.closed).slice(-1)[0];
    if (s) s.emit(type, data);
}

// ── load the shipping modules ────────────────────────────────────────────────

const errors = [];
const imports = [];
let composerOpens = 0;
const win = {
    innerWidth: 1280, innerHeight: 800,
    location: { hash: MODE === 'flow-hash' ? '#flow' : '', pathname: '/operator/station/7' },
    listeners: {},
    addEventListener(ev, fn) { (this.listeners[ev] = this.listeners[ev] || []).push(fn); },
};
// THE MODEL'S READS, COUNTED. Before the model is in, window.ComposerModel is
// undefined, so a picture function that ran early throws (and is caught
// below). After, every call into the model is recorded by name.
const modelCalls = [];
let modelApi;
Object.defineProperty(win, 'ComposerModel', {
    configurable: true,
    get() { return modelApi; },
    set(api) {
        modelApi = api && new Proxy(api, { get(t, k) {
            const v = t[k];
            return typeof v === 'function' ? function () { modelCalls.push(String(k)); return v.apply(this, arguments); } : v;
        } });
    },
});
const ctx = vm.createContext({
    console: { log: console.log, warn: console.warn, error: (...a) => errors.push(a.map(String).join(' ')) },
    JSON, Math, Number, Array, Object, String, Promise, Error, TypeError, RegExp, Map, Set, URL, Proxy,
    parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent, AbortController,
    Date: FakeDate,
    setTimeout: fakeSetTimeout,
    clearTimeout: fakeClearTimeout,
    setInterval: fakeSetInterval,
    clearInterval: fakeClearTimeout,
    document: document,
    window: win,
    Node: FakeEl,
    EventSource: FakeEventSource,
    location: { reload() {} },
    fetch: fetchStub,
    openKeypad() {},
    __import(url) {
        imports.push(url);
        if (url === './composer-boot.js') return Promise.resolve(boot);
        if (/composer-render\.js/.test(url)) {
            win.ComposerUI = { openPicker() { composerOpens++; } };
            return Promise.resolve({});
        }
        return Promise.reject(new Error('the test does not serve ' + url));
    },
});
ctx.globalThis = ctx;
// Anything a picture function threw where a promise carried it.
process.on('unhandledRejection', err => { errors.push('unhandled: ' + (err && err.stack || err)); });

function loadModule(file, opts) {
    let src = fs.readFileSync(file, 'utf8');
    src = src.replace(/^import\s[\s\S]*?from\s+['"][^'"]+['"];?[ \t]*$/gm, '');
    if (opts && opts.imports !== undefined) {
        const n = (src.match(/\bimport\(/g) || []).length;
        if (n !== opts.imports) throw new Error(path.basename(file) + ' has ' + n + ' import() calls, this test rewrites ' + opts.imports + '; update loadModule');
        src = src.replace(/\bimport\(/g, '__import(').replace(/import\.meta\.url/g, '"http://edge.test/static/operator-station/composer-boot.js"');
    }
    const exported = [];
    src = src.replace(/^export\s+(async\s+function|function|const|let)\s+(\w+)/gm, function (_, kw, n) {
        exported.push(n);
        return kw + ' ' + n;
    });
    src = src.replace(/^export\s*\{[^}]*\};?/gm, '');
    const wrapped = '(function () {\n' + src + '\nreturn {' + exported.map(n => n + ': ' + n).join(', ') + '};\n})()';
    const out = vm.runInContext(wrapped, ctx, { filename: path.basename(file) });
    Object.assign(ctx, out);
    return out;
}

['clock.js', 'bus.js', 'esc.js'].forEach(f => loadModule(path.join(SHARED_DIR, f)));
ctx.serverNow = () => now;
const boot = loadModule(path.join(SRC_DIR, 'composer-boot.js'), { imports: 1 });
['order-status.js', 'operator-util.js', 'operator-state.js', 'operator-window-state.js',
    'operator-board-model.js', 'operator-board.js'].forEach(f => loadModule(path.join(SRC_DIR, f)));
loadModule(path.join(SRC_DIR, 'operator-flow.js'), { imports: 1 });
['operator-supply-refusal.js', 'operator-load-bin.js', 'operator-release.js', 'operator-modal.js',
    'operator-render.js', 'operator.js'].forEach(f => loadModule(path.join(SRC_DIR, f)));

const MODEL = '/static/operator-station/composer-model.js';
const modelSrc = fs.readFileSync(path.join(SRC_DIR, 'composer-model.js'), 'utf8');
// answer finds the newest unanswered tag for `asset` and answers it; a model
// tag that loads runs the real file, as the browser would.
function answer(asset, ev) {
    const t = head.children.slice().reverse().find(c => c.dataset.composerAsset === asset && !c.answered);
    if (!t) throw new Error('no unanswered ' + asset + ' tag (in <head>: ' + head.children.map(c => c.dataset.composerAsset).join(', ') + ')');
    t.answered = ev;
    if (ev === 'load' && asset === MODEL) vm.runInContext(modelSrc, ctx, { filename: 'composer-model.js' });
    (t.listeners[ev] || []).forEach(fn => fn({}));
}
function modelTags() { return appended.filter(a => a === MODEL).length; }
function styleChip() {
    const actions = document.getElementById('os-header-actions');
    return actions.descendants().find(n => n.classList.contains('os-header-style')) || null;
}
function flowPanel() { return document.getElementById('os-flow'); }
function flowSVG() { return document.getElementById('os-flow-svg'); }
function toastTexts() { return document.getElementById('os-toast').children.map(c => c.textContent); }
async function refreshes(n) {
    for (let i = 0; i < n; i++) {
        served = pressView(i + 1);
        fire('heartbeat', {});
        fire('material-refresh', { action: 'nudge' });
        await advance(5000);
    }
}

// ═════════════════════════════════════════════════════════════════════════════
function done() {
    console.log(failures ? '\n' + failures + ' failure(s)' : '\nall passed');
    finished = true;
    process.exitCode = failures ? 1 : 0;
}

(async function main() {

fire('connected', { build: 'b1' });
await advance(5000);

if (MODE === 'flow-hash') {
    // ── #flow on a cold load: the panel opens, and waits for the model ────
    console.log('#flow on a cold load');
    check('the board rendered (the header has its style chip)', !!styleChip());
    check('the panel asked for the model on its first draw', modelTags() === 1, appended.join(', '));
    check('and draws nothing until it is in', !!flowPanel() && flowPanel().hidden === true);
    answer(MODEL, 'load');
    await flush();
    check('the model in, the panel draws the cell', flowPanel().hidden === false && /PLN_1/.test(flowSVG().innerHTML));
    check('nothing threw', errors.length === 0, errors.join(' | '));
    done();
    return;
}

if (MODE === 'lost') {
    // ── a lost load says so, and waits for a tap ──────────────────────────
    console.log('the model\'s request fails');
    await styleChip().click();
    await flush();
    check('a tap requests the model', modelTags() === 1, appended.join(', '));
    answer(MODEL, 'error');
    await flush();
    check('the glass says so, in the station\'s words',
        toastTexts().indexOf('The flow did not load — tap STYLE again') >= 0, JSON.stringify(toastTexts()));
    check('the panel stays closed', flowPanel().hidden === true);
    check('no picture function ran', modelCalls.length === 0 && !errors.some(e => /TypeError/.test(e)),
        errors.join(' | '));
    await refreshes(6);
    check('six refreshes ask for nothing (no request per refresh)', modelTags() === 1, appended.join(', '));
    await styleChip().click();
    await flush();
    check('the next tap asks again', modelTags() === 2, appended.join(', '));
    answer(MODEL, 'load');
    await flush();
    check('and the panel draws', flowPanel().hidden === false && /PLN_1/.test(flowSVG().innerHTML));
    done();
    return;
}

// ── a cold board requests no model ──────────────────────────────────────────
console.log('a cold board');
check('the board rendered (the header has its style chip)', !!styleChip());
await refreshes(6);
check('boot and six refreshes request no composer-model.js', modelTags() === 0, appended.join(', '));
check('and no other deferred asset', appended.length === 0, appended.join(', '));
check('no model call ran', modelCalls.length === 0, modelCalls.join(', '));
check('nothing threw', errors.length === 0, errors.join(' | '));

// ── the style chip waits for the loader ─────────────────────────────────────
console.log('the style chip opens the flow through the loader');
await styleChip().click();
await flush();
check('a tap requests the model, once', modelTags() === 1, appended.join(', '));
check('through composer-boot.js (the one loader)', imports.indexOf('./composer-boot.js') >= 0, imports.join(', '));
check('the panel is not shown before the model is in', flowPanel().hidden === true);
await styleChip().click();
await refreshes(2);
check('a second tap and two refreshes during the load add no request', modelTags() === 1, appended.join(', '));
check('nothing threw while it waited (no picture function ran early)', errors.length === 0, errors.join(' | '));
answer(MODEL, 'load');
await flush();
check('the model in, the panel shows', flowPanel().hidden === false);
check('and draws the cell from the model', /PLN_1/.test(flowSVG().innerHTML) && modelCalls.length > 0,
    modelCalls.slice(0, 5).join(', '));
check('the viewBox is the picture\'s', /^0 0 \d+ \d+$/.test(flowSVG().getAttribute('viewBox') || ''), flowSVG().getAttribute('viewBox'));
const drawnAt = flowSVG().innerHTML;
await refreshes(2);
check('a refresh while open draws at once, with no request', modelTags() === 1 && flowSVG().innerHTML === drawnAt,
    appended.join(', '));

// ── CHANGEOVER waits for the same loader and finds the model in ─────────────
console.log('CHANGEOVER');
const coBtn = document.getElementById('os-header-actions').descendants().find(n => n.tagName === 'BUTTON' && /CHANGEOVER/.test(n.textContent));
check('the header has CHANGEOVER', !!coBtn);
// Not awaited: the tap's promise settles only once the tags below are answered.
const coTap = coBtn.click();
await flush();
answer('/static/operator-station/composer.css', 'load');
await flush();
answer('/static/operator-station/flowspec-data.js', 'load');
await flush();
await coTap;
check('the composer does not request the model again', modelTags() === 1, appended.join(', '));
check('and opens', composerOpens === 1 && imports.some(u => /composer-render\.js/.test(u)), imports.join(', '));
check('nothing threw', errors.length === 0, errors.join(' | '));
done();
})().catch(err => { console.error(err); finished = true; process.exitCode = 1; });
