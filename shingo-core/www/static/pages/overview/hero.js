// Overview Section A — Hero KPIs + conditional alerts banner (plan §15.A).
//
// Five tiles: success rate, completed, avg duration, cancelled, active orders.
// Success/completed/avg/cancelled come from /api/missions/stats/v2 (the
// corrected success-rate math, §8 #5); the delta is the same endpoint over
// the previous equal-length window. Active orders is a live count
// (/api/missions/active) refreshed on SSE order-update — the same number the
// Dashboard calls "Active orders", so it carries the same name here. The
// alerts banner (/api/missions/alerts) renders only when there are active
// issues; its stuck count links to exactly those orders. The window figures
// refresh with the page (filter change, the refresh button), which says when
// it last did (overview.js, "as of").

import { apiGet, h } from '/static/app.js';
import { onSSE, debounce, formatDuration } from '/static/shared/utils.js';
import { windowFor } from '/static/components/plantclock.js';
import { KpiTile, updateKpiTile } from '/static/components/KpiTile.js';
import { RUN_TIME_TITLE } from '/static/components/DrillModal.js';
import { createLiveSchedule, createEventThrottle, countRobotAlerts } from '/static/pages/live-schedule.js';

// A stuck order is one with NO events, so with no order event only the robot
// tick can bring it to the banner. The alerts read rides robot-update at most
// this often (LC9): a newly stuck order can appear up to 30 s later than when
// every tick read it. The thresholds it is judged by are minutes.
const ALERTS_FROM_ROBOTS_MS = 30000;

export function createHeroSection(store) {
    const tiles = {}; // id -> tile node
    // The banner's two halves. robots comes from the last robot-update or
    // alerts read, whichever is newer; stuck only from an alerts read.
    let alerts = null;
    // Every alerts read counts against the 30 s robot throttle.
    const alertsThrottle = createEventThrottle(ALERTS_FROM_ROBOTS_MS);

    function mount() {
        const grid = document.getElementById('ops-kpi-grid');
        if (!grid) return;
        grid.innerHTML = '';
        // P8b hero hierarchy: Success rate is the single hero number; the rest
        // demote to a quiet supporting row. Same tiles/values as before — only
        // the size/layout changes (no data or update-logic change; refresh()
        // still updates each tile by its `tiles` key).
        const heroTile = KpiTile({ id: 'success', label: 'Success rate', drill: 'success_rate' });
        heroTile.classList.add('kpi-tile--hero');
        tiles.success = heroTile;
        grid.appendChild(heroTile);

        const support = document.createElement('div');
        support.className = 'ops-hero-support';
        const supportSpecs = [
            { id: 'completed', label: 'Completed', drill: 'completed' },
            { id: 'avg', label: 'Avg run time', title: RUN_TIME_TITLE, drill: 'avg_duration' },
            { id: 'cancelled', label: 'Cancelled', drill: 'cancelled' },
            // No drill: a live count has no history to open.
            { id: 'inflight', label: 'Active orders' },
        ];
        for (const s of supportSpecs) {
            const t = KpiTile(s);
            t.classList.add('kpi-tile--mini');
            tiles[s.id] = t;
            support.appendChild(t);
        }
        grid.appendChild(support);
        // Live (LC9). order-update: its own debounce, with a 10 s max wait
        // (a steady stream of events closer than 1.5 s would otherwise never
        // read) → the active count and the alerts read. robot-update: no request; the robot half of the
        // banner is counted from the frame (it is the whole fleet), and the
        // alerts read runs at most once per 30 s off it. A hidden tab reads
        // nothing; one catch-up runs when it is shown again.
        let fullPending = false;
        const live = createLiveSchedule(() => { refreshActive(); refreshAlerts(); }, {
            debounceMs: 1500,
            maxWaitMs: 10000,
            catchUp: () => {
                if (fullPending) { fullPending = false; refresh(store.get()); return; }
                refreshActive(); refreshAlerts();
            },
        });
        onSSE('order-update', live.kick);
        onSSE('robot-update', (robots) => {
            if (live.hidden()) { live.markStale(); return; }
            if (alerts) { alerts = Object.assign({}, alerts, countRobotAlerts(robots)); renderAlerts(); }
            if (alertsThrottle.ready()) refreshAlerts();
        });
        // Reconnect → re-fetch everything to close the staleness gap (§13).
        const reconnect = debounce(() => refresh(store.get()), 500);
        onSSE('connected', () => {
            if (live.hidden()) { fullPending = true; live.markStale(); return; }
            reconnect();
        });
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

        // A delta needs a previous window that had something in it: against an
        // empty one (a plant's first day) every count reads as a green "▲ 999".
        const prevHasData = !!(prev && prev.total > 0);

        updateKpiTile(tiles.completed, {
            label: 'Completed', drill: 'completed',
            value: cur.confirmed,
            delta: prevHasData ? signedDelta(cur.confirmed - prev.confirmed, (v) => '' + Math.abs(v), true) : null,
        });

        updateKpiTile(tiles.avg, {
            // Headline run time (execution time: assignment→load down, fault
            // time out); lead time (created→terminal) is the sub-stat (Q-031).
            // Lower is better → a drop is "good". Delta tracks the headline.
            label: 'Avg run time', title: RUN_TIME_TITLE, drill: 'avg_duration',
            value: (cur.total > 0 && cur.avg_execution_ms > 0) ? formatDuration(cur.avg_execution_ms) : '—',
            sub: cur.avg_duration_ms > 0 ? 'Lead ' + formatDuration(cur.avg_duration_ms) : '',
            delta: (prev && prev.avg_execution_ms > 0 && cur.avg_execution_ms > 0)
                ? durationDelta(cur.avg_execution_ms - prev.avg_execution_ms)
                : null,
        });

        updateKpiTile(tiles.cancelled, {
            label: 'Cancelled', drill: 'cancelled',
            value: cur.cancelled,
            sub: cancelOriginSub(cur), // Q-030 origin split (RDS / shingo / unclassified)
            // Neutral metric per §15.A — show movement but no good/bad color.
            delta: prevHasData ? { dir: deltaDir(cur.cancelled - prev.cancelled), text: '' + Math.abs(cur.cancelled - prev.cancelled) } : null,
        });
    }

    function refreshActive() {
        // Item 10: the active count respects the global station/robot filter. Read it
        // live from the store so the SSE-driven refresh (no args) and the
        // filter-driven refresh both scope correctly. Sub-label stays "live".
        const st = store.get();
        const q = qs({ station_id: st.station, robot_id: st.robot });
        apiGet('/api/missions/active' + (q ? '?' + q : ''))
            .then((d) => updateKpiTile(tiles.inflight, { label: 'Active orders', value: (d && typeof d.count === 'number') ? d.count : '—', sub: 'live' }))
            .catch(() => updateKpiTile(tiles.inflight, { label: 'Active orders', value: '—', sub: 'live' }));
    }

    function refreshAlerts() {
        const holder = document.getElementById('ops-alerts');
        if (!holder) return;
        alertsThrottle.touch();
        apiGet('/api/missions/alerts').then((a) => {
            alerts = a || null;
            renderAlerts();
        }).catch(() => { alerts = null; holder.innerHTML = ''; });
    }

    function renderAlerts() {
        const holder = document.getElementById('ops-alerts');
        if (!holder) return;
        const a = alerts;
        // The total is re-summed here: the robot half may be newer than the
        // read that carried the stuck half.
        const total = a ? (a.robots_blocked || 0) + (a.robots_emergency || 0) + (a.robots_error || 0) + (a.stuck_missions || 0) : 0;
        if (!total) { holder.innerHTML = ''; return; }
        const parts = [];
        if (a.robots_blocked) parts.push(a.robots_blocked + ' robot' + (a.robots_blocked > 1 ? 's' : '') + ' blocked');
        if (a.robots_emergency) parts.push(a.robots_emergency + ' emergency');
        if (a.robots_error) parts.push(a.robots_error + ' in error');
        const text = parts.join(' · ');
        const stuck = a.stuck_missions
            ? h`<a href="${stuckHref(a.stuck_items)}">${a.stuck_missions + ' active order' + (a.stuck_missions > 1 ? 's' : '') + ' stuck'}</a>`
            : '';
        holder.innerHTML = h`<div class="alerts-banner" role="status"><span class="alerts-banner__count">${'⚠ ' + total + ' alert' + (total > 1 ? 's' : '')}</span><span>${text}${text && stuck ? ' · ' : ''}${{ __html: true, value: stuck }}</span></div>`;
    }

    function showError() {
        for (const id in tiles) updateKpiTile(tiles[id], { label: tiles[id].querySelector('.kpi-label').textContent, value: '—' });
    }

    return { mount, refresh };
}

// ─── helpers ──────────────────────────────────────────────────────────────

// cancelOriginSub renders the Q-030 cancel-origin split as the Cancelled tile
// sub-stat. RDS-origin ("fleet order stopped") is the anonymous vendor wedge
// and leads; shingo-origin and any unclassified follow. Empty when no cancels.
function cancelOriginSub(s) {
    const parts = [];
    if (s.cancelled_rds) parts.push(s.cancelled_rds + ' RDS');
    if (s.cancelled_shingo) parts.push(s.cancelled_shingo + ' shingo');
    if (s.unclassified_stops) parts.push(s.unclassified_stops + ' unclassified');
    return parts.join(' · ');
}

// stuckHref links the alert to the Orders list filtered to exactly the stuck
// orders the payload names.
function stuckHref(items) {
    const ids = (items || []).map((it) => it.order_id).filter((id) => id != null);
    return '/orders?ids=' + ids.join(',');
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
    if (!diffMs) return { dir: 'flat', text: formatDuration(0) };
    const down = diffMs < 0;
    return { dir: down ? 'down' : 'up', text: formatDuration(Math.abs(diffMs)), good: down };
}
