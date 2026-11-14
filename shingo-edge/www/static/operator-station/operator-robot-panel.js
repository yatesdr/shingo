// operator-robot-panel.js — the robot panel's life cycle on a cell operator
// screen: the top bar's ROBOTS toggle, when the panel shows and when it goes,
// and the drawing, loaded the first time it shows (operator-robot-map.js).
//
// THE RULES ARE THE LEAD'S (shared-map LEAD-NOTES D13), on the owner's brief:
//
//   - A CELL OPERATOR SCREEN is a station page that is not a board (a board,
//     every tile a loader's, draws its own top line and no header). It is the
//     page whose header carries the style chip, the door to its cell picture.
//   - THE TOGGLE ENABLES THE PANEL FOR THIS SCREEN, remembered per operator
//     station in this browser. A missing or unreadable store means enabled; a
//     store that cannot be written still flips the toggle for this page life.
//   - WHILE ENABLED, the panel shows when one of the screen's orders has a
//     robot (robotOrders: a status Core sends positions for, with a robot id)
//     and goes when none does. A reshuffling parent has no robot: Core tracks
//     its child legs, not it.
//   - A TAP ON THE PANEL hides it until the screen's next order gets a robot.
//     The toggle is unchanged, and a press while the panel is tap-hidden shows
//     it again.
//   - {available: false} on the panel's stream (no live robot feed at this
//     Edge, or its Core link down: U9's one signal) hides it. The panel is
//     tried again when the screen's robot orders change (an order gets or
//     loses a robot, or moves status), when the view's Core copies turn
//     current again (coreStale: the board's "as of" goes), when this screen's
//     own event stream reconnects (robotPanelLinkBack), or on a toggle press.
//     Nothing retries on a timer: hidden is zero work.
//   - A HIDDEN PAGE (document.hidden: the tablet's screen off, another tab)
//     hides it like any cause, and the page shown again shows it again (lead
//     ruling on U10, 5). A panel under a modal or the flow panel keeps
//     working: the page is shown, and that was accepted.
//   - HIDDEN, WHATEVER THE CAUSE, IS ZERO WORK: the drawing's stream is closed,
//     its comet loop stopped, its timers cleared, and nothing writes to it
//     (operator-robot-map.js hide). Pinned by operator-robot-panel.test.js.
//
// WHY THE DRAWING IS LOADED LATE. The map (operator-robot-map.js) and the five
// shared scene modules under it are about 33 kB gzipped (measured 2026-11-14)
// that most screens never need (no robot order, or a board). They load on the
// first show, by their hashed URLs, and are held for the page's life; a load
// that fails is tried again by the same triggers as {available: false}, never
// once per view refresh. This file is what every cell screen pays at boot.

import { stationID } from './operator-util.js';
import { claimedNodes } from './operator-state.js';
import { isBoard } from './operator-board-model.js';
import { isVendorTracked } from './order-status.js';

const STORE_KEY = 'shingo.robot-panel.' + stationID;

// readEnabled: only "off" is stored; anything else, or a store that throws,
// is enabled.
function readEnabled() {
    try {
        return localStorage.getItem(STORE_KEY) !== 'off';
    } catch (_) {
        return true;
    }
}

function writeEnabled(on) {
    try {
        if (on) localStorage.removeItem(STORE_KEY);
        else localStorage.setItem(STORE_KEY, 'off');
    } catch (_) {
        // A store that cannot be written: the toggle still holds for this page.
    }
}

let enabled = readEnabled();
let lastView = null;
let toggleBtn = null;
// tapHeld is the ids of the orders that had a robot when the panel was
// tapped away, or null: while every order with a robot is one of them, the
// panel stays hidden.
let tapHeld = null;
// heldKey is the robot orders' key (ordersKey) when the panel last could not
// show ({available: false}, or its drawing did not load), or null.
let heldKey = null;
let shown = false;
let map = null;          // operator-robot-map.js's panel, once loaded
let mapLoading = null;

// robotOrders is the screen's orders that have a robot, as D13 reads it: on
// one of its tiles, in a status Core sends positions for
// (protocol.IsVendorTracked: dispatched, in_transit, staged, faulted), with
// the robot Core named. Each once, by id.
export function robotOrders(view) {
    const seen = {}, out = [];
    ((view && view.nodes) || []).forEach(function (tile) {
        (tile.orders || []).forEach(function (o) {
            if (seen[o.id] || !o.robot_id || !isVendorTracked(o.status)) return;
            seen[o.id] = true;
            out.push(o);
        });
    });
    return out.sort(function (a, b) { return a.id - b.id; });
}

// ordersKey says which robot orders there are, in which status, with which
// robot: it moves when an order gets or loses a robot or changes status.
function ordersKey(orders) {
    return orders.map(function (o) { return o.id + ':' + o.status + ':' + o.robot_id; }).join(',');
}

function cellScreen(view) {
    return !!view && !isBoard(claimedNodes());
}

// coreStale says the view's copies of Core's feeds are old: the board's own
// "Bins as of" / "Catalog as of" (view.bins.as_of, view.catalog.as_of, set by
// the Edge's stampStationBins and stampStationCatalog through
// engine.CurrentAt). A copy is current while a heartbeat ack confirmed it
// within engine.FeedAsOfAfter (150 s): the rule the panel's {available} reads
// for Core's link (U9). Every board is nudged when a copy crosses it either
// way, so the first view after Core answers again is current. A view holding
// neither copy says nothing either way (false).
function coreStale(view) {
    return !!view && !!((view.bins && view.bins.as_of) || (view.catalog && view.catalog.as_of));
}
let lastStale = false;

function tapHidden(orders) {
    return !!tapHeld && orders.every(function (o) { return tapHeld[o.id]; });
}

// syncRobotPanel is the page's one door in, after every render of the view
// (operator.js renderAll): it shows the panel, hides it, or hands the shown
// panel the view.
export function syncRobotPanel(view) {
    lastView = view;
    const orders = robotOrders(view);
    const stale = coreStale(view);
    if (heldKey !== null && (heldKey !== ordersKey(orders) || (lastStale && !stale))) heldKey = null;
    lastStale = stale;
    if (tapHeld && !tapHidden(orders)) tapHeld = null;
    const want = cellScreen(view) && enabled && orders.length > 0 && !tapHeld && heldKey === null && !document.hidden;
    if (want) show(view, orders);
    else hide();
    renderToggle();
}

function show(view, orders) {
    if (shown) {
        if (map) map.update(view, orders);
        return;
    }
    shown = true;
    loadMap().then(function (m) {
        if (!m) {
            shown = false;
            heldKey = ordersKey(robotOrders(lastView));
            return;
        }
        if (!shown) return;
        if (!map) map = m.createRobotMap({ stationID: stationID, onTap: onTap, onUnavailable: onUnavailable });
        map.show(lastView, robotOrders(lastView));
    });
}

function hide() {
    if (!shown) return;
    shown = false;
    if (map) map.hide();
}

function loadMap() {
    if (mapLoading) return mapLoading;
    mapLoading = import('/static/operator-station/operator-robot-map.js').catch(function (err) {
        mapLoading = null;
        console.error('robot panel', err);
        return null;
    });
    return mapLoading;
}

// onTap: the panel was tapped. It goes until an order not robot-held now gets
// a robot.
function onTap() {
    tapHeld = {};
    robotOrders(lastView).forEach(function (o) { tapHeld[o.id] = true; });
    hide();
    renderToggle();
}

// onUnavailable: the panel's stream said {available: false}.
function onUnavailable() {
    heldKey = ordersKey(robotOrders(lastView));
    hide();
}

// The page hidden or shown again: the panel goes or comes back by the same
// rule (syncRobotPanel reads document.hidden).
document.addEventListener('visibilitychange', function () {
    if (lastView) syncRobotPanel(lastView);
});

// robotPanelLinkBack: this screen's own event stream connected again
// (operator.js, the bus's connected). A panel {available: false} held is
// tried again.
export function robotPanelLinkBack() {
    if (heldKey === null) return;
    heldKey = null;
    if (lastView) syncRobotPanel(lastView);
}

function onToggle() {
    const orders = robotOrders(lastView);
    if (enabled && !shown && tapHeld && orders.length > 0) {
        tapHeld = null;
    } else {
        enabled = !enabled;
        writeEnabled(enabled);
        tapHeld = null;
    }
    heldKey = null;
    if (lastView) syncRobotPanel(lastView);
    else renderToggle();
}

// robotPanelToggle is the top bar's ROBOTS toggle for view, one element for
// the page's life, or null on a screen that draws no panel (a board).
// renderHeader empties the bar on every draw, and operator.js puts this back
// after it.
export function robotPanelToggle(view) {
    if (!cellScreen(view)) return null;
    if (!toggleBtn) {
        toggleBtn = document.createElement('button');
        toggleBtn.type = 'button';
        toggleBtn.className = 'os-header-btn robots';
        toggleBtn.textContent = 'ROBOTS';
        toggleBtn.setAttribute('aria-label', 'Robots on this screen');
        toggleBtn.addEventListener('click', onToggle);
    }
    renderToggle();
    return toggleBtn;
}

function renderToggle() {
    if (!toggleBtn) return;
    const pressed = enabled ? 'true' : 'false';
    if (toggleBtn.getAttribute('aria-pressed') !== pressed) toggleBtn.setAttribute('aria-pressed', pressed);
}
