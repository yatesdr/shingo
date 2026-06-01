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
