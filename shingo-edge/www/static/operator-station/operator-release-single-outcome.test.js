// Unit tests for singleOutcomeReleaseBody (operator-release.js) — the guard
// that decides whether RELEASE opens the disposition modal at all.
//
// WHAT THIS PINS. The release prompt exists to collect a disposition. In two
// shapes there is none to collect and it rendered a single actionable button
// anyway, so an operator tapped RELEASE and was asked to tap RELEASE FULL /
// RELEASE EMPTY — one action, two taps at the same spot on the screen. Both
// plants reported it. This pins WHICH shapes skip the modal, and — more
// important — which ones must keep it, because collapsing a prompt that holds
// a real choice deletes the choice silently.
//
// The bodies are asserted field by field rather than deep-equal against a
// literal built here: the whole point is that the fast path posts what the
// modal's button posted, so the assertion has to name the wire fields.
//
// Runs under plain Node (no npm). Exit 0 = pass, 1 = any failure. Run via the
// Go wrapper operator_release_single_outcome_test.go.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
function eq(got, want, label) {
    if (got === want) { passed++; return; }
    failed++;
    console.error('FAIL: ' + label + '\n   got:  ' + JSON.stringify(got) + '\n   want: ' + JSON.stringify(want));
}
function truthy(got, label) {
    if (got) { passed++; return; }
    failed++;
    console.error('FAIL: ' + label + '\n   got: ' + JSON.stringify(got));
}

function extractFn(src, name) {
    const start = src.indexOf('function ' + name + '(');
    if (start < 0) throw new Error('function ' + name + ' not found');
    const open = src.indexOf('{', start);
    if (open < 0) throw new Error('no body for ' + name);
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

// operator-release.js touches document at module scope, so lift the three
// pure functions out rather than evaluating the module.
const src = fs.readFileSync(path.join(__dirname, 'operator-release.js'), 'utf8');
const ctx = vm.createContext({ console: console, getView: () => ({ station: { name: 'P400' } }) });
vm.runInContext(
    extractFn(src, 'calledByFromView') + '\n' +
    extractFn(src, 'releasedNothingBody') + '\n' +
    extractFn(src, 'singleOutcomeReleaseBody') + '\n' +
    'this.singleOutcomeReleaseBody = singleOutcomeReleaseBody;\n' +
    'this.calledByFromView = calledByFromView;', ctx);
const { singleOutcomeReleaseBody, calledByFromView } = ctx;

const produce = { active_claim: { role: 'produce' }, runtime: { remaining_uop_cached: 0 } };
const consume = (uop) => ({ active_claim: { role: 'consume' }, runtime: { remaining_uop_cached: uop } });

// ── skips the modal: nothing to decide ───────────────────────────────────

const p = singleOutcomeReleaseBody(produce, ['TESTPAYLOAD']);
truthy(p, 'produce release skips the modal even with payloads configured');
eq(p && p.disposition, 'capture_lineside', 'produce posts capture_lineside');
eq(p && Object.keys(p.qty_by_part).length, 0, 'produce pulls nothing to lineside');
eq(p && p.called_by, 'P400', 'produce names the station');

// A produce press at full capacity still skips: the engine discards the
// disposition for produce role, so the count never reaches the decision.
truthy(singleOutcomeReleaseBody({ active_claim: { role: 'produce' }, runtime: { remaining_uop_cached: 100 } }, []),
    'produce skips regardless of remaining UoP');

const c = singleOutcomeReleaseBody(consume(0), []);
truthy(c, 'consume at zero with no payloads skips the modal');
eq(c && c.disposition, 'capture_lineside', 'RELEASE EMPTY posts capture_lineside');

// ── keeps the modal: a real choice lives in it ───────────────────────────
//
// remaining_uop > 0 is the operator choosing between RELEASE PARTIAL (bin
// goes back with its count intact) and BIN EMPTY (UNDER COUNT) (the gap is
// recorded as missing inventory). Those disagree about whether stock
// vanished; picking one for the operator would fabricate a forensics signal.
eq(singleOutcomeReleaseBody(consume(40), []), null,
    'consume with UoP remaining keeps the modal — partial vs under-count is a real choice');
eq(singleOutcomeReleaseBody(consume(1), []), null,
    'one UoP is still a choice');

// Chips present means a lineside pull is declarable even when the count reads
// zero — an operator who took parts off a bin the system thinks is empty has
// no other way to say so.
eq(singleOutcomeReleaseBody(consume(0), ['PART-A']), null,
    'consume at zero WITH payload chips keeps the modal');
eq(singleOutcomeReleaseBody(consume(0), ['PART-A', 'PART-B']), null,
    'multi-payload at zero keeps the modal');

// ── degenerate inputs never auto-post ────────────────────────────────────
//
// Fail toward the modal. A missing claim or runtime means we do not know what
// this release is; showing a prompt costs a tap, auto-posting the wrong
// disposition costs an inventory record.
eq(singleOutcomeReleaseBody(null, []), null, 'no entry → modal');
eq(singleOutcomeReleaseBody({}, []), null, 'no active claim → modal');
eq(singleOutcomeReleaseBody({ active_claim: { role: 'consume' } }, []), null,
    'consume with no runtime → modal (remaining UoP unknown, not zero)');

// ── called_by matches the server's TrimSpace ─────────────────────────────
//
// A station row created with " " is truthy in JS. Shipped as called_by=" " the
// backend rejects it as "release requires called_by", and the operator gets an
// error on a release that skipped the modal — worse than the extra tap.
const blankCtx = vm.createContext({ console: console, getView: () => ({ station: { name: '   ' } }) });
vm.runInContext(extractFn(src, 'calledByFromView') + '\nthis.calledByFromView = calledByFromView;', blankCtx);
eq(blankCtx.calledByFromView(), 'operator', 'whitespace-only station name falls back');

const noStationCtx = vm.createContext({ console: console, getView: () => null });
vm.runInContext(extractFn(src, 'calledByFromView') + '\nthis.calledByFromView = calledByFromView;', noStationCtx);
eq(noStationCtx.calledByFromView(), 'operator', 'no view falls back');
eq(calledByFromView(), 'P400', 'a real station name is used verbatim');

console.log('release single-outcome: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed === 0 ? 0 : 1);
