// Unit tests for the unloader-window decisions: isUnloaderWindow
// (operator-board-model.js: which loader tiles are windows of a shared
// unloader, drawn as slots with the slot-only tap and the loader-board palette)
// and unloaderWindowColour (operator-render.js: the idle/awaiting/ready mapping).
// The DOM wiring (tap → confirmUnloadSwap) is pinned by
// operator-loader-station.test.js.
//
// operator-render.js isn't import-free or DOM-free, so we strip its ES imports,
// stub the module-level document lookups + the one imported helper the colour
// uses (isActive), and only exercise the pure decisions. Runs under plain Node
// via the Go wrapper operator_render_drain_test.go. Exit 0 = pass.

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

const model = fs.readFileSync(path.join(__dirname, 'operator-board-model.js'), 'utf8')
    .replace(/^import\s[^;]*;/gm, '').replace(/^export\s+/gm, '');
const src = fs.readFileSync(path.join(__dirname, 'operator-render.js'), 'utf8')
    .replace(/^import\s.*$/gm, '')                  // drop ES import lines
    .replace(/export\s+function\s+/g, 'function ')
    .replace(/export\s+const\s+/g, 'const ')
    .replace(/export\s*\{[^}]*\}\s*;?/g, '');        // drop `export { fillColor };`
const ctx = vm.createContext({
    document: { getElementById: () => null },        // module-level os-grid/etc. lookups
    isActive: function (s) { return s !== 'completed' && s !== 'cancelled' && s !== 'failed'; },
});
// The model reads a window's role through the window model (windowRole).
vm.runInContext(fs.readFileSync(path.join(__dirname, 'operator-window-state.js'), 'utf8')
    .replace(/^export\s+/gm, ''), ctx);
vm.runInContext(model, ctx);
vm.runInContext(src + '\nthis.unloaderWindowColour = unloaderWindowColour;', ctx);
const isUnloaderWindow = ctx.isUnloaderWindow, colour = ctx.unloaderWindowColour;

function tile(role, layout) { return { loader: { role: role, layout: layout || 'shared_window' } }; }

// ── isUnloaderWindow: a shared unloader's window, not a dedicated home ──
eq(isUnloaderWindow(tile('consume')), true, 'shared unloader window → slot');
eq(isUnloaderWindow(tile('produce')), false, 'loader window → not a slot (keeps the payload board)');
eq(isUnloaderWindow(tile('consume', 'dedicated_positions')), false, 'dedicated unloader home → not a slot (keeps its per-position board)');
eq(isUnloaderWindow({ active_claim: { role: 'consume', swap_mode: 'two_robot' } }), false, 'a cell → not a slot');
eq(isUnloaderWindow({}), false, 'no loader → not a slot');

// ── unloaderWindowColour: parked full = ready(green), inbound = awaiting(amber), empty = idle(neutral) ──
eq(colour({ bin_state: { occupied: true, payload_code: 'LK41 PIA15' }, orders: [] }), 'os-drain-ready', 'parked full → ready');
eq(colour({ bin_state: { occupied: false }, orders: [{ status: 'in_transit' }] }), 'os-drain-awaiting', 'inbound full → awaiting');
eq(colour({ bin_state: { occupied: false }, orders: [] }), 'os-drain-idle', 'empty → idle');
eq(colour({ bin_state: { occupied: true, payload_code: '' }, orders: [] }), 'os-drain-idle', 'occupied empty tote (no part) → idle, NOT a fault');

if (failed) { console.error(passed + ' passed, ' + failed + ' FAILED'); process.exit(1); }
console.log('operator-render unloader windows: ' + passed + ' passed');
