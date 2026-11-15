// operator-robot-map.js — the robot panel's drawing: this screen's robots on
// the plant map, bottom right over the board, the way Core's robot map draws
// them. Loaded the first time the panel shows (operator-robot-panel.js, which
// decides when it shows and when it goes) and held for the page's life.
//
// THE SAME MAP AND COMETS AS CORE, on the shared machinery only (lead
// decisions D6, D7, D10, D11, D12): the scene's lanes and point glyphs drawn
// by shared/scene-view.js, each robot's route RDS's path drawn along the
// stored rows by shared/scene-path.js pathWorld, the comets shared/
// comet-layer.js's dots. Nothing comes from Core's dashboard-map.js, and the
// panel works out no route: an empty path, or one with a hop the scene has
// no row for, is no route and no comet. The look is shared/scene-view.css,
// and when a robot is moving, the busy-floor calm and the pickup radius are
// shared/robot-motion.js, both Core's map's too. What the page decides it
// decides as Core's map does: a robot's colour is its order's status hue, a
// comet runs only while it is moving (motion means motion), a faulted order's
// robot draws its path as a static grey lane and the fault flare. The leg
// (fetch hollow, carry solid) is the panel's own orderLeg, since Core's reads
// the board's `carrying`, which the panel does not have.
//
// LIMITED TO THIS SCREEN'S ORDERS. The page hands over the orders that have
// a robot (robotOrders); the stream carries a position for each of them that
// Core has a fresh sample for, joined to the Edge's own order id. The robot
// drawn is the one the stream names, never the one the order stored (D14).
//
// THE DATA, ONCE PER PAGE LIFE WHERE IT CAN BE:
//   - the scene, D11's shape, from GET /api/operator-stations/<id>/scene on
//     the first show (D7). The door answers no-cache with an ETag on the
//     geometry's revision, so a later page life revalidates it to a 304 in
//     the browser's cache. An empty answer (no map yet) or a failed one is
//     asked again on the next show, never per frame;
//   - the positions, on the panel's own EventSource,
//     /events?topics=robot-pos.<id> (D12), opened when it shows and closed
//     when it hides. Its first frame is {available, interval_ms} (D17); then
//     frames {t, interval_ms, orders: [{order_id, robot_id, x, y, heading,
//     path?}]} and {available, interval_ms} again on a change;
//   - the ETA and the order's status, from the view the page already holds.
//
// THE CROP is the cell's nodes (this screen's tile nodes on the map) and
// every robot drawn, both of a swap included, padded and fitted to the
// panel. RDS's path is not fitted: it is drawn as far as the crop shows it.
// The crop is worked out again only when that set changes (a robot comes or
// goes, the cell changes), when a robot drawn has left it, or when a name
// chip would be cut at its edge, never per position. A robot that leaves
// grows the crop by a quarter of its extent on every side, so a robot
// driving away refits a few times, not on every position. A cut chip widens
// the crop on its side, never past the whole plant: zooming out packs the
// chips tighter, so past that point it would only make them worse.
//
// POSITION LOST: an order with no position for 3 x interval_ms (D3, D13)
// draws its robot faded where it was last seen, no route and no comet, and
// its row says so. An order with a robot that has had no position at all
// since the panel opened (or since it was handed over, the later) goes lost
// the same way, on the interval the stream's last frame said (its first,
// {available}, already says Core's: D17): its row says Position lost and
// names no robot (D14; lead ruling on U10, 6).
// One timer, armed only while the panel shows, wakes the panel at the first
// such deadline.
//
// HIDDEN IS ZERO WORK (hide): the stream closed, the comet loop stopped, the
// timer cleared, the positions forgotten, and nothing written to the panel
// until it shows again.

import { makeProjector, headingDeg, isCoord, dist2 } from '/static/shared/scene-geom.js';
import { sceneNet } from '/static/shared/scene-net.js';
import { pathWorld } from '/static/shared/scene-path.js';
import { createSceneView } from '/static/shared/scene-view.js';
import { createCometLayer } from '/static/shared/comet-layer.js';
import { COMET_LIMIT, PICKUP_RADIUS, movedFrom, movingAt } from '/static/shared/robot-motion.js';
import { formatETA } from './operator-util.js';

// The map's box in CSS px (operator.css .os-robot-panel, .os-rp-map), read
// back when the panel shows; these stand in when the box reads no size.
const MAP_W = 360;
const MAP_H = 216;
// Marker sizes in CSS px, whatever the crop: the chips stay legible at any
// zoom. 12 px is the type scale's smallest step (--font-xs).
const ROBOT_PX = 9;
const NODE_PX = 5;
const FONT_PX = 12;
// Position lost after this many intervals with no position (D13).
const LOST_AFTER = 3;
// When a robot is moving (movedFrom, movingAt), the calm's COMET_LIMIT and the
// PICKUP_RADIUS are shared/robot-motion.js's, Core's robot map's too.
// The crop: the set padded by FIT_SLACK of its extent on every side, by
// LEAVE_SLACK when a robot has left it; never narrower than MIN_PLANT of the
// plant's larger extent (Core's map keeps 55% of it; the panel is a crop), or
// NO_SCENE_MIN metres with no scene to measure.
const FIT_SLACK = 0.1;
const LEAVE_SLACK = 0.25;
const MIN_PLANT = 0.25;
const NO_SCENE_MIN = 20;
// How many times one draw may widen the crop for a chip it would cut.
const CHIP_FITS = 4;
// The statuses a robot drives on. A staged robot stands at its spot, and a
// faulted one has stopped (its path is a static lane).
const MOVING = { dispatched: true, in_transit: true };

function cssVar(name, fallback) {
    try {
        const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
        return v || fallback;
    } catch (_) {
        return fallback;
    }
}

// readLook is the colours, from the shared tokens as Core's map reads them
// (its STATUS_COLOR, IDLE_MOVE_COLOR, ACCENT): read once, the station is
// dark-only. The fallbacks are the dark token values.
function readLook() {
    const blocked = cssVar('--status-blocked-dot', '#f85149');
    return {
        status: {
            dispatched: cssVar('--status-dispatched-dot', '#4f9bff'),
            in_transit: cssVar('--status-in-transit-dot', '#34c3e0'),
            staged: cssVar('--status-staged-dot', '#15b8a0'),
            faulted: blocked,
        },
        idle: cssVar('--map-robot-idle', '#8b949e'),
        node: cssVar('--map-node', '#7e92b3'),
        scene: { flare: blocked, accent: cssVar('--accent', '#7c7cf0'), accentGlow: cssVar('--accent-glow', 'rgba(124,124,240,0.55)') },
    };
}

function reducedMotion() {
    return !!(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches);
}

// chipHalfPx is half a name chip's width for an id of n characters, in px:
// scene-view.js chipSpecs' measure. Only the crop's first guess at the room a
// chip needs; the drawn chips are read back and the crop widened if one is cut.
function chipHalfPx(n) {
    return (n * FONT_PX * 0.62) / 2 + FONT_PX * 0.55;
}

// createRobotMap builds the panel (hidden) and answers show(view, orders),
// update(view, orders) and hide(). opts: stationID, onTap (the panel was
// tapped), onUnavailable (the stream said {available: false}).
export function createRobotMap(opts) {
    const look = readLook();
    const proj = makeProjector(false);
    const topic = 'robot-pos.' + opts.stationID;

    const root = document.createElement('section');
    root.id = 'os-robot-panel';
    root.className = 'os-robot-panel';
    root.setAttribute('aria-label', 'Robots on this screen. Tap to hide.');
    root.hidden = true;
    const box = document.createElement('div');
    box.className = 'os-rp-map';
    const host = document.createElement('div');
    host.className = 'os-rp-scene';
    box.appendChild(host);
    root.appendChild(box);
    const rows = document.createElement('ul');
    rows.className = 'os-rp-rows';
    root.appendChild(rows);
    // After the footer: over the board on the chrome rung, under the modals,
    // the toasts and the flow panel (same rung, later in the page), so a
    // screen the operator opened covers it.
    const footer = document.getElementById('os-footer');
    if (footer && footer.parentNode) footer.parentNode.insertBefore(root, footer.nextSibling);
    else document.body.appendChild(root);
    // A TAP HIDES IT, AND ENDS THERE. The panel covers what is under it for the
    // whole press, so the press's events all land on the panel; hiding on the
    // click, the press's last event, means nothing of it can reach the control
    // the panel was covering. Hiding on pointerdown would let the release and
    // its click land on that control.
    root.addEventListener('click', function (ev) {
        ev.preventDefault();
        ev.stopPropagation();
        opts.onTap();
    });

    const sceneView = createSceneView(host, look.scene);
    const comets = createCometLayer(host);
    comets.wirePause();

    let es = null;          // the stream, while shown
    let available = false;  // the stream said {available: true}
    let revealed = false;   // on screen
    let scene = null, sceneState = '', sceneSeq = 0;
    let view = null, orders = [];
    let recs = {};          // order id -> its last position
    let picked = {};        // order id -> at its source once (orderLeg)
    let fit = null;         // the crop: {key, view, s, margins, slack, lanes}
    let fits = 0;           // crops worked out, for the pins
    let lostTimer = 0;
    let rowsKey = null;
    let boxW = MAP_W, boxH = MAP_H;
    const tally = { misses: 0 };
    // openedAt is when the stream opened; listedAt, order id -> when the order
    // was first handed over with a robot since then; lastInterval the latest
    // interval_ms a frame said, {available} or positions, held for the page's
    // life.
    let openedAt = 0, listedAt = {}, lastInterval = 0;

    function show(v, o) {
        view = v;
        orders = o;
        if (es) return;
        es = new EventSource('/events?topics=' + topic);
        es.addEventListener(topic, onFrame);
        openedAt = Date.now();
        noteListed(o);
        loadScene();
    }

    function update(v, o) {
        view = v;
        orders = o;
        if (!es) return;
        noteListed(o);
        prune();
        if (revealed) draw();
    }

    // noteListed notes when each order was first handed over while shown.
    function noteListed(o) {
        const now = Date.now();
        o.forEach(function (ord) { if (!listedAt[ord.id]) listedAt[ord.id] = now; });
    }

    function hide() {
        if (es) { es.close(); es = null; }
        if (lostTimer) { clearTimeout(lostTimer); lostTimer = 0; }
        available = false;
        recs = {};
        picked = {};
        listedAt = {};
        fit = null;
        if (revealed) {
            revealed = false;
            root.hidden = true;
            comets.clear();
        }
    }

    function loadScene() {
        if (sceneState === 'ready' || sceneState === 'loading') return;
        sceneState = 'loading';
        fetch('/api/operator-stations/' + opts.stationID + '/scene')
            .then(function (res) {
                if (!res.ok) throw new Error('scene answered ' + res.status);
                return res.json();
            })
            .then(function (j) {
                const placed = !!(j && j.points && Object.keys(j.points).length > 0);
                scene = placed ? j : null;
                sceneState = placed ? 'ready' : 'empty';
            })
            .catch(function (err) {
                console.error('robot panel scene', err);
                scene = null;
                sceneState = 'failed';
            })
            .then(function () {
                sceneSeq++;
                fit = null;
                if (es) reveal();
            });
    }

    function onFrame(ev) {
        let d;
        try { d = JSON.parse(ev.data); } catch (_) { return; }
        if (!d || typeof d !== 'object' || !es) return;
        if (d.interval_ms > 0) lastInterval = d.interval_ms;
        if (Object.prototype.hasOwnProperty.call(d, 'available')) {
            if (!d.available) { opts.onUnavailable(); return; }
            available = true;
            reveal();
            return;
        }
        const now = Date.now();
        const net = scene ? sceneNet(scene) : null;
        const scale = net ? net.scale : 0;
        (d.orders || []).forEach(function (p) {
            if (!p || !isCoord(p.x) || !isCoord(p.y)) return;
            const rec = recs[p.order_id] || (recs[p.order_id] = { lastMoveAt: 0, interval: 0 });
            if (isCoord(rec.x) && movedFrom(rec, p, scale)) rec.lastMoveAt = now;
            rec.robot = String(p.robot_id || '');
            rec.x = p.x;
            rec.y = p.y;
            rec.heading = isCoord(p.heading) ? p.heading : 0;
            rec.path = Array.isArray(p.path) ? p.path : [];
            rec.route = undefined;
            rec.seenAt = now;
            if (d.interval_ms > 0) rec.interval = d.interval_ms;
        });
        if (revealed) draw();
    }

    function reveal() {
        if (!es || !available || sceneState === 'loading') return;
        if (!revealed) {
            revealed = true;
            root.hidden = false;
            boxW = box.clientWidth || MAP_W;
            boxH = box.clientHeight || MAP_H;
        }
        draw();
    }

    // prune forgets the positions, the pickup latches and the listing times of
    // orders that no longer have a robot here.
    function prune() {
        const live = {};
        orders.forEach(function (o) { live[o.id] = true; });
        [recs, picked, listedAt].forEach(function (m) {
            Object.keys(m).forEach(function (id) { if (!live[id]) delete m[id]; });
        });
    }

    // neverSeen is the orders with a robot that have had no position since the
    // panel opened, each with when it goes lost: 3 x the interval after the
    // panel opened or the order was handed over, the later. The interval is
    // the stream's last interval_ms; its first frame, {available}, carries
    // Core's (D17). A stream that has said none gives nothing to count with:
    // no deadline, never lost (the panel holds no copy of Core's cadence).
    // Its row names no robot: the robot is the stream's to name (D14).
    function neverSeen(now) {
        return orders.filter(function (o) { return !recs[o.id]; }).map(function (o) {
            const due = lastInterval > 0 ? Math.max(openedAt, listedAt[o.id] || openedAt) + LOST_AFTER * lastInterval : Infinity;
            return { o: o, due: due, lost: now > due };
        });
    }

    // shown is the robots drawn: each order with a position, once per robot,
    // by robot id.
    function shown(now) {
        const seen = {}, out = [];
        orders.forEach(function (o) {
            const rec = recs[o.id];
            if (!rec || !rec.robot || seen[rec.robot]) return;
            seen[rec.robot] = true;
            const lost = rec.interval > 0 && now - rec.seenAt > LOST_AFTER * rec.interval;
            out.push({ o: o, rec: rec, lost: lost, moving: !lost && movingAt(rec.lastMoveAt, now) });
        });
        return out.sort(function (a, b) { return a.rec.robot < b.rec.robot ? -1 : a.rec.robot > b.rec.robot ? 1 : 0; });
    }

    // cellNames is this screen's tile nodes, by the name the view carries.
    function cellNames() {
        const out = {};
        ((view && view.nodes) || []).forEach(function (t) {
            const n = t.node && t.node.core_node_name;
            if (n) out[n] = true;
        });
        return out;
    }

    // cellPlaces is the cell's nodes the scene places, in the scene's own
    // spelling, sorted by name.
    function cellPlaces(net, names) {
        if (!net) return [];
        const out = [], seen = {};
        Object.keys(names).forEach(function (n) {
            const s = net.resolve(n);
            const p = s && scene.points[s];
            if (!p || seen[s] || !isCoord(p.x) || !isCoord(p.y)) return;
            seen[s] = true;
            out.push({ name: s, x: p.x, y: p.y });
        });
        return out.sort(function (a, b) { return a.name < b.name ? -1 : a.name > b.name ? 1 : 0; });
    }

    function draw() {
        const now = Date.now();
        const net = scene ? sceneNet(scene) : null;
        const list = shown(now);
        const names = cellNames();
        const cell = cellPlaces(net, names);
        const pts = cell.map(function (c) { return proj(c.x, c.y); })
            .concat(list.map(function (r) { return proj(r.rec.x, r.rec.y); }));
        if (!pts.length) {
            sceneView.clear();
            comets.clear();
            fit = null;
        } else {
            const key = cell.map(function (c) { return c.name; }).join(',') + '|' +
                list.map(function (r) { return r.rec.robot; }).join(',');
            if (!fit || fit.key !== key) refit(net, key, pts, FIT_SLACK, roomFor(list));
            else if (list.some(function (r) { return !inside(proj(r.rec.x, r.rec.y)); })) refit(net, key, pts, LEAVE_SLACK, fit.margins);
            let routes = drawFrame(net, cell, list, names);
            // A chip cut at an edge widens the crop on that side, but never past
            // the whole plant: robots knotted tighter than the chips can stack
            // keep their crop, and the chips that cannot fit are cut.
            for (let i = 0; i < CHIP_FITS; i++) {
                const over = chipOverflow();
                if (!over) break;
                const before = fit;
                refit(net, key, pts, fit.slack, widen(fit.margins, over));
                if (fit.s < plantScale(pts, before.margins)) { fit = before; break; }
                routes = drawFrame(net, cell, list, names);
            }
            comets.sync(routes, ROBOT_PX / fit.s, fit.view);
        }
        const unseen = neverSeen(now);
        drawRows(list, names, unseen);
        armLost(list, now, unseen);
    }

    // roomFor is the px each side of the crop keeps clear around the set, for
    // what is drawn beyond a point: a robot's halo, its name chip above it.
    function roomFor(list) {
        if (!list.length) return { top: NODE_PX * 3, right: NODE_PX * 3, bottom: NODE_PX * 3, left: NODE_PX * 3 };
        let longest = 0;
        list.forEach(function (r) { longest = Math.max(longest, r.rec.robot.length); });
        const side = Math.max(ROBOT_PX * 1.35, chipHalfPx(longest)) + 4;
        return { top: ROBOT_PX * 2 + FONT_PX * 1.3 * 0.75 + 4, right: side, bottom: ROBOT_PX * 1.35 + 4, left: side };
    }

    // widen is margins m with each side a chip runs past grown by the overrun.
    function widen(m, over) {
        function side(k) { return over[k] > 0.5 ? m[k] + over[k] + 2 : m[k]; }
        return { top: side('top'), right: side('right'), bottom: side('bottom'), left: side('left') };
    }

    // plantScale is the px per metre at which the whole plant, and every point
    // in pts, fits the panel inside margins m: the furthest a chip may widen
    // the crop.
    function plantScale(pts, m) {
        let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
        function take(p) { x0 = Math.min(x0, p[0]); x1 = Math.max(x1, p[0]); y0 = Math.min(y0, p[1]); y1 = Math.max(y1, p[1]); }
        pts.forEach(take);
        Object.keys((scene && scene.points) || {}).forEach(function (n) {
            const p = scene.points[n];
            if (isCoord(p.x) && isCoord(p.y)) take(proj(p.x, p.y));
        });
        const innerW = Math.max(boxW - m.left - m.right, 24), innerH = Math.max(boxH - m.top - m.bottom, 24);
        return Math.min(innerW / Math.max(x1 - x0, 1e-6), innerH / Math.max(y1 - y0, 1e-6));
    }

    // refit works out the crop for screen points pts: their box, padded by
    // slack of its extent on every side, fitted to the panel inside the
    // margins (px), centred, and never narrower than MIN_PLANT of the plant.
    function refit(net, key, pts, slack, margins) {
        let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
        pts.forEach(function (p) {
            x0 = Math.min(x0, p[0]); x1 = Math.max(x1, p[0]);
            y0 = Math.min(y0, p[1]); y1 = Math.max(y1, p[1]);
        });
        const minExt = plantExtent() > 0 ? plantExtent() * MIN_PLANT : NO_SCENE_MIN;
        const pad = Math.max(x1 - x0, y1 - y0, minExt) * slack;
        x0 -= pad; x1 += pad; y0 -= pad; y1 += pad;
        const innerW = Math.max(boxW - margins.left - margins.right, 24);
        const innerH = Math.max(boxH - margins.top - margins.bottom, 24);
        let s = Math.min(innerW / Math.max(x1 - x0, 1e-6), innerH / Math.max(y1 - y0, 1e-6));
        s = Math.min(s, Math.max(innerW, innerH) / minExt);
        const cx = (x0 + x1) / 2, cy = (y0 + y1) / 2;
        const v = {
            minX: cx - (margins.left + innerW / 2) / s, minY: cy - (margins.top + innerH / 2) / s,
            w: boxW / s, h: boxH / s,
        };
        fit = { key: key, view: v, s: s, margins: margins, slack: slack, lanes: net ? lanesIn(net, v) : [] };
        fits++;
    }

    // plantExtent is the scene's larger extent, every point counted, once per
    // scene (0 with none).
    let extentOf = -1, extent = 0;
    function plantExtent() {
        if (extentOf === sceneSeq) return extent;
        extentOf = sceneSeq;
        let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
        Object.keys((scene && scene.points) || {}).forEach(function (n) {
            const p = scene.points[n];
            if (!isCoord(p.x) || !isCoord(p.y)) return;
            x0 = Math.min(x0, p.x); x1 = Math.max(x1, p.x); y0 = Math.min(y0, p.y); y1 = Math.max(y1, p.y);
        });
        extent = x1 >= x0 ? Math.max(x1 - x0, y1 - y0) : 0;
        return extent;
    }

    // inside: screen point p is within the crop, clear of its margins.
    function inside(p) {
        const v = fit.view, m = fit.margins, s = fit.s;
        return p[0] >= v.minX + m.left / s && p[0] <= v.minX + (boxW - m.right) / s &&
            p[1] >= v.minY + m.top / s && p[1] <= v.minY + (boxH - m.bottom) / s;
    }

    // lanesIn is the scene's lanes that reach into view v: the base layer
    // draws only those, not the whole plant's under a clip.
    function lanesIn(net, v) {
        const x0 = v.minX - v.w * 0.05, x1 = v.minX + v.w * 1.05, y0 = v.minY - v.h * 0.05, y1 = v.minY + v.h * 1.05;
        return net.lanes.filter(function (l) {
            const ps = [proj(l.a.x, l.a.y), proj(l.b.x, l.b.y)];
            if (l.h) ps.push(proj(l.h[0], l.h[1]), proj(l.h[2], l.h[3]));
            let a = Infinity, b = Infinity, c = -Infinity, d = -Infinity;
            ps.forEach(function (p) { a = Math.min(a, p[0]); c = Math.max(c, p[0]); b = Math.min(b, p[1]); d = Math.max(d, p[1]); });
            return c >= x0 && a <= x1 && d >= y0 && b <= y1;
        });
    }

    // drawFrame puts one frame on the scene view and answers the comets it
    // wants.
    function drawFrame(net, cell, list, names) {
        const v = fit.view, s = fit.s;
        const robotR = ROBOT_PX / s, nodeR = NODE_PX / s, fontS = FONT_PX / s;
        const reduce = reducedMotion();
        let moving = 0;
        list.forEach(function (r) { if (MOVING[r.o.status] && r.moving) moving++; });
        const quiet = moving <= COMET_LIMIT;
        const lines = [], cometRoutes = [];
        list.forEach(function (r) {
            if (r.lost) return;
            const blocked = r.o.status === 'faulted';
            if (!blocked && !MOVING[r.o.status]) return;
            const route = robotRoute(net, r.rec);
            if (!route) return;
            const pts = route.map(function (p) { return proj(p[0], p[1]); });
            const color = look.status[r.o.status];
            if (!reduce && !blocked && r.moving && quiet) {
                const leg = orderLeg(net, r.o, r.rec, names);
                cometRoutes.push({ pts: pts, color: color, leg: leg, sig: r.o.id + '>' + leg + '>' + r.o.status });
            } else if (blocked || reduce) {
                lines.push({ key: 'lane:' + r.o.id, kind: 'frozen', pts: pts, color: blocked ? look.idle : color });
            }
        });
        cometRoutes.sort(function (a, b) { return a.sig < b.sig ? -1 : a.sig > b.sig ? 1 : 0; });
        const baseKey = v.minX + ' ' + v.minY + ' ' + v.w + ' ' + v.h + '|' + sceneSeq;
        sceneView.draw({
            view: v, proj: proj, unit: Math.max(v.w, v.h), nodeR: nodeR, robotR: robotR, fontS: fontS,
            baseKey: baseKey,
            lanes: fit.lanes,
            nodesKey: baseKey + '|' + cell.map(function (c) { return c.name; }).join(','),
            // The cell's nodes, each marked as Core marks an order's node: the
            // glyph takes the accent.
            nodes: function () {
                return cell.map(function (c) { return { x: c.x, y: c.y, kind: 'class', fill: look.node, hot: true }; });
            },
            routes: lines,
            robots: list.map(function (r) { return robotFact(r, nodeR); }),
        });
        return cometRoutes;
    }

    // robotRoute is the robot's route, world: RDS's path along the stored
    // rows from the robot (pathWorld), or null. Once per position and scene.
    function robotRoute(net, rec) {
        if (rec.route === undefined || rec.routeScene !== sceneSeq) {
            rec.route = net ? pathWorld(scene, rec, rec.path, tally) : null;
            rec.routeScene = sceneSeq;
        }
        return rec.route;
    }

    // orderLeg is the panel's own, beside Core's (dashboard-map.js orderLeg):
    // the inputs differ. Core trusts the board's `carrying` (the robot's lift)
    // first, which neither the view nor the stream carries. The panel reads,
    // in order:
    //   - RDS's remaining path, which ends where the robot is going: for an
    //     order coming to one of this screen's nodes, a path whose last point
    //     lies within the pickup radius of that node is the carry leg (lead
    //     ruling on U10, item 1: a robot first seen mid-carry drew hollow);
    //   - departed, the station's word for a leg carrying a bin away from the
    //     cell (docs/terminology.md, Departed);
    //   - then Core's inference: carrying once the robot has been at its
    //     source, or while it is nearer its delivery than its source;
    //     fetching otherwise.
    function orderLeg(net, o, r, names) {
        const pr = (net && net.scale > 0 ? net.scale : 1) * PICKUP_RADIUS;
        if (names[o.delivery_node]) {
            const dst = nodeAt(net, o.delivery_node);
            const end = r.path && r.path.length ? nodeAt(net, r.path[r.path.length - 1]) : null;
            if (dst && end && dist2(end, dst) <= pr * pr) return 'carry';
        }
        if (o.departed) return 'carry';
        const src = nodeAt(net, o.source_node);
        if (!src || picked[o.id]) return 'carry';
        const ds = dist2(r, src);
        if (ds <= pr * pr) { picked[o.id] = true; return 'carry'; }
        const dst = nodeAt(net, o.delivery_node);
        if (dst && dist2(r, dst) < ds) return 'carry';
        return 'fetch';
    }

    function nodeAt(net, name) {
        if (!net || !name) return null;
        const s = net.resolve(String(name));
        const p = s && scene.points[s];
        return p && isCoord(p.x) && isCoord(p.y) ? p : null;
    }

    // robotFact is what scene-view.js draws for one robot (robotSpec,
    // chipSpecs): Core's robotFact for a robot on an active order. A lost
    // robot is the faded disc Core draws for one it cannot reach.
    function robotFact(r, nodeR) {
        const rec = r.rec, color = look.status[r.o.status] || look.idle;
        return {
            id: rec.robot, x: rec.x, y: rec.y,
            state: r.lost ? 'offline' : 'ready',
            color: color, moving: r.moving, alert: false,
            fault: !r.lost && r.o.status === 'faulted',
            focused: false,
            rot: r.moving ? headingDeg(rec.heading, 0) : null,
            rest: r.moving ? null : { x: rec.x, y: rec.y, r: nodeR * 1.05 },
            chip: true, chipColor: color,
        };
    }

    // chipOverflow is how far, in px, the drawn name chips run past each edge
    // of the crop, or null when none does. It reads the chips scene-view.js
    // drew, so its rule is the one placed.
    function chipOverflow() {
        const svg = host.firstChild;
        const layer = svg && svg.children ? svg.children[3] : null;
        if (!layer) return null;
        const v = fit.view, s = fit.s;
        let over = null;
        (function walk(n) {
            const kids = n.children || [];
            for (let i = 0; i < kids.length; i++) {
                const k = kids[i];
                if (k.getAttribute('class') === 'map-chip') {
                    const x = +k.getAttribute('x'), y = +k.getAttribute('y');
                    const w = +k.getAttribute('width'), h = +k.getAttribute('height');
                    const o = {
                        left: (v.minX - x) * s, right: (x + w - v.minX - v.w) * s,
                        top: (v.minY - y) * s, bottom: (y + h - v.minY - v.h) * s,
                    };
                    if (o.left > 0.5 || o.right > 0.5 || o.top > 0.5 || o.bottom > 0.5) {
                        over = over || { left: 0, right: 0, top: 0, bottom: 0 };
                        Object.keys(o).forEach(function (side) { over[side] = Math.max(over[side], o[side]); });
                    }
                } else {
                    walk(k);
                }
            }
        })(layer);
        return over;
    }

    // etaWords is a row's words for order o, as the tile's pill words it
    // (operator-render.js appendETAPills): Arrived when staged, the fault's
    // first word when faulted, else the order's ETA (formatETA) when it is
    // coming to one of this screen's nodes.
    function etaWords(o, names) {
        if (o.status === 'faulted') return (o.fault_line || 'Replanning').split(' · ')[0];
        if (!names[o.delivery_node]) return '';
        if (o.status === 'staged') return 'Arrived';
        const d = formatETA(o.eta);
        return d.empty ? '' : d.text;
    }

    // drawRows writes one row per robot drawn (its status dot, its id, its
    // words), then one per order whose robot was never seen and has gone lost
    // (its dot and Position lost, no robot named); written only when one of
    // them changed.
    function drawRows(list, names, unseen) {
        const items = list.map(function (r) {
            return { robot: r.rec.robot, status: r.o.status, lost: r.lost, words: r.lost ? 'Position lost' : etaWords(r.o, names) };
        }).concat(unseen.filter(function (u) { return u.lost; }).map(function (u) {
            return { robot: '', status: u.o.status, lost: true, words: 'Position lost' };
        }));
        const key = JSON.stringify(items);
        if (key === rowsKey) return;
        rowsKey = key;
        while (rows.firstChild) rows.removeChild(rows.firstChild);
        items.forEach(function (it) {
            const li = document.createElement('li');
            li.className = 'os-rp-row' + (it.lost ? ' is-lost' : '');
            const dot = document.createElement('span');
            dot.className = 'os-rp-dot os-rp-dot--' + it.status;
            const id = document.createElement('span');
            id.className = 'os-rp-id';
            id.textContent = it.robot;
            const words = document.createElement('span');
            words.className = 'os-rp-words';
            words.textContent = it.words;
            li.appendChild(dot);
            li.appendChild(id);
            li.appendChild(words);
            rows.appendChild(li);
        });
    }

    // armLost wakes the panel when the first robot drawn, or the first order
    // still waiting on its first position, would go lost.
    function armLost(list, now, unseen) {
        if (lostTimer) { clearTimeout(lostTimer); lostTimer = 0; }
        if (!es) return;
        let next = Infinity;
        list.forEach(function (r) {
            if (!r.lost && r.rec.interval > 0) next = Math.min(next, r.rec.seenAt + LOST_AFTER * r.rec.interval);
        });
        unseen.forEach(function (u) { if (!u.lost) next = Math.min(next, u.due); });
        if (next < Infinity) lostTimer = setTimeout(onLost, Math.max(0, next - now) + 1);
    }

    function onLost() {
        lostTimer = 0;
        if (es && revealed) draw();
    }

    return {
        show: show,
        update: update,
        hide: hide,
        // inspect is for the pins: what the panel holds, never written to.
        inspect: function () {
            return { root: root, fits: fits, fit: fit, misses: tally.misses, comets: comets, open: !!es, revealed: revealed,
                openedAt: openedAt, listedAt: Object.assign({}, listedAt) };
        },
    };
}
