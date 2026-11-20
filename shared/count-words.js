// count-words.js — the words a count shows, the same on every screen of both
// servers: the floor's tiles and keypads, the Edge desktop's pages, Core's
// pages. The arithmetic is count.js's; this file is the words around it.
//
// THE OWNER'S RULES (2026-11-19). A UOP is one assembly cycle's worth; a bin
// stores what is in it (parts, or kits for a kit). Every screen shows the UOP
// big with the parts small beside it when the ratio is above 1; at a ratio of
// 1 nothing extra is shown, so a screen reads exactly as it did. Everything a
// person types is in parts (kits for a kit), and the box shows the UOP it makes
// as they type. A count above the pack is accepted, with a visible note ("it
// happens in real life"); the note never refuses. A typed 0 is an empty bin.
//
// The wording is the owner's to set (OWNER-WORDS); this file is the one place
// it is written, so changing it is one edit. No DOM: it runs in a browser and in
// node's test runner (testdata/js/count.test.js).

import { splitUOP } from '/static/shared/count.js';

// UOP_WORD is the unit's word on screen. docs/terminology.md › Units and
// casing writes UoP in UI text, and every existing label does; the owner's
// brief writes UOP. One constant, so the choice is one edit: a screen that
// writes the word beside a number it composed itself reads it from here.
// OWNER-WORDS
export const UOP_WORD = 'UoP';

// partsWords is a number of parts, said: "404 parts", "1 part".
function partsWords(n) {
    // OWNER-WORDS
    return n + (n === 1 || n === -1 ? ' part' : ' parts');
}

// countReading is how a count shows at a ratio: big is the number the screen
// shows where it showed the count, small the words beside it. Above a ratio of
// 1, big is the whole UOP and small the parts ("50" and "404 parts" for 404 at
// 8); at 1, big is the count and small is '', the screen as it always was.
export function countReading(count, ratio) {
    const n = Math.trunc(Number(count) || 0);
    const r = Math.trunc(Number(ratio) || 0);
    if (r <= 1) return { big: n, small: '', split: false };
    return { big: splitUOP(n, r).uop, small: partsWords(n), split: true };
}

// countSaid is a count said in a line of text: above a ratio of 1 the UOP and
// the parts, "50 UoP · 404 parts"; at 1, plain, the screen's own words for it,
// unchanged.
export function countSaid(count, ratio, plain) {
    const r = countReading(count, ratio);
    // OWNER-WORDS
    return r.split ? r.big + ' ' + UOP_WORD + ' \u00b7 ' + r.small : plain;
}

// countBracketed is a count said in a sentence: plain at a ratio of 1; above
// it the UOP, then after (a word such as "left", or ''), then the parts in
// brackets: "50 UoP left (404 parts)", "100 UoP (800 parts)".
export function countBracketed(count, ratio, plain, after) {
    const r = countReading(count, ratio);
    // OWNER-WORDS
    return r.split ? r.big + ' ' + UOP_WORD + (after ? ' ' + after : '') + ' (' + r.small + ')' : plain;
}

// countOfSaid is a count against its pack: plain at a ratio of 1; above it
// "50 of 100 UoP (404 of 800 parts)" (RELEASE FULL's prompt, the loader
// board's press rows).
export function countOfSaid(have, capacity, ratio, plain) {
    const h = countReading(have, ratio), c = countReading(capacity, ratio);
    // OWNER-WORDS
    return h.split ? h.big + ' of ' + c.big + ' ' + UOP_WORD + ' (' + have + ' of ' + capacity + ' parts)' : plain;
}

// countBoxLabel is the label of a box a person types a bin's count into:
// above a ratio of 1 it names the parts the box takes ("Count (parts)"); at 1
// it is the box's own label (plain), unchanged.
export function countBoxLabel(ratio, plain) {
    // OWNER-WORDS
    return Math.trunc(Number(ratio) || 0) > 1 ? 'Count (parts)' : plain;
}

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

// typedNote is the line under a box a person types a count into: the UOP it
// makes, above a ratio of 1 ("= 50 UoP + 4 parts" for 404 at 8, "= 100 UoP"
// for 800), then the above-pack note when the count is over the pack. null when
// there is nothing to say: at a ratio of 1 under the pack, or a box that is not
// yet a number. {text, tone} in the keypad's opts.note shape; tone 'warn' when
// the note is above the pack.
export function typedNote(count, ratio, pack) {
    const n = Number(count);
    if (!Number.isFinite(n)) return null;
    const r = Math.trunc(Number(ratio) || 0);
    let text = '';
    if (r > 1) {
        const s = splitUOP(n, r);
        // OWNER-WORDS
        text = '= ' + s.uop + ' ' + UOP_WORD + (s.parts ? ' + ' + partsWords(s.parts) : '');
    }
    const over = abovePackNote(n, pack);
    if (over) text = (text ? text + '. ' : '') + over.text;
    if (!text) return null;
    return { text: text, tone: over ? 'warn' : '' };
}
