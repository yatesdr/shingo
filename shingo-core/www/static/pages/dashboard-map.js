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
// rest. Charge/park bays recede to quiet outlines; they light up only when a robot docks.
//
// Routes follow the aisles. The travel network is the scene's real
// connectivity — drivable path segments (SEER advanced curves) served by
// /api/map/edges — and each robot→destination route is the lit shortest path
// along it. Nothing is derived from point proximity (a derived graph invented
// links through walls); with no synced edges the network is simply empty and
// routes fall back to a straight robot→destination hint line.

import { onSSE, setSSEReloadOnBuild } from '/static/shared/utils.js';

(function () {
  var body = document.body;
  var dashboardId = body.getAttribute('data-dashboard-id');
  var SVGNS = 'http://www.w3.org/2000/svg';

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
    dispatched: cssVar('--status-dispatched-dot', '#4f9bff'),
    blocked: cssVar('--status-blocked-dot', '#f85149'),
    acknowledged: cssVar('--status-pending-dot', '#8b95a5'),
    queued: cssVar('--status-queued-dot', '#7aa2f0'),
    pending: cssVar('--status-pending-dot', '#8b95a5'),
    delivered: cssVar('--status-delivered-dot', '#3fb950')
  };
  // Robot states unchanged (P13): ready green, error red, offline gray; a moving
  // robot with no order falls back to the in-transit cyan.
  var STATE_COLOR = {
    ready: cssVar('--status-delivered-dot', '#3fb950'),
    busy: cssVar('--status-in-transit-dot', '#34c3e0'),
    paused: cssVar('--text-muted', '#8b949e'),
    error: cssVar('--status-blocked-dot', '#f85149'),
    offline: cssVar('--text-tertiary', '#6e7681')
  };
  // Bays as sockets: a robot docked on a charge/park point takes the bay's
  // hue, so an occupied ring reads as a filled socket and an empty ring as an
  // available bay. Amber belongs to charging (a robot state, unchanged by the
  // P13 palette work — staged is now teal in the unified status scheme).
  var DOCK_COLOR = { charge: '#e3b341', park: '#d98c4a' };
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
  var rotate90 = false;     // orient the plant's long axis along screen X
  var focusRobot = null;    // click-to-focus: id of the robot whose route is lit
  var mapKeyOpen = false;   // node-type symbol key collapsed by default (reference info)
  var cometLayer = null;    // persistent SVG overlay holding the route comets
  var lastCometSig = '';    // signature of the last-built comet set (rebuild only on change)
  var cometState = [];      // live comet geometry + dot elements, positioned each frame by rAF
  var cometRAF = 0;         // requestAnimationFrame handle for the comet positioner loop
  var clickBound = false;   // host click handler attached once
  // ── auto-framing state ─────────────────────────────────────────────
  var viewTarget = null;          // desired view after hysteresis; `view` eases toward this
  var fullPlantView = null;       // whole-plant bbox in screen space (minimap fixed viewBox)
  var viewEaseRAF = 0;            // rAF handle for the viewBox ease loop
  var minimapEl = null;           // persistent minimap <svg> inside .map-region
  var minimapViewportRect = null; // viewport indicator rect inside the minimap
  var minimapRobotGroup = null;   // robot-dots <g> inside the minimap
  // Above this many active routes, the ambient view calms to dim lines (no
  // comets) so a busy floor doesn't become 20 arrows fighting for attention;
  // clicking a robot still lights its comet. Tune to taste.
  var COMET_LIMIT = 12;

  // ── activity feed + status rail ────────────────────────────────────
  var activityFeed = [];   // [{text, level, ts}] — newest first
  var prevOrderMap = {};   // order_id -> order snapshot, for transition diffs
  var feedTimer = 0;       // setInterval handle for age-fade re-renders
  var FEED_MAX_AGE_MS = 3 * 60 * 1000; // events older than 3 min are dropped
  var FEED_MAX_ITEMS = 5;

  // proj maps world (x, y) to screen coords. Y is negated (world up -> screen
  // down). When the plant footprint is taller than wide, the whole map rotates
  // 90° CW so its long axis fills a landscape monitor instead of being
  // letterboxed into a thin central strip.
  function proj(x, y) {
    if (rotate90) return [y, x]; // 90° CW of the (x, -y) base image
    return [x, -y];
  }

  // ── header chrome ──────────────────────────────────────────────────
  function tickClock() {
    var el = document.getElementById('dash-clock');
    if (el) el.textContent = new Date().toLocaleTimeString();
  }
  setInterval(tickClock, 1000); tickClock();

  function setConnected(ok) {
    var el = document.getElementById('dash-conn');
    if (el) el.className = 'dash-conn ' + (ok ? 'dash-conn-ok' : 'dash-conn-down');
  }

  // Bucket a robot into one of the header-legend categories so the legend can
  // double as a live fleet summary. Paused/offline robots are uncounted.
  function robotBucket(r) {
    var ord = orderByRobot[r.id];
    if (ord) {
      if (ord.status === 'blocked') return 'blocked';
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
    el.innerHTML = items.filter(function (it) { return counts[it[0]] > 0; }).map(function (it) {
      return '<span class="map-legend-item"><span class="map-legend-dot" style="background:' + it[2] +
        '"></span>' + it[1] + '<span class="map-legend-count">' + counts[it[0]] + '</span></span>';
    }).join('') || '<span class="map-legend-item map-legend-zero">No active robots</span>';
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
      station: r.station || r.CurrentStation || ''
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
  function isMoving(r) {
    return (Date.now() - (r.lastMoveAt || 0)) < MOVE_LINGER_MS;
  }

  // Which bay (charge/park point) a robot is docked on, if any. Robots park
  // dead-on their bay point, so proximity within half a typical edge length
  // is the test; an explicit charging flag (REST payload) wins outright.
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
    if (cometLayer) cometLayer.setAttribute('viewBox', vb);
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

  // Hysteresis: only adopt a new target if the shift is meaningful.
  // Prevents per-SSE-tick robot micro-jitter from nudging the frame constantly.
  function adoptViewTarget(candidate) {
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
    tnodes.forEach(function (t) {
      if (t.orphan) { wx.push(t.x); wy.push(t.y); }
    });
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
    rotate90 = (maxWy - minWy) > (maxWx - minWx);

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
      // Active: include each active order's robot + source + delivery + route.
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
        if (rbt && dn) {
          var rworld = routeWorld(rbt.x, rbt.y, dn);
          if (rworld) {
            rworld.forEach(function (w) {
              var ws = proj(w[0], w[1]); roiSx.push(ws[0]); roiSy.push(ws[1]);
            });
          }
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

  // ── travel graph (the scene's real connectivity) ────────────────────
  // The graph is exactly the drivable path segments synced from the fleet
  // (SEER advanced curves, GET /api/map/edges). Vertices key by endpoint
  // instance name so curves meeting at a point share a vertex; endpoints with
  // a name but no synced scene point are flagged as orphans — "missing"
  // points that still join the network via the curve's own coordinates.
  // Deliberately NO proximity-derived fallback: a guessed graph draws
  // plausible-but-wrong aisles (through walls); honest emptiness is better.
  var tnodes = [];          // [{x, y, orphan}] world coords of graph vertices
  var tadj = [];            // adjacency: tadj[i] = [{n, w}]
  var routeCache = {};      // "s:d" -> array of tnode indices (cleared on rebuild)
  var graphScale = 0;       // median edge length (world units); also the local
                            // scale that drives marker/label sizing

  function isTravel(cls) { return cls === 'LocationMark' || cls === 'GeneralLocation'; }

  function buildGraph() {
    tnodes = []; tadj = []; routeCache = {}; graphScale = 0;
    var byKey = {}, lens = [];
    function vertex(name, x, y) {
      var k = name ? String(name).toLowerCase() : '@' + x + ',' + y;
      if (byKey[k] === undefined) {
        byKey[k] = tnodes.length;
        var orphan = !!name && !nodeIndex[String(name).toLowerCase()];
        tnodes.push({ x: x, y: y, orphan: orphan });
      }
      return byKey[k];
    }
    function addEdge(a, b, w) {
      tadj[a] = tadj[a] || [];
      if (!tadj[a].some(function (e) { return e.n === b; })) tadj[a].push({ n: b, w: w });
    }
    sceneEdges.forEach(function (e) {
      if (!isFinite(e.from_x) || !isFinite(e.from_y) || !isFinite(e.to_x) || !isFinite(e.to_y)) return;
      var a = vertex(e.from_name, e.from_x, e.from_y);
      var b = vertex(e.to_name, e.to_x, e.to_y);
      if (a === b) return;
      var w = Math.sqrt(dist2(tnodes[a], tnodes[b]));
      if (!(w > 0)) return;
      lens.push(w);
      // Curves are stored directed; for display routing robots drive them
      // either way, so the graph is undirected.
      addEdge(a, b, w); addEdge(b, a, w);
    });
    for (var i = 0; i < tnodes.length; i++) if (!tadj[i]) tadj[i] = [];
    lens.sort(function (a, b) { return a - b; });
    graphScale = lens[Math.floor(lens.length / 2)] || 0;
  }

  function dist2(p, q) { var dx = p.x - q.x, dy = p.y - q.y; return dx * dx + dy * dy; }

  function nearestTNode(wx, wy) {
    var best = -1, bd = Infinity, q = { x: wx, y: wy };
    for (var i = 0; i < tnodes.length; i++) {
      var d = dist2(tnodes[i], q);
      if (d < bd) { bd = d; best = i; }
    }
    return best;
  }

  // Dijkstra over the travel graph. Small graphs (a few hundred nodes), so a
  // plain O(V^2) scan is fine; results are cached per start/dest node pair.
  function shortestPath(s, d) {
    if (s < 0 || d < 0) return null;
    if (s === d) return [s];
    var key = s + ':' + d;
    if (routeCache[key]) return routeCache[key];
    var n = tnodes.length, dist = new Array(n), prev = new Array(n), seen = new Array(n);
    for (var i = 0; i < n; i++) { dist[i] = Infinity; prev[i] = -1; seen[i] = false; }
    dist[s] = 0;
    for (var k = 0; k < n; k++) {
      var u = -1, ud = Infinity;
      for (var t = 0; t < n; t++) { if (!seen[t] && dist[t] < ud) { ud = dist[t]; u = t; } }
      if (u < 0 || u === d) break;
      seen[u] = true;
      var edges = tadj[u] || [];
      for (var e = 0; e < edges.length; e++) {
        var nd = dist[u] + edges[e].w;
        if (nd < dist[edges[e].n]) { dist[edges[e].n] = nd; prev[edges[e].n] = u; }
      }
    }
    if (dist[d] === Infinity) { routeCache[key] = null; return null; }
    var path = [], c = d;
    while (c !== -1) { path.unshift(c); c = prev[c]; }
    routeCache[key] = path;
    return path;
  }

  // World-space route polyline robot -> aisle network -> destination, or null
  // if the graph can't connect them (caller falls back to a straight line).
  function routeWorld(rx, ry, dest) {
    if (tnodes.length < 2) return null;
    var s = nearestTNode(rx, ry), d = nearestTNode(dest.x, dest.y);
    var seq = shortestPath(s, d);
    if (!seq) return null;
    var pts = [[rx, ry]];
    for (var i = 0; i < seq.length; i++) pts.push([tnodes[seq[i]].x, tnodes[seq[i]].y]);
    pts.push([dest.x, dest.y]);
    return pts;
  }

  // ── node classes (e.g. advanced/action points vs bin locations) ────
  var CLASS_PALETTE = ['#6cb0ff', '#56d364', '#e3b341', '#d2a8ff', '#ff9b72', '#79c0ff', '#f0883e'];
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

  function svgEl(name, attrs) {
    var e = document.createElementNS(SVGNS, name);
    for (var k in attrs) if (attrs[k] !== undefined && attrs[k] !== null && attrs[k] !== '') e.setAttribute(k, attrs[k]);
    return e;
  }

  // Chevron pointing +X (heading 0), centered at origin: a forward triangle
  // with a notched tail so heading reads at a glance.
  function chevronPoints(s) {
    return s + ',0 ' + (-s * 0.85) + ',' + (s * 0.72) + ' ' +
      (-s * 0.42) + ',0 ' + (-s * 0.85) + ',' + (-s * 0.72);
  }

  // Compact lightning bolt centered on (cx, cy), sized to sit inside a
  // charge-point ring.
  function boltPoints(cx, cy, s) {
    var p = [[0.12, -0.6], [-0.38, 0.08], [-0.06, 0.08], [-0.12, 0.6], [0.38, -0.08], [0.06, -0.08]];
    return p.map(function (q) {
      return (cx + q[0] * s * 1.4) + ',' + (cy + q[1] * s * 1.4);
    }).join(' ');
  }

  // Mix a hex color toward white by `amt` (0..1) — used to brighten the comet's
  // leading arrow so the head reads as the front of the streak while the tail
  // keeps the route's status hue.
  function hexLighten(hex, amt) {
    var h = String(hex).replace('#', '');
    if (h.length === 3) h = h[0] + h[0] + h[1] + h[1] + h[2] + h[2];
    var r = parseInt(h.slice(0, 2), 16), g = parseInt(h.slice(2, 4), 16), b = parseInt(h.slice(4, 6), 16);
    if (!isFinite(r) || !isFinite(g) || !isFinite(b)) return hex;
    r = Math.round(r + (255 - r) * amt); g = Math.round(g + (255 - g) * amt); b = Math.round(b + (255 - b) * amt);
    return '#' + [r, g, b].map(function (v) { return ('0' + v.toString(16)).slice(-2); }).join('');
  }

  // Comet trail params, shared by the builder and the rAF positioner: a bright
  // leading arrow (i=0) trailed by dots that taper in size and fade behind it.
  // ~6s per lap; the tail spans ~16% of the path so the lane sits empty between
  // passes and other routes/nodes show through.
  var COMET_PERIOD = 6000;  // ms per lap
  var COMET_COUNT = 10;     // dots per comet (head + 9 tail)
  var COMET_SPAN = 0.16;    // tail occupies this fraction of the path length

  // buildCometDots appends one comet's dot elements to the overlay and returns
  // their specs. NO SMIL: positions are set every frame by tickComets from the
  // route's LIVE polyline, so the comet leaves the robot and tracks it as it
  // drives, and never restarts/teleports when the path is retargeted. (P17 —
  // replaces the animateMotion/mpath approach, which Chrome would not re-sample
  // when the referenced path's geometry changed mid-animation.)
  function buildCometDots(layer, color, robotR) {
    var headK = robotR * 0.5;
    var headColor = hexLighten(color, 0.45);
    var dots = [];
    for (var i = 0; i < COMET_COUNT; i++) {
      var el;
      if (i === 0) {
        el = svgEl('polygon', {
          points: (headK * 1.7) + ',0 ' + (-headK) + ',' + headK + ' ' +
            (-headK * 0.35) + ',0 ' + (-headK) + ',' + (-headK),
          fill: headColor, class: 'map-comet-head'
        });
      } else {
        el = svgEl('circle', { r: (robotR * 0.4 * (1 - 0.7 * i / COMET_COUNT)).toFixed(3), fill: color });
      }
      el.setAttribute('opacity', '0');
      layer.appendChild(el);
      dots.push({ el: el, idx: i, isHead: i === 0, baseOpacity: (1 - i / COMET_COUNT) });
    }
    return dots;
  }

  // Cumulative segment lengths so a 0..1 fraction maps to an arc-length point.
  function measurePolyline(pts) {
    var cum = [0], total = 0, i;
    for (i = 1; i < pts.length; i++) {
      var dx = pts[i][0] - pts[i - 1][0], dy = pts[i][1] - pts[i - 1][1];
      total += Math.sqrt(dx * dx + dy * dy);
      cum.push(total);
    }
    return { cum: cum, total: total };
  }

  // Point + heading (deg) at fraction f (0..1) along a measured polyline.
  function pointAt(pts, meas, f) {
    if (!pts || pts.length < 2 || meas.total <= 0) {
      return { x: pts && pts[0] ? pts[0][0] : 0, y: pts && pts[0] ? pts[0][1] : 0, a: 0 };
    }
    var target = f * meas.total, lo = 0;
    while (lo < meas.cum.length - 2 && meas.cum[lo + 1] < target) lo++;
    var segLen = meas.cum[lo + 1] - meas.cum[lo];
    var t = segLen > 0 ? (target - meas.cum[lo]) / segLen : 0;
    var p0 = pts[lo], p1 = pts[lo + 1];
    return {
      x: p0[0] + (p1[0] - p0[0]) * t,
      y: p0[1] + (p1[1] - p0[1]) * t,
      a: Math.atan2(p1[1] - p0[1], p1[0] - p0[0]) * 180 / Math.PI
    };
  }

  // tickComets positions every comet's dots along its LIVE polyline each frame.
  // Geometry comes from cometState, which syncCometLayer refreshes every render
  // tick — so the streak follows the robot as it drives, gliding continuously
  // with no teardown or restart. Idles itself (stops re-arming) when empty.
  function tickComets(now) {
    cometRAF = 0;
    if (!cometState.length) return;
    var base = (now % COMET_PERIOD) / COMET_PERIOD; // head phase, advances with time
    var gapFrac = COMET_SPAN / COMET_COUNT;
    for (var c = 0; c < cometState.length; c++) {
      var cs = cometState[c];
      for (var d = 0; d < cs.dots.length; d++) {
        var dot = cs.dots[d];
        var f = base - dot.idx * gapFrac;
        f -= Math.floor(f); // wrap into [0,1)
        var p = pointAt(cs.pts, cs.meas, f);
        // Fade in near the robot, out near the destination — no pop at the wrap.
        var env = f < 0.06 ? f / 0.06 : (f > 0.9 ? (1 - f) / 0.1 : 1);
        dot.el.setAttribute('opacity', (dot.baseOpacity * Math.max(0, Math.min(1, env))).toFixed(3));
        if (dot.isHead) {
          dot.el.setAttribute('transform', 'translate(' + p.x.toFixed(2) + ' ' + p.y.toFixed(2) + ') rotate(' + p.a.toFixed(1) + ')');
        } else {
          dot.el.setAttribute('cx', p.x.toFixed(2));
          dot.el.setAttribute('cy', p.y.toFixed(2));
        }
      }
    }
    cometRAF = requestAnimationFrame(tickComets);
  }
  function startComets() { if (!cometRAF && cometState.length) cometRAF = requestAnimationFrame(tickComets); }
  function stopComets() { if (cometRAF) { cancelAnimationFrame(cometRAF); cometRAF = 0; } }

  // ── persistent comet overlay ───────────────────────────────────────
  // The comets live on their own <svg> layered over the map — a sibling of
  // #map-svg-wrap inside the position:relative .map-main, with the same viewBox
  // and pointer-events:none so robot clicks fall through to the scene. The
  // per-tick scene rebuild wipes #map-svg-wrap, NOT this layer, so the comet
  // animations keep running; we only clear+rebuild the layer when the comet SET
  // actually changes (route added/removed, status/focus/scale/frame change),
  // detected by a signature. That decoupling is what fixes the teleporting and
  // the renderer churn.
  function ensureCometLayer() {
    if (cometLayer && cometLayer.parentNode) return cometLayer;
    var host = document.getElementById('map-svg-wrap');
    var parent = host && host.parentNode;
    if (!parent) return null;
    cometLayer = svgEl('svg', { class: 'map-comet-layer', preserveAspectRatio: 'xMidYMid meet' });
    // Insert right after the map wrap: above the scene (so comets aren't hidden
    // by the SVG's opaque background) but below the class-legend overlay.
    parent.insertBefore(cometLayer, host.nextSibling);
    return cometLayer;
  }

  function clearCometLayer() {
    if (cometLayer) { while (cometLayer.firstChild) cometLayer.removeChild(cometLayer.firstChild); }
    cometState = [];
    stopComets();
    lastCometSig = '';
  }

  function syncCometLayer(cometRoutes, robotR) {
    var layer = ensureCometLayer();
    if (!layer) return;
    if (view) layer.setAttribute('viewBox', view.minX + ' ' + view.minY + ' ' + view.w + ' ' + view.h);
    var vbKey = view ? (Math.round(view.minX) + ',' + Math.round(view.minY) + ',' +
      Math.round(view.w) + ',' + Math.round(view.h)) : 'none';
    // SET signature = which comets exist (identity + color + frame + scale), NOT
    // the path geometry. Robot movement only refreshes geometry (below); the dot
    // elements are rebuilt solely when the SET changes.
    var sig = vbKey + '|' + Math.round(robotR * 100) + '|' +
      cometRoutes.map(function (c) { return c.sig + ':' + c.color; }).join(';');
    if (sig !== lastCometSig) {
      // SET changed (route added/removed, focus, status, scale, frame) — rebuild
      // the dot elements. Brief and infrequent.
      lastCometSig = sig;
      while (layer.firstChild) layer.removeChild(layer.firstChild);
      cometState = cometRoutes.map(function (c) {
        return { pts: c.pts, meas: measurePolyline(c.pts), dots: buildCometDots(layer, c.color, robotR) };
      });
    } else {
      // Same SET — just refresh each comet's live geometry (robot moved). The rAF
      // positioner reads it next frame, so the streak tracks the robot with no
      // teardown and no restart. The deterministic sort in render() keeps index →
      // comet stable, so cometState[i] always matches cometRoutes[i].
      for (var i = 0; i < cometState.length && i < cometRoutes.length; i++) {
        cometState[i].pts = cometRoutes[i].pts;
        cometState[i].meas = measurePolyline(cometRoutes[i].pts);
      }
    }
    startComets();
  }

  // ── minimap (persistent small overview, bottom-right of .map-region) ──
  // The minimap SVG has a FIXED viewBox = the full plant. Only the robot dots
  // and the viewport rect change — those are updated cheaply by tickEase and
  // render() without rebuilding the static network layer.

  function rebuildMinimap() {
    if (!fullPlantView) return;
    var region = document.querySelector('.map-region');
    if (!region) return;
    if (!minimapEl) {
      minimapEl = svgEl('svg', { class: 'map-minimap', 'pointer-events': 'none' });
      region.appendChild(minimapEl);
    }
    var fp = fullPlantView;
    minimapEl.setAttribute('viewBox', fp.minX + ' ' + fp.minY + ' ' + fp.w + ' ' + fp.h);
    minimapEl.setAttribute('preserveAspectRatio', 'xMidYMid meet');
    while (minimapEl.firstChild) minimapEl.removeChild(minimapEl.firstChild);

    // Static layer: travel network + all scene point dots
    var netG = svgEl('g', { opacity: '0.3' });
    var sw = (graphScale > 0 ? graphScale : Math.max(fp.w, fp.h) * 0.01) * 0.06;
    if (tnodes.length > 1) {
      var seen = {};
      for (var ai = 0; ai < tadj.length; ai++) {
        var edges = tadj[ai] || [];
        for (var ei = 0; ei < edges.length; ei++) {
          var bi = edges[ei].n;
          var ekey = ai < bi ? ai + '_' + bi : bi + '_' + ai;
          if (seen[ekey]) continue; seen[ekey] = true;
          var mpa = proj(tnodes[ai].x, tnodes[ai].y);
          var mpb = proj(tnodes[bi].x, tnodes[bi].y);
          netG.appendChild(svgEl('line', {
            x1: mpa[0], y1: mpa[1], x2: mpb[0], y2: mpb[1],
            stroke: cssVar('--map-aisle', '#45566e'), 'stroke-width': sw
          }));
        }
      }
    }
    var dotR = sw * 1.2;
    points.forEach(function (p) {
      if (!isFinite(p.pos_x) || !isFinite(p.pos_y)) return;
      var ms = proj(p.pos_x, p.pos_y);
      netG.appendChild(svgEl('circle', { cx: ms[0], cy: ms[1], r: dotR, fill: cssVar('--map-node', '#66768f') }));
    });
    minimapEl.appendChild(netG);

    // Dynamic: robot dots group + viewport indicator rect
    minimapRobotGroup = svgEl('g');
    minimapEl.appendChild(minimapRobotGroup);
    var vbSw = sw * 1.8;
    minimapViewportRect = svgEl('rect', {
      fill: 'none', stroke: cssVar('--map-aisle', '#45566e'),
      'stroke-width': vbSw, rx: vbSw * 1.5
    });
    minimapEl.appendChild(minimapViewportRect);

    updateMinimapDynamic();
  }

  // Called from tickEase (viewport rect) and render() (robot dots + rect).
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
        var moving = r.state === 'busy' || !!ord || isMoving(r);
        var ms2 = proj(r.x, r.y);
        minimapRobotGroup.appendChild(svgEl('circle', {
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

  function drawNode(svg, p, nodeR) {
    var wx = p.pos_x, wy = p.pos_y;
    var cls = classOf(p);
    // Action points snap onto the nearest network vertex when their scene
    // coordinate sits a hair off the curve endpoint they serve — otherwise
    // the ring floats beside its node with its own dot, which reads sloppy.
    // Half a typical edge length is close enough to mean "same node".
    if (cls === 'ActionPoint' && graphScale > 0 && tnodes.length) {
      var ni = nearestTNode(wx, wy);
      if (ni >= 0) {
        var nd = Math.sqrt(dist2(tnodes[ni], { x: wx, y: wy }));
        if (nd > 0 && nd <= graphScale * 0.5) { wx = tnodes[ni].x; wy = tnodes[ni].y; }
      }
    }
    var s = proj(wx, wy);
    var hot = hotNodes[String(p.point_name || '').toLowerCase()] ||
      hotNodes[String(p.label || '').toLowerCase()] ||
      hotNodes[String(p.instance_name || '').toLowerCase()];
    // Capture each node's PRIMARY glyph so an active node can take the accent on
    // the glyph itself (P21) rather than via a separate ring drawn on top.
    var glyph = null;
    if (isTravel(cls)) {
      // Travel nodes are path structure only — no dot drawn. The aisle lines
      // carry the network; vertex dots added visual noise without information.
    } else if (cls === 'ActionPoint') {
      // Single dot — no ring.
      glyph = svgEl('circle', { cx: s[0], cy: s[1], r: nodeR * 1.0, fill: NODE_ACTION_COLOR });
      svg.appendChild(glyph);
    } else if (cls === 'ChargePoint') {
      // Thin ring (charge stays identifiable) + faint bolt; smaller than before
      // so a row of chargers reads as a calm strip, not a noisy fence.
      glyph = svgEl('circle', {
        cx: s[0], cy: s[1], r: nodeR * 0.95, class: 'map-node-charge',
        fill: 'none', stroke: CHARGE_RING, 'stroke-width': nodeR * 0.22
      });
      svg.appendChild(glyph);
      svg.appendChild(svgEl('polygon', {
        points: boltPoints(s[0], s[1], nodeR * 0.8), fill: cssVar('--map-bay-glyph', '#6b7a90'), 'fill-opacity': 0.4
      }));
    } else if (cls === 'ParkPoint') {
      // Single faint dot — no ring. Park bays are plentiful; rings-on-rings made
      // a charge row look identical to a pack of park bays.
      glyph = svgEl('circle', {
        cx: s[0], cy: s[1], r: nodeR * 0.85,
        fill: cssVar('--map-node', '#7e92b3'), 'fill-opacity': 0.55
      });
      svg.appendChild(glyph);
    } else {
      glyph = svgEl('circle', { cx: s[0], cy: s[1], r: nodeR * 0.9, fill: classColors[cls] || '#67748f', 'fill-opacity': 0.7 });
      svg.appendChild(glyph);
    }
    // Active node (order source/destination): the glyph itself takes the UI
    // accent — an indigo stroke + soft glow — marking it as live/active (per the
    // guide, the accent marks genuinely live elements). No separate ring; green
    // stays delivered-only.
    if (hot && glyph) {
      glyph.setAttribute('stroke', ACCENT);
      glyph.setAttribute('stroke-width', nodeR * 0.45);
      glyph.setAttribute('style', 'filter: drop-shadow(0 0 ' + (nodeR * 1.1).toFixed(2) + 'px ' + ACCENT_GLOW + ')');
    }
  }

  function render() {
    var host = document.getElementById('map-svg-wrap');
    var empty = document.getElementById('map-empty');
    if (!host) return;
    // Click-to-focus: clicking a robot (its glyph or name chip) lights that
    // robot's route comet and dims the rest; clicking it again, or clicking
    // empty floor, clears the focus. Delegated once — the SVG is rebuilt each
    // render, so we walk up to the nearest [data-robot] ancestor.
    if (!clickBound) {
      clickBound = true;
      host.addEventListener('click', function (ev) {
        var t = ev.target;
        while (t && t !== host && !(t.getAttribute && t.getAttribute('data-robot'))) t = t.parentNode;
        var id = (t && t.getAttribute) ? t.getAttribute('data-robot') : null;
        focusRobot = (id && id === focusRobot) ? null : (id || null);
        scheduleRender();
      });
    }
    computeView();
    if (!view) {
      host.innerHTML = '';
      clearCometLayer();
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

    var svg = svgEl('svg', {
      class: 'map-svg',
      viewBox: view.minX + ' ' + view.minY + ' ' + view.w + ' ' + view.h,
      preserveAspectRatio: 'xMidYMid meet'
    });

    drawBackdrop(svg, unit);

    // travel network edges — faint connective tissue under everything.
    if (tnodes.length > 1) {
      var seen = {};
      for (var a = 0; a < tadj.length; a++) {
        var edges = tadj[a] || [];
        for (var e = 0; e < edges.length; e++) {
          var b = edges[e].n, key = a < b ? a + '_' + b : b + '_' + a;
          if (seen[key]) continue; seen[key] = true;
          var pa = proj(tnodes[a].x, tnodes[a].y), pb = proj(tnodes[b].x, tnodes[b].y);
          svg.appendChild(svgEl('line', {
            x1: pa[0], y1: pa[1], x2: pb[0], y2: pb[1],
            class: 'map-aisle', 'stroke-width': nodeR * 0.48
          }));
        }
      }
    }

    // ── routes ────────────────────────────────────────────────────────
    // Comet-only: the standing route line is gone. The comet alone carries the
    // route, and it lives on a PERSISTENT overlay (syncCometLayer below) that the
    // per-tick scene rebuild does not touch — so it animates continuously instead
    // of restarting every SSE tick (which made it teleport, and the constant SMIL
    // churn pinned the renderer). Here we only draw the destination ring, collect
    // each comet's stable path, and — for reduced-motion users — a faint static
    // lane line as the accessible, motion-free fallback.
    var reduceMotion = !!(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches);
    var activeRoutes = orders.filter(function (o) {
      var r = robots[o.robot_id];
      return r && isFinite(r.x) && isFinite(r.y) && findNode(o.delivery_node);
    });
    // Which routes get a comet: with a focus set, only the focused robot's;
    // otherwise all of them while the floor is quiet (≤ COMET_LIMIT). A busy
    // unfocused floor shows none (just robots + destination rings) so it doesn't
    // become a swarm of arrows — click a robot to light its lane.
    var cometRoutes = [];
    activeRoutes.forEach(function (o, idx) {
      var r = robots[o.robot_id];
      var dest = findNode(o.delivery_node);
      var color = STATUS_COLOR[o.status] || '#888';
      var focused = !!focusRobot && o.robot_id === focusRobot;
      var dimmed = !!focusRobot && !focused;
      var wantComet = !reduceMotion && (focusRobot ? focused : activeRoutes.length <= COMET_LIMIT);
      if (wantComet) {
        // Comet runs from the ROBOT'S CURRENT position to its destination, along
        // the network — routeWorld() starts the path at (r.x, r.y), so the streak
        // leaves the robot, not a fixed node. The polyline is recomputed every
        // tick and handed to syncCometLayer; the rAF positioner reads it live, so
        // the comet tracks the robot while gliding. syncCometLayer rebuilds the
        // dot elements only when the comet SET changes (route added/removed/
        // focus/status).
        var cworld = routeWorld(r.x, r.y, dest);
        var cpts = cworld
          ? cworld.map(function (w) { return proj(w[0], w[1]); })
          : [proj(r.x, r.y), proj(dest.x, dest.y)];
        cometRoutes.push({
          pts: cpts,
          color: color,
          // membership/rebuild key (NOT geometry) — geometry tracks the robot
          // live via the rAF positioner, never forcing a rebuild on movement.
          sig: o.robot_id + '>' + o.delivery_node + '>' + o.status
        });
      }
      // Reduced-motion fallback: a faint static lane line (no animation).
      if (reduceMotion) {
        var rworld = routeWorld(r.x, r.y, dest);
        var rscreen = rworld ? rworld.map(function (w) { return proj(w[0], w[1]); }) : [proj(r.x, r.y), proj(dest.x, dest.y)];
        svg.appendChild(svgEl('polyline', {
          points: rscreen.map(function (p) { return p[0] + ',' + p[1]; }).join(' '),
          class: 'map-route-base', fill: 'none', stroke: color,
          'stroke-width': robotR * 0.22, 'stroke-opacity': dimmed ? 0.25 : 0.5
        }));
      }
      // destination marker ring (always).
      var dp = proj(dest.x, dest.y);
      svg.appendChild(svgEl('circle', {
        cx: dp[0], cy: dp[1], r: robotR * 0.7, fill: 'none', stroke: color,
        'stroke-width': robotR * 0.14, 'stroke-opacity': dimmed ? 0.35 : 0.65
      }));
    });
    // Deterministic order so the comet SET signature is order-independent — a
    // server reshuffle of `orders` doesn't force a needless rebuild, and index →
    // overlay path id stays stable for in-place geometry updates.
    cometRoutes.sort(function (a, b) { return a.sig < b.sig ? -1 : a.sig > b.sig ? 1 : 0; });

    // nodes — receded travel dots + distinct outlined waypoint shapes, ON TOP
    // of routes so they stay legible even with paths running underneath.
    points.forEach(function (p) {
      if (!isFinite(p.pos_x) || !isFinite(p.pos_y)) return;
      drawNode(svg, p, nodeR);
    });
    // Orphan tnodes (edge endpoints with no scene point) — no dot drawn; the
    // aisle lines already show where the network goes.

    // robots — halo, then chevron, so labels (last pass) sit above everything.
    var robotList = Object.keys(robots).map(function (k) { return robots[k]; })
      .filter(function (r) { return isFinite(r.x) && isFinite(r.y); });
    robotList.forEach(function (r) {
      var s = proj(r.x, r.y);
      var ord = orderByRobot[r.id];
      var moving = r.state === 'busy' || !!ord || isMoving(r);
      var color = robotColor(r, ord, moving);
      var alert = r.state === 'error';
      var fault = alert || (ord && (ord.status === 'blocked' || ord.status === 'faulted'));
      // Each robot's glyphs live in a [data-robot] group so a click anywhere on
      // the robot focuses its route (delegated handler walks up to this group).
      var rg = svgEl('g', { 'data-robot': r.id, class: 'map-robot-hit' });
      svg.appendChild(rg);
      // Focus ring on the selected robot — a quiet outline, not another color.
      if (focusRobot && r.id === focusRobot) {
        rg.appendChild(svgEl('circle', {
          cx: s[0], cy: s[1], r: robotR * 1.8, class: 'map-robot-focus',
          fill: 'none', 'stroke-width': robotR * 0.12
        }));
      }
      // Fault flare: loud outer ring so a blocked/error robot is the first thing
      // the eye hits. Drawn outside the normal halo so both rings compound.
      if (fault) {
        rg.appendChild(svgEl('circle', {
          cx: s[0], cy: s[1], r: robotR * 2.4, class: 'map-robot-fault-flare',
          fill: STATUS_COLOR.blocked || STATE_COLOR.error
        }));
      }
      // Halo only where it carries signal: motion or a fault. Parked robots
      // get none, so a charge row reads as a tidy strip of docked units.
      if (moving || alert) {
        rg.appendChild(svgEl('circle', { cx: s[0], cy: s[1], r: robotR * 1.35, class: 'map-robot-halo', fill: color }));
      }
      if (moving) {
        // In motion the chevron shows heading. Fleet Angle is radians
        // (confirmed live); SVG rotate wants degrees.
        var rot = -(r.angle * 180 / Math.PI) + (rotate90 ? 90 : 0);
        var g = svgEl('g', { transform: 'translate(' + s[0] + ',' + s[1] + ') rotate(' + rot + ')' });
        g.appendChild(svgEl('polygon', { points: chevronPoints(robotR), class: 'map-robot', fill: color, 'stroke-width': robotR * 0.16 }));
        rg.appendChild(g);
        rg.appendChild(svgEl('circle', { cx: s[0], cy: s[1], r: robotR * 0.22, class: 'map-robot-core' }));
      } else {
        // Parked/stopped: heading is noise — a compact disc reads as a docked
        // unit. A ready robot docked on a bay snaps to the bay center and
        // fills the ring's inner edge: the reported pose sits a few cm off
        // the bay point, which left a crescent of background between disc
        // and ring instead of a filled socket.
        var cx = s[0], cy = s[1], dr = robotR * 0.62;
        if (r.state === 'ready') {
          var dock = dockOf(r);
          if (dock) {
            var bs = proj(dock.x, dock.y);
            cx = bs[0]; cy = bs[1];
            dr = nodeR * (dock.kind === 'charge' ? 1.05 : 0.88);
          }
        }
        rg.appendChild(svgEl('circle', {
          cx: cx, cy: cy, r: dr, class: 'map-robot',
          fill: color, 'stroke-width': robotR * 0.14
        }));
        rg.appendChild(svgEl('circle', { cx: cx, cy: cy, r: dr * 0.3, class: 'map-robot-core' }));
      }
    });

    // Second pass: name chips with greedy downward de-collision, so a cluster of
    // robots parked on top of each other reads as a vertical list of names.
    var placed = [];
    robotList.forEach(function (r) {
      var ord = orderByRobot[r.id];
      // Name chips only where they carry signal: on an order, physically
      // moving, or faulted. Stationary paused/offline robots keep their grey
      // disc but no tag — the cluster stays calm.
      if (!ord && r.state !== 'error' && !isMoving(r)) return;
      var s = proj(r.x, r.y);
      var lx = s[0], ly = s[1] - robotR * 2.0;
      var guard = 0;
      while (guard++ < 16 && placed.some(function (p) {
        return Math.abs(p.x - lx) < fontS * 5.2 && Math.abs(p.y - ly) < fontS * 1.25;
      })) { ly += fontS * 1.35; }
      placed.push({ x: lx, y: ly });
      var color = robotColor(r, ord, r.state === 'busy' || !!ord || isMoving(r));
      var halfW = (r.id.length * fontS * 0.62) / 2 + fontS * 0.55;
      var chipH = fontS * 1.3;
      // Chip lives in a [data-robot] group too, so clicking the name focuses
      // the same robot as clicking its glyph.
      var cg = svgEl('g', { 'data-robot': r.id, class: 'map-robot-hit' });
      svg.appendChild(cg);
      // Leader line ties a displaced chip back to its robot so a stacked
      // cluster of names stays attributable.
      if (Math.abs(ly - s[1]) > robotR * 2.6) {
        cg.appendChild(svgEl('line', {
          x1: s[0], y1: s[1], x2: lx, y2: ly - chipH * 0.7,
          class: 'map-chip-leader', 'stroke-width': fontS * 0.06
        }));
      }
      cg.appendChild(svgEl('rect', {
        x: lx - halfW, y: ly - chipH * 0.75, width: halfW * 2, height: chipH, rx: fontS * 0.25,
        class: 'map-chip', stroke: color, 'stroke-width': fontS * 0.06
      }));
      cg.appendChild(svgEl('circle', { cx: lx - halfW + fontS * 0.5, cy: ly - chipH * 0.1, r: fontS * 0.2, fill: color }));
      var label = svgEl('text', { x: lx + fontS * 0.35, y: ly, class: 'map-robot-label', 'font-size': fontS });
      label.textContent = r.id;
      cg.appendChild(label);
    });

    host.innerHTML = '';
    host.appendChild(svg);
    syncCometLayer(cometRoutes, robotR);
    renderClassLegend();
    renderLegend(); // live fleet counts track every robot/order tick
    renderRail();
    rebuildMinimap(); // rebuild static network layer + robot dots + viewport rect
  }

  // Faint grid + corner brackets — gives the floor a frame so the scene reads
  // as a plant view rather than dots in a void.
  function drawBackdrop(svg, unit) {
    var step = unit * 0.08;
    var gridW = unit * 0.0012; // stroke width in user units — CSS px wouldn't scale
    var x0 = view.minX, y0 = view.minY, x1 = view.minX + view.w, y1 = view.minY + view.h;
    var gx, gy;
    for (gx = Math.ceil(x0 / step) * step; gx < x1; gx += step) {
      svg.appendChild(svgEl('line', { x1: gx, y1: y0, x2: gx, y2: y1, class: 'map-grid', 'stroke-width': gridW }));
    }
    for (gy = Math.ceil(y0 / step) * step; gy < y1; gy += step) {
      svg.appendChild(svgEl('line', { x1: x0, y1: gy, x2: x1, y2: gy, class: 'map-grid', 'stroke-width': gridW }));
    }
    var L = unit * 0.04, inset = unit * 0.015, sw = unit * 0.0025;
    var corners = [
      [x0 + inset, y0 + inset, 1, 1], [x1 - inset, y0 + inset, -1, 1],
      [x0 + inset, y1 - inset, 1, -1], [x1 - inset, y1 - inset, -1, -1]
    ];
    corners.forEach(function (c) {
      svg.appendChild(svgEl('path', {
        d: 'M' + (c[0] + c[2] * L) + ' ' + c[1] + ' H' + c[0] + ' V' + (c[1] + c[3] * L),
        class: 'map-bracket', fill: 'none', 'stroke-width': sw
      }));
    });
  }

  function escapeText(s) {
    var d = document.createElement('span');
    d.textContent = (s === null || s === undefined) ? '' : s;
    return d.innerHTML;
  }

  // Class legend mirrors the actual node encoding: travel dots recede, typed
  // waypoints are outlined shapes. Unknown classes fall back to palette dots.
  function legendSwatch(color, shape, label) {
    var style = 'background:' + color;
    if (shape === 'ring') style = 'background:transparent;border:2px solid ' + color;
    if (shape === 'square') style = 'background:transparent;border:2px solid ' + color + ';border-radius:3px';
    return '<span class="map-legend-item"><span class="map-legend-dot" style="' + style + '"></span>' +
      escapeText(label) + '</span>';
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
    if (!items.length) { el.innerHTML = ''; return; }
    // The node-type key is reference an operator learns once, so it lives behind
    // a small collapsible toggle (default closed) instead of a permanent second
    // legend strip competing with the live status counts in the header.
    el.innerHTML =
      '<button type="button" class="map-key-toggle" aria-expanded="' + mapKeyOpen + '">' +
        (mapKeyOpen ? '▾' : '▸') + ' Map key</button>' +
      '<div class="map-key-items"' + (mapKeyOpen ? '' : ' hidden') + '>' + items.join('') + '</div>';
    var btn = el.querySelector('.map-key-toggle');
    if (btn) btn.addEventListener('click', function () { mapKeyOpen = !mapKeyOpen; renderClassLegend(); });
  }

  // ── status rail + activity feed ────────────────────────────────────

  function pushFeedEvent(text, level) {
    activityFeed.unshift({ text: text, level: level || 'info', ts: Date.now() });
    if (activityFeed.length > 30) activityFeed.length = 30;
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
          pushFeedEvent((o.delivery_node || o.source_node || '?') + ' blocked', 'alert');
        }
      }
    });
    prevOrderMap = newMap;
  }

  function renderRail() {
    var motionEl = document.getElementById('rail-motion-list');
    var activityEl = document.getElementById('rail-activity-list');
    if (!motionEl || !activityEl) return;

    // ── In-motion list ────────────────────────────────────────────────
    var INACTIVE = { delivered: true, confirmed: true, cancelled: true };
    var active = orders.filter(function (o) { return !INACTIVE[o.status]; });
    active.sort(function (a, b) {
      function rank(o) {
        if (o.status === 'blocked' || o.status === 'faulted') return 0;
        if (o.status === 'in_transit') return 1;
        if (o.status === 'staged' || o.status === 'dispatched' || o.status === 'queued') return 2;
        return 3;
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
      motionEl.innerHTML = '<li class="rail-empty">No active orders</li>';
    } else {
      motionEl.innerHTML = shown.map(function (o) {
        var color = STATUS_COLOR[o.status] || '#888';
        var label = statusLabel[o.status] || o.status;
        return '<li class="rail-row" style="border-left-color:' + color + '">' +
          '<span class="rail-row-id">' + escapeText(o.robot_id || '—') + '</span>' +
          '<span class="rail-row-arrow">→</span>' +
          '<span class="rail-row-node">' + escapeText(o.delivery_node || '?') + '</span>' +
          '<span class="rail-row-status" style="color:' + color + '">' + escapeText(label) + '</span>' +
          '</li>';
      }).join('') + (overflow ? '<li class="rail-empty">+' + overflow + ' more</li>' : '');
    }

    // ── Activity feed ─────────────────────────────────────────────────
    var now = Date.now();
    activityFeed = activityFeed.filter(function (e) { return (now - e.ts) < FEED_MAX_AGE_MS; });
    var showing = activityFeed.slice(0, FEED_MAX_ITEMS);
    if (!showing.length) {
      activityEl.innerHTML = '<li class="rail-empty">No recent activity</li>';
    } else {
      activityEl.innerHTML = showing.map(function (e) {
        var ageFrac = (now - e.ts) / FEED_MAX_AGE_MS;
        var opacity = Math.max(0.25, 1 - ageFrac * 0.75).toFixed(2);
        var cls = 'rail-event' + (e.level === 'alert' ? ' rail-event-alert' : '');
        return '<li class="' + cls + '" style="opacity:' + opacity + '">' + escapeText(e.text) + '</li>';
      }).join('');
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
      orders.forEach(function (o) {
        if (o.robot_id) orderByRobot[o.robot_id] = o;
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
