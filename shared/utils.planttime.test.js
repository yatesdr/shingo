// Unit tests for the plant-local time formatters in shared/utils.js —
// formatTime, formatClock, and the convertTimestamps rollover shim.
// Run under plain Node via the Go test wrapper. Exit 0 on pass, 1 on
// any assertion failure.
//
// These pin the CLIENT half of the plant-local convention: JS-rendered
// timestamps must land on the same clock the server painted (window.PLANT_TZ),
// and the rollover shim must rewrite only old UTC-painted text — never a
// plant-local server paint, which would double-convert.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0;
let failed = 0;
function assert(cond, label) {
    if (cond) { passed++; }
    else { failed++; console.error('FAIL: ' + label); }
}

function loadUtils(windowObj) {
    const src = fs.readFileSync(path.join(__dirname, 'utils.js'), 'utf8');
    const transformed = src
        .replace(/export\s+function\s+/g, 'function ')
        .replace(/export\s+const\s+/g, 'const ');
    const win = Object.assign({ addEventListener: () => {} }, windowObj || {});
    const ctxObj = {
        console,
        Intl,
        Date,
        Map,
        window: win,
    };
    const ctx = vm.createContext(ctxObj);
    vm.runInContext(transformed, ctx, { filename: 'utils.js' });
    return ctxObj;
}

// ── formatTime / formatClock honor window.PLANT_TZ ─────────────────────────
//
// 2026-09-05T14:23:01Z is 09:23 CDT in America/Chicago. Node's Intl carries
// full tzdata on every platform this test runs on (CI docker image and dev
// boxes), so the zone conversion is exercised for real.

function tzTests() {
    const ctx = loadUtils({ PLANT_TZ: 'America/Chicago' });

    const full = ctx.formatTime('2026-09-05T14:23:01Z');
    assert(full === 'Sep 5, 2026 09:23 CDT',
        'formatTime plant-local: got ' + JSON.stringify(full) + ' want "Sep 5, 2026 09:23 CDT"');

    const clock = ctx.formatClock('2026-09-05T14:23:01Z');
    assert(clock === '09:23',
        'formatClock plant-local: got ' + JSON.stringify(clock) + ' want "09:23"');

    // The Go twin renders the same instant identically — this is the
    // drift guard between planttime.Format and the JS formatter.
    // (Byte-for-byte: "Sep 5, 2026 09:23 CDT".)
}

function fallbackTests() {
    // No PLANT_TZ: falls back to the browser zone, which in this VM context
    // is the host's. formatClock must still produce H:MM-shaped text —
    // degraded, not garbage.
    const ctx = loadUtils({});
    const clock = ctx.formatClock('2026-09-05T14:23:01Z');
    assert(/^\d{2}:\d{2}$/.test(clock),
        'formatClock without PLANT_TZ degrades to browser zone, got ' + JSON.stringify(clock));

    // Zero/invalid spellings match the Go package: zero time and null are
    // "-", never "Invalid Date".
    assert(ctx.formatTime('0001-01-01T00:00:00Z') === '-',
        'formatTime zero instant renders "-"');
    assert(ctx.formatTime(null) === '-',
        'formatTime null renders "-"');
    assert(ctx.formatClock(null) === '',
        'formatClock null renders ""');
    assert(ctx.formatTime('not-a-date') === 'not-a-date',
        'formatTime invalid input passes through unchanged');
}

// ── convertTimestamps rollover shim ────────────────────────────────────────
//
// The shim rewrites ONLY old UTC-painted text (ends in " UTC"). A server
// paint that is already plant-local ("... CDT") must be left alone —
// rewriting it would double-convert and show a wrong clock mid-deploy.

function shimTests() {
    // Minimal time-element stub.
    function makeTimeNode(utc, text) {
        return {
            tagName: 'TIME',
            textContent: text,
            getAttribute: (name) => (name === 'data-utc' ? utc : (name === 'data-converted' ? null : null)),
            setAttribute: (name, v) => {
                if (name === 'data-converted') makeTimeNode.converted++;
            },
        };
    }
    makeTimeNode.converted = 0;

    const nodes = [
        makeTimeNode('2026-09-05T14:23:01Z', 'Sep 5, 2026 14:23 UTC'), // old UTC paint → rewritten
        makeTimeNode('2026-09-05T14:23:01Z', 'Sep 5, 2026 09:23 CDT'), // plant-local paint → untouched
    ];
    let convertedCount = 0;
    nodes[0].setAttribute = (name) => { if (name === 'data-converted') convertedCount++; };

    const ctx = loadUtils({ PLANT_TZ: 'America/Chicago' });
    const document = {
        querySelectorAll: (sel) => {
            assert(sel === 'time[data-utc]',
                'shim selector stays time[data-utc] (converted-check is in-loop), got ' + sel);
            return nodes;
        },
    };
    ctx.convertTimestamps(document);

    assert(nodes[0].textContent === 'Sep 5, 2026 09:23 CDT',
        'shim rewrites old UTC paint to plant-local, got ' + JSON.stringify(nodes[0].textContent));
    assert(nodes[1].textContent === 'Sep 5, 2026 09:23 CDT',
        'shim leaves plant-local server paint untouched');
    assert(convertedCount === 1,
        'shim marks exactly the rewritten node data-converted, count=' + convertedCount);
}

tzTests();
fallbackTests();
shimTests();

if (failed > 0) {
    console.error(passed + ' passed, ' + failed + ' FAILED');
    process.exit(1);
}
console.log(passed + ' passed');
