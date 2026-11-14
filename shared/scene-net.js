// scene-net.js — one scene's drivable network, read once: the stored rows by
// the direction a robot may drive them, the physical lanes a map draws, and the
// route model over the rows.
//
// THE SCENE IS ComposerMap's SHAPE (shingo-edge/domain/composer_view.go):
//   points  name -> {x, y, class}
//   edges   [{from, to, h?: [c1x, c1y, c2x, c2y]}]
// one edge per stored row, in the direction a robot may drive it, with its
// handles (all four or none) in that direction. The Edge's composer read
// carries it; Core adapts its /api/map/edges rows to it once per load
// (dashboard-map.js sceneOfRows). `len` is not read here: a length is measured
// from the points and the handles.
//
// READ ONCE PER SCENE. sceneNet(scene) builds the network the first time it is
// handed a scene object and hands back that same network after, from a WeakMap
// keyed by the object: a page holding two scenes holds two networks, and a
// scene let go is a network let go. So a scene handed here is not changed
// afterwards; a new load is a new object.
//
// WHAT COUNTS AS A ROW. An edge whose ends are both placed points (finite x
// and y), two different points at two different places: a row whose ends
// coincide is no hop and no lane, as Core's map has always dropped it. Of two
// rows stored the same way between the same two points the FIRST is the row,
// the one weighed, routed and drawn; Core's wire to the Edge keeps the first
// too (messaging/scene_slices.go projectSceneEdges). Handles count only as four
// finite numbers.
//
// THE ROUTE MODEL IS FOR INTENT LINES AND THE KEY-ROUTE PICKER ONLY (lead
// decision D9). A robot that is driving draws the path RDS planned for it
// (scene-path.js pathWorld), never this: the two agreed on the three drives
// compared on 2026-11-14, which is three samples, not RDS's cost function.

import { dist2, isCoord, cubicLength, laneKey } from '/static/shared/scene-geom.js';

var nets = new WeakMap();

function placed(p) { return !!p && isCoord(p.x) && isCoord(p.y); }

function handlesOf(h) {
    if (!h || h.length !== 4) return null;
    for (var i = 0; i < 4; i++) if (!isCoord(h[i])) return null;
    return [h[0], h[1], h[2], h[3]];
}

function has(map, k) { return Object.prototype.hasOwnProperty.call(map, k); }

// sceneNet is the network of `scene`. Its fields:
//   names     the vertices: every point an edge names at both of its placed
//             ends, in the order the edges first name them (from, then to);
//             a row later dropped still names its ends, as Core's graph did
//   at        name -> {x, y}, a vertex's place (the scene's point)
//   hop(a, b) the row a robot drives from a to b, {from, to, h, len}, or null
//   rows      the kept rows, in stored order
//   lanes     each physical lane once, as Core's aisles have always drawn it:
//             [{from, to, a: {x, y}, b: {x, y}, h}] (see laneList)
//   scale     the median row length, every kept row counted (Core's
//             graphScale, which sizes the map's markers)
//   resolve(name)  the scene's own spelling of a point name, matched exactly
//             and then without case (RDS names keep their case, Core's map
//             always matched without it), or ''
export function sceneNet(scene) {
    if (!scene || typeof scene !== 'object') return buildNet({ points: {}, edges: [] });
    var net = nets.get(scene);
    if (!net) { net = buildNet(scene); nets.set(scene, net); }
    return net;
}

function buildNet(scene) {
    var points = scene.points || {}, edges = scene.edges || [];
    var names = [], index = Object.create(null), at = Object.create(null);
    var hops = Object.create(null), out = Object.create(null), into = Object.create(null);
    var outOrder = [], inOrder = [], rows = [], lens = [], link = Object.create(null);
    var lower = Object.create(null);
    Object.keys(points).forEach(function (n) {
        var k = n.toLowerCase();
        if (!(k in lower)) lower[k] = n;
    });
    function vertex(n) {
        if (n in index) return;
        index[n] = names.length;
        names.push(n);
        at[n] = { x: points[n].x, y: points[n].y };
    }
    // Both ways round, the first row that joins two vertices in either
    // direction: the undirected view the lanes are drawn from.
    function lanePeer(a, b, h) {
        var l = link[a] || (link[a] = []);
        for (var i = 0; i < l.length; i++) if (l[i].n === b) return;
        l.push({ n: b, h: h });
    }
    edges.forEach(function (e) {
        if (!e || !has(points, e.from) || !has(points, e.to)) return;
        var p0 = points[e.from], p3 = points[e.to];
        if (!placed(p0) || !placed(p3)) return;
        vertex(e.from);
        vertex(e.to);
        if (e.from === e.to) return;
        var h = handlesOf(e.h);
        var len = h ? cubicLength(at[e.from], h, at[e.to]) : Math.sqrt(dist2(at[e.from], at[e.to]));
        if (!(len > 0)) return;
        lens.push(len);
        var key = e.from + '\u0000' + e.to;
        if (!(key in hops)) {
            var row = { from: e.from, to: e.to, h: h, len: len };
            hops[key] = row;
            rows.push(row);
            if (!(e.from in out)) { out[e.from] = []; outOrder.push(e.from); }
            out[e.from].push(row);
            if (!(e.to in into)) { into[e.to] = true; inOrder.push(e.to); }
        }
        lanePeer(e.from, e.to, h);
        lanePeer(e.to, e.from, h && [h[2], h[3], h[0], h[1]]);
    });
    lens.sort(function (a, b) { return a - b; });
    return {
        names: names,
        at: at,
        rows: rows,
        lanes: laneList(names, index, at, link),
        scale: lens[Math.floor(lens.length / 2)] || 0,
        hop: function (a, b) { return hops[a + '\u0000' + b] || null; },
        resolve: function (n) {
            if (typeof n !== 'string' || !n) return '';
            if (has(points, n)) return n;
            return lower[n.toLowerCase()] || '';
        },
        // The route model's own tables (sceneRoute, below).
        out: out, into: into, outOrder: outOrder, inOrder: inOrder, points: points,
        routes: Object.create(null)
    };
}

// laneList is every physical lane once, in the order Core's drawAisles has
// always drawn them: by vertex, in `names` order, then by the order that
// vertex's rows were stored, each lane first met at its earlier vertex and
// drawn from it, with the handles of the first row stored between the two in
// either direction, turned to run from that vertex when that row runs the other
// way. laneKey over the two vertices' positions in `names` is the identity, so
// a lane stored one way only, in either direction, is still one lane: the
// aisle drawing gets every physical lane exactly once.
function laneList(names, index, at, link) {
    var lanes = [], seen = Object.create(null);
    for (var i = 0; i < names.length; i++) {
        var peers = link[names[i]] || [];
        for (var k = 0; k < peers.length; k++) {
            var key = laneKey(i, index[peers[k].n]);
            if (seen[key]) continue;
            seen[key] = true;
            lanes.push({ from: names[i], to: peers[k].n, a: at[names[i]], b: at[peers[k].n], h: peers[k].h });
        }
    }
    return lanes;
}

// nearestVertex is the vertex nearest (x, y) by placement, the first of equals
// in `names` order, or '' when there is none. Placement only: no direction.
export function nearestVertex(scene, x, y) {
    var net = sceneNet(scene), best = '', bd = Infinity, q = { x: x, y: y };
    for (var i = 0; i < net.names.length; i++) {
        var d = dist2(net.at[net.names[i]], q);
        if (d < bd) { bd = d; best = net.names[i]; }
    }
    return best;
}

// sceneRoute is the route model (lead decision D9): the shortest way from
// `from` to `to` over the stored rows, in their stored direction only, each row
// weighed by the distance driven along it (the cubic's arc on a curved row, the
// chord on a straight one). It answers the vertex names in driving order, both
// ends included, or null: no way there, an end that is no place, or the same
// vertex at both ends.
//
// AN END is a point name or a place {x, y}. A name that is a vertex with a row
// leaving it is itself as the start, and one with a row arriving is itself as
// the destination; any other name is its point's place. A place snaps to the
// nearest vertex with a row leaving it (the start) or arriving at it (the
// destination). Of two at the same distance the one whose first such row is
// stored first wins.
//
// THE DESKTOP's ROUTER, WITH ONE FLAT BUG FIXED. processes-desktop.js
// travelGraph/graphNodeFor/route is this model, pinned by
// TestProcessesDesktopRouteJS on the route scene, and these answers are those
// pins' answers. Except: the desktop snaps both ends over the vertices with a
// row LEAVING them, so a destination only rows arrive at (a dead end) snapped
// away from itself to its nearest neighbour with a way out. Here a destination
// snaps over the vertices rows arrive at. Pinned with the desktop's vectors in
// shared/testdata/js/scene-net.test.js.
//
// Dijkstra over a binary heap ordered by distance and then by the order a
// vertex was reached, which is the order the desktop's stable-sorted queue
// pops in, so a tie between two equal routes is broken as the desktop broke
// it. A route is worked out once per network and remembered.
export function sceneRoute(scene, from, to) {
    var net = sceneNet(scene);
    var a = endOf(net, from, true), b = endOf(net, to, false);
    if (!a || !b || a === b) return null;
    var key = a + '\u0000' + b;
    if (!(key in net.routes)) net.routes[key] = shortest(net, a, b);
    var r = net.routes[key];
    return r ? r.slice() : null;
}

function endOf(net, end, isStart) {
    var place = null;
    if (typeof end === 'string') {
        var n = net.resolve(end);
        if (!n) return '';
        if (isStart ? (n in net.out) : (n in net.into)) return n;
        place = net.points[n];
    } else {
        place = end;
    }
    if (!placed(place)) return '';
    var pool = isStart ? net.outOrder : net.inOrder, best = '', bd = Infinity;
    for (var i = 0; i < pool.length; i++) {
        var d = dist2(net.at[pool[i]], place);
        if (d < bd) { bd = d; best = pool[i]; }
    }
    return best;
}

function shortest(net, a, b) {
    var dist = Object.create(null), prev = Object.create(null), done = Object.create(null);
    var heap = [], seq = 0;
    function before(x, y) { return x[0] < y[0] || (x[0] === y[0] && x[1] < y[1]); }
    function push(item) {
        var i = heap.push(item) - 1;
        while (i > 0) {
            var p = (i - 1) >> 1;
            if (!before(heap[i], heap[p])) break;
            var t = heap[i]; heap[i] = heap[p]; heap[p] = t; i = p;
        }
    }
    function pop() {
        var top = heap[0], last = heap.pop();
        if (heap.length) {
            heap[0] = last;
            var i = 0;
            for (;;) {
                var l = 2 * i + 1, r = l + 1, m = i;
                if (l < heap.length && before(heap[l], heap[m])) m = l;
                if (r < heap.length && before(heap[r], heap[m])) m = r;
                if (m === i) break;
                var t = heap[i]; heap[i] = heap[m]; heap[m] = t; i = m;
            }
        }
        return top;
    }
    dist[a] = 0;
    push([0, seq++, a]);
    while (heap.length) {
        var head = pop(), d = head[0], u = head[2];
        if (u === b) break;
        if (done[u]) continue;
        done[u] = true;
        var rows = net.out[u] || [];
        for (var i = 0; i < rows.length; i++) {
            var nd = d + rows[i].len, v = rows[i].to;
            if (!(v in dist) || nd < dist[v]) {
                dist[v] = nd;
                prev[v] = u;
                push([nd, seq++, v]);
            }
        }
    }
    if (!(b in dist)) return null;
    var path = [b];
    for (var u2 = b; u2 !== a;) { u2 = prev[u2]; path.unshift(u2); }
    return path;
}
