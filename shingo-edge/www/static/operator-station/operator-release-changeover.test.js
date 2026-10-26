// Unit tests for the changeover release screen (operator-release.js):
// isChangeoverRelease and releasePromptStep1HTML.
//
// WHAT THIS PINS. A changeover's release is answered on the same screen as a
// steady-state one, and that screen's main button pulls the cart's whole count
// to the bench. At a changeover the cutover freezes every bench pile, so those
// parts were never counted again while the cart was wiped as empty and came
// back as new stock. A changeover release must not offer the pull at all; the
// cart goes back with its count, or is declared empty. The steady-state screen
// keeps PULL PARTS.
//
// Runs under plain Node (no npm). Exit 0 = pass, 1 = any failure. Run via the
// Go wrapper operator_release_changeover_test.go.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
function check(cond, label, detail) {
    if (cond) { passed++; return; }
    failed++;
    console.error('FAIL: ' + label + (detail ? '\n   ' + detail : ''));
}

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

// operator-release.js imports modules and touches the DOM at module scope, so
// lift the two pure functions out and give them the escape they import.
const src = fs.readFileSync(path.join(__dirname, 'operator-release.js'), 'utf8');
const ctx = vm.createContext({ esc: (s) => String(s) });
vm.runInContext(
    extractFn(src, 'isChangeoverRelease') + '\n' +
    extractFn(src, 'releasePromptStep1HTML') + '\n' +
    'this.isChangeoverRelease = isChangeoverRelease;\n' +
    'this.releasePromptStep1HTML = releasePromptStep1HTML;', ctx);
const { isChangeoverRelease, releasePromptStep1HTML } = ctx;

// ── which release is a changeover's ──────────────────────────────────────

const coURL = '/api/processes/3/changeover/release?node_id=12&purpose=ready';
check(isChangeoverRelease(coURL) === true, 'the tile\'s RELEASE: READY url is a changeover release');
check(isChangeoverRelease('/api/processes/3/changeover/release') === true, 'the changeover release url with no query');
check(isChangeoverRelease('/api/process-nodes/12/release-staged') === false, 'the steady-state pair release is not');
check(isChangeoverRelease('/api/orders/41/release') === false, 'the per-order release is not');
check(isChangeoverRelease('/api/processes/3/changeover/release-all') === false, 'a longer path that only starts the same is not');

// ── what each screen offers ──────────────────────────────────────────────

const state = (changeover, uop) => ({
    changeover,
    payloads: ['SYN-PART'],
    selected: { 'SYN-PART': uop },
    entry: { runtime: { remaining_uop_cached: uop } },
    partialCount: uop,
});

const steady = releasePromptStep1HTML(state(false, 33));
check(steady.includes('data-action="release-submit-parts"'), 'steady state with parts left offers PULL PARTS');
check(steady.includes('data-action="release-pick:SYN-PART"'), 'steady state shows the part chips');

const co = releasePromptStep1HTML(state(true, 33));
check(!co.includes('release-submit-parts'), 'a changeover release with parts left offers no PULL PARTS', co);
check(!co.includes('release-pick:'), 'a changeover release shows no part chips to pull', co);
check(co.includes('RELEASE PARTIAL') && co.includes('data-action="release-partial-edit">33 UoP'),
    'a changeover release sends the cart back with its count', co);
check(co.includes('BIN EMPTY (UNDER COUNT)'), 'a changeover release can still declare the bin empty', co);
check(co.includes('CANCEL'), 'a changeover release can be cancelled', co);

const coEmpty = releasePromptStep1HTML(state(true, 0));
check(coEmpty.includes('RELEASE EMPTY') && !coEmpty.includes('release-submit-parts'),
    'a changeover release at 0 offers RELEASE EMPTY and no pull', coEmpty);

console.log(passed + ' passed, ' + failed + ' failed');
process.exit(failed === 0 ? 0 : 1);
