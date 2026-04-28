// Operator Station Display — touch-centric 10" screen.
// Entry module wires SSE → loadView, refreshes the view, and bootstraps
// the render / modal / load-bin / release / keypad sub-modules.

import { stationID, showToast, friendlyOrderError } from './operator-util.js';
import {
    getView, setView, getSelectedNodeID,
    getLastViewJSON, setLastViewJSON,
    findNodeByID,
} from './operator-state.js';
import {
    renderHeader, renderGrid, renderFooter, setRenderRefs,
} from './operator-render.js';
import {
    openModal, closeModal, renderModal, setModalLoadView,
} from './operator-modal.js';
import { openLoadBin, setLoadView } from './operator-load-bin.js';
import { setReleaseRefs } from './operator-release.js';

let refreshTimer = null;

async function loadView() {
    try {
        const res = await fetch('/api/operator-stations/' + stationID + '/view');
        if (!res.ok) { showToast('Connection error: ' + res.status, 'error'); return; }
        const text = await res.text();
        if (text === getLastViewJSON()) return;
        setLastViewJSON(text);
        setView(JSON.parse(text));
        renderAll();
    } catch (err) {
        console.error('loadView', err);
        showToast('Network error', 'error');
    }
}

function renderAll() {
    if (!getView()) return;
    renderHeader();
    renderGrid();
    renderFooter();
    const sid = getSelectedNodeID();
    if (sid !== null) {
        const entry = findNodeByID(sid);
        if (entry) {
            renderModal(entry);
        } else {
            closeModal();
        }
    }
}

function scheduleRefresh() {
    if (refreshTimer) return;
    refreshTimer = setTimeout(async () => {
        refreshTimer = null;
        await loadView();
        // Follow-up gives Core time to process receipt + ApplyBinArrival
        // after auto-confirm. With the retrieve_empty staging exemption in
        // Core, bins are available immediately; this covers residual latency.
        setTimeout(() => scheduleRefresh(), 3000);
    }, 500);
}

function handleOrderFailed(data) {
    scheduleRefresh();
    const reason = data && (data.reason || data.Reason || data.detail || data.Detail);
    const msg = friendlyOrderError(reason) || 'Order failed';
    showToast(msg, 'error', { sticky: true });
}

// ─── Wire sub-module callbacks (one-way, breaks the import cycle) ───

setRenderRefs({ openModal, openLoadBin, loadView });
setModalLoadView(loadView);
setLoadView(loadView);
setReleaseRefs({ renderModal, closeModal, loadView });

// ─── SSE ───
//
// shingoedge.js loads as a classic script (window.ShingoEdge); module-scope
// `import` can't reach it, so call through the global. createSSE handles
// reconnect with backoff and the connected-event reset internally.

const SSE = window.ShingoEdge && window.ShingoEdge.createSSE;
if (!SSE) {
    console.error('ShingoEdge.createSSE missing — SSE will not connect');
} else {
    SSE('/events', {
        onOrderUpdate: () => scheduleRefresh(),
        onOrderCompleted: () => scheduleRefresh(),
        onCounterUpdate: () => scheduleRefresh(),
        onChangeoverUpdate: () => scheduleRefresh(),
        onMaterialRefresh: () => scheduleRefresh(),
        // order-failed also fires a sticky error toast so the operator sees
        // the failure even if they've looked away. Without this, async
        // failures (fleet failure, admin terminate, structural resolver
        // error) are only visible on the next view refresh.
        onOrderFailed: handleOrderFailed,
    });
}

// ─── Init ───

loadView();

// Re-layout on resize (orientation change, window resize).
window.addEventListener('resize', function() { if (getView()) renderGrid(); });
