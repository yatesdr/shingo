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
        lift('function', 'renderHoldDrops'),
        lift('function', 'renderDeclaredStamps'),
    ];
    const missing = ['num', 'HOLD_DROP_REASON', 'renderHoldDrops', 'renderDeclaredStamps'].filter((n, i) => !parts[i]);
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
        hold_drops: [
            { node: 'ALN_011', payload_code: 'SYN44-PARTA.06', qty: 24, reason: 'sweep', station: 'stn-a',
              actor: 'edge:sweep', occurred_at: '2026-10-20T08:15:00Z' },
            { node: 'ALN_<b>', payload_code: 'SYN44-PARTB.06', qty: 1, reason: 'something_new', station: 'stn-a',
              actor: '', occurred_at: '2026-10-20T09:00:00Z' },
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
        check('drops: head counts the rows', /2 hold drops/.test(d), d);
        check('drops: head names the window', d.includes('T(2026-10-13T08:00:00Z)'), d);
        check('drops: node, part, qty and time on the row', d.includes('ALN_011') && d.includes('SYN44-PARTA.06')
            && d.includes('<b>24</b>') && d.includes('T(2026-10-20T08:15:00Z)'), d);
        check('drops: a known reason reads as words', d.includes('no carrier coming (sweep)'), d);
        check('drops: an unknown reason prints as sent', d.includes('something_new'), d);
        check('drops: escaped', d.includes('ALN_&lt;b&gt;') && !d.includes('ALN_<b>'), d);
        check('drops: no red chip', !d.includes('chip-err') && !d.includes('ledger-exceptions"'), d);
        const s = out.stamps;
        check('stamps: head counts the rows', /1 bin declared full/.test(s), s);
        check('stamps: node, bin, counted to declared, who and time on the row', s.includes('ALN_011')
            && s.includes('SHG:0812') && s.includes('131') && s.includes('132') && s.includes('operator-7')
            && s.includes('T(2026-10-20T08:20:00Z)'), s);
        const kept = load({ since: '2026-10-13T08:00:00Z', hold_drops: [], declared_full_stamps: [
            { id: 2, bin_id: 9, bin_label: 'SHG:0900', node: 'ALN_011', payload_code: 'SYN44-PARTA.06',
              counted: 97, declared: 97, actor: 'operator-7', applied_at: '2026-10-20T08:30:00Z' }] }).stamps;
        check('stamps: a no_pack row says the count was kept', kept.includes('kept: no pack for this part') && !kept.includes('→'), kept);
        check('stamps: no red chip', !s.includes('chip-err') && !s.includes('ledger-exceptions"'), s);
    }
}

console.log('inventory: an empty window is a quiet line, not a blank');
{
    const out = load({ since: '2026-10-13T08:00:00Z', hold_drops: [], declared_full_stamps: [] });
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
