// operator-board-model.js — the loader and unloader board's model: the layout
// a station's tiles make, the parts each loader tile draws and which of them a
// running style uses, each served cell's coverage, the changeover instruction,
// and the CLEAR panel's highlight.
//
// Every read of a loader tile is here, off tile.loader and view.serves, the
// keys BuildView ships for the board (domain.TileLoader, domain.ServedRow);
// operator-render.js, operator-modal.js and operator-window-state.js draw from
// what these return. The Go test TestStationView_ModelReadsAreWireKeys
// (shingo-edge/www/board_model_keys_test.go) reads the `tile.`, `loader.`,
// `row.`, `bin.` and `view.` reads out of this file and checks each one is a key of the
// station's JSON: a key renamed in Go cannot leave the board drawing blanks.
//
// Pure: no DOM and no fetch. The board reads the model; the model reads only
// its arguments.

'use strict';

// loaderOf is a tile's loader, or null on every tile that is not a Core
// loader's window or position.
export function loaderOf(tile) {
    return (tile && tile.loader) || null;
}

// tileRole is a loader tile's role, produce (a loader) or consume (an unloader).
export function tileRole(tile) {
    const loader = loaderOf(tile);
    return loader ? loader.role : undefined;
}

// partCodes is the parts a loader tile can load, in the loader's order.
export function partCodes(tile) {
    const loader = loaderOf(tile);
    return loader ? (loader.parts || []).map(function (p) { return p.code; }) : [];
}

// carriersOf is the bin types an unloader tile's CLEAR panels offer, from
// Core's catalog for the tile's parts (service carriersFor). Empty when Core
// has sent no catalog: the panel then asks no type.
export function carriersOf(tile) {
    const loader = loaderOf(tile);
    return (loader && loader.carriers) || [];
}

// isOperatorReplenished: a person stages this loader (Core's replenishment
// `operator`). A board set to hide idle cards still keeps such a loader's
// EMPTY spot, where the operator orders the next empty.
export function isOperatorReplenished(tile) {
    const loader = loaderOf(tile);
    return !!loader && loader.replenishment === 'operator';
}

// showsAllParts: the station's board lists every part this loader takes, not
// only what a running style needs (Core's bin_loaders.board_shows_all_parts,
// backfilled from replenishment = operator).
export function showsAllParts(tile) {
    const loader = loaderOf(tile);
    return !!loader && !!loader.board_shows_all_parts;
}

// isDedicated: a loader with a home per part (Core's dedicated_positions).
export function isDedicated(tile) {
    const loader = loaderOf(tile);
    return !!loader && loader.layout === 'dedicated_positions';
}

// isUnloaderWindow: a window of a shared unloader, which the board draws as a
// slot of the node grid (a tap on a full is the CLEAR panel).
export function isUnloaderWindow(tile) {
    const loader = loaderOf(tile);
    return !!loader && loader.role === 'consume' && !isDedicated(tile);
}

// ── the layout ───────────────────────────────────────────────────────────────

// boardLayout is how the board draws a station's claimed tiles:
//
//   kind   'empty'    nothing to draw;
//          'homes'    every tile a loader's and one of them dedicated: a card per
//                     home and part;
//          'payload'  every tile a loader's, none dedicated, not every one an
//                     unloader: a card per window and part;
//          'grid'     the node grid, every other station (unloader windows as
//                     slots, a loader beside a press).
//   header 'pull-full-from-home' on a station with a dedicated loader;
//          'none' on a payload board, and on a station of one loader tile;
//          'changeover' (CHANGEOVER, or CUTOVER and CANCEL) otherwise.
//
// A payload board of two windows gets no cell buttons in its header: its acts
// are the loader's (a bug fixed, the header kept the cell's buttons when the
// board had more than one window).
export function boardLayout(tiles) {
    const loaders = tiles.filter(function (t) { return !!loaderOf(t); });
    const homes = loaders.some(isDedicated);
    const all = loaders.length >= 1 && loaders.length === tiles.length;
    const slots = all && !homes && loaders.every(isUnloaderWindow);
    let kind = 'grid';
    if (tiles.length === 0) kind = 'empty';
    else if (all && !slots) kind = homes ? 'homes' : 'payload';
    let header = 'changeover';
    if (homes) header = 'pull-full-from-home';
    else if (kind === 'payload' || (tiles.length === 1 && loaders.length === 1)) header = 'none';
    return { kind: kind, header: header, loaders: loaders };
}

// ── the parts a running style uses ───────────────────────────────────────────

// servedRole is the role of the cells a tile serves: a loader fills cells that
// consume, an unloader empties cells that produce.
function servedRole(tile) {
    return tileRole(tile) === 'produce' ? 'consume' : 'produce';
}

// activeParts is the tile's parts that a running style of a served cell uses
// (a row of view.serves of the tile's served role, not incoming), in the
// loader's order; null when serves is absent (the board could not look), which
// is not the same as [] (it looked, nothing runs).
export function activeParts(tile, serves) {
    if (!Array.isArray(serves)) return null;
    const role = servedRole(tile);
    return partCodes(tile).filter(function (code) {
        return serves.some(function (row) { return !row.incoming && row.role === role && row.part === code; });
    });
}

// cardParts is the parts a loader tile draws a card for: every part on a
// loader whose board lists every part (showsAllParts), running parts first
// then by code; on any other the running parts, or every part when none runs
// or the board could not look.
export function cardParts(tile, serves) {
    const parts = partCodes(tile);
    const active = activeParts(tile, serves) || [];
    if (!showsAllParts(tile)) return active.length > 0 ? active : parts;
    return parts.slice().sort(function (a, b) {
        const ra = active.indexOf(a) !== -1 ? 0 : 1;
        const rb = active.indexOf(b) !== -1 ? 0 : 1;
        if (ra !== rb) return ra - rb;
        return a < b ? -1 : (a > b ? 1 : 0);
    });
}

// partState is what a card says of its part beyond its order state:
//
//   active     a running style of a served cell uses it;
//   inScope    a queued call for it is downtime: it is active, or the board
//              could not look (no serves) and so cannot rule it out. A part no
//              running style uses is not (a bug fixed: "no part running"
//              arrived as no data and every queued call escalated);
//   lineside   the served cells' count of it, the sum of its running rows;
//   short      the part is out, waiting or exposed at a served cell and no
//              tile of this board holds it ready (coverage). Core's reorder
//              threshold is not the danger level: it sits at or above a bin,
//              so every running part reads below it.
export function partState(tile, code, view) {
    const serves = view && view.serves;
    const active = activeParts(tile, serves);
    const isActive = !!active && active.indexOf(code) !== -1;
    let lineside = 0;
    let short = false;
    if (isActive) {
        const role = servedRole(tile);
        coverage(view).forEach(function (c) {
            if (c.row.incoming || c.row.role !== role || c.row.part !== code) return;
            if (typeof c.row.have_uop === 'number') lineside += c.row.have_uop;
            if (c.state === 'out' || c.state === 'waiting' || c.state === 'exposed') short = true;
        });
    }
    return { active: isActive, inScope: active === null || isActive, lineside: lineside, short: short };
}

// ── coverage ─────────────────────────────────────────────────────────────────

const STATE_ORDER = { out: 0, waiting: 1, exposed: 2, unknown: 3, covered: 4 };

// readyAt reports whether a tile of this board holds a stocked bin of the
// part: what a served cell running short is waiting for.
function readyAt(view, part) {
    return (view.nodes || []).some(function (tile) {
        const bin = tile.bin_state;
        return !!loaderOf(tile) && !!bin && bin.occupied && bin.payload_code === part && bin.stocked;
    });
}

// emptyReady reports whether the board's empties hold one of the bin type
// (view.empties, absent until the board reads its outbound group).
function emptyReady(view, binType) {
    const empties = view.empties;
    if (!empties || !Array.isArray(empties.rows)) return false;
    return empties.rows.some(function (r) { return r.bin_type === binType && r.have > 0; });
}

// coverage grades each served row, most urgent first:
//
//   a running row: left = have_uop; below 0 (overpack, the cell still runs) or
//   null (a cell the Edge cannot see) is 'unknown', never out; else ready
//   (covered) when a tile of this board holds a stocked bin of the part, or for
//   a produce row on an unloader board when the empties hold one of its bin
//   type; not ready, 'out' with none left, else 'waiting' while the cell has
//   asked for it (row.waiting: an order queued with Core's reason) and
//   'exposed' when it has not;
//   an incoming row: drawn only while waiting, 'out' with none left, else
//   'waiting'; never exposed.
//
// Ordered out, waiting, exposed, unknown, covered, then by the count, fewest
// left first; rows of one state and count keep the plant's order (serves').
export function coverage(view) {
    const serves = (view && view.serves) || [];
    const out = [];
    serves.forEach(function (row, i) {
        const left = row.have_uop;
        const known = typeof left === 'number' && left >= 0;
        let state;
        if (row.incoming) {
            if (!row.waiting) return;
            state = known && left === 0 ? 'out' : 'waiting';
        } else if (!known) {
            state = 'unknown';
        } else {
            const ready = row.role === 'produce' ? emptyReady(view, row.carrier) : readyAt(view, row.part);
            if (ready) state = 'covered';
            else if (left === 0) state = 'out';
            else state = row.waiting ? 'waiting' : 'exposed';
        }
        out.push({ row: row, state: state, left: known ? left : null, i: i });
    });
    out.sort(function (a, b) {
        if (a.state !== b.state) return STATE_ORDER[a.state] - STATE_ORDER[b.state];
        const la = a.left === null ? Infinity : a.left;
        const lb = b.left === null ? Infinity : b.left;
        if (la !== lb) return la - lb;
        return a.i - b.i;
    });
    return out;
}

// ── the changeover instruction ───────────────────────────────────────────────

// changeoverInstruction is what the board tells a loader while a served cell
// waits on what it makes: the bin types of the waiting
// rows (what the operator fetches), their parts and the cells' positions, each
// once and sorted. Null when the station's directive setting is off or no
// waiting row names a bin type.
//
// One rule for every served cell, the station's own process included; and it
// reads the rows of the board's role, so a loader is told what its cells use.
export function changeoverInstruction(tile, view) {
    const loader = loaderOf(tile);
    if (!loader || !loader.directive) return null;
    const role = servedRole(tile);
    const bins = {}, parts = {}, cells = {};
    ((view && view.serves) || []).forEach(function (row) {
        if (!row.waiting || row.role !== role || !row.carrier) return;
        bins[row.carrier] = true;
        parts[row.part] = true;
        cells[row.node] = true;
    });
    const binTypes = Object.keys(bins).sort();
    if (binTypes.length === 0) return null;
    return {
        bin_type_codes: binTypes,
        payload_codes: Object.keys(parts).sort(),
        for_nodes: Object.keys(cells).sort(),
    };
}

// ── the CLEAR panel's highlight ──────────────────────────────────────────────

// clearHighlight is the bin type an unloader's CLEAR panel lights for the cart
// at a window: the window's assignment from Core (tile.assignment, absent until
// Core assigns windows); else a waiting row's bin type; else like for like, the
// cart's own type (for a tote cart its height from its row, once heights
// exist). Nothing when nothing grounds a choice.
export function clearHighlight(tile, serves) {
    const assignment = tile && tile.assignment;
    if (assignment && assignment.bin_type) return assignment.bin_type;
    const role = servedRole(tile);
    const waiting = (serves || []).find(function (row) {
        return row.waiting && row.role === role && !!row.carrier;
    });
    if (waiting) return waiting.carrier;
    const bin = tile && tile.bin_state;
    if (bin && bin.occupied && bin.bin_type_code) return bin.bin_type_code;
    return null;
}

// ── a window's words on the node grid ────────────────────────────────────────

// windowWords is what a loader window's slot says, from what stands in it:
//
//   an unloader window holding a full: the part (a next full on its way does
//   not hide the one to clear);
//   a live order: AWAITING and what it waits for (awaiting, by role);
//   a bin with a part: the part; counted parts with no bin read: LOADED;
//   an empty: EMPTY; nothing there: NO BIN;
//   and no bin read at all (Core could not be asked): COULD NOT LOOK (a bug
//   fixed: it read NO BIN, a fact nobody had established).
export function windowWords(tile, awaiting, hasActiveOrder) {
    const bin = tile.bin_state;
    const binPayload = bin && bin.payload_code ? bin.payload_code : '';
    const remaining = tile.runtime && tile.runtime.remaining_uop_cached != null ? tile.runtime.remaining_uop_cached : 0;
    if (isUnloaderWindow(tile) && bin && bin.occupied && binPayload) return binPayload;
    if (hasActiveOrder) return 'AWAITING ' + awaiting;
    if (binPayload) return binPayload;
    if (remaining > 0) return 'LOADED';
    if (bin && bin.occupied) return 'EMPTY';
    if (!bin) return 'COULD NOT LOOK';
    return 'NO BIN';
}
