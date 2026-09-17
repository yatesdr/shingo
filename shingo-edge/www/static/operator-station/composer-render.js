// composer-render.js — the Flow Composer's screens, drawn from model state.
//
// Ported from REFERENCE-press400-flow-composer-HK-2026-09-03.html. The sizes,
// copy, order and colours are that file's and SPEC-composer-ux-2026-09-03's;
// nothing here chooses one. What changed in the port: the reference's inlined
// DATA became the station view, its `state` global became composer-model.js, and
// its own renderPress became a call into operator-flow.js's renderFlowPicture —
// there is ONE drawing of this press on this station (U4's read-only picture and
// this one are the same function, called with different options).
//
// TWO SCREENS, EVERYTHING ELSE A SHEET OR A PANEL (SPEC §0.1). The board is
// untouched; S2/S3/S9/S10 are sheets over it, S4–S8 are the picture and panels
// over that.
//
// THE HASH ENTRY. #compose=<styleId>;state=<S2…S10>[;node=PLN_03] sets LOCAL UI
// STATE ONLY and never writes — the same pattern U4 used for #flow, and how
// scripts/composer-shots.sh reaches each state without driving the DOM.

import { renderFlowPicture, pictureRows, sentencesFromModel } from './operator-flow.js';
import { esc } from '/static/shared/esc.js';

const M = () => window.ComposerModel;
// composer-glyphs.js ships verbatim (brief R6) and declares `function glyph`,
// which a classic <script> hangs on window. Resolved here rather than edited
// there: that file is the one drawing of these four choreographies, and a fork
// of it is a second set of geometry nobody would keep in step.
const gl = (mode, size, opts) => (typeof window.glyph === 'function' ? window.glyph(mode, size, opts || {}) : '');

// ── module state ─────────────────────────────────────────────────────────────
let view = null;        // the station view, as the board has it
let flowPayload = null; // the composer's own read — see ensureFlow
let flowPayloadFor = 0; // the process flowPayload was fetched for
let flowPending = null; // the in-flight fetch, so two taps make one request
let flowPendingFor = 0;
let model = null;       // composer-model state
let screen = null;      // 'S2' … 'S10'
let panelFor = null;    // the position or dock half a popover is open on
let previewTimer = null;
let previewAbort = null;
let savedFingerprint = '';
let countdown = null;

const $ = id => document.getElementById(id);

function gate() { return !!(view && view.process && view.process.flow_composer_enabled); }
function processID() { return view && view.process ? view.process.id : 0; }
function stationID() { return view && view.station ? view.station.id : 0; }
// THE PICKER'S FACTS RIDE THE VIEW; THE COMPOSER'S ARE FETCHED ON OPEN.
//
// The board's style list is bare names, so the picker still needs a block of
// its own on the view: the claim count, the flow summary and the parts. What
// it does NOT need is every style's stored cells, the routing set, the preset
// cards and the travel graph — and those used to ride the same poll, rebuilt
// and discarded at 500 ms because the composer reads them once, on a tap.
//
// So `comp()` is the view's block and `flow()` is the composer's own read
// (GET /api/operator-stations/{id}/composer), fetched when the composer opens
// and held for the session. The server builds both from one builder in
// widening scopes, so flow()'s style blocks are a superset of comp()'s —
// which is what lets styleFor() below merge them without either side
// inventing a field.
function comp() { return (view && view.composer) || {}; }
function styles() { return comp().styles || []; }
function styleByID(id) { return styles().find(s => s.id === id) || null; }
function flow() { return flowPayload || {}; }
function routing() { return flow().routing || []; }
function activeStyleID() { return view && view.process ? view.process.active_style_id : 0; }

// styleFor is one style as every screen past the picker needs it: the view's
// live picker facts with the fetched flow's cells and provenance over them,
// and the two derivable summaries filled in.
//
// THE FILL IS WHY THIS GOES THROUGH styleFacts. `parts` and `claim_nodes` ride
// the PICKER scope only — on a composer scope they would be the same strings
// the cells already carry — so a merged block has them from `base`, and the
// arm below that returns the fetched block ALONE (no picker row for this
// style, which is what a style created since the last poll looks like) has
// neither. One helper answers both cases and the desktop's.
function styleFor(id) {
    const base = styleByID(id);
    const f = (flow().styles || []).find(s => s.id === id);
    const merged = base ? (f ? Object.assign({}, base, f) : base) : f;
    if (!merged) return null;
    return Object.assign({}, merged, M().styleFacts(merged));
}

// ensureFlow fetches the composer payload once per process and holds it.
//
// RE-FETCHED ON A 409 AND NOT OTHERWISE. A stale save is the one event that
// means the stored rows moved under this copy; everything else the composer
// does is local until it saves. onStale() clears it.
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
// show() rather than root() at every draw site: the root is created hidden, and
// a screen that sets innerHTML without clearing `hidden` renders perfectly into
// a box nobody can see. That cost a full shots run — the picker cleared it and
// nothing else did, so every state after S2 photographed the board underneath.
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
    screen = null; model = null; panelFor = null;
    // THE HELD PAYLOAD GOES WITH THE SESSION IT WAS FETCHED FOR.
    //
    // ensureFlow caches for the PAGE's life, and an HMI runs for weeks: a part
    // set an engineer edits on the desktop moves no fingerprint and fires no
    // event this page listens to, so a station that opened the composer once on
    // Monday would offer Monday's part list for the rest of the week and never
    // know. Clearing it here makes the boundary the COMPOSER SESSION rather
    // than the page — a few 7-query fetches a shift, on a read that only
    // happens when an operator taps CHANGEOVER.
    //
    // onStale() clears it too, for the other reason: a 409 means the stored
    // rows moved under this copy.
    flowPayload = null;
    flowPayloadFor = 0;
    if (countdown) { clearInterval(countdown); countdown = null; }
    if (previewAbort) { previewAbort.abort(); previewAbort = null; }
}

// ── S2 · picker ──────────────────────────────────────────────────────────────
// Slides up over the board. The search field is the SCANNER TARGET — focused on
// open, no on-screen keypad (SPEC S2) — and filtering is client-side over the
// view's styles, so a scan never waits on a round trip.
export function openPicker(v) {
    if (v) view = v;
    screen = 'S2';
    buildPickerIndex();
    show().innerHTML =
        '<div class="os-comp-scrim on"></div>' +
        '<div class="os-comp-sheet on" role="dialog" aria-label="Change over to">' +
        '<div class="os-comp-sheet-h"><h2>Change over to</h2>' +
        '<label class="os-comp-search">' +
        // The magnifier the reference draws. inputmode="none" keeps the on-screen
        // keypad away: this field is the SCANNER's target (SPEC S2), and a keypad
        // over a 700 px sheet hides the rows the scan is meant to filter.
        '<svg class="mag" width="18" height="18" viewBox="0 0 18 18" aria-hidden="true">' +
        '<circle cx="7.5" cy="7.5" r="5.5" fill="none" stroke="currentColor" stroke-width="1.6"/>' +
        '<path d="M11.5 11.5 L16 16" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>' +
        '<input id="os-comp-q" type="text" autocomplete="off" ' +
        'placeholder="scan or type a part, CATID, or flow" inputmode="none"></label>' +
        '<button class="os-btn os-btn-quiet" data-act="close">Close</button></div>' +
        '<div class="os-comp-sheet-b" id="os-comp-pick"></div></div>';
    drawPickerRows('');
    const q = $('os-comp-q');
    if (q) { q.addEventListener('input', () => drawPickerRows(q.value)); q.focus(); }
}

// RECENT is the last 4 distinct targets from the process's changeover history.
function recentStyleIDs() {
    return (comp().recent_targets || []).slice(0, 4);
}

// THE SEARCH FIELD IS THE SCANNER'S TARGET (SPEC S2), so what it costs per
// character is the whole point. This was called about three times per style
// per keystroke — twice in the filter (once for the full list and once for
// RECENT) and once more for the row it renders — and each call built a Set
// and asked modeLabels() for a FRESH COPY of the mode table. At 40 styles and
// a ~15-character scan that is 1,800 summaries, 1,800 Sets and 1,800 object
// copies for one scan of one barcode.
//
// It is computed once per style when the picker opens, with the labels
// hoisted, and the search runs over a pre-lowercased haystack.
function flowSummary(s, labels) {
    const modes = [...new Set((s.claim_modes || []))];
    if (!modes.length) return 'no flow yet';
    return modes.map(m => labels[m] || m).join(' + ') +
        (s.claim_nodes && s.claim_nodes.length ? ' · ' + s.claim_nodes.map(n => n.replace('PLN_', 'P')).join('/') : '');
}

// pickerIndex is the rows the search filters: each style with its summary and
// the lowercased text a scan is matched against. Rebuilt when the picker opens
// and when a new view arrives, never per keystroke.
let pickerIndex = null;

function buildPickerIndex() {
    const labels = M().modeLabels();
    pickerIndex = styles().map(s => {
        const summary = flowSummary(s, labels);
        return {
            s: s, summary: summary,
            // Joined with a character no part name carries, so a needle
            // cannot match across the boundary between two fields.
            hay: [s.name || '', s.catid || '', summary].join('  ').toLowerCase(),
        };
    });
}

// The four verdicts of SPEC S2 (owner 2026-09-10: four, not five — the test
// payload word is deferred to the payload-identity work, because nothing on
// this tree marks one).
//
// The verdict comes from the SAME feed the board's picker reads: the view's
// top-level sourcing_by_style, keyed by style NAME, with Core's own four codes.
// Read here rather than copied into the composer block so there is one live
// answer on this screen instead of two that can disagree.
//
// A red style stays TAPPABLE: the operator is authoritative, and findings block
// save, never selection (SPEC §0.12).
function sourcingFor(styleName) {
    return ((view && view.sourcing_by_style) || {})[styleName] || null;
}

function verdict(s) {
    // A STYLE WITH NO FLOW SAYS SO, WHICHEVER WAY THE GATE IS SET. It read "Set
    // up from the desktop" with the gate off, which was a verdict about
    // permission on a column of verdicts about PARTS — and it is not even true
    // any more: a first flow may be applied here from a preset (R3). What the
    // row now says is the fact, and the card behind it says what can be done
    // about it.
    if (!s.claim_count) return { cls: 'build', text: 'No flow yet' };
    const src = sourcingFor(s.name);
    switch (src && src.code) {
        case 'green': return { cls: 'ok', text: 'Parts available' };
        case 'red': return { cls: 'no', text: 'No parts' };
        // Core's third tier. "Running low" is the board's word for it and it is
        // not one of the spec's four, but it is the truth Core sent and the
        // amber pill is where it belongs — flattening it into "Parts available"
        // would hide a shortage the operator is about to walk into.
        case 'yellow': return { cls: 'unv', text: 'Running low' };
        default: return { cls: 'unv', text: 'Unverified' };
    }
}

function pickerRow(row) {
    const s = row.s;
    const v = verdict(s);
    // R7 SAID THIS ROW WAS DEAD, AND IT IS NOT (U10 §3 C1). With the gate off a
    // style nobody had set up rendered `disabled` and labelled "Set up from the
    // desktop" — true about BUILDING a flow and false about the thing the
    // operator was standing there to do. The cell already runs this shape for
    // eight other parts; a preset an engineer named is exactly that shape, and
    // applying one to a part with no flow is the one write R3 opens the gate
    // for (engine.gateRefusal).
    //
    // So the row is tappable and what it opens says what is possible: the
    // set-up card's no-flow variant, with the preset cards at full size and
    // "Start blank" only where the gate allows one.
    const last = s.last_run ? ' · ran ' + esc(s.last_run) : '';
    return '<button class="os-comp-row"' +
        ' data-act="pick" data-style="' + s.id + '">' +
        '<span class="id">' + esc(s.name) + '</span>' +
        '<span class="meta"><b>' + esc(row.summary) + '</b>' + last + '</span>' +
        '<span class="vd ' + v.cls + '">' + esc(v.text) + '</span></button>';
}

function drawPickerRows(q) {
    if (!pickerIndex) buildPickerIndex();
    const needle = String(q || '').trim().toLowerCase();
    // One indexOf over one pre-built string per style. The name, the CATID and
    // the flow summary are all in it, separated by a character no name carries.
    const match = row => !needle || row.hay.indexOf(needle) >= 0;
    const all = pickerIndex.filter(match);
    const recentIDs = recentStyleIDs();
    const recent = recentIDs.map(id => all.find(r => r.s.id === id)).filter(Boolean);
    const rest = all.filter(r => recent.indexOf(r) < 0);
    let h = '';
    if (recent.length) {
        h += '<div class="grp"><span class="os-lbl">Recent</span><span class="n">from changeover history</span></div>' +
            recent.map(pickerRow).join('');
    }
    h += '<div class="grp"><span class="os-lbl">All parts</span><span class="n">' + rest.length + ' live styles</span></div>' +
        rest.map(pickerRow).join('');
    const box = $('os-comp-pick');
    if (box) box.innerHTML = h;
}

// ── S3 · set-up card ─────────────────────────────────────────────────────────
//
// THE CARD SAYS WHAT IS ABOUT TO HAPPEN, AND ITS BUTTON SAYS THE VERB THE
// OPERATOR KNOWS (owner, 2026-09-10: "this wording doesn't seem professional …
// I want it similar to how operators work today when it's locked down").
//
// It used to read `SET UP ALREADY · Run as is · Change the flow`. Three
// problems, all of them wording:
//
//   - "SET UP ALREADY" is the system talking about its own records. The
//     operator picked a part to change over to; the label is the picker's own
//     words back at them, `CHANGE OVER TO`.
//   - "Run as is" is an answer to a question nobody asked. On a locked-down
//     press there is no alternative to run, so the primary is the verb:
//     `Start changeover`.
//   - "Change the flow" sat beside the primary as an equal. It is not: it is
//     the rare path, so it is quiet, underneath, and reads `Edit the flow
//     first` — which says what it does to the thing about to happen.
//
// WITH THE GATE OFF THE CARD HAS EXACTLY ONE BUTTON and nothing explaining
// what the operator cannot do. The gate removes a button, not a screen, and a
// sentence about a door that is locked is worse than no door: it is the system
// telling the floor there is a half of it they are not allowed to understand.
// (A RUNNING style is the one exception, and it keeps its sentence, because
// that is not a permission — the part is on the press and the change lands at
// the next changeover, which the operator has to know.)
//
// BEHAVIOUR IS UNCHANGED (R8). The primary previews the STORED cells and goes
// to the confirm; nothing is saved from here, whatever the button says.
function openSetup(styleID) {
    const s = styleFor(styleID);
    if (!s) return;
    screen = 'S3';
    buildModel(s);
    drawSetup(s);
    // The provenance line's order count comes from a preview of the stored
    // cells — the same read the primary button makes, run early so the card
    // can say how many orders the change will fire. READ-ONLY: flow/preview
    // writes nothing, and R8's "never a save from S3" is untouched.
    //
    // A STYLE WITH NO FLOW IS NOT PREVIEWED. There are no cells to plan, the
    // no-flow card has no provenance line to fill, and the round trip would
    // come back saying the flow fires no orders — which is true and is not news
    // to anybody looking at a card that says "No flow yet".
    if (s.claim_count || (s.claims || []).length) {
        runPreview().then(() => { if (screen === 'S3') drawSetup(s); });
    }
}

// The dim line under the chips: where the flow came from, when, and what it
// will do — `Flow saved 09‑02 from the desktop · 4 orders · Robot 1 and Robot 2`.
//
// THE SURFACE IS THE SERVER'S ANSWER, not a guess here. Both surfaces POST the
// same save route and the HANDLER decides the authorship from the
// authenticated caller (owner ruling R1), so `saved_from` on the style block
// is either "the desktop" or the station that saved it. A style whose claims
// disagree carries no surface and the line says when without saying where,
// which is the honest shape of "half of this was written somewhere else".
function setupProvenance(s) {
    const parts = [];
    if (s.saved_on || s.saved_from) {
        parts.push('Flow saved' + (s.saved_on ? ' ' + s.saved_on : '') +
            (s.saved_from ? ' from ' + s.saved_from : ''));
    }
    const pv = model && model.preview;
    if (pv && pv.orderCount) {
        parts.push(pv.orderCount + ' order' + (pv.orderCount === 1 ? '' : 's'));
    }
    parts.push((s.claim_count || 0) + ' position' + (s.claim_count === 1 ? '' : 's'));
    parts.push(M().robotWords(model));
    return parts.join(' · ');
}

// THE SET-UP CARD WITH NO FLOW TO SET UP (§3 C1).
//
// The card's ordinary job is "here is what will happen, press Start". A style
// nobody has configured has nothing to start, so the same card becomes the
// place a flow is CHOSEN: the cell's named shapes, full size, one tap each.
//
// "START BLANK" ONLY WITH THE GATE ON. Applying a named shape to a part with no
// flow is the one write R3 opens a gated-off cell for; authoring one from
// nothing is not, and an operator offered a button the server will refuse is
// an operator sent to find out the hard way.
//
// AN EMPTY STRIP SAYS WHO MAKES THESE. A cell with no presets and the gate off
// has nothing on this card at all, and a card with nothing on it is the dead
// end the disabled row used to be — one sentence further in.
function drawSetupNoFlow(s) {
    const presets = flow().presets || [];
    const cards = presets.map(p =>
        '<button class="os-comp-preset lg" data-act="preset-apply" data-preset="' + esc(p.id) + '"' +
        ' title="' + esc(p.name) + (p.where ? ' · ' + esc(p.where) : '') + '">' +
        gl(p.mode, 40, {}) +
        '<span><span class="nm">' + esc(p.name) + '</span>' +
        (p.where ? '<span class="where">' + esc(p.where) + '</span>' : '') +
        '<span class="use">' + esc(p.use || '') + '</span></span></button>').join('');
    const blank = gate()
        ? '<button class="os-comp-quiet" data-act="compose-blank">Start blank</button>'
        : '';
    const empty = presets.length ? ''
        : '<div class="os-comp-note">This cell has no named shapes yet. An engineer names one on the ' +
          'desktop — Processes › Presets — from a flow that already runs here, and it appears on this card.</div>';
    show().innerHTML =
        '<div class="os-comp-scrim on"></div>' +
        '<div class="os-comp-setup on"><div class="os-comp-card">' +
        '<div class="top"><div>' +
        '<span class="os-lbl">Change over to</span>' +
        '<h2>' + esc(s.name) + '</h2>' +
        '<div class="desc">' + esc('No flow yet · pick the shape this part runs') + '</div>' +
        '</div><button class="x" data-act="topick" aria-label="Back to the part list">&#10005;</button></div>' +
        '<div class="os-comp-presets-lg">' + cards + '</div>' +
        empty +
        '<div class="actions">' + blank + '</div></div></div>';
}

function drawSetup(s) {
    if (!(s.claim_count || (s.claims || []).length)) { drawSetupNoFlow(s); return; }
    const running = s.id === activeStyleID();
    const parts = (s.parts || []).map(p =>
        '<span class="os-chip part">' + esc(M().shortPart(p.payload_code || p)) +
        (p.node ? ' <small>→ ' + esc(p.node) + '</small>' : '') + '</span>').join('');
    // A running style keeps its sentence; the gate's absence never gets one.
    // `+ Add a part` is gone from here: a part is added ON the picture, where
    // the position it lands on is visible (owner ruling, F6).
    let secondary = '';
    if (running) {
        secondary = '<div class="os-comp-note">This part is running — change its flow from the desktop, or after the next changeover</div>';
    } else if (gate()) {
        secondary = '<button class="os-comp-quiet" data-act="compose">Edit the flow first</button>';
    }
    const glyphFor = model ? firstMode(s) : '';
    // A PART WITH NO POSITION STOPS THE START (owner ruling R8, 2026-09-12).
    // Not the save — the flow can be half-built and kept — but a changeover
    // the robots cannot serve for one of this part's payloads is a line that
    // runs dry on it, and this card is the last screen before the robots move.
    // The chips above already show WHICH parts are loose; this says how many,
    // and takes the button away until they are placed.
    const loose = model ? M().findings(model).find(f => f.field === 'unplaced_part') : null;
    show().innerHTML =
        '<div class="os-comp-scrim on"></div>' +
        '<div class="os-comp-setup on"><div class="os-comp-card">' +
        '<div class="top"><div>' +
        '<span class="os-lbl">Change over to</span>' +
        '<h2>' + esc(s.name) + '</h2>' +
        '<div class="desc">' + (glyphFor ? gl(glyphFor, 26) : '') + esc(setupMeta(s)) + '</div>' +
        '</div><button class="x" data-act="topick" aria-label="Back to the part list">&#10005;</button></div>' +
        '<div class="os-chips">' + parts + '</div>' +
        '<div class="os-comp-prov">' + esc(setupProvenance(s)) + '</div>' +
        (loose ? '<div class="os-comp-need">' + esc(loose.message) + '</div>' : '') +
        '<div class="actions"><button class="os-btn os-btn-primary os-btn-lg" data-act="runasis"' +
        (loose ? ' disabled' : '') + '>Start changeover</button>' +
        secondary + '</div></div></div>';
}

// The one meta line: the choreography, the positions it runs on, and when the
// part last ran here.
//
// THE POSITIONS ARE SPELLED OUT. A picker ROW abbreviates them (`P01/P04`)
// because it is one line of a list an operator scans; this card is the one
// screen about this one part, and the names it uses have to be the names on
// the press and on the picture behind it.
function setupMeta(st) {
    const modes = [...new Set(st.claim_modes || [])];
    const labels = M().modeLabels();
    const bits = [modes.length ? modes.map(m => labels[m] || m).join(' + ') : 'no flow yet'];
    if (st.claim_nodes && st.claim_nodes.length) bits.push(st.claim_nodes.join(' / '));
    if (st.last_run) bits.push('last run ' + st.last_run);
    return bits.join(' · ');
}

// firstMode is the glyph the meta line carries: the style's own swap mode when
// it has one, and nothing when the modes disagree — a press running two
// choreographies has no single picture, and one of the two would be a lie.
function firstMode(s) {
    const modes = [...new Set(s.claim_modes || [])];
    return modes.length === 1 ? modes[0] : '';
}

// ── S4 · the picture ─────────────────────────────────────────────────────────
// THE COMPOSER'S OWN PICTURE, NOT THE BOARD'S (SYNTH §3 B2).
//
// This read `view.cell` — the POLLED picture, which is the board's: the
// staging the RUNNING flow parks at, and nothing the cell merely could park
// at. So the composer, the one screen whose job is choosing where a bin goes,
// was the surface with no staging options on it. The composer's own fetch
// carries a station-scoped Cell that also holds the lanes the routing set
// offers; the board's stays what it was.
//
// `|| view.cell` is the fallback and it is a real path, not defensiveness: the
// hash entry can open a state before ensureFlow has resolved, and a picture
// one field short beats a blank stage.
function composerCell() {
    return (flow() && flow().cell) || (view && view.cell) || { positions: [] };
}

function buildModel(s) {
    const cell = composerCell();
    model = M().init({
        styleId: s.id,
        styleName: s.name,
        positions: (cell.positions || []).map(p => ({
            core_node_name: p.core_node_name, kind: p.kind, sequence: p.sequence,
        })),
        routing: routing(),
        // The rows that are switched off, by role — a count, not rows. It is
        // what lets an empty row say which of the two empties it is; see
        // composer-model's routingNote.
        routingOff: (flow() && flow().routing_off) || {},
        // The travel network, for the "Robot drives via" walk. Indexed once
        // by the model rather than rebuilt on every panel open.
        scene: flow().scene || null,
        claims: s.claims || [],
        parts: (s.parts || []).map(p => p.payload_code || p),
        // THE PROCESS'S PART SET, which is a different list from the style's
        // parts above: this is what the part picker OFFERS, and it is why a
        // cell nobody has configured can be given a payload at all.
        palette: flow().palette || [],
        // For the picker's order alone — the part a scan just named goes to the
        // top. See composer-model's partOffers.
        catid: s.catid || '',
        lastRun: s.last_run || null,
        flowspec: window.FLOWSPEC || null,
        // The dock strip's third line is the routing group's members, and the
        // picture already carries that map.
        groups: cell.groups || {},
        // Every claim this process has, across all its styles: a style with no
        // claims of its own derives its role from what the press does elsewhere
        // rather than defaulting to consume (owner ruling 2026-09-10).
        // The cells live on the fetched payload, not on the polled view.
        processClaims: (flow().styles || []).reduce((all, st) => all.concat(st.claims || []), []),
    });
    savedFingerprint = '';
    // A fresh model is a fresh session with this style; the self-opening panel
    // is armed again for the first apply made in it. See openFirstUnanswered.
    firstUnansweredArmed = true;
}

function openComposer(styleID, blank) {
    const s = styleFor(styleID);
    if (!s) return;
    screen = 'S4';
    if (!model || model.styleId !== styleID) buildModel(s);
    if (blank) model = M().reduce(model, { type: 'startBlank' });
    drawComposer();
    schedulePreview();
}

function drawComposer() {
    const s = styleFor(model.styleId) || { name: model.styleName };
    const sub = (s.claim_count || (s.claims || []).length)
        ? 'changing the flow · last run ' + esc(model.lastRun || '—')
        : 'building the flow · never run here';
    show().innerHTML =
        '<div class="os-comp-full">' +
        '<div class="os-comp-hdr"><h1>' + esc(model.styleName) + '</h1><span class="sub">' + sub + '</span>' +
        '<span class="sp"></span><button class="os-btn os-btn-quiet" data-act="close">Cancel</button></div>' +
        '<div class="os-comp-strip" id="os-comp-strip"></div>' +
        '<div class="os-comp-stage" id="os-comp-stage">' +
        '<div class="os-comp-hint">Tap a <b>position</b> to set how it swaps, which part, and where bins come from and go · tap the strip below to change the defaults</div>' +
        '<div class="os-comp-legend"><span><i class="r1"></i>Robot 1</span><span><i class="r2"></i>Robot 2</span></div>' +
        '<svg id="os-comp-svg" class="os-flow-picture" viewBox="0 0 1280 560"></svg>' +
        '<div class="os-comp-pop" id="os-comp-pop" hidden></div></div>' +
        '<div class="os-comp-bar" id="os-comp-bar"></div></div>';
    drawStrip();
    drawPicture();
    drawBar();
}

// THE PART PALETTE HAS ITS DOOR NOW, AND BOTH BUTTONS OPEN IT.
//
// `+ Add a part` (the strip) and `+ another part` (the S5 part row) are the
// same control drawn twice, and both used to be rendered DISABLED with their
// reason in the title, because a part reaches a flow by being claimed on a
// position and this surface had nowhere to pick a payload from. The process's
// part set is that somewhere (domain.ComposerData.Palette), and the picker
// sheet below is the pick.
//
// THE BUTTON IS STILL DISABLED WHEN THE SET IS EMPTY, with a title that names
// where the set is made. A live-looking button that opens an empty sheet is
// the dead control this pattern exists to avoid, one screen further in.
const ADDPART_EMPTY_TITLE = 'This process has no part set yet. An engineer adds one on the ' +
    'desktop — Processes › Edit — and every part in it becomes pickable here.';

function drawStrip() {
    const presets = flow().presets || [];
    let h = '<span class="os-lbl">Flow</span>';
    // START BLANK IS FIRST WHEN THERE IS NOTHING TO PICK, and after the cards
    // when there is (§1b). With presets on the strip it is the fallback and
    // reads as one; with none it is the only thing to do, and a fallback drawn
    // after nothing is a strip that looks empty.
    const blank = '<button class="os-comp-preset" data-act="blank">' + gl('', 32, {}) +
        '<span><span class="nm">Start blank</span><span class="use">tap positions</span></span></button>';
    if (!presets.length) h += blank;
    for (const p of presets) {
        // THE CARD'S SECOND LINE IS THE SERVER'S, WHOLE (owner rulings R2 and
        // R5, 2026-09-12): `PLN_01 / PLN_04 · used by 7 parts`. The positions
        // come off the SHAPE, because the name is whatever an engineer typed
        // and a card that showed only a name could be two cards saying the
        // same word about two different presses' worth of positions. And
        // nothing is appended here: this used to add `· totes`, a fact about
        // the style in hand rather than about the card, and the word is gone
        // from every surface.
        //
        // THREE LINES, NOT TWO. The card is locked at 200x56 (U8 §A S4) with
        // 138 px of it for text, and `PLN_01 / PLN_04 · used by 7 parts` on
        // one line ellipsised to `PLN_01 / PLN_04 · use…` — cutting off the
        // half that says why the card is worth tapping. Abbreviating the
        // positions the way a picker row does still did not fit. A line each
        // does, inside the same box: the lock is the box, and three short
        // lines of 13/11/11 come to 39 px of 56.
        //
        // A TYPED NAME LONGER THAN THE CARD ELLIPSISES, with the whole of it —
        // and the positions — in the title.
        h += '<button class="os-comp-preset" data-act="preset" data-preset="' + esc(p.id) + '"' +
            ' title="' + esc(p.name) + (p.where ? ' · ' + esc(p.where) : '') + '">' +
            gl(p.mode, 32, {}) +
            '<span><span class="nm">' + esc(p.name) + '</span>' +
            (p.where ? '<span class="where">' + esc(p.where) + '</span>' : '') +
            '<span class="use">' + esc(p.use || '') + '</span></span></button>';
    }
    if (presets.length) h += blank;
    h += '<span class="sp"></span><span class="os-lbl">Parts</span>';
    for (const p of model.parts) {
        const used = Object.keys(model.cells).some(n => model.cells[n].part === p);
        h += '<span class="os-chip part' + (used ? '' : ' free') + '">' + esc(M().shortPart(p)) +
            (used ? '' : ' · unplaced') + '</span>';
    }
    h += addPartButton('os-chip add', '+ Add a part', '');
    const strip = $('os-comp-strip');
    if (strip) strip.innerHTML = h;
    reportStripFit();
}

// addPartButton is the one control, drawn in two places. `node` is the position
// the picker should place the part on when it is opened from a position panel,
// and empty from the strip, where a part arrives loose.
function addPartButton(cls, label, node) {
    const empty = !(model && model.palette && model.palette.length);
    return '<button class="' + cls + '" data-act="addpart" data-node="' + esc(node || '') + '"' +
        (empty ? ' disabled title="' + esc(ADDPART_EMPTY_TITLE) + '"' : '') +
        '>' + esc(label) + '</button>';
}

// ── the part picker sheet ────────────────────────────────────────────────────
//
// THE PICKER SHEET, NOT A CHIP WALL. A process's part set is a dozen or two
// names and each is 15+ characters; laid out as chips they wrap into a block an
// operator has to read rather than scan. This is the shape S2 already uses for
// the style list — scrolling rows over a search field that is the SCANNER's
// target — so the gun works here for the same reason it works there, and an
// operator who has used the picker once has used this.
//
// inputmode="none" for the same reason as S2: the field is scanned into, and an
// on-screen keypad over the sheet hides the rows the scan is meant to filter.
//
// IT IS A SHEET OVER THE COMPOSER, appended rather than replacing the root, so
// the picture and the strip stay visible behind it — the operator picking a
// part can see the positions it is about to go on.
let partPickerNode = '';

function openPartPicker(node) {
    if (!model) return;
    partPickerNode = node || '';
    const host = document.createElement('div');
    host.className = 'os-comp-layer';
    host.innerHTML =
        '<div class="os-comp-scrim on"></div>' +
        '<div class="os-comp-sheet on" role="dialog" aria-label="Add a part">' +
        '<div class="os-comp-sheet-h"><h2>' +
        (partPickerNode ? 'Add a part to ' + esc(partPickerNode) : 'Add a part to this flow') + '</h2>' +
        '<label class="os-comp-search">' +
        '<svg class="mag" width="18" height="18" viewBox="0 0 18 18" aria-hidden="true">' +
        '<circle cx="7.5" cy="7.5" r="5.5" fill="none" stroke="currentColor" stroke-width="1.6"/>' +
        '<path d="M11.5 11.5 L16 16" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>' +
        '<input id="os-comp-partq" type="text" autocomplete="off" ' +
        'placeholder="scan or type a part" inputmode="none"></label>' +
        '<button class="os-btn os-btn-quiet" data-act="partcancel">Close</button></div>' +
        '<div class="os-comp-sheet-b" id="os-comp-partpick"></div></div>';
    const r = show();
    const old = r.querySelector('.os-comp-layer');
    if (old) old.remove();
    r.appendChild(host);
    drawPartRows('');
    const q = $('os-comp-partq');
    if (q) { q.addEventListener('input', () => drawPartRows(q.value)); q.focus(); }
}

// The rows are the MODEL's (partOffers): which parts exist, which this style
// already runs, and the CATID order. This filters and draws them.
function drawPartRows(q) {
    const needle = String(q || '').trim().toLowerCase();
    const rows = M().partOffers(model)
        .filter(r => !needle || r.code.toLowerCase().indexOf(needle) >= 0);
    let h = '';
    if (!rows.length) {
        h = '<div class="grp"><span class="os-lbl">No match</span>' +
            '<span class="n">' + (needle ? 'nothing in this cell’s part set matches that'
            : 'this process has no part set yet') + '</span></div>';
    } else {
        h += '<div class="grp"><span class="os-lbl">Parts</span><span class="n">' +
            rows.length + ' in this cell’s part set</span></div>';
        for (const r of rows) {
            // THE FULL CODE ONLY WHEN IT SAYS SOMETHING THE SHORT ONE DOES NOT.
            // shortPart trims a known prefix and is a no-op on a plain part
            // number, so a row for `SYN-A-P016` printed that string twice,
            // side by side, in two type sizes — which reads as two facts and is
            // one.
            const short = M().shortPart(r.code);
            h += '<button class="os-comp-row" data-act="partpick" data-part="' + esc(r.code) + '">' +
                '<span class="id">' + esc(short) + '</span>' +
                (short === r.code ? '' : '<span class="meta">' + esc(r.code) + '</span>') +
                (r.onFlow ? '<span class="vd ok">already on this flow</span>' : '') +
                '</button>';
        }
    }
    const box = $('os-comp-partpick');
    if (box) box.innerHTML = h;
}

function closePartPicker() {
    const layer = root().querySelector('.os-comp-layer');
    if (layer) layer.remove();
    partPickerNode = '';
}

// THE CARD'S GEOMETRY, MEASURED AND PUBLISHED, like the position panel's
// (reportPanelFit) and for the same reason: the two things the U8 brief locks
// about this card — 200x56, and a 32 px glyph — are exactly what a screenshot
// shows and no test asserts. The glyph is written with inline width/height, so
// the way it goes wrong is a CSS rule reaching it from somewhere else; that
// already happened once to the popover's 26 px glyph
// (`.os-comp-stage svg { width: 100% }` stretched it to 171).
//
// Also published: whether either line of the card is ELLIPSISED, which is how
// the second line's count disappeared behind `PLN_01 / PLN_04 · use…`.
function reportStripFit() {
    const card = document.querySelector('#os-comp-strip .os-comp-preset[data-preset]');
    if (!card) return;
    const g = card.querySelector('svg');
    const nm = card.querySelector('.nm'), where = card.querySelector('.where'), use = card.querySelector('.use');
    const clipped = el => !!el && el.scrollWidth > el.clientWidth + 1;
    const r = card.getBoundingClientRect();
    document.body.dataset.composerStrip = JSON.stringify({
        cardW: Math.round(r.width), cardH: Math.round(r.height),
        glyphW: g ? Math.round(g.getBoundingClientRect().width) : -1,
        nameText: nm ? nm.textContent : null,
        whereText: where ? where.textContent : null,
        useText: use ? use.textContent : null,
        nameClipped: clipped(nm), whereClipped: clipped(where), useClipped: clipped(use),
    });
}

// The composer's model state, in the shape renderFlowPicture draws. The
// adapter is composer-model.js's; this names the base picture it draws over.
function cellFromModel() { return M().pictureCells(model, composerCell()); }

// WHICH ROW EACH POSITION LANDED IN, COMPUTED WITH THE DRAWING (P6).
//
// rowWord needs one label per panel open and used to get it by calling
// pictureRows(cellFromModel()) — which re-runs layoutPositions: the projection,
// the pairwise collision scan over every position, the row clustering. The
// whole layout, on a touch screen, to read one word that the drawing standing
// on screen had just computed. drawPicture is the one place the layout is
// decided, so it is the one place that records it.
let pictureRowOf = {};

function drawPicture() {
    const svg = $('os-comp-svg');
    if (!svg) return;
    const fs = M().findings(model);
    const byNode = {};
    for (const f of fs) if (f.node && !byNode[f.node]) byNode[f.node] = f.short;
    const cell = cellFromModel();
    pictureRowOf = pictureRows(cell);
    svg.innerHTML = renderFlowPicture(
        { cell: cell, station: view && view.station },
        {
            selected: model.selected, findings: byNode, editable: true,
            sentences: sentencesFromModel(model),
        });
}

function drawBar() {
    const b = M().bar(model);
    const fix = b.fixIt ? '<button class="os-comp-fix" data-act="fix">' + esc(b.fixIt.label) + '</button>' : '';
    const bar = $('os-comp-bar');
    if (!bar) return;
    bar.innerHTML =
        '<div class="status"><div class="l1' + (b.tone === 'blocked' ? ' bad' : '') + '">' + esc(b.heading) + '</div>' +
        '<div class="l2">' + esc(b.detail) + '</div></div>' + fix + '<span class="sp"></span>' +
        '<button class="os-btn os-btn-primary" data-act="confirm"' + (b.button.enabled ? '' : ' disabled') + '>' +
        esc(b.button.label) + '</button>';
}

// ── preview ──────────────────────────────────────────────────────────────────
// Debounced ≥400 ms, in-flight cancelled. The bar shows the LAST COMPLETED
// preview, never a spinner over a blank bar (SPEC S4).
function schedulePreview() {
    if (previewTimer) clearTimeout(previewTimer);
    previewTimer = setTimeout(runPreview, 400);
}

// preflight: ask Core whether the draft's parts exist. OFF on the edit loop —
// this runs on a 400 ms debounce, and a Core round trip per keystroke is a
// network call nobody reads: only the confirm sheet shows the answer. ON for
// the one preview the confirm sheet makes, which is the step before the robots
// move and the only place the answer changes what an operator does.
async function runPreview(opts) {
    if (!model) return;
    if (previewAbort) previewAbort.abort();
    previewAbort = new AbortController();
    const body = {
        to_style_id: model.styleId,
        cells: M().toCells(model),
    };
    const q = (opts && opts.preflight) ? '?preflight=1' : '';
    try {
        const res = await fetch('/api/processes/' + processID() + '/flow/preview' + q, {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body), signal: previewAbort.signal,
        });
        const json = await res.json().catch(() => ({}));
        // 400 carries the same fields as 200 — the flow fires no orders, which
        // is a thing to show, not an error to swallow.
        //
        // NOT EVERY 409 IS A STALE FINGERPRINT. The seam answers 409 three ways:
        // stale:true, "already running style N", and "already has an active
        // changeover". Only the first is fixed by previewing again; showing
        // "the flow changed since you previewed" over the other two tells the
        // operator to re-check a flow that is fine and says nothing about the
        // press already running the part they picked.
        if (res.status === 409 && json && json.stale) { onStale(json); return; }
        model = M().applyPreview(model, json);
    } catch (e) {
        if (e && e.name === 'AbortError') return;
    }
    if (screen === 'S4') { drawPicture(); drawBar(); }
}

function onStale(json) {
    // THE STORED ROWS MOVED, SO THE HELD PAYLOAD IS BEHIND THEM. This is the
    // one event that invalidates it — everything else the composer does is
    // local until it saves — so the next screen that needs the flow re-fetches
    // rather than editing on top of cells somebody else has already changed.
    flowPayload = null;
    flowPayloadFor = 0;
    ensureFlow();
    model = M().applyPreview(model, json || {});
    model.previewStale = true;
    if (screen !== 'S4') { screen = 'S4'; drawComposer(); }
    const bar = $('os-comp-bar');
    if (bar) {
        bar.innerHTML = '<div class="status"><div class="l1 bad">The flow changed since you previewed — check it and try again</div>' +
            '<div class="l2">' + esc((json && json.error) || '') + '</div></div>';
    }
    schedulePreview();
}

// ── S5 · position panel / S6 · dock panel ────────────────────────────────────
function openPositionPanel(node) {
    model = M().reduce(model, { type: 'select', node: node });
    const c = model.cells[node];
    if (!c.on) model = M().reduce(model, { type: 'addPosition', node: node });
    panelFor = { kind: 'pos', node: node };
    drawPicture();
    const cc = model.cells[node];
    const labels = M().modeLabels(), help = M().modeHelp();
    const pos = model.positions.find(p => p.core_node_name === node) || {};
    const seg = Object.keys(labels).map(m =>
        '<button class="' + (cc.mode === m ? 'on' : '') + '" data-act="mode" data-mode="' + m + '">' +
        gl(m, 26, { rest: cc.mode !== m }) + '<span>' + esc(labels[m]) + '</span></button>').join('');
    const partChips = model.parts.map(p =>
        '<button class="os-chip btn ' + (cc.part === p ? 'on' : '') + '" data-act="part" data-part="' + esc(p) + '">' +
        esc(M().shortPart(p)) + '</button>').join('') +
        (cc.part ? '' : '<span class="os-chip need">pick one</span>') +
        addPartButton('os-chip btn', '+ another part', node);

    const backs = model.positions.filter(p => p.kind === 'back').map(p => p.core_node_name);
    // The view already filtered to ENABLED members (a retired lane is not an
    // option to offer), so these read role alone.
    //
    // THE PICTURE'S OWN BAND IS IN THE LIST TOO, and it is the same set: the
    // composer's Cell carries the enabled staging-role routing nodes as cards
    // (domain.CellPicture.Staging), so a lane an operator can SEE in the band
    // is a lane they can pick in the row. A card with no chip behind it is the
    // shape this whole unit exists to remove.
    const drawn = ((composerCell().staging) || []).map(st => st.core_node_name);
    const staging = [...new Set(routing()
        .filter(r => r.role === 'staging').map(r => r.core_node_name).concat(backs).concat(drawn))];
    const srcs = routing().filter(r => r.role === 'source');
    const dests = routing().filter(r => r.role === 'destination');
    const chips = (list, act, cur, attr) => list.map(o => {
        const val = o.core_node_name || o;
        return '<button class="os-chip btn ' + (cur === val ? 'on' : '') + '" data-act="' + act + '" data-' + attr + '="' + esc(val) + '">' +
            esc(o.label || val) + '</button>';
    }).join('');

    // THE MODE'S ROWS COME FROM THE MODEL, AND THEIR WORDS FROM FLOWSPEC.
    // This was four hardcoded branches beside composer-model.js's ROW_FIELDS,
    // which said the same thing and was read only by a test — so the rows a
    // mode draws were stated twice and the headings were a third set of names
    // for four claim columns.
    //
    // What stays here is which OPTIONS each field offers, which is a fact
    // about this screen: a press-index pair is chosen from the back
    // positions, a sequential partner from the other positions, and a staging
    // slot from the routing set plus the backs.
    const optionsFor = field => {
        if (field === 'paired_core_node') {
            return cc.mode === 'sequential'
                ? { list: model.positions.map(p => p.core_node_name).filter(n => n !== node), act: 'pair', cur: cc.paired }
                : { list: backs, act: 'pair', cur: cc.paired };
        }
        // THE THIRD POSITION, which arrived here the day the rows started
        // coming from flowspec: `second_paired_core_node` is Used on a press
        // index and the literal this replaced never listed it, so it would have
        // fallen through to the staging arm and written a park into a pair.
        // Its options are the back positions the pair does not already hold —
        // the desktop's `partnering` branch, same question, same answer.
        if (field === 'second_paired_core_node') {
            return {
                list: backs.filter(n => n !== cc.paired), act: 'pair2', cur: cc.secondPaired,
            };
        }
        if (field === 'outbound_staging') return { list: staging, act: 'parkold', cur: cc.parkOld };
        return { list: staging, act: 'stage', cur: cc.staging };
    };
    const modeRow = M().rowFields(model, node).map(field => {
        const o = optionsFor(field);
        const inner = chips(o.list, o.act, o.cur, 'val');
        // EVERY ROW ANSWERS ITS OWN EMPTY, paired_core_node included. This
        // asked routingRoleOf first and drew paired_core_node as a plain row,
        // because Amendment A wrote sentences only for the routing roles — so
        // on Hopkinsville's 4x2, where all five positions are kind `front` and
        // `backs` is therefore empty, picking "2-robot index" drew the heading
        // of the one REQUIRED field with nothing under it. The MODEL decides
        // which fields have a sentence and what it says; see routingNote.
        return routingRow(field, o.list, inner, node);
    }).join('');
    const via = M().viaWaypoints(model, node);
    const viaRow = cc.mode ? row(M().fieldLabel(model, 'key_route'),
        '<button class="os-chip btn ' + (!(cc.keyRoute || []).length ? 'on' : '') + '" data-act="via" data-val="">shortest way</button>' +
        via.map(w => '<button class="os-chip btn ' + ((cc.keyRoute || [])[0] === w ? 'on' : '') + '" data-act="via" data-val="' + esc(w) + '">' + esc(w) + '</button>').join('')) : '';

    showPop('[data-pos="' + node + '"]',
        '<h3>' + esc(node) + '<span>' + esc(rowWord(node)) + ' · ' +
        (cc.mode ? esc(labels[cc.mode]) : 'not in the flow yet') + '</span></h3>' +
        '<div><div class="os-lbl">How it swaps</div><div class="os-comp-seg">' + seg + '</div>' +
        '<div class="help">' + esc(cc.mode ? help[cc.mode] : 'Pick how the bin gets swapped at this position.') + '</div></div>' +
        // F3: every row is the claim's own field name. `Part on this position`
        // stays — `part` is the floor's word for payload_code and every surface
        // already uses it, from the PARTS strip to the finding pill.
        //
        // AND THE ROW IS DRAWN ONLY WHERE THE CHOREOGRAPHY HAS THE FIELD. A
        // mode flowspec marks payload_code forbidden has no part to pick, and
        // offering one there is a control whose value the validator refuses —
        // the operator finds out at the preview rather than at the tap. Same
        // rule as every mode-dependent row below it; see model.partAllowed.
        (M().partAllowed(model, node) ? row('Part on this position', partChips) : '') + modeRow +
        routingRow('inbound_source', srcs, chips(srcs, 'src', cc.source, 'val')) + viaRow +
        routingRow('outbound_destination', dests, chips(dests, 'dest', cc.dest, 'val')) +
        '<button class="os-comp-rm" data-act="remove">Remove ' + esc(node) + ' from the flow</button>');
}

// rowWord is which row of the PICTURE this position is drawn in — the same
// source the desktop's positions table reads (processes-desktop.js's rowWord,
// via pictureRows).
//
// NOT pos.kind, WHICH IS A DIFFERENT QUESTION AND A DIFFERENT ANSWER. Kind is
// "is this a partner slot for any style this process runs"; at Hopkinsville
// PLN_01 and PLN_04 are Kind `front` and are DRAWN in the back row. U9 fixed
// the desktop table and this panel kept the old read, so the station's panel
// captioned PLN_01 `front` under a picture that said BACK and beside a desktop
// table that said `back`. One word, one meaning, one source (F2).
//
// The station draws its picture in the default frame, so the row lookup uses
// it too; a middle row and a picture with only one row get no word, which is
// the renderer declining to guess rather than a gap.
//
// READ FROM THE LAYOUT ALREADY COMPUTED (P6), not computed again: see
// pictureRowOf. openPositionPanel draws the picture before it asks, so the
// answer is always the one on screen.
function rowWord(node) {
    return pictureRowOf[node] || '';
}

function row(label, inner) {
    return '<div class="os-comp-row-l"><span class="os-lbl">' + esc(label) + '</span>' + inner + '</div>';
}

// NEVER A HEADING OVER NOTHING (owner ruling, Amendment A 2026-09-16). A row
// whose options come from the routing set and has none draws its heading and
// an empty space, and an engineer at Hopkinsville read four of those as the
// derivation being broken. The sentence is the model's — the desktop's pickers
// say the same thing in the same words — and it names the next action.
//
// STILL NAMED FOR THE ROUTING ROLES, no longer only theirs. paired_core_node's
// options are the press's own positions and its empty has its own sentence
// (composer-model's PAIRED_NOTE, reached through the same routingNote), so it
// comes through here too: "never a heading over nothing" is one rule and this
// is the one place that keeps it. `node` is the position the panel is open on,
// which is what tells the model which choreography is asking; the dock panel
// has no position and passes none.
function routingRow(field, list, inner, node) {
    const note = M().routingNote(model, field, list.length, node);
    return row(M().fieldLabel(model, field),
        note ? '<span class="os-comp-none">' + esc(note) + '</span>' : inner);
}

function openDockPanel(half) {
    panelFor = { kind: 'dock', half: half };
    const opts = routing().filter(r => r.role === (half === 'in' ? 'source' : 'destination'));
    const act = model.positions.map(p => model.cells[p.core_node_name]).filter(c => c.on && c.mode);
    const cur = [...new Set(act.map(c => half === 'in' ? c.source : c.dest))];
    const dockNote = M().routingNote(model, half === 'in' ? 'inbound_source' : 'outbound_destination', opts.length);
    showPop('[data-dock="' + half + '"]',
        '<h3>' + (half === 'in' ? 'Inbound source' : 'Outbound destination') +
        '<span>applies to every position in this flow</span></h3>' +
        '<div class="os-comp-row-l">' + (dockNote
            ? '<span class="os-comp-none">' + esc(dockNote) + '</span>'
            : opts.map(o =>
                '<button class="os-chip btn ' + (cur.length === 1 && cur[0] === o.core_node_name ? 'on' : '') +
                '" data-act="dock" data-half="' + half + '" data-val="' + esc(o.core_node_name) + '">' +
                esc(o.label || o.core_node_name) + '</button>').join('')) + '</div>' +
        '<div class="help">Set one position differently from its own panel — it shows there, not here.</div>');
}

// PLACEMENT IS A CONSTRAINT, NOT A PREFERENCE (SPEC S5: never off-screen, never
// over the bar). placePop picks the side of the anchor that keeps the whole panel
// inside the picture and clear of the bar and the primary button; if no side fits
// vertically the panel scrolls INSIDE the picture rather than growing past its
// bottom edge, which is what put it over `Save and start`.
//
// Measured twice and again after the fonts land: the height at the moment the
// markup is set is not the height it renders at, and placing against the first
// one is how a too-tall panel ended up believing it fit.
function showPop(anchorSel, html) {
    const pop = $('os-comp-pop');
    const stage = $('os-comp-stage');
    if (!pop || !stage) return;
    pop.innerHTML = html;
    pop.hidden = false;
    const place = () => { placePop(pop, stage, anchorSel); reportPanelFit(pop); };
    // SCROLLING CHANGES THE FADE AND NOTHING ELSE, so only the fade is on this
    // listener. placePop alone fires on open and resize, and the fade has to go
    // when the operator reaches the last row. Passive, and re-registered with
    // the panel's contents because the node is rebuilt on every open.
    //
    // It used to call reportPanelFit, which is ~7 getBoundingClientRect and two
    // JSON.stringify — per scroll frame, on a touch screen, to publish
    // measurements only the shots harness reads, and --dump-dom never scrolls.
    // Round 3 removed data-composer-scroll for exactly this reason and left the
    // listener that carried it; see reportPanelFit.
    pop.addEventListener('scroll', () => updateScrollFade(pop), { passive: true });
    place();
    requestAnimationFrame(() => { requestAnimationFrame(place); });
    if (document.fonts && document.fonts.ready && document.fonts.ready.then) {
        document.fonts.ready.then(place);
    }
}

function placePop(pop, stage, anchorSel) {
    if (pop.hidden) return;
    const sb = stage.getBoundingClientRect();
    // The picture's own floor: the bar and the primary button are outside the
    // stage, so the stage's bottom IS the line the panel may not cross. Taking
    // the tighter of the two costs nothing and survives a bar that grows.
    const bar = document.querySelector('.os-comp-bar');
    const barTop = bar ? bar.getBoundingClientRect().top : Infinity;
    const floor = Math.min(sb.bottom, barTop) - 10;
    const ceil = sb.top + 10;
    pop.style.maxHeight = Math.max(160, floor - ceil) + 'px';

    const anchor = anchorSel ? stage.querySelector(anchorSel) : null;
    const w = pop.offsetWidth, h = pop.offsetHeight;
    let left = sb.width - w - 10, top = ceil - sb.top;
    if (anchor && anchor.getBoundingClientRect) {
        const r = anchor.getBoundingClientRect();
        // SIDE: whichever of left/right of the anchor holds the whole panel.
        // The reference shows this — PLN_03 sits on the right of the press and
        // its panel opens at the far left.
        const rightOf = r.right - sb.left + 12;
        const leftOf = r.left - sb.left - w - 12;
        left = rightOf + w <= sb.width - 10 ? rightOf
            : leftOf >= 10 ? leftOf
                : Math.max(10, Math.min(sb.width - w - 10, r.left - sb.left - w / 2));
        // VERTICAL: below the anchor, else above it, else pinned to the top and
        // scrolling — never past the floor.
        const below = r.bottom + 10;
        const above = r.top - 10 - h;
        top = (below + h <= floor ? below : above >= ceil ? above : ceil) - sb.top;
    }
    pop.style.left = Math.round(left) + 'px';
    pop.style.top = Math.round(Math.max(ceil - sb.top, top)) + 'px';
}

// updateScrollFade is P2 · the fade is on only while the panel can scroll
// further. It is the ONLY thing the scroll path needs, and it is three scalar
// reads on one element: no getBoundingClientRect, no JSON, no layout of
// anything else on the page.
//
// The 1 px tolerance is the usual fractional-scroll one: a panel scrolled to
// its end reports scrollTop + clientHeight a fraction short of scrollHeight on
// a fractional device pixel ratio, and a fade that never quite goes away at the
// bottom is the thing this guards against.
function updateScrollFade(pop) {
    if (!pop || pop.hidden) return;
    pop.classList.toggle('more-below', pop.scrollHeight - pop.scrollTop - pop.clientHeight > 1);
}

// SPEC S5, MEASURED AND PUBLISHED. The measurement goes into a data attribute on
// <body>, not into console.error.
//
// The console version was not an assertion at all. Headless Chrome's
// --enable-logging=stderr carries the BROWSER process's log, not the renderer's:
// a whole shots run yields ~300 bytes of stderr, all of it
// chrome_browser_cloud_management_controller.cc, and every console.error the page
// makes is dropped. So the guard "passed" on a shot that visibly covered the
// primary button — worse than no guard, because the report then says it fits.
//
// A data attribute survives into --dump-dom, which the shots harness reads and
// fails on.
//
// ON THE PLACE PATH ONLY — open, re-place, resize and fonts.ready (showPop's
// `place`). It is ~7 getBoundingClientRect and two JSON.stringify, which is
// fine four times per panel and was not fine on every scroll frame of a touch
// screen, which is where it also used to run. --dump-dom never scrolls, so the
// scroll-path copies reached nobody. The fade is the only thing scrolling
// changes and it moved to updateScrollFade above.
function reportPanelFit(pop) {
    const bar = document.querySelector('.os-comp-bar');
    const primary = document.querySelector('.os-comp-bar .os-btn-primary');
    const stage = $('os-comp-stage');
    if (!pop || !stage || pop.hidden) return;
    const p = pop.getBoundingClientRect(), s = stage.getBoundingClientRect();
    const hits = r => !!r && !(p.right <= r.left || p.left >= r.right ||
        p.bottom <= r.top || p.top >= r.bottom);
    document.body.dataset.composerFit = JSON.stringify({
        panel: [Math.round(p.left), Math.round(p.top), Math.round(p.right), Math.round(p.bottom)],
        overlapsBar: hits(bar && bar.getBoundingClientRect()),
        overlapsPrimary: hits(primary && primary.getBoundingClientRect()),
        outsidePicture: p.top < s.top - 1 || p.left < s.left - 1 ||
            p.right > s.right + 1 || p.bottom > s.bottom + 1,
    });
    updateScrollFade(pop);

    const seg = pop.querySelector('.os-comp-seg button');
    if (seg) {
        const sp = seg.querySelector('span'), g = seg.querySelector('svg');
        document.body.dataset.composerSeg = JSON.stringify({
            btnW: Math.round(seg.getBoundingClientRect().width),
            spanW: sp ? Math.round(sp.getBoundingClientRect().width) : -1,
            spanText: sp ? sp.textContent : null,
            glyphW: g ? Math.round(g.getBoundingClientRect().width) : -1,
        });
    }
}

// THE PANEL OPENS ITSELF ONCE, AFTER A SHAPE IS APPLIED (owner ruling 3,
// 2026-09-17: "it directs them to what's important").
//
// A preset carries a shape and no part, by construction — so the moment after
// an apply, every position in the new flow is asking the same question and the
// operator has no way of knowing which one to tap first. This opens the first
// one that has not been answered.
//
// ONCE, AND ONLY AFTER AN APPLY. It is not a mode: answering it closes it and
// nothing opens itself again, because a panel that reappears is a panel
// fighting the operator. `armed` is reset when the composer is built, which is
// once per style per session.
let firstUnansweredArmed = true;

function openFirstUnanswered() {
    if (!firstUnansweredArmed || !model) return;
    firstUnansweredArmed = false;
    const f = M().findings(model).find(x => x.node);
    if (f) openPositionPanel(f.node);
}

function closePop() {
    const pop = $('os-comp-pop');
    if (pop) { pop.hidden = true; pop.innerHTML = ''; }
    panelFor = null;
    if (model) { model = M().reduce(model, { type: 'select', node: null }); drawPicture(); }
}

// ── S9 · confirm ─────────────────────────────────────────────────────────────
// The rows and orders are the preview response, VERBATIM — not client-side prose.
async function openConfirm(runAsIs) {
    // A CONFIRM SHEET WITHOUT A PREVIEW IS A LIST OF NOTHING. The rows and the
    // orders are the preview response verbatim (SPEC S9), so if one is still in
    // flight — or was never run, which is how the hash entry arrives — wait for
    // it rather than draw an empty ORDERS column and claim inventory was checked.
    // The preflight is asked HERE and only here: this sheet is the one screen
    // that shows Core's answer about the parts.
    await runPreview({ preflight: true });
    const pv = model.preview;
    // F1: NOTHING TO START IS NOT SOMETHING TO CONFIRM, and there are four ways
    // to have nothing: no preview at all, a preview that refused, a plan that
    // fires no orders, and the RUNNING style.
    //
    // THE RUNNING STYLE WALKED STRAIGHT PAST THIS GUARD. Its preview is
    // validated and fingerprinted and never planned (engine.PreviewFlow's R3
    // branch), so it carries `running: true` and NO order_count — and
    // applyPreview stores that absence as null, which `orderCount === 0` does
    // not catch. The sheet then drew "Orders that fire now" over a changeover
    // the engine refuses outright (ErrStyleAlreadyRunning), two taps from the
    // set-up card, whose primary stays enabled on the running part.
    //
    // `running` rather than the missing count, because a preview that failed to
    // plan would also be missing one (engine.FlowPreview.Running says exactly
    // this). It goes to S4, where the bar's own running heading — "saved changes
    // take effect on the next trip" — says why nothing is starting.
    if (!pv || pv.error || pv.running || !pv.orderCount) { screen = 'S4'; drawComposer(); return; }
    screen = 'S9';
    const cells = M().toCells(model);
    const labels = M().modeLabels();
    const rows = cells.map(c =>
        '<tr><td>' + esc(c.core_node_name) + '</td><td>' + esc(labels[c.swap_mode] || c.swap_mode) +
        (c.paired_core_node ? ' · pair ' + esc(c.paired_core_node) : '') +
        (c.inbound_staging ? ' · stage ' + esc(c.inbound_staging) : '') + '</td>' +
        '<td>' + esc(M().shortPart(c.payload_code)) + '</td>' +
        '<td>' + esc(c.inbound_source) + ' → ' + esc(c.outbound_destination) + '</td></tr>').join('');
    // P1 · ONE SENTENCE PER ORDER, from the shared model. The robot's NAME
    // carries the robot's colour and the rest of the row does not: the hue is
    // an identity here, and a whole line in it reads as a warning.
    //
    // The trip comes from the model — including the rule that a row ends
    // after the destination when there is no part to name. This built it from
    // the fields and printed a trailing separator with nothing after it:
    // `PLN_03 → Supermarket Area ·`.
    //
    // The ROBOT is named here because it has its own cell: the hue is an
    // identity in this table and a whole line in it reads as a warning.
    const orders = M().orderSentences(model).map(o =>
        '<tr><td class="r' + o.robot + '">Robot ' + o.robot + '</td>' +
        '<td>' + esc(M().orderTrip(o)) + '</td></tr>').join('');
    // Appended over the picture rather than replacing the root: SPEC S9 is a
    // bottom sheet over S4, and the operator checking rows against the flow they
    // just drew should be able to see it behind the sheet.
    const host = document.createElement('div');
    host.className = 'os-comp-layer';
    host.innerHTML =
        '<div class="os-comp-scrim on"></div>' +
        '<div class="os-comp-confirm on"><h2>' +
        (runAsIs ? 'Start the changeover to ' + esc(model.styleName) + '?'
            : 'Save this flow to ' + esc(model.styleName) + ' and start the changeover?') + '</h2>' +
        '<div class="cols"><div><div class="os-lbl">Rows written to the part</div>' +
        '<table><tr><th>Position</th><th>Swap</th><th>Part</th><th>From → to</th></tr>' + rows +
        '<tr class="muted"><td colspan="4">— back positions, no row, paired</td></tr></table></div>' +
        '<div><div class="os-lbl">Orders that fire now</div>' +
        '<table><tr><th>Robot</th><th>From → to</th></tr>' + orders + '</table></div></div>' +
        // THE WORD IS CELL (owner, 2026-09-17). An operator reading this is
        // standing at whatever this process is — a 4x2 weld cell as often as a
        // press — and "the robots wait at the press" is wrong on the floor it
        // is most often read on. The second "press" is the verb and stays.
        '<div class="foot">Robots wait at the cell until you press Release.' +
        // Only the two things the preflight can actually say. "Inventory
        // checked." with no preflight at all was the sheet asserting a check
        // nobody ran.
        (!pv.preflight ? ''
            : pv.preflight.state === 'unchecked' ? ' Inventory not checked — Core is unreachable.'
                : ' Inventory checked.') + '</div>' +
        '<div class="acts"><button class="os-btn os-btn-quiet os-btn-lg" data-act="back">Back</button>' +
        '<button class="os-btn os-btn-primary os-btn-lg" data-act="start" data-runasis="' + (runAsIs ? '1' : '') + '">Start changeover</button></div></div>';
    const r = show();
    if (!r.querySelector('.os-comp-full')) drawComposer();
    const old = r.querySelector('.os-comp-layer');
    if (old) old.remove();
    r.appendChild(host);
}

// ── start ────────────────────────────────────────────────────────────────────
async function start(runAsIs) {
    const pv = model.preview || {};
    let fingerprint = pv.fingerprint || '';
    if (!runAsIs) {
        // R8: the save happens ONLY here. "Run as is" changed nothing, so there
        // is nothing to write — and with the gate off a save is a 403.
        const res = await fetch('/api/processes/' + processID() + '/flow/save', {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
                to_style_id: model.styleId, cells: M().toCells(model),
                fingerprint: fingerprint,
                station_id: stationID(),
            }),
        });
        const json = await res.json().catch(() => ({}));
        if (res.status === 409) { onStale(json); return; }
        if (res.status === 422) { model = M().applyPreview(model, json); screen = 'S4'; drawComposer(); return; }
        // A REFUSED SAVE IS NOT A STALE FLOW, and until R3 there was no way to
        // tell them apart because only one of them was reachable. Every non-OK
        // status went to onStale, which throws the held payload away and heads
        // the bar "The flow changed since you previewed" — advice that is wrong
        // about a 403 and sends the operator to re-check a flow that is fine.
        //
        // R3 makes the 403 reachable for the first time (a gated-off cell
        // refuses anything but a first flow from a preset), so the refusal is
        // shown BY NAME, on the sheet the operator is standing on, in the same
        // shape a refused START already uses.
        if (!res.ok) { showStartRefusal(json, res.status, false); return; }
        fingerprint = json.fingerprint || fingerprint;
        savedFingerprint = fingerprint;
    }
    const res = await fetch('/api/processes/' + processID() + '/changeover/start', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
            to_style_id: model.styleId, called_by: (view && view.station && view.station.name) || '',
            flow_fingerprint: fingerprint,
        }),
    });
    const json = await res.json().catch(() => ({}));
    if (res.status === 409) { onStale(json); return; }
    // ANYTHING THAT IS NOT A 409 IS NOT A STARTED CHANGEOVER.
    //
    // apiStartProcessChangeover answers exactly ONE refusal with 409 — the
    // stale fingerprint it checks itself — and EVERY engine refusal with 400:
    // ErrStyleAlreadyRunning, ErrChangeoverActive, a plan that would not build.
    // This tested `status === 409` and called the whole rest of the response
    // space success, so a REFUSED changeover drew the green tick, said
    // "Changeover started", and took the operator back to the board in four
    // seconds. The robots never moved and the operator walked away believing
    // they had; the next thing to notice would have been a line running dry.
    //
    // res.ok is the success test, and the refusal is shown BY NAME on the sheet
    // the operator is standing on — same shape as desktop-bodies' saveOutcome:
    // the server's own sentence, or the status when it sent none.
    if (!res.ok) { showStartRefusal(json, res.status, !runAsIs); return; }
    openStarted(runAsIs);
}

// showStartRefusal puts the server's sentence on the confirm sheet, above the
// buttons that asked for it.
//
// NOT onStale(). That one is for the stale fingerprint: it throws the held flow
// payload away, drops to S4 and heads the bar "The flow changed since you
// previewed — check it and try again", which over "process is already running
// style 11" sends the operator to re-check a flow that is fine and says nothing
// about the press already running the part they picked. A refusal they cannot
// act on from here belongs where they are looking, beside Back.
//
// THE SAVE HAS ALREADY LANDED when the flow was edited: start() writes first and
// starts second, so a refusal after a successful save leaves a saved flow and no
// changeover. The sentence says so, because the alternative is an operator
// saving it again to be sure.
function showStartRefusal(json, status, saved) {
    const sheet = root().querySelector('.os-comp-confirm');
    // No sheet means the hash entry rather than a tap — nothing on screen to
    // write on, so the bar takes it, which is where every other refusal lands.
    if (!sheet) { onStale(json); return; }
    let el = sheet.querySelector('.os-comp-refusal');
    if (!el) {
        el = document.createElement('div');
        el.className = 'os-comp-refusal';
        sheet.insertBefore(el, sheet.querySelector('.acts'));
    }
    // textContent, so the server's sentence needs no escaping to be safe here.
    el.textContent = (saved ? 'The flow was saved. ' : '') +
        ((json && json.error) || ('The changeover was refused (' + status + ')'));
}

// ── S10 · started ────────────────────────────────────────────────────────────
// 4 s countdown, no button, returns to the board by itself (SPEC §0.10).
async function openStarted(runAsIs, hold) {
    screen = 'S10';
    // The sentence names the orders that just fired, so it needs the preview
    // that planned them. Reached from `start()` there always is one; reached
    // from the hash entry (the shot) there is not, and the screen used to read
    // "0 orders" over a changeover that had six.
    if (!model.preview) await runPreview();
    const pv = model.preview || {};
    // THE SAME WORDS THE SET-UP CARD USED two screens ago. This said
    // "2 robots" and the card said "Robot 1 and Robot 2" — one fact, two
    // spellings, and SPEC §0.7 has only one of them.
    const robots = M().robotWords(model);
    // NO COUNT IS NOT A COUNT. applyPreview stores orderCount NULL when the
    // server sent none — the running style's preview is never planned, so
    // `order_count` is absent from its body — and this line concatenated it:
    // `Running the saved flow · null orders · Robot 1 and Robot 2.` under a
    // green tick. When there is no count the clause is left out; the rest of
    // the sentence is still true.
    //
    // And the count agrees in number. `1 orders` is the same class of defect on
    // the same line, and the set-up card's provenance line already spells it the
    // other way (setupProvenance).
    const orders = typeof pv.orderCount === 'number'
        ? ' · ' + pv.orderCount + ' order' + (pv.orderCount === 1 ? '' : 's') : '';
    const line = runAsIs
        ? 'Running the saved flow' + orders + ' · ' + robots + '.'
        : 'Flow saved to ' + model.styleName + orders + ' · ' + robots +
        '. Next time this part is picked it runs as is.';
    show().innerHTML =
        '<div class="os-comp-started on"><div class="ring">✓</div>' +
        '<h2>Changeover started</h2><p>' + esc(line) + '</p>' +
        '<span class="os-lbl">Saved by ' + esc((view && view.station && view.station.name) || '') + ' · source HMI</span>' +
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
    const tap = e.target.closest && e.target.closest('[data-tap]');
    const btn = e.target.closest && e.target.closest('[data-act]');
    if (!btn && tap) {
        e.stopPropagation();
        // A STAGING CARD CARRIES THE POSITION IT SERVES, and a tap on it opens
        // that position's panel — the lane itself has no settings, and the row
        // that chose it is on the panel of the position whose swap parks there.
        if (tap.dataset.tap === 'pos' || tap.dataset.tap === 'staging') openPositionPanel(tap.dataset.pos);
        else if (tap.dataset.tap === 'dock') openDockPanel(tap.dataset.dock);
        return;
    }
    if (!btn) {
        if (panelFor && !(e.target.closest && e.target.closest('.os-comp-pop'))) closePop();
        return;
    }
    e.stopPropagation();
    const act = btn.dataset.act;
    const node = panelFor && panelFor.node;
    // ONE DRAW PER TAP. Every action that re-opens the position panel used to
    // draw the picture twice — once here and once inside openPositionPanel —
    // so a mode chip on a touch screen rebuilt the whole SVG before replacing
    // it. sendQuiet is for the actions that re-open the panel; send is for the
    // ones that do not.
    const sendQuiet = a => { model = M().reduce(model, a); drawStrip(); drawBar(); schedulePreview(); };
    const send = a => { sendQuiet(a); drawPicture(); };
    switch (act) {
        case 'close': close(); break;
        case 'pick': {
            const id = +btn.dataset.style;
            const s = styleByID(id);
            if (!s) break;
            // THE ONE FETCH (S8). The picker drew from the view; everything
            // past it needs the stored cells, the routing set and the scene,
            // so this is where they arrive — once per session, not once per
            // poll. The tap is already a screen change, so the wait is where
            // a wait is expected; the SCAN, which must never wait, has
            // already done its filtering by here.
            ensureFlow().then(() => openSetup(id));
            break;
        }
        case 'topick': openPicker(); break;
        case 'runasis': runAsIs(); break;
        // Still on the picture's strip and the position panel; it left the
        // set-up card, where the position a part lands on is not visible.
        // From the STRIP the part arrives loose (data-node is empty); from a
        // POSITION PANEL it lands on that position, because the operator
        // opened the picker from the thing they want it on.
        case 'addpart': openPartPicker(btn.dataset.node || ''); break;
        case 'partcancel': closePartPicker(); break;
        case 'partpick': {
            const code = btn.dataset.part;
            const on = partPickerNode;
            closePartPicker();
            if (!code) break;
            // TWO ACTIONS, NOT ONE, AND BOTH ARE EXISTING ONES. addPart is the
            // seam — it puts the payload on THIS STYLE's part list — and setPart
            // is what a tap on a chip already does. A picker opened from a
            // position sends both, because an operator who opened it from
            // PLN_03 asked for the part to be on PLN_03; one opened from the
            // strip sends only the first, and the part shows as an amber
            // `· unplaced` chip until it is dropped somewhere.
            sendQuiet({ type: 'addPart', payloadCode: code });
            if (on) {
                sendQuiet({ type: 'setPart', node: on, payloadCode: code });
                openPositionPanel(on);
            } else {
                drawPicture();
            }
            break;
        }
        case 'compose': openComposer(model.styleId, false); break;
        case 'blank': send({ type: 'startBlank' }); break;
        case 'preset': {
            const p = (flow().presets || []).find(x => x.id === btn.dataset.preset);
            if (p) { send({ type: 'applyPreset', preset: p }); openFirstUnanswered(); }
            break;
        }
        // From the no-flow card: apply the shape, then open the composer on it.
        // The card is the only screen the operator has seen, so the shape has
        // to arrive somewhere they can look at it before anything is saved.
        case 'preset-apply': {
            const p = (flow().presets || []).find(x => x.id === btn.dataset.preset);
            if (!p) break;
            openComposer(model ? model.styleId : 0, false);
            send({ type: 'applyPreset', preset: p });
            openFirstUnanswered();
            break;
        }
        case 'compose-blank': openComposer(model ? model.styleId : 0, true); break;
        case 'mode': sendQuiet({ type: 'setMode', node: node, mode: btn.dataset.mode }); openPositionPanel(node); break;
        case 'part': sendQuiet({ type: 'setPart', node: node, payloadCode: btn.dataset.part }); openPositionPanel(node); break;
        case 'pair': sendQuiet({ type: 'setPartner', node: node, partner: btn.dataset.val }); openPositionPanel(node); break;
        case 'pair2': sendQuiet({ type: 'setSecondPartner', node: node, partner: btn.dataset.val }); openPositionPanel(node); break;
        case 'stage': sendQuiet({ type: 'setStaging', node: node, staging: btn.dataset.val }); openPositionPanel(node); break;
        case 'parkold': sendQuiet({ type: 'setParkOld', node: node, staging: btn.dataset.val }); openPositionPanel(node); break;
        case 'src': sendQuiet({ type: 'setSource', node: node, source: btn.dataset.val }); openPositionPanel(node); break;
        case 'dest': sendQuiet({ type: 'setDest', node: node, dest: btn.dataset.val }); openPositionPanel(node); break;
        case 'via': sendQuiet({ type: 'setVia', node: node, via: btn.dataset.val }); openPositionPanel(node); break;
        case 'remove': send({ type: 'removePosition', node: node }); closePop(); break;
        case 'dock':
            send(btn.dataset.half === 'in'
                ? { type: 'setDockSource', source: btn.dataset.val }
                : { type: 'setDockDest', dest: btn.dataset.val });
            closePop();
            break;
        case 'fix': {
            const b = M().bar(model);
            if (b.fixIt) send(b.fixIt.action);
            break;
        }
        case 'confirm': openConfirm(false); break;
        case 'back': {
            const layer = root().querySelector('.os-comp-layer');
            if (layer) layer.remove();
            screen = 'S4';
            drawBar();
            break;
        }
        case 'start': start(!!btn.dataset.runasis); break;
        default: break;
    }
}

// R8: preview the style's OWN cells, then start on that preview's fingerprint —
// it is over the stored rows, which is exactly what start checks. No save.
async function runAsIs() {
    await runPreview();
    openConfirm(true);
}

// ── hash entry (shots) ───────────────────────────────────────────────────────
export function openFromHash() {
    const h = (window.location && window.location.hash) || '';
    const m = /#compose=(\d+)(?:;state=(S\d+))?(?:;node=([A-Za-z0-9_]+))?/.exec(h);
    if (!m) return false;
    const id = +m[1], want = m[2] || 'S4', node = m[3] || null;
    // ;hold=1 stops S10's 4-second countdown so a screenshot can catch it. Local
    // UI state, never a write, and never reachable from a tap — the only caller
    // is scripts/composer-shots.sh, which cannot photograph a screen that leaves
    // by itself well before the virtual-time budget fires.
    const hold = /;hold=1/.test(h);
    const s = styleFor(id);
    if (!s) return false;
    if (want === 'S2') { openPicker(); return true; }
    if (want === 'S3') { openSetup(id); return true; }
    buildModel(s);
    if (want === 'S7') model = M().reduce(model, { type: 'startBlank' });
    openComposer(id, want === 'S7');
    if (node) openPositionPanel(node);
    else if (want === 'S6') openDockPanel('in');
    if (want === 'S9') openConfirm(false);
    if (want === 'S10') openStarted(false, hold);
    // ;addpart=1 opens the part picker over whatever state was asked for — a
    // SHEET rather than a state of its own, so it composes with `node` the way
    // a tap does: with one it opens on that position, without one it opens on
    // the flow. Local UI state, never a write, like every other hash modifier
    // here.
    if (/;addpart=1/.test(h)) openPartPicker(node || '');
    return true;
}

export function setView(v) {
    view = v;
    // The summaries are built from the view, so a new one invalidates them.
    pickerIndex = null;
    if (screen === 'S2') drawPickerRows(($('os-comp-q') || {}).value || '');
}

// A page that throws renders the board underneath and photographs as a
// plausible shot. The shots harness reads this attribute and fails the run.
window.addEventListener('error', e => {
    try {
        document.body.dataset.composerError =
            String((e && (e.message || e.error)) || 'unknown').slice(0, 300);
    } catch (_) { /* nothing left to report with */ }
});

window.ComposerUI = { openPicker, close, setView, openFromHash };

// THE HASH ENTRY FETCHES ITS OWN VIEW. On the live path the board hands us one
// (the CHANGEOVER branch passes getView()), but a shot loads the page cold at a
// hash and nothing has run yet — so this reads the same endpoint the board
// reads, once, and opens. Still no write: every state below S9 is local UI, and
// S9/S10 render from a preview rather than starting anything.
//
// It also means scripts/composer-shots.sh needs no DOM driving: Chrome opens a
// URL and the page arrives in the state the filename claims.
function bootFromHash() {
    if (!/#compose=/.test((window.location && window.location.hash) || '')) return;
    const m = /\/operator\/station\/(\d+)/.exec(window.location.pathname || '');
    if (!m) return;
    fetch('/api/operator-stations/' + m[1] + '/view')
        .then(r => r.json())
        // THE CELL PICTURE TOO. The view carries a version and no picture, and
        // on the live path operator.js has already fetched one and attached it
        // before the composer is ever opened. A shot loads cold at a hash, so
        // nothing has — and without this every composer state photographs an
        // empty stage.
        .then(v => {
            view = v;
            return fetch('/api/operator-stations/' + m[1] + '/cell')
                .then(r => (r.ok ? r.json() : null))
                .then(pic => { if (pic) view.cell = pic; })
                .catch(() => { /* the schematic caption says there is nothing to draw */ });
        })
        // The composer's own read too: a shot opens straight into S3 or later,
        // which the live path only reaches through a tap that has already
        // fetched it.
        .then(() => ensureFlow())
        .then(() => { openFromHash(); })
        .catch(() => { /* no view, no composer — the board still renders */ });
}

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', bootFromHash);
else bootFromHash();
