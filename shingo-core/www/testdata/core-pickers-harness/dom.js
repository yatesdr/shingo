// The fake browser the Core picker pins run the pages in (pins.test.js, run by
// TestCorePickerPins in core_pickers_pins_test.go). Plain Node, no packages.
//
// A page runs UNMODIFIED but for its ES module syntax: each imported name is
// lent by the pin, and the helpers every page's markup and requests go
// through are the tree's own, evaluated inside the page's context, so a page's
// fetch is the harness's and a refusal is unwrapped exactly as the page sees it:
//   - apiGet, apiPost from static/app.js, and the HTTP door they answer
//     through (shared/http.js's send, its notice off): a refusal rejects an
//     ApiError whose string is the door's sentence;
//   - escapeHtml and h from shared/dom.js.
//
// The DOM is what the pages touch and no more: elements by id (made on first
// ask, as if the template had them), innerHTML kept as the string the page
// wrote, <option>s read back off it, classes, data-*, listeners recorded.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const WWW = path.join(__dirname, '..', '..');
const REPO = path.join(WWW, '..', '..');

function readWWW(rel) { return fs.readFileSync(path.join(WWW, rel), 'utf8'); }

// fnSource is one function's source, `export ` dropped: from its `function`
// to the brace that closes its body (the brace count Go's jsFunction uses).
function fnSource(src, name) {
    const re = new RegExp('(?:^|\\n)(?:export )?((?:async )?function ' + name + '\\()');
    const m = re.exec(src);
    if (!m) throw new Error('no function ' + name);
    const at = m.index + m[0].indexOf(m[1]);
    let depth = 0;
    for (let i = src.indexOf('{', at); i < src.length; i++) {
        if (src[i] === '{') depth++;
        if (src[i] === '}' && --depth === 0) return src.slice(at, i + 1);
    }
    throw new Error('unclosed function ' + name);
}

const sharedSrc = fs.readFileSync(path.join(REPO, 'shared', 'dom.js'), 'utf8');
const appSrc = readWWW('static/app.js');
// The door, its module syntax dropped and its private names kept inside a
// function so none can meet a page's own. The context has no AbortController,
// so the door arms no deadline timer here: the pins record the page's timers.
const httpSrc = fs.readFileSync(path.join(REPO, 'shared', 'http.js'), 'utf8')
    .replace(/^import [^;]+;[ \t]*$/mg, '')
    .replace(/^export (?=(?:async )?function |const )/mg, '');
const HELPERS = [
    sharedSrc.match(/^const HTML_ESCAPES = .*$/m)[0],
    fnSource(sharedSrc, 'escapeHtml'),
    fnSource(sharedSrc, 'h'),
    'var send = (function () {\n' + httpSrc + '\nreturn send;\n})();',
    fnSource(appSrc, 'apiError'),
    fnSource(appSrc, 'apiRequest'),
    fnSource(appSrc, 'apiGet'),
    fnSource(appSrc, 'apiPost'),
].join('\n');
const OWN = new Set(['apiGet', 'apiPost', 'escapeHtml', 'h', 'send']);

// --- elements ---------------------------------------------------------------

// matches answers the selector shapes the pages ask of an element's children:
// .cls, :scope > .cls, tag.cls.
function matches(e, sel) {
    const m = /^(?::scope > )?([a-z]*)\.([\w-]+)$/.exec(sel);
    return !!m && (!m[1] || e.tagName === m[1].toUpperCase()) && e.classList.contains(m[2]);
}

function makeEl(tag, id) {
    const classes = new Set();
    const attrs = {};
    let html = '';
    const e = {
        id: id || '', tagName: String(tag || 'div').toUpperCase(), type: '',
        value: '', checked: false, disabled: false, textContent: '',
        style: {}, dataset: {}, children: [], parentNode: null, listeners: {}, submitted: 0,
        // closestMap answers closest(sel) for the selectors a test sets up.
        closestMap: null, rect: null, qsa: null,
        classList: {
            add(...cs) { cs.forEach((c) => classes.add(c)); },
            remove(...cs) { cs.forEach((c) => classes.delete(c)); },
            contains(c) { return classes.has(c); },
            toggle(c, on) {
                if (on === undefined ? !classes.has(c) : on) classes.add(c); else classes.delete(c);
                return classes.has(c);
            },
        },
        get className() { return Array.from(classes).join(' '); },
        set className(v) { classes.clear(); String(v).split(/\s+/).filter(Boolean).forEach((c) => classes.add(c)); },
        setAttribute(k, v) { attrs[k] = String(v); },
        getAttribute(k) { return k in attrs ? attrs[k] : null; },
        removeAttribute(k) { delete attrs[k]; },
        addEventListener(t, fn) { (e.listeners[t] = e.listeners[t] || []).push(fn); },
        removeEventListener() {},
        appendChild(c) { c.parentNode = e; e.children.push(c); return c; },
        insertBefore(c) { c.parentNode = e; e.children.unshift(c); return c; },
        remove() {
            if (!e.parentNode) return;
            e.parentNode.children = e.parentNode.children.filter((x) => x !== e);
            e.parentNode = null;
        },
        insertAdjacentHTML(_pos, s) { html += s; },
        focus() {}, blur() {}, scrollIntoView() {},
        submit() { e.submitted++; },
        getBoundingClientRect() { return e.rect || { left: 0, top: 0, width: 0, height: 0 }; },
        closest(sel) { return e.closestMap && sel in e.closestMap ? e.closestMap[sel] : null; },
        querySelector(sel) { return e.querySelectorAll(sel)[0] || null; },
        querySelectorAll(sel) {
            if (e.qsa) { const r = e.qsa(sel); if (r) return r; }
            const out = [];
            (function walk(n, deep) {
                n.children.forEach((c) => { if (matches(c, sel)) out.push(c); if (deep) walk(c, true); });
            })(e, sel.indexOf(':scope >') !== 0);
            return out;
        },
        get innerHTML() { return html; },
        set innerHTML(v) { html = String(v); e.children = []; },
        get options() { return parseOptions(html); },
        get firstChild() { return e.children[0] || null; },
        get nextSibling() { return null; },
    };
    return e;
}

function makeDocument() {
    const byId = {};
    const doc = {
        readyState: 'loading', byId, listeners: [], activeElement: null, qsa: null,
        body: makeEl('body'),
        getElementById(id) { return byId[id] || (byId[id] = makeEl('div', id)); },
        createElement(tag) { return makeEl(tag); },
        addEventListener(type, fn, capture) { doc.listeners.push({ type, fn, capture: !!capture }); },
        removeEventListener() {},
        querySelector(sel) { return doc.querySelectorAll(sel)[0] || null; },
        querySelectorAll(sel) { return (doc.qsa && doc.qsa(sel)) || []; },
    };
    return doc;
}

// --- markup read back --------------------------------------------------------

function unesc(s) {
    return String(s).replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"')
        .replace(/&#39;/g, "'").replace(/&amp;/g, '&');
}
function attrOf(attrs, name) {
    const m = new RegExp('\\b' + name + '="([^"]*)"').exec(attrs);
    return m ? unesc(m[1]) : null;
}

// parseOptions reads a select's <option>s off its markup: [{value, text,
// group, selected, disabled}], group being the <optgroup> label around it.
function parseOptions(html) {
    const out = [];
    const re = /<optgroup label="([^"]*)">|<\/optgroup>|<option([^>]*)>([\s\S]*?)<\/option>/g;
    let m, group = '';
    while ((m = re.exec(html)) !== null) {
        if (m[1] !== undefined) { group = unesc(m[1]); continue; }
        if (m[0] === '</optgroup>') { group = ''; continue; }
        const text = unesc(m[3]);
        const v = attrOf(m[2], 'value');
        out.push({ value: v === null ? text : v, text, group,
            selected: /\bselected\b/.test(m[2]), disabled: /\bdisabled\b/.test(m[2]) });
    }
    return out;
}

// optionLines is a select's options as golden lines: `[group]` headings, then
// `"value" | label`, indented under a heading.
function optionLines(html, indent) {
    const pad = indent || '  ';
    const out = [];
    let group = null;
    parseOptions(html).forEach((o) => {
        if (o.group !== (group || '')) {
            group = o.group;
            if (group) out.push(pad + '[' + group + ']');
        }
        out.push(pad + (o.group ? '  ' : '') + JSON.stringify(o.value) + ' | ' + o.text +
            (o.selected ? '  (selected)' : '') + (o.disabled ? '  (disabled)' : ''));
    });
    return out;
}

// selectIn finds one <select id="..."> in a block of markup: {attrs, inner}.
function selectIn(html, id) {
    const m = new RegExp('<select([^>]*\\bid="' + id + '"[^>]*)>([\\s\\S]*?)</select>').exec(html);
    return m ? { attrs: m[1], inner: m[2] } : null;
}

// --- the network --------------------------------------------------------------

// makeNet is fetch, recorded. on(method, urlPrefix, answer) says what a
// request answers; the latest matching rule wins; a request nothing answers
// never settles. An answer is {status, text} or {json}, {reject: message}
// (fetch rejects, as it does when nothing answers the connection) or
// {hang: true}; it may be a function of (method, url, body).
function makeNet() {
    let rules = [];
    const net = {
        log: [],
        on(method, prefix, answer) { rules.unshift({ method, prefix, answer }); return net; },
        clear() { net.log.length = 0; },
        fetch(url, opts) {
            const method = (opts && opts.method) || 'GET';
            const body = opts && opts.body !== undefined ? String(opts.body) : null;
            url = String(url);
            net.log.push({ method, url, body });
            const r = rules.find((x) => (x.method === method || x.method === '*') && url.indexOf(x.prefix) === 0);
            let a = r ? r.answer : { hang: true };
            if (typeof a === 'function') a = a(method, url, body);
            if (a.hang) return new Promise(() => {});
            if (a.reject) return Promise.reject(new TypeError(a.reject));
            const status = a.status || 200;
            const text = a.text !== undefined ? a.text : JSON.stringify(a.json === undefined ? {} : a.json);
            return Promise.resolve({
                ok: status >= 200 && status < 300, status,
                text: () => Promise.resolve(text),
                json: () => Promise.resolve().then(() => JSON.parse(text)),
            });
        },
    };
    return net;
}

function requestLine(r) {
    return r.method + ' ' + r.url + (r.body !== null ? ' ' + r.body : '');
}

// normal is a printed value with Node's JSON.parse wording replaced: a
// browser words it differently, and the golden is the page's, not Node's.
function normal(s) {
    return String(s).replace(/SyntaxError: [^\n]*/g, 'SyntaxError: <JSON.parse message>');
}

// --- running a page -------------------------------------------------------------

// loadPage runs one page module in its own context over `globals`: imports
// lent by lend(name, from), exports left as the context's globals.
function loadPage(rel, globals, lend) {
    const file = path.join(WWW, rel);
    let src = fs.readFileSync(file, 'utf8');
    const ctx = vm.createContext(globals);
    vm.runInContext(HELPERS, ctx, { filename: 'core-pickers-harness/helpers' });
    // The adapters are the tree's own, in the page's context (loadAdapters);
    // the pickers a page mounts are kept by their host's id.
    const pickers = {};
    let adapters = null;
    src = src.replace(/^import \{([^}]*)\} from '([^']*)';[ \t]*$/mg, (_, names, from) => {
        // `confirm as uiConfirm` binds uiConfirm: lend by the name the page uses.
        names.split(',').map((n) => n.trim().split(/\s+as\s+/).pop()).filter(Boolean).forEach((n) => {
            if (OWN.has(n)) return;
            if (from === '/static/shared/node-picker.js' || from === '/static/shared/part-picker.js') {
                adapters = adapters || loadAdapters(ctx, pickers);
                if (!(n in adapters)) throw new Error(rel + ' imports ' + n + ' from ' + from + ', which does not export it');
                globals[n] = adapters[n];
                return;
            }
            const v = lend(n, from);
            if (v === undefined) throw new Error(rel + ' imports ' + n + ' from ' + from + '; the pin lends nothing for it');
            globals[n] = v;
        });
        return '';
    });
    src = src.replace(/^export (?=(?:async )?function |var |let |const )/mg, '');
    vm.runInContext(src, ctx, { filename: file });
    return { ctx, pickers, run: (expr) => vm.runInContext(expr, ctx) };
}

// --- the shared adapters, over a recorded shell ------------------------------

// The node and part adapters (shared/node-picker.js, part-picker.js) run as
// served, inside the page's context; the shell under them (shared/picker.js)
// is this one, which this DOM can hold: it keeps the selection, draws
// nothing, and answers choose(value) as a person picking a row would (a row
// not offered, or offered greyed, is not taken). The rows a page's picker
// offers are what the goldens print (rowLines). The shell's own behaviour is
// shared/testdata/components/picker.test.js's.
function moduleSource(file) {
    const names = [];
    const src = fs.readFileSync(path.join(REPO, 'shared', file), 'utf8')
        .replace(/^import [^;]+;[ \t]*$/mg, '')
        .replace(/^export (?:const|function|async function) ([A-Za-z_$][\w$]*)/mg, (m, n) => { names.push(n); return m.replace(/^export /, ''); });
    return { src, names };
}

function asList(v) {
    if (v === null || v === undefined || v === '') return [];
    return (Array.isArray(v) ? v : [v]).map(String);
}

function makeShell(registry) {
    return function createPicker(host, opts) {
        const o = opts || {};
        const multi = !!o.multi;
        let sel = asList(o.selected);
        if (!multi) sel = sel.slice(0, 1);
        const input = makeEl('input');
        const k = {
            el: host, input, opts: o, refreshes: 0,
            value: () => (multi ? sel.slice() : (sel[0] || '')),
            set(v) { sel = asList(v); if (!multi) sel = sel.slice(0, 1); },
            markClean() {}, refresh() { k.refreshes++; }, close() {}, focus() {}, destroy() {},
            open() { (input.listeners.focus || []).forEach((fn) => fn({ type: 'focus' })); },
            rows() { const r = typeof o.rows === 'function' ? o.rows() : o.rows; return r === undefined ? null : r; },
            // choose is a person picking value's row: '' why it was taken, or why not.
            choose(value) {
                const v = String(value);
                const rows = k.rows();
                if (v === '' && !multi && !(rows || []).some((r) => String(r.value) === '')) {
                    sel = [];
                } else {
                    const all = [];
                    (rows || []).forEach((r) => { all.push(r); (r.members || []).forEach((m) => all.push(m)); });
                    const r = all.find((x) => String(x.value) === v);
                    if (!r) return 'not offered';
                    if (r.reason) return 'offered greyed: ' + r.reason;
                    const vals = (Array.isArray(r.values) ? r.values : [r.value]).map(String);
                    if (multi) {
                        const on = vals.every((x) => sel.indexOf(x) >= 0);
                        sel = on ? sel.filter((x) => vals.indexOf(x) < 0) : sel.concat(vals.filter((x) => sel.indexOf(x) < 0));
                    } else {
                        sel = vals.slice(0, 1);
                    }
                }
                if (o.onChange) o.onChange(k.value(), k);
                return '';
            },
        };
        host.picker = k;
        if (registry) registry[host.id] = k;
        return k;
    };
}

// loadAdapters evaluates the two adapters in a page's context over the
// recorded shell; returns their exports by name.
function loadAdapters(ctx, registry) {
    const np = moduleSource('node-picker.js');
    const pp = moduleSource('part-picker.js');
    const make = vm.runInContext('(function (createPicker, el) {\n' + np.src + '\nreturn {' + np.names.join(',') + '};\n})', ctx,
        { filename: 'shared/node-picker.js' });
    const lend = {};
    lend.el = function (tag, props) {
        const e = makeEl(tag);
        Object.keys(props || {}).forEach((key) => {
            if (key === 'className') e.className = props[key];
            else { e.setAttribute(key, props[key]); if (key === 'type') e.type = props[key]; }
        });
        return e;
    };
    const N = make(makeShell(registry), lend.el);
    const makeP = vm.runInContext('(function (mountPicker, pickerSource) {\n' + pp.src + '\nreturn {' + pp.names.join(',') + '};\n})', ctx,
        { filename: 'shared/part-picker.js' });
    const P = makeP(N.mountPicker, N.pickerSource);
    return Object.assign({}, N, P);
}

// rowLines is a picker's rows as golden lines: "[section]" headings, then
// "value" | label · note, a greyed one with (off: reason), members indented.
function rowLines(rows, indent) {
    const pad = indent || '  ';
    if (rows === null) return [pad + '(not read yet)'];
    const out = [];
    let section;
    const one = (r, p) => out.push(p + JSON.stringify(String(r.value)) + ' | ' + (r.label === undefined ? r.value : r.label) +
        (r.note ? ' · ' + r.note : '') + (r.reason ? '  (off: ' + r.reason + ')' : ''));
    rows.forEach((r) => {
        if (r.section !== undefined && r.section !== section) { section = r.section; out.push(pad + '[' + section + ']'); }
        const p = pad + (r.section !== undefined ? '  ' : '');
        one(r, p);
        (r.members || []).forEach((m) => one(m, p + '  '));
    });
    if (!rows.length) out.push(pad + '(no rows)');
    return out;
}

function tick() { return new Promise((r) => setImmediate(r)); }
async function settle() { for (let i = 0; i < 25; i++) await tick(); }

module.exports = {
    readWWW, makeEl, makeDocument, parseOptions, optionLines, selectIn,
    makeNet, requestLine, normal, loadPage, settle, unesc, loadAdapters, rowLines,
};
