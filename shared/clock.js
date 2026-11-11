// clock.js — the server's clock and what reads it, shared by Core and Edge:
// serverNow and the sim speed, the plant-local formatters (formatTime,
// formatClock, timeAgo), the one duration ladder (formatDuration), the live
// durations that tick on a page, and the <time data-utc> rollover shim, with
// the htmx hooks that re-run both after a swap.
//
// ES module, served at /static/shared/clock.js; a page imports each helper
// from here, so a page has one clock. No imports.
//
// ONE THING RUNS AT IMPORT, and it touches nothing outside this file: the
// clock copies the server's now from window.SHINGO_CLOCK, the inline the page
// painted, into its own state (below). Reading it on first use instead would
// move the extrapolation's origin from the load to that call, and on the sim
// rig, where now runs at a multiplier, every later reading would lag by the
// wait times the speed. Listeners, timers and the DOM are touched only by the
// install* helpers, when a page calls them.

// ─── Time formatting ─────────────────────────────────────────────────────

// ─── Server-owned now ────────────────────────────────────────────────────
//
// THE SERVER OWNS NOW, the same way it owns the timezone. Every elapsed
// reading below used to be `Date.now() - serverStamp` — a BROWSER wall clock
// differenced against a SERVER timestamp. Those are not the same clock, and on
// the sim rig they were not even the same year: simulated time runs at a speed
// multiplier, so it stood weeks ahead of the wall, every difference came out
// negative, and formatDuration clamps negatives to zero. Every live duration
// on the rig read "0 s" — the instrument that distinguishes backpressure from
// a wedge, dead, for as long as the rig has existed.
//
// The server inlines its own now and the rate that now advances (layout.html /
// header.html, from planttime.ServerClockJS). We extrapolate from the paint
// rather than re-fetching: correct for a tab left open a week, because the
// multiplier is applied to the elapsed browser time rather than assumed to be
// one.
//
// Absent the inline — a page served without the layout, a test harness, a
// partial rendered standalone — this degrades to the browser clock at 1x,
// which is exactly the old behaviour. Never to garbage.
const _clock = {
    serverMs: null,   // server's now at paint, ms
    wallMs: null,     // browser's now at the same instant, ms
    speed: 1,
    sim: false,
};

// syncServerClock re-pins the origin. Called once at load from the inline, and
// exported because the sim speed can change under a live page (POST
// /api/sim/speed) — a page that knows it changed the speed must re-pin or it
// keeps extrapolating at the old rate.
export function syncServerClock(sc) {
    if (!sc || !sc.now) return false;
    const ms = Date.parse(sc.now);
    if (isNaN(ms)) return false;
    _clock.serverMs = ms;
    _clock.wallMs = Date.now();
    _clock.speed = (typeof sc.speed === 'number' && sc.speed > 0) ? sc.speed : 1;
    _clock.sim = !!sc.sim;
    return true;
}

// serverNow is the drop-in for Date.now() ANYWHERE the result is compared
// against a server-supplied timestamp. Milliseconds since the epoch, in the
// server's frame.
export function serverNow() {
    if (_clock.serverMs === null) return Date.now();
    return _clock.serverMs + (Date.now() - _clock.wallMs) * _clock.speed;
}

// isSimClock reports whether the server's now is simulated — for the marker
// that makes a sim page say so.
export function isSimClock() { return _clock.sim; }

// simSpeed is the multiplier, for the marker's readout. 1 when unknown.
export function simSpeed() { return _clock.speed; }

if (typeof window !== 'undefined' && window.SHINGO_CLOCK) {
    syncServerClock(window.SHINGO_CLOCK);
}

export function timeAgo(ts) {
    if (!ts) return '-';
    // serverNow, not Date.now: the stamp came from the server, so the
    // difference has to be taken in the server's frame. See serverNow.
    const d = serverNow() - new Date(ts).getTime();
    if (d < 60000) return 'just now';
    if (d < 3600000) return Math.floor(d / 60000) + 'm ago';
    if (d < 86400000) return Math.floor(d / 3600000) + 'h ago';
    return Math.floor(d / 86400000) + 'd ago';
}

// PLANT_TZ pins every date rendering below to the plant's wall clock, not
// the viewer's. The server inlines `window.PLANT_TZ = "America/Chicago"`
// (or the edge's zone) synchronously in the page head — a fetch would paint
// wrong text first, which is the flicker the plant-local convention
// removed. Everything degrades to the browser zone if the inline is absent
// (a page served without the layout, a test harness), never to garbage.
function plantTZ() {
    return (typeof window !== 'undefined' && window.PLANT_TZ) || undefined;
}

// One Intl.DateTimeFormat per (zone, shape) — constructing these is
// expensive enough to matter on boards that render hundreds of rows per
// reconcile. Keyed cache so PLANT_TZ resolution costs once per page.
const _fmtCache = new Map();
function tzFormatter(options) {
    const tz = plantTZ();
    const key = (tz || 'local') + '|' + JSON.stringify(options);
    let f = _fmtCache.get(key);
    if (!f) {
        f = new Intl.DateTimeFormat('en-US', Object.assign({ timeZone: tz }, options));
        _fmtCache.set(key, f);
    }
    return f;
}

export function formatTime(ts, opts) {
    if (!ts || ts === '0001-01-01T00:00:00Z') return '-';
    const d = new Date(ts);
    if (isNaN(d.getTime())) return ts;
    if (opts && opts.precision === 'ms') {
        return tzFormatter({ hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' })
            .format(d) + '.' + String(d.getMilliseconds()).padStart(3, '0');
    }
    // The plant-local twin of shared/planttime displayLayout ("Jan 2, 2006
    // 15:04 MST" in Go). Change them in both or in neither — byte for byte.
    // Intl emits "Sep 5, 2026, 09:23 CDT" (comma after the year in en-US);
    // Go's layout has none, so the comma is stripped here to keep the JS
    // twin identical to the server paint.
    return tzFormatter({
        month: 'short', day: 'numeric', year: 'numeric',
        hour12: false, hour: '2-digit', minute: '2-digit',
        timeZoneName: 'short',
    }).format(d).replace(/(\d{4}),\s(\d{2}:)/, '$1 $2');
}

// formatClock renders time-of-day only, plant-local — the JS twin of
// planttime.Clock ("15:04"). Wall-clock displays and log columns whose
// shape is deliberately narrow; do NOT swap these to formatTime, a full
// datetime in a clock slot breaks the layout.
export function formatClock(ts) {
    if (!ts) return '';
    const d = new Date(ts);
    if (isNaN(d.getTime())) return String(ts);
    return tzFormatter({ hour12: false, hour: '2-digit', minute: '2-digit' }).format(d);
}

// formatDuration is THE SAME LADDER as protocol.FormatDuration (Go), output
// byte for byte. That is a hard requirement, not a nicety.
//
// The fault line is rendered ONCE by the server and then re-rendered every
// second by installLiveDurations below. When the two ladders disagreed, every
// faulted row on every board visibly rewrote itself one second after it
// painted — "Replanning · 4m 07s" became "Replanning · 4m 7s" — and again after
// each reconcile, because the reconcile restores the server's text. The
// mismatch was not one space: the old ladder kept seconds all the way to an
// hour where Go drops them at ten minutes, zero-padded nothing, had a
// millisecond tier Go has no equivalent for, had no days tier at all, and
// rendered zero as "-" where Go renders "0 s".
//
// The tiers, from protocol/duration.go — change them in both or in neither:
//
//	< 1m    "18 s"     seconds, with the space
//	< 10m   "4m 07s"   zero-padded seconds so a tnum column stays aligned
//	< 1h    "23m"      whole minutes; seconds stop carrying anything
//	< 24h   "2h 05m"
//	else    "3d 04h"
//
// Zero is "0 s", NOT the dash — Go's ladder floors negatives at zero and prints
// that, and a fault line rendered at the instant of the fault must agree. The
// dash is reserved for ABSENT (null / undefined / NaN), which Go never renders:
// BuildFaultLine omits the element entirely when it has no instant, rather than
// printing a duration measured from the zero time.
export function formatDuration(ms) {
    if (ms === null || ms === undefined || isNaN(ms)) return '—';
    const total = Math.max(0, Math.floor(ms));
    const s = Math.floor(total / 1000);
    if (s < 60) return s + ' s';
    const m = Math.floor(s / 60);
    if (s < 600) return m + 'm ' + String(s % 60).padStart(2, '0') + 's';
    if (s < 3600) return m + 'm';
    const h = Math.floor(m / 60);
    if (s < 86400) return h + 'h ' + String(m % 60).padStart(2, '0') + 'm';
    return Math.floor(h / 24) + 'd ' + String(h % 24).padStart(2, '0') + 'h';
}

// ─── Live durations ──────────────────────────────────────────────────────

// Tick the elapsed / remaining durations on a page.
//
//   <span class="tnum" data-since="2026-08-22T14:00:00Z"></span>   -> "3m 12s"
//   <span class="tnum" data-until="2026-08-22T14:45:00Z"></span>   -> "41m"
//
// Both render through formatDuration. A past data-until shows its data-past
// text, or an em-dash. A data-since may also carry data-notice-after (seconds)
// to swap a sibling's wording at a server-supplied threshold — see
// applyNoticeWording; the client only compares, it never picks the number.
//
// The interval runs only while such nodes exist, so a page without them pays
// nothing. Idempotent; re-run after any DOM change that may insert nodes.
let _liveDurationTimer = null;

export function installLiveDurations(root) {
    const live = renderLiveDurations(root);
    if (_liveDurationTimer || !live) return;
    _liveDurationTimer = setInterval(() => {
        // Sweep the document, not `root` — a second caller's nodes must keep
        // ticking after the first caller's are gone.
        if (!renderLiveDurations(document)) {
            clearInterval(_liveDurationTimer);
            _liveDurationTimer = null;
        }
    }, 1000);
}

// Paint one frame; report whether anything remains to tick. Exported for tests
// and for a single repaint without arming the interval.
export function renderLiveDurations(root) {
    const scope = root || document;
    let live = 0;
    scope.querySelectorAll('[data-since]').forEach(elem => {
        const since = Date.parse(elem.getAttribute('data-since'));
        if (isNaN(since)) return;
        live++;
        const elapsed = serverNow() - since;
        elem.textContent = formatDuration(elapsed);
        applyNoticeWording(elem, elapsed);
    });
    scope.querySelectorAll('[data-until]').forEach(elem => {
        const until = Date.parse(elem.getAttribute('data-until'));
        if (isNaN(until)) return;
        const left = until - serverNow();
        if (left <= 0) {
            elem.textContent = elem.getAttribute('data-past') || '—';
            return;
        }
        live++;
        elem.textContent = formatDuration(left);
    });
    return live > 0;
}

// Swap a sibling's wording once elapsed crosses the server's threshold. The
// node carries the threshold and both words, so no policy lives here.
function applyNoticeWording(elem, elapsedMs) {
    const afterS = parseInt(elem.getAttribute('data-notice-after'), 10);
    if (!afterS) return;
    const target = elem.parentNode && elem.parentNode.querySelector
        ? elem.parentNode.querySelector('[data-notice-word]') : null;
    if (!target) return;
    const over = elapsedMs >= afterS * 1000;
    const word = over
        ? target.getAttribute('data-notice-over')
        : target.getAttribute('data-notice-under');
    if (word !== null && word !== undefined && target.textContent !== word) {
        target.textContent = word;
    }
    // Clauses that belong only to the over-threshold sentence (the grace
    // countdown: noise beside a 14s replan, the point beside a 3m fault).
    const parent = elem.parentNode;
    if (parent && parent.querySelectorAll) {
        parent.querySelectorAll('[data-notice-only-over]').forEach(node => {
            node.hidden = !over;
        });
    }
}

// Re-render live durations after an htmx swap, as
// installHtmxTimestampConversion does for <time data-utc>. Without it a
// swapped-in row's duration stays empty until the next tick, or forever if the
// interval had already stopped.
let _htmxLiveDurInstalled = false;
export function installHtmxLiveDurations() {
    if (_htmxLiveDurInstalled) return;
    _htmxLiveDurInstalled = true;
    const attach = () => {
        document.body.addEventListener('htmx:afterSwap', (evt) => {
            const target = (evt && evt.detail && evt.detail.target) || document;
            installLiveDurations(target);
        });
    };
    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', attach);
    } else {
        attach();
    }
}

// Rewrite <time data-utc="..."> elements to plant-local.
//
// UNDER THE PLANT-LOCAL CONVENTION THIS IS A ROLLOVER SHIM, not the render
// path: the server already paints plant-local labeled text, correct on
// first paint. The only nodes still carrying the OLD UTC-first markup are
// partials swapped in by an edge still running pre-plant-local during a
// rolling deploy. Rewriting them keeps one deploy from showing two clock
// conventions. When no plant is left on the old build, this whole
// function and its htmx hook (installHtmxTimestampConversion) can go.
//
// Idempotent; safe to re-run after htmx swaps insert new <time> nodes.
// Marks rewritten nodes so a re-run targets only the unconverted — the
// server's own plant-local text must never be touched, so no element
// ever rewrites twice.
export function convertTimestamps(root) {
    const scope = root || document;
    scope.querySelectorAll('time[data-utc]').forEach(elem => {
        if (elem.getAttribute('data-converted')) return;
        const d = new Date(elem.getAttribute('data-utc'));
        if (isNaN(d.getTime())) return;
        // Only convert nodes whose server text is the old UTC convention —
        // recognizable by the trailing " UTC" the old helper emitted.
        const text = elem.textContent || '';
        if (!/ UTC$/.test(text)) { elem.setAttribute('data-converted', '1'); return; }
        elem.textContent = formatTime(elem.getAttribute('data-utc'));
        elem.setAttribute('data-converted', '1');
    });
}

// installHtmxTimestampConversion wires a document.body listener for
// `htmx:afterSwap` that re-runs convertTimestamps() against the
// swapped-in subtree (event.detail.target). Without this, <time
// data-utc=…> nodes that arrive via partial swap stay as raw UTC
// strings until the next full page load.
//
// Surfaces that use HTMX (Edge admin) call this once at module
// load. The initial DOMContentLoaded conversion is still wired
// per-surface — this only handles the swap case. Idempotent.
let _htmxTsInstalled = false;
export function installHtmxTimestampConversion() {
    if (_htmxTsInstalled) return;
    _htmxTsInstalled = true;
    const attach = () => {
        document.body.addEventListener('htmx:afterSwap', (evt) => {
            const target = (evt && evt.detail && evt.detail.target) || document;
            convertTimestamps(target);
        });
    };
    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', attach);
    } else {
        attach();
    }
}
