// Overview Section A — Hero KPIs + conditional alerts banner (plan §15.A).
//
// Five tiles: success rate, completed, avg duration, cancelled, in flight.
// Success/completed/avg/cancelled come from /api/missions/stats/v2 (the
// corrected success-rate math, §8 #5); the delta is the same endpoint over
// the previous equal-length window. In flight is a live count
// (/api/missions/active) refreshed on SSE order-update. The alerts banner
// (/api/missions/alerts) renders only when there are active issues.

import { apiGet, formatDuration } from '/static/app.js';
import { onSSE, debounce } from '/static/shared/utils.js';
import { KpiTile, updateKpiTile } from '/static/components/KpiTile.js';

export function createHeroSection(store) {
    const tiles = {}; // id -> tile node

    function mount() {
        const grid = document.getElementById('ops-kpi-grid');
        if (!grid) return;
        grid.innerHTML = '';
        const specs = [
            { id: 'success', label: 'Success rate', drill: 'success_rate' },
            { id: 'completed', label: 'Completed', drill: 'completed' },
            { id: 'avg', label: 'Avg duration', drill: 'avg_duration' },
            { id: 'cancelled', label: 'Cancelled', drill: 'cancelled' },
            { id: 'inflight', label: 'In flight', drill: 'in_flight' },
        ];
        for (const s of specs) {
            const t = KpiTile(s);
            tiles[s.id] = t;
            grid.appendChild(t);
        }
        // Live: in-flight count + alerts react to order/robot churn.
        const live = debounce(() => { refreshActive(); refreshAlerts(); }, 1500);
        onSSE('order-update', live);
        onSSE('robot-update', live);
        // Reconnect → re-fetch everything to close the staleness gap (§13).
        onSSE('connected', debounce(() => refresh(store.get()), 500));
    }

    function refresh(state) {
        const win = windowFor(state.range);
        const scope = { station_id: state.station, robot_id: state.robot };
        const curQS = qs(Object.assign({ since: win.since, until: win.until }, scope));
        const prevQS = qs(Object.assign({ since: win.prevSince, until: win.prevUntil }, scope));

        Promise.all([
            apiGet('/api/missions/stats/v2?' + curQS).catch(() => null),
            apiGet('/api/missions/stats/v2?' + prevQS).catch(() => null),
        ]).then(([cur, prev]) => {
            if (!cur) { showError(); return; }
            renderStats(cur, prev);
        });
        refreshActive();
        refreshAlerts();
    }

    function renderStats(cur, prev) {
        const denom = (cur.confirmed || 0) + (cur.failed || 0);
        const prevDenom = prev ? (prev.confirmed || 0) + (prev.failed || 0) : 0;

        // Success rate — '—' on a cold/empty window (§8 #19).
        updateKpiTile(tiles.success, {
            label: 'Success rate', drill: 'success_rate',
            value: denom > 0 ? cur.success_rate.toFixed(1) + '%' : '—',
            sub: denom > 0 ? cur.confirmed + ' of ' + denom : 'no completed missions',
            delta: (prev && prevDenom > 0)
                ? signedDelta(cur.success_rate - prev.success_rate, (v) => Math.abs(v).toFixed(1) + 'pt', true)
                : null,
        });

        updateKpiTile(tiles.completed, {
            label: 'Completed', drill: 'completed',
            value: cur.confirmed,
            delta: prev ? signedDelta(cur.confirmed - prev.confirmed, (v) => '' + Math.abs(v), true) : null,
        });

        updateKpiTile(tiles.avg, {
            label: 'Avg duration', drill: 'avg_duration',
            value: (cur.total > 0 && cur.avg_duration_ms > 0) ? formatDuration(cur.avg_duration_ms) : '—',
            sub: cur.p50_duration_ms > 0 ? 'P50 ' + formatDuration(cur.p50_duration_ms) : '',
            // Lower is better → a drop is "good".
            delta: (prev && prev.avg_duration_ms > 0 && cur.avg_duration_ms > 0)
                ? durationDelta(cur.avg_duration_ms - prev.avg_duration_ms)
                : null,
        });

        updateKpiTile(tiles.cancelled, {
            label: 'Cancelled', drill: 'cancelled',
            value: cur.cancelled,
            // Neutral metric per §15.A — show movement but no good/bad color.
            delta: prev ? { dir: deltaDir(cur.cancelled - prev.cancelled), text: '' + Math.abs(cur.cancelled - prev.cancelled) } : null,
        });
    }

    function refreshActive() {
        apiGet('/api/missions/active')
            .then((d) => updateKpiTile(tiles.inflight, { label: 'In flight', drill: 'in_flight', value: (d && typeof d.count === 'number') ? d.count : '—', sub: 'live' }))
            .catch(() => updateKpiTile(tiles.inflight, { label: 'In flight', drill: 'in_flight', value: '—', sub: 'live' }));
    }

    function refreshAlerts() {
        const holder = document.getElementById('ops-alerts');
        if (!holder) return;
        apiGet('/api/missions/alerts').then((a) => {
            if (!a || !a.total) { holder.innerHTML = ''; return; }
            const parts = [];
            if (a.robots_blocked) parts.push(a.robots_blocked + ' robot' + (a.robots_blocked > 1 ? 's' : '') + ' blocked');
            if (a.robots_emergency) parts.push(a.robots_emergency + ' emergency');
            if (a.robots_error) parts.push(a.robots_error + ' in error');
            if (a.stuck_missions) parts.push(a.stuck_missions + ' mission' + (a.stuck_missions > 1 ? 's' : '') + ' stuck');
            holder.innerHTML =
                '<div class="alerts-banner" role="status">' +
                '<span class="alerts-banner__count">⚠ ' + a.total + ' alert' + (a.total > 1 ? 's' : '') + '</span>' +
                '<span>' + parts.join(' · ') + '</span></div>';
        }).catch(() => { holder.innerHTML = ''; });
    }

    function showError() {
        for (const id in tiles) updateKpiTile(tiles[id], { label: tiles[id].querySelector('.kpi-label').textContent, value: '—' });
    }

    return { mount, refresh };
}

// ─── helpers ──────────────────────────────────────────────────────────────
function ymd(d) {
    return d.getFullYear() + '-' +
        String(d.getMonth() + 1).padStart(2, '0') + '-' +
        String(d.getDate()).padStart(2, '0');
}

// windowFor maps the range selector to browser-local since/until date strings
// plus the previous equal-length window for deltas. NOTE: the backend parses
// these as server-local bare dates (§8 #17 timezone ambiguity, Q-004).
function windowFor(range) {
    const today = new Date(); today.setHours(0, 0, 0, 0);
    let days = 1;
    if (range === '7d') days = 7;
    else if (range === '30d') days = 30;
    const since = new Date(today); since.setDate(since.getDate() - (days - 1));
    const prevUntil = new Date(since); prevUntil.setDate(prevUntil.getDate() - 1);
    const prevSince = new Date(prevUntil); prevSince.setDate(prevSince.getDate() - (days - 1));
    return { since: ymd(since), until: ymd(today), prevSince: ymd(prevSince), prevUntil: ymd(prevUntil), days };
}

function qs(params) {
    const p = new URLSearchParams();
    for (const k in params) {
        if (params[k] !== '' && params[k] !== null && params[k] !== undefined) p.set(k, params[k]);
    }
    return p.toString();
}

function deltaDir(diff) { return diff > 0 ? 'up' : diff < 0 ? 'down' : 'flat'; }

// signedDelta: arrow follows the sign; `goodWhenUp` decides the color.
function signedDelta(diff, fmt, goodWhenUp) {
    if (!diff) return { dir: 'flat', text: fmt(0) };
    const up = diff > 0;
    return { dir: up ? 'up' : 'down', text: fmt(diff), good: goodWhenUp ? up : !up };
}

// durationDelta: a *drop* in duration is good, so colour accordingly.
function durationDelta(diffMs) {
    if (!diffMs) return { dir: 'flat', text: '0s' };
    const down = diffMs < 0;
    return { dir: down ? 'down' : 'up', text: formatDuration(Math.abs(diffMs)), good: down };
}
