// pins.js — the CATID health items on the Processes page: the requests one
// refresh of the list sends with N processes that have a red or amber item,
// the one count each row draws from them, what the 10 s refresh, window focus
// and a process's own tabs read, and that the page opens no stream (the
// "catid-health" SSE hint is gone). Brief S4's "pin first" for the strip, moved
// with S4's change: the list shows no strip, one count per row, one request.
//
// Run by www/catid_health_page_pins_test.go (TestCatidHealthPagePins), which
// compares what this prints against www/testdata/catid-health-page/*. It
// prints ONE JSON object, golden file name -> text, and nothing else.
//
// THE ANSWERS ARE THE ROUTES' OWN. Every /api/catid-health and
// /api/processes/{id}/catid-health answer is read from the bodies
// TestCatidHealthBodiesPins serves through the real handlers over a real
// engine (www/testdata/catid-health/*.txt), so a change to what the routes
// serve moves these goldens too. Regenerate that pin first.
//
// THE PAGE IS U0's HARNESS, unmodified (../process-setup-harness/
// page-harness.js): the page module byte for byte, its classic scripts off
// processes.html's own tags, fetch recorded. Two things that harness leaves
// out are added here at the one seam it has, the realm it builds
// (vm.createContext), without touching it:
//   - setInterval is recorded, not dropped, so a pin can run the page's 10 s
//     refresh by hand (the callback boot() registers) and count what it reads;
//   - EventSource is a recorder, so a pin can say whether the page opens a
//     stream at all, and what it listens to.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const H = require('../process-setup-harness/page-harness.js');

const BODIES = path.join(__dirname, '..', 'catid-health');
const out = {};

// ── the routes' answers ──────────────────────────────────────────────────────

// A golden of TestCatidHealthBodiesPins: '# what', 'GET url', 'status type',
// then the body.
function served(name) {
    const lines = fs.readFileSync(path.join(BODIES, name), 'utf8').split('\n');
    const req = /^GET (\S+)$/.exec(lines[1]);
    const st = /^(\d+) /.exec(lines[2]);
    if (!req || !st) throw new Error(name + ': not a body golden (the format of testdata/catid-health drifted)');
    return { url: req[1], status: Number(st[1]), body: JSON.parse(lines.slice(3).join('\n')) };
}

const ITEMS = {};
for (const f of fs.readdirSync(BODIES).filter(f => /^items-cell-.*\.txt$/.test(f))) {
    const b = served(f);
    ITEMS[b.url] = b;
}
const SUMMARY = { 0: served('summary-n0.txt'), 1: served('summary-n1.txt'), 3: served('summary-n3.txt') };
const TWIN = { summary: served('item5-summary.txt'), items: served('item5-shared-twin.txt') };

// The processes each stage's store holds, as the page's data block carries
// them (the list's rows are drawn from it, not from the health routes).
const NAMES = { 1: 'CELL-QUIET', 2: 'CELL-HELD', 3: 'CELL-SHARED', 4: 'CELL-IDENT', 5: 'CELL-PAIR' };
const STAGE = { 0: [1, 2], 1: [1, 2, 3], 3: [1, 2, 3, 4, 5] };
const pageData = ids => ({
    processes: ids.map(id => ({ id: id, name: NAMES[id], group_id: 0 })),
    processGroups: [], styles: [], stations: [], stationNodes: {}, loaderGaps: [],
});

const EMPTY_COMPOSER = { styles: [], routing: [], presets: [], cell: { positions: [], groups: {} } };

// server answers the two health routes from their goldens (refuse: urls
// answered 500 with writeError's shape instead), a process's composer read
// empty, and nothing else (a 404 the golden records like any request).
function server(summary, items, refuse) {
    return (method, url) => {
        const u = url.split('?')[0];
        if (refuse && refuse[u]) return { status: 500, body: { error: refuse[u] } };
        if (method === 'GET' && u === '/api/catid-health') return { status: summary.status, body: summary.body };
        if (method === 'GET' && items[u]) return { status: items[u].status, body: items[u].body };
        if (method === 'GET' && /^\/api\/processes\/\d+\/composer$/.test(u)) return { body: EMPTY_COMPOSER };
        return undefined;
    };
}

// ── the page, with its intervals and its streams recorded ───────────────────

// open boots the page through U0's harness, the realm given a recording
// setInterval and EventSource first (see the header).
async function open(opts) {
    const rec = { intervals: [], streams: [] };
    const make = vm.createContext;
    vm.createContext = function (ctx) {
        // The page's realm is the one carrying the harness's clock stubs; the
        // flow picture's own realm is left as it is.
        if (ctx && typeof ctx.formatTime === 'function') {
            ctx.setInterval = (fn, ms) => { rec.intervals.push({ fn: fn, ms: ms }); return rec.intervals.length; };
            ctx.EventSource = function (url) {
                const s = { url: String(url), listens: [] };
                rec.streams.push(s);
                this.addEventListener = type => s.listens.push(type);
                this.close = () => {};
            };
        }
        return make.apply(this, arguments);
    };
    let page;
    try {
        page = await H.open(opts);
    } finally {
        vm.createContext = make;
    }
    page.rec = rec;
    return page;
}

// tick runs the page's 10 s refresh: the one 10000 ms interval boot() sets.
async function tick(page) {
    const ten = page.rec.intervals.filter(i => i.ms === 10000);
    if (ten.length !== 1) throw new Error('the page sets ' + ten.length + ' 10 s intervals; the refresh this pins has moved');
    ten[0].fn();
    await page.settle();
}

function callLines(calls) {
    if (!calls.length) return ['  (none)'];
    return calls.map(c => '  ' + c.method + ' ' + c.url);
}

function reqs(label, calls) {
    return [label + ': ' + calls.length + ' request' + (calls.length === 1 ? '' : 's')].concat(callLines(calls));
}

// The list's health counts: each process row's count box (#pd-h-<id>), as
// drawn; and whether a strip is drawn anywhere (S4: none).
function stripLines(page, ids, names) {
    const out = ['  #pd-catid on the page: ' + (page.byId('pd-catid') ? 'yes' : 'no')];
    for (const id of ids) {
        const box = page.byId('pd-h-' + id);
        out.push('  ' + (names || NAMES)[id] + ': ' + (!box ? '(no count box)' : box.innerHTML || '(no count)'));
    }
    return out;
}

function splitMarkup(html) {
    return html.replace(/(<div |<span class="pd-ch-acts">|<button |<select )/g, '\n$1').split('\n')
        .filter(Boolean).map(l => '  ' + l);
}

function errors(page) {
    const e = page.doc.body.dataset.pdError;
    return e ? ['  page error: ' + e] : [];
}

// ── the list: requests per refresh with N processes shown ───────────────────

async function listPins() {
    const lines = ['# The Processes list (no ?process=), its health counts fed the summary the route serves',
        '# (testdata/catid-health/). "shown" = summary rows with a red or amber item: the page reads the summary',
        '# alone, every boot of the list and every 10 s tick, and no process\'s items (S4: one request).'];
    const strip = ['# What each list row draws from that answer (#pd-h-<id>): one count, red + amber, toned by the worst;',
        '# no strip.'];
    const perTick = [];
    for (const n of [0, 1, 3]) {
        const summary = SUMMARY[n];
        const shown = summary.body.filter(r => r.worst === 'red' || r.worst === 'amber').length;
        const page = await open({ page: pageData(STAGE[n]), serve: server(summary, ITEMS) });
        const boot = page.since(0);
        let at = page.mark();
        await tick(page);
        const ticked = page.since(at);
        at = page.mark();
        page.fireWindow('focus');
        await page.settle();
        const focus = page.since(at);
        at = page.mark();
        for (const fn of (page.doc.listeners.visibilitychange || []).slice()) fn({ type: 'visibilitychange' });
        await page.settle();
        const vis = page.since(at);
        lines.push('', '## N = ' + n + ' shown (summary-n' + n + '.txt: ' + summary.body.length + ' summary row' +
            (summary.body.length === 1 ? '' : 's') + ', ' + shown + ' shown)');
        lines.push(...reqs('boot (drawList -> refreshCatidHealth)', boot));
        lines.push(...reqs('10 s tick (refreshRail, then refreshCatidHealth: no process open)', ticked));
        lines.push(...reqs('window focus (refreshRail: no process open)', focus));
        lines.push(...reqs('visibilitychange to visible (refreshRail)', vis));
        lines.push(...errors(page));
        perTick.push('N = ' + n + ': ' + ticked.length + ' (1 summary + ' + (ticked.length - 1) + ' items reads)');
        strip.push('', '## N = ' + n + ' shown', ...stripLines(page, STAGE[n]));
    }
    lines.push('', '## requests per 10 s tick of the list, by N shown', ...perTick.map(l => '  ' + l));

    // The summary read refused: the counts are left as they were (advisory).
    {
        // refuse is read on every call, so filling it mid-run refuses from
        // the next request on.
        const refuse = {};
        const page = await open({ page: pageData(STAGE[3]), serve: server(SUMMARY[3], ITEMS, refuse) });
        const counts = () => STAGE[3].map(id => (page.byId('pd-h-' + id) || {}).innerHTML).join('|');
        const before = counts();
        const at = page.mark();
        refuse['/api/catid-health'] = 'boom';
        await tick(page);
        lines.push('', '## N = 3 shown, then a tick whose summary read answers 500');
        lines.push(...reqs('the tick', page.since(at)), ...errors(page));
        lines.push('  counts after: ' + (counts() === before ? 'unchanged (advisory)' : 'CHANGED'));
    }
    // Item 5: two styles whose names differ by a trailing space.
    {
        const items = {};
        items[TWIN.items.url] = TWIN.items;
        const page = await open({ page: { processes: [{ id: 1, name: 'CELL-TWIN', group_id: 0 }] }, serve: server(TWIN.summary, items) });
        strip.push('', '## item 5: CELL-TWIN, CATID 6001 in "TWIN" and "TWIN " (item5-*.txt): the list, then its Settings',
            ...stripLines(page, [1], { 1: 'CELL-TWIN' }));
        const pd = { processes: [{ id: 1, name: 'CELL-TWIN', group_id: 0 }], activeProcessId: 1 };
        const open1 = await open({ page: pd, search: '?process=1', serve: server(TWIN.summary, items) });
        open1.click('root', { tab: 'settings' });
        await open1.settle();
        strip.push('  Settings, text as drawn: ' + open1.segments().filter(s => /6001/.test(s)).join(' | '));
    }
    out['requests-list.txt'] = lines.join('\n') + '\n';
    out['strip.txt'] = strip.join('\n') + '\n';
}

// ── a process open: what its tabs read of the health routes ─────────────────

async function processPins() {
    const lines = ['# A process open (?process=4, CELL-IDENT): its health items are read on the Settings tab only;',
        '# the 10 s tick never reads them, and the list\'s summary is not read while a process is open.',
        '# Requests other than the health routes and the composer read answer 404 (no fixture): listed, not',
        '# pinned for their own sake.'];
    const pd = pageData(STAGE[3]);
    pd.activeProcessId = 4;
    const page = await open({ page: pd, search: '?process=4', serve: server(SUMMARY[3], ITEMS) });
    const health = c => /catid-health/.test(c.url);
    lines.push('', ...reqs('boot on Flows (health reads only)', page.since(0).filter(health)));
    let at = page.mark();
    await tick(page);
    lines.push(...reqs('10 s tick on Flows (all)', page.since(at)));
    at = page.mark();
    page.click('root', { tab: 'settings' });
    await page.settle();
    lines.push(...reqs('the Settings tab opened (health reads only)', page.since(at).filter(health)));
    at = page.mark();
    await tick(page);
    lines.push(...reqs('10 s tick on Settings (all)', page.since(at)));
    at = page.mark();
    await page.pd('refreshCatidHealth')();
    await page.settle();
    lines.push(...reqs('refreshCatidHealth on Settings (what a verb run there calls after; health reads only)',
        page.since(at).filter(health)));
    lines.push('  #pd-catid on the page: ' + (page.byId('pd-catid') ? 'yes' : 'no'));
    // The health section as the Settings tab draws it (healthSection, the
    // page's own function): every item, red and amber beside the info ones,
    // worst first, with their verbs (S4 item 2, one renderer).
    const healthHTML = String(page.pd('healthSection')() || '');
    lines.push('', '## the Settings tab\'s Health (healthSection: every item, worst first, with its verbs)',
        ...(healthHTML ? splitMarkup(healthHTML) : ['  (none)']));
    lines.push(...errors(page));
    out['requests-process.txt'] = lines.join('\n') + '\n';
}

// ── the SSE hint ────────────────────────────────────────────────────────────

async function ssePins() {
    const lines = ['# The "catid-health" SSE hint is gone (S4: the engine sends no such frame; the owner: "we are',
        '# good with 10s of delay"). The Processes page opens no /events stream of its own (it imports no',
        '# createSSE or onSSE): the counts move only on the 10 s tick, a boot of the list, or after a verb',
        '# runs.'];
    for (const [label, opts] of [
        ['the list (N = 3 shown)', { page: pageData(STAGE[3]), serve: server(SUMMARY[3], ITEMS) }],
        ['a process open (?process=4)', Object.assign({ search: '?process=4', serve: server(SUMMARY[3], ITEMS) },
            { page: Object.assign(pageData(STAGE[3]), { activeProcessId: 4 }) })],
    ]) {
        const page = await open(opts);
        lines.push('', '## ' + label);
        lines.push('  EventSource opened: ' + page.rec.streams.length +
            (page.rec.streams.length ? ' (' + page.rec.streams.map(s => s.url + ' listening ' + s.listens.join(',')).join('; ') + ')' : ''));
        const listening = page.rec.streams.some(s => s.listens.indexOf('catid-health') >= 0);
        lines.push('  a "catid-health" frame reaches a listener: ' + (listening ? 'yes' : 'no'));
        // With no stream there is no frame to dispatch: what the page sends
        // after the hint is what it sends with nothing at all, after boot.
        const at = page.mark();
        await page.settle();
        lines.push(...reqs('  requests after a frame (no stream: none can arrive)', page.since(at)));
        lines.push('  intervals set: ' + page.rec.intervals.map(i => i.ms + ' ms').join(', '));
    }
    out['sse-hint.txt'] = lines.join('\n') + '\n';
}

(async () => {
    await listPins();
    await processPins();
    await ssePins();
    process.stdout.write(JSON.stringify(out));
})().catch(e => {
    process.stderr.write(String((e && e.stack) || e) + '\n');
    process.exit(1);
});
