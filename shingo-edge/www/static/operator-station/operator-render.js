import { el, fillColor, postAction, showToast, fetchWithTimeout, formatETA } from './operator-util.js';
import { formatClock } from '/static/shared/clock.js';
import { typedNote } from '/static/shared/count-words.js';
import { getView, claimedNodes, isReplenishing, getLink } from './operator-state.js';
import { isActive } from './order-status.js';
import { ROLE_WORDS, clearRunning, CLEAR_RUNNING } from './operator-window-state.js';
import { loaderOf, tileRole, takesBare, stageTwoOn, carriersOf, isDedicated, isUnloaderWindow, boardLayout, isBoard, staleSign } from './operator-board-model.js';
import { clearChoices, choiceLabel, heightWords, isLit, clearHighlight } from './operator-board-model.js';
import { windowWords, tileCount, ratioFor } from './operator-board-model.js';
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
        // (On the board its cards say it, in Bring partials home.)
        headerActions.appendChild(headerBtn('FULL BIN OFF HOME', 'pull-full-home', openPullFullFromHomePicker));
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

    // The line's auto-reorder switch, where Processes shows it on the station
    // (view.auto_reorder_switch), named as the line's: a tile's chip is its
    // own position's (auto_reorder_chip, "REORDER AT 110: ..."). The server
    // refuses turning it on without a readable counter and says why.
    if (view.auto_reorder_switch) {
        const on = !!view.auto_reorder_switch.on;
        headerActions.appendChild(headerBtn(on ? 'LINE AUTO-REORDER: ON' : 'LINE AUTO-REORDER: OFF',
            'auto-reorder' + (on ? '' : ' off'), () => confirmAutoReorderFlip(!on)));
    }

    headerActions.appendChild(headerBtn('REFRESH', 'refresh', loadViewRef));
}

// confirmAutoReorderFlip asks before switching the line's auto-reorder: off
// stops every automatic ask at the line until someone turns it back on here or
// in Processes; on lets them start again.
function confirmAutoReorderFlip(turnOn) {
    const view = getView();
    const sid = view.station.id;
    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker' });
    panel.appendChild(el('div', { className: 'os-co-picker-title',
        textContent: (turnOn ? 'Turn line auto-reorder on for ' : 'Turn line auto-reorder off for ') + view.process.name + '?' }));
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
        textContent: turnOn ? 'Positions ask for material on their own again.'
            : 'No position asks for material on its own until someone turns it back on. REQUEST still asks.' }));
    const confirm = el('button', { className: 'os-co-picker-btn' + (turnOn ? '' : ' danger'),
        textContent: turnOn ? 'TURN ON' : 'TURN OFF' });
    confirm.addEventListener('click', async () => {
        overlay.remove();
        const ok = await postAction('/api/operator-stations/' + sid + '/auto-reorder', { on: turnOn }, loadViewRef);
        if (ok) showToast('Line auto-reorder ' + (turnOn ? 'on' : 'off'), 'success');
    });
    panel.appendChild(confirm);
    const cancel = el('button', { className: 'os-co-picker-btn cancel', textContent: 'CANCEL' });
    cancel.addEventListener('click', () => overlay.remove());
    panel.appendChild(cancel);
    overlay.appendChild(panel);
    overlay.addEventListener('click', evt => { if (evt.target === overlay) overlay.remove(); });
    showOverlay(overlay);
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
export function openComposerPicker(targetStyleID) {
    return ensureComposer().then(function (ui) {
        if (ui) ui.openPicker(getView(), targetStyleID || 0);
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
        case 'unload': confirmUnloadSwap(a.node, a.carriers, a.discarding, a.leavesBare, a.lit, a.cart); break;
        case 'unload-held': confirmUnloadHeld(a.node, a.heldFor, a.count, a.stageTwo, a.pack, a.part); break;
        case 'push-empty': confirmPushEmpty(a.node, a.carriers, a.bare, a.lit, a.stageTwo, a.cart); break;
        case 'clear-home': confirmClearLoaderHome(a.node, a.home, a.spot, a.partial); break;
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
    grid.classList.remove('os-pc-board', 'os-pc-step-0', 'os-pc-step-1', 'os-pc-step-2', 'os-pc-step-3', 'os-pc-showall', 'os-pc-cellsopen', 'os-pc-zoneopen');
    grid.style.removeProperty('--os-pc-scale');
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

// confirmUnloadHeld is a held partial's tap: no plain CLEAR, because the count
// is what makes it worth finishing. Its one act posts clear-bin {unload:true},
// which Core books as any clear.
//
// stageTwo is the window's takesBare: a stage 2 of a two-stage unloader. There
// the panel has ONE primary button, PARTIAL OFF · EMPTY ON, named for both
// motions the person makes before the tap; the cart is then a dressed empty
// the line may fetch. COUNT is a small secondary beside it, the one correction
// for a lost count. At any other unloader window: COUNT and UNLOAD.
function confirmUnloadHeld(nodeID, line, count, stageTwo, pack, part) {
    var overlay = el('div', { className: 'os-co-picker-overlay' });
    var panel = el('div', { className: 'os-co-picker os-co-picker--one-tap' });
    panel.appendChild(el('div', { className: 'os-co-picker-title', textContent: 'Partial, held for ' + (line || 'its line') }));
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
        textContent: stageTwo ? 'Pull the partial bin off and set an empty on, then tap.'
            : 'Take the partial bin off, then UNLOAD. COUNT if it is full.' }));
    // COUNT declares what the carrier holds now (a person counted it, or topped
    // it up): the number goes to Core, which records it as this carrier's count.
    var countBtn = el('button', { className: 'os-co-picker-btn' + (stageTwo ? ' os-co-picker-btn--secondary' : ''),
        textContent: 'COUNT' });
    countBtn.addEventListener('click', function() {
        overlay.remove();
        const ratio = ratioFor((getView() || {}).load_templates, null, part);
        openKeypad(nodeID, count || 0, {
            // OWNER-WORDS: above a ratio of 1 the box takes parts.
            title: ratio > 1 ? 'Count on this carrier (parts)' : 'Count on this carrier',
            note: function(qty) { return typedNote(qty, ratio, pack); },
            onOk: function(_n, qty) {
                return postAction('/api/process-nodes/' + nodeID + '/record-count', { actual_uop: qty }, loadViewRef);
            },
        });
    });
    var go = el('button', { className: 'os-co-picker-btn', textContent: stageTwo ? 'PARTIAL OFF · EMPTY ON' : 'UNLOAD' });
    go.addEventListener('click', function() {
        overlay.remove();
        postAction('/api/process-nodes/' + nodeID + '/clear-bin', { unload: true }, loadViewRef);
    });
    if (stageTwo) {
        panel.appendChild(go);
        panel.appendChild(countBtn);
    } else {
        panel.appendChild(countBtn);
        panel.appendChild(go);
        panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
            textContent: 'Unload clears it and books what it held.' }));
    }
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
// no bin on it, so there is nothing to pick — the CLEAR is one tap, BIN OFF,
// and posts a blank code; Core stamps the cart from its own type. The panel
// names no type: the marker Core stamps is bookkeeping, never an operator's word.
//
// lit is the choice the board's card says to send back, {bin_type, layers?}
// (clearHighlight): that button is lit, every button still tappable.
//
// cartType is the type of the cart at the window: the panel offers the types of
// its family, and lit (clearChoices).
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
export function confirmUnloadSwap(nodeID, carriers, discarding, leavesBare, lit, cartType) {
    if (leavesBare) {
        confirmFullOff(nodeID, discarding);
        return;
    }
    const choices = clearChoices(getView(), carriers, cartType, lit);

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
//
// extras, when given, are more acts after them, each {label, act}: a stage 2's
// FULL ON and PARTIAL ON (confirmStageTwoOn); each closes the panel and runs act.
function oneTapPanel(nodeID, title, subtitles, label, after, heights, lit, extras) {
    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker os-co-picker--one-tap' });
    panel.appendChild(el('div', { className: 'os-co-picker-title', textContent: title }));
    subtitles.forEach(function(s) { panel.appendChild(s); });
    // label '' is a panel with no CLEAR of its own: only the extras (a consume
    // side's stage 2, where nothing goes on as an empty).
    if (label) {
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
    }
    (extras || []).forEach(function(extra) {
        const more = el('button', { className: 'os-co-picker-btn', textContent: extra.label });
        more.addEventListener('click', function() {
            overlay.remove();
            extra.act();
        });
        panel.appendChild(more);
    });
    if (after) panel.appendChild(el('div', { className: 'os-co-picker-subtitle', textContent: after }));
    const cancel = el('button', { className: 'os-co-picker-btn cancel', textContent: 'CANCEL' });
    cancel.addEventListener('click', () => overlay.remove());
    panel.appendChild(cancel);
    overlay.appendChild(panel);
    overlay.addEventListener('click', evt => { if (evt.target === overlay) overlay.remove(); });
    showOverlay(overlay);
}

// confirmFullOff is stage 1 of a two-stage unloader: whatever came back from
// the line comes off the cart (a full from a produce line; an empty or a
// partial from a consume line) and the cart goes on to stage 2 bare. BIN OFF
// on either side; the CLEAR books the count. The discard count is still
// stated. It leaves the cart bare, so it offers no height and posts no layers.
function confirmFullOff(nodeID, discarding) {
    const subs = [];
    if (discarding > 0) {
        subs.push(el('div', { className: 'os-co-picker-subtitle os-co-picker-warn',
            textContent: 'This slot still counts ' + discarding + '. Confirming writes it to zero.' }));
    }
    oneTapPanel(nodeID, 'Bin on the cart', subs, 'BIN OFF', 'The cart goes on to Stage 2.');
}

// confirmSendOn is stage 2 with a bare cart in the window (bin_state.bare):
// the operator puts an empty bin on it and sends it on. Plain PUSH EMPTY would
// send the cart on still bare, and a PUSH AS picker asks a question the cart
// already answers, so it is one tap — or, where the group wants heights of the
// window's type, one tap per height beside it (the type stays Core's).
function confirmSendOn(nodeID, carriers, lit) {
    oneTapPanel(nodeID, 'Cart, no bin',
        [el('div', { className: 'os-co-picker-subtitle', textContent: 'Put an empty bin on it, then send it on.' })],
        'Send on', 'Goes to the empties supermarket. No type to pick — it\'s the cart it came in as.', sendOnHeights(carriers), lit);
}

// sendOnHeights is the wanted heights a bare cart's one tap is offered beside:
// each height the group wants of the window's types, once (clearChoices).
function sendOnHeights(carriers) {
    const heights = [];
    clearChoices(getView(), carriers).forEach(function (c) {
        if (typeof c.layers === 'number' && !heights.some(function (h) { return h.layers === c.layers; })) heights.push(c);
    });
    return heights;
}

// confirmStageTwoOn is a bare cart at a stage-2 window (takesBare): one panel,
// what goes on it as stageTwoOn decided (on). EMPTY ON is the Send on above
// (clear-bin with a blank code: Core re-types the cart, count 0), with its
// heights; a consume side has none. FULL ON is the LOAD door (openLoadBin) with
// the consume lines' parts, the count defaulting to the pack. PARTIAL ON is the
// LOAD door with every part the lines take and a count the person types, which
// must be above 0. Core un-bares the cart first on either LOAD.
function confirmStageTwoOn(nodeID, carriers, lit, on) {
    const extras = [];
    if (on.full.length > 0) {
        extras.push({ label: 'FULL ON', act: function () { openLoadBinRef(nodeID, on.full, 0); } });
    }
    if (on.partial.length > 0) {
        extras.push({ label: 'PARTIAL ON', act: function () { openLoadBinRef(nodeID, on.partial, 0, { typedCount: true }); } });
    }
    oneTapPanel(nodeID, 'Cart, no bin', [el('div', { className: 'os-co-picker-subtitle', textContent: on.copy })],
        on.empty ? 'EMPTY ON' : '', on.empty ? 'No type to pick — it\'s the cart it came in as.' : '',
        on.empty ? sendOnHeights(carriers) : [], lit, extras);
}

// confirmPushEmpty shows a confirmation panel for an empty carrier sitting in an
// unloader window. Tapping PUSH EMPTY fires the push-empty endpoint so the carrier is
// sent to the supermarket and the node can request a fresh full bin.
//
// bare is the carrier's bin_state.bare: a stage-2 cart with no bin on it gets
// the one-tap Send on panel instead (confirmSendOn), or, at a takes_bare
// window, what goes on it (confirmStageTwoOn; stageTwo is then the card
// stageTwoOn decided, null anywhere else). lit is the choice the board's card
// says to send it back as: its PUSH AS is lit. cartType is the cart's type: PUSH
// AS offers the types of its family, and lit (clearChoices).
function confirmPushEmpty(nodeID, carriers, bare, lit, stageTwo, cartType) {
    if (bare) {
        if (stageTwo) confirmStageTwoOn(nodeID, carriers, lit, stageTwo);
        else confirmSendOn(nodeID, carriers, lit);
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
    // panel offers the types the node's payloads use (or the whole catalog when
    // they match none, service carriersFor), on the cart's floor.
    const choices = clearChoices(getView(), carriers, cartType, lit);
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
export function confirmClearLoaderHome(nodeID, home, spot, partial) {
    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker' });
    // What is on the home: a full bin, or a partial (Core's row flag).
    const held = partial ? 'partial' : 'full';
    panel.appendChild(el('div', { className: 'os-co-picker-title',
        textContent: (partial ? 'Partial' : 'Full') + ' bin off ' + (home || 'this home') + '?' }));
    // The words say the bin comes off by hand first, and that the cart then
    // leaves recorded empty (SPR SMN_029, 2026-11-17: the bin stayed on, and
    // 800 parts rode off as an empty).
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
        textContent: home && spot
            ? 'Take the ' + held + ' bin off the cart first. CONFIRM marks the cart empty: a robot takes it away and brings the partial from ' + spot + ' here.'
            : 'Take the ' + held + ' bin off the cart first. CONFIRM marks the cart empty.' }));

    const confirm = el('button', { className: 'os-co-picker-btn', textContent: 'BIN IS OFF · CONFIRM' });
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
// board says the same in its Bring partials home cards.
function openPullFullFromHomePicker() {
    var eligible = claimedNodes().filter(function(n) {
        var bs = (n && n.bin_state) || {};
        return isDedicated(n) && bs.occupied && bs.payload_code && !!n.buffer_partial;
    });

    var overlay = el('div', { className: 'os-co-picker-overlay' });
    var panel = el('div', { className: 'os-co-picker' });
    panel.appendChild(el('div', { className: 'os-co-picker-title', textContent: 'Full bin off a home' }));
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
        textContent: 'Pick the home you took the full bin off. Shingo marks its cart empty and brings the partial home when it can.' }));

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
            confirmClearLoaderHome(n.node.id, n.node.name, '', !!bs.partial);
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
    // Columns per screen size — 7": 2, 10": 3, 15"+: 4 — and rows that hold
    // every tile, two at least. A fixed two rows put a tile past them in an
    // implicit row the grid gives no height, where it drew over itself (the
    // HOP 4X2's fifth tile at 1024x768).
    var cols = w <= 1024 ? 2 : w <= 1400 ? 3 : 4;
    var count = claimedNodes().length || 1;
    return { cols: cols, rows: Math.max(2, Math.ceil(count / cols)) };
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

    // The residue of a refusal, once it has been answered. NOT the alert — the
    // modal was the alert, and it is gone. This is what answers "why is this cell
    // stopped" for the operator who walks up ten minutes later, for the incoming
    // shift, and for the team lead, without ever interrupting again. Who
    // refused is in the tile's panel (renderModal): a touch panel has no hover.
    var refusedForMe = entry.supply_refused_for_me;
    if (refusedForMe && refusedForMe.answered) {
        const chip = el('span', {
            className: 'os-node-alarm',
            textContent: 'NO ' + refusedForMe.payload_code,
        });
        chip.style.cssText = 'position:absolute;bottom:4px;right:4px;font-size:11px;' +
            'font-weight:700;color:#1a1204;background:#ffd98a;padding:2px 6px;border-radius:4px';
        btn.appendChild(chip);
    }

    // Keep-staged keeper: paused (REQUEST resumes it) or unable to keep its
    // spot. Amber, because it asks someone to do something; the sentence is the
    // server's (keep_staged_note), in the tile's panel. Top-left, so it does not
    // sit on the bottom-left chips.
    if (entry.keep_staged_note) {
        const paused = entry.keep_staged_note.indexOf('paused') !== -1;
        const ks = el('span', {
            className: 'os-node-alarm',
            textContent: paused ? 'KEEP-STAGED PAUSED - TAP TO RESUME' : 'SPOT BLOCKED',
        });
        ks.style.cssText = 'position:absolute;top:4px;left:4px;font-size:11px;' +
            'font-weight:700;padding:2px 6px;border-radius:4px;color:#1a1204;background:#ffd98a';
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

    // The position's automatic ask is held: its reorder point and the cause,
    // the server's words verbatim (auto_reorder_chip, domain.ReorderHoldChip),
    // "REORDER AT 110: LINE IDLE". None for a person's off where the header
    // carries the line's switch: the header says it. Amber like the keeper's
    // chip, below the tile's name (and the keeper's chip). The sentence
    // (auto_reorder_note) is in the tile's panel. Informational: REQUEST asks
    // as ever.
    if (entry.auto_reorder_chip) {
        const ar = el('span', { className: 'os-node-alarm', textContent: entry.auto_reorder_chip });
        ar.style.cssText = 'position:absolute;left:4px;top:32px;font-size:11px;' +
            'font-weight:700;padding:2px 6px;border-radius:4px;color:#1a1204;background:#ffd98a';
        btn.appendChild(ar);
    }

    // The line moved onto this sequential position before its bin arrived:
    // its strokes are logged until the bin lands. Informational; the sentence
    // is the server's (count_waits), in the tile's panel.
    if (entry.count_waits) {
        const waits = el('span', { className: 'os-node-alarm', textContent: 'NO BIN: STROKES LOGGED' });
        waits.style.cssText = 'position:absolute;bottom:4px;left:4px;font-size:11px;' +
            'font-weight:700;padding:2px 6px;border-radius:4px;color:#dbe7f5;background:#2b3a4d';
        btn.appendChild(waits);
    }

    // CONTAINED chip — the bin on this tile carries a payload whose quality
    // containment flag is ACTIVE: its next FG-bound delivery diverts to the
    // hold spot automatically. Red, and it does not ask anyone to do
    // anything — the state is the system working — but an operator watching
    // the tile sees the divert coming instead of being surprised by a robot
    // headed somewhere unexpected. What it means is in the tile's panel.
    if (entry.containment_flagged) {
        const chip = el('span', {
            className: 'os-node-alarm',
            textContent: 'CONTAINED',
        });
        chip.style.cssText = 'position:absolute;top:4px;right:4px;font-size:11px;' +
            'font-weight:700;color:#fff;background:#842029;padding:2px 6px;border-radius:4px';
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
        const awaiting = (ROLE_WORDS[tileRole(entry)] && ROLE_WORDS[tileRole(entry)].awaiting) || 'STOCK';
        const statusText = windowWords(entry, awaiting, hasActiveOrder);
        btn.appendChild(el('span', { className: 'os-node-remaining', textContent: statusText }));
        // A home whose clear runs with its empty still on it says the job above
        // the bin's label; the board's card says the same.
        if (tileRole(entry) === 'produce' && clearRunning(entry)) {
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
        // The count and the pack in UoP above a ratio of 1, the parts under
        // them; at 1 the tile as it always was (tileCount).
        const shown = tileCount(claim, remaining, capacity, noCarrier);
        btn.appendChild(el('span', {
            className: 'os-node-remaining',
            textContent: claim ? (noCarrier ? 'NO BIN' : String(shown.count)) : '--'
        }));
        if (claim && capacity > 0 && !noCarrier) {
            btn.appendChild(el('span', {
                className: 'os-node-capacity',
                textContent: '/ ' + shown.capacity
            }));
        }
        if (shown.parts) btn.appendChild(el('span', { className: 'os-node-parts', textContent: shown.parts }));
        const payloadText = claim ? (claim.payload_code || 'Unassigned') : '';
        appendETAPills(btn, inboundOrders, entry.bin_state);
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
            if (bs && bs.held_since) { confirmUnloadHeld(entry.node.id, bs.held_for, bs.held_uop, takesBare(entry), bs.uop_capacity, bs.payload_code); return; }
            // The panel lights what the window should go back as (clearHighlight).
            const lit = loader.leaves_bare ? null : clearHighlight(entry, getView().serves);
            if (fullPresent) confirmUnloadSwap(entry.node.id, carriersOf(entry),
                entry.runtime && entry.runtime.remaining_uop_cached, !!loader.leaves_bare, lit);
            else if (emptyPresent) confirmPushEmpty(entry.node.id, carriersOf(entry), !!bs.bare, lit,
                bs.bare && takesBare(entry) ? stageTwoOn(entry) : null);
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
        } else if (o.status === 'faulted') {
            // The pill is one word ("Fault" / "Replanning"); the reason is in
            // the node's panel. Red once it is a fault, not a replan.
            const word = (o.fault_line || 'Replanning').split(' · ')[0];
            pills.push({ text: word, overdue: word !== 'Replanning' });
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
        // UI text says screen, never station (docs/terminology.md, Operator
        // Station). Pinned by operator-render-state-words.test.js.
        footerStatus.textContent = 'Operator screen ready';
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
