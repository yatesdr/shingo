// route-scene.js — shared/testdata/js/route-scene.json in the shapes Core
// serves it: GET /api/map/points (domain.ScenePoint) and GET /api/map/edges
// (domain.SceneEdge, one row per stored segment, the handles as four fields or
// none). Read by the robot-map pins (dashboard-map.curve.test.js,
// dashboard-map.render.test.js) and the localization board's
// (localization-board.test.js), so the three draw and route one scene.
//
// Not a test itself: it sits in testdata, outside the embedded static tree, so
// no binary carries or serves it.

'use strict';

const fs = require('fs');
const path = require('path');

const file = path.join(__dirname, '..', '..', '..', '..', '..', 'shared', 'testdata', 'js', 'route-scene.json');
const scene = JSON.parse(fs.readFileSync(file, 'utf8'));
const at = {};
scene.points.forEach(function (p) { at[p.name] = p; });

function pointRows() {
    return scene.points.map(function (p, i) {
        return {
            id: i + 1, area_name: 'AREA', instance_name: p.name, class_name: p.class,
            point_name: p.name, label: '', pos_x: p.x, pos_y: p.y,
        };
    });
}

function edgeRows() {
    return scene.segments.map(function (s, i) {
        const a = at[s.from], b = at[s.to];
        const row = {
            id: i + 1, area_name: 'AREA', instance_name: s.from + '-' + s.to,
            class_name: s.h ? 'DegenerateBezier' : 'StraightPath',
            from_name: s.from, to_name: s.to, from_x: a.x, from_y: a.y, to_x: b.x, to_y: b.y,
        };
        if (s.h) { row.ctrl1_x = s.h[0]; row.ctrl1_y = s.h[1]; row.ctrl2_x = s.h[2]; row.ctrl2_y = s.h[3]; }
        return row;
    });
}

module.exports = { scene: scene, at: at, pointRows: pointRows, edgeRows: edgeRows };
