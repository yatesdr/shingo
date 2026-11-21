// lend-notice.js — the notice (shared/notice.js), real, for a station test that
// builds its vm context by hand (imports dropped, names lent): the toasts the
// station shows and the two banners it calls, drawn by the code the kiosk runs.
//
//   require('./lend-notice.js')(ctx);          // ctx: the station's vm context
//   require('./lend-notice.js')(ctx, { timers }); // the lifetimes on other timers
//
// A SCOPE OF ITS OWN. notice.js builds with shared/dom.js's el, and the
// station's modules export an el of their own (operator-util.js), so loaded
// into the station's one scope notice.js would build with the station's. Here
// dom.js, clock.js and notice.js run in a context of their own that shares
// only the page with the station's: its document, timers and window. The
// station's scope gets notice.js's exports, as its import binds them.
//
// THE FAKE ELEMENT GETS isConnected when it has none: notice.js makes its
// region again once the old one has left the page (a test that empties the
// body), and asks the region whether it is still on it.
//
// Not a test itself: it sits in testdata, outside the module's embed.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SHARED = path.join(__dirname, '..', '..', '..', '..', '..', 'shared');

// load runs one shared module in into: imports dropped (into holds the names
// they bind), exports published on into.
function load(into, name) {
    let src = fs.readFileSync(path.join(SHARED, name), 'utf8');
    src = src.replace(/^import\s[\s\S]*?from\s+['"][^'"]+['"];?[ \t]*$/gm, '');
    const exported = [];
    src = src.replace(/^export\s+(async\s+function|function|const|let)\s+(\w+)/gm, function (_, kw, n) {
        exported.push(n);
        return kw + ' ' + n;
    });
    const wrapped = '(function () {\n' + src + '\nreturn {' + exported.map(n => n + ': ' + n).join(', ') + '};\n})()';
    const out = vm.runInContext(wrapped, into, { filename: name });
    Object.assign(into, out);
    return out;
}

module.exports = function lendNotice(ctx, opts) {
    const timers = (opts && opts.timers) || ctx;
    const doc = ctx.document;
    const Element = doc.createElement('div').constructor;
    if (!('isConnected' in Element.prototype)) {
        Object.defineProperty(Element.prototype, 'isConnected', {
            get() {
                for (let n = this; n; n = n.parentNode) {
                    if (n === doc.body) return true;
                    if (n.parentNode && n.parentNode.children.indexOf(n) < 0) return false;
                }
                return false;
            },
        });
    }
    const own = vm.createContext({
        console, JSON, Math, Number, Array, Object, String, Promise, Error, RegExp, Map, Set, WeakMap, Intl,
        Date: ctx.Date || Date,
        document: doc,
        window: ctx.window || {},
        // The page's timers, read at each call (a test that swaps its clock
        // still runs the toasts' lifetimes on it), or opts.timers.
        setTimeout: function (fn, ms) { return timers.setTimeout(fn, ms); },
        clearTimeout: function (id) { return timers.clearTimeout(id); },
        Node: Element,
    });
    load(own, 'dom.js');
    load(own, 'clock.js');
    const notice = load(own, 'notice.js');
    Object.assign(ctx, notice);
    return notice;
};
