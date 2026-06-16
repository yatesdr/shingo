// Unit tests for the drain-slot decision logic in operator-render.js:
// isDrainSlot (which manual_swap slots are consume "drain" slots → slot-only tap +
// loader-board palette) and drainColorClass (idle/awaiting/ready mapping). These are
// the heart of the drain-unloader behavior; the DOM wiring (tap → confirmUnloadSwap)
// is covered by manual acceptance.
//
// operator-render.js isn't import-free or DOM-free, so (unlike operator-window-state)
// we strip its ES imports, stub the module-level document lookups + the one imported
// helper the drain logic uses (isActive), and only exercise the two pure decisions.
// Runs under plain Node via the Go wrapper operator_render_drain_test.go. Exit 0 = pass.

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

const src = fs.readFileSync(path.join(__dirname, 'operator-render.js'), 'utf8')
    .replace(/^import\s.*$/gm, '')                  // drop ES import lines
    .replace(/export\s+function\s+/g, 'function ')
    .replace(/export\s+const\s+/g, 'const ')
    .replace(/export\s*\{[^}]*\}\s*;?/g, '');        // drop `export { fillColor };`
const ctx = vm.createContext({
    document: { getElementById: () => null },        // module-level os-grid/etc. lookups
    isActive: function (s) { return s !== 'completed' && s !== 'cancelled' && s !== 'failed'; },
});
vm.runInContext(src + '\nthis.isDrainSlot = isDrainSlot; this.drainColorClass = drainColorClass;', ctx);
const isDrainSlot = ctx.isDrainSlot, drainColorClass = ctx.drainColorClass;

function cl(role, swap) { return { role: role, swap_mode: swap }; }

// ── isDrainSlot: consume + manual_swap + not home-location ──
eq(isDrainSlot({ active_claim: cl('consume', 'manual_swap') }), true, 'consume manual_swap → drain');
eq(isDrainSlot({ active_claim: cl('produce', 'manual_swap') }), false, 'produce manual_swap → not drain (loader keeps payload board)');
eq(isDrainSlot({ active_claim: cl('consume', 'two_robot') }), false, 'consume non-manual_swap → not drain');
eq(isDrainSlot({ active_claim: cl('consume', 'manual_swap'), home_location_loader: true }), false, 'home-location consume → not drain (keeps per-position board)');
eq(isDrainSlot({ active_claim: null }), false, 'no claim → not drain');

// ── drainColorClass: parked full = ready(green), inbound = awaiting(amber), empty = idle(neutral) ──
eq(drainColorClass({ bin_state: { occupied: true, payload_code: 'LK41 PIA15' }, orders: [] }), 'os-drain-ready', 'parked full → ready');
eq(drainColorClass({ bin_state: { occupied: false }, orders: [{ status: 'in_transit' }] }), 'os-drain-awaiting', 'inbound full → awaiting');
eq(drainColorClass({ bin_state: { occupied: false }, orders: [] }), 'os-drain-idle', 'empty → idle');
eq(drainColorClass({ bin_state: { occupied: true, payload_code: '' }, orders: [] }), 'os-drain-idle', 'occupied empty tote (no part) → idle, NOT a fault');

if (failed) { console.error(passed + ' passed, ' + failed + ' FAILED'); process.exit(1); }
console.log('operator-render drain logic: ' + passed + ' passed');
