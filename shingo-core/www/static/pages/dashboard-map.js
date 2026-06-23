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
// Visual language (the "floor HUD"): robots are the hero — a bright chevron with
// heading, a soft pulsing halo, and a monospace name chip. The scene recedes to
// a faint travel-node network so the robots read clearly against it; action /
// charge / park points draw as distinct outlined shapes rather than filled blobs.
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
  var CHARGE_RING = '#c9a227';
  var PARK_RING = '#b0723a';

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
  var clickBound = false;   // host click handler attached once
  // Above this many active routes, the ambient view calms to dim lines (no
  // comets) so a busy floor doesn't become 20 arrows fighting for attention;
  // clicking a robot still lights its comet. Tune to taste.
  var COMET_LIMIT = 12;

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

  // Effective robot color: order status > fault > bay hue > state.
  // Color carries STATUS, shape carries MOTION. An idle robot driving itself
  // (e.g. returning to park after a cancel) is a green chevron — transit blue
  // means it's actually on an order / busy. Bay hues only apply while
  // stationary, so a robot passing a charger doesn't flash amber.
  function robotColor(r, ord, moving) {
    if (ord) return STATUS_COLOR[ord.status] || STATE_COLOR[r.state] || '#888';
    if (r.state === 'error' || r.state === 'offline') return STATE_COLOR[r.state];
    // Bay hue only for READY robots at rest — a paused robot on a park bay
    // stays grey; its pause is the signal, not the bay.
    if (!moving && r.state === 'ready') {
      var dock = dockOf(r);
      if (dock) return DOCK_COLOR[dock.kind];
    }
    return STATE_COLOR[r.state] || '#888';
  }

  // ── coordinate framing: screen = (x, -y) ───────────────────────────
  function computeView() {
    var wx = [], wy = [];
    points.forEach(function (p) {
      if (isFinite(p.pos_x) && isFinite(p.pos_y)) { wx.push(p.pos_x); wy.push(p.pos_y); }
    });
    // Robots are EXCLUDED from the view bounds when a scene is synced: folding
    // live positions into the frame re-fit (and so subtly rescaled/panned) the
    // whole map every tick, which shifted every route path out from under the
    // comet. Fall back to robot positions only when there is no scene to frame.
    if (!points.length) {
      Object.keys(robots).forEach(function (k) {
        var r = robots[k];
        if (isFinite(r.x) && isFinite(r.y)) { wx.push(r.x); wy.push(r.y); }
      });
    }
    // Orphan graph vertices (edge endpoints with no synced point) can sit
    // outside the points' bounding box — include them so lines aren't clipped.
    tnodes.forEach(function (t) {
      if (t.orphan) { wx.push(t.x); wy.push(t.y); }
    });
    if (!wx.length) { view = null; return; }
    var minWx = Math.min.apply(null, wx), maxWx = Math.max.apply(null, wx);
    var minWy = Math.min.apply(null, wy), maxWy = Math.max.apply(null, wy);
    // Orient the plant's long axis horizontally so a tall footprint fills a
    // wide screen instead of being squeezed into a thin central column.
    rotate90 = (maxWy - minWy) > (maxWx - minWx);
    var sx = [], sy = [];
    for (var i = 0; i < wx.length; i++) {
      var s = proj(wx[i], wy[i]);
      sx.push(s[0]); sy.push(s[1]);
    }
    var minX = Math.min.apply(null, sx), maxX = Math.max.apply(null, sx);
    var minY = Math.min.apply(null, sy), maxY = Math.max.apply(null, sy);
    var w = Math.max(maxX - minX, 1), h = Math.max(maxY - minY, 1);
    var pad = Math.max(w, h) * 0.05;
    view = { minX: minX - pad, minY: minY - pad, w: w + 2 * pad, h: h + 2 * pad };
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

  // Comet: a bright leading arrow followed by a tail of dots that taper in size
  // and fade in opacity behind it, riding the route path (#pathId) via SMIL
  // animateMotion. Repeats seamlessly; each dot fades out at the destination and
  // back in at the robot, so the path is empty between laps and other routes /
  // nodes show through. Trail length 10, ~6s per lap (chosen in preview).
  function addComet(svg, pathId, color, robotR) {
    var dur = 6, count = 10;
    var gap = (dur * 0.16) / count; // tail tightness: dots span ~16% of a lap
    var headK = robotR * 0.5;
    var headColor = hexLighten(color, 0.45);
    for (var i = 0; i < count; i++) {
      var begin = (-dur + i * gap).toFixed(3) + 's'; // most-negative = leading head
      var op = (1 - i / count).toFixed(3);
      var node;
      if (i === 0) {
        node = svgEl('polygon', {
          points: (headK * 1.7) + ',0 ' + (-headK) + ',' + headK + ' ' +
            (-headK * 0.35) + ',0 ' + (-headK) + ',' + (-headK),
          fill: headColor, class: 'map-comet-head'
        });
      } else {
        node = svgEl('circle', { cx: 0, cy: 0, r: (robotR * 0.4 * (1 - 0.7 * i / count)).toFixed(3), fill: color });
      }
      var am = svgEl('animateMotion', { dur: dur + 's', repeatCount: 'indefinite', begin: begin, rotate: 'auto' });
      var mp = document.createElementNS(SVGNS, 'mpath');
      mp.setAttributeNS('http://www.w3.org/1999/xlink', 'href', '#' + pathId);
      mp.setAttribute('href', '#' + pathId);
      am.appendChild(mp);
      node.appendChild(am);
      node.appendChild(svgEl('animate', {
        attributeName: 'opacity', values: '0;' + op + ';' + op + ';0', keyTimes: '0;0.06;0.9;1',
        dur: dur + 's', begin: begin, repeatCount: 'indefinite'
      }));
      svg.appendChild(node);
    }
  }

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
    lastCometSig = '';
  }

  function syncCometLayer(cometRoutes, robotR) {
    var vbKey = view ? (Math.round(view.minX) + ',' + Math.round(view.minY) + ',' +
      Math.round(view.w) + ',' + Math.round(view.h)) : 'none';
    var sig = vbKey + '|' + Math.round(robotR * 100) + '|' +
      cometRoutes.map(function (c) { return c.sig; }).join(';');
    if (sig === lastCometSig) return; // nothing structural changed — let SMIL run
    lastCometSig = sig;
    var layer = ensureCometLayer();
    if (!layer) return;
    if (view) layer.setAttribute('viewBox', view.minX + ' ' + view.minY + ' ' + view.w + ' ' + view.h);
    while (layer.firstChild) layer.removeChild(layer.firstChild);
    cometRoutes.forEach(function (c) {
      // invisible path = the comet's motion track (mpath target).
      layer.appendChild(svgEl('path', { id: c.id, d: c.d, fill: 'none', stroke: 'none' }));
      addComet(layer, c.id, c.color, robotR);
    });
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
    if (isTravel(cls)) {
      // The numerous travel waypoints recede to a faint dot network.
      svg.appendChild(svgEl('circle', { cx: s[0], cy: s[1], r: nodeR * 0.6, class: 'map-node-travel' }));
    } else if (cls === 'ActionPoint') {
      // An action point IS a node on the network — draw it as the standard
      // node dot with an outline ring around it, not a detached filled donut
      // floating beside the web.
      svg.appendChild(svgEl('circle', { cx: s[0], cy: s[1], r: nodeR * 0.55, class: 'map-node-travel' }));
      svg.appendChild(svgEl('circle', {
        cx: s[0], cy: s[1], r: nodeR * 1.5, class: 'map-node-action',
        fill: 'none', stroke: '#587aa6', 'stroke-width': nodeR * 0.4
      }));
    } else if (cls === 'ChargePoint') {
      svg.appendChild(svgEl('circle', {
        cx: s[0], cy: s[1], r: nodeR * 1.3, class: 'map-node-charge',
        fill: 'none', stroke: CHARGE_RING, 'stroke-width': nodeR * 0.45
      }));
      svg.appendChild(svgEl('polygon', {
        points: boltPoints(s[0], s[1], nodeR), fill: CHARGE_RING, 'fill-opacity': 0.75
      }));
    } else if (cls === 'ParkPoint') {
      // Ring like the other waypoint types — color differentiates. (Squares
      // merged into a striped strip when park bays sat a glyph-width apart.)
      svg.appendChild(svgEl('circle', {
        cx: s[0], cy: s[1], r: nodeR * 1.1, class: 'map-node-park',
        fill: 'none', stroke: PARK_RING, 'stroke-width': nodeR * 0.4
      }));
    } else {
      svg.appendChild(svgEl('circle', { cx: s[0], cy: s[1], r: nodeR * 0.9, fill: classColors[cls] || '#67748f', 'fill-opacity': 0.7 }));
    }
    // Order source/destination highlight: a status-colored ring on top.
    if (hot) {
      svg.appendChild(svgEl('circle', {
        cx: s[0], cy: s[1], r: nodeR * 2.2, class: 'map-node-hot',
        fill: 'none', stroke: STATUS_COLOR[hot] || '#fff', 'stroke-width': nodeR * 0.5
      }));
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
    var nodeR = Math.max(unit * 0.0018, Math.min(unit * 0.006, base * 0.3));
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
            class: 'map-aisle', 'stroke-width': nodeR * 0.35
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
        // Comet rides a STABLE lane: source → destination along the network when a
        // source node is known, so the path doesn't shift as the robot drives;
        // falls back to the robot's current position only when there's no source.
        var src = findNode(o.source_node);
        var sx = src ? src.x : r.x, sy = src ? src.y : r.y;
        var cworld = routeWorld(sx, sy, dest);
        var cpts = cworld
          ? cworld.map(function (w) { return proj(w[0], w[1]); })
          : [proj(sx, sy), proj(dest.x, dest.y)];
        cometRoutes.push({
          id: 'comet-route-' + idx,
          d: 'M ' + cpts.map(function (p) { return p[0] + ' ' + p[1]; }).join(' L '),
          color: color,
          // identity (NOT geometry) — so robot movement alone does not rebuild.
          sig: o.robot_id + '>' + (o.source_node || '') + '>' + o.delivery_node + '>' + o.status
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

    // nodes — receded travel dots + distinct outlined waypoint shapes, ON TOP
    // of routes so they stay legible even with paths running underneath.
    points.forEach(function (p) {
      if (!isFinite(p.pos_x) || !isFinite(p.pos_y)) return;
      drawNode(svg, p, nodeR);
    });
    // Edge endpoints that never synced as scene points ("missing" nodes)
    // still join the travel network — draw them so no line ends float.
    tnodes.forEach(function (t) {
      if (!t.orphan) return;
      var os = proj(t.x, t.y);
      svg.appendChild(svgEl('circle', { cx: os[0], cy: os[1], r: nodeR * 0.6, class: 'map-node-travel' }));
    });

    // robots — halo, then chevron, so labels (last pass) sit above everything.
    var robotList = Object.keys(robots).map(function (k) { return robots[k]; })
      .filter(function (r) { return isFinite(r.x) && isFinite(r.y); });
    robotList.forEach(function (r) {
      var s = proj(r.x, r.y);
      var ord = orderByRobot[r.id];
      var moving = r.state === 'busy' || !!ord || isMoving(r);
      var color = robotColor(r, ord, moving);
      var alert = r.state === 'error';
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
    if (have.LocationMark || have.GeneralLocation) items.push(legendSwatch('#323c4a', 'dot', 'Travel node'));
    if (have.ActionPoint) items.push(legendSwatch('#587aa6', 'ring', 'Action point'));
    if (have.ChargePoint) items.push(legendSwatch(CHARGE_RING, 'ring', 'Charge point'));
    if (have.ParkPoint) items.push(legendSwatch('#b0723a', 'ring', 'Park point'));
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
