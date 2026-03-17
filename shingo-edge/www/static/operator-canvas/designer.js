// designer.js — Operator Screen Designer
// Drag-and-drop canvas editor for building operator screens.

import {
    CANVAS_W, CANVAS_H, HANDLE_SIZE, MIN_SIZE,
    renderFrame, fitCanvas, canvasCoords,
    hitTest, hitHandle, applyShapeProxy,
} from './render.js';
import { createShape, cloneShape, serializeShapes, hydrateShapes } from './shapes.js';

const state = { shapes: [], selectedId: null, screenId: null, screenName: '', dirty: false };
let canvas, ctx, scale;
let dragging = null, resizing = null;
let payloads = [], lines = [];

export function initDesigner(canvasEl, screenData, payloadList, lineList) {
    canvas = canvasEl;
    ctx = canvas.getContext('2d');
    scale = fitCanvas(canvas);
    payloads = payloadList || [];
    lines = lineList || [];

    if (screenData) {
        state.screenId = screenData.id;
        state.screenName = screenData.name || '';
        state.shapes = hydrateShapes(screenData.layout);
    }

    document.querySelectorAll('.palette-item').forEach(el => {
        el.addEventListener('dragstart', e => { e.dataTransfer.setData('text/plain', el.dataset.type); });
    });

    canvas.addEventListener('dragover', e => e.preventDefault());
    canvas.addEventListener('drop', e => {
        e.preventDefault();
        const type = e.dataTransfer.getData('text/plain');
        if (!type) return;
        const { x, y } = canvasCoords(canvas, scale, e.clientX, e.clientY);
        const shape = createShape(type, x - 80, y - 40);
        state.shapes.push(shape);
        state.selectedId = shape.id;
        state.dirty = true;
        openPropertyPanel(shape);
        draw();
    });

    canvas.addEventListener('mousedown', onMouseDown);
    canvas.addEventListener('mousemove', onMouseMove);
    canvas.addEventListener('mouseup', onMouseUp);
    canvas.addEventListener('dblclick', onDoubleClick);

    document.addEventListener('keydown', e => {
        if (['INPUT','TEXTAREA','SELECT'].includes(e.target.tagName)) return;
        if ((e.key === 'Delete' || e.key === 'Backspace') && state.selectedId) {
            state.shapes = state.shapes.filter(s => s.id !== state.selectedId);
            state.selectedId = null; state.dirty = true; closePropertyPanel(); draw();
        }
        if ((e.ctrlKey || e.metaKey) && e.key === 's') { e.preventDefault(); saveScreen(); }
    });

    window.addEventListener('resize', () => { scale = fitCanvas(canvas); draw(); });
    draw();
}

function onMouseDown(e) {
    const { x, y } = canvasCoords(canvas, scale, e.clientX, e.clientY);
    const handleHit = hitHandle(state.shapes, state.selectedId, x, y);
    if (handleHit) {
        const s = handleHit.shape;
        resizing = { shape: s, handle: handleHit.handle, startX: x, startY: y, startW: s.config.w, startH: s.config.h, origX: s.config.x, origY: s.config.y };
        return;
    }
    const hit = hitTest(state.shapes, x, y);
    if (hit) {
        state.selectedId = hit.id;
        dragging = { shape: hit, offX: x - hit.config.x, offY: y - hit.config.y };
        if (e.altKey) { const clone = cloneShape(hit); state.shapes.push(clone); state.selectedId = clone.id; dragging.shape = clone; state.dirty = true; }
        openPropertyPanel(hit); draw();
    } else { state.selectedId = null; closePropertyPanel(); draw(); }
}

function onMouseMove(e) {
    const { x, y } = canvasCoords(canvas, scale, e.clientX, e.clientY);
    if (dragging) {
        const s = dragging.shape;
        s.config.x = Math.max(0, Math.min(CANVAS_W - s.config.w, x - dragging.offX));
        s.config.y = Math.max(0, Math.min(CANVAS_H - s.config.h, y - dragging.offY));
        state.dirty = true; draw(); return;
    }
    if (resizing) {
        const r = resizing, dx = x - r.startX, dy = y - r.startY, s = r.shape;
        if (r.handle === 'se') { s.config.w = Math.max(MIN_SIZE, r.startW+dx); s.config.h = Math.max(MIN_SIZE, r.startH+dy); }
        else if (r.handle === 'sw') { s.config.x = Math.min(r.origX+r.startW-MIN_SIZE, r.origX+dx); s.config.w = Math.max(MIN_SIZE, r.startW-dx); s.config.h = Math.max(MIN_SIZE, r.startH+dy); }
        else if (r.handle === 'ne') { s.config.w = Math.max(MIN_SIZE, r.startW+dx); s.config.y = Math.min(r.origY+r.startH-MIN_SIZE, r.origY+dy); s.config.h = Math.max(MIN_SIZE, r.startH-dy); }
        else if (r.handle === 'nw') { s.config.x = Math.min(r.origX+r.startW-MIN_SIZE, r.origX+dx); s.config.w = Math.max(MIN_SIZE, r.startW-dx); s.config.y = Math.min(r.origY+r.startH-MIN_SIZE, r.origY+dy); s.config.h = Math.max(MIN_SIZE, r.startH-dy); }
        state.dirty = true; draw(); return;
    }
    const hh = hitHandle(state.shapes, state.selectedId, x, y);
    if (hh) { canvas.style.cursor = (hh.handle === 'nw' || hh.handle === 'se') ? 'nwse-resize' : 'nesw-resize'; }
    else { canvas.style.cursor = hitTest(state.shapes, x, y) ? 'grab' : 'default'; }
}

function onMouseUp() { dragging = null; resizing = null; canvas.style.cursor = 'default'; }

function onDoubleClick(e) {
    const { x, y } = canvasCoords(canvas, scale, e.clientX, e.clientY);
    const hit = hitTest(state.shapes, x, y);
    if (hit) { state.selectedId = hit.id; openPropertyPanel(hit); setTimeout(() => { const inp = document.querySelector('#prop-panel input, #prop-panel select'); if (inp) inp.focus(); }, 50); }
}

function draw() {
    renderFrame(ctx, state.shapes, null);
    if (state.selectedId) {
        const s = state.shapes.find(sh => sh.id === state.selectedId);
        if (s) {
            const c = s.config;
            ctx.save(); ctx.setLineDash([6,4]); ctx.strokeStyle = '#00BFFF'; ctx.lineWidth = 2;
            ctx.strokeRect(c.x, c.y, c.w, c.h); ctx.setLineDash([]);
            ctx.fillStyle = '#00BFFF';
            for (const corner of [{x:c.x,y:c.y},{x:c.x+c.w,y:c.y},{x:c.x,y:c.y+c.h},{x:c.x+c.w,y:c.y+c.h}]) {
                ctx.fillRect(corner.x-HANDLE_SIZE/2, corner.y-HANDLE_SIZE/2, HANDLE_SIZE, HANDLE_SIZE);
            }
            ctx.restore();
        }
    }
    requestAnimationFrame(draw);
}

function openPropertyPanel(shape) {
    const panel = document.getElementById('prop-panel');
    if (!panel) return;
    panel.style.display = 'block';
    let html = `<h3 style="margin:0 0 12px;color:#fff;text-transform:capitalize">${shape.type}</h3>`;
    const c = shape.config;

    if (shape.type === 'ordercombo') {
        html += propSelect('lineId', 'Line', lines.map(l => ({v:l.id,t:l.name})), c.lineId);
        html += propSelect('payloadId', 'Payload', payloads.map(p => ({v:p.id,t:`${p.description} (${p.payload_code})`})), c.payloadId);
        html += propInput('description', 'Description Override', c.description);
        html += propInput('actionLabel', 'Button Label', c.actionLabel);
        html += propSelect('actionType', 'Action Type', [{v:'retrieve',t:'Retrieve'},{v:'store',t:'Store'}], c.actionType);
        html += propCheckbox('retrieveEmpty', 'Retrieve Empty', c.retrieveEmpty);
    } else if (shape.type === 'header') {
        html += propInput('text', 'Title Text', c.text);
    } else if (shape.type === 'statusbar') {
        html += propSelect('lineId', 'Line', lines.map(l => ({v:l.id,t:l.name})), c.lineId);
        html += propInput('lineName', 'Line Name Label', c.lineName);
    } else if (shape.type === 'label') {
        html += propInput('text', 'Text', c.text);
        html += propInput('fontSize', 'Font Size', c.fontSize, 'number');
        html += `<label>Color</label><input type="color" data-prop="textColor" class="prop-input" value="${c.textColor||'#FFFFFF'}" />`;
        html += propSelect('textAlign', 'Align', [{v:'left',t:'Left'},{v:'center',t:'Center'},{v:'right',t:'Right'}], c.textAlign);
    }

    html += `<div style="margin-top:16px;padding-top:12px;border-top:1px solid #444"><button onclick="window._designerDeleteShape()" class="btn-danger">Delete</button></div>`;
    panel.innerHTML = html;

    panel.querySelectorAll('[data-prop]').forEach(el => {
        el.addEventListener('change', () => {
            let val = el.value;
            if (el.type === 'number') val = parseFloat(val) || 0;
            if (el.type === 'checkbox') val = el.checked;
            shape.config[el.dataset.prop] = val;
            state.dirty = true;
        });
    });
}

function closePropertyPanel() { const p = document.getElementById('prop-panel'); if (p) p.style.display = 'none'; }

function propInput(prop, label, val, type='text') {
    return `<label>${label}</label><input data-prop="${prop}" class="prop-input" type="${type}" value="${esc(String(val||''))}" />`;
}
function propSelect(prop, label, opts, val) {
    let html = `<label>${label}</label><select data-prop="${prop}" class="prop-input"><option value="">—</option>`;
    for (const o of opts) html += `<option value="${o.v}" ${String(val)==String(o.v)?'selected':''}>${esc(o.t)}</option>`;
    return html + '</select>';
}
function propCheckbox(prop, label, val) {
    return `<label><input type="checkbox" data-prop="${prop}" ${val?'checked':''} /> ${label}</label>`;
}
function esc(s) { return (s||'').replace(/"/g,'&quot;').replace(/</g,'&lt;'); }

window._designerDeleteShape = () => { if (state.selectedId) { state.shapes = state.shapes.filter(s => s.id !== state.selectedId); state.selectedId = null; state.dirty = true; closePropertyPanel(); } };

async function saveScreen() {
    const layout = serializeShapes(state.shapes);
    try {
        if (!state.screenId) {
            const res = await fetch('/api/operator-screens', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: state.screenName || 'Untitled', layout }) });
            if (!res.ok) throw new Error(await res.text());
            const data = await res.json();
            state.screenId = data.id; state.screenName = data.name;
        } else {
            const res = await fetch(`/api/operator-screens/${state.screenId}/layout`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(layout) });
            if (!res.ok) throw new Error(await res.text());
        }
        state.dirty = false; showToast('Screen saved');
    } catch (err) { showToast('Save failed: ' + err.message, 'error'); }
}

function showToast(msg, type='success') {
    const t = document.createElement('div');
    t.className = 'toast toast-' + type; t.textContent = msg;
    document.body.appendChild(t); setTimeout(() => t.remove(), 3000);
}

window._designerSave = saveScreen;
window._designerState = state;

export { state, saveScreen };
