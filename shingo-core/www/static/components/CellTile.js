// CellTile — a production-cell tile (Phase E, Q-025). A pulsing primary dot for
// the cell's primary Process plus satellite dots for each sub-Process. Color =
// live state (running/slowed/micro-stop/stopped/no-data); a numeric badge shows
// the primary's current cycle time. Returns a .cell-tile node; updateCellTile()
// refreshes it in place and pulseCellDot() animates one dot on a tick — both
// reuse the same DOM so a 72h kiosk never accumulates nodes (§13). The pulse is
// a Web Animations call (no class juggling, no node-per-fire).
//
//   const tile = CellTile(cell);            // cell = a /api/cells row
//   updateCellTile(tile, resolvedState);    // resolvedState = /api/cells/{id}/state
//   pulseCellDot(tile, processID);          // on a cell-heartbeat for this cell
//
// Click handling is left to the caller (missions wires openCellDrill; the kiosk
// does too) so the component stays surface-agnostic.

import { el, h } from '/static/app.js';

const STATES = ['running', 'slowed', 'micro-stop', 'stopped', 'no-data'];

export function CellTile(cell) {
    const tile = el('div', { className: 'cell-tile', role: 'button', tabindex: '0' });
    tile.dataset.cellId = cell.cell_id;
    tile.dataset.station = cell.station || '';
    tile.dataset.primary = String(cell.primary_process_id);
    tile.dataset.subs = (cell.sub_process_ids || []).join(',');
    tile.innerHTML = h`
        <div class="cell-tile__head">
          <span class="cell-tile__name">${cell.display_name || cell.cell_id}</span>
          <span class="cell-tile__badge">—</span>
        </div>
        <div class="cell-tile__dots"></div>
        <div class="cell-tile__sub text-muted-sm"></div>`;
    const dots = tile.querySelector('.cell-tile__dots');
    dots.appendChild(makeDot(cell.primary_process_id, true));
    (cell.sub_process_ids || []).forEach((id) => dots.appendChild(makeDot(id, false)));
    return tile;
}

function makeDot(processID, isPrimary) {
    const dot = el('span', { className: 'cell-dot cell-dot--no-data' + (isPrimary ? ' cell-dot--primary' : ' cell-dot--sub') });
    dot.dataset.proc = String(processID);
    dot.title = (isPrimary ? 'Primary process ' : 'Process ') + processID;
    return dot;
}

export function updateCellTile(tile, state) {
    if (!tile || !state) return;
    const primary = state.primary || {};
    setDotState(tile, primary.process_id, primary.state);
    (state.sub_processes || []).forEach((p) => setDotState(tile, p.process_id, p.state));

    const badge = tile.querySelector('.cell-tile__badge');
    if (badge) badge.textContent = cycleLabel(primary.current_cycle_ms);

    const sub = tile.querySelector('.cell-tile__sub');
    if (sub) sub.textContent = stateSummary(primary);

    // Reflect the worst state on the tile frame so a stopped cell reads at a glance.
    tile.classList.remove.apply(tile.classList, STATES.map((s) => 'cell-tile--' + s));
    tile.classList.add('cell-tile--' + worstState(state));
}

export function pulseCellDot(tile, processID) {
    if (!tile) return;
    const dot = tile.querySelector('.cell-dot[data-proc="' + cssNum(processID) + '"]');
    if (!dot || typeof dot.animate !== 'function') return;
    dot.animate(
        [{ transform: 'scale(1)' }, { transform: 'scale(1.7)' }, { transform: 'scale(1)' }],
        { duration: 420, easing: 'ease-out' }
    );
}

// ─── helpers ────────────────────────────────────────────────────────────────
function setDotState(tile, processID, state) {
    if (processID === undefined || processID === null) return;
    const dot = tile.querySelector('.cell-dot[data-proc="' + cssNum(processID) + '"]');
    if (!dot) return;
    STATES.forEach((s) => dot.classList.remove('cell-dot--' + s));
    dot.classList.add('cell-dot--' + (STATES.indexOf(state) >= 0 ? state : 'no-data'));
}

function worstState(state) {
    const order = { 'no-data': 0, running: 1, slowed: 2, 'micro-stop': 3, stopped: 4 };
    let worst = state.primary ? state.primary.state : 'no-data';
    (state.sub_processes || []).forEach((p) => {
        if ((order[p.state] || 0) > (order[worst] || 0)) worst = p.state;
    });
    return STATES.indexOf(worst) >= 0 ? worst : 'no-data';
}

function cycleLabel(ms) {
    if (!ms || ms <= 0) return '—';
    if (ms < 60000) return (ms / 1000).toFixed(1) + 's';
    const s = Math.round(ms / 1000);
    return Math.floor(s / 60) + 'm ' + String(s % 60).padStart(2, '0') + 's';
}

function stateSummary(primary) {
    const label = { running: 'Running', slowed: 'Slowed', 'micro-stop': 'Micro-stop', stopped: 'Stopped', 'no-data': 'No data' };
    const name = label[primary.state] || 'No data';
    if (primary.since_last_ms && primary.since_last_ms > 0) {
        return name + ' · ' + agoLabel(primary.since_last_ms) + ' since last';
    }
    return name;
}

function agoLabel(ms) {
    const s = Math.round(ms / 1000);
    if (s < 60) return s + 's';
    const m = Math.round(s / 60);
    if (m < 60) return m + 'm';
    return Math.round(m / 60) + 'h';
}

// cssNum guards the attribute selector against odd values.
function cssNum(v) { return String(v).replace(/[^0-9-]/g, ''); }
