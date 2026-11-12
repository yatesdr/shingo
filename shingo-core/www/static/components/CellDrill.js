// CellDrill — the per-Process pulse-timeline detail for one cell (Phase E
// §3.D drill). Opens on a CellTile click; fetches /api/cells/{id}/heartbeat
// (the windowed history split per Process) and renders, per Process, a pulse
// strip (one dot per fire, positioned by time) plus cycle/loss stats. For a
// composite cell the sub-Processes render above the primary so the
// sub→sub→primary flow reads top-to-bottom.
//
// Deliberately chart-free (plain DOM strips) so there's no Chart.js instance to
// leak — the §13 drill concern doesn't apply here. ✕ / Escape / backdrop close
// plus a 60s idle auto-dismiss, matching DrillModal.
//
// The drill is an entry of the one overlay stack (shared/layer.js, style guide
// §10), as DrillModal is: Escape and the backdrop close it innermost first (a
// confirm over it goes before it does), the page behind is locked while it is
// open (counted with any other entry), focus goes into it and back to the tile
// that opened it, and a press that began in the card never closes it. It
// holds no typed work, so it never asks. Every close (✕, Escape, the backdrop,
// the idle timer) runs drillClosed as the entry's onClose.

import { el, h } from '/static/shared/dom.js';
import { formatClock, formatDuration, formatTime } from '/static/shared/clock.js';
import { hideModal, showModal } from '/static/shared/layer.js';

let _active = null;
const MAX_DOTS = 400; // cap per strip; a wider window samples down to this

export function openCellDrill(cellID) {
    closeCellDrill();

    const overlay = el('div', { className: 'modal-overlay drill-modal' });
    const box = el('div', { className: 'modal', style: { maxWidth: '720px' } });
    box.innerHTML = h`
        <div class="modal-header flex flex-between">
          <h2 class="cell-drill__title">${cellID}</h2>
          <button class="modal-close" title="Close">&times;</button>
        </div>
        <div class="cell-drill__body"><div class="dash-empty">Loading…</div></div>`;
    overlay.appendChild(box);
    document.body.appendChild(overlay);

    const state = { overlay, box, cellID, idleTimer: null, controller: null };
    _active = state;

    box.querySelector('.modal-close').addEventListener('click', closeCellDrill);
    showModal(overlay, { closeOnBackdrop: true, ask: false, onClose: () => drillClosed(state) });
    overlay.addEventListener('mousemove', resetIdle);
    resetIdle();

    state.controller = new AbortController();
    fetch('/api/cells/' + encodeURIComponent(cellID) + '/heartbeat', { signal: state.controller.signal })
        .then((r) => { if (!r.ok) throw new Error('http ' + r.status); return r.json(); })
        .then((data) => { if (_active === state) render(state, data); })
        .catch((e) => {
            if (e && e.name === 'AbortError') return;
            if (_active === state) box.querySelector('.cell-drill__body').innerHTML =
                '<div class="dash-empty">Failed to load: ' + e + '</div>';
        });
}

// closeCellDrill is the drill's own close (✕, the idle timer, a re-open):
// the layer's, so drillClosed runs as for Escape or the backdrop.
export function closeCellDrill() {
    if (_active) hideModal(_active.overlay);
}

// drillClosed is the drill's close however it came: its idle timer and its
// read in flight go, and so does its overlay.
function drillClosed(a) {
    if (_active === a) _active = null;
    clearTimeout(a.idleTimer);
    if (a.controller) { try { a.controller.abort(); } catch (_) {} }
    if (a.overlay.parentNode) a.overlay.parentNode.removeChild(a.overlay);
}

function resetIdle() {
    if (!_active) return;
    clearTimeout(_active.idleTimer);
    _active.idleTimer = setTimeout(closeCellDrill, 60000);
}

function render(state, data) {
    const { box } = state;
    box.querySelector('.cell-drill__title').textContent = data.display_name || data.cell_id || state.cellID;
    const body = box.querySelector('.cell-drill__body');
    body.innerHTML = '';

    const since = new Date(data.since).getTime();
    const until = new Date(data.until).getTime();
    const span = until - since || 1;
    body.appendChild(el('div', { className: 'text-muted-sm cell-drill__window' },
        'Window: ' + fmtRange(data.since, data.until)));

    const procs = data.processes || [];
    if (!procs.length) {
        body.appendChild(el('div', { className: 'dash-empty' }, 'No Processes configured for this cell.'));
        return;
    }
    // Subs above the primary so the sub→primary flow reads top-to-bottom.
    const ordered = procs.filter((p) => !p.primary).concat(procs.filter((p) => p.primary));
    ordered.forEach((p) => body.appendChild(procRow(p, since, span)));
}

function procRow(p, since, span) {
    const m = p.metrics || {};
    const label = (p.primary ? 'Primary' : 'Sub') + ' · Process ' + p.process_id;
    const strip = el('div', { className: 'cell-drill__strip' });
    const events = sampleEvents(p.events || []);
    events.forEach((e) => {
        const t = new Date(e.recorded_at).getTime();
        const pct = Math.max(0, Math.min(100, ((t - since) / span) * 100));
        strip.appendChild(el('span', { className: 'cell-drill__pulse', style: { left: pct + '%' } }));
    });
    if (!events.length) strip.appendChild(el('span', { className: 'text-muted-sm cell-drill__nodata' }, 'no fires in window'));

    const stats = el('div', { className: 'cell-drill__stats text-muted-sm' }, [
        stat('Parts', m.parts || 0),
        stat('Stops', m.stop_count || 0),
        stat('Downtime', formatDuration(m.total_downtime_ms)),
        stat('MTBF', m.mtbf_minutes ? m.mtbf_minutes.toFixed(0) + 'm' : '—'),
        stat('Eff/hr', m.effective_parts_per_hour ? m.effective_parts_per_hour.toFixed(0) : '—'),
        stat('Lost', m.parts_lost || 0),
    ]);
    // Gaps that ended in a counter jump or reset: the parts were made and are
    // counted above, only their timing is unknown — so neither a stop nor
    // downtime.
    if (m.counter_offline_count) {
        stats.appendChild(stat('Counter offline', formatDuration(m.counter_offline_ms) + ' (' + m.counter_offline_count + ')'));
    }

    return el('div', { className: 'cell-drill__proc' + (p.primary ? ' cell-drill__proc--primary' : '') }, [
        el('div', { className: 'cell-drill__proc-head flex flex-between' }, [
            el('span', { className: 'cell-drill__proc-label' }, label),
            el('span', { className: 'text-muted-sm' }, (events.length) + ' fires'),
        ]),
        strip,
        stats,
    ]);
}

function stat(label, value) {
    return el('span', { className: 'cell-drill__stat' }, [
        el('span', { className: 'cell-drill__stat-label' }, label + ' '),
        el('strong', {}, String(value)),
    ]);
}

// sampleEvents caps the strip at MAX_DOTS by even decimation so a wide window
// can't spray thousands of nodes (the drill is on-demand + idle-dismissed, but
// keep it bounded anyway).
function sampleEvents(events) {
    if (events.length <= MAX_DOTS) return events;
    const step = events.length / MAX_DOTS;
    const out = [];
    for (let i = 0; i < events.length; i += step) out.push(events[Math.floor(i)]);
    return out;
}

function fmtRange(a, b) {
    try { return formatTime(a) + ' → ' + formatClock(b); }
    catch (_) { return ''; }
}
