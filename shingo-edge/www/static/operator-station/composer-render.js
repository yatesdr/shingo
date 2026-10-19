// composer-render.js — the station's changeover modal.
//
// TWO WAYS IN, BY THE PROCESS'S "HMI SETUP" SWITCH (flow_composer_enabled).
//
// Off: CHANGEOVER lists the styles an engineer made — tap one, check
// what it does, Start. The floor sets nothing up, and the server refuses a
// flow write from it (engine.gateRefusal).
//
// On: the styles first, then every part the process runs. A part leads
// to how the press runs it — One Part, One Spot / Two Parts, Two Spots / One
// Part, Two Spots — then, when the parts already run in a style, Use
// style or Set up new; setting up picks the engineer's recipe (asked
// only when several fit), then the spots screen, then the same check.
//
// THE WORDS ARE THE DESKTOP'S: a Recipe is spots and robot moves with no part,
// a style is a recipe with its parts on it.
//
// THE SCREENS, each shown only when it has a question to ask:
//
//   saved     setup off: the styles, searchable by name, part or CATID
//   part      setup on: the styles, then the part list
//   count     One Part, One Spot / Two Parts, Two Spots / One Part, Two Spots —
//             only the ones a recipe here can run, skipped when that is one
//   second    the second part, the codes one character off the first leading
//   reuse     the styles these parts already run in: use one, or set up
//   which     the recipes that fit, by name, each with its picture
//   positions the recipe's spots on the cell picture; tap one, tap where it
//             should go instead
//   go        what the changeover does, the press it makes, the trips, Start
//   none      no recipe here takes these parts — an engineer sets it up
//   started   the tick, back to the board by itself
//
// NOTHING IS WRITTEN UNTIL START. Every screen before it is local state; Go's
// words come from flow/preview's mint form (read-only), and Start is flow/mint
// (open, resolve or mint) then changeover/start on the fingerprint it returns.
//
// THE HASH ENTRY. #compose=<screen>[;parts=A,B][;recipe=ID][;moves=X:Y,…]
// [;style=ID][;split=1][;hold=1] sets LOCAL UI STATE ONLY and never writes —
// how scripts/composer-shots.sh reaches each screen without driving the DOM.

import { renderFlowPicture } from './operator-flow.js';
import { esc } from '/static/shared/esc.js';

const M = () => window.ComposerModel;

// ── module state ─────────────────────────────────────────────────────────────
let view = null;        // the station view, as the board has it
let flowPayload = null; // the composer's own read — see ensureFlow
let flowPayloadFor = 0;
let flowPending = null;
let flowPendingFor = 0;
let screen = null;
let picks = [];         // part codes, in tap order
let split = false;      // One Part, Two Spots: the one pick fills both spots
let fresh = false;      // Set up new: do not reuse a style these parts run in
let chosen = 0;         // the style tapped, by style id, or 0
let recipe = null;      // the recipe (preset card) the parts mint from, or null
let moves = {};         // the Positions screen's moves: recipe node → node
let selected = '';      // the slot the Positions screen is moving
let preview = null;     // the last mint preview's answer
let previewStatus = 0;
let previewAbort = null;
let countdown = null;

const $ = id => document.getElementById(id);

function processID() { return view && view.process ? view.process.id : 0; }
function stationID() { return view && view.station ? view.station.id : 0; }
function stationName() { return (view && view.station && view.station.name) || ''; }
// HMI setup: the floor may set styles up here. Off, the station only
// offers the ones that exist.
function setupOn() { return !!(view && view.process && view.process.flow_composer_enabled); }
function runningID() { return (view && view.process && view.process.active_style_id) || 0; }
// The view's picker block: every live style with its parts. Polled, so a style
// minted a minute ago is here without a re-fetch.
function comp() { return (view && view.composer) || {}; }
function styles() { return comp().styles || []; }
// The composer's own read, fetched on CHANGEOVER: part rows, recipe cards and
// the cell picture.
function flow() { return flowPayload || {}; }
function presets() { return flow().presets || []; }
function partRows() { return flow().part_rows || []; }
function cell() { return (flowPayload && flowPayload.cell) || (view && view.cell) || { positions: [] }; }

function partsOf(s) { return (s.parts || []).map(p => p.payload_code || p); }
function produceSlots(p) { return (p.slots || []).filter(s => s.role === 'produce').length; }

// How many spots the picks fill: one per part, or two for One Part, Two Spots.
function spotsWanted() { return split ? 2 : picks.length; }

// The styles that run exactly these parts on this many spots — the
// server asks the same question at Go (engine.resolveByParts).
function savedMatches() {
    const want = picks.slice().sort().join('\x1f');
    return styles().filter(s => partsOf(s).slice().sort().join('\x1f') === want && (s.spots || 0) === spotsWanted());
}

// FIT: a recipe takes the parts when it has one slot per spot, and two spots
// need two PRODUCE slots — a weld cell's consume + produce recipe has two
// slots and one made part. The latest version of each name only.
function fits() {
    const n = spotsWanted();
    return presets().filter(p => (p.slots || []).length === n && (n === 1 || produceSlots(p) === n));
}

// The ways this cell can run a part, by the recipes it has.
function countOffers() {
    const out = [];
    if (presets().some(p => (p.slots || []).length === 1)) out.push('one');
    if (presets().some(p => (p.slots || []).length === 2 && produceSlots(p) === 2)) out.push('two', 'split');
    return out;
}

// A code one character off another of the same length — the plant numbers a
// left/right pair that way (SYN44 / SYN45), but which neighbour is the pair is
// not assumed: both sides lead the list.
function oneCharOff(a, b) {
    if (!a || !b || a.length !== b.length || a === b) return false;
    let n = 0;
    for (let i = 0; i < a.length; i++) if (a[i] !== b[i] && ++n > 1) return false;
    return n === 1;
}

function ensureFlow() {
    const pid = processID();
    if (flowPayload && flowPayloadFor === pid) return Promise.resolve(flowPayload);
    if (flowPending && flowPendingFor === pid) return flowPending;
    flowPendingFor = pid;
    flowPending = fetch('/api/operator-stations/' + stationID() + '/composer')
        .then(r => (r.ok ? r.json() : null))
        .then(p => {
            if (p) { flowPayload = p; flowPayloadFor = pid; }
            flowPending = null;
            return flowPayload;
        })
        .catch(() => { flowPending = null; return null; });
    return flowPending;
}

// ── root ─────────────────────────────────────────────────────────────────────
// show() rather than root() at every draw site: the root is created hidden,
// and a screen drawn into it without clearing `hidden` renders into a box
// nobody can see.
function show() {
    const r = root();
    r.hidden = false;
    return r;
}

function root() {
    let r = $('os-composer');
    if (r) return r;
    r = document.createElement('div');
    r.id = 'os-composer';
    r.className = 'os-comp-root';
    r.hidden = true;
    document.body.appendChild(r);
    r.addEventListener('click', onClick);
    return r;
}

function reset() {
    picks = []; split = false; fresh = false; chosen = 0;
    recipe = null; moves = {}; selected = ''; preview = null;
}

export function close() {
    const r = root();
    r.hidden = true;
    r.innerHTML = '';
    screen = null;
    reset();
    // THE HELD PAYLOAD GOES WITH THE SESSION IT WAS FETCHED FOR. An HMI runs
    // for weeks and a part set edited on the desktop fires nothing this page
    // listens to; clearing here makes the boundary the changeover session.
    flowPayload = null;
    flowPayloadFor = 0;
    if (countdown) { clearInterval(countdown); countdown = null; }
    if (previewAbort) { previewAbort.abort(); previewAbort = null; }
}

// ── open ─────────────────────────────────────────────────────────────────────
export function openPicker(v) {
    if (v) view = v;
    reset();
    screen = setupOn() ? 'part' : 'saved';
    drawPartSheet('Change over to');
    ensureFlow().then(() => { if (listing()) drawRows(''); });
}

function listing() { return screen === 'part' || screen === 'second' || screen === 'saved'; }

// The sheet's frame: a title, the scanner field and the rows' box. The field
// is the SCANNER'S target — focused on open, inputmode="none" so no on-screen
// keypad covers the rows the scan is meant to filter.
function drawPartSheet(title) {
    show().innerHTML =
        '<div class="os-comp-scrim on"></div>' +
        '<div class="os-comp-sheet on" role="dialog" aria-label="' + esc(title) + '">' +
        '<div class="os-comp-sheet-h"><h2>' + esc(title) + '</h2>' +
        '<label class="os-comp-search">' +
        '<svg class="mag" width="18" height="18" viewBox="0 0 18 18" aria-hidden="true">' +
        '<circle cx="7.5" cy="7.5" r="5.5" fill="none" stroke="currentColor" stroke-width="1.6"/>' +
        '<path d="M11.5 11.5 L16 16" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>' +
        '<input id="os-comp-q" type="text" autocomplete="off" ' +
        'placeholder="scan or type a part, a name or a CATID" inputmode="none"></label>' +
        '<button class="os-btn os-btn-quiet" data-act="close">Close</button></div>' +
        '<div class="os-comp-sheet-b" id="os-comp-pick"><div class="grp"><span class="n">Loading…</span></div></div></div>';
    drawRows('');
    const q = $('os-comp-q');
    if (q) { q.addEventListener('input', () => drawRows(q.value)); q.focus(); }
}

// CORE'S WORD ON THE PARTS, read from the same feed the board reads — the
// view's sourcing_by_style, keyed by style NAME. A red row stays TAPPABLE: the
// operator is authoritative, and the colour is information, not a gate.
function verdict(styleName) {
    const src = ((view && view.sourcing_by_style) || {})[styleName] || null;
    switch (src && src.code) {
        case 'green': return { cls: 'ok', text: 'Parts available' };
        case 'red': return { cls: 'no', text: 'No bin at Core', note: src.note || '' };
        case 'yellow': return { cls: 'unv', text: 'Running low', note: src.note || '' };
        case 'not_configured': return { cls: 'unv', text: 'Not checked at Core', note: src.note || '' };
        default: return { cls: 'unv', text: 'Unverified' };
    }
}

function spotsWord(n) { return n === 1 ? '1 spot' : (n || 0) + ' spots'; }

// One style row: its name, its parts and spots, Core's word on it. The
// running one says so.
function savedRow(s) {
    const running = s.id === runningID();
    const v = running ? { cls: 'ok', text: 'Running' } : verdict(s.name);
    const parts = partsOf(s).map(p => M().shortPart(p));
    const meta = [parts.join(' + '), spotsWord(s.spots || 0), (s.claim_nodes || []).join(', '),
        s.catid ? 'CATID ' + s.catid : '', v.note || ''].filter(Boolean).join(' · ');
    return '<button class="os-comp-row" data-act="saved" data-style="' + esc(s.id) + '">' +
        '<span class="id">' + esc(s.name) + '</span>' +
        '<span class="meta">' + esc(meta) + '</span>' +
        '<span class="vd ' + v.cls + '"' + (v.note ? ' title="' + esc(v.note) + '"' : '') + '>' + esc(v.text) + '</span></button>';
}

// One part row. The styles it runs in are named in it.
function partRow(r) {
    const runs = styles().filter(s => partsOf(s).indexOf(r.code) >= 0);
    const v = runs.length ? verdict(runs[0].name) : { cls: 'build', text: 'Not set up yet' };
    const meta = [r.name && r.name !== r.code ? r.name : '', r.catid ? 'CATID ' + r.catid : '',
        runs.length ? 'in ' + runs.map(s => s.name).join(', ') : '', v.note || '']
        .filter(Boolean).join(' · ');
    return '<button class="os-comp-row" data-act="part" data-part="' + esc(r.code) + '">' +
        '<span class="id">' + esc(M().shortPart(r.code)) + '</span>' +
        '<span class="meta">' + esc(meta) + '</span>' +
        '<span class="vd ' + v.cls + '"' + (v.note ? ' title="' + esc(v.note) + '"' : '') + '>' + esc(v.text) + '</span>' +
        '</button>';
}

// The styles in the order an operator looks for them: the running one,
// then the ones changed over to lately, then by name.
function savedInOrder() {
    const recent = comp().recent_targets || [];
    const rank = s => (s.id === runningID() ? -1 : (recent.indexOf(s.id) >= 0 ? recent.indexOf(s.id) : 99));
    return styles().slice().sort((a, b) => rank(a) - rank(b) || String(a.name).localeCompare(String(b.name)));
}

function grp(label, note) {
    return '<div class="grp"><span class="os-lbl">' + esc(label) + '</span><span class="n">' + esc(note) + '</span></div>';
}

function count(n, word) { return n + ' ' + word + (n === 1 ? '' : 's'); }

function processName() { return (view && view.process && view.process.name) || 'this process'; }

function drawRows(q) {
    const box = $('os-comp-pick');
    if (!box) return;
    if (!flowPayload && screen !== 'saved') { box.innerHTML = grp('', 'Loading…'); return; }
    const needle = String(q || '').trim().toLowerCase();
    const savedHay = s => [s.name, s.catid || ''].concat(partsOf(s)).join('  ').toLowerCase();
    const partHay = r => [r.code, r.name || '', r.catid || ''].join('  ').toLowerCase();
    let h = '';
    let shown = 0;
    if (screen !== 'second') {
        const saved = savedInOrder().filter(s => !needle || savedHay(s).indexOf(needle) >= 0);
        if (saved.length || screen === 'saved') h += grp('Styles', count(saved.length, 'style'));
        h += saved.map(savedRow).join('');
        shown += saved.length;
        if (screen === 'saved') {
            if (!saved.length) {
                h += grp('', (needle ? 'No style here matches that.' : 'This cell has no styles yet.') +
                    ' An engineer sets them up on the desktop: Processes › ' + processName() + ' › Styles.');
            }
            box.innerHTML = h;
            return;
        }
    }
    let rows = partRows();
    if (screen === 'second') rows = rows.filter(r => r.code !== picks[0]);
    rows = rows.filter(r => !needle || partHay(r).indexOf(needle) >= 0);
    if (screen === 'second') {
        // THE MATCHING HALF LEADS: the codes one character off the first part.
        const near = rows.filter(r => oneCharOff(r.code, picks[0]));
        if (near.length) h += grp('Matching half', 'one character off ' + M().shortPart(picks[0])) + near.map(partRow).join('');
        shown += near.length;
        rows = rows.filter(r => near.indexOf(r) < 0);
    }
    h += grp(screen === 'second' ? 'All parts' : 'Set up a part', count(rows.length, 'part')) + rows.map(partRow).join('');
    shown += rows.length;
    if (!shown) {
        h += grp('', (needle ? 'Nothing in this cell’s parts matches that.' : 'This cell has no parts yet.') +
            ' An engineer adds parts on the desktop: Processes › Settings › Parts this process runs.');
    }
    box.innerHTML = h;
}

// ── a style, tapped ───────────────────────────────────────────────────
function onSaved(id) {
    const s = styles().find(x => x.id === id);
    if (!s) return;
    reset();
    chosen = s.id;
    picks = partsOf(s);
    go();
}

// ── a part, and how the press runs it ────────────────────────────────────────
function onPart(code) {
    if (screen === 'second') { picks = [picks[0], code]; afterParts(); return; }
    picks = [code];
    split = false;
    const offers = countOffers();
    if (offers.length > 1) { screen = 'count'; drawCount(offers); return; }
    afterParts();
}

function drawCount(offers) {
    const btn = (act, label, desc) => offers.indexOf(act) < 0 ? '' :
        '<button class="os-btn os-btn-lg os-comp-way" data-act="' + act + '"><span>' + esc(label) +
        '</span><span class="d">' + esc(desc) + '</span></button>';
    show().innerHTML =
        '<div class="os-comp-scrim on"></div>' +
        '<div class="os-comp-setup on"><div class="os-comp-card">' +
        '<div class="top"><div><span class="os-lbl">Change over to</span>' +
        '<h2>' + esc(M().shortPart(picks[0])) + '</h2>' +
        '<div class="desc">how does the press run it?</div></div>' +
        '<button class="x" data-act="topart" aria-label="Back to the part list">&#10005;</button></div>' +
        '<div class="os-comp-ways">' +
        btn('one', 'One Part, One Spot', 'one part, one bin') +
        btn('two', 'Two Parts, Two Spots', 'pick the second part; one bin each') +
        btn('split', 'One Part, Two Spots', 'each stroke split into two bins') +
        '</div></div></div>';
}

// The parts are known: offer the styles they already run in, or set up.
function afterParts() {
    const matches = savedMatches();
    if (matches.length && !fresh) { screen = 'reuse'; drawReuse(matches); return; }
    toRecipe();
}

function drawReuse(matches) {
    show().innerHTML =
        '<div class="os-comp-scrim on"></div>' +
        '<div class="os-comp-setup on"><div class="os-comp-card">' +
        '<div class="top"><div><span class="os-lbl">Change over to</span>' +
        '<h2>' + esc(picksLabel()) + '</h2>' +
        '<div class="desc">' + (matches.length === 1 ? 'there is a saved style for this'
            : 'there are ' + matches.length + ' saved styles for this') + '</div></div>' +
        '<button class="x" data-act="topart" aria-label="Back to the part list">&#10005;</button></div>' +
        '<div class="os-comp-ways">' +
        matches.map(s => '<button class="os-btn os-btn-lg os-comp-way" data-act="saved" data-style="' + esc(s.id) + '">' +
            '<span>Use saved style</span><span class="d">' + esc(s.name + ' · ' + (s.claim_nodes || []).join(', ')) +
            '</span></button>').join('') +
        '<button class="os-btn os-btn-lg os-comp-way" data-act="setupnew"><span>Set up new</span>' +
        '<span class="d">pick a recipe and its spots</span></button>' +
        '</div></div></div>';
}

function picksLabel() {
    const parts = picks.map(p => M().shortPart(p)).join(' + ');
    return split ? parts + ' · One Part, Two Spots' : parts;
}

// ── which recipe ─────────────────────────────────────────────────────────────
function toRecipe() {
    const f = fits();
    if (!f.length) { screen = 'none'; drawNone(); return; }
    if (f.length === 1) { recipe = f[0]; toPositions(); return; }
    screen = 'which';
    drawWhich(f);
}

// Setting up always passes the Positions screen: the operator sees where the
// recipe puts the parts and may move a spot before anything is saved.
function toPositions() { screen = 'positions'; selected = ''; drawPositions(); }

function drawNone() {
    show().innerHTML =
        '<div class="os-comp-scrim on"></div>' +
        '<div class="os-comp-setup on"><div class="os-comp-card">' +
        '<div class="top"><div><span class="os-lbl">Change over to</span>' +
        '<h2>' + esc(picksLabel()) + '</h2>' +
        '<div class="desc">no recipe on this cell runs ' + (picks.length === 1 && !split ? 'this part' : 'this') + '</div></div>' +
        '<button class="x" data-act="topart" aria-label="Back to the part list">&#10005;</button></div>' +
        '<div class="os-comp-note">An engineer adds the recipe on the desktop: Processes › ' + esc(processName()) +
        ' › Recipes.</div></div></div>';
}

// The recipe drawn on THIS cell's picture: the composer model fed the recipe's
// cells and the picks on their slots, and the board's own renderer.
function recipePicture(p, opts) {
    const s = pictureState(renamedCells(p, moves));
    // The picks on their slots, in slot order — what Go's placement will say
    // unless a part's own history puts it elsewhere. One Part, Two Spots puts
    // the one part on both.
    (p.slots || []).forEach((slot, i) => {
        const code = split ? picks[0] : picks[i];
        if (!code) return;
        for (const n of slot.nodes) {
            const to = moves[n] || n;
            if (s.cells[to]) s.cells[to].part = code;
        }
    });
    return drawPicture(s, opts);
}

// cellsPicture draws a flow given as cells with their parts on — what the
// mint preview says the changeover goes to.
function cellsPicture(cells, opts) {
    const s = pictureState(cells);
    for (const c of cells) {
        if (c.payload_code && s.cells[c.core_node_name]) s.cells[c.core_node_name].part = c.payload_code;
    }
    return drawPicture(s, opts);
}

function pictureState(cells) {
    const model = M().init({
        styleId: 0, styleName: '', shape: true, flowspec: window.FLOWSPEC || null,
        positions: (cell().positions || []).map(x => ({ core_node_name: x.core_node_name, kind: x.kind, sequence: x.sequence })),
        routing: [], claims: [], parts: [], processClaims: [], groups: cell().groups || {},
    });
    return M().reduce(model, { type: 'applyPreset', preset: { cells: cells } });
}

function drawPicture(s, opts) {
    const o = Object.assign({ selected: null, findings: {} }, opts || {});
    const svg = renderFlowPicture({ cell: M().pictureCells(s, cell()), station: view && view.station }, o);
    return { svg: svg, height: o.height || 560 };
}

// renamedCells is the recipe's cells with the Positions screen's moves
// applied, the same rename the server makes (engine.applyMoves).
function renamedCells(p, mv) {
    const r = n => (n && mv[n]) || n;
    return Object.keys(p.cells || {}).map(n => {
        const c = Object.assign({}, p.cells[n]);
        c.core_node_name = r(c.core_node_name || n);
        c.paired_core_node = r(c.paired_core_node);
        c.second_paired_core_node = r(c.second_paired_core_node);
        c.inbound_staging = r(c.inbound_staging);
        c.outbound_staging = r(c.outbound_staging);
        return c;
    });
}

function drawWhich(list) {
    const cards = list.map(p => {
        const pic = recipePicture(p, { usedOnly: true });
        return '<button class="os-comp-recipe" data-act="recipe" data-preset="' + esc(p.id) + '">' +
            '<span class="nm">' + esc(p.name) + '</span>' +
            '<span class="where">' + esc(p.where || '') + '</span>' +
            '<svg class="os-flow-picture" viewBox="0 0 1280 ' + pic.height + '" preserveAspectRatio="xMidYMin meet">' +
            pic.svg + '</svg></button>';
    }).join('');
    show().innerHTML =
        '<div class="os-comp-scrim on"></div>' +
        '<div class="os-comp-sheet on" role="dialog" aria-label="Which recipe?">' +
        '<div class="os-comp-sheet-h"><h2>Which recipe? · ' + esc(picksLabel()) + '</h2>' +
        '<span class="sp"></span><button class="os-btn os-btn-quiet" data-act="topart">Back</button></div>' +
        '<div class="os-comp-which">' + cards + '</div></div>';
}

// ── spots ────────────────────────────────────────────────────────────────────
// The recipe's spots on the cell picture; tap one, then the position or lane
// it should use instead. Same robot moves, different spot — mode, pairing and
// role stay the recipe's, and the server checks the move against the routing
// set (engine.applyMoves).
function drawPositions() {
    const pic = recipePicture(recipe, { editable: true, selected: selected || null });
    const list = Object.keys(moves).map(n =>
        '<button class="os-chip btn" data-act="unmove" data-node="' + esc(n) + '">' +
        esc(n + ' → ' + moves[n]) + ' ✕</button>').join('');
    show().innerHTML =
        '<div class="os-comp-full">' +
        '<div class="os-comp-hdr"><h1>Spots · ' + esc(recipe.name) + '</h1>' +
        '<span class="sub">' + esc(picksLabel()) + '</span>' +
        '<span class="sp"></span><button class="os-btn os-btn-quiet" data-act="topart">Back</button></div>' +
        '<div class="os-comp-stage">' +
        '<div class="os-comp-hint">' + (selected
            ? 'Now tap where ' + esc(moves[selected] || selected) + ' goes: a <b>free position or lane</b> moves it, ' +
                '<b>another spot</b> swaps them' + (moves[selected] ? ', <b>' + esc(selected) + '</b> puts it back' : '') +
                ' · tap it again to leave it'
            : 'This is where the parts go. Tap a <b>spot</b>, then where it should go · as it is? <b>Next</b>') +
        '</div>' +
        '<div class="os-comp-scroll"><svg id="os-comp-svg" class="os-flow-picture" viewBox="0 0 1280 ' + pic.height +
        '" width="1280" height="' + pic.height + '">' + pic.svg + '</svg></div></div>' +
        '<div class="os-comp-bar"><div class="os-chips">' + list + '</div><span class="sp"></span>' +
        '<button class="os-btn os-btn-primary" data-act="next">Next</button></div></div>';
}

// The nodes a slot may be moved from: the recipe's own (positions, partners,
// lanes), after the moves so far.
function recipeNodes() {
    const out = new Set();
    for (const c of renamedCells(recipe, moves)) {
        for (const n of [c.core_node_name, c.paired_core_node, c.second_paired_core_node, c.inbound_staging, c.outbound_staging]) {
            if (n) out.add(n);
        }
    }
    return out;
}

// TAP A SPOT, THEN WHERE IT GOES. `selected` is the recipe's own name for the
// spot (a move is kept as recipe node → where it is now):
//
//   - a free position or lane moves it there;
//   - another spot of the recipe swaps the two;
//   - its own first place puts it back;
//   - where it is now again cancels.
//
// A move back to where it started is no move, so it leaves the list.
function onPositionsTap(node) {
    if (!node) return;
    const original = n => Object.keys(moves).find(k => moves[k] === n) || n;
    const at = n => moves[n] || n;
    if (!selected) {
        if (recipeNodes().has(node)) selected = original(node);
        drawPositions();
        return;
    }
    const a = selected;
    selected = '';
    if (node === at(a)) { drawPositions(); return; }
    if (recipeNodes().has(node)) {
        const b = original(node);
        const aAt = at(a), bAt = at(b);
        moves[a] = bAt;
        moves[b] = aAt;
    } else {
        moves[a] = node;
    }
    for (const k of Object.keys(moves)) if (moves[k] === k) delete moves[k];
    drawPositions();
}

// ── go ───────────────────────────────────────────────────────────────────────
function mintBody() {
    if (chosen) return M().savedBody(chosen, stationID());
    return M().mintBody(recipe ? Number(recipe.id) : 0, picks, stationID(), moves, { split: split, fresh: fresh });
}

function go() {
    screen = 'go';
    preview = null;
    drawGo();
    runPreview().then(() => { if (screen === 'go') drawGo(); });
}

// flow/preview's mint form: read-only. The preflight is asked here — Go is the
// one screen that shows Core's word on the parts.
async function runPreview() {
    if (previewAbort) previewAbort.abort();
    previewAbort = new AbortController();
    try {
        const res = await fetch('/api/processes/' + processID() + '/flow/preview?preflight=1', {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(mintBody()), signal: previewAbort.signal,
        });
        previewStatus = res.status;
        preview = await res.json().catch(() => ({}));
    } catch (e) {
        if (e && e.name === 'AbortError') return;
        previewStatus = 0;
        preview = { error: 'Could not reach this edge.' };
    }
}

// orderRobots is who moves: the robots the planned orders name.
function orderRobots(pv) {
    const robots = [...new Set(M().orderSentences({ preview: pv }).map(o => o.robot))].sort();
    return robots.length === 2 ? 'Robot 1 and Robot 2' : robots.length === 1 ? 'Robot ' + robots[0] : '';
}

function andList(xs) {
    if (xs.length <= 1) return xs.join('');
    return xs.slice(0, -1).join(', ') + ' and ' + xs[xs.length - 1];
}

function uniq(xs) { return [...new Set(xs.filter(Boolean))]; }

// WHAT THE CHANGEOVER DOES, in the order it happens: the bins that come off
// and where they go, the parts that come on and from where, the wait for
// Release, and Core's word on the parts. Built from the planned orders, so it
// says what the robots will do rather than how many trips that is.
function goSentences(pv) {
    const orders = M().orderSentences({ preview: pv });
    const out = [];
    const off = orders.filter(o => o.robot === 2);
    if (off.length) {
        out.push('Takes the bins off ' + andList(uniq(off.map(o => o.from))) + ' to ' + andList(uniq(off.map(o => o.to))) + '.');
    }
    const byPart = {};
    for (const o of orders.filter(x => x.robot === 1)) (byPart[o.what || ''] = byPart[o.what || ''] || []).push(o);
    for (const what of Object.keys(byPart)) {
        const legs = byPart[what];
        out.push('Brings ' + (what || 'a bin') + ' to ' + andList(uniq(legs.map(o => o.to))) +
            ' from ' + andList(uniq(legs.map(o => o.from))) + '.');
    }
    if (orders.length) out.push('Each robot waits at the press until you press Release.');
    else if (pv.order_count === 0) out.push('No robot has to move.');
    if (pv.preflight) {
        out.push(pv.preflight.state === 'unchecked' ? 'Core could not be asked whether the parts are there.'
            : pv.preflight.state === 'missing' ? 'Core has no bin for ' + andList(pv.preflight.missing || []) +
                ' yet: that delivery waits until it has one.'
                : 'Core has a bin for every part.');
    }
    return out;
}

function goName(pv) { return (pv && pv.mint && pv.mint.style_name) || picksLabel(); }

function goKind(pv) {
    if (pv && pv.mint && pv.mint.minted) return 'New style' + (recipe ? ' from ' + recipe.name : '');
    return 'Saved style';
}

function drawGo() {
    const pv = preview;
    const name = goName(pv);
    let say, trips = '', acts, title = 'Change over to ' + name + '?';
    const back = '<button class="os-btn os-btn-quiet os-btn-lg" data-act="back">Back</button>';
    if (!pv) {
        say = '<p class="os-comp-sentence">Checking what this changeover does…</p>';
        acts = back;
    } else if (previewStatus === 404) {
        screen = 'none';
        drawNone();
        return;
    } else if (pv.running || (pv.mint && pv.mint.running)) {
        title = name;
        say = '<p class="os-comp-sentence">' + esc(name) + ' is already running — there is nothing to change over.</p>';
        acts = back;
    } else if (pv.error && !pv.actions) {
        say = '<p class="os-comp-sentence os-comp-refusal">' + esc(pv.error) + '</p>';
        acts = back;
    } else {
        say = goSentences(pv).map(t => '<p class="os-comp-sentence">' + esc(t) + '</p>').join('') +
            (pv.error ? '<div class="os-comp-refusal">' + esc(pv.error) + '</div>' : '');
        const orders = M().orderSentences({ preview: pv });
        if (orders.length) {
            trips = '<table class="os-comp-trips"><tr><th>Robot</th><th>From → to</th></tr>' +
                orders.map(o => '<tr><td class="r' + o.robot + '">Robot ' + o.robot + '</td><td>' +
                    esc(M().orderTrip(o)) + '</td></tr>').join('') + '</table>';
        }
        acts = back + (pv.error ? '' : '<button class="os-btn os-btn-primary os-btn-lg" data-act="start">Start changeover</button>');
    }
    // THE PRESS IT MAKES, ALWAYS DRAWN: the flow the changeover goes to, with
    // the parts on their spots, so the operator checks a picture rather than
    // reading a list of node names.
    let pic = '';
    const cells = pv && pv.mint && pv.mint.cells;
    if (cells && cells.length) {
        const p = cellsPicture(cells, { usedOnly: true });
        pic = '<div class="os-comp-scroll"><svg class="os-flow-picture" viewBox="0 0 1280 ' + p.height +
            '" width="1280" height="' + p.height + '">' + p.svg + '</svg></div>';
    }
    show().innerHTML =
        '<div class="os-comp-full os-comp-go">' +
        '<div class="os-comp-hdr"><h1>' + esc(title) + '</h1><span class="sub">' + esc(pv ? goKind(pv) : '') + '</span></div>' +
        '<div class="os-comp-stage"><div class="os-comp-say">' + say + '</div>' + pic +
        (trips ? '<div class="os-comp-tripbox">' + trips + '</div>' : '') + '</div>' +
        '<div class="os-comp-bar acts"><span class="sp"></span>' + acts + '</div></div>';
}

// ── start ────────────────────────────────────────────────────────────────────
// Open, resolve or mint at Go, then start on the fingerprint it returns.
async function start() {
    const btn = root().querySelector('[data-act="start"]');
    if (btn) btn.disabled = true;
    const minted = await postJSON('/api/processes/' + processID() + '/flow/mint', mintBody());
    if (!minted.ok) { showRefusal(minted.body, minted.status); return; }
    const started = await postJSON('/api/processes/' + processID() + '/changeover/start', {
        to_style_id: minted.body.style_id, called_by: stationName(),
        flow_fingerprint: minted.body.fingerprint,
    });
    // ANYTHING THAT IS NOT A 2xx IS NOT A STARTED CHANGEOVER: the engine's
    // refusals come back 400 and the stale fingerprint 409, and both are shown
    // by name on the screen the operator is standing on.
    if (!started.ok) { showRefusal(started.body, started.status, minted.body.minted); return; }
    openStarted(minted.body);
}

async function postJSON(url, body) {
    try {
        const res = await fetch(url, {
            method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
        });
        return { ok: res.ok, status: res.status, body: await res.json().catch(() => ({})) };
    } catch (_) {
        return { ok: false, status: 0, body: { error: 'Could not reach this edge.' } };
    }
}

function showRefusal(json, status, minted) {
    const say = root().querySelector('.os-comp-say');
    if (!say) return;
    let el = say.querySelector('.os-comp-refusal');
    if (!el) {
        el = document.createElement('div');
        el.className = 'os-comp-refusal';
        say.appendChild(el);
    }
    el.textContent = (minted ? 'The style was made, and the changeover was refused: ' : '') +
        ((json && json.error) || ('The changeover was refused (' + status + ')'));
    const btn = root().querySelector('[data-act="start"]');
    if (btn) btn.disabled = false;
}

// ── started ──────────────────────────────────────────────────────────────────
// 4 s countdown, no button, back to the board by itself.
function openStarted(res, hold) {
    screen = 'started';
    const pv = preview || {};
    const n = typeof pv.order_count === 'number' ? ' · ' + pv.order_count + ' order' + (pv.order_count === 1 ? '' : 's') : '';
    const robots = orderRobots(pv);
    const line = ((res && res.style_name) || '') + n + (robots ? ' · ' + robots : '') + '.' +
        (res && res.minted ? ' New style from ' + (recipe ? recipe.name : 'the recipe') + '.' : '');
    show().innerHTML =
        '<div class="os-comp-started on"><div class="ring">✓</div>' +
        '<h2>Changeover started</h2><p>' + esc(line) + '</p>' +
        '<span class="os-lbl">Started by ' + esc(stationName()) + ' · source HMI</span>' +
        '<div class="count" id="os-comp-count">back to the board in 4</div></div>';
    if (hold) return;   // the shot-only hold; see openFromHash
    let k = 4;
    if (countdown) clearInterval(countdown);
    countdown = setInterval(() => {
        k--;
        const el = $('os-comp-count');
        if (k <= 0) { clearInterval(countdown); countdown = null; close(); return; }
        if (el) el.textContent = 'back to the board in ' + k;
    }, 1000);
}

// Back from Go returns to the screen before it: the spots when setting up,
// the list when a style was tapped.
function back() {
    if (chosen || !recipe) { openPicker(); return; }
    toPositions();
}

// ── events ───────────────────────────────────────────────────────────────────
function onClick(e) {
    const btn = e.target.closest && e.target.closest('[data-act]');
    if (!btn && screen === 'positions') {
        const lane = e.target.closest && e.target.closest('[data-staging]');
        const pos = e.target.closest && e.target.closest('[data-pos]');
        if (lane || pos) { e.stopPropagation(); onPositionsTap(lane ? lane.dataset.staging : pos.dataset.pos); }
        return;
    }
    if (!btn) return;
    e.stopPropagation();
    switch (btn.dataset.act) {
        case 'close': close(); break;
        case 'part': onPart(btn.dataset.part); break;
        case 'saved': onSaved(+btn.dataset.style); break;
        case 'topart': openPicker(); break;
        case 'back': back(); break;
        case 'one': split = false; afterParts(); break;
        case 'two': split = false; screen = 'second'; drawPartSheet('Second part'); break;
        case 'split': split = true; afterParts(); break;
        case 'setupnew': fresh = true; toRecipe(); break;
        case 'recipe': {
            recipe = presets().find(p => String(p.id) === btn.dataset.preset) || null;
            if (recipe) toPositions();
            break;
        }
        case 'unmove': delete moves[btn.dataset.node]; drawPositions(); break;
        case 'next': go(); break;
        case 'start': start(); break;
        default: break;
    }
}

// ── hash entry (shots) ───────────────────────────────────────────────────────
export function openFromHash() {
    const h = (window.location && window.location.hash) || '';
    const m = /#compose=([a-z]+)/.exec(h);
    if (!m) return false;
    const opt = name => { const x = new RegExp(';' + name + '=([^;]*)').exec(h); return x ? decodeURIComponent(x[1]) : ''; };
    reset();
    picks = opt('parts') ? opt('parts').split(',') : [];
    split = opt('split') === '1';
    chosen = Number(opt('style')) || 0;
    recipe = presets().find(p => String(p.id) === opt('recipe')) || null;
    moves = {};
    for (const pair of (opt('moves') ? opt('moves').split(',') : [])) {
        const [from, to] = pair.split(':');
        if (from && to) moves[from] = to;
    }
    // ;hold=1 stops the started screen's countdown so a screenshot can catch
    // it. Local UI state, never a write.
    const hold = /;hold=1/.test(h);
    switch (m[1]) {
        case 'part': case 'saved': openPicker(); break;
        case 'count': screen = 'count'; drawCount(countOffers()); break;
        case 'second': screen = 'second'; drawPartSheet('Second part'); break;
        case 'reuse': screen = 'reuse'; drawReuse(savedMatches()); break;
        case 'which': screen = 'which'; drawWhich(fits()); break;
        case 'positions': if (recipe) { screen = 'positions'; drawPositions(); } break;
        case 'none': screen = 'none'; drawNone(); break;
        case 'go': go(); break;
        case 'started':
            runPreview().then(() => openStarted({ style_name: (preview && preview.mint && preview.mint.style_name) || '',
                minted: !!(preview && preview.mint && preview.mint.minted) }, hold));
            break;
        default: return false;
    }
    return true;
}

export function setView(v) {
    view = v;
    if (listing()) drawRows(($('os-comp-q') || {}).value || '');
}

// A page that throws renders the board underneath and photographs as a
// plausible shot. The shots harness reads this attribute and fails the run.
window.addEventListener('error', e => {
    try {
        document.body.dataset.composerError =
            String((e && (e.message || e.error)) || 'unknown').slice(0, 300);
    } catch (_) { /* nothing left to report with */ }
});

// ESCAPE PUTS THE MODAL AWAY, except over the started screen (it closes by its
// own countdown) and Go mid-start. The scanner field's text is filtering, not
// work, so nothing is asked. `screen` is the module's own state — asking root()
// here would CREATE the modal on a page that never opened it.
export function onEscape() {
    if (!screen || screen === 'started') return;
    if (['positions', 'which', 'count', 'reuse', 'none'].indexOf(screen) >= 0) { openPicker(); return; }
    close();
}

document.addEventListener('keydown', e => {
    if (e.key !== 'Escape') return;
    onEscape();
});

window.ComposerUI = { openPicker, close, setView, openFromHash, onEscape };

// THE HASH ENTRY FETCHES ITS OWN VIEW. On the live path the board hands one
// over (CHANGEOVER passes getView()), but a shot loads the page cold at a hash:
// this reads the same endpoints the board reads, once, and opens. Still no
// write — every screen below Start is local state and Go renders a preview.
function bootFromHash() {
    if (!/#compose=/.test((window.location && window.location.hash) || '')) return;
    const m = /\/operator\/station\/(\d+)/.exec(window.location.pathname || '');
    if (!m) return;
    fetch('/api/operator-stations/' + m[1] + '/view')
        .then(r => r.json())
        .then(v => {
            view = v;
            return fetch('/api/operator-stations/' + m[1] + '/cell')
                .then(r => (r.ok ? r.json() : null))
                .then(pic => { if (pic) view.cell = pic; })
                .catch(() => { /* the picture says there is nothing to draw */ });
        })
        .then(() => ensureFlow())
        .then(() => { openFromHash(); })
        .catch(() => { /* no view, no modal — the board still renders */ });
}

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', bootFromHash);
else bootFromHash();
