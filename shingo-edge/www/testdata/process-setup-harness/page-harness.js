// page-harness.js — the Processes page module (processes-desktop.js), booted
// under node with every module-scope binding reachable, every request it makes
// recorded, and every sheet field readable the way the page reads it.
//
// WHY IT LIVES IN testdata AND NOT BESIDE THE PAGE. Everything under
// www/static is embedded and served, and the scanners there
// (static_assets_served_test.go, classic_script_globals_test.go and the drift
// tests) read it as page code. A harness is not page code: nothing serves it
// and no page loads it, so it sits where Go keeps what only tests read.
//
// WHAT IS REAL. The page module itself, byte for byte less its import and
// export lines; every classic script processes.html loads before it, read off
// the template's own <script src> tags in the template's order (so a script
// added to the page is loaded here without anyone remembering to); esc.js and
// scene-geom.js; and operator-flow.js's renderFlowPicture / pictureRows /
// sentencesFromModel / relationPlacement / moduleOf, loaded the way
// operator-flow.test.js loads them; and shingoedge.js's showModal and
// hideModal, cut out of it whole (they are the sheet stack's scroll lock).
//
// WHAT IS STUBBED, AND WHY EACH IS SAFE FOR A PIN THAT RECORDS REQUESTS.
//   - The DOM. The page builds markup by string concatenation, so an element
//     here is its innerHTML string. getElementById finds the three elements
//     processes.html carries (page-data, pd-root, pd-scrim) and any id that
//     appears in markup the page has written, as a child carrying that tag's
//     attributes and contents, which lives until its container's markup is
//     written again — the browser's lifetime for an element drawn by
//     innerHTML. querySelector finds the first tag matching the LAST compound
//     of a selector; querySelectorAll answers one-part selectors only (the
//     option rows a popover binds its clicks to), so a pin clicks the very
//     element the page's listener is on. A sheet field ([data-f="x"]) reads
//     the value the markup opened with unless the pin typed one (type()),
//     and is one element whichever selector finds it. A drawn checkbox opens
//     checked as its markup says; a drawn select opens on its selected
//     option. document.querySelector answers from #pd-root, then #pd-scrim;
//     document.querySelectorAll answers nothing. focus() is recorded
//     (page.focused) and window listeners are kept (page.fireWindow).
//     NOT FOLLOWED: an outerHTML write (redrawPositionsTable) leaves its
//     container's markup as it was.
//   - Clicks: click() hands the host's own listener (#pd-root's onClick,
//     #pd-scrim's onAdvClick) an event whose target answers closest() from
//     the data attributes the pin gives it, then the document's closer
//     unless a listener stopped it — the bubble, without the tree.
//   - fetch: every call is recorded (method, url, parsed body) and answered
//     by the caller's serve(method, url, body); an unrouted call is a 404 and
//     is recorded like any other, so a new request shows up in the golden.
//     An answer may also be a body that is not JSON, or no answer at all (a
//     rejection): see ctx.fetch below.
//   - Timers: setTimeout queues; settle() drains microtasks and runs the
//     queue until both are empty. setInterval and requestAnimationFrame are
//     collected and never run (the rail's re-read and the re-measure pass
//     are not acts a pin drives).
//   - formatClock / formatTime (shared/utils.js): the timestamp as given —
//     the plant clock is not what these pins are about.
//   - Date: a fixed instant, so a "Saved 14:05" can never make a golden flap.
//
// EACH open() IS ITS OWN vm CONTEXT, so two pages never share a global: the
// page declares `function process()` and a dozen top-level consts, and a
// second load into one realm would collide with the first.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const WWW = path.join(__dirname, '..', '..');
const STATIC = path.join(WWW, 'static');
const SHARED = path.join(WWW, '..', '..', 'shared');
const read = p => fs.readFileSync(p, 'utf8');
// The wrapper the page is run inside adds one line above its first.
const PAGE_LINE_OFFSET = 1;

// The classic scripts processes.html puts on the page before the module, in
// its order. The module tag is not one of them.
function classicScripts() {
    const tmpl = read(path.join(WWW, 'templates', 'processes.html'));
    const out = [];
    for (const m of tmpl.matchAll(/<script\s+src="\/static\/([^"?]+)"/g)) out.push(m[1]);
    if (!out.length) throw new Error('processes.html names no classic script; the tag pattern has drifted');
    return out;
}

// The flow picture in its own context, shared by every page this process
// opens: it is pure over its inputs. The same load operator-flow.test.js and
// processes-desktop.popover-acts.test.js make.
let flowCache = null;
function loadFlow() {
    if (flowCache) return flowCache;
    const dir = path.join(STATIC, 'operator-station');
    const ctx = {
        console, Math, Set, Number, isFinite, JSON, Object, Array,
        document: { getElementById() { return null; } },
        window: { location: { hash: '' } },
        el() { return {}; },
        esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); },
        getView() { return null; },
    };
    vm.createContext(ctx);
    vm.runInContext(read(path.join(dir, 'composer-model.js')), ctx);
    vm.runInContext(read(path.join(SHARED, 'scene-geom.js')).replace(/^export /mg, ''), ctx);
    const src = read(path.join(dir, 'operator-flow.js')).replace(/^import[^;]+;\s*/mg, '').replace(/^export /mg, '');
    vm.runInContext(src + '\n__out = { renderFlowPicture, pictureRows, sentencesFromModel, relationPlacement, moduleOf };', ctx);
    flowCache = ctx.__out;
    return flowCache;
}

function classList() {
    const set = new Set();
    return {
        add: (...cs) => cs.forEach(c => set.add(c)),
        remove: (...cs) => cs.forEach(c => set.delete(c)),
        toggle: (c, on) => { const want = on === undefined ? !set.has(c) : !!on; if (want) set.add(c); else set.delete(c); return want; },
        contains: c => set.has(c),
    };
}

const unescapeAttr = s => String(s).replace(/&quot;/g, '"').replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&');

// ── the page ─────────────────────────────────────────────────────────────────

// open boots the page and returns its handle once boot has settled.
//
//   page      the page-data block: {processes, processGroups, styles,
//             stations, stationNodes, loaderGaps, activeProcessId}
//   search    window.location.search ('?process=1' opens process 1)
//   hash      window.location.hash (the page's own deep links)
//   serve     (method, url, body) => {status?, body} | undefined
async function open(opts) {
    const h = {
        calls: [],       // every fetch, in order: {method, url, body}
        typed: {},       // sheet field name -> typed value
        timers: [],      // pending setTimeout callbacks
        focused: [],     // every element the page called focus() on, in order
        winListeners: {},// window.addEventListener's, by type
    };
    let timerID = 0;

    // ── elements ────────────────────────────────────────────────────────────
    const statics = {};
    function makeEl(id, tag) {
        const e = {
            id: id || '', tagName: tag || 'DIV', hidden: false, disabled: false, checked: false, value: '',
            textContent: '', className: '', type: '',
            style: {}, dataset: {}, attrs: {}, classList: classList(), listeners: {}, parentNode: null, isConnected: true,
            offsetParent: null, offsetWidth: 240, offsetHeight: 40, clientWidth: 1084, clientHeight: 600,
            scrollTop: 0, scrollLeft: 0, scrollWidth: 1084, scrollHeight: 600,
            _html: '', _kids: {}, _parts: {},
            get innerHTML() { return this._html; },
            set innerHTML(v) {
                this._html = String(v);
                this._kids = {};
                this._parts = {};
                if (this === statics['pd-scrim']) h.typed = {};
            },
            set outerHTML(v) { this.lastOuterHTML = String(v); },
            getAttribute(n) { return n in this.attrs ? this.attrs[n] : null; },
            setAttribute(n, v) { this.attrs[n] = String(v); },
            hasAttribute(n) { return n in this.attrs; },
            removeAttribute(n) { delete this.attrs[n]; },
            addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); },
            removeEventListener() {},
            appendChild(c) { c.parentNode = this; return c; },
            querySelector(sel) { return query(this, sel); },
            querySelectorAll(sel) { return queryAll(this, sel); },
            getBoundingClientRect: () => ({ left: 0, top: 0, right: 200, bottom: 24, width: 200, height: 24 }),
            closest: () => null,
            // focus is recorded, so a pin can say where the page put it.
            focus() { h.focused.push(this); doc.activeElement = this; }, blur() {}, scrollIntoView() {},
        };
        return e;
    }

    // Every container whose markup may hold an id: the static elements and,
    // breadth first, the children found in their markup.
    function containers() {
        const out = [];
        const q = Object.values(statics);
        while (q.length) {
            const c = q.shift();
            out.push(c);
            for (const k of Object.values(c._kids)) q.push(k);
        }
        return out;
    }
    // The tags in a stretch of markup, each with its attributes, so a lookup
    // can ask what an element the page drew carries.
    function tagsIn(hay) {
        const out = [];
        for (const m of hay.matchAll(/<([a-z][\w-]*)\b([^>]*)>/gi)) {
            const attrs = {};
            for (const a of m[2].matchAll(/([\w-]+)(?:="([^"]*)")?/g)) attrs[a[1]] = a[2] === undefined ? '' : unescapeAttr(a[2]);
            out.push({ tag: m[1].toLowerCase(), attrs: attrs, src: m[0], at: m.index });
        }
        return out;
    }
    const markupOf = c => [c].concat(Object.values(c._kids)).map(x => x._html).join('\n');
    const camel = a => a.replace(/^data-/, '').replace(/-([a-z])/g, (_, x) => x.toUpperCase());

    // What an element holds: the markup between its tag and the tag that
    // closes it, counting nested tags of the same name.
    const VOID = new Set(['area', 'base', 'br', 'col', 'embed', 'hr', 'img', 'input', 'link', 'meta', 'source', 'track', 'wbr']);
    function innerOf(hay, t) {
        if (VOID.has(t.tag)) return '';
        const start = t.at + t.src.length;
        const re = new RegExp('<(/?)' + t.tag + '\\b[^>]*>', 'gi');
        re.lastIndex = start;
        let depth = 1, m;
        while ((m = re.exec(hay))) {
            if (m[1]) { if (--depth === 0) return hay.slice(start, m.index); } else depth++;
        }
        return hay.slice(start);
    }

    // An element the page drew, as a stub carrying the tag's own attributes
    // and contents: its dataset, class list and id, and the markup inside it,
    // so a listener the page binds to it and a pin that clicks it meet on one
    // object, and a lookup inside it finds what the page drew there. Its
    // closest() answers for itself only: the page's popover rows ask that of
    // the row clicked.
    function fromTag(t, hay) {
        const e = makeEl(t.attrs.id || '', t.tag.toUpperCase());
        for (const k of Object.keys(t.attrs)) {
            e.attrs[k] = t.attrs[k];
            if (k.startsWith('data-')) e.dataset[camel(k)] = t.attrs[k];
        }
        for (const c of String(t.attrs.class || '').split(/\s+/).filter(Boolean)) e.classList.add(c);
        e.hidden = 'hidden' in t.attrs;
        e.disabled = 'disabled' in t.attrs;
        e.checked = 'checked' in t.attrs;
        if (t.tag === 'input') e.type = t.attrs.type || 'text';
        if (t.tag === 'input' && t.attrs.value !== undefined) e.value = t.attrs.value;
        e._html = innerOf(hay, t);
        // A select's value is its selected option's, else its first; and
        // selectedOptions answers the option the value names (the health
        // strip's "Pin" reads both off its chooser).
        if (t.tag === 'select') {
            const opts = [...e._html.matchAll(/<option value="([^"]*)"([^>]*)>([\s\S]*?)<\/option>/g)]
                .map(m => ({ value: unescapeAttr(m[1]), selected: /\bselected\b/.test(m[2]), textContent: unescapeAttr(m[3].replace(/<[^>]*>/g, '')) }));
            e.value = ((opts.find(o => o.selected) || opts[0]) || { value: '' }).value;
            Object.defineProperty(e, 'selectedOptions', { get() { return opts.filter(o => o.value === e.value); } });
        }
        e.closest = sel => (!/\s/.test(sel.trim()) && tagMatches(t, simpleParts(sel)) ? e : null);
        return e;
    }

    function byId(id) {
        if (statics[id]) return statics[id];
        for (const c of containers()) {
            if (c._kids[id]) return c._kids[id];
            if (c._html.indexOf('id="' + id + '"') < 0) continue;
            // A sheet field with an id (New style's recipe chooser) is one
            // element whichever way the page reaches it.
            const t = tagsIn(c._html).find(x => x.attrs.id === id);
            const kid = t && t.attrs['data-f'] !== undefined ? field(c, t.attrs['data-f']) : (t ? fromTag(t, c._html) : makeEl(id));
            kid.parentNode = c;
            c._kids[id] = kid;
            return kid;
        }
        return null;
    }

    // A sheet field, read the way the page reads one: [data-f="x"] in the
    // container's markup or any child's. Its value is what the pin typed, else
    // what the markup opened with — value="…" on an input, the selected option
    // on a select. Kept per container so a listener the page adds stays bound.
    function field(c, name) {
        const key = 'f:' + name;
        if (c._parts[key]) return c._parts[key];
        const hay = markupOf(c);
        const t = tagsIn(hay).find(x => x.attrs['data-f'] === name && /^(input|select|textarea)$/.test(x.tag));
        if (!t) return null;
        let opened = '';
        if (t.tag === 'input') opened = t.attrs.value || '';
        if (t.tag === 'select') {
            const rest = hay.slice(t.at);
            const end = rest.indexOf('</select>');
            const block = end < 0 ? rest : rest.slice(0, end);
            const sel = /<option value="([^"]*)"[^>]*\bselected\b/.exec(block) || /<option value="([^"]*)"/.exec(block);
            opened = sel ? unescapeAttr(sel[1]) : '';
        }
        const f = fromTag(t, hay);
        f.type = t.tag === 'input' ? (t.attrs.type || 'text') : t.tag;
        Object.defineProperty(f, 'value', {
            get() { return name in h.typed ? h.typed[name] : opened; },
            set(v) { h.typed[name] = v; },
        });
        Object.defineProperty(f, 'checked', {
            get() { return name in h.typed ? !!h.typed[name] : 'checked' in t.attrs; },
            set(v) { h.typed[name] = !!v; },
        });
        f.getAttribute = n => (n in t.attrs ? t.attrs[n] : null);
        f.hasAttribute = n => n in t.attrs;
        c._parts[key] = f;
        if (t.attrs.id) c._kids[t.attrs.id] = f;
        return f;
    }

    // A selector is found when every simple part of its LAST compound appears
    // in the container's markup: `.cls` as a class word, `[a="v"]` and `[a]`
    // as attributes, `#id`, a tag. Not a selector engine — the page only asks
    // for elements it has just drawn, and the answer is one element per
    // (container, selector) that lives until the container's markup is
    // written again, so a status line the page writes is the one a pin reads.
    function simpleParts(sel) {
        return sel.trim().split(/\s+/).pop().match(/\.[\w-]+|#[\w-]+|\[[^\]]+\]|^[a-z]+/gi) || [];
    }
    function tagMatches(t, parts) {
        return parts.length > 0 && parts.every(p => {
            if (p[0] === '.') return String(t.attrs.class || '').split(/\s+/).indexOf(p.slice(1)) >= 0;
            if (p[0] === '#') return t.attrs.id === p.slice(1);
            if (p[0] === '[') {
                const m = /^\[([^=\]]+)(?:="([^"]*)")?\]$/.exec(p);
                return !!m && (m[1] in t.attrs) && (m[2] === undefined || t.attrs[m[1]] === m[2]);
            }
            return t.tag === p.toLowerCase();
        });
    }
    function query(c, sel) {
        const f = /^\[data-f="([^"]+)"\]$/.exec(sel);
        if (f) return field(c, f[1]);
        const key = 'q:' + sel;
        if (c._parts[key]) return c._parts[key];
        const parts = simpleParts(sel);
        const hay = markupOf(c);
        const t = tagsIn(hay).find(x => tagMatches(x, parts));
        if (!t) return null;
        return (c._parts[key] = fieldOr(c, t, hay));
    }
    // A SHEET FIELD IS ONE ELEMENT WHICHEVER SELECTOR REACHES IT: an input
    // carrying data-f found by another selector (Advanced's numbers, found by
    // [data-advkind="number"] to bind and by [data-f] to test for typed work)
    // is the field, so what the page types through one is what it reads
    // through the other.
    function fieldOr(c, t, hay) {
        if (t.attrs['data-f'] !== undefined && /^(input|select|textarea)$/.test(t.tag)) return field(c, t.attrs['data-f']);
        return fromTag(t, hay);
    }
    // Every element matching a ONE-part selector ('[data-opt]', '.pd-npk'),
    // in markup order: the option rows a popover binds its clicks to, which a
    // pin then clicks. A compound selector answers nothing.
    function queryAll(c, sel) {
        if (/\s/.test(sel.trim())) return [];
        const key = 'qa:' + sel;
        if (c._parts[key]) return c._parts[key];
        const parts = simpleParts(sel);
        const hay = markupOf(c);
        return (c._parts[key] = tagsIn(hay).filter(x => tagMatches(x, parts)).map(x => fieldOr(c, x, hay)));
    }

    const pageData = makeEl('page-data');
    const pd = opts.page || {};
    pageData.dataset = {
        processes: JSON.stringify(pd.processes || []),
        processGroups: JSON.stringify(pd.processGroups || []),
        styles: JSON.stringify(pd.styles || []),
        stations: JSON.stringify(pd.stations || []),
        stationNodes: JSON.stringify(pd.stationNodes || {}),
        loaderGaps: JSON.stringify(pd.loaderGaps || []),
        activeProcessId: String(pd.activeProcessId || 0),
    };
    statics['page-data'] = pageData;
    statics['pd-root'] = makeEl('pd-root');
    statics['pd-scrim'] = makeEl('pd-scrim');

    const doc = {
        readyState: 'complete',
        hidden: false,
        body: { classList: classList(), dataset: {}, style: {}, scrollWidth: 1440 },
        activeElement: null,
        documentElement: { clientWidth: 1440, clientHeight: 900 },
        listeners: {},
        getElementById: byId,
        addEventListener(t, fn) { (doc.listeners[t] = doc.listeners[t] || []).push(fn); },
        removeEventListener() {},
        // document.querySelector answers from the page and the scrim, in that
        // order (a suggestion box finds the PLC field it names by selector).
        // querySelectorAll stays empty: the pickers it would bind are driven
        // by their rows.
        querySelector: sel => query(statics["pd-root"], sel) || query(statics["pd-scrim"], sel),
        querySelectorAll: () => [],
        createElement: tag => makeEl('', String(tag).toUpperCase()),
    };

    // ── the realm ───────────────────────────────────────────────────────────
    const RealDate = Date;
    const FIXED = new RealDate(2026, 10, 11, 14, 5, 30).getTime();
    class FixedDate extends RealDate {
        constructor(...a) { if (a.length) super(...a); else super(FIXED); }
        static now() { return FIXED; }
    }
    const replaceStates = [];
    const ctx = {
        console,
        document: doc,
        location: { search: opts.search || '', hash: opts.hash || '', pathname: '/processes' },
        history: { replaceState: (st, title, url) => replaceStates.push(String(url)) },
        innerWidth: 1440, innerHeight: 900,
        addEventListener(t, fn) { (h.winListeners[t] = h.winListeners[t] || []).push(fn); }, removeEventListener() {},
        URLSearchParams, AbortController, Date: FixedDate,
        CSS: { escape: s => String(s).replace(/[^a-zA-Z0-9_-]/g, c => '\\' + c) },
        setTimeout: fn => { const id = ++timerID; h.timers.push({ id, fn }); return id; },
        clearTimeout: id => { h.timers = h.timers.filter(t => t.id !== id); },
        setInterval: () => 0, clearInterval() {},
        requestAnimationFrame: () => 0,
        formatTime: ts => String(ts || ''), formatClock: ts => String(ts || ''),
    };
    vm.createContext(ctx);
    vm.runInContext('var window = globalThis; var self = globalThis;', ctx);
    // showModal / hideModal are shingoedge.js's own two functions, cut out of
    // the module whole: the rest of it installs document listeners and opens
    // an EventSource, and the sheet stack's scroll lock is these two lines.
    const se = read(path.join(STATIC, 'js', 'shingoedge.js'));
    const cut = name => {
        const m = new RegExp('^export function ' + name + '\\([\\s\\S]*?\\r?\\n\\}\\r?\\n', 'm').exec(se);
        if (!m) throw new Error('shingoedge.js has no export function ' + name + '; the cut has drifted');
        return m[0].replace(/^export /, '');
    };
    vm.runInContext(cut('showModal') + cut('hideModal'), ctx, { filename: 'shingoedge.js' });

    // fetch: recorded, then answered by the caller. The answer is parsed by
    // the page's own JSON, so what the page holds is of its own realm.
    //
    // AN ANSWER IS ONE OF THREE THINGS. {status, body}: JSON, as writeError
    // and writeJSON send it. {status, raw}: a body that is not JSON (chi's
    // Recoverer's empty 500, the login page an expired session's 303 lands
    // on), whose json() rejects as a browser's does. {reject}: no answer at
    // all, the TypeError a browser's fetch rejects with when the Edge is
    // down. Each call also carries `site`, the line of processes-desktop.js
    // the fetch was called from, and `stack`, every page line on the stack
    // (so a postJSON write names its caller), read off the stack (in no golden:
    // the page's line numbers move with every edit).
    const ctxJSON = vm.runInContext('JSON', ctx);
    const ctxTypeError = vm.runInContext('TypeError', ctx);
    ctx.fetch = (url, init) => {
        const method = (init && init.method) || 'GET';
        let body;
        if (init && init.body !== undefined) {
            try { body = JSON.parse(init.body); } catch (_) { body = init.body; }
        }
        const limit = Error.stackTraceLimit;
        Error.stackTraceLimit = 40;
        const trace = String(new Error().stack);
        Error.stackTraceLimit = limit;
        const frames = [...trace.matchAll(/processes-desktop\.js:(\d+):\d+/g)].map(m => Number(m[1]) - PAGE_LINE_OFFSET);
        const call = { method: method, url: String(url), body: body };
        Object.defineProperty(call, 'site', { value: frames[0] || 0 });
        Object.defineProperty(call, 'stack', { value: frames });
        h.calls.push(call);
        const ans = opts.serve ? opts.serve(method, String(url), body) : undefined;
        if (ans && ans.reject) return Promise.reject(new ctxTypeError(ans.reject));
        const status = ans ? (ans.status || 200) : 404;
        const text = ans && ans.raw !== undefined ? String(ans.raw)
            : JSON.stringify(ans ? (ans.body === undefined ? {} : ans.body) : { error: 'no route in the fixture' });
        return Promise.resolve({
            ok: status >= 200 && status < 300, status: status, redirected: !!(ans && ans.redirected),
            json: async () => ctxJSON.parse(text), text: async () => text,
        });
    };

    for (const rel of classicScripts()) {
        vm.runInContext(read(path.join(STATIC, rel)), ctx, { filename: rel });
    }
    vm.runInContext(read(path.join(SHARED, 'esc.js')).replace(/^export\s+/mg, ''), ctx, { filename: 'esc.js' });
    vm.runInContext(read(path.join(SHARED, 'scene-geom.js')).replace(/^export\s+/mg, ''), ctx, { filename: 'scene-geom.js' });
    const flow = loadFlow();
    Object.assign(ctx, {
        renderFlowPicture: flow.renderFlowPicture, pictureRows: flow.pictureRows,
        sentencesFromModel: flow.sentencesFromModel, relationPlacement: flow.relationPlacement, moduleOf: flow.moduleOf,
    });

    // THE PAGE, in a function scope of its own, with one line added at its
    // foot: __pd(name) evaluates a name in the module's scope, so a pin calls
    // the page's own functions and reads its own S rather than a copy.
    // An import or export line becomes as many empty lines as it had, so a
    // stack's line is the page's line less the one the wrapper adds.
    let src = read(path.join(STATIC, 'js', 'pages', 'processes-desktop.js'));
    const blank = m => m.replace(/[^\n]/g, '');
    src = src.replace(/^import[\s\S]*?from\s+'[^']+';[ \t]*$/gm, blank);
    src = src.replace(/^export\s*\{[^}]*\};?[ \t]*$/m, blank);
    vm.runInContext('(function(){\n' + src + '\n;globalThis.__pd = function (s) { return eval(s); };\n})();', ctx,
        { filename: 'processes-desktop.js' });

    const page = {
        ctx, doc, replaceStates,
        calls: h.calls,
        root: statics['pd-root'],
        scrim: statics['pd-scrim'],
        pd: name => ctx.__pd(name),
        get S() { return ctx.__pd('S'); },
        // type puts a value in a sheet field, as an engineer typing would.
        type(name, value) { h.typed[name] = value; },
        byId,
        // The sheet's status line as the page last wrote it, or ''.
        status() { const st = query(statics['pd-scrim'], '.mf .st'); return st ? st.textContent : ''; },
        sheetOpen() { return statics['pd-scrim'].classList.contains('active'); },
        // mark/since: the calls one act made.
        mark() { return h.calls.length; },
        since(i) { return h.calls.slice(i); },
        settle: () => settle(h),
        // click dispatches a click the way a browser's bubble reaches the
        // page, short of a tree: the target carries `data` (its data-*
        // attributes, camelCased as dataset spells them) and `cls`; its
        // closest() answers the page's handlers from those, and from the
        // ancestors named in `inside` ('.pd-modal' for a button in a sheet).
        // The host's click listeners run — 'root' is #pd-root's onClick,
        // 'scrim' is #pd-scrim's onAdvClick — then the document's closer,
        // unless a listener stopped the event.
        click(host, data, o) {
            o = o || {};
            const target = makeEl('');
            Object.assign(target.dataset, data || {});
            for (const c of o.cls || []) target.classList.add(c);
            target.closest = sel => {
                if (/\s/.test(sel.trim())) return null;
                const ok = simpleParts(sel).every(p => {
                    if (p[0] === '.') return target.classList.contains(p.slice(1));
                    if (p[0] === '#') return false;
                    const m = /^\[([^=\]]+)(?:="([^"]*)")?\]$/.exec(p);
                    if (!m || !m[1].startsWith('data-')) return false;
                    const v = target.dataset[camel(m[1])];
                    return v !== undefined && (m[2] === undefined || v === m[2]);
                });
                if (ok) return target;
                return (o.inside || []).indexOf(sel) >= 0 ? makeEl('') : null;
            };
            const ev = {
                type: 'click', target: target, stopped: false,
                stopPropagation() { this.stopped = true; }, stopImmediatePropagation() { this.stopped = true; },
                preventDefault() {},
            };
            const el = statics[host === 'scrim' ? 'pd-scrim' : 'pd-root'];
            for (const fn of (el.listeners.click || []).slice()) fn(ev);
            if (!ev.stopped) for (const fn of (doc.listeners.click || []).slice()) fn(ev);
            return target;
        },
        // fire runs the listeners the page bound to an element it drew: an
        // option row a popover bound with addEventListener, a select's change.
        fire(el, type) {
            const ev = { type: type, target: el, stopPropagation() {}, stopImmediatePropagation() {}, preventDefault() {} };
            for (const fn of (el.listeners[type] || []).slice()) fn(ev);
        },
        // The rows of a popover the page drew into element `id` that carry
        // attribute `attr` — the elements its click listeners are bound to.
        rows(id, attr) { const el = byId(id); return el ? el.querySelectorAll('[' + attr + ']') : []; },
        // key presses a key on the document: the page's keydown listeners
        // (boot's Escape rule) see {key}.
        key(k) {
            const ev = { type: 'keydown', key: k, target: doc.body, stopPropagation() {}, preventDefault() {} };
            for (const fn of (doc.listeners.keydown || []).slice()) fn(ev);
        },
        // fireWindow runs the listeners the page put on window (focus, error,
        // unhandledrejection) with `ev`.
        fireWindow(type, ev) {
            for (const fn of (h.winListeners[type] || []).slice()) fn(Object.assign({ type: type }, ev || {}));
        },
        hasWindowListener: type => !!(h.winListeners[type] || []).length,
        // Every element the page focused, in order, and the one it holds now.
        focused: h.focused,
        // segments: every run of text the page has drawn, each with where it
        // is — its container (#id) and the innermost element around it
        // (tag#id.class) — then every textContent the page set on an element
        // it looked up (a sheet's status line, a button's label). The refusal
        // pins compare two of these to say what a refusal put on screen.
        segments() {
            const out = [];
            for (const c of containers()) {
                const host = '#' + (c.id || '?');
                const stack = [];
                const re = /<(\/?)([a-z][\w-]*)\b([^>]*)>|([^<]+)/gi;
                let m;
                while ((m = re.exec(c._html))) {
                    if (m[4] !== undefined) {
                        const t = unescapeAttr(m[4]).replace(/&times;/g, '×').replace(/\s+/g, ' ').trim();
                        if (t) out.push(host + ' ' + (stack.length ? stack[stack.length - 1].d : '') + ': ' + t);
                        continue;
                    }
                    const tag = m[2].toLowerCase();
                    if (m[1]) {
                        const i = stack.map(s => s.tag).lastIndexOf(tag);
                        if (i >= 0) stack.length = i;
                        continue;
                    }
                    if (VOID.has(tag)) continue;
                    const cls = /\bclass="([^"]*)"/.exec(m[3]), id = /\bid="([^"]*)"/.exec(m[3]);
                    stack.push({ tag: tag, d: tag + (id ? '#' + id[1] : '') + (cls && cls[1].trim() ? '.' + cls[1].trim().split(/\s+/).join('.') : '') });
                }
                for (const [k, v] of Object.entries(c._parts)) {
                    for (const e of [].concat(v)) {
                        if (e && e.textContent) out.push(host + ' ' + k.replace(/^(q|qa|f):/, '') + ' (textContent): ' + e.textContent);
                    }
                }
            }
            return out;
        },
    };
    await page.settle();
    return page;
}

// settle drains the microtask queue and runs every queued timer, until
// neither has anything left: the request a debounced preview sends is on the
// wire when settle returns. Bounded, so a page that re-arms a timer forever
// fails loudly instead of hanging the gate.
async function settle(h) {
    const drain = () => new Promise(r => setImmediate(r));
    for (let round = 0; round < 50; round++) {
        for (let i = 0; i < 12; i++) await drain();
        if (!h.timers.length) return;
        const due = h.timers;
        h.timers = [];
        for (const t of due) await t.fn();
    }
    throw new Error('the page kept re-arming a timer for 50 rounds');
}

module.exports = { open, loadFlow };
