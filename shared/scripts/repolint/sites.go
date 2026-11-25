package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

// --- reasoned lists ----------------------------------------------------------
//
// A rule below compares the places the tree has with a list file of the places
// allowed, one line each: a kind, the words that name the place, and the reason
// it is allowed, which a reviewer can check against the code. The list is read
// the same way for every such rule.

// listEntry is one line of a rule's list file.
type listEntry struct {
	line   int
	kind   string   // the line's first word
	place  []string // the words after it that name the place
	reason string   // the rest of the line
}

// listShape is one kind of line: how many words name its place, and whether a
// reason must follow them.
type listShape struct {
	words  int
	reason bool
}

// readList reads a rule's list file. Blank lines and lines starting with # are
// skipped. A list that is not in the tree is one finding and found false: the
// rule then has nothing to hold the tree to, which must not print as a pass.
func readList(t *tree, path string, shapes map[string]listShape) (es []listEntry, fs []Finding, found bool) {
	var data []byte
	for _, f := range t.files {
		if f.path == path {
			data, found = f.data, f.data != nil
		}
	}
	if !found {
		return nil, []Finding{{File: path, Why: "the list is not in the tree, so the tree is held to nothing"}}, false
	}
	eachLine(data, func(n int, line []byte) {
		l := strings.TrimSpace(strings.TrimRight(string(line), "\r"))
		if l == "" || strings.HasPrefix(l, "#") {
			return
		}
		w := strings.Fields(l)
		sh, ok := shapes[w[0]]
		switch {
		case !ok:
			fs = append(fs, Finding{File: path, Line: n, Why: fmt.Sprintf("starts with %q, which is no kind of line this list has", w[0])})
		case len(w) < 1+sh.words || (sh.reason && len(w) == 1+sh.words):
			fs = append(fs, Finding{File: path, Line: n, Why: fmt.Sprintf("a %s line names %d word(s) of place then its reason", w[0], sh.words)})
		case !sh.reason && len(w) > 1+sh.words:
			fs = append(fs, Finding{File: path, Line: n, Why: fmt.Sprintf("a %s line is %d word(s) of place and nothing after", w[0], sh.words)})
		default:
			es = append(es, listEntry{line: n, kind: w[0], place: w[1 : 1+sh.words], reason: strings.Join(w[1+sh.words:], " ")})
		}
	})
	return es, fs, true
}

// siteOf is where a node sits: "<file>:<func>", the top-level function it is
// in, or "<file>:-" when it is in no function (a package-level initializer).
func siteOf(file string, decl ast.Decl) string {
	if fd, ok := decl.(*ast.FuncDecl); ok {
		return file + ":" + fd.Name.Name
	}
	return file + ":-"
}

// sitesIn parses files in parallel and calls match on every node of every
// top-level declaration; match names the kind of a node it counts, as the list
// line that allows it starts, and its line, or returns "". It returns the lines
// counted under "<kind> <site>", in source order, and the files that do not
// parse.
func sitesIn(files []*file, match func(ast.Node, *token.FileSet) (string, int)) (map[string][]int, []Finding) {
	sites := map[string][]int{}
	bad := parseAll(files, func(f *file, af *ast.File, fset *token.FileSet) {
		for _, d := range af.Decls {
			site := siteOf(f.path, d)
			ast.Inspect(d, func(n ast.Node) bool {
				if n == nil {
					return false
				}
				if kind, line := match(n, fset); kind != "" {
					sites[kind+" "+site] = append(sites[kind+" "+site], line)
				}
				return true
			})
		}
	})
	for _, lines := range sites {
		sort.Ints(lines)
	}
	return sites, bad
}

// --- bus-subscriptions -------------------------------------------------------
//
// Every place that subscribes to an engine's event bus, Core's or the Edge's,
// is named in busSiteList with the reason it subscribes there:
//
//	site shingo-core/www/sse.go:SetupEngineListeners the web layer subscribes itself, ...
//
// WHY. The bus calls its subscribers in the order they subscribed, on the
// emitter's goroutine (protocol/eventbus), so where a subscription is made is
// behaviour: a subscriber finds done whatever the ones before it did. The
// design has three places a subscription is made: the engine's registry, which
// Start calls; the web layer's own; and, where messaging must hear an engine
// event, main on its behalf, through an engine method (design round 4). A
// fourth place is a set whose position in the order nobody chose. Core's order
// is pinned by TestSubscriberCensus (shingo-core/engine); this rule is the
// cheap half, on both sides: a new place fails here until it is listed with its
// reason, and on Core it needs its rows in that census too.
//
// WHAT IS READ. The tracked non-test .go files of shingo-core and shingo-edge,
// under every build tag (the sim build's included), testdata not. A
// subscription is a call named SubscribeTyped with two or more arguments
// (eventbus.SubscribeTyped), SubscribeTypes with one or more (the bus's own),
// or Subscribe with exactly one (the bus's; the two Kafka clients' Subscribe
// takes a topic and a handler). A site is the top-level function that makes
// one, written <file>:<func>.
//
// WHAT IT CANNOT SEE: a subscription made through a wrapper of another name
// (none exists); a test's subscriptions (a test subscribes what it records);
// which of a site's subscriptions a given engine makes.
//
// WHAT FIRES: a site the list does not name; a listed site that makes no
// subscription now (gone, renamed or emptied: the line goes); a line that is
// not `site <file>:<func> <reason>`, or that names a site twice.
const busSiteList = "shared/scripts/repolint/bus-subscriptions.txt"

// busSubscription names n a "site" node, with its line, when it is a
// subscription on a bus.
func busSubscription(n ast.Node, fset *token.FileSet) (string, int) {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return "", 0
	}
	switch name := typeName(call.Fun); {
	case name == "SubscribeTyped" && len(call.Args) >= 2,
		name == "SubscribeTypes" && len(call.Args) >= 1,
		name == "Subscribe" && len(call.Args) == 1:
		return "site", fset.Position(call.Pos()).Line
	}
	return "", 0
}

// productGo is the tracked non-test .go files of the named modules that Go
// builds, under any build tag.
func productGo(t *tree, modules ...string) []*file {
	var out []*file
	for _, f := range moduleGo(t) {
		top, _, _ := strings.Cut(f.path, "/")
		for _, m := range modules {
			if top == m && !strings.HasSuffix(f.path, "_test.go") {
				out = append(out, f)
			}
		}
	}
	return out
}

func busSubscriptions(t *tree) result {
	listed, fs, found := readList(t, busSiteList, map[string]listShape{"site": {1, true}})
	if !found {
		return result{findings: fs}
	}
	var files []*file
	for _, f := range productGo(t, "shingo-core", "shingo-edge") {
		if bytes.Contains(f.data, []byte("Subscribe")) {
			files = append(files, f)
		}
	}
	sites, bad := sitesIn(files, busSubscription)
	fs = append(fs, bad...)
	seen := map[string]int{}
	for _, e := range listed {
		key := e.kind + " " + e.place[0]
		if first, dup := seen[key]; dup {
			fs = append(fs, Finding{File: busSiteList, Line: e.line, Why: fmt.Sprintf("names %s again (first on line %d)", e.place[0], first)})
			continue
		}
		seen[key] = e.line
		if len(sites[key]) == 0 {
			fs = append(fs, Finding{File: busSiteList, Line: e.line,
				Why: key + ": it makes no bus subscription now (gone, renamed or emptied), so the line goes"})
		}
	}
	n := 0
	for key, lines := range sites {
		n += len(lines)
		if seen[key] == 0 {
			file, fn, _ := strings.Cut(strings.TrimPrefix(key, "site "), ":")
			fs = append(fs, Finding{File: file, Line: lines[0],
				Why: fmt.Sprintf("%s makes %d bus subscription(s) and is not a listed site", fn, len(lines))})
		}
	}
	return result{fs, fmt.Sprintf("%d sites make %d bus subscriptions, each site listed with its reason", len(sites), n)}
}

// --- bare-goroutines ---------------------------------------------------------
//
// In Core's engine and dispatch, each goroutine Engine.Stop does not wait for
// is named in goroutineList with why Stop need not, or must not, wait for it:
//
//	go shingo-core/engine/wiring.go:wireEventHandlers 3 the e-mail sends: ...
//
// WHY. Stop waits for the engine's own loops and hand-offs, spawned on its
// tracked group (Engine.loops; TestEngineStop_WaitsForItsGoroutines). A bare
// go statement, or a func handed to an AfterFunc timer, is outside that wait:
// still running after Stop, it reads a database its caller is about to close
// and logs through a logger whose owner may be gone. Docket B4 found two (the
// re-drive after a dig's last leg failed, and the eta loop) and moved them onto
// the group; this rule keeps the class from growing back unseen. The few that
// stay untracked do so on purpose (Stop never waits on a mail server), and the
// list says why for each.
//
// WHAT IS READ. The list's scope lines name the directories read, each with the
// directories under it: their tracked non-test .go files, under every build
// tag, testdata not. Counted, by the top-level function they are in: each go
// statement, and each call named AfterFunc (time's, a context's, any clock's).
//
// WHAT FIRES: a go statement or AfterFunc call in a function its list line does
// not allow (no line, or more than the line counts); a line that counts more
// than the tree has (fewer now, or the function is gone: the count is
// corrected or the line goes); a line not of the list's shapes, a count that
// is no number, a place named twice, or no scope line at all.
const goroutineList = "shared/scripts/repolint/bare-goroutines.txt"

// goroutineNode names n a "go" or an "AfterFunc" node, with its line, when it
// starts work on a goroutine of its own.
func goroutineNode(n ast.Node, fset *token.FileSet) (string, int) {
	switch x := n.(type) {
	case *ast.GoStmt:
		return "go", fset.Position(x.Pos()).Line
	case *ast.CallExpr:
		if typeName(x.Fun) == "AfterFunc" {
			return "AfterFunc", fset.Position(x.Pos()).Line
		}
	}
	return "", 0
}

func bareGoroutines(t *tree) result {
	listed, fs, found := readList(t, goroutineList, map[string]listShape{
		"scope": {1, false}, "go": {2, true}, "AfterFunc": {2, true}})
	if !found {
		return result{findings: fs}
	}
	var scopes []string
	for _, e := range listed {
		if e.kind == "scope" {
			scopes = append(scopes, strings.TrimSuffix(e.place[0], "/")+"/")
		}
	}
	if len(scopes) == 0 {
		return result{findings: append(fs, Finding{File: goroutineList, Why: "no scope line, so no directory is read"})}
	}
	var files []*file
	for _, f := range moduleGo(t) {
		if !strings.HasSuffix(f.path, "_test.go") && hasPrefixAny(f.path, scopes) {
			files = append(files, f)
		}
	}
	counted, bad := sitesIn(files, goroutineNode)
	fs = append(fs, bad...)
	seen := map[string]int{}
	allowed := map[string]int{}
	for _, e := range listed {
		key := e.kind + " " + e.place[0]
		if first, dup := seen[key]; dup {
			fs = append(fs, Finding{File: goroutineList, Line: e.line, Why: fmt.Sprintf("names %s again (first on line %d)", key, first)})
			continue
		}
		seen[key] = e.line
		if e.kind == "scope" {
			continue
		}
		n, err := strconv.Atoi(e.place[1])
		if err != nil || n < 1 {
			fs = append(fs, Finding{File: goroutineList, Line: e.line, Why: key + " names no count: " + e.place[1]})
			continue
		}
		allowed[key] = n
		if got := len(counted[key]); got < n {
			fs = append(fs, Finding{File: goroutineList, Line: e.line,
				Why: fmt.Sprintf("%s counts %d, the tree has %d: correct the count, or the line goes", key, n, got)})
		}
	}
	var goStmts, timers int
	for key, lines := range counted {
		kind, site, _ := strings.Cut(key, " ")
		if kind == "go" {
			goStmts += len(lines)
		} else {
			timers += len(lines)
		}
		if n := allowed[key]; len(lines) > n {
			file, fn, _ := strings.Cut(site, ":")
			at := make([]string, len(lines))
			for i, l := range lines {
				at[i] = strconv.Itoa(l)
			}
			fs = append(fs, Finding{File: file, Line: lines[0], Why: fmt.Sprintf(
				"%s has %d %s (lines %s) that Engine.Stop does not wait for; the list allows %d",
				fn, len(lines), map[string]string{"go": "go statement(s)", "AfterFunc": "AfterFunc call(s)"}[kind],
				strings.Join(at, ", "), n)})
		}
	}
	return result{fs, fmt.Sprintf("%d go statements and %d AfterFunc calls under %s, each listed with its reason",
		goStmts, timers, strings.Join(scopes, ", "))}
}
