import { el, fillColor, postAction, showToast, fetchWithTimeout, formatETA } from './operator-util.js';
import { formatClock } from '/static/shared/utils.js';
import { getView, claimedNodes, isReplenishing, getLink } from './operator-state.js';
import { isActive } from './order-status.js';
import { ROLE_WORDS, clearRunning, CLEAR_RUNNING } from './operator-window-state.js';
import { loaderOf, carriersOf, isDedicated, isUnloaderWindow, boardLayout, isBoard, staleSign } from './operator-board-model.js';
import { clearChoices, choiceLabel, heightWords, isLit, clearHighlight } from './operator-board-model.js';
import { windowWords } from './operator-board-model.js';
import { renderBoard, resetBoard } from './operator-board.js';
import { mountFlowPanel, openFlowPanel, syncFlowPanel } from './operator-flow.js';
import { ensureComposer } from './composer-boot.js';
import { confirmResumeKeepStaged } from './operator-supply-refusal.js';
import { openKeypad } from './operator-keypad.js';

const grid = document.getElementById('os-grid');
const headerInfo = document.getElementById('os-header-info');
const headerActions = document.getElementById('os-header-actions');
const footerStatus = document.getElementById('os-footer-status');
const footerBadge = document.getElementById('os-footer-badge');

let openModalRef = null;
let openLoadBinRef = null;
let loadViewRef = null;

export function setRenderRefs(refs) {
    openModalRef = refs.openModal;
    openLoadBinRef = refs.openLoadBin;
    loadViewRef = refs.loadView;
}

// ─── Header ───

// stationState is the one reading of what the station is doing, shared by the
// header line, the style chip, the footer badge and the empty grid so they
// cannot disagree. Both of its meanings are already in the poll: a changeover is
// the process's target style differing from its active style (ActiveChangeover
// is present while it runs), and the running part is current_style. The
// Processes page's list column makes the same derivation.
function stationState(view) {
    const process = view.process || {};
    const changing = !!(process.target_style_id &&
        process.target_style_id !== process.active_style_id);
    const current = view.current_style || null;
    const words = changing ? 'Changing over'
        : (current ? 'Running ' + current.name : 'No part running');
    return { changing: changing, current: current, words: words };
}

export function renderHeader() {
    const view = getView();
    const state = stationState(view);
    const styleName = state.current ? state.current.name : 'No part running';
    const targetName = view.target_style ? view.target_style.name : null;
    const fromTo = targetName
        ? (state.current ? state.current.name + ' \u2192 ' : 'to ') + targetName
        : '';
    headerInfo.textContent = view.process.name + ' - ' + state.words +
        (state.changing && fromTo ? ' ' + fromTo : '');

    headerActions.innerHTML = '';

    // The board (a station whose every tile is a loader's) draws its own top
    // line and hides this header: a forklift's screen carries no REFRESH and
    // no cell buttons, and its acts are its cards and its board bar.
    if (isBoard(claimedNodes())) return;

    // The stale sign and the dot derived from it (staleSign): the dot read
    // health_status, which only the station's own stream writes, so it was
    // "online" by construction (a bug fixed).
    const sign = stationSign();
    headerActions.appendChild(el('span', { className: 'os-health-badge ' + sign.dot }));
    headerActions.appendChild(el('span', { className: 'os-stale-sign os-stale-sign--' + sign.dot, textContent: sign.words.join(' · ') }));

    // Active style chip — sits next to the changeover button so the operator
    // can see which style is running. During changeover shows "current → target".
    // The chip that names the running style is also the door to the picture
    // of its flow (operator-flow.js): a tap opens the read-only cell picture
    // over the board, its Board button comes back. No new button on the
    // board for it.
    const styleChip = el('div', { className: 'os-header-style os-header-style--tap' + (targetName ? ' changing' : ''), role: 'button', tabIndex: 0 });
    styleChip.setAttribute('aria-label', 'Show the flow of ' + styleName);
    styleChip.appendChild(el('span', { className: 'os-header-style-label', textContent: 'STYLE' }));
    const styleValue = el('span', { className: 'os-header-style-value' });
    styleValue.textContent = targetName ? styleName + ' \u2192 ' + targetName : styleName;
    styleChip.appendChild(styleValue);
    styleChip.addEventListener('click', openFlowPanel);
    headerActions.appendChild(styleChip);
    mountFlowPanel();
    syncFlowPanel(view);

    // A station that mixes loader and press tiles keeps the node grid and
    // this header (boardLayout's header).
    //
    // HMI Tier 2 (post-F'-Phase-2): the changeover-wide RELEASE header
    // button was removed. Each node tile is the release surface during
    // changeover — operator clicks the tile, gets the same production
    // release modal, picks disposition (or accepts the auto-detected
    // pre-fill), submits. Phase 2's deferred-supply chain auto-releases
    // the supply leg when the evac robot picks up.
    //
    // CUTOVER button: ALWAYS shown during an active changeover.
    //
    // It used to be hidden when the process had PLC-driven cutover enabled,
    // on the reasoning that a manual button would create ambiguity about who
    // owns cutover. That reasoning assumed the PLC path worked. It never did
    // — no plant wired Changeover_Active — so enabling the flag removed the
    // operator's only way to finish a changeover and put nothing in its
    // place, stranding it with neither an automatic nor a manual completion.
    // PLC-driven cutover is now removed entirely; CATID auto-arm handles the
    // START side and the operator always owns the FINISH.
    const header = boardLayout(claimedNodes()).header;
    if (header === 'pull-full-from-home') {
        // A dedicated home loader beside a press: the header's act is a home's
        // clear (the full pulled, the partial brought home), not a changeover.
        // (On the board its cards say it, in Clear partials.)
        headerActions.appendChild(headerBtn('PULL FULL FROM HOME', 'pull-full-home', openPullFullFromHomePicker));
    } else if (header === 'changeover') {
        if (view.active_changeover) {
            headerActions.appendChild(headerBtn('CUTOVER', 'cutover', confirmCutover));
            // CANCEL during active changeover: aborts every in-flight evac+
            // supply order on the process's node tasks, marks the changeover
            // row cancelled, and resets the process back to active_production
            // on the from-style. Core handles safe resolution of orders
            // already mid-route (queued → disappears, loaded robot → store-
            // order rerouted to a safe drop). Operator wraps this in a
            // confirmation modal because the action is destructive.
            headerActions.appendChild(headerBtn('CANCEL', 'cancel-changeover', confirmCancelChangeover));
        } else {
            headerActions.appendChild(headerBtn('CHANGEOVER', 'changeover', openComposerPicker));
        }
    }

    headerActions.appendChild(headerBtn('REFRESH', 'refresh', loadViewRef));
}

// CHANGEOVER OPENS THE PICK-A-PART MODAL on every station (pick-a-part U4):
// tap the part, say one off or two, Start. It is the only changeover entry —
// the flat style list that used to be its load fallback went with it.
//
// The modal's JS and CSS are not in the boot payload; the first tap fetches
// them and every tap after that is synchronous (composer-boot.js). A load
// that failed says so and the next tap tries again — a Pi that lost one
// request is not left without a changeover until it is rebooted.
// Exported for the part-change prompt's Start changeover (operator.js).
export function openComposerPicker() {
    return ensureComposer().then(function (ui) {
        if (ui) ui.openPicker(getView());
        else showToast('The changeover screen did not load — tap CHANGEOVER again', 'error');
    });
}


async function confirmCancelChangeover() {
    const view = getView();
    const pid = view.process.id;
    const co = view.active_changeover;
    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker' });
    panel.appendChild(el('div', { className: 'os-co-picker-title',
        textContent: 'Cancel changeover to ' + (co.to_style_name || 'target') + '?' }));
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
        textContent: 'In-flight robots will be recalled. Loaded bins are routed to safe storage.' }));

    const confirm = el('button', { className: 'os-co-picker-btn danger', textContent: 'CANCEL CHANGEOVER' });
    confirm.addEventListener('click', async () => {
        overlay.remove();
        const ok = await postAction('/api/processes/' + pid + '/changeover/cancel', {}, loadViewRef);
        if (ok) showToast('Changeover cancelled', 'success');
    });
    panel.appendChild(confirm);

    const dismiss = el('button', { className: 'os-co-picker-btn cancel', textContent: 'KEEP CHANGEOVER' });
    dismiss.addEventListener('click', () => overlay.remove());
    panel.appendChild(dismiss);

    overlay.appendChild(panel);
    overlay.addEventListener('click', evt => { if (evt.target === overlay) overlay.remove(); });
    showOverlay(overlay);
}

async function confirmCutover() {
    const view = getView();
    const pid = view.process.id;
    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker' });
    const co = view.active_changeover;
    panel.appendChild(el('div', { className: 'os-co-picker-title',
        textContent: 'Complete cutover to ' + (co.to_style_name || 'target') + '?' }));

    const confirm = el('button', { className: 'os-co-picker-btn', textContent: 'CONFIRM CUTOVER' });
    confirm.addEventListener('click', async () => {
        overlay.remove();
        const ok = await postAction('/api/processes/' + pid + '/changeover/cutover', undefined, loadViewRef);
        if (ok) showToast('Cutover complete', 'success');
    });
    panel.appendChild(confirm);

    const cancel = el('button', { className: 'os-co-picker-btn cancel', textContent: 'CANCEL' });
    cancel.addEventListener('click', () => overlay.remove());
    panel.appendChild(cancel);

    overlay.appendChild(panel);
    overlay.addEventListener('click', evt => { if (evt.target === overlay) overlay.remove(); });
    showOverlay(overlay);
}

// showOverlay puts a confirm or picker on screen in place of any other one
// open (a second activation of a still-focused card stacked a second confirm
// for the same request). The supply refusal's question is not a confirm and
// stays. Pinned by operator-safe-taps.test.js.
function showOverlay(overlay) {
    document.querySelectorAll('.os-co-picker-overlay').forEach(function(o) {
        if (!o.classList.contains('os-refusal-overlay')) o.remove();
    });
    document.body.appendChild(overlay);
}

function headerBtn(label, cls, onClick) {
    const btn = el('button', { className: 'os-header-btn ' + cls, textContent: label });
    btn.addEventListener('click', onClick);
    return btn;
}

// ─── Grid ───

// boardActs is the board's door to each panel (operator-board.js): an act from
// boardModel opens what the old boards' tap opened, so it posts what it posted.
// The board bar's two requests post once a tap: busy(act) is true while one is
// in flight, and the bar draws its button disabled (operator-safe-taps.test.js).
const barPosting = new Set();
function barKey(a) { return a.kind + ':' + a.node; }
function postFromBar(a, url, body) {
    const key = barKey(a);
    if (barPosting.has(key)) return;
    barPosting.add(key);
    renderGrid();
    postAction(url, body, loadViewRef).finally(function() {
        barPosting.delete(key);
        renderGrid();
    });
}
const boardActs = {
    act: function (a) {
        switch (a.kind) {
        case 'load': openLoadBinRef(a.node, a.codes, 0); break;
        case 'unload': confirmUnloadSwap(a.node, a.carriers, a.discarding, a.leavesBare, a.lit); break;
        case 'unload-held': confirmUnloadHeld(a.node, a.heldFor, a.count); break;
        case 'push-empty': confirmPushEmpty(a.node, a.carriers, a.bare, a.lit); break;
        case 'clear-home': confirmClearLoaderHome(a.node, a.home, a.spot); break;
        case 'modal': openModalRef(a.node); break;
        case 'request-empty': postFromBar(a, '/api/process-nodes/' + a.node + '/request-empty', {}); break;
        case 'request-full': postFromBar(a, '/api/process-nodes/' + a.node + '/request-full', { payload_code: a.payload }); break;
        case 'pull-from-market': showPullFromMarketPicker(a.node); break;
        }
    },
    busy: function (a) { return barPosting.has(barKey(a)); },
    rerender: function () { renderGrid(); },
};

// emptyGridWords says why the grid has no tiles: the screen has no positions
// at all, or it has positions and no part is running on any of them.
function emptyGridWords(view) {
    if (!view || !view.nodes || view.nodes.length === 0) return 'No positions on this screen';
    const state = stationState(view);
    if (!state.changing && !state.current) return 'No part running \u2014 start a changeover to pick one';
    return state.words + ' \u2014 no position on this screen is set up for it';
}

export function renderGrid() {
    const nodes = claimedNodes();

    // Fires on the poll, from server state alone. Guarded internally against
    // asking twice, and it asks nothing once answered.
    maybeShowRefusalModal();

    // A station whose every tile is a loader's draws the board
    // (operator-board.js), which keeps each band it can and replaces only what
    // changed; every other station the node grid, drawn whole.
    if (isBoard(nodes)) {
        document.body.classList.add('os-pc-active');
        grid.classList.add('os-pc-board');
        grid.style.removeProperty('--os-cols');
        grid.style.removeProperty('--os-rows');
        renderBoard(grid, getView(), {
            width: window.innerWidth || 1024,
            height: window.innerHeight || 768,
        }, boardActs, shortTime, stationSign());
        return;
    }
    document.body.classList.remove('os-pc-active');
    grid.classList.remove('os-pc-board', 'os-pc-step-0', 'os-pc-step-1', 'os-pc-step-2', 'os-pc-showall');
    resetBoard();
    grid.innerHTML = '';

    if (nodes.length === 0) {
        grid.style.removeProperty('--os-cols');
        grid.style.removeProperty('--os-rows');
        const empty = el('div', { id: 'os-grid-empty', textContent: emptyGridWords(getView()) });
        grid.appendChild(empty);
        return;
    }

    const { cols, rows } = gridDimensions();
    grid.style.setProperty('--os-cols', cols);
    grid.style.setProperty('--os-rows', rows);

    for (const entry of nodes) {
        grid.appendChild(createNodeButton(entry));
    }
}

// partialLine is the one rendering rule for a carrier left unfinished: its part
// set, its count above 0 and below the part's capacity, wherever it stands. A
// held one (waiting in its partial home) says whose and since when; one at its
// line says it is being finished. Empty for every other carrier. The station's
// bin carries no count (stationBinState): n is the tile's own count
// (runtime.remaining_uop_cached), and a held carrier's is Core's held_uop, which
// does not tick. Nothing is inferred from the order that brought it.
function partialLine(bs, atLine, count) {
    if (!bs || !bs.occupied || !bs.payload_code || !(bs.uop_capacity > 0)) return '';
    var n = bs.held_since ? bs.held_uop : count;
    if (!(n > 0 && n < bs.uop_capacity)) return '';
    var s = 'PARTIAL — ' + n + ' of ' + bs.payload_code;
    if (bs.held_since) {
        s += ', held for ' + (bs.held_for || 'its line') + ', since ' + heldSinceText(bs.held_since);
    } else if (atLine) {
        s += ', finishing';
    }
    return s;
}

// heldSinceText is a held partial's arrival as the floor reads a time.
function heldSinceText(iso) {
    var d = new Date(iso);
    if (isNaN(d.getTime())) return iso;
    return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

// confirmUnloadHeld is a held partial's tap: no CLEAR, because the count is
// what makes it worth calling back to its line. UNLOAD is the second tap for a
// plant that ships it anyway; Core books it as any clear.
function confirmUnloadHeld(nodeID, line, count) {
    var overlay = el('div', { className: 'os-co-picker-overlay' });
    var panel = el('div', { className: 'os-co-picker os-co-picker--one-tap' });
    panel.appendChild(el('div', { className: 'os-co-picker-title', textContent: 'Partial, held for ' + (line || 'its line') }));
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
        textContent: 'It waits here until the line calls it back to be finished.' }));
    // COUNT declares what the carrier holds now (a person counted it, or topped
    // it up): the number goes to Core, which records it as this carrier's count.
    var countBtn = el('button', { className: 'os-co-picker-btn', textContent: 'COUNT' });
    countBtn.addEventListener('click', function() {
        overlay.remove();
        openKeypad(nodeID, count || 0, {
            title: 'Count on this carrier',
            onOk: function(_n, qty) {
                return postAction('/api/process-nodes/' + nodeID + '/record-count', { actual_uop: qty }, loadViewRef);
            },
        });
    });
    panel.appendChild(countBtn);
    var go = el('button', { className: 'os-co-picker-btn', textContent: 'UNLOAD' });
    go.addEventListener('click', function() {
        overlay.remove();
        postAction('/api/process-nodes/' + nodeID + '/clear-bin', { unload: true }, loadViewRef);
    });
    panel.appendChild(go);
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
        textContent: 'Unload clears it and books what it held.' }));
    var cancel = el('button', { className: 'os-co-picker-btn cancel', textContent: 'CANCEL' });
    cancel.addEventListener('click', function() { overlay.remove(); });
    panel.appendChild(cancel);
    overlay.appendChild(panel);
    overlay.addEventListener('click', function(evt) { if (evt.target === overlay) overlay.remove(); });
    document.body.appendChild(overlay);
}

// confirmUnloadSwap gates the unloader swap behind an explicit operator
// confirmation. The swap (clear-bin) is irreversible mid-shift — it confirms
// the inbound full was received, sends the empty back, and pulls the next full
// — so the operator must declare the full pulled / bin empty before it fires.
// Reuses the os-co-picker overlay styling already used by the changeover/cutover
// confirmations so the look is consistent.
//
// carriers is the tile's carrier list (loader.carriers): the bin types Core's
// catalog gives the window's parts, or every type in the catalog when its parts
// match none, so the operator is never stuck in front of an empty menu on a
// node nobody has finished configuring (service carriersFor). Empty when Core
// sent no catalog: the panel then asks no type.
//
// discarding is the count the clear is about to throw away — the Edge's own
// remaining_uop_cached for the carrier in the window. The overlay says the
// number because the clear does not refuse on it and should not: an unloader
// clears full finished bins by design, and Core records the discard durably in
// bin_uop_ledger as clear_for_reuse. What was missing was the operator seeing
// what they were discarding at the moment they discarded it. Pass 0 or null
// when there is nothing to say and the line is omitted.
//
// leavesBare is the tile's loader.leaves_bare: true only at a window of the
// stage-1 half of a two-stage unloader. There the cart goes on to stage 2 with
// no bin on it, so there is nothing to pick — the CLEAR is one tap, Full off,
// and posts a blank code; Core stamps the cart from its own type. The panel
// names no type: the marker Core stamps is bookkeeping, never an operator's word.
//
// lit is the choice the board's card says to send back, {bin_type, layers?}
// (clearHighlight): that button is lit, every button still tappable.
//
// HEIGHTS (clearChoices): a type the empties group wants heights of shows the
// bin's name (the plain type, posting no layers), the bin's name with each
// wanted height ("TOTE-1 · 3 high"), and, where the group has a "Tops &
// bottoms" row, "TOTE-1 · tops & bottoms" (layers 0); each height posts the
// CLEAR with its type and layers. A type with no wanted heights keeps its one
// button and posts no layers, as before. Pinned by
// operator-render-clear-panels.test.js.
//
// Exported for the node modal's CLEAR BIN on an unloader, which goes through
// this panel (operator-modal.js).
export function confirmUnloadSwap(nodeID, carriers, discarding, leavesBare, lit) {
    if (leavesBare) {
        confirmFullOff(nodeID, discarding);
        return;
    }
    const choices = clearChoices(getView(), carriers);

    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker' });
    panel.appendChild(el('div', { className: 'os-co-picker-title', textContent: 'Full pulled, empty filled?' }));
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
        textContent: 'Confirms the bin is unloaded. The empty returns to the supermarket and the next full is requested.' }));
    if (discarding > 0) {
        panel.appendChild(el('div', { className: 'os-co-picker-subtitle os-co-picker-warn',
            textContent: 'This slot still counts ' + discarding + '. Confirming writes it to zero.' }));
    }

    // Single-type stations with no heights auto-fill: skip the picker and send
    // the code directly on CONFIRM SWAP so the dunnage is still recorded
    // without forcing the operator to tap a one-item menu.
    const autoCode = choices.length === 1 && typeof choices[0].layers !== 'number' ? choices[0].bin_type : null;
    if (choices.length > 0 && !autoCode) {
        // Two or more choices: show the picker so the operator selects one.
        panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
            textContent: 'Select dunnage type for this carrier:' }));
        choices.forEach(function(choice) {
            const btn = el('button', { className: 'os-co-picker-btn' + (isLit(choice, lit) ? ' os-co-picker-btn--lit' : ''),
                textContent: choiceLabel(choice) });
            btn.addEventListener('click', function() {
                overlay.remove();
                postAction('/api/process-nodes/' + nodeID + '/clear-bin', clearBody(choice), loadViewRef);
            });
            panel.appendChild(btn);
        });
    } else {
        // No catalog / exactly one type (auto-fill): one button.
        const confirm = el('button', { className: 'os-co-picker-btn', textContent: 'CONFIRM SWAP' });
        confirm.addEventListener('click', function() {
            overlay.remove();
            postAction('/api/process-nodes/' + nodeID + '/clear-bin',
                autoCode ? { bin_type_code: autoCode } : undefined, loadViewRef);
        });
        panel.appendChild(confirm);
    }

    const cancel = el('button', { className: 'os-co-picker-btn cancel', textContent: 'CANCEL' });
    cancel.addEventListener('click', () => overlay.remove());
    panel.appendChild(cancel);

    overlay.appendChild(panel);
    overlay.addEventListener('click', evt => { if (evt.target === overlay) overlay.remove(); });
    showOverlay(overlay);
}

// clearBody is the clear-bin body a CLEAR choice posts: its type, and its
// layers where it names a height.
function clearBody(choice) {
    const body = { bin_type_code: choice.bin_type };
    if (typeof choice.layers === 'number') body.layers = choice.layers;
    return body;
}

// oneTapPanel is the two-stage unloader's panel: a title, what to do, one
// button that posts clear-bin with a blank code, the line saying what happens
// next, and CANCEL. No type appears on it anywhere — Core stamps the cart from
// its own type, so there is nothing for the operator to pick or read.
//
// heights, when given, are the wanted heights offered beside the one button
// (stage 2's Send on for a type the group wants heights of): the label alone,
// posting no layers as it always has, then the label with each height ("Send
// on · 3 high", "Send on · tops & bottoms"), each posting the CLEAR with its
// layers and still no type, so no type is named on the panel.
function oneTapPanel(nodeID, title, subtitles, label, after, heights, lit) {
    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker os-co-picker--one-tap' });
    panel.appendChild(el('div', { className: 'os-co-picker-title', textContent: title }));
    subtitles.forEach(function(s) { panel.appendChild(s); });
    const many = heights && heights.length > 0;
    const plainLit = many && !!lit && typeof lit.layers !== 'number';
    const go = el('button', { className: 'os-co-picker-btn' + (plainLit ? ' os-co-picker-btn--lit' : ''), textContent: label });
    go.addEventListener('click', function() {
        overlay.remove();
        postAction('/api/process-nodes/' + nodeID + '/clear-bin', undefined, loadViewRef);
    });
    panel.appendChild(go);
    (heights || []).forEach(function(choice) {
        const btn = el('button', { className: 'os-co-picker-btn' + (isLit(choice, lit) ? ' os-co-picker-btn--lit' : ''),
            textContent: label + ' · ' + heightWords(choice.layers) });
        btn.addEventListener('click', function() {
            overlay.remove();
            postAction('/api/process-nodes/' + nodeID + '/clear-bin', { layers: choice.layers }, loadViewRef);
        });
        panel.appendChild(btn);
    });
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle', textContent: after }));
    const cancel = el('button', { className: 'os-co-picker-btn cancel', textContent: 'CANCEL' });
    cancel.addEventListener('click', () => overlay.remove());
    panel.appendChild(cancel);
    overlay.appendChild(panel);
    overlay.addEventListener('click', evt => { if (evt.target === overlay) overlay.remove(); });
    showOverlay(overlay);
}

// confirmFullOff is stage 1 of a two-stage unloader: the full comes off the
// cart and the cart goes on to stage 2 bare. The discard count is still stated.
// It leaves the cart bare, so it offers no height and posts no layers.
function confirmFullOff(nodeID, discarding) {
    const subs = [];
    if (discarding > 0) {
        subs.push(el('div', { className: 'os-co-picker-subtitle os-co-picker-warn',
            textContent: 'This slot still counts ' + discarding + '. Confirming writes it to zero.' }));
    }
    oneTapPanel(nodeID, 'Full bin on the cart', subs, 'Full off',
        'The cart goes on to Stage 2. The next full is on its way.');
}

// confirmSendOn is stage 2 with a bare cart in the window (bin_state.bare):
// the operator puts an empty bin on it and sends it on. Plain PUSH EMPTY would
// send the cart on still bare, and a PUSH AS picker asks a question the cart
// already answers, so it is one tap — or, where the group wants heights of the
// window's type, one tap per height beside it (the type stays Core's).
function confirmSendOn(nodeID, carriers, lit) {
    const heights = [];
    clearChoices(getView(), carriers).forEach(function (c) {
        if (typeof c.layers === 'number' && !heights.some(function (h) { return h.layers === c.layers; })) heights.push(c);
    });
    oneTapPanel(nodeID, 'Cart, no bin',
        [el('div', { className: 'os-co-picker-subtitle', textContent: 'Put an empty bin on it, then send it on.' })],
        'Send on', 'Goes to the empties supermarket. No type to pick — it\'s the cart it came in as.', heights, lit);
}

// confirmPushEmpty shows a confirmation panel for an empty carrier sitting in an
// unloader window. Tapping PUSH EMPTY fires the push-empty endpoint so the carrier is
// sent to the supermarket and the node can request a fresh full bin.
//
// bare is the carrier's bin_state.bare: a stage-2 cart with no bin on it gets
// the one-tap Send on panel instead (confirmSendOn). lit is the choice the
// board's card says to send it back as: its PUSH AS is lit.
function confirmPushEmpty(nodeID, carriers, bare, lit) {
    if (bare) {
        confirmSendOn(nodeID, carriers, lit);
        return;
    }
    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker' });
    panel.appendChild(el('div', { className: 'os-co-picker-title', textContent: 'Empty bin in slot' }));
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
        textContent: 'Send the empty carrier to the supermarket so a full bin can be requested.' }));

    // PUSH EMPTY declares nothing: the cart keeps its tote number.
    const push = el('button', { className: 'os-co-picker-btn', textContent: 'PUSH EMPTY' });
    push.addEventListener('click', function() {
        overlay.remove();
        postAction('/api/process-nodes/' + nodeID + '/push-empty', undefined, loadViewRef);
    });
    panel.appendChild(push);

    // ── RE-CLEAR: SAY WHAT THIS CARRIER IS, ON THE WAY OUT ──────────────────
    //
    // The swap panel has always let the operator declare the dunnage type,
    // because a clear is where that gets recorded. This panel is the SAME
    // window one state later — the carrier is already empty — and it had no way
    // to say anything about the carrier at all. So a carrier Core has typed
    // wrong leaves typed wrong, and the operator standing in front of it, who
    // can see what it is, has no way to correct the record.
    //
    // It posts the CLEAR endpoint, not push-empty, and that is the whole
    // mechanism: a clear on an already-empty carrier discards nothing (Core
    // records "discarded 0 parts"), writes the declared bin type (and, for a
    // wanted height, the height), and fires the same empty-out move push-empty
    // would have. Re-clearing IS pushing empty, plus the one fact this panel was
    // missing.
    //
    // THE OPERATOR IS ASSERTING, NOT GUESSING. This is the same reading
    // RecoverTransitAnomaly takes — a person looking at the thing is believed —
    // and it is NOT a way to argue with the Allowed Bin Types fence. Declaring
    // a carrier's real type is a correction; retyping a carrier so a refused
    // order will go through would be a lie about a physical object, and this
    // panel only ever offers the types the node's payloads actually use.
    const choices = clearChoices(getView(), carriers);
    if (choices.length > 0) {
        panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
            textContent: 'Or push it out and correct the carrier type:' }));
        choices.forEach(function(choice) {
            const btn = el('button', { className: 'os-co-picker-btn' + (isLit(choice, lit) ? ' os-co-picker-btn--lit' : ''),
                textContent: 'PUSH AS ' + choiceLabel(choice) });
            btn.addEventListener('click', function() {
                overlay.remove();
                postAction('/api/process-nodes/' + nodeID + '/clear-bin', clearBody(choice), loadViewRef);
            });
            panel.appendChild(btn);
        });
    }

    const cancel = el('button', { className: 'os-co-picker-btn cancel', textContent: 'CANCEL' });
    cancel.addEventListener('click', () => overlay.remove());
    panel.appendChild(cancel);

    overlay.appendChild(panel);
    overlay.addEventListener('click', evt => { if (evt.target === overlay) overlay.remove(); });
    showOverlay(overlay);
}

// confirmClearLoaderHome shows the confirm for a home's clear. The driver
// pulls the full first, then taps: one request, and Core records the zero and
// decides what brings the partial home (a waiting pull, an open buffer spot, or
// not yet); the server answers with one sentence saying which
// (handlers_operator_bins.go). From the
// board's card the confirm names the home and the spot the partial stands on
// (tile.buffer_partial); the header's picker names no spot. The buttons go at
// the tap, so the tap sends one request. Recorded, the confirm closes and the
// sentence is the board's success toast; refused, the confirm stays and shows
// the refusal in place of its buttons.
function confirmClearLoaderHome(nodeID, home, spot) {
    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker' });
    panel.appendChild(el('div', { className: 'os-co-picker-title',
        textContent: home ? 'Clear ' + home + '?' : 'Clear this home bin?' }));
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
        textContent: home && spot
            ? 'Pull the full from ' + home + ' first. CONFIRM records it; Shingo brings the partial on ' + spot + ' home when it can.'
            : 'Pull the full from this home first. CONFIRM records it.' }));

    const confirm = el('button', { className: 'os-co-picker-btn', textContent: 'CONFIRM CLEAR' });
    confirm.addEventListener('click', async function() {
        confirm.remove();
        cancel.remove();
        let refused = false;
        let said = '';
        const ok = await postAction('/api/process-nodes/' + nodeID + '/clear-loader-home', undefined, loadViewRef, {
            onRefused: function(msg) {
                refused = true;
                panel.appendChild(el('div', { className: 'os-co-picker-subtitle', textContent: msg }));
            },
            onResult: function(result) { said = (result && result.message) || ''; },
        });
        if (refused) return;
        overlay.remove();
        if (ok && said) showToast(said, 'success');
    });
    panel.appendChild(confirm);

    const cancel = el('button', { className: 'os-co-picker-btn cancel', textContent: 'CANCEL' });
    cancel.addEventListener('click', () => overlay.remove());
    panel.appendChild(cancel);

    overlay.appendChild(panel);
    overlay.addEventListener('click', evt => { if (evt.target === overlay) overlay.remove(); });
    showOverlay(overlay);
}

// openPullFullFromHomePicker is the header entry point to a home's clear. It
// lists ONLY the homes the board offers the clear on — a loaded home ("full")
// whose loader buffer holds a matching partial (the same gate as the Clear
// partials card). Tapping one opens the clear's confirm. It is
// offered on a dedicated loader beside a press (the node grid); the
// board says the same in its Clear partials cards.
function openPullFullFromHomePicker() {
    var eligible = claimedNodes().filter(function(n) {
        var bs = (n && n.bin_state) || {};
        return isDedicated(n) && bs.occupied && bs.payload_code && !!n.buffer_partial;
    });

    var overlay = el('div', { className: 'os-co-picker-overlay' });
    var panel = el('div', { className: 'os-co-picker' });
    panel.appendChild(el('div', { className: 'os-co-picker-title', textContent: 'Pull full from home' }));
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
        textContent: 'Pick the home whose full you pulled. Shingo records it and brings the partial home when it can.' }));

    if (eligible.length === 0) {
        panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
            textContent: 'No home has a matching buffer partial ready right now.' }));
    }
    eligible.forEach(function(n) {
        var bs = n.bin_state || {};
        var btn = el('button', { className: 'os-co-picker-btn' });
        btn.style.cssText = 'text-align:left;padding:14px 18px;display:block;width:100%;';
        btn.appendChild(el('div', { textContent: n.node.name,
            style: 'font-size:22px;font-weight:700;letter-spacing:0.03em;' }));
        btn.appendChild(el('div', { textContent: bs.payload_code,
            style: 'font-size:17px;font-weight:600;margin-top:5px;' }));
        btn.addEventListener('click', function() {
            overlay.remove();
            confirmClearLoaderHome(n.node.id, n.node.name, '');
        });
        panel.appendChild(btn);
    });

    var cancel = el('button', { className: 'os-co-picker-btn cancel', textContent: 'CANCEL' });
    cancel.addEventListener('click', function() { overlay.remove(); });
    panel.appendChild(cancel);

    overlay.appendChild(panel);
    overlay.addEventListener('click', function(evt) { if (evt.target === overlay) overlay.remove(); });
    showOverlay(overlay);
}

// showPullFromMarketPicker fetches bins currently sitting in the loader's
// outbound supermarket and presents a picker so the operator can pull one back
// to the loader window. Used during cell launches to recover bins from the
// market before resuming normal empty-request / load-full flow.
// marketBinContents is the pull-back picker's word for what a market bin
// holds, from its row in the bins table (stocked, partial), as the Production
// page's bin modal says it.
function marketBinContents(b) {
    // OWNER-WORDS
    if (!b.stocked) return 'Empty';
    // OWNER-WORDS
    if (b.partial) return 'Partial';
    // OWNER-WORDS
    return b.uop_capacity > 0 ? 'Full' : 'Has parts';
}

function showPullFromMarketPicker(nodeID) {
    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker' });
    panel.appendChild(el('div', { className: 'os-co-picker-title', textContent: 'Pull bin from market' }));
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
        textContent: 'Select a bin in the outbound market to pull back to this loader window.' }));

    const listDiv = el('div', { style: 'margin:8px 0 4px;display:flex;flex-direction:column;gap:10px;' });
    listDiv.textContent = 'Loading…';
    panel.appendChild(listDiv);

    const cancel = el('button', { className: 'os-co-picker-btn cancel', textContent: 'CANCEL' });
    cancel.addEventListener('click', () => overlay.remove());
    panel.appendChild(cancel);

    overlay.appendChild(panel);
    overlay.addEventListener('click', evt => { if (evt.target === overlay) overlay.remove(); });
    showOverlay(overlay);

    // Bounded so a severed connection trips the .catch below ("Could not load
    // market bins.") instead of leaving the picker stuck on "Loading…" forever.
    fetchWithTimeout('/api/process-nodes/' + nodeID + '/market-bins', { credentials: 'same-origin' })
        .then(function(r) { return r.json(); })
        .then(function(bins) {
            listDiv.textContent = '';
            if (!bins || bins.length === 0) {
                listDiv.textContent = 'No bins available in the market.';
                return;
            }
            bins.forEach(function(b) {
                // Strip group prefix — "AMR Supermarket.SMN_0010" → "SMN_0010"
                var nodeShort = b.node_name.replace(/^.*\./, '');

                var btn = el('button', { className: 'os-co-picker-btn' });
                btn.style.cssText = 'text-align:left;padding:14px 18px;display:block;width:100%;';

                var nodeEl = el('div', { textContent: nodeShort });
                nodeEl.style.cssText = 'font-size:22px;font-weight:700;letter-spacing:0.03em;';

                var payloadEl = el('div', { textContent: b.payload_code });
                payloadEl.style.cssText = 'font-size:17px;font-weight:600;margin-top:5px;';

                // The bins table says whether the bin holds parts and fewer
                // than its part's capacity, never how many (marketBinContents).
                var uopEl = el('div', { textContent: marketBinContents(b) });
                uopEl.style.cssText = 'font-size:13px;margin-top:3px;opacity:0.7;';

                btn.appendChild(nodeEl);
                btn.appendChild(payloadEl);
                btn.appendChild(uopEl);

                btn.addEventListener('click', function() {
                    overlay.remove();
                    postAction('/api/process-nodes/' + nodeID + '/pull-from-market',
                        { source_core_node: b.node_name }, loadViewRef);
                });
                listDiv.appendChild(btn);
            });
        })
        .catch(function() {
            listDiv.textContent = 'Could not load market bins.';
        });
}

function gridDimensions() {
    var w = window.innerWidth;
    // Fixed grid per screen size — 7": 2×2, 10": 3×2, 15"+: 4×N.
    if (w <= 1024) return { cols: 2, rows: 2 };
    if (w <= 1400) return { cols: 3, rows: 2 };
    var count = claimedNodes().length || 1;
    return { cols: 4, rows: Math.max(2, Math.ceil(count / 4)) };
}

// unloaderWindowColour maps an unloader window (isUnloaderWindow) to the
// loader-board card palette (not the saturated fill colors): full parked = green,
// full inbound = amber, empty = neutral.
function unloaderWindowColour(entry) {
    const bs = entry.bin_state;
    const awaiting = (entry.orders || []).some(o => isActive(o.status));
    if (bs && bs.occupied && bs.payload_code) return 'os-drain-ready';
    if (awaiting) return 'os-drain-awaiting';
    return 'os-drain-idle';
}

function createNodeButton(entry) {
    const claim = entry.active_claim;
    const loader = loaderOf(entry);
    const runtime = entry.runtime || {};
    const remaining = runtime.remaining_uop_cached != null ? runtime.remaining_uop_cached : 0;
    const capacity = claim ? claim.uop_capacity : 0;

    // Tile background by priority: release-ready (blue) > changeover
    // (orange) > in-transit (purple) > fill state (full/mid/low/empty).
    // Higher-priority states replace the underlying state color so the
    // operator gets a single clear cue per tile rather than a stack of
    // overlays. Replenishing stays as an inset ring (separate "robot in
    // motion" signal that doesn't compete with the click-to-act vs
    // CO-context cues). In-transit covers staged too — staged is a
    // robot parked at its wait point during a two-robot swap, still
    // inbound from the operator's POV; hiding purple at staged would
    // make the tile flicker color right when the robot is closest.
    const releaseReady = isReleaseReady(entry);
    const inChangeover = !!entry.changeover_task;
    const inboundOrders = (entry.orders || []).filter(o =>
        o.status === 'in_transit' || o.status === 'staged'
    );
    const unloaderWindow = isUnloaderWindow(entry);
    let stateClass;
    if (unloaderWindow) {
        // An unloader window owns its OWN palette (idle/awaiting/ready); the
        // generic order-state colors below are loader/press semantics. Critically,
        // an in_transit order there is the OUTbound empty (U2) leaving after a
        // clear, NOT a full arriving — so 'os-in-transit' (purple = inbound) would be
        // a lie. unloaderWindowColour maps any active order to amber 'awaiting'. (An
        // AMR-fed unloader's inbound U1 also reads as amber 'awaiting' — the
        // palette's intent.)
        stateClass = unloaderWindowColour(entry);
    } else if (releaseReady) {
        stateClass = 'os-release-ready';
    } else if (inChangeover) {
        stateClass = 'os-changeover';
    } else if (inboundOrders.length > 0) {
        stateClass = 'os-in-transit';
    } else {
        stateClass = nodeColorClass(entry);
    }
    const btn = el('div', { className: 'os-node-btn ' + stateClass });

    if (isReplenishing(entry)) btn.classList.add('os-replenishing');

    btn.appendChild(el('span', { className: 'os-node-name', textContent: entry.node.name }));

    // Parked-ticks attention badge (P2-C8): draws the operator to tap the tile,
    // where the modal chip shows the full "... Record Count on the bin tab."
    // instruction. Amber, corner-anchored, cleared automatically once a bin binds.
    // The residue of a refusal, once it has been answered. NOT the alert — the
    // modal was the alert, and it is gone. This is what answers "why is this cell
    // stopped" for the operator who walks up ten minutes later, for the incoming
    // shift, and for the team lead, without ever interrupting again.
    var refusedForMe = entry.supply_refused_for_me;
    if (refusedForMe && refusedForMe.answered) {
        const chip = el('span', {
            className: 'os-node-alarm',
            textContent: 'NO ' + refusedForMe.payload_code,
        });
        chip.style.cssText = 'position:absolute;bottom:4px;right:4px;font-size:11px;' +
            'font-weight:700;color:#1a1204;background:#ffd98a;padding:2px 6px;border-radius:4px';
        chip.title = refusedForMe.loader_node + ' cannot supply ' + refusedForMe.payload_code +
            (refusedForMe.refused_by ? ' — ' + refusedForMe.refused_by : '');
        btn.appendChild(chip);
    }

    // UOP-accumulating chip. The sentence carries its own severity: while a swap
    // is in progress it reads as a statement of what is happening, and only once
    // the wait passes the configured limit does it ask for anything ("Record
    // Count"). The chip follows — grey-blue and factual until then, amber after.
    // It used to be amber "⚠ NOT BOUND" from the first second, which is an alarm
    // face on the normal case, and an alarm that is usually nothing gets ignored
    // when it is something.
    if (entry.stranded_alarm) {
        const asking = entry.stranded_alarm.indexOf('Record Count') !== -1;
        const alarm = el('span', {
            className: 'os-node-alarm',
            textContent: asking ? '⚠ NOT BOUND' : 'UOP ACCUM',
        });
        alarm.style.cssText = 'position:absolute;bottom:4px;left:4px;font-size:11px;' +
            'font-weight:700;padding:2px 6px;border-radius:4px;' +
            (asking ? 'color:#1a1204;background:#ffd98a' : 'color:#dbe7f5;background:#2b3a4d');
        alarm.title = entry.stranded_alarm;
        btn.appendChild(alarm);
    }

    // Keep-staged keeper: paused (REQUEST resumes it) or unable to keep its
    // spot. Amber, because it asks someone to do something; the sentence is the
    // server's (keep_staged_note), shown on hover. Top-left, so it does not sit
    // on the bottom-left chips.
    if (entry.keep_staged_note) {
        const paused = entry.keep_staged_note.indexOf('paused') !== -1;
        const ks = el('span', {
            className: 'os-node-alarm',
            textContent: paused ? 'KEEP-STAGED PAUSED - TAP TO RESUME' : 'SPOT BLOCKED',
        });
        ks.style.cssText = 'position:absolute;top:4px;left:4px;font-size:11px;' +
            'font-weight:700;padding:2px 6px;border-radius:4px;color:#1a1204;background:#ffd98a';
        ks.title = entry.keep_staged_note;
        // A paused keeper resumes from its own chip: the tap is the chip's, not
        // the tile's, so it does not open the node.
        if (paused) {
            ks.style.cursor = 'pointer';
            ks.addEventListener('click', function (evt) {
                evt.stopPropagation();
                confirmResumeKeepStaged(entry.node.id, entry.node.name, loadViewRef);
            });
        }
        btn.appendChild(ks);
    }

    // The line moved onto this sequential position before its bin arrived:
    // the count waits and replays onto the bin when it binds. Informational;
    // the sentence is the server's (count_waits).
    if (entry.count_waits && !entry.stranded_alarm) {
        const waits = el('span', { className: 'os-node-alarm', textContent: 'COUNT WAITS FOR BIN' });
        waits.style.cssText = 'position:absolute;bottom:4px;left:4px;font-size:11px;' +
            'font-weight:700;padding:2px 6px;border-radius:4px;color:#dbe7f5;background:#2b3a4d';
        waits.title = entry.count_waits;
        btn.appendChild(waits);
    }

    // CONTAINED chip — the bin on this tile carries a payload whose quality
    // containment flag is ACTIVE: its next FG-bound delivery diverts to the
    // hold spot automatically. Red, and it does not ask anyone to do
    // anything — the state is the system working — but an operator watching
    // the tile sees the divert coming instead of being surprised by a robot
    // headed somewhere unexpected.
    if (entry.containment_flagged) {
        const chip = el('span', {
            className: 'os-node-alarm',
            textContent: 'CONTAINED',
        });
        chip.style.cssText = 'position:absolute;top:4px;right:4px;font-size:11px;' +
            'font-weight:700;color:#fff;background:#842029;padding:2px 6px;border-radius:4px';
        chip.title = 'This payload is quality-contained — its finished-goods deliveries divert to the hold spot until released.';
        btn.appendChild(chip);
    }

    // Banner label for the priority states. The full-tile background
    // already signals "something is up"; the label says what.
    if (releaseReady && !unloaderWindow) {
        btn.appendChild(el('span', { className: 'os-node-banner', textContent: 'RELEASE READY' }));
    } else if (inChangeover && !unloaderWindow) {
        btn.appendChild(el('span', { className: 'os-node-banner', textContent: 'CHANGEOVER' }));
    }

    // [REP] corner badge stays for replenishing (different signal — bin
    // move in flight). [CO] badge is suppressed because the full-tile
    // CHANGEOVER banner makes it redundant.
    const icon = statusIcon(entry);
    if (icon && icon === '[REP]') {
        btn.appendChild(el('span', { className: 'os-node-icon', textContent: icon }));
    }

    if (loader) {
        const binState = entry.bin_state;
        const binLabel = binState && binState.bin_label ? binState.bin_label : '';
        // "Awaiting stock" = any non-terminal order: the words go on saying it
        // while the order moves through its statuses (windowWords).
        const hasActiveOrder = (entry.orders || []).some(o => isActive(o.status));
        const awaiting = (ROLE_WORDS[loader.role] && ROLE_WORDS[loader.role].awaiting) || 'STOCK';
        const statusText = windowWords(entry, awaiting, hasActiveOrder);
        btn.appendChild(el('span', { className: 'os-node-remaining', textContent: statusText }));
        const partialHere = partialLine(binState, false, remaining);
        if (partialHere) btn.appendChild(el('span', { className: 'os-node-partial', textContent: partialHere }));
        // A home whose clear runs with its empty still on it says the job where
        // a partial's line goes (an empty has none); the board's card says the same.
        else if (loader.role === 'produce' && clearRunning(entry)) {
            btn.appendChild(el('span', { className: 'os-node-partial', textContent: CLEAR_RUNNING }));
        }
        if (binLabel) {
            const labelEl = el('span', { className: 'os-node-payload', textContent: binLabel });
            labelEl.style.cssText = 'font-size:14px;font-weight:600;color:#fff';
            btn.appendChild(labelEl);
        } else {
            btn.appendChild(el('span', { className: 'os-node-payload', textContent: 'Manual Swap' }));
        }
    } else {
        // NO BIN, not 0. A claimed node with no carrier bound has nothing to
        // count, and a bare 0 reads as a slot that has run out — the operator
        // acts on those. The switch used to paper over this window by seeding
        // the claim's capacity into the count, which put a policy number in a
        // field Core reads as a physical measurement (SwitchNode); saying what
        // is true is the replacement for inventing a number.
        const noCarrier = claim && runtime.active_bin_id == null;
        btn.appendChild(el('span', {
            className: 'os-node-remaining',
            textContent: claim ? (noCarrier ? 'NO BIN' : String(remaining)) : '--'
        }));
        if (claim && capacity > 0 && !noCarrier) {
            btn.appendChild(el('span', {
                className: 'os-node-capacity',
                textContent: '/ ' + capacity
            }));
        }
        const payloadText = claim ? (claim.payload_code || 'Unassigned') : '';
        appendETAPills(btn, inboundOrders, entry.bin_state);
        const partialAtLine = partialLine(entry.bin_state, true, remaining);
        if (partialAtLine) btn.appendChild(el('span', { className: 'os-node-partial', textContent: partialAtLine }));
        btn.appendChild(el('span', { className: 'os-node-payload', textContent: payloadText }));
    }

    if (unloaderWindow) {
        // Unloader window tap logic:
        //   full parked  → confirm-swap panel (with optional carrier picker)
        //   empty parked → push-empty confirmation panel
        //   no bin       → no-op
        btn.addEventListener('click', () => {
            const bs = entry.bin_state;
            const fullPresent = bs && bs.occupied && bs.payload_code;
            const emptyPresent = bs && bs.occupied && !bs.payload_code;
            if (bs && bs.held_since) { confirmUnloadHeld(entry.node.id, bs.held_for, bs.held_uop); return; }
            // The panel lights what the window should go back as (clearHighlight).
            const lit = loader.leaves_bare ? null : clearHighlight(entry, getView().serves);
            if (fullPresent) confirmUnloadSwap(entry.node.id, carriersOf(entry),
                entry.runtime && entry.runtime.remaining_uop_cached, !!loader.leaves_bare, lit);
            else if (emptyPresent) confirmPushEmpty(entry.node.id, carriersOf(entry), !!bs.bare, lit);
        });
    } else {
        btn.addEventListener('click', () => openModalRef(entry.node.id));
    }
    return btn;
}

// Renders one pill per inbound order. Three states:
//   - in_transit + no bin at this node → ETA countdown ("ETA: ~3 min")
//   - staged                           → "Arrived" (robot at the spot)
//   - in_transit + bin already at node → skip (the order has reverted
//     to in_transit on the back half of a two-robot swap — partner
//     robot leaving with the old bin; operator already had their
//     moment when the bin first landed)
//
// inboundOrders is the same list used by the color-priority decision in
// createNodeButton so the pill row and the background color can't
// disagree. binAtNode discriminates the approach half from the depart
// half of a swap; without it the pill would tick into "Running late"
// even though the robot has been at the spot for minutes.
function appendETAPills(btn, inboundOrders, binState) {
    if (!inboundOrders || inboundOrders.length === 0) return;
    const binAtNode = !!(binState && binState.occupied);
    const pills = [];
    inboundOrders.forEach(o => {
        if (o.status === 'staged') {
            pills.push({ text: 'Arrived', overdue: false });
        } else if (o.status === 'in_transit' && !binAtNode) {
            const d = formatETA(o.eta);
            if (!d.empty) pills.push(d);
        }
    });
    if (pills.length === 0) return;
    const row = el('div', { className: 'os-node-eta' });
    pills.forEach(p => {
        row.appendChild(el('span', {
            className: 'os-node-eta-pill' + (p.overdue ? ' overdue' : ''),
            textContent: p.text,
        }));
    });
    btn.appendChild(row);
}

// formatETA moved to operator-util.js — the modal's waiting label needs the
// same phrasing, and importing the tile renderer for one pure formatter was
// the wrong dependency direction.

// isReleaseReady drives the os-release-ready blue glow.
//
// Production: swap_ready (store.ComputeSwapReady), the pair released as one.
//
// Changeover: a leg parked at a station wait of a changeover purpose (ready,
// tooling done), from the server's release_purposes (SHAPE 3.8). The act at
// the click decides what goes and what holds, and a supply that cannot go yet
// is held with an intent that goes by itself, so the glow no longer waits on
// the supply. A node the changeover leaves unchanged keeps its production glow
// (L10): it used to take this branch, find no evac on its task, and go dark.
function isReleaseReady(entry) {
    if (entry.swap_ready) return true;
    const task = entry.changeover_task;
    if (!task || task.situation === 'unchanged') return false;
    return (entry.release_purposes || []).some(p => p.ready && p.purpose !== 'swap');
}

function nodeColorClass(entry) {
    const claim = entry.active_claim;
    const remaining = entry.runtime ? entry.runtime.remaining_uop_cached : 0;
    if (loaderOf(entry)) {
        const hasActiveOrder = entry.orders && entry.orders.some(o => isActive(o.status));
        if (hasActiveOrder) return 'os-mid';
        return remaining > 0 ? 'os-full' : 'os-empty';
    }
    if (!claim) return 'os-unclaimed';
    const capacity = claim.uop_capacity || 1;
    if (remaining <= 0) return 'os-empty';
    const pct = remaining / capacity;
    if (pct < 0.33) return 'os-low';
    if (pct < 0.66) return 'os-mid';
    return 'os-full';
}

function statusIcon(entry) {
    if (entry.changeover_task && entry.changeover_task.state !== 'switched' && entry.changeover_task.state !== 'verified') {
        return '[CO]';
    }
    if (isReplenishing(entry)) return '[REP]';
    return null;
}

// ─── Footer ───

export function renderFooter() {
    const view = getView();
    const co = view.active_changeover;

    if (co) {
        const nodes = claimedNodes();
        const coNodes = nodes.filter(n => n.changeover_task);
        const done = coNodes.filter(n => n.changeover_task.state === 'switched' || n.changeover_task.state === 'verified').length;
        footerStatus.textContent = co.from_style_name + ' \u2192 ' + co.to_style_name +
            ' [' + done + '/' + coNodes.length + ' nodes]';
    } else {
        footerStatus.textContent = 'Operator Station Ready';
    }

    // THE BADGE IS DERIVED, NOT ANNOUNCED: stationState reads it from fields
    // the poll already carries, the same reading the header makes.
    const state = stationState(view);
    const changing = state.changing;
    const current = state.current;
    footerBadge.textContent = state.words;
    footerBadge.className = 'os-footer-badge';
    if (changing) footerBadge.classList.add('changeover');
    if (!changing && current) footerBadge.classList.add('producing');
}

// Expose fillColor so the modal module can render the fill bar without
// re-importing it from operator-util.
export { fillColor };


// ─── The customer's side of a supply refusal ─────────────────────────────
//
// A modal, once, on arrival. It fires when the poll first returns a node with an
// unanswered refusal for a part that node has an outstanding call for.
//
// NO TIMERS, and that is the rule from e7e12622 rather than a preference: the
// modal's visibility is a pure function of server state (refused, and not yet
// answered), recomputed on the poll the board already runs. There is no
// setTimeout, no countdown and no local dismissal flag — reload the HMI mid
// question and the question is still there, which is correct, because it has not
// been answered.
//
// TWO BUTTONS AND NO THIRD. No close, no ✕, no click-outside. Both are real
// answers to a real question and one of them must be given. That is what makes a
// modal defensible after four rounds argued against one: the objection was never
// "don't interrupt", it was that a dismiss policy is an invented snooze interval.
// There is no interval when dismissing IS answering.
function maybeShowRefusalModal() {
    const view = getView();
    if (!view || !view.nodes) return;
    if (document.querySelector('.os-refusal-overlay')) return; // already asking

    let node = null;
    let refusal = null;
    for (const entry of view.nodes) {
        const r = entry.supply_refused_for_me;
        if (r && !r.answered) { node = entry; refusal = r; break; }
    }
    if (!refusal) return;

    const overlay = el('div', { className: 'os-co-picker-overlay os-refusal-overlay' });
    const panel = el('div', { className: 'os-co-picker' });

    // Attribution does the honest work. Not "there are no parts" — nothing in
    // this system can make that claim, because Shingo's coverage is a subset of
    // the greater Martinrea system. A PERSON told you, and they are named and
    // timed.
    panel.appendChild(el('div', {
        className: 'os-co-picker-title',
        textContent: refusal.loader_node + ' CANNOT SUPPLY ' + refusal.payload_code,
    }));
    panel.appendChild(el('div', {
        className: 'os-co-picker-verdict',
        textContent: (refusal.refused_by || 'Loader operator') +
            (refusal.refused_at ? ', ' + shortTime(refusal.refused_at) : '') +
            '. Your request is still queued.',
    }));

    // One answer: both buttons are disabled from the first tap.
    let answering = false;
    function answer(choice, then) {
        if (answering) return;
        answering = true;
        wait.disabled = true;
        co.disabled = true;
        return postAction('/api/process-nodes/' + node.node.id + '/supply-refusal/ack', {
            loader_node: refusal.loader_node,
            payload_code: refusal.payload_code,
            choice: choice,
        }, loadViewRef).then(function (ok) {
            // A refused or unanswered ack leaves the question up and live:
            // it has not been answered (operator-safe-taps.test.js).
            if (!ok) {
                answering = false;
                wait.disabled = false;
                co.disabled = false;
                return;
            }
            overlay.remove();
            if (then) then();
        });
    }

    const wait = el('button', { className: 'os-co-picker-btn', textContent: 'WAIT' });
    wait.addEventListener('click', function () { answer('wait'); });
    panel.appendChild(wait);

    // CHANGE OVER records the decision, then opens the pick-a-part modal — the
    // operator still has to say which part. The ack is the decision; the picker is the
    // destination. The cancel of the outstanding order is NOT special-cased here:
    // StartProcessChangeover cancels pre-dispatch orders as a general property.
    const co = el('button', { className: 'os-co-picker-btn danger', textContent: 'CHANGE OVER' });
    co.addEventListener('click', function () { answer('changeover', openComposerPicker); });
    panel.appendChild(co);

    overlay.appendChild(panel);
    document.body.appendChild(overlay);
}

// shortTime renders an ISO timestamp as wall-clock for the operator. Computed at
// render from server state, never held in a timer. Plant-timezone pinned —
// the operator station must agree with every other surface's clock.
function shortTime(iso) {
    try {
        return formatClock(iso);
    } catch (err) {
        return '';
    }
}

// stationSign is the station's stale sign for the top line (staleSign): the
// view on screen and whether its server is answering.
export function stationSign() {
    return staleSign(getView(), getLink(), shortTime);
}
