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

// The page code under test. This file lives in www/testdata, outside the
// embedded static tree, so no binary carries or serves it
// (TestStaticTree_HoldsNoTestJS); the code it reads stays in www/static.
const SRC_DIR = path.join(__dirname, '..', '..', '..', 'static', 'operator-station');

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
const src = fs.readFileSync(path.join(SRC_DIR, 'operator-release.js'), 'utf8');
const ctx = vm.createContext({ console: console, getView: () => ({ station: { name: 'P400' } }) });
vm.runInContext(
    extractFn(src, 'calledByFromView') + '\n' +
    extractFn(src, 'releasedNothingBody') + '\n' +
    extractFn(src, 'releaseBody') + '\n' +
    extractFn(src, 'singleOutcomeReleaseBody') + '\n' +
    'this.singleOutcomeReleaseBody = singleOutcomeReleaseBody;\n' +
    'this.calledByFromView = calledByFromView;', ctx);
const { singleOutcomeReleaseBody, calledByFromView } = ctx;

const produce = { active_claim: { role: 'produce' }, runtime: { remaining_uop_cached: 0 } };
const consume = (uop) => ({ active_claim: { role: 'consume' }, runtime: { remaining_uop_cached: uop } });

// ── skips the modal: nothing to decide ───────────────────────────────────

const p = singleOutcomeReleaseBody(produce, ['TESTPAYLOAD']);
truthy(p, 'produce release skips the modal even with payloads configured');
// A PRODUCE RELEASE CARRIES NO DISPOSITION, and this assertion used to say the
// opposite. capture_lineside with an empty qty_by_part is the RELEASE EMPTY
// wire shape: ComputeReleaseRemainingUOP turns it into &0 and Core clears the
// carrier. The button said RELEASE FULL. That contradiction was inert only
// while the engine's produce-role branch discarded the disposition, and
// releaseOrderDropFastPath skips that branch — so at a changeover it became the
// instruction that emptied a full bin. Hopkinsville: 46 wipes, 140,762 parts,
// 2026-07-16 to 09-08. An absent mode reaches ComputeReleaseRemainingUOP's
// default, which is "no manifest action".
eq(p && p.disposition, undefined, 'produce posts NO disposition — the bin goes out full');
eq(p && p.called_by, 'P400', 'produce still names who released it');
// No lineside fields at all, rather than empty ones. An empty qty_by_part is
// still an ANSWER to "what was pulled to lineside?" — and the answer it encodes
// is "nothing, so the bin is empty". A produce release is not answering that
// question, so it must not appear to.
eq(p && p.qty_by_part, undefined, 'produce carries no qty_by_part — it is not answering a lineside question');
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

// ── RELEASE SHORT asks: partial or full ──────────────────────────────────
//
// The one produce release with a choice in it. The tile reads RELEASE SHORT
// (release_short: a strict claim, the count short of the pack and outside its
// tolerance), and the person at the line may see a full cart the count calls
// short. RELEASE PARTIAL posts the plain produce body; RELEASE FULL adds
// declare_full and nothing else — no disposition, so it can never be the
// RELEASE EMPTY wire shape that wiped full bins at Hopkinsville. A changeover's
// release never offers it: a changeover ships the real count.
const fullCtx = vm.createContext({ console: console, esc: (v) => String(v), getView: () => ({ station: { name: 'P400' } }) });
vm.runInContext(
    extractFn(src, 'isChangeoverRelease') + '\n' +
    extractFn(src, 'offersReleaseFull') + '\n' +
    extractFn(src, 'releaseFullBody') + '\n' +
    extractFn(src, 'releaseFullPromptHTML') + '\n' +
    'this.offersReleaseFull = offersReleaseFull; this.releaseFullBody = releaseFullBody;' +
    'this.releaseFullPromptHTML = releaseFullPromptHTML;', fullCtx);
const short = { active_claim: { role: 'produce', uop_capacity: 2400 }, runtime: { remaining_uop_cached: 1990 }, release_short: true };
const pairURL = '/api/process-nodes/7/release-staged';
truthy(fullCtx.offersReleaseFull(short, pairURL), 'RELEASE SHORT on the pair door asks');
truthy(fullCtx.offersReleaseFull(short, '/api/orders/9/release'), 'RELEASE SHORT on the per-order door asks');
eq(fullCtx.offersReleaseFull(Object.assign({}, short, { release_short: false }), pairURL), false,
    'a produce RELEASE that is not short does not ask');
eq(fullCtx.offersReleaseFull(short, '/api/processes/3/changeover/release?node_id=7'), false,
    'a changeover release never offers RELEASE FULL');
eq(fullCtx.offersReleaseFull({ active_claim: { role: 'consume' }, release_short: true }, pairURL), false,
    'consume never offers RELEASE FULL');
eq(fullCtx.offersReleaseFull(null, pairURL), false, 'no entry does not ask');

const fb = fullCtx.releaseFullBody('P400');
eq(fb.declare_full, true, 'RELEASE FULL declares');
eq(fb.called_by, 'P400', 'RELEASE FULL names who declared it');
eq(fb.disposition, undefined, 'RELEASE FULL carries no disposition — it cannot clear the bin');
eq(fb.qty_by_part, undefined, 'RELEASE FULL answers no lineside question');
const pb = singleOutcomeReleaseBody(short, ['767A5']);
eq(pb && pb.declare_full, undefined, 'the plain produce body (RELEASE PARTIAL) does not declare');

const html = fullCtx.releaseFullPromptHTML(short);
truthy(html.indexOf('Counted 1990 of 2400') >= 0, 'the prompt shows the count against the pack');
truthy(html.indexOf('data-action="release-short-partial"') >= 0 && html.indexOf('>RELEASE PARTIAL<') >= 0, 'RELEASE PARTIAL is offered');
truthy(html.indexOf('data-action="release-short-full"') >= 0 && html.indexOf('>RELEASE FULL<') >= 0, 'RELEASE FULL is offered');
truthy(html.indexOf('data-action="release-cancel"') >= 0, 'CANCEL is offered');

console.log('release single-outcome: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed === 0 ? 0 : 1);
