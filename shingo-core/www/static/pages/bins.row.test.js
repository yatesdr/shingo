// Unit tests for bins-row.js, the Bins table row drawn from a bin's detail
// answer, and bins-echo.js, the counting of the page's own-action echoes. Run
// under plain Node via the Go wrapper bins_row_view_test.go. Exit
// 0 on pass, 1 on any assertion failure.
//
// The bug these exist to prevent: refreshBinRow (the repaint after an action)
// wrote the raw _TRANSIT / _ROBOT:<id> node name into Location, rebuilt Flags
// without the Return button, and left the sort and search values as the page
// loaded them. The row must draw as templates/bins.html draws it.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let failures = 0;
function check(name, cond, detail) {
    if (cond) {
        console.log('  ok  ' + name);
    } else {
        failures++;
        console.log('  FAIL ' + name + (detail ? ' — ' + detail : ''));
    }
}
function eq(name, got, want) {
    check(name, got === want, 'got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
}

// escapeHtml mirrors shared/utils.js's (the five-character map). Injected in
// place of the ES import: the harness runs the module in a vm with no ES
// resolver, the same reason the other page tests inject their imports.
const HTML_ESCAPES = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
function escapeHtml(s) {
    if (s === null || s === undefined || s === '') return '';
    return String(s).replace(/[&<>"']/g, (c) => HTML_ESCAPES[c]);
}

function load() {
    let src = fs.readFileSync(path.join(__dirname, 'bins-row.js'), 'utf8');
    src = src.replace(/^import .*$/m, '').replace(/^export /mg, '');
    const ctx = { escapeHtml };
    vm.createContext(ctx);
    vm.runInContext(src + '\nthis.binRowView = binRowView; this.uopText = uopText; this.binTransit = binTransit;', ctx);
    return ctx;
}

const m = load();

// loadEcho runs bins-echo.js (no imports) in its own context.
function loadEcho() {
    let src = fs.readFileSync(path.join(__dirname, 'bins-echo.js'), 'utf8');
    src = src.replace(/^export /mg, '');
    const ctx = {};
    vm.createContext(ctx);
    vm.runInContext(src + '\nthis.ECHOES = ECHOES; this.echoesFor = echoesFor; this.newEchoLedger = newEchoLedger;', ctx);
    return ctx;
}
const ec = loadEcho();

function bin(over) {
    return Object.assign({
        id: 7, label: 'CARRIER-0007', bin_type_code: 'TOTE', status: 'available',
        node_name: 'SMN_001', payload_code: 'PART-1', uop_remaining: 40, uop_capacity: 100,
        manifest_confirmed: true, locked: false, locked_by: '', claimed_by: undefined,
    }, over || {});
}

console.log('plain bin at a node');
{
    const v = m.binRowView({ bin: bin({ locked: true, locked_by: 'ann', manifest_confirmed: false }), recent_orders: [] }, true);
    eq('location is the node name', v.location.html, 'SMN_001');
    eq('location sort is the node name', v.location.sort, 'SMN_001');
    eq('data-node is the node name', v.data.node, 'SMN_001');
    eq('payload cell', v.payload.html, '<code>PART-1</code>');
    eq('payload sort', v.payload.sort, 'PART-1');
    eq('contents', v.data.contents, 'unconfirmed');
    eq('uop sort', v.uop.sort, '40');
    eq('uop capacity carried for the cycle count', v.data.uopCapacity, '100');
    eq('label sort', v.label.sort, 'CARRIER-0007');
    eq('type', v.type.text, 'TOTE');
    eq('status sort', v.status.sort, 'available');
    check('flags carry locked and unconfirmed', v.flags.html.indexOf('>locked<') !== -1 && v.flags.html.indexOf('>unconfirmed<') !== -1, v.flags.html);
    eq('no Return button off a robot', v.flags.returnHTML, '');
}

console.log('names are escaped in the cells, raw in the sort/search values');
{
    const v = m.binRowView({ bin: bin({ node_name: '<b>"N"</b>', label: 'A&B' }), recent_orders: [] }, true);
    eq('location escaped', v.location.html, '&lt;b&gt;&quot;N&quot;&lt;/b&gt;');
    eq('location sort raw', v.location.sort, '<b>"N"</b>');
    check('label escaped', v.label.html.indexOf('<code>A&amp;B</code>') !== -1, v.label.html);
}

console.log('in transit, claimed: route from the claim');
{
    const v = m.binRowView({
        bin: bin({ node_name: '_TRANSIT', claimed_by: 31, payload_code: '' }),
        current_order: { id: 31, status: 'in_transit', source_node: 'SMN_001', delivery_node: 'P400', payload_code: 'PART-9' },
        recent_orders: [{ id: 30, status: 'confirmed', source_node: 'X', delivery_node: 'Y', payload_code: 'OLD' }],
    }, true);
    eq('location is the route', v.location.html, '<span title="In transit">SMN_001 &rarr; P400</span>');
    eq('data-node is the route', v.data.node, 'SMN_001 → P400');
    eq('location sort is the route', v.location.sort, 'SMN_001 → P400');
    eq('payload is the cargo, in transit', v.payload.html, '<code>PART-9</code> <span class="text-muted" style="font-size:0.85em">(in transit)</span>');
    eq('data-payload is the cargo', v.data.payload, 'PART-9');
    check('raw _TRANSIT is never shown', v.location.html.indexOf('_TRANSIT') === -1);
}

console.log('in transit, claimed, the claim unreadable: "in transit"');
{
    const v = m.binRowView({ bin: bin({ node_name: '_TRANSIT', claimed_by: 31, payload_code: '' }), recent_orders: [] }, true);
    eq('location', v.location.html, '<span title="In transit">in transit</span>');
    eq('data-node is the empty route', v.data.node, ' → ');
}

console.log('stranded: the last order, and the note');
{
    const v = m.binRowView({
        bin: bin({ node_name: '_TRANSIT', payload_code: '', anomaly_note: 'near <dock> 3' }),
        recent_orders: [{ id: 12, status: 'cancelled', source_node: 'ALN_006', delivery_node: 'SMN_0013', payload_code: 'PART-2' }],
    }, true);
    eq('location', v.location.html,
        '<span title="The order this route came from is over; the bin is not moving">last order #12 (cancelled): ALN_006 &rarr; SMN_0013</span>' +
        '<div class="text-muted-xs">stranded &mdash; near &lt;dock&gt; 3</div>');
    eq('payload says last order', v.payload.html, '<code>PART-2</code> <span class="text-muted" style="font-size:0.85em">(last order)</span>');
}

console.log('stranded with no order at all');
{
    const v = m.binRowView({ bin: bin({ node_name: '_TRANSIT', payload_code: '' }), recent_orders: [] }, true);
    eq('location', v.location.html, 'lost in transit<div class="text-muted-xs">stranded</div>');
    eq('payload', v.payload.html, '<span class="text-muted">-</span>');
}

console.log('on a robot deck: the robot, and the Return button');
{
    const d = {
        bin: bin({ id: 44, node_name: '_ROBOT:AMR-04', claimed_by: 9, anomaly_note: 'by "B" aisle' }),
        current_order: { id: 9, status: 'in_transit', source_node: 'A', delivery_node: 'B', payload_code: 'PART-1' },
        recent_orders: [],
    };
    const v = m.binRowView(d, true);
    eq('location', v.location.html,
        '<span title="In transit">A &rarr; B</span><div class="text-muted-xs">on AMR-04</div>' +
        '<div class="text-muted-xs bin-loc-note" title="by &quot;B&quot; aisle">by &quot;B&quot; aisle</div>');
    eq('Return button', v.flags.returnHTML,
        '<span data-action="stopPropagation"><button class="btn btn-sm" data-action="askRobotToSetDown:44:AMR-04" title="Return this bin to where it is used from">Return</button></span>');
    eq('no Return button when signed out', m.binRowView(d, false).flags.returnHTML, '');
    check('raw _ROBOT: is never shown', v.location.html.indexOf('_ROBOT:') === -1);
}

console.log('uopText, the UoP cell');
{
    eq('payload-less bin with a negative count shows it', m.uopText({ payload_code: '', uop_remaining: -3 }), '-3');
    eq('payload-less bin with no count', m.uopText({ payload_code: '', uop_remaining: 0 }), '<span class="text-muted">-</span>');
    eq('loaded bin at zero', m.uopText({ payload_code: 'P', uop_remaining: 0 }), '0');
}

console.log('echo counts per verb (the Go pin checks them against the handlers)');
{
    eq('16 verbs counted', Object.keys(ec.ECHOES).length, 16);
    eq('flag emits one', ec.echoesFor('flag'), 1);
    eq('add_note emits none', ec.echoesFor('add_note'), 0);
    eq('an unknown verb is counted as one', ec.echoesFor('bogus'), 1);
}

console.log('echo ledger: counted, not timed');
{
    const L = ec.newEchoLedger(5000);
    // The echo beats the answer (Core emits before it writes the response).
    const e = L.expect(7, 1);
    eq('echo during the post is taken', L.take(7, 10), true);
    eq('settle ok', L.settle(e, true, 20), 0);
    eq('the next event is a real update', L.take(7, 30), false);
    eq('nothing left', L.size(), 0);
}
{
    const L = ec.newEchoLedger(5000);
    const e = L.expect(7, 1);
    L.settle(e, true, 0);
    eq('echo after the answer is taken', L.take(7, 100), true);
    eq('a second event is real', L.take(7, 200), false);
}
{
    const L = ec.newEchoLedger(5000);
    const e = L.expect(7, 1);
    L.settle(e, true, 0);
    eq('a lost echo expires: a later real update is not swallowed', L.take(7, 5001), false);
    eq('expired entry dropped', L.size(), 0);
}
{
    const L = ec.newEchoLedger(5000);
    const e = L.expect(7, 1);
    eq('refused: nothing was taken', L.settle(e, false, 10), 0);
    eq('refused: the next event is real', L.take(7, 20), false);
}
{
    const L = ec.newEchoLedger(5000);
    const e = L.expect(7, 1);
    eq('an event during a post that is then refused is taken...', L.take(7, 5), true);
    eq('...and handed back as real on the refusal', L.settle(e, false, 10), 1);
}
{
    const L = ec.newEchoLedger(5000);
    const e = L.expect(7, ec.echoesFor('add_note'));
    eq('add_note expects none: an event during it is real', L.take(7, 5), false);
    L.settle(e, true, 10);
    eq('add_note leaves nothing behind', L.size(), 0);
}
{
    const L = ec.newEchoLedger(5000);
    const a = L.expect(7, 1), b = L.expect(7, 1);
    L.settle(a, true, 10); L.settle(b, true, 12);
    eq('two actions: first echo taken', L.take(7, 20), true);
    eq('two actions: second echo taken', L.take(7, 21), true);
    eq('two actions: third event is real', L.take(7, 22), false);
}
{
    const L = ec.newEchoLedger(5000);
    // Bulk: one entry per bin; bin 2 refused (locked), bin 3 not in the post.
    const e1 = L.expect(1, 1), e2 = L.expect(2, 1);
    eq('an event for another bin is not consumed', L.take(3, 5), false);
    L.settle(e1, true, 10);
    L.settle(e2, false, 10);
    eq('bulk: bin 1 echo taken', L.take(1, 20), true);
    eq('bulk: refused bin 2 event is real', L.take(2, 20), false);
}

if (failures > 0) {
    console.log(failures + ' failure(s)');
    process.exit(1);
}
console.log('all passed');
