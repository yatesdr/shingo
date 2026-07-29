package shared

import (
	"regexp"
	"sort"
	"strings"
)

// Layer-scale drift support (D19).
//
// The layer tokens in tokens.css only mean something if everything uses them.
// One raw `z-index: 10000` re-establishes the ratchet the scale exists to end:
// the next person needs to cover it, picks 10001, and within a release there
// are three topmost layers again. That is not hypothetical — it is what the
// tree looked like before the scale, with 2000, 9999 and 10000 all claiming
// the top.
//
// So the guard is on the SHAPE, not on any particular file: any z-index
// declaration whose value is not a --z-* token (or an explicit keyword) is a
// finding, wherever it lives — stylesheet, template <style> block, or a
// cssText string built in JS.

// zIndexDeclPattern matches a z-index declaration and captures its value, in
// CSS or in a JS style string. The value stops at the first `;`, `}` or quote,
// which is what ends a declaration in every one of those contexts.
var zIndexDeclPattern = regexp.MustCompile(`z-index\s*:\s*([^;}"'` + "`" + `]+)`)

// zLayerVarPattern matches the accepted form: var(--z-<name>), optionally with
// a fallback.
var zLayerVarPattern = regexp.MustCompile(`^var\(\s*--z-[a-z-]+\s*(?:,[^)]*)?\)$`)

// zIndexKeywords are the CSS-wide and initial values, which carry no ordering
// and so need no token. `auto` in particular is how you say "do not create a
// stacking context", which is a meaningful thing to write.
var zIndexKeywords = map[string]bool{
	"auto": true, "inherit": true, "initial": true, "unset": true, "revert": true,
}

// RawZIndexUses returns every z-index declaration in src whose value is not a
// --z-* token or a keyword, as the literal text found. Comments are stripped
// first: prose about z-index in a comment above a rule is normal, and without
// the strip the comment explaining the scale would report itself.
//
// A value carrying !important is returned even when it IS a token. An
// !important on a z-index is the escape hatch that makes a layer system
// unenforceable — whoever writes the next one copies it, and after two of
// them the scale describes the code rather than governing it.
func RawZIndexUses(src string) []string {
	src = cssCommentPattern.ReplaceAllString(src, " ")
	src = jsLineCommentPattern.ReplaceAllString(src, " ")

	seen := map[string]bool{}
	for _, m := range zIndexDeclPattern.FindAllStringSubmatch(src, -1) {
		val := strings.TrimSpace(m[1])
		if strings.Contains(val, "!important") {
			seen[val] = true
			continue
		}
		if zIndexKeywords[strings.ToLower(val)] || zLayerVarPattern.MatchString(val) {
			continue
		}
		seen[val] = true
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// jsLineCommentPattern matches a whole-line `//` comment. Deliberately
// anchored to the start of a line: a bare `//` search would eat the rest of
// any line containing a URL.
var jsLineCommentPattern = regexp.MustCompile(`(?m)^\s*//.*$`)

// ZLayerScale reads the --z-* tokens out of a stylesheet's :root block, name
// to value, so two copies of the scale can be compared. Returns nil when the
// block is absent, which the caller must treat as a failure rather than as an
// empty match — the operator station's copy going missing would look exactly
// like "no tokens to compare" otherwise.
func ZLayerScale(css string) map[string]string {
	css = cssCommentPattern.ReplaceAllString(css, " ")
	m := regexp.MustCompile(`(?s):root\s*\{(.*?)\}`).FindStringSubmatch(css)
	if m == nil {
		return nil
	}
	out := map[string]string{}
	for _, d := range regexp.MustCompile(`(--z-[a-z-]+)\s*:\s*([^;]+);`).FindAllStringSubmatch(m[1], -1) {
		out[d[1]] = strings.TrimSpace(d[2])
	}
	return out
}
