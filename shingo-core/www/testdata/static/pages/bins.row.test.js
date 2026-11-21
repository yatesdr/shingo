// Unit tests for bins-row.js, the Bins table row drawn from a bin's detail
// answer, bins-echo.js, the counting of the page's own-action echoes, and
// bins-live.js, the feed that keeps the rows current. Run under plain Node via
// the Go wrapper bins_row_view_test.go. Exit 0 on pass, 1 on any assertion
// failure.
//
// The bug these exist to prevent: refreshBinRow (the repaint after an action)
// wrote the raw _TRANSIT / _ROBOT:<id> node name into Location, rebuilt Flags
// without the Return button, and left the sort and search values as the page
// loaded them. The row must draw as templates/bins.html draws it.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

// The page code under test. This file lives in www/testdata, outside the
// embedded static tree, so no binary carries or serves it
// (TestStaticTree_HoldsNoTestJS); the code it reads stays in www/static.
const SRC_DIR = path.join(__dirname, '..', '..', '..', 'static', 'pages');

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

// escapeHtml mirrors shared/dom.js's (the five-character map). Injected in
// place of the ES import: the harness runs the module in a vm with no ES
// resolver, the same reason the other page tests inject their imports.
const HTML_ESCAPES = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
function escapeHtml(s) {
    if (s === null || s === undefined || s === '') return '';
    return String(s).replace(/[&<>"']/g, (c) => HTML_ESCAPES[c]);
}

function load() {
    let src = fs.readFileSync(path.join(SRC_DIR, 'bins-row.js'), 'utf8');
    src = src.replace(/^import .*$/mg, '').replace(/^export /mg, '');
    const ctx = { escapeHtml };
    vm.createContext(ctx);
    require('./lend-count.js')(ctx);
    vm.runInContext(src + '\nthis.binRowView = binRowView; this.uopText = uopText; this.binTransit = binTransit;', ctx);
    return ctx;
}

const m = load();

// loadEcho runs bins-echo.js (no imports) in its own context.
function loadEcho() {
    let src = fs.readFileSync(path.join(SRC_DIR, 'bins-echo.js'), 'utf8');
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
    eq('no process in the answer: an empty Process cell', v.process.text, '');
    check('no has_notes: no notes chip', v.flags.html.indexOf('>notes<') === -1, v.flags.html);
}

console.log('Process and the notes chip come from the answer, as handleBins draws them');
{
    const v = m.binRowView({ bin: bin(), recent_orders: [], process: 'SNF3 / SNF4', has_notes: true }, true);
    eq('process cell', v.process.text, 'SNF3 / SNF4');
    check('notes chip, after the other flags', /chip-muted" title="Has notes">notes<\/span>$/.test(v.flags.html), v.flags.html);
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
        '<div class="text-muted-xs bin-loc-note cell-clip" title="by &quot;B&quot; aisle">by &quot;B&quot; aisle</div>');
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

// renderContents (bins.js) is the pop-up's Contents table, drawn from the same
// detail answer: each part's Qty is derived from uop_remaining and the payload
// template by protocol.ManifestItem's count rule. bins.js wires the page when it
// loads, so the function is lifted out by name and run with shared/dom.js's h.
function extractFn(src, name) {
    const m = new RegExp('function\\s+' + name + '\\s*\\(').exec(src);
    if (!m) throw new Error('no function ' + name);
    let depth = 0;
    for (let i = src.indexOf('{', m.index); i < src.length; i++) {
        if (src[i] === '{') depth++;
        else if (src[i] === '}' && --depth === 0) return src.slice(m.index, i + 1);
    }
    throw new Error('unclosed function ' + name);
}
function loadContents() {
    const bins = fs.readFileSync(path.join(SRC_DIR, 'bins.js'), 'utf8');
    const dom = fs.readFileSync(path.join(SRC_DIR, '..', '..', '..', '..', 'shared', 'dom.js'), 'utf8');
    const count = fs.readFileSync(path.join(SRC_DIR, '..', '..', '..', '..', 'shared', 'count.js'), 'utf8');
    const els = {};
    const ctx = {
        escapeHtml, PAGE_AUTH: false,
        document: { getElementById: (id) => (els[id] = els[id] || { innerHTML: '' }) },
    };
    vm.createContext(ctx);
    vm.runInContext(extractFn(dom, 'h') + '\n' + extractFn(count, 'linePieces').replace(/^export /, '') + '\n' +
        extractFn(bins, 'renderContents') +
        '\nthis.renderContents = renderContents;', ctx);
    // qtys draws the answer and returns each row's part and Qty cell.
    return function qtys(data) {
        ctx.renderContents(data);
        const out = {};
        const re = /<tr><td><code>([^<]*)<\/code><\/td><td>([^<]*)<\/td>/g;
        let row;
        while ((row = re.exec(els['bd-contents'].innerHTML)) !== null) out[row[1]] = row[2];
        return out;
    };
}
const qtys = loadContents();

console.log('the Contents table counts a bin of one part in its pieces');
{
    const oneLine = [{ part_number: 'SYN-BN-P01', parts_per_cycle: 2 }];
    eq('one line at 2 per assembly cycle: the bin\'s 8, not 8 x 2',
        JSON.stringify(qtys({ bin: bin({ uop_remaining: 8 }), manifest: { items: [{ part_number: 'SYN-BN-P01' }] },
            template_manifest: oneLine })), JSON.stringify({ 'SYN-BN-P01': '8' }));
    eq('one line: a part the template does not list cannot be counted',
        JSON.stringify(qtys({ bin: bin({ uop_remaining: 8 }), manifest: { items: [{ catid: 'SYN-BN-OTHER' }] },
            template_manifest: oneLine })), JSON.stringify({ 'SYN-BN-OTHER': '—' }));
    const kit = [{ part_number: 'SYN-BN-L', parts_per_cycle: 2 }, { part_number: 'SYN-BN-R', parts_per_cycle: 3 }];
    eq('a kit keeps the product: 4 cycles x 2 and x 3',
        JSON.stringify(qtys({ bin: bin({ uop_remaining: 4 }),
            manifest: { items: [{ part_number: 'SYN-BN-L' }, { part_number: 'SYN-BN-R' }] }, template_manifest: kit })),
        JSON.stringify({ 'SYN-BN-L': '8', 'SYN-BN-R': '12' }));
}

// ── bins-live.js: the live feed ─────────────────────────────────────────────
// Driven with a fake clock and a reader whose answers the test settles, so each
// case says exactly when a read goes out and what it names.
function loadLive() {
    let src = fs.readFileSync(path.join(SRC_DIR, 'bins-live.js'), 'utf8');
    src = src.replace(/^export /mg, '');
    const ctx = {};
    vm.createContext(ctx);
    vm.runInContext(src + '\nthis.newRowsFeed = newRowsFeed; this.rowsQuery = rowsQuery; this.MAX_NAMED = MAX_NAMED;', ctx);
    return ctx;
}
const live = loadLive();

// rig is one feed with its clock, its reads and its paints.
function rig() {
    let now = 0, seq = 0;
    const timers = new Map();
    const r = { reads: [], applied: [], hidden: false };
    r.feed = live.newRowsFeed({
        read: (query) => new Promise((resolve, reject) => r.reads.push({ query, resolve, reject })),
        apply: (answer, every) => r.applied.push({ answer, every }),
        hidden: () => r.hidden,
        later: (fn, ms) => { const id = ++seq; timers.set(id, { at: now + ms, fn }); return id; },
        cancel: (id) => { timers.delete(id); },
        quietMs: 400,
        maxWaitMs: 2000,
    });
    // advance runs every timer due within ms, in time order.
    r.advance = (ms) => {
        const end = now + ms;
        for (;;) {
            let next = null;
            for (const [id, t] of timers) if (t.at <= end && (!next || t.at < next.t.at)) next = { id, t };
            if (!next) break;
            timers.delete(next.id);
            now = next.t.at;
            next.t.fn();
        }
        now = end;
    };
    return r;
}
const settled = () => new Promise((resolve) => setImmediate(resolve));

async function feedTests() {
    console.log('live feed: rowsQuery');
    eq('bins and orders', live.rowsQuery(['1', '2'], ['7']), 'ids=1,2&orders=7');
    eq('orders only', live.rowsQuery([], ['7']), 'orders=7');
    eq('nothing named is every row', live.rowsQuery([], []), '');

    console.log('live feed: a burst is one read, once it goes quiet');
    {
        const r = rig();
        r.feed.bin(1); r.feed.bin(2); r.feed.bin(1); r.feed.order(7);
        r.advance(399);
        eq('no read while the burst may go on', r.reads.length, 0);
        r.advance(1);
        eq('one read', r.reads.length, 1);
        eq('it names each bin once, and the order', r.reads[0].query, 'ids=1,2&orders=7');
        r.reads[0].resolve({ rows: [], gone: [] });
        await settled();
        eq('the answer is painted', r.applied.length, 1);
        eq('not as every row', r.applied[0].every, false);
    }

    console.log('live feed: names that keep coming are read at the cap');
    {
        const r = rig();
        for (let t = 0; t < 2000; t += 300) { r.feed.bin(10 + t); r.advance(300); }
        eq('read by the cap, not starved by the stream', r.reads.length, 1);
    }

    console.log('live feed: a name that arrives during a read waits for the next');
    {
        const r = rig();
        r.feed.bin(1);
        r.advance(400);
        r.feed.bin(3);
        r.advance(400);
        eq('no second read while the first is out', r.reads.length, 1);
        r.reads[0].resolve({ rows: [], gone: [] });
        await settled();
        r.advance(400);
        eq('then a second read', r.reads.length, 2);
        eq('naming what waited', r.reads[1].query, 'ids=3');
    }

    console.log('live feed: a hidden tab reads once it is shown');
    {
        const r = rig();
        r.hidden = true;
        r.feed.bin(1); r.feed.bin(2);
        r.advance(10000);
        eq('nothing read while hidden', r.reads.length, 0);
        r.hidden = false;
        r.feed.shown();
        r.advance(400);
        eq('one read on showing', r.reads.length, 1);
        eq('naming what waited', r.reads[0].query, 'ids=1,2');
    }
    {
        const r = rig();
        r.feed.shown();
        r.advance(10000);
        eq('shown with nothing waiting reads nothing', r.reads.length, 0);
    }

    console.log('live feed: every row');
    {
        const r = rig();
        r.feed.bin(4);
        r.feed.every();
        r.advance(400);
        eq('every() reads every row', r.reads[0].query, '');
        r.reads[0].resolve({ rows: [] });
        await settled();
        eq('painted as every row', r.applied[0].every, true);
    }
    {
        const r = rig();
        for (let id = 1; id <= live.MAX_NAMED + 1; id++) r.feed.bin(id);
        r.advance(400);
        eq('more bins than one read names is every row', r.reads[0].query, '');
    }

    console.log('live feed: a failed read loses its names, so the next read is every row');
    {
        const r = rig();
        r.feed.bin(1);
        r.advance(400);
        r.reads[0].reject(new Error('503'));
        await settled();
        r.advance(10000);
        eq('nothing retries on its own', r.reads.length, 1);
        r.feed.bin(5);
        r.advance(400);
        eq('the next name reads every row', r.reads[1].query, '');
    }

    console.log('live feed: only positive ids are names');
    {
        const r = rig();
        r.feed.bin(0); r.feed.bin(undefined); r.feed.order('x');
        r.advance(10000);
        eq('nothing read', r.reads.length, 0);
    }
}

// The UoP cell above a ratio of 1 (owner, 2026-11-19): a bin pulled at 404 at
// 8 a cycle reads 50 with its 404 parts beside it; at 1, and with no ratio, the
// count as it always was.
{
    eq('uop: 404 at 8', m.uopText({ payload_code: 'P-8', uop_remaining: 404 }, 8),
        '50 <span class="text-muted bin-uop-parts">404 parts</span>');
    eq('uop: 404 at 1', m.uopText({ payload_code: 'P-1', uop_remaining: 404 }, 1), '404');
    eq('uop: no ratio given', m.uopText({ payload_code: 'P-1', uop_remaining: 404 }), '404');
    const v = m.binRowView({ bin: bin({ payload_code: 'P-8', uop_remaining: 800 }), recent_orders: [] }, true, { 'P-8': 8 });
    eq('row: a full pack at 8 reads 100', v.uop.html, '100 <span class="text-muted bin-uop-parts">800 parts</span>');
    eq('row: the cell still sorts by the count', v.uop.sort, '800');
}

function finish() {
    if (failures > 0) {
        console.log(failures + ' failure(s)');
        process.exit(1);
    }
    console.log('all passed');
}
feedTests().then(finish, (e) => {
    failures++;
    console.log('  FAIL live feed tests threw — ' + (e && e.stack || e));
    finish();
});
