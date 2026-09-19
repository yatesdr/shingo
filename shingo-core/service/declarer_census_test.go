package service

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// declarer_census_test.go — who is allowed to say a person declared a reset.
//
// WHAT THE COMPILER ALREADY DOES, so this does not: bumpEpoch takes a
// protocol.Declarer with no default, so a new reset path cannot omit the
// question. The build fails on it. A census repeating that would pin nothing.
//
// WHAT IT CANNOT DO is notice that a new site answered DeclaredByPerson. That
// answer is the one with a blast radius: it tells the station it may bind an
// empty slot to the carrier the message names, and every path where that is
// wrong is wrong in the same way — the carrier has already been driven away,
// and the press's next parts are charged to it until something lands. The four
// machine paths all reach bumpEpoch through the same helpers as the two human
// ones, so copying a call and keeping its declarer is a one-line change with
// no compile error and no failing test behind it.
//
// So the pin is the SET, not the count of sites: a person declares a reset at
// the two admin doors and nowhere else.
//
// ── ANSWERING IS NOT READING, AND THIS TELLS THEM APART ────────────────────
//
// It used to count occurrences of the text "protocol.DeclaredByPerson", which
// was exact while every mention of the value WAS an answer. It is not any more:
// service.judgeBinTypeCarriesPayload compares against it to decide whether a
// payload written into a carrier its payload_bin_types rows exclude is refused
// or accepted-and-flagged. That site announces nothing and binds nothing — it
// is one comparison — so the text census called a door what is only a reader.
//
// The fix is not to widen the allowlist. An entry there asserts "an operator
// with the bin in front of them typing what it now holds", and adding a
// comparison to it would state something false about the plant in the one file
// whose job is to keep that statement true.
//
// So this parses instead of grepping, and counts an occurrence as an ANSWER
// unless it is an operand of == / != or an expression in a switch case. An
// answer is the value GOING somewhere — a call argument, a return, an
// assignment, a struct field — which is exactly the set that can reach
// bumpEpoch and the wire. A read is a branch on a value somebody else already
// chose, and cannot.
//
// READS ARE NOT PINNED HERE, deliberately: they have no blast radius, and a
// list of them would need updating for every new `if`. What a reader changes is
// the MEANING of a new Declarer value, and that is recorded where the type is —
// see protocol/constants.go, which names this reader.
//
// Failing open is the default on purpose: anything this cannot classify counts
// as an answer and has to be justified in the map.

// personDeclarationSites is every non-test site in this module that may
// announce a reset as a person's declaration, by module-relative path, with
// how many times.
//
// Both are on the bin detail modal — an operator with the bin in front of them
// typing what it now holds, or saying it is now empty. That is what makes the
// declaration true, and it is the property a new entry has to be able to claim.
var personDeclarationSites = map[string]int{
	"www/bin_actions.go": 2, // binLoadPayload, binClear
}

func TestDeclaredByPerson_OnlyAtTheAdminDoors(t *testing.T) {
	t.Parallel()
	root := ".."
	found := map[string]int{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Vendored and build output carry no doors of ours. The root is
			// exempt from the dotted-name rule: it is spelled "..", which
			// starts with a dot, and skipping it walks nothing — a census that
			// reads no files reports no violations.
			if path == root {
				return nil
			}
			if name := d.Name(); name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		// Non-test sources only. A test naming a person is a test describing
		// this behaviour, not a door the plant can reach.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		// Cheap pre-filter, so the walk parses only the handful of files that
		// mention the value at all rather than every file in the module.
		if !strings.Contains(string(src), "DeclaredByPerson") {
			return nil
		}
		n, cErr := countPersonAnswers(path, src)
		if cErr != nil {
			return cErr
		}
		if n == 0 {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			// A path the walk produced that will not relativise against the
			// walk's own root is not a file to skip quietly: every later
			// comparison is by module-relative path, so dropping it here
			// would exempt that file from the census.
			return fmt.Errorf("relativise %s against %s: %w", path, root, relErr)
		}
		found[filepath.ToSlash(rel)] = n
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v (this test is the only thing holding the person-declaration set "+
			"to the code; if the module moved, repoint it rather than deleting it)", root, err)
	}

	for _, path := range sortedKeys(found) {
		want, allowed := personDeclarationSites[path]
		switch {
		case !allowed:
			t.Errorf("%s declares a reset as a person (%d site(s)) and is not in "+
				"personDeclarationSites.\n\nA person's declaration lets the station BIND an empty "+
				"slot to the carrier this names. That is right when somebody is standing at the "+
				"bin saying what is in it, and wrong everywhere else: an announcement Core "+
				"generated from a carrier's own lifecycle routinely lands after a robot has "+
				"lifted that carrier, and binding there charges the press's next parts to a bin "+
				"that has left.\n\nIf this site really is a person at a door, add it here with "+
				"what makes the declaration true. If it is machinery, it wants "+
				"protocol.DeclaredByLifecycle. If it only COMPARES against the value it is a "+
				"reader, not a door, and this census already ignores it — check the spelling is "+
				"a == or != rather than a value being passed.", path, found[path])
		case found[path] != want:
			t.Errorf("%s has %d person declarations, pinned at %d. A site was added or removed; "+
				"check the new one is a door where somebody is looking at the carrier, then "+
				"update this number.", path, found[path], want)
		}
	}
	for _, path := range sortedKeys(personDeclarationSites) {
		if _, ok := found[path]; !ok {
			t.Errorf("personDeclarationSites names %s, which declares no reset as a person. "+
				"If the door moved, repoint this; if it is gone, drop the entry — a stale "+
				"allowlist entry is a hole the next copy-paste lands in.", path)
		}
	}
}

// TestDeclarerCensusTellsAnAnswerFromARead is the census's own pin.
//
// The classifier is the whole guard now, and it is the kind of thing that can
// quietly start answering "read" to everything — at which point the census
// passes forever and watches nothing. These two fixtures are the two shapes
// that must never be confused: a value going somewhere, and a branch on a value
// somebody else chose.
func TestDeclarerCensusTellsAnAnswerFromARead(t *testing.T) {
	t.Parallel()
	const src = `package p

import "shingo/protocol"

type msg struct{ By protocol.Declarer }

func answers(by protocol.Declarer) protocol.Declarer {
	send(protocol.DeclaredByPerson)               // answer: call argument
	_ = msg{By: protocol.DeclaredByPerson}        // answer: struct field
	by = protocol.DeclaredByPerson                // answer: assignment
	return protocol.DeclaredByPerson              // answer: return
}

func reads(by protocol.Declarer) bool {
	if by == protocol.DeclaredByPerson {          // read
		return true
	}
	if by != protocol.DeclaredByPerson {          // read
		return false
	}
	switch by {
	case protocol.DeclaredByPerson:               // read
		return true
	}
	return false
}
`
	got, err := countPersonAnswers("fixture.go", []byte(src))
	if err != nil {
		t.Fatalf("classify fixture: %v", err)
	}
	if got != 4 {
		t.Errorf("countPersonAnswers = %d, want 4.\n\nThe fixture holds four answers "+
			"(argument, struct field, assignment, return) and four reads (==, !=, switch "+
			"case). A lower number means a shape that hands the value onward is being read "+
			"as a comparison, which is the census going blind; a higher one means a "+
			"comparison is being called a door, which is what sent judgeBinTypeCarriesPayload "+
			"to this list in the first place.", got)
	}
}

// countPersonAnswers returns how many times this file ANSWERS
// protocol.DeclaredByPerson — hands the value onward — as opposed to comparing
// against it.
//
// Parsed rather than matched because the distinction is syntactic and a regex
// over `==` would be wrong the first time somebody wraps a comparison over two
// lines. Object resolution is skipped: the question is shape, not identity, and
// the walk parses files from the whole module without their packages.
//
// The selector is matched as `protocol.DeclaredByPerson`, the spelling this
// module uses everywhere; a file that aliased the import would read as zero
// answers here, which is why the import is not aliased anywhere and why this
// says so out loud.
func countPersonAnswers(path string, src []byte) (int, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w (a source file this census cannot parse is a "+
			"file it cannot police; fix the file or the census, do not skip it)", path, err)
	}

	isPerson := func(n ast.Expr) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "DeclaredByPerson" {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "protocol"
	}

	// Positions that are OPERANDS OF A COMPARISON, collected first so the count
	// below can subtract them. Everything else the value appears in hands it
	// onward.
	reads := map[token.Pos]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BinaryExpr:
			if x.Op == token.EQL || x.Op == token.NEQ {
				for _, side := range []ast.Expr{x.X, x.Y} {
					if isPerson(side) {
						reads[side.Pos()] = true
					}
				}
			}
		case *ast.CaseClause:
			// `case protocol.DeclaredByPerson:` is a comparison the language
			// writes for you.
			for _, e := range x.List {
				if isPerson(e) {
					reads[e.Pos()] = true
				}
			}
		}
		return true
	})

	answers := 0
	ast.Inspect(f, func(n ast.Node) bool {
		e, ok := n.(ast.Expr)
		if !ok || !isPerson(e) {
			return true
		}
		if !reads[e.Pos()] {
			answers++
		}
		return true
	})
	return answers, nil
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
