// Shared module state. ES modules give each importer the same binding,
// so getView()/setView() act as a single source of truth without globals.

let _view = null;
let _selectedNodeID = null;
let _lastViewJSON = '';

// Manual_swap loader board mode: 'active' (demand-driven cards) or 'preload'
// (the full covered-payload list, with manual requests enabled). Session-only
// — resets to the safe 'active' default on reload. Held here so it survives
// the setView()/renderAll() cycle that every SSE event runs. _boardModeTouched
// records an explicit operator toggle so the transitional "default to preload"
// rule doesn't clobber the operator's choice on the next refresh.
let _boardMode = 'active';
let _boardModeTouched = false;

export function getBoardMode() { return _boardMode; }
export function setBoardMode(mode) { _boardMode = mode; _boardModeTouched = true; }

// applyTransitionalDefault flips the board to preload ONCE for a transitional
// loader (which has no meaningful active-demand mode), unless the operator has
// already toggled. Returns the effective mode. Called on each board render.
export function applyTransitionalDefault(isTransitional) {
    if (isTransitional && !_boardModeTouched) { _boardMode = 'preload'; }
    return _boardMode;
}

export function getView() { return _view; }
export function setView(v) { _view = v; }

export function getSelectedNodeID() { return _selectedNodeID; }
export function setSelectedNodeID(id) { _selectedNodeID = id; }

export function getLastViewJSON() { return _lastViewJSON; }
export function setLastViewJSON(s) { _lastViewJSON = s; }

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
