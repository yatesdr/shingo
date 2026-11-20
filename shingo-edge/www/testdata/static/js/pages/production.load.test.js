// production.load.test.js — the Production page's LOAD modal
// (production.js onLoadPayloadChanged) fills each manifest line's count with a
// full bin of the part, by protocol.ManifestItem's count rule: a template of
// ONE line counts pieces, so its default is the standard pack itself, never the
// pack times an assembly's pieces per cycle; a kit's lines are each the pack
// times their ratio, as they always were.
//
// The function is lifted out of production.js by name (the page wires itself
// when it loads) and run in a vm with a stubbed DOM and transport, as
// ask-pending.test.js runs the page's submits.

const fs = require('fs');
const path = require('path');
const vm = require('vm');

// The page code under test. This file lives in www/testdata, outside the
// embedded static tree, so no binary carries or serves it
// (TestStaticTree_HoldsNoTestJS); the code it reads stays in www/static.
const SRC_DIR = path.join(__dirname, '..', '..', '..', '..', 'static', 'js', 'pages');

let failures = 0;
let passed = 0;

function eq(what, got, want) {
    const g = JSON.stringify(got), w = JSON.stringify(want);
    if (g === w) { passed++; return; }
    failures++;
    console.log(`FAIL: ${what}\n  expected: ${w}\n  actual:   ${g}`);
}

// extractFn returns the source of the function named name in src: from its
// declaration to the brace that closes it.
function extractFn(src, name) {
    const m = new RegExp('(async\\s+)?function\\s+' + name + '\\s*\\(').exec(src);
    if (!m) throw new Error('no function ' + name);
    let depth = 0;
    for (let i = src.indexOf('{', m.index); i < src.length; i++) {
        if (src[i] === '{') depth++;
        else if (src[i] === '}' && --depth === 0) return src.slice(m.index, i + 1);
    }
    throw new Error('unclosed function ' + name);
}

// COUNT is shared/count.js, real: the line counts are its linePieces.
const COUNT = (() => {
    const c = vm.createContext({});
    for (const f of ['count.js', 'count-words.js']) {
        vm.runInContext(fs.readFileSync(path.join(__dirname, '..', '..', '..', '..', '..', '..', 'shared', f), 'utf8')
            .replace(/^import\s[^;]*;[ \t]*$/mg, '').replace(/^export const /mg, 'var ').replace(/^export (?=function )/mg, ''), c);
    }
    return c;
})();

// defaults opens the modal on a part whose manifest read answers data, and
// returns each line's default count, keyed by part number.
async function defaults(data) {
    const src = fs.readFileSync(path.join(SRC_DIR, 'production.js'), 'utf8');
    const rows = { innerHTML: '', children: [], appendChild(c) { this.children.push(c); return c; } };
    const els = { 'rb-payload-code': { value: '' }, 'rb-manifest-rows': rows };
    const ctx = vm.createContext({
        document: {
            getElementById: (id) => els[id] || null,
            createElement: () => ({ style: {}, innerHTML: '' }),
        },
        rbPicker: { value: () => 'SYN-PL-A' },
        linePieces: COUNT.linePieces, uopRatio: COUNT.uopRatio, countBoxLabel: COUNT.countBoxLabel, typedNote: COUNT.typedNote,
        api: { get: async () => data },
        escapeHtml: (s) => String(s == null ? '' : s),
        encodeURIComponent, console, Promise,
    });
    vm.runInContext(extractFn(src, 'onLoadPayloadChanged') + '\n;globalThis.__fn = onLoadPayloadChanged;', ctx);
    await ctx.__fn();
    const out = {};
    for (const c of rows.children) {
        const m = /value="([^"]*)" data-part="([^"]*)"/.exec(c.innerHTML);
        if (m) out[m[2]] = m[1];
    }
    return out;
}

async function main() {
    eq('one line at 2 pieces per assembly cycle: the default is the standard pack, 24, not 24 x 2',
        await defaults({ uop_capacity: 24, items: [{ part_number: 'SYN-PL-A1', parts_per_cycle: 2 }] }),
        { 'SYN-PL-A1': '24' });
    eq('a kit of two lines: each line is the pack times its ratio, as it always was',
        await defaults({ uop_capacity: 100, items: [
            { part_number: 'SYN-PL-L', parts_per_cycle: 2 }, { part_number: 'SYN-PL-R', parts_per_cycle: 1 }] }),
        { 'SYN-PL-L': '200', 'SYN-PL-R': '100' });

    if (failures > 0) {
        console.log(`production load: ${passed} passed, ${failures} FAILED`);
        process.exit(1);
    }
    console.log(`production load: ${passed} passed`);
}

main().catch((e) => { console.log('FAIL harness: ' + (e && e.stack || e)); process.exit(1); });
