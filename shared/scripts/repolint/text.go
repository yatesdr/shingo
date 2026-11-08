package main

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// THE PATTERNS ARE THE BASH GUARDS' PATTERNS, translated from POSIX ERE (what
// `git grep -E` and `grep -E` read) to RE2. The languages are the same; two
// spellings differ. A backslash inside a bracket is literal in ERE, so ERE's
// `[\]?` (an optional backslash) is `\\?` here, and `\s` is spelled
// `[[:space:]]`, which RE2 reads as GNU grep reads `\s` (it includes \v). Where
// a rule needs the extent of a match and not just its presence (plant
// addresses), every alternative is bounded so that leftmost-longest and
// leftmost-first pick the same match.

// --- plant-addresses -------------------------------------------------------
//
// No tracked text file carries a private IPv4 address (RFC 1918: 10/8,
// 172.16/12, 192.168/16), with one exemption: 192.168.1.0/24, the example range
// the docs and example configs use for "your server here".
//
// THE RULE IS GENERIC ON PURPOSE. A guard that listed the networks it was
// protecting would itself be the leak it exists to stop, and it would bless
// every network it did not list.
//
// WHY. Fixtures captured from a live fleet carry the site's addressing (robot
// IPs, the fleet server's own address in every lock record), and it looks like
// test data. A private address in a new capture must fail here rather than ride
// in on that trust. Test literals and fixtures use TEST-NET-1/2/3 (RFC 5737).
// The Go twin is shingo-core/fleet/seerrds/testdata_scan_test.go (same rule,
// over that package's fixtures, in the default test suite).
//
// Binary files are skipped (`git grep -I`). The exemption is decided per
// ADDRESS, not per line, so an example address cannot shelter a real one on the
// same line.

// octet is 0-255.
const octet = `(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])`

// privateRe is bounded both sides: not preceded by a digit or dot (so 110.x and
// 1.10.x.y are not read as 10/8), not followed by a digit or by ".digit" (so a
// longer dotted run is not an address). A sentence-ending "." is allowed.
var privateRe = regexp.MustCompile(`(^|[^0-9.])(10\.` + octet + `\.` + octet + `|172\.(1[6-9]|2[0-9]|3[01])\.` + octet +
	`|192\.168\.` + octet + `)\.` + octet + `([^0-9.]|\.[^0-9]|\.?$)`)

// privateAddrs returns the non-exempt private addresses in one line. It scans
// as the guard's awk loop did: each search resumes at the last character of the
// previous match, which may be the next address's leading boundary.
func privateAddrs(s string) []string {
	var out []string
	for {
		loc := privateRe.FindStringIndex(s)
		if loc == nil {
			return out
		}
		a := s[loc[0]:loc[1]]
		if r, size := utf8.DecodeRuneInString(a); r < '0' || r > '9' {
			a = a[size:]
		}
		a = strings.TrimRightFunc(a, func(r rune) bool { return r < '0' || r > '9' })
		if !strings.HasPrefix(a, "192.168.1.") {
			out = append(out, a)
		}
		_, size := utf8.DecodeLastRuneInString(s[:loc[1]])
		s = s[loc[1]-size:]
		if utf8.RuneCountInString(s) <= 1 {
			return out
		}
	}
}

func plantAddresses(t *tree) result {
	var fs []Finding
	for _, f := range t.files {
		if f.data == nil || f.binary() || !mayHoldPrivate(f.data) {
			continue
		}
		eachLine(f.data, func(n int, line []byte) {
			if !mayHoldPrivate(line) {
				return
			}
			for _, a := range privateAddrs(string(line)) {
				fs = append(fs, Finding{File: f.path, Line: n, Why: "private IPv4 address " + a})
			}
		})
	}
	return result{fs, "no private IPv4 in tracked files"}
}

// mayHoldPrivate is a cheap prefilter: every match holds one of these.
func mayHoldPrivate(b []byte) bool {
	return bytes.Contains(b, []byte("10.")) || bytes.Contains(b, []byte("172.")) || bytes.Contains(b, []byte("192.168."))
}

// --- synthetic-part-numbers ------------------------------------------------
//
// No committed file carries a string shaped like a customer part number, or a
// plant PLC struct name.
//
// WHY THIS IS A RULE AND NOT A REVIEW NOTE. The anonymiser used to invent part
// codes in the real ones' shape, on the reasoning that a fixture should
// exercise the shapes a plant uses. Nothing about them said they were
// invented: 503 sat in two committed fixtures, a dozen more were copied into
// test literals by hand, and every handoff shot of the composer photographed a
// part chip that read like a customer's drawing number. AND THE OTHER 137 WERE
// NOT INVENTED AT ALL: a second family with an alphanumeric middle block was
// spread over 52 files in every module, and those came off a plant. (No example
// here: this file is scanned like every other.)
//
// Part numbers only. NODE NAMES AND PLANT NAMES STAY (PLN_01, SMN_BUF_100,
// Supermarket Area, Press B4): they are shingo's own vocabulary and carry
// nothing a customer owns.
//
// THE SHAPES, not a list of known strings:
//
//	shape   five digits, a dash, three to six alphanumerics, a dot, two digits.
//	        THE DOT IS WHAT MAKES IT A PART NUMBER and not a timestamp: Go's
//	        reference time matches the digits and the dash, and not the dot. The
//	        dot may be backslash-escaped, because a JS regex literal spells it so.
//	no-dot  the same with its dot dropped: five digits, a dash, a block with a
//	        letter in it, two digits, then a non-alphanumeric or the end. The
//	        letter keeps the Go reference time (all digits) out.
//	prefix  a prefix in place of the five digits: a dash, a DIGIT-LED block with
//	        a letter in it, a dot (escaped or not), two digits. Digit-led keeps
//	        the synthetic SYN-PART07A.06 (letter-led) out.
//	struct  MES_, a P, the press number, an underscore: the plants publish one
//	        MES struct per process, named for the press. Synthetic structs
//	        (MES_Press_A1) keep the MES_ convention and do not match.
//
// The last two were added after a scrub on 2026-10-17 found fixtures made from
// a real code by dropping its dot or by putting a prefix in place of its digits.
// A code with its separators replaced by spaces is not covered: that shape is
// ordinary prose. Every tracked file is read, binary ones included (the guard
// ran `git grep` without -I), except *.png, *.jpg and *.pdf.
var (
	partRe = regexp.MustCompile(`[0-9]{5}-[0-9A-Z]{3,6}\\?\.[0-9]{2}` +
		`|[0-9]{5}-[0-9A-Z]{0,5}[A-Z][0-9A-Z]{0,5}[0-9]{2}([^0-9A-Za-z.]|$)` +
		`|-[0-9][0-9A-Z]{0,4}[A-Z][0-9A-Z]{0,4}\\?\.[0-9]{2}`)
	structRe = regexp.MustCompile(`MES_P[0-9]+_`)
)

func syntheticPartNumbers(t *tree) result {
	var fs []Finding
	for _, f := range t.files {
		if f.data == nil || strings.HasSuffix(f.path, ".png") || strings.HasSuffix(f.path, ".jpg") ||
			strings.HasSuffix(f.path, ".pdf") {
			continue
		}
		eachLine(f.data, func(n int, line []byte) {
			if dashByDigit(line) {
				if m := partRe.Find(line); m != nil {
					fs = append(fs, Finding{File: f.path, Line: n, Why: fmt.Sprintf("customer-shaped part number %q", m)})
				}
			}
			if bytes.Contains(line, []byte("MES_P")) {
				if m := structRe.Find(line); m != nil {
					fs = append(fs, Finding{File: f.path, Line: n, Why: fmt.Sprintf("plant PLC struct name %q", m)})
				}
			}
		})
	}
	return result{fs, "no NNNNN-XXXXX.NN in any of its three shapes and no plant MES struct in tracked files"}
}

// dashByDigit is a prefilter: every part-number shape holds a dash with a digit
// on one side of it (five digits before, or a digit-led block after).
func dashByDigit(line []byte) bool {
	digit := func(i int) bool { return i >= 0 && i < len(line) && line[i] >= '0' && line[i] <= '9' }
	for i, c := range line {
		if c == '-' && (digit(i-1) || digit(i+1)) {
			return true
		}
	}
	return false
}

// --- swap-mode-literals ----------------------------------------------------
//
// No raw swap-mode string outside the canonical constants and the surfaces
// whose job is naming modes.
//
// WHY. forbidigo only matches identifier expressions, not string literals, so
// "use a protocol.SwapMode constant" cannot be enforced through golangci-lint. This
// catches the "added a new mode, forgot a comparison site" pattern that took
// three commits to clean up. A new mode in protocol/swap_mode.go joins
// swapModes below.
//
// Go, under protocol, shingo-core, shingo-edge and integration: allowed in
// protocol/swap_mode.go (the definitions) and in _test.go files (fixtures).
//
// JavaScript, under shingo-core and shingo-edge, has no typed constant to spell
// a mode with, so the rule is stated per surface. A mode literal is allowed
// where naming the mode IS the job:
//   - *.test.js: fixtures, as for Go.
//   - the FLOW COMPOSER, the surface that names modes since U8/U9 replaced the
//     claim editor, listed by file rather than globbed so the rest of the
//     operator station keeps the rule: processes-desktop.js (the engineer's
//     picker), composer-model.js, composer-render.js, composer-glyphs.js,
//     operator-flow.js, and flowspec-data.js (GENERATED from domain/flowspec,
//     which is generated from swap_mode.go).
//   - shingo-core's test-orders.js page, which builds a swap for whichever
//     mode the tester picks.
//   - manual_swap anywhere in the operator station: a hand-loaded node renders
//     a different board (no robot, no pair), so that check picks a surface; it
//     does not steer a robot.
//
// Everywhere else a JS gate reads the order graph (sibling_order_id) or a
// property the server declares (releases_as_pair). A two_robot test in the
// operator modal's card is what kept a press-index pair from reading as one
// wait, and the Go half could not see it; that is why the composer list is
// named files and not the directory operator-modal.js and operator-render.js
// share with it.
//
// A hit is dropped, as the guard dropped it, when its "path:line:text" line
// holds a colon, optional space, then // anywhere: a comment line, and also
// any line with a URL on it.
var (
	swapModes = []string{"two_robot_press_index", "two_robot", "single_robot", "manual_swap", "sequential", "simple"}

	swapGoDirs   = []string{"protocol/", "shingo-core/", "shingo-edge/", "integration/"}
	swapJSDirs   = []string{"shingo-core/", "shingo-edge/"}
	swapGoSkip   = regexp.MustCompile(`(_test\.go:|protocol/swap_mode\.go:)`)
	swapComment  = regexp.MustCompile(`:[[:space:]]*//`)
	swapJSAllow  = regexp.MustCompile(`(\.test\.js:|^shingo-edge/www/static/js/pages/processes-desktop\.js:|^shingo-edge/www/static/operator-station/(composer-glyphs|composer-model|composer-render|flowspec-data|operator-flow)\.js:|^shingo-core/www/static/pages/test-orders\.js:)`)
	swapJSManual = regexp.MustCompile(`^shingo-edge/www/static/operator-station/`)
	swapJSQuote  = map[string]*regexp.Regexp{}
)

func init() {
	for _, m := range swapModes {
		swapJSQuote[m] = regexp.MustCompile("['\"`]" + m + "['\"`]")
	}
}

func hasPrefixAny(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// swapHits returns one file's raw mode literals.
func swapHits(f *file) []Finding {
	goFile := strings.HasSuffix(f.path, ".go") && hasPrefixAny(f.path, swapGoDirs)
	jsFile := strings.HasSuffix(f.path, ".js") && hasPrefixAny(f.path, swapJSDirs) &&
		!strings.Contains("/"+f.path, "/node_modules/")
	if f.data == nil || !goFile && !jsFile || !namesAMode(f.data) {
		return nil
	}
	var fs []Finding
	eachLine(f.data, func(n int, line []byte) {
		full := "" // the guard's "path:line:text", built only for a line naming a mode
		for _, m := range swapModes {
			if !bytes.Contains(line, []byte(m)) {
				continue
			}
			if full == "" {
				full = fmt.Sprintf("%s:%d:%s", f.path, n, line)
			}
			switch {
			case goFile && bytes.Contains(line, []byte(`"`+m+`"`)) && !swapGoSkip.MatchString(full) && !swapComment.MatchString(full):
				fs = append(fs, Finding{File: f.path, Line: n, Why: fmt.Sprintf("raw swap-mode literal %q outside protocol/swap_mode.go and tests", m)})
			case jsFile && swapJSQuote[m].Match(line) && !swapJSAllow.MatchString(full) && !swapComment.MatchString(full) &&
				(m != "manual_swap" || !swapJSManual.MatchString(full)):
				fs = append(fs, Finding{File: f.path, Line: n, Why: fmt.Sprintf("raw swap-mode literal '%s' in JavaScript outside the surfaces that name modes", m)})
			}
		}
	})
	return fs
}

// namesAMode is a prefilter: a file with no mode name in it has no hit.
func namesAMode(b []byte) bool {
	for _, m := range swapModes {
		if bytes.Contains(b, []byte(m)) {
			return true
		}
	}
	return false
}

func swapModeLiterals(t *tree) result {
	var fs []Finding
	for _, f := range t.files {
		fs = append(fs, swapHits(f)...)
	}
	return result{fs, "no raw swap-mode string outside protocol/swap_mode.go, tests and the JS surfaces that name modes"}
}

// --- conflict-markers ------------------------------------------------------
//
// No tracked text file carries a merge-conflict marker line.
//
// WHY. A stray diff3 marker line rode 71 commits of one rebase in CHANGELOG.md
// before anyone saw it; Go source would have failed to build, but Markdown,
// YAML, SQL and scripts carry one silently.
//
// A marker is a line that starts with seven <, > or | followed by a space or the
// end of the line, or a line that is exactly seven =. Binary files are skipped
// (`git grep -I`). A \r before the line end is part of the line, as it is to
// `git grep`.
var conflictRe = regexp.MustCompile(`^(<<<<<<<|>>>>>>>|\|\|\|\|\|\|\|)( |$)|^=======$`)

func conflictMarkers(t *tree) result {
	var fs []Finding
	for _, f := range t.files {
		if f.data == nil || f.binary() {
			continue
		}
		eachLine(f.data, func(n int, line []byte) {
			if len(line) >= 7 && strings.IndexByte("<>|=", line[0]) >= 0 && conflictRe.Match(line) {
				fs = append(fs, Finding{File: f.path, Line: n, Why: "merge-conflict marker line"})
			}
		})
	}
	return result{fs, fmt.Sprintf("%d tracked files, no marker line", len(t.files))}
}

// --- text-files ------------------------------------------------------------
//
// No tracked file with the text attribute set is indexed as non-text.
//
// WHY. gate.sh once carried literal CR+NUL bytes inside a `tr -d` argument. A
// NUL byte makes Git index the file -text and makes ripgrep skip the WHOLE FILE,
// so every search over scripts/ silently missed gate.sh until someone noticed
// grep could not find a string that was visibly in it. The bytes came in on an
// editor round-trip and survived because nothing looked for them.
//
// The index copy's eol class is `git ls-files --eol`'s i/ column, and the
// attribute is `git check-attr text`: git's own answers, not a reading of
// .gitattributes here. For a file whose text attribute is set (text=auto is
// not "set"), the class must be lf, crlf, mixed or none; -text means the index
// copy has a NUL byte or is otherwise binary, which for a file declared text is
// always corruption, never intent. mixed is allowed: that is a line-ending
// question, not a corrupted file.
func textFiles(t *tree) result {
	var fs []Finding
	for _, f := range t.files {
		if f.eol == "-text" && f.text == "set" {
			fs = append(fs, Finding{File: f.path, Why: "is indexed -text but has the text attribute: the index copy contains a NUL byte or is binary"})
		}
	}
	return result{fs, fmt.Sprintf("%d tracked files, no text-attributed file indexed -text", len(t.files))}
}
