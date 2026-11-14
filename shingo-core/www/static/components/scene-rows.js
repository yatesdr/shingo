// scene-rows.js — Core's GET /api/map/edges rows as the shared scene shape,
// for the two Core pages that draw the scene: the robot map (dashboard-map.js)
// and the localization board (components/localization-board.js). ONE adapter,
// so the two pages read one network from one list of rows (lead decision D15:
// the board's rows become a scene the way the robot map's do).
//
// Stays in Core: the rows are Core's own wire, and the Edge reads its scene in
// the shared shape already (docs/shared-layer-promotion.md, clause 2).

import { isCoord } from '/static/shared/scene-geom.js';

// sceneOfRows is GET /api/map/edges as the shape every shared scene module
// reads (shared/scene-net.js: ComposerMap's): points name -> {x, y, class},
// edges [{from, to, h?}], one edge per stored row with its handles, all four
// or none. Built once per load by each page.
//
// A point is a row's END: its place is where the first row naming it puts it
// (a scene point by the same name is not consulted, as the graph never has),
// and two spellings of one name are one point, spelled as first seen,
// because the robot map has always matched names without case. A row end with
// no name is a point named by its place, '@x,y', and marked anon. A row with a
// coordinate that is no number is left out whole.
export function sceneOfRows(rows) {
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
