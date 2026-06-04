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
// Color: a robot working one of THIS dashboard's active orders takes the order's
// status color (the "highlight active orders" signal); otherwise it shows its
// own state color. Order destination nodes are highlighted and a route line is
// drawn robot→destination when the node name resolves to a scene point.

(function () {
  var body = document.body;
  var dashboardId = body.getAttribute('data-dashboard-id');
  var SVGNS = 'http://www.w3.org/2000/svg';

  // ── status / state palettes (kept in sync with dashboard.css) ──────
  var STATUS_COLOR = {
    in_transit: '#2f81f7', staged: '#d29922', dispatched: '#1f6feb',
    blocked: '#bf5af2', acknowledged: '#8b949e', queued: '#a371f7',
    pending: '#8b949e', delivered: '#2ea043'
  };
  var STATE_COLOR = {
    ready: '#2ea043', busy: '#2f81f7', paused: '#d29922',
    error: '#f85149', offline: '#6e7681'
  };

  // ── state ──────────────────────────────────────────────────────────
  var points = [];          // scene points (static layout)
  var nodeIndex = {};       // lowercased node name -> {x, y} (screen space)
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
    var pad = Math.max(w, h) * 0.04;
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
  }
  function findNode(name) {
    if (!name) return null;
    return nodeIndex[String(name).toLowerCase()] || null;
  }

  // ── node classes (e.g. advanced/action points vs bin locations) ────
  // Color nodes by their scene ClassName so the layout reads as typed
  // locations, not anonymous dots. Built dynamically: whatever class_name
  // values the scene carries get a stable color from the palette + a legend
  // entry, so this works without hard-coding the fleet's class strings.
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
  // Size emphasis by class: operational waypoints (action/charge/park) read
  // larger; the numerous travel nodes (LocationMark) stay small so they read as
  // a background path network rather than drowning the waypoints.
  function classScale(cls) {
    if (cls === 'ActionPoint') return 2.2;
    if (cls === 'ChargePoint' || cls === 'ParkPoint') return 1.4;
    return 0.7; // LocationMark / GeneralLocation (travel) — recede
  }

  function svgEl(name, attrs) {
    var e = document.createElementNS(SVGNS, name);
    for (var k in attrs) if (attrs[k] !== undefined && attrs[k] !== null && attrs[k] !== '') e.setAttribute(k, attrs[k]);
    return e;
  }

  function triPoints(s) {
    // triangle pointing +X (heading 0), centered at origin
    return s + ',0 ' + (-s * 0.8) + ',' + (s * 0.7) + ' ' + (-s * 0.8) + ',' + (-s * 0.7);
  }

  // ── render (coalesced via rAF) ─────────────────────────────────────
  var dirty = false;
  function scheduleRender() {
    if (dirty) return;
    dirty = true;
    requestAnimationFrame(function () { dirty = false; render(); });
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
    var nodeR = unit * 0.005;
    var robotR = unit * 0.012;
    var fontS = unit * 0.011;

    var svg = svgEl('svg', {
      class: 'map-svg',
      viewBox: view.minX + ' ' + view.minY + ' ' + view.w + ' ' + view.h,
      preserveAspectRatio: 'xMidYMid meet'
    });

    // nodes — colored by scene class so location types are distinguishable;
    // larger + status-colored when they're a source/destination of a scoped order.
    points.forEach(function (p) {
      if (!isFinite(p.pos_x) || !isFinite(p.pos_y)) return;
      var s = proj(p.pos_x, p.pos_y);
      var cls = classOf(p);
      var travel = (cls === 'LocationMark' || cls === 'GeneralLocation');
      var hot = hotNodes[String(p.point_name || '').toLowerCase()] ||
        hotNodes[String(p.label || '').toLowerCase()] ||
        hotNodes[String(p.instance_name || '').toLowerCase()];
      var rad = hot ? nodeR * 2.4 : nodeR * classScale(cls);
      // Travel nodes (the numerous LocationMarks) recede to a faint path
      // network; waypoints, charge/park, and highlights stay opaque.
      svg.appendChild(svgEl('circle', {
        cx: s[0], cy: s[1], r: rad,
        class: 'map-node' + (hot ? ' map-node-hot' : ''),
        fill: hot ? (STATUS_COLOR[hot] || '#fff') : (classColors[cls] || '#67748f'),
        'fill-opacity': (!hot && travel) ? 0.5 : 1
      }));
    });

    // routes: robot -> destination, when both are placeable
    orders.forEach(function (o) {
      var r = robots[o.robot_id];
      if (!r || !isFinite(r.x) || !isFinite(r.y)) return;
      var dest = findNode(o.delivery_node);
      if (!dest) return;
      var rs = proj(r.x, r.y), ds = proj(dest.x, dest.y);
      svg.appendChild(svgEl('line', {
        x1: rs[0], y1: rs[1], x2: ds[0], y2: ds[1],
        class: 'map-route', stroke: STATUS_COLOR[o.status] || '#888',
        'stroke-width': robotR * 0.3
      }));
    });

    // robots — two passes. First all triangles (order-status color if on a
    // scoped order, else state color), so labels always sit above every marker.
    var robotList = Object.keys(robots).map(function (k) { return robots[k]; })
      .filter(function (r) { return isFinite(r.x) && isFinite(r.y); });
    robotList.forEach(function (r) {
      var s = proj(r.x, r.y);
      var ord = orderByRobot[r.id];
      var color = ord ? (STATUS_COLOR[ord.status] || STATE_COLOR[r.state]) : (STATE_COLOR[r.state] || '#888');
      // Fleet Angle is radians (confirmed live); SVG rotate wants degrees.
      var rot = -(r.angle * 180 / Math.PI) + (rotate90 ? 90 : 0);
      var g = svgEl('g', { transform: 'translate(' + s[0] + ',' + s[1] + ') rotate(' + rot + ')' });
      g.appendChild(svgEl('polygon', { points: triPoints(robotR), class: 'map-robot', fill: color, 'stroke-width': robotR * 0.18 }));
      svg.appendChild(g);
    });
    // Second pass: labels with greedy downward de-collision, so a cluster of
    // robots parked on top of each other reads as a vertical list of names
    // rather than an unreadable pile.
    var placed = [];
    robotList.forEach(function (r) {
      var s = proj(r.x, r.y);
      var lx = s[0], ly = s[1] - robotR * 1.6;
      var guard = 0;
      while (guard++ < 14 && placed.some(function (p) {
        return Math.abs(p.x - lx) < fontS * 3.2 && Math.abs(p.y - ly) < fontS * 1.05;
      })) { ly += fontS * 1.15; }
      placed.push({ x: lx, y: ly });
      var label = svgEl('text', { x: lx, y: ly, class: 'map-robot-label', 'font-size': fontS });
      label.textContent = r.id;
      svg.appendChild(label);
    });

    host.innerHTML = '';
    host.appendChild(svg);
    renderClassLegend();
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
