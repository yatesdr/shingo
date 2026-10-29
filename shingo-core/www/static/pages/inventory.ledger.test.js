// Unit tests for the /inventory page's two strict-pack lists (lane P0): the
// dropped produce holds and the declared-full stamps. Run under plain Node via
// the Go wrapper inventory_ledger_panels_test.go. Exit 0 on pass, 1 on any
// assertion failure.
//
// The renderers are lifted out of inventory.js by name and run in a vm with the
// page's imports stubbed, because the module's top level starts its fetch loop
// and its SSE wiring on load. What is pinned: each list renders its fixed keys
// (node and time together on every row), a count in the head, no red chip (a
// routine list, not an alarm), escaping, and a quiet "none" line when the
// window holds nothing.

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

// lift returns the source of one top-level declaration (a function or a const
// object literal) by name, brace-matched.
function lift(kind, name) {
    const at = src.search(new RegExp('^' + kind + '\\s+' + name + '\\b', 'm'));
    if (at < 0) return null;
    for (let i = src.indexOf('{', at), depth = 0; i < src.length; i++) {
        if (src[i] === '{') depth++;
        else if (src[i] === '}' && --depth === 0) return src.slice(at, i + 1) + (kind === 'const' ? ';' : '');
    }
    return null;
}

function load(ledger) {
    const hosts = { 'rh-hold-drops': { innerHTML: 'stale' }, 'rh-declared-stamps': { innerHTML: 'stale' } };
    const ctx = {
        document: { getElementById: (id) => hosts[id] || null },
        escapeHtml: (s) => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;')
            .replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;'),
        formatTime: (iso) => 'T(' + iso + ')',
        ledgerExceptions: ledger,
    };
    vm.createContext(ctx);
    const parts = [
        lift('function', 'num'),
        lift('const', 'HOLD_DROP_REASON'),
        'const HOLD_DROP_RUNS_SHOWN = ' + (src.match(/^const HOLD_DROP_RUNS_SHOWN = (\d+);/m) || [])[1] + ';',
        lift('function', 'renderHoldDrops'),
        lift('function', 'renderDeclaredStamps'),
    ];
    const missing = ['num', 'HOLD_DROP_REASON', 'HOLD_DROP_RUNS_SHOWN', 'renderHoldDrops', 'renderDeclaredStamps']
        .filter((n, i) => !parts[i] || /undefined/.test(parts[i].slice(0, 40)));
    if (missing.length) return { missing };
    // `let ledgerExceptions` in the page; a context global here.
    vm.runInContext(parts.join('\n'), ctx);
    vm.runInContext('renderHoldDrops(); renderDeclaredStamps();', ctx);
    return { drops: hosts['rh-hold-drops'].innerHTML, stamps: hosts['rh-declared-stamps'].innerHTML };
}

console.log('inventory: the two lists render their rows');
{
    const out = load({
        since: '2026-10-13T08:00:00Z',
        hold_drop_runs: [
            { node: 'ALN_011', payload_code: 'SYN44-PARTA.06', drops: 1, qty: 24, reason: 'sweep', station: 'stn-a',
              first: '2026-10-20T08:15:00Z', last: '2026-10-20T08:15:00Z' },
            { node: 'ALN_<b>', payload_code: 'SYN44-PARTB.06', drops: 1, qty: 1, reason: 'something_new', station: 'stn-a',
              first: '2026-10-20T09:00:00Z', last: '2026-10-20T09:00:00Z' },
            { node: 'ALN_012', payload_code: 'SYN44-PARTC.06', drops: 1, qty: 37, reason: 'no_bin', station: 'stn-b',
              first: '2026-10-20T09:30:00Z', last: '2026-10-20T09:30:00Z' },
        ],
        declared_full_stamps: [
            { node: 'ALN_011', bin_id: 812, bin_label: 'SHG:0812', payload_code: 'SYN44-PARTA.06',
              counted: 131, declared: 132, actor: 'operator-7', applied_at: '2026-10-20T08:20:00Z' },
        ],
    });
    if (out.missing) {
        check('inventory.js defines the two renderers', false, 'missing: ' + out.missing.join(', '));
    } else {
        const d = out.drops;
        check('drops: head counts what was dropped and the runs', /62 dropped in 3 runs/.test(d), d);
        check('drops: head names the window', d.includes('T(2026-10-13T08:00:00Z)'), d);
        check('drops: node, part, qty and time on the row', d.includes('ALN_011') && d.includes('SYN44-PARTA.06')
            && d.includes('<b>24</b>') && d.includes('T(2026-10-20T08:15:00Z)'), d);
        check('drops: a known reason reads as words', d.includes('no carrier coming (sweep)'), d);
        check('drops: strokes with no bin read as words', d.includes('<b>37</b>')
            && d.includes('made with no bin to count into'), d);
        check('drops: an unknown reason prints as sent', d.includes('something_new'), d);
        check('drops: escaped', d.includes('ALN_&lt;b&gt;') && !d.includes('ALN_<b>'), d);
        check('drops: no red chip', !d.includes('chip-err') && !d.includes('ledger-exceptions"'), d);
        const s = out.stamps;
        check('stamps: head counts the rows', /1 bin declared full/.test(s), s);
        check('stamps: node, bin, counted to declared, who and time on the row', s.includes('ALN_011')
            && s.includes('SHG:0812') && s.includes('131') && s.includes('132') && s.includes('operator-7')
            && s.includes('T(2026-10-20T08:20:00Z)'), s);
        const kept = load({ since: '2026-10-13T08:00:00Z', hold_drop_runs: [], declared_full_stamps: [
            { id: 2, bin_id: 9, bin_label: 'SHG:0900', node: 'ALN_011', payload_code: 'SYN44-PARTA.06',
              counted: 97, declared: 97, reason: 'no_pack', actor: 'operator-7', applied_at: '2026-10-20T08:30:00Z' }] }).stamps;
        check('stamps: a no_pack row says the count was kept', kept.includes('kept: no pack for this part') && !kept.includes('→'), kept);
        // The reason decides, not the numbers: a row with no reason is a stamp.
        const stamped = load({ since: '2026-10-13T08:00:00Z', hold_drop_runs: [], declared_full_stamps: [
            { id: 3, bin_id: 10, bin_label: 'SHG:0901', node: 'ALN_011', payload_code: 'SYN44-PARTA.06',
              counted: 97, declared: 97, actor: 'operator-7', applied_at: '2026-10-20T08:40:00Z' }] }).stamps;
        check('stamps: a row without no_pack is not read as no pack', !stamped.includes('no pack'), stamped);
        check('stamps: no red chip', !s.includes('chip-err') && !s.includes('ledger-exceptions"'), s);
    }
}

console.log('inventory: a line running full is one line with its total, its runs underneath');
{
    // SPR ALN_011, 2026-10-29: a part a stroke dropped (hold_cap) in runs
    // through the day; 200 identical lines before. Runs arrive latest first.
    const run = (first, mins, qty) => ({ node: 'ALN_011', payload_code: 'SYN44-PARTA.06', reason: 'hold_cap',
        station: 'stn-a', drops: qty, qty: qty, first: first,
        last: new Date(new Date(first).getTime() + mins * 60000).toISOString() });
    const runs = [
        run('2026-10-29T18:13:00.000Z', 3, 2), run('2026-10-29T15:17:00.000Z', 7, 12),
        run('2026-10-29T12:41:00.000Z', 70, 71), run('2026-10-29T09:27:00.000Z', 15, 30),
        run('2026-10-28T20:00:00.000Z', 5, 4), run('2026-10-28T10:00:00.000Z', 5, 3),
        run('2026-10-27T10:00:00.000Z', 5, 6), run('2026-10-26T10:00:00.000Z', 5, 9),
    ];
    const out = load({ since: '2026-10-22T18:17:00Z', hold_drop_runs: runs, declared_full_stamps: [] });
    if (!out.missing) {
        const d = out.drops;
        check('running full: one line, not one per stroke', (d.match(/<li>/g) || []).length === 1, d);
        check('running full: the line carries the true total', d.includes('<b>137</b> of'), d);
        check('running full: head says what and how often', /137 dropped in 8 runs/.test(d), d);
        check('running full: a run says when, for how long and how many',
            d.includes('T(2026-10-29T12:41:00.000Z) for 70 min — <b>71</b>'), d);
        check('running full: a run of one moment has no duration', !d.includes('for 0 min'), d);
        check('running full: the six latest spelled out, the rest summed',
            d.includes('8 runs:') && d.includes('and 2 earlier — <b>15</b>')
            && !d.includes('T(2026-10-27T10:00:00.000Z)'), d);
        check('running full: reads as words', d.includes('over a pack while the bin was full'), d);
    }
}

console.log('inventory: an empty window is a quiet line, not a blank');
{
    const out = load({ since: '2026-10-13T08:00:00Z', hold_drop_runs: [], declared_full_stamps: [] });
    if (!out.missing) {
        check('drops: none in the window', /No hold drops since/.test(out.drops) && out.drops.includes('text-muted'), out.drops);
        check('stamps: none in the window', /No bins declared full since/.test(out.stamps) && out.stamps.includes('text-muted'), out.stamps);
    }
    const old = load({});
    if (!old.missing) {
        check('a Core without the keys renders the quiet line', /No hold drops/.test(old.drops) && /No bins declared full/.test(old.stamps),
            old.drops + ' / ' + old.stamps);
    }
}

console.log('inventory ledger lists: ' + (failures ? failures + ' failed' : 'all passed'));
process.exit(failures ? 1 : 0);
