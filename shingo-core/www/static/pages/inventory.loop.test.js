// Unit tests for /inventory's two tables (inventory.js): the doubt line, the
// durations, the supermarket positions (values only, red past the threshold,
// one chip), the lines (consume and produce), the default view (the rows that
// need a look, worst first, the rest folded; never an empty body), and the
// calculator reached only from the UoP threshold column's ⓘ. Run under plain
// Node via the Go wrapper inventory_ledger_panels_test.go. Exit 0 on pass, 1 on
// any failure.
//
// The renderers are lifted out of inventory.js by name and run in a vm with the
// page's imports stubbed: the module's top level starts its fetch loop.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let failures = 0;
function check(name, cond, detail) {
    if (cond) {
        console.log('  ok  ' + name);
    } else {
        failures++;
        console.log('  FAIL ' + name + (detail ? ' — ' + detail : ''));
    }
}

const src = fs.readFileSync(path.join(__dirname, 'inventory.js'), 'utf8');

function lift(kind, name) {
    const at = src.search(new RegExp('^' + kind + '\\s+' + name + '\\b', 'm'));
    if (at < 0) return null;
    for (let i = src.indexOf('{', at), depth = 0; i < src.length; i++) {
        if (src[i] === '{') depth++;
        else if (src[i] === '}' && --depth === 0) return src.slice(at, i + 1) + (kind === 'const' ? ';' : '');
    }
    return null;
}

const NAMES = ['num', 'dur', 'timeToEmpty', 'timeLeft', 'worstFirst', 'foldedBodyHtml', 'offHomeTitle', 'thrTitle',
    'doubtHtml', 'hl', 'help', 'posHeadHtml', 'thrHeadHtml', 'thresholdCell', 'meterHtml', 'coming', 'posRowHtml', 'posBodyHtml',
    'lineHeadHtml', 'lineRowHtml', 'lineBodyHtml', 'matches'];

function load(authenticated) {
    const ctx = {
        escapeHtml: (s) => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;')
            .replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;'),
        formatTime: (iso) => 'T(' + iso + ')',
        encodeURIComponent,
        authenticated, searchTerm: '', flashKey: '', thrPopOpen: false, open: { positions: false, lines: false },
    };
    vm.createContext(ctx);
    const parts = [lift('const', 'HELP')].concat(NAMES.map((n) => lift('function', n)));
    const missing = ['HELP'].concat(NAMES).filter((n, i) => !parts[i]);
    if (missing.length) return { missing };
    vm.runInContext(parts.join('\n'), ctx);
    return ctx;
}

// A monitored position, as /api/inventory/loop sends it.
function loader(over) {
    return Object.assign({
        key: 'loader|SMN_018|SYN44-PARTA.06', kind: 'loader', group: 'SNF3', payload_code: 'SYN44-PARTA.06',
        node: 'SMN_018', loader_id: 3, loader_name: 'Supermarket Dedicated Locations', layout: 'dedicated_positions',
        position_node_id: 18, pack: 250, state: 'above', in_loop: 300, bin_uop: 300, bucket_uop: 0, threshold: 120,
        home_bins: 1, home_capacity: 250, reorder_point: 0, cycle_seconds: 12, cover_seconds: 3600, lead_seconds: 600,
        inputs: { l1_queue_seconds: 60, l1_transit_seconds: 240, l2_load_seconds: 300, l2_transit_seconds: 0,
            market_to_cell_seconds: 90, samples_l1: 4, samples_retrieve: 6 },
        suggested: 75, confidence: 'LOW', completed_l1: 2, coming: 0, off_home: [], provenance: null,
        beyond: false, look: false,
    }, over || {});
}

// A claim at a running line, produce unless over says consume.
function line(over) {
    return Object.assign({
        key: 'line|SNF3|ALN_011|SYN44-FG.01|produce', kind: 'line', group: 'SNF3', payload_code: 'SYN44-FG.01',
        node: 'ALN_011', role: 'produce', pack: 132, in_bin: 16, reorder_point: 110, cycle_seconds: 12,
        cover_seconds: 1392, station: 'EDGE 1', coming: 0, beyond: false, look: false,
    }, over || {});
}

const ctx = load(true);
if (ctx.missing) {
    check('inventory.js defines the table renderers', false, 'missing: ' + ctx.missing.join(', '));
    process.exit(1);
}
const run = (expr) => vm.runInContext(expr, ctx);
const call = (fn, ...args) => { ctx.__args = args; return run(fn + '(...__args)'); };
// cells is a row's <td> contents, tags stripped but titles kept out.
const cells = (html) => (html.match(/<td[^>]*>([\s\S]*?)<\/td>/g) || [])
    .map((td) => td.replace(/<[^>]+>/g, '').replace(/&#39;/g, '\'').trim());

console.log('inventory: the doubt line');
{
    check('none: absent, not "0 problems"', call('doubtHtml', []) === '' && call('doubtHtml', null) === '');
    const two = call('doubtHtml', [
        { bin_id: 7, label: 'SHG:0007', node: 'ALN_004', payload_code: 'SYN44-PARTA.06', uop: -407, reason: 'below_zero' },
        { bin_id: 9, label: 'SHG:0009', node: 'SMN_031', payload_code: 'SYN44-PARTB.06', uop: 0, reason: 'unconfirmed' },
    ]);
    check('two: one line naming both', (two.match(/<a /g) || []).length === 2 && two.includes('2 doubtful'), two);
    check('two: each links to its recount door', two.includes('href="/bins?open=7"') && two.includes('href="/bins?open=9"'), two);
    check('two: says why, in a phrase', two.includes('reads -407') && two.includes('not confirmed') && !two.includes('no count'), two);
}

console.log('inventory: durations');
{
    check('47 min', call('dur', 2820) === '47 min', call('dur', 2820));
    check('from 90 minutes up, hours with one decimal', call('dur', 5400) === '1.5 h' && call('dur', 8280) === '2.3 h',
        call('dur', 5400) + ' ' + call('dur', 8280));
    check('none is a dash', call('dur', null) === '—');
}

console.log('inventory: supermarket positions');
{
    const head = call('posHeadHtml');
    const ths = (head.match(/<th[^>]*>([\s\S]*?)<\/th>/g) || []).map((th) => th.replace(/<div[\s\S]*<\/div>/, '')
        .replace(/<[^>]+>/g, '').replace('ⓘ', '').trim());
    check('the columns, in order', JSON.stringify(ths) === JSON.stringify(['Part', 'Spot', 'In loop (UoP)', 'Time to empty',
        'UoP threshold', 'On the way']), JSON.stringify(ths));
    check('no lead-time column, no Suggested', !/Refill takes|Suggested|Last 30|Empty arrives|Waited/.test(head), head);
    check('the calculator is reached only from the UoP threshold ⓘ, labelled uncalibrated',
        head.includes('data-action="openCalcPicker"') && head.includes('calculator (uncalibrated)')
        && (head.match(/openCalcPicker/g) || []).length === 1, head);

    const row = call('posRowHtml', loader({ off_home: [{ node: 'SMN_004', bins: 1 }], coming: 2 }));
    const c = cells(row);
    check('values only: part, spot, number, the time, the threshold, the count',
        JSON.stringify(c) === JSON.stringify(['SYN44-PARTA.06', 'SMN_018', '300', '60 min', '', '2']), JSON.stringify(c));
    check('no loader name, no words under the numbers', !/Supermarket|lead|cycle|calc|refill|REFILLS/i.test(row.replace(/title="[^"]*"/g, '')), row);
    check('bins away from the spot are on hover, not text', row.includes('title="1 bin at SMN_004"')
        && !/bin at/.test(row.replace(/title="[^"]*"/g, '')), row);
    check('nothing on the way is a dash', cells(call('posRowHtml', loader({ coming: 0 })))[5] === '—');

    const refill = call('posRowHtml', loader({ state: 'refills_when_empty', threshold: 251 }));
    check('a one-bin home at a pack or more carries no chip', !/badge|REFILLS/.test(refill), refill);
    const over = call('posRowHtml', loader({ layout: 'shared_window', state: 'above_home_capacity', threshold: 80 }));
    check('ABOVE HOME CAPACITY is the one chip', over.includes('badge-warn') && over.includes('ABOVE HOME CAPACITY'), over);

    const beyond = call('posRowHtml', loader({ in_loop: 100, cover_seconds: 1200, beyond: true }));
    check('at or under the threshold, time to empty is red', /<td class="loop-r tnum loop-short">20 min<\/td>/.test(beyond), beyond);
    check('within it, not red', !call('posRowHtml', loader()).includes('loop-short'));
    const idle = call('posRowHtml', loader({ group: '' }));
    check('no running line: time to empty is a dash', cells(idle)[3] === '—', JSON.stringify(cells(idle)));
    const norate = call('posRowHtml', loader({ cover_seconds: null }));
    check('a running line with no measured rate: ?', cells(norate)[3] === '?', JSON.stringify(cells(norate)));

    // The threshold line (owner, 2026-10-30: "the old threshold line with the
    // | and colors"): in loop against the threshold tick, in the In loop cell.
    // 300 in loop, threshold 120: scale max(192, 330) = 330, fill 90.9 %,
    // tick 36.4 %.
    const clear = call('meterHtml', loader());
    check('the line: the fill and the threshold tick on one scale', clear.includes('width:90.9%')
        && clear.includes('thr-meter__tick" style="left:36.4%"'), clear);
    check('clear of the threshold: green (no state class)', clear.startsWith('<span class="thr-meter" '), clear);
    check('within a fifth above it: amber', call('meterHtml', loader({ in_loop: 130 })).includes('thr-meter--near'));
    check('past it (Core\'s beyond): red', call('meterHtml', loader({ in_loop: 100, beyond: true })).includes('thr-meter--below'));
    check('a refill-when-empty home stocked is not red, its home empty is',
        !call('meterHtml', loader({ in_loop: 250, threshold: 251 })).includes('--below')
        && call('meterHtml', loader({ in_loop: 250, threshold: 251, beyond: true })).includes('--below'));
    check('below zero: hatched, no fill', call('meterHtml', loader({ in_loop: -40 })).includes('thr-meter--err')
        && !call('meterHtml', loader({ in_loop: -40 })).includes('__fill'));
    check('the line rides in the In loop cell, its value still the number',
        row.includes('thr-meter') && cells(row)[2] === '300', JSON.stringify(cells(row)));
    check('the number sits in a fixed box after the line, so every bar starts at the same place',
        /thr-meter[\s\S]*<span class="loop-inloop__n">/.test(row), row);

    const ro = load(false);
    ro.__args = [loader({})];
    const cellRO = vm.runInContext('thresholdCell(...__args)', ro);
    check('no session: the value as a number, no input', !cellRO.includes('<input') && cellRO.includes('>120<'), cellRO);
    const cellRW = call('thresholdCell', loader({ provenance: { value: 120, old: 0, source: 'manual', by: 'eng', at: 'z', current: true } }));
    check('with a session: the input, value only, history on hover', cellRW.includes('<input') && cellRW.includes('value="120"')
        && cellRW.includes('manual 120 (was 0) by eng') && cellRW.includes('min at the 14-day median running rate'), cellRW);

}

console.log('inventory: lines');
{
    const head = call('lineHeadHtml');
    const ths = (head.match(/<th[^>]*>([\s\S]*?)<\/th>/g) || []).map((th) => th.replace(/<[^>]+>/g, '').replace('ⓘ', '').trim());
    check('the columns, in order', JSON.stringify(ths) === JSON.stringify(['Line', 'Part', 'Role', 'In bin / pack', 'Time left',
        'Cell reorder point', 'On the way']), JSON.stringify(ths));
    check('time left says both directions once, in its help', head.includes('Consume: until the bin at the line is empty. Produce: until it is full.'), head);
    check('no lead-time column', !/Time to full|Empty arrives|Waited/.test(head), head);
    const row = call('lineRowHtml', line(), false);
    const c = cells(row);
    check('values only', JSON.stringify(c) === JSON.stringify(['SNF3', 'SYN44-FG.01', 'produce', '16 / 132', '23 min', '110', '—']),
        JSON.stringify(c));
    check('the role is a plain small word, not a badge', row.includes('<td class="loop-role">produce</td>') && !/badge/.test(row), row);
    check('the reorder point says on hover where it is set', row.includes('title="Set on EDGE 1&#39;s Edge"'), row);
    const consume = cells(call('lineRowHtml', line({ role: 'consume', in_bin: 84, cover_seconds: 1008, reorder_point: 20, coming: 1 }), false));
    check('a consume row: its bin, the time until it is empty, a full coming',
        JSON.stringify(consume) === JSON.stringify(['SNF3', 'SYN44-FG.01', 'consume', '84 / 132', '17 min', '20', '1']), JSON.stringify(consume));
    const past = call('lineRowHtml', line({ beyond: true }), false);
    check('past the threshold, time left is red', /<td class="loop-r tnum loop-short">23 min<\/td>/.test(past), past);
    check('within it, not red', !row.includes('loop-short'), row);
    const norate = cells(call('lineRowHtml', line({ cover_seconds: null }), false));
    check('a bin and no running rate: ?', norate[4] === '?', JSON.stringify(norate));
    const none = cells(call('lineRowHtml', line({ in_bin: undefined, cover_seconds: null }), false));
    check('no bin at a produce spot: dashes', none[3] === '—' && none[4] === '—', JSON.stringify(none));
    const out = cells(call('lineRowHtml', line({ role: 'consume', in_bin: undefined, cover_seconds: 0, beyond: true }), false));
    check('no bin at a consume spot: nothing left', out[3] === '—' && out[4] === '0 min', JSON.stringify(out));
    const two = call('lineBodyHtml', [line({ key: 'p2', group: 'Press 4', node: 'PLN_002', look: true }),
        line({ key: 'p3', group: 'Press 4', node: 'PLN_003', look: true })]);
    check('a line with two spots for one part names the spot', two.includes('PLN_002') && two.includes('PLN_003'), two);
    const roles = call('lineBodyHtml', [line({ key: 'a', look: true }), line({ key: 'b', role: 'consume', payload_code: 'SYN44-IN.01', node: 'ALN_012', look: true })]);
    check('one spot per part: the line alone', !roles.includes('ALN_011') && !roles.includes('ALN_012'), roles);
}

console.log('inventory: the default view');
{
    const keysOf = (html) => Array.from(html.matchAll(/<tr [^>]*data-key="([a-z])"/g)).map((m) => m[1]);
    // Core decides look; the page shows those, least time left first, the
    // unknown last, and folds the rest behind one control.
    const rows = [
        loader({ key: 'a', node: 'A', cover_seconds: 3600, look: true }),
        loader({ key: 'b', node: 'B', cover_seconds: 600, look: true }),
        loader({ key: 'c', node: 'C', cover_seconds: null, look: true }),
        loader({ key: 'd', node: 'D', cover_seconds: 300 }),
        loader({ key: 'e', node: 'E', cover_seconds: 9000 }),
    ];
    const body = call('posBodyHtml', rows);
    check('the rows that need a look, worst first, ? last', JSON.stringify(keysOf(body)) === '["b","a","c"]', JSON.stringify(keysOf(body)));
    check('the healthy rest behind "Show N more", for this table', body.includes('Show 2 more') && body.includes('data-table="positions"')
        && !body.includes('Show all'), body);
    run('open.positions = true');
    const opened = call('posBodyHtml', rows);
    check('open: every row, worst first, and a way back', JSON.stringify(keysOf(opened)) === '["d","b","a","e","c"]'
        && opened.includes('Show fewer'), JSON.stringify(keysOf(opened)));
    const lineOpen = call('lineBodyHtml', [line({ key: 'l' }), line({ key: 'm', node: 'ALN_012', look: true })]);
    check('the fold is per table: lines stay folded', lineOpen.includes('Show 1 more') && lineOpen.includes('data-table="lines"'), lineOpen);
    run('open.positions = false');

    // The fixture where nothing qualifies: 22 healthy positions, 7 healthy lines.
    const calm = Array.from({ length: 22 }, (_, i) => loader({ key: 'k', node: 'N' + i }));
    const calmBody = call('posBodyHtml', calm);
    check('nothing needs a look: one line, never an empty body', (calmBody.match(/<tr/g) || []).length === 1
        && calmBody.includes('All 22 positions healthy') && calmBody.includes('Show all 22'), calmBody);
    const calmLines = call('lineBodyHtml', Array.from({ length: 7 }, (_, i) => line({ key: 'l', node: 'ALN_0' + i, payload_code: 'P' + i })));
    check('the same for lines', calmLines.includes('All 7 lines healthy') && calmLines.includes('Show all 7'), calmLines);
    check('"Show all" appears nowhere else', !body.includes('Show all') && !opened.includes('Show all'));
    run('open.positions = true');
    const calmOpen = call('posBodyHtml', calm);
    check('opened from healthy: all 22 and Show fewer', (calmOpen.match(/<tr class="loop-row/g) || []).length === 22
        && calmOpen.includes('Show fewer'), calmOpen.slice(-200));
    run('open.positions = false');

    run('searchTerm = "SMN"');
    const found = call('posBodyHtml', rows);
    check('a search shows every match, unfolded, no control', keysOf(found).length === 5 && !/Show /.test(found), found.slice(-200));
    run('searchTerm = ""');
    check('nothing monitored says so', call('posBodyHtml', []).includes('No part is monitored.'));
}

console.log('inventory: the calculator, from the ⓘ only');
{
    // Owner, 2026-10-29/30: no suggestion is one click into a live setting;
    // the calculator is uncalibrated and opens only from the column's ⓘ.
    const open = (authenticated) => {
        const posts = [];
        const opened = [];
        const els = {};
        const el = (id) => (els[id] = els[id] || { id, innerHTML: '', textContent: '', value: '',
            classList: { add: (c) => opened.push(id + '.' + c), remove: () => {} } });
        const ctx2 = {
            apiPost: (url, body) => { posts.push({ url, body }); return Promise.resolve({}); },
            toast: () => {}, loadAll: () => Promise.resolve(), CSS: { escape: (s) => s },
            escapeHtml: (s) => String(s == null ? '' : s), num: (n) => String(n),
            document: { querySelectorAll: () => [], getElementById: el },
            Number, authenticated, view: { lead_days: 14, safety: 1.5, rows: [
                loader({ key: 'far', node: 'FAR', cover_seconds: 9000 }),
                loader({ key: 'near', node: 'NEAR', cover_seconds: 700 }),
            ] },
            flashKey: '', calcKey: '', calcSuggested: null,
        };
        vm.createContext(ctx2);
        const names = ['timeToEmpty', 'timeLeft', 'worstFirst', 'rowByKey', 'saveThreshold', 'calcRows', 'openCalcPicker', 'secs',
            'openCalc', 'calcOutHtml', 'confirmCalc', 'closeCalc'];
        const fns = names.map((n) => lift('function', n) || lift('async function', n));
        const missing = names.filter((n, i) => !fns[i]);
        if (missing.length) return { missing };
        vm.runInContext(fns.join('\n'), ctx2);
        vm.runInContext('openCalcPicker()', ctx2);
        return { posts, opened, els, ctx2 };
    };
    const rw = open(true);
    const anon = open(false);
    if (rw.missing) {
        check('inventory.js defines the calculator functions', false, 'missing: ' + rw.missing.join(', '));
        process.exit(1);
    }
    setTimeout(() => {
        const body = (rw.els['loop-calc-body'] || {}).innerHTML || '';
        check('opening posts nothing', rw.posts.length === 0, JSON.stringify(rw.posts));
        check('it opens on the closest position, with a picker', rw.opened.includes('loop-calc.active')
            && body.includes('<option value="near" selected>') && body.includes('value="far"'), body);
        check('titled uncalibrated', (rw.els['loop-calc-title'] || {}).textContent === 'Calculator (uncalibrated)',
            (rw.els['loop-calc-title'] || {}).textContent);
        check('prefilled with the suggestion and offers Confirm', body.includes('id="calc-value"') && body.includes('value="75"')
            && body.includes('data-action="confirmCalc"'), body);
        rw.els['calc-value'] = { value: '75' };
        vm.runInContext('confirmCalc()', rw.ctx2);
        setTimeout(() => {
            check('Confirm posts once, source calculated', rw.posts.length === 1 && rw.posts[0].url === '/api/loader/set-home'
                && rw.posts[0].body.uop_threshold === 75 && rw.posts[0].body.source === 'calculated', JSON.stringify(rw.posts));
            check('logged out: read-only, no Confirm', anon.posts.length === 0 && anon.opened.includes('loop-calc.active')
                && !((anon.els['loop-calc-body'] || {}).innerHTML || '').includes('confirmCalc'), JSON.stringify(anon.opened));
            console.log('inventory table: ' + (failures ? failures + ' failed' : 'all passed'));
            process.exit(failures ? 1 : 0);
        }, 20);
    }, 20);
}
