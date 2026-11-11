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
// THE PLANT IS U0's (fixture.json); the routes an act needs that the base
// plant does not answer (a health strip, a station counter) are added per act
// here, synthetic like the rest.

'use strict';

const fs = require('fs');
const path = require('path');

let T = null;               // pins.js's helpers: see run()
const MSG = 'the Edge refused it';
const PAGE = path.join(__dirname, '..', '..', 'static', 'js', 'pages', 'processes-desktop.js');

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
function okButton(page) { return page.scrim.querySelector('[data-act="sheet-ok"]'); }
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
    const st = page.scrim.querySelector('.mf .st');
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

// ── the fetch( site table (`node pins.js --sites`) ───────────────────────
function sitesTable(seen) {
    const src = fs.readFileSync(PAGE, 'utf8').split('\n');
    const rows = [];
    let fn = '';
    src.forEach((l, i) => {
        const m = /^(?:async\s+)?function\s+(\w+)/.exec(l);
        if (m) fn = m[1];
        if (/\bfetch\(/.test(l) && !/^\s*\/\//.test(l)) {
            const n = i + 1;
            const at = seen.filter(s => s.site === n);
            const goldens = [...new Set(at.map(s => s.golden))].sort();
            rows.push('| ' + n + ' | ' + fn + ' | ' + (goldens.length ? goldens.join(', ') : '—') + ' |');
        }
    });
    // postJSON's one fetch( carries every write that goes through it, and
    // sheetSubmit is postJSON again: so each line that calls either is a site
    // of its own, attributed by the first frame of the stack past the two.
    const via = [];
    let fn2 = '';
    const helper = new Set();
    src.forEach((l, i) => { if (/^(?:async\s+)?function\s+(postJSON|sheetSubmit)\b/.test(l)) helper.add(i + 1); });
    const inHelper = n => [...helper].some(h => n >= h && n < h + 12);
    src.forEach((l, i) => {
        const m = /^(?:async\s+)?function\s+(\w+)/.exec(l);
        if (m) fn2 = m[1];
        if (/\b(postJSON|sheetSubmit)\(/.test(l) && !/^(?:async\s+)?function\s/.test(l) && !/^\s*\/\//.test(l) && !inHelper(i + 1)) {
            const n = i + 1;
            const at = seen.filter(s => (s.stack || []).filter(x => !inHelper(x))[0] === n);
            const goldens = [...new Set(at.map(s => s.golden))].sort();
            via.push('| ' + n + ' | ' + fn2 + ' | ' + /\b(postJSON|sheetSubmit)\(/.exec(l)[1] + ' | ' + (goldens.length ? goldens.join(', ') : '—') + ' |');
        }
    });
    return '| line | function | goldens that reach it |\n|---|---|---|\n' + rows.join('\n') + '\n\n' + rows.length + ' fetch( sites\n\n' +
        '| line | function | through | goldens that reach it |\n|---|---|---|---|\n' + via.join('\n') + '\n\n' +
        via.length + ' postJSON( / sheetSubmit( call sites\n';
}

async function run(t) {
    T = t;
    await pinRequests();
}

module.exports = { run, sitesTable };
