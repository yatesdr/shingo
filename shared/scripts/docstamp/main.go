// Command docstamp keeps the "Verified against: <sha>" line under the title of
// every in-scope doc (shared/scripts/internal/docindex says which docs, and
// where the line goes), and lists the docs a change may have made wrong.
//
// WHY A STAMP. "Last touched" is read as "current", and it is not: a doc
// edited last week for a typo can be wrong about a mechanism rewritten since,
// and a doc nobody has touched since July can be exactly right. The stamp says
// which commit the doc was last read against with the citation gate clean.
// It moves only when someone verifies (scripts/docs-verify.sh, run once at the
// end of a stream), never on CI, so it never churns a commit of its own.
//
// THREE MODES.
//
//	-check              read-only, what the gate runs: every in-scope doc has
//	                    the stamp under its title, and its sha names a commit
//	                    that is an ancestor of HEAD. A doc with no title has
//	                    nowhere to carry one, and is a finding. It never writes.
//	-write -failing F   set the stamp of every in-scope doc NOT listed in F (one
//	  [-sha REV]        path per line: the docs a reader found fault with) to
//	                    REV's 8-character short sha, HEAD by default, inserting
//	                    the line where it is missing. Prints what it stamped and
//	                    what it skipped, and why.
//	-touched BASE       the docs a change may have made wrong: every in-scope
//	                    doc whose citations, read through docindex (test names,
//	                    code-shaped identifiers, cited paths and link targets),
//	                    name a path `git diff --name-only BASE..HEAD` lists
//	                    (by full path, module-relative path or basename, as
//	                    the resolver reads a citation) or a Go identifier
//	                    declared on a line the diff adds or removes (func and
//	                    method, type, const and var names). One doc per line,
//	                    with the citation that matched. Exit 0.
//
// WHAT -touched CANNOT SEE. A change that makes a doc wrong without touching
// a name the doc cites: a function body that inverts what its unchanged
// signature does, a SQL migration, a YAML key, a JavaScript function, a
// template; a name the doc writes as a bare prose word rather than as code; a
// changed name shorter than 8 characters or all one case (Run, Load, config),
// which would list every doc that uses the word, by deadident's rule; a
// declaration whose name sits on an unchanged line of a multi-line edit; and
// anything not committed (it reads BASE..HEAD). It is a reading list, not a
// verdict.
//
// Usage: go run ./scripts/docstamp [-root DIR] -check | -write -failing FILE [-sha REV] | -touched BASE
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"shingo/shared/scripts/internal/docindex"
)

// git runs git in root and returns its stdout.
func git(root string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Finding is one doc whose stamp is not good.
type Finding struct {
	File string
	Line int
	Why  string
}

func (f Finding) String() string {
	if f.Line == 0 {
		return fmt.Sprintf("%s: %s", f.File, f.Why)
	}
	return fmt.Sprintf("%s:%d: %s", f.File, f.Line, f.Why)
}

// stampVerdict says why sha cannot stand on a doc, or "" when it can: it must
// name a commit, and that commit must be an ancestor of HEAD.
func stampVerdict(root, sha string) string {
	full, err := git(root, nil, "rev-parse", "--verify", "--quiet", sha+"^{commit}")
	if err != nil {
		return fmt.Sprintf("Verified against: %s names no commit in this repo", sha)
	}
	_, err = git(root, nil, "merge-base", "--is-ancestor", strings.TrimSpace(string(full)), "HEAD")
	var exit *exec.ExitError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return fmt.Sprintf("Verified against: %s is not an ancestor of HEAD", sha)
	}
	return fmt.Sprintf("Verified against: %s could not be checked: %v", sha, err)
}

// Check returns every in-scope doc whose stamp is missing or does not stand.
func Check(root string, ix *docindex.Index) []Finding {
	var fs []Finding
	verdict := map[string]string{} // one git answer per distinct sha
	for _, d := range ix.Docs {
		if d.Title == 0 {
			fs = append(fs, Finding{d.Path, 0, "no title to stamp under: the doc has no # heading"})
			continue
		}
		sha, line, ok := d.Stamp()
		if !ok {
			fs = append(fs, Finding{d.Path, d.Title, `no "Verified against: <sha>" line under the title`})
			continue
		}
		why, seen := verdict[sha]
		if !seen {
			why = stampVerdict(root, sha)
			verdict[sha] = why
		}
		if why != "" {
			fs = append(fs, Finding{d.Path, line, why})
		}
	}
	return fs
}

// readList reads the -failing file: one repo path per line. /dev/null (or
// the Windows nul device it becomes) is the empty list.
func readList(name string) (map[string]bool, error) {
	out := map[string]bool{}
	if name == "/dev/null" || strings.EqualFold(name, "nul") {
		return out, nil
	}
	b, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if p := strings.TrimSpace(sc.Text()); p != "" {
			out[filepath.ToSlash(p)] = true
		}
	}
	return out, sc.Err()
}

// Write sets the stamp of every in-scope doc not in failing to sha, and
// reports what it stamped and what it skipped.
func Write(root string, ix *docindex.Index, failing map[string]bool, sha string, w io.Writer) error {
	var stamped, already int
	var skipped []string
	for _, d := range ix.Docs {
		if failing[d.Path] {
			skipped = append(skipped, d.Path+": a reader found fault with it")
			continue
		}
		file := filepath.Join(root, filepath.FromSlash(d.Path))
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		out, err := docindex.SetStamp(d.Path, data, sha)
		if err != nil {
			skipped = append(skipped, d.Path+": no title to stamp under")
			continue
		}
		if bytes.Equal(out, data) {
			already++
			continue
		}
		fi, err := os.Stat(file)
		if err != nil {
			return err
		}
		if err := os.WriteFile(file, out, fi.Mode().Perm()); err != nil {
			return err
		}
		stamped++
	}
	fmt.Fprintf(w, "stamped %d doc(s) Verified against: %s (%d already were); skipped %d\n", stamped, sha, already, len(skipped))
	for _, s := range skipped {
		fmt.Fprintf(w, "  skipped %s\n", s)
	}
	return nil
}

// --- -touched ----------------------------------------------------------------

var (
	identRe  = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	shapedRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+|[A-Za-z_][A-Za-z0-9_]*\(`)
	testRe   = regexp.MustCompile(`\b(?:Test|Fuzz|Benchmark|Example)[A-Z0-9_][A-Za-z0-9_]*`)
	hunkRe   = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@`)
)

// significant is deadident's rule for a name long and mixed-case enough to be
// read as a citation rather than a word.
func significant(name string) bool {
	if len(name) < 8 || strings.HasSuffix(name, "_") {
		return false
	}
	if strings.Contains(name, "_") && !strings.HasPrefix(name, "Test") &&
		!strings.HasPrefix(name, "Benchmark") && !strings.HasPrefix(name, "Fuzz") && !strings.HasPrefix(name, "Example") {
		return false
	}
	var up, low bool
	for _, r := range name {
		up = up || unicode.IsUpper(r)
		low = low || unicode.IsLower(r)
	}
	return up && low
}

// cite is one citation a doc makes, as -touched reads it.
type cite struct {
	line int
	text string
	path bool // a path (resolved like a cited path or a link), else a name
	link bool // a link target: relative to the doc only
}

// docCites reads every citation a doc makes: names in code spans, call and
// selector shapes and test names in prose and fences, cited paths, link
// targets and generated-marker sources.
func docCites(d *docindex.Doc) []cite {
	var out []cite
	names := func(b docindex.Block, text string) {
		for _, m := range shapedRe.FindAllStringIndex(text, -1) {
			for _, id := range identRe.FindAllString(text[m[0]:m[1]], -1) {
				out = append(out, cite{line: b.LineAt(m[0]), text: id})
			}
		}
		for _, m := range testRe.FindAllStringIndex(text, -1) {
			out = append(out, cite{line: b.LineAt(m[0]), text: text[m[0]:m[1]]})
		}
	}
	for _, b := range d.Prose {
		names(b, b.Bare())
		for _, c := range b.Code {
			for _, id := range identRe.FindAllString(c.Text, -1) {
				out = append(out, cite{line: c.Line, text: id})
			}
			if p, ok := docindex.CitedPath(c.Text); ok {
				out = append(out, cite{line: c.Line, text: p, path: true})
			}
		}
	}
	for _, f := range d.Fences {
		if f.Body.Text != "" {
			names(f.Body, f.Body.Text)
		}
	}
	for _, l := range d.Links {
		if p, ok := docindex.LinkTarget(l.Target); ok {
			out = append(out, cite{line: l.Line, text: p, path: true, link: true})
		}
	}
	for _, s := range d.Sources {
		if p, ok := docindex.SourcePath(s.Target); ok {
			out = append(out, cite{line: s.Line, text: p, path: true})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].line < out[j].line })
	return out
}

// changedLines parses `git diff -U0` output into the lines each Go file had
// removed (in BASE) and added (in HEAD).
func changedLines(diff []byte) (removed, added map[string]map[int]bool) {
	removed, added = map[string]map[int]bool{}, map[string]map[int]bool{}
	var oldPath, newPath string
	mark := func(m map[string]map[int]bool, p string, start, n int) {
		if p == "" || n == 0 {
			return
		}
		if m[p] == nil {
			m[p] = map[int]bool{}
		}
		for i := start; i < start+n; i++ {
			m[p][i] = true
		}
	}
	for _, l := range strings.Split(string(diff), "\n") {
		switch {
		case strings.HasPrefix(l, "--- "):
			oldPath = strings.TrimPrefix(strings.TrimPrefix(l, "--- "), "a/")
			if oldPath == "/dev/null" {
				oldPath = ""
			}
		case strings.HasPrefix(l, "+++ "):
			newPath = strings.TrimPrefix(strings.TrimPrefix(l, "+++ "), "b/")
			if newPath == "/dev/null" {
				newPath = ""
			}
		case strings.HasPrefix(l, "@@ "):
			m := hunkRe.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			count := func(s string) int {
				if s == "" {
					return 1 // "@@ -3 +3 @@": one line
				}
				return digits(s)
			}
			mark(removed, oldPath, digits(m[1]), count(m[2]))
			mark(added, newPath, digits(m[3]), count(m[4]))
		}
	}
	return removed, added
}

// digits is the value of a run of decimal digits (hunkRe matched only digits).
func digits(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// blobs reads rev:path for each key through one `git cat-file --batch`; a
// path missing at its rev is absent from the result.
func blobs(root string, keys []string) (map[string][]byte, error) {
	var in bytes.Buffer
	for _, k := range keys {
		in.WriteString(k + "\n")
	}
	out, err := git(root, in.Bytes(), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	res := map[string][]byte{}
	for _, k := range keys {
		nl := bytes.IndexByte(out, '\n')
		if nl < 0 {
			break
		}
		head := strings.Fields(string(out[:nl]))
		out = out[nl+1:]
		if len(head) != 3 {
			continue // "<key> missing"
		}
		n, err := strconv.Atoi(head[2])
		if err != nil || n > len(out) {
			return nil, fmt.Errorf("git cat-file: bad header %q", head)
		}
		res[k] = out[:n]
		out = out[n:]
		if len(out) > 0 && out[0] == '\n' {
			out = out[1:]
		}
	}
	return res, nil
}

// declaredOn returns the names src declares (func and method, type, const
// and var) whose name sits on one of lines.
func declaredOn(src []byte, lines map[int]bool) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var out []string
	add := func(id *ast.Ident) {
		if id != nil && id.Name != "_" && lines[fset.Position(id.Pos()).Line] {
			out = append(out, id.Name)
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			add(x.Name)
		case *ast.TypeSpec:
			add(x.Name)
		case *ast.ValueSpec:
			for _, id := range x.Names {
				add(id)
			}
		}
		return true
	})
	return out
}

// changedNames is every significant Go name declared on a line BASE..HEAD
// adds or removes, with the file it is declared in.
func changedNames(root, base string) (map[string]string, error) {
	diff, err := git(root, nil, "diff", "--no-renames", "-U0", base+"..HEAD", "--", "*.go")
	if err != nil {
		return nil, err
	}
	removed, added := changedLines(diff)
	var keys []string
	for p := range removed {
		keys = append(keys, base+":"+p)
	}
	for p := range added {
		keys = append(keys, "HEAD:"+p)
	}
	sort.Strings(keys)
	src, err := blobs(root, keys)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, side := range []struct {
		rev   string
		lines map[string]map[int]bool
	}{{base, removed}, {"HEAD", added}} {
		for p, ls := range side.lines {
			for _, name := range declaredOn(src[side.rev+":"+p], ls) {
				if significant(name) {
					out[name] = p
				}
			}
		}
	}
	return out, nil
}

// Touched lists each in-scope doc whose citations name a changed path or a
// changed Go name, with the first citation that matched.
func Touched(root string, ix *docindex.Index, base string) ([]string, error) {
	out, err := git(root, nil, "diff", "--no-renames", "--name-only", base+"..HEAD")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	names, err := changedNames(root, base)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, d := range ix.Docs {
	doc:
		for _, c := range docCites(d) {
			if !c.path {
				if file, ok := names[c.text]; ok {
					lines = append(lines, fmt.Sprintf("%s:%d: %s (declared on a changed line of %s)", d.Path, c.line, c.text, file))
					break doc
				}
				continue
			}
			for _, p := range paths {
				if p == d.Path {
					continue
				}
				match := docindex.Names(d.Path, c.text, p)
				if c.link {
					match = docindex.LinkNames(d.Path, c.text, p)
				}
				if match {
					lines = append(lines, fmt.Sprintf("%s:%d: %s (%s changed)", d.Path, c.line, c.text, p))
					break doc
				}
			}
		}
	}
	return lines, nil
}

func main() {
	root := flag.String("root", ".", "the repo root")
	checkMode := flag.Bool("check", false, "check every in-scope doc's stamp (read-only)")
	writeMode := flag.Bool("write", false, "stamp every in-scope doc not listed in -failing")
	failing := flag.String("failing", "", "with -write: a file listing the docs a reader found fault with, one per line")
	rev := flag.String("sha", "HEAD", "with -write: the commit to stamp")
	touched := flag.String("touched", "", "list the docs whose citations name what BASE..HEAD changed")
	flag.Parse()
	modes := 0
	for _, on := range []bool{*checkMode, *writeMode, *touched != ""} {
		if on {
			modes++
		}
	}
	if modes != 1 || *writeMode && *failing == "" {
		fmt.Fprintln(os.Stderr, "usage: docstamp [-root DIR] -check | -write -failing FILE [-sha REV] | -touched BASE")
		os.Exit(2)
	}
	ix, err := docindex.Load(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "docstamp:", err)
		os.Exit(2)
	}
	switch {
	case *checkMode:
		fs := Check(*root, ix)
		for _, f := range fs {
			fmt.Println(f)
		}
		if len(fs) > 0 {
			fmt.Printf("%d doc(s) without a stamp that stands. scripts/docs-verify.sh stamps the docs the readers pass.\n", len(fs))
			os.Exit(1)
		}
	case *writeMode:
		list, err := readList(*failing)
		if err != nil {
			fmt.Fprintln(os.Stderr, "docstamp:", err)
			os.Exit(2)
		}
		short, err := git(*root, nil, "rev-parse", "--short=8", "--verify", *rev+"^{commit}")
		if err != nil {
			fmt.Fprintln(os.Stderr, "docstamp:", err)
			os.Exit(2)
		}
		if err := Write(*root, ix, list, strings.TrimSpace(string(short)), os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "docstamp:", err)
			os.Exit(2)
		}
	default:
		lines, err := Touched(*root, ix, *touched)
		if err != nil {
			fmt.Fprintln(os.Stderr, "docstamp:", err)
			os.Exit(2)
		}
		for _, l := range lines {
			fmt.Println(l)
		}
	}
}
