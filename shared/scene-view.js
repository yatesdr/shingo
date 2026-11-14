// scene-view.js — a scene map's retained <svg>: the grid, the aisles, the point
// glyphs, the route lines and the robots, kept for the life of the page.
//
// Moved whole, 2026-11-14, from Core's pages/dashboard-map.js (the robot map),
// which keeps everything that decides WHAT is drawn (its orders, legs, focus,
// calm rule, legends and rail) and hands this the items. Pinned there, by the
// render pins theWholeSceneAsDrawn (the look, dashboard-map.scene.golden.txt)
// and aFrameWithNothingStaticBuildsNothing and its siblings (what a frame
// costs).
//
// The aisles are a scene's lanes as scene-net.js lists them, each physical
// lane once. Geometry is scene-geom's; this file only spells it as SVG.

import { cubicPathD } from '/static/shared/scene-geom.js';

var SVGNS = 'http://www.w3.org/2000/svg';

// svgNode makes one SVG element with the attributes given, leaving out an
// attribute whose value is undefined, null or ''.
export function svgNode(name, attrs) {
  var e = document.createElementNS(SVGNS, name);
  for (var k in attrs) if (attrs[k] !== undefined && attrs[k] !== null && attrs[k] !== '') e.setAttribute(k, attrs[k]);
  return e;
}

// createSceneView(host, look) keeps ONE <svg> in `host` for the life of the
// page, in four layers in paint order: base (the grid, then the aisles),
// routes (intent lines and frozen lanes), nodes (point glyphs, over the
// routes so they stay legible) and robots (every robot's glyph group, then
// every name chip, so a chip sits over any robot). draw(frame) puts one
// frame on screen:
//
//   - base and nodes are STATIC: each is built only when its key changes.
//     The caller says what a key covers (render: baseKey, nodesKey), since
//     only it knows what its data depends on;
//   - routes and robots are updated IN PLACE: every item has a key, and an
//     element whose key is still there next frame is kept and gets only the
//     attributes that changed. A moving robot's group, halo and chevron are
//     the same elements tick after tick, so the halo's CSS pulse runs on
//     instead of restarting at every render as it did when the whole scene
//     was rebuilt.
//
// It reaches only what it is given: the host and the look (colours) at
// creation (look: flare, accent, accentGlow), and per frame the view, the
// projector, the marker sizes and the items, plus this file's svgNode and
// scene-geom's cubicPathD, all pure.
export function createSceneView(host, look) {
  // routeRecs and robotRecs: syncKids' records of the two in-place layers.
  var svg = null, layers = null, baseKey = null, nodesKey = null, routeRecs = [], robotRecs = [];
  function mount() {
    if (svg && svg.parentNode === host) return;
    host.innerHTML = '';
    svg = svgNode('svg', { class: 'map-svg', preserveAspectRatio: 'xMidYMid meet' });
    layers = {};
    ['base', 'routes', 'nodes', 'robots'].forEach(function (name) {
      layers[name] = svg.appendChild(svgNode('g', { 'data-layer': name }));
    });
    host.appendChild(svg);
    baseKey = null; nodesKey = null; routeRecs = []; robotRecs = [];
  }
  function emptyLayer(g) { while (g.firstChild) g.removeChild(g.firstChild); }
  return {
    // f: view {minX, minY, w, h} and proj (world -> screen); unit, nodeR,
    // robotR, fontS (marker sizes, user units); baseKey and lanes (the
    // travel network: scene-net's lanes, world); nodesKey and nodes (a
    // function, called on a nodes build only, listing drawNodeGlyph's
    // items); routes (routeSpec's items) and robots (robotSpec's and
    // chipSpecs' items).
    draw: function (f) {
      mount();
      var vb = f.view.minX + ' ' + f.view.minY + ' ' + f.view.w + ' ' + f.view.h;
      if (svg.getAttribute('viewBox') !== vb) svg.setAttribute('viewBox', vb);
      if (f.baseKey !== baseKey) {
        baseKey = f.baseKey;
        emptyLayer(layers.base);
        drawBackdrop(layers.base, f.view, f.unit);
        drawAisles(layers.base, f.lanes, f.nodeR, f.proj);
      }
      if (f.nodesKey !== nodesKey) {
        nodesKey = f.nodesKey;
        emptyLayer(layers.nodes);
        f.nodes().forEach(function (n) { drawNodeGlyph(layers.nodes, n, f.nodeR, f.proj, look); });
      }
      routeRecs = syncKids(layers.routes, routeRecs, f.routes.map(function (rt) { return routeSpec(rt, f.robotR); }));
      robotRecs = syncKids(layers.robots, robotRecs, f.robots.map(function (r) { return robotSpec(r, f.robotR, f.proj, look); })
        .concat(chipSpecs(f.robots, f.robotR, f.fontS, f.proj)));
    },
    // clear empties the host and forgets every element, for a frame with
    // nothing to draw; the next draw builds the whole scene again.
    clear: function () { host.innerHTML = ''; svg = null; }
  };
}

// syncKids makes the children of `parent` the elements for `specs`, in
// order, and returns their records. A spec is {key, tag, attrs, text?,
// kids?}; an element whose key (and tag) was there last time is kept and
// patched, a new key gets a new element, and a key that has gone takes its
// element with it. A key may repeat: each spec takes the next record left
// under it. `parent` holds only the elements these records own.
function syncKids(parent, old, specs) {
  var byKey = {}, next = [], i, k;
  for (i = 0; i < old.length; i++) (byKey[old[i].key] = byKey[old[i].key] || []).push(old[i]);
  for (i = 0; i < specs.length; i++) {
    var left = byKey[specs[i].key];
    var prev = left && left.length ? left.shift() : null;
    var rec = patchEl(prev, specs[i]);
    if (prev && rec !== prev) parent.removeChild(prev.el);
    next.push(rec);
  }
  for (k in byKey) byKey[k].forEach(function (gone) { parent.removeChild(gone.el); });
  var at = parent.firstChild;
  for (i = 0; i < next.length; i++) {
    if (next[i].el === at) at = at.nextSibling;
    else parent.insertBefore(next[i].el, at);
  }
  return next;
}

// patchEl brings record `rec` to `spec` and returns it, writing to its
// element only the attributes and text that changed and removing the ones
// the spec no longer has; with no record, or a different tag, it returns a
// new one. Values are filtered and stringified as svgNode does them, so a
// patched element holds exactly what a fresh one would.
function patchEl(rec, spec) {
  if (!rec || rec.tag !== spec.tag) {
    rec = { key: spec.key, tag: spec.tag, el: document.createElementNS(SVGNS, spec.tag), attrs: {}, text: '', kids: [] };
  }
  var want = {}, k, v;
  for (k in spec.attrs) {
    v = spec.attrs[k];
    if (v === undefined || v === null || v === '') continue;
    want[k] = String(v);
    if (rec.attrs[k] !== want[k]) rec.el.setAttribute(k, want[k]);
  }
  for (k in rec.attrs) if (!Object.prototype.hasOwnProperty.call(want, k)) rec.el.removeAttribute(k);
  rec.attrs = want;
  var text = spec.text || '';
  if (text !== rec.text) { rec.el.textContent = text; rec.text = text; }
  rec.kids = syncKids(rec.el, rec.kids, spec.kids || []);
  return rec;
}

// Faint grid — gives the floor a frame so the scene reads as a plant view
// rather than dots in a void.
//
// THE CORNER BRACKETS ARE GONE, and the reason they read badly is the reason
// they looked like a good idea: they were derived from the live view
// (view.minX/minY/w/h) rather than from the plant, so they framed the VIEWPORT
// instead of the floor. A frame that is redrawn at the new edges on every zoom
// and every pan is not a frame — it is four marks that chase the cursor, and
// on an auto-following display it never settles, because the ROI re-frames as
// robots move. They also read as a box the map was clipped to, which is
// exactly what they were not.
//
// The grid stays: it is keyed to WORLD coordinates, so it slides under the
// scene as you pan instead of moving with you, which is what actually conveys
// that the floor is a fixed thing you are looking around.
function drawBackdrop(svg, view, unit) {
  var step = unit * 0.08;
  var gridW = unit * 0.0012; // stroke width in user units — CSS px wouldn't scale
  var x0 = view.minX, y0 = view.minY, x1 = view.minX + view.w, y1 = view.minY + view.h;
  var gx, gy;
  for (gx = Math.ceil(x0 / step) * step; gx < x1; gx += step) {
    svg.appendChild(svgNode('line', { x1: gx, y1: y0, x2: gx, y2: y1, class: 'map-grid', 'stroke-width': gridW }));
  }
  for (gy = Math.ceil(y0 / step) * step; gy < y1; gy += step) {
    svg.appendChild(svgNode('line', { x1: x0, y1: gy, x2: x1, y2: gy, class: 'map-grid', 'stroke-width': gridW }));
  }
}

// drawAisles paints the travel network — the faint connective tissue under
// everything else on the map. lanes is scene-net's list, each physical lane
// once, a lane stored one way only included: [{a: {x, y}, b: {x, y}, h}] in
// world coordinates, h the handles turned to run from a to b, or null.
//
// A lane with handles draws as a cubic <path>; one without stays a <line>.
// Both get the same map-aisle class and the same stroke width, so a bend
// never reads as a different KIND of lane — only as the shape the fleet
// actually drives. <path> needs fill:none explicitly: SVG's default fill is
// black, and a filled aisle would blot the floor.
function drawAisles(svg, lanes, nodeR, proj) {
  for (var i = 0; i < lanes.length; i++) {
    var lane = lanes[i];
    var pa = proj(lane.a.x, lane.a.y), pb = proj(lane.b.x, lane.b.y);
    var ctl = lane.h;
    if (ctl) {
      var q1 = proj(ctl[0], ctl[1]), q2 = proj(ctl[2], ctl[3]);
      svg.appendChild(svgNode('path', {
        d: cubicPathD(pa, q1, q2, pb),
        fill: 'none',
        class: 'map-aisle', 'stroke-width': nodeR * 0.48
      }));
    } else {
      svg.appendChild(svgNode('line', {
        x1: pa[0], y1: pa[1], x2: pb[0], y2: pb[1],
        class: 'map-aisle', 'stroke-width': nodeR * 0.48
      }));
    }
  }
}

// drawNodeGlyph draws one scene point: n is {x, y} (world), kind, fill, ring
// (a charge point's) and hot (an order names it). The caller has already
// left out what is not drawn (nodeFacts).
function drawNodeGlyph(svg, n, nodeR, proj, look) {
  var s = proj(n.x, n.y);
  // The node's PRIMARY glyph, so an active node can take the accent on the
  // glyph itself (P21) rather than via a separate ring drawn on top.
  var glyph;
  if (n.kind === 'action') {
    // Single dot — no ring.
    glyph = svgNode('circle', { cx: s[0], cy: s[1], r: nodeR * 1.0, fill: n.fill });
    svg.appendChild(glyph);
  } else if (n.kind === 'charge') {
    // Thin ring (charge stays identifiable) + faint bolt; smaller than before
    // so a row of chargers reads as a calm strip, not a noisy fence.
    glyph = svgNode('circle', {
      cx: s[0], cy: s[1], r: nodeR * 0.95, class: 'map-node-charge',
      fill: 'none', stroke: n.ring, 'stroke-width': nodeR * 0.22
    });
    svg.appendChild(glyph);
    svg.appendChild(svgNode('polygon', {
      points: boltPoints(s[0], s[1], nodeR * 0.8), fill: n.fill, 'fill-opacity': 0.4
    }));
  } else if (n.kind === 'park') {
    // Single faint dot — no ring. Park bays are plentiful; rings-on-rings made
    // a charge row look identical to a pack of park bays.
    glyph = svgNode('circle', { cx: s[0], cy: s[1], r: nodeR * 0.85, fill: n.fill, 'fill-opacity': 0.55 });
    svg.appendChild(glyph);
  } else {
    glyph = svgNode('circle', { cx: s[0], cy: s[1], r: nodeR * 0.9, fill: n.fill, 'fill-opacity': 0.7 });
    svg.appendChild(glyph);
  }
  // Active node (order source/destination): the glyph itself takes the UI
  // accent — an indigo stroke + soft glow — marking it as live/active (per the
  // guide, the accent marks genuinely live elements). No separate ring; green
  // stays delivered-only.
  if (n.hot) {
    glyph.setAttribute('stroke', look.accent);
    glyph.setAttribute('stroke-width', nodeR * 0.45);
    glyph.setAttribute('style', 'filter: drop-shadow(0 0 ' + (nodeR * 1.1).toFixed(2) + 'px ' + look.accentGlow + ')');
  }
}

// Compact lightning bolt centered on (cx, cy), sized to sit inside a
// charge-point ring.
function boltPoints(cx, cy, s) {
  var p = [[0.12, -0.6], [-0.38, 0.08], [-0.06, 0.08], [-0.12, 0.6], [0.38, -0.08], [0.06, -0.08]];
  return p.map(function (q) {
    return (cx + q[0] * s * 1.4) + ',' + (cy + q[1] * s * 1.4);
  }).join(' ');
}

// routeSpec is one route line: rt is {key, kind, pts (screen), color, dimmed}.
//  - 'intent': the dashed line source→delivery of a pre-dispatch or staged
//    order. No robot is driving that leg, so it gets structure, not motion.
//    (dimmed is never true here today: under focus only the focused robot's
//    intent line is drawn, pinned by clickToFocus.)
//  - 'frozen': a static lane, no flow, drawn where laneVisible says. A
//    blocked/faulted robot is signalled on the ROBOT glyph itself (robotColor
//    → red error state), so its lane draws the SAME neutral frozen treatment —
//    the old loud red route line doubled the signal and was removed (2026-07).
function routeSpec(rt, robotR) {
  var spelled = rt.pts.map(function (p) { return p[0] + ',' + p[1]; }).join(' ');
  if (rt.kind === 'intent') {
    return { key: rt.key, tag: 'polyline', attrs: {
      points: spelled, class: 'map-route-intent', fill: 'none',
      'stroke-width': robotR * 0.16, 'stroke-opacity': rt.dimmed ? 0.18 : 0.34
    } };
  }
  return { key: rt.key, tag: 'polyline', attrs: {
    points: spelled, class: 'map-route-frozen', fill: 'none',
    stroke: rt.color, 'stroke-width': robotR * 0.22, 'stroke-opacity': rt.dimmed ? 0.22 : 0.4
  } };
}

// robotSpec is one robot's glyph group: r is {id, x, y (world), color,
// moving, alert, fault, focused, rot (the chevron's degrees, when moving),
// rest ({x, y, r}, world: the disc, when not), state}. The group carries
// data-robot so a click anywhere on the robot finds it (render's delegated
// handler walks up to it).
function robotSpec(r, robotR, proj, look) {
  var s = proj(r.x, r.y), kids = [];
  // Focus ring on the selected robot — a quiet outline, not another color.
  if (r.focused) {
    kids.push({ key: 'focus', tag: 'circle', attrs: {
      cx: s[0], cy: s[1], r: robotR * 1.8, class: 'map-robot-focus', fill: 'none', 'stroke-width': robotR * 0.12
    } });
  }
  // Fault flare: loud outer ring so a blocked/error robot is the first thing
  // the eye hits. Drawn outside the normal halo so both rings compound.
  if (r.fault) {
    kids.push({ key: 'flare', tag: 'circle', attrs: {
      cx: s[0], cy: s[1], r: robotR * 2.4, class: 'map-robot-fault-flare', fill: look.flare
    } });
  }
  // Halo only where it carries signal: motion or a fault. Parked robots
  // get none, so a charge row reads as a tidy strip of docked units.
  if (r.moving || r.alert) {
    kids.push({ key: 'halo', tag: 'circle', attrs: { cx: s[0], cy: s[1], r: robotR * 1.35, class: 'map-robot-halo', fill: r.color } });
  }
  if (r.moving) {
    // In motion the chevron shows heading. Fleet Angle is radians
    // (confirmed live); SVG rotate wants degrees, which the caller works out
    // (headingDeg). (Open question worth re-checking: the reversed-arrow
    // report was the comet LEG direction — the comet always ran
    // robot→delivery, i.e. backwards on the pickup leg — now fixed via
    // orderLeg. This chevron convention is the second candidate only if
    // reversed CHEVRONS, not streaks, are ever seen again.)
    kids.push({ key: 'heading', tag: 'g', attrs: { transform: 'translate(' + s[0] + ',' + s[1] + ') rotate(' + r.rot + ')' },
      kids: [{ key: 'chevron', tag: 'polygon', attrs: {
        points: chevronPoints(robotR), class: 'map-robot', fill: r.color, 'stroke-width': robotR * 0.16
      } }] });
    kids.push({ key: 'core', tag: 'circle', attrs: { cx: s[0], cy: s[1], r: robotR * 0.22, class: 'map-robot-core' } });
  } else {
    // At rest heading is noise: one solid disc (restDisc), so it reads as
    // a robot and not as one more ring on the floor. Paused, not taking
    // orders: the same disc with its fill faded, so it never reads as the
    // ready robot beside it. Offline: the disc dimmed whole.
    var ds = proj(r.rest.x, r.rest.y);
    kids.push({ key: 'disc', tag: 'circle', attrs: {
      cx: ds[0], cy: ds[1], r: r.rest.r, fill: r.color, 'stroke-width': robotR * 0.14,
      class: 'map-robot' + (r.state === 'paused' ? ' is-paused' : r.state === 'offline' ? ' is-offline' : ''),
      style: r.state === 'paused' ? 'stroke: ' + r.color : null
    } });
  }
  return { key: 'robot:' + r.id, tag: 'g', attrs: { 'data-robot': r.id, class: 'map-robot-hit' }, kids: kids };
}

// Chevron pointing +X (heading 0), centered at origin: a forward triangle
// with a notched tail so heading reads at a glance.
function chevronPoints(s) {
  return s + ',0 ' + (-s * 0.85) + ',' + (s * 0.72) + ' ' +
    (-s * 0.42) + ',0 ' + (-s * 0.85) + ',' + (-s * 0.72);
}

// chipSpecs is the name chips, for the robots with r.chip (drawn in
// r.chipColor), placed with greedy downward de-collision so a cluster of
// robots parked on top of each other reads as a vertical list of names. A
// chip is a [data-robot] group too, so clicking the name focuses the same
// robot as clicking its glyph.
function chipSpecs(list, robotR, fontS, proj) {
  var placed = [], out = [];
  list.forEach(function (r) {
    if (!r.chip) return;
    var s = proj(r.x, r.y);
    var lx = s[0], ly = s[1] - robotR * 2.0;
    var guard = 0;
    while (guard++ < 16 && placed.some(function (p) {
      return Math.abs(p.x - lx) < fontS * 5.2 && Math.abs(p.y - ly) < fontS * 1.25;
    })) { ly += fontS * 1.35; }
    placed.push({ x: lx, y: ly });
    var color = r.chipColor;
    var halfW = (r.id.length * fontS * 0.62) / 2 + fontS * 0.55;
    var chipH = fontS * 1.3;
    var kids = [];
    // Leader line ties a displaced chip back to its robot so a stacked
    // cluster of names stays attributable.
    if (Math.abs(ly - s[1]) > robotR * 2.6) {
      kids.push({ key: 'leader', tag: 'line', attrs: {
        x1: s[0], y1: s[1], x2: lx, y2: ly - chipH * 0.7, class: 'map-chip-leader', 'stroke-width': fontS * 0.06
      } });
    }
    kids.push({ key: 'chip', tag: 'rect', attrs: {
      x: lx - halfW, y: ly - chipH * 0.75, width: halfW * 2, height: chipH, rx: fontS * 0.25,
      class: 'map-chip', stroke: color, 'stroke-width': fontS * 0.06
    } });
    kids.push({ key: 'dot', tag: 'circle', attrs: { cx: lx - halfW + fontS * 0.5, cy: ly - chipH * 0.1, r: fontS * 0.2, fill: color } });
    kids.push({ key: 'label', tag: 'text', attrs: { x: lx + fontS * 0.35, y: ly, class: 'map-robot-label', 'font-size': fontS }, text: r.id });
    out.push({ key: 'chip:' + r.id, tag: 'g', attrs: { 'data-robot': r.id, class: 'map-robot-hit' }, kids: kids });
  });
  return out;
}

