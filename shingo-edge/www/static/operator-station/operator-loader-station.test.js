// Characterisation of the loader and unloader station's glass as it is:
// what the board draws for each layout, every act it offers a loader or
// unloader tile, the state that offers it, and exactly what its tap posts.
//
// WHY THE WHOLE PATH AND NOT cardModel. cardModel says load, unload or
// nothing; the acts themselves are chosen in the render layer (renderGrid,
// renderHeader, renderPayloadBoard, renderDedicatedPositionsBoard, buildLoaderCard,
// createNodeButton), in the node modal (renderModal, handleModalAction) and in
// the load modal (openLoadBin). So this loads the SHIPPING modules whole —
// operator-render, operator-modal, operator-load-bin, operator-supply-refusal,
// operator-board-model, operator-window-state, operator-state, operator-util,
// order-status — into
// one vm with a small fake DOM, renders a view, taps what the glass offers and
// captures the request at fetch. A pinned post is the method, the route and
// the JSON body as postAction puts them on the wire.
//
// Stubbed, because they are not the loader's: the flow picture and the
// composer (operator-flow.js, composer-boot.js), the keypad, the release
// prompt, and the shared clock helpers.
//
// The outcomes are pinned as they are. Five were wrong when this was first
// written and are fixed; each is labelled "fixed" below with what it was.
// Others that later work changes are labelled "today". A change to any of them
// is a changed outcome and has to be named.
//
// The tiles are built as BuildView ships them: a loader tile carries
// tile.loader and no claim, its bin carries stocked and no count, and the
// served cells ride view.serves.
//
// Runs under plain Node (no npm). Exit 0 = pass, 1 = any failure. Run via the
// Go wrapper operator_loader_station_test.go.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
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
    set innerHTML(v) { this._html = String(v); this._text = ''; this.children = []; }
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
    click() {
        const evt = { target: this, currentTarget: this, stopPropagation() {}, preventDefault() {} };
        return Promise.all((this.listeners.click || []).map(fn => fn.call(this, evt)));
    }
    descendants() {
        const out = [];
        (function walk(n) { n.children.forEach(c => { out.push(c); walk(c); }); })(this);
        return out;
    }
    querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
    querySelectorAll(sel) {
        // The node modal writes an HTML string and wires every [data-action] in
        // it; the fake reads them back out of the string.
        if (sel === '[data-action]' && this._html != null) return actionsInHTML(this._html);
        if (sel[0] === '.') return this.descendants().filter(n => n.classList.contains(sel.slice(1)));
        return [];
    }
}

// actionsInHTML returns one fake element per tag carrying data-action, with
// the action, whether it is disabled, and (for a button) its label.
function actionsInHTML(html) {
    const out = [];
    const re = /<(\w+)\b([^>]*)\bdata-action="([^"]*)"([^>]*)>/g;
    let m;
    while ((m = re.exec(html)) !== null) {
        const n = new FakeEl(m[1]);
        n.dataset.action = m[3].replace(/&amp;/g, '&');
        n.disabled = /\sdisabled\b/.test(m[2] + m[4]);
        if (m[1] === 'button') {
            const end = html.indexOf('</button>', re.lastIndex);
            n._text = html.slice(re.lastIndex, end).replace(/&amp;/g, '&');
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

// ── the network: every request is captured, and answered ─────────────────────

let requests = [];
const marketBins = [{ node_name: 'MARKET-GROUP.SMN_M1', payload_code: 'PART-A', uop_remaining: 40 }];
const manifests = {
    'PART-A': { uop_capacity: 24, items: [{ part_number: 'PN-A1', parts_per_cycle: 2, description: 'left' }] },
};
function fetchStub(url, opts) {
    opts = opts || {};
    const method = opts.method || 'GET';
    let answer = {};
    if (method === 'GET') {
        if (/\/market-bins$/.test(url)) answer = marketBins;
        const mm = /^\/api\/payload\/([^/]+)\/manifest$/.exec(url);
        if (mm) answer = manifests[decodeURIComponent(mm[1])] || { uop_capacity: 0, items: [] };
    }
    requests.push({ method: method, url: url, body: opts.body === undefined ? undefined : JSON.parse(opts.body) });
    const res = {
        ok: true, status: 200,
        json: async () => answer,
        text: async () => JSON.stringify(answer),
        clone() { return res; },
    };
    return Promise.resolve(res);
}
// Posts only: the GETs a picker or the load modal makes to fill itself are
// not what a tap posts.
function posts() {
    return requests.filter(r => r.method !== 'GET').map(r => ({ method: r.method, url: r.url, body: r.body }));
}
function flush() { return new Promise(r => setImmediate(r)).then(() => new Promise(r => setImmediate(r))); }

// ── load the shipping modules ────────────────────────────────────────────────

const ctx = vm.createContext({
    console, JSON, Math, Date, Number, Array, Object, String, Promise, Error, RegExp, Map, Set,
    parseInt, isNaN, encodeURIComponent, decodeURIComponent, AbortController,
    setTimeout: (fn, ms) => { const t = setTimeout(fn, ms); if (t.unref) t.unref(); return t; },
    clearTimeout,
    document: document,
    window: { innerWidth: 1280, addEventListener() {} },
    fetch: fetchStub,
    // The shared clock helpers (/static/shared/utils.js).
    serverNow: () => Date.now(),
    formatClock: (iso) => String(iso),
    // Not the loader's: the flow picture, the composer, the keypad, the
    // release prompt.
    mountFlowPanel() {}, openFlowPanel() {}, syncFlowPanel() {},
    ensureComposer: async () => null,
    openKeypad() {},
    openReleasePrompt() {},
});
ctx.globalThis = ctx;

// loadModule runs one ES module as a script: imports dropped (the names they
// bind are this vm's globals), exports published as globals, and every
// top-level function kept on mods[name] for the internal ones a test drives.
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
['order-status.js', 'operator-util.js', 'operator-state.js', 'operator-window-state.js',
    'operator-board-model.js', 'operator-supply-refusal.js', 'operator-load-bin.js', 'operator-modal.js', 'operator-render.js',
].forEach(loadModule);

const R = mods['operator-render.js'];
let viewLoads = 0;
ctx.setRenderRefs({ openModal: ctx.openModal, openLoadBin: ctx.openLoadBin, loadView: async () => { viewLoads++; } });
ctx.setModalLoadView(async () => { viewLoads++; });

// ── view builders ────────────────────────────────────────────────────────────

const catalog = [
    { payload_code: 'PART-A', bin_type_code: 'TOTE-1' },
    { payload_code: 'PART-B', bin_type_code: 'TOTE-2' },
];

// carriersFor is the server's carrier rule for an unloader tile (service
// carriersFor): the catalog's types for the parts, each once, in catalog order;
// every type when the parts match none.
function carriersFor(parts) {
    const pick = match => catalog.filter(match).map(e => e.bin_type_code)
        .filter((c, i, all) => all.indexOf(c) === i);
    const out = pick(e => parts.indexOf(e.payload_code) >= 0);
    return out.length ? out : pick(() => true);
}

// loaderTile builds one loader tile the way BuildView ships it: the loader as
// the board reads it (tile.loader) and no claim. Options: role, parts, operator
// (Core's replenishment `operator`), hides_idle and all_parts (the board
// settings, defaulted from operator as Core's backfill set them), dedicated
// (dedicated_positions), outbound, directive, leaves_bare, pulled_directly, bin,
// uop, orders, buffer_partial, supply_refusals.
function loaderTile(id, core, o) {
    o = o || {};
    const role = o.role || 'produce';
    const parts = o.parts || ['PART-A'];
    const t = {
        node: { id: id, process_id: 1, core_node_name: core, name: core },
        runtime: { remaining_uop_cached: o.uop || 0 },
        loader: {
            role: role, layout: o.dedicated ? 'dedicated_positions' : 'shared_window',
            replenishment: o.operator ? 'operator' : 'threshold',
            outbound_dest: o.outbound === undefined ? 'FG-MARKET' : o.outbound,
            parts: parts.map(c => ({ code: c })),
            directive: !!o.directive, leaves_bare: !!o.leaves_bare, pulled_directly: !!o.pulled_directly,
            // The board settings as Core's v157 backfilled them: a threshold
            // station hides idle cards, an operator station lists every part.
            board_hides_idle: o.hides_idle === undefined ? !o.operator : !!o.hides_idle,
            board_shows_all_parts: o.all_parts === undefined ? !!o.operator : !!o.all_parts,
        },
        orders: o.orders || [],
        swap_ready: false,
        releases_as_pair: false,
    };
    if (role === 'consume') t.loader.carriers = carriersFor(parts);
    if (o.bin !== undefined) t.bin_state = o.bin;
    ['buffer_partial', 'supply_refusals'].forEach(k => { if (o[k] !== undefined) t[k] = o[k]; });
    return t;
}

// row builds one served row (view.serves) of a running consume claim, as the
// served pass ships it.
function row(cell, part, have, o) {
    return Object.assign({ cell: cell, part: part, role: 'consume', carrier: '', have_uop: have,
        capacity_uop: 100, incoming: false, node: 'WLN_' + cell, process_id: 2, process_node_id: 2,
        reorder_uop: 0, waiting: false }, o || {});
}

function order(id, status, o) {
    o = o || {};
    return Object.assign({ id: id, uuid: 'u-' + id, order_type: 'retrieve', status: status, payload_code: '',
        source_node: '', delivery_node: '', created_at: new Date(Date.now() - 5 * 60000).toISOString() }, o);
}

const FULL = (part, uop) => ({ occupied: true, payload_code: part, stocked: (uop == null ? 12 : uop) > 0, bin_label: 'BIN-' + part });
const EMPTY = { occupied: true, payload_code: '', stocked: false, bin_label: 'BIN-E' };
const NONE = { occupied: false };

function setView(nodes, extra) {
    const view = Object.assign({
        station: { id: 7, name: 'Loader station', health_status: 'online' },
        process: { id: 1, name: 'LDR', active_style_id: null, target_style_id: null },
        current_style: null,
        available_styles: [{ id: 11, name: 'STYLE-1' }, { id: 12, name: 'STYLE-2' }],
        nodes: nodes,
        payload_bin_types: catalog,
    }, extra || {});
    ctx.setView(view);
    return view;
}

// ── reading the board ────────────────────────────────────────────────────────

const grid = document.getElementById('os-grid');
const header = document.getElementById('os-header-actions');

function resetDOM() {
    body.children = [];
    requests = [];
    loadOpens = [];
    viewLoads = 0;
    ctx.closeModal();
    const modal = document.getElementById('node-modal-content');
    modal.innerHTML = '';
}
function render() { resetDOM(); ctx.renderHeader(); ctx.renderGrid(); }
function headerButtons() { return header.children.filter(n => n.tagName === 'BUTTON').map(b => b.textContent); }
function headerButton(label) {
    const b = header.children.find(n => n.tagName === 'BUTTON' && n.textContent === label);
    if (!b) throw new Error('no header button ' + label);
    return b;
}
function cards() { return grid.descendants().filter(n => n.classList.contains('os-board-card')); }
function cardFor(code, home) {
    return cards().find(c => {
        const codeEl = c.children.find(k => k.classList.contains('os-board-code'));
        const homeEl = c.children.find(k => k.classList.contains('os-board-home'));
        return codeEl && codeEl.textContent === code && (!home || (homeEl && homeEl.textContent.indexOf(home) === 0));
    }) || null;
}
function part(card, cls) {
    const n = card.descendants().find(k => k.classList.contains(cls));
    return n ? n.textContent : null;
}
function slotTiles() { return grid.children.filter(n => n.classList.contains('os-node-btn')); }
function slotTile(name) {
    return slotTiles().find(t => t.children.some(k => k.classList.contains('os-node-name') && k.textContent === name)) || null;
}
function reqBarButtons() {
    const bar = grid.descendants().find(n => n.classList.contains('os-board-reqbar'));
    return bar ? bar.children.filter(n => n.tagName === 'BUTTON') : [];
}
function overlay() {
    const live = body.children.filter(n => !n.removed);
    return live[live.length - 1] || null;
}
function overlayButtons() {
    const o = overlay();
    return o ? o.descendants().filter(n => n.tagName === 'BUTTON').map(b => b.textContent) : [];
}
function overlayTexts() {
    const o = overlay();
    return o ? o.descendants().filter(n => n.tagName === 'DIV' && n._text).map(n => n._text) : [];
}
async function tapOverlay(label) {
    const o = overlay();
    const b = o && o.descendants().find(n => n.tagName === 'BUTTON' && n.textContent === label);
    if (!b) throw new Error('no overlay button ' + label + ' in ' + JSON.stringify(overlayButtons()));
    await b.click();
    await flush();
}
async function tap(node) { await node.click(); await flush(); }

// The node modal: its [data-action] controls, read back from its HTML.
function modalActions() {
    return document.getElementById('node-modal-content').querySelectorAll('[data-action]');
}
async function tapModal(match) {
    const a = modalActions().find(n => n.dataset.action === match || n.textContent === match);
    if (!a) throw new Error('no modal action ' + match + ' in ' + JSON.stringify(modalActions().map(n => n.dataset.action)));
    await ctx.handleModalAction({ currentTarget: a });
    await flush();
}
function modalButtons() { return modalActions().filter(n => n.tagName === 'BUTTON').map(n => n.textContent); }

// The load modal: CONFIRM LOAD is its submit button.
async function confirmLoad() {
    await flush();
    await document.getElementById('load-bin-submit').click();
    await flush();
}
let loadOpens = [];
const realOpenLoadBin = ctx.openLoadBin;
function spyLoadBin() {
    loadOpens = [];
    const spy = function (nodeID, codes, cap) { loadOpens.push({ node: nodeID, codes: codes, cap: cap }); return realOpenLoadBin(nodeID, codes, cap); };
    // The board reaches it through setRenderRefs, the node modal through its
    // import (a global here).
    ctx.openLoadBin = spy;
    ctx.setRenderRefs({ openModal: ctx.openModal, openLoadBin: spy, loadView: async () => { viewLoads++; } });
}
spyLoadBin();

const loadBody = { payload_code: 'PART-A', uop_count: 24,
    manifest: [{ part_number: 'PN-A1', quantity: 48, description: 'left' }] };

// ═════════════════════════════════════════════════════════════════════════════
(async function main() {

// ── 1. The payload board, one window (produce, shared window) ──────────────
{
    // An empty delivered and nothing parked: LOAD on the card, REQUEST EMPTY
    // greyed because an order is live.
    setView([loaderTile(1, 'SMN_A1', {
        parts: ['PART-A', 'PART-B'], bin: NONE,
        orders: [order(101, 'delivered', { payload_code: 'PART-A', delivery_node: 'SMN_A1' })],
    })], { serves: [row('CELL-1', 'PART-A', 30)] });
    render();
    eq(headerButtons(), ['REFRESH'], 'payload board, one window: the header carries REFRESH only (board mode)');
    eq(reqBarButtons().map(b => b.textContent), ['REQUEST EMPTY — bin already inbound', 'PULL FROM MARKET'],
        'payload board: REQUEST EMPTY greyed while an order is live; PULL FROM MARKET beside it');
    eq(reqBarButtons()[0].disabled, true, 'payload board: the greyed REQUEST EMPTY is disabled');
    const c = cardFor('PART-A');
    eq(part(c, 'os-board-tag'), 'DELIVERED', 'payload board: a delivered empty for PART-A reads DELIVERED');
    eq(cardFor('PART-B'), null, 'payload board: an idle card on a normal (threshold) loader is hidden');
    await tap(c);
    eq(loadOpens, [{ node: 1, codes: ['PART-A'], cap: 0 }], 'LOAD: the card opens the load modal for that part, capacity 0 (the synthesised claim has none)');
    await confirmLoad();
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/load-bin', body: loadBody }],
        'LOAD: CONFIRM LOAD posts load-bin with the part, the UoP count and the manifest scaled to it');
}
{
    // Nothing parked, nothing live: REQUEST EMPTY is offered and posts an empty body.
    setView([loaderTile(1, 'SMN_A1', { parts: ['PART-A'], bin: NONE })], { serves: [row('CELL-1', 'PART-A', 30)] });
    render();
    const req = reqBarButtons()[0];
    eq(req.textContent, 'REQUEST EMPTY', 'REQUEST EMPTY: offered with no bin and no live order');
    await tap(req);
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/request-empty', body: {} }],
        'REQUEST EMPTY: posts request-empty with no payload code');

    // PULL FROM MARKET: a picker filled from market-bins; a bin's tap posts its node.
    render();
    await tap(reqBarButtons()[1]);
    eq(requests.map(r => r.method + ' ' + r.url), ['GET /api/process-nodes/1/market-bins'],
        'PULL FROM MARKET: the picker reads the market');
    await tapOverlay('SMN_M1PART-AUoP: 40');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/pull-from-market',
        body: { source_core_node: 'MARKET-GROUP.SMN_M1' } }],
    'PULL FROM MARKET: a bin\'s tap posts pull-from-market with its full node name');

    // No outbound destination on the claim: no PULL FROM MARKET.
    setView([loaderTile(1, 'SMN_A1', { parts: ['PART-A'], bin: NONE, outbound: '' })]);
    render();
    eq(reqBarButtons().map(b => b.textContent), ['REQUEST EMPTY'], 'PULL FROM MARKET: absent without an outbound destination');

    // A bin parked: REQUEST EMPTY greyed for that reason.
    setView([loaderTile(1, 'SMN_A1', { parts: ['PART-A'], bin: EMPTY })]);
    render();
    eq(reqBarButtons()[0].textContent, 'REQUEST EMPTY — bin at node', 'REQUEST EMPTY: greyed with a bin parked');
}
{
    // An empty parked with no order: LOAD now, on every allowed part.
    setView([loaderTile(1, 'SMN_A1', { parts: ['PART-A', 'PART-B'], bin: EMPTY })]);
    render();
    eq([part(cardFor('PART-A'), 'os-board-tag'), part(cardFor('PART-B'), 'os-board-tag')], ['LOAD', 'LOAD'],
        'payload board: an empty parked with no order reads LOAD on every part');
    // A card with nothing to do opens the node modal.
    setView([loaderTile(1, 'SMN_A1', { parts: ['PART-A'], bin: NONE, operator: true,
        orders: [order(102, 'in_transit', { payload_code: 'PART-A' })] })]);
    render();
    const c = cardFor('PART-A');
    eq(part(c, 'os-board-tag'), 'IN TRANSIT', 'payload board: an empty on its way reads IN TRANSIT');
    await tap(c);
    eq(document.getElementById('node-modal').classList.contains('active'), true,
        'a card with no load or unload act opens the node modal');
    eq(posts(), [], 'opening the node modal posts nothing');
}

// ── 2. The payload board, two windows ──────────────────────────────────────
{
    const two = [
        loaderTile(1, 'SMN_B1', { parts: ['PART-A'], bin: NONE, operator: true }),
        loaderTile(2, 'SMN_B2', { parts: ['PART-A'], bin: EMPTY, operator: true,
            orders: [order(201, 'delivered', { delivery_node: 'SMN_B2' })] }),
    ];
    const serves = { serves: [row('CELL-2', 'PART-A', 10)] };
    setView(two, serves);
    render();
    // Fixed. It was: the header kept the cell's CHANGEOVER (isBoardMode asked
    // for exactly one node), only the first window's cards were drawn, the
    // delivered empty at the second never showed LOAD, and REQUEST EMPTY read
    // the first window alone.
    eq(headerButtons(), ['REFRESH'], 'two windows, fixed: the header carries the loader\'s acts, not the cell\'s CHANGEOVER');
    eq(cards().length, 2, 'two windows, fixed: every window\'s cards are drawn');
    eq(cards().map(c => part(c, 'os-board-home')), ['SMN_B1 · AWAITING', 'SMN_B2 · EMPTY'],
        'two windows: each card names its window and what stands there');
    eq(part(cardFor('PART-A', 'SMN_B1'), 'os-board-tag'), 'ACTIVE', 'two windows: the first window idle reads ACTIVE');
    eq(part(cardFor('PART-A', 'SMN_B2'), 'os-board-tag'), 'LOAD', 'two windows, fixed: the delivered empty at the second reads LOAD');
    await tap(cardFor('PART-A', 'SMN_B2'));
    eq(loadOpens, [{ node: 2, codes: ['PART-A'], cap: 0 }], 'two windows, fixed: the second window\'s LOAD opens its load modal');
    render();
    eq(reqBarButtons()[0].textContent, 'REQUEST EMPTY',
        'two windows: REQUEST EMPTY offered while a window of the loader is free');
    await tap(reqBarButtons()[0]);
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/request-empty', body: {} }],
        'two windows: REQUEST EMPTY posts for the first window (the server stages at a free one)');
    // Both windows taken: greyed, and for the reason that holds for both.
    setView([loaderTile(1, 'SMN_B1', { parts: ['PART-A'], bin: EMPTY, operator: true }), two[1]], serves);
    render();
    eq(reqBarButtons()[0].textContent, 'REQUEST EMPTY — bin at node', 'two windows, fixed: greyed only when no window is free');
    setView([loaderTile(1, 'SMN_B1', { parts: ['PART-A'], bin: NONE, operator: true,
        orders: [order(202, 'in_transit', { delivery_node: 'SMN_B1' })] }), two[1]], serves);
    render();
    eq(reqBarButtons()[0].textContent, 'REQUEST EMPTY — bin already inbound', 'two windows: greyed with an empty on its way to the free one');
}

// ── 3. Dedicated homes (produce) ───────────────────────────────────────────
{
    const homes = [
        loaderTile(1, 'SMN_H1', { parts: ['PART-A'], bin: FULL('PART-A', 12), dedicated: true,
            operator: true, buffer_partial: { node: 'SMN_F1' } }),
        loaderTile(2, 'SMN_H2', { parts: ['PART-B'], bin: EMPTY, dedicated: true, operator: true }),
        loaderTile(3, 'SMN_H3', { parts: ['PART-A'], bin: FULL('PART-A', 5), dedicated: true, operator: true }),
    ];
    const homeServes = { serves: [row('CELL-H', 'PART-A', 12)] };
    setView(homes, homeServes);
    render();
    eq(headerButtons(), ['PULL FULL FROM HOME', 'REFRESH'], 'homes: the header offers PULL FULL FROM HOME in place of CHANGEOVER');
    eq(cards().length, 3, 'homes, an operator loader: every home is drawn');
    const h1 = cardFor('PART-A', 'SMN_H1');
    eq(part(h1, 'os-board-home'), 'SMN_H1 · LOADED', 'homes: the home line');
    // Changed: the mark was Core's reorder threshold (12 below it read
    // "— PRELOAD" and red), which sits at or above a bin and so reads every
    // running part as low; the board now grades a part by its coverage, and a
    // home holding a stocked bin of it covers it.
    eq(part(h1, 'os-board-lineside'), 'Lineside 12 UoP', 'homes: an active part says its lineside; covered, it carries no mark');
    eq(h1.classList.contains('os-board-card--starved'), false, 'homes: a covered part is not marked short');
    // Exposed: no stocked bin of the part on the board, the cell still has some.
    setView([homes[1]].concat([loaderTile(4, 'SMN_H4', { parts: ['PART-A'], bin: NONE, dedicated: true, operator: true })]),
        homeServes);
    render();
    const h4 = cardFor('PART-A', 'SMN_H4');
    eq([part(h4, 'os-board-lineside'), h4.classList.contains('os-board-card--starved')], ['Lineside 12 UoP — PRELOAD', true],
        'homes: a part exposed at a served cell is marked short');
    setView(homes, homeServes);
    render();
    eq(h1.descendants().filter(n => n.tagName === 'BUTTON').map(b => b.textContent), ['CLEAR BIN'],
        'homes: CLEAR BIN on a loaded home whose buffer holds a partial of its part');
    eq(cardFor('PART-A', 'SMN_H3').descendants().filter(n => n.tagName === 'BUTTON').length, 0,
        'homes: no CLEAR BIN without a buffer partial');
    eq(part(cardFor('PART-B', 'SMN_H2'), 'os-board-tag'), 'LOAD', 'homes: an empty home reads LOAD');

    // CLEAR BIN on the card: the confirm, then clear-loader-home with an empty body.
    await tap(h1.descendants().find(n => n.tagName === 'BUTTON'));
    eq(overlayTexts()[0], 'Clear this home bin?', 'CLEAR BIN: the confirm names no node (today)');
    eq(overlayButtons(), ['CONFIRM CLEAR', 'CANCEL'], 'CLEAR BIN: the confirm offers CONFIRM CLEAR');
    await tapOverlay('CONFIRM CLEAR');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/clear-loader-home', body: {} }],
        'CLEAR BIN: posts clear-loader-home with no body fields (the engine picks the first matching partial)');

    // PULL FULL FROM HOME lists the eligible homes only, and ends in the same post.
    render();
    await tap(headerButton('PULL FULL FROM HOME'));
    eq(overlayButtons(), ['SMN_H1PART-A', 'CANCEL'], 'PULL FULL FROM HOME: only the home with a buffer partial is listed');
    await tapOverlay('SMN_H1PART-A');
    await tapOverlay('CONFIRM CLEAR');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/clear-loader-home', body: {} }],
        'PULL FULL FROM HOME: posts clear-loader-home for the picked home');

    // LOAD from an empty home.
    render();
    await tap(cardFor('PART-B', 'SMN_H2'));
    eq(loadOpens, [{ node: 2, codes: ['PART-B'], cap: 0 }], 'homes: LOAD on an empty home opens the load modal for its part');

    // A home board set to hide idle cards (a threshold station's backfill) hides its idle homes.
    setView(homes.map(h => Object.assign({}, h, { loader: Object.assign({}, h.loader,
        { replenishment: 'threshold', board_hides_idle: true, board_shows_all_parts: false }) })), homeServes);
    render();
    eq(cards().length, 1, 'homes, on a threshold: idle homes are hidden (only the empty home\'s LOAD is drawn)');
}

// ── 4. A home while its clear is in flight (Order A on the home's orders) ──
{
    // ClearLoaderHome zeroes the home's bin on Core (its payload goes blank)
    // and files Order A: a blank-payload move of the carrier, from the home to
    // the buffer spot. isOwnEmptyOut counts only a CONSUME node's own move, so
    // on a produce home Order A is general (untagged) demand over an empty bin.
    const orderA = order(301, 'queued', { order_type: 'move', source_node: 'SMN_H1', delivery_node: 'SMN_F1' });
    const cleared = loaderTile(1, 'SMN_H1', { parts: ['PART-A'], bin: EMPTY, dedicated: true,
        operator: false, orders: [orderA] });
    let m = ctx.cardModel(cleared, 'PART-A');
    eq([m.statusText, m.action, m.loadNow, m.detail], ['LOAD', 'load', true, 'Empty bin at node — tap to load'],
        'clear in flight, today: the emptied home reads LOAD while Order A waits (the agnostic-demand branch)');
    eq(ctx.isOwnEmptyOut(cleared, orderA), false, 'clear in flight: Order A is not the produce home\'s own empty-out');
    eq(m.queueCount, 0, 'clear in flight: no queue badge (Order A carries no part)');
    setView([cleared]);
    render();
    eq(part(cardFor('PART-A', 'SMN_H1'), 'os-board-tag'), 'LOAD', 'clear in flight, today: the home card draws LOAD');
    await tap(cardFor('PART-A', 'SMN_H1'));
    eq(loadOpens, [{ node: 1, codes: ['PART-A'], cap: 0 }], 'clear in flight, today: the LOAD card opens the load modal');
    // The node modal says TAP TO LOAD on the same home.
    resetDOM();
    ctx.openModal(1);
    eq(modalActions().map(a => a.dataset.action).filter(a => a.indexOf('demand-card') === 0), ['demand-card:PART-A'],
        'clear in flight, today: the node modal offers the load (TAP TO LOAD)');

    // The same moment, with the acked status: still LOAD (the order carries no part).
    m = ctx.cardModel(Object.assign({}, cleared, { orders: [Object.assign({}, orderA, { status: 'acknowledged' })] }), 'PART-A');
    eq(m.statusText, 'LOAD', 'clear in flight, Order A acknowledged: still LOAD');
    // Before Core's zero is read back (the bin still shows its part): no demand.
    m = ctx.cardModel(Object.assign({}, cleared, { bin_state: FULL('PART-A', 12) }), 'PART-A');
    eq([m.statusText, m.action], ['NO DEMAND', ''], 'clear in flight, bin still labelled: NO DEMAND');
    // Once the carrier is lifted (no bin, Order A in transit): no demand.
    m = ctx.cardModel(Object.assign({}, cleared, { bin_state: NONE,
        orders: [Object.assign({}, orderA, { status: 'in_transit' })] }), 'PART-A');
    eq([m.statusText, m.action], ['NO DEMAND', ''], 'clear in flight, carrier lifted: NO DEMAND');
}

// ── 5. The slot grid (consume, shared window) ──────────────────────────────
{
    const slots = [
        loaderTile(1, 'ULN_A1', { role: 'consume', parts: ['PART-A', 'PART-B'], bin: FULL('PART-A', 7), uop: 7,
            operator: true, outbound: 'EMPTY-SUPER' }),
        loaderTile(2, 'ULN_A2', { role: 'consume', parts: ['PART-A', 'PART-B'], bin: EMPTY, operator: true }),
        loaderTile(3, 'ULN_A3', { role: 'consume', parts: ['PART-A', 'PART-B'], bin: NONE, operator: true }),
    ];
    setView(slots);
    render();
    eq(slotTiles().length, 3, 'slot grid: a consume shared-window loader renders the node grid, one tile per window');
    eq(cards().length, 0, 'slot grid: no payload cards');
    eq(headerButtons(), ['CHANGEOVER', 'REFRESH'], 'slot grid: the header keeps CHANGEOVER (more than one node)');
    // CHANGEOVER on that header: the picker (the composer is not loaded here)
    // and a style's tap posts changeover/start.
    await tap(headerButton('CHANGEOVER'));
    await tapOverlay('STYLE-1');
    eq(posts(), [{ method: 'POST', url: '/api/processes/1/changeover/start',
        body: { to_style_id: 11, called_by: 'Loader station', notes: '' } }],
    'slot grid: CHANGEOVER\'s picker posts changeover/start');
    // During a changeover the same header offers CUTOVER and CANCEL.
    setView(slots, { active_changeover: { id: 5, to_style_name: 'STYLE-2', from_style_name: 'STYLE-1' },
        process: { id: 1, name: 'LDR', active_style_id: 11, target_style_id: 12 } });
    render();
    eq(headerButtons(), ['CUTOVER', 'CANCEL', 'REFRESH'], 'slot grid during a changeover: CUTOVER and CANCEL');
    await tap(headerButton('CUTOVER'));
    await tapOverlay('CONFIRM CUTOVER');
    eq(posts(), [{ method: 'POST', url: '/api/processes/1/changeover/cutover', body: {} }], 'CUTOVER posts changeover/cutover');
    render();
    await tap(headerButton('CANCEL'));
    await tapOverlay('CANCEL CHANGEOVER');
    eq(posts(), [{ method: 'POST', url: '/api/processes/1/changeover/cancel', body: {} }], 'CANCEL posts changeover/cancel');
    setView(slots);
    render();
    const words = n => slotTile(n).children.find(k => k.classList.contains('os-node-remaining')).textContent;
    eq([words('ULN_A1'), words('ULN_A2'), words('ULN_A3')], ['PART-A', 'EMPTY', 'NO BIN'], 'slot grid: the slot words');

    // A full: the CLEAR panel. Two types for the window's parts: a picker.
    await tap(slotTile('ULN_A1'));
    eq(overlayTexts()[0], 'Full pulled, empty filled?', 'slot full: the CLEAR panel');
    eq(overlayButtons(), ['TOTE-1', 'TOTE-2', 'CANCEL'], 'slot full: a type per part');
    eq(overlayTexts().indexOf('This slot still counts 7. Confirming writes it to zero.') >= 0, true, 'slot full: the discard count');
    await tapOverlay('TOTE-2');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/clear-bin', body: { bin_type_code: 'TOTE-2' } }],
        'slot full: a type\'s tap posts clear-bin with it');

    // An empty: PUSH EMPTY and PUSH AS.
    render();
    await tap(slotTile('ULN_A2'));
    eq(overlayButtons(), ['PUSH EMPTY', 'PUSH AS TOTE-1', 'PUSH AS TOTE-2', 'CANCEL'], 'slot empty: PUSH EMPTY and PUSH AS');
    await tapOverlay('PUSH EMPTY');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/2/push-empty', body: {} }], 'PUSH EMPTY posts push-empty');
    render();
    await tap(slotTile('ULN_A2'));
    await tapOverlay('PUSH AS TOTE-1');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/2/clear-bin', body: { bin_type_code: 'TOTE-1' } }],
        'PUSH AS posts clear-bin with the type');

    // No bin: the tap does nothing.
    render();
    await tap(slotTile('ULN_A3'));
    eq([overlay(), posts(), document.getElementById('node-modal').classList.contains('active')], [null, [], false],
        'slot with no bin: the tap does nothing');

    // One type for the parts: auto-filled, one button.
    setView([loaderTile(1, 'ULN_A1', { role: 'consume', parts: ['PART-A'], bin: FULL('PART-A', 0), operator: true })]);
    render();
    await tap(slotTile('ULN_A1'));
    eq(overlayButtons(), ['CONFIRM SWAP', 'CANCEL'], 'slot full, one type: CONFIRM SWAP');
    await tapOverlay('CONFIRM SWAP');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/clear-bin', body: { bin_type_code: 'TOTE-1' } }],
        'CONFIRM SWAP posts the one type');
}
{
    // Core unreachable: the handler stamps no bin_state. Fixed: the slot said
    // NO BIN, a fact nobody had established; it says it could not look. The tap
    // does nothing, as before.
    setView([loaderTile(1, 'ULN_A1', { role: 'consume', parts: ['PART-A'], operator: true }),
        loaderTile(2, 'ULN_A2', { role: 'consume', parts: ['PART-A'], operator: true })]);
    render();
    eq(slotTile('ULN_A1').children.find(k => k.classList.contains('os-node-remaining')).textContent, 'COULD NOT LOOK',
        'Core unreachable, fixed: the slot says it could not look');
    await tap(slotTile('ULN_A1'));
    eq([overlay(), posts()], [null, []], 'Core unreachable: the tap does nothing');
}

// ── 6. The two-stage unloader ──────────────────────────────────────────────
{
    setView([
        loaderTile(1, 'ST1_A1', { role: 'consume', parts: ['PART-A'], bin: FULL('PART-A', 4), uop: 4, leaves_bare: true, operator: true }),
        loaderTile(2, 'ST2_A1', { role: 'consume', parts: ['PART-A'], bin: Object.assign({}, EMPTY, { bare: true }), pulled_directly: true, operator: true }),
        loaderTile(3, 'ST2_A2', { role: 'consume', parts: ['PART-A'], bin: EMPTY, pulled_directly: true, operator: true }),
    ]);
    render();
    eq(slotTiles().length, 3, 'two-stage: the node grid');
    await tap(slotTile('ST1_A1'));
    eq(overlayButtons(), ['Full off', 'CANCEL'], 'stage 1 full: one tap, Full off');
    await tapOverlay('Full off');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/clear-bin', body: {} }], 'Full off posts clear-bin with no type');
    render();
    await tap(slotTile('ST2_A1'));
    eq(overlayButtons(), ['Send on', 'CANCEL'], 'stage 2 bare cart: one tap, Send on');
    await tapOverlay('Send on');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/2/clear-bin', body: {} }], 'Send on posts clear-bin with no type');
    render();
    await tap(slotTile('ST2_A2'));
    eq(overlayButtons(), ['PUSH EMPTY', 'PUSH AS TOTE-1', 'CANCEL'], 'stage 2 empty that is not bare: PUSH EMPTY');
}

// ── 7. The consume payload board (a station mixing roles, consume first) ───
{
    setView([
        loaderTile(1, 'ULN_M1', { role: 'consume', parts: ['PART-B', 'PART-A'], bin: NONE, operator: true }),
        loaderTile(2, 'SMN_M2', { role: 'produce', parts: ['PART-A'], bin: NONE, operator: true }),
    ]);
    render();
    eq(slotTiles().length, 0, 'mixed roles: not the slot grid (the slot grid needs every window an unloader\'s)');
    eq(headerButtons(), ['REFRESH'], 'mixed roles, fixed with the two windows: a payload board\'s header carries no cell buttons');
    eq(reqBarButtons().map(b => b.textContent), ['REQUEST FULL'], 'mixed roles: the first window\'s board, an unloader: REQUEST FULL');
    await tap(reqBarButtons()[0]);
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/request-full', body: { payload_code: 'PART-A' } }],
        'REQUEST FULL posts request-full with the first part the board lists (sorted on an operator-driven board)');
    // A delivered full on that board: the card's tap is the CLEAR panel.
    setView([
        loaderTile(1, 'ULN_M1', { role: 'consume', parts: ['PART-A'], bin: FULL('PART-A', 3), uop: 3, operator: true }),
        loaderTile(2, 'SMN_M2', { role: 'produce', parts: ['PART-A'], bin: NONE, operator: true }),
    ]);
    render();
    const c = cardFor('PART-A');
    eq(part(c, 'os-board-tag'), 'SWAP', 'consume card with its full parked: SWAP');
    await tap(c);
    await tapOverlay('CONFIRM SWAP');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/clear-bin', body: { bin_type_code: 'TOTE-1' } }],
        'consume card: the unload tap posts clear-bin through the panel');
}

// ── 8. The node modal on a loader tile ─────────────────────────────────────
{
    // A loader window on a station with a press tile: the node grid, and the
    // loader tile's tap opens the node modal.
    const press = { node: { id: 9, process_id: 1, core_node_name: 'PLN_X1', name: 'PLN_X1' },
        runtime: { remaining_uop_cached: 10, active_bin_id: 1 }, orders: [],
        active_claim: { id: 4, core_node_name: 'PLN_X1', role: 'produce', swap_mode: 'single_robot',
            payload_code: 'PART-A', uop_capacity: 20 } };
    setView([press, loaderTile(1, 'SMN_A1', { parts: ['PART-A'], bin: NONE })]);
    render();
    eq(slotTiles().length, 2, 'loader beside a press: the node grid');
    await tap(slotTile('SMN_A1'));
    eq([document.getElementById('node-modal').classList.contains('active'), posts()], [true, []],
        'loader beside a press: the loader tile opens the node modal');
}
{
    // Produce window, idle: the demand card asks for an empty with the part.
    setView([loaderTile(1, 'SMN_A1', { parts: ['PART-A', 'PART-B'], bin: NONE,
        supply_refusals: { 'PART-B': { refused_by: 'Loader one', answered: false } } })]);
    resetDOM();
    ctx.openModal(1);
    eq(modalActions().map(a => a.dataset.action), [
        '/api/process-nodes/1/request-empty|PART-A', '/api/process-nodes/1/request-empty|PART-B',
        'close', 'supply-refuse:PART-A', 'supply-undo:PART-B',
    ], 'modal, idle loader: a request card per part, CLOSE, and the refusal controls');
    eq(modalButtons(), ['CLOSE', 'NO PARTS AVAILABLE — PART-A', 'UNDO — I CAN SUPPLY — PART-B'], 'modal: the refusal labels');
    await tapModal('/api/process-nodes/1/request-empty|PART-A');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/request-empty', body: { payload_code: 'PART-A' } }],
        'modal request card: posts request-empty WITH the part (the board bar posts none)');

    resetDOM();
    ctx.openModal(1);
    await tapModal('supply-refuse:PART-A');
    eq(overlayButtons(), ['YES — TELL THEM', 'CANCEL'], 'NO PARTS AVAILABLE: the confirm');
    await tapOverlay('YES — TELL THEM');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/supply-refusal', body: { payload_code: 'PART-A' } }],
        'NO PARTS AVAILABLE posts supply-refusal with the part');
    resetDOM();
    ctx.openModal(1);
    await tapModal('supply-undo:PART-B');
    await tapOverlay('YES — I CAN SUPPLY');
    eq(posts(), [{ method: 'DELETE', url: '/api/process-nodes/1/supply-refusal', body: { payload_code: 'PART-B' } }],
        'UNDO deletes supply-refusal with the part');

    // Delivered empty: the demand card loads, CONFIRM DELIVERY posts its order.
    setView([loaderTile(1, 'SMN_A1', { parts: ['PART-A'], bin: NONE,
        orders: [order(401, 'delivered', { payload_code: 'PART-A' })] })]);
    resetDOM();
    ctx.openModal(1);
    eq(modalButtons(), ['CONFIRM DELIVERY', 'CLOSE', 'NO PARTS AVAILABLE'], 'modal, delivered: CONFIRM DELIVERY');
    await tapModal('demand-card:PART-A');
    eq(loadOpens, [{ node: 1, codes: ['PART-A'], cap: 0 }], 'modal demand card: opens the load modal');
    await confirmLoad();
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/load-bin', body: loadBody }], 'modal demand card: LOAD posts load-bin');
    resetDOM();
    ctx.openModal(1);
    await tapModal('CONFIRM DELIVERY');
    eq(posts(), [{ method: 'POST', url: '/api/confirm-delivery/401', body: {} }], 'CONFIRM DELIVERY posts confirm-delivery');

    // The load modal carries the same refusal control for the part picked.
    resetDOM();
    ctx.openLoadBin(1, ['PART-A'], 0);
    await flush();
    eq(document.getElementById('load-bin-refuse').textContent, 'NO PARTS AVAILABLE', 'load modal: the refusal control');
    await document.getElementById('load-bin-refuse').click();
    await tapOverlay('YES — TELL THEM');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/supply-refusal', body: { payload_code: 'PART-A' } }],
        'load modal NO PARTS AVAILABLE posts supply-refusal with the part');

    // A loaded carrier: CLEAR BIN.
    setView([loaderTile(1, 'SMN_A1', { parts: ['PART-A'], bin: FULL('PART-A', 12), uop: 12 })]);
    resetDOM();
    ctx.openModal(1);
    eq(modalButtons(), ['CLEAR BIN', 'CLOSE', 'NO PARTS AVAILABLE'], 'modal, loaded loader: CLEAR BIN');
    await tapModal('CLEAR BIN');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/clear-bin', body: {} }], 'modal CLEAR BIN (loader): clear-bin, no type');

    // The unloader: the modal's CLEAR BIN goes through the panel the board's
    // tap opens. Fixed: it posted clear-bin with no carrier type, around it.
    setView([loaderTile(1, 'ULN_A1', { role: 'consume', parts: ['PART-A'], bin: FULL('PART-A', 7), uop: 7 }),
        loaderTile(2, 'ULN_A2', { role: 'consume', parts: ['PART-A'], bin: NONE })]);
    resetDOM();
    ctx.openModal(1);
    eq(modalButtons(), ['CLEAR BIN', 'CLOSE', 'NO PARTS AVAILABLE'], 'modal, unloader full: CLEAR BIN');
    await tapModal('CLEAR BIN');
    eq(overlayButtons(), ['CONFIRM SWAP', 'CANCEL'], 'modal CLEAR BIN (unloader), fixed: the CLEAR panel');
    await tapOverlay('CONFIRM SWAP');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/clear-bin', body: { bin_type_code: 'TOTE-1' } }],
        'modal CLEAR BIN (unloader), fixed: clear-bin with the carrier type');
    // An idle unloader window: the request card asks for a full of the part.
    resetDOM();
    ctx.openModal(2);
    await tapModal('/api/process-nodes/2/request-full|PART-A');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/2/request-full', body: { payload_code: 'PART-A' } }],
        'modal request card (unloader): posts request-full with the part');
}

// ── 9. The dedicated unloader and "no part running" ──────────────────────
{
    // A dedicated-position unloader with nothing making its parts. Fixed: every
    // home read ACTIVE and "Lineside 0 UoP" whatever ran; a home is ACTIVE when a
    // running cell makes its part (a produce row of view.serves).
    const unloaderHomes = [
        loaderTile(1, 'SMN_U1', { role: 'consume', parts: ['PART-A'], bin: NONE, dedicated: true, operator: true }),
        loaderTile(2, 'SMN_U2', { role: 'consume', parts: ['PART-B'], bin: NONE, dedicated: true, operator: true }),
    ];
    setView(unloaderHomes, { serves: [] });
    render();
    eq(cards().map(c => [part(c, 'os-board-cov'), part(c, 'os-board-tag'), part(c, 'os-board-lineside')]),
        [['PRELOAD', 'PRELOAD', null], ['PRELOAD', 'PRELOAD', null]],
        'dedicated unloader, fixed: nothing makes its parts, no home reads ACTIVE');
    setView(unloaderHomes, { serves: [row('PRESS-1', 'PART-B', 40, { role: 'produce' })] });
    render();
    eq(part(cardFor('PART-B', 'SMN_U2'), 'os-board-cov'), 'ACTIVE', 'dedicated unloader: a home whose part a running press makes reads ACTIVE');
    eq(headerButtons(), ['PULL FULL FROM HOME', 'REFRESH'], 'dedicated unloader: the header offers PULL FULL FROM HOME');

    // No part running: serves is [] (read, nothing runs), and a queued call is
    // not downtime. Fixed: no part running arrived as no data and the call
    // escalated. A board that could not look (no serves) still escalates.
    const queued = [order(501, 'queued', { payload_code: 'PART-A', queue_reason: 'no empty in the market' })];
    setView([loaderTile(1, 'SMN_N1', { parts: ['PART-A'], bin: NONE, orders: queued })], { serves: [] });
    render();
    let c = cardFor('PART-A');
    eq([c.classList.contains('os-board-card--waiting'), part(c, 'os-board-downtime')], [false, null],
        'no part running, fixed: the queued call is not downtime');
    setView([loaderTile(1, 'SMN_N1', { parts: ['PART-A'], bin: NONE, orders: queued })]);
    render();
    c = cardFor('PART-A');
    eq([c.classList.contains('os-board-card--waiting'), part(c, 'os-board-downtime')],
        [true, 'QUEUED 5m — no empty in the market'],
        'could not look (no serves): the queued call escalates as downtime');
    // On a normal loader the cards come from the active list, so a part no
    // running style uses has no card at all.
    setView([loaderTile(1, 'SMN_N1', { parts: ['PART-A', 'PART-B'], bin: NONE, orders: queued })],
        { serves: [row('CELL-N', 'PART-B', 20)] });
    render();
    eq(cardFor('PART-A'), null, 'normal loader: a queued part no running style uses draws no card');
    // An operator loader's board draws every part; a list naming another part
    // keeps the queued call out of scope.
    setView([loaderTile(1, 'SMN_N1', { parts: ['PART-A', 'PART-B'], bin: NONE, orders: queued, operator: true })],
        { serves: [row('CELL-N', 'PART-B', 20)] });
    render();
    c = cardFor('PART-A');
    eq([c.classList.contains('os-board-card--waiting'), part(c, 'os-board-downtime')], [false, null],
        'a running style that does not use the part: no escalation');
}

// ── 10. The changeover banner ──────────────────────────────────────────────
{
    // Changed: the banner is drawn from the served rows that wait on what this
    // loader makes (view.serves, waiting), one rule for every served cell; it was
    // the station's own process's incoming produce claims, whatever was short.
    // The directive setting still gates it.
    const waiting = { serves: [row('CELL-C', 'PART-A', 0, { incoming: true, waiting: true, carrier: 'TOTE-1', node: 'WLN_C1' }),
        row('CELL-C', 'PART-B', 0, { incoming: true, carrier: 'TOTE-2', node: 'WLN_C1' })] };
    setView([loaderTile(1, 'SMN_C1', { parts: ['PART-A', 'PART-B'], bin: NONE, directive: true })], waiting);
    render();
    const banner = grid.children.find(n => n.classList.contains('os-changeover-load-directive'));
    eq(banner && banner.innerHTML, '<div class="os-cld-label">CHANGEOVER — LOAD</div><div class="os-cld-bintype">TOTE-1</div>' +
        '<div class="os-cld-why">for PART-A</div><div class="os-cld-who">waiting: WLN_C1</div>',
    'banner: the payload board draws what the waiting rows wait for, the bin type first (a row not waiting is not named)');
    eq(grid.children.indexOf(banner), 1, 'banner: under the board header, above the request bar');
    setView([loaderTile(1, 'SMN_C1', { parts: ['PART-A'], bin: NONE })], waiting);
    render();
    eq(grid.descendants().some(n => n.classList.contains('os-changeover-load-directive')), false,
        'banner: the directive setting off draws nothing');
    setView([loaderTile(1, 'SMN_C1', { parts: ['PART-A'], bin: NONE, directive: true })],
        { serves: [row('CELL-C', 'PART-A', 0, { incoming: true, carrier: 'TOTE-1' })] });
    render();
    eq(grid.descendants().some(n => n.classList.contains('os-changeover-load-directive')), false,
        'banner: nothing waiting draws nothing');
    setView([loaderTile(1, 'SMN_H1', { parts: ['PART-A'], bin: NONE, dedicated: true, operator: true, directive: true })], waiting);
    render();
    eq(grid.descendants().some(n => n.classList.contains('os-changeover-load-directive')), false,
        'banner: the home board does not draw it');
    setView([loaderTile(1, 'ULN_C1', { role: 'consume', parts: ['PART-A'], bin: NONE, directive: true }),
        loaderTile(2, 'ULN_C2', { role: 'consume', parts: ['PART-A'], bin: NONE })], waiting);
    render();
    eq(grid.descendants().some(n => n.classList.contains('os-changeover-load-directive')), false,
        'banner: the slot grid does not draw it');
    setView([loaderTile(1, 'SMN_C1', { parts: ['PART-A'], bin: NONE, directive: true })],
        { serves: [row('CELL-C', 'PART-A', 0, { incoming: true, waiting: true, carrier: '' })] });
    render();
    eq(grid.descendants().some(n => n.classList.contains('os-changeover-load-directive')), false,
        'banner: a waiting row with no bin type draws nothing');
}

if (failed) { console.error(passed + ' passed, ' + failed + ' FAILED'); process.exit(1); }
console.log('operator loader station: ' + passed + ' passed');
process.exit(0);
})().catch(function (err) { console.error(err && err.stack || err); process.exit(1); });
