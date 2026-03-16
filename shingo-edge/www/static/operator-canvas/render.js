// render.js — Operator Canvas Renderer
// Adapted from Andon v4 screen designer for Shingo Edge operator screens.
// Provides a canvas-based rendering engine for operator interaction elements:
//   - OrderCombo: payload status + bin level + order button + status badge
//   - Header: gradient banner (reused from andon)
//   - StatusBar: connection / line status indicator
//   - Label: freeform text label

export const CANVAS_W = 1920;
export const CANVAS_H = 1080;
export const HANDLE_SIZE = 10;
export const MIN_SIZE = 40;

// ── Flash timer ──────────────────────────────────────────────────────
let flashOn = true;
setInterval(() => { flashOn = !flashOn; }, 500);
export function isFlashOn() { return flashOn; }

// ── Default sizes per element type ───────────────────────────────────
export const DEFAULT_SIZES = {
    ordercombo:  { w: 320, h: 200 },
    header:      { w: CANVAS_W, h: 100 },
    statusbar:   { w: CANVAS_W, h: 50 },
    label:       { w: 200, h: 60 },
};

// ── Shape proxy: maps .x/.y/.w/.h to .config.* ──────────────────────
export function applyShapeProxy(s) {
    if (!s.config) return s;
    for (const prop of ['x', 'y', 'w', 'h']) {
        if (s.config[prop] !== undefined) {
            Object.defineProperty(s, prop, {
                get() { return this.config[prop]; },
                set(v) { this.config[prop] = v; },
                configurable: true,
                enumerable: true,
            });
        }
    }
    return s;
}

// ── Color utilities ──────────────────────────────────────────────────
function statusColor(status) {
    switch (status) {
        case 'active':       return '#4CAF50';
        case 'replenishing': return '#FF9800';
        case 'empty':        return '#F44336';
        default:             return '#9E9E9E';
    }
}

function orderStatusColor(status) {
    switch (status) {
        case 'pending':      return '#9E9E9E';
        case 'submitted':    return '#2196F3';
        case 'acknowledged': return '#2196F3';
        case 'in_transit':   return '#FF9800';
        case 'positioning':  return '#FF9800';
        case 'staged':       return '#9C27B0';
        case 'delivered':    return '#4CAF50';
        case 'confirmed':    return '#388E3C';
        case 'cancelled':    return '#757575';
        case 'failed':       return '#D32F2F';
        default:             return '#9E9E9E';
    }
}

function orderStatusLabel(status) {
    switch (status) {
        case 'pending':      return 'PENDING';
        case 'submitted':    return 'SUBMITTED';
        case 'acknowledged': return 'ACKNOWLEDGED';
        case 'in_transit':   return 'IN TRANSIT';
        case 'positioning':  return 'POSITIONING';
        case 'staged':       return 'STAGED';
        case 'delivered':    return 'DELIVERED';
        case 'confirmed':    return 'CONFIRMED';
        case 'cancelled':    return 'CANCELLED';
        case 'failed':       return 'FAILED';
        default:             return '';
    }
}

// ── OrderCombo rendering ─────────────────────────────────────────────
// Layout (within bounding box w×h):
// ┌──────────────────────────────────┐
// │  PAYLOAD DESCRIPTION      [BADGE]│  ← title bar (18% height)
// ├──────────────────────────────────┤
// │  ████████████░░░░░░░░  75%       │  ← bin level bar (14% height)
// ├──────────────────────────────────┤
// │        [ REQUEST ]               │  ← action button (38% height)
// ├──────────────────────────────────┤
// │  Status: IN TRANSIT  ETA: 2min   │  ← status footer (remaining)
// └──────────────────────────────────┘

function drawOrderCombo(ctx, cfg, live) {
    const { x, y, w, h } = cfg;
    const pad = 8, cornerR = 12;
    const titleH = h * 0.18, barH = h * 0.14, btnH = h * 0.38;
    const footerH = h - titleH - barH - btnH;

    const payloadStatus = (live && live.payload_status) || cfg.payloadStatus || 'active';
    const remainingPct = (live && live.remaining_pct != null) ? live.remaining_pct : (cfg.remainingPct != null ? cfg.remainingPct : 100);
    const remaining = (live && live.remaining != null) ? live.remaining : (cfg.remaining != null ? cfg.remaining : '—');
    const total = (live && live.total != null) ? live.total : (cfg.total != null ? cfg.total : '—');
    const orderStatus = (live && live.order_status) || cfg.orderStatus || '';
    const orderETA = (live && live.order_eta) || cfg.orderETA || '';
    const description = cfg.description || cfg.payloadCode || 'Payload';
    const actionLabel = cfg.actionLabel || 'REQUEST';
    const canConfirm = (live && live.can_confirm) || false;
    const isFlashing = payloadStatus === 'empty' && flashOn;

    ctx.save();

    // Shadow
    ctx.shadowColor = 'rgba(0,0,0,0.3)';
    ctx.shadowBlur = 10;
    ctx.shadowOffsetX = 2;
    ctx.shadowOffsetY = 2;

    // Card background
    ctx.beginPath();
    roundRect(ctx, x, y, w, h, cornerR);
    ctx.fillStyle = '#1E1E1E';
    ctx.fill();
    ctx.shadowColor = 'transparent';

    // Border glow when empty/replenishing
    if (payloadStatus === 'empty' || payloadStatus === 'replenishing') {
        ctx.strokeStyle = isFlashing ? '#FF0000' : statusColor(payloadStatus);
        ctx.lineWidth = 4;
        ctx.stroke();
    } else {
        ctx.strokeStyle = '#333';
        ctx.lineWidth = 2;
        ctx.stroke();
    }

    // ── Title bar ────────────────────────────────────────────────
    ctx.beginPath();
    roundRectTop(ctx, x, y, w, titleH, cornerR);
    ctx.fillStyle = '#2A2A2A';
    ctx.fill();

    const titleFontSize = Math.min(titleH * 0.55, 24);
    ctx.font = `bold ${titleFontSize}px Arial`;
    ctx.fillStyle = '#FFFFFF';
    ctx.textAlign = 'left';
    ctx.textBaseline = 'middle';
    ctx.fillText(description, x + pad, y + titleH / 2, w - pad * 2 - 80);

    // Status badge (top right)
    if (orderStatus) {
        const badgeColor = orderStatusColor(orderStatus);
        const badgeLabel = orderStatusLabel(orderStatus);
        const badgeFontSize = Math.min(titleFontSize * 0.7, 14);
        ctx.font = `bold ${badgeFontSize}px Arial`;
        const badgeW = ctx.measureText(badgeLabel).width + 12;
        const badgeH = titleH * 0.55;
        const badgeX = x + w - pad - badgeW;
        const badgeY = y + (titleH - badgeH) / 2;

        ctx.beginPath();
        roundRect(ctx, badgeX, badgeY, badgeW, badgeH, 4);
        ctx.fillStyle = badgeColor;
        ctx.fill();

        ctx.fillStyle = '#FFFFFF';
        ctx.textAlign = 'center';
        ctx.fillText(badgeLabel, badgeX + badgeW / 2, badgeY + badgeH / 2);
    }

    // ── Bin level bar ────────────────────────────────────────────
    const barY = y + titleH;
    const barInnerW = w - pad * 2;
    const barInnerH = barH - pad;
    const barX = x + pad;
    const barYInner = barY + pad / 2;

    ctx.beginPath();
    roundRect(ctx, barX, barYInner, barInnerW, barInnerH, 4);
    ctx.fillStyle = '#333';
    ctx.fill();

    const fillW = Math.max(0, Math.min(1, remainingPct / 100)) * barInnerW;
    if (fillW > 0) {
        ctx.beginPath();
        roundRect(ctx, barX, barYInner, fillW, barInnerH, 4);
        const barColor = remainingPct > 50 ? '#4CAF50' : remainingPct > 25 ? '#FF9800' : '#F44336';
        ctx.fillStyle = barColor;
        ctx.fill();
    }

    const barFontSize = Math.min(barInnerH * 0.7, 16);
    ctx.font = `bold ${barFontSize}px Arial`;
    ctx.fillStyle = '#FFFFFF';
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    ctx.fillText(`${remaining} / ${total}  (${Math.round(remainingPct)}%)`, barX + barInnerW / 2, barYInner + barInnerH / 2);

    // ── Action button ────────────────────────────────────────────
    const btnY = barY + barH;
    const btnPadX = w * 0.1, btnPadY = btnH * 0.15;
    const btnW = w - btnPadX * 2, btnActualH = btnH - btnPadY * 2;
    const btnX = x + btnPadX, btnYInner = btnY + btnPadY;

    let btnColor, btnText, btnTextColor;
    if (canConfirm) {
        btnColor = '#4CAF50'; btnText = 'CONFIRM DELIVERY'; btnTextColor = '#FFFFFF';
    } else if (orderStatus === 'staged') {
        btnColor = '#FF9800'; btnText = 'RELEASE'; btnTextColor = '#FFFFFF';
    } else if (orderStatus === 'positioning') {
        btnColor = '#555'; btnText = 'POSITIONING...'; btnTextColor = '#FF9800';
    } else if (payloadStatus === 'empty' && !orderStatus) {
        btnColor = '#F44336'; btnText = actionLabel; btnTextColor = '#FFFFFF';
    } else if (orderStatus && orderStatus !== 'confirmed' && orderStatus !== 'cancelled' && orderStatus !== 'failed') {
        btnColor = '#555'; btnText = 'ORDER IN PROGRESS'; btnTextColor = '#AAA';
    } else {
        btnColor = '#1976D2'; btnText = actionLabel; btnTextColor = '#FFFFFF';
    }

    ctx.shadowColor = 'rgba(0,0,0,0.4)';
    ctx.shadowBlur = 6;
    ctx.shadowOffsetY = 2;
    ctx.beginPath();
    roundRect(ctx, btnX, btnYInner, btnW, btnActualH, 8);
    ctx.fillStyle = btnColor;
    ctx.fill();
    ctx.shadowColor = 'transparent';

    const btnFontSize = Math.min(btnActualH * 0.35, 28);
    ctx.font = `bold ${btnFontSize}px Arial`;
    ctx.fillStyle = btnTextColor;
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    ctx.fillText(btnText, btnX + btnW / 2, btnYInner + btnActualH / 2);

    cfg._btnBounds = { x: btnX, y: btnYInner, w: btnW, h: btnActualH };

    // ── Footer ───────────────────────────────────────────────────
    const footerY = btnY + btnH;
    const footerFontSize = Math.min(footerH * 0.5, 16);
    ctx.font = `${footerFontSize}px Arial`;
    ctx.fillStyle = '#888';
    ctx.textAlign = 'left';
    ctx.textBaseline = 'middle';

    if (orderStatus) {
        let footerText = `Status: ${orderStatusLabel(orderStatus)}`;
        if (orderETA) footerText += `   ETA: ${orderETA}`;
        ctx.fillText(footerText, x + pad, footerY + footerH / 2, w - pad * 2);
    } else {
        ctx.fillText('No active order', x + pad, footerY + footerH / 2);
    }

    ctx.restore();
}

// ── Header rendering ─────────────────────────────────────────────────
function drawHeader(ctx, cfg) {
    const { x, y, w, h, text } = cfg;
    const grad = ctx.createLinearGradient(x, y, x, y + h);
    grad.addColorStop(0, '#001B3D');
    grad.addColorStop(1, '#003A7A');
    ctx.fillStyle = grad;
    ctx.fillRect(x, y, w, h);

    const highlight = ctx.createLinearGradient(x, y + h * 0.7, x, y + h);
    highlight.addColorStop(0, 'rgba(255,255,255,0)');
    highlight.addColorStop(1, 'rgba(255,255,255,0.05)');
    ctx.fillStyle = highlight;
    ctx.fillRect(x, y, w, h);

    if (text) {
        const fontSize = Math.round(h * 0.55);
        ctx.font = `bold ${fontSize}px "Arial Black", Arial, sans-serif`;
        ctx.fillStyle = '#FFFFFF';
        ctx.textAlign = 'center';
        ctx.textBaseline = 'middle';
        ctx.fillText(text, x + w * 0.5, y + h * 0.5);
    }
}

// ── StatusBar rendering ──────────────────────────────────────────────
function drawStatusBar(ctx, cfg, live) {
    const { x, y, w, h } = cfg;
    const connected = (live && live.connected != null) ? live.connected : true;
    const lineName = cfg.lineName || 'Line';
    const styleName = (live && live.style_name) || cfg.styleName || '';

    ctx.fillStyle = connected ? '#1B5E20' : '#B71C1C';
    ctx.fillRect(x, y, w, h);

    const dotR = h * 0.25;
    ctx.beginPath();
    ctx.arc(x + h / 2, y + h / 2, dotR, 0, Math.PI * 2);
    ctx.fillStyle = connected ? '#69F0AE' : (flashOn ? '#FF5252' : '#B71C1C');
    ctx.fill();

    const fontSize = Math.min(h * 0.55, 20);
    ctx.font = `bold ${fontSize}px Arial`;
    ctx.fillStyle = '#FFFFFF';
    ctx.textAlign = 'left';
    ctx.textBaseline = 'middle';
    ctx.fillText(connected ? `${lineName} — ${styleName || 'No Style'}` : `${lineName} — DISCONNECTED`, x + h + 8, y + h / 2);
}

// ── Label rendering ──────────────────────────────────────────────────
function drawLabel(ctx, cfg) {
    const { x, y, w, h, text } = cfg;
    const fontSize = cfg.fontSize || Math.min(h * 0.6, 32);
    const align = cfg.textAlign || 'center';

    ctx.font = `bold ${fontSize}px Arial`;
    ctx.fillStyle = cfg.textColor || '#FFFFFF';
    ctx.textAlign = align;
    ctx.textBaseline = 'middle';

    const tx = align === 'center' ? x + w / 2 : (align === 'right' ? x + w : x);
    ctx.fillText(text || '', tx, y + h / 2, w);
}

// ── Rounded rectangle helpers ────────────────────────────────────────
function roundRect(ctx, x, y, w, h, r) {
    r = Math.min(r, w / 2, h / 2);
    ctx.moveTo(x + r, y);
    ctx.arcTo(x + w, y, x + w, y + h, r);
    ctx.arcTo(x + w, y + h, x, y + h, r);
    ctx.arcTo(x, y + h, x, y, r);
    ctx.arcTo(x, y, x + w, y, r);
    ctx.closePath();
}

function roundRectTop(ctx, x, y, w, h, r) {
    r = Math.min(r, w / 2, h);
    ctx.moveTo(x + r, y);
    ctx.arcTo(x + w, y, x + w, y + h, r);
    ctx.lineTo(x + w, y + h);
    ctx.lineTo(x, y + h);
    ctx.arcTo(x, y, x + w, y, r);
    ctx.closePath();
}

// ── Hit testing ──────────────────────────────────────────────────────
export function hitTest(shapes, cx, cy) {
    for (let i = shapes.length - 1; i >= 0; i--) {
        const s = shapes[i];
        const c = s.config;
        if (cx >= c.x && cx <= c.x + c.w && cy >= c.y && cy <= c.y + c.h) return s;
    }
    return null;
}

export function hitButton(shape, cx, cy) {
    if (shape.type !== 'ordercombo') return false;
    const b = shape.config._btnBounds;
    if (!b) return false;
    return cx >= b.x && cx <= b.x + b.w && cy >= b.y && cy <= b.y + b.h;
}

export function hitHandle(shapes, selectedId, cx, cy) {
    if (!selectedId) return null;
    const s = shapes.find(sh => sh.id === selectedId);
    if (!s) return null;
    const c = s.config;
    const corners = [
        { name: 'nw', x: c.x, y: c.y },
        { name: 'ne', x: c.x + c.w, y: c.y },
        { name: 'sw', x: c.x, y: c.y + c.h },
        { name: 'se', x: c.x + c.w, y: c.y + c.h },
    ];
    for (const corner of corners) {
        if (Math.abs(cx - corner.x) <= HANDLE_SIZE && Math.abs(cy - corner.y) <= HANDLE_SIZE) {
            return { handle: corner.name, shape: s };
        }
    }
    return null;
}

// ── Main render frame ────────────────────────────────────────────────
export function renderFrame(ctx, shapes, liveData) {
    ctx.clearRect(0, 0, CANVAS_W, CANVAS_H);
    ctx.fillStyle = '#121212';
    ctx.fillRect(0, 0, CANVAS_W, CANVAS_H);

    for (const s of shapes) { if (s.type === 'header') drawHeader(ctx, s.config); }
    for (const s of shapes) { if (s.type === 'statusbar') drawStatusBar(ctx, s.config, liveData ? liveData.get(s.id) : null); }
    for (const s of shapes) { if (s.type === 'label') drawLabel(ctx, s.config); }
    for (const s of shapes) { if (s.type === 'ordercombo') drawOrderCombo(ctx, s.config, liveData ? liveData.get(s.id) : null); }
}

// ── Fit canvas to viewport ───────────────────────────────────────────
export function fitCanvas(canvas) {
    const parent = canvas.parentElement;
    const pw = parent.clientWidth, ph = parent.clientHeight;
    const scale = Math.min(pw / CANVAS_W, ph / CANVAS_H);
    canvas.style.width = (CANVAS_W * scale) + 'px';
    canvas.style.height = (CANVAS_H * scale) + 'px';
    canvas.width = CANVAS_W;
    canvas.height = CANVAS_H;
    return scale;
}

// ── Canvas coordinate transform ──────────────────────────────────────
export function canvasCoords(canvas, scale, clientX, clientY) {
    const rect = canvas.getBoundingClientRect();
    return { x: (clientX - rect.left) / scale, y: (clientY - rect.top) / scale };
}
