// bigline.js — the Processes page on a big line: 23 positions on two screens,
// 108 plant nodes, 46 parts, 13 styles, 3 recipes in 12 versions, 2 light
// curtains and 1 style setting. Press 6's shape at SPR, every name invented.
//
// WHY A SECOND PLANT. fixture.json is a 12-position line, and every pin there
// was taken on it. The 23-row wall on Settings shipped because nothing was ever
// looked at on a line the size of a real press. Each slice of the one-process-
// page build is checked here before and after, and these goldens are where it
// shows what moved.
//
// What it writes, one golden each (TestBigLinePagePins):
//   - bigline-requests.txt: each view's requests in order, with the size of
//     each answer, and a total. A view that asks once per position shows it.
//   - bigline-settings.txt: what Settings draws at this size, counted: the
//     old one-row-per-position wall (0 since the exception list), what each
//     exception list says and how many rows it draws, the chips and the save
//     bar.
//
// The page runs unmodified under page-harness.js, as for pins.js.
'use strict';
const H = require('./page-harness.js');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

// The press-index mode as the generated flowspec names it (flowspec-data.js,
// from domain/flowspec and protocol/swap_mode.go): the fixture takes the name
// from there rather than spelling a mode itself.
const PRESS_INDEX = (() => {
    const w = {};
    vm.runInNewContext(fs.readFileSync(path.join(__dirname, '..', '..', 'static', 'operator-station', 'flowspec-data.js'), 'utf8'), { window: w });
    const m = w.FLOWSPEC.modes.find(x => /press_index$/.test(x));
    if (!m) throw new Error('flowspec-data.js names no press-index mode');
    return m;
})();

// ── the line ─────────────────────────────────────────────────────────────────
const MAIN = ['BPN_008', 'BPN_005', 'BPN_006BCD', 'BPN_006EFG', 'BPN_006HIJ', 'BPN_006KLM', 'BPN_006NOP',
    'BPN_007BCD', 'BPN_007EFG', 'BPN_007HIJ', 'BPN_007KLM', 'BPN_007NOP', 'BPN_009BCD', 'BPN_009EFG',
    'BPN_009HIJ', 'BPN_009KLM', 'BPN_009NOP', 'BPN_006PAL', 'BPN_005PAL', 'BPN_008PAL', 'BPN_009PAL'];
const UNL = ['BUN_002', 'BUN_005'];
const CURTAINS = ['BPN_006PAL', 'BPN_009PAL'];

// bigMap is a plant-sized map, as Core sends it: plant A's scene has 383
// points and 635 lanes. Every node of the line is a point; the rest are lane
// marks (BLM_###), and each lane joins neighbours on a grid, some curved.
function bigMap(core) {
    const names = core.filter(c => c.node_type !== 'NGRP').map(c => c.name);
    for (let i = 1; names.length < 383; i++) names.push('BLM_' + String(i).padStart(3, '0'));
    const at = i => ({ x: (i % 20) * 3.5, y: Math.floor(i / 20) * 3.5 });
    const points = Object.fromEntries(names.map((n, i) => [n, Object.assign({ class: n.startsWith('BLM_') ? 'LocationMark' : 'ActionPoint' }, at(i))]));
    const edges = [];
    for (let i = 0; edges.length < 635; i = (i + 1) % names.length) {
        const j = (i + 1 + (edges.length % 3) * 19) % names.length;
        const a = at(i), b = at(j);
        const e = { from: names[i], to: names[j], len: Math.round(Math.hypot(a.x - b.x, a.y - b.y) * 100) / 100 };
        if (edges.length % 4 === 0) e.h = [a.x + 1, a.y + 0.5, b.x - 1, b.y - 0.5];
        edges.push(e);
    }
    return { revision: 'map-big', points: points, edges: edges };
}
const SRC = ['BSM_038', 'BSM_039', 'BSM_040', 'BSM_041'];
const DST = ['BUN_002', 'BUN_003', 'BUN_004', 'BUN_005'];
const pad = (n, w) => String(n).padStart(w, '0');
const PARTS = Array.from({ length: 46 }, (_, i) => 'BP-PART-' + pad(i + 1, 2));

function bigLine(o) {
    o = o || {};
    const positions = MAIN.concat(UNL);
    const stations = [
        { id: 61, process_id: 1, name: 'BIG-PRESS Operator Screen', code: 'big-press', note: '', area_label: '', sequence: 1, controller_node_id: '', enabled: true },
        { id: 62, process_id: 1, name: 'BIG Unloader', code: 'big-unl', note: '', area_label: '', sequence: 2, controller_node_id: '', enabled: true },
    ];
    const process = {
        id: 1, name: 'BIG-PRESS', description: 'Synthetic big press', group_id: 7, active_style_id: 1,
        flow_composer_enabled: true, counter_plc_name: 'PLC_BIG', counter_tag_name: 'BigCount', counter_enabled: true,
        identity_tag_name: '', identity_none: false, changeover_auto_arm: 'off',
        quality_hold_enabled: false, quality_hold_destination: '',
        // The list row as the server sends it, first paint included.
        curtain_nodes: CURTAINS.slice(),
    };
    // 13 styles: the running one works BPN_007KLM decked by BPN_008 and sets
    // its next carrier; the rest each work one or two positions.
    const pressPos = MAIN.filter(n => n !== 'BPN_008' && n !== 'BPN_005');
    const claim = (node, part, deck) => ({
        core_node_name: node, role: 'produce', swap_mode: PRESS_INDEX, payload_code: part,
        paired_core_node: deck, inbound_source: 'BIG Buffer Zone', outbound_destination: 'BIG Unloader Zone',
    });
    const styles = [];
    for (let i = 0; i < 13; i++) {
        const node = i === 0 ? 'BPN_007KLM' : pressPos[i % pressPos.length];
        const deck = node.startsWith('BPN_009') ? 'BPN_008' : 'BPN_005';
        const part = PARTS[(i * 3) % PARTS.length];
        const claims = [claim(node, part, i === 0 ? 'BPN_008' : deck)];
        if (i % 4 === 1) claims.push(claim(node.replace('BPN_006', 'BPN_009'), PARTS[(i * 3 + 1) % PARTS.length], 'BPN_008'));
        styles.push({
            id: i + 1, name: part + (claims.length > 1 ? ' (two spots)' : ''), catid: '', claim_count: claims.length,
            claim_modes: [PRESS_INDEX], last_run: i < 9 ? '11-' + pad(13 - (i % 9), 2) : null,
            claims: claims, advanced: i === 0 ? { BPN_007KLM: { refill_by: 'index_robot' } } : {},
        });
    }
    // 108 plant nodes: this line's 23 positions and its 6 other spots, four
    // groups, and the rest of a plant.
    const core = [
        { name: 'BIG Buffer Zone', node_type: 'NGRP', members: SRC },
        { name: 'BIG Unloader Zone', node_type: 'NGRP', members: DST },
        { name: 'BIG Supermarket', node_type: 'NGRP', members: Array.from({ length: 8 }, (_, i) => 'BSM_' + pad(i + 1, 3)) },
        { name: 'BIG FG stage 1', node_type: 'NGRP', members: ['BFG_001', 'BFG_002', 'BFG_003', 'BFG_004'] },
    ];
    const seen = new Set(core.map(c => c.name));
    const add = (name, t) => { if (!seen.has(name)) { seen.add(name); core.push({ name: name, node_type: t || 'storage' }); } };
    positions.forEach(n => add(n, 'station'));
    SRC.concat(DST).forEach(n => add(n));
    core[2].members.concat(core[3].members).forEach(n => add(n));
    for (let i = 1; core.length < 108; i++) add('BXN_' + pad(i, 3));
    const routingNodes = [
        { id: 401, core_node_name: 'BIG Buffer Zone', role: 'source', sequence: 1, enabled: true, origin: 'engineer', label: 'BIG Buffer Zone' },
        { id: 402, core_node_name: 'BIG Unloader Zone', role: 'destination', sequence: 2, enabled: true, origin: 'engineer', label: 'BIG Unloader Zone' },
    ];
    const processNodes = positions.map((n, i) => ({
        id: 501 + i, process_id: 1, process_name: 'BIG-PRESS', operator_station_id: i < MAIN.length ? 61 : 62,
        station_name: i < MAIN.length ? 'BIG-PRESS Operator Screen' : 'BIG Unloader', core_node_name: n, name: n,
        sequence: i + 1, enabled: true, curtain_enabled: CURTAINS.includes(n),
        curtain_plc_name: CURTAINS.includes(n) ? 'PLC_BIG' : '', curtain_tag_name: CURTAINS.includes(n) ? 'Curtain_' + n : '',
        curtain_safe_value: CURTAINS.includes(n) ? true : null,
    }));
    const recipes = [['One Spot', 6, ['BPN_007NOP']], ['Totes', 2, ['BPN_006PAL', 'BPN_009PAL']], ['Two Spots', 4, ['BPN_006BCD', 'BPN_009BCD']]];
    let rid = 800;
    const presets = [];
    for (const [name, versions, spots] of recipes) {
        for (let v = versions; v >= 1; v--) {
            presets.push({ id: ++rid, name: 'Big Index - ' + name, version: v,
                shape: spots.map(n => ({ core_node_name: n, role: 'produce', swap_mode: PRESS_INDEX, payload_code: '',
                    paired_core_node: n.startsWith('BPN_009') ? 'BPN_008' : 'BPN_005', inbound_source: 'BIG Buffer Zone',
                    outbound_destination: 'BIG Unloader Zone' })), members: [] });
        }
    }
    const cellPositions = positions.map((n, i) => ({
        core_node_name: n, sequence: i + 1, x: 2 + (i % 7) * 4, y: 4 + Math.floor(i / 7) * 6, kind: 'front',
        station_id: i < MAIN.length ? 61 : 62, curtain: CURTAINS.includes(n),
    }));
    return {
        page: {
            activeProcessId: 1, processes: [process], processGroups: [{ id: 7, name: 'Big Presses' }],
            styles: styles.map(s => ({ id: s.id, process_id: 1, name: s.name, description: '', expected_catid: '' })),
            stations: stations, stationNodes: { 61: MAIN.slice(), 62: UNL.slice() }, loaderGaps: [],
        },
        composer: {
            styles: styles, routing: routingNodes.map(r => ({ core_node_name: r.core_node_name, role: r.role, sequence: r.sequence })),
            routing_off: {}, presets: [], palette: PARTS.slice(),
            cell: { positions: cellPositions, geometry: true, groups: { 'BIG Buffer Zone': SRC, 'BIG Unloader Zone': DST } },
            map: bigMap(core),
        },
        server: {
            processNodes: processNodes, coreNodes: core, routingNodes: routingNodes, partSet: PARTS.slice(),
            payloadCatalog: PARTS.map(p => ({ code: p, name: 'Synthetic ' + p })),
            presetsView: { presets: presets, candidates: [] },
            stationCounters: o.stationCounter ? [{ id: 71, process_id: 1, label: 'Load station', plc_name: 'PLC_BIG',
                tag_name: 'LoadCount', last_count: 0, warlink_managed: false }] : [],
        },
    };
}

// ── its server, and what each answer weighs ──────────────────────────────────
function serverFor(FX, log) {
    const id = (u, re) => { const m = re.exec(u); return m ? m[1] : null; };
    const routes = [
        ['GET', /^\/api\/catid-health$/, () => ({ items: [] })],
        ['GET', /^\/api\/processes$/, () => FX.page.processes],
        ['GET', /^\/api\/processes\/\d+\/composer$/, () => FX.composer],
        ['GET', /^\/api\/processes\/\d+\/composer\/lists$/, () => ({ styles: FX.composer.styles, presets: FX.composer.presets })],
        ['POST', /^\/api\/processes\/\d+\/flow\/preview$/, () => ({ fingerprint: 'fp-big', findings: [], running: true })],
        ['GET', /^\/api\/processes\/\d+\/styles$/, () => FX.page.styles],
        ['GET', /^\/api\/processes\/\d+\/routing-nodes$/, () => ({ rows: FX.server.routingNodes })],
        ['GET', /^\/api\/processes\/\d+\/payloads$/, () => FX.server.partSet],
        ['GET', /^\/api\/processes\/\d+\/presets$/, () => FX.server.presetsView],
        ['GET', /^\/api\/processes\/\d+\/station-counters$/, () => FX.server.stationCounters],
        ['GET', /^\/api\/processes\/\d+\/counting-slots$/, () => ({ slots: [] })],
        ['GET', /^\/api\/processes\/\d+\/catid-health$/, () => ({ items: [] })],
        ['GET', /^\/api\/processes\/\d+\/auto-reorder$/, () => ({ shown_on_station: false, positions: 23, switched_off: 0, flips: [] })],
        ['GET', /^\/api\/operator-stations$/, () => FX.page.stations],
        ['GET', /^\/api\/operator-stations\/\d+\/claimed-nodes$/, u => FX.page.stationNodes[id(u, /stations\/(\d+)\//)] || []],
        ['GET', /^\/api\/process-nodes$/, () => FX.server.processNodes],
        ['GET', /^\/api\/processes\/\d+\/countable$/, () => Object.fromEntries(FX.server.processNodes
            .map(n => [n.id, { countable: true, refusal: '' }]))],
        ['GET', /^\/api\/core-nodes$/, () => FX.server.coreNodes],
        ['GET', /^\/api\/plcs$/, () => [{ name: 'PLC_BIG', connected: true }]],
        ['GET', /^\/api\/payload-catalog$/, () => FX.server.payloadCatalog],
    ];
    return (method, url) => {
        const u = url.split('?')[0];
        for (const [m, re, fn] of routes) {
            if (m === method && re.test(u)) {
                const body = fn(u);
                log.push({ method: method, url: url, bytes: Buffer.byteLength(JSON.stringify(body)) });
                return { body: body };
            }
        }
        log.push({ method: method, url: url, bytes: 0, unanswered: true });
        return undefined;
    };
}

// A position's id in a URL reads as its role, so a per-position request shows
// as one line with its count.
function fmtView(title, log) {
    const lines = ['== ' + title];
    const grouped = [];
    for (const c of log) {
        const key = c.method + ' ' + c.url.replace(/\/process-nodes\/\d+\//, '/process-nodes/{id}/') + (c.unanswered ? '  (unanswered)' : '');
        const g = grouped.find(x => x.key === key);
        if (g) { g.n++; g.bytes += c.bytes; } else grouped.push({ key: key, n: 1, bytes: c.bytes });
    }
    for (const g of grouped) lines.push((g.n > 1 ? g.n + ' × ' : '') + g.key + '  ' + g.bytes + ' B');
    const total = log.reduce((a, c) => a + c.bytes, 0);
    lines.push('-- ' + log.length + ' requests, ' + total + ' B of answers');
    return lines.join('\n');
}

// partChips is the part set picker's chips as drawn: how many, and the fold's
// words after them.
function partChips(page) {
    const host = page.byId('npk-parts');
    if (!host) return 'no part set picker';
    const chips = host.querySelectorAll('.picker-chip').length;
    const more = host.querySelectorAll('.picker-more').map(b => b.textContent);
    return chips + (more.length ? ' · then ' + JSON.stringify(more[0]) : '');
}

async function open(FX, search, hash) {
    const log = [];
    const page = await H.open({ page: FX.page, search: search, hash: hash || '', serve: serverFor(FX, log) });
    await page.settle();
    return { page: page, log: log };
}

// count of a string in the page's markup, the way the page wrote it.
const count = (html, s) => html.split(s).length - 1;

// excSum and excRows read one exception list (shared/exceptions.js) by its
// data-exc: the rule it says, and how many positions it lists.
function excList(html, id) {
    const i = html.indexOf('<div class="exc" data-exc="' + id + '">');
    if (i < 0) return '';
    const j = html.indexOf('<div class="exc"', i + 1);
    return html.slice(i, j < 0 ? undefined : j);
}
function excSum(html, id) {
    const m = /<div class="exc-sum"><span>([^<]*)<\/span>/.exec(excList(html, id));
    return m ? m[1] : '(not drawn)';
}
const excRows = (html, id) => count(excList(html, id), '<td class="exc-pos">');

(async () => {
    const out = {};
    const req = [];
    const FX = bigLine();
    const FXC = bigLine({ stationCounter: true });

    let v = await open(FX, '', '');
    req.push(fmtView('process list (page data, then its first read)', v.log));

    v = await open(FX, '?process=1', '');
    req.push(fmtView('open BIG-PRESS (Styles, the running style)', v.log));
    const mark = v.log.length;
    await v.page.pd('refreshRail')();
    await v.page.settle();
    req.push(fmtView('the Styles poll, once (it runs every 10 s and on focus)', v.log.slice(mark)));

    v = await open(FX, '?process=1', '#tab=settings');
    req.push(fmtView('Settings, opened', v.log));
    const html = v.page.root.innerHTML || '';
    const S = v.page.S;

    const vc = await open(FXC, '?process=1', '#tab=settings');
    req.push(fmtView('Settings, opened, with one station counter', vc.log));
    out['bigline-requests.txt'] = req.join('\n\n') + '\n';

    const shape = [
        'positions on the line: ' + (S.curtainNodes || []).length,
        'position rows drawn (each "light curtain …" line): ' + count(html, 'light curtain '),
        'positions whose curtain is on: ' + (S.curtainNodes || []).filter(n => n.curtain_enabled).length,
        'rows saying the curtain is off: ' + count(html, 'light curtain off'),
        'Light curtain says: ' + excSum(html, 'curtain') + ' · rows: ' + excRows(html, 'curtain'),
        'Counted at says: ' + excSum(html, 'counted') + ' · rows: ' + excRows(html, 'counted'),
        'Counted at, with one station counter, says: ' + excSum(vc.page.root.innerHTML || '', 'counted'),
        'parts in the part set: ' + (S.partSet || FX.server.partSet).length,
        // The part set's chips fold past PART_FOLD (slice 5): "+ N more · show all".
        'part set chips drawn: ' + partChips(v.page),
        'recipe versions listed: ' + (html.match(/<tr class="[^"]*" data-preset="/g) || []).length,
        'save bar: ' + (count(html, 'pd-savebar') ? 'yes' : 'no'),
        'markup written, characters: ' + html.length,
    ];
    out['bigline-settings.txt'] = shape.join('\n') + '\n';
    process.stdout.write(JSON.stringify(out));
})().catch(e => { process.stderr.write(String(e && e.stack || e) + '\n'); process.exit(1); });
