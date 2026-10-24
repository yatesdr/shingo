// operator-flow.js — the picture of the cell, with the dock the bins come from
// and go to. Two pictures, one function: a changeover cell drawn by relation,
// every other cell as one module per position, laid out like that swap mode's
// glyph.
//
// THE PICTURE BY RELATION (recipe groups, 2026-10-24; SYNTH round 2 rows
// 12-20). A cell with a changeover — a claim in one of the four choreographies
// — is drawn as cards in a grid, one position to a cell, placed by
// composer-model.js's relationLayout from the plant map's left and right and
// its front and back, never its distances: "whats important is showing
// relation to points" (owner, 2026-10-23). Every position is a card (what is
// not in play fades, which replaces usedOnly), each move is a chevron pair on
// the card doing it, a recipe group's L/R tag sits on its cards, and the lanes
// that are not positions stand in a band under the grid. relationScope is the
// gate; a market, a loader or an unloader keeps the module picture byte for
// byte, and a changeover cell the map cannot place falls back to it with one
// line in the margin saying why. See drawRelation.
//
// THE MODULE PICTURE. The drawing used to place position cards on a to-scale
// projection of the cell (schematic when that failed), hang the staging lanes
// in a band above the dock, and draw the choreography as lines between cards.
// It was replaced on 2026-10-05 by the locked module reference
// (REFERENCE-module-picture-2026-10-05.html; sections 1, 2, 4 and 5 are the
// builder's spec, and the numbers table at its foot is the sizes this file
// draws from). One module per position in that swap mode's glyph: the press
// card on top, its staging slots under it, an index pair as one module with
// the on-deck position inside it, a sequential A/B pair as one two-card
// module. One move is one pair of slender chevrons inside the module — no
// lines cross between modules, and the chevrons carry no label (owner pick 1):
// the move sentences sit on the position card, and each staging name appears
// once, on its slot.
//
// COLOUR IS THE ROBOT, NOT THE DIRECTION. Teal is Robot 1, indigo is Robot 2 —
// on the chevrons, the in/out marks, the dock notes and the words "Robot 1" /
// "Robot 2" on the cards. Which robot makes a move is read from the model's
// legs and each cell's own dock robots (legRobot, dockRobot), never written into a template,
// and the legend — drawn inside the picture — lists only the robots the flow
// uses.
//
// NOTHING IS MEASURED TO PLACE A BOX. Every size is a constant — the module
// picture's from the reference's numbers table, the relation picture's one
// card and one gap a column — and a long name is cut in the middle, both ends
// kept and the full name the tooltip, so nothing can overlap or run off its
// card at any width. On the module picture the frame's width decides the
// wrap. The relation picture never wraps and never shrinks: a grid wider than
// the frame makes the picture wider, and the caller's frame scrolls sideways.
//
// THE PICTURE REPORTS ITS OWN SIZE. The layout consumes the frame's width
// and nothing else; the height comes out of the packing (rows of fixed-size
// modules or cards over the dock band) and is written back to the caller
// through opts.height, and the width — the frame's, or the relation grid's
// when that is wider — through opts.width. The desktop sizes its frame from
// them; the station's flow panel scrolls instead of scaling. Nothing in this
// file reads the frame's height.
//
// DATA COMES FROM THE STATION VIEW, and only from there: view.cell (positions
// with their scene coordinates, kinds and the running claim on each — built in
// Go by domain.BuildCellPicture), view.current_style, view.station. The move
// sentences and the dock notes come from composer-model.js — this file
// computes no sentence of its own (see sentencesFromView below). On the module
// picture a staging node is a slot in the module that stages there even when
// it is a position of the cell; a position with no claim of its own is then
// drawn only as that slot, never as a card as well. On the relation picture
// every position is a card in its own cell, a staging position included, and
// only a lane that is not a position is a slot (in the band).

import { el } from './operator-util.js';
// The QUOTE-SAFE escape, not operator-util's. This file builds attributes by
// concatenation — data-pos, data-tap, transform — and the base's escaper
// leaves `"` alone. See shared/esc.js.
import { esc } from '/static/shared/esc.js';
import { getView } from './operator-state.js';

// ── the numbers table (the module reference, 2026-10-05) ───────────────────

export const CARD_W = 188, CARD_H = 92;   // position card: today's, unchanged
const SLOT_W = 116, SLOT_H = 44;          // staging slot: fixed width, name truncates
const TAG_X = SLOT_W - 56;                // a shared slot's tag: 60, as on the reference
const DECK_H = 56;                        // the on-deck position inside an index module
const BAR = 12;                           // room for the line-side bar above the card
const GAP = 38;                           // card → slot row; one move lives here
const ROW_GAP = 12;                       // on-deck card → a slot under it with no move between
const M_W = 2 * SLOT_W + 12;              // 244: two slots and the gap between them
const INSET = (M_W - CARD_W) / 2;         // 28: the card is centred; stubs live in the inset
const GX = 20, GY = 28;                   // between modules, between rows
const FIT = 20;                           // the picture's own margin
const DOCK_BAND = 74;                     // today's dock, unchanged
const CHIP_H = 24;                        // an unused-staging chip's height

// STATION_FRAME is the picture's rest width and the frame the station panel
// is built around. The height is the panel's scroll floor, not a layout input.
export const STATION_FRAME = { w: 1280, h: 560 };

// ── truncation ─────────────────────────────────────────────────────────────
//
// fit cuts by a character budget rather than by measuring: node names are
// capitals, digits and underscores, which run wide, so they get their own
// budget. A node name keeps BOTH ends — the tail is what tells
// …IN_01 from …OUT_1 apart — and the full name goes back as the tooltip.

function fit(text, px, size, weight, caps) {
    weight = weight || 600;
    const per = size * (caps ? 0.64 : weight >= 600 ? 0.58 : 0.52);
    const n = Math.max(3, Math.floor(px / per));
    if (text.length <= n) return [String(text), null];
    if (caps) {
        const head = Math.floor(n / 2), tail = n - 1 - head;
        return [text.slice(0, head) + '…' + text.slice(text.length - tail), text];
    }
    return [text.slice(0, n - 1) + '…', text];
}

// txt draws one fitted text element. 'ln' lines name each robot in that
// robot's colour — a tspan around the words, never a second text element, so
// the sentence stays one sentence to a screen reader and to the tests that
// read these cards back as text. Card names and lines carry NO inline
// tooltip — theirs is the card group's (nameTip), and a reader stripping tags
// must not see a cut name twice.
function txt(cls, x, y, text, px, size, weight, anchor) {
    const shown = fit(text, px, size, weight, cls === 'nm');
    let body = esc(shown[0]);
    if (cls === 'ln') {
        for (let r = 1; r <= 2; r++) {
            body = body.split('Robot ' + r).join('<tspan class="rw r' + r + '">Robot ' + r + '</tspan>');
        }
    }
    const a = anchor ? ' text-anchor="' + anchor + '"' : '';
    return '<text class="' + cls + '" x="' + x + '" y="' + y + '"' + a + '>' + body + '</text>';
}

// ── the glyphs ─────────────────────────────────────────────────────────────

const IN = '<path d="M6 0v9M2 5l4 4 4-4M0 12h12" fill="none" stroke="currentColor" stroke-width="1.6"/>';
const OUT = '<path d="M6 12V3M2 7l4-4 4 4M0 14h12" fill="none" stroke="currentColor" stroke-width="1.6"/>';

// move is one pair of slender chevrons centred in the gap a→b, rotated to the
// segment's angle. The pair, not an arrowhead: two open strokes read as "this
// way" from across a cell, where a filled triangle reads as decoration.
// Coordinates are rounded to a tenth — the markup is read back as text by the
// pins, and a float tail is a diff nobody can read for no accuracy anyone can
// see.
//
// data-from and data-to name the two boxes the move joins — a position, a
// staging slot, or 'dock' for the dock band — so a reader of the markup can
// tell which trip a pair of chevrons is without working it out from their
// coordinates.
function move(a, b, robot, from, to) {
    const dx = b[0] - a[0], dy = b[1] - a[1];
    const L = Math.hypot(dx, dy);
    const ux = dx / L, uy = dy / L;
    const th = Math.atan2(dy, dx) * 180 / Math.PI;
    const mx = (a[0] + b[0]) / 2 + ux * 2.5, my = (a[1] + b[1]) / 2 + uy * 2.5;
    let out = '';
    for (const d of [-3.6, 3.6]) {
        out += '<path class="sc r' + robot + '" d="M-5 -4.2L0 0L-5 4.2" transform="translate(' +
            (mx + ux * d).toFixed(1) + ' ' + (my + uy * d).toFixed(1) + ') rotate(' + th.toFixed(1) + ')"/>';
    }
    return '<g class="mv" data-from="' + esc(from) + '" data-to="' + esc(to) + '">' + out + '</g>';
}

function bar(x, y, w, dim) {
    return '<path class="press' + (dim ? ' dim' : '') + '" d="M' + (x + 26) + ' ' + (y + 5) + 'H' + (x + w - 26) + '"/>';
}

// ── the cards ──────────────────────────────────────────────────────────────
//
// All coordinates are ABSOLUTE: the module wrapper carries no transform of its
// own, and every card, slot and chevron is placed in picture coordinates
// directly. The pins read these transforms back as boxes, and a card transform
// that were relative to its module would read three cards on top of each
// other.

// io draws the in/out corner marks. Which robot each mark belongs to is the
// class, not an inline style — the rules in flow-picture.css name the shared
// token, because both surfaces that draw this picture load it.
function io(kind, robot, x, y) {
    return '<g class="io r' + robot + '" transform="translate(' + x + ',' + y + ')">' +
        (kind === 'in' ? IN : OUT) + '<text x="16" y="11" class="iot">' + kind + '</text></g>';
}

// nameTip is the group-level tooltip for a card whose name was cut: the full
// name, on the card's own group — hovering anywhere on the card names it. It
// sits before the rect so a name reader stripping tags sees the name alone.
function nameTip(name, px, size) {
    const shown = fit(name, px, size, 600, true);
    return shown[1] !== null ? '<title>' + esc(shown[1]) + '</title>' : '';
}

function posCard(x, y, name, lines, part, robots, on, sel, bad, showIo, tap, tgt) {
    const nmPx = CARD_W - 28 - (on && showIo ? 80 : 0);
    const cls = 'node ' + (on ? 'on' : 'off') + (sel ? ' sel' : '') + (bad ? ' bad' : '') + (tgt ? ' tgt' : '');
    let s = '<g class="' + cls + '" data-pos="' + esc(name) + '"' + (tap || '') +
        ' transform="translate(' + x + ',' + y + ')">';
    s += nameTip(name, nmPx, 15);
    s += '<rect class="box" width="' + CARD_W + '" height="' + CARD_H + '" rx="12"/>';
    s += txt('nm', 14, 26, name, nmPx, 15);
    (lines || []).forEach((l, i) => { s += txt('ln', 14, 43 + i * 14, l, CARD_W - 28, 12, 500); });
    if (on && showIo) {
        s += io('in', robots.in, CARD_W - 80, 10) + io('out', robots.out, CARD_W - 46, 10);
    }
    if (bad) {
        s += '<rect class="needbg" x="12" y="64" width="' + (CARD_W - 24) + '" height="20" rx="5"/>' +
            txt('needlbl', CARD_W / 2, 78, bad, CARD_W - 40, 11, 600, 'middle');
    } else if (part) {
        s += '<rect class="partbg" x="12" y="64" width="' + (CARD_W - 24) + '" height="20" rx="5"/>' +
            txt('partlbl', CARD_W / 2, 78, part, CARD_W - 40, 11, 600, 'middle');
    } else if (tgt) {
        s += '<rect class="tgtbg" x="12" y="64" width="' + (CARD_W - 24) + '" height="20" rx="5"/>' +
            txt('tgtlbl', CARD_W / 2, 78, 'move here', CARD_W - 40, 11, 600, 'middle');
    }
    return s + '</g>';
}

function deckCard(x, y, name, line, inRobot, sel, bad, tap) {
    let s = '<g class="node on deck' + (sel ? ' sel' : '') + (bad ? ' bad' : '') +
        '" data-pos="' + esc(name) + '"' + (tap || '') +
        ' transform="translate(' + x + ',' + y + ')">';
    s += nameTip(name, CARD_W - 28 - 44, 15);
    s += '<rect class="box" width="' + CARD_W + '" height="' + DECK_H + '" rx="12"/>';
    s += txt('nm', 14, 24, name, CARD_W - 28 - 44, 15);
    s += txt('ln', 14, 42, line, CARD_W - 28, 12, 500);
    s += io('in', inRobot, CARD_W - 46, 10);
    return s + '</g>';
}

// slot is a staging place inside its module: fixed width, the lane name cut to
// fit, and the field it is as the caption. A SHARED slot — one two flows use —
// keeps its caption to its first word and carries a "shared" tag on the
// caption row, so two modules can draw the same lane without either lying
// about who it parks for. The name keeps the slot's full width; the caption
// word gets the room from its own x to the tag's left edge (TAG_X), which
// holds "Outbound" whole — the slot's place under the card already says it
// is staging, and the reference draws the shared slot the same way. A TAP GOES TO THE POSITION THIS LANE SERVES, which
// is the thing an operator can change — there is no panel for a lane itself.
//
// tip, when given, is the slot's whole tooltip in place of the cut name's: a
// slot no running leg reaches says what it is for.
function slotCard(x, y, name, caption, shared, sel, tap, tip, tgt) {
    let s = '<g class="stage on' + (sel ? ' sel' : '') + (tgt ? ' tgt' : '') +
        '" data-staging="' + esc(name) + '"' + (tap || '') +
        ' transform="translate(' + x + ',' + y + ')">';
    s += tip ? '<title>' + esc(tip) + '</title>' : nameTip(name, SLOT_W - 24, 13);
    s += '<rect class="box" width="' + SLOT_W + '" height="' + SLOT_H + '" rx="10"/>';
    s += txt('nm', 12, 19, name, SLOT_W - 24, 13);
    if (shared) {
        s += txt('ln', 12, 35, caption.split(' ')[0], TAG_X - 12, 11, 500);
        s += '<rect class="tagbg" x="' + TAG_X + '" y="24.5" width="46" height="14" rx="7"/>' +
            '<text class="tag" x="' + (TAG_X + 23) + '" y="35" text-anchor="middle">shared</text>';
    } else {
        s += txt('ln', 12, 35, caption, SLOT_W - 22, 11, 500);
    }
    return s + '</g>';
}

// ── the route strip (owner, 2026-09-17 — kept as it was) ───────────────────
//
// "THE POINT OF THE LMs ISN'T TO REPRESENT THEM TO SCALE, IT'S TO DIRECT FLOW."
// It hangs under the slot or card its trip arrives at, in that trip's robot's
// colour, rising into the bottom edge with the chevron pointing in: the
// ordered waypoints an engineer chose, numbered in driving order and read
// BOTTOM TO TOP — the first waypoint is the furthest out, the last is the one
// the robot arrives from. Evenly spaced is
// the whole of "not to scale", and more than fits is COUNTED, not dropped: the
// top row becomes "+N" rather than the route being silently shortened.

const STRIP_SLOTS = 4;     // waypoint rows the strip draws; the rest are counted
const STRIP_HEAD = 28;     // the slot's bottom edge down to the top row
const STRIP_STEP = 21;     // one row to the next
const STRIP_TAIL = 18;     // the bottom row down to the line's foot
const STRIP_DX = 11;       // the label column, right of the line

function stripRoom(n) { return STRIP_HEAD + (n - 1) * STRIP_STEP + STRIP_TAIL; }

// oneStrip at x, hanging from top. Names beyond STRIP_SLOTS are COUNTED, not
// dropped: the top waypoint row gives its place to the count ('+N').
function oneStrip(route, x, top, robot) {
    const rows = Math.min(STRIP_SLOTS, route.length);
    const shown = route.slice(0, rows - (route.length > rows ? 1 : 0));
    const more = route.length - shown.length;
    const foot = top + STRIP_HEAD + (rows - 1) * STRIP_STEP + STRIP_TAIL;
    const lx = x + STRIP_DX;
    let out = '<g class="lmroute r' + robot + '">' +
        '<path class="lmline" d="M' + x + ' ' + foot + 'V' + (top + 4) + '"/>' +
        '<path class="lmtip" d="M-5 -4.4L5.5 0L-5 4.4Z" transform="translate(' + x + ',' + (top + 14) + ') rotate(-90)"/>';
    shown.forEach((name, k) => {
        const y = top + STRIP_HEAD + (rows - 1 - k) * STRIP_STEP;   // driving order: first waypoint lowest
        // The number stays whole; the name is cut in the middle like every
        // node name, the full name the label's own tooltip.
        const num = (k + 1) + ' · ';
        const shown = fit(name, SLOT_W - 34 - num.length * 6.4, 11, 600, true);
        out += '<circle class="lmdot" cx="' + x + '" cy="' + y + '" r="4"/>' +
            '<text class="lmlbl" x="' + lx + '" y="' + (y + 4) + '">' +
            (shown[1] !== null ? '<title>' + esc(shown[1]) + '</title>' : '') + esc(num + shown[0]) + '</text>';
    });
    if (more > 0) {
        out += '<text class="lmmore" x="' + lx + '" y="' + (top + STRIP_HEAD + 4) + '">+' + more + '</text>';
    }
    out += '<text class="lmvia" x="' + lx + '" y="' + (foot + 2) + '">Robot ' + robot + ' via</text></g>';
    return out;
}

// ── building the modules ───────────────────────────────────────────────────
//
// modulesOf maps the cell onto module descriptors — which positions group
// into which module, and what orders them. It reads no sentences, so
// moduleOf can ask it for a selection's members without drawing anything.
//
// THE PAIR IS ONE MODULE. A press-index position and its unclaimed partner
// draw as one module, the on-deck card inside it, so layout cannot pull the
// pair apart. A partner that runs its own claim keeps its own module and the
// press draws without a deck — the deck slot would print that position twice,
// and the card's own words ("Robot 1 supplies PLN_01") still say the pairing.
// A sequential A/B pair is one two-card module built from the first claimed
// position whose partner is another claimed sequential position.
//
// A STAGING PLACE IS THE MODULE'S SLOT, WHETHER OR NOT IT IS A POSITION. At a
// press a swap usually stages at a back position; that position is drawn as
// the slot of every module whose claim stages there (each marked shared when
// two do), and a position with no claim of its own that a module draws this
// way gets no card of its own. A position that runs its own claim keeps its
// own module and is drawn as a slot in the other module as well. mod.staged
// lists the positions a module draws as slots, so a selection of one of them
// outlines the module that draws it. A press index's inbound staging is a
// slot of its module as well (see drawModule), under the on-deck card.

// swapSlots is which staging places a swap module draws as slots: the inbound
// staging, and a one-robot swap's outbound staging. A two-robot swap draws no
// outbound slot (Robot 2 takes the old bin straight to the dock); the other
// modes draw no swap slots.
function swapSlots(c) {
    if (!c || (c.swap_mode !== 'two_robot' && c.swap_mode !== 'single_robot')) return { in: null, out: null };
    return {
        in: c.inbound_staging || null,
        out: c.swap_mode === 'single_robot' ? (c.outbound_staging || null) : null,
    };
}

// usedOnly leaves out a position no claim uses and no module draws. The
// station's read-only pictures ask for it (opts.usedOnly): they show the press
// the flow runs, and an idle position drawn as an empty card reads as a spot
// the operator has to do something about. The editing pictures keep every
// position, because those are where a spot can be moved to. A cell with no
// claim at all keeps them too, so its picture is not blank.
function modulesOf(cell, usedOnly) {
    const positions = (cell && cell.positions) || [];
    const byName = {};
    positions.forEach(p => { byName[p.core_node_name] = p; });
    const claimed = positions.filter(p => p.claim && p.claim.swap_mode);
    const mods = [];
    const deckOf = {};   // deck position name → the press that draws it
    for (const p of claimed) {
        if (p.claim.swap_mode !== 'two_robot_press_index') continue;
        const d = p.claim.paired_core_node;
        const dp = d && byName[d];
        if (dp && !(dp.claim && dp.claim.swap_mode) && !deckOf[d]) deckOf[d] = p.core_node_name;
    }
    const seqUsed = new Set();
    for (const p of claimed) {
        if (p.claim.swap_mode !== 'sequential') continue;
        const q = byName[p.claim.paired_core_node];
        if (q && q.claim && q.claim.swap_mode === 'sequential' &&
            !seqUsed.has(p.core_node_name) && !seqUsed.has(q.core_node_name)) {
            seqUsed.add(p.core_node_name);
            seqUsed.add(q.core_node_name);
            mods.push({ kind: 'seq', a: p.core_node_name, b: q.core_node_name, names: [p.core_node_name, q.core_node_name] });
        }
    }
    for (const p of claimed) {
        const n = p.core_node_name, m = p.claim.swap_mode;
        if (seqUsed.has(n)) continue;
        if (m === 'two_robot_press_index') mods.push({ kind: 'index', name: n, names: [n] });
        else if (m === 'two_robot') mods.push({ kind: 'two', name: n, names: [n] });
        else if (m === 'single_robot') mods.push({ kind: 'single', name: n, names: [n] });
        else mods.push({ kind: 'free', name: n, names: [n], on: true });   // a legacy mode word: a card, not a crash
    }
    const slotted = new Set();   // positions some module draws as a slot
    for (const mod of mods) {
        mod.staged = [];
        let names = [];
        if (mod.kind === 'two' || mod.kind === 'single') {
            const sl = swapSlots(byName[mod.name].claim);
            names = [sl.in, sl.out];
        } else if (mod.kind === 'index') {
            names = [byName[mod.name].claim.inbound_staging];
        }
        for (const n of names) {
            if (n && n !== mod.name && byName[n] && mod.staged.indexOf(n) < 0) { mod.staged.push(n); slotted.add(n); }
        }
    }
    for (const p of positions) {
        const n = p.core_node_name;
        if (p.claim && p.claim.swap_mode) continue;
        if (deckOf[n]) continue;   // drawn inside its press's module
        if (slotted.has(n)) continue;   // drawn as a slot in the module that stages there
        if (usedOnly && claimed.length) continue;   // idle, and this picture is read-only
        mods.push({ kind: 'free', name: n, names: [n], staged: [] });
    }
    for (const mod of mods) {
        if (mod.kind !== 'index') continue;
        for (const d of Object.keys(deckOf)) {
            if (deckOf[d] === mod.name) { mod.deck = d; mod.names.push(d); }
        }
    }
    // ORDER: line-side modules along the station's longer axis when the cell
    // has coordinates — west first on an east-west station, north first on a
    // north-south one — and by sequence when it does not; then the back
    // positions that are not inside a module, in the same order among
    // themselves. Where the plant's handedness mirrors the picture's, the row
    // mirrors with it — the order is what the operator walks, not the
    // distance.
    //
    // THE LONGER AXIS, NOT WORLD X ALONE. A weld line that runs north to south
    // drifts a few tenths of a metre east and west, and sorting it by x read
    // that drift as the order: five spots 1.5 m apart came out third, fifth,
    // first, fourth, second. Along its length is the order the line has.
    //
    // LINE-SIDE IS WHERE THE FLOW WORKS: every module a claim covers, in
    // whatever row it stands — a running index press in the back row is
    // line-side, and drawing it after idle cards would swap front and back.
    // An unclaimed front-row card is line-side too. What goes after is a
    // position in worldRows' back row (world Y with coordinates, Kind
    // without — never the grid) that no claim covers and no other module
    // draws: a back-row staging position stands alone at the end, while an
    // on-deck card or the B of an A/B pair stays inside its module.
    //
    // "WITH COORDINATES" IS THE MODEL'S ONE PREDICATE (hasCoords: the cell on
    // the map, every position of it), the one the row words and the relation
    // picture's fallback read too. This read x alone, so a cell the server
    // said was off the map but whose x values were set sorted by x here and
    // was worded by Kind beside it.
    const coords = window.ComposerModel.hasCoords(cell);
    const span = k => Math.max(...positions.map(p => p[k])) - Math.min(...positions.map(p => p[k]));
    const northSouth = coords && span('y') > span('x');
    const rows = worldRows(cell);
    const back = mod => mod.names.every(n => rows[n] === 'back' &&
        !(byName[n].claim && byName[n].claim.swap_mode)) ? 1 : 0;
    const key = mod => {
        const p = byName[mod.names[0]];
        if (northSouth) return -p.y;
        if (coords) return p.x;
        return p.sequence || 0;
    };
    mods.sort((a, b) => back(a) - back(b) || key(a) - key(b) ||
        String(a.names[0]).localeCompare(String(b.names[0])));
    return { mods, byName };
}

// moduleOf names every position drawn in the same module as `name` — the set
// a selection of `name` outlines (drawModule's `sel`). A caller that has to
// know whether a position belongs to the selection asks this rather than
// re-deriving the pairing rules above. [] when no module draws the name.
//
// A position drawn as a slot belongs to the module that draws it: its own
// module when it has one, else the first module that stages there.
export function moduleOf(cell, name) {
    const mods = modulesOf(cell).mods;
    const m = mods.find(mod => mod.names.indexOf(name) >= 0) ||
        mods.find(mod => mod.staged.indexOf(name) >= 0);
    return m ? m.names.concat(m.staged) : [];
}

// stagingNamed is the staging nodes one claim puts to use: its inbound and
// outbound staging unless flowspec forbids the field for the claim's mode — a
// press index's inbound staging has no running leg, but the staged tooling
// changeover reads it, so it counts; a two-robot claim's outbound staging is
// named but not used (Robot 2 takes the old bin straight to the dock). The
// rule is the model's stagingFieldUsed, which the desktop's staging menu asks
// too. Both stagingUse and the unused-staging line read this, so "shared",
// "used by" and "not used by this flow" cannot disagree about a node.
function stagingNamed(c) {
    if (!c || !c.swap_mode) return [];
    const M = window.ComposerModel;
    return ['inbound_staging', 'outbound_staging']
        .filter(f => c[f] && M.stagingFieldUsed(c.swap_mode, f))
        .map(f => c[f]);
}

// stagingUse counts how many claimed positions name a staging node — a node
// two claims name is drawn in both their modules, each marked shared. A
// two-robot claim's outbound staging is not counted: its module draws no slot
// for it, so it cannot make another module's slot shared.
function stagingUse(cell) {
    const use = {};
    for (const p of ((cell && cell.positions) || [])) {
        for (const f of stagingNamed(p.claim)) use[f] = (use[f] || 0) + 1;
    }
    return use;
}

// ── drawing the modules ────────────────────────────────────────────────────
//
// drawModule renders one descriptor at its absolute origin and returns its
// height. The templates are the reference's five, with the data the view
// carries: which staging places get slots (never one that is a position of
// the cell — it is already a card), which moves get chevrons, what the card
// reads.

// The card's lines: the model's own words, from its one card-line table
// (composer-model.js CARDLINE) for every mode. No move carries a label.
function cardLinesFor(pos, sentences) {
    return ((sentences.cardLines && sentences.cardLines[pos.core_node_name]) || []).slice();
}

// WHICH ROBOT MAKES A MOVE IS THE MODEL'S, never this file's. A move that
// is a leg — the new bin up out of its staging lane, the index up into the
// press — takes that leg's robot; a move to or from the dock takes THAT
// CELL'S robot for that direction (the model's cellRobots), never the
// flow's: a flow mixing a one-robot cell with a two-robot one clears the
// first with Robot 1 and the second with Robot 2. A template says where a
// move sits; it never says who makes it.
function legRobot(sentences, pos, kind, fallback) {
    for (const L of (sentences.legs || [])) {
        if (L.kind !== kind) continue;
        if (L.from !== pos && L.to !== pos) continue;
        if (L.robot) return L.robot;
    }
    return fallback;
}

function dockRobot(sentences, pos, dir) {
    const r = (sentences.robots || {})[pos];
    if (r && r[dir]) return r[dir];
    const d = sentences.dock || {};
    return (dir === 'in' ? d.inRobot : d.outRobot) || 1;
}

function drawModule(mod, at, ctx) {
    const mx = at.x, my = at.y;
    const cx = mx + INSET, cy = my + BAR;
    const sy = cy + CARD_H + GAP;
    const findings = ctx.findings;
    const s = [];
    // A SELECTION IS THE MODULE'S: selecting any position in it outlines
    // every card and slot the module draws, and nothing else changes.
    // A position drawn only as a slot is selected with the module(s) that draw
    // it; one that has its own module is selected with that module alone.
    //
    // EXCEPT ON THE SPOTS STEP (opts.selectCard, spots v0): there an on-deck
    // card or a lane is a place of its own, and outlining its press's whole
    // module read as "the press is selected". The tapped card or slot alone
    // takes the outline (selOf).
    const sel = !!ctx.sel && (mod.names.indexOf(ctx.sel) >= 0 ||
        ((mod.staged || []).indexOf(ctx.sel) >= 0 && !ctx.carded.has(ctx.sel)));
    const selOf = n => (ctx.selectCard ? ctx.sel === n : sel);
    const tgt = n => ctx.targets.has(n);
    // A card already names its position (data-pos); the tap mark is all an
    // editable picture adds. EVERY card takes it, a dashed one included: on a
    // new part every card is dashed, and a tap is how a position joins.
    const tap = () => ctx.editable ? ' data-tap="pos"' : '';
    let h;

    if (mod.kind === 'free') {
        const pos = ctx.byName[mod.name];
        const on = !!(mod.on || (pos && pos.claim));
        s.push(bar(cx, my, CARD_W, !on));
        s.push(posCard(cx, cy, mod.name, cardLinesFor(pos, ctx.sentences),
            null, null, on, selOf(mod.name), findings[mod.name], false,
            tap(), tgt(mod.name)));
        // A POSITION THAT IS ANOTHER'S STAGING carries that trip's key route
        // under its own card: the move leg arriving from here is the trip, and
        // the leg brings its robot and its waypoints with it.
        const arrive = (ctx.sentences.legs || []).find(L =>
            L.kind === 'move' && L.from === mod.name && L.to !== mod.name && (L.keyRoute || []).length);
        let fh = cy + CARD_H;
        if (arrive) {
            s.push(oneStrip(arrive.keyRoute, cx + 22, cy + CARD_H, arrive.robot || dockRobot(ctx.sentences, arrive.to, 'in')));
            fh += stripRoom(Math.min(arrive.keyRoute.length, STRIP_SLOTS)) + 8;
        }
        return { svg: s.join(''), w: M_W, h: fh };
    }

    if (mod.kind === 'seq') {
        const a = ctx.byName[mod.a], b = ctx.byName[mod.b];
        const inR = dockRobot(ctx.sentences, mod.b, 'in'), outR = dockRobot(ctx.sentences, mod.b, 'out');
        const w = 2 * M_W + GX;
        const ax = mx + Math.floor((w - (2 * CARD_W + 16)) / 2);
        const bx = ax + CARD_W + 16;
        s.push('<path class="press" d="M' + (ax + 26) + ' ' + (my + 5) + 'H' + (bx + CARD_W - 26) + '"/>');
        s.push(posCard(ax, cy, a.core_node_name, cardLinesFor(a, ctx.sentences),
            a.claim && a.claim.payload_code, null, true, selOf(mod.a),
            findings[mod.a], false, tap()));
        s.push(posCard(bx, cy, b.core_node_name, cardLinesFor(b, ctx.sentences),
            b.claim && b.claim.payload_code, { in: inR, out: outR }, true, selOf(mod.b),
            findings[mod.b], true, tap()));
        s.push(move([bx + CARD_W / 2, cy + CARD_H + GAP], [bx + CARD_W / 2, cy + CARD_H], inR, mod.a, mod.b)); // B pulls the finished part up from A
        s.push(move([bx + CARD_W, cy + 46], [bx + CARD_W + INSET, cy + 46], outR, mod.b, 'dock'));            // it leaves B for the dock
        return { svg: s.join(''), w: w, h: cy + CARD_H + GAP };
    }

    const pos = ctx.byName[mod.name];
    const c = pos.claim;
    const part = c.payload_code || null;
    const lines = cardLinesFor(pos, ctx.sentences);

    if (mod.kind === 'index') {
        const outRobot = dockRobot(ctx.sentences, mod.name, 'out');
        const deckIn = dockRobot(ctx.sentences, mod.name, 'in');
        const indexRobot = legRobot(ctx.sentences, mod.name, 'index', outRobot);
        s.push(bar(cx, my, CARD_W));
        s.push(posCard(cx, cy, mod.name, lines, part, { in: indexRobot, out: outRobot }, true, selOf(mod.name),
            findings[mod.name], true, tap()));
        let hRun = cy + CARD_H;
        if (mod.deck) {
            const deckLines = (ctx.sentences.cardLines && ctx.sentences.cardLines[mod.deck]) || [];
            s.push(deckCard(cx, sy, mod.deck, deckLines[0] || '', deckIn, selOf(mod.deck),
                findings[mod.deck], tap()));
            s.push(move([mx, sy + DECK_H / 2], [cx, sy + DECK_H / 2], deckIn, 'dock', mod.deck));       // next bin in, behind the one on deck
            s.push(move([mx + M_W / 2, sy], [mx + M_W / 2, cy + CARD_H], indexRobot, mod.deck, mod.name)); // index up into the press
            hRun = sy + DECK_H;
        }
        // THE INBOUND STAGING OF A PRESS INDEX is where a changeover keeps the
        // next style's tooling staged. The claim names it, so the module draws
        // it — under the on-deck card (under the press card when there is no
        // deck), captioned by its field — but the running choreography has no
        // leg to it, so no chevron joins it to anything.
        if (c.inbound_staging) {
            const st = c.inbound_staging;
            const ty = mod.deck ? hRun + ROW_GAP : sy;
            s.push(slotCard(mx, ty, st, 'Inbound staging', (ctx.use[st] || 0) > 1, selOf(st),
                ctx.editable ? ' data-tap="staging" data-pos="' + esc(mod.name) + '"' : '',
                st + ' — inbound staging, used at changeover', tgt(st)));
            hRun = ty + SLOT_H;
        }
        s.push(move([cx + CARD_W, cy + 46], [mx + M_W, cy + 46], outRobot, mod.name, 'dock')); // old bin out, to the dock
        return { svg: s.join(''), w: M_W, h: hRun };
    }

    // single_robot and two_robot: the swap module — press card over its
    // staging slot row, the moves as chevrons in the gap between them. Every
    // staging place the claim parks at is a slot here, a position of the cell
    // included (see modulesOf: such a position has no card of its own unless
    // it runs its own claim). A TWO-ROBOT SWAP HAS NO OUTBOUND SLOT:
    // Robot 2 takes the old bin straight to the dock (the out-stub drawn
    // below), so an outbound staging its claim happens to name is not a place
    // this choreography parks, and a slot for it would draw a trip it never
    // makes.
    //
    // THE OLD BIN LEAVES ONE WAY. A one-robot swap's park leg takes it down
    // into the outbound slot, in that leg's robot's colour, and that is the
    // whole of its trip in this module: no out-stub to the dock beside it,
    // which would draw the one bin leaving twice, to two places. The stub is
    // for an old bin that leaves the module with no park leg drawn — a
    // two-robot swap's, or one whose park target has no slot here.
    const slots = swapSlots(c);
    const inName = slots.in, outName = slots.out;
    const outRobot = dockRobot(ctx.sentences, mod.name, 'out');
    const inRobot = legRobot(ctx.sentences, mod.name, 'move', dockRobot(ctx.sentences, mod.name, 'in'));
    const park = outName ? (ctx.sentences.legs || []).find(L =>
        L.kind === 'park' && L.from === mod.name && L.to === outName) : null;
    const parkRobot = park ? (park.robot || outRobot) : null;
    s.push(bar(cx, my, CARD_W));
    s.push(posCard(cx, cy, mod.name, lines, part, { in: inRobot, out: parkRobot || outRobot }, true, selOf(mod.name),
        findings[mod.name], true, tap()));
    if (inName) {
        s.push(slotCard(mx, sy, inName, 'Inbound staging', (ctx.use[inName] || 0) > 1, selOf(inName),
            ctx.editable ? ' data-tap="staging" data-pos="' + esc(mod.name) + '"' : '', undefined, tgt(inName)));
        s.push(move([mx + 52, sy], [mx + 52, cy + CARD_H], inRobot, inName, mod.name));      // new bin in, from the lane
    }
    if (outName) {
        s.push(slotCard(mx + SLOT_W + 12, sy, outName, 'Outbound staging',
            (ctx.use[outName] || 0) > 1, selOf(outName),
            ctx.editable ? ' data-tap="staging" data-pos="' + esc(mod.name) + '"' : '', undefined, tgt(outName)));
    }
    if (park) {
        s.push(move([mx + M_W - 52, cy + CARD_H], [mx + M_W - 52, sy], parkRobot, mod.name, outName)); // old bin down, parked
    } else {
        s.push(move([cx + CARD_W, cy + 46], [mx + M_W, cy + 46], outRobot, mod.name, 'dock'));     // old bin out, to the dock
    }
    h = sy + SLOT_H;
    if (inName && (c.key_route || []).length) {
        s.push(oneStrip(c.key_route, mx + 22, sy + SLOT_H, inRobot));
        h += stripRoom(Math.min(c.key_route.length, STRIP_SLOTS)) + 8;
    }
    return { svg: s.join(''), w: M_W, h: h };
}

// ── packing ────────────────────────────────────────────────────────────────
//
// Left to right, wrap when the row is full, each row as tall as its tallest
// module and centred in the span. Nothing is measured: a module is 244 wide
// (a sequential pair 508), so the wrap at any width is arithmetic and two
// modules can never overlap.

function packRows(built, width) {
    const span = width - 2 * FIT;
    const rows = [];
    let cur = [], curW = 0;
    for (const b of built) {
        let need = b.w + (cur.length ? GX : 0);
        if (cur.length && curW + need > span) {
            rows.push(cur);
            cur = []; curW = 0;
            need = b.w;
        }
        cur.push(b);
        curW += need;
    }
    if (cur.length) rows.push(cur);
    return rows;
}

// truncList joins a note's prefix and its names within a character budget,
// counting what does not fit rather than dropping it: "…, PLN_05 +2".
function truncList(prefix, names, budget) {
    const out = [];
    let used = prefix.length;
    for (let i = 0; i < names.length; i++) {
        const add = names[i].length + (out.length ? 2 : 0);
        const rest = names.length - i - 1;
        const tail = rest ? String(' +' + rest).length : 0;
        if (used + add + tail > budget && out.length) {
            return prefix + out.join(', ') + ' +' + (names.length - i);
        }
        out.push(names[i]);
        used += add;
    }
    return prefix + out.join(', ');
}

// dockLine is one dock note, cut to the budget: the side's label, then each
// robot's group — `Robot 2 ← PLN_01, PLN_04` — with its list cut to what is
// left ("+N"). The words are the model's (dockNotes' groups); this only
// truncates, escapes, and wraps each robot's name in its colour, the way a
// card line does. Returns markup.
function dockLine(label, groups, budget) {
    let out = label, used = label.length;
    for (const g of (groups || [])) {
        const seg = truncList(' · ' + g.head, g.targets, Math.max(budget - used, g.head.length + 3));
        out += seg;
        used += seg.length;
    }
    let body = esc(out);
    for (let r = 1; r <= 2; r++) {
        body = body.split('Robot ' + r).join('<tspan class="rw r' + r + '">Robot ' + r + '</tspan>');
    }
    return body;
}

// ── the model bridge ───────────────────────────────────────────────────────
//
// THE SENTENCES COME FROM THE MODEL. ALWAYS. This file computes no sentence:
// the read-only callers build a model state FROM THE VIEW and ask the model
// (sentencesFromView); the composer passes its live state's sentences through
// opts. The dock's robots travel with the notes — dockNotes derives them from
// the same facts its words come from, and the renderer only colours by them.

//
// relation says the sentences are for the relation picture (relationPlacement):
// a card nothing names has no caption there.
export function sentencesFromModel(state, relation) {
    const M = typeof window !== 'undefined' ? window.ComposerModel : null;
    if (!M || !state) return null;
    const cardLines = {};
    for (const p of state.positions) cardLines[p.core_node_name] = M.cardLines(state, p.core_node_name, relation);
    const robots = {};
    for (const p of state.positions) {
        const r = M.cellRobots(state, p.core_node_name);
        if (r) robots[p.core_node_name] = r;
    }
    const d = M.dockNotes(state);
    return {
        cardLines: cardLines,
        legs: M.legs(state),
        robots: robots,
        dock: {
            srcs: [d.in.group], dsts: [d.out.group],
            inNote: d.in.note, outNote: d.out.note,
            inLabel: d.in.label, outLabel: d.out.label,
            inGroups: d.in.groups, outGroups: d.out.groups,
            inMembers: d.in.members.join(', '), outMembers: d.out.members.join(', '),
            inTargets: d.in.targets, outTargets: d.out.targets,
            inRobot: d.in.robot, outRobot: d.out.robot,
        },
    };
}

export function sentencesFromView(view, relation) {
    const M = typeof window !== 'undefined' ? window.ComposerModel : null;
    const cell = (view && view.cell) || null;
    if (!M || !cell || !cell.positions) return null;
    const state = M.init({
        positions: cell.positions.map(p => ({
            core_node_name: p.core_node_name, kind: p.kind, sequence: p.sequence,
        })),
        claims: cell.positions.filter(p => p.claim).map(p => Object.assign(
            { core_node_name: p.core_node_name }, p.claim)),
        groups: cell.groups || {},
        routing: [],
    });
    return sentencesFromModel(state, relation);
}

// pictureRows is which row of the PRESS each position stands in: 'front' for
// the row nearest the aisle, 'back' for the far one, '' for a middle row or a
// cell with one row. Every caption beside the picture that says "front" or
// "back" has to come from here. opts takes renderFlowPicture's `relation` and
// `pairs`, so the words follow the picture the caller draws.
//
// A CELL THE RELATION PICTURE DRAWS AND PLACES HAS NO ROW WORDS. Its grid's
// rows are the press's — the layout never wraps, and its rows come from the
// plant map (relationLayout) — but which of them faces the aisle, the
// operator's side, is not in x and y: the map has no aisle, and a press can
// face any way. A word there would be a guess. Everywhere else, worldRows.
export function pictureRows(cell, opts) {
    if (!relationPlacement(cell, opts)) return worldRows(cell);
    const out = {};
    for (const p of (cell.positions || [])) out[p.core_node_name] = '';
    return out;
}

// relationPlacement is the relation layout of a cell the relation picture
// draws and places, or null where it does not: the module picture, and the
// fallback for a cell the map cannot place.
export function relationPlacement(cell, opts) {
    if (!drawnByRelation(cell, opts)) return null;
    const lay = window.ComposerModel.relationLayout(cell, { pairs: opts && opts.pairs });
    return lay.fallback || !lay.ncols ? null : lay;
}

// worldRows is the row words of a cell the relation picture does not place —
// the module picture's, whose order reads them too.
//
// FROM THE COORDINATES, NEVER FROM THE GRID. The module grid wraps at the
// frame's width, so a grid row is a fact about the screen, not the press: the
// same cell is one row at 1280 and three at 640. The rows are the positions'
// world Y — the line side is the higher Y, the top of the projection
// (makeProjector draws [x, −y]) — clustered within ROW_TOL metres of each
// row's first.
//
// WITHOUT COORDINATES, THE KIND. A cell with no geometry, or with a position
// off the map, has no world Y to read; the schematic has always stood the
// press in two rows by CellPosition.Kind — every position not Kind "back" in
// the front row, the Kind "back" ones behind it — and the words keep that
// meaning. A cell whose positions are all one kind is one row and gets no
// word.
//
// KIND IS NOT THE ROW WHEN THERE ARE COORDINATES, and the two disagree on a
// real press. Kind answers "is this a partner slot for any style this process
// runs" — at Hopkinsville PLN_01 and PLN_04 are Kind "front" and stand in the
// back row with their on-deck partners. It is read only where nothing better
// exists.
const ROW_TOL = 0.5;

function worldRows(cell) {
    const positions = (cell && cell.positions) || [];
    const out = {};
    positions.forEach(p => { out[p.core_node_name] = ''; });
    if (!positions.length) return out;
    if (!window.ComposerModel.hasCoords(cell)) {
        const back = positions.filter(p => p.kind === 'back').length;
        if (back === 0 || back === positions.length) return out;
        positions.forEach(p => { out[p.core_node_name] = p.kind === 'back' ? 'back' : 'front'; });
        return out;
    }
    const byY = positions.slice().sort((a, b) => b.y - a.y);
    const rows = [];
    for (const p of byY) {
        const last = rows[rows.length - 1];
        if (last && last.y - p.y <= ROW_TOL) { last.names.push(p.core_node_name); continue; }
        rows.push({ y: p.y, names: [p.core_node_name] });
    }
    if (rows.length < 2) return out;
    rows[0].names.forEach(n => { out[n] = 'front'; });
    rows[rows.length - 1].names.forEach(n => { out[n] = 'back'; });
    return out;
}

// ── the picture by relation ────────────────────────────────────────────────
//
// relationScope is the gate (SYNTH round 2 row 17): a cell with a claim in
// one of the four choreographies the model's MODES names is a changeover
// cell, drawn by relation on every surface that draws it — the station's flow
// panel, the HMI's recipe steps, the desktop. A cell with none (a market, a
// loader, an unloader, a legacy manual_swap or simple row) keeps the module
// picture. One predicate, so "recipe surfaces only" — the owner's
// alternative, decided at the real-cell renders — is one caller's
// `relation: false`.
export function relationScope(cell) {
    const M = typeof window !== 'undefined' ? window.ComposerModel : null;
    const modes = M ? M.modeLabels() : {};
    return ((cell && cell.positions) || []).some(p => !!(p.claim && modes[p.claim.swap_mode]));
}

// drawnByRelation is renderFlowPicture's choice: `relation` when the caller
// says, the gate when it does not.
function drawnByRelation(cell, opts) {
    const r = opts && opts.relation;
    return r === true || (r !== false && relationScope(cell));
}

// THE RELATION PICTURE'S NUMBERS. A card is the module card's width with a
// chevron row more height (the lead's D13: 112 and 76 here, 92 and 56 on the
// module picture, so a cell the gate leaves alone draws as it did). Columns
// are one card and one gap: six fit the HMI's 1240 (6 x 188 + 5 x 22 = 1238).
const R_CARD_H = 112;      // name, two lines, the chevron row, the chip
const R_DECK_H = 76;       // an on-deck or third card: name, one line, the chevron row
const R_GX = 22, R_GY = 22;
const R_PITCH = CARD_W + R_GX;
const R_ROW = BAR + R_CARD_H + R_GY;
const R_SLOT_H = 58;       // a band lane: name, caption, the chevron row
const R_BAND_GAP = 28;     // the grid's last row to the band
const R_CHEV_Y = { card: 76, deck: 58, slot: 48 };   // the chevron row's centre line
const R_CHIP_Y = 86;       // the part chip, the findings pill, "move here"

// chevPair is one move drawn on the card that makes it (SYNTH row 19): a pair
// of slender chevrons centred on (cx, cy), pointing the way the bin goes —
// r, l, d, or u (up, in from the aisle) — in the robot's colour. data-of names
// the claim whose choreography it is and data-robot / data-dir what it says,
// so a reader of the markup need not work a move out from a rotation. The
// pair is 14.4 along its way and 10 across.
const CHEV_DIR = { r: [1, 0, 0], l: [-1, 0, 180], d: [0, 1, 90], u: [0, -1, -90] };
function chevPair(cx, cy, dir, robot, of) {
    const d = CHEV_DIR[dir];
    let s = '<g class="cv" data-of="' + esc(of) + '" data-robot="' + robot + '" data-dir="' + dir + '">';
    for (const o of [-1.2, 7.2]) {
        s += '<path class="sc r' + robot + '" d="M-6 -5L0 0L-6 5" transform="translate(' +
            (cx + d[0] * o).toFixed(1) + ' ' + (cy + d[1] * o).toFixed(1) + ') rotate(' + d[2] + ')"/>';
    }
    return s + '</g>';
}

// chevRow centres a card's moves under its words, side by side, 14 apart.
// Nothing is drawn in the gaps between cards.
function chevRow(list, w, cy) {
    if (!list || !list.length) return '';
    const wide = list.map(v => (v.dir === 'r' || v.dir === 'l' ? 14.4 : 10));
    let x = (w - (wide.reduce((a, b) => a + b, 0) + 14 * (list.length - 1))) / 2;
    let s = '';
    list.forEach((v, i) => { s += chevPair(x + wide[i] / 2, cy, v.dir, v.robot, v.of); x += wide[i] + 14; });
    return s;
}

// groupTag is a recipe group's letter on its card's top-left corner, over the
// border (the mock's .gtag), filled with the group's identity token — the
// one place --group-1 / --group-2 are drawn.
function groupTag(tag) {
    if (!tag || !tag.letter) return '';
    const w = Math.max(18, 8 + 7 * String(tag.letter).length);
    return '<g class="gtag g' + (Number(tag.group) || 1) + '"><rect x="4" y="-9" width="' + w + '" height="18" rx="5"/>' +
        '<text x="' + (4 + w / 2) + '" y="4" text-anchor="middle">' + esc(tag.letter) + '</text></g>';
}

// relCard is one position in its grid cell: the 10-05 card's content — the
// name, the model's lines, the in/out marks, the part chip or findings pill
// or "move here" — and a chevron row under the words. A feeder (an on-deck or
// third position) is the shorter card, as on the module picture. The bar is
// the press: over a spot, never over a feeder or a lane.
function relCard(x, y, name, lines, o) {
    const feeder = !!o.feeder;
    const h = feeder ? R_DECK_H : R_CARD_H;
    const marks = o.io || [];
    const nmPx = CARD_W - 28 - (marks.length === 2 ? 80 : marks.length === 1 ? 44 : 0);
    const cls = 'node ' + (o.on ? 'on' : 'off') + (feeder ? ' deck' : '') + (o.faint ? ' faint' : '') +
        (o.sel ? ' sel' : '') + (o.bad ? ' bad' : '') + (o.tgt ? ' tgt' : '');
    // THE BAR IS THE CARD'S NEIGHBOUR, NOT ITS CONTENT: drawn before the card,
    // in picture coordinates, as the module picture's bar() draws it, so
    // everything inside a card's group lies inside its box. It fades with its
    // card (.press.faint), which it did for free while it sat inside the group.
    let s = o.bar ? '<path class="press' + (o.on ? '' : ' dim') + (o.faint ? ' faint' : '') + '" d="M' + (x + 26) + ' ' + (y - 7) +
        'H' + (x + CARD_W - 26) + '"/>' : '';
    s += '<g class="' + cls + '" data-pos="' + esc(name) + '"' + (o.tap || '') +
        ' transform="translate(' + x + ',' + y + ')">';
    s += nameTip(name, nmPx, 15);
    s += '<rect class="box" width="' + CARD_W + '" height="' + h + '" rx="12"/>';
    s += txt('nm', 14, feeder ? 24 : 26, name, nmPx, 15);
    (lines || []).slice(0, feeder ? 1 : 2).forEach((l, i) => {
        s += txt('ln', 14, (feeder ? 42 : 43) + i * 14, l, CARD_W - 28, 12, 500);
    });
    if (!feeder) {
        const chip = (bg, lbl, text) => '<rect class="' + bg + '" x="12" y="' + R_CHIP_Y + '" width="' + (CARD_W - 24) +
            '" height="20" rx="5"/>' + txt(lbl, CARD_W / 2, R_CHIP_Y + 14, text, CARD_W - 40, 11, 600, 'middle');
        if (o.bad) s += chip('needbg', 'needlbl', o.bad);
        else if (o.part) s += chip('partbg', 'partlbl', o.part);
        else if (o.tgt) s += chip('tgtbg', 'tgtlbl', 'move here');
    }
    if (marks.length === 2) s += io('in', marks[0], CARD_W - 80, 10) + io('out', marks[1], CARD_W - 46, 10);
    else if (marks.length === 1) s += io('in', marks[0], CARD_W - 46, 10);
    s += chevRow(o.chev, CARD_W, feeder ? R_CHEV_Y.deck : R_CHEV_Y.card);
    s += groupTag(o.tag);
    return s + '</g>';
}

// relSlot is a lane in the band: the module slot's name and caption (a shared
// lane keeps the caption's first word and the "shared" tag), a chevron row
// under them. An offered lane is dashed and says it is not used.
function relSlot(x, y, name, caption, o) {
    const cls = 'stage ' + (o.offered ? 'off' : 'on') + (o.faint ? ' faint' : '') + (o.sel ? ' sel' : '') + (o.tgt ? ' tgt' : '');
    let s = '<g class="' + cls + '" data-staging="' + esc(name) + '"' + (o.tap || '') +
        ' transform="translate(' + x + ',' + y + ')">';
    s += nameTip(name, SLOT_W - 24, 13);
    s += '<rect class="box" width="' + SLOT_W + '" height="' + R_SLOT_H + '" rx="10"/>';
    s += txt('nm', 12, 19, name, SLOT_W - 24, 13);
    if (o.tgt) {
        s += txt('tgtlbl', 12, 35, 'move here', SLOT_W - 22, 11, 600);
    } else if (o.shared) {
        s += txt('ln', 12, 35, caption.split(' ')[0], TAG_X - 12, 11, 500);
        s += '<rect class="tagbg" x="' + TAG_X + '" y="24.5" width="46" height="14" rx="7"/>' +
            '<text class="tag" x="' + (TAG_X + 23) + '" y="35" text-anchor="middle">shared</text>';
    } else {
        s += txt('ln', 12, 35, caption, SLOT_W - 22, 11, 500);
    }
    s += chevRow(o.chev, SLOT_W, R_CHEV_Y.slot);
    return s + '</g>';
}

// drawRelation is the relation picture of one cell, laid out by lay
// (relationLayout). It returns the cards, the band and their height and
// width; renderFlowPicture puts the legend over it and the dock under it.
//
// THE CHOREOGRAPHY IS THE CLAIMS', AND ITS ROBOTS THE MODEL'S. Which cards
// carry which moves (SYNTH row 19): a 2-Robot Index's on-deck card Robot 1 up
// and Robot 2 toward its press spot, the press card one Robot 2 from the
// deck's side, a third Robot 1 up and Robot 2 toward the deck; a 2-Robot
// Swap's lane Robot 1 up and its spot Robot 1 from the lane's side; a 1-Robot
// Swap's in-lane Robot 1 up, its spot from the in-lane's side and its
// out-lane from the spot's; a Sequential A/B's B card Robot 1 up and A
// nothing. The direction is the grid's — target cell less source cell,
// horizontal when its columns are at least its rows (a tie is horizontal) —
// and the robot is legRobot's or dockRobot's, never a template's.
//
// A DECK MAY FEED MORE THAN ONE PRESS SPOT. A recipe group's on-deck spot
// serves every spot of its group, and the desktop's draft draws the group
// with a claim on each; a style can pair one deck twice too. The card's shape
// is the first claim's (`feeder`), but every claim indexing from the deck
// draws its own move, on its own card and on the deck's.
function drawRelation(cell, sentences, lay, opts, width) {
    const positions = cell.positions || [];
    const byName = {};
    positions.forEach(p => { byName[p.core_node_name] = p; });
    const claimOf = n => (byName[n] && byName[n].claim && byName[n].claim.swap_mode ? byName[n].claim : null);
    const modes = window.ComposerModel.modeLabels();
    const heads = positions.map(p => p.core_node_name).filter(claimOf);
    const stagingBy = {};
    for (const st of (cell.staging || [])) if (st && st.core_node_name) stagingBy[st.core_node_name] = st;
    const bandAt = {};
    for (const b of lay.band) bandAt[b.node] = b;
    const at = n => lay.cells[n] || (bandAt[n] ? { c: bandAt[n].c, r: lay.nrows } : null);

    // What each claim names: its feeders (a press index's on-deck and third
    // positions that run no claim of their own), its lanes, and the pairs of
    // a Sequential A/B (built as the module picture builds them).
    const named = new Set();
    const feeder = {};
    const laneUse = {};
    const seqB = {};
    const seqUsed = new Set();
    for (const h of heads) {
        const c = claimOf(h);
        named.add(h);
        for (const f of ['paired_core_node', 'second_paired_core_node', 'inbound_staging', 'outbound_staging']) {
            if (c[f]) named.add(c[f]);
        }
        if (c.swap_mode === 'two_robot_press_index') {
            for (const n of [c.paired_core_node, c.second_paired_core_node]) {
                if (n && byName[n] && !claimOf(n) && !feeder[n]) feeder[n] = h;
            }
        }
        for (const f of ['inbound_staging', 'outbound_staging']) {
            if (c[f]) (laneUse[c[f]] = laneUse[c[f]] || []).push(f);
        }
        if (c.swap_mode === 'sequential') {
            const q = c.paired_core_node, qc = claimOf(q);
            if (qc && qc.swap_mode === 'sequential' && !seqUsed.has(h) && !seqUsed.has(q)) {
                seqUsed.add(h); seqUsed.add(q); seqB[q] = h;
            }
        }
    }

    // ── the chevrons ──
    const show = opts.chevrons ? new Set(opts.chevrons) : new Set(heads);
    const chev = {};
    const dirOf = (a, b) => {
        const A = at(a), B = at(b);
        if (!A || !B) return '';
        const dc = B.c - A.c, dr = B.r - A.r;
        if (!dc && !dr) return '';
        return Math.abs(dc) >= Math.abs(dr) ? (dc > 0 ? 'r' : 'l') : (dr > 0 ? 'd' : 'u');
    };
    const add = (n, robot, dir, of) => {
        if (!n || !dir || !at(n)) return;
        const l = (chev[n] = chev[n] || []);
        if (!l.some(v => v.robot === robot && v.dir === dir)) l.push({ robot: robot, dir: dir, of: of });
    };
    for (const h of heads) {
        if (!show.has(h) && !(seqB[h] && show.has(seqB[h]))) continue;
        const c = claimOf(h);
        const inR = dockRobot(sentences, h, 'in');
        if (c.swap_mode === 'two_robot_press_index') {
            const idx = legRobot(sentences, h, 'index', dockRobot(sentences, h, 'out'));
            const d = feeder[c.paired_core_node] ? c.paired_core_node : '';
            const t = feeder[c.second_paired_core_node] ? c.second_paired_core_node : '';
            if (d) { add(d, inR, 'u', h); add(d, idx, dirOf(d, h), h); add(h, idx, dirOf(d, h), h); }
            if (t) { add(t, inR, 'u', h); if (d) add(t, idx, dirOf(t, d), h); }
        } else if (c.swap_mode === 'two_robot' || c.swap_mode === 'single_robot') {
            const lin = c.inbound_staging;
            if (lin && at(lin)) {
                add(lin, inR, 'u', h);
                add(h, legRobot(sentences, h, 'move', inR), dirOf(lin, h), h);
            }
            const lout = c.swap_mode === 'single_robot' ? c.outbound_staging : '';
            if (lout && at(lout)) {
                const park = (sentences.legs || []).find(L => L.kind === 'park' && L.from === h && L.to === lout);
                add(lout, (park && park.robot) || dockRobot(sentences, h, 'out'), dirOf(h, lout), h);
            }
        } else if (c.swap_mode === 'sequential' && seqB[h]) {
            add(h, inR, 'u', h);
        }
    }

    // ── what is drawn full ──
    const active = opts.active ? new Set(opts.active) : named;
    const targets = new Set(opts.targets || []);
    const findings = opts.findings || {};
    const members = h => {
        const c = claimOf(h);
        const out = [h];
        if (c.swap_mode === 'two_robot_press_index') {
            if (feeder[c.paired_core_node] === h) out.push(c.paired_core_node);
            if (c.inbound_staging) out.push(c.inbound_staging);
        } else if (c.swap_mode === 'two_robot' || c.swap_mode === 'single_robot') {
            if (c.inbound_staging) out.push(c.inbound_staging);
            if (c.swap_mode === 'single_robot' && c.outbound_staging) out.push(c.outbound_staging);
        } else if (seqB[h]) out.push(seqB[h]);
        else for (const b of Object.keys(seqB)) if (seqB[b] === h) out.push(b);
        return out;
    };
    // A SELECTION OUTLINES WHAT ITS MODULE WOULD: the claim and everything it
    // names, or — on the Spots step (selectCard) — the tapped card alone.
    const sel = opts.selected || null;
    let selSet = new Set();
    if (sel && opts.selectCard) selSet = new Set([sel]);
    else if (sel) {
        const own = claimOf(sel) ? [sel] : heads.filter(h => members(h).indexOf(sel) >= 0);
        selSet = new Set(own.length ? [].concat(...own.map(members)) : [sel]);
    }
    const faint = n => !active.has(n) && !targets.has(n) && !selSet.has(n);
    const tapPos = opts.editable ? ' data-tap="pos"' : '';

    // ── where the grid stands ──
    const gridW = lay.ncols * CARD_W + (lay.ncols - 1) * R_GX;
    const bandRight = lay.band.reduce((m, b) => Math.max(m, b.c * R_PITCH + CARD_W / 2 + SLOT_W / 2), 0);
    const contentW = Math.max(gridW, bandRight);
    const span = width - 2 * FIT;
    let gx0 = FIT, pictureW = width;
    if (contentW <= span) {
        gx0 = FIT + Math.floor((span - gridW) / 2);
        if (gx0 + bandRight > width - FIT) gx0 = width - FIT - Math.ceil(bandRight);
    } else {
        pictureW = Math.ceil(contentW) + 2 * FIT;
    }
    const gy0 = FIT + 22;                               // under the legend's line

    let s = '';
    for (const p of positions) {
        const n = p.core_node_name;
        const g = lay.cells[n];
        if (!g) continue;
        const c = claimOf(n);
        const fd = !c && feeder[n];
        const lane = !c && !fd && named.has(n);
        const x = gx0 + g.c * R_PITCH;
        const y = gy0 + g.r * R_ROW + BAR + (fd ? (R_CARD_H - R_DECK_H) / 2 : 0);
        let marks = [];
        if (c && modes[c.swap_mode]) {
            const outR = dockRobot(sentences, n, 'out');
            if (c.swap_mode === 'two_robot_press_index') {
                marks = [legRobot(sentences, n, 'index', outR), outR];
            } else if (c.swap_mode === 'sequential') {
                marks = seqB[n] ? [dockRobot(sentences, n, 'in'), outR] : [];
            } else {
                const park = c.swap_mode === 'single_robot' && c.outbound_staging ? (sentences.legs || []).find(L =>
                    L.kind === 'park' && L.from === n && L.to === c.outbound_staging) : null;
                marks = [legRobot(sentences, n, 'move', dockRobot(sentences, n, 'in')), (park && (park.robot || outR)) || outR];
            }
        } else if (fd) {
            marks = [dockRobot(sentences, fd, 'in')];
        }
        s += relCard(x, y, n, cardLinesFor(p, sentences), {
            feeder: !!fd, on: !!c || named.has(n), bar: !fd && !lane, io: marks,
            part: c && modes[c.swap_mode] ? (c.payload_code || null) : null,
            faint: faint(n), sel: selSet.has(n), bad: findings[n], tgt: targets.has(n),
            tap: tapPos, chev: chev[n], tag: opts.tags && opts.tags[n],
        });
    }
    let bottom = gy0 + lay.nrows * R_ROW - R_GY;

    // ── the band ──
    if (lay.band.length) {
        const by = bottom + R_BAND_GAP;
        let strip = 0;
        for (const b of lay.band) {
            const st = stagingBy[b.node] || {};
            const serves = (st.serves || []).filter(n => lay.cells[n]);
            const use = laneUse[b.node] || [];
            const field = use.indexOf('inbound_staging') >= 0 ? 'inbound_staging'
                : use.indexOf('outbound_staging') >= 0 ? 'outbound_staging' : (st.field || '');
            const caption = b.offered ? 'not used'
                : field === 'outbound_staging' ? 'Outbound staging' : field === 'inbound_staging' ? 'Inbound staging' : 'Staging';
            const x = Math.round(gx0 + b.c * R_PITCH + CARD_W / 2 - SLOT_W / 2);
            s += relSlot(x, by, b.node, caption, {
                offered: b.offered, shared: serves.length > 1,
                faint: b.offered ? !targets.has(b.node) && !selSet.has(b.node) : faint(b.node),
                sel: selSet.has(b.node), tgt: targets.has(b.node), chev: chev[b.node],
                tap: opts.editable && serves.length ? ' data-tap="staging" data-pos="' + esc(serves[0]) + '"' : '',
            });
            // THE ROUTE STRIP hangs under the lane its trip arrives at, as on
            // the module picture: the first claim that stages here and chose a
            // route. (A lane that is a position is a card in the grid, where a
            // strip would run into the row below; it is not drawn there.)
            const rider = serves.map(n => claimOf(n)).find(c => c && c.inbound_staging === b.node && (c.key_route || []).length);
            if (rider) {
                const h = serves.find(n => claimOf(n) === rider);
                s += oneStrip(rider.key_route, x + 22, by + R_SLOT_H,
                    legRobot(sentences, h, 'move', dockRobot(sentences, h, 'in')));
                strip = Math.max(strip, stripRoom(Math.min(rider.key_route.length, STRIP_SLOTS)) + 8);
            }
        }
        bottom = by + R_SLOT_H + strip;
    }
    return { svg: s, bottom: bottom, width: pictureW };
}

// ── the dock and the legend, both pictures' ────────────────────────────────

// dockBand draws the dock along the frame's width with its rule at dockY.
//
// It names a robot only where that robot makes the trip — the notes and
// their robots come from the model (dockNotes), which derives both from
// the same choreography facts.
function dockBand(sentences, width, dockY, editable) {
    const span = width - 2 * FIT;
    const half = Math.floor(span / 2);
    const budget = Math.floor((half - 80) / 5.9);
    const din = dockLine(sentences.dock.inLabel || sentences.dock.inNote, sentences.dock.inGroups, budget);
    const dout = dockLine(sentences.dock.outLabel || sentences.dock.outNote, sentences.dock.outGroups, budget);
    // ONE MARK, ONE ROBOT. A side two robots use (a mixed flow's OUT) has no
    // one robot's colour to take — colour is the robot, and a teal mark would
    // say Robot 1 makes Robot 2's trips — so its mark stays the muted default
    // and the note's words carry each robot in its own colour.
    const rcls = r => r ? ' r' + r : '';
    const tapIn = editable ? ' data-tap="dock" data-dock="in"' : '';
    const tapOut = editable ? ' data-tap="dock" data-dock="out"' : '';
    return '<g class="dock"><line x1="' + FIT + '" y1="' + dockY + '" x2="' + (width - FIT) + '" y2="' + dockY + '"/>' +
        '<g class="half"' + tapIn + ' transform="translate(' + FIT + ',' + dockY + ')">' +
        '<g class="io' + rcls(sentences.dock.inRobot) + '" transform="translate(16,26)">' + IN + '</g>' +
        '<text class="k" x="36" y="36">IN</text>' +
        '<text class="v" x="70" y="30">' + esc(sentences.dock.srcs.join(' · ') || '—') + '</text>' +
        '<text class="s' + rcls(sentences.dock.inRobot) + '" x="70" y="48">' + din + '</text>' +
        '<text class="s" x="70" y="64">' + esc(truncList('', (sentences.dock.inMembers || '').split(', ').filter(Boolean), budget)) + '</text></g>' +
        '<g class="half"' + tapOut + ' transform="translate(' + (FIT + half + 10) + ',' + dockY + ')">' +
        '<g class="io' + rcls(sentences.dock.outRobot) + '" transform="translate(16,26)">' + OUT + '</g>' +
        '<text class="k" x="36" y="36">OUT</text>' +
        '<text class="v" x="80" y="30">' + esc(sentences.dock.dsts.join(' · ') || '—') + '</text>' +
        '<text class="s' + rcls(sentences.dock.outRobot) + '" x="80" y="48">' + dout + '</text>' +
        '<text class="s" x="80" y="64">' + esc(truncList('', (sentences.dock.outMembers || '').split(', ').filter(Boolean), budget)) + '</text></g></g>';
}

// legendOf draws the robot legend in the frame's top right corner.
//
// Only the robots the flow uses: the dock's robots plus every robot a leg
// names. A 1-robot cell lists Robot 1 and nothing else — Robot 2 is not
// in this flow, and a legend listing it would be a colour with no referent.
// A cell with no flow lists no robot, for the same reason.
function legendOf(sentences, width) {
    const robots = new Set();
    for (const g of [...(sentences.dock.inGroups || []), ...(sentences.dock.outGroups || [])]) {
        if (g.robot) robots.add(g.robot);
    }
    for (const L of (sentences.legs || [])) robots.add(L.robot === 2 ? 2 : 1);
    let lg = '', lx = width - FIT;
    for (const r of [...robots].sort((a, b) => b - a)) {
        lx -= 76;
        lg += '<g class="lg" transform="translate(' + lx + ',' + (FIT + 4) + ')">' +
            '<path class="sc r' + r + '" d="M2 -4.2L7 0L2 4.2"/>' +
            '<path class="sc r' + r + '" d="M9.2 -4.2L14.2 0L9.2 4.2"/>' +
            '<text class="lgt" x="24" y="4">Robot ' + r + '</text></g>';
    }
    return { svg: lg, w: width - FIT - lx };
}

// ── render ─────────────────────────────────────────────────────────────────

// renderFlowPicture returns the SVG inner markup for one station view, and
// reports the picture's own size by writing opts.height and opts.width (see
// the header).
//
// opts, all optional:
//   relation   — true: draw by relation (a recipe surface asks for it);
//                false: the module picture whatever the cell; absent: the
//                scope gate (relationScope) decides.
//   selected   — the selected position. On the module picture its module
//                (every card and slot in it) takes the accent outline; on the
//                relation picture its claim's cards do. Nothing dims for it.
//   selectCard — the selection outlines the selected card or slot alone, not
//                its module (the Spots step, where an on-deck card and a lane
//                are places of their own).
//   targets    — [node] the selection may move to: each takes a dashed
//                accent outline and "move here", and is never faded.
//   findings   — {node: shortText}; the card outlines alarm and the red pill
//                takes the part chip's slot.
//   sentences  — {cardLines, legs, dock} from composer-model. The COMPOSER
//                passes its live state's, so the copy an operator is editing
//                is the copy they read; every other caller leaves it out and
//                this derives the same thing from the view through the same
//                model (sentencesFromView). Sentences passed for a relation
//                picture say so (relationPlacement), as these do.
//   editable   — marks the cards, slots and dock halves tappable (data-tap)
//                and, on the module picture, draws the unused-staging chips
//                line, which is a desktop affordance: the station screen
//                leaves it out.
//   frame      — {w}: the frame the picture is drawn into. The module picture
//                wraps at it; the relation picture centres in it, or is wider
//                when its grid is, and the dock and legend span the frame.
//                Absent = the station's 1280.
//   usedOnly   — the module picture leaves out an idle position (see
//                modulesOf). The relation picture draws every position and
//                fades what is not in play instead.
// The relation picture's own (each ignored by the module picture):
//   active     — [node] drawn full; every other card and band lane takes
//                .faint. Absent: every node a claim on the picture names.
//   tags       — {node: {letter, group}}: a recipe group's L/R tag on that
//                card, filled with --group-1 / --group-2.
//   chevrons   — [claim head] whose choreography is drawn as chevrons.
//                Absent: every claim's; [] draws none.
//   pairs      — {deck: [press, ...]} for the layout's deck rule, held fixed
//                by the caller so the grid does not re-flow while the
//                operator moves places (the lead's D11). Absent: the claims'.
export function renderFlowPicture(view, opts) {
    opts = opts || {};
    const cell = (view && view.cell) || { positions: [] };
    const lay = drawnByRelation(cell, opts) ? window.ComposerModel.relationLayout(cell, { pairs: opts.pairs }) : null;
    const placed = lay && !lay.fallback && lay.ncols ? lay : null;
    const sentences = opts.sentences || sentencesFromView(view, !!placed) || {
        cardLines: {}, legs: [], dock: {
            srcs: [], dsts: [], inNote: 'Inbound source', outNote: 'Outbound destination',
            inLabel: 'Inbound source', outLabel: 'Outbound destination', inGroups: [], outGroups: [],
            inMembers: '', outMembers: '', inTargets: [], outTargets: [],
            inRobot: 1, outRobot: 1,
        },
    };
    const width = (opts.frame && opts.frame.w) || STATION_FRAME.w;
    opts.width = width;

    // THE RELATION PICTURE, where it is drawn and the map can place it. A
    // changeover cell it cannot place (a position off the map, two on one
    // point) is drawn as modules below, with the layout's sentence in the
    // margin (the lead's D12).
    if (placed) {
        const r = drawRelation(cell, sentences, placed, opts, width);
        const dockY = r.bottom + 26;
        opts.height = dockY + DOCK_BAND;
        opts.width = r.width;
        return legendOf(sentences, width).svg + r.svg + dockBand(sentences, width, dockY, !!opts.editable);
    }
    const fallback = (lay && lay.fallback) || '';

    const sel = opts.selected || null;
    const findings = opts.findings || {};
    const { mods, byName } = modulesOf(cell, !!opts.usedOnly);
    if (!mods.length) {
        const g = { x: width / 2 };
        return '<text class="mlbl" x="' + g.x + '" y="246" text-anchor="middle">No positions on this station yet</text>';
    }

    // Which staging places are drawn as slots, so the rest — lanes in the
    // routing set this flow does not use — can be counted on the chips line.
    const positionNames = new Set((cell.positions || []).map(p => p.core_node_name));
    const use = stagingUse(cell);
    const carded = new Set();
    for (const md of mods) for (const n of md.names) carded.add(n);
    const ctx = { byName, sentences, sel, findings, editable: !!opts.editable, use, carded,
        selectCard: !!opts.selectCard, targets: new Set(opts.targets || []) };

    // Measure pass: every module at the origin, for its width and height.
    // Draw pass: the same module at its packed place. Coordinates are
    // absolute, so the two passes differ only in the origin they are given.
    const built = mods.map(m => {
        const d = drawModule(m, { x: 0, y: 0 }, ctx);
        return { mod: m, w: d.w, h: d.h, at0: d.svg };
    });
    const rows = packRows(built, width);
    const span = width - 2 * FIT;
    let s = '';
    let y = FIT + 22;                                   // the legend's line
    for (const row of rows) {
        const rw = row.reduce((a, b) => a + b.w, 0) + GX * (row.length - 1);
        let x = FIT + Math.floor((span - rw) / 2);
        for (const b of row) {
            s += '<g class="module" data-w="' + b.w + '" data-h="' + b.h + '">' +
                drawModule(b.mod, { x: x, y: y }, ctx).svg + '</g>';
            x += b.w + GX;
        }
        y += Math.max(...row.map(b => b.h)) + GY;
    }
    y -= GY;

    // ── the unused-staging chips line (desktop only, owner pick 3) ──────────
    //
    // Staging lanes the running flow does not use: not a position, not a slot
    // a module drew, and not named by any claim (stagingUse — a press index's
    // inbound staging has no running leg but is used all the same). Dashed chips on
    // one labelled line under the grid, wrapping to the frame's width like
    // the modules do.
    if (opts.editable) {
        // The slots the modules drew, read off the measure pass's markup.
        const drawn = new Set();
        for (const b of built) {
            for (const mm of b.at0.matchAll(/data-staging="([^"]+)"/g)) drawn.add(mm[1]);
        }
        const chips = (cell.staging || [])
            .map(st => st.core_node_name)
            .filter(n => !positionNames.has(n) && !drawn.has(n) && !use[n]);
        if (chips.length) {
            y += 22;
            s += '<text class="mlbl" x="' + FIT + '" y="' + (y + 15) + '">In the routing set, not used by this flow</text>';
            let ux = FIT + 250;
            for (const n of chips) {
                if (ux + SLOT_W > width - FIT) { ux = FIT; y += CHIP_H + 6; }
                // EDITABLE, A CHIP NAMES ITS LANE: the station's Positions screen
                // (pick-a-part U4) moves a recipe's slot onto an unused lane.
                const tg = ctx.targets.has(n);
                s += '<g class="stage off chip' + (tg ? ' tgt' : '') + (opts.selectCard && sel === n ? ' sel' : '') + '"' +
                    (opts.editable ? ' data-staging="' + esc(n) + '"' : '') +
                    ' transform="translate(' + ux + ',' + y + ')">' +
                    nameTip(n, SLOT_W - 24, 12) +
                    '<rect class="box" width="' + SLOT_W + '" height="' + CHIP_H + '" rx="8"/>' +
                    txt('nm', 12, 16, n, tg ? SLOT_W - 92 : SLOT_W - 24, 12) +
                    (tg ? txt('tgtlbl', SLOT_W - 10, 16, 'move here', 72, 10, 600, 'end') : '') + '</g>';
                ux += SLOT_W + 8;
            }
            y += CHIP_H;
        }
    }

    // ── the dock ────────────────────────────────────────────────────────────
    const dock_y = y + 26;
    s += dockBand(sentences, width, dock_y, !!opts.editable);
    opts.height = dock_y + DOCK_BAND;

    // ── the legend, and the fallback's line beside it ───────────────────────
    //
    // THE FALLBACK SAYS WHY IT IS IN ORDER. A changeover cell the relation
    // picture cannot place is drawn as modules in sequence order, and the line
    // in the margin opposite the legend names the position that put it here —
    // "Shown in order: PLN_008 is not on the plant map." — so the operator
    // reads an order, not a plan, and an engineer knows which point to fix.
    const lg = legendOf(sentences, width);
    if (fallback) {
        const shown = fit(fallback, width - 2 * FIT - lg.w - 24, 12, 500);
        lg.svg += '<text class="fbl" x="' + FIT + '" y="' + (FIT + 8) + '">' +
            (shown[1] !== null ? '<title>' + esc(shown[1]) + '</title>' : '') + esc(shown[0]) + '</text>';
    }
    return lg.svg + s;
}

// ── panel ──────────────────────────────────────────────────────────────────
//
// One panel over the board, opened from the header's style chip (the thing
// that names the running style is the thing that shows its flow) and closed
// by its own Board button. #flow in the URL opens it on load, which is what
// scripts/composer-shots.sh renders. It re-draws on every view refresh while
// open, so the picture follows a changeover.

let open = false;

function panel() { return document.getElementById('os-flow'); }

export function isFlowPanelOpen() { return open; }

export function openFlowPanel() {
    open = true;
    syncFlowPanel(getView());
}

export function closeFlowPanel() {
    open = false;
    const p = panel();
    if (p) p.hidden = true;
}

// syncFlowPanel re-renders the panel from the view when it is open. Called by
// the header render on every refresh.
//
// THE SVG IS THE PICTURE'S OWN SIZE. The viewBox and the element's width and
// height are all the picture's, one user unit to one pixel, and the panel's
// stage scrolls a taller picture — or a relation grid wider than the frame,
// sideways (operator.css) — and never scales one, so the cards stay their
// fixed size on the kiosk whatever the cell needs. A changeover cell is drawn
// by relation here too (the scope gate): one cell, one picture, on the board
// and on the recipe steps.
export function syncFlowPanel(view) {
    const p = panel();
    if (!p) return;
    if (!open) { p.hidden = true; return; }
    if (!view) return;
    const title = document.getElementById('os-flow-title');
    const sub = document.getElementById('os-flow-sub');
    const svg = document.getElementById('os-flow-svg');
    const styleName = view.current_style ? view.current_style.name : 'No style running';
    if (title) title.textContent = styleName;
    if (sub) sub.textContent = view.target_style ? 'running · changing over to ' + view.target_style.name : (view.current_style ? 'running' : '');
    if (svg) {
        const o = { frame: { w: STATION_FRAME.w }, usedOnly: true };
        svg.innerHTML = renderFlowPicture(view, o);
        const h = o.height || STATION_FRAME.h;
        const w = o.width || STATION_FRAME.w;
        svg.setAttribute('viewBox', '0 0 ' + w + ' ' + h);
        svg.setAttribute('width', String(w));
        svg.setAttribute('height', String(h));
    }
    p.hidden = false;
}

// mountFlowPanel builds the panel once and wires its Board button. Safe to
// call more than once.
export function mountFlowPanel() {
    if (panel()) return;
    const section = el('section', { id: 'os-flow', className: 'os-flow' });
    section.hidden = true;
    const hdr = el('header', { className: 'os-flow-hdr' });
    hdr.appendChild(el('h1', { id: 'os-flow-title', className: 'os-flow-title' }));
    hdr.appendChild(el('span', { id: 'os-flow-sub', className: 'os-flow-sub' }));
    hdr.appendChild(el('span', { className: 'os-flow-sp' }));
    const close = el('button', { type: 'button', id: 'os-flow-close', className: 'os-flow-close', textContent: 'Board' });
    close.addEventListener('click', closeFlowPanel);
    hdr.appendChild(close);
    section.appendChild(hdr);
    const stage = el('div', { className: 'os-flow-stage' });
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 1280 560');
    svg.id = 'os-flow-svg';
    // The picture's CSS is scoped to this CLASS, not to the id: the composer
    // draws the same function into its own <svg> and has to look identical.
    svg.setAttribute('class', 'os-flow-picture');
    stage.appendChild(svg);
    section.appendChild(stage);
    document.body.appendChild(section);
    if (window.location && window.location.hash === '#flow') open = true;
}
