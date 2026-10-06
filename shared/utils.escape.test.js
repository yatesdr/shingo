// Pins what the HTML escapers return, on one input table, for every copy that
// exists. Run under plain Node via TestUtilsEscapeJS. Exit 0 on pass, 1 on
// any assertion failure.
//
// The DOM-based copies (a text node in, innerHTML out) run against a fake
// document whose innerHTML follows the HTML serializer's text rule: `&`, NBSP,
// `<` and `>` are escaped and nothing else is. That rule is the whole of what
// those copies did, so this pins the copies, not the fake.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0;
let failed = 0;
function assert(cond, label) {
    if (cond) { passed++; }
    else { failed++; console.error('FAIL: ' + label); }
}

const ROOT = path.join(__dirname, '..');

// The body of `function name(...) {...}` in a file, braces matched.
function extract(file, name) {
    const src = fs.readFileSync(path.join(ROOT, file), 'utf8');
    const at = src.search(new RegExp('function ' + name + '\\s*\\('));
    if (at < 0) return null;
    let depth = 0;
    for (let i = src.indexOf('{', at); i < src.length; i++) {
        if (src[i] === '{') depth++;
        else if (src[i] === '}' && --depth === 0) return src.slice(at, i + 1);
    }
    return null;
}

function serializeText(s) {
    return s.replace(/&/g, '&amp;').replace(/ /g, '&nbsp;')
        .replace(/</g, '&lt;').replace(/>/g, '&gt;');
}
const fakeDocument = {
    createElement: () => ({
        _text: '',
        appendChild(t) { this._text += t.data; },
        set textContent(v) { this._text = String(v); },
        get innerHTML() { return serializeText(this._text); },
    }),
    createTextNode: (s) => ({ data: String(s) }),
};

// Load the named functions from a file into one context, so h finds the
// escapeHtml beside it.
function load(file, names) {
    const ctx = vm.createContext({ document: fakeDocument, String });
    const src = names.map((n) => extract(file, n)).filter(Boolean).join('\n');
    vm.runInContext(src, ctx);
    const out = {};
    names.forEach((n) => { out[n] = vm.runInContext('typeof ' + n + ' === "function" ? ' + n + ' : null', ctx); });
    return out;
}

// input → what every copy returns today.
const TABLE = [
    { in: '& < > " \'', want: '&amp; &lt; &gt; " \'' },
    { in: null, want: '' },
    { in: undefined, want: '' },
    { in: '', want: '' },
    { in: 0, want: '0' },
    { in: 42, want: '42' },
    { in: '&amp;', want: '&amp;amp;' },
    { in: 'O\'Neil "B" <x>', want: 'O\'Neil "B" &lt;x&gt;' },
    { in: 'a b', want: 'a&nbsp;b' },
];

const COPIES = [
    ['shared/utils.js', 'escapeHtml'],
    ['shingo-core/www/static/app.js', 'escapeHtml'],
];

COPIES.forEach(([file, name]) => {
    const fn = load(file, [name])[name];
    assert(fn, file + ' defines ' + name);
    if (!fn) return;
    TABLE.forEach((row) => {
        const got = fn(row.in);
        assert(got === row.want, file + ' ' + name + '(' + JSON.stringify(row.in) + ') = '
            + JSON.stringify(got) + ', want ' + JSON.stringify(row.want));
    });
});

// Core's page-local escapers. The three DOM ones set textContent, so they
// agree with the table above; missions.js's pair is regex and stringifies a
// missing value.
const DOM_LOCAL = [
    ['shingo-core/www/static/pages/dashboard.js', 'esc'],
    ['shingo-core/www/static/pages/dashboard-node-report.js', 'esc'],
    ['shingo-core/www/static/pages/dashboard-map.js', 'escapeText'],
];
DOM_LOCAL.forEach(([file, name]) => {
    const fn = load(file, [name])[name];
    if (!fn) { assert(false, file + ' defines ' + name); return; }
    TABLE.forEach((row) => {
        const got = fn(row.in);
        assert(got === row.want, file + ' ' + name + '(' + JSON.stringify(row.in) + ') = '
            + JSON.stringify(got) + ', want ' + JSON.stringify(row.want));
    });
});
const MISSIONS = 'shingo-core/www/static/pages/missions.js';
const MISSIONS_TABLE = [
    // input, escapeText, escapeAttr
    ['& < > " \'', '&amp; &lt; &gt; " \'', '&amp; &lt; &gt; &quot; &#39;'],
    [null, 'null', 'null'],
    [undefined, 'undefined', 'undefined'],
    ['', '', ''],
    [0, '0', '0'],
    ['&amp;', '&amp;amp;', '&amp;amp;'],
    ['a b', 'a b', 'a b'],
];
(function () {
    const fns = load(MISSIONS, ['escapeText', 'escapeAttr']);
    if (!fns.escapeText || !fns.escapeAttr) { assert(false, MISSIONS + ' defines escapeText/escapeAttr'); return; }
    MISSIONS_TABLE.forEach(([input, text, attr]) => {
        const gt = fns.escapeText(input), ga = fns.escapeAttr(input);
        assert(gt === text, 'missions escapeText(' + JSON.stringify(input) + ') = ' + JSON.stringify(gt) + ', want ' + JSON.stringify(text));
        assert(ga === attr, 'missions escapeAttr(' + JSON.stringify(input) + ') = ' + JSON.stringify(ga) + ', want ' + JSON.stringify(attr));
    });
})();

// h runs every interpolation through escapeHtml, so an attribute built from a
// value carrying a quote is the case that matters.
['shared/utils.js', 'shingo-core/www/static/app.js'].forEach((file) => {
    const fns = load(file, ['escapeHtml', 'h']);
    if (!fns.h) { assert(false, file + ' defines h'); return; }
    const got = fns.h(['<a title="', '">', '</a>'], 'x" onmouseover="y', '<b>');
    const want = '<a title="x" onmouseover="y">&lt;b&gt;</a>';
    assert(got === want, file + ' h attribute: got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
});

console.log('escape: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
