// The Core picker pins (census rows #50-#71). Run by TestCorePickerPins
// (core_pickers_pins_test.go) as `node pins.test.js <fixture.json>`; prints
// {"goldens": {name: text}, "twinJS": [...]} on stdout and nothing else.
//
// Every page runs unmodified in dom.js's fake browser, driven through its own
// functions (the ones its data-action verbs, listeners and drops call). Per
// page the goldens record:
//   - the option list each picker builds from the fixture plant (Go's
//     corePickerFixture): order, labels, headings, what is left out;
//   - the request each pick sends, in order, method, URL and body as sent;
//   - each refusal path: the answer (refusal-shapes.txt's, by key), what the
//     page shows (toast, status line, button) and what it asks next.
// Bugs are recorded as they are and named where they show ("pinned as is").
//
// Named .test.js because it is a test script, not a page: the repo's text
// rules read *.test.js as fixtures (the cycle modes it reads off
// test-orders.html land in a golden, not in a page).

'use strict';

const fs = require('fs');
const path = require('path');
const D = require('./dom.js');
// The notice is the real shared/notice.js, in a realm of the helper pins'
// harness (one per page): a page's toasts and the door's show on it under its
// own rules (one of each; a sticky one stays), and each act records what it
// left on screen.
const { makeRealm } = require(path.join(__dirname, '..', '..', '..', '..', 'shared', 'testdata', 'helper-pins-harness', 'world.js'));

const fx = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const goldens = {};
let twinJS = [];

// A rejection a page leaves unhandled is an error it never showed anyone.
const unhandled = [];
process.on('unhandledRejection', (e) => unhandled.push(D.normal(String(e))));

// answer is a refusal from refusal-shapes.txt as a fetch answer.
function answer(key) {
    if (key === 'reject') return { reject: 'Failed to fetch' };
    const a = fx.refusals[key];
    if (!a) throw new Error('no refusal ' + key);
    return { status: a.status, text: a.body };
}
// The refusals every write is tried against: a 4xx the handler writes, the
// expired session, a store error, a page that is not JSON, and no answer.
const REFUSALS = ['json.400', 'auth.401', 'json.500', 'proxy.502', 'reject'];
function refusalTitle(key) {
    if (key === 'reject') return 'fetch rejects (TypeError: Failed to fetch)';
    const a = fx.refusals[key];
    return key + ': ' + a.status + ' ' + JSON.stringify(a.body.trim());
}

function newRec() {
    const realm = makeRealm({ surface: 'core' });
    return {
        toasts: [], confirms: [], modals: [], errors: [], timers: [], reloads: 0, confirmAnswer: true,
        realm, notice: realm.load('/static/shared/notice.js'),
    };
}

// noticeToasts takes the toasts off the notice as golden lines, then clears it,
// so each act records only what it showed.
function noticeToasts(rec) {
    const stack = rec.realm.doc.querySelector('.notice-stack');
    const out = [];
    (stack ? stack.children.slice() : []).forEach((t) => {
        const level = (/notice-toast--(\w+)/.exec(t.className) || [])[1] || 'info';
        out.push('toast ' + level + (t.classList.contains('is-sticky') ? ' (sticky)' : '') + ': ' + D.normal(t.querySelector('.notice-text').textContent));
        t.remove();
    });
    return out;
}

// lender lends a page's imports: the recording stubs, then `extra`.
function lender(rec, doc, extra) {
    const stubs = {
        toast(msg, level, opts) { return rec.notice.toast(msg, level, opts); },
        uiConfirm(msg) { rec.confirms.push(msg); return Promise.resolve(rec.confirmAnswer); },
        uiPrompt() { return Promise.resolve(null); },
        showModal(id) { rec.modals.push('show ' + id); },
        hideModal(id) { rec.modals.push('hide ' + id); },
        delegateActions() {}, openFromQuery() {}, enterSubmits() {},
        debounce(fn) { return fn; },
        layerDepth() { return 0; },
        el(tag) { return D.makeEl(tag); },
        setText(id, v) { doc.getElementById(id).textContent = (v === null || v === undefined) ? '—' : v; },
        timeAgoHTML() { return ''; },
        onSSE() {}, installLiveDurations() {}, reconcileList() {}, serverNow() { return 0; },
        formatDuration() { return ''; }, formatTime() { return ''; },
        formatClockSeconds() { return ''; }, plantDate() { return ''; },
        relevantNotices() { return []; }, routeStripHTML() { return ''; }, routeTimelineHTML() { return ''; },
        openNodeModal() { rec.modals.push('openNodeModal'); },
    };
    return (n) => (extra && n in extra ? extra[n] : stubs[n]);
}

function baseGlobals(doc, net, rec) {
    const g = {
        document: doc, fetch: net.fetch, URL,
        console: {
            log() {}, info() {}, warn() {},
            error(...a) { rec.errors.push('console.error ' + a.map((x) => D.normal(String(x))).join(' ')); },
        },
        setTimeout(_fn, ms) { rec.timers.push(ms); return 0; }, clearTimeout() {},
        setInterval() { return 0; }, clearInterval() {},
        location: { href: 'http://core.test/', pathname: '/', search: '', hash: '', reload() { rec.reloads++; } },
        history: { replaceState() {} },
        innerHeight: 900, scrollBy() {},
    };
    g.window = g;
    return g;
}

// flush writes what an act did, then forgets it: its requests, what it
// showed, what it asked, and anything left unhandled.
function flush(L, net, rec, pad) {
    pad = pad || '  ';
    if (!net.log.length) L.push(pad + '(no request)');
    net.log.forEach((r) => L.push(pad + D.requestLine(r)));
    rec.confirms.forEach((m) => L.push(pad + 'confirm: ' + JSON.stringify(m)));
    noticeToasts(rec).forEach((m) => L.push(pad + m));
    rec.errors.forEach((m) => L.push(pad + m));
    rec.modals.forEach((m) => L.push(pad + 'modal: ' + m));
    if (rec.timers.length) L.push(pad + 'timers: ' + rec.timers.join(', ') + ' ms');
    if (rec.reloads) L.push(pad + 'location.reload() x' + rec.reloads);
    unhandled.splice(0).forEach((m) => L.push(pad + 'UNHANDLED REJECTION: ' + m));
    net.clear();
    rec.toasts.length = rec.confirms.length = rec.modals.length = rec.errors.length = rec.timers.length = 0;
    rec.reloads = 0;
}

// seedSelects gives every <select id> of a template its served markup, and
// its value the first option's, as the browser starts it.
function seedSelects(doc, rel) {
    const src = D.readWWW(rel);
    const re = /<select([^>]*\bid="([^"]+)"[^>]*)>([\s\S]*?)<\/select>/g;
    let m;
    while ((m = re.exec(src)) !== null) {
        const e = doc.getElementById(m[2]);
        e.tagName = 'SELECT';
        e.innerHTML = m[3];
        const first = D.parseOptions(m[3])[0];
        e.value = first ? first.value : '';
    }
}

// ============================================================================
// Core Orders · Manual order (census #53-#57, C4 C5 C6; bugs #6, #7)
// ============================================================================

function ordersPage() {
    const rec = newRec();
    const doc = D.makeDocument();
    const net = D.makeNet();
    seedSelects(doc, 'templates/orders.html');
    doc.getElementById('mo-priority').value = '0';
    doc.getElementById('mo-description').value = '';
    doc.getElementById('mo-quantity').value = '1';
    ['mo-bin-group', 'mo-quantity-group', 'mo-staged-payload-group'].forEach((id) => doc.getElementById(id).classList.add('hide'));
    doc.getElementById('mo-staged-payload-readout').textContent = 'Select a source node.';
    const p = D.loadPage('static/pages/orders.js', baseGlobals(doc, net, rec), lender(rec, doc));
    return { p, doc, net, rec };
}

function answerOrderReads(net) {
    net.on('GET', '/api/nodes', { json: fx.nodes })
        .on('GET', '/api/payloads/templates', { json: fx.payloads })
        .on('GET', '/api/bins/available', { json: fx.availableBins });
}

const NODE_SELECTS = ['mo-source', 'mo-delivery', 'mo-staged-source', 'mo-staged-staging', 'mo-staged-delivery', 'mo-moverobot-dest'];

function stagedPayloadShown(t) {
    const doc = t.doc;
    if (!doc.getElementById('mo-staged-payload-group').classList.contains('hide')) {
        return 'picker: ' + D.parseOptions(doc.getElementById('mo-staged-payload').innerHTML)
            .map((o) => JSON.stringify(o.value) + ' ' + o.text).join(', ');
    }
    return 'read-out: ' + JSON.stringify(doc.getElementById('mo-staged-payload-readout').textContent);
}

function moStatus(t) {
    const s = t.doc.getElementById('mo-status');
    return 'mo-status ' + JSON.stringify(D.normal(s.textContent)) + ' color ' + (s.style.color || '(unset)') +
        '; submit button ' + (t.doc.getElementById('manual-order-submit-btn').disabled ? 'disabled' : 'enabled');
}

// setForm sets the manual order's fields and its tab, as a person would.
function setForm(t, tab, fields) {
    t.p.run('_moActiveTab = ' + JSON.stringify(tab));
    Object.keys(fields).forEach((id) => { t.doc.getElementById(id).value = fields[id]; });
    if (tab === 'transport') t.p.run('manualOrderTransportTypeChanged()');
    if (tab === 'staged') t.p.run('stagedSourceChanged()');
}

async function ordersPins() {
    const L = [
        '# Core Orders, manual order modal (census #53-#57; C4 buildNodeOptionsHTML, C5 mo-payload,',
        '# C6 stagedSourceChanged). static/pages/orders.js run as served; the plant is the fixture.',
        '# Option lines are "value" | label, under their <optgroup>.',
        '',
    ];

    let t = ordersPage();
    answerOrderReads(t.net);
    t.p.run('openManualOrderModal()');
    await D.settle();
    L.push('== open the modal (the three reads answer)');
    flush(L, t.net, t.rec);
    const html = t.doc.getElementById('mo-source').innerHTML;
    twinJS = D.parseOptions(html).map((o) => ({ zone: o.group, value: o.value, label: o.text }));
    L.push('mo-source, the node list (C4):');
    D.optionLines(html).forEach((l) => L.push(l));
    NODE_SELECTS.slice(1).forEach((id) => {
        L.push(id + ': ' + (t.doc.getElementById(id).innerHTML === html ? 'the same list' : 'A DIFFERENT LIST'));
    });
    L.push('Pinned as is: census #6, GRP-A (a group of lanes) is under Other; its lane and slots and');
    L.push('GRP-OFF-N1 (member of a disabled group) are not offered at all; _TRANSIT, _ROBOT:AMR-01 and');
    L.push('VIRT-1 are offered as "group"; the disabled OLD-SLOT and GRP-OFF are left out.');
    L.push('mo-payload (C5, /api/payloads/templates in its order, whole catalog):');
    D.optionLines(t.doc.getElementById('mo-payload').innerHTML).forEach((l) => L.push(l));
    L.push('mo-bin (Retrieve specific, /api/bins/available):');
    D.optionLines(t.doc.getElementById('mo-bin').innerHTML).forEach((l) => L.push(l));
    L.push('staged payload after the load: ' + stagedPayloadShown(t));

    t.p.run('closeManualOrderModal(); openManualOrderModal()');
    await D.settle();
    L.push('', '== close and open again');
    flush(L, t.net, t.rec);

    L.push('', '== staged payload (C6) per source picked');
    ['GRP-A', 'GRP-B', 'GRP-B-N1', 'GRP-B-N2', 'LINE-1', 'SPOT-9', '_TRANSIT', 'VIRT-1', 'NOT-A-NODE', ''].forEach((name) => {
        t.doc.getElementById('mo-staged-source').value = name;
        t.p.run('stagedSourceChanged()');
        L.push('  ' + JSON.stringify(name) + ' -> ' + stagedPayloadShown(t));
    });
    flush(L, t.net, t.rec);

    // --- bodies ---------------------------------------------------------
    t.net.on('POST', '/api/orders/spot', { json: { order_id: 41, status: 'dispatched' } })
        .on('POST', '/api/robots/move', { json: { destination: 'LINE-1' } });
    const cases = [
        ['transport move GRP-B-N1 -> LINE-1 (payload set, not sent)', 'transport',
            { 'mo-transport-type': 'move', 'mo-source': 'GRP-B-N1', 'mo-delivery': 'LINE-1', 'mo-payload': 'PART-A' }],
        ['transport retrieve PART-A x3 to LINE-1 (source not sent)', 'transport',
            { 'mo-transport-type': 'retrieve', 'mo-source': 'GRP-B-N1', 'mo-delivery': 'LINE-1', 'mo-payload': 'PART-A', 'mo-quantity': '3' }],
        ['transport retrieve_empty to the group GRP-B, no payload', 'transport',
            { 'mo-transport-type': 'retrieve_empty', 'mo-delivery': 'GRP-B', 'mo-payload': '', 'mo-quantity': '1' }],
        ['transport retrieve_specific BIN-1 to _TRANSIT (offered, so sendable)', 'transport',
            { 'mo-transport-type': 'retrieve_specific', 'mo-bin': 'BIN-1', 'mo-delivery': '_TRANSIT' }],
        ['transport move with no source', 'transport', { 'mo-transport-type': 'move', 'mo-source': '', 'mo-delivery': 'LINE-1' }],
        ['staged from the group GRP-B, payload PART-C picked', 'staged',
            { 'mo-staged-source': 'GRP-B', 'mo-staged-staging': 'SPOT-9', 'mo-staged-delivery': 'LINE-1' }, { 'mo-staged-payload': 'PART-C' }],
        ['staged from the node LINE-1 (no payload sent)', 'staged',
            { 'mo-staged-source': 'LINE-1', 'mo-staged-staging': 'SPOT-9', 'mo-staged-delivery': 'GRP-B-N1' }],
        ['staged with no staging node', 'staged',
            { 'mo-staged-source': 'LINE-1', 'mo-staged-staging': '', 'mo-staged-delivery': 'GRP-B-N1' }],
        ['move robot to VIRT-1', 'move_robot', { 'mo-moverobot-dest': 'VIRT-1' }],
        ['move robot, nothing picked', 'move_robot', { 'mo-moverobot-dest': '' }],
    ];
    L.push('', '== the body each pick sends (an answer of 200)');
    for (const c of cases) {
        setForm(t, c[1], c[2]);
        Object.keys(c[3] || {}).forEach((id) => { t.doc.getElementById(id).value = c[3][id]; });
        t.p.run('submitManualOrder()');
        await D.settle();
        L.push('-- ' + c[0]);
        flush(L, t.net, t.rec);
        L.push('  ' + moStatus(t));
        t.doc.getElementById('mo-status').textContent = '';
        t.doc.getElementById('manual-order-submit-btn').disabled = false;
    }

    // --- refusals -------------------------------------------------------
    L.push('', '== refusals: staged from LINE-1 (the handler\'s own 400 shape)');
    t.net.on('POST', '/api/orders/spot', answer('orders.spot.400'));
    setForm(t, 'staged', { 'mo-staged-source': 'LINE-1', 'mo-staged-staging': 'SPOT-9', 'mo-staged-delivery': 'GRP-B-N1' });
    t.p.run('submitManualOrder()');
    await D.settle();
    L.push('-- ' + refusalTitle('orders.spot.400'));
    flush(L, t.net, t.rec);
    L.push('  ' + moStatus(t));
    t.doc.getElementById('manual-order-submit-btn').disabled = false;
    L.push('', '== refusals: transport move GRP-B-N1 -> LINE-1');
    for (const key of REFUSALS) {
        t.net.on('POST', '/api/orders/spot', answer(key));
        setForm(t, 'transport', { 'mo-transport-type': 'move', 'mo-source': 'GRP-B-N1', 'mo-delivery': 'LINE-1' });
        t.p.run('submitManualOrder()');
        await D.settle();
        L.push('-- ' + refusalTitle(key));
        flush(L, t.net, t.rec);
        L.push('  ' + moStatus(t));
        t.doc.getElementById('manual-order-submit-btn').disabled = false;
    }
    L.push('', '== refusals: move robot to VIRT-1');
    for (const key of ['robots.move.400', 'json.500', 'reject']) {
        t.net.on('POST', '/api/robots/move', answer(key));
        setForm(t, 'move_robot', { 'mo-moverobot-dest': 'VIRT-1' });
        t.p.run('submitManualOrder()');
        await D.settle();
        L.push('-- ' + refusalTitle(key));
        flush(L, t.net, t.rec);
        L.push('  ' + moStatus(t));
        t.doc.getElementById('manual-order-submit-btn').disabled = false;
    }

    // --- census #7 ------------------------------------------------------
    L.push('', '== census #7 (pinned as is): the first /api/nodes read fails');
    t = ordersPage();
    t.net.on('GET', '/api/nodes', answer('json.500'))
        .on('GET', '/api/payloads/templates', answer('proxy.502'))
        .on('GET', '/api/bins/available', answer('reject'));
    t.p.run('openManualOrderModal()');
    await D.settle();
    L.push('-- open');
    flush(L, t.net, t.rec);
    NODE_SELECTS.concat(['mo-payload', 'mo-bin']).forEach((id) => {
        L.push('  ' + id + ': ' + D.parseOptions(t.doc.getElementById(id).innerHTML).map((o) => o.text).join(' / '));
    });
    L.push('  staged payload: ' + stagedPayloadShown(t));
    t.net.on('GET', '/api/nodes', { json: fx.nodes });
    t.p.run('closeManualOrderModal(); openManualOrderModal()');
    await D.settle();
    L.push('-- second open: no request (the read is never asked again; _moNodesLoaded was set before it)');
    flush(L, t.net, t.rec);
    L.push('  mo-source: ' + D.parseOptions(t.doc.getElementById('mo-source').innerHTML).map((o) => o.text).join(' / '));
    setForm(t, 'transport', { 'mo-transport-type': 'move' });
    t.p.run('submitManualOrder()');
    await D.settle();
    L.push('-- submit a move from the Loading... selects');
    flush(L, t.net, t.rec);
    L.push('  ' + moStatus(t));

    goldens['orders-manual-order-page.txt'] = L.join('\n') + '\n';
}

// ============================================================================
// Core Bins · bin pop-up Move and Load, Create bin's label check (census #50-#52, C2 C3)
// ============================================================================

function binsPage() {
    const rec = newRec();
    const doc = D.makeDocument();
    const net = D.makeNet();
    const g = baseGlobals(doc, net, rec);
    // As bins.html's inline script sets them (handleBins' four JSON values).
    g.PAGE_NODES = fx.nodes;
    g.PAGE_PAYLOADS = fx.payloads;
    g.PAGE_BIN_TYPES = fx.binTypes;
    g.PAGE_PAYLOAD_BIN_TYPES = fx.payloadBinTypes;
    g.PAGE_AUTH = true;
    // bins.js's own two modules, run as they are.
    const echo = D.loadPage('static/pages/bins-echo.js', { console: g.console }, () => undefined);
    const row = D.loadPage('static/pages/bins-row.js', { console: g.console }, () => undefined);
    const p = D.loadPage('static/pages/bins.js', g, lender(rec, doc, {
        echoesFor: echo.ctx.echoesFor, newEchoLedger: echo.ctx.newEchoLedger,
        binRowView: row.ctx.binRowView, uopText: row.ctx.uopText,
    }));
    return { p, doc, net, rec };
}

function binAt(nodeID, binTypeID, extra) {
    const n = fx.nodes.find((x) => x.id === nodeID);
    return Object.assign({ id: 50, label: 'BIN-50', bin_type_id: binTypeID, bin_type_code: 'CART-S', status: 'available',
        node_id: nodeID, node_name: n ? n.name : '', payload_code: '', uop_remaining: 0, locked: false,
        manifest_confirmed: false, description: '' }, extra || {});
}

async function binsPins() {
    const L = [
        '# Core Bins, bin pop-up (census #51 Move C2, #52 Load C3) and Create bin\'s label check.',
        '# static/pages/bins.js run as served over PAGE_NODES / PAGE_PAYLOADS = the fixture.',
        '# (The Create bin Node select itself is server-rendered: bins-template.txt.)',
        '',
    ];
    const t = binsPage();

    L.push('== Move destination (C2): renderActions, bin at GRP-A-L1-S1');
    await t.p.run('renderActions(' + JSON.stringify({ bin: binAt(3, 1) }) + ')');
    D.optionLines(D.selectIn(t.doc.getElementById('bd-actions').innerHTML, 'bd-move-node').inner).forEach((l) => L.push(l));
    L.push('  dropped: the bin\'s own node, _TRANSIT, _ROBOT:*; kept: groups, lanes, the disabled OLD-SLOT');
    L.push('  and GRP-OFF, VIRT-1. No headings, no badges, value = id, label = name (not display name).');
    await t.p.run('renderActions(' + JSON.stringify({ bin: binAt(null, 1) }) + ')');
    const unassigned = D.parseOptions(D.selectIn(t.doc.getElementById('bd-actions').innerHTML, 'bd-move-node').inner);
    L.push('  an unassigned bin: ' + (unassigned.length - 1) + ' destinations, GRP-A-L1-S1 ' +
        (unassigned.some((o) => o.text === 'GRP-A-L1-S1') ? 'included' : 'left out'));

    L.push('', '== Load payload (C3): renderContents, per bin type (PAGE_PAYLOAD_BIN_TYPES filters)');
    for (const bt of [1, 2, 3]) {
        t.p.run('renderContents(' + JSON.stringify({ bin: binAt(3, bt), manifest: null }) + ')');
        const sel = D.selectIn(t.doc.getElementById('bd-contents').innerHTML, 'bd-load-payload');
        L.push('  bin type ' + bt + ': ' + D.parseOptions(sel.inner).map((o) => JSON.stringify(o.value)).join(', '));
    }
    t.p.run('renderContents(' + JSON.stringify({ bin: binAt(3, 1), manifest: null, load_refusal: 'Held by order 41 until it is delivered.' }) + ')');
    const held = t.doc.getElementById('bd-contents').innerHTML;
    L.push('  held (load_refusal set): select ' + (/id="bd-load-payload"[^>]*disabled/.test(held) ? 'disabled' : 'enabled') +
        ', sentence shown: ' + (held.indexOf('Held by order 41') >= 0) + ', Load button ' +
        (/<button[^>]*disabled>Load<\/button>/.test(held) ? 'disabled' : 'live'));
    flush(L, t.net, t.rec);

    // --- bodies ---------------------------------------------------------
    t.p.run('currentBinId = 50; currentBinData = ' + JSON.stringify({ bin: binAt(3, 1) }));
    t.net.on('POST', '/api/bins/action', { json: { status: 'ok', bin: binAt(8, 1) } })
        .on('POST', '/api/bins/request-move', { json: { order_id: 41, status: 'queued', queue_reason: 'the lane is busy', to: 'GRP-B-N1' } });
    const val = (id, v) => { t.doc.getElementById(id).value = v; };
    L.push('', '== the body each pick sends (an answer of 200)');
    const cases = [
        ['Move to LINE-1', () => { val('bd-move-node', '8'); t.p.run('moveBin()'); }],
        ['Move, nothing picked', () => { val('bd-move-node', ''); t.p.run('moveBin()'); }],
        ['Move to where it is', () => { val('bd-move-node', '3'); t.p.run('moveBin()'); }],
        ['Request Move to GRP-B-N1 (the detail re-reads never answer here)', () => { val('bd-move-node', '6'); t.p.run('requestMove()'); }],
        ['Load PART-A, count blank', () => { val('bd-load-payload', 'PART-A'); val('bd-load-uop', ''); t.p.run('loadPayload()'); }],
        ['Load PART-A, count 0', () => { val('bd-load-payload', 'PART-A'); val('bd-load-uop', '0'); t.p.run('loadPayload()'); }],
        ['Load, nothing picked', () => { val('bd-load-payload', ''); t.p.run('loadPayload()'); }],
    ];
    for (const c of cases) {
        c[1]();
        await D.settle();
        L.push('-- ' + c[0]);
        flush(L, t.net, t.rec);
    }

    L.push('', '== refusals: Move to LINE-1');
    for (const key of REFUSALS) {
        t.net.on('POST', '/api/bins/action', answer(key));
        val('bd-move-node', '8');
        t.p.run('moveBin()');
        await D.settle();
        L.push('-- ' + refusalTitle(key));
        flush(L, t.net, t.rec);
    }
    L.push('', '== refusals: Request Move to GRP-B-N1');
    for (const key of ['json.409', 'reject']) {
        t.net.on('POST', '/api/bins/request-move', answer(key));
        val('bd-move-node', '6');
        t.p.run('requestMove()');
        await D.settle();
        L.push('-- ' + refusalTitle(key));
        flush(L, t.net, t.rec);
    }
    L.push('', '== refusals: Load PART-A');
    for (const key of ['json.400', 'reject']) {
        t.net.on('POST', '/api/bins/action', answer(key));
        val('bd-load-payload', 'PART-A');
        val('bd-load-uop', '');
        t.p.run('loadPayload()');
        await D.settle();
        L.push('-- ' + refusalTitle(key));
        flush(L, t.net, t.rec);
    }

    // --- Create bin's submit gate --------------------------------------
    L.push('', '== Create bin: confirmBinCreate, the label checked against PAGE_NODES (every node)');
    for (const c of [['LINE-1', '1'], ['OLD-SLOT', '1'], ['_TRANSIT', '1'], ['GRP-A', '1'], ['CART-7', '1'], ['CART-', '3']]) {
        const label = D.makeEl('input');
        label.value = c[0];
        const qty = D.makeEl('input');
        qty.value = c[1];
        const form = D.makeEl('form');
        form.qsa = (sel) => (sel === 'input[name=label_prefix]' ? [label] : sel === 'input[name=quantity]' ? [qty] : null);
        let prevented = 0;
        await t.p.ctx.confirmBinCreate(form, { preventDefault() { prevented++; } });
        await D.settle();
        L.push('-- label ' + JSON.stringify(c[0]) + ' x' + c[1] + ': checked as ' +
            JSON.stringify(t.p.ctx.batchLabelsPreview(c[0], Number(c[1]))) +
            '; default prevented ' + prevented + ', form.submit() x' + form.submitted);
        flush(L, t.net, t.rec);
    }

    goldens['bins-page.txt'] = L.join('\n') + '\n';
}

// ============================================================================
// Core Nodes · node pop-up · kanban Overflow and Sources (census #63-#64, C9 C10)
// ============================================================================

// pageTiles are the Nodes page's tiles as nodes.html draws them: every node
// but _TRANSIT (nodes_page_data.go drops it), data-* from the node.
function pageTiles(extra) {
    return fx.nodes.filter((n) => n.name !== '_TRANSIT').map((n) => {
        const t = D.makeEl('div');
        t.className = 'node-tile' + (n.enabled ? '' : ' tile-disabled') + (n.is_synthetic ? ' tile-synthetic' : '');
        t.dataset = {
            id: String(n.id), name: n.name, typeCode: n.node_type_code || '', synthetic: String(!!n.is_synthetic),
            parentId: n.parent_id ? String(n.parent_id) : '', parentName: n.parent_name || '', enabled: String(n.enabled),
            hasPayload: 'false', hasEmptyBin: 'false',
        };
        if (extra) extra(n, t);
        return t;
    });
}

function kanbanPage() {
    const rec = newRec();
    const doc = D.makeDocument();
    const net = D.makeNet();
    doc.getElementById('page-data').dataset = { authenticated: 'true', edges: '[]', binTypes: '[]' };
    const tiles = pageTiles();
    doc.qsa = (sel) => (sel === '.node-tile' ? tiles : null);
    doc.getElementById('nf-kanban').classList.add('hide');
    const p = D.loadPage('static/pages/nodes-kanban.js', baseGlobals(doc, net, rec), lender(rec, doc));
    return { p, doc, net, rec };
}

const KANBAN_CFG = {
    strict_sourcing: false, maintenance_station: '', overflow_destination: 'GRP-GONE',
    sources: [{ source_node_id: 5, source_node_name: 'GRP-B' }], levels: [], layer_wants: [], supports: [],
};

async function openKanban(t, groupID, cfg) {
    t.net.on('GET', '/api/nodes/kanban?id=', { json: cfg })
        .on('GET', '/api/nodes/process-options', { json: { processes: [] } });
    t.p.run('renderKanbanSection(true, ' + groupID + ')');
    await D.settle();
}

function kanbanShown(L, t) {
    const ov = t.doc.getElementById('nf-kanban-overflow');
    L.push('  Overflow (C9) value ' + JSON.stringify(ov.value) + ':');
    D.optionLines(ov.innerHTML, '    ').forEach((l) => L.push(l));
    const host = t.doc.getElementById('nf-kanban-sources').innerHTML;
    const chips = (host.match(/<span class="loader-chip">([^<]*)<\/span>/g) || []).map((s) => D.unesc(s.replace(/<[^>]+>/g, '')));
    L.push('  Sources (C10), ranked: ' + (chips.length ? chips.join(', ') : '(none) ' +
        JSON.stringify(D.unesc((/<div class="text-muted"[^>]*>([^<]*)<\/div>/.exec(host) || [])[1] || ''))));
    const add = D.selectIn(host, 'nf-kanban-add-source');
    if (!add) { L.push('  Add a source: no select (nothing left to add)'); return; }
    L.push('  Add a source (one pick, then Add):');
    D.optionLines(add.inner, '    ').forEach((l) => L.push(l));
}

async function kanbanPins() {
    const L = [
        '# Core Nodes, node pop-up, kanban section (census #63 Overflow C9, #64 Sources C10).',
        '# static/pages/nodes-kanban.js run as served. otherGroups() reads the page\'s tiles (every node',
        '# but _TRANSIT, as nodes.html draws them): every NGRP tile, the open group left out, by name.',
        '',
    ];
    let t = kanbanPage();
    await openKanban(t, 1, KANBAN_CFG);
    L.push('== open GRP-A (stored: overflow GRP-GONE, a group no longer on the page; source GRP-B)');
    flush(L, t.net, t.rec);
    kanbanShown(L, t);
    L.push('  The disabled GRP-OFF is offered in both; lanes, plain nodes and VIRT-1 are not.');

    t.doc.getElementById('nf-kanban-add-source').value = '14';
    t.p.run('addKanbanSource()');
    L.push('', '== Add GRP-OFF as a source');
    kanbanShown(L, t);

    t.net.on('POST', '/api/nodes/kanban/', { json: { status: 'ok', warnings: [] } });
    L.push('', '== Save (saveKanban, an answer of 200 to each post): returns ' + await t.p.run('saveKanban()'));
    await D.settle();
    flush(L, t.net, t.rec);
    L.push('  The stale overflow GRP-GONE is written back as it was shown.');

    L.push('', '== open GRP-B (no stored overflow, no sources)');
    t = kanbanPage();
    await openKanban(t, 5, Object.assign({}, KANBAN_CFG, { overflow_destination: '', sources: [] }));
    flush(L, t.net, t.rec);
    kanbanShown(L, t);

    L.push('', '== the group\'s read fails');
    t = kanbanPage();
    t.net.on('GET', '/api/nodes/kanban?id=', answer('json.500'))
        .on('GET', '/api/nodes/process-options', { json: { processes: [] } });
    t.p.run('renderKanbanSection(true, 1)');
    await D.settle();
    flush(L, t.net, t.rec);
    L.push('  section hidden: ' + t.doc.getElementById('nf-kanban').classList.contains('hide') +
        '; Save then returns ' + await t.p.run('saveKanban()') + ' and posts nothing');
    flush(L, t.net, t.rec);

    // --- refusals -------------------------------------------------------
    // Settings and supports go through postConfirmable (its own fetch,
    // rawPost); the level and the sources through apiPost.
    const runs = [
        ['sources', ['kanban.409', 'kanban.sources.400'].concat(REFUSALS)],
        ['settings', ['kanban.409', 'kanban.409.needs_force', 'kanban.settings.400', 'proxy.502', 'reject']],
    ];
    for (const r of runs) {
        L.push('', '== refusals: Save, the ' + r[0] + ' post refused (the others answer 200)');
        for (const key of r[1]) {
            for (const yes of key === 'kanban.409.needs_force' ? [true, false] : [true]) {
                t = kanbanPage();
                await openKanban(t, 1, KANBAN_CFG);
                t.net.clear();
                t.rec.confirmAnswer = yes;
                t.net.on('POST', '/api/nodes/kanban/', { json: { status: 'ok', warnings: [] } })
                    .on('POST', '/api/nodes/kanban/' + r[0], (method, url, body) =>
                        (JSON.parse(body).force ? { json: { status: 'ok', warnings: [] } } : answer(key)));
                const ok = await t.p.run('saveKanban()');
                await D.settle();
                L.push('-- ' + refusalTitle(key) + (key === 'kanban.409.needs_force' ? (yes ? ', answered yes' : ', answered no') : '') +
                    ': saveKanban returns ' + ok);
                flush(L, t.net, t.rec);
            }
        }
    }

    goldens['kanban-page.txt'] = L.join('\n') + '\n';
}

// ============================================================================
// Core Nodes · stations (census #65-#69, C11 tap-to-assign, C12 drag-drop,
// C13 payloadSelect/fillPayloadSelect, C14 payloadChipsHtml)
// ============================================================================

// Two stations: UNL-A, an unloader with shared windows (window GRP-A-L1-S1,
// fulls from GRP-B, part PART-B), and LDR-B, a loader with one spot per part
// (GRP-B-N2 holding PART-C, empties from GRP-A). /api/loader/list's shape.
const STATIONS = [
    { loader: { id: 20, name: 'UNL-A', role: 'consume', layout: 'shared_window', replenishment: 'operator',
        inbound_source: 'GRP-B', outbound_dest: '', funnel_windows: false, fed_directly: false },
    homes: [{ position_node_id: 3, sort_order: 0, home_kind: 'home', payload_code: '', uop_threshold: 0 }],
    payloads: [{ payload_code: 'PART-B' }] },
    { loader: { id: 21, name: 'LDR-B', role: 'produce', layout: 'dedicated_positions', replenishment: 'threshold',
        inbound_source: 'GRP-A', outbound_dest: '', funnel_windows: false, fed_directly: false },
    homes: [{ position_node_id: 7, sort_order: 0, home_kind: 'home', payload_code: 'PART-C', uop_threshold: 0 }],
    payloads: [] },
];

function loadersPage() {
    const rec = newRec();
    const doc = D.makeDocument();
    const net = D.makeNet();
    doc.getElementById('page-data').dataset = { authenticated: 'true' };
    const TILE = '#nodes-drop-area .node-tile:not(.loader-member)';
    // After nodes-supermarket.js buildHierarchy: group and lane tiles are
    // absorbed (hidden) into a group card, still .node-tile on the grid.
    const tiles = pageTiles((n, t) => {
        if (n.node_type_code === 'NGRP' || n.node_type_code === 'LANE') t.classList.add('smkt-absorbed');
        if (n.name === 'GRP-A-L1-S1') t.dataset.hasPayload = 'true';
        if (n.name === 'LINE-1') t.dataset.hasEmptyBin = 'true';
        t.closestMap = { [TILE]: t };
    });
    const groups = fx.nodes.filter((n) => n.node_type_code === 'NGRP').map((n) => {
        const g = D.makeEl('div');
        g.className = 'smkt-group';
        g.dataset.smktId = String(n.id);
        const hdr = D.makeEl('div');
        hdr.className = 'smkt-header';
        const name = D.makeEl('span');
        name.className = 'smkt-name';
        name.textContent = n.name;
        g.qsa = (sel) => (sel === '.smkt-header' ? [hdr] : sel === '.smkt-name' ? [name] : null);
        hdr.closestMap = { '#nodes-drop-area .smkt-header': hdr, '.smkt-group': g };
        return { g, hdr, n };
    });
    doc.qsa = (sel) => (sel === TILE ? tiles : sel === '#nodes-drop-area .smkt-group' ? groups.map((x) => x.g) : []);
    const p = D.loadPage('static/pages/loaders.js', baseGlobals(doc, net, rec), lender(rec, doc));
    const t = { p, doc, net, rec, tiles, groups };
    t.tile = (name) => tiles.find((x) => x.dataset.name === name);
    t.header = (name) => groups.find((x) => x.n.name === name).hdr;
    return t;
}

function answerStations(net) {
    net.on('GET', '/api/nodes', { json: fx.nodes })
        .on('GET', '/api/payloads', { json: fx.payloads })
        .on('GET', '/api/loader/list', { json: { loaders: STATIONS } })
        .on('GET', '/api/bin-types', { json: { bin_types: [] } })
        .on('POST', '/api/loader/', { json: { status: 'ok' } });
}

async function stationsReady() {
    const t = loadersPage();
    answerStations(t.net);
    t.p.run('refresh()');
    await D.settle();
    t.net.clear();
    return t;
}

function decoration(el) {
    const cls = ['assign-lit', 'assign-dim', 'assign-current'].find((c) => el.classList.contains(c));
    const note = el.children.find((c) => c.classList.contains('assign-note'));
    return (cls ? cls.replace('assign-', '') : '(not decorated)') + (note ? ' "' + note.textContent + '"' : '') +
        (el.classList.contains('assign-filtered') ? '  (filtered out by Find)' : '');
}

function decorated(L, t) {
    t.tiles.forEach((x) => L.push('    tile  ' + x.dataset.name.padEnd(14) + (x.classList.contains('smkt-absorbed') ? ' (absorbed) ' : '            ') + decoration(x)));
    t.groups.forEach((x) => L.push('    group ' + x.n.name.padEnd(14) + '            ' + decoration(x.hdr)));
}

function tap(t, el) {
    t.p.ctx.onArmedClick({ target: el, preventDefault() {}, stopPropagation() {} });
}

function dragEvent(types, target, clientX) {
    return {
        preventDefault() {}, stopPropagation() {}, target, clientX: clientX || 0,
        dataTransfer: { types: Object.keys(types), getData(k) { return types[k] || ''; }, dropEffect: '' },
    };
}

function box(lid, memberIDs) {
    const b = D.makeEl('div');
    b.dataset.loaderId = String(lid);
    const members = memberIDs.map((id, i) => {
        const m = D.makeEl('div');
        m.className = 'loader-member';
        m.dataset = { id: String(id), kind: 'home' };
        m.rect = { left: i * 100, width: 100, top: 0, height: 40 };
        return m;
    });
    b.qsa = (sel) => (sel === '.loader-member' ? members : null);
    return b;
}

function placeSlot(lid, slot) {
    const s = D.makeEl('div');
    s.dataset = { loaderId: String(lid), slot };
    return s;
}

// refreshCallers names each top-level function in loaders.js that calls
// refresh(), once per call, so a change to how many writes re-read the plant
// shows here.
function refreshCallers() {
    const src = D.readWWW('static/pages/loaders.js').split('\n');
    let fn = '(top level)';
    const out = [];
    src.forEach((line) => {
        const m = /^(?:async )?function (\w+)\(/.exec(line);
        if (m) fn = m[1];
        if (/function refresh\(/.test(line)) return;
        const n = (line.match(/\.then\(refresh\)|[^\w.]refresh\(\)/g) || []).length;
        for (let i = 0; i < n; i++) out.push(fn);
    });
    return out;
}

async function loadersPins() {
    const L = [
        '# Core Nodes, stations (census #65-#69): tap-to-assign C11, drag-drop C12, the per-spot part',
        '# picker C13, the shared-window part checklist C14. static/pages/loaders.js run as served over',
        '# the page\'s tiles (every node but _TRANSIT; group and lane tiles absorbed into group cards).',
        '# Stations: UNL-A (shared windows; window GRP-A-L1-S1; fulls from GRP-B; part PART-B),',
        '# LDR-B (one spot per part; GRP-B-N2 holds PART-C; empties from GRP-A).',
        '',
    ];

    let t = loadersPage();
    answerStations(t.net);
    t.doc.listeners.filter((x) => x.type === 'DOMContentLoaded').forEach((x) => x.fn());
    await D.settle();
    L.push('== page load (init)');
    flush(L, t.net, t.rec);
    const calls = refreshCallers();
    L.push('', '== refresh() re-reads /api/nodes, /api/payloads and /api/loader/list; called ' + calls.length + ' times, from:');
    L.push('  ' + calls.join(', '));

    // --- C11: what each armed slot lights --------------------------------
    t = await stationsReady();
    for (const a of [[20, 'windows', 'UNL-A Windows'], [20, 'inbound', 'UNL-A "Fulls come from"'],
        [20, 'outbound', 'UNL-A "Empties go to"'], [21, 'windows', 'LDR-B Windows (one spot per part)']]) {
        t.p.run('armSlotFor(' + a[0] + ', ' + JSON.stringify(a[1]) + ')');
        L.push('', '== armed: ' + a[2] + '; the grid as decorated (assignReason via decorateGrid)');
        L.push('  bar: ' + JSON.stringify(D.unesc((/<span class="loader-assign-text">([^<]*)<\/span>/.exec(t.doc.getElementById('loader-assign-bar').innerHTML) || [])[1] || '')));
        decorated(L, t);
    }
    L.push('  Pinned as is: a place slot lights the robot deck, VIRT-1, the disabled OLD-SLOT and the');
    L.push('  disabled group GRP-OFF; a Windows slot lights OLD-SLOT. Only lanes and own windows are dimmed.');
    t.p.run('armSlotFor(20, "outbound")');
    const find = D.makeEl('input');
    find.value = 'grp-b';
    t.p.ctx.filterAssign(find);
    L.push('', '== armed UNL-A "Empties go to", Find "grp-b": what the filter hides');
    L.push('  ' + t.tiles.concat(t.groups.map((x) => x.hdr)).filter((x) => !x.classList.contains('assign-filtered'))
        .map((x) => x.dataset.name || t.groups.find((g) => g.hdr === x).n.name + ' (header)').join(', ') + ' stay; the rest are filtered');
    t.p.run('disarmSlot()');
    flush(L, t.net, t.rec);

    // --- the two representative writes, and the taps around them ----------
    L.push('', '== tap to assign: the requests, in order (each write is followed by the re-read)');
    t = await stationsReady();
    t.p.run('armSlotFor(20, "outbound")');
    tap(t, t.header('GRP-A'));
    await D.settle();
    L.push('-- UNL-A "Empties go to", tap the GRP-A header (one update, then refresh)');
    flush(L, t.net, t.rec);
    L.push('  armed after: ' + JSON.stringify(t.p.run('armed')));
    t.p.run('armSlotFor(20, "windows")');
    tap(t, t.tile('LINE-1'));
    await D.settle();
    L.push('-- UNL-A Windows, tap LINE-1 (set-home, reorder with it last, then refresh)');
    flush(L, t.net, t.rec);
    L.push('  armed after: ' + JSON.stringify(t.p.run('armed')));
    tap(t, t.tile('_ROBOT:AMR-01'));
    await D.settle();
    L.push('-- UNL-A Windows, tap _ROBOT:AMR-01 (dimmed: a tap does nothing)');
    flush(L, t.net, t.rec);
    t.p.run('armSlotFor(20, "inbound")');
    tap(t, t.header('GRP-B'));
    await D.settle();
    L.push('-- UNL-A "Fulls come from", tap GRP-B (already here: dimmed as current)');
    flush(L, t.net, t.rec);

    // --- C12: drag and drop --------------------------------------------------
    L.push('', '== drag and drop (C12)');
    t = await stationsReady();
    let b = box(20, [3]);
    t.p.ctx.onBoxDrop.call(b, dragEvent({ 'text/plain': '9' }, b, 500));
    await D.settle();
    L.push('-- drop the SPOT-9 tile on UNL-A\'s box, after GRP-A-L1-S1');
    flush(L, t.net, t.rec);
    t.p.ctx.onBoxDrop.call(b, dragEvent({ 'text/plain': '9' }, b, 10));
    await D.settle();
    L.push('-- drop the SPOT-9 tile on UNL-A\'s box, before GRP-A-L1-S1');
    flush(L, t.net, t.rec);
    t.p.ctx.onBoxDrop.call(b, dragEvent({ 'application/x-node-group': 'GRP-B' }, b, 10));
    await D.settle();
    L.push('-- drop the GRP-B header on UNL-A\'s box');
    flush(L, t.net, t.rec);
    for (const d of [
        ['the SPOT-9 tile on "Empties go to"', 'outbound', { 'text/plain': '9' }],
        ['the GRP-A-L1 (lane) tile on "Empties go to"', 'outbound', { 'text/plain': '2' }],
        ['the GRP-A-L1-S1 tile (its own window) on "Empties go to"', 'outbound', { 'text/plain': '3' }],
        ['the GRP-B header on "Fulls come from" (already there)', 'inbound', { 'application/x-node-group': 'GRP-B' }],
        ['a drag carrying _TRANSIT\'s id (no tile carries it; /api/nodes lists it) on "Empties go to"', 'outbound', { 'text/plain': '10' }],
    ]) {
        const s = placeSlot(20, d[1]);
        t.p.ctx.onPlaceDrop.call(s, dragEvent(d[2], s));
        await D.settle();
        L.push('-- drop ' + d[0]);
        flush(L, t.net, t.rec);
    }

    // --- C13 and C14: the part pickers ---------------------------------------
    L.push('', '== C13: LDR-B\'s per-spot part picker (payloadSelect at rest, fillPayloadSelect on open)');
    L.push('  at rest, PART-C picked: ' + t.p.run('payloadSelect("PART-C")'));
    L.push('  at rest, none picked:   ' + t.p.run('payloadSelect("")'));
    const s = D.makeEl('select');
    s.value = 'PART-C';
    t.p.ctx.fillPayloadSelect(s);
    L.push('  opened (the whole catalog, /api/payloads order), value kept ' + JSON.stringify(s.value) + ':');
    D.optionLines(s.innerHTML, '    ').forEach((l) => L.push(l));
    t.p.ctx.setMemberPayload(21, '7', 'PART-A');
    await D.settle();
    L.push('-- pick PART-A on GRP-B-N2');
    flush(L, t.net, t.rec);

    L.push('', '== C14: UNL-A\'s parts (payloadChipsHtml): chips, then the whole-catalog checklist');
    const chipsHtml = t.p.run('payloadChipsHtml(loaderItem(20))');
    L.push('  chips: ' + (chipsHtml.match(/<span class="loader-chip">([^<]*)<\/span>/g) || []).map((x) => x.replace(/<[^>]+>/g, '')).join(', '));
    (chipsHtml.match(/<input type="checkbox" class="loader-pc-cb"[^>]*>/g) || []).forEach((x) => {
        L.push('    [' + (/ checked/.test(x) ? 'x' : ' ') + '] ' + /data-pc="([^"]*)"/.exec(x)[1]);
    });
    const checks = { 'PART-A': true, 'PART-B': false, 'PART-C': false };
    const pbox = D.makeEl('div');
    pbox.qsa = (sel) => (sel === '.loader-pc-cb'
        ? Object.keys(checks).map((pc) => Object.assign(D.makeEl('input'), { dataset: { pc }, checked: checks[pc] })) : null);
    const btn = D.makeEl('button');
    t.p.ctx.savePayloads(20, pbox, btn);
    await D.settle();
    L.push('-- tick PART-A, untick PART-B, Save parts');
    flush(L, t.net, t.rec);

    // --- refusals ----------------------------------------------------------------
    L.push('', '== refusals: UNL-A "Empties go to", tap GRP-A (assignPlace re-reads after a refusal too)');
    for (const key of ['loader.update.400'].concat(REFUSALS)) {
        t = await stationsReady();
        t.net.on('POST', '/api/loader/update', answer(key));
        t.p.run('armSlotFor(20, "outbound")');
        tap(t, t.header('GRP-A'));
        await D.settle();
        L.push('-- ' + refusalTitle(key));
        flush(L, t.net, t.rec);
    }
    L.push('', '== refusals: UNL-A Windows, tap LINE-1 (appendWindow re-reads in its catch)');
    for (const key of ['loader.set-home.400', 'reject']) {
        t = await stationsReady();
        t.net.on('POST', '/api/loader/set-home', answer(key));
        t.p.run('armSlotFor(20, "windows")');
        tap(t, t.tile('LINE-1'));
        await D.settle();
        L.push('-- set-home ' + refusalTitle(key));
        flush(L, t.net, t.rec);
    }
    t = await stationsReady();
    t.net.on('POST', '/api/loader/reorder-homes', answer('json.500'));
    t.p.run('armSlotFor(20, "windows")');
    tap(t, t.tile('LINE-1'));
    await D.settle();
    L.push('-- reorder-homes ' + refusalTitle('json.500') + ' (the window was added)');
    flush(L, t.net, t.rec);
    L.push('', '== refusals: drop SPOT-9 on UNL-A\'s box (no re-read after a refusal)');
    for (const key of ['loader.set-home.400', 'reject']) {
        t = await stationsReady();
        t.net.on('POST', '/api/loader/set-home', answer(key));
        b = box(20, [3]);
        t.p.ctx.onBoxDrop.call(b, dragEvent({ 'text/plain': '9' }, b, 500));
        await D.settle();
        L.push('-- set-home ' + refusalTitle(key));
        flush(L, t.net, t.rec);
    }
    L.push('', '== refusals: pick PART-A on GRP-B-N2 (no re-read after a refusal)');
    t = await stationsReady();
    t.net.on('POST', '/api/loader/set-home', answer('json.400'));
    t.p.ctx.setMemberPayload(21, '7', 'PART-A');
    await D.settle();
    L.push('-- set-home ' + refusalTitle('json.400'));
    flush(L, t.net, t.rec);
    L.push('', '== refusals: Save parts (both posts are sent at once; one refused)');
    t = await stationsReady();
    t.net.on('POST', '/api/loader/set-payload', answer('loader.set-payload.400'));
    const btn2 = D.makeEl('button');
    t.p.ctx.savePayloads(20, pbox, btn2);
    await D.settle();
    L.push('-- set-payload ' + refusalTitle('loader.set-payload.400'));
    flush(L, t.net, t.rec);
    L.push('  Save parts button: ' + JSON.stringify(btn2.textContent) + ', ' + (btn2.disabled ? 'disabled' : 'enabled'));

    L.push('', '== the first read fails (GET /api/nodes 500)');
    t = loadersPage();
    answerStations(t.net);
    t.net.on('GET', '/api/nodes', answer('json.500'));
    t.p.run('refresh()');
    await D.settle();
    flush(L, t.net, t.rec);
    L.push('  boxes: ' + JSON.stringify(t.doc.getElementById('loader-boxes').innerHTML));

    goldens['stations-page.txt'] = L.join('\n') + '\n';
}

// ============================================================================
// Core Nodes · supermarket drag into a group or lane (census #70, C15): bodies only
// ============================================================================

function supermarketPage() {
    const rec = newRec();
    const doc = D.makeDocument();
    const net = D.makeNet();
    doc.getElementById('page-data').dataset = { authenticated: 'true' };
    const tiles = {};
    const p = D.loadPage('static/pages/nodes-supermarket.js', baseGlobals(doc, net, rec), lender(rec, doc));
    doc.qsa = (sel) => {
        const m = /^\.node-tile\[data-id="(\d+)"\]$/.exec(sel);
        return m ? (tiles[m[1]] ? [tiles[m[1]]] : []) : null;
    };
    return { p, doc, net, rec, tiles };
}

// laneSlots is a lane's (or a group's direct section's) slot container with
// its tiles laid out 100 px apart, as onDrop reads them.
function laneSlots(t, laneID, direct, tileIDs) {
    const section = D.makeEl('div');
    section.className = 'smkt-lane' + (direct ? ' ngrp-direct' : '');
    section.dataset.laneId = String(laneID);
    const slots = D.makeEl('div');
    slots.className = 'smkt-lane-slots';
    slots.closestMap = { '.smkt-lane': section };
    const tiles = tileIDs.map((id, i) => {
        const x = D.makeEl('div');
        x.dataset = { id: String(id), parentId: String(laneID) };
        x.rect = { left: i * 100, width: 100, top: 0, height: 40 };
        t.tiles[id] = x;
        return x;
    });
    slots.qsa = (sel) => (sel === '.node-tile[draggable="true"]' ? tiles : null);
    return slots;
}

async function supermarketPins() {
    const L = [
        '# Core Nodes, supermarket drag (census #70, C15; kept as layout editing, so the bodies only).',
        '# static/pages/nodes-supermarket.js onDrop / onDropGrid -> reparentNode / reorderLane, as served.',
        '# Node identity travels as tile ids; success reloads the page.',
        '',
    ];
    let t = supermarketPage();
    t.tiles[9] = Object.assign(D.makeEl('div'), { dataset: { id: '9', parentId: '' } });
    t.net.on('POST', '/api/', { json: { status: 'ok' } });
    const drops = [
        ['SPOT-9 into lane GRP-A-L1, before its first slot', () => laneSlots(t, 2, false, [3, 4]), { 'text/plain': '9' }, 10],
        ['SPOT-9 into lane GRP-A-L1, after its last slot', () => laneSlots(t, 2, false, [3, 4]), { 'text/plain': '9' }, 500],
        ['SPOT-9 into GRP-B (the group\'s direct section)', () => laneSlots(t, 5, true, [6, 7]), { 'text/plain': '9' }, 10],
        ['GRP-A-L1-S2 within its own lane, to the front', () => laneSlots(t, 2, false, [3, 4]), { 'text/plain': '4' }, 10],
        ['the GRP-B header onto lane GRP-A-L1', () => laneSlots(t, 2, false, [3, 4]), { 'application/x-node-group': 'GRP-B' }, 10],
    ];
    for (const d of drops) {
        const slots = d[1]();
        t.p.ctx.onDrop.call(slots, dragEvent(d[2], slots, d[3]));
        await D.settle();
        L.push('-- drop ' + d[0]);
        flush(L, t.net, t.rec);
    }
    const grid = D.makeEl('div');
    t.p.ctx.onDropGrid.call(grid, dragEvent({ 'text/plain': '3' }, grid));
    await D.settle();
    L.push('-- drop GRP-A-L1-S1 on the grid (out of its lane)');
    flush(L, t.net, t.rec);

    L.push('', '== refusals: SPOT-9 into lane GRP-A-L1');
    for (const key of ['nodes.reparent.409', 'nodes.reparent.400', 'json.500', 'proxy.502', 'reject']) {
        t = supermarketPage();
        t.tiles[9] = Object.assign(D.makeEl('div'), { dataset: { id: '9', parentId: '' } });
        t.net.on('POST', '/api/nodes/reparent', answer(key));
        const slots = laneSlots(t, 2, false, [3, 4]);
        t.p.ctx.onDrop.call(slots, dragEvent({ 'text/plain': '9' }, slots, 10));
        await D.settle();
        L.push('-- ' + refusalTitle(key));
        flush(L, t.net, t.rec);
    }
    goldens['supermarket-page.txt'] = L.join('\n') + '\n';
}

// ============================================================================
// Core RDS explorer · block builder Location (census #71, C16): the body only
// ============================================================================

async function rdsPins() {
    const L = [
        '# Core RDS explorer, block builder (census #71, C16; kept: the vendor console takes a raw RDS',
        '# location string, typed). static/pages/rds-explorer.js bbAddBlock -> bbGenerate -> sendRequest.',
        '',
    ];
    const rec = newRec();
    const doc = D.makeDocument();
    const net = D.makeNet();
    const p = D.loadPage('static/pages/rds-explorer.js', baseGlobals(doc, net, rec), lender(rec, doc));
    const val = (id, v) => { doc.getElementById(id).value = v; };
    val('bb-type', 'bin'); val('bb-blockId', ''); val('bb-binTask', 'Load'); val('bb-goodsId', '');
    val('bb-location', '');
    p.run('bbAddBlock()');
    L.push('-- Add a block with no location');
    flush(L, net, rec);
    val('bb-location', '  GRP-A-L1-S1 ');
    p.run('bbAddBlock()');
    val('bb-type', 'operation'); val('bb-blockId', 'b9'); val('bb-operation', 'JackLoad'); val('bb-opArgs', '');
    val('bb-location', 'LM<b>7</b>');
    p.run('bbAddBlock()');
    L.push('-- two blocks added: the rows drawn (bb-blocks)');
    (doc.getElementById('bb-blocks').innerHTML.match(/<span class="bb-detail">[\s\S]*?<\/span>/g) || []).forEach((r) => L.push('  ' + r));
    val('bb-orderId', 'T-1'); val('bb-complete', 'true'); val('bb-priority', '0'); val('bb-vehicle', ''); val('bb-group', '');
    p.run('bbGenerate()');
    L.push('-- Generate JSON (req-body):');
    doc.getElementById('req-body').value.split('\n').forEach((l) => L.push('  ' + l));
    val('req-method', 'POST'); val('req-path', '/setOrder');
    net.on('POST', '/api/fleet/proxy', { json: { status_code: 200, elapsed_ms: 3, body: { ok: true } } });
    p.run('sendRequest()');
    await D.settle();
    L.push('-- Send');
    flush(L, net, rec);
    goldens['rds-explorer-page.txt'] = L.join('\n') + '\n';
}

// ============================================================================
// Core Test orders · the submits (census #58-#62, C7 C8): what the served selects send
// ============================================================================

function testOrdersPage() {
    const rec = newRec();
    const doc = D.makeDocument();
    const net = D.makeNet();
    doc.getElementById('page-data').dataset = { authenticated: 'true' };
    const p = D.loadPage('static/pages/test-orders.js', baseGlobals(doc, net, rec), lender(rec, doc));
    return { p, doc, net, rec };
}

// shownToasts is what test-orders.js's own showToast appended to the body.
function shownToasts(L, t) {
    t.doc.body.children.splice(0).forEach((c) => L.push('  showToast ' + c.className + ': ' + D.normal(c.textContent)));
}

async function testOrdersPins() {
    const L = [
        '# Core Test orders, the submits (census #58-#62; the selects are test-orders-template.txt).',
        '# static/pages/test-orders.js as served: its writes go through the HTTP door (send), so any',
        '# refusal is one sentence, a body that is not JSON included.',
        '',
    ];
    // The cycle modes as the template offers them.
    const modes = D.parseOptions(D.selectIn(D.readWWW('templates/test-orders.html'), 'kc-cycle-mode').inner).map((o) => o.value);
    let t = testOrdersPage();
    const val = (id, v) => { t.doc.getElementById(id).value = v; };
    t.net.on('POST', '/api/test-orders/submit', { json: { order_uuid: 'u-1' } })
        .on('POST', '/api/test-orders/submit/complex', { json: { orders: [{ role: 'swap', order_uuid: 'u-2' }] } })
        .on('POST', '/api/test-orders/direct', { json: { vendor_order_id: 'v-1' } })
        .on('POST', '/api/test-orders/direct/complex', { json: { orders: [{ role: 'swap', order_uuid: 'u-3' }] } });
    const kafka = () => {
        val('k-order-type', 'move'); val('k-source-node', 'GRP-A'); val('k-delivery-node', 'LINE-1');
        val('k-payload', 'PART-A'); val('k-quantity', '1'); val('k-priority', '0');
    };
    const complex = (prefix) => {
        val(prefix + '-cycle-mode', modes[1]); val(prefix + '-location', 'GRP-A-L1-S1'); val(prefix + '-payload', 'PART-A');
        val(prefix + '-full-source', ''); val(prefix + '-staging1', 'SPOT-9'); val(prefix + '-staging2', '');
        val(prefix + '-outgoing', 'GRP-B'); val(prefix + '-priority', '0');
    };
    const cases = [
        ['Kafka order: move GRP-A -> LINE-1, PART-A', () => { kafka(); t.p.run('submitKafkaOrder()'); }],
        ['Kafka order, no payload', () => { kafka(); val('k-payload', ''); t.p.run('submitKafkaOrder()'); }],
        ['Kafka complex, ' + modes[1] + ' at GRP-A-L1-S1, staging SPOT-9, out GRP-B, source "Core Decides"',
            () => { complex('kc'); t.p.run('submitKafkaComplexOrder()'); }],
        ['Direct order: from GRP-B-N1 (id 6) to LINE-1 (id 8)', () => { val('d-from-node', '6'); val('d-to-node', '8'); val('d-priority', '0'); t.p.run('submitDirectOrder()'); }],
        ['Direct order, no "to"', () => { val('d-from-node', '6'); val('d-to-node', ''); t.p.run('submitDirectOrder()'); }],
        ['Direct complex, the same five as the Kafka one', () => { complex('cx'); t.p.run('submitComplexOrder()'); }],
    ];
    L.push('== the body each pick sends (an answer of 200)');
    for (const c of cases) {
        c[1]();
        await D.settle();
        L.push('-- ' + c[0]);
        flush(L, t.net, t.rec);
        shownToasts(L, t);
    }

    L.push('', '== refusals: Kafka order');
    for (const key of ['test-orders.submit.400'].concat(REFUSALS)) {
        t.net.on('POST', '/api/test-orders/submit', answer(key));
        kafka();
        t.p.run('submitKafkaOrder()');
        await D.settle();
        L.push('-- ' + refusalTitle(key));
        flush(L, t.net, t.rec);
    }
    L.push('', '== refusals: Kafka complex');
    for (const key of ['test-orders.complex.400', 'proxy.502', 'reject']) {
        t.net.on('POST', '/api/test-orders/submit/complex', answer(key));
        complex('kc');
        t.p.run('submitKafkaComplexOrder()');
        await D.settle();
        L.push('-- ' + refusalTitle(key));
        flush(L, t.net, t.rec);
        shownToasts(L, t);
    }
    L.push('', '== refusals: Direct order');
    for (const key of ['json.409', 'reject']) {
        t.net.on('POST', '/api/test-orders/direct', answer(key));
        val('d-from-node', '6'); val('d-to-node', '8');
        t.p.run('submitDirectOrder()');
        await D.settle();
        L.push('-- ' + refusalTitle(key));
        flush(L, t.net, t.rec);
    }
    goldens['test-orders-page.txt'] = L.join('\n') + '\n';
}

// ============================================================================

async function main() {
    await ordersPins();
    await binsPins();
    await kanbanPins();
    await loadersPins();
    await supermarketPins();
    await rdsPins();
    await testOrdersPins();
    process.stdout.write(JSON.stringify({ goldens, twinJS }));
}

main().catch((e) => {
    process.stderr.write('core picker pins threw: ' + (e && e.stack || e) + '\n');
    process.exit(1);
});
