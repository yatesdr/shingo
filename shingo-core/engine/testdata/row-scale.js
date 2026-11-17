// row-scale.js — the scale Core's robot map gives each list of /api/map/edges
// rows: components/scene-rows.js sceneOfRows, then shared/scene-net.js
// sceneNet(...).scale, the page's own code. Prints the scales as a JSON array.
//
//   node row-scale.js <fixtures.json>     (a JSON array of row lists)
//
// Read by TestMedianRowLength_IsTheMapsScale (robot_motion_test.go), which
// holds Core's medianRowLength to it.

'use strict';

const fs = require('fs');
const path = require('path');

const ROOT = path.join(__dirname, '..', '..', '..');
const SM = require(path.join(ROOT, 'shared', 'testdata', 'js', 'shared-modules.js'));

const ctx = {};
const shared = SM.realm(ctx);
const { sceneOfRows } = SM.moduleOf(ctx, shared,
    path.join(ROOT, 'shingo-core', 'www', 'static', 'components', 'scene-rows.js'));
const { sceneNet } = shared('scene-net.js');

const fixtures = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
process.stdout.write(JSON.stringify(fixtures.map(function (rows) { return sceneNet(sceneOfRows(rows || [])).scale; })));
