// count.js — the arithmetic of a bin's count, the browser's twin of
// protocol/count.go, served to both UIs from /static/shared/.
//
// THE RULE (owner, 2026-11-19). A UOP is one assembly cycle's worth. A bin's
// count is what is physically in the carrier: parts for a payload of one
// manifest line, kits for a payload of several. UOP is never stored; it is
// worked out wherever it is shown, count ÷ ratio, the whole part, with the
// parts left over shown beside it.
//
// Pinned to the Go copy by one file of worked examples,
// protocol/testdata/count-examples.json (testdata/js/count.test.js here,
// TestCountExamples there). A line is a payload manifest line as both servers
// send it: {part_number, parts_per_cycle}. No imports, no DOM.

// uopRatio is a payload's UOP ratio: how many of a bin's counted things one
// assembly cycle takes. One line: its parts_per_cycle, at least 1. A kit
// (several lines) or no line: 1.
export function uopRatio(lines) {
    if (!lines || lines.length !== 1) return 1;
    return Math.max(Math.trunc(Number(lines[0].parts_per_cycle) || 0), 1);
}

// splitUOP splits a bin's count into the whole UOP it makes and the parts left
// over, truncated toward zero (-5 at 8 is 0 UOP and -5 parts), so
// uop*ratio + parts is the count: never a fraction, never a lost part. A ratio
// below 1 counts as 1.
export function splitUOP(count, ratio) {
    const n = Math.trunc(Number(count) || 0);
    const r = Math.trunc(Number(ratio) || 0);
    if (r <= 1) return { uop: n, parts: 0 };
    // `+ 0` folds the -0 a negative count under one UOP would give.
    return { uop: Math.trunc(n / r) + 0, parts: (n % r) + 0 };
}

// linePieces is how many pieces of line i a bin of count holds: the count for
// a payload of one line, count × the line's parts_per_cycle for a kit. An
// index outside lines is 0.
export function linePieces(lines, i, count) {
    if (!lines || i < 0 || i >= lines.length) return 0;
    const n = Math.trunc(Number(count) || 0);
    if (lines.length === 1) return n;
    return n * Math.trunc(Number(lines[i].parts_per_cycle) || 0);
}
