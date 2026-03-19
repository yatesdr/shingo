// display.js — Live operator display
// Connects to Edge SSE events and renders the operator canvas with real-time data.
//
// ── SSE field name compatibility ────────────────────────────────────
// Edge's order event structs currently lack json tags, so fields arrive
// PascalCase (OrderID, NewStatus) instead of snake_case. This file handles
// both conventions so it works before AND after the Edge fix lands.
//
// Order events also lack payload_id, so we maintain an orderId→shapeId
// reverse map. When an unknown order ID arrives (externally created order),
// we re-fetch /api/orders/active to rebuild the map.
//
// /api/payloads is admin-only, so we DON'T call it. Auto-generated screens
// have current state baked into the shape configs. Designer screens may show
// briefly stale counts until the first SSE event arrives.
// ─────────────────────────────────────────────────────────────────────

import { renderFrame, fitCanvas, canvasCoords, hitTest, hitButton } from './render.js';
import { hydrateShapes } from './shapes.js';

let canvas, ctx, scale;
let shapes = [];
const liveData = new Map();
let screenId = null;

const payloadShapeMap = new Map();
const lineShapeMap = new Map();
const orderShapeMap = new Map();
let refetchPending = false;

export function initDisplay(canvasEl, screenData) {
    canvas = canvasEl;
    ctx = canvas.getContext('2d');
    scale = fitCanvas(canvas);
    screenId = screenData.id;
    shapes = hydrateShapes(screenData.layout);
    buildIndexes();
    connectSSE();
    fetchActiveOrders();
    canvas.addEventListener('click', onCanvasClick);
    canvas.addEventListener('mousemove', onCanvasMove);
    window.addEventListener('resize', () => { scale = fitCanvas(canvas); });
    requestAnimationFrame(renderLoop);
}

function buildIndexes() {
    payloadShapeMap.clear();
    lineShapeMap.clear();
    for (const s of shapes) {
        if (s.type === 'ordercombo' && s.config.payloadId) {
            payloadShapeMap.set(String(s.config.payloadId), s.id);
        }
        if (s.config.lineId) {
            const lid = String(s.config.lineId);
            if (!lineShapeMap.has(lid)) lineShapeMap.set(lid, []);
            lineShapeMap.get(lid).push(s.id);
        }
    }
}

function clearLiveState(live) {
    live.order_status = '';
    live.order_id = null;
    live.can_confirm = false;
    live.cycle_mode = '';
    live.resupply_order_id = null;
    live.removal_order_id = null;
    live.resupply_status = '';
    live.removal_status = '';
}

function connectSSE() {
    const es = new EventSource('/events');

    es.addEventListener('payload-update', e => {
        const d = JSON.parse(e.data);
        const sid = payloadShapeMap.get(String(d.payload_id));
        if (!sid) return;
        const live = getLive(sid);
        live.remaining = d.new_remaining;
        live.payload_status = d.status;
        const shape = shapes.find(s => s.id === sid);
        if (shape) {
            if (shape.config.total) {
                live.remaining_pct = (d.new_remaining / shape.config.total) * 100;
            }
            shape.config.remaining = d.new_remaining;
            shape.config.payloadStatus = d.status;
        }
        liveData.set(sid, live);
    });

    es.addEventListener('payload-empty', e => {
        const d = JSON.parse(e.data);
        const sid = payloadShapeMap.get(String(d.payload_id));
        if (!sid) return;
        const live = getLive(sid);
        live.payload_status = 'empty';
        live.remaining = 0;
        live.remaining_pct = 0;
        liveData.set(sid, live);
    });

    es.addEventListener('order-update', e => {
        const d = JSON.parse(e.data);
        const orderId = d.order_id || d.OrderID;
        if (!orderId) return;

        if (d.payload_id && !orderShapeMap.has(String(orderId))) {
            const sid = payloadShapeMap.get(String(d.payload_id));
            if (sid) orderShapeMap.set(String(orderId), sid);
        }

        let sid = orderShapeMap.get(String(orderId));
        if (!sid) {
            debouncedRefetch();
            return;
        }

        const live = getLive(sid);
        const newStatus = d.new_status || d.NewStatus || d.Status || '';

        if (newStatus) {
            if (live.cycle_mode === 'two_robot') {
                // Two robot: dual tracking, derive combined status
                const oid = String(orderId);
                if (live.resupply_order_id && oid === String(live.resupply_order_id)) {
                    live.resupply_status = newStatus;
                } else if (live.removal_order_id && oid === String(live.removal_order_id)) {
                    live.removal_status = newStatus;
                }
                live.order_status = deriveHotSwapStatus(live);
                live.order_eta = d.eta || d.ETA || '';
                live.can_confirm = (live.resupply_status === 'delivered');

                if (['confirmed', 'cancelled'].includes(live.resupply_status) &&
                    ['confirmed', 'cancelled', ''].includes(live.removal_status)) {
                    const resOid = String(live.resupply_order_id);
                    const remOid = String(live.removal_order_id);
                    setTimeout(() => {
                        const l = getLive(sid);
                        clearLiveState(l);
                        liveData.set(sid, l);
                        orderShapeMap.delete(resOid);
                        orderShapeMap.delete(remOid);
                    }, 3000);
                }
            } else if (live.cycle_mode === 'single_robot' || live.cycle_mode === 'sequential') {
                // Single robot or sequential: one order, direct tracking
                live.resupply_status = newStatus;
                live.order_status = newStatus;
                live.order_eta = d.eta || d.ETA || '';
                live.can_confirm = (newStatus === 'delivered');

                if (['confirmed', 'cancelled'].includes(newStatus)) {
                    const oid = String(orderId);
                    setTimeout(() => {
                        const l = getLive(sid);
                        clearLiveState(l);
                        liveData.set(sid, l);
                        orderShapeMap.delete(oid);
                    }, 3000);
                }
            } else {
                // No cycle mode set — simple order (manual, etc.)
                live.order_status = newStatus;
                live.order_eta = d.eta || d.ETA || '';
                live.can_confirm = (newStatus === 'delivered');

                if (['confirmed', 'cancelled'].includes(newStatus)) {
                    const oid = String(orderId);
                    setTimeout(() => {
                        const l = getLive(sid);
                        clearLiveState(l);
                        liveData.set(sid, l);
                        orderShapeMap.delete(oid);
                    }, 3000);
                }
            }
        }

        liveData.set(sid, live);
    });

    es.addEventListener('order-failed', e => {
        const d = JSON.parse(e.data);
        const orderId = d.order_id;
        if (!orderId) return;

        const sid = orderShapeMap.get(String(orderId));
        if (!sid) return;

        const live = getLive(sid);

        if (live.cycle_mode === 'two_robot') {
            const oid = String(orderId);
            if (live.resupply_order_id && oid === String(live.resupply_order_id)) {
                live.resupply_status = 'failed';
            } else if (live.removal_order_id && oid === String(live.removal_order_id)) {
                live.removal_status = 'failed';
            }
            live.order_status = deriveHotSwapStatus(live);
        } else {
            live.order_status = 'failed';
        }
        live.can_confirm = false;
        liveData.set(sid, live);

        showNotification('Order failed: ' + (d.reason || 'unknown'), 'error');

        const resOid = live.resupply_order_id ? String(live.resupply_order_id) : null;
        const remOid = live.removal_order_id ? String(live.removal_order_id) : null;
        const oid = String(orderId);
        setTimeout(() => {
            const l = getLive(sid);
            clearLiveState(l);
            liveData.set(sid, l);
            orderShapeMap.delete(oid);
            if (resOid) orderShapeMap.delete(resOid);
            if (remOid) orderShapeMap.delete(remOid);
        }, 5000);
    });

    es.addEventListener('changeover-update', e => {
        const d = JSON.parse(e.data);
        if (d.old_state) return;
        showNotification('Changeover detected — reloading...', 'info');
        setTimeout(() => location.reload(), 1500);
    });

    es.addEventListener('plc-status', e => {
        const d = JSON.parse(e.data);
        for (const s of shapes) {
            if (s.type === 'statusbar') {
                const live = getLive(s.id);
                live.connected = d.connected;
                liveData.set(s.id, live);
            }
        }
    });

    es.onerror = () => {
        for (const s of shapes) {
            if (s.type === 'statusbar') {
                const live = getLive(s.id);
                live.connected = false;
                liveData.set(s.id, live);
            }
        }
    };
}

async function fetchActiveOrders() {
    try {
        const res = await fetch('/api/orders/active');
        if (!res.ok) return;
        const orders = await res.json();
        applyActiveOrders(orders);
    } catch (err) {
        console.error('Failed to fetch active orders:', err);
    }
}

function applyActiveOrders(orders) {
    const byPayload = new Map();
    for (const o of orders) {
        if (!o.payload_id) continue;
        const key = String(o.payload_id);
        if (!byPayload.has(key)) byPayload.set(key, []);
        byPayload.get(key).push(o);
    }

    for (const [payloadKey, group] of byPayload) {
        const sid = payloadShapeMap.get(payloadKey);
        if (!sid) continue;

        const live = getLive(sid);

        if (group.length >= 2) {
            // Two-robot: two active orders for same payload
            group.sort((a, b) => a.id - b.id);
            const resupply = group[0];
            const removal = group[1];

            live.cycle_mode = 'two_robot';
            live.resupply_order_id = resupply.id;
            live.removal_order_id = removal.id;
            live.resupply_status = resupply.status;
            live.removal_status = removal.status;
            live.order_id = resupply.id;
            live.order_status = deriveHotSwapStatus(live);
            live.order_eta = resupply.eta || '';
            live.can_confirm = (resupply.status === 'delivered');

            orderShapeMap.set(String(resupply.id), sid);
            orderShapeMap.set(String(removal.id), sid);
        } else if (group.length === 1 && group[0].order_type === 'complex') {
            // Single complex order = single_robot or sequential (both have staging/release)
            const o = group[0];
            live.cycle_mode = 'single_robot'; // could be sequential too; both behave the same for display
            live.order_id = o.id;
            live.resupply_order_id = o.id;
            live.resupply_status = o.status;
            live.removal_order_id = null;
            live.removal_status = '';
            live.order_status = o.status;
            live.order_eta = o.eta || '';
            live.can_confirm = (o.status === 'delivered');
            orderShapeMap.set(String(o.id), sid);
        } else {
            // Simple retrieve
            const o = group[0];
            live.cycle_mode = '';
            live.order_status = o.status;
            live.order_id = o.id;
            live.order_eta = o.eta || '';
            live.can_confirm = (o.status === 'delivered');
            orderShapeMap.set(String(o.id), sid);
        }

        liveData.set(sid, live);
    }
}

function debouncedRefetch() {
    if (refetchPending) return;
    refetchPending = true;
    setTimeout(async () => {
        refetchPending = false;
        await fetchActiveOrders();
    }, 500);
}

function getLive(sid) {
    return liveData.get(sid) || {};
}

// ── Click handling ──────────────────────────────────────────────────

async function onCanvasClick(e) {
    const { x, y } = canvasCoords(canvas, scale, e.clientX, e.clientY);
    const hit = hitTest(shapes, x, y);
    if (!hit || hit.type !== 'ordercombo' || !hitButton(hit, x, y)) return;

    const live = getLive(hit.id);
    const cfg = hit.config;

    // Staged → release (all cycle modes have this)
    if (live.order_status === 'staged') {
        if (live.cycle_mode === 'two_robot') {
            await releaseHotSwap(hit);
        } else if (live.order_id) {
            await releaseOrder(live.order_id, hit);
        }
        return;
    }

    // Confirm delivery
    if (live.can_confirm && live.order_id) {
        await confirmDelivery(live.order_id, hit);
        return;
    }

    // Order in progress — button disabled
    if (live.order_status && !['confirmed', 'cancelled', 'failed'].includes(live.order_status)) {
        return;
    }

    // REQUEST — create new order
    await createOrder(cfg, hit);
}

function onCanvasMove(e) {
    const { x, y } = canvasCoords(canvas, scale, e.clientX, e.clientY);
    const hit = hitTest(shapes, x, y);
    canvas.style.cursor = (hit && hit.type === 'ordercombo' && hitButton(hit, x, y)) ? 'pointer' : 'default';
}

async function createOrder(cfg, shape) {
    const live = getLive(shape.id);
    live.order_status = 'pending';
    liveData.set(shape.id, live);

    try {
        const body = { quantity: 1 };
        if (cfg.payloadId) body.payload_id = parseInt(cfg.payloadId);

        const endpoint = cfg.actionType === 'store' ? '/api/orders/store' : '/api/orders/request';
        if (cfg.actionType !== 'store') body.retrieve_empty = cfg.retrieveEmpty || false;

        const res = await fetch(endpoint, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body),
        });
        if (!res.ok) throw new Error(await res.text());

        const data = await res.json();
        const mode = data.cycle_mode || '';

        live.cycle_mode = mode;

        if (mode === 'two_robot') {
            live.resupply_order_id = data.resupply.id;
            live.removal_order_id = data.removal.id;
            live.resupply_status = data.resupply.status;
            live.removal_status = data.removal.status;
            live.order_id = data.resupply.id;
            live.order_status = deriveHotSwapStatus(live);
            liveData.set(shape.id, live);

            orderShapeMap.set(String(data.resupply.id), shape.id);
            orderShapeMap.set(String(data.removal.id), shape.id);

            showNotification('Hot-swap orders created', 'success');
        } else if (mode === 'single_robot' || mode === 'sequential') {
            live.order_id = data.resupply.id;
            live.resupply_order_id = data.resupply.id;
            live.resupply_status = data.resupply.status;
            live.order_status = data.resupply.status;
            liveData.set(shape.id, live);

            orderShapeMap.set(String(data.resupply.id), shape.id);

            showNotification('Order created', 'success');
        } else {
            // Simple (no cycle mode — shouldn't happen with new API, but safe fallback)
            const order = data.resupply;
            live.order_id = order.id;
            live.order_status = order.status;
            liveData.set(shape.id, live);

            orderShapeMap.set(String(order.id), shape.id);

            showNotification('Order created', 'success');
        }
    } catch (err) {
        live.order_status = '';
        live.cycle_mode = '';
        liveData.set(shape.id, live);
        showNotification('Order failed: ' + err.message, 'error');
    }
}

// Derive the overall card status from two hot-swap order statuses.
function deriveHotSwapStatus(live) {
    const a = live.resupply_status || '';
    const b = live.removal_status || '';

    if (a === 'failed' || b === 'failed') return 'failed';
    if (a === 'cancelled' || b === 'cancelled') return 'cancelled';
    if (a === 'staged' && b === 'staged') return 'staged';
    if (a === 'delivered') return 'delivered';
    if (a === 'staged' || b === 'staged') return 'positioning';
    if (a === 'in_transit' || b === 'in_transit') return 'in_transit';

    return a || b || 'pending';
}

async function confirmDelivery(orderId, shape) {
    try {
        const res = await fetch(`/api/confirm-delivery/${orderId}`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ final_count: 0 }),
        });
        if (!res.ok) throw new Error(await res.text());

        const live = getLive(shape.id);
        live.order_status = 'confirmed';
        live.can_confirm = false;
        liveData.set(shape.id, live);

        const oid = String(orderId);
        const resupplyOid = live.resupply_order_id ? String(live.resupply_order_id) : null;
        const removalOid = live.removal_order_id ? String(live.removal_order_id) : null;

        setTimeout(() => {
            const l = getLive(shape.id);
            clearLiveState(l);
            liveData.set(shape.id, l);

            orderShapeMap.delete(oid);
            if (resupplyOid) orderShapeMap.delete(resupplyOid);
            if (removalOid) orderShapeMap.delete(removalOid);
        }, 3000);

        showNotification('Delivery confirmed', 'success');
    } catch (err) {
        showNotification('Confirm failed: ' + err.message, 'error');
    }
}

async function releaseOrder(orderId, shape) {
    try {
        const res = await fetch(`/api/orders/${orderId}/release`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({}),
        });
        if (!res.ok) throw new Error(await res.text());

        const live = getLive(shape.id);
        live.order_status = 'in_transit';
        liveData.set(shape.id, live);

        showNotification('Order released', 'success');
    } catch (err) {
        showNotification('Release failed: ' + err.message, 'error');
    }
}

async function releaseHotSwap(shape) {
    const live = getLive(shape.id);
    const resupplyId = live.resupply_order_id;
    const removalId = live.removal_order_id;

    if (!resupplyId || !removalId) {
        showNotification('Missing order IDs for hot-swap release', 'error');
        return;
    }

    try {
        const [resRes, remRes] = await Promise.all([
            fetch(`/api/orders/${resupplyId}/release`, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({}),
            }),
            fetch(`/api/orders/${removalId}/release`, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({}),
            }),
        ]);

        if (!resRes.ok) throw new Error('Resupply release: ' + await resRes.text());
        if (!remRes.ok) throw new Error('Removal release: ' + await remRes.text());

        live.resupply_status = 'in_transit';
        live.removal_status = 'in_transit';
        live.order_status = 'in_transit';
        liveData.set(shape.id, live);

        showNotification('Hot-swap released — both robots moving', 'success');
    } catch (err) {
        live.order_status = 'staged';
        liveData.set(shape.id, live);
        showNotification('Release failed: ' + err.message, 'error');
    }
}

function showNotification(msg, type = 'info') {
    const el = document.createElement('div');
    el.className = `op-notification op-notification-${type}`;
    el.textContent = msg;
    document.body.appendChild(el);
    setTimeout(() => el.classList.add('show'), 10);
    setTimeout(() => {
        el.classList.remove('show');
        setTimeout(() => el.remove(), 300);
    }, 2500);
}

function renderLoop() {
    renderFrame(ctx, shapes, liveData);
    requestAnimationFrame(renderLoop);
}

export { shapes, liveData };
