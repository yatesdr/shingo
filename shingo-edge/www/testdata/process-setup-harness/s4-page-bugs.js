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
    // The dry run, one read for the whole process: every position countable.
    ['GET', /^\/api\/processes\/1\/countable$/, () => Object.fromEntries(T.FX.server.processNodes
        .filter(n => n.process_id === 1).map(n => [n.id, { countable: true, refusal: '' }]))],
    ['POST', /^\/api\/processes\/1\/station-counters$/, () => ({ id: 52 })],
    ['PUT', /^\/api\/processes\/1\/counted-at$/, OK],
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
// THE PART SET IS SAID ONCE (round 5, owner 2026-11-15): under the Parts
// heading, with no field label saying Parts again (which, drawn as a sheet's
// .pd-fld, was also the wrong grid; slice 4).
block({
    title: 'smoke: Settings\' part set sits under its Parts heading, not labelled Parts a second time',
    open: () => ({ hash: '#tab=settings' }),
    act: async () => {},
    checks: [r => {
        const html = String(r.page.root.innerHTML);
        const labelled = /<label>Parts<small>/.test(html);
        const box = /<h2>Parts<\/h2>[\s\S]*?<div class="pd-partsbox">/.test(html);
        return ['the part set is under the heading, with no second Parts label', box && !labelled,
            (labelled ? 'a field label still says Parts' : '') + (box ? '' : ' no .pd-partsbox under the heading')];
    }],
});

// The Add process sheet's map went with the sheet's routing set (slice 4):
// Add asks a name and a group, and the routing set is Settings'. The map
// itself was removed on main too (2026-11-14, the shared-map brief's tier A1),
// and with it the composer read the sheet made to draw it: the sheet makes no
// composer read, and draws no Show on map.
const sheetToggle = page => !!page.scrim.querySelector('[data-act="add-showmap"]') ||
    /Show on map/.test(String((page.scrim.querySelector('.layer-body') || {}).innerHTML || ''));
block({
    title: 'smoke: Add process on a fresh list reads no plant map as it opens (A1)',
    open: () => ({ search: '' }),
    act: async page => { page.click('root', { act: 'add-process' }); await page.settle(); },
    checks: [
        r => ['no composer read', r.calls.filter(c => c.method === 'GET' && /\/composer$/.test(c.url.split('?')[0])).length === 0,
            r.calls.map(lineOf).join(' · ') || 'no request'],
        r => ['no Show on map on the sheet', !sheetToggle(r.page), 'a map toggle is drawn'],
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
    checks: [r => ['each hash lands on its tab', r.seen.join(' | ') === '#tab=settings -> settings | #tab=screens -> settings | #tab=recipes -> settings | #style=2 -> flows style 2',
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

// The set pick on Settings sits in #pd-root: a click on a box's title
// bubbles through the page's onClick, which opens a process from any
// [data-open] it finds. The title carried one once, and the click opened
// process NaN.
block({
    title: 'set pick: a click on a box of Settings\' set pick opens that box and navigates nowhere',
    open: () => ({ hash: '#tab=settings' }),
    act: async page => {
        const host = page.byId('sp-st');
        const box = host.querySelectorAll('.setpick-box').find(b => b.getAttribute('data-box') === 'destination');
        box.querySelector('.setpick-t').click();
        await page.settle();
    },
    checks: [
        r => ['the box opens', !!r.page.S.setPicks.st && r.page.S.setPicks.st.api.active() === 'destination',
            'open ' + (r.page.S.setPicks.st ? r.page.S.setPicks.st.api.active() : 'no set pick')],
        r => ['no request names a process NaN', !r.calls.some(c => /NaN/.test(c.url)), r.calls.map(lineOf).join(' · ')],
        r => ['the page stays on Press A', r.page.S.processID === 1, 'processID ' + r.page.S.processID],
    ],
});

// Census #3 — a routing name added in Settings, then the Styles tab. The save
// reads the composer back, whose routing is what the Styles pickers offer.
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
                if (m === 'PUT' && p === '/api/processes/1/routing-nodes') { added = true; return { body: { status: 'ok', warnings: [] } }; }
                if (m === 'GET' && p === '/api/processes/1/routing-nodes') return { body: { rows: rows() } };
                if (m === 'GET' && p === '/api/processes/1/composer' && added) {
                    const c = JSON.parse(JSON.stringify(T.FX.composer));
                    c.routing = (c.routing || []).concat([{ core_node_name: 'SRC_2', role: 'source', sequence: 3 }]);
                    return { body: c };
                }
                return undefined;
            },
        };
    },
    act: async page => {
        T.setPick(page, 'st', 'source', 'SRC_2');
        await T.stSave(page);
        page.click('root', { tab: 'flows' });
        await page.settle();
    },
    checks: [r => {
        const offered = r.page.pd('optionsFor')('POS_A5', 'source').map(o => o.value);
        return ['POS_A5\'s Inbound source offers SRC_2', offered.indexOf('SRC_2') >= 0, 'offers ' + JSON.stringify(offered)];
    }],
});

// Settings' set pick's box (the routing set's roles are its boxes now).
const setPickValue = (page, box) => (page.S.setPicks.st ? page.S.setPicks.st.api.value(box) : []);

// The P0 flat bugs on this page (REPORT-v2-stop1-P0, "S4 flat bugs").
// A refused routing-set save is said on the bar, and its picks stay picked
// (P0 :3746 and :3626, the add and Turn all on, on the one Save now).
const barSays = r => {
    const said = (r.page.S.stShell && r.page.S.stShell.page.state.refusal) || '';
    return ['the bar says ' + JSON.stringify(MSG), said.toLowerCase().indexOf(MSG.toLowerCase()) >= 0, 'refusal ' + JSON.stringify(said)];
};
block({
    title: 'P0 :3746: a refused routing-set save is said, and the name picked stays',
    open: () => ({ hash: '#tab=settings', answer: refuse('PUT', '/api/processes/1/routing-nodes', REFUSED) }),
    act: async page => { T.setPick(page, 'st', 'source', 'SRC_2'); await T.stSave(page); },
    checks: [barSays,
        r => ['SRC_2 is still picked', setPickValue(r.page, 'source').indexOf('SRC_2') >= 0,
            'Inbound ' + JSON.stringify(setPickValue(r.page, 'source'))]],
});
block({
    title: 'P0 :3626: a refused Turn all on is said, and the names stay picked',
    open: () => ({ hash: '#tab=settings', answer: refuse('PUT', '/api/processes/1/routing-nodes', REFUSED) }),
    act: async page => { page.click('root', { act: 'rs-all-on', role: 'staging' }); await page.settle(); await T.stSave(page); },
    checks: [barSays,
        r => {
            const off = (r.page.S.routing || []).filter(x => x.role === 'staging' && !x.enabled).map(x => x.core_node_name);
            const sel = setPickValue(r.page, 'staging');
            return ['the switched-off names stay picked', off.length > 0 && off.every(n => sel.indexOf(n) >= 0),
                'off ' + JSON.stringify(off) + ', Staging ' + JSON.stringify(sel)];
        }],
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
// The row's Advanced sheet (round 5) keeps the rule the list kept: a setting
// switched on and off again is no change (advancedAction).
block({
    title: "P0 :4169: a setting switched on and off again in a position's Advanced sheet changes nothing",
    open: () => ({}),
    act: async page => {
        const before = JSON.stringify(page.pd('M')().toCells(page.S.model));
        page.click('root', { act: 'adv-open', node: 'POS_A1' });
        await page.settle();
        page.click('scrim', { act: 'set-switch', key: 'auto_confirm', node: 'POS_A1', v: '1' }, { inside: ['.pd-modal'] });
        await page.settle();
        const from = page.mark();
        page.click('scrim', { act: 'set-switch', key: 'auto_confirm', node: 'POS_A1', v: '0' }, { inside: ['.pd-modal'] });
        await page.settle();
        return { before: before, after: JSON.stringify(page.pd('M')().toCells(page.S.model)), sent: page.since(from).filter(c => !/flow\/preview/.test(c.url)) };
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

// Owner 2026-11-12: parts picked in Setup's Change… reached the Styles tab's
// part popover as the page first read them — "No parts yet" over Core's
// catalog. The write is any refreshProcess off the Styles tab; the composer
// read after the first one names one more part.
block({
    title: 'owner 2026-11-12: a part added off the Styles tab is in the Styles part popover, the catalog behind its button',
    open: () => {
        let k = 0;
        return {
            hash: '#tab=settings', answer: (m, u) => {
                if (m !== 'GET' || u.split('?')[0] !== '/api/processes/1/composer' || k++ === 0) return undefined;
                const c = JSON.parse(JSON.stringify(T.FX.composer));
                c.palette = c.palette.concat(['PART-NEW']).sort();
                return { body: c };
            },
        };
    },
    act: async page => {
        await page.pd('refreshProcess')();
        await page.settle();
        page.pd('showTab')('flows');
        await page.settle();
        page.click('root', { act: 'pick', node: 'POS_A1', kind: 'part' });
        await page.settle();
        const rows = page.rows('pd-partlist', 'data-pp');
        return {
            offered: rows.filter(x => x.dataset.pp === 'place').map(x => x.dataset.code),
            catalog: rows.filter(x => x.dataset.pp === 'add').length,
            button: rows.some(x => x.dataset.pp === 'catalog'),
        };
    },
    checks: [
        r => ['PART-NEW is one of this process\'s parts', r.offered.indexOf('PART-NEW') >= 0, 'offers ' + r.offered.join(', ')],
        r => ['Core\'s catalog waits behind its button', r.button && !r.catalog,
            r.catalog + ' catalog rows, ' + (r.button ? 'a' : 'no') + ' catalog button'],
    ],
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
// Counted at is a draft saved by Save settings, every changed row in one
// write: a refused save keeps its rows on screen, and Save again sends the
// same one write.
block({
    title: 'Counted at: a refused save keeps its rows, and Save again sends the same one write',
    open: () => ({ hash: '#tab=settings', extra: COUNTER_ROUTES, answer: refuse('PUT', '/api/processes/1/counted-at', REFUSED) }),
    act: async page => {
        page.click('root', { act: 'ca-add' });
        await page.settle();
        for (const v of ['201', '202']) {
            const pick = page.byId('pd-stpop').querySelectorAll('.picker-row').find(r => r.getAttribute('data-value') === v);
            if (!pick) throw new Error('+ Add a position under Counted at offers no ' + v);
            pick.click();
            await page.settle();
        }
        page.key('Escape');
        await page.settle();
        (() => { const b = page.root.querySelector('[data-set="save"]'); if (!b) throw new Error('Settings draws no Save'); b.click(); })();
        await page.settle();
        // The save's outcome is the shared bar's (shared/settings.js).
        const first = page.S.stShell.page.state.refusal;
        const rows = page.root.querySelectorAll('[data-act="ca-pick"]').length;
        const from = page.mark();
        (() => { const b = page.root.querySelector('[data-set="save"]'); if (!b) throw new Error('Settings draws no Save'); b.click(); })();
        await page.settle();
        return { first: first, rows: rows, again: writes(page.since(from)) };
    },
    checks: [
        r => ['the refusal is said on the bar', r.first.toLowerCase().indexOf(MSG.toLowerCase()) >= 0, 'refusal ' + JSON.stringify(r.first)],
        r => ['both rows stay on screen', r.rows === 2, r.rows + ' rows'],
        r => ['Save again sends one write, with both rows', r.again.length === 1 && /^PUT \/api\/processes\/1\/counted-at$/.test(lineOf(r.again[0])) &&
            JSON.stringify((r.again[0].body || {}).bindings) === '[{"node_id":201,"line_counter_id":51},{"node_id":202,"line_counter_id":51}]',
            r.again.map(c => lineOf(c) + ' ' + JSON.stringify(c.body)).join(' · ') || 'no write'],
    ],
});
resume('ruling 5: + New style from Recipe One, its CATID refused, then pressed again', {
    open: { answer: refuse('PUT', '/api/styles/9', REFUSED) },
    drive: page => T.DRIVE.mint.drive(page),
    once: /^POST \/api\/processes\/1\/flow\/mint$/, step: /^PUT \/api\/styles\/9$/,
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

// SLICE 5: A CORE LOADER'S PROCESS (core_boards) has no styles, so no HMI gate
// line and a Danger line that names only what it has; its windows are Core's,
// so its screen's positions are chips with no × and no picker.
block({
    title: 'slice 5: a Core loader’s process draws no gate, its own Danger line, and its windows read-only',
    open: () => ({
        hash: '#tab=settings',
        page: withPage(p => ({ processes: p.processes.map(x => x.id === 1 ? Object.assign({}, x, { core_boards: true }) : x) })),
    }),
    act: async () => {},
    checks: [
        r => ['no HMI setup line in the bar', String(r.page.root.innerHTML).indexOf('class="pd-gate"') < 0, 'drawn'],
        r => ['Danger names its screens and positions only',
            String(r.page.root.innerHTML).indexOf('its screens and positions go with it') >= 0 &&
            String(r.page.root.innerHTML).indexOf('its styles, flows') < 0, 'not said'],
        r => {
            const html = String(r.page.root.innerHTML);
            const picker = html.indexOf('id="npk-scr-11"') >= 0;
            const chips = (html.match(/<span class="picker-chip">POS_A\d+<\/span>/g) || []).length;
            return ['Screen A’s positions are chips, no picker', !picker && chips === 11, 'picker ' + picker + ', ' + chips + ' chips'];
        },
    ],
});

// SLICE 5: THE MAP IS NOT SENT AGAIN. A composer read after the first quotes
// the revision of the map the page holds (map_rev); a reply that says map_same
// carries no map, and the page keeps drawing the one it has.
block({
    title: 'slice 5: a composer read quotes the held map, and map_same keeps it',
    open: () => ({
        extra: (m, u) => {
            const [p, q] = u.split('?');
            if (m === 'GET' && p === '/api/processes/1/composer' && /(^|&)map_rev=/.test(q || '')) {
                const c = JSON.parse(JSON.stringify(T.FX.composer));
                delete c.map;
                c.map_same = true;
                return { body: c };
            }
            return undefined;
        },
    }),
    act: async page => {
        const held = page.S.composer.map;
        const from = page.mark();
        await page.pd('refreshProcess')();
        await page.settle();
        const reads = page.since(from).map(lineOf).filter(l => /\/composer/.test(l));
        return { held: held, reads: reads, after: page.S.composer.map };
    },
    checks: [
        r => ['the page held a map to quote', !!(r.held && r.held.revision), 'map ' + JSON.stringify(r.held && r.held.revision)],
        r => ['the read quotes its revision', r.reads.length === 1 && r.reads[0] === 'GET /api/processes/1/composer?map_rev=' +
            encodeURIComponent(r.held.revision), JSON.stringify(r.reads)],
        r => ['map_same: the map drawn is the one held', r.after === r.held, 'map ' + JSON.stringify(r.after && r.after.revision)],
    ],
});

// SLICE 5, ruling 8. A process whose positions are all on no screen is not an
// empty process: the loader block (made for an empty one, a loader's board
// being made on it) said "Every loader … has a screen. Nothing to bind." there.
block({
    title: 'slice 5: a process whose positions are all on no screen is not an empty one to a loader',
    open: () => ({ page: withPage(() => ({ stations: [] })) }),
    act: async page => {
        const S = page.S;
        S.loaderGaps = [{ loader: 'LDR_9', process_id: 0 }];
        S.curtainNodes = [{ id: 901, process_id: S.processID, core_node_name: 'POS_A1', operator_station_id: null }];
        const loose = page.pd('loaderGapsHere')();
        S.curtainNodes = [];
        const empty = page.pd('loaderGapsHere')();
        return { loose: loose, empty: empty };
    },
    checks: [
        r => ['positions on no screen: no loader block', r.loose === null, 'gaps ' + JSON.stringify(r.loose)],
        r => ['no position at all: the block offers the loader', Array.isArray(r.empty) && r.empty.length === 1,
            'gaps ' + JSON.stringify(r.empty)],
    ],
});
// A style from a recipe is minted at a screen (flow/mint is the station's
// door): with none, New style says so where the chooser would be, and links
// to where one is added.
block({
    title: 'slice 5: New style on a process with no screen says a recipe style is made at one',
    open: () => ({ page: withPage(() => ({ stations: [] })) }),
    act: async page => {
        page.click('root', { act: 'new-style' });
        await page.settle();
        const body = page.scrim.querySelector('.layer-body');
        return { html: String(body ? body.innerHTML : '') };
    },
    checks: [
        r => ['no recipe chooser', r.html.indexOf('data-act="newstyle-recipe"') < 0, 'drawn'],
        r => ['the line says why, and goes to Positions and routing',
            r.html.indexOf('A style from a recipe is made at a screen') >= 0 && r.html.indexOf('data-act="newstyle-screens"') >= 0,
            r.html.slice(0, 300)],
    ],
});

// ROUND 6: A TAKEN NAME (owner's walk). The server answers it 409 on the name
// field (writeNameTaken); before, the page drew SQLite's "constraint failed:
// UNIQUE constraint failed: processes.name (2067)" on the sheet's status line.
// It is one plain line on the Name field, and typing takes it away.
const TAKEN = name => { const msg = 'another process is already called "' + name + '"'; return { status: 409, body: { error: msg, errors: { name: msg } } }; };
const DB_WORDS = /constraint|UNIQUE|2067|processes\.name/;
const nameLine = host => {
    // A hidden line is no line: the page hides it, empty, once the name is typed again.
    const el = host && host.querySelector('.pd-ferr[data-ferr="name"]');
    return el && !el.hidden ? (el.textContent || T.text(el.innerHTML)) : '';
};
const dbWordsShown = page => page.segments().some(s => DB_WORDS.test(s)) || DB_WORDS.test(statusText(page)) ||
    page.notices.some(n => DB_WORDS.test(n.text)) ||
    DB_WORDS.test((page.S.stShell && page.S.stShell.page.state.refusal) || '');
const typeIn = (page, el, value) => { el.value = value; page.fire(el, 'input'); };
const nameChecks = (want, after) => [
    r => ['the Name field says ' + JSON.stringify(want), r.line === want, 'Name field ' + JSON.stringify(r.line)],
    r => ['no database words on screen', !r.dbWords, 'status ' + JSON.stringify(r.status) + ', bar ' + JSON.stringify(r.bar)],
    r => [after, r.status === r.wantStatus, 'status ' + JSON.stringify(r.status)],
    r => ['typing a name takes the line away', r.lineAfterTyping === '', 'still ' + JSON.stringify(r.lineAfterTyping)],
];
block({
    title: 'round 6: Add process with a taken name says so on the Name field',
    open: () => ({ answer: every('POST', /^\/api\/processes$/, TAKEN('Press B')) }),
    act: async page => {
        page.click('root', { act: 'add-process' });
        await page.settle();
        page.type('name', 'Press B');
        await pressOK(page);
        const r = { line: nameLine(page.scrim), status: statusText(page), dbWords: dbWordsShown(page), bar: '', wantStatus: '' };
        typeIn(page, page.scrim.querySelector('[data-f="name"]'), 'Press C');
        r.lineAfterTyping = nameLine(page.scrim);
        return r;
    },
    checks: nameChecks('Another process is already called Press B.', 'the status line adds nothing'),
});
block({
    title: 'round 6: Setup\'s Change… with a taken name says so on the Name field',
    open: () => ({ answer: every('PUT', /^\/api\/processes\/1$/, TAKEN('Press B')) }),
    act: async page => {
        await page.pd('openEditProcess')();
        await page.settle();
        page.type('name', 'Press B');
        await pressOK(page);
        const r = { line: nameLine(page.scrim), status: statusText(page), dbWords: dbWordsShown(page), bar: '', wantStatus: '' };
        typeIn(page, page.scrim.querySelector('[data-f="name"]'), 'Press C');
        r.lineAfterTyping = nameLine(page.scrim);
        return r;
    },
    checks: nameChecks('Another process is already called Press B.', 'the status line adds nothing'),
});
block({
    title: 'round 6: Settings › Line renamed to a taken name says so on the Name field',
    open: () => ({ hash: '#tab=settings', answer: every('PUT', /^\/api\/processes\/1$/, TAKEN('Press B')) }),
    act: async page => {
        const field = () => page.root.querySelectorAll('[data-st]').find(el => el.dataset.st === 'name' && el.tagName === 'INPUT');
        T.typeSetting(page, '[data-st]', el => el.dataset.st === 'name' && el.tagName === 'INPUT', 'Press B');
        // The browser bubbles the input to the shared page, which marks the bar;
        // the harness fires it on the field alone (page.update is that mark).
        page.S.stShell.page.update();
        await T.stSave(page);
        const bar = (page.S.stShell && page.S.stShell.page.state.refusal) || '';
        const r = { line: nameLine(page.root), status: bar, bar: bar, dbWords: dbWordsShown(page), wantStatus: 'Not saved.' };
        r.kept = field() ? field().value : '';
        typeIn(page, field(), 'Press C');
        r.lineAfterTyping = nameLine(page.root);
        return r;
    },
    checks: nameChecks('Another process is already called Press B.', 'the bar says "Not saved." and no more').concat([
        r => ['the typed name stays in the field', r.kept === 'Press B', 'field ' + JSON.stringify(r.kept)],
    ]),
});

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
