// Shared module state. ES modules give each importer the same binding,
// so getView()/setView() act as a single source of truth without globals.

let _view = null;
let _selectedNodeID = null;
let _lastViewJSON = '';

export function getView() { return _view; }
export function setView(v) { _view = v; }

export function getSelectedNodeID() { return _selectedNodeID; }
export function setSelectedNodeID(id) { _selectedNodeID = id; }

export function getLastViewJSON() { return _lastViewJSON; }
export function setLastViewJSON(s) { _lastViewJSON = s; }

// ── the cell picture ─────────────────────────────────────────────────────────
//
// IT IS NOT ON THE VIEW ANY MORE (owner ruling 4, 2026-09-17). The picture rode
// every poll — two queries and the plant's whole group map, rebuilt at 500 ms a
// board on a Pi with one SQLite connection — for a drawing that changes when an
// engineer edits a cell. The view carries a VERSION and this holds the picture
// the page fetched for it.
//
// KEPT HERE, beside the view, because that is where it is read from: every
// renderer takes `view.cell` and ensureCellPicture re-attaches this to each
// freshly parsed view. One store, one attachment point.
//
// THE VERSION HELD IS THE PICTURE'S OWN, not the one the poll asked with. A
// fetch can return a picture built from rows NEWER than the version that sent
// the page for it; storing what came back means the next poll matches and
// nothing asks again. See domain.CellPicture.Version.
let _cellPicture = null;
let _cellPictureVersion = '';

export function getCellPicture() { return _cellPicture; }

export function setCellPicture(pic) {
    _cellPicture = pic || null;
    _cellPictureVersion = (pic && pic.version) || '';
}

export function getCellPictureVersion() { return _cellPictureVersion; }

export function findNodeByID(id) {
    if (!_view || !_view.nodes) return null;
    return _view.nodes.find(n => n.node.id === id) || null;
}

// claimedNodes is the tiles the board draws: a loader's window or position
// (tile.loader: a loader tile carries no claim), a node with a claim, or a node
// with changeover work.
export function claimedNodes() {
    if (!_view || !_view.nodes) return [];
    return _view.nodes.filter(n => n.loader || n.active_claim || n.changeover_task);
}

export function isReplenishing(entry) {
    const rt = entry.runtime;
    return rt && (rt.active_order_id || rt.staged_order_id);
}

// ── which events concern this station ────────────────────────────────────────
//
// eventConcerns reports whether an event from /events names something this
// station's view draws, so the station rebuilds only for its own events (every
// event reaches every screen on the Edge). It draws:
//
//   - its process (view.process.id, and by name for a sourcing report) and the
//     processes of the cells its loaders serve (a served row's process_id and
//     cell);
//   - its process nodes (a tile's node.id) and the served rows' process nodes;
//   - the orders on its tiles;
//   - the Core nodes its tiles stand at and a loader's buffer spots;
//   - a refusal at its loader window, or for a part an order on its tiles is
//     for.
//
// An event that names none of these kinds concerns every station: an
// order-update with no process node, material-refresh, containment, Core's
// refusal snapshot, a sourcing snapshot naming no process. A board
// that draws view.empties takes every bins event: the group's members are not
// on the view. Pinned by operator-refresh.test.js.
export function eventConcerns(view, type, data) {
    if (!view) return true;
    data = data || {};
    const tiles = view.nodes || [];
    const serves = view.serves || [];
    const processID = view.process ? view.process.id : null;
    const ownProcess = function (pid) {
        return pid === processID || serves.some(function (r) { return r.process_id === pid; });
    };
    const ownProcessNode = function (pn) {
        return tiles.some(function (t) { return t.node && t.node.id === pn; }) ||
            serves.some(function (r) { return r.process_node_id === pn; });
    };
    const ownOrder = function (id) {
        return tiles.some(function (t) { return (t.orders || []).some(function (o) { return o.id === id; }); });
    };
    const ownNode = function (name) {
        return tiles.some(function (t) {
            if (t.node && t.node.core_node_name === name) return true;
            return !!(t.loader && (t.loader.buffer || []).some(function (s) { return s.node === name; }));
        });
    };
    switch (type) {
    case 'order-update':
        if (data.process_node_id == null) return true;
        return ownProcessNode(data.process_node_id) || ownOrder(data.order_id);
    case 'counter-update':
        if (data.process_node_id != null) return ownProcessNode(data.process_node_id);
        if (data.process_id != null) return ownProcess(data.process_id);
        return true;
    case 'changeover-update':
    case 'changeover-verify-mismatch':
        return data.process_id == null || ownProcess(data.process_id);
    case 'order-failed':
    case 'order-faulted':
        return ownOrder(data.order_id);
    case 'uop-stranded-alarm':
        return ownNode(data.core_node_name);
    case 'bins':
        // A group's wants are drawn only on a board that draws its empties.
        return !!view.empties || (data.nodes || []).some(ownNode);
    case 'supply-refusal':
        // A refusal is drawn at its loader window and on a cell with an order
        // for the part; Core's snapshot names no card.
        if (!data.loader_node && !data.payload_code) return true;
        return ownNode(data.loader_node) || tiles.some(function (t) {
            return (t.orders || []).some(function (o) { return o.payload_code === data.payload_code; });
        });
    case 'sourcing': {
        const named = data.processes || [];
        if (named.length === 0) return true;
        const name = view.process ? view.process.name : '';
        return named.indexOf(name) !== -1 || serves.some(function (r) { return named.indexOf(r.cell) !== -1; });
    }
    default:
        return true;
    }
}

// ── the link: whether this station's server is answering ─────────────────────
//
// answering is false after a failed view fetch or while the event stream is
// down (the bus's disconnected, its watchdog included), until a fetch answers
// with the stream up;
// dataAt is when the view on screen was last fetched (the server's clock,
// ISO). The station's sign reads it (operator-board-model.js staleSign).
let _link = { answering: true, dataAt: '' };

export function getLink() { return _link; }
export function setLink(link) { _link = Object.assign({}, _link, link); }
