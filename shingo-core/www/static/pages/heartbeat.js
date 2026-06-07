// heartbeat.js — the /heartbeat production kiosk (Phase F). A wall display of
// cell tiles plus a live rhythm strip, fed by cell-heartbeat SSE. Built to run
// for days without reload, so every §13 leak vector is addressed:
//
//   • Rhythm strip = ONE requestAnimationFrame loop drawing a <canvas> from a
//     fixed, pre-allocated ring buffer. A fire pushes a number into the ring
//     (overwriting the oldest) — never a DOM node, so the strip can't grow.
//   • visibilitychange pauses the rAF loop while the display is hidden, so a
//     blanked monitor burns no CPU and there's no tick-backlog to render on
//     return (the loop draws against the live clock, not a queue).
//   • Tiles update in place (updateCellTile / pulseCellDot reuse their DOM).
//   • New Core build → location.reload() (setSSEReloadOnBuild) since there's no
//     operator to dismiss a banner.
//   • Clock is server-synced (offset from the connected + cell-heartbeat ts) so
//     "X ago" doesn't drift over a long soak.

import { onSSE, setSSEReloadOnBuild } from '/static/shared/utils.js';
import { CellTile, updateCellTile, pulseCellDot } from '/static/components/CellTile.js';
import { openCellDrill } from '/static/components/CellDrill.js';

setSSEReloadOnBuild(true);

// ─── server-synced clock ────────────────────────────────────────────────────
let clockOffset = 0; // serverNow - localNow (ms)
function syncClock(tsStr) {
    if (!tsStr) return;
    const t = Date.parse(tsStr);
    if (!isNaN(t)) clockOffset = t - Date.now();
}
function serverNow() { return Date.now() + clockOffset; }

// ─── cells ──────────────────────────────────────────────────────────────────
const cellTiles = new Map(); // cell_id -> tile
const cellIndex = new Map();  // cell_id -> stable index (rhythm color)
let cellList = [];
const cellStateTimers = {};

function loadCells() {
    fetch('/api/cells').then((r) => r.json()).then((list) => {
        cellList = list || [];
        const grid = document.getElementById('hb-grid');
        const empty = document.getElementById('hb-empty');
        if (!grid) return;
        grid.innerHTML = '';
        cellTiles.clear();
        cellIndex.clear();
        if (!cellList.length) {
            grid.appendChild(el('div', 'hb-empty', 'No cells configured. Set them up at /admin/cells.'));
            return;
        }
        cellList.forEach((c, i) => {
            cellIndex.set(c.cell_id, i);
            const tile = CellTile(c);
            tile.addEventListener('click', () => openCellDrill(c.cell_id));
            cellTiles.set(c.cell_id, tile);
            grid.appendChild(tile);
            refreshCellState(c.cell_id);
            seedRhythm(c.cell_id, i);
        });
    }).catch(() => {});
}

function refreshCellState(cellID) {
    fetch('/api/cells/' + encodeURIComponent(cellID) + '/state')
        .then((r) => r.json())
        .then((s) => { const t = cellTiles.get(cellID); if (t) updateCellTile(t, s); })
        .catch(() => {});
}

// seedRhythm pre-fills the ring buffer from the last couple minutes of stored
// fires so the strip isn't blank on first paint (live cell-heartbeat events
// stream in on top). Only fires within the strip window end up drawn.
function seedRhythm(cellID, cellIdx) {
    const since = new Date(Date.now() - 2 * 60 * 1000).toISOString();
    fetch('/api/cells/' + encodeURIComponent(cellID) + '/heartbeat?since=' + encodeURIComponent(since))
        .then((r) => r.json())
        .then((data) => {
            (data.processes || []).forEach((p) => {
                (p.events || []).forEach((e) => {
                    const t = Date.parse(e.recorded_at);
                    if (!isNaN(t)) pushFire(t, cellIdx);
                });
            });
        })
        .catch(() => {});
}

function scheduleCellState(cellID) {
    if (cellStateTimers[cellID]) return;
    cellStateTimers[cellID] = setTimeout(() => {
        delete cellStateTimers[cellID];
        refreshCellState(cellID);
    }, 2000);
}

// ─── rhythm ring buffer ─────────────────────────────────────────────────────
const RING = 4096;
const ringT = new Float64Array(RING); // server-time ms of each fire
const ringC = new Int32Array(RING);   // cell index (color), -1 = unmatched
let ringHead = 0;
const WINDOW_MS = 60000;

function pushFire(tMs, cellIdx) {
    ringT[ringHead] = tMs;
    ringC[ringHead] = cellIdx;
    ringHead = (ringHead + 1) % RING;
}

function onFire(data) {
    if (!data) return;
    syncClock(data.ts);
    const tMs = data.recorded_at ? Date.parse(data.recorded_at) : serverNow();
    const pid = Number(data.process_id);
    let matchedIdx = -1;
    cellList.forEach((c) => {
        if (c.station !== data.station) return;
        if (c.primary_process_id !== pid && (c.sub_process_ids || []).indexOf(pid) < 0) return;
        matchedIdx = cellIndex.get(c.cell_id);
        const tile = cellTiles.get(c.cell_id);
        if (tile) { pulseCellDot(tile, pid); scheduleCellState(c.cell_id); }
    });
    pushFire(isNaN(tMs) ? serverNow() : tMs, matchedIdx);
}

// ─── rAF draw loop ──────────────────────────────────────────────────────────
let rafId = null;
let paused = false;
let lastDraw = 0;
let canvas = null;
let ctx = null;
let cssW = 0, cssH = 0;

function sizeCanvas() {
    if (!canvas) return;
    const dpr = window.devicePixelRatio || 1;
    const rect = canvas.getBoundingClientRect();
    cssW = rect.width; cssH = rect.height;
    canvas.width = Math.max(1, Math.round(cssW * dpr));
    canvas.height = Math.max(1, Math.round(cssH * dpr));
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
}

function draw(ts) {
    rafId = null;
    if (!ctx) return;
    // Throttle to ~15fps — the strip scrolls slowly; full 60fps is wasted CPU
    // on a long-running wall PC.
    if (ts - lastDraw >= 66) {
        lastDraw = ts;
        const now = serverNow();
        ctx.clearRect(0, 0, cssW, cssH);
        // baseline
        ctx.strokeStyle = 'rgba(128,128,128,0.25)';
        ctx.lineWidth = 1;
        ctx.beginPath();
        ctx.moveTo(0, cssH - 0.5);
        ctx.lineTo(cssW, cssH - 0.5);
        ctx.stroke();
        // each fire in the window as a vertical line; newest at the right edge
        for (let i = 0; i < RING; i++) {
            const t = ringT[i];
            if (!t) continue;
            const age = now - t;
            if (age < 0 || age > WINDOW_MS) continue;
            const x = (1 - age / WINDOW_MS) * cssW;
            const idx = ringC[i];
            ctx.strokeStyle = idx < 0 ? 'rgba(148,163,184,0.55)' : 'hsl(' + ((idx * 47) % 360) + ',70%,60%)';
            // fade older fires
            ctx.globalAlpha = 0.25 + 0.75 * (1 - age / WINDOW_MS);
            ctx.beginPath();
            ctx.moveTo(x, cssH);
            ctx.lineTo(x, 4);
            ctx.stroke();
        }
        ctx.globalAlpha = 1;
    }
    if (!paused) rafId = requestAnimationFrame(draw);
}

function startLoop() {
    if (rafId == null && !paused) rafId = requestAnimationFrame(draw);
}
function stopLoop() {
    if (rafId != null) { cancelAnimationFrame(rafId); rafId = null; }
}

// ─── clock text (1Hz; separate from the rAF strip) ──────────────────────────
function tickClock() {
    const elc = document.getElementById('hb-clock');
    if (elc) elc.textContent = new Date(serverNow()).toLocaleTimeString();
}

// ─── connection pill ────────────────────────────────────────────────────────
function setConnected(on) {
    const c = document.getElementById('hb-conn');
    if (!c) return;
    c.classList.toggle('is-live', !!on);
    c.textContent = on ? 'live' : 'offline';
}

// ─── boot ───────────────────────────────────────────────────────────────────
function init() {
    canvas = document.getElementById('hb-rhythm-canvas');
    if (canvas) { ctx = canvas.getContext('2d'); sizeCanvas(); }
    window.addEventListener('resize', sizeCanvas);

    document.addEventListener('visibilitychange', () => {
        paused = document.hidden;
        if (paused) stopLoop(); else { lastDraw = 0; startLoop(); }
    });

    onSSE('connected', (d) => { setConnected(true); syncClock(d && d.ts); loadCells(); });
    onSSE('disconnected', () => setConnected(false));
    onSSE('cell-heartbeat', onFire);

    loadCells();
    tickClock();
    setInterval(tickClock, 1000);
    startLoop();
}

// tiny DOM helper (avoids importing app.js's el for one node)
function el(tag, cls, text) {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text) n.textContent = text;
    return n;
}

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
else init();
