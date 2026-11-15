// scene-path.js — a list of point names drawn as the world polyline a robot
// drives along them, hop by hop along the scene's stored rows.
//
// A DRIVING ROBOT's ROUTE IS RDS's PATH (lead decisions D6, D10). Core reads
// the path RDS has planned for each robot off its fleet poll and names its
// points from the stored scene (engine robot_path.go, D5); the robot-update
// frame and /api/robots carry it as `path`, the robot's current point first.
// pathWorld draws that and nothing else: no extension to the order's node, no
// straight line, no route of shingo's own. A path that cannot be drawn is no
// route.
//
// Every hop a->b is the stored a->b row's own geometry, its handles in its own
// direction (scene-net.js hop). A hop with no stored row in that direction
// makes the whole path undrawable: half a route, or a hop drawn the wrong way
// along a one-way lane, would be a route nobody planned. A curved row is
// sampled at scene-geom's CUBIC_SAMPLES, so the line rides the aisle the map
// draws under it.
//
// The scene is the D11 shape scene-net.js reads, and its network is read once
// per scene object there.

import { cubicPoint, CUBIC_SAMPLES, isCoord } from '/static/shared/scene-geom.js';
import { sceneNet } from '/static/shared/scene-net.js';

// pathWorld is the route of a robot at {x, y} driving `names` (RDS's path, its
// current point first) as world [[x, y], ...], or null:
//   - fewer than two names: an empty path is no plan, and a one-name path is a
//     robot at its last point, with nothing left to drive;
//   - a name the scene does not have, or a hop with no stored row from one name
//     to the next: no route at all, and `tally.misses` (when a tally is given)
//     counts it, so a page can say how many paths it could not draw.
// THE ROUTE STARTS AT THE ROBOT: the first hop is drawn from the robot's
// projection onto it (the nearest point of that row's drawn line) to its far
// end, then every later hop whole. A robot with no position starts at the
// first hop's start.
export function pathWorld(scene, robot, names, tally) {
    var net = sceneNet(scene);
    var rows = hopRows(net, names, tally);
    if (!rows) return null;
    var first = rowLine(net, rows[0]);
    var out = (robot && isCoord(robot.x) && isCoord(robot.y)) ? fromNearest(first, robot) : first;
    for (var i = 1; i < rows.length; i++) out = out.concat(rowLine(net, rows[i]).slice(1));
    return out.length > 1 ? out : null;
}

// hopsWorld is `names` drawn whole from the first name's place, the same way
// hop by hop, or null when there are fewer than two names or a hop has no
// stored row. The map draws a route model answer with it (an intent line).
export function hopsWorld(scene, names) {
    var net = sceneNet(scene);
    var rows = hopRows(net, names, null);
    if (!rows) return null;
    var out = rowLine(net, rows[0]);
    for (var i = 1; i < rows.length; i++) out = out.concat(rowLine(net, rows[i]).slice(1));
    return out;
}

function hopRows(net, names, tally) {
    if (!names || names.length < 2) return null;
    var rows = [], prev = net.resolve(names[0]);
    for (var i = 1; i < names.length; i++) {
        var next = net.resolve(names[i]);
        var row = prev && next ? net.hop(prev, next) : null;
        if (!row) {
            if (tally) tally.misses = (tally.misses || 0) + 1;
            return null;
        }
        rows.push(row);
        prev = next;
    }
    return rows;
}

// rowLine is one row as drawn: its start, CUBIC_SAMPLES - 1 points along its
// cubic when it has handles, its end.
function rowLine(net, row) {
    var a = net.at[row.from], b = net.at[row.to], pts = [[a.x, a.y]];
    if (row.h) {
        for (var k = 1; k < CUBIC_SAMPLES; k++) {
            var q = cubicPoint(a, row.h, b, k / CUBIC_SAMPLES);
            pts.push([q.x, q.y]);
        }
    }
    pts.push([b.x, b.y]);
    return pts;
}

// fromNearest is `line` from the point of it nearest `p` on: that point, then
// the rest of the line. Of two pieces equally near, the earlier.
function fromNearest(line, p) {
    var best = 0, bt = 0, bd = Infinity;
    for (var i = 0; i < line.length - 1; i++) {
        var ax = line[i][0], ay = line[i][1], dx = line[i + 1][0] - ax, dy = line[i + 1][1] - ay;
        var len2 = dx * dx + dy * dy;
        var t = len2 > 0 ? ((p.x - ax) * dx + (p.y - ay) * dy) / len2 : 0;
        t = t < 0 ? 0 : t > 1 ? 1 : t;
        var ex = ax + dx * t - p.x, ey = ay + dy * t - p.y, d = ex * ex + ey * ey;
        if (d < bd) { bd = d; best = i; bt = t; }
    }
    if (bt === 1) return line.slice(best + 1);
    var a = line[best], b = line[best + 1];
    return [[a[0] + (b[0] - a[0]) * bt, a[1] + (b[1] - a[1]) * bt]].concat(line.slice(best + 1));
}
