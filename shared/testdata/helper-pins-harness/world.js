// world.js — the page a helper runs in, for the helper pins (pins.js).
//
// A REALM is one browser tab: its own document, window, location, fake clock,
// fetch and EventSource, and its own module instances. The hub modules
// (shingo-core/www/static/app.js, shingo-edge/www/static/js/shingoedge.js) and
// the shared/ modules load UNMODIFIED through a small ES-module loader: each file
// becomes a function whose imports are bound from the realm's module cache and
// whose exports are returned, so a hub's own definition of a name and the
// shared one it imports stay two things, as they are in the browser. When a
// later phase swaps a hub's copy for an import, the hub's line in the goldens
// changes, which is the point.
//
// No jsdom (no new dependency): the DOM here is what the helpers touch —
// elements, text, classList, dataset, style, attributes, the input/select/
// textarea value model, focus, events with capture and bubble, a selector
// engine for compound selectors joined by descendant combinators. innerHTML
// set to a string keeps the string as one raw node (the dialogs write
// '<p>' + escapeHtml(message) + '</p>'; nothing reads inside it), and the
// dump prints it verbatim. A selector the engine cannot read throws, so a
// helper that starts using one fails loudly rather than matching nothing.
//
// The vm context gets its own JS intrinsics (JSON, SyntaxError, TypeError):
// a helper that tests `e instanceof SyntaxError` on a JSON.parse failure (utils.js
// did, before it was deleted) would see
// a SyntaxError from the harness's realm fail that test where the
// browser's passes. Response.json() parses with the context's JSON for the
// same reason.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const REPO = path.resolve(__dirname, '..', '..', '..');

// ── events ───────────────────────────────────────────────────────────────────

class Event {
    constructor(type, init) {
        init = init || {};
        this.type = type;
        this.bubbles = !!init.bubbles;
        this.cancelable = !!init.cancelable;
        this.detail = init.detail === undefined ? null : init.detail;
        this.defaultPrevented = false;
        this.target = null;
        this.currentTarget = null;
        this._stop = false;
        this._stopNow = false;
        for (const k of Object.keys(init)) {
            if (!(k in this)) this[k] = init[k];
        }
    }
    preventDefault() { if (this.cancelable) this.defaultPrevented = true; }
    stopPropagation() { this._stop = true; }
    stopImmediatePropagation() { this._stop = true; this._stopNow = true; }
}
class CustomEvent extends Event {}

class Target {
    constructor() { this._ls = []; }
    addEventListener(type, fn, opts) {
        if (typeof fn !== 'function') return;
        const capture = opts === true || !!(opts && opts.capture);
        if (this._ls.some((l) => l.type === type && l.fn === fn && l.capture === capture)) return;
        this._ls.push({ type, fn, capture, once: !!(opts && opts.once) });
    }
    removeEventListener(type, fn, opts) {
        const capture = opts === true || !!(opts && opts.capture);
        this._ls = this._ls.filter((l) => !(l.type === type && l.fn === fn && l.capture === capture));
    }
    listenerTypes() { return this._ls.map((l) => l.type); }
    // A target outside the tree (window) has no path: its listeners only.
    dispatchEvent(evt) {
        evt.target = this;
        this._fire(evt, 'target');
        return !evt.defaultPrevented;
    }
    // _fire runs this node's listeners for one phase. A property handler
    // (el.onclick = fn, which the Core and Edge dialogs use) runs at target and
    // bubble, after the registered listeners.
    _fire(evt, phase) {
        evt.currentTarget = this;
        for (const l of this._ls.slice()) {
            if (evt._stopNow) return;
            if (l.type !== evt.type) continue;
            if (phase === 'capture' && !l.capture) continue;
            if (phase === 'bubble' && l.capture) continue;
            if (l.once) this.removeEventListener(l.type, l.fn, l.capture);
            l.fn.call(this, evt);
        }
        const prop = this['on' + evt.type];
        if (phase !== 'capture' && typeof prop === 'function' && !evt._stopNow) prop.call(this, evt);
    }
}

// ── nodes ────────────────────────────────────────────────────────────────────

class Node extends Target {
    constructor(doc) {
        super();
        this.ownerDocument = doc;
        this.parentNode = null;
        this.childNodes = [];
    }
    get children() { return this.childNodes.filter((n) => n instanceof Element); }
    get firstChild() { return this.childNodes[0] || null; }
    get lastChild() { return this.childNodes[this.childNodes.length - 1] || null; }
    get firstElementChild() { return this.children[0] || null; }
    get parentElement() { return this.parentNode instanceof Element ? this.parentNode : null; }
    get nextSibling() {
        const p = this.parentNode;
        return p ? p.childNodes[p.childNodes.indexOf(this) + 1] || null : null;
    }
    get previousSibling() {
        const p = this.parentNode;
        return p ? p.childNodes[p.childNodes.indexOf(this) - 1] || null : null;
    }
    appendChild(c) { return this.insertBefore(c, null); }
    insertBefore(c, ref) {
        if (c.parentNode) c.parentNode.removeChild(c);
        const i = ref ? this.childNodes.indexOf(ref) : -1;
        c.parentNode = this;
        if (i < 0) this.childNodes.push(c); else this.childNodes.splice(i, 0, c);
        return c;
    }
    removeChild(c) {
        const i = this.childNodes.indexOf(c);
        if (i >= 0) this.childNodes.splice(i, 1);
        c.parentNode = null;
        return c;
    }
    remove() { if (this.parentNode) this.parentNode.removeChild(this); }
    contains(o) {
        for (let n = o; n; n = n.parentNode) if (n === this) return true;
        return false;
    }
    get isConnected() {
        let n = this;
        while (n.parentNode) n = n.parentNode;
        return n === this.ownerDocument;
    }
    get textContent() { return this.childNodes.map((c) => c.textContent).join(''); }
    set textContent(v) {
        this.childNodes.forEach((c) => { c.parentNode = null; });
        this.childNodes = [];
        const s = v === null || v === undefined ? '' : String(v);
        if (s) this.appendChild(new Text(this.ownerDocument, s));
    }
    dispatchEvent(evt) {
        evt.target = this;
        const pathUp = [];
        for (let n = this.parentNode; n; n = n.parentNode) pathUp.push(n);
        const doc = this.ownerDocument;
        if (pathUp[pathUp.length - 1] === doc && doc._window) pathUp.push(doc._window);
        if (this === doc && doc._window) pathUp.push(doc._window);
        for (let i = pathUp.length - 1; i >= 0 && !evt._stop; i--) pathUp[i]._fire(evt, 'capture');
        if (!evt._stop) this._fire(evt, 'target');
        if (evt.bubbles) {
            for (const n of pathUp) { if (evt._stop) break; n._fire(evt, 'bubble'); }
        }
        return !evt.defaultPrevented;
    }
    querySelectorAll(sel) {
        const groups = parseSelector(sel);
        const out = [];
        const walk = (n) => {
            for (const c of n.childNodes) {
                if (!(c instanceof Element)) continue;
                if (groups.some((g) => matchGroup(c, g))) out.push(c);
                walk(c);
            }
        };
        walk(this);
        return out;
    }
    querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
}

class Text extends Node {
    constructor(doc, data) { super(doc); this.data = String(data); }
    get textContent() { return this.data; }
    set textContent(v) { this.data = String(v); }
}

// RawHTML is what innerHTML = '<p>…</p>' leaves: the string, kept whole.
class RawHTML extends Node {
    constructor(doc, html) { super(doc); this.html = html; }
    get textContent() {
        return this.html.replace(/<[^>]*>/g, '').replace(/&lt;/g, '<').replace(/&gt;/g, '>')
            .replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&amp;/g, '&');
    }
}

function kebab(p) { return String(p).replace(/[A-Z]/g, (m) => '-' + m.toLowerCase()); }

// makeStyle is a CSSStyleDeclaration: camelCase and kebab properties, cssText
// both ways, declarations kept in the order they were set.
function makeStyle() {
    const decl = new Map();
    const api = {
        get cssText() { return Array.from(decl, ([k, v]) => k + ':' + v).join(';'); },
        set cssText(s) {
            decl.clear();
            String(s).split(';').forEach((part) => {
                const i = part.indexOf(':');
                if (i < 0) return;
                const k = part.slice(0, i).trim().toLowerCase();
                if (k) decl.set(k, part.slice(i + 1).trim());
            });
        },
        setProperty(k, v) { if (v === '' || v === null || v === undefined) decl.delete(k); else decl.set(k, String(v)); },
        removeProperty(k) { decl.delete(k); },
        getPropertyValue(k) { return decl.get(k) || ''; },
    };
    return new Proxy(api, {
        get(t, p) {
            if (p in t) return t[p];
            if (typeof p !== 'string') return undefined;
            return decl.get(kebab(p)) || '';
        },
        set(t, p, v) {
            if (p === 'cssText') { t.cssText = v; return true; }
            t.setProperty(kebab(p), v);
            return true;
        },
    });
}

function makeClassList(el) {
    const get = () => el.className.split(/\s+/).filter(Boolean);
    const put = (l) => { if (l.length || el.hasAttribute('class')) el.className = l.join(' '); };
    return {
        add(...cs) { const l = get(); cs.forEach((c) => { if (l.indexOf(c) < 0) l.push(c); }); put(l); },
        remove(...cs) { put(get().filter((c) => cs.indexOf(c) < 0)); },
        contains(c) { return get().indexOf(c) >= 0; },
        toggle(c, force) {
            const want = force === undefined ? !this.contains(c) : !!force;
            if (want) this.add(c); else this.remove(c);
            return want;
        },
        get length() { return get().length; },
        toString() { return el.className; },
    };
}

function makeDataset(el) {
    const attr = (k) => 'data-' + kebab(k);
    return new Proxy({}, {
        get(_, k) { return typeof k === 'string' && el.hasAttribute(attr(k)) ? el.getAttribute(attr(k)) : undefined; },
        set(_, k, v) { el.setAttribute(attr(k), v); return true; },
        has(_, k) { return el.hasAttribute(attr(k)); },
        deleteProperty(_, k) { el.removeAttribute(attr(k)); return true; },
        ownKeys() {
            return Array.from(el._attrs.keys()).filter((a) => a.indexOf('data-') === 0)
                .map((a) => a.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase()));
        },
        getOwnPropertyDescriptor(_, k) {
            return el.hasAttribute(attr(k)) ? { enumerable: true, configurable: true, value: el.getAttribute(attr(k)) } : undefined;
        },
    });
}

const REFLECTED = ['id', 'title', 'placeholder', 'min', 'max', 'name', 'href', 'action', 'role'];

class Element extends Node {
    constructor(doc, tag) {
        super(doc);
        this.tagName = String(tag).toUpperCase();
        this._attrs = new Map();
        this.style = makeStyle();
        this.classList = makeClassList(this);
        this.dataset = makeDataset(this);
        this._value = null;
        this._checked = null;
        this._selectedIndex = 0;
        this.disabled = false;
    }
    getAttribute(k) { k = String(k).toLowerCase(); return this._attrs.has(k) ? this._attrs.get(k) : null; }
    setAttribute(k, v) { this._attrs.set(String(k).toLowerCase(), String(v)); }
    hasAttribute(k) { return this._attrs.has(String(k).toLowerCase()); }
    removeAttribute(k) { this._attrs.delete(String(k).toLowerCase()); }
    get className() { return this.getAttribute('class') || ''; }
    set className(v) { this.setAttribute('class', v); }
    get type() {
        const t = this.getAttribute('type');
        if (t) return t.toLowerCase();
        if (this.tagName === 'INPUT') return 'text';
        if (this.tagName === 'BUTTON') return 'submit';
        if (this.tagName === 'SELECT') return 'select-one';
        if (this.tagName === 'TEXTAREA') return 'textarea';
        return '';
    }
    set type(v) { this.setAttribute('type', v); }
    get value() {
        if (this.tagName === 'SELECT') { const o = this.options[this._selectedIndex]; return o ? o.value : ''; }
        if (this.tagName === 'OPTION') return this.hasAttribute('value') ? this.getAttribute('value') : this.textContent;
        if (this._value !== null) return this._value;
        if (this.tagName === 'TEXTAREA') return this.textContent;
        return this.getAttribute('value') || '';
    }
    set value(v) { this._value = v === null || v === undefined ? '' : String(v); }
    get defaultValue() { return this.tagName === 'TEXTAREA' ? this.textContent : (this.getAttribute('value') || ''); }
    set defaultValue(v) { if (this.tagName === 'TEXTAREA') this.textContent = v; else this.setAttribute('value', v); }
    get checked() { return this._checked !== null ? this._checked : this.hasAttribute('checked'); }
    set checked(v) { this._checked = !!v; }
    get options() { return this.querySelectorAll('option'); }
    get selectedIndex() { return this._selectedIndex; }
    set selectedIndex(v) { this._selectedIndex = Number(v); }
    get innerHTML() { return this.childNodes.map(dumpInline).join(''); }
    set innerHTML(v) {
        this.textContent = '';
        const s = String(v);
        if (s) this.appendChild(new RawHTML(this.ownerDocument, s));
    }
    focus() { this.ownerDocument._active = this; }
    blur() { if (this.ownerDocument._active === this) this.ownerDocument._active = null; }
    click() { this.dispatchEvent(new Event('click', { bubbles: true, cancelable: true })); }
    matches(sel) { return parseSelector(sel).some((g) => matchGroup(this, g)); }
    closest(sel) {
        const groups = parseSelector(sel);
        for (let n = this; n instanceof Element; n = n.parentNode) {
            if (groups.some((g) => matchGroup(n, g))) return n;
        }
        return null;
    }
}
REFLECTED.forEach((k) => {
    Object.defineProperty(Element.prototype, k, {
        get() { return this.getAttribute(k) || ''; },
        set(v) { this.setAttribute(k, v); },
    });
});

class Document extends Node {
    constructor() {
        super(null);
        this.ownerDocument = this;
        this.readyState = 'complete';
        this.visibilityState = 'visible';
        this._active = null;
        this._window = null;
        this.documentElement = new Element(this, 'html');
        this.appendChild(this.documentElement);
        this.head = this.documentElement.appendChild(new Element(this, 'head'));
        this.body = this.documentElement.appendChild(new Element(this, 'body'));
    }
    createElement(tag) { return new Element(this, tag); }
    createTextNode(t) { return new Text(this, t); }
    getElementById(id) { return this.querySelectorAll('*').find((e) => e.id === id) || null; }
    get activeElement() { return this._active && this._active.isConnected ? this._active : this.body; }
}

// ── selectors: tag, #id, .class, [attr], [attr=v], [attr="v"], *, groups
//    joined by commas, compounds joined by descendant combinators ───────────

const selectorCache = new Map();
function parseSelector(sel) {
    if (selectorCache.has(sel)) return selectorCache.get(sel);
    const groups = String(sel).split(',').map((g) => {
        const parts = g.trim().match(/(?:[^\s[\]]+|\[[^\]]*\])+/g) || [];
        return parts.map((p) => {
            const c = { tag: null, id: null, classes: [], attrs: [] };
            const re = /^(\*|[a-zA-Z][\w-]*)|#([\w-]+)|\.([\w-]+)|\[\s*([\w-]+)\s*(?:=\s*(?:"([^"]*)"|'([^']*)'|([^\]\s]*)))?\s*\]/y;
            let i = 0;
            while (i < p.length) {
                re.lastIndex = i;
                const m = re.exec(p);
                if (!m || m.index !== i) throw new Error('stub selector not supported: ' + sel);
                if (m[1] && m[1] !== '*') c.tag = m[1].toUpperCase();
                if (m[2]) c.id = m[2];
                if (m[3]) c.classes.push(m[3]);
                if (m[4]) {
                    const v = m[5] !== undefined ? m[5] : m[6] !== undefined ? m[6] : m[7];
                    c.attrs.push({ name: m[4].toLowerCase(), value: v });
                }
                i = re.lastIndex;
            }
            return c;
        });
    });
    selectorCache.set(sel, groups);
    return groups;
}
function matchCompound(el, c) {
    if (c.tag && el.tagName !== c.tag) return false;
    if (c.id && el.id !== c.id) return false;
    for (const k of c.classes) if (!el.classList.contains(k)) return false;
    for (const a of c.attrs) {
        if (!el.hasAttribute(a.name)) return false;
        if (a.value !== undefined && el.getAttribute(a.name) !== a.value) return false;
    }
    return true;
}
function matchGroup(el, group) {
    let i = group.length - 1;
    if (!matchCompound(el, group[i])) return false;
    let n = el.parentNode;
    for (i--; i >= 0; i--) {
        while (n instanceof Element && !matchCompound(n, group[i])) n = n.parentNode;
        if (!(n instanceof Element)) return false;
        n = n.parentNode;
    }
    return true;
}

// ── the dump: one line per node, children indented ───────────────────────────

function attrsOf(el) {
    const out = [];
    el._attrs.forEach((v, k) => out.push(k + '=' + JSON.stringify(v)));
    const css = el.style.cssText;
    if (css) out.push('style=' + JSON.stringify(css));
    if (el.tagName === 'INPUT' && el._value !== null && el._value !== el.defaultValue) out.push('.value=' + JSON.stringify(el._value));
    if (typeof el.onclick === 'function') out.push('onclick');
    const ls = el.listenerTypes();
    if (ls.length) out.push('listens=' + ls.join(','));
    return out;
}
function dumpInline(n) {
    if (n instanceof Text) return n.data;
    if (n instanceof RawHTML) return n.html;
    return '<' + n.tagName.toLowerCase() + '>' + n.childNodes.map(dumpInline).join('') + '</' + n.tagName.toLowerCase() + '>';
}
function dump(n, indent) {
    indent = indent || '';
    if (n instanceof Text) return indent + JSON.stringify(n.data);
    if (n instanceof RawHTML) return indent + 'html ' + JSON.stringify(n.html);
    const head = indent + n.tagName.toLowerCase() + (attrsOf(n).length ? ' ' + attrsOf(n).join(' ') : '');
    return [head].concat(n.childNodes.map((c) => dump(c, indent + '  '))).join('\n');
}

// ── the clock: every timer a helper arms runs on it ──────────────────────────

function makeClock() {
    const c = { now: 0, timers: [], seq: 0, onError: null };
    c.setTimeout = (fn, ms, ...args) => {
        const t = { id: ++c.seq, at: c.now + Math.max(0, Number(ms) || 0), fn, args };
        c.timers.push(t);
        return t.id;
    };
    c.clearTimeout = (id) => { c.timers = c.timers.filter((t) => t.id !== id); };
    c.setInterval = (fn, ms, ...args) => {
        const id = c.setTimeout(fn, ms, ...args);
        c.timers.find((t) => t.id === id).every = Math.max(1, Number(ms) || 0);
        return id;
    };
    c.clearInterval = c.clearTimeout;
    return c;
}

// flush lets every promise a step started settle (real macrotask turns; the
// page's own timers are on the fake clock and do not move).
async function flush() { for (let i = 0; i < 10; i++) await new Promise((r) => setImmediate(r)); }

async function advance(clock, ms) {
    const end = clock.now + ms;
    await flush();
    for (;;) {
        clock.timers.sort((a, b) => a.at - b.at || a.id - b.id);
        const t = clock.timers[0];
        if (!t || t.at > end) break;
        clock.now = t.at;
        if (t.every) t.at = clock.now + t.every; else clock.timers.shift();
        try { t.fn(...t.args); } catch (e) { if (clock.onError) clock.onError(e); }
        await flush();
    }
    clock.now = end;
    await flush();
}

// ── a realm ──────────────────────────────────────────────────────────────────

// STATIC_ROOTS maps a surface's /static/ prefix to its tree; /static/shared/
// is shared/ on both, as the routers mount it.
const STATIC_ROOTS = {
    core: path.join(REPO, 'shingo-core', 'www', 'static'),
    edge: path.join(REPO, 'shingo-edge', 'www', 'static'),
};

function makeRealm(opts) {
    opts = opts || {};
    const realm = { notes: [], streams: [], reloads: 0, requests: [], answer: null, modules: new Map() };
    const clock = makeClock();
    realm.clock = clock;
    realm.note = (s) => realm.notes.push('t=' + clock.now + ' ' + s);
    clock.onError = (e) => realm.note('timer threw ' + describeThrown(e));

    const doc = new Document();
    doc.readyState = opts.readyState || 'complete';
    realm.doc = doc;
    const location = {
        href: 'http://surface.test/', pathname: '/', search: '',
        reload() { realm.reloads++; realm.note('location.reload()'); },
    };
    const win = new Target();
    win.location = location;
    win.document = doc;
    doc._window = win;
    realm.window = win;

    const realmConsole = {
        log: (...a) => realm.note('console.log ' + a.map(String).join(' ')),
        info: (...a) => realm.note('console.info ' + a.map(String).join(' ')),
        warn: (...a) => realm.note('console.warn ' + a.map(String).join(' ')),
        // An error's own message is the engine's words (they move between node
        // versions); its constructor name and the helper's text are pinned.
        error: (...a) => realm.note('console.error ' + a.map((x) => (isErrorLike(x) ? x.constructor.name : String(x))).join(' ')),
    };

    class FakeEventSource {
        constructor(url, init) {
            this.url = String(url);
            this.withCredentials = !!(init && init.withCredentials);
            this.readyState = 0;
            this._ls = {};
            this.onerror = null;
            this.onopen = null;
            this.onmessage = null;
            this.openedAt = clock.now;
            this.closedAt = null;
            realm.streams.push(this);
            this.n = realm.streams.length;
            realm.note('open #' + this.n + ' ' + this.url + (this.withCredentials ? ' (withCredentials)' : ''));
        }
        addEventListener(type, fn) { (this._ls[type] = this._ls[type] || []).push(fn); }
        removeEventListener(type, fn) { this._ls[type] = (this._ls[type] || []).filter((f) => f !== fn); }
        close() {
            if (this.readyState === 2) return;
            this.readyState = 2;
            this.closedAt = clock.now;
            realm.note('close #' + this.n);
        }
        // ── the server's side, driven by the pins ──
        types() { return Object.keys(this._ls).filter((k) => this._ls[k].length).sort(); }
        open() { this.readyState = 1; if (this.onopen) this.onopen({ type: 'open' }); }
        emit(type, data, id) {
            const ev = { type, data: typeof data === 'string' ? data : JSON.stringify(data), lastEventId: id || '' };
            (this._ls[type] || []).slice().forEach((fn) => fn(ev));
        }
        // fail is a network error: state 0 is the browser retrying by itself
        // (EventSource.CONNECTING), 2 is a fatal one (EventSource.CLOSED).
        fail(state) {
            this.readyState = state === undefined ? 0 : state;
            if (this.onerror) this.onerror({ type: 'error' });
        }
    }
    FakeEventSource.CONNECTING = 0;
    FakeEventSource.OPEN = 1;
    FakeEventSource.CLOSED = 2;
    realm.EventSource = FakeEventSource;
    realm.openStreams = () => realm.streams.filter((s) => s.readyState !== 2);
    realm.lastStream = () => realm.streams[realm.streams.length - 1] || null;

    const fetchStub = (url, init) => {
        realm.requests.push({ url: String(url), init });
        const a = realm.answer ? realm.answer(String(url), init) : { reject: 'no answer configured' };
        if (a.reject) return Promise.reject(new realm.TypeError(a.reject));
        return Promise.resolve(makeResponse(realm, a));
    };

    const ctx = vm.createContext({
        document: doc, window: win, location, navigator: { userAgent: 'node' },
        console: realmConsole,
        setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout,
        setInterval: clock.setInterval, clearInterval: clock.clearInterval,
        fetch: fetchStub, EventSource: FakeEventSource,
        Node, Element, Text, Event, CustomEvent, MouseEvent: Event, KeyboardEvent: Event,
        // shared/http.js gives every request its own deadline; the fetch stub
        // ignores the signal unless a pin answers through its own stub.
        URLSearchParams, AbortController,
    });
    realm.ctx = ctx;
    // window is the page's global in a browser; the names a script reaches
    // through it (htmx-sse.js calls window.setTimeout) are the same objects.
    Object.assign(win, {
        setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout,
        setInterval: clock.setInterval, clearInterval: clock.clearInterval,
        fetch: fetchStub, EventSource: FakeEventSource,
    });
    realm.TypeError = vm.runInContext('TypeError', ctx);
    realm.JSON = vm.runInContext('JSON', ctx);

    realm.surface = opts.surface || 'core';
    realm.resolve = (spec, from) => {
        if (spec.indexOf('/static/shared/') === 0) return path.join(REPO, 'shared', spec.slice('/static/shared/'.length));
        if (spec.indexOf('/static/') === 0) return path.join(STATIC_ROOTS[realm.surface], spec.slice('/static/'.length));
        if (spec[0] === '.') return path.resolve(path.dirname(from), spec);
        return path.join(REPO, spec);
    };
    // load imports one ES module (and what it imports) into this realm, once.
    realm.load = (spec, from) => {
        const file = realm.resolve(spec, from || REPO);
        if (realm.modules.has(file)) return realm.modules.get(file);
        const exportsObj = {};
        realm.modules.set(file, exportsObj);
        const fn = vm.runInContext(moduleWrapper(file), ctx, { filename: file });
        const imp = (s) => realm.load(s, file);
        Object.defineProperties(exportsObj, Object.getOwnPropertyDescriptors(fn(imp)));
        return exportsObj;
    };
    // runScript runs a classic <script> (htmx-sse.js) in the page's global scope.
    realm.runScript = (rel) => {
        const file = path.join(REPO, rel);
        vm.runInContext(fs.readFileSync(file, 'utf8'), ctx, { filename: file });
    };
    realm.advance = (ms) => advance(clock, ms);
    return realm;
}

// makeResponse is a fetch Response whose body reads once, as the browser's.
function makeResponse(realm, a) {
    let used = false;
    const read = () => {
        if (used) return Promise.reject(new realm.TypeError('body stream already read'));
        used = true;
        return Promise.resolve(a.body === undefined ? '' : a.body);
    };
    return {
        ok: a.status >= 200 && a.status < 300,
        status: a.status,
        statusText: '',
        // A followed redirect (a lapsed session's 303 to the login page)
        // answers with the page it landed on.
        redirected: !!a.redirected,
        url: a.url || '',
        headers: { get: (k) => (a.headers || {})[String(k).toLowerCase()] || null },
        get bodyUsed() { return used; },
        text: read,
        json: () => read().then((t) => realm.JSON.parse(t)),
        clone() { return makeResponse(realm, a); },
    };
}

// moduleWrapper turns one ES module's source into a function of its importer:
//   import { a, b as c } from 'X';      → const { a, b: c } = __import('X');
//   export function f / export const v → declared as written, exported by name
//   export { a, b as c };               → exported
//   export { a } from 'X';              → re-exported from X
// Any other import or export form throws, so a hub that changes shape fails
// here instead of loading half.
function moduleWrapper(file) {
    let src = fs.readFileSync(file, 'utf8');
    const exported = [];   // [exportName, expression]
    src = src.replace(/^import\s*\{([\s\S]*?)\}\s*from\s*['"]([^'"]+)['"];?/gm, (_, names, spec) => {
        const binds = names.split(',').map((s) => s.trim()).filter(Boolean).map((s) => {
            const m = /^(\w+)(?:\s+as\s+(\w+))?$/.exec(s);
            if (!m) throw new Error(file + ': import binding the loader does not read: ' + s);
            return m[2] ? m[1] + ': ' + m[2] : m[1];
        });
        return 'const { ' + binds.join(', ') + ' } = __import(' + JSON.stringify(spec) + ');';
    });
    src = src.replace(/^export\s*\{([^}]*)\}\s*from\s*['"]([^'"]+)['"];?/gm, (_, names, spec) => {
        names.split(',').map((s) => s.trim()).filter(Boolean).forEach((s) => {
            const m = /^(\w+)(?:\s+as\s+(\w+))?$/.exec(s);
            exported.push([m[2] || m[1], '__import(' + JSON.stringify(spec) + ').' + m[1]]);
        });
        return '';
    });
    src = src.replace(/^export\s*\{([^}]*)\};?/gm, (_, names) => {
        names.split(',').map((s) => s.trim()).filter(Boolean).forEach((s) => {
            const m = /^(\w+)(?:\s+as\s+(\w+))?$/.exec(s);
            exported.push([m[2] || m[1], m[1]]);
        });
        return '';
    });
    src = src.replace(/^export\s+((?:async\s+)?function\s*\*?\s*|const\s+|let\s+|var\s+|class\s+)(\w+)/gm, (_, kw, name) => {
        exported.push([name, name]);
        return kw + name;
    });
    if (/^\s*import[\s{*]/m.test(src) || /^\s*export\s/m.test(src)) {
        throw new Error(file + ': an import or export form the loader does not read');
    }
    const getters = exported.map(([n, e]) => 'get ' + JSON.stringify(n) + '() { return ' + e + '; }');
    return '(function (__import) {\n"use strict";\n' + src + '\n;return {' + getters.join(', ') + '};\n})';
}

// isErrorLike is true for an Error of either realm (instanceof would miss the
// page's own).
function isErrorLike(v) { return !!v && typeof v === 'object' && 'stack' in v && 'message' in v; }

// describeThrown names a thrown value: a string as itself, an Error by its
// constructor (the engine's message text moves between node versions; a
// message the harness made itself is kept).
function describeThrown(v) {
    if (typeof v === 'string') return 'string ' + JSON.stringify(v);
    if (v === null || v === undefined) return String(v);
    if (isErrorLike(v)) {
        return v.constructor.name + (v.__harness ? ' ' + JSON.stringify(v.message) : '');
    }
    try { return typeof v + ' ' + JSON.stringify(v); } catch (_) { return typeof v; }
}

module.exports = {
    REPO, Event, CustomEvent, Element, Text, RawHTML, makeRealm, dump, flush, describeThrown,
};
