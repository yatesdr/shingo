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
// THE DESKTOP VERB IS SAVE (SPEC §0.5). The flow editor never calls
// changeover/start; the one start on this page is the health strip's, the
// changeover a wrong part offers (catidStartChangeover).

import { esc } from '/static/shared/esc.js';
import { formatClock, formatTime } from '/static/shared/clock.js';
// THE ONE OVERLAY STACK (style guide §10). Every sheet, popover and question
// this page puts up is an entry on it — see pageSheet, showSheet and showPop.
import { openLayer, closeLayer, layerDepth, openSheet, showModal, confirm as layerConfirm } from '/static/shared/layer.js';
// THE ONE HTTP DOOR (style guide §17). Every request this page sends goes
// through send: it never throws, it answers {ok, status, body, error,
// fieldErrors} with status 0 when nothing answered and 401 for a lapsed
// session (ruling 3), and it puts a failure on the notice once. The page
// still says a refusal where it is about (a sheet's status line, the bar).
//
// WHICH CALLS KEEP THE NOTICE. Every write, and every read whose refusal
// the page draws itself (a sheet's status line, the bar, an apply row, the
// Settings and Recipes refusal lines), passes NO_NOTICE: the refusal is said
// once, where it is about, and the stale 409 the apply loop retries is not
// put up as a failure. A 401 goes on the notice whatever the call said (only
// the notice has Sign in), and the page draws nothing more for it (said).
// The ten-second re-reads pass NO_NOTICE too: a dead Edge is the
// lost-connection banner's to say (brief S5), not a toast per tick. A read
// the page draws no reason for keeps the notice, which is then its one place.
//
// `cache: 'no-store'` went with the bare fetches: the Edge's JSON answers
// carry no validator (writeJSON sets none), so the browser never held one.
import { send } from '/static/shared/http.js';
import { toast } from '/static/shared/notice.js';
// THE PICKERS ARE THE SHARED ADAPTERS' (style guide §34): every node and part
// picker on this page draws from the page's one node list and one part list,
// each field with its own rule (pickerInit, drawPicker).
import { mountPicker, nodePicker, nodeRows, nodeSource } from '/static/shared/node-picker.js';
import { partPicker, partSource } from '/static/shared/part-picker.js';
import { createSetPick } from '/static/shared/set-pick.js';

// THE COMPOSER STACK IS A PROCESS'S, NOT THE LIST'S (slice 5): the flow
// picture, Settings' page shell and its exception lists, and the classic
// scripts and stylesheets in processes.html's <template>s. loadStack brings
// them once, before the first openProcess, and they stay; the list's cold
// load is 12 requests fewer. These names are bound when it lands.
let renderFlowPicture, pictureRows, relationPlacement, sentencesFromModel, moduleOf;
// sceneRoute is the key-route walk's route model (routeOver); it waits with the
// stack because only a style opened needs it, and the list's cold load stays
// without shared/scene-net.js and the geometry under it.
let exceptionList, countOf, settingsPage, sceneRoute;
let stackLoad = null;

// loadStack puts a copy of each tag in the page's stack templates where its
// template stands (the cascade and the script order the tags had in the
// page; a classic script inserted by script runs in insertion order only with
// async off), and imports the modules beside them: none of those reads the
// classic scripts' globals as it evaluates. A failure stays until a reload:
// asking again would run the classic scripts that did load a second time,
// and a second run of a top-level const throws.
function loadStack() {
    if (stackLoad) return stackLoad;
    const tags = [];
    for (const id of ['pd-stack-before', 'pd-stack']) {
        const t = $(id);
        if (!t || !t.content) continue;
        for (const src of Array.from(t.content.children)) {
            const tag = document.createElement(src.tagName.toLowerCase());
            for (const a of Array.from(src.attributes)) tag.setAttribute(a.name, a.value);
            if (tag.tagName === 'SCRIPT') tag.async = false;
            tags.push(new Promise((ok, bad) => {
                tag.addEventListener('load', ok);
                tag.addEventListener('error', () => bad(new Error((src.getAttribute('src') || src.getAttribute('href')) + ' did not load')));
            }));
            t.before(tag);
        }
    }
    stackLoad = Promise.all([
        Promise.all(tags),
        import('/static/operator-station/operator-flow.js'),
        import('/static/shared/exceptions.js'),
        import('/static/shared/settings.js'),
        import('/static/shared/scene-net.js'),
    ]).then(([, flow, exc, set, net]) => {
        ({ renderFlowPicture, pictureRows, relationPlacement, sentencesFromModel, moduleOf } = flow);
        ({ exceptionList, countOf } = exc);
        ({ settingsPage } = set);
        ({ sceneRoute } = net);
    });
    return stackLoad;
}
const NO_NOTICE = { notice: false };

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
    heldMap: null,       // the plant map last read, quoted back on the next read (composerURL)
    styleID: 0,          // the style being edited
    model: null,         // composer-model state (the draft)
    baseline: '',        // toCells JSON at load — what "dirty" is measured against
    previewTimer: null,
    previewAbort: null,
    selected: null,      // the selected position, shared by the picture and the table
    picRows: {},         // node -> 'front' | 'back' | '', from pictureRows (never claim kind)
    openAs: '',          // the one dim cell currently drawn as a chip, because it was clicked
    setAdds: {},         // Set for this style's rows added this visit, not given their value yet
    showFree: false,     // the picture and the POSITIONS line draw the free positions too
    routing: null,       // D3's rows — every one, the disabled backfills included
    routingError: '',    // the server's refusal, by name
    routingWarning: '',  // what the last removal orphans, by name
    graph: null,         // the travel graph, built once per map
    settings: null,      // D5's draft; nothing is written until Save settings
    settingsError: '',
    settingsNotice: '',   // a success, which is not a refusal and is not drawn as one
    notice: '',          // the same, on the Flows and Presets tabs: what the last write saved
    flowSaved: null,     // {styleID, at}: the last Save style, which the bar names until the next edit
    sheet: null,         // the open sheet: {handle} (its layer entry), and {run} for the apply modal's own OK
    menuFrom: null,      // the control the open popover menu hangs off: a sheet a row opens returns focus there
    presets: null,       // D6's read: {view, error, expanded}
    papply: null,        // the apply modal: {id, rows:{styleID:{ticked,diff,cells,fingerprint,…}}, running}
    coreNodes: null,     // Core's node list as the Edge sent it (NODES' rows), null until read
    pickers: {},         // every open node picker, by key — see pickerInit
    setPicks: {},        // every set pick drawn, by key: {pid, api} — see mountSetPick
    looseOnto: '',       // Settings' draft puts the positions on no screen onto this screen (its id)
    steady: null,        // the last preview's steady state (flow/preview?steps=1): {styleId, cells}
    stSet: null,         // Settings' set picker's place: {pid, station, warnedFor}
    stShell: null,       // Settings' shell for one visit: {pid, top, sheet, body, page} (settingsShell)
    stPickersPid: 0,     // the process Settings' pickers were declared for (settingsPickers)
    posWarned: null,     // the screens' lists a removal warning was said for: Save again takes them off
    saveNotes: [],       // what a save's parts had to say (kept positions, routing removals)
    livePositions: null, // every live position on this Edge, every process's — see loadLivePositions
    add: null,           // the process sheet's own non-text draft: {groupID, edit, station, before}
    partPop: null,       // the open part popover: {node, q, note, catalogFailed}
    partSet: null,       // the STORED half of the open process's part set; null = not read
    newStyle: null,      // the New style sheet's Spots step: {recipe, moves, sel} — see drawNewSpots
    fromSetup: false,    // the style on screen was opened by Setup's First style step: its Save goes back there
    setupView: '',       // Setup's step on screen: '' (the three steps) or 'style' (First style)
    firstStyle: null,    // the First style step's choices: {styleID, parts:{node:code}, role, mode, each, modes, filled}
    setupEnd: 0,         // the process whose Setup just finished on screen: the tab stays until it is left
    setupPoll: null,     // Run it's re-read of the screens while it waits for one to be opened
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
    // A RECIPE GROUP'S CARD IS HOVERED, NOT CLICKED, to see it on the picture
    // (the mock's tab 1), and its name is typed. Both bubble to the root like
    // the clicks do; neither redraws the tab, because a redraw under the
    // pointer takes an open popover with it.
    root().addEventListener('mouseover', e => onGroupHover(e, true));
    root().addEventListener('mouseout', e => onGroupHover(e, false));
    root().addEventListener('input', onGroupName);
    // Set for this style's numbers, written as they are typed (onSetNumber).
    root().addEventListener('input', onSetNumber);
    // The picture is drawn 1:1 into the column it has, so a resize is a
    // re-layout and not a re-scale. Only the picture is redrawn.
    window.addEventListener('resize', () => { if (S.model && S.tab === 'flows') drawPicture(); });
    const scrim = $('pd-scrim');
    if (scrim) scrim.addEventListener('click', onScrimClick);
    // The page's own Escape, for when nothing is open: while anything is,
    // Escape is the layer's (it listens on the document after this one, so
    // this sees the stack before the layer's close).
    document.addEventListener('keydown', e => {
        if (e.key !== 'Escape' || layerDepth()) return;
        onEscape();
    });
    document.addEventListener('click', e => {
        if (!e.target.closest || !e.target.closest('.pd-pop')) closePop();
    });
    // Scroll does not bubble, so the headings' sideways sync listens in the
    // capture phase on the document: it survives every redraw of the table.
    document.addEventListener('scroll', syncPosHead, true);
    window.addEventListener('hashchange', onHashChange);
    window.addEventListener('focus', () => { void refreshRail(); });
    document.addEventListener('visibilitychange', () => { if (!document.hidden) void refreshRail(); });
    setInterval(() => {
        void refreshRail();
        if (!S.processID) void refreshCatidHealth();
    }, 10000);
    if (S.processID) openProcess(S.processID);
    else {
        // #add=1 opens Add process over the list: a one-shot for the shots
        // harness, which cannot press the button (see syncHash). Read before
        // drawList, whose syncHash rewrites the address bar.
        const add = 'add' in hashOpts();
        drawList();
        if (add) openAddProcess();
    }
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
    syncHash();
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
        // THE STATE IS DERIVED, NOT ANNOUNCED. production_state is a store
        // column the page only ever read to split Running/Idle, and both of
        // those are already derivable from the style pointers the list row
        // carries: a target set and not yet active is the changeover, the
        // active style is the running part, and neither means anything is
        // being made. The word says what is true; the ok tone marks only a
        // part being made, so a changeover stays untoned (it is neither a
        // success nor an alert). ONE STATE COLUMN (F19): a Running column
        // beside it said the running style's name a second time. The style
        // name is typed by an engineer, so it is escaped; the row is one line
        // (.pd-proctbl), and the column is as wide as its longest state (see
        // cols below).
        const changing = !!(p.target_style_id && p.target_style_id !== p.active_style_id);
        const running = !changing && !!run;
        const state = changing ? 'Changing over'
            : (run ? 'Running <span class="pd-statename" title="' + esc(run.name) + '">' + esc(run.name) + '</span>'
                : 'No part running');
        // EVERY STYLE IS SEARCHABLE, not only the running one: the cells show
        // one style name at most, so a part that is not running could never
        // be found by name. The names are already on the page. So is every
        // screen's name, which the row no longer prints under the process's
        // (one line per row, F19): the search box still finds a station.
        const hay = stylesOf(p.id).map(s => s.name).concat(st.map(s => s.name)).join(' · ');
        return '<tr data-open="' + p.id + '" data-hay="' + esc(hay) + '">' +
            '<td class="pn"><b>' + esc(p.name) + '</b><span id="pd-h-' + p.id + '">' + healthCount(p.id) + '</span></td>' +
            '<td>' + esc(groupName(p.group_id) || '—') + '</td>' +
            '<td class="num">' + stylesOf(p.id).length + '</td>' +
            '<td>' + (p.flow_composer_enabled ? '<span class="pd-tag ok">on</span>' : '<span class="pd-dim">off</span>') + '</td>' +
            // WIRED BUT SWITCHED OFF IS "off", as the Setup tab and Settings
            // say it; "not wired" is a counter with no PLC and tag.
            '<td><span class="pd-cnt' + (counting ? ' ok' : '') + '"><i></i>' +
            esc(counting ? p.counter_plc_name + ' · counting'
                : p.counter_plc_name && p.counter_tag_name ? 'off · ' + p.counter_plc_name : 'not wired') + '</span></td>' +
            // THE CURTAIN COLUMN: the interlock's state at a glance, the
            // flow-composer column's on/off shape. The interlock is set per
            // node, so the chip counts the curtained nodes and its title names
            // them; the pointers and polarity are in Settings, per node.
            '<td>' + ((p.curtain_nodes || []).length
                ? '<span class="pd-tag ok" title="' + esc(p.curtain_nodes.join(' \u00B7 ')) + '">curtain \u00D7 ' +
                    p.curtain_nodes.length + '</span>'
                : '<span class="pd-dim">off</span>') + '</td>' +
            '<td class="num">' + st.length + '</td>' +
            '<td><span class="pd-state' + (running ? ' running' : '') + '">' + state + '</span></td>' +
            // A ROW THAT OPENS SOMETHING SAYS SO. The whole row has been
            // clickable since P0 was written and nothing on screen said it,
            // so "how do I get to a process" was a fair question with no
            // answer visible on the page that lists them. The two doors are
            // the two things an engineer comes here to do — work a flow, or
            // change what the process is — and each fact has exactly one
            // editor: a process's name and group on Settings › Line, its
            // positions under Screens and positions, its routing set and part
            // set beside them.
            '<td class="pd-acts">' +
            '<button class="pd-dimlink" data-act="open-flows" data-process="' + p.id + '">Styles</button>' +
            '<button class="pd-dimlink" data-act="open-settings" data-process="' + p.id + '">Settings</button>' +
            '</td></tr>';
    };

    // EVERY GROUP'S STATE COLUMN IS ONE WIDTH. Each group is its own table,
    // and a content-sized column would be wider in the group that runs the
    // long name and narrower in the next, so the columns would not line up
    // down the page. Each State heading carries every running name, unseen
    // and zero-high, so every table's column is sized by the longest one.
    const sizer = '<span class="pd-statesizer" aria-hidden="true">' +
        [...new Set(S.processes.map(runningStyle).filter(Boolean).map(s => s.name))]
            .map(n => '<span>Running ' + esc(n) + '</span>').join('') + '</span>';
    const head = '<thead><tr><th>Process</th><th>Group</th><th class="num">Styles</th>' +
        '<th>HMI setup</th><th>Counter</th><th>Curtain</th><th class="num">Screens</th><th>State' + sizer + '</th>' +
        '<th></th></tr></thead>';
    // THE STATE COLUMN HAS NO WIDTH OF ITS OWN. Its name is whole and on one
    // line, so the column has to grow to it; under a fixed layout with an 8%
    // <col> it never did, and a LOADER-*-RUN name ran into the actions column.
    // The table lays out by content (.pd-proctbl), the other columns keep
    // their shares as hints and give way, and the state takes the rest (8%,
    // what "No part running" needs) or its longest name, whichever is more.
    // The shares sum to 85 so that rest exists (the Running column's 19%
    // went to the process name, the counter and the state).
    const cols = '<colgroup><col style="width:20%"><col style="width:9%"><col style="width:6%">' +
        '<col style="width:8%"><col style="width:15%"><col style="width:9%"><col style="width:6%"><col>' +
        '<col style="width:12%"></colgroup>';
    // P0 is a table and gets the page, with the same 24 px gutter every other
    // block on this page has.


    let body = '';
    for (const n of names) {
        const rows = groups.get(n);
        body += '<div class="pd-lgrp"><span class="pd-lbl">' + esc(n) + '</span>' +
            '<span class="pd-dim">' + rows.length + ' process' + (rows.length === 1 ? '' : 'es') + '</span></div>' +
            '<table class="pd-tbl pd-proctbl">' + cols + head + '<tbody>' + rows.map(row).join('') + '</tbody></table>';
    }

    root().innerHTML =
        '<div class="pd-app"><div class="pd-crumb"><b>Processes</b></div><div class="pd-spacer"></div>' +
        '<div class="pd-gate">' + searchBox('pd-q', 'find a process, station or part') +
        '<button class="pd-btn" data-act="add-group">Add group</button>' +
        '<button class="pd-btn primary" data-act="add-process">Add process</button></div></div>' +
        '<div class="pd-list" id="pd-list">' + body + '</div>';
    const q = $('pd-q');
    if (q) q.addEventListener('input', () => filterList(q.value));
    refreshCatidHealth();
}

// THE LIST CARRIES ONE COUNT PER PROCESS, NOT A STRIP (S4: the strip buried
// the list at a plant). The count is the process's red and amber items, toned
// by its worst (catid-health.js countHTML); an info item adds nothing. The
// summary carries the counts, so a refresh of the list is ONE request
// (/api/catid-health) and no process's items are read until it is opened: they
// and their verbs are on its Settings tab (healthSection). Advisory: a failed
// read leaves the counts as they were. Refetched on the page's 10 s refresh
// while the list is showing. On a process's Settings tab it re-reads that
// process's items instead (the health rows, and the slots row's words), so a
// verb run from there redraws there.
async function refreshCatidHealth() {
    if (S.processID && S.tab === 'settings') {
        await loadCatidItems();
        drawSettings();
        return;
    }
    if (S.processID) return;
    const res = await send('GET', '/api/catid-health', undefined, NO_NOTICE);
    if (!res.ok) return;
    S.catidSummary = Array.isArray(res.body) ? res.body : [];
    for (const p of S.processes) {
        const box = $('pd-h-' + p.id);
        if (box) box.innerHTML = healthCount(p.id);
    }
}

// healthCount is one list row's count, from the last summary read ('' before
// the first, and for a process with no red or amber item).
function healthCount(pid) {
    const h = window.CatidHealth;
    const row = (S.catidSummary || []).find(r => r.process_id === pid);
    return h && row ? h.countHTML(row) : '';
}

// The strip's verbs. Each asks first, in the page's one sheet, and the sheet
// shows the Edge's refusal (a CATID another style runs, a cart of another
// part) where it says it.
function catidPin(pid, styleID, catid, name) {
    pageSheet('Pin CATID ' + catid + ' to ' + (name || 'style ' + styleID) + '?',
        'the style\'s CATIDs become its set plus this one.', '',
        'Pin', () => sheetSubmit('POST', '/api/processes/' + pid + '/styles/' + styleID + '/catid',
            { catid: catid, evidence: 'the health strip' }, refreshCatidHealth));
}

// catidRepoint is the slot item's click: the process counts on the slot the
// PLC counts on, through the counter edit (refused while a changeover is
// open); its part identity follows to that slot's CATID.
// openCatidPinPick is the strip's "Pin to a style…": the shared picker over
// the styles the item carries; a pick asks, as Pin to <suggestion> does.
function openCatidPinPick(btn) {
    const pop = $('pd-stpop');
    if (!pop) return;
    let styles = [];
    try { styles = JSON.parse(btn.dataset.styles || '[]'); } catch (_) { styles = []; }
    openRowPicker(pop, btn, styles.map(s => ({ value: String(s.id), label: s.name })), v => {
        const st = styles.find(s => String(s.id) === v);
        catidPin(btn.dataset.pid, v, btn.dataset.catid, st ? st.name : '');
    }, { head: 'Pin CATID ' + esc(btn.dataset.catid) + ' to', placeholder: 'find a style' });
}

function catidRepoint(pid, tag) {
    pageSheet('Count ' + tag + '?',
        'the process counts on this slot from its next read, and its part identity follows to that slot’s CATID.', '',
        'Count', () => sheetSubmit('POST', '/api/processes/' + pid + '/counter', { tag_name: tag }, afterRepoint));
}

// afterRepoint redraws where the re-point was clicked. On the Settings tab
// the process is read again and the draft's counter tag follows the save, so a
// later Save settings does not send the old tag back; anything else in the
// draft is kept.
async function afterRepoint() {
    if (!(S.processID && S.tab === 'settings')) return refreshCatidHealth();
    const was = (process() || {}).counter_tag_name;
    await reloadProcesses();
    if (S.settings && S.settings.counter_tag_name === was) S.settings.counter_tag_name = (process() || {}).counter_tag_name || '';
    await loadCountingSlots();
    await refreshCatidHealth();
}

async function catidCharge(holdID) {
    const charge = '/api/catid-holds/' + holdID + '/charge';
    const res = await send('POST', charge + '?mode=preview', null, NO_NOTICE);
    const p = res.ok ? res.body : null;
    // A preview that did not answer is said, not swallowed: the button
    // otherwise looked dead.
    if (!res.ok || !p || !p.hold) {
        const why = res.ok ? 'it named no hold' : said(res) || 'Signed out';
        pageSheet('Charge this hold?', 'its preview could not be read: ' + why + '. Nothing was charged.', '',
            'Close', closeSheet);
        return;
    }
    const rows = (p.rows || []).map(r => '<li>' + esc(r.node) + ': ' + esc(r.qty) + ' from ' +
        (r.bin_id ? 'cart ' + esc(r.bin_id) + ' (' + esc(r.carrier_part || 'part unknown') + ')' : 'no cart: its share stays held') +
        (r.wrong_part ? ' <b>(another part: its share stays held)</b>' : '') + '</li>').join('');
    pageSheet('Charge ' + p.hold.qty + ' on CATID ' + p.hold.catid + '?',
        p.refusal ? p.refusal : 'charged under ' + p.style_name + ' to the carts below.',
        '<ul class="pd-note">' + rows + '</ul>',
        'Charge', () => sheetSubmit('POST', charge + '?mode=charge', {}, refreshCatidHealth));
}

// catidStartChangeover is the wrong-part item's one tap: the PLC changed parts
// with no changeover open, and the strip offers the changeover to the style
// that runs the part. The tap starts it through the start verb, as the
// changeover page does; the changeover orders its legs and the CATID monitor
// completes it. A refusal is shown in the sheet, by name.
async function catidStartChangeover(pid, styleID, name) {
    const out = await send('POST', '/api/processes/' + pid + '/changeover/start',
        { to_style_id: Number(styleID), called_by: 'engineer', notes: 'from the health strip: the PLC is on its part' }, NO_NOTICE);
    if (!out.ok) {
        pageSheet('The changeover to ' + (name || 'style ' + styleID) + ' did not start', said(out), '', 'Close', closeSheet);
        return;
    }
    await refreshCatidHealth();
}

function catidWriteOff(holdID) {
    pageSheet('Write off this hold?', 'nothing is charged; the hold is closed and logged.', '',
        'Write off', () => sheetSubmit('POST', '/api/catid-holds/' + holdID + '/write-off', {}, refreshCatidHealth), true);
}

function filterList(text) {
    const needle = String(text || '').trim().toLowerCase();
    // THE HEADINGS GO WITH THEIR ROWS. Only the <tr>s were hidden, so a group
    // whose every process was filtered out still drew its name and its
    // pre-filter count — "Presses · 6 processes" over nothing.
    for (const tbl of root().querySelectorAll('.pd-tbl')) {
        const grp = tbl.previousElementSibling;
        if (grp && grp.classList.contains('pd-lgrp')) grp.dataset.forTable = '1';
    }
    for (const tr of root().querySelectorAll('.pd-tbl tbody tr')) {
        // THE ACTIONS CELL IS NOT SEARCHABLE TEXT. Every row carries the words
        // "Flows" and "Settings" now, and a row's own buttons matching the
        // search box would mean typing either word selects the whole table.
        const cells = [...tr.children].filter(td => !td.classList.contains('pd-acts'));
        const hay = (cells.map(td => td.textContent).join(' ') + ' ' + (tr.dataset.hay || '')).toLowerCase();
        tr.hidden = !!needle && hay.indexOf(needle) < 0;
    }
    for (const tbl of root().querySelectorAll('.pd-tbl')) {
        const shown = [...tbl.querySelectorAll('tbody tr')].filter(tr => !tr.hidden).length;
        const grp = tbl.previousElementSibling;
        tbl.hidden = shown === 0;
        if (grp && grp.classList.contains('pd-lgrp')) {
            grp.hidden = shown === 0;
            const count = grp.querySelector('.pd-dim');
            if (count) count.textContent = shown + ' process' + (shown === 1 ? '' : 'es');
        }
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
    // THE WAY BACK IS A CONTROL, NOT A BREADCRUMB. It was a --text-muted link
    // reading "Processes" before a › and the process name, and an engineer who
    // had opened a process could not find their way back to the list — the one
    // word that returns them was drawn in the same value as the separator
    // beside it. Same ruling as the positions footer and the station card's
    // chips: if it is the way out, it looks like a button.
    // A SCREEN NAMED AFTER ITS PROCESS IS NOT SAID TWICE. Add process names
    // the screen after the process when its box is left blank, so every new
    // process read "Weld Cell 13 · Weld Cell 13".
    const screen = p && stationsOf(p.id)[0];
    return '<div class="pd-app">' +
        '<div class="pd-crumb"><button class="pd-back" data-act="list">‹ All processes</button>' +
        '<b>' + esc(p ? p.name : '') +
        (screen && screen.name !== p.name ? ' · ' + esc(screen.name) : '') + '</b></div>' +
        // NO ROUTING TAB. The routing set is REVIEWED, and review is Settings'
        // job — it sits there beside the gate it is the precondition for
        // (owner ruling 2026-09-16). Creation is the Add-process sheet's.
        // SETUP IS FIRST, AND ONLY WHILE THE PROCESS CANNOT RUN (U2).
        '<div class="pd-tabs">' + (needsSetup() ? tab('setup', 'Setup') : '') + (coreBoards(p) ? '' : tab('flows', 'Styles')) +
        // NO RECIPES TAB (F36): recipes are HMI setup's, and sit beside it
        // on Settings (hmiRecipes); #tab=presets and #tab=recipes land there.
        tab('settings', 'Settings') + '</div>' +
        '<div class="pd-spacer"></div>' +
        // THE GATE IS A SENTENCE, NOT A BADGE, AND THE SETTINGS LINK WAS THE
        // TABS' OWN DOOR DRAWN TWICE: a pill shape claims "status chip" for a
        // thing that never changes here (the gate is set in Settings), and the
        // link beside it opened the tab one glance to the left. The gate stays
        // read-only; the words carry the state, so no border and no tone.
        // A Core loader's process has no styles to pick from: no gate line.
        (S.tab === 'setup' && S.setupEnd !== S.processID ? '<button class="pd-dimlink pd-skip" data-act="setup-skip">Skip setup, use Settings</button>'
            : setupHidden() ? '<button class="pd-dimlink pd-skip" data-act="setup-show">Show setup</button>' : '') +
        (coreBoards(p) || S.tab === 'setup' ? '' : '<div class="pd-gate">' +
            (on ? 'HMI setup on: operators set up styles' : 'HMI setup off: operators pick from the styles') + '</div>') +
        '</div>';
}

// ── Setup · a short path to a first line ─────────────────────────────────────
//
// FOR SOMEONE MAKING THEIR FIRST LINE, AND SKIPPABLE (owner, 2026-11-15: "walk
// someone through quickly and easily to just get a basic line going then they
// can come back later and do advanced setup ... but make it skippable if a
// more advanced person is there"). Three steps, each done where it is shown:
//   1. Nodes and part: the Add process sheet answers it; Change… opens that
//      sheet again, filled in.
//   2. First style: a part on each position and one way they all swap, filled
//      from the routing set, saved through the Styles tab's own save.
//   3. Run it: the screen Add made, opened. "they should leave with having an
//      operator screen they can click to start a swap on."
// No step lands on Settings. Beside them, the line so far and what can wait.
//
// SETUP IS FINISHED WHEN THE LINE CAN RUN AND ITS SCREEN HAS BEEN OPENED
// (owner, 2026-11-15: "some lines might not have changeovers ... basically
// when operator screen is opened"), not at the first changeover. Opened is the
// screen's own seen fact, last_seen_at, which its event stream writes when it
// connects (StationService.Touch). The tab then says the line is set up, and
// goes once the engineer leaves it.
//
// SKIP IS THIS BROWSER'S, per process (setupSkipped). It hides the tab and
// opens Settings; while the process still cannot run, a small "Show setup"
// brings it back. A loader's or unloader's process has no Setup: Core runs
// its board.
//
// EVERYTHING HERE IS READ OFF WHAT THE PAGE HOLDS: the composer read, the
// routing rows when Settings has read them, the screens and the process row.
// Run it alone re-reads the screens, every 5 s, while it waits for one to be
// opened.
const SETUP_POLL_MS = 5000;

// setupFacts is what the steps are drawn from.
function setupFacts() {
    const p = process() || {};
    const c = S.composer || {};
    const positions = ((c.cell || {}).positions || []).map(x => x.core_node_name);
    const routing = (S.routing || c.routing || []).filter(r => r.enabled !== false);
    const role = r => routing.filter(x => x.role === r).map(x => x.core_node_name);
    const styles = (c.styles || []).map(st => Object.assign({}, st, M().styleFacts(st)));
    const flowed = styles.filter(st => st.claim_count);
    const screens = stationsOf(S.processID);
    const working = screens.filter(st => (S.stationNodes[String(st.id)] || []).length);
    const parts = c.palette || [];
    const nodes = positions.length > 0 && role('source').length > 0 && role('destination').length > 0 && parts.length > 0;
    const canRun = nodes && flowed.length > 0 && working.length > 0;
    return {
        p: p, positions: positions, inbound: role('source'), staging: role('staging'), outbound: role('destination'),
        parts: parts, styles: styles, flowed: flowed, screens: screens, working: working, nodes: nodes, canRun: canRun,
        seen: working.find(st => st.last_seen_at) || null,
    };
}

// setupSkipKey is the browser's own record that this process's Setup was
// skipped. Storage that throws (a private window, blocked site data) is no
// skip: Setup shows.
function setupSkipKey(pid) { return 'pd.setup.skipped.' + pid; }
function setupSkipped(pid) {
    try { return window.localStorage.getItem(setupSkipKey(pid)) === '1'; } catch (_) { return false; }
}
function setSetupSkipped(pid, on) {
    try {
        if (on) window.localStorage.setItem(setupSkipKey(pid), '1'); else window.localStorage.removeItem(setupSkipKey(pid));
    } catch (_) { /* this browser keeps no record; Setup shows */ }
}

// needsSetup: the Setup tab is drawn, first. Until the line can run and its
// screen has been opened, unless skipped here; and once that happens while it
// is on screen, until the engineer leaves it (S.setupEnd).
function needsSetup() {
    if (!S.processID || !S.composer || coreBoards(process())) return false;
    if (S.setupEnd === S.processID) return true;
    if (setupSkipped(S.processID)) return false;
    const f = setupFacts();
    return !(f.canRun && f.seen);
}

// setupHidden: skipped here while the process still cannot run, so the app
// bar offers "Show setup".
function setupHidden() {
    if (!S.processID || !S.composer || coreBoards(process()) || !setupSkipped(S.processID)) return false;
    return !setupFacts().canRun;
}

// coreBoards: Core runs this process's boards (process.core_boards: every
// position is a Core loader's window). Its page is Line, its screens and a
// pointer to Core: no Setup, no Styles, no flow picture (owner, 2026-11-14).
function coreBoards(p) { return !!(p && p.core_boards); }

// hmiSetupFacts is HMI setup's value and its action (F36): one of HMI setup
// and its recipes, so a switch with no recipe behind it says so and offers one.
function hmiSetupFacts() {
    const p = process() || {};
    const n = (S.composer && S.composer.presets || []).length;
    // On Settings the switch is a draft until Save: the row beside it reads
    // the draft, so switching it on with no recipe says so at once (F36).
    const on = S.tab === 'settings' && S.settings ? !!S.settings.flow_composer_enabled : !!p.flow_composer_enabled;
    return {
        value: (on ? 'on' : 'off') + ' · ' + (n ? n + ' recipe' + (n === 1 ? '' : 's') : 'no recipe yet'),
        warn: on && !n,
        verb: n ? 'Change…' : 'New recipe',
    };
}

// SETUP_LATER is what a line runs without, each a link to its Settings card.
const SETUP_LATER = [['Production counter', 'Production counter'], ['Auto-reorder', 'Auto-reorder'],
    ['Light curtain', 'Light curtain'], ['Operator setup', 'Operator setup'], ['Quality hold', 'Quality hold']];

function drawSetup() {
    if (!needsSetup()) { stopSetupPoll(); S.tab = 'flows'; drawFlows(); syncHash(); return; }
    const f = setupFacts();
    if (S.setupView === 'style' && !f.flowed.length && firstStyleTarget()) { drawFirstStyle(f); return; }
    S.setupView = '';
    const doneN = [f.nodes, f.flowed.length > 0, f.canRun && !!f.seen].filter(Boolean).length;
    const left = f.canRun && f.seen ? setupEndHTML(f)
        : '<div class="pd-sutitle"><h1>Set up ' + esc(f.p.name || '') + '</h1><span class="pd-dim">' + doneN + ' of 3 done</span></div>' +
        '<ol class="pd-sustep">' + stepNodes(f) + stepStyle(f) + stepRun(f) + '</ol>';
    root().innerHTML = appbar() + '<div class="pd-sheet pd-setup"><div class="pd-sugrid"><div class="pd-sumain">' + left + '</div>' +
        '<div class="pd-suside">' + lineSoFar(f) + setupLaterHTML() + '</div></div>' +
        '<div class="pd-pop" id="pd-pop" hidden></div></div>';
    if (f.canRun && !f.seen) startSetupPoll(); else stopSetupPoll();
}

// A step: its number (✓ once done; the one to do now lit), its title, what it
// holds, and what to do.
function stepHTML(n, state, title, sub, body) {
    return '<li class="pd-sus ' + state + '"><span class="pd-susn">' + (state === 'done' ? '✓' : n) + '</span>' +
        '<div class="pd-susb"><b>' + esc(title) + '</b><small>' + esc(sub) + '</small>' + body + '</div></li>';
}

function stepNodes(f) {
    const first = list => list.length ? list[0] + (list.length > 1 ? ' +' + (list.length - 1) : '') : '';
    if (f.nodes) {
        const holds = [nOf(f.positions.length, 'position'), first(f.inbound) + ' in',
            f.staging.length ? first(f.staging) + ' staging' : '', first(f.outbound) + ' out',
            nOf(f.parts.length, 'part')].filter(Boolean).join(' · ');
        return stepHTML(1, 'done', 'Nodes and part', holds,
            '<div class="pd-susact"><button class="pd-btn" data-act="setup-edit">Change…</button>' +
            '<span class="pd-dim">opens the sheet you filled in</span></div>');
    }
    const need = [f.positions.length ? '' : 'Positions', f.inbound.length ? '' : 'Inbound', f.outbound.length ? '' : 'Outbound',
        f.parts.length ? '' : 'a part'].filter(Boolean);
    return stepHTML(1, 'now', 'Nodes and part', 'still to pick: ' + need.join(', '),
        '<div class="pd-susact"><button class="pd-btn primary" data-act="setup-edit">Pick them</button>' +
        '<span class="pd-dim">the sheet you made the process with</span></div>');
}

function stepStyle(f) {
    if (f.flowed.length) {
        const st = f.flowed[0];
        return stepHTML(2, 'done', 'First style', st.name + (f.flowed.length > 1 ? ' · +' + (f.flowed.length - 1) : ''),
            '<div class="pd-susact"><button class="pd-btn" data-act="setup-styles">Open it on Styles</button></div>');
    }
    if (!f.nodes) return stepHTML(2, 'next', 'First style', 'a part on each position and how they swap', '');
    return stepHTML(2, 'now', 'First style', 'a part on each position and how they swap',
        '<div class="pd-sucard"><div><b>Make the first style</b><small>Pick how the positions swap. Inbound, Outbound and any ' +
        'Staging come from step 1.</small></div><button class="pd-btn primary" data-act="setup-style">Start</button></div>');
}

function stepRun(f) {
    const sub = 'open the operator screen, then start the first changeover or make the first call';
    if (!f.nodes || !f.flowed.length) return stepHTML(3, 'next', 'Run it', sub, f.working.length
        ? '<p class="pd-dim">The screen “' + esc(f.working[0].name) + '” is already made and holds ' + nOf((S.stationNodes[String(f.working[0].id)] || []).length, 'position') + '.</p>' : '');
    if (!f.working.length) {
        const st = f.screens[0];
        return stepHTML(3, 'now', 'Run it', sub, '<div class="pd-sucard"><div><b>' +
            (st ? 'The screen “' + esc(st.name) + '” holds no position yet' : 'This line has no operator screen yet') + '</b>' +
            '<small>' + (st ? 'Put the positions on it, then open it.' : 'Add one; it takes the positions.') + '</small></div>' +
            (st ? '<button class="pd-btn primary" data-act="setup-onto">Put the positions on it</button>'
                : '<button class="pd-btn primary" data-act="screen-add">+ Add a screen</button>') + '</div>');
    }
    const st = f.working[0];
    const style = f.flowed[0];
    const press = (n, what) => '<div class="pd-supress"><span class="pd-susn">' + n + '</span><b>' + esc(what) + '</b></div>';
    return stepHTML(3, 'now', 'Run it', sub, '<div class="pd-sucard col">' +
        '<div class="pd-surow"><div><b>The operator screen “' + esc(st.name) + '” is ready</b><small>It holds ' +
        esc((S.stationNodes[String(st.id)] || []).join(', ')) + ' and the style ' + esc(style.name) + '.</small></div>' +
        '<a class="pd-btn primary" href="/operator/station/' + st.id + '" target="_blank" rel="noopener">Open the screen</a></div>' +
        '<div class="pd-supresses">' + press(1, 'Press CHANGEOVER') + press(2, 'Pick ' + style.name) + press(3, 'Press Start') + '</div>' +
        '<p class="pd-dim">A line that has no changeover makes its first call from the screen instead.</p>' +
        '<p class="pd-suwait"><i class="pd-cnt warn"><i></i></i>Waiting for the screen to be opened. This step ticks by itself.</p></div>');
}

// setupEndHTML is Setup's last word: the line is set up, and where its three
// doors are. The tab goes once the engineer leaves it.
function setupEndHTML(f) {
    const st = f.seen || f.working[0];
    return '<div class="pd-sutitle"><h1>' + esc(f.p.name || '') + ' is set up</h1><span class="pd-dim">3 of 3 done</span></div>' +
        '<div class="pd-sucard col"><div><b>Its operator screen “' + esc(st.name) + '” has been opened.</b>' +
        '<small>Setup closes now. Everything else is on Settings.</small></div>' +
        '<div class="pd-susact"><a class="pd-btn primary" href="/operator/station/' + st.id + '">Open the screen</a>' +
        '<button class="pd-btn" data-tab="flows">Styles</button><button class="pd-btn" data-tab="settings">Settings</button></div></div>';
}

// lineSoFar is the picture of the line beside Setup's steps: where the bin
// comes from, the positions (each with what its first style runs there),
// where the bin goes, and its staging. A position a style leaves free is "not
// in this style"; before any style, "no style yet". First style draws none:
// its checklist is the line so far.
function lineSoFar(f) {
    const st = f.flowed[0];
    const claimOf = n => (st && st.claims || []).find(c => c.core_node_name === n);
    const posBox = n => {
        const cl = claimOf(n);
        const sub = cl ? (M().modeLabels()[cl.swap_mode] || cl.swap_mode) + (cl.payload_code ? ' · ' + M().shortPart(cl.payload_code) : '')
            : st ? 'not in this style' : 'no style yet';
        return '<div class="pd-supos" data-role="positions"><b>' + esc(n) + '</b><small>' + esc(sub) + '</small></div>';
    };
    const end = (role, label, names) => '<div class="pd-supos" data-role="' + role + '"' + (names.length ? '' : ' data-empty') + '>' +
        '<small class="pd-surole">' + esc(label) + '</small><b>' + esc(names.length ? names.join(', ') : 'none yet') + '</b></div>';
    const staging = f.staging.length ? 'Staging: ' + f.staging.join(', ') + '.' : 'Staging: none. Optional; a swap that needs one will ask.';
    return '<div class="pd-card"><div class="pd-sect"><h2>Your line so far</h2></div><div class="pd-suline">' +
        end('inbound', 'Inbound', f.inbound) + '<span class="pd-dim">→</span>' +
        (f.positions.length ? f.positions.map(posBox).join('') : '<span class="pd-dim">no positions yet</span>') +
        '<span class="pd-dim">→</span>' + end('outbound', 'Outbound', f.outbound) + '</div>' +
        '<p class="pd-note"><i class="setpick-dot" data-role="staging"></i> ' + esc(staging) + '</p></div>';
}

function setupLaterHTML() {
    return '<div class="pd-card"><div class="pd-sect"><h2>Later, on Settings · not needed to run</h2></div><div class="pd-sulater">' +
        SETUP_LATER.map(x => '<button class="pd-dimlink" data-act="setup-later" data-to="' + esc(x[1]) + '">' + esc(x[0]) + '</button>').join('') +
        '</div></div>';
}

// The Run it step re-reads the screens while it waits for one to be opened.
function startSetupPoll() {
    if (S.setupPoll) return;
    const pid = S.processID;
    S.setupPoll = setInterval(async () => {
        if (S.tab !== 'setup' || S.processID !== pid) { stopSetupPoll(); return; }
        await reloadStations();
        const f = setupFacts();
        if (f.canRun && f.seen) {
            stopSetupPoll();
            S.setupEnd = pid;
            setSetupSkipped(pid, false);
            if (S.tab === 'setup' && S.processID === pid) drawSetup();
        }
    }, SETUP_POLL_MS);
}
function stopSetupPoll() {
    if (S.setupPoll) clearInterval(S.setupPoll);
    S.setupPoll = null;
}

// setupGo is a step's action.
async function setupGo(act) {
    if (act === 'edit') { await openEditProcess(); return; }
    if (act === 'styles') {
        const f = setupFacts();
        S.tab = 'flows';
        selectStyle((f.flowed[0] || f.styles[0] || {}).id || 0);
        syncHash();
        return;
    }
    if (act === 'style') { await startFirstStyle(); return; }
    if (act === 'skip') {
        setSetupSkipped(S.processID, true);
        stopSetupPoll();
        await openSettingsAt('Line');
        return;
    }
    if (act === 'onto') {
        // The screen takes the positions on no screen; with none, the sheet.
        const st = stationsOf(S.processID)[0];
        await loadCurtainNodes();
        const loose = loosePositions();
        if (!st || !loose.length) { await openEditProcess(); return; }
        const want = B().stationNodes((S.stationNodes[String(st.id)] || []).concat(loose));
        const out = await send('PUT', '/api/operator-stations/' + st.id + '/claimed-nodes', want, NO_NOTICE);
        if (!out.ok) { toast(said(out), 'error'); return; }
        await refreshStationNodes(st.id);
        await loadCurtainNodes();
        await refreshProcess();
        return;
    }
    if (act === 'show') {
        setSetupSkipped(S.processID, false);
        S.tab = 'setup';
        drawSetup();
        syncHash();
    }
}

// ── Setup · First style ──────────────────────────────────────────────────────
//
// ONE POSITION AT A TIME (owner, 2026-11-15: "id like to go position by
// position in the setup helper then mark green when its done"). The left is
// the line's positions as a checklist: done (a green tick, its part and its
// swap), the one open now, and the rest to do; a row opens its position, a
// done one too, to change it. "+ Add a position" puts a free node on the line
// and its screen, and "Take <position> off this line" takes one off, both
// through Settings' positions write (savePositions, screensDraft), asked
// about first as Settings asks. The middle is the open position: its part,
// consume or produce, its swap (it starts as the position before it), and
// what was picked for it from the routing set, each a dropdown of that role's
// box, only what its swap uses. "Done · next position" ticks it; "Not in this
// style" leaves it free. Save style is live once every position is done or
// left free.
//
// The name starts as the first part's and takes any text, as the New style
// sheet's one-off name does: POST /api/styles takes any name, and the only
// rule is the store's, one live style per name in a process. A name somebody
// chose is kept; a part-code name follows the first position's part.
//
// A part is on one position (the composer's rule, setPart): a part another
// position has moves here, and that position is not done any more. A
// position not opened yet runs as the one before it does, until it is opened:
// then its own copy is changed. A swap that requires staging is filled as the
// Styles tab fills a new row (addFreeAs): inbound staging the Staging box's
// first name, outbound staging the next, or the first again when the box
// holds one; one the swap may use starts empty.
//
// It is the Styles tab's draft (S.model), previewed and saved by the Styles
// tab's own doors (flow/preview, saveFlow): no second writer. A changed name
// is the style's rename (PUT /api/styles/{id}), written first.
//
// A press's back positions are never rows: they are a swap's partner, and the
// model pairs them (ensurePartner).

const FIRST_ROLES = [['consume', 'Consume'], ['produce', 'Produce']];

// FIRST_PICKS is what the step fills in, in the order it is said: the cell
// key, the claim field, its word, its role's colour, the routing box its
// choices come from, and the model's action with its argument.
const FIRST_PICKS = [
    { key: 'source', field: 'inbound_source', label: 'Comes from', role: 'inbound', box: 'source', act: 'setSource', arg: 'source' },
    { key: 'staging', field: 'inbound_staging', label: 'Inbound staging', role: 'staging', box: 'staging', act: 'setStaging', arg: 'staging' },
    { key: 'parkOld', field: 'outbound_staging', label: 'Outbound staging', role: 'staging', box: 'staging', act: 'setParkOld', arg: 'staging' },
    { key: 'dest', field: 'outbound_destination', label: 'Goes to', role: 'outbound', box: 'destination', act: 'setDest', arg: 'dest' },
];

// firstStyleTarget is the style the step draws: the first with no flow.
function firstStyleTarget() {
    return (S.composer.styles || []).find(st => !M().styleFacts(st).claim_count) || null;
}

function firstStyleRows() {
    return ((S.composer.cell || {}).positions || []).filter(p => p.kind !== 'back').map(p => p.core_node_name);
}

// startFirstStyle opens the step: on the style with no flow, or on a new one
// named by the first part (a style is named by its part until it is renamed).
async function startFirstStyle() {
    let st = firstStyleTarget();
    if (!st) {
        const part = (S.composer.palette || [])[0];
        if (!part) return;
        const made = await send('POST', '/api/styles', B().styleCreate(part, S.processID, ''), NO_NOTICE);
        if (!made.ok) { toast(said(made), 'error'); return; }
        S.styleID = Number(bodyOf(made).id) || 0;
        S.setupView = 'style';
        await refreshProcess();
        st = firstStyleTarget();
        if (!st) return;
    }
    const rows = firstStyleRows();
    const palette = S.composer.palette || [];
    const parts = {};
    rows.forEach((n, i) => { if (palette[i]) parts[n] = palette[i]; });
    // A NAME SOMEBODY CHOSE IS KEPT (named): only a part-code name follows.
    S.firstStyle = {
        styleID: st.id, name: st.name || '', named: palette.indexOf(st.name) < 0, parts: parts,
        own: {}, done: {}, out: {}, cur: '', error: '', warned: '',
    };
    if (rows.length) openFirstPosition(rows[0]);
    S.setupView = 'style';
    S.tab = 'setup';
    buildFirstStyle();
    drawSetup();
    syncHash();
    void previewFirstStyle();
}

// firstEff is what position n runs as: its own copy once opened, else the
// nearest opened position before it, else a consume 1‑Robot Swap with every
// pick at its default.
function firstEff(n) {
    const d = S.firstStyle;
    if (d.own[n]) return d.own[n];
    const rows = firstStyleRows();
    for (let i = rows.indexOf(n) - 1; i >= 0; i--) if (d.own[rows[i]]) return d.own[rows[i]];
    return { role: 'consume', mode: 'single_robot', picks: {} };
}

// openFirstPosition makes n the open position; opened the first time, it
// takes a copy of what it ran as (the position before it).
function openFirstPosition(n) {
    const d = S.firstStyle;
    if (!d.own[n]) {
        const e = firstEff(n);
        d.own[n] = { role: e.role, mode: e.mode, picks: Object.assign({}, e.picks) };
    }
    d.cur = n;
    d.warned = '';
}

// firstChoices is a pick's choices: its routing box's names, in order. A
// staging spot is never one of this process's positions.
function firstChoices(p) {
    return (S.composer.routing || []).filter(r => r.role === p.box && r.enabled !== false &&
        (p.box !== 'staging' || !isPositionRow(r.core_node_name)))
        .sort((a, b) => (a.sequence || 0) - (b.sequence || 0)).map(r => r.core_node_name);
}

// firstUse is how a swap uses a pick's field in a role, from the flowspec the
// model carries: 'required', 'used' (it may), or '' (it does not).
function firstUse(state, role, mode, p) {
    const row = ((((state && state.flowspec) || {}).steady || {})[role] || {})[mode] || {};
    const u = row[p.field];
    return u === 'required' || u === 'used' ? u : '';
}

// firstValue is what a pick holds at position n: its chosen value, else the
// default (the box's first; a required outbound staging the next one).
function firstValue(state, n, p) {
    const e = firstEff(n);
    if (e.picks[p.key] !== undefined) return e.picks[p.key];
    const choices = firstChoices(p);
    if (p.box !== 'staging') return choices[0] || '';
    if (firstUse(state, e.role, e.mode, p) !== 'required') return '';
    return (p.key === 'parkOld' ? choices[1] || choices[0] : choices[0]) || '';
}

// buildFirstStyle is the step's draft as the composer's own actions on the
// style: each position with a part, not left free, added in its swap, its
// role, its part, and each pick its swap uses.
function buildFirstStyle() {
    const d = S.firstStyle;
    const st = composerStyle(d.styleID);
    if (!st) return;
    S.styleID = st.id;
    S.selected = null;
    let m = initModel(st);
    for (const n of firstStyleRows()) {
        const part = d.parts[n];
        if (!part || d.out[n]) continue;
        const e = firstEff(n);
        m = M().reduce(m, { type: 'addPosition', node: n, mode: e.mode });
        if (e.role === 'produce') m = M().reduce(m, { type: 'setRole', node: n, role: 'produce' });
        m = M().reduce(m, { type: 'addPart', payloadCode: part });
        m = M().reduce(m, { type: 'setPart', node: n, payloadCode: part });
        for (const p of FIRST_PICKS) {
            if (!firstUse(m, e.role, e.mode, p)) continue;
            const a = { type: p.act, node: n };
            a[p.arg] = firstValue(m, n, p);
            m = M().reduce(m, a);
        }
    }
    S.model = m;
    S.baseline = '';
}

// firstToGo is the positions neither done nor left free.
function firstToGo() {
    const d = S.firstStyle;
    return firstStyleRows().filter(n => !d.done[n] && !d.out[n]);
}

function drawFirstStyle(f) {
    const d = S.firstStyle;
    if (!d || !S.model || S.model.styleId !== d.styleID) { S.setupView = ''; drawSetup(); return; }
    const labels = M().modeLabels();
    const rows = firstStyleRows();
    if (d.cur && rows.indexOf(d.cur) < 0) d.cur = '';
    const togo = firstToGo();
    const doneN = rows.length - togo.length;
    const item = (n, i) => {
        const e = firstEff(n);
        const state = d.out[n] ? 'out' : n === d.cur ? 'now' : d.done[n] ? 'done' : 'todo';
        const sub = d.out[n] ? 'not in this style'
            : d.done[n] ? (d.parts[n] ? M().shortPart(d.parts[n]) + ' · ' : '') + (labels[e.mode] || e.mode)
                : n === d.cur ? 'now' : 'to do';
        return '<li class="pd-fsitem ' + state + (d.done[n] ? ' is-done' : '') + '"><button data-act="fs-open" data-node="' + esc(n) + '">' +
            '<span class="pd-fsmark">' + (d.done[n] ? '✓' : d.out[n] ? '–' : String(i + 1)) + '</span>' +
            '<span class="pd-fsname"><b>' + esc(n) + '</b><small>' + esc(sub) + '</small></span></button></li>';
    };
    const list = '<div class="pd-fslist"><div class="pd-fslhd">Positions · ' + doneN + ' of ' + rows.length + ' done</div>' +
        '<ol>' + rows.map(item).join('') + '</ol>' +
        '<button class="pd-btn quiet" data-act="fs-addpos">+ Add a position</button></div>';
    const top = '<div class="pd-fstop"><h1>First style</h1>' +
        '<label class="pd-fsnamefld"><span>Style name</span><input class="pd-inp" id="pd-fsname" data-fs="name" type="text" autocomplete="off" value="' + esc(d.name) + '"></label>' +
        '<span class="pd-spacer"></span><span class="pd-fsgo" id="pd-fsgo">' + esc(firstSaveWords()) + '</span>' +
        '<button class="pd-btn primary" data-act="fs-save"' + (firstStyleReady() ? '' : ' disabled') + '>Save style</button></div>';
    const body = d.cur ? firstPositionHTML(d.cur, rows) : firstAllDoneHTML(rows);
    root().innerHTML = appbar() + '<div class="pd-sheet pd-setup">' + top +
        (d.error ? '<div class="pd-refusal">' + esc(d.error) + '</div>' : '') +
        '<div class="pd-fsgrid">' + list + '<div class="pd-fspos">' + body + '</div>' +
        (d.cur ? '<div class="pd-fswork">' + workListHTML(d.cur, firstWorkWait(d.cur)) + '</div>' : '') + '</div>' +
        '<div class="pd-pop" id="pd-pop" hidden></div></div>';
    const name = $('pd-fsname');
    if (name && name.addEventListener) {
        name.addEventListener('input', () => {
            d.name = name.value;
            d.named = true;
            const go = $('pd-fsgo');
            if (go) go.textContent = firstSaveWords();
            const save = root().querySelector('[data-act="fs-save"]');
            if (save) save.disabled = !firstStyleReady();
        });
    }
}

// firstPositionHTML is the open position: its part, consume or produce, its
// swap, its picks, and its buttons.
function firstPositionHTML(n, rows) {
    const d = S.firstStyle;
    const e = firstEff(n);
    const labels = M().modeLabels();
    const order = Object.keys(MODE_WORDS).filter(m => labels[m]);
    const part = d.parts[n];
    const roleSeg = '<div class="pd-seg">' + FIRST_ROLES.map(r => '<button class="' + (e.role === r[0] ? 'on' : '') +
        '" data-act="fs-role" data-role="' + r[0] + '">' + esc(r[1]) + '</button>').join('') + '</div>';
    const cards = order.map(m => '<button class="pd-fsmode' + (e.mode === m ? ' on' : '') + '" data-act="fs-mode" data-mode="' + esc(m) + '">' +
        gl(m, 26, {}) + '<span><b>' + esc(labels[m]) + '</b><small>' + esc(MODE_WORDS[m]) + '</small></span></button>').join('');
    // A STAGING PICK ONLY WHERE THE SWAP READS IT. flowspec marks staging
    // `used` on Index Swap and Sequential A/B for a tool change, which holds
    // the incoming carrier there (changeover_tooling.go); their steady swap
    // never reads it (the work list beside shows no stop there), and First
    // style marks no tool change. The Styles table keeps both.
    const picks = FIRST_PICKS.map(p => [p, firstUse(S.model, e.role, e.mode, p)])
        .filter(x => x[1] && (x[0].box !== 'staging' || x[1] === 'required')).map(x => {
        const p = x[0];
        // What the position runs with, in the draft or (a position with no
        // part yet, not in the draft) as it would. No colour on the box: the
        // bar says what is missing.
        const cell = S.model.cells[n];
        const value = (cell && cell.on ? cell[p.key] : firstValue(S.model, n, p)) || '';
        const shown = value || (x[1] === 'required' ? (firstChoices(p).length ? 'pick one' : 'none in the ' + boxWord(p.box) + ' box') : 'none');
        return '<div class="pd-fsfield"><span class="pd-fsflbl"><i class="setpick-dot" data-role="' + p.role + '"></i>' + esc(p.label) + '</span>' +
            '<button class="pd-sel" data-act="fs-pick" data-pick="' + p.key + '">' +
            '<span class="t">' + esc(shown) + '</span><i class="car"></i></button></div>';
    }).join('');
    const warn = d.warned === n && S.firstWarn ? '<div class="pd-warn">' + esc(S.firstWarn) + '</div>' : '';
    return '<div class="pd-fsposhd"><h2>' + esc(n) + '</h2><span class="pd-spacer"></span>' +
        '<button class="pd-dimlink" data-act="fs-takeoff">Take ' + esc(n) + ' off this line</button></div>' + warn +
        '<div class="pd-fsfield"><span class="pd-fsflbl">Part</span>' +
        '<button class="pd-sel" data-act="fs-part" data-node="' + esc(n) + '"><span class="t">' + esc(part ? M().shortPart(part) : 'no part') +
        '</span><i class="car"></i></button>' +
        '<button class="pd-dimlink" data-act="fs-addpart" data-node="' + esc(n) + '">+ add a part</button></div>' +
        '<div class="pd-fsfield"><span class="pd-fsflbl">Consume or produce</span>' + roleSeg + '</div>' +
        '<div class="pd-fsblock"><span class="pd-fsflbl">Swap</span><div class="pd-fsmodes">' + cards + '</div></div>' +
        (picks ? '<div class="pd-fsblock"><span class="pd-fsflbl">Picked for you</span>' + picks + '</div>' : '') +
        '<div class="pd-fsfoot"><div id="pd-bar" class="pd-fsbar">' + firstStyleBar() + '</div><span class="pd-spacer"></span>' +
        '<button class="pd-btn" data-act="fs-out">Not in this style</button>' +
        '<button class="pd-btn" data-act="fs-back">Back</button>' +
        // OFF UNTIL THE POSITION IS READY: a part, and nothing of its own to
        // fix (positionFixes), as the bar says it.
        '<button class="pd-btn done" data-act="fs-done"' + (part && !positionFixes(n).length ? '' : ' disabled') + '>Done · next position</button></div>';
}

// firstAllDoneHTML is the middle once no position is open: every one done or
// left free, or none on the line yet.
function firstAllDoneHTML(rows) {
    const togo = firstToGo();
    if (!rows.length) return '<p class="pd-dim">This line has no positions yet. Add one on the left.</p>';
    return '<div class="pd-fsposhd"><h2 class="pd-fsplain">' + (togo.length ? esc(nOf(togo.length, 'position')) + ' to go' : 'Every position is done') + '</h2></div>' +
        '<div class="pd-fsfoot"><div id="pd-bar" class="pd-fsbar">' + firstStyleBar() + '</div><span class="pd-spacer"></span>' +
        '<button class="pd-dimlink" data-act="fs-table">Open the full flow table</button>' +
        '<button class="pd-btn" data-act="fs-back">Back</button></div>';
}

// positionFixes is what position n still needs, its findings' own sentences
// (errors only; a warning is advice and blocks nothing).
function positionFixes(n) {
    return M().findings(S.model).filter(f => f.node === n && f.severity !== 'warning').map(f => f.message);
}

// firstWorkWait is what the work list says while position n has none: it
// has no part yet, so it is not in the draft, or the preview is on its way.
function firstWorkWait(n) {
    const d = S.firstStyle;
    if (!d.parts[n]) return 'Give ' + n + ' a part to see what its robots do.';
    return d.out[n] ? n + ' is not in this style.' : 'Checking…';
}

// firstSaveWords is the top line's word beside Save style.
function firstSaveWords() {
    const togo = firstToGo().length;
    if (togo) return nOf(togo, 'position') + ' to go';
    if (!String(S.firstStyle.name || '').trim()) return 'the style needs a name';
    return '';
}

// firstStyleBar is the step's one line: ready, with its orders, or the first
// thing to fix, in the Styles bar's own words (composer-model bar).
function firstStyleBar() {
    if (!Object.keys(S.firstStyle.parts).some(n => S.firstStyle.parts[n] && !S.firstStyle.out[n])) {
        return '<span class="pd-dim">Give a position a part to make the style.</span>';
    }
    const b = M().bar(S.model);
    const pv = S.model.preview || {};
    if (b.tone === 'blocked') return '<i class="pd-cnt warn"><i></i></i><b>' + esc(b.heading) + '</b> <span class="pd-dim">' + esc(b.detail || '') + '</span>';
    if (!pv.fingerprint) return '<span class="pd-dim">Checking…</span>';
    const n = Object.keys(S.model.cells).filter(k => S.model.cells[k].on).length;
    return '<i class="pd-cnt ok"><i></i></i>Ready: ' + esc(nOf(n, 'position')) +
        (typeof pv.orderCount === 'number' ? ', ' + esc(nOf(pv.orderCount, 'order')) : '') + '.';
}

function firstStyleReady() {
    const d = S.firstStyle;
    return !!S.model && !!(S.model.preview || {}).fingerprint && M().bar(S.model).tone !== 'blocked' &&
        Object.keys(d.parts).some(n => d.parts[n] && !d.out[n]) && !!String(d.name || '').trim() && !firstToGo().length;
}

// previewFirstStyle is the Styles tab's preview of the draft (flow/preview).
async function previewFirstStyle() {
    if (!S.model || !S.firstStyle) return;
    const forStyle = S.model.styleId;
    const res = await send('POST', '/api/processes/' + S.processID + '/flow/preview?steps=1', {
        to_style_id: forStyle, cells: M().toCells(S.model), taken_off: M().takenOff(S.model),
    }, { notice: false });
    if (!S.model || S.model.styleId !== forStyle || S.setupView !== 'style') return;
    S.model = M().applyPreview(S.model, answerBody(res));
    keepSteady(forStyle, answerBody(res));
    if (S.tab === 'setup') drawSetup();
}

// firstStyleChanged rebuilds the draft after a choice and previews it again.
// The name follows the first position's part until somebody types one.
function firstStyleChanged() {
    const d = S.firstStyle;
    if (!d.named) {
        const first = firstStyleRows().map(n => (d.out[n] ? '' : d.parts[n])).find(Boolean);
        if (first) d.name = first;
    }
    d.error = '';
    buildFirstStyle();
    drawSetup();
    void previewFirstStyle();
}

// setFirstPart puts code on position node (or takes its part off): a part on
// another position moves here, and that position is not done any more.
function setFirstPart(node, code) {
    const d = S.firstStyle;
    for (const n of Object.keys(d.parts)) {
        if (code && n !== node && d.parts[n] === code) { delete d.parts[n]; d.done[n] = false; }
    }
    if (code) { d.parts[node] = code; d.out[node] = false; } else delete d.parts[node];
    firstStyleChanged();
}

// openFirstPart is the open position's part, from the part set: one part is
// on one position, so picking one another position has moves it here.
function openFirstPart(btn) {
    const pop = $('pd-pop');
    const d = S.firstStyle;
    const node = btn.dataset.node;
    if (!pop || !d) return;
    const at = code => Object.keys(d.parts).find(n => n !== node && d.parts[n] === code);
    const rows = [{ value: '-', label: 'no part' }].concat((S.composer.palette || []).map(code =>
        ({ value: code, label: M().shortPart(code), note: at(code) ? 'on ' + at(code) + ', moves here' : '' })));
    openRowPicker(pop, btn, rows, v => setFirstPart(node, v === '-' ? '' : v),
        { selected: d.parts[node] || '-', placeholder: 'find a part' });
}

// openFirstAddPart is "+ add a part": Core's catalog, as the Styles tab's part
// popover offers it. The part joins the process's part set (the same PUT …/
// payloads, written at once: the set is not a flow's) and goes on this
// position; one already on another position moves here.
async function openFirstAddPart(btn) {
    const pop = $('pd-pop');
    const d = S.firstStyle;
    const node = btn.dataset.node;
    if (!pop || !d) return;
    await Promise.all([loadPayloadCatalog(), loadPartSet()]);
    if (!S.firstStyle || S.firstStyle !== d) return;
    const at = code => Object.keys(d.parts).find(n => n !== node && d.parts[n] === code);
    const inSet = new Set(S.partSet || []);
    const rows = (PARTS.rows() || []).map(p => ({
        value: p.code, label: p.code,
        note: at(p.code) ? 'on ' + at(p.code) + ', moves here' : inSet.has(p.code) ? 'in this process’s parts' : p.name || '',
    }));
    openRowPicker(pop, btn, rows, async code => {
        if (!S.partSet) { d.error = 'This process’s part list could not be read, so a part cannot be added here. Reload the page.'; drawSetup(); return; }
        if (!inSet.has(code)) {
            const out = await writePartSet(S.partSet.concat([code]));
            if (!out.ok) { d.error = said(out, 'Could not add ' + code + ': '); drawSetup(); return; }
        }
        setFirstPart(node, code);
    }, {
        placeholder: 'find a part in Core’s catalog',
        empty: S.payloadCatalog ? 'No part matches' : 'Core’s catalog could not be read. Try again in a moment.',
    });
}

// openFirstPick is one of the open position's picks: its routing box's names,
// and "none" for a field its swap may go without.
function openFirstPick(btn) {
    const pop = $('pd-pop');
    const d = S.firstStyle;
    const p = FIRST_PICKS.find(x => x.key === btn.dataset.pick);
    const n = d && d.cur;
    if (!pop || !d || !p || !n || !S.model) return;
    const e = firstEff(n);
    const use = firstUse(S.model, e.role, e.mode, p);
    const rows = (use === 'used' ? [{ value: '-', label: 'none' }] : []).concat(firstChoices(p).map(x => ({ value: x, label: x })));
    openRowPicker(pop, btn, rows, v => {
        d.own[n].picks[p.key] = v === '-' ? '' : v;
        firstStyleChanged();
    }, {
        selected: firstValue(S.model, n, p) || '-', placeholder: 'find a name', head: esc(p.label),
        empty: 'The ' + boxWord(p.box) + ' box is empty. Add one with Change… on step 1.',
    });
}

// firstDone ticks the open position and opens the next one to do after it
// (then any before it), or none.
function firstDone() {
    const d = S.firstStyle;
    const n = d.cur;
    if (!n || !d.parts[n] || positionFixes(n).length) return;
    d.done[n] = true;
    d.out[n] = false;
    firstNext(n);
}

// firstOut leaves the open position free: not in this style.
function firstOut() {
    const d = S.firstStyle;
    const n = d.cur;
    if (!n) return;
    delete d.parts[n];
    d.done[n] = false;
    d.out[n] = true;
    firstNext(n);
}

function firstNext(from) {
    const rows = firstStyleRows();
    const togo = firstToGo();
    const i = rows.indexOf(from);
    const next = togo.find(x => rows.indexOf(x) > i) || togo[0] || '';
    if (next) openFirstPosition(next); else S.firstStyle.cur = '';
    firstStyleChanged();
}

// openFirstAddPosition is "+ Add a position": the free nodes (a node held
// by another process, in this process's routing set or a group is not
// offered). The pick goes on the line and its screen through Settings'
// positions write, and opens as the position after the last one.
function openFirstAddPosition(btn) {
    const pop = $('pd-pop');
    if (!pop || !S.firstStyle) return;
    loadCoreNodes().then(() => {
        const mine = new Set(processPositions());
        const routing = new Set((S.composer.routing || []).map(r => r.core_node_name));
        const rows = (S.coreNodes || []).filter(n => n && n.name && n.node_type !== 'NGRP' && !mine.has(n.name))
            .map(n => {
                const r = { value: n.name, label: n.name };
                if (heldElsewhere(n.name, S.processID)) r.reason = positionHeld(n.name, S.processID) || 'held by another process';
                else if (routing.has(n.name)) r.reason = 'in this process’s routing set';
                return r;
            })
            .sort((a, b) => (a.reason ? 1 : 0) - (b.reason ? 1 : 0) || (a.label < b.label ? -1 : 1));
        openRowPicker(pop, btn, rows, name => { void firstWritePositions(all => all.concat([name]), name); },
            { placeholder: 'find a free node', empty: 'No free node matches' });
    });
}

// firstTakeOff is "Take <position> off this line": asked about first, as
// Settings asks (savePositions: the server says what still names it, and a
// second press takes it off).
function firstTakeOff() {
    const d = S.firstStyle;
    const n = d && d.cur;
    if (!n) return;
    void firstWritePositions(all => all.filter(x => x !== n), '', n);
}

// firstWritePositions writes the line's positions through Settings' door and
// reads the line again. change(all) is the new list from every position the
// line has, read first: the positions on no screen are only known once the
// process's positions are read, and a list made without them takes them off.
// open: a position to open after (one just added); gone: the position taken
// off (its warning is said on it).
async function firstWritePositions(change, open, gone) {
    const d = S.firstStyle;
    await loadCurtainNodes();
    const out = await savePositions(screensDraft(change(processPositions())));
    if (!out.ok) {
        S.firstWarn = out.error || 'The positions could not be saved.';
        d.warned = gone || d.cur;
        drawSetup();
        return;
    }
    S.firstWarn = '';
    d.warned = '';
    await refreshProcessStationNodes();
    await loadCurtainNodes();
    await refreshProcess();
    if (!S.firstStyle) return;
    if (gone) {
        delete d.parts[gone];
        delete d.own[gone];
        delete d.done[gone];
        delete d.out[gone];
        if (d.cur === gone) d.cur = '';
        firstNext(gone);
        return;
    }
    if (open && firstStyleRows().indexOf(open) >= 0) openFirstPosition(open);
    firstStyleChanged();
}

// saveFirstStyle is the Styles tab's save of the draft; it comes back to
// Setup. A changed name is the style's rename, written first: a refused
// rename stops the save and says why.
async function saveFirstStyle() {
    if (!firstStyleReady()) return;
    const d = S.firstStyle;
    const name = String(d.name || '').trim();
    const st = composerStyle(d.styleID);
    if (st && name !== st.name) {
        const row = await styleListRow(d.styleID);
        if (!row) { d.error = 'This style could not be read, so it was not renamed or saved. Reload the page.'; drawSetup(); return; }
        const out = await send('PUT', '/api/styles/' + d.styleID, styleBody(row, { name: name }), NO_NOTICE);
        if (!out.ok) { d.error = said(out, 'Not saved. The name was refused: '); drawSetup(); return; }
        row.name = name;
        st.name = name;
    }
    S.fromSetup = true;
    await saveFlow();
}

// boxWord is a routing box's name as the set pick says it.
function boxWord(box) {
    const g = ROUTING_GROUPS.find(x => x[0] === box);
    return g ? g[1] : box;
}

// openFirstTable takes the draft to the Styles tab's full flow table.
function openFirstTable() {
    S.fromSetup = true;
    S.setupView = '';
    S.tab = 'flows';
    drawFlows();
    syncHash();
    schedulePreview();
}

// openSettingsAt opens Settings scrolled to the section whose heading starts
// with `heading` — the words of the brief's sections, which Settings' own
// headings carry.
async function openSettingsAt(heading) {
    S.tab = 'settings';
    // What the last write saved is said on Settings' own notice line.
    if (S.notice) { S.settingsNotice = S.notice; S.notice = ''; }
    await openSettings();
    syncHash();
    const h = settingsHeading(heading);
    if (h && h.scrollIntoView) h.scrollIntoView({ block: 'start' });
}

// ── D1 · Flows ───────────────────────────────────────────────────────────────
async function openProcess(id) {
    try {
        await loadStack();
    } catch (err) {
        root().innerHTML = '<div class="pd-list"><p class="pd-dim">This process’s page could not load (' +
            esc(err && err.message ? err.message : String(err)) + '). Reload to try again.</p></div>';
        return;
    }
    if (S.processID !== id) { S.setupEnd = 0; S.setupView = ''; S.firstStyle = null; stopSetupPoll(); }
    S.processID = id;
    S.tab = 'flows';
    // Everything derived from the last process goes with it: another press's
    // routing rows would be read as this one's.
    S.routing = null;
    S.routingError = '';
    S.routingWarning = '';
    // The routing pickers go with the routing set: they are write-through onto
    // THIS process's rows, and one left behind would post another press's name
    // into this one.
    for (const k of Object.keys(S.pickers)) dropPicker(k);
    // The part set is this process's too, and the popover writes it through.
    S.partSet = null;
    S.partPop = null;
    // AND SO DOES THE SETTINGS DRAFT. openSettings is `S.settings ||
    // settingsDraft()`, so a draft left behind was shown against the process
    // just opened — its name, description, PLC, tag, counter and group, with
    // the bar already claiming "Unsaved changes" on a process nobody had
    // touched. Saving that renamed THIS process to the last one and took its
    // counter config and group with it, because the PUT writes every field it
    // decodes. Found by the audit; the door the tab click uses never reset it.
    S.settings = null;
    S.settingsError = '';
    S.settingsNotice = '';
    S.notice = '';
    root().innerHTML = appbar() + '<div class="pd-page"><div class="pd-dim" style="padding:24px">Loading…</div></div>';
    const res = await send('GET', composerURL(id));
    if (!res.ok) {
        root().innerHTML = appbar() + '<div class="pd-list"><p class="pd-dim">Could not read this process.</p></div>';
        return;
    }
    takeComposer(res.body);
    // S.stationNodes covers only the process the PAGE was loaded for, and this
    // is a single-page app — so every process reached by a click had an empty
    // map, and D4 drew "nothing claimed" against every HMI that had claims.
    // Read them for the process being opened.
    await refreshProcessStationNodes();
    const p = process();
    const h = hashOpts();
    // A LOADER'S PROCESS OPENS ON SETTINGS, the only tab it has.
    if (coreBoards(p)) {
        S.tab = 'settings';
        S.settings = settingsDraft();
        await openSettings();
        syncHash();
        return;
    }
    const first = Number(h.style) || (p && p.active_style_id) ||
        (S.composer.styles[0] && S.composer.styles[0].id) || 0;
    selectStyle(first);
    // #part=<position> opens that position's part popover, for the shots
    // harness and for a link that means "add a part here".
    if (h.part) {
        const chip = root().querySelector('.pd-postbl [data-act="pick"][data-kind="part"][data-node="' +
            CSS.escape(h.part) + '"]');
        if (chip) await openPartPop(chip, h.part);
    }
    // A PROCESS THAT CANNOT RUN YET OPENS ON SETUP, unless the link names
    // another tab or a style.
    if (h.tab === 'setup' || (!h.tab && !h.style && !h.newstyle && needsSetup())) { S.tab = 'setup'; drawSetup(); }
    else if (h.tab === 'screens') await openSettingsAt(coreBoards(p) ? 'Screens' : 'Positions and routing');
    else if (h.tab === 'presets' || h.tab === 'recipes') {
        await openPresets();
        // #tab=presets;preset=<id> opens the recipes (beside HMI setup on
        // Settings) with that row's member list
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
        // ;recipe=<id> opens that recipe as a draft, the rail row's click;
        // ;groups=1 then presses Set up groups, ;focus=<group> hovers a group,
        // ;row=<node> clicks a spot's row (recipe groups) — every state of
        // the engineer's editor a shot of the mock's tab 1 has to reach.
        if (h.recipe) openRecipeAt(h);
    }
    else if (h.tab === 'settings') {
        S.tab = 'settings';
        S.settings = settingsDraft();
        await openSettings();
    }
    // #newstyle=<recipe> opens + New style on that recipe, at its Spots step;
    // ;sel=<node> selects a place there, as the station's #compose ;sel= does.
    if (h.newstyle) openNewStyle(h.newstyle, h.sel);
    syncHash();
}

// openRecipeAt is the recipe draft's one-shots, in the order an engineer
// clicks them. Local state only: nothing here writes.
function openRecipeAt(h) {
    openRecipe(h.recipe);
    const d = S.shapeDraft;
    if (!d) return;
    if (h.groups && !d.groups) setUpGroups();
    if (!d.groups) return;
    const byName = name => d.groups.findIndex(g => g.name === name);
    if (h.focus && byName(h.focus) >= 0) { d.focus = { g: byName(h.focus) }; drawFlows(); }
    if (h.row) {
        const g = d.groups.findIndex(x => x.spots.some(s => s.core_node_name === h.row));
        if (g >= 0) { d.focus = { g: g, row: h.row }; drawFlows(); }
    }
}

// The list's Settings link: open the process, then land on the tab the
// engineer asked for rather than on Flows and a second click.
async function openProcessAt(id, tab) {
    await openProcess(id);
    if (tab !== 'settings') return;
    S.tab = 'settings';
    await openSettings();
    syncHash();
}

// #style=<id>;tab=settings opens a particular flow or another tab on load. It
// is how the shots harness photographs a state without driving a mouse, the
// same convention U8's composer states use, and it deep-links a review comment
// to the exact place being argued about.
function hashOpts() {
    const out = {};
    for (const part of String(window.location.hash || '').replace(/^#/, '').split(';')) {
        const i = part.indexOf('=');
        if (i > 0) out[part.slice(0, i)] = decodeURIComponent(part.slice(i + 1));
    }
    return out;
}

// AND THE URL FOLLOWS THE STATE, not the other way round. hashOpts reads once
// at boot; every navigation then restates where it landed, in the same
// vocabulary the templates mint (?process=) and this page reads (#style=,
// #tab=) — so a reload, a bookmark made after three clicks, or a link pasted
// out of the address bar lands on the same state. replaceState mints no
// history entries, so back and forward stay the browser's, not this page's.
// The boot-only one-shots (#part=, ;preset=, ;apply=, ;tick=,
// ;recipe= with ;groups= ;focus= ;row=, #newstyle= with ;sel=, and the
// list's #add=1) are
// consumed once and never re-minted: they exist so the shots harness can
// reach a state without driving a mouse, and re-writing them would pin every
// later click to a deep link nobody asked for.
function syncHash() {
    if (!window.history || !window.history.replaceState) return;
    if (!S.processID) {
        // The list owns no state: the bare path, so a reload stays a list.
        // (An empty URL would be a no-op and leave the last process's deep
        // link in the bar.) pathname is the page's own and always present in
        // a browser; the fallback only spares the stub.
        window.history.replaceState({}, '', window.location.pathname || '/processes');
        return;
    }
    let url = '?process=' + S.processID;
    if (S.tab === 'flows') {
        if (S.styleID) url += '#style=' + S.styleID;
    } else {
        url += '#tab=' + S.tab;
    }
    window.history.replaceState({}, '', url);
}

// A NIL GO SLICE MARSHALS TO null, NOT []. ComposerData.Styles has no
// omitempty, so a process with no parts — which is exactly the state an Add
// process lands in — answers `"styles": null`, and `S.composer.styles[0]` on
// that is the page throwing on the first screen after a create. Half the reads
// on this page already wrote `|| []` by hand; the other half indexed straight
// in, and which half a field fell into was down to who wrote it.
//
// So the read is normalised ONCE, here, and the page may then index. The two
// nested lists inside Cell get the same treatment for the same reason.
//
// THE MAP IS KEPT WHEN THE READ SAYS IT IS THE ONE HELD (map_same): the read
// quoted its revision (composerURL), and the server left it off.
function takeComposer(data) {
    if (data && data.map_same && S.heldMap) data.map = S.heldMap;
    S.heldMap = (data && data.map) || null;
    S.composer = data || {};
    S.composer.styles = S.composer.styles || [];
    S.composer.routing = S.composer.routing || [];
    S.composer.presets = S.composer.presets || [];
    S.composer.palette = S.composer.palette || [];
    S.composer.cell = S.composer.cell || {};
    S.composer.cell.positions = S.composer.cell.positions || [];
    S.composer.cell.groups = S.composer.cell.groups || {};
}

// composerURL is the composer read of process id. THE PLANT MAP IS THE
// EDGE'S, NOT THE PROCESS'S, and most of the read's bytes: a page that holds
// one quotes its revision (map_rev), and the reply leaves it off while it is
// still the Edge's (takeComposer keeps it), on this process or the next.
function composerURL(id) {
    const rev = S.heldMap && S.heldMap.revision;
    return '/api/processes/' + id + '/composer' + (rev ? '?map_rev=' + encodeURIComponent(rev) : '');
}

// composerStyle is one style of the open process, with the two derivable
// summaries filled in.
//
// THE DESKTOP NEVER HAS THEM FROM THE SERVER. `parts` and `claim_nodes` ride
// the picker scope, which is the station VIEW's block — this page reads the
// composer block alone, which carries the cells instead. styleFacts is the one
// derivation both surfaces use; without it `applyBlocker` reads every style as
// "no parts placed" and the Flows model opens with an empty part list.
function composerStyle(id) {
    const st = S.composer.styles.find(s => s.id === id) || null;
    return st ? Object.assign({}, st, M().styleFacts(st)) : null;
}

// routeOver is the route model over one ComposerMap, as the composer model
// asks it: (from, to) → the vertex names of the way, or null. sceneNet reads
// a map object once and keeps its network, so every style opened on one
// composer read walks one network.
function routeOver(map) {
    return map ? (from, to) => sceneRoute(map, from, to) : null;
}

// ONE INIT, EVERY STYLE THIS PAGE OPENS. D1 edits one style and the Presets
// tab's apply modal previews many, and a second init would be a second answer
// to "what is this style's flow" — which is the thing the shared model exists
// to prevent.
function initModel(st, opts) {
    const cell = S.composer.cell;
    return M().init({
        // A SHAPE DRAFT (Presets › New shape) is a recipe with no style and no
        // part; the model drops the part question for it.
        shape: !!(opts && opts.shape),
        styleId: st.id,
        styleName: st.name,
        // station_id is what keeps a row's move and pair inside its own
        // screen (positionChoices; B4: dropping it offered an unloader's
        // window to every press row).
        positions: (cell.positions || []).map(p => ({
            core_node_name: p.core_node_name, kind: p.kind, sequence: p.sequence, station_id: p.station_id || 0,
        })),
        routing: S.composer.routing,
        // Same count the station's card reads, so both surfaces say the same
        // sentence about an empty role rather than one saying it and one
        // drawing a heading over nothing.
        routingOff: S.composer.routing_off || {},
        // The key-route walk's route model: shared/scene-net.js over this
        // process's ComposerMap as this style opens (lead decision D9). No
        // map, no model, and the picker says why (pickerEmptyWord).
        route: routeOver(S.composer.map),
        claims: st.claims || [],
        parts: (st.parts || []).map(p => p.payload_code || p),
        // The process's part set — what a part picker OFFERS. Different from
        // `parts` above, which is what this style runs and what the
        // unplaced-part finding reads.
        palette: S.composer.palette || [],
        catid: st.catid || '',
        lastRun: st.last_run || null,
        flowspec: window.FLOWSPEC || null,
        groups: cell.groups || {},
        processClaims: S.composer.styles.reduce((all, x) => all.concat(x.claims || []), []),
        // What Set for this style reads. Read-only in the model: the draft's
        // own copy appears on a cell only once a setting is edited.
        advanced: st.advanced || {},
    });
}

function selectStyle(id) {
    // Opening a style leaves any shape draft: the draft is never saved by
    // navigating, only by naming it.
    S.shapeDraft = null;
    S.setAdds = {};
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
    S.flowStale = false;
    drawFlows();
    schedulePreview();
}

function drawFlows() {
    if (S.flowStale) rebuildStaleFlow();
    // WHERE THE ENGINEER WAS SURVIVES THE REDRAW, as on Settings: the write
    // below replaces both scrollers with fresh ones at the top, so a cell
    // edit far down the table would otherwise throw the page back up. The
    // column scrolls down; the table's box scrolls only sideways, and its
    // headings strip goes with it.
    const scrollers = [['.pd-main', 'scrollTop'], ['#pd-postbl', 'scrollLeft'], ['#pd-poshead', 'scrollLeft']];
    const wasAt = scrollers.map(([sel, axis]) => { const el = root().querySelector(sel); return el ? el[axis] : 0; });
    root().innerHTML = appbar() + '<div class="pd-page">' + rail() + main() + '</div>';
    fitPositionColumns();
    drawPicture();
    drawBar();
    markRecipeState();
    scrollers.forEach(([sel, axis], i) => { const el = root().querySelector(sel); if (el) el[axis] = wasAt[i]; });
    bindRailSearch();
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
    const tbl = root().querySelector('#pd-postbl table');
    const box = $('pd-postbl');
    const outer = $('pd-posbox');
    const bar = $('pd-bar');
    const pic = root().querySelector('.pd-pic');
    const col = root().querySelector('.pd-main');
    if (!tbl || !box || !bar || !pic || !col) return;
    const b = bar.getBoundingClientRect(), pb = pic.getBoundingClientRect(), bx = box.getBoundingClientRect();
    const ox = outer ? outer.getBoundingClientRect() : bx;
    const cb = col.getBoundingClientRect();
    const svgEl = pic.querySelector('#pd-svg');
    const svgRect = svgEl ? svgEl.getBoundingClientRect() : { width: 0, height: 0 };
    // The add control is the dashed row closing the table and the
    // paired-above line the last row: both have to be inside the positions
    // box and reachable by scrolling the column. Both are rendered geometry,
    // so both are measured.
    const add = root().querySelector('#pd-postbl .pd-addrowbtn');
    const paired = root().querySelector('#pd-postbl tr.paired');
    // A point's distance from the top of the column's scrolled content.
    const inColumn = y => Math.round(y - cb.top + col.scrollTop);
    const fit = {
        viewport: [Math.round(document.documentElement.clientWidth), Math.round(document.documentElement.clientHeight)],
        // T1: the table fits its box. Equal, not "close": one pixel of overflow
        // is a column an engineer cannot see.
        tableScrollW: Math.round(tbl.scrollWidth),
        tableClientW: Math.round(box.clientWidth),
        pageScrollW: Math.round(document.body.scrollWidth),
        // The bar pins inside the viewport, and the table sits under the
        // picture.
        barBottom: Math.round(b.bottom),
        barTop: Math.round(b.top),
        picBottom: Math.round(pb.bottom),
        tableTop: Math.round(ox.top),
        tableBottom: Math.round(ox.bottom),
        // The rows' box's own extent.
        scrollerBottom: Math.round(bx.bottom),
        // ONE VERTICAL SCROLL: the column scrolls, the rows' box does not.
        colScrollH: Math.round(col.scrollHeight),
        colClientH: Math.round(col.clientHeight),
        scrollerScrollH: Math.round(box.scrollHeight),
        scrollerClientH: Math.round(box.clientHeight),
        // THE PICTURE IS DRAWN 1:1, and this is how that is checked now.
        //
        // The property is a RELATION, not a constant: the viewBox equals the
        // svg's rendered size, whatever the picture's own height made it — a
        // picture laid out at one size and fitted by preserveAspectRatio into
        // another is the bug, and it scales cards and type together.
        picW: Math.round(svgRect.width),
        picH: Math.round(svgRect.height),
        // AND IT IS NEVER CLIPPED: the frame is at least the svg's height.
        picFrameH: Math.round(pic.clientHeight),
        viewBox: (svgEl || { getAttribute: () => '' }).getAttribute('viewBox') || '',
        hasAdd: !!add,
        hasPairedLine: !!paired,
    };
    if (add) {
        const ab = add.getBoundingClientRect();
        fit.addTop = Math.round(ab.top);
        fit.addBottom = Math.round(ab.bottom);
    }
    // HOW MANY POSITION ROWS ARE ON SCREEN AS THE PAGE OPENS, measured rather
    // than divided by an assumed row height: rows wholly between the headings
    // and the bar. `.pd-postbl td` says 32 px and a rendered row is taller —
    // the chips inside it are 26-28 and carry their own padding.
    const head = root().querySelector('#pd-poshead thead');
    const rows = root().querySelectorAll('#pd-postbl tbody tr[data-row]');
    if (head && rows.length) {
        const hr = head.getBoundingClientRect();
        fit.headerH = Math.round(hr.height);
        fit.rowH = Math.round(rows[0].getBoundingClientRect().height);
        const top = Math.max(cb.top, hr.bottom), bottom = Math.min(cb.bottom, b.top);
        fit.rowsVisible = [...rows].filter(r => {
            const rr = r.getBoundingClientRect();
            return rr.top >= top - 1 && rr.bottom <= bottom + 1;
        }).length;
    }
    if (paired) fit.pairedInColumn = inColumn(paired.getBoundingClientRect().bottom);
    document.body.dataset.desktopFit = JSON.stringify(fit);
}

function styleRow(st, isRunning) {
    const on = st.id === S.styleID;
    const modes = [...new Set(st.claim_modes || [])].map(m => M().modeLabels()[m] || m).join(' + ');
    const nodes = (st.claim_nodes || []).join('/');
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

// THE RAIL IS THE PROCESS'S TWO LISTS, RECIPES FIRST. A recipe is the spots
// and robot moves with no part; a style is a recipe with its parts on it. An
// engineer builds a recipe here — open one to change it, or + New recipe —
// and the styles under it are what the floor changes over to.
//
// A RECIPE IS OPTIONAL (owner ruling 5), so Recipes and + New recipe are not
// drawn until the process has one: the first is made beside HMI setup
// (hmiRecipes), or from a saved style's Save as recipe….
function rail() {
    const p = process();
    const running = p ? p.active_style_id : 0;
    // THROUGH styleFacts: the rail's sub-line is the flow summary, built from
    // `claim_nodes`, which rides the picker scope and not this page's read.
    // See composerStyle.
    const all = S.composer.styles.map(st => Object.assign({}, st, M().styleFacts(st)));
    const run = all.find(s => s.id === running);
    const rest = all.filter(s => s.id !== running);
    const recipes = S.composer.presets || [];
    return '<div class="pd-rail" id="pd-rail">' + searchBox('pd-railq', recipes.length
        ? 'find a recipe, part, CATID or style' : 'find a part, CATID or style') +
        '<div class="pd-rail-body" id="pd-rail-body">' +
        (recipes.length
            ? '<div class="pd-grp pd-lbl">Recipes · ' + recipes.length + '</div>' +
            recipes.map(recipeRow).join('') +
            '<div class="pd-railadd"><button class="pd-btn" data-act="new-recipe">+ New recipe</button></div>'
            : '') +
        '<div class="pd-grp pd-lbl">Styles · ' + all.length + '</div>' +
        (run ? styleRow(run, true) : '') +
        rest.map(s => styleRow(s, false)).join('') +
        '</div><div class="pd-railfoot"><button class="pd-btn" data-act="new-style">+ New style</button>' +
        '<span class="pd-dim">a part this process runs</span></div></div>';
}

// One recipe in the rail: its name, its swap and its spots. The one open as a
// draft is marked. A recipe naming a node the cell no longer has says so in
// the server's words (card.stale), the same the HMI shows on its card: the
// floor cannot set it up until it is re-saved here.
function recipeRow(r) {
    const on = !!(S.shapeDraft && S.shapeDraft.recipe && String(S.shapeDraft.recipe.id) === String(r.id));
    const mode = M().modeLabels()[r.mode] || r.mode || '';
    return '<div class="pd-row pd-recipe' + (on ? ' on' : '') + '" data-act="recipe-open" data-preset="' + esc(r.id) + '">' +
        '<div class="n">' + esc(r.name) + '</div>' +
        '<div class="s">' + esc([mode, r.where || ''].filter(Boolean).join(' · ')) + '</div>' +
        (r.stale || []).map(t => '<div class="s pd-warn">' + esc(t) + '</div>').join('') + '</div>';
}

// openRecipe puts a saved recipe in the positions table as a draft. Saving it
// under the same name makes its next version; the styles minted from the old
// one keep it as their provenance.
//
// A RECIPE THAT HAS GROUPS OPENS WITH ITS CARDS, as the desktop read carries
// them — a group's settings included, so a re-save keeps them (SYNTH-round2 row
// 4). One that has none opens without, and stays v0's until an engineer
// presses Set up groups (row 5: never generated).
function openRecipe(id) {
    const r = (S.composer.presets || []).find(x => String(x.id) === String(id));
    if (!r) return;
    startShapeDraft(Object.values(r.cells || {}), {
        recipe: { id: r.id, name: r.name },
        groups: (r.groups || []).length ? JSON.parse(JSON.stringify(r.groups)) : null,
    });
}

// + New recipe opens an empty recipe draft at once (owner, 2026-10-26: start
// with a fresh recipe and let them build from there). Its mode is picked at
// its first row, by the chooser an empty style uses (addFreePosition); each
// row takes what any added row takes, the routing set's defaults included.
// Nothing is stored until it is named.
//
// THE COMPOSER IS READ AGAIN FIRST. A row's defaults, and the sentence beside
// a field the routing set cannot fill (routing_gaps), come from it, and the
// page holds the one it opened with: a destination added in Settings since
// would not reach the row. One composer read per click.
async function openNewRecipe() {
    // A failed read opens the draft on the routing set the page holds.
    const res = await send('GET', composerURL(S.processID));
    if (res.ok) takeComposer(res.body);
    startShapeDraft([], {});
}

// THE RAIL FOLLOWS THE FLOOR. A style a station builds on its HMI lands in
// this process's read and nothing tells this page, so the rail re-reads its
// two lists while the Styles tab is in front — on focus and every ten seconds
// — and redraws itself alone when they changed: the draft, the table and the
// picture are the engineer's and are left exactly as they are.
async function refreshRail() {
    if (!S.processID || S.tab !== 'flows' || document.hidden) return;
    // The lists only: the full read carries the press picture and the whole
    // plant map, which this poll never draws.
    const res = await send('GET', '/api/processes/' + S.processID + '/composer/lists', undefined, NO_NOTICE);
    const data = res.ok ? res.body : null;
    if (!data || S.tab !== 'flows') return;
    const key = d => JSON.stringify([((d && d.styles) || []).map(x => [x.id, x.name, x.claim_count, x.last_run]),
        ((d && d.presets) || []).map(x => [x.id, x.name, x.where])]);
    if (key(data) === key(S.composer)) return;
    S.composer.styles = data.styles || [];
    S.composer.presets = data.presets || [];
    // THE LISTS ONLY: the search field keeps its text and focus, and the body
    // keeps where it was scrolled to.
    const body = $('pd-rail-body');
    if (!body) return;
    const scroll = body.scrollTop || 0;
    const fresh = rail();
    const at = fresh.indexOf('id="pd-rail-body">') + 'id="pd-rail-body">'.length;
    body.innerHTML = fresh.slice(at, fresh.lastIndexOf('</div><div class="pd-railfoot">'));
    body.scrollTop = scroll;
    const q = $('pd-railq');
    if (q && q.value) q.dispatchEvent(new Event('input'));
}

function bindRailSearch() {
    const q = $('pd-railq');
    if (!q) return;
    const filter = () => {
        const n = q.value.trim().toLowerCase();
        for (const r of root().querySelectorAll('.pd-row')) {
            r.hidden = !!n && r.textContent.toLowerCase().indexOf(n) < 0;
        }
    };
    q.addEventListener('input', filter);
}

// The model's own cell shape, in the shape renderFlowPicture draws — the same
// adapter the station uses, from the same place, over the folded cell.
function cellFromModel() { return M().pictureCells(S.model, foldedCell(draftCell(), S.model)); }

// foldedCell is the cell the picture draws. FREE POSITIONS FOLD (owner,
// 2026-11-14, as the Stop 1 render drew it): the positions the style uses
// and the ones its rows name (partners, staging), and the rest behind "N more
// positions on this process are free · show them" (freeLine). On a 23-position
// press running one position that was 21 empty boxes. A flow that uses
// nothing yet draws them all: there is nothing to fold to.
function foldedCell(base, model) {
    if (S.showFree || !model) return base;
    const used = usedPositions(model);
    if (!used.size) return base;
    const positions = (base.positions || []).filter(p => used.has(p.core_node_name));
    return positions.length === (base.positions || []).length ? base : Object.assign({}, base, { positions: positions });
}

// freeLine is the fold's line under the picture: how many positions the
// picture leaves out, and the one control that shows them (or folds them back).
function freeLine() {
    const free = freePositions().length;
    if (!free || !usedPositions(S.model).size) return '';
    return '<div class="pd-freeline">' + (S.showFree
        ? esc(nOf(free, 'free position')) + ' shown · <button class="pd-dimlink" data-act="free-toggle">fold them</button>'
        : esc(free + ' more position' + (free === 1 ? '' : 's') + ' on this process ' + (free === 1 ? 'is' : 'are') + ' free') +
            ' · <button class="pd-dimlink" data-act="free-toggle">show them</button>') + '</div>';
}

// draftCell is the cell a draft is drawn over: the process's whole cell, or —
// in a recipe draft — the recipe's own station's (SYNTH round 2 row 23).
//
// A RECIPE IS ONE STATION'S. The composer read's cell is process-wide, so a
// press whose process also holds an unloader station carries that station's
// windows from across the plant, and drawn by relation they put far columns
// on the recipe's picture. The ★ heads — the draft's own
// cells — name the station; its positions and the stationless ones (the back
// and staging slots a press shares) are the recipe's picture. A filter over
// the read the page already holds: no request.
function draftCell() {
    if (!S.shapeDraft || !S.model) return S.composer.cell || { positions: [] };
    return stationCellOf(M().toCells(S.model).map(c => c.core_node_name));
}

// stationCellOf is the process's cell cut to the stations of these heads, and
// the stationless positions beside them: a recipe's own picture. The New
// style and Use a recipe sheets draw a recipe's shape over the same cut.
function stationCellOf(heads) {
    const base = S.composer.cell || { positions: [] };
    const station = {};
    for (const p of (base.positions || [])) station[p.core_node_name] = p.station_id || 0;
    const ids = new Set((heads || []).map(n => station[n]).filter(Boolean));
    if (!ids.size) return base;
    return Object.assign({}, base, {
        positions: (base.positions || []).filter(p => !p.station_id || ids.has(p.station_id)),
    });
}

// pictureOpts is how this page asks for the picture: by relation when the
// process has a recipe — the floor sets this cell up, so the engineer sees the
// picture the floor sees — or the page is drawing one (a recipe is a
// changeover's by definition), and otherwise as the scope gate decides.
//
// A DRAFT WITH GROUPS HOLDS ITS GRID BY THEIR PAIRS (the lead's D11): an
// on-deck spot is laid out by every spot its groups let it feed, not by the
// ★ cell alone, and the pairs come from the groups, never from the focus, so
// hovering never re-flows the grid. At Press 6 PLN_005 feeds PLN_006 and
// PLN_010, which stand in two rows, so it takes the middle one, as the mock
// draws it. With no groups the picture's own claims decide, as before.
// deskSentences is the picture's card words on the desktop (F27, R3): a pair's
// card says its field's name over "for <row>", as the staging cards and the
// table's heading do, where the model's words, the HMI's, say "on deck for"
// on a press index and "paired with" on a sequential.
function deskSentences(sentences) {
    if (!sentences || !sentences.cardLines) return sentences;
    const word = M().fieldLabel(S.model || { flowspec: window.FLOWSPEC || null }, 'paired_core_node');
    for (const n of Object.keys(sentences.cardLines)) {
        const lines = sentences.cardLines[n] || [];
        const m = lines.length === 1 && /^(?:on deck for|paired with) (.+)$/.exec(lines[0]);
        if (m) sentences.cardLines[n] = [word, 'for ' + m[1]];
    }
    return sentences;
}

function pictureOpts() {
    const o = (((S.composer && S.composer.presets) || []).length || S.shapeDraft) ? { relation: true } : {};
    const pairs = groupPairs();
    if (pairs) o.pairs = pairs;
    return o;
}

// PICTURE_H is the height of a frame the picture reported nothing for — an
// empty cell, whose one line of text sits inside it.
const PICTURE_H = 430;
const PICTURE_W_FALLBACK = 1084;   // the main column at the spec's 1440, for a frame not laid out yet

// pass is the re-measure guard — see the note at the foot of this function.
function drawPicture(pass) {
    const svg = $('pd-svg');
    if (!svg || !S.model) return;
    // THE PICTURE IS DRAWN INTO THE FRAME'S WIDTH AT ITS OWN SIZE. The
    // renderer lays out in absolute pixels — the module picture wraps at the
    // width it is given, the relation picture centres in it — so the width
    // asked for is the frame's (.pd-pic, the svg's parent — the svg's own
    // width is whatever the last draw set). The size is not the frame's to
    // choose: the picture reports it through opts.height and opts.width (wider
    // than the frame when a relation grid is), and the svg is set to exactly
    // that size with a viewBox to match, so a card is CARD_W wide on screen
    // and nothing is ever scaled to fit. The frame takes that height whole
    // (.pd-pic does not shrink) and scrolls a wider picture sideways; when it
    // and the table come to more than the window, the main column scrolls.
    const frameEl = svg.parentNode;
    const w = Math.round((frameEl && frameEl.clientWidth) || PICTURE_W_FALLBACK);
    const fs = M().findings(S.model);
    const byNode = {};
    for (const f of fs) if (f.node && !byNode[f.node]) byNode[f.node] = f.short;
    const p = process();
    // A DRAFT WITH GROUPS DRAWS THE GROUPS (groupPicture): every spot they
    // list as a card the floor could run, faded, tagged and given its
    // chevrons by what the engineer is looking at.
    const gp = groupPicture();
    const cell = gp ? gp.cell : cellFromModel();
    const o = Object.assign({
        selected: S.selected, findings: byNode, editable: true, frame: { w: w },
    }, pictureOpts(), gp ? gp.opts : {});
    o.sentences = gp ? gp.sentences : deskSentences(sentencesFromModel(S.model, !!relationPlacement(cell, o)));
    const markup = renderFlowPicture({ cell: cell, station: { name: p ? p.name : '' } }, o);
    const h = Math.round(o.height || PICTURE_H);
    // A RELATION GRID WIDER THAN THE COLUMN IS DRAWN WIDER, never shrunk: the
    // svg takes the picture's own width and .pd-pic scrolls it sideways.
    const pw = Math.round(o.width || w);

    // AN UNCHANGED PICTURE IS NOT REDRAWN, and that is what stops the pulse.
    //
    // Selecting a style draws the picture, and 400 ms later the preview lands
    // and draws it AGAIN — `applyPreview` only sets model.preview, so the
    // cells, the geometry and almost always the findings are identical and the
    // second render produces the same bytes. Assigning them anyway replaces
    // every node in the SVG, which the browser repaints: on the floor that
    // reads as the picture flashing and re-fitting on every click, twice per
    // style. The same happens on every resize event.
    //
    // So the render is idempotent at the DOM: build the markup, compare, and
    // only write when it actually differs. A preview that DOES bring a finding
    // still redraws — once, because it changed something.
    // THE CACHE BELONGS TO THE ELEMENT, NOT TO THE PAGE, and holding it on S
    // was a blank picture waiting to happen — which is what it did.
    //
    // drawFlows rebuilds the whole tab, so the #pd-svg that drawPicture writes
    // into is a BRAND NEW, EMPTY element. Selecting a card the page already has
    // selected redraws the tab and produces the same markup as last time, so a
    // cache kept on S said "unchanged, skip" and left the new element empty:
    // click a position once and it outlines, click it again and the picture
    // vanishes. Reported from the floor as "clicked again, whole screen blank".
    //
    // Hung off the node, a fresh element simply has no cache and is always
    // written; only a redraw of the SAME element with the same bytes is
    // skipped, which is the case this is for.
    const viewBox = '0 0 ' + pw + ' ' + h;
    if (svg.getAttribute('viewBox') !== viewBox) {
        svg.setAttribute('viewBox', viewBox);
        svg.setAttribute('width', String(pw));
        svg.setAttribute('height', String(h));
    }
    if (markup !== svg.pdMarkup) {
        svg.innerHTML = markup;
        svg.pdMarkup = markup;
    }

    // THE WIDTH IS CHECKED ONCE MORE, ON THE NEXT FRAME. Setting the svg's
    // height can change the frame's width: the flow column scrolls
    // (.pd-main is overflow-y: auto), and a picture that makes it overflow
    // brings in its scrollbar, which narrows every frame inside it where the
    // browser does not honour the column's reserved gutter. The draw above was laid out at the width from before that, so the
    // picture would sit a scrollbar's width wider than its frame.
    //
    // requestAnimationFrame runs AFTER layout and BEFORE paint, so the
    // corrected layout lands in the very frame the engineer first sees: no
    // snap. `pass` bounds it to one correction — the width cannot move again
    // once the scrollbar is in.
    if (!pass) {
        requestAnimationFrame(() => {
            // The tab may have been redrawn out from under this frame, and
            // writing into a detached element would be drawing on nothing.
            const el = $('pd-svg');
            if (!el || el !== svg || !el.parentNode) return;
            if (Math.round(el.parentNode.clientWidth || w) !== w) drawPicture(1);
        });
    }
    reportDesktopFit();
    markRecipeState();
}

// A PROCESS WITH NO PART IS THE STATE A CREATE LANDS IN, and until U10 there
// was no way to reach it, so nothing drew it: selectStyle(0) fell through to
// drawFlows and positionsTable read S.model.positions off a null model. The
// page threw on the first screen an engineer saw after making a press.
//
// The empty state says the one thing that is true — a press runs parts, and
// this one has none yet — and points at the button that adds one, which is
// already in the rail beside it.
// STYLE FIRST (F2, owner ruling 5): a recipe is optional, so the empty state
// asks for the first style and draws nothing about recipes.
function noStyleYet() {
    return '<div class="pd-main"><div class="pd-head"><div class="t"><h1>No style yet</h1>' +
        '<div class="sub">name the first style, then draw its flow</div></div>' +
        '<div class="act"><button class="pd-btn primary" data-act="new-style">+ New style</button></div></div></div>';
}

function main() {
    if (!S.model) return noStyleYet();
    if (S.shapeDraft) return shapeDraftMain();
    const st = composerStyle(S.styleID) || { name: '', claim_count: 0 };
    const p = process();
    const isRunning = !!(p && p.active_style_id === S.styleID);
    // THE SUBTITLE IS THE DRAFT'S. It was read off the saved style, so a mode
    // changed in the table below left the head naming the old one until the
    // flow was saved: two answers to "what is this flow" on one screen. Only
    // `last run` is the style's, because a draft has never run. The running
    // style with no changeover on record says nothing of its runs: "never run
    // here" beside its running pill was two answers again (SPR Press 6, whose
    // style was set running without a changeover).
    const cells = M().toCells(S.model);
    const modes = [...new Set(cells.map(c => c.swap_mode))].map(m => M().modeLabels()[m] || m).join(' + ');
    const sub = [
        modes || 'no flow yet',
        cells.length + ' position' + (cells.length === 1 ? '' : 's'),
        st.last_run ? 'last run ' + st.last_run : isRunning ? '' : 'never run here',
    ].filter(Boolean).join(' · ');
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
        // SAVE AS PRESET IS MADE FROM THE SAVED FLOW, so it is disabled while
        // the draft is dirty: a preset named from an unsaved draft would be a
        // shape the station does not run, under a name that claims it does.
        // drawBar() enables it on the same measurement that disables Save style.
        '<button class="pd-btn" data-act="save-preset" disabled>Save as recipe…</button>' +
        // USE A PRESET, ON THE SCREEN THAT EDITS A FLOW (owner: "is presets
        // pickable on both"). The HMI's strip has offered the cell's named
        // shapes since U10 and the desktop had them only on the Presets tab,
        // behind a row's ⋯ menu, as "Apply to parts…" — which is the same act
        // approached from the other end (pick a shape, then tick the parts).
        // An engineer with one style open wanted the near end.
        //
        // A THIRD CALLER, NOT A THIRD IMPLEMENTATION. It applies through the
        // model's own applyPreset and shows the model's own shapeDiff — the
        // same reducer the HMI dispatches and the same diff the apply modal
        // draws. A second reducer would be a second answer to "what does this
        // shape do to this flow".
        ((S.composer.presets || []).length ? '<button class="pd-btn" data-act="use-preset">Use a recipe ▾</button>' : '') +
        '<button class="pd-btn primary" data-act="save" disabled>Save style</button></div></div>' +
        (S.notice ? '<div class="pd-notice">' + esc(S.notice) + '</div>' : '') +
        '<div class="pd-pic"><svg id="pd-svg" class="os-flow-picture"></svg></div>' + freeLine() +
        positionsTable() + styleSettings() +
        '<div class="pd-bar" id="pd-bar"></div><div class="pd-pop" id="pd-pop" hidden></div></div>';
}

// shapeDraftMain is main() for a New shape draft: the same picture and table,
// a header that says it is a recipe, and the two acts a draft has — leave, or
// name it. The role question is asked only when no claim on the process says
// which positions make parts (the generator's note); the Role chip on each row
// is the answer.
function shapeDraftMain() {
    // The mode its rows have: none until the first row picks one.
    const label = M().modeLabels()[defaultModeFor()] || '';
    const r = S.shapeDraft.recipe;
    const title = r ? 'Recipe · ' + r.name : label ? 'New recipe · ' + label : 'New recipe';
    const sub = r ? 'spots, how they swap, where bins come from and go; no part. Saved under the same ' +
        'name it becomes the next version' :
        'a recipe — spots, how they swap, where bins come from and go; no part, and nothing is saved until you name it';
    return '<div class="pd-main">' +
        '<div class="pd-head"><div class="t"><h1>' + esc(title) + '</h1>' +
        '<div class="sub">' + esc(sub) + '</div></div>' +
        '<div class="act"><button class="pd-btn quiet" data-act="shape-cancel">Back</button>' +
        '<button class="pd-btn primary" data-act="shape-save">Save recipe…</button></div></div>' +
        (S.notice ? '<div class="pd-notice">' + esc(S.notice) + '</div>' : '') +
        '<div class="pd-pic"><svg id="pd-svg" class="os-flow-picture"></svg></div>' + freeLine() +
        positionsTable() + groupsSection() +
        '<div class="pd-bar" id="pd-bar"></div><div class="pd-pop" id="pd-pop" hidden></div></div>';
}

// ── recipe groups: the engineer's editor (SYNTH-round2 rows 2-4, 9, 10, 18) ──
//
// AN ENGINEER SETS GROUPS ON A RECIPE: Left Hand and Right Hand on a two-off,
// the press spots each part may run on, the on-deck spots allowed for each,
// and what is set for each group; the floor's Spots step then offers that and
// nothing else (owner, 2026-10-22). The editor is this draft, not a screen of
// its own: the positions table above stays the ★ cells' editor and the plain
// path's (O-11), and the cards under it are the groups.
//
// TWO COPIES THE SAVE KEEPS ONE (row 2). A group is of its slot's ★ cell and
// its first spot is that cell's head, with the cell's own attached nodes in
// its lists; the server refuses every way the two could disagree, by name,
// and this page shows the refusal as the server words it. syncGroups keeps
// the cards on the cells as the table changes them, so most of those
// refusals cannot be reached from here.
//
// A GROUP'S ROUTING IS ITS ★ CELL'S (row 3): the card's Inbound source and Outbound destination
// are that cell's own pickers, so a card and its table row cannot disagree.
//
// NOTHING IS GENERATED (row 5): a recipe without groups has none until an
// engineer presses Set up groups, and none are stored until Save recipe.

// The four lists a group spot carries, by the claim's own field names (owner
// ruling F3), each beside the ★ cell field it widens.
const GROUP_FIELDS = ['paired_core_node', 'second_paired_core_node', 'inbound_staging', 'outbound_staging'];
const DECK_FIELDS = ['paired_core_node', 'second_paired_core_node'];

// What a group's Advanced may say: FlowAdvanced less the part fields, the
// loader board's order and the carry-over (domain.RecipeAdvanced), in that
// type's order. The press hardware — which robot fetches the empty — is IN
// (2026-11-05): without it every style the floor built from a recipe minted
// Robot 1. The carry-over is OUT: it is a Tool change position's, and a group
// marks none.
const GROUP_ADV_KEYS = ['reorder_point', 'auto_reorder', 'lineside_soft_threshold', 'auto_push',
    'keep_staged_node', 'refill_by', 'auto_confirm', 'curtain_required', 'strict_pack', 'pack_tolerance_pct'];

// listWords is a list's column heading, its add chip and the noun "On the
// floor" and the running-style line say it with, per mode.
function listWords(field, mode) {
    switch (field) {
        case 'paired_core_node':
            return mode === 'sequential'
                ? { head: 'Pair (B side)', add: '+ B side', noun: 'with' }
                : { head: 'On deck (can feed it)', add: '+ on deck', noun: 'on deck' };
        case 'second_paired_core_node': return { head: 'Third', add: '+ third', noun: 'third' };
        case 'inbound_staging': return { head: mode === 'two_robot' ? 'Staging lane (in)' : 'Lane in', add: '+ lane', noun: 'lane' };
        default: return { head: 'Lane out', add: '+ lane', noun: 'lane out' };
    }
}

// spotWords is the spot column's heading and its add button.
function spotWords(mode) {
    if (mode === 'two_robot_press_index') return { head: 'Press spot', add: '+ Add a press spot' };
    if (mode === 'sequential') return { head: 'Pair (A side)', add: '+ Add a pair' };
    return { head: 'Spot', add: '+ Add a spot' };
}

// starLists is the ★ cell's own attached places: one entry per list field
// flowspec lets its mode and role have AND the cell fills — the model's own
// rowColumns, so the fields a card offers are the ones the table draws. A
// field the ★ cell leaves blank gets no column: the server refuses a list
// there ("set one on PLN_006's row first"), and a control whose every value
// is refused is not one to hand an engineer.
function starLists(head) {
    if (!S.model || !S.model.cells[head]) return [];
    return M().rowColumns(S.model, head, 'partner').concat(M().rowColumns(S.model, head, 'staging'))
        .filter(ch => ch.value);
}

// draftSlots is the draft's parts, in cell order: domain.PayloadSlots' rule
// over the draft's own cells — a cell flowspec says takes a part is a slot,
// and a mutual Sequential pair is one slot over both. The saved card ships the
// same list as `slots`; this one follows the table as the engineer edits it.
function draftSlots() {
    const cells = M().toCells(S.model);
    const by = {};
    for (const c of cells) by[c.core_node_name] = c;
    const taken = new Set();
    const out = [];
    for (const c of cells) {
        const n = c.core_node_name;
        if (taken.has(n) || !M().fieldRequired(S.model, n, 'payload_code')) continue;
        const slot = { nodes: [n], role: c.role, mode: c.swap_mode };
        taken.add(n);
        const p = by[c.paired_core_node];
        if (p && p.paired_core_node === n && !taken.has(p.core_node_name)) {
            slot.nodes.push(p.core_node_name);
            taken.add(p.core_node_name);
        }
        out.push(slot);
    }
    return out;
}

// newGroup is one slot's card as Set up groups lays it out: its ★ row and the
// ★ cell's own attached places in its lists.
function newGroup(head, name) {
    const spot = { core_node_name: head };
    for (const ch of starLists(head)) spot[ch.field] = [ch.value];
    return { of: head, name: name, spots: [spot] };
}

// groupNames pre-fills the cards' names. TWO PRODUCE SLOTS ARE LEFT HAND AND
// RIGHT HAND, the left the one the picture draws further west (by relation,
// the floor's own left; slot order when the cell is off the map). Any other
// slot is a letter, A, B, C in the cards' order: NOT ITS SWAP MODE (F32),
// which named a part "2-Robot Swap". Every name is the engineer's to change:
// the floor asks for each part by it.
function groupNames(slots) {
    if (slots.length === 2 && slots.every(s => s.role === 'produce')) {
        const lay = M().relationLayout(cellFromModel(), {});
        const col = n => (lay.cells && lay.cells[n] ? lay.cells[n].c : null);
        const a = col(slots[0].nodes[0]), b = col(slots[1].nodes[0]);
        return a !== null && b !== null && b < a ? ['Right Hand', 'Left Hand'] : ['Left Hand', 'Right Hand'];
    }
    return slots.map((s, i) => (i < 26 ? String.fromCharCode(65 + i) : 'Group ' + (i + 1)));
}

// setUpGroups is the button: one card per slot, pre-filled, nothing saved.
// The cards stand in the picture's order, left first.
function setUpGroups() {
    const d = S.shapeDraft;
    if (!d || d.groups || !S.model) return;
    const slots = draftSlots();
    const names = groupNames(slots);
    const cards = slots.map((s, i) => newGroup(s.nodes[0], names[i]));
    d.groups = names[0] === 'Right Hand' ? cards.reverse() : cards;
    d.focus = null;
    d.hover = null;
    d.refusal = '';
    drawFlows();
}

// syncGroups keeps the cards on the draft's cells as the positions table
// changes them: each group stays of its slot's ★ head, and a group whose head
// the table moved follows it to the head no group is of (the old head stays in
// the group as one of its spots); its ★ row holds the cell's own attached
// places at the front of its lists. A slot with no card gets one, pre-filled;
// a card whose slot is gone goes with it — the server would refuse a group
// count that is not the slot count.
function syncGroups() {
    const d = S.shapeDraft;
    if (!d || !d.groups || !S.model) return;
    const heads = draftSlots().map(s => s.nodes[0]);
    const free = heads.filter(h => !d.groups.some(g => g.of === h));
    const out = [];
    for (const g of d.groups) {
        if (heads.indexOf(g.of) < 0) {
            if (!free.length) continue;
            g.of = free.shift();
        }
        out.push(g);
    }
    for (const h of free) {
        // The first letter no card has, as groupNames names them (F32: never
        // the swap mode).
        const taken = new Set(out.map(g => g.name));
        let k = 0;
        while (taken.has(k < 26 ? String.fromCharCode(65 + k) : 'Group ' + (k + 1))) k++;
        out.push(newGroup(h, k < 26 ? String.fromCharCode(65 + k) : 'Group ' + (k + 1)));
    }
    for (const g of out) {
        g.spots = g.spots || [];
        if (!g.spots.length || g.spots[0].core_node_name !== g.of) {
            const own = g.spots.find(s => s.core_node_name === g.of);
            g.spots = [own || { core_node_name: g.of }].concat(g.spots.filter(s => s.core_node_name !== g.of));
        }
        const star = g.spots[0];
        for (const ch of starLists(g.of)) {
            const list = star[ch.field] || [];
            if (list.indexOf(ch.value) < 0) star[ch.field] = [ch.value].concat(list);
        }
    }
    d.groups = out;
    if (d.focus && !out[d.focus.g]) d.focus = null;
    if (d.hover !== null && d.hover !== undefined && !out[d.hover]) d.hover = null;
}

// groupsBody is the save's `groups`: the cards in the server's own shape —
// of, name, spots with their non-empty lists in field order, and an Advanced
// only when it says something (domain.RecipeAdvanced omits every field at its
// zero). Read the same way off a saved card to tell whether the draft changed.
function groupsBody(groups) {
    return (groups || []).map(g => {
        const out = { of: g.of, name: String(g.name || '').trim(), spots: (g.spots || []).map(s => {
            const spot = { core_node_name: s.core_node_name };
            for (const f of GROUP_FIELDS) if ((s[f] || []).length) spot[f] = s[f].slice();
            return spot;
        }) };
        const a = recipeAdvanced(g.advanced || {}, g.of);
        if (Object.keys(a).length) out.advanced = a;
        return out;
    });
}

// recipeAdvanced is a group's Advanced as a recipe may say it: GROUP_ADV_KEYS
// that carry a value, by domain.ClaimHas's rule (the refill's Robot 1 is no
// opinion), and only those flowspec draws for the ★ cell's mode and role —
// the server refuses the others (D26).
function recipeAdvanced(a, head) {
    const out = {};
    for (const key of GROUP_ADV_KEYS) {
        const v = a[key];
        const set = key === 'refill_by' ? !!v && v !== 'clearing_robot'
            : Array.isArray(v) ? v.length > 0 : Boolean(v);
        if (!set) continue;
        if (head && S.model && S.model.cells[head] && !M().advancedShows(S.model, head, key)) continue;
        out[key] = v;
    }
    return out;
}

// groupNodes is every node a group names: its spots and their lists.
function groupNodes(g) {
    const out = [];
    for (const s of (g && g.spots) || []) {
        out.push(s.core_node_name);
        for (const f of GROUP_FIELDS) for (const v of s[f] || []) out.push(v);
    }
    return [...new Set(out)];
}

// groupFocus is what the engineer is looking at: a clicked row, else a group
// pinned (its tag, or ;focus=), else the card under the pointer, else nothing.
function groupFocus() {
    const d = S.shapeDraft;
    if (!d || !d.groups) return null;
    if (d.focus && d.groups[d.focus.g]) return d.focus;
    if (d.hover !== null && d.hover !== undefined && d.groups[d.hover]) return { g: d.hover };
    return null;
}

// groupPairs is the picture's deck pairing for a draft with groups: every
// press spot whose list holds a deck, and the ★ cell that names it — held
// fixed by the groups, so a focus never re-flows the grid (see pictureOpts).
// Only a press index's decks: a Sequential pair's B side is a spot of its own.
function groupPairs() {
    const d = S.shapeDraft;
    if (!d || !d.groups || !S.model) return null;
    const cells = M().toCells(S.model);
    const heads = new Set(cells.map(c => c.core_node_name));
    const out = {};
    const add = (deck, spot) => {
        if (!deck || heads.has(deck)) return;
        out[deck] = out[deck] || [];
        if (out[deck].indexOf(spot) < 0) out[deck].push(spot);
    };
    for (const c of cells) {
        if (c.swap_mode === 'sequential') continue;
        for (const f of DECK_FIELDS) add(c[f], c.core_node_name);
    }
    for (const g of d.groups) {
        if ((S.model.cells[g.of] || {}).mode === 'sequential') continue;
        for (const s of g.spots) for (const f of DECK_FIELDS) for (const v of s[f] || []) add(v, s.core_node_name);
    }
    return out;
}

// groupPicture is the draft's picture once it has groups (SYNTH row 18):
//
//   - every spot a group lists is drawn as the floor could run it: the ★
//     cell's claim laid on it, with the first node of each of its lists;
//   - no focus: every node any group names is full, tagged, and no chevron
//     is drawn; a group hovered: its spots and lists, with their chevrons; a
//     row clicked: that spot and its lists, with its chevrons;
//   - the L/R tag is the group's first letter, on its spots' cards only (the
//     mock's tab 1), and a card says whose it is: "Left Hand · ★ default" on
//     the ★ spot, "feeds PLN_006 · PLN_010" on a deck, the recipe a spot no
//     group holds runs in.
//
// The claims are the model's own (reduce over the draft), so the legs, the
// robots and the dock read as the floor would see them; nothing here is saved.
function groupPicture() {
    const d = S.shapeDraft;
    if (!d || !d.groups || !S.model) return null;
    syncGroups();
    let m = S.model;
    for (const g of d.groups) {
        const star = m.cells[g.of];
        if (!star || !star.on || !star.mode) continue;
        for (const s of g.spots.slice(1)) m = withGroupSpot(m, star, s);
    }
    const heads = (g, s) => {
        const out = [s.core_node_name];
        if ((S.model.cells[g.of] || {}).mode === 'sequential' && (s.paired_core_node || [])[0]) out.push(s.paired_core_node[0]);
        return out;
    };
    const f = groupFocus();
    let active, chevrons;
    if (!f) {
        active = [...new Set([].concat(...d.groups.map(groupNodes)))];
        chevrons = [];
    } else if (f.row) {
        const g = d.groups[f.g];
        const s = g.spots.find(x => x.core_node_name === f.row) || g.spots[0];
        active = groupNodes({ spots: [s] });
        chevrons = heads(g, s);
    } else {
        const g = d.groups[f.g];
        active = groupNodes(g);
        chevrons = [].concat(...g.spots.map(s => heads(g, s)));
    }
    const tags = {};
    const lines = {};
    const feeds = {};
    d.groups.forEach((g, i) => {
        const tag = { letter: (String(g.name || '').trim()[0] || '?').toUpperCase(), group: (i % 2) + 1 };
        const seq = (S.model.cells[g.of] || {}).mode === 'sequential';
        g.spots.forEach((s, k) => {
            for (const n of heads(g, s)) {
                tags[n] = tag;
                lines[n] = [g.name + (k === 0 ? ' · ★ default' : '')];
            }
            if (seq) return;
            for (const fd of DECK_FIELDS) for (const v of s[fd] || []) (feeds[v] = feeds[v] || []).push(s.core_node_name);
        });
    });
    for (const v of Object.keys(feeds)) if (!lines[v]) lines[v] = ['feeds ' + feeds[v].join(' · ')];
    const cell = M().pictureCells(m, foldedCell(draftCell(), m));
    const here = new Set((cell.positions || []).map(p => p.core_node_name));
    for (const r of S.composer.presets || []) {
        if (d.recipe && String(r.id) === String(d.recipe.id)) continue;
        for (const n of Object.keys(r.cells || {})) if (here.has(n) && !lines[n]) lines[n] = ['in ' + r.name];
    }
    const sentences = deskSentences(sentencesFromModel(m, !!relationPlacement(cell, pictureOpts())));
    if (sentences) sentences.cardLines = Object.assign({}, sentences.cardLines, lines);
    return { model: m, cell: cell, sentences: sentences, opts: { active: active, tags: tags, chevrons: chevrons } };
}

// withGroupSpot lays a group's ★ claim on one of its other spots, in the
// model, with the first node of each of the spot's lists.
function withGroupSpot(m, star, s) {
    const n = s.core_node_name;
    if (!m.cells[n] || m.cells[n].on) return m;
    const first = f => ((s[f] || [])[0]) || '';
    const lay = (state, node, partner) => {
        let x = M().reduce(state, { type: 'addPosition', node: node, mode: star.mode });
        x = M().reduce(x, { type: 'setRole', node: node, role: star.role });
        x = M().reduce(x, { type: 'setPartner', node: node, partner: partner });
        x = M().reduce(x, { type: 'setSource', node: node, source: star.source });
        return M().reduce(x, { type: 'setDest', node: node, dest: star.dest });
    };
    m = lay(m, n, first('paired_core_node'));
    m = M().reduce(m, { type: 'setSecondPartner', node: n, partner: first('second_paired_core_node') });
    m = M().reduce(m, { type: 'setStaging', node: n, staging: first('inbound_staging') });
    m = M().reduce(m, { type: 'setParkOld', node: n, staging: first('outbound_staging') });
    const b = first('paired_core_node');
    if (star.mode === 'sequential' && b && m.cells[b] && !m.cells[b].on) m = lay(m, b, n);
    return m;
}

// The group tag, as the picture draws it, beside a card's name.
function groupTagHTML(g, i) {
    return '<span class="pd-gtag g' + ((i % 2) + 1) + '">' + esc((String(g.name || '').trim()[0] || '?').toUpperCase()) + '</span>';
}

// groupsSection is the draft's groups: the Set up groups button, or a card per
// slot and the "On the floor" block under them.
function groupsSection() {
    const d = S.shapeDraft;
    if (!d) return '';
    if (!d.groups) {
        return '<div class="pd-groups" id="pd-groups"><div class="pd-grpbar">' +
            '<button class="pd-btn" data-act="groups-setup">Set up groups</button>' +
            '<span class="pd-dim">a card per part: the spots it may run on, what may serve each one, ' +
            'and what is set for it. Nothing is saved until Save recipe.</span></div></div>';
    }
    syncGroups();
    return '<div class="pd-groups" id="pd-groups">' +
        '<div class="pd-lbl">Groups<span class="hint">hover a group, or click a spot’s row, to see it on the picture</span></div>' +
        (d.refusal ? '<div class="pd-refusal" id="pd-grefusal">' + esc(d.refusal) + '</div>' : '') +
        '<div class="pd-grps">' + d.groups.map(groupCard).join('') + '</div>' + floorBlock() + '</div>';
}

function groupCard(g, i) {
    const f = groupFocus();
    const star = S.model.cells[g.of] || {};
    const mode = star.mode;
    const lists = starLists(g.of);
    const words = lists.map(ch => listWords(ch.field, mode));
    const sw = spotWords(mode);
    const rows = g.spots.map((s, k) => {
        const n = s.core_node_name;
        const cells = lists.map((ch, j) => {
            const list = s[ch.field] || [];
            const chips = list.map(v => (k === 0 && v === ch.value)
                // THE ★ CELL'S OWN PLACE IS READ-ONLY HERE: the server refuses
                // a ★ row without it, and the table is where it changes.
                ? '<span class="pd-chip set lock" title="' + esc(g.of + '’s own in the recipe — change it on its row in the table') + '">' +
                    esc(v) + '</span>'
                : '<button class="pd-chip set" data-act="glist-rm" data-g="' + i + '" data-node="' + esc(n) + '" data-field="' + ch.field +
                    '" data-v="' + esc(v) + '" title="Take ' + esc(v) + ' out of this list">' + esc(v) + ' <i>&times;</i></button>').join('');
            // A PAIR IS TWO: a Sequential spot takes one B side.
            const full = mode === 'sequential' && ch.field === 'paired_core_node' && list.length > 0;
            return '<td>' + chips + (full ? '' : '<button class="pd-chip add" data-act="glist-add" data-g="' + i +
                '" data-node="' + esc(n) + '" data-field="' + ch.field + '">' + esc(words[j].add) + '</button>') + '</td>';
        }).join('');
        const rm = k === 0 ? '' : '<button class="pd-dimlink" data-act="gspot-rm" data-g="' + i + '" data-node="' + esc(n) +
            '" title="Take ' + esc(n) + ' out of ' + esc(g.name) + '">&times;</button>';
        return '<tr' + (f && f.row === n ? ' class="focus"' : '') + ' data-grow="' + esc(n) + '" data-g="' + i + '">' +
            '<td><span class="pd-chip">' + esc(n) + (k === 0 ? ' <span class="pd-gstar">★</span>' : '') + '</span></td>' +
            cells + '<td>' + rm + '</td></tr>';
    }).join('');
    const on = !!f && f.g === i && !f.row;
    return '<div class="pd-grp' + (on ? ' focus' : '') + '" data-gcard="' + i + '">' +
        '<div class="gh"><button class="pd-gfocus" data-act="gfocus" data-g="' + i + '" title="show ' + esc(g.name) +
        ' on the picture">' + groupTagHTML(g, i) + '</button>' +
        '<input class="pd-inp" type="text" data-gname="' + i + '" value="' + esc(g.name) + '" aria-label="this group’s name">' +
        '<span class="pd-dim">the part on ' + esc(g.of) + '</span></div>' +
        '<table class="pd-gtbl"><thead><tr><th>' + esc(sw.head) + '</th>' + words.map(w => '<th>' + esc(w.head) + '</th>').join('') +
        '<th></th></tr></thead><tbody>' + rows + '</tbody></table>' +
        '<button class="pd-chip add" data-act="gspot-add" data-g="' + i + '">' + esc(sw.add) + '</button>' +
        '<div class="pd-lbl">Where bins come from and go</div><div class="pd-gkv">' +
        // ONE NAME PER FIELD (F32): the claim's words, not "In from / Out to".
        '<span class="k">' + esc(W('inbound_source')) + '</span>' + picker(g.of, 'source', esc(star.source || '—'), star.source ? '' : 'bad',
            W('inbound_source') + ' ' + (star.source || 'not set')) +
        '<span class="k">' + esc(W('outbound_destination')) + '</span>' + picker(g.of, 'dest', esc(star.dest || '—'), star.dest ? '' : 'bad',
            W('outbound_destination') + ' ' + (star.dest || 'not set')) + '</div>' +
        '<div class="pd-lbl">Set for this group</div>' + groupSettings(g, i) + '</div>';
}

// ── the card's edits: every one a draft change, nothing written ──

// groupChanged redraws after a card edit; the last refusal no longer says
// anything about what is on screen.
function groupChanged() {
    if (S.shapeDraft) S.shapeDraft.refusal = '';
    drawFlows();
}

// stationNames is the recipe's own station's positions, in sequence order:
// what a card may add.
function stationNames() {
    return (draftCell().positions || []).slice().sort((a, b) => (a.sequence || 0) - (b.sequence || 0))
        .map(p => p.core_node_name);
}

// openGroupPick is the shared picker over `names` in the page's popover; a
// pick runs `pick` on it.
function openGroupPick(btn, title, names, empty, pick) {
    const pop = $('pd-pop');
    if (!pop) return;
    openRowPicker(pop, btn, names.map(n => ({ value: n, label: n })), pick,
        { head: esc(title), empty: empty, placeholder: 'find a position' });
}

// + Add a press spot: a picker over the station's positions no group holds
// and no row of the recipe stands on. The new row's lists start as the ★
// row's, so an on-deck spot that feeds the ★ spot is offered for it too —
// PLN_010 arrives with PLN_005 (a Sequential pair's B side does not: it is
// half of the new pair, and the engineer picks it).
function addGroupSpot(btn, i) {
    const d = S.shapeDraft;
    const g = d && d.groups && d.groups[i];
    if (!g) return;
    const held = new Set([].concat(...d.groups.map(groupNodes), M().toCells(S.model).map(c => c.core_node_name)));
    const names = stationNames().filter(n => !held.has(n));
    const seq = (S.model.cells[g.of] || {}).mode === 'sequential';
    openGroupPick(btn, spotWords((S.model.cells[g.of] || {}).mode).add.replace(/^\+ /, ''), names,
        'Every position of this station is in a group or a row already.', n => {
            const spot = { core_node_name: n };
            if (!seq) for (const f of GROUP_FIELDS) if ((g.spots[0][f] || []).length) spot[f] = g.spots[0][f].slice();
            g.spots.push(spot);
            groupChanged();
        });
}

// + on deck / + third / + lane: a deck or third is one of the station's
// positions, a lane is a staging lane of the routing set or one of the press's
// own back positions (the table's own staging list); never a node another
// group holds, a spot of this one, or one the list has.
function addGroupList(btn, i, node, field) {
    const d = S.shapeDraft;
    const g = d && d.groups && d.groups[i];
    const s = g && g.spots.find(x => x.core_node_name === node);
    if (!s) return;
    const others = new Set([].concat(...d.groups.filter(x => x !== g).map(groupNodes)));
    const spots = new Set(g.spots.map(x => x.core_node_name));
    const have = new Set(s[field] || []);
    const backs = S.model.positions.filter(p => p.kind === 'back').map(p => p.core_node_name);
    const pool = DECK_FIELDS.indexOf(field) >= 0 ? stationNames()
        : [...new Set((S.composer.routing || []).filter(r => r.role === 'staging').map(r => r.core_node_name).concat(backs))];
    const heads = new Set(M().toCells(S.model).map(c => c.core_node_name));
    const names = pool.filter(n => !others.has(n) && !spots.has(n) && !have.has(n) && !(heads.has(n) && !spots.has(n)));
    const words = listWords(field, (S.model.cells[g.of] || {}).mode);
    openGroupPick(btn, words.add.replace(/^\+ /, '') + ' for ' + node, names, 'Nothing left to offer here.', n => {
        s[field] = (s[field] || []).concat([n]);
        groupChanged();
    });
}

function dropGroupList(i, node, field, v) {
    const g = S.shapeDraft && S.shapeDraft.groups && S.shapeDraft.groups[i];
    const s = g && g.spots.find(x => x.core_node_name === node);
    if (!s) return;
    s[field] = (s[field] || []).filter(x => x !== v);
    groupChanged();
}

function dropGroupSpot(i, node) {
    const d = S.shapeDraft;
    const g = d && d.groups && d.groups[i];
    if (!g || g.of === node) return;
    g.spots = g.spots.filter(x => x.core_node_name !== node);
    if (d.focus && d.focus.row === node) d.focus = null;
    groupChanged();
}

// A click on a spot's row pins it on the picture, a second lets go; the
// group's tag does the same for the group.
function pinGroupRow(i, node) {
    const d = S.shapeDraft;
    if (!d || !d.groups) return;
    d.focus = d.focus && d.focus.row === node ? null : { g: i, row: node };
    drawFlows();
}

function pinGroup(i) {
    const d = S.shapeDraft;
    if (!d || !d.groups) return;
    d.focus = d.focus && !d.focus.row && d.focus.g === i ? null : { g: i };
    drawFlows();
}

// onGroupHover follows the pointer over the cards. It redraws the picture and
// the cards' outline, nothing else; moving inside one card is one card.
function onGroupHover(e, entering) {
    const d = S.shapeDraft;
    if (!d || !d.groups || S.tab !== 'flows') return;
    const card = e.target && e.target.closest ? e.target.closest('[data-gcard]') : null;
    if (!card) return;
    if (entering) {
        const g = Number(card.dataset.gcard);
        if (g === d.hover) return;
        d.hover = g;
    } else {
        const to = e.relatedTarget;
        if (to && typeof card.contains === 'function' && card.contains(to)) return;
        if (d.hover === null || d.hover === undefined) return;
        d.hover = null;
    }
    const f = groupFocus();
    const mark = (el, i) => (f && !f.row && f.g === i ? el.classList.add('focus') : el.classList.remove('focus'));
    mark(card, Number(card.dataset.gcard));
    for (const el of root().querySelectorAll('[data-gcard]')) mark(el, Number(el.dataset.gcard));
    drawPicture();
    markRecipeState();
}

// A card's name is typed in place: the tags follow it, the tab is not
// redrawn under the cursor.
function onGroupName(e) {
    const el = e.target;
    const d = S.shapeDraft;
    if (!el || !el.dataset || el.dataset.gname === undefined || !d || !d.groups) return;
    const g = d.groups[Number(el.dataset.gname)];
    if (!g) return;
    g.name = el.value;
    d.refusal = '';
    drawPicture();
}

// ── "On the floor" (SYNTH row 10) ──
//
// WHAT THE FLOOR CAN PICK, FROM THE SAVED CARD: the spots, at and carry Go
// computed for the version the floor has, never the draft's own guess — the
// HMI enforces that same object, so the block and the floor cannot disagree.
// An edited draft says its changes wait for Save; a recipe with no saved
// groups says the block appears once they are.

function savedRecipeCard() {
    const d = S.shapeDraft;
    if (!d || !d.recipe) return null;
    return (S.composer.presets || []).find(x => String(x.id) === String(d.recipe.id)) || null;
}

function orList(names) {
    const n = (names || []).filter(Boolean);
    if (n.length < 2) return n.join('');
    return n.slice(0, -1).join(', ') + ' or ' + n[n.length - 1];
}

const FLOOR_NOUN = { on_deck: 'on deck', third: 'third', lane: 'lane', spot: 'with' };

// floorSentence: "Left Hand runs on PLN_006 or PLN_010, on deck PLN_005 ·
// Right Hand on PLN_007 or PLN_012, on deck PLN_008 · nothing else is
// offered." A place whose list differs at one of its spot's stands says so at
// that stand, and that it moves with its spot when the card carries it there.
function floorSentence(card) {
    const spots = card.spots || [];
    const parts = (card.groups || []).map((g, i) => {
        const own = spots.filter(s => s.group === g.name);
        const head = own.find(s => s.node === g.of) || own.find(s => !s.at);
        const stands = head ? head.targets || [head.node] : [g.of];
        let s = g.name + (i === 0 ? ' runs on ' : ' on ') + orList(stands);
        for (const p of own.filter(x => x !== head && x.at)) {
            const base = (p.at[head ? head.node : stands[0]] || []);
            s += ', ' + (FLOOR_NOUN[p.job] || p.job) + ' ' + orList(base);
            for (const st of stands) {
                const list = p.at[st] || [];
                if (list.join() === base.join()) continue;
                const carried = head && head.carry && head.carry[st] && head.carry[st][p.node];
                s += ', or ' + orList(list) + ' at ' + st + (carried ? ' (it moves there with its spot)' : '');
            }
        }
        return s;
    });
    return parts.join(' · ') + ' · nothing else is offered.';
}

// runningOutside names what the running style runs that these groups do not
// hold, when that style is this recipe's shape (a claim per slot in the
// recipe's modes): the floor cannot set it up again from this recipe.
function runningOutside(card) {
    const p = process();
    const st = p && p.active_style_id ? composerStyle(p.active_style_id) : null;
    const groups = card.groups || [];
    if (!st || !groups.length) return '';
    const modes = new Set(Object.values(card.cells || {}).map(c => c.swap_mode));
    const rows = (st.claims || []).filter(c => modes.has(c.swap_mode) && c.payload_code);
    if (rows.length !== (card.slots || []).length) return '';
    const out = [];
    for (const c of rows) {
        const g = groups.find(x => (x.spots || []).some(s => s.core_node_name === c.core_node_name));
        if (!g) {
            if (!groups.some(x => groupNodes(x).indexOf(c.core_node_name) >= 0)) out.push(c.core_node_name + ' is in none of them');
            continue;
        }
        const s = g.spots.find(x => x.core_node_name === c.core_node_name);
        for (const f of GROUP_FIELDS) {
            if (c[f] && (s[f] || []).indexOf(c[f]) < 0) {
                out.push(c[f] + ' is not ' + listWords(f, c.swap_mode).noun + ' for ' + c.core_node_name + ' in ' + g.name);
            }
        }
    }
    return out.length ? 'The running style ' + st.name + ' stands outside these groups: ' + out.join('; ') + '.' : '';
}

function floorBlock() {
    const card = savedRecipeCard();
    let body;
    if (card && (card.groups || []).length) {
        body = esc(floorSentence(card));
        const changed = JSON.stringify(groupsBody(S.shapeDraft.groups)) !== JSON.stringify(groupsBody(card.groups)) ||
            M().shapeDiff(S.model, card).length > 0;
        if (changed) body += ' <span class="pd-dim">Your changes reach the floor when you save.</span>';
        const out = runningOutside(card);
        if (out) body += '<br><span class="pd-warn">' + esc(out) + '</span>';
    } else {
        body = 'Not saved yet: what the floor can pick is said here once these groups are saved. ' +
            'Until then the floor runs this recipe without groups.';
    }
    return '<div class="pd-floor" id="pd-floor"><div class="pd-lbl">On the floor</div><p>' + body + '</p></div>';
}

// markRecipeState publishes the draft's state for the shots harness, beside
// the words it shows: body[data-pd-recipe] = "draft;groups=2;floor=saved;
// row=PLN_010" (a group hovered or pinned is ";focus=<name>").
function markRecipeState() {
    const d = S.shapeDraft;
    let m = '';
    if (d && S.tab === 'flows') {
        m = 'draft';
        if (d.groups) {
            m += ';groups=' + d.groups.length;
            const card = savedRecipeCard();
            if (card && (card.groups || []).length) m += ';floor=saved';
            const f = groupFocus();
            if (f && f.row) m += ';row=' + f.row;
            else if (f) m += ';focus=' + d.groups[f.g].name;
        }
    }
    try { document.body.dataset.pdRecipe = m; } catch (_) { /* nothing to publish on */ }
}

// A position is USED when it has a row of its own, or when it is somebody
// else's partner or staging — those are drawn on the row that names them and
// have no row of their own. What is left is free, and free is what "+ Add a
// position" adds. Its own function because the footer names the free ones and
// the click that follows has to add one of exactly those.
//
// model is S.model unless the picture of a draft with groups asks about its
// own (groupPicture lays the groups' spots into a copy).
function usedPositions(model) {
    const m = model || S.model;
    const used = new Set();
    for (const pos of m.positions) {
        const n = pos.core_node_name;
        const c = m.cells[n];
        if (!c || !c.on || !c.mode) continue;
        used.add(n);
        for (const col of ['partner', 'staging']) {
            for (const chip of M().rowColumns(m, n, col)) if (chip.value) used.add(chip.value);
        }
    }
    return used;
}

// rowlessRoles says what each position without a row of its own is for, as
// "PLN_04 is paired core node for PLN_03" — one clause per row that sets it, in
// position order. The foot said "paired above" for all of them, but a
// position set as another row's inbound staging is staging, not paired. The
// words are the picture card's own for that position (the model's cardLines:
// each field's name over "for"), run into one sentence (F27: one name per
// field; it said "on deck for" under a "Paired core node" heading).
function rowlessRoles(used) {
    const active = n => !!(S.model.cells[n] && S.model.cells[n].on && S.model.cells[n].mode);
    const phrase = {
        paired: W('paired_core_node').toLowerCase() + ' for',
        secondPaired: M().fieldLabel(S.model, 'second_paired_core_node').toLowerCase() + ' for',
        staging: 'inbound staging for',
        parkOld: 'outbound staging for',
    };
    const out = [];
    for (const pos of S.model.positions) {
        const n = pos.core_node_name;
        if (!used.has(n) || active(n)) continue;
        for (const owner of S.model.positions.map(p => p.core_node_name).filter(active)) {
            for (const col of ['partner', 'staging']) {
                for (const chip of M().rowColumns(S.model, owner, col)) {
                    if (chip.value === n && phrase[chip.key]) out.push(n + ' is ' + phrase[chip.key] + ' ' + owner);
                }
            }
        }
    }
    return out;
}

// THE FREE LIST IS THE PROCESS'S, every station's positions in it: the
// desktop's cell is the whole process (processCellPicture, station 0), so a
// position another station of this process owns is offered here too. That is
// right and stays (owner, 2026-10-26): a press's outbound is the unloader's
// position, which the unloader's own station owns.
function freePositions() {
    if (!S.model) return [];
    const used = usedPositions();
    return S.model.positions.map(p => p.core_node_name).filter(n => !used.has(n));
}

function positionsTable() {
    // The front/back word under each position (rowWord). pictureRows reads the
    // cell's coordinates and nothing about the frame, so the table computes it
    // here, as it draws, rather than waiting for the picture to.
    S.picRows = pictureRows(cellFromModel(), pictureOpts());
    const used = usedPositions();
    const drawn = drawnRows();
    // R2: A COLUMN NO ROW USES IS NOT DRAWN, and a row whose mode does not
    // use a drawn column leaves its cell empty. An empty table keeps them all:
    // there is no row to judge by.
    const columns = tableColumns().filter(col => !col.uses || !drawn.length || drawn.some(col.uses));
    // THE SELECTED POSITION'S ROBOTS are a row of their own under its row
    // (workRow): where the engineer just clicked, and only there.
    const workAt = workRowNode();
    const rows = drawn.map(n => '<tr class="' + (S.selected === n ? 'selrow' : '') + '" data-row="' + n + '">' +
        columns.map(col => '<td' + (col.td ? ' class="' + col.td + '"' : '') + '>' +
            (!col.uses || col.uses(n) ? col.cell(n, S.model.cells[n]) : '') + '</td>').join('') + '</tr>' +
        (n === workAt ? '<tr class="pd-workrow"><td colspan="' + columns.length + '" id="pd-workrow">' +
            workListHTML(S.selected, 'Checking…') + '</td></tr>' : ''));
    const rowless = rowlessRoles(used);
    const free = freePositions();
    let tail = '';
    if (rowless.length) {
        tail += '<tr class="paired"><td colspan="' + columns.length + '">' + esc(rowless.join(' · ')) +
            ' — no row of ' + (new Set(rowless.map(r => r.split(' ')[0])).size === 1 ? 'its' : 'their') +
            ' own</td></tr>';
    }
    // ONE "+ ADD A POSITION": a dashed row closing the table, where an
    // engineer who has just filled a row looks for the next one, drawn as a
    // control and never as a caption. The head line carried a second one
    // (browser walk 2026-11-11) for the same act, with the free names beside
    // it; those names are the picker's rows once it opens, and the picture
    // still says how many are free (freeLine).
    const addRow = free.length
        ? '<tr class="pd-addrow"><td colspan="' + columns.length + '"><button class="pd-addrowbtn" data-act="add-position">' +
        '+ Add a position</button></td></tr>'
        : '';
    const cols = '<colgroup>' + columns.map(col => '<col class="' + col.col + '">').join('') + '</colgroup>';
    // THE LABEL (WITH THE ADD CONTROL) AND THE HEADINGS ARE OUTSIDE THE ROWS' BOX.
    // `#pd-postbl` scrolls the rows sideways and is still the box T1 measures
    // the table against — its clientWidth is what a column has to fit inside.
    // The headings are a strip of their own, `#pd-poshead`, so they can stick
    // to the main column as it scrolls down: inside a box that scrolls
    // sideways they would stick to that box, which never scrolls down. The
    // strip is a second `.pd-postbl` with the same colgroup, so its columns
    // are the rows' columns; syncPosHead carries it sideways with the rows.
    return '<div class="pd-posbox" id="pd-posbox">' +
        '<div class="pd-lbl">Positions' +
        '<span class="hint">click a card to find its row · click a cell to change it</span></div>' +
        '<div class="pd-postbl pd-poshead" id="pd-poshead">' +
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
        // `Position` is not a claim field: it is the press's word for a
        // node, and the set chip's column has no heading. `Part` stays
        // the floor's word for payload_code (owner-accepted). `Staging` is a
        // group of two, and its chips carry the two names.
        '<table>' + cols + '<thead><tr>' + columns.map(col => '<th>' + esc(col.head) + '</th>').join('') +
        '</tr></thead></table></div>' +
        '<div class="pd-postbl" id="pd-postbl">' +
        '<table>' + cols + '<tbody>' + rows.join('') + addRow + tail + '</tbody></table></div>' +
        '</div>';
}

// tableColumns is the positions table's columns, in order: the <col> class,
// the heading, the cell, and `uses`, whether a row's mode uses the column
// (flowspec, through the model), for the columns a mode can go without.
//
// A SHAPE DRAFT HAS NO PART COLUMN (pick-a-part U2): a recipe never carries
// one, so the column would be a control whose every value is refused. NOR
// AUTO REORDER OR THE SET CHIP (recipe groups): a recipe's save strips every
// cell's Advanced (domain.StripFlowPayload), so a per-row setting would be
// dropped without a word. A recipe's settings are its groups', on their cards.
function tableColumns() {
    const shows = field => n => M().advancedShows(S.model, n, field);
    const chips = column => n => M().rowColumns(S.model, n, column).some(c => c.possible);
    const out = [
        // THE POSITION IS A CONTROL: a picker of where it may move
        // (composer-model positionChoices, the HMI's rule too) and a delete.
        // Both are draft edits like every other chip; on the running style
        // the save refuses one that would leave a bin behind, and says how
        // to move the bin in Core first.
        { col: 'c-pos', td: 'pos', head: 'Position', cell: n => picker(n, 'position', esc(n), 'pd-posname', n) +
            '<button class="pd-rmpos" data-act="rmpos" data-node="' + esc(n) + '" title="Take ' + esc(n) +
            ' off this flow">×</button><small>' + esc(rowWord(n)) + '</small>' },
        { col: 'c-role', head: 'Role', cell: (n, c) => roleCell(n, c.role) },
        // The mode's word alone in the row: its glyph is on the picture's card
        // and in the menu, and in the row it cost a name its room (F18).
        { col: 'c-mode', head: W('swap_mode'), cell: (n, c) => picker(n, 'mode', esc(M().modeLabels()[c.mode] || '')) },
        { col: 'c-part', head: 'Part', draftless: true,
            cell: (n, c) => picker(n, 'part', esc(M().shortPart(c.part) || 'pick one'), c.part ? 'part' : 'bad', c.part || '') },
        { col: 'c-partner', head: W('paired_core_node'), uses: chips('partner'), cell: n => columnCell(n, 'partner') },
        { col: 'c-staging', head: 'Staging', uses: chips('staging'), cell: n => columnCell(n, 'staging') },
        { col: 'c-src', head: W('inbound_source'), uses: shows('inbound_source'),
            cell: (n, c) => fieldCell(n, 'source', 'inbound_source', c.source, '—') + routingGap(n, 'inbound_source', c.source) },
        { col: 'c-via', td: 'via', head: W('key_route'), uses: shows('key_route'), cell: n => viaCell(n) },
        { col: 'c-dst', head: W('outbound_destination'), uses: shows('outbound_destination'),
            cell: (n, c) => fieldCell(n, 'dest', 'outbound_destination', c.dest, '—') + routingGap(n, 'outbound_destination', c.dest) },
        { col: 'c-reorder', head: W('auto_reorder'), draftless: true, uses: shows('auto_reorder'), cell: n => reorderCell(n) },
        // THE ROW SAYS IT SETS SOMETHING FOR THIS STYLE, and the chip takes
        // you to it (Set for this style, under the table). A row that sets
        // nothing draws nothing: the chip marks an exception. It replaces the
        // gear, which opened a sheet of every field at its default. The count
        // is the list's own (setCount): one per setting the position is
        // listed under.
        { col: 'c-set', head: '', draftless: true, uses: n => setCount(n) > 0, cell: n => {
            const set = setCount(n);
            return set ? '<button class="pd-chip pd-setchip" data-act="set-goto" title="see it under Set for this style">' +
                set + ' set</button>' : '';
        } },
    ];
    return S.shapeDraft ? out.filter(col => !col.draftless) : out;
}

// roleCell is the position's consume/produce control. The role starts derived
// (deriveRole) and an engineer sets it here when the press does the other
// thing. A CONTROL IS DRAWN AS A CONTROL: it was a small pill riding the
// front/back line under the name, with no heading, and a click flipped the
// role with nothing to say there were two. It is a chip under its own Role
// heading, like every other field in the row, and its menu offers both roles
// through the same setRole action the pill sent. The station's cell card
// draws the same two choices under the same word.
function roleCell(node, role) {
    return picker(node, 'role', esc(role || 'pick one'), role ? '' : 'bad', 'Role ' + (role || 'not set'));
}

// ONE WORD, ONE MEANING. The sub-label under a position is which ROW of the
// picture it was drawn in, and nothing else. It used to be CellPosition.Kind,
// which answers a different question — is this a partner slot for any style
// this process runs — and on Press 6 the two disagree: PLN_013, 014 and 015
// are Kind "front" and stand in its back line, so the table said front under
// a picture that drew them at the back. What a position does for the running style
// is said by the paired-back line at the foot of the table.
//
// A middle row, a picture with only one row, and every position of a cell
// drawn by relation get no word: the renderer declines to label those and a
// guess printed in a table is still a guess.
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
// routingGap is the server's sentence (the composer's routing_gaps) beside a
// recipe draft row's empty field that the row's mode requires and the
// routing set has no row to fill: which field, and where to add one. The
// server says it; the page only draws it.
function routingGap(node, field, value) {
    const why = !value && S.shapeDraft && (S.composer.routing_gaps || {})[field];
    return why && M().fieldRequired(S.model, node, field) ? '<small class="pd-gap">' + esc(why) + '</small>' : '';
}

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
    if (!chips.length) return '';
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
        // `Second paired core node` under PAIRED CORE NODE is `Second`, and
        // `Inbound staging` under STAGING is `Inbound` (F18: the long word
        // was the part that showed, `Second paired core n…`).
        const lw = chip.label.toLowerCase(), hw = heading.toLowerCase();
        const word = lw === hw ? '' : (hw && lw.endsWith(' ' + hw) ? chip.label.slice(0, -hw.length - 1) : chip.label);
        const k = word ? '<span class="k">' + esc(word) + '</span> ' : '';
        const kind = 'col:' + column + ':' + chip.key;
        // THE VALUE IS WHAT THE ROW IS READ FOR. A set chip drew its word in
        // front of its value, `Inbound Stag… ▾`, so at table width the word
        // was all that showed and the node it named was cut off. A set chip
        // draws its value alone and the word moves to the title, where two
        // chips of one cell are still told apart. An empty chip has no value
        // to show, so it keeps its word in front of the dash.
        // STAGING SAYS WHICH IS WHICH (owner, 2026-11-15: two dropdowns
        // stacked, SMN_017 over SMN_017, said nothing). A set staging chip
        // keeps its value whole and carries its word on a small line above
        // it, so neither is cut: the word is not inside the narrow chip.
        if (chip.required || chip.value || isOpen(node, kind)) {
            const above = column === 'staging' && chip.value && word ? '<small class="pd-cellk">' + esc(word) + '</small>' : '';
            return above + picker(node, kind, chip.value ? esc(chip.value) : k + '—',
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

// AUTO REORDER IS ON THE ROW (U3): `off`, or the reorder point the position
// asks at. It is what an engineer sets on every position before a style runs,
// so it is not one of Set for this style's lists. A new position starts off, consume
// and produce alike (owner ruling 8); a mode that does not read it draws an
// empty cell (R2).
function reorderCell(node) {
    if (!M().advancedShows(S.model, node, 'auto_reorder')) return '';
    const a = M().advancedFor(S.model, node);
    const title = W('reorder_point') + ' ' + (a.reorder_point || 'not set') + ' · ' + W('auto_reorder') + ' ' + (a.auto_reorder ? 'on' : 'off');
    if (!a.auto_reorder) return picker(node, 'reorder', 'off', 'dflt', title);
    return picker(node, 'reorder', a.reorder_point ? 'on at ' + esc(String(a.reorder_point)) : 'on at —', a.reorder_point ? '' : 'bad', title);
}

// REORDER_WORDS is the reorder point's sentence: true for both roles, since
// a consume bin counts down to it and a produce bin counts up to it
// (protocol.ClaimDemandLevel).
const REORDER_WORDS = 'units in the bin when the next one is requested';

// openReorderPop is the cell's popover: the point, Turn on (Set once it is
// on), which writes the point and turns Auto reorder on at it, Off, and Clear
// the point (0, the opt-out, and off). Each is a draft edit, saved with Save
// style.
function openReorderPop(btn, node) {
    const pop = $('pd-pop');
    if (!pop || !S.model) return;
    pop.classList.remove('pd-partpop');
    S.partPop = null;
    const a = M().advancedFor(S.model, node);
    pop.innerHTML = '<div class="pd-rpop">' +
        '<label class="pd-lbl">' + esc(W('reorder_point')) + '</label>' +
        '<div class="pd-rprow"><input class="pd-inp" type="text" inputmode="numeric" data-f="rp" value="' +
            esc(a.reorder_point ? String(a.reorder_point) : '') + '" placeholder="—">' +
        '<button class="pd-btn primary" data-rp="set">' + (a.auto_reorder ? 'Set' : 'Turn on') + '</button></div>' +
        '<small>' + esc(REORDER_WORDS) + '</small>' +
        '<button data-rp="off" class="' + (a.auto_reorder ? '' : 'on') + '">' + esc(W('auto_reorder')) + ' off</button>' +
        (a.reorder_point ? '<button data-rp="clear">Clear the point</button>' : '') +
        '<div class="none" data-rpnote></div></div>';
    showPop(pop, btn);
    placePopover(pop, btn);
    const input = pop.querySelector('[data-f="rp"]');
    if (input) input.focus();
    pop.onclick = e => {
        const b = e.target.closest && e.target.closest('[data-rp]');
        if (e.stopPropagation) e.stopPropagation();
        if (!b) return;
        if (b.dataset.rp === 'off') { setReorder(node, null, false); return; }
        // A point of 0 is the opt-out on either role (domain DemandLevel): the
        // clear writes it and turns auto-reorder off with it, what the
        // Replenishment page's blank point did before the page went.
        if (b.dataset.rp === 'clear') { setReorder(node, 0, false); return; }
        const digits = String((pop.querySelector('[data-f="rp"]') || {}).value || '').replace(/[^0-9]/g, '');
        if (!Number(digits)) {
            const note = pop.querySelector('[data-rpnote]');
            if (note) note.textContent = 'Type the point first';
            return;
        }
        setReorder(node, Number(digits), true);
    };
}

// setReorder writes the two fields onto the position's Advanced block, as the
// sheet's Apply does; point null keeps the stored point.
function setReorder(node, point, on) {
    const a = M().advancedFor(S.model, node);
    if (point !== null) a.reorder_point = point;
    a.auto_reorder = on;
    closePop();
    apply({ type: 'setAdvanced', node: node, advanced: a });
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
    fitPositionColumns();
}

// fitPositionColumns sizes the table's columns to what they hold (F18, F28:
// `WKB_…`, `Inbound stagi…`, `Sequential A…` at 1440). The shares in the CSS
// were written for one set of names; a plant's group names are longer and
// every chip past its share was cut. So the page measures: both tables laid
// out at their content's width once (`pd-measure`: the rows' chips on one
// line, the headings wrapped at their words), each column takes the wider of
// its heading and its widest cell, and the shares are those widths. The
// table's floor is their sum, so where the column is narrower than that the
// rows' box scrolls sideways (its headings strip with it) instead of cutting
// a name. Run after every draw of the table; a layout read, no timer.
function fitPositionColumns() {
    const box = $('pd-postbl'), head = $('pd-poshead');
    const tables = [head, box].map(b => (b && b.querySelector ? b.querySelector('table') : null));
    if (!tables[0] || !tables[1] || typeof tables[1].getBoundingClientRect !== 'function' || !box.clientWidth) return;
    const rows = [head.querySelector('tr'), box.querySelector('tr[data-row]')];
    if (!rows[0] || !rows[1]) return;
    const cols = tables.map(t => [...t.querySelectorAll('col')]);
    cols.forEach(list => list.forEach(c => { c.style.width = ''; }));
    tables.forEach(t => { t.style.minWidth = ''; t.classList.add('pd-measure'); });
    const need = [...rows[0].children].map((th, i) => {
        const td = rows[1].children[i];
        return Math.ceil(Math.max(th.getBoundingClientRect().width, td ? td.getBoundingClientRect().width : 0));
    });
    // The key route was measured at its widest point; where the column has
    // room left, it takes up to its longest route on one line.
    const via = [...rows[1].children].findIndex(td => td.classList.contains('via'));
    let long = 0;
    if (via >= 0) {
        box.querySelectorAll('tr[data-row] td.via').forEach(td => {
            const w = [...td.children].reduce((a, c) => a + c.getBoundingClientRect().width + 4, 8);
            long = Math.max(long, Math.ceil(w));
        });
    }
    tables.forEach(t => t.classList.remove('pd-measure'));
    let total = need.reduce((a, b) => a + b, 0);
    if (!total) return;
    if (via >= 0 && long > need[via]) {
        const more = Math.min(long - need[via], Math.max(0, box.clientWidth - total));
        need[via] += more;
        total += more;
    }
    cols.forEach(list => list.forEach((c, i) => { c.style.width = (need[i] / total * 100).toFixed(3) + '%'; }));
    tables.forEach(t => { t.style.minWidth = total + 'px'; });
}

// syncPosHead carries the headings strip sideways with the rows. The strip
// sits outside the rows' box so it can stick to the main column; its own
// overflow is hidden, so the only thing that moves it is this.
function syncPosHead(e) {
    if (!e.target || e.target.id !== 'pd-postbl') return;
    const head = $('pd-poshead');
    if (head) head.scrollLeft = e.target.scrollLeft;
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
    let b = M().bar(S.model);
    const bar = $('pd-bar');
    if (!bar) return;
    // A RECIPE DRAFT IS NOT PREVIEWED, so the bar never says "Preview OK" over
    // one: its required cells left empty are its findings (F5, F33), counted
    // the way a style's bar counts the server's, and nothing left is "Ready
    // to save".
    const gaps = S.shapeDraft && b.tone !== 'blocked' && b.tone !== 'empty' ? draftGaps() : [];
    if (gaps.length) {
        b = { heading: gaps.length + ' to fix' + (gaps.length > 1 ? ' · showing the first' : ''),
            detail: gaps[0].message, tone: 'blocked', node: gaps[0].node };
    } else if (S.shapeDraft && b.tone === 'ok') {
        b = Object.assign({}, b, { heading: 'Ready to save' });
    }
    // THE PROBLEM LINE IS THE CONTROL. With fixIt gone there is no second
    // button on the bar; the first finding names a position, and the word
    // that names it takes you there — selected in the picture, its table row
    // scrolled into view. A finding with no position (an unplaced part) is
    // not clickable: there is nothing to take you to.
    let target = b.node || null;
    if (b.tone === 'blocked' && !target) {
        for (const f of M().findings(S.model)) {
            if (f.node && S.model.cells[f.node]) { target = f.node; break; }
        }
    }
    // THE SENTENCE IS PER SURFACE (R3, F24). The model's says "Tap a
    // position" / "Tap the position and pick …", which is the HMI's door; on
    // the desktop the picture never edits, and the way in is the table's row
    // or its "+ Add a position".
    // "BELOW" ONLY WHEN SOMETHING IS BELOW. The footer's "+ Add a position"
    // is drawn for a free position; with none the process has no position
    // yet, and they are added in Settings, on a screen or on none (positions
    // come before the screen).
    const empty = freePositions().length ? 'Add a position below'
        : 'This process has no position yet: add them in Settings › Positions and routing';
    const detail = b.tone === 'empty' ? empty : String(b.detail || '')
        .replace('Tap the position and pick a choreography.', 'Pick its ' + W('swap_mode').toLowerCase() + ' on its row.')
        .replace('Tap the position and pick one.', 'Pick it on its row.');
    const head = '<div class="h' + (b.tone === 'blocked' ? ' bad' : '') + '">' + esc(b.heading) + '</div>';
    // AN UNPLACED PART CARRIES ITS OWN DOOR OFF THE FLOW, as on the station's
    // strip. The part picker's take-off needs a position, and a part in the
    // style's set that sits on none has no position to open — so without this
    // the desktop had no way to drop it short of discarding the draft. It is
    // the same removePart the picker sends: an edit to the draft, saved with
    // the rest. Drawn whenever the finding exists, not only when it is the
    // first one the bar names, because this bar is the one place the desktop
    // shows a part with no position. A placed part gets no door here; its way
    // out is its position's picker.
    const loose = M().findings(S.model)
        .filter(f => f.field === 'unplaced_part')
        .reduce((all, f) => all.concat(f.parts || []), []);
    const doors = loose.length
        ? '<div class="fix">' + loose.map(p => '<button class="pd-chip" data-act="rmpart" data-part="' + esc(p) + '">' +
            esc('Take ' + M().shortPart(p) + ' off this flow') + '</button>').join('') + '</div>'
        : '';
    bar.innerHTML = '<div' + (target ? ' class="as-link" data-act="bar-goto" data-node="' + esc(target) + '"' : '') + '>' +
        head +
        '<div class="d">' + esc(detail) + '</div></div>' + doors +
        '<div class="prov' + ((S.shapeDraft ? draftDirty() : dirty()) ? ' dirty' : '') + '">' + esc(provText()) + '</div>';
    const save = root().querySelector('[data-act="save"]');
    if (save) save.disabled = !dirty() || b.tone === 'blocked';
    // `Save as preset…` is the mirror image: enabled only on `No unsaved
    // changes`, because a preset is made FROM the saved flow and so can never
    // disagree with what the station runs. The two buttons are therefore never
    // enabled at the same time, which is the point rather than an accident.
    const asPreset = root().querySelector('[data-act="save-preset"]');
    if (asPreset) asPreset.disabled = dirty() || !(S.model && M().toCells(S.model).length);
    // A shape draft is always unsaved; it may be named once nothing blocks it.
    const shapeSave = root().querySelector('[data-act="shape-save"]');
    if (shapeSave) shapeSave.disabled = b.tone === 'blocked' || !M().toCells(S.model).length;
}

// A SAVE SAYS IT SAVED. The bar read `No unsaved changes` before a save and
// after one, so a Save style that landed looked like a click that did nothing.
// The time it saved stays on the bar until the next edit makes it dirty
// again, and only on the flow that was saved.
function provText() {
    if (S.shapeDraft) return draftDirty() ? 'Unsaved changes' : 'No unsaved changes';
    if (dirty()) return 'Unsaved changes';
    const s = S.flowSaved;
    return s && s.styleID === S.styleID ? 'Saved ' + s.at : 'No unsaved changes';
}

function clockNow() {
    const d = new Date();
    return String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
}

// ── preview ──────────────────────────────────────────────────────────────────
function schedulePreview() {
    if (S.previewTimer) clearTimeout(S.previewTimer);
    S.previewTimer = setTimeout(runPreview, 400);
}

async function runPreview() {
    if (!S.model) return;
    // A shape draft has no style to plan a changeover to; its findings are
    // the model's own, and the store validates the shape when it is named.
    if (S.shapeDraft) { drawBar(); drawPicture(); return; }
    if (S.previewAbort) S.previewAbort.abort();
    S.previewAbort = new AbortController();
    // WHOSE PREVIEW THIS IS. schedulePreview clears the timer but not an
    // in-flight fetch, and the abort only happens when the NEXT run starts —
    // so a response for the style just left could land on the style just
    // opened. Its bar, its order count and its per-node findings were then the
    // other style's for up to 400 ms, and saveFlow posted that style's
    // fingerprint under this one's id, which comes back 409 "the flow changed
    // since you previewed" on a flow nobody had changed.
    const forStyle = S.model.styleId;
    // A PREVIEW WITH NO BODY IS NOT AN EMPTY PREVIEW (P0 bug, :2400). A 500
    // with no body read as {} through the .catch, and applyPreview took {} as
    // a preview with no count: "Preview OK · null orders". The door's answer
    // carries its sentence as the preview's error (answerBody), so the bar
    // blocks on it; no answer at all says so the same way rather than keeping
    // the last preview's words. The preview's own abort, when a newer edit
    // replaced it, is `aborted` and draws nothing.
    const res = await send('POST', '/api/processes/' + S.processID + '/flow/preview?steps=1', {
        to_style_id: S.model.styleId, cells: M().toCells(S.model),
        taken_off: M().takenOff(S.model),
    }, { notice: false, signal: S.previewAbort.signal });
    if (res.aborted) return;
    if (!S.model || S.model.styleId !== forStyle) return;
    S.model = M().applyPreview(S.model, answerBody(res));
    keepSteady(forStyle, answerBody(res));
    drawBar();
    drawPicture();
    drawWorkRow();
}

// ── save ─────────────────────────────────────────────────────────────────────
// SPEC §3: flow/save on the last preview's fingerprint; 409 stale re-previews
// and says so; 422 puts the server's findings on the rows.
async function saveFlow() {
    const pv = S.model.preview || {};
    // A DEAD EDGE IS AN ANSWER (P0 bug, :2415). The bare fetch rejected on
    // no answer and nothing caught it, so Save did nothing on screen; the door
    // answers status 0 and saveOutcome reads its sentence as the refusal.
    const res = await send('POST', '/api/processes/' + S.processID + '/flow/save', {
        to_style_id: S.model.styleId, cells: M().toCells(S.model),
        fingerprint: pv.fingerprint || '',
        station_id: stationIDForSave(),
    }, NO_NOTICE);
    const json = answerBody(res);
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
    if (out.kind === 'findings') {
        S.model = M().applyPreview(S.model, json);
        // From Setup's First style, the findings are shown on the table.
        if (S.tab === 'setup') { S.setupView = ''; S.tab = 'flows'; syncHash(); }
        drawFlows();
        return;
    }
    // Saved: the draft becomes the new baseline and the read is refreshed so the
    // rail's counts and last-run follow.
    S.baseline = JSON.stringify(M().toCells(S.model));
    S.flowSaved = { styleID: S.model.styleId, at: clockNow() };
    // A STYLE SETUP SENT THE ENGINEER TO GOES BACK TO SETUP once it is saved
    // (HOP, 2026-11-12): the walkthrough is where the next step is, and
    // landing back on the Styles tab left them to find their own way there.
    // Only while Setup still has a step; a process it no longer needs stays
    // on its style.
    const back = S.fromSetup;
    S.fromSetup = false;
    await openProcess(S.processID);
    if (back && needsSetup()) {
        const style = composerStyle(S.flowSaved.styleID);
        S.tab = 'setup';
        drawSetup();
        syncHash();
        toast('Saved' + (style ? ' ' + style.name : '') + '. Next: Run it.', 'info');
    }
}

// answerBody is the body a flow answer's readers take (saveOutcome,
// applyOutcome, applyPreview), from the door's answer: the server's own body,
// and on a failure the door's sentence as its `error`, so a 500 with no body
// or no answer at all is a refusal by name and not an empty object read as a
// success.
function answerBody(out) {
    const body = bodyOf(out);
    return out.ok ? body : Object.assign({}, body, { error: out.error });
}

// stationIDForSave is the screen a desktop write names: the process's first,
// or 0 for a process with no screen yet. flow/save takes 0 (a first style is
// drawn before any screen is hung on the press); flow/mint is a station's
// door and refuses it. Who saved is the signed-in person, never the screen.
function stationIDForSave() {
    const st = stationsOf(S.processID);
    return st.length ? st[0].id : 0;
}

// ── pickers ──────────────────────────────────────────────────────────────────
function optionsFor(node, kind) {
    const c = S.model.cells[node];
    const routing = S.composer.routing;
    const backs = S.model.positions.filter(p => p.kind === 'back').map(p => p.core_node_name);
    // A kind may carry arguments: "col:staging:parkOld", "via:1".
    const parts = String(kind).split(':');
    switch (parts[0]) {
        // Where this position may move: this station's free line positions,
        // never another station's node, never a node the flow already uses.
        case 'position':
            return M().positionChoices(S.model, node).map(to => ({
                value: to, label: to, on: false,
                action: { type: 'movePosition', from: node, to: to },
            }));
        case 'role':
            return ['consume', 'produce'].map(r => ({
                value: r, label: r, on: c.role === r,
                action: { type: 'setRole', node: node, role: r },
            }));
        case 'mode': {
            const labels = M().modeLabels();
            return Object.keys(labels).map(m => ({
                value: m, label: labels[m], icon: gl(m, 22, { rest: c.mode !== m }), on: c.mode === m,
                action: { type: 'setMode', node: node, mode: m },
            }));
        }
        // col:<column>:<cellKey> — one of the mode-dependent chips. The FIELD
        // comes from the model's own rowColumns, which reads flowspec, so a
        // picker can only ever write a column this mode is allowed to have.
        // A chip that is not drawn has no picker: the table renders a dash,
        // and a dash is not a control.
        case 'col': {
            const column = parts[1], key = parts[2];
            const chip = M().rowColumns(S.model, node, column).find(x => x.key === key);
            if (!chip) return [];
            // A PAIR IS THIS SCREEN'S OR NO SCREEN'S (B4): another station's
            // position (an unloader's window) is never a press row's pair.
            const stationOf = n => (S.model.positions.find(p => p.core_node_name === n) || {}).station_id || 0;
            const mine = stationOf(node);
            const others = S.model.positions.map(p => p.core_node_name)
                .filter(n => n !== node && (!stationOf(n) || stationOf(n) === mine));
            // A PARTNER IS A POSITION; STAGING IS A ROUTING ROLE. These four
            // keys used to share one list — the back positions plus every
            // other position of this process — and for `paired` and
            // `secondPaired` that is right: a press pairs with its own slots.
            //
            // For `staging` and `parkOld` it was wrong, and wrong in the way
            // this page exists to prevent: the station's composer (since torn
            // out, 681a912c) offered these two the routing set's staging rows
            // plus the backs, so one field had two answers depending on which
            // screen you asked. At Hopkinsville the 4x2's five positions are
            // all `front`, so `backs` is empty and the desktop offered the
            // other four ALNs as places to park a bin — none of which is a
            // staging node, while the seven real ones sat switched off in the
            // routing set where the station was correctly reporting them.
            //
            // The back positions stay in the list because a press that parks
            // on its own back slot has staging without a routing row — the
            // same union the station takes, and the reason Settings lists
            // those slots under Staging as "always available".
            const partnering = key === 'paired' || key === 'secondPaired';
            const list = partnering
                ? backs.concat(others.filter(n => backs.indexOf(n) < 0))
                : [...new Set(routing.filter(r => r.role === 'staging')
                    .map(r => r.core_node_name).concat(backs))];
            const act = { paired: 'setPartner', secondPaired: 'setSecondPartner', staging: 'setStaging', parkOld: 'setParkOld' }[key];
            const arg = { paired: 'partner', secondPaired: 'partner', staging: 'staging', parkOld: 'staging' }[key];
            // A REQUIRED CHIP SAYS SO. It has no `none` row, and a list that
            // simply lacked one gave no reason why; the first line names the
            // mode that needs the field.
            const clear = chip.required
                ? [{ note: 'required for ' + (M().modeLabels()[c.mode] || c.mode) }]
                : [{
                    value: '', label: 'none', on: !chip.value,
                    action: Object.assign({ type: act, node: node }, { [arg]: '' }),
                }];
            // SHARED STAGING IS A WARNING, NOT A REFUSAL. Two positions of one
            // flow may stage on the same node (the model's bar already says
            // so as a note), so a node another active position uses for
            // either staging field stays a choice and is marked with who.
            const usedBy = partnering ? {} : stagingUsers(node);
            return clear.concat(list.map(n => ({
                value: n, label: n, on: chip.value === n, use: usedBy[n] || '',
                action: Object.assign({ type: act, node: node }, { [arg]: n }),
            })));
        }
        case 'source':
            return routing.filter(r => r.role === 'source').map(r => ({
                value: r.core_node_name, label: r.label || r.core_node_name, on: c.source === r.core_node_name,
                action: { type: 'setSource', node: node, source: r.core_node_name },
            }));
        // THE PROCESS'S OTHER STATIONS' POSITIONS ARE DESTINATIONS TOO. A press
        // whose old bins go to an unloader on the same process names one of
        // that unloader's windows — Press 6's ULN_002 and ULN_005 — and the
        // server takes it (the routing set is the union of the positions and
        // the routing rows, domain.RoutingSetNames). A position can never be
        // a routing row as well (processes.ErrRoutingNodeIsPosition), so the
        // rows alone left such a destination with no picker anywhere: the
        // deploy note's "Out to ULN_005" had nothing to click. This
        // station's own positions are not offered: a spot's old bin does not
        // go to its neighbour.
        case 'dest': {
            const offered = routing.filter(r => r.role === 'destination').map(r => ({
                value: r.core_node_name, label: r.label || r.core_node_name, on: c.dest === r.core_node_name,
                action: { type: 'setDest', node: node, dest: r.core_node_name },
            }));
            const all = S.composer.cell.positions || [];
            const mine = (all.find(p => p.core_node_name === node) || {}).station_id || 0;
            const named = new Set(offered.map(o => o.value));
            const station = id => ((S.stations || []).find(s => s.id === id) || {}).name || 'another station';
            return offered.concat(!mine ? [] : all.filter(p => p.station_id && p.station_id !== mine &&
                !named.has(p.core_node_name)).map(p => ({
                value: p.core_node_name, label: p.core_node_name, on: c.dest === p.core_node_name,
                where: 'a position of ' + station(p.station_id),
                action: { type: 'setDest', node: node, dest: p.core_node_name },
            })));
        }
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
            // A WAYPOINT IS ON THE ROUTE ONCE (B7): one already on it is not
            // offered again, except at its own point. via-add filters the same.
            const offered = M().viaWaypoints(S.model, node).filter(w => w === route[at] || route.indexOf(w) < 0);
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

// stagingUsers maps each staging node to the other active positions that
// stage or park on it, as "PLN_02, PLN_06". A field flowspec forbids for the
// position's mode does not use its node — the model's stagingFieldUsed, the
// rule the picture's "not used by this flow" line reads — so a two-robot
// swap's leftover outbound staging is not a "used by".
function stagingUsers(node) {
    const by = {};
    for (const p of S.model.positions) {
        const n = p.core_node_name, cell = S.model.cells[n];
        if (n === node || !cell || !cell.on || !cell.mode) continue;
        const used = [[cell.staging, 'inbound_staging'], [cell.parkOld, 'outbound_staging']]
            .filter(([v, f]) => v && M().stagingFieldUsed(cell.mode, f))
            .map(([v]) => v);
        for (const v of new Set(used)) (by[v] = by[v] || []).push(n);
    }
    const out = {};
    for (const k of Object.keys(by)) out[k] = by[k].join(', ');
    return out;
}

// routingFieldOf maps a picker kind to the claim field whose options it offers,
// for the two that come from the routing set. `col:staging:*` is the
// mode-dependent cell, whose key names the field.
function routingFieldOf(kind) {
    const parts = String(kind).split(':');
    if (parts[0] === 'source') return 'inbound_source';
    if (parts[0] === 'dest') return 'outbound_destination';
    if (parts[0] === 'col' && parts[1] === 'staging') {
        return parts[2] === 'parkOld' ? 'outbound_staging' : 'inbound_staging';
    }
    return '';
}

// The key route is walked over the plant map (viaWaypoints), so with no map
// its picker is empty for a reason nothing on this page can fix, and it says
// that instead of the generic line.
function pickerEmptyWord(kind) {
    if (String(kind).split(':')[0] === 'via' && !(S.composer && S.composer.map)) return 'No plant map on this Edge yet';
    if (kind === 'position') return 'No free position on this station to move to';
    const note = S.model ? M().routingNote(S.model, routingFieldOf(kind), 0) : '';
    return note || 'Nothing to choose here yet.';
}

// placePopover puts a popover under the control that opened it — or over it,
// and never off the bottom of the screen.
//
// EVERY POPOVER ON THIS PAGE DID THIS ITSELF, seven times, and all seven did
// the same two things: clamp `left` into the host, and set `top` to the
// anchor's bottom. None of them looked at how much room was actually below.
// `.pd-pop` is 320 px tall at most and the app shell is `height: 100vh;
// overflow: hidden` — so a picker opened from a row near the foot of the page
// ran off the bottom with no way to reach the rest, and the only way to read
// your own options was to zoom the browser out or make the window taller.
// Reported from the floor on the inbound source, outbound destination and
// staging pickers, which are the ones lowest on the positions table.
//
// The rule is the one the station's composer had (its popover, torn out with
// it in 681a912c): measure the room above and below the anchor in the VIEWPORT, open
// on the side with room, and cap the height to what is there so the list
// scrolls inside the screen rather than past it.
// `min` is the least room worth opening into; the part popover asks for more
// than a short option list does, because it carries a search and two lists.
function placePopover(pop, btn, min) {
    const GAP = 6, EDGE = 8, MIN = min || 120;
    const r = btn.getBoundingClientRect();
    const host = pop.offsetParent
        ? pop.offsetParent.getBoundingClientRect()
        : { left: 0, top: 0, width: window.innerWidth };
    const below = window.innerHeight - r.bottom - GAP - EDGE;
    const above = r.top - GAP - EDGE;
    // Flip only when going up genuinely helps: a short list that fits below
    // stays below, which is where the eye expects it.
    const flip = below < MIN && above > below;
    const room = Math.max(MIN, flip ? above : below);
    pop.style.maxHeight = room + 'px';
    const w = pop.offsetWidth || 240;
    pop.style.left = Math.max(EDGE, Math.min(host.width - w - EDGE, r.left - host.left)) + 'px';
    pop.style.top = flip
        ? Math.max(EDGE, r.top - host.top - Math.min(pop.offsetHeight, room) - GAP) + 'px'
        : (r.bottom - host.top + GAP) + 'px';
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
    if (kind === 'part') { void openPartPop(btn, node); return; }
    if (kind === 'reorder') { openReorderPop(btn, node); return; }
    const opts = optionsFor(node, kind);
    const pop = $('pd-pop');
    if (!pop) return;
    // A chip clicked while the part popover is open reuses the element.
    pop.classList.remove('pd-partpop');
    S.partPop = null;
    // NEVER AN EMPTY LIST WITH NO REASON (owner ruling, Amendment A). A source,
    // destination or staging picker with nothing in it is a routing set with
    // nothing in that role, and "Nothing to choose here yet" said the symptom
    // while the cause and the next action sat one tab away. The sentence is
    // composer-model's — the station's cell card says the same words — and the
    // kinds it does not know about keep the old line.
    //
    // A LIST OF ONLY `none` IS AN EMPTY LIST. Every optional chip carries a
    // `none` entry to clear it, so a role with nothing to offer still drew one
    // button and so never counted as empty — which would have left the staging
    // picker below silently offering a single `none` where the station says
    // why. The note is keyed on the options that carry a VALUE.
    const note = opts.some(o => o.value) ? '' : pickerEmptyWord(kind);
    // A NODE CHIP IS THE SHARED PICKER (slice 5): a search over the model's own
    // choices, the one on marked, what another position already takes said on
    // its row. Role and swap mode stay the menus below: two to four answers,
    // each with its words, where a search box adds nothing. An empty list
    // keeps its reason and its Add and use, below.
    if (!note && kind !== 'role' && kind !== 'mode') {
        const choices = opts.filter(o => !o.note);
        openRowPicker(pop, btn, choices.map((o, i) => ({
            value: String(i), label: o.label, note: o.use ? 'used by ' + o.use : o.where || '',
        })), v => apply(choices[Number(v)].action), {
            placeholder: 'find a node',
            selected: String(choices.findIndex(o => o.on)),
            head: opts.filter(o => o.note).map(o => esc(o.note)).join(' · '),
        });
        return;
    }
    // An option with a `note` is a line of text, not a choice; `use` marks a
    // choice another position already takes, without disabling it, and
    // `where` says where a choice that is not a routing row stands.
    pop.innerHTML =
        opts.map((o, i) => o.note ? '<div class="none">' + esc(o.note) + '</div>'
            : '<button data-opt="' + i + '" class="' + (o.on ? 'on' : '') + '">' + (o.icon || '') + esc(o.label) +
            (o.use ? '<span class="use"> · used by ' + esc(o.use) + '</span>' : '') +
            (o.where ? '<span class="use"> · ' + esc(o.where) + '</span>' : '') + '</button>').join('') +
        (note ? '<div class="none">' + esc(note) + '</div>' : '');
    showPop(pop, btn);
    placePopover(pop, btn);
    pop.querySelectorAll('[data-opt]').forEach(b => b.addEventListener('click', () => {
        apply(opts[Number(b.dataset.opt)].action);
        closePop();
    }));
    if (note) offerAddAndUse(pop, node, kind);
}

// showPop puts a popover up as an entry on the layer, anchored to the control
// that opened it: Escape closes it before anything under it, a press outside
// it and its control closes it, and it closes with the sheet it was opened in.
// However the layer closes it, closePop clears it.
function showPop(pop, anchor) {
    openLayer(pop, { kind: 'popover', anchor: anchor, focus: false, onClose: why => { if (why !== 'close') closePop(); } });
}

function closePop() {
    const pop = $('pd-pop');
    if (pop) { closeLayer(pop); pop.hidden = true; pop.innerHTML = ''; pop.classList.remove('pd-partpop'); }
    S.partPop = null;
    // SETTINGS' GROUP LIST IS A POPOVER TOO. It draws into #pd-stpop and
    // nothing ever hid it: click Group, change your mind, click elsewhere, and
    // the list stayed open and then floated over the form as the sheet
    // scrolled, until a group was picked or the tab redrew.
    const stpop = $('pd-stpop');
    if (stpop) { closeLayer(stpop); stpop.hidden = true; stpop.innerHTML = ''; }
    // AND SO IS THE SHEETS' OWN. A sheet opens its pickers into #pd-advpop,
    // which rides in the sheet; Escape closes it before the sheet.
    const advpop = $('pd-advpop');
    if (advpop) { closeLayer(advpop); advpop.hidden = true; advpop.innerHTML = ''; }
    // The placeholder that was clicked goes back to being a dash, unless the
    // pick gave it a value — `apply` redraws the whole tab and clears this
    // either way.
    if (S.openAs) { S.openAs = ''; redrawPositionsTable(); }
}

// ── the part popover ─────────────────────────────────────────────────────────
//
// THE PART CHIP IS WHERE PARTS ARE CHANGED. It offered only the process's part
// set, and the one way to grow that set was Edit, a different sheet the picker
// never mentioned — so a payload missing from the list looked like a bug. The
// popover now searches the part set and Core's catalog together: a catalog
// part picked here joins the part set AND lands on this position in one tap.
//
// TWO KINDS OF WRITE, SAID APART. Putting a part on a position is a draft
// edit, saved with Save style like every other chip. Adding a part to the
// process, or taking one out with ×, writes the part set straight away — it is
// the same PUT the Edit sheet sends, and it is not part of any one flow.
async function openPartPop(btn, node) {
    const pop = $('pd-pop');
    if (!pop || !S.model) return;
    S.partPop = { node: node, q: '', note: '', catalogFailed: false, catalog: false, anchor: btn };
    pop.classList.add('pd-partpop');
    pop.innerHTML =
        '<input class="pd-npkq pd-partq" type="text" autocomplete="off" ' +
        'placeholder="Find a part, or add one from Core’s catalog">' +
        '<div class="pd-partlist" id="pd-partlist"></div>';
    showPop(pop, btn);
    drawPartList();
    placePopover(pop, btn, 300);
    const q = pop.querySelector('.pd-partq');
    q.addEventListener('input', () => {
        if (!S.partPop) return;
        S.partPop.q = q.value;
        S.partPop.note = '';
        drawPartList();
    });
    q.focus();
    pop.onclick = onPartPopClick;
    await Promise.all([loadPayloadCatalog(), loadPartSet()]);
    if (!S.partPop || S.partPop.node !== node) return;
    S.partPop.catalogFailed = !S.payloadCatalog;
    drawPartList();
    // The catalog made the list taller, so it is placed again: a popover
    // flipped above its chip at the short height would now hang over it.
    if (btn.isConnected) placePopover(pop, btn, 300);
}

async function loadPartSet() {
    if (S.partSet) return;
    S.partSet = await loadProcessPayloads(S.processID);
}

// partPlace is where a part sits in the draft, or '' when it has no position.
function partPlace(code) {
    for (const n of Object.keys(S.model.cells)) {
        if (S.model.cells[n].part === code) return n;
    }
    return '';
}

// partUsedElsewhere names the OTHER saved flows of this process that run a
// part — the half of the palette the part set cannot take away (see
// store/processes.ProcessPalette). The open flow is the draft's to answer.
function partUsedElsewhere(code) {
    const out = [];
    for (const st of S.composer.styles) {
        if (st.id === S.styleID) continue;
        for (const p of M().styleFacts(st).parts) {
            if ((p.payload_code || p) === code) out.push((st.name || 'style ' + st.id) + (p.node ? ' at ' + p.node : ''));
        }
    }
    return out;
}

function drawPartList() {
    const box = $('pd-partlist');
    const pp = S.partPop;
    if (!box || !pp || !S.model) return;
    const c = S.model.cells[pp.node] || {};
    const needle = pp.q.trim().toLowerCase();
    const hit = (code, name) => !needle || String(code).toLowerCase().indexOf(needle) >= 0 ||
        String(name || '').toLowerCase().indexOf(needle) >= 0;
    const stored = new Set(S.partSet || []);
    const offers = M().partOffers(S.model).filter(r => hit(r.code));
    const palette = new Set(S.model.palette || []);

    let h = '<div class="pd-lbl">This process’s parts</div>';
    if (!offers.length) {
        h += '<div class="none">' + (needle ? 'None of this process’s parts match.'
            : 'No parts yet. Pick one from Core’s catalog below.') + '</div>';
    }
    for (const r of offers) {
        const here = c.part === r.code;
        const at = here ? '' : partPlace(r.code);
        const sub = here ? 'on this position' : at ? 'on ' + at + ', moves here' : '';
        h += '<div class="pd-partrow">' +
            '<button class="' + (here ? 'on' : '') + '" data-pp="place" data-code="' + esc(r.code) + '" title="' + esc(r.code) + '">' +
            '<span class="t">' + esc(M().shortPart(r.code)) + '</span>' +
            (sub ? '<small>' + esc(sub) + '</small>' : '') + '</button>' +
            (stored.has(r.code)
                ? '<button class="x" data-pp="drop" data-code="' + esc(r.code) + '" ' +
                'title="Take ' + esc(r.code) + ' out of this process’s parts" aria-label="Take ' +
                esc(r.code) + ' out of this process’s parts">&times;</button>'
                : '') +
            '</div>';
    }

    // CORE'S CATALOG IS FOR THE PART SOMEBODY FORGOT (owner, 2026-11-12). The
    // parts were dialled in at Setup, so the list is those, and a few hundred
    // catalog rows under them buried it. The catalog opens on its own button,
    // on a search, or when the process has no parts to offer yet.
    if (!pp.catalog && !needle && palette.size) {
        h += '<div class="pd-partrow"><button data-pp="catalog">+ Add from Core’s catalog…</button></div>';
    } else {
        h += catalogRows(pp, palette, needle, hit);
    }

    if (c.part) {
        h += '<div class="pd-partfoot"><button data-pp="clear">Take ' + esc(M().shortPart(c.part)) +
            ' off this position</button></div>';
    }
    if (pp.note) h += '<div class="none pd-partnote">' + esc(pp.note) + '</div>';
    box.innerHTML = h;
}

// catalogRows is the popover's second half: Core's catalog less the parts the
// process already has, searched by the same needle.
function catalogRows(pp, palette, needle, hit) {
    let h = '<div class="pd-lbl">Add from Core’s catalog</div>';
    if (pp.catalogFailed) {
        h += '<div class="none">Core’s catalog could not be read. Reload the page to try again.</div>';
    } else if (!S.payloadCatalog) {
        h += '<div class="none loading">Reading Core’s catalog…</div>';
    } else {
        const LIMIT = 40;
        const more = payloadCatalogList()
            .map(e => ({ code: String(e.code || e.name || ''), name: e.name || '' }))
            .filter(e => e.code && !palette.has(e.code) && hit(e.code, e.name));
        if (!more.length) {
            h += '<div class="none">' + (needle ? 'Nothing in Core’s catalog matches. A part Core does not ' +
                'have has to be made in Core first.' : 'Every part in Core’s catalog is already here.') + '</div>';
        }
        for (const e of more.slice(0, LIMIT)) {
            h += '<div class="pd-partrow"><button data-pp="add" data-code="' + esc(e.code) + '" title="' + esc(e.code) + '">' +
                '<span class="t">+ ' + esc(e.code) + '</span>' +
                (e.name && e.name !== e.code ? '<small>' + esc(e.name) + '</small>' : '') + '</button></div>';
        }
        if (more.length > LIMIT) {
            h += '<div class="none">' + (more.length - LIMIT) + ' more — type to narrow the list.</div>';
        }
    }
    return h;
}

// The popover redraws its own list, so a click inside it can land on a button
// that is gone by the time the document's closer asks where it was. Stopping
// it here keeps the popover open for the clicks that mean to.
function onPartPopClick(e) {
    const b = e.target.closest && e.target.closest('[data-pp]');
    if (!S.partPop) return;
    e.stopPropagation();
    if (!b) return;
    const node = S.partPop.node, code = b.dataset.code;
    switch (b.dataset.pp) {
        case 'place':
            closePop();
            apply([{ type: 'addPart', payloadCode: code }, { type: 'setPart', node: node, payloadCode: code }]);
            return;
        case 'clear': {
            const cur = (S.model.cells[node] || {}).part;
            closePop();
            // One part is on one position (setPart moves it), so taking it off
            // the position is taking it off the flow; removePart does both and
            // leaves no part on the flow with nowhere to go.
            if (cur) apply({ type: 'removePart', payloadCode: cur });
            return;
        }
        case 'add': void addToPartSet(node, code); return;
        case 'drop': void dropFromPartSet(code); return;
        case 'catalog': {
            S.partPop.catalog = true;
            drawPartList();
            // Taller now, so placed again, as when the catalog first arrives.
            const pop = $('pd-pop'), at = S.partPop.anchor;
            if (pop && at && at.isConnected) placePopover(pop, at, 300);
            const q = pop && pop.querySelector('.pd-partq');
            if (q) q.focus();
            return;
        }
        default: return;
    }
}

function partNote(text) {
    if (!S.partPop) return;
    S.partPop.note = text;
    drawPartList();
}

async function writePartSet(codes) {
    const out = await send('PUT', '/api/processes/' + S.processID + '/payloads', B().processPayloads(codes), NO_NOTICE);
    if (!out.ok) return out;
    S.partSet = codes.slice().sort();
    // The palette is the server's union with what saved flows run, so it is
    // read back rather than guessed at here.
    // A failed read leaves the list as it was: the write landed, and the
    // list catches up on the next open.
    const res = await send('GET', composerURL(S.processID));
    if (res.ok) {
        const pal = (res.body || {}).palette || [];
        S.composer.palette = pal;
        if (S.model) S.model.palette = pal.slice();
    }
    return out;
}

async function addToPartSet(node, code) {
    if (!S.partSet) { partNote('This process’s part list could not be read, so it cannot be changed here. Reload the page.'); return; }
    const out = await writePartSet(S.partSet.indexOf(code) >= 0 ? S.partSet : S.partSet.concat([code]));
    if (!out.ok) { partNote(said(out, 'Could not add ' + code + ': ')); return; }
    closePop();
    apply([{ type: 'addPart', payloadCode: code }, { type: 'setPart', node: node, payloadCode: code }]);
}

async function dropFromPartSet(code) {
    if (!S.partSet) { partNote('This process’s part list could not be read, so it cannot be changed here. Reload the page.'); return; }
    const at = partPlace(code);
    if (at) {
        partNote(M().shortPart(code) + ' is on ' + (at === S.partPop.node ? 'this position' : at) +
            '. Take it off there first.');
        return;
    }
    const elsewhere = partUsedElsewhere(code);
    if (elsewhere.length) {
        partNote(M().shortPart(code) + ' is still used by ' + elsewhere.join(', ') +
            '. Take it off there first.');
        return;
    }
    const out = await writePartSet(S.partSet.filter(c => c !== code));
    if (!out.ok) { partNote(said(out, 'Could not take ' + code + ' out: ')); return; }
    // Still listed means a SAVED version of this flow runs it; the draft does
    // not, so it goes when this flow is saved.
    partNote((S.model.palette || []).indexOf(code) >= 0
        ? M().shortPart(code) + ' is out of the part list. It stays here until this flow is saved.'
        : '');
}

// Every picker applies instantly to the DRAFT; nothing is written until Save
// flow (SPEC §3).
//
// A LIST OF ACTIONS IS FOLDED, which is what a reducer is for and is not a new
// protocol: the part picker sends `addPart` (the part joins this style) and
// then `setPart` (it goes on this position), because those are two facts and
// the model already has an action for each. Overloading either one to do both
// would be the seam this pair exists to keep straight — addPart is the door
// onto the part set, setPart is the placement.
function apply(action) {
    for (const a of (Array.isArray(action) ? action : [action])) {
        const before = S.model;
        S.model = M().reduce(S.model, a);
        if (a.type === 'setMode') sayDropped(a.node, before, S.model);
    }
    drawFlows();
    schedulePreview();
}

// MODE_FIELDS is the cell fields a swap-mode change can clear (the model's
// clearForbidden), each with the claim field it is.
const MODE_FIELDS = [['paired', 'paired_core_node'], ['secondPaired', 'second_paired_core_node'],
    ['staging', 'inbound_staging'], ['parkOld', 'outbound_staging'], ['source', 'inbound_source'],
    ['dest', 'outbound_destination'], ['partialDest', 'partial_destination'],
    ['evacNodes', 'changeover_evac_nodes'], ['keyRoute', 'key_route']];

// sayDropped says what a swap-mode change took off the position (F26): the
// model keeps every value the new mode may carry and clears the ones it
// forbids, and that clearing was silent. One line on the notice, the value
// named, so an engineer who switches back knows what to put again.
function sayDropped(node, before, after) {
    const was = before.cells[node], now = after.cells[node];
    if (!was || !now) return;
    const gone = [];
    for (const [k, field] of MODE_FIELDS) {
        const v = Array.isArray(was[k]) ? was[k].join(' › ') : was[k];
        const left = Array.isArray(now[k]) ? now[k].length : now[k];
        if (v && !left) gone.push(W(field) + ' ' + v);
    }
    const a0 = M().advancedFor(before, node), a1 = M().advancedFor(after, node);
    for (const k of Object.keys(a0)) {
        if (advIsSet(k, { a: a0 }) && !advIsSet(k, { a: a1 })) gone.push(W(k));
    }
    if (!gone.length) return;
    toast((M().modeLabels()[now.mode] || now.mode) + ' does not use ' + gone.join(', ') + ': dropped from ' + node + '.', 'info');
}

// ── D3 · the routing set's roles, and the key route's waypoints ──────────────
//
// The routing set is the places this press may draw bins from, stage them at
// and send them to, picked by name off one list in four boxes (the set pick,
// shared/set-pick.js).
//
// IT HAS NO PLANT MAP ANY MORE. Settings and the process sheet drew the set,
// its two route legs and the press on Core's plant behind Show on map; that
// drawing was removed (2026-11-14, the shared-map brief's tier A1). Its data
// stays: the composer read still carries the ComposerMap, the key-route walk's
// network.

// waypointNames is the waypoints the composer offers for the first live
// position with a mode: the LM points along its supply path, evenly spaced, at
// most four, from the model's walk over the shared route model
// (composer-model.js viaWaypoints). Settings' Waypoints line reads it.
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
//
// THE WORDS ON SCREEN ARE INBOUND AND OUTBOUND, the keys stay source and
// destination. The keys are the data's, the API's and every test's; the words
// are what an engineer reads, and "Sources" beside "Destinations" read as two
// unrelated lists rather than the two ends of one trip.
//
// THE WORDS SAY WHERE THE BIN IS, for produce and consume alike (owner,
// 2026-11-15: no "full" bin, no "finished" one, "just where the bin comes
// from"): a consume line's inbound brings a full bin and a produce line's an
// empty one, and the role is the same place either way.
const ROUTING_GROUPS = [
    ['source', 'Inbound', 'where the bin comes from'],
    ['staging', 'Staging', 'where a bin waits'],
    ['destination', 'Outbound', 'where the bin goes'],
];

function isPositionRow(name) {
    return S.composer.cell.positions.some(p => p.core_node_name === name);
}

// SET_BOXES are the set pick's four (shared/set-pick.js): positions, then the
// three roles, each in its role's colour (the --role-* tokens). Positions,
// Inbound and Outbound are needed for a flow to have anywhere to work; Staging
// only some swaps use.
const ROLE_OF = { positions: 'positions', source: 'inbound', staging: 'staging', destination: 'outbound' };
const SET_BOXES = [['positions', 'Positions', 'the spots this process works']].concat(ROUTING_GROUPS)
    .map(g => ({ key: g[0], label: g[1], sub: g[2], role: ROLE_OF[g[0]], need: g[0] === 'staging' ? 'optional' : 'needed' }));

// ── the set pick on this page (shared/set-pick.js) ───────────────────────────
//
// ONE PIECE, TWO PLACES: the process sheet (Add process and its Change…) and
// Settings' Positions and routing. Each is S.setPicks[key] = {pid, api}, the
// pick for process pid (0 for one not made yet). What a box may take is its
// field (setPickField):
//   - Positions: a node another process holds is greyed with its name; a
//     group's row folds its nodes, which are positions one at a time.
//   - Inbound and Outbound: a position of this process is greyed (every flow
//     on it may use it already), and a group's members are not drawn: where
//     Core picks, a source or destination names the group (legExclusion).
//   - Staging: the same, except that a staging spot names the exact node, so
//     a group's nodes fold under it, pickable once shown.
// A node another process holds is behind "In use by other processes".
//
// TICKING A NODE INTO POSITIONS TAKES IT OUT OF THE ROLES (as the sheet did
// before it was shared): one node is a position or a routing name, never both
// (ErrRoutingNodeIsPosition).
const ST_SET = 'st';
const HELD_ELSEWHERE = 'In use by other processes';

function setPickValue(key, box) {
    const sp = S.setPicks[key];
    return sp ? sp.api.value(box) : [];
}

// heldElsewhere: another process holds name as a position, and no Core
// loader shares it (a shared loader window may be on several).
function heldElsewhere(name, pid) {
    return (S.livePositions || []).some(n => n.core_node_name === name && n.process_id !== pid) && !sharedLoaderNode(name);
}

// A group is never a position itself: in the Positions box its row is greyed
// and the reason is said once, above the list (the set pick's hint).
const GROUP_POSITIONS = 'a group: its nodes are positions one at a time';
function isPickGroup(n) { return n.type === 'NGRP' || isGroupRow(n.raw); }

function setPickField(key, box, pid) {
    const later = n => heldElsewhere(n.name, pid);
    if (box === 'positions') {
        return {
            rule: n => (isPickGroup(n) ? GROUP_POSITIONS : positionHeld(n.name, pid)),
            quiet: isPickGroup,
            note: n => {
                const other = (S.livePositions || []).find(x => x.core_node_name === n.name && x.process_id !== pid);
                return other && sharedLoaderNode(n.name) ? 'loader window · also on ' + other.process_name : '';
            },
            members: 'fold', later: later,
        };
    }
    return {
        rule: n => (setPickValue(key, 'positions').indexOf(n.name) >= 0
            ? 'a position of this process — available to every flow on it already'
            : B().legExclusion(S.coreNodes, n.name, box)),
        note: n => (pid ? routeNote(box, n.name) : ''),
        members: box === 'staging' ? 'fold' : 'none', later: later,
    };
}

// mountSetPick draws pick `key` into the element hostId for process o.pid,
// from sel ({box: [names]}). A pick already drawn for that process is moved
// into the new host, not drawn again, so a redraw of the page around it keeps
// its open box, its search and its scroll; o.fresh draws it anew.
function mountSetPick(key, hostId, sel, o) {
    const host = $(hostId);
    if (!host) return null;
    const have = S.setPicks[key];
    if (have && have.pid === o.pid && !o.fresh) {
        host.appendChild(have.api.el);
        return have.api;
    }
    if (have) have.api.destroy();
    const api = createSetPick(host, {
        boxes: SET_BOXES, source: NODES, selected: sel, active: o.active,
        field: box => setPickField(key, box, o.pid),
        closed: o.closed, note: o.note,
        hint: b => (b === 'positions' ? 'A group is one row: its nodes are positions one at a time. Show a group to pick one.' : ''),
        later: { label: HELD_ELSEWHERE },
        placeholder: 'filter by name',
        onChange: (box, sp) => {
            if (box === 'positions') {
                const pos = sp.value('positions');
                for (const g of ROUTING_GROUPS) {
                    const was = sp.value(g[0]);
                    const left = was.filter(n => pos.indexOf(n) < 0);
                    if (left.length !== was.length) sp.set(g[0], left);
                }
            }
            if (o.onChange) o.onChange(box);
        },
    });
    S.setPicks[key] = { pid: o.pid, api: api };
    loadCoreNodes();
    return api;
}

function dropSetPick(key) {
    const sp = S.setPicks[key];
    if (sp) sp.api.destroy();
    delete S.setPicks[key];
}

// stillNeeded is the boxes of a pick that a line needs and that are empty, by
// name: Positions, Inbound, Outbound.
function stillNeeded(key) {
    return SET_BOXES.filter(b => b.need === 'needed' && !setPickValue(key, b.key).length).map(b => b.label);
}

// processPositions is the open process's positions: each screen's in its
// order, then those on no screen.
function processPositions() {
    const out = [];
    for (const st of stationsOf(S.processID)) {
        for (const n of S.stationNodes[String(st.id)] || []) if (out.indexOf(n) < 0) out.push(n);
    }
    for (const n of loosePositions()) if (out.indexOf(n) < 0) out.push(n);
    return out;
}

// screensDraft is a positions box as savePositions' draft ({screen id: its
// list, none: the positions on no screen}). A position stays where it is; a
// new one goes on the line's screen (the first, on a line that has two from
// before); on a line with no screen it is on none. onto: a screen's id, whose
// list takes the positions on no screen too ("Put them on <screen>").
function screensDraft(names, onto) {
    const screens = stationsOf(S.processID).filter(st => String(st.id) in S.stationNodes);
    const loose = loosePositions();
    const d = {};
    const placed = new Set();
    for (const st of screens) {
        d[st.id] = (S.stationNodes[String(st.id)] || []).filter(n => names.indexOf(n) >= 0);
        d[st.id].forEach(n => placed.add(n));
    }
    const to = onto ? screens.find(st => String(st.id) === String(onto)) : null;
    const stay = to ? [] : loose.filter(n => names.indexOf(n) >= 0);
    stay.forEach(n => placed.add(n));
    if (to) {
        d[to.id] = d[to.id].concat(loose.filter(n => names.indexOf(n) >= 0 && !placed.has(n)));
        d[to.id].forEach(n => placed.add(n));
    }
    const fresh = names.filter(n => !placed.has(n));
    if (screens.length) d[screens[0].id] = d[screens[0].id].concat(fresh);
    if (loose.length || !screens.length) d.none = stay.concat(screens.length ? [] : fresh);
    return d;
}

// WHAT BACKFILL MEANS, IN WORDS, ON THE ROW (owner ruling 2026-09-10, Q5).
//
// The row used to carry an origin PILL reading "backfill" and, when it was
// off, an "Adopt" button beside it. Neither said anything: "backfill" is a
// word from the implementation, and an engineer reading it learns that some
// process put the name there, not which of their own flows did. And Adopt was
// a second control for what the switch already does.
//
// So: no pills. Where a name the backfill found came from, with the evidence
// — the role it plays in a claim and how many live styles play it — is said
// in words (below). The server stamps the authorship.
const ROLE_EVIDENCE = {
    source: 'inbound source',
    staging: 'staging',
    destination: 'outbound destination',
};

// ROLE_EVIDENCE is what a backfilled name PLAYS, in the words the derive used,
// and it is the whole of Q5's evidence line now: the picker annotates an
// unadopted name with its role and how many live styles route through it, at
// the moment an engineer is deciding whether to pick it. The row renderer that
// used to carry it — with a switch, a provenance line and a members line per
// name — went with the per-row switches (owner, 2026-09-16 second pass).

// ── the routing set, in Settings (owner ruling 2026-09-16) ───────────────────
//
// THE ROUTING SET HAD A TAB, AND CREATION IS NOT REVIEW. That tab was built to
// read backfilled rows on a migrated process — the derive endpoint, the amber
// switch, the evidence line under a name — and every one of those is a REVIEW,
// which is what Settings is for. Making a routing set is three short lists on
// the Add-process sheet and is over in a minute. So the review lives here,
// beside the gate it is the precondition for.
//
// THREE ROLES, NOT ONE SET OF NAMES, because composer-model's defaultRouting
// picks a cell's source and destination by lowest sequence PER ROLE: source,
// staging and destination are different questions. On Settings each is its
// own picker (routingSection).

// MEMBERSHIP IS THE SWITCH (owner ruling 2026-09-16, second pass).
//
// This was three lists of rows, each row carrying its own on/off switch, a
// provenance sub-line and a members line, with a picker under each list. The
// owner's verdict on it was "a bit of a mess" and "over-engineering": putting
// a node in the set IS the intent to use it, so a second control asking
// whether you meant it is a decision drawn twice. The whole-set on/off already
// exists and is the flow-composer gate directly above this section.
//
// So each role's chips are the set. Picking a name puts it in the role; the
// chip's × takes it out.
//
// OUT MEANS DELETED, and it always works: what the removal orphans comes back
// as a warning (the routing write's warnings, said after the save).
//
// THE EVIDENCE MOVED INTO THE PICKER rather than being deleted. Q5's sub-line
// — where a name came from and how many styles use it — is what an engineer
// adopting a backfilled name needs, and it is needed AT THE MOMENT OF PICKING,
// not as a permanent line under a name already chosen. It is the picker row's
// annotation now, which is both shorter and better timed.

function routingRowsFor(role) {
    return (S.routing || []).filter(r => r.role === role && r.enabled);
}

// Whether the set was actually read. null is "the read failed" — see
// loadRouting — and the section says so instead of drawing three empty roles.
function routingUnread() { return S.routing === null; }

// The press's own back positions are staging and are NOT routing rows: the
// derive reads inbound_staging / outbound_staging, and a press-index cell
// parks on its PAIRED position instead, deliberately, because a paired
// position is choreography and not a routing choice. They are named in the
// sub-line rather than drawn as chips, because a chip with no × is a control
// that does not work.
function alwaysAvailableStaging() {
    return S.composer.cell.positions
        .filter(pos => pos.kind === 'back')
        .map(pos => pos.core_node_name);
}

// switchedOffIn is the rows of a role the process HAS and has not switched on
// — names taken out with a chip's ×, or landed off by the backfill before
// 2026-09-16 — which are invisible to the picker above (it
// draws the enabled rows) and to the HMI (the composer filters to them).
function switchedOffIn(role) {
    return (S.routing || []).filter(r => r.role === role && !r.enabled);
}

// allOffLine is one role's names the process has and has not switched on,
// with the one button that switches them all on.
//
// TURN ALL ON, PER ROLE (SYNTH §3 B6). The backfill landed derived names
// DISABLED until 2026-09-16, and nothing re-derives now (migration v25 runs
// once, only for a process with no rows): a row nobody switched on stays off
// forever, and the only way back is one PATCH per row by hand. The 4x2 arrived
// with seven staging names in exactly that state — the composer's own card said
// so, by name — and there was nowhere on this screen to say yes to all seven.
//
// NAMED, NOT COUNTED, and the button carries the count because the sentence
// beside it carries the names: an engineer agreeing to seven wants to know it
// is seven of the names they are looking at.
function allOffLine(g) {
    const picked = setPickValue(ST_SET, g[0]);
    const off = switchedOffIn(g[0]).filter(r => picked.indexOf(r.core_node_name) < 0);
    if (!off.length) return '';
    return '<div class="pd-rs-alloff"><span class="pd-dim">' +
        esc(g[1] + ': ' + off.length + ' ' + (off.length === 1 ? 'name' : 'names') +
            ' found from your flows, switched off — ' + off.map(r => r.core_node_name).join(', ')) +
        '</span><button class="pd-btn quiet" data-act="rs-all-on" data-role="' + esc(g[0]) + '">' +
        'Turn all ' + off.length + ' on</button></div>';
}

// rsAllOn puts every switched-off name of one role into its picker's draft;
// the save switches them on (the routing write adopts a row it names).
function rsAllOn(role) {
    const sp = S.setPicks[ST_SET];
    if (!sp) return;
    const have = sp.api.value(role);
    sp.api.set(role, have.concat(switchedOffIn(role).map(r => r.core_node_name).filter(n => have.indexOf(n) < 0)));
    drawSettings();
}

async function loadRouting() {
    const res = await send('GET', '/api/processes/' + S.processID + '/routing-nodes', undefined, NO_NOTICE);
    // A READ THAT FAILED IS NOT AN EMPTY SET. This swallowed !res.ok into {},
    // so a 500 or a dropped connection drew three roles of "nothing picked
    // yet" and an empty summary — indistinguishable
    // from a process that genuinely has no routing set, and permanently so,
    // because S.routing was then [] rather than null and openSettings never
    // retried. An engineer could have "re-added" names that were already
    // there. Say what happened and leave the set unread so the next open asks
    // again.
    if (!res.ok) {
        // The server's own sentence when it gave one, else the door's.
        const why = res.shown ? '' : res.body ? res.error : 'Could not read this process’s routing set: ' + res.error + '.';
        S.routingError = (why ? why + ' ' : '') + 'Nothing below is the set — it is what could not be read.';
        S.routing = null;
        return;
    }
    const view = res.body || {};
    const rows = Array.isArray(view) ? view : (view.rows || view.routing_nodes || []);
    S.routing = rows;
    S.routingError = '';
}

// The bar's problem line, clicked: the finding's position becomes the
// selection — the same state a card click or a row click sets, so the
// picture, the table and the panel all agree — and the table row scrolls
// into view. Nothing else changes: no model action, no write.
//
// The row scrolls into view in the MAIN COLUMN, which is the one vertical
// scroller, and the column has the table's headings stuck to its top and the
// bar stuck to its foot: a row scrolled to either edge would land under one of
// them. The column reserves both rendered heights first — measured, because
// the headings wrap to two lines at some widths and not at others — and
// scrollIntoView honours that padding.
function gotoFinding(node) {
    S.selected = node;
    drawFlows();
    const col = root().querySelector('.pd-main');
    const head = $('pd-poshead');
    const bar = $('pd-bar');
    if (col) {
        if (head) col.style.scrollPaddingTop = head.offsetHeight + 'px';
        if (bar) col.style.scrollPaddingBottom = bar.offsetHeight + 'px';
    }
    const row = root().querySelector('[data-row="' + CSS.escape(node) + '"]');
    if (row) row.scrollIntoView({ block: 'nearest' });
}

// A click on the selected position lets go of it; any other click goes to it.
// THE SELECTION IS THE MODULE'S: the picture outlines every card and slot of
// the selected position's module, the on-deck card of a press index included,
// so a click on any of them is a click on what is selected. Membership is the
// picture's own (moduleOf), not a second reading of the pairing rules.
function toggleSelected(node) {
    if (S.selected && (S.selected === node ||
        (S.model && moduleOf(cellFromModel(), S.selected).indexOf(node) >= 0))) {
        clearSelected();
        return;
    }
    gotoFinding(node);
}

function clearSelected() {
    S.selected = null;
    drawFlows();
}

// openSettings reads what Settings draws and draws it. kept is a re-read
// under an open draft (refreshProcess): the sections in it keep their drafts,
// and the rest are taken as saved.
async function openSettings(kept) {
    S.settings = S.settings || settingsDraft();
    if (!S.routing) await loadRouting();
    await Promise.all([loadCurtainNodes(), loadPayloadCatalog(), loadPartSet(), loadStationCounters(), loadCountingSlots(),
        loadCatidItems(), loadPLCs(), loadPresets(), loadAutoReorder()]);
    if (kept && S.stShell) {
        takeSettingsRead(kept);
        drawSettings();
        S.stShell.page.rebase(Object.keys(SETTINGS_SECTIONS).filter(n => !(n in kept)));
        return;
    }
    // THE DRAFTS: each screen's positions, each routing role, the part set
    // (pick-a-part U5: the parts the floor may run), saved by the one Save.
    settingsPickers();
    drawSettings();
}

// ── Set for this style, and a recipe group's settings ───────────────────────
//
// WHAT A POSITION SETS FOR ONE STYLE, AND A GROUP FOR A RECIPE, ARE EXCEPTION
// LISTS (shared/exceptions.js): one per setting that is set, under the
// positions table and on the group's card. They replace the Advanced sheet,
// which drew every field at its default behind a gear on each row, so one
// setting on one position was a sheet of a dozen "off". A setting is listed
// only where it is set, "+ Add a setting" offers the rest, and every edit is
// a draft edit: a style's through the model (setAdvanced, setEvacNodes,
// setPartialDest, as the sheet's Apply sent them), saved with Save style; a
// group's in its `advanced` (domain.RecipeAdvanced), saved with Save recipe.
//
// WHAT IS OFFERED IS flowspec's DECISION, not a list here: a field this
// choreography forbids or never reads is not offered (advShows).

// SET_SECTIONS are the settings in the order "+ Add a setting" lists them,
// under the headings it lists them by. AUTO REORDER (`reorder`, the page's own
// key over reorder_point and auto_reorder) is a group's only, one row reading
// "on at <point>" as a style's position reads it on its row (reorderCell). The
// pack tolerance is strict pack's second value and the carry-over is Tool
// change's, so neither is a setting of its own. BOARD ORDER IS NOT HERE: it is
// drawn on a loader board's claim (manual_swap), and no stored claim is one
// (protocol.ConfigurableSwapModes).
const SET_SECTIONS = [
    ['Part identity', ['allowed_payload_codes']],
    ['Replenishment', ['reorder', 'lineside_soft_threshold', 'auto_request_payload', 'auto_push', 'keep_staged_node']],
    ['Tool change', ['evac_nodes']],
    ['Finished goods', ['strict_pack', 'partial_dest']],
    ['Press hardware', ['refill_by']],
    ['Light curtain', ['curtain_required']],
    ['Station policy', ['auto_confirm']],
];

// SET_COPY is each setting's claim field (its name, from flowspec, like every
// word on this page) and the one line of help under it, kept only where it
// changes what a person would pick. Two keys are the page's own (evac_nodes,
// partial_dest); the field each edits is named here rather than guessed.
const SET_COPY = {
    // WHAT ITS READERS DO: each takes a carrier, pile or bin of a listed part
    // as this position's own (the changeover gate, counting, the lineside
    // piles, Core's sourceability); none brings a second part to the line.
    allowed_payload_codes: ['allowed_payload_codes', 'other parts this position counts as its own'],
    reorder: ['auto_reorder', REORDER_WORDS],
    lineside_soft_threshold: ['lineside_soft_threshold', 'warn the operator above twice this on a release'],
    auto_request_payload: ['auto_request_payload', 'the part a vacated position asks for on its own'],
    auto_push: ['auto_push', 'drain the source whenever the window is free'],
    keep_staged_node: ['keep_staged_node', 'a spare waits here, and the full or empty comes from it instead of the market'],
    evac_nodes: ['changeover_evac_nodes', 'cleared for a setup; the changeover waits for Tooling done'],
    strict_pack: ['strict_pack', 'counted only up to its pack; parts made past it wait for the next bin'],
    partial_dest: ['partial_destination', 'where a bin released short goes; a node group holds it to be finished, an unloader unloads it'],
    // WHICH ROBOT, FOR THIS PART. It said "the same for every part on this
    // press", and it is stored per style and differs by part (owner,
    // 2026-11-14); what each answer does is the position's work list
// (workListHTML), and on a recipe group, which builds none, REFILL_NOTE.
    refill_by: ['refill_by', 'which robot brings this part’s next carrier'],
    curtain_required: ['curtain_required', 'a full-bin release here waits for the node’s light curtain'],
    auto_confirm: ['auto_confirm', 'on here, whatever the plant setting'],
};

// The settings a switch is: the row is the setting, so an add turns it on and
// × turns it off.
const SET_SWITCHES = ['auto_push', 'strict_pack', 'curtain_required', 'auto_confirm'];

// CARRYOVER is the carry-over's three answers, said as what happens to the
// bin on a position marked for a tool change when the next style runs the
// same part there (domain.ClearanceTreatmentAtNode, which the changeover
// reads from the style being left): replace clears it and another carrier
// comes back, keep_lineside leaves it, outbound_staging walks it to this
// position's outbound staging and back once Tooling done is pressed. A part
// that changes is cleared whatever this says. It read "what happens to a
// part-full bin", and nothing on that path reads fill.
const CARRYOVER = { replace: 'Fresh bin', keep_lineside: 'Keep it', outbound_staging: 'Out and back' };
const CARRYOVER_NOTE = 'If the part stays: Fresh bin clears it, Keep it leaves it, Out and back parks it at ' +
    'outbound staging until Tooling done. A part that changes is always cleared.';

// setKeys is the settings a scope lists: a style's (not the two its row
// carries) or a group's (GROUP_ADV_KEYS: no part, no cell fields).
function setKeys(scope) {
    const all = [].concat(...SET_SECTIONS.map(s => s[1]));
    return scope === 'group' ? all.filter(k => k === 'reorder' || GROUP_ADV_KEYS.indexOf(k) >= 0)
        : all.filter(k => k !== 'reorder');
}

// A PLACE is where a setting is set: { node } for a style's position, { g }
// for a recipe group (its index on the draft).
function placeKey(p) { return p.g !== undefined ? 'g:' + p.g : p.node; }

// setCtx is one place's settings, read fresh: { node, a, evacNodes,
// partialDest, group }. A group's are its ★ cell's defaults with its own
// `advanced` laid over them, as the mint lays them.
function setCtx(p) {
    if (p.g !== undefined) {
        const g = S.shapeDraft.groups[p.g];
        const a = M().advancedFor(S.model, g.of);
        for (const key of GROUP_ADV_KEYS) {
            if (g.advanced && g.advanced[key] !== undefined && g.advanced[key] !== null) a[key] = g.advanced[key];
        }
        return { node: g.of, group: p.g, a: a, evacNodes: [], partialDest: '' };
    }
    const c = S.model.cells[p.node] || {};
    return { node: p.node, a: M().advancedFor(S.model, p.node), evacNodes: (c.evacNodes || []).slice(), partialDest: c.partialDest || '' };
}

// advShows is whether a setting is offered at a place (ctx, setCtx's).
function advShows(key, d) {
    // The tolerance is read only through strict pack, so it is drawn only
    // while strict pack is on, in either scope. A stored value stays when the
    // cap is switched off; nothing reads it then.
    if (key === 'pack_tolerance_pct' && !d.a.strict_pack) return false;
    // THE CARRY-OVER IS A TOOL CHANGE'S SECOND VALUE: read only for a marked
    // position (domain.ClearanceTreatmentAtNode), and refused on a claim that
    // marks none, so it is drawn only beside marks. A group marks none.
    if (key === 'changeover_carryover_disposition' && (d.group !== undefined || !d.evacNodes.length)) return false;
    if (key === 'reorder') return d.group !== undefined && M().advancedShows(S.model, d.node, 'auto_reorder');
    if (d.group !== undefined) return GROUP_ADV_KEYS.indexOf(key) >= 0 && M().advancedShows(S.model, d.node, key);
    if (key === 'evac_nodes') return M().advancedShows(S.model, d.node, 'changeover_evac_nodes');
    if (key === 'partial_dest') return M().advancedShows(S.model, d.node, 'partial_destination');
    // THE ROW CARRIES AUTO REORDER (reorderCell), so a position does not list
    // it twice; a recipe group, which has no row, does.
    if (key === 'reorder_point' || key === 'auto_reorder') return false;
    // F15: BOARD ORDER IS A LOADER BOARD'S, drawn on a loader position only
    // (a manual_swap claim, which a loader board draws).
    if (key === 'sequence' && (S.model.cells[d.node] || {}).mode !== 'manual_swap') return false;
    // F15: A CURTAIN TIE NEEDS A CURTAIN. Offered where the node has one wired
    // (the cell picture's `curtain`, from the node's curtain switch). ONE SET
    // WHERE NONE IS WIRED IS STILL LISTED, so it can be seen and taken off: a
    // recipe group lays the flag on every claim it mints, curtain or not, and
    // it waits for nothing until a curtain is switched on there, when every
    // such style is gated at once (release_load.go reads the node's switch
    // before the flag). It was hidden here and could not be cleared.
    if (key === 'curtain_required' && !hasCurtain(d.node)) return advIsSet(key, d);
    return M().advancedShows(S.model, d.node, key);
}

// hasCurtain is whether the node's light curtain is switched on, as the
// composer's cell picture says (CellPosition.Curtain).
function hasCurtain(node) {
    return !!((S.composer.cell.positions || []).find(p => p.core_node_name === node) || {}).curtain;
}

function advValue(key, d) {
    if (key === 'evac_nodes') return d.evacNodes;
    if (key === 'partial_dest') return d.partialDest;
    return d.a[key];
}

// A field "carries a value" by the rule the server counts by (the model's
// advancedSet, domain.ClaimHas): the carry-over's replace and the refill's
// Robot 1 are the defaults, not an opinion. BOARD ORDER IS NEVER COUNTED:
// the store stamps one on every claim it inserts (max + 1), so a value says
// nothing about whether an engineer set it (F15: "1 set" on every row).
function advIsSet(key, d) {
    if (key === 'reorder') return !!d.a.auto_reorder || !!d.a.reorder_point;
    const v = advValue(key, d);
    if (key === 'changeover_carryover_disposition') return !!v && v !== 'replace';
    if (key === 'refill_by') return !!v && v !== 'clearing_robot';
    if (key === 'sequence') return false;
    return Array.isArray(v) ? v.length > 0 : Boolean(v);
}

// isListed is whether a setting has a row at a place: set, or added this
// visit and not given its value yet (S.setAdds). A row a control was used on
// stays too, so picking Robot 1 does not take the row away under the pointer;
// × is what removes a row.
function isListed(key, p, d) {
    return advIsSet(key, d) || !!(S.setAdds || {})[key + '|' + placeKey(p)];
}

function keepRow(key, p) { (S.setAdds = S.setAdds || {})[key + '|' + placeKey(p)] = true; }

// setCount is the row's "N set" chip: the settings its position sets for this
// style, by the list's own rule.
function setCount(node) {
    const d = setCtx({ node: node });
    return setKeys('style').filter(key => advShows(key, d) && advIsSet(key, d)).length;
}

// drawnRows is the positions the table draws a row for, in its order.
function drawnRows() {
    return S.model.positions.map(p => p.core_node_name)
        .filter(n => S.model.cells[n] && S.model.cells[n].on && S.model.cells[n].mode);
}

// setDefault is what × puts back: the value that is no setting.
function setDefault(key) {
    if (key === 'allowed_payload_codes' || key === 'evac_nodes') return [];
    if (key === 'refill_by') return 'clearing_robot';
    if (key === 'changeover_carryover_disposition') return 'replace';
    if (key === 'auto_request_payload' || key === 'keep_staged_node' || key === 'partial_dest') return '';
    if (SET_SWITCHES.indexOf(key) >= 0) return false;
    return 0;
}

// setWrite puts one value at a place as a draft edit: a position's through the
// model, redrawn and previewed; a group's in its `advanced`, redrawn. quiet
// writes without the redraw, for a number being typed (a redraw would take the
// field from under the caret); the bar still measures the draft.
function setWrite(p, key, value, quiet) {
    if (p.g !== undefined) {
        const g = S.shapeDraft && S.shapeDraft.groups && S.shapeDraft.groups[p.g];
        if (!g) return;
        const d = setCtx(p);
        d.a[key] = value;
        const a = recipeAdvanced(d.a, g.of);
        if (Object.keys(a).length) g.advanced = a; else delete g.advanced;
        if (quiet) { S.shapeDraft.refusal = ''; return; }
        groupChanged();
        return;
    }
    const node = p.node;
    let actions;
    if (key === 'evac_nodes') {
        actions = [{ type: 'setEvacNodes', node: node, nodes: value }];
        // NO MARK, NO CARRY-OVER: the server refuses a carry-over on a claim
        // that marks nothing, so the last mark taken off puts it back.
        const a = M().advancedFor(S.model, node);
        if (!value.length && advIsSet('changeover_carryover_disposition', { a: a })) {
            a.changeover_carryover_disposition = 'replace';
            actions.push(advancedAction(node, a));
        }
    } else if (key === 'partial_dest') {
        actions = [{ type: 'setPartialDest', node: node, dest: value }];
    } else {
        const a = M().advancedFor(S.model, node);
        a[key] = value;
        actions = [advancedAction(node, a)];
    }
    if (!quiet) { apply(actions); return; }
    for (const x of actions) S.model = M().reduce(S.model, x);
    drawBar();
    schedulePreview();
}

// advancedAction is the model's setAdvanced for a position's block. BACK TO
// WHAT WAS SAVED IS UNTOUCHED: a block that says what the saved style says is
// sent as null, which the model reads as the saved one, so a setting added
// and taken off again leaves the draft clean, as the sheet's Cancel did. A
// written block, even one of defaults, is a change the next save sends (P0
// :4169).
function advancedAction(node, a) {
    const saved = M().advancedFor(M().reduce(S.model, { type: 'setAdvanced', node: node, advanced: null }), node);
    return { type: 'setAdvanced', node: node, advanced: JSON.stringify(a) === JSON.stringify(saved) ? null : a };
}

function setRedraw(p) {
    if (p.g !== undefined) groupChanged(); else drawFlows();
}

// setAddAt adds a setting at a place. THE FIRST VALUE IT PUTS: a switch is on
// (its row is the setting), the next carrier is Robot 2 (Robot 1 is the
// default and has no row), a Tool change marks the position itself. The rest
// wait in the list, empty, for their value.
function setAddAt(key, p) {
    keepRow(key, p);
    if (SET_SWITCHES.indexOf(key) >= 0) { setWrite(p, key, true); return; }
    if (key === 'reorder') { setWrite(p, 'auto_reorder', true); return; }
    if (key === 'refill_by') { setWrite(p, key, 'index_robot'); return; }
    if (key === 'evac_nodes' && p.node) { setWrite(p, key, [p.node]); return; }
    setRedraw(p);
}

// setRemove is ×: the setting goes back to its default and its row goes.
function setRemove(key, p) {
    delete (S.setAdds || {})[key + '|' + placeKey(p)];
    if (!advIsSet(key, setCtx(p))) { setRedraw(p); return; }
    // AUTO REORDER OFF IS THE POINT CLEARED TOO, as the row's Clear the point
    // does: 0 is the opt-out, and a point left behind would read as set.
    if (key === 'reorder') {
        setWrite(p, 'reorder_point', 0, true);
        setWrite(p, 'auto_reorder', false);
        return;
    }
    setWrite(p, key, setDefault(key));
}

// setData is a control's data attributes: the setting, the place, and extra.
function setData(key, p, extra) {
    const m = Object.assign({ key: key }, p.g !== undefined ? { g: p.g } : { node: p.node }, extra || {});
    return Object.keys(m).map(k => ' data-' + k + '="' + esc(String(m[k])) + '"').join('');
}

// setPlace is the place a control names.
function setPlace(el) {
    return el.dataset.g !== undefined ? { g: Number(el.dataset.g) } : { node: el.dataset.node };
}

// setNumber is a number field for key; row is the setting whose row it sits
// in, kept listed while it is typed into (strict pack's tolerance, the
// reorder row's point).
function setNumber(key, p, d, row) {
    const v = advValue(key, d);
    return '<input class="pd-inp pd-setnum" type="text" inputmode="numeric" data-setnum="' + key + '"' + setData(row || key, p) +
        ' value="' + esc(v ? String(v) : '') + '" placeholder="—" aria-label="' + esc(W(SET_COPY[key] ? SET_COPY[key][0] : key)) + '">';
}

function setChip(key, p, v, label) {
    return '<button class="pd-chip set" data-act="set-unchip"' + setData(key, p, { v: v }) +
        ' title="Take ' + esc(label) + ' off">' + esc(label) + ' <i>&times;</i></button>';
}

function setPickBtn(key, p, label, cls) {
    return '<button class="pd-sel' + (cls ? ' ' + cls : '') + '" data-act="set-pick"' + setData(key, p) + '>' +
        '<span class="t">' + label + '</span><i class="car"></i></button>';
}

// setSeg is a choice of two to four answers, drawn whole (style guide 33,
// rule 6): the answer on is marked, each one is a press.
function setSeg(key, p, on, answers, off) {
    return '<div class="pd-seg">' + answers.map(([v, label]) => {
        const why = off && off(v);
        return '<button class="' + (on === v ? 'on' : '') + '" data-act="set-seg"' + setData(key, p, { v: v }) +
            (why && on !== v ? ' disabled title="' + esc(why) + '"' : '') + '>' + esc(label) + '</button>';
    }).join('') + '</div>';
}

// setCells is one row's controls for a setting at a place.
function setCells(key, p, d) {
    switch (key) {
        case 'allowed_payload_codes':
            return [(d.a.allowed_payload_codes || []).map(c => setChip(key, p, c, M().shortPart(c))).join('') +
                '<button class="pd-chip add" data-act="set-pick"' + setData(key, p) + '>+ add</button>'];
        case 'auto_request_payload': {
            const v = d.a.auto_request_payload;
            return [setPickBtn(key, p, esc(v ? M().shortPart(v) : 'pick one'), v ? '' : 'bad')];
        }
        case 'keep_staged_node':
        case 'partial_dest': {
            const v = advValue(key, d);
            return [setPickBtn(key, p, esc(v || 'pick one'), v ? '' : 'bad')];
        }
        case 'reorder':
            return [(d.a.auto_reorder ? 'on at ' : 'off · point ') + setNumber('reorder_point', p, d, 'reorder')];
        case 'lineside_soft_threshold':
            return [setNumber(key, p, d)];
        case 'strict_pack':
            return ['on', '<span class="k">' + esc(W('pack_tolerance_pct')) + '</span> ' + setNumber('pack_tolerance_pct', p, d, 'strict_pack') + ' %'];
        case 'refill_by': {
            const v = d.a.refill_by || 'clearing_robot';
            // WHAT EACH ANSWER DOES IS THE POSITION'S WORK LIST, from the
            // builders (workListHTML). A recipe group has no part and so no
            // steps to build: its note is the one hand-written answer kept.
            if (p.g !== undefined) {
                return [setSeg(key, p, v, Object.keys(REFILL_ROBOT).map(k => [k, REFILL_ROBOT[k]])),
                    '<span class="pd-dim">' + esc(REFILL_NOTE[v] || REFILL_NOTE.clearing_robot) + '</span>'];
            }
            return [setSeg(key, p, v, Object.keys(REFILL_ROBOT).map(k => [k, REFILL_ROBOT[k]])),
                '<button class="pd-dimlink" data-act="work-goto" data-node="' + esc(p.node) + '">see what its robots do</button>'];
        }
        case 'evac_nodes': {
            const own = toolChangeOwn(p.node);
            const marks = d.evacNodes.map(n => setChip(key, p, n, n)).join('') +
                (own.some(n => d.evacNodes.indexOf(n) < 0)
                    ? '<button class="pd-chip add" data-act="set-pick"' + setData(key, p) + '>+ add</button>' : '');
            if (!d.evacNodes.length) return [marks];
            const park = (S.model.cells[p.node] || {}).parkOld;
            return [marks, '<span class="k">if the part stays</span> ' +
                setSeg('changeover_carryover_disposition', p, d.a.changeover_carryover_disposition || 'replace',
                    Object.keys(CARRYOVER).map(k => [k, CARRYOVER[k]]),
                    v => (v === 'outbound_staging' && !park ? 'this position has no outbound staging' : ''))];
        }
        case 'curtain_required':
            // Said where it waits for nothing yet: no curtain is switched on.
            if (p.g !== undefined) return ['on', '<span class="pd-dim">waits only where a spot’s curtain is switched on</span>'];
            return hasCurtain(p.node) ? ['on'] : ['on', '<span class="pd-dim">no curtain is switched on here: nothing waits until one is</span>'];
        default:
            return ['on'];
    }
}

// toolChangeOwn is the positions a Tool change may mark: the cell's own (the
// position, its partners, its staging), never the press's others —
// changeover_evac_nodes marks which of THIS cell's nodes are in the way of a
// setup, and the save refuses one it does not occupy.
function toolChangeOwn(node) {
    const c = S.model.cells[node] || {};
    return [...new Set([node, c.paired, c.secondPaired, c.staging, c.parkOld].filter(Boolean))];
}

// setList is one setting's exception list: a row per place it is listed at.
// A group's has the one row, named for the group.
function setList(key, places, eligible, group) {
    const copy = SET_COPY[key];
    return exceptionList({
        id: 'set-' + key, label: W(copy[0]), sub: copy[1],
        summary: group ? '' : countOf(places.length, eligible, 'position'),
        rows: places.map(p => ({
            name: group ? group.name : p.node,
            cells: setCells(key, p, setCtx(p)),
            remove: Object.assign({ act: 'set-rm', key: key }, p.g !== undefined ? { g: p.g } : { node: p.node }),
        })),
        add: !group && places.length < eligible ? { label: '+ Add a position', act: 'set-addpos', key: key } : null,
    }) + (key === 'evac_nodes' && places.some(p => setCtx(p).evacNodes.length)
        ? '<p class="pd-note pd-setnote">' + esc(CARRYOVER_NOTE) + '</p>' : '');
}

// styleSettings is under the positions table: the Advanced settings button,
// which offers every setting (openSetAdd), and "Set for this style", a list per
// setting some position of this style sets.
//
// THE WAY IN IS A BUTTON, IN THE ACCENT, IN ONE PLACE. As "+ Add a setting"
// it was a quiet grey line under whatever lists were drawn, so with nothing
// set an engineer had no sign the settings existed. Outlined rather than
// filled: Save style is the tab's one filled button. Not amber: amber on
// these pages is state (needed, stale), and this is not a state.
function styleSettings() {
    const rows = drawnRows();
    if (!rows.length) return '';
    const lists = setKeys('style').map(key => {
        const eligible = rows.filter(n => advShows(key, setCtx({ node: n })));
        const listed = eligible.filter(n => isListed(key, { node: n }, setCtx({ node: n })));
        return listed.length ? setList(key, listed.map(n => ({ node: n })), eligible.length) : '';
    }).join('');
    return '<div class="pd-setfor" id="pd-setfor">' +
        '<button class="pd-btn accent" data-act="set-add">Advanced settings</button>' +
        (lists ? '<div class="pd-lbl">Set for this style<span class="hint">only where a position differs from the default</span></div>' + lists : '') +
        '</div>';
}

// groupSettings is a recipe group's own list, on its card: the settings every
// style the floor makes from the recipe takes on the group's spots.
function groupSettings(g, i) {
    const p = { g: i };
    const d = setCtx(p);
    const lists = setKeys('group').filter(key => advShows(key, d) && isListed(key, p, d))
        .map(key => setList(key, [p], 1, g)).join('');
    return '<div class="pd-gset">' +
        (lists || '<span class="pd-dim">nothing set: a style made from it takes the minted defaults</span>') +
        '<button class="pd-btn quiet exc-add" data-act="set-add" data-g="' + i + '">+ Add a setting</button></div>';
}

// openSetAdd is + Add a setting: the shared picker over the settings, under
// their headings. A setting no place here may take, or one every place that
// may take it already sets, is greyed with why. A style's pick then asks for
// the position, unless only one may take it; a group's is the group.
function openSetAdd(btn, group) {
    const pop = $('pd-pop');
    if (!pop) return;
    const rows = [];
    const places = group ? [{ g: group.g }] : drawnRows().map(n => ({ node: n }));
    const free = {};
    for (const [section, keys] of SET_SECTIONS) {
        for (const key of keys) {
            if (setKeys(group ? 'group' : 'style').indexOf(key) < 0) continue;
            const may = places.filter(p => advShows(key, setCtx(p)));
            if (group && !may.length) continue;
            free[key] = may.filter(p => !isListed(key, p, setCtx(p)));
            const r = { value: key, label: W(SET_COPY[key][0]), section: section };
            if (!may.length) r.reason = 'no position of this style takes it';
            else if (!free[key].length) r.reason = group ? 'set' : 'set on every position that takes it';
            rows.push(r);
        }
    }
    openRowPicker(pop, btn, rows, key => {
        if (group || free[key].length === 1) { setAddAt(key, free[key][0]); return; }
        openSetPosition(btn, key);
    }, { placeholder: 'find a setting' });
}

// openSetPosition is the second step, and a list's own + Add a position: the
// style's positions that may take the setting, one already listed greyed.
function openSetPosition(btn, key) {
    const pop = $('pd-pop');
    if (!pop) return;
    const rows = drawnRows().filter(n => advShows(key, setCtx({ node: n }))).map(n => {
        const r = { value: n, label: n };
        if (isListed(key, { node: n }, setCtx({ node: n }))) r.reason = 'set';
        return r;
    });
    openRowPicker(pop, btn, rows, n => setAddAt(key, { node: n }), { placeholder: 'find a position' });
}

// openSetPick is a row's own picker: parts, a staging node, a destination, the
// positions a Tool change marks.
function openSetPick(btn, key, p) {
    const pop = $('pd-pop');
    if (!pop) return;
    const d = setCtx(p);
    const routing = S.composer.routing || [];
    const node = r => ({ value: r.core_node_name, label: r.label || r.core_node_name });
    keepRow(key, p);
    switch (key) {
        case 'allowed_payload_codes': {
            const have = d.a.allowed_payload_codes || [];
            const rows = S.model.parts.filter(c => have.indexOf(c) < 0).map(c => ({ value: c, label: M().shortPart(c), search: c }));
            openRowPicker(pop, btn, rows, list => setWrite(p, key, have.concat(list)), { multi: true, placeholder: 'find parts' });
            return;
        }
        case 'auto_request_payload':
            openRowPicker(pop, btn, S.model.parts.map(c => ({ value: c, label: M().shortPart(c), search: c })),
                c => setWrite(p, key, c), { placeholder: 'find a part' });
            return;
        // The staging nodes of the routing set: a spot is a node a robot sets
        // one bin down on and lifts it from, which is what a staging node is.
        // It may be this position's own inbound staging (the robot waits under
        // the spare); the save refuses one another position already names.
        case 'keep_staged_node':
            openRowPicker(pop, btn, routing.filter(r => r.role === 'staging').map(node),
                n => setWrite(p, key, n), { placeholder: 'find a staging node' });
            return;
        case 'partial_dest':
            openRowPicker(pop, btn, routing.filter(r => r.role === 'destination').map(node),
                n => setWrite(p, key, n), { placeholder: 'find a destination' });
            return;
        case 'evac_nodes':
            openRowPicker(pop, btn, toolChangeOwn(p.node).filter(n => d.evacNodes.indexOf(n) < 0).map(n => ({ value: n, label: n })),
                list => setWrite(p, key, d.evacNodes.concat(list)), { multi: true, placeholder: 'find positions' });
            return;
        default:
    }
}

// onSetNumber writes a number as it is typed, without a redraw.
function onSetNumber(e) {
    const el = e.target;
    if (!el || !el.dataset || el.dataset.setnum === undefined) return;
    const digits = String(el.value || '').replace(/[^0-9]/g, '');
    const p = setPlace(el);
    keepRow(el.dataset.key, p);
    setWrite(p, el.dataset.setnum, digits ? Number(digits) : 0, true);
}

// ── robot work lists ─────────────────────────────────────────────────────────
//
// WHAT EACH ROBOT IS SENT, FROM THE ONE BUILDER. The preview's steady state
// (flow/preview?steps=1, engine.SteadyCell) is the orders a position makes
// when it asks for a swap, as the Edge's request-time builders make them: the
// pair from BuildSwapDispatch, a sequential line's Order B from
// BuildSequentialBackfillSteps, Robot 3's carrier from carrierSpec. Nothing
// here decides a step: it draws the server's, one robot per order, in the
// order the server lists them (A, B, then the follow-ons), as Robot 1, 2, 3.
//
// THE SWAP AS CONFIGURED. What a REQUEST can change is not drawn: an empty
// line (the clear and prime), an index press's back positions being primed, a
// keep-staged spot with no spare (it fetches from the market), and a partial
// held for the line (its bin named on the steps).

// WORK_STEP is a step's glyph and verb, by the action Core receives.
const WORK_STEP = { pickup: ['↑', 'Pick up'], dropoff: ['↓', 'Drop'], wait: ['‖', 'Wait'] };

// keepSteady keeps a preview's steady state for the style it was asked for,
// and, when it carries none (a refusal), the preview's own words for why.
function keepSteady(styleId, body) {
    S.steady = { styleId: styleId, cells: (body && Array.isArray(body.steady)) ? body.steady : null, error: (body && body.error) || '' };
}

// steadyOf is node's entry in the last preview's steady state: its own cell's,
// or, for an index press's back position, the press's (the entry whose legs
// work it).
function steadyOf(node) {
    const st = S.steady;
    if (!node || !st || !st.cells || !S.model || st.styleId !== S.model.styleId) return null;
    const touches = e => (e.legs || []).some(l => (l.steps || []).some(s => s.node === node) ||
        (l.retrieve && l.retrieve.delivery === node));
    return st.cells.find(e => e.core_node_name === node) || st.cells.find(touches) || null;
}

// workRowNode is the table row the selected position's work list goes under:
// the selection's own row, or its press's.
function workRowNode() {
    if (!S.selected || !S.model) return '';
    if (S.model.cells[S.selected] && S.model.cells[S.selected].on) return S.selected;
    const e = steadyOf(S.selected);
    return e ? e.core_node_name : '';
}

// drawWorkRow redraws the open work list once a preview answers.
function drawWorkRow() {
    const td = $('pd-workrow');
    if (td) td.innerHTML = workListHTML(S.selected, 'Checking…');
}

// workRole is the role dot a step's node wears: a position, or the routing
// box it is named in; none for a node the process does not name.
function workRole(node) {
    if (isPositionRow(node)) return 'positions';
    const r = (S.composer.routing || []).find(x => x.core_node_name === node);
    return r ? ROLE_OF[r.role] || '' : '';
}

// workWords is a robot's two or three words, read off its own steps: a bin
// lifted at a position and set down off it is the old one out, one set down
// at a position from off it the new one in, and position to position an
// index.
function workWords(steps) {
    let from = null, out = false, inn = false, idx = false;
    for (const s of steps) {
        if (s.action === 'pickup') { from = s.node; continue; }
        if (s.action !== 'dropoff' || from === null) continue;
        const a = isPositionRow(from), b = isPositionRow(s.node);
        if (a && b) idx = true; else if (a) out = true; else if (b) inn = true;
        from = null;
    }
    if (idx) return inn ? 'indexes, refills' : 'indexes';
    if (out && inn) return 'out, then in';
    return out ? 'old bin out' : inn ? 'new bin in' : '';
}

// workListHTML is position node's robot work lists: per robot its name, its
// dot and its words, then its steps as glyph, verb and node. wait is what it
// says when the answered preview has no entry for node (it is not in the
// draft); before the style's first answer it says Checking…, and after an
// answer with no steady state, the preview's refusal.
function workListHTML(node, wait) {
    const e = steadyOf(node);
    const label = e ? (M().modeLabels()[e.swap_mode] || e.swap_mode) : '';
    // Named for the cell whose list it is: a back position's is its press's.
    const head = '<div class="pd-wlhd"><h2>Robot work lists · ' + esc(e ? e.core_node_name : node) + '</h2>' +
        (label ? '<small>' + esc(label) + ' · the steps sent to Core</small>' : '') + '</div>';
    if (!e) {
        const st = S.steady && S.model && S.steady.styleId === S.model.styleId ? S.steady : null;
        const why = !st ? 'Checking…' : !st.cells ? (st.error || 'The preview sent no work list.') : wait;
        return '<div class="pd-wl">' + head + '<p class="pd-dim">' + esc(why) + '</p></div>';
    }
    // A SWAP NOT COMPLETE SENDS NO ROBOT. The builder's refusal names the
    // mode and the field in code (two_robot_press_index, paired_core_node), so
    // it is not shown, hover included: what the position still needs is said
    // from its own findings, in the words the bar uses ("Index Swap requires a
    // paired core node").
    if (e.error) {
        const needs = positionFixes(e.core_node_name);
        return '<div class="pd-wl">' + head + '<p class="pd-dim">No robot is sent until this swap is complete' +
            (needs.length ? ': ' + esc(needs[0]) : '.') + '</p></div>';
    }
    if (!(e.legs || []).length) return '<div class="pd-wl">' + head + '<p class="pd-dim">This swap sends no robot.</p></div>';
    const step = s => {
        const g = WORK_STEP[s.action] || ['·', s.action];
        const role = s.node ? workRole(s.node) : '';
        return '<span class="pd-wlg">' + g[0] + '</span><span class="pd-wlv">' + esc(g[1] + (s.action === 'pickup' && s.empty ? ' empty' : '')) +
            '</span><span class="pd-wln">' + (s.node ? '<span class="setpick-chip">' +
                (role ? '<i class="setpick-dot" data-role="' + role + '"></i>' : '') + esc(s.node) + '</span>' : '<span class="pd-wlerr">no node</span>') + '</span>';
    };
    const robots = e.legs.map((leg, i) => {
        // A plain retrieve is its spec, from its source to its delivery.
        const steps = leg.kind === 'retrieve' && leg.retrieve
            ? [{ action: 'pickup', node: leg.retrieve.source, empty: leg.retrieve.empty }, { action: 'dropoff', node: leg.retrieve.delivery }]
            : (leg.steps || []);
        const n = i + 1;
        return '<div class="pd-wlrobot r' + n + '"><div class="pd-wlname"><i class="pd-wldot"></i><b>Robot ' + n + '</b>' +
            '<small>' + esc(workWords(steps)) + '</small></div><div class="pd-wlsteps">' + steps.map(step).join('') + '</div></div>';
    });
    const owed = e.follow_ons - e.legs.filter(l => l.leg === 'follow_on').length;
    return '<div class="pd-wl">' + head + robots.join('') +
        (owed > 0 ? '<p class="pd-dim">+' + nOf(owed, 'follow-on order') + '</p>' : '') + '</div>';
}

// REFILL_ROBOT is the refill setting's stored words (protocol.RefillBy) as the
// screens say them; blank or unknown reads as Robot 1, the default.
const REFILL_ROBOT = { clearing_robot: 'Robot 1', index_robot: 'Robot 2', own_robot: 'Robot 3' };
const REFILL_NOTE = {
    clearing_robot: 'Robot 2 indexes',
    index_robot: 'Robot 1 evacuates and leaves',
    own_robot: 'Robot 1 evacuates, Robot 2 indexes, and Robot 3 brings the back its carrier when Robot 2 lifts it',
};

function onScrimClick(e) {
    // A node picker in a sheet answers its own clicks (shared/picker.js).
    // A POPOVER OPENED FROM A SHEET CLOSES ON THE NEXT CLICK OUTSIDE IT, which
    // is what #pd-pop's own document listener does for the page. The option
    // rows are inside .pd-pop and so are not "outside" — they run their own
    // handler and close it themselves.
    const spop = $('pd-advpop');
    if (spop && !spop.hidden && !(e.target.closest && e.target.closest('.pd-pop'))) {
        closeLayer(spop);
        spop.hidden = true;
        spop.innerHTML = '';
    }
    const act = e.target.closest && e.target.closest('[data-act]');
    // NEW STYLE'S SPOTS STEP draws into this scrim: a card or band lane of its
    // picture is a tap (a lane first — a used lane's slot carries the spot it
    // serves as its data-pos), and a move's chip puts it back.
    if (S.newStyle) {
        const lane = e.target.closest && e.target.closest('#pd-nsvg [data-staging]');
        const spot = !lane && e.target.closest && e.target.closest('#pd-nsvg [data-pos]');
        if (lane || spot) { e.stopPropagation(); tapNewSpot(lane ? lane.dataset.staging : spot.dataset.pos); return; }
        if (act && act.dataset.act === 'nmove-rm') { e.stopPropagation(); undoNewMove(act.dataset.node); return; }
    }
    if (act && act.dataset.act === 'newstyle-parts') { e.stopPropagation(); closeSheet(); void openSettingsAt('Parts'); return; }
    if (act && act.dataset.act === 'newstyle-screens') {
        e.stopPropagation();
        closeSheet();
        void openSettingsAt('Positions and routing');
        return;
    }
    if (act && act.dataset.act === 'newstyle-recipe') { e.stopPropagation(); openNewRecipePick(act); return; }
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
    // The apply modal draws its own footer on #pd-scrim, so its two buttons
    // answer here; every other sheet's are the layer's own.
    if (act && act.dataset.act === 'sheet-cancel') { closeSheet(); return; }
    if (act && act.dataset.act === 'sheet-ok') { runSheet(); return; }
}

// ── a screen's health and the loader windows (Settings' Screens and positions) ──
//
// Whether a screen that works this process is answering, and the loaders Core
// knows about that no screen on this edge works.
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

// The binding table appears when there is something to bind. The Edge learns
// about a loader from Core's aggregate; one with no operator screen anywhere
// here has nobody to work it, and that is the only case worth a row.
function loaderKeyOf(g) { return g.loader_key || g.core_node_name || g.name || String(g); }

// LOADER WINDOWS ONLY ON A LOADER'S PROCESS (F17): the block offered "Make a
// screen on <this process>" for every unboarded loader on every process, a
// press included. loaderGapsHere is the gaps this process may take, or null
// where the block has no business: a process is a loader's when a loader's
// windows are its positions (the gap names it: LoaderBoardGaps' process_id), a
// style of it claims a loader window (manual_swap, the stored loader claim),
// or it has no position yet (an empty process is what a loader's board is made
// on). A gap whose windows another process holds is that process's to bind.
// All of it is data the tab already holds; nothing is read for it.
function loaderGapsHere() {
    const pid = S.processID;
    const gaps = S.loaderGaps || [];
    const owns = gaps.some(g => g.process_id === pid);
    const claims = ((S.composer && S.composer.styles) || []).some(st =>
        (st.claims || []).some(c => c.swap_mode === 'manual_swap'));
    // Positions on no screen are positions: such a process is not empty.
    const empty = !stationsOf(pid).some(st => (S.stationNodes[String(st.id)] || []).length) &&
        !loosePositions().length;
    if (!(owns || claims || empty)) return null;
    return gaps.filter(g => !g.process_id || g.process_id === pid);
}

function loaderBindingSentence() {
    const gaps = loaderGapsHere() || [];
    if (!gaps.length) return 'Every loader Core knows about has a screen. Nothing to bind.';
    const names = gaps.map(loaderKeyOf);
    return names.length + ' loader' + (names.length === 1 ? '' : 's') + ' Core knows about have no screen ' +
        'on this edge: ' + names.join(', ') + '.';
}

// AND THE THING TO DO ABOUT IT. This section named the gaps and offered
// nothing — POST /api/loader-boards exists, makes the screen and binds the
// loader's windows to it in one action, and its own handler comment says "the
// screen that offers the button is where it gets made". There was no button,
// and no caller anywhere in www/static. So D4 told an engineer that three
// loaders had nobody to work them and left them there.
//
// WHICH PROCESS OWNS IT IS THE HUMAN DECISION the handler refuses to infer —
// Core sends every loader to every edge — so the button is on the process the
// engineer is looking at, and says so.
function loaderGapRows() {
    const gaps = loaderGapsHere() || [];
    if (!gaps.length) return '';
    const p = process();
    return gaps.map(g => {
        const key = loaderKeyOf(g);
        return '<div class="pd-rrow"><span class="sq grp"></span><span class="nm">' + esc(key) +
            '<small>no screen on this edge works it</small></span><span class="sp"></span>' +
            '<button class="pd-btn" data-act="bind-loader" data-loader="' + esc(key) + '">' +
            'Make a screen on ' + esc(p ? p.name : 'this process') + '</button></div>';
    }).join('');
}

// The screen and the window bindings in one write, which is what the endpoint
// does. A refusal names the window Core does not have.
//
// IT READS BACK WHAT IT MOVED, NOT SETTINGS' LISTS (owner, 2026-11-14: five
// requests at most): the screens, the new screen's positions, the process's
// positions (its windows are positions now) and the process rows (whether Core
// runs its boards). A re-read of the whole process was twelve.
async function bindLoaderBoard(key) {
    const out = await send('POST', '/api/loader-boards',
        { loader_key: key, process_id: S.processID }, NO_NOTICE);
    if (!out.ok) {
        S.settingsError = said(out, 'Could not make a screen for ' + key + ': ');
        S.settingsNotice = '';
    } else {
        S.settingsNotice = key + ' now has a screen on this process, with its windows bound to it.';
        S.settingsError = '';
        // The gap list arrives with the page and there is no endpoint to
        // re-read it from. A loader that now has a screen is no longer a gap,
        // which the page can say for itself without inventing a route.
        S.loaderGaps = (S.loaderGaps || []).filter(g => loaderKeyOf(g) !== key);
    }
    const id = out.ok ? Number(bodyOf(out).station_id) || 0 : 0;
    // The curtain and Counted at drafts are rebuilt by the positions' read;
    // what they held for a position still there is put back over it.
    const drafts = { curtain: S.curtain, counted: S.counted };
    // The new screen's positions, read: a row whose list was never read is
    // not offered for editing (screenRow).
    await Promise.all([reloadStations(), loadCurtainNodes(), reloadProcesses(), id ? refreshStationNodes(id) : null]);
    keepDraft(S.curtain, drafts.curtain);
    keepDraft(S.counted, drafts.counted);
    if (S.tab !== 'settings') return;
    settingsPickers();
    drawSettings();
}

// ── Settings · Screens and positions ─────────────────────────────────────────
//
// THE OPERATOR SCREENS TAB, FOLDED IN (owner ruling 2026-11-14). Each screen
// that works this process is one row: its name, how many positions it claims
// and when it last answered, and its positions as the shared picker's chips.
// A position is one screen's: picking one that is on a sibling screen moves it
// there. + Add a screen and a screen's Edit open the screen sheet (its name and
// note); Open is the screen itself. A draft, saved by the one Save
// (SETTINGS_SECTIONS.positions).
//
// POSITIONS COME BEFORE THE SCREEN (owner's setup order, 2026-11-14): a
// process's positions are its own, and the ones no screen has taken yet are a
// row of their own, "On no screen" (looseRow), drawn while the process has no
// screen or has positions on none. A screen picked later takes them onto it
// (SetNodes adopts a position on no screen).
function screensSection() {
    const screens = stationsOf(S.processID);
    const total = screens.reduce((n, st) => n + pickerValue(screenKey(st.id)).length, 0);
    return '<div class="pd-sect"><h2>Screens and positions</h2>' +
        (screens.length ? '<span class="pd-dim">' + esc(nOf(total, 'position') + ' on ' + nOf(screens.length, 'screen')) + '</span>' : '') +
        '<span class="pd-spacer"></span><button class="pd-btn" data-act="screen-add">+ Add a screen</button></div>' +
        screens.map(screenRow).join('') + looseRow() +
        (screens.length ? '' : '<p class="pd-note">No screen works this process yet: an operator reaches it once one takes its positions.</p>') +
        loaderWindows();
}

// LOOSE is the picker key of the positions on no screen.
const LOOSE = 'scr-none';

// loosePositions is the process's positions on no screen, as last read, in
// their order.
function loosePositions() {
    return (S.curtainNodes || []).filter(n => !n.operator_station_id).map(n => n.core_node_name);
}

// looseShown: the row is drawn while there is no screen or a position on none.
function looseShown() {
    return !stationsOf(S.processID).length || loosePositions().length > 0;
}

function looseRow() {
    if (!looseShown()) return '';
    return '<div class="pd-sfld pd-scrrow"><label>On no screen' +
        '<small>' + esc(nOf(pickerValue(LOOSE).length, 'position') + ' · a screen has not taken them yet') + '</small></label>' +
        '<div class="v col">' + pickerBox(LOOSE) + '</div></div>';
}

// nOf is "1 position", "23 positions".
function nOf(n, one) { return n + ' ' + one + (n === 1 ? '' : 's'); }

function screenKey(id) { return 'scr-' + id; }

// screenRow is one screen. A screen whose positions could not be read is not
// offered for editing: its list is a set-to, and one saved over a read that
// failed would take every position off it.
//
// A CORE LOADER'S WINDOWS ARE CORE'S: its screen's positions are drawn as
// chips with no × and no picker under them (the Loader board line says where
// they are set). Taking one off here would leave Core's loader with a window
// no screen works.
function screenRow(st) {
    const h = healthOf(st);
    const unread = !(String(st.id) in S.stationNodes);
    const core = coreBoards(process());
    return '<div class="pd-sfld pd-scrrow"><label>' + esc(st.name) +
        '<small>' + esc((unread ? '' : nOf(pickerValue(screenKey(st.id)).length, 'position') + ' · ') + h.text) + '</small>' +
        '<span class="pd-scracts"><button class="pd-dimlink" data-act="screen-edit" data-station="' + st.id + '">Edit</button>' +
        '<a class="pd-dimlink" href="/operator/station/' + st.id + '">Open</a></span></label>' +
        '<div class="v col">' + (unread
            ? '<p class="pd-note">This screen’s positions could not be read, so they cannot be changed here. Reload the page.</p>'
            : core ? '<div class="picker-chips">' + pickerValue(screenKey(st.id)).map(n =>
                '<span class="picker-chip">' + esc(n) + '</span>').join('') + '</div>'
                : pickerBox(screenKey(st.id))) + '</div></div>';
}

// loaderWindows is the loader windows Core knows about with no screen on this
// edge, for a loader's process only (loaderGapsHere): each with the one button
// that makes its screen and binds its windows.
function loaderWindows() {
    if (!loaderGapsHere()) return '';
    return stBlock('Loader windows', 'Core loaders with no screen on this edge',
        '<p class="pd-dim">' + esc(loaderBindingSentence()) + '</p>' + loaderGapRows());
}

// ── Settings · Positions and routing ─────────────────────────────────────────
//
// THE SAME PICK AS ADD PROCESS, on the page's one Save (owner, 2026-11-15:
// "that pick for the nodes was really good the plants like it"). Under it,
// the screen. A line has one (owner: "lines shouldnt have two screens"), made
// with the process, and a position picked here goes on it. A line with none
// offers "+ Add a screen", which takes the positions on no screen. A line
// that has two from before (two at SPR) shows each with its positions as they
// are; a new position goes on the first, and nothing here moves one between
// them. A loader's process keeps its screens' rows (screensSection).
// A routing name may sit in several roles (a buffer is inbound and outbound),
// never in a role and on a screen (ErrRoutingNodeIsPosition). Inside a role
// the order picked is the order saved, the first a new position's default;
// the roles are one write that adds and takes out together
// (SETTINGS_SECTIONS.routing).
function positionsRoutingSection() {
    const way = waypointNames();
    return '<div class="pd-sect"><h2>Positions and routing</h2><span class="pd-dim">the same pick as Add process</span></div>' +
        '<div class="pd-sphost" id="sp-' + ST_SET + '"></div>' +
        '<div class="pd-scrlines" id="pd-scrlines"></div>' +
        loaderWindows() +
        (routingUnread() ? '' : ROUTING_GROUPS.map(allOffLine).join('')) +
        (way.length ? '<p class="pd-note">Waypoints, from the map, on the shortest supply path (not editable): ' + esc(way.join(' · ')) + '</p>' : '') +
        (S.routingError && !routingUnread() ? '<div class="pd-refusal">' + esc(S.routingError) + '</div>' : '') +
        (S.routingWarning ? '<div class="pd-warn">' + esc(S.routingWarning) + '</div>' : '') +
        '<p class="pd-note">' + esc(ROUTING_NOTE) + '</p>';
}

// ROUTING_NOTE is what a name in a role means, in one line; the rest of main's
// Routing set paragraph is in the Edge UI guide (Settings, Positions and
// routing).
const ROUTING_NOTE = 'Operators are offered these names, never the plant.';

// settingsSetSel is Settings' pick as the process stands: its positions and
// its routing set's enabled names, each role in its order.
function settingsSetSel() {
    const sel = { positions: processPositions() };
    for (const g of ROUTING_GROUPS) sel[g[0]] = routingRowsFor(g[0]).map(r => r.core_node_name);
    return sel;
}

// backPositionsLine is Staging's note: the press's own back positions are
// staging already, and are not routing rows (alwaysAvailableStaging).
function backPositionsLine() {
    const backs = alwaysAvailableStaging();
    if (!backs.length) return '';
    return backs.join(', ') + (backs.length === 1 ? ' is a back position' : ' are back positions') + ' and always available';
}

// settingsSetMoved: a box moved. The screen lines follow the draft, the
// section list's dot follows what is still needed, and the bar is marked.
function settingsSetMoved() {
    drawScreenLines();
    redrawSettingsNav();
    if (S.stShell && S.stShell.pid === S.processID) S.stShell.page.update();
}

// drawScreenLines is the line under the pick: each screen with what it will
// hold once saved, a chip per position, its health, Edit and Open; the
// positions on no screen, with "Put them on <screen>" for each screen; and
// "+ Add a screen" on a line that has none.
function drawScreenLines() {
    const el = $('pd-scrlines');
    if (!el) return;
    const screens = stationsOf(S.processID);
    const names = setPickValue(ST_SET, 'positions');
    const d = screensDraft(names, S.looseOnto);
    const loose = d.none || [];
    const lines = screens.map(st => {
        const unread = !(String(st.id) in S.stationNodes);
        const held = unread ? [] : (d[st.id] || []);
        const n = held.length;
        return '<div class="pd-scrline"><span>Screen <b>' + esc(st.name) + '</b> ' +
            (unread ? '· its positions could not be read, so they cannot be changed here. Reload the page.'
                : (screens.length === 1 && n && !loose.length ? 'holds all ' + nOf(n, 'position') : 'holds ' + nOf(n, 'position'))) +
            ' · ' + esc(healthOf(st).text) + '</span>' +
            '<span class="pd-scracts"><button class="pd-dimlink" data-act="screen-edit" data-station="' + st.id + '">Edit</button>' +
            '<a class="pd-dimlink" href="/operator/station/' + st.id + '">Open</a></span></div>' +
            (n ? '<div class="setpick-chips pd-scrchips">' + held.map(p => '<span class="setpick-chip">' + esc(p) + '</span>').join('') + '</div>' : '');
    });
    // "PUT THEM ON" FOR EACH SCREEN: on a line with two, the positions on no
    // screen are offered to either, as main's checklist on each screen did,
    // and once they are going on one, to the others still.
    const readable = screens.filter(st => String(st.id) in S.stationNodes);
    const onto = readable.find(st => String(st.id) === String(S.looseOnto));
    const putOn = list => '<span class="pd-scracts">' + list.map(st => '<button class="pd-btn quiet" data-act="loose-onto" data-station="' + st.id + '">Put them on ' +
        esc(st.name) + '</button>').join('') + '</span>';
    if (loose.length) {
        lines.push('<div class="pd-scrline"><span>' + esc(nOf(loose.length, 'position') + ' on no screen: ' + loose.join(', ')) + '</span>' +
            putOn(readable) + '</div>');
    }
    if (onto) {
        lines.push('<div class="pd-scrline"><span>The positions on no screen go on ' + esc(onto.name) + ' when this is saved.</span>' +
            putOn(readable.filter(st => st !== onto)) + '</div>');
    }
    if (!screens.length) {
        lines.push('<div class="pd-scrline"><span>No screen works this process yet: an operator reaches it once one holds its positions.</span>' +
            '<button class="pd-btn" data-act="screen-add">+ Add a screen</button></div>');
    }
    el.innerHTML = lines.join('');
}

// ── Settings' pickers: a loader's screens, and the part set ──────────────────
//
// DECLARED ONCE PER VISIT TO A PROCESS, not per draw: a redraw (every pick,
// a refresh after a sheet) keeps what is picked and not yet saved. After a
// save, afterSettingsSave declares them again from what was read back (force),
// except the ones whose section was refused, which keep their drafts.
function settingsPickers(force, keep) {
    const fresh = force || S.stPickersPid !== S.processID;
    S.stPickersPid = S.processID;
    const want = (key, opts) => {
        if (keep && keep[key]) return;
        if (!fresh && S.pickers[key]) return;
        pickerInit(key, opts);
    };
    for (const st of stationsOf(S.processID)) {
        want(screenKey(st.id), {
            selected: (S.stationNodes[String(st.id)] || []).slice(),
            only: raw => !isGroupRow(raw),
            exclude: n => screenRule(n),
            annotate: n => screenNote(n, st.id),
            onChange: () => screensMoved(st.id),
            placeholder: 'add a position',
        });
    }
    // A position on a screen is that screen's: it is greyed here, and taken
    // off its screen first if it is to be on none.
    want(LOOSE, {
        selected: loosePositions(),
        only: raw => !isGroupRow(raw),
        exclude: n => {
            const on = stationsOf(S.processID).find(st => pickerValue(screenKey(st.id)).indexOf(n) >= 0);
            return on ? 'on ' + on.name : screenRule(n);
        },
        onChange: () => screensMoved(0),
        placeholder: 'add a position',
    });
    // THE PART SET FOLDS (slice 5): a big line's 46 parts are 46 chips above
    // the search box; the first PART_FOLD, then "+ N more · show all".
    want(PART_SET_PICKER, {
        source: 'payloads', selected: (S.partSet || []).slice(), fold: PART_FOLD,
        onChange: () => settingsPickersMoved(PART_SET_PICKER),
        placeholder: 'find a part by name',
    });
}

// draftRoles is the roles the routing boxes' draft holds name in.
function draftRoles(name) {
    return ROUTING_GROUPS.filter(g => setPickValue(ST_SET, g[0]).indexOf(name) >= 0).map(g => g[1]);
}

// screenRule is why a screen may not take a node: it is in the routing set
// (take it out there first: one node is a position or a routing name, never
// both), or another process holds it.
function screenRule(name) {
    const roles = draftRoles(name);
    if (roles.length) return 'in ' + roles.join(' and ') + ' — take it out of the routing set to make it a position';
    return positionHeld(name, S.processID);
}

// screenNote is what picking a node on screen stationID does when it is more
// than adding it: it moves from a sibling screen, it puts one of this
// process's positions on no screen onto this one, or it shares a loader window.
function screenNote(name, stationID) {
    const on = stationsOf(S.processID).find(st => st.id !== stationID && pickerValue(screenKey(st.id)).indexOf(name) >= 0);
    if (on) return 'on ' + on.name + ' · picking moves it here';
    const rows = (S.livePositions || []).filter(n => n.core_node_name === name);
    if (rows.some(n => n.process_id === S.processID && !n.operator_station_id)) return 'on no screen yet';
    const other = rows.find(n => n.process_id !== S.processID);
    return other && sharedLoaderNode(name) ? 'loader window · also on ' + other.process_name : '';
}

// screensMoved: a node picked on one screen leaves its sibling's draft and
// the no-screen row's (a position is one screen's, or none's), and the other
// pickers' rules are read again. stationID 0 is the no-screen row, whose picks
// are never on a screen (greyed there), so it moves nothing.
function screensMoved(stationID) {
    const key = stationID ? screenKey(stationID) : LOOSE;
    const mine = pickerValue(key);
    const siblings = stationsOf(S.processID).filter(st => st.id !== stationID)
        .map(st => [screenKey(st.id), (S.stationNodes[String(st.id)] || []).slice()]);
    if (stationID) siblings.push([LOOSE, loosePositions()]);
    for (const [k, was] of siblings) {
        const p = S.pickers[k];
        if (!p || !stationID) continue;
        const left = p.sel.filter(n => mine.indexOf(n) < 0);
        if (left.length !== p.sel.length) {
            p.sel = left;
            if (p.ui) p.ui.set(left.slice(), { was: was });
        }
    }
    settingsPickersMoved(key);
}

// settingsPickersMoved: after a pick, the other pickers re-read their rules
// (a position is greyed in the routing set, and the other way round), and the
// bar is marked. The counts beside the headings follow on the next draw: the
// list stays open while the engineer picks.
function settingsPickersMoved(key) {
    for (const k of Object.keys(S.pickers)) {
        if (k !== key && k.indexOf('scr-') === 0 && S.pickers[k].ui) S.pickers[k].ui.refresh();
    }
    if (S.stShell && S.stShell.pid === S.processID) S.stShell.page.update();
}

// routeNote is where a name the process already has switched off came from:
// the backfill's evidence, or an engineer's earlier list.
function routeNote(role, name) {
    const have = (S.routing || []).find(r => r.core_node_name === name && r.role === role);
    if (!have || have.enabled) return '';
    if (have.origin === 'backfill') {
        const c = have.style_count || 0;
        return 'in your flows · ' + (ROLE_EVIDENCE[role] || role) + ' on ' + c + ' style' + (c === 1 ? '' : 's');
    }
    return 'was in this list, switched off';
}

// sameList is two name lists in the same order.
function sameList(a, b) {
    return a.length === b.length && a.every((n, i) => n === b[i]);
}

// savePositions is the screens' save. claimed-nodes is a set-to per screen.
//
// THE WRITE IS THE SCREEN'S LIST AS THE SERVER HOLDS IT NOW, plus what this
// draft added and minus what it took off: never the list the page held, which
// may be missing a position another screen or another engineer put there
// since (a set-to sent over it would take that position off). Each changed
// screen is read just before it is written; a read that fails stops the save.
//
// A POSITION TAKEN OFF IS ASKED ABOUT FIRST: what still names it comes back
// as the refusal, and Save again with the same lists takes it off. A position
// moving to a sibling screen is not taken off, and is not asked about. The
// screens that gain a position are written first, so a move is the server's
// move (SetNodes), not a retire and a new position.
//
// THE POSITIONS ON NO SCREEN (d.none) are one more write, after the screens:
// a screen takes one off the row by adopting it, and the row is the process's
// own set-to (PUT …/positions), asked about first the same way. It is written
// from the list as last read: only this row changes it.
//
// ONE READ PER CHANGED SCREEN: the read before its write. What a write leaves
// comes back in its answer (`nodes`), so the screen is not read again after.
async function savePositions(d) {
    const stored = id => (S.stationNodes[String(id)] || []);
    const screens = stationsOf(S.processID).filter(st => (String(st.id) in S.stationNodes) && d[st.id] && !sameList(d[st.id], stored(st.id)));
    const everywhere = new Set();
    for (const id of Object.keys(d)) for (const n of d[id]) everywhere.add(n);
    const loose = 'none' in d && !sameList(d.none, loosePositions()) ? B().stationNodes(d.none).nodes : null;
    const want = {};
    for (const st of screens) {
        const now = await readStationNodes(st.id);
        if (!now) return { ok: false, error: st.name + '’s positions could not be read, so nothing was changed. Try again.' };
        const added = d[st.id].filter(n => stored(st.id).indexOf(n) < 0);
        const taken = stored(st.id).filter(n => d[st.id].indexOf(n) < 0);
        want[st.id] = B().stationNodes(now.filter(n => taken.indexOf(n) < 0).concat(added.filter(n => now.indexOf(n) < 0))).nodes;
    }
    const key = JSON.stringify(d);
    // THE IMPACT IS ASKED OF WHAT IS TAKEN OFF, NOT OF WHAT MOVES: a position
    // leaving this list for a sibling screen (or for no screen) is in the list
    // the impact reads, though not in the write, or the server warns that the
    // move clears its curtain and counter, which a move keeps.
    const moving = (from, to) => from.filter(n => to.indexOf(n) < 0 && everywhere.has(n));
    if (S.posWarned !== key) {
        const says = [];
        for (const st of screens) {
            if (!stored(st.id).some(n => d[st.id].indexOf(n) < 0 && !everywhere.has(n))) continue;
            const asked = want[st.id].concat(moving(stored(st.id), d[st.id]).filter(n => want[st.id].indexOf(n) < 0));
            const impact = await send('POST', '/api/operator-stations/' + st.id + '/claimed-nodes/impact', { nodes: asked }, NO_NOTICE);
            if (!impact.ok) return doorOut(impact, st.name + ': ');
            if (bodyOf(impact).warning) says.push(bodyOf(impact).warning);
        }
        if (loose && loosePositions().some(n => loose.indexOf(n) < 0 && !everywhere.has(n))) {
            const asked = loose.concat(moving(loosePositions(), d.none).filter(n => loose.indexOf(n) < 0));
            const impact = await send('POST', '/api/processes/' + S.processID + '/positions/impact', { nodes: asked }, NO_NOTICE);
            if (!impact.ok) return doorOut(impact, 'On no screen: ');
            if (bodyOf(impact).warning) says.push(bodyOf(impact).warning);
        }
        // The server's warning ends with what to do: "Save again to remove it
        // anyway" (domain/position_removal.go).
        if (says.length) {
            S.posWarned = key;
            return { ok: false, error: says.join(' ') };
        }
    }
    S.posWarned = null;
    const gains = st => d[st.id].filter(n => stored(st.id).indexOf(n) < 0).length;
    screens.sort((a, b) => gains(b) - gains(a));
    let landed = false;
    for (const st of screens) {
        const out = await send('PUT', '/api/operator-stations/' + st.id + '/claimed-nodes', { nodes: want[st.id] }, NO_NOTICE);
        if (!out.ok) return Object.assign(doorOut(out, st.name + ': '), { landed: landed });
        landed = true;
        const kept = Array.isArray(bodyOf(out).kept) ? bodyOf(out).kept : [];
        if (kept.length) S.saveNotes.push(keptPositionsSentence(kept));
        S.stationNodes[String(st.id)] = Array.isArray(bodyOf(out).nodes) ? bodyOf(out).nodes : want[st.id].slice();
    }
    if (loose) {
        const out = await send('PUT', '/api/processes/' + S.processID + '/positions', { nodes: loose }, NO_NOTICE);
        if (!out.ok) return Object.assign(doorOut(out, 'On no screen: '), { landed: landed });
        const kept = Array.isArray(bodyOf(out).kept) ? bodyOf(out).kept : [];
        if (kept.length) S.saveNotes.push(keptPositionsSentence(kept));
    }
    return { ok: true };
}

// saveRouting is the routing set's save: every name the draft holds, in the
// order picked (the order is the sequence), and the names it took out, in one
// write that lands whole or not at all. Each removal's warning is said after.
async function saveRouting(d) {
    const rows = [];
    const remove = [];
    for (const g of ROUTING_GROUPS) {
        for (const n of d[g[0]] || []) rows.push({ core_node_name: n, role: g[0] });
        for (const r of routingRowsFor(g[0])) {
            if ((d[g[0]] || []).indexOf(r.core_node_name) < 0) remove.push({ core_node_name: r.core_node_name, role: g[0] });
        }
    }
    const out = await send('PUT', '/api/processes/' + S.processID + '/routing-nodes', B().routingSet(rows, remove), NO_NOTICE);
    if (out.ok) for (const w of (bodyOf(out).warnings || [])) S.saveNotes.push(w);
    return doorOut(out);
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

// THE WORD IS CELL (owner, 2026-09-17: "it could be a weld cell or some other
// process"). The auto-arm setting is on every process, and a 4x2 weld cell is
// not stamping anything — it is running the new part, which is the fact this
// sentence is actually about.
//
// ONE LINE (owner, 2026-11-14): a note is 68ch wide (style guide). It went on
// to say when (the field's own words: a confirmed part change) and why:
// starting moves robots, and only a person knows the material is there.
const AUTO_ARM_NOTE = 'Cut over automatically finishes a started changeover; it never starts one.';

function settingsDraft() { return settingsDraftFor(process() || {}); }

// The same draft for a process that is not the one open — the Edit sheet works
// on a row from the list without navigating to it first.
function settingsDraftFor(p) {
    p = p || {};
    return {
        name: p.name || '',
        description: p.description || '',
        group_id: p.group_id || 0,
        counter_plc_name: p.counter_plc_name || '',
        counter_tag_name: p.counter_tag_name || '',
        counter_enabled: !!p.counter_enabled,
        identity_tag_name: p.identity_tag_name || '',
        identity_none: !!p.identity_none,
        changeover_auto_arm: p.changeover_auto_arm || 'auto',
        flow_composer_enabled: !!p.flow_composer_enabled,
        // Quality containment: DERIVED from the process's produce claims
        // (stamped on the list rows by the server) - the toggle is a batch
        // editor over the claims, which stay the storage the divert reads.
        quality_hold_enabled: !!p.quality_hold_enabled,
        quality_hold_destination: p.quality_hold_destination || '',
        auto_reorder_switch_hmi: !!p.auto_reorder_switch_hmi,
    };
}

// THE LIGHT-CURTAIN INTERLOCK IS PER NODE, so its draft is not on the process
// draft above: S.curtainNodes is the open process's live nodes as the server
// last sent them, and S.curtain is the edit, keyed by node id. safe is true,
// false or null - null is "not chosen", and nothing here turns it into a
// value: which reading means "released" is a site fact, and a default is an
// inverted gate wherever it guesses wrong.
function curtainDraft() {
    const out = {};
    for (const n of (S.curtainNodes || [])) {
        out[n.id] = {
            enabled: !!n.curtain_enabled,
            plc: n.curtain_plc_name || '',
            tag: n.curtain_tag_name || '',
            safe: (n.curtain_safe_value === true || n.curtain_safe_value === false) ? n.curtain_safe_value : null,
        };
    }
    return out;
}

// The open process's live nodes, for the Positions section and the curtain
// switch on each. Every node, not only produce ones: the screen stands where
// the bin is picked up, and the engineer who wired it knows which spot that is.
async function loadCurtainNodes() {
    S.curtainNodes = (await loadLivePositions())
        .filter(n => n.process_id === S.processID)
        .sort((a, b) => (a.sequence - b.sequence) || String(a.name).localeCompare(String(b.name)));
    S.curtain = curtainDraft();
    S.counted = countedDraft();
}

// loadLivePositions reads every live position on this Edge, every process's,
// into S.livePositions and returns it. A failed read is [] here and marks
// nothing: SetNodes still refuses what it must, it just cannot be warned of.
async function loadLivePositions() {
    const res = await send('GET', '/api/process-nodes');
    S.livePositions = res.ok && Array.isArray(res.body) ? res.body : [];
    return S.livePositions;
}

function sharedLoaderNode(name) {
    const n = (S.coreNodes || []).find(c => c.name === name);
    return !!(n && n.shared_loader);
}

// ── where a position may go (a screen's positions picker on Settings) ────────

// positionScreen is the screen of process pid that claims name, from the
// screens' claimed lists the page read, or null.
function positionScreen(name, pid) {
    return stationsOf(pid).find(st => (S.stationNodes[String(st.id)] || []).indexOf(name) >= 0) || null;
}

// positionHeld is why name cannot be a position of process pid: another
// process holds it, and no Core loader shares it (shared_loader).
function positionHeld(name, pid) {
    if (positionScreen(name, pid)) return '';
    const rows = (S.livePositions || []).filter(n => n.core_node_name === name);
    if (rows.some(n => n.process_id === pid)) return '';
    const other = rows[0];
    return other && !sharedLoaderNode(name) ? 'on ' + other.process_name : '';
}

// THE COUNTED-AT DRAFT. S.counted is each position's station counter id, 0 for
// the production counter, and -1 for one + Add picked whose counter is not
// chosen yet. Saved with the rest (SETTINGS_SECTIONS.counted): every row it
// changed, in one write (PUT …/counted-at), which binds all of them or none.
function countedDraft() {
    const out = {};
    for (const n of (S.curtainNodes || [])) out[n.id] = n.line_counter_id || 0;
    return out;
}

// ── Settings' one Save (shared/settings.js) ─────────────────────────────────
//
// THE SHARED SETTINGS PAGE OWNS THE BAR. Its sections are the drafts, one per
// door the server has: each says how it reads, is put back by Discard, is
// checked before anything is sent, and saves. A save runs every dirty section
// in this order, each on its own; the outcome says which saved and which did
// not (settingsPage), and Settings then reads back once (afterSettingsSave).
const PROCESS_KEYS = ['name', 'description', 'group_id', 'counter_plc_name', 'counter_tag_name', 'counter_enabled',
    'identity_tag_name', 'identity_none', 'changeover_auto_arm'];
const GATE_KEYS = ['flow_composer_enabled'];
const HOLD_KEYS = ['quality_hold_enabled', 'quality_hold_destination'];
const REORDER_KEYS = ['auto_reorder_switch_hmi'];

const pickKeys = (o, keys) => Object.fromEntries(keys.map(k => [k, (o || {})[k]]));

// doorOut is a door's answer as settingsPage reads an own save's. A section
// that writes through several doors adds landed: whether any write before the
// refusal went through (afterSettingsSave reads back only what was written).
const doorOut = (out, lead) => (out.ok ? { ok: true } : { ok: false, shown: !!out.shown, error: (lead || '') + (out.error || 'refused') });

// processSection is a section over some of S.settings' keys.
function processSection(label, keys, save, validate) {
    return {
        label: label,
        keys: keys,
        read: () => pickKeys(S.settings, keys),
        put: v => { S.settings = Object.assign({}, S.settings, v); },
        write(v) { this.put(v); drawSettings(); },
        validate: validate,
        save: save,
    };
}

const SETTINGS_SECTIONS = {
    process: processSection('line, counter and changeover', PROCESS_KEYS, async () =>
        doorOut(await send('PUT', '/api/processes/' + S.processID, B().processSettings(process(), S.settings), NO_NOTICE))),
    // Before the routing set: a node moving off a screen into a role has to
    // stop being a position first, and one moving the other way stops being a
    // routing name when its screen claims it (SetNodes).
    // THE POSITIONS ARE THE SET PICK'S POSITIONS BOX, as each screen's list
    // and the positions on no screen (screensDraft); a loader's process keeps
    // a row per screen. `none` is there while a position is on no screen.
    positions: {
        label: 'positions',
        read: () => (coreBoards(process())
            ? Object.assign(Object.fromEntries(stationsOf(S.processID).filter(st => String(st.id) in S.stationNodes)
                .map(st => [st.id, pickerValue(screenKey(st.id))])), looseShown() ? { none: pickerValue(LOOSE) } : {})
            : screensDraft(S.setPicks[ST_SET] ? setPickValue(ST_SET, 'positions') : processPositions(), S.looseOnto)),
        put: v => {
            if (!coreBoards(process())) {
                const names = [];
                for (const id of Object.keys(v)) for (const n of v[id]) if (names.indexOf(n) < 0) names.push(n);
                if (S.setPicks[ST_SET]) S.setPicks[ST_SET].api.set('positions', names);
                return;
            }
            for (const id of Object.keys(v)) {
                const k = id === 'none' ? LOOSE : screenKey(id);
                if (S.pickers[k]) S.pickers[k].sel = v[id].slice();
            }
        },
        write(v) { this.put(v); drawSettings(); },
        save: savePositions,
    },
    // THE ROUTING SET IS THE SET PICK'S THREE ROLE BOXES. A set that could
    // not be read is not part of the draft (null), and Save leaves it alone.
    routing: {
        label: 'routing set',
        read: () => {
            if (routingUnread() || coreBoards(process())) return null;
            const sel = S.setPicks[ST_SET] ? null : settingsSetSel();
            return Object.fromEntries(ROUTING_GROUPS.map(g => [g[0], sel ? sel[g[0]] : setPickValue(ST_SET, g[0])]));
        },
        put: v => { if (v && S.setPicks[ST_SET]) for (const g of ROUTING_GROUPS) S.setPicks[ST_SET].api.set(g[0], v[g[0]] || []); },
        write(v) { this.put(v); drawSettings(); },
        save: saveRouting,
    },
    // THE LINE'S AUTO-REORDER SWITCH SAVES WITH THE PAGE (owner, 2026-11-15:
    // "just do it on settings when saved. keep things simple and consistent"),
    // replacing 2026-11-14's "written as it is pressed". The server still
    // records who switched it, from where and when; the station's own switch
    // is unchanged.
    lineReorder: {
        label: 'auto-reorder for the line',
        read: () => (S.autoReorder ? { on: !!S.arLine } : null),
        put: v => { if (v) S.arLine = !!v.on; },
        write(v) { this.put(v); drawSettings(); },
        save: async d => doorOut(await send('POST', '/api/processes/' + S.processID + '/auto-reorder', { on: d.on }, NO_NOTICE)),
    },
    parts: {
        label: 'parts',
        read: () => (S.partSet === null ? null : pickerValue(PART_SET_PICKER)),
        put: v => { if (v && S.pickers[PART_SET_PICKER]) S.pickers[PART_SET_PICKER].sel = v.slice(); },
        write(v) { this.put(v); drawSettings(); },
        save: async d => doorOut(await writePartSet(d)),
    },
    gate: processSection('operator setup', GATE_KEYS, async d =>
        doorOut(await send('PATCH', '/api/processes/' + S.processID, B().processGate(d.flow_composer_enabled), NO_NOTICE))),
    // QUALITY HOLD rides its own door: the switch stamps or clears the
    // containment route on the process's produce claims.
    hold: processSection('quality hold', HOLD_KEYS, async d =>
        doorOut(await send('POST', '/api/processes/' + S.processID + '/containment-setting',
            B().processContainment(d.quality_hold_enabled, d.quality_hold_destination), NO_NOTICE)),
    d => (d.quality_hold_enabled && !d.quality_hold_destination
        ? { quality_hold_destination: 'Pick a containment destination before switching the quality hold on.' } : null)),
    // WHETHER THE STATION SHOWS THE AUTO-REORDER SWITCH is a setting, on its
    // own door; switching the line is an action (autoReorderSection).
    reorder: processSection('auto-reorder on the station', REORDER_KEYS, async d =>
        doorOut(await send('POST', '/api/processes/' + S.processID + '/auto-reorder-station',
            { shown: !!d.auto_reorder_switch_hmi }, NO_NOTICE))),
    // THE LIGHT-CURTAIN INTERLOCK rides its own door per position: only the
    // positions whose four fields moved are sent, in order, stopping at the
    // first refusal. A curtain switched on needs both pointers and a chosen
    // release value, refused here before the round trip and by the server
    // regardless.
    curtain: {
        label: 'light curtain',
        read: () => S.curtain || {},
        put: v => { S.curtain = v; },
        write(v) { this.put(v); drawSettings(); },
        validate: d => {
            const out = {};
            for (const n of (S.curtainNodes || [])) {
                const c = d[n.id];
                if (!c || !c.enabled) continue;
                if (!String(c.plc || '').trim() || !String(c.tag || '').trim()) {
                    out['curtain-' + n.id] = posName(n) + ': a PLC and a tag are needed for its light curtain.';
                } else if (c.safe === null) {
                    out['curtain-' + n.id] = posName(n) + ': pick which reading releases, TRUE or FALSE.';
                }
            }
            return Object.keys(out).length ? out : null;
        },
        save: async d => {
            const was = curtainDraft();
            let landed = false;
            for (const n of (S.curtainNodes || [])) {
                const c = d[n.id];
                if (!c || JSON.stringify(c) === JSON.stringify(was[n.id])) continue;
                const out = await send('POST', '/api/process-nodes/' + n.id + '/curtain-setting',
                    B().nodeCurtain(c.enabled, c.plc, c.tag, c.safe), NO_NOTICE);
                if (!out.ok) return Object.assign(doorOut(out, posName(n) + ': '), { landed: landed });
                landed = true;
            }
            return { ok: true };
        },
    },
    counted: {
        label: 'counted at',
        read: () => S.counted || {},
        put: v => { S.counted = v; },
        write(v) { this.put(v); drawSettings(); },
        validate: d => {
            const open = (S.curtainNodes || []).filter(n => d[n.id] === -1).map(posName);
            return open.length ? { counted: 'Choose a counter for ' + open.join(', ') + ' under Counted at.' } : null;
        },
        save: async d => {
            const was = countedDraft();
            const bindings = (S.curtainNodes || []).filter(n => (d[n.id] || 0) !== (was[n.id] || 0))
                .map(n => ({ node_id: n.id, line_counter_id: d[n.id] || null }));
            return doorOut(await send('PUT', '/api/processes/' + S.processID + '/counted-at', { bindings: bindings }, NO_NOTICE));
        },
    },
};

// settingsShell is Settings for one visit to one process: the app bar's place,
// the body drawSettings redraws, the shared bar and the popover, drawn once,
// with one settingsPage over them. The bar and the save's outcome live outside
// the body, so a redraw (every pick is one) keeps them.
//
// THE BASELINE IS WHAT IS STORED, and the edits come back after it: a visit to
// another tab keeps a half-made change, and the bar still calls it unsaved.
function settingsShell() {
    const sh = S.stShell;
    if (sh && sh.pid === S.processID && root().contains(sh.sheet)) return sh;
    // BUILT AS ELEMENTS, not one markup string: the shared page keeps
    // references to its bar and its buttons, and so does this shell.
    const make = (tag, cls, attrs, text) => {
        const e = document.createElement(tag);
        if (cls) e.className = cls;
        for (const k of Object.keys(attrs || {})) e.setAttribute(k, attrs[k]);
        if (text) e.textContent = text;
        return e;
    };
    root().innerHTML = '';
    const top = root().appendChild(make('div', 'pd-stbar', { id: 'pd-stbar' }));
    const sheet = root().appendChild(make('div', 'pd-sheet pd-settings set-ui'));
    const body = sheet.appendChild(make('div', 'pd-stbody', { id: 'pd-stbody' }));
    const bar = sheet.appendChild(make('div', 'set-savebar pd-savebar'));
    bar.appendChild(make('span', 'prov', null, 'No unsaved changes'));
    bar.appendChild(make('button', 'pd-btn', { 'data-set': 'discard', type: 'button' }, 'Discard'));
    bar.appendChild(make('button', 'pd-btn primary', { 'data-set': 'save', type: 'button' }, 'Save'));
    const pop = sheet.appendChild(make('div', 'pd-pop', { id: 'pd-stpop' }));
    pop.hidden = true;
    const edits = { settings: S.settings, curtain: S.curtain, counted: S.counted };
    S.settings = settingsDraft();
    S.curtain = curtainDraft();
    S.counted = countedDraft();
    // A REFUSAL STOPS THE WRITES AFTER IT (ruling 3): the parts after it are
    // not sent and stay unsaved, and the bar says which.
    const page = settingsPage(sheet, { sections: SETTINGS_SECTIONS, stopOnFailure: true, afterSave: afterSettingsSave });
    if (edits.settings) S.settings = edits.settings;
    if (edits.curtain) S.curtain = edits.curtain;
    if (edits.counted) S.counted = edits.counted;
    S.stShell = { pid: S.processID, top: top, sheet: sheet, body: body, page: page };
    return S.stShell;
}

// afterSettingsSave reads back once what a save wrote: the positions (curtain
// and counted at), the station counters and the process list. A section that
// saved takes what was read as its new baseline; one that was refused keeps
// its draft on screen, so the engineer fixes the one field instead of
// re-entering it.
async function afterSettingsSave(out) {
    const failed = n => !!(out.own[n] && !out.own[n].ok);
    const kept = {};
    for (const n of Object.keys(SETTINGS_SECTIONS)) if (failed(n)) kept[n] = SETTINGS_SECTIONS[n].read();
    S._tagCache = {};
    const wrote = n => out.own[n] && out.own[n].ok;
    // WHAT A SAVE WROTE IS WHAT IT READS BACK. A section that wrote re-reads
    // what its doors move, and nothing else: one part picked is its write and
    // the part set's read, not every list on the page. A refused section
    // wrote something only where its save says so (landed: the screens and
    // the curtains are one door each, and the ones before the refusal stand).
    const sent = (...names) => names.some(n => out.own[n] && (out.own[n].ok || out.own[n].landed));
    await Promise.all([
        // The live positions: the curtain and Counted at drafts are built on them.
        sent('positions', 'curtain', 'counted') ? loadCurtainNodes() : null,
        // A counter's row says which positions it counts.
        sent('positions', 'counted') ? loadStationCounters() : null,
        // The process rows carry its settings, its quality hold, the station
        // switch and the list's Curtain column.
        sent('process', 'gate', 'hold', 'reorder', 'curtain') ? reloadProcesses() : null,
        wrote('routing') ? loadRouting() : null,
        wrote('parts') ? loadProcessPayloads(S.processID).then(codes => { S.partSet = codes; }) : null,
        wrote('reorder') || wrote('lineReorder') ? loadAutoReorder() : null]);
    // A position or a routing name moved is the Styles tab's too: its pickers
    // and its picture read the composer.
    if (sent('positions') || wrote('routing')) {
        const res = await send('GET', composerURL(S.processID));
        if (res.ok) takeComposer(res.body);
    }
    takeSettingsRead(kept);
    S.settingsError = '';
    S.settingsNotice = S.saveNotes.join(' ');
    S.saveNotes = [];
    drawSettings();
    S.stShell.page.rebase(Object.keys(SETTINGS_SECTIONS).filter(n => !failed(n)));
}

// takeSettingsRead rebuilds Settings' drafts from what was just read, and
// puts back the drafts in kept ({section: its draft}): a failed section's
// after a save, a section not saved yet under a re-read (refreshProcess).
// The caller rebases the rest.
function takeSettingsRead(kept) {
    S.settings = settingsDraft();
    const keepPickers = {};
    if ('positions' in kept) {
        for (const st of stationsOf(S.processID)) keepPickers[screenKey(st.id)] = true;
        keepPickers[LOOSE] = true;
    }
    if ('parts' in kept) keepPickers[PART_SET_PICKER] = true;
    settingsPickers(true, keepPickers);
    // The set pick takes what was read as its starting point, box by box,
    // except the sections that were refused, which keep their drafts.
    const sp = S.setPicks[ST_SET];
    if (sp && sp.pid === S.processID) {
        const read = settingsSetSel();
        if (!('positions' in kept)) { sp.api.set('positions', read.positions, { clean: true }); S.looseOnto = ''; }
        if (!('routing' in kept)) for (const g of ROUTING_GROUPS) sp.api.set(g[0], read[g[0]], { clean: true });
    }
    for (const n of Object.keys(kept)) SETTINGS_SECTIONS[n].put(kept[n]);
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

// cuCombo is a node's curtain typeahead field: a plain input bound to one
// field of that node's curtain draft (data-cn / data-cf, so typing still
// binds and saves exactly what is typed) with the picker's dropdown attached.
// Options load lazily per kind - 'plc' lists WarLink's PLCs with their
// connection state, 'tag' lists the tags of the PLC typed on the SAME node,
// published state marked - and each keystroke filters to the closest
// matches. Picking writes through the same binding a keystroke would, so the
// dropdown saves keystrokes and adds no state of its own.
function cuCombo(nodeID, field, kind) {
    return '<span class="pd-combo" data-combo="' + kind + '" data-cn="' + nodeID + '"' +
        (kind === 'tag' ? ' data-plcsel="[data-cn=&quot;' + nodeID + '&quot;][data-cf=&quot;plc&quot;]"' : '') + '>' +
        '<input class="pd-inp" type="text" data-cn="' + nodeID + '" data-cf="' + field + '" value="' +
        esc(S.curtain[nodeID][field]) + '" autocomplete="off" spellcheck="false">' +
        '<div class="pd-pop pd-combo-list" data-combolist hidden></div>' +
        '</span>';
}

// stCombo is a settings field (data-st) with the tag combo: its PLC from the
// input plcsel names (or the fixed plcname), its options the suggestions of
// kind (tag-suggest.js: counter, identity, or every tag). Its box is the wide
// one: a tag is a struct path (MES_Cell.Prod_Counter_01), and the narrow box
// cut it off.
function stCombo(key, kind, plcsel, plcname) {
    return '<span class="pd-combo" data-combo="tag" data-suggest="' + kind + '"' +
        (plcsel ? ' data-plcsel="' + esc(plcsel) + '"' : '') + (plcname ? ' data-plcname="' + esc(plcname) + '"' : '') + '>' +
        '<input class="pd-inp wide" type="text" data-st="' + key + '" value="' + esc(S.settings[key]) +
        '" autocomplete="off" spellcheck="false">' +
        '<div class="pd-pop pd-combo-list" data-combolist hidden></div></span>';
}

function stToggle(key) {
    return '<button class="pd-chk' + (S.settings[key] ? ' on' : '') + '" data-act="st-toggle" data-st="' + key +
        '" role="switch" aria-checked="' + (S.settings[key] ? 'true' : 'false') + '"></button>';
}

// identityField is the part-identity tag: derived from the counter tag (the
// default), typed over it, or none (the process has no identity tag).
// OVERRIDE PRESSED IS OVERRIDE before a tag is typed (F20): the segment read
// the mode off the tag alone, so it drew the tag box and kept Derive lit.
// S.identityOverride is the draft it was pressed on, so a draft rebuilt from
// the store (Discard, a save, another process) forgets it.
function identityField() {
    const mode = S.settings.identity_none ? 'none'
        : (S.settings.identity_tag_name || S.identityOverride === S.settings ? 'override' : 'derive');
    const seg = [['derive', 'Derive'], ['override', 'Override'], ['none', 'None']].map(m =>
        '<button class="' + (mode === m[0] ? 'on' : '') + '" data-act="st-ident" data-mode="' + m[0] + '">' +
        esc(m[1]) + '</button>').join('');
    const derived = window.TagSuggest ? window.TagSuggest.derivedIdentity(S.settings.counter_tag_name) : '';
    const sub = mode === 'none' ? 'no part identity: no wrong-part alert, no automatic cutover or switch'
        : mode === 'derive' ? (derived ? 'reads ' + derived : 'the counter tag names no struct member to derive from')
            : '';
    return stField('Read from', sub, '<div class="pd-seg">' + seg + '</div>') +
        (mode === 'override'
            ? stField('Identity tag', '', stCombo('identity_tag_name', 'identity', '[data-st="counter_plc_name"]', ''))
            : '');
}

// countingView is what the Production counter section's live words and its
// MES slots and station counters rows read (station-card.js): the process as
// saved, the slots' read, the station counters, the positions (each counter
// says which it counts), the process's health items, its styles, and the
// plant-local time formats.
function countingView(p) {
    return { process: p, slots: S.countingSlots || [], counters: S.stationCounters || [],
        positions: S.curtainNodes || [],
        items: S.catidItems || [], styles: stylesOf(p.id), fmt: formatTime, clock: formatClock };
}

// PRESS_INDEX_COUNTING is what the Counting switch also decides for a
// process with a press-index position, said only where one exists. ONE LINE
// (owner, 2026-11-14; a note is 68ch wide): it went on to say a sequential
// press reuses its parked empty either way.
const PRESS_INDEX_COUNTING = 'Index Swap keeps a drained same-part bin at changeover only while counted.';

// hasPressIndexClaim: some live style of the open process claims a position
// in press-index mode.
function hasPressIndexClaim() {
    return ((S.composer && S.composer.styles) || []).some(st =>
        (st.claims || []).some(c => c.swap_mode === 'two_robot_press_index'));
}

// healthSection is the open process's health items, worst first, each with its
// verbs (CatidHealth.itemHTML, the one renderer): the red and amber ones the
// list's count stands for, beside the info ones (a hold to clear, an incoming
// style with no CATIDs) that need no one now. It heads the tab, so the count
// that opened it lands on what it counted; the live words in its title are the
// count (33-settings-pages.md rule 2). A second counter of the same stroke is
// said on its slot's line in the MES slots row instead, when that row has the
// slot. No items, no section.
function healthSection() {
    const h = window.CatidHealth;
    if (!h) return '';
    const items = healthItems();
    if (!items.length) return '';
    const need = healthNeed();
    return '<div class="pd-sect"><h2>Health</h2><span class="pd-cnt' + (need ? ' warn' : '') + '"><i></i>' +
        (need ? need + ' to look at' : 'nothing needs anyone now') + '</span></div>' +
        '<div class="pd-sfld pd-scl-fld"><label>Items<small>worst first; hover one for its detail</small></label>' +
        '<div class="v col">' + items.map(it => h.itemHTML(S.processID, it)).join('') + '</div></div>';
}

// loadCatidItems reads the open process's health items, for the slots row's
// words and the health section ([] on a failed read).
async function loadCatidItems() {
    const res = await send('GET', '/api/processes/' + S.processID + '/catid-health', undefined, NO_NOTICE);
    S.catidItems = res.ok ? ((res.body || {}).items || []) : [];
}

// loadStationVerdicts reads the binding's dry run for every position of the
// process, in one read. Its one reader is Counted at's + Add, so it is read
// when that list opens, fresh each time (a bind changes the answers). A failed
// read leaves every position greyed as not checked.
async function loadStationVerdicts() {
    S.stationVerdicts = {};
    const res = await send('GET', '/api/processes/' + S.processID + '/countable');
    if (res.ok && res.body) S.stationVerdicts = res.body;
}

async function loadCountingSlots() {
    const res = await send('GET', '/api/processes/' + S.processID + '/counting-slots');
    S.countingSlots = res.ok ? ((res.body || {}).slots || []) : [];
}

// loadStationCounters reads the process's station counters ([] on a failed read).
// loadAutoReorder reads the line's auto-reorder switch: how many positions a
// person has switched off, of how many, and the last flip. Whether the station
// shows it is the process row's (auto_reorder_switch_hmi).
async function loadAutoReorder() {
    const res = await send('GET', '/api/processes/' + S.processID + '/auto-reorder', undefined, NO_NOTICE);
    S.autoReorder = res.ok ? res.body : null;
    // The draft's switch starts where the line is: on unless a person has
    // switched a position off.
    S.arLine = S.autoReorder ? !(S.autoReorder.switched_off > 0) : null;
}

async function loadStationCounters() {
    const res = await send('GET', '/api/processes/' + S.processID + '/station-counters');
    S.stationCounters = res.ok ? res.body || [] : [];
}

// openStationCounter is the station counter's editor: its label, its tag and
// an optional second tag (the process's counter PLC; the counter counts the
// two tags' sum), with the rule. bindNode, when given, is the Counted at row
// whose draft counts at the new counter once it is made.
function openStationCounter(id, bindNode) {
    const p = process() || {};
    const c = (S.stationCounters || []).find(x => x.id === id) || { label: '', tag_name: '', tag_name_2: '' };
    // The station kind: each station struct's count member by the family's
    // convention. Not the counter kind, which leads with the MES slots a
    // station counter may not read (stationTagFree).
    const tagBox = (inputID, value) =>
        '<span class="pd-combo" data-combo="tag" data-suggest="station" data-plcname="' + esc(p.counter_plc_name || '') + '">' +
        '<input class="pd-inp wide" id="' + inputID + '" value="' + esc(value) + '" autocomplete="off" spellcheck="false">' +
        '<div class="pd-pop pd-combo-list" data-combolist hidden></div></span>';
    pageSheet(id ? 'Edit station counter' : 'New station counter', window.StationCard.RULE,
        '<label class="pd-lbl">Label</label><input class="pd-inp wide" id="sc-label" value="' + esc(c.label) + '">' +
        '<label class="pd-lbl">Tag on ' + esc(p.counter_plc_name || '') + '</label>' + tagBox('sc-tag', c.tag_name) +
        '<label class="pd-lbl">Second tag (optional)</label>' + tagBox('sc-tag2', c.tag_name_2 || '') +
        '<p class="pd-note">' + esc(window.StationCard.SECOND_TAG) + '</p>',
        'Save', async () => {
            const body = { label: $('sc-label').value, tag_name: $('sc-tag').value, tag_name_2: $('sc-tag2').value };
            const url = id ? '/api/station-counters/' + id : '/api/processes/' + S.processID + '/station-counters';
            // A new counter is made once per sheet: its binding refused is
            // resumed, not a second counter (ruling 5).
            const out = await send(id ? 'PUT' : 'POST', url, body, NO_NOTICE);
            if (!out.ok) { sheetStatus(said(out), true); return; }
            // From a Counted at row's "New station counter…": the counter is
            // made now, and the row counts at it in the draft; the binding is
            // written by Save settings with the rest.
            if (!id && bindNode && bodyOf(out).id) S.counted[bindNode] = Number(bodyOf(out).id);
            closeSheet();
            await afterStationChange();
            // A new counter counts nothing yet: the positions it counts are
            // picked under Counted somewhere else, the one editor of a binding.
            if (!id && !bindNode && bodyOf(out).id) {
                S.settingsNotice = 'Station counter added. Pick the positions it counts under Counted somewhere else.';
                drawSettings();
            }
        });
    bindCombos(sheetEl());
}

async function afterStationChange() {
    S._tagCache = {};
    const counted = S.counted;
    await Promise.all([loadStationCounters(), loadCurtainNodes(), loadCountingSlots(), loadCatidItems()]);
    keepDraft(S.counted, counted);
    drawSettings();
}

// setCountedAt sets the counter a position's draft counts at ('0' the
// production counter, 'new' to make one for it). From a row's select, a row
// still waiting for its counter (-1, + Add's) takes it, and so does every
// other row waiting: the positions one pick added count at one counter, each
// changeable after.
function setCountedAt(nodeID, value, fromSelect) {
    if (value === 'new') {
        openStationCounter(0, nodeID);
        return;
    }
    const to = Number(value) || 0;
    if (fromSelect && S.counted[nodeID] === -1) {
        for (const id of Object.keys(S.counted)) if (S.counted[id] === -1) S.counted[id] = to;
    }
    S.counted[nodeID] = to;
    drawSettings();
}

// countedAtField is a position's "Counted at": the production counter, one of
// the process's station counters, or a new one.
function counterName(c) { return window.StationCard ? window.StationCard.nameOf(c) : c.label || c.tag_name; }

function countedAtField(n) {
    const at = S.counted[n.id] || 0;
    const c = (S.stationCounters || []).find(x => x.id === at);
    const label = at === -1 ? 'Choose a counter' : at === 0 ? 'Production counter' : c ? counterName(c) : 'Production counter';
    return '<button class="pd-sel' + (at === -1 ? ' bad' : '') + '" data-act="ca-pick" data-node="' + n.id + '">' +
        esc(label) + '<i class="car"></i></button>';
}

// openCountedPick is a Counted at row's counter, on the shared picker: the
// production counter, one of the process's station counters, or a new one.
function openCountedPick(btn, nodeID) {
    const pop = $('pd-stpop');
    if (!pop) return;
    const rows = [{ value: '0', label: 'Production counter' }]
        .concat((S.stationCounters || []).map(c => ({ value: String(c.id), label: counterName(c) })))
        .concat([{ value: 'new', label: 'New station counter…' }]);
    openRowPicker(pop, btn, rows, v => setCountedAt(nodeID, v, true),
        { selected: String(S.counted[nodeID] || 0), placeholder: 'find a counter' });
}

// ── Settings · the per-position short lists ──────────────────────────────────
//
// A SETTING KEPT PER POSITION IS SAID AS ITS RULE AND THE POSITIONS THAT DIFFER
// (shared/exceptions.js; owner ruling 2026-11-14), not as one row per position
// saying the default: a 23-position press drew 23 rows of "light curtain off ·
// counted at the production counter". Each sits with its subject: Counted
// somewhere else with the production counter, Light curtain at the station.
// The card's heading is the setting's name, so the list draws no name column
// of its own (bare).
const NO_POSITIONS_NOTE = 'This process has no positions yet: pick them under Positions and routing.';

function curtainCard(nodes) {
    return '<div class="pd-sect"><h2>Light curtain</h2><span class="pd-dim">positions where a release waits for the curtain</span></div>' +
        (nodes.length ? curtainExceptions(nodes) : '<p class="pd-note">' + esc(NO_POSITIONS_NOTE) + '</p>');
}

function countedCard(nodes) {
    return '<div class="pd-sect"><h2>Counted somewhere else</h2><span class="pd-dim">positions whose strokes do not count at the production counter</span></div>' +
        (nodes.length ? countedAtExceptions(nodes) : '<p class="pd-note">' + esc(NO_POSITIONS_NOTE) + '</p>');
}

// posName is a position as the exception rows name it.
function posName(n) {
    return n.name + (n.core_node_name && n.core_node_name !== n.name ? ' (' + n.core_node_name + ')' : '');
}

// curtainExceptions: the positions whose light curtain is on, each with its
// PLC, tag and release value.
//
// FG LIGHT-CURTAIN INTERLOCK - a per-node opt-in. Off (the default) means
// releases at the node go as always. On, a produce release there waits for the
// node's curtain tag to read its release value, checked by a direct WarLink
// read at the click (fail-closed on any read trouble); the operator station's
// RELEASE button greys when the cached value disagrees. The release value
// starts unchosen and must be picked before the node can be saved switched on
// - the server refuses the same, and the list says so while one is unchosen.
// + Add and × are drafts, saved by Save settings.
function curtainExceptions(nodes) {
    const on = nodes.filter(n => S.curtain[n.id] && S.curtain[n.id].enabled);
    const seg = n => [[true, 'Releases on TRUE'], [false, 'on FALSE']].map(v =>
        '<button class="' + (S.curtain[n.id].safe === v[0] ? 'on' : '') + '" data-act="cu-val" data-node="' + n.id +
        '" data-val="' + v[0] + '">' + esc(v[1]) + '</button>').join('');
    const unchosen = on.filter(n => S.curtain[n.id].safe === null).map(posName);
    return exceptionList({
        id: 'curtain', label: 'Light curtain', bare: true,
        summary: on.length ? countOf(on.length, nodes.length, 'position')
            : 'off at ' + (nodes.length === 1 ? 'its one position' : 'all ' + nodes.length + ' positions'),
        rows: on.map(n => ({
            name: posName(n),
            cells: [cuCombo(n.id, 'plc', 'plc'), cuCombo(n.id, 'tag', 'tag'), '<div class="pd-seg">' + seg(n) + '</div>'],
            remove: { act: 'cu-off', node: n.id },
        })),
        add: on.length < nodes.length ? { label: '+ Add a position', act: 'cu-add' } : null,
    }) + (unchosen.length
        ? '<p class="pd-note">Pick which reading releases at ' + esc(unchosen.join(', ')) +
            ': it is a site fact, and there is no default.</p>'
        : '');
}

// countedAtExceptions: the positions counted at a station counter instead of
// the production counter, each with its counter, from the draft (countedDraft):
// a position + Add picked sits here, its counter to choose when the process has
// more than one. Saved by Save settings.
function countedAtExceptions(nodes) {
    const bound = nodes.filter(n => (S.counted[n.id] || 0) !== 0);
    const at = nodes.length - bound.length;
    return exceptionList({
        id: 'counted', label: 'Counted somewhere else', bare: true,
        summary: (at === nodes.length ? 'all ' + nodes.length : countOf(at, nodes.length, 'position')) + ' at the production counter',
        rows: bound.map(n => ({
            name: posName(n),
            cells: [countedAtField(n)],
            remove: { act: 'ca-off', node: n.id },
        })),
        add: bound.length < nodes.length ? { label: '+ Add a position', act: 'ca-add' } : null,
    });
}

// positionFacts: what the old one-row-per-position wall said and no list
// does: a position that is off until its active order finishes. Said only
// when there is one, under the screen lines of Positions and routing. A
// position on no screen is said by the screen lines themselves.
function positionFacts(nodes) {
    const retiring = nodes.filter(n => n.enabled === false).map(posName);
    return retiring.length ? '<p class="pd-note">Off until its active order finishes: ' + esc(retiring.join(', ')) + '.</p>' : '';
}

// openRowPicker is the shared picker in a popover (pop, anchored to btn) over
// rows (shared/picker.js's shape: a row with a reason is greyed with it). One
// pick closes it and hands onPick the value; with multi each tick is one row,
// there is no tick-all, and what is ticked is handed to onPick, as a list, as
// the popover closes (Escape, a press outside). Settings' + Add lists and Set
// for this style's are this one.
//
// A PRESS IS INSIDE THE LIST. The shell redraws its rows on a tick, so the
// row clicked is out of the document by the time the click reaches the page's
// closer (boot: a click outside every .pd-pop closes it), and closest('.pd-pop')
// reads it as a click outside: a multi list closed after the first tick, and a
// list opened by a pick (Set for this style's position, after the setting) was
// closed by the press that picked. Its clicks stop at the list while it is
// open.
//
// opts: multi, placeholder, selected (the value(s) on as it opens), head (a
// line above the list, already escaped: what is being picked, or why a list is
// short), empty (the line when nothing is offered or matches).
function openRowPicker(pop, btn, rows, onPick, opts) {
    const o = opts || {};
    pop.innerHTML = o.head ? '<div class="pd-lbl">' + o.head + '</div>' : '';
    // The shell replaces its host's content, so a heading needs a host of its own.
    const host = o.head ? pop.appendChild(document.createElement('div')) : pop;
    let picked = [];
    // THE PRESS THAT PICKS IS STILL INSIDE when the list closes under it: a
    // pick that opens the next popover (+ Add a position's mode menu) had its
    // press reach the document's closer, which shut the new menu at once,
    // because this listener went with the list. It stays for that press and
    // goes with it; a list closed any other way takes it at once.
    let state = 'open';
    const inside = e => {
        if (state !== 'closed') e.stopPropagation();
        if (state !== 'open') pop.removeEventListener('click', inside);
    };
    pop.addEventListener('click', inside);
    openLayer(pop, { kind: 'popover', anchor: btn, focus: false, onClose: w => {
        if (state === 'open') { state = 'closed'; pop.removeEventListener('click', inside); }
        if (w !== 'close') closePop();
        if (o.multi && picked.length) onPick(picked.slice());
    } });
    mountPicker(host, null, () => rows, {
        multi: !!o.multi, open: true, chips: false, placeholder: o.placeholder || '',
        selected: o.selected, empty: o.empty ? () => o.empty : undefined,
        onChange: v => {
            if (o.multi) { picked = v.slice(); return; }
            state = 'picked';
            closeLayer(pop);
            pop.hidden = true;
            pop.innerHTML = '';
            if (v) onPick(v);
        },
    });
    placePopover(pop, btn);
}

// openPositionPicker is + Add's list: the shared picker over this process's
// positions; a position the setting cannot take is greyed with why (why(n), ''
// for none). One at a time, or with multi several, handed over as ids.
function openPositionPicker(btn, nodes, why, onPick, multi) {
    const pop = $('pd-stpop');
    if (!pop) return;
    const rows = nodes.map(n => {
        const r = { value: String(n.id), label: posName(n) };
        const no = why(n);
        if (no) r.reason = no;
        return r;
    });
    openRowPicker(pop, btn, rows, v => onPick(multi ? v.map(Number) : Number(v)),
        { multi: !!multi, placeholder: multi ? 'find positions' : 'find a position' });
}

// openCurtainAdd: + Add under Light curtain, over the positions whose curtain
// is off. A pick switches it on in the draft, its release value unchosen.
function openCurtainAdd(btn) {
    const off = (S.curtainNodes || []).filter(n => S.curtain[n.id] && !S.curtain[n.id].enabled);
    openPositionPicker(btn, off, () => '', id => {
        S.curtain[id].enabled = true;
        drawSettings();
    });
}

// openCountedAdd: + Add under Counted at, over the positions the draft counts
// at the production counter, each refused one greyed with the binding's own
// sentence (the dry run, read as the list opens). Several in one pick, one row
// each, all at the same counter: the process's only station counter, or one
// to choose per row when it has several (or none yet: New station counter…).
async function openCountedAdd(btn) {
    await loadStationVerdicts();
    const v = S.stationVerdicts || {};
    const free = (S.curtainNodes || []).filter(n => !(S.counted[n.id] || 0));
    const counters = S.stationCounters || [];
    const at = counters.length === 1 ? counters[0].id : -1;
    openPositionPicker(btn, free,
        n => (v[n.id] ? (v[n.id].countable ? '' : v[n.id].refusal) : 'not checked: the dry run could not be read'),
        ids => {
            for (const id of ids) S.counted[id] = at;
            drawSettings();
        }, true);
}

// The positions one screen holds, read fresh, or null when the read failed.
//
// THE ADD AND THE REMOVE SEND WHAT THIS RETURNS, never S.stationNodes.
// claimed-nodes is a set-to: a name left out of the list is a position taken
// off the screen. refreshStationNodes keeps the old list when its read fails,
// and an add sent over a stale list would remove whatever the page had not
// seen yet — so a failed read stops the write instead.
async function readStationNodes(stationID) {
    const res = await send('GET', '/api/operator-stations/' + stationID + '/claimed-nodes');
    if (!res.ok) return null;
    return Array.isArray(res.body) ? res.body : [];
}

// keepDraft puts a draft's entries back over a fresh read, for the positions
// still there: a re-read is not the edit's to throw away.
function keepDraft(fresh, was) {
    for (const id of Object.keys(was || {})) if (id in fresh) fresh[id] = was[id];
}

// THE COUNTER'S PLC IS A PICK (F12) from the PLCs WarLink reports, the read
// the Configuration page lists them from (/api/plcs, once per page: the
// curtain's PLC boxes share it), with each one's connection; the Tag
// typeahead under it keys on the PLC picked. A saved PLC WarLink does not
// report (renamed, or WarLink down) stays the answer and says so, so drawing
// the pick never changes what is saved. A read that failed keeps the typed
// box: a field is not lost to a read.
function plcPick() {
    const cur = S.settings.counter_plc_name || '';
    if (!Array.isArray(S._plcList)) return stText('counter_plc_name');
    const known = S._plcList.find(p => p.name === cur);
    // The pick is the shared picker (openPLCPick); the hidden field carries
    // the value the Tag's typeahead keys on ([data-st="counter_plc_name"]).
    return '<button class="pd-sel" data-act="st-plc">' + esc(cur ? cur + (known ? '' : ' · not reported by WarLink') : 'none') +
        '<i class="car"></i></button><input type="hidden" data-st="counter_plc_name" value="' + esc(cur) + '">';
}

function openPLCPick(btn) {
    const pop = $('pd-stpop');
    if (!pop || !Array.isArray(S._plcList)) return;
    const cur = S.settings.counter_plc_name || '';
    const plcs = S._plcList.slice().sort((a, b) => String(a.name).localeCompare(String(b.name)));
    const rows = [{ value: '-', label: 'none' }]
        .concat(cur && !plcs.some(p => p.name === cur) ? [{ value: cur, label: cur, note: 'not reported by WarLink' }] : [])
        .concat(plcs.map(p => ({ value: p.name, label: p.name, note: p.connected ? 'connected' : 'not connected' })));
    openRowPicker(pop, btn, rows, v => {
        S.settings.counter_plc_name = v === '-' ? '' : v;
        drawSettings();
    }, { selected: cur || '-', placeholder: 'find a PLC' });
}

// loadPLCs reads WarLink's PLCs once per page for the PLC pick; a failed read
// is not cached (null: the typed box, and the next open asks again).
async function loadPLCs() {
    if (Array.isArray(S._plcList)) return;
    const res = await send('GET', '/api/plcs');
    S._plcList = res.ok && Array.isArray(res.body) ? res.body : null;
}

// AUTO-REORDER, THE LINE'S SWITCH (owner, 2026-11-13). Off is a person's off
// at every position: nothing asks on its own until someone turns it back on,
// here or on the station where it is shown. On needs the line's counter,
// configured and reading; the server refuses it otherwise and says why.
//
// THE SWITCH IS IN THE PAGE'S DRAFT (owner, 2026-11-15: "just do it on
// settings when saved. keep things simple and consistent"), written by the
// one Save (SETTINGS_SECTIONS.lineReorder); the server still records who,
// where and when, and the row reads the last switch back. Whether the station
// shows the switch is a setting too (SETTINGS_SECTIONS.reorder).
function autoReorderSection() {
    const head = '<div class="pd-sect"><h2>Auto-reorder</h2><span class="pd-dim">needs the production counter</span></div>';
    const station = stField('Show the switch on the station', '', stSwitch('auto_reorder_switch_hmi',
        'On · operators can switch the line there too', 'Off · the line is switched here only'));
    const a = S.autoReorder;
    if (!a) return head + '<p class="pd-note">This line’s switch could not be read. Reload the page to switch it.</p>' + station;
    const on = !!S.arLine;
    const stored = !(a.switched_off > 0);
    const partial = a.switched_off > 0 && a.switched_off < a.positions;
    const mean = on ? 'On · positions armed on their style rows ask on their own'
        : on === stored && partial ? 'Off at ' + a.switched_off + ' of ' + a.positions + ' positions'
            : 'Off · no position asks on its own';
    const when = at => { const d = new Date(at); return isNaN(d) ? '' : d.toLocaleString(); };
    const last = a.last_flip;
    const said = last
        ? 'switched ' + (last.turned_on ? 'on' : 'off') + ' by ' + last.by + (last.source === 'station' ? ' from the station' : '') + ', ' + when(last.at)
        : 'never switched';
    return head +
        stField('This line', said, '<span class="pd-switch"><button class="pd-chk' + (on ? ' on' : '') + '" data-act="ar-toggle" role="switch" aria-checked="' +
            (on ? 'true' : 'false') + '"></button><span class="pd-swmean">' + esc(mean) + '</span></span>') +
        station;
}

// THE SECTIONS ARE GROUPED BY WHAT THEY ARE ABOUT (owner, 2026-11-15: "isnt
// positions settings kind of linked with the cat id stuff"): the line itself,
// counting and part identity, what happens at the station, and Danger. Health
// heads the page, in no group, while there is something on it. Within a group
// two half cards share a row and are the same height; a card holding a table
// or a list takes the full width.
const SETTINGS_GROUPS = [
    ['', ['Health']],
    ['The line', ['Line', 'Parts', 'Positions and routing']],
    ['Counting and part identity', ['Production counter', 'Part identity (CATID)', 'Counted somewhere else']],
    ['At the station', ['Operator setup', 'Auto-reorder', 'Quality hold', 'Light curtain']],
    ['Danger', ['Danger']],
];

// THE SECTION LIST is the sections in their order down the left, under their
// groups' headings, each a button to its card, with a dot on one that needs
// attention (stAttention): a page of twelve sections is not scrolled through
// to reach the ninth, and the one still to fill in says so before it is
// reached.
const SETTINGS_ORDER = [].concat(...SETTINGS_GROUPS.map(g => g[1]));

// settingsOrder is SETTINGS_ORDER as this process's Settings draws it: Health
// only while it has items.
function settingsOrder() {
    return SETTINGS_ORDER.filter(h => h !== 'Health' || healthSection());
}

// redrawSettingsNav draws the section list again, its dots from the page as it
// is now.
function redrawSettingsNav() {
    const nav = root().querySelector('.pd-stnav');
    if (nav) nav.outerHTML = settingsNav(settingsOrder());
}

// LOADER_ORDER is a loader's or unloader's Settings (coreBoards).
const LOADER_ORDER = ['Line', 'Screens and positions', 'Loader board', 'Danger'];

// stAttention is why a section's dot is lit, or '': Health with an item to
// look at, a production counter not wired, a line still missing positions,
// an inbound or an outbound, and a line with no part.
function stAttention(name) {
    const p = process() || {};
    if (name === 'Health') return healthNeed() ? 'something to look at' : '';
    if (name === 'Production counter') return p.counter_plc_name && p.counter_tag_name ? '' : 'not wired';
    // The pick is mounted after the cards are drawn, so the first draw reads
    // what it will be mounted with. A routing set that could not be read has
    // its boxes closed: nothing in them is known to be missing, so only
    // Positions can ask.
    if (name === 'Positions and routing') {
        const sp = S.setPicks[ST_SET];
        const sel = sp && sp.pid === S.processID ? sp.api.values() : settingsSetSel();
        const need = SET_BOXES.filter(b => b.need === 'needed' && !(sel[b.key] || []).length &&
            (b.key === 'positions' || !routingUnread())).map(b => b.label);
        return need.length ? 'still to pick: ' + need.join(', ') : '';
    }
    // A part a saved flow runs is one the line may run (the palette).
    if (name === 'Parts') return S.partSet !== null && !pickerValue(PART_SET_PICKER).length && !(S.composer.palette || []).length ? 'no part yet' : '';
    return '';
}

// settingsNav is the list for the sections in order. A section of a group
// with more than one card comes under the group's heading; a group of one
// (Danger) is its own button. A loader's short list has no groups.
function settingsNav(order) {
    const btn = h => {
        const why = stAttention(h);
        return '<button type="button" data-act="st-jump" data-to="' + esc(h) + '"' + (why ? ' title="' + esc(why) + '"' : '') + '>' +
            '<span>' + esc(h) + '</span>' + (why ? '<i class="pd-stdot" aria-label="' + esc(why) + '"></i>' : '') + '</button>';
    };
    const groupOf = h => SETTINGS_GROUPS.find(g => g[1].indexOf(h) >= 0);
    let at = null;
    return '<nav class="pd-stnav" aria-label="Settings sections">' + order.map(h => {
        const g = groupOf(h);
        const head = g && g !== at && g[0] && g[1].length > 1 ? '<span class="pd-stnavg">' + esc(g[0]) + '</span>' : '';
        at = g || at;
        return head + btn(h);
    }).join('') + '</nav>';
}

// stCard is one section as a card: its markup (which opens with its heading),
// whether it takes the page's full width, and its tone ('warn': the outline
// says it still needs filling in).
function stCard(name, html, wide, tone) {
    if (!html) return '';
    return '<section class="pd-card' + (wide ? ' wide' : '') + (tone ? ' ' + tone : '') + '" data-sect="' + esc(name) + '">' + html + '</section>';
}

// settingsHeading is the card named text (its section name, which the list
// links by), else the card whose heading starts with text, or that heading.
function settingsHeading(text) {
    const card = [...root().querySelectorAll('section.pd-card')].find(x => x.dataset && x.dataset.sect === text);
    if (card) return card;
    const h = [...root().querySelectorAll('h2')].find(x => String(x.textContent || '').indexOf(text) === 0) || null;
    return h ? (h.closest ? h.closest('.pd-card') || h : h) : null;
}

// stSwitch is an on/off setting as a switch with what it means beside it,
// in the state it is in.
function stSwitch(key, on, off) {
    return '<span class="pd-switch">' + stToggle(key) + '<span class="pd-swmean">' + esc(S.settings[key] ? on : off) + '</span></span>';
}

function drawSettings() {
    if (!S.settings) S.settings = settingsDraft();
    const p = process() || {};
    const counting = !!(p.counter_plc_name && p.counter_enabled);
    const group = S.groups.find(g => g.id === S.settings.group_id);

    const line = '<div class="pd-sect"><h2>Line</h2></div>' +
        stField('Name', '', stText('name')) +
        stField('Description', '', stText('description', true)) +
        stField('Group', 'groups processes on the list',
            '<button class="pd-sel" data-act="st-group">' + esc(group ? group.name : 'Ungrouped') + '<i class="car"></i></button>');

    // THE LIVE STATE IS THE TITLE'S WORDS (33-settings-pages.md rule 2): what
    // the process counts on, its CATID and style, its last stroke, from the
    // process as saved and the slots' read. The MES slots and the station
    // counters are this section's last two rows: they are facts about this
    // counter, not a section of their own.
    const card = window.StationCard;
    const view = countingView(p);
    const counter = '<div class="pd-sect"><h2>Production counter</h2>' +
        (card ? card.titleHTML(view)
            : '<span class="pd-cnt' + (counting ? ' ok' : '') + '"><i></i>' + esc(counting ? 'counting' : 'not wired') + '</span>') +
        '<span class="pd-dim">needed for auto-reorder</span></div>' +
        // WarLink's own list, not one Shingo keeps: a PLC missing here is one
        // WarLink was never told about, so the field says where to add it.
        stField('PLC', 'PLCs set up in WarLink', plcPick()) +
        stField('Tag', '', stCombo('counter_tag_name', 'counter', '[data-st="counter_plc_name"]', '')) +
        // Auto-reorder needs the line's counter configured and reading: the
        // server refuses switching it on otherwise.
        stField('Counting', '', stSwitch('counter_enabled', 'On · this line’s strokes are counted', 'Off · not counted; auto-reorder needs it')) +
        (hasPressIndexClaim() ? '<p class="pd-note">' + esc(PRESS_INDEX_COUNTING) + '</p>' : '') +
        (card ? card.countingHTML(view) : '');

    // PART IDENTITY AND WHAT A CHANGE OF IT DOES ARE ONE CARD: the auto-arm
    // choice acts on a confirmed CATID change, so it sits with where the CATID
    // is read from. Three long answers do not fit a half card side by side, so
    // they stack (.pd-seg.col).
    const seg = AUTO_ARM.map(m => '<button class="' + (S.settings.changeover_auto_arm === m[0] ? 'on' : '') +
        '" data-act="st-arm" data-arm="' + m[0] + '">' + esc(m[1]) + '</button>').join('');
    const identity = '<div class="pd-sect"><h2>Part identity (CATID)</h2><span class="pd-dim">which part the PLC says is running</span></div>' +
        identityField() +
        stField('When the part changes', 'a confirmed CATID change',
            '<div class="pd-seg col">' + seg + '</div>') +
        '<p class="pd-note">' + esc(AUTO_ARM_NOTE) + '</p>';

    if (!S.curtain) S.curtain = curtainDraft();
    const nodes = S.curtainNodes || [];

    // PARTS THIS PROCESS RUNS (pick-a-part U5, R-I): the part set the floor
    // picks from, a draft saved by the one Save (SETTINGS_SECTIONS.parts). Its
    // count is the heading's; Sync catalog is the section's action, not a
    // setting: it asks Core for the catalog the picker offers.
    const partsSect = '<div class="pd-sect"><h2>Parts</h2>' +
        (S.partSet === null ? '' : '<span class="pd-dim">' + pickerValue(PART_SET_PICKER).length + '</span>') +
        '<span class="pd-spacer"></span>' +
        '<button class="pd-btn" data-act="st-sync">Sync catalog</button></div>' +
        (S.partSet === null
            ? '<p class="pd-note">This process’s part list could not be read. Reload the page to change it.</p>'
            : '<div class="pd-partsbox">' + pickerBox(PART_SET_PICKER) + '</div>') +
        '<p class="pd-note">' + esc(PART_SET_HELP.charAt(0).toUpperCase() + PART_SET_HELP.slice(1) + '. ' + PART_SET_NOTE) + '</p>';

    // EVERY SECTION IS OPEN (owner, 2026-11-14): Operator setup and Quality
    // hold show their controls in place, in the page's one draft.
    // OPERATOR SETUP: the switch and the recipes it sets styles up from, one
    // feature (F36). THE SWITCH LOCKS THE FLOOR'S ONE EDIT (owner ruling R-K,
    // 2026-10-18). Off, the operator picks parts and Shingo runs the
    // engineer's recipe as set up. On, one more screen lets them move a
    // recipe's slot to another position or staging lane — never a mode, a
    // pairing or a role — and the result is saved as a style with the recipe
    // as its provenance.
    const setup = '<div class="pd-sect"><h2>Operator setup</h2><span class="pd-spacer"></span>' +
        '<button class="pd-btn" data-act="new-recipe">New recipe</button></div>' +
        stField('Operators set styles up', '', stSwitch('flow_composer_enabled',
            'On · operators set styles up from a recipe, on the station', 'Off · operators pick from the styles')) +
        hmiRecipes();

    // QUALITY HOLD — a per-process opt-in. Off (the default) means every
    // finished-goods delivery goes to its outbound destination as always; on,
    // a payload flagged for quality containment diverts its bins to the
    // destination picked here instead. The toggle stamps or clears the
    // containment route on the produce claims of every style live at toggle
    // time; a style minted later copies the current route (engine mintStamp).
    // Core's divert reads those claims;
    // the claim is the one storage, this screen is its batch editor.
    const holdOn = !!S.settings.quality_hold_enabled;
    const dest = S.settings.quality_hold_destination || '';
    const hold = '<div class="pd-sect"><h2>Quality hold</h2></div>' +
        stField('Contain finished goods on a quality alert', '', stSwitch('quality_hold_enabled',
            'On · they go to the containment destination', 'Off · finished goods go to their outbound destination')) +
        (holdOn
            ? stField('Containment destination', '',
                '<button class="pd-sel" data-act="st-holddest">' +
                esc(dest || '-- None --') + '<i class="car"></i></button>')
            : '');

    const danger = '<div class="pd-sect danger"><h2>Danger</h2></div>' +
        stField('Delete this process', deleteGoes(),
            '<button class="pd-btn danger" data-act="st-delete">Delete ' + esc(S.settings.name) + '…</button>');
    // In the grouped page the group heading says Danger, so the card is the
    // one thing it holds.
    const deleteCard = '<div class="pd-sect danger"><h2>Delete this process</h2><span class="pd-dim">' + esc(deleteGoes()) +
        '</span><span class="pd-spacer"></span>' +
        '<button class="pd-btn danger" data-act="st-delete">Delete ' + esc(S.settings.name) + '…</button></div>';

    // WHERE THE ENGINEER WAS SURVIVES THE REDRAW. .pd-sheet is the scroller and
    // drawSettings replaces it wholesale, so every switch flip and auto-arm
    // pick threw the routing set, at the foot of a sheet twice the viewport,
    // back off the top of the screen.
    const shell = settingsShell();
    const body = shell.body;
    const wasAt = shell.sheet.scrollTop || 0;
    shell.top.innerHTML = appbar();
    const notes = (S.settingsError ? '<div class="pd-refusal">' + esc(S.settingsError) + '</div>' : '') +
        (S.settingsNotice ? '<div class="pd-notice">' + esc(S.settingsNotice) + '</div>' : '');
    // A LOADER'S OR UNLOADER'S SETTINGS ARE ITS LINE AND ITS SCREENS, and
    // where the rest is set: Core runs its board (coreBoards). Danger stays,
    // the one way to delete the process.
    if (coreBoards(p)) {
        body.innerHTML = '<div class="pd-stlayout">' + settingsNav(LOADER_ORDER) + '<div class="pd-stcards">' + notes +
            stCard('Line', line) + stCard('Screens and positions', screensSection(), true) +
            stCard('Loader board', '<div class="pd-sect"><h2>Loader board</h2></div>' +
                '<p class="pd-note">Core runs this board: its windows, parts and order are set on Core, under Loaders.</p>') +
            stCard('Danger', danger, false, 'danger') + '</div></div>';
    } else {
        const wired = !!(p.counter_plc_name && p.counter_tag_name);
        const cards = {
            'Health': [healthSection(), true],
            'Line': [line],
            'Parts': [partsSect],
            'Positions and routing': [positionsRoutingSection() + positionFacts(nodes), true],
            'Production counter': [counter, false, wired ? '' : 'warn'],
            'Part identity (CATID)': [identity],
            'Counted somewhere else': [countedCard(nodes), true],
            'Operator setup': [setup, true],
            'Auto-reorder': [autoReorderSection()],
            'Quality hold': [hold],
            'Light curtain': [curtainCard(nodes), true],
            'Danger': [deleteCard, true, 'danger'],
        };
        body.innerHTML = '<div class="pd-stlayout">' + settingsNav(settingsOrder()) + '<div class="pd-stcards">' + notes +
            SETTINGS_GROUPS.map(g => {
                const html = g[1].map(name => (cards[name] ? stCard(name, cards[name][0], cards[name][1], cards[name][2]) : '')).join('');
                return html ? (g[0] ? '<h3 class="pd-stgroup">' + esc(g[0]) + '</h3>' : '') + html : '';
            }).join('') + '</div></div>';
        mountSetPick(ST_SET, 'sp-' + ST_SET, settingsSetSel(), {
            pid: S.processID,
            closed: b => (b !== 'positions' && routingUnread() ? S.routingError || 'the routing set could not be read' : ''),
            note: b => (b === 'staging' ? backPositionsLine() : ''),
            onChange: settingsSetMoved,
        });
        drawScreenLines();
    }

    // A typed value is the draft at once; the shared page marks the bar on the
    // same input, after this listener.
    for (const el of body.querySelectorAll('[data-st]')) {
        if (el.tagName !== 'INPUT') continue;
        el.addEventListener('input', () => { S.settings[el.dataset.st] = el.value; });
    }
    for (const el of body.querySelectorAll('input[data-cn]')) {
        el.addEventListener('input', () => { S.curtain[el.dataset.cn][el.dataset.cf] = el.value; });
    }
    bindPickers();
    bindCombos(body);
    if (wasAt) shell.sheet.scrollTop = wasAt;
    // A draft changed from code (a pick, a switch) is the bar's to mark too.
    shell.page.update();
}

// healthItems is the items Health draws: the process's, less a second counter
// of a stroke the MES slots row already says on its slot's line.
function healthItems() {
    const slotTags = new Set((S.countingSlots || []).map(s => s.tag));
    return (S.catidItems || []).filter(it => !(it.kind === 'counter_slot_lockstep' && it.data && slotTags.has(it.data.tag)));
}

// healthNeed is how many of Health's items need someone now (red or amber).
function healthNeed() {
    return healthItems().filter(it => it.severity === 'red' || it.severity === 'amber').length;
}

// bindCombos wires the curtain's per-node typeahead fields. The list is FILTERED BY
// WHAT IS TYPED - substring, case-insensitive, closest-first, then in the
// options' order - and the typed
// value remains the saved value: picking an option only saves keystrokes, it
// never adds state of its own. Tag options key on the PLC currently typed,
// cached per PLC, so correcting the PLC refetches on the next open. The
// dropdown closes on blur with a grace period so a click on an option (which
// fires before blur) still lands.
function bindCombos(container) {
    for (const wrap of (container || root()).querySelectorAll('[data-combo]')) {
        if (wrap.dataset.bound) continue;
        wrap.dataset.bound = '1';
        const input = wrap.querySelector('input');
        const list = wrap.querySelector('[data-combolist]');
        let opts = null;
        const load = async () => {
            const kind = wrap.dataset.combo;
            if (kind === 'plc') {
                if (!S._plcList) {
                    const res = await send('GET', '/api/plcs');
                    S._plcList = res.ok ? res.body || [] : [];
                }
                return (S._plcList || []).map(p => ({
                    name: p.name, note: p.connected ? 'connected' : 'not connected',
                })).sort((a, b) => a.name.localeCompare(b.name));
            }
            // The PLC is a sibling field's (plcsel: the curtain's own PLC input,
            // the counter's) or a fixed one (plcname: the process's counter PLC).
            const src = wrap.dataset.plcsel ? document.querySelector(wrap.dataset.plcsel) : null;
            const plcName = (wrap.dataset.plcname || (src ? src.value : '') || '').trim();
            if (!plcName) return [{ name: '', note: 'pick a PLC first' }];
            S._tagCache = S._tagCache || {};
            if (!S._tagCache[plcName]) {
                // A failed read is not cached: the next open asks again.
                const res = await send('GET', '/api/plcs/all-tags/' + encodeURIComponent(plcName));
                if (!res.ok) return [{ name: '', note: 'WarLink did not answer — the typed value is kept' }];
                S._tagCache[plcName] = res.body || [];
            }
            const suggest = wrap.dataset.suggest || '';
            return window.TagSuggest ? window.TagSuggest.options(S._tagCache[plcName], suggest)
                : S._tagCache[plcName].map(t => ({ name: t.name, note: t.type || '' }))
                    .sort((a, b) => a.name.localeCompare(b.name));
        };
        const render = items => {
            const q = (input.value || '').trim().toLowerCase();
            // Closest first; among equally close, the options' own order (the
            // suggestions' ranking, else name order where they load).
            const matches = items
                .map((o, i) => ({ o: o, i: i }))
                .filter(x => x.o.name && x.o.name.toLowerCase().includes(q))
                .sort((a, b) =>
                    (a.o.name.toLowerCase().indexOf(q) - b.o.name.toLowerCase().indexOf(q)) || (a.i - b.i))
                .slice(0, 60)
                .map(x => x.o);
            list.innerHTML = matches.length
                ? matches.map(o => '<button data-comboopt="' + esc(o.name) + '">' + esc(o.name) +
                    '<span class="dim">' + esc(o.note) + '</span></button>').join('')
                : '<div class="none">no match - the typed value is kept</div>';
            openLayer(list, { kind: 'popover', anchor: wrap, focus: false });
        };
        input.addEventListener('focus', async () => {
            opts = await load();
            if (opts.length) render(opts);
        });
        input.addEventListener('input', async () => {
            if (!opts) opts = await load();
            if (opts.length) render(opts);
        });
        input.addEventListener('blur', () => setTimeout(() => closeLayer(list), 200));
        // mousedown, not click: it fires before the blur's timeout and so
        // survives the close.
        list.addEventListener('mousedown', ev => {
            const b = ev.target.closest('[data-comboopt]');
            closeLayer(list);
            if (!b || !b.dataset.comboopt) return;
            ev.preventDefault();
            input.value = b.dataset.comboopt;
            // Through the input's own binding, as a keystroke goes.
            input.dispatchEvent(new Event('input', { bubbles: true }));
        });
    }
}

// The save bar alone, so typing a name does not redraw the field under the
// caret and put the cursor back at the start.
async function reloadProcesses() {
    const res = await send('GET', '/api/processes');
    if (!res.ok) return;
    const rows = res.body;
    // A NIL GO SLICE MARSHALS TO null, AND null MEANS EMPTY. Guarding with
    // Array.isArray avoided the crash and made null a no-op instead, so
    // deleting the last process left it on the list.
    S.processes = Array.isArray(rows) ? rows : [];
}

async function reloadStations() {
    const res = await send('GET', '/api/operator-stations');
    if (!res.ok) return;
    const rows = res.body;
    S.stations = Array.isArray(rows) ? rows : [];
}

// The positions one screen claims, re-read from the server rather than assumed
// from what was just sent. S.stationNodes arrives with the page, so without a
// re-read a screen whose positions were set here went on drawing the old list
// until the page was reloaded. Every screen of the current process, so each
// screen's row on Settings draws what the server holds.
async function refreshProcessStationNodes() {
    await Promise.all(stationsOf(S.processID).map(st => refreshStationNodes(st.id)));
}

async function refreshStationNodes(stationID) {
    const res = await send('GET', '/api/operator-stations/' + stationID + '/claimed-nodes');
    if (!res.ok) return;
    const names = res.body;
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
// IT IS AN ENTRY ON THE ONE OVERLAY STACK (shared/layer.js). The two sheets
// that draw their own whole markup here, Advanced and the apply modal, open it
// with layer.js's showModal: Escape closes whatever is innermost, a backdrop
// click is Escape (ruling 2: the Processes sheets opt in), typed work is
// asked about first, the scroll lock is counted, and focus goes in and comes
// back. Every other sheet is layer.js's own shell (pageSheet).
//
// preserveState: the layer's close resets a modal's fields to their markup,
// which is for a static modal that will be shown again. This scrim's content
// is rebuilt from scratch on every open and thrown away on close, so the walk
// would be work done on fields about to stop existing.
//
// ONE OPEN AND ONE CLOSE. There were four of the first and three of the
// second — Advanced, the sheets and the apply modal each did their
// own `scrim.hidden = false`, so a fix to what opening means had four places
// to land in and reached whichever the author remembered. showSheet returns
// the scrim's layer handle; opening it again (a redraw) returns the same one.
// onClose runs however it closed (Cancel, Escape, the backdrop, Discard).
function showSheet(html, onClose) {
    const scrim = $('pd-scrim');
    if (!scrim) return null;
    if (html !== undefined) scrim.innerHTML = html;
    return showModal(scrim, { closeOnBackdrop: true, preserveState: true, onClose: onClose });
}


// sheetEl is the element the open sheet lives in: its layer handle's, or
// #pd-scrim. Every read of a sheet's fields goes through it.
function sheetEl() {
    return S.sheet && S.sheet.handle ? S.sheet.handle.el : $('pd-scrim');
}

// ── the node picker ──────────────────────────────────────────────────────────
//
// NODES ARE PICKED FROM LISTS, BY NAME (owner ruling 2026-09-16). An engineer
// identifies a node by its name — SMN_029, PLK_H1 — because that is how the
// fleet, the PLC and the floor refer to it. Finding that name as a dot on a
// 383-point plant is the wrong way round, and it is why the routing set was
// hard to author. Every place this page asks for nodes is this one component.
//
// ONE COMPONENT, EVERY HOST: the Add-process sheet's four lists, the operator
// screen's positions, and Settings' three role lists. Each is the shared node
// adapter (shared/node-picker.js) over the page's one node list, with the
// field's own rule; the part set is the part adapter over the one part list.
// The shell answers its own clicks, in a sheet and on the page alike.
//
// EXCLUDED NAMES ARE SHOWN, DIMMED, WITH THE REASON. Hiding them makes the
// engineer think the node does not exist, and the reason — "already a position
// of this press" — is the sentence they actually need.

// THE PAGE'S ONE NODE LIST AND ONE PART LIST, each read once a page load and
// asked again after a failure (a failed read is not an empty plant, and is not
// kept as one: the pickers say it and re-ask on their next open). S.coreNodes
// and S.payloadCatalog are the rows as the Edge sent them, for the rules and
// the page's other readers; they stay null until a read lands.
const NODES = nodeSource(() => send('GET', '/api/core-nodes'));
const PARTS = partSource(() => send('GET', '/api/payload-catalog'));
NODES.watch(() => { const r = NODES.rows(); S.coreNodes = r ? r.map(n => n.raw) : null; });
PARTS.watch(() => { const r = PARTS.rows(); S.payloadCatalog = r ? r.map(c => c.raw) : null; });

// loadCoreNodes resolves once the node list is read (or its read failed: it
// never rejects, and a failure leaves S.coreNodes null). Four pickers opening
// in one sheet share the one request.
function loadCoreNodes() {
    return NODES.load();
}

// pickerInit declares one picker before it is drawn: its rules, which the
// shared adapter draws and runs (drawPicker).
//
//   selected  the names it opens with
//   exclude   name -> reason, or '' — a name that may not be picked, and why:
//             the field's rule, drawn greyed with the reason, never reached by
//             click or key
//   annotate  name -> note, or '' — a name that MAY be picked and comes with a
//             consequence. Drawn beside it, because the difference between
//             "you cannot" and "you can, and here is what happens" is the
//             whole of it
//   onPick    given, the picker WRITES THROUGH (picker.js onPick): a click
//             calls this instead of moving the selection, the row shows
//             pending and the chips stay where they are until it answers; then
//             they are the selection the page re-read from wherever it really
//             lives. That is Settings' three routing lists and the part set,
//             whose truth is the server. Without it the picker accumulates,
//             which is every sheet.
//   onDrop    the same for a chip's ×, or a picked row clicked again. A
//             write-through picker needs both or its two halves disagree about
//             where the selection lives.
//   onChange  after the selection moved, for a picker whose choice changes
//             what another picker may offer.
//   source    'core' (default) or 'payloads' — which list the rows are drawn
//             from. A part is not a place, and the part set's picker offers
//             the payload catalog rather than Core's node list
//   checklist the list is always open, one box per node, with the name
//             prefixes (PLN, SMN…) as filters over it — see checklistFilters
//   only      raw row -> whether the list draws it at all (default for a
//             checklist: no group, each node its own box); read on every
//             draw, so a host may change it (a set picker's bucket)
//   tags      name -> [{label, tone}], marks drawn after the name: what else
//             the node already is
//   single    one value, not a set (pickerValue is still a list, of one)
//   allowed   a part picker's codes, in order: all it offers
//   add       the shell's "Add and use" slot, {label(q), run(q)}
//   placeholder  the search box's hint, when the default is not the words
// How many chips the part set shows before "+ N more · show all".
const PART_FOLD = 12;

function pickerInit(key, opts) {
    const o = opts || {};
    dropPicker(key);
    S.pickers[key] = {
        sel: (o.selected || []).slice(),
        q: '',
        source: o.source || 'core',
        exclude: o.exclude || (() => ''),
        annotate: o.annotate || (() => ''),
        onPick: o.onPick || null,
        onDrop: o.onDrop || null,
        onChange: o.onChange || null,
        checklist: !!o.checklist,
        only: o.only || null,
        tags: o.tags || null,
        single: !!o.single,
        allowed: o.allowed || null,
        add: o.add || null,
        fold: o.fold || 0,
        placeholder: o.placeholder || '',
        filters: 0, // the prefixes its checklist was drawn with
        ui: null,   // the adapter's picker drawn for it, while its host is in the page
    };
}

// dropPicker forgets a picker, its drawn shell first: a list left open on the
// layer, and the shell's watch on the node list, go with it.
function dropPicker(key) {
    const p = S.pickers[key];
    if (p && p.ui) p.ui.destroy();
    delete S.pickers[key];
}

function pickerValue(key) {
    const p = S.pickers[key];
    return p ? p.sel.slice() : [];
}

// pickerBox is where a picker is drawn: an empty host the adapter fills when
// the markup it is in lands (bindPickers). THE SEARCH BOX IS THE SHELL'S, and
// it is never redrawn under the caret (picker.js).
function pickerBox(key) {
    const p = S.pickers[key];
    return '<div class="pd-npk' + (p && p.checklist ? ' check' : '') + '" data-npkbox="' + key + '" id="npk-' + key + '"></div>';
}

// The same component inside a sheet's field grid, so a picker sits under a
// label like every other control on the sheet.
function pickerField(key, label, sub) {
    return '<div class="pd-fld"><label>' + esc(label) +
        (sub ? '<small>' + esc(sub) + '</small>' : '') + '</label>' +
        '<div class="v">' + pickerBox(key) + '</div></div>';
}

// THE PART SET'S OPTIONS COME FROM THE PAYLOAD CATALOG, not from Core's node
// list. Same shell, same chips, same search field — a different universe,
// because a part is not a place. The catalog is Core's too and shingo mirrors
// it the same way.
function payloadCatalogList() {
    return (S.payloadCatalog || []).slice().sort((a, b) =>
        String(a.code || a.name || '').localeCompare(String(b.code || b.name || '')));
}

// ── the checklist: the same picker, one box per node ─────────────────────────
//
// For positions, which an engineer adds a handful at a time from one family
// of names (PLN_006BCD…NOP). The list stays open, the prefixes Core's names
// carry are filters over it, and every node is its own box.
//
// ONE BOX, ONE NODE. A group is not drawn here: as a position it is its
// members, and a box that ticks a dozen nodes at once is not one the engineer
// looked at. Nor is there a tick-all: each position added is one chosen.

// isGroupRow is a row of Core's list that stands for other nodes: a group with
// members. It is outside the checklist's universe (the adapter's only()).
function isGroupRow(raw) {
    return !!(raw && Array.isArray(raw.members) && raw.members.length);
}

// nodePrefix is the family a node's name opens with — PLN of PLN_006BCD — or
// '' for a name with none.
function nodePrefix(name) {
    const m = /^([A-Za-z]+)_/.exec(String(name || ''));
    return m ? m[1].toUpperCase() : '';
}

// checklistFilters is one filter per family in the checklist's universe. A
// prefix only FILTERS: it puts `PLN_` in the search box, and a second press
// clears it.
function checklistFilters() {
    const seen = [];
    for (const n of S.coreNodes || []) {
        if (isGroupRow(n)) continue;
        const pre = nodePrefix(n.name);
        if (pre && seen.indexOf(pre) < 0) seen.push(pre);
    }
    return seen.sort().map(pre => ({ label: pre, query: pre + '_' }));
}

// loadPayloadCatalog reads Core's mirrored catalog once per page, the way
// loadCoreNodes reads the node list, and never rejects.
function loadPayloadCatalog() {
    return PARTS.load();
}

// drawPicker draws a declared picker into its host, once per host: the node
// adapter over the page's node list with the field's own rule and notes, or
// the part adapter over its part list. ONE LIST OPEN AT A TIME is the shell's
// (a dropdown list is a layer popover); a checklist keeps its list up. A
// write-through picker's answer is the selection as the page re-read it: a
// refusal leaves it where it was, and the page says why where it says every
// refusal (the Settings refusal line).
function drawPicker(key) {
    const p = S.pickers[key];
    const host = $('npk-' + key);
    if (!p || !host) return;
    if (p.ui && p.ui.el.parentNode === host) return;
    if (p.ui) p.ui.destroy();
    const parts = p.source === 'payloads';
    const shell = {
        multi: !p.single,
        open: p.checklist,
        selected: p.sel,
        placeholder: p.placeholder || (parts ? 'find a part by name' : p.checklist ? 'filter by name, or pick a prefix below' : 'find a node by name'),
        filters: p.checklist ? checklistFilters() : null,
        add: p.add || undefined,
        fold: p.fold || 0,
        onChange: v => { p.sel = Array.isArray(v) ? v.slice() : (v ? [v] : []); if (p.onChange) p.onChange(); },
        onPick: p.onPick ? (next, row) => {
            const had = p.sel.indexOf(row.value) >= 0;
            const write = had ? (p.onDrop ? p.onDrop(row.value) : null) : p.onPick(row.value);
            return Promise.resolve(write).then(() => (S.pickers[key] || p).sel.slice());
        } : undefined,
    };
    p.ui = parts
        ? partPicker(host, PARTS, { allowed: p.allowed || undefined, note: c => p.annotate(c.code) }, shell)
        : nodePicker(host, NODES, {
            rule: n => p.exclude(n.name),
            note: n => p.annotate(n.name),
            tags: p.tags ? n => p.tags(n.name) : undefined,
            only: p.only ? n => p.only(n.raw) : p.checklist ? n => !isGroupRow(n.raw) : undefined,
        }, shell);
    p.filters = shell.filters ? shell.filters.length : 0;
    // The query it had (a prefix the page opened it on) stays its query: a
    // prefix is pressed as its filter, so the filter reads on; anything else
    // is typed.
    if (p.q) {
        const f = (shell.filters || []).find(x => x.query === p.q);
        const b = f && p.ui.el.querySelector('[data-query="' + f.query + '"]');
        if (b) b.click();
        else { p.ui.input.value = p.q; p.ui.input.dispatchEvent(new Event('input')); }
    }
    p.ui.input.addEventListener('input', () => { p.q = p.ui.input.value; });
}

// redrawPicker draws a picker again from the page's state: the selection the
// page holds, and its rows (an exclusion that a read just made true). clean:
// that selection is the new starting point (another screen's list), not work.
function redrawPicker(key, clean) {
    const p = S.pickers[key];
    if (!p || !p.ui) return;
    p.ui.set(p.sel, clean ? { clean: true } : undefined);
}

// Every declared picker whose host is in the document, drawn. BOTH LISTS,
// EACH READ ONCE: a page can carry pickers of both kinds (Settings carries a
// node picker per screen and role and the part set's), and a picker drawn
// before its list lands says so and fills itself when it does. A checklist
// drawn before the node list is drawn again then, its filters being the
// list's prefixes. NO PICKER, NO READ: Add process on the list asks a name
// and a group, and read Core's node list for nothing.
function bindPickers() {
    const keys = Object.keys(S.pickers);
    for (const key of keys) drawPicker(key);
    if (!keys.length) return;
    loadCoreNodes().then(() => {
        for (const key of Object.keys(S.pickers)) {
            const p = S.pickers[key];
            if (!p.checklist || !p.ui || p.filters || !S.coreNodes) continue;
            p.ui.destroy();
            p.ui = null;
            drawPicker(key);
        }
    });
    if (Object.keys(S.pickers).some(k => S.pickers[k].source === 'payloads')) loadPayloadCatalog();
}

// ── the sheets D4 and D5 open ────────────────────────────────────────────────
//
// ONE SHELL, THE LAYER'S (shared/layer.js openSheet, which is this page's
// sheet shell moved): a card with a title, a body of labelled fields, a status
// line, Cancel and OK. The card keeps .pd-modal, so every sheet is the one
// width: .pd-modal is sized in CSS from the label column and the wide input
// that sheetField draws, so every field fits every sheet and no sheet picks a
// width of its own.
//
// WHAT THE LAYER ANSWERS FOR, and this page no longer does: one run per press
// (OK is off while the run is out and back on in a finally, whatever the run
// did, so a chain refused halfway is pressed again and a write that never
// answers cannot hold "Saving…"); Escape innermost first, a popover before
// its sheet; a backdrop click as Escape (ruling 2: these sheets opt in, and a
// press that began in the card never closes it); asking before Escape or the
// backdrop throws typed work away, the question being a confirm on the stack
// whose Escape is Keep editing (and the sheet under it is never redrawn, so
// keeping is keeping); the counted scroll lock; focus into the sheet and back
// to what opened it.
//
// ONE SHEET AT A TIME, as the one scrim had it: a sheet opened over another
// takes its place ('replaced') rather than stacking on it. opener, when given,
// is where focus returns on close (a sheet a menu row opened: the menu's
// trigger, since the row is gone).
//
// #pd-advpop RIDES IN THE BODY rather than on the sheets that want a popover.
// It answers #pd-pop living inside #pd-root, which the scrim covers; a list opened from a sheet against the page's popover
// draws behind the modal. One per sheet — and the sheets that never open one
// pay an empty div.
// wide: the process sheet, whose set pick draws its four boxes side by side.
function pageSheet(title, sub, body, confirm, run, danger, opener, wide) {
    if (S.sheet && S.sheet.handle) S.sheet.handle.close('replaced');
    const h = openSheet({
        title: title, sub: sub, ok: confirm, danger: !!danger, opener: opener || undefined,
        body: body + '<div class="pd-pop" id="pd-advpop" hidden></div>',
        cardClass: 'layer-card layer-sheet pd-modal' + (wide ? ' pd-wide' : ''),
        backdrop: true,
        run: () => run(),
        onClose: why => sheetGone(h, why),
    });
    // The sheet's own controls (pickers, choosers, its popover's rows) answer
    // in onScrimClick, as they did on #pd-scrim: the sheet is outside #pd-root.
    h.el.addEventListener('click', onScrimClick);
    S.sheet = { handle: h };
    bindPickers();
    return h;
}

// sheetStatus is the sheet's one line of feedback, and every refusal on this
// page reaches the engineer through it. It was written out by hand at three
// call sites; a fourth (the Add-process chain, which reports a refusal midway
// through a sequence of writes) made that a fourth copy. It is the layer's.
function sheetStatus(text, bad) {
    if (S.sheet && S.sheet.handle) S.sheet.handle.status(text, bad);
}

// runSheet is OK on a sheet that draws its own footer (the apply modal, on
// #pd-scrim): its run goes through the layer's one run per press too.
function runSheet() {
    const s = S.sheet;
    if (s && s.handle && s.run) return s.handle.run(() => s.run());
    return undefined;
}

// closeSheet is a deliberate close (Cancel, a write that landed): it never
// asks. The clearing is sheetGone's, which runs however a sheet closed.
function closeSheet() {
    if (S.sheet && S.sheet.handle) S.sheet.handle.close('close');
    else sheetGone(null, 'close');
}

// ── a chain of writes behind one OK (ruling 5) ───────────────────────────────
//
// A SHEET WHOSE OK IS SEVERAL WRITES (Add process, Add operator screen, a new
// station counter bound to its position, New style's mint and CATID) used to
// run them all again on the next press after a refusal halfway: a second
// process, screen, counter or mint. The sheet now keeps what its chain made
// (chainMade) and the next press resumes at the step that failed. That lives
// only while the sheet is open. Closing it deletes nothing: what was made
// stays, and the read the chain would have ended with (chainHalf) runs then,
// so it is on the page. No new endpoint and no new request on the way.
function chainMade() {
    if (!S.sheet) return {};
    return S.sheet.made || (S.sheet.made = {});
}
function chainHalf(readBack) { if (S.sheet) S.sheet.half = readBack; }
function chainDone() { if (S.sheet) S.sheet.half = null; }

function sheetGone(h, why) {
    if (h && S.sheet && S.sheet.handle !== h) return;   // a sheet already replaced
    const half = S.sheet && S.sheet.half;
    S.sheet = null;
    if (half) void half();
    if (h && h.el === $('pd-scrim')) h.el.innerHTML = '';
    // A sheet another took the place of leaves its page state to the one
    // that replaced it, as the scrim's overwrite always did.
    if (why === 'replaced') return;
    // A PICKER'S STATE BELONGS TO THE RENDER THAT DREW IT. The sheet is gone
    // now, so every picker whose box went with it is dead state — and a dead
    // picker with a selection is what would put last time's nodes in front of
    // the engineer when this sheet is opened again. Settings' own pickers are
    // in #pd-root and stay.
    for (const k of Object.keys(S.pickers)) if (!$('npk-' + k)) dropPicker(k);
    // So is the process sheet's set pick; Settings' is kept across tabs.
    dropSetPick(PROC_SET);
    // The apply modal's ticks and previews go with it: reopening it must start
    // from nothing ticked (explicit re-apply), not from what was ticked last
    // time and previewed against a flow that has since been saved.
    S.papply = null;
    // So do New style's spots: the next sheet opens on the recipe's pre-fill.
    if (S.newStyle) { S.newStyle = null; markNewStyle(); }
}

// ── Escape with nothing open ─────────────────────────────────────────────────
//
// ESCAPE IS THE LAYER'S while anything is open on it: a popover, a sheet,
// Advanced, the question. Innermost first, one per press. What is left for
// the page is the outermost thing Escape can let go of, with nothing open
// (layerDepth() is 0): a recipe group's row or tag pinned on the picture, else
// the selected position.
function onEscape() {
    if (S.shapeDraft && S.shapeDraft.focus && S.tab === 'flows') { S.shapeDraft.focus = null; drawFlows(); return; }
    if (S.selected && S.tab === 'flows') clearSelected();
}

function sheetValue(name) {
    // A PICKER IS READ BY THE SAME ACCESSOR AS A TEXT FIELD. It is the sheet's
    // value for that name, and a second read path would be a second answer to
    // "what did the engineer put in this field" — which is how a sheet ends up
    // writing one thing and reporting another.
    if (S.pickers[name]) return pickerValue(name);
    const el = sheetEl().querySelector('[data-f="' + name + '"]');
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
// same three lines — "PLN_02 is used by PART SYN-A-S003" is an answer and
// "could not save" is not — and the Add-process chain wants the answer as well,
// because step 2 is posted against the id step 1 returned. That read is the
// door's (send): postJSON's own took a lapsed session's login page as a
// success with an empty body (P0 bug, :6056), so Add process went on to write
// /api/processes/NaN/…, and let a dead Edge's rejection go uncaught (:6049).
// The door answers 401 "Signed out" and status 0, and every chain on this page
// stops at the first answer that is not ok (ruling 3: no write proceeds on a
// 401). bodyOf is the body a caller reads an id or a warning from: {} for a
// write that answered with nothing, as postJSON's was.
function bodyOf(out) {
    return out.body && typeof out.body === 'object' ? out.body : {};
}

// said is what the page draws in place for a refused answer: the door's
// sentence after lead, or nothing when the door's notice showed it (shown).
// A lapsed session's 401 always goes on the notice, which alone carries Sign
// in (the wave C ruling): it is said once, there, and the sheet that was
// refused stays open as it was.
function said(out, lead) {
    return out.shown ? '' : (lead || '') + out.error;
}

// A write that goes through a sheet, with the server's refusal shown BY NAME
// rather than swallowed into "could not save".
async function sheetSubmit(method, url, body, after) {
    const out = await send(method, url, body, NO_NOTICE);
    if (!out.ok) { sheetStatus(said(out), true); return false; }
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
    showPop(pop, btn);
    placePopover(pop, btn);
    // The ⋯ the menu hangs off: a sheet a row opens returns focus there, the
    // row itself being gone by the time it closes.
    S.menuFrom = btn;
}

// What a blank Expected CATID means for a style: its produced parts' catalog
// CATIDs, or — for a style with no produce claim — none at all, until one is
// confirmed from a changeover (the health strip's one-click, or the
// post-cutover flag's Confirm).
function catidBlankHint(id) {
    const cs = composerStyle(id);
    const produces = !cs || (cs.claims || []).some(c => c.role === 'produce');
    return produces ? 'blank means the CATIDs come from its parts in the catalog'
        : 'blank — no part IDs; confirm one from a changeover';
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

// writeStyle is the rename's and the CATID's one write. The style it starts
// from is the page's /styles row: the composer block carries no description,
// so a style the list lacks (its last read failed while the composer's did
// not) is read first, and when it still cannot be found nothing is written —
// a body built from the composer's row, or styleByID's last resort, blanked
// the description, and the last resort the name (LEAD-NOTES B2).
async function writeStyle(id, change) {
    const st = await styleListRow(id, res => sheetStatus(res.ok ? 'This style is not on the Edge any more. Nothing was written.'
        : said(res, 'This style could not be read, so nothing was written: '), true));
    if (!st) return false;
    return sheetSubmit('PUT', '/api/styles/' + id, styleBody(st, change), refreshProcess);
}

// styleListRow is the page's /styles row for style id, read when the list
// lacks it, or null (missing(res) hears why: res is the read's answer).
async function styleListRow(id, missing) {
    let st = S.styles.find(s => s.id === id);
    if (st) return st;
    const res = await send('GET', '/api/processes/' + S.processID + '/styles', undefined, NO_NOTICE);
    if (res.ok && Array.isArray(res.body)) S.styles = S.styles.filter(s => s.process_id !== S.processID).concat(res.body);
    st = S.styles.find(s => s.id === id);
    if (!st && missing) missing(res);
    return st || null;
}

function styleAction(act, id) {
    const st = styleByID(id);
    const from = S.menuFrom;
    closePop();
    switch (act) {
        case 'rename':
            pageSheet('Rename ' + st.name, '', sheetField('Name', 'what this part is called here', 'name', st.name),
                'Rename', () => writeStyle(id, { name: sheetValue('name') }), false, from);
            return;
        case 'catid':
            pageSheet('Expected CATID · ' + st.name,
                'what the PLC calls this part. A confirmed change to it is what arms a changeover cutover.',
                sheetField('Expected CATID', catidBlankHint(id), 'catid', st.expected_catid),
                'Save', () => writeStyle(id, { expected_catid: sheetValue('catid') }), false, from);
            return;
        case 'clone':
            pageSheet('Clone ' + st.name, 'a new style with the same flow, ready to be renamed.',
                sheetField('Name', '', 'name', st.name + ' copy'),
                'Clone', () => {
                    const name = sheetValue('name');
                    return sheetSubmit('POST', '/api/styles/' + id + '/clone', B().styleClone(name), () => {
                        S.notice = 'Saved ' + name + ', a copy of ' + st.name + '.';
                        return refreshProcess();
                    });
                }, false, from);
            return;
        // R4: "Set Active" is now "Mark as running", and the sentence says
        // exactly what it does — because the old verb read like an instruction
        // to the cell, and this one is an admin correction to shingo's record.
        case 'running':
            openRunningSheet(st, id, from);
            return;
        case 'delete':
            pageSheet('Delete ' + st.name + '?', 'its flow goes with it.',
                '<p class="pd-note">A style the process has run is part of its history. Deleting it removes ' +
                'the flow an operator would pick, not what already ran.</p>',
                'Delete', () => sheetSubmit('DELETE', '/api/styles/' + id, null, refreshProcess), true, from);
            return;
        default: return;
    }
}

// "Mark as running" is the admin's style switch. With a changeover open it
// completes that changeover now — to its target, then to this style when this
// is a third — past whatever the cutover still waits on, and leaves every robot
// already moving to finish its trip. The sheet names what is in flight first.
async function openRunningSheet(st, id, from) {
    const p = process();
    let inFlight = '';
    if (p && p.target_style_id && p.target_style_id !== p.active_style_id) {
        const target = styleByID(p.target_style_id);
        let waiting = [];
        // A failed read still opens the sheet saying a changeover is open.
        const res = await send('GET', '/api/processes/' + S.processID + '/changeover/gate-status');
        if (res.ok) waiting = ((res.body || {}).blockers || []).map(b => b.reason);
        const how = id === p.target_style_id ? 'completes it now'
            : 'completes it to ' + target.name + ' and then switches to ' + st.name;
        // The style the changeover leaves is the server's refusal, not a
        // switch: staying on it is the changeover's Cancel.
        inFlight = id === p.active_style_id
            ? '<p class="pd-note">A changeover to ' + esc(target.name) + ' is open, leaving ' + esc(st.name) +
              '. Marking ' + esc(st.name) + ' as running is refused: use Cancel on the changeover to stay on it.</p>'
            : '<p class="pd-note">A changeover to ' + esc(target.name) + ' is open. Marking ' + esc(st.name) +
              ' as running ' + esc(how) + (waiting.length ? ', past: ' + esc(waiting.join('; ')) +
              '. Robots already moving finish their trips' : '') + '.</p>';
    }
    pageSheet('Mark ' + st.name + ' as running?',
        "Robots may move within a minute for the new style's levels and kept spots.",
        inFlight + '<p class="pd-note">Use this when the plant changed parts without shingo. To change ' +
        'parts WITH shingo, start a changeover from the operator screen.</p>',
        'Mark as running', () => sheetSubmit('PUT', '/api/processes/' + S.processID + '/active-style',
            B().processActiveStyle(id), refreshProcess), false, from);
}

// After any write that changes what a screen shows: re-read the process list
// and the composer block, then redraw the tab the engineer is on. One path, so
// no screen can be left showing what it read before the write.
async function refreshProcess() {
    // SETTINGS KEEPS WHAT IS NOT SAVED YET over the re-read: a screen renamed
    // in its sheet, a loader given a screen or a recipe applied is no reason
    // to drop a pick or a typed value the engineer has not saved.
    const kept = {};
    if (S.tab === 'settings' && S.stShell) {
        for (const n of S.stShell.page.dirtySections()) kept[n] = SETTINGS_SECTIONS[n].read();
    }
    await reloadProcesses();
    const res = await send('GET', composerURL(S.processID));
    if (res.ok) {
        takeComposer(res.body);
        if (S.tab !== 'flows') S.flowStale = true;
    }
    const styles = await send('GET', '/api/processes/' + S.processID + '/styles');
    if (styles.ok) {
        const rows = styles.body;
        // null is this process having no styles left — which is exactly the
        // state deleting the last one produces, and the state in which the old
        // guard kept it on screen.
        S.styles = S.styles.filter(s => s.process_id !== S.processID)
            .concat(Array.isArray(rows) ? rows : []);
    }
    if (S.tab !== 'settings') S.settings = settingsDraft();
    // A RECIPE DRAFT STAYS (F4): a write from the rail's ⋯ (a rename, a
    // clone) is no way out of it, and re-selecting a style dropped it unasked.
    if (S.tab === 'flows' && S.shapeDraft) drawFlows();
    else if (S.tab === 'flows') selectStyle(composerStyle(S.styleID) ? S.styleID : ((S.composer.styles[0] || {}).id || 0));
    else if (S.tab === 'setup') drawSetup();
    else await openSettings(kept);
}

// rebuildStaleFlow rebuilds the open style's flow from the composer block
// refreshProcess re-read while the page was on another tab.
//
// THE FLOW IS BUILT WHEN THE PROCESS OPENS, whatever tab it lands on, and the
// Styles tab only redraws it. So a part set, positions or routing set changed
// through Setup's Change… reached the Styles tab as the page first read them:
// the part popover said "No parts yet" over Core's whole catalog (owner,
// 2026-11-12). On Styles itself refreshProcess re-selects the style instead.
//
// ON THE WAY BACK TO STYLES, not at the write: drawFlows is every door onto
// the tab, and a rebuild at the write would preview a flow nobody is looking
// at after every Settings save. An edited draft is kept, with only its part
// list brought up to date.
function rebuildStaleFlow() {
    S.flowStale = false;
    if (!S.model || S.shapeDraft) return;
    if (dirty()) { S.model.palette = S.composer.palette.slice(); return; }
    const st = composerStyle(S.styleID);
    if (!st) return;
    S.selected = null;
    S.model = initModel(st);
    S.baseline = JSON.stringify(M().toCells(S.model));
    schedulePreview();
}

// ── the rail's and the list's own creates ────────────────────────────────────

// "+ New style": a part this press runs, with the CATID the PLC calls it.
//
// NEW PART FLOW IS PICK A RECIPE, FILL EVERY SLOT (pick-a-part U3). The style
// is minted server-side (POST /flow/mint, the floor's own door): the recipe's
// cells with these parts on their slots, named per R-D, the recipe stamped as
// provenance, no reorder until somebody sets one. The desktop names a node per
// part; the station names none and the server places them.
//
// A TYPED NAME STAYS POSSIBLE for an engineer's one-off — "— none, I'll draw
// it —" is the old sheet: a named style with no flow, drawn next on this
// screen.
//
// THE SPOTS STEP COMES AFTER THE PARTS (F6, SYNTH-round2 row 24): the same
// relation picture and the same spotsTap / spotsLit the station's Spots step
// runs, over the same card, opened where the cell last ran the recipe
// (last_moves). The mint sends the moves as overrides — the station's own
// mintBody bytes, with the node the desktop names for each part.
//
// A STYLE FROM A RECIPE IS MADE AT A SCREEN: flow/mint is the station's door
// and refuses a process with none. With no screen the sheet says so, with the
// way to one, where the recipe chooser would be; drawing by hand still works
// (flow/save takes no screen).
function openNewStyle(recipeID, sel) {
    const presets = S.composer.presets || [];
    const palette = S.composer.palette || [];
    const atScreen = stationsOf(S.processID).length > 0;
    const recipe = atScreen ? presets.find(p => String(p.id) === String(recipeID || '')) : null;
    S.newStyle = null;
    // THE NAME IS A PART OF THIS PROCESS'S PART SET (HOP, 2026-11-12). A style
    // is a part, and the part set is what this process runs, so with no recipe
    // the name is picked from it, as a recipe's slots are; a typed one-off name
    // is still taken, through the picker's Add slot. What Create reads is the
    // hidden name field the picker keeps in step, so the request is the one a
    // typed name always made. An empty part set says so and goes to Parts.
    pickerInit('newname', {
        source: 'payloads', single: true, allowed: palette,
        placeholder: palette.length ? 'pick a part, or type a one-off name' : 'type a name',
        add: { label: q => 'Use “' + q + '” as a one-off name', run: q => ({ value: q, label: q }) },
        onChange: () => {
            const el = sheetEl().querySelector('[data-f="name"]');
            if (el) el.value = pickerValue('newname')[0] || '';
        },
    });
    const nameField = '<div class="pd-fld"><label>Name<small>the part it runs, from this process’s part set · ' +
        'or type a one-off name</small></label><div class="v">' + pickerBox('newname') +
        '<input type="hidden" data-f="name" value="">' +
        (palette.length ? '' : '<p class="pd-note">This process has no parts yet. ' +
            '<button class="pd-dimlink" data-act="newstyle-parts">Add them in Settings › Parts</button></p>') +
        '</div></div>';
    // STYLE FIRST (F2): the sheet opens on "draw it" — a name, then the flow
    // on the Styles tab. "Start from a recipe" is there only when the process
    // has one to start from.
    pageSheet('New style', 'a part this process runs: name it, then draw its flow.',
        (!presets.length ? '' : atScreen
            ? '<div class="pd-fld"><label>Start from a recipe<small>optional: its spots, with a part on each</small></label>' +
            '<div class="v"><button class="pd-sel wide" data-act="newstyle-recipe" id="pd-newrecipe">' +
            esc(recipe ? recipe.name : NO_RECIPE) + '<i class="car"></i></button>' +
            '<input type="hidden" data-f="recipe" value="' + esc(recipe ? recipe.id : '') + '"></div></div>'
            : '<p class="pd-note">A style from a recipe is made at a screen, and this process has none. ' +
            '<button class="pd-dimlink" data-act="newstyle-screens">Add one in Settings › Positions and routing</button>' +
            ', or draw this style by hand.</p>') +
        '<div id="pd-newslots"></div>' +
        '<div id="pd-newspots"></div>' +
        '<div id="pd-newname">' + nameField + '</div>' +
        sheetField('Expected CATID', 'what the PLC calls it · blank: from the catalog', 'catid', '') +
        '<p class="pd-note">Set it only when the catalog’s is wrong; the style’s ⋯ menu changes it later.</p>',
        'Create', async () => {
            const recipe = sheetValue('recipe');
            if (recipe) { await mintNewStyle(recipe); return; }
            const name = String(sheetValue('name') || '').trim();
            if (!name) { sheetStatus('A part needs a name, or pick a recipe.', true); return; }
            const made = await send('POST', '/api/styles',
                B().styleCreate(name, S.processID, sheetValue('catid')), NO_NOTICE);
            if (!made.ok) { sheetStatus(said(made), true); return; }
            closeSheet();
            // Selected, because the reason to make one is to draw its flow.
            // refreshProcess keeps the id when the re-read still has it.
            S.styleID = Number(bodyOf(made).id) || S.styleID;
            await refreshProcess();
        }, false);
    if (recipe) {
        pickNewRecipe(String(recipe.id));
        if (sel && S.newStyle) {
            const card = recipeCard(S.newStyle.recipe);
            if ((card.spots || []).some(s => s.node === sel)) { S.newStyle.sel = sel; drawNewSpots(); } else tapNewSpot(sel);
        }
    }
}

const NO_RECIPE = 'none, draw it';

// openNewRecipePick is New style's "Start from a recipe", on the shared
// picker: none, or one of the cell's recipes, said with where it runs.
function openNewRecipePick(btn) {
    const pop = $('pd-advpop');
    if (!pop) return;
    const rows = [{ value: '-', label: NO_RECIPE }].concat((S.composer.presets || []).map(p =>
        ({ value: String(p.id), label: p.name, note: p.where || '' })));
    openRowPicker(pop, btn, rows, v => pickNewRecipe(v === '-' ? '' : v),
        { selected: String(sheetValue('recipe') || '-'), placeholder: 'find a recipe' });
}

// pickNewRecipe sets the sheet to start from recipe id ('' for none): its
// button and field, a part picker per slot, the name field hidden (a recipe
// names the style, R-D), and the Spots step.
function pickNewRecipe(id) {
    const p = (S.composer.presets || []).find(x => String(x.id) === String(id));
    const field = sheetEl().querySelector('[data-f="recipe"]');
    if (field) field.value = p ? String(p.id) : '';
    const btn = $('pd-newrecipe');
    if (btn) btn.innerHTML = esc(p ? p.name : NO_RECIPE) + '<i class="car"></i>';
    const slots = $('pd-newslots');
    if (slots) { slots.innerHTML = newSlotsHTML(p); bindPickers(); }
    const name = $('pd-newname');
    if (name) name.hidden = !!p;
    startNewSpots(p ? String(p.id) : '');
}

// newSlotsHTML is a recipe's slots, each a part picker over the process's part
// set (the one Name's is); the hidden field beside it is what Create reads.
// A GROUPED RECIPE ASKS FOR EACH PART BY ITS GROUP'S NAME, as the floor does
// (SYNTH row 11).
function newSlotsHTML(p) {
    for (const k of Object.keys(S.pickers)) if (/^nslot\d+$/.test(k)) dropPicker(k);
    if (!p) return '';
    const palette = S.composer.palette || [];
    const group = s => (p.groups || []).find(g => g.of === s.nodes[0]);
    return (p.slots || []).map((s, i) => {
        const key = 'nslot' + i;
        pickerInit(key, {
            source: 'payloads', single: true, allowed: palette, placeholder: 'pick a part',
            onChange: () => {
                const el = sheetEl().querySelector('[data-f="slot' + i + '"]');
                if (el) el.value = pickerValue(key)[0] || '';
            },
        });
        return '<div class="pd-fld"><label>' + esc(group(s) ? group(s).name + ' part' : s.nodes.join(' / ')) +
            '<small>' + esc(group(s) ? s.nodes.join(' / ') + ' · ' + s.role : s.role) + '</small></label>' +
            '<div class="v">' + pickerBox(key) +
            '<input type="hidden" data-f="slot' + i + '" data-node="' + esc(s.nodes[0]) + '" value=""></div></div>';
    }).join('') +
        (palette.length ? '' : '<p class="pd-note">This process has no parts yet — add them in Settings › ' +
            'Parts this process runs.</p>');
}

// The frame a sheet's picture is drawn into: the sheet's 732 less its body's
// padding, its border and the scrollbar gutter (the sum
// processes_desktop_sheet_width_test.go holds). A grid wider than that
// scrolls sideways, as the relation picture does in every frame.
const SHEET_PIC_W = 664;

function recipeCard(id) {
    return (S.composer.presets || []).find(x => String(x.id) === String(id)) || null;
}

// recipePicture draws a recipe's shape where the moves put it, on its own
// station's cut of the cell, by relation (SYNTH row 18 for the HMI's Spots
// step: every place's node full, and what the selection may move to; the
// group's tag on its spots). Its grid is held by the card as it opened — the
// recipe's cells moved by last_moves, widened by a grouped card's `at` at
// every stand — so a tap never re-flows the cards under the pointer (D11).
function recipePicture(card, moves, o) {
    const shape = M().presetMoved(card, moves);
    const model = M().reduce(initModel({ id: 0, name: card.name, claims: [], parts: [] }, { shape: true }),
        { type: 'applyPreset', preset: shape });
    const heads = M().toCells(model).map(c => c.core_node_name);
    const at = s => moves[s.node] || s.node;
    const placed = (card.spots || []).map(at);
    const pairs = {};
    const add = (deck, spot) => { (pairs[deck] = pairs[deck] || []).indexOf(spot) < 0 && pairs[deck].push(spot); };
    const opened = M().presetMoved(card, card.last_moves || {});
    for (const c of Object.values(opened.cells || {})) {
        if (c.swap_mode === 'sequential') continue;
        for (const f of DECK_FIELDS) if (c[f]) add(c[f], c.core_node_name);
    }
    for (const s of card.spots || []) {
        if (!s.at || (s.job !== 'on_deck' && s.job !== 'third')) continue;
        for (const stand of Object.keys(s.at)) for (const n of s.at[stand]) add(n, stand);
    }
    const tags = {};
    (card.groups || []).forEach((g, i) => {
        const tag = { letter: (String(g.name || '').trim()[0] || '?').toUpperCase(), group: (i % 2) + 1 };
        for (const s of card.spots || []) if (s.group === g.name && s.job === 'spot') tags[at(s)] = tag;
    });
    const cell = M().pictureCells(model, stationCellOf(heads));
    const opts = {
        relation: true, frame: { w: o.w || SHEET_PIC_W }, editable: !!o.editable, pairs: pairs, tags: tags,
        active: [...new Set(placed.concat(o.lit || []))],
    };
    opts.sentences = deskSentences(sentencesFromModel(model, !!relationPlacement(cell, opts)));
    if (o.selected) {
        opts.selected = moves[o.selected] || o.selected;
        opts.selectCard = true;
        opts.targets = o.lit || [];
    }
    const p = process();
    const markup = renderFlowPicture({ cell: cell, station: { name: p ? p.name : '' } }, opts);
    const w = Math.round(opts.width || o.w || SHEET_PIC_W), h = Math.round(opts.height || PICTURE_H);
    return '<div class="pd-pic pd-sheetpic"><svg id="' + o.id + '" class="os-flow-picture" viewBox="0 0 ' + w + ' ' + h +
        '" width="' + w + '" height="' + h + '">' + markup + '</svg></div>';
}

// startNewSpots opens the Spots step for the recipe picked, on its pre-fill.
function startNewSpots(id) {
    const card = recipeCard(id);
    S.newStyle = card ? { recipe: card.id, moves: Object.assign({}, card.last_moves || {}), sel: '' } : null;
    drawNewSpots();
}

// drawNewSpots redraws the step alone, so the parts picked and the CATID
// typed stay as they are.
function drawNewSpots() {
    const el = $('pd-newspots');
    const ns = S.newStyle;
    const card = ns && recipeCard(ns.recipe);
    if (el && !card) el.innerHTML = '';
    if (el && card) {
        const lit = ns.sel ? M().spotsLit(card, ns.moves, ns.sel) : [];
        const place = ns.sel ? (card.spots || []).find(s => s.node === ns.sel) : null;
        const standing = place ? (ns.moves[place.node] || place.node) : '';
        const hint = !place
            ? (Object.keys(ns.moves).length ? 'Where this cell last ran the recipe.' : 'The recipe’s own spots.') +
                ' Click a spot or an on-deck spot to move it.'
            : standing + (place.group ? ' · ' + place.group : '') + ' · ' + (lit.length
                ? 'click a spot marked “move here”, or ' + standing + ' again to leave it.'
                : 'nowhere else is set up for it.');
        el.innerHTML = '<div class="pd-fld"><label>Spots<small>where this style runs</small></label>' +
            '<div class="v"><span class="pd-note">' + esc(hint) + '</span></div></div>' +
            recipePicture(card, ns.moves, { id: 'pd-nsvg', editable: true, selected: ns.sel, lit: lit }) +
            '<div class="pd-mvs">' + newMoveChips(card, ns.moves) + '</div>';
    }
    markNewStyle();
}

// newMoveChips is one chip per spot moved, its carried places in it, and one
// per attached place moved on its own; a chip's ✕ puts it back — a spot with
// what it carried.
function newMoveChips(card, moves) {
    const spots = card.spots || [];
    const moved = n => moves[n] && moves[n] !== n;
    const chip = (node, text) => '<button class="pd-chip set" data-act="nmove-rm" data-node="' + esc(node) + '">' +
        esc(text) + ' <i>&times;</i></button>';
    const out = [];
    for (const s of spots) {
        if (!moved(s.node)) continue;
        const attached = !!s.at || s.job !== 'spot';
        if (attached && s.of && moved(s.of)) continue;
        if (attached) { out.push(chip(s.node, (FLOOR_NOUN[s.job] || s.job) + ' for ' + (moves[s.of] || s.of) + ': ' + s.node + ' → ' + moves[s.node])); continue; }
        const carried = spots.filter(x => x.of === s.node && x !== s && moved(x.node))
            .map(x => (FLOOR_NOUN[x.job] || x.job) + ' ' + moves[x.node]);
        out.push(chip(s.node, s.node + ' → ' + moves[s.node] + (carried.length ? ' (' + carried.join(', ') + ')' : '')));
    }
    return out.length ? out.join('') : '<span class="pd-dim">the recipe’s own spots</span>';
}

function undoNewMove(node) {
    const ns = S.newStyle;
    const card = ns && recipeCard(ns.recipe);
    if (!card) return;
    delete ns.moves[node];
    for (const s of card.spots || []) if (s.of === node) delete ns.moves[s.node];
    ns.sel = '';
    drawNewSpots();
}

// tapNewSpot is one click on the Spots step's picture: the station's own
// spotsTap over the card, nothing restated here.
function tapNewSpot(node) {
    const ns = S.newStyle;
    const card = ns && recipeCard(ns.recipe);
    if (!card) return;
    const r = M().spotsTap(card, ns.moves, ns.sel, node);
    ns.moves = r.moves;
    ns.sel = r.selected;
    drawNewSpots();
}

// body[data-pd-new-style] = "spots;sel=PLN_010;moves=2", for the shots
// harness: the step is up, what is selected (where it stands), how many moves.
function markNewStyle() {
    const ns = S.newStyle;
    let m = '';
    if (ns) {
        const card = recipeCard(ns.recipe);
        const place = ns.sel && card ? (card.spots || []).find(s => s.node === ns.sel) : null;
        m = 'spots' + (place ? ';sel=' + (ns.moves[place.node] || place.node) : '') +
            ';moves=' + Object.keys(ns.moves).filter(k => ns.moves[k] && ns.moves[k] !== k).length;
    }
    try { document.body.dataset.pdNewStyle = m; } catch (_) { /* nothing to publish on */ }
}

// withFindings is a mint's refusal with the findings a 422 names
// (apiMintFlow writes them beside the error); the sheet said the error alone
// (P0 :6467).
function withFindings(out) {
    const found = out.shown ? [] : (bodyOf(out).findings || []).map(f => f && f.message).filter(Boolean);
    return said(out) + (found.length ? ': ' + found.join('; ') : '');
}

// mintNewStyle posts the sheet's recipe and parts to flow/mint, then the CATID
// to the minted style if one was typed, and opens it. Each part names its
// slot's spot where the Spots step put it (a moved slot's node, D33), and the
// moves ride as overrides.
async function mintNewStyle(recipe) {
    const p = (S.composer.presets || []).find(x => String(x.id) === String(recipe));
    const moves = (S.newStyle && String(S.newStyle.recipe) === String(recipe) && S.newStyle.moves) || {};
    const picks = [];
    for (let i = 0; i < ((p && p.slots) || []).length; i++) {
        const code = sheetValue('slot' + i);
        if (!code) { sheetStatus('Every slot needs a part.', true); return; }
        const head = (p.slots[i].nodes || [])[0] || '';
        picks.push({ code: code, node: moves[head] || head });
    }
    // The mint once per sheet: a CATID refused after it is resumed, not minted
    // again (ruling 5).
    const chain = chainMade();
    const made = chain.mint || await send('POST', '/api/processes/' + S.processID + '/flow/mint',
        B().flowMint(recipe, picks, stationIDForSave(), moves), NO_NOTICE);
    if (!made.ok) { sheetStatus(withFindings(made), true); return; }
    chain.mint = made;
    const catid = String(sheetValue('catid') || '').trim();
    if (catid) {
        // A MINT THAT RESOLVED ("Already runs as …") NAMES A STYLE THAT HAS A
        // DESCRIPTION, and apiUpdateStyle writes every field it decodes: the
        // CATID's write starts from that style as the page holds it, or it
        // blanks the description (LEAD-NOTES B1). A minted one has none yet.
        const id = Number(bodyOf(made).style_id);
        const st = S.styles.find(s => s.id === id) ||
            { id: id, name: bodyOf(made).style_name, process_id: S.processID };
        const set = await send('PUT', '/api/styles/' + st.id, B().styleWrite(st, S.processID, { expected_catid: catid }), NO_NOTICE);
        if (!set.ok) {
            chainHalf(refreshProcess);
            sheetStatus('Minted ' + st.name + ', but its CATID did not save' + said(set, ': ') + '.', true);
            return;
        }
    }
    chainDone();
    closeSheet();
    S.notice = (bodyOf(made).minted ? 'Minted ' : 'Already runs as ') + bodyOf(made).style_name + '.';
    S.styleID = Number(bodyOf(made).style_id) || S.styleID;
    await refreshProcess();
}

// P0's "Add group". Taxonomy for the list and nothing else reads it, which the
// sheet says so nobody goes looking for what it does.
function openNewGroup() {
    pageSheet('Add group', 'how the process list is grouped. Nothing else reads it.',
        sheetField('Name', 'what this group of processes is called', 'name', '') +
        sheetField('Description', '', 'description', ''),
        'Add', async () => {
            const name = String(sheetValue('name') || '').trim();
            if (!name) { sheetStatus('A group needs a name.', true); return; }
            const made = await send('POST', '/api/process-groups',
                B().processGroupCreate(name, sheetValue('description')), NO_NOTICE);
            if (!made.ok) { sheetStatus(said(made), true); return; }
            closeSheet();
            const res = await send('GET', '/api/process-groups');
            if (res.ok && Array.isArray(res.body)) S.groups = res.body;
            drawList();
        }, false);
}

// "+ Add a position" is a MODEL OP, not a server write: it turns on a position
// the press already has, in the draft, and nothing is written until Save style.
// One free position is the whole decision, so it is made without a dialog;
// more than one and the engineer says which.
// THE MODE COMES WITH IT, because on this screen a cell without one does not
// exist. reduce's addPosition leaves `mode` null unless the action carries one,
// and positionsTable draws a row only for `on && mode` — so "+ Add a position"
// turned the cell on, drew nothing, left the name in the free list, and turned
// the bar red naming a position that was nowhere on screen with "tap the
// position and pick a choreography" against a row that did not exist. Save was
// disabled by the finding, so the only way out was Discard.
//
// The desktop's editor IS the row. So the row arrives with a mode and the
// engineer changes it from the row's own swap-mode picker.
//
// WHICH MODE: the one this flow already uses. A press runs one choreography
// on almost every position, so the common case is right and the odd one out is
// one click. Inside a recipe draft that is the recipe's own mode, since its
// rows have one; a draft whose rows were all taken off is not bound to it.
//
// A FLOW WITH NO ROWS HAS NONE TO BORROW, and there is no default (pick-a-part
// U2): the fallback was the first key of the mode table,
// `two_robot_press_index`, which put a press-index row on a weld cell. So the
// engineer is asked — the row picker's own list of choreographies, then the
// position. It used to answer with a notice sending them to a recipe instead,
// which left a style with no rows no way to gain its first from this screen.
function defaultModeFor() {
    const seen = {};
    for (const n of Object.keys(S.model.cells)) {
        const c = S.model.cells[n];
        if (c.on && c.mode) seen[c.mode] = (seen[c.mode] || 0) + 1;
    }
    return Object.keys(seen).sort((a, b) => seen[b] - seen[a])[0] || '';
}

// THE POSITION FIRST, THEN HOW IT SWAPS (F6). The engineer is looking at the
// press and knows which spot they mean; the mode is a question about that
// spot. One free position is the whole first answer, so it is not asked.
function addFreePosition(btn) {
    const free = freePositions();
    if (!free.length) return;
    const mode = defaultModeFor();
    if (free.length === 1) { pickFreeMode(btn, free[0], mode); return; }
    const pop = $('pd-pop');
    if (!pop) return;
    // The shared picker; its press stops at the list (openRowPicker), so the
    // mode menu that replaces it is not closed by the press that picked.
    openRowPicker(pop, btn, free.map(n => ({ value: n, label: n })), node => pickFreeMode(btn, node, mode),
        { head: 'Add a position', placeholder: 'find a position' });
}

// THE MODE MENU CARRIES THE CARDS' OWN WORDS (F25): the mode's name, and
// under it the line the picture's card says about that mode. A mode with no
// line here still shows, by its name alone.
const MODE_WORDS = {
    single_robot: 'One robot, parks and swaps',
    two_robot: 'Robot 1 stages the new bin, Robot 2 pulls old',
    two_robot_press_index: 'Robot 1 takes the front out, Robot 2 indexes',
    sequential: 'Robot 1 pulls old, then Robot 2 brings new',
};

// pickFreeMode adds node with the mode the flow already runs, or asks for one
// when it runs none (a recipe's first row, an empty style's).
function pickFreeMode(btn, node, mode) {
    if (mode) { addFreeAs(node, mode); return; }
    const pop = $('pd-pop');
    if (!pop) return;
    const labels = M().modeLabels();
    const order = Object.keys(MODE_WORDS).filter(m => labels[m])
        .concat(Object.keys(labels).filter(m => !MODE_WORDS[m]));
    pop.innerHTML = '<div class="pd-lbl">How ' + esc(node) + ' swaps</div>' +
        order.map(m => '<button class="pd-modeopt" data-freemode="' + esc(m) + '">' + gl(m, 22, {}) +
            '<span>' + esc(labels[m]) + (MODE_WORDS[m] ? '<small>' + esc(MODE_WORDS[m]) + '</small>' : '') +
            '</span></button>').join('');
    showPop(pop, btn);
    placePopover(pop, btn);
    pop.querySelectorAll('[data-freemode]').forEach(b => b.addEventListener('click', () => {
        const m = b.dataset.freemode;
        closePop();
        addFreeAs(node, m);
    }));
}

// addFreeAs adds the position as mode, and the staging its mode requires from
// the routing set (F7), the way the model fills its source and destination:
// the role's rows in their order, inbound first, outbound the next one.
function addFreeAs(node, mode) {
    const acts = [{ type: 'addPosition', node: node, mode: mode }];
    const after = M().reduce(S.model, acts[0]);
    const rows = (S.composer.routing || []).filter(r => r.role === 'staging' && r.enabled !== false &&
        r.core_node_name !== node && !isPositionRow(r.core_node_name))
        .sort((a, b) => (a.sequence || 0) - (b.sequence || 0)).map(r => r.core_node_name);
    const set = { staging: 'setStaging', parkOld: 'setParkOld' };
    const taken = [];
    for (const chip of M().rowColumns(after, node, 'staging')) {
        if (!chip.required || chip.value || !set[chip.key]) continue;
        const pick = rows.find(n => taken.indexOf(n) < 0);
        if (!pick) break;
        taken.push(pick);
        acts.push({ type: set[chip.key], node: node, staging: pick });
    }
    apply(acts);
}

// ── Add and use: no dead ends (F3, owner ruling 2) ───────────────────────────
//
// A REQUIRED CELL WITH NOTHING TO OFFER OFFERS THE PLANT. Its picker drew the
// reason (no row in that role of the routing set) and the fix sat on another
// tab, so a recipe draft for a 1-Robot Swap with no staging lane could not be
// finished from where it was being drawn. Under the reason, the picker lists
// Core's nodes; a pick adds the node to the routing set in that role and uses
// it, in one request (the routing set's own upsert). The node list is the one
// the page reads once and every picker reuses (NODES).
const ADD_AND_USE_ROLE = { inbound_source: 'source', outbound_destination: 'destination',
    inbound_staging: 'staging', outbound_staging: 'staging' };
const ADD_AND_USE_ACT = { inbound_source: ['setSource', 'source'], outbound_destination: ['setDest', 'dest'],
    inbound_staging: ['setStaging', 'staging'], outbound_staging: ['setParkOld', 'staging'] };

let addUsePicker = null;   // the open block's picker (offerAddAndUse)

// offerAddAndUse draws the block: the node adapter over the page's one node
// list, open in the popover, each node greyed with why this cell cannot take
// it (a position of this process, a name already in the role, a lane or a
// member where Core picks). A pick is the write (the shell's write-through):
// the routing set gains the node, then the row uses it. The shell's Add and
// use slot takes a name the list does not carry, for a node Core sent after
// this page read its list; the Edge checks it against Core's list on the
// write and says why when it refuses.
function offerAddAndUse(pop, node, kind) {
    const field = routingFieldOf(kind);
    const role = ADD_AND_USE_ROLE[field];
    if (!role || !S.model || !M().fieldRequired(S.model, node, field)) return;
    const words = ROUTING_GROUPS.find(g => g[0] === role)[1];
    const inRole = new Set((S.composer.routing || []).filter(r => r.role === role).map(r => r.core_node_name));
    const box = document.createElement('div');
    box.className = 'pd-adduse';
    const part = (cls, text) => {
        const d = document.createElement('div');
        d.className = cls;
        if (text) d.appendChild(document.createTextNode(text));
        return box.appendChild(d);
    };
    part('pd-lbl', 'Add and use · ' + words);
    const host = part('pd-addusepk');
    const warn = part('none pd-warn');
    warn.hidden = true;
    pop.appendChild(box);
    // THE PICK STAYS IN THE POPOVER. The shell redraws its rows as it takes a
    // pick, so the row clicked is out of the tree by the time the click
    // reaches the document, whose closer would take it for a press outside
    // and close the popover over the write, and over a refusal's reason.
    box.addEventListener('click', e => e.stopPropagation());
    const use = name => addAndUse(node, field, name).then(why => {
        warn.textContent = '';
        if (why) warn.appendChild(document.createTextNode(why));
        warn.hidden = !why;
        return why ? null : name;
    });
    // One at a time: the last popover's picker lets go of the node list.
    if (addUsePicker) addUsePicker.destroy();
    addUsePicker = nodePicker(host, NODES, {
        rule: n => (n.name === node ? 'this position'
            : n.name[0] === '_' ? 'not a place'
            : isPositionRow(n.name) ? 'a position of this process — available to every flow on it already'
            : inRole.has(n.name) ? 'already in ' + words
            : B().legExclusion(S.coreNodes, n.name, role)),
    }, {
        open: true, chips: false, placeholder: 'find a node by name',
        onPick: (next, row) => use(row.value).then(name => (name ? [name] : {})),
        add: { label: q => 'Add “' + q + '” to ' + words + ' and use it', run: q => use(q) },
    });
    loadCoreNodes();
}

// addAndUse adds name to the routing set in the field's role, in one request
// (the routing set's own upsert), and puts it on the row; it resolves '' when
// it did, or why it did not.
async function addAndUse(node, field, name) {
    const role = ADD_AND_USE_ROLE[field];
    const count = (S.composer.routing || []).filter(r => r.role === role).length;
    const out = await send('POST', '/api/processes/' + S.processID + '/routing-nodes',
        B().routingAdd(name, role, count), NO_NOTICE);
    if (!out.ok) return said(out) || 'Not added.';
    // The composer's routing set gains the row, so every picker on the page
    // offers it now; Settings' copy is read again when it next opens.
    S.composer.routing = (S.composer.routing || []).concat([{ core_node_name: name, role: role, sequence: count + 1, label: name }]);
    S.routing = null;
    closePop();
    const [act, arg] = ADD_AND_USE_ACT[field];
    apply({ type: act, node: node, [arg]: name });
    return '';
}

// ── Add process, and its Change… (the same sheet, filled in) ────────────────
//
// ONE SHEET MAKES A LINE (owner, 2026-11-15): its name, its group, its
// operator screen, its positions and routing set on the set pick, and its
// parts. Only the name is needed: Setup walks through the rest, and its
// Change… opens this sheet again, filled in (HOP, 2026-11-12: correcting a
// process is answering the questions that made it). The screen is made with
// the process, so the line someone leaves Setup with has one to open.

// The part set's picker, Settings' "Parts this process runs".
const PART_SET_PICKER = 'parts';

// What Settings says under the part set.
const PART_SET_NOTE = 'A part used by a flow stays available until it is removed from that flow.';
// PART_SET_HELP is the part set's one help line: what it is and whose list
// the parts come from.
const PART_SET_HELP = 'what this line runs, from Core’s catalog';

// loadProcessPayloads reads the STORED half of a process's part set — what
// Settings' picker is about to replace. A failed read opens the picker empty, and
// saving that would be an accidental wipe, so a failure is reported rather
// than treated as "no parts": the caller opens on null and the sheet says so.
async function loadProcessPayloads(processID) {
    const res = await send('GET', '/api/processes/' + processID + '/payloads');
    if (!res.ok) return null;
    return Array.isArray(res.body) ? res.body : [];
}

const PROC_SET = 'proc';

function openAddProcess() { processSheet(null); }

// openEditProcess is Setup's Change…: the sheet over the open process, read
// fresh first. Each door behind it writes destructively (claimed-nodes and the
// part set are set-tos), so a screen whose positions could not be read opens
// nothing, and a part set or routing set that could not be read is left alone
// by Save.
async function openEditProcess() {
    const p = process();
    if (!p) return;
    const screens = stationsOf(p.id);
    const [, parts, , ...lists] = await Promise.all([loadCoreNodes(), loadProcessPayloads(p.id), loadCurtainNodes(),
        ...screens.map(st => readStationNodes(st.id))]);
    if (lists.some(l => !l)) {
        toast('A screen’s positions could not be read, so they cannot be changed yet. Try again.', 'error');
        return;
    }
    screens.forEach((st, i) => { S.stationNodes[String(st.id)] = lists[i].slice(); });
    if (!S.routing) await loadRouting();
    processSheet({ p: p, screens: screens, parts: parts, routingRead: !routingUnread() });
}

// processSheet draws the process sheet: Add process (edit null) or its
// Change… (edit: what openEditProcess read).
function processSheet(edit) {
    const pid = edit ? edit.p.id : 0;
    S.add = { groupID: edit ? (edit.p.group_id || 0) : 0, edit: edit };
    const sel = { positions: [], source: [], staging: [], destination: [] };
    if (edit) {
        sel.positions = processPositions();
        for (const g of ROUTING_GROUPS) sel[g[0]] = routingRowsFor(g[0]).map(r => r.core_node_name);
        S.add.before = {
            process: settingsDraftFor(edit.p),
            screen: edit.screens.length === 1 ? edit.screens[0].name : '',
            sel: JSON.parse(JSON.stringify(sel)),
            parts: edit.parts ? edit.parts.slice() : null,
        };
    }
    pickerInit(PART_SET_PICKER, { source: 'payloads', selected: edit && edit.parts ? edit.parts : [], fold: PART_FOLD,
        placeholder: 'find a part by name' });
    const group = S.groups.find(g => g.id === S.add.groupID);
    // A LINE HAS ONE SCREEN (owner, 2026-11-15). One made before that with
    // two is named here, and each of its screens is its own on Settings.
    let screenField = sheetField('Operator screen', 'blank: named after the process', 'screen', '');
    if (edit && edit.screens.length === 1) screenField = sheetField('Operator screen', 'its HMI', 'screen', edit.screens[0].name);
    else if (edit && edit.screens.length > 1) {
        screenField = '<div class="pd-fld"><label>Operator screens<small>each set on Settings</small></label>' +
            '<div class="v"><span class="pd-dim">' + esc(edit.screens.map(st => st.name).join(' · ')) + '</span></div></div>';
    }
    const unread = what => '<p class="pd-note pd-warn">This process’s ' + what + ' could not be read, so Save leaves it alone. ' +
        'Reload the page to change it.</p>';
    const body = '<div class="pd-addproc pd-procsheet">' +
        '<div class="pd-two">' + sheetField('Name', 'what this process is called here', 'name', edit ? edit.p.name : '') +
        screenField + '</div>' +
        '<div class="pd-fld"><label>Group<small>groups processes on the list</small></label>' +
        '<div class="v"><button class="pd-sel" id="pd-addgroup" data-act="add-pickgroup">' +
        esc(group ? group.name : 'Ungrouped') + '<i class="car"></i></button></div></div>' +
        // PARTS FIRST (owner, 2026-11-15: "couple times now i forgot to get to
        // parts ... parts above so you select parts first"): the node list
        // below them takes scrolling, so a field after it was missed.
        // ONE HEADING, THE FIELD, ONE LINE (owner, 2026-11-15: "whats this
        // redundancy with parts stuff?"): it said Parts and what the line runs
        // twice over, as a step heading and again as a field label.
        '<div class="pd-sec"><div class="pd-lbl">1 · Parts</div><span class="pd-dim">' + esc(PART_SET_HELP) + '</span></div>' +
        '<div class="pd-partsbox">' + pickerBox(PART_SET_PICKER) + '</div>' +
        (edit && edit.parts === null ? unread('part set') : '') +
        '<div class="pd-sec"><div class="pd-lbl">2 · Nodes</div><span class="pd-dim">click a box, then tick nodes into it</span></div>' +
        (edit && !edit.routingRead ? unread('routing set') : '') +
        '<div class="pd-sphost" id="sp-' + PROC_SET + '"></div>' +
        '<p class="pd-still" id="pd-still"></p></div>';
    pageSheet(edit ? 'Edit process' : 'Add process',
        edit ? 'what this process is, what works it, and where its bins come from and go.'
            : 'Name it, pick its parts, pick its nodes. Only the name is needed now; Setup walks you through the rest.',
        body, edit ? 'Save' : 'Create', edit ? submitEditProcess : submitAddProcess, false, undefined, true);
    mountSetPick(PROC_SET, 'sp-' + PROC_SET, sel, {
        pid: pid, fresh: true,
        closed: b => (edit && !edit.routingRead && b !== 'positions' ? 'the routing set could not be read' : ''),
        onChange: drawStill,
    });
    loadLivePositions().then(() => { const sp = S.setPicks[PROC_SET]; if (sp) sp.api.refresh(); });
    drawStill();
    // A REFUSED FIELD STOPS BEING REPORTED ONCE IT IS TYPED IN. Only the
    // up-front check clears this way: a refusal from the write chain is the
    // record of what was already made, and typing must not erase it.
    for (const f of ['name', 'screen']) {
        const el = sheetEl().querySelector('[data-f="' + f + '"]');
        if (el) el.addEventListener('input', () => {
            if (S.add && S.add.refused) { S.add.refused = false; sheetStatus('', false); }
        });
    }
    // The screen's name follows the process's until one is typed.
    const nameBox = sheetEl().querySelector('[data-f="name"]');
    const screenBox = sheetEl().querySelector('[data-f="screen"]');
    if (nameBox && screenBox && !screenBox.value) {
        nameBox.addEventListener('input', () => { screenBox.placeholder = nameBox.value.trim() || 'blank: named after the process'; });
    }
}

// drawStill is the sheet's line over its buttons: what the line still needs,
// in the colour of the first box it names, and that Setup finishes it too.
function drawStill() {
    const el = $('pd-still');
    if (!el) return;
    const need = stillNeeded(PROC_SET);
    const first = SET_BOXES.find(b => b.label === need[0]);
    el.innerHTML = need.length
        ? '<i class="setpick-dot" data-role="' + esc(first.role) + '"></i>Still to pick: <b>' + esc(need.join(', ')) +
            '</b>. You can also finish it in Setup.'
        : '';
}

// submitAddProcess is Create: a chain of writes, each once per sheet, that
// stops at the first refusal and resumes there on the next press (ruling 5),
// saying what already landed:
//   1. the process; 2. its screen, named as typed or after the process (a
//   line with no screen cannot be run); 3. the positions on that screen;
//   4. the routing set, inbound, staging and outbound, each in picked order
//   (the lowest of a role is a new position's default), in one write;
//   5. the part set. The flow-composer gate stays shut (brief U0).
async function submitAddProcess() {
    const name = String(sheetValue('name') || '').trim();
    if (!name) { S.add.refused = true; sheetStatus('A process needs a name.', true); return; }
    const screen = String(sheetValue('screen') || '').trim() || name;
    S.add.refused = false;
    sheetStatus('Creating…');
    const chain = chainMade();
    if (!chain.processID) {
        const made = await send('POST', '/api/processes', B().processCreate({ name: name, description: '', group_id: S.add.groupID }), NO_NOTICE);
        if (!made.ok) { sheetStatus(said(made), true); return; }
        chain.processID = Number(bodyOf(made).id);
        chain.done = ['The process was created'];
        chainHalf(async () => { await reloadProcesses(); await reloadStations(); if (!S.processID) drawList(); });
    }
    const processID = chain.processID;
    const done = chain.done;
    const stop = (what, out) => sheetStatus(done.join('; ') + '. ' + what + (out.shown ? ' was not written.' : ' was refused: ' + out.error), true);
    if (!chain.stationID) {
        const st = await send('POST', '/api/operator-stations', B().stationWrite(null, processID, false, { name: screen, note: '' }), NO_NOTICE);
        if (!st.ok) { stop('Its operator screen', st); return; }
        chain.stationID = Number(bodyOf(st).id);
        done.push('its screen added');
    }
    const stationID = chain.stationID;
    const claimed = setPickValue(PROC_SET, 'positions');
    if (claimed.length && !chain.claimed) {
        const nodes = await send('PUT', '/api/operator-stations/' + stationID + '/claimed-nodes', B().stationNodes(claimed), NO_NOTICE);
        if (!nodes.ok) { stop('Its positions', nodes); return; }
        chain.claimed = true;
        done.push('its positions claimed');
    }
    const rows = [];
    for (const g of ROUTING_GROUPS) for (const n of setPickValue(PROC_SET, g[0])) rows.push({ core_node_name: n, role: g[0] });
    if (rows.length && !chain.routed) {
        const out = await send('PUT', '/api/processes/' + processID + '/routing-nodes', B().routingSet(rows), NO_NOTICE);
        if (!out.ok) { stop('Its routing set', out); return; }
        chain.routed = true;
        done.push('its routing set written');
    }
    const partSet = pickerValue(PART_SET_PICKER);
    if (partSet.length && !chain.parts) {
        const out = await send('PUT', '/api/processes/' + processID + '/payloads', B().processPayloads(partSet), NO_NOTICE);
        if (!out.ok) { stop('Its part set', out); return; }
        chain.parts = true;
        done.push('its part set written');
    }
    chainDone();
    closeSheet();
    dropSetPick(PROC_SET);
    await reloadProcesses();
    await reloadStations();
    await openProcess(processID);
}

// submitEditProcess is the Change… sheet's Save: what moved, through the doors
// Settings saves with, in the order one write needs another, stopping at the
// first refusal with what already landed named:
//   1. the process row (name, group); 2. the screen, renamed, or made when the
//   line has none and is given a name or positions (it takes every position on
//   no screen); 3. the positions (savePositions, which asks first when one is
//   taken off: Save again takes it off); 4. the routing set, in one write that
//   adds and takes out (saveRouting); 5. the part set, when it was read.
// Each step moves its `before` forward as it lands, so a press after a refusal
// resumes where it stopped.
async function submitEditProcess() {
    const e = S.add.edit;
    const p = e.p;
    const before = S.add.before;
    const typed = (f, was) => (sheetEl().querySelector('[data-f="' + f + '"]') ? String(sheetValue(f) || '').trim() : was);
    const name = typed('name', p.name || '');
    if (!name) { S.add.refused = true; sheetStatus('A process needs a name.', true); return; }
    S.add.refused = false;
    const done = S.add.done || (S.add.done = []);
    const stop = (what, out) => sheetStatus((done.length ? done.join('; ') + '. ' : '') + what +
        (out.shown ? ' was not written.' : ' was refused: ' + out.error), true);
    sheetStatus('Saving…');
    S.saveNotes = [];

    const draft = Object.assign(settingsDraftFor(p), { name: name, group_id: S.add.groupID || 0 });
    if (JSON.stringify(draft) !== JSON.stringify(before.process)) {
        const out = await send('PUT', '/api/processes/' + p.id, B().processSettings(p, draft), NO_NOTICE);
        if (!out.ok) { stop('The process', out); return; }
        before.process = draft;
        done.push('The process was saved');
    }

    const positions = setPickValue(PROC_SET, 'positions');
    const screenName = e.screens.length < 2 ? typed('screen', before.screen) : '';
    let made = false;
    if (e.screens.length === 1 && screenName && screenName !== before.screen) {
        const st = e.screens[0];
        const out = await send('PUT', '/api/operator-stations/' + st.id, stationBody(st, true, { name: screenName, note: st.note || '' }), NO_NOTICE);
        if (!out.ok) { stop('Its operator screen', out); return; }
        before.screen = screenName;
        done.push('its screen renamed');
    } else if (!e.screens.length && (screenName || positions.length)) {
        const out = await send('POST', '/api/operator-stations',
            B().stationWrite(null, p.id, false, { name: screenName || name, note: '' }), NO_NOTICE);
        if (!out.ok) { stop('Its operator screen', out); return; }
        await reloadStations();
        S.stationNodes[String(Number(bodyOf(out).id))] = [];
        e.screens = stationsOf(p.id);
        made = true;
        done.push('its screen added');
    }

    if (made || JSON.stringify(positions) !== JSON.stringify(before.sel.positions)) {
        const out = await savePositions(screensDraft(positions, made));
        if (!out.ok) { stop('Its positions', out); return; }
        before.sel.positions = positions.slice();
        done.push('its positions set');
    }

    if (e.routingRead) {
        const now = {};
        const was = {};
        for (const g of ROUTING_GROUPS) { now[g[0]] = setPickValue(PROC_SET, g[0]); was[g[0]] = before.sel[g[0]]; }
        if (JSON.stringify(now) !== JSON.stringify(was)) {
            const out = await saveRouting(now);
            if (!out.ok) { stop('Its routing set', out); return; }
            for (const g of ROUTING_GROUPS) before.sel[g[0]] = now[g[0]].slice();
            done.push('its routing set written');
            S.saveNotes.push('The routing set was saved.');
        }
    }

    if (before.parts !== null) {
        const want = pickerValue(PART_SET_PICKER);
        if (JSON.stringify(want) !== JSON.stringify(before.parts)) {
            const out = await writePartSet(want);
            if (!out.ok) { stop('Its part set', out); return; }
            before.parts = want.slice();
            done.push('its part set saved');
            S.saveNotes.push('The part set was saved.');
        }
    }

    const summary = S.saveNotes.join(' ') || (done.length ? 'Saved.' : 'Nothing had changed.');
    S.saveNotes = [];
    closeSheet();
    dropSetPick(PROC_SET);
    await reloadStations();
    await refreshProcessStationNodes();
    await Promise.all([loadRouting(), loadCurtainNodes()]);
    S.partSet = null;
    await loadPartSet();
    // Settings' own pick is built on what this sheet changed: drawn anew.
    dropSetPick(ST_SET);
    await refreshProcess();
    if (S.tab === 'settings') { S.settingsNotice = summary; drawSettings(); } else toast(summary, 'info');
}

function stationBody(st, editing, change) {
    return B().stationWrite(st, S.processID, editing, change);
}

// keptPositionsSentence is what a save says of the positions the server kept
// on a screen instead of retiring them: each has an active order.
function keptPositionsSentence(names) {
    return names.length === 1
        ? 'Saved. ' + names[0] + ' stays on this screen until its active order finishes.'
        : 'Saved. ' + names.join(', ') + ' stay on this screen until their active orders finish.';
}

// THE SCREEN SHEET IS ITS NAME AND NOTE. Its positions are its row under
// Settings › Screens and positions, the one editor of a screen's positions
// (owner ruling 2026-11-14: one editor per fact); this sheet wrote the same
// claimed-nodes list from a second picker.
// A NEW SCREEN TAKES THE POSITIONS ON NO SCREEN (the setup inventory's trap:
// a screen added after its positions took none of them, and its row had no
// picker until a Save, so the same three were picked twice). It is offered
// only on a line with no screen (owner, 2026-11-15: a line has one), and it
// takes every position the Positions box holds, saved or not yet.
async function openScreenSheet(stationID) {
    const st = stationsOf(S.processID).find(s => s.id === Number(stationID)) || {};
    const editing = !!st.id;
    let made = 0;
    const takes = editing || stationsOf(S.processID).length ? []
        : (S.setPicks[ST_SET] && S.setPicks[ST_SET].pid === S.processID ? setPickValue(ST_SET, 'positions') : loosePositions());
    pageSheet(editing ? 'Edit ' + st.name : 'Add operator screen',
        editing ? 'the HMI an operator works this process from. Its positions are the Positions box in Settings.'
            : 'the HMI an operator works this process from.',
        sheetField('Name', 'what the screen is called on the floor', 'name', editing ? st.name : (process() || {}).name || '') +
        sheetField('Note', 'anything the next engineer should know about it', 'note', st.note) +
        (takes.length ? '<p class="pd-note">It takes ' + (takes.length === 1 ? 'the position' : 'the ' + takes.length + ' positions') +
            ' on no screen: ' + esc(takes.join(', ')) + '.</p>' : ''),
        editing ? 'Save' : 'Add',
        async () => {
            // A press after its positions were refused resumes there: the
            // screen it already made is not made twice.
            let id = made;
            if (!id) {
                const wrote = await send(editing ? 'PUT' : 'POST',
                    editing ? '/api/operator-stations/' + st.id : '/api/operator-stations',
                    stationBody(st, editing, { name: sheetValue('name'), note: sheetValue('note') }), NO_NOTICE);
                if (!wrote.ok) { sheetStatus(said(wrote), true); return; }
                id = editing ? st.id : Number(bodyOf(wrote).id);
                if (!editing) made = id;
            }
            if (takes.length) {
                const put = await send('PUT', '/api/operator-stations/' + id + '/claimed-nodes', B().stationNodes(takes), NO_NOTICE);
                if (!put.ok) { sheetStatus('The screen was added. Its positions were refused: ' + said(put), true); await reloadStations(); return; }
            }
            closeSheet();
            await reloadStations();
            await refreshStationNodes(id);
            if (takes.length) await loadCurtainNodes();
            await refreshProcess();
            // WHAT IT TOOK IS SAVED: the Positions box starts from what the
            // screen now holds, so the bar does not call the screen's own
            // positions unsaved.
            const sp = S.setPicks[ST_SET];
            if (takes.length && sp && sp.pid === S.processID && S.tab === 'settings' && S.stShell) {
                sp.api.set('positions', processPositions(), { clean: true });
                S.looseOnto = '';
                S.stShell.page.rebase(['positions']);
                drawSettings();
            }
        },
        false);
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
// openContainmentPicker — the Quality Hold destination picker: the node
// adapter over the page's one node list, in the popover. A lane, or a member
// where Core picks, is drawn greyed with why (the stamp writes a claim leg,
// and no leg names one: legExclusion); "-- None --" is the blank that means
// "no route yet" — the save refuses an enabled toggle with none picked. The
// node list is the shared read every picker uses, so this is async where its
// sibling is not.
async function openContainmentPicker(btn) {
    // loadCoreNodes never rejects: a failed read leaves S.coreNodes null (the
    // door's notice says why), and this opened on "-- None --" alone with
    // nothing on the page (census #1).
    await loadCoreNodes();
    if (!S.coreNodes) {
        S.settingsError = 'Could not read Core’s node list. Try again.';
        drawSettings();
        return;
    }
    const pop = $('pd-stpop');
    if (!pop) return;
    const current = S.settings.quality_hold_destination || '';
    pop.innerHTML = '';
    showPop(pop, btn);
    const field = { rule: n => B().legExclusion(S.coreNodes, n.name, 'destination') };
    mountPicker(pop, NODES, nodes => [{ value: '', label: '-- None --' }].concat(nodeRows(nodes, field)), {
        open: true, chips: false, selected: current, placeholder: 'find a node by name',
        onChange: v => {
            closeLayer(pop);
            pop.hidden = true;
            pop.innerHTML = '';
            S.settings.quality_hold_destination = v;
            drawSettings();
        },
    });
    placePopover(pop, btn);
}

// openGroupPicker is the process's group, on the shared picker: Settings'
// Line, or Add process (onPick).
function openGroupPicker(btn, onPick) {
    const inSheet = !!(btn.closest && btn.closest('.pd-modal'));
    const pop = inSheet ? $('pd-advpop') : $('pd-stpop');
    if (!pop) return;
    const current = onPick ? (S.add ? S.add.groupID : 0) : S.settings.group_id;
    const rows = [{ id: 0, name: 'Ungrouped' }].concat(S.groups).map(g => ({ value: String(g.id), label: g.name }));
    openRowPicker(pop, btn, rows, v => {
        const id = Number(v);
        if (onPick) { onPick(id); return; }
        S.settings.group_id = id;
        drawSettings();
    }, { selected: String(current || 0), placeholder: 'find a group' });
}

// deleteGoes is what a process's delete takes with it: a Core loader's
// process has its screens and positions, and nothing of the rest.
function deleteGoes() {
    return coreBoards(process()) ? 'its screens and positions go with it'
        : 'its styles, flows, routing set and screens go with it';
}

function openDeleteProcess() {
    const p = process() || {};
    pageSheet('Delete ' + p.name + '?', deleteGoes() + '.',
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

async function syncCatalog() {
    S.settingsError = '';
    S.settingsNotice = '';
    // The sync only asks the Edge to ask Core (apiSyncPayloadCatalog answers
    // ok and never refuses), so what can fail here is this Edge: no answer, a
    // lapsed session, a panic. The door's sentence says which; "(0). Core may
    // be unreachable" named the wrong machine.
    const res = await send('POST', '/api/payload-catalog/sync', null, NO_NOTICE);
    if (!res.ok) {
        S.settingsError = said(res, 'The catalog sync failed: ') + (res.shown ? '' : '.');
    } else {
        S.settingsNotice = 'Catalog synced from Core.';
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
// (P0's pattern) and its two modals are the page's modal shell (.pd-modal), so every
// size and colour here is one the guide already describes. Anything that was
// not already described is a question in the report rather than a decision.

function presetsView() { return (S.presets && S.presets.view) || null; }

// THE RECIPES' HOME IS BESIDE HMI SETUP (F36): openPresets is every door the
// Recipes tab had (#tab=presets, #tab=recipes, its tab click), and opens
// Settings there. Settings reads the recipes view with the rest of its reads.
async function openPresets() {
    S.presets = S.presets || { view: null, error: '', expanded: 0 };
    await openSettingsAt('Operator setup');
}

// A LOGIN PAGE IS NOT "COULD NOT REACH" (P0 bug, :7138). The read parsed the
// login page a lapsed session lands on, threw, and the catch said the Edge
// was unreachable. Through the door it is "Signed out", with the notice's
// sign-in link, and no answer is the door's own sentence.
async function loadPresets() {
    S.presets = S.presets || { view: null, error: '', expanded: 0 };
    const res = await send('GET', '/api/processes/' + S.processID + '/presets', undefined, NO_NOTICE);
    if (!res.ok) { S.presets.error = said(res); return; }
    S.presets.view = res.body;
    S.presets.error = '';
}

// shapeCell is a shape found in the flows, not named yet: the glyph the mode
// draws on every other surface, the mode word, and the positions in PRESS
// order — the order of the picture, not of whichever list the shape arrived
// in, so two shapes over the same positions read as the same positions.
//
// DRAWN FROM THE SHAPE, ALWAYS (owner ruling R5, 2026-09-12). A preset's NAME
// is whatever the engineer typed — the suggestion is the mode word alone now —
// so the positions are never in it, and every surface that shows a preset
// draws them itself: the recipes' Spots column, the strip card's second line
// (service/station_composer.go's presetUse), and the apply modal's title.
// THE POSITIONS COME FROM THE SERVER, IN PRESS ORDER. This sorted them by the
// picture's position index and the apply modal's title sorted them
// lexically — the same preset, two orders, one click apart — so the order is
// computed once where the process's own node sequence lives
// (domain.PresetShapeNodes) and both surfaces print it.
//
// A MIXED SHAPE HAS ITS OWN MARK. The server leaves mode blank when the
// shape's positions disagree, and the glyph for no mode is the empty dashed
// square — the drawing for "nothing here", on a shape that has several things.
// Two overlapped squares say "more than one" without claiming any one mode's
// geometry; composer-glyphs.js ships verbatim, so the mark is this page's.
function shapeCell(mode, where) {
    const label = M().modeLabels()[mode];
    const mark = label ? gl(mode, 22, {})
        : '<span class="pd-mixed" aria-hidden="true"><i></i><i></i></span>';
    const word = label ? '<span class="w">' + esc(label) + '</span>'
        : '<span class="w" title="its positions run different modes">mixed</span>';
    return '<span class="pd-shape">' + mark + word +
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
        '<td class="pn" title="' + esc(p.name) + '"><b>' + esc(p.name) + '</b>\u00a0' +
        '<span class="pd-vtag">v' + (p.version || 1) + '</span></td>' +
        // THE SPOTS, NOT THE MODE: the recipe's name is what tells two recipes
        // apart (a repeated name is the same recipe, a new version), and the
        // spots are what an engineer checks it against the press by.
        '<td class="pd-dim pd-spots" title="' + esc(p.where || '') + '">' + esc(p.where || '') + '</td>' +
        // APPLIED TO SAYS ITS DRIFT: "6 parts · 6 drifted", the drift a
        // count of the parts beside it, not a column of its own.
        '<td class="pd-dim">' + members + ' part' + (members === 1 ? '' : 's') +
            (drifted ? ' · <span class="pd-warn">' + drifted + ' drifted</span>' : '') + '</td>' +
        '<td class="pd-dim pd-saved">' + esc(savedWord(p)) + '</td>' +
        // APPLY IS A BUTTON, NOT A MENU ITEM (§3 C5). It is the only thing on
        // this row anybody comes here to DO — Rename and Archive are
        // housekeeping, which is what a ⋯ is for. A control that is the point
        // of the row belongs on the row: this one was a menu item for long
        // enough that a round where the menu itself did not open left the whole
        // apply flow unreachable by mouse (see PRESET_MENU's note), and a
        // discoverable control cannot fail that way.
        '<td class="pd-acts"><button class="pd-btn quiet" data-act="preset-apply" data-preset="' +
        p.id + '">Apply to parts…</button>' +
        '<button class="pd-dimlink" data-act="preset-menu" data-preset="' +
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
    return head + '<tr class="pd-memrow"><td colspan="5">' + (members ? rows
        : '<p class="pd-dim">Not applied to any part yet. Saving a preset from a flow does not ' +
        'apply it to that flow; Apply to parts… does.</p>') +
        '</td></tr>';
}

function candidateRow(c) {
    const names = c.member_names || [];
    const sub = names.slice(0, 4).join(', ') + (names.length > 4 ? ' and ' + (names.length - 4) + ' more' : '');
    // THE MODE AND THE POSITIONS (owner ruling R5, 2026-09-12): a shape not
    // named yet has nothing else to tell it by. The suggested name is the mode
    // word alone, so a line that drew only the name would offer `2‑Robot Swap`
    // with nothing to say WHICH swap — and two candidate shapes of one mode
    // would be two identical lines. The positions have a column of their own,
    // so they wrap rather than being cut; the parts that run it follow.
    return '<div class="pd-cand" data-cand="' + esc(c.shape_key) + '">' + shapeCell(c.mode, '') +
        '<span class="pd-cand-at">' + esc(c.where || '') + '</span>' +
        '<span class="pd-dim" title="' + esc(names.join(', ')) + '">' + esc(sub) + '</span>' +
        '<button class="pd-dimlink" data-act="preset-name-cand" data-cand="' +
        esc(c.shape_key) + '">Name it…</button></div>';
}

// drawPresets redraws the recipes where they are drawn: Settings, whose
// notice line is its own (a recipe write's "Saved to 2 parts." lands there).
function drawPresets() {
    if (S.tab !== 'settings') return;
    if (S.notice) { S.settingsNotice = S.notice; S.notice = ''; }
    drawSettings();
}

// hmiRecipes is Operator setup's recipes, under its switch on Settings (F36):
// the recipes with their Apply to parts… and ⋯, and "Found in your flows" —
// what the Recipes tab held, in the place the feature is switched on.
function hmiRecipes() {
    return '<div class="pd-hmirecipes" id="pd-hmirecipes">' + recipesHTML() +
        '<div class="pd-pop" id="pd-pop" hidden></div></div>';
}

// newestRecipes is each recipe name's newest version: a repeated name is the
// same recipe changing (ErrFlowPresetNameIsAnotherShape), and its older
// versions are what claims made from them still point at.
function newestRecipes(presets) {
    const top = {};
    for (const p of presets) if (!top[p.name] || (p.version || 1) > (top[p.name].version || 1)) top[p.name] = p;
    return presets.filter(p => top[p.name] === p);
}

// CAND_FOLD is how many found shapes Operator setup draws before "N more".
const CAND_FOLD = 3;

function recipesHTML() {
    if (S.presets && S.presets.error) return '<div class="pd-refusal">' + esc(S.presets.error) + '</div>';
    const v = presetsView();
    if (!v) return '';
    const presets = v.presets || [];
    const newest = newestRecipes(presets);
    const older = presets.length - newest.length;
    const shown = S.presets.older ? presets : newest;
    const cands = v.candidates || [];
    // THE COLUMNS ARE SIZED, the way D1's positions table is, and for the same
    // reason: left to itself the browser gave Name a third of what its content
    // needs and the spots less than their list. THE ACTIONS COLUMN HOLDS A
    // BUTTON, not just a ⋯, so it gets the room for one: the table is
    // `table-layout: fixed`, so a cell does not grow to its contents, it cuts
    // them.
    const presetCols = '<colgroup><col style="width:32%"><col style="width:20%">' +
        '<col style="width:16%"><col style="width:14%"><col style="width:18%"></colgroup>';
    const table = presets.length
        ? '<table class="pd-tbl pd-presettbl">' + presetCols +
        '<thead><tr><th>Recipe</th><th>Spots</th><th>Applied to</th><th>Saved</th><th></th></tr></thead><tbody>' +
        shown.map(presetRow).join('') + '</tbody></table>' +
        (older ? '<p class="pd-dim">' + (S.presets.older ? 'every version'
            : esc(nOf(older, 'older version'))) + ' · <button class="pd-dimlink" data-act="preset-older">' +
            (S.presets.older ? 'newest only' : 'show') + '</button></p>' : '')
        // ON WITH NO RECIPE IS SAID (F36): the operator's SET UP A PART then
        // has nothing to set a part up from.
        : '<span class="pd-cnt' + (hmiSetupFacts().warn ? ' warn' : '') + '"><i></i>no recipe yet</span>';
    const names = newest.length;
    let h = stBlock('Recipes', names ? nOf(names, 'recipe') + ' · newest version of each' : '', table);
    // THE OFFER, while there is one. Once every shape the press runs has a
    // name it is finished — and a row over nothing would read as a thing
    // still to do.
    //
    // THREE, THEN THE REST ON ASK (owner, 2026-11-14), as the recipes fold
    // their older versions: ten found shapes on a big line were ten rows over
    // Auto-reorder, the wall this page exists to take down.
    if (cands.length) {
        const fold = cands.length > CAND_FOLD;
        const shownCands = fold && !S.presets.cands ? cands.slice(0, CAND_FOLD) : cands;
        h += stBlock('Found in your flows', 'a shape run by styles, not named yet', shownCands.map(candidateRow).join('') +
            (fold ? '<p class="pd-dim">' + (S.presets.cands ? 'all ' + cands.length : (cands.length - CAND_FOLD) + ' more') +
                ' · <button class="pd-dimlink" data-act="cand-more">' + (S.presets.cands ? 'first ' + CAND_FOLD + ' only' : 'show') +
                '</button></p>' : ''));
    }
    // THE ROW MENU NEEDS A POPOVER TO OPEN INTO: hmiRecipes draws #pd-pop
    // beside these rows (Settings' own is #pd-stpop).
    return h;
}

// openUsePreset lists this cell's named shapes for the style currently open.
//
// FROM THE COMPOSER BLOCK, not the Presets tab's read: S.composer.presets is
// already loaded (the picture, the routing set and the strip cards come from
// it), and asking the other endpoint would be a second list of the same shapes
// that could disagree about which versions are live.
function openUsePreset(btn) {
    const pop = $('pd-pop');
    if (!pop || !S.model) return;
    const presets = S.composer.presets || [];
    if (!presets.length) {
        pop.innerHTML = '<div class="none">This cell has no recipes yet. Build one from + New recipe ' +
            'in the rail, or save a style as one — Save as recipe….</div>';
        showPop(pop, btn);
        placePopover(pop, btn);
        return;
    }
    openRowPicker(pop, btn, presets.map(p => ({ value: String(p.id), label: p.name, note: p.where || '' })),
        id => usePreset(id), { placeholder: 'find a recipe' });
}

// usePreset applies a shape to the OPEN draft, after showing what it would
// change — the same preview-before-write the apply modal gives, on one style
// instead of a ticked list.
//
// NOTHING IS WRITTEN HERE. It changes the draft, exactly as every picker on
// this page does, and Save style is still the only door onto the server. That
// is what makes this a CALLER rather than a second apply: the write path is
// unchanged. It stamps NO provenance — saveFlow sends no source_preset_id;
// only the apply modal's rows do (desktop-bodies flowPresetApplySave).
//
// THE SPOTS ARE PRE-FILLED where the last style that used this recipe on this
// cell has them: the card's last_moves as shipped, which the server keeps to
// what the Spots step allows (domain.LegalSpotMoves) — the same pre-fill the
// HMI opens on. The recipe's own nodes otherwise. The positions table it lands
// on is the pick step: every spot is a picker, and nothing is written until
// Save style.
//
// THE SHEET DRAWS THE SHAPE ON THE RELATION PICTURE (SYNTH-round2 row 17),
// where the pre-fill puts it, before the lines that say what changes.
function usePreset(id) {
    const card = (S.composer.presets || []).find(x => String(x.id) === String(id));
    if (!card || !S.model) return;
    const moves = card.last_moves || {};
    const p = M().presetMoved(card, moves);
    const diff = M().shapeDiff(S.model, p);
    const lines = recipePicture(card, moves, { id: 'pd-usvg' }) + (diff.length
        ? diff.map(d => '<div class="pd-diffrow"><b>' + esc(d.node) + '</b> · ' + esc(d.label) +
            (d.whole ? '' : ': ' + esc(d.from) + ' → ' + esc(d.to)) + '</div>').join('')
        : '<p class="pd-note">This flow already has that shape — applying it changes nothing.</p>');
    pageSheet('Use “' + p.name + '” on this style?',
        'the spots only — a recipe carries no parts, and the parts you have placed stay where they are.' +
            (Object.keys(moves).length ? ' Its spots are where this cell last ran it; change any in the table before you save.' : ''),
        lines, 'Apply to the draft', () => {
            apply({ type: 'applyPreset', preset: p });
            closeSheet();
        });
}

// ── the recipe draft, in the positions table (pick-a-part U2) ───────────────
//
// A MODE OF D1, NOT A NEW EDITOR. The draft is a model with no style and no
// part (init's `shape`), a recipe's cells applied to it or none, and the
// positions table drawn over it with the Part column hidden. Paired, Staging,
// source, destination and Role are the table's own pickers. Save is the naming
// modal and the shape lands as a recipe; the name is the engineer's.

// startShapeDraft puts a shape in the positions table as a draft — an empty
// one for + New recipe, a recipe's own to change it. draft is S.shapeDraft less
// `back`: {recipe, groups} for a recipe.
function startShapeDraft(shape, draft) {
    const back = S.styleID;
    const from = S.tab;
    S.tab = 'flows';
    S.styleID = 0;
    S.selected = null;
    S.notice = '';
    S.model = M().reduce(initModel({ id: 0, name: 'New shape', claims: [], parts: [] }, { shape: true }),
        { type: 'applyPreset', preset: { shape: shape } });
    S.baseline = '';
    // groups is the draft's cards (null until Set up groups), focus the
    // clicked row or pinned group, hover the card under the pointer, and
    // refusal the server's last word on a save of them.
    S.shapeDraft = Object.assign({ groups: null, focus: null, hover: null, refusal: '' },
        draft, { back: back, fromTab: from });
    S.setAdds = {};
    S.shapeDraft.baseline = draftKey();
    drawFlows();
}

// A RECIPE DRAFT HAS ITS OWN "UNSAVED" (F5): its cells and its group cards as
// it opened, so an untouched draft reads "No unsaved changes" and Back asks
// only when there is something to lose (F4).
function draftKey() {
    return JSON.stringify([M().toCells(S.model), (S.shapeDraft && S.shapeDraft.groups) || null]);
}
function draftDirty() { return !!S.shapeDraft && !!S.model && draftKey() !== S.shapeDraft.baseline; }

// draftHold puts the question before anything that leaves a changed recipe
// draft (F4), and answers true when it asked. On Discard the act runs again,
// the draft counted clean, so it leaves; Keep editing leaves it as it was.
// The question is the layer's own for typed work (style guide §10).
function draftHold(again) {
    if (!draftDirty()) return false;
    void layerConfirm('Discard the changes?', { ok: 'Discard', cancel: 'Keep editing' }).then(yes => {
        if (!yes || !S.shapeDraft) return;
        S.shapeDraft.baseline = draftKey();
        again();
    });
    return true;
}

// draftGaps is a recipe draft's required cells left empty, in table order
// (F5, F33). A style's bar has the server's preview to say them; a recipe is
// not previewed, so the page reads flowspec's own word for each field
// (rowColumns' `required`, fieldRequired) — the same that draws the cell red.
function draftGaps() {
    const out = [];
    for (const pos of S.model.positions) {
        const n = pos.core_node_name, c = S.model.cells[n];
        if (!c || !c.on || !c.mode) continue;
        for (const col of ['partner', 'staging']) {
            for (const chip of M().rowColumns(S.model, n, col)) {
                if (chip.required && !chip.value) out.push({ node: n, message: n + ' · ' + chip.label + ' is required' });
            }
        }
        for (const [field, key] of [['inbound_source', 'source'], ['outbound_destination', 'dest']]) {
            if (!c[key] && M().fieldRequired(S.model, n, field)) out.push({ node: n, message: n + ' · ' + W(field) + ' is required' });
        }
    }
    return out;
}

// leaveShapeDraft goes back to where the draft was opened from, with the style
// that was open before it re-selected underneath so Styles is where it was;
// given `to`, it lands on that tab instead (a tab out of the draft, F4).
async function leaveShapeDraft(to) {
    const back = S.shapeDraft ? S.shapeDraft.back : S.styleID;
    const fromTab = S.shapeDraft ? S.shapeDraft.fromTab : '';
    S.shapeDraft = null;
    // A failed read: the rail catches up on its next read.
    const res = await send('GET', composerURL(S.processID));
    if (res.ok) takeComposer(res.body);
    selectStyle(back);
    if (to) showTab(to);
    else if (fromTab === 'settings') await openSettingsAt('Operator setup');
    else if (fromTab === 'setup') { S.tab = 'setup'; drawSetup(); syncHash(); }
}

function presetByID(id) { return (presetsView() ? presetsView().presets : []).find(p => p.id === id) || null; }
function candidateByKey(k) { return (presetsView() ? presetsView().candidates : []).find(c => c.shape_key === k) || null; }

// TWO ITEMS. `Apply to parts…` left this menu for the row itself (§3 C5): it is
// the only thing on the row anybody comes to do. What is left is housekeeping,
// which is what a ⋯ is for.
//
// Rename was absent until owner ruling R6 (2026-09-12) settled what it means:
// PATCH the name on every version of it and nothing else, so a lineage keeps
// one name and a member's source_preset_id keeps pointing at the same version
// of the same shape. Both ways of faking it were worse than the absence —
// re-creating under the new name and archiving the old row aims every member's
// provenance at an archived preset, and a version n+1 under a new name leaves
// the old name live on a different shape.
const PRESET_MENU = [
    ['rename', 'Rename'],
    ['archive', 'Archive'],
];

function openPresetMenu(btn) {
    const id = Number(btn.dataset.preset);
    const pop = $('pd-pop');
    if (!pop) return;
    pop.innerHTML = PRESET_MENU.map(m => '<button data-act="preset-' + m[0] + '" data-preset="' + id + '">' +
        esc(m[1]) + '</button>').join('');
    showPop(pop, btn);
    placePopover(pop, btn);
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
    pageSheet(title, '',
        sheetField('Name', 'what this recipe is called on this process', 'name', prefill) +
        '<p class="pd-note">A recipe is the spots of a style — which positions, how they swap, ' +
        'where bins come from and go. Parts are never part of it.</p>',
        'Save recipe',
        () => {
            const name = sheetValue('name');
            if (from && from.groups) return saveGroupedRecipe(name, from);
            return sheetSubmit('POST', '/api/processes/' + S.processID + '/presets',
                B().flowPresetCreate(name, from), async () => {
                    // A NAMED DRAFT IS DONE: back to the list it now sits in.
                    if (from && from.cells) {
                        S.notice = 'Saved recipe ' + name + '.';
                        await leaveShapeDraft();
                        return;
                    }
                    // Named from the Flows tab, the preset lands somewhere
                    // else, and the sentence says where; on Presets it is in
                    // the list under the notice.
                    S.notice = 'Saved recipe ' + name + '.' +
                        (S.tab === 'settings' ? '' : ' It is in the rail under Recipes.');
                    await refreshPresets();
                    if (S.tab === 'flows') drawFlows();
                });
        },
        false);
}

// saveGroupedRecipe is the naming sheet's Save for a draft with groups: the
// cells form of the create, with the groups beside the cells
// (flowPresetCreate(name, {cells, groups}) — a style's or a found shape's form
// carries none, and the server refuses groups on them).
//
// A REFUSAL IS THE SERVER'S SENTENCE, on the sheet and beside the cards: the
// save checks the groups by name (domain.ValidateRecipeGroups) and its words
// say which node and which group, which is what the engineer fixes on the
// card under the sheet.
//
// A SAVED GROUPED RECIPE STAYS OPEN, at the version just saved, so its "On
// the floor" block reads the server's card of it: what the floor can now pick
// is the answer to the save. The way back is the draft's own Back.
async function saveGroupedRecipe(name, from) {
    const d = S.shapeDraft;
    const out = await send('POST', '/api/processes/' + S.processID + '/presets', B().flowPresetCreate(name, from), NO_NOTICE);
    if (!out.ok) {
        sheetStatus(said(out), true);
        if (d && S.shapeDraft === d) { d.refusal = said(out); drawFlows(); }
        return;
    }
    closeSheet();
    const keep = d ? { back: d.back, fromTab: d.fromTab } : null;
    // A failed read: the rail catches up on its next read.
    const read = await send('GET', composerURL(S.processID));
    if (read.ok) takeComposer(read.body);
    const id = String(bodyOf(out).id || '');
    if (id && (S.composer.presets || []).some(p => String(p.id) === id)) {
        openRecipe(id);
        if (S.shapeDraft && keep) Object.assign(S.shapeDraft, keep);
        S.notice = 'Saved recipe ' + name + '.';
        drawFlows();
        return;
    }
    S.notice = 'Saved recipe ' + name + '.';
    await leaveShapeDraft();
}

async function refreshPresets() {
    await loadPresets();
    drawPresets();
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
    // THROUGH styleFacts, because applyBlocker below reads `parts` and the
    // composer block does not carry it — see composerStyle.
    const withFacts = S.composer.styles.map(st => Object.assign({}, st, M().styleFacts(st)));
    return B().applyOrder(withFacts, process() ? process().active_style_id : 0);
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
    // The modal redraws itself whole on every tick and pick; the first draw
    // puts the scrim on the layer and every later one gets the same entry.
    const h = showSheet(undefined, why => sheetGone(h, why));
    S.sheet = { run: runPresetApply, handle: h };
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
// THE TICK IS A TOGGLE; RE-PREVIEWING IS NOT. They were one function, and the
// 409 retry called it on a row that is ticked by definition (applyRowReady
// requires it) — so the retry's first act was to DELETE the row it was about
// to rebuild. Everything after that read undefined: the fresh row was missing,
// the else arm recorded a failure, and the refusal was written onto an object
// no longer in S.papply.rows, where nothing renders it. On a 409 the row's tick
// silently cleared itself and the footer said "1 to look at" with no row
// saying which or why — and the retry the code advertises never happened.
async function tickApplyRow(styleID) {
    const r = S.papply.rows[styleID] || {};
    if (r.ticked) { delete S.papply.rows[styleID]; drawPresetApply(); return; }
    return buildApplyRow(styleID);
}

// Build (or rebuild) one row's model, diff and preview. The tick calls it to
// turn a row on; the retry calls it to get a fingerprint that is true again.
async function buildApplyRow(styleID) {
    if (!S.papply) return;
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
    if (!S.papply) return;
    const row = S.papply.rows[styleID];
    if (!row || !row.model) return;
    row.cells = M().toCells(row.model);
    // A PREVIEW STARTS CLEAN. A row that failed once and is previewed again
    // after a pick would otherwise keep the old refusal under a new answer.
    row.error = '';
    // The row says its preview's refusal by name, the door's sentence when
    // the server gave none (answerBody), so it passes NO_NOTICE.
    const res = await send('POST', '/api/processes/' + S.processID + '/flow/preview',
        { to_style_id: styleID, cells: row.cells }, NO_NOTICE);
    const j = answerBody(res);
    row.loading = false;
    row.fingerprint = j.fingerprint || '';
    // THE PREVIEW'S FINDINGS BELONG ON THE ROW, not on the save that comes
    // after it. Measured on the HK fixture: applying `PLN_01 / PLN_04` to a
    // part running `PLN_03 / PLN_06` lands two cells with no part on them,
    // and the preview says so twice — `PLN_01 · Select a part`,
    // `PLN_04 · Select a part` — while planning two real press-index
    // swaps whose supply legs carry no payload_code at all. The modal drew
    // `2 orders after the change` over `Save to 1 part`, and the engineer
    // learned the rest from a 422 AFTER pressing it.
    //
    // A ROW WITH FINDINGS IS NOT SAVED (owner ruling F1, 2026-09-12).
    // R8's "an apply that leaves parts unplaced saves" stands — it is the
    // PART with no position that saves, and the row says so. What may not
    // happen is the modal offering a save the server refuses, so a row
    // still carrying a finding keeps its tick and stays out of the count
    // (applyRowReady), and the question it can answer it asks. A warning
    // is not one: the server saves past it (two positions staged at one
    // spot), and the row's drawing shows it.
    row.findings = (j.findings || []).filter(f => f.core_node_name && f.severity !== 'warning');
    // NULL, NOT ZERO, when the body carried no count: a preview of the
    // running style is not planned (R3) and a zero there would read as a
    // changeover that moves nothing.
    row.orders = typeof j.order_count === 'number' ? j.order_count : null;
    // A refusal is shown BY NAME on the row it belongs to, and leaves the
    // row ticked-but-unsavable rather than quietly unticking itself.
    if (!res.ok || j.error) row.error = j.error || 'The server refused this preview (' + res.status + ').';
    else if (!row.fingerprint) row.error = 'No fingerprint came back — nothing to save against.';
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
    // THE RUN ENDS HOWEVER IT ENDS (P0 bug, :7895 + runSheet's :5917). A save
    // that got no answer rejected out of this loop with S.papply.running still
    // true: runSheet's finally gave the OK back and every press after that
    // returned at the guard above, under "Saving…", sending nothing. The saves
    // go through the door now, which does not throw, and the finally gives the
    // run back whatever else might.
    try {
        for (const st of applyStyles()) {
            // CANCEL STOPS THE LOOP. closeSheet sets S.papply to null and the
            // modal's Cancel is live while this runs, so every read after an await
            // was a TypeError waiting to happen — and the loop would have carried
            // on writing parts into a modal that is gone, with no report of which
            // ones landed. The engineer's cancel means stop.
            if (!S.papply) return;
            const row = S.papply.rows[st.id];
            // THE SAME TEST THE BUTTON COUNTED (F1). Two answers to "can this row
            // be saved" is one of them being wrong, and the wrong one here is a
            // write the button did not promise.
            if (!applyRowReady(row)) continue;
            const res = await send('POST', '/api/processes/' + S.processID + '/flow/save',
                B().flowPresetApplySave(st.id, row.cells, row.fingerprint, stationIDForSave(), { id: p.id, version: p.version }), NO_NOTICE);
            // What the response means for this row — B().applyOutcome, where the
            // rule lives and is tested.
            let out = B().applyOutcome(res.status, answerBody(res), 1);
            if (out.retry) {
                // Re-preview THIS row first, so the modal ends up showing a diff
                // that is true rather than one the save already disagreed with,
                // and so the retry carries the fingerprint that preview returned.
                row.result = { ok: false, text: out.text };
                drawPresetApply();
                await buildApplyRow(st.id);
                if (!S.papply) return;   // cancelled while the preview was in flight
                const fresh = S.papply.rows[st.id];
                if (fresh && applyRowReady(fresh)) {
                    const again = await send('POST', '/api/processes/' + S.processID + '/flow/save',
                        B().flowPresetApplySave(st.id, fresh.cells, fresh.fingerprint, stationIDForSave(), { id: p.id, version: p.version }), NO_NOTICE);
                    out = B().applyOutcome(again.status, answerBody(again), 2);
                } else {
                    out = { ok: false, text: out.text };
                }
            }
            if (!S.papply) return;
            const target = S.papply.rows[st.id] || row;
            target.result = { ok: !!out.ok, text: out.text };
            if (out.ok) { saved++; } else { failed++; }
            drawPresetApply();
        }
    } finally {
        if (S.papply && S.papply.running) {
            S.papply.running = false;
            S.papply.status = saved + ' saved' + (failed ? ', ' + failed + ' to look at' : '');
        }
    }
    if (!S.papply) return;
    S.papply.running = false;
    S.papply.status = saved + ' saved' + (failed ? ', ' + failed + ' to look at' : '');
    drawPresetApply();
    // THE TAB'S DRIFT FIGURES ARE COMPUTED FROM TRUTH, and so is every other
    // screen this page holds. refreshPresets alone re-read /presets and left
    // S.composer's claims at their PRE-apply state — so clicking Flows drew
    // the old flow for every part just changed, over "No unsaved changes",
    // and reopening the modal re-ticked to the same full diff for parts that
    // were already in step. Worse, an edit made on that stale draft and saved
    // would have written the whole cell list back and silently reverted the
    // apply. refreshProcess is the one path that re-reads the composer and the
    // styles, and every other write on this page already goes through it; on
    // Settings, the recipes' home, it re-reads /presets too.
    await refreshProcess();
    // EVERY PART SAVED, SO THE DIALOG HAS NOTHING LEFT TO SAY. It stayed open
    // on `N saved` over rows that were all done, and the engineer closed it by
    // hand to see the result. A refusal keeps it open: the refused row says
    // why, and that is the one place it is said.
    if (S.papply && !failed && saved) {
        closeSheet();
        S.notice = 'Saved to ' + saved + ' part' + (saved === 1 ? '' : 's') + '.';
        drawPresets();
        return;
    }
    if (S.papply) drawPresetApply();
}

// ── events ───────────────────────────────────────────────────────────────────
// The acts that open #pd-pop. See the note in onClick. The SETTINGS tab's two
// popover acts are here too ('st-group', 'st-holddest'): they open into
// #pd-stpop, and the document-level closer does not care which popover — a
// click that does not stop propagating opens the list and hides it before
// paint, which is the "clickable, nothing happens" report.
const POPOVER_ACTS = { pick: 1, 'style-menu': 1, 'preset-menu': 1, 'add-position': 1, 'st-group': 1, 'st-holddest': 1, 'use-preset': 1,
    'gspot-add': 1, 'glist-add': 1, 'cu-add': 1, 'ca-add': 1, 'set-add': 1, 'set-addpos': 1, 'set-pick': 1, 'ca-pick': 1, 'st-plc': 1, 'ch-pin-pick': 1, 'fs-part': 1,
    'fs-addpart': 1, 'fs-pick': 1, 'fs-addpos': 1 };

// hashTab is the tab a #tab= names, or '' for none this page draws. No tab is
// Styles. The Recipes tab's hash still says presets (F23); recipes names it
// too, so a link in either word lands.
function hashTab(t) {
    if (!t) return 'flows';
    if (t === 'recipes') return 'presets';
    // The tab reads Styles; its id is flows. #tab=styles typed on an open
    // process went nowhere (it is no id).
    if (t === 'styles') return 'flows';
    // The Operator screens tab is Settings' Screens and positions now; a link
    // to it lands there.
    if (t === 'screens') return 'settings';
    return ['flows', 'setup', 'settings', 'presets'].indexOf(t) >= 0 ? t : '';
}

// showTab is a tab's click, and the address bar's #tab= edited in place.
function showTab(t) {
    S.tab = coreBoards(process()) ? 'settings' : t;
    // A notice is about the tab it was written on, and this is another.
    S.notice = '';
    if (S.tab === 'flows') drawFlows();
    else if (S.tab === 'setup') drawSetup();
    else if (S.tab === 'presets') openPresets();
    else if (S.tab === 'settings') openSettings();
    syncHash();
}

// tabTo is a tab's click. Out of a recipe draft it is a way out, left the way
// Back leaves, asking first when the draft has changed (F4); the tab the draft
// is drawn on is not.
function tabTo(t) {
    // A tab picked by hand is the engineer leaving the walkthrough.
    S.fromSetup = false;
    if (t !== 'setup') { S.setupEnd = 0; S.setupView = ''; stopSetupPoll(); }
    if (!S.shapeDraft || t === S.tab) { showTab(t); return; }
    const go = () => void leaveShapeDraft(t);
    if (!draftHold(go)) go();
}

// A #tab= (or #style=) EDITED IN THE ADDRESS BAR is a hashchange and no load,
// and hashOpts is read only at boot, so the page stayed where it was (F21).
// syncHash's replaceState fires no hashchange, so this runs only for an edit
// or a pasted link. The boot-only one-shots are not re-run here.
function onHashChange() {
    if (!S.processID || !S.composer) return;
    const h = hashOpts();
    const t = hashTab(h.tab);
    if (!t) return;
    if (t === 'flows' && Number(h.style) && Number(h.style) !== S.styleID && composerStyle(Number(h.style))) {
        const go = () => { S.tab = 'flows'; selectStyle(Number(h.style)); syncHash(); };
        // Out of a changed recipe draft it asks, as the rail's row does (F4),
        // and the address bar says where the page is until it is answered.
        if (S.shapeDraft) syncHash();
        if (!draftHold(go)) go();
        return;
    }
    if (t === S.tab) return;
    if (S.shapeDraft) syncHash();
    tabTo(t);
}

function onClick(e) {
    const openRow = e.target.closest && e.target.closest('[data-open]');
    const tab = e.target.closest && e.target.closest('[data-tab]');
    const btn = e.target.closest && e.target.closest('[data-act]');
    const styleRowEl = e.target.closest && e.target.closest('[data-style]');
    const card = e.target.closest && e.target.closest('#pd-svg [data-pos]');
    const row = e.target.closest && e.target.closest('[data-row]');

    // EVERY ACT THAT OPENS #pd-pop HAS TO STOP THE EVENT. onClick is bound to
    // #pd-root and boot() puts a listener on the DOCUMENT that closes the
    // popover on any click outside it — and a click on the button that opens
    // one is, by that test, outside it. So the menu opened during the bubble
    // through #pd-root and was hidden again before paint, one listener later.
    //
    // `pick` had the stopPropagation and the three others did not, which is
    // why the style row's ⋯ — the only door to Rename, Expected CATID, Clone,
    // Mark as running and Delete — has never opened, and why the
    // add-a-position chooser does nothing whenever more than one is free.
    if (btn && POPOVER_ACTS[btn.dataset.act]) e.stopPropagation();
    if (btn && btn.dataset.act === 'pick') { openPicker(btn); return; }
    if (btn && btn.dataset.act === 'bar-goto' && btn.dataset.node) { gotoFinding(btn.dataset.node); return; }
    if (btn) {
        switch (btn.dataset.act) {
            case 'list': if (draftHold(drawList)) return; drawList(); return;
            case 'ch-pin': catidPin(btn.dataset.pid, btn.dataset.style, btn.dataset.catid, btn.dataset.name); return;
            case 'ch-pin-pick': openCatidPinPick(btn); return;
            case 'ch-repoint': catidRepoint(btn.dataset.pid, btn.dataset.tag); return;
            case 'ch-charge': catidCharge(btn.dataset.hold); return;
            case 'ch-writeoff': catidWriteOff(btn.dataset.hold); return;
            case 'ch-start': catidStartChangeover(btn.dataset.pid, btn.dataset.style, btn.dataset.name); return;
            case 'save': saveFlow(); return;
            case 'discard': selectStyle(S.styleID); return;
            // Set for this style, and a group's list (see setList).
            case 'set-add': openSetAdd(btn, btn.dataset.g !== undefined ? { g: Number(btn.dataset.g) } : null); return;
            case 'set-addpos': openSetPosition(btn, btn.dataset.key); return;
            case 'set-pick': openSetPick(btn, btn.dataset.key, setPlace(btn)); return;
            case 'set-rm': setRemove(btn.dataset.key, setPlace(btn)); return;
            case 'set-seg': keepRow(btn.dataset.key, setPlace(btn)); setWrite(setPlace(btn), btn.dataset.key, btn.dataset.v); return;
            case 'set-unchip': {
                const p = setPlace(btn);
                keepRow(btn.dataset.key, p);
                setWrite(p, btn.dataset.key, (advValue(btn.dataset.key, setCtx(p)) || []).filter(v => v !== btn.dataset.v));
                return;
            }
            case 'set-goto': {
                const el = $('pd-setfor');
                if (el && el.scrollIntoView) el.scrollIntoView({ block: 'start' });
                return;
            }
            case 'rmpart': apply({ type: 'removePart', payloadCode: btn.dataset.part }); return;
            case 'rmpos': apply({ type: 'removePosition', node: btn.dataset.node }); return;
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
            case 'st-toggle':
                S.settings[btn.dataset.st] = !S.settings[btn.dataset.st];
                drawSettings();
                return;
            case 'st-arm':
                S.settings.changeover_auto_arm = btn.dataset.arm;
                drawSettings();
                return;
            case 'cu-add': openCurtainAdd(btn); return;
            case 'cu-off': {
                const d = S.curtain[btn.dataset.node];
                if (d) d.enabled = false;
                drawSettings();
                return;
            }
            case 'ca-add': void openCountedAdd(btn); return;
            case 'ca-off': setCountedAt(Number(btn.dataset.node), '0'); return;
            case 'ca-pick': openCountedPick(btn, Number(btn.dataset.node)); return;
            case 'st-plc': openPLCPick(btn); return;
            case 'cu-val': {
                const d = S.curtain[btn.dataset.node];
                if (d) d.safe = btn.dataset.val === 'true';
                drawSettings();
                return;
            }
            case 'ar-toggle': S.arLine = !S.arLine; drawSettings(); return;
            case 'loose-onto': S.looseOnto = btn.dataset.station || ''; settingsSetMoved(); return;
            case 'work-goto': gotoFinding(btn.dataset.node); return;
            case 'preset-older':
                S.presets.older = !S.presets.older;
                drawPresets();
                return;
            case 'free-toggle':
                S.showFree = !S.showFree;
                drawFlows();
                return;
            case 'cand-more':
                S.presets.cands = !S.presets.cands;
                drawPresets();
                return;
            case 'st-jump': {
                const h = settingsHeading(btn.dataset.to);
                if (h && h.scrollIntoView) h.scrollIntoView({ block: 'start' });
                return;
            }
            case 'st-group': openGroupPicker(btn); return;
        case 'st-holddest': openContainmentPicker(btn); return;
            case 'st-ident': {
                const mode = btn.dataset.mode;
                S.settings.identity_none = mode === 'none';
                S.identityOverride = mode === 'override' ? S.settings : null;
                if (mode !== 'override') S.settings.identity_tag_name = '';
                drawSettings();
                return;
            }
            case 'sc-new': openStationCounter(0, 0); return;
            case 'sc-edit': openStationCounter(Number(btn.dataset.sc), 0); return;
            case 'sc-delete': {
                const id = Number(btn.dataset.sc);
                pageSheet('Remove this station counter?', 'its positions are counted by the production counter again', '',
                    'Remove', () => sheetSubmit('DELETE', '/api/station-counters/' + id, undefined, afterStationChange), true);
                return;
            }
            case 'st-sync': syncCatalog(); return;
            case 'st-delete': openDeleteProcess(); return;
            case 'bind-loader': bindLoaderBoard(btn.dataset.loader); return;
            case 'screen-add': void openScreenSheet(0); return;
            case 'screen-edit': void openScreenSheet(btn.dataset.station); return;
            case 'style-menu': openStyleMenu(btn); return;
            // D6's own actions.
            case 'preset-menu': openPresetMenu(btn); return;
            case 'use-preset': openUsePreset(btn); return;
            case 'preset-apply': closePop(); openPresetApply(Number(btn.dataset.preset)); return;
            case 'preset-archive': {
                closePop();
                const p = presetByID(Number(btn.dataset.preset));
                if (!p) return;
                pageSheet('Archive ' + p.name + ' v' + (p.version || 1) + '?',
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
                pageSheet('Rename ' + p.name,
                    'every version of this name is renamed; nothing else changes.',
                    sheetField('Name', 'what this shape is called on this process', 'name', p.name) +
                    '<p class="pd-note">A preset is named once and read for months. The shape, its ' +
                    'versions and the parts that came from it are untouched — a member points at a ' +
                    'version by id, and the id does not move.</p>',
                    'Rename',
                    () => sheetSubmit('PATCH', '/api/processes/' + S.processID + '/presets/' + p.id,
                        { name: sheetValue('name') }, refreshPresets),
                    false);
                return;
            }
            case 'shape-cancel': if (draftHold(() => void leaveShapeDraft())) return; void leaveShapeDraft(); return;
            // THE NAME IS THE ENGINEER'S (pick-a-part U2): the field opens
            // empty, never on the mode word — "Standard", "Tooling change".
            case 'shape-save': {
                const r = S.shapeDraft && S.shapeDraft.recipe;
                syncGroups();
                const groups = S.shapeDraft && S.shapeDraft.groups ? groupsBody(S.shapeDraft.groups) : null;
                openPresetNaming(r ? 'Save recipe ' + r.name : 'Name this recipe', r ? r.name : '',
                    groups ? { cells: M().toCells(S.model), groups: groups } : { cells: M().toCells(S.model) });
                return;
            }
            // The recipe groups' cards (see groupsSection).
            case 'groups-setup': setUpGroups(); return;
            case 'gspot-add': addGroupSpot(btn, Number(btn.dataset.g)); return;
            case 'gspot-rm': dropGroupSpot(Number(btn.dataset.g), btn.dataset.node); return;
            case 'glist-add': addGroupList(btn, Number(btn.dataset.g), btn.dataset.node, btn.dataset.field); return;
            case 'glist-rm': dropGroupList(Number(btn.dataset.g), btn.dataset.node, btn.dataset.field, btn.dataset.v); return;
            case 'gfocus': pinGroup(Number(btn.dataset.g)); return;
            case 'recipe-open': {
                const id = btn.dataset.preset;
                if (draftHold(() => openRecipe(id))) return;
                openRecipe(id);
                return;
            }
            case 'new-recipe': if (draftHold(() => void openNewRecipe())) return; void openNewRecipe(); return;
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
                if (st) openPresetNaming('Save ' + st.name + ' as a recipe',
                    suggestedPresetName(), { styleID: st.id });
                return;
            }
            case 'style-rename': case 'style-catid': case 'style-clone':
            case 'style-running': case 'style-delete':
                styleAction(btn.dataset.act.slice('style-'.length), Number(btn.dataset.style));
                return;
            case 'sheet-cancel': closeSheet(); return;
            case 'sheet-ok': runSheet(); return;
            case 'open-flows': openProcess(Number(btn.dataset.process)); return;
            case 'open-settings': openProcessAt(Number(btn.dataset.process), 'settings'); return;
            case 'rs-all-on': rsAllOn(btn.dataset.role); return;
            case 'add-process': openAddProcess(); return;
            case 'add-group': openNewGroup(); return;
            // + New style's Create opens the new style, so out of a recipe
            // draft it leaves the draft first, asking when it changed (F4).
            case 'new-style': {
                if (!S.shapeDraft) { openNewStyle(); return; }
                const go = () => void leaveShapeDraft('flows').then(() => openNewStyle());
                if (!draftHold(go)) go();
                return;
            }
            case 'add-position': addFreePosition(btn); return;
            case 'setup-edit': void setupGo('edit'); return;
            case 'setup-styles': void setupGo('styles'); return;
            case 'setup-style': void setupGo('style'); return;
            case 'setup-onto': void setupGo('onto'); return;
            case 'setup-skip': void setupGo('skip'); return;
            case 'setup-show': void setupGo('show'); return;
            case 'setup-later': void openSettingsAt(btn.dataset.to); return;
            // The open position's swap and role are its own copy.
            case 'fs-mode': S.firstStyle.own[S.firstStyle.cur].mode = btn.dataset.mode; firstStyleChanged(); return;
            case 'fs-role': S.firstStyle.own[S.firstStyle.cur].role = btn.dataset.role; firstStyleChanged(); return;
            case 'fs-part': openFirstPart(btn); return;
            case 'fs-addpart': void openFirstAddPart(btn); return;
            case 'fs-pick': openFirstPick(btn); return;
            case 'fs-open': openFirstPosition(btn.dataset.node); S.firstWarn = ''; drawSetup(); return;
            case 'fs-done': firstDone(); return;
            case 'fs-out': firstOut(); return;
            case 'fs-addpos': openFirstAddPosition(btn); return;
            case 'fs-takeoff': firstTakeOff(); return;
            case 'fs-table': openFirstTable(); return;
            case 'fs-back': S.setupView = ''; drawSetup(); return;
            case 'fs-save': void saveFirstStyle(); return;
            default: break;
        }
    }
    if (tab) { tabTo(tab.dataset.tab); return; }
    // A preset row expands its member list; a second click closes it.
    const presetRowEl = e.target.closest && e.target.closest('[data-preset]');
    if (presetRowEl && !btn && S.tab === 'settings') {
        const id = Number(presetRowEl.dataset.preset);
        S.presets.expanded = S.presets.expanded === id ? 0 : id;
        drawPresets();
        return;
    }
    if (openRow) { openProcess(Number(openRow.dataset.open)); return; }
    if (styleRowEl && !btn) {
        const id = Number(styleRowEl.dataset.style);
        const go = () => { S.tab = 'flows'; selectStyle(id); syncHash(); };
        if (!draftHold(go)) go();
        return;
    }
    // A group's row pins that spot on the picture; a second click lets go.
    const grow = e.target.closest && e.target.closest('[data-grow]');
    if (grow) { pinGroupRow(Number(grow.dataset.g), grow.dataset.grow); return; }
    // A card and its row are one selection. The desktop picture never edits:
    // a card click finds the row (the bar's problem line's own path), and a
    // second click on the selected card or row lets go of it.
    if (card) { toggleSelected(card.dataset.pos); return; }
    if (row) { toggleSelected(row.dataset.row); return; }
}

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
else boot();

export { boot };
