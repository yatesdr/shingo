// composer-model.js — the Flow Composer's pure model. No DOM, no fetch, no timers.
//
// Everything the operator does to a flow happens here: reduce(state, action)
// returns a new state, and the derived views (legs, glyphs, dockNotes, cardLines,
// bar, findings) read that state and nothing else. composer-render.js draws what
// these return; it decides nothing.
//
// WHY A PURE MODEL AND NOT A RENDERER WITH STATE IN IT. The reference file
// (REFERENCE-press400-flow-composer-HK-2026-09-03.html) mutates a `state` global
// from inside its click handlers and re-renders. That is fine for a design
// artefact and wrong here for one reason: toCells() is the wire, and the wire is
// what U7's endpoints validate. Keeping the transform pure is what lets
// composer-model.test.js assert the exact FlowCell JSON against bytes generated
// by Go's own domain.Collapse — the round-trip pin that makes a shape change on
// the server fail the JS on the same day.
//
// THE FLOWSPEC IS INJECTED, NOT FETCHED. init() takes it. There is exactly one
// flowspec on this tree — flowspec-data.js beside this file, held to
// flowspec.ExportJSON() by TestStationFlowspecFileMatchesGo — and
// this file reads whichever copy its caller hands it rather than adding a second
// delivery mechanism (the brief forbids a new endpoint, §5).

'use strict';

// ── vocabulary ───────────────────────────────────────────────────────────────
// "Robot 1" / "Robot 2", never the short forms. SPEC §0.7.
const MODES = {
    two_robot_press_index: '2‑robot index',
    two_robot: '2‑robot swap',
    single_robot: '1‑robot swap',
    sequential: 'Sequential A/B',
};

// SHAPE_FIELDS — what a flow's SHAPE is made of (U10).
//
// NO WORDS HERE AT ALL, which is the point. This names the FIELDS; what each
// one is CALLED comes from the generated flowspec block (fieldWord below),
// which is domain/flowspec's fieldLabels — the same table a server refusal
// words itself from.
//
// Owner ruling F3 (2026-09-12) said a field is called what the claim calls it,
// because D1's headings were invented: `old bins to` for
// `outbound_destination`, `new bins from` for `inbound_source`. The first fix
// wrote better words HERE, which left three tables — this one, Go's
// shapeFields and flowspec's — and "paired position" on one screen was "Paired
// Core Node" in the refusal about it. One table, read by all three.
//
// THE PART IS NOT IN IT, and neither is Advanced. A preset is a shape, never a
// part (SYNTH R-L1), and a reorder point is a policy on a position rather than
// a shape — a preset carrying one would apply a policy along with a shape
// without saying so.
//
// domain.shapeFields names the same eleven fields in the same order and is
// held to this list by TestShapeFieldsMatchTheModel. The server words a
// drifted member's field list and this file words an apply's diff; both look
// the word up, so neither can spell it differently.
const SHAPE_FIELDS = [
    { field: 'swap_mode', of: c => c.mode || '' },
    { field: 'role', of: c => c.role || '' },
    { field: 'paired_core_node', of: c => c.paired || '' },
    { field: 'second_paired_core_node', of: c => c.secondPaired || '' },
    { field: 'inbound_staging', of: c => c.staging || '' },
    { field: 'outbound_staging', of: c => c.parkOld || '' },
    { field: 'inbound_source', of: c => c.source || '' },
    { field: 'outbound_destination', of: c => c.dest || '' },
    { field: 'changeover_evac_destination', of: c => c.evacDest || '' },
    { field: 'changeover_evac_nodes', of: c => (c.evacNodes || []).join(',') },
    { field: 'key_route', of: c => (c.keyRoute || []).join(' › ') },
];

// fieldWord is what a field is CALLED on screen, from the generated flowspec
// block — the same table domain/flowspec.Label reads, so the word in a preset
// diff, the word on a position panel row and the word in a server refusal are
// one word.
//
// The field's own name is the fallback, which is what a surface should show if
// the block is ever missing a label: `outbound_destination` is ugly and
// correct, and correct is the requirement.
function fieldWord(state, field) {
    const labels = (state && state.flowspec && state.flowspec.labels) || {};
    return labels[field] || field;
}

// CARDLINE — the two 12 px lines on a position card, ported from the reference.
//
// NO BIN WORD (owner ruling R2, 2026-09-12). The index line read `Robot 1
// brings <bin word> to PLN_05`, resolved from the style's payload; it
// says `Robot 1 supplies PLN_05`. The verb is what the operator needs — which
// robot does the far trip — and the thing it carries is drawn on the card
// below it, named by its part.
// A CARD SAYS WHERE, NOT JUST WHO (owner, 2026-09-16). single_robot named
// neither of its two parking spots — "One robot, parks and swaps" is the
// choreography, which the Swaps chip beside it already carries — so the one
// mode with BOTH an inbound and an outbound staging spot was the one mode that
// named neither. It names them in the words the desktop's staging cell already
// uses, `Inbound PLN_02 · Outbound —`, and falls back to the reference
// sentence when the flow has not set either yet.
//
// ONLY THE FIELDS THE MODE HAS, which is the correction this went through.
// The first pass appended the outbound staging to two_robot and to
// two_robot_press_index as well, and ROW_FIELDS above says plainly that
// neither has it: a press index has `paired_core_node` and nothing else, and
// two_robot has `inbound_staging` alone. Printing `outbound_staging` there
// would have named a place off a stale column that the choreography never
// reads — telling an operator a bin goes somewhere it does not.
//
// TWO LINES IS THE CEILING, and it is a hard one: the lines sit at y=43 and
// y=57 and the part chip is a rect from y=64, so a third would be drawn
// through it. Each ' · ' is a line break, which is why a mode gets two
// segments and never three. At 12 px in a 188 px card that is about 27
// characters a line.
const CARDLINE = {
    two_robot_press_index: c => 'Robot 1 supplies ' + (c.paired || '?') + ' · Robot 2 indexes',
    two_robot: c => 'Robot 1 stages at ' + (c.staging || '?') + ' · Robot 2 pulls old',
    single_robot: c => ((c.staging || c.parkOld)
        ? 'Inbound ' + (c.staging || '—') + ' · Outbound ' + (c.parkOld || '—')
        : 'One robot, parks and swaps'),
    sequential: c => 'One robot, A/B flip' + (c.paired ? ' · with ' + c.paired : ''),
};

const MODEHELP = {
    two_robot_press_index: 'Two robots: Robot 1 brings the new bin to the back position; Robot 2 takes the old one out and indexes the new one forward.',
    two_robot: 'Two robots: Robot 1 stages the new bin beside the line while Robot 2 pulls the old bin; then Robot 1 moves the new one in.',
    single_robot: 'One robot, one trip: park the new bin, clear the old one to a second park, put the new one in, come back for the old one.',
    sequential: 'A/B positions, one robot: fills the parked side while the other side runs, then you flip.',
};

// The four short texts of SPEC S8, keyed by the claim field the server names.
// The short pill a card carries when a field is missing. F3 reaches these
// too: `nowhere to send old bin` was a sentence about `outbound_destination`
// that never said which field it meant, and a pill an operator cannot map to a
// control is a pill they cannot act on.
const FINDING_SHORT = {
    swap_mode: 'how does it swap?',
    payload_code: 'which part?',
    inbound_staging: 'no inbound staging',
    outbound_destination: 'no outbound destination',
    unplaced_part: 'needs a position',
};

// ROW_FIELDS — which claim field each S5 row writes, per mode (§3.3), in the
// order the rows are drawn.
//
// FIELDS, NO WORDS. Its keys used to be the row headings — `STAGE THE NEW BIN
// AT`, `PARK OLD AT`, `A/B PARTNER`, `PAIRED BACK POSITION` — five names for
// four claim columns, which is exactly what owner ruling F3 took off the
// desktop. And nothing read them: the panel had its own hardcoded list of
// rows, so the two said the same thing twice and only a test read this one.
//
// The panel builds its rows from this now, and the word for each comes from
// the flowspec labels like every other word on the screen.
const ROW_FIELDS = {
    two_robot_press_index: ['paired_core_node'],
    two_robot: ['inbound_staging'],
    single_robot: ['inbound_staging', 'outbound_staging'],
    sequential: ['paired_core_node'],
};

// ROW_COLUMNS — the two mode-dependent columns the desktop's positions table
// draws, and the fields each one owns. ONE ROW OF FLOWSPEC DECIDES BOTH.
//
// The table used to have a single `Partner` column that took whichever field
// the mode happened to have: `Paired` on a press index, `Staged at` on a
// two-robot swap, `A/B` on a sequential, and a dash on single_robot. That
// conflated two different questions — which position this one is PAIRED with,
// and where a bin is PARKED — and it left five fields with no editor at all,
// because a column that can only hold one thing can only show one. The owner's
// words: "other complex orders have staging spots too."
//
// So: Partner is the paired positions, Staging is the parking spots, and each
// chip is drawn exactly when the flowspec row for this (role, mode) says the
// field is Used or Required — absent when it is Forbidden or Unused. That is
// the same table the HMI's S5 mode-dependent row reads through ROW_FIELDS, so
// the two surfaces cannot disagree about what a mode has.
const ROW_COLUMNS = {
    // NO PER-MODE WORD FOR A FIELD THAT HAS A NAME (owner ruling F3). A
    // sequential's paired position read `A/B` and a press index's read
    // `Paired`, which is two names for `paired_core_node` — and `A/B` is the
    // choreography's name, which the Swaps chip beside it already carries.
    partner: [
        { key: 'paired', field: 'paired_core_node', label: {} },
        { key: 'secondPaired', field: 'second_paired_core_node', label: {} },
    ],
    staging: [
        { key: 'staging', field: 'inbound_staging', label: {} },
        { key: 'parkOld', field: 'outbound_staging', label: {} },
    ],
};

// THE CHIP'S WORD IS THE CLAIM'S FIELD NAME (owner ruling F3, 2026-09-12).
// `Stage new at` / `Park old at` were `inbound_staging` / `outbound_staging`
// under invented names, and `Third` was `second_paired_core_node` described by
// where it sits rather than called what it is. Inside the Staging cell the
// column heading already says Staging, so the chips carry the direction alone
// — `Inbound PLN_02 · Outbound —`.
// The composer's cell fields, and the FlowCell key each one writes. Anything
// not in this map is carried through by Expand from the prior claim and is not
// the composer's to show (brief R1b).
const CELL_TO_FIELD = {
    part: 'payload_code',
    paired: 'paired_core_node',
    secondPaired: 'second_paired_core_node',
    staging: 'inbound_staging',
    parkOld: 'outbound_staging',
    source: 'inbound_source',
    dest: 'outbound_destination',
    evacDest: 'changeover_evac_destination',
    evacNodes: 'changeover_evac_nodes',
    keyRoute: 'key_route',
};

// ADVANCED — the columns the picture has no line for (U9b, spec §2 D2).
//
// THE KEYS ARE THE WIRE'S KEYS, not a camelCase spelling of them. A cell field
// above has two names because the model had one before the wire did; this
// block is new on both sides at once, so giving it one name means the object
// the modal edits IS the object toCells sends and the view's per-position
// block reads back — no map to get wrong in either direction.
//
// The values here are the INSERT defaults, and "is anything set" is measured
// against them by advancedSet below.
const ADVANCED_DEFAULTS = {
    allowed_payload_codes: [],
    reorder_point: 0,
    reorder_point_source: 'legacy',
    auto_reorder: false,
    lineside_soft_threshold: 0,
    auto_request_payload: '',
    auto_push: false,
    evacuate_on_changeover: false,
    changeover_carryover_disposition: 'replace',
    index_robot_supplies: false,
    auto_confirm: false,
    // The loader board's draw order. Zero is "the store's order", not
    // position zero — Expand speaks the column only when this block exists at
    // all, so a save that never opened the sheet leaves it alone.
    sequence: 0,
};

// Which flowspec field decides whether each Advanced control is drawn.
// reorder_point_source has no flowspec row of its own — it is a stamp on how
// the number was set, so it follows the number.
// ── helpers ──────────────────────────────────────────────────────────────────
const clone = v => JSON.parse(JSON.stringify(v));
const blankCell = () => ({
    on: false, mode: null, part: null, source: '', dest: '',
    staging: '', parkOld: '', paired: '', secondPaired: '',
    keyRoute: [], evacNodes: [], evacDest: '', role: '',
    // null until an engineer opens the Advanced modal and presses Apply. It is
    // the same nil FlowCell.Advanced carries: untouched means Expand's
    // carry-through keeps the stored row's policy, and there is no way to say
    // that with a value.
    advanced: null,
});

function backPositions(state) {
    return state.positions.filter(p => p.kind === 'back')
        .slice().sort((a, b) => (a.sequence || 0) - (b.sequence || 0))
        .map(p => p.core_node_name);
}

// A back position is "taken" when some active cell names it as a partner or a
// park. The first free one in sequence order is what a fresh index position gets.
function freeBackPosition(state, exclude) {
    const taken = new Set();
    for (const n of Object.keys(state.cells)) {
        const c = state.cells[n];
        if (!c.on || n === exclude) continue;
        if (c.paired) taken.add(c.paired);
        if (c.staging) taken.add(c.staging);
        if (c.parkOld) taken.add(c.parkOld);
    }
    return backPositions(state).find(n => !taken.has(n)) || '';
}

// ROLE IS DERIVED, NEVER ASKED (brief R2). Four sources, in order: the prior
// claim on this very node; the majority role of THIS style's other claims; the
// majority role of the PROCESS's other styles' claims; then consume.
//
// The third source is the one that matters at Springfield (owner ruling
// 2026-09-10). A style with no claims at all — the "Build" row — used to fall
// straight to consume, and on SPR that is 635 of the 713 (style x position)
// pairs the composer can open. It happened to be right there, because 33 of
// SPR's 41 claims are consume, but it was right by coincidence: the same
// default on a press cell would have said consume about a produce position. The
// press already knows what it does, from every other style that runs on it.
//
// Consume survives as the floor for a process with no claims anywhere — a cell
// nobody has ever configured — where there is nothing to read and the server's
// validator answers before anything moves.
function deriveRole(claims, processClaims, node) {
    const own = (claims || []).find(c => c.core_node_name === node);
    if (own && own.role) return { role: own.role, source: 'prior claim' };
    const majority = rows => {
        const tally = {};
        for (const c of rows || []) if (c.role) tally[c.role] = (tally[c.role] || 0) + 1;
        return Object.keys(tally).sort((a, b) => tally[b] - tally[a])[0] || '';
    };
    const mine = majority(claims);
    if (mine) return { role: mine, source: 'majority of sibling claims' };
    const theirs = majority(processClaims);
    if (theirs) return { role: theirs, source: "majority of the process's other styles" };
    return { role: 'consume', source: 'default' };
}

function steadyRow(state, role, mode) {
    const fs = state.flowspec;
    if (!fs || !fs.steady || !fs.steady[role]) return null;
    return fs.steady[role][mode] || null;
}

// Clear every composer-authored field the flowspec marks forbidden for this
// (role, mode). Called on every mode change so a value from the old
// choreography cannot survive into one that refuses it.
function clearForbidden(state, cell) {
    const row = steadyRow(state, cell.role, cell.mode);
    if (!row) return cell;
    for (const key of Object.keys(CELL_TO_FIELD)) {
        const field = CELL_TO_FIELD[key];
        if (row[field] !== 'forbidden') continue;
        cell[key] = Array.isArray(cell[key]) ? [] : (key === 'part' ? null : '');
    }
    if (cell.advanced) clearForbiddenAdvanced(state, cell, cell.advanced);
    return cell;
}

// flowspecFieldFor is the flowspec row's column for one Advanced key.
//
// TWELVE OF THIRTEEN ARE THE SAME NAME. This was a thirteen-entry lookup table
// in which every entry but one mapped a key to itself — a table whose content
// was "these names are the same", maintained by hand, one line per field, so
// adding a field meant adding a row that said nothing. The one real mapping is
// reorder_point_source, which has no flowspec row of its own: it is HOW the
// reorder point was chosen, so the point's row governs it.
function flowspecFieldFor(key) {
    return key === 'reorder_point_source' ? 'reorder_point' : key;
}

// The same rule over the Advanced block, and only for Forbidden.
//
// UNUSED IS NOT CLEARED, deliberately. flowspec draws the line for us:
// Forbidden is a value the validator refuses, so a mode change that leaves one
// behind is a 422 the operator did not ask for; Unused is a value this
// choreography does not read, and clearing a column at save because a screen
// stopped drawing it is a data change made by a UI decision (flowspec.go's own
// words about reuse_compatible_bins). The modal hides both; only one is wiped.
function clearForbiddenAdvanced(state, cell, adv) {
    const row = steadyRow(state, cell.role, cell.mode);
    if (!row) return adv;
    for (const key of Object.keys(ADVANCED_DEFAULTS)) {
        if (row[flowspecFieldFor(key)] !== 'forbidden') continue;
        adv[key] = clone(ADVANCED_DEFAULTS[key]);
    }
    return adv;
}

// advancedFor is what the modal opens on for a position: the draft's opinion
// if Apply has been pressed, else the stored claim's columns, else the INSERT
// defaults for a position being added. Always a fresh object — the modal edits
// a copy and hands it back through setAdvanced.
function advancedFor(state, node) {
    const cell = state.cells[node];
    const src = (cell && cell.advanced) || state.priorAdvanced[node] || null;
    const out = clone(ADVANCED_DEFAULTS);
    // KEY BY KEY, SKIPPING null — not Object.assign. Go marshals an empty
    // []string as null (cloneStrings reads empty as nil, and the field has no
    // omitempty because the modal is allowed to clear the list), so a straight
    // merge replaces the default [] with null and the first .map on it throws.
    // Absent and null both mean "the default", which is what a caller wants to
    // read either way.
    for (const key of Object.keys(out)) {
        if (src && src[key] !== null && src[key] !== undefined) out[key] = clone(src[key]);
    }
    return out;
}

// advancedShows says whether the modal draws a field at all, from flowspec.
// A field this choreography forbids or does not read is not a control the
// engineer should be given — the old claim editor's claimFieldVisibility said
// the same thing with a hand-written list.
function advancedShows(state, node, key) {
    const cell = state.cells[node];
    if (!cell) return false;
    const row = steadyRow(state, cell.role, cell.mode);
    if (!row) return true;
    const need = row[flowspecFieldFor(key)];
    return need !== 'forbidden' && need !== 'unused';
}

// advancedSet is which drawn fields carry a value — the row's "N set" badge
// and each section's count.
//
// THE RULE IS domain.ClaimHas's, field for field: a non-empty string, a true
// flag, a non-zero number, a non-empty list, and the carry-over disposition's
// exception that blank-or-replace is the absence of an opinion. A second
// spelling of "has a value" would put an indigo count on a row the server
// reads as untouched.
function advancedSet(state, node, adv) {
    const a = adv || advancedFor(state, node);
    const out = [];
    for (const key of Object.keys(ADVANCED_DEFAULTS)) {
        if (!advancedShows(state, node, key)) continue;
        const v = a[key];
        if (key === 'changeover_carryover_disposition') {
            if (v && v !== 'replace') out.push(key);
            continue;
        }
        if (key === 'reorder_point_source') continue;   // a stamp on the number, not a value of its own
        if (Array.isArray(v) ? v.length > 0 : Boolean(v)) out.push(key);
    }
    return out;
}

// rowColumns is what the Partner and Staging cells hold for one position:
// one entry per chip, in the order they are drawn, with the cell field it
// writes, the word in front of it and whether a blank one is a finding.
//
// An empty list means the column draws a plain dash — and a dash is not a
// control (there is no field behind it to write, and writing one anyway is how
// a save came back refused for a value nobody chose).
function rowColumns(state, node, column) {
    const cell = state.cells[node];
    const out = [];
    if (!cell || !cell.mode) return out;
    const row = steadyRow(state, cell.role, cell.mode);
    for (const spec of ROW_COLUMNS[column] || []) {
        const need = row ? row[spec.field] : 'used';
        if (need === 'forbidden' || need === 'unused') continue;
        const value = cell[spec.key] || '';
        out.push({
            key: spec.key,
            field: spec.field,
            label: spec.label[cell.mode] || fieldWord(state, spec.field),
            value: value,
            required: need === 'required',
            possible: !!value || hasCandidate(state, node, spec.key),
        });
    }
    return out;
}

// fieldRequired is flowspec's word for one claim field under this cell's
// (role, mode) — the only thing that decides whether an empty cell on the
// desktop's table is a chip or a dash. The page asks rather than carrying a
// list, for the same reason advancedShows does: a hand-written copy of this
// table is the thing the old claim editor got wrong.
function fieldRequired(state, node, field) {
    const cell = state.cells[node];
    if (!cell || !cell.mode) return false;
    const row = steadyRow(state, cell.role, cell.mode);
    return !!row && row[field] === 'required';
}

// hasCandidate answers whether an EMPTY optional field could be filled at all
// on this press. flowspec says which fields a choreography HAS; it cannot say
// whether the hardware has anywhere to put them.
//
// The one field this matters for is `secondPaired`, the optional third
// (back-most) position of a press index — "when set, the layout is C → B → A"
// (domain/process.go:278). Hopkinsville's Press 400 has two back positions and
// both are already somebody's pair, so there is no third to choose and an
// empty `Third` chip is a control with nothing behind it. Every other field's
// candidates are routing-set names or press positions, which a press that runs
// the mode at all has by construction.
function hasCandidate(state, node, key) {
    if (key !== 'secondPaired') return true;
    // NOT freeBackPosition(): that one excludes the asking cell, because its
    // job is to find this cell a PAIR and this cell's own pair should not
    // block it. A third position has to clear every claim including this
    // cell's own, or PLN_01 would be offered PLN_02 — the slot it already
    // indexes from — as the thing behind PLN_02.
    const taken = new Set();
    for (const n of Object.keys(state.cells)) {
        const c = state.cells[n];
        if (!c.on) continue;
        if (c.paired) taken.add(c.paired);
        if (c.staging) taken.add(c.staging);
        if (c.parkOld) taken.add(c.parkOld);
        if (c.secondPaired && n !== node) taken.add(c.secondPaired);
    }
    return backPositions(state).some(n => n !== node && !taken.has(n));
}

// ── init ─────────────────────────────────────────────────────────────────────
function init(opts) {
    const positions = (opts.positions || []).slice()
        .sort((a, b) => (a.sequence || 0) - (b.sequence || 0));
    const claims = opts.claims || [];
    const state = {
        styleId: opts.styleId,
        styleName: opts.styleName || '',
        positions: positions,
        routing: opts.routing || [],
        // How many rows of each role the process has and has NOT switched on.
        // The rows themselves are deliberately absent (the view filters them);
        // the count is what lets a role with nothing to offer say which of the
        // two empties it is. See routingNote.
        routingOff: opts.routingOff || {},
        // TWO PART LISTS, AND CONFLATING THEM BLOCKS EVERY CHANGEOVER.
        //
        // `parts` is what THIS STYLE runs — its claimed payloads, plus whatever
        // the operator has added to the flow — and every entry of it that is
        // not on a position raises `unplaced_part`, which turns the bar
        // `blocked`, which disables Start on the HMI and Save on the desktop.
        //
        // `palette` is what the PROCESS may run: the offer list the part picker
        // draws (domain.ComposerData.Palette). It is a dozen or two names on a
        // real cell, and it has nothing to do with whether this flow is ready —
        // a cell with twelve parts in its set and one on a position would
        // otherwise read "11 parts need a position" and refuse to change over.
        parts: (opts.parts || []).slice(),
        palette: (opts.palette || []).slice(),
        // The style's expected CATID, for the part picker's order alone. Not a
        // rule: see partOffers.
        catid: opts.catid || '',
        lastRun: opts.lastRun || null,
        flowspec: opts.flowspec || null,
        // groups is the routing group -> member nodes map the dock strip's third
        // line names. Without it that line is blank; it is never the positions
        // (those are already in the note above it).
        groups: opts.groups || {},
        // The travel network as an adjacency, built ONCE here rather than on
        // every panel open. Carried by reference through reduce; see
        // viaWaypoints. opts.scene is the station's ComposerScene or the
        // desktop's ComposerMap — both are {edges:[{from,to,len}]}.
        sceneAdj: sceneIndex(opts.scene),
        // Every claim on the process, across styles — the third role source.
        processClaims: opts.processClaims || [],
        cells: {},
        priorClaims: {},
        // What the Advanced modal OPENS on, keyed by position: the stored
        // claim's twelve policy columns, from the view's per-style block. Read
        // only — the draft's copy lives on the cell and starts null.
        priorAdvanced: opts.advanced || {},
        roleSources: {},
        selected: null,
        preview: null,
        previewStale: false,
    };
    for (const p of positions) {
        const node = p.core_node_name;
        const cell = blankCell();
        const d = deriveRole(claims, opts.processClaims, node);
        cell.role = d.role;
        state.roleSources[node] = d.source;
        state.cells[node] = cell;
    }
    for (const c of claims) {
        state.priorClaims[c.core_node_name] = c;
        const cell = state.cells[c.core_node_name];
        if (!cell) continue;
        cell.on = true;
        cell.mode = c.swap_mode || null;
        cell.part = c.payload_code || null;
        cell.source = c.inbound_source || '';
        cell.dest = c.outbound_destination || '';
        cell.staging = c.inbound_staging || '';
        cell.parkOld = c.outbound_staging || '';
        cell.paired = c.paired_core_node || '';
        cell.secondPaired = c.second_paired_core_node || '';
        cell.keyRoute = (c.key_route || []).slice();
        cell.evacNodes = (c.changeover_evac_nodes || []).slice();
        cell.evacDest = c.changeover_evac_destination || '';
        cell.role = c.role || cell.role;
    }
    return state;
}

// ── reduce ───────────────────────────────────────────────────────────────────
function reduce(state, action) {
    const s = clone(state);
    // flowspec and the scene adjacency are DATA, not state — clone() would
    // deep-copy them on every keypress, and the adjacency is 840 edges.
    s.flowspec = state.flowspec;
    s.sceneAdj = state.sceneAdj;
    const cell = action && action.node ? s.cells[action.node] : null;
    switch (action && action.type) {

        case 'addPosition': {
            if (!cell) break;
            cell.on = true;
            if (!cell.mode && action.mode) cell.mode = action.mode;
            if (!cell.source) cell.source = defaultRouting(s, 'source');
            if (!cell.dest) cell.dest = defaultRouting(s, 'destination');
            // THE PAIR THE STYLE ALREADY HAD WINS. A position put back after a
            // remove returns to the back position it was paired with, not to
            // whichever one happens to be free — the operator undid a mistake,
            // they did not ask to be re-partnered.
            if (!cell.paired) {
                const prior = s.priorClaims[action.node];
                cell.paired = (prior && prior.paired_core_node) || freeBackPosition(s, action.node);
            }
            ensurePartner(s, action.node);
            s.selected = action.node;
            break;
        }

        case 'removePosition': {
            if (!cell) break;
            // The partner loses its role with the position it served — SPEC §3.
            const partners = [cell.paired, cell.staging, cell.parkOld].filter(Boolean);
            s.cells[action.node] = Object.assign(blankCell(), { role: cell.role });
            for (const p of partners) {
                if (s.cells[p] && !s.cells[p].on) s.cells[p] = Object.assign(blankCell(), { role: s.cells[p].role });
            }
            if (s.selected === action.node) s.selected = null;
            break;
        }

        case 'setMode': {
            if (!cell) break;
            cell.mode = action.mode || null;
            clearForbidden(s, cell);
            ensurePartner(s, action.node);
            break;
        }

        case 'setPart': {
            if (!cell) break;
            // ONE POSITION PER PART. A part dragged onto a second position is a
            // move, not a copy: two positions filling the same part is a flow
            // nobody meant and the server would not catch it.
            if (action.payloadCode) {
                for (const n of Object.keys(s.cells)) {
                    if (n !== action.node && s.cells[n].part === action.payloadCode) s.cells[n].part = null;
                }
            }
            cell.part = action.payloadCode || null;
            break;
        }

        // THE SEAM THE PART PICKER DISPATCHES. It adds the payload to THIS
        // STYLE's parts — the list the strip draws chips for and the list
        // `unplaced_part` reads — and it does NOT touch the palette, which is
        // the process's offer list and is the server's to change.
        //
        // NEVER PRE-PLACED. The part arrives loose, as an amber `· unplaced`
        // chip, and the operator drops it on a position. Choosing one for them
        // would be the composer deciding a flow, and the finding's own detail
        // ("drop each on a position, or take it off this flow") is the
        // instruction that follows.
        case 'addPart': {
            if (action.payloadCode && s.parts.indexOf(action.payloadCode) < 0) {
                s.parts.push(action.payloadCode);
            }
            break;
        }

        case 'setPartner': if (cell) cell.paired = action.partner || ''; break;
        // The third press position, where the hardware has one — flowspec
        // marks second_paired_core_node Used on a press index and Forbidden
        // everywhere else, so clearForbidden wipes it on any other mode.
        case 'setSecondPartner': if (cell) cell.secondPaired = action.partner || ''; break;
        case 'setStaging': if (cell) cell.staging = action.staging || ''; break;
        case 'setParkOld': if (cell) cell.parkOld = action.staging || ''; break;
        case 'setSource': if (cell) cell.source = action.source || ''; break;
        case 'setDest': if (cell) cell.dest = action.dest || ''; break;
        // KEY ROUTE IS AN ORDERED LIST. SEER drives the points in the order
        // given, so LM167 › LM9 and LM9 › LM167 are different routes and the
        // action has to be able to say which. It takes the WHOLE list: a
        // caller adding, removing or reordering a point computes the new route
        // and hands it over, which keeps one action instead of three and keeps
        // the model free of an editing protocol. A bare string is still
        // accepted — the station's S5 picks exactly one waypoint — and an
        // empty string or empty list clears the route.
        case 'setVia':
            if (cell) {
                cell.keyRoute = Array.isArray(action.via) ? action.via.filter(Boolean).slice()
                    : (action.via ? [action.via] : []);
            }
            break;

        case 'setDockSource':
            for (const n of Object.keys(s.cells)) if (s.cells[n].on) s.cells[n].source = action.source || '';
            break;
        case 'setDockDest':
            for (const n of Object.keys(s.cells)) if (s.cells[n].on) s.cells[n].dest = action.dest || '';
            break;

        // A PRESET IS A SHAPE, NEVER A PART (SYNTH R-L1). So an apply replaces
        // the shape and CARRIES THE PART ACROSS: the preset has no opinion
        // about which part sits where, and an apply that dropped the parts
        // would make the strip card destructive on the floor — tap a card to
        // change how the press swaps, and the operator's placements are gone
        // with no undo and nothing said.
        //
        // It read `built[n]` over a blank cell, and a preset's cells are
        // payload-free by construction (the shape is stripped before it is
        // stored, domain/flow_preset_shape.go), so `part` came back null every
        // time.
        //
        // THE ROLE IS THE PRESS'S, NOT THE FLOW'S, and it is assigned LAST so
        // that stays true. It was assigned first and then overwritten by the
        // shape's own — `presetCells` carries `role: r.role || ''`, and
        // FlowCell.Role has no omitempty, so a shape whose role is blank
        // blanked the position's. A cell with no role finds no flowspec row
        // (`steadyRow(state, '', mode)` is null), so nothing reads as Required,
        // the desktop draws dashes where chips belong, and the save sends a
        // claim with an empty role.
        //
        // A POSITION THE PRESET DOES NOT NAME RELEASES ITS PART, and does not
        // keep one on an off cell. The strip's part chip reads "placed" from
        // ANY cell carrying the part (composer-render.js:382), off ones
        // included, so a part parked on a position the new shape dropped would
        // report as placed while being nowhere in the flow. Released, it goes
        // amber and says `· unplaced`, which is the true thing and the one the
        // operator can act on.
        case 'applyPreset': {
            // THROUGH presetCells, AND THAT IS THE FIX. This read
            // `action.preset.cells` and indexed it by position name — which
            // works for the STATION, whose strip card carries
            // ComposerPreset.Cells as a map keyed by position, and does not
            // work for the DESKTOP, whose apply modal carries PresetRow.Shape
            // as the server's ARRAY of FlowCells. `built['PLN_01']` on an
            // array is undefined, so every cell fell to the second arm and the
            // apply wrote an EMPTY flow: `Save to 1 part` would have deleted
            // every claim of the part it was applied to, under a diff that
            // said `PLN_01 · added to the flow`, because shapeDiff normalises
            // through presetCells and this did not.
            //
            // One parser for both shapes, which is what presetCells was
            // written for; the diff and the apply now read the same cells.
            const built = presetCells(action.preset);
            for (const n of Object.keys(s.cells)) {
                const role = s.cells[n].role;
                const part = s.cells[n].part;
                s.cells[n] = built[n]
                    ? Object.assign(blankCell(), built[n], { on: true, part: part, role: role })
                    : Object.assign(blankCell(), { role: role });
            }
            s.selected = null;
            break;
        }

        case 'startBlank': {
            for (const n of Object.keys(s.cells)) {
                s.cells[n] = Object.assign(blankCell(), { role: s.cells[n].role });
            }
            s.selected = null;
            break;
        }

        case 'select': s.selected = action.node || null; break;

        // The two changeover fields the cell already carries, from the same
        // sheet as the advanced block. They are HERE and not in it because the
        // picture draws what they do — a marked position is a card the setup
        // clears — and a field the picture draws is the cell's.
        case 'setEvacNodes': if (cell) cell.evacNodes = (action.nodes || []).slice(); break;
        case 'setEvacDest': if (cell) cell.evacDest = action.dest || ''; break;

        // Apply on the Advanced modal. The whole block at once — the modal
        // shows every field of it, so there is no half-opinion to express —
        // and null puts the position back on Expand's carry-through.
        case 'setAdvanced': {
            if (!cell) break;
            cell.advanced = action.advanced ? clearForbiddenAdvanced(s, cell, clone(action.advanced)) : null;
            break;
        }
        default: break;
    }
    // Any structural change invalidates the last preview until a new one lands.
    if (action && action.type !== 'select') s.previewStale = true;
    return s;
}

// An index position needs its structural partner; a fresh one takes the first
// free back position in sequence order (SPEC §3).
function ensurePartner(state, node) {
    const cell = state.cells[node];
    if (!cell || !cell.on) return;
    if (cell.mode !== 'two_robot_press_index' && cell.mode !== 'sequential') return;
    if (cell.paired && state.cells[cell.paired]) return;
    const free = freeBackPosition(state, node);
    if (free) cell.paired = free;
}

// enabled !== false, not enabled === true: the station view hands the composer a
// routing set it has ALREADY filtered to the adopted members, so those rows carry
// no flag at all. Requiring true here would leave every default blank on the real
// screen while the test fixture (which sets it) passed.
function defaultRouting(state, role) {
    const r = (state.routing || []).filter(x => x.enabled !== false && x.role === role)
        .sort((a, b) => (a.sequence || 0) - (b.sequence || 0))[0];
    return r ? r.core_node_name : '';
}

// ── the wire ─────────────────────────────────────────────────────────────────
// The set of positions that are only somebody else's partner — they are drawn,
// they are never rows (SPEC S9's "back positions, no row, paired").
function partnerOnly(state) {
    const out = new Set();
    for (const n of Object.keys(state.cells)) {
        const c = state.cells[n];
        if (!c.on || !c.mode) continue;
        for (const p of [c.paired, c.staging, c.parkOld]) {
            if (p && state.cells[p] && !state.cells[p].on) out.add(p);
        }
    }
    return out;
}

function toCells(state) {
    const only = partnerOnly(state);
    const out = [];
    for (const p of state.positions) {
        const n = p.core_node_name;
        const c = state.cells[n];
        if (!c || !c.on || !c.mode) continue;   // a position with no choreography is not yet a row
        if (only.has(n)) continue;
        // THE FOUR THAT ARE ALWAYS SPOKEN are a cell's identity: the
        // position, its role, its choreography and its part. A reader that
        // had to test for those would be reading something that is not a cell.
        const cell = {
            core_node_name: n,
            role: c.role,
            swap_mode: c.mode,
            payload_code: c.part || '',
        };
        // THE PARTNER AND ROUTING FIELDS ARE OMITTED WHEN BLANK, the same
        // omitempty the Go FlowCell carries — which is what the round-trip
        // pin compares this against. Blank and absent were always the same
        // value to both sides (Expand reads a blank cell field as a blank
        // column; init above reads a missing key as ''), and spelling them
        // out cost a third of every cell on a payload the HMI parses.
        const optional = {
            paired_core_node: c.paired,
            second_paired_core_node: c.secondPaired,
            inbound_source: c.source,
            inbound_staging: c.staging,
            outbound_staging: c.parkOld,
            outbound_destination: c.dest,
            changeover_evac_destination: c.evacDest,
        };
        for (const k of Object.keys(optional)) {
            if (optional[k]) cell[k] = optional[k];
        }
        // An empty slice is an absent key for the same reason.
        if (c.evacNodes && c.evacNodes.length) cell.changeover_evac_nodes = c.evacNodes.slice();
        if (c.keyRoute && c.keyRoute.length) cell.key_route = c.keyRoute.slice();
        // Spoken only when the engineer opened the modal — omitempty again,
        // and this one carries the whole meaning of the field: an absent key
        // is Expand's carry-through, a present one is an opinion.
        if (c.advanced) cell.advanced = clone(c.advanced);
        out.push(cell);
    }
    return out;
}

// ── preview ──────────────────────────────────────────────────────────────────
// Takes the body of a 200 OR a 400 (the no-orders case carries the same fields).
function applyPreview(state, res) {
    const s = clone(state);
    s.flowspec = state.flowspec;
    s.sceneAdj = state.sceneAdj;
    s.preview = {
        actions: (res && res.actions) || [],
        // NULL, NOT ZERO, when the server sent no count. A preview of the
        // RUNNING style is validated and fingerprinted and never planned
        // (owner ruling R3), so `order_count` is absent from its body — and
        // zero is a real and different answer the bar blocks on.
        orderCount: (res && typeof res.order_count === 'number') ? res.order_count : null,
        running: !!(res && res.running),
        findings: (res && res.findings) || [],
        unresolved: (res && res.unresolved) || [],
        preflight: (res && res.preflight) || null,
        fingerprint: (res && res.fingerprint) || '',
        error: (res && res.error) || '',
    };
    s.previewStale = false;
    return s;
}

// ── findings ─────────────────────────────────────────────────────────────────
// THREE LOCAL FINDINGS. A position with no choreography, a position with no
// part, and — owner ruling R8, 2026-09-12 — a part with no position. The first
// is one the server cannot raise, because a cell with no choreography is not a
// cell toCells() sends; the other two the server raises too, from the stored
// rows, and its copies are dropped below rather than counted twice. Everything
// else is the preview's findings[], mapped through findingShort. The client
// never invents a finding the server would not raise.
function findings(state) {
    const out = [];
    for (const p of state.positions) {
        const n = p.core_node_name;
        const c = state.cells[n];
        if (!c || !c.on) continue;
        if (!c.mode) {
            out.push({
                local: true, node: n, field: 'swap_mode', short: FINDING_SHORT.swap_mode,
                message: n + ' · how does it swap?',
                detail: 'Tap the position and pick a choreography.',
            });
        } else if (!c.part) {
            out.push({
                local: true, node: n, field: 'payload_code', short: FINDING_SHORT.payload_code,
                message: n + ' · which part runs here?',
                detail: 'Tap the position and pick one.',
            });
        }
    }
    // A PART WITH NO POSITION IS A FINDING (owner ruling R8, 2026-09-12).
    //
    // An apply that leaves parts unplaced SAVES — the engineer is part way
    // through moving a part onto a new shape and a half-built flow is not a
    // reason to lose their work. What must not happen is a CHANGEOVER the
    // robots cannot serve: nothing would be delivered for that part and the
    // line would run dry on it. So it blocks the START and not the save.
    //
    // ONE FINDING FOR ALL OF THEM, with no node. "Which position?" is the
    // question the finding exists to ask, so naming one would be answering it.
    // The amber `unplaced` chips on the strip already say WHICH parts, and the
    // detail names them again for the bar, which shows one line.
    //
    // A FLOW WITH NO CHOREOGRAPHY LEAVES NOTHING UNPLACED. Every part is loose
    // before the first position is set up, and `2 parts need a position` over a
    // blank stage is the screen scolding an operator for not having started —
    // `Nothing in the flow yet` is what that state says, and a position added
    // but not yet answered for says `how does it swap?` and nothing else. The
    // finding is about a flow that EXISTS and does not carry one of its parts,
    // which is the state an apply leaves behind.
    //
    // PLACED MEANS ON AN ACTIVE CELL, which is the server's rule
    // (domain.ValidateFlowPartsPlaced reads the DRAFT, and a cell that is off
    // is not in the draft). This looked at every cell including the off ones,
    // so a part left on a position the operator had switched off read as
    // placed here and as lost on the server — the bar said the flow was ready
    // and the preview came back with a finding under it.
    const loose = activeCells(state).length
        ? state.parts.filter(p => !activeCells(state).some(([, c]) => c.part === p))
        : [];
    if (loose.length) {
        out.push({
            local: true, node: '', field: 'unplaced_part', parts: loose.slice(),
            short: FINDING_SHORT.unplaced_part,
            message: loose.length === 1 ? '1 part needs a position' : loose.length + ' parts need a position',
            detail: loose.map(shortPart).join(', ') + ' — drop each on a position, or take it off this flow.',
        });
    }
    const pv = state.preview;
    if (pv) {
        // ONE PROBLEM, ONE ROW. The local findings above and the server's
        // validator answer some of the same questions — a cell with a mode and
        // no part is sent, and comes back as a payload_code error on that node
        // — and a bar whose first line counts them would have said `2 things to
        // fix` about one. Keyed by node AND field, so two problems on one
        // position still read as two.
        // The server's copy of the unplaced-part finding carries no node and
        // the payload_code field (domain.ValidateFlowPartsPlaced); the local
        // one is keyed unplaced_part. Named here rather than matched by shape,
        // because two findings that mean one thing under two field names is a
        // fact about this pair and not a rule worth generalising.
        const key = (node, field) => (node || '') + '|' +
            (!node && field === 'payload_code' ? 'unplaced_part' : field);
        const local = new Set(out.map(f => key(f.node, f.field)));
        for (const f of pv.findings) {
            if (local.has(key(f.core_node_name, f.field))) continue;
            out.push({
                local: false, node: f.core_node_name, field: f.field, side: f.side,
                severity: f.severity, short: findingShort(f), message: f.message,
                detail: f.message,
            });
        }
    }
    return out;
}

function findingShort(f) {
    if (!f) return '';
    const s = FINDING_SHORT[f.field];
    if (s) return s;
    // Never blank: an unmapped field shows the server's own sentence rather
    // than an empty pill the operator cannot act on.
    return (f.message && String(f.message)) || String(f.field || 'needs attention');
}

// One-tap fixes, for the four the spec names. Returns null when there is
// nothing safe to offer — a fix-it button that guesses is worse than none.
function fixItFor(state, f) {
    if (!f) return null;
    const node = f.node || f.core_node_name;
    // A FINDING WITH NO NODE HAS NO ONE-TAP FIX. Every arm below writes to
    // `state.cells[node]`, and an unplaced part's finding deliberately names no
    // position — offering `Use PIA09` there would dispatch a setPart onto a
    // cell that does not exist and report nothing.
    if (!node) return null;
    switch (f.field) {
        case 'payload_code': {
            const free = state.parts.find(p => !Object.keys(state.cells).some(n => state.cells[n].part === p));
            const part = free || state.parts[0];
            if (!part) return null;
            return { label: 'Use ' + shortPart(part), action: { type: 'setPart', node: node, payloadCode: part } };
        }
        case 'inbound_staging': {
            const free = freeBackPosition(state, node);
            if (!free) return null;
            return { label: 'Stage at ' + free, action: { type: 'setStaging', node: node, staging: free } };
        }
        case 'outbound_destination': {
            const d = defaultRouting(state, 'destination');
            if (!d) return null;
            return { label: 'Send to ' + d, action: { type: 'setDest', node: node, dest: d } };
        }
        default:
            return null;   // swap_mode has no one-tap answer: it is the operator's choice
    }
}

function shortPart(p) { return String(p || '').replace(/^.*?(PIA\d+|Payload)$/, '$1'); }

// ── derived views ────────────────────────────────────────────────────────────

// legs is WHICH choreography lines exist and what each one says. The drawing
// of them — where they sit on screen, the rounding, the dot and the ring — is
// operator-flow.js's legsFor, which takes these. Two questions, two places:
// this one is about the flow, that one is about the layout.
//
// glyphs() sat here beside it and answered a third question nobody asked: it
// had no reader on either surface, only its own test.
function legs(state) {
    const out = [];
    for (const p of state.positions) {
        const n = p.core_node_name;
        const c = state.cells[n];
        if (!c || !c.on || !c.mode) continue;
        if (c.mode === 'two_robot_press_index' && c.paired) {
            out.push({ robot: 2, kind: 'index', from: c.paired, to: n, label: 'Robot 2 indexes' });
        } else if (c.mode === 'two_robot' && c.staging) {
            out.push({ robot: 1, kind: 'move', from: c.staging, to: n, label: 'Robot 1 moves in' });
        }
    }
    return out;
}

// WHICH ROBOTS THE CHOREOGRAPHY USES, not how many legs got drawn: an index
// flow draws one leg and still takes two robots (Robot 1 supplies the deck,
// and that trip is outside the cell so it has no line).
//
// THE WORDS, NOT A COUNT, and one set of them. The set-up card said "Robot 1
// and Robot 2" and the started screen said "2 robots" — the same fact about
// the same flow, spelled two ways, on two screens an operator sees one after
// the other. SPEC §0.7 names the robots and never abbreviates them, so that
// is the spelling, and the pluralisation the render layer carried for the
// other one goes with it.
//
// THE BAR NO LONGER SAYS IT AT ALL (owner ruling R1, 2026-09-12) — a preview
// cannot know how many AMRs the fleet will send. This answers what the
// CHOREOGRAPHY uses, which is a fact about the flow and not about the fleet.
function robotWords(state) {
    const twoRobot = activeCells(state).some(([, c]) =>
        c.mode === 'two_robot_press_index' || c.mode === 'two_robot');
    return twoRobot ? 'Robot 1 and Robot 2' : 'Robot 1';
}

function activeCells(state) {
    return state.positions
        .map(p => [p.core_node_name, state.cells[p.core_node_name]])
        .filter(([, c]) => c && c.on && c.mode);
}

// ── the part picker's rows ───────────────────────────────────────────────────
//
// partOffers is the process's part set as the picker sheet draws it: one row
// per payload, the ones this style is already running marked, and the ones
// whose code carries the style's CATID first.
//
// HERE AND NOT IN EITHER RENDERER. The HMI's sheet and the desktop's chip
// picker offer the same list, and the day one of them decided its own order
// would be the day an operator and an engineer read two different part lists
// for one cell. Both call this.
//
// THE CATID ORDER IS AN ORDER AND NOT A FILTER. A scan carries the part
// identity, and on a cell with two dozen parts the one the operator just
// scanned should be at the top — but a CATID that matches nothing must not
// empty the sheet, because the operator may be setting up a part the PLC has
// never seen. Every row is always offered; only the order moves.
//
// CONTAINMENT, BECAUSE THERE IS NO JOIN. Nothing on this tree relates a
// payload code to a CATID except the characters they share — styles.
// expected_catid is the value the PLC reports and payload_code is Core's part
// code — so the test is "the code contains the CATID", which is checkable by
// eye and wrong in no direction that costs anything: a false match sorts a row
// up, and the row is still the row.
function partOffers(state) {
    const catid = String((state && state.catid) || '').trim().toUpperCase();
    const running = new Set((state && state.parts) || []);
    const rows = ((state && state.palette) || []).map(code => ({
        code: code,
        // Already on this flow: the sheet says so rather than offering it as
        // new, and tapping it is still harmless (addPart is idempotent).
        onFlow: running.has(code),
        match: !!catid && String(code).toUpperCase().indexOf(catid) >= 0,
    }));
    rows.sort((a, b) => {
        if (a.match !== b.match) return a.match ? -1 : 1;
        return a.code < b.code ? -1 : (a.code > b.code ? 1 : 0);
    });
    return rows;
}

// partAllowed says whether this position may carry a part at all, from
// flowspec — the same question advancedShows asks of an Advanced control.
//
// A CHOREOGRAPHY THAT FORBIDS payload_code HAS NO PART ROW. Drawing one there
// is offering a control whose value the validator refuses, and the operator
// finds out at the preview rather than at the tap. A cell with no mode yet
// keeps its row: the part is how most operators start.
function partAllowed(state, node) {
    const cell = state.cells[node];
    if (!cell || !cell.mode) return true;
    const row = steadyRow(state, cell.role, cell.mode);
    return !row || row.payload_code !== 'forbidden';
}

// membersOf names the routing group's nodes — SMN_05, SMN_06 — which is what
// the strip's third line shows under the group name. The note above it already
// names the POSITIONS, so repeating them here said the same thing twice and
// never said where the bins actually come from.
function membersOf(state, group) {
    const m = (state.groups || {})[group];
    return Array.isArray(m) ? m.slice() : [];
}

function dockNotes(state) {
    const act = activeCells(state);
    const srcs = [...new Set(act.map(([, c]) => c.source || '—'))];
    const dsts = [...new Set(act.map(([, c]) => c.dest || '—'))];
    const inTargets = act.map(([n, c]) =>
        c.mode === 'two_robot_press_index' ? (c.paired || '?')
            : c.mode === 'two_robot' ? (c.staging || '?') : n);
    const outFrom = act.map(([n]) => n);
    return {
        in: {
            group: srcs.join(' · ') || '—',
            note: act.length ? 'Inbound source · Robot 1 → ' + inTargets.join(', ')
                : 'Inbound source',
            members: srcs.length === 1 ? membersOf(state, srcs[0]) : [],
            targets: inTargets,
        },
        out: {
            group: dsts.join(' · ') || '—',
            note: act.length ? 'Outbound destination · Robot 2 ← ' + outFrom.join(', ')
                : 'Outbound destination',
            members: dsts.length === 1 ? membersOf(state, dsts[0]) : [],
            targets: outFrom,
        },
    };
}

function cardLines(state, node) {
    const c = state.cells[node];
    if (!c) return [];
    // A MODE WITH NO CARD LINE IS A CARD, NOT A CRASH. CARDLINE holds the four
    // choreographies MODES names; style_node_claims also carries the legacy
    // `simple` and `manual_swap` values, and `CARDLINE[c.mode](c)` on one of
    // those threw a TypeError — on the BOARD's own read-only flow panel, which
    // is drawn from whatever the running style happens to carry. One throw
    // there takes the module down and the panel renders nothing.
    //
    // The fallback is the mode's own name, which is true and readable, rather
    // than a guess at what the legacy row means.
    if (c.on && c.mode) {
        const line = CARDLINE[c.mode];
        return line ? line(c).split(' · ') : [String(c.mode)];
    }
    // A BACK POSITION DOING SOMEBODY'S WORK SAYS WHOSE — and now says WHICH
    // work. "PLN_05 is feeding PLN_06 — should it show PLN_05 with inbound
    // staging or whatever? Same for outbound staging if applicable." It said
    // "staging for PLN_06" for both, so the card could not tell an engineer
    // whether that slot takes the new bin in or the old one out, which is the
    // difference between the two trips drawn on the picture around it.
    //
    // THE SAME TWO WORDS THE PANEL'S ROW USES, but written out rather than
    // taken from fieldWord — which is the opposite of what F3 usually asks,
    // and the reason is this surface. operator-display.html deliberately does
    // NOT load flowspec-data.js (it says so, and why), so on the read-only
    // picture an operator actually reads, fieldWord has no labels and falls
    // back to the field's own name. `outbound_staging` is the documented
    // fallback and it is right in a refusal — on a shop-floor card it is worse
    // than the "staging for PLN_03" it would be replacing. F3 forbids a SECOND
    // NAME for a field; this is the same name, in sentence case.
    for (const p of state.positions) {
        const o = state.cells[p.core_node_name];
        if (!o || !o.on || !o.mode) continue;
        if (o.paired === node) return ['on deck for ' + p.core_node_name];
        if (o.staging === node) return ['Inbound staging', 'for ' + p.core_node_name];
        if (o.parkOld === node) return ['Outbound staging', 'for ' + p.core_node_name];
    }
    const pos = state.positions.find(p => p.core_node_name === node);
    return [(pos && pos.kind === 'back') ? 'back position' : 'front position'];
}

// pictureCells is model state in the shape renderFlowPicture draws: the
// server's cell picture with each position's claim replaced by what the
// operator has built.
//
// ONE ADAPTER, TWO SURFACES. It was written out twice, character for
// character, differing only in how each file spells its own state — and the
// day a field was added to a cell would have been the day the HMI and the
// desktop drew the same press differently. It belongs here because it is a
// question about model state, not about either page.
//
// The base picture travels through untouched (Object.assign over it), so the
// positions keep their geometry, their kind and their partner captions; only
// the claim is the draft's.
function pictureCells(state, base) {
    base = base || { positions: [] };
    const positions = (base.positions || []).map(p => {
        const c = state.cells[p.core_node_name];
        const on = c && c.on && c.mode;
        return Object.assign({}, p, {
            claim: on ? {
                swap_mode: c.mode, payload_code: c.part || '',
                paired_core_node: c.paired || '', inbound_staging: c.staging || '',
                outbound_staging: c.parkOld || '', inbound_source: c.source || '',
                outbound_destination: c.dest || '',
            } : null,
        });
    });
    return Object.assign({}, base, { positions: positions });
}

// ── robot drives via ─────────────────────────────────────────────────────────
//
// ONE KEY-ROUTE WALK, AND IT IS THE HMI'S. The station walked the scene for
// LM waypoints along a position's supply path; the desktop had its own picker
// that filtered the routing set for `role === 'waypoint'` — a role the schema
// forbids on purpose (domain/routing_set.go: "'waypoint' is deliberately not a
// role. A key route is ordered and validated against the vendor map, not
// against this list") — so it offered nothing, ever, and the desktop's whole
// key-route authoring was inert.
//
// The HMI's answers are the ones already pinned and the ones operators use, so
// they are the ones that survive. The desktop reads this now and gains a
// picker that works.
//
// THE ADJACENCY IS BUILT ONCE, when the composer opens: sceneIndex runs in
// init and is carried by reference through reduce, the same way flowspec is,
// because clone() would deep-copy 840 edges on every keystroke. It was rebuilt
// on every panel open.

function sceneIndex(scene) {
    const adj = {};
    for (const e of (scene && scene.edges) || []) {
        if (!e || !e.from || !e.to) continue;
        const len = e.len || 1;
        (adj[e.from] = adj[e.from] || []).push({ to: e.to, len: len });
        (adj[e.to] = adj[e.to] || []).push({ to: e.from, len: len });
    }
    return adj;
}

// A binary heap, not an array re-sorted on every pop. The scene is 840 edges
// and the search runs on a touch screen; an O(n log n) sort per step turns a
// walk into a stutter on the one interaction that is supposed to feel instant.
//
// TIES BREAK BY INSERTION ORDER, and that is not a detail. The array version
// used a STABLE sort, so among equal distances the earliest-pushed point won
// and the route it produced was the route operators have been offered. A heap
// makes no such promise on its own, and a different winner at a tie is a
// different shortest path and a different waypoint list — a silent change to
// what the HMI offers. The sequence number makes the order the same one.
function heapLess(a, b) { return a[0] !== b[0] ? a[0] < b[0] : a[1] < b[1]; }

function heapPush(h, d, n) {
    h.push([d, h.seq = (h.seq || 0) + 1, n]);
    let i = h.length - 1;
    while (i > 0) {
        const p = (i - 1) >> 1;
        if (!heapLess(h[i], h[p])) break;
        const t = h[p]; h[p] = h[i]; h[i] = t;
        i = p;
    }
}

function heapPop(h) {
    const top = h[0], last = h.pop();
    if (!h.length) return top;
    h[0] = last;
    let i = 0;
    for (;;) {
        const l = 2 * i + 1, r = l + 1;
        let m = i;
        if (l < h.length && heapLess(h[l], h[m])) m = l;
        if (r < h.length && heapLess(h[r], h[m])) m = r;
        if (m === i) break;
        const t = h[m]; h[m] = h[i]; h[i] = t;
        i = m;
    }
    return top;
}

// viaWaypoints is the LM points along this position's shortest supply path,
// evenly spaced, at most four. Empty when there is no scene: a waypoint list
// invented without a map is a route the robot cannot drive, and the row then
// offers "shortest way" alone.
function viaWaypoints(state, node) {
    const c = state.cells[node];
    const adj = state.sceneAdj;
    if (!c || !adj) return [];
    const target = c.mode === 'two_robot_press_index' ? (c.paired || node)
        : (c.mode === 'two_robot' ? (c.staging || node) : node);
    const src = ((state.groups || {})[c.source] || [])[0];
    if (!src || !adj[src]) return [];
    const dist = { [src]: 0 }, prev = {}, seen = new Set();
    const q = [];
    heapPush(q, 0, src);
    while (q.length) {
        const [d, , u] = heapPop(q);
        if (u === target) break;
        if (seen.has(u)) continue;
        seen.add(u);
        for (const e of adj[u] || []) {
            const nd = d + e.len;
            if (dist[e.to] === undefined || nd < dist[e.to]) { dist[e.to] = nd; prev[e.to] = u; heapPush(q, nd, e.to); }
        }
    }
    if (dist[target] === undefined) return [];
    const path = [];
    for (let n = target; n !== undefined; n = prev[n]) { path.unshift(n); if (n === src) break; }
    const lms = path.filter(n => /^LM/.test(n));
    const step = Math.max(1, Math.floor(lms.length / 4));
    return lms.filter((_, i) => i % step === 0).slice(0, 4);
}

// ── the bottom bar ───────────────────────────────────────────────────────────
function bar(state) {
    const fs = findings(state);
    const active = activeCells(state);
    if (!active.length && !fs.length) {
        return {
            heading: 'Nothing in the flow yet',
            detail: 'Tap a position to add it, or pick a flow above',
            tone: 'empty', fixIt: null,
            button: { label: 'Save and start', enabled: false },
        };
    }
    if (fs.length) {
        const first = fs[0];
        const fix = fixItFor(state, first);
        return {
            heading: fs.length + ' thing' + (fs.length > 1 ? 's' : '') + ' to fix before you can start',
            detail: first.message + (first.detail ? ' — ' + first.detail : ''),
            tone: 'blocked', fixIt: fix,
            button: { label: 'Fix the flow to continue', enabled: false },
        };
    }
    // A PREVIEW THAT FIRES NO ORDERS IS NOT AN OK PREVIEW. The seam answers 400
    // with the same body shape when the flow plans nothing, and the refusals it
    // cannot plan around (Core unavailable for a press-index bin type, say)
    // answer 400 with `error` alone. Either way there is nothing to start, and
    // saying "Preview OK · 0 orders" over an enabled button is the composer
    // offering to start a changeover that would move no material.
    const pv = state.preview;
    // THE RUNNING STYLE IS SAVEABLE AND NOT PREVIEWED (owner ruling R3,
    // 2026-09-12). The engine writes its rows and starts nothing; the runtime
    // reads them on its next trip. There is no changeover to plan, so there is
    // no order count, and the bar says both halves of that rather than
    // borrowing the blocked arm's `0 orders` - which would be the composer
    // refusing a save the engine allows.
    // ON THE NEXT TRIP, NOT AT THE NEXT CHANGEOVER. This said changeover, and
    // the comment directly above it already said otherwise — "the runtime
    // reads them on its next trip" — so the bar was contradicting its own
    // reason on screen. The engine's ruling is explicit (flow_compose.go,
    // refuseRunningPositionMove): "SOURCES, DESTINATIONS AND ROUTES SAVE
    // FREELY MID-RUN, and take effect on the next trip. Every runtime reader
    // of the running style's flow resolves its claim at the moment it needs
    // one ... None of them caches."
    //
    // THE SWAP MODE IS ONE OF THOSE FIELDS. The produce tick, the release tap
    // and the request-empty path all read claim.SwapMode off a claim they
    // loaded for that operation (operator_produce.go's loadActiveNode,
    // operator_release.go, operator_bin_ops.go), so changing how a position
    // swaps changes how the next swap runs. Telling an engineer it waits for a
    // changeover is telling them to start one they do not need.
    //
    // The ONE edit that must wait is moving a position on or off the running
    // style, and that is refused outright with its own sentence naming the
    // position — so this heading never covered it either.
    if (pv && pv.running) {
        return {
            heading: 'running — saved changes take effect on the next trip',
            detail: 'not previewed while running · an order already on its way finishes as planned',
            tone: 'ok', fixIt: null,
            button: { label: 'Save flow', enabled: !state.previewStale },
        };
    }
    if (pv && (pv.error || pv.orderCount === 0)) {
        return {
            heading: pv.error || 'This flow fires no orders',
            detail: pv.error
                ? 'Check the flow, or ask an engineer if this needs Core.'
                : 'Nothing would move. Check the parts and the positions.',
            tone: 'blocked', fixIt: fixItFor(state, findings(state)[0]),
            button: { label: 'Fix the flow to continue', enabled: false },
        };
    }
    const orders = pv ? pv.orderCount : 0;
    const d = dockNotes(state);
    const via = [...new Set(active.flatMap(([, c]) => c.keyRoute || []))];
    // F3: the claim's names, here too. It read `Empties in X · old bins to Y ·
    // supply via Z` — three invented phrases for three fields that have names.
    let detail = 'Inbound source ' + d.in.group + ' · outbound destination ' + d.out.group +
        (via.length ? ' · key route ' + via.join(', ') : '');
    if (pv && pv.preflight && pv.preflight.state === 'unchecked') {
        detail = 'Inventory not checked — Core is unreachable';
    }
    return {
        // NO ROBOT COUNT (owner ruling R1, 2026-09-12). It read `· 2 robots`,
        // counted off the choreography's roles, and a preview cannot know how
        // many AMRs the fleet will send: a two-role flow can be driven by one
        // robot making both trips, and six orders can be spread over five. The
        // roles are still named where they mean something — the picture's
        // legend, the order rows, the started screen's sentence — and the bar
        // says the one number it can stand behind.
        heading: 'Preview OK · ' + orders + ' orders',
        detail: detail,
        tone: 'ok', fixIt: null,
        button: {
            label: 'Save and start',
            enabled: !state.previewStale && !!pv,
        },
    };
}

// ── the ORDERS sentences (U10 P1, ruled R2 on 09-12) ─────────────────────────
//
// ONE SHAPE FOR EVERY ROBOT: `Robot N · from → to · what`, the robot's name in
// the robot's colour and nothing else coloured. The HMI's S9 and the desktop's
// confirm both read this, so an operator and an engineer checking the same
// changeover read the same sentences.
//
//   Robot 1 · Supermarket Empty Totes → PLN_02 · PIA27
//   Robot 2 · PLN_03 → Supermarket Area
//
// WHAT IT REPLACED, and why. S9 said `Robot 1 · brings the new bin to PLN_02`
// and `Robot 2 · takes the old bin from PLN_01` — a verb phrase per order type,
// each naming ONE end of a move. An operator standing at the press checking
// whether the robots are about to do the right thing needs both ends: where
// the bin comes from is the half that says whether the right supermarket was
// picked, and it was the half that was missing.
//
// THE PRESS'S OWN INDEX GETS NO ROW. On a two-robot press index the press
// itself advances the bin from the back position to the line, and no robot
// drives that leg. The heading says ORDERS, an index is not one, and a row for
// it under that heading is a row claiming a robot will do something it will
// not. (It is still drawn on the PICTURE, where the heading is the press.)
function orderSentences(state) {
    const pv = state.preview;
    if (!pv || !pv.actions) return [];
    const out = [];
    for (const a of pv.actions) {
        // SupplyOrder is Robot 1 and EvacOrder is Robot 2, positionally, which
        // is what the planner means by the names (changeover/plan.go): they
        // describe what the order does, not which side of a pair it is on. A
        // single-robot mode fills one of them and the row then says Robot 1
        // for a supply and Robot 2 for an evac, which is how the picture's
        // legend colours them too.
        for (const [spec, robot] of [[a.supply_order, 1], [a.evac_order, 2]]) {
            if (!spec) continue;
            const from = spec.from || '';
            const to = spec.to || spec.delivery_node || a.core_node_name || '';
            // A LEG WITH ONLY ONE END IS NOT A SENTENCE. Rather than print
            // `→ PLN_02` and let the operator guess, the row is left out and
            // the order count in the bar still says how many there are.
            if (!from || !to) continue;
            out.push({
                robot: robot,
                from: from,
                to: to,
                // The new bin is named by its part; the old one has no part to
                // name, because what comes out is whatever is in there.
                //
                // NO BIN WORD (owner ruling R2). It read `PIA27 tote` and
                // `old tote`, with the word resolved from the style's payload
                // catalog. What the trip carries is said by the PART when
                // there is one to name, and by nothing when there is not: an
                // evacuation takes out whatever is in there, which is why the
                // row for it is `Robot 2 · PLN_03 → Supermarket Area` and
                // stops there.
                what: robot === 1 ? (spec.payload_code ? shortPart(spec.payload_code) : '') : '',
                node: a.core_node_name || '',
            });
        }
    }
    return out;
}

// orderTrip is what the robot does: `PLN_03 → Supermarket Area`, and the part
// after it when there is one.
//
// `· what` ONLY WHEN THERE IS ONE, and THIS is where that rule lives. A supply
// trip names the part it carries; an evacuation takes out whatever is in there
// and the sentence stops (owner ruling R2, 2026-09-12 — the bin word it used
// to end on is gone). The confirm sheet built the same string from the fields
// and printed a trailing separator with nothing after it —
// `PLN_03 → Supermarket Area ·` — which is what one rule in two places looks
// like from the floor.
function orderTrip(o) {
    return o.what ? o.from + ' → ' + o.to + ' · ' + o.what : o.from + ' → ' + o.to;
}

// orderSentence is the whole line, robot included, for a surface that prints
// one string. The confirm sheet gives the robot its own table cell — the hue
// is an identity there and a whole line in it reads as a warning — so it takes
// orderTrip and names the robot itself.
function orderSentence(o) {
    return 'Robot ' + o.robot + ' · ' + orderTrip(o);
}

// ── presets: the shape, and what an apply would change (U10) ─────────────────

// presetCells turns a preset's shape — the server's array of FlowCells, or the
// composer block's map keyed by position — into the model's own cell shape,
// keyed by position. One reader for both, because the strip card and the
// Presets tab are handed the same shape by two different reads.
function presetCells(preset) {
    const out = {};
    if (!preset) return out;
    const src = preset.cells || preset.shape || [];
    const rows = Array.isArray(src) ? src : Object.keys(src).map(n => Object.assign({ core_node_name: n }, src[n]));
    for (const r of rows) {
        const n = r.core_node_name || r.node || '';
        if (!n) continue;
        // Either spelling: the wire's snake_case FlowCell, or a cell already in
        // the model's words (what the composer block's `presets` carries).
        out[n] = {
            mode: r.swap_mode || r.mode || null,
            role: r.role || '',
            part: null,                       // a preset carries none, by construction
            source: r.inbound_source || r.source || '',
            dest: r.outbound_destination || r.dest || '',
            staging: r.inbound_staging || r.staging || '',
            parkOld: r.outbound_staging || r.parkOld || '',
            paired: r.paired_core_node || r.paired || '',
            secondPaired: r.second_paired_core_node || r.secondPaired || '',
            evacDest: r.changeover_evac_destination || r.evacDest || '',
            evacNodes: (r.changeover_evac_nodes || r.evacNodes || []).slice(),
            keyRoute: (r.key_route || r.keyRoute || []).slice(),
        };
    }
    return out;
}

// shapeDiff says what applying a shape would change, in the words D1 heads its
// columns with: `[{node, label, from, to}]`, in position order then field order.
//
// THE APPLY MODAL SHOWS THIS BEFORE IT SHOWS A SAVE. An engineer ticking eight
// parts is agreeing to eight writes, and "apply" is not a description of a
// write — `PLN_04 · old bins to: Supermarket Area → Empty Tote Return` is.
//
// It is the same comparison domain.CompareFlowShape makes, over the same
// eleven fields in the same order (SHAPE_FIELDS, held to Go by
// TestShapeFieldWordsMatchTheModel). A position one side names and the other
// does not is reported as the whole position arriving or leaving, because that
// is what it is — not as eleven fields each going blank.
function shapeDiff(state, preset) {
    const want = presetCells(preset);
    const out = [];
    const names = [];
    for (const p of state.positions) {
        const n = p.core_node_name;
        const live = state.cells[n] && state.cells[n].on;
        if (live || want[n]) names.push(n);
    }
    for (const n of names) {
        const to = want[n];
        const from = (state.cells[n] && state.cells[n].on) ? state.cells[n] : null;
        // A WHOLE POSITION ARRIVING OR LEAVING IS ONE LINE, not eleven fields
        // each going blank, and it is worded as the event rather than as a
        // from-to pair: "PLN_01 · added to the flow" reads, where "PLN_01 ·
        // the position: not in the flow -> added" is the same fact spelled as
        // a field change, which it is not.
        if (!from) { out.push({ node: n, label: 'added to the flow', whole: true }); continue; }
        if (!to) { out.push({ node: n, label: 'removed from the flow', whole: true }); continue; }
        for (const f of SHAPE_FIELDS) {
            const a = f.of(from), b = f.of(to);
            if (a === b) continue;
            out.push({ node: n, label: fieldWord(state, f.field), from: a || '—', to: b || '—' });
        }
    }
    return out;
}

// ── a role with nothing to offer says why ────────────────────────────────────
//
// NEVER A HEADING OVER NOTHING (owner ruling, Amendment A 2026-09-16).
//
// The composer offers a process only its routing set, so a process with no
// enabled rows in a role gets a heading and no chips. An engineer at
// Hopkinsville read four of those — inbound staging, outbound staging, inbound
// source, outbound destination — as the derivation being broken. It was not:
// the set was empty, and the card said nothing, which from where they stood is
// the same thing.
//
// TWO EMPTIES, TWO SENTENCES, because they have two different next actions. A
// role nobody has put names in needs names. A role whose names were found from
// the flows and never switched on needs a switch — and the second one is
// invisible from the card without the count the view now carries, which is why
// it carries it.
//
// HERE, AND NOT IN EITHER RENDERER. The station's cell card and the desktop's
// Flows pickers draw from the same composer response and would otherwise say
// this in two voices, or — as happened — in one and none.
const ROLE_WORD = {
    source: 'inbound source',
    staging: 'staging',
    destination: 'outbound destination',
};

// FIELD_ROLE maps a card row to the routing role its options come from. The
// two staging fields share one role, because the routing set has one.
const FIELD_ROLE = {
    inbound_source: 'source',
    outbound_destination: 'destination',
    inbound_staging: 'staging',
    outbound_staging: 'staging',
};

function routingRoleOf(field) { return FIELD_ROLE[field] || ''; }

// ── the one field that is not a routing role, and its own two empties ────────
//
// PAIRED IS EMPTY FOR A DIFFERENT REASON AND NEEDS A DIFFERENT SENTENCE
// (2026-09-16). Amendment A gave every routing role a sentence and left
// paired_core_node silent on purpose, because its options are the press's OWN
// POSITIONS and "add them in Settings › Routing" would be false about them. The
// silence was worse than the wrong words.
//
// At Hopkinsville the 4x2's five positions are ALL kind `front` — a position
// becomes `back` only when some live claim of the process names it as a pair or
// a staging slot (domain.BackPositionNames), and nothing on that cell ever
// has — so composer-render's optionsFor offers a 2-robot index NO back
// position, while flowspec marks paired_core_node REQUIRED for that mode. The
// operator picked the choreography and got the heading of the one field that
// blocks the save, with nothing under it, no sentence and no fix-it.
//
// TWO CHOREOGRAPHIES PAIR AND THEY PAIR WITH DIFFERENT THINGS, so they get
// different sentences: a press index pairs with a BACK position (which is why
// addPosition defaults its pair to freeBackPosition), and Sequential A/B flips
// between two positions of any kind.
//
// THE NEXT ACTION IS THE DESKTOP, and it is real: the desktop's Flows picker
// offers the backs AND the other positions for a pair (processes-desktop's
// optionsFor, its `partnering` branch), so the first pair on a press with no
// back position is made there, and the station offers it from the next save on.
// That is the same shape as the routing sentences' "Settings › Routing" — the
// fact, then somewhere to go.
// THE WORD IS CELL, NOT PRESS (owner, 2026-09-17: "it could be a weld cell or
// some other process"). These three sentences are read by an operator standing
// at a 4x2 weld cell as often as at a press, and the code's own name for the
// thing they are about has always been CellPicture. The press-index mode keeps
// its own words: that one really is about a press indexing.
const PAIRED_NOTE = {
    two_robot_press_index: 'No back position on this cell — a ' + MODES.two_robot_press_index +
        ' pairs with one, and a position becomes a back position only when a flow names it. ' +
        'Pair this one from the desktop’s Flows, or pick another choreography.',
    sequential: 'Only one position on this cell — ' + MODES.sequential +
        ' fills one side while the other runs, so it needs a second position to flip to.',
};

// pairedNote reads the choreography off the cell the card is open on. `node` is
// the position the panel passes; state.selected is the fallback, and it is the
// same node — the panel selects a position before it draws a row for it.
function pairedNote(state, node) {
    const c = ((state && state.cells) || {})[node || (state && state.selected)] || {};
    // A mode nobody has written words for still never draws a heading over
    // nothing. ROW_FIELDS gives paired_core_node to exactly the two above; a
    // third would arrive here before anyone noticed it had no sentence.
    return PAIRED_NOTE[c.mode] || 'Nothing on this cell to pair this position with.';
}

// routingNote is the line under an empty row, or '' when the row has options.
//
// `offered` is how many the SCREEN can offer, which for staging is the routing
// rows plus the process's own back positions — a press that parks on its own
// back slot has staging without a routing row, and telling its operator the set
// is empty would be false.
//
// `node` is the position the row belongs to, and only paired_core_node reads
// it; the dock panel and the desktop's pickers have no position and pass none.

// How many switched-off names the sentence spells out before it counts the
// rest. Three fits the card's width and is enough to recognise a role by: an
// engineer who sees SLN_09 knows instantly whether the staging role holds
// staging lanes or, as they feared, supermarket slots.
const ROUTING_NOTE_NAMES = 3;

function routingNote(state, field, offered, node) {
    if (offered) return '';
    // The one card row whose options are positions rather than routing rows —
    // see PAIRED_NOTE. It is still not a routing role (routingRoleOf answers ''
    // for it) and the sentence must never send anyone to Settings › Routing to
    // look for a press position.
    if (field === 'paired_core_node') return pairedNote(state, node);
    const role = routingRoleOf(field);
    if (!role) return '';
    const word = ROLE_WORD[role] || role;
    const off = ((state.routingOff || {})[role] || []).slice();
    if (off.length) {
        // THE NAMES, BECAUSE A COUNT IS NOT CHECKABLE. This said "7 staging
        // nodes … switched off" and the first question off the floor was which
        // seven — the engineer was looking at supermarket locations and had no
        // way to tell from the card that the staging seven were SLN lanes and
        // the SMNs in front of them were the source and destination rows, a
        // different seven. Naming a few turns the sentence into something that
        // can be agreed with or disputed without leaving the screen.
        const shown = off.slice(0, ROUTING_NOTE_NAMES).join(', ');
        const rest = off.length - Math.min(off.length, ROUTING_NOTE_NAMES);
        // AGREEING IN NUMBER MATTERS ON A SHOP-FLOOR SCREEN: "turn them on"
        // over a count of one reads as a second thing the operator has not
        // found yet.
        return off.length + ' ' + word + ' node' + (off.length === 1 ? '' : 's') +
            ' found from your flows, switched off — ' + shown +
            (rest ? ' and ' + rest + ' more' : '') +
            '. Turn ' + (off.length === 1 ? 'it' : 'them') + ' on in Settings › Routing';
    }
    return 'No ' + word + ' nodes in this process’s routing set — add them in Settings › Routing';
}

// ── exported constants the render layer needs (it invents no copy) ───────────
function modeLabels() { return Object.assign({}, MODES); }
function modeHelp() { return Object.assign({}, MODEHELP); }
function rowFields() { return clone(ROW_FIELDS); }
function fieldLabel(state, field) { return fieldWord(state, field); }

function advancedDefaults() { return clone(ADVANCED_DEFAULTS); }

// EXPORTED WITHOUT A GLOBAL LEXICAL NAME. Two classic scripts on one page
// share one global lexical environment, so a top-level `const api` in each is
// a redeclaration: the SECOND script dies before its last line and its global
// is never set. That is exactly what processes.html was doing —
// composer-model.js and desktop-bodies.js both declared `const api`, so
// window.DesktopBodies was undefined and every write button on the Processes
// page threw. The block is a function scope now, in both files, and
// www/classic_script_globals_test.go proves no page can make that mistake
// again.
(function () {
    const api = {
        init, reduce, toCells, applyPreview,
        legs, dockNotes, cardLines, pictureCells, bar,
        findings, findingShort, fixItFor,
        modeLabels, modeHelp, rowFields, rowColumns, fieldRequired, fieldLabel, shortPart, robotWords,
        partOffers, partAllowed,
        routingNote, routingRoleOf,
        presetCells, shapeDiff, orderSentences, orderSentence, orderTrip, viaWaypoints,
        advancedFor, advancedShows, advancedSet, advancedDefaults,
    };

    if (typeof module !== 'undefined' && module.exports) module.exports = api;
    if (typeof window !== 'undefined') window.ComposerModel = api;
})();
