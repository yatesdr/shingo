// RobotTile — shared robot renderers (plan §3.C / §6). Two shapes:
//
//   createRobotTile/updateRobotTile  — the /robots grid tile, matching the
//       server-rendered .robot-tile markup + dataset so robots.js can drive
//       it via reconcileList (adopting server DOM by data-name). Accepts the
//       SSE robot-update shape (snake_case).
//   createFleetRow/updateFleetRow    — the Operations Overview per-robot row
//       (state pill, mission-derived utilization bar, battery). Accepts the
//       /api/robots/fleet row shape.
//
// Extracted so both the existing grid and the new fleet rows reconcile
// through one component instead of duplicating innerHTML strings.

import { el, h } from '/static/app.js';

// ─── /robots grid tile ─────────────────────────────────────────────────────
export function createRobotTile(r) {
    const tile = el('div', { className: 'robot-tile robot-' + r.state, dataset: { action: 'openRobotModal' } });
    const name = el('div', { className: 'robot-name' }, r.vehicle_id); // textContent — XSS-safe
    const bat = el('div', { className: 'robot-battery' });
    bat.appendChild(el('div', { className: 'robot-battery-fill' }));
    tile.appendChild(name);
    tile.appendChild(bat);
    updateRobotTile(tile, r);
    return tile;
}

export function updateRobotTile(tile, r) {
    tile.className = 'robot-tile robot-' + r.state;
    const fill = tile.querySelector('.robot-battery-fill');
    if (fill) fill.style.width = r.battery + '%';
    const bat = tile.querySelector('.robot-battery');
    if (bat) bat.title = 'Battery: ' + r.battery + '%';
    const name = tile.querySelector('.robot-name');
    if (name) {
        let chg = name.querySelector('.robot-charging');
        if (r.charging && !chg) {
            chg = el('span', { className: 'robot-charging', title: 'Charging' });
            chg.innerHTML = '&#9889;';
            name.appendChild(chg);
        } else if (!r.charging && chg) {
            chg.remove();
        }
    }
    setRobotDataset(tile, r);
}

// setRobotDataset mirrors robots.js's dataset contract exactly so the
// existing openRobotModal handler keeps working on reconcile-created tiles.
function setRobotDataset(tile, r) {
    const d = tile.dataset;
    d.name = r.vehicle_id;
    d.state = r.state;
    d.ip = r.ip || '';
    d.model = r.model || '';
    d.map = r.map || '';
    d.battery = r.battery;
    d.charging = r.charging;
    d.station = r.station || '';
    d.lastStation = r.last_station || '';
    d.available = r.available;
    d.connected = r.connected;
    d.blocked = r.blocked;
    d.emergency = r.emergency;
    d.processing = r.processing;
    d.error = r.error;
    if (typeof r.x === 'number') d.x = r.x.toFixed(1);
    if (typeof r.y === 'number') d.y = r.y.toFixed(1);
    if (typeof r.angle === 'number') d.angle = r.angle.toFixed(1);
}

// ─── Operations Overview fleet row ─────────────────────────────────────────
export function createFleetRow(r) {
    const row = el('div', { className: 'fleet-row' });
    row.innerHTML = h`
        <span class="fleet-row__name">${r.vehicle_id}</span>
        <span class="badge robot-pill"></span>
        <span class="util-cell"><span class="util-bar"><span class="util-bar__busy"></span></span><span class="util-val"></span></span>
        <span class="fleet-row__missions"></span>
        <span class="fleet-row__battery"></span>`;
    updateFleetRow(row, r);
    return row;
}

export function updateFleetRow(row, r) {
    row.classList.toggle('fleet-row--offline', !r.connected);
    row.classList.toggle('fleet-row--blocked', !!r.blocked);
    const pill = row.querySelector('.robot-pill');
    if (pill) { pill.textContent = r.state; pill.className = 'badge robot-pill robot-' + r.state; }
    const util = Math.max(0, Math.min(100, r.util_pct || 0));
    const busy = row.querySelector('.util-bar__busy');
    if (busy) busy.style.width = util.toFixed(0) + '%';
    const uv = row.querySelector('.util-val');
    if (uv) uv.textContent = util.toFixed(0) + '%';
    const m = row.querySelector('.fleet-row__missions');
    if (m) m.textContent = (r.missions || 0) + ' msn';
    const b = row.querySelector('.fleet-row__battery');
    if (b) b.textContent = (r.battery !== null && r.battery !== undefined ? Math.round(r.battery) : '—') + '%';
}
