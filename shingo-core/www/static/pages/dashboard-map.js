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
// Routes follow the aisles. The scene carries only point positions (no edge
// data), so we derive a travel graph by linking nearby LocationMark/
// GeneralLocation nodes, then draw each robot→destination route as the lit
// shortest path along that graph instead of a straight line across open floor.

(function () {
  var body = document.body;
  var dashboardId = body.getAttribute('data-dashboard-id');
  var SVGNS = 'http://www.w3.org/2000/svg';

  // ── status / state palettes (kept in sync with dashboard.css) ──────
  var STATUS_COLOR = {
    in_transit: '#4f9bff', staged: '#e3b341', dispatched: '#3a7fd0',
    blocked: '#c66bff', acknowledged: '#8b949e', queued: '#a371f7',
    pending: '#8b949e', delivered: '#3fb950'
  };
  var STATE_COLOR = {
    ready: '#3fb950', busy: '#4f9bff', paused: '#e3b341',
    error: '#f85149', offline: '#6e7681'
  };

  // ── state ──────────────────────────────────────────────────────────
  var points = [];          // scene points (static layout)
  var nodeIndex = {};       // lowercased node name -> {x, y} (world space)
  var robots = {};          // vehicle_id -> normalized robot
  var orders = [];          // scoped active orders
  var orderByRobot = {};    // robot_id -> order
  var hotNodes = {};        // lowercased node name -> status (highlight)
  var view = null;          // {minX, minY, w, h} screen-space bounding box
  var rotate90 = false;     // orient the plant's long axis along screen X

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

  function renderLegend() {
    var el = document.getElementById('map-legend');
    if (!el) return;
    var items = [
      ['In transit', STATUS_COLOR.in_transit], ['Staged', STATUS_COLOR.staged],
      ['Blocked', STATUS_COLOR.blocked], ['Idle robot', STATE_COLOR.ready],
      ['Error', STATE_COLOR.error]
    ];
    el.innerHTML = items.map(function (it) {
      return '<span class="map-legend-item"><span class="map-legend-dot" style="background:' +
        it[1] + '"></span>' + it[0] + '</span>';
    }).join('');
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
      station: r.station || r.CurrentStation || ''
    };
  }

  // ── coordinate framing: screen = (x, -y) ───────────────────────────
  function computeView() {
    var wx = [], wy = [];
    points.forEach(function (p) {
      if (isFinite(p.pos_x) && isFinite(p.pos_y)) { wx.push(p.pos_x); wy.push(p.pos_y); }
    });
    Object.keys(robots).forEach(function (k) {
      var r = robots[k];
      if (isFinite(r.x) && isFinite(r.y)) { wx.push(r.x); wy.push(r.y); }
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
    points.forEach(function (p) {
      if (!isFinite(p.pos_x) || !isFinite(p.pos_y)) return;
      var world = { x: p.pos_x, y: p.pos_y };
      [p.point_name, p.label, p.instance_name].forEach(function (n) {
        if (n) nodeIndex[String(n).toLowerCase()] = world;
      });
    });
    buildClassColors();
    buildGraph();
  }
  function findNode(name) {
    if (!name) return null;
    return nodeIndex[String(name).toLowerCase()] || null;
  }

  // ── travel graph (derived — the scene carries no edge data) ─────────
  // Robots drive a fixed network of waypoints, but the scene API only gives us
  // point positions. We reconstruct the aisle graph from the travel nodes
  // (LocationMark / GeneralLocation) in two steps:
  //
  //   1. Candidate links: each node to its neighbours within an adaptive
  //      distance threshold (× the median nearest-neighbour gap).
  //   2. Relative-neighbourhood prune: drop a candidate edge A–C whenever some
  //      node B is closer to BOTH A and C than they are to each other. That
  //      removes every "hypotenuse" — the diagonal shortcut across a corner or
  //      the long skip past an intermediate waypoint — so a route is forced to
  //      step node-to-node along the aisle instead of cutting across open floor.
  //
  // Routes are then the shortest path on this pruned graph.
  var GRAPH_K = 6;          // safety cap on links per node (RNG is already sparse)
  var GRAPH_THRESH = 2.2;   // × median nearest-neighbour distance (candidate gather)
  var tnodes = [];          // [{x, y}] world coords of travel nodes
  var tadj = [];            // adjacency: tadj[i] = [{n, w}]
  var routeCache = {};      // "s:d" -> array of tnode indices (cleared on rebuild)

  function isTravel(cls) { return cls === 'LocationMark' || cls === 'GeneralLocation'; }

  function buildGraph() {
    tnodes = []; tadj = []; routeCache = {};
    points.forEach(function (p) {
      if (!isFinite(p.pos_x) || !isFinite(p.pos_y)) return;
      if (isTravel(classOf(p))) tnodes.push({ x: p.pos_x, y: p.pos_y });
    });
    var n = tnodes.length;
    if (n < 2) return;
    // nearest-neighbour distance per node -> median sets the link threshold.
    var nn = [];
    for (var i = 0; i < n; i++) {
      var best = Infinity;
      for (var j = 0; j < n; j++) {
        if (i === j) continue;
        var d = dist2(tnodes[i], tnodes[j]);
        if (d < best) best = d;
      }
      nn.push(Math.sqrt(best));
    }
    var sorted = nn.slice().sort(function (a, b) { return a - b; });
    var median = sorted[Math.floor(sorted.length / 2)] || 1;
    var thresh2 = Math.pow(median * GRAPH_THRESH, 2);
    // Step 1: candidate neighbours within threshold, nearest first.
    var cand = [];
    for (var a = 0; a < n; a++) {
      var list = [];
      for (var b = 0; b < n; b++) {
        if (a === b) continue;
        var dd = dist2(tnodes[a], tnodes[b]);
        if (dd <= thresh2) list.push({ n: b, d2: dd });
      }
      list.sort(function (p, q) { return p.d2 - q.d2; });
      cand[a] = list;
    }
    // Step 2: relative-neighbourhood prune, blockers drawn from the candidate
    // pools of both endpoints (the blocker is always a near node).
    for (var u = 0; u < n; u++) {
      var keep = [], cu = cand[u];
      for (var c = 0; c < cu.length; c++) {
        var v = cu[c].n, duv2 = cu[c].d2, blocked = false;
        var pool = cu.concat(cand[v] || []);
        for (var k = 0; k < pool.length; k++) {
          var w = pool[k].n;
          if (w === u || w === v) continue;
          if (dist2(tnodes[u], tnodes[w]) < duv2 && dist2(tnodes[w], tnodes[v]) < duv2) { blocked = true; break; }
        }
        if (!blocked) keep.push({ n: v, w: Math.sqrt(duv2) });
      }
      tadj[u] = keep.slice(0, GRAPH_K);
    }
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

  // ── render (coalesced via rAF) ─────────────────────────────────────
  var dirty = false;
  function scheduleRender() {
    if (dirty) return;
    dirty = true;
    requestAnimationFrame(function () { dirty = false; render(); });
  }

  function drawNode(svg, p, nodeR) {
    var s = proj(p.pos_x, p.pos_y);
    var cls = classOf(p);
    var hot = hotNodes[String(p.point_name || '').toLowerCase()] ||
      hotNodes[String(p.label || '').toLowerCase()] ||
      hotNodes[String(p.instance_name || '').toLowerCase()];
    if (isTravel(cls)) {
      // The numerous travel waypoints recede to a faint dot network.
      svg.appendChild(svgEl('circle', { cx: s[0], cy: s[1], r: nodeR * 0.6, class: 'map-node-travel' }));
    } else if (cls === 'ActionPoint') {
      svg.appendChild(svgEl('circle', {
        cx: s[0], cy: s[1], r: nodeR * 1.5, class: 'map-node-action',
        fill: 'none', stroke: '#587aa6', 'stroke-width': nodeR * 0.45
      }));
    } else if (cls === 'ChargePoint') {
      svg.appendChild(svgEl('circle', {
        cx: s[0], cy: s[1], r: nodeR * 1.3, class: 'map-node-charge',
        fill: 'none', stroke: '#2f8f48', 'stroke-width': nodeR * 0.45
      }));
    } else if (cls === 'ParkPoint') {
      var sq = nodeR * 1.2;
      svg.appendChild(svgEl('rect', {
        x: s[0] - sq, y: s[1] - sq, width: sq * 2, height: sq * 2, rx: nodeR * 0.4,
        class: 'map-node-park', fill: 'none', stroke: '#b0723a', 'stroke-width': nodeR * 0.45
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
    computeView();
    if (!view) {
      host.innerHTML = '';
      if (empty) empty.style.display = points.length ? 'none' : 'block';
      return;
    }
    if (empty) empty.style.display = 'none';

    var unit = Math.max(view.w, view.h);
    var nodeR = unit * 0.006;
    var robotR = unit * 0.016;
    var fontS = unit * 0.013;

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

    // nodes — receded travel dots + distinct outlined waypoint shapes.
    points.forEach(function (p) {
      if (!isFinite(p.pos_x) || !isFinite(p.pos_y)) return;
      drawNode(svg, p, nodeR);
    });

    // routes: robot -> destination, lit along the aisle network when possible.
    orders.forEach(function (o) {
      var r = robots[o.robot_id];
      if (!r || !isFinite(r.x) || !isFinite(r.y)) return;
      var dest = findNode(o.delivery_node);
      if (!dest) return;
      var color = STATUS_COLOR[o.status] || '#888';
      var world = routeWorld(r.x, r.y, dest);
      if (world) {
        var pts = world.map(function (w) { var s = proj(w[0], w[1]); return s[0] + ',' + s[1]; }).join(' ');
        // pathLength normalizes the route so the CSS dash pattern (period 2)
        // renders at a consistent physical size on any map scale: one dash
        // unit = 0.6 × robotR world units.
        var len = 0;
        for (var wi = 1; wi < world.length; wi++) {
          len += Math.sqrt(Math.pow(world[wi][0] - world[wi - 1][0], 2) + Math.pow(world[wi][1] - world[wi - 1][1], 2));
        }
        var plen = Math.max(2, Math.round(len / (robotR * 0.6)));
        svg.appendChild(svgEl('polyline', {
          points: pts, class: 'map-route-base', fill: 'none', stroke: color, 'stroke-width': robotR * 0.22
        }));
        svg.appendChild(svgEl('polyline', {
          points: pts, class: 'map-route-flow', fill: 'none', stroke: color, 'stroke-width': robotR * 0.22, pathLength: plen
        }));
      } else {
        // graph couldn't connect them — fall back to a straight hint line.
        var rs = proj(r.x, r.y), ds = proj(dest.x, dest.y);
        svg.appendChild(svgEl('line', {
          x1: rs[0], y1: rs[1], x2: ds[0], y2: ds[1],
          class: 'map-route-base', stroke: color, 'stroke-width': robotR * 0.22
        }));
      }
      // destination marker ring.
      var dp = proj(dest.x, dest.y);
      svg.appendChild(svgEl('circle', { cx: dp[0], cy: dp[1], r: robotR * 0.7, fill: 'none', stroke: color, 'stroke-width': robotR * 0.14, 'stroke-opacity': 0.65 }));
    });

    // robots — halo, then chevron, so labels (last pass) sit above everything.
    var robotList = Object.keys(robots).map(function (k) { return robots[k]; })
      .filter(function (r) { return isFinite(r.x) && isFinite(r.y); });
    robotList.forEach(function (r) {
      var s = proj(r.x, r.y);
      var ord = orderByRobot[r.id];
      var color = ord ? (STATUS_COLOR[ord.status] || STATE_COLOR[r.state]) : (STATE_COLOR[r.state] || '#888');
      svg.appendChild(svgEl('circle', { cx: s[0], cy: s[1], r: robotR * 1.7, class: 'map-robot-halo', fill: color }));
      // Fleet Angle is radians (confirmed live); SVG rotate wants degrees.
      var rot = -(r.angle * 180 / Math.PI) + (rotate90 ? 90 : 0);
      var g = svgEl('g', { transform: 'translate(' + s[0] + ',' + s[1] + ') rotate(' + rot + ')' });
      g.appendChild(svgEl('polygon', { points: chevronPoints(robotR), class: 'map-robot', fill: color, 'stroke-width': robotR * 0.16 }));
      svg.appendChild(g);
      svg.appendChild(svgEl('circle', { cx: s[0], cy: s[1], r: robotR * 0.22, class: 'map-robot-core' }));
    });

    // Second pass: name chips with greedy downward de-collision, so a cluster of
    // robots parked on top of each other reads as a vertical list of names.
    var placed = [];
    robotList.forEach(function (r) {
      var s = proj(r.x, r.y);
      var lx = s[0], ly = s[1] - robotR * 2.0;
      var guard = 0;
      while (guard++ < 16 && placed.some(function (p) {
        return Math.abs(p.x - lx) < fontS * 4.0 && Math.abs(p.y - ly) < fontS * 1.25;
      })) { ly += fontS * 1.35; }
      placed.push({ x: lx, y: ly });
      var ord = orderByRobot[r.id];
      var color = ord ? (STATUS_COLOR[ord.status] || STATE_COLOR[r.state]) : (STATE_COLOR[r.state] || '#888');
      var halfW = (r.id.length * fontS * 0.62) / 2 + fontS * 0.9;
      var chipH = fontS * 1.5;
      svg.appendChild(svgEl('rect', {
        x: lx - halfW, y: ly - chipH * 0.78, width: halfW * 2, height: chipH, rx: fontS * 0.3,
        class: 'map-chip', stroke: color, 'stroke-width': fontS * 0.07
      }));
      svg.appendChild(svgEl('circle', { cx: lx - halfW + fontS * 0.55, cy: ly - chipH * 0.05, r: fontS * 0.22, fill: color }));
      var label = svgEl('text', { x: lx + fontS * 0.4, y: ly, class: 'map-robot-label', 'font-size': fontS });
      label.textContent = r.id;
      svg.appendChild(label);
    });

    host.innerHTML = '';
    host.appendChild(svg);
    renderClassLegend();
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

  function renderClassLegend() {
    var el = document.getElementById('map-class-legend');
    if (!el) return;
    var names = Object.keys(classColors);
    el.innerHTML = names.map(function (n) {
      return '<span class="map-legend-item"><span class="map-legend-dot" style="background:' +
        classColors[n] + '"></span>' + escapeText(prettyClass(n)) + '</span>';
    }).join('');
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

  function loadRobots() {
    return fetch('/api/robots').then(function (r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    }).then(function (data) {
      (data || []).forEach(function (raw) {
        var rb = normRobot(raw);
        if (rb.id) robots[rb.id] = rb;
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

  // ── SSE ────────────────────────────────────────────────────────────
  var es = null, reconnectDelay = 2000, MAX_DELAY = 30000, seenBuild = null;

  function checkBuild(e) {
    var build = '';
    try { build = (JSON.parse(e.data) || {}).build || ''; } catch (_) {}
    if (!build) return;
    if (seenBuild === null) seenBuild = build;
    else if (seenBuild !== build) location.reload();
  }

  function onRobotUpdate(e) {
    var list;
    try { list = JSON.parse(e.data); } catch (_) { return; }
    if (!Array.isArray(list)) list = [list];
    list.forEach(function (raw) {
      var rb = normRobot(raw);
      if (rb.id) robots[rb.id] = rb;
    });
    scheduleRender();
  }

  function connect() {
    if (es) { es.close(); es = null; }
    es = new EventSource('/events');
    es.addEventListener('connected', function (e) {
      setConnected(true);
      reconnectDelay = 2000;
      checkBuild(e);
      // Refresh everything on (re)connect — covers data missed while down.
      Promise.all([loadPoints().catch(noop), loadRobots().catch(noop), loadOrders().catch(noop)])
        .then(scheduleRender);
    });
    es.addEventListener('robot-update', onRobotUpdate);
    es.addEventListener('order-update', scheduleOrderReload);
    es.addEventListener('heartbeat', function (e) { setConnected(true); checkBuild(e); });
    es.onerror = function () {
      setConnected(false);
      if (es) { es.close(); es = null; }
      setTimeout(connect, reconnectDelay);
      reconnectDelay = Math.min(reconnectDelay * 2, MAX_DELAY);
    };
  }

  function init() {
    renderLegend();
    // Initial paint from REST so the board isn't blank before the first SSE tick.
    Promise.all([loadPoints().catch(noop), loadRobots().catch(noop), loadOrders().catch(noop)])
      .then(scheduleRender);
    connect();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
