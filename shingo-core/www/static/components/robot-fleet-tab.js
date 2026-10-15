// The Robots page's Fleet tab.
//
// Top: the E-Maint snapshot of the robot status cache, moved here from the
// Diagnostics page with its Refresh and Download JSON. /api/telemetry/e-maint
// and its download are unchanged; this renders the same report the same way.
//
// Bottom: history from the hourly record, per robot, today and the 7 plant
// days before it (GET /api/robots/hours, summed on the server in the plant's
// zone). Numbers only and right-aligned; a span with no record is a dash,
// never 0, because zero is a measurement and absence is not.
import { escapeHtml, formatTime } from '/static/shared/utils.js';

const DASH = '—';

export function loadEMaintReport() {
  const body = document.getElementById('emaint-body');
  const summary = document.getElementById('emaint-summary');
  const download = document.getElementById('emaint-download');
  if (!body) return Promise.resolve();
  body.innerHTML = '<tr><td colspan="10" class="text-muted">Loading…</td></tr>';
  return fetch('/api/telemetry/e-maint')
    .then((r) => r.json())
    .then((data) => {
      summary.innerHTML = 'Report ' + escapeHtml(data.report_id) + ' · generated <time data-utc="' +
        escapeHtml(data.generated_at) + '">' + escapeHtml(formatTime(data.generated_at)) + '</time> · ' +
        data.robot_count + ' robot(s).';
      download.hidden = false;
      body.innerHTML = '';
      if (!data.robots || !data.robots.length) {
        body.innerHTML = '<tr><td colspan="10" class="text-muted">No robots in the cache.</td></tr>';
        return;
      }
      // Robot task state → the shared .badge-robot-* classes (no inline hexes).
      const stateClass = { ready: 'badge-robot-ready', busy: 'badge-robot-busy', offline: 'badge-robot-offline', error: 'badge-robot-error' };
      data.robots.forEach((r) => {
        const runtimeH = r.runtime && r.runtime.total_ms ? (r.runtime.total_ms / 3600000).toFixed(1) : '-';
        const batteryPct = r.battery ? r.battery.level_pct.toFixed(0) + '%' : '-';
        const voltage = r.battery && r.battery.voltage_v ? r.battery.voltage_v.toFixed(1) : '-';
        const odoTotal = r.odometer ? r.odometer.total_m.toFixed(0) : '-';
        const odoToday = r.odometer ? r.odometer.today_m.toFixed(0) : '-';
        const lifts = r.lifts ? r.lifts.total_count : '-';
        const ctrlTemp = r.controller && r.controller.temp_c ? r.controller.temp_c.toFixed(1) : '-';
        const station = r.position ? r.position.current_station || '-' : '-';
        const status = r.task ? r.task.status : '-';
        const statusCell = status === '-'
          ? '-'
          : '<span class="badge ' + (stateClass[status] || '') + '">' + escapeHtml(status) + '</span>';
        const tr = document.createElement('tr');
        tr.innerHTML =
          '<td><strong>' + escapeHtml(r.vehicle_id || '') + '</strong></td>' +
          '<td>' + statusCell + '</td>' +
          '<td class="col-num tnum">' + batteryPct + '</td>' +
          '<td class="col-num tnum">' + voltage + '</td>' +
          '<td class="col-num tnum">' + odoTotal + '</td>' +
          '<td class="col-num tnum">' + odoToday + '</td>' +
          '<td class="col-num tnum">' + runtimeH + '</td>' +
          '<td class="col-num tnum">' + lifts + '</td>' +
          '<td class="col-num tnum">' + ctrlTemp + '</td>' +
          '<td>' + escapeHtml(station) + '</td>';
        body.appendChild(tr);
      });
    })
    .catch((err) => {
      body.innerHTML = '<tr><td colspan="10" class="text-muted">Error: ' + escapeHtml(err.message) + '</td></tr>';
    });
}

// hoursCells renders one span's figures, or dashes for a span with no record.
export function hoursCells(f) {
  const num = (v, digits) => '<td class="col-num tnum">' +
    (v === null || v === undefined ? DASH : (digits === undefined ? String(v) : Number(v).toFixed(digits))) + '</td>';
  if (!f) {
    let out = '';
    for (let i = 0; i < 10; i++) out += num(null);
    return out;
  }
  return num(f.onsets_54901) + num(f.onsets_52101) + num(f.onsets_battery_fatal) +
    num(f.km, 1) + num(f.charging_min) + num(f.charge_starts) +
    num(f.battery_min_pct === null ? null : f.battery_min_pct, 0) + num(f.lifts) +
    num(f.blocked_min) + num(f.connected_min);
}

export function loadRobotHours() {
  const body = document.getElementById('hours-body');
  const windowEl = document.getElementById('hours-window');
  if (!body) return Promise.resolve();
  return fetch('/api/robots/hours?days=7')
    .then((r) => r.json())
    .then((v) => {
      if (windowEl) {
        windowEl.textContent = 'Plant days in ' + v.zone + ' · today ' + v.today +
          ' · prior ' + v.prior_days + ' days from ' + v.prior_from;
      }
      if (!v.robots || !v.robots.length) {
        body.innerHTML = '<tr><td colspan="12" class="text-muted">No hours recorded yet.</td></tr>';
        return;
      }
      body.innerHTML = v.robots.map((r) =>
        '<tr><td rowspan="2"><strong>' + escapeHtml(r.vehicle_id) + '</strong></td><td>Today</td>' + hoursCells(r.today) + '</tr>' +
        '<tr><td>Prior ' + v.prior_days + ' days</td>' + hoursCells(r.prior) + '</tr>').join('');
    })
    .catch((err) => {
      body.innerHTML = '<tr><td colspan="12" class="text-muted">Error: ' + escapeHtml(err.message) + '</td></tr>';
    });
}
