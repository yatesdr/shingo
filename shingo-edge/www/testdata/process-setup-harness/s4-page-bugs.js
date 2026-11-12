// s4-page-bugs.js — the Processes page's S4 bugs (page-bugs.txt): LEAD-NOTES
// B1/B2, census #1-#4, the P0 flat bugs on this page, U5's F20/F21/F22 and
// ruling 5's chain resume, each on a fresh page with the act that shows it.
//
// A BLOCK IS ITS BUG'S PIN. Each check prints "ok <what must hold>" or
// "BUG <what must hold>: <what the page did>". The pins were written before
// any fix, so the golden's first commit holds the bugs as BUG lines and each
// fix turns its own lines to ok: the golden's history is the red, then the
// green. A BUG line coming back is the bug coming back.

'use strict';

let T = null;
const MSG = 'the Edge refused it';
const REFUSED = { status: 400, body: { error: MSG } };
const FINDING = { core_node_name: 'POS_A1', side: 'to', field: 'inbound_source', severity: 'error', message: 'POS_A1 needs a source' };
const OK = () => ({ status: 'ok' });
const route = table => (method, url) => {
    for (const [m, re, fn] of table) if (m === method && re.test(url.split('?')[0])) return { body: fn(url) };
    return undefined;
};
const withPage = change => Object.assign(JSON.parse(JSON.stringify(T.FX.page)), change(JSON.parse(JSON.stringify(T.FX.page))));
const lineOf = c => c.method + ' ' + c.url;

// refuse answers the n-th call (0-based, counted per run) to method + url
// with `answer`; every other call is the fixture's.
function refuse(method, url, answer, n) {
    let k = 0;
    return (m, u) => {
        if (m !== method || u.split('?')[0] !== url) return undefined;
        return k++ === (n || 0) ? answer : undefined;
    };
}
// every answers each call to method + url (a regexp) with `answer`.
const every = (method, re, answer) => (m, u) => (m === method && re.test(u.split('?')[0]) ? answer : undefined);

async function open(o) {
    const page = await T.openPage({ hash: o.hash, page: o.page, search: o.search, extra: o.extra, answer: o.answer });
    await page.settle();
    return page;
}
function okButton(page) { return page.scrim.querySelector('[data-act="sheet-ok"]') || page.scrim.querySelector('[data-layer-ok]'); }
async function pressOK(page) {
    const ok = okButton(page);
    if (!ok || ok.disabled) return false;
    page.click('scrim', { act: 'sheet-ok' });
    await page.settle();
    return true;
}
function statusText(page) {
    const st = page.scrim.querySelector('[data-layer-status]') || page.scrim.querySelector('.mf .st');
    return st ? (st.textContent || T.text(st.innerHTML)) : '';
}
const onScreen = (page, text) => page.segments().some(s => s.indexOf(text) >= 0) || statusText(page).indexOf(text) >= 0 ||
    page.notices.some(n => n.text.indexOf(text) >= 0);
const writes = calls => calls.filter(c => c.method !== 'GET');

// The style row's ⋯ and one of its items, as the menu drew it.
const styleMenu = (style, item, typed) => async page => {
    page.click('root', { act: 'style-menu', style: String(style) });
    const pop = page.byId('pd-pop');
    const b = pop && pop.querySelectorAll('[data-act]').find(x => x.dataset.act === 'style-' + item);
    if (!b) throw new Error('the style menu draws no ' + item);
    page.click('root', Object.assign({}, b.dataset));
    await page.settle();
    for (const [f, v] of Object.entries(typed || {})) page.type(f, v);
    await pressOK(page);
};
// The apply modal on Style B, ticked and every "which part?" answered with
// its first part (p0-page.js applyTick).
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
const COUNTER = { id: 51, label: 'Station 1', tag_name: 'S1.Count', tag_name_2: '', last_poll_at: '', ratio: null, strokes_since_click: 0, stopped: false };
const COUNTER_ROUTES = route([
    ['GET', /^\/api\/processes\/1\/station-counters$/, () => [COUNTER]],
    ['GET', /^\/api\/process-nodes\/\d+\/countable$/, () => ({ countable: true })],
    ['POST', /^\/api\/processes\/1\/station-counters$/, () => ({ id: 52 })],
    ['PUT', /^\/api\/process-nodes\/\d+\/counted-at$/, OK],
]);
// A Core node list in Core's map order, with a group, a typed node and two
// nodes with no type (Core sends '' for a plain node).
const UNSORTED_NODES = [
    { name: 'Z_PLAIN', node_type: '' },
    { name: 'DST_7', node_type: 'storage' },
    { name: 'GRP_Q', node_type: 'NGRP', members: ['DST_7', 'A_PLAIN'] },
    { name: 'A_PLAIN', node_type: '' },
    { name: 'LANE_7', node_type: 'LANE' },
];

// ── the blocks ───────────────────────────────────────────────────────────────
//
// A block: {title, open (the page), act(page) -> run facts, checks: [fn(run)
// -> [holds, ok, detail]]}. `run` is {page, from, ...} plus what act returns.
const BLOCKS = [];
const block = b => BLOCKS.push(b);

// B1 — a new style that resolves to an existing one (mint answers minted:
// false), with a CATID typed: the CATID's PUT is that style as it stands.
block({
    title: 'B1: + New style resolving to Style A, with a CATID, keeps Style A\'s description (LEAD-NOTES B1)',
    open: () => ({
        page: withPage(p => ({ styles: p.styles.map(s => (s.id === 1 ? Object.assign(s, { description: 'front press part' }) : s)) })),
        extra: route([['POST', /\/flow\/mint$/, () => ({ style_id: 1, style_name: 'Style A', minted: false })]]),
    }),
    act: async page => { await T.DRIVE.mint.drive(page); },
    checks: [r => {
        const put = r.calls.find(c => c.method === 'PUT' && c.url === '/api/styles/1');
        return ['the CATID write sends Style A\'s description', !!put && put.body.description === 'front press part',
            put ? 'description ' + JSON.stringify(put.body.description) : 'no PUT /api/styles/1'];
    }],
});

// B2 — the style menu reaches a style the page's /styles list does not hold
// (its last read failed; the composer's did not): the write starts from the
// style as the Edge stores it, or is not sent.
const STYLES_WITH_B = () => T.FX.page.styles.filter(s => s.process_id === 1)
    .map(s => Object.assign({}, s, s.id === 2 ? { description: 'back press part' } : {}));
block({
    title: 'B2: Rename Style B when the page\'s style list lacks it keeps its description (LEAD-NOTES B2)',
    open: () => ({
        page: withPage(p => ({ styles: p.styles.filter(s => s.id !== 2) })),
        extra: route([['GET', /^\/api\/processes\/1\/styles$/, STYLES_WITH_B]]),
    }),
    act: styleMenu(2, 'rename', { name: 'Style B2' }),
    checks: [r => {
        const put = r.calls.find(c => c.method === 'PUT' && c.url === '/api/styles/2');
        return ['the rename sends Style B\'s stored description', !!put && put.body.description === 'back press part' && put.body.name === 'Style B2',
            put ? 'body ' + JSON.stringify(put.body) : 'no PUT /api/styles/2'];
    }],
});
block({
    title: 'B2: Rename Style B when its stored row cannot be read writes nothing (LEAD-NOTES B2)',
    open: () => ({
        page: withPage(p => ({ styles: p.styles.filter(s => s.id !== 2) })),
        answer: every('GET', /^\/api\/processes\/1\/styles$/, { status: 500, raw: '' }),
    }),
    act: styleMenu(2, 'rename', { name: 'Style B2' }),
    checks: [
        r => ['no PUT /api/styles/2 is sent', !r.calls.some(c => c.method === 'PUT' && c.url === '/api/styles/2'),
            r.calls.filter(c => c.method === 'PUT').map(c => lineOf(c) + ' ' + JSON.stringify(c.body)).join(' · ')],
        r => ['the sheet stays open and says why', r.page.sheetOpen() && !!statusText(r.page), 'status ' + JSON.stringify(statusText(r.page))],
    ],
});

// F20 — Settings' Part identity: Override is the segment that lights.
block({
    title: 'F20: Part identity, Override pressed, lights Override (FINDINGS F20)',
    open: () => ({ hash: '#tab=settings' }),
    act: async page => { page.click('root', { act: 'st-ident', mode: 'override' }); await page.settle(); },
    checks: [r => {
        const seg = r.page.root.querySelectorAll('[data-act="st-ident"]').filter(b => /\bon\b/.test(b.className)).map(b => b.dataset.mode);
        return ['Override is lit and Derive is not', seg.length === 1 && seg[0] === 'override', 'lit: ' + JSON.stringify(seg)];
    }],
});

// Smoke (browser walk 2026-11-11): the list said "not wired" for a counter
// with its PLC and tag set and switched off, where Setup and Settings say
// "off"; and a screen Add process named after its process was said twice.
block({
    title: 'smoke: a wired counter switched off reads "off" on the list, not "not wired"',
    open: () => ({ search: '', page: withPage(p => ({ processes: p.processes.map(x => x.id === 1 ? Object.assign({}, x, { counter_enabled: false }) : x) })) }),
    act: async () => {},
    checks: [r => {
        const row = String(r.page.root.innerHTML).split('<tr').find(x => x.indexOf('<b>Press A</b>') >= 0) || '';
        const cell = (/<span class="pd-cnt[^"]*"><i><\/i>([^<]*)<\/span>/.exec(row) || ['', ''])[1];
        return ['Press A\'s counter cell: ' + JSON.stringify(cell), cell === 'off · PLC_A', 'said ' + JSON.stringify(cell)];
    }],
});
block({
    title: 'smoke: a screen named after its process is not said twice in the title',
    open: () => ({ page: withPage(p => ({ stations: p.stations.map(s => s.id === 11 ? Object.assign({}, s, { name: 'Press A' }) : s) })) }),
    act: async () => {},
    checks: [r => {
        const title = (/<div class="pd-crumb">[\s\S]*?<b>([^<]*)<\/b>/.exec(String(r.page.root.innerHTML)) || ['', ''])[1];
        return ['the title: ' + JSON.stringify(title), title === 'Press A', 'said ' + JSON.stringify(title)];
    }],
});
block({
    title: 'smoke: Settings\' Parts row is on the settings grid, as every other row is',
    open: () => ({ hash: '#tab=settings' }),
    act: async () => {},
    checks: [r => {
        const html = String(r.page.root.innerHTML);
        const row = (/<div class="(pd-s?fld)"><label>Parts<small>/.exec(html) || ['', 'missing'])[1];
        return ['the Parts row is a .pd-sfld', row === 'pd-sfld', 'drawn as .' + row + ' (the sheets\' 230 px grid)'];
    }],
});

// Smoke (browser walk 2026-11-11): Add process drew "Show on map" only after a
// process had been opened in the same page load, from that process's composer
// read the list still held. On a fresh list it had no map at all.
const sheetToggle = page => !!page.scrim.querySelector('[data-act="add-showmap"]') ||
    /data-act="add-showmap"/.test(String((page.byId('pd-addmaptoggle') || {}).innerHTML || ''));
block({
    title: 'smoke: Add process on a fresh list reads the Edge\'s plant map as it opens',
    open: () => ({ search: '' }),
    act: async page => { page.click('root', { act: 'add-process' }); await page.settle(); },
    checks: [
        r => ['one composer read for the map', r.calls.filter(c => c.method === 'GET' && /\/composer$/.test(c.url.split('?')[0])).length === 1,
            r.calls.map(lineOf).join(' · ') || 'no request'],
        r => ['Show on map is on the routing set\'s heading', sheetToggle(r.page), 'no toggle: the list holds no map'],
    ],
});
block({
    title: 'smoke: a process opened before lends Add process neither its map nor its zoom',
    open: () => ({ answer: refuse('GET', '/api/processes/1/composer', { status: 500, body: { error: 'no' } }, 1) }),
    act: async page => {
        page.S.mapZoom = 2;
        page.click('root', { act: 'list' });
        await page.settle();
        page.click('root', { act: 'add-process' });
        await page.settle();
        return { zoom: page.S.mapZoom };
    },
    checks: [
        r => ['the map read refused: no Show on map', !sheetToggle(r.page), 'drawn from the last process\'s composer'],
        r => ['the zoom is the sheet\'s own', r.zoom === 1, 'zoom ' + r.zoom + ' carried over'],
    ],
});

// F21 — the address bar's #tab= edited in place (a hashchange, no load).
block({
    title: 'F21: #tab= edited in place switches the tab (FINDINGS F21)',
    open: () => ({}),
    act: async page => {
        const seen = [];
        for (const h of ['#tab=settings', '#tab=screens', '#tab=recipes', '#style=2']) {
            page.ctx.location.hash = h;
            page.fireWindow('hashchange');
            await page.settle();
            seen.push(h + ' -> ' + page.S.tab + (page.S.tab === 'flows' ? ' style ' + page.S.styleID : ''));
        }
        return { seen: seen };
    },
    checks: [r => ['each hash lands on its tab', r.seen.join(' | ') === '#tab=settings -> settings | #tab=screens -> screens | #tab=recipes -> settings | #style=2 -> flows style 2',
        r.seen.join(' | ')]],
});

// Census #1 — Core's node list cannot be read, and Quality hold's
// containment destination is opened.
block({
    title: 'census #1: Containment destination on a failed node-list read says so',
    open: () => ({ hash: '#tab=settings', answer: every('GET', /^\/api\/core-nodes$/, { status: 500, raw: '' }) }),
    act: async page => { page.click('root', { act: 'st-holddest' }); await page.settle(); },
    checks: [
        r => {
            const rows = T.holdRows(r.page).map(b => b.getAttribute('data-value'));
            return ['the list does not open on "-- None --" alone', !(rows.length === 1 && rows[0] === ''), 'rows ' + JSON.stringify(rows)];
        },
        r => ['Settings says the node list was not read', /node list/i.test(r.page.S.settingsError || ''),
            'settingsError ' + JSON.stringify(r.page.S.settingsError)],
    ],
});

// Census #2 and #4 — the order Core's nodes are offered in.
block({
    title: 'census #2: Containment destination lists Core\'s nodes in the pickers\' order',
    open: () => ({ hash: '#tab=settings', extra: route([['GET', /^\/api\/core-nodes$/, () => UNSORTED_NODES]]) }),
    act: async page => { page.click('root', { act: 'st-holddest' }); await page.settle(); },
    checks: [r => {
        // The pickers' order is the node adapter's (nodeRows): a group first
        // in its run with its nodes under it, names in code-unit order.
        const rows = T.holdRows(r.page).map(b => b.getAttribute('data-value')).filter(Boolean);
        const flat = r.page.pd('nodeRows')(r.page.pd('NODES').rows(), {})
            .reduce((a, x) => a.concat([x.value], (x.members || []).map(m => m.value)), []);
        const want = flat.filter(n => rows.indexOf(n) >= 0);
        return ['the rows follow the node adapter\'s', rows.length > 1 && JSON.stringify(rows) === JSON.stringify(want),
            'rows ' + JSON.stringify(rows) + ', list ' + JSON.stringify(want)];
    }],
});
block({
    title: 'census #4: Core\'s nodes are offered groups first, nodes with no type last',
    open: () => ({ hash: '#tab=settings', extra: route([['GET', /^\/api\/core-nodes$/, () => UNSORTED_NODES]]) }),
    act: async () => {},
    checks: [r => {
        // The node adapter's rows (nodeRows), every picker's order: groups
        // first, each with its nodes under it, a plain node after them.
        const rows = r.page.pd('nodeRows')(r.page.pd('NODES').rows(), {});
        const tops = rows.map(x => x.value);
        const under = (rows.find(x => x.value === 'GRP_Q') || {}).members || [];
        return ['GRP_Q first with A_PLAIN and DST_7 under it, Z_PLAIN last',
            tops[0] === 'GRP_Q' && tops[tops.length - 1] === 'Z_PLAIN' && under.map(m => m.value).join() === 'A_PLAIN,DST_7',
            'order ' + JSON.stringify(tops) + ', under GRP_Q ' + JSON.stringify(under.map(m => m.value))];
    }],
});

// Census #3 — a routing name added in Settings, then the Styles tab.
block({
    title: 'census #3: a source added in Settings is offered on the Styles tab',
    open: () => {
        let added = false;
        const rows = () => T.FX.server.routingNodes.concat(added
            ? [{ id: 120, core_node_name: 'SRC_2', role: 'source', sequence: 3, enabled: true, origin: 'engineer', label: 'SRC_2' }] : []);
        return {
            hash: '#tab=settings',
            extra: (m, u) => {
                const p = u.split('?')[0];
                if (m === 'POST' && p === '/api/processes/1/routing-nodes') { added = true; return { body: { id: 120 } }; }
                if (m === 'GET' && p === '/api/processes/1/routing-nodes') return { body: { rows: rows() } };
                return undefined;
            },
        };
    },
    act: async page => {
        T.setPick(page, 'root', 'stset', 'source', 'SRC_2', 'add');
        await page.settle();
        page.click('root', { tab: 'flows' });
        await page.settle();
    },
    checks: [r => {
        const offered = r.page.pd('optionsFor')('POS_A5', 'source').map(o => o.value);
        return ['POS_A5\'s Inbound source offers SRC_2', offered.indexOf('SRC_2') >= 0, 'offers ' + JSON.stringify(offered)];
    }],
});

// The P0 flat bugs on this page (REPORT-v2-stop1-P0, "S4 flat bugs").
block({
    title: 'P0 :3746: a refused routing-set add is drawn, not cleared by the re-read',
    open: () => ({ hash: '#tab=settings', answer: refuse('POST', '/api/processes/1/routing-nodes', REFUSED) }),
    act: async page => { T.setPick(page, 'root', 'stset', 'source', 'SRC_2', 'add'); await page.settle(); },
    checks: [r => ['the page says ' + JSON.stringify(MSG), onScreen(r.page, MSG), 'routingError ' + JSON.stringify(r.page.S.routingError)]],
});
block({
    title: 'P0 :3626: a refused Turn all on is drawn, not cleared by the re-read',
    open: () => ({ hash: '#tab=settings', answer: refuse('PATCH', '/api/processes/1/routing-nodes/106', REFUSED) }),
    act: async page => { page.click('root', { act: 'rs-all-on', role: 'staging' }); await page.settle(); },
    checks: [r => ['the page says ' + JSON.stringify(MSG), onScreen(r.page, MSG), 'routingError ' + JSON.stringify(r.page.S.routingError)]],
});
block({
    title: 'P0 :5368: a refused Save settings keeps the typed draft',
    open: () => ({ hash: '#tab=settings', answer: refuse('PUT', '/api/processes/1', REFUSED) }),
    act: async page => { await T.DRIVE.settingsSave.drive(page); },
    checks: [
        r => ['the page says ' + JSON.stringify(MSG), onScreen(r.page, MSG), 'settingsError ' + JSON.stringify(r.page.S.settingsError)],
        r => ['the typed name and description stay', r.page.S.settings.name === 'Press A2' && r.page.S.settings.description === 'renamed',
            'draft ' + JSON.stringify({ name: r.page.S.settings.name, description: r.page.S.settings.description })],
    ],
});
block({
    title: 'P0 :7084: a refused catalog sync says the server\'s sentence (fixed by the S2 door; kept as a pin)',
    open: () => ({ hash: '#tab=settings', answer: refuse('POST', '/api/payload-catalog/sync', REFUSED) }),
    act: async page => { page.click('root', { act: 'st-sync' }); await page.settle(); },
    checks: [r => ['the page says ' + JSON.stringify(MSG), onScreen(r.page, MSG), 'settingsError ' + JSON.stringify(r.page.S.settingsError)]],
});
block({
    title: 'P0 :6467: a mint refused 422 with findings names them',
    open: () => ({ answer: refuse('POST', '/api/processes/1/flow/mint', { status: 422, body: { error: MSG, findings: [FINDING] } }) }),
    act: async page => { await T.DRIVE.mint.drive(page); },
    checks: [r => ['the sheet names "POS_A1 needs a source"', statusText(r.page).indexOf('POS_A1 needs a source') >= 0,
        'status ' + JSON.stringify(statusText(r.page))]],
});
block({
    title: 'P0 :4169: Apply on an untouched Advanced sheet changes nothing',
    open: () => ({}),
    act: async page => {
        const before = JSON.stringify(page.pd('M')().toCells(page.S.model));
        page.click('root', { act: 'advanced', node: 'POS_A1' });
        await page.settle();
        const from = page.mark();
        page.click('scrim', { act: 'adv-apply' });
        await page.settle();
        return { before: before, after: JSON.stringify(page.pd('M')().toCells(page.S.model)), sent: page.since(from) };
    },
    checks: [
        r => ['the flow is as it was', r.before === r.after, 'cells moved'],
        r => ['the flow is not dirty', !r.page.pd('dirty')(), 'dirty'],
        r => ['nothing is sent', !r.sent.length, r.sent.map(lineOf).join(' · ')],
    ],
});
block({
    title: 'P0 desktop-bodies.js:431: an apply save refused 409 that is not stale is not retried',
    open: () => ({ hash: '#tab=presets', answer: refuse('POST', '/api/processes/1/flow/save', { status: 409, body: { error: MSG } }) }),
    act: async page => {
        await applyTick(page);
        const from = page.mark();
        await pressOK(page);
        return { sent: page.since(from) };
    },
    checks: [r => {
        const saves = r.sent.filter(c => /\/flow\/save$/.test(c.url));
        return ['one save, no preview-and-retry', saves.length === 1, r.sent.map(lineOf).join(' · ')];
    }],
});

// Ruling 5 — a refusal midway through a chain; the next press resumes at the
// step that failed and makes nothing twice.
const resume = (title, o) => block({
    title: title,
    open: () => o.open,
    act: async page => {
        await o.drive(page);
        const first = statusText(page);
        const from = page.mark();
        const pressed = await pressOK(page);
        return { first: first, pressed: pressed, again: page.since(from) };
    },
    checks: [
        r => ['the refusal is said', r.first.indexOf(MSG) >= 0, 'status ' + JSON.stringify(r.first)],
        r => ['pressed again, nothing is made twice', r.pressed && !writes(r.again).some(c => o.once.test(lineOf(c))),
            r.pressed ? 'pressed again: ' + (writes(r.again).map(lineOf).join(' · ') || 'no write') : 'OK not pressable'],
        r => ['pressed again, the failed step is sent', writes(r.again).some(c => o.step.test(lineOf(c))),
            'pressed again: ' + (writes(r.again).map(lineOf).join(' · ') || 'no write')],
    ],
});
resume('ruling 5: Add process, its positions refused, then pressed again', {
    open: { answer: refuse('PUT', '/api/operator-stations/31/claimed-nodes', REFUSED) },
    drive: page => T.DRIVE.addProcess.drive(page),
    once: /^POST \/api\/(processes|operator-stations)$/, step: /^PUT \/api\/operator-stations\/31\/claimed-nodes$/,
});
resume('ruling 5: Counted at → New station counter, its binding refused, then pressed again', {
    open: { hash: '#tab=settings', extra: COUNTER_ROUTES, answer: refuse('PUT', '/api/process-nodes/202/counted-at', REFUSED) },
    drive: async page => {
        // Settings keeps a position's controls behind its "Change…" row
        // (33-settings-pages.md rule 7): open it first.
        const findSel = () => page.root.querySelectorAll('select[data-countedat]').find(el => el.dataset.countedat === '202');
        if (!findSel()) {
            const more = page.root.querySelectorAll('[data-act="st-more"]').find(el => el.dataset.more === 'pos-202');
            if (more) { page.click('root', Object.assign({}, more.dataset)); await page.settle(); }
        }
        const sel = findSel();
        if (!sel) throw new Error('Settings draws no Counted at for 202');
        sel.value = 'new';
        page.fire(sel, 'change');
        await page.settle();
        page.byId('sc-label').value = 'Station 3';
        page.byId('sc-tag').value = 'S3.Count';
        await pressOK(page);
    },
    once: /^POST \/api\/processes\/1\/station-counters$/, step: /^PUT \/api\/process-nodes\/202\/counted-at$/,
});
resume('ruling 5: + New style from Recipe One, its CATID refused, then pressed again', {
    open: { answer: refuse('PUT', '/api/styles/9', REFUSED) },
    drive: page => T.DRIVE.mint.drive(page),
    once: /^POST \/api\/processes\/1\/flow\/mint$/, step: /^PUT \/api\/styles\/9$/,
});
resume('ruling 5 (same chain class): Add operator screen, its positions refused, then pressed again', {
    open: { answer: refuse('PUT', '/api/operator-stations/31/claimed-nodes', REFUSED) },
    drive: page => T.DRIVE.screenAdd.drive(page),
    once: /^POST \/api\/operator-stations$/, step: /^PUT \/api\/operator-stations\/31\/claimed-nodes$/,
});
block({
    title: 'ruling 5: Add process half made, then Cancel: what was made is read back',
    open: () => ({ answer: refuse('PUT', '/api/operator-stations/31/claimed-nodes', REFUSED) }),
    act: async page => {
        await T.DRIVE.addProcess.drive(page);
        const from = page.mark();
        page.click('scrim', { act: 'sheet-cancel' });
        await page.settle();
        return { after: page.since(from) };
    },
    checks: [
        r => ['nothing is deleted', !r.after.some(c => c.method === 'DELETE'), r.after.map(lineOf).join(' · ')],
        r => ['the process list is read again', r.after.some(c => lineOf(c) === 'GET /api/processes'),
            'after Cancel: ' + (r.after.map(lineOf).join(' · ') || 'nothing')],
    ],
});

// ── F22: no refused write leaves its button silent ──────────────────────────
//
// Read off the refusal goldens this run wrote (p0-page.js): every block of a
// refused write shows new text, a notice or a status. The one exception is a
// stale 409 on Save style, which re-previews and redraws the bar.
function silentRefusals() {
    const out = [];
    for (const name of Object.keys(T.out).filter(n => /^refusals-/.test(n)).sort()) {
        let head = '', said = true, act = '';
        const flush = () => { if (head && !said && !/stale:true/.test(head)) out.push(name + ' ' + act + ': ' + head.replace(/^### /, '')); };
        for (const l of T.out[name].split('\n')) {
            if (/^## /.test(l)) { flush(); head = ''; act = l.slice(3); continue; }
            if (/^### /.test(l)) { flush(); head = l; said = false; continue; }
            if (/new text: (?!none)|notice: |status "/.test(l)) said = true;
        }
        flush();
    }
    return out;
}

async function run(t) {
    T = t;
    T.setCurrent('page-bugs.txt');
    const L = ['# The Processes page\'s S4 bugs, each on a fresh page with the act that shows it. "ok" is what must',
        '# hold; "BUG" is the bug as the page has it (see s4-page-bugs.js).'];
    for (const b of BLOCKS) {
        const o = b.open();
        const page = await open(o);
        const from = page.mark();
        const facts = (await b.act(page)) || {};
        await page.settle();
        const r = Object.assign({ page: page, from: from, calls: page.since(from) }, facts);
        L.push('', '## ' + b.title);
        for (const c of b.checks) {
            const [holds, ok, detail] = c(r);
            L.push(ok ? 'ok   ' + holds : 'BUG  ' + holds + ': ' + detail);
        }
        for (const c of page.calls) T.SEEN.push({ site: c.site, stack: c.stack, golden: 'page-bugs.txt' });
    }
    const silent = silentRefusals();
    L.push('', '## F22: every refused write says something (the refusal goldens; a stale 409 re-previews)');
    L.push(!silent.length ? 'ok   no refusal block leaves the page silent'
        : 'BUG  no refusal block leaves the page silent: ' + silent.join(' | '));
    T.out['page-bugs.txt'] = L.join('\n') + '\n';
}

module.exports = { run };
