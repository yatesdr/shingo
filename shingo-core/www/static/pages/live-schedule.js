// When a live page re-reads, driven by events (LC9, LC11).
//
// Three rules, the same on every page that uses this:
//   - a burst of events is one read, after the burst settles (the debounce);
//   - a hidden tab reads nothing: an event there only marks the page stale;
//   - when the tab is shown again, one catch-up read runs if anything was missed.
// No timer of its own runs while nothing happens: every read is caused by an
// event, by the tab being shown, or by a caller's existing timer.
//
// Plain functions with injectable clock and document, so the scheduling is
// tested under node (live-schedule.test.js) without a browser.

// createLiveSchedule calls run() after events settle. kick() is the event
// hook. catchUp (default run) is what the tab runs when it is shown again
// after missing something.
//
// maxWaitMs (optional, off by default) caps the wait: run() also fires at most
// maxWaitMs after the first kick of a burst. A plain trailing debounce never
// fires while events keep arriving closer together than debounceMs, so a page
// under a steady stream would never read. With sparse events it changes
// nothing. Same shape as the Dashboard's debounceMaxWait (dashboard-landing.js,
// W4); kept here because this schedule owns the hidden-tab rule too.
export function createLiveSchedule(run, opts) {
    const o = Object.assign({
        debounceMs: 1500,
        maxWaitMs: 0,
        catchUp: null,
        doc: typeof document !== 'undefined' ? document : null,
        setTimeout: (fn, ms) => setTimeout(fn, ms),
        clearTimeout: (id) => clearTimeout(id),
    }, opts || {});
    let timer = null;
    let ceiling = null; // the max-wait timer, set by the first kick of a burst
    let stale = false;

    function hidden() { return !!(o.doc && o.doc.visibilityState === 'hidden'); }
    function cancel() {
        if (timer !== null) { o.clearTimeout(timer); timer = null; }
        if (ceiling !== null) { o.clearTimeout(ceiling); ceiling = null; }
    }

    function fire() {
        cancel();
        if (hidden()) { stale = true; return; }
        run();
    }

    function kick() {
        if (hidden()) { cancel(); stale = true; return; }
        if (timer !== null) o.clearTimeout(timer);
        timer = o.setTimeout(fire, o.debounceMs);
        if (o.maxWaitMs > 0 && ceiling === null) ceiling = o.setTimeout(fire, o.maxWaitMs);
    }

    function markStale() { stale = true; }

    if (o.doc && o.doc.addEventListener) {
        o.doc.addEventListener('visibilitychange', () => {
            if (hidden() || !stale) return;
            stale = false;
            cancel();
            (o.catchUp || run)();
        });
    }

    return { kick, markStale, hidden };
}

// createEventThrottle answers ready() true at most once per ms. It is driven
// by the events that ask, not by a timer: with no events it never fires.
// touch() records a read made for another reason, so it counts too.
export function createEventThrottle(ms, now) {
    const clock = now || (() => Date.now());
    let last = -Infinity;
    return {
        ready() {
            const t = clock();
            if (t - last < ms) return false;
            last = t;
            return true;
        },
        touch() { last = clock(); },
    };
}

// countRobotAlerts counts the robot part of the Overview banner from a
// robot-update frame, which carries the whole fleet (engine_background.go
// robotRefreshLoop). Slots with no vehicle id are skipped, as the robot cache
// that /api/missions/alerts counts from skips them.
export function countRobotAlerts(robots) {
    const out = { robots_blocked: 0, robots_emergency: 0, robots_error: 0 };
    (Array.isArray(robots) ? robots : []).forEach((r) => {
        if (!r || !r.vehicle_id) return;
        if (r.blocked) out.robots_blocked++;
        if (r.emergency) out.robots_emergency++;
        if (r.error) out.robots_error++;
    });
    return out;
}
