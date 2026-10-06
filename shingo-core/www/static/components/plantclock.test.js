// plantclock.test.js — agreement between Core's plantclock.js and the shared
// plant-clock helpers in shared/utils.js, on one vector table. Run under plain
// node by TestPlantClockJS (clock_globals_drift_test.go). Exit 0 on pass.
//
// plantclock.js must not become a second clock: for every instant and zone
// below, its HH:MM:SS truncated to minutes must equal shared formatClock, and
// its plant date must equal the date part of shared formatTime.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let failures = 0;
function check(name, cond, detail) {
    if (cond) return;
    failures++;
    console.log('FAIL ' + name + (detail ? ': ' + detail : ''));
}

const sharedPath = path.join(__dirname, '..', '..', '..', '..', 'shared', 'utils.js');
const plantPath = path.join(__dirname, 'plantclock.js');

function load(win) {
    const usrc = fs.readFileSync(sharedPath, 'utf8')
        .replace(/export\s+function\s+/g, 'function ')
        .replace(/export\s+const\s+/g, 'const ');
    const window = Object.assign({ addEventListener: () => {} }, win);
    const u = { console, Intl, Date, Map, Set, window };
    vm.createContext(u);
    vm.runInContext(usrc + '\n;__out = { formatClock, formatTime, serverNow };', u, { filename: 'utils.js' });

    const raw = fs.readFileSync(plantPath, 'utf8');
    const stripped = raw.replace(/^import[^;]+;\s*/mg, '');
    if (stripped === raw) throw new Error('plantclock.js imports moved; update load() in plantclock.test.js');
    const p = { console, Intl, Date, Map, window, serverNow: u.__out.serverNow };
    vm.createContext(p);
    vm.runInContext(stripped.replace(/^export /mg, '') +
        '\n;__out = { plantDate, formatClockSeconds, addDays, windowFor, bucketLabel, inProgress };',
        p, { filename: 'plantclock.js' });
    return Object.assign({}, u.__out, p.__out);
}

const MONTHS = { Jan: '01', Feb: '02', Mar: '03', Apr: '04', May: '05', Jun: '06',
    Jul: '07', Aug: '08', Sep: '09', Oct: '10', Nov: '11', Dec: '12' };
function dateOfFormatTime(s) {
    const m = /^([A-Z][a-z]{2}) (\d{1,2}), (\d{4}) /.exec(s);
    return m ? m[3] + '-' + MONTHS[m[1]] + '-' + m[2].padStart(2, '0') : null;
}

const ZONES = ['UTC', 'America/Chicago', 'Asia/Tokyo'];
const INSTANTS = [
    '2026-10-05T00:00:00Z', '2026-10-04T23:59:59Z',      // UTC midnight
    '2026-10-05T04:59:59Z', '2026-10-05T05:00:00Z',      // Chicago midnight (CDT)
    '2026-10-04T14:59:59Z', '2026-10-04T15:00:00Z',      // Tokyo midnight
    '2026-03-08T07:59:59Z', '2026-03-08T08:00:00Z',      // Chicago spring-forward edge
    '2026-11-01T05:59:59Z', '2026-11-01T06:00:00Z',      // Chicago: first 01:00 (CDT)
    '2026-11-01T06:59:59Z', '2026-11-01T07:00:00Z',      // Chicago: second 01:00 (CST)
    '2026-09-05T14:23:01Z',
];

for (const tz of ZONES) {
    const c = load({ PLANT_TZ: tz });
    for (const ts of INSTANTS) {
        const sec = c.formatClockSeconds(ts);
        check(tz + ' ' + ts + ' seconds shape', /^\d{2}:\d{2}:\d{2}$/.test(sec), sec);
        check(tz + ' ' + ts + ' HH:MM agrees with formatClock',
            sec.slice(0, 5) === c.formatClock(ts), sec + ' vs ' + c.formatClock(ts));
        check(tz + ' ' + ts + ' plant date agrees with formatTime',
            c.plantDate(ts) === dateOfFormatTime(c.formatTime(ts)), c.plantDate(ts) + ' vs ' + c.formatTime(ts));
        check(tz + ' ' + ts + ' hour label agrees with formatClock',
            c.bucketLabel(ts, 'hour') === c.formatClock(ts).slice(0, 2) + ':00', c.bucketLabel(ts, 'hour'));
    }
}

// Specific values the shapes above cannot catch.
{
    const c = load({ PLANT_TZ: 'America/Chicago' });
    check('Chicago midnight date', c.plantDate('2026-10-05T05:00:00Z') === '2026-10-05');
    check('Chicago last second of Oct 4', c.plantDate('2026-10-05T04:59:59Z') === '2026-10-04');
    check('Chicago seconds', c.formatClockSeconds('2026-09-05T14:23:01Z') === '09:23:01');
    check('day label is plant date', c.bucketLabel('2026-10-05T05:00:00Z', 'day') === '10/5',
        c.bucketLabel('2026-10-05T05:00:00Z', 'day'));
    check('hour label midnight', c.bucketLabel('2026-10-05T05:00:00Z', 'hour') === '00:00');
    check('addDays across fall-back', c.addDays('2026-10-31', 2) === '2026-11-02');
    check('addDays across month', c.addDays('2026-03-01', -1) === '2026-02-28');
    check('null is empty', c.plantDate(null) === '' && c.formatClockSeconds(null) === '' && c.bucketLabel(null, 'day') === '');
}

// windowFor resolves from the SERVER's now on the plant's calendar. 2026-10-05
// 03:00Z is still Oct 4 in Chicago and already Oct 5 in Tokyo.
{
    const at = { now: '2026-10-05T03:00:00Z', speed: 1, sim: false };
    const ch = load({ PLANT_TZ: 'America/Chicago', SHINGO_CLOCK: at });
    const w = ch.windowFor('7d');
    check('7d until is plant today', w.until === '2026-10-04', w.until);
    check('7d since', w.since === '2026-09-28', w.since);
    check('7d prev window', w.prevUntil === '2026-09-27' && w.prevSince === '2026-09-21', w.prevSince + '..' + w.prevUntil);
    check('7d buckets by day', w.bucket === 'day' && w.days === 7);
    const t = ch.windowFor('today');
    check('today is one plant day, hourly', t.since === '2026-10-04' && t.until === '2026-10-04' && t.bucket === 'hour');
    check('52w', ch.windowFor('52w').since === ch.addDays('2026-10-04', -363));

    const tk = load({ PLANT_TZ: 'Asia/Tokyo', SHINGO_CLOCK: at });
    check('Tokyo today', tk.windowFor('today').until === '2026-10-05', tk.windowFor('today').until);

    check('current hour in progress', ch.inProgress('2026-10-05T03:00:00Z', 'hour'));
    check('previous hour finished', !ch.inProgress('2026-10-05T02:00:00Z', 'hour'));
    check('plant today in progress', ch.inProgress('2026-10-04T05:00:00Z', 'day'));
    check('plant yesterday finished', !ch.inProgress('2026-10-03T05:00:00Z', 'day'));
}

// No PLANT_TZ: degrades to the browser zone, same as shared/utils.js.
{
    const c = load({});
    const ts = '2026-09-05T14:23:01Z';
    check('fallback agrees with formatClock', c.formatClockSeconds(ts).slice(0, 5) === c.formatClock(ts));
    check('fallback plant date shape', /^\d{4}-\d{2}-\d{2}$/.test(c.plantDate(ts)));
}

if (failures) { console.log(failures + ' failure(s)'); process.exit(1); }
console.log('plantclock: ok');
