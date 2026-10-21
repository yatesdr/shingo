// Characterisation of the loader and unloader station's glass as it is:
// what the board draws for each layout, every act it offers a loader or
// unloader tile, the state that offers it, and exactly what its tap posts.
//
// WHY THE WHOLE PATH AND NOT cardModel. cardModel says load, unload or
// nothing; the acts themselves are chosen in the board's model (boardModel,
// operator-board-model.js) and opened by the render layer (renderGrid's
// boardActs, renderHeader, createNodeButton on the node grid), in the node
// modal (renderModal, handleModalAction) and in the load modal (openLoadBin).
// So this loads the SHIPPING modules whole — operator-render, operator-board,
// operator-modal, operator-load-bin, operator-supply-refusal,
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
    openReleasePrompt() {}, closeReleasePrompt() {},
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
['../../../../shared/esc.js', 'order-status.js', 'operator-util.js', 'operator-state.js', 'operator-window-state.js',
    'operator-board-model.js', 'operator-board.js', 'operator-supply-refusal.js', 'operator-load-bin.js', 'operator-modal.js',
    'operator-render.js',
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
// (Core's replenishment `operator`), all_parts (the board setting, defaulted
// from operator as Core's backfill set it), dedicated
// (dedicated_positions), outbound, directive, leaves_bare, pulled_directly, bin,
// uop, orders, buffer_partial, supply_refusals and buffer (tile.loader.buffer,
// which rides on a loader's first home only).
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
            // The setting as Core's v157 backfilled it: an operator station
            // lists every part.
            board_shows_all_parts: o.all_parts === undefined ? !!o.operator : !!o.all_parts,
        },
        orders: o.orders || [],
        swap_ready: false,
        releases_as_pair: false,
    };
    if (role === 'consume') t.loader.carriers = carriersFor(parts);
    if (o.buffer) t.loader.buffer = o.buffer;
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

const FULL = (part, uop) => ({ occupied: true, payload_code: part, stocked: (uop == null ? 12 : uop) > 0, bin_label: 'BIN-' + part, bin_type_code: 'TOTE-1' });
const EMPTY = { occupied: true, payload_code: '', stocked: false, bin_label: 'BIN-E', bin_type_code: 'TOTE-1' };
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
function part(card, cls) {
    const n = card.descendants().find(k => k.classList.contains(cls));
    return n ? n.textContent : null;
}
function slotTiles() { return grid.children.filter(n => n.classList.contains('os-node-btn')); }
function slotTile(name) {
    return slotTiles().find(t => t.children.some(k => k.classList.contains('os-node-name') && k.textContent === name)) || null;
}
// The board (operator-board.js): its cards, its zones, its top line and bar.
function pcCards() { return grid.descendants().filter(n => n.classList.contains('os-pc-card') && !n.classList.contains('os-pc-more')); }
const LEAVES = ['os-pc-spot-name', 'os-pc-spot-words', 'os-pc-spot-code', 'os-pc-headline', 'os-pc-also', 'os-pc-word', 'os-pc-why', 'os-pc-note'];
function cardText(card) {
    return card.descendants().filter(n => LEAVES.some(c => n.classList.contains(c))).map(n => n.textContent);
}
function pcCard(headline, spot) {
    return pcCards().find(c => part(c, 'os-pc-headline') === headline && (!spot || part(c, 'os-pc-spot-name') === spot)) || null;
}
function zoneEls() { return grid.descendants().filter(n => n.classList.contains('os-pc-zone')); }
function zoneTitles() { return zoneEls().map(z => part(z, 'os-pc-zone-title')); }
function zoneEl(title) { return zoneEls().find(z => part(z, 'os-pc-zone-title') === title) || null; }
function zoneButton(title, label) {
    const b = zoneEl(title).descendants().find(n => n.tagName === 'BUTTON' && n.textContent === label);
    if (!b) throw new Error('no ' + label + ' in ' + title);
    return b;
}
function topLine() {
    const top = grid.descendants().find(n => n.classList.contains('os-pc-top'));
    return top ? [part(top, 'os-pc-title'), part(top, 'os-pc-words'), part(top, 'os-pc-showall')] : [];
}
function showAllButton() { return grid.descendants().find(n => n.classList.contains('os-pc-showall')); }
function barButtons() {
    const bar = grid.descendants().find(n => n.classList.contains('os-pc-bar'));
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

// ── 1. A loader of one shared window (the old payload board) ───────────────
{
    // An empty delivered and nothing parked: the Load card, the board bar's
    // Request empty greyed because an order is live.
    setView([loaderTile(1, 'SMN_A1', {
        parts: ['PART-A', 'PART-B'], bin: NONE,
        orders: [order(101, 'delivered', { payload_code: 'PART-A', delivery_node: 'SMN_A1' })],
    })], { serves: [row('CELL-1', 'PART-A', 30)] });
    render();
    // Changed: the board draws its own top line; the station header carries
    // no REFRESH and no cell buttons (a forklift's screen; the loader's acts
    // are its cards and its bar).
    eq(headerButtons(), [], 'board: the header carries no buttons (it was REFRESH)');
    eq(topLine(), ['Loader station', '1 to load', 'Show all 1'], 'board: the top line, its counts and Show all');
    eq(barButtons().map(b => b.textContent), ['Request empty — bin already inbound', 'Pull from market'],
        'board bar: Request empty greyed while an order is live; Pull from market beside it (sentence case)');
    eq(barButtons()[0].disabled, true, 'board bar: the greyed Request empty is disabled');
    const c = pcCard('PART-A');
    eq(cardText(c), ['SMN_A1', ' · no bin', 'PART-A', '· CELL-1 has 30 left'.slice(2)],
        'Load card: the spot, the part, the cell it serves');
    eq(pcCard('PART-B'), null, 'a part no running style uses on a threshold loader draws no card');
    await tap(c);
    eq(loadOpens, [{ node: 1, codes: ['PART-A'], cap: 0 }], 'LOAD: the card opens the load modal for that part, capacity 0 (the synthesised claim has none)');
    await confirmLoad();
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/load-bin', body: loadBody }],
        'LOAD: CONFIRM LOAD posts load-bin with the part, the UoP count and the manifest scaled to it');
}
{
    // Nothing parked, nothing live: Request empty is offered and posts an empty body.
    setView([loaderTile(1, 'SMN_A1', { parts: ['PART-A'], bin: NONE })], { serves: [row('CELL-1', 'PART-A', 30)] });
    render();
    const req = barButtons()[0];
    eq(req.textContent, 'Request empty', 'REQUEST EMPTY: offered with no bin and no live order');
    await tap(req);
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/request-empty', body: {} }],
        'REQUEST EMPTY: posts request-empty with no payload code');

    // PULL FROM MARKET: a picker filled from market-bins; a bin's tap posts its node.
    render();
    await tap(barButtons()[1]);
    eq(requests.map(r => r.method + ' ' + r.url), ['GET /api/process-nodes/1/market-bins'],
        'PULL FROM MARKET: the picker reads the market');
    await tapOverlay('SMN_M1PART-AUoP: 40');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/pull-from-market',
        body: { source_core_node: 'MARKET-GROUP.SMN_M1' } }],
    'PULL FROM MARKET: a bin\'s tap posts pull-from-market with its full node name');

    // No outbound destination on the claim: no PULL FROM MARKET.
    setView([loaderTile(1, 'SMN_A1', { parts: ['PART-A'], bin: NONE, outbound: '' })]);
    render();
    eq(barButtons().map(b => b.textContent), ['Request empty'], 'PULL FROM MARKET: absent without an outbound destination');

    // A bin parked: Request empty greyed for that reason.
    setView([loaderTile(1, 'SMN_A1', { parts: ['PART-A'], bin: EMPTY })]);
    render();
    eq(barButtons()[0].textContent, 'Request empty — bin at node', 'REQUEST EMPTY: greyed with a bin parked');
}
{
    // An empty parked with no order: LOAD on every allowed part, one card for
    // the window listing them (changed: a card per part).
    setView([loaderTile(1, 'SMN_A1', { parts: ['PART-A', 'PART-B'], bin: EMPTY })]);
    render();
    const c = pcCard('PART-A');
    eq([pcCards().length, part(c, 'os-pc-also')], [1, 'Also PART-B'],
        'a shared window: one card listing the parts called for (changed: a card per part)');
    await tap(c);
    eq(loadOpens, [{ node: 1, codes: ['PART-A', 'PART-B'], cap: 0 }],
        'a shared window\'s card opens the load modal with the parts called for; the pick posts as before');
    // A window with nothing to do draws no card; Show all draws it, and its
    // tap opens the node modal.
    setView([loaderTile(1, 'SMN_A1', { parts: ['PART-A'], bin: NONE, operator: true,
        orders: [order(102, 'in_transit', { payload_code: 'PART-A' })] })]);
    render();
    eq(pcCards().length, 0, 'a window with no act draws no card (changed: IN TRANSIT card)');
    await tap(showAllButton());
    eq(pcCards().length, 1, 'Show all: every tile drawn');
    await tap(pcCards()[0]);
    eq(document.getElementById('node-modal').classList.contains('active'), true,
        'Show all: a tile with no act opens the node modal');
    eq(posts(), [], 'opening the node modal posts nothing');
    await tap(showAllButton());
}

// ── 2. A loader of two shared windows ──────────────────────────────────────
{
    const two = [
        loaderTile(1, 'SMN_B1', { parts: ['PART-A'], bin: NONE, operator: true }),
        loaderTile(2, 'SMN_B2', { parts: ['PART-A'], bin: EMPTY, operator: true,
            orders: [order(201, 'delivered', { delivery_node: 'SMN_B2' })] }),
    ];
    const serves = { serves: [row('CELL-2', 'PART-A', 10)] };
    setView(two, serves);
    render();
    eq(headerButtons(), [], 'two windows: no header buttons');
    eq(pcCards().map(c => cardText(c).slice(0, 2)), [['SMN_B2', ' · holds an empty']],
        'two windows: the window with an act draws its card, naming the window and what stands there');
    await tap(pcCard('PART-A', 'SMN_B2'));
    eq(loadOpens, [{ node: 2, codes: ['PART-A'], cap: 0 }], 'two windows: the second window\'s LOAD opens its load modal');
    render();
    eq(barButtons()[0].textContent, 'Request empty', 'two windows: Request empty offered while a window of the loader is free');
    await tap(barButtons()[0]);
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/request-empty', body: {} }],
        'two windows: REQUEST EMPTY posts for the first window (the server stages at a free one)');
    setView([loaderTile(1, 'SMN_B1', { parts: ['PART-A'], bin: EMPTY, operator: true }), two[1]], serves);
    render();
    eq(barButtons()[0].textContent, 'Request empty — bin at node', 'two windows: greyed only when no window is free');
    setView([loaderTile(1, 'SMN_B1', { parts: ['PART-A'], bin: NONE, operator: true,
        orders: [order(202, 'in_transit', { delivery_node: 'SMN_B1' })] }), two[1]], serves);
    render();
    eq(barButtons()[0].textContent, 'Request empty — bin already inbound', 'two windows: greyed with an empty on its way to the free one');
}

// ── 3. Dedicated homes (produce) ───────────────────────────────────────────
{
    const buffer = [{ node: 'LDR.SMN_F1', state: 'partial' }, { node: 'LDR.SMN_F2', state: 'empty' }, { node: 'LDR.SMN_F3', state: 'open' }];
    const homes = [
        loaderTile(1, 'SMN_H1', { parts: ['PART-A'], bin: FULL('PART-A', 12), dedicated: true,
            operator: true, buffer: buffer, buffer_partial: { node: 'LDR.SMN_F1' } }),
        loaderTile(2, 'SMN_H2', { parts: ['PART-B'], bin: EMPTY, dedicated: true, operator: true }),
        loaderTile(3, 'SMN_H3', { parts: ['PART-A'], bin: FULL('PART-A', 5), dedicated: true, operator: true }),
    ];
    const homeServes = { serves: [row('CELL-H', 'PART-A', 12), row('CELL-H', 'PART-B', 4)] };
    setView(homes, homeServes);
    render();
    // Changed: the Clear partials cards replace PULL FULL FROM HOME.
    eq(headerButtons(), [], 'homes: no PULL FULL FROM HOME (the Clear partials cards replace it)');
    eq(pcCards().map(c => cardText(c)), [['SMN_H2', ' · holds an empty', ' TOTE-1', 'PART-B', 'CELL-H has 4 left']],
        'homes: the empty home\'s Load card; stocked homes are hidden');
    eq(zoneTitles(), ['Load', 'Clear partials'], 'homes: the zones, Clear partials folded at the bottom');
    eq(part(zoneEl('Clear partials'), 'os-pc-strip-words'), '1 partial · 1 empty · 1 open',
        'Clear partials, a normal day: folded to its strip and words');
    await tap(zoneButton('Clear partials', 'Show'));
    const h1 = pcCard('PART-A', 'SMN_H1');
    eq([cardText(h1), part(h1, 'os-pc-num')], [['SMN_H1', ' · holds a full bin', 'PART-A', 'Pull the full · partial from SMN_F1'], '1'],
        'Clear partials opened by a tap: the numbered card, the home act, where the partial comes from');

    // The card's tap: the confirm, naming the home and the spot, then
    // clear-loader-home with an empty body.
    await tap(h1);
    eq(overlayTexts()[0], 'Clear SMN_H1?', 'the clear\'s confirm names the home (changed: it named no node)');
    eq(overlayTexts()[1], 'Declares the bin at SMN_H1 empty. Shingo brings the partial from SMN_F1 to SMN_H1, and SMN_F1 ends holding the empty.',
        'the clear\'s confirm names the spot and says it ends holding an empty');
    eq(overlayButtons(), ['CONFIRM CLEAR', 'CANCEL'], 'the clear\'s confirm offers CONFIRM CLEAR');
    await tapOverlay('CONFIRM CLEAR');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/clear-loader-home', body: {} }],
        'CLEAR: posts clear-loader-home with no body fields (the engine picks the first matching partial, the one the card names)');
    await tap(zoneButton('Clear partials', 'Hide'));

    // LOAD from an empty home.
    render();
    await tap(pcCard('PART-B', 'SMN_H2'));
    eq(loadOpens, [{ node: 2, codes: ['PART-B'], cap: 0 }], 'homes: LOAD on an empty home opens the load modal for its part');

    // PULL FULL FROM HOME stays on a station that mixes a home with a press
    // (the node grid keeps its header), and ends in the same post.
    const pressTile = { node: { id: 9, process_id: 1, core_node_name: 'PLN_X1', name: 'PLN_X1' },
        runtime: { remaining_uop_cached: 10, active_bin_id: 1 }, orders: [],
        active_claim: { id: 4, core_node_name: 'PLN_X1', role: 'produce', swap_mode: 'single_robot', payload_code: 'PART-A', uop_capacity: 20 } };
    setView([pressTile].concat(homes), homeServes);
    render();
    eq(headerButtons(), ['PULL FULL FROM HOME', 'REFRESH'], 'a home beside a press: the node grid keeps PULL FULL FROM HOME');
    await tap(headerButton('PULL FULL FROM HOME'));
    eq(overlayButtons(), ['SMN_H1PART-A', 'CANCEL'], 'PULL FULL FROM HOME: only the home with a buffer partial is listed');
    await tapOverlay('SMN_H1PART-A');
    await tapOverlay('CONFIRM CLEAR');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/clear-loader-home', body: {} }],
        'PULL FULL FROM HOME: posts clear-loader-home for the picked home');
}

// ── 4. A home while its clear is in flight (Order A on the home's orders) ──
{
    // ClearLoaderHome zeroes the home's bin on Core (its payload goes blank)
    // and files Order A: a blank-payload move of the carrier, from the home to
    // the buffer spot. isOwnEmptyOut counts only a CONSUME node's own move, so
    // on a produce home Order A is general (untagged) demand over an empty bin.
    const orderA = order(301, 'queued', { order_type: 'move', source_node: 'SMN_H1', delivery_node: 'SMN_F1' });
    const cleared = loaderTile(1, 'SMN_H1', { parts: ['PART-A'], bin: EMPTY, dedicated: true,
        operator: false, orders: [orderA], buffer: [{ node: 'SMN_F1', state: 'partial' }] });
    let m = ctx.cardModel(cleared, 'PART-A');
    eq([m.statusText, m.action, m.loadNow, m.detail], ['LOAD', 'load', true, 'Empty bin at node — tap to load'],
        'clear in flight: cardModel reads the emptied home as LOAD while Order A waits (the agnostic-demand branch)');
    eq(ctx.isOwnEmptyOut(cleared, orderA), false, 'clear in flight: Order A is not the produce home\'s own empty-out');
    eq(m.queueCount, 0, 'clear in flight: no queue badge (Order A carries no part)');
    setView([cleared]);
    render();
    // Changed: a home whose own move is pending draws no Load card.
    eq(pcCards().length, 0, 'clear in flight: the home draws no Load card (changed: it drew LOAD)');
    eq([...zoneEl('Clear partials').descendants().filter(n => n.classList.contains('os-pc-spot-box'))].map(n => n.className),
        ['os-pc-spot-box os-pc-spot-box--coming'], 'clear in flight: its spot is drawn as coming');
    // The node modal still says TAP TO LOAD on the same home (not the board's).
    resetDOM();
    ctx.openModal(1);
    eq(modalActions().map(a => a.dataset.action).filter(a => a.indexOf('demand-card') === 0), ['demand-card:PART-A'],
        'clear in flight, today: the node modal offers the load (TAP TO LOAD)');

    m = ctx.cardModel(Object.assign({}, cleared, { orders: [Object.assign({}, orderA, { status: 'acknowledged' })] }), 'PART-A');
    eq(m.statusText, 'LOAD', 'clear in flight, Order A acknowledged: still LOAD');
    m = ctx.cardModel(Object.assign({}, cleared, { bin_state: FULL('PART-A', 12) }), 'PART-A');
    eq([m.statusText, m.action], ['NO DEMAND', ''], 'clear in flight, bin still labelled: NO DEMAND');
    m = ctx.cardModel(Object.assign({}, cleared, { bin_state: NONE,
        orders: [Object.assign({}, orderA, { status: 'in_transit' })] }), 'PART-A');
    eq([m.statusText, m.action], ['NO DEMAND', ''], 'clear in flight, carrier lifted: NO DEMAND');
}

// ── 5. A shared unloader (the old slot grid) ───────────────────────────────
{
    const slots = [
        loaderTile(1, 'ULN_A1', { role: 'consume', parts: ['PART-A', 'PART-B'], bin: FULL('PART-A', 7), uop: 7,
            operator: true, outbound: 'EMPTY-SUPER' }),
        loaderTile(2, 'ULN_A2', { role: 'consume', parts: ['PART-A', 'PART-B'], bin: EMPTY, operator: true }),
        loaderTile(3, 'ULN_A3', { role: 'consume', parts: ['PART-A', 'PART-B'], bin: NONE, operator: true }),
    ];
    setView(slots);
    render();
    // Changed: a shared unloader is a board, one list of carts; its header
    // carries no CHANGEOVER.
    eq(slotTiles().length, 0, 'a shared unloader is a board, not the node grid (changed)');
    eq(headerButtons(), [], 'a shared unloader: no CHANGEOVER (changed: the header kept it)');
    eq(zoneTitles(), ['At the windows'], 'a shared unloader: one list');
    eq(pcCards().map(c => cardText(c)), [
        ['ULN_A1', ' · holds a full bin', ' · PART-A', ' · 7', 'TOTE-1', 'Send back TOTE-1'],
        ['ULN_A2', ' · holds an empty bin', 'TOTE-1', 'Send it'],
    ], 'At the windows: one card a cart, the bin the headline; a station of two bin types says what to send back');

    // A full: the CLEAR panel. Two types for the window's parts: a picker.
    await tap(pcCard('TOTE-1', 'ULN_A1'));
    eq(overlayTexts()[0], 'Full pulled, empty filled?', 'cart full: the CLEAR panel');
    eq(overlayButtons(), ['TOTE-1', 'TOTE-2', 'CANCEL'], 'cart full: a type per part');
    eq(overlayTexts().indexOf('This slot still counts 7. Confirming writes it to zero.') >= 0, true, 'cart full: the discard count');
    await tapOverlay('TOTE-2');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/clear-bin', body: { bin_type_code: 'TOTE-2' } }],
        'cart full: a type\'s tap posts clear-bin with it');

    // An empty: PUSH EMPTY and PUSH AS.
    render();
    await tap(pcCard('TOTE-1', 'ULN_A2'));
    eq(overlayButtons(), ['PUSH EMPTY', 'PUSH AS TOTE-1', 'PUSH AS TOTE-2', 'CANCEL'], 'cart empty: PUSH EMPTY and PUSH AS');
    await tapOverlay('PUSH EMPTY');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/2/push-empty', body: {} }], 'PUSH EMPTY posts push-empty');
    render();
    await tap(pcCard('TOTE-1', 'ULN_A2'));
    await tapOverlay('PUSH AS TOTE-1');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/2/clear-bin', body: { bin_type_code: 'TOTE-1' } }],
        'PUSH AS posts clear-bin with the type');

    // No bin: no card; Show all draws it and opens the node modal (changed:
    // the slot's tap did nothing).
    render();
    eq(pcCard('PART-A', 'ULN_A3'), null, 'a window with no cart: no card');

    // One type for the parts: auto-filled, one button.
    setView([loaderTile(1, 'ULN_A1', { role: 'consume', parts: ['PART-A'], bin: FULL('PART-A', 0), operator: true })]);
    render();
    await tap(pcCards()[0]);
    eq(overlayButtons(), ['CONFIRM SWAP', 'CANCEL'], 'cart full, one type: CONFIRM SWAP');
    await tapOverlay('CONFIRM SWAP');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/clear-bin', body: { bin_type_code: 'TOTE-1' } }],
        'CONFIRM SWAP posts the one type');

    // A press waiting on another bin type: the note says change, the panel
    // lights it, every button still there.
    setView(slots, { serves: [row('P1', 'PART-A', 40, { role: 'produce', carrier: 'TOTE-2', capacity_uop: 40, waiting: true })] });
    render();
    const red = pcCards()[0];
    eq([cardText(red), red.className], [['ULN_A2', ' · holds an empty bin', 'TOTE-1', 'NOW', '· Change to TOTE-2'], 'os-pc-card os-pc-card--red'],
        'a full press with no empty ready: the first cart that can fix it is red, "Change to"');
    await tap(pcCard('TOTE-1', 'ULN_A1'));
    eq(body.children.filter(n => !n.removed).slice(-1)[0].descendants().filter(n => n.classList.contains('os-co-picker-btn--lit')).map(n => n.textContent),
        ['TOTE-2'], 'the CLEAR panel lights the card\'s send-back type');
}
{
    // Core unreachable: the handler stamps no bin_state. The windows hold no
    // cart the board can see: no card; the top line says nothing at the windows.
    setView([loaderTile(1, 'ULN_A1', { role: 'consume', parts: ['PART-A'], operator: true }),
        loaderTile(2, 'ULN_A2', { role: 'consume', parts: ['PART-A'], operator: true })]);
    render();
    eq([pcCards().length, topLine()[1]], [0, 'nothing at the windows'], 'Core unreachable: no card, nothing to tap');
    await tap(showAllButton());
    eq(cardText(pcCards()[0]).slice(0, 2), ['ULN_A1', ' · could not look'], 'Core unreachable: Show all says it could not look');
    await tap(showAllButton());
}

// ── 6. The two-stage unloader ──────────────────────────────────────────────
{
    setView([
        loaderTile(1, 'ST1_A1', { role: 'consume', parts: ['PART-A'], bin: FULL('PART-A', 4), uop: 4, leaves_bare: true, operator: true }),
        loaderTile(2, 'ST2_A1', { role: 'consume', parts: ['PART-A'], bin: Object.assign({}, EMPTY, { bare: true }), pulled_directly: true, operator: true }),
        loaderTile(3, 'ST2_A2', { role: 'consume', parts: ['PART-A'], bin: EMPTY, pulled_directly: true, operator: true }),
    ]);
    render();
    eq(pcCards().length, 3, 'two-stage: a card per cart');
    eq(part(pcCard('TOTE-1', 'ST2_A2'), 'os-pc-why'), 'The cart waits here for the line',
        'stage 2 pulled directly: the empty cart waits for the line');
    await tap(pcCard('TOTE-1', 'ST1_A1'));
    eq(overlayButtons(), ['Full off', 'CANCEL'], 'stage 1 full: one tap, Full off');
    await tapOverlay('Full off');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/clear-bin', body: {} }], 'Full off posts clear-bin with no type');
    render();
    await tap(pcCards().find(c => cardText(c)[0] === 'ST2_A1'));
    eq(overlayButtons(), ['Send on', 'CANCEL'], 'stage 2 bare cart: one tap, Send on');
    await tapOverlay('Send on');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/2/clear-bin', body: {} }], 'Send on posts clear-bin with no type');
    render();
    await tap(pcCard('TOTE-1', 'ST2_A2'));
    eq(overlayButtons(), ['PUSH EMPTY', 'PUSH AS TOTE-1', 'CANCEL'], 'stage 2 empty that is not bare: PUSH EMPTY');
}

// ── 7. A station mixing roles, consume first (the old consume payload board) ─
{
    setView([
        loaderTile(1, 'ULN_M1', { role: 'consume', parts: ['PART-B', 'PART-A'], bin: NONE, operator: true }),
        loaderTile(2, 'SMN_M2', { role: 'produce', parts: ['PART-A'], bin: NONE, operator: true }),
    ]);
    render();
    eq(slotTiles().length, 0, 'mixed roles: the board');
    eq(headerButtons(), [], 'mixed roles: no header buttons');
    eq(barButtons().map(b => b.textContent), ['Request full'], 'mixed roles: the first window\'s bar, an unloader: Request full');
    await tap(barButtons()[0]);
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/request-full', body: { payload_code: 'PART-A' } }],
        'REQUEST FULL posts request-full with the first part the board lists (sorted on an operator-driven board)');
    // A delivered full on that board: the card's tap is the CLEAR panel.
    setView([
        loaderTile(1, 'ULN_M1', { role: 'consume', parts: ['PART-A'], bin: FULL('PART-A', 3), uop: 3, operator: true }),
        loaderTile(2, 'SMN_M2', { role: 'produce', parts: ['PART-A'], bin: NONE, operator: true }),
    ]);
    render();
    await tap(pcCard('TOTE-1', 'ULN_M1'));
    await tapOverlay('CONFIRM SWAP');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/clear-bin', body: { bin_type_code: 'TOTE-1' } }],
        'consume card: the unload tap posts clear-bin through the panel');
    // An empty parked there: the old board's card opened the CLEAR, and still does.
    setView([
        loaderTile(1, 'ULN_M1', { role: 'consume', parts: ['PART-A'], bin: EMPTY, operator: true }),
        loaderTile(2, 'SMN_M2', { role: 'produce', parts: ['PART-A'], bin: NONE, operator: true }),
    ]);
    render();
    await tap(pcCard('TOTE-1', 'ULN_M1'));
    eq(overlayButtons(), ['CONFIRM SWAP', 'CANCEL'], 'consume empty on a mixed board: the CLEAR panel, as the old card opened');
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
    // That station's header keeps CHANGEOVER (the board's does not). The
    // composer is not loaded here, so its tap opens nothing and posts
    // nothing: the screen says it did not load.
    eq(headerButtons(), ['CHANGEOVER', 'REFRESH'], 'loader beside a press: the header keeps CHANGEOVER');
    render();
    await tap(headerButton('CHANGEOVER'));
    const toasts = document.getElementById('os-toast').children.filter(n => !n.removed).map(n => n.textContent);
    eq([posts(), overlayButtons(), toasts.slice(-1)],
        [[], [], ['The changeover screen did not load — tap CHANGEOVER again']],
        'CHANGEOVER with no composer: no picker, nothing posted');
    setView([press, loaderTile(1, 'SMN_A1', { parts: ['PART-A'], bin: NONE })],
        { active_changeover: { id: 5, to_style_name: 'STYLE-2', from_style_name: 'STYLE-1' },
            process: { id: 1, name: 'LDR', active_style_id: 11, target_style_id: 12 } });
    render();
    eq(headerButtons(), ['CUTOVER', 'CANCEL', 'REFRESH'], 'during a changeover: CUTOVER and CANCEL');
    await tap(headerButton('CUTOVER'));
    await tapOverlay('CONFIRM CUTOVER');
    eq(posts(), [{ method: 'POST', url: '/api/processes/1/changeover/cutover', body: {} }], 'CUTOVER posts changeover/cutover');
    render();
    await tap(headerButton('CANCEL'));
    await tapOverlay('CANCEL CHANGEOVER');
    eq(posts(), [{ method: 'POST', url: '/api/processes/1/changeover/cancel', body: {} }], 'CANCEL posts changeover/cancel');
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
    // A dedicated-position unloader with no cart at any home: the one list
    // says so in one quiet line. (Changed: the ACTIVE / PRELOAD cards are gone;
    // the board draws work only.)
    const unloaderHomes = [
        loaderTile(1, 'SMN_U1', { role: 'consume', parts: ['PART-A'], bin: NONE, dedicated: true, operator: true }),
        loaderTile(2, 'SMN_U2', { role: 'consume', parts: ['PART-B'], bin: NONE, dedicated: true, operator: true }),
    ];
    setView(unloaderHomes, { serves: [row('PRESS-1', 'PART-B', 40, { role: 'produce' })] });
    render();
    eq([pcCards().length, part(zoneEl('At the windows'), 'os-pc-zone-empty'), headerButtons()],
        [0, 'No cart at any window.', []], 'dedicated unloader: no cart, one quiet line, no header buttons');
    // A full delivered to a home with no cart read yet: the old card's Unload,
    // in its own zone.
    setView([loaderTile(1, 'SMN_U1', { role: 'consume', parts: ['PART-A'], bin: NONE, dedicated: true, operator: true,
        orders: [order(502, 'delivered', { payload_code: 'PART-A' })] })], { serves: [] });
    render();
    eq(zoneTitles(), ['At the windows', 'Unload'], 'a delivered full with no cart read: the catch-all zone, titled by its act');
    await tap(pcCard('PART-A'));
    await tapOverlay('CONFIRM SWAP');
    eq(posts(), [{ method: 'POST', url: '/api/process-nodes/1/clear-bin', body: { bin_type_code: 'TOTE-1' } }],
        'the catch-all Unload card opens the CLEAR panel, as the old card did');

    // A queued call with nothing coming draws no card (changed: the QUEUED
    // downtime card; the board draws a tile's act, and a queued call has none).
    const queued = [order(501, 'queued', { payload_code: 'PART-A', queue_reason: 'no empty in the market' })];
    setView([loaderTile(1, 'SMN_N1', { parts: ['PART-A'], bin: NONE, orders: queued })], { serves: [] });
    render();
    eq(pcCards().length, 0, 'a queued call with nothing to load draws no card');
}

// ── 10. The changeover banner is gone ──────────────────────────────────────
{
    // Changed: the directive setting gated the banner, which the board retires; a
    // cell changing over to a part it has none of is a red card where a tile
    // can fill it, and the top line names the cell either way.
    const waiting = { serves: [row('CELL-C', 'PART-A', 0, { incoming: true, waiting: true, carrier: 'TOTE-1', node: 'WLN_C1' })] };
    setView([loaderTile(1, 'SMN_C1', { parts: ['PART-A', 'PART-B'], bin: NONE, directive: true })], waiting);
    render();
    eq(grid.descendants().some(n => n.classList.contains('os-changeover-load-directive')), false, 'no banner on the board');
    eq(topLine()[1], '0 to load · CELL-C out', 'the cell changing over with none of the part is named in the top line');
    setView([loaderTile(1, 'SMN_C1', { parts: ['PART-A', 'PART-B'], bin: EMPTY, directive: true })], waiting);
    render();
    const c = pcCard('PART-A');
    eq([c.className, cardText(c).slice(-2)], ['os-pc-card os-pc-card--red', ['NOW', '· CELL-C out']],
        'an empty home that can fill it: the red card, NOW, no word "changeover"');
}

// ── 11. A board that overflows the screen ─────────────────────────────────
{
    // 40 homes to load, four refusals (two rows), at 1280 x 768: the zones give
    // way from the last. A cut zone draws the cards that fit and a "+N more"
    // for exactly the rest (a bug fixed: a zone cut to no card drew its title
    // and nothing to tap), and "+N more" opens Show all.
    const homes = [], serves = [];
    for (let i = 0; i < 40; i++) {
        const p = 'PART-' + (100 + i);
        homes.push(loaderTile(i + 1, 'SMN_' + (100 + i), { parts: [p], bin: EMPTY, dedicated: true }));
        serves.push(row('CELL-' + (i % 5), p, i === 0 ? 0 : 50 + i * 10));
    }
    for (let i = 0; i < 4; i++) {
        const p = 'PART-R' + i;
        homes.push(loaderTile(20 + i, 'SMN_R' + i, { parts: [p], bin: NONE, dedicated: true,
            supply_refusals: { [p]: { refused_at: new Date().toISOString() } } }));
        serves.push(row('CELL-R', p, 500));
    }
    setView(homes, { serves: serves });
    render();
    const more = z => zoneEl(z).descendants().filter(n => n.classList.contains('os-pc-more')).map(n => n.textContent);
    const drawn = zoneEl('Waiting on the plant').descendants().filter(n => n.classList.contains('os-pc-card')
        && !n.classList.contains('os-pc-more')).length;
    eq([drawn < 4, more('Waiting on the plant')], [true, ['+' + (4 - drawn) + ' more']],
        'overflow: a cut zone draws what fits and "+N more" for the rest');
    await tap(zoneEl('Waiting on the plant').descendants().find(n => n.classList.contains('os-pc-more')));
    eq(topLine()[2], 'Show work', '"+N more" opens Show all');
    await tap(showAllButton());
}

// ── 12. A band is replaced only when what it draws changed ────────────────
{
    // The board is drawn in bands (the top line, the status row, a section a
    // zone). A poll that moves one band rebuilds that band and keeps every
    // other one, its nodes and listeners; an unchanged view replaces nothing
    // (a bug fixed: every render rebuilt every card).
    const homes = [
        loaderTile(1, 'SMN_K1', { parts: ['PART-A'], bin: EMPTY, dedicated: true }),
        loaderTile(2, 'SMN_K2', { parts: ['PART-B'], bin: FULL('PART-B'), dedicated: true }),
    ];
    const serves = b => [row('CELL-K', 'PART-A', 40), row('CELL-K', 'PART-B', b)];
    setView(homes, { serves: serves(70) });
    render();
    const before = grid.children.slice();
    eq(before.map(n => n.className.split(' ')[0]), ['os-pc-top', 'os-pc-status', 'os-pc-zone'],
        'bands: the top line, the cells row, the Load section');
    setView(homes, { serves: serves(70) });
    ctx.renderGrid();
    eq(grid.children.map((n, i) => n === before[i]), [true, true, true], 'an unchanged view replaces no band');
    // PART-B is covered (its home holds a stocked bin): its count moves its bar
    // in the cells row and nothing else.
    setView(homes, { serves: serves(55) });
    ctx.renderGrid();
    eq(grid.children.map((n, i) => n === before[i]), [true, false, true], 'a moved count replaces the cells row only');
    await tap(pcCard('PART-A', 'SMN_K1'));
    eq(loadOpens, [{ node: 1, codes: ['PART-A'], cap: 0 }], 'a kept card still opens its act');
}

// ── 13. A buffer full of partials no home can take ────────────────────────
{
    // The section's header says NOW and why; "No partial can come home" is one
    // plain line under it, not in the header (a bug fixed: in the header it
    // pushed the strip off a 1024 px screen).
    const red = [1, 2, 3].map(i => ({ node: 'LDR.SMN_P' + i, state: 'partial' }));
    setView([loaderTile(1, 'SMN_H1', { parts: ['PART-A'], bin: FULL('PART-B'), dedicated: true, buffer: red })],
        { serves: [row('CELL-H', 'PART-A', 40)] });
    render();
    const z = zoneEl('Clear partials');
    const head = z.children.find(n => n.classList.contains('os-pc-zone-head'));
    eq([part(head, 'os-pc-word'), head.textContent.indexOf('No partial can come home'),
        z.children.filter(n => n !== head).map(n => n.textContent)],
    ['NOW', -1, ['No partial can come home']], 'no partial can come home: a plain line under the header');
}

if (failed) { console.error(passed + ' passed, ' + failed + ' FAILED'); process.exit(1); }
console.log('operator loader station: ' + passed + ' passed');
process.exit(0);
})().catch(function (err) { console.error(err && err.stack || err); process.exit(1); });
