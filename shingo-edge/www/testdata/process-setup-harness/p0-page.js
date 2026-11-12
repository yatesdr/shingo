// p0-page.js — the Processes page's P0 pins, beside U0-page's (pins.js runs
// both and prints one JSON object; processes_desktop_setup_pins_test.go
// compares it against testdata/process-setup-page/*).
//
// EACH FAMILY IS WHAT THE PAGE DOES TODAY, bugs included (a bug is named in a
// comment where the pin meets it, and stays pinned as is):
//
//   requests-*.txt (new ones): the acts U0 left out — the health strip's
//     verbs, the style row menu, station counters, Add/Remove positions, the
//     loader-board bind, catalog sync, Delete process, Add group, the PLC
//     suggestions, the Recipes tab's rename/archive/apply, and the reads the
//     tab clicks and the rail's re-read send. Same format as U0's.
//
//   refusals-*.txt: every write of every act pinned (U0's and these), run
//     again with that one write answered the way the Edge refuses: the
//     handler's own 4xx shape (writeError's {"error"}, and flow's 409/422
//     shapes where the handler has them, handlers_flow.go), a 500 with no
//     body (chi's Recoverer on a panic, router.go:195), no answer at all
//     (fetch rejects, a dead Edge), and — for the admin routes only — the
//     login page an expired session's 303 lands on (adminMiddleware,
//     router.go:820). Each says what followed, what text appeared and where,
//     whether the sheet is open and its OK pressable, and whether the page
//     let a rejection go unhandled.
//
//   sheet-stack.txt: the page's sheet stack — openSheet, runSheet's one run
//     per press, what closeSheet clears, the Escape rule innermost first, the
//     discard question, the Advanced sheet — and the facts about scroll lock
//     and focus.
//
// THE PLANT IS U0's (fixture.json); the routes an act needs that the base
// plant does not answer (a health strip, a station counter) are added per act
// here, synthetic like the rest.

'use strict';

const fs = require('fs');
const path = require('path');

let T = null;               // pins.js's helpers: see run()
const MSG = 'the Edge refused it';
const PAGE = path.join(__dirname, '..', '..', 'static', 'js', 'pages', 'processes-desktop.js');

// ── unhandled rejections, per run ────────────────────────────────────────────
//
// A page act whose fetch rejects with no catch is an unhandled rejection: in a
// browser it reaches the page's own window listener (processes-desktop.js:43,
// which writes body[data-pd-error]) and the console, and the engineer sees
// nothing. Here it reaches node's, which hands it to the page that is running
// and records it for the run's outcome.
//
// THE WORDS ARE THE PIN'S, NOT V8's: a SyntaxError's message (JSON.parse of
// the login page) is the engine's sentence and moves with node's version, so
// a rejection is its name, and its message only when this harness wrote it
// (fetch's "Failed to fetch").
const reasonOf = r => (!r || !r.name ? String(r)
    : r.name === 'SyntaxError' ? 'SyntaxError (JSON.parse of a body that is not JSON)' : r.name + ': ' + r.message);
let running = null;
process.on('unhandledRejection', reason => {
    if (!running) { process.stderr.write('p0 pins: an unhandled rejection with no page running: ' + reason + '\n'); process.exit(1); }
    running.rejections.push({ text: reasonOf(reason), message: String((reason && reason.message) || reason) });
    running.page.fireWindow('unhandledrejection', { reason: reason });
});
function rejectionLine(r, page) {
    if (!r.rejections.length) return 'unhandled rejection: none';
    const pd = page.doc.body.dataset.pdError || '';
    const last = r.rejections[r.rejections.length - 1];
    return 'unhandled rejection: ' + r.rejections.map(x => x.text).join('; ') + ' (body data-pd-error ' +
        (pd === last.message.slice(0, 300) ? 'set to its message' : JSON.stringify(pd)) + ')';
}

// ── small helpers ────────────────────────────────────────────────────────────
const lineOf = c => c.method + ' ' + c.url;
const isPreview = c => /\/flow\/preview$/.test(c.url.split('?')[0]);
// The elements a page message is written into: a notice, a refusal, a
// warning, a sheet's status line or header, the Flows bar, a dim note.
const MESSAGE = /pd-notice|pd-refusal|pd-warn|\.bad\b|\bh2\b|\.st \(textContent\)|#pd-bar|pd-note|pd-dim\b/;

// A button the page drew into `hostID`, found by its data-act (and, when
// given, more of its dataset), as the dataset a click carries.
function drawn(page, hostID, act, more) {
    const host = page.byId(hostID);
    if (!host) throw new Error('#' + hostID + ' is not drawn');
    const b = host.querySelectorAll('[data-act]').find(x => x.dataset.act === act &&
        Object.keys(more || {}).every(k => x.dataset[k] === String(more[k])));
    if (!b) throw new Error('#' + hostID + ' draws no ' + act + ' ' + JSON.stringify(more || {}) + ': ' + host.innerHTML.slice(0, 400));
    return Object.assign({}, b.dataset);
}
// The same, in the page's own markup (#pd-root, whatever tab it drew).
function rootButton(page, act, more) {
    const b = page.root.querySelectorAll('[data-act]').find(x => x.dataset.act === act &&
        Object.keys(more || {}).every(k => x.dataset[k] === String(more[k])));
    if (!b) throw new Error('#pd-root draws no ' + act + ' ' + JSON.stringify(more || {}));
    return Object.assign({}, b.dataset);
}
function sheetTitle(page) {
    const m = /<h2>([\s\S]*?)<\/h2>/.exec(page.scrim.innerHTML);
    return m ? T.decode(m[1].replace(/<[^>]*>/g, '')) : '';
}
// A sheet's OK: the layer's (data-layer-ok) on its own shell, the page's
// sheet-ok on a sheet that draws its footer itself (the apply modal).
function okButton(page) { return page.scrim.querySelector('[data-act="sheet-ok"]') || page.scrim.querySelector('[data-layer-ok]'); }
// OK, as an engineer presses it: never when it is drawn disabled (a browser
// sends no click to a disabled button).
async function pressOK(page) {
    const ok = okButton(page);
    if (!ok) throw new Error('no sheet-ok to press: ' + page.scrim.innerHTML.slice(0, 300));
    if (ok.disabled) return false;
    page.click('scrim', { act: 'sheet-ok' });
    await page.settle();
    return true;
}
function sheetLine(page) {
    if (!page.sheetOpen()) return 'sheet: closed';
    const st = page.scrim.querySelector('[data-layer-status]') || page.scrim.querySelector('.mf .st');
    const ok = okButton(page);
    const label = ok ? (ok.textContent || T.text(ok.innerHTML)) : '';
    return 'sheet: open ' + JSON.stringify(sheetTitle(page)) +
        (st && st.textContent ? ', status ' + JSON.stringify(st.textContent) + (/\bbad\b/.test(st.className || '') ? ' (bad)' : '') : ', no status') +
        (ok ? ', OK ' + JSON.stringify(label) + (ok.disabled ? ' disabled' : ' enabled') : ', no OK');
}
// The page's own message state, the fields that are set.
function stateLine(page) {
    const S = page.S;
    const f = [
        ['notice', S.notice], ['settingsError', S.settingsError], ['settingsNotice', S.settingsNotice],
        ['routingError', S.routingError], ['routingWarning', S.routingWarning],
        ['presets.error', S.presets && S.presets.error], ['shapeDraft.refusal', S.shapeDraft && S.shapeDraft.refusal],
        ['partPop.note', S.partPop && S.partPop.note], ['papply.status', S.papply && S.papply.status],
    ].filter(x => x[1]);
    return f.length ? 'state: ' + f.map(x => x[0] + '=' + JSON.stringify(x[1])).join(', ') : 'state: no message set';
}

// What the HTTP door put on the notice (shared/http.js: a failure is shown
// once, there): its level, its words and its one action.
function noticeLine(n) {
    return 'notice: ' + n.level + ' ' + JSON.stringify(n.text) + (n.sticky ? ' (sticky)' : '') + (n.action ? ' · ' + n.action : '') +
        (n.times > 1 ? ' (shown ' + n.times + ' times, one on screen)' : '');
}
// noticeLines is the notices as notice.js shows them: the same words at the
// same level are one toast, its lifetime restarted (notice.js, "ONE OF EACH").
function noticeLines(list) {
    const out = [];
    for (const n of list) {
        const same = out.find(o => o.text === n.text && o.level === n.level && o.action === n.action);
        if (same) same.times++; else out.push(Object.assign({}, n, { times: 1 }));
    }
    return out.map(noticeLine);
}

// ── refusal answers ──────────────────────────────────────────────────────
//
// The answers a write is refused with. The 4xx shapes are the handler's own:
// writeError's {"error": msg} (handlers_api.go:28) for every write on this
// page, plus, for the flow routes, the shapes handlers_flow.go writes beside
// it — 409 from writeFlowRefusal (:84-91, with stale:true for a stale
// fingerprint), 422 with the validation findings (:219-223, :307-311), 422
// from writeMintRefusal (:263-275), and the preview's 400 that carries the
// findings and fingerprint (:133-146). writeAdvisoryRefusal (notice:true)
// and writeErrorWithExit (exit) are station-action shapes
// (handlers_operator_actions.go) that no route this page writes answers.
const FINDING = { core_node_name: 'POS_A1', side: 'to', field: 'inbound_source', severity: 'error', message: 'POS_A1 needs a source' };
// THE PUBLIC ROUTES this page calls: router.go:290-413 sit outside the admin
// group, so no session answers them. Every other route it calls, read or
// write, is behind adminMiddleware (router.go:414-415), which answers a
// caller with no session by a 303 to /login (:820): fetch follows it and the
// page gets the login page, 200 text/html. A lapsed session lapses for every
// later call too, so that answer is STICKY: the refused write and every admin
// call after it get the login page.
const PUBLIC = [
    /^\/api\/processes\/\d+\/flow\/(preview|save|mint)$/, /^\/api\/processes\/\d+\/changeover\/(start|gate-status)$/,
    /^\/api\/processes\/\d+\/catid-health$/, /^\/api\/catid-health$/, /^\/api\/core-nodes$/, /^\/api\/payload-catalog$/,
];
const isPublic = url => PUBLIC.some(re => re.test(url.split('?')[0]));
function answersFor(method, url) {
    const u = url.split('?')[0];
    let four;
    if (/\/flow\/preview$/.test(u)) {
        four = [
            ['409 {"error"} (writeFlowRefusal)', { status: 409, body: { error: MSG } }],
            ['400 {"error", order_count:0, findings, fingerprint} (no orders)', { status: 400, body: { error: MSG, order_count: 0, findings: [FINDING], unresolved: [], fingerprint: 'fp-refused' } }],
        ];
    } else if (/\/flow\/save$/.test(u)) {
        four = [
            ['409 {"error"} (writeFlowRefusal)', { status: 409, body: { error: MSG } }],
            ['409 {"error", stale:true} (a stale fingerprint)', { status: 409, body: { error: MSG, stale: true } }],
            ['422 {"error", findings} (validation)', { status: 422, body: { error: MSG, findings: [FINDING] } }],
        ];
    } else if (/\/flow\/mint$/.test(u)) {
        four = [
            ['422 {"error"} (writeMintRefusal)', { status: 422, body: { error: MSG } }],
            ['422 {"error", findings} (validation)', { status: 422, body: { error: MSG, findings: [FINDING] } }],
        ];
    } else {
        four = [['400 {"error"} (writeError)', { status: 400, body: { error: MSG } }]];
    }
    const all = four.concat([
        ['500, no body (chi Recoverer)', { status: 500, raw: '' }],
        ['no answer (fetch rejects: TypeError "Failed to fetch")', { reject: 'Failed to fetch' }],
    ]);
    if (!isPublic(u)) {
        all.push(['session lapsed (303 to /login, 200 text/html, for it and every admin call after)',
            { status: 200, raw: '<!doctype html><title>Log in</title>', redirected: true, sticky: true }]);
    }
    return all;
}

// ── the refusal runner ───────────────────────────────────────────────────
//
// A spec is an act: {title, drive, setup?, hash?, page?, search?, extra?,
// previews?, again?}. Its writes are the non-GET calls the act sends against
// the fixture's answers, less the previews (a preview is the act only where
// `previews` says so: the flow edit, the apply modal). Each write is then
// refused, one at a time and with each answer in turn, on a fresh page: the
// k-th call with that method and url after setup is the one refused, which is
// the same request, because every call before it is the same.
async function runOnce(spec, target, answer) {
    let page = null, armed = false, k = 0, hit = null;
    page = await T.openPage({
        hash: spec.hash, page: spec.page, search: spec.search, extra: spec.extra,
        answer: (m, u) => {
            if (hit && answer.sticky && !isPublic(u)) return answer;
            if (!armed || !target || m !== target.method || u !== target.url) return undefined;
            if (k++ !== target.k) return undefined;
            hit = { at: page.calls.length - 1, before: new Set(page.segments()), notices: page.notices.length };
            return answer;
        },
    });
    running = { page: page, rejections: [] };
    if (spec.setup) await spec.setup(page);
    await page.settle();
    const from = page.mark();
    armed = true;
    // A refused write can leave the act nothing to do next (no sheet to
    // press): the driver stops there, and the run says where.
    let stopped = "";
    try { await spec.drive(page); } catch (e) { if (!target) throw e; stopped = String(e.message).split(":")[0]; }
    await page.settle();
    const r = { page: page, from: from, hit: hit, rejections: running.rejections, stopped: stopped };
    return r;
}
function writesOf(spec, calls) {
    const seenK = {};
    const out = [];
    for (const c of calls) {
        if (c.method === 'GET') continue;
        if (isPreview(c) && !spec.previews) continue;
        const key = lineOf(c);
        const k = seenK[key] = (seenK[key] === undefined ? 0 : seenK[key] + 1);
        out.push({ method: c.method, url: c.url, k: k });
    }
    return out;
}
async function refusalBlock(spec) {
    T.setCurrent(spec.golden);
    const clean = await runOnce(spec, null, null);
    // spec.refuse, when given, picks which of the act's writes are refused
    // (an act split in two: the sheet a read opens, then its OK).
    const writes = writesOf(spec, clean.page.since(clean.from)).filter(w => !spec.refuse || spec.refuse.test(w.method + ' ' + w.url));
    const lines = ['## ' + spec.title];
    if (!writes.length) { lines.push('# (this act sends no write)'); return lines; }
    lines.push('# writes, in order: ' + writes.map(w => w.method + ' ' + w.url + (w.k ? ' (#' + (w.k + 1) + ')' : '')).join(' · '));
    for (const w of writes) {
        for (const [label, answer] of answersFor(w.method, w.url)) {
            const r = await runOnce(spec, w, answer);
            if (!r.hit) throw new Error(spec.title + ': ' + w.method + ' ' + w.url + ' was not sent on the refused run');
            const page = r.page;
            const after = page.calls.slice(r.hit.at + 1);
            lines.push('### ' + w.method + ' ' + w.url + (w.k ? ' (#' + (w.k + 1) + ')' : '') + '  ←  ' + label);
            lines.push('    then: ' + (after.length ? after.map(lineOf).join(' · ') : 'nothing'));
            const fresh = page.segments().filter(s => !r.hit.before.has(s));
            if (!fresh.length) lines.push('    new text: none');
            // A TAB THAT REDREW is dozens of runs of text, most of them the same
            // words in a new place; past eight, only the runs in a message
            // element are listed and the rest are counted.
            const shown = fresh.length > 8 ? fresh.filter(s => MESSAGE.test(s)) : fresh;
            for (const s of shown) lines.push("    new text: " + s);
            if (shown.length < fresh.length) lines.push("    new text: … and " + (fresh.length - shown.length) + " more runs (the tab redrew)");
            for (const l of noticeLines(page.notices.slice(r.hit.notices))) lines.push('    ' + l);
            if (r.stopped) lines.push('    the act stopped: ' + r.stopped);
            lines.push('    ' + sheetLine(page));
            lines.push('    ' + stateLine(page));
            lines.push('    ' + rejectionLine(r, page));
            // PRESSED AGAIN: a sheet that stays open after a refusal, its OK
            // live, is pressed once more, answered as the fixture answers (a
            // lapsed session stays lapsed) — what the second press re-sends
            // is the act's retry.
            if (spec.again && page.sheetOpen() && okButton(page) && !okButton(page).disabled) {
                const mark = page.mark();
                await pressOK(page);
                const again = page.since(mark);
                lines.push('    pressed again: ' + (again.length ? again.map(lineOf).join(' · ') : 'nothing sent') +
                    ' → ' + sheetLine(page));
            }
            for (const c of page.calls.slice(r.from)) T.SEEN.push({ site: c.site, stack: c.stack, golden: spec.golden });
            running = null;
        }
    }
    return lines;
}
async function refusals(golden, specs) {
    const lines = ['# Each write of each act, refused one at a time on a fresh page. A block: the write and the',
        '# answer; the requests that followed it; each run of text that appeared on the page (container,',
        '# innermost element); the sheet; the page\'s message state; any rejection the page left unhandled.', ''];
    for (const s of specs) {
        s.golden = golden;
        lines.push(...(await refusalBlock(s)), '');
    }
    T.out[golden] = lines.join('\n').replace(/\n+$/, '\n');
}

// ── the routes these acts need ───────────────────────────────────────────
const route = (table) => (method, url) => {
    for (const [m, re, fn] of table) if (m === method && re.test(url)) return { body: fn(url) };
    return undefined;
};
const OK = () => ({ status: 'ok' });

// The health strip: Press A with one item of each kind that carries a verb
// (catid-health.js actionsHTML).
const CATID_ITEMS = [
    { kind: 'wrong_part', severity: 'red', headline: 'The PLC is on CAT-B', detail: 'no changeover is open',
        action: { kind: 'start_changeover', to_style_id: 2, to_style_name: 'Style B' } },
    { kind: 'unmapped_catid', severity: 'amber', headline: 'CAT-Q runs on no style',
        data: { catid: 'CAT-Q', suggestion: { style_id: 2, style_name: 'Style B' }, styles: [{ id: 1, name: 'Style A' }, { id: 2, name: 'Style B' }] } },
    { kind: 'counter_slot_idle', severity: 'amber', headline: 'The PLC counts on Slot2', data: { tag: 'MES.Slot2' } },
    { kind: 'held', severity: 'red', headline: '12 held on CAT-Q', data: { hold_id: 41 } },
];
const CATID_ROUTES = route([
    ['GET', /^\/api\/catid-health$/, () => [{ process_id: 1, name: 'Press A', worst: 'red', count: 4, headline: 'The PLC is on CAT-B' }]],
    ['GET', /^\/api\/processes\/1\/catid-health$/, () => ({ items: CATID_ITEMS })],
    ['POST', /^\/api\/catid-holds\/41\/charge\?mode=preview$/, () => ({
        hold: { qty: 12, catid: 'CAT-Q' }, style_name: 'Style B',
        rows: [{ node: 'POS_A1', qty: 12, bin_id: 77, carrier_part: 'PART-1' }],
    })],
    ['POST', /^\/api\/catid-holds\/41\/charge\?mode=charge$/, OK],
    ['POST', /^\/api\/catid-holds\/41\/write-off$/, OK],
    ['POST', /^\/api\/processes\/1\/styles\/\d+\/catid$/, OK],
    ['POST', /^\/api\/processes\/1\/counter$/, OK],
    ['POST', /^\/api\/processes\/1\/changeover\/start$/, () => ({ id: 61 })],
]);
const STYLE_ROUTES = route([
    ['POST', /^\/api\/styles\/\d+\/clone$/, () => ({ id: 7 })],
    ['PUT', /^\/api\/processes\/1\/active-style$/, OK],
    ['DELETE', /^\/api\/styles\/\d+$/, OK],
    ['GET', /^\/api\/processes\/1\/changeover\/gate-status$/, () => ({ blockers: [{ reason: 'POS_A3 is still being evacuated' }] })],
]);
// One station counter on Press A, every position countable.
const COUNTER = { id: 51, label: 'Station 1', tag_name: 'S1.Count', tag_name_2: '', last_poll_at: '', ratio: null, strokes_since_click: 0, stopped: false };
const COUNTER_ROUTES = route([
    ['GET', /^\/api\/processes\/1\/station-counters$/, () => [COUNTER]],
    ['GET', /^\/api\/process-nodes\/\d+\/countable$/, () => ({ countable: true })],
    ['POST', /^\/api\/processes\/1\/station-counters$/, () => ({ id: 52 })],
    ['PUT', /^\/api\/station-counters\/\d+$/, OK],
    ['DELETE', /^\/api\/station-counters\/\d+$/, OK],
    ['PUT', /^\/api\/process-nodes\/\d+\/counted-at$/, OK],
]);
const MISC_ROUTES = route([
    ['POST', /^\/api\/loader-boards$/, () => ({ station_id: 32 })],
    ['POST', /^\/api\/payload-catalog\/sync$/, OK],
    ['DELETE', /^\/api\/processes\/1$/, OK],
    ['POST', /^\/api\/process-groups$/, () => ({ id: 9 })],
    ['GET', /^\/api\/process-groups$/, () => T.FX.page.processGroups],
    ['GET', /^\/api\/plcs$/, () => [{ name: 'PLC_A', connected: true }, { name: 'PLC_B', connected: false }]],
    ['GET', /^\/api\/plcs\/all-tags\/[^/]+$/, () => [{ name: 'CountA', type: 'DINT' }, { name: 'Slot2', type: 'DINT' }]],
]);
const PRESET_ROUTES = route([
    ['PATCH', /^\/api\/processes\/1\/presets\/7$/, OK],
    ['POST', /^\/api\/processes\/1\/presets\/7\/archive$/, OK],
]);
const withPage = change => Object.assign(JSON.parse(JSON.stringify(T.FX.page)), change(JSON.parse(JSON.stringify(T.FX.page))));

// ── the new acts' drivers ────────────────────────────────────────────────
// Clicked off the markup the page drew: the button's own dataset is what the
// click carries, so a verb the page stops drawing is a pin that fails.
const strip = (act, more) => async page => {
    page.click('root', drawn(page, 'pd-catid', act, more));
    await page.settle();
};
const D = {
    chStart: { search: '', extra: CATID_ROUTES, drive: strip('ch-start') },
    chPin: { search: '', extra: CATID_ROUTES, drive: async page => { await strip('ch-pin')(page); await pressOK(page); } },
    chPinPick: { search: '', extra: CATID_ROUTES, drive: async page => { await strip('ch-pin-pick')(page); await pressOK(page); } },
    chChargeAsk: { search: '', extra: CATID_ROUTES, drive: strip('ch-charge') },
    chRepoint: { search: '', extra: CATID_ROUTES, drive: async page => { await strip('ch-repoint')(page); await pressOK(page); } },
    chCharge: { search: '', extra: CATID_ROUTES, drive: async page => { await strip('ch-charge')(page); await pressOK(page); } },
    chWriteOff: { search: '', extra: CATID_ROUTES, drive: async page => { await strip('ch-writeoff')(page); await pressOK(page); } },
};
// The style row's ⋯, then one of its items, as the menu drew it.
const styleMenu = (style, item, typed) => async page => {
    page.click('root', { act: 'style-menu', style: String(style) });
    page.click('root', drawn(page, 'pd-pop', 'style-' + item));
    await page.settle();
    for (const [f, v] of Object.entries(typed || {})) page.type(f, v);
    await pressOK(page);
};
Object.assign(D, {
    styleRename: { extra: STYLE_ROUTES, drive: styleMenu(2, 'rename', { name: 'Style B2' }) },
    styleCatid: { extra: STYLE_ROUTES, drive: styleMenu(1, 'catid', { catid: 'CAT-A2' }) },
    styleClone: { extra: STYLE_ROUTES, drive: styleMenu(1, 'clone') },
    styleRunning: { extra: STYLE_ROUTES, drive: styleMenu(2, 'running') },
    // A changeover open to Style B: the sheet reads the gate's blockers first.
    styleRunningOpen: {
        extra: STYLE_ROUTES, drive: styleMenu(2, 'running'),
        page: () => withPage(p => ({ processes: p.processes.map(x => (x.id === 1 ? Object.assign(x, { target_style_id: 2 }) : x)) })),
    },
    styleDelete: { extra: STYLE_ROUTES, drive: styleMenu(2, 'delete') },
});
// Station counters, on Settings with COUNTER on the process.
const scSheet = (label, tag) => async page => {
    page.byId('sc-label').value = label;
    page.byId('sc-tag').value = tag;
    await pressOK(page);
};
const countedAt = (node, value) => async page => {
    const sel = page.root.querySelectorAll('select[data-countedat]').find(el => el.dataset.countedat === String(node));
    if (!sel) throw new Error('Settings draws no Counted at for ' + node);
    sel.value = value;
    page.fire(sel, 'change');
    await page.settle();
};
Object.assign(D, {
    scNew: { hash: '#tab=settings', extra: COUNTER_ROUTES, drive: async page => {
        page.click('root', rootButton(page, 'sc-new'));
        await scSheet('Station 2', 'S2.Count')(page);
    } },
    scEdit: { hash: '#tab=settings', extra: COUNTER_ROUTES, drive: async page => {
        page.click('root', rootButton(page, 'sc-edit', { sc: 51 }));
        await scSheet('Station 1b', 'S1.Count')(page);
    } },
    scCountedAt: { hash: '#tab=settings', extra: COUNTER_ROUTES, drive: countedAt(201, '51') },
    scCountedAtNew: { hash: '#tab=settings', extra: COUNTER_ROUTES, drive: async page => {
        await countedAt(202, 'new')(page);
        await scSheet('Station 3', 'S3.Count')(page);
    } },
    // Change positions…: every countable box opens ticked (the only counter,
    // counting none: station-card.js rows); all but POS_A1 and POS_A2 are
    // unticked, so the save binds those two.
    scPositions: { hash: '#tab=settings', extra: COUNTER_ROUTES, drive: async page => {
        page.click('root', rootButton(page, 'sc-checklist', { sc: 51 }));
        const sel = 'input[data-scpos][data-sc="51"]';
        const keep = new Set(page.S.curtainNodes.filter(n => n.core_node_name === 'POS_A1' || n.core_node_name === 'POS_A2').map(n => String(n.id)));
        for (const el of page.root.querySelectorAll(sel)) el.checked = keep.has(el.dataset.scpos);
        page.click('root', rootButton(page, 'sc-positions', { sc: 51 }));
        await page.settle();
        await pressOK(page);
    } },
    scDelete: { hash: '#tab=settings', extra: COUNTER_ROUTES, drive: async page => {
        page.click('root', rootButton(page, 'sc-delete', { sc: 51 }));
        await pressOK(page);
    } },
});
Object.assign(D, {
    posAdd: { hash: '#tab=settings', drive: async page => {
        page.click('root', rootButton(page, 'pos-add'));
        await page.settle();
        T.npk(page, 'scrim', 'addpos', 'NEW_1');
        await pressOK(page);
    } },
    posRemoveAsk: { hash: '#tab=settings', drive: async page => {
        page.click('root', rootButton(page, 'pos-remove', { node: 'POS_A11' }));
        await page.settle();
    } },
    posRemove: { hash: '#tab=settings', drive: async page => {
        page.click('root', rootButton(page, 'pos-remove', { node: 'POS_A11' }));
        await page.settle();
        await pressOK(page);
    } },
    bindLoader: {
        hash: '#tab=screens', extra: MISC_ROUTES,
        page: () => withPage(() => ({ loaderGaps: [{ loader_key: 'LDR_1' }] })),
        drive: async page => { page.click('root', rootButton(page, 'bind-loader')); await page.settle(); },
    },
    sync: { hash: '#tab=settings', extra: MISC_ROUTES, drive: async page => {
        page.click('root', rootButton(page, 'st-sync'));
        await page.settle();
    } },
    deleteProcess: { hash: '#tab=settings', extra: MISC_ROUTES, drive: async page => {
        page.click('root', rootButton(page, 'st-delete'));
        page.type('confirm', 'Press A');
        await pressOK(page);
    } },
    addGroup: { search: '', extra: MISC_ROUTES, drive: async page => {
        page.click('root', rootButton(page, 'add-group'));
        page.type('name', 'Welders 2');
        await pressOK(page);
    } },
});
// Recipes: the ⋯ menu's two items, and Apply to parts…
const presetMenu = (item, typed) => async page => {
    page.click('root', { act: 'preset-menu', preset: '7' });
    page.click('root', drawn(page, 'pd-pop', 'preset-' + item));
    await page.settle();
    for (const [f, v] of Object.entries(typed || {})) page.type(f, v);
    await pressOK(page);
};
// The apply modal on Style B: tick, answer each "which part?" with its first
// part, then Save. The modal's controls answer on the scrim (onAdvClick).
async function applyTick(page) {
    page.click('root', { act: 'preset-apply', preset: '7' });
    await page.settle();
    page.click('scrim', { act: 'papply-tick', style: '2' });
    await page.settle();
    for (let guard = 0; guard < 6; guard++) {
        const need = page.scrim.querySelectorAll('[data-act="papply-pick"]').find(b => b.dataset.style === '2');
        if (!need) break;
        page.click('scrim', Object.assign({}, need.dataset));
        await page.settle();
        const part = page.scrim.querySelectorAll('[data-act="papply-part"]').find(b => b.dataset.style === '2');
        if (!part) throw new Error('the apply modal offers no part for ' + need.dataset.node);
        page.click('scrim', Object.assign({}, part.dataset));
        await page.settle();
    }
}
Object.assign(D, {
    presetRename: { hash: '#tab=presets', extra: PRESET_ROUTES, drive: presetMenu('rename', { name: 'Recipe Uno' }) },
    presetArchive: { hash: '#tab=presets', extra: PRESET_ROUTES, drive: presetMenu('archive') },
    applyPreview: { hash: '#tab=presets', previews: true, drive: applyTick },
    applySave: { hash: '#tab=presets', setup: applyTick, drive: async page => { await pressOK(page); } },
});

// ── the new request goldens ──────────────────────────────────────────────
async function requests(golden, acts, head) {
    T.setCurrent(golden);
    const lines = head ? head.map(h => '# ' + h) : [];
    for (const [title, d, after] of acts) {
        const page = await T.openPage({ hash: d.hash, page: d.page && d.page(), search: d.search, extra: d.extra });
        if (d.setup) await d.setup(page);
        await page.settle();
        const from = page.mark();
        await d.drive(page);
        await page.settle();
        lines.push(...T.actLines(title, page, from));
        if (after) lines.push(...[].concat(after(page)));
        lines.push('');
    }
    T.out[golden] = lines.join('\n').replace(/\n+$/, '\n');
}
// A spec's page is built per run (withPage copies the fixture).
function specOf(title, d, more) {
    return Object.assign({ title: title }, d, { page: d.page && d.page() }, more || {});
}

async function pinRequests() {
    await requests('requests-catid-strip.txt', [
        ['the list\'s health strip: Start changeover to Style B (one tap, no sheet)', D.chStart],
        ['Pin to Style B (the suggestion), then the sheet\'s Pin', D.chPin],
        ['the chooser\'s Pin, on its first option (Style A), then the sheet\'s Pin', D.chPinPick],
        ['Count Slot2, then the sheet\'s Count', D.chRepoint],
        ['Charge: its preview, then the sheet\'s Charge', D.chCharge],
        ['Write off, then the sheet\'s Write off', D.chWriteOff],
    ], ['The list (no ?process=), its health strip drawn from /api/catid-health: one process, one item of each',
        'kind that carries a verb. Each act on a fresh list.']);
    await requests('requests-style-menu.txt', [
        ['Style B ⋯ › Rename: Style B2', D.styleRename],
        ['Style A ⋯ › Expected CATID: CAT-A2', D.styleCatid],
        ['Style A ⋯ › Clone, the name as it opens', D.styleClone],
        ['Style B ⋯ › Mark as running (no changeover open)', D.styleRunning],
        ['Style B ⋯ › Mark as running with a changeover to Style B open (the sheet reads the gate first)', D.styleRunningOpen],
        ['Style B ⋯ › Delete', D.styleDelete],
    ]);
    await requests('requests-station-counters.txt', [
        ['Settings › + Add station counter: Station 2 on S2.Count', D.scNew],
        ['Settings › Station 1 › Edit: Station 1b', D.scEdit],
        ['Settings › POS_A1 › Counted at: Station 1 (the select\'s change)', D.scCountedAt],
        ['Settings › POS_A2 › Counted at: New station counter… (Station 3 on S3.Count, then the bind)', D.scCountedAtNew],
        ['Settings › Station 1 › Change positions…: POS_A1 and POS_A2 ticked, the rest unticked, Save positions', D.scPositions],
        ['Settings › Station 1 › Remove', D.scDelete],
    ], ['Settings with one station counter on Press A (Station 1, id 51), every position countable.']);
    await requests('requests-positions.txt', [
        ['Settings › Add positions: NEW_1 onto Screen A (the first screen)', D.posAdd],
        ['Settings › POS_A11 › Remove (the impact is read before the sheet opens)', D.posRemove],
        ['Operator screens › Loader windows › Make a screen on Press A (LDR_1)', D.bindLoader],
    ]);
    await requests('requests-settings-misc.txt', [
        ['Settings › Sync from Core', D.sync],
        ['Settings › Delete process: the name typed, then Delete Press A', D.deleteProcess],
        ['the list › Add group: Welders 2', D.addGroup],
    ]);
    await requests('requests-presets.txt', [
        ['Recipes › Recipe One ⋯ › Rename: Recipe Uno', D.presetRename],
        ['Recipes › Recipe One ⋯ › Archive', D.presetArchive],
        ['Recipes › Apply to parts…: tick Style B (its preview), each "which part?" answered with its first part', D.applyPreview,
            page => '-- apply OK ' + JSON.stringify(T.text(okButton(page).innerHTML)) + (okButton(page).disabled ? ' disabled' : ' enabled')],
        ['…then Save to 1 part', D.applySave],
    ]);
    await pinReads();
}

// The reads the tab clicks, the rail's re-read, the part popover and the
// PLC suggestions send; no write in any of them.
async function pinReads() {
    T.setCurrent('requests-reads.txt');
    const lines = ['# Reads only: what each act asks the Edge for. Each on a fresh page on Press A\'s Styles tab',
        '# unless it says otherwise.'];
    const act = async (title, open, drive) => {
        const page = await T.openPage(open || {});
        const from = page.mark();
        await drive(page);
        await page.settle();
        lines.push(...T.actLines(title, page, from), '');
    };
    for (const tab of ['settings', 'screens', 'presets']) {
        await act('the ' + tab + ' tab clicked', null, async page => { page.click('root', { tab: tab }); });
    }
    await act('window focus on the Styles tab (refreshRail)', null, async page => { page.fireWindow('focus'); });
    await act('the Part chip on POS_A1 (the part popover opens)', null, async page => {
        page.click('root', { act: 'pick', node: 'POS_A1', kind: 'part' });
    });
    // A PLC box is drawn only under a curtain switched on (cuCombo); the
    // counter's tag box names its PLC field by selector (data-plcsel), which
    // reads PLC_A off the drawn draft.
    await act('Settings: POS_A1\'s curtain switched on and its PLC box focused, then the counter\'s tag box focused',
        { hash: '#tab=settings', extra: MISC_ROUTES }, async page => {
            const node = page.S.curtainNodes.find(n => n.core_node_name === 'POS_A1');
            page.click('root', { act: 'cu-toggle', node: String(node.id) });
            const box = (kind, attr, v) => {
                const w = page.root.querySelectorAll('[data-combo]').find(c => c.dataset.combo === kind && c.dataset[attr] === v);
                if (!w) throw new Error('Settings draws no ' + kind + ' suggestion box with ' + attr + '=' + v);
                return w.querySelector('input');
            };
            page.fire(box('plc', 'cn', String(node.id)), 'focus');
            await page.settle();
            page.fire(box('tag', 'suggest', 'counter'), 'focus');
            await page.settle();
        });
    T.out['requests-reads.txt'] = lines.join('\n').replace(/\n+$/, '\n');
}

// ── the refusal goldens ──────────────────────────────────────────────────
//
// BUGS THESE PIN, AS THEY ARE (each is a line of a golden below, and stays
// one until a named change moves it; processes-desktop.js unless named):
//   - A 500 with no body previews as "Preview OK · null orders": runPreview
//     parses whatever came back (:2400) with no res.ok, and the bar prints
//     the count it did not get (composer-model.js:2053, :2074).
//   - A preview with no answer is silent: the catch (:2404) swallows it and
//     the bar keeps the last preview's words. Save style with no answer is
//     an unhandled rejection and nothing on screen (FINDINGS F22's silence;
//     every postJSON write with no answer is the same: :6049 has no catch).
//   - A lapsed session's write reads as done: adminMiddleware's 303 lands on
//     the login page, 200, and postJSON takes any 2xx as ok with an empty
//     body (:6056). Add process then writes to /api/processes/NaN/… (:6785),
//     recipes and the catalog sync claim "Saved", and the next read throws.
//   - A refused routing-set write shows nothing: the refusal is set (:3715)
//     and loadRouting's next good read clears it (:3746) before the draw.
//   - A refused Save settings throws the typed draft away: the draft is
//     rebuilt from the stored process whatever the answer (:5368). With no
//     answer it clears its own messages (:5306) and never redraws.
//   - The catalog sync drops the server's sentence for its status (:7084).
//   - Apply with no answer wedges the modal: S.papply.running stays true,
//     runSheet's finally re-enables the button (:5917), and every press after
//     returns at :7895 — "Saving…", forever.
//   - A refusal midway through a chain is retried from the top by the next
//     press: Add process POSTs a second process, New station counter… a
//     second counter (:4825), the mint a second mint.
//   - "Creating…" stays on Add process's status line on a dead Edge (:6781).
//   - Apply retries a plain 409 as if it were stale (desktop-bodies.js:431).
async function pinRefusals() {
    const U = T.DRIVE;
    await refusals('refusals-flow.txt', [
        specOf('flow edit: the debounced preview (POS_A5 source, POS_A1 key route)', U.flowEdit, { previews: true }),
        specOf('Save style', U.flowSave),
    ]);
    await refusals('refusals-style-create.txt', [specOf('+ New style with no recipe (Style C, CAT-C)', U.styleCreate, { again: true })]);
    await refusals('refusals-flow-mint.txt', [specOf('+ New style from Recipe One, a part per slot and a CATID', U.mint, { again: true })]);
    await refusals('refusals-add-process.txt', [specOf('Add process (the chain of five writes)', U.addProcess, { again: true })]);
    await refusals('refusals-settings-save.txt', [specOf('Save settings (U0\'s act: five writes)', U.settingsSave)]);
    await refusals('refusals-screen-sheet.txt', [
        specOf('Edit Screen A (POS_A11 off, NEW_3 on)', U.screenEdit, { again: true }),
        specOf('Add operator screen (Screen C on NEW_1)', U.screenAdd, { again: true }),
    ]);
    await refusals('refusals-part-set.txt', [
        specOf('Part popover: + PART-12 (on a fresh page)', U.partPopAdd),
        specOf('Part popover: × PART-11 (on a fresh page)', U.partPopDrop),
        specOf('Settings › Parts: + PART-13', U.partSettingsAdd),
        specOf('Settings › Parts: × PART-11', U.partSettingsDrop),
    ]);
    await refusals('refusals-routing-set.txt', [
        specOf('Routing set: add SRC_2 (POST)', U.routingAddNew),
        specOf('Routing set: add DST_9 (PATCH adopts)', U.routingAdopt),
        specOf('Routing set: × STG_3 (DELETE)', U.routingDrop),
        specOf('Routing set: click STG_1 (DELETE)', U.routingToggleOff),
        specOf('Routing set: Turn all 1 on (Staging)', U.routingAllOn),
    ]);
    await refusals('refusals-recipe-create.txt', [
        specOf('Save as recipe… on Style A', U.recipeFromStyle, { again: true }),
        specOf('Save recipe… on a new draft (cells)', U.recipeCells, { again: true }),
        specOf('Save recipe… with groups', U.recipeGrouped, { again: true }),
    ]);
    await refusals('refusals-catid-strip.txt', [
        specOf('Start changeover to Style B', D.chStart),
        specOf('Pin to Style B', D.chPin, { again: true }),
        specOf('the chooser\'s Pin (Style A)', D.chPinPick, { again: true }),
        specOf('Count Slot2', D.chRepoint, { again: true }),
        specOf('Charge: the sheet its preview opens', D.chChargeAsk),
        specOf('Charge: the sheet\'s Charge', D.chCharge, { again: true, refuse: /mode=charge/ }),
        specOf('Write off', D.chWriteOff, { again: true }),
    ]);
    await refusals('refusals-style-menu.txt', [
        specOf('Rename Style B', D.styleRename, { again: true }),
        specOf('Expected CATID on Style A', D.styleCatid, { again: true }),
        specOf('Clone Style A', D.styleClone, { again: true }),
        specOf('Mark Style B as running', D.styleRunning, { again: true }),
        specOf('Delete Style B', D.styleDelete, { again: true }),
    ]);
    await refusals('refusals-station-counters.txt', [
        specOf('+ Add station counter', D.scNew, { again: true }),
        specOf('Edit Station 1', D.scEdit, { again: true }),
        specOf('POS_A1 › Counted at: Station 1', D.scCountedAt),
        specOf('POS_A2 › Counted at: New station counter… (the create, then the bind)', D.scCountedAtNew, { again: true }),
        specOf('Change positions… › Save positions (two binds)', D.scPositions, { again: true }),
        specOf('Remove Station 1', D.scDelete, { again: true }),
    ]);
    await refusals('refusals-positions.txt', [
        specOf('Add positions: NEW_1', D.posAdd, { again: true }),
        specOf('Remove POS_A11: the sheet the impact read opens', D.posRemoveAsk),
        specOf('Remove POS_A11: the sheet\'s Remove', D.posRemove, { again: true, refuse: /^PUT / }),
        specOf('Make a screen for LDR_1', D.bindLoader),
    ]);
    await refusals('refusals-settings-misc.txt', [
        specOf('Sync from Core', D.sync),
        specOf('Delete process', D.deleteProcess, { again: true }),
        specOf('Add group', D.addGroup, { again: true }),
    ]);
    await refusals('refusals-presets.txt', [
        specOf('Rename Recipe One', D.presetRename, { again: true }),
        specOf('Archive Recipe One', D.presetArchive, { again: true }),
        specOf('Apply: the tick\'s preview (and each pick\'s)', D.applyPreview),
        specOf('Apply: Save to 1 part', D.applySave, { again: true }),
    ]);
}

// ── the sheet stack ──────────────────────────────────────────────────────
//
// The overlay stack (shared/layer.js, which this page's own stack moved into):
// what opening, running, closing and Escape do, one act per block. Every block
// starts on a fresh page. A sheet is the layer's shell (pageSheet) or
// #pd-scrim (Advanced and the apply modal, which draw their own markup); the
// discard question is the layer's confirm, on the stack above the sheet.
function stackFacts(page) {
    const S = page.S, scrim = page.byId('pd-scrim');
    const q = page.dialog();
    return 'sheet ' + (page.sheetOpen() ? 'open (' + (page.scrim === scrim ? '#pd-scrim' : 'the layer\'s shell') + ')' : 'closed') +
        ', #pd-scrim markup ' + (scrim.innerHTML ? 'drawn' : 'empty') + ', layers ' + page.pd('layerDepth')() +
        ', body overflow ' + JSON.stringify(page.doc.body.style.overflow || '') +
        ', S.sheet ' + (S.sheet ? 'set' : 'null') + ', S.adv ' + (S.adv ? 'set' : 'null') +
        ', question ' + (q ? JSON.stringify(q.text) + ' [' + q.buttons.join(' · ') + ']' : 'none');
}
function popFacts(page) {
    return ['pd-pop', 'pd-stpop', 'pd-advpop'].map(id => {
        const p = page.byId(id);
        return id + ' ' + (!p ? 'absent' : p.hidden ? 'hidden' : 'open');
    }).join(', ');
}
function modalShape(page) {
    const html = page.scrim.innerHTML;
    if (page.scrim !== page.byId('pd-scrim')) return shellShape(page, html);
    const m = /<div class="pd-modal([^"]*)"([^>]*)>/.exec(html);
    const lines = [];
    lines.push('modal: class "pd-modal' + (m ? m[1] : '') + '"' + (m ? m[2].replace(/\s+/g, ' ') : ' (none)'));
    lines.push('header: ' + JSON.stringify(sheetTitle(page)) + ', sub ' + JSON.stringify(T.text((/<div class="mh">[\s\S]*?<p>([\s\S]*?)<\/p>/.exec(html) || ['', ''])[1])));
    lines.push('fields: ' + ([...html.matchAll(/data-f="([^"]*)"/g)].map(x => x[1]).join(', ') || 'none'));
    const foot = (/<div class="mf">([\s\S]*?)<\/div>/.exec(html) || ['', ''])[1];
    lines.push('footer: ' + [...foot.matchAll(/<(span|button) class="([^"]*)"([^>]*)>([\s\S]*?)<\/\1>/g)]
        .map(x => x[1] === 'span' ? 'status(' + x[2] + ')' : x[2] + ' [' + ((/data-act="([^"]*)"/.exec(x[3]) || [])[1] || '') + '] ' +
            JSON.stringify(T.text(x[4])) + (/\bdisabled\b/.test(x[3]) ? ' disabled' : '')).join(' · '));
    lines.push('inner popover: ' + (/id="pd-advpop"[^>]*\bhidden\b/.test(html) ? '#pd-advpop, hidden' : /id="pd-advpop"/.test(html) ? '#pd-advpop' : 'none'));
    return lines;
}
// The same lines for a sheet on the layer's own shell: its card, its head,
// the fields its body draws, its footer (the status line, Cancel, OK).
function shellShape(page, html) {
    const host = page.scrim;
    const card = host.querySelector('[role="dialog"]');
    const head = host.querySelector('.layer-head');
    const foot = host.querySelector('.layer-foot');
    const sub = head && head.querySelector('p');
    const attr = n => ' ' + n + '="' + (card.getAttribute(n) || '') + '"';
    return [
        'modal: class "' + card.className + '"' + attr('role') + attr('aria-modal') + attr('aria-label'),
        'header: ' + JSON.stringify(sheetTitle(page)) + ', sub ' + JSON.stringify(sub ? sub.textContent : ''),
        'fields: ' + ([...html.matchAll(/data-f="([^"]*)"/g)].map(x => x[1]).join(', ') || 'none'),
        'footer: ' + foot.children.map(b => b.tagName === 'SPAN' ? 'status(' + b.className + ')'
            : b.className + (b.hasAttribute('data-layer-ok') ? ' [data-layer-ok]' : '') + ' ' + JSON.stringify(b.textContent) +
                (b.disabled ? ' disabled' : '')).join(' · '),
        'inner popover: ' + (/id="pd-advpop"[^>]*\bhidden\b/.test(html) ? '#pd-advpop, hidden' : /id="pd-advpop"/.test(html) ? '#pd-advpop' : 'none'),
    ];
}
async function pinSheetStack() {
    // Every page a block opens, for the facts at the foot.
    const pages = [];
    const open = async o => { const p = await T.openPage(o); pages.push(p); return p; };
    T.setCurrent('sheet-stack.txt');
    const L = ['# The sheet stack on shared/layer.js (pageSheet / runSheet / closeSheet / Escape / the discard question /',
        '# Advanced), one act per block, each on a fresh page. "facts" is the sheet, the layer\'s depth, body scroll',
        '# lock, the page\'s sheet state and the question; "focus" is every element focus() was called on during the act.'];
    const block = (title, rows) => { L.push('', '## ' + title); for (const r of rows) L.push(r); };
    const focusSince = (page, n) => {
        const f = page.focused.slice(n);
        return 'focus: ' + (f.length ? f.map(e => e.tagName.toLowerCase() + (e.id ? '#' + e.id : '') + (e.className ? '.' + e.className : '') +
            (e.dataset && e.dataset.act ? '[data-act="' + e.dataset.act + '"]' : '')).join(', ') : 'none');
    };
    const openRename = async page => {
        page.click('root', { act: 'style-menu', style: '2' });
        page.click('root', drawn(page, 'pd-pop', 'style-rename'));
        await page.settle();
    };

    {   // openSheet
        const page = await open({ extra: STYLE_ROUTES });
        const rows = ['before: ' + stackFacts(page)];
        const f0 = page.focused.length;
        await openRename(page);
        rows.push('after Style B ⋯ › Rename: ' + stackFacts(page), ...modalShape(page), focusSince(page, f0),
            'active element: ' + (page.doc.activeElement ? 'set' : 'none'), popFacts(page));
        const f1 = page.focused.length;
        page.click('scrim', { act: 'sheet-cancel' });
        await page.settle();
        rows.push('after Cancel: ' + stackFacts(page), focusSince(page, f1));
        block('pageSheet: the style menu\'s Rename sheet, then Cancel', rows);
    }
    {   // the backdrop (ruling 2): Escape for the innermost, opt-in, never a drag from the card
        const rows = [];
        let page = await open({ extra: STYLE_ROUTES });
        await openRename(page);
        page.pressBackdrop();
        await page.settle();
        rows.push('nothing typed, pressed and released on the backdrop: ' + stackFacts(page));
        page = await open({ extra: STYLE_ROUTES });
        await openRename(page);
        page.pressBackdrop(true);
        await page.settle();
        rows.push('pressed in the card, released on the backdrop: ' + stackFacts(page));
        page = await open({ extra: STYLE_ROUTES });
        await openRename(page);
        page.type('name', 'Style B typed');
        page.pressBackdrop();
        await page.settle();
        rows.push('a typed name, pressed and released on the backdrop: ' + stackFacts(page));
        page.dialog().press('Keep editing');
        await page.settle();
        rows.push('Keep editing: ' + stackFacts(page) + '; name reads ' + JSON.stringify(page.pd('sheetValue')('name')));
        block('the backdrop (ruling 2: the Processes sheets opt in)', rows);
    }
    {   // one run per press
        const page = await open({ extra: STYLE_ROUTES });
        await openRename(page);
        page.type('name', 'Style B2');
        const from = page.mark();
        page.click('scrim', { act: 'sheet-ok' });
        const between = 'between the presses: ' + stackFacts(page) + ', OK ' + (okButton(page).disabled ? 'disabled' : 'enabled');
        page.click('scrim', { act: 'sheet-ok' });
        await page.settle();
        block('runSheet: OK pressed twice before the first write answers', [between,
            'requests: ' + page.since(from).filter(c => c.method !== 'GET').map(lineOf).join(' · '), 'after: ' + stackFacts(page)]);
    }
    {   // a refused run re-enables OK and runs again on the next press
        let refuse = true;
        const page = await open({ extra: STYLE_ROUTES, answer: (m, u) => (m === 'PUT' && refuse ? { status: 400, body: { error: MSG } } : undefined) });
        await openRename(page);
        page.type('name', 'Style B2');
        let from = page.mark();
        await pressOK(page);
        const first = 'first press: ' + page.since(from).map(lineOf).join(' · ') + ' → ' + sheetLine(page);
        refuse = false;
        from = page.mark();
        await pressOK(page);
        block('runSheet: a refused run, then OK again', [first,
            'second press: ' + page.since(from).filter(c => c.method !== 'GET').map(lineOf).join(' · ') + ' → ' + sheetLine(page)]);
    }
    {   // closeSheet clears
        const page = await open({ hash: '#tab=settings' });
        page.click('root', rootButton(page, 'pos-add'));
        await page.settle();
        T.npk(page, 'scrim', 'addpos', 'NEW_1');
        const S = page.S;
        const keys = () => Object.keys(S.pickers).sort().join(', ');
        const rows = ['open Add positions, NEW_1 ticked: ' + stackFacts(page) + '; S.addPos ' + (S.addPos ? 'set' : 'null') + '; pickers ' + keys()];
        page.click('scrim', { act: 'sheet-cancel' });
        await page.settle();
        rows.push('after Cancel: ' + stackFacts(page) + '; S.addPos ' + (S.addPos ? 'set' : 'null') + '; pickers ' + keys() +
            ' (Settings\' own pickers stay: their boxes are in #pd-root)');
        block('closeSheet: what it clears (Add positions on Settings)', rows);
    }
    {
        const page = await open({ hash: '#tab=presets' });
        page.click('root', { act: 'preset-apply', preset: '7' });
        await page.settle();
        page.click('scrim', { act: 'papply-tick', style: '2' });
        await page.settle();
        const S = page.S;
        const rows = ['apply modal, Style B ticked: ' + stackFacts(page) + '; S.papply ' + (S.papply ? 'rows ' + Object.keys(S.papply.rows).join(',') : 'null')];
        page.click('scrim', { act: 'sheet-cancel' });
        await page.settle();
        rows.push('after Cancel: ' + stackFacts(page) + '; S.papply ' + (S.papply ? 'set' : 'null'));
        page.click('root', { act: 'preset-apply', preset: '7' });
        await page.settle();
        rows.push('reopened: ' + (Object.keys(S.papply.rows).length ? 'rows ticked ' + Object.keys(S.papply.rows).join(',') : 'nothing ticked'));
        block('closeSheet: the apply modal\'s ticks go with it', rows);
    }
    {
        const page = await open();
        page.click('root', { act: 'new-style' });
        const chooser = page.byId('pd-newrecipe');
        chooser.value = '7';
        page.fire(chooser, 'change');
        await page.settle();
        const S = page.S;
        const rows = ['New style, Recipe One picked: ' + stackFacts(page) + '; S.newStyle ' + (S.newStyle ? 'set' : 'null')];
        page.click('scrim', { act: 'sheet-cancel' });
        await page.settle();
        rows.push('after Cancel: ' + stackFacts(page) + '; S.newStyle ' + (S.newStyle ? 'set' : 'null'));
        block('closeSheet: New style\'s spots go with it', rows);
    }
    // ── Escape, innermost first ──
    const esc = async (page, rows, what) => {
        page.key('Escape');
        await page.settle();
        rows.push('Escape → ' + (what ? what(page) + '; ' : '') + stackFacts(page) + '; ' + popFacts(page));
    };
    {
        const page = await open();
        const rows = [];
        page.click('root', { act: 'style-menu', style: '2' });
        rows.push('style menu open: ' + popFacts(page));
        await esc(page, rows);
        block('Escape: the page popover (#pd-pop) with no sheet', rows);
    }
    {
        const page = await open({ hash: '#tab=settings' });
        const rows = [];
        page.click('root', { act: 'st-group' });
        rows.push('Settings group list open: ' + popFacts(page));
        await esc(page, rows);
        block('Escape: Settings\' group list (#pd-stpop)', rows);
    }
    {
        const page = await open();
        const rows = [];
        page.click('root', { act: 'add-process' });
        await page.settle();
        page.click('scrim', { act: 'add-pickgroup' }, { inside: ['.pd-modal'] });
        rows.push('Add process with its group list open: ' + stackFacts(page) + '; ' + popFacts(page));
        await esc(page, rows);
        await esc(page, rows);
        block('Escape: a popover in a sheet (#pd-advpop), then the sheet (nothing typed)', rows);
    }
    const advNode = () => 'POS_A1';
    {
        const page = await open();
        const rows = [];
        page.click('root', { act: 'advanced', node: advNode() });
        rows.push('Advanced open on POS_A1: ' + stackFacts(page), ...modalShape(page).filter(l => /^(modal|header|footer)/.test(l)));
        const pick = page.scrim.querySelectorAll('[data-advkind="pick"]')[0];
        if (pick) {
            page.click('scrim', Object.assign({}, pick.dataset), { cls: [] });
            rows.push('its first picker (' + pick.dataset.adv + ') clicked: ' + popFacts(page));
            await esc(page, rows);
        }
        await esc(page, rows);
        block('Escape: Advanced\'s own picker, then Advanced (nothing typed)', rows);
    }
    {
        const page = await open();
        const rows = [];
        page.click('root', { act: 'advanced', node: advNode() });
        const num = page.scrim.querySelectorAll('[data-advkind="number"]')[0];
        if (!num) throw new Error('Advanced on POS_A1 draws no number field');
        const key = num.dataset.adv;
        num.value = '7';
        page.fire(num, 'input');
        rows.push('typed 7 into ' + key + ': S.adv.a.' + key + '=' + page.S.adv.a[key]);
        await esc(page, rows, p => 'sheet ' + JSON.stringify(sheetTitle(p)));
        // Escape on the question is Keep editing: Escape closes the innermost
        // entry, and the question is a confirm on the stack, which always
        // resolves (S2-components lead decision 4).
        page.key('Escape');
        await page.settle();
        const back = page.scrim.querySelectorAll('[data-advkind="number"]').find(e => e.dataset.adv === key);
        rows.push('Escape again, with the question up (Keep editing) → sheet ' + JSON.stringify(sheetTitle(page)) + ', ' + key + ' field reads ' +
            JSON.stringify(back ? back.value : '(gone)') + ', S.adv.a.' + key + '=' + page.S.adv.a[key] + '; ' + stackFacts(page));
        await esc(page, rows, p => 'sheet ' + JSON.stringify(sheetTitle(p)));
        page.dialog().press('Keep editing');
        await page.settle();
        rows.push('Keep editing → sheet ' + JSON.stringify(sheetTitle(page)) + ', S.adv.a.' + key + '=' + page.S.adv.a[key] + '; ' + stackFacts(page));
        await esc(page, rows, p => 'sheet ' + JSON.stringify(sheetTitle(p)));
        page.dialog().press('Discard');
        await page.settle();
        rows.push('Discard → ' + stackFacts(page));
        block('the discard question on Advanced: a typed number, Escape, Escape, Escape, Keep editing, Escape, Discard', rows);
    }
    {
        const page = await open({ extra: STYLE_ROUTES });
        const rows = [];
        await openRename(page);
        page.type('name', 'Style B typed');
        await esc(page, rows, p => 'sheet ' + JSON.stringify(sheetTitle(p)));
        const q = page.dialog();
        rows.push('question: ' + JSON.stringify(q.text) + ', buttons ' + q.buttons.map(b => JSON.stringify(b)).join(' · '));
        page.dialog().press('Keep editing');
        await page.settle();
        rows.push('Keep editing → sheet ' + JSON.stringify(sheetTitle(page)) + ', name reads ' + JSON.stringify(page.pd('sheetValue')('name')) + '; ' + stackFacts(page));
        const from = page.mark();
        await pressOK(page);
        rows.push('then OK (the sheet\'s own run): ' + page.since(from).filter(c => c.method !== 'GET' && !isPreview(c))
            .map(c => lineOf(c) + ' ' + JSON.stringify(c.body)).join(' · '));
        block('the discard question on a sheet: a typed name, Escape, Keep editing, OK', rows);
    }
    {
        const page = await open({ extra: STYLE_ROUTES });
        const rows = [];
        await openRename(page);
        page.type('name', 'Style B typed');
        await esc(page, rows, p => 'sheet ' + JSON.stringify(sheetTitle(p)));
        const from = page.mark();
        page.dialog().press('Discard');
        await page.settle();
        rows.push('Discard → ' + stackFacts(page) + '; requests ' + (page.since(from).length || 'none'));
        block('the discard question on a sheet: a typed name, Escape, Discard', rows);
    }
    {
        const page = await open();
        const rows = [];
        await T.DRIVE.recipeGroupsOpen.drive(page);
        page.click('root', { act: 'gfocus', g: '0' });
        rows.push('recipe group 0 pinned: S.shapeDraft.focus ' + JSON.stringify(page.S.shapeDraft.focus));
        await esc(page, rows, p => 'S.shapeDraft.focus ' + JSON.stringify(p.S.shapeDraft.focus));
        block('Escape with no layer: a pinned recipe group lets go', rows);
    }
    {
        const page = await open();
        const rows = [];
        page.click('root', { row: 'POS_A3' });
        rows.push('row POS_A3 clicked: S.selected ' + JSON.stringify(page.S.selected));
        await esc(page, rows, p => 'S.selected ' + JSON.stringify(p.S.selected));
        await esc(page, rows, p => 'S.selected ' + JSON.stringify(p.S.selected));
        block('Escape with no layer: the selected position lets go (then nothing)', rows);
    }
    // A BUG, PINNED AS IS: Apply on a sheet nothing was changed in writes an
    // all-defaults advanced block onto the cell (setAdvanced with the sheet's
    // whole draft, processes-desktop.js:4169), so the flow reads dirty and the
    // next Save sends the block.
    {
        const page = await open();
        const rows = [];
        page.click('root', { act: 'advanced', node: advNode() });
        rows.push('open: ' + stackFacts(page));
        page.click('scrim', { act: 'adv-cancel' });
        await page.settle();
        rows.push('Cancel: ' + stackFacts(page));
        page.click('root', { act: 'advanced', node: advNode() });
        const from = page.mark();
        page.click('scrim', { act: 'adv-apply' });
        await page.settle();
        rows.push('reopened, Apply with nothing changed: ' + stackFacts(page) + '; dirty=' + page.pd('dirty')() + '; requests ' +
            (page.since(from).map(lineOf).join(' · ') || 'none'));
        block('the Advanced sheet: open, Cancel, open, Apply', rows);
    }
    // THE FACTS, MEASURED over every page the blocks above opened: the lock
    // is the layer's counted one, and focus is every focus() call made in them.
    const focusCalls = pages.reduce((n, p) => n + p.focused.length, 0);
    const keydowns = pages.map(p => (p.doc.listeners.keydown || []).length);
    L.push('', '## facts',
        'body scroll lock: overflow "hidden" while a sheet is up, "" once it closes (the layer\'s counted lock; every block above)',
        'focus() calls across every block above: ' + focusCalls +
            (focusCalls ? '' : ' — nothing moves focus into a sheet, and nothing returns it on close'),
        'Escape: document keydown listeners per page, at most ' + Math.max(...keydowns) +
            ' (the page\'s own, for nothing open; the layer\'s, added with its first entry); the window listeners boot adds: ' +
            ['error', 'unhandledrejection', 'resize', 'focus'].filter(t => T.lastPage.hasWindowListener(t)).join(', '));
    T.out['sheet-stack.txt'] = L.join('\n') + '\n';
}

// ── the door bugs ────────────────────────────────────────────────────────
//
// THE S2 DOOR BUGS the refusal pins found (REPORT-v2-stop1-P0, the bug table:
// "S2 HTTP door on the Processes page", "S2 door conversion", ruling 3), each
// driven on a fresh page with the one answer that shows it, and what must hold
// once it is fixed. A line is "ok   <what holds>" or "BUG  <what holds>:
// <what happened>". The golden holds only ok lines, so each block was red
// before its fix and goes red again if the bug comes back. The refusal
// goldens say what the page does; these say what it must not do.
const NO_ANSWER = { reject: 'Failed to fetch' };
const LAPSED = { status: 200, raw: '<!doctype html><title>Log in</title>', redirected: true, sticky: true };
const BODILESS_500 = { status: 500, raw: '' };
// What a run put on screen: the runs of text that were not there before the
// refused write, the sheet's status line, and the notices.
function shown(r) {
    const fresh = r.page.segments().filter(s => !r.hit.before.has(s));
    return fresh.concat(['status: ' + r.page.status()], r.page.notices.map(n => 'notice: ' + n.text));
}
const has = (r, text) => shown(r).some(s => s.indexOf(text) >= 0);
// The status line's words, whether the page set them (textContent) or drew
// them in its markup (the apply modal redraws its footer).
function statusWords(page) {
    const host = page.scrim;
    const st = host.querySelector('[data-layer-status]') || host.querySelector('.mf .st');
    return st ? (st.textContent || T.text(st.innerHTML)) : '';
}
const after = r => r.page.calls.slice(r.hit.at + 1);
const checks = {
    noRejection: r => ['no unhandled rejection', !r.rejections.length, r.rejections.map(x => x.text).join('; ')],
    says: text => r => ['the page says ' + JSON.stringify(text), has(r, text), 'on screen: ' + JSON.stringify(shown(r).slice(-6))],
    notSays: text => r => ['the page does not say ' + JSON.stringify(text), !has(r, text), 'it does'],
    noNaN: r => ['no request names /NaN/', !after(r).some(c => /\bNaN\b/.test(c.url)),
        after(r).filter(c => /\bNaN\b/.test(c.url)).map(c => c.method + ' ' + c.url).join(' · ')],
    noWriteAfter: r => ['no write follows it', !after(r).some(c => c.method !== 'GET'),
        after(r).filter(c => c.method !== 'GET').map(c => c.method + ' ' + c.url).join(' · ')],
    signIn: r => ['the notice offers Sign in', r.page.notices.some(n => /^Sign in → \/login\?next=/.test(n.action)),
        'notices: ' + JSON.stringify(r.page.notices)],
    okLive: r => { const ok = okButton(r.page) || r.page.scrim.querySelector('[data-layer-ok]'); return ['the sheet is open and its OK pressable', r.page.sheetOpen() && !!ok && !ok.disabled, sheetLine(r.page)]; },
};
const DOOR_BUGS = [
    {
        title: 'dead Edge, Save style (processes-desktop.js:2415): flow/save gets no answer',
        spec: () => T.DRIVE.flowSave, target: { method: 'POST', url: '/api/processes/1/flow/save', k: 0 }, answer: NO_ANSWER,
        checks: [checks.noRejection, checks.says('No answer from the server')],
    },
    {
        title: 'dead Edge, a sheet\'s write (processes-desktop.js:6049, postJSON): Rename Style B gets no answer',
        spec: () => D.styleRename, target: { method: 'PUT', url: '/api/styles/2', k: 0 }, answer: NO_ANSWER,
        checks: [checks.noRejection, checks.says('No answer from the server'), checks.okLive],
    },
    {
        title: 'lapsed session reads as done (processes-desktop.js:6056; ruling 3): Add process, POST /api/processes lands on the login page',
        spec: () => T.DRIVE.addProcess, target: { method: 'POST', url: '/api/processes', k: 0 }, answer: LAPSED,
        checks: [checks.noNaN, checks.noWriteAfter, checks.says('Signed out'), checks.signIn],
    },
    {
        title: '"Creating…" stuck (processes-desktop.js:6781): Add process, POST /api/processes gets no answer',
        spec: () => T.DRIVE.addProcess, target: { method: 'POST', url: '/api/processes', k: 0 }, answer: NO_ANSWER,
        checks: [checks.noRejection, r => ['the status line is not left on "Creating…"', statusWords(r.page) !== 'Creating…', 'status ' + JSON.stringify(statusWords(r.page))],
            checks.says('No answer from the server')],
    },
    {
        title: 'Apply wedge (processes-desktop.js:7895 + :5917): Save to 1 part gets no answer',
        spec: () => D.applySave, target: { method: 'POST', url: '/api/processes/1/flow/save', k: 0 }, answer: NO_ANSWER,
        checks: [checks.noRejection,
            r => ['the status line is not left on "Saving…"', statusWords(r.page) !== 'Saving…', 'status ' + JSON.stringify(statusWords(r.page))],
            checks.okLive,
            async r => {
                const mark = r.page.mark();
                await pressOK(r.page);
                const again = r.page.since(mark).filter(c => /\/flow\/save$/.test(c.url));
                return ['pressed again, it saves again', again.length > 0, 'pressed again: ' + (r.page.since(mark).map(lineOf).join(' · ') || 'nothing sent')];
            }],
    },
    {
        title: '"Preview OK · null orders" (processes-desktop.js:2400): the flow edit\'s preview answers a 500 with no body',
        spec: () => Object.assign({ previews: true }, T.DRIVE.flowEdit), target: { method: 'POST', url: '/api/processes/1/flow/preview', k: 0 },
        answer: BODILESS_500,
        checks: [checks.notSays('Preview OK · null orders'), checks.says('Server error (500)')],
    },
    {
        title: 'Save settings on a dead Edge (processes-desktop.js:5306): PUT /api/processes/1 gets no answer',
        spec: () => T.DRIVE.settingsSave, target: { method: 'PUT', url: '/api/processes/1', k: 0 }, answer: NO_ANSWER,
        checks: [checks.noRejection, checks.says('No answer from the server')],
    },
    {
        title: 'the presets read (processes-desktop.js:7138): the Recipes tab\'s read lands on the login page',
        spec: () => ({ drive: async page => { page.click('root', { tab: 'presets' }); await page.settle(); } }),
        target: { method: 'GET', url: '/api/processes/1/presets', k: 0 }, answer: LAPSED,
        checks: [checks.says('Signed out'), checks.notSays('Could not reach this edge')],
    },
];
async function pinDoorBugs() {
    T.setCurrent('door-bugs.txt');
    const L = ['# The S2 door bugs on the Processes page (REPORT-v2-stop1-P0 bug table; ruling 3), each on a fresh',
        '# page with the one answer that shows it. "ok" is what must hold; "BUG" is a bug back.'];
    for (const b of DOOR_BUGS) {
        const r = await runOnce(b.spec(), b.target, b.answer);
        if (!r.hit) throw new Error(b.title + ': ' + b.target.method + ' ' + b.target.url + ' was not sent');
        L.push('', '## ' + b.title);
        for (const c of b.checks) {
            const [holds, ok, detail] = await c(r);
            L.push(ok ? 'ok   ' + holds : 'BUG  ' + holds + ': ' + detail);
        }
        for (const c of r.page.calls.slice(r.from)) T.SEEN.push({ site: c.site, stack: c.stack, golden: 'door-bugs.txt' });
        running = null;
    }
    T.out['door-bugs.txt'] = L.join('\n') + '\n';
}

// ── the request site table (`node pins.js --sites`) ─────────────────────
//
// Every line of the page that calls the HTTP door (send, shared/http.js),
// which replaced its 41 bare fetch( sites, then every line that calls
// sheetSubmit, the page's one sheet write over the door.
function sitesTable(seen) {
    const src = fs.readFileSync(PAGE, 'utf8').split('\n');
    const rows = [];
    let fn = '';
    src.forEach((l, i) => {
        const m = /^(?:async\s+)?function\s+(\w+)/.exec(l);
        if (m) fn = m[1];
        if (/\b(fetch|send)\(/.test(l) && !/^\s*\/\//.test(l) && !/^(?:async\s+)?function\s/.test(l)) {
            const n = i + 1;
            const at = seen.filter(s => s.site === n);
            const goldens = [...new Set(at.map(s => s.golden))].sort();
            rows.push('| ' + n + ' | ' + fn + ' | ' + (goldens.length ? goldens.join(', ') : '—') + ' |');
        }
    });
    // sheetSubmit's one send( carries every write that goes through it, so
    // each line that calls it is a site of its own, attributed by the first
    // frame of the stack past it.
    const via = [];
    let fn2 = '';
    const helper = new Set();
    src.forEach((l, i) => { if (/^(?:async\s+)?function\s+sheetSubmit\b/.test(l)) helper.add(i + 1); });
    const inHelper = n => [...helper].some(h => n >= h && n < h + 12);
    src.forEach((l, i) => {
        const m = /^(?:async\s+)?function\s+(\w+)/.exec(l);
        if (m) fn2 = m[1];
        if (/\bsheetSubmit\(/.test(l) && !/^(?:async\s+)?function\s/.test(l) && !/^\s*\/\//.test(l) && !inHelper(i + 1)) {
            const n = i + 1;
            const at = seen.filter(s => (s.stack || []).filter(x => !inHelper(x))[0] === n);
            const goldens = [...new Set(at.map(s => s.golden))].sort();
            via.push('| ' + n + ' | ' + fn2 + ' | sheetSubmit | ' + (goldens.length ? goldens.join(', ') : '—') + ' |');
        }
    });
    return '| line | function | goldens that reach it |\n|---|---|---|\n' + rows.join('\n') + '\n\n' + rows.length + ' send( sites\n\n' +
        '| line | function | through | goldens that reach it |\n|---|---|---|---|\n' + via.join('\n') + '\n\n' +
        via.length + ' sheetSubmit( call sites\n';
}

async function run(t) {
    T = t;
    await pinRequests();
    await pinRefusals();
    T.lastPage = await T.openPage();
    await pinSheetStack();
    await pinDoorBugs();
}

module.exports = { run, sitesTable, noticeLines };
