// pins.js — the node and part pickers on the Manual order, Production and
// Kafka Direct pages, characterised (census rows 38–44, E14–E19).
//
// Run by www/edge_pages_pins_test.go with one argument: a JSON file holding
// the three pages as their handlers rendered them, the SSE `core-nodes` frame
// the engine would send for the same plant, and the answers the harness's
// fetch replays (each from the handler or writer that makes it). It prints ONE
// JSON object, golden file name -> text, and nothing else on stdout.
//
// WHAT IS REAL. The page markup (sliced from <main> to the page's module
// tag), each page module byte for byte less its import/export lines, and the
// helpers they import: shingoedge.js's api (over shared/http.js, read whole);
// shared/layer.js's showModal and hideModal (read whole); shared/dom.js's
// escapeHtml; ask-act.js's postAct; and shingoedge.js's refreshOnEvents. Each
// is read off its own file.
//
// The node and part adapters (shared/node-picker.js, part-picker.js) are real
// too, read whole: each field's rows, rules and reasons are theirs.
//
// WHAT IS STUBBED, AND WHY EACH IS SAFE FOR THESE PINS.
//   - The picker's shell (shared/picker.js createPicker): a fake that keeps
//     the selection and draws nothing. The pins read the rows the adapter
//     hands it, and pick(value) moves the selection as the shell would: a
//     greyed row cannot be picked, a single pick replaces, onChange fires.
//     The shell's own behaviour is its spec's (TestSharedComponentSpecs).
//   - The DOM (dom.js): elements parsed from the markup, selects answering
//     value / selectedIndex as a browser does.
//   - toast: recorded as [kind, text]; the pins read what the page said.
//   - onSSE (shared/bus.js): each subscription is kept by type, and a pin
//     fires core-nodes with the frame the engine sends
//     (CoreNodesUpdatedEvent).
//   - delegateActions: its handler map is kept, and a pin calls a handler
//     with `this` set to the element the click would have been on.
//   - fetch: every call is recorded (method, url, body) and answered by
//     the next scripted answer, else by its route; an unrouted call is a 404.
//     'reject' is a fetch that rejects (TypeError: Failed to fetch).
//   - Date.now: a clock the pins move, so postAct's hold can be crossed.
//   - crypto.randomUUID: one fixed uuid, so a golden never flaps.
//   - The production chart's <canvas> is dropped from the markup: the chart
//     is not a picker, and renderShiftChart returns without it.
//
// EACH SCENARIO IS ITS OWN BOOT, in its own vm context, so a held ask, a
// cached catalog or a reset form never leaks from one into the next.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const { Document } = require('./dom.js');

const WWW = path.join(__dirname, '..', '..');
const JS = path.join(WWW, 'static', 'js');
const SHARED = path.join(WWW, '..', '..', 'shared');
const read = p => fs.readFileSync(p, 'utf8');
const IN = JSON.parse(read(process.argv[2]));
const A = IN.answers;
const out = {};

// ── the helpers the pages import, lifted off their files ─────────────────────

// block returns the source from the match of re to the brace that closes the
// first brace after it (and a `;` straight after, for `const x = {…};`).
function block(src, re, what) {
    const m = re.exec(src);
    if (!m) throw new Error('no ' + what);
    let i = src.indexOf('{', m.index), depth = 0;
    for (; i < src.length; i++) {
        if (src[i] === '{') depth++;
        else if (src[i] === '}' && --depth === 0) break;
    }
    if (src[i + 1] === ';') i++;
    return src.slice(m.index, i + 1).replace(/^export\s+/, '');
}
// unModule runs a module as a script: imports dropped (the names they bind are
// the context's globals), and an exported const or let made a var, so a pin
// can read it off the context (postAct's ASK_HOLD_MS).
const unModule = src => src
    .replace(/^import[\s\S]*?from\s+['"][^'"]+['"];?[ \t]*$/gm, '')
    .replace(/^export\s+(const|let)\s+/gm, 'var ')
    .replace(/^export\s+(?=(async\s+)?function|var)/gm, '')
    .replace(/^export\s*\{[^}]*\}(\s*from\s*['"][^'"]+['"])?;?/gm, '');

const EDGE = read(path.join(JS, 'shingoedge.js'));
const SHARED_DOM = read(path.join(SHARED, 'dom.js'));
// shared/http.js whole, in a function of its own so its private names stay
// its own; its notice is the context's toast (the hub's api turns it off).
const HTTP = 'var http = (function () {\n' + unModule(read(path.join(SHARED, 'http.js'))) + '\nreturn http;\n})();';
// shared/layer.js whole, likewise: the pages' showModal and hideModal are its
// (one overlay stack: focus in and back, a counted scroll lock, fields back to
// their markup on close). The el it builds its dialogs with is dom.js's.
const LAYER = 'var { showModal, hideModal } = (function () {\n'
    + block(SHARED_DOM, /export function el\(/, 'el') + '\n'
    + block(SHARED_DOM, /function appendChildren\(/, 'appendChildren') + '\n'
    + unModule(read(path.join(SHARED, 'layer.js'))) + '\nreturn { showModal, hideModal };\n})();';
// The picker's shell, faked (see above): PICKERS is each one by its host's id.
const FAKE_PICKER = `
var PICKERS = {};
function pickList(v) { return v === null || v === undefined || v === '' ? [] : (Array.isArray(v) ? v : [v]).map(String); }
function createPicker(host, o) {
    var listeners = {};
    var st = { sel: pickList(o.selected) };
    var input = { value: '', addEventListener: function (t, fn) { (listeners[t] = listeners[t] || []).push(fn); } };
    function fire(t) { (listeners[t] || []).forEach(function (fn) { fn(); }); }
    var p = {
        el: host, input: input, opts: o, refreshes: 0, fire: fire,
        value: function () { return o.multi ? st.sel.slice() : (st.sel[0] || ''); },
        set: function (v) { st.sel = pickList(v); if (!o.multi) st.sel = st.sel.slice(0, 1); },
        markClean: function () {}, refresh: function () { p.refreshes++; },
        open: function () { fire('focus'); }, focus: function () { fire('focus'); }, close: function () {}, destroy: function () {},
        rows: function () { var r = typeof o.rows === 'function' ? o.rows() : o.rows; return r === null || r === undefined ? null : r; },
        // pick is a click on the row of that value: '' when taken, else why not.
        pick: function (value) {
            var all = [];
            (p.rows() || []).forEach(function (r) { all.push(r); (r.members || []).forEach(function (m) { all.push(m); }); });
            var r = all.find(function (x) { return String(x.value) === String(value); });
            if (!r) return 'no row ' + value;
            if (r.reason) return 'greyed: ' + r.reason;
            var vals = (Array.isArray(r.values) ? r.values : [r.value]).map(String);
            if (o.multi) {
                var on = vals.every(function (v) { return st.sel.indexOf(v) >= 0; });
                st.sel = on ? st.sel.filter(function (v) { return vals.indexOf(v) < 0; })
                    : st.sel.concat(vals.filter(function (v) { return st.sel.indexOf(v) < 0; }));
            } else {
                st.sel = vals.slice(0, 1);
            }
            if (o.onChange) o.onChange(p.value(), p);
            return '';
        },
    };
    host.textContent = '';
    PICKERS[host.id] = p;
    return p;
}
`;
// The adapters, whole, each in a function of its own; createPicker is the
// fake above, and el is dom.js's.
const ADAPTERS = 'var { nodeSource, nodePicker, nodeWhy, nodeRows, pickerSource, mountPicker } = (function () {\n'
    + block(SHARED_DOM, /export function el\(/, 'el') + '\n'
    + block(SHARED_DOM, /function appendChildren\(/, 'appendChildren') + '\n'
    + unModule(read(path.join(SHARED, 'node-picker.js')))
    + '\nreturn { nodeSource, nodePicker, nodeWhy, nodeRows, pickerSource, mountPicker };\n})();\n'
    + 'var { partSource, partPicker, partChoices } = (function () {\n'
    + unModule(read(path.join(SHARED, 'part-picker.js')))
    + '\nreturn { partSource, partPicker, partChoices };\n})();';
const HELPERS = [
    block(SHARED_DOM, /const HTML_ESCAPES = /, 'HTML_ESCAPES').replace(/^const /, 'var '),
    block(SHARED_DOM, /export function escapeHtml\(/, 'escapeHtml'),
    HTTP,
    block(EDGE, /const QUIET = /, 'QUIET').replace(/^const /, 'var '),
    block(EDGE, /function answered\(/, 'answered'),
    block(EDGE, /export const api = /, 'api').replace(/^const /, 'var '),
    LAYER,
    block(EDGE, /export function refreshOnEvents\(/, 'refreshOnEvents'),
    block(EDGE, /function sseTriggerTypes\(/, 'sseTriggerTypes'),
    unModule(read(path.join(JS, 'ask-act.js'))),
    FAKE_PICKER,
    ADAPTERS,
].join('\n');

// pageMarkup is the page's own markup: from <main> to its import map, which
// precedes its module entries, less the htmx tag the three swap pages carry.
function pageMarkup(html) {
    const i = html.indexOf('<main');
    const j = html.indexOf('<script type="importmap">');
    if (i < 0 || j < i) throw new Error('page markup not found');
    return html.slice(html.indexOf('>', i) + 1, j).replace(/<script src="[^"]*\/htmx\.[^"]*"><\/script>/g, '').replace(/<canvas\b[\s\S]*?<\/canvas>/g, '');
}

// ── one boot ─────────────────────────────────────────────────────────────────

function routeOf(method, url) {
    if (method === 'GET' && url === '/api/payload-catalog') return A.catalog;
    if (method === 'GET' && url === '/api/core-nodes') return A['core-nodes'];
    let m = /^\/api\/payload\/([^/]+)\/manifest$/.exec(url);
    if (method === 'GET' && m) return A['manifest:' + decodeURIComponent(m[1])];
    if (method === 'POST' && /^\/api\/orders\/(retrieve|move|complex)$/.test(url)) return A['order-ok'];
    if (method === 'POST' && /^\/api\/process-nodes\/\d+\/(request-empty|load-bin)$/.test(url)) return A['act-ok'];
    if (method === 'POST' && url === '/api/manual-message') return A['message-ok'];
    return null;
}

function boot(page) {
    const doc = new Document(pageMarkup(IN.pages[page]));
    const rec = { requests: [], toasts: [], next: [], now: Date.UTC(2026, 10, 11, 12, 0, 0), sse: {}, actions: null };
    const fetch = async (url, opts) => {
        opts = opts || {};
        const method = opts.method || 'GET';
        rec.requests.push({ method, url, body: opts.body === undefined ? undefined : JSON.parse(opts.body) });
        let a = rec.next.length ? rec.next.shift() : routeOf(method, url);
        if (a === 'reject') throw new TypeError('Failed to fetch');
        if (!a) a = { status: 404, body: '404 page not found\n' };
        const res = {
            ok: a.status >= 200 && a.status < 300, status: a.status,
            json: async () => JSON.parse(a.body),
            text: async () => a.body,
            clone() { return res; },
        };
        return res;
    };
    const RealDate = Date;
    class FakeDate extends RealDate {
        constructor(...a) { super(...(a.length ? a : [rec.now])); }
        static now() { return rec.now; }
    }
    const ctx = vm.createContext({
        console, JSON, Math, Object, Array, String, Number, Boolean, Promise, Map, Set, Error, TypeError, RegExp,
        parseInt, parseFloat, isNaN, encodeURIComponent, decodeURIComponent,
        Date: FakeDate,
        document: doc,
        window: { location: { reload() { rec.toasts.push(['reload']); } }, devicePixelRatio: 1, htmx: null },
        fetch,
        crypto: { randomUUID: () => '00000000-0000-4000-8000-0000000000aa' },
        setTimeout: () => 0, clearTimeout() {},
        toast: (msg, kind) => { rec.toasts.push([kind || 'info', String(msg)]); },
        onSSE: (type, fn) => { (rec.sse[type] = rec.sse[type] || []).push(fn); return () => {}; },
        delegateActions: (root, map) => { rec.actions = map; },
        prompt: async () => null,
        navigateToProcess() {},
    });
    vm.runInContext(HELPERS, ctx, { filename: 'helpers' });
    vm.runInContext(unModule(read(path.join(JS, 'pages', page + '.js'))), ctx, { filename: page + '.js' });
    return { doc, rec, ctx, page };
}
// fireSSE is a frame of the type on the page's one stream: every handler the
// page subscribed for it, in order, as shared/bus.js calls them.
const fireSSE = (b, type, data) => (b.rec.sse[type] || []).forEach(fn => fn(data, { type }));
const settle = () => new Promise(r => setImmediate(r)).then(() => new Promise(r => setImmediate(r)));

// ── reading the page ─────────────────────────────────────────────────────────

// show writes a string the way a golden can hold it: JSON-quoted, with the
// non-breaking space spelled, since a golden reader cannot see it.
const show = s => JSON.stringify(String(s)).replace(/ /g, '\\u00a0');

function labelOf(el) {
    const group = el.parentNode;
    const lab = group && group.children.find(n => n.tagName === 'LABEL');
    return lab ? lab.textContent.trim() : '';
}
function shown(el) {
    for (let n = el; n && n.tagName !== 'BODY'; n = n.parentNode) if (n.style.display === 'none') return false;
    return true;
}
// options is one select as lines: its id, label and whether it shows, then
// each option's value, label, the data-* it carries, and the selected one.
function options(b, id, note) {
    const sel = b.doc.getElementById(id);
    if (b.ctx.PICKERS[id]) return pickerLines(b, id, note);
    const lines = ['#' + id + ' (' + labelOf(sel) + ')' + (shown(sel) ? '' : ' [hidden]') + (note ? ' — ' + note : '') +
        ' selectedIndex=' + sel.selectedIndex];
    sel.options.forEach((o, i) => {
        const data = Object.keys(o.dataset).sort().map(k => ' ' + k + '=' + show(o.dataset[k])).join('');
        lines.push('  ' + (i === sel.selectedIndex ? '*' : ' ') + ' value=' + show(o.value) + ' label=' + show(o.textContent) + data);
    });
    return lines.join('\n');
}
// pickerLines is one picker as lines: its id, label, whether it shows and its
// value, then each row the adapter handed it, members indented: value, label,
// note, and why a greyed one cannot be picked.
function pickerLines(b, id, note) {
    const p = b.ctx.PICKERS[id];
    const el = b.doc.getElementById(id);
    const lines = ['#' + id + ' (' + labelOf(el) + ')' + (shown(el) ? '' : ' [hidden]') + (note ? ' — ' + note : '') +
        ' picker value=' + show(p.value())];
    const rows = p.rows();
    if (rows === null) return lines.concat('  (not read: ' + show(p.opts.loading) + ')').join('\n');
    const on = v => [].concat(p.value()).indexOf(String(v)) >= 0;
    const one = (r, pad) => lines.push('  ' + (on(r.value) ? '*' : ' ') + pad + ' value=' + show(r.value) +
        (r.label !== undefined && r.label !== r.value ? ' label=' + show(r.label) : '') +
        (r.note ? ' note=' + show(r.note) : '') + (r.reason ? ' greyed=' + show(r.reason) : ''));
    rows.forEach(r => { one(r, ''); (r.members || []).forEach(m => one(m, '   ')); });
    return lines.join('\n');
}
function valueOf(b, id) {
    const p = b.ctx.PICKERS[id];
    return p ? p.value() : b.doc.getElementById(id).value;
}
function form(b, ids) {
    return ids.map(id => {
        const el = b.doc.getElementById(id);
        return id + '=' + show(valueOf(b, id)) + (el.tagName === 'SELECT' || b.ctx.PICKERS[id] || shown(el) ? '' : '[hidden]');
    }).join(' ');
}
function requests(b) {
    const lines = b.rec.requests.map(r => r.method + ' ' + r.url + (r.body === undefined ? '' : ' ' + JSON.stringify(r.body)));
    b.rec.requests = [];
    return lines.length ? lines : ['(no request)'];
}
function toasts(b) {
    const lines = b.rec.toasts.map(([k, m]) => k === 'reload' ? 'page reload' : 'toast ' + k + ': ' + show(m));
    b.rec.toasts = [];
    return lines.length ? lines : ['(nothing shown)'];
}
function answerLine(a) {
    if (a === 'reject') return 'answer: fetch rejects (TypeError: Failed to fetch)';
    return 'answer: ' + a.status + ' ' + (a.body === '' ? '(empty body)' : a.body.trim());
}

// ── the manual-order form (Manual order page and Production's copy) ──────────

const MO = ['mo-type', 'mo-node', 'mo-qty', 'mo-pickup', 'mo-delivery', 'mo-staging'];
function pick(b, id, value) {
    const p = b.ctx.PICKERS[id];
    if (p) {
        const why = p.pick(value);
        if (why) throw new Error(b.page + ': ' + id + ' cannot pick ' + value + ' (' + why + ')');
        return;
    }
    const el = b.doc.getElementById(id);
    el.value = value;
    if (el.value !== String(value)) throw new Error(b.page + ': ' + id + ' has no option ' + value);
    if (id === 'mo-type') b.rec.actions.updateOrderForm.call(el);
    if (id === 'mo-node') b.rec.actions.autofillNodeDefaults.call(el);
}

async function manualOrderOptions(page) {
    const b = boot(page);
    const lines = ['# ' + page + ': the manual-order form\'s node pickers at load (type Move)', ''];
    MO.filter(id => b.doc.getElementById(id).tagName === 'SELECT' || b.ctx.PICKERS[id]).forEach(id => lines.push(options(b, id), ''));
    // Every type, for which pickers it shows and what each is called.
    ['retrieve', 'complex'].forEach(t => {
        pick(b, 'mo-type', t);
        lines.push('type ' + t + ': ' + ['mo-node', 'mo-pickup', 'mo-delivery', 'mo-staging']
            .map(id => id + '(' + labelOf(b.doc.getElementById(id)) + ')' + (shown(b.doc.getElementById(id)) ? '' : '[hidden]')).join(' '));
    });
    lines.push('');

    // THE SSE FRAME (census bug 8 was its rebuild dropping the group/lane
    // tag, the indent and the description, and offering members to the
    // pickup): a `core-nodes` frame names nodes and types, not groups, so the
    // form reads the node list again (/api/core-nodes) and every field draws
    // from it as at load. The pick is kept while its node is on the list.
    const b2 = boot(page);
    pick(b2, 'mo-type', 'complex');
    pick(b2, 'mo-delivery', 'GRP-B.SLOT-A1');
    fireSSE(b2, 'core-nodes', IN.coreNodesEvent);
    await settle();
    lines.push('# after an SSE core-nodes frame for the same plant (type Complex, Core Node had GRP-B.SLOT-A1 picked)', '',
        'on the frame: ' + requests(b2).join(' '), '');
    ['mo-pickup', 'mo-delivery'].forEach(id => lines.push(options(b2, id), ''));
    // A second frame, the list now without the picked node: it is unpicked.
    const without = JSON.parse(A['core-nodes'].body).filter(n => n.name !== 'GRP-B.SLOT-A1')
        .map(n => Object.assign({}, n, n.members ? { members: n.members.filter(m => m !== 'GRP-B.SLOT-A1') } : {}));
    b2.rec.next = [{ status: 200, body: JSON.stringify(without) }];
    fireSSE(b2, 'core-nodes', { nodes: IN.coreNodesEvent.nodes.filter(n => n.name !== 'GRP-B.SLOT-A1') });
    await settle();
    lines.push('# after a second frame, the list without GRP-B.SLOT-A1 (the picked node)', '',
        'on the frame: ' + requests(b2).join(' '), '', options(b2, 'mo-delivery'), '');
    out[page + '-options.txt'] = lines.join('\n');
}

// One scenario: picks, then Create Order, under the given answers.
async function createScenario(page, title, picks, answers, then) {
    const b = boot(page);
    picks.forEach(([id, v]) => pick(b, id, v));
    const lines = ['## ' + title, 'picks: ' + form(b, MO)];
    b.rec.next = answers.slice();
    await b.rec.actions.createOrder();
    await settle();
    lines.push(...requests(b));
    answers.forEach(a => lines.push(answerLine(a)));
    lines.push('shows: ' + toasts(b).join(' | '));
    lines.push('form after: ' + form(b, MO));
    if (then) lines.push(...(await then(b)));
    lines.push('');
    return lines.join('\n');
}

async function manualOrderRequests(page) {
    const L = ['# ' + page + ': what Create Order sends for each pick, and each answer', ''];
    const move = [['mo-type', 'move'], ['mo-pickup', 'DOCK-1'], ['mo-delivery', 'PRESS-1'], ['mo-qty', '2']];
    const retrieve = [['mo-type', 'retrieve'], ['mo-node', '2'], ['mo-staging', 'GRP-A']];
    const complex = [['mo-type', 'complex'], ['mo-staging', 'LANE-1'], ['mo-delivery', 'PRESS-1']];
    L.push(await createScenario(page, 'move: DOCK-1 to PRESS-1, qty 2', move, [A['order-ok']]));
    L.push(await createScenario(page, 'move with nothing picked: sent as is (the page checks nothing)', [['mo-type', 'move']], [A['order-ok']]));
    L.push(await createScenario(page, 'move refused (apiCreateMoveOrder: an engine refusal is a 500)', move, [A['move-500']]));
    L.push(await createScenario(page, 'retrieve: process node "Slot one" (Delivery autofilled from it), staging GRP-A', retrieve, [A['order-ok']]));
    L.push(await createScenario(page, 'retrieve: a node group picked as Delivery, no process node', [['mo-type', 'retrieve'], ['mo-delivery', 'GRP-A']], [A['order-ok']]));
    L.push(await createScenario(page, 'retrieve refused: Core made none at the loader (409)', retrieve, [A['retrieve-refused']]));
    L.push(await createScenario(page, 'retrieve pending: 202, then Create again inside the hold', retrieve, [A.pending], async b => {
        pick(b, 'mo-type', 'retrieve'); pick(b, 'mo-node', '2');
        await b.rec.actions.createOrder(); await settle();
        const again = ['again (same act, inside ASK_HOLD_MS): ' + requests(b).join(' '), 'shows: ' + toasts(b).join(' | ')];
        b.rec.now += b.ctx.ASK_HOLD_MS + 1;
        pick(b, 'mo-type', 'retrieve'); pick(b, 'mo-node', '2');
        b.rec.next = [A['order-ok']];
        await b.rec.actions.createOrder(); await settle();
        again.push('past the hold: ' + requests(b).join(' '), 'shows: ' + toasts(b).join(' | '));
        return again;
    }));
    L.push(await createScenario(page, 'retrieve 500 (writeError)', retrieve, [A['retrieve-500']]));
    L.push(await createScenario(page, 'retrieve 500 from a panic (Recoverer: empty body)', retrieve, [A.panic]));
    L.push(await createScenario(page, 'retrieve, fetch rejects', retrieve, ['reject']));
    L.push(await createScenario(page, 'complex: staging LANE-1, Core Node PRESS-1', complex, [A['order-ok']]));
    L.push(await createScenario(page, 'complex with no staging: refused on the page', [['mo-type', 'complex'], ['mo-delivery', 'PRESS-1']], []));
    L.push(await createScenario(page, 'complex refused (400)', complex, [A['complex-refused']]));
    L.push(await createScenario(page, 'complex, fetch rejects', complex, ['reject']));
    out[page + '-requests.txt'] = L.join('\n');
}

// ── Production's two part pickers (census 42, 43) ────────────────────────────

// button is the element the template draws for the act
// (partials/production-body.html): data-node-id and data-allowed-payloads as
// `{{json .ActiveClaim.AllowedPayloads}}` writes them. The stub engine has no
// loader, and only a loader's synthesised claim draws these buttons
// (Loader.SynthClaim), so the element is built with the template's attributes.
function button(b, nodeID, allowed, extra) {
    const el = b.doc.createElement('button');
    el.setAttribute('data-node-id', String(nodeID));
    el.setAttribute('data-allowed-payloads', JSON.stringify(allowed));
    Object.keys(extra || {}).forEach(k => el.setAttribute(k, extra[k]));
    return el;
}
const active = (b, id) => b.doc.getElementById(id).classList.contains('active') ? 'open' : 'closed';

async function requestEmpty(title, allowed, choose, answers, then) {
    const b = boot('production');
    b.rec.actions.openRequestEmptyModal.call(button(b, 2, allowed));
    const lines = ['## ' + title, 'button data-allowed-payloads=' + show(JSON.stringify(allowed)),
        options(b, 're-payload', 'modal ' + active(b, 'request-empty-modal'))];
    if (choose) { b.doc.getElementById('re-payload').value = choose; lines.push('picked ' + show(choose)); }
    b.rec.next = answers.slice();
    await b.rec.actions.submitRequestEmpty();
    await settle();
    lines.push(...requests(b));
    answers.forEach(a => lines.push(answerLine(a)));
    lines.push('shows: ' + toasts(b).join(' | '), 'modal ' + active(b, 'request-empty-modal') +
        ', re-payload=' + show(b.doc.getElementById('re-payload').value));
    if (then) lines.push(...(await then(b)));
    lines.push('');
    return lines.join('\n');
}

async function loadBin(title, allowed, choose, answers, opts) {
    opts = opts || {};
    const b = boot('production');
    if (opts.catalogFails) b.rec.next.push(A['retrieve-500']);
    await b.rec.actions.openLoadBinModal.call(button(b, 2, allowed, { 'data-uop-capacity': '24' }));
    await settle();
    const lines = ['## ' + title, 'button data-allowed-payloads=' + show(JSON.stringify(allowed)) + ' data-uop-capacity="24"',
        'on open: ' + requests(b).join(' ') + (opts.catalogFails ? ' (answered 500)' : ''),
        options(b, 'rb-payload', 'modal ' + active(b, 'load-bin-modal'))];
    if (opts.reopen) {
        await b.rec.actions.openLoadBinModal.call(button(b, 2, allowed, { 'data-uop-capacity': '24' }));
        await settle();
        lines.push('opened again: ' + requests(b).join(' '), options(b, 'rb-payload'));
    }
    if (choose) {
        const sel = b.doc.getElementById('rb-payload');
        sel.value = choose;
        await b.rec.actions.onLoadPayloadChanged.call(sel);
        await settle();
        lines.push('picked ' + show(choose) + ': ' + requests(b).join(' '));
        const uop = b.doc.getElementById('rb-uop-count');
        lines.push('  rows: UoP count=' + (uop ? show(uop.value) : '(none)') + ' ' +
            b.doc.querySelectorAll('.rb-manifest-qty').map(i => i.dataset.part + '=' + i.value + ' ' + show(i.dataset.desc)).join(', '));
    }
    b.rec.next = answers.slice();
    await b.rec.actions.submitLoadBin();
    await settle();
    lines.push(...requests(b));
    answers.forEach(a => lines.push(answerLine(a)));
    lines.push('shows: ' + toasts(b).join(' | '), 'modal ' + active(b, 'load-bin-modal') +
        ', rb-payload=' + show(b.doc.getElementById('rb-payload').value));
    lines.push('');
    return lines.join('\n');
}

async function productionPartPickers() {
    const many = ['PART-B', 'PART-A', 'PART-C'];
    const L = ['# production: Request empty ("compatible with") and Load bin part pickers', ''];
    // "COMPATIBLE WITH" SENDS ITS FIRST ALLOWED PART BY DEFAULT: the select
    // has no placeholder, so the first code is the pick unless the person
    // changes it (pinned as is; the brief's REQUEST EMPTY ruling reads it).
    L.push(await requestEmpty('request empty, three allowed, left as opened', many, null, [A['act-ok']]));
    L.push(await requestEmpty('request empty, three allowed, the second picked', many, 'PART-A', [A['act-ok']]));
    L.push(await requestEmpty('request empty, one allowed', ['PART-A'], null, [A['act-ok']]));
    L.push(await requestEmpty('request empty, none allowed (the template writes [])', [], null, []));
    L.push(await requestEmpty('request empty, none allowed (the template writes null for a nil list)', null, null, []));
    L.push(await requestEmpty('request empty refused (apiRequestEmptyBin: 400)', many, null, [A['request-empty-refused']]));
    L.push(await requestEmpty('request empty pending (202), then Request again inside the hold', many, 'PART-A', [A.pending], async b => {
        b.rec.actions.openRequestEmptyModal.call(button(b, 2, many));
        await b.rec.actions.submitRequestEmpty(); await settle();
        return ['again inside the hold (re-opened, left on its first part): ' + requests(b).join(' '), 'shows: ' + toasts(b).join(' | ')];
    }));
    L.push(await requestEmpty('request empty 500 from a panic (Recoverer: empty body)', many, null, [A.panic]));
    L.push(await requestEmpty('request empty, fetch rejects', many, null, ['reject']));

    L.push(await loadBin('load bin, three allowed, PART-A picked', many, 'PART-A', [A['act-ok']], { reopen: true }));
    L.push(await loadBin('load bin, PART-B picked (its catalog row has no manifest lines)', many, 'PART-B', [A['act-ok']]));
    L.push(await loadBin('load bin, nothing picked', many, null, []));
    L.push(await loadBin('load bin, the catalog read fails (500): codes alone, and the next open reads it again', many, 'PART-A', [A['act-ok']],
        { catalogFails: true, reopen: true }));
    L.push(await loadBin('load bin refused (apiLoadBin: 400)', many, 'PART-A', [A['load-bin-refused']]));
    L.push(await loadBin('load bin pending (202)', many, 'PART-A', [A.pending]));
    L.push(await loadBin('load bin, fetch rejects', many, 'PART-A', ['reject']));
    out['production-part-pickers.txt'] = L.join('\n');
}

// ── Kafka Direct (census 44, bugs 9 and 10) ──────────────────────────────────

// fieldsMarkup is #mm-fields' markup as the page wrote it, one form-group a
// line. ESCAPED (census bug 9, fixed): renderField escapes node names, the
// field values and the order uuid/type/status, so the name `NODE <b>&"'` and
// the uuid `ab<b>&"'-0001` show as text.
function fieldsMarkup(b) {
    return b.doc.getElementById('mm-fields').innerHTML.split('<div class="form-group">').filter(Boolean)
        .map(s => '  <div class="form-group">' + s);
}

async function kafkaDirect() {
    const L = ['# manual-message (Kafka Direct): the message forms, their node pickers, and what Send sends', ''];
    const b = boot('manual-message');
    const type = b.doc.getElementById('mm-type');
    L.push('#mm-type: ' + type.options.map(o => show(o.value)).join(' ') + ' (at load ' + show(type.value) + ')', '');
    for (const t of ['order.request', 'order.redirect', 'order.cancel']) {
        type.value = t;
        b.rec.actions.updateMessageForm.call(type);
        L.push('## ' + t + ': #mm-fields markup', ...fieldsMarkup(b), '');
    }
    L.push('');

    // order.request CANNOT NAME A PART (census bug 10, pinned as is): the form
    // has Payload Description (text) and no payload code, and the body has no
    // payload_code key, though protocol.OrderRequest carries one.
    const send = async (title, picks, answers, withPreview) => {
        const s = boot('manual-message');
        const ty = s.doc.getElementById('mm-type');
        ty.value = 'order.request';
        s.rec.actions.updateMessageForm.call(ty);
        picks.forEach(([id, v]) => {
            const el = s.doc.getElementById('mm-f-' + id);
            if (el.type === 'checkbox') el.checked = v; else el.value = v;
            if (el.tagName === 'SELECT' && el.value !== v) throw new Error('mm-f-' + id + ' has no option ' + v);
        });
        s.rec.actions.updatePreview();
        const lines = ['## ' + title];
        if (withPreview) lines.push('preview:', ...s.doc.getElementById('mm-preview').textContent.split('\n').map(l => '  ' + l));
        s.rec.next = answers.slice();
        await s.rec.actions.sendMessage();
        await settle();
        lines.push(...requests(s));
        answers.forEach(a => lines.push(answerLine(a)));
        const st = s.doc.getElementById('mm-status');
        lines.push('shows: ' + toasts(s).join(' | '), 'status: ' + show(st.textContent) + ' color=' + show(st.style.color), '');
        return lines.join('\n');
    };
    const picks = [['delivery_node', 'SLOT-A1'], ['source_node', 'GRP-A'], ['payload_desc', 'PART-A'], ['quantity', '2'], ['retrieve_empty', true]];
    L.push(await send('order.request: delivery SLOT-A1, source GRP-A, description "PART-A", qty 2, retrieve empty', picks, [A['message-ok']], true));
    L.push(await send('order.request refused (apiSendManualMessage: 400)', picks, [A['message-refused']]));
    L.push(await send('order.request, send failed (500)', picks, [A['message-500']]));
    L.push(await send('order.request, fetch rejects', picks, ['reject']));
    out['manual-message.txt'] = L.join('\n');
}

(async () => {
    await manualOrderOptions('manual-order');
    await manualOrderRequests('manual-order');
    await manualOrderOptions('production');
    await manualOrderRequests('production');
    await productionPartPickers();
    await kafkaDirect();
    process.stdout.write(JSON.stringify(out));
})().catch(err => { console.error(err && err.stack || err); process.exit(1); });
