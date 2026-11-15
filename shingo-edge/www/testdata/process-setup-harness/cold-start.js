// cold-start.js — the Processes page's cold start (brief U2), pinned on
// pins.js's drivers and plant: the Setup tab at each of its states, the tabs
// and where #tab= lands, the rail and New style with and without a recipe,
// Add a position's two menus, and the U2 bugs, each as "ok <what holds>" or
// "BUG <what holds>: <what happened>" (the golden holds only ok lines; the
// red run at the base is saved in the unit's report folder).
//
// No mode is spelled here (the swap-mode-literal rule reads this file): a
// mode is found by what the model says about it — the one whose inbound
// staging is required, say.

'use strict';

let T = null;
const L = [];

const clone = x => JSON.parse(JSON.stringify(x));

// A composer read for Press A with some of it taken away, answered in place
// of the fixture's own; page overrides the page data (the process row, the
// screens), and the screens' re-read answers the same screens, so a screen
// never opened on the page is never opened on the server either.
function pressA(edit, pageEdit) {
    const composer = clone(T.FX.composer);
    if (edit) edit(composer);
    const page = clone(T.FX.page);
    if (pageEdit) pageEdit(page);
    return {
        page: page,
        extra: (method, url) => {
            const u = url.split('?')[0];
            if (method === 'GET' && /^\/api\/processes\/1\/composer$/.test(u)) return { body: clone(composer) };
            if (method === 'GET' && u === '/api/operator-stations') return { body: clone(page.stations) };
            return undefined;
        },
    };
}

// The mode whose inbound staging the model requires on a consume row: the
// one a recipe's row cannot be finished without a staging lane for.
function stagingMode(page, node) {
    const M = page.pd('M')();
    for (const m of Object.keys(M.modeLabels())) {
        const s = M.reduce(page.S.model, { type: 'addPosition', node: node, mode: m });
        const chip = M.rowColumns(s, node, 'staging').find(c => c.key === 'staging');
        if (chip && chip.required) return m;
    }
    throw new Error('no mode requires inbound staging');
}

// + Add a position, then `node` and `mode` from the menus it opens, in
// whichever order the page asks.
function addPosition(page, node, mode) {
    page.click('root', { act: 'add-position' });
    for (let i = 0; i < 2; i++) {
        const pop = page.byId('pd-pop');
        if (pop && pop.querySelectorAll('.picker-row').length && !page.rows('pd-pop', 'data-freemode').length) T.pickerRow(page, 'pd-pop', node);
        else if (page.rows('pd-pop', 'data-freemode').length) T.pickRow(page, 'pd-pop', 'data-freemode', mode);
    }
}

async function newRecipe(o) {
    const page = await T.openPage(o);
    page.click('root', { act: 'new-recipe' });
    await page.settle();
    return page;
}

const NO_STAGING = c => { c.routing = c.routing.filter(r => r.role !== 'staging'); };
// ...and no back position either: a press that parks on its own back slot has
// staging without a routing row, so only with neither is the list empty.
const NO_STAGING_NO_BACKS = c => {
    NO_STAGING(c);
    c.cell.positions = c.cell.positions.filter(p => p.kind !== 'back');
};
const barText = page => T.text((page.byId('pd-bar') || {}).innerHTML || '');
const check = (holds, ok, detail) => L.push(ok ? 'ok   ' + holds : 'BUG  ' + holds + ': ' + detail);

// ── the U2 bugs ──────────────────────────────────────────────────────────────

// F7: staging is defaulted from the routing set, as source and destination are.
async function bugStagingDefault() {
    L.push('', '## F7: + Add a position on a recipe draft, a mode that needs inbound staging');
    const page = await newRecipe();
    const mode = stagingMode(page, 'POS_A11');
    addPosition(page, 'POS_A11', mode);
    await page.settle();
    const c = page.S.model.cells.POS_A11 || {};
    check('the row is added with the mode picked', c.on && c.mode === mode, 'on=' + c.on + ' mode=' + c.mode);
    check('its inbound source and destination come from the routing set', c.source === 'GRP_A' && c.dest === 'DST_1',
        'source=' + c.source + ' dest=' + c.dest);
    check('its inbound staging comes from the routing set too (STG_1, the first staging row)', c.staging === 'STG_1',
        'staging=' + JSON.stringify(c.staging || ''));
}

// F5, F33: the recipe draft's bar.
async function bugDraftBar() {
    L.push('', '## F5/F33: the recipe draft\'s bar, on a routing set with no staging row');
    const page = await newRecipe(pressA(NO_STAGING));
    const prov0 = (/class="prov[^"]*">([^<]*)</.exec((page.byId('pd-bar') || {}).innerHTML || '') || [])[1];
    check('an untouched draft says "No unsaved changes"', prov0 === 'No unsaved changes', 'prov=' + JSON.stringify(prov0));
    const mode = stagingMode(page, 'POS_A11');
    addPosition(page, 'POS_A11', mode);
    await page.settle();
    const bar = barText(page);
    check('a required cell left empty is "1 to fix" on the bar', /^1 to fix/.test(bar), 'bar=' + JSON.stringify(bar));
    check('the bar never says "Preview OK" over a recipe draft', !/Preview OK/.test(bar), 'bar=' + JSON.stringify(bar));
    const save = /<button class="pd-btn primary" data-act="shape-save"( disabled)?/.exec(page.root.innerHTML) || [];
    const shapeSave = page.root.querySelector('[data-act="shape-save"]');
    check('Save recipe… is disabled while it is', !!(shapeSave && shapeSave.disabled) || !!save[1],
        'Save recipe… enabled');
    check('and the bar says "Unsaved changes" now', /Unsaved changes/.test(bar), 'bar=' + JSON.stringify(bar));
}

// F4: Back on a dirty recipe draft asks first.
async function bugDraftBack() {
    L.push('', '## F4: Back on a recipe draft');
    {
        const page = await newRecipe();
        page.click('root', { act: 'shape-cancel' });
        await page.settle();
        check('an untouched draft leaves without asking', !page.dialog() && !page.S.shapeDraft,
            'dialog=' + !!page.dialog() + ' draft=' + !!page.S.shapeDraft);
    }
    const page = await newRecipe();
    addPosition(page, 'POS_A11', stagingMode(page, 'POS_A11'));
    await page.settle();
    page.click('root', { act: 'shape-cancel' });
    await page.settle();
    const d = page.dialog();
    check('a changed draft asks before it is discarded', !!d && !!page.S.shapeDraft,
        'dialog=' + (d ? JSON.stringify(d.text) : 'none') + ', draft ' + (page.S.shapeDraft ? 'kept' : 'discarded'));
    if (!d) return;
    L.push('#    asked: ' + JSON.stringify(d.text) + ' [' + d.buttons.join(' · ') + ']');
    d.press(d.buttons.find(b => /keep/i.test(b)) || d.buttons[0]);
    await page.settle();
    check('Keep editing keeps the draft and its row', !!page.S.shapeDraft && !!(page.S.model.cells.POS_A11 || {}).on,
        'draft=' + !!page.S.shapeDraft);
    page.click('root', { act: 'shape-cancel' });
    await page.settle();
    const d2 = page.dialog();
    if (d2) d2.press(d2.buttons.find(b => /discard/i.test(b)) || d2.buttons[d2.buttons.length - 1]);
    await page.settle();
    check('Discard leaves the draft', !page.S.shapeDraft, 'draft kept');
}

// F4: every other way out of a changed recipe draft asks Back's question:
// a tab, the address bar's #tab=, the rail's + New style. Keep editing keeps
// the draft where it was; Discard lands where the click went. A write from the
// rail's ⋯ (a rename) is not a way out: the draft stays.
const LEAVES = [
    ['the Settings tab', page => page.click('root', { tab: 'settings' }),
        page => page.S.tab === 'settings' && /class="pd-sheet/.test(page.root.innerHTML)],
    ['#tab=settings typed in the address bar', page => { page.ctx.location.hash = '#tab=settings'; page.fireWindow('hashchange'); },
        page => page.S.tab === 'settings'],
    ['the rail\'s + New style', page => page.click('root', { act: 'new-style' }),
        page => page.sheetOpen() && /New style/.test(page.scrim.querySelector('.layer-head').textContent)],
];
async function changedDraft() {
    const page = await newRecipe();
    addPosition(page, 'POS_A11', stagingMode(page, 'POS_A11'));
    await page.settle();
    return page;
}
async function bugDraftLeave() {
    for (const [how, leave, landed] of LEAVES) {
        L.push('', '## F4: ' + how + ' out of a recipe draft');
        {
            const page = await newRecipe();
            leave(page);
            await page.settle();
            check('an untouched draft leaves without asking, and lands there', !page.dialog() && !page.S.shapeDraft && landed(page),
                'dialog=' + !!page.dialog() + ' draft=' + !!page.S.shapeDraft + ' tab=' + page.S.tab);
        }
        const page = await changedDraft();
        const states = page.replaceStates.length;
        leave(page);
        await page.settle();
        const d = page.dialog();
        check('a changed draft asks first, the draft still on screen', !!d && !!page.S.shapeDraft && page.S.tab === 'flows',
            'dialog=' + (d ? JSON.stringify(d.text) : 'none') + ', draft ' + (page.S.shapeDraft ? 'kept' : 'discarded') +
            ', tab ' + page.S.tab);
        if (!d) continue;
        check('the question is Back\'s', d.text === 'Discard the changes?' && d.buttons.join(' · ') === 'Keep editing · Discard',
            JSON.stringify(d.text) + ' [' + d.buttons.join(' · ') + ']');
        d.press('Keep editing');
        await page.settle();
        check('Keep editing keeps the draft and its row, on Styles', !!page.S.shapeDraft && !!(page.S.model.cells.POS_A11 || {}).on &&
            page.S.tab === 'flows' && !page.sheetOpen(), 'draft=' + !!page.S.shapeDraft + ' tab=' + page.S.tab);
        const url = page.replaceStates[page.replaceStates.length - 1] || '';
        check('and the address bar says Styles', page.replaceStates.length === states || !/#tab=/.test(url),
            'url=' + JSON.stringify(url));
        leave(page);
        await page.settle();
        const d2 = page.dialog();
        if (d2) d2.press('Discard');
        await page.settle();
        check('Discard leaves the draft and lands there', !page.S.shapeDraft && landed(page),
            'draft=' + !!page.S.shapeDraft + ' tab=' + page.S.tab);
    }
    L.push('', '## F4: a rename from the rail\'s ⋯ on a changed recipe draft');
    const page = await changedDraft();
    page.click('root', { act: 'style-rename', style: '2' });
    await page.settle();
    page.type('name', 'Style B2');
    await T.sheetOK(page);
    check('the rename lands and the draft stays, its row with it', !page.sheetOpen() && !!page.S.shapeDraft &&
        !!(page.S.model.cells.POS_A11 || {}).on && page.S.tab === 'flows',
        'sheet ' + (page.sheetOpen() ? 'open' : 'closed') + ', draft ' + (page.S.shapeDraft ? 'kept' : 'discarded'));
}

// F3: a required cell with nothing to offer is not a dead end.
async function bugDeadEnd() {
    L.push('', '## F3: the required inbound staging chip, with no staging row and no back position');
    const page = await newRecipe(pressA(NO_STAGING_NO_BACKS));
    addPosition(page, 'POS_A11', stagingMode(page, 'POS_A11'));
    await page.settle();
    page.click('root', { act: 'pick', node: 'POS_A11', kind: 'col:staging:staging' });
    await page.settle();
    const pop = page.byId('pd-pop');
    const words = T.text((pop || {}).innerHTML || '');
    L.push('#    the popover: ' + JSON.stringify(words.slice(0, 160)));
    // The block is the node adapter's picker: a node it may take is a row
    // that is not greyed (is-off).
    const use = pop ? pop.querySelectorAll('.picker-row').filter(r => !r.classList.contains('is-off')) : [];
    check('the popover offers places from the plant to add and use', use.length > 0, 'nothing to pick');
    if (!use.length) return;
    const from = page.mark();
    const row = use.find(r => r.getAttribute('data-value') === 'STG_9') || use[0];
    const name = row.getAttribute('data-value');
    row.click();
    await page.settle();
    const calls = page.since(from);
    L.push('#    picked ' + name + ': ' + calls.map(c => c.method + ' ' + c.url + (c.body ? ' ' + JSON.stringify(c.body) : '')).join(' · '));
    check('Add and use is one request: the routing set gains it for staging',
        calls.length === 1 && calls[0].method === 'POST' && /\/routing-nodes$/.test(calls[0].url) &&
        calls[0].body && calls[0].body.role === 'staging' && calls[0].body.core_node_name === name,
        calls.length + ' requests');
    check('and the row uses it', (page.S.model.cells.POS_A11 || {}).staging === name,
        'staging=' + JSON.stringify((page.S.model.cells.POS_A11 || {}).staging || ''));
}

// F3's slot: a name the page's node list does not carry is offered to add and
// use (Core may have sent it since the page read its list); the Edge checks
// it on the write, and its refusal is said in the popover, which stays.
async function bugDeadEndSlot() {
    L.push('', '## F3: Add and use, a name the node list does not carry');
    const refusal = 'core has no node "STG_NEW"';
    const o = pressA(NO_STAGING_NO_BACKS);
    o.answer = (m, u) => (m === 'POST' && /\/routing-nodes$/.test(u) ? { status: 400, body: { error: refusal } } : undefined);
    const page = await newRecipe(o);
    addPosition(page, 'POS_A11', stagingMode(page, 'POS_A11'));
    await page.settle();
    page.click('root', { act: 'pick', node: 'POS_A11', kind: 'col:staging:staging' });
    await page.settle();
    const pop = page.byId('pd-pop');
    const q = pop ? pop.querySelector('.picker-q') : null;
    check('the block has a search box', !!q, 'none');
    if (!q) return;
    q.value = 'STG_NEW';
    page.fire(q, 'input');
    const slot = pop.querySelector('.picker-add');
    L.push('#    the slot: ' + JSON.stringify(slot ? slot.textContent : ''));
    check('a name with no row offers to add and use it', !!slot, 'no slot');
    if (!slot) return;
    const from = page.mark();
    slot.click();
    await page.settle();
    const calls = page.since(from);
    L.push('#    pressed: ' + calls.map(c => c.method + ' ' + c.url + (c.body ? ' ' + JSON.stringify(c.body) : '')).join(' · '));
    check('the press is the routing set\'s one upsert', calls.length === 1 && calls[0].method === 'POST' &&
        calls[0].body && calls[0].body.core_node_name === 'STG_NEW' && calls[0].body.role === 'staging', calls.length + ' requests');
    const warn = page.byId('pd-pop') ? page.byId('pd-pop').querySelector('.pd-warn') : null;
    check('the refusal is said in the popover, which stays', !!warn && !warn.hidden && warn.textContent.indexOf(refusal) >= 0,
        warn ? JSON.stringify(warn.textContent) : 'no line');
    check('and the row is as it was', !(page.S.model.cells.POS_A11 || {}).staging,
        'staging=' + JSON.stringify((page.S.model.cells.POS_A11 || {}).staging || ''));
}

// F36: HMI setup with no recipe says so, and offers New recipe there.
async function bugHmiNoRecipe() {
    L.push('', '## F36: Settings › HMI setup on a process with no recipe');
    const o = pressA(c => { c.presets = []; });
    const presetsView = { presets: [], candidates: [{
        shape_key: 'k1', suggested_name: 'Found shape', mode: '', where: 'POS_A1 / POS_A3',
        shape: clone(T.FX.server.presetsView.presets[0].shape), members: [1, 2], member_names: ['Style A', 'Style B'],
    }] };
    const extra = o.extra;
    o.extra = (m, u, b) => (m === 'GET' && /\/api\/processes\/1\/presets$/.test(u) ? { body: presetsView } : extra(m, u, b));
    o.hash = '#tab=settings';
    const page = await T.openPage(o);
    await page.settle();
    const html = page.root.innerHTML.slice(page.root.innerHTML.indexOf('<div class="pd-sheet'));
    const at = html.indexOf('id="pd-hmirecipes"');
    const hmi = at < 0 ? '' : T.text(html.slice(at, at + 1600));
    L.push('#    ' + JSON.stringify(hmi.slice(0, 160)));
    check('the HMI setup row says the process has no recipe', /no recipe/i.test(hmi), 'nothing says so');
    check('and offers New recipe there', /data-act="new-recipe"/.test(html), 'no New recipe on Settings');
    check('"Found in your flows" is beside it', /Found in your flows/.test(html), 'not on Settings');
    const before = html.slice(html.indexOf('id="pd-hmirecipes"'));
    check('HMI setup on with no recipe: the recipes row says it in the warning hue', /pd-cnt warn/.test(before.slice(0, 600)),
        'no warning');
    page.click('root', { act: 'st-toggle', st: 'flow_composer_enabled' });
    const after = page.root.innerHTML.slice(page.root.innerHTML.indexOf('id="pd-hmirecipes"'));
    const on = !!page.S.settings.flow_composer_enabled;
    check('switched ' + (on ? 'on' : 'off') + ' (not saved), the recipes row ' + (on ? 'warns' : 'does not warn'),
        /pd-cnt warn/.test(after.slice(0, 600)) === on, 'warn=' + /pd-cnt warn/.test(after.slice(0, 600)));
}

// ── style first (F2): the rail, the header and New style, with and without a recipe ──
async function pinStyleFirst() {
    for (const [title, o] of [['Press A with Recipe One', undefined],
        ['Press A with no recipe', pressA(c => { c.presets = []; })],
        ['Press A with no style and no recipe', pressA(c => { c.presets = []; c.styles = []; })]]) {
        L.push('', '## ' + title);
        const page = await T.openPage(o);
        const html = page.root.innerHTML;
        const rail = (/<div class="pd-rail-body"[^>]*>([\s\S]*?)<\/div><div class="pd-railfoot">/.exec(html) || [])[1] || '';
        const heads = [...rail.matchAll(/<div class="pd-grp pd-lbl">([^<]*)<\/div>/g)].map(m => m[1]);
        L.push('rail headings: ' + JSON.stringify(heads) + ' · + New recipe ' +
            (/data-act="new-recipe"/.test(rail) ? 'drawn' : 'not drawn'));
        L.push('rail foot: ' + JSON.stringify(T.text((/<div class="pd-railfoot">([\s\S]*?)<\/div><\/div>/.exec(html) || [])[1] || '')));
        const acts = [...html.matchAll(/<button class="pd-btn[^"]*" data-act="([^"]*)"[^>]*>([^<]*)</g)]
            .filter(m => !/pd-railfoot|pd-railadd/.test(m[0])).map(m => m[2]);
        L.push('main: ' + JSON.stringify(T.text((/<div class="pd-head">([\s\S]*?)<div class="act">/.exec(html) || [])[1] || '')) +
            ' · buttons ' + JSON.stringify(acts));
        page.click('root', { act: 'new-style' });
        await page.settle();
        const sheet = page.scrim;
        const body = sheet.querySelector('.layer-body');
        const labels = [...String(body ? body.innerHTML : '').matchAll(/<label>([^<]*)</g)].map(m => m[1]);
        const head = sheet.querySelector('.layer-head');
        L.push('New style: ' + JSON.stringify(head ? [...head.childNodes].map(n => n.textContent).join(' — ') : '') +
            ' · fields ' + JSON.stringify(labels));
        const chooser = page.byId('pd-newrecipe');
        if (chooser) {
            const field = (sheet.querySelectorAll('input').find(el => el.getAttribute('data-f') === 'recipe') || {});
            L.push('  the chooser opens on ' + JSON.stringify(String(chooser.innerHTML || '').replace(/<[^>]*>/g, '')) + ' (value ' + JSON.stringify(field.value || '') + ')');
        }
    }
}

// ── the Setup tab (ruling 2026-11-15): three steps, and when it is finished ──
//
// SETUP IS THREE STEPS: Nodes and part (the Add process sheet, opened again by
// Change…), First style (a part on each position and one way they swap, saved
// by the Styles save), Run it (the screen, opened). IT IS FINISHED WHEN THE
// LINE CAN RUN AND ITS SCREEN HAS BEEN OPENED: positions, an inbound, an
// outbound, a part, a style with a flow, a screen holding a position, and that
// screen's last_seen_at. SKIP is this browser's (pd.setup.skipped.<pid>).
const tabsOf = page => [...page.root.innerHTML.matchAll(/<button class="([^"]*)" data-tab="([^"]*)">([^<]*)</g)]
    .map(m => m[3] + (/\bon\b/.test(m[1]) ? '*' : ''));
// A step: its state (done, now, next), its title and what it holds, then its
// body's words and its controls (a primary one marked).
function setupLines(page) {
    const html = page.root.innerHTML;
    const out = [];
    const head = /<div class="pd-sutitle">([\s\S]*?)<\/div>/.exec(html);
    if (head) out.push('  title: ' + JSON.stringify(T.text(head[1])));
    for (const h of html.split(/(?=<li class="pd-sus )/).slice(1)) {
        const li = h.slice(0, h.indexOf('</li>') + 5);
        const state = (/^<li class="pd-sus ([a-z]+)"/.exec(li) || [])[1];
        const t = /<div class="pd-susb"><b>([^<]*)<\/b><small>([^<]*)<\/small>([\s\S]*)<\/div><\/li>$/.exec(li) || [];
        const acts = [...String(t[3] || '').matchAll(/<(button|a) class="pd-btn( primary)?"([^>]*)>([^<]*)</g)]
            .map(m => m[4] + (m[2] ? ' [primary]' : '') + ' (' + ((/data-act="([^"]*)"/.exec(m[3]) || [])[1] || (/href="([^"]*)"/.exec(m[3]) || [])[1] || '') + ')');
        out.push('  ' + state + ' ' + T.decode(t[1] || '?') + ': ' + JSON.stringify(T.decode(t[2] || '')) + (acts.length ? ' → ' + acts.join(' · ') : ''));
        const words = T.text(String(t[3] || '').replace(/<(button|a) class="pd-btn[\s\S]*?<\/(button|a)>/g, ''));
        if (words) out.push('      ' + JSON.stringify(words));
    }
    const end = /<div class="pd-sucard col"><div><b>([^<]*)<\/b><small>([^<]*)<\/small>/.exec(html);
    if (!out.some(l => /^  [a-z]+ /.test(l)) && end) out.push('  ' + JSON.stringify(T.decode(end[1] + ' ' + end[2])));
    const side = /<div class="pd-suside">([\s\S]*?)<div class="pd-pop"/.exec(html);
    if (side) out.push('  beside: ' + JSON.stringify(T.text(side[1])));
    const bar = /<button class="pd-dimlink pd-skip" data-act="(setup-[a-z]+)">([^<]*)</.exec(html);
    out.push('  app bar: ' + (bar ? JSON.stringify(bar[2]) + ' (' + bar[1] + ')' : 'no skip or show'));
    return out;
}
const NEVER_SEEN = pg => { for (const st of pg.stations) delete st.last_seen_at; };
const NO_FLOW = c => { c.styles = c.styles.slice(0, 1).map(st => Object.assign(st, { claims: [], claim_count: 0, claim_modes: [], last_run: '' })); c.presets = []; };
const STATES = [
    ['a new process: nothing yet', c => { c.cell.positions = []; c.routing = []; c.styles = []; c.presets = []; c.palette = []; }],
    ['positions claimed', c => { c.routing = []; c.styles = []; c.presets = []; }],
    ['and a routing set', c => { c.styles = []; c.presets = []; }],
    ['and a style with no flow yet', NO_FLOW],
    ['and its flow drawn: the line can run, its screen never opened', c => { c.styles.forEach(st => { st.last_run = ''; }); }],
];
const NOT_RUN = pg => { pg.processes[0].active_style_id = 0; pg.processes[0].flow_composer_enabled = false; NEVER_SEEN(pg); };
// A request as one line: a write with its body, but a flow preview's or
// save's (their cells are the table's goldens).
const lines = (page, from) => page.since(from).map(c => c.method + ' ' + c.url.replace('/api/processes/1', '…') +
    (c.method !== 'GET' && c.body !== undefined && !/\/flow\/(preview|save)$/.test(c.url) ? ' ' + JSON.stringify(c.body) : ''));
// Setup's own re-read of the screens (SETUP_POLL_MS, 5 s); the rail's 10 s
// re-read of the styles runs on every tab and is not Setup's.
const setupTicks = page => page.intervals().filter(t => t.ms === 5000);

async function pinSetup() {
    L.push('', '## the Setup tab: Press A opened at each state, its screens never opened (the steps: state title: what it holds → its controls)');
    for (const [title, edit] of STATES) {
        const page = await T.openPage(pressA(edit, NOT_RUN));
        L.push('# ' + title + ' · lands on ' + page.S.tab + ' · tabs ' + JSON.stringify(tabsOf(page)) +
            ' · ' + (setupTicks(page).length ? 're-reads the screens every 5 s' : 'no re-read of the screens'));
        L.push(...setupLines(page));
    }

    L.push('', '## when Setup is finished: the line can run AND its screen has been opened (last_seen_at)');
    {
        const page = await T.openPage(pressA(c => { c.styles.forEach(st => { st.last_run = ''; }); }));
        L.push('# the line can run, its screen seen 30 s ago · lands on ' + page.S.tab + ' · tabs ' + JSON.stringify(tabsOf(page)));
    }
    {
        const page = await T.openPage(pressA(NO_FLOW));
        L.push('# its screen seen, no style with a flow · lands on ' + page.S.tab + ' · tabs ' + JSON.stringify(tabsOf(page)));
    }
    {
        // The Run it step's re-read: the screens answer seen at the next tick.
        let seen = false;
        const o = pressA(c => { c.styles.forEach(st => { st.last_run = ''; }); }, NOT_RUN);
        const base = o.extra;
        o.extra = (m, u) => (m === 'GET' && u.split('?')[0] === '/api/operator-stations' && seen
            ? { body: T.FX.page.stations.map(st => Object.assign({}, st, { last_seen_at: '2026-11-11T14:05:20' })) } : base(m, u));
        const page = await T.openPage(o);
        const ticks = setupTicks(page);
        check('waiting on Run it, the screens are re-read every 5 s, one timer', ticks.length === 1,
            JSON.stringify(page.intervals().map(t => t.ms)));
        let from = page.mark();
        await ticks[0].fn();
        await page.settle();
        check('a tick with no screen opened is one read and changes nothing', lines(page, from).join() === 'GET /api/operator-stations' &&
            page.S.tab === 'setup' && /Waiting for the screen to be opened/.test(page.root.innerHTML), lines(page, from).join(' · ') + ' tab ' + page.S.tab);
        seen = true;
        from = page.mark();
        await ticks[0].fn();
        await page.settle();
        L.push('# the screen opened, on the next tick: ' + lines(page, from).join(', '));
        L.push(...setupLines(page));
        check('it says the line is set up and stops re-reading', /is set up<\/h1>/.test(page.root.innerHTML) && setupTicks(page).length === 0,
            'timers ' + setupTicks(page).length);
        check('the tab stays while the engineer is on it', tabsOf(page).indexOf('Setup*') >= 0, JSON.stringify(tabsOf(page)));
        page.click('root', { tab: 'flows' });
        await page.settle();
        check('and goes once they leave it', tabsOf(page).indexOf('Setup') < 0 && tabsOf(page).indexOf('Setup*') < 0, JSON.stringify(tabsOf(page)));
    }

    L.push('', '## Skip and Show setup (this browser\'s, per process)');
    {
        const page = await T.openPage(pressA(STATES[1][1], NOT_RUN));
        page.click('root', { act: 'setup-skip' });
        await page.settle();
        L.push('# Skip setup: tab ' + page.S.tab + ' · tabs ' + JSON.stringify(tabsOf(page)) + ' · stored ' + JSON.stringify(page.storage) +
            ' · ' + JSON.stringify((/<button class="pd-dimlink pd-skip" data-act="(setup-[a-z]+)">([^<]*)</.exec(page.root.innerHTML) || []).slice(1)));
        check('Skip opens Settings with no Setup tab, and offers Show setup', page.S.tab === 'settings' && tabsOf(page).every(t => !/^Setup/.test(t)) &&
            /data-act="setup-show">Show setup</.test(page.root.innerHTML), 'tab ' + page.S.tab + ' tabs ' + JSON.stringify(tabsOf(page)));
    }
    {
        const page = await T.openPage(Object.assign(pressA(STATES[1][1], NOT_RUN), { storage: { 'pd.setup.skipped.1': '1' } }));
        L.push('# opened again, skipped: lands on ' + page.S.tab + ' · tabs ' + JSON.stringify(tabsOf(page)));
        page.click('root', { act: 'setup-show' });
        await page.settle();
        L.push('# Show setup: tab ' + page.S.tab + ' · tabs ' + JSON.stringify(tabsOf(page)) + ' · stored ' + JSON.stringify(page.storage));
        check('Show setup brings the tab back and forgets the skip', page.S.tab === 'setup' && !('pd.setup.skipped.1' in page.storage),
            'tab ' + page.S.tab + ' stored ' + JSON.stringify(page.storage));
    }
    {
        const page = await T.openPage(Object.assign(pressA(c => { c.styles.forEach(st => { st.last_run = ''; }); }, NOT_RUN),
            { storage: { 'pd.setup.skipped.1': '1' } }));
        check('skipped, a line that can run offers no Show setup', !/data-act="setup-show"/.test(page.root.innerHTML) && page.S.tab === 'flows',
            'tab ' + page.S.tab);
    }
    {
        const page = await T.openPage(pressA(null, pg => { pg.processes[0].core_boards = true; NEVER_SEEN(pg); }));
        check('a Core loader\'s process has no Setup, and nothing to skip', tabsOf(page).every(t => !/^Setup/.test(t)) &&
            !/data-act="setup-(skip|show)"/.test(page.root.innerHTML), JSON.stringify(tabsOf(page)));
    }

    L.push('', '## where a #tab= link lands (the Recipes tab went to Operator setup on Settings; #tab=styles is the Styles tab)');
    for (const hash of ['#tab=presets', '#tab=recipes', '#tab=presets;preset=7', '#tab=settings', '#tab=setup', '#tab=styles']) {
        const page = await T.openPage({ hash: hash });
        await page.settle();
        const html = page.root.innerHTML;
        L.push('  ' + hash + ' → tab ' + page.S.tab + ' · tabs ' + JSON.stringify(tabsOf(page)) +
            (/id="pd-hmirecipes"/.test(html) ? ' · the recipes beside HMI setup' : '') +
            (/<tr class="on" data-preset="7">/.test(html) ? ' · Recipe One expanded' : '') +
            ' · hash ' + JSON.stringify(page.replaceStates[page.replaceStates.length - 1] || ''));
    }
    {
        const page = await T.openPage({ hash: '#tab=settings' });
        const at = page.root.innerHTML.indexOf('id="pd-hmirecipes"');
        L.push('  Settings › Operator setup: ' + JSON.stringify(T.text(page.root.innerHTML.slice(at, at + 2000)).slice(0, 220)));
    }

    L.push('', '## the Setup tab\'s actions (each on a fresh page; its requests)');
    const ACTS = [
        ['Pick them (step 1, a new process with positions)', STATES[1][1], 'setup-edit'],
        ['Start (step 2, a routing set and no style: one is made, named by the first part)', STATES[2][1], 'setup-style'],
        ['Start (step 2, a style with no flow yet)', NO_FLOW, 'setup-style'],
        ['Production counter (Later, on Settings)', STATES[1][1], 'setup-later', { to: 'Production counter' }],
    ];
    for (const [title, edit, act, more] of ACTS) {
        // A style Start makes is in the composer's next read.
        let made = false;
        const o = pressA(edit, NOT_RUN);
        const base = o.extra;
        o.extra = (m, u) => {
            if (m === 'POST' && u === '/api/styles') { made = true; return undefined; }
            if (made && m === 'GET' && /^\/api\/processes\/1\/composer$/.test(u.split('?')[0])) {
                const r = base(m, u);
                r.body.styles = [{ id: 6, name: 'PART-1', claims: [], claim_count: 0, claim_modes: [], parts: [], advanced: {} }];
                return r;
            }
            return base(m, u);
        };
        const page = await T.openPage(o);
        const from = page.mark();
        page.click('root', Object.assign({ act: act }, more || {}));
        await page.settle();
        const sheet = page.sheetOpen() ? page.scrim.querySelector('.layer-head') : null;
        L.push('  ' + title + ' → tab ' + page.S.tab + (page.S.setupView ? ' (' + page.S.setupView + ' step)' : '') +
            (sheet ? ', sheet ' + JSON.stringify(sheet.childNodes[0].textContent) : '') + ' · ' + lines(page, from).join(', '));
    }
}

// ── Setup · First style: a name, a part on each position, how they swap ────
//
// Round 2 (2026-11-15): the name is a field; the positions are rows of
// Position, Part and Swap; everything the step fills in is a dropdown in its
// role's colour (Inbound source, Outbound destination, Inbound staging,
// Outbound staging), one set for the line and one per position whose swap
// differs; a free row offers "add a part" from Core's catalog.
async function pinFirstStyle() {
    L.push('', '## Setup › First style on Press A (a style with no flow yet): the step, a choice, its preview and its Save');
    // Once saved, the composer read holds the flow (the fixture's Style A).
    let saved = false;
    const o = pressA(NO_FLOW, NOT_RUN);
    const unsaved = o.extra;
    o.extra = (m, u) => {
        if (m === 'POST' && /\/flow\/save$/.test(u)) { saved = true; return undefined; }
        return saved && m === 'GET' && /^\/api\/processes\/1\/composer$/.test(u.split('?')[0]) ? undefined : unsaved(m, u);
    };
    const page = await T.openPage(o);
    page.click('root', { act: 'setup-style' });
    await page.settle();
    const html = () => page.root.innerHTML;
    const rows = () => [...html().matchAll(/<div class="pd-fsrow"><b>([^<]*)<\/b>(<button class="pd-sel" data-act="fs-part"[^>]*><span class="t">([^<]*)<\/span>.*?data-act="fs-swap"[^>]*><span class="t">([^<]*)<\/span>|<button class="pd-btn quiet" data-act="fs-addpart")/g)]
        .map(m => m[1] + '=' + (m[3] !== undefined ? T.decode(m[3]) + ' (' + T.decode(m[4]) + ')' : 'free'));
    const on = re => [...html().matchAll(re)].map(m => m[1]);
    const picks = () => [...html().matchAll(/<div class="pd-fspick" data-role="([^"]*)">.*?<b>([^<]*)<\/b>.*?data-pick="([^"]*)" data-node="([^"]*)"><span class="t">([^<]*)<\/span>/g)]
        .map(m => (m[4] ? m[4] + ' ' : '') + m[2] + '=' + T.decode(m[5]) + ' [' + m[1] + ']');
    const say = () => {
        L.push('  name: ' + JSON.stringify(((page.byId('pd-fsname') || {}).value) || '') + ' · rows: ' + rows().join(' · '));
        L.push('  role: ' + on(/<button class="on" data-act="fs-role" data-role="([^"]*)"/g).join() +
            ' · swap for every position: ' + (on(/<button class="pd-fsmode on" data-act="fs-mode" data-mode="([^"]*)"/g).join() || 'none') +
            ' · bar: ' + JSON.stringify(T.text((/<div id="pd-bar" class="pd-fsbar">([\s\S]*?)<\/div>/.exec(html()) || [])[1] || '')) +
            ' · Save style ' + (/data-act="fs-save" disabled/.test(html()) ? 'disabled' : 'enabled'));
        L.push('  picked for you: ' + picks().join(' · '));
        const line = /<div class="pd-suline">([\s\S]*?)<p class="pd-note">([\s\S]*?)<\/p>/.exec(html());
        L.push('  line so far: ' + JSON.stringify(line ? T.text(line[1]) + ' | ' + T.text(line[2]) : ''));
        const free = /<p class="pd-fsfree">([^<]*)<\/p>/.exec(html());
        if (free) L.push('  free: ' + JSON.stringify(T.decode(free[1])));
    };
    check('Start opens the First style step on Setup', page.S.tab === 'setup' && page.S.setupView === 'style', 'tab ' + page.S.tab + ' view ' + page.S.setupView);
    check('one preview of the draft, through the Styles tab\'s door', page.calls.filter(c => /\/flow\/preview$/.test(c.url)).length >= 1, 'no preview');
    say();
    // The swap the step starts on, read off its own card (a mode is never
    // spelled here: the page's cards name them).
    const startMode = on(/<button class="pd-fsmode on" data-act="fs-mode" data-mode="([^"]*)"/g)[0];
    check('a name somebody chose (Style A) is kept as the parts move: it is not a part', page.S.firstStyle.named === true, 'named ' + page.S.firstStyle.named);
    check('no "set each one": a row\'s own Swap is the way to a mix', !/data-act="fs-each"/.test(html()), 'fs-each still drawn');
    let from = page.mark();
    const mode = (/data-act="fs-mode" data-mode="([^"]*)"/.exec(html().replace(/<button class="pd-fsmode on"[^>]*>/g, '')) || [])[1];
    page.click('root', { act: 'fs-mode', mode: mode });
    await page.settle();
    L.push('# another swap for every position (' + mode + '): ' + lines(page, from).join(', '));
    say();
    from = page.mark();
    page.click('root', { act: 'fs-role', role: 'produce' });
    await page.settle();
    L.push('# Produce: ' + lines(page, from).join(', '));
    say();
    // A ROW'S OWN SWAP: POS_A2 back on the swap the step started on, the rest
    // on the line's. It gets a set of picks of its own, listing only what its
    // swap uses.
    page.click('root', { act: 'fs-swap', node: 'POS_A2' });
    await page.settle();
    from = page.mark();
    T.pickerRow(page, 'pd-pop', startMode);
    await page.settle();
    L.push('# POS_A2 on its own swap (the one the step started on): ' + lines(page, from).join(', '));
    say();
    check('a position on its own swap has its own set of picks', picks().some(p => /^POS_A2 /.test(p)), picks().join(' · '));
    // A PICK IS A DROPDOWN OF ITS ROLE'S BOX: POS_A2's outbound staging moves
    // to STG_3, and the line so far follows.
    page.click('root', { act: 'fs-pick', pick: 'parkOld', node: 'POS_A2' });
    await page.settle();
    const offered = (page.byId('pd-pop') ? page.byId('pd-pop').querySelectorAll('.picker-row') : []).map(r => r.getAttribute('data-value'));
    L.push('# POS_A2\'s Outbound staging offers: ' + offered.join(', '));
    from = page.mark();
    T.pickerRow(page, 'pd-pop', 'STG_3');
    await page.settle();
    L.push('# POS_A2\'s Outbound staging → STG_3: ' + lines(page, from).join(', '));
    say();
    check('the draft holds the pick', (page.S.model.cells.POS_A2 || {}).parkOld === 'STG_3', JSON.stringify(page.S.model.cells.POS_A2));
    // A FREE ROW'S "+ add a part": Core's catalog; PART-1, on POS_A1, moves.
    page.click('root', { act: 'fs-addpart', node: 'ULD_1' });
    await page.settle();
    const pop = page.byId('pd-pop');
    const note = ((pop ? pop.querySelectorAll('.picker-row') : []).find(r => r.getAttribute('data-value') === 'PART-1') || { querySelector: () => null });
    L.push('# ULD_1\'s + add a part, PART-1 in Core\'s catalog: ' + JSON.stringify((note.querySelector('.picker-note') || { textContent: '' }).textContent));
    from = page.mark();
    T.pickerRow(page, 'pd-pop', 'PART-1');
    await page.settle();
    L.push('# ULD_1 takes PART-1: ' + lines(page, from).join(', '));
    say();
    // THE NAME TAKES ANY TEXT; a changed one is the style's rename, written
    // before the Styles save.
    const name = page.byId('pd-fsname');
    name.value = 'FG-7731 finished';
    page.fire(name, 'input');
    from = page.mark();
    page.click('root', { act: 'fs-save' });
    await page.settle();
    L.push('# Save style: ' + lines(page, from).filter(l => !/^GET /.test(l)).join(', ') + ' · then tab ' + page.S.tab +
        (page.S.setupView ? ' (' + page.S.setupView + ')' : '') + ' · notices ' + JSON.stringify(page.notices.map(n => n.text)));
    const writes = page.since(from).filter(c => c.method !== 'GET').map(c => c.method + ' ' + c.url);
    check('a changed name is the rename (PUT /api/styles/1), written before the Styles save',
        writes[0] === 'PUT /api/styles/1' && writes.some(w => /\/flow\/save$/.test(w)), writes.join(' · '));
    const save = page.since(from).find(c => /\/flow\/save$/.test(c.url));
    check('Save style is the Styles save (flow/save) of the step\'s draft', !!save && Array.isArray(save.body.cells) && save.body.cells.length > 0,
        save ? JSON.stringify(save.body).slice(0, 200) : 'no flow/save');
    check('and Setup is back on its steps, the one to do now Run it', page.S.tab === 'setup' && !page.S.setupView &&
        /<li class="pd-sus now"><span class="pd-susn">3<\/span>/.test(page.root.innerHTML), 'tab ' + page.S.tab + ' view ' + page.S.setupView);
}

// ── Setup's Change…: the process sheet again, titled Edit process ──────────
//
// SAVE WRITES ONLY WHAT MOVED, through Settings' own doors (savePositions,
// saveRouting, writePartSet): the sheet filled in from what was read, one
// change, and that change's write alone.
async function pinEditProcess() {
    L.push('', '## Setup › Change… (Edit process): Save writes only what moved, through Settings\' doors');
    const run = async (title, change) => {
        const page = await T.openPage(pressA(c => { c.styles = []; c.presets = []; }, NOT_RUN));
        page.click('root', { act: 'setup-edit' });
        await page.settle();
        const head = page.scrim.querySelector('.layer-head');
        const sp = page.byId('sp-proc');
        const chips = key => (sp.querySelectorAll('.setpick-box').find(b => b.getAttribute('data-box') === key) || { querySelectorAll: () => [] })
            .querySelectorAll('.setpick-n').map(n => n.textContent).join();
        if (!change) {
            L.push('  the sheet: ' + JSON.stringify(head ? head.childNodes[0].textContent : '') + ' · boxes hold ' +
                ['positions', 'source', 'staging', 'destination'].map(k => k + ' ' + chips(k)).join(', ') +
                ' · name ' + JSON.stringify((page.scrim.querySelector('[data-f="name"]') || {}).value) +
                ' · screens ' + JSON.stringify(T.text(String((page.scrim.querySelector('.layer-body') || {}).innerHTML || '').replace(/[\s\S]*<label>Operator screens/, 'Operator screens').slice(0, 200)).slice(0, 60)));
        }
        if (change) change(page);
        await page.settle();
        const from = page.mark();
        await T.sheetOK(page);
        L.push('  ' + title + ': ' + (lines(page, from).filter(l => !/^GET /.test(l)).join(', ') || 'no write') +
            ' · sheet ' + (page.sheetOpen() ? 'open' : 'closed') + ' · notices ' + JSON.stringify(page.notices.map(n => n.text)));
    };
    await run('nothing changed, Save');
    await run('Outbound + NEW_1, Save', page => T.setPick(page, 'proc', 'destination', 'NEW_1'));
    await run('the name typed Press A2, Save', page => page.type('name', 'Press A2'));
    await run('the part set less PART-11, Save', page => T.npk(page, 'scrim', 'parts', 'PART-11', 'drop'));
}

// ── the Add process sheet: one sheet makes a line (ruling 2026-11-15) ───────
async function pinAddProcess() {
    L.push('', '## Add process');
    const page = await T.openPage();
    page.click('root', { act: 'add-process' });
    await page.settle();
    const body = page.scrim.querySelector('.layer-body');
    const html = String(body ? body.innerHTML : '');
    const fields = [...html.matchAll(/<label>([^<]*)<small>([^<]*)<\/small>/g)].map(m => m[1] + ' (' + T.decode(m[2]) + ')');
    L.push('  fields ' + JSON.stringify(fields));
    const sp = page.byId('sp-proc');
    const boxes = sp ? sp.querySelectorAll('.setpick-box').map(b => b.getAttribute('data-box') + (b.classList.contains('is-needed') ? ' (needed)' : '')) : [];
    L.push('  the set pick: ' + JSON.stringify(boxes) + ' · open on ' + JSON.stringify(sp && (sp.querySelectorAll('.setpick-box').find(b => b.classList.contains('is-on')) || { getAttribute: () => '' }).getAttribute('data-box')));
    L.push('  still to pick: ' + JSON.stringify(T.text((page.byId('pd-still') || {}).innerHTML || '')));
    page.type('name', 'Press N');
    const from = page.mark();
    await T.sheetOK(page);
    const writes = page.since(from).filter(c => c.method !== 'GET');
    check('Create on a name alone makes the process and its screen, named after it, and nothing else',
        writes.map(c => c.method + ' ' + c.url).join(' · ') === 'POST /api/processes · POST /api/operator-stations' &&
        writes[1].body.name === 'Press N' && writes[1].body.process_id === 5,
        writes.map(c => c.method + ' ' + c.url + ' ' + JSON.stringify(c.body && c.body.name)).join(' · ') || 'nothing written');
    L.push('#    then it opens on ' + page.S.tab + ' · tabs ' + JSON.stringify(tabsOf(page)));
}

// Smoke (browser walk 2026-11-11): a style opened on a process whose screen
// has no position said "Add a position below", and nothing was below it.
async function bugNothingBelow() {
    L.push('', '## the empty flow bar names a door that is on the screen');
    const page = await T.openPage(Object.assign(pressA(c => {
        c.cell.positions = [];
        c.styles = c.styles.slice(0, 1).map(st => Object.assign(st, { claims: [], claim_count: 0, claim_modes: [], last_run: '' }));
    }, NOT_RUN), { hash: '#style=1' }));
    await page.settle();
    const bar = T.text(String((page.byId('pd-bar') || {}).innerHTML || ''));
    const foot = /data-act="add-position"/.test(page.root.innerHTML);
    check('no position to add: the bar does not send the engineer below (' + JSON.stringify(bar) + ')',
        foot || !/below/.test(bar), 'no "+ Add a position" is drawn');
}

async function run(t) {
    T = t;
    T.setCurrent('cold-start.txt');
    L.push('# The cold start (brief U2): the U2 bugs, then the Setup tab, the tabs, the rail, New style and',
        '# Add a position. "ok" is what must hold; "BUG" is a bug back.');
    await bugStagingDefault();
    await bugDraftBar();
    await bugDraftBack();
    await bugDraftLeave();
    await bugDeadEnd();
    await bugDeadEndSlot();
    await bugNothingBelow();
    await bugHmiNoRecipe();
    await pinStyleFirst();
    await pinSetup();
    await pinFirstStyle();
    await pinEditProcess();
    await pinAddProcess();
    T.out['cold-start.txt'] = L.join('\n') + '\n';
}

module.exports = { run };
