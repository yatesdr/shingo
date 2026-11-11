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
// operator-flow.test.js loads them.
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
//     the value the markup opened with unless the pin typed one (type()).
//     NOT FOLLOWED: an outerHTML write (redrawPositionsTable) leaves its
//     container's markup as it was.
//   - Clicks: click() hands the host's own listener (#pd-root's onClick,
//     #pd-scrim's onAdvClick) an event whose target answers closest() from
//     the data attributes the pin gives it, then the document's closer
//     unless a listener stopped it — the bubble, without the tree.
//   - fetch: every call is recorded (method, url, parsed body) and answered
//     by the caller's serve(method, url, body); an unrouted call is a 404 and
//     is recorded like any other, so a new request shows up in the golden.
//   - Timers: setTimeout queues; settle() drains microtasks and runs the
//     queue until both are empty. setInterval and requestAnimationFrame are
//     collected and never run (the rail's re-read and the re-measure pass
//     are not acts a pin drives).
//   - showModal / hideModal (shingoedge.js): toggle .active, as the base does.
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
            focus() {}, blur() {}, scrollIntoView() {},
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
        if (t.tag === 'input' && t.attrs.value !== undefined) e.value = t.attrs.value;
        e._html = innerOf(hay, t);
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
        return (c._parts[key] = fromTag(t, hay));
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
        return (c._parts[key] = tagsIn(hay).filter(x => tagMatches(x, parts)).map(x => fromTag(x, hay)));
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
        body: { classList: classList(), dataset: {}, scrollWidth: 1440 },
        documentElement: { clientWidth: 1440, clientHeight: 900 },
        listeners: {},
        getElementById: byId,
        addEventListener(t, fn) { (doc.listeners[t] = doc.listeners[t] || []).push(fn); },
        removeEventListener() {},
        querySelector: () => null,
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
        addEventListener() {}, removeEventListener() {},
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
    ctx.showModal = id => { const m = byId(id); if (m) m.classList.add('active'); };
    ctx.hideModal = id => { const m = byId(id); if (m) m.classList.remove('active'); };

    // fetch: recorded, then answered by the caller. The answer is parsed by
    // the page's own JSON, so what the page holds is of its own realm.
    const ctxJSON = vm.runInContext('JSON', ctx);
    ctx.fetch = (url, init) => {
        const method = (init && init.method) || 'GET';
        let body;
        if (init && init.body !== undefined) {
            try { body = JSON.parse(init.body); } catch (_) { body = init.body; }
        }
        h.calls.push({ method: method, url: String(url), body: body });
        const ans = opts.serve ? opts.serve(method, String(url), body) : undefined;
        const status = ans ? (ans.status || 200) : 404;
        const text = JSON.stringify(ans ? (ans.body === undefined ? {} : ans.body) : { error: 'no route in the fixture' });
        return Promise.resolve({ ok: status >= 200 && status < 300, status: status, json: async () => ctxJSON.parse(text) });
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
    let src = read(path.join(STATIC, 'js', 'pages', 'processes-desktop.js'));
    src = src.replace(/^import[\s\S]*?from\s+'[^']+';\s*$/gm, '');
    src = src.replace(/^export\s*\{[^}]*\};?\s*$/m, '');
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
