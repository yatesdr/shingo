// Operator Station Display — touch-centric 10" screen.
// Entry module wires SSE → loadView, refreshes the view, and bootstraps
// the render / modal / load-bin / release / keypad sub-modules.

import { stationID, showToast, friendlyOrderError, postAction, el } from './operator-util.js';
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
import { setReleaseRefs, isReleasePromptOpen } from './operator-release.js';

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
    checkPostCutoverFlag();
    const sid = getSelectedNodeID();
    if (sid !== null) {
        const entry = findNodeByID(sid);
        if (entry) {
            // Skip the modal repaint while the release prompt is up — its
            // step-1/step-2 markup lives in the same node-modal-content
            // container, so renderModal would clobber the operator's
            // in-progress selection on every SSE refresh.
            if (!isReleasePromptOpen()) {
                renderModal(entry);
            }
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
    let msg = friendlyOrderError(reason) || 'Order failed';
    if (data && data.order_type) {
        msg = data.order_type + ': ' + msg;
    }
    if (data && data.order_id) {
        msg += ' (#' + data.order_id + ')';
    }
    showToast(msg, 'error', { sticky: true });
}

function handleUopStranded(data) {
    scheduleRefresh();
    // One sticky toast per alarm window (Core emits once per window). The detail
    // is the full front-door instruction, rendered verbatim.
    const msg = data && (data.detail || data.Detail);
    if (msg) {
        showToast(msg, 'error', { sticky: true });
    }
}

// ─── Post-cutover part-id verification flag ───
//
// After a cutover, if the press's live part id still disagrees with the style it
// was set to, Core flags the changeover. We fetch that flag on every view render
// (so a flag raised while no one was looking still surfaces) and offer the two
// resolutions in place: a one-tap corrective changeover to the mapped style, and
// a link to review the style's expected part-id config. "Confirm" clears it.

function removePostCutoverBanner() {
    const b = document.getElementById('os-postcutover-flag');
    if (b) b.remove();
}

async function checkPostCutoverFlag() {
    const view = getView();
    if (!view || !view.process) { removePostCutoverBanner(); return; }
    const pid = view.process.id;
    try {
        const res = await fetch('/api/processes/' + pid + '/post-cutover-flag');
        if (!res.ok) return;
        const body = await res.json();
        if (!body || !body.flagged || !body.flag) { removePostCutoverBanner(); return; }
        renderPostCutoverFlag(pid, body.flag);
    } catch (err) {
        console.error('checkPostCutoverFlag', err);
    }
}

function renderPostCutoverFlag(pid, flag) {
    removePostCutoverBanner(); // id-stable re-render — an SSE refresh can't stack banners
    const bannerStyle = {
        position: 'fixed', top: '0', left: '0', right: '0', zIndex: '9000',
        background: '#7a1f1f', color: '#fff', padding: '0.9rem 1.1rem',
        boxShadow: '0 2px 10px rgba(0,0,0,0.4)', display: 'flex',
        flexDirection: 'column', gap: '0.6rem',
    };
    const btnStyle = {
        padding: '0.6rem 0.9rem', fontSize: '1rem', borderRadius: '6px',
        border: '1px solid rgba(255,255,255,0.5)', background: 'rgba(255,255,255,0.12)',
        color: '#fff', cursor: 'pointer', textDecoration: 'none', display: 'inline-block',
    };
    const banner = el('div', { id: 'os-postcutover-flag', style: bannerStyle });
    banner.appendChild(el('div', { textContent: flag.message, style: { fontWeight: '600', fontSize: '1.05rem' } }));

    const actions = el('div', { style: { display: 'flex', gap: '0.6rem', flexWrap: 'wrap' } });
    // Resolution 1 — one-tap corrective changeover to the mapped style.
    if (flag.has_mapped) {
        const fixStyle = Object.assign({}, btnStyle, { background: '#2f7a2f', borderColor: '#2f7a2f' });
        const fix = el('button', { type: 'button', style: fixStyle, textContent: 'Change over to ' + flag.mapped_style_name });
        fix.addEventListener('click', async () => {
            fix.disabled = true;
            const ok = await postAction('/api/processes/' + pid + '/changeover/start', {
                to_style_id: flag.mapped_style_id,
                called_by: 'post-cutover-correction',
                notes: 'corrective changeover from post-cutover part-id mismatch',
            }, loadView);
            if (ok) { showToast('Corrective changeover to ' + flag.mapped_style_name + ' started', 'success'); removePostCutoverBanner(); }
        });
        actions.appendChild(fix);
    }
    // Resolution 2 — review the style's expected part-id config.
    actions.appendChild(el('a', { href: '/processes', target: '_blank', style: btnStyle, textContent: 'Review part-id config' }));
    // Confirm the press is correct → clear the flag.
    const confirmBtn = el('button', { type: 'button', style: btnStyle, textContent: 'Confirm — press is correct' });
    confirmBtn.addEventListener('click', async () => {
        confirmBtn.disabled = true;
        const ok = await postAction('/api/processes/' + pid + '/post-cutover-flag/confirm', {}, loadView);
        if (ok) removePostCutoverBanner();
    });
    actions.appendChild(confirmBtn);

    banner.appendChild(actions);
    document.body.appendChild(banner);
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
        // uop-stranded-alarm (P2-C7/C8): parked ticks piling up on an unbound
        // node. Refresh so the tile chip + attention badge appear immediately,
        // and fire one sticky toast per alarm window with the exact instruction.
        onUopStrandedAlarm: handleUopStranded,
        // changeover-verify-mismatch: a post-cutover part-id disagreement was
        // flagged. Refresh so checkPostCutoverFlag fetches + shows the prompt.
        onChangeoverVerifyMismatch: () => scheduleRefresh(),
    });
}

// ─── Init ───

loadView();

// Re-layout on resize (orientation change, window resize).
window.addEventListener('resize', function() { if (getView()) renderGrid(); });
