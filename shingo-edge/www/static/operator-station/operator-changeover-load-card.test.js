// Rendering test for the changeover loading directive banner.
//
// The deliverable is a sentence an operator reads off a board while standing
// at a loader, so it is asserted by RENDERING it. Round 1's changeover banner
// ("1 participant node HAVE no process node configured") and round 2's modal
// geometry both survived review and died the moment they were rendered.
//
// Runs under plain Node (no npm). Exit 0 = pass, 1 = any failure. Run via the
// Go wrapper operator_changeover_load_card_test.go.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
function fail(what, expected, actual) {
    failed++;
    console.error('FAIL: ' + what + '\n   expected: ' + expected + '\n   actual:   ' + actual);
}
function ok(cond, what, expected, actual) {
    if (cond) { passed++; return; }
    fail(what, expected, actual);
}

// Extract a top-level function by brace matching, so the test exercises the
// SHIPPING source rather than a copy that can drift. operator-render.js cannot
// be loaded wholesale — it imports and touches the DOM at module scope — but
// this builder is pure apart from el() and esc().
function extractFn(src, name) {
    const start = src.indexOf('function ' + name + '(');
    if (start < 0) throw new Error('function ' + name + ' not found');
    const open = src.indexOf('{', start);
    let depth = 0;
    for (let j = open; j < src.length; j++) {
        if (src[j] === '{') depth++;
        else if (src[j] === '}') {
            depth--;
            if (depth === 0) return src.slice(start, j + 1);
        }
    }
    throw new Error('unbalanced braces in ' + name);
}

const renderSrc = fs.readFileSync(path.join(__dirname, 'operator-render.js'), 'utf8');
const ctx = vm.createContext({
    console,
    esc: (s) => String(s == null ? '' : s)
        .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'),
    el: (tag, props) => Object.assign({ tagName: tag, innerHTML: '' }, props || {}),
});
vm.runInContext(
    extractFn(renderSrc, 'buildChangeoverLoadDirective') +
    '\nthis.build = buildChangeoverLoadDirective;', ctx);
const build = ctx.build;

// ── the ordinary case ────────────────────────────────────────────────────
{
    const html = build({
        bin_type_codes: ['TOTE-L'],
        payload_codes: ['76683-6TA0A.06'],
        for_nodes: ['PLN_002', 'PLN_004'],
        changeover_id: 12,
    }).innerHTML;

    ok(/TOTE-L/.test(html), 'the bin type is on the card', 'TOTE-L', html);
    ok(/CHANGEOVER/i.test(html), 'the card says why it is there', 'mentions CHANGEOVER', html);
    ok(/LOAD/i.test(html), 'the card states the action', 'mentions LOAD', html);
    ok(/76683-6TA0A\.06/.test(html), 'the payload gives the "for what"', 'the payload code', html);
    ok(/PLN_002/.test(html) && /PLN_004/.test(html),
        'both waiting cells are named', 'PLN_002 and PLN_004', html);

    // THE BIN TYPE IS THE INSTRUCTION and must be the biggest thing on the
    // banner — it is what the operator walks off to fetch. A directive whose
    // part number outweighs its carrier sends them to the wrong rack.
    const sizeOf = (cls) => {
        const m = html.match(new RegExp('class="' + cls + '"'));
        return m ? cls : '';
    };
    ok(sizeOf('os-cld-bintype') === 'os-cld-bintype',
        'the bin type has its own emphasis class', 'os-cld-bintype present', html);
}

// ── two presses onto different dunnage ───────────────────────────────────
{
    const html = build({
        bin_type_codes: ['TOTE-L', 'TOTE-S'],
        payload_codes: ['PART-A', 'PART-B'],
        for_nodes: ['PLN_002', 'PLN_004'],
    }).innerHTML;
    ok(/TOTE-L/.test(html) && /TOTE-S/.test(html),
        'both carrier types are named', 'TOTE-L and TOTE-S', html);
    ok(/TOTE-L\s*\+\s*TOTE-S/.test(html),
        'two types read as two things to fetch, not one hyphenated name',
        'joined with +', html);
}

// ── partial data must not render half a sentence ─────────────────────────
{
    const html = build({ bin_type_codes: ['TOTE-L'] }).innerHTML;
    ok(/TOTE-L/.test(html), 'the instruction survives with no context', 'TOTE-L', html);
    ok(!/for\s*<\/div>/.test(html) && !/waiting:\s*<\/div>/.test(html),
        'no dangling "for" or "waiting:" with nothing after it',
        'context lines omitted entirely', html);
}

// ── escaping ─────────────────────────────────────────────────────────────
{
    const html = build({ bin_type_codes: ['<img src=x onerror=alert(1)>'] }).innerHTML;
    ok(!/<img/.test(html), 'values are escaped, not injected', 'no raw <img', html);
}

if (failed > 0) {
    console.error('\n' + failed + ' failure(s), ' + passed + ' passed');
    process.exit(1);
}
console.log('OK: ' + passed + ' assertions passed');
