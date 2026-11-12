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
// of the fixture's own; page overrides the page data (the process row).
function pressA(edit, pageEdit) {
    const composer = clone(T.FX.composer);
    if (edit) edit(composer);
    const page = clone(T.FX.page);
    if (pageEdit) pageEdit(page);
    return {
        page: page,
        extra: (method, url) => (method === 'GET' && /^\/api\/processes\/1\/composer$/.test(url.split('?')[0])
            ? { body: clone(composer) } : undefined),
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
        if (page.rows('pd-pop', 'data-freepos').length) T.pickRow(page, 'pd-pop', 'data-freepos', node);
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
    const use = page.rows('pd-pop', 'data-adduse');
    check('the popover offers places from the plant to add and use', use.length > 0, 'nothing to pick');
    if (!use.length) return;
    const from = page.mark();
    const row = use.find(r => r.dataset.adduse === 'STG_9') || use[0];
    const name = row.dataset.adduse;
    page.fire(row, 'click');
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

// F36: HMI setup with no recipe says so, and offers New recipe there.
async function bugHmiNoRecipe() {
    L.push('', '## F36: Settings › HMI setup on a process with no recipe');
    const o = pressA(c => { c.presets = []; });
    const presetsView = { presets: [], styles_with_flow: 2, candidates: [{
        shape_key: 'k1', suggested_name: 'Found shape', mode: '', where: 'POS_A1 / POS_A3',
        shape: clone(T.FX.server.presetsView.presets[0].shape), members: [1, 2], member_names: ['Style A', 'Style B'],
    }] };
    const extra = o.extra;
    o.extra = (m, u, b) => (m === 'GET' && /\/api\/processes\/1\/presets$/.test(u) ? { body: presetsView } : extra(m, u, b));
    o.hash = '#tab=settings';
    const page = await T.openPage(o);
    await page.settle();
    const html = page.root.innerHTML.slice(page.root.innerHTML.indexOf('<div class="pd-sheet'));
    const at = html.indexOf('HMI setup');
    const hmi = T.text(html.slice(Math.max(0, at), at + 1200));
    L.push('#    ' + JSON.stringify(hmi.slice(0, 160)));
    check('the HMI setup row says the process has no recipe', /no recipe/i.test(hmi), 'nothing says so');
    check('and offers New recipe there', /data-act="new-recipe"/.test(html), 'no New recipe on Settings');
    check('"Found in your flows" is beside it', /Found in your flows/.test(html), 'not on Settings');
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
            const first = (/<option value="([^"]*)"[^>]*>([^<]*)</.exec(chooser.innerHTML) || []);
            L.push('  the chooser opens on ' + JSON.stringify(first[2] || '') + ' (value ' + JSON.stringify(first[1] || '') + ')');
        }
    }
}

async function run(t) {
    T = t;
    T.setCurrent('cold-start.txt');
    L.push('# The cold start (brief U2): the U2 bugs, then the Setup tab, the tabs, the rail, New style and',
        '# Add a position. "ok" is what must hold; "BUG" is a bug back.');
    await bugStagingDefault();
    await bugDraftBar();
    await bugDraftBack();
    await bugDeadEnd();
    await bugHmiNoRecipe();
    await pinStyleFirst();
    T.out['cold-start.txt'] = L.join('\n') + '\n';
}

module.exports = { run };
