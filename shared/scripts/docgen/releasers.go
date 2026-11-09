package main

// releasers.go — the two tables in docs/queue-releasers.md, from
// shingo-core/dispatch/queue_releasers.go:
//
//   - waitPopulations: each population an order can wait in, who owns it, the
//     re-driver an event and the floor both call (with its doc sentence), the
//     events that release it, and its floor or why it has none;
//   - causeReleasers: each queue cause, its value on the row, the populations
//     an order carrying it can sit in, what ends the wait, and the finding
//     when the row could not be written as an event.
//
// Both in source order: the file groups causes by family and that grouping is
// the code's own.

import (
	"fmt"
	"go/ast"
	"path"
	"regexp"
	"strings"
)

const releasersDir = "shingo-core/dispatch"

func init() {
	register(Generator{Name: "shingo-core/dispatch/queue_releasers.go waitPopulations", Emit: func(root string, args []string) (string, error) {
		if len(args) != 0 {
			return "", fmt.Errorf("takes no arguments, got %q", args)
		}
		return releasersPopulations(root)
	}})
	register(Generator{Name: "shingo-core/dispatch/queue_releasers.go causeReleasers", Emit: func(root string, args []string) (string, error) {
		if len(args) != 0 {
			return "", fmt.Errorf("takes no arguments, got %q", args)
		}
		return releasersCauses(root)
	}})
}

// releasersRows parses a table var: a slice literal of struct literals, each
// returned as its field name → value expression, with the file they are in.
func releasersRows(root, table string) (*gosrcPkg, *gosrcFile, []map[string]ast.Expr, error) {
	tree, err := newGosrcTree(root)
	if err != nil {
		return nil, nil, nil, err
	}
	pkg, err := tree.pkg(releasersDir)
	if err != nil {
		return nil, nil, nil, err
	}
	v := pkg.values[table]
	if v == nil {
		return nil, nil, nil, fmt.Errorf("%s declares no %s", releasersDir, table)
	}
	lit, ok := v.expr.(*ast.CompositeLit)
	if !ok {
		return nil, nil, nil, fmt.Errorf("%s is not a slice literal", table)
	}
	var rows []map[string]ast.Expr
	for _, el := range lit.Elts {
		row, ok := el.(*ast.CompositeLit)
		if !ok {
			return nil, nil, nil, fmt.Errorf("%s: an entry is not a struct literal", table)
		}
		fields := map[string]ast.Expr{}
		for _, f := range row.Elts {
			kv, ok := f.(*ast.KeyValueExpr)
			if !ok {
				return nil, nil, nil, fmt.Errorf("%s: an entry's field is not key: value", table)
			}
			fields[gosrcExpr(kv.Key)] = kv.Value
		}
		rows = append(rows, fields)
	}
	if len(rows) == 0 {
		return nil, nil, nil, fmt.Errorf("%s is empty", table)
	}
	return pkg, v.file, rows, nil
}

// releasersString is a field's string value; a missing field is "".
func releasersString(file *gosrcFile, fields map[string]ast.Expr, name string) (string, error) {
	e, ok := fields[name]
	if !ok {
		return "", nil
	}
	s, ok := file.stringConst(e)
	if !ok {
		return "", fmt.Errorf("field %s = %s is not a string constant", name, gosrcExpr(e))
	}
	return s, nil
}

// releasersStrings is a []string field's values.
func releasersStrings(file *gosrcFile, fields map[string]ast.Expr, name string) ([]string, error) {
	e, ok := fields[name]
	if !ok {
		return nil, nil
	}
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return nil, fmt.Errorf("field %s is not a list literal", name)
	}
	var out []string
	for _, el := range lit.Elts {
		s, ok := file.stringConst(el)
		if !ok {
			return nil, fmt.Errorf("field %s: %s is not a string constant", name, gosrcExpr(el))
		}
		out = append(out, s)
	}
	return out, nil
}

// releasersSymbol matches a dotted Go name (pkg.Type.Method, Type.Method) or
// an event constant, the words worth setting as code.
var releasersSymbol = regexp.MustCompile(`^([A-Za-z_]\w*(\.[A-Za-z_]\w*)+|Event[A-Z]\w*)$`)

// releasersCode sets each symbol-shaped word of a table string as code.
func releasersCode(s string) string {
	words := strings.Split(s, " ")
	for i, w := range words {
		core := strings.TrimLeft(w, "(")
		lead := w[:len(w)-len(core)]
		trimmed := strings.TrimRight(core, ",);:")
		tail := core[len(trimmed):]
		if releasersSymbol.MatchString(trimmed) {
			words[i] = lead + "`" + trimmed + "`" + tail
		}
	}
	return strings.Join(words, " ")
}

// releasersRedriver resolves a re-driver string — "Dispatcher.Method" in the
// dispatch package, or "pkg.Type.Method" in a sibling package, with any
// parenthetical after it — to its declaration.
func releasersRedriver(pkg *gosrcPkg, s string) (*gosrcFunc, string, error) {
	name := strings.Fields(s)[0]
	parts := strings.Split(name, ".")
	target := pkg
	switch len(parts) {
	case 2:
	case 3:
		p, err := pkg.tree.pkg(path.Join(path.Dir(releasersDir), parts[0]))
		if err != nil {
			return nil, name, fmt.Errorf("re-driver %s: %v", name, err)
		}
		target, parts = p, parts[1:]
	default:
		return nil, name, fmt.Errorf("re-driver %q is not Type.Method or pkg.Type.Method", s)
	}
	fn := target.funcs[parts[0]+"."+parts[1]]
	if fn == nil {
		return nil, name, fmt.Errorf("re-driver %s names no method", name)
	}
	return fn, name, nil
}

func releasersPopulations(root string) (string, error) {
	pkg, file, rows, err := releasersRows(root, "waitPopulations")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("| Population | What waits there | Owner | Re-driver | Released by | Floor |\n|---|---|---|---|---|---|\n")
	for _, r := range rows {
		popExpr, ok := r["population"]
		if !ok {
			return "", fmt.Errorf("waitPopulations: an entry names no population")
		}
		popName := gosrcExpr(popExpr)
		pop, err := releasersString(file, r, "population")
		if err != nil {
			return "", err
		}
		popDoc := ""
		if v := pkg.values[popName]; v != nil {
			popDoc = gosrcDocSentence(v.doc)
		}
		if popDoc == "" {
			return "", fmt.Errorf("waitPopulations: %s has no doc comment", popName)
		}
		owner, err := releasersString(file, r, "owner")
		if err != nil {
			return "", err
		}
		redriver, err := releasersString(file, r, "redriver")
		if err != nil {
			return "", err
		}
		fn, rname, err := releasersRedriver(pkg, redriver)
		if err != nil {
			return "", err
		}
		rdoc := gosrcDocSentence(fn.decl.Doc)
		if rdoc == "" {
			return "", fmt.Errorf("waitPopulations: re-driver %s has no doc comment", rname)
		}
		rest := strings.TrimSpace(strings.TrimPrefix(redriver, rname))
		redriverCell := "`" + rname + "`"
		if rest != "" {
			redriverCell += " " + rest
		}
		redriverCell += " — " + rdoc
		events, err := releasersStrings(file, r, "events")
		if err != nil {
			return "", err
		}
		for i, e := range events {
			events[i] = releasersCode(e)
		}
		floor, err := releasersString(file, r, "floor")
		if err != nil {
			return "", err
		}
		floorCell := releasersCode(floor)
		if floor == "" {
			why, err := releasersString(file, r, "noFloorBecause")
			if err != nil {
				return "", err
			}
			if why == "" {
				return "", fmt.Errorf("waitPopulations: %s has neither a floor nor a reason for none", popName)
			}
			floorCell = "none: " + gosrcFirstSentence(why)
		}
		fmt.Fprintf(&b, "| `%s` (`%s`) | %s | %s | %s | %s | %s |\n",
			popName, pop, gosrcCell(popDoc), owner, gosrcCell(redriverCell),
			gosrcCell(strings.Join(events, ", ")), gosrcCell(floorCell))
	}
	return b.String(), nil
}

func releasersCauses(root string) (string, error) {
	_, file, rows, err := releasersRows(root, "causeReleasers")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("| Cause | Value | Waits in | What ends it | Finding |\n|---|---|---|---|---|\n")
	for _, r := range rows {
		causeExpr, ok := r["cause"]
		if !ok {
			return "", fmt.Errorf("causeReleasers: an entry names no cause")
		}
		cause, err := releasersString(file, r, "cause")
		if err != nil {
			return "", err
		}
		pops, err := releasersStrings(file, r, "populations")
		if err != nil {
			return "", err
		}
		what, err := releasersString(file, r, "what")
		if err != nil {
			return "", err
		}
		finding, err := releasersString(file, r, "finding")
		if err != nil {
			return "", err
		}
		// A row may leave `what` out only when its finding says why.
		switch {
		case what == "" && finding == "":
			return "", fmt.Errorf("causeReleasers: %s says neither what ends it nor why nothing does", gosrcExpr(causeExpr))
		case what == "":
			what = "—"
		}
		for i, p := range pops {
			pops[i] = "`" + p + "`"
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s | %s |\n",
			gosrcExpr(causeExpr), cause, strings.Join(pops, ", "), gosrcCell(what), gosrcCell(gosrcFirstSentence(finding)))
	}
	return b.String(), nil
}
