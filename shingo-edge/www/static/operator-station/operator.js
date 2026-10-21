// Operator Station Display — touch-centric 10" screen.
// Entry module wires SSE → loadView, refreshes the view, and bootstraps
// the render / modal / load-bin / release / keypad sub-modules.
//
// THE BOARD DOES NOT POLL. It fetches its view when the stream says something
// it draws changed (an event it takes, or the Edge's coalesced nudge after any
// write), on every connect of the stream, when the stream reports a frame it
// never received (the bus's stream-gap), when the screen becomes visible, and
// after the operator's own taps. Every door that writes a view input emits
// (the Edge's nudgeAfterWrite and EventBoardNudge); a lost frame is a
// reconnect or a gap, never a silence; a stream with no frame for 75 s is
// treated as dead. An idle board makes no request at all.

import { stationID, showToast, friendlyOrderError, postAction, el, fetchWithTimeout } from './operator-util.js';
import {
    getView, setView, getSelectedNodeID,
    getLastViewJSON, setLastViewJSON,
    findNodeByID,
    getCellPicture, setCellPicture, getCellPictureVersion,
    eventConcerns, claimedNodes, setLink,
} from './operator-state.js';
import {
    renderHeader, renderGrid, renderFooter, setRenderRefs, openComposerPicker, stationSign,
} from './operator-render.js';
import { isBoard } from './operator-board-model.js';
import {
    openModal, closeModal, renderModal, setModalLoadView, modalOpening, closeModalFrom,
} from './operator-modal.js';
import { openLoadBin, setLoadView } from './operator-load-bin.js';
import { setReleaseRefs, isReleasePromptOpen } from './operator-release.js';
import { onSSE, setSSEReloadOnBuild, setSSEQuery, setSSEWatchdog, serverNow } from '/static/shared/utils.js';

let refreshTimer = null;
// Wall-clock deadline of the pending refreshTimer, so armRefresh can tell
// whether an incoming request wants the board sooner than what is queued.
let refreshDueAt = 0;
// In-flight view build, or null. Every refresh path funnels through
// loadView, so holding the guard HERE (rather than in scheduleRefresh)
// covers the event refresh, postAction's post-action refresh, and the modal
// callbacks alike.
let inFlightLoad = null;
// A refresh was asked for while a build was already running. Set by the
// joining caller (a tap's postAction, a modal callback), consumed by loadView
// to run one prompt follow-up.
let refreshPending = false;
// An EVENT asked for a refresh while a build was running. Its follow-up keeps
// the cap: it starts no sooner than eventCapMs after the build that was
// running started (scheduleRefresh).
let eventPending = false;
// The debounce applied to event-driven refreshes: at most one build per this.
const eventCapMs = 500;

// THE FOLLOW-UP LIVES IN loadView, NOT IN THE TIMER. A refresh asked for while
// a build runs is recorded (refreshPending, eventPending) and served from
// loadView's finally, whichever path started the build — postAction and the
// modal callbacks call loadView directly. An earlier version kept its only
// continuation inside the timer's callback and returned early while a build
// was in flight, so a build started off that path swallowed the request.
// Nothing re-arms otherwise: there is no idle chain.
//
// armRefresh keeps whichever deadline is SOONER, so an event landing while one
// is queued is served at the earlier time instead of pushing it back.
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
        // guard, so a refresh asked for meanwhile joins this build.
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
    const startedAt = Date.now();
    inFlightLoad = doLoadView();
    try {
        return await inFlightLoad;
    } finally {
        inFlightLoad = null;
        // The ONE follow-up point, run for every build regardless of which
        // path started it (see above). A tap's refresh requested mid-build is
        // served at once; an event's waits out the cap from this build's start
        // (a bug fixed: it re-armed at 0 ms, so steady events gave
        // back-to-back builds); nothing asked, nothing armed. Pinned by
        // operator-refresh.test.js.
        let delay = -1;
        if (eventPending) delay = Math.max(0, startedAt + eventCapMs - Date.now());
        if (refreshPending) delay = 0;
        refreshPending = false;
        eventPending = false;
        if (delay >= 0) armRefresh(delay);
    }
}

// viewETag is the ETag of the view held (getLastViewJSON), '' before the first.
let viewETag = '';

// ── the link, for the stale sign ──
//
// The server is answering while the event stream is up (the bus's connected /
// disconnected, its watchdog included) and the last view fetch answered. No
// poll refreshes either: a fetch that failed is fetched again on the next
// heartbeat (the server's, every 30 s), and only while it keeps failing. The
// sign is redrawn only when what it says changed: the header, and on the
// board its top band.
let fetchOK = true;
let streamUp = true;
let shownSign = '';

function fetchAnswered(ok) {
    fetchOK = ok;
    const change = { answering: ok && streamUp };
    if (ok) change.dataAt = new Date(serverNow()).toISOString();
    setLink(change);
    redrawSign();
}

function streamChanged(up) {
    streamUp = up;
    setLink({ answering: fetchOK && up });
    redrawSign();
}

function redrawSign() {
    if (!getView()) return;
    const key = JSON.stringify(stationSign());
    if (key === shownSign) return;
    shownSign = key;
    renderHeader();
    renderGrid();
}

async function doLoadView() {
    try {
        // Bounded (fetchWithTimeout): loadView is single-flight, so a hung view
        // fetch — a severed connection — would hold every later refresh behind
        // it until a hard refresh. On timeout it throws → caught below → the
        // next refresh can start. The timeout only guards a true hang: it is set
        // well above the slowest legitimate view so it never aborts a slow-but-working
        // one (a 10s cap once stranded a heavy bin-loader view mid-load).
        //
        // An abort here now propagates: the handler honours r.Context() and the
        // shared build is cancelled once its last waiter leaves, so the server
        // stops holding the single DB connection. Before that, an under-set
        // timeout made the pile-up strictly worse — every retry added an orphan
        // build and removed none.
        //
        // CONDITIONAL: the request quotes the ETag of the view it holds, and an
        // unchanged view answers 304 with no body, so a refetch that finds
        // nothing new costs no bytes, no parse and no render
        // (handlers_operator_stations.go writeConditional). The cache is
        // bypassed so the 304 reaches here.
        const headers = viewETag ? { 'If-None-Match': viewETag } : undefined;
        const res = await fetchWithTimeout('/api/operator-stations/' + stationID + '/view',
            { headers: headers, cache: 'no-store' }, 30000);
        // A failed fetch is said by the stale sign, not a toast
        // (staleSign: "Not answering · data from <time>").
        if (res.status !== 304 && !res.ok) { fetchAnswered(false); return; }
        if (res.status === 304) { fetchAnswered(true); checkPostCutoverFlag(); return; }
        const text = await res.text();
        viewETag = (res.headers && res.headers.get('ETag')) || '';
        if (text === getLastViewJSON()) { fetchAnswered(true); checkPostCutoverFlag(); return; }
        setLastViewJSON(text);
        setView(JSON.parse(text));
        fetchOK = true;
        setLink({ answering: streamUp, dataAt: new Date(serverNow()).toISOString() });
        shownSign = JSON.stringify(stationSign());
        // THE CELL PICTURE, FETCHED ONLY WHEN ITS VERSION MOVED. The view
        // carries `cell_version` and no picture; this puts one back on the
        // object every renderer reads, from a copy held across fetches. On the
        // ordinary fetch it is an assignment and nothing else.
        //
        // AWAITED, so the picture is on the view before renderAll draws it.
        // The wait happens on the fetch that changed the version and on the
        // first fetch of the session, which is the load the operator is already
        // watching, and never again.
        await ensureCellPicture(getView());
        // The composer holds its own copy of the view for the screens stacked
        // over the board. Handing it the new one keeps an OPEN picker's rows
        // live — a style whose flow was set up from the desktop while the
        // sheet was open stops reading "Set up from the desktop" — and costs
        // nothing when the composer is closed.
        if (window.ComposerUI) window.ComposerUI.setView(getView());
        renderAll();
    } catch (err) {
        console.error('loadView', err);
        fetchAnswered(false);
    }
}

// ensureCellPicture attaches the held picture to this view, fetching a new one
// first when the view's version no longer matches the one the held picture
// carries.
//
// A FAILED FETCH KEEPS THE OLD PICTURE rather than blanking it. The station
// board draws a cell an operator is standing in front of; a picture that is one
// edit out of date is better than no picture, and the next fetch tries again
// because the version still will not match.
//
// A VIEW WITH NO VERSION AT ALL is an edge older than this feature answering a
// newer page (or the other way round). Nothing is fetched and nothing is
// attached, which draws the "No positions on this station yet" caption — the
// same thing a station with no positions shows, and better than a request loop
// against a door that is not there.
let cellPictureFetch = null;

async function ensureCellPicture(view) {
    if (!view) return;
    const want = view.cell_version || '';
    if (want && want !== getCellPictureVersion()) {
        // ONE FETCH IN FLIGHT. loadView is single-flight, but a post-action
        // refresh can land while this await is open; two fetches of the same
        // picture is two builds on the one SQLite connection.
        if (!cellPictureFetch) {
            cellPictureFetch = fetchWithTimeout('/api/operator-stations/' + stationID + '/cell', undefined, 30000)
                .then(r => (r.ok ? r.json() : null))
                .then(pic => { if (pic) setCellPicture(pic); })
                .catch(err => { console.error('cell picture', err); })
                .finally(() => { cellPictureFetch = null; });
        }
        await cellPictureFetch;
    }
    view.cell = getCellPicture();
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
// never drops a request (loadView serves one asked for mid-build).
//
// The prompt follow-up (rather than an immediate second build) also gives Core
// time to process receipt + ApplyBinArrival after auto-confirm. With the
// retrieve_empty staging exemption in Core, bins are available immediately; this
// covers residual latency.
//
// THE DEBOUNCE IS A CAP: at most one event-driven build per eventCapMs. An
// event on a quiet board arms one eventCapMs out (armRefresh keeps the sooner
// deadline, so later events do not push it back); one landing mid-build waits
// for the build to settle and then for eventCapMs from that build's start.
// A tap's own refresh is not an event and is never delayed (loadView).
function scheduleRefresh() {
    if (inFlightLoad) {
        // A build is already running and will re-arm when it settles. Record the
        // interest so that re-arm comes after the cap instead of a full idle gap.
        eventPending = true;
        return;
    }
    armRefresh(eventCapMs);
}

// onEvent is the station's door for an event from /events: a refresh when the
// event names something this station draws (eventConcerns), else nothing; and
// then, only here, the event's own words (a toast).
function onEvent(type, then) {
    return function (data) {
        if (!eventConcerns(getView(), type, data)) return;
        scheduleRefresh();
        if (then) then(data);
    };
}

function handleOrderFailed(data) {
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
    // One toast per window (the engine emits once per window). The detail is the
    // operator sentence, rendered verbatim.
    //
    // Severity follows needs_action rather than being fixed at error. Under the
    // configured wait this is a swap in progress — a normal, temporary state —
    // and a sticky red toast for it is how operators learned the notice means
    // nothing. Past the wait it is a real ask and keeps the sticky error.
    const msg = data && (data.detail || data.Detail);
    if (!msg) return;
    const needsAction = !!(data && (data.needs_action || data.NeedsAction));
    if (needsAction) {
        showToast(msg, 'error', { sticky: true });
    } else {
        showToast(msg, 'info');
    }
}

// ─── Alerts: the post-cutover flag and the part-change alerts ───
//
// One place at the top of the screen (#os-alerts) holds them: the post-cutover
// part-id flag and the three part-change alerts (catid-mismatch,
// catid-change-prompt, catid-auto-cutover). Each is shown when it arrives, on
// the stations of the process it names and nowhere else, and stays until
// dismissed on that screen. NOTHING IS STORED: a screen that was closed or
// reloading when an alert arrived does not show it.
//
// A BOARD OF LOADER TILES (isBoard) DRAWS NONE OF THEM: it is a forklift's
// screen, not a press's, and takes no banner. Pinned by
// operator-refresh.test.js.

function onBoard() { return isBoard(claimedNodes()); }

// alertsBox is the page's #os-alerts (operator-display.html), under the header;
// a page without one gets one at its end.
function alertsBox() {
    let box = document.getElementById('os-alerts');
    if (!box) {
        box = el('div', { id: 'os-alerts', className: 'os-alerts' });
        document.body.appendChild(box);
    }
    return box;
}

function removeAlert(key) {
    const a = document.getElementById('os-alert-' + key);
    if (a) a.remove();
}

// showAlert draws one alert, in place of any with the same key. tone is red
// (a problem on the press) or plain; buttons are [{label, primary, onClick}],
// each handed its own button.
function showAlert(key, tone, text, buttons) {
    removeAlert(key);
    const alert = el('div', { id: 'os-alert-' + key, className: 'os-alert os-alert--' + tone });
    alert.appendChild(el('div', { className: 'os-alert-text', textContent: text }));
    const row = el('div', { className: 'os-alert-actions' });
    buttons.forEach(function (b) {
        const btn = el('button', { type: 'button', className: 'os-alert-btn' + (b.primary ? ' os-alert-btn--primary' : ''),
            textContent: b.label });
        btn.addEventListener('click', function () { b.onClick(btn); });
        row.appendChild(btn);
    });
    alert.appendChild(row);
    alertsBox().appendChild(alert);
}

function dismissButton(key) {
    return { label: 'Dismiss', onClick: function () { removeAlert(key); } };
}

// forThisStation reports whether an alert naming a process is this station's
// to draw: its own process, and not a board of loader tiles.
function forThisStation(data) {
    const view = getView();
    return !!view && !!view.process && !!data && data.process_id === view.process.id && !onBoard();
}

function handleCatidMismatch(data) {
    if (!forThisStation(data)) return;
    showAlert('catid-mismatch', 'red', 'Wrong part on ' + (data.process_name || 'the process') + ': the process reports ' +
        data.live_catid + '; ' + data.style_name + ' runs ' + data.expected_catid + '.', [dismissButton('catid-mismatch')]);
}

function handleCatidChangePrompt(data) {
    if (!forThisStation(data)) return;
    showAlert('catid-change-prompt', 'plain', 'The process\'s part changed.', [
        // Today's picker: it takes no target, so none is passed.
        { label: 'Start changeover', primary: true, onClick: function () {
            removeAlert('catid-change-prompt');
            openComposerPicker();
        } },
        dismissButton('catid-change-prompt'),
    ]);
}

function handleCatidAutoCutover(data) {
    if (!forThisStation(data)) return;
    showAlert('catid-auto-cutover', 'plain', 'Cutover was pressed automatically.', [dismissButton('catid-auto-cutover')]);
}

// ── the post-cutover part-id flag ──
//
// After a cutover, if the press's live part id still disagrees with the style it
// was set to, the Edge flags the changeover. The flag is fetched when the
// changeover state moved (postCutoverKey) and when its event names this
// process; the banner offers the two resolutions in place: a one-tap corrective
// changeover to the mapped style, and a link to review the style's expected
// part-id config. "Confirm" clears it.
//
// ONLY WHEN THE KEY MOVED OR THE EVENT ASKED. This is an HTTP request, and it
// used to run on every CHANGED view, which stopped being fine once a flow save
// changed one too: a preset applied to forty parts is forty view changes.
//
// The key is recorded only once a fetch answered, so a failed one is tried
// again on the station's next view fetch, with no timer of its own (a bug
// fixed: it was latched before the fetch and never retried).
let lastPostCutoverKey = null;

function postCutoverKey(view) {
    const co = view.active_changeover;
    return [
        view.process ? view.process.id : 0,
        view.process ? view.process.active_style_id : 0,
        co ? co.id : 0,
        co ? co.state : '',
    ].join(':');
}

let postCutoverFetch = null;

function checkPostCutoverFlag() {
    const view = getView();
    if (!view || !view.process || onBoard()) { removeAlert('postcutover'); lastPostCutoverKey = null; return; }
    const key = postCutoverKey(view);
    if (key === lastPostCutoverKey || postCutoverFetch) return;
    const pid = view.process.id;
    postCutoverFetch = (async function () {
        try {
            const res = await fetch('/api/processes/' + pid + '/post-cutover-flag');
            if (!res.ok) return;
            const body = await res.json();
            lastPostCutoverKey = key;
            if (!body || !body.flagged || !body.flag) { removeAlert('postcutover'); return; }
            renderPostCutoverFlag(pid, body.flag);
        } catch (err) {
            console.error('checkPostCutoverFlag', err);
        } finally {
            postCutoverFetch = null;
        }
    })();
}

// recheckPostCutoverFlag: the flag was raised (changeover-verify-mismatch
// names this process) though the changeover state did not move; the next view
// fetch asks again.
function recheckPostCutoverFlag() { lastPostCutoverKey = null; }

// clearPostCutoverFlag: changeover-verify-cleared, a confirm made on any
// screen; every board of the process takes its banner down.
function clearPostCutoverFlag(data) {
    const view = getView();
    if (!view || !view.process || (data && data.process_id != null && data.process_id !== view.process.id)) return;
    removeAlert('postcutover');
}

function renderPostCutoverFlag(pid, flag) {
    const buttons = [];
    // Resolution 1 — one-tap corrective changeover to the mapped style.
    if (flag.has_mapped) {
        buttons.push({ label: 'Change over to ' + flag.mapped_style_name, primary: true, onClick: async function (btn) {
            btn.disabled = true;
            const ok = await postAction('/api/processes/' + pid + '/changeover/start', {
                to_style_id: flag.mapped_style_id,
                called_by: 'post-cutover-correction',
                notes: 'corrective changeover from post-cutover part-id mismatch',
            }, loadView);
            // A refused or unanswered POST gives the button back.
            if (!ok) { btn.disabled = false; return; }
            showToast('Corrective changeover to ' + flag.mapped_style_name + ' started', 'success');
            removeAlert('postcutover');
        } });
    }
    // Resolution 2 — review the style's expected part-id config.
    buttons.push({ label: 'Review part-id config', onClick: function () { window.open('/processes', '_blank'); } });
    // Confirm the press is correct → clear the flag.
    buttons.push({ label: 'Confirm — process is correct', onClick: async function (btn) {
        btn.disabled = true;
        const ok = await postAction('/api/processes/' + pid + '/post-cutover-flag/confirm', {}, loadView);
        if (!ok) { btn.disabled = false; return; }
        removeAlert('postcutover');
        // The flag moved without the changeover state moving, so the key has
        // to be cleared or the next view would skip the re-check.
        lastPostCutoverKey = null;
    } });
    showAlert('postcutover', 'red', flag.message, buttons);
}

// ─── Wire sub-module callbacks (one-way, breaks the import cycle) ───

setRenderRefs({ openModal, openLoadBin, loadView });
setModalLoadView(loadView);
setLoadView(loadView);
setReleaseRefs({ renderModal, modalOpening, closeModalFrom, loadView });

// ─── SSE ───
//
// The station takes its events from the shared bus (shared/utils.js onSSE):
// one EventSource for every type, reconnecting after 1 s and doubling to 10 s,
// back to 1 s on each connect, and reloading the kiosk when a connect or a
// heartbeat names a new build (setSSEReloadOnBuild). Every type is subscribed
// here, before the bus's first connect, so the stream opens once (the bus
// reopens it when a type is added after connect). Pinned by
// operator-refresh.test.js.
//
// The stream names this station (the Edge marks its screen online on connect
// and on every heartbeat: the Processes page's "online"), asks only for the
// types below (the Edge honours ?topics=), and is treated as dead after 75 s
// with no frame (two and a half heartbeats).
//
// Each event refreshes this station only when it names something the station
// draws (onEvent, eventConcerns); a toast shows only there.

setSSEReloadOnBuild(true);
setSSEQuery({ station: stationID });
const streamWatchdogMs = 75000;
setSSEWatchdog(streamWatchdogMs);
const stationEvents = {
    'order-update': onEvent('order-update'),
    'counter-update': onEvent('counter-update'),
    'changeover-update': onEvent('changeover-update'),
    // The Edge's coalesced nudge after any write, and the engine's doors with
    // no scoped event (EventHub.Nudge): every station.
    'material-refresh': onEvent('material-refresh'),
    // Core's node list applied (the loaders and the catalog ride the same
    // apply, and nudge too).
    'core-nodes': onEvent('core-nodes'),
    // The Edge's copy of Core's node bins changed at the nodes it names.
    'bins': onEvent('bins'),
    // A faulted order on one of this station's tiles.
    'order-faulted': onEvent('order-faulted'),
    // The Edge's copies of Core's containment and refusal state, and a
    // sourcing report, each changed.
    'containment': onEvent('containment'),
    'supply-refusal': onEvent('supply-refusal'),
    'sourcing': onEvent('sourcing'),
    // order-failed also fires a sticky error toast so the operator sees the
    // failure even if they have looked away (fleet failure, admin terminate,
    // a structural resolver error), not only on the next view refresh.
    'order-failed': onEvent('order-failed', handleOrderFailed),
    // Parked ticks piling up on an unbound node: refresh so the tile's chip
    // appears now, and one toast per alarm window with the instruction.
    'uop-stranded-alarm': onEvent('uop-stranded-alarm', handleUopStranded),
    // A post-cutover part-id disagreement was flagged: refresh so
    // checkPostCutoverFlag fetches and shows the prompt.
    'changeover-verify-mismatch': onEvent('changeover-verify-mismatch', recheckPostCutoverFlag),
    // The part-change alerts and the flag's clearing: drawn, not rebuilt.
    'catid-mismatch': handleCatidMismatch,
    'catid-change-prompt': handleCatidChangePrompt,
    'catid-auto-cutover': handleCatidAutoCutover,
    'changeover-verify-cleared': clearPostCutoverFlag,
};
Object.keys(stationEvents).forEach(function (type) { onSSE(type, stationEvents[type]); });
// EVERY connect fetches, the first included: what changed before the stream
// was up (or while it was down) was announced to nobody on this screen.
onSSE('connected', function () {
    scheduleRefresh();
    streamChanged(true);
});
// The stream dropped, or its watchdog found it silent: the sign says the
// server is not answering until it is back (the bus reconnects by itself).
onSSE('disconnected', function () { streamChanged(false); });
// A frame the server queued never arrived (the heartbeat's number is ahead of
// the last one seen): fetch what it would have said.
onSSE('stream-gap', function () { scheduleRefresh(); });
// A failed fetch is tried again on the server's heartbeat, and only then.
onSSE('heartbeat', function () { if (!fetchOK) scheduleRefresh(); });
// A screen that comes back into view fetches: a browser may have throttled or
// frozen the tab while it was hidden.
document.addEventListener('visibilitychange', function () {
    if (document.visibilityState === 'visible') scheduleRefresh();
});

// ─── Init ───

loadView();

// Re-layout on resize (orientation change, window resize).
window.addEventListener('resize', function() { if (getView()) renderGrid(); });

// THE MINUTE TICK. The time-relative words (the node grid's ETA pills, an open
// node modal's waiting line) are computed at render from the held view. They
// used to move only because the view churned every 15 s; an unchanged view now
// answers 304 and draws nothing, so this redraws them once a minute from the
// held view and fetches nothing. On the board (every tile a loader's) the band
// diff keeps every band: it draws no relative time. Pinned by
// operator-refresh.test.js.
const minuteMs = 60000;
setInterval(function () {
    if (!getView()) return;
    renderGrid();
    const sid = getSelectedNodeID();
    const entry = sid !== null ? findNodeByID(sid) : null;
    if (entry && !isReleasePromptOpen()) renderModal(entry);
}, minuteMs);

// Tells operator-display.html's boot watchdog the module graph linked and ran
// to the end. Last statement in the file on purpose: anything earlier could be
// set by a module that later threw. If a link error means this never runs, the
// watchdog arms and reloads the board once a fixed build is deployed.
window.__osBooted = true;
