package main

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"shingo/shared/scripts/internal/docindex"
)

// DOCS (-docs). The in-scope markdown docs (shared/scripts/internal/docindex
// says which, and how a doc is read) are read in place of Go comments, by the
// same citations(): the prose of each line, for the call, selector and
// retired-name shapes, and each inline code span whole, read as markdown
// reads it (a span may wrap across lines, which a per-line backtick match
// would cut in two). Not read: fenced blocks (a shell session or a JSON body
// is not a citation of a Go name), link targets and HTML comments. A line
// that reads as a gravestone is exempt, as a comment line is; a span is
// judged by the line it starts on. The live set is the same.
//
// Three shapes a doc writes that a comment does not:
//
//   - a file's name, a Makefile or a Dockerfile with a suffix. A word that is
//     a tracked path or a tracked file's basename is a path, and repolint's
//     to judge, not a Go name: a dotted file name is no selector;
//   - a brace expansion in a code span, `Build{A,B}Steps`, cites each name it
//     expands to; a lone `{Word}` is a placeholder (`SYN_{Name}`) and cites
//     nothing;
//   - a selector chain rooted at another module's package, a type and its
//     field (`http.Request.URL`, a kafka-go config field): every part of it
//     is that module's, as the first selector after the package already is.
//
// And a code span that starts with `/` is a route or a host path
// (`/api/orders`, `/etc/shingo`), never a Go name; repolint
// skips the same spans as paths.
var (
	fileWordRe = regexp.MustCompile(`[A-Za-z0-9_./-]+`)
	braceRe    = regexp.MustCompile(`([A-Za-z0-9_]*)\{([A-Za-z0-9_]+(?:,[A-Za-z0-9_]+)*)\}([A-Za-z0-9_]*)`)
	chainRe    = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+`)
)

// blankForeign blanks every selector chain of s whose root is a package for
// which foreign is true.
func blankForeign(s string, foreign func(pkg string) bool) string {
	return chainRe.ReplaceAllStringFunc(s, func(w string) string {
		if root, _, _ := strings.Cut(w, "."); foreign(root) {
			return strings.Repeat(" ", len(w))
		}
		return w
	})
}

// blankFiles blanks every word of s that names a tracked file.
func blankFiles(s string, files map[string]bool) string {
	return fileWordRe.ReplaceAllStringFunc(s, func(w string) string {
		if files[strings.TrimRight(w, ".")] {
			return strings.Repeat(" ", len(w))
		}
		return w
	})
}

// expandBraces returns the spans a brace expansion spells: a{x,y}b is the
// span with axb in its place and the span with ayb, and a lone {x} is the
// span without it. Each variant is read as a span of its own, so an
// expansion adds no space that would read the span as a quoted value.
func expandBraces(s string) []string {
	m := braceRe.FindStringSubmatchIndex(s)
	if m == nil {
		return []string{s}
	}
	var out []string
	for _, alt := range strings.Split(s[m[4]:m[5]], ",") {
		if strings.Count(s[m[4]:m[5]], ",") == 0 {
			alt = ""
		}
		for _, v := range expandBraces(s[:m[0]] + s[m[2]:m[3]] + alt + s[m[6]:m[7]] + s[m[1]:]) {
			if len(out) < 32 {
				out = append(out, v)
			}
		}
	}
	return out
}

// fileNames is every tracked path and every tracked file's basename.
func fileNames(tracked []string) map[string]bool {
	out := map[string]bool{}
	for _, p := range tracked {
		out[p] = true
		out[path.Base(p)] = true
	}
	return out
}

// docLine is one prose line of a doc and the code spans that start on it.
type docLine struct {
	line int
	text string   // the line, code spans kept, link targets and comments blanked
	bare string   // the same with the code spans blanked
	code []string // the content of each span starting on the line
}

// docLines splits a doc's prose into lines.
func docLines(d *docindex.Doc) []docLine {
	var out []docLine
	for _, b := range d.Prose {
		texts, bares := strings.Split(b.Text, "\n"), strings.Split(b.Bare(), "\n")
		at := map[int]int{} // file line -> index in out
		for i := range texts {
			at[b.Lines[i]] = len(out)
			out = append(out, docLine{line: b.Lines[i], text: texts[i], bare: bares[i]})
		}
		for _, c := range b.Code {
			l := &out[at[c.Line]]
			l.code = append(l.code, c.Text)
		}
	}
	return out
}

// ScanDocs is Scan over the in-scope docs under root (a git checkout) in
// place of Go comments.
func ScanDocs(root string) ([]Hit, error) {
	goFiles, webFiles, err := sources(root)
	if err != nil {
		return nil, err
	}
	live, external := map[string]bool{}, map[string]bool{}
	if _, err := collect(goFiles, parseGo, live, external); err != nil {
		return nil, err
	}
	if _, err := collect(webFiles, parseWeb, live, external); err != nil {
		return nil, err
	}
	ix, err := docindex.Load(root)
	if err != nil {
		return nil, err
	}
	dead := func(ct citation) bool {
		return retired[ct.name] || !live[ct.name] && !external[ct.qual] && !stdlib(ct.qual)
	}
	files := fileNames(ix.Tracked)
	foreign := func(pkg string) bool { return external[pkg] || stdlib(pkg) }
	prepare := func(s string) string { return blankForeign(blankFiles(s, files), foreign) }
	var hits []Hit
	for _, d := range ix.Docs {
		for _, l := range docLines(d) {
			if isGravestone(l.text) {
				continue
			}
			cs := citations(prepare(l.bare))
			for _, span := range l.code {
				if strings.HasPrefix(strings.TrimSpace(span), "/") {
					continue // a route or a host path (`/api/orders`), not a Go name
				}
				for _, v := range expandBraces(prepare(strings.ReplaceAll(span, "`", " "))) {
					cs = append(cs, citations("`"+v+"`")...)
				}
			}
			seen := map[string]bool{}
			for _, ct := range cs {
				if dead(ct) && !seen[ct.name] {
					seen[ct.name] = true
					hits = append(hits, Hit{d.Path, l.line, ct.name})
				}
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].File != hits[j].File {
			return hits[i].File < hits[j].File
		}
		return hits[i].Line < hits[j].Line
	})
	return hits, nil
}
