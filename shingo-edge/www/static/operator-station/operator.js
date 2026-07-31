// Operator Station Display — touch-centric 10" screen.
// Entry module wires SSE → loadView, refreshes the view, and bootstraps
// the render / modal / load-bin / release / keypad sub-modules.

import { stationID, showToast, friendlyOrderError, postAction, el, fetchWithTimeout } from './operator-util.js';
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
// Wall-clock deadline of the pending refreshTimer, so armRefresh can tell
// whether an incoming request wants the board sooner than what is queued.
let refreshDueAt = 0;
// In-flight view build, or null. Every refresh path funnels through
// loadView, so holding the guard HERE (rather than in scheduleRefresh)
// covers the SSE poll, postAction's post-action refresh, and the modal
// callbacks alike.
let inFlightLoad = null;
// A refresh was asked for while a build was already running. Set by the
// joining caller, consumed by loadView to run one prompt follow-up.
let refreshPending = false;

// Quiet-board cadence, and the debounce applied to event-driven refreshes.
const pollIdleMs = 3000;
const pollBurstMs = 500;

// THE INVARIANT: once the board has started polling, exactly one of "a timer is
// pending" or "a build is in flight" is true at all times. loadView re-arms in a
// finally, so the chain continues even if a build throws.
//
// This matters because it is easy to get wrong in the other direction. An
// earlier version of this file kept the chain's only continuation INSIDE the
// poll timer's callback, and had scheduleRefresh return early while a build was
// in flight. A build started off-chain — postAction and the modal callbacks call
// loadView directly — would then swallow the continuation: the timer fired,
// scheduleRefresh took the early return, armed nothing, and the poll chain was
// gone for good. The board only came back if an SSE event happened to arrive
// while idle, so a quiet board could sit stale indefinitely. That is a worse
// failure than the pile-up this file was changed to fix, and it would have been
// made more likely by anything that reduces SSE traffic. Keep the re-arm in
// loadView, not in the timer.
//
// armRefresh keeps whichever deadline is SOONER, so an event landing during the
// idle gap is still served promptly instead of waiting out the full gap.
function armRefresh(delayMs) {
    const dueAt = Date.now() + delayMs;
    if (refreshTimer !== null) {
        if (refreshDueAt <= dueAt) return;
        clearTimeout(refreshTimer);
    }
    refreshDueAt = dueAt;
    refreshTimer = setTimeout(function() {
        refreshTimer = null;
        // No await between clearing the timer and loadView taking the in-flight
        // guard, so the invariant above never has a gap.
        loadView();
    }, delayMs);
}

// loadView is single-flight: concurrent callers join the running build
// instead of starting another one.
//
// The old shape cleared refreshTimer *before* awaiting the build, so the
// re-entrancy guard was released for the whole duration of the fetch. Every
// SSE event landing during a build (onOrderUpdate / onCounterUpdate /
// onMaterialRefresh fire once or twice a second on a live plant) therefore
// armed another concurrent build. Edge serialises all DB work on a single
// connection (store.Open sets SetMaxOpenConns(1)), so those builds queue
// behind each other and behind the write stream, and every one of them gets
// slower — a ratchet that only a process restart cleared. Springfield's
// 22-home bin-loader board measured 3.1s on a freshly restarted edge and
// 25-116s after a day of uptime, purely from this pile-up.
//
// Joining rather than dropping matters: postAction awaits loadView to show
// the result of what the operator just did, and a dropped refresh would
// leave the board stale. A joined caller may get a snapshot that predates
// its own action, so it sets refreshPending to earn one prompt follow-up.
async function loadView() {
    if (inFlightLoad) {
        refreshPending = true;
        return inFlightLoad;
    }
    inFlightLoad = doLoadView();
    try {
        return await inFlightLoad;
    } finally {
        inFlightLoad = null;
        // Continue the poll chain. This is the ONE continuation point, and it
        // runs for every build regardless of which path started it — see the
        // invariant above. A refresh requested mid-build is served promptly
        // rather than waiting out the full idle gap.
        const soon = refreshPending;
        refreshPending = false;
        armRefresh(soon ? 0 : pollIdleMs);
    }
}

async function doLoadView() {
    try {
        // Bounded (fetchWithTimeout): scheduleRefresh awaits this before arming the
        // next poll, so a hung view fetch — a severed connection — would freeze every
        // board update until a hard refresh. On timeout it throws → caught below →
        // the poll loop keeps ticking. The timeout only guards a true hang: it is set
        // well above the slowest legitimate view so it never aborts a slow-but-working
        // one (a 10s cap once stranded a heavy bin-loader view mid-load).
        //
        // An abort here now propagates: the handler honours r.Context() and the
        // shared build is cancelled once its last waiter leaves, so the server
        // stops holding the single DB connection. Before that, an under-set
        // timeout made the pile-up strictly worse — every retry added an orphan
        // build and removed none.
        const res = await fetchWithTimeout('/api/operator-stations/' + stationID + '/view', undefined, 30000);
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
            // The selected node is genuinely absent from a SUCCESSFULLY loaded
            // view — it was deleted, unassigned from this station, or the
            // process changed under the operator. Closing is right; a modal
            // acting on a node that no longer exists is worse.
            //
            // C5 (2026-07-31) checked the failure path and it is safe: a failed
            // or timed-out fetch returns from doLoadView BEFORE renderAll, so a
            // network blip cannot reach this branch and slam the modal shut
            // mid-interaction. Only a real, parsed view without the node does.
            closeModal();
        }
    }
}

// scheduleRefresh asks for the board to be refreshed soon. Safe to call from
// anywhere, any number of times: it never starts a second concurrent build and
// never drops the poll chain.
//
// The prompt follow-up (rather than an immediate second build) also gives Core
// time to process receipt + ApplyBinArrival after auto-confirm. With the
// retrieve_empty staging exemption in Core, bins are available immediately; this
// covers residual latency.
function scheduleRefresh() {
    if (inFlightLoad) {
        // A build is already running and will re-arm when it settles. Record the
        // interest so that re-arm is prompt instead of a full idle gap.
        refreshPending = true;
        return;
    }
    armRefresh(pollBurstMs);
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
