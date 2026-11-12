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
// operator-flow.test.js loads them; the shared components the page imports
// (layer.js, notice.js, http.js, picker.js and dom.js under them), each loaded
// once into the page's realm with its imports bound, as a browser's module map
// holds them (sharedLoader).
//
// WHAT THE COMPONENTS BUILD IS A REAL TREE. They build with dom.js's el(), so
// an element here holds either the markup the page wrote (a string, read by
// the lookups below) or built children (childNodes, text nodes, events that
// bubble to the document), and a lookup walks both in document order. A sheet
// whose shell the layer built and whose body is the page's markup is one
// sheet to the page and to a pin. A toast is recorded as notice.js shows it
// (page.notices), since settle() runs the timer that takes it down.
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
//   - formatClock / formatTime (shared/clock.js): the timestamp as given —
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

function classList(set) {
    set = set || new Set();
    return {
        add: (...cs) => cs.forEach(c => set.add(c)),
        remove: (...cs) => cs.forEach(c => set.delete(c)),
        toggle: (c, on) => { const want = on === undefined ? !set.has(c) : !!on; if (want) set.add(c); else set.delete(c); return want; },
        contains: c => set.has(c),
        _set: set,
    };
}

// The modules a page imports from shared/ that run here for real: the
// components (layer.js, notice.js, http.js, picker.js) and the DOM file they
// build with. esc.js, clock.js and scene-geom.js stay the realm globals they
// always were (see open).
const SHARED_REAL = new Set(['dom.js', 'notice.js', 'http.js', 'layer.js', 'picker.js']);
const sharedFile = spec => {
    const m = /^\/static\/shared\/([\w.-]+\.js)$/.exec(spec);
    return m && SHARED_REAL.has(m[1]) ? m[1] : null;
};
const blankLines = m => m.replace(/[^\n]/g, '');
// An import list as a destructuring pattern: `a, b as c` is `a, b: c`.
const destructure = list => '{' + list.replace(/\s+/g, ' ').replace(/\b([\w$]+)\s+as\s+([\w$]+)/g, '$1: $2') + '}';

// sharedLoader is a browser's module map for shared/, in one realm: each
// file loads once, its imports bound from the files already loaded, its
// exports returned, so the page and http.js hold the one notice.js and the
// page and picker.js the one layer.js, as they do in a browser.
function sharedLoader(ctx) {
    const cache = {};
    const load = spec => {
        const file = sharedFile(spec);
        if (!file) throw new Error('the page harness loads no ' + spec + ' (only ' + [...SHARED_REAL].join(', ') + ')');
        if (cache[file]) return cache[file];
        let src = read(path.join(SHARED, file));
        const names = [];
        src = src.replace(/^import\s*\{([^}]*)\}\s*from\s*'([^']+)';[ \t]*$/gm,
            (m, list, from) => 'const ' + destructure(list) + ' = __shared(' + JSON.stringify(from) + ');' + blankLines(m));
        src = src.replace(/^export\s+(async\s+function|function|const|let|class)\s+([A-Za-z_$][\w$]*)/gm,
            (m, kw, name) => { names.push(name); return kw + ' ' + name; });
        const fn = vm.runInContext('(function (__shared) {' + src + '\nreturn { ' + names.join(', ') + ' };\n})', ctx,
            { filename: file });
        cache[file] = fn(load);
        return cache[file];
    };
    return load;
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
        notices: [],     // every toast notice.js showed, in order: see noticeAppended
        noticeAt: {},    // a mark (a call count) -> how many notices had shown by then
    };
    let timerID = 0;

    // ── elements ────────────────────────────────────────────────────────────
    //
    // TWO KINDS OF CONTENT, ONE ELEMENT. What the page writes as markup stays
    // a string (_html), read by the string lookups below. What the shared
    // components build with dom.js's el() (a sheet's shell, the discard
    // question, a toast, a picker) is a real tree: childNodes, parentNode,
    // text nodes, events that bubble to the document. An element can hold
    // either, and a lookup walks both in document order, so a sheet whose
    // shell the layer built and whose body is the page's markup is one sheet
    // to the page and to a pin.
    const statics = {};
    const NodeBase = function Node() {};
    function makeEl(id, tag) {
        const cls = new Set();
        const e = {
            id: id || '', tagName: tag || 'DIV', hidden: false, disabled: false, checked: false, value: '',
            defaultValue: '', type: '',
            style: {}, dataset: {}, attrs: {}, classList: classList(cls), listeners: {}, parentNode: null,
            childNodes: [], nodeType: 1,
            offsetParent: null, offsetWidth: 240, offsetHeight: 40, clientWidth: 1084, clientHeight: 600,
            scrollTop: 0, scrollLeft: 0, scrollWidth: 1084, scrollHeight: 600,
            _html: '', _kids: {}, _parts: {}, _text: '', _stale: false,
            get className() { return [...cls].join(' '); },
            set className(v) { cls.clear(); for (const c of String(v || '').split(/\s+/).filter(Boolean)) cls.add(c); },
            get innerHTML() { return this.childNodes.length ? this.childNodes.map(serialize).join('') : this._html; },
            set innerHTML(v) {
                this._html = String(v);
                for (const k of Object.values(this._kids)) k._stale = true;
                for (const p of Object.values(this._parts)) for (const x of [].concat(p)) if (x) x._stale = true;
                this._kids = {};
                this._parts = {};
                for (const c of this.childNodes) c.parentNode = null;
                this.childNodes = [];
                if (this === statics['pd-scrim'] || cls.has('layer-body')) h.typed = {};
            },
            get textContent() { return this.childNodes.length ? this.childNodes.map(c => c.textContent).join('') : this._text; },
            set textContent(v) {
                for (const c of this.childNodes) c.parentNode = null;
                this.childNodes = [];
                this._text = v === null || v === undefined ? '' : String(v);
                if (this._made) this._html = '';
            },
            get children() { return this.childNodes.filter(c => c.nodeType === 1); },
            get parentElement() { return this.parentNode && this.parentNode.nodeType === 1 ? this.parentNode : null; },
            // Connected while its chain reaches the page: a static element,
            // or the body. An element found in markup that was written again
            // since is gone, as the browser's would be.
            get isConnected() {
                for (let n = this; n; n = n.parentNode) {
                    if (n._stale) return false;
                    if (n._static || n === doc.body) return true;
                }
                return false;
            },
            set outerHTML(v) { this.lastOuterHTML = String(v); },
            getAttribute(n) { return n in this.attrs ? this.attrs[n] : null; },
            setAttribute(n, v) {
                v = String(v);
                this.attrs[n] = v;
                if (n === 'id') this.id = v;
                else if (n === 'class') this.className = v;
                else if (n === 'type') this.type = v;
                else if (n === 'value') { this.value = v; this.defaultValue = v; }
                else if (n === 'disabled') this.disabled = true;
                else if (n === 'hidden') this.hidden = true;
                else if (n.startsWith('data-')) this.dataset[camel(n)] = v;
            },
            hasAttribute(n) { return n in this.attrs; },
            removeAttribute(n) {
                delete this.attrs[n];
                if (n.startsWith('data-')) delete this.dataset[camel(n)];
            },
            addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); },
            removeEventListener(t, fn) { if (this.listeners[t]) this.listeners[t] = this.listeners[t].filter(f => f !== fn); },
            dispatchEvent(ev) { return fire(this, ev); },
            click() { if (!this.disabled) fire(this, makeEvent('click')); },
            appendChild(c) { return this.insertBefore(c, null); },
            insertBefore(c, ref) {
                if (c.parentNode) c.parentNode.removeChild(c);
                const at = ref ? this.childNodes.indexOf(ref) : -1;
                if (at < 0) this.childNodes.push(c); else this.childNodes.splice(at, 0, c);
                c.parentNode = this;
                c._stale = false;
                if (c.nodeType === 1) noticeAppended(c);
                return c;
            },
            removeChild(c) {
                const at = this.childNodes.indexOf(c);
                if (at >= 0) this.childNodes.splice(at, 1);
                c.parentNode = null;
                return c;
            },
            remove() { if (this.parentNode && this.parentNode.removeChild) this.parentNode.removeChild(this); },
            contains(x) { for (let n = x; n; n = n.parentNode) if (n === this) return true; return false; },
            matches(sel) { return matchesEl(this, sel); },
            querySelector(sel) { return query(this, sel); },
            querySelectorAll(sel) { return queryAll(this, sel); },
            getBoundingClientRect: () => ({ left: 0, top: 0, right: 200, bottom: 24, width: 200, height: 24 }),
            closest(sel) { for (let n = this; n && n.nodeType === 1; n = n.parentNode) if (matchesEl(n, sel)) return n; return null; },
            // focus is recorded, so a pin can say where the page put it.
            focus() { h.focused.push(this); doc.activeElement = this; }, blur() {}, scrollIntoView() {},
        };
        Object.setPrototypeOf(e, NodeBase.prototype);
        return e;
    }
    function makeText(s) {
        const t = { nodeType: 3, parentNode: null, _stale: false, textContent: String(s), remove() { if (this.parentNode) this.parentNode.removeChild(this); } };
        Object.setPrototypeOf(t, NodeBase.prototype);
        return t;
    }
    // serialize writes a built element back as markup, so a pin that reads a
    // sheet's markup reads the shell the layer built around the page's body.
    const escText = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
    function serialize(n) {
        if (n.nodeType === 3) return escText(n.textContent);
        const tag = n.tagName.toLowerCase();
        const attrs = Object.assign({}, n.attrs);
        if (n.className) attrs.class = n.className; else delete attrs.class;
        if (n.id) attrs.id = n.id;
        if (n.disabled) attrs.disabled = ''; else delete attrs.disabled;
        if (n.hidden) attrs.hidden = ''; else delete attrs.hidden;
        const a = Object.keys(attrs).map(k => ' ' + k + (attrs[k] === '' && /^(disabled|hidden|data-[\w-]+)$/.test(k) ? '' : '="' + escText(attrs[k]).replace(/"/g, '&quot;') + '"')).join('');
        return '<' + tag + a + '>' + (VOID.has(tag) ? '' : n.innerHTML + '</' + tag + '>');
    }

    // A selector against a built element: alternatives split on commas, each
    // a compound of .class, #id, [attr], [attr="v"] and a tag. A combinator
    // matches nothing (the components ask none of a built element).
    function matchesEl(n, sel) {
        if (!n || n.nodeType !== 1) return false;
        return String(sel).split(',').some(alt => {
            alt = alt.trim();
            if (!alt || /\s/.test(alt)) return false;
            const parts = alt.match(/\.[\w-]+|#[\w-]+|\[[^\]]+\]|^[a-z][\w-]*/gi) || [];
            return parts.length > 0 && parts.every(p => {
                if (p[0] === '.') return n.classList.contains(p.slice(1));
                if (p[0] === '#') return n.id === p.slice(1);
                if (p[0] === '[') {
                    const m = /^\[([^=\]]+)(?:="([^"]*)")?\]$/.exec(p);
                    return !!m && n.hasAttribute(m[1]) && (m[2] === undefined || n.getAttribute(m[1]) === m[2]);
                }
                return n.tagName.toLowerCase() === p.toLowerCase();
            });
        });
    }

    // ── events through the built tree ───────────────────────────────────────
    function makeEvent(type, extra) {
        return Object.assign({
            type: type, target: null, stopped: false, defaultPrevented: false,
            stopPropagation() { this.stopped = true; }, stopImmediatePropagation() { this.stopped = true; },
            preventDefault() { this.defaultPrevented = true; },
        }, extra || {});
    }
    // fire is a browser's dispatch, short of capture on elements: the
    // document's capture listeners, then the target and each ancestor, then
    // the document's own (a bubbling event that reached the page).
    function fire(target, ev) {
        if (typeof ev.stopPropagation !== 'function') Object.assign(ev, makeEvent(ev.type), { type: ev.type, detail: ev.detail });
        ev.target = target;
        for (const fn of (doc.capture[ev.type] || []).slice()) fn(ev);
        let reached = false;
        for (let n = target; n; n = n.parentNode) {
            for (const fn of ((n.listeners && n.listeners[ev.type]) || []).slice()) { ev.currentTarget = n; fn.call(n, ev); }
            if (n === doc.body || n._static) reached = true;
            if (ev.stopped) return !ev.defaultPrevented;
        }
        if (reached || target === doc.body) for (const fn of (doc.listeners[ev.type] || []).slice()) fn(ev);
        return !ev.defaultPrevented;
    }

    // NOTICES ARE READ AS THEY ARE SHOWN. A toast lives 3.2 or 5 s, and
    // settle() runs every queued timer, so by the time a pin looks the toast
    // is gone: each one is recorded when notice.js puts it in its stack.
    function noticeAppended(c) {
        if (!c.classList.contains('notice-toast')) return;
        const lvl = [...c.classList._set].map(x => /^notice-toast--(\w+)$/.exec(x)).filter(Boolean)[0];
        const act = c.querySelector('.notice-action');
        h.notices.push({
            text: (c.querySelector('.notice-text') || c).textContent, level: lvl ? lvl[1] : '',
            action: act ? act.textContent + (act.getAttribute('href') ? ' → ' + act.getAttribute('href') : '') : '',
            sticky: c.classList.contains('is-sticky'),
        });
    }

    // Every container whose markup may hold an id: the static elements and,
    // breadth first, the children found in their markup and the elements
    // built into them, then what the components built on the body.
    function containers() {
        const out = [];
        const q = Object.values(statics).concat([doc.body]);
        const seen = new Set();
        while (q.length) {
            const c = q.shift();
            if (seen.has(c)) continue;
            seen.add(c);
            out.push(c);
            for (const k of Object.values(c._kids)) q.push(k);
            for (const k of c.childNodes) if (k.nodeType === 1) q.push(k);
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
    function selectOptions(html) {
        return [...String(html).matchAll(/<option value="([^"]*)"([^>]*)>([\s\S]*?)<\/option>/g)].map(m => {
            const o = { value: unescapeAttr(m[1]), selected: /\bselected\b/.test(m[2]), textContent: unescapeAttr(m[3].replace(/<[^>]*>/g, '')) };
            o.hasAttribute = n => n === 'selected' && o.selected;
            return o;
        });
    }
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
        if (t.tag === 'input' && t.attrs.value !== undefined) { e.value = t.attrs.value; e.defaultValue = t.attrs.value; }
        e._html = innerOf(hay, t);
        // A select's value is its selected option's, else its first; and
        // selectedOptions answers the option the value names (the health
        // strip's "Pin" reads both off its chooser). options answers the
        // layer's question of which option the markup selected.
        if (t.tag === 'select') {
            const opts = selectOptions(e._html);
            e.value = ((opts.find(o => o.selected) || opts[0]) || { value: '' }).value;
            Object.defineProperty(e, 'selectedOptions', { get() { return opts.filter(o => o.value === e.value); } });
            e.options = opts;
        }
        e.closest = sel => (!/\s/.test(sel.trim()) && tagMatches(t, simpleParts(sel)) ? e : null);
        return e;
    }

    function byId(id) {
        if (statics[id]) return statics[id];
        for (const c of containers()) {
            if (c._made && c.id === id) return c;
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
        f.parentNode = c;
        f.defaultValue = opened;
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
    // A selector's alternatives (`input, select, textarea`), each read as
    // simpleParts reads a selector.
    const alternatives = sel => String(sel).split(',').map(s => s.trim()).filter(Boolean);
    function query(c, sel) {
        if (c.childNodes.length) return walkFind(c, sel, true)[0] || null;
        const f = /^\[data-f="([^"]+)"\]$/.exec(sel);
        if (f) return field(c, f[1]);
        const key = 'q:' + sel;
        if (c._parts[key]) return c._parts[key];
        const list = alternatives(sel).map(simpleParts);
        const hay = markupOf(c);
        const t = tagsIn(hay).find(x => list.some(parts => tagMatches(x, parts)));
        if (!t) return null;
        return (c._parts[key] = fieldOr(c, t, hay));
    }
    // walkFind answers a selector over an element that holds built children,
    // in document order: each built element is matched itself, and the
    // markup a built element holds (a sheet's body) is read the string way.
    function walkFind(c, sel, first) {
        const out = [];
        const visit = n => {
            for (const k of n.childNodes) {
                if (k.nodeType !== 1) continue;
                if (matchesEl(k, sel)) { out.push(k); if (first) return true; }
                if (k.childNodes.length) { if (visit(k) && first) return true; continue; }
                if (!k._html) continue;
                const found = first ? [query(k, sel)].filter(Boolean) : queryAll(k, sel);
                out.push(...found);
                if (first && found.length) return true;
            }
            return false;
        };
        visit(c);
        return out;
    }
    // A SHEET FIELD IS ONE ELEMENT WHICHEVER SELECTOR REACHES IT: an input
    // carrying data-f found by another selector (Advanced's numbers, found by
    // [data-advkind="number"] to bind and by [data-f] to test for typed work)
    // is the field, so what the page types through one is what it reads
    // through the other.
    function fieldOr(c, t, hay) {
        if (t.attrs['data-f'] !== undefined && /^(input|select|textarea)$/.test(t.tag)) return field(c, t.attrs['data-f']);
        const e = fromTag(t, hay);
        e.parentNode = c;
        return e;
    }
    // Every element matching a ONE-part selector ('[data-opt]', '.pd-npk'),
    // or a list of them ('input, select, textarea'), in markup order: the
    // option rows a popover binds its clicks to, which a pin then clicks. A
    // compound selector answers nothing.
    function queryAll(c, sel) {
        if (c.childNodes.length) return walkFind(c, sel, false);
        const alts = alternatives(sel);
        if (!alts.length || alts.some(a => /\s/.test(a))) return [];
        const key = 'qa:' + sel;
        if (c._parts[key]) return c._parts[key];
        const list = alts.map(simpleParts);
        const hay = markupOf(c);
        return (c._parts[key] = tagsIn(hay).filter(x => list.some(parts => tagMatches(x, parts))).map(x => fieldOr(c, x, hay)));
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
    for (const s of Object.values(statics)) s._static = true;

    const doc = {
        readyState: 'complete',
        hidden: false,
        body: null,
        activeElement: null,
        documentElement: { clientWidth: 1440, clientHeight: 900 },
        // listeners[type] is every listener, in the order added (the pins
        // run them by hand); capture[type] is the ones added for the capture
        // phase, which fire() runs first.
        listeners: {},
        capture: {},
        getElementById: byId,
        addEventListener(t, fn, o) {
            (doc.listeners[t] = doc.listeners[t] || []).push(fn);
            if (o === true || (o && o.capture)) (doc.capture[t] = doc.capture[t] || []).push(fn);
        },
        removeEventListener(t, fn) {
            if (doc.listeners[t]) doc.listeners[t] = doc.listeners[t].filter(f => f !== fn);
            if (doc.capture[t]) doc.capture[t] = doc.capture[t].filter(f => f !== fn);
        },
        // document.querySelector answers from the page and the scrim, in that
        // order (a suggestion box finds the PLC field it names by selector),
        // then from what the components built on the body (an open sheet).
        // querySelectorAll stays empty: the pickers it would bind are driven
        // by their rows.
        querySelector: sel => query(statics["pd-root"], sel) || query(statics["pd-scrim"], sel) || query(doc.body, sel),
        querySelectorAll: () => [],
        createElement: tag => { const e = makeEl('', String(tag).toUpperCase()); e._made = true; return e; },
        createTextNode: s => makeText(s),
    };
    doc.body = makeEl('', 'BODY');
    doc.body.scrollWidth = 1440;

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
        Node: NodeBase,
        CustomEvent: function CustomEvent(type, init) { Object.assign(this, makeEvent(type), { detail: init && init.detail, bubbles: !!(init && init.bubbles) }); },
    };
    vm.createContext(ctx);
    vm.runInContext('var window = globalThis; var self = globalThis;', ctx);
    const pageSrc = read(path.join(STATIC, 'js', 'pages', 'processes-desktop.js'));
    ctx.__shared = sharedLoader(ctx);

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
    // An import from a shared module that runs here for real (SHARED_REAL)
    // becomes the same names bound from the realm's module map, on the
    // import's own lines; every other import line is blanked, its names being
    // realm globals.
    let src = pageSrc;
    const blank = m => m.replace(/[^\n]/g, '');
    src = src.replace(/^import\s*\{([^}]*)\}\s*from\s+'([^']+)';[ \t]*$/gm, (m, list, from) =>
        (sharedFile(from) ? 'const ' + destructure(list) + ' = __shared(' + JSON.stringify(from) + ');' : '') + blank(m));
    src = src.replace(/^import[\s\S]*?from\s+'[^']+';[ \t]*$/gm, blank);
    src = src.replace(/^export\s*\{[^}]*\};?[ \t]*$/m, blank);
    vm.runInContext('(function(){\n' + src + '\n;globalThis.__pd = function (s) { return eval(s); };\n})();', ctx,
        { filename: 'processes-desktop.js' });

    // Where a run of text is, for segments(): a static element or one found
    // in markup by its #id; a built one by its own id or its nearest built
    // ancestor's, else by the class of the outermost built element it is in
    // (.layer-scrim for a sheet, .notice-root for a toast).
    function hostOf(c) {
        if (!c._made) return '#' + (c.id || '?');
        let top = c;
        for (let n = c; n && n._made; n = n.parentNode) { if (n.id) return '#' + n.id; top = n; }
        return '.' + (top.className.split(/\s+/)[0] || top.tagName.toLowerCase());
    }
    function descOf(e) {
        return e.tagName.toLowerCase() + (e.id ? '#' + e.id : '') + (e.className ? '.' + e.className.split(/\s+/).join('.') : '');
    }

    // The sheets the layer built on the body, innermost last: a .layer-scrim
    // whose card is a dialog (a confirm or prompt is an alertdialog).
    function layerSheets() {
        return doc.body.children.filter(c => c.classList.contains('layer-scrim') && c.querySelector('[role="dialog"]'));
    }
    function sheetHost() { return layerSheets().pop() || statics['pd-scrim']; }

    const page = {
        ctx, doc, replaceStates,
        calls: h.calls,
        root: statics['pd-root'],
        // The element an open sheet lives in: the innermost sheet the layer
        // built on the body (shared/layer.js openSheet), else #pd-scrim, the
        // overlay processes.html carries.
        get scrim() { return sheetHost(); },
        pd: name => ctx.__pd(name),
        get S() { return ctx.__pd('S'); },
        // type puts a value in a sheet field, as an engineer typing would.
        type(name, value) { h.typed[name] = value; },
        byId,
        // The sheet's status line as the page last wrote it, or ''.
        status() {
            const host = sheetHost();
            const st = query(host, '[data-layer-status]') || query(host, '.mf .st');
            return st ? st.textContent : '';
        },
        sheetOpen() { return layerSheets().length > 0 || statics['pd-scrim'].classList.contains('active'); },
        // The confirm or prompt the layer put up last (the discard question),
        // or null: its message, its buttons' words, and press(word).
        dialog() {
            const d = doc.body.children.filter(c => c.classList.contains('layer-scrim') && c.querySelector('[role="alertdialog"]')).pop();
            if (!d) return null;
            const buttons = d.querySelectorAll('button');
            return {
                text: (d.querySelector('.layer-msg') || d).textContent,
                buttons: buttons.map(b => b.textContent),
                press: word => { const b = buttons.find(x => x.textContent === word); if (!b) throw new Error('the dialog has no ' + word); b.click(); },
            };
        },
        // pressBackdrop releases on the open sheet's backdrop itself, outside
        // its card, after a press there (ruling 2: that is Escape for the
        // sheet) or, fromCard, a press inside the card (a drag out of a
        // field, which closes nothing). The browser's click lands on the
        // backdrop either way.
        pressBackdrop(fromCard) {
            const host = sheetHost();
            const card = host.querySelector('[role="dialog"]') || host.querySelector('.pd-modal') || host;
            fire(fromCard ? card : host, makeEvent('mousedown'));
            fire(host, makeEvent('click'));
        },
        notices: h.notices,
        // mark/since: the calls one act made.
        // (and the notices shown since that mark, for the act's golden).
        mark() { h.noticeAt[h.calls.length] = h.notices.length; return h.calls.length; },
        noticesSince(i) { return h.notices.slice(h.noticeAt[i] !== undefined ? h.noticeAt[i] : h.notices.length); },
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
            const el = host === 'scrim' ? sheetHost() : statics['pd-root'];
            // A SHEET'S OWN OK AND CANCEL ARE THE LAYER'S BUTTONS when the
            // layer built its shell: a pin names them by the words the page's
            // shell used (sheet-ok, sheet-cancel), and they are pressed, as an
            // engineer presses them (a disabled one takes no click).
            if (host === 'scrim' && el._made && data && /^sheet-(ok|cancel)$/.test(data.act || '') &&
                !el.querySelector('[data-act="' + data.act + '"]')) {
                const b = data.act === 'sheet-ok' ? el.querySelector('[data-layer-ok]')
                    : (el.querySelector('.layer-foot') || el).querySelectorAll('button').find(x => !x.hasAttribute('data-layer-ok'));
                if (!b) throw new Error('the sheet has no ' + data.act + ' button');
                b.click();
                return target;
            }
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
            const ev = makeEvent('keydown', { key: k, target: doc.body });
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
                const host = hostOf(c);
                const stack = [];
                // A built element's own words: its text nodes, or the text
                // a component set on it.
                if (c._made) {
                    const words = c.childNodes.length ? c.childNodes.filter(k => k.nodeType === 3).map(k => k.textContent) : [c._text];
                    for (const w of words) {
                        const t = String(w || '').replace(/\s+/g, ' ').trim();
                        if (t) out.push(host + ' ' + descOf(c) + ': ' + t);
                    }
                }
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
