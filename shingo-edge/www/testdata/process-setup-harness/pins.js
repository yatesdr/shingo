// pins.js — the Processes page's process-setup characterisation (U0-page).
//
// Run by www/processes_desktop_setup_pins_test.go, which compares what this
// prints against www/testdata/process-setup-page/*. It prints ONE JSON object,
// golden file name -> text, and nothing else on stdout.
//
// WHAT THIS PINS IS WHAT THE PAGE DOES ON TOP OF THE MODEL: which options a
// picker offers, what the positions table says in each cell, which Advanced
// fields the sheet draws and counts, and the requests each setup act sends,
// in order. The model's own grid (rowColumns, advancedShows, advancedSet) and
// desktop-bodies.js's builders are pinned on their own; this is the page's
// use of them, so a rework that changes either the page or the model shows up
// here as the cell that moved.
//
// One synthetic plant (fixture.json): Press A, every swap mode the composer
// offers on both roles, a routing set with source, staging and destination
// rows and a node group. Its answers are fixed: the server here never changes
// what it says because of a write, so a sequence is "what the page sends
// given these answers", which is what a characterisation can promise.
//
// No mode is spelled in this file: the modes come from the fixture and the
// model's own MODES (modeLabels), so the swap-mode-literal rule has nothing
// to find and a new mode reaches these pins through the fixture alone.

'use strict';

const fs = require('fs');
const path = require('path');
const H = require('./page-harness.js');

const FX = JSON.parse(fs.readFileSync(path.join(__dirname, 'fixture.json'), 'utf8'));
const out = {};

// ── the fixture's server ─────────────────────────────────────────────────────
const EMPTY_COMPOSER = { styles: null, routing: [], cell: { positions: [], groups: {} } };
function server() {
    let previews = 0;
    const id = (u, re) => { const m = re.exec(u); return m ? m[1] : null; };
    // A preview of the RUNNING style is validated and fingerprinted and never
    // planned, so it answers no order_count (owner ruling R3; applyPreview
    // reads the absence); any other style's plans to four orders.
    const preview = body => {
        const running = (FX.page.processes.find(p => p.id === 1) || {}).active_style_id === (body && body.to_style_id);
        const out = { fingerprint: 'fp-' + (++previews), findings: [] };
        if (running) out.running = true; else out.order_count = 4;
        return out;
    };
    return (method, url, body) => {
        const u = url.split('?')[0];
        const pid = id(u, /^\/api\/processes\/(\d+)\//);
        const routes = [
            ['GET', /^\/api\/processes\/\d+\/composer$/, () => (pid === '1' ? FX.composer : EMPTY_COMPOSER)],
            ['POST', /^\/api\/processes\/\d+\/flow\/preview$/, () => preview(body)],
            ['POST', /^\/api\/processes\/\d+\/flow\/save$/, () => ({ status: 'ok' })],
            ['POST', /^\/api\/processes\/\d+\/flow\/mint$/, () => ({ style_id: 9, style_name: 'PART-1 PART-3', minted: true })],
            ['GET', /^\/api\/processes$/, () => FX.page.processes],
            ['POST', /^\/api\/processes$/, () => ({ id: 5 })],
            ['PUT', /^\/api\/processes\/\d+$/, () => ({ status: 'ok' })],
            ['PATCH', /^\/api\/processes\/\d+$/, () => ({ status: 'ok' })],
            ['POST', /^\/api\/processes\/\d+\/containment-setting$/, () => ({ status: 'ok' })],
            ['GET', /^\/api\/processes\/\d+\/styles$/, () => FX.page.styles.filter(s => String(s.process_id) === pid)],
            ['GET', /^\/api\/processes\/\d+\/routing-nodes$/, () => ({ rows: pid === '1' ? FX.server.routingNodes : [] })],
            ['POST', /^\/api\/processes\/\d+\/routing-nodes$/, () => ({ id: 110 })],
            ['PUT', /^\/api\/processes\/\d+\/routing-nodes$/, () => ({ status: 'ok' })],
            ['PATCH', /^\/api\/processes\/\d+\/routing-nodes\/\d+$/, () => ({ status: 'ok' })],
            // A removal answers what it orphans, by name (owner ruling 2026-10-30).
            ['DELETE', /^\/api\/processes\/\d+\/routing-nodes\/\d+$/, () => ({
                warning: ((FX.server.routingNodes.find(r => String(r.id) === id(u, /routing-nodes\/(\d+)$/)) || {}).core_node_name || '?') +
                    ' is still named by Style A.',
            })],
            ['GET', /^\/api\/processes\/\d+\/payloads$/, () => (pid === '1' ? FX.server.partSet : [])],
            ['PUT', /^\/api\/processes\/\d+\/payloads$/, () => ({ status: 'ok' })],
            ['GET', /^\/api\/processes\/\d+\/presets$/, () => FX.server.presetsView],
            ['POST', /^\/api\/processes\/\d+\/presets$/, () => ({ id: 8 })],
            ['GET', /^\/api\/processes\/\d+\/station-counters$/, () => []],
            ['GET', /^\/api\/processes\/\d+\/counting-slots$/, () => ({ slots: [] })],
            ['GET', /^\/api\/processes\/\d+\/catid-health$/, () => ({ items: [] })],
            ['GET', /^\/api\/operator-stations$/, () => FX.page.stations],
            ['POST', /^\/api\/operator-stations$/, () => ({ id: 31 })],
            ['PUT', /^\/api\/operator-stations\/\d+$/, () => ({ status: 'ok' })],
            ['GET', /^\/api\/operator-stations\/\d+\/claimed-nodes$/, () => FX.page.stationNodes[id(u, /stations\/(\d+)\//)] || []],
            ['PUT', /^\/api\/operator-stations\/\d+\/claimed-nodes$/, () => ({ status: 'ok', kept: [] })],
            ['POST', /^\/api\/operator-stations\/\d+\/claimed-nodes\/impact$/, () => ({ warning: '' })],
            ['GET', /^\/api\/process-nodes$/, () => FX.server.processNodes],
            ['POST', /^\/api\/process-nodes\/\d+\/curtain-setting$/, () => ({ status: 'ok' })],
            ['GET', /^\/api\/core-nodes$/, () => FX.server.coreNodes],
            ['GET', /^\/api\/payload-catalog$/, () => FX.server.payloadCatalog],
            ['POST', /^\/api\/styles$/, () => ({ id: 6 })],
            ['PUT', /^\/api\/styles\/\d+$/, () => ({ status: 'ok' })],
        ];
        for (const [m, re, fn] of routes) if (m === method && re.test(u)) return { body: fn() };
        return undefined;
    };
}


function openPage(o) {
    o = o || {};
    return H.open({ page: FX.page, search: '?process=1', hash: o.hash || '', serve: server() });
}

// The rows the positions table draws, in its order: a position that is on
// the flow with a mode.
function tableRows(S) {
    return S.model.positions.map(p => p.core_node_name).filter(n => S.model.cells[n].on && S.model.cells[n].mode);
}
const modeRole = (S, n) => S.model.cells[n].mode + '/' + S.model.cells[n].role;

// One position per (mode, role), in table order: the Advanced pins are per
// choreography, and a second position with the same pair says nothing new.
function onePerModeRole(S) {
    const seen = new Set();
    return tableRows(S).filter(n => { const k = modeRole(S, n); if (seen.has(k)) return false; seen.add(k); return true; });
}

// A row of a popover the page drew, found by its attribute and value and
// clicked through the listener the page bound to it.
function pickRow(page, popID, attr, value) {
    const key = attr.replace(/^data-/, '').replace(/-([a-z])/g, (_, x) => x.toUpperCase());
    const row = page.rows(popID, attr).find(r => r.dataset[key] === String(value));
    if (!row) throw new Error('#' + popID + ' has no [' + attr + '="' + value + '"]: ' + (page.byId(popID) || {}).innerHTML);
    page.fire(row, 'click');
}

// ── 1 · picker option lists ──────────────────────────────────────────────────
//
// Every kind the table draws a control for — position, role, mode, the four
// mode-dependent chips (col:<column>:<key>), source, dest, and one via:<i> per
// route point (via:0 on an empty route) — asked of optionsFor for every row.
// A col kind the row draws no chip for answers []; it is listed so a chip
// that appears or goes is a line that changes. `part` is not optionsFor's: it
// opens the part popover (see the report).
const COL_KINDS = ['col:partner:paired', 'col:partner:secondPaired', 'col:staging:staging', 'col:staging:parkOld'];
function pickerKinds(S, n) {
    const route = S.model.cells[n].keyRoute || [];
    const via = route.length ? route.map((_, i) => 'via:' + i) : ['via:0'];
    return ['position', 'role', 'mode'].concat(COL_KINDS, ['source', 'dest'], via);
}
function fmtOption(o) {
    if (o.note !== undefined && o.value === undefined) return 'note=' + JSON.stringify(o.note);
    const bits = ['value=' + JSON.stringify(o.value), 'label=' + JSON.stringify(o.label), 'on=' + (o.on ? 'yes' : 'no'),
        'icon=' + (o.icon ? 'present' : 'absent')];
    if (o.use) bits.push('use=' + JSON.stringify(o.use));
    if (o.where) bits.push('where=' + JSON.stringify(o.where));
    bits.push('action=' + JSON.stringify(o.action));
    return bits.join('  ');
}
async function pinPickers() {
    const page = await openPage();
    const optionsFor = page.pd('optionsFor'), emptyWord = page.pd('pickerEmptyWord');
    const S = page.S;
    const lines = ['# optionsFor(node, kind) for every table row of ' + S.model.styleName + ', in table order.',
        '# The last line of a kind is the sentence openPicker draws when no option carries a value.'];
    for (const n of tableRows(S)) {
        for (const kind of pickerKinds(S, n)) {
            const head = n + '  ' + kind;
            const opts = optionsFor(n, kind);
            if (!opts.length) lines.push(head + '  (no options)');
            opts.forEach((o, i) => lines.push(head + '  #' + i + '  ' + fmtOption(o)));
            if (!opts.some(o => o.value)) lines.push(head + '  empty-sentence=' + JSON.stringify(emptyWord(kind)));
        }
    }
    out['picker-options.txt'] = lines.join('\n') + '\n';
}

// ── 2 · the positions table, as text ─────────────────────────────────────────
const ENT = { '&amp;': '&', '&lt;': '<', '&gt;': '>', '&quot;': '"', '&times;': '×', '&minus;': '−', '&#8962;': '⌂' };
const decode = s => String(s).replace(/&[#\w]+;/g, e => (e in ENT ? ENT[e] : e));
const text = html => decode(String(html).replace(/<[^>]*>/g, ' ')).replace(/\s+/g, ' ').trim();
// A cell is its text, then the titles that carry values, then its controls:
// each pick control's kind, as a chip (sel) or a dim placeholder (dash), with
// ! when the chip is drawn bad; and every other act it carries.
function cellText(td) {
    const titles = [...td.matchAll(/title="([^"]*)"/g)].map(m => decode(m[1]));
    const ctl = [];
    for (const m of td.matchAll(/<button class="([^"]*)" data-act="([^"]*)"([^>]*)>/g)) {
        const cls = m[1].split(/\s+/), act = m[2];
        const kind = (/data-kind="([^"]*)"/.exec(m[3]) || [])[1];
        if (act === 'pick') ctl.push(kind + ':' + (cls.includes('pd-blank') ? 'dash' : 'sel') + (cls.includes('bad') ? '!' : ''));
        else ctl.push(act + (cls.includes('set') ? ':set' : ''));
    }
    let s = JSON.stringify(text(td));
    if (titles.length) s += ' title=' + JSON.stringify(titles);
    if (ctl.length) s += ' ctl=[' + ctl.join(' ') + ']';
    return s;
}
function tableGrid(html) {
    const head = [...(/<thead>([\s\S]*?)<\/thead>/.exec(html) || ['', ''])[1].matchAll(/<th>([\s\S]*?)<\/th>/g)].map(m => text(m[1]));
    const body = (/<tbody>([\s\S]*?)<\/tbody>/.exec(html) || ['', ''])[1];
    const lines = ['head: ' + head.join(' ‖ ')];
    for (const tr of body.matchAll(/<tr([^>]*)>([\s\S]*?)<\/tr>/g)) {
        const row = (/data-row="([^"]*)"/.exec(tr[1]) || [])[1];
        const tds = [...tr[2].matchAll(/<td[^>]*>([\s\S]*?)<\/td>/g)].map(m => m[1]);
        if (!row) { lines.push('tail: ' + tds.map(cellText).join(' ‖ ')); continue; }
        lines.push(row + ': ' + tds.map((td, i) => (head[i] || '?') + '=' + cellText(td)).join(' ‖ '));
    }
    const foot = /<div class="pd-posfoot"[\s\S]*<\/div><\/div>$/.exec(html);
    lines.push('footer: ' + (foot ? JSON.stringify(text(foot[0])) : '(none)'));
    return lines;
}
async function pinTable() {
    const page = await openPage();
    const lines = ['# positionsTable() for each style of Press A (selected by a click on its rail row): one line',
        '# per row, cells in heading order. A cell: its text, title=[…] the titles that carry values,',
        '# ctl=[…] its controls (kind:sel a chip, kind:dash a dim placeholder, ! drawn bad).'];
    for (const st of page.S.composer.styles) {
        page.click('root', { style: String(st.id) });
        await page.settle();
        if (page.S.styleID !== st.id) throw new Error('the rail click did not open ' + st.name);
        lines.push('', '## ' + st.name);
        for (const l of tableGrid(page.pd('positionsTable')())) lines.push(l);
    }
    out['positions-table.txt'] = lines.join('\n') + '\n';
}

// ── 3 · the Advanced field set ───────────────────────────────────────────────
//
// advShows(key) for every key ADV_SECTIONS draws, per (mode, role), in two
// scopes — one position (the row's gear) and a recipe group of that position
// (a group card's Advanced, over a one-group draft) — each with strict_pack
// off and on, which is the one key whose answer depends on another's value.
//
// THE GROUP SCOPE'S DRAFT IS SYNTHETIC: one group whose ★ cell is the row, laid
// over the style's own model. A real recipe draft has groups only on its
// recipe's slots, so it could not put every (mode, role) under the group
// sheet; advShows reads only the group index, the ★ node and the model, which
// this is.
async function pinAdvanced() {
    const page = await openPage();
    const pd = page.pd, S = page.S;
    const keys = pd('ADV_SECTIONS').reduce((all, s) => all.concat(s[1]), []);
    const shows = ['# advShows(key) per scope × strict_pack × (mode/role) × key, in ADV_SECTIONS order.',
        '# scope=position is the row\'s gear (openAdvanced); scope=group is a group card\'s Advanced',
        '# (openGroupAdvanced) over a one-group draft whose ★ cell is that row (GROUP_ADV_KEYS applies).'];
    const scopes = [
        ['position', n => page.click('root', { act: 'advanced', node: n })],
        ['group', n => {
            S.shapeDraft = { groups: [{ of: n, name: 'Group ' + n, spots: [] }], focus: null, hover: null, refusal: '' };
            page.click('root', { act: 'gadv', g: '0' });
        }],
    ];
    for (const [scope, openSheet] of scopes) {
        for (const strict of [false, true]) {
            for (const n of onePerModeRole(S)) {
                openSheet(n);
                if (!S.adv || S.adv.node !== n) throw new Error('the ' + scope + ' sheet did not open for ' + n);
                S.adv.a.strict_pack = strict;
                for (const k of keys) {
                    shows.push(scope + '  strict_pack=' + (strict ? 'on ' : 'off') + '  ' + n + ' ' + modeRole(S, n) + '  ' + k +
                        '  ' + (pd('advShows')(k) ? 'shown' : 'hidden'));
                }
                page.click('scrim', { act: 'adv-cancel' });
                S.shapeDraft = null;
            }
        }
    }
    out['advanced-shows.txt'] = shows.join('\n') + '\n';

    // advancedCount(node) — the row's "N set" badge — for every row of the
    // all-default style (A) and the all-set style (B); for a row with any set,
    // the sheet its gear opens: each drawn section's title and "N set" count,
    // and their total, which is the number the badge is there to forecast.
    const count = ['# advancedCount(node) per style and row; for a row with any set, the sections its sheet draws',
        '# with their "N set" count, and the sheet\'s total beside the badge\'s.'];
    for (const st of S.composer.styles) {
        page.click('root', { style: String(st.id) });
        await page.settle();
        count.push('', '## ' + st.name);
        for (const n of tableRows(page.S)) {
            const badge = pd('advancedCount')(n);
            if (!badge) { count.push(n + ' ' + modeRole(page.S, n) + '  advancedCount=0'); continue; }
            page.click('root', { act: 'advanced', node: n });
            const secs = [...page.scrim.innerHTML.matchAll(/<div class="pd-sec"><div class="pd-lbl">([^<]*)(?:<span class="cnt">(\d+) set<\/span>)?<\/div>/g)];
            const total = secs.reduce((t, m) => t + Number(m[2] || 0), 0);
            count.push(n + ' ' + modeRole(page.S, n) + '  advancedCount=' + badge + '  sheet total=' + total);
            for (const m of secs) count.push('    section ' + JSON.stringify(decode(m[1])) + '  ' + (m[2] || 0) + ' set');
            page.click('scrim', { act: 'adv-cancel' });
        }
    }
    out['advanced-count.txt'] = count.join('\n') + '\n';
}

// ── 4 · request sequences ────────────────────────────────────────────────────
//
// Each act is driven the way an engineer drives it: a click dispatched to the
// page's own handler (#pd-root's onClick, #pd-scrim's onAdvClick), a row of a
// popover the page drew clicked through the listener it bound, a field typed
// where the page reads it (a sheet's [data-f], Settings' [data-st] and curtain
// inputs through their own input listeners). A golden lists every request the
// act made — method, url, and the parsed body indented under it — then
// whether the sheet is still open and its status line.
//
// ONE BODY, ONE GOLDEN. Most acts end by re-reading the process, which
// previews the running style again with exactly the body the boot preview
// sent. That body is written out once, in requests-open-process.txt, and
// named everywhere else, so a change to it is one diff in one file and not
// the same diff in nine.
let bootPreview = null;
function fmtCalls(calls) {
    const lines = [];
    calls.forEach((c, i) => {
        lines.push(String(i + 1).padStart(3) + '  ' + c.method + ' ' + c.url);
        if (c.body === undefined) return;
        const raw = JSON.stringify(c.body);
        if (bootPreview && raw === bootPreview.raw && c.url === bootPreview.url) {
            lines.push('       (the boot preview\'s body, as requests-open-process.txt pins it)');
            return;
        }
        for (const l of JSON.stringify(c.body, null, 2).split('\n')) lines.push('       ' + l);
    });
    return lines;
}
function actLines(title, page, from, note) {
    const lines = ['## ' + title];
    if (note) for (const n of [].concat(note)) lines.push('# ' + n);
    for (const l of fmtCalls(page.since(from))) lines.push(l);
    lines.push('-- sheet ' + (page.sheetOpen() ? 'open' : 'closed') + (page.status() ? ', status ' + JSON.stringify(page.status()) : ''));
    return lines;
}
// A picker in a sheet answers on the scrim; one on a page answers on #pd-root.
function npk(page, host, key, name, how) {
    page.click(host, { npk: how || 'add', npkkey: key, npkname: name });
}
async function sheetOK(page) {
    page.click('scrim', { act: 'sheet-ok' });
    await page.settle();
}
// Settings' typed fields: the page binds an input listener to each input it
// finds by [data-st] or input[data-cn]; typing is a value and that event.
function typeSetting(page, sel, match, value) {
    const el = page.root.querySelectorAll(sel).find(match);
    if (!el) throw new Error('Settings draws no ' + sel + ' for that field');
    el.value = value;
    page.fire(el, 'input');
}

async function actOpenProcess() {
    const page = await openPage();
    out['requests-open-process.txt'] = actLines('boot on ?process=1: the composer read, each screen\'s claimed nodes, ' +
        'then the first preview', page, 0).join('\n') + '\n';
    const pv = page.calls.find(c => /\/flow\/preview$/.test(c.url));
    if (!pv) throw new Error('boot sent no preview');
    bootPreview = { url: pv.url, raw: JSON.stringify(pv.body) };
}

async function actFlow() {
    const page = await openPage();
    const pd = page.pd, S = page.S;
    // A pick is a click on the chip and a click on the option's row. The
    // chip is clicked as a chip: a dim placeholder first redraws itself into
    // one (redrawPositionsTable's outerHTML write), which this DOM does not
    // follow, and the options it then opens are the same.
    const pick = (n, kind, value) => {
        page.click('root', { act: 'pick', node: n, kind: kind });
        const i = pd('optionsFor')(n, kind).findIndex(o => o.value === value);
        if (i < 0) throw new Error(n + ' ' + kind + ' offers no ' + value);
        pickRow(page, 'pd-pop', 'data-opt', i);
    };
    let from = page.mark();
    pick('POS_A5', 'source', 'SRC_1');
    pick('POS_A1', 'via:0', 'LM_1');
    await page.settle();
    const lines = actLines('flow preview: two picker edits (POS_A5 source → SRC_1, POS_A1 key route + LM_1), ' +
        'debounced into one preview', page, from);
    lines.push('-- bar ' + pd('M')().bar(S.model).tone + ', dirty=' + pd('dirty')());
    from = page.mark();
    page.click('root', { act: 'save' });
    await page.settle();
    lines.push('', ...actLines('Save style: flow/save on the last preview\'s fingerprint, then the re-open it does', page, from));
    out['requests-flow.txt'] = lines.join('\n') + '\n';
}

async function actStyleCreate() {
    const page = await openPage();
    const from = page.mark();
    page.click('root', { act: 'new-style' });
    page.type('name', 'Style C');
    page.type('catid', 'CAT-C');
    await sheetOK(page);
    out['requests-style-create.txt'] = actLines('+ New style with no recipe: a typed name and CATID', page, from).join('\n') + '\n';
}

async function actMint() {
    const page = await openPage();
    const from = page.mark();
    page.click('root', { act: 'new-style' });
    const chooser = page.byId('pd-newrecipe');
    if (!chooser) throw new Error('New style draws no recipe chooser');
    chooser.value = '7';
    page.fire(chooser, 'change');
    await page.settle();
    page.type('slot0', 'PART-1');
    page.type('slot1', 'PART-3');
    page.type('catid', 'CAT-M');
    await sheetOK(page);
    out['requests-flow-mint.txt'] = actLines('+ New style, Recipe One (id 7) picked in the chooser: a part per slot and a ' +
        'CATID; no spot moved', page, from).join('\n') + '\n';
}

async function actAddProcess() {
    const page = await openPage();
    const from = page.mark();
    page.click('root', { act: 'add-process' });
    await page.settle();
    page.type('name', 'Press N');
    page.type('description', 'a new press');
    page.type('screen', 'Screen N');
    page.click('scrim', { act: 'add-pickgroup' }, { inside: ['.pd-modal'] });
    pickRow(page, 'pd-advpop', 'data-grpopt', 3);
    for (const n of ['NEW_1', 'NEW_2']) npk(page, 'scrim', 'positions', n);
    npk(page, 'scrim', 'rs_source', 'SRC_2');
    npk(page, 'scrim', 'rs_source', 'GRP_A');
    npk(page, 'scrim', 'rs_staging', 'STG_9');
    npk(page, 'scrim', 'rs_destination', 'DST_9');
    npk(page, 'scrim', 'parts', 'PART-12');
    npk(page, 'scrim', 'parts', 'PART-1');
    await sheetOK(page);
    out['requests-add-process.txt'] = actLines('Add process: name, description, group Presses, screen Screen N on ' +
        'NEW_1 + NEW_2, sources SRC_2 then GRP_A, staging STG_9, destination DST_9, parts PART-12 then PART-1', page, from)
        .join('\n') + '\n';
}

async function actSettings() {
    const page = await openPage({ hash: '#tab=settings' });
    const S = page.S;
    const from = page.mark();
    const st = (key, value) => typeSetting(page, '[data-st]', el => el.dataset.st === key && el.tagName === 'INPUT', value);
    st('name', 'Press A2');
    st('description', 'renamed');
    st('counter_plc_name', 'PLC_B');
    st('counter_tag_name', 'CountB');
    page.click('root', { act: 'st-toggle', st: 'counter_enabled' });
    page.click('root', { act: 'st-arm', arm: 'prompt' });
    page.click('root', { act: 'st-toggle', st: 'flow_composer_enabled' });
    page.click('root', { act: 'st-group' });
    pickRow(page, 'pd-stpop', 'data-grpopt', 4);
    page.click('root', { act: 'st-toggle', st: 'quality_hold_enabled' });
    page.click('root', { act: 'st-holddest' });
    await page.settle();
    pickRow(page, 'pd-stpop', 'data-holdopt', 'DST_2');
    // Two curtains switched on, each with both pointers and a release value
    // (one TRUE, one FALSE); every other node's draft is left as it was, so
    // only these two are sent.
    const nodes = S.curtainNodes;
    for (const [n, tag, val] of [[nodes[0], 'CurtainA1', 'true'], [nodes[2], 'CurtainA3', 'false']]) {
        page.click('root', { act: 'cu-toggle', node: String(n.id) });
        const cu = f => typeSetting(page, 'input[data-cn]', el => el.dataset.cn === String(n.id) && el.dataset.cf === f,
            f === 'plc' ? 'PLC_A' : tag);
        cu('plc');
        cu('tag');
        page.click('root', { act: 'cu-val', node: String(n.id), val: val });
    }
    page.click('root', { act: 'st-save' });
    await page.settle();
    const lines = actLines('Save settings: name, description, counter PLC and tag typed, counter off, auto-arm Prompt, ' +
        'the HMI gate off, group Welders, the quality hold on to DST_2, curtains on ' + nodes[0].name + ' and ' +
        nodes[2].name, page, from);
    lines.push('-- settings error ' + JSON.stringify(S.settingsError) + ', notice ' + JSON.stringify(S.settingsNotice));
    out['requests-settings-save.txt'] = lines.join('\n') + '\n';
}

async function actScreenSheet() {
    const lines = [];
    {
        const page = await openPage();
        const from = page.mark();
        page.click('root', { act: 'screen-edit', station: '11' });
        await page.settle();
        page.type('note', 'front HMI, moved');
        npk(page, 'scrim', 'positions', 'POS_A11');   // take one off
        npk(page, 'scrim', 'positions', 'NEW_3');     // and add one
        await sheetOK(page);
        lines.push(...actLines('Edit Screen A: the note, POS_A11 off and NEW_3 on; the removal is asked about first',
            page, from), '');
    }
    {
        const page = await openPage();
        const from = page.mark();
        page.click('root', { act: 'screen-add' });
        await page.settle();
        page.type('name', 'Screen C');
        page.type('note', 'spare');
        npk(page, 'scrim', 'positions', 'NEW_1');
        await sheetOK(page);
        lines.push(...actLines('Add operator screen: Screen C claiming NEW_1', page, from));
    }
    out['requests-screen-sheet.txt'] = lines.join('\n') + '\n';
}

async function actPartSet() {
    const lines = [];
    {
        const page = await openPage();
        // The Part chip opens the part popover; its rows answer through the
        // popover's own onclick (onPartPopClick).
        const openPop = async () => {
            page.click('root', { act: 'pick', node: 'POS_A1', kind: 'part' });
            await page.settle();
        };
        const pp = (how, code) => {
            const pop = page.byId('pd-pop');
            const row = page.rows('pd-partlist', 'data-pp').find(r => r.dataset.pp === how && r.dataset.code === code);
            if (!row) throw new Error('the part popover has no ' + how + ' ' + code + ': ' + (page.byId('pd-partlist') || {}).innerHTML);
            pop.onclick({ target: row, stopPropagation() {} });
        };
        await openPop();
        let from = page.mark();
        pp('add', 'PART-12');
        await page.settle();
        lines.push(...actLines('Part popover on POS_A1: + PART-12 from Core\'s catalog (joins the part set and lands ' +
            'on POS_A1)', page, from), '');
        await openPop();
        from = page.mark();
        pp('drop', 'PART-11');
        await page.settle();
        lines.push(...actLines('Part popover: × PART-11 (on no position, in no other flow)', page, from), '');
    }
    {
        const page = await openPage({ hash: '#tab=settings' });
        let from = page.mark();
        npk(page, 'root', 'parts', 'PART-13');
        await page.settle();
        lines.push(...actLines('Settings › Parts this process runs: + PART-13', page, from), '');
        from = page.mark();
        npk(page, 'root', 'parts', 'PART-11', 'drop');
        await page.settle();
        lines.push(...actLines('Settings › Parts this process runs: × PART-11', page, from));
    }
    out['requests-part-set.txt'] = lines.join('\n') + '\n';
}

async function actRouting() {
    const lines = [];
    const acts = [
        ['add SRC_2 to Sources (a name with no row: POST at the end of its role)', 'source', 'SRC_2', 'add'],
        ['add DST_9 to Destinations (a switched-off row: PATCH adopts it)', 'destination', 'DST_9', 'add'],
        ['× STG_3 out of Staging (DELETE, and the warning it answers)', 'staging', 'STG_3', 'drop'],
        ['click STG_1, already in Staging (the picker takes it out)', 'staging', 'STG_1', 'add'],
    ];
    for (const [title, role, name, how] of acts) {
        const page = await openPage({ hash: '#tab=settings' });
        const from = page.mark();
        npk(page, 'root', 'rs_' + role, name, how);
        await page.settle();
        const ls = actLines('Settings › Routing set: ' + title, page, from);
        ls.push('-- routing warning ' + JSON.stringify(page.S.routingWarning) + ', error ' + JSON.stringify(page.S.routingError));
        lines.push(...ls, '');
    }
    {
        const page = await openPage({ hash: '#tab=settings' });
        const from = page.mark();
        page.click('root', { act: 'rs-all-on', role: 'staging' });
        await page.settle();
        lines.push(...actLines('Settings › Routing set: Turn all 1 on (Staging)', page, from));
    }
    out['requests-routing-set.txt'] = lines.join('\n') + '\n';
}

async function actRecipe() {
    const lines = [];
    {
        const page = await openPage();
        const from = page.mark();
        page.click('root', { act: 'save-preset' });
        page.type('name', 'From A');
        await sheetOK(page);
        lines.push(...actLines('Save as recipe… on the saved Style A (from_style_id)', page, from), '');
    }
    {
        // + New recipe; + Add a position asks the choreography first (the
        // draft has none), then the position; Save recipe… (the cells form).
        // The choreography is the first the model would not block on.
        const page = await openPage();
        const pd = page.pd, S = page.S, M = pd('M')();
        let from = page.mark();
        page.click('root', { act: 'new-recipe' });
        await page.settle();
        const mode = Object.keys(M.modeLabels()).find(m =>
            M.bar(M.reduce(S.model, { type: 'addPosition', node: 'POS_A11', mode: m })).tone !== 'blocked');
        if (!mode) throw new Error('every choreography blocks a one-position recipe on POS_A11');
        page.click('root', { act: 'add-position' });
        pickRow(page, 'pd-pop', 'data-freemode', mode);
        pickRow(page, 'pd-pop', 'data-freepos', 'POS_A11');
        await page.settle();
        lines.push(...actLines('+ New recipe, then + Add a position: ' + mode + ' on POS_A11', page, from));
        lines.push('-- bar ' + M.bar(S.model).tone, '');
        from = page.mark();
        page.click('root', { act: 'shape-save' });
        page.type('name', 'Fresh');
        await sheetOK(page);
        lines.push(...actLines('Save recipe… on that draft (cells, no groups)', page, from), '');
    }
    {
        const page = await openPage();
        const S = page.S;
        let from = page.mark();
        page.click('root', { act: 'recipe-open', preset: '7' });
        page.click('root', { act: 'groups-setup' });
        await page.settle();
        lines.push(...actLines('Recipe One opened from the rail, then Set up groups', page, from));
        lines.push('-- groups ' + JSON.stringify((S.shapeDraft.groups || []).map(g => g.name + ' of ' + g.of)), '');
        from = page.mark();
        page.click('root', { act: 'shape-save' });
        await sheetOK(page);
        lines.push(...actLines('Save recipe… with groups (saveGroupedRecipe), the name as it opens', page, from));
    }
    out['requests-recipe-create.txt'] = lines.join('\n') + '\n';
}

(async () => {
    await actOpenProcess();
    await pinPickers();
    await pinTable();
    await pinAdvanced();
    await actFlow();
    await actStyleCreate();
    await actMint();
    await actAddProcess();
    await actSettings();
    await actScreenSheet();
    await actPartSet();
    await actRouting();
    await actRecipe();
    process.stdout.write(JSON.stringify(out));
})().catch(e => {
    process.stderr.write('process-setup pins: ' + ((e && e.stack) || e) + '\n');
    process.exit(1);
});
