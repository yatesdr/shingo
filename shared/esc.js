// esc.js — the one HTML escape for markup built by string concatenation.
//
// WHY IT IS NOT dom.js's escapeHtml. That one was DOM-based when this file
// was written: it put the value in a text node and read innerHTML back, which
// escapes `&`, `<` and `>` and leaves `"` alone. That is correct for text
// BETWEEN tags and wrong for a value going INSIDE an attribute, which is most
// of what the composer's renderers build:
//
//     '<button data-val="' + esc(name) + '">'
//
// A name carrying a quote closes the attribute there. No plant name does
// today; that is a fact about this week's data, not a property of the code.
//
// There were four escapers on this tree: two DOM-based ones in the base
// (escapeHtml, then in utils.js, and operator-util.js's esc) and two
// identical regex copies added by the composer — which DID escape quotes,
// silently differing from the base pair they sat beside. This is that pair, once, with the
// difference written down instead of implied.
//
// No imports, no DOM. It runs in a browser, in node's test runner and inside
// the vm sandbox the render tests use, which is why it is its own file rather
// than another export of utils.js: the station would otherwise pull the clock,
// the SSE factory and the modal machinery to escape a string.
//
// escapeHtml has since learned quotes; the two now differ by one character:
// escapeHtml escapes `'` too (&#39;), esc leaves it. They stay two, each
// pinned as it is (dom.js's header says why they are not one file).

const ESCAPES = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' };

// esc escapes a value for either a text node or a double-quoted attribute.
// null and undefined are the empty string, not the words "null" and
// "undefined" — a missing value renders as nothing, which is what every
// caller means by it.
export function esc(s) {
    return String(s === null || s === undefined ? '' : s).replace(/[&<>"]/g, c => ESCAPES[c]);
}
