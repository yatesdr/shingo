// apply-loop.test.js — the preset apply's two decisions.
//
// runPresetApply is the Processes page's only unguarded state machine: it
// walks every ticked style, saves each one, and reacts to what comes back.
// The walk needs a DOM and a network. The two DECISIONS inside it do not, and
// they are the two that were wrong — which is why they live in
// desktop-bodies.js, the file this page's logic goes to in order to be
// testable, and why the page calls them rather than repeating them.
//
// WHAT THEY COST WHEN THEY WERE WRONG:
//
//   - The order. The fingerprint covers BOTH sides, target and running style,
//     because the planner switches on the outgoing claim's mode. Saving the
//     running style therefore moves the from-side under every other row's
//     fingerprint at once. In rail order that 409-cascaded through every row
//     after it, and a 40-part apply could leave 39 of them unwritten.
//   - The retry. A 409 is the ORDINARY outcome of the row before this one.
//     The loop re-previewed and then stopped, so the row sat unwritten under a
//     sentence about previewing and the engineer re-ran the whole apply.

'use strict';

const path = require('path');
const assert = require('assert');

const B = require(path.join(__dirname, 'desktop-bodies.js'));

let failures = 0;
let checks = 0;
function test(name, fn) {
    try {
        fn();
        checks++;
    } catch (e) {
        failures++;
        console.log('  FAIL ' + name + '\n       ' + (e && e.message));
    }
}

const STYLES = [{ id: 1 }, { id: 2 }, { id: 3 }, { id: 4 }];

test('applyOrder: the running style is saved last', () => {
    assert.deepStrictEqual(B.applyOrder(STYLES, 2).map(s => s.id), [1, 3, 4, 2]);
});

test('applyOrder: no running style leaves the rail order alone', () => {
    assert.deepStrictEqual(B.applyOrder(STYLES, 0).map(s => s.id), [1, 2, 3, 4]);
});

test('applyOrder: a running style that is not in the list changes nothing', () => {
    assert.deepStrictEqual(B.applyOrder(STYLES, 99).map(s => s.id), [1, 2, 3, 4]);
});

test('applyOrder: an empty list is an empty list', () => {
    assert.deepStrictEqual(B.applyOrder([], 2), []);
    assert.deepStrictEqual(B.applyOrder(null, 2), []);
});

test('applyOrder: the rail order is not mutated', () => {
    const rail = STYLES.slice();
    B.applyOrder(rail, 2);
    assert.deepStrictEqual(rail.map(s => s.id), [1, 2, 3, 4]);
});

test('applyOutcome: 200 is saved', () => {
    const o = B.applyOutcome(200, {}, 1);
    assert.strictEqual(o.ok, true);
    assert.strictEqual(o.retry, false);
    assert.strictEqual(o.text, 'saved');
});

test('applyOutcome: the first 409 asks for a retry', () => {
    const o = B.applyOutcome(409, { stale: true }, 1);
    assert.strictEqual(o.retry, true);
    assert.ok(/previewing again/.test(o.text), o.text);
});

// ONCE. A second 409 means something is genuinely moving the rows underneath —
// another session, or a changeover — and retrying into that is a loop.
test('applyOutcome: the second 409 is a refusal, not another retry', () => {
    const o = B.applyOutcome(409, { error: 'the flow changed since it was previewed' }, 2);
    assert.strictEqual(o.retry, false);
    assert.strictEqual(o.ok, false);
    assert.strictEqual(o.text, 'the flow changed since it was previewed');
});

// Every other refusal reads as the SERVER'S sentence. The engine names its
// refusals — that is what the sentences on flow/save are for — and a row that
// said "refused (403)" would make the engineer go looking in the console for
// something the response already said.
test('applyOutcome: a named refusal keeps its name', () => {
    const o = B.applyOutcome(403, { error: 'flow composer is not enabled for this process' }, 1);
    assert.strictEqual(o.ok, false);
    assert.strictEqual(o.text, 'flow composer is not enabled for this process');
});

test('applyOutcome: an unnamed refusal still says what happened', () => {
    assert.strictEqual(B.applyOutcome(500, {}, 1).text, 'refused (500)');
    assert.strictEqual(B.applyOutcome(500, null, 1).text, 'refused (500)');
});

// ── the loop, over the two decisions ─────────────────────────────────────────
//
// A stand-in for runPresetApply that makes the same two calls in the same
// order. It is not the page — the page's own walk draws and fetches — but it
// is the sequence, and it is what proves the two decisions compose into the
// behaviour the modal reports.
function runApply(styles, runningID, respond) {
    const results = [];
    for (const st of B.applyOrder(styles, runningID)) {
        let attempt = 1;
        let r = respond(st.id, attempt);
        let out = B.applyOutcome(r.status, r.body, attempt);
        if (out.retry) {
            attempt = 2;
            r = respond(st.id, attempt);
            out = B.applyOutcome(r.status, r.body, attempt);
        }
        results.push({ id: st.id, ok: !!out.ok, text: out.text });
    }
    return results;
}

test('the loop: a stale row is written on the retry', () => {
    const seen = [];
    const got = runApply(STYLES, 2, (id, attempt) => {
        seen.push(id + ':' + attempt);
        return id === 3 && attempt === 1 ? { status: 409, body: { stale: true } } : { status: 200, body: {} };
    });
    assert.deepStrictEqual(got.map(r => r.ok), [true, true, true, true],
        'every row saved: ' + JSON.stringify(got));
    // Style 3 was tried twice; the running style (2) came last.
    assert.deepStrictEqual(seen, ['1:1', '3:1', '3:2', '4:1', '2:1']);
});

test('the loop: a row that stays stale is reported, and the rest still save', () => {
    const got = runApply(STYLES, 2, id => (id === 3 ? { status: 409, body: { error: 'still stale' } } : { status: 200, body: {} }));
    assert.deepStrictEqual(got.map(r => r.ok), [true, false, true, true]);
    assert.strictEqual(got[1].text, 'still stale');
});

// ── the Save button's own decision ───────────────────────────────────────────
//
// saveOutcome was inline in processes-desktop.js's saveFlow, which means the
// only thing that ever exercised it was the shots harness looking at a
// .pd-refusal in a PNG — and a screenshot cannot tell "the sentence the server
// sent" from "a sentence". These are the branches an engineer meets.

test('save: 409 stale re-previews and says so', () => {
    const out = B.saveOutcome(409, { stale: true });
    assert.strictEqual(out.kind, 'stale');
    assert.match(out.text, /checking it again/);
});

test('save: 409 that is a RULE keeps the server sentence', () => {
    // The running-position move. Flattening this into "checking it again" tells
    // the engineer to wait for something that will never resolve itself.
    const msg = 'PRESS-2-RUN is running on PLN_03 — move it to PLN_04 after the next changeover';
    const out = B.saveOutcome(409, { error: msg });
    assert.strictEqual(out.kind, 'refused');
    assert.strictEqual(out.text, msg);
});

test('save: 422 is findings on the rows, not a bar message', () => {
    const out = B.saveOutcome(422, { findings: [{ node: 'PLN_01' }] });
    assert.strictEqual(out.kind, 'findings');
    assert.strictEqual(out.text, '');
});

test('save: every other refusal arrives by name', () => {
    // 403 is the flow-composer gate, 400 an unknown station or a half-written
    // preset provenance, 500 the rest. All three used to redraw the bar saying
    // nothing, which reads as a button that does not work.
    for (const [status, msg] of [
        [403, 'the flow composer is off for this process'],
        [400, 'unknown station 91'],
        [500, 'write the flow: database is locked'],
    ]) {
        const out = B.saveOutcome(status, { error: msg });
        assert.strictEqual(out.kind, 'refused', String(status));
        assert.strictEqual(out.text, msg, String(status));
    }
});

test('save: a refusal with no sentence still names the status', () => {
    const out = B.saveOutcome(500, {});
    assert.strictEqual(out.kind, 'refused');
    assert.match(out.text, /500/);
});

test('save: 2xx is saved', () => {
    assert.strictEqual(B.saveOutcome(200, {}).kind, 'saved');
    assert.strictEqual(B.saveOutcome(204, {}).kind, 'saved');
});


if (failures) {
    console.error('\n' + failures + ' failed, ' + checks + ' passed');
    process.exit(1);
}
console.log(checks + ' apply-loop checks passed');
