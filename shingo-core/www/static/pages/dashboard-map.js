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
    blocked: '#f85149', acknowledged: '#8b949e', queued: '#a371f7',
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
    var xs = [], ys = [];
    points.forEach(function (p) {
      if (isFinite(p.pos_x) && isFinite(p.pos_y)) { xs.push(p.pos_x); ys.push(-p.pos_y); }
    });
    Object.keys(robots).forEach(function (k) {
      var r = robots[k];
      if (isFinite(r.x) && isFinite(r.y)) { xs.push(r.x); ys.push(-r.y); }
    });
    if (!xs.length) { view = null; return; }
    var minX = Math.min.apply(null, xs), maxX = Math.max.apply(null, xs);
    var minY = Math.min.apply(null, ys), maxY = Math.max.apply(null, ys);
    var w = Math.max(maxX - minX, 1), h = Math.max(maxY - minY, 1);
    var pad = Math.max(w, h) * 0.06;
    view = { minX: minX - pad, minY: minY - pad, w: w + 2 * pad, h: h + 2 * pad };
  }

  function buildNodeIndex() {
    nodeIndex = {};
    points.forEach(function (p) {
      if (!isFinite(p.pos_x) || !isFinite(p.pos_y)) return;
      var screen = { x: p.pos_x, y: -p.pos_y };
      [p.point_name, p.label, p.instance_name].forEach(function (n) {
        if (n) nodeIndex[String(n).toLowerCase()] = screen;
      });
    });
  }
  function findNode(name) {
    if (!name) return null;
    return nodeIndex[String(name).toLowerCase()] || null;
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
    var nodeR = unit * 0.004;
    var robotR = unit * 0.012;
    var fontS = unit * 0.014;

    var svg = svgEl('svg', {
      class: 'map-svg',
      viewBox: view.minX + ' ' + view.minY + ' ' + view.w + ' ' + view.h,
      preserveAspectRatio: 'xMidYMid meet'
    });

    // nodes (highlighted if they're a source/destination of a scoped order)
    points.forEach(function (p) {
      if (!isFinite(p.pos_x) || !isFinite(p.pos_y)) return;
      var hot = hotNodes[String(p.point_name || '').toLowerCase()] ||
        hotNodes[String(p.label || '').toLowerCase()] ||
        hotNodes[String(p.instance_name || '').toLowerCase()];
      svg.appendChild(svgEl('circle', {
        cx: p.pos_x, cy: -p.pos_y, r: hot ? nodeR * 2.2 : nodeR,
        class: 'map-node' + (hot ? ' map-node-hot' : ''),
        fill: hot ? (STATUS_COLOR[hot] || '#fff') : null
      }));
    });

    // routes: robot -> destination, when both are placeable
    orders.forEach(function (o) {
      var r = robots[o.robot_id];
      if (!r || !isFinite(r.x) || !isFinite(r.y)) return;
      var dest = findNode(o.delivery_node);
      if (!dest) return;
      svg.appendChild(svgEl('line', {
        x1: r.x, y1: -r.y, x2: dest.x, y2: dest.y,
        class: 'map-route', stroke: STATUS_COLOR[o.status] || '#888',
        'stroke-width': robotR * 0.35
      }));
    });

    // robots (order-status color if on a scoped order, else state color)
    Object.keys(robots).forEach(function (k) {
      var r = robots[k];
      if (!isFinite(r.x) || !isFinite(r.y)) return;
      var ord = orderByRobot[r.id];
      var color = ord ? (STATUS_COLOR[ord.status] || STATE_COLOR[r.state]) : (STATE_COLOR[r.state] || '#888');
      var g = svgEl('g', { transform: 'translate(' + r.x + ',' + (-r.y) + ') rotate(' + (-r.angle) + ')' });
      g.appendChild(svgEl('polygon', { points: triPoints(robotR), class: 'map-robot', fill: color }));
      svg.appendChild(g);
      var label = svgEl('text', {
        x: r.x, y: -r.y - robotR * 1.5, class: 'map-robot-label', 'font-size': fontS
      });
      label.textContent = r.id;
      svg.appendChild(label);
    });

    host.innerHTML = '';
    host.appendChild(svg);
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
