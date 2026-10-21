// Safe taps on the operator station: a modal closed or refused leaves nothing
// dead or frozen, a finished request closes only the modal it came from, and
// one tap sends one request.
//
// Loads the SHIPPING modules whole (operator-render, operator-board,
// operator-modal, operator-release, operator-load-bin, operator-supply-refusal,
// operator-board-model, operator-window-state, operator-state, operator-util,
// order-status) into one vm with a small fake DOM, as
// operator-loader-station.test.js does, with the network held where a test
// needs a request in flight, failed where it needs a refusal.
//
// Runs under plain Node (no npm). Exit 0 = pass, 1 = any failure. Run via the
// Go wrapper operator_safe_taps_test.go.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
// A tap whose request is held forever leaves nothing on the event loop and
// node exits 0 halfway through; a run that did not reach its end fails.
let finished = false;
process.on('exit', function (code) {
    if (!finished && code === 0) { console.error('FAIL: the test did not reach its end'); process.exitCode = 1; }
});
function eq(got, want, label) {
    const g = JSON.stringify(got), w = JSON.stringify(want);
    if (g === w) { passed++; return; }
    failed++;
    console.error('FAIL: ' + label + '\n   got:  ' + g + '\n   want: ' + w);
}

// ── a small fake DOM ─────────────────────────────────────────────────────────

function escapeText(s) {
    return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}
function unescapeAttr(s) {
    return s.replace(/&quot;/g, '"').replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&');
}

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
        this.disabled = false;
        this._text = '';
        this._html = null;
        this._style = new FakeStyle();
        const self = this;
        this.classList = {
            add(c) { const l = self.className.split(/\s+/).filter(Boolean); if (l.indexOf(c) < 0) l.push(c); self.className = l.join(' '); },
            remove(c) { self.className = self.className.split(/\s+/).filter(x => x && x !== c).join(' '); },
            contains(c) { return self.className.split(/\s+/).indexOf(c) >= 0; },
        };
    }
    get style() { return this._style; }
    set style(v) { this._style.cssText = String(v); }
    get textContent() {
        if (this.children.length === 0) return this._text;
        return this._text + this.children.map(c => c.textContent).join('');
    }
    set textContent(v) { this._text = v == null ? '' : String(v); this._html = null; this.children = []; }
    get innerHTML() { return this._html != null ? this._html : escapeText(this._text); }
    set innerHTML(v) { this._html = String(v); this._text = ''; this.children = []; this._wired = null; }
    get firstChild() { return this.children[0] || null; }
    appendChild(c) {
        if (c.parentNode) c.remove();
        c.parentNode = this; this.children.push(c); return c;
    }
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
        this.removed = true;
    }
    setAttribute(k, v) { this.attrs[k] = v; }
    addEventListener(ev, fn) { (this.listeners[ev] = this.listeners[ev] || []).push(fn); }
    click(target) {
        const evt = { target: target || this, currentTarget: this, stopPropagation() {}, preventDefault() {} };
        return Promise.all((this.listeners.click || []).map(fn => fn.call(this, evt)));
    }
    descendants() {
        const out = [];
        (function walk(n) { n.children.forEach(c => { out.push(c); walk(c); }); })(this);
        return out;
    }
    querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
    querySelectorAll(sel) {
        // A modal writes an HTML string and wires every [data-action] in it;
        // the fake reads them back out of the string, once per write, so the
        // listeners it wires are the ones a tap reaches.
        if (sel === '[data-action]' && this._html != null) {
            if (!this._wired) this._wired = actionsInHTML(this._html);
            return this._wired;
        }
        if (sel[0] === '.') return this.descendants().filter(n => n.classList.contains(sel.slice(1)));
        return [];
    }
}

// actionsInHTML returns one fake element per tag carrying data-action, with
// the action (as the browser decodes it), whether it is disabled, and (for a
// button) its label.
function actionsInHTML(html) {
    const out = [];
    const re = /<(\w+)\b([^>]*)\bdata-action="([^"]*)"([^>]*)>/g;
    let m;
    while ((m = re.exec(html)) !== null) {
        const n = new FakeEl(m[1]);
        n.dataset.action = unescapeAttr(m[3]);
        n.disabled = /\sdisabled\b/.test(m[2] + m[4]);
        if (m[1] === 'button') {
            const end = html.indexOf('</button>', re.lastIndex);
            n._text = unescapeAttr(html.slice(re.lastIndex, end));
        }
        out.push(n);
    }
    return out;
}

const byID = {};
const body = new FakeEl('body');
body.dataset.stationId = '7';
const document = {
    body: body,
    getElementById(id) { if (!byID[id]) { byID[id] = new FakeEl('div'); byID[id].id = id; } return byID[id]; },
    createElement(tag) { return new FakeEl(tag); },
    createTextNode(t) { const n = new FakeEl('#text'); n._text = String(t); return n; },
    querySelector(sel) { return body.querySelector(sel); },
    querySelectorAll(sel) {
        const m = /^#([\w-]+) (\w+)$/.exec(sel);
        if (m) return document.getElementById(m[1]).descendants().filter(n => n.tagName === m[2].toUpperCase());
        return body.querySelectorAll(sel);
    },
};

// ── the network: every request captured; answered at once, or held ─────────

let requests = [];
// held: while true, a POST waits until release() answers it. fail: the next
// answers are a refusal (409 with an error).
let held = false;
let failNext = 0;
let waiting = [];
function answer(ok) {
    const res = {
        ok: ok, status: ok ? 200 : 409,
        json: async () => (ok ? {} : { error: 'refused' }),
        text: async () => (ok ? '{}' : '{"error":"refused"}'),
        clone() { return res; },
    };
    return res;
}
function fetchStub(url, opts) {
    opts = opts || {};
    const method = opts.method || 'GET';
    requests.push({ method: method, url: url, body: opts.body === undefined ? undefined : JSON.parse(opts.body) });
    if (method === 'GET') return Promise.resolve(answer(true));
    const ok = failNext > 0 ? (failNext--, false) : true;
    if (!held) return Promise.resolve(answer(ok));
    return new Promise(function (resolve, reject) {
        waiting.push(function () { resolve(answer(ok)); });
        if (opts.signal) {
            opts.signal.addEventListener('abort', function () {
                const e = new Error('aborted'); e.name = 'AbortError'; reject(e);
            });
        }
    });
}
// release answers every held request.
async function release() {
    held = false;
    const w = waiting; waiting = [];
    w.forEach(fn => fn());
    await flush();
}
function posts() {
    return requests.filter(r => r.method !== 'GET').map(r => ({ method: r.method, url: r.url, body: r.body }));
}
function flush() {
    return new Promise(r => setImmediate(r)).then(() => new Promise(r => setImmediate(r)))
        .then(() => new Promise(r => setImmediate(r)));
}

// ── load the shipping modules ────────────────────────────────────────────────

let composerOpens = 0;
// fastTimers: a timer of 10 s or more fires at once, so a request timeout is
// reached without the wait.
let fastTimers = false;
const ctx = vm.createContext({
    console, JSON, Math, Date, Number, Array, Object, String, Promise, Error, RegExp, Map, Set,
    parseInt, isNaN, encodeURIComponent, decodeURIComponent, AbortController,
    setTimeout: (fn, ms) => {
        const t = setTimeout(fn, fastTimers && ms >= 10000 ? 0 : ms);
        if (t.unref) t.unref();
        return t;
    },
    clearTimeout,
    document: document,
    window: { innerWidth: 1280, innerHeight: 800, addEventListener() {} },
    fetch: fetchStub,
    serverNow: () => Date.now(),
    formatClock: (iso) => String(iso),
    mountFlowPanel() {}, openFlowPanel() {}, syncFlowPanel() {},
    // The composer's picker, as the header's CHANGEOVER opens it.
    ensureComposer: async () => ({ openPicker() { composerOpens++; } }),
    openKeypad() {},
});
ctx.globalThis = ctx;

const mods = {};
function loadModule(name) {
    let src = fs.readFileSync(path.join(__dirname, name), 'utf8');
    src = src.replace(/^import\s[\s\S]*?from\s+['"][^'"]+['"];?[ \t]*$/gm, '');
    const exported = [];
    src = src.replace(/^export\s+(async\s+function|function|const|let)\s+(\w+)/gm, function (_, kw, n) {
        exported.push(n);
        return kw + ' ' + n;
    });
    src = src.replace(/^export\s*\{[^}]*\};?/gm, '');
    const fns = [];
    src.replace(/^(?:async\s+)?function\s+(\w+)/gm, function (_, n) { fns.push(n); return _; });
    const wrapped = '(function () {\n' + src + '\nreturn { exported: {' +
        exported.map(n => n + ': ' + n).join(', ') + '}, fns: {' +
        fns.map(n => n + ': ' + n).join(', ') + '} };\n})()';
    const out = vm.runInContext(wrapped, ctx, { filename: name });
    Object.assign(ctx, out.exported);
    mods[name] = out.fns;
}
['../../../../shared/esc.js', 'order-status.js', 'operator-util.js', 'operator-state.js', 'operator-window-state.js',
    'operator-board-model.js', 'operator-board.js', 'operator-supply-refusal.js', 'operator-load-bin.js',
    'operator-release.js', 'operator-modal.js', 'operator-render.js',
].forEach(loadModule);

// Wired as operator.js wires them.
let viewLoads = 0;
const loadView = async () => { viewLoads++; };
ctx.setRenderRefs({ openModal: ctx.openModal, openLoadBin: ctx.openLoadBin, loadView: loadView });
ctx.setModalLoadView(loadView);
ctx.setLoadView(loadView);
ctx.setReleaseRefs({ renderModal: ctx.renderModal, modalOpening: ctx.modalOpening, closeModalFrom: ctx.closeModalFrom,
    loadView: loadView });

const REL = mods['operator-release.js'];

// ── views ────────────────────────────────────────────────────────────────────

// pressTile is a press node with its claim: consume (a prompt with a part to
// pick) or produce (RELEASE posts at once).
function pressTile(id, role, o) {
    o = o || {};
    return {
        node: { id: id, process_id: 1, core_node_name: 'PLN_' + id, name: 'PLN_' + id },
        runtime: { remaining_uop_cached: o.uop == null ? 5 : o.uop, active_bin_id: 1 },
        active_claim: { id: 40 + id, core_node_name: 'PLN_' + id, role: role, swap_mode: 'single_robot',
            payload_code: 'PART-A', allowed_payload_codes: ['PART-A'], uop_capacity: 20 },
        orders: o.orders || [], swap_ready: false, releases_as_pair: false,
        bin_state: { occupied: true, payload_code: 'PART-A', stocked: true, bin_label: 'BIN-1' },
    };
}
// staged is an order standing at the node's wait, which offers RELEASE.
function staged(id, nodeName) {
    return { id: id, uuid: 'u-' + id, order_type: 'complex', status: 'staged', payload_code: 'PART-A',
        source_node: '', delivery_node: nodeName, created_at: new Date().toISOString() };
}
// loaderTile is a loader tile as BuildView ships it (tile.loader, no claim).
function loaderTile(id, core, o) {
    o = o || {};
    const role = o.role || 'produce';
    const t = {
        node: { id: id, process_id: 1, core_node_name: core, name: core },
        runtime: { remaining_uop_cached: o.uop || 0 },
        loader: { role: role, layout: 'shared_window', replenishment: 'operator', outbound_dest: '',
            parts: (o.parts || ['PART-A']).map(c => ({ code: c })), directive: false, leaves_bare: false, pulled_directly: false },
        orders: o.orders || [], swap_ready: false, releases_as_pair: false,
    };
    if (role === 'consume') t.loader.carriers = ['TOTE-1', 'TOTE-2'];
    if (o.bin !== undefined) t.bin_state = o.bin;
    return t;
}
function setView(nodes, extra) {
    const view = Object.assign({
        station: { id: 7, name: 'Press station', health_status: 'online' },
        process: { id: 1, name: 'PRESS', active_style_id: 11, target_style_id: 11 },
        current_style: { id: 11, name: 'STYLE-1' },
        available_styles: [{ id: 11, name: 'STYLE-1' }, { id: 12, name: 'STYLE-2' }],
        nodes: nodes,
    }, extra || {});
    ctx.setView(view);
    return view;
}

const modal = document.getElementById('node-modal');
const content = document.getElementById('node-modal-content');

function resetDOM() {
    body.children = [];
    requests = [];
    viewLoads = 0;
    held = false;
    failNext = 0;
    waiting = [];
    ctx.closeModal();
    content.innerHTML = '';
}
function actions() { return content.querySelectorAll('[data-action]'); }
function action(match) {
    const a = actions().find(n => n.dataset.action === match || n.dataset.action.indexOf(match) === 0 || n.textContent === match);
    if (!a) throw new Error('no action ' + match + ' in ' + JSON.stringify(actions().map(n => n.dataset.action)));
    return a;
}
// tapModal taps a node modal control the way its listener sees it.
async function tapModal(match) {
    await ctx.handleModalAction({ currentTarget: action(match) });
    await flush();
}
// tapPrompt taps a release prompt control.
async function tapPrompt(match) {
    await REL.handleReleasePromptAction({ currentTarget: action(match) });
    await flush();
}
function overlays() { return body.children.filter(n => !n.removed && n.classList.contains('os-co-picker-overlay')); }
function overlayButton(label) {
    const o = overlays()[overlays().length - 1];
    const b = o && o.descendants().find(n => n.tagName === 'BUTTON' && n.textContent === label);
    if (!b) throw new Error('no overlay button ' + label);
    return b;
}
async function tap(node) { await node.click(); await flush(); }

// ═════════════════════════════════════════════════════════════════════════════
(async function main() {

// ── A release prompt closed or refused leaves nothing dead or frozen ─────────
{
    // The prompt open on a consume node, then the backdrop tapped: the prompt
    // is closed with the modal (a bug fixed: closeModal left the prompt's
    // state set, and the board skipped every later repaint of the modal).
    resetDOM();
    setView([pressTile(1, 'consume', { orders: [staged(501, 'PLN_1')] })]);
    ctx.openModal(1);
    await tapModal('release-prompt:');
    eq(ctx.isReleasePromptOpen(), true, 'RELEASE on a consume node opens the prompt');
    await modal.click(modal);
    eq([modal.classList.contains('active'), ctx.isReleasePromptOpen()], [false, false],
        'the backdrop closes the modal and the prompt with it');

    // A refused release: the prompt stays, live (a bug fixed: its state was
    // dropped before the post, so every button, CANCEL too, did nothing).
    resetDOM();
    ctx.openModal(1);
    await tapModal('release-prompt:');
    failNext = 1;
    await tapPrompt('release-submit-parts');
    eq(posts().length, 1, 'the submit posts once');
    eq([ctx.isReleasePromptOpen(), action('release-submit-parts').disabled], [true, false],
        'refused: the prompt stays open and its buttons are live');
    await tapPrompt('release-cancel');
    eq([ctx.isReleasePromptOpen(), actions().some(a => a.dataset.action === 'close')], [false, true],
        'refused: CANCEL closes the prompt and shows the node modal');
}
{
    // A finished request closes only the modal it came from (a bug fixed: it
    // closed whatever modal was open when the answer came).
    resetDOM();
    setView([pressTile(1, 'produce', { orders: [staged(601, 'PLN_1')] }),
        pressTile(2, 'produce', { orders: [staged(602, 'PLN_2')] })]);
    ctx.openModal(1);
    held = true;
    const pending = ctx.handleModalAction({ currentTarget: action('release-prompt:') });
    await flush();
    eq(posts().length, 1, 'produce RELEASE posts at once');
    ctx.closeModal();
    ctx.openModal(2);
    await release();
    await pending;
    eq([modal.classList.contains('active'), ctx.getSelectedNodeID()], [true, 2],
        'the release answered after another node\'s modal opened: that modal stays');

    // The same through the prompt's submit.
    resetDOM();
    setView([pressTile(1, 'consume', { orders: [staged(603, 'PLN_1')] }),
        pressTile(2, 'produce', { orders: [staged(604, 'PLN_2')] })]);
    ctx.openModal(1);
    await tapModal('release-prompt:');
    held = true;
    const submit = REL.handleReleasePromptAction({ currentTarget: action('release-submit-parts') });
    await flush();
    await modal.click(modal);
    ctx.openModal(2);
    await release();
    await submit;
    eq([modal.classList.contains('active'), ctx.getSelectedNodeID()], [true, 2],
        'the prompt\'s release answered after another modal opened: that modal stays');
}

// ── One request per tap ────────────────────────────────────────────────────
{
    // A node modal button stays disabled across a repaint while its POST is in
    // flight, and a second tap sends nothing (a bug fixed: a changed view
    // rebuilt the modal with the button live, and a second tap posted again).
    resetDOM();
    const delivered = { id: 701, uuid: 'u-701', order_type: 'retrieve', status: 'delivered', payload_code: 'PART-A',
        source_node: '', delivery_node: 'SMN_D1', created_at: new Date().toISOString() };
    setView([loaderTile(1, 'SMN_D1', { bin: { occupied: false }, orders: [delivered] }),
        pressTile(2, 'consume')]);
    ctx.openModal(1);
    held = true;
    const first = ctx.handleModalAction({ currentTarget: action('/api/confirm-delivery/701') });
    await flush();
    ctx.renderModal(ctx.findNodeByID(1));
    eq(action('/api/confirm-delivery/701').disabled, true, 'CONFIRM DELIVERY stays disabled across a repaint while it posts');
    const second = ctx.handleModalAction({ currentTarget: action('/api/confirm-delivery/701') });
    await release();
    await Promise.all([first, second]);
    eq(posts().length, 1, 'CONFIRM DELIVERY: two taps, one request');

    // The modal's request card (a tap target that cannot be disabled).
    resetDOM();
    setView([loaderTile(1, 'SMN_D1', { bin: { occupied: false } })]);
    ctx.openModal(1);
    held = true;
    const card = action('/api/process-nodes/1/request-empty|PART-A');
    const one = ctx.handleModalAction({ currentTarget: card });
    await flush();
    const again = ctx.handleModalAction({ currentTarget: card });
    await release();
    await Promise.all([one, again]);
    eq(posts().length, 1, 'the modal\'s request card: two taps, one request');
}
{
    // RELEASE on a produce node posts at once: two taps, one release.
    resetDOM();
    setView([pressTile(1, 'produce', { orders: [staged(801, 'PLN_1')] })]);
    ctx.openModal(1);
    held = true;
    const rel = action('release-prompt:');
    const one = ctx.handleModalAction({ currentTarget: rel });
    await flush();
    ctx.renderModal(ctx.findNodeByID(1));
    eq(action('release-prompt:').disabled, true, 'RELEASE stays disabled across a repaint while it posts');
    const again = ctx.handleModalAction({ currentTarget: rel });
    await release();
    await Promise.all([one, again]);
    eq(posts().length, 1, 'RELEASE (produce): two taps, one request');

    // Verify Good on a containment position.
    resetDOM();
    setView([{ node: { id: 3, process_id: 1, core_node_name: 'QH_1', name: 'QH_1' }, runtime: {}, orders: [],
        containment_release_target: 'FG-DROP', swap_ready: false, releases_as_pair: false,
        bin_state: { occupied: true, bin_id: 77, payload_code: 'PART-A', stocked: true } }]);
    ctx.openModal(3);
    held = true;
    const vg = action('release-containment:');
    const two = ctx.handleModalAction({ currentTarget: vg });
    await flush();
    const vgAgain = ctx.handleModalAction({ currentTarget: vg });
    await release();
    await Promise.all([two, vgAgain]);
    eq(posts().length, 1, 'Verify Good: two taps, one request');
}
{
    // The board bar's Request empty: disabled while it posts, one request.
    resetDOM();
    setView([loaderTile(1, 'SMN_B1', { bin: { occupied: false } }), loaderTile(2, 'SMN_B2', { bin: { occupied: false } })]);
    ctx.renderHeader();
    ctx.renderGrid();
    const grid = document.getElementById('os-grid');
    const bar = () => grid.descendants().find(n => n.classList.contains('os-pc-bar')).children.filter(n => n.tagName === 'BUTTON');
    held = true;
    const btn = bar()[0];
    eq(btn.textContent, 'Request empty', 'the board bar offers Request empty');
    const one = btn.click();
    await flush();
    eq(bar()[0].disabled, true, 'Request empty is disabled while it posts');
    const again = btn.click();
    await release();
    await Promise.all([one, again]);
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/request-empty', body: {} }], 'Request empty: two taps, one request');
    eq(bar()[0].disabled, false, 'Request empty is live again once it is answered');

    // Request full, on a board whose first window is an unloader's.
    resetDOM();
    setView([loaderTile(1, 'ULN_M1', { role: 'consume', bin: { occupied: false } }), loaderTile(2, 'SMN_M2', { bin: { occupied: false } })]);
    ctx.renderGrid();
    held = true;
    const full = bar()[0];
    eq(full.textContent, 'Request full', 'the board bar offers Request full');
    const two = full.click();
    await flush();
    const twice = full.click();
    await release();
    await Promise.all([two, twice]);
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/request-full', body: { payload_code: 'PART-A' } }],
        'Request full: two taps, one request');
}
{
    // The supply refusal's question: WAIT and CHANGE OVER answer once.
    resetDOM();
    const asked = pressTile(1, 'consume');
    asked.supply_refused_for_me = { loader_node: 'SMN_L1', payload_code: 'PART-A', refused_by: 'Loader', answered: false };
    setView([asked]);
    ctx.renderGrid();
    held = true;
    const wait = overlayButton('WAIT');
    const one = wait.click();
    await flush();
    eq([overlayButton('WAIT').disabled, overlayButton('CHANGE OVER').disabled], [true, true],
        'WAIT tapped: both answers are disabled while it posts');
    const again = [wait.click(), overlayButton('CHANGE OVER').click()];
    await release();
    await Promise.all([one].concat(again));
    eq(posts().length, 1, 'the refusal\'s answer: three taps, one request');
}
{
    // A confirm overlay does not stack: a card tapped twice opens one CLEAR
    // panel (the overlay covers the board, so the second tap is the focused
    // card's key; the test taps it twice).
    resetDOM();
    setView([loaderTile(1, 'ULN_A1', { role: 'consume', bin: { occupied: true, payload_code: 'PART-A', stocked: true, bin_type_code: 'TOTE-1' }, uop: 7 }),
        loaderTile(2, 'ULN_A2', { role: 'consume', bin: { occupied: false } })]);
    ctx.renderGrid();
    const grid = document.getElementById('os-grid');
    const card = grid.descendants().find(n => n.classList.contains('os-pc-card') && !n.classList.contains('os-pc-more'));
    await tap(card);
    await tap(card);
    eq(overlays().length, 1, 'a card tapped twice opens one confirm');
}

// ── The supply refusal's question ──────────────────────────────────────────
{
    // A failed answer leaves the question up, its buttons live (a bug fixed:
    // the overlay was removed whatever the answer, and came back only on the
    // next changed view).
    resetDOM();
    const asked = pressTile(1, 'consume');
    asked.supply_refused_for_me = { loader_node: 'SMN_L1', payload_code: 'PART-A', refused_by: 'Loader', answered: false };
    setView([asked]);
    ctx.renderGrid();
    failNext = 1;
    await tap(overlayButton('WAIT'));
    const q = body.children.filter(n => !n.removed && n.classList.contains('os-refusal-overlay'));
    eq([posts().length, q.length, q.length && overlayButton('WAIT').disabled], [1, 1, false],
        'a refused answer: the question stays, WAIT live again');
    await tap(overlayButton('WAIT'));
    eq([posts().length, body.children.filter(n => !n.removed && n.classList.contains('os-refusal-overlay')).length], [2, 0],
        'answered: the question goes');
}

{
    // CHANGE OVER on the question opens what the header's CHANGEOVER opens,
    // the composer's picker (a bug fixed: it opened the old flat style list,
    // which starts a changeover with no set-up card).
    resetDOM();
    composerOpens = 0;
    const asked = pressTile(1, 'consume');
    asked.supply_refused_for_me = { loader_node: 'SMN_L1', payload_code: 'PART-A', refused_by: 'Loader', answered: false };
    setView([asked]);
    ctx.renderGrid();
    await tap(overlayButton('CHANGE OVER'));
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/supply-refusal/ack',
        body: { loader_node: 'SMN_L1', payload_code: 'PART-A', choice: 'changeover' } }], 'CHANGE OVER posts the ack');
    eq([composerOpens, overlays().length], [1, 0], 'then opens the composer picker, not the flat list');
}

// ── The queue badge ────────────────────────────────────────────────────────
{
    // The node modal's badge on a part is its place in the queue by age: the
    // oldest live order first (a bug fixed: it counted the cards in the order
    // the loader lists its parts).
    resetDOM();
    const at = mins => new Date(Date.now() - mins * 60000).toISOString();
    const queued = (id, part, mins) => ({ id: id, uuid: 'u-' + id, order_type: 'retrieve', status: 'queued',
        payload_code: part, source_node: '', delivery_node: 'SMN_Q1', created_at: at(mins) });
    setView([loaderTile(1, 'SMN_Q1', { parts: ['PART-A', 'PART-B', 'PART-C'], bin: { occupied: false },
        orders: [queued(901, 'PART-A', 2), queued(902, 'PART-C', 20), queued(903, 'PART-B', 9)] })]);
    ctx.openModal(1);
    const badges = [];
    content.innerHTML.replace(/border-radius:50%[^>]*>(\d+)<\/span><span[^>]*>([^<]*)<\/span>/g, function (_, n, code) {
        badges.push(code + ' ' + n);
    });
    eq(badges, ['PART-A 3', 'PART-B 2', 'PART-C 1'], 'the badge is the place by age');
}

// ── Names inside an attribute ──────────────────────────────────────────────
{
    // A name with a quote or an ampersand goes into a data-action attribute
    // whole and comes back out as itself (a bug fixed: the text-only escape
    // left the quote, which closed the attribute, and Verify Good escaped the
    // node's name twice).
    resetDOM();
    setView([{ node: { id: 3, process_id: 1, core_node_name: 'QH"1&2', name: 'QH"1&2' }, runtime: {}, orders: [],
        containment_release_target: 'FG-DROP', swap_ready: false, releases_as_pair: false,
        bin_state: { occupied: true, bin_id: 77, payload_code: 'PART-A', stocked: true } }]);
    ctx.openModal(3);
    await tapModal('VERIFY GOOD — RELEASE TO FG');
    eq(posts().map(p => p.body.node_name), ['QH"1&2'], 'Verify Good posts the node name it holds');

    resetDOM();
    const quoted = pressTile(1, 'consume', { orders: [staged(951, 'PLN_1')] });
    quoted.active_claim.allowed_payload_codes = ['PART"A&B'];
    setView([quoted]);
    ctx.openModal(1);
    await tapModal('release-prompt:');
    await tapPrompt('release-pick:');
    eq(action('release-qty-ok:').dataset.action, 'release-qty-ok:PART"A&B', 'the release prompt carries the part whole');
}

// ── A request with no answer ───────────────────────────────────────────────
{
    // postAction gives up on a request with no answer, as the view fetch does
    // (a bug fixed: a hung POST left the Load Bin modal with its submit and
    // cancel disabled for good).
    resetDOM();
    held = true;
    fastTimers = true;
    const toasts = document.getElementById('os-toast');
    toasts.children = [];
    const outcome = await Promise.race([
        ctx.postAction('/api/process-nodes/1/load-bin', {}, loadView),
        new Promise(r => setTimeout(() => r('still waiting'), 500)),
    ]);
    fastTimers = false;
    eq(outcome, false, 'a POST with no answer gives up: false');
    eq(toasts.children.map(t => t.textContent), ['No answer — check the board before trying again'],
        'and says so');
    waiting = [];
}

if (failed) { console.error(passed + ' passed, ' + failed + ' FAILED'); process.exit(1); }
console.log('operator safe taps: ' + passed + ' passed');
finished = true;
process.exit(0);
})().catch(function (err) { console.error(err && err.stack || err); process.exit(1); });
