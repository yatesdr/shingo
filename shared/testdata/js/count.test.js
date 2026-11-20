// count.test.js — a bin's count in the browser: the arithmetic (shared/count.js,
// over protocol/testdata/count-examples.json, the file TestCountExamples reads
// for the Go copy) and the words a typed count box shows (count-words.js).
//
// The owner's rule (2026-11-19): a count above the pack is accepted where a
// person types one, with a visible note. The note is said above the pack and
// only there; a pack of 0 is unknown and says nothing.
//
// Run under plain Node via the Go wrapper count_words_test.go. Exit 0 on pass.

'use strict';

const fs = require('fs');
const path = require('path');
const { realm } = require('./shared-modules.js');

let failures = 0;
function check(name, cond, detail) {
    if (cond) { console.log('  ok   ' + name); return; }
    failures++;
    console.log('  FAIL ' + name + (detail ? '\n       ' + detail : ''));
}

const load = realm(undefined, ['count.js', 'count-words.js'], { 'count-words.js': ['partsWords'] });
const { uopRatio, splitUOP, linePieces } = load('count.js');
const { abovePackNote, countReading, typedNote, partsWords, countBoxLabel, countSaid, countBracketed, countOfSaid, UOP_WORD } = load('count-words.js');

const EXAMPLES = JSON.parse(fs.readFileSync(
    path.join(__dirname, '..', '..', '..', 'protocol', 'testdata', 'count-examples.json'), 'utf8')).examples;
check('the worked examples are there to check', EXAMPLES.length > 0);
EXAMPLES.forEach(function(ex) {
    check(ex.name + ': ratio ' + ex.ratio, uopRatio(ex.lines) === ex.ratio, 'got ' + uopRatio(ex.lines));
    const s = splitUOP(ex.count, ex.ratio);
    check(ex.name + ': ' + ex.uop + ' UOP, ' + ex.parts + ' parts', s.uop === ex.uop && s.parts === ex.parts,
        'got ' + JSON.stringify(s));
    check(ex.name + ': no part lost', s.uop * Math.max(ex.ratio, 1) + s.parts === ex.count);
    ex.line_pieces.forEach(function(want, i) {
        const got = linePieces(ex.lines, i, ex.count);
        check(ex.name + ': line ' + i + ' holds ' + want, got === want, 'got ' + got);
    });
    check(ex.name + ': a line past the template is 0', linePieces(ex.lines, ex.lines.length, ex.count) === 0);
});

const over = abovePackNote(803, 800);
check('803 of a pack of 800 is noted, as a warning',
    over && over.tone === 'warn' && over.text === 'Above the pack of 800. Saved as typed.', JSON.stringify(over));
check('800 of 800 is not noted', abovePackNote(800, 800) === null);
check('404 of 800 is not noted', abovePackNote(404, 800) === null);
check('0 of 800 is not noted', abovePackNote(0, 800) === null);
check('an unknown pack says nothing', abovePackNote(803, 0) === null);
check('a box not yet a number says nothing', abovePackNote(NaN, 800) === null);

// countReading: UOP big and the parts small above a ratio of 1; the count
// itself and nothing beside it at 1, so a ratio-1 screen reads as it did.
const reads = [
    [800, 8, 100, '800 parts'], [404, 8, 50, '404 parts'], [803, 8, 100, '803 parts'], [-5, 8, 0, '-5 parts'],
    [1500, 5, 300, '1500 parts'], [1, 8, 0, '1 part'], [800, 1, 800, ''], [60, 1, 60, ''], [12, 0, 12, ''],
];
reads.forEach(function(c) {
    const r = countReading(c[0], c[1]);
    check(c[0] + ' at ' + c[1] + ' reads ' + c[2] + (c[3] ? ' with ' + c[3] : ' and nothing beside'),
        r.big === c[2] && r.small === c[3] && r.split === (c[1] > 1), JSON.stringify(r));
});
check('the unit word is UoP (UOP_WORD)', countSaid(16, 8, '') === '2 UoP \u00b7 16 parts');
check('one part is a part', partsWords(1) === '1 part' && partsWords(2) === '2 parts');

// typedNote: the live line under a typed box.
const notes = [
    [800, 8, 800, '= 100 UoP', ''],
    [404, 8, 800, '= 50 UoP + 4 parts', ''],
    [803, 8, 800, '= 100 UoP + 3 parts. Above the pack of 800. Saved as typed.', 'warn'],
    [1500, 5, 1500, '= 300 UoP', ''],
    [803, 1, 800, 'Above the pack of 800. Saved as typed.', 'warn'],
];
notes.forEach(function(c) {
    const n = typedNote(c[0], c[1], c[2]);
    check('typing ' + c[0] + ' at ' + c[1] + ': "' + c[3] + '"', n && n.text === c[3] && n.tone === c[4], JSON.stringify(n));
});
check('typing 404 at ratio 1 under the pack says nothing', typedNote(404, 1, 800) === null);
check('typing 0 at 8 says it makes 0', (typedNote(0, 8, 800) || {}).text === '= 0 UoP');
check('a box that is not a number says nothing', typedNote(NaN, 8, 800) === null);
check('a count box above ratio 1 names its parts', countBoxLabel(8, 'UoP Count') === 'Count (parts)');
check('a count box at ratio 1 keeps its label', countBoxLabel(1, 'UoP Count') === 'UoP Count');
check('a count said at 8', countSaid(404, 8, '404 UoP') === '50 UoP · 404 parts');
check('a count said at 1 is the plain words', countSaid(404, 1, '404 UoP') === '404 UoP');
check('the one unit word is exported for a screen that composes its own', UOP_WORD === 'UoP');
check('a count left, said at 8', countBracketed(404, 8, '404 left', 'left') === '50 UoP left (404 parts)');
check('a pack said at 8', countBracketed(800, 8, '800', '') === '100 UoP (800 parts)');
check('a count left at 1 is the plain words', countBracketed(404, 1, '404 left', 'left') === '404 left');
check('a count against its pack at 8', countOfSaid(404, 800, 8, '404 of 800') === '50 of 100 UoP (404 of 800 parts)');
check('a count against its pack at 1 is the plain words', countOfSaid(404, 800, 1, '404 of 800') === '404 of 800');

if (failures > 0) {
    console.log(failures + ' failure(s)');
    process.exit(1);
}
console.log('all count checks passed');
