// Pins what escapeHtml and h return, on one input table. Run under plain Node
// via TestUtilsEscapeJS. Exit 0 on pass, 1 on any assertion failure.
//
// History: until 2026-10-05 there were two copies of escapeHtml (this file's
// and Core app.js's) plus five page-local escapers in Core, all DOM-based
// (text node in, innerHTML out), so `"` and `'` came through unescaped and
// h`` let an attribute built from a name or note be broken out of. The pin
// commit before the fix held the old outputs for every copy; the table below
// is the one helper that is left. That only one is left is
// shingo-core/www/clock_globals_drift_test.go TestEscapeHtmlDefinedOnce.

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

// The body of `function name(...) {...}` in utils.js, braces matched.
const SRC = fs.readFileSync(path.join(__dirname, 'utils.js'), 'utf8');
function extract(name) {
    const at = SRC.search(new RegExp('function ' + name + '\\s*\\('));
    if (at < 0) return null;
    let depth = 0;
    for (let i = SRC.indexOf('{', at); i < SRC.length; i++) {
        if (SRC[i] === '{') depth++;
        else if (SRC[i] === '}' && --depth === 0) return SRC.slice(at, i + 1);
    }
    return null;
}
const escTable = SRC.match(/const HTML_ESCAPES = [^\n]*\n/);
assert(escTable, 'utils.js defines HTML_ESCAPES');

// No document in the sandbox: the helper must not need one.
const ctx = vm.createContext({ String });
vm.runInContext((escTable ? escTable[0] : '') + extract('escapeHtml') + '\n' + extract('h'), ctx);
const escapeHtml = vm.runInContext('escapeHtml', ctx);
const h = vm.runInContext('h', ctx);

// input → output. "was" is what the DOM copies returned (the pin before the
// fix); a row where they differ is the change.
const TABLE = [
    { in: '& < > " \'', want: '&amp; &lt; &gt; &quot; &#39;', was: '&amp; &lt; &gt; " \'' },
    { in: null, want: '' },
    { in: undefined, want: '' },
    { in: '', want: '' },
    { in: 0, want: '0' },
    { in: 42, want: '42' },
    { in: '&amp;', want: '&amp;amp;' },
    { in: 'O\'Neil "B" <x>', want: 'O&#39;Neil &quot;B&quot; &lt;x&gt;', was: 'O\'Neil "B" &lt;x&gt;' },
    // The serializer wrote NBSP as &nbsp;; raw NBSP renders the same.
    { in: 'a b', want: 'a b', was: 'a&nbsp;b' },
];
TABLE.forEach((row) => {
    const got = escapeHtml(row.in);
    assert(got === row.want, 'escapeHtml(' + JSON.stringify(row.in) + ') = '
        + JSON.stringify(got) + ', want ' + JSON.stringify(row.want));
});

// h runs every interpolation through escapeHtml, so an attribute built from a
// value carrying a quote is the case that matters.
const got = h(['<a title="', '">', '</a>'], 'x" onmouseover="y', '<b>');
const want = '<a title="x&quot; onmouseover=&quot;y">&lt;b&gt;</a>';
assert(got === want, 'h attribute: got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
assert(h(['<p>', '</p>'], ['<i>a</i>', '<i>b</i>']) === '<p><i>a</i><i>b</i></p>', 'h joins arrays unescaped');
assert(h(['<p>', '</p>'], { __html: true, value: '<b>x</b>' }) === '<p><b>x</b></p>', 'h __html opt-out');
assert(h(['<p>', '</p>'], false) === '<p></p>', 'h drops false');

// Inventory's search highlight (shingo-core/www/static/pages/inventory.js hl)
// is the caller the quote fix would have made worse. It used to escape first
// and highlight the escaped string, so a search for "amp" or "lt" put the mark
// inside an entity ("R&<mark>amp</mark>;D"), and with quotes escaped "quot"
// would have joined them. It now splits the RAW text and escapes each piece.
(function () {
    const inv = fs.readFileSync(path.join(__dirname, '..', 'shingo-core', 'www', 'static', 'pages', 'inventory.js'), 'utf8');
    const at = inv.search(/function hl\s*\(/);
    let body = null;
    for (let i = inv.indexOf('{', at), depth = 0; at >= 0 && i < inv.length; i++) {
        if (inv[i] === '{') depth++;
        else if (inv[i] === '}' && --depth === 0) { body = inv.slice(at, i + 1); break; }
    }
    assert(body, 'inventory.js defines hl');
    if (!body) return;
    vm.runInContext(body + '\nvar searchTerm = "";', ctx);
    const MARK = (s) => '<mark class="inv-hit">' + s + '</mark>';
    [
        ['', 'R&D "x"', 'R&amp;D &quot;x&quot;'],
        ['amp', 'R&D amp', 'R&amp;D ' + MARK('amp')],
        ['quot', 'say "quot"', 'say &quot;' + MARK('quot') + '&quot;'],
        ['lt', 'a<b lt', 'a&lt;b ' + MARK('lt')],
        ['AMP', 'Amp', MARK('Amp')],
        ['a.b', 'axb a.b', 'axb ' + MARK('a.b')],
    ].forEach(([term, text, want]) => {
        vm.runInContext('searchTerm = ' + JSON.stringify(term), ctx);
        const got = vm.runInContext('hl(' + JSON.stringify(text) + ')', ctx);
        assert(got === want, 'inventory hl(' + JSON.stringify(text) + ') searching ' + JSON.stringify(term)
            + ' = ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
    });
})();

console.log('escape: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
