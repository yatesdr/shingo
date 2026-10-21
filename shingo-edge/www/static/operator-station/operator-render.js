import { el, esc, fillColor, postAction, showToast, fetchWithTimeout, formatETA } from './operator-util.js';
import { formatClock, serverNow } from '/static/shared/utils.js';
import { getView, claimedNodes, isReplenishing } from './operator-state.js';
import { isActive } from './order-status.js';
import { cardModel, headerModel, nodeFacts, ROLE_WORDS } from './operator-window-state.js';
import { loaderOf, tileRole, partCodes, carriersOf, isOperatorReplenished, showsAllParts } from './operator-board-model.js';
import { isDedicated, isUnloaderWindow, boardLayout, cardParts } from './operator-board-model.js';
import { partState, changeoverInstruction, windowWords } from './operator-board-model.js';
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

    const badge = el('span', {
        className: 'os-health-badge ' + (view.station.health_status === 'online' ? 'online' : 'offline')
    });
    headerActions.appendChild(badge);

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

    // A loader's board is operated by a forklift driver: its header carries
    // the loader's acts, not the changeover/cutover buttons, which belong on a
    // regular operator station (boardLayout's header).
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
        // Dedicated home loader (forklift-operated): the operator's header action
        // is consolidating homes, not a changeover. Surface the buffer chain here
        // in place of CHANGEOVER so it's discoverable instead of hidden behind a
        // per-card condition.
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
async function openComposerPicker() {
    const ui = await ensureComposer();
    if (ui) ui.openPicker(getView());
    else showToast('The changeover screen did not load — tap CHANGEOVER again', 'error');
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
    document.body.appendChild(overlay);
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
    document.body.appendChild(overlay);
}

function headerBtn(label, cls, onClick) {
    const btn = el('button', { className: 'os-header-btn ' + cls, textContent: label });
    btn.addEventListener('click', onClick);
    return btn;
}

// ─── Grid ───

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

    const cardGrid = grid.querySelector('.os-board-cards');
    const savedScrollTop = cardGrid ? cardGrid.scrollTop : 0;

    grid.innerHTML = '';

    if (nodes.length === 0) {
        grid.classList.remove('os-board-mode');
        document.body.classList.remove('os-board-mode-active');
        grid.style.removeProperty('--os-cols');
        grid.style.removeProperty('--os-rows');
        const empty = el('div', { id: 'os-grid-empty', textContent: emptyGridWords(getView()) });
        grid.appendChild(empty);
        return;
    }

    // The layout comes from the tiles (boardLayout): a loader's board draws a
    // card per home and part, or per window and part; a shared unloader's
    // windows, and a station that mixes loaders with presses, the node grid.
    const layout = boardLayout(nodes);
    if (layout.kind === 'homes' || layout.kind === 'payload') {
        grid.classList.add('os-board-mode');
        document.body.classList.add('os-board-mode-active');
        grid.style.removeProperty('--os-cols');
        grid.style.removeProperty('--os-rows');
        if (layout.kind === 'homes') {
            renderDedicatedPositionsBoard(layout.loaders);
        } else {
            renderPayloadBoard(layout.loaders);
        }
        const restored = grid.querySelector('.os-board-cards');
        if (restored) restored.scrollTop = savedScrollTop;
        return;
    }

    grid.classList.remove('os-board-mode');
    document.body.classList.remove('os-board-mode-active');
    const { cols, rows } = gridDimensions();
    grid.style.setProperty('--os-cols', cols);
    grid.style.setProperty('--os-rows', rows);

    for (const entry of nodes) {
        grid.appendChild(createNodeButton(entry));
    }
}

// The per-card state machine (status tag, detail, action, badge facts) and the
// header badge now live in operator-window-state.js — cardModel(entry, code) and
// headerModel(entry) — so the loader/unloader wording is one table and the header
// can't contradict the cards. operator-render is the render layer that consumes them.

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

// pulledDirectlyNote is the card's sentence at a window of a two-stage
// unloader's stage 2 pulled directly by the process: its finished cart is not
// sent anywhere, it waits on the window for the line. Empty for every other
// window, and while the window holds no cart or a cart still carrying parts.
function pulledDirectlyNote(entry) {
    const loader = loaderOf(entry);
    if (!loader || !loader.pulled_directly) return '';
    var b = entry.bin_state;
    if (!b || !b.occupied || b.payload_code) return '';
    return 'The cart waits here for the line';
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
// Exported for the node modal's CLEAR BIN on an unloader, which goes through
// this panel (operator-modal.js).
export function confirmUnloadSwap(nodeID, carriers, discarding, leavesBare) {
    if (leavesBare) {
        confirmFullOff(nodeID, discarding);
        return;
    }
    var binTypeCodes = carriers || [];

    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker' });
    panel.appendChild(el('div', { className: 'os-co-picker-title', textContent: 'Full pulled, empty filled?' }));
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
        textContent: 'Confirms the bin is unloaded. The empty returns to the supermarket and the next full is requested.' }));
    if (discarding > 0) {
        panel.appendChild(el('div', { className: 'os-co-picker-subtitle os-co-picker-warn',
            textContent: 'This slot still counts ' + discarding + '. Confirming writes it to zero.' }));
    }

    const binTypes = binTypeCodes.length > 0 ? binTypeCodes : null;
    // Single-type stations auto-fill: skip the picker and send the code
    // directly on CONFIRM SWAP so the dunnage is still recorded without
    // forcing the operator to tap a one-item menu.
    const autoCode = binTypes && binTypes.length === 1 ? binTypes[0] : null;
    if (binTypes && !autoCode) {
        // Two or more types: show the picker so the operator selects one.
        panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
            textContent: 'Select dunnage type for this carrier:' }));
        binTypes.forEach(function(code) {
            const btn = el('button', { className: 'os-co-picker-btn', textContent: code });
            btn.addEventListener('click', function() {
                overlay.remove();
                postAction('/api/process-nodes/' + nodeID + '/clear-bin',
                    { bin_type_code: code }, loadViewRef);
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
    document.body.appendChild(overlay);
}

// oneTapPanel is the two-stage unloader's panel: a title, what to do, one
// button that posts clear-bin with a blank code, the line saying what happens
// next, and CANCEL. No type appears on it anywhere — Core stamps the cart from
// its own type, so there is nothing for the operator to pick or read.
function oneTapPanel(nodeID, title, subtitles, label, after) {
    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker os-co-picker--one-tap' });
    panel.appendChild(el('div', { className: 'os-co-picker-title', textContent: title }));
    subtitles.forEach(function(s) { panel.appendChild(s); });
    const go = el('button', { className: 'os-co-picker-btn', textContent: label });
    go.addEventListener('click', function() {
        overlay.remove();
        postAction('/api/process-nodes/' + nodeID + '/clear-bin', undefined, loadViewRef);
    });
    panel.appendChild(go);
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle', textContent: after }));
    const cancel = el('button', { className: 'os-co-picker-btn cancel', textContent: 'CANCEL' });
    cancel.addEventListener('click', () => overlay.remove());
    panel.appendChild(cancel);
    overlay.appendChild(panel);
    overlay.addEventListener('click', evt => { if (evt.target === overlay) overlay.remove(); });
    document.body.appendChild(overlay);
}

// confirmFullOff is stage 1 of a two-stage unloader: the full comes off the
// cart and the cart goes on to stage 2 bare. The discard count is still stated.
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
// already answers, so it is one tap.
function confirmSendOn(nodeID) {
    oneTapPanel(nodeID, 'Cart, no bin',
        [el('div', { className: 'os-co-picker-subtitle', textContent: 'Put an empty bin on it, then send it on.' })],
        'Send on', 'Goes to the empties supermarket. No type to pick — it\'s the cart it came in as.');
}

// confirmPushEmpty shows a confirmation panel for an empty carrier sitting in an
// unloader window. Tapping PUSH EMPTY fires the push-empty endpoint so the carrier is
// sent to the supermarket and the node can request a fresh full bin.
//
// bare is the carrier's bin_state.bare: a stage-2 cart with no bin on it gets
// the one-tap Send on panel instead (confirmSendOn).
function confirmPushEmpty(nodeID, carriers, bare) {
    if (bare) {
        confirmSendOn(nodeID);
        return;
    }
    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker' });
    panel.appendChild(el('div', { className: 'os-co-picker-title', textContent: 'Empty bin in slot' }));
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
        textContent: 'Send the empty carrier to the supermarket so a full bin can be requested.' }));

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
    // records "discarded 0 parts"), writes the declared bin type, and fires the
    // same empty-out move push-empty would have. Re-clearing IS pushing empty,
    // plus the one fact this panel was missing.
    //
    // THE OPERATOR IS ASSERTING, NOT GUESSING. This is the same reading
    // RecoverTransitAnomaly takes — a person looking at the thing is believed —
    // and it is NOT a way to argue with the Allowed Bin Types fence. Declaring
    // a carrier's real type is a correction; retyping a carrier so a refused
    // order will go through would be a lie about a physical object, and this
    // panel only ever offers the types the node's payloads actually use.
    const dunnage = carriers || [];
    if (dunnage.length > 0) {
        panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
            textContent: 'Or push it out and correct the carrier type:' }));
        dunnage.forEach(function(code) {
            const btn = el('button', { className: 'os-co-picker-btn', textContent: 'PUSH AS ' + code });
            btn.addEventListener('click', function() {
                overlay.remove();
                postAction('/api/process-nodes/' + nodeID + '/clear-bin',
                    { bin_type_code: code }, loadViewRef);
            });
            panel.appendChild(btn);
        });
    }

    const cancel = el('button', { className: 'os-co-picker-btn cancel', textContent: 'CANCEL' });
    cancel.addEventListener('click', () => overlay.remove());
    panel.appendChild(cancel);

    overlay.appendChild(panel);
    overlay.addEventListener('click', evt => { if (evt.target === overlay) overlay.remove(); });
    document.body.appendChild(overlay);
}

// confirmClearLoaderHome shows a confirmation overlay before firing the
// clear-loader-home consolidation sequence. The operator declares this home bin
// empty; Shingo moves the carrier to the buffer, then pulls the buffer partial
// back to the home.
function confirmClearLoaderHome(nodeID) {
    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker' });
    panel.appendChild(el('div', { className: 'os-co-picker-title', textContent: 'Clear this home bin?' }));
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
        textContent: 'Declares this bin empty. Shingo will move the empty carrier to the buffer, then pull the buffer partial to this home.' }));

    const confirm = el('button', { className: 'os-co-picker-btn', textContent: 'CONFIRM CLEAR' });
    confirm.addEventListener('click', function() {
        overlay.remove();
        postAction('/api/process-nodes/' + nodeID + '/clear-loader-home', undefined, loadViewRef);
    });
    panel.appendChild(confirm);

    const cancel = el('button', { className: 'os-co-picker-btn cancel', textContent: 'CANCEL' });
    cancel.addEventListener('click', () => overlay.remove());
    panel.appendChild(cancel);

    overlay.appendChild(panel);
    overlay.addEventListener('click', evt => { if (evt.target === overlay) overlay.remove(); });
    document.body.appendChild(overlay);
}

// openPullFullFromHomePicker is the header entry point to the buffer chain. It
// lists ONLY the homes that can actually run it — a loaded home ("full") whose
// loader buffer holds a matching partial to backfill (the same gate as the
// per-card CLEAR BIN). Showing only eligible homes means every option is
// actionable; tapping one runs the clear-loader-home consolidation. The board
// itself pulses these same homes (os-board-card--ready) so the operator can also
// see at a glance which ones are ready.
function openPullFullFromHomePicker() {
    var eligible = claimedNodes().filter(function(n) {
        var bs = (n && n.bin_state) || {};
        return isDedicated(n) && bs.occupied && bs.payload_code && !!n.buffer_partial;
    });

    var overlay = el('div', { className: 'os-co-picker-overlay' });
    var panel = el('div', { className: 'os-co-picker' });
    panel.appendChild(el('div', { className: 'os-co-picker-title', textContent: 'Pull full from home' }));
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
        textContent: 'Pick a full home. Shingo sends its empty carrier to the buffer, then pulls the matching buffer partial into the home.' }));

    if (eligible.length === 0) {
        panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
            textContent: 'No home has a matching buffer partial ready right now.' }));
    }
    eligible.forEach(function(n) {
        var bs = n.bin_state || {};
        var btn = el('button', { className: 'os-co-picker-btn' });
        btn.style.cssText = 'text-align:left;padding:14px 18px;display:block;width:100%;';
        btn.appendChild(el('div', { textContent: esc(n.node.name),
            style: 'font-size:22px;font-weight:700;letter-spacing:0.03em;' }));
        btn.appendChild(el('div', { textContent: esc(bs.payload_code),
            style: 'font-size:17px;font-weight:600;margin-top:5px;' }));
        btn.addEventListener('click', function() {
            overlay.remove();
            confirmClearLoaderHome(n.node.id);
        });
        panel.appendChild(btn);
    });

    var cancel = el('button', { className: 'os-co-picker-btn cancel', textContent: 'CANCEL' });
    cancel.addEventListener('click', function() { overlay.remove(); });
    panel.appendChild(cancel);

    overlay.appendChild(panel);
    overlay.addEventListener('click', function(evt) { if (evt.target === overlay) overlay.remove(); });
    document.body.appendChild(overlay);
}

// showPullFromMarketPicker fetches bins currently sitting in the loader's
// outbound supermarket and presents a picker so the operator can pull one back
// to the loader window. Used during cell launches to recover bins from the
// market before resuming normal empty-request / load-full flow.
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
    document.body.appendChild(overlay);

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

                var uopEl = el('div', { textContent: 'UoP: ' + b.uop_remaining });
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

// waitedLabel formats how long a queued call has gone unanswered, coarsely — the
// operator needs "is this minutes or is this an hour", not seconds. Computed at
// render time from the order's created_at with NO timer: the board already
// re-renders on its own refresh cycle, and this surface has a history of
// self-amplifying refresh loops (the station-view refresh ratchet), so nothing
// here is allowed to schedule its own repaint. Returns '' on a missing or
// unparseable timestamp, and on a future one (clock skew between edge and Core),
// so a bad value degrades to the bare word "Waiting" instead of "Waiting -3m".
function waitedLabel(created) {
    if (!created) return '';
    var t = Date.parse(created);
    if (isNaN(t)) return '';
    var mins = Math.floor((serverNow() - t) / 60000);
    if (mins < 0) return '';
    if (mins < 1) return 'under a minute';
    if (mins < 60) return mins + 'm';
    var hrs = Math.floor(mins / 60);
    return hrs + 'h' + (mins % 60) + 'm';
}

// idleCardHidden reports whether a card is left off the board: the station
// hides what has nothing to do (board_hides_idle) and this card is idle. On an
// operator-ordered station an EMPTY spot is not idle in that sense. It is where
// the operator orders the next empty (the card opens the modal with REQUEST
// EMPTY), so it stays. A loaded spot with nothing ordered has nothing to do.
function idleCardHidden(entry, cs) {
    var loader = loaderOf(entry);
    if (!loader || !loader.board_hides_idle || cs.cls !== 'os-board-nodemand') return false;
    var occupied = !!(entry.bin_state && entry.bin_state.occupied);
    if (isOperatorReplenished(entry) && !occupied) return false;
    return true;
}

// buildLoaderCard renders ONE (position × payload) card — the atomic unit of the
// loader board. Returns the card element, or null when the station hides idle
// cards and this one is (idleCardHidden). counters.queuePos tracks the per-payload queue badge
// across cards. The card's STATE (status/detail/action/badge facts) comes from
// cardModel (operator-window-state.js) and what the part is at the served cells
// from partState (operator-board-model.js); this function is presentation only —
// DOM, the coverage badge of a loader that lists every part, and idle-card hiding.
function buildLoaderCard(entry, code, counters, opts) {
    var allParts = showsAllParts(entry);
    var ps = partState(entry, code, getView());
    var card = el('div', { className: 'os-board-card' });
    var cs = cardModel(entry, code);

    // Coverage (ACTIVE = a running style of a served cell uses it now; PRELOAD =
    // only an inactive style does) — drives the badge + the operator board's idle
    // override. NOT gated on the loader being one a person stages: every consumer
    // below carries its own gate, and the downtime escalation needs the same fact
    // on a normal board, where the ACTIVE/PRELOAD badge is deliberately not drawn.
    var isActiveStylePayload = ps.active;

    // Downtime escalation is scoped to running parts — a queued call on a part no
    // running style uses is not costing the line anything. When the board could
    // not look (no view.serves) there is nothing to scope BY, and staying quiet
    // would hide real downtime behind missing data, so it escalates (ps.inScope).
    var downtimeInScope = ps.inScope;

    // A board listing every part (loader.board_shows_all_parts): on an idle card,
    // "NO DEMAND" says less than whether a running style needs the part. Show
    // that instead.
    if (allParts && cs.cls === 'os-board-nodemand') {
        if (isActiveStylePayload) {
            cs.statusText = 'ACTIVE'; cs.statusClass = 'os-board-tag-lineside'; cs.detail = '';
        } else {
            cs.statusText = 'PRELOAD'; cs.statusClass = 'os-board-tag-preload'; cs.detail = 'Available to stage';
        }
    }

    // Hide what has nothing to do, when the station is set to: a wall of grey
    // idle cards buries the handful the operator can act on. It is a setting
    // of its own (Core: bin_loaders.board_hides_idle). It used to be read off
    // the loader's replenishment, so an operator-ordered station could never
    // hide a loaded home. The home board counts what this hides in its subtitle.
    if (idleCardHidden(entry, cs)) return null;

    card.classList.add(cs.cls);
    if (cs.loadNow) card.classList.add('os-board-load-now');

    // ACTIVE badge on EVERY board. It used to be operator-loader-only, reasoning that a
    // normal board renders only active-style payloads so the label adds nothing — but
    // redundant to the code is not redundant to a driver reading a wall of part numbers
    // from a forklift position, who otherwise has to KNOW that rule to trust the board. Say
    // it on the card. PRELOAD only where every part is listed: a board of the running
    // style's parts has no preload cards to contrast against, so an unpaired PRELOAD
    // badge would be noise.
    if (isActiveStylePayload || allParts) {
        card.appendChild(el('span', {
            className: 'os-board-cov ' + (isActiveStylePayload ? 'os-board-cov-active' : 'os-board-cov-preload'),
            textContent: isActiveStylePayload ? 'ACTIVE' : 'PRELOAD',
        }));
    }
    // The coverage TINT goes with listing every part. It exists to make that board's
    // IDLE cards read as actionable (.os-board-nodemand.os-board-cov-on-*); on a board
    // of the running style's parts it would repaint NO DEMAND cards blue for no reason.
    if (allParts) {
        card.classList.add(isActiveStylePayload ? 'os-board-cov-on-active' : 'os-board-cov-on-preload');
    }

    card.appendChild(el('div', { className: 'os-board-code', textContent: code }));
    card.appendChild(el('span', { className: 'os-board-tag ' + cs.statusClass, textContent: cs.statusText }));
    card.appendChild(el('div', { className: 'os-board-detail', textContent: cs.detail }));

    // NOT GATED ON the loader being an operator's: a dedicated-home board once
    // threw the answer away and an active part running short drew whatever the
    // status precedence gave it, green when a bin happened to be parked at the
    // home. Reported from Springfield: the cell was starving and the loader card
    // read healthy.
    //
    // The count is the served cells' (partState.lineside, from view.serves); the
    // mark is the part's coverage: exposed or out at a served cell with no
    // stocked bin of it on this board. Counts are what the board goes on: Core's
    // reorder threshold sits at or above a bin, so every running part read low.
    if (isActiveStylePayload) {
        var lsUOP = ps.lineside;
        var starved = ps.short;
        card.appendChild(el('div', {
            className: 'os-board-lineside' + (starved ? ' os-board-lineside--starved' : ''),
            textContent: 'Lineside ' + lsUOP + ' UoP' + (starved ? ' — PRELOAD' : ''),
        }));
        if (starved) card.classList.add('os-board-card--starved');
    }

    // QUEUED → RED. A queued call under an active style with nothing coming for it is
    // the line going down, and until now it looked identical to a LOAD cue: both
    // rendered os-board-queued blue, and the REASON Core had already computed was only
    // visible by tapping into the modal. Red the card and say it underneath.
    // A standing refusal for THIS card — (this node, this payload). The key is
    // the card, so both layouts reach it identically: a shared window's payload
    // card and a dedicated home's position card look it up the same way.
    var refusal = (entry.supply_refusals || {})[code] || null;

    if (cs.waitingOnRobot && downtimeInScope) {
        card.classList.add('os-board-card--waiting');
        var waited = waitedLabel(cs.waitingSince);
        card.appendChild(el('div', {
            className: 'os-board-downtime',
            textContent: 'QUEUED' + (waited ? ' ' + waited : '') +
                (cs.queueReason ? ' — ' + cs.queueReason : ''),
        }));
    }

    // THE CARD SHOWS THE STATE AND CARRIES NO BUTTON.
    //
    // Both controls — reporting and withdrawing — live in the modals the card
    // opens, under the actions already there. Owner direction, and the board
    // makes the case: a control on every card put a permanent block on every
    // healthy tile, and the resting label had to assert "NO PARTS AVAILABLE" on
    // cards where that was not true. The board says what IS; the modal is where
    // you say something.
    //
    // What stays here is the answer, once given: red, attributed, and timed.
    if (refusal) {
        // --waiting supplies the hue and --refused only kills the animation, so
        // the two cannot drift apart into a second red. A refused card reads red
        // whether or not it is also queued.
        card.classList.add('os-board-card--waiting');
        card.classList.add('os-board-card--refused');
        // Attribution, not a verdict. A PERSON said this — the sentence a
        // count could never make — so it is signed and timed. Station-level,
        // because the loader board carries no operator identity; saying "the
        // loader" rather than inventing a name is the honest granularity.
        card.appendChild(el('div', {
            className: 'os-board-downtime',
            textContent: 'NO PARTS AVAILABLE' +
                (refusal.refused_by ? ' — ' + refusal.refused_by : '') +
                (refusal.answered ? ' · cell chose to ' + (refusal.ack_choice || 'wait') : ' · awaiting the cell'),
        }));
    }

    // A pulled-directly stage 2's finished cart stays on its window for the
    // line to take; the card says so, so an empty cart standing there reads
    // as done rather than stuck.
    var waits = pulledDirectlyNote(entry);
    if (waits) {
        card.appendChild(el('div', { className: 'os-board-downtime', textContent: waits }));
    }

    // Corner badge, only for REAL per-payload orders (the agnostic blank-payload
    // empty must not stamp every card with a meaningless marker).
    //
    // Its TEXT depends on the board. A window board shares one budget across its
    // payloads, so the queue ordinal answers "where am I in line". A home board
    // does not: every home is an independent one-bin slot, so the ordinal is noise
    // — sourceBadge swaps it for the supermarket/buffer node the carrier is being
    // pulled from, which is what the operator actually wants to know. Falls back to
    // the ordinal when the order carries no source (pre-plan, unresolved group).
    if (cs.queueCount > 0) {
        var useSource = opts && opts.sourceBadge && cs.sourceNode;
        var badge = el('span', {
            className: 'os-board-pos' + (useSource ? ' os-board-pos--node' : ''),
            textContent: useSource ? cs.sourceNode : String(counters.queuePos),
        });
        if (cs.delivered) badge.classList.add('os-board-pos-delivered');
        else if (cs.inTransit) badge.classList.add('os-board-pos-transit');
        else badge.classList.add('os-board-pos-queued');
        card.appendChild(badge);
        counters.queuePos++;
    }

    if (cs.action === 'load') {
        card.style.cursor = 'pointer';
        card.addEventListener('click', function() {
            // A loader tile carries no capacity: the load modal takes the part's.
            openLoadBinRef(entry.node.id, [code], 0);
        });
    } else if (cs.action === 'unload') {
        card.style.cursor = 'pointer';
        card.addEventListener('click', function() {
            var held = entry.bin_state && entry.bin_state.held_since;
            if (held) { confirmUnloadHeld(entry.node.id, entry.bin_state.held_for, entry.bin_state.held_uop); return; }
            confirmUnloadSwap(entry.node.id, carriersOf(entry),
                entry.runtime && entry.runtime.remaining_uop_cached, !!loaderOf(entry).leaves_bare);
        });
    } else {
        card.style.cursor = 'pointer';
        card.addEventListener('click', function() {
            openModalRef(entry.node.id);
        });
    }

    return card;
}

// buildChangeoverLoadDirective renders the "load this" banner.
//
// THE BIN TYPE IS THE INSTRUCTION and gets the size to match — it is what the
// operator physically walks off to fetch. The payloads and the waiting cells
// are context underneath: they answer "why" and "for whom", which is what
// stops the directive reading as an arbitrary order from the system.
function buildChangeoverLoadDirective(d) {
    var wrap = el('div', { className: 'os-changeover-load-directive' });
    var types = (d.bin_type_codes || []).map(esc).join(' + ');
    var payloads = (d.payload_codes || []).map(esc).join(', ');
    var nodes = (d.for_nodes || []).map(esc).join(', ');
    wrap.innerHTML =
        '<div class="os-cld-label">CHANGEOVER — LOAD</div>' +
        '<div class="os-cld-bintype">' + types + '</div>' +
        (payloads ? '<div class="os-cld-why">for ' + payloads + '</div>' : '') +
        (nodes ? '<div class="os-cld-who">waiting: ' + nodes + '</div>' : '');
    return wrap;
}

// windowLabel is the line a card carries naming the window or home it stands
// for, and what stands there.
function windowLabel(entry) {
    var bs = entry.bin_state || {};
    var binTxt = bs.occupied ? (bs.payload_code ? 'LOADED' : 'EMPTY') : 'AWAITING';
    return el('div', { className: 'os-board-home', textContent: esc(entry.node.name) + ' · ' + binTxt });
}

// renderPayloadBoard draws a shared loader's windows: a card per window and
// part. With one window it is the board it always was. With several, every
// window's cards are drawn, each carrying its window's line (a bug fixed: only
// the first window was drawn, so a delivered empty at the second never showed
// LOAD).
function renderPayloadBoard(entries) {
    const view = getView();
    const entry = entries[0];
    const runtime = entry.runtime || {};
    const remaining = runtime.remaining_uop_cached != null ? runtime.remaining_uop_cached : 0;
    const binState = entry.bin_state;
    const binLabel = binState && binState.bin_label ? binState.bin_label : 'No bin';
    const binPayload = binState && binState.payload_code ? binState.payload_code : '';
    const role = tileRole(entry);
    const roleLabel = role === 'produce' ? 'Loader' : 'Unloader';
    const several = entries.length > 1;

    // Header status badge from the shared model — same facts the cards read, so the
    // badge can't contradict a card. Worded by role (loader awaits a BIN, unloader a FULL).
    // One window's bin is the board's; several windows each say theirs on their cards.
    var hb = headerModel(entry);

    var infoBar = el('div', { className: 'os-board-header' });
    infoBar.innerHTML =
        '<div>' +
            '<div style="font-size:42px;font-weight:700;color:#fff">' +
                entries.map(function(e) { return esc(e.node.name); }).join(' · ') + ' - ' + roleLabel + '</div>' +
            '<div style="font-size:20px;color:#aab;margin-top:6px">' +
                (several ? entries.length + ' windows | ' : '') +
                partCodes(entry).length + ' payloads configured</div>' +
        '</div>' +
        (several ? '' :
        '<div style="text-align:right">' +
            '<div style="font-size:28px;font-weight:600;color:#fff">Bin: ' + esc(binLabel) + '</div>' +
            (binPayload ? '<div style="font-size:20px;color:#aab;margin-top:4px">' + esc(binPayload) + ' | UoP: ' + remaining + '</div>' : '') +
            '<div style="display:inline-block;font-size:22px;font-weight:700;padding:8px 20px;border-radius:6px;margin-top:8px;' +
                hb.color + '">' + hb.text +
            '</div>' +
        '</div>');
    grid.appendChild(infoBar);

    // CHANGEOVER LOADING INSTRUCTION. While a served cell waits on what this
    // loader makes, the loader's ordinary question — "which of my payloads would
    // you like to load" — is the wrong one: the cell is waiting for one specific
    // carrier, and any other choice puts a bin nobody asked for on the window
    // and blocks the one that is needed. Drawn from the waiting rows of
    // view.serves when the station's directive setting is on
    // (changeoverInstruction).
    var directive = changeoverInstruction(entry, view);
    if (directive) {
        grid.appendChild(buildChangeoverLoadDirective(directive));
    }

    // Manual request / jumpstart button — ALL loaders (rendered just below,
    // above the cards). One button, not per-card: the operator is asking for a
    // bin to be brought to the loader, not picking a payload. Use cases:
    //   - an operator loader staging an empty proactively;
    //   - a threshold loader whose queue was cancelled in Core and now sits idle
    //     with no demand — this is the operator's jumpstart.
    //
    // Anti-spam: each window holds ONE bin, so the button is offered while some
    // window of the loader's role on this board has no bin parked and nothing
    // inbound (the gate the server applies across the loader's windows reads
    // the same set: a bug fixed, the button read only the first window). The
    // instant a request fires, the window's demand flips true on the next SSE
    // refresh and the button greys out when no window is left, so repeated taps
    // can't stack the queue. The server enforces the same rule (RequestEmptyBin
    // rejects when no empty can be staged) as defense-in-depth against
    // double-tap races and direct callers. The request posts for the first
    // window: the server stages at whichever window is free.
    var reqWindows = entries.filter(function(e) { return tileRole(e) === role; });
    var holdsBin = function(e) { return !!(e.bin_state && e.bin_state.occupied); };
    var canRequest = reqWindows.some(function(e) { return !holdsBin(e) && !nodeFacts(e).hasDemand; });

    // Payload for the request splits by role:
    //   - produce (loader): an empty bin is a generic carrier, so the request is
    //     payload-AGNOSTIC — post NO payload_code. Core sources any compatible
    //     empty and LoadBin binds the real payload when the operator fills it.
    //     (Replaces the old allowed[0] default, which fabricated a payload nobody
    //     asked for and could tag/stage the wrong card — see RequestEmptyBin.
    //     Assumes a single-carrier loader; a multi-carrier loader would need a
    //     carrier picker here.)
    //   - consume (unloader): REQUEST FULL pulls a SPECIFIC payload's full bin,
    //     so it needs a code: the first part the board lists for the window.
    var allowed = cardParts(entry, view.serves);
    var isProduce = role === 'produce';
    var requestPayload = isProduce ? '' : (allowed.length > 0 ? allowed[0] : '');

    // Render the bar when the action is expressible: always for a loader (the
    // empty needs no payload), and for an unloader only when there's a payload
    // to ask for.
    if (isProduce || requestPayload) {
        var reqBar = el('div', { className: 'os-board-reqbar' });
        var reqLabel = isProduce ? 'REQUEST EMPTY' : 'REQUEST FULL';
        var reqReason = reqWindows.every(holdsBin) ? 'bin at node' : 'bin already inbound';
        var reqBtn = el('button', {
            className: 'os-board-request-btn' + (canRequest ? '' : ' disabled'),
            textContent: canRequest ? reqLabel : reqLabel + ' — ' + reqReason,
        });
        if (canRequest) {
            reqBtn.addEventListener('click', function() {
                var url = isProduce
                    ? '/api/process-nodes/' + entry.node.id + '/request-empty'
                    : '/api/process-nodes/' + entry.node.id + '/request-full';
                var body = isProduce ? {} : { payload_code: requestPayload };
                postAction(url, body, loadViewRef);
            });
        } else {
            reqBtn.disabled = true;
        }
        reqBar.appendChild(reqBtn);

        // PULL FROM MARKET — produce loaders with an outbound destination.
        // Pulls a specific bin from the supermarket back to this window; Edge
        // auto-clears (zeroes UoP) when the bin is delivered so the operator
        // doesn't need a separate Clear Bin step. The door refuses a loader with
        // none (engine/operator_window_pullback.go), so the button is not drawn.
        if (isProduce && loaderOf(entry).outbound_dest) {
            var pullBtn = el('button', {
                className: 'os-board-request-btn',
                textContent: 'PULL FROM MARKET',
            });
            pullBtn.addEventListener('click', function() {
                showPullFromMarketPicker(entry.node.id);
            });
            reqBar.appendChild(pullBtn);
        }

        grid.appendChild(reqBar);
    }

    var cardGrid = el('div', { className: 'os-board-cards' });

    // One card per window and part, via the shared Card builder (also used by
    // the dedicated-positions board). buildLoaderCard returns null for an idle
    // card the station hides.
    var counters = { queuePos: 1 };
    var rendered = 0;
    var configured = 0;
    entries.forEach(function(w) {
        var parts = w === entry ? allowed : cardParts(w, view.serves);
        configured += parts.length;
        parts.forEach(function(code) {
            var card = buildLoaderCard(w, code, counters);
            if (!card) return;
            if (several) card.insertBefore(windowLabel(w), card.firstChild);
            cardGrid.appendChild(card);
            rendered++;
        });
    });

    // Column count tracks the cards actually shown (a demand-driven loader can
    // render fewer than its parts), so the grid doesn't reserve empty columns.
    var cols = rendered <= 3 ? Math.max(rendered, 1) : (rendered <= 6 ? 3 : 4);
    cardGrid.style.setProperty('--os-board-cols', cols);

    if (rendered === 0) {
        cardGrid.appendChild(el('div', {
            style: 'color:#666;font-style:italic;padding:24px;text-align:center;grid-column:1/-1',
            // no parts → nothing configured; parts but nothing rendered → a
            // threshold loader with no active demand (all idle cards filtered).
            // Distinct copy so the operator knows which it is.
            textContent: configured === 0 ? 'No payloads configured' : 'No active demand'
        }));
    }

    grid.appendChild(cardGrid);
}

// renderDedicatedPositionsBoard renders a dedicated-positions loader: each station
// position (node) is a home for one part, so we show one card per (home × its
// part) across all the station's loader nodes — N homes on one screen, the
// whole loader at a glance. Each card is backed by its own node's bin state +
// actions and carries a home label. Idle homes stay on screen unless the
// station hides them (idleCardHidden), and then the subtitle counts them.
function renderDedicatedPositionsBoard(nodes) {
    // Dedicated positions are a role-neutral layout: produce loaders OR consume
    // unloaders (dedicated finished-goods exits). Title reflects whichever this
    // station is.
    var serves = getView().serves;
    var roles = {};
    nodes.forEach(function(n) { roles[tileRole(n)] = true; });
    var title = (roles.produce && roles.consume) ? 'Stations'
        : roles.consume ? 'Unloader' : 'Bin Loader';

    // Cards are built BEFORE the header so the subtitle can report how many homes
    // were filtered out. Hidden cards must stay counted and visible as a number:
    // a board that silently drops two thirds of its tiles reads as "this is
    // everything" when it is not.
    var cardGrid = el('div', { className: 'os-board-cards' });
    var counters = { queuePos: 1 };
    var rendered = 0;
    var idle = 0;
    nodes.forEach(function(node) {
        cardParts(node, serves).forEach(function(code) {
            var card = buildLoaderCard(node, code, counters, { sourceBadge: true });
            if (!card) { idle++; return; }
            // Home label (physical position) + its own bin state, prepended so a
            // wall of cards stays scannable by home.
            var bs = node.bin_state || {};
            card.insertBefore(windowLabel(node), card.firstChild);
            // "Clear Bin" — only for home tiles with a buffer partial of the same
            // payload. Shows the consolidation confirmation overlay on tap. The card
            // also pulses (os-board-card--ready) so the operator can spot at a glance
            // which loaded homes are ready to consolidate — the same set the header
            // "PULL FULL FROM HOME" picker offers.
            if (node.buffer_partial && bs.occupied && bs.payload_code && code === bs.payload_code) {
                card.classList.add('os-board-card--ready');
                var clearBtn = el('button', {
                    className: 'os-co-picker-btn',
                    textContent: 'CLEAR BIN',
                });
                clearBtn.style.cssText = 'margin-top:12px;width:100%;font-size:18px;';
                clearBtn.addEventListener('click', function(evt) {
                    evt.stopPropagation();
                    confirmClearLoaderHome(node.node.id);
                });
                card.appendChild(clearBtn);
            }
            cardGrid.appendChild(card);
            rendered++;
        });
    });

    var cols = rendered <= 3 ? Math.max(rendered, 1) : (rendered <= 6 ? 3 : (rendered <= 12 ? 4 : 5));
    cardGrid.style.setProperty('--os-board-cols', cols);
    if (rendered === 0) {
        cardGrid.appendChild(el('div', {
            style: 'color:#666;font-style:italic;padding:24px;text-align:center;grid-column:1/-1',
            // Distinguish "nothing set up" from "set up, nothing to do right now" —
            // on a home board the second is the normal resting state and must not
            // look like a broken configuration.
            textContent: idle > 0 ? 'All homes stocked — nothing to load' : 'No homes configured',
        }));
    }

    var header = el('div', { className: 'os-board-header' });
    header.innerHTML =
        '<div><div style="font-size:42px;font-weight:700;color:#fff">' + title + ' — Home Locations</div>' +
        '<div style="font-size:20px;color:#aab;margin-top:6px">' +
        nodes.length + ' dedicated position' + (nodes.length === 1 ? '' : 's') +
        (idle > 0 ? ' · ' + idle + ' idle hidden' : '') +
        '</div></div>';
    grid.appendChild(header);
    grid.appendChild(cardGrid);
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
            if (fullPresent) confirmUnloadSwap(entry.node.id, carriersOf(entry),
                entry.runtime && entry.runtime.remaining_uop_cached, !!loader.leaves_bare);
            else if (emptyPresent) confirmPushEmpty(entry.node.id, carriersOf(entry), !!bs.bare);
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

    function answer(choice, then) {
        postAction('/api/process-nodes/' + node.node.id + '/supply-refusal/ack', {
            loader_node: refusal.loader_node,
            payload_code: refusal.payload_code,
            choice: choice,
        }, loadViewRef).then(function (ok) {
            overlay.remove();
            if (ok && then) then();
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
