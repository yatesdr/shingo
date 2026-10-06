// plantclock.js — Core's plant-clock helpers that shared/utils.js does not
// carry: the plant calendar date, the clock with seconds, the range window and
// the chart bucket labels. ONE module for all of them; page modules and
// charts.js keep no time helper of their own (guarded by
// clock_globals_drift_test.go).
//
// It is not a second clock. The zone is window.PLANT_TZ with the same fallback
// as shared/utils.js (absent → the browser zone, degraded but never garbage),
// and "now" is serverNow() — never Date.now() or new Date(). Agreement with
// shared/utils.js formatClock / formatTime is pinned by plantclock.test.js.
//
// Stays in Core until Edge computes any of it (docs/shared-layer-promotion.md,
// clause 2).

import { serverNow } from '/static/shared/utils.js';

function plantZone() {
    return (typeof window !== 'undefined' && window.PLANT_TZ) || undefined;
}

const _fmtCache = new Map();
function partsIn(ts, options) {
    const tz = plantZone();
    const key = (tz || 'local') + '|' + JSON.stringify(options);
    let f = _fmtCache.get(key);
    if (!f) {
        f = new Intl.DateTimeFormat('en-US', Object.assign({ timeZone: tz }, options));
        _fmtCache.set(key, f);
    }
    const out = {};
    for (const p of f.formatToParts(new Date(ts))) out[p.type] = p.value;
    // Some engines spell midnight "24" under hourCycle h23 quirks; normalise.
    if (out.hour === '24') out.hour = '00';
    return out;
}

function valid(ts) {
    if (ts === null || ts === undefined || ts === '') return false;
    return !isNaN(new Date(ts).getTime());
}

// plantDate: the plant's calendar date of an instant, "YYYY-MM-DD" — the same
// bare date the server's since/until parameters resolve plant-local (Q-004).
export function plantDate(ts) {
    if (!valid(ts)) return '';
    const p = partsIn(ts, { year: 'numeric', month: '2-digit', day: '2-digit' });
    return p.year + '-' + p.month + '-' + p.day;
}

// formatClockSeconds: time of day with seconds, plant-local — the JS twin of
// planttime's ClockSeconds ("15:04:05"). For rows that share a minute.
export function formatClockSeconds(ts) {
    if (!valid(ts)) return '';
    const p = partsIn(ts, { hourCycle: 'h23', hour: '2-digit', minute: '2-digit', second: '2-digit' });
    return p.hour + ':' + p.minute + ':' + p.second;
}

// addDays is calendar arithmetic on a bare "YYYY-MM-DD": zone-free by
// construction, so a DST day is still one day.
export function addDays(ymd, n) {
    const [y, m, d] = ymd.split('-').map(Number);
    const t = new Date(Date.UTC(y, m - 1, d + n));
    return t.getUTCFullYear() + '-' + String(t.getUTCMonth() + 1).padStart(2, '0') + '-' +
        String(t.getUTCDate()).padStart(2, '0');
}

const RANGE_DAYS = { today: 1, '7d': 7, '30d': 30, '4w': 28, '12w': 84, '52w': 364 };

// windowFor maps a range control (today / 7d / 30d on Overview, 4w / 12w / 52w
// in the drill modal) to plant-local since/until dates ending on the plant's
// today, the previous equal-length window (for deltas), and the bucket grain:
// hourly for a single day, daily otherwise.
export function windowFor(range) {
    const days = RANGE_DAYS[range] || 1;
    const until = plantDate(serverNow());
    const since = addDays(until, -(days - 1));
    const prevUntil = addDays(since, -1);
    const prevSince = addDays(prevUntil, -(days - 1));
    return { since, until, prevSince, prevUntil, days, bucket: days === 1 ? 'hour' : 'day' };
}

// bucketLabel labels a bucket_start on a chart axis: "M/D" for a day bucket,
// "HH:00" for an hour bucket, both on the plant's clock.
export function bucketLabel(iso, bucket) {
    if (!valid(iso)) return '';
    if (bucket === 'day') {
        const p = partsIn(iso, { month: 'numeric', day: 'numeric' });
        return p.month + '/' + p.day;
    }
    return partsIn(iso, { hourCycle: 'h23', hour: '2-digit' }).hour + ':00';
}

// inProgress reports whether a bucket has not finished yet — the hour or plant
// day that "now" falls in. Such a bucket is drawn distinctly and kept out of
// peak and trend text, so a partly-elapsed hour does not read as a drop.
export function inProgress(iso, bucket) {
    if (!valid(iso)) return false;
    const now = serverNow();
    const start = new Date(iso).getTime();
    if (start > now) return false;
    if (bucket === 'day') return plantDate(iso) === plantDate(now);
    return now - start < 3600000;
}
