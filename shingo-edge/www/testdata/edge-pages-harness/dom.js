// dom.js — the smallest DOM the Manual order, Production and Kafka Direct
// pages run on: elements parsed from the markup their handlers rendered, and
// from the markup the page scripts write into innerHTML.
//
// WHAT IT MODELS, because the pins read it:
//   - <select>: options in document order (optgroups looked through), value
//     and selectedIndex as a browser answers them — the first option when none
//     is selected, -1 (value '') after value is set to one no option has,
//     until an option is added or removed (the selectedness algorithm runs
//     again and the first option is taken).
//   - <input>: value (the attribute until written), defaultValue, type,
//     checked.
//   - style (display, cssText, any property), classList, dataset (data-*
//     attributes, camel-cased), textContent, innerHTML (the string as written,
//     until a child is added or removed; serialised after).
//   - getElementById over the document's tree, querySelector(All) for
//     comma-separated compound selectors: tag, #id, .class, [attr], [attr="v"].
//
// WHAT IT DOES NOT: events (the pins call the page's functions themselves),
// layout, scripts in markup, descendant combinators.

'use strict';

const VOID = new Set(['area', 'base', 'br', 'col', 'embed', 'hr', 'img', 'input', 'link', 'meta', 'source', 'track', 'wbr']);

const ENTITIES = { amp: '&', lt: '<', gt: '>', quot: '"', apos: "'", nbsp: ' ', times: '×' };
function decode(s) {
    return String(s).replace(/&(#x[0-9a-fA-F]+|#\d+|[a-zA-Z]+);/g, (m, e) => {
        if (e[0] === '#') return String.fromCodePoint(e[1] === 'x' ? parseInt(e.slice(2), 16) : parseInt(e.slice(1), 10));
        return Object.prototype.hasOwnProperty.call(ENTITIES, e) ? ENTITIES[e] : m;
    });
}
function escText(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }
function escAttr(s) { return escText(s).replace(/"/g, '&quot;'); }
const camel = s => s.replace(/-([a-z])/g, (_, c) => c.toUpperCase());

class Text {
    constructor(text) { this.nodeType = 3; this.tagName = '#text'; this.text = text; this.parentNode = null; }
    get textContent() { return this.text; }
    remove() { if (this.parentNode) this.parentNode._detach(this); }
}

class Style {
    constructor() { this.display = ''; }
    get cssText() { return this._css || ''; }
    set cssText(v) {
        this._css = String(v);
        String(v).split(';').forEach(d => {
            const i = d.indexOf(':');
            if (i > 0) this[camel(d.slice(0, i).trim())] = d.slice(i + 1).trim();
        });
    }
}

class Element {
    constructor(tag, doc) {
        this.nodeType = 1;
        this.tagName = String(tag).toUpperCase();
        this.ownerDocument = doc;
        this.childNodes = [];
        this.parentNode = null;
        this.attrs = {};
        this.dataset = {};
        this.style = new Style();
        this._raw = null;
        this._value = undefined;
        this._checked = undefined;
        this._none = false;
        this.selected = false;
        this.disabled = false;
        this.listeners = {};
        const self = this;
        this.classList = {
            add(...cs) { const l = self._classes(); cs.forEach(c => { if (!l.includes(c)) l.push(c); }); self.className = l.join(' '); },
            remove(...cs) { self.className = self._classes().filter(c => !cs.includes(c)).join(' '); },
            contains(c) { return self._classes().includes(c); },
            toggle(c, on) { const want = on === undefined ? !this.contains(c) : !!on; if (want) this.add(c); else this.remove(c); return want; },
        };
    }
    _classes() { return String(this.attrs.class || '').split(/\s+/).filter(Boolean); }
    get className() { return this.attrs.class || ''; }
    set className(v) { this.attrs.class = String(v); }
    get id() { return this.attrs.id || ''; }
    set id(v) { this.attrs.id = String(v); }
    setAttribute(k, v) {
        k = String(k).toLowerCase();
        this.attrs[k] = String(v);
        if (k.startsWith('data-')) this.dataset[camel(k.slice(5))] = String(v);
        if (k === 'style') this.style.cssText = v;
        if (k === 'selected') this.selected = true;
        if (k === 'disabled') this.disabled = true;
    }
    getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; }
    hasAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k); }
    addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); }
    removeEventListener() {}

    get children() { return this.childNodes.filter(n => n.nodeType === 1); }
    get firstChild() { return this.childNodes[0] || null; }
    _mutated() {
        this._raw = null;
        for (let n = this; n; n = n.parentNode) if (n.tagName === 'SELECT') { n._none = false; n._raw = null; }
    }
    _detach(c) {
        const i = this.childNodes.indexOf(c);
        if (i >= 0) this.childNodes.splice(i, 1);
        c.parentNode = null;
        this._mutated();
    }
    appendChild(c) { return this.insertBefore(c, null); }
    insertBefore(c, ref) {
        if (c.parentNode) c.parentNode._detach(c);
        const i = ref ? this.childNodes.indexOf(ref) : -1;
        if (i < 0) this.childNodes.push(c); else this.childNodes.splice(i, 0, c);
        c.parentNode = this;
        this._mutated();
        return c;
    }
    remove() { if (this.parentNode) this.parentNode._detach(this); }
    closest(sel) {
        for (let n = this; n && n.nodeType === 1; n = n.parentNode) if (matches(n, sel)) return n;
        return null;
    }

    get textContent() { return this.childNodes.map(n => n.textContent).join(''); }
    set textContent(v) {
        this.childNodes.forEach(n => { n.parentNode = null; });
        this.childNodes = v === '' || v == null ? [] : [new Text(String(v))];
        this._mutated();
        if (this.childNodes[0]) this.childNodes[0].parentNode = this;
    }
    get innerHTML() {
        if (this._raw != null) return this._raw;
        return this.childNodes.map(serialise).join('');
    }
    set innerHTML(v) {
        this.childNodes.forEach(n => { n.parentNode = null; });
        this.childNodes = [];
        parseInto(this, String(v), this.ownerDocument);
        this._mutated();
        this._raw = String(v);
    }

    descendants() {
        const out = [];
        (function walk(n) { n.childNodes.forEach(c => { if (c.nodeType === 1) { out.push(c); walk(c); } }); })(this);
        return out;
    }
    querySelectorAll(sel) { return this.descendants().filter(n => matches(n, sel)); }
    querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }

    // <select>
    get options() { return this.descendants().filter(n => n.tagName === 'OPTION'); }
    get selectedIndex() {
        const opts = this.options;
        let at = -1;
        opts.forEach((o, i) => { if (o.selected) at = i; });
        if (at >= 0) return at;
        return this._none || opts.length === 0 ? -1 : 0;
    }
    set selectedIndex(i) {
        this.options.forEach((o, j) => { o.selected = j === i; });
        this._none = !(i >= 0 && i < this.options.length);
    }

    get type() {
        if (this.tagName === 'SELECT') return 'select-one';
        if (this.tagName === 'INPUT') return (this.attrs.type || 'text').toLowerCase();
        if (this.tagName === 'BUTTON') return (this.attrs.type || 'submit').toLowerCase();
        return this.attrs.type || '';
    }
    set type(v) { this.attrs.type = String(v); }
    get defaultValue() { return this.attrs.value || ''; }
    get value() {
        if (this.tagName === 'SELECT') {
            const o = this.options[this.selectedIndex];
            return o ? o.value : '';
        }
        if (this.tagName === 'OPTION') return this.hasAttribute('value') ? this.attrs.value : this.textContent.trim();
        if (this._value !== undefined) return this._value;
        return this.attrs.value || '';
    }
    set value(v) {
        v = v == null ? '' : String(v);
        if (this.tagName === 'SELECT') {
            const opts = this.options;
            const i = opts.findIndex(o => o.value === v);
            opts.forEach((o, j) => { o.selected = j === i; });
            this._none = i < 0;
            return;
        }
        if (this.tagName === 'OPTION') { this.attrs.value = v; return; }
        this._value = v;
    }
    get checked() { return this._checked !== undefined ? this._checked : this.hasAttribute('checked'); }
    set checked(v) { this._checked = !!v; }
}

// matches answers a comma-separated list of compound selectors.
function matches(el, sel) {
    return String(sel).split(',').some(part => {
        const s = part.trim();
        if (!s) return false;
        if (/\s/.test(s)) throw new Error('dom.js: no descendant combinators (' + s + ')');
        const re = /([.#]?[\w-]+)|\[([\w-]+)(?:="([^"]*)")?\]/g;
        let m, ok = true, any = false;
        while ((m = re.exec(s)) !== null) {
            any = true;
            if (m[2]) { ok = ok && el.hasAttribute(m[2]) && (m[3] === undefined || el.attrs[m[2]] === m[3]); continue; }
            const t = m[1];
            if (t[0] === '.') ok = ok && el.classList.contains(t.slice(1));
            else if (t[0] === '#') ok = ok && el.id === t.slice(1);
            else ok = ok && el.tagName === t.toUpperCase();
        }
        return any && ok;
    });
}

function serialise(n) {
    if (n.nodeType === 3) return escText(n.text);
    const attrs = Object.keys(n.attrs).map(k => ' ' + k + '="' + escAttr(n.attrs[k]) + '"').join('');
    const tag = n.tagName.toLowerCase();
    if (VOID.has(tag)) return '<' + tag + attrs + '>';
    return '<' + tag + attrs + '>' + n.childNodes.map(serialise).join('') + '</' + tag + '>';
}

// parseInto builds markup into parent: a tag tokenizer, not an HTML parser.
// An end tag closes the nearest open element of its name; one with none open
// is dropped, as a browser drops it. A quote opens a value only straight after
// `=`, as a browser's tokenizer reads it, so an unescaped quote inside a value
// ends that value and the tag ends at the next `>` (manual-message.js
// renderField's markup, census bug 9).
function parseInto(parent, html, doc) {
    html = html.replace(/<!--[\s\S]*?-->/g, '');
    const stack = [parent];
    const top = () => stack[stack.length - 1];
    const re = /<\/(\w+)\s*>|<(\w+)((?:=\s*"[^"]*"|=\s*'[^']*'|=(?!\s*["'])|[^>=])*?)\s*(\/?)>/g;
    let at = 0, m;
    const text = s => { if (s) top().childNodes.push(Object.assign(new Text(decode(s)), { parentNode: top() })); };
    while ((m = re.exec(html)) !== null) {
        text(html.slice(at, m.index));
        at = re.lastIndex;
        if (m[1]) {
            const name = m[1].toUpperCase();
            const i = stack.map(n => n.tagName).lastIndexOf(name);
            if (i > 0) stack.length = i;
            continue;
        }
        const el = new Element(m[2], doc);
        const are = /([^\s=]+)(?:\s*=\s*("([^"]*)"|'([^']*)'|([^\s"'>]+)))?/g;
        let a;
        while ((a = are.exec(m[3])) !== null) {
            const v = a[3] !== undefined ? a[3] : a[4] !== undefined ? a[4] : a[5] !== undefined ? a[5] : '';
            el.setAttribute(a[1], decode(v));
        }
        el.parentNode = top();
        top().childNodes.push(el);
        if (!VOID.has(m[2].toLowerCase()) && !m[4]) stack.push(el);
    }
    text(html.slice(at));
}

// Document is one page: its body holds the parsed markup.
class Document {
    constructor(html) {
        this.body = new Element('body', this);
        if (html) parseInto(this.body, html, this);
    }
    createElement(tag) { return new Element(tag, this); }
    createTextNode(t) { return new Text(String(t)); }
    getElementById(id) { return this.body.descendants().find(n => n.id === id) || null; }
    querySelectorAll(sel) { return this.body.querySelectorAll(sel); }
    querySelector(sel) { return this.body.querySelector(sel); }
    addEventListener() {}
}

module.exports = { Document, Element, decode };
