package main

import (
	"fmt"
	"regexp"
	"strings"

	"shingo/shared/scripts/internal/docindex"
)

// DOCS (-docs). The in-scope markdown docs (shared/scripts/internal/docindex
// says which, and how a doc is read) are read in place of Go comments: their
// prose, their inline code and their fenced blocks, each a block at a time so
// a wrapped gravestone phrase is read whole. A citation in a doc is what it
// is in a comment, by the same rules, and resolves the same way, subtests
// included.
//
// A -RUN ARGUMENT IS A REGEX, AND IS JUDGED AS ONE. A doc tells its reader
// to run `go test -run 'A|B/case'`, and a renamed B leaves the command
// running nothing for that half without a word. After -run, -run=,
// -test.run or -test.run= (bare, '…' or "…"), the argument up to its first
// top-level `/` (go test's subtest separator) is split on its top-level `|`,
// and each alternative must match the name of at least one Test, Fuzz,
// Benchmark or Example function in the five modules: Go regexp, unanchored,
// as go test matches. A name inside the argument is judged by this rule only,
// not also as a bare citation.
//
// Not judged: an argument that is a placeholder (`<pattern>`, `$RUN`, one
// that starts with `-`); and another command's -run flag (`simcalc -run
// 400m`), which is any -run with neither `go test` (or a `.test` binary)
// before it on its command line nor a test-shaped name in its argument.
var (
	runFlagRe  = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])-(?:test\.)?run(?:=|[ \t]+)`)
	testNameRe = regexp.MustCompile(`(?:Test|Fuzz|Benchmark|Example)(?:[A-Z0-9_(]|$)`)
	goTestRe   = regexp.MustCompile(`\bgo test\b|\.test\b`)
)

// runFlag is one -run argument in a block of text.
type runFlag struct {
	start, end int // the argument's offsets, quotes excluded
	alts       []runAlt
}

// runAlt is one top-level alternative of a -run argument's first level.
type runAlt struct {
	text  string
	start int
}

// runArgs finds the judged -run arguments in text.
func runArgs(text string) []runFlag {
	var out []runFlag
	for _, m := range runFlagRe.FindAllStringIndex(text, -1) {
		a, ok := readRunArg(text, m[1])
		if !ok {
			continue
		}
		arg := text[a.start:a.end]
		if strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "$") || strings.HasPrefix(arg, "<") ||
			strings.HasPrefix(arg, "{") || strings.ContainsAny(arg, "<>…") || strings.Contains(arg, "...") {
			continue // a placeholder, or the next flag
		}
		first := splitTop(arg, '/')[0]
		for _, alt := range splitTop(first.text, '|') {
			if alt.text != "" {
				a.alts = append(a.alts, runAlt{alt.text, a.start + alt.start})
			}
		}
		if !testNameRe.MatchString(arg) && !goTestRe.MatchString(commandLine(text, m[0])) &&
			!strings.Contains(text[m[0]:m[1]], "-test.run") {
			continue // another command's -run flag
		}
		out = append(out, a)
	}
	return out
}

// readRunArg reads the argument starting at offset p: quoted, or bare up to
// a space or a backtick, a trailing `.`/`,`/`;`/`:` or unmatched `)` being
// the sentence's.
func readRunArg(text string, p int) (runFlag, bool) {
	if p >= len(text) {
		return runFlag{}, false
	}
	if q := text[p]; q == '\'' || q == '"' {
		j := strings.IndexByte(text[p+1:], q)
		if j <= 0 {
			return runFlag{}, false
		}
		return runFlag{start: p + 1, end: p + 1 + j}, true
	}
	j := p
	for j < len(text) && !strings.ContainsRune(" \t\n`'\"", rune(text[j])) {
		j++
	}
	for j > p {
		c, arg := text[j-1], text[p:j]
		if strings.IndexByte(".,;:", c) >= 0 || c == ')' && strings.Count(arg, ")") > strings.Count(arg, "(") {
			j--
			continue
		}
		break
	}
	return runFlag{start: p, end: j}, j > p
}

// commandLine is the text of the command line holding offset at: its own
// line back to the line start, and earlier lines joined by a trailing `\`.
func commandLine(text string, at int) string {
	start := strings.LastIndexByte(text[:at], '\n') + 1
	for start > 0 {
		prev := strings.LastIndexByte(text[:start-1], '\n') + 1
		if !strings.HasSuffix(strings.TrimRight(text[prev:start-1], " \t"), "\\") {
			break
		}
		start = prev
	}
	return text[start:at]
}

// splitTop splits s on sep outside (), [] and a backslash escape.
func splitTop(s string, sep byte) []runAlt {
	var out []runAlt
	depth, from := 0, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\':
			i++
		case c == '(' || c == '[':
			depth++
		case (c == ')' || c == ']') && depth > 0:
			depth--
		case c == sep && depth == 0:
			out = append(out, runAlt{s[from:i], from})
			from = i + 1
		}
	}
	return append(out, runAlt{s[from:], from})
}

// runnable reports whether some Test, Fuzz, Benchmark or Example function's
// name matches re.
func (t *Tree) runnable(re *regexp.Regexp) bool {
	for name := range t.tests {
		if re.MatchString(name) {
			return true
		}
	}
	for name := range t.examples {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

// scanDoc appends one doc block's dangling citations and gravestones.
func (t *Tree) scanDoc(file string, b docindex.Block, hits, graves []Hit) ([]Hit, []Hit) {
	args := runArgs(b.Text)
	inArg := func(off int) bool {
		for _, a := range args {
			if off >= a.start && off < a.end {
				return true
			}
		}
		return false
	}
	for _, a := range args {
		for _, alt := range a.alts {
			re, err := regexp.Compile(alt.text)
			switch {
			case err != nil:
				hits = append(hits, Hit{file, b.LineAt(alt.start), alt.text, "is a -run alternative that is not a Go regexp: " + err.Error()})
			case !t.runnable(re):
				hits = append(hits, Hit{file, b.LineAt(alt.start), alt.text, "is a -run alternative no Test, Fuzz, Benchmark or Example name matches"})
			}
		}
	}
	for _, c := range citations(b.Text) {
		if inArg(c.start) {
			continue // judged as a -run alternative above
		}
		if _, isTest := t.tests[c.name]; !isTest && t.declared[c.name] && !c.pin {
			continue
		}
		why := t.resolve(c)
		switch {
		case why == "":
		case c.grave && !c.pin:
			graves = append(graves, Hit{file, b.LineAt(c.start), c.text, "is a gravestone"})
		default:
			hits = append(hits, Hit{file, b.LineAt(c.start), c.text, why})
		}
	}
	return hits, graves
}

// ScanDocs is Scan over the in-scope docs under root (a git checkout) in
// place of Go comments.
func ScanDocs(root string) (hits, graves []Hit, err error) {
	t, err := Load(root)
	if err != nil {
		return nil, nil, err
	}
	ix, err := docindex.Load(root)
	if err != nil {
		return nil, nil, err
	}
	for _, d := range ix.Docs {
		blocks := append([]docindex.Block{}, d.Prose...)
		for _, f := range d.Fences {
			blocks = append(blocks, f.Body)
		}
		for _, b := range blocks {
			if b.Text != "" {
				hits, graves = t.scanDoc(d.Path, b, hits, graves)
			}
		}
	}
	sortHits(hits)
	sortHits(graves)
	return hits, graves, nil
}

// docsSummary is the closing line of a -docs run with hits.
func docsSummary(n int) string {
	return fmt.Sprintf("%d dangling test citation(s) in docs. Point the doc at the test that pins it now, or say the pin is gone.", n)
}
