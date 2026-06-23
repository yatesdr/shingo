import { el, esc, fillColor, postAction, showToast } from './operator-util.js';
import { getView, claimedNodes, isReplenishing } from './operator-state.js';
import { isActive } from './order-status.js';
import { cardModel, headerModel, nodeFacts, ROLE_WORDS } from './operator-window-state.js';

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

export function renderHeader() {
    const view = getView();
    const style = view.current_style ? view.current_style.name : 'No Style';
    const target = view.target_style ? (' \u2192 ' + view.target_style.name) : '';
    headerInfo.textContent = view.process.name + ' - ' + style + target;

    headerActions.innerHTML = '';

    const badge = el('span', {
        className: 'os-health-badge ' + (view.station.health_status === 'online' ? 'online' : 'offline')
    });
    headerActions.appendChild(badge);

    // Active style chip — sits next to the changeover button so the operator
    // can see which style is running. During changeover shows "current → target".
    const styleName = view.current_style ? view.current_style.name : 'No Style';
    const targetName = view.target_style ? view.target_style.name : null;
    const styleChip = el('div', { className: 'os-header-style' + (targetName ? ' changing' : '') });
    styleChip.appendChild(el('span', { className: 'os-header-style-label', textContent: 'STYLE' }));
    const styleValue = el('span', { className: 'os-header-style-value' });
    styleValue.textContent = targetName ? styleName + ' \u2192 ' + targetName : styleName;
    styleChip.appendChild(styleValue);
    headerActions.appendChild(styleChip);

    // Bin-loader board mode (single manual_swap claim) is operated by a
    // forklift driver — strip the changeover/cutover buttons. Those flows
    // belong on a regular operator station, not this view.
    //
    // HMI Tier 2 (post-F'-Phase-2): the changeover-wide RELEASE header
    // button was removed. Each node tile is the release surface during
    // changeover — operator clicks the tile, gets the same production
    // release modal, picks disposition (or accepts the auto-detected
    // pre-fill), submits. Phase 2's deferred-supply chain auto-releases
    // the supply leg when the evac robot picks up.
    //
    // CUTOVER button: shown only when the process does NOT have PLC-
    // driven cutover enabled (Theme C's auto_cutover_enabled flag). On
    // auto-cutover processes, the PLC's falling edge on Changeover_Active
    // fires CompleteProcessProductionCutoverFromPLC automatically — a
    // manual button creates ambiguity about who's "really" in charge of
    // cutover, and a stuck PLC bit is something to investigate, not to
    // mask with a manual override. Theme B's gate is the safety net for
    // both paths.
    if (!isBoardMode()) {
        if (view.active_changeover) {
            if (!view.process.auto_cutover_enabled) {
                headerActions.appendChild(headerBtn('CUTOVER', 'cutover', confirmCutover));
            }
            // CANCEL during active changeover: aborts every in-flight evac+
            // supply order on the process's node tasks, marks the changeover
            // row cancelled, and resets the process back to active_production
            // on the from-style. Core handles safe resolution of orders
            // already mid-route (queued → disappears, loaded robot → store-
            // order rerouted to a safe drop). Operator wraps this in a
            // confirmation modal because the action is destructive.
            headerActions.appendChild(headerBtn('CANCEL', 'cancel-changeover', confirmCancelChangeover));
        } else {
            headerActions.appendChild(headerBtn('CHANGEOVER', 'changeover', openChangeoverPicker));
        }
    }

    headerActions.appendChild(headerBtn('REFRESH', 'refresh', loadViewRef));
}

function isBoardMode() {
    const nodes = claimedNodes();
    return nodes.length === 1
        && nodes[0].active_claim
        && nodes[0].active_claim.swap_mode === 'manual_swap';
}

function openChangeoverPicker() {
    const view = getView();
    const styles = view.available_styles || [];
    const currentID = view.current_style ? view.current_style.id : null;
    const others = styles.filter(s => s.id !== currentID);
    if (others.length === 0) {
        showToast('No other styles available', 'error');
        return;
    }

    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker' });
    panel.appendChild(el('div', { className: 'os-co-picker-title', textContent: 'Change over to:' }));

    for (const s of others) {
        const btn = el('button', { className: 'os-co-picker-btn', textContent: s.name });
        btn.addEventListener('click', () => {
            overlay.remove();
            startChangeover(s.id, s.name);
        });
        panel.appendChild(btn);
    }

    const cancel = el('button', { className: 'os-co-picker-btn cancel', textContent: 'CANCEL' });
    cancel.addEventListener('click', () => overlay.remove());
    panel.appendChild(cancel);

    overlay.appendChild(panel);
    overlay.addEventListener('click', evt => { if (evt.target === overlay) overlay.remove(); });
    document.body.appendChild(overlay);
}

async function startChangeover(toStyleID, styleName) {
    const view = getView();
    const pid = view.process.id;
    const ok = await postAction('/api/processes/' + pid + '/changeover/start', {
        to_style_id: toStyleID,
        called_by: (view.station.name && view.station.name.trim()) || 'operator',
        notes: ''
    }, loadViewRef);
    if (ok) showToast('Changeover to ' + styleName + ' started', 'success');
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

export function renderGrid() {
    const nodes = claimedNodes();

    const cardGrid = grid.querySelector('.os-board-cards');
    const savedScrollTop = cardGrid ? cardGrid.scrollTop : 0;

    grid.innerHTML = '';

    if (nodes.length === 0) {
        grid.classList.remove('os-board-mode');
        document.body.classList.remove('os-board-mode-active');
        grid.style.removeProperty('--os-cols');
        grid.style.removeProperty('--os-rows');
        const empty = el('div', { id: 'os-grid-empty', textContent: 'No claimed nodes' });
        grid.appendChild(empty);
        return;
    }

    // Loader board mode: all claimed nodes are manual_swap loaders. Branch on the
    // LAYOUT flag — home-location (dedicated per-payload positions) renders one
    // card per home across all positions; single-window renders the one slot's
    // payloads. A station is one layout (the engineer's pick), so if any loader
    // node is home-location the whole board renders that way.
    const manualSwapNodes = nodes.filter(function(n) {
        return n.active_claim && n.active_claim.swap_mode === 'manual_swap';
    });
    const allManualSwap = manualSwapNodes.length >= 1 && manualSwapNodes.length === nodes.length;
    // A consume (drain) shared-window loader renders as physical SLOTS — the node
    // grid below (one tile per window, showing the bin/part actually present), NOT
    // the loader payload board. An unloader operator just clears whatever full bin
    // lands in a slot, so a card-per-allowed-payload catalog (often the whole plant's
    // parts) is pure noise. Home-location (dedicated-position) unloaders keep their
    // per-position board; produce-side loaders keep the payload board.
    const drainSlots = allManualSwap
        && !manualSwapNodes.some(function(n) { return n.home_location_loader; })
        && manualSwapNodes.every(function(n) { return n.active_claim.role === 'consume'; });
    if (allManualSwap && !drainSlots) {
        grid.classList.add('os-board-mode');
        document.body.classList.add('os-board-mode-active');
        grid.style.removeProperty('--os-cols');
        grid.style.removeProperty('--os-rows');
        if (manualSwapNodes.some(function(n) { return n.home_location_loader; })) {
            renderHomeLocationBoard(manualSwapNodes);
        } else {
            renderPayloadBoard(manualSwapNodes[0]);
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

// confirmUnloadSwap gates the unloader swap behind an explicit operator
// confirmation. The swap (clear-bin) is irreversible mid-shift — it confirms
// the inbound full was received, sends the empty back, and pulls the next full
// — so the operator must declare the full pulled / bin empty before it fires.
// Reuses the os-co-picker overlay styling already used by the changeover/cutover
// confirmations so the look is consistent.
function confirmUnloadSwap(nodeID) {
    const overlay = el('div', { className: 'os-co-picker-overlay' });
    const panel = el('div', { className: 'os-co-picker' });
    panel.appendChild(el('div', { className: 'os-co-picker-title', textContent: 'Full pulled, empty filled?' }));
    panel.appendChild(el('div', { className: 'os-co-picker-subtitle',
        textContent: 'Confirms the bin is unloaded. The empty returns to the supermarket and the next full is requested.' }));

    const confirm = el('button', { className: 'os-co-picker-btn', textContent: 'CONFIRM SWAP' });
    confirm.addEventListener('click', function() {
        overlay.remove();
        postAction('/api/process-nodes/' + nodeID + '/clear-bin', undefined, loadViewRef);
    });
    panel.appendChild(confirm);

    const cancel = el('button', { className: 'os-co-picker-btn cancel', textContent: 'CANCEL' });
    cancel.addEventListener('click', () => overlay.remove());
    panel.appendChild(cancel);

    overlay.appendChild(panel);
    overlay.addEventListener('click', evt => { if (evt.target === overlay) overlay.remove(); });
    document.body.appendChild(overlay);
}

// buildLoaderCard renders ONE (position × payload) card — the atomic unit of the
// loader board. Returns the card element, or null when a normal kanban loader's
// idle card should be hidden. counters.queuePos tracks the per-payload queue badge
// across cards. The card's STATE (status/detail/action/badge facts) comes from
// cardModel (operator-window-state.js); this function is presentation only — DOM,
// the transitional coverage badge, and idle-card hiding.
function buildLoaderCard(entry, code, counters, opts) {
    var claim = entry.active_claim;
    var card = el('div', { className: 'os-board-card' });
    var cs = cardModel(entry, code);

    // Coverage (ACTIVE = a running style needs this now; PRELOAD = covered only
    // by an inactive style) — drives the badge + the transitional idle override.
    var isActiveStylePayload = entry.operator_driven &&
        (entry.active_style_payloads || []).indexOf(code) !== -1;

    // Transitional board: "NO DEMAND" is meaningless (operator-driven). On an
    // idle card show the coverage meaning instead.
    if (entry.operator_driven && cs.cls === 'os-board-nodemand') {
        if (isActiveStylePayload) {
            cs.statusText = 'ACTIVE'; cs.statusClass = 'os-board-tag-lineside'; cs.detail = '';
        } else {
            cs.statusText = 'PRELOAD'; cs.statusClass = 'os-board-tag-preload'; cs.detail = 'Available to stage';
        }
    }

    // Normal (non-transitional) loader hides idle produce cards so the operator
    // sees only what's called for. Transitional keeps every card; a home-location
    // board passes keepIdle so every physical home stays on screen.
    if (!entry.operator_driven && claim.role === 'produce' && cs.cls === 'os-board-nodemand' && !(opts && opts.keepIdle)) return null;

    card.classList.add(cs.cls);
    if (cs.loadNow) card.classList.add('os-board-load-now');

    if (entry.operator_driven) {
        card.classList.add(isActiveStylePayload ? 'os-board-cov-on-active' : 'os-board-cov-on-preload');
        card.appendChild(el('span', {
            className: 'os-board-cov ' + (isActiveStylePayload ? 'os-board-cov-active' : 'os-board-cov-preload'),
            textContent: isActiveStylePayload ? 'ACTIVE' : 'PRELOAD',
        }));
    }

    card.appendChild(el('div', { className: 'os-board-code', textContent: code }));
    card.appendChild(el('span', { className: 'os-board-tag ' + cs.statusClass, textContent: cs.statusText }));
    card.appendChild(el('div', { className: 'os-board-detail', textContent: cs.detail }));

    if (entry.operator_driven && isActiveStylePayload) {
        var lsMap = entry.active_payload_lineside || {};
        var lsUOP = lsMap[code] != null ? lsMap[code] : 0;
        var starved = (entry.starved_payloads || {})[code] === true;
        card.appendChild(el('div', {
            className: 'os-board-lineside' + (starved ? ' os-board-lineside--starved' : ''),
            textContent: 'Lineside ' + lsUOP + ' UOP' + (starved ? ' — PRELOAD' : ''),
        }));
        if (starved) card.classList.add('os-board-card--starved');
    }

    // Queue-position badge only for REAL per-payload orders (the agnostic
    // blank-payload empty must not stamp every card with a meaningless number).
    if (cs.queueCount > 0) {
        var badge = el('span', { className: 'os-board-pos', textContent: String(counters.queuePos) });
        if (cs.delivered) badge.classList.add('os-board-pos-delivered');
        else if (cs.inTransit) badge.classList.add('os-board-pos-transit');
        else badge.classList.add('os-board-pos-queued');
        card.appendChild(badge);
        counters.queuePos++;
    }

    if (cs.action === 'load') {
        card.style.cursor = 'pointer';
        card.addEventListener('click', function() {
            openLoadBinRef(entry.node.id, [code], claim.uop_capacity || 0);
        });
    } else if (cs.action === 'unload') {
        card.style.cursor = 'pointer';
        card.addEventListener('click', function() {
            confirmUnloadSwap(entry.node.id);
        });
    } else {
        card.style.cursor = 'pointer';
        card.addEventListener('click', function() {
            openModalRef(entry.node.id);
        });
    }

    return card;
}

// allowedPayloadsFor resolves the payload list a loader entry's cards come from:
// the multi-process active/all union (transitional shows all, normal shows
// active), falling back to the single claim's list.
function allowedPayloadsFor(entry) {
    var claim = entry.active_claim;
    var modeList = entry.operator_driven ? entry.all_style_payloads : entry.active_style_payloads;
    var allowed = (modeList && modeList.length > 0)
        ? modeList
        : ((claim.allowed_payload_codes && claim.allowed_payload_codes.length > 0)
            ? claim.allowed_payload_codes
            : (claim.payload_code ? [claim.payload_code] : []));
    // Transitional: sort ACTIVE-style payloads ahead of PRELOAD-only, alpha
    // tiebreak (deterministic — some field kiosks predate V8 stable sort).
    if (entry.operator_driven) {
        var activeSet = entry.active_style_payloads || [];
        allowed = allowed.slice().sort(function(a, b) {
            var aRank = activeSet.indexOf(a) !== -1 ? 0 : 1;
            var bRank = activeSet.indexOf(b) !== -1 ? 0 : 1;
            if (aRank !== bRank) return aRank - bRank;
            return a < b ? -1 : (a > b ? 1 : 0);
        });
    }
    return allowed;
}

function renderPayloadBoard(entry) {
    const claim = entry.active_claim;
    const runtime = entry.runtime || {};
    const remaining = runtime.remaining_uop_cached != null ? runtime.remaining_uop_cached : 0;
    const binState = entry.bin_state;
    const hasBin = binState && binState.occupied;
    const binLabel = binState && binState.bin_label ? binState.bin_label : 'No bin';
    const binPayload = binState && binState.payload_code ? binState.payload_code : '';
    const roleLabel = claim.role === 'produce' ? 'Loader' : 'Unloader';

    // Header status badge from the shared model — same facts the cards read, so the
    // badge can't contradict a card. Worded by role (loader awaits a BIN, unloader a FULL).
    var hb = headerModel(entry);

    var infoBar = el('div', { className: 'os-board-header' });
    infoBar.innerHTML =
        '<div>' +
            '<div style="font-size:42px;font-weight:700;color:#fff">' + esc(entry.node.name) + ' - ' + roleLabel + '</div>' +
            '<div style="font-size:20px;color:#aab;margin-top:6px">Manual Swap | ' +
                (claim.allowed_payload_codes ? claim.allowed_payload_codes.length : 0) + ' payloads configured</div>' +
        '</div>' +
        '<div style="text-align:right">' +
            '<div style="font-size:28px;font-weight:600;color:#fff">Bin: ' + esc(binLabel) + '</div>' +
            (binPayload ? '<div style="font-size:20px;color:#aab;margin-top:4px">' + esc(binPayload) + ' | UOP: ' + remaining + '</div>' : '') +
            '<div style="display:inline-block;font-size:22px;font-weight:700;padding:8px 20px;border-radius:6px;margin-top:8px;' +
                hb.color + '">' + hb.text +
            '</div>' +
        '</div>';
    grid.appendChild(infoBar);

    // Payload cards come from the active/all union (transitional shows all, a
    // normal loader shows active), falling back to the claim list — see
    // allowedPayloadsFor. Each card's state comes from cardModel via buildLoaderCard;
    // facts here are only for the request-bar guard.
    var allowed = allowedPayloadsFor(entry);
    var facts = nodeFacts(entry);

    // Manual request / jumpstart button — ALL bin loaders (rendered just below,
    // above the cards). One button, not per-card: the operator is asking for a
    // bin to be brought to the node, not picking a payload. Use cases:
    //   - transitional loader staging an empty proactively;
    //   - a NORMAL (automated kanban) loader whose queue was cancelled in Core
    //     and now sits idle with no demand — this is the operator's jumpstart.
    //
    // Anti-spam: a manual_swap node has ONE physical bin slot, so at most one bin
    // may be inbound at a time. canRequest is the single guard — allowed only
    // when no bin is parked (!hasBin) AND nothing is already inbound (!hasDemand,
    // which covers any non-terminal order at the node, including a bin en route,
    // and on a normal loader is exactly the "queue cancelled / idle" state). The
    // instant a request fires, hasDemand flips true on the next SSE refresh and
    // the button greys out, so repeated taps can't stack the queue. The server
    // enforces the same rule (RequestEmptyBin rejects when an empty is already in
    // flight) as defense-in-depth against double-tap races and direct callers.
    var canRequest = !hasBin && !facts.hasDemand;

    // Payload for the request splits by role:
    //   - produce (bin loader): an empty bin is a generic carrier, so the
    //     request is payload-AGNOSTIC — post NO payload_code. Core sources any
    //     compatible empty and LoadBin binds the real payload when the operator
    //     fills it. (Replaces the old allowed[0] default, which fabricated a
    //     payload nobody asked for and could tag/stage the wrong card — see
    //     RequestEmptyBin. Assumes a single-carrier loader; a multi-carrier
    //     loader would need a carrier picker here.)
    //   - consume (unloader): REQUEST FULL pulls a SPECIFIC payload's full bin,
    //     so it still needs a code. claim.payload_code is blank on manual_swap,
    //     so fall to the first allowed.
    var isProduce = claim.role === 'produce';
    var requestPayload = isProduce
        ? ''
        : ((claim.payload_code && allowed.indexOf(claim.payload_code) !== -1)
            ? claim.payload_code
            : (allowed.length > 0 ? allowed[0] : ''));

    // Render the bar when the action is expressible: always for a loader (the
    // empty needs no payload), and for an unloader only when there's a payload
    // to ask for.
    if (isProduce || requestPayload) {
        var reqBar = el('div', { className: 'os-board-reqbar' });
        var reqLabel = isProduce ? 'REQUEST EMPTY' : 'REQUEST FULL';
        var reqReason = hasBin ? 'bin at node' : (facts.hasDemand ? 'bin already inbound' : '');
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
        grid.appendChild(reqBar);
    }

    var cardGrid = el('div', { className: 'os-board-cards' });

    // One card per payload, via the shared Card builder (also used by the
    // home-location board). buildLoaderCard returns null for an idle card a
    // normal kanban loader hides.
    var counters = { queuePos: 1 };
    var rendered = 0;
    allowed.forEach(function(code) {
        var card = buildLoaderCard(entry, code, counters);
        if (card) { cardGrid.appendChild(card); rendered++; }
    });

    // Column count tracks the cards actually shown (a demand-driven loader can
    // render fewer than `allowed`), so the grid doesn't reserve empty columns.
    var cols = rendered <= 3 ? Math.max(rendered, 1) : (rendered <= 6 ? 3 : 4);
    cardGrid.style.setProperty('--os-board-cols', cols);

    if (rendered === 0) {
        cardGrid.appendChild(el('div', {
            style: 'color:#666;font-style:italic;padding:24px;text-align:center;grid-column:1/-1',
            // allowed empty → nothing configured; allowed non-empty but nothing
            // rendered → a normal loader with no active demand (all idle cards
            // filtered). Distinct copy so the operator knows which it is.
            textContent: allowed.length === 0 ? 'No payloads configured' : 'No active demand'
        }));
    }

    grid.appendChild(cardGrid);
}

// renderHomeLocationBoard renders a home-location loader: each station position
// (node) is a dedicated home for one payload, so we show one card per
// (home × its payload) across all the station's loader nodes — N homes on one
// screen, the whole loader at a glance. Each card is backed by its own node's
// bin state + actions and carries a home label. keepIdle keeps every physical
// home on screen even with no current demand.
function renderHomeLocationBoard(nodes) {
    // Home-location is a role-neutral layout: produce loaders OR consume unloaders
    // (dedicated finished-goods exits). Title reflects whichever this station is.
    var roles = {};
    nodes.forEach(function(n) { if (n.active_claim) roles[n.active_claim.role] = true; });
    var title = (roles.produce && roles.consume) ? 'Stations'
        : roles.consume ? 'Unloader' : 'Bin Loader';
    var header = el('div', { className: 'os-board-header' });
    header.innerHTML =
        '<div><div style="font-size:42px;font-weight:700;color:#fff">' + title + ' — Home Locations</div>' +
        '<div style="font-size:20px;color:#aab;margin-top:6px">' + nodes.length + ' dedicated position' + (nodes.length === 1 ? '' : 's') + '</div></div>';
    grid.appendChild(header);

    var cardGrid = el('div', { className: 'os-board-cards' });
    var counters = { queuePos: 1 };
    var rendered = 0;
    nodes.forEach(function(node) {
        if (!node.active_claim) return;
        allowedPayloadsFor(node).forEach(function(code) {
            var card = buildLoaderCard(node, code, counters, { keepIdle: true });
            if (!card) return;
            // Home label (physical position) + its own bin state, prepended so a
            // wall of cards stays scannable by home.
            var bs = node.bin_state || {};
            var binTxt = bs.occupied ? (bs.payload_code ? 'LOADED' : 'EMPTY') : 'AWAITING';
            card.insertBefore(el('div', {
                className: 'os-board-home',
                textContent: esc(node.node.name) + ' · ' + binTxt,
            }), card.firstChild);
            cardGrid.appendChild(card);
            rendered++;
        });
    });

    var cols = rendered <= 3 ? Math.max(rendered, 1) : (rendered <= 6 ? 3 : (rendered <= 12 ? 4 : 5));
    cardGrid.style.setProperty('--os-board-cols', cols);
    if (rendered === 0) {
        cardGrid.appendChild(el('div', {
            style: 'color:#666;font-style:italic;padding:24px;text-align:center;grid-column:1/-1',
            textContent: 'No homes configured',
        }));
    }
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

// isDrainSlot reports a consume (drain) shared-window loader slot — the same class
// renderGrid's drainSlots routes to the per-window slot view. Drives the slot-only
// click (tap a parked full → confirm swap; empty = no-op) and the board palette below.
function isDrainSlot(entry) {
    const c = entry.active_claim;
    return !!c && c.swap_mode === 'manual_swap' && c.role === 'consume'
        && !entry.home_location_loader;
}

// drainColorClass maps a drain slot to the loader-board card palette (not the
// saturated fill colors): full parked = green, full inbound = amber, empty = neutral.
function drainColorClass(entry) {
    const bs = entry.bin_state;
    const awaiting = (entry.orders || []).some(o => isActive(o.status));
    if (bs && bs.occupied && bs.payload_code) return 'os-drain-ready';
    if (awaiting) return 'os-drain-awaiting';
    return 'os-drain-idle';
}

function createNodeButton(entry) {
    const claim = entry.active_claim;
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
    const drain = isDrainSlot(entry);
    let stateClass;
    if (drain) {
        // A drain (consume) slot owns its OWN palette (idle/awaiting/ready); the
        // generic order-state colors below are loader/press semantics. Critically,
        // an in_transit order at a drain is the OUTbound empty (U2) leaving after a
        // clear, NOT a full arriving — so 'os-in-transit' (purple = inbound) would be
        // a lie. drainColorClass maps any active order to amber 'awaiting'. (An AMR-fed
        // unloader's inbound U1 also reads as amber 'awaiting' — the palette's intent.)
        stateClass = drainColorClass(entry);
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

    // Banner label for the priority states. The full-tile background
    // already signals "something is up"; the label says what.
    if (releaseReady && !drain) {
        btn.appendChild(el('span', { className: 'os-node-banner', textContent: 'RELEASE READY' }));
    } else if (inChangeover && !drain) {
        btn.appendChild(el('span', { className: 'os-node-banner', textContent: 'CHANGEOVER' }));
    }

    // [REP] corner badge stays for replenishing (different signal — bin
    // move in flight). [CO] badge is suppressed because the full-tile
    // CHANGEOVER banner makes it redundant.
    const icon = statusIcon(entry);
    if (icon && icon === '[REP]') {
        btn.appendChild(el('span', { className: 'os-node-icon', textContent: icon }));
    }

    if (claim && claim.swap_mode === 'manual_swap') {
        const binState = entry.bin_state;
        const binLabel = binState && binState.bin_label ? binState.bin_label : '';
        const binPayload = binState && binState.payload_code ? binState.payload_code : '';
        // "Awaiting stock" = any non-terminal order. Just 'queued' lost the
        // indicator the moment the order advanced (sourcing, dispatched,
        // in_transit, delivered-awaiting-confirm).
        const hasActiveOrder = (entry.orders || []).some(o => isActive(o.status));
        let statusText;
        if (isDrainSlot(entry) && binState && binState.occupied && binPayload) {
            // Drain slot with a full PARKED → show the part that's here to clear, even
            // if a NEXT full is queued/inbound (hasActiveOrder). "AWAITING" must not
            // mask a present full — the green tile + tap-to-confirm already say "act".
            statusText = binPayload;
        } else if (hasActiveOrder) {
            statusText = 'AWAITING ' + ((ROLE_WORDS[claim.role] && ROLE_WORDS[claim.role].awaiting) || 'STOCK');
        } else if (binPayload) {
            statusText = binPayload;
        } else if (remaining > 0) {
            statusText = 'LOADED';
        } else if (binState && binState.occupied) {
            statusText = 'EMPTY';
        } else {
            statusText = 'NO BIN';
        }
        btn.appendChild(el('span', { className: 'os-node-remaining', textContent: statusText }));
        if (binLabel) {
            const labelEl = el('span', { className: 'os-node-payload', textContent: binLabel });
            labelEl.style.cssText = 'font-size:14px;font-weight:600;color:#fff';
            btn.appendChild(labelEl);
        } else {
            btn.appendChild(el('span', { className: 'os-node-payload', textContent: 'Manual Swap' }));
        }
    } else {
        btn.appendChild(el('span', {
            className: 'os-node-remaining',
            textContent: claim ? String(remaining) : '--'
        }));
        if (claim && capacity > 0) {
            btn.appendChild(el('span', {
                className: 'os-node-capacity',
                textContent: '/ ' + capacity
            }));
        }
        const payloadText = claim ? (claim.payload_code || 'Unassigned') : '';
        appendETAPills(btn, inboundOrders, entry.bin_state);
        btn.appendChild(el('span', { className: 'os-node-payload', textContent: payloadText }));
    }

    if (isDrainSlot(entry)) {
        // Drain slot: tap a parked full → confirm-swap panel (no payload picker).
        // An empty/idle slot has nothing to pull, so the tap is a no-op.
        btn.addEventListener('click', () => {
            const bs = entry.bin_state;
            const fullPresent = bs && bs.occupied && bs.payload_code;
            if (fullPresent) confirmUnloadSwap(entry.node.id);
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

// Bucket boundaries match the user-approved display rules:
//   < 45s   → "Arriving"
//   45–90s  → "ETA: ~1 min"
//   ≥ 90s   → "ETA: ~N min" rounded to nearest whole minute
//   overdue by > 60s → "Running late" + amber pill
// No sub-minute precision past the first bucket — fake precision was the
// thing Uber's UX research dropped. If the order has no ETA yet (Core
// hasn't stamped one, e.g. mid-transition or backfill pending) we show
// nothing rather than a placeholder; the purple background already says
// "robot inbound", the pill is the time-detail layer.
function formatETA(etaStr) {
    if (!etaStr) return { text: '', overdue: false, empty: true };
    const etaMs = Date.parse(etaStr);
    if (isNaN(etaMs)) return { text: '', overdue: false, empty: true };
    const remainingSec = (etaMs - Date.now()) / 1000;
    const graceSec = 60;
    if (remainingSec < -graceSec) {
        return { text: 'Running late', overdue: true };
    }
    if (remainingSec < 45) {
        return { text: 'Arriving', overdue: false };
    }
    if (remainingSec < 90) {
        return { text: 'ETA: ~1 min', overdue: false };
    }
    const mins = Math.round(remainingSec / 60);
    return { text: 'ETA: ~' + mins + ' min', overdue: false };
}

// isReleaseReady drives the os-release-ready blue glow. Same screen
// handles both production and changeover; the gate behind the operator's
// click differs per context, so this function picks the right gate.
//
// Changeover context (entry.changeover_task present):
//   - Phase 2 model. Click fires evac via ReleaseOrderWithLineside; the
//     supply leg auto-fires on evac pickup-confirm via HandleBinPickedUp's
//     deferred-supply branch.
//   - Paired evac+supply: glow when evac is at `staged` (robot at slot
//     wait point) AND supply is at `in_transit` or `staged` (dispatched,
//     past Manager.ReleaseOrder's pre-dispatch guard — supply has a
//     VendorOrderID so the auto-release will fire cleanly when evac
//     picks up). If supply is still `acknowledged` or earlier, clicking
//     would fire evac but the supply auto-release would silently no-op
//     against the pre-dispatch supply order; the glow waits past that.
//   - Standalone evac (no paired supply, e.g. drop-situation tasks):
//     glow when evac is at `staged`. No supply chain to coordinate.
//
// Production context (no changeover_task):
//   - Two-robot swap mid-cycle. Click fires ReleaseStagedOrders which
//     releases both legs at once — needs both robots at wait points.
//     entry.swap_ready (computed in store/station_views.go ComputeSwapReady)
//     already encodes that condition. Single source of truth: defer to
//     it for the production gate.
//
// Returns false otherwise (pre-dispatch, terminal, single-robot consume
// where Release is always available without a "ready" moment, etc.).
function isReleaseReady(entry) {
    const task = entry.changeover_task;
    if (task) {
        const orders = entry.orders || [];
        const byID = (id) => id == null ? null : orders.find(o => o.id === id) || null;
        const evac = byID(task.old_material_release_order_id);
        const supply = byID(task.next_material_order_id);
        if (!evac) return false;
        if (evac.status !== 'staged') return false;
        if (supply) {
            return supply.status === 'in_transit' || supply.status === 'staged';
        }
        return true;
    }
    // Production context — defer to the existing swap_ready flag.
    return !!entry.swap_ready;
}

function nodeColorClass(entry) {
    const claim = entry.active_claim;
    if (!claim) return 'os-unclaimed';
    const remaining = entry.runtime ? entry.runtime.remaining_uop_cached : 0;
    if (claim.swap_mode === 'manual_swap') {
        const hasActiveOrder = entry.orders && entry.orders.some(o => isActive(o.status));
        if (hasActiveOrder) return 'os-mid';
        return remaining > 0 ? 'os-full' : 'os-empty';
    }
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
    const state = view.process.production_state || '';

    if (co) {
        const nodes = claimedNodes();
        const coNodes = nodes.filter(n => n.changeover_task);
        const done = coNodes.filter(n => n.changeover_task.state === 'switched' || n.changeover_task.state === 'verified').length;
        footerStatus.textContent = co.from_style_name + ' \u2192 ' + co.to_style_name +
            ' [' + done + '/' + coNodes.length + ' nodes]';
    } else {
        footerStatus.textContent = 'Operator Station Ready';
    }

    footerBadge.textContent = state.replace(/_/g, ' ');
    footerBadge.className = 'os-footer-badge';
    if (state === 'active_production') footerBadge.classList.add('producing');
    if (state === 'changeover_active') footerBadge.classList.add('changeover');
}

// Expose fillColor so the modal module can render the fill bar without
// re-importing it from operator-util.
export { fillColor };
