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

export function claimedNodes() {
    if (!_view || !_view.nodes) return [];
    return _view.nodes.filter(n => n.active_claim || n.changeover_task);
}

export function isReplenishing(entry) {
    const rt = entry.runtime;
    return rt && (rt.active_order_id || rt.staged_order_id);
}
