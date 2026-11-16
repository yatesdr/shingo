// pins.js — the Processes page's process-setup characterisation (U0-page),
// and the P0 pins beside it (p0-page.js: the acts U0 left out, every write's
// refusals, the sheet stack), which run on this file's drivers and plant.
//
// Run by www/processes_desktop_setup_pins_test.go, which compares what this
// prints against www/testdata/process-setup-page/*. It prints ONE JSON object,
// golden file name -> text, and nothing else on stdout. `node pins.js --sites`
// prints instead the table of every fetch( site in the page and the goldens
// that reach it (no golden: its line numbers move with every edit).
//
// WHAT THIS PINS IS WHAT THE PAGE DOES ON TOP OF THE MODEL: which options a
// picker offers, what the positions table says in each cell, which settings
// Set for this style offers and counts, and the requests each setup act sends,
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
// THE STEADY STATE IS THE BUILDERS', NOT THIS FILE'S. steady.json is written by
// engine's TestSteady_PageHarnessFixture from the fixture's own claims, through
// the preview's draft path and the request-time builders; a preview asked with
// ?steps=1 gets, for each cell that matches one of its entries field for
// field (and on its Next carrier), that entry's legs, and nothing for any other
// cell. No step is written here.
const STEADY = JSON.parse(fs.readFileSync(path.join(__dirname, 'steady.json'), 'utf8'));
const STEADY_KEYS = ['core_node_name', 'role', 'swap_mode', 'payload_code', 'paired_core_node', 'second_paired_core_node',
    'inbound_source', 'inbound_staging', 'outbound_staging', 'outbound_destination'];
function steadyFor(body) {
    return ((body && body.cells) || []).map(c => STEADY.find(e => e.style_id === body.to_style_id &&
        STEADY_KEYS.every(k => (e.cell[k] || '') === (c[k] || '')) &&
        e.refill_by === ((c.advanced && c.advanced.refill_by) || ''))).filter(Boolean).map(e => e.steady);
}
// A RUNNING PRESS'S SCREENS HAVE BEEN OPENED. Setup is finished once the line
// can run and its screen has been opened, the screen's last_seen_at (owner
// ruling 2026-11-15); until then a process opens on Setup. Press A runs Style
// A, so its screens (and Press B's) are seen here, 30 s before the harness's
// fixed clock (page-harness.js: 2026-11-11 14:05:30, local). fixture.json
// carries no last_seen_at; the Setup pins (cold-start.js) take it away where
// they mean a screen never opened.
for (const st of FX.page.stations) if (!st.last_seen_at) st.last_seen_at = '2026-11-11T14:05:00';
const out = {};

// ── the fixture's server ─────────────────────────────────────────────────────
const EMPTY_COMPOSER = { styles: null, routing: [], cell: { positions: [], groups: {} } };
// extra(method, url, body), when given, is asked first: an act the base plant
// cannot reach (a health strip, a station counter, a loader gap) answers the
// routes it needs there, and every other route answers as it always has.
function server(extra) {
    let previews = 0;
    const id = (u, re) => { const m = re.exec(u); return m ? m[1] : null; };
    // A preview of the RUNNING style is validated and fingerprinted and never
    // planned, so it answers no order_count (owner ruling R3; applyPreview
    // reads the absence); any other style's plans to four orders.
    const preview = (body, url) => {
        const running = (FX.page.processes.find(p => p.id === 1) || {}).active_style_id === (body && body.to_style_id);
        const out = { fingerprint: 'fp-' + (++previews), findings: [] };
        if (running) out.running = true; else out.order_count = 4;
        if (/[?&]steps=1(&|$)/.test(url)) out.steady = steadyFor(body);
        return out;
    };
    return (method, url, body) => {
        const u = url.split('?')[0];
        const own = extra && extra(method, url, body);
        if (own) return own;
        const pid = id(u, /^\/api\/processes\/(\d+)\//);
        const routes = [
            ['GET', /^\/api\/processes\/\d+\/composer$/, () => (pid === '1' ? FX.composer : EMPTY_COMPOSER)],
            // The Styles poll's read: the two lists of the same answer.
            ['GET', /^\/api\/processes\/\d+\/composer\/lists$/, () => {
                const c = pid === '1' ? FX.composer : EMPTY_COMPOSER;
                return { styles: c.styles, presets: c.presets };
            }],
            ['POST', /^\/api\/processes\/\d+\/flow\/preview(\?steps=1)?$/, () => preview(body, url)],
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
            // The routing set's one write answers each removal's warning.
            ['PUT', /^\/api\/processes\/\d+\/routing-nodes$/, () => ({
                status: 'ok',
                warnings: ((body && (typeof body === 'string' ? JSON.parse(body) : body).remove) || [])
                    .map(r => r.core_node_name + ' is still named by Style A.'),
            })],
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
            // WarLink's PLCs: the counter's PLC pick and the curtains' PLC boxes.
            ['GET', /^\/api\/plcs$/, () => [{ name: 'PLC_A', connected: true }, { name: 'PLC_B', connected: false }]],
            ['GET', /^\/api\/payload-catalog$/, () => FX.server.payloadCatalog],
            ['POST', /^\/api\/styles$/, () => ({ id: 6 })],
            ['PUT', /^\/api\/styles\/\d+$/, () => ({ status: 'ok' })],
        ];
        for (const [m, re, fn] of routes) if (m === method && re.test(u)) return { body: fn() };
        return undefined;
    };
}


// o.page / o.search / o.extra replace the fixture's page data, the query and
// add routes; o.storage is this browser's localStorage at boot; o.answer(method, url, body), when given, may answer any call
// first (the refusal pins' one refused write).
function openPage(o) {
    o = o || {};
    const base = server(o.extra);
    const serve = o.answer ? (m, u, b) => o.answer(m, u, b) || base(m, u, b) : base;
    return H.open({ page: o.page || FX.page, search: o.search === undefined ? '?process=1' : o.search,
        hash: o.hash || '', serve: serve, storage: o.storage });
}

// The rows the positions table draws, in its order: a position that is on
// the flow with a mode.
function tableRows(S) {
    return S.model.positions.map(p => p.core_node_name).filter(n => S.model.cells[n].on && S.model.cells[n].mode);
}
const modeRole = (S, n) => S.model.cells[n].mode + '/' + S.model.cells[n].role;

// One position per (mode, role), in table order: the settings pins are per
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

// pickRow on the shared picker (slice 5: every chooser on the page is it): the
// row whose data-value is `value`, in the last picker the popover holds.
function pickerRow(page, popID, value) {
    const pop = page.byId(popID);
    const pickers = pop ? pop.querySelectorAll('.picker') : [];
    const p = pickers[pickers.length - 1];
    const row = p && p.querySelectorAll('.picker-row').find(r => r.getAttribute('data-value') === String(value));
    if (!row) throw new Error('#' + popID + '\'s picker has no row ' + value + ': ' + (pop || {}).innerHTML);
    row.click();
}

// New style's "Start from a recipe" is the shared picker (slice 5).
function pickRecipe(page, id) {
    page.click('scrim', { act: 'newstyle-recipe' }, { inside: ['.pd-modal'] });
    pickerRow(page, 'pd-advpop', id);
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
    const add = /<span class="pd-posadd"[\s\S]*?<\/span><\/span>/.exec(html);
    lines.push('add: ' + (add ? JSON.stringify(text(add[0])) : '(none)'));
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

// ── 3 · the settings a position or a group may set ───────────────────────────
//
// advShows(key) for every field the old Advanced sheet drew, per (mode, role),
// in two scopes — one position (Set for this style) and a recipe group of that
// position (its card's list, over a one-group draft) — each with strict_pack
// off and on, which is the one key whose answer depends on another's value.
//
// THE GROUP SCOPE'S DRAFT IS SYNTHETIC: one group whose ★ cell is the row, laid
// over the style's own model. A real recipe draft has groups only on its
// recipe's slots, so it could not put every (mode, role) under the group
// sheet; advShows reads only the group index, the ★ node and the model, which
// this is.
// THE KEYS ARE EVERY FIELD THE OLD ADVANCED SHEET DREW, in its order, so this
// golden reads across the change: the sheet went in slice 3 and Set for this
// style (and a group's list) offers by the same advShows. The carry-over and
// the tolerance are second values now (of Tool change, of strict pack), and
// board order is offered nowhere a stored claim can reach.
const ADV_FIELDS = ['allowed_payload_codes', 'reorder_point', 'auto_reorder', 'lineside_soft_threshold',
    'auto_request_payload', 'auto_push', 'keep_staged_node', 'evac_nodes', 'changeover_carryover_disposition',
    'strict_pack', 'pack_tolerance_pct', 'partial_dest', 'refill_by', 'curtain_required', 'auto_confirm', 'sequence'];

async function pinAdvanced() {
    const page = await openPage();
    const pd = page.pd, S = page.S;
    const shows = ['# advShows(key) per scope × strict_pack × (mode/role) × key, in the old Advanced sheet\'s order.',
        '# scope=position is a style\'s position (Set for this style, setCtx({node})); scope=group is a recipe',
        '# group (its card\'s list, setCtx({g})) over a one-group draft whose ★ cell is that row (GROUP_ADV_KEYS applies).',
        '# No position here marks a Tool change, so the carry-over (its second value) is hidden on every row.'];
    const scopes = [
        ['position', n => pd('setCtx')({ node: n })],
        ['group', n => {
            S.shapeDraft = { groups: [{ of: n, name: 'Group ' + n, spots: [] }], focus: null, hover: null, refusal: '' };
            return pd('setCtx')({ g: 0 });
        }],
    ];
    for (const [scope, ctxOf] of scopes) {
        for (const strict of [false, true]) {
            for (const n of onePerModeRole(S)) {
                const ctx = ctxOf(n);
                if (!ctx || ctx.node !== n) throw new Error('no ' + scope + ' settings for ' + n);
                ctx.a.strict_pack = strict;
                for (const k of ADV_FIELDS) {
                    shows.push(scope + '  strict_pack=' + (strict ? 'on ' : 'off') + '  ' + n + ' ' + modeRole(S, n) + '  ' + k +
                        '  ' + (pd('advShows')(k, ctx) ? 'shown' : 'hidden'));
                }
                S.shapeDraft = null;
            }
        }
    }
    out['advanced-shows.txt'] = shows.join('\n') + '\n';

    // THE ROW'S ADVANCED (round 5): every row's control, "Advanced" or
    // "Advanced · N set" (setCount), for the all-default style (A) and the
    // all-set style (B); for a row of B, what its sheet lists, each setting by
    // its name with its value control as text.
    const count = ['# The row\'s Advanced control per style and row, and for Style B (every setting set) what each',
        '# row\'s sheet lists: heading, then name = its value control as text.'];
    for (const st of S.composer.styles) {
        page.click('root', { style: String(st.id) });
        await page.settle();
        count.push('', '## ' + st.name);
        for (const n of tableRows(page.S)) {
            const cell = (new RegExp('data-act="adv-open" data-node="' + n + '">([^<]*)</button>').exec(page.root.innerHTML) || [])[1];
            count.push(n + ' ' + modeRole(page.S, n) + '  setCount=' + pd('setCount')(n) + '  control ' + JSON.stringify(cell ? decode(cell) : '(none)'));
            if (st.name !== 'Style B') continue;
            page.click('root', { act: 'adv-open', node: n });
            await page.settle();
            const sheet = String(page.scrim.innerHTML).split('<div class="layer-foot"')[0];
            for (const sec of sheet.split('<div class="pd-advsec">').slice(1)) {
                count.push('    · ' + decode((/<div class="pd-lbl">([^<]*)/.exec(sec) || [])[1] || ''));
                for (const row of sec.split('<div class="pd-advrow">').slice(1)) {
                    const name = decode((/<div class="pd-advname">([^<]*)/.exec(row) || [])[1] || '');
                    const val = text(row.slice(row.indexOf('<div class="pd-advval">')));
                    count.push('      ' + name + ' = ' + val);
                }
            }
            pd('closeSheet')();
            await page.settle();
        }
    }
    out['advanced-count.txt'] = count.join('\n') + '\n';
}

// ── 4 · request sequences ────────────────────────────────────────────────────
//
// Each act is driven the way an engineer drives it: a click dispatched to the
// page's own handler (#pd-root's onClick, #pd-scrim's onScrimClick), a row of a
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
// WHICH fetch( SITE EACH GOLDEN REACHES: every call a golden lists, with the
// page line it was sent from and the golden it is in. Not a golden itself (a
// line number moves with every edit to the page); `node pins.js --sites`
// prints it as the table the P0 report carries.
const SEEN = [];
let CURRENT = '';
function seen(c) { SEEN.push({ site: c.site, stack: c.stack, golden: CURRENT, method: c.method, url: c.url }); }
function fmtCalls(calls) {
    const lines = [];
    calls.forEach((c, i) => {
        seen(c);
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
    for (const l of P0.noticeLines(page.noticesSince(from))) lines.push('-- ' + l);
    lines.push('-- sheet ' + (page.sheetOpen() ? 'open' : 'closed') + (page.status() ? ', status ' + JSON.stringify(page.status()) : ''));
    return lines;
}
// A picker is the shared shell (shared/picker.js, under the node and part
// adapters) drawn into #npk-<key>: a name is picked by clicking its row, as an
// engineer does (a dropdown's list opens when its search box takes focus; a
// checklist's is open), and dropped by its chip's ×. host (the sheet or
// #pd-root) is where the picker sits; the row's click bubbles from the row as
// a browser's would.
function npk(page, host, key, name, how) {
    const box = page.byId('npk-' + key);
    if (!box) throw new Error('no picker ' + key + ' is drawn');
    if (how === 'drop') {
        const x = box.querySelectorAll('[data-drop]').find(b => b.getAttribute('data-drop') === name);
        if (!x) throw new Error(key + ' has no chip ' + name);
        x.click();
        return;
    }
    const q = box.querySelector('.picker-q');
    if (q) page.fire(q, 'focus');
    const row = box.querySelectorAll('.picker-row').find(r => r.getAttribute('data-value') === name);
    if (!row) throw new Error(key + ' draws no row ' + name);
    row.click();
}
// The positions and the routing set are the shared set pick
// (shared/set-pick.js) drawn into #sp-<key>: proc on the process sheet, st
// on Settings. A box is opened by a click on its title, then a name is ticked
// in the one list under the boxes, as an engineer does; a name is dropped by
// its chip's × in its box. A row behind "In use by other processes" or a
// group's "show" is opened first, as an engineer would have to.
function setPick(page, key, box, name, how) {
    const host = page.byId('sp-' + key);
    if (!host) throw new Error('no set pick ' + key + ' is drawn');
    const boxEl = () => host.querySelectorAll('.setpick-box').find(x => x.getAttribute('data-box') === box);
    if (!boxEl()) throw new Error('set pick ' + key + ' has no box ' + box);
    if (how === 'drop') {
        // A box past eight chips says "+ N more"; the chip is behind it.
        const chip = () => boxEl().querySelectorAll('[data-drop]').find(d => d.getAttribute('data-drop') === name);
        const more = boxEl().querySelector('[data-more]');
        if (!chip() && more && /more$/.test(more.textContent)) more.click();
        if (!chip()) throw new Error(key + '/' + box + ' has no chip ' + name);
        chip().click();
        return;
    }
    if (!boxEl().classList.contains('is-on')) boxEl().querySelector('.setpick-t').click();
    if (!boxEl().classList.contains('is-on')) throw new Error(key + '/' + box + ' did not open');
    const find = () => host.querySelectorAll('.picker-row').find(r => r.getAttribute('data-value') === name);
    for (let i = 0; i < 12 && !find(); i++) {
        const f = host.querySelectorAll('[data-later]').find(x => x.textContent === 'show') ||
            host.querySelectorAll('[data-fold]').find(x => x.getAttribute('aria-expanded') === 'false');
        if (!f) break;
        f.click();
    }
    const row = find();
    if (!row) throw new Error(key + '/' + box + ' draws no row ' + name);
    row.click();
}
async function sheetOK(page) {
    page.click('scrim', { act: 'sheet-ok' });
    await page.settle();
}
// Settings' Save is the shared settings page's bar (shared/settings.js): its
// own button, pressed as a person presses it.
async function stSave(page) {
    const b = page.root.querySelector('[data-set="save"]');
    if (!b) throw new Error('Settings draws no Save');
    b.click();
    await page.settle();
}
// The containment destination is the node adapter's picker, open in
// #pd-stpop: a node is picked by clicking its row.
function holdRows(page) {
    const pop = page.byId('pd-stpop');
    return pop ? pop.querySelectorAll('.picker-row') : [];
}
function holdPick(page, name) {
    const row = holdRows(page).find(r => r.getAttribute('data-value') === name);
    if (!row) throw new Error('#pd-stpop draws no row ' + name);
    row.click();
}
// Settings' typed fields: the page binds an input listener to each input it
// finds by [data-st] or input[data-cn]; typing is a value and that event.
function typeSetting(page, sel, match, value) {
    const el = page.root.querySelectorAll(sel).find(match);
    if (!el) throw new Error('Settings draws no ' + sel + ' for that field');
    el.value = value;
    page.fire(el, 'input');
}

// ── the acts' drivers ────────────────────────────────────────────────────────
//
// ONE DRIVER, TWO PINS. Each act below is a driver: what is done before the
// act's own requests are counted (setup) and the act itself (drive), against a
// page opened on `hash`. The request goldens run a driver once against the
// fixture's answers; the refusal goldens (p0-page.js) run the same driver
// again with one of its writes refused, so a refusal pin always drives
// exactly the act its request pin does.

// A pick is a click on the chip and a click on the option's row. The chip is
// clicked as a chip: a dim placeholder first redraws itself into one
// (redrawPositionsTable's outerHTML write), which this DOM does not follow,
// and the options it then opens are the same.
function pickChip(page, n, kind, value) {
    page.click('root', { act: 'pick', node: n, kind: kind });
    const opts = page.pd('optionsFor')(n, kind);
    const i = opts.findIndex(o => o.value === value);
    if (i < 0) throw new Error(n + ' ' + kind + ' offers no ' + value);
    // A node chip is the shared picker, its rows the choices (a note is not
    // one); role and swap mode stay the page's menus.
    if (kind === 'role' || kind === 'mode') pickRow(page, 'pd-pop', 'data-opt', i);
    else pickerRow(page, 'pd-pop', opts.filter(o => !o.note).findIndex(o => o.value === value));
}
// The Part chip opens the part popover; its rows answer through the
// popover's own onclick (onPartPopClick).
async function openPartPop(page) {
    page.click('root', { act: 'pick', node: 'POS_A1', kind: 'part' });
    await page.settle();
}
function partPopRow(page, how, code) {
    const pop = page.byId('pd-pop');
    const row = page.rows('pd-partlist', 'data-pp').find(r => r.dataset.pp === how && (code === undefined || r.dataset.code === code));
    if (!row) throw new Error('the part popover has no ' + how + ' ' + code + ': ' + (page.byId('pd-partlist') || {}).innerHTML);
    pop.onclick({ target: row, stopPropagation() {} });
}
// The choreography + New recipe's + Add a position takes: the first the
// model would not block a one-position recipe on.
function freeMode(page) {
    const M = page.pd('M')();
    const mode = Object.keys(M.modeLabels()).find(m =>
        M.bar(M.reduce(page.S.model, { type: 'addPosition', node: 'POS_A11', mode: m })).tone !== 'blocked');
    if (!mode) throw new Error('every choreography blocks a one-position recipe on POS_A11');
    return mode;
}

// PRESS A WITH NO SCREEN: its two screens gone and every position of it on
// none, the line "+ Add a screen" is drawn on (ruling 2026-11-15, item 4).
function noScreenPage() {
    const page = JSON.parse(JSON.stringify(FX.page));
    page.stations = page.stations.filter(st => st.process_id !== 1);
    page.stationNodes = {};
    return page;
}
function noScreenRoutes(method, url) {
    const u = url.split('?')[0];
    if (method === 'GET' && u === '/api/process-nodes') {
        return { body: FX.server.processNodes.map(n => (n.process_id === 1 ? Object.assign({}, n, { operator_station_id: null, station_name: '' }) : n)) };
    }
    if (method === 'GET' && u === '/api/operator-stations') return { body: FX.page.stations.filter(st => st.process_id !== 1) };
    return undefined;
}

const DRIVE = {
    flowEdit: { drive: async page => {
        pickChip(page, 'POS_A5', 'source', 'SRC_1');
        pickChip(page, 'POS_A1', 'via:0', 'LM_1');
        await page.settle();
    } },
    flowSave: {
        setup: page => DRIVE.flowEdit.drive(page),
        drive: async page => { page.click('root', { act: 'save' }); await page.settle(); },
    },
    styleCreate: { drive: async page => {
        page.click('root', { act: 'new-style' });
        page.type('name', 'Style C');
        page.type('catid', 'CAT-C');
        await sheetOK(page);
    } },
    mint: { drive: async page => {
        page.click('root', { act: 'new-style' });
        if (!page.byId('pd-newrecipe')) throw new Error('New style draws no recipe chooser');
        pickRecipe(page, '7');
        await page.settle();
        page.type('slot0', 'PART-1');
        page.type('slot1', 'PART-3');
        page.type('catid', 'CAT-M');
        await sheetOK(page);
    } },
    // ONE SHEET MAKES A LINE (ruling 2026-11-15): a name, the screen left
    // blank (named after the process), a group, two positions, an inbound and
    // an outbound on the set pick, and a part. Create is its five writes.
    addProcess: { drive: async page => {
        page.click('root', { act: 'add-process' });
        await page.settle();
        page.type('name', 'Press N');
        page.click('scrim', { act: 'add-pickgroup' }, { inside: ['.pd-modal'] });
        pickerRow(page, 'pd-advpop', 3);
        setPick(page, 'proc', 'positions', 'NEW_1');
        setPick(page, 'proc', 'positions', 'NEW_2');
        setPick(page, 'proc', 'source', 'SRC_2');
        setPick(page, 'proc', 'destination', 'DST_1');
        npk(page, 'scrim', 'parts', 'PART-1');
        await page.settle();
        await sheetOK(page);
    } },
    settingsSave: { hash: '#tab=settings', drive: async page => {
        const S = page.S;
        const st = (key, value) => typeSetting(page, '[data-st]', el => el.dataset.st === key && el.tagName === 'INPUT', value);
        st('name', 'Press A2');
        st('description', 'renamed');
        // The counter's PLC is a pick from WarLink's PLCs (F12), on the
        // shared picker (slice 5).
        page.click('root', { act: 'st-plc' });
        pickerRow(page, 'pd-stpop', 'PLC_B');
        st('counter_tag_name', 'CountB');
        page.click('root', { act: 'st-toggle', st: 'counter_enabled' });
        page.click('root', { act: 'st-arm', arm: 'prompt' });
        page.click('root', { act: 'st-toggle', st: 'flow_composer_enabled' });
        page.click('root', { act: 'st-group' });
        pickerRow(page, 'pd-stpop', 4);
        page.click('root', { act: 'st-toggle', st: 'quality_hold_enabled' });
        page.click('root', { act: 'st-holddest' });
        await page.settle();
        holdPick(page, 'DST_2');
        // Two curtains switched on, each with both pointers and a release
        // value (one TRUE, one FALSE); every other node's draft is left as it
        // was, so only these two are sent.
        const nodes = S.curtainNodes;
        for (const [n, tag, val] of [[nodes[0], 'CurtainA1', 'true'], [nodes[2], 'CurtainA3', 'false']]) {
            // + Add a position under Light curtain, then the position.
            page.click('root', { act: 'cu-add' });
            await page.settle();
            holdPick(page, String(n.id));
            const cu = f => typeSetting(page, 'input[data-cn]', el => el.dataset.cn === String(n.id) && el.dataset.cf === f,
                f === 'plc' ? 'PLC_A' : tag);
            cu('plc');
            cu('tag');
            page.click('root', { act: 'cu-val', node: String(n.id), val: val });
        }
        await stSave(page);
    } },
    // A screen's name and note are its sheet; its positions are the Positions
    // box under Settings › Positions and routing, saved by the one Save.
    screenEdit: { drive: async page => {
        page.click('root', { act: 'screen-edit', station: '11' });
        await page.settle();
        page.type('note', 'front HMI, moved');
        await sheetOK(page);
    } },
    // "+ Add a screen" is a line with no screen's (ruling 2026-11-15, item 4):
    // Press A with its two screens gone, its positions on none. The sheet
    // takes the positions in the Positions box.
    screenAdd: { hash: '#tab=settings', page: () => noScreenPage(), extra: noScreenRoutes, drive: async page => {
        page.click('root', { act: 'screen-add' });
        await page.settle();
        page.type('name', 'Screen C');
        page.type('note', 'spare');
        await sheetOK(page);
    } },
    // The Positions box: POS_A11 off and NEW_3 on, then Save; a position taken
    // off is asked about first, and Save again takes it off. NEW_3 goes on
    // the line's first screen.
    screenPositions: { hash: '#tab=settings', drive: async page => {
        setPick(page, 'st', 'positions', 'POS_A11', 'drop');
        setPick(page, 'st', 'positions', 'NEW_3');
        await page.settle();
        await stSave(page);
        await stSave(page);
    } },
    partPopAdd: { setup: openPartPop, drive: async page => {
        // Core's catalog waits behind its own button while the process has parts.
        partPopRow(page, 'catalog');
        partPopRow(page, 'add', 'PART-12');
        await page.settle();
    } },
    partPopDrop: { setup: openPartPop, drive: async page => { partPopRow(page, 'drop', 'PART-11'); await page.settle(); } },
    // Settings' part set and routing set are drafts: a pick, then Save.
    partSettingsAdd: { hash: '#tab=settings', drive: async page => { npk(page, 'root', 'parts', 'PART-13'); await stSave(page); } },
    partSettingsDrop: { hash: '#tab=settings', drive: async page => { npk(page, 'root', 'parts', 'PART-11', 'drop'); await stSave(page); } },
    // The routing set is the set pick's three role boxes on Settings.
    routingAddNew: { hash: '#tab=settings', drive: async page => { setPick(page, 'st', 'source', 'SRC_2'); await stSave(page); } },
    routingAdopt: { hash: '#tab=settings', drive: async page => { setPick(page, 'st', 'destination', 'DST_9'); await stSave(page); } },
    routingDrop: { hash: '#tab=settings', drive: async page => { setPick(page, 'st', 'staging', 'STG_3', 'drop'); await stSave(page); } },
    routingToggleOff: { hash: '#tab=settings', drive: async page => { setPick(page, 'st', 'staging', 'STG_1'); await stSave(page); } },
    routingAllOn: { hash: '#tab=settings', drive: async page => {
        page.click('root', { act: 'rs-all-on', role: 'staging' });
        await page.settle();
        await stSave(page);
    } },
    recipeFromStyle: { drive: async page => {
        page.click('root', { act: 'save-preset' });
        page.type('name', 'From A');
        await sheetOK(page);
    } },
    // + New recipe; + Add a position asks the position first, then how it
    // swaps (the draft runs no mode yet).
    recipeDraft: { drive: async page => {
        page.click('root', { act: 'new-recipe' });
        await page.settle();
        const mode = freeMode(page);
        page.click('root', { act: 'add-position' });
        pickerRow(page, 'pd-pop', 'POS_A11');
        pickRow(page, 'pd-pop', 'data-freemode', mode);
        await page.settle();
    } },
    recipeCells: {
        setup: page => DRIVE.recipeDraft.drive(page),
        drive: async page => {
            page.click('root', { act: 'shape-save' });
            page.type('name', 'Fresh');
            await sheetOK(page);
        },
    },
    recipeGroupsOpen: { drive: async page => {
        page.click('root', { act: 'recipe-open', preset: '7' });
        page.click('root', { act: 'groups-setup' });
        await page.settle();
    } },
    recipeGrouped: {
        setup: page => DRIVE.recipeGroupsOpen.drive(page),
        drive: async page => {
            page.click('root', { act: 'shape-save' });
            await sheetOK(page);
        },
    },
};

async function actOpenProcess() {
    CURRENT = 'requests-open-process.txt';
    const page = await openPage();
    out['requests-open-process.txt'] = actLines('boot on ?process=1: the composer read, each screen\'s claimed nodes, ' +
        'then the first preview', page, 0).join('\n') + '\n';
    const pv = page.calls.find(c => /\/flow\/preview(\?steps=1)?$/.test(c.url));
    if (!pv) throw new Error('boot sent no preview');
    bootPreview = { url: pv.url, raw: JSON.stringify(pv.body) };
}

async function actFlow() {
    CURRENT = 'requests-flow.txt';
    const page = await openPage();
    const pd = page.pd, S = page.S;
    let from = page.mark();
    await DRIVE.flowEdit.drive(page);
    const lines = actLines('flow preview: two picker edits (POS_A5 source → SRC_1, POS_A1 key route + LM_1), ' +
        'debounced into one preview', page, from);
    lines.push('-- bar ' + pd('M')().bar(S.model).tone + ', dirty=' + pd('dirty')());
    from = page.mark();
    await DRIVE.flowSave.drive(page);
    lines.push('', ...actLines('Save style: flow/save on the last preview\'s fingerprint, then the re-open it does', page, from));
    out['requests-flow.txt'] = lines.join('\n') + '\n';
}

async function actStyleCreate() {
    CURRENT = 'requests-style-create.txt';
    const page = await openPage();
    const from = page.mark();
    await DRIVE.styleCreate.drive(page);
    out['requests-style-create.txt'] = actLines('+ New style with no recipe: a typed name and CATID', page, from).join('\n') + '\n';
}

async function actMint() {
    CURRENT = 'requests-flow-mint.txt';
    const page = await openPage();
    const from = page.mark();
    await DRIVE.mint.drive(page);
    out['requests-flow-mint.txt'] = actLines('+ New style, Recipe One (id 7) picked in the chooser: a part per slot and a ' +
        'CATID; no spot moved', page, from).join('\n') + '\n';
}

async function actAddProcess() {
    CURRENT = 'requests-add-process.txt';
    const page = await openPage();
    const from = page.mark();
    await DRIVE.addProcess.drive(page);
    out['requests-add-process.txt'] = actLines('Add process: name Press N, its screen left blank, group Presses, positions NEW_1 and ' +
        'NEW_2, Inbound SRC_2, Outbound DST_1, part PART-1, then Create (the process, its screen, its positions, its routing ' +
        'set in one write, its part set)', page, from).join('\n') + '\n';
}

async function actSettings() {
    CURRENT = 'requests-settings-save.txt';
    const page = await openPage({ hash: DRIVE.settingsSave.hash });
    const S = page.S;
    const from = page.mark();
    const nodes = S.curtainNodes;
    await DRIVE.settingsSave.drive(page);
    const lines = actLines('Save settings: name, description, counter PLC and tag typed, counter off, auto-arm Prompt, ' +
        'the HMI gate off, group Welders, the quality hold on to DST_2, curtains on ' + nodes[0].name + ' and ' +
        nodes[2].name, page, from);
    lines.push('-- settings error ' + JSON.stringify(S.settingsError) + ', notice ' + JSON.stringify(S.settingsNotice));
    out['requests-settings-save.txt'] = lines.join('\n') + '\n';
}

async function actScreenSheet() {
    CURRENT = 'requests-screen-sheet.txt';
    const lines = [];
    {
        const page = await openPage();
        const from = page.mark();
        await DRIVE.screenEdit.drive(page);
        lines.push(...actLines('Edit Screen A: the note (its positions are its row on Settings)', page, from), '');
    }
    {
        const d = DRIVE.screenAdd;
        const page = await openPage({ hash: d.hash, page: d.page(), extra: d.extra });
        const from = page.mark();
        await d.drive(page);
        lines.push(...actLines('Settings › + Add a screen on Press A with no screen: Screen C, taking the positions on no screen', page, from), '');
    }
    {
        const page = await openPage({ hash: DRIVE.screenPositions.hash });
        const from = page.mark();
        await DRIVE.screenPositions.drive(page);
        lines.push(...actLines('Settings › Positions and routing › the Positions box: POS_A11 off and NEW_3 on, Save; the removal is asked about first, ' +
            'and Save again takes it off', page, from));
    }
    out['requests-screen-sheet.txt'] = lines.join('\n') + '\n';
}

async function actPartSet() {
    CURRENT = 'requests-part-set.txt';
    const lines = [];
    {
        const page = await openPage();
        await openPartPop(page);
        let from = page.mark();
        await DRIVE.partPopAdd.drive(page);
        lines.push(...actLines('Part popover on POS_A1: + PART-12 from Core\'s catalog (joins the part set and lands ' +
            'on POS_A1)', page, from), '');
        await openPartPop(page);
        from = page.mark();
        await DRIVE.partPopDrop.drive(page);
        lines.push(...actLines('Part popover: × PART-11 (on no position, in no other flow)', page, from), '');
    }
    {
        const page = await openPage({ hash: '#tab=settings' });
        let from = page.mark();
        await DRIVE.partSettingsAdd.drive(page);
        lines.push(...actLines('Settings › Parts this process runs: + PART-13', page, from), '');
        from = page.mark();
        await DRIVE.partSettingsDrop.drive(page);
        lines.push(...actLines('Settings › Parts this process runs: × PART-11', page, from));
    }
    out['requests-part-set.txt'] = lines.join('\n') + '\n';
}

async function actRouting() {
    CURRENT = 'requests-routing-set.txt';
    const lines = [];
    const acts = [
        ['SRC_2 picked in Inbound, Save (a name with no row: the one write adds it at the end of its role)', DRIVE.routingAddNew],
        ['DST_9 picked in Outbound, Save (a switched-off row: the one write adopts it)', DRIVE.routingAdopt],
        ['× STG_3 out of Staging, Save (the one write takes it out, and says its warning)', DRIVE.routingDrop],
        ['STG_1 clicked in Staging, Save (STG_1 was picked: the click takes it out)', DRIVE.routingToggleOff],
    ];
    for (const [title, d] of acts) {
        const page = await openPage({ hash: d.hash });
        const from = page.mark();
        await d.drive(page);
        const ls = actLines('Settings › Routing set: ' + title, page, from);
        ls.push('-- settings notice ' + JSON.stringify(page.S.settingsNotice) + ', routing error ' + JSON.stringify(page.S.routingError));
        lines.push(...ls, '');
    }
    {
        const page = await openPage({ hash: DRIVE.routingAllOn.hash });
        const from = page.mark();
        await DRIVE.routingAllOn.drive(page);
        lines.push(...actLines('Settings › Routing set: Turn all 1 on (Staging), Save', page, from));
    }
    out['requests-routing-set.txt'] = lines.join('\n') + '\n';
}

async function actRecipe() {
    CURRENT = 'requests-recipe-create.txt';
    const lines = [];
    {
        const page = await openPage();
        const from = page.mark();
        await DRIVE.recipeFromStyle.drive(page);
        lines.push(...actLines('Save as recipe… on the saved Style A (from_style_id)', page, from), '');
    }
    {
        // + New recipe, + Add a position, then Save recipe… (the cells form).
        const page = await openPage();
        const S = page.S, M = page.pd('M')();
        let from = page.mark();
        await DRIVE.recipeDraft.drive(page);
        lines.push(...actLines('+ New recipe, then + Add a position: ' + freeModeUsed(page) +
            ' on POS_A11', page, from));
        lines.push('-- bar ' + M.bar(S.model).tone, '');
        from = page.mark();
        await DRIVE.recipeCells.drive(page);
        lines.push(...actLines('Save recipe… on that draft (cells, no groups)', page, from), '');
    }
    {
        const page = await openPage();
        const S = page.S;
        let from = page.mark();
        await DRIVE.recipeGroupsOpen.drive(page);
        lines.push(...actLines('Recipe One opened from the rail, then Set up groups', page, from));
        lines.push('-- groups ' + JSON.stringify((S.shapeDraft.groups || []).map(g => g.name + ' of ' + g.of)), '');
        from = page.mark();
        await DRIVE.recipeGrouped.drive(page);
        lines.push(...actLines('Save recipe… with groups (saveGroupedRecipe), the name as it opens', page, from));
    }
    out['requests-recipe-create.txt'] = lines.join('\n') + '\n';
}

// The mode recipeDraft put on POS_A11, read back off the draft.
function freeModeUsed(page) { return (page.S.model.cells.POS_A11 || {}).mode; }

const P0 = require('./p0-page.js');

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
    await P0.run({
        H, FX, out, DRIVE, openPage, actLines, fmtCalls, npk, setPick, sheetOK, stSave, pickRow, pickerRow, pickRecipe, typeSetting, decode, text,
        bootPreview: () => bootPreview, setCurrent: n => { CURRENT = n; }, SEEN,
    });
    await require('./s4-page-bugs.js').run({ FX, out, DRIVE, openPage, npk, setPick, stSave, holdRows, text, setCurrent: n => { CURRENT = n; }, SEEN });
    await require('./cold-start.js').run({
        H, FX, out, DRIVE, openPage, actLines, fmtCalls, npk, setPick, sheetOK, pickRow, pickerRow, typeSetting, decode, text,
        setCurrent: n => { CURRENT = n; }, SEEN,
    });
    await require('./s4-table.js').run({ out, openPage, pickRow, pickChip });
    await require('./work-lists.js').run({ out, openPage, decode, text });
    if (process.argv.includes('--sites')) {
        process.stdout.write(P0.sitesTable(SEEN));
        return;
    }
    process.stdout.write(JSON.stringify(out));
})().catch(e => {
    process.stderr.write('process-setup pins: ' + ((e && e.stack) || e) + '\n');
    process.exit(1);
});
