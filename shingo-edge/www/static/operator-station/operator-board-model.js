// operator-board-model.js — the loader and unloader board's model: the layout
// a station's tiles make, the parts each loader tile draws and which of them a
// running style uses, each served cell's coverage, the CLEAR panel's
// highlight, and the board a station of loader tiles draws (boardModel) with
// its fit (fitBoard).
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
// its arguments (and operator-window-state.js, pure as well, for the acts the
// old boards offered a part).

'use strict';

import { cardModel, emptyCarrierHeld, clearRunning, CLEAR_RUNNING, isActiveStatus } from './operator-window-state.js';

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

// isUnloaderWindow: a window of a shared unloader (a tap on a full is the
// CLEAR panel, on an empty the empty-cart panel).
export function isUnloaderWindow(tile) {
    const loader = loaderOf(tile);
    return !!loader && loader.role === 'consume' && !isDedicated(tile);
}

// ── the layout ───────────────────────────────────────────────────────────────

// boardLayout is the layout the old boards gave a station's claimed tiles. A
// station whose every tile is a loader's now draws the board (isBoard), which
// keeps kind to give each tap the act the old board gave it (windowAct,
// boardBar); header is read only where loader and press tiles mix, on the node
// grid's header (renderHeader).
//
//   kind   'empty'    no tiles;
//          'homes'    every tile a loader's and one of them dedicated;
//          'payload'  every tile a loader's, none dedicated, not every one an
//                     unloader (the old payload board, with the board bar);
//          'grid'     every other station: a shared unloader's windows (the
//                     old slot grid), or a loader beside a press.
//   header 'pull-full-from-home' on a station with a dedicated loader;
//          'none' on a payload board, and on a station of one loader tile;
//          'changeover' (CHANGEOVER, or CUTOVER and CANCEL) otherwise.
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

// ── the CLEAR panel's highlight ──────────────────────────────────────────────

// clearHighlight is the choice an unloader's CLEAR panel lights for the cart at
// a window, {bin_type, layers?}: the window's assignment from Core
// (tile.assignment, from the group line of the Edge's copy; absent from a Core
// that sends none); else a waiting row's bin type; else like for like, the
// cart's own type, and for a tote cart the height it stands at (heightOf).
// Nothing when nothing grounds a choice. A choice with no button on the panel
// lights nothing (clearChoices, isLit).
export function clearHighlight(tile, serves) {
    const assignment = tile && tile.assignment;
    if (assignment && assignment.bin_type) {
        return typeof assignment.layers === 'number'
            ? { bin_type: assignment.bin_type, layers: assignment.layers } : { bin_type: assignment.bin_type };
    }
    const role = servedRole(tile);
    const waiting = (serves || []).find(function (row) {
        return row.waiting && row.role === role && !!row.carrier;
    });
    if (waiting) return { bin_type: waiting.carrier };
    const bin = tile && tile.bin_state;
    if (bin && bin.occupied && bin.bin_type_code) {
        const h = heightOf(bin);
        return h && h.extra === 0 ? { bin_type: bin.bin_type_code, layers: h.layers } : { bin_type: bin.bin_type_code };
    }
    return null;
}

// ── a cart's height ──────────────────────────────────────────────────────────

// heightOf is the height a cart stands at, from its tote numbers
// (bin_state.totes_on_cart, totes_per_layer): {layers, extra}, extra the totes
// over a whole layer; null when either number is not known.
export function heightOf(bin) {
    if (!bin || typeof bin.totes_on_cart !== 'number' || typeof bin.totes_per_layer !== 'number' || bin.totes_per_layer <= 0) {
        return null;
    }
    return { layers: Math.floor(bin.totes_on_cart / bin.totes_per_layer), extra: bin.totes_on_cart % bin.totes_per_layer };
}

// heightLabel is a cart's height as its spot line says it: "3 high" on an
// exact multiple, "3 high + 2" with two totes over, the bin's name alone at 0
// (a base), "Unknown" with no number, and nothing for a type with no totes per
// layer.
export function heightLabel(bin) {
    if (!bin || typeof bin.totes_per_layer !== 'number') return '';
    if (typeof bin.totes_on_cart !== 'number') return 'Unknown';
    if (bin.totes_on_cart === 0) return bin.bin_type_code || '';
    const h = heightOf(bin);
    return h.layers + ' high' + (h.extra > 0 ? ' + ' + h.extra : '');
}

// choiceLabel is a CLEAR choice's words: the bin's name, with its height
// ("TOTE-1 · 3 high"); the bin's name alone at 0 layers, a base, and no
// "Base" (a "-BARE" type and that word are one at twenty feet).
export function choiceLabel(choice) {
    if (!choice) return '';
    return choice.bin_type + (choice.layers > 0 ? ' · ' + choice.layers + ' high' : '');
}

// wantedHeights is the heights the board's empties group wants of a bin type,
// lowest first: the height rows of view.empties (layers set). None on a group
// with no layer wants, or from a Core that sends no group lines.
export function wantedHeights(view, binType) {
    const rows = (view && view.empties && view.empties.rows) || [];
    const out = [];
    rows.forEach(function (r) {
        if (r.bin_type === binType && typeof r.layers === 'number' && out.indexOf(r.layers) === -1) out.push(r.layers);
    });
    return out.sort(function (a, b) { return a - b; });
}

// clearChoices is what the CLEAR panels offer for a window's bin types: one
// choice per type, {bin_type}; for a type the group wants heights of, in place
// of its one choice, the bin's name (layers 0, a base) and the bin's name with
// each wanted height, {bin_type, layers}. A choice with layers posts them with
// the CLEAR; one without posts none.
export function clearChoices(view, carriers) {
    const out = [];
    (carriers || []).forEach(function (t) {
        const heights = wantedHeights(view, t);
        if (heights.length === 0) { out.push({ bin_type: t }); return; }
        if (heights.indexOf(0) === -1) heights.unshift(0);
        heights.forEach(function (h) { out.push({ bin_type: t, layers: h }); });
    });
    return out;
}

// isLit reports whether a panel's choice is the lit one: the same type, and
// the same height where either names one.
export function isLit(choice, lit) {
    if (!choice || !lit || choice.bin_type !== lit.bin_type) return false;
    const a = typeof choice.layers === 'number' ? choice.layers : null;
    const b = typeof lit.layers === 'number' ? lit.layers : null;
    return a === b;
}

// ── a window's words on the node grid ────────────────────────────────────────

// windowWords is what a loader window's slot says, from what stands in it:
//
//   an unloader window holding a full: the part (a next full on its way does
//   not hide the one to clear);
//   a loader home whose clear runs with its empty still on it: CLEARING (the
//   tile adds the job, CLEAR_RUNNING, below; clearRunning);
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
    if (tileRole(tile) === 'produce' && clearRunning(tile)) return 'CLEARING';
    if (hasActiveOrder) return 'AWAITING ' + awaiting;
    if (binPayload) return binPayload;
    if (remaining > 0) return 'LOADED';
    if (bin && bin.occupied) return 'EMPTY';
    if (!bin) return 'COULD NOT LOOK';
    return 'NO BIN';
}

// ── what the screen knows: the stale sign ────────────────────────────────────

// binsKnown reports whether the view's bins are known: false while the Edge
// holds no copy of Core's node bins from a Core that serves them, or when the
// read over HTTP failed (view.bins.known). A view with no bins key is an
// older Edge's and reads as known, as before. While bins are not known an
// absent bin_state means nothing: no act whose offer reads it is offered, and
// a tile says "could not look".
export function binsKnown(view) {
    return !(view && view.bins && view.bins.known === false);
}

// staleSign is the station's sign, in the top line of every station: its
// words and the header dot. link is {answering, dataAt} (operator-state.js
// getLink); clock(iso) prints a time.
//
//   "Not answering · data from <time>"   a failed view fetch, or the stream down
//   "Bins as of <time>"                  the held copy is not current
//   "Bins not known yet"                 view.bins.known false
//   "Containment not known yet"          view.containment.known false
//   "Catalog as of <time>"               the held payload catalog is not current
//   "Catalog not known yet"              view.catalog.known false
//   "Live"                               none of these
//
// The dot: offline while not answering, stale while anything else is said,
// online when live. Pinned by operator-refresh.test.js.
export function staleSign(view, link, clock) {
    const words = [];
    const answering = !link || link.answering !== false;
    if (!answering) words.push('Not answering' + (link.dataAt ? ' · data from ' + clock(link.dataAt) : ''));
    if (view) {
        if (!binsKnown(view)) words.push('Bins not known yet');
        else if (view.bins && view.bins.as_of) words.push('Bins as of ' + clock(view.bins.as_of));
        if (view.containment && view.containment.known === false) words.push('Containment not known yet');
        if (view.catalog && view.catalog.known === false) words.push('Catalog not known yet');
        else if (view.catalog && view.catalog.as_of) words.push('Catalog as of ' + clock(view.catalog.as_of));
    }
    let dot = 'online';
    if (!answering) dot = 'offline';
    else if (words.length > 0) dot = 'stale';
    return { live: words.length === 0, words: words.length > 0 ? words : ['Live'], dot: dot };
}

// ═════════════════════════════════════════════════════════════════════════════
// The board: a station whose every tile is a loader's
// ═════════════════════════════════════════════════════════════════════════════
//
// What the board draws, decided here and drawn by operator-board.js. Colour is
// an instruction on one ladder: 'red' (act now, the word NOW), 'amber' (act
// soon, SOON) and 'plain' (no word). The reason beside a word is three words
// or fewer.
//
// The card is the button, and it opens what that tile's primary act opened on
// the old boards: an act is {kind, node, ...} and operator-board.js maps each
// kind onto the same panel, so a tap posts what it posted
// (operator-loader-station.test.js pins every post).
//
//   load              the load modal for the parts (openLoadBin)
//   unload            the CLEAR panel (confirmUnloadSwap; Full off at stage 1)
//   push-empty        the empty-cart panel (confirmPushEmpty; Send on when bare)
//   clear-home        the home's clear (confirmClearLoaderHome)
//   modal             the node modal
//   request-empty, request-full, pull-from-market    the board bar's three

// isBoard reports whether a station's tiles draw as the board: every tile
// carries tile.loader. A shared unloader's windows are a board too; a station
// that mixes loader and press tiles keeps the node grid.
export function isBoard(tiles) {
    return tiles.length > 0 && tiles.every(function (t) { return !!loaderOf(t); });
}

// fmtCount writes a count the way the board prints it: "1,027".
export function fmtCount(n) {
    return String(n).replace(/\B(?=(\d{3})+(?!\d))/g, ',');
}

function spotOf(tile) {
    return (tile.node && (tile.node.name || tile.node.core_node_name)) || '';
}

// shortNode drops a group prefix: "GROUP.SMN_A1" is the spot SMN_A1.
function shortNode(name) {
    return String(name || '').replace(/^.*\./, '');
}

// spotWords is what a spot holds, from a closed set: "holds a full bin",
// "holds an empty <bin>" (words "holds an empty", code the bin), "holds an
// empty bin", "no bin" (a cart with no bin on it too), "count unknown" (a bin
// with a part and a count of 0 or below) and "could not look" (no bin read).
export function spotWords(tile) {
    const bin = tile.bin_state;
    if (!bin) return { words: 'could not look', code: '' };
    if (!bin.occupied || bin.bare) return { words: 'no bin', code: '' };
    if (bin.payload_code) return { words: bin.stocked ? 'holds a full bin' : 'count unknown', code: '' };
    if (bin.bin_type_code) return { words: 'holds an empty', code: bin.bin_type_code };
    return { words: 'holds an empty bin', code: '' };
}

function liveOrders(tile) {
    return (tile.orders || []).filter(function (o) { return isActiveStatus(o.status); });
}

// clearMoves is a home's own clear while it runs: the move of the home's
// emptied carrier off the home (Core's clear, on its spot arm, to an open
// buffer spot; pushed to the station as a plain row), live on the home's
// orders. The home draws no Load card until it ends, and the spot it goes to
// is drawn as coming.
function clearMoves(tile) {
    const home = (tile.node && tile.node.core_node_name) || '';
    if (!home || tileRole(tile) !== 'produce') return [];
    return liveOrders(tile).filter(function (o) {
        return o.order_type === 'move' && shortNode(o.source_node) === shortNode(home);
    });
}

// CLEAR_RUNNING (the job a home's card says while its clear runs) and
// clearRunning are declared beside cardModel, so the node grid's tile says the
// same words from the same rule.

// clearRow is the home's clear while it runs: a live move that delivers TO the
// home (Core's clear pushes its parent, home to home, and the partial's move,
// spot to home, to the station), or null. A loader's own moves leave a home
// and never deliver to it, so nothing else reads as a clear.
function clearRow(tile) {
    const home = (tile.node && tile.node.core_node_name) || '';
    if (!home || tileRole(tile) !== 'produce') return null;
    return liveOrders(tile).find(function (o) {
        return o.order_type === 'move' && shortNode(o.delivery_node) === shortNode(home);
    }) || null;
}

// ── what a part is at the served cells ───────────────────────────────────────

// worstRow is the most urgent drawn row of a part among the served rows of one
// role (coverage's order), or null when no cell runs it.
function worstRow(cov, part, role) {
    for (let i = 0; i < cov.length; i++) {
        if (cov[i].row.role === role && cov[i].row.part === part) return cov[i];
    }
    return null;
}

// loadReason is a Load card's colour and reason, from the cell its part
// serves. The colour follows the cell's state, never the kind of demand: none
// of the part (an active row that is out, or an incoming row waiting with
// none) is red; a cell that has asked with some left is amber; the rest is
// plain. rank orders the cards: out, waiting, the count the card prints
// (fewest first), unknown, could not look, a part no cell runs.
function loadReason(view, cov, part) {
    if (!Array.isArray(view.serves)) return { tone: 'plain', reason: 'Cells could not look', rank: [4, 0] };
    const c = worstRow(cov, part, 'consume');
    if (!c) return { tone: 'plain', reason: 'No cell runs this part', rank: [5, 0] };
    const cell = c.row.cell;
    if (c.state === 'out') return { tone: 'red', word: 'NOW', reason: cell + ' out', rank: [0, 0], fixes: cell + '|' + part };
    if (c.state === 'waiting') return { tone: 'amber', word: 'SOON', reason: cell + ' waiting', rank: [1, c.left] };
    if (c.state === 'unknown') return { tone: 'plain', reason: cell + ' count unknown', rank: [3, 0] };
    return { tone: 'plain', reason: cell + ' has ' + fmtCount(c.left) + ' left', rank: [2, c.left] };
}

function byRank(a, b) {
    for (let i = 0; i < Math.max(a.rank.length, b.rank.length); i++) {
        const x = a.rank[i] || 0, y = b.rank[i] || 0;
        if (x !== y) return x - y;
    }
    return a.order - b.order;
}

// ── the cells row (a loader) ─────────────────────────────────────────────────

const MAX_STATUS_TILES = 6;
export const MAX_TILE_ROWS = 4;

// cellsRow is the loader's status row: one tile per served cell, a row per
// drawn consume row (coverage: an incoming row only while it waits). A covered
// row is a bar with no words; exposed and waiting say "N left", out "none
// left", unknown "count unknown". A tile is red only when a red card on this
// board fixes it (fixes holds "cell|part"). Rows: out and exposed first, at
// most four, then "+N". Cells: by the worst drawn row's count, covered rows
// left out, unknown last; past six, the covered cells fold into "+N more
// cells". failed when the served read failed.
export function cellsRow(view, fixes) {
    if (!Array.isArray(view.serves)) return { failed: true, tiles: [], more: 0 };
    fixes = fixes || {};
    const cells = [];
    const byCell = {};
    coverage(view).forEach(function (c) {
        if (c.row.role !== 'consume') return;
        let cell = byCell[c.row.cell];
        if (!cell) {
            cell = { cell: c.row.cell, rows: [], first: c.i };
            byCell[c.row.cell] = cell;
            cells.push(cell);
        }
        cell.first = Math.min(cell.first, c.i);
        let words = fmtCount(c.left) + ' left';
        if (c.state === 'out') words = 'none left';
        else if (c.state === 'unknown') words = 'count unknown';
        else if (c.state === 'covered') words = '';
        cell.rows.push({
            part: c.row.part, state: c.state, words: words,
            left: c.left, capacity: c.row.capacity_uop,
            tick: c.row.reorder_uop > 0 ? c.row.reorder_uop : 0,
            red: c.state === 'out' && !!fixes[c.row.cell + '|' + c.row.part],
        });
    });
    cells.forEach(function (cell) {
        let worst = Infinity;
        let unknown = false;
        cell.rows.forEach(function (r) {
            if (r.state === 'unknown') unknown = true;
            else if (r.state !== 'covered') worst = Math.min(worst, r.left);
        });
        cell.group = worst !== Infinity ? 0 : (unknown ? 2 : 1);
        cell.worst = worst === Infinity ? 0 : worst;
        cell.red = cell.rows.some(function (r) { return r.red; });
        cell.covered = cell.rows.every(function (r) { return r.state === 'covered'; });
        cell.hidden = Math.max(0, cell.rows.length - MAX_TILE_ROWS);
        cell.rows = cell.rows.slice(0, MAX_TILE_ROWS);
    });
    cells.sort(function (a, b) {
        if (a.group !== b.group) return a.group - b.group;
        if (a.worst !== b.worst) return a.worst - b.worst;
        return a.first - b.first;
    });
    let tiles = cells;
    let more = 0;
    if (cells.length > MAX_STATUS_TILES) {
        tiles = cells.filter(function (c) { return !c.covered; });
        more = cells.length - tiles.length;
    }
    return {
        failed: false, more: more,
        tiles: tiles.map(function (c) {
            return { cell: c.cell, rows: c.rows, hidden: c.hidden, red: c.red, covered: c.covered };
        }),
    };
}

// ── the presses row and the empties (an unloader) ────────────────────────────

// emptiesOf is the board's empties group, view.empties {group, rows:
// [{bin_type, have, spots, want?}]}: null when there is none to draw (no group
// read, or a stage-1 unloader, whose outbound group is stage 2's windows and
// not an empties source).
function emptiesOf(view, tiles) {
    const e = view.empties;
    if (!e || !Array.isArray(e.rows)) return null;
    if (tiles.some(function (t) { return !!loaderOf(t).leaves_bare; })) return null;
    return e;
}

// typeRow is a bin type's own row of the empties (a height's row carries
// layers), or null.
function typeRow(e, binType) {
    return e.rows.find(function (r) { return r.bin_type === binType && typeof r.layers !== 'number'; }) || null;
}

function emptiesHave(e, binType) {
    const r = typeRow(e, binType);
    return r ? r.have || 0 : 0;
}

// pressesRow is the unloader's status row: one tile per running produce row,
// "N of M" or "full", its bin type the headline, its part; not ready ("no
// empty ready") when the empties hold none of its type. Closest to full first,
// a count the Edge cannot see last; past six tiles the ready ones fold. Then
// the Empties tile, in words: a line per bin type a served press uses, each
// once (a type the group holds that no served press uses gets none), "N ready"
// or "none" ("N of M" where the group wants M), and under it, small, a line
// per height the group wants of it ("TOTE-1 · 3 high  1 of 2"); the group's
// open spots once (every row carries the group's count); its name.
export function pressesRow(view, tiles) {
    const e = emptiesOf(view, tiles);
    const rows = (view.serves || []).filter(function (r) { return r.role === 'produce' && !r.incoming; });
    const presses = rows.map(function (r, i) {
        const known = typeof r.have_uop === 'number' && r.have_uop >= 0;
        const full = known && r.capacity_uop > 0 && r.have_uop >= r.capacity_uop;
        let words = 'count unknown';
        if (known) words = full ? 'full' : fmtCount(r.have_uop) + ' of ' + fmtCount(r.capacity_uop);
        return {
            cell: r.cell, node: shortNode(r.node), bin: r.carrier, part: r.part,
            have: known ? r.have_uop : null, capacity: r.capacity_uop, words: words,
            full: full, ready: !!e && emptiesHave(e, r.carrier) > 0, red: false,
            room: known ? r.capacity_uop - r.have_uop : Infinity, i: i,
        };
    });
    presses.sort(function (a, b) {
        if (a.full !== b.full) return a.full ? -1 : 1;
        if (a.room !== b.room) return a.room - b.room;
        return a.i - b.i;
    });
    let empties = null;
    if (e) {
        const types = [];
        rows.forEach(function (r) { if (r.carrier && types.indexOf(r.carrier) === -1) types.push(r.carrier); });
        let open = 0;
        e.rows.forEach(function (r) { open = Math.max(open, r.spots || 0); });
        empties = {
            group: e.group || '', open: open,
            lines: [],
        };
        types.forEach(function (t) {
            const have = emptiesHave(e, t);
            const typed = typeRow(e, t);
            const want = typed && typeof typed.want === 'number' ? typed.want : null;
            empties.lines.push({ bin: t, words: want !== null ? have + ' of ' + want : (have > 0 ? have + ' ready' : 'none') });
            // Per height, small, where the plant set layer wants.
            e.rows.forEach(function (r) {
                if (r.bin_type !== t || typeof r.layers !== 'number') return;
                empties.lines.push({ bin: choiceLabel({ bin_type: t, layers: r.layers }), small: true,
                    words: (r.have || 0) + (typeof r.want === 'number' ? ' of ' + r.want : ' ready') });
            });
        });
    }
    return { all: presses, empties: empties };
}

// foldPresses keeps the status row to six tiles: past six, the ready presses
// that are not full fold into "+N more".
function foldPresses(presses) {
    if (presses.length <= MAX_STATUS_TILES) return { tiles: presses, more: 0 };
    const tiles = presses.filter(function (p) { return p.full || !p.ready; });
    return { tiles: tiles, more: presses.length - tiles.length };
}

// ── the buffer (a dedicated loader's partial spots) ──────────────────────────

// bufferOf is the loader's buffer spots, tile.loader.buffer [{node, state}] in
// the plant's order (state partial | empty | open | unknown; unknown is a spot
// the board cannot place: a count of 0 or below, no payload, or not read), or
// null when the loader has none. A spot a clear is moving a home's carrier to
// is 'coming': it ends holding an empty. Partials are numbered in order.
export function bufferOf(tiles) {
    let spots = null;
    tiles.forEach(function (t) {
        const loader = loaderOf(t);
        if (!spots && loader && Array.isArray(loader.buffer) && loader.buffer.length > 0) spots = loader.buffer;
    });
    if (!spots) return null;
    const coming = {};
    tiles.forEach(function (t) {
        clearMoves(t).forEach(function (o) { coming[shortNode(o.delivery_node)] = true; });
    });
    let n = 0;
    return spots.map(function (s) {
        const node = shortNode(s.node);
        const state = coming[node] ? 'coming' : s.state;
        return { node: node, state: state, number: state === 'partial' ? ++n : 0 };
    });
}

// partialsState grades the buffer on what a clear makes, an empty: red when it
// holds a partial and no spot is empty or open ("NOW · Buffer full · clear
// one"); amber when it holds a partial and no empty ("SOON · No empty");
// plain, and folded, otherwise. An unknown spot is neither open nor empty; a
// coming one counts as an empty.
export function partialsState(spots) {
    const n = { partial: 0, empty: 0, open: 0, unknown: 0 };
    spots.forEach(function (s) {
        if (s.state === 'partial') n.partial++;
        else if (s.state === 'empty' || s.state === 'coming') n.empty++;
        else if (s.state === 'open') n.open++;
        else n.unknown++;
    });
    let tone = 'plain';
    if (n.partial > 0 && n.empty === 0) tone = n.open === 0 ? 'red' : 'amber';
    const words = [n.partial + (n.partial === 1 ? ' partial' : ' partials'), n.empty + ' empty', n.open + ' open'];
    if (n.unknown > 0) words.push(n.unknown + ' ?');
    return { tone: tone, counts: n, words: words.join(' · ') };
}

// ── the acts ─────────────────────────────────────────────────────────────────

// partAct is the old boards' act for a part on a loader tile (cardModel):
// 'load', 'unload' or '' for none.
function partAct(tile, code) {
    return cardModel(tile, code).action;
}

function unloadAct(tile, lit) {
    const bin = tile.bin_state || {};
    // A HELD partial, waiting in its partial home for its line, is not cleared
    // on one tap: the count is what makes it worth calling back. Its tap is
    // UNLOAD and COUNT (operator-render.js confirmUnloadHeld), with Core's count
    // of it (bin.held_uop, which does not tick while it waits).
    if (bin.held_since) {
        return { kind: 'unload-held', node: tile.node.id, heldFor: bin.held_for || '', count: bin.held_uop || 0 };
    }
    return {
        kind: 'unload', node: tile.node.id, carriers: carriersOf(tile),
        discarding: (tile.runtime && tile.runtime.remaining_uop_cached) || 0,
        leavesBare: !!loaderOf(tile).leaves_bare, lit: lit,
    };
}

// windowAct is what a tap on a cart at an unloader window opened before:
// where every window was a shared unloader's (the old node grid), a full the
// CLEAR and an empty the empty-cart panel; on a payload or home board the
// card's act (cardModel), the CLEAR for either.
function windowAct(tile, kind, lit) {
    const bin = tile.bin_state || {};
    if (kind === 'grid') {
        if (bin.payload_code) return unloadAct(tile, lit);
        return { kind: 'push-empty', node: tile.node.id, carriers: carriersOf(tile), bare: !!bin.bare, lit: lit };
    }
    const codes = bin.payload_code ? [bin.payload_code] : partCodes(tile);
    if (codes.some(function (c) { return partAct(tile, c) === 'unload'; })) return unloadAct(tile, lit);
    return { kind: 'modal', node: tile.node.id };
}

// refusalsOf is a loader tile's standing refusals, by part.
function refusalsOf(tile) {
    return tile.supply_refusals || {};
}

// bufferPartialOf is the buffer spot holding a partial this home can pull back
// (tile.buffer_partial {node}), or null.
function bufferPartialOf(tile) {
    return tile.buffer_partial || null;
}

// alarmOf is a tile's alarm sentence: stranded counts, or a contained payload.
function alarmOf(tile) {
    if (tile.stranded_alarm) return tile.stranded_alarm;
    if (tile.containment_flagged) return 'Contained: its next delivery goes to the hold spot';
    return '';
}

function headlineOf(tile) {
    const bin = tile.bin_state || {};
    if (tileRole(tile) === 'consume') return bin.bin_type_code || partCodes(tile)[0] || '';
    return bin.payload_code || partCodes(tile)[0] || '';
}

// ── the loader's cards ───────────────────────────────────────────────────────

// loaderCards sorts a loader tile's work into its zones: Load (one card per
// home and part, or per shared window listing the parts called for, the first
// by coverage lit), Waiting on the plant (a standing refusal; its tap opens the
// load modal, where Undo and LOAD are), Clear partials (tile.buffer_partial)
// and Check bin (a bin with a part and a count of 0 or below).
function loaderCards(t, ctx) {
    const view = ctx.view;
    const spot = spotWords(t);
    const refusals = refusalsOf(t);
    const inFlight = clearMoves(t).length > 0;
    // The empty on the home is an unfinished order's: nothing here offers LOAD
    // on it (emptyCarrierHeld), and a running clear's card says the job.
    const held = emptyCarrierHeld(t);
    const loadable = [];
    cardParts(t, view.serves).forEach(function (code) {
        if (refusals[code]) {
            const c = worstRow(ctx.cov, code, 'consume');
            const exposes = !!c && (c.state === 'out' || c.state === 'waiting' || c.state === 'exposed');
            const when = ctx.when(refusals[code].refused_at);
            let reason = 'Said no parts' + (when ? ' · ' + when : '');
            if (exposes) reason += ' · ' + c.row.cell + ' has ' + (c.left > 0 ? fmtCount(c.left) : 'none') + ' left';
            ctx.refused.push(ctx.card(t, 'refused:' + code, [exposes ? 0 : 1], {
                spotWords: spot.words, spotCode: spot.code, headline: code, reason: reason,
                act: held ? { kind: 'modal', node: t.node.id } : { kind: 'load', node: t.node.id, codes: [code] },
            }));
            return;
        }
        if (!inFlight && !held && ctx.known && partAct(t, code) === 'load') loadable.push(code);
    });
    const clearing = held && ctx.known ? clearRow(t) : null;
    if (clearing) {
        ctx.load.push(ctx.card(t, 'clearing', [1], {
            spotWords: spot.words, spotCode: spot.code, headline: clearing.payload_code || cardParts(t, view.serves)[0] || '',
            reason: CLEAR_RUNNING, job: true, act: { kind: 'modal', node: t.node.id },
        }));
    }
    if (loadable.length > 0) {
        const lit = loadable.map(function (code, i) { return { code: code, r: loadReason(view, ctx.cov, code), order: i }; });
        lit.sort(function (a, b) { return byRank(a.r, b.r) || a.order - b.order; });
        const first = lit[0].r;
        const card = ctx.card(t, 'load', first.rank, {
            tone: first.tone, word: first.word || '', reason: first.reason,
            spotWords: spot.words, spotCode: spot.code, headline: lit[0].code,
            act: { kind: 'load', node: t.node.id, codes: lit.map(function (x) { return x.code; }) },
        });
        if (lit.length > 1) card.also = lit.slice(1).map(function (x) { return x.code; });
        if (first.fixes) card.fixes = first.fixes;
        ctx.load.push(card);
    }
    const bp = bufferPartialOf(t);
    const bin = t.bin_state || {};
    if (!ctx.known) return;
    if (bp && bp.node && !inFlight && bin.occupied && bin.payload_code) {
        const from = shortNode(bp.node);
        ctx.partials.push(ctx.card(t, 'partial', [0], {
            spotWords: 'holds a full bin', headline: bin.payload_code,
            reason: 'Pull the full · partial from ' + from, partialSpot: from,
            act: { kind: 'clear-home', node: t.node.id, home: spotOf(t), spot: from },
        }));
    } else if (loadable.length === 0 && bin.occupied && bin.payload_code && !bin.stocked) {
        ctx.other('Check bin', ctx.card(t, 'check', [0], {
            spotWords: 'count unknown', headline: bin.payload_code, reason: 'Count unknown · check bin',
            act: { kind: 'modal', node: t.node.id },
        }));
    }
}

// ── the unloader's cards ─────────────────────────────────────────────────────

// windowCards is the unloader's one list: one card per cart at a window, the
// bin's name the headline, what to send back the note. The note is "Change to
// <bin>" on any cart whose send-back type is not its own, or whose send-back
// height is not the one it stands at; "Send back <bin>" on
// a full of its own type and "Send it" on an empty of its own type, these two
// only on a station that handles more than one bin type. What to send back is
// clearHighlight's: the window's assignment, else a waiting press's bin type,
// else the cart's own; the note names it with its height where it has one
// ("Change to TOTE-1 · 3 high"). The cart's own height rides the spot line
// (heightLabel). A delivered order with no cart yet is the old boards' Unload
// card.
function windowCards(consumers, ctx) {
    // While the bins are not known no cart is drawn: what stands at a window is
    // what each card and its act read.
    if (!ctx.known) return;
    const types = {};
    consumers.forEach(function (t) { carriersOf(t).forEach(function (c) { types[c] = true; }); });
    const multi = Object.keys(types).length > 1;
    consumers.forEach(function (t) {
        const bin = t.bin_state;
        const loader = loaderOf(t);
        if (!bin || !bin.occupied) {
            if (ctx.kind === 'grid') return;
            const code = partCodes(t).find(function (c) { return partAct(t, c) === 'unload'; });
            if (code) {
                ctx.other('Unload', ctx.card(t, 'unload', [0], {
                    spotWords: 'no bin', headline: code, act: unloadAct(t, null),
                }));
            }
            return;
        }
        const full = !!bin.payload_code;
        const own = bin.bin_type_code || '';
        const lit = loader.leaves_bare ? null : clearHighlight(t, ctx.view.serves);
        const back = lit ? lit.bin_type : '';
        // A change is another type, or a height the cart does not stand at.
        const at = heightOf(bin);
        const atLayers = at && at.extra === 0 ? at.layers : null;
        const changes = !!back && !!own && (back !== own || (typeof lit.layers === 'number' && lit.layers !== atLayers));
        let note = '';
        if (full && bin.held_since) {
            const since = ctx.when(bin.held_since);
            note = 'Partial · held for ' + (bin.held_for || 'its line') + (since ? ' · since ' + since : '');
        } else if (loader.pulled_directly && !full) note = 'The cart waits here for the line';
        else if (changes) note = 'Change to ' + choiceLabel(lit);
        else if (back && multi) note = full ? 'Send back ' + choiceLabel(lit) : 'Send it';
        ctx.windows.push(ctx.card(t, 'cart', [1], {
            spotWords: full ? 'holds a full bin' : (bin.bare ? 'no bin' : 'holds an empty bin'),
            spotPart: full ? bin.payload_code : '',
            spotCount: full ? fmtCount((t.runtime && t.runtime.remaining_uop_cached) || 0) : '',
            spotHeight: bin.bare ? '' : heightLabel(bin),
            headline: own || bin.bin_label || 'Unknown bin', reason: note,
            back: back, own: own, full: full,
            act: windowAct(t, ctx.kind, lit),
        }));
    });
}

// lightStoppedPresses gives each stopped press one red: a full press with no
// empty of its bin ready lights the first cart, carts holding an empty first
// and then fulls, each in the plant's order, whose send-back bin is the one
// it needs ("NOW · Change to <bin>" or "NOW · Send back <bin>"). The press's
// tile is red with it; every other cart stays plain.
function lightStoppedPresses(presses, windows) {
    const walk = windows.slice().sort(function (a, b) {
        if (a.full !== b.full) return a.full ? 1 : -1;
        return a.order - b.order;
    });
    presses.forEach(function (p) {
        if (!p.full || p.ready || !p.bin) return;
        const cart = walk.find(function (c) { return c.tone === 'plain' && c.back === p.bin; });
        if (!cart) return;
        cart.tone = 'red';
        cart.word = 'NOW';
        cart.reason = (cart.own && cart.own !== p.bin ? 'Change to ' : 'Send back ') + p.bin;
        cart.rank = [0];
        p.red = true;
    });
}

// ── the board ────────────────────────────────────────────────────────────────

// boardModel is everything the board draws, from the view:
//
//   title, words   the top line: the station, its counts by zone, then which
//                  cell is out or full and the buffer's state while it is red
//                  or amber
//   tileCount      Show all's N
//   sign           the station's stale sign (staleSign), from ui
//   bar            the board bar (boardBar), or null
//   cells          the loader's status row (cellsRow), or null
//   presses        the unloader's status row and its Empties tile, or null
//   zones          [{key, title, count, tone, word, reason, cards, empty,
//                  folded, openedByBoard, strip, stripWords}] in order: Load,
//                  Waiting on the plant, At the windows, the catch-all zones
//                  (each titled by its act), Clear partials
//   all            every tile's cards in the plant's order (Show all); a tile
//                  with no card gets a plain one opening the node modal
//
// ui: {partialsOpen: the partials opened by a tap, when(iso): the time a
// refusal was said, sign: the station's stale sign}.
//
// WHILE THE BINS ARE NOT KNOWN (binsKnown) the board claims nothing about what
// stands at a spot: no Load card, no cart at a window, no partial or Check bin
// card (each card and its act read bin_state); Load, At the windows and an
// opened Clear partials say "Could not look" in place of their empty
// sentence, and neither their titles nor the top line give a count. Waiting on the plant, the
// cells row and the strip ("?") stay. Pinned by operator-board-model.test.js.
export function boardModel(view, ui) {
    ui = ui || {};
    const tiles = (view.nodes || []).filter(function (t) { return !!loaderOf(t); });
    const producers = tiles.filter(function (t) { return tileRole(t) === 'produce'; });
    const consumers = tiles.filter(function (t) { return tileRole(t) === 'consume'; });
    const otherCards = {}, otherOrder = [];
    const ctx = {
        view: view, cov: coverage(view), kind: boardLayout(tiles).kind, known: binsKnown(view),
        when: ui.when || function () { return ''; },
        load: [], refused: [], partials: [], windows: [],
        other: function (label, card) {
            if (!otherCards[label]) { otherCards[label] = []; otherOrder.push(label); }
            otherCards[label].push(card);
        },
        card: function (t, key, rank, o) {
            const i = tiles.indexOf(t);
            return Object.assign({
                key: key + ':' + t.node.id, tile: i, order: i, rank: rank, tone: 'plain', word: '', reason: '',
                spot: spotOf(t), spotWords: '', spotCode: '',
            }, o);
        },
    };
    producers.forEach(function (t) { loaderCards(t, ctx); });
    ctx.load.sort(byRank);
    ctx.refused.sort(byRank);

    const words = [];
    let presses = null;
    if (consumers.length > 0) {
        windowCards(consumers, ctx);
        const pr = pressesRow(view, tiles);
        lightStoppedPresses(pr.all, ctx.windows);
        ctx.windows.sort(byRank);
        pr.all.forEach(function (p) { if (p.full) words.push(p.cell + ' full'); });
        const fold = foldPresses(pr.all);
        presses = { tiles: fold.tiles, more: fold.more, empties: pr.empties };
    }

    // Alarms: the sentence is its card's note; a tile with no card of its own
    // gets one in the board's first zone, which opens the node modal.
    const cards = ctx.load.concat(ctx.refused, ctx.partials, ctx.windows);
    otherOrder.forEach(function (l) { cards.push.apply(cards, otherCards[l]); });
    tiles.forEach(function (t, i) {
        const alarm = alarmOf(t);
        if (!alarm) return;
        const mine = cards.filter(function (c) { return c.tile === i; });
        if (mine.length > 0) { mine.forEach(function (c) { c.note = alarm; }); return; }
        const spot = spotWords(t);
        const card = ctx.card(t, 'alarm', [9], {
            spotWords: spot.words, spotCode: spot.code, headline: headlineOf(t), note: alarm,
            act: { kind: 'modal', node: t.node.id },
        });
        (producers.length > 0 ? ctx.load : ctx.windows).push(card);
        cards.push(card);
    });

    // The buffer.
    const spots = bufferOf(tiles);
    let partials = null;
    if (spots) {
        const st = partialsState(spots);
        ctx.partials.forEach(function (c) {
            const s = spots.find(function (x) { return x.node === c.partialSpot; });
            c.number = s ? s.number : 0;
        });
        ctx.partials.sort(function (a, b) { return (a.number || 99) - (b.number || 99) || a.order - b.order; });
        const opened = st.tone !== 'plain' || !!ui.partialsOpen;
        partials = {
            key: 'partials', title: 'Clear partials', count: ctx.partials.length, tone: st.tone,
            word: st.tone === 'red' ? 'NOW' : (st.tone === 'amber' ? 'SOON' : ''),
            reason: st.tone === 'red' ? 'Buffer full · clear one' : (st.tone === 'amber' ? 'No empty' : ''),
            cards: opened ? ctx.partials : [], folded: !opened, openedByBoard: st.tone !== 'plain',
            strip: spots, stripWords: st.words,
            empty: opened && ctx.partials.length === 0 ? (ctx.known ? 'No partial can come home' : 'Could not look') : '',
        };
        if (st.tone === 'red') words.push('Buffer full');
        else if (st.tone === 'amber') words.push('Buffer: no empty');
    }

    // A running clear's card is the home's job, not a load to do.
    const toLoad = ctx.load.filter(function (c) { return !c.job; }).length;
    const zones = [];
    if (producers.length > 0) {
        zones.push({ key: 'load', title: 'Load', count: ctx.known ? toLoad : '', tone: 'plain', cards: ctx.load,
            empty: ctx.load.length === 0 ? (ctx.known ? 'Nothing to load right now.' : 'Could not look') : '' });
        if (ctx.refused.length > 0) {
            zones.push({ key: 'refused', title: 'Waiting on the plant', count: ctx.refused.length, tone: 'plain', cards: ctx.refused });
        }
    }
    if (consumers.length > 0) {
        zones.push({ key: 'windows', title: 'At the windows', count: ctx.known ? ctx.windows.length : '', tone: 'plain', cards: ctx.windows,
            empty: ctx.windows.length === 0 ? (ctx.known ? 'No cart at any window.' : 'Could not look') : '' });
    }
    otherOrder.forEach(function (l) {
        zones.push({ key: 'other:' + l, title: l, count: otherCards[l].length, tone: 'plain', cards: otherCards[l] });
    });
    if (partials) zones.push(partials);

    // The cells row: red on a cell only where a red card fixes it; the top
    // line names every cell that is out, card or none.
    let cells = null;
    if (producers.length > 0) {
        const fixes = {};
        ctx.load.forEach(function (c) { if (c.tone === 'red' && c.fixes) fixes[c.fixes] = true; });
        cells = cellsRow(view, fixes);
        const out = [];
        ctx.cov.forEach(function (c) {
            if (c.row.role === 'consume' && c.state === 'out' && out.indexOf(c.row.cell) === -1) out.push(c.row.cell);
        });
        out.reverse().forEach(function (cell) { words.unshift(cell + ' out'); });
        if (cells.failed) words.push('Cells: could not look');
    }

    const counts = [];
    if (producers.length > 0) {
        if (ctx.known) counts.push(toLoad + ' to load');
        if (ctx.refused.length > 0) counts.push(ctx.refused.length + ' waiting on the plant');
    }
    if (consumers.length > 0 && ctx.known) counts.push(ctx.windows.length > 0 ? ctx.windows.length + ' at the windows' : 'nothing at the windows');
    otherOrder.forEach(function (l) { counts.push(otherCards[l].length + ' ' + l.toLowerCase()); });
    if (spots) {
        const p = spots.filter(function (s) { return s.state === 'partial'; }).length;
        counts.push(p + (p === 1 ? ' partial' : ' partials'));
    }

    const byTile = tiles.map(function () { return []; });
    cards.forEach(function (c) { byTile[c.tile].push(c); });
    const all = [];
    tiles.forEach(function (t, i) {
        if (byTile[i].length > 0) { all.push.apply(all, byTile[i]); return; }
        const spot = spotWords(t);
        all.push(ctx.card(t, 'tile', [0], {
            spotWords: spot.words, spotCode: spot.code, headline: headlineOf(t),
            act: { kind: 'modal', node: t.node.id },
        }));
    });

    return {
        title: (view.station && view.station.name) || '',
        words: counts.concat(words),
        tileCount: tiles.length,
        bar: boardBar(tiles, ctx.kind, view),
        sign: ui.sign || null,
        cells: cells,
        presses: presses,
        zones: zones,
        all: all,
    };
}

// boardBar is the old payload board's bar, wherever that board was drawn (a
// board of shared windows, not every one an unloader's): REQUEST EMPTY on a
// loader, REQUEST FULL with the first part the board lists on an unloader,
// offered while a window of the loader's role holds no bin and nothing is
// coming to it, and posting for the first window (the server stages at the
// free one); PULL FROM MARKET on a loader with an outbound destination. The
// same doors and guards as that board, in sentence case. While the bins are not
// known its request is not offered ("could not look"): whether a window holds a
// bin is what it asks (a bug fixed: an unread window offered the request).
function boardBar(tiles, kind, view) {
    if (kind !== 'payload') return null;
    const entry = tiles[0];
    const role = tileRole(entry);
    const isProduce = role === 'produce';
    const windows = tiles.filter(function (t) { return tileRole(t) === role; });
    const holdsBin = function (t) { return !!(t.bin_state && t.bin_state.occupied); };
    const known = binsKnown(view);
    const canRequest = known && windows.some(function (t) { return !holdsBin(t) && liveOrders(t).length === 0; });
    const payload = isProduce ? '' : (cardParts(entry, view.serves)[0] || '');
    if (!isProduce && !payload) return null;
    const label = isProduce ? 'Request empty' : 'Request full';
    let why = windows.every(holdsBin) ? 'bin at node' : 'bin already inbound';
    if (!known) why = 'could not look';
    const buttons = [{
        key: 'request', enabled: canRequest,
        label: canRequest ? label : label + ' — ' + why,
        act: isProduce ? { kind: 'request-empty', node: entry.node.id } : { kind: 'request-full', node: entry.node.id, payload: payload },
    }];
    if (isProduce && loaderOf(entry).outbound_dest) {
        buttons.push({ key: 'market', enabled: true, label: 'Pull from market', act: { kind: 'pull-from-market', node: entry.node.id } });
    }
    return { buttons: buttons };
}

// ═════════════════════════════════════════════════════════════════════════════
// Fit: one step for the whole board, columns from the width
// ═════════════════════════════════════════════════════════════════════════════
//
// The board does not scroll (Show all is the one view that may). fitBoard picks
// the largest step at which every zone fits; where none does, the last zone
// gives way first to a "+N more" card, then the one before it. Pinned, with
// the clip proofs, by operator-board-model.test.js ("fit").

// STEPS are the board's three sizes: a card's height (its least: a card with
// more lines than that holds grows, cardHeight) and headline, a section title
// and its header row, the spot line, the note, and the gap between a card's
// lines. On a card the word (NOW, SOON) is 20 px and its reason 17 px at every
// step; the reason's line is 24 px and the "Also" line 17 px.
export const STEPS = [
    { card: 120, headline: 32, title: 24, header: 34, spot: 15, note: 15, lineGap: 5 },
    { card: 98, headline: 26, title: 20, header: 30, spot: 14, note: 14, lineGap: 5 },
    { card: 74, headline: 20, title: 18, header: 28, spot: 13, note: 13, lineGap: 1 },
];

// GEOM is the board's fixed geometry in px, shared with operator.css's
// .os-pc-* rules: the page's padding, the top line, the gap between bands, the
// board bar, a section's padding and widest outline, the gap under its header
// and between cards, a card's padding, widest outline, chevron and the
// numbered badge of a partial, a folded section (its Show button is 48 px),
// the partials header's strip box and Show button, the partials section's line
// under its header when no partial can come home, and the status row's label
// with the gap under it, a tile's padding, its head (outline, padding, name)
// and each line under it with the gap above it (a row with its words beside
// the bar or under it, a covered row's bare bar, the "+N"), the press tile and
// an Empties line. Measured on the glass in headless Chrome at each step.
export const GEOM = {
    padX: 20, padTop: 14, padBottom: 16, topLine: 40, gap: 12, bar: 56,
    sectionPadX: 12, sectionPadY: 22, sectionBorder: 3, headerGap: 10, cardGap: 10,
    cardPadX: 14, cardPadY: 2, cardBorder: 3, chevron: 26, badge: 48, folded: 72, stripBox: 36, toggle: 48, zoneLine: 21,
    reasonLine: 24, alsoLine: 17,
    statusLabel: 27, tilePad: 27, tileGap: 10, tileHead: 43, rowLine: 37, rowUnder: 56, rowBar: 14, rowMore: 23,
    pressTile: 132, emptiesLine: 25, emptiesFoot: 46,
};

// columnsFor is the card columns at a width: 2 under 1100 px, 3 from 1100, 4
// from 1700.
export function columnsFor(width) {
    if (width >= 1700) return 4;
    if (width >= 1100) return 3;
    return 2;
}

// textWidth is a string's width in px at a font size, weight 600, measured
// high on purpose: per class of character, between the station sans's mean
// and its widest (headless Chrome, 600 weight: capitals 0.62 em mean and 0.77
// widest, digits 0.58), so a fit that passes here does not clip on the glass.
export function textWidth(s, px) {
    let em = 0;
    String(s || '').split('').forEach(function (ch) {
        if ('MWmw@%'.indexOf(ch) !== -1) em += 0.97;
        else if ("ijlI.,:;|!`'".indexOf(ch) !== -1) em += 0.31;
        else if (ch === ' ') em += 0.28;
        else if (ch === '·') em += 0.25;
        else if (ch === '-' || ch === '_') em += 0.42;
        else if (ch >= '0' && ch <= '9') em += 0.58;
        else if (ch >= 'A' && ch <= 'Z') em += 0.68;
        else if (ch >= 'a' && ch <= 'z') em += 0.56;
        else em += 0.68;
    });
    return Math.ceil(em * px);
}

// cardInnerWidth is the width a card's text column has at a viewport width and
// column count: the section's width shared by the columns, less the card's
// padding, outline and chevron, and the badge on a numbered card.
export function cardInnerWidth(width, cols, numbered) {
    const g = GEOM;
    const section = width - 2 * g.padX - 2 * (g.sectionPadX + g.sectionBorder);
    const card = (section - (cols - 1) * g.cardGap) / cols;
    return Math.floor(card - 2 * g.cardPadX - 2 * g.cardBorder - g.chevron - (numbered ? g.badge : 0));
}

// cardFits reports whether a card's part number or bin code and its spot
// line's names fit unclipped at a step: the headline whole, and on the spot
// line the spot, the bin code and an unloader's part, count and height whole
// (the words between them give way, never a code); and its note, which wraps
// and is never cut, in two lines or fewer.
export function cardFits(card, step, inner) {
    const s = STEPS[step];
    if (textWidth(card.headline, s.headline) > inner) return false;
    if (noteLines(card, step, inner) > 2) return false;
    const fixed = [card.spot, card.spotCode, card.spotPart, card.spotCount, card.spotHeight].filter(Boolean).join(' · ');
    return textWidth(fixed + ' · ', s.spot) <= inner;
}

// noteLines is the lines a card's note wraps to at a step, measured high (a
// wrap breaks at a word, so a line holds nine tenths of the column).
export function noteLines(card, step, inner) {
    if (!card.note) return 0;
    return Math.max(1, Math.ceil(textWidth(card.note, STEPS[step].note) / (inner * 0.9)));
}

// cardHeight is a card's height at a step: the step's, or its lines' where
// they need more (a long note at the smallest step). The lines are the spot
// line, the headline, the reason, the "Also" line and the note's lines, with
// the step's gap between them (operator.css's line-heights: 1.2, the headline
// 1.05).
export function cardHeight(card, step, inner) {
    const g = GEOM;
    const s = STEPS[step];
    let h = Math.ceil(s.spot * 1.2) + s.lineGap + Math.ceil(s.headline * 1.05);
    if (card.word || card.reason) h += s.lineGap + g.reasonLine;
    if (card.also && card.also.length) h += s.lineGap + g.alsoLine;
    if (card.note) h += s.lineGap + noteLines(card, step, inner) * Math.ceil(s.note * 1.2);
    return Math.max(s.card, h + 2 * (g.cardPadY + g.cardBorder));
}

function zoneRows(n, cut, cols) {
    const slots = n + (cut > 0 ? 1 : 0);
    return Math.ceil(slots / cols);
}

// cardRowsHeight is the height of a zone's drawn cards: each row as tall as
// its tallest card (cardHeight; a "+N more" card is the step's), the gaps
// between rows.
function cardRowsHeight(cards, shown, cut, step, cols, width) {
    const slots = cards.slice(0, shown).map(function (c) {
        return cardHeight(c, step, cardInnerWidth(width, cols, !!c.number));
    });
    if (cut > 0) slots.push(STEPS[step].card);
    let h = 0;
    let rows = 0;
    for (let i = 0; i < slots.length; i += cols) {
        h += Math.max.apply(null, slots.slice(i, i + cols));
        rows++;
    }
    return h + (rows - 1) * GEOM.cardGap;
}

// zoneHeight is a section's height with shown of its cards drawn. A zone cut
// to no card still draws its "+N more" card, one row. The partials header
// holds the strip's boxes, and its Show / Hide button unless the board opened
// it; with no card, its empty sentence is a line under the header.
function zoneHeight(z, shown, step, cols, width) {
    const g = GEOM;
    const s = STEPS[step];
    if (z.folded) return g.folded;
    let header = s.header;
    if (z.strip) header = Math.max(header, z.openedByBoard ? g.stripBox : g.toggle);
    // The outline by tone: red 3 px, amber 2, plain 1 (operator.css); a zone
    // that names no tone is counted at the widest.
    const border = { plain: 1, amber: 2, red: 3 }[z.tone] || g.sectionBorder;
    const base = 2 * border + g.sectionPadY + header;
    const cut = z.cards.length - shown;
    if (shown === 0 && cut === 0) return base + (z.strip && z.empty ? g.headerGap + g.zoneLine : 0);
    return base + g.headerGap + cardRowsHeight(z.cards, shown, cut, step, cols, width);
}

function boardHeight(zones, shown, step, cols, fixedTop, width) {
    let h = fixedTop;
    zones.forEach(function (z, i) { h += zoneHeight(z, shown[i], step, cols, width) + (i > 0 ? GEOM.gap : 0); });
    return h + GEOM.padBottom;
}

// fitBoard is the board's fit: (viewport {width, height}, the status row's
// height, the zones with their cards, whether the board bar is drawn) to
// {step, columns, shown (cards drawn per zone; the rest behind "+N more"),
// rowsPerTile}. statusHeight is a number, or a function of the rows a cell
// tile shows (statusRowLayout's height at that count) where those may give way.
//
//   - the largest step at which every card's codes fit unclipped (cardFits) and
//     every zone fits the height;
//   - where no step fits the height, the smallest step whose codes fit, and the
//     zones give way from the last: cards are cut from the end of a zone, plain
//     ones only (a red or amber card is never cut; they sort first), and a
//     "+N more" card takes one slot, in a zone cut to no card too;
//   - a folded Clear partials is one row and never cut; opened by the board
//     (amber or red) it keeps its first card and gives way only after every
//     other zone has;
//   - where the zones can give no more, a cell tile shows fewer rows (the rest
//     behind its "+N"), down to one.
export function fitBoard(viewport, statusHeight, zones, hasBar) {
    const g = GEOM;
    const cols = columnsFor(viewport.width);
    const statusAt = typeof statusHeight === 'function' ? statusHeight : function () { return statusHeight; };
    const topAt = function (rows) {
        const h = statusAt(rows);
        return g.padTop + g.topLine + g.gap + (hasBar ? g.bar + g.gap : 0) + (h > 0 ? h + g.gap : 0);
    };
    let fixedTop = topAt(MAX_TILE_ROWS);
    const full = zones.map(function (z) { return z.folded ? 0 : z.cards.length; });
    const fits = function (step) {
        return zones.every(function (z) {
            return z.cards.every(function (c) { return cardFits(c, step, cardInnerWidth(viewport.width, cols, !!c.number)); });
        });
    };
    for (let step = 0; step < STEPS.length; step++) {
        if (!fits(step)) continue;
        if (boardHeight(zones, full, step, cols, fixedTop, viewport.width) <= viewport.height) {
            return { step: step, columns: cols, shown: full, rowsPerTile: MAX_TILE_ROWS };
        }
    }
    const step = STEPS.length - 1;
    const shown = full.slice();
    const keep = zones.map(function (z) {
        if (z.folded) return 0;
        const coloured = z.cards.filter(function (c) { return c.tone !== 'plain'; }).length;
        return z.openedByBoard ? Math.max(1, coloured) : coloured;
    });
    const giveWay = function (i) {
        const z = zones[i];
        while (boardHeight(zones, shown, step, cols, fixedTop, viewport.width) > viewport.height && shown[i] > keep[i]) {
            const rows = zoneRows(shown[i], z.cards.length - shown[i], cols);
            // One row fewer, with a slot for "+N more".
            shown[i] = Math.max(keep[i], Math.min(shown[i] - 1, (rows - 1) * cols - 1));
        }
    };
    for (let i = zones.length - 1; i >= 0; i--) {
        if (zones[i].folded || zones[i].openedByBoard) continue;
        giveWay(i);
    }
    zones.forEach(function (z, i) { if (z.openedByBoard) giveWay(i); });
    let rows = MAX_TILE_ROWS;
    while (rows > 1 && boardHeight(zones, shown, step, cols, fixedTop, viewport.width) > viewport.height) {
        rows--;
        fixedTop = topAt(rows);
    }
    // Give back what fits: a cut that saved no height (a zone of one row cut to
    // its "+N more"), or room a later cut freed, draws its cards again, the
    // first zone first. Pinned by operator-board-model.test.js ("draws every
    // card row that fits").
    zones.forEach(function (z, i) {
        while (shown[i] < full[i]) {
            shown[i]++;
            if (boardHeight(zones, shown, step, cols, fixedTop, viewport.width) > viewport.height) { shown[i]--; break; }
        }
    });
    return { step: step, columns: cols, shown: shown, rowsPerTile: rows };
}

// statusRowLayout lays the status row (cellsRow's or pressesRow's) out at a
// width: its tiles per line, each wide enough for its longest part and count
// side by side (or with the count moved under the bar where that cannot fit),
// the lines it takes and its height, the input fitBoard takes. A cell tile
// shows at most rowsPerTile of its rows (MAX_TILE_ROWS when not given), the
// rest behind its "+N"; the "+N more" and Empties tiles sit beside the tiles.
export function statusRowLayout(status, width, rowsPerTile) {
    const most = rowsPerTile || MAX_TILE_ROWS;
    const g = GEOM;
    if (!status) return { columns: 0, lines: 0, height: 0, countUnder: false };
    const avail = width - 2 * g.padX;
    const n = status.tiles.length + (status.more > 0 ? 1 : 0) + (status.empties ? 1 : 0) + (status.failed ? 1 : 0);
    if (n === 0) return { columns: 0, lines: 0, height: 0, countUnder: false };
    let side = 0, under = 0;
    status.tiles.forEach(function (t) {
        if (t.rows) {
            t.rows.forEach(function (r) {
                const part = textWidth(r.part, 14);
                const words = textWidth(r.words, 13);
                side = Math.max(side, part + (r.words ? 8 + words : 0));
                under = Math.max(under, part, words);
            });
            side = Math.max(side, textWidth(t.cell, 14));
            under = Math.max(under, textWidth(t.cell, 14));
        } else {
            const top = textWidth(t.cell + ' · ' + t.node, 14) + 8 + textWidth(t.words, 13);
            const need = Math.max(top, textWidth(t.bin, 24), textWidth(t.part + ' · no empty ready', 12));
            side = Math.max(side, need);
            under = Math.max(under, need);
        }
    });
    if (status.empties) {
        status.empties.lines.forEach(function (l) {
            const w = textWidth(l.bin, 14) + 8 + textWidth(l.words, 14);
            side = Math.max(side, w);
            under = Math.max(under, w);
        });
    }
    const perLine = function (need) {
        return Math.max(1, Math.floor((avail + g.tileGap) / (need + g.tilePad + g.tileGap)));
    };
    let countUnder = false;
    let per = perLine(side);
    if (per < Math.min(n, MAX_STATUS_TILES + 1) && perLine(under) > per) {
        countUnder = true;
        per = perLine(under);
    }
    const columns = Math.min(per, Math.max(n, 4));
    const lines = Math.ceil(n / Math.min(per, n));
    // A cell tile's height: its head, each drawn row (a covered row is its
    // bar alone), and the "+N" when rows are hidden; the row is the tallest.
    let tileH = g.pressTile;
    if (status.tiles.length > 0 && status.tiles[0].rows) {
        tileH = 0;
        status.tiles.forEach(function (t) {
            const drawn = t.rows.slice(0, most);
            let h = g.tileHead + (t.hidden > 0 || t.rows.length > drawn.length ? g.rowMore : 0);
            drawn.forEach(function (r) { h += r.words ? (countUnder ? g.rowUnder : g.rowLine) : g.rowBar; });
            tileH = Math.max(tileH, h);
        });
    }
    if (status.empties) tileH = Math.max(tileH, g.tileHead + status.empties.lines.length * g.emptiesLine + g.emptiesFoot);
    return {
        columns: columns, lines: lines, countUnder: countUnder,
        height: g.statusLabel + lines * tileH + (lines - 1) * g.tileGap,
    };
}
