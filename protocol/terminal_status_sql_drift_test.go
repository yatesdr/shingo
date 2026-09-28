package protocol_test

import (
	"go/ast"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"shingo/protocol"
)

// TestTerminalStatusSQLHasOneSpelling refuses a hand-spelled copy of the
// terminal-status set in SQL anywhere outside protocol/.
//
// protocol.TerminalStatusSQLList() derives 'cancelled','confirmed','failed',
// 'skipped' from the status enum. A query that types the four out agrees with
// it today and silently stops agreeing the day a terminal status is added or
// renamed — the same drift that left `pending` and `sourcing` watched by
// nothing (see StatusSQLList). One such copy was live in
// engine/reconciliation_service.go when this landed.
//
// It flags an IN-list whose members are EXACTLY the terminal set. A wider list
// (store/telemetry's completion set, which adds delivered; its defensive
// 'canceled') is a different population on purpose and is not a copy.
//
// MIGRATIONS ARE EXEMPT: a migration is frozen history, and its SQL must mean
// what it meant when it ran, whatever the enum says later.
func TestTerminalStatusSQLHasOneSpelling(t *testing.T) {
	t.Parallel()

	var terminal []string
	for _, s := range protocol.AllStatuses() {
		if protocol.IsTerminal(s) {
			terminal = append(terminal, string(s))
		}
	}
	sort.Strings(terminal)
	want := strings.Join(terminal, ",")

	inList := regexp.MustCompile(`(?is)\bIN\s*\(\s*((?:'[a-z_]+'\s*,?\s*)+)\)`)
	member := regexp.MustCompile(`'([a-z_]+)'`)

	var found []string
	walkRepoProduction(t, func(rel string, file *ast.File, fset *token.FileSet) {
		if strings.HasPrefix(rel, "protocol/") || strings.HasSuffix(rel, "/migrations.go") {
			return
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, m := range inList.FindAllStringSubmatch(val, -1) {
				var got []string
				for _, v := range member.FindAllStringSubmatch(m[1], -1) {
					got = append(got, v[1])
				}
				sort.Strings(got)
				if strings.Join(got, ",") == want {
					found = append(found, rel+":"+strconv.Itoa(fset.Position(lit.Pos()).Line))
				}
			}
			return true
		})
	})

	if len(found) != 0 {
		t.Errorf("the terminal-status set is spelled out by hand in SQL in %d place(s):\n  %s\n\n"+
			"Splice protocol.TerminalStatusSQLList() instead. A hand-typed copy agrees with the enum "+
			"only until a terminal status changes.", len(found), strings.Join(found, "\n  "))
	}
}
