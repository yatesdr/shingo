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

// Forward: payloadId → shapeId (for payload events)
const payloadShapeMap = new Map();
// Forward: lineId → [shapeId, ...] (for status bar / line-level events)
const lineShapeMap = new Map();
// Reverse: orderId → shapeId (for order events that lack payload_id)
const orderShapeMap = new Map();
// Debounce flag for fallback re-fetch
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

function connectSSE() {
    const es = new EventSource('/events');

    // ── Payload events (have json tags → snake_case) ────────────
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
            // Keep shape config in sync for renderer
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

    // ── Order events ────────────────────────────────────────────
    // Compatibility: check both PascalCase (current) and snake_case (after fix).
    // OrderCreatedEvent:        { OrderID | order_id }
    // OrderStatusChangedEvent:  { OrderID | order_id, NewStatus | new_status, ETA | eta }
    // OrderCompletedEvent:      { OrderID | order_id }
    //
    // None of these carry payload_id (yet). We look up the shape via
    // orderShapeMap. If the order ID is unknown, we re-fetch active orders.
    es.addEventListener('order-update', e => {
        const d = JSON.parse(e.data);
        const orderId = d.order_id || d.OrderID;
        if (!orderId) return;

        // Also check if event carries payload_id (after Edge Change 2 lands)
        if (d.payload_id && !orderShapeMap.has(String(orderId))) {
            const sid = payloadShapeMap.get(String(d.payload_id));
            if (sid) orderShapeMap.set(String(orderId), sid);
        }

        let sid = orderShapeMap.get(String(orderId));
        if (!sid) {
            // Unknown order — likely created externally (auto-reorder, main UI, WarLink)
            // Re-fetch active orders to rebuild the map
            debouncedRefetch();
            return;
        }

        const live = getLive(sid);
        const newStatus = d.new_status || d.NewStatus || d.Status || '';

        if (newStatus) {
            // Hot-swap mode: update the individual order status, then derive overall
            if (live.hot_swap) {
                const oid = String(orderId);
                if (live.resupply_order_id && oid === String(live.resupply_order_id)) {
                    live.resupply_status = newStatus;
                } else if (live.removal_order_id && oid === String(live.removal_order_id)) {
                    live.removal_status = newStatus;
                }
                live.order_status = deriveHotSwapStatus(live);
                live.order_eta = d.eta || d.ETA || '';
                // Confirm available when resupply is delivered
                live.can_confirm = (live.resupply_status === 'delivered');

                // Terminal: both done
                if (['confirmed', 'cancelled'].includes(live.resupply_status) &&
                    ['confirmed', 'cancelled', ''].includes(live.removal_status)) {
                    const resOid = String(live.resupply_order_id);
                    const remOid = String(live.removal_order_id);
                    setTimeout(() => {
                        const l = getLive(sid);
                        l.order_status = '';
                        l.order_id = null;
                        l.can_confirm = false;
                        l.hot_swap = false;
                        l.resupply_order_id = null;
                        l.removal_order_id = null;
                        l.resupply_status = '';
                        l.removal_status = '';
                        liveData.set(sid, l);
                        orderShapeMap.delete(resOid);
                        orderShapeMap.delete(remOid);
                    }, 3000);
                }
            } else {
                // Simple single-order mode
                live.order_status = newStatus;
                live.order_eta = d.eta || d.ETA || '';
                live.can_confirm = (newStatus === 'delivered');

                // Terminal state — clear after a delay
                if (['confirmed', 'cancelled'].includes(newStatus)) {
                    const oid = String(orderId);
                    setTimeout(() => {
                        const l = getLive(sid);
                        l.order_status = '';
                        l.order_id = null;
                        l.can_confirm = false;
                        liveData.set(sid, l);
                        orderShapeMap.delete(oid);
                    }, 3000);
                }
            }
        }

        liveData.set(sid, live);
    });

    // OrderFailedEvent has json tags: { order_id, order_uuid, order_type, reason }
    es.addEventListener('order-failed', e => {
        const d = JSON.parse(e.data);
        const orderId = d.order_id;
        if (!orderId) return;

        const sid = orderShapeMap.get(String(orderId));
        if (!sid) return;

        const live = getLive(sid);

        if (live.hot_swap) {
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

        // Clear failed status after a few seconds — clean up all tracked IDs
        const resOid = live.resupply_order_id ? String(live.resupply_order_id) : null;
        const remOid = live.removal_order_id ? String(live.removal_order_id) : null;
        const oid = String(orderId);
        setTimeout(() => {
            const l = getLive(sid);
            l.order_status = '';
            l.order_id = null;
            l.can_confirm = false;
            l.hot_swap = false;
            l.resupply_order_id = null;
            l.removal_order_id = null;
            l.resupply_status = '';
            l.removal_status = '';
            liveData.set(sid, l);
            orderShapeMap.delete(oid);
            if (resOid) orderShapeMap.delete(resOid);
            if (remOid) orderShapeMap.delete(remOid);
        }, 5000);
    });

    // ── Changeover events ──────────────────────────────────────────
    // All changeover lifecycle events arrive as 'changeover-update'.
    // ChangeoverStateChangedEvent has old_state/new_state (mid-changeover).
    // Completed and Cancelled events signal the end — payloads may change.
    // We reload on any non-transition event (no old_state field). This also
    // catches ChangeoverStartedEvent, but an extra reload there is harmless.
    es.addEventListener('changeover-update', e => {
        const d = JSON.parse(e.data);
        if (d.old_state) return; // mid-changeover state transition — ignore
        showNotification('Changeover detected — reloading...', 'info');
        setTimeout(() => location.reload(), 1500);
    });

    // ── PLC connection status ────────────────────────────────────
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

// ── Initial state fetch ─────────────────────────────────────────────
// We only fetch active orders (public endpoint). Payload state comes from
// the shape configs baked in by the Go handler (auto-layout) or from SSE
// events (designer screens). We do NOT call /api/payloads (admin-only).

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
    // Group orders by payload_id to detect hot-swap pairs.
    // If two active orders share the same payload, treat them as hot-swap.
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
            // Hot-swap pair — first is resupply, second is removal
            // (Core creates resupply first, so it has the lower ID)
            group.sort((a, b) => a.id - b.id);
            const resupply = group[0];
            const removal = group[1];

            live.hot_swap = true;
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
        } else {
            // Single active order
            const o = group[0];
            live.order_status = o.status;
            live.order_id = o.id;
            live.order_eta = o.eta || '';
            live.can_confirm = (o.status === 'delivered');
            orderShapeMap.set(String(o.id), sid);
        }

        liveData.set(sid, live);
    }
}

// Debounced re-fetch — called when an SSE order event arrives for an
// unknown order ID (externally created order). Waits 500ms to batch
// multiple rapid events, then re-fetches /api/orders/active.
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

    // Hot-swap: both robots staged → release both
    if (live.hot_swap && live.order_status === 'staged') {
        await releaseHotSwap(hit);
        return;
    }

    // Single-order staged → release
    if (!live.hot_swap && live.order_status === 'staged' && live.order_id) {
        await releaseOrder(live.order_id, hit);
        return;
    }

    // Confirm delivery (hot-swap or single)
    if (live.can_confirm && live.order_id) {
        await confirmDelivery(live.order_id, hit);
        return;
    }

    // Order in progress — button disabled
    if (live.order_status && !['confirmed', 'cancelled', 'failed'].includes(live.order_status)) {
        return;
    }

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

        if (data.hot_swap) {
            // Two-robot hot-swap response
            live.hot_swap = true;
            live.resupply_order_id = data.resupply.id;
            live.removal_order_id = data.removal.id;
            live.resupply_status = data.resupply.status;
            live.removal_status = data.removal.status;
            live.order_id = data.resupply.id; // primary for confirm
            live.order_status = deriveHotSwapStatus(live);
            liveData.set(shape.id, live);

            orderShapeMap.set(String(data.resupply.id), shape.id);
            orderShapeMap.set(String(data.removal.id), shape.id);

            showNotification('Hot-swap orders created', 'success');
        } else {
            // Simple single-order response
            const order = data.resupply;
            live.hot_swap = false;
            live.order_id = order.id;
            live.order_status = order.status;
            liveData.set(shape.id, live);

            orderShapeMap.set(String(order.id), shape.id);

            showNotification('Order created', 'success');
        }
    } catch (err) {
        live.order_status = '';
        live.hot_swap = false;
        liveData.set(shape.id, live);
        showNotification('Order failed: ' + err.message, 'error');
    }
}

// Derive the overall card status from two hot-swap order statuses.
// Both must be 'staged' for the card to show RELEASE.
// If one is staged and the other is still in transit, show 'positioning'.
function deriveHotSwapStatus(live) {
    const a = live.resupply_status || '';
    const b = live.removal_status || '';

    // Terminal states propagate immediately
    if (a === 'failed' || b === 'failed') return 'failed';
    if (a === 'cancelled' || b === 'cancelled') return 'cancelled';

    // Both staged → ready for release
    if (a === 'staged' && b === 'staged') return 'staged';

    // Delivered (post-release) — resupply drives to line
    if (a === 'delivered') return 'delivered';

    // One staged, other still moving → positioning
    if (a === 'staged' || b === 'staged') return 'positioning';

    // Both in transit
    if (a === 'in_transit' || b === 'in_transit') return 'in_transit';

    // Fallback to resupply status
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

        // Clear after brief confirmation display — clean up all tracked IDs
        const oid = String(orderId);
        const resupplyOid = live.resupply_order_id ? String(live.resupply_order_id) : null;
        const removalOid = live.removal_order_id ? String(live.removal_order_id) : null;

        setTimeout(() => {
            const l = getLive(shape.id);
            l.order_status = '';
            l.order_id = null;
            l.can_confirm = false;
            l.hot_swap = false;
            l.resupply_order_id = null;
            l.removal_order_id = null;
            l.resupply_status = '';
            l.removal_status = '';
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

        showNotification('Order released — delivering to line', 'success');
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
        // Fire both releases simultaneously
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

        // Only update state after both succeed
        live.resupply_status = 'in_transit';
        live.removal_status = 'in_transit';
        live.order_status = 'in_transit';
        liveData.set(shape.id, live);

        showNotification('Hot-swap released — both robots moving', 'success');
    } catch (err) {
        // Revert to staged so operator can retry
        live.order_status = 'staged';
        liveData.set(shape.id, live);
        showNotification('Release failed: ' + err.message, 'error');
    }
}

// ── UI helpers ──────────────────────────────────────────────────────

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
