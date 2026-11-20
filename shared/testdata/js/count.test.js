// count.test.js — the words a typed count box shows (shared/count-words.js).
//
// The owner's rule (2026-11-19): a count above the pack is accepted where a
// person types one, with a visible note. The note is said above the pack and
// only there; a pack of 0 is unknown and says nothing.
//
// Run under plain Node via the Go wrapper count_words_test.go. Exit 0 on pass.

'use strict';

const { realm } = require('./shared-modules.js');

let failures = 0;
function check(name, cond, detail) {
    if (cond) { console.log('  ok   ' + name); return; }
    failures++;
    console.log('  FAIL ' + name + (detail ? '\n       ' + detail : ''));
}

const load = realm(undefined, ['count-words.js']);
const { abovePackNote } = load('count-words.js');

const over = abovePackNote(803, 800);
check('803 of a pack of 800 is noted, as a warning',
    over && over.tone === 'warn' && over.text === 'Above the pack of 800. Saved as typed.', JSON.stringify(over));
check('800 of 800 is not noted', abovePackNote(800, 800) === null);
check('404 of 800 is not noted', abovePackNote(404, 800) === null);
check('0 of 800 is not noted', abovePackNote(0, 800) === null);
check('an unknown pack says nothing', abovePackNote(803, 0) === null);
check('a box not yet a number says nothing', abovePackNote(NaN, 800) === null);

if (failures > 0) {
    console.log(failures + ' failure(s)');
    process.exit(1);
}
console.log('all count checks passed');
