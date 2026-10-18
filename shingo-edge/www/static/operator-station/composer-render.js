// composer-render.js — the station's changeover modal: pick a part.
//
// THE FLOOR DESCRIBES WHAT IT WANTS; SHINGO FINDS THE ENGINEER'S RECIPE
// (pick-a-part brief, 2026-10-18). Tap CHANGEOVER, tap the part, if the press
// runs two off say so and tap the second, and Start. If the part already runs
// in a style here that style is used, silently. Otherwise one recipe that fits
// is used without being shown; several fit → the operator picks by the
// engineer's names; none → see an engineer. Never a swap mode, a lane, a robot
// leg or a role.
//
// THE SCREENS, each shown only when it has a question to ask:
//
//   part      the part list (codes, names, CATIDs; the scanner's target)
//   two       "two off?" — only when an enabled recipe has two produce slots
//   second    the part list again, for the second part
//   which     the recipes that fit, by name, each with its picture — only when
//             the parts run in no live style and more than one recipe fits
//   positions the recipe's slots on the cell picture; tap a slot, tap the
//             position or lane it should use instead — only where the
//             engineer has switched the floor's edit on (R-K)
//   go        one sentence, the receipt under <details>, Start
//   none      no recipe here takes these parts — an engineer sets it up
//   started   the tick, back to the board by itself
//
// NOTHING IS WRITTEN UNTIL START. Every screen before it is local state; Go's
// sentence comes from flow/preview's mint form (read-only), and Start is
// flow/mint (resolve or mint) then changeover/start on the fingerprint it
// returns. Backing out leaves nothing behind.
//
// THE HASH ENTRY. #compose=<screen>[;parts=A,B][;recipe=ID][;moves=X:Y,…]
// [;hold=1] sets LOCAL UI STATE ONLY and never writes — how
// scripts/composer-shots.sh reaches each screen without driving the DOM.

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
function floorMayMove() { return !!(view && view.process && view.process.flow_composer_enabled); }
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

// The live style whose parts are exactly these, or null. The server decides
// the same question again at Go (engine.resolveByParts) and breaks ties; this
// one only decides whether a recipe has to be asked about.
function styleRunning(codes) {
    const want = codes.slice().sort().join('\x1f');
    return styles().find(s => partsOf(s).slice().sort().join('\x1f') === want) || null;
}

// FIT (brief U3/U4): a recipe takes the parts when it has one slot per part,
// and a floor two-off needs two PRODUCE slots — a weld cell's consume +
// produce recipe has two slots and one made part (O-12). The latest version
// of each name only; the server sends nothing else.
function fits(codes) {
    return presets().filter(p => (p.slots || []).length === codes.length &&
        (codes.length === 1 || produceSlots(p) === codes.length));
}

function twoOffOffered() { return presets().some(p => produceSlots(p) === 2); }

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

export function close() {
    const r = root();
    r.hidden = true;
    r.innerHTML = '';
    screen = null;
    picks = []; recipe = null; moves = {}; selected = ''; preview = null;
    // THE HELD PAYLOAD GOES WITH THE SESSION IT WAS FETCHED FOR. An HMI runs
    // for weeks and a part set edited on the desktop fires nothing this page
    // listens to; clearing here makes the boundary the changeover session.
    flowPayload = null;
    flowPayloadFor = 0;
    if (countdown) { clearInterval(countdown); countdown = null; }
    if (previewAbort) { previewAbort.abort(); previewAbort = null; }
}

// ── part ─────────────────────────────────────────────────────────────────────
export function openPicker(v) {
    if (v) view = v;
    picks = []; recipe = null; moves = {}; selected = ''; preview = null;
    screen = 'part';
    drawPartSheet('Change over to');
    ensureFlow().then(() => { if (screen === 'part' || screen === 'second') drawPartRows(''); });
}

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
    drawPartRows('');
    const q = $('os-comp-q');
    if (q) { q.addEventListener('input', () => drawPartRows(q.value)); q.focus(); }
}

// CORE'S WORD ON THE PARTS (owner, 2026-09-10: four verdicts), read from the
// same feed the board reads — the view's sourcing_by_style, keyed by style
// NAME. A part row carries the verdict of the style it runs alone in; a part
// with no flow yet says so. A red row stays TAPPABLE: the operator is
// authoritative, and the colour is information, not a gate.
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

// One row per part the process may run. The styles a part already runs in are
// named under it; a style that runs SEVERAL parts (a weld cell's, a two-off
// press's) is a chip of its own — tapping it picks that style whole, which is
// how a cell whose recipes hold more than a press does is changed over.
function partRow(r) {
    const live = styles().filter(s => partsOf(s).indexOf(r.code) >= 0);
    const alone = live.filter(s => partsOf(s).length === 1);
    const shared = live.filter(s => partsOf(s).length > 1);
    // "No flow yet" only when the part runs in no style at all; a part that runs
    // only with others carries that style's verdict.
    const v = alone.length ? verdict(alone[0].name)
        : shared.length ? verdict(shared[0].name) : { cls: 'build', text: 'No flow yet' };
    const meta = [r.name && r.name !== r.code ? r.name : '', r.catid ? 'CATID ' + r.catid : '',
        alone.length ? 'runs as ' + alone.map(s => s.name).join(', ') : '', v.note || '']
        .filter(Boolean).join(' · ');
    const chips = shared.map(s => '<button class="os-chip btn" data-act="style" data-style="' + s.id + '">' +
        esc(partsOf(s).map(p => M().shortPart(p)).join(' + ')) + '</button>').join('');
    return '<div class="os-comp-part"><button class="os-comp-row" data-act="part" data-part="' + esc(r.code) + '">' +
        '<span class="id">' + esc(M().shortPart(r.code)) + '</span>' +
        '<span class="meta">' + esc(meta) + '</span>' +
        '<span class="vd ' + v.cls + '"' + (v.note ? ' title="' + esc(v.note) + '"' : '') + '>' + esc(v.text) + '</span>' +
        '</button>' + (chips ? '<div class="os-chips">' + chips + '</div>' : '') + '</div>';
}

function drawPartRows(q) {
    const box = $('os-comp-pick');
    if (!box) return;
    if (!flowPayload) { box.innerHTML = '<div class="grp"><span class="n">Loading…</span></div>'; return; }
    const needle = String(q || '').trim().toLowerCase();
    const hay = r => [r.code, r.name || '', r.catid || ''].join('  ').toLowerCase();
    let rows = partRows();
    if (screen === 'second') rows = rows.filter(r => picks.indexOf(r.code) < 0);
    const all = rows.filter(r => !needle || hay(r).indexOf(needle) >= 0);
    // RECENT, AS PARTS: the parts of the last styles this press changed over
    // to, newest first.
    const recentCodes = [];
    for (const id of (comp().recent_targets || []).slice(0, 4)) {
        const s = styles().find(x => x.id === id);
        for (const c of (s ? partsOf(s) : [])) if (recentCodes.indexOf(c) < 0) recentCodes.push(c);
    }
    const recent = recentCodes.map(c => all.find(r => r.code === c)).filter(Boolean);
    const rest = all.filter(r => recent.indexOf(r) < 0);
    let h = '';
    if (screen !== 'second' && recent.length) {
        h += '<div class="grp"><span class="os-lbl">Recent</span><span class="n">from changeover history</span></div>' +
            recent.map(partRow).join('');
    }
    h += '<div class="grp"><span class="os-lbl">' + (screen === 'second' ? 'Runs with ' + esc(picks[0]) : 'All parts') +
        '</span><span class="n">' + rest.length + ' part' + (rest.length === 1 ? '' : 's') + '</span></div>' +
        rest.map(partRow).join('');
    if (!all.length) {
        h += '<div class="grp"><span class="n">' + (needle ? 'Nothing in this cell’s parts matches that.'
            : 'This cell has no parts yet.') + ' An engineer adds parts on the desktop: Processes › Settings › ' +
            'Parts this process runs.</span></div>';
    }
    box.innerHTML = h;
}

function onPart(code) {
    if (screen === 'second') { picks = [picks[0], code]; resolve(); return; }
    picks = [code];
    if (twoOffOffered()) { screen = 'two'; drawTwo(); return; }
    resolve();
}

// A style picked whole (a chip under a part): its parts are the picks, and the
// server resolves them to it.
function onStyle(id) {
    const s = styles().find(x => x.id === id);
    if (!s) return;
    picks = partsOf(s);
    recipe = null;
    go();
}

// ── two off? ─────────────────────────────────────────────────────────────────
function drawTwo() {
    show().innerHTML =
        '<div class="os-comp-scrim on"></div>' +
        '<div class="os-comp-setup on"><div class="os-comp-card">' +
        '<div class="top"><div><span class="os-lbl">Change over to</span>' +
        '<h2>' + esc(M().shortPart(picks[0])) + '</h2>' +
        '<div class="desc">does the cell run it alone, or two off?</div></div>' +
        '<button class="x" data-act="topart" aria-label="Back to the part list">&#10005;</button></div>' +
        '<div class="os-comp-two">' +
        '<button class="os-btn os-btn-lg" data-act="just">Just this part</button>' +
        '<button class="os-btn os-btn-lg" data-act="second">+ second part</button></div></div></div>';
}

// ── resolve: a live style, one recipe, several, or none ──────────────────────
function resolve() {
    if (styleRunning(picks)) { recipe = null; go(); return; }
    const f = fits(picks);
    if (!f.length) { screen = 'none'; drawNone(); return; }
    if (f.length === 1) { recipe = f[0]; afterRecipe(); return; }
    screen = 'which';
    drawWhich(f);
}

function afterRecipe() {
    if (floorMayMove()) { screen = 'positions'; selected = ''; drawPositions(); return; }
    go();
}

function drawNone() {
    show().innerHTML =
        '<div class="os-comp-scrim on"></div>' +
        '<div class="os-comp-setup on"><div class="os-comp-card">' +
        '<div class="top"><div><span class="os-lbl">Change over to</span>' +
        '<h2>' + esc(picks.map(p => M().shortPart(p)).join(' + ')) + '</h2>' +
        '<div class="desc">no recipe on this cell takes ' + (picks.length === 1 ? 'this part' : 'these parts') + '</div></div>' +
        '<button class="x" data-act="topart" aria-label="Back to the part list">&#10005;</button></div>' +
        '<div class="os-comp-note">An engineer sets this up on the desktop.</div></div></div>';
}

// ── which one? ───────────────────────────────────────────────────────────────
// The recipe drawn on THIS cell's picture: the composer model fed the recipe's
// cells and the picks on their slots, and the board's own renderer.
function recipePicture(p, opts) {
    const cells = renamedCells(p, moves);
    const model = M().init({
        styleId: 0, styleName: p.name, shape: true, flowspec: window.FLOWSPEC || null,
        positions: (cell().positions || []).map(x => ({ core_node_name: x.core_node_name, kind: x.kind, sequence: x.sequence })),
        routing: [], claims: [], parts: [], processClaims: [], groups: cell().groups || {},
    });
    const s = M().reduce(model, { type: 'applyPreset', preset: { cells: cells } });
    // The picks on their slots, in slot order — what Go's placement will say
    // unless a part's own history puts it elsewhere.
    (p.slots || []).forEach((slot, i) => {
        const code = picks[i];
        if (!code) return;
        for (const n of slot.nodes) {
            const to = moves[n] || n;
            if (s.cells[to]) s.cells[to].part = code;
        }
    });
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
        const pic = recipePicture(p);
        return '<button class="os-comp-recipe" data-act="recipe" data-preset="' + esc(p.id) + '">' +
            '<span class="nm">' + esc(p.name) + '</span>' +
            '<span class="where">' + esc(p.where || '') + '</span>' +
            '<svg class="os-flow-picture" viewBox="0 0 1280 ' + pic.height + '" preserveAspectRatio="xMidYMin meet">' +
            pic.svg + '</svg></button>';
    }).join('');
    show().innerHTML =
        '<div class="os-comp-scrim on"></div>' +
        '<div class="os-comp-sheet on" role="dialog" aria-label="Which one?">' +
        '<div class="os-comp-sheet-h"><h2>Which one? · ' + esc(picks.map(p => M().shortPart(p)).join(' + ')) + '</h2>' +
        '<span class="sp"></span><button class="os-btn os-btn-quiet" data-act="topart">Back</button></div>' +
        '<div class="os-comp-which">' + cards + '</div></div>';
}

// ── positions (R-K) ──────────────────────────────────────────────────────────
// Only with the engineer's switch on. The recipe's slots on the cell picture;
// tap one, then the position or lane it should use instead. Same
// choreography, different core node — mode, pairing and role stay locked, and
// the server refuses a move the switch does not allow (engine.gateRefusal).
function drawPositions() {
    const pic = recipePicture(recipe, { editable: true, selected: selected || null });
    const list = Object.keys(moves).map(n =>
        '<button class="os-chip btn" data-act="unmove" data-node="' + esc(n) + '">' +
        esc(n + ' → ' + moves[n]) + ' ✕</button>').join('');
    show().innerHTML =
        '<div class="os-comp-full">' +
        '<div class="os-comp-hdr"><h1>Positions · ' + esc(recipe.name) + '</h1>' +
        '<span class="sub">' + esc(picks.map(p => M().shortPart(p)).join(' + ')) + '</span>' +
        '<span class="sp"></span><button class="os-btn os-btn-quiet" data-act="topart">Back</button></div>' +
        '<div class="os-comp-stage">' +
        '<div class="os-comp-hint">' + (selected
            ? 'Now tap the <b>position or lane</b> ' + esc(selected) + ' should use instead — or tap it again to keep it'
            : 'Tap a <b>slot</b> to move it to another position or lane · nothing to move? <b>Next</b>') + '</div>' +
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

function onPositionsTap(node) {
    if (!node) return;
    const original = n => Object.keys(moves).find(k => moves[k] === n) || n;
    if (!selected) {
        if (recipeNodes().has(node)) selected = original(node);
    } else if (node === selected || node === moves[selected]) {
        selected = '';
    } else if (!recipeNodes().has(node)) {
        moves[selected] = node;
        selected = '';
    }
    drawPositions();
}

// ── go ───────────────────────────────────────────────────────────────────────
function mintBody() {
    return M().mintBody(recipe ? Number(recipe.id) : 0, picks, stationID(), moves);
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

// THE SENTENCE (brief U4): who moves, how many trips, where each part lands
// when more than one is placed, and what Core said about the parts. One
// sentence, then the receipt under <details>.
function goSentence(pv) {
    const robots = orderRobots(pv);
    const one = robots === 'Robot 1' || robots === 'Robot 2';
    const n = pv.order_count || 0;
    let s = (robots || 'The robots') + (n ? (one ? ' makes ' : ' make ') + n + ' trip' + (n === 1 ? '' : 's') + ' and' : '') +
        (one ? ' waits' : ' wait') + ' at the cell until you press Release.';
    const placed = pv.mint && pv.mint.placement ? pv.mint.placement : {};
    const nodes = Object.keys(placed).sort();
    if (picks.length > 1 && nodes.length) {
        s = nodes.map(node => M().shortPart(placed[node]) + ' on ' + node).join(', ') + '. ' + s;
    }
    if (pv.preflight) {
        s += pv.preflight.state === 'unchecked' ? ' Inventory not checked — Core is unreachable.'
            : pv.preflight.state === 'missing' ? ' Core has no bin for ' + (pv.preflight.missing || []).join(', ') + '.'
                : ' Inventory checked.';
    }
    return s;
}

function drawGo() {
    const pv = preview;
    const name = (pv && pv.mint && pv.mint.style_name) || picks.map(p => M().shortPart(p)).join(' + ');
    let body, acts;
    const back = '<button class="os-btn os-btn-quiet os-btn-lg" data-act="topart">Back</button>';
    if (!pv) {
        body = '<p class="os-comp-sentence">Checking what this changeover does…</p>';
        acts = back;
    } else if (previewStatus === 404) {
        screen = 'none';
        drawNone();
        return;
    } else if (pv.running || (pv.mint && pv.mint.running)) {
        body = '<p class="os-comp-sentence">' + esc(name) + ' is already running — there is nothing to change over.</p>';
        acts = back;
    } else if (pv.error && !pv.actions) {
        body = '<p class="os-comp-sentence os-comp-refusal">' + esc(pv.error) + '</p>';
        acts = back;
    } else {
        const orders = M().orderSentences({ preview: pv }).map(o =>
            '<tr><td class="r' + o.robot + '">Robot ' + o.robot + '</td><td>' + esc(M().orderTrip(o)) + '</td></tr>').join('');
        const placed = (pv.mint && pv.mint.placement) || {};
        const rows = Object.keys(placed).sort().map(node =>
            '<tr><td>' + esc(node) + '</td><td>' + esc(M().shortPart(placed[node])) + '</td></tr>').join('');
        const refused = pv.error ? '<div class="os-comp-refusal">' + esc(pv.error) + '</div>' : '';
        body = '<p class="os-comp-sentence">' + esc(goSentence(pv)) + '</p>' + refused +
            '<details class="os-comp-receipt"><summary>Positions and orders</summary>' +
            '<div class="cols"><div><div class="os-lbl">Parts on positions</div>' +
            '<table><tr><th>Position</th><th>Part</th></tr>' + rows + '</table></div>' +
            '<div><div class="os-lbl">Orders that fire</div>' +
            '<table><tr><th>Robot</th><th>From → to</th></tr>' + orders + '</table></div></div></details>';
        acts = back + (pv.error ? '' : '<button class="os-btn os-btn-primary os-btn-lg" data-act="start">Start changeover</button>');
    }
    show().innerHTML =
        '<div class="os-comp-scrim on"></div>' +
        '<div class="os-comp-confirm on"><h2>' + (acts === back && pv && (pv.running || (pv.mint && pv.mint.running))
            ? esc(name) : 'Change over to ' + esc(name) + '?') + '</h2>' + body +
        '<div class="acts">' + acts + '</div></div>';
}

// ── start ────────────────────────────────────────────────────────────────────
// Mint (or resolve) at Go, then start on the fingerprint it returns.
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
    // by name on the sheet the operator is standing on.
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
    const sheet = root().querySelector('.os-comp-confirm');
    if (!sheet) return;
    let el = sheet.querySelector('.os-comp-refusal');
    if (!el) {
        el = document.createElement('div');
        el.className = 'os-comp-refusal';
        sheet.insertBefore(el, sheet.querySelector('.acts'));
    }
    el.textContent = (minted ? 'The flow was made, and the changeover was refused: ' : '') +
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
        (res && res.minted ? ' New flow from ' + (recipe ? recipe.name : 'the recipe') + '.' : '');
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
        case 'style': onStyle(+btn.dataset.style); break;
        case 'topart': openPicker(); break;
        case 'just': resolve(); break;
        case 'second': screen = 'second'; drawPartSheet('Second part'); break;
        case 'recipe': {
            recipe = presets().find(p => String(p.id) === btn.dataset.preset) || null;
            if (recipe) afterRecipe();
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
    picks = opt('parts') ? opt('parts').split(',') : [];
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
        case 'part': openPicker(); break;
        case 'two': screen = 'two'; drawTwo(); break;
        case 'second': screen = 'second'; drawPartSheet('Second part'); break;
        case 'which': screen = 'which'; drawWhich(fits(picks)); break;
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
    if (screen === 'part' || screen === 'second') drawPartRows(($('os-comp-q') || {}).value || '');
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
    if (screen === 'positions' || screen === 'which' || screen === 'two' || screen === 'none') { openPicker(); return; }
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
