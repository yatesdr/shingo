// comet-layer.js — a scene map's route comets: a bright head and nine fading,
// tapering dots running along each moving robot's route, on an <svg> of their
// own laid over the map.
//
// Moved whole, 2026-11-14, from Core's pages/dashboard-map.js (the robot map),
// which still decides which comets there are and along what (its calm rule,
// its focus, a robot's leg and RDS's path). The look and the loop are as they
// were: drawn at most 30 times a second, and not at all while the page is
// hidden or the map is off screen. Pinned there, in
// dashboard-map.render.test.js: the draw lists theCarryCometAtHalfLap,
// theFetchCometAtHalfLap, theHeadTurnsTheCorner and theEndsFade, and the loop's
// cost thirtyDrawnFramesASecond, aSkippedFrameIsSkippedNotSlowed,
// noCometWorkWhileHidden, noCometWorkWhileOffScreen and noCometWorkWithNoComets.
//
// ONE LAYER PER createCometLayer, its state its own: a page that made two would
// run two loops, never one loop for both.

import { svgNode } from '/static/shared/scene-view.js';

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
// leg: 'fetch' → hollow (outlined) dots = empty run to source; 'carry' → solid
// (filled) dots = loaded run to delivery. So the map reads loaded vs empty at a
// glance.
function buildCometDots(layer, color, robotR, leg) {
  var fetch = leg === 'fetch';
  var headK = robotR * 0.5;
  var headColor = hexLighten(color, 0.45);
  var sw = (robotR * 0.14).toFixed(3); // outline weight for the hollow fetch dots
  var dots = [];
  for (var i = 0; i < COMET_COUNT; i++) {
    var el;
    if (i === 0) {
      var head = {
        points: (headK * 1.7) + ',0 ' + (-headK) + ',' + headK + ' ' +
          (-headK * 0.35) + ',0 ' + (-headK) + ',' + (-headK),
        // Fetch uses a distinct class so the `.map-comet-head { stroke: none }`
        // rule doesn't kill the hollow outline (CSS beats a presentation attr).
        class: fetch ? 'map-comet-head-hollow' : 'map-comet-head'
      };
      if (fetch) { head.fill = 'none'; head.stroke = headColor; head['stroke-width'] = sw; }
      else { head.fill = headColor; }
      el = svgNode('polygon', head);
    } else {
      var dot = { r: (robotR * 0.4 * (1 - 0.7 * i / COMET_COUNT)).toFixed(3) };
      if (fetch) { dot.fill = 'none'; dot.stroke = color; dot['stroke-width'] = sw; }
      else { dot.fill = color; }
      el = svgNode('circle', dot);
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

var COMET_FRAME_MS = 1000 / 30;
var COMET_FRAME_SLACK_MS = 1;

// createCometLayer is the comet layer over the map drawn in `host` (the
// element the scene's <svg> lives in): the layer is put in host's parent,
// right after host, the first time there is something to draw. It answers:
//   sync(routes, robotR, view)  the comets to show, [{pts (screen), color,
//                               leg, sig}], sorted by the caller, at the
//                               view {minX, minY, w, h}
//   clear()                     no comets, and the loop stopped
//   setViewBox(vb)              the view moved without a sync (an ease, a
//                               zoom, a drag)
//   wirePause()                 stop the loop while the page is hidden or the
//                               map is off screen; call it once
//   layer(), sig(), state()     the layer element, the set signature and the
//                               live comets, for a test to read
export function createCometLayer(host) {
  var cometLayer = null;    // persistent SVG overlay holding the route comets
  var lastCometSig = '';    // signature of the last-built comet set (rebuild only on change)
  var cometState = [];      // live comet geometry + dot elements, positioned each frame by rAF
  var cometRAF = 0;         // requestAnimationFrame handle for the comet positioner loop

  // tickComets is the comet loop: one call per display frame, drawing at most
  // 30 of them a second. Where a dot sits is a function of the frame's own
  // timestamp, never of how many frames were drawn, so a skipped frame is
  // skipped, not slowed, and after a pause the comets come back where the clock
  // says. The 1 ms slack is for a 60 Hz display, whose second frame lands a hair
  // under 1000/30 ms after the first.
  //
  // The loop runs only while there is something to see (cometsMayRun): a comet,
  // a visible page, the map on screen. Otherwise it stops re-arming, and
  // startComets arms it again from sync, visibilitychange or the map's
  // IntersectionObserver (wirePause).
  var cometDrawnAt = -Infinity; // timestamp of the last drawn frame
  var mapOnScreen = true;       // until the IntersectionObserver says otherwise
  function cometsMayRun() { return cometState.length > 0 && !document.hidden && mapOnScreen; }
  function tickComets(now) {
    cometRAF = 0;
    if (!cometsMayRun()) return;
    if (now - cometDrawnAt >= COMET_FRAME_MS - COMET_FRAME_SLACK_MS) {
      cometDrawnAt = now;
      placeComets(now);
    }
    cometRAF = requestAnimationFrame(tickComets);
  }
  function startComets() { if (!cometRAF && cometsMayRun()) cometRAF = requestAnimationFrame(tickComets); }
  function stopComets() { if (cometRAF) { cancelAnimationFrame(cometRAF); cometRAF = 0; } }

  // wireCometPause stops the comet loop while the page is hidden or the map is
  // scrolled off screen, and restarts it when either comes back. Without
  // IntersectionObserver the map counts as on screen.
  var cometPauseWired = false;
  function wireCometPause() {
    if (cometPauseWired) return;
    cometPauseWired = true;
    document.addEventListener('visibilitychange', function () {
      if (document.hidden) stopComets(); else startComets();
    });
    var Observer = window.IntersectionObserver;
    if (!host || !Observer) return;
    new Observer(function (entries) {
      mapOnScreen = entries[entries.length - 1].isIntersecting;
      if (mapOnScreen) startComets(); else stopComets();
    }).observe(host);
  }

  // placeComets positions every comet's dots along its LIVE polyline at `now`.
  // Geometry comes from cometState, which sync refreshes every render tick, so
  // the streak follows the robot as it drives, gliding continuously with no
  // teardown or restart.
  function placeComets(now) {
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
  }

  // ── persistent comet overlay ───────────────────────────────────────
  // The comets live on their own <svg> layered over the map — a sibling of
  // host inside host's parent (on the robot map, #map-svg-wrap inside the
  // position:relative .map-region), with the same viewBox and
  // pointer-events:none (.map-comet-layer) so robot clicks fall through to the
  // scene. The scene view never touches it; the layer is cleared and rebuilt
  // only when the comet SET changes (route added/removed,
  // status/focus/scale/frame change), detected by a signature, and otherwise
  // its dots only move.
  function ensureCometLayer() {
    if (cometLayer && cometLayer.parentNode) return cometLayer;
    var parent = host && host.parentNode;
    if (!parent) return null;
    cometLayer = svgNode('svg', { class: 'map-comet-layer', preserveAspectRatio: 'xMidYMid meet' });
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

  function syncCometLayer(cometRoutes, robotR, view) {
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
        return { pts: c.pts, meas: measurePolyline(c.pts), dots: buildCometDots(layer, c.color, robotR, c.leg) };
      });
    } else {
      // Same SET — just refresh each comet's live geometry (robot moved). The rAF
      // positioner reads it next frame, so the streak tracks the robot with no
      // teardown and no restart. The caller's deterministic sort keeps index →
      // comet stable, so cometState[i] always matches cometRoutes[i].
      for (var i = 0; i < cometState.length && i < cometRoutes.length; i++) {
        cometState[i].pts = cometRoutes[i].pts;
        cometState[i].meas = measurePolyline(cometRoutes[i].pts);
      }
    }
    // No comets left: cancel the frame already queued rather than let it run
    // once to find nothing.
    if (cometState.length) startComets(); else stopComets();
  }

  return {
    sync: syncCometLayer,
    clear: clearCometLayer,
    setViewBox: function (vb) { if (cometLayer) cometLayer.setAttribute('viewBox', vb); },
    wirePause: wireCometPause,
    layer: function () { return cometLayer; },
    sig: function () { return lastCometSig; },
    state: function () { return cometState; }
  };
}
