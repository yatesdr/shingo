// s4-table.js — the positions table's and its settings' bugs (S4 U3:
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
                const need = steady(S, n, 'changeover_evac_nodes');
                const allowed = need !== 'forbidden' && need !== 'unused';
                if (page.pd('advShows')('evac_nodes', page.pd('setCtx')({ node: n })) !== allowed) bad.push(n + ' (' + S.model.cells[n].mode + ': ' + need + ')');
            }
            return [['every row', !bad.length, bad.join(', ')]];
        },
    },
    {
        // THE ROW'S ADVANCED (round 5): every row ends with one control,
        // "Advanced" with nothing set and "Advanced · N set" with N its
        // settings set (setCount), on the all-default and all-set styles.
        title: 'B6 · every row\'s Advanced control says how many of its settings are set',
        run: async page => {
            const S = page.S;
            const bad = [];
            for (const name of ['Style A', 'Style B']) {
                const style = S.composer.styles.find(s => s.name === name);
                page.click('root', { style: String(style.id) });
                await page.settle();
                for (const n of tableRows(page.S)) {
                    const chip = page.pd('setCount')(n);
                    const want = 'Advanced' + (chip ? ' · ' + chip + ' set' : '');
                    const got = (new RegExp('data-act="adv-open" data-node="' + n + '">([^<]*)</button>').exec(String(page.root.innerHTML)) || [])[1];
                    if (got !== want) bad.push(name + ' ' + n + ' reads ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
                }
            }
            return [['every row of Style A and Style B', !bad.length, bad.join('; ')]];
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
                const ctx = page.pd('setCtx')({ node: n });
                const shows = k => page.pd('advShows')(k, ctx);
                const curtain = !!((S.composer.cell.positions || []).find(p => p.core_node_name === n) || {}).curtain;
                if (shows('sequence')) seq.push(n + ' (' + page.S.model.cells[n].mode + ')');
                // SLICE 4: offered where a curtain is wired, and LISTED where
                // the flag is set without one (a recipe group lays it on every
                // claim it mints), so it can be seen and taken off.
                const set = page.pd('advIsSet')('curtain_required', ctx);
                if (shows('curtain_required') !== (curtain || set)) {
                    cur.push(n + (curtain ? ' has a curtain, hidden' : set ? ' is set with none, hidden' : ' has none, drawn'));
                }
            }
            return [
                ['Board order is drawn on no line-mode position', !seq.length, seq.join(', ')],
                ['Curtain required is drawn on the curtained position, and wherever it is set', !cur.length, cur.join(', ')],
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
            // ON AT ITS POINT (brief, 2026-11-14): an armed row reads "on at 15".
            out.push(['Set 15 writes reorder_point 15 and auto_reorder on; the cell reads on at 15', a.reorder_point === 15 && a.auto_reorder &&
                T2(cell('POS_A11')) === 'on at 15', 'point ' + a.reorder_point + ', auto ' + a.auto_reorder + ', cell ' + JSON.stringify(T2(cell('POS_A11')))]);
            press('off');
            a = M.advancedFor(page.S.model, 'POS_A11');
            out.push(['Off writes auto_reorder off and keeps the point; the cell reads off', a.reorder_point === 15 && !a.auto_reorder &&
                T2(cell('POS_A11')) === 'off', 'point ' + a.reorder_point + ', auto ' + a.auto_reorder + ', cell ' + JSON.stringify(T2(cell('POS_A11')))]);
            const sub = page.pd('SET_COPY').reorder[1];
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
        title: 'F27 · the desktop picture\'s pair cards say Paired core node, as the column does',
        run: async page => {
            const svg = () => String((page.byId('pd-svg') || {}).innerHTML || '');
            const pics = svg().replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ');
            const word = page.pd('M')().fieldLabel(page.S.model, 'paired_core_node');
            return [
                ['no card says "on deck for" or "paired with"', !/on deck for|paired with/.test(pics), (/(on deck for|paired with) \S+/.exec(pics) || [''])[0]],
                ['a pair card says "' + word + '"', pics.indexOf(word) >= 0, 'the picture: ' + pics.slice(0, 200)],
            ];
        },
    },
    {
        // ONE "+ Add a position" (round 4, 2026-11-15): the dashed row closing
        // the table; the head line's copy is gone.
        title: '+ Add a position is one control, the dashed row under the rows',
        run: async page => {
            const html = String(page.pd('positionsTable')());
            const adds = html.split('data-act="add-position"').length - 1;
            const row = html.indexOf('class="pd-addrowbtn" data-act="add-position"');
            const rows = html.indexOf('id="pd-postbl"');
            return [['one add control, the dashed row inside the rows\' box', adds === 1 && row > rows,
                adds + ' add controls; the dashed one at ' + row + ', rows at ' + rows]];
        },
    },
    {
        // A switch in a position's Advanced sheet (round 5) is the style's
        // draft, which Save style or Discard changes answers, as the list's
        // was; there is no Cancel to drop it.
        title: 'A position\'s Advanced sheet · a switch turned on is the style\'s draft, and off takes the draft back',
        run: async page => {
            const n = tableRows(page.S).find(x => page.pd('advShows')('strict_pack', page.pd('setCtx')({ node: x })));
            const dirty = () => page.pd('dirty')();
            const before = dirty();
            page.click('root', { act: 'adv-open', node: n });
            await page.settle();
            const sw = v => page.click('scrim', { act: 'set-switch', key: 'strict_pack', node: n, v: v }, { inside: ['.pd-modal'] });
            sw('1');
            await page.settle();
            const after = dirty();
            sw('0');
            await page.settle();
            const back = dirty();
            return [
                [n + ': the style as it opened is not dirty', !before, 'dirty before any change'],
                [n + ': Strict pack on makes the draft dirty', after, 'not dirty: Save style would not send it'],
                [n + ': off again, the draft is clean', !back, 'still dirty'],
            ];
        },
    },
    {
        // A SEQUENTIAL A/B PAIR IS TWO ROWS THAT NAME EACH OTHER (round 11).
        // The fixture pairs POS_A7 with POS_A8; POS_A5 is a 1-Robot Swap
        // row with staging of its own. No mode is spelled: the rows say them.
        title: 'Partner picked again · the old Sequential partner is unpaired, the new one paired both ways; an Index Swap back pairs nothing back',
        run: async page => {
            const labels = page.pd('M')().modeLabels();
            const was = JSON.parse(JSON.stringify(page.S.model.cells.POS_A5));
            const seq = page.S.model.cells.POS_A7.mode;
            const row = n => {
                const c = page.S.model.cells[n];
                return c.on ? labels[c.mode] + (c.paired ? ' naming ' + c.paired : ' naming none') + (c.part ? ', ' + c.part : '') : 'no row';
            };
            const names = n => (page.S.model.cells[n].on ? page.S.model.cells[n].paired || '' : '');
            T.pickChip(page, 'POS_A7', 'col:partner:paired', 'POS_A5');
            await page.settle();
            const out = [['POS_A5 picked for POS_A7: POS_A7 ' + row('POS_A7') + ' · POS_A5 ' + row('POS_A5') + ' · POS_A8 ' + row('POS_A8'), true, '']];
            out.push(['the new partner is paired both ways: POS_A5 names POS_A7, on Sequential A/B',
                names('POS_A7') === 'POS_A5' && names('POS_A5') === 'POS_A7' && page.S.model.cells.POS_A5.mode === seq, 'POS_A5 ' + row('POS_A5')]);
            out.push(['the old partner is unpaired: POS_A8 no longer names POS_A7', names('POS_A8') !== 'POS_A7', 'POS_A8 ' + row('POS_A8')]);
            out.push(['and is still a row of its own, with its swap and its part', page.S.model.cells.POS_A8.on &&
                page.S.model.cells.POS_A8.mode === seq && !!page.S.model.cells.POS_A8.part, 'POS_A8 ' + row('POS_A8')]);
            T.pickChip(page, 'POS_A7', 'col:partner:paired', 'POS_A8');
            await page.settle();
            const c5 = page.S.model.cells.POS_A5;
            out.push(['POS_A8 picked back: POS_A7 ' + row('POS_A7') + ' · POS_A8 ' + row('POS_A8') + ' · POS_A5 ' + row('POS_A5') +
                ' (staging ' + (c5.staging || '-') + ', ' + (c5.parkOld || '-') + ')', true, '']);
            out.push(['picked back, POS_A8 and POS_A7 name each other again', names('POS_A7') === 'POS_A8' && names('POS_A8') === 'POS_A7',
                'POS_A8 ' + row('POS_A8')]);
            out.push(['and POS_A5 has the swap it had before it was paired, with what it carried', c5.mode === was.mode && !names('POS_A5') &&
                c5.staging === was.staging && c5.parkOld === was.parkOld && c5.part === was.part, 'POS_A5 ' + row('POS_A5')]);
            // AN INDEX SWAP'S BACK IS NO ROW: picked again, the old back is
            // named by no position and the new one is not made a row.
            T.pickChip(page, 'POS_A1', 'col:partner:paired', 'POS_B4');
            await page.settle();
            const naming = n => Object.keys(page.S.model.cells).filter(x => page.S.model.cells[x].on && page.S.model.cells[x].paired === n);
            out.push(['POS_B4 picked for the Index Swap POS_A1: POS_A1 ' + row('POS_A1') + ' · POS_B1 named by ' + (naming('POS_B1').join(', ') || 'none') +
                ' · POS_B4 ' + row('POS_B4'), true, '']);
            out.push(['the old back is named by no position, and the new back is no row', names('POS_A1') === 'POS_B4' &&
                !naming('POS_B1').length && !page.S.model.cells.POS_B4.on, JSON.stringify({ B1: naming('POS_B1'), B4: row('POS_B4') })]);
            return out;
        },
    },
    {
        // NO PARTNER IS NO PAIR: the partner set to none (the action a none
        // row sends), or a swap that takes none, unpairs both. The fixture
        // pairs POS_A7 with POS_A8 and POS_A9 with POS_A10; POS_A5's swap
        // takes no partner.
        title: 'Partner cleared · no partner, or a swap with none, unpairs both rows of a Sequential A/B pair',
        run: async page => {
            const names = n => (page.S.model.cells[n].on ? page.S.model.cells[n].paired || '' : '');
            page.pd('apply')({ type: 'setPartner', node: 'POS_A7', partner: '' });
            await page.settle();
            const out = [['POS_A7 set to no partner: POS_A7 names ' + (names('POS_A7') || 'none') + ' · POS_A8 names ' + (names('POS_A8') || 'none'),
                !names('POS_A7') && names('POS_A8') !== 'POS_A7', 'POS_A8 still names POS_A7']];
            // ITS PARTNER CELL SAYS THE CHOICE (final brief 2b): a Sequential
            // A/B row with none is one node, not a gap.
            const cellText = String(page.pd('columnCell')('POS_A7', 'partner')).replace(/<[^>]*>/g, '');
            out.push(['POS_A7\'s partner cell reads ' + JSON.stringify(cellText),
                cellText === page.pd('M')().NO_PARTNER, 'the cell does not name the choice']);
            // AND ITS GLYPH IS ONE SLOT: the row's mode menu draws its
            // Sequential A/B as the shape it runs; POS_A9, still paired, two.
            const slots = n => (String((page.pd('optionsFor')(n, 'mode').find(o => o.value === page.S.model.cells[n].mode) || {}).icon || '')
                .match(/class="slot"/g) || []).length;
            out.push(['the mode menu\'s Sequential A/B glyph: POS_A7 ' + slots('POS_A7') + ' slot(s), POS_A9 ' + slots('POS_A9'),
                slots('POS_A7') === 1 && slots('POS_A9') === 2, 'the one-node row does not draw one slot']);
            T.pickChip(page, 'POS_A9', 'mode', page.S.model.cells.POS_A5.mode);
            await page.settle();
            out.push(['POS_A9 given a swap with no partner: POS_A9 names ' + (names('POS_A9') || 'none') + ' · POS_A10 names ' + (names('POS_A10') || 'none'),
                !names('POS_A9') && names('POS_A10') !== 'POS_A9', 'POS_A10 still names POS_A9']);
            return out;
        },
    },
    {
        // NO STAGING IS THE SOURCE NAMED AGAIN (brief G C2): a 2-Robot Swap
        // whose source is one node offers "No staging (waits at the source)"
        // first on its inbound staging pick, which stores the source; a
        // staging equal to the source reads back as that choice, on the pick
        // and on the row; a group source is offered none. Changing the
        // source later leaves the staging naming the old node, and the row
        // then reads that node. The fixture's POS_A3 and POS_A4 are 2-Robot
        // Swaps from GRP_A (a group), POS_A3 staging at STG_1; SRC_1 is one
        // node.
        title: 'No staging · a 2-Robot Swap from one node may wait at its source instead of staging',
        run: async page => {
            const apply = page.pd('apply'), optionsFor = page.pd('optionsFor');
            await page.pd('loadCoreNodes')();
            const word = page.pd('typeof NO_STAGING === "string" ? NO_STAGING : ""') || 'No staging (waits at the source)';
            const choice = n => optionsFor(n, 'col:staging:staging').find(o => o.label === word);
            const cellText = n => String(page.pd('columnCell')(n, 'staging')).replace(/<[^>]*>/g, '');
            const out = [];
            out.push(['POS_A4, from the group GRP_A, is offered no such choice', !choice('POS_A4'),
                JSON.stringify(choice('POS_A4'))]);
            apply({ type: 'setSource', node: 'POS_A3', source: 'SRC_1' });
            await page.settle();
            const c = choice('POS_A3');
            out.push(['POS_A3 from SRC_1: its staging pick offers ' + JSON.stringify(word) + ' storing SRC_1, not picked',
                !!c && c.value === 'SRC_1' && !c.on && !!c.action && c.action.type === 'setStaging' && c.action.staging === 'SRC_1',
                JSON.stringify(c)]);
            const first = optionsFor('POS_A3', 'col:staging:staging').filter(o => !o.note)[0];
            out.push(['and offers it first', !!first && first.label === word, JSON.stringify(first)]);
            if (!c) return out;
            T.pickChip(page, 'POS_A3', 'col:staging:staging', 'SRC_1');
            await page.settle();
            const cell = page.S.model.cells.POS_A3;
            out.push(['picked: POS_A3 stores inbound staging ' + JSON.stringify(cell.staging), cell.staging === 'SRC_1', '']);
            out.push(['the pick reads back as the choice', !!choice('POS_A3') && choice('POS_A3').on,
                JSON.stringify(choice('POS_A3'))]);
            const short = word.split(' (')[0];
            out.push(['the row reads ' + JSON.stringify(cellText('POS_A3')) + ', all of the choice and its node in the title',
                cellText('POS_A3').indexOf(short) >= 0 &&
                String(page.pd('columnCell')('POS_A3', 'staging')).indexOf('SRC_1: ' + word) >= 0,
                'the row names the node, not the choice']);
            out.push(['SRC_1 is not offered twice', optionsFor('POS_A3', 'col:staging:staging').filter(o => o.value === 'SRC_1').length === 1,
                JSON.stringify(optionsFor('POS_A3', 'col:staging:staging').map(o => o.value))]);
            apply({ type: 'setSource', node: 'POS_A3', source: 'GRP_A' });
            await page.settle();
            out.push(['the source moved to GRP_A: the staging is left at ' + JSON.stringify(page.S.model.cells.POS_A3.staging) +
                ' and the row reads ' + JSON.stringify(cellText('POS_A3')),
                page.S.model.cells.POS_A3.staging === 'SRC_1' && cellText('POS_A3').indexOf(short) < 0,
                'the row still reads the choice']);
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
    {
        // A SEQUENTIAL ROW MOVED KEEPS ITS PAIR (round 11): the Position pick
        // moves POS_A7's settings to a free position, and its partner POS_A8
        // must name the new one, not the empty POS_A7 it left.
        title: 'Partner moved · a Sequential A/B row moved to another position is still named by its partner',
        run: async page => {
            const names = n => (page.S.model.cells[n] && page.S.model.cells[n].on ? page.S.model.cells[n].paired || '' : '');
            const to = page.pd('M')().positionChoices(page.S.model, 'POS_A7')[0];
            page.pd('apply')({ type: 'movePosition', from: 'POS_A7', to: to });
            await page.settle();
            return [
                ['POS_A7 moved to ' + to + ': ' + to + ' names ' + (names(to) || 'none') + ' · POS_A8 names ' + (names('POS_A8') || 'none'), true, ''],
                ['the moved row and its partner name each other', names(to) === 'POS_A8' && names('POS_A8') === to,
                    'POS_A8 still names ' + (names('POS_A8') || 'none')],
            ];
        },
    },
];

async function pinTableBugs() {
    const L = ['# The positions table\'s and its settings\' bugs (S4 U3), each on a fresh page of the fixture.',
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
