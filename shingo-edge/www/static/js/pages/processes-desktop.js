// processes-desktop.js — the Edge Processes page: P0 (the list) and D1 (Flows).
//
// Ported from desktop/REFERENCE-press400-desktop-U9-2026-09-10.html against
// SPEC-desktop-ux-2026-09-10.md. Sizes, copy, order and colours are theirs.
//
// ONE THING, TWO SCREENS (SPEC §0.1). The draft is composer-model.js — the same
// pure model the station HMI runs, characterization-tested against Go's own
// Collapse — and the picture is renderFlowPicture, the same function U4 opens
// read-only and U8 edits. Nothing here re-implements either. An engineer and an
// operator looking at one part are looking at one flow, and that is only true
// while there is one model and one drawing behind both.
//
// THE DESKTOP VERB IS SAVE (SPEC §0.5). This page never calls changeover/start.

import { renderFlowPicture, pictureRows, sentencesFromModel } from '/static/operator-station/operator-flow.js';
import { esc } from '/static/shared/esc.js';
import {
    makeProjector, rotate90For, dist2, cubicLength, cubicPathD, laneKey,
} from '/static/shared/scene-geom.js';
// The edge's own modal helpers. header.html loads shingoedge.js on every page,
// so this import is already in the module map — see showSheet.
import { showModal, hideModal } from '/static/js/shingoedge.js';

const M = () => window.ComposerModel;
// Every request body this page sends is built in desktop-bodies.js, so a
// handler test can post exactly what the page posts. See that file's header.
const B = () => window.DesktopBodies;

// A PAGE THAT THROWS RENDERS THE SHELL AND PHOTOGRAPHS AS A PLAUSIBLE SHOT.
// composer-render.js has published its error like this since U8, and the
// desktop did not — so a broken Presets tab reported as "the page rendered
// without class=pd-modal — the shot is of some other screen", which names the
// symptom and hides the cause. The shots harness reads this attribute.
window.addEventListener('error', e => {
    try {
        document.body.dataset.pdError =
            String((e && (e.message || e.error)) || 'unknown').slice(0, 300);
    } catch (_) { /* nothing left to report with */ }
});
window.addEventListener('unhandledrejection', e => {
    try {
        document.body.dataset.pdError =
            String((e && e.reason && (e.reason.message || e.reason)) || 'unknown rejection').slice(0, 300);
    } catch (_) { /* nothing left to report with */ }
});
const gl = (mode, size, opts) => (typeof window.glyph === 'function' ? window.glyph(mode, size, opts || {}) : '');

const $ = id => document.getElementById(id);

// ── page state ───────────────────────────────────────────────────────────────
const S = {
    processes: [], groups: [], styles: [], stations: [],
    processID: 0,
    tab: 'flows',
    composer: null,      // the process-scoped composer read
    styleID: 0,          // the style being edited
    model: null,         // composer-model state (the draft)
    baseline: '',        // toCells JSON at load — what "dirty" is measured against
    previewTimer: null,
    previewAbort: null,
    selected: null,      // the selected position, shared by the picture and the table
    picRows: {},         // node -> 'front' | 'back' | '', from the DRAWING (never claim kind)
    openAs: '',          // the one dim cell currently drawn as a chip, because it was clicked
    adv: null,           // the Advanced sheet's own draft: {node, a, evacNodes, evacDest}
    routing: null,       // D3's rows — every one, the disabled backfills included
    routingError: '',    // the server's refusal, by name
    graph: null,         // the travel graph, built once per map
    settings: null,      // D5's draft; nothing is written until Save settings
    settingsError: '',
    sheet: null,         // the open confirm/edit sheet: {run}
    gen: null,           // the Generate-variants dialog: {baseID, cols, rows, error}
    mapZoom: 1,
    presets: null,       // D6's read: {view, error, expanded}
    papply: null,        // the apply modal: {id, rows:{styleID:{ticked,diff,cells,fingerprint,…}}, running}
    coreNodes: null,     // Core's node list, [{name, node_type}] — the node picker's source
    coreNodesReq: null,  // the in-flight read of it, so four pickers share one request
    pickers: {},         // every open node picker, by key — see pickerInit
    add: null,           // the Add-process sheet's own non-text draft: {groupID}
};

function root() { return $('pd-root'); }
function process() { return S.processes.find(p => p.id === S.processID) || null; }
function dirty() { return !!S.model && JSON.stringify(M().toCells(S.model)) !== S.baseline; }

// ── boot ─────────────────────────────────────────────────────────────────────
function boot() {
    const el = $('page-data');
    if (!el || !root()) return;
    const read = k => { try { return JSON.parse(el.dataset[k] || 'null') || []; } catch (_) { return []; } };
    const readObj = k => { try { return JSON.parse(el.dataset[k] || 'null') || {}; } catch (_) { return {}; } };
    S.processes = read('processes');
    S.groups = read('processGroups');
    S.styles = read('styles');
    S.stations = read('stations');
    // StationNodeMap and LoaderBoardGaps are already on the page for the
    // template's own use; D4 reads them there rather than asking again.
    S.stationNodes = readObj('stationNodes');
    S.loaderGaps = read('loaderGaps');
    S.processID = Number(el.dataset.activeProcessId || 0);

    // ?process=<id> opens that process; no id at all is the list.
    const q = new URLSearchParams(window.location.search);
    if (!q.get('process')) S.processID = 0;

    // THE FLOWS TAB IS APP-SHAPED (owner ruling Q6): the site nav stays, and
    // everything under it is one viewport-tall app — the rail and the main
    // column scroll inside it and the bottom bar pins to the column's foot.
    // The class goes on <body> because the shell it has to reshape (body >
    // nav + main.container + footer) is the header template's, and a rule
    // scoped to this page's own root cannot reach a parent.
    document.body.classList.add('pd-body');

    root().addEventListener('click', onClick);
    // The picture is drawn 1:1 into the column it has, so a resize is a
    // re-layout and not a re-scale. Only the picture is redrawn.
    window.addEventListener('resize', () => { if (S.model && S.tab === 'flows') drawPicture(); });
    const scrim = $('pd-scrim');
    if (scrim) scrim.addEventListener('click', onAdvClick);
    document.addEventListener('keydown', e => {
        if (e.key !== 'Escape') return;
        if (S.adv) closeAdvanced();
        else if (S.gen) closeGenerate();
        else if (S.sheet) closeSheet();
    });
    document.addEventListener('click', e => {
        if (!e.target.closest || !e.target.closest('.pd-pop')) closePop();
    });
    if (S.processID) openProcess(S.processID);
    else drawList();
}

// ── P0 · the list ────────────────────────────────────────────────────────────
// Built from what the page already carries. The columns are counts and names
// over data the handler read for other reasons, so the list costs no request.
function groupName(id) {
    const g = S.groups.find(g => g.id === id);
    return g ? g.name : '';
}
function stationsOf(pid) { return S.stations.filter(s => s.process_id === pid); }
function stylesOf(pid) { return S.styles.filter(s => s.process_id === pid); }
function runningStyle(p) {
    if (!p.active_style_id) return null;
    return S.styles.find(s => s.id === p.active_style_id) || null;
}

function drawList() {
    S.processID = 0;
    const groups = new Map();
    for (const p of S.processes) {
        const key = groupName(p.group_id) || 'Ungrouped';
        if (!groups.has(key)) groups.set(key, []);
        groups.get(key).push(p);
    }
    // Ungrouped last (SPEC P0).
    const names = [...groups.keys()].filter(n => n !== 'Ungrouped').sort();
    if (groups.has('Ungrouped')) names.push('Ungrouped');

    const row = p => {
        const st = stationsOf(p.id);
        const run = runningStyle(p);
        const counting = !!(p.counter_plc_name && p.counter_enabled);
        const state = (p.production_state === 'active_production') ? 'Running' : 'Idle';
        return '<tr data-open="' + p.id + '">' +
            '<td class="pn"><b>' + esc(p.name) + '</b><small>' + esc(st.length ? st[0].name : '—') + '</small></td>' +
            '<td>' + esc(groupName(p.group_id) || '—') + '</td>' +
            '<td>' + (run ? '<span class="pd-runname">' + esc(run.name) + '</span>' : '<span class="pd-dim">—</span>') + '</td>' +
            '<td class="num">' + stylesOf(p.id).length + '</td>' +
            '<td>' + (p.flow_composer_enabled ? '<span class="pd-tag ok">on</span>' : '<span class="pd-dim">off</span>') + '</td>' +
            '<td><span class="pd-cnt' + (counting ? ' ok' : '') + '"><i></i>' +
            esc(counting ? p.counter_plc_name + ' · counting' : 'not wired') + '</span></td>' +
            '<td class="num">' + st.length + '</td>' +
            '<td><span class="pd-state ' + state.toLowerCase() + '">' + state.toUpperCase() + '</span></td></tr>';
    };

    const head = '<thead><tr><th>Process</th><th>Group</th><th>Running</th><th class="num">Flows</th>' +
        '<th>HMI editing</th><th>Counter</th><th class="num">Screens</th><th>State</th></tr></thead>';
    const cols = '<colgroup><col style="width:20%"><col style="width:11%"><col style="width:24%"><col style="width:7%">' +
        '<col style="width:12%"><col style="width:14%"><col style="width:7%"><col style="width:9%"></colgroup>';
    // P0 is a table and gets the page, with the same 24 px gutter every other
    // block on this page has.


    let body = '';
    for (const n of names) {
        const rows = groups.get(n);
        body += '<div class="pd-lgrp"><span class="pd-lbl">' + esc(n) + '</span>' +
            '<span class="pd-dim">' + rows.length + ' process' + (rows.length === 1 ? '' : 'es') + '</span></div>' +
            '<table class="pd-tbl">' + cols + head + '<tbody>' + rows.map(row).join('') + '</tbody></table>';
    }

    root().innerHTML =
        '<div class="pd-app"><div class="pd-crumb"><b>Processes</b></div><div class="pd-spacer"></div>' +
        '<div class="pd-gate">' + searchBox('pd-q', 'find a process, station or part') +
        '<button class="pd-btn" data-act="add-group">Add group</button>' +
        '<button class="pd-btn primary" data-act="add-process">Add process</button></div></div>' +
        '<div class="pd-list" id="pd-list">' + body + '</div>';
    const q = $('pd-q');
    if (q) q.addEventListener('input', () => filterList(q.value));
}

function filterList(text) {
    const needle = String(text || '').trim().toLowerCase();
    for (const tr of root().querySelectorAll('.pd-tbl tbody tr')) {
        tr.hidden = !!needle && tr.textContent.toLowerCase().indexOf(needle) < 0;
    }
}

function searchBox(id, placeholder) {
    return '<span class="pd-search">' +
        '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true">' +
        '<circle cx="7" cy="7" r="4.5"/><path d="M10.5 10.5 14 14"/></svg>' +
        '<input id="' + id + '" type="text" autocomplete="off" placeholder="' + esc(placeholder) + '"></span>';
}

// ── the app bar ──────────────────────────────────────────────────────────────
// R3: the gate is a READ-ONLY status here and is set in Settings. A switch on
// the app bar would be the same decision in two places, which is the thing this
// page exists to stop.
function appbar() {
    const p = process();
    const on = !!(p && p.flow_composer_enabled);
    const tab = (id, label, extra) =>
        '<button class="' + (S.tab === id ? 'on' : '') + (extra || '') + '" data-tab="' + id + '">' + label + '</button>';
    return '<div class="pd-app">' +
        '<div class="pd-crumb"><button class="pd-dimlink" data-act="list">Processes</button> › <b>' +
        esc(p ? p.name : '') + (p && stationsOf(p.id)[0] ? ' · ' + esc(stationsOf(p.id)[0].name) : '') + '</b></div>' +
        // NO ROUTING TAB. The routing set is REVIEWED, and review is Settings'
        // job — it sits there beside the gate it is the precondition for
        // (owner ruling 2026-09-16). Creation is the Add-process sheet's.
        '<div class="pd-tabs">' + tab('flows', 'Flows') +
        tab('screens', 'Operator screens') + tab('settings', 'Settings') +
        tab('presets', 'Presets') + '</div>' +
        '<div class="pd-spacer"></div>' +
        '<div class="pd-gate"><span class="pd-pill' + (on ? ' on' : '') + '">' +
        (on ? 'Operators may change flows' : 'Operators run flows as set up') + '</span>' +
        '<button class="pd-dimlink" data-tab="settings">Settings ›</button></div></div>';
}

// ── D1 · Flows ───────────────────────────────────────────────────────────────
async function openProcess(id) {
    S.processID = id;
    S.tab = 'flows';
    // Everything derived from the last process goes with it: a travel graph
    // built from another plant's map, or another press's routing rows, would
    // draw one process's routes on another's picture.
    S.graph = null;
    S.routing = null;
    S.routingError = '';
    S.mapZoom = 1;
    // The routing pickers go with the routing set: they are write-through onto
    // THIS process's rows, and one left behind would post another press's name
    // into this one.
    S.pickers = {};
    root().innerHTML = appbar() + '<div class="pd-page"><div class="pd-dim" style="padding:24px">Loading…</div></div>';
    const res = await fetch('/api/processes/' + id + '/composer');
    if (!res.ok) {
        root().innerHTML = appbar() + '<div class="pd-list"><p class="pd-dim">Could not read this process.</p></div>';
        return;
    }
    S.composer = await res.json();
    const p = process();
    const h = hashOpts();
    const first = Number(h.style) || (p && p.active_style_id) ||
        (S.composer.styles[0] && S.composer.styles[0].id) || 0;
    selectStyle(first);
    if (h.adv) openAdvanced(h.adv);
    if (h.tab === 'screens') { S.tab = 'screens'; drawScreens(); }
    else if (h.tab === 'presets') {
        await openPresets();
        // #tab=presets;preset=<id> opens the tab with that row's member list
        // expanded, and `;apply=<id>` with its apply modal open on the first
        // part that would change. Same convention as `;adv=` above and for the
        // same reason: the shots harness cannot drive a mouse, and a state only
        // a mouse can reach is a state no shot checks.
        if (h.preset) {
            S.presets.expanded = Number(h.preset);
            drawPresets();
        }
        if (h.apply) {
            openPresetApply(Number(h.apply));
            if (h.tick) await tickApplyRow(Number(h.tick));
        }
    }
    else if (h.tab === 'settings') { S.tab = 'settings'; S.settings = settingsDraft(); await openSettings(); }
}

// #style=<id>;adv=<position>;tab=routing opens a particular flow, a particular
// position's Advanced sheet, or another tab, on load. It is how the shots harness photographs
// D2 without driving a mouse, the same convention U8's composer states use,
// and it deep-links a review comment to the exact field being argued about.
function hashOpts() {
    const out = {};
    for (const part of String(window.location.hash || '').replace(/^#/, '').split(';')) {
        const i = part.indexOf('=');
        if (i > 0) out[part.slice(0, i)] = decodeURIComponent(part.slice(i + 1));
    }
    return out;
}

function composerStyle(id) { return (S.composer.styles || []).find(s => s.id === id) || null; }

// ONE INIT, EVERY STYLE THIS PAGE OPENS. D1 edits one style and the Presets
// tab's apply modal previews many, and a second init would be a second answer
// to "what is this style's flow" — which is the thing the shared model exists
// to prevent.
function initModel(st) {
    const cell = S.composer.cell || { positions: [] };
    return M().init({
        styleId: st.id,
        styleName: st.name,
        positions: (cell.positions || []).map(p => ({
            core_node_name: p.core_node_name, kind: p.kind, sequence: p.sequence,
        })),
        routing: S.composer.routing || [],
        // The travel network for the key-route walk. The desktop's half of it
        // is Map, whose edges carry the same lengths Scene does.
        scene: S.composer.map || null,
        claims: st.claims || [],
        parts: (st.parts || []).map(p => p.payload_code || p),
        lastRun: st.last_run || null,
        flowspec: window.FLOWSPEC || null,
        groups: cell.groups || {},
        processClaims: (S.composer.styles || []).reduce((all, x) => all.concat(x.claims || []), []),
        // What the Advanced modal opens on. Read-only in the model: the draft's
        // own copy appears on a cell only once an engineer presses Apply.
        advanced: st.advanced || {},
    });
}

function selectStyle(id) {
    const st = composerStyle(id);
    if (!st) {
        // NO SUCH STYLE, so no draft — and the last process's draft has to go
        // with it. openProcess clears the graph and the routing set for the
        // same reason; leaving the model behind drew one press's positions
        // under another press's name, which only became reachable once a
        // process could be created with no parts at all.
        S.styleID = 0;
        S.model = null;
        S.baseline = '';
        S.selected = null;
        drawFlows();
        return;
    }
    S.styleID = id;
    S.selected = null;
    S.model = initModel(st);
    S.baseline = JSON.stringify(M().toCells(S.model));
    drawFlows();
    schedulePreview();
}

function drawFlows() {
    root().innerHTML = appbar() + '<div class="pd-page">' + rail() + main() + '</div>';
    drawPicture();
    drawBar();
    const q = $('pd-railq');
    if (q) q.addEventListener('input', () => {
        const n = q.value.trim().toLowerCase();
        for (const r of root().querySelectorAll('.pd-row')) {
            r.hidden = !!n && r.textContent.toLowerCase().indexOf(n) < 0;
        }
    });
}

// reportDesktopFit publishes D1's geometry for the shots harness, the way U8's
// reportPanelFit publishes the position panel's (composer-render.js).
//
// WHY MEASURED AND NOT ASSERTED IN CSS. Both of the things this round fixed
// were invisible to every test and plausible in a screenshot: a table whose
// last three columns are off the right edge still renders, and a bottom bar
// below the fold still exists in the DOM. Only the rendered geometry says
// which one you are looking at, and only a browser can produce it.
//
// Published on the body, read by composer_shots_test.go's checkDesktopFits.
function reportDesktopFit() {
    const tbl = root().querySelector('.pd-postbl table');
    const box = $('pd-postbl');
    const outer = $('pd-posbox');
    const bar = $('pd-bar');
    const pic = root().querySelector('.pd-pic');
    if (!tbl || !box || !bar || !pic) return;
    const b = bar.getBoundingClientRect(), pb = pic.getBoundingClientRect(), bx = box.getBoundingClientRect();
    const ox = outer ? outer.getBoundingClientRect() : bx;
    // P3: the add row is the footer and is always visible; the paired-above
    // line is the last SCROLLING row and has to be reachable rather than on
    // screen. Both are rendered geometry, so both are measured — a footer
    // that had slipped under the bar would still be in the DOM.
    const foot = $('pd-posfoot');
    const paired = root().querySelector('.pd-postbl tr.paired');
    const fit = {
        viewport: [Math.round(document.documentElement.clientWidth), Math.round(document.documentElement.clientHeight)],
        // T1: the table fits its box. Equal, not "close": one pixel of overflow
        // is a column an engineer cannot see.
        tableScrollW: Math.round(tbl.scrollWidth),
        tableClientW: Math.round(box.clientWidth),
        pageScrollW: Math.round(document.body.scrollWidth),
        // T2: the bar pins inside the viewport, and the table sits between the
        // picture and it.
        barBottom: Math.round(b.bottom),
        barTop: Math.round(b.top),
        picBottom: Math.round(pb.bottom),
        tableTop: Math.round(ox.top),
        tableBottom: Math.round(ox.bottom),
        // The scroller's own extent, which is what the footer has to sit under.
        scrollerBottom: Math.round(bx.bottom),
        // THE PICTURE IS DRAWN 1:1, and this is how that is checked now.
        //
        // It used to be checked by looking for ` 430"` in the DOM — the
        // viewBox height at the frame's resting size. That worked while the
        // height never moved, and P3 moved it: the box now has a floor and
        // the picture gives way to it (T2's rule), so 430 is one of several
        // right answers and the assertion was reading a constant where the
        // property is a RELATION. What matters is that the viewBox equals the
        // frame, whatever the frame is — a picture laid out at 1280x560 and
        // fitted by preserveAspectRatio is the bug, and it scales cards and
        // type together at 0.59.
        picW: Math.round(pic.clientWidth),
        picH: Math.round(pic.clientHeight),
        viewBox: (root().querySelector('#pd-svg') || { getAttribute: () => '' }).getAttribute('viewBox') || '',
        hasFooter: !!foot,
        hasPairedLine: !!paired,
    };
    if (foot) {
        const fb = foot.getBoundingClientRect();
        fit.footerTop = Math.round(fb.top);
        fit.footerBottom = Math.round(fb.bottom);
    }
    // HOW MANY POSITION ROWS ARE ACTUALLY ON SCREEN, measured rather than
    // divided by an assumed row height. `.pd-postbl td` says 32 px and a
    // rendered row is taller than that — the chips inside it are 26-28 and
    // carry their own padding — so a floor computed from 32 said two rows fit
    // while the second one was clipped through the middle of its chips.
    const head = root().querySelector('.pd-postbl thead');
    const firstRow = root().querySelector('.pd-postbl tbody tr[data-row]');
    if (head && firstRow) {
        const hh = head.getBoundingClientRect().height;
        const rh = firstRow.getBoundingClientRect().height;
        fit.headerH = Math.round(hh);
        fit.rowH = Math.round(rh);
        fit.rowsVisible = rh > 0 ? Math.floor((box.clientHeight - hh) / rh) : 0;
    }
    if (paired) {
        // Reachable by scroll: the line's offset within the scroller is inside
        // the scrollable range. Visible outright when the box is tall enough,
        // which is the same test with a scrollTop of zero.
        fit.pairedOffsetTop = Math.round(paired.offsetTop);
        fit.scrollerScrollH = Math.round(box.scrollHeight);
        fit.scrollerClientH = Math.round(box.clientHeight);
    }
    document.body.dataset.desktopFit = JSON.stringify(fit);
}

function styleRow(st, isRunning) {
    const on = st.id === S.styleID;
    const modes = [...new Set(st.claim_modes || [])].map(m => M().modeLabels()[m] || m).join(' + ');
    const nodes = (st.claim_nodes || []).map(n => n.replace('PLN_', 'P')).join('/');
    const sub = st.claim_count
        ? modes + (nodes ? ' · ' + nodes : '') + (st.last_run ? ' · ran ' + esc(st.last_run) : '')
        : 'no flow yet';
    return '<div class="pd-row' + (on ? ' on' : '') + '" data-style="' + st.id + '">' +
        '<div class="n">' + esc(st.name) +
        (isRunning ? '<span class="pd-tag run">running</span>' : '') +
        (st.claim_count ? '' : '<span class="pd-tag build">build</span>') +
        '<button class="more" data-act="style-menu" data-style="' + st.id + '" title="Rename · Expected CATID · Clone · Mark as running · Delete">⋯</button></div>' +
        '<div class="s">' + esc(sub) + '</div></div>';
}

function rail() {
    const p = process();
    const running = p ? p.active_style_id : 0;
    const all = S.composer.styles || [];
    const run = all.find(s => s.id === running);
    const rest = all.filter(s => s.id !== running);
    return '<div class="pd-rail">' + searchBox('pd-railq', 'find a part, CATID or flow') +
        '<div class="pd-rail-body">' +
        (run ? '<div class="pd-grp pd-lbl">Running</div>' + styleRow(run, true) : '') +
        '<div class="pd-grp pd-lbl">All parts · ' + all.length + ' live styles</div>' +
        rest.map(s => styleRow(s, false)).join('') +
        '</div><div class="pd-railfoot"><button class="pd-btn" data-act="new-style">+ New part flow</button>' +
        '<span class="pd-dim">or generate variants from Settings</span></div></div>';
}

// The model's own cell shape, in the shape renderFlowPicture draws — the same
// adapter the station uses, from the same place.
function cellFromModel() { return M().pictureCells(S.model, S.composer.cell || { positions: [] }); }

// PICTURE_H is the frame's height at rest, and the reference's: a 430 px band
// between the header and the positions table. It is a fallback here only —
// the real height is measured, because the frame shrinks on a short screen.
const PICTURE_H = 430;
const PICTURE_W_FALLBACK = 1084;   // the main column at the spec's 1440, for a frame not laid out yet

function drawPicture() {
    const svg = $('pd-svg');
    if (!svg || !S.model) return;
    // THE PICTURE IS DRAWN AT THE COLUMN'S OWN SIZE. The renderer lays out in
    // absolute pixels, so a card is CARD_W wide on screen only when the viewBox
    // is the element's real width. Handing it the station's 1280x560 and
    // letting preserveAspectRatio fit that into this frame is what drew the
    // whole picture at 0.59 with 9 px card titles.
    // The HEIGHT is measured too, because the frame gives way on a short screen
    // (.pd-pic is `flex: 0 1 430px`). Laying out at 430 into a box that ended
    // up 300 tall is the same mistake as laying out at 1280 into a 1084 column
    // — preserveAspectRatio would scale the whole drawing, cards and type
    // included, instead of the layout using the room it actually has.
    const w = Math.round(svg.clientWidth || (svg.parentNode && svg.parentNode.clientWidth) || PICTURE_W_FALLBACK);
    const h = Math.round(svg.clientHeight || (svg.parentNode && svg.parentNode.clientHeight) || PICTURE_H);
    const frame = { w: w, h: h };
    svg.setAttribute('viewBox', '0 0 ' + w + ' ' + h);
    const fs = M().findings(S.model);
    const byNode = {};
    for (const f of fs) if (f.node && !byNode[f.node]) byNode[f.node] = f.short;
    const p = process();
    const cell = cellFromModel();
    // The table's front/back sub-label is this, and only this — see pictureRows.
    const before = JSON.stringify(S.picRows);
    S.picRows = pictureRows(cell, frame);
    const rowsMoved = JSON.stringify(S.picRows) !== before;
    svg.innerHTML = renderFlowPicture(
        { cell: cell, station: { name: p ? p.name : '' } },
        {
            selected: S.selected, findings: byNode, editable: true, frame: frame,
            sentences: sentencesFromModel(S.model),
        });
    if (rowsMoved) redrawPositionsTable();
    reportDesktopFit();
}

// A PROCESS WITH NO PART IS THE STATE A CREATE LANDS IN, and until U10 there
// was no way to reach it, so nothing drew it: selectStyle(0) fell through to
// drawFlows and positionsTable read S.model.positions off a null model. The
// page threw on the first screen an engineer saw after making a press.
//
// The empty state says the one thing that is true — a press runs parts, and
// this one has none yet — and points at the button that adds one, which is
// already in the rail beside it.
function noStyleYet() {
    return '<div class="pd-main"><div class="pd-head"><div class="t"><h1>No part yet</h1>' +
        '<div class="sub">this press runs nothing until one of its parts has a flow</div></div></div>' +
        '<div class="pd-empty"><p>A flow is drawn for a part: which positions the press works, ' +
        'how each one swaps, and where its bins come from and go. Add the first part from ' +
        '<b>+ New part flow</b> in the rail, or stamp out a family of them from ' +
        'Settings › Generate variants.</p>' +
        '<p class="pd-dim">Positions come from the operator screen that claims them — ' +
        'Operator screens › Edit.</p></div></div>';
}

function main() {
    if (!S.model) return noStyleYet();
    const st = composerStyle(S.styleID) || { name: '', claim_count: 0 };
    const p = process();
    const isRunning = !!(p && p.active_style_id === S.styleID);
    const modes = [...new Set(st.claim_modes || [])].map(m => M().modeLabels()[m] || m).join(' + ');
    const sub = [
        modes || 'no flow yet',
        (st.claim_count || 0) + ' position' + (st.claim_count === 1 ? '' : 's'),
        st.last_run ? 'last run ' + st.last_run : 'never run here',
    ].join(' · ');
    return '<div class="pd-main">' +
        // THE ACTIONS SIT ON THE TITLE ROW, right-aligned beside the sub line,
        // the way the reference draws them. They were on a row of their own
        // because the head was one wrapping flex line and the title, the sub,
        // the running note and three buttons do not fit on one at 1440 — so
        // the title and its sub line are their own block now, and the actions
        // are the second item on a row that does not wrap.
        '<div class="pd-head"><div class="t"><h1>' + esc(st.name) + '</h1>' +
        '<div class="sub">' + esc(sub) +
        // RUNNING IS A STATE, NOT A SENTENCE (owner ruling F4, 2026-09-12). The
        // sub line carried `· running — a saved change applies at the next
        // changeover` and the header is not wide enough for it: on the running
        // part it ellipsised to `…applies at the next cha…`, which is a
        // sentence that stops before the half that matters. The BAR says it
        // whole and always has. So the header carries the rail's own green
        // `running` tag, which is the thing an engineer is scanning for, and
        // the sentence lives in one place.
        (isRunning ? '<span class="pd-tag run">running</span>' : '') +
        '</div></div>' +
        '<div class="act">' +
        '<button class="pd-btn quiet" data-act="discard">Discard changes</button>' +
        '<button class="pd-btn" data-act="copy-to">Copy to another part…</button>' +
        // SAVE AS PRESET IS MADE FROM THE SAVED FLOW, so it is disabled while
        // the draft is dirty: a preset named from an unsaved draft would be a
        // shape the station does not run, under a name that claims it does.
        // drawBar() enables it on the same measurement that disables Save flow.
        '<button class="pd-btn" data-act="save-preset" disabled>Save as preset…</button>' +
        '<button class="pd-btn primary" data-act="save" disabled>Save flow</button></div></div>' +
        '<div class="pd-pic"><svg id="pd-svg" class="os-flow-picture" viewBox="0 0 1280 560"></svg>' +
        '<div class="pd-legend"><span><i class="r1"></i>Robot 1</span><span><i class="r2"></i>Robot 2</span></div></div>' +
        positionsTable() +
        '<div class="pd-bar" id="pd-bar"></div><div class="pd-pop" id="pd-pop" hidden></div></div>';
}

function positionsTable() {
    const rows = [];
    const used = new Set();
    for (const pos of S.model.positions) {
        const n = pos.core_node_name;
        const c = S.model.cells[n];
        if (!c || !c.on || !c.mode) continue;
        used.add(n);
        for (const col of ['partner', 'staging']) {
            for (const chip of M().rowColumns(S.model, n, col)) if (chip.value) used.add(chip.value);
        }
        const advSet = advancedCount(n);
        rows.push('<tr class="' + (S.selected === n ? 'selrow' : '') + '" data-row="' + n + '">' +
            '<td class="pos">' + esc(n) + '<small>' + esc(rowWord(n)) + '</small></td>' +
            '<td>' + picker(n, 'mode', gl(c.mode, 22, {}) + esc(M().modeLabels()[c.mode] || '')) + '</td>' +
            '<td>' + picker(n, 'part', esc(M().shortPart(c.part) || 'pick one'), c.part ? 'part' : 'bad') + '</td>' +
            '<td>' + columnCell(n, 'partner') + '</td>' +
            '<td>' + columnCell(n, 'staging') + '</td>' +
            '<td>' + fieldCell(n, 'source', 'inbound_source', c.source, '—') + '</td>' +
            '<td>' + viaCell(n) + '</td>' +
            '<td>' + fieldCell(n, 'dest', 'outbound_destination', c.dest, '—') + '</td>' +
            '<td><button class="pd-adv' + (advSet ? ' set' : '') + '" data-act="advanced" data-node="' + esc(n) + '">' +
            gearSVG() + (advSet ? '<b>' + advSet + ' set</b>' : 'defaults') + '</button></td></tr>');
    }
    const paired = S.model.positions.map(p => p.core_node_name)
        .filter(n => used.has(n) && !(S.model.cells[n] && S.model.cells[n].on && S.model.cells[n].mode));
    const free = S.model.positions.map(p => p.core_node_name).filter(n => !used.has(n));
    let tail = '';
    if (paired.length) {
        tail += '<tr class="paired"><td colspan="9">' + esc(paired.join(', ')) +
            ' — back positions, no row of their own, paired above</td></tr>';
    }
    // P3 · THE ADD ROW IS THE BOX'S FOOTER, NOT ITS LAST ROW. In the accepted
    // D1 shot neither it nor the paired-above line was on screen: both were
    // inside a tbody that scrolls, and with the picture at its resting height
    // the box showed exactly the two position rows. An engineer looking at that
    // screen has no way to learn that PLN_03 and PLN_06 are free — the one
    // control that adds a position was below the fold of a box with no visible
    // scrollbar.
    //
    // Ruling: the rows scroll, the add row does not. It leaves the table and
    // becomes a sibling of the scroller, so it is always visible however many
    // positions the press has. The paired-above line STAYS a scrolling row: it
    // is a statement about the rows, it belongs at the end of them, and it is
    // reachable by scroll rather than always on screen.
    const footer = free.length
        ? '<div class="pd-posfoot" data-act="add-position" id="pd-posfoot">+ Add a position' +
        '<span>' + esc(free.join(' · ')) + (free.length === 1 ? ' is free' : ' are free') + '</span></div>'
        : '';
    const cols = '<colgroup><col class="c-pos"><col class="c-mode"><col class="c-part"><col class="c-partner">' +
        '<col class="c-staging"><col class="c-src"><col class="c-via"><col class="c-dst"><col class="c-adv"></colgroup>';
    // THE LABEL AND THE FOOTER ARE OUTSIDE THE SCROLLER. `#pd-postbl` is still
    // the scrolling element and still the box T1 measures the table against —
    // its clientWidth is what a column has to fit inside — and the box around
    // all three is what the picture now gives way to (T2's rule).
    return '<div class="pd-posbox" id="pd-posbox">' +
        '<div class="pd-lbl">Positions' +
        '<span class="hint">click a card or a cell to change it · the picture and the rows are one thing</span></div>' +
        '<div class="pd-postbl" id="pd-postbl">' +
        // F3: A FIELD IS CALLED WHAT THE CLAIM CALLS IT. These read `New bins
        // from`, `Old bins to`, `Robot drives via` and `Partner` — four names
        // shingo already had, invented a second time for one table. An
        // engineer reading a refusal that says `outbound_destination` and a
        // column that says `old bins to` has to work out they are the same
        // field, and the compare table then had to pick one of the two.
        // EVERY HEADING IS THE CLAIM'S OWN NAME, from the one label table
        // (owner ruling F3). `Swaps`, `Paired`, `Inbound source`, `Key route`
        // and `Outbound destination` were five more spellings of five claim
        // columns — close enough to look deliberate and different enough that
        // a refusal naming `outbound_destination` read as being about
        // something else.
        //
        // `Position` and `Advanced` are not claim fields: one is the press's
        // word for a node and the other is the name of a sheet. `Part` stays
        // the floor's word for payload_code (owner-accepted). `Staging` is a
        // group of two, and its chips carry the two names.
        '<table>' + cols + '<thead><tr><th>Position</th><th>' + esc(W('swap_mode')) + '</th><th>Part</th>' +
        '<th>' + esc(W('paired_core_node')) + '</th><th>Staging</th>' +
        '<th>' + esc(W('inbound_source')) + '</th><th>' + esc(W('key_route')) + '</th>' +
        '<th>' + esc(W('outbound_destination')) + '</th><th>Advanced</th></tr></thead>' +
        '<tbody>' + rows.join('') + tail + '</tbody></table></div>' +
        footer + '</div>';
}

// ONE WORD, ONE MEANING. The sub-label under a position is which ROW of the
// picture it was drawn in, and nothing else. It used to be CellPosition.Kind,
// which answers a different question — is this a partner slot for any style
// this process runs — and at Hopkinsville the two disagree: PLN_01 and PLN_04
// are Kind "front" and are drawn in the BACK row, so the table said front
// under a picture that said BACK. What a position does for the running style
// is said by the paired-back line at the foot of the table.
//
// A middle row, and a picture with only one row, get no word: the renderer
// declines to label those and a guess printed in a table is still a guess.
function rowWord(node) {
    return S.picRows[node] || '';
}

// A CHIP IS A CONTROL; AN EMPTY OPTIONAL FIELD IS TEXT.
//
// Nine columns of chips do not fit the main column at 1440, and a primary
// table that scrolls sideways is not a table that fits: the last three columns
// were off the right edge of the shot. The reference fits its eight because it
// spends a chip only where one is needed.
//
// So: a chip where flowspec says the field is REQUIRED for this mode, or where
// a value is set. Everything else is a dim placeholder — still the click
// target, and clicking it draws the chip and opens its picker, so nothing
// becomes unreachable. The chip is what an engineer has to look at; the dash
// is what they can skip.
//
// openAs is which cell is currently showing as a chip because it was clicked
// (node + ':' + kind). It clears when the popover closes.
function isOpen(node, kind) { return S.openAs === node + ':' + kind; }

function markOpen(node, kind) {
    S.openAs = node + ':' + kind;
}

// blank is the dim placeholder: a bare button, no chip chrome, that turns into
// the chip it stands for.
function blank(node, kind, text, title) {
    return '<button class="pd-blank" data-act="pick" data-node="' + esc(node) + '" data-kind="' + kind + '"' +
        (title ? ' title="' + esc(title) + '"' : '') + '>' + text + '</button>';
}

// fieldCell is one single-field column: source, destination.
function fieldCell(node, kind, field, value, empty) {
    if (value || M().fieldRequired(S.model, node, field) || isOpen(node, kind)) {
        return picker(node, kind, esc(value || '—'), '', value);
    }
    return blank(node, kind, '<span class="pd-dim">' + esc(empty) + '</span>', 'not set');
}

// A mode-dependent cell — Partner, Staging — is ONE cell holding the fields
// flowspec says the mode has.
//
// If any of them is required or set, every one of them is drawn with its word
// in front of it, the unset ones as `Park old at —`: inside an active cell the
// engineer is reading a pair of places and a missing half is information. If
// none is required or set, the whole cell is a single dim dash.
//
// A field with no candidate is not drawn at all, even as a dash — see the
// model's hasCandidate. `Third` on Press 400 is that case: flowspec says a
// press index has the field, and this press has no third position to put in
// it.
// The two mode-dependent columns' headings, so a cell can tell when its chip
// word would repeat one. Kept beside the <thead> that draws them.
const COLUMN_HEADING = () => ({ partner: W('paired_core_node'), staging: 'Staging' });

// W is the word for a claim field, from the generated flowspec block — the one
// table domain/flowspec.Label reads. Every heading and every chip word on this
// page goes through it, so the column an engineer reads and the field a
// refusal names are spelled the same.
function W(field) { return M().fieldLabel(S.model, field); }

function columnCell(node, column) {
    const chips = M().rowColumns(S.model, node, column).filter(c => c.possible);
    if (!chips.length) return '<span class="pd-dim">—</span>';
    const live = chips.some(c => c.required || c.value) ||
        chips.some(c => isOpen(node, 'col:' + column + ':' + c.key));
    if (!live) {
        return blank(node, 'col:' + column + ':' + chips[0].key, '<span class="pd-dim">—</span>',
            chips.map(c => c.label).join(' · '));
    }
    // A CELL DOES NOT REPEAT ITS COLUMN HEADING. The chip word is there to tell
    // two chips in one cell apart — `Inbound PLN_02 · Outbound —` under STAGING
    // — and where it IS the heading it says the same thing twice: F3 renamed
    // the Partner column to PAIRED, which is what its one chip was already
    // called. The same rule D6's candidate row follows under USED BY. The
    // chip's TITLE keeps the word either way, for the case where the column is
    // narrower than the value.
    const heading = COLUMN_HEADING()[column] || '';
    return chips.map(chip => {
        const word = chip.label.toLowerCase() === heading.toLowerCase() ? '' : chip.label;
        const k = word ? '<span class="k">' + esc(word) + '</span> ' : '';
        const kind = 'col:' + column + ':' + chip.key;
        if (chip.required || chip.value || isOpen(node, kind)) {
            return picker(node, kind, k + esc(chip.value || '—'),
                chip.required && !chip.value ? 'bad' : '', chip.label + ' ' + (chip.value || 'not set'));
        }
        return blank(node, kind, k + '<span class="pd-dim">—</span>', chip.label + ' not set');
    }).join('<i class="pd-cellsep"></i>');
}

// KEY ROUTE IS AN ORDERED LIST, so the cell is an ordered list of chips: each
// one opens the picker that replaces or removes it, the arrows move it, and
// "+ add" appends. A route with no points reads "shortest way", which is what
// an empty key_route means to SEER.
function viaCell(node) {
    const route = (S.model.cells[node].keyRoute || []);
    if (!route.length) {
        // key_route is Used everywhere it is allowed, never Required, and an
        // empty one is not a gap: it is what makes SEER pick the way.
        if (!isOpen(node, 'via:0')) {
            return blank(node, 'via:0', '<span class="pd-dim">shortest way</span>', 'no waypoints');
        }
        return picker(node, 'via:0', '<span class="pd-dim">shortest way</span>');
    }
    const last = route.length - 1;
    return route.map((w, i) =>
        '<span class="pd-via">' +
        (i > 0 ? '<button class="pd-mv" data-act="via-move" data-node="' + esc(node) + '" data-i="' + i +
            '" data-d="-1" title="earlier in the route">‹</button>' : '') +
        picker(node, 'via:' + i, esc(w)) +
        (i < last ? '<button class="pd-mv" data-act="via-move" data-node="' + esc(node) + '" data-i="' + i +
            '" data-d="1" title="later in the route">›</button>' : '') +
        '</span>').join('<i class="pd-viasep">›</i>') +
        '<button class="pd-chip add" data-act="via-add" data-node="' + esc(node) + '">+ add</button>';
}

// The picture's row assignment can change with the frame, so the table is
// redrawn when it does — and only then, because a redraw takes an open
// popover with it.
//
// IT REPLACES THE WHOLE BOX, which is what positionsTable() returns. P3 split
// that box into a label, a scroller and a footer, and this went on replacing
// the SCROLLER (`#pd-postbl`) with the whole box — nesting a second box inside
// the first, so D1 drew two POSITIONS labels and two add-a-position footers
// while the rows were crushed to nothing between them. The id this reaches for
// and the element that function returns are the same element again.
function redrawPositionsTable() {
    const el = $('pd-posbox');
    if (el) el.outerHTML = positionsTable();
}

// The row's `N set` badge. The model counts it, by domain.ClaimHas's rule, so
// the badge and the server agree on what "set" means; the two evacuation
// fields are the cell's own and are counted here beside them because the modal
// draws all fourteen in one sheet.
function advancedCount(node) {
    const c = S.model.cells[node];
    let n = M().advancedSet(S.model, node).length;
    if ((c.evacNodes || []).length) n++;
    if (c.evacDest) n++;
    return n;
}

function gearSVG() {
    return '<svg width="12" height="12" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.4" aria-hidden="true">' +
        '<circle cx="6" cy="6" r="2"/><path d="M6 1v1.5M6 9.5V11M1 6h1.5M9.5 6H11M2.5 2.5l1 1M8.5 8.5l1 1M2.5 9.5l1-1M8.5 3.5l1-1"/></svg>';
}

// title is the chip's full text, for the case where the column is narrower
// than the label: the chip ellipsises rather than wrapping, and the whole name
// stays available on hover.
function picker(node, kind, inner, cls, title) {
    // The label is its own box so `text-overflow: ellipsis` has something to
    // apply to: on a flex container it does nothing, and a chip too wide for
    // its column was clipping mid-word with no sign that it had been cut.
    return '<button class="pd-sel' + (cls ? ' ' + cls : '') + '" data-act="pick" data-node="' + esc(node) +
        '" data-kind="' + kind + '"' + (title ? ' title="' + esc(title) + '"' : '') + '>' +
        '<span class="t">' + inner + '</span><i class="car"></i></button>';
}

function drawBar() {
    if (!S.model) return;
    const b = M().bar(S.model);
    const bar = $('pd-bar');
    if (!bar) return;
    bar.innerHTML = '<div><div class="h' + (b.tone === 'blocked' ? ' bad' : '') + '">' + esc(b.heading) + '</div>' +
        '<div class="d">' + esc(b.detail) + '</div></div>' +
        '<div class="prov' + (dirty() ? ' dirty' : '') + '">' + (dirty() ? 'Unsaved changes' : 'No unsaved changes') + '</div>';
    const save = root().querySelector('[data-act="save"]');
    if (save) save.disabled = !dirty() || b.tone === 'blocked';
    // `Save as preset…` is the mirror image: enabled only on `No unsaved
    // changes`, because a preset is made FROM the saved flow and so can never
    // disagree with what the station runs. The two buttons are therefore never
    // enabled at the same time, which is the point rather than an accident.
    const asPreset = root().querySelector('[data-act="save-preset"]');
    if (asPreset) asPreset.disabled = dirty() || !(S.model && M().toCells(S.model).length);
}

// ── preview ──────────────────────────────────────────────────────────────────
function schedulePreview() {
    if (S.previewTimer) clearTimeout(S.previewTimer);
    S.previewTimer = setTimeout(runPreview, 400);
}

async function runPreview() {
    if (!S.model) return;
    if (S.previewAbort) S.previewAbort.abort();
    S.previewAbort = new AbortController();
    try {
        const res = await fetch('/api/processes/' + S.processID + '/flow/preview', {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
                to_style_id: S.model.styleId, cells: M().toCells(S.model),
            }),
            signal: S.previewAbort.signal,
        });
        const json = await res.json().catch(() => ({}));
        S.model = M().applyPreview(S.model, json);
    } catch (e) {
        if (e && e.name === 'AbortError') return;
    }
    drawBar();
    drawPicture();
}

// ── save ─────────────────────────────────────────────────────────────────────
// SPEC §3: flow/save on the last preview's fingerprint; 409 stale re-previews
// and says so; 422 puts the server's findings on the rows.
async function saveFlow() {
    const pv = S.model.preview || {};
    const res = await fetch('/api/processes/' + S.processID + '/flow/save', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
            to_style_id: S.model.styleId, cells: M().toCells(S.model),
            fingerprint: pv.fingerprint || '',
            station_id: stationIDForSave(),
        }),
    });
    const json = await res.json().catch(() => ({}));
    // The decision is B().saveOutcome — every branch in one place, driven under
    // node by apply-loop.test.js. This function does the DOM.
    const out = B().saveOutcome(res.status, json);
    if (out.kind === 'stale' || out.kind === 'refused') {
        if (res.status === 409) S.model = M().applyPreview(S.model, json);
        const bar = $('pd-bar');
        if (bar) bar.innerHTML = '<div><div class="h bad">' + esc(out.text) + '</div></div>';
        if (out.kind === 'stale') schedulePreview();
        return;
    }
    if (out.kind === 'findings') { S.model = M().applyPreview(S.model, json); drawFlows(); return; }
    // Saved: the draft becomes the new baseline and the read is refreshed so the
    // rail's counts and last-run follow.
    S.baseline = JSON.stringify(M().toCells(S.model));
    await openProcess(S.processID);
}

// flow/save wants the station that saved it, and on the desktop there is no
// station at the keyboard. The process's first screen stands for the press —
// the same press the engineer is editing — and `called_by` then reads as that
// screen's name rather than as nothing.
function stationIDForSave() {
    const st = stationsOf(S.processID);
    return st.length ? st[0].id : 0;
}

// ── pickers ──────────────────────────────────────────────────────────────────
function optionsFor(node, kind) {
    const c = S.model.cells[node];
    const routing = S.composer.routing || [];
    const backs = S.model.positions.filter(p => p.kind === 'back').map(p => p.core_node_name);
    // A kind may carry arguments: "col:staging:parkOld", "via:1".
    const parts = String(kind).split(':');
    switch (parts[0]) {
        case 'mode': {
            const labels = M().modeLabels();
            return Object.keys(labels).map(m => ({
                value: m, label: labels[m], icon: gl(m, 22, { rest: c.mode !== m }), on: c.mode === m,
                action: { type: 'setMode', node: node, mode: m },
            }));
        }
        case 'part':
            return S.model.parts.map(p => ({
                value: p, label: M().shortPart(p), on: c.part === p,
                action: { type: 'setPart', node: node, payloadCode: p },
            }));
        // col:<column>:<cellKey> — one of the mode-dependent chips. The FIELD
        // comes from the model's own rowColumns, which reads flowspec, so a
        // picker can only ever write a column this mode is allowed to have.
        // A chip that is not drawn has no picker: the table renders a dash,
        // and a dash is not a control.
        case 'col': {
            const column = parts[1], key = parts[2];
            const chip = M().rowColumns(S.model, node, column).find(x => x.key === key);
            if (!chip) return [];
            const others = S.model.positions.map(p => p.core_node_name).filter(n => n !== node);
            // A paired or parked position is a position of this press. The
            // back positions are offered first because they are what a press
            // pairs and parks on; the rest follow, because a sequential A/B
            // partner is a front position.
            const list = backs.concat(others.filter(n => backs.indexOf(n) < 0));
            const act = { paired: 'setPartner', secondPaired: 'setSecondPartner', staging: 'setStaging', parkOld: 'setParkOld' }[key];
            const arg = { paired: 'partner', secondPaired: 'partner', staging: 'staging', parkOld: 'staging' }[key];
            const clear = chip.required ? [] : [{
                value: '', label: 'none', on: !chip.value,
                action: Object.assign({ type: act, node: node }, { [arg]: '' }),
            }];
            return clear.concat(list.map(n => ({
                value: n, label: n, on: chip.value === n,
                action: Object.assign({ type: act, node: node }, { [arg]: n }),
            })));
        }
        case 'source':
            return routing.filter(r => r.role === 'source').map(r => ({
                value: r.core_node_name, label: r.label || r.core_node_name, on: c.source === r.core_node_name,
                action: { type: 'setSource', node: node, source: r.core_node_name },
            }));
        case 'dest':
            return routing.filter(r => r.role === 'destination').map(r => ({
                value: r.core_node_name, label: r.label || r.core_node_name, on: c.dest === r.core_node_name,
                action: { type: 'setDest', node: node, dest: r.core_node_name },
            }));
        // via:<i> — the picker behind one point of the route. It replaces
        // that point, or removes it; "+ add" appends through via-add. The
        // whole list goes to setVia either way, because the order IS the
        // route.
        case 'via': {
            const at = Number(parts[1] || 0);
            const route = (c.keyRoute || []).slice();
            // THE SAME WALK THE HMI MAKES. This filtered the routing set for
            // `role === 'waypoint'` — a role the schema forbids on purpose,
            // because a key route is validated against the vendor MAP and not
            // against that list — so it offered nothing and the desktop's
            // whole key-route authoring was inert.
            const offered = M().viaWaypoints(S.model, node);
            const replace = w => { const next = route.slice(); next[at] = w; return { type: 'setVia', node: node, via: next }; };
            const head = route.length
                ? [{
                    value: '', label: 'remove this point', on: false,
                    action: { type: 'setVia', node: node, via: route.filter((_, i) => i !== at) },
                }]
                : [{ value: '', label: 'shortest way', on: true, action: { type: 'setVia', node: node, via: [] } }];
            return head.concat(offered.map(w => ({
                value: w, label: w, on: route[at] === w,
                action: route.length ? replace(w) : { type: 'setVia', node: node, via: [w] },
            })));
        }
        default: return [];
    }
}

// A popover list, never a native <select> — the style guide's "never use native
// dialogs" is about the browser drawing its own chrome over ours, and a select's
// dropdown is exactly that.
function openPicker(btn) {
    const node = btn.dataset.node, kind = btn.dataset.kind;
    // A dim placeholder becomes the chip it stands for, and the picker opens
    // on it. The redraw has to come first or the popover anchors to a button
    // that is about to be replaced.
    if (btn.classList.contains('pd-blank')) {
        markOpen(node, kind);
        redrawPositionsTable();
        const chip = root().querySelector('.pd-postbl [data-act="pick"][data-node="' + CSS.escape(node) +
            '"][data-kind="' + CSS.escape(kind) + '"]');
        if (chip) { openPicker(chip); return; }
    }
    const opts = optionsFor(node, kind);
    const pop = $('pd-pop');
    if (!pop) return;
    pop.innerHTML = opts.length
        ? opts.map((o, i) => '<button data-opt="' + i + '" class="' + (o.on ? 'on' : '') + '">' +
            (o.icon || '') + esc(o.label) + '</button>').join('')
        : '<div class="none">Nothing to choose here yet.</div>';
    pop.hidden = false;
    const r = btn.getBoundingClientRect();
    const host = pop.offsetParent ? pop.offsetParent.getBoundingClientRect() : { left: 0, top: 0, width: window.innerWidth };
    pop.style.left = Math.min(host.width - pop.offsetWidth - 8, Math.max(8, r.left - host.left)) + 'px';
    pop.style.top = (r.bottom - host.top + 6) + 'px';
    pop.querySelectorAll('[data-opt]').forEach(b => b.addEventListener('click', () => {
        apply(opts[Number(b.dataset.opt)].action);
        closePop();
    }));
}

function closePop() {
    const pop = $('pd-pop');
    if (pop) { pop.hidden = true; pop.innerHTML = ''; }
    // The placeholder that was clicked goes back to being a dash, unless the
    // pick gave it a value — `apply` redraws the whole tab and clears this
    // either way.
    if (S.openAs) { S.openAs = ''; redrawPositionsTable(); }
}

// Every picker applies instantly to the DRAFT; nothing is written until Save
// flow (SPEC §3).
function apply(action) {
    S.model = M().reduce(S.model, action);
    drawFlows();
    schedulePreview();
}

// ── D3 · Routing set on the map (SPEC §2 D3) ─────────────────────────────────
//
// The routing set was a list of names. It is the set of places this press may
// draw bins from, park them at and send them to, and every one of those is
// somewhere — so the screen is the plant, with the names beside it.
//
// VIEW AND ADOPT, NEVER EDIT (SPEC §3). Nothing here writes geometry; the map
// is Core's, cached by U4, and the only writes are the routing set's own:
// enable, adopt, add. A backfill row nobody ruled on shows amber with Adopt,
// which is the whole reason origin is stored.
//
// The projection is shared/scene-geom.js — Core's map, the station's cell
// picture and this screen project the same plant with the same function, so a
// route weighed on one surface is the route drawn on the others.

const AISLE_FIT_PAD = 3;        // metres of plant left around the fitted region
const LABEL_PAD = 56;           // screen px kept clear for the marks' labels

function mapOf() { return (S.composer && S.composer.map) || null; }

// The travel graph: adjacency over the cache's edges, weighted by the DRIVEN
// length — the cubic's arc length on a bowed lane, not its chord, because a
// chord is up to a quarter short and makes the shortest path prefer a route
// the robot does not find shorter (shared/scene-geom.js).
function travelGraph() {
    if (S.graph) return S.graph;
    const m = mapOf();
    const adj = {};
    if (!m) { S.graph = adj; return adj; }
    // An edge names its endpoints; the map places them. The server guarantees
    // every endpoint is in points (composerMap), so this cannot miss — and it
    // is why the edge no longer ships a second copy of the coordinates.
    for (const e of m.edges || []) {
        const p0 = m.points[e.from], p3 = m.points[e.to];
        if (!p0 || !p3) continue;
        const len = e.h ? cubicLength(p0, e.h, p3) : Math.sqrt(dist2(p0, p3));
        (adj[e.from] = adj[e.from] || []).push({ to: e.to, len: len, e: e });
    }
    S.graph = adj;
    return adj;
}

// A node name is placed at its GeneralLocation — the join rule the server's
// LocateCellPosition states. The TRAVEL graph, though, runs between the
// robot's approach points, and no row joins a bin location to the point a
// robot stands at to serve it. So the graph entry for a name is the nearest
// graph node to its placement: measured rather than guessed from a spelling,
// and it is the same answer the fleet reaches by driving there.
function graphNodeFor(name) {
    const m = mapOf();
    if (!m || !name) return '';
    const adj = travelGraph();
    if (adj[name]) return name;
    const p = m.points[name];
    if (!p) return '';
    let best = '', bestD = Infinity;
    for (const g of Object.keys(adj)) {
        const q = m.points[g];
        if (!q) continue;
        const d = dist2(p, q);
        if (d < bestD) { bestD = d; best = g; }
    }
    return best;
}

// Dijkstra. Small graphs — Hopkinsville is 383 points — walked twice a draw.
function route(a, b) {
    if (!a || !b || a === b) return null;
    const adj = travelGraph();
    const dist = {}, prev = {}, seen = {};
    dist[a] = 0;
    const pq = [[0, a]];
    while (pq.length) {
        pq.sort((x, y) => x[0] - y[0]);
        const head = pq.shift();
        const d = head[0], u = head[1];
        if (u === b) break;
        if (seen[u]) continue;
        seen[u] = true;
        for (const edge of adj[u] || []) {
            const nd = d + edge.len;
            if (nd < (dist[edge.to] === undefined ? Infinity : dist[edge.to])) {
                dist[edge.to] = nd;
                prev[edge.to] = edge;
                pq.push([nd, edge.to]);
            }
        }
    }
    if (dist[b] === undefined) return null;
    const path = [];
    let u = b;
    while (u !== a) {
        const edge = prev[u];
        if (!edge) return null;
        path.unshift(edge.e);
        u = edge.e.from;
    }
    return path;
}

// NGRP membership, from the PICTURE. It used to come off ComposerScene, which
// this read no longer carries: Scene and Map are the same network and a payload
// must not carry both, so the desktop takes Map. The membership map was never
// scene geometry anyway — CellPicture.Groups is the one copy, and both
// surfaces already have the picture.
function membersOfGroup(name) {
    const g = (S.composer.cell && S.composer.cell.groups) || {};
    return g[name] || [];
}

// The region the view is fitted to: the press's positions and the members of
// every group in the routing set. The rest of the plant is drawn behind it and
// runs off the edges, which is what "the rest of the plant faint" means.
function fitRegion(m) {
    const names = [];
    for (const p of (S.composer.cell && S.composer.cell.positions) || []) names.push(p.core_node_name);
    for (const r of S.routing || []) {
        names.push(r.core_node_name);
        for (const mem of membersOfGroup(r.core_node_name)) names.push(mem);
    }
    const pts = names.map(n => m.points[n]).filter(Boolean);
    if (!pts.length) return null;
    return {
        x0: Math.min.apply(null, pts.map(p => p.x)) - AISLE_FIT_PAD,
        x1: Math.max.apply(null, pts.map(p => p.x)) + AISLE_FIT_PAD,
        y0: Math.min.apply(null, pts.map(p => p.y)) - AISLE_FIT_PAD,
        y1: Math.max.apply(null, pts.map(p => p.y)) + AISLE_FIT_PAD,
    };
}

// The WHOLE plant's bounds decide the orientation, never the fitted region: a
// region that happens to be tall must not flip the map under the engineer
// (shared/scene-geom.js, rotate90For).
function plantProjector(m) {
    const xs = [], ys = [];
    for (const k of Object.keys(m.points)) { xs.push(m.points[k].x); ys.push(m.points[k].y); }
    return makeProjector(rotate90For(
        Math.min.apply(null, xs), Math.max.apply(null, xs),
        Math.min.apply(null, ys), Math.max.apply(null, ys)));
}

function drawMap(w, h) {
    const m = mapOf();
    if (!m) {
        return '<div class="pd-nomap">The plant map has not reached this edge yet. Core sends it ' +
            'with the node list; the routing set is still a list beside this.</div>';
    }
    const project = plantProjector(m);
    const region = fitRegion(m);
    const corners = region
        ? [project(region.x0, region.y0), project(region.x1, region.y0),
        project(region.x0, region.y1), project(region.x1, region.y1)]
        : Object.keys(m.points).map(k => project(m.points[k].x, m.points[k].y));
    const sx = corners.map(c => c[0]), sy = corners.map(c => c[1]);
    const rx0 = Math.min.apply(null, sx), rx1 = Math.max.apply(null, sx);
    const ry0 = Math.min.apply(null, sy), ry1 = Math.max.apply(null, sy);
    // The fit leaves a margin in SCREEN space, not a fraction of the region: a
    // position's label is drawn 11 px outside its square and is the same 11 px
    // whatever the plant measures, so a percentage shrink either wastes half
    // the frame on a long plant or clips the labels on a short one.
    const k = Math.min((w - 2 * LABEL_PAD) / (rx1 - rx0), (h - 2 * LABEL_PAD) / (ry1 - ry0)) * S.mapZoom;
    const ox = (w - (rx1 - rx0) * k) / 2, oy = (h - (ry1 - ry0) * k) / 2;
    const T = (x, y) => { const s = project(x, y); return [ox + (s[0] - rx0) * k, oy + (s[1] - ry0) * k]; };
    const Tp = n => { const p = m.points[n]; return p ? T(p.x, p.y) : null; };
    const edgeD = e => {
        const a = Tp(e.from), d = Tp(e.to);
        if (!a || !d) return '';
        if (!e.h) return 'M' + a[0] + ' ' + a[1] + 'L' + d[0] + ' ' + d[1];
        return cubicPathD(a, T(e.h[0], e.h[1]), T(e.h[2], e.h[3]), d);
    };

    let s = '';
    // Every drivable segment, ONCE PER PHYSICAL LANE. The graph is stored
    // directed and driven both ways, so a straight walk double-strokes every
    // aisle (shared/scene-geom.js, laneKey).
    const drawn = {};
    for (const e of m.edges) {
        const key = laneKey(e.from, e.to);
        if (drawn[key]) continue;
        drawn[key] = true;
        s += '<path class="mp-edge" d="' + edgeD(e) + '"/>';
    }

    // The two trips the routing set implies: Robot 1 out with a new bin,
    // Robot 2 home with the old one.
    const legs = routeLegs();
    const pathD = p => p.map(edgeD).join('');
    if (legs.supply) s += '<path class="mp-route r1" d="' + pathD(legs.supply) + '"/>';
    if (legs.back) s += '<path class="mp-route r2" d="' + pathD(legs.back) + '"/>';

    for (const n of waypointNames()) {
        const p = Tp(n);
        if (!p) continue;
        s += '<circle class="mp-key" cx="' + p[0] + '" cy="' + p[1] + '" r="4"/>' +
            '<text class="mp-keyt" x="' + (p[0] + 7) + '" y="' + (p[1] - 5) + '">' + esc(n) + '</text>';
    }

    // The groups the set names, as a dashed rectangle around their members.
    for (const r of S.routing || []) {
        const mem = membersOfGroup(r.core_node_name).map(Tp).filter(Boolean);
        if (!mem.length) continue;
        const gx = mem.map(p => p[0]), gy = mem.map(p => p[1]);
        const x0 = Math.min.apply(null, gx) - 14, y0 = Math.min.apply(null, gy) - 14;
        const x1 = Math.max.apply(null, gx) + 14, y1 = Math.max.apply(null, gy) + 14;
        s += '<rect class="mp-grp" x="' + x0 + '" y="' + y0 + '" width="' + (x1 - x0) +
            '" height="' + (y1 - y0) + '" rx="8" data-mapname="' + esc(r.core_node_name) + '"/>' +
            '<text class="mp-grpt" x="' + x0 + '" y="' + (y0 - 8) + '">' +
            esc(r.label || r.core_node_name) + '</text>';
        for (const p of mem) {
            s += '<rect class="mp-smn" x="' + (p[0] - 5) + '" y="' + (p[1] - 5) + '" width="10" height="10" rx="2"/>';
        }
    }

    // THE PRESS, AND ITS LABELS OFF EACH OTHER.
    //
    // A press's positions are about 1.7 m apart, which at plant scale is a few
    // pixels: at Hopkinsville PLN_04/PLN_06 and PLN_01/PLN_03 land within 6 px
    // of each other and their labels sat on top of each other, unreadable.
    //
    // The side used to be chosen by CellPosition.Kind, which is not a place —
    // it says whether a position is a partner slot for any style, and all four
    // of those are Kind "front", so all four labels went right at their own y.
    // Nor is a front/back ROW the answer here: this map is rotated to lie the
    // supply run along the wide axis, and the press comes out as a near-
    // vertical line of six marks rather than two rows of three.
    //
    // So the labels are PLACED, not sided: each one takes the first of four
    // offsets (right-up, right-down, left-up, left-down) whose box clears every
    // label already placed. It is a few marks on one press — the quadratic cost
    // is nothing, and it is the only rule that keeps working whichever way the
    // map is turned.
    const positions = (S.composer.cell && S.composer.cell.positions) || [];
    const live = {};
    if (S.model) for (const n of Object.keys(S.model.cells)) if (S.model.cells[n].on) live[n] = true;
    for (const lab of placeMapLabels(positions, Tp)) {
        s += '<rect class="mp-pos' + (live[lab.name] ? ' on' : '') + '" x="' + (lab.x - 6) + '" y="' + (lab.y - 6) +
            '" width="12" height="12" rx="2" data-mapname="' + esc(lab.name) + '"/>' +
            '<text class="mp-post" x="' + lab.lx + '" y="' + lab.ly + '" text-anchor="' + lab.anchor + '">' +
            esc(lab.name) + '</text>';
    }

    // The press's own name, above its rows — the map says which press this is
    // without the engineer reading position ids to find out.
    const placed = positions.map(pos => Tp(pos.core_node_name)).filter(Boolean);
    if (placed.length) {
        const p = process();
        const lx = placed.reduce((a, q) => a + q[0], 0) / placed.length;
        const ly = Math.min.apply(null, placed.map(q => q[1]));
        s += '<text class="mp-grpt" x="' + lx + '" y="' + (ly - 22) + '" text-anchor="middle">' +
            esc((p ? p.name : '').toUpperCase()) + '</text>';
    }

    return '<svg viewBox="0 0 ' + w + ' ' + h + '" id="pd-map">' + s + '</svg>';
}

// routeLegs picks the pair of trips the map draws: whatever the flow on screen
// actually says. The first live cell's source to where its new bin waits, and
// the position itself back out to where its old bin goes. A group is entered
// at one of its members, because a group is not a place a robot drives to.
// placeMapLabels puts each press label somewhere it does not sit on another.
//
// Four candidate offsets per mark, in order — right-up, right-down, left-up,
// left-down — and the first whose box clears every box already placed wins. If
// all four collide (three marks in a 12 px huddle), the last is taken and
// pushed a row further down, which is worse than ideal and still readable.
//
// The box is estimated rather than measured: getBBox() needs the element in
// the document and these are built as a string. 5.4 px per character at the
// 9 px mono the labels use, and a 12 px line.
const MAP_LABEL_CH = 5.4;
const MAP_LABEL_H = 12;

function placeMapLabels(positions, Tp) {
    const out = [];
    const placed = [];
    const hits = (a, b) => a.x0 < b.x1 && b.x0 < a.x1 && a.y0 < b.y1 && b.y0 < a.y1;
    for (const pos of positions) {
        const n = pos.core_node_name;
        const p = Tp(n);
        if (!p) continue;
        const w = n.length * MAP_LABEL_CH;
        let chosen = null;
        for (const [dx, dy] of [[11, -6], [11, 6], [-11, -6], [-11, 6]]) {
            const lx = p[0] + dx, ly = p[1] + 3.5 + dy;
            const box = {
                x0: dx > 0 ? lx : lx - w, x1: dx > 0 ? lx + w : lx,
                y0: ly - MAP_LABEL_H, y1: ly + 3,
            };
            if (placed.every(q => !hits(box, q))) {
                chosen = { lx: lx, ly: ly, anchor: dx > 0 ? 'start' : 'end', box: box };
                break;
            }
        }
        if (!chosen) {
            // Every side taken. Drop it a line rather than draw it on a
            // neighbour: an offset label is readable, an overlapped one is not.
            const lx = p[0] + 11, ly = p[1] + 3.5 + 6 + MAP_LABEL_H;
            chosen = {
                lx: lx, ly: ly, anchor: 'start',
                box: { x0: lx, x1: lx + w, y0: ly - MAP_LABEL_H, y1: ly + 3 },
            };
        }
        placed.push(chosen.box);
        out.push({ name: n, x: p[0], y: p[1], lx: chosen.lx, ly: chosen.ly, anchor: chosen.anchor });
    }
    return out;
}

function routeLegs() {
    const out = { supply: null, back: null };
    if (!S.model) return out;
    const n = Object.keys(S.model.cells).find(k => S.model.cells[k].on && S.model.cells[k].mode);
    if (!n) return out;
    const c = S.model.cells[n];
    const oneOf = name => membersOfGroup(name)[0] || name;
    const park = c.staging || c.paired || n;
    out.supply = route(graphNodeFor(oneOf(c.source)), graphNodeFor(park));
    out.back = route(graphNodeFor(n), graphNodeFor(oneOf(c.dest)));
    return out;
}

// The waypoints the composer offers, and the dots on the map: the LM points
// along the supply path, evenly spaced, at most four.
//
// ONE WALK, AND NOW THAT IS TRUE. This comment said it while reading
// routeLegs() — the desktop's own cubic-arc route, which is a DRAWING — so the
// dots on the map and the options in the picker came from two different
// searches over two different graphs. The drawing stays the desktop's; which
// points are offered is a choice, and the model makes it once for both
// surfaces.
function waypointNames() {
    if (!S.model) return [];
    const n = Object.keys(S.model.cells).find(k => S.model.cells[k].on && S.model.cells[k].mode);
    return n ? M().viaWaypoints(S.model, n) : [];
}

// F3 reaches D3's sub-lines too: each one said in other words what the role
// beside it already names, and the evidence line under a backfilled row was
// already saying `inbound source on 10 styles`.
// THREE PARTS, because the heading and the sentence under it are read in two
// places now: as one line over a list of rows, and as a label and its sub-line
// over the picker that adds to that role.
const ROUTING_GROUPS = [
    ['source', 'Sources', "a flow's inbound source"],
    ['staging', 'Staging', "a flow's inbound or outbound staging"],
    ['destination', 'Destinations', "a flow's outbound destination"],
];

function isPositionRow(name) {
    return ((S.composer.cell && S.composer.cell.positions) || [])
        .some(p => p.core_node_name === name);
}

// WHAT BACKFILL MEANS, IN WORDS, ON THE ROW (owner ruling 2026-09-10, Q5).
//
// The row used to carry an origin PILL reading "backfill" and, when it was
// off, an "Adopt" button beside it. Neither said anything: "backfill" is a
// word from the implementation, and an engineer reading it learns that some
// process put the name there, not which of their own flows did. And Adopt was
// a second control for what the switch already does.
//
// So: no pills. A name the backfill found carries one dim sub-line that says
// exactly where it came from, with the evidence — the role it plays in a claim
// and how many live styles play it. A name an engineer typed says so and when.
// The switch IS the approval, so a backfilled row arrives off with its
// sub-line in amber and the sentence that says what turning it on does; once
// on, the same sentence, dim. The server stamps the authorship.
const ROLE_EVIDENCE = {
    source: 'inbound source',
    staging: 'staging',
    destination: 'outbound destination',
};

function backfillEvidence(r) {
    const role = ROLE_EVIDENCE[r.role] || r.role;
    const n = r.style_count || 0;
    return 'Backfilled from your flows · ' + role + ' on ' + n + ' style' + (n === 1 ? '' : 's');
}

function addedHere(r) {
    const d = r.created_at ? String(r.created_at).slice(5, 10).replace('-', '‑') : '';
    return 'Added here' + (d ? ' · ' + d : '');
}

function routingRow(r) {
    const members = membersOfGroup(r.core_node_name);
    const position = !members.length && isPositionRow(r.core_node_name);
    const backfill = r.origin === 'backfill';
    const waiting = backfill && !r.enabled;
    // THE PROVENANCE LINE IS THE ROW'S, AND A GROUP'S MEMBERS DO NOT REPLACE
    // IT. Members used to win, so every group row — which is most of them —
    // showed who was inside and never where the name came from, and Q5's whole
    // sub-line was invisible on exactly the rows it was written for. The
    // provenance is the sub-line; a group's members are a second, quieter one
    // under it.
    const sub = position ? 'back position · always available'
        : backfill ? backfillEvidence(r) + (waiting ? ' · switch on to let operators pick it' : '')
            : addedHere(r);
    const members2 = members.length ? '<small class="mem">' + esc(members.join(' · ')) + '</small>' : '';
    // A position is always available to the flow that owns it, so it carries
    // no switch: turning off a press's own back slot is not a routing
    // decision, it is a broken cell.
    const swatch = members.length ? '<span class="sq grp"></span>'
        : position ? '<span class="sq pos"></span>' : '<span class="sq"></span>';
    const tail = position ? ''
        : '<button class="pd-chk' + (r.enabled ? ' on' : '') + '" data-act="rs-toggle" data-row="' + r.id +
        '" role="switch" aria-checked="' + (r.enabled ? 'true' : 'false') + '"></button>';
    return '<div class="pd-rrow' + (waiting ? ' waiting' : '') + '" data-rsname="' + esc(r.core_node_name) + '">' +
        swatch + '<span class="nm">' + esc(r.label || r.core_node_name) +
        '<small' + (waiting ? ' class="bf"' : '') + '>' + esc(sub) + '</small>' + members2 + '</span>' +
        '<span class="sp"></span>' + tail + '</div>';
}

// mapSubject is what the map is a map OF: the press, by the name an engineer
// calls it. The station's name when the process has one, because that is the
// name on the floor; the process's otherwise.
function mapSubject() {
    const p = process();
    const st = p ? stationsOf(p.id)[0] : null;
    return (st && st.name) || (p && p.name) || 'this press';
}

// ── the routing set, in Settings (owner ruling 2026-09-16) ───────────────────
//
// THE ROUTING SET HAD A TAB, AND CREATION IS NOT REVIEW. That tab was built to
// read backfilled rows on a migrated process — the derive endpoint, the amber
// switch, the evidence line under a name — and every one of those is a REVIEW,
// which is what Settings is for. Making a routing set is three short lists on
// the Add-process sheet and is over in a minute. So the review lives here,
// beside the gate it is the precondition for, and the map beside it is a
// read-back rather than the only way in.
//
// THREE LISTS, NOT ONE, because composer-model's defaultRouting picks a cell's
// source and destination by lowest sequence PER ROLE: source, staging and
// destination are different questions and a single list of nodes could not
// answer any of them.

const ROUTING_ADD_SUB = 'add by name — the picker is Core’s own list';

function routingPickersReady() {
    for (const g of ROUTING_GROUPS) {
        const key = routingPickerKey(g[0]);
        // THE PICKER'S STATE BELONGS TO THE TAB, NOT TO A REDRAW. drawSettings
        // runs on every switch flip and after every save, and re-initialising
        // here would clear the half-typed filter under the engineer's hands.
        // openProcess drops them when the process changes.
        if (S.pickers[key] && S.pickers[key].onPick) continue;
        pickerInit(key, {
            // WRITE-THROUGH: the rows above the picker are the selection, so
            // the picker holds none of its own. A name picked here is a row on
            // the server before the list redraws.
            onPick: name => addRoutingNode(g[0], name),
            exclude: n => (isPositionRow(n)
                ? 'a position of this press — available to every flow on it already'
                : ''),
            annotate: n => {
                const have = (S.routing || []).find(r => r.core_node_name === n && r.role === g[0]);
                return have ? (have.enabled ? 'already in this list' : 'in this list, switched off') : '';
            },
        });
    }
}

// The rows of one role, with the press's own back positions folded into
// staging — see the note in routingRole.
function routingRowsFor(role) {
    let mine = (S.routing || []).filter(r => r.role === role);
    // THE PRESS'S OWN BACK POSITIONS ARE STAGING, and they are not routing
    // rows. The derive reads inbound_staging / outbound_staging, and a
    // press-index cell parks on its PAIRED position instead — deliberately,
    // because a paired position is choreography and not a routing choice.
    // They still belong under this heading: an engineer reading "where Robot 1
    // parks a bin" and seeing nothing would conclude the press has nowhere to
    // park. So they are listed from the picture, with no switch, which is what
    // "always available" means.
    if (role !== 'staging') return mine;
    const named = {};
    for (const r of mine) named[r.core_node_name] = true;
    for (const pos of (S.composer.cell && S.composer.cell.positions) || []) {
        if (pos.kind !== 'back' || named[pos.core_node_name]) continue;
        mine = mine.concat([{
            id: 0, core_node_name: pos.core_node_name, role: 'staging',
            label: pos.core_node_name, origin: 'position', enabled: true, style_count: 0,
        }]);
    }
    return mine;
}

function routingRole(g) {
    const rows = routingRowsFor(g[0]);
    const body = rows.length
        ? rows.map(routingRow).join('')
        : '<div class="pd-dim">nothing yet — this press can draw on nothing under this heading</div>';
    return stBlock(g[1], g[2] + ' · ' + ROUTING_ADD_SUB,
        body + pickerBox(routingPickerKey(g[0])));
}

function routingSection() {
    routingPickersReady();
    const way = waypointNames();
    const waypoints = way.length
        ? stBlock('Waypoints', 'the points the composer offers on the shortest supply path · from the map',
            way.map(n => '<div class="pd-rrow"><span class="sq lm"></span><span class="nm">' + esc(n) +
                '<small>on the shortest supply path</small></span><span class="sp"></span></div>').join(''))
        : '';
    const m = mapOf();
    return '<div class="pd-sect"><h2>Routing set</h2>' +
        '<span class="pd-dim">' + esc(S.routingSummary ||
            'where this press may draw bins from, stage them, and send them') + '</span></div>' +
        ROUTING_GROUPS.map(routingRole).join('') +
        waypoints +
        (S.routingError ? '<div class="pd-refusal">' + esc(S.routingError) + '</div>' : '') +
        '<p class="pd-note">Operators are offered these names and never the plant. A name a live ' +
        'flow uses cannot be removed. A name the backfill found arrives switched off, in amber: ' +
        'switching it on is what lets operators pick it — and the flow composer below stays shut ' +
        'until this list has been read.</p>' +
        // THE MAP IS THE READ-BACK. It draws what the set says, fitted to this
        // press, and a click on a name flashes that name's row. It is not an
        // input any more: a click on a dot could only guess at the role, and
        // the three lists above are the engineer saying it.
        '<div class="pd-map pd-rsmap">' + drawMap(1040, 520) +
        '<div class="cap">' + esc(mapSubject()) + ' · ' +
        esc(m ? 'plant map from Core · revision ' + m.revision : 'no plant map cached') +
        ' · teal = Robot 1 supply path · indigo = Robot 2 return</div>' +
        '<div class="zoom"><button data-act="rs-zoom" data-z="in">+</button>' +
        '<button data-act="rs-zoom" data-z="out">&minus;</button>' +
        '<button data-act="rs-zoom" data-z="home">&#8962;</button></div></div>';
}

// The writes this screen makes are U5's, and a refusal comes back BY NAME —
// which is the whole reason the server refuses rather than the page hiding the
// switch: "PLN_02 is used by PART 40421-RVJ56.37" is an answer, "cannot
// disable" is not.
async function routingPatch(id, body) {
    const out = await postJSON('PATCH', '/api/processes/' + S.processID + '/routing-nodes/' + id, body);
    S.routingError = out.ok ? '' : out.error;
    await loadRouting();
    drawSettings();
}

// A name picked in one of the three lists, posted in THAT role. origin and the
// author are the server's to stamp.
async function addRoutingNode(role, name) {
    const out = await postJSON('POST', '/api/processes/' + S.processID + '/routing-nodes',
        B().routingAdd(name, role));
    S.routingError = out.ok ? '' : out.error;
    await loadRouting();
    drawSettings();
    // Back into the field the name was typed into: the engineer adding one
    // name is usually adding three.
    const box = $('npk-' + routingPickerKey(role));
    const input = box && box.querySelector('[data-npkq]');
    if (input) input.focus();
}

async function loadRouting() {
    const res = await fetch('/api/processes/' + S.processID + '/routing-nodes');
    const view = res.ok ? await res.json() : {};
    let rows = Array.isArray(view) ? view : (view.rows || view.routing_nodes || []);
    S.routing = rows;
    // THE SUMMARY IS THE SERVER'S SENTENCE. The panel used to build its own
    // from the rows — "derived from N claims" counted the claim_count on the
    // styles block — and the derivation's own numbers are not on this page:
    // how many claims it READ, and how many names still need a decision
    // against Core's list. Two sentences about one derivation, and the one on
    // screen was the one that could not see it.
    S.routingSummary = view.summary || '';
}

// Clicking a name on the map FLASHES ITS ROW, and that is the whole of it.
//
// It used to ADD the name, in the role the map implied — a group sources and
// receives, a press position stages — and that implication is a guess the
// engineer can make better than the drawing can: they say the role by which of
// the three lists they put the name in. SPEC's "clicking a map group or node
// adds it" is superseded (owner ruling 2026-09-16).
//
// A name with no row — a front position, or any of the plant drawn faint
// behind the press — flashes nothing. The map is most of a plant this press
// has no business in, and a sentence for every faint dot is noise.
function flashRoutingRow(name) {
    const row = root().querySelector('[data-rsname="' + CSS.escape(name) + '"]');
    if (!row) return;
    row.scrollIntoView({ block: 'nearest' });
    row.classList.add('flash');
}

async function openSettings() {
    S.settings = S.settings || settingsDraft();
    if (!S.routing) await loadRouting();
    drawSettings();
}

// ── D2 · Advanced (SPEC §2 D2) ───────────────────────────────────────────────
//
// The modal is the ONLY place these columns appear (ruling R2). Twelve of the
// fourteen fields are the model's advanced block; the other two — evacuation
// positions and evacuation destination — are on the cell already, because the
// picture draws what they do. Apply dispatches both and marks the flow dirty;
// nothing is written until Save flow.
//
// WHAT IS DRAWN IS flowspec's DECISION, not a list here. A field this
// choreography forbids or never reads is not a control an engineer should be
// handed, and the same table the server validates against decides it.
//
// The sheet lives outside #pd-root so a redraw of the page behind it cannot
// take it away mid-edit.
const ADV_SECTIONS = [
    ['Part identity', ['allowed_payload_codes']],
    ['Replenishment', ['reorder_point', 'auto_reorder', 'lineside_soft_threshold', 'auto_request_payload', 'auto_push']],
    ['Changeover specials', ['evacuate_on_changeover', 'evac_nodes', 'evac_dest', 'changeover_carryover_disposition']],
    ['Press hardware', ['index_robot_supplies']],
    ['Station policy', ['auto_confirm']],
    // BOARD is one field, and it earns a section rather than a corner of
    // another: the loader board's draw order is not replenishment, not a
    // changeover special and not station policy, and putting it under a
    // heading it does not belong to is how a field becomes unfindable.
    ['Board', ['sequence']],
];

// ADV_COPY — the sheet's thirteen rows: which CLAIM FIELD each one edits, and
// the one-line help under it.
//
// THE HELP IS THIS PAGE'S; THE NAME IS NOT. This used to carry both, so it was
// a sixth table of words for claim fields — "Reorder point" here, "Reorder
// Point" in a refusal; "Evacuation positions" here, "Changeover Evac Nodes" in
// the diagnostic about the same column. The help sentences say what the field
// DOES on this screen and have no equivalent anywhere else, so they stay; the
// names come from flowspec like every other word.
//
// Two keys are the sheet's own (evac_nodes, evac_dest) because the sheet had
// short names for them before the fields did; the field each one edits is
// named here rather than guessed from the key.
const ADV_COPY = {
    allowed_payload_codes: ['allowed_payload_codes', 'which payloads a robot may bring to this position'],
    reorder_point: ['reorder_point', 'units left when the next bin is requested'],
    auto_reorder: ['auto_reorder', 'request without the operator'],
    lineside_soft_threshold: ['lineside_soft_threshold', 'warn the operator above twice this on a release'],
    auto_request_payload: ['auto_request_payload', 'which part a vacated position asks for on its own'],
    auto_push: ['auto_push', 'drain the source whenever the window is free'],
    evacuate_on_changeover: ['evacuate_on_changeover', 'clear this position before the new style'],
    evac_nodes: ['changeover_evac_nodes', 'tooling change — which positions get cleared'],
    evac_dest: ['changeover_evac_destination', ''],
    changeover_carryover_disposition: ['changeover_carryover_disposition', 'what happens to a part-full bin'],
    index_robot_supplies: ['index_robot_supplies', 'the same for every part on this press'],
    auto_confirm: ['auto_confirm', 'the plant setting applies unless set here'],
    sequence: ['sequence', 'where this position sits on the loader board'],
};

const CARRYOVER = {
    replace: 'replace',
    keep_lineside: 'keep at the line',
    outbound_staging: 'hop out and come back',
};

function openAdvanced(node) {
    const c = S.model.cells[node];
    if (!c) return;
    S.adv = {
        node: node,
        a: M().advancedFor(S.model, node),
        evacNodes: (c.evacNodes || []).slice(),
        evacDest: c.evacDest || '',
    };
    drawAdvanced();
}

function advShows(key) {
    if (key === 'evac_nodes' || key === 'evac_dest') {
        return M().advancedShows(S.model, S.adv.node, 'evacuate_on_changeover');
    }
    return M().advancedShows(S.model, S.adv.node, key);
}

function advValue(key) {
    if (key === 'evac_nodes') return S.adv.evacNodes;
    if (key === 'evac_dest') return S.adv.evacDest;
    return S.adv.a[key];
}

// A field "carries a value" by the same rule the server counts by; the two
// cell fields follow it because they are counted on the row beside the twelve.
function advIsSet(key) {
    const v = advValue(key);
    if (key === 'changeover_carryover_disposition') return !!v && v !== 'replace';
    return Array.isArray(v) ? v.length > 0 : Boolean(v);
}

function advToggle(key) {
    return '<button class="pd-chk' + (advValue(key) ? ' on' : '') + '" data-adv="' + key +
        '" data-advkind="toggle" role="switch" aria-checked="' + (advValue(key) ? 'true' : 'false') + '"></button>';
}

function advNumber(key) {
    const v = advValue(key);
    return '<input class="pd-inp' + (v ? '' : ' dflt') + '" type="text" inputmode="numeric" ' +
        'data-adv="' + key + '" data-advkind="number" value="' + esc(v ? String(v) : '') + '" placeholder="—">';
}

function advPick(key, label, cls) {
    return '<button class="pd-sel' + (cls ? ' ' + cls : '') + '" data-adv="' + key +
        '" data-advkind="pick">' + label + '<i class="car"></i></button>';
}

function advField(key) {
    // The row's name is the field's word; the sentence under it is this
    // screen's. See ADV_COPY.
    const spec = ADV_COPY[key] || [key, ''];
    const copy = [W(spec[0]), spec[1]];
    let control = '';
    switch (key) {
        case 'allowed_payload_codes': {
            const chips = S.adv.a.allowed_payload_codes.map((p, i) =>
                '<button class="pd-chip set" data-adv="allowed_payload_codes" data-advkind="unchip" data-i="' + i + '">' +
                esc(M().shortPart(p)) + ' <i>&times;</i></button>').join('');
            control = chips + '<button class="pd-chip add" data-adv="allowed_payload_codes" data-advkind="pick">+ add</button>';
            break;
        }
        case 'reorder_point': {
            // The source is a STAMP on the number, not a field of its own: it
            // says how the value got there. There is no calculator behind it on
            // Edge — see the report — so the pill is read-only.
            const src = S.adv.a.reorder_point_source || 'legacy';
            control = advNumber('reorder_point') + '<span class="pd-src">' + esc(src) + '</span>';
            break;
        }
        case 'lineside_soft_threshold': case 'sequence': control = advNumber(key); break;
        case 'auto_request_payload': {
            const v = S.adv.a.auto_request_payload;
            control = advPick(key, esc(v ? M().shortPart(v) : 'off'), v ? '' : 'dflt');
            break;
        }
        case 'evac_nodes': {
            const list = S.adv.evacNodes;
            control = list.length
                ? advPick(key, esc(list.join(' · ')))
                : '<button class="pd-chip add" data-adv="evac_nodes" data-advkind="pick">+ pick positions</button>';
            break;
        }
        case 'evac_dest': {
            const v = S.adv.evacDest;
            const fallback = S.model.cells[S.adv.node].dest || '—';
            // The default is "wherever this position's outbound bins go",
            // named by the field rather than by a second word for it.
            control = advPick(key, v ? esc(v) : '<span class="k">' + esc(W('outbound_destination')) + ' ·</span> ' + esc(fallback), v ? '' : 'dflt');
            break;
        }
        case 'changeover_carryover_disposition': {
            const v = S.adv.a.changeover_carryover_disposition || 'replace';
            control = advPick(key, esc(CARRYOVER[v] || v), v === 'replace' ? 'dflt' : '');
            break;
        }
        case 'index_robot_supplies': {
            // A choice between two named robots, not a flag called "flipped".
            const on = S.adv.a.index_robot_supplies;
            control = advPick(key, on ? 'Robot 2' : 'Robot 1') +
                '<span class="pd-note">' + (on ? 'Robot 1 evacuates and leaves' : 'Robot 2 indexes') + '</span>';
            break;
        }
        default: control = advToggle(key);
    }
    return '<div class="pd-fld"><label>' + esc(copy[0]) +
        (copy[1] ? '<small>' + esc(copy[1]) + '</small>' : '') +
        '</label><div class="v">' + control + '</div></div>';
}

function drawAdvanced() {
    const scrim = $('pd-scrim');
    if (!scrim || !S.adv) return;
    const node = S.adv.node;
    let body = '';
    for (const [title, keys] of ADV_SECTIONS) {
        const shown = keys.filter(advShows);
        if (!shown.length) continue;
        const n = shown.filter(advIsSet).length;
        body += '<div class="pd-sec"><div class="pd-lbl">' + esc(title) +
            (n ? '<span class="cnt">' + n + ' set</span>' : '') + '</div>' +
            shown.map(advField).join('') + '</div>';
    }
    scrim.innerHTML = '<div class="pd-modal" role="dialog" aria-modal="true" aria-label="' + esc(node) + ' Advanced">' +
        '<div class="mh"><h2>' + esc(node) + ' · Advanced</h2><p>' + esc(S.model.styleName) +
        ' · settings the flow doesn’t draw. They ride along with this position when the flow is ' +
        'saved from either screen.</p></div>' +
        '<div class="mb">' + body + '</div>' +
        '<div class="mf"><span class="st">Nothing here changes the flow or the picture.</span>' +
        '<button class="pd-btn" data-act="adv-cancel">Cancel</button>' +
        '<button class="pd-btn primary" data-act="adv-apply">Apply</button></div>' +
        '<div class="pd-pop" id="pd-advpop" hidden></div></div>';
    showSheet();
    scrim.querySelectorAll('[data-advkind="number"]').forEach(el => {
        el.addEventListener('input', () => {
            const digits = el.value.replace(/[^0-9]/g, '');
            S.adv.a[el.dataset.adv] = digits ? Number(digits) : 0;
            // A number the engineer types by hand is a manual reorder point.
            if (el.dataset.adv === 'reorder_point') S.adv.a.reorder_point_source = digits ? 'manual' : 'legacy';
        });
    });
}

function closeAdvanced() {
    hideSheet();
    S.adv = null;
}

// Apply: the twelve through setAdvanced, the two evacuation fields through the
// cell's own actions. Three reduces rather than one because they are three
// different things the model already knows how to do.
function applyAdvanced() {
    const node = S.adv.node;
    let m = M().reduce(S.model, { type: 'setAdvanced', node: node, advanced: S.adv.a });
    m = M().reduce(m, { type: 'setEvacNodes', node: node, nodes: S.adv.evacNodes });
    m = M().reduce(m, { type: 'setEvacDest', node: node, dest: S.adv.evacDest });
    S.model = m;
    closeAdvanced();
    drawFlows();
    schedulePreview();
}

function advOptions(key) {
    const c = S.model.cells[S.adv.node];
    const routing = S.composer.routing || [];
    switch (key) {
        case 'allowed_payload_codes': {
            const have = new Set(S.adv.a.allowed_payload_codes);
            return S.model.parts.filter(p => !have.has(p))
                .map(p => ({ label: M().shortPart(p), run: () => S.adv.a.allowed_payload_codes.push(p) }));
        }
        case 'auto_request_payload':
            return [{ label: 'off', on: !S.adv.a.auto_request_payload, run: () => { S.adv.a.auto_request_payload = ''; } }]
                .concat(S.model.parts.map(p => ({
                    label: M().shortPart(p), on: S.adv.a.auto_request_payload === p,
                    run: () => { S.adv.a.auto_request_payload = p; },
                })));
        case 'changeover_carryover_disposition':
            return Object.keys(CARRYOVER).map(k => ({
                label: CARRYOVER[k], on: (S.adv.a.changeover_carryover_disposition || 'replace') === k,
                run: () => { S.adv.a.changeover_carryover_disposition = k; },
            }));
        case 'index_robot_supplies':
            return [
                { label: 'Robot 1', on: !S.adv.a.index_robot_supplies, run: () => { S.adv.a.index_robot_supplies = false; } },
                { label: 'Robot 2', on: !!S.adv.a.index_robot_supplies, run: () => { S.adv.a.index_robot_supplies = true; } },
            ];
        case 'evac_nodes': {
            // THE CELL'S OWN POSITIONS, not the press's. changeover_evac_nodes
            // marks which of THIS cell's nodes are in the way of a setup.
            const mine = [S.adv.node, c.paired, c.secondPaired, c.staging, c.parkOld].filter(Boolean);
            return [...new Set(mine)].map(n => ({
                label: n, on: S.adv.evacNodes.indexOf(n) >= 0, keepOpen: true,
                run: () => {
                    const i = S.adv.evacNodes.indexOf(n);
                    if (i >= 0) S.adv.evacNodes.splice(i, 1); else S.adv.evacNodes.push(n);
                },
            }));
        }
        case 'evac_dest':
            return [{
                label: W('outbound_destination') + ' · ' + (c.dest || '—'), on: !S.adv.evacDest,
                run: () => { S.adv.evacDest = ''; },
            }].concat(routing.filter(r => r.role === 'destination').map(r => ({
                label: r.label || r.core_node_name, on: S.adv.evacDest === r.core_node_name,
                run: () => { S.adv.evacDest = r.core_node_name; },
            })));
        default: return [];
    }
}

function openAdvPicker(btn) {
    const opts = advOptions(btn.dataset.adv);
    const pop = $('pd-advpop');
    if (!pop) return;
    pop.innerHTML = opts.length
        ? opts.map((o, i) => '<button data-opt="' + i + '" class="' + (o.on ? 'on' : '') + '">' + esc(o.label) + '</button>').join('')
        : '<div class="none">Nothing to choose here yet.</div>';
    pop.hidden = false;
    const r = btn.getBoundingClientRect();
    const host = pop.offsetParent.getBoundingClientRect();
    pop.style.left = Math.min(host.width - pop.offsetWidth - 8, Math.max(8, r.left - host.left)) + 'px';
    pop.style.top = (r.bottom - host.top + 6) + 'px';
    pop.querySelectorAll('[data-opt]').forEach(b => b.addEventListener('click', ev => {
        ev.stopPropagation();
        const o = opts[Number(b.dataset.opt)];
        o.run();
        drawAdvanced();
        if (o.keepOpen) openAdvPicker(root().ownerDocument.querySelector('[data-adv="' + btn.dataset.adv + '"][data-advkind="pick"]') || btn);
    }));
}

function onAdvClick(e) {
    // A NODE PICKER IN A SHEET ANSWERS HERE, for the reason the apply modal's
    // controls do: the scrim is outside #pd-root and onClick never sees it.
    const npk = e.target.closest && e.target.closest('[data-npk]');
    if (npk) { e.stopPropagation(); onPickerClick(npk); return; }
    // A POPOVER OPENED FROM A SHEET CLOSES ON THE NEXT CLICK OUTSIDE IT, which
    // is what #pd-pop's own document listener does for the page. The option
    // rows are inside .pd-pop and so are not "outside" — they run their own
    // handler and close it themselves.
    const spop = $('pd-advpop');
    if (spop && !spop.hidden && !(e.target.closest && e.target.closest('.pd-pop'))) {
        spop.hidden = true;
        spop.innerHTML = '';
    }
    const act = e.target.closest && e.target.closest('[data-act]');
    if (act && act.dataset.act === 'add-pickgroup') {
        e.stopPropagation();
        openGroupPicker(act, id => {
            S.add.groupID = id;
            const g = S.groups.find(x => x.id === id);
            // The button alone, not the sheet: a redraw here would take away
            // the name the engineer has half-typed two fields above.
            const btn = $('pd-addgroup');
            if (btn) btn.innerHTML = esc(g ? g.name : 'Ungrouped') + '<i class="car"></i>';
        });
        return;
    }
    // THE APPLY MODAL'S OWN CONTROLS, and they answer HERE because the modal
    // draws into #pd-scrim, which processes.html puts outside #pd-root — and
    // onClick, which has these three cases, is bound to #pd-root. So a tick, a
    // picker and a part chip all rendered and did nothing: an engineer could
    // open the modal, press every control in it, and only Cancel and Save
    // answered, because those two are `sheet-*` and this handler owns them.
    //
    // It was invisible because the only thing that ever drove this modal was
    // the shots harness's `;pick=auto` hash, which called pickApplyPart and
    // tickApplyRow DIRECTLY and never went near a click. Taking that door out
    // (2026-09-13) is what surfaced it — the same shape as the two classic
    // scripts that killed window.DesktopBodies, and the same lesson: a
    // screenshot never clicks.
    if (act && act.dataset.act === 'papply-tick') { tickApplyRow(Number(act.dataset.style)); return; }
    if (act && act.dataset.act === 'papply-pick') {
        // F1: one picker open at a time, and clicking the open one closes it.
        // The key carries the STYLE as well as the position, because eleven
        // rows can each have a PLN_01.
        const key = act.dataset.style + ':' + act.dataset.node;
        S.papply.open = S.papply.open === key ? null : key;
        drawPresetApply();
        return;
    }
    if (act && act.dataset.act === 'papply-part') {
        pickApplyPart(Number(act.dataset.style), act.dataset.node, act.dataset.part);
        return;
    }
    if (act && act.dataset.act === 'adv-cancel') { closeAdvanced(); return; }
    if (act && act.dataset.act === 'adv-apply') { applyAdvanced(); return; }
    // The confirm/edit sheets share this scrim, so their two buttons answer here.
    if (act && act.dataset.act === 'sheet-cancel') { closeSheet(); return; }
    if (act && act.dataset.act === 'sheet-ok') { if (S.sheet && S.sheet.run) S.sheet.run(); return; }
    // And so does the Generate-variants dialog.
    if (act && act.dataset.act === 'gen-cancel') { closeGenerate(); return; }
    if (act && act.dataset.act === 'gen-run') { runGenerate(); return; }
    if (act && act.dataset.act === 'gen-add') { addGenRow(); drawGenerate(); return; }
    if (act && act.dataset.act === 'gen-drop') {
        S.gen.rows.splice(Number(act.dataset.i), 1);
        if (!S.gen.rows.length) addGenRow();
        drawGenerate();
        return;
    }
    // Its pickers are the page's pickers, and they live inside the scrim, so
    // they open against the dialog's own popover rather than the page's.
    const genPick = e.target.closest && e.target.closest('.pd-modal [data-act="pick"]');
    if (S.gen && genPick) { e.stopPropagation(); openGenPicker(genPick); return; }
    if (S.gen) {
        const pop = $('pd-advpop');
        if (pop && !(e.target.closest && e.target.closest('.pd-pop'))) { pop.hidden = true; pop.innerHTML = ''; }
        return;
    }
    if (!S.adv) return;
    const f = e.target.closest && e.target.closest('[data-advkind]');
    if (!f) {
        const pop = $('pd-advpop');
        if (pop && !(e.target.closest && e.target.closest('.pd-pop'))) { pop.hidden = true; pop.innerHTML = ''; }
        return;
    }
    e.stopPropagation();
    switch (f.dataset.advkind) {
        case 'toggle': S.adv.a[f.dataset.adv] = !S.adv.a[f.dataset.adv]; drawAdvanced(); return;
        case 'unchip': S.adv.a.allowed_payload_codes.splice(Number(f.dataset.i), 1); drawAdvanced(); return;
        case 'pick': openAdvPicker(f); return;
        default: return;
    }
}

// ── D4 · Operator screens (SPEC §2 D4) ───────────────────────────────────────
//
// The HMIs that work this process, what each one claims, and whether it is
// answering. The gate column is read-only here for the same reason the app
// bar's pill is: it is one decision and it lives in Settings (ruling R3).
function healthOf(st) {
    const seen = st.last_seen_at || st.last_seen || '';
    if (!seen) return { on: false, text: 'never seen' };
    const age = (Date.now() - new Date(seen).getTime()) / 1000;
    if (!isFinite(age)) return { on: false, text: 'never seen' };
    const ago = age < 90 ? Math.max(0, Math.round(age)) + ' s ago'
        : age < 5400 ? Math.round(age / 60) + ' min ago'
            : Math.round(age / 3600) + ' h ago';
    // ONLINE IS A CLAIM ABOUT NOW. A screen that answered two minutes ago is
    // not online, and calling it that is how a dead HMI goes unnoticed.
    return { on: age < 90, text: (age < 90 ? 'online · ' : 'last seen ') + ago };
}

function drawScreens() {
    const p = process();
    const gateOn = !!(p && p.flow_composer_enabled);
    const rows = stationsOf(S.processID).map(st => {
        const claimed = (S.stationNodes[String(st.id)] || []);
        const h = healthOf(st);
        return '<tr>' +
            '<td class="pn"><b>' + esc(st.name) + '</b><small>' +
            esc((st.code || 'station') + (st.area_label ? ' · ' + st.area_label : '')) + '</small></td>' +
            '<td>' + (claimed.length
                ? claimed.map(n => '<span class="pd-nchip">' + esc(n) + '</span>').join('')
                : '<span class="pd-dim">nothing claimed</span>') + '</td>' +
            '<td class="pd-dim">' + esc(st.note || '—') + '</td>' +
            '<td><span class="pd-cnt' + (h.on ? ' ok' : '') + '"><i></i>' + esc(h.text) + '</span></td>' +
            '<td class="pd-dim">' + (gateOn ? 'may change flows' : 'run as set up') + '</td>' +
            '<td class="pd-acts">' +
            '<button class="pd-dimlink" data-act="screen-edit" data-station="' + st.id + '">Edit</button>' +
            '<a class="pd-dimlink" href="/operator/station/' + st.id + '">Open</a></td></tr>';
    }).join('');

    root().innerHTML = appbar() + '<div class="pd-sheet">' +
        '<div class="pd-sect"><h2>Operator screens</h2><span class="pd-dim">the HMIs that work this process</span>' +
        '<span class="pd-spacer"></span>' +
        '<button class="pd-btn" data-act="screen-add">Add operator screen</button></div>' +
        (rows
            ? '<table class="pd-tbl"><thead><tr><th>Screen</th><th>Claimed positions</th><th>Note</th>' +
            '<th>Health</th><th>Flow composer</th><th></th></tr></thead><tbody>' + rows + '</tbody></table>'
            : '<p class="pd-dim">No screen works this process yet. An operator cannot reach it until one does.</p>') +
        '<div class="pd-sect"><h2>Loader windows</h2>' +
        '<span class="pd-dim">Core loaders with no screen on this edge</span></div>' +
        '<p class="pd-dim">' + esc(loaderBindingSentence()) + '</p>' +
        '</div>';
}

// The binding table appears when there is something to bind. The Edge learns
// about a loader from Core's aggregate; one with no operator screen anywhere
// here has nobody to work it, and that is the only case worth a row.
function loaderBindingSentence() {
    const gaps = S.loaderGaps || [];
    if (!gaps.length) return 'Every loader Core knows about has a screen. Nothing to bind.';
    const names = gaps.map(g => g.loader_key || g.core_node_name || g.name || String(g));
    return names.length + ' loader' + (names.length === 1 ? '' : 's') + ' Core knows about have no screen ' +
        'on this edge: ' + names.join(', ') + '.';
}

// ── D5 · Settings (SPEC §2 D5) ───────────────────────────────────────────────
//
// R3: THE GATE LIVES HERE. Everywhere else it is a read-only pill, because a
// switch drawn twice is a decision made in two places.
//
// The draft is S.settings; nothing is written until Save settings. The
// counter's live state sits in the section title rather than beside the
// Enabled switch: "is this press counting right now" is a fact about the
// section, and the switch is a setting.
const AUTO_ARM = [
    ['auto', 'Cut over automatically'],
    ['prompt', 'Prompt the operator'],
    ['off', 'Do nothing'],
];

const AUTO_ARM_NOTE = 'Cut over automatically finishes a changeover the operator already started, once ' +
    'the press is confirmed stamping the new part. It never starts one — starting moves robots, and only ' +
    'a person knows the material is there.';

function settingsDraft() {
    const p = process() || {};
    return {
        name: p.name || '',
        description: p.description || '',
        group_id: p.group_id || 0,
        counter_plc_name: p.counter_plc_name || '',
        counter_tag_name: p.counter_tag_name || '',
        counter_enabled: !!p.counter_enabled,
        changeover_auto_arm: p.changeover_auto_arm || 'auto',
        flow_composer_enabled: !!p.flow_composer_enabled,
    };
}

function settingsDirty() {
    return !!S.settings && JSON.stringify(S.settings) !== JSON.stringify(settingsDraft());
}

function stField(label, sub, control) {
    return '<div class="pd-sfld"><label>' + esc(label) +
        (sub ? '<small>' + esc(sub) + '</small>' : '') + '</label><div class="v">' + control + '</div></div>';
}

// A settings field whose control is a COLUMN — a list of rows and the picker
// under it — rather than one control on a line. Same grid, same label column;
// only the value box stacks.
function stBlock(label, sub, inner) {
    return '<div class="pd-sfld"><label>' + esc(label) +
        (sub ? '<small>' + esc(sub) + '</small>' : '') + '</label><div class="v col">' + inner + '</div></div>';
}

function stText(key, wide) {
    return '<input class="pd-inp' + (wide ? ' wide' : '') + '" type="text" data-st="' + key +
        '" value="' + esc(S.settings[key]) + '">';
}

function stToggle(key) {
    return '<button class="pd-chk' + (S.settings[key] ? ' on' : '') + '" data-act="st-toggle" data-st="' + key +
        '" role="switch" aria-checked="' + (S.settings[key] ? 'true' : 'false') + '"></button>';
}

function drawSettings() {
    if (!S.settings) S.settings = settingsDraft();
    const p = process() || {};
    const counting = !!(p.counter_plc_name && p.counter_enabled);
    const group = S.groups.find(g => g.id === S.settings.group_id);
    const styles = stylesOf(S.processID).length;

    const general = '<div class="pd-sect"><h2>General</h2></div>' +
        stField('Name', '', stText('name')) +
        stField('Description', '', stText('description', true)) +
        stField('Group', 'pure taxonomy for the list — nothing reads it',
            '<button class="pd-sel" data-act="st-group">' + esc(group ? group.name : 'Ungrouped') + '<i class="car"></i></button>');

    const counter = '<div class="pd-sect"><h2>Production counter</h2>' +
        '<span class="pd-cnt' + (counting ? ' ok' : '') + '"><i></i>' +
        esc(counting ? 'counting' : 'not wired') + '</span></div>' +
        stField('PLC', '', stText('counter_plc_name')) +
        stField('Tag', '', stText('counter_tag_name')) +
        stField('Enabled',
            'a press that counts can skip a same-part swap at changeover; an unwired one never does',
            stToggle('counter_enabled'));

    const seg = AUTO_ARM.map(m => '<button class="' + (S.settings.changeover_auto_arm === m[0] ? 'on' : '') +
        '" data-act="st-arm" data-arm="' + m[0] + '">' + esc(m[1]) + '</button>').join('');
    const changeover = '<div class="pd-sect"><h2>Changeover</h2></div>' +
        stField('On a confirmed part (CATID) change', 'the PLC says a new part is stamping',
            '<div class="pd-seg">' + seg + '</div>') +
        '<p class="pd-note">' + esc(AUTO_ARM_NOTE) + '</p>';

    const hmi = '<div class="pd-sect"><h2>HMI</h2></div>' +
        stField('Operators may change the flow on the HMI',
            'off: the operator picks a part and runs it as set up · on: they may also change how it flows',
            stToggle('flow_composer_enabled')) +
        '<p class="pd-note">Turn this on once the routing set below has been reviewed — it is the ' +
        'list an operator would be choosing from. Off is right for a press whose flows are hammered ' +
        'out.</p>';

    const stylesSect = '<div class="pd-sect"><h2>Styles</h2><span class="pd-dim">' + styles +
        ' live · every part this press runs</span><span class="pd-spacer"></span>' +
        // GENERATE VARIANTS, PORTED. The dialog is not new design: the retired
        // admin page had one (base style, a column per produce claim, a row
        // per variant, Generate) and the owner ruled it is the same dialog
        // behind this button. What changed is the mechanism, not the shape:
        // the desktop's own modal and its own chip pickers, because the old
        // one was built from native <select>, which the style guide refuses.
        // POST /api/styles/{id}/generate is untouched.
        '<button class="pd-btn" data-act="st-generate">Generate variants…</button>' +
        '<button class="pd-btn" data-act="st-sync">Sync catalog</button></div>' +
        '<p class="pd-note">Styles are managed from Flows — each row in its left rail is a style. Rename, ' +
        'expected CATID, clone and delete are on the row’s menu; "Mark as running" is there too, for when ' +
        'the plant changed parts without shingo.</p>';

    const danger = '<div class="pd-sect danger"><h2>Danger</h2></div>' +
        stField('Delete this process', 'its styles, flows, routing set and screens go with it',
            '<button class="pd-btn danger" data-act="st-delete">Delete ' + esc(S.settings.name) + '…</button>');

    root().innerHTML = appbar() + '<div class="pd-sheet pd-settings">' +
        general + counter + changeover + hmi + routingSection() + stylesSect + danger +
        '<div class="pd-savebar"><span class="prov' + (settingsDirty() ? ' dirty' : '') + '">' +
        (settingsDirty() ? 'Unsaved changes' : 'No unsaved changes') + '</span>' +
        '<button class="pd-btn" data-act="st-discard">Discard</button>' +
        '<button class="pd-btn primary" data-act="st-save"' + (settingsDirty() ? '' : ' disabled') +
        '>Save settings</button></div>' +
        (S.settingsError ? '<div class="pd-refusal">' + esc(S.settingsError) + '</div>' : '') +
        '<div class="pd-pop" id="pd-stpop" hidden></div></div>';

    for (const el of root().querySelectorAll('[data-st]')) {
        if (el.tagName !== 'INPUT') continue;
        el.addEventListener('input', () => {
            S.settings[el.dataset.st] = el.value;
            markSettingsBar();
        });
    }
    bindPickers();
}

// The save bar alone, so typing a name does not redraw the field under the
// caret and put the cursor back at the start.
function markSettingsBar() {
    const bar = root().querySelector('.pd-savebar');
    if (!bar) return;
    const d = settingsDirty();
    bar.querySelector('.prov').className = 'prov' + (d ? ' dirty' : '');
    bar.querySelector('.prov').textContent = d ? 'Unsaved changes' : 'No unsaved changes';
    bar.querySelector('[data-act="st-save"]').disabled = !d;
}

// TWO DOORS, AND THAT IS THE SERVER'S SHAPE, NOT A CHOICE MADE HERE. PUT
// /processes/{id} takes the process; the flow-composer gate has its own PATCH
// because it is flipped from its own control after a review, never alongside a
// name edit (router.go). Saving both from one screen means calling both.
async function saveSettings() {
    const before = settingsDraft();
    const body = B().processSettings(process(), S.settings);
    const fail = async res => {
        let why = 'The server refused the change (' + res.status + ').';
        try { const j = await res.json(); why = j.error || j.message || why; } catch (_) { /* status only */ }
        S.settingsError = why;
    };
    S.settingsError = '';
    const res = await fetch('/api/processes/' + S.processID, {
        method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
    });
    if (!res.ok) await fail(res);
    if (!S.settingsError && S.settings.flow_composer_enabled !== before.flow_composer_enabled) {
        const g = await fetch('/api/processes/' + S.processID, {
            method: 'PATCH', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(B().processGate(S.settings.flow_composer_enabled)),
        });
        if (!g.ok) await fail(g);
    }
    await reloadProcesses();
    S.settings = settingsDraft();
    drawSettings();
}

async function reloadProcesses() {
    const res = await fetch('/api/processes');
    if (!res.ok) return;
    const rows = await res.json();
    if (Array.isArray(rows)) S.processes = rows;
}

async function reloadStations() {
    const res = await fetch('/api/operator-stations');
    if (!res.ok) return;
    const rows = await res.json();
    if (Array.isArray(rows)) S.stations = rows;
}

// The positions one screen claims, re-read from the server rather than assumed
// from what was just sent. S.stationNodes arrives with the page and nothing
// refreshes it, so a screen whose positions were set here went on reporting
// "nothing claimed" on D4 until the page was reloaded.
async function refreshStationNodes(stationID) {
    const res = await fetch('/api/operator-stations/' + stationID + '/claimed-nodes');
    if (!res.ok) return;
    const names = await res.json();
    S.stationNodes[String(stationID)] = Array.isArray(names) ? names : [];
}

// ── one scrim, the edge's own ─────────────────────────────────────
//
// #pd-scrim IS a .modal-overlay. It was a second one: .pd-scrim's rule was
// position:fixed/inset:0/var(--scrim)/flex-centre/var(--z-modal), which is
// .modal-overlay's rule with a different name and a different switch (the
// hidden attribute instead of .active). Two names for one backdrop is two
// things to keep in step, and they had already diverged — every other modal on
// this edge locks the body's scroll while it is open and this one did not, so
// the page behind an open sheet scrolled under it.
//
// THE BASE'S HELPERS, NOT A COPY OF THEM. header.html loads shingoedge.js as a
// module on every page, so importing showModal/hideModal here is free: no
// request, no bytes, no second parse. That is the whole reason this can be a
// thin layer rather than a re-implementation.
//
// preserveState: hideModal's default walks the modal's inputs and resets them,
// which is for a static modal that will be shown again. This scrim's content
// is rebuilt from scratch on every open and thrown away here, so the walk
// would be work done on fields about to stop existing.
//
// ONE OPEN AND ONE CLOSE. There were four of the first and three of the
// second — Advanced, Generate, the sheets and the apply modal each did their
// own `scrim.hidden = false`, so a fix to what opening means had four places
// to land in and reached whichever the author remembered.
function showSheet(html) {
    const scrim = $('pd-scrim');
    if (!scrim) return;
    if (html !== undefined) scrim.innerHTML = html;
    showModal('pd-scrim');
}

function hideSheet() {
    const scrim = $('pd-scrim');
    if (!scrim) return;
    hideModal('pd-scrim', { preserveState: true });
    scrim.innerHTML = '';
}

// ── the node picker ──────────────────────────────────────────────────────────
//
// NODES ARE PICKED FROM LISTS, BY NAME (owner ruling 2026-09-16). An engineer
// identifies a node by its name — SMN_029, PLK_H1 — because that is how the
// fleet, the PLC and the floor refer to it. Finding that name as a dot on a
// 383-point plant is the wrong way round, and it is why the routing set was
// hard to author. Every place this page asks for nodes is this one component;
// the map beside it is a read-back and not an input.
//
// ONE COMPONENT, EVERY HOST: the Add-process sheet's four lists, the operator
// screen's positions, and Settings' three role lists. A picker in a sheet and
// a picker on a page are the same object — only the handler that reaches its
// clicks differs, because #pd-scrim is outside #pd-root.
//
// EXCLUDED NAMES ARE SHOWN, DIMMED, WITH THE REASON. Hiding them makes the
// engineer think the node does not exist, and the reason — "already a position
// of this press" — is the sentence they actually need.
//
// No native <select>, for the reason the rest of this page has none: the
// browser draws its own chrome over ours.

// The node list, once per page load, held as the PROMISE rather than as the
// rows alone — four pickers opening in one sheet share one request instead of
// racing four.
function loadCoreNodes() {
    if (!S.coreNodesReq) {
        S.coreNodesReq = fetch('/api/core-nodes')
            .then(res => (res.ok ? res.json() : []))
            .then(rows => { S.coreNodes = Array.isArray(rows) ? rows : []; })
            .catch(() => { S.coreNodes = []; });
    }
    return S.coreNodesReq;
}

// Core answers from a map, so the order it sends is that map's iteration order.
// Grouped by type and alphabetical inside it is the order a list is read in.
function coreNodeList() {
    return (S.coreNodes || []).slice().sort((a, b) =>
        String(a.node_type || '').localeCompare(String(b.node_type || '')) ||
        String(a.name || '').localeCompare(String(b.name || '')));
}

// pickerInit declares one picker before it is drawn.
//
//   selected  the names it opens with
//   exclude   name -> reason, or '' — a name that may not be picked, and why
//   annotate  name -> note, or '' — a name that MAY be picked and comes with a
//             consequence. Drawn the same way as a reason and live, because
//             the difference between "you cannot" and "you can, and here is
//             what happens" is the whole of it
//   onPick    given, the picker WRITES THROUGH: a click calls this and the
//             picker holds no selection of its own. That is Settings' three
//             routing lists, where the rows above the picker are the
//             selection. Without it the picker accumulates, which is every
//             sheet.
//   onChange  after the selection moved, for a picker whose choice changes
//             what another picker may offer.
function pickerInit(key, opts) {
    const o = opts || {};
    S.pickers[key] = {
        sel: (o.selected || []).slice(),
        q: '',
        open: false,
        exclude: o.exclude || (() => ''),
        annotate: o.annotate || (() => ''),
        onPick: o.onPick || null,
        onChange: o.onChange || null,
    };
}

function pickerValue(key) {
    const p = S.pickers[key];
    return p ? p.sel.slice() : [];
}

// pickerBox is the component's markup. THE INPUT IS NOT REDRAWN — the chips
// and the option list are their own elements, so a keystroke replaces neither
// the field under the caret nor the caret in it. Same reason markSettingsBar
// exists.
function pickerBox(key) {
    return '<div class="pd-npk" data-npkbox="' + key + '" id="npk-' + key + '">' +
        '<div class="chips" data-npkpart="chips">' + pickerChips(key) + '</div>' +
        '<input class="pd-npkq" type="text" autocomplete="off" data-npkq="' + key +
        '" placeholder="find a node by name">' +
        '<div class="opts" data-npkpart="opts">' + pickerOptions(key) + '</div></div>';
}

// The same component inside a sheet's field grid, so a picker sits under a
// label like every other control on the sheet.
function pickerField(key, label, sub) {
    return '<div class="pd-fld"><label>' + esc(label) +
        (sub ? '<small>' + esc(sub) + '</small>' : '') + '</label>' +
        '<div class="v">' + pickerBox(key) + '</div></div>';
}

function pickerChips(key) {
    const p = S.pickers[key];
    // A write-through picker has no chips: what it wrote is the list above it.
    if (!p || p.onPick) return '';
    if (!p.sel.length) return '<span class="pd-dim">nothing picked yet</span>';
    return p.sel.map(n => '<span class="pd-nchip">' + esc(n) +
        '<button data-npk="drop" data-npkkey="' + key + '" data-npkname="' + esc(n) +
        '" aria-label="remove ' + esc(n) + '">&times;</button></span>').join('');
}

function pickerOptions(key) {
    const p = S.pickers[key];
    if (!p || !p.open) return '';
    if (!S.coreNodes) return '<div class="none">Reading Core’s node list…</div>';
    const needle = p.q.trim().toLowerCase();
    const hits = coreNodeList().filter(n => !needle || String(n.name).toLowerCase().indexOf(needle) >= 0);
    if (!hits.length) {
        return '<div class="none">No node Core knows about is called that. The list is the ' +
            'fleet’s — shingo mirrors it and cannot add to it.</div>';
    }
    let out = '', group = null;
    for (const n of hits) {
        const type = n.node_type || 'other';
        if (type !== group) { group = type; out += '<div class="pd-lbl">' + esc(type) + '</div>'; }
        const why = p.exclude(n.name);
        if (why) {
            out += '<button class="off" disabled>' + esc(n.name) + '<small>' + esc(why) + '</small></button>';
            continue;
        }
        const on = p.sel.indexOf(n.name) >= 0;
        const note = p.annotate(n.name);
        out += '<button class="' + (on ? 'on' : '') + '" data-npk="add" data-npkkey="' + key +
            '" data-npkname="' + esc(n.name) + '">' + esc(n.name) +
            (note ? '<small>' + esc(note) + '</small>' : '') + '</button>';
    }
    return out;
}

function redrawPicker(key) {
    const box = $('npk-' + key);
    if (!box) return;
    const chips = box.querySelector('[data-npkpart="chips"]');
    const opts = box.querySelector('[data-npkpart="opts"]');
    if (chips) chips.innerHTML = pickerChips(key);
    if (opts) opts.innerHTML = pickerOptions(key);
}

// Every picker now in the document, bound and filled. The node list is read
// once here rather than once per picker, and a picker drawn before it lands
// says so and fills itself when it does.
function bindPickers() {
    for (const box of document.querySelectorAll('.pd-npk')) {
        const key = box.dataset.npkbox;
        const input = box.querySelector('[data-npkq]');
        if (!key || !input || !S.pickers[key]) continue;
        input.value = S.pickers[key].q;
        input.addEventListener('input', () => {
            S.pickers[key].q = input.value;
            openOnlyPicker(key);
        });
        input.addEventListener('focus', () => openOnlyPicker(key));
    }
    loadCoreNodes().then(() => {
        for (const box of document.querySelectorAll('.pd-npk')) redrawPicker(box.dataset.npkbox);
    });
}

// ONE LIST OPEN AT A TIME. The Add-process sheet carries four pickers over the
// whole plant, and four open lists is fifteen hundred buttons in one modal
// while the engineer is typing into one of them.
function openOnlyPicker(key) {
    for (const k of Object.keys(S.pickers)) {
        if (k === key || !S.pickers[k].open) continue;
        S.pickers[k].open = false;
        redrawPicker(k);
    }
    S.pickers[key].open = true;
    redrawPicker(key);
}

// The picker's clicks, from either handler: one drawn in a sheet is reached by
// onAdvClick and one drawn on a page by onClick, and this is what both call.
function onPickerClick(el) {
    const key = el.dataset.npkkey, name = el.dataset.npkname;
    const p = S.pickers[key];
    if (!p) return;
    if (el.dataset.npk === 'drop') {
        const at = p.sel.indexOf(name);
        if (at < 0) return;
        p.sel.splice(at, 1);
    } else if (p.onPick) {
        p.onPick(name);
        return;
    } else {
        // A name already picked is unpicked, which is what clicking a chosen
        // row of a multi-select means everywhere else.
        const at = p.sel.indexOf(name);
        if (at >= 0) p.sel.splice(at, 1); else p.sel.push(name);
    }
    redrawPicker(key);
    if (p.onChange) p.onChange();
}

// ── the sheets D4 and D5 open ────────────────────────────────────────────────
//
// One shell, the Advanced sheet's, because they are the same object: a small
// modal over a scrim with a header, a body of labelled fields and two buttons.
// A second modal shape would be a second set of paddings to keep in step.
// cls is an optional width class — pd-narrow (480) for a one-field modal,
// pd-wide (920) for Generate variants and Add process. The shell, the paddings
// and the footer are the same in every case, because they are the same object.
//
// #pd-advpop RIDES ON THE SHELL rather than on the sheets that want a popover.
// It is the Advanced sheet's answer to #pd-pop living inside #pd-root, which
// the scrim covers; a list opened from a sheet against the page's popover
// draws behind the modal. One per scrim, whichever sheet is open — and the
// sheets that never open one pay an empty div.
function openSheet(title, sub, body, confirm, run, danger, cls) {
    S.sheet = { run: run };
    showSheet('<div class="pd-modal ' + (cls || '') + '" role="dialog" aria-modal="true" aria-label="' + esc(title) + '">' +
        '<div class="mh"><h2>' + esc(title) + '</h2>' + (sub ? '<p>' + esc(sub) + '</p>' : '') + '</div>' +
        '<div class="mb">' + body + '</div>' +
        '<div class="mf"><span class="st"></span>' +
        '<button class="pd-btn" data-act="sheet-cancel">Cancel</button>' +
        '<button class="pd-btn ' + (danger ? 'danger' : 'primary') + '" data-act="sheet-ok">' +
        esc(confirm) + '</button></div>' +
        '<div class="pd-pop" id="pd-advpop" hidden></div></div>');
    bindPickers();
}

// sheetStatus is the sheet's one line of feedback, and every refusal on this
// page reaches the engineer through it. It was written out by hand at three
// call sites; a fourth (the Add-process chain, which reports a refusal midway
// through a sequence of writes) made that a fourth copy.
function sheetStatus(text, bad) {
    const st = $('pd-scrim').querySelector('.mf .st');
    if (!st) return;
    st.textContent = text;
    st.className = 'st' + (bad ? ' bad' : '');
}

function closeSheet() {
    hideSheet();
    S.sheet = null;
    // A PICKER'S STATE BELONGS TO THE RENDER THAT DREW IT. The scrim is empty
    // now, so every picker whose box went with it is dead state — and a dead
    // picker with a selection is what would put last time's nodes in front of
    // the engineer when this sheet is opened again. Settings' own pickers are
    // in #pd-root and stay.
    for (const k of Object.keys(S.pickers)) if (!$('npk-' + k)) delete S.pickers[k];
    // The apply modal's ticks and previews go with it: reopening it must start
    // from nothing ticked (explicit re-apply), not from what was ticked last
    // time and previewed against a flow that has since been saved.
    S.papply = null;
}

function sheetValue(name) {
    // A PICKER IS READ BY THE SAME ACCESSOR AS A TEXT FIELD. It is the sheet's
    // value for that name, and a second read path would be a second answer to
    // "what did the engineer put in this field" — which is how a sheet ends up
    // writing one thing and reporting another.
    if (S.pickers[name]) return pickerValue(name);
    const el = $('pd-scrim').querySelector('[data-f="' + name + '"]');
    if (!el) return '';
    return el.type === 'checkbox' ? el.checked : el.value;
}

function sheetField(label, sub, name, value) {
    return '<div class="pd-fld"><label>' + esc(label) + (sub ? '<small>' + esc(sub) + '</small>' : '') +
        '</label><div class="v"><input class="pd-inp wide" type="text" data-f="' + name +
        '" value="' + esc(value || '') + '"></div></div>';
}

// ONE READ OF A WRITE'S ANSWER: what came back when it landed, and the
// server's refusal BY NAME when it did not. Every write on this page wants the
// same three lines — "PLN_02 is used by PART 40421-RVJ56.37" is an answer and
// "could not save" is not — and the Add-process chain wants the answer as well,
// because step 2 is posted against the id step 1 returned.
async function postJSON(method, url, body) {
    const res = await fetch(url, {
        method: method,
        headers: { 'Content-Type': 'application/json' },
        body: body === null ? undefined : JSON.stringify(body),
    });
    let parsed = null;
    try { parsed = await res.json(); } catch (_) { /* a write may answer with nothing */ }
    if (res.ok) return { ok: true, body: parsed || {} };
    const named = parsed && (parsed.error || parsed.message);
    return { ok: false, status: res.status, error: named || ('the server refused it (' + res.status + ')') };
}

// A write that goes through a sheet, with the server's refusal shown BY NAME
// rather than swallowed into "could not save".
async function sheetSubmit(method, url, body, after) {
    const out = await postJSON(method, url, body);
    if (!out.ok) { sheetStatus(out.error, true); return false; }
    closeSheet();
    if (after) await after();
    return true;
}

// ── R4 · the style row menu ──────────────────────────────────────────────────
//
// Styles have no tab. Everything one can be is on its row: rename it, tell
// shingo what CATID the PLC calls it, copy it, say the press is already
// running it, remove it.
const STYLE_MENU = [
    ['rename', 'Rename'],
    ['catid', 'Expected CATID'],
    ['clone', 'Clone'],
    ['running', 'Mark as running'],
    ['delete', 'Delete'],
];

function openStyleMenu(btn) {
    const id = Number(btn.dataset.style || (btn.closest('[data-style]') || {}).dataset.style);
    const pop = $('pd-pop');
    if (!pop) return;
    pop.innerHTML = STYLE_MENU.map(m => '<button data-act="style-' + m[0] + '" data-style="' + id + '"' +
        (m[0] === 'delete' ? ' class="bad"' : '') + '>' + esc(m[1]) + '</button>').join('');
    pop.hidden = false;
    const r = btn.getBoundingClientRect();
    const host = pop.offsetParent ? pop.offsetParent.getBoundingClientRect() : { left: 0, top: 0, width: window.innerWidth };
    pop.style.left = Math.min(host.width - 200, Math.max(8, r.left - host.left)) + 'px';
    pop.style.top = (r.bottom - host.top + 6) + 'px';
}

function styleByID(id) { return S.styles.find(s => s.id === id) || composerStyle(id) || { id: id, name: '' }; }

// apiUpdateStyle WRITES EVERY FIELD IT DECODES: name, description, process_id
// (refused when zero) and expected_catid. A rename that sent only the name
// came back 400, and one that left out the description would have blanked it.
// So every style write starts from the style as it stands and changes only
// what the sheet asked about.
function styleBody(st, change) {
    return B().styleWrite(st, S.processID, change);
}

function styleAction(act, id) {
    const st = styleByID(id);
    closePop();
    switch (act) {
        case 'rename':
            openSheet('Rename ' + st.name, '', sheetField('Name', 'what this part is called here', 'name', st.name),
                'Rename', () => sheetSubmit('PUT', '/api/styles/' + id,
                    styleBody(st, { name: sheetValue('name') }), refreshProcess));
            return;
        case 'catid':
            openSheet('Expected CATID · ' + st.name,
                'what the PLC calls this part. A confirmed change to it is what arms a changeover cutover.',
                sheetField('Expected CATID', 'blank means shingo never matches this style to a scan', 'catid', st.expected_catid),
                'Save', () => sheetSubmit('PUT', '/api/styles/' + id,
                    styleBody(st, { expected_catid: sheetValue('catid') }), refreshProcess));
            return;
        case 'clone':
            openSheet('Clone ' + st.name, 'a new style with the same flow, ready to be renamed.',
                sheetField('Name', '', 'name', st.name + ' copy'),
                'Clone', () => sheetSubmit('POST', '/api/styles/' + id + '/clone',
                    B().styleClone(sheetValue('name')), refreshProcess));
            return;
        // R4: "Set Active" is now "Mark as running", and the sentence says
        // exactly what it does — because the old verb read like an instruction
        // to the press, and this one is an admin correction to shingo's record.
        case 'running':
            openSheet('Mark ' + st.name + ' as running?',
                'No robots move — this tells shingo what the press is already stamping.',
                '<p class="pd-note">Use this when the plant changed parts without shingo. To change ' +
                'parts WITH shingo, start a changeover from the operator screen.</p>',
                'Mark as running', () => sheetSubmit('PUT', '/api/processes/' + S.processID + '/active-style',
                    B().processActiveStyle(id), refreshProcess));
            return;
        case 'delete':
            openSheet('Delete ' + st.name + '?', 'its flow goes with it.',
                '<p class="pd-note">A style the press has run is part of its history. Deleting it removes ' +
                'the flow an operator would pick, not what already ran.</p>',
                'Delete', () => sheetSubmit('DELETE', '/api/styles/' + id, null, refreshProcess), true);
            return;
        default: return;
    }
}

// After any write that changes what a screen shows: re-read the process list
// and the composer block, then redraw the tab the engineer is on. One path, so
// no screen can be left showing what it read before the write.
async function refreshProcess() {
    await reloadProcesses();
    const res = await fetch('/api/processes/' + S.processID + '/composer');
    if (res.ok) {
        S.composer = await res.json();
        S.graph = null;
    }
    const styles = await fetch('/api/processes/' + S.processID + '/styles');
    if (styles.ok) {
        const rows = await styles.json();
        if (Array.isArray(rows)) {
            S.styles = S.styles.filter(s => s.process_id !== S.processID).concat(rows);
        }
    }
    S.settings = settingsDraft();
    if (S.tab === 'flows') selectStyle(composerStyle(S.styleID) ? S.styleID : ((S.composer.styles[0] || {}).id || 0));
    else if (S.tab === 'screens') drawScreens();
    else if (S.tab === 'presets') await refreshPresets();
    else await openSettings();
}

// ── Add process, end to end ──────────────────────────────────────────────────
//
// THE WHOLE CHAIN, IN ONE SHEET. The flow-composer wave left `add-process`
// returning without doing anything, and it was not the only half missing:
// nothing on the desktop set a station's claimed nodes either, and
// StationService.SetNodes is what mints process_nodes rows. So a process made
// any other way had no positions and no way to get them. An engineer at
// Hopkinsville could neither create a press nor give one places to put bins.
//
// THREE SECTIONS, because a press is three things at once and an engineer
// making one knows all three: what it is called, the screen an operator works
// it from, and where it may draw bins from and send them. Creation is not
// review — the routing set's own tab was built for reading backfilled rows on
// a migrated process, and this is a minute's typing.
//
// NOTHING IS WRITTEN UNTIL OK, and then the writes go in dependency order and
// stop at the first refusal. There is NO ROLLBACK: a delete on failure is a
// second write path with its own failures, and an engineer told exactly what
// exists is better off than one whose half-made press was cleaned up by a
// guess. The status line names what landed and what did not.

// The three routing pickers, by role. Keyed apart from the positions picker
// because sheetValue reads a picker by its key.
function routingPickerKey(role) { return 'rs_' + role; }

function openAddProcess() {
    S.add = { groupID: 0 };
    const positions = () => pickerValue('positions');
    pickerInit('positions', {
        onChange: () => {
            // A name that has just become a position cannot also be a routing
            // row: process_routing_nodes refuses one (ErrRoutingNodeIsPosition)
            // because a node in both halves would be offered twice. So it
            // leaves the role lists here, rather than sitting in one until the
            // server says so three writes later.
            for (const g of ROUTING_GROUPS) {
                const key = routingPickerKey(g[0]);
                const p = S.pickers[key];
                if (!p) continue;
                p.sel = p.sel.filter(n => positions().indexOf(n) < 0);
                redrawPicker(key);
            }
        },
    });
    for (const g of ROUTING_GROUPS) {
        pickerInit(routingPickerKey(g[0]), {
            // A POSITION IS EXCLUDED; THE OTHER TWO ROLES ARE NOT. A buffer is
            // legitimately both a source and a destination (domain/routing_set.go),
            // and the engineer said which role by which list they put it in —
            // so the three do not exclude each other.
            exclude: n => (positions().indexOf(n) >= 0
                ? 'a position of this press — available to every flow on it already'
                : ''),
        });
    }

    const body =
        '<div class="pd-sec"><div class="pd-lbl">The process</div></div>' +
        sheetField('Name', 'what this press is called here', 'name', '') +
        sheetField('Description', '', 'description', '') +
        '<div class="pd-fld"><label>Group<small>pure taxonomy for the list — nothing reads it</small></label>' +
        '<div class="v"><button class="pd-sel" id="pd-addgroup" data-act="add-pickgroup">' +
        'Ungrouped<i class="car"></i></button></div></div>' +

        '<div class="pd-sec"><div class="pd-lbl">The operator screen</div></div>' +
        sheetField('Screen name', 'what the screen is called on the floor', 'screen', '') +
        pickerField('positions', 'Positions',
            'the press positions this screen claims — a flow can only use a position its screen owns') +

        '<div class="pd-sec"><div class="pd-lbl">The routing set</div></div>' +
        ROUTING_GROUPS.map(g => pickerField(routingPickerKey(g[0]), g[1], g[2])).join('') +
        '<p class="pd-note">A name in one of these three lists is a place this press may route ' +
        'material through. Operators are offered these and never the plant. Fill them in and the ' +
        'flow composer opens on this press, because reviewing the set is exactly what that gate ' +
        'is waiting for; leave them empty and it stays shut until Settings says otherwise.</p>';

    openSheet('Add process', 'a press, the screen that works it, and where its bins come from and go.',
        body, 'Create', submitAddProcess, false, 'pd-wide');
}

async function submitAddProcess() {
    const name = String(sheetValue('name') || '').trim();
    if (!name) { sheetStatus('A process needs a name.', true); return; }
    const screen = String(sheetValue('screen') || '').trim();
    // A press with no HMI cannot be run by anybody, so the screen is required
    // here rather than left to be noticed on D4 later.
    if (!screen) { sheetStatus('Name the operator screen — a press with no HMI cannot be run.', true); return; }

    sheetStatus('Creating…');
    const made = await postJSON('POST', '/api/processes',
        B().processCreate({ name: name, description: sheetValue('description'), group_id: S.add.groupID }));
    if (!made.ok) { sheetStatus(made.error, true); return; }
    const processID = Number(made.body.id);

    // What already exists, so a refusal halfway through says so rather than
    // leaving the engineer to find out from the list.
    const done = ['The process was created'];
    const stop = (what, why) => sheetStatus(done.join('; ') + '. ' + what + ' was refused: ' + why, true);

    const st = await postJSON('POST', '/api/operator-stations',
        B().stationWrite(null, processID, false, { name: screen, note: '' }));
    if (!st.ok) { stop('Its operator screen', st.error); return; }
    const stationID = Number(st.body.id);
    done.push('its screen added');

    const claimed = pickerValue('positions');
    if (claimed.length) {
        const nodes = await postJSON('PUT', '/api/operator-stations/' + stationID + '/claimed-nodes',
            B().stationNodes(claimed));
        if (!nodes.ok) { stop('Its positions', nodes.error); return; }
        done.push('its positions claimed');
    }

    // ONE PICK, ONE ROW. pickOnMap posted a group as both a source and a
    // destination because a click on the map cannot say which was meant; a
    // list can, and did.
    let routed = 0;
    for (const g of ROUTING_GROUPS) {
        for (const node of pickerValue(routingPickerKey(g[0]))) {
            const row = await postJSON('POST', '/api/processes/' + processID + '/routing-nodes',
                B().routingAdd(node, g[0]));
            if (!row.ok) { stop(node + ' as a ' + g[0], row.error); return; }
            routed++;
        }
    }

    // THE GATE OPENS ON A REVIEWED ROUTING SET, and the engineer just reviewed
    // one — they wrote it. The flag exists to wait for that (domain/process.go,
    // and D5's own note says so); a process created with none stays shut, and
    // Settings turns it either way afterwards.
    if (routed) {
        done.push('its routing set written');
        const gate = await postJSON('PATCH', '/api/processes/' + processID, B().processGate(true));
        if (!gate.ok) { stop('The flow-composer gate', gate.error); return; }
    }

    closeSheet();
    await reloadProcesses();
    await reloadStations();
    if (claimed.length) await refreshStationNodes(stationID);
    await openProcess(processID);
}

// ── D4's screen sheet, D5's group picker and its danger ──────────────────────
// Both station handlers decode the WHOLE StationInput and write every field.
// A body with only a name and a note would blank the screen's code, area,
// sequence and controller, reset its device mode, and (enabled defaulting to
// false) switch a live HMI off to fix a typo in its note. So an edit starts
// from the station as it stands, which is also better than the retired page,
// whose saveStation blanked those four on every edit. A new screen takes what
// that page sent for one (ffa4ff0d): fixed_hmi, enabled.
function stationBody(st, editing, change) {
    return B().stationWrite(st, S.processID, editing, change);
}

// THE POSITIONS ARE PART OF THE SCREEN, and this sheet is where they are set.
// D4 showed them read-only and the edit sheet took a name and a note, so the
// only door onto PUT .../claimed-nodes was one nothing on this page opened —
// which is why a screen created here had nothing to work.
//
// TWO WRITES, IN ORDER, because they are two endpoints: the station row, then
// the list of nodes it claims. A refusal on the second leaves the first, and
// the status line says so rather than closing on a half-done edit.
function openScreenSheet(stationID) {
    const st = stationsOf(S.processID).find(s => s.id === Number(stationID)) || {};
    const editing = !!st.id;
    const claimedElsewhere = {};
    for (const other of stationsOf(S.processID)) {
        if (other.id === st.id) continue;
        for (const n of S.stationNodes[String(other.id)] || []) claimedElsewhere[n] = other.name;
    }
    pickerInit('positions', {
        selected: editing ? (S.stationNodes[String(st.id)] || []) : [],
        // A position claimed by a sibling screen is NOT hidden and not refused:
        // SetNodes moves it, deliberately, because one Core node has exactly one
        // process_node row per process. The engineer is told whose it is before
        // they take it.
        exclude: () => '',
        annotate: n => (claimedElsewhere[n] ? 'claimed by ' + claimedElsewhere[n] : ''),
    });
    openSheet(editing ? 'Edit ' + st.name : 'Add operator screen',
        'the HMI an operator works this process from.',
        sheetField('Name', 'what the screen is called on the floor', 'name', st.name) +
        sheetField('Note', 'anything the next engineer should know about it', 'note', st.note) +
        pickerField('positions', 'Positions',
            'the press positions this screen claims — a flow can only use a position its screen owns'),
        editing ? 'Save' : 'Add',
        async () => {
            const wrote = await postJSON(editing ? 'PUT' : 'POST',
                editing ? '/api/operator-stations/' + st.id : '/api/operator-stations',
                stationBody(st, editing, { name: sheetValue('name'), note: sheetValue('note') }));
            if (!wrote.ok) { sheetStatus(wrote.error, true); return; }
            const id = editing ? st.id : Number(wrote.body.id);
            const nodes = await postJSON('PUT', '/api/operator-stations/' + id + '/claimed-nodes',
                B().stationNodes(sheetValue('positions')));
            if (!nodes.ok) {
                sheetStatus('The screen was saved; its positions were refused: ' + nodes.error, true);
                await reloadStations();
                return;
            }
            closeSheet();
            await reloadStations();
            await refreshStationNodes(id);
            await refreshProcess();
        },
        false, 'pd-wide');
}

// ONE GROUP LIST, TWO HOSTS. Settings picks a group into its draft and the
// Add-process sheet into its own; the list, its order and the Ungrouped row in
// front of it are the same, so they are one function.
//
// THE POPOVER IS RESOLVED FROM THE BUTTON, for the reason openGenPicker's is:
// #pd-stpop lives on the Settings page, which the scrim covers, so a list
// opened from a sheet has to draw on the sheet's own popover or it draws
// behind the modal.
//
// Its options carry their handler directly rather than a data-act, like every
// other popover on this page — a second act for "the same list, written
// somewhere else" is the kind of near-duplicate this page keeps collapsing.
function openGroupPicker(btn, onPick) {
    const inSheet = !!(btn.closest && btn.closest('.pd-modal'));
    const pop = inSheet ? $('pd-advpop') : $('pd-stpop');
    if (!pop) return;
    const current = onPick ? (S.add ? S.add.groupID : 0) : S.settings.group_id;
    const opts = [{ id: 0, name: 'Ungrouped' }].concat(S.groups);
    pop.innerHTML = opts.map(g => '<button data-grpopt="' + g.id + '" class="' +
        (current === g.id ? 'on' : '') + '">' + esc(g.name) + '</button>').join('');
    pop.hidden = false;
    const r = btn.getBoundingClientRect();
    const host = pop.offsetParent ? pop.offsetParent.getBoundingClientRect() : { left: 0, top: 0 };
    pop.style.left = Math.max(8, r.left - host.left) + 'px';
    pop.style.top = (r.bottom - host.top + 6) + 'px';
    pop.querySelectorAll('[data-grpopt]').forEach(b => b.addEventListener('click', ev => {
        ev.stopPropagation();
        pop.hidden = true;
        pop.innerHTML = '';
        const id = Number(b.dataset.grpopt);
        if (onPick) { onPick(id); return; }
        S.settings.group_id = id;
        drawSettings();
    }));
}

function openDeleteProcess() {
    const p = process() || {};
    openSheet('Delete ' + p.name + '?', 'its styles, flows, routing set and screens go with it.',
        '<p class="pd-note">Type the name to confirm. Nothing about this is undoable from here.</p>' +
        sheetField('Process name', '', 'confirm', ''),
        'Delete ' + p.name, () => {
            if (sheetValue('confirm') !== p.name) {
                sheetStatus('That is not the name.', true);
                return Promise.resolve(false);
            }
            return sheetSubmit('DELETE', '/api/processes/' + S.processID, null, async () => {
                await reloadProcesses();
                drawList();
            });
        }, true);
}

// ── Generate variants (Q2, ported from the retired admin page) ──────────────
//
// Stamp out a family of styles from one base: the base's PRODUCE claims become
// the grid's columns, and each variant row is a name plus one payload per
// column. Capacity and the allowed set derive from the payload, so a variant
// is one value per produce node and nothing else. The whole batch goes in one
// POST, which the handler applies atomically.
//
// WHAT WAS PORTED AND WHAT WAS NOT. The shape, the column rule (produce
// claims, falling back to every claim when the base has none), the
// manual_swap exception — a manual-swap claim stores '' in payload_code and
// drives off the allowed set, so an override there sets the list and leaves
// the code blank — and the endpoint. NOT ported: the native <select> elements
// it was built from. Every choice here is the page's own chip picker, which is
// what the rest of this screen uses and what the style guide allows.

async function openGenerate() {
    const all = stylesOf(S.processID);
    if (!all.length) {
        S.settingsError = 'This press has no style to generate a family from yet. Add one from Flows first.';
        drawSettings();
        return;
    }
    const p = process();
    const base = (p && p.active_style_id) || all[0].id;
    S.gen = { baseID: base, cols: [], rows: [], error: '', seq: 0, catalog: [] };
    await loadGenCatalog();
    await loadGenColumns();
    addGenRow();
    drawGenerate();
}

async function loadGenCatalog() {
    try {
        const res = await fetch('/api/payload-catalog');
        const rows = res.ok ? await res.json() : [];
        S.gen.catalog = Array.isArray(rows) ? rows : [];
    } catch (_) {
        S.gen.catalog = [];
    }
}

// The base's produce claims are the columns. A base with no produce claim
// takes every claim instead — a consume-only press still has payloads worth
// stamping out, and an empty grid would say the base was unusable when it is
// only differently shaped.
async function loadGenColumns() {
    S.gen.cols = [];
    if (!S.gen.baseID) return;
    try {
        const res = await fetch('/api/styles/' + S.gen.baseID + '/node-claims');
        let claims = res.ok ? await res.json() : [];
        if (!Array.isArray(claims)) claims = [];
        const produce = claims.filter(c => c.role === 'produce');
        S.gen.cols = (produce.length ? produce : claims).map(c => ({
            node: c.core_node_name, swapMode: c.swap_mode, base: c.payload_code || '',
        }));
    } catch (e) {
        S.gen.error = 'Could not read the base flow: ' + e;
    }
}

// A row starts with every cell inheriting the base's payload, so an untouched
// cell is "the same part as the base" rather than a blank the server refuses.
function addGenRow() {
    S.gen.rows.push({
        id: 'g' + (S.gen.seq++), name: '',
        payloads: S.gen.cols.map(c => c.base),
    });
}

function drawGenerate() {
    const scrim = $('pd-scrim');
    if (!scrim || !S.gen) return;
    const styles = stylesOf(S.processID);
    const baseName = (styles.find(s => s.id === S.gen.baseID) || {}).name || 'pick one';
    const grid = S.gen.cols.length
        ? '<table class="pd-gentbl"><thead><tr><th>New style name</th>' +
        S.gen.cols.map(c => '<th>' + esc(c.node) + '</th>').join('') + '<th></th></tr></thead><tbody>' +
        S.gen.rows.map((r, i) =>
            '<tr><td><input class="pd-geninput" data-genrow="' + i + '" type="text" autocomplete="off" ' +
            'placeholder="e.g. 55544-DWC33.31" value="' + esc(r.name) + '"></td>' +
            S.gen.cols.map((c, j) => '<td>' + picker('', 'gen:' + i + ':' + j,
                esc(M().shortPart(r.payloads[j]) || 'inherit'), r.payloads[j] ? '' : 'dflt',
                r.payloads[j] || 'the base payload') + '</td>').join('') +
            '<td><button class="pd-mv" data-act="gen-drop" data-i="' + i + '" title="remove this variant">&times;</button></td></tr>').join('') +
        '</tbody></table>' +
        '<button class="pd-chip add" data-act="gen-add">+ Add variant</button>'
        : '<p class="pd-note">' + esc(baseName) + ' has no flow yet, so there is nothing to set a payload on. ' +
        'Build its flow in Flows first — a family is stamped out of a base that already works.</p>';

    scrim.innerHTML = '<div class="pd-modal pd-wide" role="dialog" aria-modal="true" aria-label="Generate variants">' +
        '<div class="mh"><h2>Generate variants</h2><p>One new style per row, stamped out of a base ' +
        'that already runs. A cell left on <b>inherit</b> keeps the base part.</p></div>' +
        '<div class="mb">' +
        '<div class="pd-fld"><label>Base style<small>its flow is the shape every variant gets</small></label>' +
        '<div class="v">' + picker('', 'gen-base', esc(baseName)) + '</div></div>' +
        '<div class="pd-sec"><div class="pd-lbl">The family</div>' + grid + '</div>' +
        (S.gen.error ? '<div class="pd-refusal">' + esc(S.gen.error) + '</div>' : '') +
        '</div>' +
        '<div class="mf"><span class="st">Nothing is written until Generate.</span>' +
        '<button class="pd-btn" data-act="gen-cancel">Cancel</button>' +
        '<button class="pd-btn primary" data-act="gen-run">Generate</button></div>' +
        '<div class="pd-pop" id="pd-advpop" hidden></div></div>';
    showSheet();
    scrim.querySelectorAll('[data-genrow]').forEach(el => {
        el.addEventListener('input', () => { S.gen.rows[Number(el.dataset.genrow)].name = el.value; });
    });
}

// The dialog's two picker kinds: the base style, and one payload cell.
//
// THE POPOVER IS THE DIALOG'S, not the page's: #pd-pop lives inside #pd-root,
// which the scrim covers, so a list opened from here would draw behind the
// modal. The Advanced sheet solved this with its own #pd-advpop and this
// reuses it — one popover per scrim, whichever sheet is open.
function genOptions(kind) {
    const parts = String(kind).split(':');
    if (parts[0] === 'gen-base') {
        return stylesOf(S.processID).map(st => ({
            label: st.name, on: st.id === S.gen.baseID,
            run: async () => {
                S.gen.baseID = st.id;
                S.gen.rows = [];
                S.gen.error = '';
                await loadGenColumns();
                addGenRow();
                drawGenerate();
            },
        }));
    }
    const i = Number(parts[1]), j = Number(parts[2]);
    const set = code => () => { S.gen.rows[i].payloads[j] = code; drawGenerate(); };
    // "inherit" is the base's payload, and it is the first option because it
    // is the default a row opens on.
    const head = [{ label: 'inherit · ' + (M().shortPart(S.gen.cols[j].base) || 'nothing'), on: !S.gen.rows[i].payloads[j], run: set('') }];
    return head.concat(S.gen.catalog.map(c => ({
        label: c.code + (c.uop_capacity ? ' · ' + c.uop_capacity + ' UOP' : ''),
        on: S.gen.rows[i].payloads[j] === c.code,
        run: set(c.code),
    })));
}

function openGenPicker(btn) {
    const opts = genOptions(btn.dataset.kind);
    const pop = $('pd-advpop');
    if (!pop) return;
    pop.innerHTML = opts.length
        ? opts.map((o, i) => '<button data-genopt="' + i + '" class="' + (o.on ? 'on' : '') + '">' + esc(o.label) + '</button>').join('')
        : '<div class="none">Nothing to choose here yet.</div>';
    pop.hidden = false;
    const host = pop.offsetParent ? pop.offsetParent.getBoundingClientRect() : { left: 0, top: 0, width: window.innerWidth };
    const r = btn.getBoundingClientRect();
    pop.style.left = Math.max(8, Math.min(host.width - pop.offsetWidth - 8, r.left - host.left)) + 'px';
    pop.style.top = (r.bottom - host.top + 6) + 'px';
    pop.querySelectorAll('[data-genopt]').forEach(b => b.addEventListener('click', () => {
        pop.hidden = true;
        pop.innerHTML = '';
        opts[Number(b.dataset.genopt)].run();
    }));
}

function closeGenerate() {
    hideSheet();
    S.gen = null;
}

// The variants, exactly as the retired page built them — including the
// manual_swap exception, which is the one rule in here that is not obvious.
function genVariants() {
    const out = [];
    for (const r of S.gen.rows) {
        const name = String(r.name || '').trim();
        if (!name) continue;                     // a blank row is not a variant
        const overrides = [];
        S.gen.cols.forEach((c, j) => {
            const code = r.payloads[j];
            if (!code) return;                   // inherit: leave this claim alone
            overrides.push({
                core_node_name: c.node,
                // manual_swap stores '' in payload_code and drives off the
                // allowed set; every other mode binds payload_code directly.
                payload_code: c.swapMode === 'manual_swap' ? '' : code,
                allowed_payload_codes: [code],
            });
        });
        out.push({ name: name, description: '', overrides: overrides });
    }
    return out;
}

async function runGenerate() {
    const variants = genVariants();
    if (!variants.length) {
        S.gen.error = 'Give at least one variant a name.';
        drawGenerate();
        return;
    }
    const res = await fetch('/api/styles/' + S.gen.baseID + '/generate', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ variants: variants }),
    });
    if (!res.ok) {
        let why = 'The server refused that (' + res.status + ').';
        try { const j = await res.json(); why = j.error || j.message || why; } catch (_) { /* status only */ }
        S.gen.error = why;
        drawGenerate();
        return;
    }
    const n = variants.length;
    closeGenerate();
    await reloadProcesses();
    S.settingsError = 'Generated ' + n + ' style' + (n === 1 ? '' : 's') + '. They are in the Flows rail, with no flow until you build one.';
    drawSettings();
}

async function syncCatalog() {
    S.settingsError = '';
    const res = await fetch('/api/payload-catalog/sync', { method: 'POST' });
    if (!res.ok) {
        S.settingsError = 'The catalog sync failed (' + res.status + '). Core may be unreachable.';
    } else {
        S.settingsError = 'Catalog synced from Core.';
    }
    drawSettings();
}

// ── D6 · Presets (U10) ───────────────────────────────────────────────────────
//
// A PRESET IS A SHAPE, NEVER A PART (SYNTH R-L1, locked). This tab names
// shapes, shows which parts run each one, and says which of them have since
// drifted away from it. Naming is an engineer's act and it is only here — the
// floor taps a card and never names one.
//
// TWO SECTIONS, AND THE SECOND ONE IS AN OFFER. `PRESETS` is what has been
// named; `FOUND IN YOUR FLOWS` is one row per distinct shape the press already
// runs that nobody has named yet, so an engineer arriving at a press with
// ninety styles is offered two rows rather than a list of ninety. It applies
// nothing, ever, and the section disappears when every shape has a name.
//
// DRIFT IS IN THE WARNING HUE AND NEVER THE ACCENT. Indigo on this page means
// "this is the thing you are working on"; amber means "this needs your
// attention". A drifted member is the second one.
//
// THERE IS NO PNG FOR THIS TAB. Its layout is the guide's grouped list table
// (P0's pattern) and its two modals are the Advanced sheet's shell, so every
// size and colour here is one the guide already describes. Anything that was
// not already described is a question in the report rather than a decision.

function presetsView() { return (S.presets && S.presets.view) || null; }

async function openPresets() {
    S.tab = 'presets';
    S.presets = S.presets || { view: null, error: '', expanded: 0 };
    if (!S.presets.view) {
        root().innerHTML = appbar() + '<div class="pd-sheet"><div class="pd-dim" style="padding:24px">Loading…</div></div>';
    }
    await loadPresets();
    drawPresets();
}

async function loadPresets() {
    S.presets = S.presets || { view: null, error: '', expanded: 0 };
    try {
        const res = await fetch('/api/processes/' + S.processID + '/presets');
        if (!res.ok) {
            let why = 'Could not read the presets for this press (' + res.status + ').';
            try { const j = await res.json(); why = j.error || why; } catch (_) { /* status only */ }
            S.presets.error = why;
            return;
        }
        S.presets.view = await res.json();
        S.presets.error = '';
    } catch (_) {
        S.presets.error = 'Could not reach this edge.';
    }
}

// The Shape column: the glyph the mode draws on every other surface, the mode
// word, and the positions in PRESS order — the order of the picture, not of
// whichever list the shape arrived in, so two presets over the same positions
// read as the same positions.
//
// DRAWN FROM THE SHAPE, ALWAYS (owner ruling R5, 2026-09-12). A preset's NAME
// is whatever the engineer typed — the suggestion is the mode word alone now —
// so the positions are never in it, and every surface that shows a preset
// draws them itself: this column, the strip card's second line
// (service/station_composer.go's presetUse), and the apply modal's title.
// THE POSITIONS COME FROM THE SERVER, IN PRESS ORDER. This sorted them by the
// picture's position index and the apply modal's title sorted them
// lexically — the same preset, two orders, one click apart — so the order is
// computed once where the process's own node sequence lives
// (domain.PresetShapeNodes) and both surfaces print it.
function shapeCell(mode, where) {
    return '<span class="pd-shape">' + gl(mode || '', 22, {}) +
        '<span class="w">' + esc(M().modeLabels()[mode] || 'mixed') + '</span>' +
        '<span class="ns">' + esc(where || '') + '</span></span>';
}

// `09‑12 · s.brown`. A preset is named once and read for months, so the row
// says the day and who, and not the hour.
function savedWord(row) {
    const at = String(row.created_at || '');
    const m = at.match(/^\d{4}-(\d{2})-(\d{2})/);
    const day = m ? m[1] + '‑' + m[2] : at.slice(0, 10);
    return (day || '—') + (row.created_by ? ' · ' + row.created_by : '');
}

// A drifted member names its FIRST differing field and counts the rest, with
// the whole list in the title. CompareFlowShape returns them in position
// order, so "the first" is the one highest up the press.
function driftedLine(m) {
    const fs = m.fields || [];
    if (!fs.length) return '<span class="pd-warn">drifted</span>';
    const more = fs.length > 1 ? ' <span class="pd-dim">+' + (fs.length - 1) + ' more</span>' : '';
    return '<span class="pd-warn">drifted · ' + esc(fs[0]) + '</span>' + more;
}

function presetRow(p) {
    const open = S.presets.expanded === p.id;
    const drifted = (p.drifted || []).length;
    const members = (p.members || []).length;
    const head = '<tr class="' + (open ? 'on' : '') + '" data-preset="' + p.id + '">' +
        '<td class="pn" title="' + esc(p.name) + '"><b>' + esc(p.name) + '</b> ' +
        '<span class="pd-vtag">v' + (p.version || 1) + '</span></td>' +
        '<td>' + shapeCell(p.mode, p.where) + '</td>' +
        '<td class="pd-dim">' + members + ' part' + (members === 1 ? '' : 's') + '</td>' +
        '<td>' + (drifted
            ? '<span class="pd-warn">' + drifted + ' drifted</span>'
            : '<span class="pd-dim">in step</span>') + '</td>' +
        '<td class="pd-dim">' + esc(savedWord(p)) + '</td>' +
        '<td class="pd-acts"><button class="pd-dimlink" data-act="preset-menu" data-preset="' +
        p.id + '">⋯</button></td></tr>';
    if (!open) return head;

    // EVERY MEMBER, drifted or not: "which parts run this shape" is the
    // question the row was clicked to answer, and an answer that listed only
    // the problems would not be one.
    const driftedBy = {};
    for (const d of p.drifted || []) driftedBy[d.style_id] = d;
    const rows = (p.members || []).map(m => {
        const d = driftedBy[m.style_id];
        return '<div class="pd-mem"><span class="nm">' + esc(m.name) + '</span><span class="sp"></span>' +
            (d ? '<span title="' + esc((d.fields || []).join(' · ')) + '">' + driftedLine(d) + '</span>'
                : '<span class="pd-dim">in step</span>') + '</div>';
    }).join('');
    return head + '<tr class="pd-memrow"><td colspan="6">' + (members ? rows
        : '<p class="pd-dim">Nothing runs this shape yet. Apply it to a part to give it members.</p>') +
        '</td></tr>';
}

function candidateRow(c) {
    const names = c.member_names || [];
    const n = (c.members || []).length;
    const sub = names.slice(0, 4).join(', ') + (names.length > 4 ? ', +' + (names.length - 4) + ' more' : '');
    // `N parts` and not `used by N parts`: the column is headed USED BY, and a
    // cell that repeats its own heading says it twice. It counts the way the
    // presets table above it counts, because two tables on one screen counting
    // the same thing differently is two things to learn.
    //
    // THE SAME SHAPE CELL AS THE PRESETS TABLE (owner ruling R5, 2026-09-12).
    // The suggested name is the mode word alone now, so a row that drew only
    // the name would offer `2‑robot swap` with nothing to say WHICH swap —
    // and two candidate shapes of one mode would be two identical rows. The
    // positions come off the shape, here as there.
    return '<tr data-cand="' + esc(c.shape_key) + '">' +
        '<td class="pn">' + shapeCell(c.mode, c.where) + '<small>' + esc(sub) + '</small></td>' +
        '<td class="pd-dim">' + n + ' part' + (n === 1 ? '' : 's') + '</td>' +
        '<td class="pd-acts"><button class="pd-dimlink" data-act="preset-name-cand" data-cand="' +
        esc(c.shape_key) + '">Name it…</button></td></tr>';
}

function drawPresets() {
    if (S.presets.error) {
        root().innerHTML = appbar() + '<div class="pd-sheet"><div class="pd-refusal">' +
            esc(S.presets.error) + '</div></div>';
        return;
    }
    const v = presetsView();
    if (!v) { root().innerHTML = appbar() + '<div class="pd-sheet"></div>'; return; }

    const presets = v.presets || [];
    const cands = v.candidates || [];
    const flows = v.styles_with_flow || 0;
    let h = '<div class="pd-sect"><h2>Presets</h2><span class="pd-lbl"><span class="cnt">' +
        presets.length + '</span></span>' +
        '<span class="pd-dim">named shapes this press can apply to a part</span></div>';
    // THE COLUMNS ARE SIZED, the way D1's positions table is, and for the same
    // reason: left to itself the browser gave Name a third of what its content
    // needs and Shape less than its position list, so the one thing an
    // engineer scans down wrapped onto two lines with `v1` orphaned on the
    // second, while `PLN_01 / PLN_04` ellipsised beside acres of empty Saved.
    const presetCols = '<colgroup><col style="width:26%"><col style="width:27%">' +
        '<col style="width:10%"><col style="width:15%"><col style="width:16%"><col style="width:6%"></colgroup>';
    h += presets.length
        ? '<table class="pd-tbl pd-presettbl">' + presetCols +
        '<thead><tr><th>Name</th><th>Shape</th><th>Used by</th>' +
        '<th>Drift</th><th>Saved</th><th></th></tr></thead><tbody>' +
        presets.map(presetRow).join('') + '</tbody></table>'
        : '<p class="pd-dim">No presets yet — name a flow from the Flows tab, or start from a shape below.</p>';

    // THE OFFER, while there is one. Once every shape the press runs has a
    // name this section is not empty, it is finished — and a heading over
    // nothing would read as a thing still to do.
    if (cands.length) {
        h += '<div class="pd-sect"><h2>Found in your flows</h2><span class="pd-lbl"><span class="cnt">' +
            cands.length + '</span></span>' +
            '<span class="pd-dim">shape' + (cands.length === 1 ? '' : 's') +
            ' this press already runs, over ' + flows + ' flow' + (flows === 1 ? '' : 's') +
            ', that nobody has named</span></div>' +
            // THREE COLUMNS, NOT FOUR. Section 1c asks for "glyph + suggested
            // name, used by N parts, Name it…", and a suggested name IS the
            // mode word and the positions — so a separate Shape column
            // printed the mode and positions once in the name and again
            // beside it. The glyph joins the name.
            '<table class="pd-tbl pd-candtbl"><colgroup><col style="width:53%">' +
            '<col style="width:10%"><col style="width:37%"></colgroup>' +
            '<thead><tr><th>Shape</th><th>Used by</th><th></th></tr></thead><tbody>' +
            cands.map(candidateRow).join('') + '</tbody></table>';
    }
    root().innerHTML = appbar() + '<div class="pd-sheet">' + h + '</div>';
}

function presetByID(id) { return (presetsView() ? presetsView().presets : []).find(p => p.id === id) || null; }
function candidateByKey(k) { return (presetsView() ? presetsView().candidates : []).find(c => c.shape_key === k) || null; }

// THREE ITEMS. Rename was absent until owner ruling R6 (2026-09-12) settled
// what it means: PATCH the name on every version of it and nothing else, so a
// lineage keeps one name and a member's source_preset_id keeps pointing at the
// same version of the same shape. Both ways of faking it were worse than the
// absence — re-creating under the new name and archiving the old row aims
// every member's provenance at an archived preset, and a version n+1 under a
// new name leaves the old name live on a different shape.
const PRESET_MENU = [
    ['apply', 'Apply to parts…'],
    ['rename', 'Rename'],
    ['archive', 'Archive'],
];

function openPresetMenu(btn) {
    const id = Number(btn.dataset.preset);
    const pop = $('pd-pop');
    if (!pop) return;
    pop.innerHTML = PRESET_MENU.map(m => '<button data-act="preset-' + m[0] + '" data-preset="' + id + '">' +
        esc(m[1]) + '</button>').join('');
    pop.hidden = false;
    const r = btn.getBoundingClientRect();
    const host = pop.offsetParent ? pop.offsetParent.getBoundingClientRect() : { left: 0, top: 0, width: window.innerWidth };
    pop.style.left = Math.min(host.width - 200, Math.max(8, r.left - host.left)) + 'px';
    pop.style.top = (r.bottom - host.top + 6) + 'px';
}

// suggestedPresetName prefills the naming modal from D1's own draft, the way
// the server prefills an offered shape's row: THE MODE WORD ALONE (owner
// ruling R5, 2026-09-12).
//
// THE SAME SHAPE OF NAME AS domain.SuggestPresetName, deliberately, so a
// preset named from a flow and one named from an offered shape sit in the same
// list looking like the same kind of thing. It is a PREFILL and nothing reads
// it back, which is why it is worth keeping in step by eye rather than pinning:
// an engineer types over it. A draft whose cells disagree on a mode gets no
// suggestion and the field opens empty.
function suggestedPresetName() {
    const cells = M().toCells(S.model);
    const modes = [...new Set(cells.map(c => c.swap_mode))];
    return modes.length === 1 ? (M().modeLabels()[modes[0]] || '') : '';
}

// ── the naming modal (480) ───────────────────────────────────────────────────
//
// Reached three ways and built once: `Name it…` on an offered shape, `Save as
// preset…` on D1's header, and Rename on a row's menu. The dim line says what
// a preset IS, in the style guide's words, because "name this" on its own
// invites an engineer to expect the parts to come along.
function openPresetNaming(title, prefill, from) {
    openSheet(title, '',
        sheetField('Name', 'what this shape is called on this press', 'name', prefill) +
        '<p class="pd-note">A preset is the shape of a flow — which positions, how they swap, ' +
        'where bins come from and go. Parts are never part of it.</p>',
        'Save preset',
        () => sheetSubmit('POST', '/api/processes/' + S.processID + '/presets',
            B().flowPresetCreate(sheetValue('name'), from), refreshPresets),
        false, 'pd-narrow');
}

async function refreshPresets() {
    await loadPresets();
    if (S.tab === 'presets') drawPresets();
}

// ── the apply modal (640) ────────────────────────────────────────────────────
//
// NEVER A SAVE WITHOUT ITS PREVIEW ON SCREEN (SYNTH R-L1). Ticking a part
// previews it and shows the diff; `Save to N parts` then saves the previews
// that are on screen, on their own fingerprints, one row at a time. There is
// no bulk endpoint and no path here that reaches flow/save without having
// drawn what it would change.
//
// NONE PRE-TICKED. An explicit re-apply is the ruling: a modal that opened
// with eight parts ticked would be one click from eight writes an engineer
// never read.
function openPresetApply(id) {
    const p = presetByID(id);
    if (!p) return;
    S.papply = { id: id, rows: {}, running: false, open: null };
    drawPresetApply();
}

function applyStyles() {
    // Every style of the process, in the rail's order. A preset applies to a
    // part, and which parts is the engineer's choice — including the ones
    // already in step, because re-applying to one of those is a no-op they can
    // see rather than a row the modal hid.
    //
    // THE RUNNING STYLE GOES LAST — see B().applyOrder, which is where that
    // rule lives and is tested.
    return B().applyOrder(S.composer.styles || [], process() ? process().active_style_id : 0);
}

// WHY A STYLE MIGHT NOT BE APPLYABLE, in the words the rest of the page uses.
function applyBlocker(st) {
    if (!((st.parts || []).length)) {
        return 'no parts placed — apply from the Flows tab';
    }
    return '';
}

// THE RUNNING STYLE IS NOT BLOCKED HERE, AND NOW THE PROMISE IS TRUE.
//
// Until owner ruling R3 (2026-09-12) SaveFlow refused the running style
// outright and this row could only ever show `process is already running
// style 7` — a sentence beside a Save button that could not succeed. The
// refusal is gone: a save to the running style writes its rows and starts
// nothing, and the runtime reads them on its next trip. So the row previews
// like any other, shows its diff, and carries the same sentence D1's header
// carries, which is now what actually happens.
//
// WHAT IT STILL CANNOT DO is move the running style off a position while its
// bin is on it, and that is refused by the engine BY NAME
// (ErrRunningPositionMove) rather than guessed at here. A preset whose shape
// names different positions is exactly that move, so the refusal lands on the
// row, in the engine's words, where the engineer can read which position is
// which.
function applyNote(st) {
    const p = process();
    return (p && p.active_style_id === st.id)
        ? 'running — a saved change applies at the next changeover' : '';
}

// orderLine is the diff's last line. A preview of the RUNNING style plans
// nothing (R3), so there is no count to print and the row says so instead of
// printing a zero it would have to invent.
function orderLine(r) {
    if (r.orders === null || r.orders === undefined) {
        return '<div class="pd-dim">orders not previewed while running</div>';
    }
    return '<div class="pd-dim">' + r.orders + ' order' + (r.orders === 1 ? '' : 's') +
        ' after the change</div>';
}

// THE ONE QUESTION A PRESET CANNOT ANSWER (owner ruling F1, 2026-09-12).
//
// A preset is a shape and carries no part, so a shape that brings in a position
// the part was not running has nobody to put on it. The modal used to preview
// that, draw `PLN_01 · which part?` in amber, and leave `Save to 1 part`
// enabled over a click that answers 422 — offering a save shingo refuses.
//
// It asks instead. `applyNeeds` is the positions the applied flow leaves with
// no part, read off the row's OWN model rather than off the preview, so the
// pickers are drawn before the round trip comes back and do not flicker.
function applyNeeds(r) {
    if (!r || !r.model) return [];
    return M().toCells(r.model).filter(c => !c.payload_code).map(c => c.core_node_name);
}

// applyLoose is the parts this row's flow currently leaves with nowhere to sit,
// read off the row's OWN model so the line follows the pickers.
//
// NOT r.unplaced, WHICH IS A DIFFERENT QUESTION AND IS CAPTURED ONCE. That one
// is what the APPLY took off, and it is the order the pickers offer — answering
// one must not shorten the list the next one is chosen from. This one is what
// is still loose, and it is what the diff line says.
function applyLoose(r) {
    if (!r || !r.model) return [];
    return (M().findings(r.model) || [])
        .filter(f => f.field === 'unplaced_part')
        .reduce((all, f) => all.concat(f.parts || []), []);
}

// applyRowReady is what `Save to N parts` counts: a previewed row with nothing
// left to answer. A row with findings is not saved — that is the whole ruling —
// and the tick stays on it, because unticking a row the engineer chose would be
// the modal deciding for them.
function applyRowReady(r) {
    return !!(r && r.ticked && !r.loading && !r.error && r.fingerprint &&
        !applyNeeds(r).length && !(r.findings || []).length);
}

// THE OFFER IS THE PART'S OWN PART NUMBERS, the ones this shape just unplaced
// first. An apply that moves PIA09/10 from PLN_03/PLN_06 onto PLN_01/PLN_04 is
// asking "which of your two parts goes here", and the two it took off are the
// answer nine times out of ten — so they lead, and the style's others follow
// for the press that runs three parts over two positions.
function applyPartOptions(r, st) {
    const out = [];
    for (const code of (r.unplaced || []).concat((st.parts || []).map(x => x.payload_code || x))) {
        if (code && out.indexOf(code) < 0) out.push(code);
    }
    return out;
}

function applyPickerHTML(st, r, node) {
    const open = S.papply.open === st.id + ':' + node;
    const chips = open
        ? '<div class="pd-apick">' + applyPartOptions(r, st).map(code =>
            '<button class="pd-chip" data-act="papply-part" data-style="' + st.id +
            '" data-node="' + esc(node) + '" data-part="' + esc(code) + '">' +
            esc(M().shortPart(code)) + '</button>').join('') + '</div>'
        : '';
    return '<div class="pd-aneed"><button class="pd-chip need" data-act="papply-pick" data-style="' +
        st.id + '" data-node="' + esc(node) + '">' + esc(node) + ' · which part? ▾</button>' +
        chips + '</div>';
}

function applyRowHTML(st) {
    const r = S.papply.rows[st.id] || {};
    const blocked = applyBlocker(st);
    const note = applyNote(st);
    const box = blocked
        ? '<span class="pd-tick off"></span>'
        : '<span class="pd-tick' + (r.ticked ? ' on' : '') + '" data-act="papply-tick" data-style="' +
        st.id + '"></span>';
    let detail = '';
    if (blocked) {
        detail = '<div class="pd-dim">' + esc(blocked) + '</div>';
    } else if (r.result) {
        detail = '<div class="' + (r.result.ok ? 'pd-dim' : 'pd-warn') + '">' + esc(r.result.text) + '</div>';
    } else if (r.ticked && r.loading) {
        detail = '<div class="pd-dim">Previewing…</div>';
    } else if (r.ticked && r.error) {
        detail = '<div class="pd-warn">' + esc(r.error) + '</div>';
    } else if (r.ticked && r.diff) {
        // R8: AN APPLY THAT LEAVES PARTS UNPLACED SAVES, and says which parts.
        // A preset carries no part, so a position the new shape does not name
        // releases whatever was on it; the row that would have said only
        // `PLN_03 · removed from the flow` now also says what came off it.
        // The row stays saveable — that is the ruling — and the part is a
        // finding on the flow, which is what stops a changeover.
        const needs = applyNeeds(r);
        const still = applyLoose(r);
        const loose = still.length
            ? '<div class="pd-warn">' + esc(still.map(M().shortPart).join(', ')) +
              ' will need a position</div>'
            : '';
        // F1: a position with no part is a QUESTION, not a warning. The
        // preview's own `PLN_01 · which part?` finding is the same fact, so it
        // is not printed twice — the picker replaces it and the rest of the
        // server's findings still show by name.
        const asked = needs.map(n => applyPickerHTML(st, r, n)).join('');
        const bad = (r.findings || []).filter(f => needs.indexOf(f.core_node_name) < 0).map(f =>
            '<div class="pd-warn">' + esc(f.core_node_name) + ' · ' + esc(M().findingShort(f)) + '</div>').join('');
        // F1, and the bar's own rule: FINDINGS REPLACE THE ORDER COUNT. A row
        // that cannot be saved must not also advertise what it would fire.
        const orders = (needs.length || bad) ? '' : orderLine(r);
        detail = r.diff.length
            ? '<div class="pd-diff">' + r.diff.map(d => '<div><b>' + esc(d.node) + '</b> · ' + esc(d.label) +
                (d.whole ? '' : ': <span class="pd-dim">' + esc(d.from) + '</span> → ' + esc(d.to)) +
                '</div>').join('') +
            '</div>' + orders + loose + asked + bad
            : '<div class="pd-dim">already this shape — nothing would change</div>' + loose + asked + bad;
    } else if (note) {
        detail = '<div class="pd-dim">' + esc(note) + '</div>';
    }
    // The note rides along with a DIFF, never with a refusal: a row that says
    // both "refused" and something about when a change lands is a row saying
    // two things that cannot both be true.
    if (r.ticked && note && r.diff && !r.error && !r.result) {
        detail += '<div class="pd-dim">' + esc(note) + '</div>';
    }
    return '<div class="pd-arow' + (blocked ? ' off' : '') + '">' + box +
        '<div class="b"><div class="nm">' + esc(st.name) + '</div>' + detail + '</div></div>';
}

function drawPresetApply() {
    const p = presetByID(S.papply.id);
    if (!p) return;
    const ticked = applyStyles().filter(st => (S.papply.rows[st.id] || {}).ticked);
    // F1: THE BUTTON COUNTS WHAT IT CAN SAVE, not what is ticked. A row whose
    // pickers are unanswered keeps its tick — the engineer chose it — and is
    // not in the count, so the button never offers a save shingo refuses.
    const ready = ticked.filter(st => applyRowReady(S.papply.rows[st.id]));
    const body = '<div class="pd-alist">' + applyStyles().map(applyRowHTML).join('') + '</div>';
    const scrim = $('pd-scrim');
    if (!scrim) return;
    S.sheet = { run: runPresetApply };
    const label = ready.length ? 'Save to ' + ready.length + ' part' + (ready.length === 1 ? '' : 's')
        : 'Save to 0 parts';
    const anyPreviewed = ready.length > 0;
    // R5: the positions come off the SHAPE and sit beside the name, because the
    // name is whatever was typed and this modal is about to write that shape
    // onto parts.
    const where = p.where || '';
    scrim.innerHTML = '<div class="pd-modal pd-apply" role="dialog" aria-modal="true" aria-label="Apply preset">' +
        '<div class="mh"><h2>Apply ' + esc(p.name) + (where ? ' (' + esc(where) + ')' : '') +
        ' v' + (p.version || 1) + ' to parts</h2>' +
        '<p>Tick a part to see what would change. Nothing is written until you save.</p></div>' +
        '<div class="mb">' + body + '</div>' +
        '<div class="mf"><span class="st">' + esc(S.papply.status || '') + '</span>' +
        '<button class="pd-btn" data-act="sheet-cancel">Cancel</button>' +
        '<button class="pd-btn primary" data-act="sheet-ok"' +
        (anyPreviewed && !S.papply.running ? '' : ' disabled') + '>' + esc(label) +
        '</button></div></div>';
    showSheet();
}

// Ticking a row PREVIEWS it. The diff is computed by the shared model from the
// style's own flow and the preset's shape; the order count and any refusal are
// the server's, from the same flow/preview the Flows tab uses.
//
// THE ROW KEEPS ITS MODEL (owner ruling F1). It used to build one, read the
// cells off it and throw it away, which was enough while the modal could only
// ever show what an apply WOULD do. It finishes the job now — a pick writes to
// this model and the row previews again — so the state the modal edits is a
// composer model like any other, and `applyPreset` then `setPart` comes out the
// same as a hand-placed cell.
async function tickApplyRow(styleID) {
    const r = S.papply.rows[styleID] || {};
    if (r.ticked) { delete S.papply.rows[styleID]; drawPresetApply(); return; }
    const st = composerStyle(styleID);
    const p = presetByID(S.papply.id);
    if (!st || !p) return;
    S.papply.rows[styleID] = { ticked: true, loading: true };
    drawPresetApply();

    const base = initModel(st);
    const preset = { cells: p.shape };
    const row = S.papply.rows[styleID];
    row.model = M().reduce(base, { type: 'applyPreset', preset: preset });
    row.diff = M().shapeDiff(base, preset);
    // R8: which of this style's parts the applied shape leaves with nowhere to
    // sit. Read off the SAME model state the cells are read off, so the line
    // on the row and the flow that would be saved cannot disagree; the model's
    // own findings() raises it as the flow-level finding on the other screens.
    // It is captured ONCE, from the apply, because it is also the order the
    // pickers offer — answering one must not shorten the list of the others.
    row.unplaced = (M().findings(row.model) || [])
        .filter(f => f.field === 'unplaced_part')
        .flatMap(f => f.parts || []);
    await previewApplyRow(styleID);
}

// pickApplyPart answers one picker and previews the row again, so the diff and
// the count on screen are the ones the save would use.
async function pickApplyPart(styleID, node, code) {
    const row = S.papply.rows[styleID];
    if (!row || !row.model) return;
    row.model = M().reduce(row.model, { type: 'setPart', node: node, payloadCode: code });
    S.papply.open = null;
    row.loading = true;
    drawPresetApply();
    await previewApplyRow(styleID);
}

// previewApplyRow is the round trip, from the row's own model. Split out of
// tickApplyRow so a pick takes the same path a tick does — two ways to preview
// one row is two ways for the cells on screen and the cells in the save to
// drift apart.
async function previewApplyRow(styleID) {
    const row = S.papply.rows[styleID];
    if (!row || !row.model) return;
    row.cells = M().toCells(row.model);
    // A PREVIEW STARTS CLEAN. A row that failed once and is previewed again
    // after a pick would otherwise keep the old refusal under a new answer.
    row.error = '';
    try {
        const res = await fetch('/api/processes/' + S.processID + '/flow/preview', {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ to_style_id: styleID, cells: row.cells }),
        });
        const j = await res.json().catch(() => ({}));
        row.loading = false;
        row.fingerprint = j.fingerprint || '';
        // THE PREVIEW'S FINDINGS BELONG ON THE ROW, not on the save that comes
        // after it. Measured on the HK fixture: applying `PLN_01 / PLN_04` to a
        // part running `PLN_03 / PLN_06` lands two cells with no part on them,
        // and the preview says so twice — `PLN_01 · Select a payload`,
        // `PLN_04 · Select a payload` — while planning two real press-index
        // swaps whose supply legs carry no payload_code at all. The modal drew
        // `2 orders after the change` over `Save to 1 part`, and the engineer
        // learned the rest from a 422 AFTER pressing it.
        //
        // A ROW WITH FINDINGS IS NOT SAVED (owner ruling F1, 2026-09-12).
        // R8's "an apply that leaves parts unplaced saves" stands — it is the
        // PART with no position that saves, and the row says so. What may not
        // happen is the modal offering a save the server refuses, so a row
        // still carrying a finding keeps its tick and stays out of the count
        // (applyRowReady), and the question it can answer it asks.
        row.findings = (j.findings || []).filter(f => f.core_node_name);
        // NULL, NOT ZERO, when the body carried no count: a preview of the
        // running style is not planned (R3) and a zero there would read as a
        // changeover that moves nothing.
        row.orders = typeof j.order_count === 'number' ? j.order_count : null;
        // A refusal is shown BY NAME on the row it belongs to, and leaves the
        // row ticked-but-unsavable rather than quietly unticking itself.
        if (!res.ok || j.error) row.error = j.error || 'The server refused this preview (' + res.status + ').';
        else if (!row.fingerprint) row.error = 'No fingerprint came back — nothing to save against.';
    } catch (_) {
        row.loading = false;
        row.error = 'Could not reach this edge.';
    }
    drawPresetApply();
}

// ONE ROW AT A TIME, each on its own preview's fingerprint, each reported on
// its own row. A 409 means the flow moved under the preview: that row is
// re-previewed and says so, and it is not saved on the old fingerprint.
async function runPresetApply() {
    const p = presetByID(S.papply.id);
    if (!p || S.papply.running) return;
    S.papply.running = true;
    S.papply.status = 'Saving…';
    drawPresetApply();

    let saved = 0, failed = 0;
    for (const st of applyStyles()) {
        const row = S.papply.rows[st.id];
        // THE SAME TEST THE BUTTON COUNTED (F1). Two answers to "can this row
        // be saved" is one of them being wrong, and the wrong one here is a
        // write the button did not promise.
        if (!applyRowReady(row)) continue;
        const res = await fetch('/api/processes/' + S.processID + '/flow/save', {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(B().flowPresetApplySave(st.id, row.cells, row.fingerprint,
                stationIDForSave(), { id: p.id, version: p.version })),
        });
        let j = await res.json().catch(() => ({}));
        // What the response means for this row — B().applyOutcome, where the
        // rule lives and is tested.
        let out = B().applyOutcome(res.status, j, 1);
        if (out.retry) {
            // Re-preview THIS row first, so the modal ends up showing a diff
            // that is true rather than one the save already disagreed with,
            // and so the retry carries the fingerprint that preview returned.
            row.result = { ok: false, text: out.text };
            drawPresetApply();
            await tickApplyRow(st.id);
            const fresh = S.papply.rows[st.id];
            if (fresh && applyRowReady(fresh)) {
                const again = await fetch('/api/processes/' + S.processID + '/flow/save', {
                    method: 'POST', headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(B().flowPresetApplySave(st.id, fresh.cells, fresh.fingerprint,
                        stationIDForSave(), { id: p.id, version: p.version })),
                });
                j = await again.json().catch(() => ({}));
                out = B().applyOutcome(again.status, j, 2);
            } else {
                out = { ok: false, text: out.text };
            }
        }
        const target = S.papply.rows[st.id] || row;
        target.result = { ok: !!out.ok, text: out.text };
        if (out.ok) { saved++; } else { failed++; }
        drawPresetApply();
    }
    S.papply.running = false;
    S.papply.status = saved + ' saved' + (failed ? ', ' + failed + ' to look at' : '');
    drawPresetApply();
    // The tab's drift figures are computed from truth, so they are only true
    // after a re-read.
    await refreshPresets();
    if (S.papply) drawPresetApply();
}

// ── events ───────────────────────────────────────────────────────────────────
function onClick(e) {
    const openRow = e.target.closest && e.target.closest('[data-open]');
    const tab = e.target.closest && e.target.closest('[data-tab]');
    const btn = e.target.closest && e.target.closest('[data-act]');
    const styleRowEl = e.target.closest && e.target.closest('[data-style]');
    const card = e.target.closest && e.target.closest('#pd-svg [data-pos]');
    const row = e.target.closest && e.target.closest('[data-row]');

    const npk = e.target.closest && e.target.closest('[data-npk]');
    if (npk) { onPickerClick(npk); return; }
    if (btn && btn.dataset.act === 'pick') { e.stopPropagation(); openPicker(btn); return; }
    if (btn) {
        switch (btn.dataset.act) {
            case 'list': drawList(); return;
            case 'save': saveFlow(); return;
            case 'discard': selectStyle(S.styleID); return;
            case 'advanced': openAdvanced(btn.dataset.node); return;
            // The route's own two verbs. Both compute the whole new list and
            // hand it to setVia, because the order is the route and the model
            // has one action for it.
            case 'via-add': {
                const route = (S.model.cells[btn.dataset.node].keyRoute || []).slice();
                const taken = new Set(route);
                // The model's walk, like the picker beside it. This filtered
                // the routing set for a role the schema forbids, so `+ add`
                // never added anything.
                const next = M().viaWaypoints(S.model, btn.dataset.node).find(w => !taken.has(w)) || '';
                if (next) apply({ type: 'setVia', node: btn.dataset.node, via: route.concat([next]) });
                return;
            }
            case 'via-move': {
                const route = (S.model.cells[btn.dataset.node].keyRoute || []).slice();
                const i = Number(btn.dataset.i), j = i + Number(btn.dataset.d);
                if (i < 0 || j < 0 || i >= route.length || j >= route.length) return;
                const tmp = route[i]; route[i] = route[j]; route[j] = tmp;
                apply({ type: 'setVia', node: btn.dataset.node, via: route });
                return;
            }
            case 'rs-toggle': {
                const row = (S.routing || []).find(r => String(r.id) === btn.dataset.row);
                if (row) routingPatch(row.id, B().routingEnable(!row.enabled));
                return;
            }
            case 'st-toggle':
                S.settings[btn.dataset.st] = !S.settings[btn.dataset.st];
                drawSettings();
                return;
            case 'st-arm':
                S.settings.changeover_auto_arm = btn.dataset.arm;
                drawSettings();
                return;
            case 'st-group': openGroupPicker(btn); return;
            case 'st-discard': S.settings = settingsDraft(); S.settingsError = ''; drawSettings(); return;
            case 'st-save': saveSettings(); return;
            case 'st-generate': openGenerate(); return;
            case 'st-sync': syncCatalog(); return;
            case 'st-delete': openDeleteProcess(); return;
            case 'screen-add': openScreenSheet(0); return;
            case 'screen-edit': openScreenSheet(btn.dataset.station); return;
            case 'style-menu': openStyleMenu(btn); return;
            // D6's own actions.
            case 'preset-menu': openPresetMenu(btn); return;
            case 'preset-apply': closePop(); openPresetApply(Number(btn.dataset.preset)); return;
            case 'preset-archive': {
                closePop();
                const p = presetByID(Number(btn.dataset.preset));
                if (!p) return;
                openSheet('Archive ' + p.name + ' v' + (p.version || 1) + '?',
                    'it stops being offered; the parts that came from it keep saying so.',
                    '<p class="pd-note">Archiving hides this version from both screens. The parts ' +
                    'that were applied from it keep their provenance, because a claim points at it ' +
                    'and that reference has to keep meaning what it meant.</p>',
                    'Archive', () => sheetSubmit('POST', '/api/processes/' + S.processID +
                        '/presets/' + p.id + '/archive', null, refreshPresets));
                return;
            }
            case 'preset-rename': {
                closePop();
                const p = presetByID(Number(btn.dataset.preset));
                if (!p) return;
                // EVERY VERSION, and the modal says so: an engineer renaming
                // v2 has to know v1 comes with it, because a lineage split
                // across two names is two presets that mean one shape.
                openSheet('Rename ' + p.name,
                    'every version of this name is renamed; nothing else changes.',
                    sheetField('Name', 'what this shape is called on this press', 'name', p.name) +
                    '<p class="pd-note">A preset is named once and read for months. The shape, its ' +
                    'versions and the parts that came from it are untouched — a member points at a ' +
                    'version by id, and the id does not move.</p>',
                    'Rename',
                    () => sheetSubmit('PATCH', '/api/processes/' + S.processID + '/presets/' + p.id,
                        { name: sheetValue('name') }, refreshPresets),
                    false, 'pd-narrow');
                return;
            }
            case 'preset-name-cand': {
                const c = candidateByKey(btn.dataset.cand);
                if (c) openPresetNaming('Name this shape', c.suggested_name, { shapeKey: c.shape_key });
                return;
            }
            // D1's fourth header action. The flow is saved (the button is
            // disabled otherwise), so the shape named is the one the station
            // runs.
            case 'save-preset': {
                const st = composerStyle(S.styleID);
                if (st) openPresetNaming('Save ' + st.name + '’s flow as a preset',
                    suggestedPresetName(), { styleID: st.id });
                return;
            }
            case 'style-rename': case 'style-catid': case 'style-clone':
            case 'style-running': case 'style-delete':
                styleAction(btn.dataset.act.slice('style-'.length), Number(btn.dataset.style));
                return;
            case 'sheet-cancel': closeSheet(); return;
            case 'sheet-ok': if (S.sheet && S.sheet.run) S.sheet.run(); return;
            case 'rs-zoom': {
                const z = btn.dataset.z;
                S.mapZoom = z === 'in' ? Math.min(4, S.mapZoom * 1.4)
                    : z === 'out' ? Math.max(0.4, S.mapZoom / 1.4) : 1;
                drawSettings();
                return;
            }
            case 'add-process': openAddProcess(); return;
            case 'add-position': case 'copy-to': case 'new-style':
            case 'add-group':
                return;   // U10
            default: break;
        }
    }
    const mapEl = e.target.closest && e.target.closest('[data-mapname]');
    if (mapEl && S.tab === 'settings') { flashRoutingRow(mapEl.dataset.mapname); return; }
    if (tab) {
        S.tab = tab.dataset.tab;
        if (S.tab === 'flows') drawFlows();
        else if (S.tab === 'screens') drawScreens();
        else if (S.tab === 'presets') openPresets();
        else if (S.tab === 'settings') openSettings();
        return;
    }
    // A preset row expands its member list; a second click closes it.
    const presetRowEl = e.target.closest && e.target.closest('[data-preset]');
    if (presetRowEl && !btn && S.tab === 'presets') {
        const id = Number(presetRowEl.dataset.preset);
        S.presets.expanded = S.presets.expanded === id ? 0 : id;
        drawPresets();
        return;
    }
    if (openRow) { openProcess(Number(openRow.dataset.open)); return; }
    if (styleRowEl && !btn) { selectStyle(Number(styleRowEl.dataset.style)); return; }
    // SPEC §3: selecting a card selects its row and vice versa.
    if (card) { S.selected = card.dataset.pos; drawFlows(); return; }
    if (row) { S.selected = row.dataset.row; drawFlows(); return; }
}

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
else boot();

export { boot };
