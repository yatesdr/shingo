// Command docgen rewrites the generated blocks of the repo's markdown docs.
//
// WHY. A table that transcribes something the code already states (a message
// type registry, a schema, a route list, a config key list) is a copy, and a
// hand-kept copy drifts the first time the code moves without it. Each such
// table is emitted here from its source of truth instead, and the drift test
// fails the gate when the committed block is not what the generator emits.
//
// THE BLOCK. Hand prose stays around it; only the lines between the markers
// are the generator's:
//
//	<!-- generated: <source> [args...] -->
//	...emitted markdown...
//	<!-- /generated -->
//
// <source> names the generator by the source of truth it reads (a path, and a
// symbol where one file feeds several generators). The longest registered
// name that is a whole-word prefix of the marker text picks the generator;
// the words after it are its arguments.
//
// Usage:
//
//	go run ./scripts/docgen [-root DIR]          rewrite every block in place
//	go run ./scripts/docgen [-root DIR] -check   list stale blocks, exit 1 if any
//
// `make docs-gen` runs the first. The second is what the drift test asserts.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Generator emits the body of a generated block.
type Generator struct {
	// Name is the marker's source text that selects this generator.
	Name string
	// Emit returns the block body for the repo at root, given the marker's
	// remaining words. It must be deterministic: the same tree, the same bytes.
	Emit func(root string, args []string) (string, error)
}

var generators = map[string]Generator{}

// register adds g; each generator file calls it from init.
func register(g Generator) {
	if _, dup := generators[g.Name]; dup {
		panic("docgen: two generators named " + g.Name)
	}
	generators[g.Name] = g
}

var (
	openRe  = regexp.MustCompile(`^<!-- generated: (.+?) -->\s*$`)
	closeRe = regexp.MustCompile(`^<!-- /generated -->\s*$`)
)

// block is one generated block of one doc.
type block struct {
	file      string // slash path from the repo root
	line      int    // 1-based line of the open marker
	source    string // the marker text
	gen       Generator
	args      []string
	bodyStart int // line index (0-based) of the first body line
	bodyEnd   int // line index of the close marker
}

// lookup picks the generator for a marker's source text: the longest
// registered name that is a whole-word prefix of it.
func lookup(source string) (Generator, []string, bool) {
	words := strings.Fields(source)
	for n := len(words); n > 0; n-- {
		if g, ok := generators[strings.Join(words[:n], " ")]; ok {
			return g, words[n:], true
		}
	}
	return Generator{}, nil, false
}

// parseBlocks finds the generated blocks of one doc's lines.
func parseBlocks(file string, lines []string) ([]block, error) {
	var out []block
	for i := 0; i < len(lines); i++ {
		m := openRe.FindStringSubmatch(lines[i])
		if m == nil {
			if closeRe.MatchString(lines[i]) {
				return nil, fmt.Errorf("%s:%d: a close marker with no open marker", file, i+1)
			}
			continue
		}
		g, args, ok := lookup(m[1])
		if !ok {
			return nil, fmt.Errorf("%s:%d: no generator reads %q", file, i+1, m[1])
		}
		b := block{file: file, line: i + 1, source: m[1], gen: g, args: args, bodyStart: i + 1, bodyEnd: -1}
		for j := i + 1; j < len(lines); j++ {
			if openRe.MatchString(lines[j]) {
				return nil, fmt.Errorf("%s:%d: a block opens inside the block opened at line %d", file, j+1, i+1)
			}
			if closeRe.MatchString(lines[j]) {
				b.bodyEnd = j
				break
			}
		}
		if b.bodyEnd < 0 {
			return nil, fmt.Errorf("%s:%d: the block is never closed", file, i+1)
		}
		out = append(out, b)
		i = b.bodyEnd
	}
	return out, nil
}

// docs lists the markdown files under root, skipping VCS, vendor, testdata
// and node_modules trees.
func docs(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		n := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(n, ".") || n == "vendor" || n == "testdata" || n == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(n, ".md") {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// emitted renders a block body as it sits between the markers: the emitted
// text with no leading or trailing blank lines, framed by one blank line on
// each side so the markdown around it renders as written.
func emitted(body string) []string {
	body = strings.Trim(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	if body == "" {
		return []string{""}
	}
	return append(append([]string{""}, strings.Split(body, "\n")...), "")
}

// Stale is one block whose committed body is not what its generator emits.
type Stale struct {
	File, Source string
	Line         int
}

// Run regenerates every block under root. With write it rewrites the docs
// that changed; either way it returns the blocks that were stale and the
// generators no block uses.
func Run(root string, write bool) (stale []Stale, unused []string, err error) {
	files, err := docs(root)
	if err != nil {
		return nil, nil, err
	}
	used := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			return nil, nil, err
		}
		text := strings.ReplaceAll(string(raw), "\r\n", "\n")
		lines := strings.Split(text, "\n")
		blocks, err := parseBlocks(f, lines)
		if err != nil {
			return nil, nil, err
		}
		if len(blocks) == 0 {
			continue
		}
		var out []string
		prev := 0
		for _, b := range blocks {
			used[b.gen.Name] = true
			body, err := b.gen.Emit(root, b.args)
			if err != nil {
				return nil, nil, fmt.Errorf("%s:%d: %s: %w", f, b.line, b.source, err)
			}
			want := emitted(body)
			if strings.Join(lines[b.bodyStart:b.bodyEnd], "\n") != strings.Join(want, "\n") {
				stale = append(stale, Stale{f, b.source, b.line})
			}
			out = append(out, lines[prev:b.bodyStart]...)
			out = append(out, want...)
			prev = b.bodyEnd
		}
		out = append(out, lines[prev:]...)
		if next := strings.Join(out, "\n"); write && next != text {
			if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(f)), []byte(next), 0o644); err != nil {
				return nil, nil, err
			}
		}
	}
	for name := range generators {
		if !used[name] {
			unused = append(unused, name)
		}
	}
	sort.Strings(unused)
	return stale, unused, nil
}

func main() {
	root := flag.String("root", ".", "the repo root")
	check := flag.Bool("check", false, "report stale blocks and write nothing")
	flag.Parse()
	stale, unused, err := Run(*root, !*check)
	if err != nil {
		fmt.Fprintln(os.Stderr, "docgen:", err)
		os.Exit(2)
	}
	for _, u := range unused {
		fmt.Printf("generator %q is used by no block\n", u)
	}
	if *check {
		for _, s := range stale {
			fmt.Printf("%s:%d: the block generated from %q is stale; run make docs-gen\n", s.File, s.Line, s.Source)
		}
		if len(stale) > 0 || len(unused) > 0 {
			os.Exit(1)
		}
		return
	}
	fmt.Printf("docgen: %d block(s) regenerated\n", len(stale))
	if len(unused) > 0 {
		os.Exit(1)
	}
}
