// Command repolint runs the repo-wide text rules over the tracked tree in one
// process: one `git ls-files` listing, one `git check-attr` call, one read of
// each tracked file, and one node process for every .js file (two: js-parse's
// and js-undeclared's).
//
// WHY ONE PROCESS. Each rule was a bash guard of its own (scripts/check-*.sh),
// and on Windows a guard's cost is its process spawns, not its walk: the three
// `git grep` guards took 35-41 s each inside the gate, and js-parse started
// bash, mktemp, cp and node for every one of 158 files. The rules are the same
// rules; what changed is that the tree is read once.
//
// THE RULES, each in its own function with its reason beside it:
//
//	comment-references      a file:line citation in a Go comment names a line that exists
//	conflict-markers        no merge-conflict marker line in a tracked text file
//	expiry                  an EXPIRY: when <symbol> is gone pin names a declared symbol
//	js-parse                every .js file parses as an ES module or as CommonJS
//	js-undeclared           a www/static page script reads only names something declares
//	plant-addresses         no private IPv4 address in a tracked text file
//	swap-mode-literals      no raw swap-mode string outside its constants and the surfaces that name modes
//	synthetic-part-numbers  no customer-shaped part number or plant PLC struct name
//	text-files              no text-attributed file indexed -text
//
// TRACKED FILES ONLY. The listing is `git ls-files`; content is read from the
// working tree, as `git grep` reads it. The guards that used `grep -R` or
// `find` (swap-mode literals, comment references, js-parse) also read untracked
// files; these rules do not, the same answer CI gets from a clean checkout.
//
// GUARD-SHAPED OUTPUT. One `ok   <rule> (...)` line per clean rule, in the
// order above, or `FAIL <rule>  file:line  why` per finding (at most 30 per
// rule) and one indented hint. Exit 1 on any finding, 2 when the tree cannot
// be read.
//
// LIVENESS. A pattern that matches nothing prints the same "ok" as a clean
// tree. Every rule has cases in main_test.go that must fire, assembled at run
// time so the test file passes its own rules; a rule without one fails that
// test.
//
// A new repo-wide rule is a rule here, not a new scripts/check-*.sh.
//
// -docs runs comment-references over the in-scope markdown docs instead of Go
// comments, and nothing else (docs.go says what it reads); the doc-citation
// gate, scripts/check-doc-citations.sh, runs it.
//
// Usage: go run ./scripts/repolint [-root DIR] [-rule NAME | -docs]
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Modules are the five Go modules.
var Modules = []string{"protocol", "shared", "shingo-core", "shingo-edge", "integration"}

// maxShown caps the findings printed per rule; the count of the rest is
// printed in their place.
const maxShown = 30

// file is one tracked path.
type file struct {
	path string // slash-separated, from the repo root
	eol  string // the index copy's eol class (`git ls-files --eol`): lf, crlf, mixed, none, -text
	text string // the text attribute as `git check-attr` prints it: set, unset, unspecified, or a value
	diff string // the diff attribute, likewise
	data []byte // the working-tree content; nil for a symlink, a submodule, or a file not on disk
}

// tree is the tracked files of one checkout, sorted by path.
type tree struct {
	root  string
	files []*file
}

// Finding is one thing a rule reports.
type Finding struct {
	File   string // "" when the finding is about the run, not a file
	Line   int    // 0 when it is about the whole file
	Why    string
	Detail string // printed indented under the finding, when not ""
}

func (f Finding) where() string {
	switch {
	case f.File == "":
		return "-"
	case f.Line == 0:
		return f.File
	}
	return fmt.Sprintf("%s:%d", f.File, f.Line)
}

// result is what one rule returns: its findings, and the parenthetical of its
// ok line for when there are none.
type result struct {
	findings []Finding
	ok       string
}

// rule is one check over the tree.
type rule struct {
	name  string
	hint  string
	check func(*tree) result
}

// rules are run in parallel and printed in this order.
var rules = []rule{
	{"comment-references", "Cite a SYMBOL for anything you expect to survive; line numbers drift silently.", commentReferences},
	{"conflict-markers", "Resolve the merge: a marker line that is not Go source builds and ships silently.", conflictMarkers},
	{"expiry", "Retire the pin in the edit that deletes its symbol: rewrite or delete the scar, and say so.", expiry},
	{"js-parse", "A module that does not parse takes its whole import graph down with it.", jsParse},
	{"js-undeclared", "Declare it, pass it, or import it. A page global goes on window or in the template's inline script; a browser global the rule does not know goes in KNOWN in jsundeclared.js.", jsUndeclared},
	{"plant-addresses", "Use TEST-NET (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24); 192.168.1.0/24 is reserved for doc examples. To derive a fixture from a capture, see shared/scripts/anonymiserobotsstatus.", plantAddresses},
	{"swap-mode-literals", "Go: use the typed constant from protocol/swap_mode.go. JavaScript: key the gate on the order graph or a property the server declares (releases_as_pair).", swapModeLiterals},
	{"synthetic-part-numbers", "Use a synthetic code (SYN-A-P007, SYN-PART07A.06) or struct (MES_Press_A1).", syntheticPartNumbers},
	{"text-files", "Fix the file content (often an editor round-trip), then re-add it.", textFiles},
}

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
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// listIndex parses `git ls-files -s --eol -z`: one record per path, holding
// "<mode> <oid> <stage>\t<eolinfo>\t<path>". Only regular files are read
// (`git grep` greps nothing else); every path is listed.
func listIndex(out []byte) (files []*file, regular map[string]bool, err error) {
	seen := map[string]bool{}
	regular = map[string]bool{}
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		head, rest, ok := strings.Cut(string(rec), "\t")
		eolinfo, path, ok2 := strings.Cut(rest, "\t")
		if !ok || !ok2 {
			return nil, nil, fmt.Errorf("git ls-files: unexpected record %q", rec)
		}
		if seen[path] { // an unmerged path has one record per stage
			continue
		}
		seen[path] = true
		mode, _, _ := strings.Cut(head, " ")
		regular[path] = mode == "100644" || mode == "100755"
		eol, _, _ := strings.Cut(eolinfo, " ")
		files = append(files, &file{path: path, eol: strings.TrimPrefix(eol, "i/")})
	}
	return files, regular, nil
}

// readAttrs fills each file's diff and text attributes from one
// `git check-attr -z --stdin` call, whose output is path, attribute, value
// triples.
func readAttrs(root string, files []*file) error {
	var in bytes.Buffer
	byPath := map[string]*file{}
	for _, f := range files {
		in.WriteString(f.path)
		in.WriteByte(0)
		byPath[f.path] = f
	}
	out, err := git(root, in.Bytes(), "check-attr", "-z", "--stdin", "diff", "text")
	if err != nil {
		return err
	}
	fields := bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0})
	if len(fields)%3 != 0 {
		return errors.New("git check-attr: output is not path/attribute/value triples")
	}
	for i := 0; i+2 < len(fields); i += 3 {
		f := byPath[string(fields[i])]
		if f == nil {
			continue
		}
		switch string(fields[i+1]) {
		case "diff":
			f.diff = string(fields[i+2])
		case "text":
			f.text = string(fields[i+2])
		}
	}
	return nil
}

// load lists the tracked files under root, their attributes, and the
// working-tree content of every regular file.
func load(root string) (*tree, error) {
	out, err := git(root, nil, "ls-files", "-s", "--eol", "-z")
	if err != nil {
		return nil, err
	}
	files, regular, err := listIndex(out)
	if err != nil {
		return nil, err
	}
	if err := readAttrs(root, files); err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 32)
	for _, f := range files {
		if !regular[f.path] {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(f *file) {
			defer func() { <-sem; wg.Done() }()
			// A tracked file missing from the working tree is skipped, as
			// `git grep` skips it.
			f.data, _ = os.ReadFile(filepath.Join(root, filepath.FromSlash(f.path)))
		}(f)
	}
	wg.Wait()
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return &tree{root: root, files: files}, nil
}

// binary is `git grep -I`'s test: the diff attribute decides when it is set
// (`binary` unsets it), and otherwise a NUL in the first 8000 bytes.
func (f *file) binary() bool {
	switch f.diff {
	case "unset":
		return true
	case "set":
		return false
	}
	head := f.data
	if len(head) > 8000 {
		head = head[:8000]
	}
	return bytes.IndexByte(head, 0) >= 0
}

// eachLine calls fn with every line of data and its 1-based number, the way
// `git grep -n` numbers them: split on \n, a \r kept.
func eachLine(data []byte, fn func(n int, line []byte)) {
	for n := 1; len(data) > 0; n++ {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			fn(n, data)
			return
		}
		fn(n, data[:i])
		data = data[i+1:]
	}
}

// run runs the rules (one, when only is not "") over t and prints them in
// order. It reports whether every rule was clean.
func run(t *tree, only string, w io.Writer) bool {
	var picked []rule
	for _, r := range rules {
		if only == "" || r.name == only {
			picked = append(picked, r)
		}
	}
	if len(picked) == 0 {
		fmt.Fprintf(w, "FAIL repolint  -  no rule is named %q\n", only)
		return false
	}
	results := make([]result, len(picked))
	var wg sync.WaitGroup
	for i, r := range picked {
		wg.Add(1)
		go func(i int, r rule) {
			defer wg.Done()
			results[i] = r.check(t)
		}(i, r)
	}
	wg.Wait()
	clean := true
	for i, r := range picked {
		if !report(w, r, results[i]) {
			clean = false
		}
	}
	return clean
}

// report prints one rule's result, at most maxShown findings, and reports
// whether it was clean.
func report(w io.Writer, r rule, res result) bool { return reportN(w, r, res, maxShown) }

// reportN is report with its own cap; 0 prints every finding.
func reportN(w io.Writer, r rule, res result, limit int) bool {
	if len(res.findings) == 0 {
		fmt.Fprintf(w, "ok   %s (%s)\n", r.name, res.ok)
		return true
	}
	fs := res.findings
	sort.SliceStable(fs, func(i, j int) bool {
		if fs[i].File != fs[j].File {
			return fs[i].File < fs[j].File
		}
		return fs[i].Line < fs[j].Line
	})
	for i, f := range fs {
		if limit > 0 && i == limit {
			fmt.Fprintf(w, "FAIL %s  ...  and %d more\n", r.name, len(fs)-limit)
			break
		}
		fmt.Fprintf(w, "FAIL %s  %s  %s\n", r.name, f.where(), f.Why)
		if f.Detail != "" {
			for _, l := range strings.Split(strings.TrimRight(f.Detail, "\n"), "\n") {
				fmt.Fprintf(w, "         %s\n", l)
			}
		}
	}
	fmt.Fprintf(w, "     %s\n", r.hint)
	return false
}

func main() {
	root := flag.String("root", ".", "the repo root")
	only := flag.String("rule", "", "run only the rule of this name")
	docs := flag.Bool("docs", false, "read the in-scope docs instead of Go comments: comment-references only")
	flag.Parse()
	t, err := load(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "repolint:", err)
		os.Exit(2)
	}
	clean := false
	if *docs {
		clean = runDocs(t, os.Stdout)
	} else {
		clean = run(t, *only, os.Stdout)
	}
	if !clean {
		os.Exit(1)
	}
}
