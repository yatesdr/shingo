// s4-table.js — the positions table's and the Advanced sheet's bugs (S4 U3:
// B4, B5, B6, B7, F15, F26), one block per bug, each on a fresh page of the
// fixture's plant. A line is `ok <what must hold>` or `BUG <what must hold>:
// <what happened>`; the golden holds what the page does, and a fixed bug's
// block reads only ok lines.
//
// Run from pins.js with its drivers (T): openPage, pickRow.

'use strict';

let T = null;

// The rows the positions table draws, in its order.
function tableRows(S) {
    return S.model.positions.map(p => p.core_node_name).filter(n => S.model.cells[n].on && S.model.cells[n].mode);
}
function stationOf(S, n) {
    return ((S.composer.cell.positions || []).find(p => p.core_node_name === n) || {}).station_id || 0;
}
// flowspec's word for a field on a row, read off the page's own realm
// (flowspec-data.js sets window.FLOWSPEC there).
let realm = null;
function steady(S, n, field) {
    const c = S.model.cells[n];
    const row = ((realm.FLOWSPEC || {}).steady || {})[c.role] || {};
    return (row[c.mode] || {})[field] || '';
}

const BUGS = [
    {
        title: 'B4 · another screen\'s positions are never move or paired targets',
        run: async page => {
            const S = page.S, optionsFor = page.pd('optionsFor');
            const out = [];
            for (const kind of ['position', 'col:partner:paired', 'col:partner:secondPaired']) {
                const bad = [];
                for (const n of tableRows(S)) {
                    const mine = stationOf(S, n);
                    for (const o of optionsFor(n, kind)) {
                        const st = o.value ? stationOf(S, o.value) : 0;
                        if (st && st !== mine) bad.push(n + '→' + o.value);
                    }
                }
                out.push([kind + ' offers no position bound to another screen', !bad.length, bad.join(', ')]);
            }
            return out;
        },
    },
    {
        title: 'B5 · Tool change is drawn exactly where flowspec lets the mode carry changeover_evac_nodes',
        run: async page => {
            const S = page.S;
            const bad = [];
            for (const n of tableRows(S)) {
                page.click('root', { act: 'advanced', node: n });
                const need = steady(S, n, 'changeover_evac_nodes');
                const allowed = need !== 'forbidden' && need !== 'unused';
                if (page.pd('advShows')('evac_nodes') !== allowed) bad.push(n + ' (' + S.model.cells[n].mode + ': ' + need + ')');
                page.click('scrim', { act: 'adv-cancel' });
            }
            return [['every row', !bad.length, bad.join(', ')]];
        },
    },
    {
        title: 'B6 · the row\'s "N set" is the sheet\'s own total',
        run: async page => {
            const S = page.S;
            const style = S.composer.styles.find(s => s.name === 'Style B');
            page.click('root', { style: String(style.id) });
            await page.settle();
            const bad = [];
            for (const n of tableRows(page.S)) {
                const badge = page.pd('advancedCount')(n);
                page.click('root', { act: 'advanced', node: n });
                const secs = [...page.scrim.innerHTML.matchAll(/<div class="pd-sec"><div class="pd-lbl">[^<]*(?:<span class="cnt">(\d+) set<\/span>)?<\/div>/g)];
                const total = secs.reduce((t, m) => t + Number(m[1] || 0), 0);
                if (badge !== total) bad.push(n + ' badge ' + badge + ', sheet ' + total);
                page.click('scrim', { act: 'adv-cancel' });
            }
            return [['every row of Style B', !bad.length, bad.join('; ')]];
        },
    },
    {
        title: 'F15 · Board order only on a loader position; Curtain required only where the node has a curtain',
        run: async page => {
            const S = page.S;
            const style = S.composer.styles.find(s => s.name === 'Style B');
            page.click('root', { style: String(style.id) });
            await page.settle();
            const seq = [], cur = [], counted = [];
            for (const n of tableRows(page.S)) {
                page.click('root', { act: 'advanced', node: n });
                const shows = page.pd('advShows');
                const curtain = !!((S.composer.cell.positions || []).find(p => p.core_node_name === n) || {}).curtain;
                if (shows('sequence')) seq.push(n + ' (' + page.S.model.cells[n].mode + ')');
                if (shows('curtain_required') !== curtain) cur.push(n + (curtain ? ' has a curtain, hidden' : ' has none, drawn'));
                page.click('scrim', { act: 'adv-cancel' });
            }
            return [
                ['Board order is drawn on no line-mode position', !seq.length, seq.join(', ')],
                ['Curtain required is drawn exactly on the curtained position', !cur.length, cur.join(', ')],
            ];
        },
    },
    {
        title: 'B7 · the key-route picker offers no waypoint already on the route',
        run: async page => {
            const S = page.S, optionsFor = page.pd('optionsFor');
            const out = [];
            for (const n of tableRows(S)) {
                const route = S.model.cells[n].keyRoute || [];
                route.forEach((w, i) => {
                    const twice = optionsFor(n, 'via:' + i).filter(o => o.value && o.value !== w && route.indexOf(o.value) >= 0)
                        .map(o => o.value);
                    out.push([n + ' via:' + i + ' (' + w + ' of ' + route.join(' › ') + ')', !twice.length,
                        'offers ' + twice.join(', ') + ', already on the route']);
                });
            }
            return out;
        },
    },
    {
        title: 'F26 · a swap-mode change keeps what the new mode uses and says what it dropped',
        run: async page => {
            const S = page.S;
            const before = Object.assign({}, S.model.cells.POS_A5);
            // POS_A3's mode: the one that keeps inbound staging and forbids
            // outbound (no mode is spelled here; the fixture says it).
            const to = S.model.cells.POS_A3.mode;
            const from = page.mark();
            T.pickChip(page, 'POS_A5', 'mode', to);
            await page.settle();
            const c = page.S.model.cells.POS_A5;
            const said = page.noticesSince(from).map(x => x.text || x.message || String(x)).join(' / ');
            return [
                ['inbound staging ' + before.staging + ' is kept (2-Robot Swap uses it)', c.staging === before.staging, 'it is ' + JSON.stringify(c.staging)],
                ['outbound staging ' + before.parkOld + ' is dropped (2-Robot Swap forbids it)', !c.parkOld, 'it is ' + JSON.stringify(c.parkOld)],
                ['the page says outbound staging ' + before.parkOld + ' was dropped', /outbound staging/i.test(said) && said.indexOf(before.parkOld) >= 0,
                    said ? 'it said ' + JSON.stringify(said) : 'nothing was said'],
                ['the words: ' + JSON.stringify(said), true, ''],
            ];
        },
    },
    {
        title: 'U3 · Auto reorder on the row: off on a new position of either role; Set turns it on at the point; Off turns it off',
        run: async page => {
            const S = page.S, M = page.pd('M')();
            const apply = page.pd('apply'), cell = page.pd('reorderCell');
            const T2 = h => String(h).replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ').trim();
            const mode = Object.keys(M.modeLabels())[0];
            apply({ type: 'addPosition', node: 'POS_A11', mode: mode });
            const out = [];
            for (const role of ['consume', 'produce']) {
                apply({ type: 'setRole', node: 'POS_A11', role: role });
                const a = M.advancedFor(page.S.model, 'POS_A11');
                out.push(['a new ' + role + ' position reads off', T2(cell('POS_A11')) === 'off' && !a.auto_reorder && !a.reorder_point,
                    'it reads ' + JSON.stringify(T2(cell('POS_A11'))) + ', point ' + a.reorder_point + ', auto ' + a.auto_reorder]);
            }
            const press = which => {
                page.click('root', { act: 'pick', node: 'POS_A11', kind: 'reorder' });
                if (which === 'set') page.type('rp', '15');
                const row = page.rows('pd-pop', 'data-rp').find(r => r.dataset.rp === which);
                if (!row) throw new Error('the reorder popover has no ' + which + ': ' + (page.byId('pd-pop') || {}).innerHTML);
                page.byId('pd-pop').onclick({ target: row, stopPropagation() {} });
            };
            press('set');
            let a = M.advancedFor(page.S.model, 'POS_A11');
            out.push(['Set 15 writes reorder_point 15 and auto_reorder on; the cell reads 15', a.reorder_point === 15 && a.auto_reorder &&
                T2(cell('POS_A11')) === '15', 'point ' + a.reorder_point + ', auto ' + a.auto_reorder + ', cell ' + JSON.stringify(T2(cell('POS_A11')))]);
            press('off');
            a = M.advancedFor(page.S.model, 'POS_A11');
            out.push(['Off writes auto_reorder off and keeps the point; the cell reads off', a.reorder_point === 15 && !a.auto_reorder &&
                T2(cell('POS_A11')) === 'off', 'point ' + a.reorder_point + ', auto ' + a.auto_reorder + ', cell ' + JSON.stringify(T2(cell('POS_A11')))]);
            const sub = page.pd('ADV_COPY').reorder_point[1];
            out.push(['the reorder point\'s sub-label speaks of the bin, not of what is left in it: ' + JSON.stringify(sub), !/left/.test(sub), 'consume-only words']);
            return out;
        },
    },
    {
        title: 'R2 · a column no row uses is not drawn; a row that does not use a drawn column leaves its cell empty',
        run: async page => {
            const S = page.S, M = page.pd('M')();
            const heads = () => [...(/<thead>([\s\S]*?)<\/thead>/.exec(page.pd('positionsTable')()) || ['', ''])[1]
                .matchAll(/<th>([\s\S]*?)<\/th>/g)].map(m => m[1]);
            const paired = M.fieldLabel(S.model, 'paired_core_node');
            const out = [['every mode on the table: ' + paired + ' is drawn', heads().indexOf(paired) >= 0, heads().join(' · ')]];
            // Take off every row whose mode pairs (press index, sequential).
            for (const n of tableRows(S)) {
                if (M.rowColumns(page.S.model, n, 'partner').length) page.pd('apply')({ type: 'removePosition', node: n });
            }
            const left = tableRows(page.S).map(n => n + ' ' + page.S.model.cells[n].mode);
            out.push(['only ' + left.join(', ') + ' left: ' + paired + ' is not drawn', heads().indexOf(paired) < 0, heads().join(' · ')]);
            return out;
        },
    },
    {
        title: 'F24 · R3 · the desktop\'s bar never says "Tap the position" or "use a preset"',
        run: async page => {
            const M = page.pd('M')(), apply = page.pd('apply');
            const bar = () => String((page.byId('pd-bar') || {}).innerHTML || '').replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ').trim();
            const out = [];
            apply({ type: 'addPosition', node: 'POS_A11' });
            out.push(['a position with no swap mode: ' + JSON.stringify(bar()), !/Tap|choreography/.test(bar()), 'the HMI\'s words']);
            apply({ type: 'setMode', node: 'POS_A11', mode: Object.keys(M.modeLabels())[1] });
            out.push(['a position with no part: ' + JSON.stringify(bar()), !/Tap/.test(bar()), 'the HMI\'s words']);
            return out;
        },
    },
];

async function pinTableBugs() {
    const L = ['# The positions table\'s and Advanced sheet\'s bugs (S4 U3), each on a fresh page of the fixture.',
        '# "ok" is what must hold; "BUG" is the bug, and what the page did.'];
    for (const b of BUGS) {
        const page = await T.openPage();
        realm = page.ctx;
        L.push('', '## ' + b.title);
        for (const [holds, ok, detail] of await b.run(page)) L.push(ok ? 'ok   ' + holds : 'BUG  ' + holds + ': ' + detail);
    }
    T.out['table-bugs.txt'] = L.join('\n') + '\n';
}

async function run(t) {
    T = t;
    await pinTableBugs();
}

module.exports = { run };
