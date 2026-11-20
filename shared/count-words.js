// count-words.js — the words a typed count box shows under its number, the
// same on every screen of both servers: the floor keypads, the Edge desktop's
// Load and release prompts, Core's Bins page.
//
// THE OWNER'S RULE (2026-11-19): a count above the pack is accepted where a
// person types one, with a visible note ("it happens in real life"). A typed 0
// is an empty bin; a person can never type a negative. The note never refuses:
// the box takes the number as typed. A strict-pack press still stops its own
// counting at the pack; that is the engine's, not a typed box's.
//
// The wording is the owner's to set (OWNER-WORDS); this file is the one place
// it is written, so changing it is one edit. No imports, no DOM: it runs in a
// browser and in node's test runner (testdata/js/count-words.test.js).

// abovePackNote is the note for a typed count above the payload's pack, or
// null when the count is at or under it or the pack is unknown (0). It answers
// {text, tone} in the shape the floor keypad's opts.note takes.
export function abovePackNote(count, pack) {
    const n = Number(count);
    const p = Number(pack);
    if (!(p > 0) || !(n > p)) return null;
    // OWNER-WORDS
    return { text: 'Above the pack of ' + p + '. Saved as typed.', tone: 'warn' };
}
