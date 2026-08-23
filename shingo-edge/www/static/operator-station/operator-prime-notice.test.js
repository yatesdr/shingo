// Unit tests for primeNoticeText (operator-util.js) — "press again when it
// lands".
//
// WHAT THIS PINS. Round 1 made a press-index cell with a bare index position
// prime that position instead of minting a swap that could never source. The
// fix worked and the operator could not see it: prime_orders crossed the wire
// and no consumer read it, so REQUEST SWAP appeared to do nothing.
//
// Asserted at the rendering level, on the sentence an operator actually reads
// — round 1's unit-5 lesson, where "1 participant node HAVE no process node
// configured" survived review and died the moment it was rendered.
//
// Runs under plain Node (no npm). Exit 0 = pass, 1 = any failure. Run via the
// Go wrapper operator_prime_notice_test.go.

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

const utilSrc = fs.readFileSync(path.join(__dirname, 'operator-util.js'), 'utf8')
    .replace(/^export\s+/gm, '');
const ctx = vm.createContext({
    document: { body: { dataset: { stationId: '1' } }, getElementById: () => null },
    console: console,
});
vm.runInContext(utilSrc, ctx);
const { primeNoticeText } = ctx;

// ── the primes-only round ────────────────────────────────────────────────

const oneP = primeNoticeText({
    cycle_mode: 'simple',
    prime_orders: [{ delivery_node: 'INDEX-B', source_node: 'MARKET-EMPTIES' }],
});
truthy(/INDEX-B/.test(oneP), 'the notice names the position being primed');
truthy(/MARKET-EMPTIES/.test(oneP), 'the notice names where the empty comes from');
truthy(/press again when it lands/i.test(oneP), 'the notice says what to do next');

// A 3-position layout primes two positions and must name both — an operator
// told about one of them will press again too early.
const twoP = primeNoticeText({
    cycle_mode: 'simple',
    prime_orders: [
        { delivery_node: 'INDEX-B', source_node: 'MARKET-EMPTIES' },
        { delivery_node: 'INDEX-C', source_node: 'MARKET-EMPTIES' },
    ],
});
truthy(/INDEX-B/.test(twoP) && /INDEX-C/.test(twoP), 'both primed positions are named');

// ── silence where there is nothing to say ────────────────────────────────

eq(primeNoticeText(null), '', 'no result → no notice');
eq(primeNoticeText({}), '', 'no primes → no notice');
eq(primeNoticeText({ prime_orders: [] }), '', 'empty primes → no notice');
eq(primeNoticeText({ order_a: { id: 1 }, order_b: { id: 2 }, prime_orders: [] }),
    '', 'an ordinary swap says nothing');

// THE DISCRIMINATOR. A consume downgrade emits primes ALONGSIDE its delivery:
// that round did do what the operator asked, so telling them to press again
// would be wrong. Keyed on "primes and no swap legs", never on cycle_mode —
// both rounds report cycle_mode "simple".
eq(primeNoticeText({
    cycle_mode: 'simple',
    order_a: { id: 7 },
    prime_orders: [{ delivery_node: 'INDEX-B', source_node: 'MARKET-EMPTIES' }],
}), '', 'primes alongside a swap leg are not a primes-only round');
eq(primeNoticeText({
    cycle_mode: 'simple',
    order_b: { id: 8 },
    prime_orders: [{ delivery_node: 'INDEX-B' }],
}), '', 'order_b alone also means the swap happened');

// A prime whose source Core resolved globally still reads as a sentence.
const noSrc = primeNoticeText({ prime_orders: [{ delivery_node: 'INDEX-B' }] });
truthy(/INDEX-B/.test(noSrc), 'a sourceless prime still names the destination');
truthy(noSrc.indexOf('from') < 0, 'a sourceless prime does not say "from undefined"');

// A prime with neither field must not render "Priming  — press again".
const bare = primeNoticeText({ prime_orders: [{}] });
truthy(/index position/.test(bare), 'a prime with no names falls back to a readable phrase');

if (failed > 0) {
    console.error('\n' + failed + ' failure(s), ' + passed + ' passed');
    process.exit(1);
}
console.log('OK: ' + passed + ' assertions passed');
