// operator-flow.js — the read-only picture of the cell: the press positions in
// their true relative arrangement, the running style's choreography drawn
// between them, and the dock the bins come from and go to.
//
// A PORT OF renderPress from the flow-composer reference
// (REFERENCE-press400-flow-composer-HK-2026-09-03.html, SPEC-composer-ux §2 S4)
// minus every control: no taps on positions, no panel, no bar, no strip. The
// composer (U8) will grow the controls back onto this drawing; this file is
// what every plant sees first.
//
// DATA COMES FROM THE STATION VIEW, and only from there: view.cell (positions
// with their scene coordinates, roles and the running claim on each — built
// in Go by domain.BuildCellPicture, where the node→map join rule lives),
// view.current_style, view.station. Nothing here invents a position, a
// partner, and no word for what a bin is: that came from view.cell.bin_word
// until owner ruling R2 (2026-09-12) removed the word and the field with it.
//
// ONLY THE PRESS IS DRAWN. Lines exist only between press positions; the
// supermarket is a name in the dock strip, never a box. Structure is the
// substrate ramp (--os-sub-*); the two robot hues (--os-r1/--os-r2) colour
// the legs, the in/out glyphs and the dock notes, and nothing static.
//
// THE PICTURE IS NOT ROTATED WITH THE PLANT. The shared projector's
// rotate90 is decided over the whole scene, and both plants are portrait —
// which would turn the press's two rows into two columns. This is a
// schematic of one cell, not a map: world X runs along the screen, the line
// side faces the operator, exactly as the reference drew it.

import { makeProjector, isCoord } from '/static/shared/scene-geom.js';
import { el } from './operator-util.js';
// The QUOTE-SAFE escape, not operator-util's. This file builds attributes by
// concatenation — data-pos, data-tap, transform — and the base's escaper
// leaves `"` alone. See shared/esc.js.
import { esc } from '/static/shared/esc.js';
import { getView } from './operator-state.js';

// Geometry of the drawing, in viewBox units (the reference's numbers).
export const CARD_W = 188, CARD_H = 92;
const GAP = 12;                 // the least daylight two cards may have
const PREFERRED_SCALE = 120;    // px per metre, what the reference used at Press 400
// ── the frame ─────────────────────────────────────────────────────────────
//
// THE PICTURE IS DRAWN 1:1 IN THE FRAME IT IS GIVEN, and the caller sets the
// viewBox to the same numbers. That is the whole reason the frame is a
// parameter and not six constants: a card is CARD_W pixels wide on screen only
// when the viewBox matches the element's real width, and the moment the two
// disagree preserveAspectRatio silently scales the drawing — text included.
//
// The station is 1280x560 and always was; every constant below was measured in
// that frame and STATION_FRAME keeps it the default, so passing no frame
// renders exactly what the kiosk rendered before. The desktop composer's main
// column is 1084 px wide at the spec's 1440 and its picture frame is 430 tall,
// and it says so rather than letting the browser fit a 1280x560 drawing into
// it at 0.59 — which is what it was doing, with 9 px card titles to show for it.
export const STATION_FRAME = { w: 1280, h: 560 };

const GUTTER = 150;     // where the dock strip starts and the row labels sit
const FIT_INSET = 20;   // the picture's own margins — the cards may reach these

// DOCK_BAND is what the dock needs BELOW its rule: four text rows whose last
// baseline is at +64 (the routing group's member line), plus room for its
// descenders. The dock is placed at whichever is higher up — the station
// frame's proportion, or the foot of the frame less this — so no frame height
// can clip it.
//
// WHY NOT PURELY FROM THE FOOT. At the station's 560 the proportion and
// `h - 90` are the same number, 470, and the ruling asked for the second
// spelling. But the cards are a FIXED 92 tall and do not scale with the frame,
// and at the desktop's 304 a dock pinned 90 above the foot leaves 97 px for
// two rows of them — the picture drops out of true spacing into the schematic
// and says so under itself. The frame gives way to the dock only as far as the
// dock actually needs, which at 304 is 234 rather than 214, and the cards keep
// their band. Measured, not reasoned: see the report's V1 note.
const DOCK_BAND = 74;
const CAPTION_ROOM = 38;  // the station-name line under the last row of cards

// ── the staging band ────────────────────────────────────────────────────────
//
// A STAGING CARD IS SHORTER THAN A POSITION, and that is the whole reason the
// band fits. At the station's 560 frame the space between the caption under the
// last row of cards and the dock's rule is about 77 units; a 92-tall position
// card does not go in it and a 48-tall one does, with room for the rule to stay
// where it is.
//
// It is also the right shape: a position card carries a name, two choreography
// lines and a part chip, and a staging card carries a name and who it parks
// for. Drawing it at a position's height would be claiming it is one.
//
// LAID IN ITS OWN BAND, NOT PLACED TO SCALE (SYNTH §3 B3). This is the RULE and
// not a fallback: placeToScale takes the largest scale at which no pair of
// cards collides, so one staging lane 40 m from the cell drives that scale to
// nothing, returns null, and flips the whole cell to the schematic — P400's
// 1.772 m spacing with it. The band is what keeps a far lane from moving a
// near cell.
export const STAGING_W = 150, STAGING_H = 48;
const STAGING_GAP = 14;   // between two cards in the band
const STAGING_LIFT = 14;  // between the band's floor and the dock's rule

// frameOf derives one frame's geometry. The horizontals are INSETS from the
// edges, because a gutter is a fixed amount of room for a label and does not
// want to shrink with the frame; the verticals are PROPORTIONS of the station
// frame's height, because they divide a fixed budget between two rows of cards
// and a dock strip. ROW_PITCH keeps a card's worth of daylight whatever the
// proportion works out to, so the schematic fallback cannot be squeezed into a
// collision by a short frame.
function frameOf(f) {
    const w = (f && f.w) || STATION_FRAME.w;
    const h = (f && f.h) || STATION_FRAME.h;
    const k = h / STATION_FRAME.h;
    return {
        w: w, h: h,
        LEFT: GUTTER, RIGHT: w - GUTTER,
        FIT_LEFT: FIT_INSET, FIT_RIGHT: w - FIT_INSET,
        CENTER_X: w / 2, CENTER_Y: Math.round(246 * k),
        ROW_BAND: Math.round(220 * k),
        ROW_PITCH: Math.max(CARD_H + GAP, Math.round(138 * k)),
        DOCK_Y: Math.min(Math.round(470 * k), h - DOCK_BAND),
    };
}

// THE SENTENCES COME FROM THE MODEL. ALWAYS.
//
// This file used to carry its own MODES table and its own cardLine(), a second
// copy of composer-model.js's MODES and CARDLINE — so the read-only picture
// (U4, the style chip's panel) and the composer drew the same press with two
// authors, and the mode-word drift test only regexed one of them. Same story
// for dockNotes: two implementations of the same two sentences, one reading a
// CellPicture and one reading model state.
//
// sentencesFromView closes it by going the other way: instead of this file
// re-deriving what the model already knows, the read-only caller builds a
// model state FROM THE VIEW and asks the model. The composer passes its own
// live state's sentences through opts. One author either way.
//
// The view's claims are readable by the model as they stand: CellClaim's json
// tags are the claim's own field names, which is what init() reads.
// sentencesFromModel is the opts.sentences block, shaped once. Both editing
// surfaces built this literal themselves — the same six fields, the same
// joins — which is the copy one layer up from the one this file just lost.
export function sentencesFromModel(state) {
    const M = typeof window !== 'undefined' ? window.ComposerModel : null;
    if (!M || !state) return null;
    const cardLines = {};
    for (const p of state.positions) cardLines[p.core_node_name] = M.cardLines(state, p.core_node_name);
    const d = M.dockNotes(state);
    return {
        cardLines: cardLines,
        legs: M.legs(state),
        dock: {
            srcs: [d.in.group], dsts: [d.out.group],
            inNote: d.in.note, outNote: d.out.note,
            inMembers: d.in.members.join(', '), outMembers: d.out.members.join(', '),
        },
    };
}

export function sentencesFromView(view) {
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
    return sentencesFromModel(state);
}

// ── layout ──────────────────────────────────────────────────────────────
//
// Returns {boxes, rows, toScale}. boxes: name → {x, y, w, h} in viewBox
// units. rows: [{kind, y, top, bottom}] top to bottom. toScale: false when
// the cards are evenly spaced instead of placed.
//
// TRUE RELATIVE SPACING, at the reference's scale where it fits, spread
// wider where the closest pair would otherwise touch, and even spacing
// under a "positions not to scale" caption when no legible scale keeps the
// cards apart — never a collision. The collision test is a COORDINATE test
// over every pair (dx AND dy), not a number test on the closest distance.
export function layoutPositions(cell, frame) {
    const g = frameOf(frame);
    const positions = (cell && cell.positions) || [];
    if (!positions.length) return { boxes: {}, rows: [], toScale: false };
    if (cell.geometry && positions.every(p => isCoord(p.x) && isCoord(p.y))) {
        const placed = placeToScale(positions, g);
        if (placed) return liftAboveDock(placed, g);
    }
    return liftAboveDock(placeEvenly(positions, g), g);
}

// liftAboveDock is the other half of V1: the DOCK is laid out from the frame's
// foot, and the cards take what is left.
//
// Both placements centre their rows on CENTER_Y, which is a proportion of the
// station frame's height — and a card is a FIXED 92 tall whatever the frame
// does, so at a short frame the rows reach further down, relative to
// everything else, than the proportion expected. At the desktop's 304 the
// second row ends 2 px below the dock's rule.
//
// Rather than re-derive CENTER_Y from the dock (which moves the station's own
// cards, and the u4 shots with them), the whole block SLIDES UP by exactly the
// overlap, and no further than the picture's top inset. At the station frame
// the overlap is zero and nothing moves — which is what keeps "the station
// frame is untouched" true while the short frame stops colliding.
function liftAboveDock(placed, g) {
    const names = Object.keys(placed.boxes);
    if (!names.length) return placed;
    let lowest = -Infinity, highest = Infinity;
    for (const n of names) {
        lowest = Math.max(lowest, placed.boxes[n].y + placed.boxes[n].h);
        highest = Math.min(highest, placed.boxes[n].y);
    }
    // CAPTION_ROOM is the station-name line under the last row: one baseline
    // at +32 with its descenders, so the rule needs to sit below that.
    const overlap = (lowest + CAPTION_ROOM) - g.DOCK_Y;
    const slack = highest - FIT_INSET;
    const lift = Math.min(Math.max(overlap, 0), Math.max(slack, 0));
    if (lift <= 0) return placed;
    for (const n of names) placed.boxes[n].y -= lift;
    for (const r of placed.rows) { r.top -= lift; r.bottom -= lift; }
    return placed;
}

function placeToScale(positions, g) {
    const proj = makeProjector(false);
    const pts = positions.map(p => { const s = proj(p.x, p.y); return { name: p.core_node_name, sx: s[0], sy: s[1] }; });
    const xs = pts.map(p => p.sx), ys = pts.map(p => p.sy);
    const spanX = Math.max(...xs) - Math.min(...xs), spanY = Math.max(...ys) - Math.min(...ys);
    const cx = (Math.max(...xs) + Math.min(...xs)) / 2, cy = (Math.max(...ys) + Math.min(...ys)) / 2;
    // The largest scale at which every card stays inside the picture.
    const fitX = spanX > 0 ? (g.FIT_RIGHT - g.FIT_LEFT - CARD_W) / spanX : Infinity;
    const fitY = spanY > 0 ? g.ROW_BAND / spanY : Infinity;
    const fit = Math.min(fitX, fitY);
    let scale = Math.min(PREFERRED_SCALE, fit);
    // The smallest scale at which no pair collides: for each pair, enough
    // room on EITHER axis.
    let needed = 0;
    for (let i = 0; i < pts.length; i++) {
        for (let j = i + 1; j < pts.length; j++) {
            const dx = Math.abs(pts[i].sx - pts[j].sx), dy = Math.abs(pts[i].sy - pts[j].sy);
            const sx = dx > 0 ? (CARD_W + GAP) / dx : Infinity;
            const sy = dy > 0 ? (CARD_H + GAP) / dy : Infinity;
            needed = Math.max(needed, Math.min(sx, sy));
        }
    }
    if (needed > scale) scale = needed;
    if (scale > fit || !isFinite(scale)) return null; // no legible scale keeps them apart
    const boxes = {};
    for (const p of pts) {
        boxes[p.name] = { x: g.CENTER_X + (p.sx - cx) * scale - CARD_W / 2, y: g.CENTER_Y + (p.sy - cy) * scale - CARD_H / 2, w: CARD_W, h: CARD_H };
    }
    return { boxes, rows: rowsOf(boxes), toScale: true };
}

// rowsOf clusters cards on screen y: cards whose centres are within half a
// card of each other share a row.
function rowsOf(boxes) {
    const centres = Object.keys(boxes).map(n => ({ name: n, cy: boxes[n].y + CARD_H / 2 })).sort((a, b) => a.cy - b.cy);
    const rows = [];
    for (const c of centres) {
        const last = rows[rows.length - 1];
        if (last && Math.abs(c.cy - last.cy) < CARD_H / 2) { last.names.push(c.name); continue; }
        rows.push({ cy: c.cy, names: [c.name] });
    }
    return rows.map(r => ({ names: r.names, top: r.cy - CARD_H / 2, bottom: r.cy + CARD_H / 2 }));
}

// placeEvenly is the schematic: the press's front slots on one row, its
// back slots on another when there are any, in sequence order, wrapped at
// five to a row so nothing overlaps.
function placeEvenly(positions, g) {
    const front = positions.filter(p => p.kind !== 'back');
    const back = positions.filter(p => p.kind === 'back');
    const lines = [];
    const wrap = list => { for (let i = 0; i < list.length; i += 5) lines.push(list.slice(i, i + 5)); };
    wrap(front); wrap(back);
    const boxes = {};
    const pitchY = lines.length > 1 ? g.ROW_PITCH : 0;
    const firstY = g.CENTER_Y - pitchY * (lines.length - 1) / 2;
    lines.forEach((line, r) => {
        const pitch = Math.min(CARD_W + 24, (g.RIGHT - g.LEFT) / line.length);
        const width = pitch * (line.length - 1);
        line.forEach((p, i) => {
            boxes[p.core_node_name] = { x: g.CENTER_X - width / 2 + pitch * i - CARD_W / 2, y: firstY + pitchY * r - CARD_H / 2, w: CARD_W, h: CARD_H };
        });
    });
    return { boxes, rows: rowsOf(boxes), toScale: false };
}

// layoutStaging places the staging cards in the band above the dock's rule.
//
// LEFT TO RIGHT IN THE ORDER THE PLANT HAS THEM when the picture is to scale,
// and by name when it is not. A band whose order had nothing to do with the
// floor would be a row of names to read; ordered by true X it is a row an
// operator can point along.
//
// CENTRED ON THE PICTURE, and narrowed to fit rather than allowed to run past
// the gutters: six lanes at 150 wide do not fit 1280 at full pitch, and a card
// drawn off the edge is a card nobody can tap.
export function layoutStaging(cell, frame) {
    const g = frameOf(frame);
    const cards = (cell && cell.staging) || [];
    if (!cards.length) return { boxes: {}, top: 0 };
    const placed = cards.every(c => isCoord(c.x));
    const order = cards.slice().sort((a, b) => (placed
        ? a.x - b.x
        : String(a.core_node_name).localeCompare(String(b.core_node_name))));
    const top = g.DOCK_Y - STAGING_LIFT - STAGING_H;
    const pitch = Math.min(STAGING_W + STAGING_GAP,
        (g.FIT_RIGHT - g.FIT_LEFT) / Math.max(order.length, 1));
    const width = pitch * (order.length - 1);
    const boxes = {};
    order.forEach((c, i) => {
        boxes[c.core_node_name] = {
            x: g.CENTER_X - width / 2 + pitch * i - STAGING_W / 2,
            y: top, w: STAGING_W, h: STAGING_H,
        };
    });
    return { boxes, top };
}

// pictureRows is which row of the DRAWING each position landed in: 'front' for
// the line-side row, 'back' for the far one, '' for a middle row or a picture
// with only one. Every caption beside the picture that says "front" or "back"
// has to come from here.
//
// IT IS NOT CellPosition.Kind, and the two disagree on a real press. Kind
// answers "is this a partner slot for any style this process runs" — at
// Hopkinsville PLN_01 and PLN_04 are Kind "front" and are DRAWN in the back
// row, with their on-deck partners. The desktop's positions table captioned
// them from Kind and so told the engineer "front" under a picture that said
// BACK. One word, one meaning: the sub-label is the geometry, and what a
// position does for a style is said by the paired-back line instead.
export function pictureRows(cell, frame) {
    const { rows } = layoutPositions(cell, frame);
    const kinds = rowKinds(rows);
    const out = {};
    rows.forEach((r, i) => r.names.forEach(n => { out[n] = kinds[i]; }));
    return out;
}

// rowKinds labels the rows the way the reference does: the row nearest the
// line — the top of the picture, where the projection puts the higher world
// Y — is FRONT · LINE SIDE and the bottom row is BACK. That is the geometry
// of the press, not the roles of the running style: at Hopkinsville the
// index positions PLN_01/PLN_04 sit in the BACK row with their on-deck
// partners, and the line-side row holds the swap positions. Rows between
// the two, and a lone row, are not labelled — a guess printed in capitals
// is still a guess.
function rowKinds(rows) {
    if (rows.length < 2) return rows.map(() => '');
    return rows.map((r, i) => (i === 0 ? 'front' : (i === rows.length - 1 ? 'back' : '')));
}

// ── legs ───────────────────────────────────────────────────────────────

// ortho spells a rounded orthogonal polyline (the reference's).
function ortho(pts, r) {
    r = r || 16;
    let d = 'M' + pts[0][0] + ' ' + pts[0][1];
    for (let i = 1; i < pts.length - 1; i++) {
        const [x0, y0] = pts[i - 1], [x1, y1] = pts[i], [x2, y2] = pts[i + 1];
        const d1 = Math.hypot(x1 - x0, y1 - y0), d2 = Math.hypot(x2 - x1, y2 - y1);
        const rr = Math.min(r, d1 / 2, d2 / 2);
        const ax = x1 - (x1 - x0) / d1 * rr, ay = y1 - (y1 - y0) / d1 * rr, bx = x1 + (x2 - x1) / d2 * rr, by = y1 + (y2 - y1) / d2 * rr;
        d += ' L' + ax + ' ' + ay + ' Q' + x1 + ' ' + y1 + ' ' + bx + ' ' + by;
    }
    const l = pts[pts.length - 1];
    return d + ' L' + l[0] + ' ' + l[1];
}

// legsFor DRAWS the choreography the model decided on: an index pair is a
// straight Robot 2 line between the paired cards, a staging move a rounded
// Robot 1 path from the staging card into the front card. Dot at the start,
// ring at the end, and one chevron on the run saying which way the bins go.
//
// THE DOT AND THE RING WERE THE DIRECTION, AND NOBODY READ THEM. A leg has
// always been drawn FROM its source TO its destination — that is what `a` and
// `z` are, and the path's own point order is the travel order — but the only
// thing on screen saying so was a filled circle at one end and a hollow one at
// the other. That is a convention, not a picture: an engineer looking at a
// still shot of a Robot 1 leg into a front position and a Robot 2 leg out of
// one saw two lines in two hues and had to already know which hue meant which
// way. `tip` is the same fact drawn as an arrowhead, which nobody has to be
// told how to read.
//
// THE ANGLE IS COMPUTED HERE, NOT LEFT TO AN SVG <marker orient="auto">, for
// two reasons and only the second is about taste. A marker is referenced by
// url(#id) and resolves over the WHOLE DOCUMENT: the composer draws this same
// function into its own <svg> beside another one, and two <marker id="legtip">
// in one document is one marker — whichever rendered last — so the hue of one
// picture's arrowheads would follow the other picture. And a marker sits at a
// path VERTEX, which on these legs is either the end (under the ring) or a
// rounded corner. The place a direction cue belongs is the middle of the long
// straight run, and this function is the only thing that knows where that is.
//
// IT TAKES THE LEGS RATHER THAN FINDING THEM. Which legs exist is a fact about
// the flow — composer-model.js's legs(state) answers it, in one place, and its
// label is the label — and where they go on screen is a fact about the layout,
// which is this file's business. This used to decide both, re-reading swap
// modes and partner fields to reach the same two answers the model had already
// reached, with the sentences spelled out a second time.
// A LEG WITH NO CARD AT ONE END IS A DEFECT, NOT A SKIP (SYNTH §3 B4). This
// was a bare `continue`: the model said a robot drives from A to B, the layout
// had no card for one of them, and the picture silently drew one line fewer.
// That is the failure the staging work is fixing — a bin that moves on the
// floor and not on the screen — so it reports rather than hides. The reporter
// is injected by the test harness; on a live screen it is a console line and
// the picture still draws every leg it can.
let onLegDropped = (leg, missing) => {
    if (typeof console !== 'undefined' && console.error) {
        console.error('flow picture: no card for ' + missing + ', so the ' +
            (leg.label || 'leg') + ' between ' + leg.from + ' and ' + leg.to + ' is not drawn');
    }
};

export function setLegDropReporter(fn) { onLegDropped = fn; }

export function legsFor(modelLegs, boxes) {
    const legs = [];
    for (const L of modelLegs || []) {
        const b = boxes[L.to], pb = boxes[L.from];
        if (!b || !pb) {
            onLegDropped(L, b ? L.from : L.to);
            continue;
        }
        const cx = b.x + b.w / 2, cy = b.y + b.h / 2;
        const pcx = pb.x + pb.w / 2, pcy = pb.y + pb.h / 2;
        const cls = L.robot === 2 ? 'r2' : 'r1';
        if (L.kind === 'index' && Math.abs(pcy - cy) < CARD_H / 2) {
            const dir = pcx < cx ? 1 : -1;
            const a = [pcx + dir * CARD_W / 2, pcy], z = [cx - dir * CARD_W / 2, cy];
            // An index leg is the daylight between two neighbouring cards and
            // nothing more — 24.6 units for the Hopkinsville pair the test
            // measures — so the chevron takes the midpoint and the dot and the
            // ring keep the ends. That is why it is a SMALL chevron: on the
            // shortest leg the picture draws there is about 16 units of line
            // between the two circles, and an arrowhead that needed more than
            // that would be an arrowhead that only fits on the long legs.
            legs.push({ cls: cls, d: 'M' + a[0] + ' ' + a[1] + ' L' + z[0] + ' ' + z[1],
                a: a, z: z, tip: [a[0] + (z[0] - a[0]) * TIP_AT, a[1] + (z[1] - a[1]) * TIP_AT, degOf(a, z)],
                lbl: L.label, lx: (pcx + cx) / 2, ly: b.y + b.h + 18, keyRoute: L.keyRoute || [] });
            continue;
        }
        const below = L.kind === 'index' ? pcy > cy : pb.y > b.y;
        const from = below ? [pcx, pb.y] : [pcx, pb.y + pb.h];
        const to = below ? [cx, b.y + b.h] : [cx, b.y];
        const mid = (from[1] + to[1]) / 2;
        // A rounded leg is a vertical, a horizontal run at `mid`, and another
        // vertical. The chevron goes on the RUN, where the line is straight and
        // long: ortho() eats 16 units at each end of it for the corner radius,
        // so a run shorter than 40 has no straight middle left to stand on and
        // the leg reads as a vertical drop anyway — that is the second branch,
        // which points the chevron the way the drop goes.
        const run = to[0] - from[0];
        const tip = Math.abs(run) >= 40
            ? [from[0] + run * TIP_AT, mid, run > 0 ? 0 : 180]
            : [(from[0] + to[0]) / 2, mid, to[1] > from[1] ? 90 : -90];
        legs.push({ cls: cls, d: ortho([from, [from[0], mid], [to[0], mid], to]), a: from, z: to, tip: tip,
            lbl: L.label, lx: (pcx + cx) / 2, ly: mid - 8, keyRoute: L.keyRoute || [] });
    }
    return legs;
}

// degOf is the tangent of a straight leg in degrees, rounded so the markup
// carries 65.2 rather than 65.19999999999999. operator-flow.test.js reads this
// drawing back as TEXT — every check in it is a regex over the markup — and a
// float tail is a diff nobody can read for no accuracy anyone can see.
// WHERE THE CHEVRON SITS ALONG ITS LEG. It was the midpoint, which is the
// obvious place and the wrong one: on a press-index pair the leg is about 25
// units long, so a chevron in the middle sits equidistant between the dot and
// the ring and reads as decoration on a line rather than as the line going
// somewhere. Two thirds along points AT the card receiving the bin, which is
// the fact an engineer is looking for — "I was thinking inbound staging would
// show up on that node flowing into the core node".
//
// Not at the very end: that is where the ring is, and an arrowhead under a
// ring is a smudge.
const TIP_AT = 0.68;

function degOf(a, z) {
    return Math.round(Math.atan2(z[1] - a[1], z[0] - a[0]) * 1800 / Math.PI) / 10;
}

// ── dock ───────────────────────────────────────────────────────────────

// litSet is the focus rule of SPEC §0.9: the tapped position and the positions
// it works with stay lit; everything else dims. "Works with" is structural —
// its partner, its staging, and whichever position is using IT as one — so
// tapping a back position lights the front position it serves rather than
// leaving the operator looking at one lit card with no context.
function litSet(cell, sel) {
    const out = new Set();
    if (!sel) return out;
    out.add(sel);
    for (const pos of cell.positions) {
        const c = pos.claim;
        if (!c) continue;
        const n = pos.core_node_name;
        const partners = [c.paired_core_node, c.inbound_staging, c.outbound_staging].filter(Boolean);
        if (n === sel) partners.forEach(p => out.add(p));
        if (partners.indexOf(sel) >= 0) out.add(n);
    }
    return out;
}

// ── render ─────────────────────────────────────────────────────────────

const IN = '<path d="M6 0v9M2 5l4 4 4-4M0 12h12" fill="none" stroke="currentColor" stroke-width="1.6"/>';
const OUT = '<path d="M6 12V3M2 7l4-4 4 4M0 14h12" fill="none" stroke="currentColor" stroke-width="1.6"/>';

// renderFlowPicture returns the SVG inner markup for one station view.
//
// EVERY INLINE COLOUR NAMES BOTH SURFACES' TOKEN. The rules in
// flow-picture.css read `var(--os-x, var(--shared-x))` because the station
// loads only operator.css and the desktop loads only shared/tokens.css; the
// inline styles written here are the same drawing and need the same pair.
// They did not have it, so on the desktop the in/out corner glyphs and the two
// dock notes resolved to nothing and inherited: the teal and indigo that say
// WHICH ROBOT went grey on one of the two surfaces. The station was right and
// the admin page was quietly wrong, which is the hardest version to notice.
//
// ONE DRAWING OF THIS PRESS, TWO CALLERS. U4 opens it read-only from the header
// style chip; the Flow Composer (U8) draws the SAME function over its own model
// state, passing opts. Extended rather than copied: a second renderer is a
// second set of card geometry, and the day one of them learns about a new
// position kind is the day the operator sees two different presses on one
// station depending on which door they came through.
//
// opts, all optional:
//   selected   — the tapped position; it lights and its partners stay lit,
//                everything else drops to 35% (SPEC §0.9). Absent = nothing dims.
//   findings   — {node: shortText}; the card outlines alarm and the red pill
//                takes the part chip's slot.
//   sentences  — {cardLines: {node: [line, …]}, legs: […], dock: {…}} from
//                composer-model. The COMPOSER passes its live state's, so the
//                copy an operator is editing is the copy they read; every
//                other caller leaves it out and this derives the same thing
//                from the view through the same model (sentencesFromView).
//                There is no third answer: this file computes no sentence.
//   editable   — marks the cards and dock halves tappable (data-tap).
//   frame      — {w, h} the picture is drawn 1:1 into, and the viewBox the
//                caller must set to match. Absent = the station's 1280x560.
export function renderFlowPicture(view, opts) {
    opts = opts || {};
    const cell = (view && view.cell) || { positions: [], geometry: false };
    // The composer passes its live state's sentences; every other caller gets
    // them derived from the view through the same model. See sentencesFromView.
    const sentences = opts.sentences || sentencesFromView(view) || { cardLines: {}, legs: [], dock: {
        srcs: [], dsts: [], inNote: 'Inbound source', outNote: 'Outbound destination',
        inMembers: '', outMembers: '' } };
    const stationName = view && view.station ? view.station.name : '';
    const byName = {};
    cell.positions.forEach(p => { byName[p.core_node_name] = p; });
    const g = frameOf(opts.frame);
    const { boxes, rows, toScale } = layoutPositions(cell, opts.frame);
    // THE STAGING BAND IS LAID APART AND THEN MERGED FOR THE LEGS. Apart,
    // because a staging lane must never reach placeToScale's span, centroid or
    // collision set — one lane 40 m out drives the scale to nothing and flips
    // the whole cell to the schematic. Merged, because a leg ends on a card and
    // does not care which list the card came from.
    const staged = layoutStaging(cell, opts.frame);
    const allBoxes = Object.assign({}, boxes, staged.boxes);
    let s = '';
    if (!cell.positions.length) {
        return '<text class="mlbl" x="' + g.CENTER_X + '" y="' + g.CENTER_Y + '" text-anchor="middle">No positions on this station yet</text>';
    }
    const kinds = rowKinds(rows);
    rows.forEach((r, i) => {
        const label = kinds[i] === 'front' ? 'FRONT · LINE SIDE' : (kinds[i] === 'back' ? 'BACK' : '');
        if (label) s += '<text class="mlbl" x="' + g.LEFT + '" y="' + (r.top - 12) + '" style="letter-spacing:.08em">' + label + '</text>';
    });
    const last = rows[rows.length - 1];
    // The caption sits under the last row of cards, and above the dock's rule:
    // at a short frame the cards reach further down than the proportion
    // expected and a caption placed from them alone crosses the line it is
    // supposed to sit above.
    const capY = Math.min(last.bottom + 32, g.DOCK_Y - 12);
    s += '<text class="mlbl" x="' + g.LEFT + '" y="' + capY + '" style="fill:var(--os-text-dim, var(--text-muted))">' + esc(stationName.toUpperCase()) +
        (toScale ? ' · positions at true spacing' : ' · positions not to scale') + '</text>';

    // THE LEG ITSELF IS UNTOUCHED, AND THAT IS THE POINT. .legflow is a SECOND
    // path with the same `d` carrying the travelling dashes, rather than a dash
    // pattern put on .leg — so the line an operator has been looking at for a
    // year keeps its hue, its width and its place in the stack, and everything
    // the animation does can be switched off by hiding one element. Under
    // prefers-reduced-motion flow-picture.css does exactly that, and what is
    // left on screen is the picture as it was plus the chevron.
    //
    // TIP AFTER THE RING, RING AFTER THE DOT: within one leg's group the marks
    // are drawn in the order they must not be hidden in, and the groups
    // themselves are still emitted in the model's leg order, so which robot's
    // leg is over which is unchanged.
    for (const L of legsFor(sentences.legs, allBoxes)) {
        s += '<g class="legg"><path class="leg thin ' + L.cls + '" d="' + L.d + '"/>' +
            '<path class="legflow ' + L.cls + '" d="' + L.d + '"/>' +
            '<circle class="legdot ' + L.cls + '" cx="' + L.a[0] + '" cy="' + L.a[1] + '" r="4"/>' +
            '<circle class="legring ' + L.cls + '" cx="' + L.z[0] + '" cy="' + L.z[1] + '" r="4"/>' +
            '<path class="legtip ' + L.cls + '" d="M-5 -4.4L5.5 0L-5 4.4Z" transform="translate(' + L.tip[0] + ',' + L.tip[1] + ') rotate(' + L.tip[2] + ')"/>' +
            '<text class="leg-lbl ' + L.cls + '" x="' + L.lx + '" y="' + L.ly + '" text-anchor="middle">' + L.lbl + '</text></g>';
    }

    const sel = opts.selected || null;
    const findings = opts.findings || {};
    const lit = litSet(cell, sel);
    for (const pos of cell.positions) {
        const b = boxes[pos.core_node_name];
        if (!b) continue;
        const n = pos.core_node_name, c = pos.claim;
        const bad = findings[n];
        const dim = sel && !lit.has(n);
        let glyphs = '', chip = '';
        if (c) {
            glyphs = '<g class="io" transform="translate(' + (b.w - 58) + ',10)" style="color:var(--os-r1, var(--robot-1))"><g>' + IN + '</g><text x="16" y="11" class="iot">in</text></g>' +
                '<g class="io" transform="translate(' + (b.w - 30) + ',10)" style="color:var(--os-r2, var(--robot-2))"><g>' + OUT + '</g><text x="16" y="11" class="iot">out</text></g>';
            if (bad) {
                // The finding takes the chip's slot rather than sitting beside
                // it: two things in one row is how a card starts scrolling.
                chip = '<rect class="needbg" x="12" y="64" width="' + (b.w - 24) + '" height="20" rx="5"/><text class="needlbl" x="' + (b.w / 2) + '" y="78" text-anchor="middle">' + esc(bad) + '</text>';
            } else if (c.payload_code) {
                chip = '<rect class="partbg" x="12" y="64" width="' + (b.w - 24) + '" height="20" rx="5"/><text class="partlbl" x="' + (b.w / 2) + '" y="78" text-anchor="middle">' + esc(c.payload_code) + '</text>';
            }
        } else if (pos.role === 'back') {
            glyphs = '<g class="io" transform="translate(' + (b.w - 30) + ',10)" style="color:var(--os-r1, var(--robot-1))"><g>' + IN + '</g><text x="16" y="11" class="iot">in</text></g>';
        }
        const parts = (sentences.cardLines && sentences.cardLines[n]) || [];
        const lines = parts.map((l, i) => '<text class="ln" x="14" y="' + (43 + i * 14) + '">' + esc(l) + '</text>').join('');
        s += '<g class="node ' + (c ? 'on' : 'off') + (sel === n ? ' sel' : '') + (bad ? ' bad' : '') + (dim ? ' dim' : '') +
            '" data-pos="' + esc(n) + '"' + (opts.editable ? ' data-tap="pos"' : '') + ' transform="translate(' + b.x + ',' + b.y + ')"><rect class="box" width="' + b.w + '" height="' + b.h + '" rx="12"/>' +
            '<text class="nm" x="14" y="26">' + esc(n) + '</text>' + lines + chip + glyphs + '</g>';
    }

    // ── the staging band ────────────────────────────────────────────────
    //
    // A SHORTER CARD, AND IT SAYS WHICH DIRECTION IT IS. The two lines are the
    // model's own words for a staging slot — `Inbound staging` / `for PLN_03` —
    // the same pair cardLines already puts on a back position doing the same
    // job, so the floor reads one sentence about staging and not two.
    //
    // AN OFFERED LANE IS DASHED AND SAYS SO. On the composer's picture the band
    // carries every staging lane the cell MAY park at, and an operator has to be
    // able to tell the one this flow uses from the ones it could.
    for (const st of (cell.staging || [])) {
        const b = staged.boxes[st.core_node_name];
        if (!b) continue;
        const inUse = !!st.partner_of;
        const word = st.field === 'outbound_staging' ? 'Outbound staging'
            : st.field === 'inbound_staging' ? 'Inbound staging' : 'Staging';
        const line2 = inUse ? 'for ' + st.partner_of : 'not in this flow';
        s += '<g class="stage ' + (inUse ? 'on' : 'off') + '" data-staging="' + esc(st.core_node_name) + '"' +
            // A TAP GOES TO THE POSITION THIS LANE SERVES, which is the thing
            // an operator can change — there is no panel for a lane itself, and
            // an OFFERED lane serves nobody yet, so it is not tappable at all
            // rather than tappable and inert.
            (opts.editable && inUse
                ? ' data-tap="staging" data-pos="' + esc(st.partner_of) + '"'
                : '') +
            ' transform="translate(' + b.x + ',' + b.y + ')">' +
            '<rect class="box" width="' + b.w + '" height="' + b.h + '" rx="10"/>' +
            '<text class="nm" x="12" y="20">' + esc(st.core_node_name) + '</text>' +
            '<text class="ln" x="12" y="36">' + esc(word) + ' · ' + esc(line2) + '</text></g>';
    }

    s += lmPath(cell, allBoxes, sentences.legs, g, toScale);

    const d = sentences.dock;
    const tapIn = opts.editable ? ' data-tap="dock" data-dock="in"' : '';
    const tapOut = opts.editable ? ' data-tap="dock" data-dock="out"' : '';
    s += '<g class="dock"><line x1="' + g.LEFT + '" y1="' + g.DOCK_Y + '" x2="' + g.RIGHT + '" y2="' + g.DOCK_Y + '"/>' +
        '<g class="half"' + tapIn + ' transform="translate(' + g.LEFT + ',' + g.DOCK_Y + ')"><g transform="translate(16,26)" style="color:var(--os-r1, var(--robot-1))">' + IN + '</g><text class="k" x="36" y="36">IN</text>' +
        '<text class="v" x="70" y="30">' + esc(d.srcs.join(' · ') || '—') + '</text><text class="s" x="70" y="48" style="fill:var(--os-r1, var(--robot-1))">' + esc(d.inNote) + '</text><text class="s" x="70" y="64">' + esc(d.inMembers) + '</text></g>' +
        '<g class="half"' + tapOut + ' transform="translate(' + (g.CENTER_X + 10) + ',' + g.DOCK_Y + ')"><g transform="translate(16,26)" style="color:var(--os-r2, var(--robot-2))">' + OUT + '</g><text class="k" x="36" y="36">OUT</text>' +
        '<text class="v" x="80" y="30">' + esc(d.dsts.join(' · ') || '—') + '</text><text class="s" x="80" y="48" style="fill:var(--os-r2, var(--robot-2))">' + esc(d.outNote) + '</text><text class="s" x="80" y="64">' + esc(d.outMembers) + '</text></g></g>';
    return s;
}

// ── the LM path (owner, 2026-09-17) ────────────────────────────────────────
//
// FIRST CUT, FOR THE OWNER'S EYE. The brief calls this the one piece of new
// visual design in the work and says to propose it with shots before polishing
// it, so what is here is the mechanism drawn plainly: the waypoints a robot
// passes on its way between two cards, marked along the leg that joins them.
// Nothing about the marks' size, weight or labelling is settled.
//
// WHY IT IS NOT THE ROUTE PLANNER. The station has no plant map — the desktop's
// Map is a hundred kilobytes for a screen the station never opens — so the
// picture cannot walk the network to find a path. What it has is the LM points
// inside its own region (CellPicture.LMs, bounded to the cell) and the legs it
// is already drawing. So the drawing is: the LMs that lie near a leg, in the
// order they lie along it.
//
// TO SCALE, THE TRUE PLACES; SCHEMATIC, EVEN SPACING. When the picture is drawn
// to scale the marks go where the map puts them, projected with the same
// projector the cards use — an operator can point at one. When it is the
// schematic there is no true place to go to, so they are spread evenly along
// the leg in path order, which still says how many and in what sequence.
//
// A PATH THAT LEAVES THE FRAME ENDS AT THE DOCK STRIP. The "no long lines to
// far supermarkets" ruling: a mark outside the picture's own inset is not
// drawn, and the leg keeps its ordinary end.
// TWO WEIGHTS, AND ONLY ONE OF THEM CARRIES A NAME.
//
// THE PATH is every waypoint the robot passes, as small hollow beads: it says
// "the robot goes this way, through there", which is a shape to glance at.
// THE KEY ROUTE is the subset an engineer CHOSE — the ordered points SEER is
// told to drive (claim.key_route) — drawn filled and labelled, because those
// are the ones worth being able to name out loud on the floor.
//
// The first draft labelled every bead. On the two-robot fixture that is ten
// labels along a 230-unit line: unreadable, and it turned the drawing into a
// list. A name that nobody chose is not a name worth the ink.
const LM_R = 2.5;          // the path's bead
const LM_KEY_R = 4.5;      // a chosen key-route point
const LM_NEAR = 42;        // how close to the leg's line an LM has to be, in viewBox units

// HOW MANY BEADS A LEG'S PATH SHOWS. The aisle beside a cell carries a
// waypoint every metre or so, which on a 230-unit leg is eleven of them — and
// eleven evenly spaced dots is not a path with waypoints on it, it is a dotted
// line. Four says the same thing (the robot goes this way, through there) and
// leaves the leg reading as a leg.
//
// A CHOSEN KEY-ROUTE POINT IS NEVER THINNED OUT: it is the half of the drawing
// that carries a name, and dropping one would be the picture hiding the thing
// an engineer explicitly asked SEER to drive through.
const LM_PATH_MAX = 4;

function lmPath(cell, boxes, modelLegs, g, toScale) {
    const lms = (cell && cell.lms) || [];
    if (!lms.length) return '';
    const placed = toScale && lms.every(l => isCoord(l.x) && isCoord(l.y));
    const proj = makeProjector(false);
    // The cards' own transform, recovered from a placed position: the picture
    // scales and centres the scene, and an LM has to land in the same frame.
    const anchor = placedAnchor(cell, boxes, proj);
    const at = lm => {
        const sp = proj(lm.x, lm.y);
        return [sp[0] * anchor.scale + anchor.ox, sp[1] * anchor.scale + anchor.oy];
    };
    let out = '';
    for (const L of legsFor(modelLegs, boxes)) {
        const chosen = L.keyRoute || [];
        // A CHOSEN POINT IS ALWAYS DRAWN, wherever it is. It is the half of
        // this drawing that carries a name, and the reason it cannot be found
        // by the "lies along the leg" test is the shape of a real cell: a swap
        // leg is a 1.8 m move between two adjacent cards, and the waypoints an
        // engineer picks are on the AISLE the robot drives in from — metres
        // away, and on no segment between two cards. A route the engineer
        // chose that the picture declined to draw because of where it is would
        // be the picture answering a different question.
        if (placed && anchor && chosen.length) {
            const marks = chosen
                .map(name => lms.find(l => l.name === name))
                .filter(Boolean)
                .map(lm => { const p = at(lm); return { name: lm.name, x: p[0], y: p[1] }; })
                // A PATH THAT LEAVES THE FRAME ENDS AT THE DOCK STRIP: a mark
                // outside the picture's own inset is not drawn, and the leg
                // keeps its ordinary end. That is the "no long lines to far
                // supermarkets" ruling, drawn rather than argued.
                .filter(m => m.x >= FIT_INSET && m.x <= g.w - FIT_INSET &&
                             m.y >= FIT_INSET && m.y <= g.DOCK_Y);
            if (marks.length) {
                // The thin line from the leg's start THROUGH the chosen points,
                // in the order SEER is told to drive them. It is the route, and
                // the leg it joins is where the route ends.
                let d = 'M' + L.a[0] + ' ' + L.a[1];
                for (const m of marks) d += ' L' + m.x + ' ' + m.y;
                out += '<path class="lmroute ' + L.cls + '" d="' + d + '"/>';
                for (const m of marks) {
                    out += '<g class="lm key ' + L.cls + '">' +
                        '<circle cx="' + m.x + '" cy="' + m.y + '" r="' + LM_KEY_R + '"/>' +
                        '<text class="lmlbl" x="' + m.x + '" y="' + (m.y - 9) + '" text-anchor="middle">' +
                        esc(m.name) + '</text></g>';
                }
            }
        }
        // And the PATH: the waypoints the robot passes along this leg itself,
        // thinned, unnamed. Empty on a cell whose swaps do not cross an aisle,
        // which is most of them.
        const along = thinPath(placed && anchor
            ? lmsAlongLeg(lms, L, proj, anchor)
            : lmsEvenly(lms, L), new Set(chosen));
        for (const m of along) {
            if (m.x < FIT_INSET || m.x > g.w - FIT_INSET || m.y < FIT_INSET || m.y > g.DOCK_Y) continue;
            if (chosen.indexOf(m.name) >= 0) continue;   // already drawn, named
            out += '<g class="lm ' + L.cls + '">' +
                '<circle cx="' + m.x + '" cy="' + m.y + '" r="' + LM_R + '"/></g>';
        }
    }
    return out;
}

// placedAnchor recovers scene → screen from one position the layout placed:
// a card's centre is its projected point scaled and offset, so two placed
// cards give the scale and the offset back.
function placedAnchor(cell, boxes, proj) {
    const pts = [];
    for (const p of (cell.positions || [])) {
        const b = boxes[p.core_node_name];
        if (!b || !isCoord(p.x) || !isCoord(p.y)) continue;
        const sp = proj(p.x, p.y);
        pts.push({ sx: sp[0], sy: sp[1], cx: b.x + b.w / 2, cy: b.y + b.h / 2 });
        if (pts.length === 2) break;
    }
    if (pts.length < 2) return null;
    const dsx = pts[1].sx - pts[0].sx, dsy = pts[1].sy - pts[0].sy;
    const dcx = pts[1].cx - pts[0].cx, dcy = pts[1].cy - pts[0].cy;
    const span = Math.hypot(dsx, dsy);
    if (span < 1e-9) return null;
    const scale = Math.hypot(dcx, dcy) / span;
    return { scale: scale, ox: pts[0].cx - pts[0].sx * scale, oy: pts[0].cy - pts[0].sy * scale };
}

// thinPath keeps every CHOSEN point and evenly samples the rest down to
// LM_PATH_MAX, preserving the order they lie in along the leg.
function thinPath(along, chosen) {
    const rest = along.filter(m => !chosen.has(m.name));
    if (rest.length <= LM_PATH_MAX) return along;
    const step = rest.length / LM_PATH_MAX;
    const keep = new Set();
    for (let i = 0; i < LM_PATH_MAX; i++) keep.add(rest[Math.floor(i * step)]);
    return along.filter(m => chosen.has(m.name) || keep.has(m));
}

// lmsAlongLeg is the LMs lying near this leg's straight run, at their true
// screen places, ordered from the leg's start to its end.
//
// USUALLY EMPTY ON A REAL CELL, and that is not a defect. A swap leg is a 1.8 m
// move between two adjacent cards; the aisle a robot drives in along is metres
// off it. What this finds is the case where a leg DOES cross a waypoint — a
// wide cell, a staging lane across a gangway — and there the beads say so.
function lmsAlongLeg(lms, L, proj, anchor) {
    const ax = L.a[0], ay = L.a[1], zx = L.z[0], zy = L.z[1];
    const dx = zx - ax, dy = zy - ay;
    const len2 = dx * dx + dy * dy;
    if (len2 < 1e-9) return [];
    const out = [];
    for (const lm of lms) {
        const sp = proj(lm.x, lm.y);
        const x = sp[0] * anchor.scale + anchor.ox, y = sp[1] * anchor.scale + anchor.oy;
        const t = ((x - ax) * dx + (y - ay) * dy) / len2;
        if (t < 0 || t > 1) continue;
        const px = ax + dx * t, py = ay + dy * t;
        if (Math.hypot(x - px, y - py) > LM_NEAR) continue;
        out.push({ name: lm.name, x: x, y: y, t: t });
    }
    return out.sort((p, q) => p.t - q.t);
}

// lmsEvenly is the schematic's answer: the same marks, spread along the leg in
// name order, because a picture that is not to scale has no true place to put
// them and pretending otherwise is the drawing lying about the floor.
function lmsEvenly(lms, L) {
    const picked = lms.slice().sort((a, b) => String(a.name).localeCompare(String(b.name))).slice(0, 3);
    return picked.map((lm, i) => {
        const t = (i + 1) / (picked.length + 1);
        return { name: lm.name, x: L.a[0] + (L.z[0] - L.a[0]) * t, y: L.a[1] + (L.z[1] - L.a[1]) * t };
    });
}

// ── panel ──────────────────────────────────────────────────────────────
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
    if (svg) svg.innerHTML = renderFlowPicture(view);
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
    const legend = el('div', { className: 'os-flow-legend' });
    const l1 = el('span'); l1.appendChild(el('i', { className: 'r1' })); l1.appendChild(document.createTextNode('Robot 1'));
    const l2 = el('span'); l2.appendChild(el('i', { className: 'r2' })); l2.appendChild(document.createTextNode('Robot 2'));
    legend.appendChild(l1); legend.appendChild(l2);
    section.appendChild(legend);
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
