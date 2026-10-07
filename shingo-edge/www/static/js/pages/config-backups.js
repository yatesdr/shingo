// config-backups.js — the Configuration page's backup list, restore action
// and the Backups section's live words, built from explicit inputs so a node
// test can drive them (config_backups_js_test.go). No imports: the page passes
// in el / formatClock / the request and prompt functions it already has.
//
// E5. The restore button used to be built as
//     data-action="stageRestore:"<key>")"
// so the handler received the key with its quotes and a stray bracket, and a
// restore posted a key that matched no backup. The key now rides in data-key,
// set through the DOM (never through an HTML string), and the handler reads
// it from the element it was clicked on.

// formatBytes: 1536 → "1.5 KB".
export function formatBytes(bytes) {
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let value = bytes || 0;
    let unit = 0;
    while (value >= 1024 && unit < units.length - 1) {
        value /= 1024;
        unit++;
    }
    return (unit === 0 ? String(value) : value.toFixed(1)) + ' ' + units[unit];
}

// backupRow is one listed backup as a settings row: when it was made, its size
// and key under it, and Restore on restart (or "pending restart" for the one
// already staged). el is shared/utils.js's element builder.
export function backupRow(item, el, fmt) {
    const when = fmt(item.created_at || item.last_modified || '') || item.key;
    const action = item.restore_pending
        ? el('span', { className: 'set-dim' }, 'pending restart')
        : el('button', {
            type: 'button',
            className: 'set-btn danger',
            dataset: { action: 'stageRestore', key: item.key },
        }, 'Restore on restart');
    return el('div', { className: 'set-fld', dataset: { backupRow: '1' } }, [
        el('label', null, [when, el('small', null, formatBytes(item.size || 0) + ' · ' + item.key)]),
        el('div', { className: 'v' }, [action]),
    ]);
}

// makeStageRestore returns the delegateActions handler for a Restore button.
// deps: stationID, prompt(message, opts) → Promise<string|null>,
// post(url, body) → Promise<{ok, body}>, done(message, ok).
// The typed-station-ID confirmation stays (U3).
export function makeStageRestore(deps) {
    return async function stageRestore(...args) {
        // delegateActions passes (…verb args, element, event).
        const node = args.length >= 2 ? args[args.length - 2] : this;
        const key = node && node.dataset ? node.dataset.key : '';
        if (!key) return null;
        // As before U3: the box opens holding the station ID, and anything
        // else typed (or Cancel) stops the restore.
        const typed = await deps.prompt('Type the station ID to confirm restore.', { value: deps.stationID });
        if (typed !== deps.stationID) {
            deps.done('Restore cancelled: station ID mismatch.', false);
            return null;
        }
        const res = await deps.post('/api/backups/restore', { key: key });
        if (res && res.ok) {
            deps.done('Restore staged. It applies when shingo-edge restarts.', true);
        } else {
            deps.done('Restore not staged: ' + ((res && res.body && res.body.error) || 'no answer.'), false);
        }
        return res;
    };
}

// backupWords is the Backups title's live words, in plant time:
//   off · no backup yet
//   automatic · last 14:05 · next 15:05
// with the warning hue when automatic backups are behind or a run failed.
// clock(ts) formats a timestamp as plant-local HH:MM.
export function backupWords(status, clock) {
    if (!status || status.available === false) return { text: 'unavailable', cls: 'warn' };
    if (!status.configured && !status.enabled) return { text: 'off · no storage set up', cls: '' };
    const parts = [status.enabled ? 'automatic' : 'off'];
    parts.push(status.last_success_at ? 'last ' + clock(status.last_success_at) : 'no backup yet');
    if (status.enabled && status.next_scheduled_at) parts.push('next ' + clock(status.next_scheduled_at));
    let cls = status.enabled && !status.stale ? 'ok' : '';
    const failedLast = status.last_failure_at &&
        (!status.last_success_at || new Date(status.last_failure_at) > new Date(status.last_success_at));
    if (failedLast) parts.push('last run failed');
    if (status.stale || failedLast) cls = 'warn';
    return { text: parts.join(' · '), cls: cls };
}

// ─── The Station section's wire body (lead ruling on E1, 2026-10-07) ─────
//
// A legacy-only Edge (Messaging.StationID set, no Station UID) must still be
// able to save its timezone and auto-confirm. The page has no field for the
// legacy id: it is rendered into the page's data and carried UNCHANGED in the
// station body, so the door sees "either one set" (E1) and saves. It is sent
// only while the config has no UID; once a UID is typed it is the UID alone.

// stationErrors: the client half of E1 — a UID, or a legacy id to carry.
export function stationErrors(d, legacyID) {
    return (d.station_uid || legacyID) ? null : { station_uid: 'Station UID is required.' };
}

// stationBody: what the page sends for the Station section.
export function stationBody(d, legacyID) {
    const body = { station_uid: d.station_uid, timezone: d.timezone, auto_confirm: d.auto_confirm };
    if (legacyID) body.station_id = legacyID;
    return body;
}
