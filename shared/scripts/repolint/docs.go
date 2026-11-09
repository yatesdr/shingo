package main

import (
	"fmt"
	"io"
	"strings"

	"shingo/shared/scripts/internal/docindex"
)

// --- comment-references, -docs ----------------------------------------------
//
// With -docs, comment-references reads the in-scope docs (the scope and the
// markdown reader are shared/scripts/internal/docindex's) instead of Go
// comments, and is the only rule that runs. What a doc cites must be there:
//
//	line numbers  any name.ext:NNN, anywhere in the doc, fences included, is a
//	              finding: a doc cites the symbol, never the line, because a
//	              line number drifts with every edit above it and nothing
//	              fails when it does.
//	cited paths   a one-token code span that holds a slash or ends in a source
//	              extension (docindex.CitedPath), and a docgen marker's
//	              source, must name a tracked file or directory. They resolve
//	              the way a reader reads them (docindex.Resolver.Cited):
//	              relative to the doc, from the root or as a Go import path,
//	              module-relative (`engine/foo.go`), a component suffix inside
//	              the modules, or a bare basename of any tracked file.
//	links         a relative link target, image or src/srcset/href must
//	              resolve relative to the doc itself (a leading / from the
//	              root): that is what GitHub follows. Anchors are not checked.
//
// Every finding is printed, uncapped: the run is a worklist, and
// scripts/docs-verify.sh reads which docs failed from it.

// docAllowedMissing are names a doc may cite that are never tracked: files
// the operator writes on the host. Each needs a reason, as allowedMissing's do.
var docAllowedMissing = map[string]string{
	"shingocore.yaml": "the operator's Core config on the host; shingocore.example.yaml is the tracked template",
	"shingoedge.yaml": "the operator's Edge config on the host; shingoedge.example.yaml is the tracked template",
	".env":            "the operator's environment file, never committed",
}

// docsRule is the one rule -docs runs.
var docsRule = rule{"comment-references",
	"Cite the SYMBOL, not its line; cite a path that is tracked; link relative to the doc.", docReferences}

// runDocs runs docsRule over t and prints every finding. It reports whether
// the docs were clean.
func runDocs(t *tree, w io.Writer) bool {
	return reportN(w, docsRule, docsRule.check(t), 0)
}

func allowedName(p string) bool {
	_, ok := docAllowedMissing[p[strings.LastIndexByte(p, '/')+1:]]
	return ok
}

func docReferences(t *tree) result {
	var tracked []string
	var docs []*docindex.Doc
	for _, f := range t.files {
		tracked = append(tracked, f.path)
		if f.data != nil && docindex.InScope(f.path) {
			docs = append(docs, docindex.Parse(f.path, f.data))
		}
	}
	r := docindex.NewResolver(tracked)
	var fs []Finding
	for _, d := range docs {
		fs = append(fs, docFindings(d, r)...)
	}
	return result{fs, fmt.Sprintf("%d docs: no line numbers; every cited path and link resolves", len(docs))}
}

// docFindings is one doc's findings, at most one per citation per line.
func docFindings(d *docindex.Doc, r *docindex.Resolver) []Finding {
	var fs []Finding
	seen := map[string]bool{}
	add := func(line int, why string) {
		k := fmt.Sprintf("%d %s", line, why)
		if !seen[k] {
			seen[k] = true
			fs = append(fs, Finding{File: d.Path, Line: line, Why: why})
		}
	}
	for i, l := range d.Lines {
		for _, c := range docindex.LineCites(l) {
			add(i+1, fmt.Sprintf("cites %s -- a doc cites a line number; cite the symbol", c))
		}
	}
	cited := func(line int, what, p string) {
		if !allowedName(p) && !r.Cited(d.Path, p) {
			add(line, fmt.Sprintf("cites %s %s -- no such tracked file or directory", what, p))
		}
	}
	for _, b := range d.Prose {
		for _, c := range b.Code {
			if p, ok := docindex.CitedPath(c.Text); ok {
				cited(c.Line, "path", p)
			}
		}
	}
	for _, s := range d.Sources {
		if p, ok := docindex.SourcePath(s.Target); ok {
			cited(s.Line, "generated source", p)
		}
	}
	for _, l := range d.Links {
		if p, ok := docindex.LinkTarget(l.Target); ok && !r.Link(d.Path, p) {
			add(l.Line, fmt.Sprintf("links %s -- no such tracked file or directory relative to the doc", l.Target))
		}
	}
	return fs
}
