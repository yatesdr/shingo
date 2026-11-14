// dashboard-map.js — 'robot-map' dashboard kind. A spatial plant view: scene
// nodes laid out by their world coordinates, live robot positions, and this
// dashboard's active orders color-coded by status.
//
// Same platform contract as the task board:
//   - static layout  : GET /api/map/points        (scene points: pos_x/pos_y)
//   - live robots     : robot-update SSE feed      (seeded once by GET /api/robots)
//   - scoped orders   : GET /api/board/orders?dashboard=<id>, refetched on the
//                       order-update change-ping
//
// Rendering is SVG. World coords map straight into the viewBox with Y negated
// (world Y is up, screen Y is down) so the plant isn't drawn upside-down; text
// stays upright because we negate per-element rather than flipping a group.
//
// Visual language (the "floor HUD"): the travel network is a legible neutral-steel
// floor plan — aisles and LM nodes render in a neutral steel hue so the room structure
// reads at a glance. Robots stay the hero via saturation, halo, and motion: a bright
// chevron with heading, a pulsing halo, and a monospace name chip. Robot color tracks
// order status (saturated lifecycle hue while on an active job); a delivered/confirmed
// robot still moving, or any idle robot in motion, shows neutral grey — green only at
// rest. A charge or park bay is drawn only while a robot stands on it or an order
// names it.
//
// Routes follow the aisles. The travel network is the scene's real
// connectivity — drivable path segments (SEER advanced curves) served by
// /api/map/edges, each stored in the direction robots may drive it. A driving
// robot's route is the path RDS planned for it (the frame's `path`), drawn
// along those segments; with no path, or one the scene cannot draw, it draws
// no route at all (lead decisions D6, D10). An order not yet driving draws a
// dashed intent line along the route model's way (shared/scene-net.js,
// directed, weighed by the distance driven: D9), or a straight hint where the
// network does not join its ends. Nothing is derived from point proximity (a
// derived graph invented links through walls); with no synced edges the
// network is simply empty.
//
// A curved segment is DRAWN CURVED and WEIGHED CURVED. SEER states each
// segment's shape with two cubic-Bezier handles, which arrive as
// ctrl1_x/ctrl1_y/ctrl2_x/ctrl2_y; the map used to drop them and draw the
// chord, which at Springfield paints a lane up to 1.30 m from the one the
// robot drives (LM10-LM113, off a 7.17 m chord), so the fleet visibly leaves
// the network as painted. The same handles fix the route model's weight,
// which on a chord runs up to 24% short of the distance actually driven.
//
// THE DECISION IS THE HANDLES, NOT THE CLASS NAME. In the scene these counts
// were taken from (377 segments; Springfield had 807 directed rows on
// 2026-11-14), className said "DegenerateBezier" on 264; 171 of those sit
// within a centimetre of their chord (a cubic spelling a straight line), 93
// bow further, and 58 bow more than 10 cm. The scene holds exactly TWO
// BezierPaths, both at 0.17 m. Meanwhile "StraightPath" segments carry no
// handles at all, or an all-zero pair Core rejects before this file sees it.
// Every class name in the scene therefore covers both shapes, and only the
// geometry says which one a given lane is.

import { escapeHtml } from '/static/shared/dom.js';
import { formatClock, serverNow } from '/static/shared/clock.js';
import { onSSE, setSSEReloadOnBuild } from '/static/shared/bus.js';
// The scene machinery is shared/'s: scene-geom.js the projection, orientation
// and cubic arithmetic; scene-net.js the scene's network read once (its rows,
// its lanes); scene-view.js the retained <svg> that draws it; comet-layer.js
// the comets over it. This page keeps what decides what is drawn: its orders,
// legs, focus, calm rule, legends and rail.
import {
  makeProjector, turnedProjector, mapTurn, headingDeg, dist2, isCoord
} from '/static/shared/scene-geom.js';
import { sceneNet, sceneRoute, nearestVertex } from '/static/shared/scene-net.js';
import { pathWorld, hopsWorld } from '/static/shared/scene-path.js';
import { createSceneView, svgNode } from '/static/shared/scene-view.js';
import { createCometLayer } from '/static/shared/comet-layer.js';

(function () {
  var body = document.body;
  var dashboardId = body.getAttribute('data-dashboard-id');

  // Read a CSS custom property off :root with a hex fallback, so the map's
  // colors come from the shared token palette (P13, shared/tokens.css) instead
  // of duplicated hexes. Resolved once at module load — the kiosk theme is
  // static (data-theme=dark) — and the fallback keeps the map sane if
  // tokens.css somehow didn't load.
  function cssVar(name, fallback) {
    try {
      var v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
      return v || fallback;
    } catch (e) { return fallback; }
  }

  // ── status / state palettes — the UNIFIED status dots (shared/tokens.css) ──
  // Same palette that feeds the badges and the floor-display board, so a status
  // reads the same hue everywhere. Indigo is absent on purpose (it's the UI
  // accent). Fallbacks mirror the dark-theme token values.
  var STATUS_COLOR = {
    in_transit: cssVar('--status-in-transit-dot', '#34c3e0'),
    staged: cssVar('--status-staged-dot', '#15b8a0'),
    reshuffling: cssVar('--status-reshuffling-dot', '#df6fb4'),
    dispatched: cssVar('--status-dispatched-dot', '#4f9bff'),
    blocked: cssVar('--status-blocked-dot', '#f85149'),
    // A faulted order reads as a blocked one to a person, so it is red too;
    // without this entry a fault fell through to grey everywhere it is drawn.
    faulted: cssVar('--status-blocked-dot', '#f85149'),
    acknowledged: cssVar('--status-pending-dot', '#8b95a5'),
    queued: cssVar('--status-queued-dot', '#7aa2f0'),
    pending: cssVar('--status-pending-dot', '#8b95a5'),
    delivered: cssVar('--status-delivered-dot', '#3fb950')
  };
  // Robot states unchanged (P13): ready green, error red, offline gray; a moving
  // robot with no order falls back to the in-transit cyan.
  //
  // `offline` reads --sub-4, not --text-tertiary. Two reasons, and the second is
  // the one that matters. --text-tertiary was deleted for measuring below the
  // normal-text floor in both themes (see shared/tokens.css). But it should not
  // have been here even at a legal value: a robot dot is a MARK, held to 3:1,
  // and --sub-4's documented role is exactly "tick marks, structural dots —
  // marks that carry meaning". Painting a mark with a type token is the mirror
  // of the category error TestChipInkIsNotItsFill guards, and a nudge to a text
  // token for a text reason would have silently moved this dot.
  // --sub-4 is #66768f dark / #76859d light, clearing 3:1 in both.
  //
  // `paused` is still on --text-muted and is the same category error, unfixed:
  // it is a live token so nothing breaks, and picking its replacement is a
  // question about the robot-state vocabulary (is paused structure or status?)
  // rather than about this deletion.
  var STATE_COLOR = {
    // Grey at rest (HOP, 2026-11-12): green is delivered's, and a robot
    // with nothing to do is not news.
    ready: cssVar('--map-robot-idle', '#8b949e'),
    busy: cssVar('--status-in-transit-dot', '#34c3e0'),
    paused: cssVar('--text-muted', '#8b949e'),
    error: cssVar('--status-blocked-dot', '#f85149'),
    offline: cssVar('--sub-4', '#66768f')
  };
  // Bays as sockets: a robot docked on a charge/park point takes the bay's
  // hue, so an occupied ring reads as a filled socket and an empty ring as an
  // available bay. Amber belongs to charging (a robot state, unchanged by the
  // P13 palette work — staged is now teal in the unified status scheme).
  // Charging is yellow; parked is the same grey as any robot at rest.
  var DOCK_COLOR = { charge: cssVar('--map-dock-charge', '#e3b341'), park: cssVar('--map-robot-idle', '#8b949e') };
  var CHARGE_RING = cssVar('--map-bay-ring', '#3c4a5e');
  var PARK_RING = cssVar('--map-bay-ring', '#3c4a5e');
  // Active-node accent (P21): an order source/destination ("hot") node is marked
  // by the GLYPH ITSELF taking the UI accent — an indigo stroke + soft glow —
  // instead of a separate status-colored ring on top (which crossed delivered-
  // green with "active" and misaligned/z-fought the glyph). Green stays reserved
  // for delivered. Read once at load; the kiosk theme is static.
  var ACCENT = cssVar('--accent', '#7c7cf0');
  var ACCENT_GLOW = cssVar('--accent-glow', 'rgba(124,124,240,0.55)');
  // Neutral grey for a robot in motion without an active job (just-delivered driving
  // away, idle repositioning to park). Never green while physically moving.
  var IDLE_MOVE_COLOR = cssVar('--map-robot-idle', '#8b949e');
  // Action point dot: one step brighter than the travel dot, quieter than a ring.
  var NODE_ACTION_COLOR = cssVar('--map-node-action', '#90a2bf');

  // ── state ──────────────────────────────────────────────────────────
  var points = [];          // scene points (static layout)
  var sceneEdges = [];      // real drivable segments from /api/map/edges
  var bays = [];            // charge/park points, for dock detection
  var nodeIndex = {};       // lowercased node name -> {x, y} (world space)
  var robots = {};          // vehicle_id -> normalized robot
  var orders = [];          // scoped active orders
  var orderByRobot = {};    // robot_id -> order
  var hotNodes = {};        // lowercased node name -> status (highlight)
  var view = null;          // {minX, minY, w, h} screen-space bounding box
  var turnDeg = 0;          // the map's quarter turn clockwise (computeView)
  var focusRobot = null;    // click-to-focus: id of the robot whose route is lit
  var mapKeyOpen = false;   // node-type symbol key collapsed by default (reference info)
  // The route comets, on their own layer over the map (shared/comet-layer.js).
  var comets = createCometLayer(document.getElementById('map-svg-wrap'));
  var clickBound = false;   // host click handler attached once
  // ── auto-framing state ─────────────────────────────────────────────
  var viewTarget = null;          // desired view after hysteresis; `view` eases toward this
  var fullPlantView = null;       // whole-plant bbox in screen space (minimap fixed viewBox)
  var viewEaseRAF = 0;            // rAF handle for the viewBox ease loop
  var minimapEl = null;           // persistent minimap <svg> inside .map-region
  var minimapViewportRect = null; // viewport indicator rect inside the minimap
  var minimapRobotGroup = null;   // robot-dots <g> inside the minimap
  // Above this many moving orders (render's movingComets: each order moving
  // with its robot seen moving, counted per order, path or not), the ambient
  // view calms: no comets and no intent lines, just the robots (a lane still
  // draws where laneVisible says), so a busy floor doesn't become 20 arrows
  // fighting for attention. Clicking a robot lights its comet and its intent
  // line again. Pinned: twelveCometsThenNone, countedPerOrderNotPerRobot,
  // theCalmCountsMovingOrdersPathOrNot.
  var COMET_LIMIT = 12;

  // ── manual view: wheel-zoom + drag-pan break auto-follow ────────────
  // Once the user zooms or pans, `userView` latches true and computeView()
  // stops re-framing to the active-order ROI — the operator keeps the view they
  // set. The recenter control (and re-enabling follow) clears it. Zoom/pan write
  // `view` directly and refresh the live viewBox + minimap without the ease
  // loop, so there is no fight between the user's gesture and auto-framing.
  var userView = false;
  var forceReframe = false;  // recenter sets this so adoptViewTarget eases past hysteresis
  var dragging = false;      // left-button drag-pan in progress
  var dragLastX = 0, dragLastY = 0;
  var dragMoved = false;     // exceeded the click/drag threshold → suppress the focus click
  var suppressClick = false; // set on drag end so the trailing click doesn't focus a robot

  // ── pickup-latch for leg-aware comets ──────────────────────────────
  // The board payload exposes NO explicit picked-up/leg field (see BoardOrder in
  // engine_board.go — only status/source/delivery/current_station), and no
  // protocol status distinguishes the fetch leg from the carry leg (in_transit
  // spans both). So the current leg is INFERRED from robot↔source proximity and
  // LATCHED: once the robot reaches its source (bin pickup) during an order it is
  // "carrying" for the rest of that order; before that it is "fetching". Latch
  // per order_id, pruned when the order leaves the active set.
  var pickedUpByOrder = {};   // order_id → true once the robot has reached source
  var PICKUP_RADIUS = 0.9;    // × graphScale (world units) — "at the source node"

  // ── activity feed + status rail ────────────────────────────────────
  var activityFeed = [];   // [{text, level, ts}] — newest first
  var prevOrderMap = {};   // order_id -> order snapshot, for transition diffs
  var feedTimer = 0;       // setInterval handle for age-fade re-renders
  var FEED_MAX_AGE_MS = 3 * 60 * 1000; // events older than 3 min are dropped
  var FEED_MAX_ITEMS = 5;

  // proj maps world (x, y) to screen coords — see scene-geom.js for the
  // projection itself. It is a VARIABLE, not a function declaration, because
  // the projection is configured by orientation: computeView reassigns it the
  // moment it learns the plant footprint, and everything that projects runs
  // after computeView. The `false` here is the same starting orientation the
  // old module-state flag had, so the first frame is unchanged.
  var proj = makeProjector(false);

  // The quarter turn this screen was set to with the map's Rotate button. It is
  // the screen's own, kept in its browser, because a wall monitor has no login to
  // save anything with. None saved, the plant's shape decides (mapTurn).
  var turnKey = '';
  var chosenTurn;
  try {
    turnKey = 'shingo.map-turn.' + document.body.dataset.dashboardId;
    chosenTurn = parseInt(localStorage.getItem(turnKey), 10);
  } catch (_) { /* no storage: this screen just doesn't remember */ }

  // ── header chrome ──────────────────────────────────────────────────
  function tickClock() {
    var el = document.getElementById('dash-clock');
    if (el) el.textContent = formatClock(serverNow());
  }
  setInterval(tickClock, 1000); tickClock();

  function setConnected(ok) {
    var el = document.getElementById('dash-conn');
    if (el) el.className = 'dash-conn ' + (ok ? 'dash-conn-ok' : 'dash-conn-down');
  }

  // writeHTML sets el's markup only when it differs from what this page last
  // wrote there, so a legend or rail that has not changed costs no parse, no
  // layout and no new listeners on a render. It compares with the string it
  // wrote, not with el.innerHTML, which a browser re-serialises. Returns
  // whether it wrote. Pinned by the render pin legendsAndRailWriteOnlyOnChange.
  var shownHTML = new WeakMap();
  function writeHTML(el, html) {
    if (shownHTML.get(el) === html) return false;
    shownHTML.set(el, html);
    el.innerHTML = html;
    return true;
  }

  // Bucket a robot into one of the header-legend categories so the legend can
  // double as a live fleet summary. Paused/offline robots are uncounted.
  function robotBucket(r) {
    var ord = orderByRobot[r.id];
    if (ord) {
      if (ord.status === 'blocked' || ord.status === 'faulted') return 'blocked';
      if (ord.status === 'staged' || ord.status === 'queued' ||
          ord.status === 'acknowledged' || ord.status === 'pending') return 'staged';
      if (ord.status === 'delivered') return 'idle';
      return 'in_transit';
    }
    if (r.state === 'error') return 'error';
    if (r.state === 'busy') return 'in_transit';
    if (r.state === 'ready') {
      // A moving idle robot stays in the Idle bucket (green chevron on the
      // map) — In transit is reserved for robots actually working.
      if (!isMoving(r)) {
        var dock = dockOf(r);
        if (dock && dock.kind === 'charge') return 'charging';
        if (dock && dock.kind === 'park') return 'parked';
      }
      return 'idle';
    }
    return null;
  }

  function renderLegend() {
    var el = document.getElementById('map-legend');
    if (!el) return;
    var counts = { in_transit: 0, staged: 0, blocked: 0, charging: 0, parked: 0, idle: 0, error: 0 };
    Object.keys(robots).forEach(function (k) {
      var b = robotBucket(robots[k]);
      if (b) counts[b]++;
    });
    var items = [
      ['in_transit', 'In transit', STATUS_COLOR.in_transit], ['staged', 'Staged', STATUS_COLOR.staged],
      ['blocked', 'Blocked', STATUS_COLOR.blocked],
      ['charging', 'Charging', DOCK_COLOR.charge], ['parked', 'Parked', DOCK_COLOR.park],
      ['idle', 'Idle', STATE_COLOR.ready], ['error', 'Error', STATE_COLOR.error]
    ];
    // Only active states show — a zero bucket is just clutter (Blocked sitting
    // there doing nothing). Exceptions (Blocked/Error) therefore appear only
    // when something is actually blocked/faulted, so they read as real signal.
    writeHTML(el, items.filter(function (it) { return counts[it[0]] > 0; }).map(function (it) {
      return '<span class="map-legend-item"><span class="map-legend-dot" style="background:' + it[2] +
        '"></span>' + it[1] + '<span class="map-legend-count">' + counts[it[0]] + '</span></span>';
    }).join('') || '<span class="map-legend-item map-legend-zero">No active robots</span>');
  }

  // ── robot normalization (handles SSE lowercase + REST PascalCase) ──
  function deriveState(r) {
    if (r.Connected === false) return 'offline';
    if (r.Emergency || r.Blocked) return 'error';
    if (r.Busy) return 'busy';
    if (r.Available === false) return 'paused';
    return 'ready';
  }
  function normRobot(r) {
    var x = (r.x !== undefined) ? r.x : r.X;
    var y = (r.y !== undefined) ? r.y : r.Y;
    var a = (r.angle !== undefined) ? r.angle : r.Angle;
    return {
      id: r.vehicle_id || r.VehicleID || '',
      x: x, y: y,
      angle: a || 0,
      state: r.state || deriveState(r),
      charging: !!((r.charging !== undefined) ? r.charging : r.Charging),
      station: r.station || r.CurrentStation || '',
      // The alarm Core judged to have begun with this robot's fault (sse.go
      // faultAlarm); never the robot's whole alarm list.
      faultAlarm: r.fault_alarm || null,
      // The route RDS has planned for the robot, as the scene's point names,
      // its current point first; `path` on both the frame and /api/robots
      // (lowercase there too), and absent when there is none.
      path: Array.isArray(r.path) ? r.path : []
    };
  }

  // Motion detection from position deltas. The fleet's Busy flag (SEER
  // ProcBusiness) stays false for direct/manual moves, so a driving robot
  // would render as idle if we trusted flags alone. If the robot physically
  // displaced more than a jitter threshold between updates, it's moving;
  // the state lingers briefly so turn pauses don't flicker disc/chevron.
  var MOVE_LINGER_MS = 5000; // bridges brief stops (turns, order-cancel pauses) without flickering to a disc
  function mergeRobot(rb) {
    var prev = robots[rb.id];
    rb.lastMoveAt = prev ? (prev.lastMoveAt || 0) : 0;
    if (prev && isFinite(prev.x) && isFinite(rb.x)) {
      var eps = (graphScale > 0) ? Math.max(graphScale * 0.02, 0.05) : 0.05;
      var dx = rb.x - prev.x, dy = rb.y - prev.y;
      if (dx * dx + dy * dy > eps * eps) rb.lastMoveAt = Date.now();
    }
    robots[rb.id] = rb;
  }
  // DELIBERATELY Date.now(), not serverNow. lastMoveAt and the
  // activity feed's ts are stamped BY THIS BROWSER when a frame arrives, and
  // compared against this browser's clock. They measure how long since this tab
  // observed something, which is a real wall-time question about the SSE feed
  // and not a question about the plant's clock. Converting them would compare a
  // browser stamp against a server now, which is the defect serverNow exists to
  // remove — pointed the other way.
  function isMoving(r) {
    return (Date.now() - (r.lastMoveAt || 0)) < MOVE_LINGER_MS;
  }

  // Which bay (charge/park point) a robot is docked on, if any. Robots park
  // dead-on their bay point, so proximity within half a typical edge length
  // is the test; an explicit charging flag (REST payload) wins outright.
  // bayOccupied: a robot stands on the bay at (x, y), by dockOf's own distance.
  // With no network to scale by there is no judging it, and every bay draws.
  function bayOccupied(x, y) {
    if (!(graphScale > 0)) return true;
    var lim = Math.pow(graphScale * 0.5, 2);
    for (var k in robots) {
      var r = robots[k];
      if (!isFinite(r.x) || !isFinite(r.y)) continue;
      var dx = x - r.x, dy = y - r.y;
      if (dx * dx + dy * dy <= lim) return true;
    }
    return false;
  }

  function dockOf(r) {
    if (r.charging) return { kind: 'charge', x: r.x, y: r.y };
    if (!bays.length || !(graphScale > 0)) return null;
    var lim = Math.pow(graphScale * 0.5, 2);
    for (var i = 0; i < bays.length; i++) {
      var dx = bays[i].x - r.x, dy = bays[i].y - r.y;
      if (dx * dx + dy * dy <= lim) return bays[i];
    }
    return null;
  }

  // A robot at rest is one disc, one size, whatever its state (HOP,
  // 2026-11-12: a paused robot on a park bay drew larger than the ready robots
  // in the row, because only a ready robot was fitted to its bay). On a bay it
  // sits on the bay's centre, since the reported pose is a few cm off it and a
  // row of parked robots should read as a row. The size is the one a charging
  // robot fills its ring at, so the bay under it never shows as a rim.
  function restDisc(r, nodeR) {
    var dock = dockOf(r);
    return { x: dock ? dock.x : r.x, y: dock ? dock.y : r.y, r: nodeR * 1.05 };
  }

  // Effective robot color: fault > active job > physical motion > bay hue > state.
  // A delivered/confirmed order is NOT a job — the robot is transitioning. While
  // physically moving with no active job (just delivered, repositioning, returning
  // to park) the robot shows IDLE_MOVE_COLOR (neutral grey) so green never appears
  // on a moving robot. Bay hues apply only at rest so a passing robot doesn't flash.
  function robotColor(r, ord, moving) {
    // Error/offline takes priority regardless of order state.
    if (r.state === 'error' || r.state === 'offline') return STATE_COLOR[r.state];
    // Treat the order as a job only while it is still active.
    var job = ord && ord.status !== 'delivered' && ord.status !== 'confirmed' ? ord : null;
    if (job) return STATUS_COLOR[job.status] || STATE_COLOR[r.state] || '#888';
    // No active job: grey if physically in motion; dock-hue / state-color at rest.
    // Use physical-motion signals only (not !!ord) so delivered-order robots that
    // have stopped already get their resting color immediately.
    var physMoving = r.state === 'busy' || isMoving(r);
    if (physMoving) return IDLE_MOVE_COLOR;
    if (r.state === 'ready') {
      var dock = dockOf(r);
      if (dock) return DOCK_COLOR[dock.kind];
    }
    return STATE_COLOR[r.state] || '#888';
  }

  // ── coordinate framing: ROI view with smooth easing ─────────────────
  // Four concerns kept separate: (1) full-plant extent for orientation + clamp,
  // (2) region-of-interest (robots + active routes), (3) hysteresis so tiny jitter
  // doesn't nudge the frame each tick, (4) a lerp ease loop that updates the live
  // viewBox attributes between render() calls without a full SVG rebuild.

  function lerpView(a, b, t) {
    return {
      minX: a.minX + (b.minX - a.minX) * t,
      minY: a.minY + (b.minY - a.minY) * t,
      w: a.w + (b.w - a.w) * t,
      h: a.h + (b.h - a.h) * t
    };
  }

  // Set the viewBox on both live SVGs without rebuilding the scene.
  function updateLiveViewBox() {
    if (!view) return;
    var vb = view.minX + ' ' + view.minY + ' ' + view.w + ' ' + view.h;
    var sceneSVG = document.querySelector('#map-svg-wrap .map-svg');
    if (sceneSVG) sceneSVG.setAttribute('viewBox', vb);
    comets.setViewBox(vb);
  }

  var EASE_ALPHA = 0.18; // per-frame lerp factor (~18% per ~16ms frame)

  function tickEase() {
    viewEaseRAF = 0;
    if (!view || !viewTarget) return;
    var eps = Math.max(view.w, view.h) * 0.002; // stop when within 0.2% of extent
    if (Math.abs(view.minX - viewTarget.minX) < eps &&
        Math.abs(view.minY - viewTarget.minY) < eps &&
        Math.abs(view.w - viewTarget.w) < eps &&
        Math.abs(view.h - viewTarget.h) < eps) {
      view = viewTarget;
      updateLiveViewBox();
      updateMinimapDynamic();
      // Sized for the view the ease began from: redraw at the one it reached
      // (see ZOOM_SETTLE_MS). adoptViewTarget's hysteresis holds the frame
      // unless the robots have really moved, so this redraw seldom eases again.
      scheduleRender();
      return;
    }
    view = lerpView(view, viewTarget, EASE_ALPHA);
    updateLiveViewBox();
    updateMinimapDynamic();
    viewEaseRAF = requestAnimationFrame(tickEase);
  }

  function startEase() {
    if (viewEaseRAF) return;
    if (window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
      view = viewTarget;
      updateLiveViewBox();
      updateMinimapDynamic();
      return;
    }
    viewEaseRAF = requestAnimationFrame(tickEase);
  }

  // ── manual view controls ───────────────────────────────────────────
  // Map a client pixel to view (screen/proj) space. Uses the scene SVG's
  // screen CTM for an exact inverse regardless of preserveAspectRatio; falls
  // back to a linear container-rect map (view is aspect-corrected, so exact).
  function clientToWorld(cx, cy) {
    var s = document.querySelector('#map-svg-wrap .map-svg');
    if (s && s.getScreenCTM) {
      var m = s.getScreenCTM();
      if (m) {
        var inv = m.inverse();
        return { x: cx * inv.a + cy * inv.c + inv.e, y: cx * inv.b + cy * inv.d + inv.f };
      }
    }
    var host = document.getElementById('map-svg-wrap');
    if (!host || !view) return null;
    var rect = host.getBoundingClientRect();
    if (rect.width < 1 || rect.height < 1) return null;
    return {
      x: view.minX + ((cx - rect.left) / rect.width) * view.w,
      y: view.minY + ((cy - rect.top) / rect.height) * view.h
    };
  }

  // Clamp the manual zoom to a sane band around the full-plant extent so the
  // user can't zoom into the void or way past the whole plant.
  function clampManualView() {
    if (!view || !fullPlantView) return;
    var fpExt = Math.max(fullPlantView.w, fullPlantView.h);
    var curExt = Math.max(view.w, view.h);
    var clamped = Math.max(fpExt * 0.08, Math.min(fpExt * 1.25, curExt));
    if (clamped !== curExt) {
      var sc = clamped / curExt;
      var cx = view.minX + view.w / 2, cy = view.minY + view.h / 2;
      view.w *= sc; view.h *= sc;
      view.minX = cx - view.w / 2; view.minY = cy - view.h / 2;
    }
  }

  function applyManualView() {
    if (viewEaseRAF) { cancelAnimationFrame(viewEaseRAF); viewEaseRAF = 0; }
    clampManualView();
    updateLiveViewBox();
    updateMinimapDynamic();
    var btn = document.getElementById('map-recenter');
    if (btn) btn.classList.add('is-active');
  }

  // Zoom toward a client point, keeping the world point under the cursor fixed.
  function zoomAt(cx, cy, factor) {
    if (!view) return;
    var w = clientToWorld(cx, cy);
    if (!w) return;
    userView = true;
    view = {
      minX: w.x - (w.x - view.minX) * factor,
      minY: w.y - (w.y - view.minY) * factor,
      w: view.w * factor,
      h: view.h * factor
    };
    applyManualView();
  }

  // Recenter: drop manual mode and ease from the current view back to the ROI.
  function recenterView() {
    if (!userView) return;
    userView = false;
    forceReframe = true;
    computeView(); // recomputes ROI + adoptViewTarget (forceReframe eases to it)
  }

  // Rotate: a quarter turn clockwise, remembered on this screen. Everything drawn
  // is re-projected, so the frame snaps to the turned plant rather than easing from
  // a view of the old one.
  function rotateMap() {
    chosenTurn = (turnDeg + 90) % 360;
    try { localStorage.setItem(turnKey, String(chosenTurn)); } catch (_) { /* this visit only */ }
    if (viewEaseRAF) { cancelAnimationFrame(viewEaseRAF); viewEaseRAF = 0; }
    userView = false;
    viewTarget = null;
    var btn = document.getElementById('map-recenter');
    if (btn) btn.classList.remove('is-active');
    comets.clear();
    render();
  }

  // Minimap click-to-jump: center the current view on the clicked world point
  // (stays in manual mode — it's navigation, not a follow-restore; recenter is
  // the follow-restore).
  function jumpToWorld(cx, cy) {
    if (!view || !minimapEl || !minimapEl.getScreenCTM) return;
    var m = minimapEl.getScreenCTM();
    if (!m) return;
    var inv = m.inverse();
    var wx = cx * inv.a + cy * inv.c + inv.e;
    var wy = cx * inv.b + cy * inv.d + inv.f;
    userView = true;
    view.minX = wx - view.w / 2;
    view.minY = wy - view.h / 2;
    applyManualView();
  }

  // Marker sizes follow the view and are set by render(), while a wheel zoom
  // moves only the viewBox. Without a redraw, robots, names and lines grew and
  // shrank with the zoom and snapped back on the next robot update, seconds
  // later (HOP, 2026-11-12). Redraw once the wheel rests.
  var ZOOM_SETTLE_MS = 150;
  var zoomSettleTimer = 0;

  var viewControlsWired = false;
  function wireViewControls() {
    if (viewControlsWired) return;
    var host = document.getElementById('map-svg-wrap');
    if (!host) return;
    viewControlsWired = true;

    host.addEventListener('wheel', function (e) {
      if (!view) return;
      e.preventDefault();
      // Scroll up → zoom in (extent shrinks); down → zoom out. Gentle per-notch.
      var factor = e.deltaY > 0 ? 1.12 : 1 / 1.12;
      zoomAt(e.clientX, e.clientY, factor);
      clearTimeout(zoomSettleTimer);
      zoomSettleTimer = setTimeout(scheduleRender, ZOOM_SETTLE_MS);
    }, { passive: false });

    host.addEventListener('mousedown', function (e) {
      if (e.button !== 0 || !view) return;
      dragging = true; dragMoved = false;
      dragLastX = e.clientX; dragLastY = e.clientY;
      host.classList.add('map-dragging');
    });
    window.addEventListener('mousemove', function (e) {
      if (!dragging || !view) return;
      var host2 = document.getElementById('map-svg-wrap');
      if (!host2) return;
      var rect = host2.getBoundingClientRect();
      if (rect.width < 1 || rect.height < 1) return;
      var dxp = e.clientX - dragLastX, dyp = e.clientY - dragLastY;
      dragLastX = e.clientX; dragLastY = e.clientY;
      if (Math.abs(dxp) + Math.abs(dyp) > 2) dragMoved = true;
      userView = true;
      view.minX -= dxp * (view.w / rect.width);
      view.minY -= dyp * (view.h / rect.height);
      applyManualView();
    });
    window.addEventListener('mouseup', function () {
      if (!dragging) return;
      dragging = false;
      host.classList.remove('map-dragging');
      if (dragMoved) suppressClick = true; // swallow the trailing focus click
    });

    var recenter = document.getElementById('map-recenter');
    if (recenter) recenter.addEventListener('click', function () { recenterView(); });
    var rotate = document.getElementById('map-rotate');
    if (rotate) rotate.addEventListener('click', rotateMap);
  }

  // Hysteresis: only adopt a new target if the shift is meaningful.
  // Prevents per-SSE-tick robot micro-jitter from nudging the frame constantly.
  function adoptViewTarget(candidate) {
    // Recenter: ease from the current manual view back to the ROI,
    // ignoring the jitter hysteresis so a deliberate recenter always animates.
    if (forceReframe) {
      forceReframe = false;
      viewTarget = candidate;
      startEase();
      return;
    }
    if (!viewTarget) {
      viewTarget = candidate;
      view = candidate; // snap on first paint — no animation for the initial frame
      return;
    }
    var extentOld = Math.max(viewTarget.w, viewTarget.h);
    var cxOld = viewTarget.minX + viewTarget.w / 2;
    var cyOld = viewTarget.minY + viewTarget.h / 2;
    var cxNew = candidate.minX + candidate.w / 2;
    var cyNew = candidate.minY + candidate.h / 2;
    var drift = Math.sqrt(Math.pow(cxNew - cxOld, 2) + Math.pow(cyNew - cyOld, 2));
    var extentNew = Math.max(candidate.w, candidate.h);
    var extentChange = Math.abs(extentNew - extentOld) / extentOld;
    if (drift > extentOld * 0.15 || extentChange > 0.25) {
      viewTarget = candidate;
      startEase();
    }
  }

  function computeView() {
    // Full-plant world coords: all scene points + orphan graph vertices.
    var wx = [], wy = [];
    points.forEach(function (p) {
      if (isFinite(p.pos_x) && isFinite(p.pos_y)) { wx.push(p.pos_x); wy.push(p.pos_y); }
    });
    orphans.forEach(function (t) { wx.push(t.x); wy.push(t.y); });
    // No scene at all: fall back to robot positions
    if (!wx.length) {
      Object.keys(robots).forEach(function (k) {
        var r = robots[k];
        if (isFinite(r.x) && isFinite(r.y)) { wx.push(r.x); wy.push(r.y); }
      });
    }
    if (!wx.length) { view = null; viewTarget = null; return; }

    var minWx = Math.min.apply(null, wx), maxWx = Math.max.apply(null, wx);
    var minWy = Math.min.apply(null, wy), maxWy = Math.max.apply(null, wy);
    // Orientation is based on the FULL plant so a zoomed-in ROI never flips.
    // This is the ONLY place orientation is decided, so it is the only place
    // the projector is rebuilt — the turn itself is still read directly by the
    // robot chevron, which rotates with it.
    turnDeg = mapTurn(chosenTurn, minWx, maxWx, minWy, maxWy);
    proj = turnedProjector(turnDeg);

    // Full-plant screen-space bbox (stable reference for clamping + minimap viewBox)
    var fsx = [], fsy = [];
    for (var i = 0; i < wx.length; i++) {
      var fs = proj(wx[i], wy[i]);
      fsx.push(fs[0]); fsy.push(fs[1]);
    }
    var fpMinX = Math.min.apply(null, fsx), fpMaxX = Math.max.apply(null, fsx);
    var fpMinY = Math.min.apply(null, fsy), fpMaxY = Math.max.apply(null, fsy);
    var fpW = Math.max(fpMaxX - fpMinX, 1), fpH = Math.max(fpMaxY - fpMinY, 1);
    var fpPad = Math.max(fpW, fpH) * 0.05;
    fullPlantView = {
      minX: fpMinX - fpPad, minY: fpMinY - fpPad,
      w: fpW + 2 * fpPad, h: fpH + 2 * fpPad
    };

    // ── State-based view target ────────────────────────────────────────
    // IDLE floor (no active orders): target = whole plant so parked robots
    // never shrink the frame (fixes the P24a cramped-strip problem).
    // ACTIVE: gentle ROI zoom, never smaller than 55% of full-plant extent.
    var INACTIVE_ROI = { delivered: true, confirmed: true, cancelled: true };
    var activeOrds = orders.filter(function (o) { return !INACTIVE_ROI[o.status]; });
    var fullExt = Math.max(fpW, fpH);
    var maxExt = Math.max(fpW + 2 * fpPad, fpH + 2 * fpPad);

    var roiMinX, roiMinY, roiW, roiH;

    if (!activeOrds.length) {
      // Idle: start from the pre-padded full-plant rect; aspect-fill below.
      roiMinX = fullPlantView.minX; roiMinY = fullPlantView.minY;
      roiW = fullPlantView.w; roiH = fullPlantView.h;
    } else {
      // Active: include each active order's robot + source + delivery + the
      // robot's route, as RDS planned it (robotRoute, lead decision D10).
      var roiSx = [], roiSy = [];
      activeOrds.forEach(function (o) {
        var rbt = robots[o.robot_id];
        if (rbt && isFinite(rbt.x) && isFinite(rbt.y)) {
          var rs = proj(rbt.x, rbt.y); roiSx.push(rs[0]); roiSy.push(rs[1]);
        }
        var sn = findNode(o.source_node);
        if (sn) { var ss = proj(sn.x, sn.y); roiSx.push(ss[0]); roiSy.push(ss[1]); }
        var dn = findNode(o.delivery_node);
        if (dn) { var ds = proj(dn.x, dn.y); roiSx.push(ds[0]); roiSy.push(ds[1]); }
        var rworld = rbt ? robotRoute(rbt) : null;
        if (rworld) {
          rworld.forEach(function (w) {
            var ws = proj(w[0], w[1]); roiSx.push(ws[0]); roiSy.push(ws[1]);
          });
        }
      });
      if (!roiSx.length) { roiSx = fsx.slice(); roiSy = fsy.slice(); }

      var rMinX = Math.min.apply(null, roiSx), rMaxX = Math.max.apply(null, roiSx);
      var rMinY = Math.min.apply(null, roiSy), rMaxY = Math.max.apply(null, roiSy);
      var rW = Math.max(rMaxX - rMinX, 1), rH = Math.max(rMaxY - rMinY, 1);
      var rCX = (rMinX + rMaxX) / 2, rCY = (rMinY + rMaxY) / 2;

      // Pad ~12% then clamp: max = full plant, min = 55% of full-plant extent.
      var rPad = Math.max(rW, rH) * 0.12;
      rW += 2 * rPad; rH += 2 * rPad;
      var curExt = Math.max(rW, rH);
      var minExt = fullExt * 0.55;
      if (curExt > maxExt) {
        var sd = maxExt / curExt; rW *= sd; rH *= sd;
      } else if (curExt < minExt) {
        var su = minExt / curExt; rW *= su; rH *= su;
      }
      roiMinX = rCX - rW / 2; roiMinY = rCY - rH / 2;
      roiW = rW; roiH = rH;
    }

    // Expand to fill the container's pixel aspect ratio — kills letterbox margins.
    var container = document.getElementById('map-svg-wrap');
    if (container && container.clientWidth > 1 && container.clientHeight > 1) {
      var cAsp = container.clientWidth / container.clientHeight;
      var vAsp = roiW / roiH;
      var vCX = roiMinX + roiW / 2, vCY = roiMinY + roiH / 2;
      if (vAsp < cAsp) {
        roiW = roiH * cAsp; roiMinX = vCX - roiW / 2;
      } else {
        roiH = roiW / cAsp; roiMinY = vCY - roiH / 2;
      }
    }

    // While the user drives the view, keep the full-plant reference (minimap +
    // orientation, computed above) fresh but DON'T re-frame — leave `view` where
    // the operator put it. Recenter clears userView and calls computeView again.
    if (userView) return;
    adoptViewTarget({ minX: roiMinX, minY: roiMinY, w: roiW, h: roiH });
  }

  function buildNodeIndex() {
    nodeIndex = {};
    bays = [];
    points.forEach(function (p) {
      if (!isFinite(p.pos_x) || !isFinite(p.pos_y)) return;
      var world = { x: p.pos_x, y: p.pos_y };
      [p.point_name, p.label, p.instance_name].forEach(function (n) {
        if (n) nodeIndex[String(n).toLowerCase()] = world;
      });
      var cls = classOf(p);
      if (cls === 'ChargePoint' || cls === 'ParkPoint') {
        bays.push({ x: p.pos_x, y: p.pos_y, kind: cls === 'ChargePoint' ? 'charge' : 'park' });
      }
    });
    buildClassColors();
    buildGraph();
  }
  function findNode(name) {
    if (!name) return null;
    return nodeIndex[String(name).toLowerCase()] || null;
  }

  // ── the scene's network ────────────────────────────────────────────
  // The network is exactly the drivable path segments synced from the fleet
  // (SEER advanced curves, GET /api/map/edges), adapted once per load to the
  // shared scene shape (sceneOfRows) and read by shared/scene-net.js: its
  // vertices, its rows by the direction a robot may drive them, its lanes.
  // Endpoints with a name but no synced scene point are orphans — "missing"
  // points that still join the network via the curve's own coordinates, and
  // that still count toward the plant's extent (computeView).
  // Deliberately NO proximity-derived fallback: a guessed graph draws
  // plausible-but-wrong aisles (through walls); honest emptiness is better.
  var scene = sceneOfRows([]); // sceneEdges as the shared scene shape (sceneOfRows)
  var orphans = [];         // [{x, y}]: the vertices no scene point names, world
  var graphScale = 0;       // median row length (scene-net's scale, world units);
                            // also the local scale that drives marker/label sizing
  var sceneRev = 0;         // bumped by every buildGraph, which every scene load
                            // runs (loadPoints via buildNodeIndex, loadEdges);
                            // keys the minimap's static layer (syncMinimap)
  // pathTally.misses counts the robot paths this page could not draw because
  // a hop has no stored row in its direction (scene-path.js pathWorld), once
  // per robot frame. Read by the render pins.
  var pathTally = { misses: 0 };

  function isTravel(cls) { return cls === 'LocationMark' || cls === 'GeneralLocation'; }

  // sceneOfRows is GET /api/map/edges as the shape every shared scene module
  // reads (shared/scene-net.js: ComposerMap's): points name -> {x, y, class},
  // edges [{from, to, h?}], one edge per stored row with its handles, all four
  // or none. Built once per load, by buildGraph.
  //
  // A point is a row's END: its place is where the first row naming it puts it
  // (a scene point by the same name is not consulted, as the graph never has),
  // and two spellings of one name are one point, spelled as first seen,
  // because this map has always matched names without case. A row end with no
  // name is a point named by its place, '@x,y', and marked anon. A row with a
  // coordinate that is no number is left out whole.
  function sceneOfRows(rows) {
    var points = {}, edges = [], byKey = {};
    function end(name, x, y) {
      var k = name ? String(name).toLowerCase() : '@' + x + ',' + y;
      if (byKey[k] === undefined) {
        byKey[k] = name ? String(name) : k;
        points[byKey[k]] = name ? { x: +x, y: +y, class: '' } : { x: +x, y: +y, class: '', anon: true };
      }
      return byKey[k];
    }
    rows.forEach(function (e) {
      if (!isFinite(e.from_x) || !isFinite(e.from_y) || !isFinite(e.to_x) || !isFinite(e.to_y)) return;
      var edge = { from: end(e.from_name, e.from_x, e.from_y), to: end(e.to_name, e.to_x, e.to_y) };
      if (isCoord(e.ctrl1_x) && isCoord(e.ctrl1_y) && isCoord(e.ctrl2_x) && isCoord(e.ctrl2_y)) {
        edge.h = [e.ctrl1_x, e.ctrl1_y, e.ctrl2_x, e.ctrl2_y];
      }
      edges.push(edge);
    });
    return { points: points, edges: edges };
  }

  // buildGraph reads the network again from sceneEdges, and the orphans
  // against the scene points loaded so far (nodeIndex): both loads run it.
  function buildGraph() {
    scene = sceneOfRows(sceneEdges);
    sceneRev++;
    var net = sceneNet(scene);
    graphScale = net.scale;
    orphans = [];
    net.names.forEach(function (n) {
      if (!scene.points[n].anon && !nodeIndex[n.toLowerCase()]) orphans.push(net.at[n]);
    });
  }

  // robotRoute is robot r's route, world [[x, y], ...]: the path RDS planned
  // for it (r.path, current point first) drawn along the stored rows from the
  // robot's projection onto its first hop (shared/scene-path.js pathWorld,
  // lead decisions D6 and D10). null when it has no path, is at its last
  // point, or a hop has no stored row in its direction (counted in
  // pathTally): then the robot draws no route at all. Worked out once per
  // robot frame and scene, as computeView and render both ask: each frame is
  // a new robot object (mergeRobot), and each scene load bumps sceneRev.
  function robotRoute(r) {
    if (r.routeRev !== sceneRev) {
      r.routeRev = sceneRev;
      r.route = pathWorld(scene, r, r.path, pathTally);
    }
    return r.route;
  }

  // ── leg-aware routing helpers ──────────────────────────────────────
  // orderLeg infers whether the robot is FETCHING (heading to source, empty) or
  // CARRYING (heading to delivery, loaded). Per the pickup-latch note at the top
  // of the module: no payload field or protocol status distinguishes the legs,
  // so we latch on robot↔source proximity — once the robot reaches source it is
  // carrying for the rest of the order. This fixes the reversed-arrow bug where
  // the comet always ran robot→delivery, i.e. opposite the AMR's motion on the
  // pickup leg.
  function orderLeg(o, r, src) {
    // Authoritative physical signal first: the board carries `carrying` from the
    // robot's lift/jack state (BoardOrder.Carrying ← LiftHeight). A loaded robot is on
    // its carry leg wherever it is — this fixes the stale "arrow back to the supermarket"
    // on multi-leg/complex orders, where the single source_node + geometry never latched.
    // carrying false/absent falls through to the geometric inference below (covers
    // non-lift robots and older Core that doesn't send the field).
    if (o.carrying === true) return 'carry';
    if (!src) return 'carry';                        // no source known → carrying
    if (pickedUpByOrder[o.order_id]) return 'carry'; // latched at source earlier
    var pr = (graphScale > 0 ? graphScale : 1) * PICKUP_RADIUS;
    var dxs = r.x - src.x, dys = r.y - src.y;
    if (dxs * dxs + dys * dys <= pr * pr) {           // reached source → latch carry
      pickedUpByOrder[o.order_id] = true;
      return 'carry';
    }
    // Not latched, not at source: if the robot is already closer to delivery than
    // to source it has likely picked up (map opened mid-carry) — treat as carry
    // WITHOUT latching, so if it's actually still approaching source the next
    // ticks self-correct back to fetch as the source distance shrinks.
    var dst = findNode(o.delivery_node);
    if (dst) {
      var dxd = r.x - dst.x, dyd = r.y - dst.y;
      if (dxd * dxd + dyd * dyd < dxs * dxs + dys * dys) return 'carry';
    }
    return 'fetch';
  }

  // intentPts is an intent line, world→screen: order o's source place, the
  // route model's way from its source to its delivery along the stored rows
  // (shared/scene-net.js sceneRoute, lead decision D9: directed, weighed by
  // the distance driven), its delivery place. With no way there, a straight
  // two-point hint: an intent line is not a robot's route, so the hint stays.
  // An order node named as a scene point is that point; any other is its
  // place on the map (src, dst).
  function intentPts(o, src, dst) {
    var net = sceneNet(scene);
    var a = net.resolve(String(o.source_node || '')) || src;
    var b = net.resolve(String(o.delivery_node || '')) || dst;
    var seq = sceneRoute(scene, a, b);
    var w = seq ? hopsWorld(scene, seq) : null;
    var s = proj(src.x, src.y), d = proj(dst.x, dst.y);
    if (!w) return [s, d];
    return [s].concat(w.map(function (p) { return proj(p[0], p[1]); }), [d]);
  }

  // laneVisible: a working robot's route draws as a static lane only where a
  // comet cannot carry it — the order is blocked/faulted (the lane says where it
  // stopped), or prefers-reduced-motion suppresses comets (the lane IS the
  // route). A robot that has simply stood still draws none, however long: the
  // lane once drawn for a robot still past 45 s read as a highlighted route
  // across the map and was removed (HOP, 2026-11-12). The robot glyph and the lit
  // destination node still say where its order is going.
  function laneVisible(blocked, reduceMotion) {
    return blocked || reduceMotion;
  }

  // ── node classes (e.g. advanced/action points vs bin locations) ────
  // Node-class colours read from the shared --map-class-* tokens (the hex is a
  // defensive fallback, matching the other cssVar() reads in this file).
  var CLASS_PALETTE = ['#6cb0ff', '#56d364', '#e3b341', '#d2a8ff', '#ff9b72', '#79c0ff', '#f0883e']
    .map(function (h, i) { return cssVar('--map-class-' + (i + 1), h); });
  var classColors = {};
  function classOf(p) { return String(p.class_name || 'node'); }
  function buildClassColors() {
    var names = {};
    points.forEach(function (p) { names[classOf(p)] = true; });
    var sorted = Object.keys(names).sort();
    classColors = {};
    sorted.forEach(function (n, i) { classColors[n] = CLASS_PALETTE[i % CLASS_PALETTE.length]; });
  }
  function prettyClass(n) {
    return n.replace(/[_-]+/g, ' ').replace(/([a-z])([A-Z])/g, '$1 $2')
      .replace(/\b\w/g, function (c) { return c.toUpperCase(); });
  }

  // ── minimap (persistent small overview, bottom-right of .map-region) ──
  // The minimap SVG has a FIXED viewBox = the full plant. Its static layer, the
  // travel network and the scene's point dots, is built once per scene, turn
  // and full-plant view: syncMinimap keys it on all three (minimapBuiltFor)
  // and builds it only when the key changes. Every other render, like tickEase
  // and the manual view controls, only refreshes the robot dots and the
  // viewport rect (updateMinimapDynamic). The container's pixel size is not in
  // the key: the viewBox scales the layer, so a resize draws nothing new.
  // Pinned by the render pin theMinimap.
  var minimapBuiltFor = '';

  function syncMinimap() {
    if (!fullPlantView) return;
    var region = document.querySelector('.map-region');
    if (!region) return;
    if (!minimapEl) {
      // Clickable: click-to-jump centers the main view on the clicked spot.
      // A click on a dot or the viewport rect inside bubbles up to this
      // listener, and jumpToWorld reads the point off the minimap's own CTM,
      // so a click anywhere on it jumps there.
      minimapEl = svgNode('svg', { class: 'map-minimap' });
      minimapEl.addEventListener('click', function (e) { jumpToWorld(e.clientX, e.clientY); });
      region.appendChild(minimapEl);
    }
    var fp = fullPlantView;
    var builtFor = sceneRev + '|' + turnDeg + '|' + fp.minX + ' ' + fp.minY + ' ' + fp.w + ' ' + fp.h;
    if (builtFor === minimapBuiltFor) { updateMinimapDynamic(); return; }
    minimapBuiltFor = builtFor;
    minimapEl.setAttribute('viewBox', fp.minX + ' ' + fp.minY + ' ' + fp.w + ' ' + fp.h);
    minimapEl.setAttribute('preserveAspectRatio', 'xMidYMid meet');
    while (minimapEl.firstChild) minimapEl.removeChild(minimapEl.firstChild);

    // Static layer: travel network + all scene point dots
    var netG = svgNode('g', { opacity: '0.3' });
    var sw = (graphScale > 0 ? graphScale : Math.max(fp.w, fp.h) * 0.01) * 0.06;
    // Each physical lane once, as the aisles draw it, but as a chord.
    sceneNet(scene).lanes.forEach(function (lane) {
      var mpa = proj(lane.a.x, lane.a.y);
      var mpb = proj(lane.b.x, lane.b.y);
      netG.appendChild(svgNode('line', {
        x1: mpa[0], y1: mpa[1], x2: mpb[0], y2: mpb[1],
        stroke: cssVar('--map-aisle', '#45566e'), 'stroke-width': sw
      }));
    });
    var dotR = sw * 1.2;
    points.forEach(function (p) {
      if (!isFinite(p.pos_x) || !isFinite(p.pos_y)) return;
      var ms = proj(p.pos_x, p.pos_y);
      netG.appendChild(svgNode('circle', { cx: ms[0], cy: ms[1], r: dotR, fill: cssVar('--map-node', '#66768f') }));
    });
    minimapEl.appendChild(netG);

    // Dynamic: robot dots group + viewport indicator rect
    minimapRobotGroup = svgNode('g');
    minimapEl.appendChild(minimapRobotGroup);
    var vbSw = sw * 1.8;
    minimapViewportRect = svgNode('rect', {
      fill: 'none', stroke: cssVar('--map-aisle', '#45566e'),
      'stroke-width': vbSw, rx: vbSw * 1.5
    });
    minimapEl.appendChild(minimapViewportRect);

    updateMinimapDynamic();
  }

  // Called from syncMinimap on every render (robot dots + rect), and from
  // tickEase, startEase and applyManualView as the view moves.
  function updateMinimapDynamic() {
    if (!minimapEl || !minimapViewportRect) return;
    // Hide the minimap at full-plant view (idle); it adds no context when the
    // detail view IS the whole plant. Show it once active orders push a zoom.
    var INACTIVE_MM = { delivered: true, confirmed: true, cancelled: true };
    minimapEl.style.display = orders.some(function (o) { return !INACTIVE_MM[o.status]; }) ? '' : 'none';
    // Refresh robot dots
    if (minimapRobotGroup) {
      while (minimapRobotGroup.firstChild) minimapRobotGroup.removeChild(minimapRobotGroup.firstChild);
      var dr = (graphScale > 0 ? graphScale : 0.5) * 0.22;
      Object.keys(robots).forEach(function (k) {
        var r = robots[k];
        if (!isFinite(r.x) || !isFinite(r.y)) return;
        var ord = orderByRobot[r.id];
        var moving = r.state === 'busy' || isMoving(r);
        var ms2 = proj(r.x, r.y);
        minimapRobotGroup.appendChild(svgNode('circle', {
          cx: ms2[0], cy: ms2[1], r: dr, fill: robotColor(r, ord, moving)
        }));
      });
    }
    // Refresh viewport rect
    if (view) {
      minimapViewportRect.setAttribute('x', view.minX);
      minimapViewportRect.setAttribute('y', view.minY);
      minimapViewportRect.setAttribute('width', view.w);
      minimapViewportRect.setAttribute('height', view.h);
      minimapViewportRect.removeAttribute('visibility');
    } else {
      minimapViewportRect.setAttribute('visibility', 'hidden');
    }
  }

  // ── render (coalesced via rAF) ─────────────────────────────────────
  var dirty = false;
  function scheduleRender() {
    if (dirty) return;
    dirty = true;
    requestAnimationFrame(function () { dirty = false; render(); });
  }

  // What the page asks the scene view to draw, and in what colours.
  var sceneView = null;
  var SCENE_LOOK = { flare: STATUS_COLOR.blocked || STATE_COLOR.error, accent: ACCENT, accentGlow: ACCENT_GLOW };

  // nodeFacts lists, in point order, the scene points the nodes layer draws,
  // as drawNodeGlyph takes them. Travel points draw nothing: they are path
  // structure, and the aisle lines carry the network. AN EMPTY BAY IS NOT
  // DRAWN (HOP, 2026-11-12): a plant's rows of chargers and park spots were
  // most of the marks on the map and said nothing until a robot stood on one.
  // An occupied bay draws under its robot; a bay an order names draws as any
  // hot node. Called by the scene view only when render's nodesKey changes.
  function nodeFacts() {
    var out = [];
    points.forEach(function (p) {
      if (!isFinite(p.pos_x) || !isFinite(p.pos_y)) return;
      var cls = classOf(p);
      if (isTravel(cls)) return;
      var wx = p.pos_x, wy = p.pos_y;
      // Action points snap onto the nearest network vertex when their scene
      // coordinate sits a hair off the curve endpoint they serve — otherwise
      // the ring floats beside its node with its own dot, which reads sloppy.
      // Half a typical edge length is close enough to mean "same node".
      if (cls === 'ActionPoint' && graphScale > 0) {
        var nv = nearestVertex(scene, wx, wy);
        if (nv) {
          var at = sceneNet(scene).at[nv];
          var nd = Math.sqrt(dist2(at, { x: wx, y: wy }));
          if (nd > 0 && nd <= graphScale * 0.5) { wx = at.x; wy = at.y; }
        }
      }
      var hot = !!(hotNodes[String(p.point_name || '').toLowerCase()] ||
        hotNodes[String(p.label || '').toLowerCase()] ||
        hotNodes[String(p.instance_name || '').toLowerCase()]);
      if ((cls === 'ChargePoint' || cls === 'ParkPoint') && !hot && !bayOccupied(wx, wy)) return;
      var n = { x: wx, y: wy, hot: hot };
      if (cls === 'ActionPoint') { n.kind = 'action'; n.fill = NODE_ACTION_COLOR; }
      else if (cls === 'ChargePoint') { n.kind = 'charge'; n.ring = CHARGE_RING; n.fill = cssVar('--map-bay-glyph', '#6b7a90'); }
      else if (cls === 'ParkPoint') { n.kind = 'park'; n.fill = cssVar('--map-node', '#7e92b3'); }
      else { n.kind = 'class'; n.fill = classColors[cls] || '#67748f'; }
      out.push(n);
    });
    return out;
  }

  // hotKey and occupiedKey are the parts of the nodes layer that change
  // without the scene changing: which node names an order holds (hotNodes,
  // set by loadOrders), and which bays a robot stands on (bayOccupied, by
  // bay, in point order).
  function hotKey() {
    return Object.keys(hotNodes).filter(function (k) { return hotNodes[k]; }).sort().join(',');
  }
  function occupiedKey() {
    return bays.map(function (b) { return bayOccupied(b.x, b.y) ? '1' : '0'; }).join('');
  }

  // robotFact is what robotSpec and chipSpecs draw for one robot.
  function robotFact(r, nodeR) {
    var ord = orderByRobot[r.id];
    var moving = r.state === 'busy' || isMoving(r);
    var alert = r.state === 'error';
    // Name chips only where they carry signal: on an order, physically
    // moving, or faulted. Stationary paused/offline robots keep their grey
    // disc but no tag — the cluster stays calm.
    var chip = !!ord || alert || isMoving(r);
    return {
      id: r.id, x: r.x, y: r.y, state: r.state,
      color: robotColor(r, ord, moving),
      moving: moving, alert: alert,
      fault: alert || !!(ord && (ord.status === 'blocked' || ord.status === 'faulted')),
      focused: !!focusRobot && r.id === focusRobot,
      rot: moving ? headingDeg(r.angle, turnDeg) : null,
      rest: moving ? null : restDisc(r, nodeR),
      chip: chip,
      chipColor: chip ? robotColor(r, ord, r.state === 'busy' || !!ord || isMoving(r)) : null
    };
  }

  function render() {
    var host = document.getElementById('map-svg-wrap');
    var empty = document.getElementById('map-empty');
    if (!host) return;
    // Click-to-focus: clicking a robot (its glyph or name chip) focuses it:
    // its comet and intent line stay, every other robot's comet and intent
    // line go, and their frozen lanes dim. Clicking it again, or clicking
    // empty floor, clears the focus. Delegated once, on the host: the handler
    // walks up from whatever was hit to the nearest [data-robot] group.
    if (!clickBound) {
      clickBound = true;
      host.addEventListener('click', function (ev) {
        // A click that ends a drag-pan must not also focus a robot.
        if (suppressClick) { suppressClick = false; return; }
        var t = ev.target;
        while (t && t !== host && !(t.getAttribute && t.getAttribute('data-robot'))) t = t.parentNode;
        var id = (t && t.getAttribute) ? t.getAttribute('data-robot') : null;
        focusRobot = (id && id === focusRobot) ? null : (id || null);
        scheduleRender();
      });
    }
    if (!sceneView) sceneView = createSceneView(host, SCENE_LOOK);
    computeView();
    if (!view) {
      sceneView.clear();
      comets.clear();
      if (empty) empty.style.display = points.length ? 'none' : 'block';
      return;
    }
    if (empty) empty.style.display = 'none';

    var unit = Math.max(view.w, view.h);
    // Local-scale sizing: proportion markers to the median waypoint gap
    // (clamped against plant extent) so robots fit the dense cells they
    // cluster in instead of dominating them. Sizing off the full plant extent
    // made everything huge on a floor that is two tight cells + a long
    // corridor.
    var base = graphScale || unit * 0.03;
    var robotR = Math.max(unit * 0.004, Math.min(unit * 0.010, base * 0.9));
    var nodeR = Math.max(unit * 0.0024, Math.min(unit * 0.006, base * 0.3));
    var fontS = Math.max(unit * 0.006, Math.min(unit * 0.0085, base * 0.8));

    // ── routes ────────────────────────────────────────────────────────
    // Three honest visual states, so the map shows what's REALLY happening:
    //  • Moving, working order → an animated comet along the path RDS planned
    //    for the robot (robotRoute, lead decision D10). Fetch legs draw
    //    hollow/empty dots, carry legs solid/loaded (orderLeg), so
    //    loaded-vs-empty reads at a glance. The comet lives on a PERSISTENT
    //    overlay (comets.sync below) so it glides continuously instead of
    //    restarting each SSE tick.
    //  • Blocked/faulted, or reduced-motion → a faint STATIC lane along that
    //    same path, no flow (see laneVisible). Motion means motion: a
    //    stationary robot never animates, and a robot standing still draws no
    //    line at all.
    //  • Pre-dispatch / staged → a dashed INTENT line source→delivery at most,
    //    along the route model's way (intentPts); nothing is driving that leg
    //    yet.
    // A robot with no path RDS planned, or one this scene cannot draw, draws
    // no comet and no lane: no estimate stands in for it. Its glyph, focus
    // ring and lit destination are drawn as ever.
    var reduceMotion = !!(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches);
    var MOVING_PHASE = { dispatched: 1, in_transit: 1, reshuffling: 1 };
    var INTENT_PHASE = { pending: 1, queued: 1, acknowledged: 1, submitted: 1, staged: 1 };

    // Prune pickup latches for orders that have left the active set.
    var liveOrderIds = {};
    orders.forEach(function (o) { liveOrderIds[o.order_id] = 1; });
    Object.keys(pickedUpByOrder).forEach(function (id) { if (!liveOrderIds[id]) delete pickedUpByOrder[id]; });

    // Count the moving orders first, so the busy-floor calm (more than
    // COMET_LIMIT moving orders → no comets and no intent lines, just robots,
    // unless one is focused) keys off real motion: an order moving with a
    // robot seen moving, whether or not its path can be drawn (lead decision
    // D10 keeps this count as it was).
    var movingComets = 0;
    orders.forEach(function (o) {
      if (!MOVING_PHASE[o.status]) return;
      var rr = robots[o.robot_id];
      if (rr && isFinite(rr.x) && isFinite(rr.y) && isMoving(rr)) movingComets++;
    });
    var quiet = movingComets <= COMET_LIMIT;

    var cometRoutes = [];
    var routeLines = []; // the scene view's routes layer, in order
    orders.forEach(function (o) {
      if (o.status === 'delivered' || o.status === 'confirmed' || o.status === 'cancelled') return;
      var focused = !!focusRobot && o.robot_id === focusRobot;
      var dimmed = !!focusRobot && !focused;
      var calmOK = focusRobot ? focused : quiet;
      var src = findNode(o.source_node);
      var dst = findNode(o.delivery_node);

      // Pre-dispatch / staged: a dashed intent line at most (no comet, no robot
      // needed). Calmed like the comets — only when quiet or focused.
      if (INTENT_PHASE[o.status]) {
        if (src && dst && calmOK) routeLines.push({ key: 'intent:' + o.order_id, kind: 'intent', pts: intentPts(o, src, dst), dimmed: dimmed });
        return;
      }

      // Actively worked (dispatched/in_transit/reshuffling) or blocked/faulted —
      // needs a robot with a known position.
      var r = robots[o.robot_id];
      if (!r || !isFinite(r.x) || !isFinite(r.y)) return;

      var blocked = (o.status === 'blocked' || o.status === 'faulted');
      var leg = orderLeg(o, r, src);
      var color = STATUS_COLOR[o.status] || '#888';
      // The route is RDS's path and nothing else: it neither needs nor ends
      // at the order's nodes.
      var route = robotRoute(r);
      if (!route) return;
      var pts = route.map(function (p) { return proj(p[0], p[1]); });

      var wantComet = !reduceMotion && !blocked && isMoving(r) && calmOK;
      if (wantComet) {
        cometRoutes.push({
          pts: pts, color: color, leg: leg,
          // membership/rebuild key (NOT geometry) — includes the leg so a
          // fetch→carry flip rebuilds the dots (hollow→solid).
          sig: o.order_id + '>' + leg + '>' + o.status
        });
      } else {
        // A static lane only where it carries signal (see laneVisible). The block
        // itself is shown by the RED ROBOT glyph, so a blocked robot's lane draws NEUTRAL
        // grey (IDLE_MOVE_COLOR), not a loud red alert line.
        if (laneVisible(blocked, reduceMotion)) {
          routeLines.push({ key: 'lane:' + o.order_id, kind: 'frozen', pts: pts, color: blocked ? IDLE_MOVE_COLOR : color, dimmed: dimmed });
        }
      }
      // Destination is marked by the node glyph's indigo accent (nodeFacts/hotNodes).
    });
    // Deterministic order so the comet SET signature is order-independent — a
    // server reshuffle of `orders` doesn't force a needless rebuild, and index →
    // overlay path id stays stable for in-place geometry updates.
    cometRoutes.sort(function (a, b) { return a.sig < b.sig ? -1 : a.sig > b.sig ? 1 : 0; });

    // The grid and the aisles show the view, the turn (proj is
    // turnedProjector(turnDeg)) and the scene (sceneRev, bumped by every
    // scene load); nodeR follows from the view and the scene. The view is
    // spelled exactly: in steady state it is the same object render to render,
    // and a rounded key would leave the grid and the marker sizes from a view
    // up to the rounding away.
    var baseKey = view.minX + ' ' + view.minY + ' ' + view.w + ' ' + view.h + '|' + turnDeg + '|' + sceneRev;
    var robotList = Object.keys(robots).map(function (k) { return robots[k]; })
      .filter(function (r) { return isFinite(r.x) && isFinite(r.y); });
    sceneView.draw({
      view: view, proj: proj, unit: unit, nodeR: nodeR, robotR: robotR, fontS: fontS,
      baseKey: baseKey,
      lanes: sceneNet(scene).lanes,
      // The nodes show all of that, plus which nodes an order names and which
      // bays a robot stands on.
      nodesKey: baseKey + '|' + hotKey() + '|' + occupiedKey(),
      nodes: nodeFacts,
      routes: routeLines,
      robots: robotList.map(function (r) { return robotFact(r, nodeR); })
    });
    comets.sync(cometRoutes, robotR, view);
    renderClassLegend();
    renderLegend(); // live fleet counts track every robot/order tick
    renderRail();
    syncMinimap(); // robot dots + viewport rect; the network only when the scene, turn or plant changed
  }

  // Class legend mirrors the actual node encoding: travel dots recede, typed
  // waypoints are outlined shapes. Unknown classes fall back to palette dots.
  function legendSwatch(color, shape, label) {
    var style = 'background:' + color;
    if (shape === 'ring') style = 'background:transparent;border:2px solid ' + color;
    if (shape === 'square') style = 'background:transparent;border:2px solid ' + color + ';border-radius:3px';
    return '<span class="map-legend-item"><span class="map-legend-dot" style="' + style + '"></span>' +
      escapeHtml(label) + '</span>';
  }

  function renderClassLegend() {
    var el = document.getElementById('map-class-legend');
    if (!el) return;
    var have = {};
    points.forEach(function (p) { have[classOf(p)] = true; });
    var items = [];
    if (have.LocationMark || have.GeneralLocation) items.push(legendSwatch(cssVar('--map-node', '#7e92b3'), 'dot', 'Travel node'));
    if (have.ActionPoint) items.push(legendSwatch(NODE_ACTION_COLOR, 'dot', 'Action point'));
    if (have.ChargePoint) items.push(legendSwatch(CHARGE_RING, 'ring', 'Charge point'));
    if (have.ParkPoint) items.push(legendSwatch(cssVar('--map-node', '#7e92b3'), 'dot', 'Park point'));
    Object.keys(have).sort().forEach(function (n) {
      if (n === 'LocationMark' || n === 'GeneralLocation' || n === 'ActionPoint' ||
          n === 'ChargePoint' || n === 'ParkPoint') return;
      items.push(legendSwatch(classColors[n] || '#67748f', 'dot', prettyClass(n)));
    });
    if (!items.length) { writeHTML(el, ''); return; }
    // The node-type key is reference an operator learns once, so it lives behind
    // a small collapsible toggle (default closed) instead of a permanent second
    // legend strip competing with the live status counts in the header.
    var html =
      '<button type="button" class="map-key-toggle" aria-expanded="' + mapKeyOpen + '">' +
        (mapKeyOpen ? '▾' : '▸') + ' Map key</button>' +
      '<div class="map-key-items"' + (mapKeyOpen ? '' : ' hidden') + '>' + items.join('') + '</div>';
    // Unchanged: the button on screen is the one already listened to.
    if (!writeHTML(el, html)) return;
    var btn = el.querySelector('.map-key-toggle');
    if (btn) btn.addEventListener('click', function () { mapKeyOpen = !mapKeyOpen; renderClassLegend(); });
  }

  // ── the fault alarm, in a few words ─────────────────────────────────
  //
  // Core sends the one alarm that began with a robot's fault (sse.go
  // faultAlarm), as the fleet words it: SEER's sentence, sometimes a format
  // string with its arguments ('["Do not find path to {1}","CP68"]'). The map
  // says it in a few words a person on the floor reads at a glance. The table
  // is the codes HOP raised in the 30 days to 2026-11-12, by count; a code it
  // does not hold is said as the fleet said it, shortened.
  var ALARM_WORDS = {
    50401: 'battery dead, shutting down',
    52503: 'battery too low',
    52200: 'blocked',
    54231: 'obstacle ahead',
    54004: 'e-stop pressed',
    54025: 'motors off',
    52313: 'wheel slip',
    54313: 'wheel slip',
    54901: 'can’t see the cart',
    52952: 'can’t recognise the cart',
    54020: 'lost position',
    54019: 'few reflectors seen',
    52600: 'position uncertain',
    52101: 'laser fault',
    52094: 'laser fault',
    52102: 'laser fault',
    52103: 'laser fault',
    54040: 'laser fault',
    52106: 'odometry lost',
    52111: 'motor fault',
    52135: 'motor fault',
    52202: 'charging failed',
    52702: 'no path',
    52700: 'no path to {1}',
    52958: 'controller fault'
  };
  var ALARM_TEXT_MAX = 40;

  // seerArgs reads a fleet alarm's text: plain, or a format string and its
  // arguments as a JSON array. Returns {text, args}.
  function seerArgs(desc) {
    var d = String(desc || '').trim();
    if (d.charAt(0) === '[') {
      try {
        var a = JSON.parse(d);
        if (Array.isArray(a) && typeof a[0] === 'string') return { text: a[0], args: a };
      } catch (e) { /* not an array after all: plain text */ }
    }
    return { text: d, args: [] };
  }

  function fillArgs(fmt, args) {
    return String(fmt).replace(/\{(\d+)\}/g, function (m, i) {
      var v = args[Number(i)];
      if (v === undefined) return m;
      return typeof v === 'number' && !Number.isInteger(v) ? v.toFixed(2) : String(v);
    });
  }

  // alarmWords is a fault alarm in a few words, or '' for none.
  function alarmWords(fa) {
    if (!fa) return '';
    var seer = seerArgs(fa.desc);
    var words = ALARM_WORDS[fa.code];
    if (words) return fillArgs(words, seer.args);
    var text = fillArgs(seer.text, seer.args).replace(/[\s,;:.]+$/, '');
    if (!text) return 'alarm ' + fa.code;
    return text.length > ALARM_TEXT_MAX ? text.slice(0, ALARM_TEXT_MAX - 1).replace(/\s+\S*$/, '') + '…' : text;
  }

  // ── status rail + activity feed ────────────────────────────────────

  function pushFeedEvent(text, level, extra) {
    var e = Object.assign({ text: text, level: level || 'info', ts: Date.now() }, extra || {});
    activityFeed.unshift(e);
    if (activityFeed.length > 30) activityFeed.length = 30;
    return e;
  }

  // faultWordsFor: the few words for the alarm that began with this order's
  // robot's fault, '' while Core has named none.
  function faultWordsFor(o) {
    var r = o && robots[o.robot_id];
    return r ? alarmWords(r.faultAlarm) : '';
  }

  // A FAULT'S LINE GAINS ITS WORDS WHEN THEY ARRIVE. Core names the alarm a
  // poll or two after the order faults, so a line pushed at the transition
  // without them takes them the first time they are known.
  function fillFaultWords() {
    activityFeed.forEach(function (e) {
      if (!e.faultOrder || e.worded) return;
      var o = orders.find(function (x) { return String(x.order_id) === String(e.faultOrder); });
      var words = faultWordsFor(o);
      if (words) { e.text = e.base + ' · ' + words; e.worded = true; }
    });
  }

  // Called with each fresh orders array BEFORE `orders` is updated, so we
  // can detect transitions from the previous snapshot.
  // delivery_node is the consumer (destination that called for parts).
  function diffAndUpdateOrders(newOrders) {
    var newMap = {};
    newOrders.forEach(function (o) {
      if (o.order_id == null) return;
      var key = String(o.order_id);
      newMap[key] = o;
      var prev = prevOrderMap[key];
      if (!prev) {
        if (o.delivery_node) pushFeedEvent(o.delivery_node + ' called for parts');
      } else if (prev.status !== o.status) {
        var st = o.status;
        if (st === 'dispatched' || st === 'in_transit') {
          pushFeedEvent((o.robot_id || '?') + ' responding');
        } else if (st === 'staged') {
          pushFeedEvent((o.robot_id || '?') + ' staged · ' + (o.delivery_node || '?'));
        } else if (st === 'delivered' || st === 'confirmed') {
          pushFeedEvent('Delivered to ' + (o.delivery_node || '?'));
        } else if (st === 'blocked' || st === 'faulted') {
          // Who, what, where, and why when Core knows: "AMR-12 faulted at
          // PLN_04 · obstacle ahead".
          var where = o.current_station || o.delivery_node || o.source_node || '';
          var base = (o.robot_id || '?') + ' ' + st + (where ? ' at ' + where : '');
          var words = faultWordsFor(o);
          pushFeedEvent(base + (words ? ' · ' + words : ''), 'alert',
            { faultOrder: o.order_id, base: base, worded: !!words });
        }
      }
    });
    prevOrderMap = newMap;
  }

  function renderRail() {
    var motionEl = document.getElementById('rail-motion-list');
    var activityEl = document.getElementById('rail-activity-list');
    if (!motionEl || !activityEl) return;

    // ── Active list ───────────────────────────────────────────────────
    // EVERY ROBOT ON AN ORDER, staged ones included (HOP, 2026-11-12). A
    // staged robot is parked at a wait point for a person to release it,
    // which is the robot a person most needs to find on this list; it was
    // left off "In motion" because it is not moving, and so it was nowhere.
    var INACTIVE = { delivered: true, confirmed: true, cancelled: true };
    var active = orders.filter(function (o) { return !INACTIVE[o.status]; });
    active.sort(function (a, b) {
      function rank(o) {
        if (o.status === 'blocked' || o.status === 'faulted') return 0;
        if (o.status === 'staged') return 1;
        if (o.status === 'in_transit') return 2;
        if (o.status === 'dispatched' || o.status === 'queued') return 3;
        return 4;
      }
      return rank(a) - rank(b);
    });
    var overflow = active.length > 8 ? active.length - 8 : 0;
    var shown = active.slice(0, 8);
    var statusLabel = {
      in_transit: 'moving', staged: 'staged', dispatched: 'dispatched',
      blocked: 'BLOCKED', faulted: 'FAULTED', acknowledged: 'waiting',
      queued: 'queued', pending: 'pending', reshuffling: 'reshuffling'
    };
    if (!shown.length) {
      writeHTML(motionEl, '<li class="rail-empty">Nothing active</li>');
    } else {
      writeHTML(motionEl, shown.map(function (o) {
        var color = STATUS_COLOR[o.status] || '#888';
        var label = statusLabel[o.status] || o.status;
        return '<li class="rail-row" style="border-left-color:' + color + '">' +
          '<span class="rail-row-id">' + escapeHtml(o.robot_id || '—') + '</span>' +
          '<span class="rail-row-arrow">→</span>' +
          // No destination yet is no data, not a question mark (guide rule 4).
          (o.delivery_node
            ? '<span class="rail-row-node">' + escapeHtml(o.delivery_node) + '</span>'
            : '<span class="rail-row-node text-muted" title="no destination yet">—</span>') +
          '<span class="rail-row-status" style="color:' + color + '">' + escapeHtml(label) + '</span>' +
          (function () {
            var words = (o.status === 'blocked' || o.status === 'faulted') ? faultWordsFor(o) : '';
            return words ? '<span class="rail-row-why" style="color:' + color + '">' + escapeHtml(words) + '</span>' : '';
          })() +
          '</li>';
      }).join('') + (overflow ? '<li class="rail-empty">+' + overflow + ' more</li>' : ''));
    }

    // ── Activity feed ─────────────────────────────────────────────────
    fillFaultWords();
    var now = Date.now();
    activityFeed = activityFeed.filter(function (e) { return (now - e.ts) < FEED_MAX_AGE_MS; });
    var showing = activityFeed.slice(0, FEED_MAX_ITEMS);
    if (!showing.length) {
      writeHTML(activityEl, '<li class="rail-empty">No recent activity</li>');
    } else {
      writeHTML(activityEl, showing.map(function (e) {
        var ageFrac = (now - e.ts) / FEED_MAX_AGE_MS;
        var opacity = Math.max(0.25, 1 - ageFrac * 0.75).toFixed(2);
        var cls = 'rail-event' + (e.level === 'alert' ? ' rail-event-alert' : '');
        return '<li class="' + cls + '" style="opacity:' + opacity + '">' + escapeHtml(e.text) + '</li>';
      }).join(''));
    }
  }

  function startFeedTimer() {
    if (feedTimer) return;
    feedTimer = setInterval(function () {
      var now = Date.now();
      activityFeed = activityFeed.filter(function (e) { return (now - e.ts) < FEED_MAX_AGE_MS; });
      renderRail();
    }, 10000);
  }

  // ── data loads ─────────────────────────────────────────────────────
  function loadPoints() {
    return fetch('/api/map/points').then(function (r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    }).then(function (data) {
      points = data || [];
      buildNodeIndex();
    });
  }

  function loadEdges() {
    return fetch('/api/map/edges').then(function (r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    }).then(function (data) {
      sceneEdges = data || [];
      // Rebuild — the graph is the edge list. Order with loadPoints is
      // unimportant; buildGraph reads both (points only for orphan flags).
      buildGraph();
    });
  }

  function loadRobots() {
    return fetch('/api/robots').then(function (r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    }).then(function (data) {
      (data || []).forEach(function (raw) {
        var rb = normRobot(raw);
        if (rb.id) mergeRobot(rb);
      });
    });
  }

  function loadOrders() {
    return fetch('/api/board/orders?dashboard=' + encodeURIComponent(dashboardId)).then(function (r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    }).then(function (data) {
      diffAndUpdateOrders(data || []); // diff against snapshot BEFORE updating `orders`
      orders = data || [];
      orderByRobot = {};
      hotNodes = {};
      var INACTIVE_HOT = { delivered: true, confirmed: true, cancelled: true };
      orders.forEach(function (o) {
        if (o.robot_id) orderByRobot[o.robot_id] = o;
        if (INACTIVE_HOT[o.status]) return;
        if (o.source_node) hotNodes[String(o.source_node).toLowerCase()] = o.status;
        if (o.delivery_node) hotNodes[String(o.delivery_node).toLowerCase()] = o.status;
      });
    });
  }

  var orderTimer = null;
  function scheduleOrderReload() {
    clearTimeout(orderTimer);
    orderTimer = setTimeout(function () { loadOrders().then(scheduleRender).catch(noop); }, 250);
  }
  function noop() {}

  // ── SSE via the shared onSSE bus (Q-020): ONE EventSource per tab. The bus
  // owns connection, reconnect/backoff, and build-id detection;
  // setSSEReloadOnBuild(true) reloads the kiosk on a build change.

  // robot-update handler — receives the already-parsed payload from the bus.
  function onRobotUpdate(list) {
    if (!Array.isArray(list)) list = list ? [list] : [];
    list.forEach(function (raw) {
      var rb = normRobot(raw);
      if (rb.id) mergeRobot(rb);
    });
    scheduleRender();
  }

  function refreshAll() {
    Promise.all([loadPoints().catch(noop), loadEdges().catch(noop), loadRobots().catch(noop), loadOrders().catch(noop)])
      .then(scheduleRender);
  }

  function init() {
    renderLegend();
    wireViewControls(); // wheel-zoom + drag-pan + recenter on the map host
    comets.wirePause(); // comet loop off while the tab is hidden or the map off screen
    startFeedTimer(); // starts the 10s interval that ages/expires feed events
    // Initial paint from REST so the board isn't blank before the first SSE tick.
    refreshAll();
    setSSEReloadOnBuild(true);
    // 'connected' re-fires on every (re)connect — refetch covers data missed
    // while down. 'disconnected' drives the offline dot.
    onSSE('connected', function () { setConnected(true); refreshAll(); });
    onSSE('disconnected', function () { setConnected(false); });
    onSSE('robot-update', onRobotUpdate);
    onSSE('order-update', scheduleOrderReload);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
